package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/maaxton/waiveo-next/internal/app/auth"
)

// deviceignore_test.go drives the third decision a discovered device carries
// (spec §7): IGNORE. Everything here goes through the real HTTP surface against
// the real store, because the point of ignore is a durable, reversible decision
// that outlives a re-sighting — a test that stubbed the store would prove only
// that a handler ran.

func ignoredIDs(t *testing.T, e *devicePlaneEnv) []string {
	t.Helper()
	ids, err := e.store.ListIgnoredDeviceIDs(context.Background())
	if err != nil {
		t.Fatalf("ListIgnoredDeviceIDs: %v", err)
	}
	return ids
}

// TestIgnoreDeviceMarksItIgnored: the operation's real subject is the durable
// decision and the flag the console reads, not merely the 200.
func TestIgnoreDeviceMarksItIgnored(t *testing.T) {
	e := newDevicePlaneEnv(t)
	seedAdoptable(t, e, adoptDeviceID, "uuid:roku:ecp:X1")

	resp, raw := e.do(t, http.MethodPost, "/api/v1/devices/"+adoptDeviceID+"/ignore", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST ignore = %d, want 200 (%s)", resp.StatusCode, raw)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode ignore response: %v", err)
	}
	if body["id"] != adoptDeviceID {
		t.Errorf("response id = %v, want %s", body["id"], adoptDeviceID)
	}
	if body["ignored"] != true {
		t.Errorf("response ignored = %v, want true", body["ignored"])
	}

	// The durable half. An ignore that only flipped an in-memory flag would be
	// gone on the next restart AND the next relay report.
	if ids := ignoredIDs(t, e); len(ids) != 1 || ids[0] != adoptDeviceID {
		t.Errorf("ignored rows = %v, want [%s]", ids, adoptDeviceID)
	}

	// And the list the console reads now reports it ignored.
	_, listRaw := e.do(t, http.MethodGet, "/api/v1/devices", nil, nil)
	seen := false
	for _, item := range decodePage(t, listRaw).Items {
		if item["id"] == adoptDeviceID {
			seen = true
			if item["ignored"] != true {
				t.Errorf("listed ignored = %v after ignoring, want true", item["ignored"])
			}
		}
	}
	if !seen {
		t.Errorf("an ignored device vanished from the list — ignored must be VISIBLE, not hidden (spec §7)")
	}
}

// TestIgnoreDeviceIsIdempotent: a double-click or a retried request is a 200 that
// changes nothing, and never a second row.
func TestIgnoreDeviceIsIdempotent(t *testing.T) {
	e := newDevicePlaneEnv(t)
	seedAdoptable(t, e, adoptDeviceID, "uuid:roku:ecp:X1")

	for i := range 2 {
		resp, raw := e.do(t, http.MethodPost, "/api/v1/devices/"+adoptDeviceID+"/ignore", nil, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("ignore #%d = %d, want 200 (%s)", i+1, resp.StatusCode, raw)
		}
	}
	if ids := ignoredIDs(t, e); len(ids) != 1 {
		t.Errorf("ignored rows = %d after two ignores, want 1", len(ids))
	}
}

// TestUnignoreDeviceReverses pins spec §7's reversibility end to end: the DELETE
// clears the durable decision and the list stops reporting it ignored.
func TestUnignoreDeviceReverses(t *testing.T) {
	e := newDevicePlaneEnv(t)
	seedAdoptable(t, e, adoptDeviceID, "uuid:roku:ecp:X1")

	if resp, raw := e.do(t, http.MethodPost, "/api/v1/devices/"+adoptDeviceID+"/ignore", nil, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("ignore = %d, want 200 (%s)", resp.StatusCode, raw)
	}
	resp, raw := e.do(t, http.MethodDelete, "/api/v1/devices/"+adoptDeviceID+"/ignore", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("DELETE ignore = %d, want 200 (%s)", resp.StatusCode, raw)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode unignore response: %v", err)
	}
	if body["ignored"] != false {
		t.Errorf("response ignored = %v after unignore, want false", body["ignored"])
	}
	if ids := ignoredIDs(t, e); len(ids) != 0 {
		t.Errorf("ignored rows = %v after unignore, want []", ids)
	}
	// Un-ignoring a device that is not ignored is a no-op 200, not an error.
	if resp, raw := e.do(t, http.MethodDelete, "/api/v1/devices/"+adoptDeviceID+"/ignore", nil, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("second DELETE ignore = %d, want 200 (%s)", resp.StatusCode, raw)
	}
}

// TestIgnoreUnknownDeviceIs404: an id no relay has reported names nothing, and
// ignore answers with the same 404 any unresolvable identifier draws — and
// writes no row.
func TestIgnoreUnknownDeviceIs404(t *testing.T) {
	e := newDevicePlaneEnv(t)

	resp, raw := e.do(t, http.MethodPost, "/api/v1/devices/"+devMissing+"/ignore", nil, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("ignore unknown = %d, want 404 (%s)", resp.StatusCode, raw)
	}
	if ids := ignoredIDs(t, e); len(ids) != 0 {
		t.Errorf("a refused ignore wrote %v, want no rows", ids)
	}
}

// TestReadOnlyAtTheNodeCannotIgnoreItsDevice pins the write gate on the shared
// resolve-and-authorize preamble: ignoring is a durable operator decision at the
// device's placement (SEC-005), so READ authority is not enough — for the ignore
// POST and the unignore DELETE alike.
func TestReadOnlyAtTheNodeCannotIgnoreItsDevice(t *testing.T) {
	e, tr := newScopedDevicePlaneEnv(t)
	mirrorScopedDevice(t, e, scopedDeviceA, tr.screensA[0], "uuid:roku:ecp:SCOPEDA")
	viewer := e.principalWith(t, roleAt{tr.siteA, auth.RoleViewer})

	for _, method := range []string{http.MethodPost, http.MethodDelete} {
		resp, raw := e.as(t, viewer, method, "/api/v1/devices/"+scopedDeviceA+"/ignore", nil, nil)
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("%s ignore with only READ authority = %d, want 403 (%s)", method, resp.StatusCode, raw)
		}
		assertProblem(t, resp, raw, "FORBIDDEN")
	}
	if ids := ignoredIDs(t, e); len(ids) != 0 {
		t.Errorf("a refused ignore wrote %v, want no rows — the check must run before the write", ids)
	}
}

// TestOutOfScopeCannotIgnoreADeviceItCannotSee pins the other guard: a device the
// caller cannot see answers 404, indistinguishable from an unknown id, so the
// refusal cannot confirm the device exists elsewhere.
func TestOutOfScopeCannotIgnoreADeviceItCannotSee(t *testing.T) {
	e, tr := newScopedDevicePlaneEnv(t)
	mirrorScopedDevice(t, e, scopedDeviceA, tr.screensA[0], "uuid:roku:ecp:SCOPEDA")
	elsewhere := e.principalWith(t, roleAt{tr.siteB, auth.RoleOperator})

	resp, raw := e.as(t, elsewhere, http.MethodPost, "/api/v1/devices/"+scopedDeviceA+"/ignore", nil, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("ignoring an out-of-scope device = %d, want 404 (%s)", resp.StatusCode, raw)
	}
	assertProblem(t, resp, raw, "NOT_FOUND")
}
