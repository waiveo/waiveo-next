package engine

import (
	"testing"
	"time"

	"github.com/maaxton/waiveo-next/internal/rules/clock"
	"github.com/maaxton/waiveo-next/internal/rules/registry"
	"github.com/maaxton/waiveo-next/internal/rules/schedule"
)

// TestBuildSunTriggerDispatchesOnTick: newTriggerRuntime wires a `sun` trigger
// (kind "sun") from the rule member, and Tick with the wall clock advanced past
// the computed sunset routes it through fire() exactly once (RUL-060/340). The
// expected instant is computed independently via schedule.SunInstant.
func TestBuildSunTriggerDispatchesOnTick(t *testing.T) {
	reg := registry.FixtureRegistry{}
	clk := clock.NewFakeClock()
	sink := &recordingSink{}

	e := New(reg, clk, sink, nil)
	if err := e.SetLocation("UTC", 51.4779, 0.0); err != nil { // Greenwich
		t.Fatalf("SetLocation: %v", err)
	}
	loadTimeRule(t, reg, e, "01J8Z3K4N5P6Q7R8S9T0SUNSET1", `{"type":"sun","event":"sunset"}`)

	loc := schedule.Location{TZ: time.UTC, Lat: 51.4779, Lon: 0.0}
	sunset, ok := schedule.SunInstant(loc, 2023, time.June, 21, "sunset", 0)
	if !ok {
		t.Fatalf("precondition: expected a sunset on the test date")
	}

	floor := time.Date(2023, time.June, 21, 0, 0, 0, 0, time.UTC).UnixMilli()
	e.SeedScheduleFloor(floor)
	primeLiveTick(e, floor)    // engine already ticking live; this Tick evaluates sunset live
	clk.SetWall(sunset + 1000) // one second after sunset

	disps := e.Tick(clk)
	if len(disps) != 1 || disps[0].Disposition != Ran {
		t.Fatalf("want one ran disposition, got %+v", disps)
	}
	if disps[0].MisfireCaught {
		t.Fatalf("a live sun occurrence must not be misfire_caught")
	}
	if len(sink.calls) != 1 || sink.calls[0].Command != "power" {
		t.Fatalf("want one power dispatch, got %+v", sink.calls)
	}

	// A later Tick the same day does not re-fire (the wall cursor passed sunset).
	clk.SetWall(time.Date(2023, time.June, 21, 23, 0, 0, 0, time.UTC).UnixMilli())
	if d := e.Tick(clk); len(d) != 0 {
		t.Fatalf("sun trigger re-fired within the same day: %+v", d)
	}
}

// TestBuildSunTriggerOffsetShiftsInstant: a negative offset fires the sun trigger
// earlier by exactly the offset (RUL-060). With a -1800s offset, a Tick landing
// between (sunset-1800s) and sunset dispatches; the same window without the
// offset would not.
func TestBuildSunTriggerOffsetShiftsInstant(t *testing.T) {
	reg := registry.FixtureRegistry{}
	clk := clock.NewFakeClock()
	sink := &recordingSink{}

	e := New(reg, clk, sink, nil)
	if err := e.SetLocation("UTC", 51.4779, 0.0); err != nil {
		t.Fatalf("SetLocation: %v", err)
	}
	loadTimeRule(t, reg, e, "01J8Z3K4N5P6Q7R8S9T0SUNOFF1", `{"type":"sun","event":"sunset","offset":-1800}`)

	loc := schedule.Location{TZ: time.UTC, Lat: 51.4779, Lon: 0.0}
	sunset, _ := schedule.SunInstant(loc, 2023, time.June, 21, "sunset", 0)

	floor := sunset - 3600*1000 // one hour before bare sunset
	e.SeedScheduleFloor(floor)
	primeLiveTick(e, floor)       // engine already ticking live; this Tick evaluates the offset instant live
	clk.SetWall(sunset - 60*1000) // one minute before bare sunset

	disps := e.Tick(clk)
	if len(disps) != 1 || disps[0].Disposition != Ran {
		t.Fatalf("offset trigger: want one ran, got %+v", disps)
	}
}

// TestBuildSunTriggerPolarNoFire is the RUL-061 oracle at the engine level: over
// a polar-night Tick at a high latitude the sun trigger dispatches nothing — the
// platform must not synthesize a firing instant the physical event never reached.
func TestBuildSunTriggerPolarNoFire(t *testing.T) {
	reg := registry.FixtureRegistry{}
	clk := clock.NewFakeClock()
	sink := &recordingSink{}

	e := New(reg, clk, sink, nil)
	if err := e.SetLocation("UTC", 78.2232, 15.6267); err != nil { // Longyearbyen
		t.Fatalf("SetLocation: %v", err)
	}
	loadTimeRule(t, reg, e, "01J8Z3K4N5P6Q7R8S9T0POLAR01", `{"type":"sun","event":"sunrise"}`)

	e.SeedScheduleFloor(time.Date(2023, time.December, 18, 0, 0, 0, 0, time.UTC).UnixMilli())
	clk.SetWall(time.Date(2023, time.December, 24, 0, 0, 0, 0, time.UTC).UnixMilli())

	if d := e.Tick(clk); len(d) != 0 {
		t.Fatalf("polar-night sun trigger fired: %+v", d)
	}
	if len(sink.calls) != 0 {
		t.Fatalf("polar-night sun trigger dispatched: %+v", sink.calls)
	}
}
