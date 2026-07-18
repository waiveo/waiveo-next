package eval

import (
	"testing"
	"time"

	"github.com/maaxton/waiveo-next/internal/rules/clock"
	"github.com/maaxton/waiveo-next/internal/rules/registry"
	"github.com/maaxton/waiveo-next/internal/rules/schedule"
	"github.com/maaxton/waiveo-next/internal/rules/state"
)

// greenwich is the effective evaluation environment used by the sun-condition
// tests: the Royal Observatory (lon 0) so the local wall clock equals UTC.
func greenwich() schedule.Location {
	return schedule.Location{TZ: time.UTC, Lat: 51.4779, Lon: 0.0}
}

// evalSun evaluates a sun condition JSON against loc at the given UTC wall
// instant, via EvalConditionAt.
func evalSun(t *testing.T, loc schedule.Location, wall int64, conditionJSON string) bool {
	t.Helper()
	reg := registry.FixtureRegistry{}
	snap := state.Snapshot{}
	clk := clock.NewFakeClock()
	clk.SetWall(wall)
	m := parseCondition(t, conditionJSON)
	return EvalConditionAt(reg, snap, clk, loc, nil, m)
}

// TestEvalSunConditionDaylightWindow is the RUL-140 oracle: an
// after-sunrise/before-sunset window passes when the current instant is between
// the two events and fails outside it, computed from the same solar algorithm as
// the sun trigger. On 2023-06-21 at Greenwich sunrise is ~03:43 UTC and sunset
// ~20:21 UTC.
func TestEvalSunConditionDaylightWindow(t *testing.T) {
	loc := greenwich()
	cond := `{"type":"sun","after":{"event":"sunrise"},"before":{"event":"sunset"}}`

	noon := time.Date(2023, time.June, 21, 12, 0, 0, 0, time.UTC).UnixMilli()
	if !evalSun(t, loc, noon, cond) {
		t.Error("midday: expected inside the sunrise..sunset window (pass)")
	}

	preDawn := time.Date(2023, time.June, 21, 2, 0, 0, 0, time.UTC).UnixMilli()
	if evalSun(t, loc, preDawn, cond) {
		t.Error("pre-dawn: expected before sunrise (fail)")
	}

	night := time.Date(2023, time.June, 21, 21, 0, 0, 0, time.UTC).UnixMilli()
	if evalSun(t, loc, night, cond) {
		t.Error("after sunset: expected outside the window (fail)")
	}
}

// TestEvalSunConditionOffsetApplied checks the {event, offset?} bound shape
// (RUL-140/060): a +offset on the `after` bound pushes the window's start later,
// so an instant just after bare sunset-of-window-start now falls before it.
func TestEvalSunConditionOffsetApplied(t *testing.T) {
	loc := greenwich()

	// Sunrise ~03:43 UTC. At 04:00 UTC a bare after-sunrise bound passes.
	at := time.Date(2023, time.June, 21, 4, 0, 0, 0, time.UTC).UnixMilli()
	if !evalSun(t, loc, at, `{"type":"sun","after":{"event":"sunrise"}}`) {
		t.Error("04:00 with bare after-sunrise: expected pass")
	}
	// A +3600s (one-hour) offset moves the bound to ~04:43 UTC, so 04:00 fails.
	if evalSun(t, loc, at, `{"type":"sun","after":{"event":"sunrise","offset":3600}}`) {
		t.Error("04:00 with after-sunrise+3600s: expected fail")
	}
}

// TestEvalSunConditionOneSidedBefore checks a single `before` bound (RUL-140):
// passes while the current instant is at or before the computed event.
func TestEvalSunConditionOneSidedBefore(t *testing.T) {
	loc := greenwich()
	cond := `{"type":"sun","before":{"event":"sunset"}}`

	morning := time.Date(2023, time.June, 21, 10, 0, 0, 0, time.UTC).UnixMilli()
	if !evalSun(t, loc, morning, cond) {
		t.Error("morning: expected before sunset (pass)")
	}
	lateNight := time.Date(2023, time.June, 21, 23, 0, 0, 0, time.UTC).UnixMilli()
	if evalSun(t, loc, lateNight, cond) {
		t.Error("late night: expected after sunset (fail)")
	}
}

// TestEvalSunConditionMissingBoundsFailsClosed: a sun condition declaring
// neither after nor before fails closed (RUL-140).
func TestEvalSunConditionMissingBoundsFailsClosed(t *testing.T) {
	loc := greenwich()
	at := time.Date(2023, time.June, 21, 12, 0, 0, 0, time.UTC).UnixMilli()
	if evalSun(t, loc, at, `{"type":"sun"}`) {
		t.Error("no after/before: expected fail-closed")
	}
}

// TestEvalSunConditionPolarNearestOccurrence exercises RUL-140's deliberate
// asymmetry with RUL-061: on a polar-night date the referenced event does not
// occur, but the condition MUST resolve against the nearest defined occurrence
// rather than treating the day as unbounded. The test computes that nearest
// occurrence independently and asserts the condition agrees.
func TestEvalSunConditionPolarNearestOccurrence(t *testing.T) {
	loc := schedule.Location{TZ: time.UTC, Lat: 78.2232, Lon: 15.6267} // Longyearbyen
	// Deep polar night; sunrise does not occur on this date.
	when := time.Date(2023, time.December, 21, 12, 0, 0, 0, time.UTC).UnixMilli()
	if _, ok := schedule.SunInstant(loc, 2023, time.December, 21, "sunrise", 0); ok {
		t.Fatalf("precondition: expected polar night (no sunrise) on the test date")
	}

	got := evalSun(t, loc, when, `{"type":"sun","after":{"event":"sunrise"}}`)

	// Independently find the nearest date with a defined sunrise and compare.
	nearest, ok := nearestSunrise(loc, 2023, time.December, 21)
	if !ok {
		t.Fatalf("precondition: expected a nearby defined sunrise")
	}
	want := when >= nearest
	if got != want {
		t.Errorf("polar nearest-occurrence: condition = %v, want %v (nearest sunrise %s)",
			got, want, time.UnixMilli(nearest).UTC())
	}
}

// nearestSunrise finds, by outward day search, the defined sunrise instant
// nearest to local noon of the given date — the test's independent oracle for
// RUL-140's nearest-occurrence fallback.
func nearestSunrise(loc schedule.Location, y int, mo time.Month, d int) (int64, bool) {
	base := time.Date(y, mo, d, 12, 0, 0, 0, loc.TZ)
	var best int64
	found := false
	for delta := 1; delta <= 200; delta++ {
		for _, sign := range []int{-1, 1} {
			cand := base.AddDate(0, 0, sign*delta)
			cy, cmo, cd := cand.Date()
			ms, ok := schedule.SunInstant(loc, cy, cmo, cd, "sunrise", 0)
			if !ok {
				continue
			}
			if !found || absCmp(ms, base.UnixMilli()) < absCmp(best, base.UnixMilli()) {
				best = ms
				found = true
			}
		}
		if found {
			return best, true
		}
	}
	return 0, false
}

func absCmp(a, b int64) int64 {
	d := a - b
	if d < 0 {
		return -d
	}
	return d
}
