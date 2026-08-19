package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/maaxton/waiveo-next/internal/app/auth"
	"github.com/maaxton/waiveo-next/internal/app/devices"
	"github.com/maaxton/waiveo-next/internal/app/store"
)

// devicefirstseen_test.go drives the correction path for a device's stored age.
//
// `first_seen` is planted once and never moves, which is what stops a relay
// restart re-dating every device on the network as new (#196). The cost of that
// rule is that a WRONG value is permanent too — and one population of wrong
// values exists by construction, adopted at the upgrade from the older column
// where they had been written off the reporting relay's unattested clock. This
// operation is the one sanctioned way such a value ever leaves, on the same
// reasoning that gave the SEC-066 clock floor its `clock_floor.reset`.
//
// Everything here goes through the real HTTP surface against the real store,
// because the whole difficulty is that the value lives in THREE places — the
// ledger, its mirror projection, and the running read model — and a test that
// stubbed any of them would pass while the retire was undone by the next reboot.

// storedFirstSeen reads what the durable ledger holds, which is the only reading
// that can tell a real retire from a cleared in-memory flag.
func storedFirstSeen(t *testing.T, e *devicePlaneEnv) (rows, answered int) {
	t.Helper()
	ledger, err := e.store.InspectDeviceFirstSeen(context.Background())
	if err != nil {
		t.Fatalf("InspectDeviceFirstSeen: %v", err)
	}
	return len(ledger.Rows), ledger.Answered
}

// projectStoredAge teaches the read model what the store committed, exactly as
// the live report path and the boot restore both do (candidatemirror.go). Without
// it the fixture's device has no age in the read model and the test would prove
// nothing about clearing one.
func projectStoredAge(t *testing.T, e *devicePlaneEnv, deviceID string) int64 {
	t.Helper()
	mirrored, err := e.store.DiscoveredDevices(context.Background())
	if err != nil {
		t.Fatalf("DiscoveredDevices: %v", err)
	}
	for _, d := range mirrored {
		if d.DeviceID != deviceID {
			continue
		}
		if d.FirstSeen <= 0 {
			t.Fatalf("fixture: the store planted no first_seen for %s, so there is nothing to retire", deviceID)
		}
		e.registry.MarkSeen(map[string]devices.Seen{deviceID: {
			FirstMs: d.FirstSeen, LastMs: d.LastSeen, FirstOrigin: d.FirstSeenOrigin,
		}})
		return d.FirstSeen
	}
	t.Fatalf("fixture: %s is not in the mirror", deviceID)
	return 0
}

// TestRetireDeviceFirstSeenClearsEveryCopy is the operation end to end, and it
// checks all three copies because clearing fewer than three is the failure mode.
//
// The response body must have dropped the member (an absent `first_seen` is the
// honest answer — the console renders an em dash); the durable ledger row must be
// gone; and the list an operator reads next must not still be serving the retired
// instant out of the running process's memory.
func TestRetireDeviceFirstSeenClearsEveryCopy(t *testing.T) {
	e := newDevicePlaneEnv(t)
	seedAdoptable(t, e, adoptDeviceID, "uuid:roku:ecp:X1")
	planted := projectStoredAge(t, e, adoptDeviceID)

	if rows, answered := storedFirstSeen(t, e); rows != 1 || answered != 1 {
		t.Fatalf("fixture: ledger holds %d row(s) answering %d mirrored device(s), want 1 and 1", rows, answered)
	}
	// The device reads with an age before the retire, or the test proves nothing.
	if _, listRaw := e.do(t, http.MethodGet, "/api/v1/devices", nil, nil); !listedFirstSeen(t, listRaw, adoptDeviceID, planted) {
		t.Fatalf("fixture: the list does not report the planted first_seen %d", planted)
	}
	// And it reports where that age CAME FROM. Without this member every age on
	// the page reads as an instant this deployment observed, which on an upgraded
	// box is false for every row — the whole of #197 (see the api/1 Device schema
	// and web/src/routes/devices).
	if _, listRaw := e.do(t, http.MethodGet, "/api/v1/devices", nil, nil); listedOrigin(t, listRaw, adoptDeviceID) != store.FirstSeenPlanted {
		t.Fatalf("fixture: the list reports first_seen_origin %q for a value the store planted itself, want %q",
			listedOrigin(t, listRaw, adoptDeviceID), store.FirstSeenPlanted)
	}

	resp, raw := e.do(t, http.MethodDelete, "/api/v1/devices/"+adoptDeviceID+"/first-seen", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("DELETE first-seen = %d, want 200 (%s)", resp.StatusCode, raw)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode retire response: %v", err)
	}
	if body["id"] != adoptDeviceID {
		t.Errorf("response id = %v, want %s", body["id"], adoptDeviceID)
	}
	if _, present := body["first_seen"]; present {
		t.Errorf("response still carries first_seen = %v; an absent member is the honest answer, and a zero would "+
			"claim the device was first seen at the epoch", body["first_seen"])
	}
	// last_seen is a different fact answered by a different rule. A retire that
	// took it too would report a device that reported a minute ago as never heard
	// from.
	if _, present := body["last_seen"]; !present {
		t.Errorf("retiring first_seen also cleared last_seen from the response body")
	}
	// The provenance describes the value, so it goes with it. Left behind, an
	// origin on a device that has no age tells the console to caveat a number
	// that is not there.
	if _, present := body["first_seen_origin"]; present {
		t.Errorf("response still carries first_seen_origin = %v after the value it describes was retired",
			body["first_seen_origin"])
	}

	if rows, _ := storedFirstSeen(t, e); rows != 0 {
		t.Errorf("the durable ledger still holds %d row(s) after a retire", rows)
	}
	if _, listRaw := e.do(t, http.MethodGet, "/api/v1/devices", nil, nil); listedFirstSeen(t, listRaw, adoptDeviceID, planted) {
		t.Errorf("the list still serves the retired instant %d out of the running process's memory; the durable "+
			"clear is only two of the three copies", planted)
	}

	// Naturally idempotent: retiring a device with no stored answer is a 200 that
	// changes nothing, the contract the unignore DELETE beside it keeps.
	if resp, raw := e.do(t, http.MethodDelete, "/api/v1/devices/"+adoptDeviceID+"/first-seen", nil, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("second DELETE first-seen = %d, want 200 (%s)", resp.StatusCode, raw)
	}
}

// listedFirstSeen reports whether the device list carries want as this device's
// first_seen — the reading an operator actually gets, taken through the API
// rather than off the registry.
func listedFirstSeen(t *testing.T, listRaw []byte, deviceID string, want int64) bool {
	t.Helper()
	for _, item := range decodePage(t, listRaw).Items {
		if item["id"] != deviceID {
			continue
		}
		got, ok := item["first_seen"].(float64)
		return ok && int64(got) == want
	}
	return false
}

// listedOrigin reads the served `first_seen_origin` for one device, or "" when
// the member is absent — the difference an operator's renderer turns on.
func listedOrigin(t *testing.T, listRaw []byte, deviceID string) string {
	t.Helper()
	for _, item := range decodePage(t, listRaw).Items {
		if item["id"] != deviceID {
			continue
		}
		got, _ := item["first_seen_origin"].(string)
		return got
	}
	return ""
}

// TestRetireFirstSeenOfAnUnknownDeviceIs404: an id no relay has reported names
// nothing, and the retire answers with the same 404 any unresolvable identifier
// draws — and writes nothing.
func TestRetireFirstSeenOfAnUnknownDeviceIs404(t *testing.T) {
	e := newDevicePlaneEnv(t)
	seedAdoptable(t, e, adoptDeviceID, "uuid:roku:ecp:X1")

	resp, raw := e.do(t, http.MethodDelete, "/api/v1/devices/"+devMissing+"/first-seen", nil, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("retire on an unknown device = %d, want 404 (%s)", resp.StatusCode, raw)
	}
	if rows, _ := storedFirstSeen(t, e); rows != 1 {
		t.Errorf("a refused retire changed the ledger (rows=%d, want 1)", rows)
	}
}

// TestReadOnlyAtTheNodeCannotRetireAFirstSeen pins the write gate. Retiring is a
// durable, destructive operator decision at the device's placement (SEC-005), so
// READ authority is not enough — and the check has to run BEFORE the write, since
// there is nothing to undo afterwards.
func TestReadOnlyAtTheNodeCannotRetireAFirstSeen(t *testing.T) {
	e, tr := newScopedDevicePlaneEnv(t)
	mirrorScopedDevice(t, e, scopedDeviceA, tr.screensA[0], "uuid:roku:ecp:SCOPEDA")
	viewer := e.principalWith(t, roleAt{tr.siteA, auth.RoleViewer})

	resp, raw := e.as(t, viewer, http.MethodDelete, "/api/v1/devices/"+scopedDeviceA+"/first-seen", nil, nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("retire with only READ authority = %d, want 403 (%s)", resp.StatusCode, raw)
	}
	assertProblem(t, resp, raw, "FORBIDDEN")
	if rows, _ := storedFirstSeen(t, e); rows == 0 {
		t.Errorf("a refused retire cleared the ledger anyway — the authorization check must run before the write")
	}
}

// TestOutOfScopeCannotRetireAFirstSeenItCannotSee pins the other guard: a device
// the caller cannot see answers 404, indistinguishable from an unknown id, so the
// refusal cannot confirm the device exists elsewhere.
func TestOutOfScopeCannotRetireAFirstSeenItCannotSee(t *testing.T) {
	e, tr := newScopedDevicePlaneEnv(t)
	mirrorScopedDevice(t, e, scopedDeviceA, tr.screensA[0], "uuid:roku:ecp:SCOPEDA")
	elsewhere := e.principalWith(t, roleAt{tr.siteB, auth.RoleOperator})

	resp, raw := e.as(t, elsewhere, http.MethodDelete, "/api/v1/devices/"+scopedDeviceA+"/first-seen", nil, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("retiring an out-of-scope device's first_seen = %d, want 404 (%s)", resp.StatusCode, raw)
	}
	assertProblem(t, resp, raw, "NOT_FOUND")
}
