package engine

import (
	"testing"

	"github.com/maaxton/waiveo-next/internal/rules/clock"
	"github.com/maaxton/waiveo-next/internal/rules/registry"
	"github.com/maaxton/waiveo-next/internal/rules/state"
)

// fixture ULIDs for the first-observation carry-forward cases (RUL-303/304).
const (
	ruleR = "01J8Z3K4N5P6Q7R8S9T0V1RULER"
	ruleS = "01J8Z3K4N5P6Q7R8S9T0V1RULES"
)

// stateTriggerRule builds a single-trigger rule whose state trigger and (empty)
// action set make it fire once on the given entity's off->on transition.
func stateTriggerRule(ruleID, entityID string, forSeconds int) string {
	forField := ""
	if forSeconds > 0 {
		forField = `,"for":` + itoa(forSeconds)
	}
	return `{
		"id":"` + ruleID + `",
		"triggers":[{"type":"state","entity_id":"` + entityID + `","from":["off"],"to":["on"]` + forField + `}],
		"actions":[{"type":"device_command","entity_id":"` + entityID + `","command":"power"}]
	}`
}

// TestGenerationSwapUnchangedTriggerPreservesBaseline is the named corpus case
// RUL-303-generation-swap-unchanged-trigger-preserves-baseline. Two rules share
// one entity: rule-R's trigger is byte-identical across the swap (unchanged),
// rule-S's trigger has its `for` edited 10->20 (changed). Both baselines are
// primed to `off` before the swap. After the swap a genuine off->on transition
// fires rule-R (its first-observation baseline carried forward, RUL-303) but NOT
// rule-S (its baseline reset by the change, so this is a first observation that
// RUL-300 suppresses) — proving first-observation state is kept independently per
// (trigger, entity), RUL-304, not per entity alone.
func TestGenerationSwapUnchangedTriggerPreservesBaseline(t *testing.T) {
	reg := registry.FixtureRegistry{}
	clk := clock.NewFakeClock()
	sink := &recordingSink{}
	e := New(reg, clk, sink, nil)

	// Generation 1: rule-R (no `for`), rule-S (`for:10`), both on subjectEntity.
	applyGenMulti(t, e, 1, []string{
		stateTriggerRule(ruleR, subjectEntity, 0),
		stateTriggerRule(ruleS, subjectEntity, 10),
	}, nil)

	// seq 2 (t=100): reconfirm durable `off` — establishes the first-observation
	// baseline for BOTH rule-R and rule-S.
	clk.SetWall(100)
	if d := e.Observe(state.NewObservation(reg, ent(subjectEntity, "off"), ent(subjectEntity, "off"))); len(d) != 0 {
		t.Fatalf("seq2 prime fired: %+v", d)
	}

	// seq 3 (t=5000): an UNRELATED preset edit recompiles generation 2. rule-R is
	// byte-identical (unchanged); rule-S's `for` is edited 10->20 (changed).
	clk.SetWall(5000)
	out := applyGenMulti(t, e, 2, []string{
		stateTriggerRule(ruleR, subjectEntity, 0),
		stateTriggerRule(ruleS, subjectEntity, 20),
	}, nil)
	if len(out) != 0 {
		t.Fatalf("no in-flight run existed, swap should cancel nothing, got %+v", out)
	}

	// seq 4 (t=65000): a genuine off->on transition, well clear of any window.
	clk.SetWall(65000)
	clk.Advance(65000)
	disps := e.Observe(state.NewObservation(reg, ent(subjectEntity, "off"), ent(subjectEntity, "on")))

	// rule-R fires (unchanged trigger's baseline carried forward, RUL-303);
	// rule-S does NOT (changed trigger reset -> first observation, RUL-300/304).
	if len(disps) != 1 {
		t.Fatalf("seq4: exactly rule-R should fire, got %+v", disps)
	}
	if disps[0].RuleID != ruleR || disps[0].Disposition != Ran {
		t.Fatalf("seq4: want rule-R Ran, got %+v", disps[0])
	}
}

// TestGenerationSwapCarriesUnchangedTriggerWithinChangedRule exercises the
// trigger-granularity of RUL-303/304: the changed/unchanged test is applied at
// the individual trigger's level, NOT the whole rule's. One rule carries two
// state triggers on two different entities. In the new generation only the
// SECOND trigger's compiled structure changes (a `from` bound added); the rule
// as a whole is therefore "changed", but the FIRST trigger is untouched. The
// unchanged trigger MUST carry its first-observation baseline forward (fire on a
// genuine transition), while the changed trigger MUST reset (its first post-swap
// observation is suppressed as a first observation). This is the case the
// rule-level diff alone cannot satisfy.
func TestGenerationSwapCarriesUnchangedTriggerWithinChangedRule(t *testing.T) {
	reg := registry.FixtureRegistry{}
	clk := clock.NewFakeClock()
	sink := &recordingSink{}
	e := New(reg, clk, sink, nil)

	gen1 := `{
		"id":"` + ruleA + `",
		"triggers":[
			{"type":"state","entity_id":"` + entityA + `","from":["off"],"to":["on"]},
			{"type":"state","entity_id":"` + entityB + `","to":["on"]}
		],
		"actions":[{"type":"device_command","entity_id":"` + entityA + `","command":"power"}]
	}`
	// gen2: trigger-1 byte-identical; trigger-2 gains a `from` bound (changed).
	gen2 := `{
		"id":"` + ruleA + `",
		"triggers":[
			{"type":"state","entity_id":"` + entityA + `","from":["off"],"to":["on"]},
			{"type":"state","entity_id":"` + entityB + `","from":["off"],"to":["on"]}
		],
		"actions":[{"type":"device_command","entity_id":"` + entityA + `","command":"power"}]
	}`

	applyGenMulti(t, e, 1, []string{gen1}, nil)

	// Prime BOTH triggers' baselines to `off`.
	if d := e.Observe(state.NewObservation(reg, ent(entityA, "off"), ent(entityA, "off"))); len(d) != 0 {
		t.Fatalf("entityA prime fired: %+v", d)
	}
	if d := e.Observe(state.NewObservation(reg, ent(entityB, "off"), ent(entityB, "off"))); len(d) != 0 {
		t.Fatalf("entityB prime fired: %+v", d)
	}

	// Apply the changed rule. The rule as a whole is changed (trigger-2 differs),
	// but no run is in flight, so nothing cancels.
	out := applyGenMulti(t, e, 2, []string{gen2}, nil)
	if len(out) != 0 {
		t.Fatalf("no in-flight run to cancel, got %+v", out)
	}

	// Unchanged trigger-1 (entityA): its baseline `off` carried forward, so this
	// off->on is a genuine transition and fires (RUL-303 at trigger granularity).
	disps := e.Observe(state.NewObservation(reg, ent(entityA, "off"), ent(entityA, "on")))
	if len(disps) != 1 || disps[0].Disposition != Ran {
		t.Fatalf("unchanged trigger-1 should fire on genuine transition, got %+v", disps)
	}

	// Changed trigger-2 (entityB): its baseline was reset, so this off->on is a
	// FIRST observation and RUL-300 suppresses it — no fire.
	disps = e.Observe(state.NewObservation(reg, ent(entityB, "off"), ent(entityB, "on")))
	if len(disps) != 0 {
		t.Fatalf("changed trigger-2 must reset and suppress its first observation, got %+v", disps)
	}

	// The reset trigger-2 is merely reset, not broken: after its baseline is
	// re-established (now `on`), a subsequent genuine on->off->on transition fires.
	e.Observe(state.NewObservation(reg, ent(entityB, "on"), ent(entityB, "off")))
	disps = e.Observe(state.NewObservation(reg, ent(entityB, "off"), ent(entityB, "on")))
	if len(disps) != 1 || disps[0].Disposition != Ran {
		t.Fatalf("changed trigger-2 should fire on its next genuine transition, got %+v", disps)
	}
}
