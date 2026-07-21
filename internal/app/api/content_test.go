package api_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/maaxton/waiveo-next/internal/shared/signhash"
)

// contentUploadResponse is the 201 body POST /api/v1/content returns: the
// server-computed content-addressed asset_ref and the direct-fetch URL a screen
// resolves against the content origin (relay/1 REL-061).
type contentUploadResponse struct {
	AssetRef string `json:"asset_ref"`
	URL      string `json:"url"`
}

// TestContentUploadRoundTrip proves the asset-upload half of the content
// pipeline: POST bytes to /api/v1/content -> 201 with a server-computed
// sha256:<hex> asset_ref and a <base>/content/<hex> url; the shared origin
// store Has() the hash only after the upload; and a GET of the returned url's
// path against that SAME store's Handler() returns the exact uploaded bytes,
// which hash back to the asset_ref (content-addressing verified end to end; the
// relay is never in this path, REL-140).
func TestContentUploadRoundTrip(t *testing.T) {
	e := newEnv(t)

	asset := []byte("hello content pipeline\x00\x01\x02 the quick brown fox")
	wantRef := signhash.ContentID(asset)
	wantHex := strings.TrimPrefix(wantRef, "sha256:")

	// The content is not in the shared origin store before the upload.
	if e.content.Has(wantHex) {
		t.Fatalf("Has(%q) = true before upload; want false", wantHex)
	}

	resp, raw := e.do(t, http.MethodPost, "/api/v1/content", asset, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("upload status = %d, want 201 (body %s)", resp.StatusCode, raw)
	}

	var got contentUploadResponse
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode upload response: %v (body %s)", err, raw)
	}
	// asset_ref is server-COMPUTED from the bytes (a client-supplied ref is never
	// trusted): it must equal signhash.ContentID(asset).
	if got.AssetRef != wantRef {
		t.Fatalf("asset_ref = %q, want %q (server-computed sha256)", got.AssetRef, wantRef)
	}
	// The url is the single-sourced <base>/content/<hex> form snapshot.Build uses.
	wantURL := e.contentBase + "/content/" + wantHex
	if got.URL != wantURL {
		t.Fatalf("url = %q, want %q", got.URL, wantURL)
	}

	// The content is now in the shared origin store.
	if !e.content.Has(wantHex) {
		t.Fatalf("Has(%q) = false after upload; want true", wantHex)
	}

	// A GET of the returned url's PATH against the SAME store's Handler() returns
	// the exact uploaded bytes — the upload is immediately servable.
	u, err := url.Parse(got.URL)
	if err != nil {
		t.Fatalf("parse url %q: %v", got.URL, err)
	}
	cs := httptest.NewServer(e.content.Handler())
	t.Cleanup(cs.Close)

	fresp, err := http.Get(cs.URL + u.Path)
	if err != nil {
		t.Fatalf("GET content: %v", err)
	}
	defer fresp.Body.Close()
	fetched, err := io.ReadAll(fresp.Body)
	if err != nil {
		t.Fatalf("read content body: %v", err)
	}
	if fresp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200 (body %s)", u.Path, fresp.StatusCode, fetched)
	}
	if !bytes.Equal(fetched, asset) {
		t.Fatalf("fetched bytes != uploaded bytes (%d vs %d)", len(fetched), len(asset))
	}
	// Content-addressing: the fetched bytes hash back to the asset_ref.
	if id := signhash.ContentID(fetched); id != wantRef {
		t.Fatalf("fetched-bytes content id = %q, want %q", id, wantRef)
	}
}

// TestContentUploadIdempotent proves content-addressing makes re-uploading the
// same bytes return the same asset_ref (no duplicate, no new ref).
func TestContentUploadIdempotent(t *testing.T) {
	e := newEnv(t)
	asset := []byte("idempotent-by-content-address")

	first := e.uploadContent(t, asset)
	second := e.uploadContent(t, asset)

	if first.AssetRef != second.AssetRef {
		t.Fatalf("re-upload asset_ref = %q, want %q (content-addressed idempotent)", second.AssetRef, first.AssetRef)
	}
	if first.URL != second.URL {
		t.Fatalf("re-upload url = %q, want %q", second.URL, first.URL)
	}
}

// TestContentUploadEmptyBodyRejected proves a zero-length upload is a 400
// VALIDATION_FAILED — you cannot store empty content.
func TestContentUploadEmptyBodyRejected(t *testing.T) {
	e := newEnv(t)
	resp, raw := e.do(t, http.MethodPost, "/api/v1/content", []byte{}, nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("empty upload status = %d, want 400 (body %s)", resp.StatusCode, raw)
	}
	assertProblem(t, resp, raw, "VALIDATION_FAILED")
}

// uploadContent POSTs asset bytes and fails unless the status is 201, returning
// the decoded upload response.
func (e *testEnv) uploadContent(t *testing.T, asset []byte) contentUploadResponse {
	t.Helper()
	resp, raw := e.do(t, http.MethodPost, "/api/v1/content", asset, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("upload status = %d, want 201 (body %s)", resp.StatusCode, raw)
	}
	var out contentUploadResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode upload response: %v (body %s)", err, raw)
	}
	return out
}
