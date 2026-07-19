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
	"github.com/maaxton/waiveo-next/internal/rules/closure"
	"github.com/maaxton/waiveo-next/internal/rules/compile"
	"github.com/maaxton/waiveo-next/internal/rules/eval"
	"github.com/maaxton/waiveo-next/internal/rules/expr"
	"github.com/maaxton/waiveo-next/internal/rules/model"
	"github.com/maaxton/waiveo-next/internal/rules/registry"
	"github.com/maaxton/waiveo-next/internal/rules/schedule"
	"github.com/maaxton/waiveo-next/internal/rules/state"
)

// Disposition is the closed outcome a firing (or a dropped/preempted firing)
// resolves to for mode evaluation (RUL-246): `ran`, `skipped`, or `restarted`.
// Canceled is the orthogonal generation-swap outcome (RUL-380): it is not a
// firing outcome but the record that an in-flight run was torn down when a
// changed or removed rule's generation was superseded.
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
	// Canceled — the in-flight run of a rule that was changed or removed by a
	// generation swap (RUL-380/381), torn down when ApplyGeneration superseded
	// the generation it belonged to. Unlike ran/skipped/restarted it does not
	// arise from a trigger firing; ApplyGeneration is its only producer.
	Canceled Disposition = "canceled"
)

// RunDisposition is the recorded outcome of one trigger firing that reached
// mode evaluation (RUL-246, Wire shapes) — the record from which the relay's
// events/1 automation.run event is emitted (EVT-040/041). RuleID, Disposition,
// Mode, and MisfireCaught are the mode-evaluation outcome; MisfireCaught is the
// orthogonal marker RUL-355 sets on a caught-up misfired firing (a later part),
// always false here.
//
// RuleRevision, TriggerSnapshot, ConditionResults, and ActionOutcomes are the
// remaining automation.run payload context EVT-040 requires (the applied rule
// revision, the firing trigger, and the condition/action records of the run).
// They are carried as the firing's own events/1 JSON — opaque here, so the
// disposition record does not redefine the events/1 schema (REL-095). The
// firing path that assembles a run's trigger/condition/action record populates
// them (a later part wires them from live evaluation); the bare mode-evaluation
// dispositions this part produces leave them empty.
type RunDisposition struct {
	RuleID           string          `json:"rule_id"`
	Disposition      Disposition     `json:"disposition"`
	Mode             string          `json:"mode"`
	MisfireCaught    bool            `json:"misfire_caught"`
	RuleRevision     int             `json:"rule_revision"`
	TriggerSnapshot  json.RawMessage `json:"trigger_snapshot"`
	ConditionResults json.RawMessage `json:"condition_results"`
	ActionOutcomes   json.RawMessage `json:"action_outcomes"`
}

// Engine drives the rules of one applied generation. It is not safe for
// concurrent use: Observe and Tick advance every rule's trigger/hold/run state
// serially, as a relay's single evaluation loop drives them.
type Engine struct {
	reg     registry.Registry
	clk     clock.Clock
	sink    eval.CommandSink
	presets eval.PresetStore

	// rules are the driven rule instances of the applied generation (RUL-390);
	// gen is that generation's number. Load applies a one-rule generation, so
	// the single-rule path is just rules with one element.
	rules []*ruleInstance
	gen   int

	// snap is the engine's running view of every entity it has observed, the
	// point-in-time snapshot RUL-101 condition evaluation resolves against.
	snap state.Snapshot

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

	// clockUntrusted is the engine's clock trust state (RUL-370/371): false is
	// `trusted` (the default), true is `untrusted`. While untrusted the wall clock
	// is unverified, so schedule triggers do not fire live and time-based conditions
	// evaluate against the persisted floor rather than the wall (see effectiveClk /
	// dispatchSchedule); the untrusted-window occurrences are replayed through each
	// trigger's misfire policy on the untrusted->trusted transition (SetClockTrust).
	clockUntrusted bool

	// stabWindowMs is the stabilization window (RUL-301) in monotonic
	// milliseconds: the bounded, fallback-timed span a state/numeric trigger made
	// newly eligible by a readiness point defers its first dispatch through. Zero
	// disables the gate (the effective default) — see SetStabilizationWindow and
	// stabilization.go.
	stabWindowMs int64
}

// runState is the single in-flight run's continuation: the flattened tail of
// actions a `delay` deferred, the monotonic instant at which to resume it
// (RUL-190), and the firing trigger's subject entity that scopes the run's
// Expressions (RUL-282) so a resumed tail live-evaluates its `log`/`params`
// Expressions against the same edge scope as the firing that deferred it.
type runState struct {
	pending *eval.DelayPending
	scope   string
}

// triggerRuntime is one trigger's per-(trigger,entity) evaluation state: the
// decoded trigger, its `for`-hold, and the durable baselines Observe diffs the
// next observation against. This part supports the entity_id form only; the
// selector/device_class fan-out forms are a later concern.
type triggerRuntime struct {
	kind     string // "state" | "numeric" | "time" | "time_pattern" | "sun"
	entityID string

	// sig is this trigger's per-(trigger,entity) canonical fingerprint (RUL-303/
	// 304): the canonical JSON of its authored member plus its resolved subject
	// entity. ApplyGeneration matches an incoming trigger against a prior one by
	// this signature to decide, at trigger granularity, whether the trigger is
	// unchanged across a generation swap and so carries its first-observation
	// baseline forward, or is new/changed and resets it (RUL-381 at the trigger
	// level). Two selector-matched entities of one member differ only by their
	// entity, so each carries its own signature and its own first-observation
	// state (RUL-011/304).
	sig string

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

	// stabilization window (RUL-301), per (trigger, entity). stabArmed marks the
	// trigger as gated by a window opened at a readiness point (a generation-apply
	// that made it new/changed, or a fresh engine's first apply — an engine
	// restart); stabDeadline is the monotonic instant that window elapses; a fire
	// arriving while gated sets stabPending, held until the window elapses (on a
	// Tick, or disarmed lazily by an Observe past the deadline). See
	// stabilization.go. Unarmed (the default and the elapsed state) never gates.
	stabArmed    bool
	stabDeadline int64
	stabPending  bool

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
// Load is a thin wrapper over ApplyGeneration of a one-rule generation carrying
// an empty closure (its caller performs no compile-time resolution) — it
// replaces any previously applied generation and its in-flight state.
func (e *Engine) Load(entry compile.CompiledRuleEntry, rule model.Rule) error {
	if entry.ExecutionClass != "edge" {
		return errors.New("engine: refusing to load a non-edge rule (execution_class=" + entry.ExecutionClass + ")")
	}
	e.ApplyGeneration(Generation{
		Number: e.gen + 1,
		Rules:  []CompiledRule{{Entry: entry, Rule: rule, Closure: closure.Closure{}}},
	})
	return nil
}

// newTriggerRuntime builds the runtime for one trigger member, or nil for a
// trigger kind this evaluation core does not drive, a state/numeric trigger
// lacking an entity_id subject, or a malformed wall-clock spec. State/numeric
// triggers fire from Observe; `time`/`time_pattern`/`sun` triggers carry no
// EntityRef and fire from Tick via their schedule enumerator (RUL-040/050/060/340).
func newTriggerRuntime(m model.Member) *triggerRuntime {
	switch m.Type {
	case "state", "numeric":
		// entity_id form: the subject comes from the member's own EntityRef.
		return newStateOrNumericRuntime(m, "")
	}
	// A schedule trigger carries no EntityRef (RUL-040/050/060), so its
	// per-(trigger,entity) signature is keyed on an empty subject (RUL-303).
	tr := &triggerRuntime{kind: m.Type, forSeconds: parseForSeconds(m.Raw), sig: triggerSig(m.Raw, "")}
	switch m.Type {
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
}

// newStateOrNumericRuntime builds a state/numeric trigger's per-(trigger,entity)
// runtime. overrideEntity, when non-empty, is the subject a selector/device-class
// fan-out (RUL-011/013) resolved from the frozen closure — it supersedes the
// member's own (empty) entity_id; an empty overrideEntity uses the member's
// entity_id form. Returns nil for a malformed trigger or one with no resolvable
// subject.
func newStateOrNumericRuntime(m model.Member, overrideEntity string) *triggerRuntime {
	tr := &triggerRuntime{kind: m.Type, forSeconds: parseForSeconds(m.Raw)}
	switch m.Type {
	case "state":
		st, err := eval.NewStateTrigger(m)
		if err != nil {
			return nil
		}
		tr.entityID = overrideEntity
		if tr.entityID == "" {
			tr.entityID = st.EntityID
		}
		if tr.entityID == "" {
			return nil
		}
		tr.st = st
		// RUL-023: an unscoped state trigger (no attribute, no from/to) debounces
		// its `for` (RUL-026); cases 1-3 are bounded level holds (RUL-024).
		if st.Attribute == "" && !st.HasFrom && !st.HasTo {
			tr.holdKind = eval.DebounceHold
		} else {
			tr.holdKind = eval.BoundedHold
		}
	case "numeric":
		nt, err := eval.NewNumericTrigger(m)
		if err != nil {
			return nil
		}
		tr.entityID = overrideEntity
		if tr.entityID == "" {
			tr.entityID = nt.EntityID
		}
		if tr.entityID == "" {
			return nil
		}
		tr.nt = nt
		tr.holdKind = eval.BoundedHold // every numeric trigger is bounded (RUL-033)
	default:
		return nil
	}
	tr.hold = eval.NewHold(tr.holdKind, tr.forSeconds)
	// Key the trigger's first-observation signature on its authored member plus
	// its resolved subject entity (the selector/device-class fan-out's per-entity
	// override, or the member's own entity_id) so an unchanged (trigger,entity)
	// carries its baseline across a generation swap (RUL-303/304).
	tr.sig = triggerSig(m.Raw, tr.entityID)
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
	for _, ri := range e.rules {
		for _, tr := range ri.triggers {
			if tr.entityID != entityID {
				continue
			}
			e.seedTriggerBaseline(tr, st, cur)
		}
	}
}

// seedTriggerBaseline seeds one (trigger,entity) runtime's durable baseline from
// a pre-restart / self-describing state value (RUL-304), and — for a bounded
// state hold — its remembered level so a boot reconfirm of unchanged durable
// state is not read as a fresh rising edge (RUL-300/360).
func (e *Engine) seedTriggerBaseline(tr *triggerRuntime, st string, cur state.Entity) {
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

// Observe advances the engine by one entity observation: it updates the
// snapshot, runs every trigger whose subject is the observed entity, and — for
// each trigger that fires — evaluates the rule's conditions (RUL-004) and, when
// they pass, starts a run of the action sequence. It returns the dispositions
// of any firings that reached mode evaluation.
func (e *Engine) Observe(obs state.Observation) []RunDisposition {
	if len(e.rules) == 0 {
		return nil
	}
	e.snap[obs.Entity.ID] = obs.Entity

	var out []RunDisposition
	for _, ri := range e.rules {
		for _, tr := range ri.triggers {
			if tr.entityID != obs.Entity.ID {
				continue
			}
			if e.stepTriggerObservation(tr, obs) {
				out = append(out, e.dispatchFire(ri, tr)...)
			}
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
	if len(e.rules) == 0 {
		return nil
	}
	var out []RunDisposition
	for _, ri := range e.rules {
		for _, tr := range ri.triggers {
			if tr.forSeconds <= 0 || tr.hold == nil {
				continue // nothing to advance between observations
			}
			if e.stepTriggerTick(tr, now) {
				out = append(out, e.dispatchFire(ri, tr)...)
			}
		}
	}
	// Release any stabilization-held fire whose window has now elapsed (RUL-301):
	// the bounded, fallback-timed gate opens even if no further readiness signal
	// arrives, so a held transition dispatches once its monotonic deadline passes.
	out = append(out, e.releaseStabilized()...)
	out = append(out, e.dispatchSchedule(now)...)
	for _, ri := range e.rules {
		e.resumeDelay(ri, now)
	}
	return out
}

// dispatchSchedule advances the wall-clock (schedule) triggers on a Tick.
//
// The FIRST Tick after the engine (re)starts — no live evaluation has happened
// yet, so lastTickWall is still zero — is a RESUME, not a live tick: across the
// whole span from the persisted trust floor up to now the engine was down and
// could not evaluate at any scheduled instant (RUL-350), so every occurrence in
// it is a MISSED occurrence caught up through its trigger's declared `misfire`
// policy via replayMissedSchedules — `skip` (and the "" default) fires nothing (a
// missed one-shot does not fire late, RUL-351/354), `catch_up_once` collapses to
// one, `fire_each` replays each, and every caught-up fire carries misfire_caught
// (RUL-352/353/355). This is precisely why a `skip` schedule missed across
// downtime never fires live after the fact.
//
// Every SUBSEQUENT Tick is a live tick: the engine has been evaluating
// continuously, so the occurrences enumerated over the half-open interval
// (max(lastTickWall, floor), nowWall] fire live through fire() with
// misfire_caught=false (RUL-041/051/340) — a `skip` schedule therefore fires
// normally at its instant while the engine is up, the misfire policy governing
// only what was missed while it was down. The interval's lower bound is the
// last-evaluated instant (or the floor, whichever is later, so nothing rests on a
// clock reading earlier than the floor, RUL-370) and its upper bound the current
// wall reading; the wall cursor then advances to nowWall so the next Tick
// continues with no gap or overlap. When no schedule trigger is registered the
// wall clock is not read and the cursor is left untouched.
func (e *Engine) dispatchSchedule(now clock.Clock) []RunDisposition {
	hasSchedule := false
	for _, ri := range e.rules {
		for _, tr := range ri.triggers {
			if tr.sched != nil {
				hasSchedule = true
				break
			}
		}
	}
	if !hasSchedule {
		return nil
	}

	// While the clock is untrusted the wall reading is unverified (RUL-370): no
	// schedule occurrence fires live and the wall clock is not even read, so the
	// cursor does not advance. Every occurrence whose instant falls in the untrusted
	// window is instead replayed through its misfire policy on the untrusted->trusted
	// transition (SetClockTrust, RUL-371) — never a live tick, never a silent drop.
	if e.clockUntrusted {
		return nil
	}

	nowWall := now.WallMillis()

	// First Tick after (re)start: catch the downtime span up through the misfire
	// policy rather than firing every missed occurrence live (RUL-350/351/354).
	// replayMissedSchedules clamps the lower bound up to the floor (RUL-370) and
	// advances the wall cursor, so subsequent Ticks resume live from here.
	if e.lastTickWall == 0 {
		return e.replayMissedSchedules(e.scheduleFloor, nowWall)
	}

	from := e.lastTickWall
	if e.scheduleFloor > from {
		from = e.scheduleFloor
	}

	var out []RunDisposition
	for _, ri := range e.rules {
		for _, tr := range ri.triggers {
			if tr.sched == nil {
				continue
			}
			for range tr.sched.Occurrences(e.loc, from, nowWall) {
				// Schedule firing: no entity subject, empty edge scope (RUL-282).
				out = append(out, e.fireRule(ri, "")...)
			}
		}
	}
	// The wall cursor only ever advances forward. A trusted backward wall step
	// (routine NTP/admin correction on RTC-less appliances) must NOT regress the
	// cursor: a later recovery Tick would otherwise re-enumerate the intervening
	// span and re-fire occurrences that already fired once (RUL-041/RUL-051 — a
	// schedule occurrence fires once per its instant). Mirrors the forward-only
	// guard in replayMissedSchedules.
	if nowWall > e.lastTickWall {
		e.lastTickWall = nowWall
	}
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

// fireRule resolves one trigger firing of rule instance ri against ri's
// conditions and mode (RUL-004/241/242/246), returning the disposition(s) that
// firing produces.
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
func (e *Engine) fireRule(ri *ruleInstance, scope string) []RunDisposition {
	return e.fireRuleWith(ri, scope, false)
}

// fireRuleWith is fireRule's implementation with the orthogonal misfire_caught
// marker threaded through (RUL-355): a live observation fires with
// misfireCaught=false, a caught-up misfired occurrence (catch_up_once/fire_each,
// dispatchMisfire) with misfireCaught=true. The marker is stamped on every
// disposition this firing produces — it records how the firing arose, orthogonal
// to whichever mode disposition (`ran`/`skipped`/`restarted`) each records — so a
// caught-up fire dropped by single or preempting under restart still carries it
// (RUL-246/355).
func (e *Engine) fireRuleWith(ri *ruleInstance, scope string, misfireCaught bool) []RunDisposition {
	if !e.conditionsPass(ri) {
		return nil
	}
	var out []RunDisposition
	switch {
	case ri.run != nil && ri.mode == "restart":
		// Cancel the in-flight run, discarding its pending delay/hold (RUL-242),
		// then start a fresh run whose own timers start at zero from the current
		// monotonic instant.
		ri.run = nil
		restarted := RunDisposition{RuleID: ri.ruleID, Disposition: Restarted, Mode: ri.mode}
		e.startRun(ri, ri.rule.Actions, scope)
		out = []RunDisposition{restarted, {RuleID: ri.ruleID, Disposition: Ran, Mode: ri.mode}}
	case ri.run != nil:
		// single (RUL-241): drop the firing; the in-flight run is left untouched.
		out = []RunDisposition{{RuleID: ri.ruleID, Disposition: Skipped, Mode: ri.mode}}
	default:
		e.startRun(ri, ri.rule.Actions, scope)
		out = []RunDisposition{{RuleID: ri.ruleID, Disposition: Ran, Mode: ri.mode}}
	}
	if misfireCaught {
		for i := range out {
			out[i].MisfireCaught = true
		}
	}
	return out
}

// replayMissedSchedules re-evaluates each schedule trigger over a window in which
// the engine could not evaluate at the scheduled instant — the first-Tick resume
// span after a (re)start (dispatchSchedule), or (Task 5) an untrusted-clock
// window — treating every occurrence in
// (fromExclusive, toInclusive] as a MISSED occurrence governed by that trigger's
// declared `misfire` policy (RUL-350/371), never a silent drop nor an ordinary
// live tick. The window's lower bound is clamped up to the persisted trust floor
// so no caught-up fire ever rests on a clock reading earlier than the floor
// (RUL-370). The wall cursor advances to toInclusive so a subsequent live Tick
// does not re-enumerate these same occurrences.
func (e *Engine) replayMissedSchedules(fromExclusive, toInclusive int64) []RunDisposition {
	if len(e.rules) == 0 {
		return nil
	}
	from := fromExclusive
	if e.scheduleFloor > from {
		from = e.scheduleFloor
	}
	var out []RunDisposition
	for _, ri := range e.rules {
		for _, tr := range ri.triggers {
			if tr.sched == nil {
				continue
			}
			missed := tr.sched.Occurrences(e.loc, from, toInclusive)
			out = append(out, e.dispatchMisfire(ri, tr, missed)...)
		}
	}
	if toInclusive > e.lastTickWall {
		e.lastTickWall = toInclusive
	}
	return out
}

// dispatchMisfire applies one schedule trigger's declared misfire policy to its
// missed occurrences (RUL-350–354) and routes each resulting Fire through the
// firing path, stamping misfire_caught (RUL-355). `skip` (and the "" default,
// RUL-354) yields no Fire; `catch_up_once` collapses to one; `fire_each` dispatches
// each in chronological order — each independently subject to full mode evaluation
// like any other firing (RUL-353/355), so an earlier caught-up dispatch can leave a
// later one `skipped`/`restarted` under a busy single/restart mode.
func (e *Engine) dispatchMisfire(ri *ruleInstance, tr *triggerRuntime, missed []int64) []RunDisposition {
	var out []RunDisposition
	for _, f := range schedule.ApplyMisfire(tr.misfire, missed) {
		// A schedule trigger has no entity subject (RUL-040/050/060), so its
		// firing carries an empty edge scope (RUL-282).
		out = append(out, e.fireRuleWith(ri, "", f.MisfireCaught)...)
	}
	return out
}

// conditionsPass evaluates rule instance ri's conditions array as an implicit
// AND (RUL-004): every entry must pass; an empty array always passes. Each entry
// is evaluated against the engine's current snapshot (RUL-101); a `variable`
// condition reads its comparison value from ri's frozen closure — the
// compile-time value substituted as a constant (RUL-150/391), never a live
// lookup.
func (e *Engine) conditionsPass(ri *ruleInstance) bool {
	for _, c := range ri.rule.Conditions {
		if !eval.EvalConditionAt(e.reg, e.snap, e.effectiveClk(), e.loc, ri.closure.Variables, c) {
			return false
		}
	}
	return true
}

// startRun executes rule instance ri's action sequence, capturing a `delay`'s
// deferred tail as ri's single in-flight run when one is reached (RUL-190). A
// sequence that runs to completion leaves no in-flight run.
func (e *Engine) startRun(ri *ruleInstance, actions []model.Member, scope string) {
	err := eval.RunActions(e.actionContext(ri, scope), actions)
	var dp *eval.DelayPending
	if errors.As(err, &dp) {
		ri.run = &runState{pending: dp, scope: scope}
		return
	}
	ri.run = nil
}

// resumeDelay resumes rule instance ri's deferred tail once the monotonic resume
// instant has been reached (RUL-190). If the tail hits another delay it is
// re-deferred; otherwise the run completes.
func (e *Engine) resumeDelay(ri *ruleInstance, now clock.Clock) {
	if ri.run == nil || ri.run.pending == nil {
		return
	}
	if now.Mono() < ri.run.pending.ResumeAtMono {
		return
	}
	tail := ri.run.pending.RemainingActions
	err := eval.RunActions(e.actionContext(ri, ri.run.scope), tail)
	var dp *eval.DelayPending
	if errors.As(err, &dp) {
		ri.run.pending = dp
		return
	}
	ri.run = nil
}

// actionContext assembles the ActionContext RunActions executes rule instance
// ri's actions against. Its frozen closure drives the action side (RUL-390):
// Vars supplies a nested `choose` guard's `variable` value (RUL-150), Resolve
// expands a selector/device-class device_command over the frozen matched set
// (RUL-011/012/013), and Presets dispatches a preset_batch's frozen command list
// (RUL-173). EvalExpr, by contrast, is the LIVE side (RUL-393): a `log` message
// (RUL-200) and each `params` value are Expressions evaluated at dispatch, never
// frozen — scoped to the firing trigger's subject entity (RUL-282). scope is
// that subject ("" for a subjectless schedule firing, whose Expressions may then
// reference no entity — every state/attr source is out of scope and fails
// closed). No outcome/log recording slots are wired here; a later part adds them.
func (e *Engine) actionContext(ri *ruleInstance, scope string) eval.ActionContext {
	return eval.ActionContext{
		Reg:      e.reg,
		Snap:     e.snap,
		Clk:      e.effectiveClk(),
		Loc:      e.loc,
		Vars:     ri.closure.Variables,
		Sink:     e.sink,
		Presets:  closurePresetStore{frozen: ri.closure.PresetBatches, live: e.presets},
		Resolve:  closureResolver(ri.closure),
		EvalExpr: e.exprEvaluator(scope),
	}
}

// exprEvaluator builds the live Expression evaluator the action context routes a
// `log` message (RUL-200) and each `params` value (RUL-393) through, closing over
// the current snapshot/registry/clock and the firing trigger's subject entity as
// the edge scope (RUL-282). Every rule this engine drives is edge-classified
// (Load refuses non-edge, RUL-240), so evaluation is always edge-scoped: a
// state/attr source referencing any entity other than scope fails closed
// (defense in depth behind compile.Validate's EDGE_EXPRESSION_CROSS_ENTITY_REFERENCE
// check, RUL-282/284). now() is subject to the clock-trust floor exactly as the
// engine's condition/schedule clock is (RUL-370/275): while untrusted the wall is
// unverified and now() reports the persisted floor. A parse failure or a
// fail-closed evaluation (RUL-284) reports ok=false, and the caller skips the
// field.
func (e *Engine) exprEvaluator(scope string) eval.ExprEvaluator {
	return func(raw json.RawMessage) (any, bool) {
		parsed, perr := expr.Parse(raw)
		if perr != nil {
			return nil, false
		}
		val, failed := expr.Evaluate(parsed, expr.Env{
			Snapshot:            e.snap,
			Reg:                 e.reg,
			Clk:                 e.clk,
			ClockUntrustedFloor: e.scheduleFloor,
			TrustUntrusted:      e.clockUntrusted,
			EdgeScoped:          true,
			ScopeEntity:         scope,
		})
		if failed {
			return nil, false
		}
		return val, true
	}
}

// closureResolver expands a device-affecting action's selector/device-class
// EntityRef to its frozen matched entity set (RUL-011/012/013), keyed by the
// filter string exactly as closure.Compute froze it. An entity_id ref resolves
// to itself; an unfrozen filter resolves to no targets (fail-closed).
func closureResolver(cl closure.Closure) func(ref model.EntityRef) []string {
	return func(ref model.EntityRef) []string {
		if ref.EntityID != "" {
			return []string{ref.EntityID}
		}
		key := ref.Selector
		if key == "" {
			key = ref.DeviceClass
		}
		return cl.Selectors[key]
	}
}
