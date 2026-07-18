// Package schedule owns the wall-clock trigger vocabulary of the rules/1
// edge-rules engine: the tz/DST-correct enumeration of when a `time`,
// `time_pattern`, or `sun` trigger nominally fires (RUL-040/041/050/051/060/061),
// the astronomy behind `sun` (a pure-Go NOAA solar algorithm — no deps), and the
// `misfire` collapse/replay policy (RUL-350–355). Later tasks in this part supply
// the concrete enumerators and the misfire logic; this file establishes the shared
// vocabulary the engine's Tick path drives through.
//
// Unlike a `for`-hold or a `delay` — measured on MONOTONIC time so a wall-clock
// jump cannot lengthen or shorten a running duration (RUL-024/361) — a schedule
// trigger's occurrences ARE local wall-clock instants (RUL-340): each nominal
// local time is converted to an absolute instant through the owning scope's
// effective timezone, honouring the tz database's DST rules. The engine reads
// clock.WallMillis() only here, and never below its persisted trust floor
// (RUL-370).
package schedule

import "time"

// Location is the effective evaluation environment a schedule trigger's
// occurrences are computed against: the owning scope node's timezone (RUL-340,
// via time.LoadLocation) and the geographic latitude/longitude the `sun`
// astronomy uses (RUL-060/061). Lat/Lon are decimal degrees, north/east
// positive; they are ignored by `time`/`time_pattern` triggers.
type Location struct {
	TZ  *time.Location
	Lat float64
	Lon float64
}

// Occurrence is one nominal firing instant of a schedule trigger: an absolute
// wall-clock instant, as Unix milliseconds (the same units clock.WallMillis
// reports). It is the tz/DST-resolved absolute time a local schedule time maps
// to (RUL-340) — a DST-skipped local time yields no Occurrence at all (RUL-341),
// a fall-back-duplicated one yields a single Occurrence at its first absolute
// instant (RUL-342).
type Occurrence struct {
	Wall int64
}

// ScheduleTrigger is one wall-clock trigger's occurrence enumerator. Occurrences
// returns, in ascending order, the absolute wall instants (Unix ms) at which the
// trigger nominally fires within the half-open interval
// (fromWallExclusive, toWallInclusive] — occurrences strictly after the lower
// bound and at or before the upper bound. The lower bound is exclusive so an
// instant the engine has already evaluated (its last-evaluated wall cursor, or
// the persisted trust floor, RUL-370) is never re-enumerated. loc supplies the
// effective timezone and, for `sun`, the latitude/longitude (RUL-340/060).
//
// The concrete `time`/`time_pattern`/`sun` enumerators (Tasks 2–3) implement this;
// the engine's Tick path calls it once per elapsed wall interval and routes each
// returned instant through its existing conditions/mode dispatch (RUL-041/051).
type ScheduleTrigger interface {
	Occurrences(loc Location, fromWallExclusive, toWallInclusive int64) []int64
}
