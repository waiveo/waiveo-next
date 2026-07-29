package store_test

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	apiv1 "github.com/maaxton/waiveo-next/api/gen/go"
	"github.com/maaxton/waiveo-next/internal/app/store"
	"github.com/maaxton/waiveo-next/internal/shared/apijob"
)

// jobs_test.go drives the durable half of api/1's Job resource (API-116): the
// per-target execution record a restart must not lose, and the transitions that
// record is written by.
//
// Fixture ULIDs (fixture-ULID convention; no secrets).
const (
	jobID          = "01J8Z9J0BJ0B00000000000000"
	jobTargetAID   = "01J8Z9J0BTARGETA0000000000"
	jobTargetBID   = "01J8Z9J0BTARGETB0000000000"
	jobPrincipalID = "01J8Z9J0BPR1NC1PA100000000"
	jobScopeNodeA  = "01J8Z9J0BSC0PEN0DEA0000000"
	jobScopeNodeB  = "01J8Z9J0BSC0PEN0DEB0000000"
)

// jobCreatedAt is the pinned instant every fixture Job below is accepted at — an
// injected value, never a wall-clock read, so a restored created_at is compared
// against a constant the test declared rather than against "roughly now".
var jobCreatedAt = time.UnixMilli(1_752_537_600_000).UTC()

// newFixtureJob builds the two-target Job every case here starts from, in the
// state the accepting handler would hand the store: both targets pending.
func newFixtureJob() *apijob.Job {
	return apijob.New(jobID, jobPrincipalID, jobCreatedAt, []string{jobTargetAID, jobTargetBID})
}

func fixtureScopes() map[string]string {
	return map[string]string{jobTargetAID: jobScopeNodeA, jobTargetBID: jobScopeNodeB}
}

// fixtureOp is the accepted operation every fixture Job below carries. Its kind
// and payload are DELIBERATELY not the api layer's real vocabulary: the store
// persists an operation opaquely and parses neither field, so a fixture using
// the real names would let a store that quietly interpreted them pass.
func fixtureOp() store.JobOperation {
	return store.JobOperation{Kind: "fixture.operation", Payload: []byte(`{"wanted":true}`)}
}

// openFile opens a FILE-backed store under the test's own temp dir and returns
// it with its dsn, so a case can close it and open the same database again — the
// only honest way to test "survives a restart" (a :memory: store's contents are
// the process's memory, so reopening one would prove nothing).
func openFile(t *testing.T) (*store.Store, string) {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "app.db")
	s, err := store.Open(dsn, store.WallClockMs)
	if err != nil {
		t.Fatalf("Open(%s): %v", dsn, err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, dsn
}

func mustGetJob(t *testing.T, s *store.Store, id string) store.JobRecord {
	t.Helper()
	rec, found, err := s.GetJob(context.Background(), id)
	if err != nil {
		t.Fatalf("GetJob(%s): %v", id, err)
	}
	if !found {
		t.Fatalf("GetJob(%s): not found", id)
	}
	return rec
}

// targetStates projects a record's targets to {id: state} so a case compares the
// whole set at once rather than indexing into a slice by position.
func targetStates(rec store.JobRecord) map[string]apiv1.JobTargetState {
	out := map[string]apiv1.JobTargetState{}
	for _, tg := range rec.Job.Targets() {
		out[tg.ID] = tg.State
	}
	return out
}

// TestCreateJobRoundTrips: a persisted Job comes back as the SAME Job — id,
// submitter, accepted-at instant, target order, per-target state, and the
// scope-node placements a later read is scoped by (API-112's shape, recorded).
func TestCreateJobRoundTrips(t *testing.T) {
	s := openMem(t)
	if err := s.CreateJob(context.Background(), newFixtureJob(), fixtureScopes(), fixtureOp()); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}

	rec := mustGetJob(t, s, jobID)
	if rec.Job.ID != jobID {
		t.Fatalf("restored id = %q, want %q", rec.Job.ID, jobID)
	}
	if rec.Job.CreatedBy != jobPrincipalID {
		t.Fatalf("restored created_by = %q, want %q", rec.Job.CreatedBy, jobPrincipalID)
	}
	if !rec.Job.CreatedAt.Equal(jobCreatedAt) {
		t.Fatalf("restored created_at = %s, want %s", rec.Job.CreatedAt, jobCreatedAt)
	}
	targets := rec.Job.Targets()
	if len(targets) != 2 || targets[0].ID != jobTargetAID || targets[1].ID != jobTargetBID {
		t.Fatalf("restored targets = %+v, want [%s %s] in that order", targets, jobTargetAID, jobTargetBID)
	}
	for _, tg := range targets {
		if tg.State != apiv1.JobTargetStatePending {
			t.Fatalf("target %s restored %q, want pending", tg.ID, tg.State)
		}
	}
	if rec.Job.State() != apiv1.JobStatePending {
		t.Fatalf("restored job state = %q, want pending (every target pending)", rec.Job.State())
	}
	if got := rec.ScopeNodes[jobTargetAID]; got != jobScopeNodeA {
		t.Fatalf("target A placement = %q, want %q", got, jobScopeNodeA)
	}
	if got := rec.ScopeNodes[jobTargetBID]; got != jobScopeNodeB {
		t.Fatalf("target B placement = %q, want %q", got, jobScopeNodeB)
	}
}

// TestGetJobUnknownID: an id no job was ever minted for is simply absent — not
// an error, so the api layer can answer it as a plain 404.
func TestGetJobUnknownID(t *testing.T) {
	s := openMem(t)
	_, found, err := s.GetJob(context.Background(), jobID)
	if err != nil {
		t.Fatalf("GetJob(absent): %v", err)
	}
	if found {
		t.Fatal("GetJob reported an unminted job id as found")
	}
}

// TestCreateJobRejectsNonULIDAndDuplicate: the two ways a Job write is a wiring
// fault rather than a client mistake. DAT-005a requires every persisted id to be
// a valid ULID, and a second Create under an id already held is refused with
// ErrJobExists rather than silently overwriting the first job's target record.
func TestCreateJobRejectsNonULIDAndDuplicate(t *testing.T) {
	s := openMem(t)
	bad := apijob.New("not-a-ulid", jobPrincipalID, jobCreatedAt, []string{jobTargetAID})
	if err := s.CreateJob(context.Background(), bad, nil, fixtureOp()); err == nil {
		t.Fatal("CreateJob accepted a non-ULID job id (DAT-005a)")
	}

	if err := s.CreateJob(context.Background(), newFixtureJob(), fixtureScopes(), fixtureOp()); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	err := s.CreateJob(context.Background(), newFixtureJob(), fixtureScopes(), fixtureOp())
	if !errors.Is(err, store.ErrJobExists) {
		t.Fatalf("second CreateJob under the same id = %v, want ErrJobExists", err)
	}
}

// TestAdvanceJobPersistsTransitionsToTerminal walks one Job the whole way down
// API-113's closed progression — pending -> running -> a terminal value — and
// re-reads the store after EVERY step, so each assertion is about what was
// committed rather than about the in-memory Job the transition was applied to.
//
// The two targets end differently (one succeeded, one failed with a registry
// code, API-115), which is what makes the job-level terminal value `partial` and
// not merely "the last state anyone set".
func TestAdvanceJobPersistsTransitionsToTerminal(t *testing.T) {
	s := openMem(t)
	ctx := context.Background()
	if err := s.CreateJob(ctx, newFixtureJob(), fixtureScopes(), fixtureOp()); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}

	if _, err := s.AdvanceJob(ctx, jobID, func(j *apijob.Job) error { return j.StartTarget(jobTargetAID) }); err != nil {
		t.Fatalf("AdvanceJob(start A): %v", err)
	}
	rec := mustGetJob(t, s, jobID)
	if got := targetStates(rec); got[jobTargetAID] != apiv1.JobTargetStateRunning || got[jobTargetBID] != apiv1.JobTargetStatePending {
		t.Fatalf("after start A, persisted target states = %v, want A running / B pending", got)
	}
	if rec.Job.State() != apiv1.JobStateRunning {
		t.Fatalf("job state = %q, want running (one target started)", rec.Job.State())
	}

	if _, err := s.AdvanceJob(ctx, jobID, func(j *apijob.Job) error { return j.SucceedTarget(jobTargetAID) }); err != nil {
		t.Fatalf("AdvanceJob(succeed A): %v", err)
	}
	if _, err := s.AdvanceJob(ctx, jobID, func(j *apijob.Job) error {
		if err := j.StartTarget(jobTargetBID); err != nil {
			return err
		}
		return j.FailTarget(jobTargetBID, "UNAVAILABLE", "target was unreachable.")
	}); err != nil {
		t.Fatalf("AdvanceJob(start+fail B): %v", err)
	}

	rec = mustGetJob(t, s, jobID)
	if got := targetStates(rec); got[jobTargetAID] != apiv1.JobTargetStateSucceeded || got[jobTargetBID] != apiv1.JobTargetStateFailed {
		t.Fatalf("persisted target states = %v, want A succeeded / B failed", got)
	}
	if rec.Job.State() != apiv1.JobStatePartial {
		t.Fatalf("job state = %q, want partial (mixed outcomes, API-113)", rec.Job.State())
	}
	// The registry-typed per-target error is part of the record, not only of the
	// transition that produced it (API-115).
	for _, tg := range rec.Job.Targets() {
		if tg.ID != jobTargetBID {
			continue
		}
		if tg.ErrCode != "UNAVAILABLE" || tg.ErrDetail != "target was unreachable." {
			t.Fatalf("failed target restored err = {%q, %q}, want the committed registry code and detail", tg.ErrCode, tg.ErrDetail)
		}
	}
}

// TestAdvanceJobRefusesIllegalTransition: succeeding a target that was never
// started is not a legal step in API-113's closed progression, and the refusal
// must leave the record untouched — a transition that half-applies is worse than
// one that is rejected, because the record would then hold a state the state
// machine never produced.
func TestAdvanceJobRefusesIllegalTransition(t *testing.T) {
	s := openMem(t)
	ctx := context.Background()
	if err := s.CreateJob(ctx, newFixtureJob(), fixtureScopes(), fixtureOp()); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}

	_, err := s.AdvanceJob(ctx, jobID, func(j *apijob.Job) error {
		// The legal step first, so the refusal below is the ONLY reason nothing
		// is persisted: if the rollback were missing, target A would come back
		// running and this case would catch it.
		if err := j.StartTarget(jobTargetAID); err != nil {
			return err
		}
		return j.SucceedTarget(jobTargetBID) // B is still pending — illegal.
	})
	if err == nil {
		t.Fatal("AdvanceJob accepted succeeding a target that was never started (API-113)")
	}

	rec := mustGetJob(t, s, jobID)
	for _, tg := range rec.Job.Targets() {
		if tg.State != apiv1.JobTargetStatePending {
			t.Fatalf("a refused transition changed the persisted record: target %s = %q, want pending", tg.ID, tg.State)
		}
	}
	if rec.Job.State() != apiv1.JobStatePending {
		t.Fatalf("job state after a refused transition = %q, want pending", rec.Job.State())
	}
}

// TestAdvanceJobUnknownID: advancing a job that does not exist is ErrNotFound,
// the same sentinel every other missing-row write path returns.
func TestAdvanceJobUnknownID(t *testing.T) {
	s := openMem(t)
	_, err := s.AdvanceJob(context.Background(), jobID, func(j *apijob.Job) error { return nil })
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("AdvanceJob(absent) = %v, want ErrNotFound", err)
	}
}

// TestJobSurvivesStoreRestart is API-116 itself: a target's already-reached
// terminal state, and a target left running, both come back from a REOPENED
// database — not from the process's memory, which is why this case is the only
// one here that uses a file-backed store.
//
// It also asserts the running target is still enumerable as resumable work:
// API-116 requires a restart to "resume any target left `running` rather than
// silently drop it", and a record that quietly reset it to pending, or lost it,
// would satisfy neither half.
func TestJobSurvivesStoreRestart(t *testing.T) {
	ctx := context.Background()
	s, dsn := openFile(t)
	if err := s.CreateJob(ctx, newFixtureJob(), fixtureScopes(), fixtureOp()); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	// A terminal target and a still-running one: the two states API-116 names.
	if _, err := s.AdvanceJob(ctx, jobID, func(j *apijob.Job) error {
		if err := j.StartTarget(jobTargetAID); err != nil {
			return err
		}
		if err := j.SucceedTarget(jobTargetAID); err != nil {
			return err
		}
		return j.StartTarget(jobTargetBID)
	}); err != nil {
		t.Fatalf("AdvanceJob: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened, err := store.Open(dsn, store.WallClockMs)
	if err != nil {
		t.Fatalf("reopen %s: %v", dsn, err)
	}
	defer func() { _ = reopened.Close() }()

	rec := mustGetJob(t, reopened, jobID)
	if got := targetStates(rec); got[jobTargetAID] != apiv1.JobTargetStateSucceeded || got[jobTargetBID] != apiv1.JobTargetStateRunning {
		t.Fatalf("after restart, target states = %v, want A succeeded / B running (API-116)", got)
	}
	if rec.Job.State() != apiv1.JobStateRunning {
		t.Fatalf("after restart, job state = %q, want running", rec.Job.State())
	}
	if rec.Job.CreatedBy != jobPrincipalID || !rec.Job.CreatedAt.Equal(jobCreatedAt) {
		t.Fatalf("after restart, created_by/created_at = %q/%s, want %q/%s",
			rec.Job.CreatedBy, rec.Job.CreatedAt, jobPrincipalID, jobCreatedAt)
	}

	// The inventory a resume reads back carries BOTH halves it needs: the target
	// left running, and the operation that was being applied to it. A restart
	// that recovered only the target list would have nothing to re-apply, which
	// is the state that made "resume" impossible rather than merely unwritten.
	runs, err := reopened.InterruptedRuns(ctx)
	if err != nil {
		t.Fatalf("InterruptedRuns: %v", err)
	}
	want := []store.InterruptedRun{{JobID: jobID, Op: fixtureOp(), TargetIDs: []string{jobTargetBID}}}
	if !reflect.DeepEqual(runs, want) {
		t.Fatalf("InterruptedRuns after restart = %+v, want %+v (the target left running is resumable, not dropped, and the operation came back with it)", runs, want)
	}
	// The succeeded target is NOT in the inventory: a resume that re-applied an
	// already-terminal target would double-apply it.
	for _, run := range runs {
		for _, tid := range run.TargetIDs {
			if tid == jobTargetAID {
				t.Fatalf("the already-succeeded target %s is listed as interrupted work", jobTargetAID)
			}
		}
	}

	// The advance seam still works on the reopened database: a resumed target
	// reaches its terminal state and that, too, is committed.
	if _, err := reopened.AdvanceJob(ctx, jobID, func(j *apijob.Job) error { return j.SucceedTarget(jobTargetBID) }); err != nil {
		t.Fatalf("AdvanceJob after restart: %v", err)
	}
	if got := mustGetJob(t, reopened, jobID); got.Job.State() != apiv1.JobStateSucceeded {
		t.Fatalf("job state after resuming the running target = %q, want succeeded", got.Job.State())
	}
}

// TestCancelPersistsCancellationAttributedFailures: API-114's cancel, driven
// through the same persisted seam. The target already running is left to finish;
// the one never started is failed with a registry-typed, cancellation-attributed
// error — and both facts are read back from the store, not from the Job the
// cancel was applied to.
func TestCancelPersistsCancellationAttributedFailures(t *testing.T) {
	s := openMem(t)
	ctx := context.Background()
	if err := s.CreateJob(ctx, newFixtureJob(), fixtureScopes(), fixtureOp()); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if _, err := s.AdvanceJob(ctx, jobID, func(j *apijob.Job) error { return j.StartTarget(jobTargetAID) }); err != nil {
		t.Fatalf("AdvanceJob(start A): %v", err)
	}
	if _, err := s.AdvanceJob(ctx, jobID, func(j *apijob.Job) error {
		j.Cancel()
		return nil
	}); err != nil {
		t.Fatalf("AdvanceJob(cancel): %v", err)
	}

	rec := mustGetJob(t, s, jobID)
	got := targetStates(rec)
	if got[jobTargetAID] != apiv1.JobTargetStateRunning {
		t.Fatalf("cancel changed the already-running target to %q; API-114 stops only what has not started", got[jobTargetAID])
	}
	if got[jobTargetBID] != apiv1.JobTargetStateFailed {
		t.Fatalf("cancel left the unstarted target %q, want failed (API-114)", got[jobTargetBID])
	}
	for _, tg := range rec.Job.Targets() {
		if tg.ID == jobTargetBID && !apijob.CodeInRegistry(tg.ErrCode) {
			t.Fatalf("canceled target's persisted code %q is outside api/1's error-code registry (API-115)", tg.ErrCode)
		}
	}
}
