package api_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/maaxton/waiveo-next/internal/app/auth"
	"github.com/maaxton/waiveo-next/internal/app/store"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// deviceadoption_authz_test.go covers the two authorization guards on POST
// /devices/{id}/adopt, which had none. Both were deletable with the whole suite
// still green — the adoption tests all run as the fixture's unrestricted
// principal, so nothing exercised a scoped caller at all.
//
// Adoption is the operation that puts a physical device under this platform's
// control: it writes a durable row that compiles into the signed
// `device_inventory` the device's relay is sent, so an unauthorized adopt
// reshapes a site's desired state. This is also this repo's recorded defect
// shape — the mechanism right, the guard around it unproven — so each case
// asserts the durable half too, not just the status.
//
// # Why the principals are shaped the way they are
//
// Same reasoning as devicecommand_authz_test.go, and for the same reason it is
// not optional: auth's middleware already refuses a POST whose principal's
// EFFECTIVE role is below the method's floor, so a plain viewer never reaches
// this handler and a test using one would pass with the guard deleted. A
// caller that is a viewer at the device's node and an operator SOMEWHERE ELSE
// clears the coarse gate and can only be refused by the per-node check.

// mirrorScopedDevice gives a scoped-tree device the durable mirror row the
// adopt path reads, so a refusal in these cases is authorization and not the
// 503 an un-mirrored device draws. Without it every case here would pass on
// the wrong answer.
func mirrorScopedDevice(t *testing.T, e *devicePlaneEnv, deviceID, scopeNode, nativeID string) {
	t.Helper()
	if err := e.store.ReplaceDiscoveredDevices(context.Background(), "relay-"+deviceID, []store.DiscoveredDevice{{
		DeviceID: deviceID, RelayID: "relay-" + deviceID, ScopeNode: scopeNode,
		Driver: "roku-ecp", NativeID: nativeID, DeviceClass: "media-player",
		Name: "Scoped TV", Address: "192.168.50.31:8060",
		FirstSeen: 1000, LastSeen: 2000,
		Entities: []wire.CandidateEntity{{Key: "main", DeviceClass: "media-player"}},
	}}); err != nil {
		t.Fatalf("mirror the discovered device: %v", err)
	}
}

// adoptedRowCount reports how many durable adoption records exist — the thing
// an unauthorized adopt must not be able to create.
func adoptedRowCount(t *testing.T, e *devicePlaneEnv) int {
	t.Helper()
	rows, err := e.store.List(context.Background(), store.KindAdoptedDevice, store.ListFilter{})
	if err != nil {
		t.Fatalf("List adopted devices: %v", err)
	}
	return len(rows)
}

// TestReadOnlyAtTheNodeCannotAdoptItsDevice pins `!view.canWrite(...)` → 403.
// Deleting that block left `go test ./internal/app/api/... -count=1` reporting
// ok, which is what this case exists to stop.
func TestReadOnlyAtTheNodeCannotAdoptItsDevice(t *testing.T) {
	e, tr := newScopedDevicePlaneEnv(t)
	mirrorScopedDevice(t, e, scopedDeviceA, tr.screensA[0], "uuid:roku:ecp:SCOPEDA")
	mixed := e.principalWith(t, roleAt{tr.siteA, auth.RoleViewer}, roleAt{tr.siteB, auth.RoleOperator})

	// It really can SEE the device: otherwise the refusal below could be
	// visibility rather than authority, and the case would prove nothing about
	// canWrite. The device plane publishes no single-device GET, so this reads
	// the list the visible set actually filters.
	resp, raw := e.as(t, mixed, http.MethodGet, "/api/v1/devices", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("listing devices as the caller = %d, want 200 (body %s)", resp.StatusCode, raw)
	}
	visible := false
	for _, it := range decodePage(t, raw).Items {
		if id, _ := it["id"].(string); id == scopedDeviceA {
			visible = true
		}
	}
	if !visible {
		t.Fatalf("the caller cannot SEE the device it is about to be refused adoption of — this case would then be "+
			"asserting visibility, not the write check it exists for (body %s)", raw)
	}

	resp, raw = e.as(t, mixed, http.MethodPost, "/api/v1/devices/"+scopedDeviceA+"/adopt", nil, nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("a caller with only READ authority at the device's node adopted it = %d, want 403 — adoption writes "+
			"a durable row and changes what the relay is told to do (SEC-005) (body %s)", resp.StatusCode, raw)
	}
	assertProblem(t, resp, raw, "FORBIDDEN")

	if n := adoptedRowCount(t, e); n != 0 {
		t.Fatalf("the refused adopt wrote %d durable adoption row(s), want 0 — a check that runs after the write is "+
			"a check that runs too late", n)
	}
}

// TestOutOfScopeCannotAdoptADeviceItCannotSee pins the other guard,
// `!view.canRead(...)` → 404. The status matters as much as the refusal: a 403
// here would confirm to a caller with no authority over Site A that a device
// with this id exists there, which is what answering identically to an unknown
// id prevents (scopeview.go).
func TestOutOfScopeCannotAdoptADeviceItCannotSee(t *testing.T) {
	e, tr := newScopedDevicePlaneEnv(t)
	mirrorScopedDevice(t, e, scopedDeviceA, tr.screensA[0], "uuid:roku:ecp:SCOPEDA")
	elsewhere := e.principalWith(t, roleAt{tr.siteB, auth.RoleOperator})

	resp, raw := e.as(t, elsewhere, http.MethodPost, "/api/v1/devices/"+scopedDeviceA+"/adopt", nil, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("adopting a device outside the caller's visible set = %d, want 404 (body %s)", resp.StatusCode, raw)
	}
	assertProblem(t, resp, raw, "NOT_FOUND")

	// And the answer is indistinguishable from an id that names nothing at all.
	unknown, unknownRaw := e.as(t, elsewhere, http.MethodPost, "/api/v1/devices/"+devMissing+"/adopt", nil, nil)
	if unknown.StatusCode != resp.StatusCode {
		t.Fatalf("an out-of-scope device answers %d and an unknown id answers %d; a caller can tell them apart, which "+
			"is the existence disclosure the 404 exists to prevent (%s)", resp.StatusCode, unknown.StatusCode, unknownRaw)
	}

	if n := adoptedRowCount(t, e); n != 0 {
		t.Fatalf("the refused adopt wrote %d durable adoption row(s), want 0", n)
	}
}

// TestWriteAuthorityAtTheNodeAdopts is the control. Without it, a guard that
// refused every caller would satisfy both cases above while making adoption
// impossible for anyone.
func TestWriteAuthorityAtTheNodeAdopts(t *testing.T) {
	e, tr := newScopedDevicePlaneEnv(t)
	mirrorScopedDevice(t, e, scopedDeviceA, tr.screensA[0], "uuid:roku:ecp:SCOPEDA")
	operator := e.principalWith(t, roleAt{tr.siteA, auth.RoleOperator})

	resp, raw := e.as(t, operator, http.MethodPost, "/api/v1/devices/"+scopedDeviceA+"/adopt", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("a principal that DOES hold write authority at the device's node was refused = %d, want 200 (body %s)",
			resp.StatusCode, raw)
	}
	if n := adoptedRowCount(t, e); n != 1 {
		t.Fatalf("adopted rows = %d after an authorized adopt, want 1", n)
	}
}
