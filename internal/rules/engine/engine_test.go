package engine

import (
	"testing"

	"github.com/maaxton/waiveo-next/internal/rules/clock"
	"github.com/maaxton/waiveo-next/internal/rules/compile"
	"github.com/maaxton/waiveo-next/internal/rules/model"
	"github.com/maaxton/waiveo-next/internal/rules/registry"
	"github.com/maaxton/waiveo-next/internal/rules/state"
)

const (
	subjectEntity = "01J8Z3K4N5P6Q7R8S9T0V1W2Z2"
	mediaPlayer   = "media-player"
)

// dispatched is one recorded CommandSink.Dispatch call.
type dispatched struct {
	EntityID string
	Command  string
	Params   map[string]any
}

// recordingSink is a CommandSink that records every dispatch and, optionally,
// fails a named target so partial-failure paths can be exercised.
type recordingSink struct {
	calls    []dispatched
	failFor  map[string]bool // entityID -> return an error
	failWith error
}

func (s *recordingSink) Dispatch(entityID, command string, params map[string]any) error {
	s.calls = append(s.calls, dispatched{EntityID: entityID, Command: command, Params: params})
	if s.failFor != nil && s.failFor[entityID] {
		return s.failWith
	}
	return nil
}

// ent builds a media-player Entity snapshot at the given state.
func ent(id, st string) state.Entity {
	return state.Entity{ID: id, DeviceClass: mediaPlayer, State: st}
}

// entAttr builds a media-player Entity snapshot with attributes.
func entAttr(id, st string, attrs map[string]any) state.Entity {
	return state.Entity{ID: id, DeviceClass: mediaPlayer, State: st, Attributes: attrs}
}

// compileRule compiles a rule JSON into its CompiledRuleEntry + parsed Rule,
// failing the test on any compile error.
func compileRule(t *testing.T, ruleJSON string) (compile.CompiledRuleEntry, model.Rule) {
	t.Helper()
	entry, cerr := compile.Compile([]byte(ruleJSON))
	if cerr != nil {
		t.Fatalf("compile: %v", cerr)
	}
	r, err := model.ParseRule([]byte(ruleJSON))
	if err != nil {
		t.Fatalf("parse rule: %v", err)
	}
	return entry, r
}

// --- Task 8: end-to-end run manager on the FakeClock ------------------------

// TestEndToEndDelayResumption is the named Task-8 corpus-shaped case: a state
// trigger -> empty-conditions rule -> [delay 20s, device_command]. Observe
// starts run-1 (delay pending, nothing dispatched yet); Tick past 20s of
// monotonic time resumes the tail and dispatches the command (RUL-004/190).
func TestEndToEndDelayResumption(t *testing.T) {
	reg := registry.FixtureRegistry{}
	clk := clock.NewFakeClock()
	sink := &recordingSink{}

	entry, rule := compileRule(t, `{
		"id":"01J8Z3K4N5P6Q7R8S9T0V1RULE",
		"triggers":[{"type":"state","entity_id":"`+subjectEntity+`","to":["on"]}],
		"actions":[
			{"type":"delay","duration_seconds":20},
			{"type":"device_command","entity_id":"`+subjectEntity+`","command":"power"}
		]
	}`)
	if entry.ExecutionClass != "edge" {
		t.Fatalf("rule should classify edge, got %q", entry.ExecutionClass)
	}

	e := New(reg, clk, sink, nil)
	if err := e.Load(entry, rule); err != nil {
		t.Fatalf("Load: %v", err)
	}

	// Prime: first observation establishes the baseline (off) and never fires
	// (RUL-300/302 first-observation suppression).
	if d := e.Observe(state.NewObservation(reg, ent(subjectEntity, "off"), ent(subjectEntity, "off"))); len(d) != 0 {
		t.Fatalf("priming observation fired: %+v", d)
	}

	// Genuine transition off -> on: the run starts and immediately hits the
	// delay, so nothing is dispatched yet.
	disps := e.Observe(state.NewObservation(reg, ent(subjectEntity, "off"), ent(subjectEntity, "on")))
	if len(disps) != 1 {
		t.Fatalf("transition: want 1 disposition, got %d (%+v)", len(disps), disps)
	}
	if disps[0].Disposition != Ran {
		t.Fatalf("transition disposition = %q, want ran", disps[0].Disposition)
	}
	if disps[0].RuleID != "01J8Z3K4N5P6Q7R8S9T0V1RULE" {
		t.Fatalf("disposition rule_id = %q", disps[0].RuleID)
	}
	if len(sink.calls) != 0 {
		t.Fatalf("command dispatched before delay elapsed: %+v", sink.calls)
	}

	// Tick before the delay elapses: still pending, no dispatch.
	clk.Advance(10_000)
	if d := e.Tick(clk); len(d) != 0 {
		t.Fatalf("early tick produced dispositions: %+v", d)
	}
	if len(sink.calls) != 0 {
		t.Fatalf("command dispatched mid-delay: %+v", sink.calls)
	}

	// Tick at/after 20s: the delay resumes and the tail dispatches.
	clk.Advance(10_000) // mono now 20_000 >= ResumeAtMono
	e.Tick(clk)
	if len(sink.calls) != 1 {
		t.Fatalf("want exactly 1 dispatch after delay, got %d (%+v)", len(sink.calls), sink.calls)
	}
	got := sink.calls[0]
	if got.EntityID != subjectEntity || got.Command != "power" {
		t.Fatalf("dispatched %+v, want power on %s", got, subjectEntity)
	}
}

// TestImmediateActionsNoDelay: a rule whose actions contain no delay dispatches
// synchronously within Observe (RUL-160).
func TestImmediateActionsNoDelay(t *testing.T) {
	reg := registry.FixtureRegistry{}
	clk := clock.NewFakeClock()
	sink := &recordingSink{}

	entry, rule := compileRule(t, `{
		"id":"RULEIMMEDIATE",
		"triggers":[{"type":"state","entity_id":"`+subjectEntity+`","to":["on"]}],
		"actions":[{"type":"device_command","entity_id":"`+subjectEntity+`","command":"power"}]
	}`)
	e := New(reg, clk, sink, nil)
	if err := e.Load(entry, rule); err != nil {
		t.Fatalf("Load: %v", err)
	}
	e.Observe(state.NewObservation(reg, ent(subjectEntity, "off"), ent(subjectEntity, "off")))
	disps := e.Observe(state.NewObservation(reg, ent(subjectEntity, "off"), ent(subjectEntity, "on")))
	if len(disps) != 1 || disps[0].Disposition != Ran {
		t.Fatalf("want one ran disposition, got %+v", disps)
	}
	if len(sink.calls) != 1 {
		t.Fatalf("want immediate dispatch, got %+v", sink.calls)
	}
}

// TestConditionsGateDispatch: a non-empty conditions array whose members do NOT
// all pass suppresses the run entirely (RUL-004 implicit-AND).
func TestConditionsGateDispatch(t *testing.T) {
	reg := registry.FixtureRegistry{}
	clk := clock.NewFakeClock()
	sink := &recordingSink{}

	// Condition requires the subject's state to be "off"; the triggering
	// transition leaves it "on", so the implicit-AND fails and no run starts.
	entry, rule := compileRule(t, `{
		"id":"RULEGATED",
		"triggers":[{"type":"state","entity_id":"`+subjectEntity+`","to":["on"]}],
		"conditions":[{"type":"state","entity_id":"`+subjectEntity+`","state":"off"}],
		"actions":[{"type":"device_command","entity_id":"`+subjectEntity+`","command":"power"}]
	}`)
	e := New(reg, clk, sink, nil)
	if err := e.Load(entry, rule); err != nil {
		t.Fatalf("Load: %v", err)
	}
	e.Observe(state.NewObservation(reg, ent(subjectEntity, "off"), ent(subjectEntity, "off")))
	disps := e.Observe(state.NewObservation(reg, ent(subjectEntity, "off"), ent(subjectEntity, "on")))
	if len(disps) != 0 {
		t.Fatalf("gated rule produced dispositions: %+v", disps)
	}
	if len(sink.calls) != 0 {
		t.Fatalf("gated rule dispatched: %+v", sink.calls)
	}
}

// TestConditionsPassDispatch: the mirror of the gate — a passing condition lets
// the run proceed (RUL-004/110).
func TestConditionsPassDispatch(t *testing.T) {
	reg := registry.FixtureRegistry{}
	clk := clock.NewFakeClock()
	sink := &recordingSink{}

	entry, rule := compileRule(t, `{
		"id":"RULEPASS",
		"triggers":[{"type":"state","entity_id":"`+subjectEntity+`","to":["on"]}],
		"conditions":[{"type":"state","entity_id":"`+subjectEntity+`","state":"on"}],
		"actions":[{"type":"device_command","entity_id":"`+subjectEntity+`","command":"power"}]
	}`)
	e := New(reg, clk, sink, nil)
	if err := e.Load(entry, rule); err != nil {
		t.Fatalf("Load: %v", err)
	}
	e.Observe(state.NewObservation(reg, ent(subjectEntity, "off"), ent(subjectEntity, "off")))
	disps := e.Observe(state.NewObservation(reg, ent(subjectEntity, "off"), ent(subjectEntity, "on")))
	if len(disps) != 1 || disps[0].Disposition != Ran {
		t.Fatalf("want ran, got %+v", disps)
	}
	if len(sink.calls) != 1 {
		t.Fatalf("want dispatch, got %+v", sink.calls)
	}
}

// TestLoadRefusesAppClass: an app-classified compiled entry MUST NOT load into
// the edge engine (RUL-240 — queued/parallel modes are the app engine's).
func TestLoadRefusesAppClass(t *testing.T) {
	reg := registry.FixtureRegistry{}
	clk := clock.NewFakeClock()

	entry, rule := compileRule(t, `{
		"id":"RULEQUEUED","mode":"queued",
		"triggers":[{"type":"state","entity_id":"`+subjectEntity+`","to":["on"]}],
		"actions":[{"type":"device_command","entity_id":"`+subjectEntity+`","command":"power"}]
	}`)
	if entry.ExecutionClass != "app" {
		t.Fatalf("queued rule should be app, got %q", entry.ExecutionClass)
	}
	e := New(reg, clk, &recordingSink{}, nil)
	if err := e.Load(entry, rule); err == nil {
		t.Fatalf("Load accepted an app-class rule")
	}
}

// TestFirstObservationSuppressed: the very first observation of an entity the
// engine has never seen never fires, even when it lands on the trigger's target
// state (RUL-300/302). A later genuine transition to the same value fires.
func TestFirstObservationSuppressed(t *testing.T) {
	reg := registry.FixtureRegistry{}
	clk := clock.NewFakeClock()
	sink := &recordingSink{}

	entry, rule := compileRule(t, `{
		"id":"RULEFIRST",
		"triggers":[{"type":"state","entity_id":"`+subjectEntity+`","to":["on"]}],
		"actions":[{"type":"device_command","entity_id":"`+subjectEntity+`","command":"power"}]
	}`)
	e := New(reg, clk, sink, nil)
	if err := e.Load(entry, rule); err != nil {
		t.Fatalf("Load: %v", err)
	}
	// First-ever observation already at "on": recorded, never fired.
	if d := e.Observe(state.NewObservation(reg, ent(subjectEntity, "on"), ent(subjectEntity, "on"))); len(d) != 0 {
		t.Fatalf("first observation fired: %+v", d)
	}
	if len(sink.calls) != 0 {
		t.Fatalf("first observation dispatched: %+v", sink.calls)
	}
	// A drop then a genuine re-entry fires.
	e.Observe(state.NewObservation(reg, ent(subjectEntity, "on"), ent(subjectEntity, "off")))
	disps := e.Observe(state.NewObservation(reg, ent(subjectEntity, "off"), ent(subjectEntity, "on")))
	if len(disps) != 1 || disps[0].Disposition != Ran {
		t.Fatalf("genuine transition should fire: %+v", disps)
	}
}

// TestSeedEntityStateEnablesFirstFire: seeding the durable prior state (a
// state_transition's `from`, or a restart's durable value) lets the very next
// observation be a genuine transition that fires (RUL-245/304 baseline seeding).
func TestSeedEntityStateEnablesFirstFire(t *testing.T) {
	reg := registry.FixtureRegistry{}
	clk := clock.NewFakeClock()
	sink := &recordingSink{}

	entry, rule := compileRule(t, `{
		"id":"RULESEED",
		"triggers":[{"type":"state","entity_id":"`+subjectEntity+`","to":["on"]}],
		"actions":[{"type":"device_command","entity_id":"`+subjectEntity+`","command":"power"}]
	}`)
	e := New(reg, clk, sink, nil)
	if err := e.Load(entry, rule); err != nil {
		t.Fatalf("Load: %v", err)
	}
	e.SeedEntityState(subjectEntity, "off")
	disps := e.Observe(state.NewObservation(reg, ent(subjectEntity, "off"), ent(subjectEntity, "on")))
	if len(disps) != 1 || disps[0].Disposition != Ran {
		t.Fatalf("seeded first transition should fire: %+v", disps)
	}
}

// TestForHoldSuppressesFlapAndFiresViaTick: a bounded `for`-hold on the
// monotonic clock does not fire while the level flaps back before the span
// elapses, and does fire once the level has held continuously for the span,
// advanced by Tick (RUL-024/190 on Mono only).
func TestForHoldSuppressesFlapAndFiresViaTick(t *testing.T) {
	reg := registry.FixtureRegistry{}
	clk := clock.NewFakeClock()
	sink := &recordingSink{}

	entry, rule := compileRule(t, `{
		"id":"RULEHOLD",
		"triggers":[{"type":"state","entity_id":"`+subjectEntity+`","to":["on"],"for":5}],
		"actions":[{"type":"device_command","entity_id":"`+subjectEntity+`","command":"power"}]
	}`)
	e := New(reg, clk, sink, nil)
	if err := e.Load(entry, rule); err != nil {
		t.Fatalf("Load: %v", err)
	}
	// Prime baseline off.
	e.Observe(state.NewObservation(reg, ent(subjectEntity, "off"), ent(subjectEntity, "off")))

	// Enter the level (off -> on): arms the hold, does not fire yet.
	if d := e.Observe(state.NewObservation(reg, ent(subjectEntity, "off"), ent(subjectEntity, "on"))); len(d) != 0 {
		t.Fatalf("hold armed but fired immediately: %+v", d)
	}
	// Flap back before 5s: the pending fire is discarded.
	clk.Advance(2_000)
	e.Observe(state.NewObservation(reg, ent(subjectEntity, "on"), ent(subjectEntity, "off")))
	clk.Advance(10_000) // well past 5s, but the level dropped
	if d := e.Tick(clk); len(d) != 0 {
		t.Fatalf("flapped hold fired: %+v", d)
	}
	if len(sink.calls) != 0 {
		t.Fatalf("flapped hold dispatched: %+v", sink.calls)
	}

	// Re-enter and hold continuously for 5s: fires via Tick.
	e.Observe(state.NewObservation(reg, ent(subjectEntity, "off"), ent(subjectEntity, "on")))
	clk.Advance(5_000)
	disps := e.Tick(clk)
	if len(disps) != 1 || disps[0].Disposition != Ran {
		t.Fatalf("held hold should fire via Tick: %+v", disps)
	}
	if len(sink.calls) != 1 {
		t.Fatalf("held hold should dispatch once, got %+v", sink.calls)
	}
}

// TestAttributeUnboundedNoForFires exercises the RUL-023 case-2 wiring end to
// end: an attribute-scoped unbounded trigger fires whenever its named
// attribute changes (independent of the state string), gating a dispatch.
func TestAttributeUnboundedNoForFires(t *testing.T) {
	reg := registry.FixtureRegistry{}
	clk := clock.NewFakeClock()
	sink := &recordingSink{}

	entry, rule := compileRule(t, `{
		"id":"RULEATTR",
		"triggers":[{"type":"state","entity_id":"`+subjectEntity+`","attribute":"volume"}],
		"actions":[{"type":"device_command","entity_id":"`+subjectEntity+`","command":"power"}]
	}`)
	e := New(reg, clk, sink, nil)
	if err := e.Load(entry, rule); err != nil {
		t.Fatalf("Load: %v", err)
	}
	e.Observe(state.NewObservation(reg,
		entAttr(subjectEntity, "on", map[string]any{"volume": 10.0}),
		entAttr(subjectEntity, "on", map[string]any{"volume": 10.0})))
	// Attribute changes value while the state string is unchanged: fires.
	disps := e.Observe(state.NewObservation(reg,
		entAttr(subjectEntity, "on", map[string]any{"volume": 10.0}),
		entAttr(subjectEntity, "on", map[string]any{"volume": 20.0})))
	if len(disps) != 1 || disps[0].Disposition != Ran {
		t.Fatalf("attribute change should fire: %+v", disps)
	}
	if len(sink.calls) != 1 {
		t.Fatalf("attribute change should dispatch: %+v", sink.calls)
	}
}
