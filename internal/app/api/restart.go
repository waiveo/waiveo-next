package api

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/maaxton/waiveo-next/internal/app/auth"
	"github.com/maaxton/waiveo-next/internal/app/screens"
	"github.com/maaxton/waiveo-next/internal/shared/apihttp"
)

// restart.go serves the one operation that stops this process so a supervisor
// starts a replacement (api/1 API-150-157):
//
//	POST /api/v1/system/restart
//
// # Why the platform owes an operator this control
//
// It already tells them to use it. The first-boot claim's refusals say "restart
// the box to have it present a fresh one" and "if the box was reset, restart it"
// (web/src/routes/setup/setup-route.tsx); a staged restore reports "Restart this
// box to finish the restore" and will not apply until one happens
// (workspacerestore.go, web/src/routes/backup/backup-route.tsx). Until this
// operation existed, every one of those sentences ended at a control that did not
// exist — the remedy was SSH or a power cycle. A product that instructs an
// operator to do something it gives them no way to do has published a dead end.
//
// It is also the floor the extension runtime stands on: an extension that carries
// code may need a fresh process image to load, and a quick app-server restart is
// the sanctioned way to get one. A full appliance reboot is not.
//
// # What "restart" MEANS here, of the three things it could mean
//
// Three mechanisms were available and they are not interchangeable:
//
//	(1) EXIT, and let a supervisor start a replacement.   <- chosen
//	(2) RE-EXEC this process image over itself (execve).
//	(3) RELOAD state in-process without stopping.
//
// (3) is a different feature. It cannot deliver the one thing the caller wants
// from a restart — a fresh process image with freshly-linked code — because the
// running image is exactly what it keeps. Legacy proved that the hard way with
// in-process ESM cache-busting: automation went on running stale code because the
// reload only ever reached the entry point.
//
// (2) is the tempting one, because it works with no supervisor at all, and it was
// rejected on four counts, each sufficient:
//
//   - execve replaces the image WITHOUT unwinding the Go runtime. No deferred
//     close runs, no goroutine mid-write is allowed to finish, and the SQLite
//     handle is inherited by a process that never opened it. A restart whose
//     whole job is to be safe cannot start by skipping the drain.
//   - It is unrecoverable when it fails. If the binary was replaced mid-update,
//     or the new image will not exec, there is no process left to report it —
//     execve does not return on success and the address space is already
//     committed. An exit is recoverable in exactly that case: the supervisor
//     retries, backs off, and the failure is in the journal.
//   - Under systemd it keeps the same PID, so the manager never observes a
//     restart. MainPID, the unit's start timestamp, its watchdog and its restart
//     counter all go on describing a process that is no longer the one running.
//   - It cannot pick up a changed unit file, a changed environment, or a changed
//     working directory — the very things an update to the deployment changes.
//
// So: (1). The stop runs the SAME graceful drain SIGTERM runs (API-157) — one
// path, so a restart cannot be a second, less careful shutdown that quietly
// diverges from the careful one.
//
// # And therefore: no supervisor, no restart. Declared, never sniffed.
//
// (1)'s whole premise is that something starts a replacement. Where nothing will,
// stopping is not a restart — it is an endpoint that takes the box down for good,
// reachable from a button in the console. That is refused, permanently and by
// name: RESTART_UNSUPPORTED (API-154).
//
// Whether a supervisor exists is DECLARED by the deployment (the feeder's
// WAIVEO_FEEDER_SUPERVISOR, set beside `Restart=always` in the same unit file),
// never inferred by the process. Sniffing was considered and is wrong in the
// direction that matters: `INVOCATION_ID` is set for every systemd unit including
// one written `Restart=no`, so the inference is confidently wrong precisely on the
// deployment where being wrong is fatal. Declaring costs one line in the unit that
// already had to say `Restart=always`; guessing costs a box.
//
// A dev run therefore refuses by default, and that is the intended behaviour, not
// a gap: `make dev` starts a bare binary with nothing behind it. The refusal's
// detail names the variable to set.
//
// # What the client can honestly be told (API-152/153)
//
// The answer is 202, never 200 "restarted". The process is still running when the
// response is written — it must be, or the bytes could not be sent — so a 2xx can
// only mean ACCEPTED. The acceptance publishes `started_at_ms`, this process
// instance's own start instant, which GET /system-health returns as well: a client
// holds the "before" and learns the restart completed when a successful health
// read returns a DIFFERENT one. Polling for reachability alone cannot distinguish
// "came back" from "never went", which for a stop that refuses late is the whole
// question.
//
// It also publishes the two windows rather than making a client guess them:
// `stopping_in_ms` (how long this process goes on serving, so the response bytes
// can land) and `drain_budget_ms` (the graceful drain's own bound). Both come from
// the process that will honour them, for the reason the webhook rotation overlap
// does: a published window nothing honours is worse than publishing none.

// restartStoppingInMs is the grace window between the acceptance being written
// and the stop beginning.
//
// It is not a guess at network latency, and it is not tunable. It exists so the
// 202 the handler just wrote reaches the client before the listener stops
// accepting, and it is deliberately generous relative to a LAN round-trip and
// negligible relative to a restart: an operator cannot perceive the difference
// between a box that comes back in 3s and one that comes back in 3.25s, and a
// client that never received its acceptance has to fall back to guessing whether
// anything happened.
const restartStoppingInMs = 250

// RestartOrder is one accepted restart, handed to the process that will perform
// it. It carries who asked, for the process's own log line — the audit record
// itself is written by the api layer's audit middleware (audit.go) before this
// order is ever acted on.
type RestartOrder struct {
	// Actor is the authenticated principal that invoked the operation.
	Actor string
	// TraceID ties the process's shutdown log lines to the request that caused
	// them, which is the correlation an operator reading a journal after the fact
	// has no other way to make.
	TraceID string
}

// RestartConfig wires the restart operation to the process that owns its own
// lifecycle (WithRestart). It is a struct of values and one func rather than an
// interface for the same reason relayCertRevoker is a func field: there is
// exactly one implementation, it lives in package main beside the shutdown path
// it drives, and an interface would only add a name.
type RestartConfig struct {
	// Supervisor is what the DEPLOYMENT declared will start a replacement when
	// this process exits — "systemd", "docker", a wrapper's own name. It is
	// reported in the acceptance so the answer says who is responsible for the
	// process coming back.
	//
	// Empty means none was declared, and the operation refuses. That is the
	// fail-closed default this whole option is arranged around: an unwired
	// deployment cannot restart, rather than restarting into nothing.
	Supervisor string
	// DrainBudgetMs bounds the graceful drain that follows the stop. It comes
	// from the process that actually enforces it, never from a constant here — a
	// budget this surface publishes and the shutdown path does not honour would
	// be a number an operator plans around and the box does not keep.
	DrainBudgetMs int64
	// Arm hands the accepted order to the process and reports whether THIS call
	// is the one that armed it. A second call while a restart is already armed
	// returns false, and the operation refuses RESTART_IN_PROGRESS.
	//
	// The arm/refuse decision lives behind this one call, in the process, rather
	// than in a flag on the api server: there is exactly one process to stop, so
	// exactly one place can decide race-free whether it is already stopping.
	// Splitting that across two components is how two accepted restarts happen.
	Arm func(RestartOrder) bool
}

// WithRestart wires the restart operation's process seam.
//
// Optional, with the degrade-not-404 posture WithPlatformLog and WithScreenStatus
// take: the route mounts either way, authorizes either way, and answers
// RESTART_UNSUPPORTED when this is absent. A 404 would say the operation does not
// exist on this surface, which is false and would send a client looking for a
// different endpoint; the refusal says the operation exists and this deployment
// cannot perform it, which is the true and actionable statement.
func WithRestart(cfg RestartConfig) Option {
	return func(srv *server) { srv.restart = &cfg }
}

// mountRestart registers the operation.
func (srv *server) mountRestart(rt *router) {
	rt.HandleFunc("POST "+apiPrefix+"/system/restart", srv.restartApplicationServer)
}

// restartRequest is the declared body (openapi RestartRequest).
type restartRequest struct {
	Confirm *bool `json:"confirm"`
}

// restartAcceptance is the 202 body (openapi RestartAcceptance).
type restartAcceptance struct {
	AcceptedAtMs  int64  `json:"accepted_at_ms"`
	StoppingInMs  int64  `json:"stopping_in_ms"`
	DrainBudgetMs int64  `json:"drain_budget_ms"`
	StartedAtMs   int64  `json:"started_at_ms"`
	Supervisor    string `json:"supervisor"`
}

// restartApplicationServer handles POST /api/v1/system/restart (openapi
// restartApplicationServer).
//
// It runs under the Idempotency-Key convention for the reason every other
// action-style POST does, and for one this operation feels more sharply than any
// of them: the connection dies as part of the operation succeeding, so a client's
// retry-on-timeout is not an edge case here, it is the EXPECTED case. A keyed
// retry that arrives before the stop replays the original acceptance verbatim; the
// same retry without a key is a second request and meets RESTART_IN_PROGRESS,
// which is also a true answer, just a less useful one.
func (srv *server) restartApplicationServer(w http.ResponseWriter, r *http.Request) {
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	// The declared schema (required `confirm`, boolean, no other members) is
	// enforced from the document itself, before the idempotency record is
	// written — the same ordering runAutomation and the workspace operations use,
	// so a malformed body never burns a key.
	if srv.schemaRejected(w, r, "RestartRequest", body) {
		return
	}
	srv.idempotent(w, r, body, func(w http.ResponseWriter) { srv.restartExec(w, r, body) })
}

// restartExec is the acceptance, executed once per fresh (non-replayed) request.
//
// The ordering below is deliberate and is the same ordering deleteWorkspaceExec
// gives its own reason for: AUTHORIZE FIRST, before anything else is examined.
// Every refusal past this point discloses something about the deployment — that
// it declared no supervisor, that a workspace export is running — and a caller
// who may not restart the box may not learn those either. Refusing them at the
// door means every non-owner gets the same 403 whatever they sent.
func (srv *server) restartExec(w http.ResponseWriter, r *http.Request, body []byte) {
	if _, ok := srv.authorizeWorkspaceOwner(w, r); !ok {
		return
	}

	var req restartRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeProblem(w, r, http.StatusUnprocessableEntity, "VALIDATION_FAILED", "Validation Failed",
			"The request body could not be parsed.")
		return
	}
	// `confirm: false` satisfies the declared schema — it is a boolean — and must
	// not satisfy the operation. The schema bounds the SHAPE; this is the
	// semantic, and it is the difference between a body that is well-formed and
	// one that asked for a restart.
	if req.Confirm == nil || !*req.Confirm {
		writeProblem(w, r, http.StatusUnprocessableEntity, "VALIDATION_FAILED", "Validation Failed",
			"`confirm` must be true; nothing was restarted. This stops the application server, which takes the console and the API away for a few seconds.")
		return
	}

	// API-154. Checked before the in-flight sweep below: a deployment that cannot
	// restart at all should be told that, not told to wait for a job first and
	// then told it could never have restarted anyway.
	if srv.restart == nil || srv.restart.Supervisor == "" || srv.restart.Arm == nil {
		writeProblem(w, r, http.StatusNotImplemented, "RESTART_UNSUPPORTED", "Not Implemented",
			"This process was not started under a supervisor that would start a replacement, so stopping it would take this box down and leave it down. "+
				"Declare one on the unit that starts it — set WAIVEO_FEEDER_SUPERVISOR (systemd: alongside Restart=always) — and restart the service once by hand to pick it up.")
		return
	}

	// API-156. A restart is safe for work whose operation can be re-applied
	// (API-116 resumes it on the next boot) and is not safe for work whose cannot
	// — those targets are reconciled to `failed` instead, so restarting through
	// one converts running work into failed work, and for the irreversible
	// ordered sequence a workspace delete runs (SEC-121) it can leave a
	// half-applied state no later read can describe.
	if blocked, detail := srv.restartBlockedBy(r); blocked {
		writeProblem(w, r, http.StatusConflict, "RESTART_BLOCKED", "Conflict", detail)
		return
	}

	// API-155. Arm is the single race-free decision point: it is the process's
	// own, and it reports false when a restart is already armed.
	armed := srv.restart.Arm(RestartOrder{Actor: auth.PrincipalID(r), TraceID: apihttp.TraceID(r)})
	if !armed {
		writeProblem(w, r, http.StatusConflict, "RESTART_IN_PROGRESS", "Conflict",
			"A restart of this box has already been accepted and is about to happen; nothing further was scheduled. Wait for the box to come back.")
		return
	}

	writeJSONValue(w, http.StatusAccepted, restartAcceptance{
		AcceptedAtMs:  srv.nowMs(),
		StoppingInMs:  restartStoppingInMs,
		DrainBudgetMs: srv.restart.DrainBudgetMs,
		StartedAtMs:   srv.processStartedAtMs(),
		Supervisor:    srv.restart.Supervisor,
	})
}

// restartBlockedBy reports whether accepted work is in flight that a restart
// cannot be resumed from, and the operator-facing sentence naming it.
//
// The inventory is store.InterruptedRuns — the same query the boot-time resume
// takes its work from. That is deliberate reuse rather than a coincidence of
// naming: the query is "which jobs still hold a target in `running`", and the
// executor commits that transition BEFORE attempting the target's write, so at
// runtime it answers exactly "what is executing right now" and after a crash it
// answers "what was executing when the process died". One query, one definition
// of in-flight, and no second notion of it for this operation to disagree with.
//
// Only a run carrying NO re-appliable operation blocks. That is the same
// distinction jobrun.go's resume already makes and it is drawn from the same
// field (store.JobOperation's zero value), so the set that blocks a restart and
// the set a restart would strand cannot drift apart: a bulk-enable is re-applied
// on the next boot and does not block, an export or a delete is not and does.
//
// A read failure does NOT block. The alternative — refusing a restart because the
// store could not be queried — makes the control unusable in exactly the
// circumstance an operator most needs it, which is a box whose store is
// misbehaving. The bias is stated rather than incidental: this check exists to
// protect an irreversible sequence it can SEE, not to stand in front of the
// restart button whenever anything is uncertain.
func (srv *server) restartBlockedBy(r *http.Request) (bool, string) {
	runs, err := srv.store.InterruptedRuns(r.Context())
	if err != nil {
		return false, ""
	}
	var ids []string
	for _, run := range runs {
		if run.Op.Kind == "" {
			ids = append(ids, run.JobID)
		}
	}
	if len(ids) == 0 {
		return false, ""
	}
	// The ids are named because "something is running" is not something an
	// operator can act on. With the id they can poll GET /api/v1/jobs/{id} and
	// see it reach a terminal state, which is a wait with an end they can watch.
	return true, "This box is running work a restart cannot resume — it would be recorded as failed rather than continued, and nothing was restarted. " +
		"Wait for " + strings.Join(ids, ", ") + " to finish (poll /api/v1/jobs/{id}), then restart."
}

// processStartedAtMs is this process instance's start instant, or the
// never-observed sentinel when the deployment publishes none.
//
// It reads the SAME field the health summary reports (SystemHealthConfig), rather
// than a second start time recorded for the restart operation's benefit. That is
// the whole point of the value: a client compares what this acceptance echoed
// against what a later /system-health returns, and two separately-sourced
// "start instants" would make that comparison meaningless the first time they
// disagreed.
func (srv *server) processStartedAtMs() int64 {
	if srv.health == nil || srv.health.StartedAtMs <= 0 {
		return screens.NeverObserved
	}
	return srv.health.StartedAtMs
}
