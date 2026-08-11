package packs_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/maaxton/waiveo-next/internal/app/packs"
)

// httpfetcher_test.go pins the confinements that keep an index author's chosen
// `download_url` from becoming a request this appliance would not otherwise make.
//
// Every one of them is keyed on a value the HOST configured — the index url —
// and none of them is masked by a later check: the digest and signature gates
// downstream decide whether bytes are ACCEPTABLE, and say nothing about whether
// the box should have gone and asked for them. A request forged out of a
// tampered index is already a success for the attacker whatever the box does
// with the response.

// registryServer starts a TLS test registry serving the given path→body map and
// returns a fetcher already pointed at /index.json on it, trusting its cert.
func registryServer(t *testing.T, bodies map[string]string) (*httptest.Server, packs.HTTPFetcher) {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := bodies[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, packs.HTTPFetcher{Base: srv.URL + "/index.json", Client: srv.Client()}
}

// TestHTTPFetcherServesTheIndexAndResolvesRelativeDownloadURLs is the working
// case, and it carries the relative-reference rule with it.
//
// The rule is not a convenience: an index that had to spell its own hostname in
// every `download_url` could not be mirrored, moved, or served under a second
// name without rewriting every entry — and each rewrite is a chance for a URL to
// end up pointing somewhere else.
func TestHTTPFetcherServesTheIndexAndResolvesRelativeDownloadURLs(t *testing.T) {
	_, f := registryServer(t, map[string]string{
		"/index.json":               `{"signed":{}}`,
		"/artifacts/menu-1.0.0.zip": "pack bytes",
	})

	got, err := f.Fetch(context.Background(), f.Base)
	if err != nil {
		t.Fatalf("fetch the index: %v", err)
	}
	if string(got) != `{"signed":{}}` {
		t.Fatalf("index = %q", got)
	}

	// A relative download_url, resolved against the index's own url.
	got, err = f.Fetch(context.Background(), "artifacts/menu-1.0.0.zip")
	if err != nil {
		t.Fatalf("fetch a relative download_url: %v", err)
	}
	if string(got) != "pack bytes" {
		t.Fatalf("artifact = %q", got)
	}
}

// TestHTTPFetcherRefusesLeavingTheConfiguredOrigin is the SSRF confinement.
//
// The index is untrusted transport — its signature chain does not exist yet
// (marketplace.go's package doc) — so whoever serves or tampers with it chooses
// every `download_url` in it. Unconfined, that is a request-forgery primitive
// originating INSIDE the LAN the appliance sits in, which can reach the router's
// admin page and the other appliances. The refusal must name the origin rule,
// not merely fail: a fetch to an unreachable host fails too, and means something
// entirely different.
func TestHTTPFetcherRefusesLeavingTheConfiguredOrigin(t *testing.T) {
	// A SECOND live server, so a fetcher that ignored the origin check would
	// SUCCEED rather than fail on an unroutable address. Without this the test
	// could not tell "refused" from "could not connect".
	elsewhere, _ := registryServer(t, map[string]string{"/secret": "internal data"})
	_, f := registryServer(t, map[string]string{"/index.json": `{}`})

	got, err := f.Fetch(context.Background(), elsewhere.URL+"/secret")
	if err == nil {
		t.Fatalf("an index author's cross-origin download_url was fetched, returning %q — the box will make requests to any host an index names", got)
	}
	if !strings.Contains(err.Error(), "origin") {
		t.Errorf("refused with %q, want the origin rule — any other refusal means the request was attempted", err)
	}
}

// TestHTTPFetcherRefusesARedirect: a redirect is a second URL the registry did
// not have to publish and the operator never saw. Following one reintroduces the
// cross-origin hop the origin check exists to prevent, one Location header later
// — so the origin check alone is not sufficient, which is why this is its own
// rule and its own test.
func TestHTTPFetcherRefusesARedirect(t *testing.T) {
	elsewhere, _ := registryServer(t, map[string]string{"/secret": "internal data"})

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL+"/secret", http.StatusFound)
	}))
	t.Cleanup(srv.Close)
	f := packs.HTTPFetcher{Base: srv.URL + "/index.json", Client: srv.Client()}

	got, err := f.Fetch(context.Background(), f.Base)
	if err == nil {
		t.Fatalf("a redirect off the configured origin was followed, returning %q", got)
	}
	if !strings.Contains(err.Error(), "redirect") {
		t.Errorf("refused with %q, want the redirect rule", err)
	}
}

// TestHTTPFetcherCapsTheResponse pins the byte cap, and pins it against a server
// that declares NO Content-Length.
//
// The declared-length pre-check is an optimisation over a value the far end
// chooses; a registry that omits or lies about it must still not be able to make
// this box allocate without bound. So the cap has to hold on the read itself,
// and this test is written so that it is the read that has to hold it.
func TestHTTPFetcherCapsTheResponse(t *testing.T) {
	const cap = 1024
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Chunked: no Content-Length reaches the client.
		flusher, _ := w.(http.Flusher)
		for i := 0; i < 8; i++ {
			_, _ = w.Write([]byte(strings.Repeat("x", 512)))
			if flusher != nil {
				flusher.Flush()
			}
		}
	}))
	t.Cleanup(srv.Close)
	f := packs.HTTPFetcher{Base: srv.URL + "/index.json", Client: srv.Client(), MaxBytes: cap}

	got, err := f.Fetch(context.Background(), f.Base)
	if err == nil {
		t.Fatalf("a 4096-byte response passed a %d-byte cap, returning %d bytes", cap, len(got))
	}
	if !strings.Contains(err.Error(), "cap") {
		t.Errorf("refused with %q, want the byte cap", err)
	}
}

// TestHTTPFetcherRefusesADeclaredOversizeBeforeReading is the other half: when
// the far end IS honest about the length, the object is refused on the header
// and the body is never read.
//
// The refusal message is asserted, because it is the only thing that
// distinguishes the two paths from outside — an over-cap object is refused
// either way, and a test that checked only "refused" would pass with the
// pre-check deleted.
func TestHTTPFetcherRefusesADeclaredOversizeBeforeReading(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := strings.Repeat("x", 4096)
		w.Header().Set("Content-Length", fmt.Sprint(len(body)))
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	f := packs.HTTPFetcher{Base: srv.URL + "/index.json", Client: srv.Client(), MaxBytes: 1024}

	_, err := f.Fetch(context.Background(), f.Base)
	if err == nil {
		t.Fatal("a response declaring 4096 bytes passed a 1024-byte cap")
	}
	if !strings.Contains(err.Error(), "declares") {
		t.Errorf("refused with %q, want the declared-length pre-check — the transfer ran before being refused", err)
	}
}

// TestHTTPFetcherTimesOutRatherThanHanging: a registry that accepts the
// connection and then stalls must not hold this fetch open forever. On the boot
// path and inside an install request alike, an unbounded fetch is a hang with no
// diagnosis rather than a refusal an operator can read.
func TestHTTPFetcherTimesOutRatherThanHanging(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release // stall until the test lets go
	}))
	t.Cleanup(func() { close(release); srv.Close() })
	f := packs.HTTPFetcher{Base: srv.URL + "/index.json", Client: srv.Client(), Timeout: 150 * time.Millisecond}

	done := make(chan error, 1)
	go func() { _, err := f.Fetch(context.Background(), f.Base); done <- err }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a stalled registry returned success")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the fetch did not honour its timeout — a stalled registry hangs the caller")
	}
}

// TestHTTPFetcherRefusesANonOKStatus: a 404 or a 500 body is not an index. Read
// as one it would parse as a document with no artifacts, which resolves to
// MARKETPLACE_REF_UNRESOLVED — the same answer as a working registry that has
// not published the pack yet, and a far worse thing to tell an operator.
func TestHTTPFetcherRefusesANonOKStatus(t *testing.T) {
	_, f := registryServer(t, map[string]string{}) // serves 404 for everything

	if _, err := f.Fetch(context.Background(), f.Base); err == nil {
		t.Fatal("a 404 body was returned as content")
	}
}

// TestHTTPFetcherRefusesPlaintextAndCredentials covers the two url shapes that
// are refused before any connection is made.
func TestHTTPFetcherRefusesPlaintextAndCredentials(t *testing.T) {
	_, f := registryServer(t, map[string]string{"/index.json": `{}`})

	// An index entry may not downgrade a source to plaintext for one object.
	if _, err := f.Fetch(context.Background(), "http://"+strings.TrimPrefix(f.Base, "https://")); err == nil {
		t.Error("a plaintext download_url was fetched")
	}
	// …nor make the box present credentials to a host it reaches.
	if _, err := f.Fetch(context.Background(), strings.Replace(f.Base, "https://", "https://user:pw@", 1)); err == nil {
		t.Error("a download_url carrying credentials was fetched")
	}
}

// TestHTTPFetcherRefusesANonHTTPSBase closes the loop at the other end: the
// fetcher must not fall back to plaintext because its own configuration was
// plaintext. LoadSources refuses an http:// index url, so this is the guard
// behind that guard — the value could also arrive from a programmatic caller.
func TestHTTPFetcherRefusesANonHTTPSBase(t *testing.T) {
	f := packs.HTTPFetcher{Base: "http://registry.example/index.json"}

	if _, err := f.Fetch(context.Background(), "artifact.zip"); err == nil {
		t.Fatal("a plaintext base url produced a fetch")
	}
}
