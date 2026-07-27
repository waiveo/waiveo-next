package api

import (
	"context"
	"encoding/json"
	"errors"
	"sync"

	"github.com/maaxton/waiveo-next/internal/app/store"
	"github.com/maaxton/waiveo-next/internal/rules/compile"
	"github.com/maaxton/waiveo-next/internal/shared/apihttp"
	"github.com/maaxton/waiveo-next/internal/shared/apijob"
)

// jobrun.go is the EXECUTION half of api/1's async convention: the thing that
// makes a 202 + Job (API-111) a promise the surface keeps rather than a shape
// with nothing behind it.
//
// It exists because the accepting handler and the poll handler between them
// only describe work. bulkEnableExec resolves a selector into a target set,
// mints a Job and persists it; getJob serves whatever that record currently
// says. Without a producer of transitions, the record says `pending` forever —
// a client polling per API-112 ("a client determines completion by reading the
// Job resource again and inspecting `state`") never sees completion, because
// completion never happens. This file supplies the missing producer, driving
// the SAME apijob state machine (StartTarget / SucceedTarget / FailTarget) that
// the store persists and reloads.
//
// # Why the execution is asynchronous
//
// API-111 is explicit about the shape: a fleet-mutating operation MUST respond
// 202 "rather than blocking the request until every target finishes". An
// in-request loop would be simpler — no goroutine, no lifecycle, no shutdown
// story, and trivially deterministic — but it is precisely the blocking
// behaviour that sentence forbids, and its 202 would be a lie told about work
// that had already finished. So the accepting request writes the 202 and
// returns; execution runs off the request goroutine, on the JobRunner below.
//
// The determinism an in-request loop would have given away for free is bought
// back explicitly: JobRunner has a lifecycle (Start / Wait / Shutdown), so a
// caller that needs to know when accepted work has finished — a graceful
// shutdown, or a test — asks the runner rather than sleeping and hoping. No
// wall-clock wait appears anywhere in this design.
//
// # Two different writes
//
// Flipping an automation's `enabled` IS a desired-state change, so it goes
// through the ordinary resource update path (store.Update): it bumps the row's
// revision, advances the store generation, and fires the post-commit hook that
// nudges connected relays — exactly as the same edit made one row at a time
// through PATCH does. A relay must learn that a rule was switched off.
//
// A target's PROGRESS through the job is not desired state, so it does not:
// store.AdvanceJob commits per-target transitions without bumping the
// generation, which is what keeps a 200-target job from nudging every relay 200
// times over a snapshot that changed once per target at most.
//
// # Resume after a restart
//
// API-116 requires a restart to resume any target left `running`. The store
// holds both halves that needs: the target inventory AND the operation the job
// accepted (store.InterruptedRuns), persisted in the same transaction as the
// targets it applies to. resumeInterruptedRuns below is the producer — submitted
// to the runner by api.New, so it rides the same lifecycle every other execution
// does and a caller learns it finished by asking (Wait), never by sleeping.
//
// A resumed target is re-applied rather than assumed. A `running` target is one
// the executor committed BEFORE attempting its write (applyBulkEnableTarget), so
// its outcome spans "never started" to "committed but unreported", and no
// process can tell which from the record alone.
//
// # Not double-applying, and not losing a target
//
// Within one process a target is started exactly once: StartTarget refuses a
// non-pending target, and the runner walks each job's target list once.
//
// Across a restart the equivalent guarantee comes from two places. The
// re-entry is guarded — ResumeTarget refuses anything not still `running`,
// evaluated inside AdvanceJob's write transaction — so a target already carried
// to a terminal value is skipped rather than re-run. And where that guard cannot
// help, because the crash landed between a committed write and its unreported
// outcome, the RE-APPLY ITSELF is safe: a bulk-enable's write states an absolute
// intent (`enabled` is this value), shallow-merged over one member, so applying
// it twice lands exactly what applying it once lands. The only cost of a
// redundant re-apply is one more revision on that row — the same cost an
// operator re-issuing the same PATCH would pay — and it cannot corrupt the
// value. That is what makes bulk-enable resumable at all.
//
// An operation that is NOT re-appliable is not resumed and is not left running
// either: its stranded targets are reconciled to a terminal `failed` with a
// registry code (reconcileStrandedTargets). Leaving them `running` would make
// API-112's poll — the only completion signal the contract offers — never
// terminate, which is the silent drop API-116 forbids wearing a different face.

// JobRunner executes accepted api/1 Jobs off the request goroutine, and owns
// their lifecycle: which executions are in flight, when they may begin, and
// when a caller may consider them finished.
//
// A runner is created STOPPED. Work accepted before Start is queued, not
// dropped and not silently executed — so a process wires its collaborators, and
// a test arranges the world a job will run against, before any background write
// begins. api.New builds and starts one of its own when the caller supplies
// none (WithJobRunner), so a handler constructed the ordinary way executes
// accepted work immediately; supplying one is how a caller takes the lifecycle
// back.
//
// The zero value is not usable; use NewJobRunner.
type JobRunner struct {
	mu      sync.Mutex
	started bool
	stopped bool
	queued  []func(context.Context)

	// wg counts every accepted execution — including one still queued — so Wait
	// cannot report completion for work that has not begun.
	wg sync.WaitGroup

	// ctx is the context every execution runs under. It is deliberately NOT any
	// request's context: a request's context is canceled the moment its handler
	// returns, which for a 202 is before the work it accepted has started.
	ctx    context.Context
	cancel context.CancelFunc
}

// NewJobRunner returns a stopped runner. Nothing executes until Start.
func NewJobRunner() *JobRunner {
	ctx, cancel := context.WithCancel(context.Background())
	return &JobRunner{ctx: ctx, cancel: cancel}
}

// Start begins execution: everything accepted while the runner was stopped is
// released, and everything accepted afterwards runs as it arrives. Starting an
// already-started or already-shut-down runner is a no-op.
func (r *JobRunner) Start() {
	r.mu.Lock()
	if r.started || r.stopped {
		r.mu.Unlock()
		return
	}
	r.started = true
	pending := r.queued
	r.queued = nil
	r.mu.Unlock()
	for _, fn := range pending {
		go r.exec(fn)
	}
}

// Wait blocks until every execution accepted so far has finished. It is the
// deterministic completion signal this package offers: a caller learns that
// accepted work is done by asking, never by sleeping.
//
// Wait on a runner that was never started blocks forever, by construction —
// queued work has not finished, and pretending otherwise is the one answer that
// would make Wait useless.
func (r *JobRunner) Wait() { r.wg.Wait() }

// Shutdown stops accepting new work and waits for what is already in flight,
// returning nil once it has drained. Work still queued (never started) is
// abandoned rather than run, which is the same outcome a crash at this instant
// would produce and is why it leaves nothing half-applied: an unstarted job's
// targets are all still `pending`.
//
// If ctx expires first, the execution context is canceled — every in-flight
// execution stops at its next target boundary rather than mid-write — and
// ctx.Err() is returned.
func (r *JobRunner) Shutdown(ctx context.Context) error {
	r.mu.Lock()
	r.stopped = true
	abandoned := r.queued
	r.queued = nil
	r.mu.Unlock()
	for range abandoned {
		r.wg.Done()
	}

	drained := make(chan struct{})
	go func() {
		r.wg.Wait()
		close(drained)
	}()
	select {
	case <-drained:
		r.cancel()
		return nil
	case <-ctx.Done():
		r.cancel()
		return ctx.Err()
	}
}

// submit accepts one execution. After Shutdown it is refused: the Job is
// already persisted with every target `pending`, so refusing here leaves a
// truthful record rather than a half-applied one.
func (r *JobRunner) submit(fn func(context.Context)) {
	r.mu.Lock()
	if r.stopped {
		r.mu.Unlock()
		return
	}
	r.wg.Add(1)
	if !r.started {
		r.queued = append(r.queued, fn)
		r.mu.Unlock()
		return
	}
	r.mu.Unlock()
	go r.exec(fn)
}

func (r *JobRunner) exec(fn func(context.Context)) {
	defer r.wg.Done()
	fn(r.ctx)
}

// WithJobRunner wires the runner accepted Jobs execute on, handing the caller
// the lifecycle: when execution begins (Start), when accepted work is known to
// be finished (Wait), and how it ends (Shutdown). Without it api.New builds a
// runner of its own and starts it immediately, so execution is never silently
// absent — the option changes who owns the lifecycle, never whether the work
// happens.
func WithJobRunner(runner *JobRunner) Option {
	return func(srv *server) {
		if runner != nil {
			srv.jobs = runner
		}
	}
}

// runBulkEnable executes one accepted bulk-enable Job: it walks the job's target
// list in the order the acceptance resolved it (the order API-112 published in
// the 202 body), applying `enabled` to each and recording that target's own
// outcome through the persisted state machine.
//
// Per-target failure isolation is the loop's whole shape: each target's outcome
// is committed on its own, and a failure is recorded and stepped past rather
// than returned. One target that no longer exists must not strand the rest at
// `pending` — that is exactly the mixed outcome API-113's job-level `partial`
// exists to describe, and a loop that aborted on the first failure could never
// produce it.
//
// Targets are NOT re-authorized here. The acceptance already narrowed the set to
// rows the submitting principal may write (bulkEnableExec), and that decision —
// like the scope-node placements the Job records for its own readability — is
// fixed by the world the job was accepted in. Re-deriving authority at execution
// time, off the request and with no principal in hand, would be a second, weaker
// answer to a question already answered correctly.
func (srv *server) runBulkEnable(ctx context.Context, jobID string, targetIDs []string, enabled bool) {
	patch, err := bulkEnablePatch(enabled)
	if err != nil {
		return
	}
	srv.walkBulkEnableTargets(ctx, jobID, targetIDs, patch, (*apijob.Job).StartTarget)
}

// bulkEnablePatch is the one-member shallow-merge patch a bulk-enable applies to
// each target. It is built in ONE place because the resume must re-apply exactly
// what the original run applied — two constructions of "the enable patch" is how
// a resumed job comes to write something subtly different from the job it is
// resuming.
func bulkEnablePatch(enabled bool) (json.RawMessage, error) {
	return json.Marshal(map[string]bool{"enabled": enabled})
}

// walkBulkEnableTargets applies patch to each target in order, claiming each one
// through claim first. claim is what distinguishes a fresh run from a resume —
// StartTarget for a pending target, ResumeTarget for one an interrupted run left
// `running` — and it is the ONLY difference between the two, so a resumed target
// travels the identical write and the identical outcome mapping.
func (srv *server) walkBulkEnableTargets(ctx context.Context, jobID string, targetIDs []string, patch json.RawMessage, claim func(*apijob.Job, string) error) {
	for _, targetID := range targetIDs {
		if ctx.Err() != nil {
			// A shutdown that expired its drain window stops at a target
			// BOUNDARY: whatever is committed stays committed, and the targets
			// not reached are still `pending` (or, on a resume, still `running`
			// and so still in the next process's inventory).
			return
		}
		srv.applyBulkEnableTarget(ctx, jobID, targetID, patch, claim)
	}
}

// applyBulkEnableTarget drives ONE target through API-113's progression: claimed
// (pending -> running on a fresh run, or re-entered on a resume) before the write
// is attempted, then the write's outcome to a terminal value. The `running`
// transition is committed BEFORE the write, not after it, so a crash mid-write
// leaves a target visibly `running` — the state API-116's resume is defined
// over — rather than a `pending` target whose row may already have been changed.
func (srv *server) applyBulkEnableTarget(ctx context.Context, jobID, targetID string, patch json.RawMessage, claim func(*apijob.Job, string) error) {
	if _, err := srv.store.AdvanceJob(ctx, jobID, func(j *apijob.Job) error {
		return claim(j, targetID)
	}); err != nil {
		// The job is gone, or this target is not in the state the claim requires
		// (already canceled, already run, or — on a resume — already carried to a
		// terminal value by something else). Either way there is no legal
		// transition to make and nothing to record: the state machine has refused,
		// verbatim, and the persisted record is unchanged.
		return
	}

	code, detail := srv.applyAutomationEnabled(ctx, targetID, patch)
	_, _ = srv.store.AdvanceJob(ctx, jobID, func(j *apijob.Job) error {
		if code == "" {
			return j.SucceedTarget(targetID)
		}
		return j.FailTarget(targetID, code, detail)
	})
}

// jobOpBulkEnableAutomations names the accepted bulk-enable operation in the
// persisted Job record (store.JobOperation.Kind). The vocabulary lives HERE, in
// the layer that accepts these operations and re-applies them, rather than in
// the store that only carries the bytes: a store that knew this string would
// need editing every time an async operation is added, which is how a durable
// record and its executor drift apart.
//
// The value is part of the on-disk record — a job accepted by one build is
// resumed by the next — so it is a stable name, not a refactorable identifier.
const jobOpBulkEnableAutomations = "automations.bulk-enable"

// bulkEnableOperation is the bulk-enable's persisted arguments: which way
// `enabled` was to be flipped. It is the whole of what a resume needs beyond the
// target list, which is what makes this operation replayable at all.
type bulkEnableOperation struct {
	Enabled bool `json:"enabled"`
}

// strandedDetail is the human-readable reason attributed to a target an
// interrupted run left `running` that cannot be resumed.
const strandedDetail = "The server restarted while this target was running, and the accepted operation cannot be re-applied; the target's outcome is unknown."

// strandedCode is the api/1 registry code (API-115) a stranded, unresumable
// target is failed with.
//
// INTERNAL, not UNAVAILABLE, and the difference is a claim about what happened.
// UNAVAILABLE is what a CANCELED target carries (apijob), and it says something
// definite: this target was never served, so resubmitting it is the whole
// remedy. A target stranded by a restart carries no such claim — its write may
// have landed, may have half-landed, may never have started — and INTERNAL,
// api/1's unclassified server-side failure, is the only value in the registry
// that does not overstate what the server knows. Reporting it as retriable
// would invite a client to re-run work that may already have happened.
const strandedCode = "INTERNAL"

// scheduleResume is API-116's resume, in the two halves it has to be split into.
//
// The inventory is READ SYNCHRONOUSLY, here in the constructor, and the work is
// submitted against that snapshot. The split is load-bearing, not stylistic: the
// resume is defined over targets an EARLIER process left `running`, and the only
// instant at which "still running" means exactly that is before this handler has
// served a request. Reading the inventory later — on the runner, concurrently
// with this process's own accepted jobs — would sweep up a target the current
// process had legitimately just started and reconcile live work as abandoned.
// Nothing in the snapshot can belong to this process, because this process has
// not accepted anything yet.
//
// The work itself still rides the runner, so it shares the lifecycle every other
// execution has: a caller holding the runner learns the resume finished by
// asking (Wait), and a handler built without WithJobRunner still performs it,
// because a process that starts up and silently abandons interrupted work is the
// failure API-116 names.
//
// A read failure is not fatal and is not retried. The inventory is durable and
// undamaged by not being read; the next start reads it again. Failing the
// process, or stalling it, over a resume would trade an unfinished job for an
// unavailable surface.
func (srv *server) scheduleResume() {
	if srv.store == nil {
		return
	}
	runs, err := srv.store.InterruptedRuns(context.Background())
	if err != nil || len(runs) == 0 {
		return
	}
	srv.jobs.submit(func(ctx context.Context) { srv.resumeInterruptedRuns(ctx, runs) })
}

// resumeInterruptedRuns carries every target in the startup snapshot to a
// terminal value — by re-applying the accepted operation where that is safe, and
// by reconciling the target where it is not.
func (srv *server) resumeInterruptedRuns(ctx context.Context, runs []store.InterruptedRun) {
	for _, run := range runs {
		if ctx.Err() != nil {
			return
		}
		srv.resumeRun(ctx, run)
	}
}

// resumeRun carries one interrupted Job's stranded targets to a terminal value.
//
// The switch is exhaustive by construction: an operation this build cannot
// re-apply — one persisted by a build that knew a kind this one does not, or a
// job that deliberately persisted none (the data-subject operations) — falls to
// reconciliation rather than to silence. A resume that quietly skipped what it
// did not recognize would leave exactly the indefinitely-`running` targets this
// whole path exists to remove.
func (srv *server) resumeRun(ctx context.Context, run store.InterruptedRun) {
	switch run.Op.Kind {
	case jobOpBulkEnableAutomations:
		var op bulkEnableOperation
		if err := json.Unmarshal(run.Op.Payload, &op); err != nil {
			// The record says what to do and its arguments are unreadable. That is
			// not a resumable job: re-applying a guessed `enabled` would be a write
			// nobody asked for.
			srv.reconcileStrandedTargets(ctx, run)
			return
		}
		patch, err := bulkEnablePatch(op.Enabled)
		if err != nil {
			srv.reconcileStrandedTargets(ctx, run)
			return
		}
		// Re-applying is safe (see the file header): the write states an absolute
		// intent over one member, so a target whose write already landed before the
		// crash simply lands it again. A target whose automation was DELETED while
		// the process was down is not resurrected either — applyAutomationEnabled
		// finds nothing at that id and the target terminates `failed` NOT_FOUND,
		// which is the same outcome the original run would have produced had it
		// reached that row after the delete.
		srv.walkBulkEnableTargets(ctx, run.JobID, run.TargetIDs, patch, (*apijob.Job).ResumeTarget)
	default:
		srv.reconcileStrandedTargets(ctx, run)
	}
}

// reconcileStrandedTargets fails every target an interrupted run left `running`
// when that run cannot be resumed, with a registry-typed error (API-115).
//
// It is the honest answer to a question the record cannot settle. The
// alternatives are worse in both directions: leaving the targets `running`
// forever makes API-112's poll — the only completion signal a client has —
// never terminate, and reporting them `succeeded` would assert work completed
// that may never have started. `failed` with an INTERNAL code and a detail that
// says the server restarted reports exactly what is known and no more.
//
// Each target is committed on its own, and a refusal is stepped past rather than
// returned: a target another path already carried to a terminal state is simply
// not this function's to change (FailTarget refuses a non-running target), and
// one target's refusal must not strand its siblings.
func (srv *server) reconcileStrandedTargets(ctx context.Context, run store.InterruptedRun) {
	for _, targetID := range run.TargetIDs {
		if ctx.Err() != nil {
			return
		}
		_, _ = srv.store.AdvanceJob(ctx, run.JobID, func(j *apijob.Job) error {
			return j.FailTarget(targetID, strandedCode, strandedDetail)
		})
	}
}

// applyAutomationEnabled performs one target's actual desired-state write and
// returns "" on success, or the api/1 registry error code (API-115) and detail
// the target's failure is typed with.
//
// The write is a read-then-patch at EXECUTION time rather than a write pinned to
// the revision the row held at acceptance. A fleet enable/disable expresses an
// absolute intent ("these automations are off"), and the patch touches exactly
// one member, shallow-merged — so an unrelated concurrent edit to the same row
// is preserved rather than clobbered, while the flag still lands. Pinning the
// acceptance revision instead would fail targets for edits that never touched
// `enabled`, which is a conflict the operation does not actually have.
func (srv *server) applyAutomationEnabled(ctx context.Context, id string, patch json.RawMessage) (code, detail string) {
	res, found, err := srv.store.Get(ctx, store.KindAutomation, id)
	if err != nil {
		return "INTERNAL", "The automation could not be read."
	}
	if !found {
		return "NOT_FOUND", "The automation no longer exists."
	}
	if _, err := srv.store.Update(ctx, store.KindAutomation, id, res.Revision, patch); err != nil {
		return targetFailure(err)
	}
	return "", ""
}

// targetFailure maps a store write error onto the api/1 error-code registry
// (API-014/115): a per-target failure is diagnosable exactly like any other
// api/1 error, never with a parallel vocabulary of the job system's own.
//
// The classification deliberately mirrors writeStoreError's (api.go), which maps
// the same store errors onto a Problem for a synchronous write — the same fault
// reported through a Job and through a 4xx must not be typed differently
// depending on which door it came through. It cannot literally reuse that
// function: writeStoreError writes an HTTP response, and there is no response
// here to write.
// The result is checked against the registry before it is returned rather than
// trusted: apijob.FailTarget refuses a code outside the closed set, and a
// refused transition would leave the target stuck `running` — a mapping bug
// would then present as a job that never terminates, which is far worse than a
// fault reported as INTERNAL.
func targetFailure(err error) (code, detail string) {
	code, detail = classifyStoreFailure(err)
	if !apijob.CodeInRegistry(code) {
		return "INTERNAL", detail
	}
	return code, detail
}

func classifyStoreFailure(err error) (code, detail string) {
	var xerr *apihttp.ExternalIDError
	if errors.As(err, &xerr) {
		return xerr.Code, xerr.Detail
	}
	var cerr *compile.CompileError
	if errors.As(err, &cerr) {
		return "VALIDATION_FAILED", cerr.Message
	}
	var verr *store.ValidationError
	if errors.As(err, &verr) {
		return "VALIDATION_FAILED", "One or more fields failed validation."
	}
	var rme *store.RevisionMismatchError
	if errors.As(err, &rme) {
		return "REVISION_CONFLICT", "The automation was modified concurrently."
	}
	if errors.Is(err, store.ErrNotFound) {
		return "NOT_FOUND", "The automation no longer exists."
	}
	return "INTERNAL", "The automation could not be updated."
}
