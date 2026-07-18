package engine

import (
	"testing"
	"time"

	"github.com/maaxton/waiveo-next/internal/rules/clock"
	"github.com/maaxton/waiveo-next/internal/rules/registry"
	"github.com/maaxton/waiveo-next/internal/rules/state"
)

// stabRule builds a rule that fires on a genuine off->on state transition of
// entityID and, with no intervening delay, dispatches a single device_command —
// so a dispatch is observable in the sink the instant the firing runs (and its
// absence proves a firing was deferred).
func stabRule(ruleID, entityID string) string {
	return `{
		"id":"` + ruleID + `",
		"triggers":[{"type":"state","entity_id":"` + entityID + `","from":["off"],"to":["on"]}],
		"actions":[
			{"type":"device_command","entity_id":"` + entityID + `","command":"power"}
		]
	}`
}

// TestStabilizationWindowDefersTransition is the named corpus case
// RUL-301-stabilization-window-defers-transition: a genuine state transition
// observed before the post-restart stabilization window elapses is held pending
// — not dropped, not dispatched early — and dispatches once the window elapses,
// released as the already-matched transition rather than re-evaluated against a
// later observation (RUL-301).
func TestStabilizationWindowDefersTransition(t *testing.T) {
	reg := registry.FixtureRegistry{}
	clk := clock.NewFakeClock()
	sink := &recordingSink{}
	e := New(reg, clk, sink, nil)

	// Fixture: a 5000ms window. The engine_restart readiness point is modeled by
	// a fresh engine (New) applying a generation (Load) seeded with the entity's
	// durable pre-restart state (off).
	e.SetStabilizationWindow(5 * time.Second)

	entry, rule := compileRule(t, stabRule("01J8Z3K4N5P6Q7R8S9T0V1RULE", subjectEntity))
	if err := e.Load(entry, rule); err != nil {
		t.Fatalf("Load: %v", err)
	}
	e.SeedEntityState(subjectEntity, "off") // durable_state_before_restart

	// seq 2 @ t=900ms: a genuine off->on transition observed BEFORE the window
	// elapses. It matches, but MUST NOT dispatch — held pending (RUL-301).
	clk.Advance(900)
	if d := e.Observe(state.NewObservation(reg, ent(subjectEntity, "off"), ent(subjectEntity, "on"))); len(d) != 0 {
		t.Fatalf("in-window transition dispatched early: %+v", d)
	}
	if len(sink.calls) != 0 {
		t.Fatalf("held transition must not dispatch its command early: %+v", sink.calls)
	}

	// A Tick still inside the window does not release it.
	clk.Advance(3000) // t=3900
	if d := e.Tick(clk); len(d) != 0 {
		t.Fatalf("Tick inside the window released early: %+v", d)
	}
	if len(sink.calls) != 0 {
		t.Fatalf("held transition dispatched inside the window: %+v", sink.calls)
	}

	// @ t=5000ms the window elapses: the already-matched transition is RELEASED
	// and dispatches exactly once.
	clk.Advance(1100) // t=5000
	d := e.Tick(clk)
	if len(d) != 1 || d[0].Disposition != Ran {
		t.Fatalf("window elapse must release the held transition as one Ran, got %+v", d)
	}
	if len(sink.calls) != 1 || sink.calls[0].EntityID != subjectEntity {
		t.Fatalf("released transition should dispatch the command once, got %+v", sink.calls)
	}
}

// TestStabilizationWindowBoundedFallbackReleasesOnce: the window is fallback-
// timed — a held transition releases once the bounded maximum monotonic wait
// elapses even if the clock jumps far past the deadline in a single step (the
// gate cannot wedge open), and it releases exactly once (RUL-301).
func TestStabilizationWindowBoundedFallbackReleasesOnce(t *testing.T) {
	reg := registry.FixtureRegistry{}
	clk := clock.NewFakeClock()
	sink := &recordingSink{}
	e := New(reg, clk, sink, nil)
	e.SetStabilizationWindow(5 * time.Second)

	entry, rule := compileRule(t, stabRule("01J8Z3K4N5P6Q7R8S9T0V1RULE", subjectEntity))
	if err := e.Load(entry, rule); err != nil {
		t.Fatalf("Load: %v", err)
	}
	e.SeedEntityState(subjectEntity, "off")

	clk.Advance(900)
	if d := e.Observe(state.NewObservation(reg, ent(subjectEntity, "off"), ent(subjectEntity, "on"))); len(d) != 0 {
		t.Fatalf("in-window transition dispatched early: %+v", d)
	}

	// Jump far past the deadline in one step: the bounded fallback still fires.
	clk.Advance(100_000) // t=100900, well past the 5000ms deadline
	d := e.Tick(clk)
	if len(d) != 1 || d[0].Disposition != Ran {
		t.Fatalf("bounded fallback must release the held fire once, got %+v", d)
	}
	if len(sink.calls) != 1 {
		t.Fatalf("held fire should dispatch exactly once, got %+v", sink.calls)
	}

	// A further Tick does not re-dispatch the consumed held fire.
	clk.Advance(1000)
	if d := e.Tick(clk); len(d) != 0 {
		t.Fatalf("held fire released twice: %+v", d)
	}
	if len(sink.calls) != 1 {
		t.Fatalf("double dispatch after release, got %+v", sink.calls)
	}
}

// TestStabilizationOnlyGatesNewlyEligible: a trigger whose window has already
// elapsed is no longer newly-eligible, so a subsequent genuine transition
// dispatches immediately — the gate applies only from the readiness point that
// made it eligible, not to every later observation (RUL-301).
func TestStabilizationOnlyGatesNewlyEligible(t *testing.T) {
	reg := registry.FixtureRegistry{}
	clk := clock.NewFakeClock()
	sink := &recordingSink{}
	e := New(reg, clk, sink, nil)
	e.SetStabilizationWindow(5 * time.Second)

	entry, rule := compileRule(t, stabRule("01J8Z3K4N5P6Q7R8S9T0V1RULE", subjectEntity))
	if err := e.Load(entry, rule); err != nil {
		t.Fatalf("Load: %v", err)
	}
	e.SeedEntityState(subjectEntity, "off")

	// Let the window elapse with no transition pending.
	clk.Advance(6000)
	if d := e.Tick(clk); len(d) != 0 {
		t.Fatalf("an empty window elapse should produce nothing, got %+v", d)
	}

	// A genuine transition now dispatches immediately — no longer gated.
	d := e.Observe(state.NewObservation(reg, ent(subjectEntity, "off"), ent(subjectEntity, "on")))
	if len(d) != 1 || d[0].Disposition != Ran {
		t.Fatalf("post-window transition should dispatch immediately, got %+v", d)
	}
	if len(sink.calls) != 1 || sink.calls[0].EntityID != subjectEntity {
		t.Fatalf("post-window transition should dispatch its command, got %+v", sink.calls)
	}
}

// TestStabilizationUnchangedTriggerNotReGatedAcrossSwap: an unchanged trigger
// carried forward across a generation swap is NOT made newly-eligible by the
// swap, so it is not re-gated — a genuine transition right after the swap
// dispatches immediately (RUL-301/303).
func TestStabilizationUnchangedTriggerNotReGatedAcrossSwap(t *testing.T) {
	reg := registry.FixtureRegistry{}
	clk := clock.NewFakeClock()
	sink := &recordingSink{}
	e := New(reg, clk, sink, nil)
	e.SetStabilizationWindow(5 * time.Second)

	// Generation 1 arms the trigger's window; let it elapse with nothing pending.
	applyGenMulti(t, e, 1, []string{stabRule(ruleA, entityA)}, nil)
	e.SeedEntityState(entityA, "off")
	clk.Advance(6000)
	e.Tick(clk)

	// Generation 2 re-applies the identical rule: the trigger is unchanged and
	// carries forward — it MUST NOT be re-armed by the swap.
	out := applyGenMulti(t, e, 2, []string{stabRule(ruleA, entityA)}, nil)
	if len(out) != 0 {
		t.Fatalf("an unchanged swap cancels nothing, got %+v", out)
	}

	// A transition immediately after the swap dispatches — not re-gated.
	d := e.Observe(state.NewObservation(reg, ent(entityA, "off"), ent(entityA, "on")))
	if len(d) != 1 || d[0].Disposition != Ran {
		t.Fatalf("unchanged carried trigger must not be re-gated, got %+v", d)
	}
	if len(sink.calls) != 1 || sink.calls[0].EntityID != entityA {
		t.Fatalf("unchanged carried trigger should dispatch, got %+v", sink.calls)
	}
}

// TestStabilizationNewTriggerInSwapIsGated: a trigger NEW in an applied
// generation is made newly-eligible by that generation-apply readiness point and
// serves its own window from it, while an unchanged sibling carried across the
// same swap is unaffected — each trigger's window is its own, not one engine-wide
// gate (RUL-301).
func TestStabilizationNewTriggerInSwapIsGated(t *testing.T) {
	reg := registry.FixtureRegistry{}
	clk := clock.NewFakeClock()
	sink := &recordingSink{}
	e := New(reg, clk, sink, nil)
	e.SetStabilizationWindow(5 * time.Second)

	// Generation 1: rule-A on entityA; let its window elapse.
	applyGenMulti(t, e, 1, []string{stabRule(ruleA, entityA)}, nil)
	e.SeedEntityState(entityA, "off")
	clk.Advance(6000) // t=6000
	e.Tick(clk)

	// Generation 2 ADDS rule-B on entityB (new) and keeps rule-A (unchanged). The
	// new trigger serves its own 5000ms window from t=6000 (deadline t=11000).
	out := applyGenMulti(t, e, 2, []string{stabRule(ruleA, entityA), stabRule(ruleB, entityB)}, nil)
	if len(out) != 0 {
		t.Fatalf("adding a new rule cancels nothing, got %+v", out)
	}
	e.SeedEntityState(entityB, "off")

	// entityB's transition inside its window is held.
	clk.Advance(1000) // t=7000 < 11000
	if d := e.Observe(state.NewObservation(reg, ent(entityB, "off"), ent(entityB, "on"))); len(d) != 0 {
		t.Fatalf("new-in-swap trigger must be gated, got %+v", d)
	}
	if len(sink.calls) != 0 {
		t.Fatalf("gated new trigger must not dispatch, got %+v", sink.calls)
	}

	// entityA (unchanged, carried) is not gated: its transition dispatches now.
	d := e.Observe(state.NewObservation(reg, ent(entityA, "off"), ent(entityA, "on")))
	if len(d) != 1 || d[0].RuleID != ruleA || d[0].Disposition != Ran {
		t.Fatalf("unchanged sibling must dispatch immediately, got %+v", d)
	}
	if len(sink.calls) != 1 || sink.calls[0].EntityID != entityA {
		t.Fatalf("only entityA should have dispatched so far, got %+v", sink.calls)
	}

	// entityB releases when its own window elapses at t=11000.
	clk.Advance(4000) // t=11000
	rel := e.Tick(clk)
	if len(rel) != 1 || rel[0].RuleID != ruleB || rel[0].Disposition != Ran {
		t.Fatalf("entityB's held transition should release at its deadline, got %+v", rel)
	}
	if len(sink.calls) != 2 || sink.calls[1].EntityID != entityB {
		t.Fatalf("entityB should dispatch on release, got %+v", sink.calls)
	}
}

// TestStabilizationDefaultOffDoesNotGate: with no window configured the engine
// gate is off (its documented default effective value is zero), so a fresh
// engine dispatches a genuine transition immediately — the Part 1-3 callers that
// never opt in are never deferred (RUL-301).
func TestStabilizationDefaultOffDoesNotGate(t *testing.T) {
	reg := registry.FixtureRegistry{}
	clk := clock.NewFakeClock()
	sink := &recordingSink{}
	e := New(reg, clk, sink, nil)

	entry, rule := compileRule(t, stabRule("01J8Z3K4N5P6Q7R8S9T0V1RULE", subjectEntity))
	if err := e.Load(entry, rule); err != nil {
		t.Fatalf("Load: %v", err)
	}
	e.SeedEntityState(subjectEntity, "off")

	d := e.Observe(state.NewObservation(reg, ent(subjectEntity, "off"), ent(subjectEntity, "on")))
	if len(d) != 1 || d[0].Disposition != Ran {
		t.Fatalf("default-off engine must not gate, got %+v", d)
	}
	if len(sink.calls) != 1 {
		t.Fatalf("default-off engine should dispatch immediately, got %+v", sink.calls)
	}
}
