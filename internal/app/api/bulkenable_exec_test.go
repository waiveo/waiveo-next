package api_test

// bulkenable_exec_test.go drives the EXECUTION of POST
// /api/v1/automations/bulk-enable through the real authenticated mux: not that
// a Job was accepted (jobs_test.go covers that), but that the work the 202
// promised actually happens — every matched automation's `enabled` flips, the
// store generation advances as it does for any other authored change, and the
// Job the client polls walks API-113's progression to a terminal value that
// reports what really happened, `partial` included.
//
// Completion is observed by asking the runner (testEnv.runJobs), never by
// sleeping: the runner is created stopped, so each case arranges the world the
// job will run against and then releases it.

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"sort"
	"testing"

	"github.com/maaxton/waiveo-next/internal/app/auth/authtest"
)

// automationEnabled reads one automation over the mux and returns its
// resource-envelope `enabled` flag. It reads the SHIPPED representation rather
// than the store row, so what is asserted is what a client would see.
func (e *testEnv) automationEnabled(t *testing.T, who authtest.Credential, id string) bool {
	t.Helper()
	resp, raw := e.as(t, who, http.MethodGet, "/api/v1/automations/"+id, nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET automation %s = %d, want 200 (body %s)", id, resp.StatusCode, raw)
	}
	var body struct {
		Enabled *bool `json:"enabled"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode automation %s: %v (body %s)", id, err, raw)
	}
	if body.Enabled == nil {
		t.Fatalf("automation %s carries no `enabled` member (body %s)", id, raw)
	}
	return *body.Enabled
}

// targetStates reads the Job's per-target states as a map, for order-independent
// comparison.
func targetStates(j jobBody) map[string]string {
	out := make(map[string]string, len(j.Targets))
	for _, tg := range j.Targets {
		out[tg.TargetID] = tg.State
	}
	return out
}

func (e *testEnv) generation(t *testing.T) int64 {
	t.Helper()
	g, err := e.store.Generation(context.Background())
	if err != nil {
		t.Fatalf("store.Generation: %v", err)
	}
	return g
}

// TestBulkEnableFlipsMatchedAutomations is the central claim this endpoint owes:
// the 202 is a promise that the matched automations WILL be enabled/disabled,
// and here they are — flipped on every matched row, untouched on every
// non-matched one.
//
// The non-matched control is what makes the assertion mean anything: a handler
// that wrote `enabled` over the whole table would satisfy the "matched rows
// flipped" half perfectly.
//
// The store generation is asserted to ADVANCE across the execution because
// flipping `enabled` is a desired-state change — a disabled rule must stop
// riding the relay's edge_rules — so it travels the ordinary resource update
// path and nudges relays exactly as the same edit made one row at a time
// through PATCH does. (A target's own progress through the Job deliberately
// does NOT advance it; that is asserted separately below.)
func TestBulkEnableFlipsMatchedAutomations(t *testing.T) {
	e := newEnv(t)
	e.placementNode(t)
	root := e.auth.Credential()
	a := e.createAutomation(t, root, autoScopeNode, map[string]string{"env": "prod"})
	b := e.createAutomation(t, root, autoScopeNode, map[string]string{"env": "prod"})
	other := e.createAutomation(t, root, autoScopeNode, map[string]string{"env": "staging"})

	// The fixture creates every automation enabled, so a `false` bulk-enable is
	// a genuine change on the matched rows rather than a no-op that would look
	// identical to doing nothing at all.
	for _, id := range []string{a, b, other} {
		if !e.automationEnabled(t, root, id) {
			t.Fatalf("fixture automation %s is already disabled: the case cannot tell a flip from a no-op", id)
		}
	}

	jobID, _ := e.bulkEnable(t, root, "env=prod", false)
	genBefore := e.generation(t)

	e.runJobs()

	if e.automationEnabled(t, root, a) {
		t.Fatalf("matched automation %s is still enabled: bulk-enable accepted the work and never did it", a)
	}
	if e.automationEnabled(t, root, b) {
		t.Fatalf("matched automation %s is still enabled: bulk-enable accepted the work and never did it", b)
	}
	if !e.automationEnabled(t, root, other) {
		t.Fatalf("automation %s was disabled but the selector never matched it — the write escaped its target set", other)
	}

	if genAfter := e.generation(t); genAfter <= genBefore {
		t.Fatalf("store generation did not advance across the execution: %d -> %d — flipping `enabled` is a desired-state change and MUST nudge relays like any other authored edit", genBefore, genAfter)
	}

	// And the Job reports what happened: every target terminal-succeeded, so the
	// job-level state is `succeeded` (API-113).
	final := assertJobShape(t, mustPoll(t, e, root, jobID), root.PrincipalID)
	if final.State != "succeeded" {
		t.Fatalf("job state = %q, want succeeded (every target succeeded, API-113)", final.State)
	}
	want := map[string]string{a: "succeeded", b: "succeeded"}
	if got := targetStates(final); !reflect.DeepEqual(got, want) {
		t.Fatalf("target states = %v, want %v", got, want)
	}
}

// TestBulkEnablePollObservesProgressToTerminal is API-112 end to end: a client
// that knows nothing but the job id learns the operation finished by re-reading
// the Job, and the states it reads are produced by the real executor rather than
// written into the record by the test.
//
// Both polls go through the mux. The first is taken while the runner is still
// stopped, so it observes the accepted job; the second after the runner has
// drained.
func TestBulkEnablePollObservesProgressToTerminal(t *testing.T) {
	e := newEnv(t)
	e.placementNode(t)
	root := e.auth.Credential()
	a := e.createAutomation(t, root, autoScopeNode, map[string]string{"env": "prod"})

	jobID, _ := e.bulkEnable(t, root, "env=prod", false)

	before := assertJobShape(t, mustPoll(t, e, root, jobID), root.PrincipalID)
	if before.State != "pending" {
		t.Fatalf("job state before execution = %q, want pending", before.State)
	}
	if got := targetStates(before); !reflect.DeepEqual(got, map[string]string{a: "pending"}) {
		t.Fatalf("target states before execution = %v, want the single matched target pending", got)
	}

	e.runJobs()

	after := assertJobShape(t, mustPoll(t, e, root, jobID), root.PrincipalID)
	if after.State != "succeeded" {
		t.Fatalf("job state after execution = %q, want succeeded — a poll MUST reach a terminal value (API-112/113)", after.State)
	}
	if got := targetStates(after); !reflect.DeepEqual(got, map[string]string{a: "succeeded"}) {
		t.Fatalf("target states after execution = %v, want the matched target succeeded", got)
	}
}

// TestBulkEnableIsPartialWhenOneTargetFails forces a GENUINE per-target failure
// and asserts the two things that follow from it: the failure does not abort the
// rest of the job, and the job-level outcome is `partial` — API-113's value for
// mixed target outcomes, which is job-level only and can only arise from targets
// that really did diverge.
//
// The failure is real, not simulated: one matched automation is DELETED between
// the job's acceptance and its execution, so the executor's write finds nothing
// at that id and fails the target NOT_FOUND. This is deterministic because the
// runner is created stopped — the delete lands, provably, before any target runs
// — and it is the honest shape of the race a fleet operation actually has, since
// the target set is resolved when the job is accepted and the world may move
// before it runs.
func TestBulkEnableIsPartialWhenOneTargetFails(t *testing.T) {
	e := newEnv(t)
	e.placementNode(t)
	root := e.auth.Credential()
	survivor := e.createAutomation(t, root, autoScopeNode, map[string]string{"env": "prod"})
	doomed := e.createAutomation(t, root, autoScopeNode, map[string]string{"env": "prod"})

	jobID, accepted := e.bulkEnable(t, root, "env=prod", false)
	acceptedJob := assertJobShape(t, accepted, root.PrincipalID)
	if got := sortedKeys(targetStates(acceptedJob)); !reflect.DeepEqual(got, sortedCopy([]string{survivor, doomed})) {
		t.Fatalf("accepted targets = %v, want both matched automations", got)
	}

	// Delete one target out from under the accepted job. The runner has not been
	// released, so this happens strictly before the executor touches either row.
	resp, raw := e.as(t, root, http.MethodDelete, "/api/v1/automations/"+doomed, nil,
		map[string]string{"If-Match": `"1"`})
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete %s = %d, want 204 (body %s)", doomed, resp.StatusCode, raw)
	}

	e.runJobs()

	final := assertJobShape(t, mustPoll(t, e, root, jobID), root.PrincipalID)
	if final.State != "partial" {
		t.Fatalf("job state = %q, want partial — outcomes were mixed (API-113)", final.State)
	}
	want := map[string]string{survivor: "succeeded", doomed: "failed"}
	if got := targetStates(final); !reflect.DeepEqual(got, want) {
		t.Fatalf("target states = %v, want %v — one target's failure MUST NOT abort the rest", got, want)
	}

	// The isolation is not merely a reported state: the surviving target's row
	// really was written.
	if e.automationEnabled(t, root, survivor) {
		t.Fatalf("automation %s is still enabled: a sibling target's failure aborted the job's remaining work", survivor)
	}
}

// TestBulkEnableFailedTargetReportsWhyOnTheWire is API-115 made OBSERVABLE: a
// client that polls a Job and finds one of its targets `failed` can read, from
// that same body, the api/1 registry code the failure was typed with — without
// which "this target failed" is the whole of what the surface will ever say.
//
// The failure is the same genuine one the partial case produces (a matched
// automation deleted between acceptance and execution), so the code asserted
// here is one the executor really derived from a real store outcome, not a
// value written into the record by the test. NOT_FOUND is the registry's own
// value for "no resource exists at the identifier named" (API-014's taxonomy),
// which is exactly what the deleted row is — and it is the SAME code a
// synchronous DELETE-then-GET of that id would answer with, which is the
// "diagnosable the same way any other api/1 error is" half of API-115.
//
// The surviving sibling is asserted to carry NO error, because "always attach
// an error" would satisfy the failed half while making the member meaningless.
func TestBulkEnableFailedTargetReportsWhyOnTheWire(t *testing.T) {
	e := newEnv(t)
	e.placementNode(t)
	root := e.auth.Credential()
	survivor := e.createAutomation(t, root, autoScopeNode, map[string]string{"env": "prod"})
	doomed := e.createAutomation(t, root, autoScopeNode, map[string]string{"env": "prod"})

	jobID, _ := e.bulkEnable(t, root, "env=prod", false)

	// Deleted while the runner is still stopped, so the executor provably reaches
	// this target after the row is gone.
	resp, raw := e.as(t, root, http.MethodDelete, "/api/v1/automations/"+doomed, nil,
		map[string]string{"If-Match": `"1"`})
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete %s = %d, want 204 (body %s)", doomed, resp.StatusCode, raw)
	}

	e.runJobs()

	final := assertJobShape(t, mustPoll(t, e, root, jobID), root.PrincipalID)
	byID := map[string]jobTarget{}
	for _, tg := range final.Targets {
		byID[tg.TargetID] = tg
	}

	failed, ok := byID[doomed]
	if !ok {
		t.Fatalf("the deleted target %s is missing from the polled Job entirely: %+v", doomed, final.Targets)
	}
	if failed.State != "failed" {
		t.Fatalf("target %s state = %q, want failed — the fixture no longer produces a genuine failure", doomed, failed.State)
	}
	if failed.Error == nil {
		t.Fatalf("target %s is `failed` and carries no `error`: a client polling GET /jobs/{job_id} can never learn why (API-115)", doomed)
	}
	if failed.Error.Code != "NOT_FOUND" {
		t.Fatalf("target %s error.code = %q, want NOT_FOUND — the row was deleted out from under the accepted job", doomed, failed.Error.Code)
	}
	if failed.Error.Detail == "" {
		t.Fatalf("target %s error carries no `detail`; the executor produced one and it MUST reach the client", doomed)
	}

	survived, ok := byID[survivor]
	if !ok {
		t.Fatalf("the surviving target %s is missing from the polled Job: %+v", survivor, final.Targets)
	}
	if survived.State != "succeeded" {
		t.Fatalf("target %s state = %q, want succeeded", survivor, survived.State)
	}
	if survived.Error != nil {
		t.Fatalf("target %s succeeded and still carries an error %+v: `error` is present if and ONLY if the target failed", survivor, *survived.Error)
	}
}

// TestBulkEnableProgressDoesNotAdvanceGeneration pins the OTHER half of the
// two-writes rule. A target's progress through a Job is not desired state, and
// committing it MUST NOT advance the store generation — otherwise a job over N
// targets would nudge every connected relay 2N times to re-fetch a snapshot that
// changed at most N times.
//
// It is asserted by counting: the generation advance across a whole execution is
// exactly one per target that actually wrote a row. A run over one target that
// SUCCEEDS costs exactly one generation, even though that target committed two
// job transitions (pending -> running -> succeeded) on either side of it.
func TestBulkEnableProgressDoesNotAdvanceGeneration(t *testing.T) {
	e := newEnv(t)
	e.placementNode(t)
	root := e.auth.Credential()
	e.createAutomation(t, root, autoScopeNode, map[string]string{"env": "prod"})

	e.bulkEnable(t, root, "env=prod", false)
	genBefore := e.generation(t)

	e.runJobs()

	if got, want := e.generation(t)-genBefore, int64(1); got != want {
		t.Fatalf("generation advanced by %d across a one-target execution, want %d: only the resource write is desired state — a per-target Job transition MUST NOT bump it", got, want)
	}
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
