package datamodel

import "time"

// This file implements DAT-110/119: the per-instant, DST-correct
// state-membership test for a daypart, and the ≤1 holding daypart of a schedule.
//
// A daypart is a STATE — continuous membership — not a one-shot event. Membership
// at an absolute instant `t` is decided solely by the UNAMBIGUOUS local wall-clock
// reading of `t` in the node's effective tz (DAT-033). An absolute instant maps to
// exactly one local reading whether or not a DST transition is in effect, so
// membership is always well-defined with no boundary special-case (DAT-119). Two
// consequences follow by construction, and this is where the daypart's STATE
// semantics diverge — deliberately — from rules/1's one-shot trigger DST rules
// (RUL-341/342), which MUST NOT be reused here:
//
//   - Spring-forward gap: a local reading removed by the gap is produced by no
//     absolute instant, so a window lying (wholly or partly) inside the gap holds
//     for a shorter span, or not at all. A window whose start is inside the gap
//     begins holding at the first real instant whose reading enters it.
//   - Fall-back repeat: the repeated local hour is produced by two distinct
//     absolute instants, each reading unambiguously into the window, so a window
//     spanning the repeat holds continuously through BOTH occurrences — no
//     fire-once collapse.
//
// The local reading is taken with time.UnixMilli(t).In(loc), the same tz/DST
// machinery internal/rules/schedule uses (Part 3, RUL-340): absolute→local is
// always single-valued, so — unlike the local→absolute direction that engine's
// localInstant resolves — there is no gap/repeat ambiguity to encode here. The
// declared coverage is expanded into half-open (weekday, [from, until)) segments
// exactly as DAT-073 defines (daypartSegments), including a wrapping window's tail
// carried onto the following weekday (DAT-072).

// Holds reports whether daypart dp holds at absolute instant tMs (Unix ms) in the
// node's effective timezone loc, per the DST-correct state-membership test
// (DAT-119). It takes the unambiguous local wall-clock reading of tMs in loc and
// tests it against dp's half-open (weekday, [from, until)) coverage (DAT-071–073).
// A nil loc, or a daypart whose time-of-day strings are unparseable, holds nowhere
// (fail-closed). This is the runtime, per-instant, tz/DST-correct membership test —
// distinct from DAT-073's authoring-time, DST-agnostic overlap comparison.
func Holds(dp Daypart, loc *time.Location, tMs int64) bool {
	if loc == nil {
		return false
	}
	segs, ok := daypartSegments(dp)
	if !ok {
		return false
	}
	// The unambiguous local reading of this absolute instant: exactly one
	// (weekday, second-of-day) whether or not a DST transition is in effect.
	local := time.UnixMilli(tMs).In(loc)
	weekday := int(local.Weekday()) // time.Sunday == 0, matching DAT-071's 0=Sunday
	sod := local.Hour()*3600 + local.Minute()*60 + local.Second()
	for _, s := range segs {
		if s.weekday == weekday && sod >= s.from && sod < s.until {
			return true
		}
	}
	return false
}

// HoldingDaypart returns the currently holding daypart of schedule at instant tMs
// (DAT-110): the one daypart among dayparts whose schedule_id is schedule.ID and
// whose coverage holds at tMs under Holds. Because a schedule's own dayparts never
// overlap (DAT-073), at most one holds — the single-holding-daypart-per-schedule
// property (DAT-077) that makes cross-schedule resolution well-defined. Dayparts
// of other schedules are ignored. Returns nil when none of this schedule's
// dayparts holds. The returned pointer addresses a copy, safe for the caller to
// retain.
func HoldingDaypart(schedule Schedule, dayparts []Daypart, loc *time.Location, tMs int64) *Daypart {
	for i := range dayparts {
		if dayparts[i].ScheduleID != schedule.ID {
			continue
		}
		if Holds(dayparts[i], loc, tMs) {
			held := dayparts[i]
			return &held
		}
	}
	return nil
}
