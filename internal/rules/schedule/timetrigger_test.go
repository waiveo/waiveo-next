package schedule

import (
	"testing"
	"time"
)

// mustZone loads an IANA zone or fails the test.
func mustZone(t *testing.T, name string) *time.Location {
	t.Helper()
	tz, err := time.LoadLocation(name)
	if err != nil {
		t.Fatalf("LoadLocation(%q): %v", name, err)
	}
	return tz
}

// atMillis is the absolute Unix-ms instant of a local wall time in tz, using
// Go's time.Date resolution (the same the enumerator uses). It is the test's
// independent oracle for an expected occurrence.
func atMillis(tz *time.Location, y int, mo time.Month, d, h, mi, s int) int64 {
	return time.Date(y, mo, d, h, mi, s, 0, tz).UnixMilli()
}

// TestTimeTriggerDailyFire: a `time` trigger fires once per calendar date at its
// declared local time (RUL-040/041). Enumerated over a multi-day window, it
// yields exactly the in-window daily instants, ascending, half-open on the lower
// bound (RUL Occurrences contract).
func TestTimeTriggerDailyFire(t *testing.T) {
	tz := mustZone(t, "UTC")
	loc := Location{TZ: tz}
	tr, err := NewTimeTrigger("09:00:00")
	if err != nil {
		t.Fatalf("NewTimeTrigger: %v", err)
	}

	from := atMillis(tz, 2026, 6, 1, 8, 0, 0) // before the day-1 fire
	to := atMillis(tz, 2026, 6, 3, 10, 0, 0)  // after the day-3 fire
	want := []int64{
		atMillis(tz, 2026, 6, 1, 9, 0, 0),
		atMillis(tz, 2026, 6, 2, 9, 0, 0),
		atMillis(tz, 2026, 6, 3, 9, 0, 0),
	}
	got := tr.Occurrences(loc, from, to)
	assertInstants(t, got, want)

	// Lower bound is exclusive: an occurrence exactly at `from` is not re-enumerated.
	from2 := atMillis(tz, 2026, 6, 1, 9, 0, 0)
	got2 := tr.Occurrences(loc, from2, to)
	assertInstants(t, got2, want[1:])
}

// TestTimeTriggerSpringForwardSkip is the RUL-341 corpus oracle
// (RUL-341-dst-spring-forward-skip): a `time` trigger at 02:30:00 local in
// America/New_York does NOT fire on the spring-forward date 2026-03-08 (02:30
// does not exist), and fires normally on 2026-03-07 and 2026-03-09 (RUL-340/341).
func TestTimeTriggerSpringForwardSkip(t *testing.T) {
	tz := mustZone(t, "America/New_York")
	loc := Location{TZ: tz}
	tr, err := NewTimeTrigger("02:30:00")
	if err != nil {
		t.Fatalf("NewTimeTrigger: %v", err)
	}

	from := atMillis(tz, 2026, 3, 7, 0, 0, 0)
	to := atMillis(tz, 2026, 3, 10, 0, 0, 0)
	want := []int64{
		atMillis(tz, 2026, 3, 7, 2, 30, 0),
		// 2026-03-08 02:30 is removed by the spring-forward gap -> no fire (RUL-341).
		atMillis(tz, 2026, 3, 9, 2, 30, 0),
	}
	got := tr.Occurrences(loc, from, to)
	assertInstants(t, got, want)

	// The skipped occurrence never fires early/late/at-the-boundary: no instant
	// on 2026-03-08 between 02:00 and 04:00 local appears.
	for _, ms := range got {
		lt := time.UnixMilli(ms).In(tz)
		if lt.Year() == 2026 && lt.Month() == 3 && lt.Day() == 8 {
			t.Fatalf("spring-forward date produced an occurrence at %v", lt)
		}
	}
}

// TestTimeTriggerFallBackFiresOnce is the RUL-342 corpus oracle
// (RUL-342-dst-fall-back-fires-once): a `time` trigger at 01:30:00 local in
// America/New_York on the fall-back date 2026-11-01 (01:30 occurs twice) fires
// exactly once, keyed to the FIRST absolute occurrence (UTC-04:00), never at the
// second (UTC-05:00) (RUL-342).
func TestTimeTriggerFallBackFiresOnce(t *testing.T) {
	tz := mustZone(t, "America/New_York")
	loc := Location{TZ: tz}
	tr, err := NewTimeTrigger("01:30:00")
	if err != nil {
		t.Fatalf("NewTimeTrigger: %v", err)
	}

	from := atMillis(tz, 2026, 11, 1, 0, 0, 0)
	to := atMillis(tz, 2026, 11, 2, 0, 0, 0)
	got := tr.Occurrences(loc, from, to)
	if len(got) != 1 {
		t.Fatalf("fall-back date fired %d times, want exactly 1: %v", len(got), got)
	}
	lt := time.UnixMilli(got[0]).In(tz)
	_, off := lt.Zone()
	if off != -4*3600 {
		t.Fatalf("fired at offset %ds, want -14400 (UTC-04:00, the first occurrence)", off)
	}
	if lt.Hour() != 1 || lt.Minute() != 30 || lt.Second() != 0 {
		t.Fatalf("fired at local %02d:%02d:%02d, want 01:30:00", lt.Hour(), lt.Minute(), lt.Second())
	}
}

// TestTimePatternDivisorMinutes: a `time_pattern` with minutes `/15`, seconds `0`,
// hours omitted fires at every quarter-hour on the minute (RUL-050/051) — the
// omitted hours field matches every hour, the `/15` divisor every 15th minute,
// the exact `0` only second zero. Half-open lower bound excludes the on-boundary
// start instant.
func TestTimePatternDivisorMinutes(t *testing.T) {
	tz := mustZone(t, "UTC")
	loc := Location{TZ: tz}
	tr, err := NewTimePatternTrigger("", "/15", "0")
	if err != nil {
		t.Fatalf("NewTimePatternTrigger: %v", err)
	}

	from := atMillis(tz, 2026, 6, 1, 10, 0, 0) // exclusive: the 10:00:00 fire is excluded
	to := atMillis(tz, 2026, 6, 1, 11, 0, 0)   // inclusive: the 11:00:00 fire is included
	want := []int64{
		atMillis(tz, 2026, 6, 1, 10, 15, 0),
		atMillis(tz, 2026, 6, 1, 10, 30, 0),
		atMillis(tz, 2026, 6, 1, 10, 45, 0),
		atMillis(tz, 2026, 6, 1, 11, 0, 0),
	}
	got := tr.Occurrences(loc, from, to)
	assertInstants(t, got, want)
}

// TestTimePatternExactHourMinuteSecond: a fully-specified `time_pattern` (exact
// hours/minutes/seconds) behaves like a once-daily `time` trigger (RUL-050/051).
func TestTimePatternExactHourMinuteSecond(t *testing.T) {
	tz := mustZone(t, "UTC")
	loc := Location{TZ: tz}
	tr, err := NewTimePatternTrigger("6", "30", "15")
	if err != nil {
		t.Fatalf("NewTimePatternTrigger: %v", err)
	}
	from := atMillis(tz, 2026, 6, 1, 0, 0, 0)
	to := atMillis(tz, 2026, 6, 2, 0, 0, 0)
	want := []int64{atMillis(tz, 2026, 6, 1, 6, 30, 15)}
	assertInstants(t, tr.Occurrences(loc, from, to), want)
}

// TestTimePatternSpringForwardSkip: the RUL-341 spring-forward skip generalizes
// per nominal `time_pattern` occurrence (RUL-328 draft-note generalization).
// minutes `/30` across the 2026-03-08 America/New_York gap drops both 02:00 and
// 02:30 local (removed range) while firing 01:00/01:30 (EST) and 03:00/03:30 (EDT).
func TestTimePatternSpringForwardSkip(t *testing.T) {
	tz := mustZone(t, "America/New_York")
	loc := Location{TZ: tz}
	tr, err := NewTimePatternTrigger("", "/30", "0")
	if err != nil {
		t.Fatalf("NewTimePatternTrigger: %v", err)
	}
	from := atMillis(tz, 2026, 3, 8, 0, 59, 0)
	to := atMillis(tz, 2026, 3, 8, 3, 30, 0)
	want := []int64{
		atMillis(tz, 2026, 3, 8, 1, 0, 0),
		atMillis(tz, 2026, 3, 8, 1, 30, 0),
		// 02:00 and 02:30 removed by the gap -> no fire (RUL-341/328).
		atMillis(tz, 2026, 3, 8, 3, 0, 0),
		atMillis(tz, 2026, 3, 8, 3, 30, 0),
	}
	got := tr.Occurrences(loc, from, to)
	assertInstants(t, got, want)
	for _, ms := range got {
		lt := time.UnixMilli(ms).In(tz)
		if lt.Hour() == 2 {
			t.Fatalf("gap hour produced an occurrence at %v", lt)
		}
	}
}

// TestTimePatternFallBackFiresOnce: the RUL-342 fall-back single-fire generalizes
// per nominal `time_pattern` occurrence (RUL-328). minutes `/30` over the
// 2026-11-01 repeated 01:00-02:00 hour fires 01:00 and 01:30 exactly once each,
// each keyed to its first absolute (UTC-04:00) occurrence.
func TestTimePatternFallBackFiresOnce(t *testing.T) {
	tz := mustZone(t, "America/New_York")
	loc := Location{TZ: tz}
	tr, err := NewTimePatternTrigger("", "/30", "0")
	if err != nil {
		t.Fatalf("NewTimePatternTrigger: %v", err)
	}
	from := atMillis(tz, 2026, 11, 1, 0, 59, 0)
	to := atMillis(tz, 2026, 11, 1, 2, 0, 0)
	got := tr.Occurrences(loc, from, to)
	// 01:00, 01:30 (once each), 02:00 -> 3 occurrences; the repeated hour fires once.
	want := []int64{
		atMillis(tz, 2026, 11, 1, 1, 0, 0),
		atMillis(tz, 2026, 11, 1, 1, 30, 0),
		atMillis(tz, 2026, 11, 1, 2, 0, 0),
	}
	assertInstants(t, got, want)
	// Each repeated-hour occurrence is at the first (UTC-04:00) offset.
	for _, ms := range got[:2] {
		if _, off := time.UnixMilli(ms).In(tz).Zone(); off != -4*3600 {
			t.Fatalf("repeated-hour occurrence at offset %ds, want -14400", off)
		}
	}
}

// TestTimeTriggerEmptyWindow: a non-positive-width window enumerates nothing, and
// a nil timezone (location not set) is fail-closed (no fire), never a panic.
func TestTimeTriggerEmptyWindow(t *testing.T) {
	tz := mustZone(t, "UTC")
	tr, _ := NewTimeTrigger("09:00:00")
	if got := tr.Occurrences(Location{TZ: tz}, 5000, 5000); len(got) != 0 {
		t.Fatalf("zero-width window enumerated %v", got)
	}
	if got := tr.Occurrences(Location{TZ: nil}, 0, 1<<40); len(got) != 0 {
		t.Fatalf("nil-tz enumerated %v", got)
	}
}

// TestTriggerConstructorRejectsBadSpecs: invalid authored specs are rejected at
// construction (fail-closed), so the engine drops the trigger rather than firing
// on garbage (RUL-040/050).
func TestTriggerConstructorRejectsBadSpecs(t *testing.T) {
	if _, err := NewTimeTrigger("nope"); err == nil {
		t.Fatalf("NewTimeTrigger accepted a malformed local time")
	}
	if _, err := NewTimeTrigger("25:00:00"); err == nil {
		t.Fatalf("NewTimeTrigger accepted an out-of-range hour")
	}
	if _, err := NewTimePatternTrigger("", "", ""); err == nil {
		t.Fatalf("NewTimePatternTrigger accepted an all-empty spec (needs >=1 field)")
	}
	if _, err := NewTimePatternTrigger("", "/0", ""); err == nil {
		t.Fatalf("NewTimePatternTrigger accepted a /0 divisor")
	}
	if _, err := NewTimePatternTrigger("", "60", ""); err == nil {
		t.Fatalf("NewTimePatternTrigger accepted an out-of-range exact minute")
	}
}

// interface assertions: the concrete triggers satisfy ScheduleTrigger.
var (
	_ ScheduleTrigger = (*TimeTrigger)(nil)
	_ ScheduleTrigger = (*TimePatternTrigger)(nil)
)

// assertInstants compares two ascending instant slices for exact equality.
func assertInstants(t *testing.T, got, want []int64) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("occurrence count = %d, want %d\n got=%v\nwant=%v", len(got), len(want), fmtInstants(got), fmtInstants(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("occurrence[%d] = %d, want %d\n got=%v\nwant=%v", i, got[i], want[i], fmtInstants(got), fmtInstants(want))
		}
	}
}

func fmtInstants(xs []int64) []string {
	out := make([]string, len(xs))
	for i, x := range xs {
		out[i] = time.UnixMilli(x).UTC().Format("2006-01-02T15:04:05Z")
	}
	return out
}
