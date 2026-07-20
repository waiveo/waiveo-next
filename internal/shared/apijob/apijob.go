// Package apijob is the shared realization of api/1's Job resource state
// machine (API-110-117): the accepted-work resource a fleet-mutating operation
// (or any operation that cannot finish within its own request/response cycle)
// returns with 202 Accepted, and the closed pending -> running -> terminal
// progression its own state and each of its targets' states follow.
//
// It exists so every async api/1 operation — a fleet enable/disable over a
// selector-matched set (API-110/111), a data-subject workspace export or
// delete (API-120-123) — builds and advances the SAME Job shape and the SAME
// state rules, rather than each handler re-deriving "is this job partial?" or
// "what does cancel do to a target already running?". The wire shape itself is
// the generated apiv1.Job (API-112): Resource renders to it, so the JSON a
// client polls is exactly the contract's model, never a hand-rolled parallel.
//
// A per-target failure is typed with a code from api/1's own error-code
// registry (API-014/115), validated here against that closed set — never a
// parallel vocabulary — so a Job's failed targets are diagnosable the same way
// any other api/1 error is.
package apijob

import (
	"fmt"
	"time"

	apiv1 "github.com/maaxton/waiveo-next/api/gen/go"
)

// registryCodes is api/1's closed error-code registry (contracts/api-1.md
// "Error taxonomy", API-011): the only values a per-target failure MAY be typed
// with (API-115). It mirrors that table — the contract, not this list, is the
// source of truth; this is the code-side gate that keeps a per-target failure
// from inventing a code outside the registry.
var registryCodes = map[string]bool{
	"VALIDATION_FAILED":           true,
	"UNAUTHENTICATED":             true,
	"FORBIDDEN":                   true,
	"NOT_FOUND":                   true,
	"REVISION_CONFLICT":           true,
	"IF_MATCH_REQUIRED":           true,
	"CURSOR_INVALID":              true,
	"SELECTOR_INVALID":            true,
	"IDEMPOTENCY_KEY_REUSED":      true,
	"IDEMPOTENCY_KEY_IN_PROGRESS": true,
	"EXTERNAL_ID_CONFLICT":        true,
	"RATE_LIMITED":                true,
	"INTERNAL":                    true,
	"UNAVAILABLE":                 true,
}

// CodeInRegistry reports whether code is a member of api/1's closed error-code
// registry (API-011). A per-target failure code (API-115) MUST satisfy this.
func CodeInRegistry(code string) bool { return registryCodes[code] }

// cancelCode is the registry code a cancellation attributes to a target it
// stops before it starts (API-114). The registry has no dedicated "canceled"
// value, so UNAVAILABLE — "temporarily unable to serve the request", retriable
// — is the closest closed-registry fit (API-115): the target was not served
// because the job was canceled, and a client MAY resubmit it under a new job.
const cancelCode = "UNAVAILABLE"

// cancelDetail is the human-readable reason a cancellation attributes to a
// target it fails (API-114 "a cancellation-attributed error").
const cancelDetail = "target was canceled before it started."

// Target is one resource a Job acts on, plus its own per-target progress and —
// once failed — the registry-typed error attributed to it (API-112/115). The
// error fields are empty for any non-failed state.
type Target struct {
	ID        string
	State     apiv1.JobTargetState
	ErrCode   string
	ErrDetail string
}

// Job is the api/1 Job resource with its state machine attached: identity and
// submitter metadata (API-112), an ordered target set, and the closed
// pending -> running -> terminal progression (API-113). The job-level state is
// always derived from the targets, never stored independently, so it cannot
// drift out of agreement with them.
type Job struct {
	ID        string
	CreatedBy string
	CreatedAt time.Time
	targets   []*Target
	index     map[string]*Target
}

// New builds a freshly-accepted Job (API-111/112): one pending target per id in
// targetIDs, in the order given, and — since every target is pending — a
// job-level state of pending. The caller supplies id (the Job's own ULID),
// createdBy (the submitting principal), createdAt, and the target ids (each a
// target's own id or external_id, API-103), exactly as the accepting handler
// resolved them. Duplicate target ids are collapsed to the first occurrence, so
// the index is unambiguous.
func New(id, createdBy string, createdAt time.Time, targetIDs []string) *Job {
	j := &Job{
		ID:        id,
		CreatedBy: createdBy,
		CreatedAt: createdAt,
		index:     make(map[string]*Target, len(targetIDs)),
	}
	for _, tid := range targetIDs {
		if _, seen := j.index[tid]; seen {
			continue
		}
		t := &Target{ID: tid, State: apiv1.JobTargetStatePending}
		j.targets = append(j.targets, t)
		j.index[tid] = t
	}
	return j
}

// Targets returns a snapshot copy of the job's targets in order, so a caller
// cannot mutate the job's internal state through the returned slice.
func (j *Job) Targets() []Target {
	out := make([]Target, len(j.targets))
	for i, t := range j.targets {
		out[i] = *t
	}
	return out
}

// State derives the job-level state from its targets (API-113): pending while
// every target is still pending, terminal once every target is terminal
// (succeeded if all succeeded, failed if all failed, partial when the outcomes
// are mixed — partial is job-level only), and running in between. A job with no
// targets is treated as pending (there is no work to have started).
func (j *Job) State() apiv1.JobState {
	total := len(j.targets)
	if total == 0 {
		return apiv1.JobStatePending
	}
	var pending, succeeded, failed int
	for _, t := range j.targets {
		switch t.State {
		case apiv1.JobTargetStatePending:
			pending++
		case apiv1.JobTargetStateSucceeded:
			succeeded++
		case apiv1.JobTargetStateFailed:
			failed++
		}
	}
	if pending == total {
		return apiv1.JobStatePending
	}
	if succeeded+failed == total { // every target terminal
		switch {
		case succeeded == total:
			return apiv1.JobStateSucceeded
		case failed == total:
			return apiv1.JobStateFailed
		default:
			return apiv1.JobStatePartial
		}
	}
	return apiv1.JobStateRunning
}

// StartTarget advances a pending target to running (API-113). It is an error to
// start an unknown target or one not currently pending — the pending -> running
// step is the only legal entry into execution.
func (j *Job) StartTarget(id string) error {
	t, ok := j.index[id]
	if !ok {
		return fmt.Errorf("apijob: no such target %q", id)
	}
	if t.State != apiv1.JobTargetStatePending {
		return fmt.Errorf("apijob: target %q is %s, not pending — cannot start", id, t.State)
	}
	t.State = apiv1.JobTargetStateRunning
	return nil
}

// SucceedTarget advances a running target to its terminal succeeded state
// (API-113). A target must be running first — a pending target has to be
// started before it can reach a terminal value.
func (j *Job) SucceedTarget(id string) error {
	t, ok := j.index[id]
	if !ok {
		return fmt.Errorf("apijob: no such target %q", id)
	}
	if t.State != apiv1.JobTargetStateRunning {
		return fmt.Errorf("apijob: target %q is %s, not running — cannot succeed", id, t.State)
	}
	t.State = apiv1.JobTargetStateSucceeded
	t.ErrCode = ""
	t.ErrDetail = ""
	return nil
}

// FailTarget advances a running target to its terminal failed state, attributing
// a registry-typed error (API-113/115). code MUST be a value from api/1's own
// error-code registry (CodeInRegistry) — a per-target failure never invents a
// parallel vocabulary. A target must be running first.
func (j *Job) FailTarget(id, code, detail string) error {
	if !CodeInRegistry(code) {
		return fmt.Errorf("apijob: per-target failure code %q is not in the api/1 error-code registry (API-115)", code)
	}
	t, ok := j.index[id]
	if !ok {
		return fmt.Errorf("apijob: no such target %q", id)
	}
	if t.State != apiv1.JobTargetStateRunning {
		return fmt.Errorf("apijob: target %q is %s, not running — cannot fail", id, t.State)
	}
	t.State = apiv1.JobTargetStateFailed
	t.ErrCode = code
	t.ErrDetail = detail
	return nil
}

// Cancel applies API-114 and returns the job's resulting state. A job already
// in a terminal state is a no-op — its current state is returned unchanged,
// never an error. Otherwise every target not yet started (still pending) is
// marked failed with a cancellation-attributed registry error; a target that
// already reached a terminal state is NOT rolled back, and a target already
// running is left to finish (cancel stops only what has not started).
func (j *Job) Cancel() apiv1.JobState {
	if isTerminal(j.State()) {
		return j.State()
	}
	for _, t := range j.targets {
		if t.State == apiv1.JobTargetStatePending {
			t.State = apiv1.JobTargetStateFailed
			t.ErrCode = cancelCode
			t.ErrDetail = cancelDetail
		}
	}
	return j.State()
}

// Resource renders the Job to the generated apiv1.Job wire model (API-112): the
// exact id / targets[{target_id, state}] / created_by / state / created_at
// shape a client polls, with the job-level state freshly derived. Because it
// returns the generated type, the JSON member names are the contract's, not a
// hand-rolled parallel.
func (j *Job) Resource() apiv1.Job {
	targets := make([]apiv1.JobTarget, len(j.targets))
	for i, t := range j.targets {
		targets[i] = apiv1.JobTarget{TargetId: t.ID, State: t.State}
	}
	return apiv1.Job{
		Id:        j.ID,
		CreatedBy: j.CreatedBy,
		CreatedAt: j.CreatedAt,
		State:     j.State(),
		Targets:   targets,
	}
}

// isTerminal reports whether a job-level state is terminal (API-113):
// succeeded, failed, or partial.
func isTerminal(s apiv1.JobState) bool {
	switch s {
	case apiv1.JobStateSucceeded, apiv1.JobStateFailed, apiv1.JobStatePartial:
		return true
	default:
		return false
	}
}
