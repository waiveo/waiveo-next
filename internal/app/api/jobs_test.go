package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
	"time"

	apiv1 "github.com/maaxton/waiveo-next/api/gen/go"
	"github.com/maaxton/waiveo-next/internal/app/api"
	"github.com/maaxton/waiveo-next/internal/app/auth/authtest"
	"github.com/maaxton/waiveo-next/internal/app/store"
	"github.com/maaxton/waiveo-next/internal/feeder/origin"
	"github.com/maaxton/waiveo-next/internal/shared/apihttp"
	"github.com/maaxton/waiveo-next/internal/shared/apijob"
	"github.com/maaxton/waiveo-next/internal/shared/ulid"
)

// jobs_test.go drives GET /api/v1/jobs/{job_id} over the REAL authenticated mux:
// the poll API-112 makes the only way a client learns an async operation
// finished. Every case here creates its Job the way a client does — by calling
// an async endpoint (POST /automations/bulk-enable) — never by writing one
// straight into the store, so what is polled is what the shipped surface
// actually accepted.

// absentJobID is a fixture ULID (no secrets) no job is ever minted under, so a
// read of it exercises a genuinely nonexistent Job — the answer an out-of-reach
// Job's read must be indistinguishable from.
const absentJobID = "01J8Z0ABSENTJ0B0000000000Z"

// envOverStore builds a testEnv over an ALREADY-OPEN store and an EXISTING auth
// fixture. It is the seam the restart case needs: a second api.New "process"
// standing up over the same database file, driven by the same credential.
func envOverStore(t *testing.T, st *store.Store, fixture *authtest.Fixture) *testEnv {
	t.Helper()
	clock := func() int64 { return fixedNowMs }
	idem := apihttp.NewIdempotencyStore(clock, 0)
	content := origin.New()
	jobs := api.NewJobRunner()
	ts := httptest.NewServer(api.New(st, idem, clock, ulid.Monotonic(), content, testContentBase, fixture.Auth,
		api.WithJobRunner(jobs)))
	t.Cleanup(ts.Close)
	return &testEnv{ts: ts, store: st, content: content, contentBase: testContentBase, auth: fixture, jobs: jobs}
}

// newFileEnv is newEnv over a FILE-backed store under the test's own temp dir,
// returning the dsn so a case can close the store and open the same database
// again. A :memory: store's contents are this process's memory, so reopening one
// would prove nothing about surviving a restart.
func newFileEnv(t *testing.T) (*testEnv, string) {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "app.db")
	st, err := store.Open(dsn, store.WallClockMs)
	if err != nil {
		t.Fatalf("store.Open(%s): %v", dsn, err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return envOverStore(t, st, newAuthFixture(t)), dsn
}

// createAutomation POSTs one automation as who and returns its server-minted id.
func (e *testEnv) createAutomation(t *testing.T, who authtest.Credential, scopeNode string, labels map[string]string) string {
	t.Helper()
	resp, raw := e.as(t, who, http.MethodPost, "/api/v1/automations", edgeAutomationBody("", scopeNode, labels), nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create automation at %s: status %d, body %s", scopeNode, resp.StatusCode, raw)
	}
	return decodeID(t, raw)
}

// bulkEnable drives the async endpoint as who and returns the accepted Job's id
// plus the raw 202 body, failing on any non-202.
func (e *testEnv) bulkEnable(t *testing.T, who authtest.Credential, selector string, enabled bool) (string, []byte) {
	t.Helper()
	req := mustJSON(t, map[string]any{"selector": selector, "enabled": enabled})
	resp, raw := e.as(t, who, http.MethodPost, "/api/v1/automations/bulk-enable", req, nil)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("bulk-enable: status %d, want 202 (body %s)", resp.StatusCode, raw)
	}
	return decodeID(t, raw), raw
}

// getJob drives one poll as who and returns the response and its body.
func (e *testEnv) getJob(t *testing.T, who authtest.Credential, jobID string) (*http.Response, []byte) {
	t.Helper()
	return e.as(t, who, http.MethodGet, "/api/v1/jobs/"+jobID, nil, nil)
}

// jobBody is API-112's required shape as this package reads it back off the
// wire. It is deliberately NOT the generated apiv1.Job: decoding into the same
// type the handler encoded from would let a member the contract requires go
// missing without any case noticing.
type jobBody struct {
	ID        string      `json:"id"`
	Targets   []jobTarget `json:"targets"`
	CreatedBy string      `json:"created_by"`
	State     string      `json:"state"`
	CreatedAt string      `json:"created_at"`
}

// jobTarget is one entry of API-112's `targets` array as it comes back off the
// wire, including the per-target failure report API-115 requires a `failed`
// entry to be diagnosable from.
type jobTarget struct {
	TargetID string `json:"target_id"`
	State    string `json:"state"`
	Error    *struct {
		Code   string `json:"code"`
		Detail string `json:"detail"`
	} `json:"error"`
}

func decodeJob(t *testing.T, raw []byte) jobBody {
	t.Helper()
	var j jobBody
	if err := json.Unmarshal(raw, &j); err != nil {
		t.Fatalf("decode job: %v (body %s)", err, raw)
	}
	return j
}

// assertJobShape checks the polled body against API-112 field for field: `id` a
// ULID, `targets` an array of {target_id, state} — plus `error` on a FAILED
// entry and NOTHING else — `created_by` the submitting principal, `state` a
// member of API-113's closed job-state set, `created_at` an RFC3339 instant —
// and no member outside that set, so the poll cannot quietly grow a field the
// contract does not define.
//
// The per-target `error` is asserted as a BICONDITIONAL, not as an allowance: a
// failed target MUST carry one and a non-failed target MUST NOT. An "error is
// optional everywhere" check would pass just as happily against the defect this
// member was added to close — a `failed` target rendered with no way to learn
// why — which is the one outcome it must catch.
func assertJobShape(t *testing.T, raw []byte, wantCreatedBy string) jobBody {
	t.Helper()
	var members map[string]json.RawMessage
	if err := json.Unmarshal(raw, &members); err != nil {
		t.Fatalf("decode job members: %v (body %s)", err, raw)
	}
	got := make([]string, 0, len(members))
	for k := range members {
		got = append(got, k)
	}
	sort.Strings(got)
	want := []string{"created_at", "created_by", "id", "state", "targets"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("job members = %v, want exactly %v (API-112)", got, want)
	}

	j := decodeJob(t, raw)
	if !ulid.Valid(j.ID) {
		t.Fatalf("job id %q is not a ULID (API-112)", j.ID)
	}
	if j.CreatedBy != wantCreatedBy {
		t.Fatalf("job created_by = %q, want the submitting principal %q (API-112)", j.CreatedBy, wantCreatedBy)
	}
	if !validJobState(j.State) {
		t.Fatalf("job state = %q, outside API-113's closed set", j.State)
	}
	if _, err := time.Parse(time.RFC3339, j.CreatedAt); err != nil {
		t.Fatalf("job created_at %q is not RFC3339: %v", j.CreatedAt, err)
	}

	var targetMembers []map[string]json.RawMessage
	if err := json.Unmarshal(members["targets"], &targetMembers); err != nil {
		t.Fatalf("decode targets: %v (body %s)", err, raw)
	}
	for i, tm := range targetMembers {
		keys := make([]string, 0, len(tm))
		for k := range tm {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		st := j.Targets[i].State
		if !validTargetState(st) {
			t.Fatalf("targets[%d].state = %q; `partial` is job-level only and nothing else is a target state (API-113)", i, st)
		}
		wantKeys := []string{"state", "target_id"}
		if st == string(apiv1.JobTargetStateFailed) {
			wantKeys = []string{"error", "state", "target_id"}
		}
		if !reflect.DeepEqual(keys, wantKeys) {
			t.Fatalf("targets[%d] (%s) members = %v, want exactly %v (API-112/115)", i, st, keys, wantKeys)
		}
		if st != string(apiv1.JobTargetStateFailed) {
			continue
		}
		// The failure is typed from api/1's OWN registry (API-115), which is what
		// makes it diagnosable the same way any other api/1 error is. Membership
		// is checked against the shared closed set rather than against whatever
		// this particular target happened to report.
		if !apijob.CodeInRegistry(j.Targets[i].Error.Code) {
			t.Fatalf("targets[%d].error.code = %q, outside api/1's error-code registry (API-115/API-014)", i, j.Targets[i].Error.Code)
		}
	}
	return j
}

func validJobState(s string) bool {
	switch apiv1.JobState(s) {
	case apiv1.JobStatePending, apiv1.JobStateRunning, apiv1.JobStateSucceeded,
		apiv1.JobStateFailed, apiv1.JobStatePartial:
		return true
	}
	return false
}

func validTargetState(s string) bool {
	switch apiv1.JobTargetState(s) {
	case apiv1.JobTargetStatePending, apiv1.JobTargetStateRunning,
		apiv1.JobTargetStateSucceeded, apiv1.JobTargetStateFailed:
		return true
	}
	return false
}

// TestJobFromAsyncEndpointIsPollable is the central claim: the Job a
// fleet-mutating operation returns with 202 can be READ BACK at
// GET /jobs/{job_id}, and what comes back is that same Job — API-112's
// "a client determines completion by reading the Job resource again".
func TestJobFromAsyncEndpointIsPollable(t *testing.T) {
	e := newEnv(t)
	e.placementNode(t)
	root := e.auth.Credential()
	a := e.createAutomation(t, root, autoScopeNode, map[string]string{"env": "prod"})
	b := e.createAutomation(t, root, autoScopeNode, map[string]string{"env": "prod"})
	e.createAutomation(t, root, autoScopeNode, map[string]string{"env": "staging"})

	jobID, accepted := e.bulkEnable(t, root, "env=prod", false)

	resp, raw := e.getJob(t, root, jobID)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /jobs/%s = %d, want 200 (body %s)", jobID, resp.StatusCode, raw)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("poll content-type = %q, want application/json", ct)
	}
	polled := assertJobShape(t, raw, root.PrincipalID)

	// The polled resource IS the accepted one, member for member — not a
	// separately-built shape that happens to share an id.
	acceptedJob := assertJobShape(t, accepted, root.PrincipalID)
	if !reflect.DeepEqual(polled, acceptedJob) {
		t.Fatalf("polled Job differs from the accepted 202 Job\npolled   %+v\naccepted %+v", polled, acceptedJob)
	}
	if polled.ID != jobID {
		t.Fatalf("polled job id = %q, want %q", polled.ID, jobID)
	}

	// The target set is the selector-matched one, still pending. This env's job
	// runner has not been released (runJobs), so what the poll reports here is
	// the ACCEPTED job — API-111's "accepted, not-yet-complete work" — which is
	// exactly what makes it comparable to the 202 body above. Execution's own
	// effect on the poll is asserted by the bulk-enable execution cases.
	gotTargets := map[string]string{}
	for _, tg := range polled.Targets {
		gotTargets[tg.TargetID] = tg.State
	}
	want := map[string]string{a: "pending", b: "pending"}
	if !reflect.DeepEqual(gotTargets, want) {
		t.Fatalf("polled targets = %v, want %v", gotTargets, want)
	}
	if polled.State != "pending" {
		t.Fatalf("polled job state = %q, want pending", polled.State)
	}
}

// TestJobPollReportsCurrentState drives the Job down API-113's progression to a
// TERMINAL value between two polls, and asserts each poll reports the state as
// it then stands. It is what separates a real read from a stub that always
// answers with whatever the accepting handler happened to build.
//
// The transitions are applied through the persisted seam the real executor
// drives (store.AdvanceJob) rather than by releasing the runner, because this
// case is about the POLL: it needs to stop the job at an intermediate `running`
// value and read it there, which a released executor would run straight past.
// The env's runner is left stopped so nothing else is moving the record
// underneath these assertions. The poll is driven through the real mux, so what
// is asserted is the HTTP surface's answer.
func TestJobPollReportsCurrentState(t *testing.T) {
	e := newEnv(t)
	e.placementNode(t)
	root := e.auth.Credential()
	a := e.createAutomation(t, root, autoScopeNode, map[string]string{"env": "prod"})
	b := e.createAutomation(t, root, autoScopeNode, map[string]string{"env": "prod"})
	jobID, _ := e.bulkEnable(t, root, "env=prod", true)

	if got := pollState(t, e, root, jobID); got != "pending" {
		t.Fatalf("state before any transition = %q, want pending", got)
	}

	if _, err := e.store.AdvanceJob(context.Background(), jobID, func(j *apijob.Job) error {
		return j.StartTarget(a)
	}); err != nil {
		t.Fatalf("AdvanceJob(start %s): %v", a, err)
	}
	if got := pollState(t, e, root, jobID); got != "running" {
		t.Fatalf("state with one target started = %q, want running (API-113)", got)
	}

	if _, err := e.store.AdvanceJob(context.Background(), jobID, func(j *apijob.Job) error {
		if err := j.SucceedTarget(a); err != nil {
			return err
		}
		if err := j.StartTarget(b); err != nil {
			return err
		}
		return j.SucceedTarget(b)
	}); err != nil {
		t.Fatalf("AdvanceJob(finish both): %v", err)
	}

	_, raw := e.getJob(t, root, jobID)
	final := assertJobShape(t, raw, root.PrincipalID)
	if final.State != "succeeded" {
		t.Fatalf("terminal job state = %q, want succeeded (every target succeeded, API-113)", final.State)
	}
	for _, tg := range final.Targets {
		if tg.State != "succeeded" {
			t.Fatalf("target %s = %q, want succeeded", tg.TargetID, tg.State)
		}
	}

	// An illegal transition is refused, and the refusal changes nothing the poll
	// can see: a target already terminal cannot be started again (API-113's
	// progression is closed and one-way).
	if _, err := e.store.AdvanceJob(context.Background(), jobID, func(j *apijob.Job) error {
		return j.StartTarget(a)
	}); err == nil {
		t.Fatal("restarting an already-succeeded target was accepted (API-113)")
	}
	_, raw = e.getJob(t, root, jobID)
	if after := assertJobShape(t, raw, root.PrincipalID); !reflect.DeepEqual(after, final) {
		t.Fatalf("a refused transition changed what the poll reports\nbefore %+v\nafter  %+v", final, after)
	}
}

func pollState(t *testing.T, e *testEnv, who authtest.Credential, jobID string) string {
	t.Helper()
	resp, raw := e.getJob(t, who, jobID)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /jobs/%s = %d, want 200 (body %s)", jobID, resp.StatusCode, raw)
	}
	return assertJobShape(t, raw, who.PrincipalID).State
}

// TestJobSurvivesProcessRestart is API-116 through the mux: a Job accepted by an
// async endpoint, with one target already terminal, is polled from a SECOND
// api.New handler standing over the SAME database file after the first store was
// closed — the shape a crash-and-restart takes in-process.
//
// The auth fixture is deliberately carried across: identity lives in its own
// store, and re-minting the caller would change WHO is polling, which is not
// what this case is about.
func TestJobSurvivesProcessRestart(t *testing.T) {
	e, dsn := newFileEnv(t)
	e.placementNode(t)
	root := e.auth.Credential()
	a := e.createAutomation(t, root, autoScopeNode, map[string]string{"env": "prod"})
	b := e.createAutomation(t, root, autoScopeNode, map[string]string{"env": "prod"})
	jobID, _ := e.bulkEnable(t, root, "env=prod", true)

	if _, err := e.store.AdvanceJob(context.Background(), jobID, func(j *apijob.Job) error {
		if err := j.StartTarget(a); err != nil {
			return err
		}
		return j.SucceedTarget(a)
	}); err != nil {
		t.Fatalf("AdvanceJob: %v", err)
	}
	before := assertJobShape(t, mustPoll(t, e, root, jobID), root.PrincipalID)

	// The "process" ends: the store is closed, and a fresh one is opened over
	// the same file behind a fresh handler.
	if err := e.store.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
	reopened, err := store.Open(dsn, store.WallClockMs)
	if err != nil {
		t.Fatalf("reopen %s: %v", dsn, err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	restarted := envOverStore(t, reopened, e.auth)

	after := assertJobShape(t, mustPoll(t, restarted, root, jobID), root.PrincipalID)
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("the Job did not survive the restart intact (API-116)\nbefore %+v\nafter  %+v", before, after)
	}
	states := map[string]string{}
	for _, tg := range after.Targets {
		states[tg.TargetID] = tg.State
	}
	if want := (map[string]string{a: "succeeded", b: "pending"}); !reflect.DeepEqual(states, want) {
		t.Fatalf("target states after restart = %v, want %v — an already-reached terminal state MUST NOT be lost (API-116)", states, want)
	}
}

func mustPoll(t *testing.T, e *testEnv, who authtest.Credential, jobID string) []byte {
	t.Helper()
	resp, raw := e.getJob(t, who, jobID)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /jobs/%s = %d, want 200 (body %s)", jobID, resp.StatusCode, raw)
	}
	return raw
}

// TestJobUnknownIDIsNotFound: an id no Job was minted under is 404 / NOT_FOUND,
// named by the resource's own noun.
func TestJobUnknownIDIsNotFound(t *testing.T) {
	e := newEnv(t)
	resp, raw := e.getJob(t, e.auth.Credential(), absentJobID)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET an unminted job id = %d, want 404 (body %s)", resp.StatusCode, raw)
	}
	p := assertProblem(t, resp, raw, "NOT_FOUND")
	if detail, _ := p["detail"].(string); detail != "No job exists with this identifier." {
		t.Fatalf("404 detail = %q, want it to name the resource by its displayName", detail)
	}
}

// TestJobOutOfReachReadsAsNonexistent is the scoping claim, and the reason it is
// stated as an INDISTINGUISHABILITY rather than as a status code: a Job's body
// enumerates by id the rows it acts on (API-112), so a caller who cannot read
// those rows must not learn the Job exists either. The refusal is therefore
// byte-for-byte the answer an id that was never minted gets — a 403 would
// confirm something is there, which is the existence oracle EVT-122 forbids.
//
// Three callers, one Job:
//   - the submitter (bound at site A) polls their own Job — 200;
//   - the root-bound principal, who can read every target, also gets 200, which
//     is what proves the Job genuinely exists and B's answer is scoping rather
//     than an empty store;
//   - the site-B principal, whose authority reaches none of the targets, gets
//     exactly the unminted-id answer.
func TestJobOutOfReachReadsAsNonexistent(t *testing.T) {
	e := newEnv(t)
	tr := seedScopedTree(t, e)
	alice := e.principalAt(t, tr.siteA)
	bob := e.principalAt(t, tr.siteB)

	// One automation under each subtree, both carrying the SAME label, so the
	// selector alone cannot explain which targets the Job ends up with.
	aliceAutomation := e.createAutomation(t, alice, tr.screensA[0], map[string]string{"env": "prod"})
	bobAutomation := e.createAutomation(t, bob, tr.screensB[0], map[string]string{"env": "prod"})

	jobID, accepted := e.bulkEnable(t, alice, "env=prod", true)
	job := assertJobShape(t, accepted, alice.PrincipalID)
	if len(job.Targets) != 1 || job.Targets[0].TargetID != aliceAutomation {
		t.Fatalf("accepted Job targets = %+v, want exactly the site-A automation %s (a selector narrows to the caller's own authority)", job.Targets, aliceAutomation)
	}

	// The submitter polls their own Job.
	if got := pollState(t, e, alice, jobID); got != "pending" {
		t.Fatalf("the submitter's own poll returned state %q, want pending", got)
	}
	// A caller who can read every target sees it too — the control.
	if resp, raw := e.getJob(t, e.auth.Credential(), jobID); resp.StatusCode != http.StatusOK {
		t.Fatalf("the root-bound principal's poll = %d, want 200 (body %s)", resp.StatusCode, raw)
	}

	// The site-B principal reaches none of the targets.
	resp, raw := e.getJob(t, bob, jobID)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("an out-of-reach Job read = %d, want 404 (body %s)", resp.StatusCode, raw)
	}
	outOfReach := assertProblem(t, resp, raw, "NOT_FOUND")

	// Member for member the unminted-id answer, apart from the two members that
	// are functions of the request itself and so can carry no information about
	// the resource: `trace_id` (per-request by construction) and `instance` (the
	// requested path, which the caller supplied and already knows).
	absentResp, absentRaw := e.getJob(t, bob, absentJobID)
	if absentResp.StatusCode != http.StatusNotFound {
		t.Fatalf("unminted job id = %d, want 404", absentResp.StatusCode)
	}
	absent := assertProblem(t, absentResp, absentRaw, "NOT_FOUND")
	for _, perRequest := range []string{"trace_id", "instance"} {
		delete(outOfReach, perRequest)
		delete(absent, perRequest)
	}
	if !reflect.DeepEqual(outOfReach, absent) {
		t.Fatalf("an out-of-reach Job is distinguishable from a nonexistent one\nout-of-reach %v\nnonexistent  %v", outOfReach, absent)
	}

	// And bob's own automation is genuinely readable by bob — so the 404 above is
	// about the Job's targets, not about bob being unable to read anything.
	if resp, raw := e.as(t, bob, http.MethodGet, "/api/v1/automations/"+bobAutomation, nil, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("the site-B principal cannot read their own automation (%d): the fixture, not the scoping, is wrong (body %s)", resp.StatusCode, raw)
	}
}
