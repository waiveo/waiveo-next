package api_test

import (
	"net/http"
	"testing"

	"github.com/maaxton/waiveo-next/internal/app/auth"
)

// TestReadOnlyAtTheNodeCannotCommandItsEntity pins the per-node write check on
// entity command dispatch, which was enforced and held by nothing: deleting
// `!view.canWrite(entity.ScopeNode)` left go test ./... green.
//
// The handler's own comment says why it is a write check: "Resolving it is a
// read; DISPATCHING to it changes the state of a physical device, so the command
// itself is authorized as a write at the entity's own scope node (SEC-005)."
//
// # Why the principal has two bindings
//
// This guard is the second layer of a two-layer control, and a test built for
// the first proves nothing about it. auth's middleware refuses a POST whose
// principal's EFFECTIVE role is below the method's floor, so a plain viewer
// never reaches this handler at all — a test using one passes with the guard
// deleted, which is how the automation-run version of this test was wrong before
// it was fixed.
//
// So: viewer at site A, operator at site B. The effective role clears the coarse
// gate, the entity at site A is readable, and only the per-node check can refuse
// the command at the node it is actually dispatched to.
func TestReadOnlyAtTheNodeCannotCommandItsEntity(t *testing.T) {
	e, tr := newScopedDevicePlaneEnv(t)
	mixed := e.principalWith(t, roleAt{tr.siteA, auth.RoleViewer}, roleAt{tr.siteB, auth.RoleOperator})

	// It really can SEE the entity: without this the refusal below could be
	// visibility rather than authority, and would prove nothing about canWrite.
	// The device plane publishes no single-entity GET, so this reads the list the
	// visible set actually filters.
	resp, raw := e.as(t, mixed, http.MethodGet, "/api/v1/entities", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("listing entities as the caller = %d, want 200 (body %s)", resp.StatusCode, raw)
	}
	visible := false
	for _, it := range decodePage(t, raw).Items {
		if id, _ := it["id"].(string); id == scopedEntityA {
			visible = true
		}
	}
	if !visible {
		t.Fatalf("the caller cannot SEE the entity it is about to be refused a command on — this test would then "+
			"be asserting visibility, not the write check it exists for (body %s)", raw)
	}

	body := mustJSON(t, map[string]any{"command": "launch"})
	resp, raw = e.as(t, mixed, http.MethodPost, "/api/v1/entities/"+scopedEntityA+"/commands", body, nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("a caller with only READ authority at the entity's node commanded it = %d, want 403 — a command "+
			"changes the state of a physical device and is a write at that node (SEC-005) (body %s)",
			resp.StatusCode, raw)
	}
	assertProblem(t, resp, raw, "FORBIDDEN")

	// The assertion that matters most: the refusal happened BEFORE anything
	// reached the relay. An authorization check performed after dispatch is a
	// check performed too late to matter.
	if calls := e.dispatcher.dispatched(); len(calls) != 0 {
		t.Fatalf("the refused command reached the relay anyway: %+v", calls)
	}
}

// TestWriteAuthorityAtTheNodeStillCommands is the control. Without it a guard
// that refused every caller satisfies the test above while making the device
// plane uncommandable.
func TestWriteAuthorityAtTheNodeStillCommands(t *testing.T) {
	e, tr := newScopedDevicePlaneEnv(t)
	operator := e.principalWith(t, roleAt{tr.siteA, auth.RoleOperator})

	body := mustJSON(t, map[string]any{"command": "launch"})
	resp, raw := e.as(t, operator, http.MethodPost, "/api/v1/entities/"+scopedEntityA+"/commands", body, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("a principal that DOES hold write authority at the entity's node was refused = %d, want 200 (body %s)",
			resp.StatusCode, raw)
	}
	if calls := e.dispatcher.dispatched(); len(calls) != 1 {
		t.Fatalf("an authorized command dispatched %d time(s), want 1", len(calls))
	}
}
