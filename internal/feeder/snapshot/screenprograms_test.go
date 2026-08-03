package snapshot

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/maaxton/waiveo-next/internal/app/store"
	"github.com/maaxton/waiveo-next/internal/datamodel"
	"github.com/maaxton/waiveo-next/internal/relay/schedulehost"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// The seeded demo partitions every day into a content daypart (06:00–22:00) and
// a blank overnight one (22:00–06:00), both in the site node's America/Chicago.
// Every test below states which side of that partition it resolves at, so no
// assertion depends on when the suite happens to run — there is no wall clock in
// this file, only these two instants.
const (
	seedInstantDate     = "2026-07-15" // a Wednesday; both seeded dayparts hold every weekday
	seedContentLocal    = "12:00:00"   // inside the 06:00–22:00 content daypart
	seedBlankLocalHours = "02:00:00"   // inside the 22:00–06:00 overnight blank daypart
)

// instantAt returns the Unix-ms instant of a local wall time in the seeded
// site's own zone.
func instantAt(t *testing.T, localTime string) int64 {
	t.Helper()
	loc, err := time.LoadLocation("America/Chicago")
	if err != nil {
		t.Fatalf("LoadLocation: %v", err)
	}
	ts, err := time.ParseInLocation("2006-01-02 15:04:05", seedInstantDate+" "+localTime, loc)
	if err != nil {
		t.Fatalf("ParseInLocation: %v", err)
	}
	return ts.UnixMilli()
}

// contentInstant is midday in the seeded site's zone — inside the seeded content
// daypart, so a screen the seeded schedule governs resolves to display:content.
func contentInstant(t *testing.T) int64 { return instantAt(t, seedContentLocal) }

// blankInstant is 02:00 in the seeded site's zone — inside the seeded overnight
// daypart, so the same screen resolves to display:blank.
func blankInstant(t *testing.T) int64 { return instantAt(t, seedBlankLocalHours) }

// desiredState reads the store's desired state or fails the test.
func desiredState(t *testing.T, s *store.Store) store.DesiredStateResult {
	t.Helper()
	ds, err := s.DesiredState(context.Background())
	if err != nil {
		t.Fatalf("DesiredState: %v", err)
	}
	return ds
}

// programFor returns the derived program for screenID, or fails.
func programForScreen(t *testing.T, programs []wire.ScreenProgram, screenID string) wire.ScreenProgram {
	t.Helper()
	for _, p := range programs {
		if p.ScreenID == screenID {
			return p
		}
	}
	t.Fatalf("no derived program for screen %q; got %+v", screenID, programs)
	return wire.ScreenProgram{}
}

// createRow marshals and Creates a row, failing the test on rejection.
func createRow(t *testing.T, s *store.Store, kind store.Kind, row any) {
	t.Helper()
	body, err := json.Marshal(row)
	if err != nil {
		t.Fatalf("marshal %s: %v", kind, err)
	}
	if _, err := s.Create(context.Background(), kind, body); err != nil {
		t.Fatalf("create %s: %v", kind, err)
	}
}

// TestEditingAPlaylistChangesTheDeliveredProgram is the end-to-end proof that
// screen_programs is derived rather than declared: an operator edits the playlist
// a real screen's daypart plays, and the program that screen is DELIVERED changes
// to match — through the store, through the snapshot builder, out on the wire.
//
// The edit is expressed the way an operator's would be (a store Update of the
// playlist row) and the expectation is derived from the EDIT — the asset ref the
// new item names — never read back out of the builder's own answer.
func TestEditingAPlaylistChangesTheDeliveredProgram(t *testing.T) {
	ctx := context.Background()
	id := testIdentity(t)
	const originalAsset = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	const replacementAsset = "sha256:2222222222222222222222222222222222222222222222222222222222222222"

	s := seededStore(t, originalAsset)

	before, _, err := BuildFromStore(desiredState(t, s), "https://origin.example", id, contentInstant(t), nil)
	if err != nil {
		t.Fatalf("BuildFromStore before the edit: %v", err)
	}
	beforeProgram := programForScreen(t, before.Sections.ScreenPrograms, store.SeedScreenID)
	if len(beforeProgram.Content) != 1 || beforeProgram.Content[0].AssetRef != originalAsset {
		t.Fatalf("pre-edit content = %+v, want the one seeded asset %q", beforeProgram.Content, originalAsset)
	}

	// The operator's edit: the playlist now plays a DIFFERENT asset, and a second
	// one after it, with a per-item dwell override on the second.
	playlists, err := s.List(ctx, store.KindPlaylist, store.ListFilter{})
	if err != nil {
		t.Fatalf("list playlists: %v", err)
	}
	if len(playlists) != 1 {
		t.Fatalf("seeded store has %d playlists, want 1", len(playlists))
	}
	dwell := 30
	edited := datamodel.Playlist{
		ID:        playlists[0].ID,
		ScopeNode: "01J8Z4DEM0SCREENF1RSTPH0TN",
		Name:      "Demo Content Playlist",
		Items: []datamodel.PlaylistItem{
			{Source: "asset", AssetRef: replacementAsset},
			{Source: "asset", AssetRef: originalAsset, DurationSeconds: &dwell},
		},
	}
	body, err := json.Marshal(edited)
	if err != nil {
		t.Fatalf("marshal edited playlist: %v", err)
	}
	if _, err := s.Update(ctx, store.KindPlaylist, playlists[0].ID, playlists[0].Revision, body); err != nil {
		t.Fatalf("update playlist: %v", err)
	}

	after, _, err := BuildFromStore(desiredState(t, s), "https://origin.example", id, contentInstant(t), nil)
	if err != nil {
		t.Fatalf("BuildFromStore after the edit: %v", err)
	}
	afterProgram := programForScreen(t, after.Sections.ScreenPrograms, store.SeedScreenID)

	// The delivered program changed in the way the edit implies: two items, in
	// the authored order, the first the replacement asset, the second carrying
	// the authored 30s dwell as duration_ms.
	if len(afterProgram.Content) != 2 {
		t.Fatalf("post-edit content = %+v, want the 2 authored items", afterProgram.Content)
	}
	if got := afterProgram.Content[0].AssetRef; got != replacementAsset {
		t.Errorf("post-edit content[0].asset_ref = %q, want the newly authored %q", got, replacementAsset)
	}
	if got := afterProgram.Content[1].AssetRef; got != originalAsset {
		t.Errorf("post-edit content[1].asset_ref = %q, want %q", got, originalAsset)
	}
	if got, want := afterProgram.Content[1].DurationMS, int64(dwell)*1000; got != want {
		t.Errorf("post-edit content[1].duration_ms = %d, want the authored %ds override as ms (%d)", got, dwell, want)
	}
	if got, want := afterProgram.Content[0].URL, "https://origin.example/content/"+replacementAsset[len("sha256:"):]; got != want {
		t.Errorf("post-edit content[0].url = %q, want %q", got, want)
	}

	// A changed program ALWAYS churns the revision — the property a player uses
	// to decide to swap.
	if afterProgram.ProgramRevision == beforeProgram.ProgramRevision {
		t.Errorf("program_revision %q survived a playlist edit that changed the delivered content", afterProgram.ProgramRevision)
	}
	// …and the generation advanced, so the relay will actually pull it.
	if !(after.Generation > before.Generation) {
		t.Errorf("generation %d -> %d did not advance across the edit", before.Generation, after.Generation)
	}
}

// TestUnchangedProgramReproducesItsRevision is the other half of the revision
// property: a rebuild that delivers the same program must reproduce the same
// revision, so an unrelated store write never makes a player re-swap to what it
// is already showing.
func TestUnchangedProgramReproducesItsRevision(t *testing.T) {
	ctx := context.Background()
	id := testIdentity(t)
	const asset = "sha256:3333333333333333333333333333333333333333333333333333333333333333"
	s := seededStore(t, asset)

	first, _, err := BuildFromStore(desiredState(t, s), "https://origin.example", id, contentInstant(t), nil)
	if err != nil {
		t.Fatalf("BuildFromStore #1: %v", err)
	}

	// A write that touches nothing this screen's program depends on: a second,
	// unreferenced playlist. The generation advances; the program does not.
	createRow(t, s, store.KindPlaylist, datamodel.Playlist{
		ID: "01J8ZVNRE1ATEDP1AY11ST0010", ScopeNode: "01J8Z4DEM0SCREENF1RSTPH0TN",
		Name:  "Unreferenced Playlist",
		Items: []datamodel.PlaylistItem{{Source: "asset", AssetRef: asset}},
	})

	second, _, err := BuildFromStore(desiredState(t, s), "https://origin.example", id, contentInstant(t), nil)
	if err != nil {
		t.Fatalf("BuildFromStore #2: %v", err)
	}
	if !(second.Generation > first.Generation) {
		t.Fatalf("generation did not advance across the unrelated write")
	}

	a := programForScreen(t, first.Sections.ScreenPrograms, store.SeedScreenID)
	b := programForScreen(t, second.Sections.ScreenPrograms, store.SeedScreenID)
	if a.ProgramRevision != b.ProgramRevision {
		t.Errorf("program_revision churned (%q -> %q) across a write that changed nothing this screen plays", a.ProgramRevision, b.ProgramRevision)
	}
	_ = ctx
}

// TestTwoScreensUnderDifferentPlacementsGetDifferentPrograms proves the DAT-051
// applicability cascade and the DAT-053 precedence order are actually consulted,
// rather than a screen being matched to a schedule by proximity in a list.
//
// The tree is org → site → {screen node A, screen node B}. The SEEDED schedule
// sits on node A. A second schedule sits on node B playing a different playlist.
// Both screens resolve from the SAME store at the SAME instant, and must get
// different programs — A the seeded asset, B the second one.
//
// A third screen row is placed on node A ALONGSIDE the seeded one, because a
// screen row may share a node with another: it must get its own entry, carrying
// the same program as its node-mate rather than displacing it.
func TestTwoScreensUnderDifferentPlacementsGetDifferentPrograms(t *testing.T) {
	id := testIdentity(t)
	const assetA = "sha256:aaaa111111111111111111111111111111111111111111111111111111111111"
	const assetB = "sha256:bbbb222222222222222222222222222222222222222222222222222222222222"

	const (
		siteNode    = "01J8Z2Q1M8H8N4T0V1W2X3Y4Z5" // the seeded site
		nodeA       = "01J8Z4DEM0SCREENF1RSTPH0TN" // the seeded screen node
		nodeB       = "01J8ZN0DEB0000000000000010"
		screenB     = "01J8ZSCREENB00000000000001"
		screenAMate = "01J8ZSCREENAMATE0000000001"
		playlistB   = "01J8ZP1AY11STB000000000001"
		scheduleB   = "01J8ZSCHEDV1EB000000000010"
		daypartB    = "01J8ZDAYPARTB00000000000AB"
	)

	s := seededStore(t, assetA)
	siteParent := siteNode

	// Node B: a second screen-kind node under the same site, with its own
	// schedule, playlist and all-day content daypart.
	createRow(t, s, store.KindScopeNode, datamodel.ScopeNode{
		ID: nodeB, Kind: "screen", ParentID: &siteParent, Name: "Screen Node B",
	})
	createRow(t, s, store.KindScreen, datamodel.Screen{
		ID: screenB, ScopeNode: nodeB, Name: "Screen B",
	})
	createRow(t, s, store.KindScreen, datamodel.Screen{
		ID: screenAMate, ScopeNode: nodeA, Name: "Screen A's node-mate",
	})
	createRow(t, s, store.KindPlaylist, datamodel.Playlist{
		ID: playlistB, ScopeNode: nodeB, Name: "Playlist B",
		Items: []datamodel.PlaylistItem{{Source: "asset", AssetRef: assetB}},
	})
	createRow(t, s, store.KindSchedule, datamodel.Schedule{
		ID: scheduleB, ScopeNode: nodeB, Name: "Schedule B",
	})
	createRow(t, s, store.KindDaypart, datamodel.Daypart{
		ID: daypartB, ScheduleID: scheduleB, ScopeNode: nodeB,
		DaysOfWeek: []int{0, 1, 2, 3, 4, 5, 6}, StartTime: "00:00:00", EndTime: "00:00:00",
		DisplayPower: "on", PlaylistID: playlistB, Name: "All Day B",
	})

	snap, degrades, err := BuildFromStore(desiredState(t, s), "https://origin.example", id, contentInstant(t), nil)
	if err != nil {
		t.Fatalf("BuildFromStore: %v", err)
	}
	if len(degrades) != 0 {
		t.Fatalf("unexpected degrades over a well-formed store: %+v", degrades)
	}
	if got := len(snap.Sections.ScreenPrograms); got != 3 {
		t.Fatalf("derived %d screen programs, want one per screen ROW (3): %+v", got, snap.Sections.ScreenPrograms)
	}

	progA := programForScreen(t, snap.Sections.ScreenPrograms, store.SeedScreenID)
	progB := programForScreen(t, snap.Sections.ScreenPrograms, screenB)
	progMate := programForScreen(t, snap.Sections.ScreenPrograms, screenAMate)

	if len(progA.Content) != 1 || progA.Content[0].AssetRef != assetA {
		t.Errorf("screen A content = %+v, want the schedule on ITS node (asset %q)", progA.Content, assetA)
	}
	if len(progB.Content) != 1 || progB.Content[0].AssetRef != assetB {
		t.Errorf("screen B content = %+v, want the schedule on ITS node (asset %q)", progB.Content, assetB)
	}
	if progA.ProgramRevision == progB.ProgramRevision {
		t.Errorf("both placements produced program_revision %q; the cascade was not consulted", progA.ProgramRevision)
	}

	// Two screens on ONE node each get their own entry, carrying the same
	// program — derivation is per screen row, never per node.
	if progMate.ProgramRevision != progA.ProgramRevision {
		t.Errorf("node-mate revision %q != %q; two screens on one node resolve the same placement", progMate.ProgramRevision, progA.ProgramRevision)
	}
	if len(progMate.Content) != 1 || progMate.Content[0].AssetRef != assetA {
		t.Errorf("node-mate content = %+v, want screen A's own (asset %q)", progMate.Content, assetA)
	}
}

// TestSiteScheduleCascadesToAScreenPlacedOnTheSiteNode proves applicability is
// decided by placement in the TREE, not by a schedule sitting on the screen's own
// node: a screen row placed directly on the SITE node is governed by a schedule
// attached at that site, and is NOT governed by the seeded schedule that sits on
// a screen node below it (a schedule on a descendant governs nothing above it).
func TestSiteScheduleCascadesToAScreenPlacedOnTheSiteNode(t *testing.T) {
	id := testIdentity(t)
	const assetScreen = "sha256:cccc111111111111111111111111111111111111111111111111111111111111"
	const assetSite = "sha256:dddd222222222222222222222222222222222222222222222222222222222222"

	const (
		siteNode     = "01J8Z2Q1M8H8N4T0V1W2X3Y4Z5"
		siteScreen   = "01J8ZSCREEN0NS1TEN0DE00001"
		sitePlaylist = "01J8ZP1AY11STS1TE000000010"
		siteSchedule = "01J8ZSCHEDV1ES1TE0000001AB"
		siteDaypart  = "01J8ZDAYPARTS1TE00000001AB"
	)

	s := seededStore(t, assetScreen)

	// A screen row placed on the SITE node itself — legal (DAT-004: a screen row
	// may hang off a node of any kind) and the case a screen-node-only derivation
	// would have lost entirely.
	createRow(t, s, store.KindScreen, datamodel.Screen{
		ID: siteScreen, ScopeNode: siteNode, Name: "Lobby (placed on the site)",
	})
	createRow(t, s, store.KindPlaylist, datamodel.Playlist{
		ID: sitePlaylist, ScopeNode: siteNode, Name: "Site-Wide Playlist",
		Items: []datamodel.PlaylistItem{{Source: "asset", AssetRef: assetSite}},
	})
	createRow(t, s, store.KindSchedule, datamodel.Schedule{
		ID: siteSchedule, ScopeNode: siteNode, Name: "Site-Wide Base Schedule",
	})
	createRow(t, s, store.KindDaypart, datamodel.Daypart{
		ID: siteDaypart, ScheduleID: siteSchedule, ScopeNode: siteNode,
		DaysOfWeek: []int{0, 1, 2, 3, 4, 5, 6}, StartTime: "00:00:00", EndTime: "00:00:00",
		DisplayPower: "on", PlaylistID: sitePlaylist, Name: "Site All Day",
	})

	snap, _, err := BuildFromStore(desiredState(t, s), "https://origin.example", id, contentInstant(t), nil)
	if err != nil {
		t.Fatalf("BuildFromStore: %v", err)
	}

	onSite := programForScreen(t, snap.Sections.ScreenPrograms, siteScreen)
	if len(onSite.Content) != 1 || onSite.Content[0].AssetRef != assetSite {
		t.Fatalf("site-placed screen content = %+v, want the site schedule's asset %q", onSite.Content, assetSite)
	}

	// The seeded schedule lives on a DESCENDANT node, so it must not reach this
	// screen. Asserted by absence of its asset, not by the winner's identity.
	for _, c := range onSite.Content {
		if c.AssetRef == assetScreen {
			t.Errorf("a schedule attached BELOW the site reached a screen placed at the site (asset %q)", assetScreen)
		}
	}

	// The seeded screen, one level down, still sees its own nearer schedule win
	// over the site-wide one — the DAT-053 specificity key.
	seeded := programForScreen(t, snap.Sections.ScreenPrograms, store.SeedScreenID)
	if len(seeded.Content) != 1 || seeded.Content[0].AssetRef != assetScreen {
		t.Errorf("seeded screen content = %+v, want its own nearer schedule's asset %q", seeded.Content, assetScreen)
	}
}

// TestScreenWithNoApplicableScheduleGetsTheTerminalDefault: a screen row placed
// on a node no schedule is applicable to resolves to DAT-118's terminal default —
// display:blank, powered on, no content — and never to some invented state or to
// whatever another screen happens to be showing.
func TestScreenWithNoApplicableScheduleGetsTheTerminalDefault(t *testing.T) {
	id := testIdentity(t)
	const asset = "sha256:eeee111111111111111111111111111111111111111111111111111111111111"
	const (
		siteNode       = "01J8Z2Q1M8H8N4T0V1W2X3Y4Z5"
		unscheduledNod = "01J8ZN0SCHEDV1EN0DE0000010"
		unscheduledScr = "01J8ZN0SCHEDV1ESCREEN00010"
	)

	s := seededStore(t, asset)
	siteParent := siteNode
	createRow(t, s, store.KindScopeNode, datamodel.ScopeNode{
		ID: unscheduledNod, Kind: "screen", ParentID: &siteParent, Name: "Unscheduled Node",
	})
	createRow(t, s, store.KindScreen, datamodel.Screen{
		ID: unscheduledScr, ScopeNode: unscheduledNod, Name: "Unscheduled Screen",
	})

	snap, _, err := BuildFromStore(desiredState(t, s), "https://origin.example", id, contentInstant(t), nil)
	if err != nil {
		t.Fatalf("BuildFromStore: %v", err)
	}
	prog := programForScreen(t, snap.Sections.ScreenPrograms, unscheduledScr)

	if prog.Display != "blank" {
		t.Errorf("display = %q, want the DAT-118 terminal default blank", prog.Display)
	}
	if len(prog.Content) != 0 {
		t.Errorf("content = %+v, want none at the terminal default", prog.Content)
	}
	if prog.Priority != "scheduled" {
		t.Errorf("priority = %q, want scheduled", prog.Priority)
	}
	if prog.ProgramRevision == "" {
		t.Error("program_revision is empty at the terminal default; a defined state still needs a stated revision")
	}

	// REL-060: the empty content array marshals as [], never null.
	raw, err := json.Marshal(prog)
	if err != nil {
		t.Fatalf("marshal program: %v", err)
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal program: %v", err)
	}
	if string(decoded["content"]) != "[]" {
		t.Errorf("content marshaled as %s, want [] (REL-060: empty, never null)", decoded["content"])
	}

	// The screen the schedule DOES govern is unaffected — one screen resolving to
	// the terminal default says nothing about another.
	governed := programForScreen(t, snap.Sections.ScreenPrograms, store.SeedScreenID)
	if governed.Display != "content" {
		t.Errorf("the governed screen's display = %q, want content", governed.Display)
	}
}

// TestBlankDaypartYieldsABlankProgramAtTheSameGeneration: the SAME store, built
// at an instant inside the seeded overnight daypart, blanks the screen. This is
// the DAT-114 projection and the reason the derivation takes an instant at all —
// and it is the case the pre-derivation hardcoded entry got wrong every night.
func TestBlankDaypartYieldsABlankProgramAtTheSameGeneration(t *testing.T) {
	id := testIdentity(t)
	const asset = "sha256:ffff111111111111111111111111111111111111111111111111111111111111"
	s := seededStore(t, asset)
	ds := desiredState(t, s)

	day, _, err := BuildFromStore(ds, "https://origin.example", id, contentInstant(t), nil)
	if err != nil {
		t.Fatalf("BuildFromStore (day): %v", err)
	}
	night, _, err := BuildFromStore(ds, "https://origin.example", id, blankInstant(t), nil)
	if err != nil {
		t.Fatalf("BuildFromStore (night): %v", err)
	}

	dayProg := programForScreen(t, day.Sections.ScreenPrograms, store.SeedScreenID)
	nightProg := programForScreen(t, night.Sections.ScreenPrograms, store.SeedScreenID)

	if dayProg.Display != "content" || len(dayProg.Content) != 1 {
		t.Errorf("midday program = %+v, want display:content with the seeded asset", dayProg)
	}
	if nightProg.Display != "blank" {
		t.Errorf("overnight display = %q, want blank (DAT-114)", nightProg.Display)
	}
	if len(nightProg.Content) != 0 {
		t.Errorf("overnight content = %+v, want none — a blanked screen has nothing to fetch", nightProg.Content)
	}
	if dayProg.ProgramRevision == nightProg.ProgramRevision {
		t.Error("the blank and content programs share a program_revision; a player would never swap between them")
	}
}

// TestScreenWithUnresolvableTZIsOmittedNotSubstituted: a screen row placed under
// a node with no site-kind ancestor has no effective tz (DAT-034), so its program
// cannot be resolved. It must be OMITTED and REPORTED — never resolved against a
// substituted zone, and never silently dropped.
func TestScreenWithUnresolvableTZIsOmittedNotSubstituted(t *testing.T) {
	id := testIdentity(t)
	const asset = "sha256:9999111111111111111111111111111111111111111111111111111111111111"
	const (
		orgRoot   = "01J8Z0DEM00RGANCEST0RB0VND" // the seeded site's parent; carries no geo by rule
		orgScreen = "01J8Z0RGP1ACEDSCREEN000001"
	)

	s := seededStore(t, asset)
	// orgRoot is already a real row: the seed inserts the org root the site hangs
	// off (DAT-002 makes an unresolvable parent_id a violation in the store, which
	// holds the whole tree). An org node carries no geo by rule (DAT-032), so
	// placing a screen row directly on it is a chain that reaches no site.
	createRow(t, s, store.KindScreen, datamodel.Screen{
		ID: orgScreen, ScopeNode: orgRoot, Name: "Screen on the org root",
	})

	snap, degrades, err := BuildFromStore(desiredState(t, s), "https://origin.example", id, contentInstant(t), nil)
	if err != nil {
		t.Fatalf("BuildFromStore: %v", err)
	}

	for _, p := range snap.Sections.ScreenPrograms {
		if p.ScreenID == orgScreen {
			t.Fatalf("a screen with no resolvable tz was given a program anyway: %+v", p)
		}
	}
	var reported bool
	for _, e := range degrades {
		if e.Code == "EFFECTIVE_GEO_UNRESOLVED" {
			reported = true
		}
	}
	if !reported {
		t.Errorf("omitted screen was not reported; degrades = %+v", degrades)
	}

	// The resolvable screen is unaffected — one screen's degrade never blanks
	// another's program.
	governed := programForScreen(t, snap.Sections.ScreenPrograms, store.SeedScreenID)
	if governed.Display != "content" {
		t.Errorf("the resolvable screen's display = %q, want content", governed.Display)
	}
}

// TestStoreWithNoScreenRowsYieldsAnEmptySection: a deployment that has authored
// no screens delivers no programs — an EMPTY array, which REL-060 provides for,
// never a fabricated placeholder entry and never a null.
func TestStoreWithNoScreenRowsYieldsAnEmptySection(t *testing.T) {
	id := testIdentity(t)
	s, err := store.Open(":memory:", store.WallClockMs)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	snap, degrades, err := BuildFromStore(desiredState(t, s), "https://origin.example", id, contentInstant(t), nil)
	if err != nil {
		t.Fatalf("BuildFromStore: %v", err)
	}
	if len(degrades) != 0 {
		t.Errorf("degrades = %+v, want none for an empty store", degrades)
	}
	if len(snap.Sections.ScreenPrograms) != 0 {
		t.Fatalf("screen_programs = %+v, want none", snap.Sections.ScreenPrograms)
	}

	raw, err := json.Marshal(snap.Sections)
	if err != nil {
		t.Fatalf("marshal sections: %v", err)
	}
	var sections map[string]json.RawMessage
	if err := json.Unmarshal(raw, &sections); err != nil {
		t.Fatalf("unmarshal sections: %v", err)
	}
	if string(sections["screen_programs"]) != "[]" {
		t.Errorf("screen_programs marshaled as %s, want [] (REL-060)", sections["screen_programs"])
	}
}

// TestDerivedContentMatchesRelaySideProjection pins the two sides of the same
// answer together. internal/relay/schedulehost projects the same rows onto a
// player/1 Lease continuously, on the relay; this package projects them onto the
// REL-061 screen program the app peer signs. They read the same rows through the
// same engine, so at a shared instant they must agree on WHAT the screen plays:
// the same assets, in the same order, at the same URLs, with the same display.
//
// Without this, the two projections could drift apart silently — a screen would
// be delivered one thing in the signed baseline and another the moment the relay
// first ticked, with nothing failing.
func TestDerivedContentMatchesRelaySideProjection(t *testing.T) {
	id := testIdentity(t)
	const asset = "sha256:7777111111111111111111111111111111111111111111111111111111111111"
	const origin = "https://origin.example"
	s := seededStore(t, asset)
	ds := desiredState(t, s)
	at := contentInstant(t)

	snap, _, err := BuildFromStore(ds, origin, id, at, nil)
	if err != nil {
		t.Fatalf("BuildFromStore: %v", err)
	}
	derived := programForScreen(t, snap.Sections.ScreenPrograms, store.SeedScreenID)

	// The relay's own projection, over the schedule section this snapshot carries
	// — the exact bytes a relay would apply.
	rowStore, errs := schedulehost.BuildStore(snap.Sections.Schedule)
	if len(errs) != 0 {
		t.Fatalf("relay-side BuildStore reported %+v", errs)
	}
	display, priority, content, _, err := schedulehost.ProjectLease(rowStore, "01J8Z4DEM0SCREENF1RSTPH0TN", at, origin, nil)
	if err != nil {
		t.Fatalf("relay-side ProjectLease: %v", err)
	}

	if derived.Display != display {
		t.Errorf("display: app-derived %q != relay-projected %q", derived.Display, display)
	}
	if derived.Priority != priority {
		t.Errorf("priority: app-derived %q != relay-projected %q", derived.Priority, priority)
	}
	if len(derived.Content) != len(content) {
		t.Fatalf("content length: app-derived %d != relay-projected %d", len(derived.Content), len(content))
	}
	for i := range content {
		if derived.Content[i].AssetRef != content[i].AssetRef {
			t.Errorf("content[%d].asset_ref: app-derived %q != relay-projected %q", i, derived.Content[i].AssetRef, content[i].AssetRef)
		}
		if derived.Content[i].URL != content[i].URL {
			t.Errorf("content[%d].url: app-derived %q != relay-projected %q", i, derived.Content[i].URL, content[i].URL)
		}
		if derived.Content[i].DurationMS != content[i].DurationMS {
			t.Errorf("content[%d].duration_ms: app-derived %d != relay-projected %d", i, derived.Content[i].DurationMS, content[i].DurationMS)
		}
	}
}
