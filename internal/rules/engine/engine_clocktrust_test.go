package engine

import (
	"testing"

	"github.com/maaxton/waiveo-next/internal/rules/clock"
	"github.com/maaxton/waiveo-next/internal/rules/registry"
	"github.com/maaxton/waiveo-next/internal/rules/state"
)

// TestUntrustedWindowFlooredThenTrustedReplaysCatchUp is the RUL-370/371 oracle:
// while the clock is untrusted a schedule occurrence does NOT fire live (the wall
// is unverified, RUL-370) and the wall cursor does not advance; on the
// untrusted->trusted transition that occurrence — whose scheduled instant fell in
// the untrusted window — is re-evaluated immediately (RUL-371) and routed through
// the trigger's misfire policy: catch_up_once collapses it to exactly one caught-up
// firing (ran, misfire_caught: true), not a live tick and not a silent drop.
func TestUntrustedWindowFlooredThenTrustedReplaysCatchUp(t *testing.T) {
	reg := registry.FixtureRegistry{}
	clk := clock.NewFakeClock()
	sink := &recordingSink{}

	e := New(reg, clk, sink, nil)
	if err := e.SetLocation("UTC", 0, 0); err != nil {
		t.Fatalf("SetLocation: %v", err)
	}
	loadTimeRule(t, reg, e, "01J8Z3K4N5P6Q7R8S9T0TRUST01",
		`{"type":"time","at":"08:00:00","misfire":"catch_up_once"}`)

	floor := utcMillis(t, 2026, 6, 1, 7, 0, 0)
	e.SeedScheduleFloor(floor)
	primeLiveTick(e, floor) // engine already ticking live from the floor

	// Clock goes untrusted; a Tick across the 08:00 occurrence must NOT fire live
	// and must NOT advance the wall cursor (RUL-370).
	if _, err := e.SetClockTrust("untrusted"); err != nil {
		t.Fatalf("SetClockTrust(untrusted): %v", err)
	}
	clk.SetWall(utcMillis(t, 2026, 6, 1, 9, 0, 0))
	if d := e.Tick(clk); len(d) != 0 {
		t.Fatalf("untrusted Tick fired live: %+v", d)
	}
	if len(sink.calls) != 0 {
		t.Fatalf("untrusted Tick dispatched: %+v", sink.calls)
	}
	if e.lastTickWall != floor {
		t.Fatalf("untrusted Tick advanced the wall cursor to %d, want unchanged %d", e.lastTickWall, floor)
	}

	// Transition to trusted: the 08:00 occurrence in the untrusted window is replayed
	// through catch_up_once — exactly one caught-up firing.
	disps, err := e.SetClockTrust("trusted")
	if err != nil {
		t.Fatalf("SetClockTrust(trusted): %v", err)
	}
	if len(disps) != 1 {
		t.Fatalf("transition replay = %d fires, want 1 (catch_up_once collapse)", len(disps))
	}
	if disps[0].Disposition != Ran || !disps[0].MisfireCaught {
		t.Fatalf("replayed fire = %+v, want ran + misfire_caught", disps[0])
	}
	if len(sink.calls) != 1 || sink.calls[0].Command != "power" {
		t.Fatalf("want exactly one caught-up dispatch, got %+v", sink.calls)
	}
}

// TestUntrustedWindowSkipReplaysNothing: a schedule trigger with no explicit
// misfire (defaults to skip) whose occurrence fell in the untrusted window does
// not fire on the trusted transition — a missed one-shot does not fire late,
// RUL-354/371.
func TestUntrustedWindowSkipReplaysNothing(t *testing.T) {
	reg := registry.FixtureRegistry{}
	clk := clock.NewFakeClock()
	sink := &recordingSink{}

	e := New(reg, clk, sink, nil)
	if err := e.SetLocation("UTC", 0, 0); err != nil {
		t.Fatalf("SetLocation: %v", err)
	}
	loadTimeRule(t, reg, e, "01J8Z3K4N5P6Q7R8S9T0TRUST02", `{"type":"time","at":"08:00:00"}`)

	floor := utcMillis(t, 2026, 6, 1, 7, 0, 0)
	e.SeedScheduleFloor(floor)
	primeLiveTick(e, floor)

	if _, err := e.SetClockTrust("untrusted"); err != nil {
		t.Fatalf("SetClockTrust(untrusted): %v", err)
	}
	clk.SetWall(utcMillis(t, 2026, 6, 1, 9, 0, 0))
	e.Tick(clk)

	disps, err := e.SetClockTrust("trusted")
	if err != nil {
		t.Fatalf("SetClockTrust(trusted): %v", err)
	}
	if len(disps) != 0 {
		t.Fatalf("default-skip missed occurrence fired on the trusted transition: %+v", disps)
	}
	if len(sink.calls) != 0 {
		t.Fatalf("default-skip missed occurrence dispatched: %+v", sink.calls)
	}
}

// TestTrustedTransitionReplaysFireEachEach: fire_each replays every occurrence in
// the untrusted window in chronological order on the trusted transition, each
// marked misfire_caught — the transition is a misfire replay (misfire_caught:true),
// never a live tick (which would be misfire_caught:false), RUL-353/371.
func TestTrustedTransitionReplaysFireEachEach(t *testing.T) {
	reg := registry.FixtureRegistry{}
	clk := clock.NewFakeClock()
	sink := &recordingSink{}

	e := New(reg, clk, sink, nil)
	if err := e.SetLocation("UTC", 0, 0); err != nil {
		t.Fatalf("SetLocation: %v", err)
	}
	loadTimeRule(t, reg, e, "01J8Z3K4N5P6Q7R8S9T0TRUST03",
		`{"type":"time_pattern","minutes":"/15","seconds":0,"misfire":"fire_each"}`)

	floor := utcMillis(t, 2026, 6, 1, 10, 0, 0)
	e.SeedScheduleFloor(floor)
	primeLiveTick(e, floor)

	if _, err := e.SetClockTrust("untrusted"); err != nil {
		t.Fatalf("SetClockTrust(untrusted): %v", err)
	}
	clk.SetWall(utcMillis(t, 2026, 6, 1, 10, 50, 0)) // 10:15/10:30/10:45 in the untrusted window
	if d := e.Tick(clk); len(d) != 0 {
		t.Fatalf("untrusted Tick fired live: %+v", d)
	}

	disps, err := e.SetClockTrust("trusted")
	if err != nil {
		t.Fatalf("SetClockTrust(trusted): %v", err)
	}
	if len(disps) != 3 {
		t.Fatalf("fire_each replayed %d, want 3 (10:15/10:30/10:45)", len(disps))
	}
	for i, d := range disps {
		if d.Disposition != Ran {
			t.Fatalf("replay %d disposition = %q, want ran", i, d.Disposition)
		}
		if !d.MisfireCaught {
			t.Fatalf("replay %d must carry misfire_caught: true (a transition replay is a misfire, not a live tick)", i)
		}
	}
	if len(sink.calls) != 3 {
		t.Fatalf("want 3 caught-up dispatches, got %+v", sink.calls)
	}
}

// TestUntrustedTransitionNeverFiresBelowFloor: an occurrence below the persisted
// trust floor is never enumerated on the trusted transition, so no caught-up fire
// rests on a clock reading earlier than the floor (RUL-370). The floor sits after
// the 08:00 occurrence, so the untrusted window's true lower bound is the floor.
func TestUntrustedTransitionNeverFiresBelowFloor(t *testing.T) {
	reg := registry.FixtureRegistry{}
	clk := clock.NewFakeClock()
	sink := &recordingSink{}

	e := New(reg, clk, sink, nil)
	if err := e.SetLocation("UTC", 0, 0); err != nil {
		t.Fatalf("SetLocation: %v", err)
	}
	loadTimeRule(t, reg, e, "01J8Z3K4N5P6Q7R8S9T0TRUST04",
		`{"type":"time","at":"08:00:00","misfire":"catch_up_once"}`)

	// Floor AFTER the 08:00 occurrence: it is below the floor and never enumerated.
	e.SeedScheduleFloor(utcMillis(t, 2026, 6, 1, 8, 30, 0))

	if _, err := e.SetClockTrust("untrusted"); err != nil {
		t.Fatalf("SetClockTrust(untrusted): %v", err)
	}
	clk.SetWall(utcMillis(t, 2026, 6, 1, 9, 0, 0))
	e.Tick(clk)

	disps, err := e.SetClockTrust("trusted")
	if err != nil {
		t.Fatalf("SetClockTrust(trusted): %v", err)
	}
	if len(disps) != 0 {
		t.Fatalf("occurrence below the floor fired on the trusted transition: %+v", disps)
	}
	if len(sink.calls) != 0 {
		t.Fatalf("occurrence below the floor dispatched: %+v", sink.calls)
	}
}

// TestUntrustedTickDoesNotEnumerateOrAdvance: while untrusted the wall clock is not
// read and no occurrence enumerator is invoked (RUL-370 — evaluate against the
// floor, not the unverified wall); the wall cursor is left untouched. On the trusted
// transition the enumerator runs over the whole untrusted window and its occurrence
// is replayed through misfire.
func TestUntrustedTickDoesNotEnumerateOrAdvance(t *testing.T) {
	reg := registry.FixtureRegistry{}
	clk := clock.NewFakeClock()
	sink := &recordingSink{}

	e := New(reg, clk, sink, nil)
	if err := e.SetLocation("UTC", 0, 0); err != nil {
		t.Fatalf("SetLocation: %v", err)
	}
	scheduleRule(t, reg, e, "01J8Z3K4N5P6Q7R8S9T0TRUST05")
	e.SeedScheduleFloor(1_000)
	primeLiveTick(e, 1_000)

	spy := &spyEnumerator{emit: []int64{5_000}}
	registerSchedule(e, spy, "catch_up_once")

	if _, err := e.SetClockTrust("untrusted"); err != nil {
		t.Fatalf("SetClockTrust(untrusted): %v", err)
	}
	clk.SetWall(10_000)
	if d := e.Tick(clk); len(d) != 0 {
		t.Fatalf("untrusted Tick fired: %+v", d)
	}
	if len(spy.calls) != 0 {
		t.Fatalf("untrusted Tick enumerated the wall clock: %+v", spy.calls)
	}
	if e.lastTickWall != 1_000 {
		t.Fatalf("untrusted Tick advanced the cursor to %d, want unchanged 1000", e.lastTickWall)
	}

	// Trusted transition: enumerate the whole untrusted window (1000, 10000] and
	// replay the occurrence through catch_up_once.
	disps, err := e.SetClockTrust("trusted")
	if err != nil {
		t.Fatalf("SetClockTrust(trusted): %v", err)
	}
	if len(spy.calls) != 1 || spy.calls[0].from != 1_000 || spy.calls[0].to != 10_000 {
		t.Fatalf("transition enumeration = %+v, want one call over (1000, 10000]", spy.calls)
	}
	if len(disps) != 1 || !disps[0].MisfireCaught {
		t.Fatalf("transition replay = %+v, want one misfire_caught fire", disps)
	}
	if e.lastTickWall != 10_000 {
		t.Fatalf("transition did not advance the cursor to 10000: %d", e.lastTickWall)
	}
}

// TestSetClockTrustRejectsUnknownState: an unrecognized trust state is refused and
// leaves the prior trust state unchanged (fail-closed).
func TestSetClockTrustRejectsUnknownState(t *testing.T) {
	reg := registry.FixtureRegistry{}
	clk := clock.NewFakeClock()
	e := New(reg, clk, &recordingSink{}, nil)

	if _, err := e.SetClockTrust("bogus"); err == nil {
		t.Fatalf("SetClockTrust accepted an unknown state")
	}
	// Default is trusted; a rejected call must not have flipped it to untrusted.
	if e.clockUntrusted {
		t.Fatalf("a rejected SetClockTrust flipped the engine to untrusted")
	}
	if _, err := e.SetClockTrust("untrusted"); err != nil {
		t.Fatalf("SetClockTrust(untrusted): %v", err)
	}
	if _, err := e.SetClockTrust("garbage"); err == nil {
		t.Fatalf("SetClockTrust accepted an unknown state")
	}
	if !e.clockUntrusted {
		t.Fatalf("a rejected SetClockTrust clobbered the untrusted state")
	}
}

// TestUntrustedFloorsTimeConditionEvaluation is the RUL-370 condition oracle: while
// untrusted a `time` condition evaluates against the persisted floor's time-of-day,
// not the unverified live wall. The state trigger fires via Observe; the condition
// window (08:00–09:00) contains the floor (08:30) but not the live wall (12:00), so
// the floored evaluation passes and the action dispatches.
func TestUntrustedFloorsTimeConditionEvaluation(t *testing.T) {
	reg := registry.FixtureRegistry{}
	clk := clock.NewFakeClock()
	sink := &recordingSink{}

	e := New(reg, clk, sink, nil)
	if err := e.SetLocation("UTC", 0, 0); err != nil {
		t.Fatalf("SetLocation: %v", err)
	}
	entry, rule := compileRule(t, `{
		"id":"01J8Z3K4N5P6Q7R8S9T0TRUST06",
		"triggers":[{"type":"state","entity_id":"`+subjectEntity+`","to":["on"]}],
		"conditions":[{"type":"time","after":"08:00:00","before":"09:00:00"}],
		"actions":[{"type":"device_command","entity_id":"`+subjectEntity+`","command":"power"}]
	}`)
	if err := e.Load(entry, rule); err != nil {
		t.Fatalf("Load: %v", err)
	}

	e.SeedScheduleFloor(utcMillis(t, 2026, 6, 1, 8, 30, 0)) // inside the window
	clk.SetWall(utcMillis(t, 2026, 6, 1, 12, 0, 0))         // outside the window

	if _, err := e.SetClockTrust("untrusted"); err != nil {
		t.Fatalf("SetClockTrust(untrusted): %v", err)
	}
	e.Observe(state.NewObservation(reg, ent(subjectEntity, "off"), ent(subjectEntity, "off"))) // prime baseline
	disps := e.Observe(state.NewObservation(reg, ent(subjectEntity, "off"), ent(subjectEntity, "on")))

	if len(disps) != 1 || disps[0].Disposition != Ran {
		t.Fatalf("floored time condition did not pass: %+v", disps)
	}
	if len(sink.calls) != 1 {
		t.Fatalf("want one dispatch (condition floored to 08:30, in-window), got %+v", sink.calls)
	}
}

// TestTrustedTimeConditionUsesLiveWall is the trusted control for the floor oracle:
// with the same window and the same live wall (12:00, out of window), a trusted
// engine evaluates the `time` condition against the live wall — so it fails and the
// action does not dispatch. This isolates the flooring to the untrusted state.
func TestTrustedTimeConditionUsesLiveWall(t *testing.T) {
	reg := registry.FixtureRegistry{}
	clk := clock.NewFakeClock()
	sink := &recordingSink{}

	e := New(reg, clk, sink, nil)
	if err := e.SetLocation("UTC", 0, 0); err != nil {
		t.Fatalf("SetLocation: %v", err)
	}
	entry, rule := compileRule(t, `{
		"id":"01J8Z3K4N5P6Q7R8S9T0TRUST07",
		"triggers":[{"type":"state","entity_id":"`+subjectEntity+`","to":["on"]}],
		"conditions":[{"type":"time","after":"08:00:00","before":"09:00:00"}],
		"actions":[{"type":"device_command","entity_id":"`+subjectEntity+`","command":"power"}]
	}`)
	if err := e.Load(entry, rule); err != nil {
		t.Fatalf("Load: %v", err)
	}

	e.SeedScheduleFloor(utcMillis(t, 2026, 6, 1, 8, 30, 0)) // in-window, but trusted ignores the floor
	clk.SetWall(utcMillis(t, 2026, 6, 1, 12, 0, 0))         // live wall, out of window

	e.Observe(state.NewObservation(reg, ent(subjectEntity, "off"), ent(subjectEntity, "off")))
	disps := e.Observe(state.NewObservation(reg, ent(subjectEntity, "off"), ent(subjectEntity, "on")))

	if len(disps) != 0 {
		t.Fatalf("trusted time condition fired against the live wall unexpectedly: %+v", disps)
	}
	if len(sink.calls) != 0 {
		t.Fatalf("trusted out-of-window condition dispatched: %+v", sink.calls)
	}
}
