package datamodel

import (
	"testing"
	"time"
)

// Five guards found by mutation: each could be deleted with the whole TREE
// green, not merely this package's own suite.
//
// They share a source. `TestHolds_NilLocationFailsClosed` pins one of the two
// ways Holds can be handed something unusable — a nil location — and nothing
// pinned the other, an unparseable time-of-day string. The daypart tests are
// otherwise built from corpus vectors, and a corpus of VALID cases exercises no
// refusal at all.
//
// What makes these worth pinning rather than noting: DAT-071 fixes the
// `HH:MM:SS` format, and nothing validates it at authoring time. A malformed
// `start_time` can be stored, so these runtime refusals are not defence in
// depth — they are the whole defence. See the filed gap for the authoring half.

// TestAnUnparseableDaypartHoldsNowhereRatherThanAllDay is the sharpest of the
// five, because deleting its guard does not merely lose a refusal — it inverts
// one.
//
// daypartSegments reports ok=false when either time-of-day fails to parse, and
// Holds documents the consequence: such a daypart "holds nowhere (fail-closed)".
// Without the check, parseTimeOfDay's error return is discarded and both bounds
// are ZERO. Zero end is not greater than zero start, so DAT-072's wrapping
// branch runs; `0 < secondsPerDay` holds, and the pre-midnight head emitted is
// `[0, 86400)` — every second of every declared weekday.
//
// So the failure mode is not that a garbled daypart stops working. It is that a
// garbled daypart takes over the entire day, on every day it names, commanding
// whatever display_power and playlist it carries. Fail-closed becomes the widest
// possible fail-open.
func TestAnUnparseableDaypartHoldsNowhereRatherThanAllDay(t *testing.T) {
	loc := mustLoadNY(t)
	// Thursday 2026-08-06, three readings across the day.
	instants := []int64{
		mustUTCms(t, "2026-08-06T05:00:00Z"), // 01:00 local
		mustUTCms(t, "2026-08-06T17:00:00Z"), // 13:00 local
		mustUTCms(t, "2026-08-07T02:00:00Z"), // 22:00 local
	}

	for _, tc := range []struct {
		name            string
		start, end      string
		whatItLooksLike string
	}{
		{"both bounds garbage", "not a time", "not a time", "00:00:00-00:00:00"},
		{"an empty end_time", "09:00:00", "", "09:00:00 to midnight"},
		{"an empty start_time", "", "17:00:00", "midnight to 17:00:00"},
		{"a two-field time", "09:00", "17:00", "a plausible typo"},
		{"a non-numeric field", "09:AB:00", "17:00:00", "09:00:00-17:00:00"},
		{"an out-of-range hour", "25:00:00", "17:00:00", "an hour that does not exist"},
		{"an out-of-range minute", "09:70:00", "17:00:00", "10:10:00 if renormalized"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dp := Daypart{
				ID: "01JDP", ScheduleID: "01JS", DaysOfWeek: []int{0, 1, 2, 3, 4, 5, 6},
				StartTime: tc.start, EndTime: tc.end, DisplayPower: "on",
			}
			for _, at := range instants {
				if Holds(dp, loc, at) {
					t.Errorf("a daypart declaring start=%q end=%q held at %s — an unparseable daypart must hold "+
						"NOWHERE (fail-closed); this one reads as %s, and the widest form of this fault covers "+
						"every second of every day it names",
						tc.start, tc.end, time.UnixMilli(at).In(loc).Format("Mon 15:04:05"), tc.whatItLooksLike)
				}
			}
		})
	}

	// The control: a well-formed daypart with the same shape still holds inside
	// its window and not outside it. Without this, a Holds that answered false to
	// everything would satisfy every case above while silencing every schedule on
	// the platform.
	good := Daypart{
		ID: "01JDP", ScheduleID: "01JS", DaysOfWeek: []int{0, 1, 2, 3, 4, 5, 6},
		StartTime: "09:00:00", EndTime: "17:00:00", DisplayPower: "on",
	}
	if !Holds(good, loc, instants[1]) {
		t.Error("a well-formed 09:00-17:00 daypart did not hold at 13:00 local")
	}
	if Holds(good, loc, instants[0]) {
		t.Error("a well-formed 09:00-17:00 daypart held at 01:00 local")
	}
}

// TestParseTimeOfDayRefusesRatherThanRenormalizing covers the two guards inside
// the parser, and states why each refusal is not interchangeable with a
// correction.
//
// Both are silent-acceptance faults, and the out-of-range one is the more
// insidious: strconv.Atoi succeeds on "70", so `09:70:00` has no error to
// propagate. Renormalized it would mean 10:10:00 — a daypart that runs an hour
// and ten minutes later than the operator wrote, forever, with nothing anywhere
// reporting a problem. `24:00:00` is worse in the opposite direction: it equals
// secondsPerDay exactly, so the pre-midnight head is never emitted and the
// daypart silently covers nothing.
func TestParseTimeOfDayRefusesRatherThanRenormalizing(t *testing.T) {
	for _, tc := range []struct{ in, why string }{
		{"09:AB:00", "Atoi returns 0 on failure, so an unchecked parse reads this as 09:00:00"},
		{"AB:CD:EF", "the same, reading as midnight"},
		{"::", "three empty fields, each parsing to 0"},
		{"25:00:00", "an hour past the day"},
		{"09:70:00", "would renormalize to 10:10:00 — an hour and ten minutes from what was written"},
		{"09:00:60", "a sixtieth second"},
		{"24:00:00", "equals secondsPerDay exactly, so a wrapping head is dropped and coverage vanishes"},
		{"-1:00:00", "Atoi accepts a negative, which would make a segment start before the day"},
		{"09:-1:00", "the same in the minute field"},
	} {
		if got, err := parseTimeOfDay(tc.in); err == nil {
			t.Errorf("parseTimeOfDay(%q) = %d with no error — %s", tc.in, got, tc.why)
		}
	}

	// The control: the boundaries that ARE legal parse to what they say. Without
	// it, a parser that refused everything would satisfy every case above.
	for _, tc := range []struct {
		in   string
		want int
	}{
		{"00:00:00", 0},
		{"09:00:00", 9 * 3600},
		{"23:59:59", 23*3600 + 59*60 + 59},
	} {
		got, err := parseTimeOfDay(tc.in)
		if err != nil {
			t.Errorf("parseTimeOfDay(%q) refused a legal time: %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("parseTimeOfDay(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// TestDaypartSegmentsEmitsNothingWhenItReportsNotOk pins the invariant Holds
// RELIES ON rather than a behaviour a test could otherwise distinguish.
//
// Holds guards its loop with `if !ok { return false }`, and that guard is an
// equivalent mutant today: daypartSegments returns a nil segment slice alongside
// ok=false, so ranging over the result without checking ok also yields false.
// Deleting the guard changes nothing — which is exactly why it is worth writing
// down what keeps it that way.
//
// The coupling is invisible at the call site. If daypartSegments ever returned
// the segments it had accumulated BEFORE failing — a natural-looking change when
// per-day parsing is added — Holds would begin honouring the partial coverage of
// a daypart it had just judged unusable, and no existing test would notice. This
// test fails at that moment instead.
func TestDaypartSegmentsEmitsNothingWhenItReportsNotOk(t *testing.T) {
	for _, dp := range []Daypart{
		{DaysOfWeek: []int{0, 1, 2, 3, 4, 5, 6}, StartTime: "bad", EndTime: "17:00:00"},
		{DaysOfWeek: []int{0, 1, 2, 3, 4, 5, 6}, StartTime: "09:00:00", EndTime: "bad"},
		{DaysOfWeek: []int{3}, StartTime: "25:00:00", EndTime: "26:00:00"},
	} {
		segs, ok := daypartSegments(dp)
		if ok {
			t.Errorf("daypartSegments(%q-%q) reported ok", dp.StartTime, dp.EndTime)
			continue
		}
		if len(segs) != 0 {
			t.Errorf("daypartSegments(%q-%q) reported NOT ok but returned %d segment(s) %v — Holds discards them "+
				"only because it checks ok first, and a caller that forgot to would honour the coverage of a "+
				"daypart this function has just judged unusable",
				dp.StartTime, dp.EndTime, len(segs), segs)
		}
	}

	// The control: a well-formed daypart does emit segments, so the assertion
	// above is not satisfied by a function that returns nothing for everything.
	segs, ok := daypartSegments(Daypart{DaysOfWeek: []int{1, 2}, StartTime: "09:00:00", EndTime: "17:00:00"})
	if !ok || len(segs) != 2 {
		t.Fatalf("a well-formed two-day daypart produced ok=%v segs=%v, want ok and 2 segments", ok, segs)
	}
}

// TestEffectiveGeoNamesTheUnknownNodeRatherThanBlamingTheTree.
//
// Both branches return an error, so the mutation costs no refusal — it changes
// the DIAGNOSIS, and the two codes carry different operator remedies in the
// error register. EFFECTIVE_GEO_UNRESOLVED's remedy is "declare geo on a site
// ancestor" (DAT-034); SCOPE_NODE_PARENT_INVALID's is that the id names no
// existing node (DAT-002).
//
// Without the guard an unknown id reads as the zero ScopeNode: no own geo, no
// parent, so the ancestor walk ends immediately and the function reports the
// tree has no site ancestor with geo. An operator then goes looking for missing
// geo on a tree that is entirely correct, for a node id that was simply wrong —
// and the tree they are sent to inspect may be large.
func TestEffectiveGeoNamesTheUnknownNodeRatherThanBlamingTheTree(t *testing.T) {
	const siteID = "01JS1TE0NENDVGH0AVNH9DN6R0"
	tree, errs := BuildScopeTree([]ScopeNode{
		{ID: siteID, Kind: "site", ParentID: ptrStr("01J0RG0NENDVPG2X7R5JQC42EJ"), Name: "Site One",
			TZ: ptrStr("America/Denver"), Lat: ptrF64(39.7392), Long: ptrF64(-104.9903)},
	})
	if len(errs) != 0 {
		t.Fatalf("BuildScopeTree: %+v", errs)
	}

	_, err := tree.EffectiveGeo("01JNOSUCHN0DE0NENDVPG2X7R5")
	if err == nil {
		t.Fatal("EffectiveGeo resolved a node id that is not in the tree")
	}
	if err.Code != "SCOPE_NODE_PARENT_INVALID" {
		t.Errorf("an unknown node id reported %s, want SCOPE_NODE_PARENT_INVALID — %s sends an operator to declare "+
			"geo on a site ancestor of a tree that is already correct, for an id that names no node at all",
			err.Code, err.Code)
	}

	// The control: a node that IS in the tree resolves, so the assertion above is
	// not satisfied by a function that reported the same code for everything.
	geo, gerr := tree.EffectiveGeo(siteID)
	if gerr != nil {
		t.Fatalf("a site with declared geo did not resolve: %v", gerr)
	}
	if geo.TZ != "America/Denver" {
		t.Errorf("tz = %q, want America/Denver", geo.TZ)
	}

	// And a real node whose chain genuinely reaches no site keeps the OTHER code,
	// so the two remain distinguishable rather than one having swallowed the other.
	bare, _ := BuildScopeTree([]ScopeNode{{ID: "grpA", Kind: "group", ParentID: ptrStr("absent-root"), Name: "Group A"}})
	if _, e := bare.EffectiveGeo("grpA"); e == nil || e.Code != "EFFECTIVE_GEO_UNRESOLVED" {
		t.Errorf("a real node with no site ancestor reported %v, want EFFECTIVE_GEO_UNRESOLVED", e)
	}
}

// TestKindOfReportsAnUnknownNodeAsNotFound.
//
// KindOf has no non-test caller today, which is why nothing killed the mutation.
// It is exported, so that is a property of this moment rather than of the
// function: the first caller inherits whatever it does now.
//
// Without the guard an unknown id returns ("", true) — the zero node's empty
// kind, asserted to exist. A caller comparing against a kind is unaffected, since
// "" matches none; a caller that trusts ok to mean the node is in the tree is
// told a node exists that does not.
func TestKindOfReportsAnUnknownNodeAsNotFound(t *testing.T) {
	const siteID = "01JS1TE0NENDVGH0AVNH9DN6R0"
	tree, errs := BuildScopeTree([]ScopeNode{
		{ID: siteID, Kind: "site", ParentID: ptrStr("01J0RG0NENDVPG2X7R5JQC42EJ"), Name: "Site One",
			TZ: ptrStr("America/Denver"), Lat: ptrF64(39.7392), Long: ptrF64(-104.9903)},
	})
	if len(errs) != 0 {
		t.Fatalf("BuildScopeTree: %+v", errs)
	}

	for _, id := range []string{"01JNOSUCHN0DE0NENDVPG2X7R5", ""} {
		if kind, ok := tree.KindOf(id); ok {
			t.Errorf("KindOf(%q) reported ok with kind %q — a node id that is not in the tree must not be reported "+
				"as one that is", id, kind)
		}
	}

	// The control: a node that IS in the tree reports its own kind.
	if kind, ok := tree.KindOf(siteID); !ok || kind != "site" {
		t.Errorf("KindOf(site) = (%q, %v), want (site, true)", kind, ok)
	}
}

// TestAPhantomWeekdayContributesNoCoverage pins the runtime half of DAT-071's
// days_of_week rule: a member outside 0–6 expands to NOTHING, exactly as an
// unparseable time does (the test above).
//
// The direction of this failure is what makes it worth its own pin. A phantom
// weekday's HEAD matches no real weekday, so it fails silent — but a WRAPPING
// daypart's tail is placed on (day+1) mod 7, which for a phantom 9 is 3, a
// real Wednesday. Without the guard the row fails OPEN: real coverage, on a
// real day, during hours whose author named neither. The write gate refuses
// such a row at authoring; this is the evaluator's own refusal to expand one
// however it arrives.
func TestAPhantomWeekdayContributesNoCoverage(t *testing.T) {
	loc := mustLoadNY(t)
	dp := Daypart{
		ID: "01JDP", ScheduleID: "01JS", DaysOfWeek: []int{9},
		StartTime: "22:00:00", EndTime: "06:00:00", DisplayPower: "on",
	}
	// Wednesday 2026-08-05 01:00 local — squarely inside where the phantom
	// day's wrap tail would land ((9+1) mod 7 = 3).
	at := mustUTCms(t, "2026-08-05T05:00:00Z")
	if Holds(dp, loc, at) {
		t.Error("a daypart declared only on phantom weekday 9 held on a real Wednesday — " +
			"the (day+1) mod 7 wrap tail turned an invalid weekday into real coverage")
	}

	// A phantom day mixed with a real one: the real day's coverage stands, the
	// phantom contributes nothing. Tuesday 23:00 local is inside day-2's head;
	// Wednesday 01:00 local is inside day-2's tail (real, (2+1) mod 7 = 3) —
	// so the tail assertion above is about day 9's phantom tail, not wrapping
	// generally.
	mixed := Daypart{
		ID: "01JDP", ScheduleID: "01JS", DaysOfWeek: []int{2, 9},
		StartTime: "22:00:00", EndTime: "06:00:00", DisplayPower: "on",
	}
	if !Holds(mixed, loc, mustUTCms(t, "2026-08-05T03:00:00Z")) { // Tue 23:00 local
		t.Error("the real day-2 head did not hold at Tuesday 23:00 local")
	}
	if !Holds(mixed, loc, at) { // Wed 01:00 local, day-2's REAL tail
		t.Error("the real day-2 wrap tail did not hold at Wednesday 01:00 local")
	}
	// Friday 01:00 local: only phantom 9's tail could have put coverage here
	// ((9+1) mod 7 = 3 is Wednesday, so nothing should hold Friday).
	if Holds(mixed, loc, mustUTCms(t, "2026-08-07T05:00:00Z")) {
		t.Error("coverage appeared on Friday — only a phantom-day expansion could put it there")
	}
}
