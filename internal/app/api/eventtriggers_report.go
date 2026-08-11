package api

import (
	"encoding/json"
	"log"

	"github.com/maaxton/waiveo-next/internal/events"
)

// eventtriggers_report.go makes an event-fired run's outcome — above all its
// PER-TARGET REFUSALS — something an operator can read.
//
// # The gap this closes
//
// A hand-initiated run answers its caller: POST /automations/{id}/run returns
// the whole effect report, target by target, refusals included
// (automations_exec.go's runReport). An EVENT-fired run has no caller. Its
// report was assembled identically and then dropped on the floor, with the only
// trace a log.Printf whose counts did not even separate an OK target from a
// refused one. So the failure mode was:
//
//	a viewer presses a button → the rule fires → a target is refused →
//	the screen does not change → there is nothing anywhere to read
//
// Which is the same "accepts work it never performs" shape the scope bound
// itself was added to close, displaced one layer: the refusal is now correct and
// still invisible. A silent refusal is not acceptable even with authoring
// tightened (automationtargets.go), because placement changes after authoring
// and the run is then the only thing that knows.
//
// # Why automation.run, and not something new
//
// events/1 EVT-040 already publishes exactly this record — one rule evaluation's
// outcome, with `mode_disposition` and `action_outcomes` — and EVT-042 already
// distinguishes an app-side evaluation (`origin: internal`) from the relay's
// edge one (`origin: relay`). The relay has emitted it for its edge rules since
// wave 1; the app peer emitted it for nothing at all, so the same durable feed
// an operator watches for "did my rule run" was answering only for half the
// rules. Adding a channel would have been adding a second answer to a question
// the catalog already answers.
//
// It rides the SAME event log the ingest appends to and the /events/v1 SSE
// server serves, so the press and the run it caused sit next to each other in
// one feed, correlated by the trace id the ingest propagated (EVT-010, API-063).
//
// # Why the fired path and not the manual one
//
// A manual run's report reaches the operator who asked for it, synchronously, in
// the response. Emitting a durable event for it as well would double the record
// of an outcome already delivered. The asymmetry is the point: this exists for
// the run NOBODY is holding a response to.

// RunEventSink is the events/1 append boundary an event-fired run publishes its
// automation.run record to. It is the same one-method shape every other
// publisher in this tree uses (auth.EventSink, eventingest.EventSink), satisfied
// by *events.EventLog directly and by the eventsse.Hub in production — so a
// published run wakes a live /events/v1 subscriber with no separate wiring.
type RunEventSink interface {
	Append(events.Envelope)
}

// WithRunEvents publishes every EVENT-FIRED automation run to sink as an
// events/1 `automation.run` record (EVT-040/042).
//
// Omitting it leaves an event-fired run reported only in this process's log.
// That is a legitimate state for a bare handler in a test, and it is NOT a
// legitimate state for a deployment: without it the only evidence a viewer's
// press was refused is a log line on the box. cmd/waiveo-feeder passes the same
// hub the ingest appends to.
func WithRunEvents(sink RunEventSink) Option {
	return func(srv *server) {
		if sink == nil {
			return
		}
		srv.runEvents = sink
	}
}

// WithVariableEvents publishes every committed variable write to sink as an
// events/1 `variable.changed` record (EVT-084/085, data-model/1 DAT-137).
//
// It is a separate option from WithRunEvents rather than a second use of the
// same field because the two publish different schemas for different reasons,
// and a deployment must be able to see, at the call site, that it wired both. A
// deployment passes the same hub to each.
//
// Omitting it leaves a variable write unrecorded. That is a legitimate state for
// a bare handler in a test, and it is NOT a legitimate state for a deployment:
// `variable.changed` is the durable event an `event`-kind trigger fires a rule
// on (RUL-080/EVT-085), so without it a rule authored to run WHEN a variable
// changes never runs — and it carries old_value, which is the only surviving
// record of what a variable used to be.
func WithVariableEvents(sink RunEventSink) Option {
	return func(srv *server) {
		if sink == nil {
			return
		}
		srv.variableEvents = sink
	}
}

// publishRunReport records one event-fired run as an events/1 automation.run
// (EVT-040), and logs a line that separates the outcomes rather than counting
// them together.
//
// scopeNode is the AUTOMATION's placement, not the triggering event's: the
// record is about the rule, and placing it at the site the event carried would
// hide it from a subscriber scoped to the node the rule actually lives at
// (EVT-120's visible set is decided by scope_node alone).
func (srv *server) publishRunReport(ruleID, scopeNode string, revision int64, env events.Envelope, rep runReport) {
	okTargets, failedTargets := countRunTargets(rep)
	log.Printf("event-trigger: %s fired automation %s at %s (%s): %d target(s) performed, %d refused or failed, %d command(s), %d signage action(s)",
		env.Schema, ruleID, scopeNode, rep.Disposition, okTargets, failedTargets, len(rep.Commands), len(rep.Signage))

	if srv.runEvents == nil {
		return
	}
	outcomes, err := json.Marshal(runActionOutcomes(rep))
	if err != nil {
		log.Printf("event-trigger: encoding the run report for automation %s: %v", ruleID, err)
		return
	}
	// The trigger snapshot is what FIRED the rule, which for an `event` trigger
	// is the durable event itself — named by id and schema so a reader can find
	// the press this run answered, without copying a payload the log already
	// holds verbatim one record earlier.
	trigger, err := json.Marshal(map[string]any{
		"type":     triggerTypeEvent,
		"event":    env.Schema,
		"event_id": env.ID,
	})
	if err != nil {
		log.Printf("event-trigger: encoding the trigger snapshot for automation %s: %v", ruleID, err)
		return
	}
	srv.runEvents.Append(events.AutomationRunEnvelope(
		scopeNode, env.TraceID, ruleID, int(revision),
		trigger,
		// No per-condition results: a fired rule's conditions are evaluated as
		// RUL-004's implicit AND and the evaluator reports the verdict, not the
		// per-leaf trace. An empty array is the honest encoding of "none
		// recorded" and EVT-040 admits it; a fabricated one would be worse.
		json.RawMessage(`[]`),
		outcomes,
		rep.Disposition,
		false, // misfire_caught: an event-fired run is never a caught misfire (RUL-246)
		"internal",
		srv.newID(), srv.nowMs(),
	))
}

// runActionOutcome is one entry of the automation.run payload's
// `action_outcomes` array: what the run tried, against which target, and whether
// it happened. `error` carries the per-target refusal verbatim — including "not
// authorized to write a screen placed at this scope node", which is the whole
// reason this record exists.
type runActionOutcome struct {
	Action   string `json:"action"`
	Target   string `json:"target,omitempty"`
	EntityID string `json:"entity_id,omitempty"`
	ScreenID string `json:"screen_id,omitempty"`
	OK       bool   `json:"ok"`
	Error    string `json:"error,omitempty"`
}

// runActionOutcomes flattens a runReport into one entry per TARGET, because a
// target is the unit that succeeds or is refused: a signage action naming three
// screens, one of which the rule may not write, is one action and three
// outcomes, and collapsing it to one would report the run as "failed" or
// "complete" when it was neither.
//
// It always returns a non-nil slice so the payload's required `action_outcomes`
// member marshals as `[]` rather than `null` (EVT-040 requires an array).
func runActionOutcomes(rep runReport) []runActionOutcome {
	out := make([]runActionOutcome, 0, len(rep.Commands)+len(rep.Signage)+len(rep.Variables))
	for _, c := range rep.Commands {
		out = append(out, runActionOutcome{
			Action: "device_command", Target: c.Command, EntityID: c.EntityID, OK: c.OK, Error: c.Error,
		})
	}
	for _, s := range rep.Signage {
		if len(s.Screens) == 0 {
			// An action that failed AS AN ACTION (an unresolvable ScreenRef, a
			// cast_id naming nothing) has no screen to attribute — reported once,
			// with the action's own error, rather than dropped for want of a
			// target field to fill.
			out = append(out, runActionOutcome{Action: s.Action, OK: s.Error == "" && s.Outcome != outcomeFailed, Error: s.Error})
			continue
		}
		for _, sc := range s.Screens {
			out = append(out, runActionOutcome{Action: s.Action, ScreenID: sc.ScreenID, OK: sc.OK, Error: sc.Error})
		}
	}
	for _, v := range rep.Variables {
		// A variable write's "target" is the variable NAME, carried in Target
		// rather than in EntityID or ScreenID: it names neither an entity nor a
		// screen, and putting a variable name in a member the schema declares as
		// a ULID is the same defect SignageOutcome.Error's own doc describes.
		//
		// It is included for the reason the two loops above exist at all: an
		// event-fired run has no caller to answer, so this record is the ONLY
		// place its refusals can be read. A rule firing on a screen press to flip
		// `guest_mode` and being refused would otherwise be a variable that did
		// not change and nothing anywhere saying why.
		out = append(out, runActionOutcome{
			Action: v.Action, Target: v.Variable, OK: v.OK, Error: v.Error,
		})
	}
	return out
}

// outcomeFailed is eval's own three-value action outcome for "nothing happened"
// (RUL-172). Spelled here rather than exported from eval for the same reason
// triggerTypeEvent is: one literal, read in one place.
const outcomeFailed = "failed"

// countRunTargets separates the targets a run PERFORMED from the ones it refused
// or failed on.
//
// The log line this feeds used to report `%d command(s), %d signage action(s)`,
// which counts attempts and says nothing about outcomes: a run that refused
// every target and a run that performed every one printed the same numbers. An
// operator reading that line to find out why a press did nothing learned that
// something was attempted.
func countRunTargets(rep runReport) (ok, failed int) {
	for _, o := range runActionOutcomes(rep) {
		if o.OK {
			ok++
			continue
		}
		failed++
	}
	return ok, failed
}
