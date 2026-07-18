package schedule

import (
	"testing"
	"time"
)

// absDiff is the absolute difference of two Unix-ms instants, in seconds.
func absDiffSec(a, b int64) float64 {
	d := a - b
	if d < 0 {
		d = -d
	}
	return float64(d) / 1000.0
}

// TestSunInstantGreenwichSummer checks the pure-Go solar algorithm against an
// independent almanac vector (RUL-060): Royal Observatory Greenwich
// (51.4779 N, 0 E) on 2023-06-21 has sunrise ~03:43 UTC and sunset ~20:21 UTC.
// The algorithm is accurate to a few minutes, so a ±5-minute tolerance is used.
func TestSunInstantGreenwichSummer(t *testing.T) {
	loc := Location{TZ: time.UTC, Lat: 51.4779, Lon: 0.0}

	rise, ok := SunInstant(loc, 2023, time.June, 21, "sunrise", 0)
	if !ok {
		t.Fatalf("sunrise: expected an occurrence, got none")
	}
	wantRise := time.Date(2023, time.June, 21, 3, 43, 0, 0, time.UTC).UnixMilli()
	if d := absDiffSec(rise, wantRise); d > 300 {
		t.Errorf("sunrise = %s UTC, want ~03:43 UTC (off by %.0fs)",
			time.UnixMilli(rise).UTC().Format("15:04:05"), d)
	}

	set, ok := SunInstant(loc, 2023, time.June, 21, "sunset", 0)
	if !ok {
		t.Fatalf("sunset: expected an occurrence, got none")
	}
	wantSet := time.Date(2023, time.June, 21, 20, 21, 0, 0, time.UTC).UnixMilli()
	if d := absDiffSec(set, wantSet); d > 300 {
		t.Errorf("sunset = %s UTC, want ~20:21 UTC (off by %.0fs)",
			time.UnixMilli(set).UTC().Format("15:04:05"), d)
	}

	// Midpoint of sunrise/sunset is solar transit; at lon 0 it is ~12:00 UTC
	// within the equation-of-time bound (~±16 min).
	mid := (rise + set) / 2
	noon := time.Date(2023, time.June, 21, 12, 0, 0, 0, time.UTC).UnixMilli()
	if d := absDiffSec(mid, noon); d > 20*60 {
		t.Errorf("solar-noon midpoint = %s UTC, want ~12:00 UTC (off by %.0fs)",
			time.UnixMilli(mid).UTC().Format("15:04:05"), d)
	}
}

// TestSunInstantNYCLongitudeSign guards the east-positive longitude convention
// (a sign error puts the event ~10 h off): NYC (40.7128 N, -74.0060 E) sunrise
// on 2023-06-21 is ~09:25 UTC (05:25 EDT).
func TestSunInstantNYCLongitudeSign(t *testing.T) {
	loc := Location{TZ: time.UTC, Lat: 40.7128, Lon: -74.0060}
	rise, ok := SunInstant(loc, 2023, time.June, 21, "sunrise", 0)
	if !ok {
		t.Fatalf("sunrise: expected an occurrence, got none")
	}
	want := time.Date(2023, time.June, 21, 9, 25, 0, 0, time.UTC).UnixMilli()
	if d := absDiffSec(rise, want); d > 300 {
		t.Errorf("NYC sunrise = %s UTC, want ~09:25 UTC (off by %.0fs)",
			time.UnixMilli(rise).UTC().Format("15:04:05"), d)
	}
}

// TestSunInstantOffsetShiftsInstant checks RUL-060's offset: an integer-seconds
// offset shifts the computed instant by exactly that many seconds (negative =
// earlier).
func TestSunInstantOffsetShiftsInstant(t *testing.T) {
	loc := Location{TZ: time.UTC, Lat: 51.4779, Lon: 0.0}
	base, ok := SunInstant(loc, 2023, time.June, 21, "sunrise", 0)
	if !ok {
		t.Fatalf("base sunrise: got none")
	}
	shifted, ok := SunInstant(loc, 2023, time.June, 21, "sunrise", -1800)
	if !ok {
		t.Fatalf("offset sunrise: got none")
	}
	if got := base - shifted; got != 1800*1000 {
		t.Errorf("offset -1800s shifted instant by %d ms, want %d ms earlier", got, 1800*1000)
	}
}

// TestSunInstantPolarNoOccurrence is the RUL-061 oracle: at a high latitude on a
// polar-night date the event does not occur, and at a polar-day date it does not
// occur either — SunInstant reports ok=false in both cases, and the trigger must
// therefore never synthesize a firing instant.
func TestSunInstantPolarNoOccurrence(t *testing.T) {
	loc := Location{TZ: time.UTC, Lat: 78.2232, Lon: 15.6267} // Longyearbyen

	if _, ok := SunInstant(loc, 2023, time.December, 21, "sunrise", 0); ok {
		t.Error("polar-night sunrise: expected no occurrence, got one")
	}
	if _, ok := SunInstant(loc, 2023, time.December, 21, "sunset", 0); ok {
		t.Error("polar-night sunset: expected no occurrence, got one")
	}
	if _, ok := SunInstant(loc, 2023, time.June, 21, "sunrise", 0); ok {
		t.Error("polar-day sunrise: expected no occurrence, got one")
	}
}

// TestSunInstantInvalidEvent: an event that is neither sunrise nor sunset yields
// no occurrence (fail-closed, RUL-060).
func TestSunInstantInvalidEvent(t *testing.T) {
	loc := Location{TZ: time.UTC, Lat: 51.4779, Lon: 0.0}
	if _, ok := SunInstant(loc, 2023, time.June, 21, "noon", 0); ok {
		t.Error("invalid event: expected no occurrence, got one")
	}
}

// TestNewSunTriggerValidation: NewSunTrigger accepts sunrise/sunset and rejects
// anything else (RUL-060, fail-closed).
func TestNewSunTriggerValidation(t *testing.T) {
	if _, err := NewSunTrigger("sunrise", 0); err != nil {
		t.Errorf("NewSunTrigger(sunrise): %v", err)
	}
	if _, err := NewSunTrigger("sunset", -900); err != nil {
		t.Errorf("NewSunTrigger(sunset): %v", err)
	}
	if _, err := NewSunTrigger("dawn", 0); err == nil {
		t.Error("NewSunTrigger(dawn): expected an error")
	}
}

// TestSunTriggerOccurrencesWindow: a sunrise trigger enumerated across a
// multi-day window yields exactly one ascending occurrence per date in the
// window, half-open on the lower bound (RUL-060, Occurrences contract).
func TestSunTriggerOccurrencesWindow(t *testing.T) {
	loc := Location{TZ: time.UTC, Lat: 51.4779, Lon: 0.0}
	tr, err := NewSunTrigger("sunrise", 0)
	if err != nil {
		t.Fatalf("NewSunTrigger: %v", err)
	}

	// Independent oracle instants for each date's sunrise.
	d1, _ := SunInstant(loc, 2023, time.June, 20, "sunrise", 0)
	d2, _ := SunInstant(loc, 2023, time.June, 21, "sunrise", 0)
	d3, _ := SunInstant(loc, 2023, time.June, 22, "sunrise", 0)

	from := time.Date(2023, time.June, 20, 0, 0, 0, 0, time.UTC).UnixMilli()
	to := time.Date(2023, time.June, 22, 12, 0, 0, 0, time.UTC).UnixMilli()
	got := tr.Occurrences(loc, from, to)
	want := []int64{d1, d2, d3}
	assertInstants(t, got, want)

	// Lower bound is exclusive: raising `from` to exactly d1 drops it.
	got2 := tr.Occurrences(loc, d1, to)
	assertInstants(t, got2, want[1:])
}

// TestSunTriggerOccurrencesPolarSkip: over a window covering a polar-night span
// the sunrise trigger enumerates nothing (RUL-061) — no synthesized instant.
func TestSunTriggerOccurrencesPolarSkip(t *testing.T) {
	loc := Location{TZ: time.UTC, Lat: 78.2232, Lon: 15.6267}
	tr, err := NewSunTrigger("sunrise", 0)
	if err != nil {
		t.Fatalf("NewSunTrigger: %v", err)
	}
	from := time.Date(2023, time.December, 18, 0, 0, 0, 0, time.UTC).UnixMilli()
	to := time.Date(2023, time.December, 24, 0, 0, 0, 0, time.UTC).UnixMilli()
	if got := tr.Occurrences(loc, from, to); len(got) != 0 {
		t.Errorf("polar-night window enumerated %d occurrences, want 0: %v", len(got), got)
	}
}
