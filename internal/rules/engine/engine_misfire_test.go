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
	if e.rules[0].run == nil {
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

// TestFirstTickSkipDoesNotFireLate is the RUL-351/354 oracle on the PUBLIC Tick
// path (the running engine's only schedule integration point): a `time` trigger
// with no explicit misfire (defaults to skip) whose 08:00 occurrence was missed
// while the engine was down does NOT fire late on the first Tick after restart —
// that first Tick is a resume that routes the missed span through the misfire
// policy, not a live tick firing every enumerated occurrence (RUL-350/351/354).
func TestFirstTickSkipDoesNotFireLate(t *testing.T) {
	reg := registry.FixtureRegistry{}
	clk := clock.NewFakeClock()
	sink := &recordingSink{}

	e := New(reg, clk, sink, nil)
	if err := e.SetLocation("UTC", 0, 0); err != nil {
		t.Fatalf("SetLocation: %v", err)
	}
	loadTimeRule(t, reg, e, "01J8Z3K4N5P6Q7R8S9T0MISFR6", `{"type":"time","at":"08:00:00"}`)

	// Engine down from before 08:00; the first Tick after restart lands at 09:00.
	e.SeedScheduleFloor(utcMillis(t, 2026, 6, 1, 7, 0, 0))
	clk.SetWall(utcMillis(t, 2026, 6, 1, 9, 0, 0))

	if d := e.Tick(clk); len(d) != 0 {
		t.Fatalf("missed skip occurrence fired late on the first Tick: %+v", d)
	}
	if len(sink.calls) != 0 {
		t.Fatalf("missed skip occurrence dispatched on the first Tick: %+v", sink.calls)
	}
}

// TestFirstTickCatchUpOnceCollapses is the RUL-352/355 oracle on the PUBLIC Tick
// path: a `time_pattern` hourly trigger declaring catch_up_once whose 07:00/08:00/
// 09:00 occurrences were all missed while the engine was down collapses to EXACTLY
// ONE caught-up firing on the first Tick after restart, marked misfire_caught.
func TestFirstTickCatchUpOnceCollapses(t *testing.T) {
	reg := registry.FixtureRegistry{}
	clk := clock.NewFakeClock()
	sink := &recordingSink{}

	e := New(reg, clk, sink, nil)
	if err := e.SetLocation("UTC", 0, 0); err != nil {
		t.Fatalf("SetLocation: %v", err)
	}
	loadTimeRule(t, reg, e, "01J8Z3K4N5P6Q7R8S9T0MISFR7",
		`{"type":"time_pattern","hours":"/1","minutes":0,"seconds":0,"misfire":"catch_up_once"}`)

	e.SeedScheduleFloor(utcMillis(t, 2026, 6, 1, 6, 0, 0)) // exclude 06:00
	clk.SetWall(utcMillis(t, 2026, 6, 1, 9, 5, 0))         // 07/08/09 missed

	disps := e.Tick(clk)
	if len(disps) != 1 {
		t.Fatalf("catch_up_once did not collapse on the first Tick: got %d (%+v)", len(disps), disps)
	}
	if disps[0].Disposition != Ran || !disps[0].MisfireCaught {
		t.Fatalf("caught-up fire = %+v, want ran + misfire_caught", disps[0])
	}
	if len(sink.calls) != 1 {
		t.Fatalf("want one caught-up dispatch, got %+v", sink.calls)
	}
}

// TestFirstTickResumeThenLiveTick proves the two-phase Tick behavior: the first
// Tick after restart is a misfire resume (a `skip` schedule catches nothing up),
// and a SUBSEQUENT live Tick fires the same schedule normally at its instant with
// misfire_caught=false — a skip schedule fires live while the engine is up, and is
// suppressed only when missed across downtime (RUL-354/041/340).
func TestFirstTickResumeThenLiveTick(t *testing.T) {
	reg := registry.FixtureRegistry{}
	clk := clock.NewFakeClock()
	sink := &recordingSink{}

	e := New(reg, clk, sink, nil)
	if err := e.SetLocation("UTC", 0, 0); err != nil {
		t.Fatalf("SetLocation: %v", err)
	}
	loadTimeRule(t, reg, e, "01J8Z3K4N5P6Q7R8S9T0MISFR8", `{"type":"time_pattern","hours":"/1","minutes":0,"seconds":0}`)

	// First Tick: the resume window (08:30, 08:45] holds no hourly occurrence, so
	// skip catches nothing up; the wall cursor advances to 08:45.
	e.SeedScheduleFloor(utcMillis(t, 2026, 6, 1, 8, 30, 0))
	clk.SetWall(utcMillis(t, 2026, 6, 1, 8, 45, 0))
	if d := e.Tick(clk); len(d) != 0 {
		t.Fatalf("first-Tick resume fired unexpectedly: %+v", d)
	}

	// Second Tick is live: the 09:00 occurrence fires normally, not misfire_caught,
	// even though the trigger has no explicit misfire (defaults to skip).
	clk.SetWall(utcMillis(t, 2026, 6, 1, 9, 0, 1))
	disps := e.Tick(clk)
	if len(disps) != 1 || disps[0].Disposition != Ran {
		t.Fatalf("live Tick did not fire the 09:00 occurrence: %+v", disps)
	}
	if disps[0].MisfireCaught {
		t.Fatalf("a live-tick fire must not be misfire_caught: %+v", disps[0])
	}
	if len(sink.calls) != 1 {
		t.Fatalf("want one live dispatch, got %+v", sink.calls)
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
