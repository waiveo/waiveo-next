package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/maaxton/waiveo-next/internal/app/store"
	"github.com/maaxton/waiveo-next/internal/datamodel"
	"github.com/maaxton/waiveo-next/internal/deviceclass"
	"github.com/maaxton/waiveo-next/internal/rules/eval"
	"github.com/maaxton/waiveo-next/internal/rules/model"
	"github.com/maaxton/waiveo-next/internal/rules/registry"
	"github.com/maaxton/waiveo-next/internal/rules/schedule"
	"github.com/maaxton/waiveo-next/internal/rules/state"
	"github.com/maaxton/waiveo-next/internal/shared/apiselector"
	"github.com/maaxton/waiveo-next/internal/shared/ulid"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// automations_exec.go is the app peer's REAL execution of one automation's
// action sequence — what `POST /automations/{id}/run` performs.
//
// # What it replaced, and why the replacement is the whole point
//
// Run-now used to drive the rule through a throwaway engine wired to a `nopSink`
// and return the resulting mode disposition. Every gate was green: the route was
// declared, authorized, idempotent, schema-checked, and covered by tests. It
// also did nothing. An operator pressing "Run now" on a rule that turns the
// lobby TVs on got a 200 with `disposition: "ran"` and an unchanged lobby.
//
// That is this codebase's recurring defect in its purest form — a surface that
// accepts work it never performs — and it is why this file exists as the
// EXECUTION side rather than as a bigger simulation. Nothing here is a stand-in:
// a `device_command` travels the same relay connection `POST
// /entities/{id}/commands` uses, and a signage action writes the same screen
// rows an operator's own edit writes, through the same store, advancing the same
// desired-state generation, reaching a screen through the same projection.
//
// # Why the actions are run directly rather than through the edge engine
//
// The edge engine (internal/rules/engine) exists to hold state ACROSS firings:
// per-trigger baselines, `for`-holds, in-flight runs, generation swaps. A manual
// run has none of that history by construction — it is one firing, initiated by
// a human, against a rule that may have no state trigger to synthesize at all
// (a `time` trigger, a `sun` trigger, or a rule that is only ever run by hand).
// Driving it through an engine forced a synthetic device observation into
// existence to justify the firing, which is how the previous implementation
// ended up refusing to run any rule whose triggers it could not fake.
//
// So this evaluates the two things a manual firing genuinely has — the rule's
// conditions (RUL-004's implicit AND, evaluated exactly as the engine evaluates
// them, via eval.EvalConditionAt) and its action sequence (eval.RunActions,
// unchanged) — and reports the mode disposition a firing with no concurrent run
// resolves to (RUL-241/246: `ran`). Nothing about mode evaluation is
// re-implemented, because there is nothing to evaluate: a manual run cannot
// collide with itself.

// runReport is everything one manual run did, and it is deliberately assembled
// as it happens rather than inferred afterwards: an operator needs to tell "it
// ran" from "it ran and changed something", and the only honest source for the
// second is the sinks themselves.
// It carries no JSON tags on purpose: the WIRE shape is automationRunResponse
// (automations.go), which the openapi AutomationRunResult declares. Two structs
// that both look serializable is how a response quietly starts being rendered
// from the wrong one.
type runReport struct {
	Disposition     string
	DryRun          bool
	Commands        []runCommand
	Signage         []eval.SignageOutcome
	Variables       []eval.VariableOutcome
	PackActions     []eval.PackActionOutcome
	Logs            []runLog
	DelaysCollapsed int
}

// runCommand is one device command this run dispatched (openapi
// AutomationRunCommand).
type runCommand struct {
	EntityID string `json:"entity_id"`
	Command  string `json:"command"`
	OK       bool   `json:"ok"`
	Error    string `json:"error,omitempty"`
}

// runLog is one `log` action's evaluated output (RUL-200).
type runLog struct {
	Level   string `json:"level"`
	Message string `json:"message"`
}

// maxCollapsedDelays bounds how many times a manual run will resume past a
// `delay` action before it stops. A rule cannot loop — `choose` may not recurse
// into itself (RUL-180) and the action sequence is finite — so this can only be
// reached by a rule with an absurd number of delays; it is a guard against a
// pathological authored rule holding a request open, not against a cycle.
const maxCollapsedDelays = 64

// runAutomationNow evaluates one automation's conditions and, when they hold,
// runs its actions for real (or, under dryRun, resolves every target and
// withholds every effect).
//
// view is the CALLER's scope view, and it is carried all the way into both sinks
// rather than consulted once here. That is deliberate: a run is authorized at
// the AUTOMATION's own scope node (the caller may run this rule), which says
// nothing about the entities and screens the rule's actions happen to name. A
// rule authored at a node the caller can write may target a screen at a node
// they cannot, and a run that quietly performed it would make the automation
// family a way around placement authorization. Each target is therefore checked
// on its own and, when refused, reported as a failed target — never silently
// dropped, and never allowed to fail the whole run (RUL-171/236's independence
// rule, which is the same rule for the same reason).
// scopeNode is the AUTOMATION ROW's own placement (DAT-006). It is a separate
// argument from view even though view is derived from it on the event-fired
// path, because the two answer different questions and a run needs both: view
// bounds what this run may touch, while scopeNode is the concrete node the
// variable environment resolves at (DAT-134) and the node a `variable_write`
// declares its row at. Deriving one from the other is not possible in either
// direction — a scopeView is a pair of predicates, and a manual run's view comes
// from the CALLER's bindings rather than from the rule's placement at all.
func (srv *server) runAutomationNow(ctx context.Context, traceID string, rule model.Rule, view scopeView, scopeNode string, dryRun bool) runReport {
	rep := runReport{
		DryRun:      dryRun,
		Commands:    []runCommand{},
		Signage:     []eval.SignageOutcome{},
		Logs:        []runLog{},
		Variables:   []eval.VariableOutcome{},
		PackActions: []eval.PackActionOutcome{},
	}

	reg := registry.FromDeviceClass(deviceclass.Builtin(), registry.Overlay{})
	snap := srv.entitySnapshot()
	clk := appClock{nowMs: srv.nowMs}

	// The variable environment a `variable` condition reads (RUL-150), resolved
	// at the rule's own scope node by DAT-134's nearest-ancestor walk, per name
	// (DAT-135).
	//
	// This used to be `map[string]any{}` — an empty map, with a comment saying a
	// manual run "is not a compiled generation, so there is no closure to read
	// from". That premise was true and the conclusion did not follow: the reason
	// RUL-150 closes a variable over at compile time is that the EDGE engine
	// holds no variable store, and the app peer does. Reading it live here is
	// what the edge engine is forbidden from doing precisely because only this
	// side can. With the empty map, a rule guarded by a `variable` condition
	// evaluated false forever and reported `skipped` — a control the builder
	// offers that can never be satisfied.
	//
	// A read failure yields an EMPTY environment rather than aborting the run,
	// which keeps the fail-closed direction RUL-150 already fixes for an absent
	// variable: a condition that cannot resolve its variable does not pass.
	vars, verr := srv.store.EffectiveVariables(ctx, scopeNode)
	if verr != nil {
		log.Printf("api: automation run %s: variable environment unreadable, evaluating with none: %v", traceID, verr)
		vars = map[string]any{}
	}

	// RUL-004: the conditions array is an implicit AND, and a rule whose
	// conditions do not hold does not run its actions. That is `skipped` — the
	// rule's own declared guard doing its job — not a client error, which is
	// what the previous implementation reported it as (a 400 that told an
	// operator their request was malformed when in fact their rule said "not
	// now").
	for _, c := range rule.Conditions {
		if !eval.EvalConditionAt(reg, snap, clk, defaultRunLocation(), vars, c) {
			rep.Disposition = "skipped"
			return rep
		}
	}
	rep.Disposition = "ran"

	cmds := &relayCommandSink{srv: srv, ctx: ctx, traceID: traceID, view: view, dry: dryRun}
	sign := &screenOverrideSink{srv: srv, ctx: ctx, view: view, dry: dryRun}
	varsink := &variableWriteSink{srv: srv, ctx: ctx, traceID: traceID, view: view, node: scopeNode, dry: dryRun}
	packsink := &packActionSink{srv: srv, ctx: ctx, traceID: traceID, dry: dryRun}

	var logs []eval.LogEntry
	var rejected []eval.CommandResult
	actx := eval.ActionContext{
		Reg:  reg,
		Snap: snap,
		Clk:  clk,
		Loc:  defaultRunLocation(),
		// The SAME environment the conditions were evaluated against, not a
		// fresh empty one. A rule whose `choose` branch is guarded by the very
		// variable its top-level condition tested must see one answer, and
		// handing the guard an empty map here while the condition read the real
		// values is how one rule text gets two readings in one run.
		Vars:            vars,
		Sink:            cmds,
		Resolve:         srv.resolveEntityRef(view),
		Signage:         sign,
		SignageOutcomes: &rep.Signage,
		// The RUL-220 write seam. Nil on the edge engine; here it is the whole
		// point — a `variable_write` used to fall through RunActions' default arm
		// and do nothing at all.
		Variables:        varsink,
		VariableOutcomes: &rep.Variables,
		// The RUL-231 seam. Nil on the edge engine (a rule containing one is
		// app-classified, so the relay is never handed it); here it routes by the
		// named pack's manifest.
		PackActions:        packsink,
		PackActionOutcomes: &rep.PackActions,
		Logs:               &logs,
		// Pre-dispatch refusals — an entity this app peer has never been told
		// about, or a command outside its device class's vocabulary — are
		// REPORTED rather than absorbed. Without this the single most likely
		// mistake an operator makes (a rule pointing at a device that was never
		// adopted) came back as a successful run with an empty command list.
		Rejected: &rejected,
	}

	// A `delay` in a manual run is COLLAPSED, not slept through: RunActions
	// hands back the remaining actions and the instant they would resume at, and
	// this resumes them immediately. Holding an HTTP request open for a rule's
	// own pacing would turn a five-minute delay into a five-minute request, and
	// backgrounding it would make the response's effect report a lie. Collapsing
	// is the third option, and the only one that keeps the report true — so it
	// is COUNTED and reported (openapi AutomationRunResult.delays_collapsed),
	// because a rule whose timing matters behaves differently here than it does
	// on a trigger and the operator has to be able to see that.
	actions := rule.Actions
	for i := 0; i < maxCollapsedDelays; i++ {
		err := eval.RunActions(actx, actions)
		dp, pending := err.(*eval.DelayPending)
		if !pending {
			break
		}
		rep.DelaysCollapsed++
		actions = dp.RemainingActions
		if len(actions) == 0 {
			break
		}
	}

	rep.Commands = cmds.results
	for _, rj := range rejected {
		rep.Commands = append(rep.Commands, runCommand{
			EntityID: rj.Target, Command: rj.Command, OK: false, Error: rj.Error,
		})
	}
	if rep.Commands == nil {
		rep.Commands = []runCommand{}
	}
	for _, l := range logs {
		rep.Logs = append(rep.Logs, runLog{Level: l.Level, Message: l.Message})
	}
	return rep
}

// defaultRunLocation is the evaluation location a manual run's `sun` and `time`
// leaves see: the zero Location, under which a `sun` leaf fails closed (it has
// no coordinates) and every other leaf behaves normally.
//
// It is a named function rather than an inline zero value so that the omission
// is stated rather than looking like an oversight. Resolving the rule's own
// scope node's effective geo (DAT-033) would be the complete answer, and it is
// not done here because the run's disposition would then depend on a tree walk
// whose failure mode (EFFECTIVE_GEO_UNRESOLVED) has no place in this response —
// a rule with a sun condition is a scheduled rule, and running it by hand at
// noon was never going to reproduce dusk anyway.
func defaultRunLocation() schedule.Location { return schedule.Location{} }

// appClock is the rules clock.Clock over the server's own injected now, so a
// manual run reads time through the same seam every other server-minted
// timestamp does (srv.nowMs) rather than calling time.Now itself — which is what
// lets a test run an automation at a fixed instant.
//
// Mono is derived from the wall reading rather than from a separate monotonic
// source. That is sound HERE and nowhere else: the only duration mechanism a
// manual run can reach is `delay`, which this file collapses without waiting, so
// no elapsed-time comparison is ever made against this value. The engine, which
// does make such comparisons, has a real monotonic clock (automationhost).
type appClock struct{ nowMs func() int64 }

func (c appClock) WallMillis() int64 { return c.nowMs() }
func (c appClock) Mono() int64       { return c.nowMs() }

// entitySnapshot projects the device plane's read model onto the rules/1
// state.Snapshot condition evaluation and command-vocabulary checking read from.
//
// This is the join that makes a manual run's conditions mean anything: a `state`
// condition asks what an entity's state IS, and the app peer learns that from
// the relay's own `device.candidates` report (REL-110a), which is exactly what
// devices.Registry holds. A deployment with no device plane wired gets an empty
// snapshot, under which every entity-scoped condition fails closed — which is
// the honest answer, since nothing here knows anything about any entity.
func (srv *server) entitySnapshot() state.Snapshot {
	snap := state.Snapshot{}
	if srv.devices == nil {
		return snap
	}
	for _, e := range srv.devices.Entities() {
		snap[e.ID] = state.Entity{ID: e.ID, DeviceClass: e.DeviceClass, State: e.State}
	}
	return snap
}

// resolveEntityRef is eval.ActionContext.Resolve for a manual run: it expands a
// `selector` or `device_class` EntityRef (RUL-012/013) to the concrete entity
// ids it matches, in id order so a run is deterministic.
//
// The expansion is drawn from the entities the CALLER can read, and narrowed by
// the selector — the same rule the list read and the bulk-enable target
// predicate apply, for the same reason (devices.go, automations.go): a fan-out
// that reached past the caller's visible set would let a rule be used to
// enumerate, and then command, entities they may not see.
//
// A malformed selector resolves to no targets rather than an error. The compile
// gate does not parse selector syntax (rules/1 leaves the grammar out of scope),
// so this is the first place it could be discovered, and a dispatch to "every
// entity" on a typo is the one outcome that must not happen.
func (srv *server) resolveEntityRef(view scopeView) func(model.EntityRef) []string {
	return func(ref model.EntityRef) []string {
		if srv.devices == nil {
			return nil
		}
		var sel apiselector.Selector
		if ref.Selector != "" {
			parsed, serr := apiselector.Parse(ref.Selector)
			if serr != nil {
				return nil
			}
			sel = parsed
		}
		var out []string
		for _, e := range srv.devices.Entities() {
			if !view.canRead(e.ScopeNode) {
				continue
			}
			if ref.DeviceClass != "" && e.DeviceClass != ref.DeviceClass {
				continue
			}
			if ref.Selector != "" && !sel.Matches(e.Labels, e.ScopeNode, view.inSubtree) {
				continue
			}
			out = append(out, e.ID)
		}
		sort.Strings(out)
		return out
	}
}

// ---- device commands -------------------------------------------------------

// relayCommandSink is eval.CommandSink over the SAME dispatch path an operator's
// `POST /entities/{id}/commands` takes: resolve the entity to the relay that
// owns it, and send one `device.command` down that relay's already-open
// persistent connection (REL-112). There is no second transport, no queue, and
// no "the relay will pick this up later" — the exchange completes within the
// run, or it is reported as failed.
//
// It records every attempt, including the refusals, because the report is the
// deliverable: "ran, and the command to screen 3 was refused because its relay
// is offline" is a useful answer and "ran" is not.
type relayCommandSink struct {
	srv     *server
	ctx     context.Context
	traceID string
	view    scopeView
	dry     bool

	results []runCommand
}

// Dispatch carries one command to one entity. It returns an error for a failed
// dispatch (which eval aggregates into a multi-target outcome) AND records the
// per-target result here, because eval's own CommandResult list is only
// populated for a multi-target action (RUL-161 makes a single-target dispatch
// atomic and unaggregated) — and the operator needs the single-target case
// reported just as much.
func (s *relayCommandSink) Dispatch(entityID, command string, params map[string]any) error {
	err := s.dispatch(entityID, command, params)
	rec := runCommand{EntityID: entityID, Command: command, OK: err == nil}
	if err != nil {
		rec.Error = err.Error()
	}
	s.results = append(s.results, rec)
	return err
}

func (s *relayCommandSink) dispatch(entityID, command string, params map[string]any) error {
	if s.srv.devices == nil {
		return fmt.Errorf("no device plane is wired; the command was not dispatched")
	}
	entity, found := s.srv.devices.Entity(entityID)
	if !found {
		return fmt.Errorf("no entity exists with this identifier")
	}
	if !s.view.canRead(entity.ScopeNode) || !s.view.canWrite(entity.ScopeNode) {
		// Reported rather than silently skipped: an operator whose rule targets
		// an entity outside their authority must be told the rule did not do
		// what it says, not left to infer it from an unchanged device.
		return fmt.Errorf("not authorized to command an entity placed at this scope node")
	}
	if s.dry {
		return nil
	}
	if s.srv.dispatch == nil {
		return fmt.Errorf("no relay connection plane is available to carry commands")
	}
	ctx, cancel := context.WithTimeout(s.ctx, deviceCommandTimeout)
	defer cancel()
	res, err := s.srv.dispatch.SendDeviceCommand(ctx, entity.RelayID, s.traceID, wire.DeviceCommandBody{
		EntityID: entity.ID,
		Command:  command,
		Params:   params,
	})
	if err != nil {
		return err
	}
	if !res.OK {
		if res.Error != nil {
			return fmt.Errorf("%s: %s", res.Error.Code, res.Error.Message)
		}
		return fmt.Errorf("the relay refused the command")
	}
	return nil
}

// ---- signage ---------------------------------------------------------------

// screenOverrideSink is eval.SignageSink over the authored screen rows: each of
// the three RUL-234/235 actions resolves its ScreenRef to concrete screen rows
// and writes (or clears) each one's program override (DAT-004c).
//
// The write goes through srv.store.Update — the SAME path a console PATCH of the
// screen takes — so it advances the store generation, is picked up by the next
// desired-state build, is projected onto that screen's `screen_programs` entry,
// and reaches the screen as a Lease. Writing anywhere else (a side table, an
// in-memory pin) would produce a run that reports success and a screen that
// never changes.
type screenOverrideSink struct {
	srv  *server
	ctx  context.Context
	view scopeView
	dry  bool
}

func (s *screenOverrideSink) PlayCast(ref eval.ScreenRef, castID string) eval.SignageOutcome {
	return s.apply("play_cast", ref, func() (*datamodel.ScreenOverride, error) {
		if err := s.castExists(castID); err != nil {
			return nil, err
		}
		return &datamodel.ScreenOverride{
			Mode:   datamodel.ScreenOverrideModePlay,
			CastID: castID,
			SetAt:  s.srv.nowMs(),
		}, nil
	})
}

func (s *screenOverrideSink) ShowAlert(ref eval.ScreenRef, castID, message string, ttlSeconds int) eval.SignageOutcome {
	return s.apply("show_alert", ref, func() (*datamodel.ScreenOverride, error) {
		if castID != "" {
			if err := s.castExists(castID); err != nil {
				return nil, err
			}
		}
		o := &datamodel.ScreenOverride{
			Mode:    datamodel.ScreenOverrideModeAlert,
			CastID:  castID,
			Message: message,
			SetAt:   s.srv.nowMs(),
		}
		if ttlSeconds > 0 {
			// The author declares a DURATION; the row stores an absolute instant,
			// because DAT-004d retires a lapsed override at resolution time with
			// no writer involved — which is what lets an alert self-limit on a
			// relay that has lost its app peer. Converting here, against the
			// server's own injected clock, is the only place both facts are known.
			o.ExpiresAt = s.srv.nowMs() + int64(ttlSeconds)*int64(time.Second/time.Millisecond)
		}
		return o, nil
	})
}

func (s *screenOverrideSink) DismissAlert(ref eval.ScreenRef) eval.SignageOutcome {
	return s.apply("dismiss_alert", ref, func() (*datamodel.ScreenOverride, error) { return nil, nil })
}

// apply resolves ref to screen rows and applies build()'s override to each,
// independently (RUL-236): one screen the caller may not write, or one whose
// conditional update lost a race, never prevents the rest and never halts the
// rule.
//
// build is called ONCE, before any write, and its failure fails every target
// rather than each one identically — a `cast_id` that names nothing is a fault
// of the ACTION, not of any screen, and reporting it N times with N screen ids
// would read as N unrelated problems.
func (s *screenOverrideSink) apply(action string, ref eval.ScreenRef, build func() (*datamodel.ScreenOverride, error)) eval.SignageOutcome {
	targets, err := s.targets(ref)
	if err != nil {
		// An unresolvable ScreenRef is a failure of the ACTION, not of any
		// screen, and it is reported on the action's own error channel. The
		// alternative this used to do — one ScreenResult with an empty ScreenID —
		// serves an empty string in a member the document declares as a ULID
		// (AutomationRunScreen.screen_id, required, `$ref: Ulid`), so a generated
		// typed client is handed an invalid id on exactly the path it most needs
		// to read.
		return eval.FailedSignageOutcome(action, err.Error())
	}
	if len(targets) == 0 {
		// RUL-233: a selector matching no screen is a legal no-op, not a failure.
		return eval.SignageOutcomeFor(action, []eval.ScreenResult{})
	}

	override, berr := build()
	if berr != nil {
		results := make([]eval.ScreenResult, 0, len(targets))
		for _, t := range targets {
			results = append(results, eval.ScreenResult{ScreenID: t.ID, OK: false, Error: berr.Error()})
		}
		return eval.SignageOutcomeFor(action, results)
	}

	results := make([]eval.ScreenResult, 0, len(targets))
	for _, t := range targets {
		results = append(results, s.write(t, override))
	}
	return eval.SignageOutcomeFor(action, results)
}

// screenTarget is one resolved screen row: the id to write and the revision the
// conditional write is made against.
//
// refusal, when non-empty, is a target this run will NOT write and the reason —
// carried as a target rather than as an action-level error so the report names
// the screen the author typed. See targets for the one case that produces it.
type screenTarget struct {
	ID        string
	Revision  int64
	ScopeNode string
	refusal   string
}

// targets resolves a ScreenRef (RUL-233) to the screen rows it names, in id
// order. A `selector` resolves against the caller's readable screens exactly as
// the list read does.
//
// A `screen_id` naming nothing the run can reach — no such row, or a row outside
// the view — resolves to ONE REFUSED TARGET carrying that id, not to an
// action-level error with no target at all. The distinction is what makes the
// refusal readable: an action-level error has no screen to hang off, so the
// effect report (and, for an event-fired run, the automation.run record that is
// its ONLY reader) said "no screen exists with this identifier" and named
// nothing. An operator holding a rule with three play_cast actions learned that
// one of them referred to some screen somewhere.
//
// The message deliberately states BOTH possibilities rather than picking one.
// Asserting non-existence about a screen that exists but sits outside the run's
// scope is a false statement that sends an operator hunting for a deleted row;
// asserting the scope reason would disclose the existence of a row the view is
// not permitted to see, which is the whole point of resolving an invisible row
// as "not found" everywhere else on this surface (scopeview.go). Naming the two
// things to check discloses nothing and is true either way.
//
// A screen the view can READ but not WRITE is a different case and is already
// handled one step later, by write, which can and does say exactly why.
func (s *screenOverrideSink) targets(ref eval.ScreenRef) ([]screenTarget, error) {
	rows, err := s.srv.store.List(s.ctx, store.KindScreen, store.ListFilter{})
	if err != nil {
		return nil, fmt.Errorf("the screen rows could not be read")
	}

	var sel apiselector.Selector
	if ref.Selector != "" {
		parsed, serr := apiselector.Parse(ref.Selector)
		if serr != nil {
			return nil, fmt.Errorf("the selector could not be parsed: %s", serr.Detail)
		}
		sel = parsed
	}

	var out []screenTarget
	for _, res := range rows {
		f := parseFields(res.Body)
		if !s.view.canRead(f.ScopeNode) {
			continue
		}
		switch {
		case ref.ScreenID != "":
			if res.ID != ref.ScreenID {
				continue
			}
		case ref.Selector != "":
			if !sel.Matches(f.Labels, f.ScopeNode, s.view.inSubtree) {
				continue
			}
		default:
			// Unreachable through a compiled rule (SCREEN_REF_AMBIGUOUS refuses
			// a ScreenRef naming neither), and fail-closed if it ever is: an
			// empty ref must never mean "every screen".
			return nil, fmt.Errorf("the action names no screen")
		}
		out = append(out, screenTarget{ID: res.ID, Revision: res.Revision, ScopeNode: f.ScopeNode})
	}
	if ref.ScreenID != "" && len(out) == 0 {
		// Attributed to the authored id — but ONLY when that id is a canonical
		// ULID. AutomationRunScreen.screen_id is `$ref: Ulid` and required, and
		// rules/1 does not constrain a ScreenRef's screen_id beyond its TYPE
		// (RUL-233's MEMBER_TYPE_INVALID is about the JSON type, not the
		// grammar), so an author can store a `screen_id` that is a perfectly good
		// string and not an identifier. Echoing that into the report would put a
		// non-ULID in a field a generated client reads as one — the exact defect
		// this function's caller was fixed for. A malformed id has nothing useful
		// to attribute anyway: it names no screen an operator could go look at.
		if !ulid.Valid(ref.ScreenID) {
			return nil, fmt.Errorf("no screen exists with this identifier")
		}
		return []screenTarget{{ID: ref.ScreenID, refusal: "this run cannot reach a screen with this identifier: " +
			"either no such screen exists, or it is placed outside the scope this run acts within"}}, nil
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// write applies one screen's override through the ordinary conditional resource
// update (API-022): the patch carries `override` (or an explicit null to clear
// it) and nothing else, against the revision the row was read at.
func (s *screenOverrideSink) write(t screenTarget, override *datamodel.ScreenOverride) eval.ScreenResult {
	// A target resolution already refused (targets): reported against the id the
	// author wrote, never silently dropped and never attempted.
	if t.refusal != "" {
		return eval.ScreenResult{ScreenID: t.ID, OK: false, Error: t.refusal}
	}
	if !s.view.canWrite(t.ScopeNode) {
		return eval.ScreenResult{ScreenID: t.ID, OK: false,
			Error: "not authorized to write a screen placed at this scope node"}
	}
	if s.dry {
		return eval.ScreenResult{ScreenID: t.ID, OK: true}
	}
	// An explicit JSON null clears the override, which is the mergeBody
	// convention for removing an optional member — the same shape a console
	// PATCH clearing it would send. Omitting the key would leave it untouched,
	// which is exactly what dismiss_alert must not do.
	patch := map[string]any{"override": override}
	body, err := json.Marshal(patch)
	if err != nil {
		return eval.ScreenResult{ScreenID: t.ID, OK: false, Error: "the override could not be encoded"}
	}
	if _, err := s.srv.store.Update(s.ctx, store.KindScreen, t.ID, t.Revision, body); err != nil {
		return eval.ScreenResult{ScreenID: t.ID, OK: false, Error: overrideWriteError(err)}
	}
	return eval.ScreenResult{ScreenID: t.ID, OK: true}
}

// overrideWriteError renders a store failure as a per-screen reason an operator
// can act on, without leaking internals: a lost revision race is retryable and
// says so, a validation failure names the members that were refused, and
// anything else is reported as an unspecified refusal rather than as success.
func overrideWriteError(err error) string {
	var verr *store.ValidationError
	switch {
	case errors.Is(err, store.ErrRevisionMismatch):
		// Someone edited this screen between the target resolution and the
		// write. Named as retryable because it is: the run did no partial work
		// on this screen, and running it again reads the new revision.
		return "the screen was changed by another writer during this run; retry the run"
	case errors.Is(err, store.ErrNotFound):
		return "the screen was deleted during this run"
	case errors.As(err, &verr):
		msgs := make([]string, 0, len(verr.Errors))
		for _, e := range verr.Errors {
			msgs = append(msgs, e.Code+": "+e.Message)
		}
		return strings.Join(msgs, "; ")
	default:
		return "the screen was not updated: " + err.Error()
	}
}

// castExists reports whether castID names a cast row, which is DAT-004c's
// reference check placed where the contract puts it: on the surface IMPOSING
// the override, because that surface holds both row families at once and the
// screen row's own validator (which sees only screens and devices) does not.
func (s *screenOverrideSink) castExists(castID string) error {
	if castID == "" {
		return fmt.Errorf("the action names no cast")
	}
	_, found, err := s.srv.store.Get(s.ctx, store.KindCast, castID)
	if err != nil {
		return fmt.Errorf("the cast could not be read")
	}
	if !found {
		return fmt.Errorf("no cast exists with this identifier")
	}
	return nil
}

// variableWriteSink is eval.VariableSink over the authored variable rows: a
// RUL-220 `variable_write` action declares `name` and a value, and this writes
// it as a data-model/1 variable row (DAT-130) at the RULE'S OWN scope node.
//
// # Which node, and why not the one the value currently resolves at
//
// The write always lands at `node` — the automation row's own placement — and
// never at whichever ancestor a read of that name currently resolves through
// (DAT-134). The alternative is a privilege widening with no gate on it: a rule
// authored at one group could mutate a value set at the site and thereby
// retarget every other group's rules, while the same operator writing the same
// name by hand at the site would be refused by placement authority. Writing at
// the rule's own node produces an OVERRIDE instead, which is exactly the shape
// DAT-134/136 already define — visible to that node's subtree, leaving the
// ancestor's value intact, and re-exposing it if the override is later deleted.
//
// # The create/update decision, and the race in it
//
// A write is an update when a row of that name already exists AT THAT NODE, and
// a create otherwise. That read-then-write is not atomic, and it does not need
// to be: the create path carries the same DAT-131 uniqueness guard the HTTP
// surface does (store.VariableNameUniqueGuard), evaluated inside the write
// transaction, so a lost race becomes a refusal reported in this action's
// outcome rather than a second row with the same name at the same node. Two
// rules writing the same variable in the same tick therefore serialize, both
// commit in some order, both emit, and the last one is the value — see
// store/variables.go, where that is stated once for every writer.
type variableWriteSink struct {
	srv     *server
	ctx     context.Context
	traceID string
	view    scopeView
	node    string
	dry     bool
}

func (s *variableWriteSink) WriteVariable(name string, value any) eval.VariableOutcome {
	out := eval.VariableOutcome{Action: "variable_write", Variable: name}

	// The row rules (DAT-131a name grammar, DAT-132/133 scalar value) are applied
	// HERE as well as at the HTTP surface, and that is not redundancy: this
	// writer calls store.Create/Update directly, so the api layer's per-kind
	// `validate` hook never runs for it. Without this, a rule could write a row
	// no operator could have written by hand.
	probe, merr := json.Marshal(map[string]any{"name": name, "value": value})
	if merr != nil {
		out.Error = "the value could not be encoded"
		return out
	}
	if errs := datamodel.ValidateVariableBody(probe); len(errs) > 0 {
		msgs := make([]string, 0, len(errs))
		for _, e := range errs {
			msgs = append(msgs, e.Code+": "+e.Message)
		}
		out.Error = strings.Join(msgs, "; ")
		return out
	}

	// Placement authorization. UNREACHABLE AS CURRENTLY CALLED, and recorded as
	// such rather than left to look load-bearing: both call sites hand this sink
	// the automation's OWN scope node, and both have already authorized a write
	// there — the manual run through runAutomationExec's canWrite(node) 403, the
	// event-fired run through automationScopeView, whose canWrite is the
	// automation's own subtree. Mutating this condition to `false &&` breaks no
	// test, and no test is claimed for it.
	//
	// It is kept rather than deleted because it is the one line that makes the
	// sink safe to call with a node other than the one it was built with, and the
	// signage sink beside it needs exactly that (a screen sits at an unrelated
	// node, so its per-target check DOES fire). A future caller that resolves a
	// write to an ancestor, or a `variable_write` that grows an explicit
	// `scope_node` member, makes this reachable on its first day; without it,
	// that change would ship an unauthorized write and no gate would notice.
	if !s.view.canWrite(s.node) {
		out.Error = "not authorized to write a variable at this automation's scope node"
		return out
	}

	existing, found, err := s.existingRow(name)
	if err != nil {
		out.Error = "the variable rows could not be read"
		return out
	}

	if s.dry {
		// A dry run resolves the target and withholds the effect, exactly as the
		// signage sink does: the value is reported as the one that WOULD be
		// written, and nothing is stored and nothing is published.
		out.OK, out.Value = true, value
		return out
	}

	var before, after json.RawMessage
	if found {
		patch, perr := json.Marshal(map[string]any{"value": value})
		if perr != nil {
			out.Error = "the value could not be encoded"
			return out
		}
		before = existing.Body
		res, uerr := s.srv.store.Update(s.ctx, store.KindVariable, existing.ID, existing.Revision, patch,
			store.VariableNameUniqueGuard(name, s.node, existing.ID))
		if uerr != nil {
			out.Error = variableWriteError(uerr)
			return out
		}
		after = res.Body
	} else {
		body, berr := json.Marshal(map[string]any{
			"id": s.srv.newID(), "name": name, "value": value, "scope_node": s.node,
		})
		if berr != nil {
			out.Error = "the variable could not be encoded"
			return out
		}
		res, cerr := s.srv.store.Create(s.ctx, store.KindVariable, body,
			store.VariableNameUniqueGuard(name, s.node, ""))
		if cerr != nil {
			out.Error = variableWriteError(cerr)
			return out
		}
		after = res.Body
	}

	// DAT-137: once per COMMITTED write, through the same emitter the HTTP
	// handlers use. Emitted after the store call returns, so a refused write
	// publishes nothing — the event says a value changed, and one that did not
	// change must not say so.
	s.srv.publishVariableChange(s.traceID, before, after)

	out.OK, out.Value = true, value
	return out
}

// existingRow finds the variable row named name AT THIS SINK'S NODE, if one
// exists. It matches on the node's OWN rows only — never on an ancestor's —
// because that is the row a write at this node would replace; an ancestor's row
// is a different row at a different placement, and updating it is the widening
// this sink's doc refuses.
func (s *variableWriteSink) existingRow(name string) (store.Resource, bool, error) {
	rows, err := s.srv.store.List(s.ctx, store.KindVariable, store.ListFilter{ScopeNode: s.node})
	if err != nil {
		return store.Resource{}, false, err
	}
	for _, res := range rows {
		var row struct {
			Name string `json:"name"`
		}
		if uerr := json.Unmarshal(res.Body, &row); uerr != nil {
			continue
		}
		if row.Name == name {
			return res, true, nil
		}
	}
	return store.Resource{}, false, nil
}

// variableWriteError renders a store failure as a reason an operator can act on,
// mirroring overrideWriteError above: a lost revision race says it is
// retryable, a field-level refusal names its own code and message (which is how
// VARIABLE_NAME_DUPLICATE from the uniqueness guard reaches this report), and
// anything else is an unspecified refusal rather than a silent success.
func variableWriteError(err error) string {
	var verr *store.ValidationError
	switch {
	case errors.Is(err, store.ErrRevisionMismatch):
		return "the variable was changed by another writer during this run; retry the run"
	case errors.Is(err, store.ErrNotFound):
		return "the variable was deleted during this run"
	case errors.As(err, &verr):
		msgs := make([]string, 0, len(verr.Errors))
		for _, e := range verr.Errors {
			msgs = append(msgs, e.Code+": "+e.Message)
		}
		return strings.Join(msgs, "; ")
	default:
		return "the variable could not be written"
	}
}
