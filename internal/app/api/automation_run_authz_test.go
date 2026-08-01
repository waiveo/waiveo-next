package api_test

import (
	"net/http"
	"testing"

	"github.com/maaxton/waiveo-next/internal/app/auth"
)

// TestViewerCannotRunAnAutomationItCanRead pins the authorization guard on the
// automation run route, which was enforced and held by nothing: deleting
// `!view.canWrite(node)` from runAutomation left `go test ./...` green.
//
// The route's own comment states why the check is a WRITE check:
//
//	Reading it is where a run BEGINS, not where it ends: a run dispatches the
//	automation's actions, so it is a write at the automation's own scope node
//	and is authorized as one (SEC-005).
//
// So the gap is a privilege escalation with real-world reach — on this platform
// an automation's actions command physical devices — and it is invisible to the
// guard immediately above it. That one refuses a node the caller cannot READ,
// with 404, and it IS covered (TestBulkEnableTargetsOnlyVisibleAutomations
// drives it). Passing the read check does not imply the write check: they are
// distinct authorities, which is the entire reason the pair exists.
//
// The guard is defence in depth rather than the only control, and the test has
// to be built for that or it proves nothing: auth/middleware.go refuses a POST
// whose principal's effective role is below the method's floor, so a plain
// viewer never reaches this handler. What ONLY this guard catches is a principal
// holding write authority at SOME node and merely read authority HERE.
//
// The refusal is 403 rather than 404 deliberately — the caller has just been
// shown it may read this row, so naming the refusal discloses nothing but its
// own authority.
func TestViewerCannotRunAnAutomationItCanRead(t *testing.T) {
	e := newEnv(t)
	tr := seedScopedTree(t, e)
	// Viewer at site A, OPERATOR at site B. The role mix is the whole point: the
	// auth middleware's coarse gate gives a POST to any principal whose EFFECTIVE
	// role is high enough anywhere, so this caller sails past it — and only the
	// handler's per-node check can then refuse the write at the node it is
	// actually writing at. A plain viewer would be refused upstream and prove
	// nothing about this guard; measured, not assumed.
	viewer := e.principalWith(t, roleAt{tr.siteA, auth.RoleViewer}, roleAt{tr.siteB, auth.RoleOperator})

	// Seeded inside the viewer's own subtree, so the run below fails on the
	// VERB rather than on visibility — a 404 here would prove nothing about
	// canWrite, and is what the existing coverage already asserts.
	id := decodeID(t, e.createOK(t, "/api/v1/automations",
		edgeAutomationBody("", tr.screensA[0], map[string]string{"env": "prod"})))

	// It really is readable by this principal: without this the refusal below
	// could be visibility rather than authority, and the test would pass for
	// the wrong reason.
	if resp, raw := e.as(t, viewer, http.MethodGet, "/api/v1/automations/"+id, nil, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("the viewer cannot READ the automation (%d) — this test would then be asserting visibility, "+
			"not the write check it exists for (body %s)", resp.StatusCode, raw)
	}

	resp, raw := e.as(t, viewer, http.MethodPost, "/api/v1/automations/"+id+"/run", []byte(`{}`), nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("a viewer ran an automation it may only read = %d, want 403 — a run dispatches the automation's "+
			"actions, so it is a write at that node (SEC-005) and commands real devices (body %s)",
			resp.StatusCode, raw)
	}
	assertProblem(t, resp, raw, "FORBIDDEN")
}

// TestOperatorCanRunAnAutomationItCanWrite is the control, and it is what stops
// the test above from passing against a route that refused every caller.
func TestOperatorCanRunAnAutomationItCanWrite(t *testing.T) {
	e := newEnv(t)
	tr := seedScopedTree(t, e)
	operator := e.principalWith(t, roleAt{tr.siteA, auth.RoleOperator})

	id := decodeID(t, e.createOK(t, "/api/v1/automations",
		edgeAutomationBody("", tr.screensA[0], map[string]string{"env": "prod"})))

	resp, raw := e.as(t, operator, http.MethodPost, "/api/v1/automations/"+id+"/run", []byte(`{}`), nil)
	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusNotFound {
		t.Fatalf("a principal that DOES hold write authority was refused the run = %d — the guard refuses "+
			"everyone, which satisfies the viewer test while breaking the route (body %s)", resp.StatusCode, raw)
	}
}
