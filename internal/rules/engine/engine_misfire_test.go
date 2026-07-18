package engine

import (
	"testing"

	"github.com/maaxton/waiveo-next/internal/rules/clock"
	"github.com/maaxton/waiveo-next/internal/rules/registry"
	"github.com/maaxton/waiveo-next/internal/rules/state"
)

// TestReplayMissedCatchUpOnceCollapses is the RUL-352 oracle at the engine level:
// a `time_pattern` trigger declaring misfire: catch_up_once, whose 07:00/08:00/
// 09:00 occurrences were all missed while the engine was offline, dispatches
// EXACTLY ONE firing on resume — collapsing the many missed occurrences into one
// (fire_count_on_resume: 1) — subject to normal mode evaluation (disposition
// `ran`, no run in progress) and marked misfire_caught: true (RUL-352/355/246).
func TestReplayMissedCatchUpOnceCollapses(t *testing.T) {
	reg := registry.FixtureRegistry{}
	clk := clock.NewFakeClock()
	sink := &recordingSink{}

	e := New(reg, clk, sink, nil)
	if err := e.SetLocation("UTC", 0, 0); err != nil {
		t.Fatalf("SetLocation: %v", err)
	}
	loadTimeRule(t, reg, e, "01J8Z3K4N5P6Q7R8S9T0MISFR1",
		`{"type":"time_pattern","hours":"/1","minutes":0,"misfire":"catch_up_once"}`)

	// Engine offline from 06:00; occurrences at 07/08/09 missed; resume at 09:05.
	floor := utcMillis(t, 2026, 6, 1, 6, 0, 0)
	resume := utcMillis(t, 2026, 6, 1, 9, 5, 0)
	disps := e.replayMissedSchedules(floor, resume)

	if len(disps) != 1 {
		t.Fatalf("fire_count_on_resume = %d, want 1 (catch_up_once collapses)", len(disps))
	}
	if disps[0].Disposition != Ran {
		t.Fatalf("disposition = %q, want ran", disps[0].Disposition)
	}
	if !disps[0].MisfireCaught {
		t.Fatalf("caught-up fire must carry misfire_caught: true")
	}
	if len(sink.calls) != 1 || sink.calls[0].Command != "power" {
		t.Fatalf("want exactly one power dispatch, got %+v", sink.calls)
	}
}

// TestReplayMissedDefaultSkip is the RUL-354 oracle at the engine level: a `time`
// trigger with NO explicit misfire defaults to skip; an occurrence missed while
// the engine was offline does not fire late once evaluation resumes (RUL-351/354).
func TestReplayMissedDefaultSkip(t *testing.T) {
	reg := registry.FixtureRegistry{}
	clk := clock.NewFakeClock()
	sink := &recordingSink{}

	e := New(reg, clk, sink, nil)
	if err := e.SetLocation("UTC", 0, 0); err != nil {
		t.Fatalf("SetLocation: %v", err)
	}
	loadTimeRule(t, reg, e, "01J8Z3K4N5P6Q7R8S9T0MISFR2", `{"type":"time","at":"08:00:00"}`)

	floor := utcMillis(t, 2026, 6, 1, 7, 0, 0)
	resume := utcMillis(t, 2026, 6, 1, 9, 0, 0)
	disps := e.replayMissedSchedules(floor, resume)

	if len(disps) != 0 {
		t.Fatalf("default skip fired late: %+v", disps)
	}
	if len(sink.calls) != 0 {
		t.Fatalf("default skip dispatched late: %+v", sink.calls)
	}
}

// TestReplayMissedFireEachDispatchesEach: `fire_each` dispatches every missed
// occurrence in chronological order, each subject to mode evaluation and each
// marked misfire_caught (RUL-353/355). With a synchronous action and no run in
// flight between them, each records `ran` and dispatches once.
func TestReplayMissedFireEachDispatchesEach(t *testing.T) {
	reg := registry.FixtureRegistry{}
	clk := clock.NewFakeClock()
	sink := &recordingSink{}

	e := New(reg, clk, sink, nil)
	if err := e.SetLocation("UTC", 0, 0); err != nil {
		t.Fatalf("SetLocation: %v", err)
	}
	loadTimeRule(t, reg, e, "01J8Z3K4N5P6Q7R8S9T0MISFR3",
		`{"type":"time_pattern","minutes":"/15","seconds":0,"misfire":"fire_each"}`)

	// 10:15, 10:30, 10:45 missed (floor excludes 10:00; resume before 11:00).
	floor := utcMillis(t, 2026, 6, 1, 10, 0, 0)
	resume := utcMillis(t, 2026, 6, 1, 10, 50, 0)
	disps := e.replayMissedSchedules(floor, resume)

	if len(disps) != 3 {
		t.Fatalf("fire_each dispatched %d, want 3 (10:15/10:30/10:45)", len(disps))
	}
	for i, d := range disps {
		if d.Disposition != Ran {
			t.Fatalf("occurrence %d disposition = %q, want ran", i, d.Disposition)
		}
		if !d.MisfireCaught {
			t.Fatalf("occurrence %d must carry misfire_caught: true", i)
		}
	}
	if len(sink.calls) != 3 {
		t.Fatalf("want 3 dispatches, got %d", len(sink.calls))
	}
}

// TestMisfireCaughtOrthogonalToModeSkip is the RUL-355 orthogonality oracle: a
// caught-up fire arriving while another run of the same single-mode rule is in
// progress resolves `skipped` per the mode (RUL-241) yet STILL carries
// misfire_caught: true — the marker is a fact about the firing's origin, not a
// fourth mode outcome (RUL-355/246).
func TestMisfireCaughtOrthogonalToModeSkip(t *testing.T) {
	reg := registry.FixtureRegistry{}
	clk := clock.NewFakeClock()
	sink := &recordingSink{}

	e := New(reg, clk, sink, nil)
	if err := e.SetLocation("UTC", 0, 0); err != nil {
		t.Fatalf("SetLocation: %v", err)
	}
	// A rule with a state trigger + a delay-tailed action leaves an in-flight run
	// after the first Observe; a manually-registered schedule trigger then supplies
	// the caught-up misfire that must resolve `skipped` under single mode.
	entry, rule := compileRule(t, `{
		"id":"01J8Z3K4N5P6Q7R8S9T0MISFR4",
		"mode":"single",
		"triggers":[{"type":"state","entity_id":"`+subjectEntity+`","to":["on"]}],
		"actions":[
			{"type":"delay","duration_seconds":30},
			{"type":"device_command","entity_id":"`+subjectEntity+`","command":"power"}
		]
	}`)
	if err := e.Load(entry, rule); err != nil {
		t.Fatalf("Load: %v", err)
	}
	// Register a catch_up_once schedule trigger emitting a fixed missed occurrence.
	registerSchedule(e, &spyEnumerator{emit: []int64{5_000}}, "catch_up_once")

	// Prime the baseline (RUL-300 suppresses the first observation), then start the
	// in-flight run: off -> on arms the delay (nothing dispatched yet).
	e.Observe(state.NewObservation(reg, ent(subjectEntity, "off"), ent(subjectEntity, "off")))
	e.Observe(state.NewObservation(reg, ent(subjectEntity, "off"), ent(subjectEntity, "on")))
	if e.run == nil {
		t.Fatalf("expected an in-flight delayed run after Observe")
	}

	disps := e.replayMissedSchedules(0, 10_000)
	if len(disps) != 1 {
		t.Fatalf("want one disposition, got %+v", disps)
	}
	if disps[0].Disposition != Skipped {
		t.Fatalf("disposition = %q, want skipped (single mode, run in flight)", disps[0].Disposition)
	}
	if !disps[0].MisfireCaught {
		t.Fatalf("a caught-up fire dropped by single mode must still carry misfire_caught: true")
	}
}

// TestReplayMissedRespectsFloor: no missed occurrence is enumerated below the
// persisted trust floor, so no caught-up fire ever rests on a clock reading
// earlier than the floor (RUL-370).
func TestReplayMissedRespectsFloor(t *testing.T) {
	reg := registry.FixtureRegistry{}
	clk := clock.NewFakeClock()
	sink := &recordingSink{}

	e := New(reg, clk, sink, nil)
	if err := e.SetLocation("UTC", 0, 0); err != nil {
		t.Fatalf("SetLocation: %v", err)
	}
	loadTimeRule(t, reg, e, "01J8Z3K4N5P6Q7R8S9T0MISFR5",
		`{"type":"time","at":"08:00:00","misfire":"catch_up_once"}`)

	// Floor is AFTER the 08:00 occurrence, so the window's true lower bound is the
	// floor: the 08:00 occurrence is below it and is never enumerated.
	e.SeedScheduleFloor(utcMillis(t, 2026, 6, 1, 8, 30, 0))
	disps := e.replayMissedSchedules(utcMillis(t, 2026, 6, 1, 7, 0, 0), utcMillis(t, 2026, 6, 1, 9, 0, 0))
	if len(disps) != 0 {
		t.Fatalf("occurrence below floor fired: %+v", disps)
	}
	if len(sink.calls) != 0 {
		t.Fatalf("occurrence below floor dispatched: %+v", sink.calls)
	}
}
