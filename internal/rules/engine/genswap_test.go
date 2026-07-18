package engine

import (
	"testing"

	"github.com/maaxton/waiveo-next/internal/rules/clock"
	"github.com/maaxton/waiveo-next/internal/rules/closure"
	"github.com/maaxton/waiveo-next/internal/rules/registry"
	"github.com/maaxton/waiveo-next/internal/rules/state"
)

// fixture ULIDs for the generation-swap cases (contracts/rules-1.md corpus
// convention: fixture ULIDs / 192.0.2.0/24).
const (
	ruleA    = "01J8Z3K4N5P6Q7R8S9T0V1RULEA"
	ruleB    = "01J8Z3K4N5P6Q7R8S9T0V1RULEB"
	entityA  = "01J8Z3K4N5P6Q7R8S9T0V1ENTA"
	entityB  = "01J8Z3K4N5P6Q7R8S9T0V1ENTB"
)

// delayRule builds a rule that on a state trigger (off->on of entityID) runs a
// [delay, device_command] sequence — an in-flight delayed run the moment the
// trigger fires, which a generation swap can then cancel or preserve.
func delayRule(ruleID, entityID string, delaySeconds int) string {
	return `{
		"id":"` + ruleID + `",
		"triggers":[{"type":"state","entity_id":"` + entityID + `","to":["on"]}],
		"actions":[
			{"type":"delay","duration_seconds":` + itoa(delaySeconds) + `},
			{"type":"device_command","entity_id":"` + entityID + `","command":"power"}
		]
	}`
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// applyGenMulti compiles each rule JSON and applies them together as one
// generation carrying the matching per-rule closure (or an empty closure).
func applyGenMulti(t *testing.T, e *Engine, gen int, rules []string, closures []closure.Closure) []RunDisposition {
	t.Helper()
	crs := make([]CompiledRule, 0, len(rules))
	for i, rj := range rules {
		entry, rule := compileRule(t, rj)
		if entry.ExecutionClass != "edge" {
			t.Fatalf("rule %d should classify edge, got %q", i, entry.ExecutionClass)
		}
		cl := closure.Closure{}
		if i < len(closures) {
			cl = closures[i]
		}
		crs = append(crs, CompiledRule{Entry: entry, Rule: rule, Closure: cl})
	}
	return e.ApplyGeneration(Generation{Number: gen, Rules: crs})
}

// primeAndFire primes entityID's baseline off (first-observation suppressed,
// RUL-300) then drives a genuine off->on transition, asserting exactly one Ran
// with nothing dispatched yet (the run is parked on its delay).
func primeAndFire(t *testing.T, e *Engine, reg registry.Registry, entityID string) {
	t.Helper()
	if d := e.Observe(state.NewObservation(reg, ent(entityID, "off"), ent(entityID, "off"))); len(d) != 0 {
		t.Fatalf("%s prime fired: %+v", entityID, d)
	}
	d := e.Observe(state.NewObservation(reg, ent(entityID, "off"), ent(entityID, "on")))
	if len(d) != 1 || d[0].Disposition != Ran {
		t.Fatalf("%s transition: want one Ran, got %+v", entityID, d)
	}
}

// TestGenerationSwapCancelsChangedPreservesUnchanged is the named corpus case
// RUL-380-generation-swap-cancels-changed-rule: applying a new generation
// cancels the in-flight run of a rule whose compiled definition changed
// (rule-A's delay duration edited 30->60), but leaves the in-flight run of an
// unchanged rule (rule-B, byte-identical across the swap despite the generation
// number advancing) uninterrupted (RUL-380/381).
func TestGenerationSwapCancelsChangedPreservesUnchanged(t *testing.T) {
	reg := registry.FixtureRegistry{}
	clk := clock.NewFakeClock()
	sink := &recordingSink{}
	e := New(reg, clk, sink, nil)

	// Generation 1: rule-A and rule-B each a [delay 30s, device_command] run.
	applyGenMulti(t, e, 1, []string{
		delayRule(ruleA, entityA, 30),
		delayRule(ruleB, entityB, 30),
	}, nil)

	// Both runs go in-flight, parked on their 30s delay (run-A1, run-B1).
	primeAndFire(t, e, reg, entityA)
	primeAndFire(t, e, reg, entityB)
	if len(sink.calls) != 0 {
		t.Fatalf("nothing should dispatch before any delay elapses, got %+v", sink.calls)
	}

	// Generation 2: rule-A's delay edited to 60s (changed); rule-B byte-identical.
	out := applyGenMulti(t, e, 2, []string{
		delayRule(ruleA, entityA, 60),
		delayRule(ruleB, entityB, 30),
	}, nil)

	// run-A1 is canceled (rule-A changed); run-B1 is not (rule-B identical).
	if len(out) != 1 {
		t.Fatalf("swap should report exactly one canceled run (rule-A), got %+v", out)
	}
	if out[0].RuleID != ruleA || out[0].Disposition != Canceled {
		t.Fatalf("expected rule-A canceled, got %+v", out[0])
	}

	// Advance past the original 30s delay and Tick: rule-B's preserved run
	// resumes and dispatches its device_command; rule-A's canceled run does not.
	clk.Advance(35_000)
	e.Tick(clk)
	if len(sink.calls) != 1 {
		t.Fatalf("only rule-B's preserved run should dispatch, got %+v", sink.calls)
	}
	if sink.calls[0].EntityID != entityB {
		t.Fatalf("dispatch should target entityB (rule-B preserved), got %+v", sink.calls[0])
	}
}

// TestGenerationSwapNumberOnlyBumpIsNoOp: a generation whose rule is byte-
// identical and whose closure is identical, differing only in generation
// number, cancels nothing — the in-flight run survives (RUL-381).
func TestGenerationSwapNumberOnlyBumpIsNoOp(t *testing.T) {
	reg := registry.FixtureRegistry{}
	clk := clock.NewFakeClock()
	sink := &recordingSink{}
	e := New(reg, clk, sink, nil)

	applyGenMulti(t, e, 1, []string{delayRule(ruleA, entityA, 30)}, nil)
	primeAndFire(t, e, reg, entityA)

	// Generation 2: identical structure + closure, only the number advanced.
	out := applyGenMulti(t, e, 2, []string{delayRule(ruleA, entityA, 30)}, nil)
	if len(out) != 0 {
		t.Fatalf("a number-only bump must cancel nothing, got %+v", out)
	}

	// The in-flight run survives the swap and still resumes.
	clk.Advance(35_000)
	e.Tick(clk)
	if len(sink.calls) != 1 || sink.calls[0].EntityID != entityA {
		t.Fatalf("preserved run should dispatch entityA's command, got %+v", sink.calls)
	}
}

// TestGenerationSwapRemovedRuleCancelsRun: a rule dropped from the new
// generation is "removed"; its in-flight run is canceled (RUL-380).
func TestGenerationSwapRemovedRuleCancelsRun(t *testing.T) {
	reg := registry.FixtureRegistry{}
	clk := clock.NewFakeClock()
	sink := &recordingSink{}
	e := New(reg, clk, sink, nil)

	applyGenMulti(t, e, 1, []string{
		delayRule(ruleA, entityA, 30),
		delayRule(ruleB, entityB, 30),
	}, nil)
	primeAndFire(t, e, reg, entityA)
	primeAndFire(t, e, reg, entityB)

	// Generation 2 drops rule-A entirely; only rule-B remains (byte-identical).
	out := applyGenMulti(t, e, 2, []string{delayRule(ruleB, entityB, 30)}, nil)
	if len(out) != 1 || out[0].RuleID != ruleA || out[0].Disposition != Canceled {
		t.Fatalf("removed rule-A's run must be canceled, got %+v", out)
	}

	clk.Advance(35_000)
	e.Tick(clk)
	if len(sink.calls) != 1 || sink.calls[0].EntityID != entityB {
		t.Fatalf("only surviving rule-B should dispatch, got %+v", sink.calls)
	}
}

// TestGenerationSwapChangedClosureCancelsRun: two generations with byte-
// identical rule structure but a differing closed-over value (frozen
// guest_mode false -> true) count as "changed" (RUL-381 — any closed-over value
// differs), so the in-flight run is canceled even though the compiled structure
// is identical.
func TestGenerationSwapChangedClosureCancelsRun(t *testing.T) {
	reg := registry.FixtureRegistry{}
	clk := clock.NewFakeClock()
	sink := &recordingSink{}
	e := New(reg, clk, sink, nil)

	ruleJSON := `{
		"id":"` + ruleA + `",
		"triggers":[{"type":"state","entity_id":"` + entityA + `","to":["on"]}],
		"conditions":[{"type":"variable","variable":"guest_mode","equals":false}],
		"actions":[
			{"type":"delay","duration_seconds":30},
			{"type":"device_command","entity_id":"` + entityA + `","command":"power"}
		]
	}`
	// Generation 1 freezes guest_mode=false; the condition passes and the run
	// parks on its delay.
	applyGenMulti(t, e, 1, []string{ruleJSON},
		[]closure.Closure{{Variables: map[string]any{"guest_mode": false}}})
	primeAndFire(t, e, reg, entityA)

	// Generation 2: identical rule structure, but the frozen closure differs
	// (guest_mode=true). RUL-381 counts this as changed.
	out := applyGenMulti(t, e, 2, []string{ruleJSON},
		[]closure.Closure{{Variables: map[string]any{"guest_mode": true}}})
	if len(out) != 1 || out[0].RuleID != ruleA || out[0].Disposition != Canceled {
		t.Fatalf("a differing closed-over value must cancel the run, got %+v", out)
	}

	// The canceled run does not resume; nothing dispatches.
	clk.Advance(35_000)
	e.Tick(clk)
	if len(sink.calls) != 0 {
		t.Fatalf("canceled run must not dispatch, got %+v", sink.calls)
	}
}

// TestGenerationSwapUnchangedPreservesInFlightNoNewFire: an unchanged rule's
// in-flight run continues uninterrupted across the swap AND its per-(trigger,
// entity) baseline carries forward — a bare re-apply produces no spurious
// firing (RUL-303/380). Here the same generation is re-applied while a run is
// parked; the run survives and no new disposition is produced by the apply.
func TestGenerationSwapUnchangedPreservesInFlightNoNewFire(t *testing.T) {
	reg := registry.FixtureRegistry{}
	clk := clock.NewFakeClock()
	sink := &recordingSink{}
	e := New(reg, clk, sink, nil)

	applyGenMulti(t, e, 1, []string{delayRule(ruleA, entityA, 30)}, nil)
	primeAndFire(t, e, reg, entityA)

	// Re-apply the identical generation: nothing cancels, nothing fires.
	out := applyGenMulti(t, e, 2, []string{delayRule(ruleA, entityA, 30)}, nil)
	if len(out) != 0 {
		t.Fatalf("re-applying an identical rule must be a no-op, got %+v", out)
	}

	// A reconfirming observation of the already-on entity does not re-fire: the
	// unchanged trigger's durable baseline (on) carried forward across the swap
	// (RUL-303), so on->on is not a transition.
	if d := e.Observe(state.NewObservation(reg, ent(entityA, "on"), ent(entityA, "on"))); len(d) != 0 {
		t.Fatalf("reconfirm of unchanged trigger after swap must not fire, got %+v", d)
	}
	// Confirm the preserved run still exists by resuming it.
	clk.Advance(35_000)
	e.Tick(clk)
	if len(sink.calls) != 1 || sink.calls[0].EntityID != entityA {
		t.Fatalf("preserved run should still resume and dispatch, got %+v", sink.calls)
	}
}
