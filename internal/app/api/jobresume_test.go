package api_test

// jobresume_test.go drives API-116's RESUME half: what a process does, at
// startup, about targets a previous process committed as `running` and never
// carried to a terminal value.
//
// Every case here restarts for real — the file-backed store is closed and the
// same database file is reopened behind a second api.New — because the whole
// claim is about state that outlived a process. A "restart" that kept one store
// object alive would be asserting nothing that a plain function call does not
// already prove.
//
// The interrupted state itself is staged through store.AdvanceJob(StartTarget),
// which is not a stand-in for the executor's behaviour: it is the executor's own
// first step, verbatim (applyBulkEnableTarget commits `running` BEFORE it
// attempts the write, precisely so a crash mid-write is visible). A target left
// exactly there, with its row not yet written, is what an interrupted run leaves
// behind. There is deliberately no test-only seam in the executor to crash it
// between those two commits — adding one would change the shipped code to suit
// the test, which is a worse trade than staging the transition the shipped code
// itself commits.
//
// Completion is observed by asking the runner (runJobs). No case sleeps.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/maaxton/waiveo-next/internal/app/api"
	"github.com/maaxton/waiveo-next/internal/app/auth/authtest"
	"github.com/maaxton/waiveo-next/internal/app/store"
	"github.com/maaxton/waiveo-next/internal/app/workspacekey"
	"github.com/maaxton/waiveo-next/internal/feeder/origin"
	"github.com/maaxton/waiveo-next/internal/shared/apihttp"
	"github.com/maaxton/waiveo-next/internal/shared/apijob"
	"github.com/maaxton/waiveo-next/internal/shared/ulid"
)

// restart closes e's store and stands a SECOND handler up over the same database
// file, returning the new env. It is the process boundary these cases are about:
// the first env's runner, its in-memory state and its handler are all gone, and
// the only thing that crosses is what was committed to disk.
//
// The auth fixture is carried across deliberately — identity lives in its own
// store, and re-minting the caller would change who is polling.
func restart(t *testing.T, e *testEnv, dsn string) *testEnv {
	t.Helper()
	if err := e.store.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
	reopened, err := store.Open(dsn, store.WallClockMs)
	if err != nil {
		t.Fatalf("reopen %s: %v", dsn, err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	return envOverStore(t, reopened, e.auth)
}

// strandTarget commits ONE target of an accepted Job to `running` and stops
// there — the executor's own pre-write step, and so exactly the record an
// interrupted run leaves. It deliberately does NOT touch the target's row: the
// point of the state is that whether the write landed is unknowable from here.
func strandTarget(t *testing.T, e *testEnv, jobID, targetID string) {
	t.Helper()
	if _, err := e.store.AdvanceJob(context.Background(), jobID, func(j *apijob.Job) error {
		return j.StartTarget(targetID)
	}); err != nil {
		t.Fatalf("stranding target %s of job %s: %v", targetID, jobID, err)
	}
}

// finishTarget drives one target all the way to `succeeded`, standing in for a
// target the interrupted run had already completed and reported before it died.
func finishTarget(t *testing.T, e *testEnv, jobID, targetID string) {
	t.Helper()
	if _, err := e.store.AdvanceJob(context.Background(), jobID, func(j *apijob.Job) error {
		if err := j.StartTarget(targetID); err != nil {
			return err
		}
		return j.SucceedTarget(targetID)
	}); err != nil {
		t.Fatalf("finishing target %s of job %s: %v", targetID, jobID, err)
	}
}

// automationRevision reads one automation's current revision off the wire (the
// ETag), which is how a case tells "this row was written again" from "this row
// was left alone" without reaching into the store.
func (e *testEnv) automationRevision(t *testing.T, who authtest.Credential, id string) string {
	t.Helper()
	resp, raw := e.as(t, who, http.MethodGet, "/api/v1/automations/"+id, nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET automation %s = %d, want 200 (body %s)", id, resp.StatusCode, raw)
	}
	etag := resp.Header.Get("ETag")
	if etag == "" {
		t.Fatalf("GET automation %s returned no ETag; the revision probe has nothing to compare", id)
	}
	return etag
}

// jobTargetsByID indexes a polled Job's targets so a case names them rather than
// indexing by position.
func jobTargetsByID(j jobBody) map[string]jobTarget {
	out := make(map[string]jobTarget, len(j.Targets))
	for _, tg := range j.Targets {
		out[tg.TargetID] = tg
	}
	return out
}

// TestInterruptedBulkEnableResumesAfterRestart is API-116's resume, end to end
// and through the shipped path: a bulk-enable that a process accepted and died
// part-way through is picked up by the NEXT process, which finishes the work and
// drives every stranded target to a terminal value.
//
// The job is set up so that all three things that can go wrong are separately
// visible:
//
//   - `done` was already succeeded before the crash. It must NOT be re-applied —
//     its row's revision is captured before the restart and asserted unchanged
//     after, which is the double-apply this design can actually prevent
//     (ResumeTarget refuses a target that is not still `running`).
//   - `stranded` was committed `running` and its row never written. It must be
//     finished: the flip really lands, and the target reaches `succeeded`. A
//     resume that only reported would leave the automation enabled forever.
//   - `vanished` was committed `running` and its automation was then DELETED. It
//     must terminate `failed` NOT_FOUND rather than resurrect a write against a
//     row that no longer exists — the operation no longer makes sense for that
//     target, and saying so is the resume working.
//
// The delete is applied on the restarted handler BEFORE its runner is released,
// so it provably lands before the resume reaches that target: the shape of a row
// removed while the process was down.
func TestInterruptedBulkEnableResumesAfterRestart(t *testing.T) {
	e, dsn := newFileEnv(t)
	root := e.auth.Credential()
	done := e.createAutomation(t, root, autoScopeNode, map[string]string{"env": "prod"})
	stranded := e.createAutomation(t, root, autoScopeNode, map[string]string{"env": "prod"})
	vanished := e.createAutomation(t, root, autoScopeNode, map[string]string{"env": "prod"})

	// The fixture creates every automation enabled, so disabling is a real change
	// rather than a no-op indistinguishable from doing nothing.
	for _, id := range []string{done, stranded, vanished} {
		if !e.automationEnabled(t, root, id) {
			t.Fatalf("fixture automation %s is already disabled: the case cannot tell a flip from a no-op", id)
		}
	}

	jobID, _ := e.bulkEnable(t, root, "env=prod", false)

	// The interrupted run's progress, committed exactly as the executor commits
	// it. The env's runner is never released, so nothing else moves this record.
	finishTarget(t, e, jobID, done)
	strandTarget(t, e, jobID, stranded)
	strandTarget(t, e, jobID, vanished)
	doneRevision := e.automationRevision(t, root, done)

	restarted := restart(t, e, dsn)

	// The row one stranded target names is removed while the work is suspended.
	resp, raw := restarted.as(t, root, http.MethodDelete, "/api/v1/automations/"+vanished, nil,
		map[string]string{"If-Match": `"1"`})
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete %s = %d, want 204 (body %s)", vanished, resp.StatusCode, raw)
	}

	restarted.runJobs()

	final := assertJobShape(t, mustPoll(t, restarted, root, jobID), root.PrincipalID)
	targets := jobTargetsByID(final)

	if got := targets[done].State; got != "succeeded" {
		t.Fatalf("target %s = %q after the restart, want succeeded — an already-reached terminal state MUST survive (API-116)", done, got)
	}
	if got := restarted.automationRevision(t, root, done); got != doneRevision {
		t.Fatalf("automation %s revision moved %s -> %s across the resume: a target that had already finished was applied a second time", done, doneRevision, got)
	}

	if got := targets[stranded].State; got != "succeeded" {
		t.Fatalf("target %s = %q after the restart, want succeeded — a target left running MUST be resumed, not dropped (API-116)", stranded, got)
	}
	if restarted.automationEnabled(t, root, stranded) {
		t.Fatalf("automation %s is still enabled: the resume reported a terminal state without doing the work it was resuming", stranded)
	}

	vanishedTarget := targets[vanished]
	if vanishedTarget.State != "failed" {
		t.Fatalf("target %s = %q, want failed — its automation was deleted, so the accepted operation no longer applies to it", vanished, vanishedTarget.State)
	}
	if vanishedTarget.Error == nil || vanishedTarget.Error.Code != "NOT_FOUND" {
		t.Fatalf("target %s error = %+v, want NOT_FOUND: a resumed target whose row is gone reports that, it does not resurrect the write", vanished, vanishedTarget.Error)
	}

	// Every target is terminal, so the job itself is terminal: the poll a client
	// has been holding since before the crash finally completes (API-112/113).
	if final.State != "partial" {
		t.Fatalf("job state after the resume = %q, want partial — two targets succeeded and one failed (API-113)", final.State)
	}
}

// TestResumeDoesNotDisturbThisProcessesOwnRun is the guard against the resume
// being too eager. It is defined over targets an EARLIER process left running,
// and a target the CURRENT process has legitimately just started looks
// identical in the record — so a resume that read its inventory late, racing
// this process's own accepted work, would reconcile live executions as
// abandoned.
//
// Both jobs run on the same released runner, so if the resume's inventory were
// read at execution time rather than captured before the handler served
// anything, the fresh job's targets would be in it.
func TestResumeDoesNotDisturbThisProcessesOwnRun(t *testing.T) {
	e, dsn := newFileEnv(t)
	root := e.auth.Credential()
	old := e.createAutomation(t, root, autoScopeNode, map[string]string{"env": "old"})
	oldJobID, _ := e.bulkEnable(t, root, "env=old", false)
	strandTarget(t, e, oldJobID, old)

	restarted := restart(t, e, dsn)

	// A brand-new job accepted by the restarted process, alongside the resume of
	// the old one.
	fresh := restarted.createAutomation(t, root, autoScopeNode, map[string]string{"env": "fresh"})
	freshJobID, _ := restarted.bulkEnable(t, root, "env=fresh", false)

	restarted.runJobs()

	freshJob := assertJobShape(t, mustPoll(t, restarted, root, freshJobID), root.PrincipalID)
	if got := jobTargetsByID(freshJob)[fresh].State; got != "succeeded" {
		t.Fatalf("this process's own target %s = %q, want succeeded — the resume swept up live work", fresh, got)
	}
	if restarted.automationEnabled(t, root, fresh) {
		t.Fatalf("automation %s is still enabled: the fresh job did not run", fresh)
	}

	oldJob := assertJobShape(t, mustPoll(t, restarted, root, oldJobID), root.PrincipalID)
	if got := jobTargetsByID(oldJob)[old].State; got != "succeeded" {
		t.Fatalf("the interrupted target %s = %q, want succeeded", old, got)
	}
}

// TestStrandedTargetOfUnresumableJobIsReconciled covers the other branch: a Job
// that deliberately persisted NO re-appliable operation (the data-subject
// export, whose only argument is a client passphrase that must not be written to
// disk) still has to stop being `running` after a restart.
//
// Leaving it there is the failure that matters. API-112 makes polling the only
// completion signal a client has, so a target parked at `running` forever is a
// poll that never terminates — a silent drop wearing a non-terminal state. The
// resume reports it `failed` with a registry code (API-115) and a detail saying
// the server restarted, which is exactly what is known and nothing more.
func TestStrandedTargetOfUnresumableJobIsReconciled(t *testing.T) {
	e, dsn := newFileWorkspaceEnv(t)
	orgID := e.seedWorkspace(t)
	who := e.auth.Credential()

	resp, raw := e.postWorkspace(t, who, "export", map[string]any{"passphrase": testExportPassphrase})
	job := acceptedJob(t, resp, raw)
	strandTarget(t, e.testEnv, job.ID, orgID)

	restarted := restart(t, e.testEnv, dsn)
	restarted.runJobs()

	final := assertJobShape(t, mustPoll(t, restarted, who, job.ID), who.PrincipalID)
	target := jobTargetsByID(final)[orgID]
	if target.State != "failed" {
		t.Fatalf("stranded target %s = %q after the restart, want failed — a target no restart can resume MUST NOT be left running indefinitely (API-112/116)", orgID, target.State)
	}
	if target.Error == nil {
		t.Fatalf("reconciled target %s carries no error: a client is told the target failed and never why (API-115)", orgID)
	}
	if target.Error.Code != "INTERNAL" {
		t.Fatalf("reconciled target %s error.code = %q, want INTERNAL — the server cannot claim the work was merely not served, only that its outcome is unknown", orgID, target.Error.Code)
	}
	if target.Error.Detail == "" {
		t.Fatalf("reconciled target %s carries no detail; an operator reading this job report has no way to tell a restart from any other INTERNAL", orgID)
	}
	if final.State != "failed" {
		t.Fatalf("job state = %q, want failed (its only target failed, API-113)", final.State)
	}
}

// newFileWorkspaceEnv is newWorkspaceEnv over a FILE-backed store, returning the
// dsn so the export case above can restart for real. The archive destination and
// signing key are wired identically — the export must be ACCEPTED (an
// unconfigured deployment refuses with UNAVAILABLE before minting a Job), even
// though this case never lets it run.
func newFileWorkspaceEnv(t *testing.T) (*workspaceEnv, string) {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "app.db")
	st, err := store.Open(dsn, store.WallClockMs)
	if err != nil {
		t.Fatalf("store.Open(%s): %v", dsn, err)
	}
	t.Cleanup(func() { _ = st.Close() })

	dir := t.TempDir()
	keyDir := t.TempDir()
	key, err := workspacekey.LoadOrCreate(keyDir, func() string { return workspaceKeyID })
	if err != nil {
		t.Fatalf("workspacekey.LoadOrCreate: %v", err)
	}

	clock := func() int64 { return fixedNowMs }
	idem := apihttp.NewIdempotencyStore(clock, 0)
	fixture := newAuthFixture(t)
	content := origin.New()
	jobs := api.NewJobRunner()
	ts := httptest.NewServer(api.New(st, idem, clock, ulid.Monotonic(), content, testContentBase, fixture.Auth,
		api.WithJobRunner(jobs),
		api.WithWorkspaceArchive(&api.WorkspaceArchive{Dir: dir, Key: key, KDF: lightKDF()})))
	t.Cleanup(ts.Close)

	return &workspaceEnv{
		testEnv:    &testEnv{ts: ts, store: st, content: content, contentBase: testContentBase, auth: fixture, jobs: jobs},
		archiveDir: dir,
		keyDir:     keyDir,
		key:        key,
	}, dsn
}
