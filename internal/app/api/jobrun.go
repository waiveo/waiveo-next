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
// # Resume after a restart is NOT implemented here, and that is deliberate
//
// API-116 requires a restart to resume any target left `running`. The store
// already holds the inventory that would be resumed from (store.RunningTargets)
// and never rolls a committed target state back. What is missing is not a
// scan — it is the INTENT: the persisted Job record says which rows the work
// acts on, and says nothing about what to do to them. Nothing durable records
// that this job was a bulk-enable, or which way `enabled` was to be flipped, so
// a resuming process has the target list and no operation to re-apply to it.
// Persisting the operation and its payload alongside the Job is the piece that
// unlocks resume; until it lands, a process that dies mid-run leaves those
// targets `running` and a poll reports that honestly. Stated here rather than
// implied by silence: this change does not resume, at startup or on poll.
//
// Within a single process the weaker guarantee does hold — a target is never
// double-applied, because a target is started exactly once (StartTarget refuses
// a non-pending target) and the runner walks each job's target list once.

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
	patch, err := json.Marshal(map[string]bool{"enabled": enabled})
	if err != nil {
		return
	}
	for _, targetID := range targetIDs {
		if ctx.Err() != nil {
			// A shutdown that expired its drain window stops at a target
			// BOUNDARY: whatever is committed stays committed, and the targets
			// not reached are still `pending`.
			return
		}
		srv.applyBulkEnableTarget(ctx, jobID, targetID, patch)
	}
}

// applyBulkEnableTarget drives ONE target through API-113's progression:
// pending -> running before the write is attempted, then the write's outcome to
// a terminal value. The `running` transition is committed BEFORE the write, not
// after it, so a crash mid-write leaves a target visibly `running` — the state
// API-116's resume is defined over — rather than a `pending` target whose row
// may already have been changed.
func (srv *server) applyBulkEnableTarget(ctx context.Context, jobID, targetID string, patch json.RawMessage) {
	if _, err := srv.store.AdvanceJob(ctx, jobID, func(j *apijob.Job) error {
		return j.StartTarget(targetID)
	}); err != nil {
		// The job is gone, or this target is not pending (already canceled, or
		// already run). Either way there is no legal transition to make and
		// nothing to record — the state machine has refused, verbatim, and the
		// persisted record is unchanged.
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
