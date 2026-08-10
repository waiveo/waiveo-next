package api_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
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

// TestContentUploadHonorsIdempotencyKey pins API-050/052/053 on the upload,
// which is a mutating POST like every other one on this surface.
//
// Content-addressing gives the replay half for free and the CONFLICT half not at
// all: the second case below reuses one key with different bytes, which without
// the guard is answered 201 with a second, unrelated asset_ref — the client's
// retry silently uploaded something else and was handed a ref for it.
func TestContentUploadHonorsIdempotencyKey(t *testing.T) {
	e := newEnv(t)
	key := map[string]string{"Idempotency-Key": "content-upload-retry-1"}

	resp, first := e.do(t, http.MethodPost, "/api/v1/content", []byte("keyed asset bytes"), key)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("first keyed upload = %d, want 201 (body %s)", resp.StatusCode, first)
	}

	// Same key, same bytes: the retained response verbatim (API-052).
	resp, replay := e.do(t, http.MethodPost, "/api/v1/content", []byte("keyed asset bytes"), key)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("keyed replay = %d, want 201 (body %s)", resp.StatusCode, replay)
	}
	if string(replay) != string(first) {
		t.Fatalf("keyed replay body differs:\n%s\n%s", first, replay)
	}

	// Same key, DIFFERENT bytes: 409 IDEMPOTENCY_KEY_REUSED, and nothing stored
	// for those bytes (API-053).
	resp, raw := e.do(t, http.MethodPost, "/api/v1/content", []byte("entirely different bytes"), key)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("keyed reuse with a different body = %d, want 409 (body %s)", resp.StatusCode, raw)
	}
	assertProblem(t, resp, raw, "IDEMPOTENCY_KEY_REUSED")
	sum := sha256.Sum256([]byte("entirely different bytes"))
	if e.content.Has(hex.EncodeToString(sum[:])) {
		t.Fatal("the refused upload still wrote its bytes to the content origin")
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

// contentListResponse is the GET /api/v1/content body: every asset the origin
// currently serves, each carrying the same asset_ref/url an upload returns plus
// the size and store time a media browser sorts on.
type contentListResponse struct {
	Content []struct {
		AssetRef  string `json:"asset_ref"`
		URL       string `json:"url"`
		SizeBytes int64  `json:"size_bytes"`
		StoredAt  int64  `json:"stored_at"`
	} `json:"content"`
}

// TestContentListReturnsUploadedAssets proves the read half of the content
// pipeline: GET /api/v1/content is empty on a fresh box, and after uploads it
// returns each asset with the SAME asset_ref/url the upload minted (so an
// authoring surface can rediscover bytes it did not keep the ref for) plus a
// correct size. Without this, upload was write-only and the media library was
// only ever the current session's own uploads.
func TestContentListReturnsUploadedAssets(t *testing.T) {
	e := newEnv(t)

	// Empty on a fresh box.
	resp, raw := e.do(t, http.MethodGet, "/api/v1/content", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list status = %d, want 200 (body %s)", resp.StatusCode, raw)
	}
	var empty contentListResponse
	if err := json.Unmarshal(raw, &empty); err != nil {
		t.Fatalf("decode empty list: %v (body %s)", err, raw)
	}
	if len(empty.Content) != 0 {
		t.Fatalf("fresh box listed %d asset(s), want 0", len(empty.Content))
	}

	assets := [][]byte{
		[]byte("first asset — a slide background"),
		[]byte("second asset \x00\x01 the quick brown fox jumps"),
	}
	want := map[string]int64{} // asset_ref -> size
	for _, a := range assets {
		up, upRaw := e.do(t, http.MethodPost, "/api/v1/content", a, nil)
		if up.StatusCode != http.StatusCreated {
			t.Fatalf("upload status = %d, want 201 (body %s)", up.StatusCode, upRaw)
		}
		want[signhash.ContentID(a)] = int64(len(a))
	}

	resp, raw = e.do(t, http.MethodGet, "/api/v1/content", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list status = %d, want 200 (body %s)", resp.StatusCode, raw)
	}
	var got contentListResponse
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode list: %v (body %s)", err, raw)
	}
	if len(got.Content) != len(assets) {
		t.Fatalf("listed %d asset(s), want %d", len(got.Content), len(assets))
	}
	for _, row := range got.Content {
		wantSize, ok := want[row.AssetRef]
		if !ok {
			t.Fatalf("listed an unexpected asset_ref %q", row.AssetRef)
		}
		if row.SizeBytes != wantSize {
			t.Fatalf("asset %q size = %d, want %d", row.AssetRef, row.SizeBytes, wantSize)
		}
		wantHex := strings.TrimPrefix(row.AssetRef, "sha256:")
		if row.URL != e.contentBase+"/content/"+wantHex {
			t.Fatalf("asset %q url = %q, want %q", row.AssetRef, row.URL, e.contentBase+"/content/"+wantHex)
		}
		if row.StoredAt == 0 {
			t.Fatalf("asset %q stored_at is zero", row.AssetRef)
		}
		delete(want, row.AssetRef)
	}
	if len(want) != 0 {
		t.Fatalf("%d uploaded asset(s) missing from the listing", len(want))
	}
}
