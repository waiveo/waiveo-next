package origin

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/maaxton/waiveo-next/internal/shared/apihttp"
	"github.com/maaxton/waiveo-next/internal/shared/signhash"
)

const testImagePath = "testdata/photon.png"

func loadTestImage(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(testImagePath)
	if err != nil {
		t.Fatalf("read fixture image %s: %v", testImagePath, err)
	}
	return b
}

// TestServeReturnsExactBytes asserts Store.Serve returns the exact bytes
// added for the right hash, and that the served bytes' own ContentID
// matches the hex key they were served under — the direct-fetch integrity
// chain a screen relies on (asset_ref == ContentID(served bytes)).
func TestServeReturnsExactBytes(t *testing.T) {
	img := loadTestImage(t)
	o := New()
	assetRef, err := o.Add(img)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	hexDigest := strings.TrimPrefix(assetRef, "sha256:")
	got := o.Serve(hexDigest)
	if got == nil {
		t.Fatalf("Serve(%q) = nil, want the added image bytes", hexDigest)
	}
	if string(got) != string(img) {
		t.Errorf("Serve(%q) returned different bytes than were added", hexDigest)
	}
	if signhash.ContentID(got) != assetRef {
		t.Errorf("ContentID(served bytes) = %q, want %q (asset_ref)", signhash.ContentID(got), assetRef)
	}
}

// TestServeUnknownHash404s asserts Serve returns nil for a hash that was
// never added.
func TestServeUnknownHash404s(t *testing.T) {
	o := New()
	if got := o.Serve("deadbeef"); got != nil {
		t.Errorf("Serve(unknown) = %v, want nil", got)
	}
}

// TestOpenPersistsContentAcrossRestart is the regression guard for the
// persistence asymmetry: the app store's scheduling rows that reference a
// content asset_ref persist to SQLite, so the content origin those refs resolve
// against MUST persist too — otherwise a routine feeder restart makes every
// resolved content url 404 (the bytes are gone) and spuriously rejects
// re-authoring a playlist for content already uploaded. A dir-backed Store
// (Open) write-throughs every Add to disk and reloads it at open, so an asset
// uploaded in one process lifetime is still served in the next.
func TestOpenPersistsContentAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	img := loadTestImage(t)

	// First feeder lifetime: open a dir-backed origin and upload the asset.
	first, err := Open(dir)
	if err != nil {
		t.Fatalf("Open(%q): %v", dir, err)
	}
	assetRef, err := first.Add(img)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	hexDigest := strings.TrimPrefix(assetRef, "sha256:")

	// Restart: a brand-new Store over the SAME dir — no shared in-memory map,
	// exactly as a fresh feeder process would reopen its persisted content.
	second, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen Open(%q): %v", dir, err)
	}
	if !second.Has(hexDigest) {
		t.Fatalf("Has(%q) = false after restart; the uploaded asset did not persist (a PATCH to a playlist referencing it would spuriously 422)", hexDigest)
	}
	got := second.Serve(hexDigest)
	if got == nil {
		t.Fatalf("Serve(%q) = nil after restart; a resolved content url would 404", hexDigest)
	}
	if string(got) != string(img) {
		t.Errorf("Serve(%q) returned different bytes than were uploaded before the restart", hexDigest)
	}
	// The reloaded bytes still hash to their own key — content-addressing
	// integrity (GET /content/<hex> returns bytes whose sha256 is <hex>) survives
	// the round-trip through disk.
	if signhash.ContentID(got) != assetRef {
		t.Errorf("ContentID(reloaded bytes) = %q, want %q (asset_ref)", signhash.ContentID(got), assetRef)
	}
}

// TestOpenSkipsCorruptOnDiskContent asserts Open refuses to load a file whose
// bytes no longer hash to its filename — a torn write or externally corrupted
// asset is dropped, never served under a hash it does not match, so the
// content-addressing integrity invariant holds even across a disk fault.
func TestOpenSkipsCorruptOnDiskContent(t *testing.T) {
	dir := t.TempDir()
	img := loadTestImage(t)
	assetRef := signhash.ContentID(img)
	hexDigest := strings.TrimPrefix(assetRef, "sha256:")

	// Plant a file named by the asset's hash but carrying different bytes.
	if err := os.WriteFile(filepath.Join(dir, hexDigest), []byte("not the image bytes"), 0o600); err != nil {
		t.Fatalf("plant corrupt file: %v", err)
	}
	o, err := Open(dir)
	if err != nil {
		t.Fatalf("Open(%q): %v", dir, err)
	}
	if o.Has(hexDigest) {
		t.Fatalf("Has(%q) = true; corrupt content was loaded and would be served under a mismatched hash", hexDigest)
	}
	if got := o.Serve(hexDigest); got != nil {
		t.Errorf("Serve(%q) = %q, want nil (corrupt content must not be served)", hexDigest, got)
	}
}

// TestNewIsInMemoryOnly pins that the dir-less constructor persists nothing, so
// the api-layer tests' default Store stays ephemeral and the (ref, error) Add
// signature never errors on the in-memory path.
func TestNewIsInMemoryOnly(t *testing.T) {
	o := New()
	ref, err := o.Add(loadTestImage(t))
	if err != nil {
		t.Fatalf("Add on in-memory store: %v", err)
	}
	if ref == "" {
		t.Fatal("Add returned an empty asset_ref")
	}
}

// TestHandlerServesOverHTTPS asserts the store's HTTP handler serves the
// exact image bytes at /content/<hex> for a known hash, and 404s an
// unknown one — exercised over an actual TLS listener, since screens
// fetch content directly over HTTPS, never through the relay.
func TestHandlerServesOverHTTPS(t *testing.T) {
	img := loadTestImage(t)
	o := New()
	assetRef, err := o.Add(img)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	hexDigest := strings.TrimPrefix(assetRef, "sha256:")

	srv := httptest.NewTLSServer(apihttp.WithTraceID(o.Handler()))
	defer srv.Close()
	client := srv.Client()

	resp, err := client.Get(srv.URL + "/content/" + hexDigest)
	if err != nil {
		t.Fatalf("GET /content/%s: %v", hexDigest, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /content/%s: status = %d, want 200", hexDigest, resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}
	if string(body) != string(img) {
		t.Error("response body did not match the added image bytes")
	}
	if got := resp.Header.Get(apihttp.TraceIDHeader); got == "" {
		t.Error("success response carries no Trace-Id header (API-060)")
	}

	resp2, err := client.Get(srv.URL + "/content/deadbeef")
	if err != nil {
		t.Fatalf("GET /content/deadbeef: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusNotFound {
		t.Errorf("GET /content/deadbeef: status = %d, want 404", resp2.StatusCode)
	}
	if ct := resp2.Header.Get("Content-Type"); ct != apihttp.ProblemContentType {
		t.Errorf("GET /content/deadbeef: Content-Type = %q, want %q", ct, apihttp.ProblemContentType)
	}
	var pb struct {
		Type     string `json:"type"`
		Status   int    `json:"status"`
		Code     string `json:"code"`
		TraceID  string `json:"trace_id"`
		Instance string `json:"instance"`
	}
	if err := json.NewDecoder(resp2.Body).Decode(&pb); err != nil {
		t.Fatalf("decode problem body: %v", err)
	}
	if pb.Code != "NOT_FOUND" {
		t.Errorf("code = %q, want %q", pb.Code, "NOT_FOUND")
	}
	if pb.Instance != "/content/deadbeef" {
		t.Errorf("instance = %q, want %q (API-015)", pb.Instance, "/content/deadbeef")
	}
	headerTraceID := resp2.Header.Get(apihttp.TraceIDHeader)
	if headerTraceID == "" {
		t.Fatal("error response carries no Trace-Id header (API-060)")
	}
	if pb.TraceID != headerTraceID {
		t.Errorf("body trace_id = %q, want it to equal the Trace-Id header %q (API-062)", pb.TraceID, headerTraceID)
	}
}
