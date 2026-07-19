package datamodel

import (
	"encoding/json"
	"os"
	"testing"
)

// loadCorpusDayparts reads a data-model-1 corpus case's input.dayparts array as
// typed Daypart rows, asserting the case's expectation shape has not drifted.
func loadCorpusDayparts(t *testing.T, name string) (dayparts []Daypart, wantCode, wantField string) {
	t.Helper()
	b, err := os.ReadFile(corpusPath(name))
	if err != nil {
		t.Fatalf("read corpus: %v", err)
	}
	var c struct {
		Input struct {
			Dayparts []Daypart `json:"dayparts"`
		} `json:"input"`
		Expected struct {
			Valid  bool `json:"valid"`
			Errors []struct {
				Field string `json:"field"`
				Code  string `json:"code"`
			} `json:"errors"`
		} `json:"expected"`
	}
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatalf("parse corpus: %v", err)
	}
	if c.Expected.Valid || len(c.Expected.Errors) == 0 {
		t.Fatalf("corpus expectation drifted (expected invalid with errors): %+v", c.Expected)
	}
	return c.Input.Dayparts, c.Expected.Errors[0].Code, c.Expected.Errors[0].Field
}

// DAT-073: two dayparts of one schedule sharing a weekday (Wednesday) and an
// intersecting half-open time-of-day interval ([12:00,13:00)) are rejected
// DAYPART_OVERLAP. Wired to the named corpus case.
func TestDAT073WithinScheduleOverlapRejected(t *testing.T) {
	dayparts, wantCode, wantField := loadCorpusDayparts(t, "DAT-073-invalid-within-schedule-daypart-overlap-rejected.json")
	if wantCode != "DAYPART_OVERLAP" {
		t.Fatalf("corpus code drifted: %q", wantCode)
	}
	e := ValidateNoOverlap(dayparts)
	if e == nil {
		t.Fatalf("expected DAYPART_OVERLAP, got nil")
	}
	if e.Code != wantCode || e.Field != wantField {
		t.Fatalf("expected {code:%q field:%q}, got {code:%q field:%q}", wantCode, wantField, e.Code, e.Field)
	}
}

// DAT-073: a Monday 22:00-06:00 wrapping daypart's post-midnight tail lands on
// Tuesday [00:00,06:00) and collides with a Tuesday 05:00-07:00 daypart on
// [05:00,06:00) — rejected DAYPART_OVERLAP even though days_of_week {1} and {2}
// are disjoint. Wired to the named corpus case.
func TestDAT073MidnightWrapCollidesNextWeekday(t *testing.T) {
	dayparts, wantCode, wantField := loadCorpusDayparts(t, "DAT-073-invalid-midnight-wrap-collides-next-weekday.json")
	e := ValidateNoOverlap(dayparts)
	if e == nil {
		t.Fatalf("expected DAYPART_OVERLAP for wrap-tail collision, got nil")
	}
	if e.Code != wantCode || e.Field != wantField {
		t.Fatalf("expected {code:%q field:%q}, got {code:%q field:%q}", wantCode, wantField, e.Code, e.Field)
	}
}

// Non-overlapping dayparts of one schedule are accepted: half-open boundaries
// abut without intersecting ([06:00,22:00) and a 22:00-06:00 wrap partition the
// day, DAT-073 partition-not-layer).
func TestDaypartsPartitionAccepted(t *testing.T) {
	business := Daypart{
		ID: "01JDPA", ScheduleID: "01JS", ScopeNode: "01JN",
		DaysOfWeek: []int{1, 2, 3, 4, 5}, StartTime: "06:00:00", EndTime: "22:00:00", DisplayPower: "on",
	}
	overnight := Daypart{
		ID: "01JDPB", ScheduleID: "01JS", ScopeNode: "01JN",
		DaysOfWeek: []int{0, 1, 2, 3, 4, 5, 6}, StartTime: "22:00:00", EndTime: "06:00:00", DisplayPower: "off",
	}
	if e := ValidateNoOverlap([]Daypart{business, overnight}); e != nil {
		t.Fatalf("abutting half-open dayparts must NOT overlap, got %+v", e)
	}
}

// The overlap check partitions time WITHIN a schedule only: two dayparts of
// DIFFERENT schedules may cover the same weekday+time (they layer cross-schedule,
// DAT-111), and are never a DAYPART_OVERLAP.
func TestOverlapIsPerScheduleOnly(t *testing.T) {
	a := Daypart{ID: "01JA", ScheduleID: "01JS-A", ScopeNode: "01JN", DaysOfWeek: []int{3}, StartTime: "09:00:00", EndTime: "17:00:00", DisplayPower: "on"}
	b := Daypart{ID: "01JB", ScheduleID: "01JS-B", ScopeNode: "01JN", DaysOfWeek: []int{3}, StartTime: "12:00:00", EndTime: "13:00:00", DisplayPower: "on"}
	if e := ValidateNoOverlap([]Daypart{a, b}); e != nil {
		t.Fatalf("dayparts of different schedules must not trip DAYPART_OVERLAP, got %+v", e)
	}
}

// Segment expansion is DST-agnostic and half-open: a wrap (end<=start) yields the
// pre-midnight [start,24:00) segment plus a next-weekday [00:00,end) tail; a
// non-wrap yields one [start,end) segment per day (DAT-072/073).
func TestDaypartSegmentExpansion(t *testing.T) {
	// Non-wrapping Wednesday 09:00-13:00.
	nonWrap := Daypart{DaysOfWeek: []int{3}, StartTime: "09:00:00", EndTime: "13:00:00"}
	segs, ok := daypartSegments(nonWrap)
	if !ok || len(segs) != 1 {
		t.Fatalf("non-wrap: want 1 segment, got %v (ok=%v)", segs, ok)
	}
	if segs[0] != (segment{weekday: 3, from: 9 * 3600, until: 13 * 3600}) {
		t.Fatalf("non-wrap segment wrong: %+v", segs[0])
	}
	// Wrapping Monday 22:00-06:00 -> Mon [22:00,24:00) + Tue [00:00,06:00).
	wrap := Daypart{DaysOfWeek: []int{1}, StartTime: "22:00:00", EndTime: "06:00:00"}
	segs, ok = daypartSegments(wrap)
	if !ok || len(segs) != 2 {
		t.Fatalf("wrap: want 2 segments, got %v (ok=%v)", segs, ok)
	}
	wantHead := segment{weekday: 1, from: 22 * 3600, until: 86400}
	wantTail := segment{weekday: 2, from: 0, until: 6 * 3600}
	if segs[0] != wantHead || segs[1] != wantTail {
		t.Fatalf("wrap segments wrong: %+v", segs)
	}
}
