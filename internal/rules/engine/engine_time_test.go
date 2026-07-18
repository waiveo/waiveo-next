package engine

import (
	"testing"
	"time"

	"github.com/maaxton/waiveo-next/internal/rules/clock"
	"github.com/maaxton/waiveo-next/internal/rules/registry"
	"github.com/maaxton/waiveo-next/internal/rules/state"
)

// loadTimeRule loads a schedule-triggered edge rule and primes the media-player
// target into the snapshot so a fired occurrence's device_command resolves its
// command vocabulary. The rule carries no state trigger, so the priming
// observation fires nothing.
func loadTimeRule(t *testing.T, reg registry.Registry, e *Engine, id, triggerJSON string) {
	t.Helper()
	entry, rule := compileRule(t, `{
		"id":"`+id+`",
		"triggers":[`+triggerJSON+`],
		"actions":[{"type":"device_command","entity_id":"`+subjectEntity+`","command":"power"}]
	}`)
	if err := e.Load(entry, rule); err != nil {
		t.Fatalf("Load: %v", err)
	}
	e.Observe(state.NewObservation(reg, ent(subjectEntity, "off"), ent(subjectEntity, "off")))
}

func utcMillis(t *testing.T, y int, mo time.Month, d, h, mi, s int) int64 {
	t.Helper()
	return time.Date(y, mo, d, h, mi, s, 0, time.UTC).UnixMilli()
}

// TestBuildTimeTriggerDispatchesOnTick: buildTrigger wires a `time` trigger from
// the rule's own member (kind "time"), and Tick with the wall clock advanced past
// the local occurrence routes it through fire() exactly once — one `ran`, one
// dispatch, not misfire-caught (RUL-040/041/340).
func TestBuildTimeTriggerDispatchesOnTick(t *testing.T) {
	reg := registry.FixtureRegistry{}
	clk := clock.NewFakeClock()
	sink := &recordingSink{}

	e := New(reg, clk, sink, nil)
	if err := e.SetLocation("UTC", 0, 0); err != nil {
		t.Fatalf("SetLocation: %v", err)
	}
	loadTimeRule(t, reg, e, "01J8Z3K4N5P6Q7R8S9T0TIMETR", `{"type":"time","at":"09:00:00"}`)

	// Floor to the start of the target day so enumeration is bounded, not from epoch.
	e.SeedScheduleFloor(utcMillis(t, 2026, 6, 1, 0, 0, 0))
	clk.SetWall(utcMillis(t, 2026, 6, 1, 9, 0, 1))

	disps := e.Tick(clk)
	if len(disps) != 1 || disps[0].Disposition != Ran {
		t.Fatalf("want one ran disposition, got %+v", disps)
	}
	if disps[0].MisfireCaught {
		t.Fatalf("a live time occurrence must not be misfire_caught")
	}
	if len(sink.calls) != 1 || sink.calls[0].Command != "power" {
		t.Fatalf("want one power dispatch, got %+v", sink.calls)
	}

	// A subsequent Tick within the same day does not re-fire (the wall cursor
	// advanced past 09:00:00).
	clk.SetWall(utcMillis(t, 2026, 6, 1, 9, 30, 0))
	if d := e.Tick(clk); len(d) != 0 {
		t.Fatalf("time trigger re-fired within the same day: %+v", d)
	}
}

// TestBuildTimePatternTriggerDispatchesEachOccurrence: buildTrigger wires a
// `time_pattern` trigger (kind "time_pattern") with number-and-string fields, and
// Tick dispatches every matching quarter-hour in the elapsed window (RUL-050/051).
func TestBuildTimePatternTriggerDispatchesEachOccurrence(t *testing.T) {
	reg := registry.FixtureRegistry{}
	clk := clock.NewFakeClock()
	sink := &recordingSink{}

	e := New(reg, clk, sink, nil)
	if err := e.SetLocation("UTC", 0, 0); err != nil {
		t.Fatalf("SetLocation: %v", err)
	}
	loadTimeRule(t, reg, e, "01J8Z3K4N5P6Q7R8S9T0PATTRN", `{"type":"time_pattern","minutes":"/15","seconds":0}`)

	e.SeedScheduleFloor(utcMillis(t, 2026, 6, 1, 10, 0, 0)) // exclude the 10:00:00 fire
	clk.SetWall(utcMillis(t, 2026, 6, 1, 11, 0, 0))         // include the 11:00:00 fire

	disps := e.Tick(clk)
	// 10:15, 10:30, 10:45, 11:00 -> 4 occurrences.
	if len(disps) != 4 {
		t.Fatalf("want 4 dispositions, got %d (%+v)", len(disps), disps)
	}
	for i, d := range disps {
		if d.Disposition != Ran {
			t.Fatalf("occurrence %d disposition = %q, want ran", i, d.Disposition)
		}
	}
	if len(sink.calls) != 4 {
		t.Fatalf("want 4 dispatches, got %d", len(sink.calls))
	}
}

// TestBuildTimeTriggerSpringForwardSkipDispatch is the RUL-341 oracle at the
// engine level: a `time` trigger at 02:30 in America/New_York enumerated across a
// Tick spanning the 2026-03-08 spring-forward gap does NOT dispatch for that
// date's removed local time, but dispatches for 03-07 and 03-09 (RUL-341).
func TestBuildTimeTriggerSpringForwardSkipDispatch(t *testing.T) {
	reg := registry.FixtureRegistry{}
	clk := clock.NewFakeClock()
	sink := &recordingSink{}

	e := New(reg, clk, sink, nil)
	if err := e.SetLocation("America/New_York", 40.7128, -74.0060); err != nil {
		t.Fatalf("SetLocation: %v", err)
	}
	loadTimeRule(t, reg, e, "01J8Z3K4N5P6Q7R8S9T0DSTSKP", `{"type":"time","at":"02:30:00"}`)

	ny, _ := time.LoadLocation("America/New_York")
	e.SeedScheduleFloor(time.Date(2026, 3, 7, 0, 0, 0, 0, ny).UnixMilli())
	clk.SetWall(time.Date(2026, 3, 10, 0, 0, 0, 0, ny).UnixMilli())

	disps := e.Tick(clk)
	// 03-07 and 03-09 fire; 03-08 (gap) does not.
	if len(disps) != 2 {
		t.Fatalf("want 2 dispositions (03-07, 03-09), got %d (%+v)", len(disps), disps)
	}
	if len(sink.calls) != 2 {
		t.Fatalf("want 2 dispatches, got %d", len(sink.calls))
	}
}

// TestBuildTimeTriggerFallBackSingleDispatch is the RUL-342 oracle at the engine
// level: a `time` trigger at 01:30 in America/New_York enumerated across the
// 2026-11-01 fall-back Tick dispatches exactly once (first absolute occurrence),
// never twice (RUL-342).
func TestBuildTimeTriggerFallBackSingleDispatch(t *testing.T) {
	reg := registry.FixtureRegistry{}
	clk := clock.NewFakeClock()
	sink := &recordingSink{}

	e := New(reg, clk, sink, nil)
	if err := e.SetLocation("America/New_York", 40.7128, -74.0060); err != nil {
		t.Fatalf("SetLocation: %v", err)
	}
	loadTimeRule(t, reg, e, "01J8Z3K4N5P6Q7R8S9T0DSTFBK", `{"type":"time","at":"01:30:00"}`)

	ny, _ := time.LoadLocation("America/New_York")
	e.SeedScheduleFloor(time.Date(2026, 11, 1, 0, 0, 0, 0, ny).UnixMilli())
	clk.SetWall(time.Date(2026, 11, 2, 0, 0, 0, 0, ny).UnixMilli())

	disps := e.Tick(clk)
	if len(disps) != 1 || disps[0].Disposition != Ran {
		t.Fatalf("want exactly one ran disposition, got %+v", disps)
	}
	if len(sink.calls) != 1 {
		t.Fatalf("want exactly one dispatch, got %d", len(sink.calls))
	}
}
