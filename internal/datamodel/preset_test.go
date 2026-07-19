package datamodel

import "testing"

// dpState wraps a daypart id + bound preset batch in an EffectiveState, the shape
// PresetTransition reads. Only the effective daypart's identity and its
// preset_batch_id drive firing (DAT-075/111).
func dpState(id, batch string) *EffectiveState {
	return &EffectiveState{Daypart: &Daypart{ID: id, PresetBatchID: batch}, Display: displayContent}
}

// DAT-075: nil→D1 is a rising edge of effective-daypart identity; the bound
// preset fires once.
func TestPresetTransitionRisingEdgeToDaypart(t *testing.T) {
	got := PresetTransition(nil, dpState("D1", "P1"))
	if got == nil || got.PresetBatchID != "P1" || got.DaypartID != "D1" {
		t.Fatalf("rising edge to D1 = %+v, want fire P1/D1", got)
	}
}

// DAT-075/DAT-119: a single daypart whose window spans the whole fall-back
// repeated hour keeps ONE identity across the repeat — identity unchanged, so no
// re-fire (this is the spanning-window carve-out DAT-119 pins).
func TestPresetTransitionIdentityUnchangedNoRefire(t *testing.T) {
	prev := dpState("D1", "P1")
	cur := dpState("D1", "P1")
	if got := PresetTransition(prev, cur); got != nil {
		t.Fatalf("identity unchanged must not re-fire, got %+v", got)
	}
}

// DAT-075/DAT-111: a fall-through — a higher-precedence daypart ends and the base
// daypart becomes effective again — is itself a rising edge and fires the base
// preset.
func TestPresetTransitionFallThroughFires(t *testing.T) {
	prev := dpState("HOLI", "PH")
	cur := dpState("BASE", "PB")
	if got := PresetTransition(prev, cur); got == nil || got.PresetBatchID != "PB" || got.DaypartID != "BASE" {
		t.Fatalf("fall-through = %+v, want fire PB/BASE", got)
	}
}

// DAT-075: a transition to NO effective daypart (masked-away/none/fallback/
// terminal) fires nothing.
func TestPresetTransitionToNoneDoesNotFire(t *testing.T) {
	prev := dpState("D1", "P1")
	cur := &EffectiveState{Display: displayBlank}
	if got := PresetTransition(prev, cur); got != nil {
		t.Fatalf("transition to no daypart must not fire, got %+v", got)
	}
}

// DAT-075: a rising edge into a daypart that binds no preset_batch_id fires
// nothing (the identity still advances; there is simply no batch to invoke).
func TestPresetTransitionRisingEdgeNoBoundPreset(t *testing.T) {
	if got := PresetTransition(nil, dpState("D1", "")); got != nil {
		t.Fatalf("no bound preset must not fire, got %+v", got)
	}
}

// DAT-092/RUL-172: BatchOutcome classifies a mixed result set as partial, an
// all-ok set as complete, and an all-fail set as failed, carrying the per-command
// results and the evaluation instant byte-exact.
func TestBatchOutcomeClassification(t *testing.T) {
	mixed := []CommandResult{
		{Target: "a", Command: "power", OK: true},
		{Target: "b", Command: "power", OK: false, Error: "unreachable"},
	}
	out := BatchOutcome(mixed, 1752537600000)
	if out.Outcome != "partial" || len(out.Results) != 2 || out.EvaluatedAt != 1752537600000 {
		t.Fatalf("mixed = %+v, want partial/2/ts", out)
	}
	if out.Results[1].Error != "unreachable" {
		t.Fatalf("per-command error not preserved: %+v", out.Results[1])
	}
	if BatchOutcome([]CommandResult{{OK: true}, {OK: true}}, 0).Outcome != "complete" {
		t.Fatalf("all-ok want complete")
	}
	if BatchOutcome([]CommandResult{{OK: false}, {OK: false}}, 0).Outcome != "failed" {
		t.Fatalf("all-fail want failed")
	}
}

// DAT-076/121: a schedule with no misfire defaults to catch_up_once — this
// contract's recurring-state default, distinct from rules/1's one-shot skip.
func TestScheduleEffectiveMisfire(t *testing.T) {
	if got := (Schedule{}).EffectiveMisfire(); got != "catch_up_once" {
		t.Fatalf("schedule default = %q, want catch_up_once", got)
	}
	if got := (Schedule{Misfire: "fire_each"}).EffectiveMisfire(); got != "fire_each" {
		t.Fatalf("schedule own = %q, want fire_each", got)
	}
}

// DAT-076: a daypart's effective misfire is its own, else its owning schedule's,
// else catch_up_once.
func TestDaypartEffectiveMisfireByKind(t *testing.T) {
	owning := Schedule{Misfire: "skip"}
	if got := (Daypart{}).EffectiveMisfire(owning); got != "skip" {
		t.Fatalf("inherits schedule = %q, want skip", got)
	}
	if got := (Daypart{Misfire: "fire_each"}).EffectiveMisfire(owning); got != "fire_each" {
		t.Fatalf("own wins = %q, want fire_each", got)
	}
	if got := (Daypart{}).EffectiveMisfire(Schedule{}); got != "catch_up_once" {
		t.Fatalf("terminal default = %q, want catch_up_once", got)
	}
}
