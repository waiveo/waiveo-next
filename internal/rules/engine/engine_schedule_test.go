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

// windowedEnumerator is a schedule.ScheduleTrigger test double that, unlike
// spyEnumerator's fixed list, returns only those occurrence instants that fall in
// the half-open enumeration window (from, to] it is asked about — exactly as a
// real enumerator does. It is what makes the backward-wall-step regression test
// meaningful: a regressed cursor would re-enumerate an interval that re-covers an
// already-fired instant, and only a window-aware double surfaces the double-fire.
type windowedEnumerator struct {
	instants []int64
}

func (w *windowedEnumerator) Occurrences(_ schedule.Location, from, to int64) []int64 {
	var out []int64
	for _, t := range w.instants {
		if t > from && t <= to {
			out = append(out, t)
		}
	}
	return out
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
	ri := e.rules[len(e.rules)-1]
	ri.triggers = append(ri.triggers, &triggerRuntime{kind: "time", sched: sched, misfire: misfire})
}

// primeLiveTick marks the engine as already having ticked live up to wall instant
// `at` — the wall cursor a prior live Tick would have left. It makes the NEXT Tick
// a live tick (occurrences fire at their instant, RUL-041/340) rather than the
// first-Tick resume that catches a downtime span up through the misfire policy
// (RUL-354). Setting it to the enumeration floor keeps the live tick's half-open
// interval identical to the floor-bounded window under test.
func primeLiveTick(e *Engine, at int64) { e.lastTickWall = at }

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

// TestTickEnumeratesScheduleIntervalAndDispatches is the Task-1 plumbing oracle
// for a LIVE tick: with the engine already ticking (wall cursor primed), a Tick
// enumerates each schedule trigger's occurrences over the half-open wall interval
// (max(lastTickWall, floor), nowWall], passing the effective location, and routes
// each returned occurrence through fire() — so a single synthesized occurrence
// dispatches the rule's action exactly once with a `ran` disposition and
// misfire_caught=false (RUL-041/340). The floor is the exclusive lower bound
// (RUL-370). (A missed occurrence caught up on a first-Tick resume instead routes
// through the misfire policy — see engine_misfire_test.go.)
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
	primeLiveTick(e, 1_000) // engine already ticking live from the floor; this Tick is live

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

// TestBackwardWallStepDoesNotRefire: the wall cursor only advances forward. A
// trusted backward wall step (a routine NTP/admin clock correction on an
// RTC-less appliance) must not regress lastTickWall, or a later recovery Tick
// re-enumerates the intervening span and re-fires an already-fired occurrence
// (RUL-041/RUL-051 — a schedule occurrence fires once per its instant). The
// backward-step instant here (4_000) sits above the static floor (1_000), so the
// floor offers no protection; only the forward-only cursor guard does.
func TestBackwardWallStepDoesNotRefire(t *testing.T) {
	reg := registry.FixtureRegistry{}
	clk := clock.NewFakeClock()
	sink := &recordingSink{}

	e := New(reg, clk, sink, nil)
	if err := e.SetLocation("UTC", 0, 0); err != nil {
		t.Fatalf("SetLocation: %v", err)
	}
	scheduleRule(t, reg, e, "RULEBACKSTEP")
	e.SeedScheduleFloor(1_000)
	primeLiveTick(e, 1_000) // engine already ticking live from the floor

	// One occurrence at 5_000, enumerated only when it falls in the tick window.
	registerSchedule(e, &windowedEnumerator{instants: []int64{5_000}}, "skip")

	// Live tick to wall=10_000 fires occurrence 5_000 once, cursor -> 10_000.
	clk.SetWall(10_000)
	if disps := e.Tick(clk); len(disps) != 1 {
		t.Fatalf("first tick: want 1 dispatch, got %d (%+v)", len(disps), disps)
	}

	// Trusted backward wall step to 4_000 (above the floor). The cursor must NOT
	// regress: (4_000, 4_000] enumerates nothing and lastTickWall stays 10_000.
	clk.SetWall(4_000)
	if disps := e.Tick(clk); len(disps) != 0 {
		t.Fatalf("backward tick: want 0 dispatches, got %d (%+v)", len(disps), disps)
	}

	// Wall recovers to 10_000. With a forward-only cursor the window is
	// (10_000, 10_000] (empty); a regressed cursor would enumerate (4_000, 10_000]
	// and re-fire occurrence 5_000 a second time.
	clk.SetWall(10_000)
	if disps := e.Tick(clk); len(disps) != 0 {
		t.Fatalf("recovery tick: occurrence re-fired after backward wall step, got %d dispatch(es) (%+v)", len(disps), disps)
	}

	if len(sink.calls) != 1 {
		t.Fatalf("occurrence 5_000 must fire exactly once across the backward step; got %d dispatch(es)", len(sink.calls))
	}
}

// TestTickDispatchesEachOccurrence: on a LIVE tick, multiple occurrences in a
// single interval each route through fire() independently, in order (RUL-041/051).
// With no in-flight run between them (the action is synchronous), each records
// `ran` and dispatches once, misfire_caught=false. (The misfire policy governs
// only occurrences MISSED across downtime — caught up on a first-Tick resume, see
// engine_misfire_test.go — not the live-tick path exercised here.)
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
	primeLiveTick(e, 1_000) // engine already ticking live; this Tick is live, not a resume

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
