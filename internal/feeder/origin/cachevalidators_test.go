package origin

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/maaxton/waiveo-next/internal/shared/apihttp"
)

// cachevalidators_test.go covers the origin's cache validators (parity row 2.6's
// server half): a strong ETag and an `immutable` freshness lifetime on every
// content response, and the bodyless 304 they buy a client that already holds
// the bytes.
//
// The point of asserting these is not that HTTP works. It is that they are only
// SAFE because this origin is content-addressed — the request path is the sha256
// of the response body — and an unconditional `immutable` on a resource that
// could ever change would be a permanently-poisoned client cache with no way to
// bust it. The digest-as-ETag assertion below is what pins that reasoning to the
// implementation: if the URL ever stops being the content address, this test is
// the thing that has to be argued with.

// servedOrigin returns an in-memory store holding `body`, a TLS test server over
// its handler, and the hex digest the bytes are addressed by.
func servedOrigin(t *testing.T, body []byte, opts ...Option) (*httptest.Server, string) {
	t.Helper()
	o := New(opts...)
	assetRef, err := o.Add(body)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	srv := httptest.NewTLSServer(apihttp.WithTraceID(o.Handler()))
	t.Cleanup(srv.Close)
	return srv, strings.TrimPrefix(assetRef, "sha256:")
}

// TestContentResponseCarriesTheDigestAsAStrongETag: the validator IS the content
// address, quoted (a strong validator — the bytes are byte-for-byte what the
// digest names, so nothing about this is "semantically equivalent").
func TestContentResponseCarriesTheDigestAsAStrongETag(t *testing.T) {
	srv, hexDigest := servedOrigin(t, []byte("waiveo-next: a scheduled clip"))

	resp, err := srv.Client().Get(srv.URL + "/content/" + hexDigest)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	want := `"` + hexDigest + `"`
	if got := resp.Header.Get("ETag"); got != want {
		t.Errorf("ETag = %q, want %q (the content address itself — a client that already holds these bytes knows it by this name and nothing else)", got, want)
	}
	cc := resp.Header.Get("Cache-Control")
	if !strings.Contains(cc, "immutable") {
		t.Errorf("Cache-Control = %q, want it to declare immutable: bytes cannot change under their own hash, so a revalidation on reload is pure waste on a video-sized asset", cc)
	}
	if !strings.Contains(cc, "max-age=") {
		t.Errorf("Cache-Control = %q, want a max-age; `immutable` alone gives a client no freshness lifetime to work from", cc)
	}
	if !strings.Contains(cc, "public") {
		t.Errorf("Cache-Control = %q, want public for an unsigned origin: the bytes are served to anyone who knows the digest, so a shared cache grants nothing the origin does not", cc)
	}
}

// TestConditionalContentRequestIs304WithNoBody is the saving itself: a client
// presenting the ETag it already holds gets a 304 and no bytes.
//
// This is what makes the whole-file re-download a screen used to perform on
// every ~10s poll unnecessary for any HTTP-caching client, and it is the reason
// the header is set BEFORE http.ServeContent rather than after — ServeContent
// reads it to answer the conditional, so a header written afterwards would be
// ignored and every request would ship the full body regardless.
func TestConditionalContentRequestIs304WithNoBody(t *testing.T) {
	payload := []byte(strings.Repeat("video-ish bytes ", 1024))
	srv, hexDigest := servedOrigin(t, payload)

	req, err := http.NewRequest(http.MethodGet, srv.URL+"/content/"+hexDigest, nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("If-None-Match", `"`+hexDigest+`"`)

	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("GET (conditional): %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotModified {
		t.Fatalf("status = %d, want 304 for a client presenting the digest it already holds", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if len(body) != 0 {
		t.Errorf("a 304 carried %d bytes of body; the whole point is that it carries none", len(body))
	}

	// A NON-matching validator still gets the full body — a cache holding some
	// other asset must not be told it is up to date.
	req2, err := http.NewRequest(http.MethodGet, srv.URL+"/content/"+hexDigest, nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req2.Header.Set("If-None-Match", `"0000000000000000000000000000000000000000000000000000000000000000"`)
	resp2, err := srv.Client().Do(req2)
	if err != nil {
		t.Fatalf("GET (stale conditional): %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 for a client holding a different validator", resp2.StatusCode)
	}
	got, err := io.ReadAll(resp2.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if string(got) != string(payload) {
		t.Error("the full response did not carry the stored bytes")
	}
}

// TestSignedOriginMarksContentPrivate: with signed content URLs in force, the
// response is per-client rather than shareable.
//
// The bytes are as immutable as ever — what changes is that the URL now carries
// an `exp` this origin enforces and a shared cache does not, so a stored
// response would keep being handed out for a signed URL whose permission has
// expired. `private` is the one part of the caching posture that is about
// authorization rather than about content addressing, and it is easy to lose by
// simplifying the header to a constant.
func TestSignedOriginMarksContentPrivate(t *testing.T) {
	srv, hexDigest := servedOrigin(t, []byte("waiveo-next: signed content"), WithSigningKey([]byte("a-content-url-signing-key")))

	// The fetch itself is refused without a signature — asserted here because it
	// is the premise: this store really is in the signed posture.
	resp, err := srv.Client().Get(srv.URL + "/content/" + hexDigest)
	if err != nil {
		t.Fatalf("GET (unsigned): %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 for an unsigned request against a signing origin", resp.StatusCode)
	}

	o := New(WithSigningKey([]byte("a-content-url-signing-key")))
	if got := o.cacheControl(); !strings.Contains(got, "private") {
		t.Errorf("a signing origin's Cache-Control = %q, want private: a shared cache does not enforce the URL's own exp", got)
	}
	if got := New().cacheControl(); !strings.Contains(got, "public") {
		t.Errorf("an unsigned origin's Cache-Control = %q, want public", got)
	}
}
