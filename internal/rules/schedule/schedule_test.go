package schedule

import (
	"testing"
	"time"
)

// emptyTrigger is the trivial always-empty ScheduleTrigger: it nominally never
// fires, so Occurrences returns no instants for any interval. It stands in for a
// real time/time_pattern/sun enumerator (Tasks 2–3) while exercising the shared
// ScheduleTrigger vocabulary this package establishes.
type emptyTrigger struct{}

func (emptyTrigger) Occurrences(loc Location, fromWallExclusive, toWallInclusive int64) []int64 {
	return nil
}

// interface assertion: emptyTrigger satisfies ScheduleTrigger.
var _ ScheduleTrigger = emptyTrigger{}

// TestLocationHoldsZoneAndCoords: a Location carries the effective timezone and
// the geographic coordinates a schedule trigger is evaluated against (RUL-340/060).
func TestLocationHoldsZoneAndCoords(t *testing.T) {
	tz, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatalf("LoadLocation: %v", err)
	}
	loc := Location{TZ: tz, Lat: 40.7128, Lon: -74.0060}
	if loc.TZ.String() != "America/New_York" {
		t.Fatalf("TZ = %q", loc.TZ.String())
	}
	if loc.Lat != 40.7128 || loc.Lon != -74.0060 {
		t.Fatalf("coords = %v,%v", loc.Lat, loc.Lon)
	}
}

// TestEmptyTriggerEnumeratesNothing: the always-empty stub returns no occurrence
// for any half-open interval — the baseline every real enumerator refines.
func TestEmptyTriggerEnumeratesNothing(t *testing.T) {
	var tr ScheduleTrigger = emptyTrigger{}
	if occ := tr.Occurrences(Location{}, 0, 1_000_000); len(occ) != 0 {
		t.Fatalf("empty trigger enumerated %v", occ)
	}
}

// TestOccurrenceCarriesWallInstant: an Occurrence is an absolute wall instant in
// the same Unix-ms units clock.WallMillis reports (RUL-340).
func TestOccurrenceCarriesWallInstant(t *testing.T) {
	o := Occurrence{Wall: 1_700_000_000_000}
	if o.Wall != 1_700_000_000_000 {
		t.Fatalf("Wall = %d", o.Wall)
	}
}
