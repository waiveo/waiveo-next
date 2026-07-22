package webui

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

// TestHandlerServesShellAtRoot proves the embedded SPA is reachable: GET / returns
// 200 HTML carrying the app root marker. With only the committed placeholder in
// the embed tree this exercises the exact shell `go build ./...` ships, so the
// feeder always has a web UI to serve even before a real `make web-build`.
func TestHandlerServesShellAtRoot(t *testing.T) {
	h, err := Handler()
	if err != nil {
		t.Fatalf("Handler: %v", err)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("GET / status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("GET / content-type = %q, want text/html", ct)
	}
	body, _ := io.ReadAll(rec.Result().Body)
	if !strings.Contains(string(body), `<div id="root"`) {
		t.Errorf("GET / body missing the SPA root marker; got:\n%s", body)
	}
}

// TestHandlerIndexFallback proves a client-side route (an unknown, non-asset path)
// falls back to the index shell with 200 rather than 404, so a deep-link reload
// lands on the app and the SPA router takes over.
func TestHandlerIndexFallback(t *testing.T) {
	h, err := Handler()
	if err != nil {
		t.Fatalf("Handler: %v", err)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/design/some/deep/route", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("GET client route status = %d, want 200 (index fallback)", rec.Code)
	}
	body, _ := io.ReadAll(rec.Result().Body)
	if !strings.Contains(string(body), `<div id="root"`) {
		t.Errorf("index fallback body missing the SPA root marker; got:\n%s", body)
	}
}

// TestServeIndexPlaceholderFallback proves the embed-absent path: when the embed
// carries no built index.html (only the .gitkeep sentinel a fresh, un-built
// checkout ships), the handler serves the Go-string placeholder shell — a 200
// text/html body carrying BOTH the <title>Waiveo</title> marker the web-UI smoke
// asserts and the SPA root marker — rather than a 500. This is deterministic
// regardless of whether a local `make web-build` has populated the real embed.
func TestServeIndexPlaceholderFallback(t *testing.T) {
	empty := fstest.MapFS{".gitkeep": {Data: []byte{}}}
	h := &spaHandler{fsys: empty, files: http.FileServerFS(empty)}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("GET / status = %d, want 200 (placeholder fallback)", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("GET / content-type = %q, want text/html", ct)
	}
	body, _ := io.ReadAll(rec.Result().Body)
	if !strings.Contains(string(body), "<title>Waiveo</title>") {
		t.Errorf("placeholder shell missing the <title>Waiveo</title> marker; got:\n%s", body)
	}
	if !strings.Contains(string(body), `<div id="root"`) {
		t.Errorf("placeholder shell missing the SPA root marker; got:\n%s", body)
	}
}

// TestHandlerRejectsNonGET keeps the static surface read-only: a mutating method
// against the SPA mount is refused rather than silently falling through to the
// shell.
func TestHandlerRejectsNonGET(t *testing.T) {
	h, err := Handler()
	if err != nil {
		t.Fatalf("Handler: %v", err)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/", nil))

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST / status = %d, want 405", rec.Code)
	}
}
