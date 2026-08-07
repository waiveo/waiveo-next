package datamodel

import (
	"fmt"
	"strings"
)

// secondsPerDay is the exclusive upper bound (24:00:00) of a local time-of-day in
// seconds — the `until` a non-wrapping segment never reaches and a wrapping
// segment's pre-midnight head runs up to (DAT-072/073).
const secondsPerDay = 24 * 60 * 60

// segment is one half-open piece of a daypart's declared local coverage: a
// nominal weekday (0=Sunday, DAT-071) and a `[from, until)` local time-of-day
// range in seconds, `until` exclusive (DAT-073). It is authoring-time, DST-
// agnostic declared coverage — never an absolute dated instant (the per-instant,
// DST-correct membership test is DAT-119, a later file).
type segment struct {
	weekday int
	from    int
	until   int
}

// parseTimeOfDay parses a `HH:MM:SS` local time-of-day (DAT-071, the lexical
// format rules/1 RUL-040 fixes) into seconds since local midnight in [0,86400).
//
// The lexical check is strict: exactly two ASCII digits per component. DAT-071
// names the FORMAT `HH:MM:SS`, so "9:00:00" is not in it — and the two-digit
// digits-only rule is also what keeps strconv's sign tolerance out ("+9" parses
// as 9, the same trap MAN-002's version grammar pins against). The components
// are then computed from the verified digits directly, so this function has
// exactly the two failure modes the grammar has: not the format, or a component
// out of range ("24:00:00" is out of range — a day's exclusive upper bound,
// never a time within it; midnight is "00:00:00").
func parseTimeOfDay(s string) (int, error) {
	parts := strings.Split(s, ":")
	if len(parts) != 3 {
		return 0, fmt.Errorf("time-of-day %q is not HH:MM:SS", s)
	}
	var c [3]int
	for i, p := range parts {
		if len(p) != 2 || p[0] < '0' || p[0] > '9' || p[1] < '0' || p[1] > '9' {
			return 0, fmt.Errorf("time-of-day %q is not HH:MM:SS", s)
		}
		c[i] = int(p[0]-'0')*10 + int(p[1]-'0')
	}
	h, m, sec := c[0], c[1], c[2]
	if h > 23 || m > 59 || sec > 59 {
		return 0, fmt.Errorf("time-of-day %q out of range", s)
	}
	return h*3600 + m*60 + sec, nil
}

// daypartSegments expands a daypart into its half-open `(weekday, [from, until))`
// segment set exactly as DAT-073 defines. A non-wrapping daypart (end_time >
// start_time) yields one `[start,end)` segment per day; a wrapping daypart
// (end_time <= start_time, DAT-072) yields a pre-midnight `[start,24:00)` head on
// each day plus a `[00:00,end)` tail carried onto `(d+1) mod 7`. Zero-length tail
// segments (a wrap whose end_time is 00:00:00) are dropped. ok is false only when
// a time-of-day string is unparseable.
func daypartSegments(d Daypart) (segs []segment, ok bool) {
	start, serr := parseTimeOfDay(d.StartTime)
	end, eerr := parseTimeOfDay(d.EndTime)
	if serr != nil || eerr != nil {
		return nil, false
	}
	for _, day := range d.DaysOfWeek {
		// A weekday outside 0–6 contributes NO coverage (DAT-071, fail-closed
		// like an unparseable time). Without this, the wrap tail below lands on
		// (day+1) mod 7 — a REAL weekday — so a phantom day would fail open:
		// coverage the author never declared, on a day they never named. The
		// write gate (checkDaysOfWeek) makes such a row unstorable; this is the
		// runtime's own refusal to expand one however it arrives.
		if day < 0 || day > 6 {
			continue
		}
		if end > start {
			// Non-wrapping: a single same-day half-open range.
			segs = append(segs, segment{weekday: day, from: start, until: end})
			continue
		}
		// Wrapping past local midnight (DAT-072): pre-midnight head + next-day tail.
		if start < secondsPerDay {
			segs = append(segs, segment{weekday: day, from: start, until: secondsPerDay})
		}
		if end > 0 {
			segs = append(segs, segment{weekday: (day + 1) % 7, from: 0, until: end})
		}
	}
	return segs, true
}

// segmentsIntersect reports whether two half-open coverage segments share a
// weekday AND have intersecting time-of-day ranges — the DAT-073 overlap
// predicate. Half-open boundaries abut without intersecting: `[06:00,22:00)` and
// `[22:00,24:00)` do not intersect.
func segmentsIntersect(a, b segment) bool {
	return a.weekday == b.weekday && a.from < b.until && b.from < a.until
}

// ValidateNoOverlap enforces DAT-073: within any one schedule_id, no two daypart
// rows may overlap. Each daypart is expanded into its half-open `(weekday,
// [from, until))` segments (daypartSegments); two dayparts of the same schedule
// overlap iff some segment of the one and some segment of the other share a
// weekday and have intersecting half-open time-of-day ranges — a wrapping
// daypart's next-weekday tail can collide with a sibling even when the two
// dayparts' days_of_week arrays are disjoint (DAT-072/073). A schedule's dayparts
// thus PARTITION time, they do not layer (the single-holding-daypart-per-schedule
// precondition, DAT-077). The comparison is over DECLARED local coverage only,
// never an absolute dated instant, so it is independent of tz offset and of any
// daylight-saving transition — authoring-time and DST-agnostic, distinct from the
// per-instant runtime membership test (DAT-119).
//
// It returns the first DAYPART_OVERLAP Error found (Error taxonomy), or nil when
// every schedule's dayparts partition cleanly. Dayparts of different schedules
// are never compared (cross-schedule layering is well-defined, DAT-111).
func ValidateNoOverlap(dayparts []Daypart) *Error {
	// Group by schedule_id, preserving input order for deterministic reporting.
	bySchedule := map[string][]Daypart{}
	var order []string
	for _, d := range dayparts {
		if _, seen := bySchedule[d.ScheduleID]; !seen {
			order = append(order, d.ScheduleID)
		}
		bySchedule[d.ScheduleID] = append(bySchedule[d.ScheduleID], d)
	}

	for _, sid := range order {
		group := bySchedule[sid]
		// Pre-expand each daypart's segments once.
		segsOf := make([][]segment, len(group))
		for i, d := range group {
			segs, ok := daypartSegments(d)
			if !ok {
				// Unparseable time-of-day is a DAT-071 shape concern surfaced
				// elsewhere; it cannot be expanded, so skip it here.
				continue
			}
			segsOf[i] = segs
		}
		for i := 0; i < len(group); i++ {
			for j := i + 1; j < len(group); j++ {
				for _, sa := range segsOf[i] {
					for _, sb := range segsOf[j] {
						if segmentsIntersect(sa, sb) {
							return &Error{
								Field: "dayparts",
								Code:  "DAYPART_OVERLAP",
								Message: fmt.Sprintf(
									"two dayparts of schedule %s (%s and %s) share weekday %d and intersect on the half-open local time-of-day range [%s,%s); a schedule's dayparts MUST partition time, not layer (DAT-073)",
									sid, group[i].ID, group[j].ID, sa.weekday,
									secToClock(maxInt(sa.from, sb.from)), secToClock(minInt(sa.until, sb.until)),
								),
							}
						}
					}
				}
			}
		}
	}
	return nil
}

// secToClock renders a seconds-of-day value (0..86400) back to HH:MM:SS for
// human-readable overlap messages.
func secToClock(sec int) string {
	h := sec / 3600
	m := (sec % 3600) / 60
	s := sec % 60
	return fmt.Sprintf("%02d:%02d:%02d", h, m, s)
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
