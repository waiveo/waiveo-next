package datamodel

import (
	"testing"
	"time"
)

// ptrInt boxes an int for a Schedule.Priority literal in these vectors.
func ptrInt(v int) *int { return &v }

// mustLocalMs converts a corpus `evaluated_at_local` wall-clock string to an
// absolute Unix-ms instant in the named IANA zone — the same local→absolute
// mapping the resolution engine's caller performs against the node's effective
// tz (DAT-033). All data-model-1 resolution vectors quote America/Denver.
func mustLocalMs(t *testing.T, tzName, local string) int64 {
	t.Helper()
	loc, err := time.LoadLocation(tzName)
	if err != nil {
		t.Fatalf("LoadLocation(%q): %v", tzName, err)
	}
	tm, err := time.ParseInLocation("2006-01-02T15:04:05", local, loc)
	if err != nil {
		t.Fatalf("ParseInLocation(%q): %v", local, err)
	}
	return tm.UnixMilli()
}

// denverTree builds the corpus's canonical org→site(Denver)→screen scope tree.
// The site declares the effective tz/geo unit (DAT-031); the screen inherits it
// by ancestor walk (DAT-033).
func denverTree(t *testing.T) ScopeTree {
	t.Helper()
	nodes := []ScopeNode{
		{ID: "01JORGNDE1PG2X7R5JQC42EJ00", Kind: "org", ParentID: nil},
		{ID: "01JSITENDE1EWH0AVNH9DN6R00", Kind: "site", ParentID: ptrStr("01JORGNDE1PG2X7R5JQC42EJ00"), TZ: ptrStr("America/Denver"), Lat: ptrF64(39.7392), Long: ptrF64(-104.9903)},
		{ID: "01JSCRNVERRDEF4W6305FAT000", Kind: "screen", ParentID: ptrStr("01JSITENDE1EWH0AVNH9DN6R00")},
	}
	tree, errs := BuildScopeTree(nodes)
	if len(errs) != 0 {
		t.Fatalf("BuildScopeTree: %+v", errs)
	}
	return tree
}

const (
	orgID  = "01JORGNDE1PG2X7R5JQC42EJ00"
	siteID = "01JSITENDE1EWH0AVNH9DN6R00"
	scrnID = "01JSCRNVERRDEF4W6305FAT000"
)

// DAT-051-valid-site-schedule-cascades-to-screen: a site schedule with no
// screen-level schedule governs the screen by cascade; its daypart drives the
// Lease (display content, playlist source).
func TestResolveSiteCascadesToScreen(t *testing.T) {
	store := RowStore{
		Tree: denverTree(t),
		Rows: RowSet{
			Schedules: []Schedule{
				{ID: "01JSCHEDSITE0RGXJVZ9CJD000", ScopeNode: siteID, Name: "Site Base", Priority: ptrInt(0)},
			},
			Dayparts: []Daypart{
				{ID: "01JDAYPARTSITE7RCG2V0CQV00", ScheduleID: "01JSCHEDSITE0RGXJVZ9CJD000", ScopeNode: siteID,
					DaysOfWeek: []int{1, 2, 3, 4, 5}, StartTime: "06:00:00", EndTime: "22:00:00",
					DisplayPower: "on", PlaylistID: "01JPAYST15TMGDMFGS8KXM40X6"},
			},
		},
	}
	tMs := mustLocalMs(t, "America/Denver", "2026-07-14T12:00:00")

	got := ApplicableSchedules(store, scrnID, tMs)
	if len(got) != 1 || got[0].ID != "01JSCHEDSITE0RGXJVZ9CJD000" {
		t.Fatalf("applicable = %v, want [01JSCHEDSITE0RGXJVZ9CJD000]", ids(got))
	}

	st, err := Resolve(store, scrnID, tMs)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if st.Daypart == nil || st.Daypart.ID != "01JDAYPARTSITE7RCG2V0CQV00" {
		t.Fatalf("effective daypart = %v, want 01JDAYPARTSITE7RCG2V0CQV00", st.Daypart)
	}
	if st.Display != "content" || st.PlaylistID != "01JPAYST15TMGDMFGS8KXM40X6" {
		t.Fatalf("lease = {%s, %s}, want {content, 01JPAYST15TMGDMFGS8KXM40X6}", st.Display, st.PlaylistID)
	}
}

// DAT-053-valid-equal-priority-same-node-lowest-id-wins: same node, equal
// priority, both holding — lowest schedule id wins (key 3).
func TestResolveEqualPriorityLowestIDWins(t *testing.T) {
	store := RowStore{
		Tree: denverTree(t),
		Rows: RowSet{
			Schedules: []Schedule{
				{ID: "01JSCHEDZZ0RGXJVZ9CJD00000", ScopeNode: scrnID, Name: "Zulu", Priority: ptrInt(5)},
				{ID: "01JSCHEDAA0RGXJVZ9CJD00000", ScopeNode: scrnID, Name: "Alpha", Priority: ptrInt(5)},
			},
			Dayparts: []Daypart{
				{ID: "01JDAYPARTALPHA7RCG2V0CQV0", ScheduleID: "01JSCHEDAA0RGXJVZ9CJD00000", ScopeNode: scrnID,
					DaysOfWeek: []int{1, 2, 3, 4, 5}, StartTime: "06:00:00", EndTime: "22:00:00",
					DisplayPower: "on", PlaylistID: "01JPAYSTALPHA1MGDMFGS8KX0"},
				{ID: "01JDAYPARTZULU7RCG2V0CQV00", ScheduleID: "01JSCHEDZZ0RGXJVZ9CJD00000", ScopeNode: scrnID,
					DaysOfWeek: []int{1, 2, 3, 4, 5}, StartTime: "06:00:00", EndTime: "22:00:00",
					DisplayPower: "on", PlaylistID: "01JPAYSTZULU1MGDMFGS8KX00"},
			},
		},
	}
	tMs := mustLocalMs(t, "America/Denver", "2026-07-14T12:00:00")

	got := ApplicableSchedules(store, scrnID, tMs)
	if want := []string{"01JSCHEDAA0RGXJVZ9CJD00000", "01JSCHEDZZ0RGXJVZ9CJD00000"}; !eqIDs(got, want) {
		t.Fatalf("precedence = %v, want %v", ids(got), want)
	}
	st, err := Resolve(store, scrnID, tMs)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if st.ScheduleID != "01JSCHEDAA0RGXJVZ9CJD00000" || st.Daypart == nil || st.Daypart.ID != "01JDAYPARTALPHA7RCG2V0CQV0" {
		t.Fatalf("winner = %s / %v, want alpha", st.ScheduleID, st.Daypart)
	}
	if st.PlaylistID != "01JPAYSTALPHA1MGDMFGS8KX0" {
		t.Fatalf("playlist = %s, want alpha", st.PlaylistID)
	}
}

// DAT-053-valid-equal-priority-specificity-nearer-node-wins: equal priority,
// key 2 (specificity) — the screen-level schedule (distance 0) outranks the
// site-level (distance 1).
func TestResolveEqualPrioritySpecificityWins(t *testing.T) {
	store := RowStore{
		Tree: denverTree(t),
		Rows: RowSet{
			Schedules: []Schedule{
				{ID: "01JSCHEDSITE0RGXJVZ9CJD000", ScopeNode: siteID, Name: "Site Base", Priority: ptrInt(5)},
				{ID: "01JSCHEDSCRN0RGXJVZ9CJD000", ScopeNode: scrnID, Name: "Screen Local", Priority: ptrInt(5)},
			},
			Dayparts: []Daypart{
				{ID: "01JDAYPARTSITE7RCG2V0CQV00", ScheduleID: "01JSCHEDSITE0RGXJVZ9CJD000", ScopeNode: siteID,
					DaysOfWeek: []int{1, 2, 3, 4, 5}, StartTime: "06:00:00", EndTime: "22:00:00",
					DisplayPower: "on", PlaylistID: "01JPAYSTSITE1MGDMFGS8KX00"},
				{ID: "01JDAYPARTSCRN7RCG2V0CQV00", ScheduleID: "01JSCHEDSCRN0RGXJVZ9CJD000", ScopeNode: scrnID,
					DaysOfWeek: []int{1, 2, 3, 4, 5}, StartTime: "06:00:00", EndTime: "22:00:00",
					DisplayPower: "on", PlaylistID: "01JPAYSTSCRN1MGDMFGS8KX00"},
			},
		},
	}
	tMs := mustLocalMs(t, "America/Denver", "2026-07-14T12:00:00")

	if want := []string{"01JSCHEDSCRN0RGXJVZ9CJD000", "01JSCHEDSITE0RGXJVZ9CJD000"}; !eqIDs(ApplicableSchedules(store, scrnID, tMs), want) {
		t.Fatalf("precedence order wrong: %v", ids(ApplicableSchedules(store, scrnID, tMs)))
	}
	st, _ := Resolve(store, scrnID, tMs)
	if st.ScheduleID != "01JSCHEDSCRN0RGXJVZ9CJD000" || st.PlaylistID != "01JPAYSTSCRN1MGDMFGS8KX00" {
		t.Fatalf("winner = %s / %s, want screen", st.ScheduleID, st.PlaylistID)
	}
}

// DAT-053-valid-screen-schedule-precedence-over-site: key 1 (priority) — the
// screen override (priority 10) beats the site base (priority 0); the base is
// masked and does not show.
func TestResolvePriorityOverridesSite(t *testing.T) {
	store := RowStore{
		Tree: denverTree(t),
		Rows: RowSet{
			Schedules: []Schedule{
				{ID: "01JSCHEDSITE0RGXJVZ9CJD000", ScopeNode: siteID, Name: "Site Base", Priority: ptrInt(0)},
				{ID: "01JSCHEDSCRN0RGXJVZ9CJD000", ScopeNode: scrnID, Name: "Screen Override", Priority: ptrInt(10)},
			},
			Dayparts: []Daypart{
				{ID: "01JDAYPARTSITE7RCG2V0CQV00", ScheduleID: "01JSCHEDSITE0RGXJVZ9CJD000", ScopeNode: siteID,
					DaysOfWeek: []int{1, 2, 3, 4, 5}, StartTime: "06:00:00", EndTime: "22:00:00",
					DisplayPower: "on", PlaylistID: "01JPAYSTSITE1MGDMFGS8KX00"},
				{ID: "01JDAYPARTSCRN7RCG2V0CQV00", ScheduleID: "01JSCHEDSCRN0RGXJVZ9CJD000", ScopeNode: scrnID,
					DaysOfWeek: []int{1, 2, 3, 4, 5}, StartTime: "09:00:00", EndTime: "17:00:00",
					DisplayPower: "on", PlaylistID: "01JPAYSTSCRN1MGDMFGS8KX00"},
			},
		},
	}
	tMs := mustLocalMs(t, "America/Denver", "2026-07-14T12:00:00")
	st, _ := Resolve(store, scrnID, tMs)
	if st.ScheduleID != "01JSCHEDSCRN0RGXJVZ9CJD000" || st.Daypart.ID != "01JDAYPARTSCRN7RCG2V0CQV00" || st.PlaylistID != "01JPAYSTSCRN1MGDMFGS8KX00" {
		t.Fatalf("winner = %s / %v, want screen override", st.ScheduleID, st.Daypart)
	}
}

// DAT-111-valid-layered-daypart-holiday-over-base: per-instant layering. At
// noon the holiday (priority 100, 10-14) wins; at 16:00 it no longer holds so
// the base (priority 0, 06-22) shows through. Same rows, two instants.
func TestResolveLayeringPerInstant(t *testing.T) {
	store := RowStore{
		Tree: denverTree(t),
		Rows: RowSet{
			Schedules: []Schedule{
				{ID: "01JSCHEDBASE0RGXJVZ9CJD000", ScopeNode: scrnID, Name: "Base", Priority: ptrInt(0)},
				{ID: "01JSCHEDHOLI0RGXJVZ9CJD000", ScopeNode: scrnID, Name: "Holiday", Priority: ptrInt(100)},
			},
			Dayparts: []Daypart{
				{ID: "01JDAYPARTBASE7RCG2V0CQV00", ScheduleID: "01JSCHEDBASE0RGXJVZ9CJD000", ScopeNode: scrnID,
					DaysOfWeek: []int{0, 1, 2, 3, 4, 5, 6}, StartTime: "06:00:00", EndTime: "22:00:00",
					DisplayPower: "on", PlaylistID: "01JPAYSTBASE1MGDMFGS8KX00"},
				{ID: "01JDAYPARTHOLI7RCG2V0CQV00", ScheduleID: "01JSCHEDHOLI0RGXJVZ9CJD000", ScopeNode: scrnID,
					DaysOfWeek: []int{0, 1, 2, 3, 4, 5, 6}, StartTime: "10:00:00", EndTime: "14:00:00",
					DisplayPower: "on", PlaylistID: "01JPAYSTHOLI1MGDMFGS8KX00"},
			},
		},
	}
	if want := []string{"01JSCHEDHOLI0RGXJVZ9CJD000", "01JSCHEDBASE0RGXJVZ9CJD000"}; !eqIDs(ApplicableSchedules(store, scrnID, mustLocalMs(t, "America/Denver", "2026-07-14T12:00:00")), want) {
		t.Fatalf("precedence order wrong")
	}

	noon, _ := Resolve(store, scrnID, mustLocalMs(t, "America/Denver", "2026-07-14T12:00:00"))
	if noon.ScheduleID != "01JSCHEDHOLI0RGXJVZ9CJD000" || noon.Daypart.ID != "01JDAYPARTHOLI7RCG2V0CQV00" || noon.PlaylistID != "01JPAYSTHOLI1MGDMFGS8KX00" {
		t.Fatalf("noon winner = %s / %v, want holiday", noon.ScheduleID, noon.Daypart)
	}
	late, _ := Resolve(store, scrnID, mustLocalMs(t, "America/Denver", "2026-07-14T16:00:00"))
	if late.ScheduleID != "01JSCHEDBASE0RGXJVZ9CJD000" || late.Daypart.ID != "01JDAYPARTBASE7RCG2V0CQV00" || late.PlaylistID != "01JPAYSTBASE1MGDMFGS8KX00" {
		t.Fatalf("late winner = %s / %v, want base show-through", late.ScheduleID, late.Daypart)
	}
}

// DAT-117-valid-layered-fallback-through-precedence: at an overnight instant no
// daypart holds; the highest-precedence schedule (override) declares no
// fallback and is skipped; the next (base) declares one, which resolves.
func TestResolveFallbackFallsThroughPrecedence(t *testing.T) {
	store := RowStore{
		Tree: denverTree(t),
		Rows: RowSet{
			Schedules: []Schedule{
				{ID: "01JSCHEDBASE0RGXJVZ9CJD000", ScopeNode: siteID, Name: "Base", FallbackID: "01JFABACKBASE9HJDNDGZG300", Priority: ptrInt(0)},
				{ID: "01JSCHEDOVER0RGXJVZ9CJD000", ScopeNode: scrnID, Name: "Override", Priority: ptrInt(50)},
			},
			Dayparts: []Daypart{
				{ID: "01JDAYPARTBASE7RCG2V0CQV00", ScheduleID: "01JSCHEDBASE0RGXJVZ9CJD000", ScopeNode: siteID,
					DaysOfWeek: []int{0, 1, 2, 3, 4, 5, 6}, StartTime: "06:00:00", EndTime: "22:00:00",
					DisplayPower: "on", PlaylistID: "01JPAYSTBASE1MGDMFGS8KX00"},
				{ID: "01JDAYPARTOVER7RCG2V0CQV00", ScheduleID: "01JSCHEDOVER0RGXJVZ9CJD000", ScopeNode: scrnID,
					DaysOfWeek: []int{0, 1, 2, 3, 4, 5, 6}, StartTime: "09:00:00", EndTime: "17:00:00",
					DisplayPower: "on", PlaylistID: "01JPAYSTOVER1MGDMFGS8KX00"},
			},
			Fallbacks: []Fallback{
				{ID: "01JFABACKBASE9HJDNDGZG300", ScopeNode: siteID, Name: "Base Fallback", DisplayPower: "on", PlaylistID: "01JPAYSTFALL1MGDMFGS8KX00"},
			},
		},
	}
	tMs := mustLocalMs(t, "America/Denver", "2026-07-14T03:00:00")
	st, err := Resolve(store, scrnID, tMs)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if st.Daypart != nil {
		t.Fatalf("effective daypart = %v, want none", st.Daypart)
	}
	if st.Fallback == nil || st.Fallback.ID != "01JFABACKBASE9HJDNDGZG300" {
		t.Fatalf("fallback = %v, want base fallback", st.Fallback)
	}
	if st.ScheduleID != "01JSCHEDBASE0RGXJVZ9CJD000" {
		t.Fatalf("resolved_from_schedule = %s, want base", st.ScheduleID)
	}
	if st.Display != "content" || st.PlaylistID != "01JPAYSTFALL1MGDMFGS8KX00" {
		t.Fatalf("lease = {%s, %s}, want {content, fall}", st.Display, st.PlaylistID)
	}
}

// DAT-118-valid-terminal-default-blank: the sole schedule's validity window has
// ended, so it is out of force; no daypart and no fallback are reachable and the
// terminal default display:blank asserts, never box-local.
func TestResolveTerminalDefaultBlank(t *testing.T) {
	store := RowStore{
		Tree: denverTree(t),
		Rows: RowSet{
			Schedules: []Schedule{
				{ID: "01JSCHEDEXPD0RGXJVZ9CJD000", ScopeNode: scrnID, Name: "Seasonal (expired)", Priority: ptrInt(0)},
			},
			ValidityWindows: []ValidityWindow{
				{ID: "01JVAWNEXPD4DG8P4FQJAWK000", ScheduleID: "01JSCHEDEXPD0RGXJVZ9CJD000", ScopeNode: scrnID,
					StartsAt: ptrI64(1735707600000), EndsAt: ptrI64(1738386000000)},
			},
			Dayparts: []Daypart{
				{ID: "01JDAYPARTEXPD7RCG2V0CQV00", ScheduleID: "01JSCHEDEXPD0RGXJVZ9CJD000", ScopeNode: scrnID,
					DaysOfWeek: []int{0, 1, 2, 3, 4, 5, 6}, StartTime: "06:00:00", EndTime: "22:00:00",
					DisplayPower: "on", PlaylistID: "01JPAYSTEXPD1MGDMFGS8KX00"},
			},
		},
	}
	tMs := int64(1784052000000) // corpus evaluated_at, well past ends_at.

	if got := ApplicableSchedules(store, scrnID, tMs); len(got) != 0 {
		t.Fatalf("applicable-in-force = %v, want []", ids(got))
	}
	st, err := Resolve(store, scrnID, tMs)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if st.Daypart != nil || st.Fallback != nil {
		t.Fatalf("state = %+v, want terminal blank", st)
	}
	if st.Display != "blank" || st.PlaylistID != "" || st.ScheduleID != "" {
		t.Fatalf("lease = {%s, %s, %s}, want {blank, , }", st.Display, st.PlaylistID, st.ScheduleID)
	}
}

// DAT-074-valid-daypart-display-power-off: the overnight daypart (display_power
// off, wrapping midnight) is effective at 23:30 and projects to a blank Lease
// while carrying its device-power-off preset batch; the daytime daypart
// (display_power on) projects to a content Lease.
func TestResolveDisplayPowerOffProjectsBlank(t *testing.T) {
	// This case's rows attach at a group; place that group under a Denver site so
	// the effective tz resolves by ancestor walk (DAT-033).
	const grpID = "01JGRPNDE1PG2X7R5JQC42EJ5E"
	nodes := []ScopeNode{
		{ID: orgID, Kind: "org", ParentID: nil},
		{ID: siteID, Kind: "site", ParentID: ptrStr(orgID), TZ: ptrStr("America/Denver"), Lat: ptrF64(39.7392), Long: ptrF64(-104.9903)},
		{ID: grpID, Kind: "group", ParentID: ptrStr(siteID)},
	}
	tree, errs := BuildScopeTree(nodes)
	if len(errs) != 0 {
		t.Fatalf("BuildScopeTree: %+v", errs)
	}
	store := RowStore{
		Tree: tree,
		Rows: RowSet{
			Schedules: []Schedule{
				{ID: "01JSCHED1S3AR0RGXJVZ9CJD33", ScopeNode: grpID, Name: "Weekday Lobby"},
			},
			Dayparts: []Daypart{
				{ID: "01JDAYPART2MV7RCG2V0CQV4NM", ScheduleID: "01JSCHED1S3AR0RGXJVZ9CJD33", ScopeNode: grpID,
					DaysOfWeek: []int{1, 2, 3, 4, 5}, StartTime: "06:00:00", EndTime: "22:00:00",
					DisplayPower: "on", PlaylistID: "01JPAYST15TMGDMFGS8KXM40X6"},
				{ID: "01JDAYPART1M33YA35B44FS7F2", ScheduleID: "01JSCHED1S3AR0RGXJVZ9CJD33", ScopeNode: grpID,
					DaysOfWeek: []int{0, 1, 2, 3, 4, 5, 6}, StartTime: "22:00:00", EndTime: "06:00:00",
					DisplayPower: "off", PresetBatchID: "01JPRESET1N8GAWV07492Q9V82"},
			},
		},
	}
	// 2026-07-14T23:30 local — inside the overnight wrap [22:00,06:00).
	st, err := Resolve(store, grpID, mustLocalMs(t, "America/Denver", "2026-07-14T23:30:00"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if st.Daypart == nil || st.Daypart.ID != "01JDAYPART1M33YA35B44FS7F2" {
		t.Fatalf("effective daypart = %v, want overnight", st.Daypart)
	}
	if st.Display != "blank" || st.PlaylistID != "" {
		t.Fatalf("overnight lease = {%s, %s}, want {blank, }", st.Display, st.PlaylistID)
	}
	if st.Daypart.PresetBatchID != "01JPRESET1N8GAWV07492Q9V82" {
		t.Fatalf("device power-off preset = %q, want 01JPRESET1N8GAWV07492Q9V82", st.Daypart.PresetBatchID)
	}

	// 2026-07-14T12:00 local — inside the daytime window, display_power on.
	day, _ := Resolve(store, grpID, mustLocalMs(t, "America/Denver", "2026-07-14T12:00:00"))
	if day.Daypart == nil || day.Daypart.ID != "01JDAYPART2MV7RCG2V0CQV4NM" {
		t.Fatalf("daytime daypart = %v, want business hours", day.Daypart)
	}
	if day.Display != "content" || day.PlaylistID != "01JPAYST15TMGDMFGS8KXM40X6" {
		t.Fatalf("daytime lease = {%s, %s}, want {content, playlist}", day.Display, day.PlaylistID)
	}
}

// ids extracts schedule ids in order for assertion messages.
func ids(ss []Schedule) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = s.ID
	}
	return out
}

func eqIDs(ss []Schedule, want []string) bool {
	if len(ss) != len(want) {
		return false
	}
	for i := range ss {
		if ss[i].ID != want[i] {
			return false
		}
	}
	return true
}
