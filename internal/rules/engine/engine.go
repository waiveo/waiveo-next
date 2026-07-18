// Package engine is the rules/1 edge-rules evaluation core's run manager: it
// drives a single compiled edge rule (Part 1's classifier output) against a
// sequence of entity observations on an injectable clock, firing its
// state/numeric triggers (with first-observation baselines and `for`-holds),
// evaluating its conditions as an implicit AND (RUL-004), and executing its
// actions through the injectable sinks of package eval — resolving each firing
// that reaches the action sequence to a RunDisposition.
//
// The base run machine drives trigger -> conditions -> actions, with a `delay`
// action's tail deferred and resumed on monotonic time via Tick (RUL-190). The
// `single`/`restart` mode disposition logic (RUL-241/242/245/246) sits on top:
// a firing that starts a fresh run records `ran`; under single a firing that
// arrives while a run is in flight is dropped and records `skipped`, leaving
// the in-flight run untouched (RUL-241); under restart such a firing cancels
// the in-flight run — recording `restarted` — and starts a fresh run recording
// `ran` with timers zeroed (RUL-242). queued/parallel are app-class and are
// refused at Load (RUL-240).
package engine

import (
	"encoding/json"
	"errors"
	"reflect"

	"github.com/maaxton/waiveo-next/internal/rules/clock"
	"github.com/maaxton/waiveo-next/internal/rules/compile"
	"github.com/maaxton/waiveo-next/internal/rules/eval"
	"github.com/maaxton/waiveo-next/internal/rules/model"
	"github.com/maaxton/waiveo-next/internal/rules/registry"
	"github.com/maaxton/waiveo-next/internal/rules/state"
)

// Disposition is the closed outcome a firing (or a dropped/preempted firing)
// resolves to for mode evaluation (RUL-246): `ran`, `skipped`, or `restarted`.
type Disposition string

const (
	// Ran — a new run started normally (RUL-246).
	Ran Disposition = "ran"
	// Skipped — a firing dropped per single's overflow handling (RUL-241; the
	// app engine's parallel overflow, RUL-244, records it too but never reaches
	// this edge engine).
	Skipped Disposition = "skipped"
	// Restarted — the run a restart-mode firing preempted (RUL-242).
	Restarted Disposition = "restarted"
)

// RunDisposition is the recorded outcome of one trigger firing that reached
// mode evaluation (RUL-246, Wire shapes). MisfireCaught is the orthogonal
// marker RUL-355 sets on a caught-up misfired firing (a later part); it is
// always false here.
type RunDisposition struct {
	RuleID        string      `json:"rule_id"`
	Disposition   Disposition `json:"disposition"`
	Mode          string      `json:"mode"`
	MisfireCaught bool        `json:"misfire_caught"`
}

// Engine drives exactly one loaded edge rule. It is not safe for concurrent
// use: Observe and Tick advance the same trigger/hold/run state serially, as a
// relay's single evaluation loop drives them.
type Engine struct {
	reg     registry.Registry
	clk     clock.Clock
	sink    eval.CommandSink
	presets eval.PresetStore

	loaded bool
	ruleID string
	mode   string
	rule   model.Rule

	triggers []*triggerRuntime

	// snap is the engine's running view of every entity it has observed, the
	// point-in-time snapshot RUL-101 condition evaluation resolves against.
	snap state.Snapshot

	// run is the single in-flight run, non-nil while a `delay` tail is pending
	// (RUL-190). One run at a time is tracked for the mode layer (next part).
	run *runState
}

// runState is the single in-flight run's continuation: the flattened tail of
// actions a `delay` deferred, and the monotonic instant at which to resume it
// (RUL-190).
type runState struct {
	pending *eval.DelayPending
}

// triggerRuntime is one trigger's per-(trigger,entity) evaluation state: the
// decoded trigger, its `for`-hold, and the durable baselines Observe diffs the
// next observation against. This part supports the entity_id form only; the
// selector/device_class fan-out forms are a later concern.
type triggerRuntime struct {
	kind     string // "state" | "numeric"
	entityID string

	st *eval.StateTrigger
	nt *eval.NumericTrigger

	hold       *eval.Hold
	holdKind   eval.HoldKind
	forSeconds int

	// state baseline (RUL-300/304): the durable last-observed value the state
	// trigger diffs against.
	baseline eval.TriggerBaseline
	// numeric baseline (RUL-033/302): the last parsed value to cross from, nil
	// when none exists.
	prevParsed *float64

	seen bool // has this trigger ever processed an observation of its subject?

	// lastLevelHolds carries whether the bounded hold's target level held at
	// the most recent observation, so Tick can advance an armed bounded hold
	// with the correct steady-state level between observations.
	lastLevelHolds bool
	// lastAttr is the previous value of an unbounded attribute-scoped trigger's
	// named attribute, used to decide whether the attribute currently "holds"
	// its last-changed value for the bounded for-hold's level (RUL-023 case 2).
	lastAttr any
}

// New builds an Engine over the given evaluation environment. presets may be
// nil when no rule loaded into this engine uses a preset_batch action.
func New(reg registry.Registry, clk clock.Clock, sink eval.CommandSink, presets eval.PresetStore) *Engine {
	return &Engine{
		reg:     reg,
		clk:     clk,
		sink:    sink,
		presets: presets,
		snap:    state.Snapshot{},
	}
}

// Load prepares the engine to evaluate one compiled rule. It refuses an
// app-classified entry (RUL-240): queued/parallel run management, and any other
// app-causing member, belong to the app engine, never this relay's edge engine.
// A single Engine holds one rule at a time; Load replaces any previously loaded
// rule and its in-flight state.
func (e *Engine) Load(entry compile.CompiledRuleEntry, rule model.Rule) error {
	if entry.ExecutionClass != "edge" {
		return errors.New("engine: refusing to load a non-edge rule (execution_class=" + entry.ExecutionClass + ")")
	}
	e.loaded = true
	e.ruleID = rule.ID
	e.mode = rule.Mode
	if e.mode == "" {
		e.mode = "single"
	}
	e.rule = rule
	e.snap = state.Snapshot{}
	e.run = nil
	e.triggers = e.triggers[:0]
	for _, m := range rule.Triggers {
		if tr := newTriggerRuntime(m); tr != nil {
			e.triggers = append(e.triggers, tr)
		}
	}
	return nil
}

// newTriggerRuntime builds the runtime for one trigger member, or nil for a
// trigger kind this evaluation core does not drive (time/time_pattern/sun are
// later parts) or one lacking an entity_id subject.
func newTriggerRuntime(m model.Member) *triggerRuntime {
	tr := &triggerRuntime{kind: m.Type, forSeconds: parseForSeconds(m.Raw)}
	switch m.Type {
	case "state":
		st, err := eval.NewStateTrigger(m)
		if err != nil || st.EntityID == "" {
			return nil
		}
		tr.st = st
		tr.entityID = st.EntityID
		// RUL-023: an unscoped state trigger (no attribute, no from/to) debounces
		// its `for` (RUL-026); cases 1-3 are bounded level holds (RUL-024).
		if st.Attribute == "" && !st.HasFrom && !st.HasTo {
			tr.holdKind = eval.DebounceHold
		} else {
			tr.holdKind = eval.BoundedHold
		}
	case "numeric":
		nt, err := eval.NewNumericTrigger(m)
		if err != nil || nt.EntityID == "" {
			return nil
		}
		tr.nt = nt
		tr.entityID = nt.EntityID
		tr.holdKind = eval.BoundedHold // every numeric trigger is bounded (RUL-033)
	default:
		return nil
	}
	tr.hold = eval.NewHold(tr.holdKind, tr.forSeconds)
	return tr
}

// parseForSeconds reads a trigger's optional `for` field (RUL-024: a
// non-negative integer number of seconds; absent decodes as 0, which behaves as
// if `for` were absent).
func parseForSeconds(raw json.RawMessage) int {
	if len(raw) == 0 {
		return 0
	}
	var spec struct {
		For int `json:"for"`
	}
	_ = json.Unmarshal(raw, &spec)
	return spec.For
}

// SeedEntityState seeds the durable prior state of an entity (RUL-304): the
// value a state trigger on that entity diffs its very next observation against.
// It is how the caller supplies a pre-restart durable value on an engine
// restart, or the `from` of a self-describing state transition, so the next
// observation is evaluated as a genuine transition rather than suppressed as a
// first-ever observation (RUL-300/302). It also records the entity into the
// snapshot so a condition can resolve it immediately.
func (e *Engine) SeedEntityState(entityID, st string) {
	cur, ok := e.snap[entityID]
	if !ok {
		cur = state.Entity{ID: entityID}
	}
	cur.State = st
	e.snap[entityID] = cur
	for _, tr := range e.triggers {
		if tr.entityID != entityID {
			continue
		}
		tr.baseline = eval.TriggerBaseline{Known: true, State: st}
		// Seeding flips `seen`, which disables the first-observation
		// hold-seed branch in stepTriggerObservation. So seed the bounded
		// hold's remembered level here too (RUL-360): the durable pre-restart
		// level must be recorded so the boot reconfirmation of unchanged
		// durable state (RUL-025/300) is read as the level continuing to hold,
		// NOT as a fresh rising edge that would arm the `for`-hold and
		// spuriously fire `for` seconds after boot. A bounded hold begins
		// counting again only once its condition FRESHLY re-matches after
		// restart (RUL-360/361). Restricted to `state` triggers to mirror
		// RUL-302's numeric exemption from RUL-300 suppression.
		if tr.kind == "state" && tr.hold != nil {
			tr.lastLevelHolds = e.stateLevelHolds(tr, cur)
			tr.hold.Seed(tr.lastLevelHolds)
		}
		tr.seen = true
	}
}

// Observe advances the engine by one entity observation: it updates the
// snapshot, runs every trigger whose subject is the observed entity, and — for
// each trigger that fires — evaluates the rule's conditions (RUL-004) and, when
// they pass, starts a run of the action sequence. It returns the dispositions
// of any firings that reached mode evaluation.
func (e *Engine) Observe(obs state.Observation) []RunDisposition {
	if !e.loaded {
		return nil
	}
	e.snap[obs.Entity.ID] = obs.Entity

	var out []RunDisposition
	for _, tr := range e.triggers {
		if tr.entityID != obs.Entity.ID {
			continue
		}
		if e.stepTriggerObservation(tr, obs) {
			out = append(out, e.fire()...)
		}
	}
	return out
}

// Tick advances any in-flight `for`-holds and the in-flight `delay` resumption
// against the current clock (RUL-024/190, monotonic only). A hold whose span
// has now elapsed fires and may start a run (returning its disposition); a
// pending delay whose resume instant has been reached dispatches its tail (no
// new disposition — the firing that scheduled it already recorded `ran`).
func (e *Engine) Tick(now clock.Clock) []RunDisposition {
	if !e.loaded {
		return nil
	}
	var out []RunDisposition
	for _, tr := range e.triggers {
		if tr.forSeconds <= 0 || tr.hold == nil {
			continue // nothing to advance between observations
		}
		if e.stepTriggerTick(tr, now) {
			out = append(out, e.fire()...)
		}
	}
	e.resumeDelay(now)
	return out
}

// stepTriggerObservation feeds one observation through a trigger's pure fire
// logic and its `for`-hold, returning whether the trigger fires now. The hold's
// rawMatched is derived per RUL-024/026: a debounce is fed the raw edge (a
// qualifying change occurred), a bounded hold the target level (does it
// currently hold). A first-ever observation seeds the bounded hold's remembered
// level so it is not mistaken for a fresh entry (RUL-300/360).
func (e *Engine) stepTriggerObservation(tr *triggerRuntime, obs state.Observation) bool {
	edge, level := e.evalTrigger(tr, obs)

	// No `for`: the raw fire decision is authoritative (0 behaves as absent,
	// RUL-024). This is the fully-tested path for every RUL-023 case and the
	// numeric level-then-crossing rule.
	if tr.forSeconds <= 0 {
		tr.seen = true
		tr.lastLevelHolds = level
		return edge
	}

	var raw bool
	if tr.holdKind == eval.DebounceHold {
		raw = edge
	} else {
		raw = level
		if !tr.seen && tr.kind == "state" {
			// Suppress a first-ever observation that already sits at the level
			// from being read as a fresh entry that would arm a hold (RUL-300).
			// This applies ONLY to `state` triggers. A numeric trigger's
			// first-ever observation is deliberately EXEMPTED from RUL-300's
			// suppression (RUL-302): if it already satisfies the bound the hold
			// arms from that first sample and fires once the level has held
			// continuously for the declared `for` (RUL-033/RUL-024). Seeding a
			// numeric hold here would defeat Step's rising-edge test and the
			// trigger would never arm on a first-observation-already-satisfying
			// value — a deliberate asymmetry with RUL-300, not an inconsistency.
			tr.hold.Seed(level)
		}
	}
	tr.seen = true
	tr.lastLevelHolds = level
	return tr.hold.Step(e.clk, raw)
}

// stepTriggerTick advances an armed `for`-hold between observations, feeding the
// steady-state signal: a bounded hold re-reads its last level (unchanged since
// the last observation), a debounce reads "no further change this tick".
func (e *Engine) stepTriggerTick(tr *triggerRuntime, now clock.Clock) bool {
	if tr.holdKind == eval.DebounceHold {
		return tr.hold.Step(now, false)
	}
	return tr.hold.Step(now, tr.lastLevelHolds)
}

// evalTrigger runs a trigger's pure Observe against its persisted baseline,
// updates that baseline, and reports (edge, level): edge is the raw qualified
// fire decision (RUL-023/033), level is whether the trigger's bounded target
// currently holds (RUL-024) — used only for a bounded `for`-hold.
func (e *Engine) evalTrigger(tr *triggerRuntime, obs state.Observation) (edge bool, level bool) {
	switch tr.kind {
	case "state":
		fired, next := tr.st.Observe(e.reg, tr.baseline, obs)
		tr.baseline = next
		level = e.stateLevelHolds(tr, obs.Entity)
		return fired, level
	case "numeric":
		fired, next := tr.nt.Observe(tr.prevParsed, obs)
		tr.prevParsed = next
		level = e.numericLevelHolds(tr, obs.Entity)
		return fired, level
	default:
		return false, false
	}
}

// stateLevelHolds reports whether a bounded state trigger's matched level
// currently holds (RUL-024): the `to` bound against the entity's canonical
// state (state-scoped) or against the named attribute's value (attribute-
// scoped). An unbounded attribute trigger (RUL-023 case 2) has no fixed target,
// so its "level" is the attribute holding its last-changed value.
func (e *Engine) stateLevelHolds(tr *triggerRuntime, ent state.Entity) bool {
	st := tr.st
	if st.Attribute == "" {
		if st.HasTo {
			return eval.MatchState(e.reg, ent.DeviceClass, ent.State, st.To)
		}
		return true // from-only bound: no steady level to drop below
	}
	av, ok := ent.Attributes[st.Attribute]
	if !ok {
		return false
	}
	prevAttr := tr.lastAttr
	tr.lastAttr = av
	if st.HasTo {
		vt := e.reg.AttributeType(ent.DeviceClass, st.Attribute)
		return eval.TypedEqual(av, st.To, vt)
	}
	// Unbounded attribute: the level holds while the value is unchanged from the
	// prior observation (RUL-023 case 2 with `for`).
	return reflect.DeepEqual(av, prevAttr)
}

// numericLevelHolds reports whether a numeric trigger's bounds are currently
// satisfied (RUL-032, strictly exclusive at both endpoints), reusing the same
// parsing (RUL-270) the trigger's own satisfaction uses. An unparseable value
// never satisfies (RUL-031/271).
func (e *Engine) numericLevelHolds(tr *triggerRuntime, ent state.Entity) bool {
	var v any
	if tr.nt.Attribute != "" {
		v = ent.Attributes[tr.nt.Attribute]
	} else {
		v = ent.State
	}
	f, ok := eval.ParseNumber(v)
	if !ok {
		return false
	}
	if tr.nt.HasAbove && !(f > tr.nt.Above) {
		return false
	}
	if tr.nt.HasBelow && !(f < tr.nt.Below) {
		return false
	}
	return true
}

// fire resolves one trigger firing against the rule's conditions and mode
// (RUL-004/241/242/246), returning the disposition(s) that firing produces.
//
// A firing whose conditions (implicit AND, RUL-004) do not all pass never
// reaches mode evaluation: it starts no run and records no disposition.
//
// A firing that passes its conditions is resolved by the rule's mode:
//   - no run in flight: a fresh run starts and the firing records `ran`.
//   - single (RUL-241): while a run is in progress the firing is dropped —
//     no new run starts, the in-flight run is untouched — and records
//     `skipped`. This is the default when `mode` is omitted.
//   - restart (RUL-242): while a run is in progress the firing cancels that
//     run (discarding its pending `delay`/`for`-hold), which records
//     `restarted`, and starts a fresh run from the top with timers zeroed —
//     the fresh run records `ran`. Both dispositions are returned, the
//     canceled run's first.
//
// Every firing that reaches this point resolves to exactly one disposition per
// run it affects (RUL-246): the closed set is `ran`, `skipped`, `restarted`.
// (queued/parallel are app-class and never reach this edge engine, RUL-240.)
func (e *Engine) fire() []RunDisposition {
	if !e.conditionsPass() {
		return nil
	}
	if e.run != nil {
		switch e.mode {
		case "restart":
			// Cancel the in-flight run, discarding its pending delay/hold
			// (RUL-242), then start a fresh run whose own timers start at zero
			// from the current monotonic instant.
			e.run = nil
			restarted := RunDisposition{RuleID: e.ruleID, Disposition: Restarted, Mode: e.mode}
			e.startRun(e.rule.Actions)
			return []RunDisposition{restarted, {RuleID: e.ruleID, Disposition: Ran, Mode: e.mode}}
		default: // single (RUL-241)
			// Drop the firing; the in-flight run is left running untouched.
			return []RunDisposition{{RuleID: e.ruleID, Disposition: Skipped, Mode: e.mode}}
		}
	}
	e.startRun(e.rule.Actions)
	return []RunDisposition{{RuleID: e.ruleID, Disposition: Ran, Mode: e.mode}}
}

// conditionsPass evaluates the rule's conditions array as an implicit AND
// (RUL-004): every entry must pass; an empty array always passes. Each entry is
// evaluated against the engine's current snapshot (RUL-101).
func (e *Engine) conditionsPass() bool {
	for _, c := range e.rule.Conditions {
		if !eval.EvalCondition(e.reg, e.snap, e.clk, nil, c) {
			return false
		}
	}
	return true
}

// startRun executes an action sequence, capturing a `delay`'s deferred tail as
// the single in-flight run when one is reached (RUL-190). A sequence that runs
// to completion leaves no in-flight run.
func (e *Engine) startRun(actions []model.Member) {
	err := eval.RunActions(e.actionContext(), actions)
	var dp *eval.DelayPending
	if errors.As(err, &dp) {
		e.run = &runState{pending: dp}
		return
	}
	e.run = nil
}

// resumeDelay resumes the in-flight run's deferred tail once the monotonic
// resume instant has been reached (RUL-190). If the tail hits another delay it
// is re-deferred; otherwise the run completes.
func (e *Engine) resumeDelay(now clock.Clock) {
	if e.run == nil || e.run.pending == nil {
		return
	}
	if now.Mono() < e.run.pending.ResumeAtMono {
		return
	}
	tail := e.run.pending.RemainingActions
	err := eval.RunActions(e.actionContext(), tail)
	var dp *eval.DelayPending
	if errors.As(err, &dp) {
		e.run.pending = dp
		return
	}
	e.run = nil
}

// actionContext assembles the ActionContext RunActions executes against. This
// part wires no selector/device_class fan-out resolver (entity_id targets
// only) and no outcome/log recording slots; later parts add them.
func (e *Engine) actionContext() eval.ActionContext {
	return eval.ActionContext{
		Reg:     e.reg,
		Snap:    e.snap,
		Clk:     e.clk,
		Vars:    nil,
		Sink:    e.sink,
		Presets: e.presets,
	}
}
