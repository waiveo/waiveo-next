package engine

import (
	"testing"

	"github.com/maaxton/waiveo-next/internal/rules/clock"
	"github.com/maaxton/waiveo-next/internal/rules/registry"
	"github.com/maaxton/waiveo-next/internal/rules/schedule"
	"github.com/maaxton/waiveo-next/internal/rules/state"
)

// spyEnumerator is a schedule.ScheduleTrigger test double: it records every
// (loc, from, to) call the engine's Tick path makes and returns a fixed list of
// occurrence instants. It lets Task 1's plumbing tests assert the exact wall
// interval enumeration is driven over, without a real (Task 2/3) enumerator.
type spyEnumerator struct {
	emit  []int64 // occurrence instants to return on each call
	calls []spyCall
}

type spyCall struct {
	tzName string
	lat    float64
	lon    float64
	from   int64
	to     int64
}

func (s *spyEnumerator) Occurrences(loc schedule.Location, from, to int64) []int64 {
	tz := ""
	if loc.TZ != nil {
		tz = loc.TZ.String()
	}
	s.calls = append(s.calls, spyCall{tzName: tz, lat: loc.Lat, lon: loc.Lon, from: from, to: to})
	return s.emit
}

// scheduleRule is a minimal edge rule (empty conditions, one device_command)
// used as the fire()-target host for a manually-registered schedule trigger. Its
// own state trigger never fires during a Tick — it is inert plumbing for the
// schedule occurrences routed through fire().
func scheduleRule(t *testing.T, reg registry.Registry, e *Engine, id string) {
	t.Helper()
	entry, rule := compileRule(t, `{
		"id":"`+id+`",
		"triggers":[{"type":"state","entity_id":"`+subjectEntity+`","to":["on"]}],
		"actions":[{"type":"device_command","entity_id":"`+subjectEntity+`","command":"power"}]
	}`)
	if err := e.Load(entry, rule); err != nil {
		t.Fatalf("Load: %v", err)
	}
	// Prime the snapshot with the media-player subject so a fired occurrence's
	// device_command resolves the target's device class (RUL-160 command-
	// vocabulary check). This off->off reconfirm never fires the state trigger.
	e.Observe(state.NewObservation(reg, ent(subjectEntity, "off"), ent(subjectEntity, "off")))
}

// registerSchedule wires a schedule trigger runtime into the engine (Task 1
// white-box plumbing: Tasks 2–3 will build these from the rule's own
// time/time_pattern/sun members via buildTrigger).
func registerSchedule(e *Engine, sched schedule.ScheduleTrigger, misfire string) {
	e.triggers = append(e.triggers, &triggerRuntime{kind: "time", sched: sched, misfire: misfire})
}

// TestSetLocationLoadsNamedZone: SetLocation resolves an IANA zone name and
// stores the coordinates (RUL-340/060); an unknown zone errors and leaves the
// prior location unchanged.
func TestSetLocationLoadsNamedZone(t *testing.T) {
	e := New(registry.FixtureRegistry{}, clock.NewFakeClock(), &recordingSink{}, nil)
	if err := e.SetLocation("America/New_York", 40.7128, -74.0060); err != nil {
		t.Fatalf("SetLocation(America/New_York): %v", err)
	}
	if e.loc.TZ == nil || e.loc.TZ.String() != "America/New_York" {
		t.Fatalf("timezone not loaded: %+v", e.loc.TZ)
	}
	if e.loc.Lat != 40.7128 || e.loc.Lon != -74.0060 {
		t.Fatalf("coordinates not stored: %+v", e.loc)
	}
	if err := e.SetLocation("Not/AZone", 1, 2); err == nil {
		t.Fatalf("SetLocation accepted an unknown zone")
	}
	if e.loc.TZ.String() != "America/New_York" {
		t.Fatalf("failed SetLocation clobbered the prior location: %+v", e.loc.TZ)
	}
}

// TestTickEnumeratesScheduleIntervalAndDispatches is the Task-1 plumbing oracle:
// on Tick the engine enumerates each schedule trigger's occurrences over the
// half-open wall interval (max(lastTickWall, floor), nowWall], passing the
// effective location, and routes each returned occurrence through fire() — so a
// single synthesized occurrence dispatches the rule's action exactly once with a
// `ran` disposition (RUL-041/340). The floor is the exclusive lower bound
// (RUL-370).
func TestTickEnumeratesScheduleIntervalAndDispatches(t *testing.T) {
	reg := registry.FixtureRegistry{}
	clk := clock.NewFakeClock()
	sink := &recordingSink{}

	e := New(reg, clk, sink, nil)
	if err := e.SetLocation("America/New_York", 40.7128, -74.0060); err != nil {
		t.Fatalf("SetLocation: %v", err)
	}
	scheduleRule(t, reg, e, "01J8Z3K4N5P6Q7R8S9T0V1SCHED")
	e.SeedScheduleFloor(1_000)

	spy := &spyEnumerator{emit: []int64{5_000}} // one occurrence in the window
	registerSchedule(e, spy, "skip")

	clk.SetWall(10_000)
	disps := e.Tick(clk)

	// Enumerated exactly once, over (floor, nowWall], with the loaded location.
	if len(spy.calls) != 1 {
		t.Fatalf("want 1 enumeration call, got %d (%+v)", len(spy.calls), spy.calls)
	}
	got := spy.calls[0]
	if got.from != 1_000 || got.to != 10_000 {
		t.Fatalf("enumeration interval = (%d, %d], want (1000, 10000]", got.from, got.to)
	}
	if got.tzName != "America/New_York" || got.lat != 40.7128 || got.lon != -74.0060 {
		t.Fatalf("enumeration location = %+v, want America/New_York 40.7128,-74.0060", got)
	}

	// The single occurrence routed through fire(): one ran disposition, one dispatch.
	if len(disps) != 1 || disps[0].Disposition != Ran {
		t.Fatalf("want one ran disposition, got %+v", disps)
	}
	if disps[0].RuleID != "01J8Z3K4N5P6Q7R8S9T0V1SCHED" {
		t.Fatalf("disposition rule_id = %q", disps[0].RuleID)
	}
	if disps[0].MisfireCaught {
		t.Fatalf("a live occurrence must not be marked misfire_caught")
	}
	if len(sink.calls) != 1 {
		t.Fatalf("want exactly one dispatch, got %d (%+v)", len(sink.calls), sink.calls)
	}
}

// TestTickAdvancesWallCursorAcrossTicks: lastTickWall advances to each Tick's
// wall reading, so the next Tick's enumeration lower bound is the previous
// Tick's wall instant — no interval is re-enumerated and none is skipped
// (RUL-340). The floor only bounds the first interval that predates it.
func TestTickAdvancesWallCursorAcrossTicks(t *testing.T) {
	reg := registry.FixtureRegistry{}
	clk := clock.NewFakeClock()
	sink := &recordingSink{}

	e := New(reg, clk, sink, nil)
	if err := e.SetLocation("UTC", 0, 0); err != nil {
		t.Fatalf("SetLocation: %v", err)
	}
	scheduleRule(t, reg, e, "RULECURSOR")
	e.SeedScheduleFloor(1_000)

	spy := &spyEnumerator{} // emit nothing; we only assert the intervals
	registerSchedule(e, spy, "skip")

	clk.SetWall(10_000)
	e.Tick(clk)
	clk.SetWall(20_000)
	e.Tick(clk)

	if len(spy.calls) != 2 {
		t.Fatalf("want 2 enumeration calls, got %d (%+v)", len(spy.calls), spy.calls)
	}
	if spy.calls[0].from != 1_000 || spy.calls[0].to != 10_000 {
		t.Fatalf("first interval = (%d, %d], want (1000, 10000]", spy.calls[0].from, spy.calls[0].to)
	}
	if spy.calls[1].from != 10_000 || spy.calls[1].to != 20_000 {
		t.Fatalf("second interval = (%d, %d], want (10000, 20000]", spy.calls[1].from, spy.calls[1].to)
	}
}

// TestTickDispatchesEachOccurrence: multiple occurrences in a single interval
// each route through fire() independently, in order — the plumbing Tasks 4/5
// layer misfire policy on top of (RUL-041/353). With no in-flight run between
// them (the action is synchronous), each records `ran` and dispatches once.
func TestTickDispatchesEachOccurrence(t *testing.T) {
	reg := registry.FixtureRegistry{}
	clk := clock.NewFakeClock()
	sink := &recordingSink{}

	e := New(reg, clk, sink, nil)
	if err := e.SetLocation("UTC", 0, 0); err != nil {
		t.Fatalf("SetLocation: %v", err)
	}
	scheduleRule(t, reg, e, "RULEEACH")
	e.SeedScheduleFloor(0)

	spy := &spyEnumerator{emit: []int64{5_000, 6_000, 7_000}}
	registerSchedule(e, spy, "skip")

	clk.SetWall(10_000)
	disps := e.Tick(clk)

	if len(disps) != 3 {
		t.Fatalf("want 3 dispositions (one per occurrence), got %d (%+v)", len(disps), disps)
	}
	for i, d := range disps {
		if d.Disposition != Ran {
			t.Fatalf("occurrence %d disposition = %q, want ran", i, d.Disposition)
		}
	}
	if len(sink.calls) != 3 {
		t.Fatalf("want 3 dispatches, got %d (%+v)", len(sink.calls), sink.calls)
	}
}

// TestTickNoScheduleTriggersReadsNoWall: a rule with no schedule trigger never
// reads the wall clock on Tick and never enumerates — the wall path is inert
// until a schedule trigger is registered.
func TestTickNoScheduleTriggersReadsNoWall(t *testing.T) {
	reg := registry.FixtureRegistry{}
	clk := clock.NewFakeClock()
	sink := &recordingSink{}

	e := New(reg, clk, sink, nil)
	scheduleRule(t, reg, e, "RULENOSCHED")

	clk.SetWall(999_999)
	if d := e.Tick(clk); len(d) != 0 {
		t.Fatalf("state-only rule fired on Tick: %+v", d)
	}
	if len(sink.calls) != 0 {
		t.Fatalf("state-only rule dispatched on Tick: %+v", sink.calls)
	}
}
