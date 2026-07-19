package datamodel

import (
	"testing"
	"time"
)

// mustUTCms parses an RFC3339 UTC instant to Unix milliseconds for the test
// vectors, which quote each evaluation as an absolute `at_utc`.
func mustUTCms(t *testing.T, rfc3339 string) int64 {
	t.Helper()
	tm, err := time.Parse(time.RFC3339, rfc3339)
	if err != nil {
		t.Fatalf("parse %q: %v", rfc3339, err)
	}
	return tm.UnixMilli()
}

func mustLoadNY(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatalf("load America/New_York: %v", err)
	}
	return loc
}

// TestHolds_SpringForwardWindowBeginsFirstRealInstant wires the exact input and
// expected results of
// DAT-119-valid-dst-spring-forward-window-begins-first-real-instant.json.
//
// On 2026-03-08 in America/New_York, local 02:00:00-02:59:59 is removed
// (02:00 EST jumps to 03:00 EDT). Daypart A (02:30-04:00) starts inside the gap,
// so it begins holding at the first real instant whose local reading enters the
// window (03:00 EDT / 07:00Z), with no boundary special-case. Daypart B
// (02:10-02:50) lies wholly inside the gap, so no absolute instant maps to a
// reading inside it and it never holds that date (DAT-119).
func TestHolds_SpringForwardWindowBeginsFirstRealInstant(t *testing.T) {
	loc := mustLoadNY(t)

	dpA := Daypart{
		ID:           "01JDAYPARTAAA7RCG2V0CQV00",
		ScheduleID:   "01JSCHEDAAA0RGXJVZ9CJD000",
		ScopeNode:    "01JSCRNYCDE1EWH0AVNH9DN600",
		DaysOfWeek:   []int{0},
		StartTime:    "02:30:00",
		EndTime:      "04:00:00",
		DisplayPower: "on",
		PlaylistID:   "01JPAYSTAAA1MGDMFGS8KX00",
	}
	dpB := Daypart{
		ID:           "01JDAYPARTBBB7RCG2V0CQV00",
		ScheduleID:   "01JSCHEDBBB0RGXJVZ9CJD000",
		ScopeNode:    "01JSCRNYCDE1EWH0AVNH9DN600",
		DaysOfWeek:   []int{0},
		StartTime:    "02:10:00",
		EndTime:      "02:50:00",
		DisplayPower: "on",
		PlaylistID:   "01JPAYSTBBB1MGDMFGS8KX00",
	}

	cases := []struct {
		label  string
		atUTC  string
		aHolds bool
		bHolds bool
	}{
		{"before-gap", "2026-03-08T06:45:00Z", false, false},
		{"first-real-instant-after-gap", "2026-03-08T07:00:00Z", true, false},
		{"inside-window-A", "2026-03-08T07:30:00Z", true, false},
		{"past-window-A-end", "2026-03-08T08:15:00Z", false, false},
	}
	for _, c := range cases {
		tMs := mustUTCms(t, c.atUTC)
		if got := Holds(dpA, loc, tMs); got != c.aHolds {
			t.Errorf("%s: Holds(A) = %v, want %v", c.label, got, c.aHolds)
		}
		if got := Holds(dpB, loc, tMs); got != c.bHolds {
			t.Errorf("%s: Holds(B) = %v, want %v", c.label, got, c.bHolds)
		}
	}

	// window_wholly_in_gap_never_holds: B never holds anywhere on the date.
	// Sweep the whole removed gap in absolute time; no instant reads into it.
	for ms := mustUTCms(t, "2026-03-08T00:00:00Z"); ms <= mustUTCms(t, "2026-03-09T00:00:00Z"); ms += 60_000 {
		if Holds(dpB, loc, ms) {
			t.Fatalf("B held at %s but its window is wholly inside the removed gap", time.UnixMilli(ms).In(loc))
		}
	}
}

// TestHolds_FallBackWindowHoldsThroughRepeat wires the exact input and expected
// results of DAT-119-valid-dst-fall-back-window-holds-through-repeat.json.
//
// On 2026-11-01 in America/New_York, local 01:00:00-01:59:59 repeats (02:00 EDT
// falls back to 01:00 EST). A daypart is a STATE: window 00:30-02:30 holds
// continuously through BOTH occurrences of the repeated hour (DAT-119),
// contrasting rules/1 RUL-342's fire-once trigger.
func TestHolds_FallBackWindowHoldsThroughRepeat(t *testing.T) {
	loc := mustLoadNY(t)

	dp := Daypart{
		ID:           "01JDAYPARTNITE7RCG2V0CQV00",
		ScheduleID:   "01JSCHEDNITE0RGXJVZ9CJD000",
		ScopeNode:    "01JSCRNYCDE1EWH0AVNH9DN600",
		DaysOfWeek:   []int{0},
		StartTime:    "00:30:00",
		EndTime:      "02:30:00",
		DisplayPower: "on",
		PlaylistID:   "01JPAYSTNITE1MGDMFGS8KX00",
	}

	cases := []struct {
		label string
		atUTC string
		holds bool
	}{
		{"before-repeat", "2026-11-01T04:45:00Z", true},
		{"repeated-hour-first-EDT", "2026-11-01T05:30:00Z", true},
		{"repeated-hour-second-EST", "2026-11-01T06:30:00Z", true},
		{"after-repeat-still-in-window", "2026-11-01T07:15:00Z", true},
		{"past-window-end", "2026-11-01T07:45:00Z", false},
	}
	for _, c := range cases {
		tMs := mustUTCms(t, c.atUTC)
		if got := Holds(dp, loc, tMs); got != c.holds {
			t.Errorf("%s (%s): Holds = %v, want %v", c.label, c.atUTC, got, c.holds)
		}
	}

	// window_holds_continuously_through_repeat: no held-gap between the first
	// EDT occurrence and the second EST occurrence of 01:30 local.
	for ms := mustUTCms(t, "2026-11-01T05:30:00Z"); ms <= mustUTCms(t, "2026-11-01T06:30:00Z"); ms += 60_000 {
		if !Holds(dp, loc, ms) {
			t.Fatalf("window dropped at %s inside the repeated hour; a state holds continuously", time.UnixMilli(ms).In(loc))
		}
	}
}

// TestHoldingDaypart_AtMostOnePerSchedule verifies DAT-077/DAT-110: HoldingDaypart
// returns the single holding daypart of a schedule (partitioned dayparts never
// contest), scoped strictly to that schedule's own dayparts, and nil when none
// holds.
func TestHoldingDaypart_AtMostOnePerSchedule(t *testing.T) {
	loc := mustLoadNY(t)

	// One schedule with two non-overlapping (partitioning) Sunday dayparts.
	morning := Daypart{ID: "01JMORNING000000000000000", ScheduleID: "01JSCHED000000000000000000", ScopeNode: "01JSCR", DaysOfWeek: []int{0}, StartTime: "06:00:00", EndTime: "12:00:00", DisplayPower: "on"}
	evening := Daypart{ID: "01JEVENING00000000000000A", ScheduleID: "01JSCHED000000000000000000", ScopeNode: "01JSCR", DaysOfWeek: []int{0}, StartTime: "18:00:00", EndTime: "23:00:00", DisplayPower: "on"}
	// A daypart of a DIFFERENT schedule that also holds at the same instant.
	other := Daypart{ID: "01JOTHER0000000000000000A", ScheduleID: "01JSCHEDOTHER0000000000000", ScopeNode: "01JSCR", DaysOfWeek: []int{0}, StartTime: "00:00:00", EndTime: "23:59:59", DisplayPower: "on"}

	sched := Schedule{ID: "01JSCHED000000000000000000", ScopeNode: "01JSCR"}
	all := []Daypart{morning, evening, other}

	// 2026-11-08 is a Sunday. 09:00 local -> inside morning only.
	nineAM := time.Date(2026, 11, 8, 9, 0, 0, 0, loc).UnixMilli()
	got := HoldingDaypart(sched, all, loc, nineAM)
	if got == nil || got.ID != morning.ID {
		t.Fatalf("at 09:00 want holding daypart %s, got %v", morning.ID, got)
	}

	// 20:00 local -> inside evening only.
	eightPM := time.Date(2026, 11, 8, 20, 0, 0, 0, loc).UnixMilli()
	got = HoldingDaypart(sched, all, loc, eightPM)
	if got == nil || got.ID != evening.ID {
		t.Fatalf("at 20:00 want holding daypart %s, got %v", evening.ID, got)
	}

	// 14:00 local -> neither daypart of this schedule holds -> nil.
	twoPM := time.Date(2026, 11, 8, 14, 0, 0, 0, loc).UnixMilli()
	if got = HoldingDaypart(sched, all, loc, twoPM); got != nil {
		t.Fatalf("at 14:00 want no holding daypart for this schedule, got %v", got)
	}
}

// TestHolds_MidnightWrapTail verifies DAT-072/DAT-119: a window wrapping past
// local midnight holds on the following weekday via its carried tail segment.
func TestHolds_MidnightWrapTail(t *testing.T) {
	loc := mustLoadNY(t)
	// Saturday(6) 22:00 -> Sunday(0) 06:00 overnight window.
	dp := Daypart{ID: "01JWRAP", ScheduleID: "01JS", ScopeNode: "01JSCR", DaysOfWeek: []int{6}, StartTime: "22:00:00", EndTime: "06:00:00", DisplayPower: "on"}

	// 2026-11-07 is a Saturday; 23:00 local is in the pre-midnight head.
	satNight := time.Date(2026, 11, 7, 23, 0, 0, 0, loc).UnixMilli()
	if !Holds(dp, loc, satNight) {
		t.Errorf("Saturday 23:00 should hold (pre-midnight head)")
	}
	// 2026-11-08 Sunday 05:00 local is in the carried tail.
	sunEarly := time.Date(2026, 11, 8, 5, 0, 0, 0, loc).UnixMilli()
	if !Holds(dp, loc, sunEarly) {
		t.Errorf("Sunday 05:00 should hold (carried wrap tail)")
	}
	// Sunday 07:00 local is past the tail.
	sunLate := time.Date(2026, 11, 8, 7, 0, 0, 0, loc).UnixMilli()
	if Holds(dp, loc, sunLate) {
		t.Errorf("Sunday 07:00 should not hold (past wrap tail)")
	}
}

// TestHolds_NilLocationFailsClosed verifies fail-closed behavior with no tz.
func TestHolds_NilLocationFailsClosed(t *testing.T) {
	dp := Daypart{ID: "01J", ScheduleID: "01JS", DaysOfWeek: []int{0}, StartTime: "00:00:00", EndTime: "23:59:59", DisplayPower: "on"}
	if Holds(dp, nil, 0) {
		t.Errorf("Holds with nil location should be false (fail-closed)")
	}
}
