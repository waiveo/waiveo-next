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
	"time"

	"github.com/maaxton/waiveo-next/internal/rules/clock"
	"github.com/maaxton/waiveo-next/internal/rules/compile"
	"github.com/maaxton/waiveo-next/internal/rules/eval"
	"github.com/maaxton/waiveo-next/internal/rules/model"
	"github.com/maaxton/waiveo-next/internal/rules/registry"
	"github.com/maaxton/waiveo-next/internal/rules/schedule"
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

	// loc is the effective evaluation environment for wall-clock (schedule)
	// triggers: the owning scope node's timezone (RUL-340) and the geographic
	// coordinates the `sun` astronomy reads (RUL-060/061). It is engine-level
	// configuration set via SetLocation, independent of any one loaded rule, so
	// Load does not clear it. The zero Location (nil TZ) means "not yet set".
	loc schedule.Location

	// lastTickWall is the last wall instant Tick evaluated schedule triggers
	// through: the exclusive lower bound of the next Tick's enumeration interval.
	// It advances only forward to each Tick's wall reading (RUL-340).
	lastTickWall int64

	// scheduleFloor is the persisted best-known time floor (RUL-370): a schedule
	// occurrence at or below it is never enumerated, so no firing rests on a
	// clock reading earlier than the floor. It is the resume cursor after
	// downtime — SeedScheduleFloor supplies the last-evaluated instant.
	scheduleFloor int64
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
	kind     string // "state" | "numeric" | "time" | "time_pattern" | "sun"
	entityID string

	st *eval.StateTrigger
	nt *eval.NumericTrigger

	// sched is the wall-clock occurrence enumerator for a schedule trigger
	// ("time"/"time_pattern"/"sun"), nil for a state/numeric trigger. Tick
	// enumerates its occurrences over the elapsed wall interval and routes each
	// through fire() (RUL-041/051/340). misfire is its declared misfire policy
	// (RUL-350), applied by a later task; unset until then.
	sched   schedule.ScheduleTrigger
	misfire string

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
// trigger kind this evaluation core does not drive, a state/numeric trigger
// lacking an entity_id subject, or a malformed wall-clock spec. State/numeric
// triggers fire from Observe; `time`/`time_pattern`/`sun` triggers carry no
// EntityRef and fire from Tick via their schedule enumerator (RUL-040/050/060/340).
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
	case "time":
		// A `time` trigger carries no EntityRef (RUL-040): it fires from the wall
		// clock through Tick, never from an observation. A malformed `at` drops
		// the trigger (fail-closed) rather than firing on garbage.
		tt, err := schedule.NewTimeTrigger(scheduleTimeAt(m.Raw))
		if err != nil {
			return nil
		}
		tr.sched = tt
		tr.misfire = parseMisfire(m.Raw)
		return tr
	case "time_pattern":
		// A `time_pattern` trigger carries no EntityRef (RUL-050) and fires from
		// the wall clock through Tick. Its component fields are number-or-string on
		// the wire; scheduleTimePatternFields canonicalizes each to the string form
		// schedule.NewTimePatternTrigger parses. A malformed spec drops the trigger.
		h, mi, s := scheduleTimePatternFields(m.Raw)
		pt, err := schedule.NewTimePatternTrigger(h, mi, s)
		if err != nil {
			return nil
		}
		tr.sched = pt
		tr.misfire = parseMisfire(m.Raw)
		return tr
	case "sun":
		// A `sun` trigger carries no EntityRef (RUL-060): it fires from the wall
		// clock through Tick, its instants computed by the solar algorithm over
		// the engine's effective latitude/longitude (SetLocation). A malformed
		// event drops the trigger (fail-closed) rather than firing on garbage.
		event, offset := scheduleSunSpec(m.Raw)
		st, err := schedule.NewSunTrigger(event, offset)
		if err != nil {
			return nil
		}
		tr.sched = st
		tr.misfire = parseMisfire(m.Raw)
		return tr
	default:
		return nil
	}
	tr.hold = eval.NewHold(tr.holdKind, tr.forSeconds)
	return tr
}

// scheduleTimeAt reads a `time` trigger's `at` local time-of-day (RUL-040).
func scheduleTimeAt(raw json.RawMessage) string {
	var spec struct {
		At string `json:"at"`
	}
	_ = json.Unmarshal(raw, &spec)
	return spec.At
}

// scheduleTimePatternFields reads a `time_pattern` trigger's three component
// fields (RUL-050) and canonicalizes each to its string form: an omitted (or
// null) field is "", a JSON string ("/N" or "N") is taken verbatim, and a JSON
// number is rendered as its decimal digits. This lets schedule.parsePatternField
// treat the exact-int and `/N`-divisor wire forms uniformly.
func scheduleTimePatternFields(raw json.RawMessage) (hours, minutes, seconds string) {
	var spec struct {
		Hours   json.RawMessage `json:"hours"`
		Minutes json.RawMessage `json:"minutes"`
		Seconds json.RawMessage `json:"seconds"`
	}
	_ = json.Unmarshal(raw, &spec)
	return canonPatternField(spec.Hours), canonPatternField(spec.Minutes), canonPatternField(spec.Seconds)
}

// canonPatternField normalizes one raw time_pattern component to its string form
// (see scheduleTimePatternFields): "" for absent/null, the unquoted contents of a
// JSON string, or a JSON number verbatim.
func canonPatternField(raw json.RawMessage) string {
	s := string(raw)
	if s == "" || s == "null" {
		return ""
	}
	if len(s) > 0 && s[0] == '"' {
		var str string
		if err := json.Unmarshal(raw, &str); err != nil {
			return ""
		}
		return str
	}
	return s
}

// scheduleSunSpec reads a `sun` trigger's event and optional offset (RUL-060):
// `event` is "sunrise"/"sunset" and `offset` is an integer number of seconds
// (positive or negative), absent decoding as 0.
func scheduleSunSpec(raw json.RawMessage) (event string, offset int) {
	var spec struct {
		Event  string `json:"event"`
		Offset int    `json:"offset"`
	}
	_ = json.Unmarshal(raw, &spec)
	return spec.Event, spec.Offset
}

// parseMisfire reads a schedule trigger's optional `misfire` policy (RUL-350).
// The policy is applied by a later task; wiring it here keeps the trigger runtime
// complete. An absent field decodes as "" (the default policy, resolved later).
func parseMisfire(raw json.RawMessage) string {
	var spec struct {
		Misfire string `json:"misfire"`
	}
	_ = json.Unmarshal(raw, &spec)
	return spec.Misfire
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

// SetLocation sets the engine's effective evaluation environment for wall-clock
// (schedule) triggers: the timezone by IANA name (RUL-340, resolved through
// time.LoadLocation against the host tz database) and the geographic latitude/
// longitude (decimal degrees, north/east positive) the `sun` astronomy reads
// (RUL-060/061). It returns time.LoadLocation's error for an unknown zone name,
// leaving the previous location unchanged. Location is engine-level configuration
// and may be set before or after Load; Load does not reset it.
func (e *Engine) SetLocation(tzName string, lat, lon float64) error {
	tz, err := time.LoadLocation(tzName)
	if err != nil {
		return err
	}
	e.loc = schedule.Location{TZ: tz, Lat: lat, Lon: lon}
	return nil
}

// SeedScheduleFloor sets the persisted best-known time floor (RUL-370): the
// last-evaluated wall instant (Unix ms) below which no schedule occurrence is
// enumerated. It is how the caller supplies the resume cursor after downtime so
// occurrences missed while the engine was down are bounded from this instant
// rather than from the epoch, and so no firing ever rests on a clock reading
// earlier than the floor. The floor is the exclusive lower bound of the next
// Tick's enumeration interval when it exceeds the last Tick's wall cursor.
func (e *Engine) SeedScheduleFloor(wallMs int64) {
	e.scheduleFloor = wallMs
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
	out = append(out, e.dispatchSchedule(now)...)
	e.resumeDelay(now)
	return out
}

// dispatchSchedule enumerates each schedule trigger's nominal occurrences over
// the wall interval elapsed since the last Tick and routes each through fire()
// (RUL-041/051/340). The interval is half-open: (max(lastTickWall, floor),
// nowWall] — exclusive of the last-evaluated instant (or the persisted trust
// floor, whichever is later, so nothing rests on a clock reading earlier than
// the floor, RUL-370) and inclusive of the current wall reading. The wall cursor
// then advances to nowWall so the next Tick continues from here with no gap or
// overlap. When no schedule trigger is registered the wall clock is not read and
// the cursor is left untouched. Misfire collapse/replay and the misfire_caught
// marker layer onto this path in later tasks (RUL-350–355); here every live
// occurrence fires with misfire_caught=false.
func (e *Engine) dispatchSchedule(now clock.Clock) []RunDisposition {
	hasSchedule := false
	for _, tr := range e.triggers {
		if tr.sched != nil {
			hasSchedule = true
			break
		}
	}
	if !hasSchedule {
		return nil
	}

	nowWall := now.WallMillis()
	from := e.lastTickWall
	if e.scheduleFloor > from {
		from = e.scheduleFloor
	}

	var out []RunDisposition
	for _, tr := range e.triggers {
		if tr.sched == nil {
			continue
		}
		for range tr.sched.Occurrences(e.loc, from, nowWall) {
			out = append(out, e.fire()...)
		}
	}
	e.lastTickWall = nowWall
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
		if !eval.EvalConditionAt(e.reg, e.snap, e.clk, e.loc, nil, c) {
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
		Loc:     e.loc,
		Vars:    nil,
		Sink:    e.sink,
		Presets: e.presets,
	}
}
