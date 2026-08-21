package main

import (
	"context"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

// RFC 2617 §3.5's own worked example. Pinning it means the Digest computation
// is verified against the standard rather than against itself — the failure
// this catches (a wrong HA2, a missing field in the response hash) presents on
// hardware as an indistinguishable "password rejected", which is exactly the
// symptom an operator would blame on the vault.
func TestBuildDigestAuthHeaderMatchesRFC2617Vector(t *testing.T) {
	got := buildDigestAuthHeader(digestParams{
		User:     "Mufasa",
		Password: "Circle Of Life",
		Realm:    "testrealm@host.com",
		Nonce:    "dcd98b7102dd2f0e8b11d0f600bfb0c093",
		Opaque:   "5ccc069c403ebaf9f0171e9517f40e41",
		QOP:      "auth",
		NC:       "00000001",
		CNonce:   "0a4f113b",
		Method:   "GET",
		URI:      "/dir/index.html",
	})
	const wantResponse = `response="6629fae49393a05397450978507c4ef1"`
	if !strings.Contains(got, wantResponse) {
		t.Fatalf("digest header %q does not carry the RFC 2617 canonical %s", got, wantResponse)
	}
}

// Without qop the response is the RFC 2069 three-part hash, not the six-part
// one. Answering a qop-less challenge with a qop-shaped response is rejected
// by the server, so the two forms must genuinely differ.
func TestBuildDigestAuthHeaderWithoutQOP(t *testing.T) {
	base := digestParams{
		User: "rokudev", Password: "pw", Realm: "roku", Nonce: "n",
		NC: "00000001", CNonce: "cn", Method: "POST", URI: "/plugin_install",
	}
	withQOP := base
	withQOP.QOP = "auth"

	plain := buildDigestAuthHeader(base)
	qop := buildDigestAuthHeader(withQOP)
	if plain == qop {
		t.Fatal("qop and no-qop produced identical headers; the response hash must differ")
	}
	if strings.Contains(plain, "qop=") || strings.Contains(plain, "cnonce=") {
		t.Errorf("no-qop header claims qop fields: %q", plain)
	}
}

func TestParseDigestChallenge(t *testing.T) {
	got := parseDigestChallenge(`Digest realm="rokudev", qop="auth", nonce=abc123, opaque="op", stale=FALSE`)
	for k, want := range map[string]string{"realm": "rokudev", "qop": "auth", "nonce": "abc123", "opaque": "op"} {
		if got[k] != want {
			t.Errorf("challenge[%q] = %q, want %q", k, got[k], want)
		}
	}
	if len(parseDigestChallenge("")) != 0 {
		t.Error("an absent challenge must decode to nothing, so the caller refuses rather than guessing a nonce")
	}
}

func TestSelectQOP(t *testing.T) {
	for offered, want := range map[string]string{
		"auth":              "auth",
		"auth-int, auth":    "auth",
		" AUTH ":            "auth",
		"auth-int":          "", // cannot be honoured: needs an entity-body hash
		"":                  "",
		"something-else":    "",
		"auth-int,md5-sess": "",
	} {
		if got := selectQOP(offered); got != want {
			t.Errorf("selectQOP(%q) = %q, want %q", offered, got, want)
		}
	}
}

// The installer answers HTTP 200 for both outcomes, so the scrape IS the
// result. Anything unrecognised must read as NOT installed — a captive portal
// or a different device at that address is not evidence of a successful build.
func TestParseInstallResult(t *testing.T) {
	if got := parseInstallResult(`<font color="red">Install Success.</font>`); !got.OK {
		t.Errorf("success page read as %+v", got)
	}
	failure := parseInstallResult(`<font color="red">Install Failure: Compilation Failed.</font>`)
	if failure.OK {
		t.Fatalf("failure page read as OK: %+v", failure)
	}
	if !strings.Contains(failure.Detail, "Compilation Failed") {
		t.Errorf("failure detail %q drops the firmware's own reason", failure.Detail)
	}
	if unknown := parseInstallResult("<html>Sign in to the guest network</html>"); unknown.OK {
		t.Errorf("an unrecognised page read as OK: %+v", unknown)
	}
}

// devInstallerStub is a stand-in for the Roku developer web installer: it
// challenges the first request, verifies the replayed Digest response against
// the password it holds, and answers in HTML with a 200 either way — the three
// behaviours that make this surface awkward.
type devInstallerStub struct {
	password string
	// requests counts POSTs received, so a test can assert the body was
	// replayed rather than re-sent from a consumed stream.
	requests int
	// lastArchive is the archive part's bytes as the server received them.
	lastArchive []byte
	// lastSubmit is the `mysubmit` action the server received.
	lastSubmit string
	// answer is the HTML the authenticated request gets.
	answer string
	// challenge, when false, skips the 401 entirely — the firmware revision
	// that accepts an unauthenticated install.
	challenge bool
	// delay stalls the first response, for the per-device timeout test.
	delay time.Duration
}

func (s *devInstallerStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.requests++
	if s.delay > 0 {
		select {
		case <-time.After(s.delay):
		case <-r.Context().Done():
			return
		}
	}
	if s.challenge && r.Header.Get("Authorization") == "" {
		w.Header().Set("WWW-Authenticate", `Digest realm="rokudev", qop="auth", nonce="deadbeef", opaque="op"`)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, "<html>401</html>")
		return
	}
	if s.challenge && !s.authorized(r) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, "<html>401</html>")
		return
	}
	s.lastArchive, s.lastSubmit = formParts(r)
	_, _ = io.WriteString(w, s.answer)
}

// authorized recomputes the expected Digest response from the challenge it
// issued and compares — a real credential check, not a "did they send a header"
// check, so a wrong password genuinely fails.
func (s *devInstallerStub) authorized(r *http.Request) bool {
	fields := parseDigestChallenge(r.Header.Get("Authorization"))
	want := buildDigestAuthHeader(digestParams{
		User:     fields["username"],
		Password: s.password,
		Realm:    "rokudev",
		Nonce:    "deadbeef",
		Opaque:   "op",
		QOP:      "auth",
		NC:       fields["nc"],
		CNonce:   fields["cnonce"],
		Method:   http.MethodPost,
		URI:      fields["uri"],
	})
	return strings.Contains(want, `response="`+fields["response"]+`"`)
}

// formParts extracts the uploaded archive bytes and the `mysubmit` action, so
// a test can assert both actually crossed the wire on the AUTHENTICATED
// request — the installer rejects a request missing either.
func formParts(r *http.Request) (archive []byte, submit string) {
	_, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil {
		return nil, ""
	}
	mr := multipart.NewReader(r.Body, params["boundary"])
	for {
		part, err := mr.NextPart()
		if err != nil {
			return archive, submit
		}
		data, _ := io.ReadAll(part)
		switch part.FormName() {
		case "archive":
			archive = data
		case "mysubmit":
			submit = string(data)
		}
	}
}

// stubDevice points a device at an httptest server.
func stubDevice(t *testing.T, srv *httptest.Server) device {
	t.Helper()
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse stub URL: %v", err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("stub port: %v", err)
	}
	return device{Name: "stub", Host: u.Hostname(), Port: port}
}

func TestInstallChannelPerformsDigestDanceAndUploadsZip(t *testing.T) {
	stub := &devInstallerStub{password: "abcd", answer: "<html>Install Success</html>", challenge: true}
	srv := httptest.NewServer(stub)
	defer srv.Close()

	zip := []byte("PK\x03\x04 pretend channel bytes")
	outcome, err := installChannel(context.Background(), srv.Client(), stubDevice(t, srv),
		credentials{User: "rokudev", Password: "abcd"}, zip)
	if err != nil {
		t.Fatalf("installChannel: %v", err)
	}
	if !outcome.OK {
		t.Fatalf("outcome = %+v, want OK", outcome)
	}
	if stub.requests != 2 {
		t.Errorf("stub saw %d request(s), want 2 (the 401 then the authenticated replay)", stub.requests)
	}
	if string(stub.lastArchive) != string(zip) {
		t.Errorf("archive part = %q, want the channel bytes replayed intact on the authenticated request", stub.lastArchive)
	}
	// `Install` fails on a device that already has a dev channel, which is
	// EVERY device on the second and later fleet updates. See installAction.
	if stub.lastSubmit != "Replace" {
		t.Errorf("mysubmit = %q, want \"Replace\" — a fleet update always installs over an existing channel", stub.lastSubmit)
	}
}

func TestInstallChannelWrongPassword(t *testing.T) {
	stub := &devInstallerStub{password: "abcd", answer: "<html>Install Success</html>", challenge: true}
	srv := httptest.NewServer(stub)
	defer srv.Close()

	_, err := installChannel(context.Background(), srv.Client(), stubDevice(t, srv),
		credentials{User: "rokudev", Password: "wrong"}, []byte("zip"))
	if err == nil {
		t.Fatal("a rejected password installed successfully")
	}
	if strings.Contains(err.Error(), "wrong") {
		t.Fatalf("the error echoes the password: %v", err)
	}
}

func TestInstallChannelWithoutChallenge(t *testing.T) {
	// Some firmware answers the very first POST. Re-sending a multi-megabyte
	// archive to "confirm" that would double every sideload.
	stub := &devInstallerStub{answer: "<html>Install Success</html>"}
	srv := httptest.NewServer(stub)
	defer srv.Close()

	outcome, err := installChannel(context.Background(), srv.Client(), stubDevice(t, srv),
		credentials{User: "rokudev", Password: "abcd"}, []byte("zip"))
	if err != nil {
		t.Fatalf("installChannel: %v", err)
	}
	if !outcome.OK {
		t.Fatalf("outcome = %+v, want OK", outcome)
	}
	if stub.requests != 1 {
		t.Errorf("stub saw %d request(s), want 1 — an unchallenged answer must not be retried", stub.requests)
	}
}

func TestInstallChannelRejects401WithoutNonce(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	_, err := installChannel(context.Background(), srv.Client(), stubDevice(t, srv),
		credentials{User: "rokudev", Password: "abcd"}, []byte("zip"))
	if err == nil {
		t.Fatal("a 401 with no Digest challenge was treated as installable")
	}
	if !strings.Contains(err.Error(), "Digest challenge") {
		t.Errorf("error %q does not point at the missing challenge", err)
	}
}

// A wedged screen must be abandoned on the caller's deadline, not hold the
// serial walk open. This is the property the per-device timeout exists for.
func TestInstallChannelHonoursContextDeadline(t *testing.T) {
	stub := &devInstallerStub{password: "abcd", answer: "<html>Install Success</html>", challenge: true, delay: 2 * time.Second}
	srv := httptest.NewServer(stub)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	started := time.Now()
	if _, err := installChannel(ctx, srv.Client(), stubDevice(t, srv),
		credentials{User: "rokudev", Password: "abcd"}, []byte("zip")); err == nil {
		t.Fatal("a stalled installer returned success")
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Errorf("gave up after %s; the deadline was 100ms", elapsed)
	}
}

// rokuLikeInstaller behaves the way a real Roku dev server does, which is what
// the previous implementation could not survive.
//
// The distinguishing behaviour: an UNAUTHENTICATED POST carrying a body is not
// politely answered 401 — the device hangs up mid-upload. Against real hardware
// that surfaces as "use of closed network connection" on the write, which reads
// like a network fault and is not one. A stub that merely returns 401 to the
// first POST cannot tell the two implementations apart, which is why the
// existing stub above passed a client that could never install anything.
type rokuLikeInstaller struct {
	password string
	// bodiedPostsWithoutAuth counts speculative uploads — the thing being fixed.
	bodiedPostsWithoutAuth int
	installed              bool
}

func (s *rokuLikeInstaller) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	auth := r.Header.Get("Authorization")

	// A bodyless GET is how a well-behaved client asks for the challenge.
	if r.Method == http.MethodGet {
		w.Header().Set("WWW-Authenticate", `Digest qop="auth", realm="rokudev", nonce="1787341409"`)
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	if auth == "" {
		// The Roku's actual behaviour: refuse by hanging up, not by replying.
		s.bodiedPostsWithoutAuth++
		if hj, ok := w.(http.Hijacker); ok {
			conn, _, err := hj.Hijack()
			if err == nil {
				_ = conn.Close()
				return
			}
		}
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if !strings.Contains(auth, `realm="rokudev"`) {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	s.installed = true
	_, _ = io.WriteString(w, "<html><font color=\"red\">Install Success</font></html>")
}

// TestInstallAuthenticatesBEFORESendingTheArchive is the case the shipped tool
// failed against real hardware.
func TestInstallAuthenticatesBEFORESendingTheArchive(t *testing.T) {
	stub := &rokuLikeInstaller{password: "abcd"}
	srv := httptest.NewServer(stub)
	defer srv.Close()

	_, err := digestPost(context.Background(), srv.Client(), stubDevice(t, srv),
		credentials{User: "rokudev", Password: "abcd"},
		"/plugin_install", []byte("PK\x03\x04 pretend this is 90 KB of channel"), "multipart/form-data; boundary=x")
	if err != nil {
		t.Fatalf("install failed against a Roku-like device: %v", err)
	}
	if stub.bodiedPostsWithoutAuth != 0 {
		t.Errorf("the archive was offered %d time(s) before authenticating — a real Roku hangs up "+
			"mid-upload and the retry writes into a dead socket", stub.bodiedPostsWithoutAuth)
	}
	if !stub.installed {
		t.Error("the device never received an authenticated upload")
	}
}

// TestTheChallengeRequestCarriesNoBody pins the property the fix turns on: what
// makes the speculative upload avoidable is that `qop="auth"` excludes the
// entity body, so a challenge can be had for free.
func TestTheChallengeRequestCarriesNoBody(t *testing.T) {
	var challengeMethod string
	var challengeLen int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			challengeMethod, challengeLen = r.Method, r.ContentLength
		}
		w.Header().Set("WWW-Authenticate", `Digest qop="auth", realm="rokudev", nonce="n"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	_, _ = digestChallenge(context.Background(), srv.Client(), stubDevice(t, srv))
	if challengeMethod != http.MethodGet {
		t.Errorf("challenge fetched with %q, want GET — a POST invites the body this exists to avoid", challengeMethod)
	}
	if challengeLen > 0 {
		t.Errorf("the challenge request carried %d bytes of body; it must risk nothing", challengeLen)
	}
}
