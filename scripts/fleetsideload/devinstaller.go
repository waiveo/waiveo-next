package main

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"regexp"
	"strings"
)

// The Roku developer installer is an HTML form, not an API, and this file
// speaks it exactly as `curl --user U:P --digest -F mysubmit=Install -F
// archive=@zip http://<ip>/plugin_install` does — the same handshake the
// legacy fleet updater used against this same hardware for years.
//
// Three properties of that surface drive everything below:
//
//  1. It authenticates with RFC 2617 HTTP Digest (MD5). Go's net/http has no
//     Digest support and this repo has no dependency that does, so the
//     challenge/response is computed here. MD5 is not a security choice — it
//     is what the firmware implements, and there is no negotiation to a
//     better one.
//
//  2. It answers the FIRST request with 401 + a nonce and only then accepts
//     the body. The multipart body is therefore built ONCE into a byte slice
//     and replayed on the retry: a streaming body cannot be re-sent, and a
//     rebuilt one would carry a fresh boundary the server has no reason to
//     accept but every reason to make debugging harder.
//
//  3. It returns HTTP 200 for BOTH outcomes. The only success signal is the
//     literal "Install Success" in the HTML body. A status-code check here
//     would report a green fleet update that installed nothing at all, which
//     is worse than no tool: the operator walks away.

// installOutcome is one device's decoded /plugin_install answer.
type installOutcome struct {
	// OK is the "Install Success" reading, and nothing else — never a status
	// code (see this file's own note 3).
	OK bool
	// Detail is the scraped line to show the operator: the success marker, the
	// firmware's own failure text, or a note that neither appeared.
	Detail string
}

// installChannel sideloads zip onto one device's dev installer and reports
// what the firmware said. ctx bounds the whole exchange, both requests
// included, so a wedged screen cannot hold the serial fleet walk open past the
// caller's per-device deadline.
func installChannel(ctx context.Context, client *http.Client, dev device, creds credentials, zip []byte) (installOutcome, error) {
	body, contentType, err := buildInstallForm(zip)
	if err != nil {
		return installOutcome{}, err
	}
	html, err := digestPost(ctx, client, dev, creds, "/plugin_install", body, contentType)
	if err != nil {
		return installOutcome{}, err
	}
	return parseInstallResult(html), nil
}

// installAction is the form's `mysubmit` value.
//
// `Replace`, not `Install`, and this is not interchangeable: `Install` is the
// first-time action and a device that already has a dev channel answers it
// with a failure. A FLEET update is by definition re-installing over an
// existing channel on every device, so `Install` would fail on all of them
// after the first round — while `Replace` is what this fleet's own firmware
// was verified against by hand (docs/runbooks/first-photon.md §3) and works
// whether or not a channel is already present.
const installAction = "Replace"

// buildInstallForm builds the exact two-part form the installer expects —
// `mysubmit` plus the archive as a file part — returning the finished bytes
// and their boundary-carrying content type.
//
// BOTH parts are required: the installer rejects a request missing either with
// "mysubmit Field Not Found" rather than anything resembling a useful error.
// Built into memory on purpose (see this file's own note 2): the same bytes
// are replayed after the 401.
func buildInstallForm(zip []byte) ([]byte, string, error) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	if err := w.WriteField("mysubmit", installAction); err != nil {
		return nil, "", fmt.Errorf("build install form: %w", err)
	}
	// CreateFormFile would stamp Content-Type: application/octet-stream; the
	// installer is happier being told it is a zip, matching curl's own
	// `-F archive=@x.zip;type=application/zip` shape.
	h := make(textproto.MIMEHeader)
	h.Set("Content-Disposition", `form-data; name="archive"; filename="waiveo-player.zip"`)
	h.Set("Content-Type", "application/zip")
	part, err := w.CreatePart(h)
	if err != nil {
		return nil, "", fmt.Errorf("build install form: %w", err)
	}
	if _, err := part.Write(zip); err != nil {
		return nil, "", fmt.Errorf("build install form: %w", err)
	}
	if err := w.Close(); err != nil {
		return nil, "", fmt.Errorf("build install form: %w", err)
	}
	return buf.Bytes(), w.FormDataContentType(), nil
}

// digestPost performs the 401 -> nonce -> retry dance against uri and returns
// the authenticated response body.
//
// A server that answers the first request WITHOUT challenging is honoured as
// answered rather than retried: some firmware revisions skip the challenge
// when a dev channel has never been installed, and re-POSTing a multi-megabyte
// archive to "confirm" that would double every sideload's wall time.
func digestPost(ctx context.Context, client *http.Client, dev device, creds credentials, uri string, body []byte, contentType string) (string, error) {
	url := "http://" + dev.Addr() + uri

	// The challenge is taken from a request that carries NO BODY, and that is
	// the whole of this function's correctness.
	//
	// The obvious shape — POST the archive, take the 401, POST it again with the
	// credential — does not work against a real Roku. It streams ~90 KB to a
	// device that has already decided to refuse it, and the device closes the
	// connection mid-write; the retry then writes into a dead socket and the
	// install fails with "use of closed network connection", which reads like a
	// network fault and is not one. Measured against a Roku on OS 15.3.4, from
	// two different network paths, with credentials proven good.
	//
	// `qop="auth"` does not cover the entity body (that is `auth-int`), so the
	// digest response can be computed from a challenge obtained anywhere in the
	// realm and attached to a single, fully-formed POST. One request, one body,
	// no speculative upload.
	challenge, err := digestChallenge(ctx, client, dev)
	if err != nil {
		return "", err
	}
	if challenge["nonce"] == "" {
		return "", fmt.Errorf("%s did not present a Digest challenge (is this the dev installer port?)", dev.Addr())
	}
	cnonce, err := randomCnonce()
	if err != nil {
		return "", err
	}
	auth := buildDigestAuthHeader(digestParams{
		User:     creds.User,
		Password: creds.Password,
		Realm:    challenge["realm"],
		Nonce:    challenge["nonce"],
		Opaque:   challenge["opaque"],
		QOP:      selectQOP(challenge["qop"]),
		NC:       "00000001",
		CNonce:   cnonce,
		Method:   http.MethodPost,
		URI:      uri,
	})

	second, err := postBytes(ctx, client, url, contentType, body, auth)
	if err != nil {
		return "", err
	}
	if second.status == http.StatusUnauthorized {
		// Deliberately does not echo the credential or the realm: this line
		// ends up in a terminal, a CI log, and a screenshot.
		return "", fmt.Errorf("%s rejected the dev password for user %q", dev.Addr(), creds.User)
	}
	return second.body, nil
}

// digestChallenge fetches a Digest challenge without offering a body.
//
// It asks `/plugin_inspect` — a GET on the same dev-installer port, in the same
// `rokudev` realm — because a challenge is realm-scoped and the point is to
// obtain one having risked nothing. Any 401 on this port serves; this endpoint
// is the one verified against real hardware.
//
// A non-401 answer is not an error here: a device that does not challenge is one
// this function has nothing to add to, and the caller's `nonce == ""` check
// turns that into the "is this the dev installer port?" message an operator can
// act on.
func digestChallenge(ctx context.Context, client *http.Client, dev device) (map[string]string, error) {
	url := "http://" + dev.Addr() + "/plugin_inspect"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build challenge request for %s: %w", url, err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s unreachable: %w", url, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusUnauthorized {
		return map[string]string{}, nil
	}
	return parseDigestChallenge(resp.Header.Get("WWW-Authenticate")), nil
}

// httpAnswer is the only three things the caller needs from a response, read
// fully so the connection is reusable for the retry.
type httpAnswer struct {
	status       int
	authenticate string
	body         string
}

// postBytes POSTs body once, reading the response fully. A transport error is
// wrapped with the address, because "connection refused" with no address in it
// is unactionable when seven devices are being walked.
func postBytes(ctx context.Context, client *http.Client, url, contentType string, body []byte, authorization string) (httpAnswer, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return httpAnswer{}, fmt.Errorf("build request for %s: %w", url, err)
	}
	req.Header.Set("Content-Type", contentType)
	req.ContentLength = int64(len(body))
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	resp, err := client.Do(req)
	if err != nil {
		return httpAnswer{}, fmt.Errorf("%s unreachable: %w", url, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return httpAnswer{}, fmt.Errorf("%s: read response: %w", url, err)
	}
	return httpAnswer{
		status:       resp.StatusCode,
		authenticate: resp.Header.Get("WWW-Authenticate"),
		body:         string(raw),
	}, nil
}

// digestParams is one RFC 2617 response computation's inputs. Kept as a struct
// so buildDigestAuthHeader is a pure function of named values and can be
// asserted against the RFC's own canonical vector.
type digestParams struct {
	User     string
	Password string
	Realm    string
	Nonce    string
	Opaque   string
	QOP      string
	NC       string
	CNonce   string
	Method   string
	URI      string
}

// buildDigestAuthHeader computes the RFC 2617 `Authorization: Digest ...`
// header value. With qop present the response is
// MD5(HA1:nonce:nc:cnonce:qop:HA2); without it, the RFC 2069 form
// MD5(HA1:nonce:HA2) — some firmware omits qop entirely, and answering with a
// qop-shaped response to a challenge that offered none is rejected.
func buildDigestAuthHeader(p digestParams) string {
	ha1 := md5hex(p.User + ":" + p.Realm + ":" + p.Password)
	ha2 := md5hex(p.Method + ":" + p.URI)

	var response string
	if p.QOP != "" {
		response = md5hex(strings.Join([]string{ha1, p.Nonce, p.NC, p.CNonce, p.QOP, ha2}, ":"))
	} else {
		response = md5hex(strings.Join([]string{ha1, p.Nonce, ha2}, ":"))
	}

	parts := []string{
		`username="` + p.User + `"`,
		`realm="` + p.Realm + `"`,
		`nonce="` + p.Nonce + `"`,
		`uri="` + p.URI + `"`,
		"algorithm=MD5",
		`response="` + response + `"`,
	}
	if p.QOP != "" {
		parts = append(parts, "qop="+p.QOP, "nc="+p.NC, `cnonce="`+p.CNonce+`"`)
	}
	if p.Opaque != "" {
		parts = append(parts, `opaque="`+p.Opaque+`"`)
	}
	return "Digest " + strings.Join(parts, ", ")
}

// challengeField matches one `key=value` or `key="value"` pair of a
// WWW-Authenticate challenge.
var challengeField = regexp.MustCompile(`([A-Za-z0-9_-]+)\s*=\s*(?:"([^"]*)"|([^,\s]+))`)

// parseDigestChallenge decodes a `WWW-Authenticate: Digest ...` header into
// lower-cased field names. An absent or non-Digest header decodes to an empty
// map, which the caller reads as "no nonce" and refuses.
func parseDigestChallenge(header string) map[string]string {
	out := map[string]string{}
	raw := strings.TrimSpace(header)
	if i := strings.Index(strings.ToLower(raw), "digest "); i == 0 {
		raw = raw[len("digest "):]
	}
	for _, m := range challengeField.FindAllStringSubmatch(raw, -1) {
		value := m[2]
		if value == "" {
			value = m[3]
		}
		out[strings.ToLower(m[1])] = value
	}
	return out
}

// selectQOP picks the qop token to answer with. Roku advertises `auth`; a
// server offering `auth,auth-int` still gets plain `auth`, and one offering
// only `auth-int` (which would require hashing the entity body) gets "" — the
// RFC 2069 fallback — rather than a claim this client cannot honour.
func selectQOP(offered string) string {
	for _, token := range strings.Split(offered, ",") {
		if strings.EqualFold(strings.TrimSpace(token), "auth") {
			return "auth"
		}
	}
	return ""
}

// installFailure captures the firmware's own failure line so the operator sees
// WHY ("Install Failure: Compilation Failed") rather than a bare "failed".
var installFailure = regexp.MustCompile(`Install Failure[^<\r\n]*`)

// parseInstallResult scrapes the installer's HTML answer (see this file's own
// note 3: the status code carries no outcome).
//
// Anything that is neither marker is reported as NOT ok. That is the whole
// point: an unrecognised page is most often the router's captive portal, a
// different device at that address, or a firmware that changed its wording —
// none of which are evidence that a build was installed, and all of which a
// "assume success unless it says otherwise" reading would silently pass.
func parseInstallResult(html string) installOutcome {
	if strings.Contains(html, "Install Success") {
		return installOutcome{OK: true, Detail: "Install Success"}
	}
	if m := installFailure.FindString(html); m != "" {
		return installOutcome{OK: false, Detail: strings.TrimSpace(m)}
	}
	return installOutcome{OK: false, Detail: "installer answered without an Install Success/Failure marker"}
}

// md5hex is RFC 2617's H() — MD5, hex, lower case. The algorithm is dictated
// by the firmware's Digest implementation; it is not protecting anything at
// rest here.
func md5hex(s string) string {
	sum := md5.Sum([]byte(s)) //nolint:gosec // RFC 2617 Digest is MD5 by definition
	return hex.EncodeToString(sum[:])
}

// randomCnonce mints the client nonce. It comes from crypto/rand rather than a
// counter because a predictable cnonce lets a passive observer of one exchange
// precompute responses for others against the same credential.
func randomCnonce() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate client nonce: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}
