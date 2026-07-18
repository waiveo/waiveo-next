package origin

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
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
	assetRef := o.Add(img)

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

// TestHandlerServesOverHTTPS asserts the store's HTTP handler serves the
// exact image bytes at /content/<hex> for a known hash, and 404s an
// unknown one — exercised over an actual TLS listener, since screens
// fetch content directly over HTTPS, never through the relay.
func TestHandlerServesOverHTTPS(t *testing.T) {
	img := loadTestImage(t)
	o := New()
	assetRef := o.Add(img)
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
