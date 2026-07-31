package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

// healthz_test.go pins that /healthz reports something that can actually differ
// between a healthy box and a sick one.
//
// It used to return a literal `{"component":"waiveo-feeder","status":"ok"}` —
// the same bytes whether or not the box had run out of room to write. This
// deployment has already lost a box to a full disk, and a probe that answers
// "ok" through that tells an operator the one thing that is not true.

func callHealthz(t *testing.T, storePath, contentPath string) map[string]any {
	t.Helper()
	rec := httptest.NewRecorder()
	healthzFor(storePath, contentPath)(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body.String())
	}
	return body
}

func TestHealthzReportsRealDiskHeadroom(t *testing.T) {
	dir := t.TempDir()
	body := callHealthz(t, filepath.Join(dir, "app.db"), dir)

	if body["component"] != "waiveo-feeder" {
		t.Errorf("component = %v", body["component"])
	}
	disk, ok := body["disk_headroom_bytes"].(map[string]any)
	if !ok {
		t.Fatalf("no disk_headroom_bytes in %v", body)
	}
	for _, name := range []string{"store", "content"} {
		v, present := disk[name]
		if !present {
			t.Errorf("no headroom reported for %s", name)
			continue
		}
		// A real reading, not a placeholder: a temp dir on a working machine has
		// room, and zero here would mean the value is not being measured.
		if n, _ := v.(float64); n <= 0 {
			t.Errorf("%s headroom = %v, want a positive measurement", name, v)
		}
	}
	if _, present := body["disk_headroom_unavailable"]; present {
		t.Errorf("both paths exist but something was reported unavailable: %v", body["disk_headroom_unavailable"])
	}
}

// TestHealthzNamesAPathItCouldNotRead: an unreadable path must be named rather
// than silently absent, or a consumer cannot tell "could not stat this" from
// "the field was dropped" — the same distinction the relay's vitals draws.
func TestHealthzNamesAPathItCouldNotRead(t *testing.T) {
	dir := t.TempDir()
	body := callHealthz(t, filepath.Join(dir, "app.db"), filepath.Join(dir, "no-such-content-dir"))

	missing, ok := body["disk_headroom_unavailable"].([]any)
	if !ok || len(missing) != 1 || missing[0] != "content" {
		t.Fatalf("disk_headroom_unavailable = %v, want [content]", body["disk_headroom_unavailable"])
	}
	// The readable one is still reported: one bad path must not suppress the other.
	disk, _ := body["disk_headroom_bytes"].(map[string]any)
	if _, present := disk["store"]; !present {
		t.Errorf("an unreadable content path suppressed the store reading: %v", body)
	}
}

// TestHealthzDoesNotTouchTheStore is the property that keeps this probe useful
// when the database is the problem. A liveness check that queries the store can
// hang exactly then, turning "degraded" into "no answer", which the deploy
// tooling reads as a dead process. Asserted by pointing the store path at a
// file that does not exist: a probe that opened it would fail or block, and this
// one must answer anyway.
func TestHealthzDoesNotTouchTheStore(t *testing.T) {
	dir := t.TempDir()
	body := callHealthz(t, filepath.Join(dir, "definitely-not-a-database.db"), dir)
	if body["status"] != "ok" {
		t.Errorf("status = %v, want ok: the probe must answer without opening the store", body["status"])
	}
}
