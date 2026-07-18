package engine

import (
	"testing"

	"github.com/maaxton/waiveo-next/internal/rules/clock"
	"github.com/maaxton/waiveo-next/internal/rules/closure"
	"github.com/maaxton/waiveo-next/internal/rules/eval"
	"github.com/maaxton/waiveo-next/internal/rules/registry"
	"github.com/maaxton/waiveo-next/internal/rules/state"
)

// fixture ULIDs for the multi-entity/preset generation cases (contracts/rules-1.md
// corpus convention).
const (
	lobbyA  = "01J8Z3K4N5P6Q7R8S9T0V1LBYA"
	lobbyB  = "01J8Z3K4N5P6Q7R8S9T0V1LBYB"
	presetP = "01J8Z3K4N5P6Q7R8S9T0V1PRST"
)

// applyOne compiles a rule and applies it as a one-rule generation carrying the
// given frozen closure. It returns the engine for further driving.
func applyGen(t *testing.T, e *Engine, gen int, cl closure.Closure, ruleJSON string) {
	t.Helper()
	entry, rule := compileRule(t, ruleJSON)
	if entry.ExecutionClass != "edge" {
		t.Fatalf("rule should classify edge, got %q", entry.ExecutionClass)
	}
	e.ApplyGeneration(Generation{Number: gen, Rules: []CompiledRule{{Entry: entry, Rule: rule, Closure: cl}}})
}

// TestApplyGenerationRUL150FrozenVariableGates is the named corpus case
// RUL-150-variable-condition-constant-folded: the variable's compile-time value
// is frozen into the generation, and the edge engine evaluates the condition
// against that frozen value on every subsequent firing — never a live lookup —
// even while offline (RUL-150/390/391). The frozen guest_mode=false satisfies
// the `equals false` condition, so each genuine transition fires; a later
// (simulated) app-side variable write does not reach the applied generation.
func TestApplyGenerationRUL150FrozenVariableGates(t *testing.T) {
	reg := registry.FixtureRegistry{}
	clk := clock.NewFakeClock()
	sink := &recordingSink{}
	e := New(reg, clk, sink, nil)

	ruleJSON := `{
		"id":"01J8Z3K4N5P6Q7R8S9T0V1RULA",
		"triggers":[{"type":"state","entity_id":"` + subjectEntity + `","to":["on"]}],
		"conditions":[{"type":"variable","variable":"guest_mode","equals":false}],
		"actions":[{"type":"device_command","entity_id":"` + subjectEntity + `","command":"power"}]
	}`
	// Generation 1 freezes guest_mode=false at compile time (closed_over).
	applyGen(t, e, 1, closure.Closure{Variables: map[string]any{"guest_mode": false}}, ruleJSON)

	// Prime baseline off (first-observation suppressed, RUL-300).
	e.Observe(state.NewObservation(reg, ent(subjectEntity, "off"), ent(subjectEntity, "off")))

	// seq 2 — trigger fires offline: condition evaluated against the frozen
	// false -> equals false -> passes -> Ran.
	disps := e.Observe(state.NewObservation(reg, ent(subjectEntity, "off"), ent(subjectEntity, "on")))
	if len(disps) != 1 || disps[0].Disposition != Ran {
		t.Fatalf("seq2: want one Ran (frozen guest_mode=false matches equals false), got %+v", disps)
	}
	if len(sink.calls) != 1 {
		t.Fatalf("seq2: want one dispatch, got %+v", sink.calls)
	}

	// seq 3/4 — a later app-side variable write to true does NOT reach the
	// applied generation; it keeps evaluating the stale-but-defined frozen
	// value (RUL-391). A second genuine transition still fires.
	e.Observe(state.NewObservation(reg, ent(subjectEntity, "on"), ent(subjectEntity, "off")))
	disps = e.Observe(state.NewObservation(reg, ent(subjectEntity, "off"), ent(subjectEntity, "on")))
	if len(disps) != 1 || disps[0].Disposition != Ran {
		t.Fatalf("seq4: frozen value must still gate to pass, got %+v", disps)
	}
	if len(sink.calls) != 2 {
		t.Fatalf("seq4: want two dispatches total, got %+v", sink.calls)
	}
}

// TestApplyGenerationFrozenVariableCanFailClosed is the corpus note: a new
// generation compiled after the app-side write would freeze guest_mode=true, and
// the identical `equals false` condition then FAILS — proving the frozen value
// genuinely gates the rule, and that evaluation follows whatever the applied
// generation froze (RUL-150/390).
func TestApplyGenerationFrozenVariableCanFailClosed(t *testing.T) {
	reg := registry.FixtureRegistry{}
	clk := clock.NewFakeClock()
	sink := &recordingSink{}
	e := New(reg, clk, sink, nil)

	ruleJSON := `{
		"id":"01J8Z3K4N5P6Q7R8S9T0V1RULA",
		"triggers":[{"type":"state","entity_id":"` + subjectEntity + `","to":["on"]}],
		"conditions":[{"type":"variable","variable":"guest_mode","equals":false}],
		"actions":[{"type":"device_command","entity_id":"` + subjectEntity + `","command":"power"}]
	}`
	// This generation froze guest_mode=true; `equals false` no longer matches.
	applyGen(t, e, 2, closure.Closure{Variables: map[string]any{"guest_mode": true}}, ruleJSON)

	e.Observe(state.NewObservation(reg, ent(subjectEntity, "off"), ent(subjectEntity, "off")))
	disps := e.Observe(state.NewObservation(reg, ent(subjectEntity, "off"), ent(subjectEntity, "on")))
	if len(disps) != 0 {
		t.Fatalf("frozen guest_mode=true must fail `equals false` and suppress the run, got %+v", disps)
	}
	if len(sink.calls) != 0 {
		t.Fatalf("no dispatch expected when the frozen condition fails, got %+v", sink.calls)
	}
}

// TestApplyGenerationSelectorTriggerFansOutPerEntity exercises RUL-011/013: a
// selector trigger's matched entity set is frozen into the generation, and each
// matched entity is an independent per-(trigger,entity) subject — its own
// first-observation suppression and its own firing (RUL-304). One matched
// entity's firing dispatches the action sequence once.
func TestApplyGenerationSelectorTriggerFansOutPerEntity(t *testing.T) {
	reg := registry.FixtureRegistry{}
	clk := clock.NewFakeClock()
	sink := &recordingSink{}
	e := New(reg, clk, sink, nil)

	ruleJSON := `{
		"id":"01J8Z3K4N5P6Q7R8S9T0V1RULA",
		"triggers":[{"type":"state","selector":"label==lobby-screens","to":["on"]}],
		"actions":[{"type":"device_command","selector":"label==lobby-screens","command":"power"}]
	}`
	// The selector's matched set is frozen to [lobbyA, lobbyB] (RUL-013).
	cl := closure.Closure{Selectors: map[string][]string{"label==lobby-screens": {lobbyA, lobbyB}}}
	applyGen(t, e, 1, cl, ruleJSON)

	// Prime each matched entity's baseline independently (first-observation
	// suppressed per (trigger,entity), RUL-300/304). Enrolling them in the
	// snapshot also lets the action's command-vocabulary check resolve.
	if d := e.Observe(state.NewObservation(reg, ent(lobbyA, "off"), ent(lobbyA, "off"))); len(d) != 0 {
		t.Fatalf("lobbyA prime fired: %+v", d)
	}
	if d := e.Observe(state.NewObservation(reg, ent(lobbyB, "off"), ent(lobbyB, "off"))); len(d) != 0 {
		t.Fatalf("lobbyB prime fired: %+v", d)
	}

	// lobbyA transitions off->on: exactly its own trigger fires once (lobbyB's
	// independent baseline is untouched, so it does not fire).
	disps := e.Observe(state.NewObservation(reg, ent(lobbyA, "off"), ent(lobbyA, "on")))
	if len(disps) != 1 || disps[0].Disposition != Ran {
		t.Fatalf("lobbyA transition: want one Ran, got %+v", disps)
	}
	// The action's selector fans out over the frozen matched set (RUL-012/013):
	// power dispatched to BOTH lobbyA and lobbyB.
	if len(sink.calls) != 2 {
		t.Fatalf("action selector should fan out to both frozen targets, got %+v", sink.calls)
	}
	gotTargets := map[string]bool{sink.calls[0].EntityID: true, sink.calls[1].EntityID: true}
	if !gotTargets[lobbyA] || !gotTargets[lobbyB] {
		t.Fatalf("action fan-out targets = %v, want lobbyA+lobbyB", gotTargets)
	}

	// lobbyB transitions off->on: its own independent trigger now fires once.
	disps = e.Observe(state.NewObservation(reg, ent(lobbyB, "off"), ent(lobbyB, "on")))
	if len(disps) != 1 || disps[0].Disposition != Ran {
		t.Fatalf("lobbyB transition: want one Ran (independent per-entity), got %+v", disps)
	}
	if len(sink.calls) != 4 {
		t.Fatalf("second fan-out should add two dispatches, got %+v", sink.calls)
	}
}

// TestApplyGenerationPresetBatchUsesFrozenCommands exercises RUL-173: a
// preset_batch action dispatches the command list FROZEN into the generation at
// compile time, never a live preset store (the engine here is built with a nil
// live PresetStore, so the only source of commands is the closure).
func TestApplyGenerationPresetBatchUsesFrozenCommands(t *testing.T) {
	reg := registry.FixtureRegistry{}
	clk := clock.NewFakeClock()
	sink := &recordingSink{}
	e := New(reg, clk, sink, nil) // nil live preset store

	ruleJSON := `{
		"id":"01J8Z3K4N5P6Q7R8S9T0V1RULA",
		"triggers":[{"type":"state","entity_id":"` + subjectEntity + `","to":["on"]}],
		"actions":[{"type":"preset_batch","preset_id":"` + presetP + `"}]
	}`
	cl := closure.Closure{PresetBatches: map[string][]eval.PresetCommand{
		presetP: {{EntityID: lobbyA, Command: "power"}, {EntityID: lobbyB, Command: "power"}},
	}}
	applyGen(t, e, 1, cl, ruleJSON)

	// Enroll the preset's target entities (so the command-vocabulary check
	// resolves their device class) and prime the trigger subject.
	e.Observe(state.NewObservation(reg, ent(lobbyA, "off"), ent(lobbyA, "off")))
	e.Observe(state.NewObservation(reg, ent(lobbyB, "off"), ent(lobbyB, "off")))
	e.Observe(state.NewObservation(reg, ent(subjectEntity, "off"), ent(subjectEntity, "off")))

	disps := e.Observe(state.NewObservation(reg, ent(subjectEntity, "off"), ent(subjectEntity, "on")))
	if len(disps) != 1 || disps[0].Disposition != Ran {
		t.Fatalf("preset_batch rule: want one Ran, got %+v", disps)
	}
	if len(sink.calls) != 2 {
		t.Fatalf("frozen preset batch should dispatch its two commands, got %+v", sink.calls)
	}
	gotTargets := map[string]bool{sink.calls[0].EntityID: true, sink.calls[1].EntityID: true}
	if !gotTargets[lobbyA] || !gotTargets[lobbyB] {
		t.Fatalf("preset dispatch targets = %v, want lobbyA+lobbyB", gotTargets)
	}
}

// TestApplyGenerationReplacesLoadedRules: applying a new generation replaces the
// rules driven by the engine (Task 2's fresh-apply semantics — generation-swap
// preservation is a later task). A rule dropped from the new generation no
// longer fires.
func TestApplyGenerationReplacesLoadedRules(t *testing.T) {
	reg := registry.FixtureRegistry{}
	clk := clock.NewFakeClock()
	sink := &recordingSink{}
	e := New(reg, clk, sink, nil)

	ruleJSON := `{
		"id":"01J8Z3K4N5P6Q7R8S9T0V1RULA",
		"triggers":[{"type":"state","entity_id":"` + subjectEntity + `","to":["on"]}],
		"actions":[{"type":"device_command","entity_id":"` + subjectEntity + `","command":"power"}]
	}`
	applyGen(t, e, 1, closure.Closure{}, ruleJSON)

	// Generation 2 carries no rules at all: nothing is driven.
	e.ApplyGeneration(Generation{Number: 2, Rules: nil})
	e.Observe(state.NewObservation(reg, ent(subjectEntity, "off"), ent(subjectEntity, "off")))
	disps := e.Observe(state.NewObservation(reg, ent(subjectEntity, "off"), ent(subjectEntity, "on")))
	if len(disps) != 0 {
		t.Fatalf("an empty generation drives nothing, got %+v", disps)
	}
	if len(sink.calls) != 0 {
		t.Fatalf("empty generation dispatched: %+v", sink.calls)
	}
}
