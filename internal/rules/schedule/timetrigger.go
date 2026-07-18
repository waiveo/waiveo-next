package schedule

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// TimeTrigger is the enumerator for a `time` trigger (RUL-040/041): a single
// declared local time-of-day (HH:MM:SS, 24-hour) that fires once at each of its
// local occurrences, resolved to an absolute instant through the effective
// timezone (RUL-340). A spring-forward-removed local time yields no occurrence
// for that date (RUL-341); a fall-back-repeated one yields a single occurrence at
// its first absolute instant (RUL-342).
type TimeTrigger struct {
	hour, min, sec int
}

// NewTimeTrigger parses a `time` trigger's `at` local time-of-day (RUL-040). It
// fails closed on any string that is not a valid 24-hour HH:MM:SS.
func NewTimeTrigger(at string) (*TimeTrigger, error) {
	h, m, s, ok := parseHMS(at)
	if !ok {
		return nil, fmt.Errorf("schedule: time trigger: invalid at %q (want HH:MM:SS)", at)
	}
	return &TimeTrigger{hour: h, min: m, sec: s}, nil
}

// Occurrences enumerates the trigger's absolute firing instants in the half-open
// interval (fromWallExclusive, toWallInclusive], ascending (RUL-041). Each
// calendar date overlapping the interval contributes at most one occurrence — its
// local HH:MM:SS resolved through loc.TZ — dropped when the local time does not
// exist on that date (RUL-341) and taken at its first absolute instant when the
// local time repeats (RUL-342). A nil timezone or non-positive window enumerates
// nothing (fail-closed).
func (t *TimeTrigger) Occurrences(loc Location, fromWallExclusive, toWallInclusive int64) []int64 {
	if loc.TZ == nil || toWallInclusive <= fromWallExclusive {
		return nil
	}
	var out []int64
	for _, day := range daysInWindow(loc.TZ, fromWallExclusive, toWallInclusive) {
		y, mo, d := day.Date()
		if ms, ok := localInstant(loc.TZ, y, mo, d, t.hour, t.min, t.sec); ok {
			if ms > fromWallExclusive && ms <= toWallInclusive {
				out = append(out, ms)
			}
		}
	}
	return out
}

// TimePatternTrigger is the enumerator for a `time_pattern` trigger
// (RUL-050/051): independent hours/minutes/seconds component matchers (an exact
// value, a `/N` divisor, or omitted = every value), firing at every local instant
// satisfying all declared fields simultaneously. Each nominal local occurrence is
// resolved through the effective timezone with the same DST semantics as a `time`
// trigger, applied per occurrence (RUL-341/342 generalized, RUL-328).
type TimePatternTrigger struct {
	hours   patternField
	minutes patternField
	seconds patternField
}

// NewTimePatternTrigger parses a `time_pattern` trigger's three component fields
// (RUL-050). Each field is "" (omitted = match every value), a non-negative
// decimal integer (exact match), or "/N" (N a positive integer, divisor match).
// At least one field MUST be present. It fails closed on a malformed field or an
// all-empty spec.
func NewTimePatternTrigger(hours, minutes, seconds string) (*TimePatternTrigger, error) {
	if hours == "" && minutes == "" && seconds == "" {
		return nil, fmt.Errorf("schedule: time_pattern trigger: at least one of hours/minutes/seconds is required")
	}
	hf, err := parsePatternField(hours, 23)
	if err != nil {
		return nil, fmt.Errorf("schedule: time_pattern hours: %w", err)
	}
	mf, err := parsePatternField(minutes, 59)
	if err != nil {
		return nil, fmt.Errorf("schedule: time_pattern minutes: %w", err)
	}
	sf, err := parsePatternField(seconds, 59)
	if err != nil {
		return nil, fmt.Errorf("schedule: time_pattern seconds: %w", err)
	}
	return &TimePatternTrigger{hours: hf, minutes: mf, seconds: sf}, nil
}

// Occurrences enumerates every absolute firing instant in the half-open interval
// (fromWallExclusive, toWallInclusive], ascending (RUL-051). For each calendar
// date overlapping the interval it walks the matching hour/minute/second
// components, resolving each nominal local instant through loc.TZ with the DST
// semantics of RUL-341/342 applied individually (RUL-328): a component instant
// removed by a spring-forward gap is dropped, and a repeated one is taken at its
// first absolute occurrence (constructed once per local time, so it fires once).
// The result is ascending because both the calendar-date walk and the intra-day
// component walk are ascending in local time, which is monotonic in absolute time
// once each repeated local time is collapsed to its first instant.
func (p *TimePatternTrigger) Occurrences(loc Location, fromWallExclusive, toWallInclusive int64) []int64 {
	if loc.TZ == nil || toWallInclusive <= fromWallExclusive {
		return nil
	}
	var out []int64
	for _, day := range daysInWindow(loc.TZ, fromWallExclusive, toWallInclusive) {
		y, mo, d := day.Date()
		for h := 0; h < 24; h++ {
			if !p.hours.match(h) {
				continue
			}
			for m := 0; m < 60; m++ {
				if !p.minutes.match(m) {
					continue
				}
				for s := 0; s < 60; s++ {
					if !p.seconds.match(s) {
						continue
					}
					if ms, ok := localInstant(loc.TZ, y, mo, d, h, m, s); ok {
						if ms > fromWallExclusive && ms <= toWallInclusive {
							out = append(out, ms)
						}
					}
				}
			}
		}
	}
	return out
}

// patternField is one time_pattern component matcher (RUL-050): all (omitted),
// an exact value, or a `/N` divisor.
type patternField struct {
	all   bool
	exact int
	div   int // divisor when > 0
}

// match reports whether the clock component v satisfies this field (RUL-050): an
// omitted field matches every value; a `/N` divisor matches multiples of N
// (including 0); an exact field matches only its value.
func (f patternField) match(v int) bool {
	switch {
	case f.all:
		return true
	case f.div > 0:
		return v%f.div == 0
	default:
		return v == f.exact
	}
}

// parsePatternField parses one time_pattern component (RUL-050) against its
// inclusive upper bound max (23 for hours, 59 for minutes/seconds).
func parsePatternField(s string, max int) (patternField, error) {
	if s == "" {
		return patternField{all: true}, nil
	}
	if rest, ok := strings.CutPrefix(s, "/"); ok {
		n, err := strconv.Atoi(rest)
		if err != nil || n <= 0 {
			return patternField{}, fmt.Errorf("invalid divisor %q (want /N, N a positive integer)", s)
		}
		return patternField{div: n}, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 || n > max {
		return patternField{}, fmt.Errorf("invalid exact value %q (want 0..%d)", s, max)
	}
	return patternField{exact: n}, nil
}

// parseHMS parses a 24-hour HH:MM:SS local time-of-day into its components,
// reporting ok=false for any string not of that exact shape or out of range
// (RUL-040, fail-closed).
func parseHMS(s string) (h, m, sec int, ok bool) {
	parts := strings.Split(s, ":")
	if len(parts) != 3 {
		return 0, 0, 0, false
	}
	h, e1 := strconv.Atoi(parts[0])
	m, e2 := strconv.Atoi(parts[1])
	sec, e3 := strconv.Atoi(parts[2])
	if e1 != nil || e2 != nil || e3 != nil {
		return 0, 0, 0, false
	}
	if len(parts[0]) != 2 || len(parts[1]) != 2 || len(parts[2]) != 2 {
		return 0, 0, 0, false
	}
	if h < 0 || h > 23 || m < 0 || m > 59 || sec < 0 || sec > 59 {
		return 0, 0, 0, false
	}
	return h, m, sec, true
}

// daysInWindow returns the local midnight time.Time of every calendar date in tz
// that overlaps the wall interval [fromExcl, toIncl] (both bounds' dates
// inclusive, so an occurrence later in the start date or on the end date is
// reachable). Iterating by calendar day (AddDate) keeps each step on a real local
// midnight across DST transitions.
func daysInWindow(tz *time.Location, fromExcl, toIncl int64) []time.Time {
	start := time.UnixMilli(fromExcl).In(tz)
	end := time.UnixMilli(toIncl).In(tz)
	sy, sm, sd := start.Date()
	day := time.Date(sy, sm, sd, 0, 0, 0, 0, tz)
	var days []time.Time
	for !day.After(end) {
		days = append(days, day)
		day = day.AddDate(0, 0, 1)
	}
	return days
}

// localInstant resolves the local wall time y-mo-d h:m:s in tz to an absolute
// Unix-ms instant, encoding the DST semantics of RUL-340–342. It reports ok=false
// when the local time does not exist on that date — a spring-forward gap, which
// Go's time.Date normalizes to a different clock reading; the mismatch of the
// constructed clock fields against the requested ones detects it (RUL-341). For a
// fall-back-repeated local time, time.Date resolves to the first (larger-offset)
// absolute occurrence, which is exactly RUL-342's "first absolute occurrence".
func localInstant(tz *time.Location, y int, mo time.Month, d, h, m, s int) (int64, bool) {
	cand := time.Date(y, mo, d, h, m, s, 0, tz)
	if cand.Hour() != h || cand.Minute() != m || cand.Second() != s {
		return 0, false // the requested local time does not exist (spring-forward gap)
	}
	return cand.UnixMilli(), true
}
