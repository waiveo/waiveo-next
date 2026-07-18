package schedule

import (
	"fmt"
	"math"
	"time"
)

// SunTrigger is the enumerator for a `sun` trigger (RUL-060/061): a solar event
// (sunrise or sunset) at the owning scope node's effective latitude/longitude,
// with an optional integer-seconds offset applied to the computed instant. Its
// occurrences are computed by a pure-Go solar-position algorithm (no external
// dependency, GLOBAL CONSTRAINTS). On a calendar date where the declared event
// does not occur — a polar day or polar night at that latitude — the trigger
// yields no occurrence for that date (RUL-061): the platform must not synthesize
// a firing instant the physical event never reached, the same fail-closed,
// non-substituted treatment a DST-skipped local time gets (RUL-341).
type SunTrigger struct {
	event     string // "sunrise" | "sunset"
	offsetSec int
}

// NewSunTrigger parses a `sun` trigger's event and optional offset (RUL-060). It
// fails closed on any event that is not exactly "sunrise" or "sunset".
func NewSunTrigger(event string, offsetSec int) (*SunTrigger, error) {
	if event != "sunrise" && event != "sunset" {
		return nil, fmt.Errorf("schedule: sun trigger: invalid event %q (want sunrise|sunset)", event)
	}
	return &SunTrigger{event: event, offsetSec: offsetSec}, nil
}

// Occurrences enumerates the trigger's absolute firing instants in the half-open
// interval (fromWallExclusive, toWallInclusive], ascending (RUL-060). Each
// calendar date overlapping the interval contributes at most one occurrence — its
// computed event instant shifted by the offset — and none at all when the event
// does not occur that date (polar day/night, RUL-061). A nil timezone or
// non-positive window enumerates nothing (fail-closed).
func (s *SunTrigger) Occurrences(loc Location, fromWallExclusive, toWallInclusive int64) []int64 {
	if loc.TZ == nil || toWallInclusive <= fromWallExclusive {
		return nil
	}
	var out []int64
	for _, day := range daysInWindow(loc.TZ, fromWallExclusive, toWallInclusive) {
		y, mo, d := day.Date()
		if ms, ok := SunInstant(loc, y, mo, d, s.event, s.offsetSec); ok {
			if ms > fromWallExclusive && ms <= toWallInclusive {
				out = append(out, ms)
			}
		}
	}
	return out
}

// SunInstant computes the absolute wall instant (Unix ms) of the solar event
// (`event` = "sunrise" or "sunset") on the calendar date year-month-day at
// loc.Lat/Lon, with offsetSec seconds added (RUL-060). It reports ok=false when
// the event does not occur on that date — a polar day or polar night, where the
// Sun never crosses the horizon (RUL-061) — and for an unrecognized event
// (fail-closed). It is the single solar computation shared by the `sun` trigger's
// occurrence enumeration and the `sun` condition's window evaluation (RUL-140),
// so trigger and condition never diverge.
func SunInstant(loc Location, year int, month time.Month, day int, event string, offsetSec int) (int64, bool) {
	var rise bool
	switch event {
	case "sunrise":
		rise = true
	case "sunset":
		rise = false
	default:
		return 0, false
	}
	ms, ok := sunEventUnixMillis(year, month, day, loc.Lat, loc.Lon, rise)
	if !ok {
		return 0, false
	}
	return ms + int64(offsetSec)*1000, true
}

const (
	degToRad = math.Pi / 180.0
	radToDeg = 180.0 / math.Pi
	// sunAltitudeDeg is the standard geometric altitude of the Sun's center at
	// sunrise/sunset: -0.833°, i.e. -50 arcminutes, accounting for the Sun's
	// mean angular radius (~16') plus mean atmospheric refraction at the horizon
	// (~34') — the same value the NOAA solar calculator uses.
	sunAltitudeDeg = -0.833
	// obliquityDeg is the mean obliquity of the ecliptic (axial tilt).
	obliquityDeg = 23.4397
)

// sunEventUnixMillis computes the UTC instant (Unix ms) of sunrise (rise=true) or
// sunset (rise=false) for the calendar date year-month-day at latitude lat and
// longitude lon (decimal degrees, north/east positive), reporting ok=false at a
// polar day/night where the event does not occur.
//
// The algorithm is the low-precision "sunrise equation" (see Wikipedia,
// "Sunrise equation", which in turn condenses the U.S. Naval Observatory /
// Almanac approximations): compute the mean solar time, the Sun's mean anomaly
// and equation of center to get the ecliptic longitude, the solar transit
// (Julian date of local solar noon), the Sun's declination, and the hour angle
// of the horizon crossing. It is accurate to about a minute for the horizon
// crossing over the coming decades — well within this contract's tolerance for a
// scheduled edge event. Longitude enters as J* = n - lon/360 with lon
// EAST-positive (the mirror of the reference's west-positive l_w); a sign error
// would displace the event by hours, so the convention is asserted by test.
func sunEventUnixMillis(year int, month time.Month, day int, lat, lon float64, rise bool) (int64, bool) {
	// Julian date at 00:00 UTC of the calendar date.
	midnightUTC := time.Date(year, month, day, 0, 0, 0, 0, time.UTC)
	jd := float64(midnightUTC.Unix())/86400.0 + 2440587.5

	// n: integer count of days since 2000-01-01 12:00 TT for this date; the
	// 0.0008 term is a small leap-second offset.
	n := math.Round(jd - 2451545.0 + 0.0008)

	// Mean solar time (an approximation of the Julian date of solar noon at lon).
	jStar := n - lon/360.0

	// Solar mean anomaly (degrees).
	m := math.Mod(357.5291+0.98560028*jStar, 360.0)
	mRad := m * degToRad

	// Equation of the center (degrees).
	c := 1.9148*math.Sin(mRad) + 0.0200*math.Sin(2*mRad) + 0.0003*math.Sin(3*mRad)

	// Ecliptic longitude of the Sun (degrees).
	lambda := math.Mod(m+c+180.0+102.9372, 360.0)
	lambdaRad := lambda * degToRad

	// Julian date of solar transit (local solar noon).
	jTransit := 2451545.0 + jStar + 0.0053*math.Sin(mRad) - 0.0069*math.Sin(2*lambdaRad)

	// Declination of the Sun.
	sinDelta := math.Sin(lambdaRad) * math.Sin(obliquityDeg*degToRad)
	cosDelta := math.Cos(math.Asin(sinDelta))

	// Hour angle of the horizon crossing.
	phi := lat * degToRad
	cosOmega := (math.Sin(sunAltitudeDeg*degToRad) - math.Sin(phi)*sinDelta) / (math.Cos(phi) * cosDelta)
	if cosOmega > 1.0 || cosOmega < -1.0 {
		// |cos| > 1: the Sun's center never reaches the horizon altitude on this
		// date at this latitude — polar day (event is a no-set) or polar night
		// (no-rise). No occurrence (RUL-061).
		return 0, false
	}
	omega := math.Acos(cosOmega) * radToDeg

	var jEvent float64
	if rise {
		jEvent = jTransit - omega/360.0
	} else {
		jEvent = jTransit + omega/360.0
	}

	unixSec := (jEvent - 2440587.5) * 86400.0
	return int64(math.Round(unixSec * 1000.0)), true
}
