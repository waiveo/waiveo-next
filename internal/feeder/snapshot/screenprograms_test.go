package snapshot

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/maaxton/waiveo-next/internal/app/store"
	"github.com/maaxton/waiveo-next/internal/datamodel"
	"github.com/maaxton/waiveo-next/internal/feeder/contenturl"
	"github.com/maaxton/waiveo-next/internal/feeder/origin"
	"github.com/maaxton/waiveo-next/internal/relay/schedulehost"
	"github.com/maaxton/waiveo-next/internal/shared/signhash"
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

// wantSeededAssetThenSlide asserts a derived content array is exactly what the
// demo seed (store.SeedDemo) produces for a single-asset seed: the plain asset
// item first, then the seeded native slide (native slide rendering, parity
// milestone 2). The slide is validated at projection, so its presence here also
// proves the seeded stack passed wire.ValidateSlideLayers; its layers are
// asserted in depth by TestSeededDemoSlideValidatesAndDerives.
func wantSeededAssetThenSlide(t *testing.T, content []wire.ContentRef, assetRef string) {
	t.Helper()
	if len(content) != 2 {
		t.Fatalf("content = %+v, want the seeded asset then the seeded slide (2 items)", content)
	}
	if content[0].AssetRef != assetRef {
		t.Errorf("content[0].asset_ref = %q, want the seeded asset %q", content[0].AssetRef, assetRef)
	}
	if content[1].ContentType != "slide" || len(content[1].Layers) == 0 {
		t.Errorf("content[1] = %+v, want the seeded slide (content_type slide, non-empty layers)", content[1])
	}
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

	before, _, err := BuildFromStore(desiredState(t, s), contenturl.Signer{Base: "https://origin.example"}, id, contentInstant(t))
	if err != nil {
		t.Fatalf("BuildFromStore before the edit: %v", err)
	}
	beforeProgram := programForScreen(t, before.Sections.ScreenPrograms, store.SeedScreenID)
	wantSeededAssetThenSlide(t, beforeProgram.Content, originalAsset)

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

	after, _, err := BuildFromStore(desiredState(t, s), contenturl.Signer{Base: "https://origin.example"}, id, contentInstant(t))
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

	first, _, err := BuildFromStore(desiredState(t, s), contenturl.Signer{Base: "https://origin.example"}, id, contentInstant(t))
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

	second, _, err := BuildFromStore(desiredState(t, s), contenturl.Signer{Base: "https://origin.example"}, id, contentInstant(t))
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

	snap, degrades, err := BuildFromStore(desiredState(t, s), contenturl.Signer{Base: "https://origin.example"}, id, contentInstant(t))
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

	wantSeededAssetThenSlide(t, progA.Content, assetA) // A plays the seeded playlist (asset + the seeded slide)
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
	wantSeededAssetThenSlide(t, progMate.Content, assetA) // the node-mate resolves the same seeded playlist as A
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

	snap, _, err := BuildFromStore(desiredState(t, s), contenturl.Signer{Base: "https://origin.example"}, id, contentInstant(t))
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
	wantSeededAssetThenSlide(t, seeded.Content, assetScreen) // its own nearer schedule's seeded playlist
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

	snap, _, err := BuildFromStore(desiredState(t, s), contenturl.Signer{Base: "https://origin.example"}, id, contentInstant(t))
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

	day, _, err := BuildFromStore(ds, contenturl.Signer{Base: "https://origin.example"}, id, contentInstant(t))
	if err != nil {
		t.Fatalf("BuildFromStore (day): %v", err)
	}
	night, _, err := BuildFromStore(ds, contenturl.Signer{Base: "https://origin.example"}, id, blankInstant(t))
	if err != nil {
		t.Fatalf("BuildFromStore (night): %v", err)
	}

	dayProg := programForScreen(t, day.Sections.ScreenPrograms, store.SeedScreenID)
	nightProg := programForScreen(t, night.Sections.ScreenPrograms, store.SeedScreenID)

	if dayProg.Display != "content" {
		t.Errorf("midday program = %+v, want display:content with the seeded content", dayProg)
	}
	wantSeededAssetThenSlide(t, dayProg.Content, asset)
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

	snap, degrades, err := BuildFromStore(desiredState(t, s), contenturl.Signer{Base: "https://origin.example"}, id, contentInstant(t))
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

	snap, degrades, err := BuildFromStore(desiredState(t, s), contenturl.Signer{Base: "https://origin.example"}, id, contentInstant(t))
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
//
// It runs over two stores rather than one. The seeded demo covers the plain
// asset and inline-slide items; the cast case covers the one item shape whose
// projection is not one-to-one — a `source:"cast"` item fans out into one slide
// content item per slide (DAT-043), so the two sides must agree not only on each
// item's fields but on HOW MANY items one authored entry became and in what
// order. A cast is also the only shape whose expansion depends on a row carried
// separately in the schedule section, which is the part a feeder that
// pre-flattened its casts would break.
//
// # And it runs over BOTH signing postures
//
// Every case runs twice: against an origin holding no key, and against one that
// signs. That is not thoroughness, it is the whole validity of the comparison.
// When content URLs became signed, every caller here was migrated to a
// contenturl.Signer with NO KEY — so the byte-for-byte url equality below went
// on holding for a configuration nothing runs in (cmd/waiveo-feeder
// loads-or-creates the key unconditionally), which is verbatim the failure mode
// this branch's own api test file was written to indict. Under a key the two
// sides legitimately differ in their query — two independent mints, each with
// its own deadline — so the comparison itself had to change too: it compares
// what is fetched and from where, and then FETCHES BOTH at the real origin.
func TestDerivedContentMatchesRelaySideProjection(t *testing.T) {
	img := loadTestImage(t)
	asset := signhash.ContentID(img)
	// Real bytes behind a real digest, because both sides' urls are fetched from
	// a real origin below; a literal hex digest names bytes no origin can hold.
	forEachSigningPosture(t, img, func(t *testing.T, oc *origin.Store) {
		t.Run("seeded demo", func(t *testing.T) {
			assertProjectionsAgree(t, seededStore(t, asset), oc)
		})
		t.Run("playlist referencing a cast", func(t *testing.T) {
			s := seededStore(t, asset)
			castID := writeCast(t, s, castWithThreeSlides(asset))
			replaceSeedPlaylistItems(t, s, []datamodel.PlaylistItem{
				{Source: "asset", AssetRef: asset},
				{Source: datamodel.PlaylistSourceCast, CastID: castID, DurationSeconds: intPtr(11)},
			})
			// The fan-out itself, asserted here rather than left implicit in the
			// two-sided comparison: an agreement on zero content items would pass a
			// pure equality check while proving nothing about casts.
			prog := programForScreen(t, buildSnapshot(t, s).Sections.ScreenPrograms, store.SeedScreenID)
			if len(prog.Content) != 4 {
				t.Fatalf("content = %d items, want 4 (one asset + the cast's three slides); got %+v", len(prog.Content), prog.Content)
			}
			assertProjectionsAgree(t, s, oc)
		})
		// The video shapes, added because the two sides resolve an item's TYPE
		// differently by construction (the app side carries the authored value, the
		// relay side substitutes the required default) and because a video layer's
		// url is derived by a second, independent copy of the same rule. Both are
		// exactly the kind of near-duplicate that agrees on the day it is written;
		// without a case that carries a video, the type and layer comparisons above
		// only ever see items where the two sides trivially match.
		t.Run("a scheduled video and a slide carrying one", func(t *testing.T) {
			s := seededStore(t, asset)
			castID := writeCast(t, s, datamodel.Cast{
				ID: "01J8ZCASTPAR1TYV1DE0000001", ScopeNode: castScopeNode, Name: "Promo",
				Slides: []datamodel.CastSlide{{ID: "promo", DurationMS: 12000, Layers: []wire.Layer{
					{Kind: wire.LayerKindVideo, X: 0, Y: 0, W: 1920, H: 1080, AssetRef: asset},
				}}},
			})
			replaceSeedPlaylistItems(t, s, []datamodel.PlaylistItem{
				{Source: datamodel.PlaylistSourceAsset, AssetRef: asset, ContentType: datamodel.PlaylistContentTypeVideo},
				{Source: datamodel.PlaylistSourceAsset, AssetRef: asset},
				{Source: datamodel.PlaylistSourceCast, CastID: castID},
			})
			// The premise, asserted rather than assumed: the app side really did
			// produce a video item and a surviving slide, so the comparison below is
			// comparing something.
			prog := programForScreen(t, buildSnapshot(t, s).Sections.ScreenPrograms, store.SeedScreenID)
			if len(prog.Content) != 3 {
				t.Fatalf("content = %d items, want 3 (video asset + image asset + the cast's one slide); got %+v", len(prog.Content), prog.Content)
			}
			if prog.Content[0].ContentType != datamodel.PlaylistContentTypeVideo {
				t.Fatalf("content[0].content_type = %q, want %q", prog.Content[0].ContentType, datamodel.PlaylistContentTypeVideo)
			}
			assertProjectionsAgree(t, s, oc)
		})
	})
}

// forEachSigningPosture runs body twice — once against a content origin holding
// NO key and once against one that signs — with img already stored in each, so a
// url either side mints is genuinely fetchable there.
//
// Both postures, because a guard that only holds in one is a guard for a
// deployment that may not exist. The keyless one is the one the fixtures were
// silently migrated to and is a real configuration (an operator who sets no
// key); the signing one is what cmd/waiveo-feeder always builds.
func forEachSigningPosture(t *testing.T, img []byte, body func(t *testing.T, oc *origin.Store)) {
	t.Helper()
	at := contentInstant(t)
	for _, posture := range []struct {
		name string
		key  []byte
	}{
		{"origin holds no key", nil},
		{"origin signs", []byte("a-32-byte-test-key-for-hmac-0001")},
	} {
		t.Run(posture.name, func(t *testing.T) {
			opts := []origin.Option{origin.WithClock(func() int64 { return at })}
			if posture.key != nil {
				opts = append(opts, origin.WithSigningKey(posture.key))
			}
			oc := origin.New(opts...)
			if _, err := oc.Add(img); err != nil {
				t.Fatalf("origin.Add: %v", err)
			}
			body(t, oc)
		})
	}
}

// parityOrigin is the content origin both sides of the parity comparison derive
// their URLs from. It has to be ONE value: the whole claim is that two
// projections handed the same origin produce the same URLs.
const parityOrigin = "https://origin.example"

// buildSnapshot builds the signed snapshot for s at the seeded content instant.
func buildSnapshot(t *testing.T, s *store.Store) SignedSnapshot {
	t.Helper()
	snap, _, err := BuildFromStore(desiredState(t, s), contenturl.Signer{Base: parityOrigin}, testIdentity(t), contentInstant(t))
	if err != nil {
		t.Fatalf("BuildFromStore: %v", err)
	}
	return snap
}

// assertProjectionsAgree is the comparison itself: build the app-signed program
// for the seeded screen, re-project the SAME snapshot's schedule section through
// the relay's own resolver at the same instant, and require the two to describe
// the same playback — against the content origin oc, under whatever signing
// posture it holds.
//
// # What "the same url" means once urls are signed
//
// Not "the same bytes". Each side mints independently, and a signed url carries
// its own `exp` and `sig`; requiring those to match would be requiring the two
// producers to agree on a deadline, which is neither true nor desirable (the
// relay's serve-time mint is measured from when it serves). What must agree is
// WHAT IS FETCHED AND FROM WHERE — origin and content-addressed path — which is
// exactly the reduction program_revision already digests (withoutMintedQuery).
//
// Equality of that reduction is necessary and not sufficient, so both urls are
// then FETCHED at oc's real handler and required to serve the right bytes. That
// is what makes the comparison mean something under a key: two urls can share a
// path and differ in whether either of them actually works.
func assertProjectionsAgree(t *testing.T, s *store.Store, oc *origin.Store) {
	t.Helper()
	id := testIdentity(t)
	const base = parityOrigin
	ds := desiredState(t, s)
	at := contentInstant(t)

	sign := oc.Signer(base, contenturl.SnapshotTTL)
	snap, _, err := BuildFromStore(ds, sign, id, at)
	if err != nil {
		t.Fatalf("BuildFromStore: %v", err)
	}
	derived := programForScreen(t, snap.Sections.ScreenPrograms, store.SeedScreenID)

	// The relay's own projection, over the schedule section this snapshot carries
	// — the exact bytes a relay would apply, under THE SAME key the origin holds
	// (REL-066a delivers it to the relay in revocation_and_site). Handing it a nil
	// key against a signing origin would compare a working url with a broken one
	// and call the difference expected.
	rowStore, errs := schedulehost.BuildStore(snap.Sections.Schedule)
	if len(errs) != 0 {
		t.Fatalf("relay-side BuildStore reported %+v", errs)
	}
	display, priority, content, _, err := schedulehost.ProjectLease(rowStore, "01J8Z4DEM0SCREENF1RSTPH0TN", at, base, sign.Key)
	if err != nil {
		t.Fatalf("relay-side ProjectLease: %v", err)
	}

	// The premise: this fixture really is exercising the posture it claims. A
	// keyless run that quietly signed, or a signing run that quietly did not,
	// would make every assertion below true of the wrong configuration — which is
	// how the byte-for-byte comparison this replaced came to hold only for a
	// deployment nothing runs.
	assertSigningPosture(t, derived, sign.Signs())

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
		// Compared through the reduction, then PROVEN by fetching: see this
		// function's own doc for why the raw bytes must not be required to match
		// once each side mints its own deadline.
		if a, b := withoutMintedQuery(derived.Content[i].URL), withoutMintedQuery(content[i].URL); a != b {
			t.Errorf("content[%d].url: app-derived %q != relay-projected %q (queries aside) — the two projections "+
				"disagree about WHICH bytes this item is, or about which origin they come from", i, a, b)
		}
		assertFetchable(t, oc, derived.Content[i].URL, "app-derived content[%d].url", i)
		assertFetchable(t, oc, content[i].URL, "relay-projected content[%d].url", i)
		if derived.Content[i].DurationMS != content[i].DurationMS {
			t.Errorf("content[%d].duration_ms: app-derived %d != relay-projected %d", i, derived.Content[i].DurationMS, content[i].DurationMS)
		}
		// The item's TYPE is what a player switches its renderer on, so the two
		// sides disagreeing about it is a screen that shows a video at boot and
		// a still frame the moment a daypart boundary makes the relay
		// re-resolve — or the reverse. Compared through the REL-061a default
		// (an absent content_type means `image`) because the two sides
		// legitimately spell that default differently and must not be forced to
		// agree on the spelling: a REL-061 ContentRef's content_type is
		// optional, so the app side carries the authored value verbatim
		// (including empty), while a Lease's `type` is required, so the relay
		// side substitutes. What must agree is the RESOLVED type, which is what
		// this compares.
		if got, want := leaseContentTypeOf(content[i].Type), leaseContentTypeOf(derived.Content[i].ContentType); got != want {
			t.Errorf("content[%d].type: app-derived %q != relay-projected %q", i, want, got)
		}
		// A slide item's substance is its layers, not an asset_ref/url — so the
		// two projections must agree on the whole layer stack too (the seeded
		// demo's last item is a slide, native slide rendering, parity milestone
		// 2). Both derive content-layer URLs from the same origin through the same
		// wire.ValidateSlideLayers gate, so a mismatch here is exactly the silent
		// drift this test exists to catch.
		if !reflect.DeepEqual(layersWithoutMintedQuery(derived.Content[i].Layers), layersWithoutMintedQuery(content[i].Layers)) {
			t.Errorf("content[%d].layers: app-derived %+v != relay-projected %+v", i, derived.Content[i].Layers, content[i].Layers)
		}
		for j := range derived.Content[i].Layers {
			if !wire.LayerFetchesContent(derived.Content[i].Layers[j].Kind) {
				continue
			}
			assertFetchable(t, oc, derived.Content[i].Layers[j].URL, "app-derived content[%d].layers[%d].url", i, j)
			assertFetchable(t, oc, content[i].Layers[j].URL, "relay-projected content[%d].layers[%d].url", i, j)
		}
	}
}

// layersWithoutMintedQuery reduces a layer stack the way revisionContent does,
// so two independently-minted stacks compare on everything BUT the deadline each
// side chose.
func layersWithoutMintedQuery(layers []wire.Layer) []wire.Layer {
	out := make([]wire.Layer, len(layers))
	for i, l := range layers {
		if wire.LayerFetchesContent(l.Kind) {
			l.URL = withoutMintedQuery(l.URL)
		}
		out[i] = l
	}
	return out
}

// assertSigningPosture requires that the urls a program carries are signed
// exactly when the origin they were minted from holds a key.
func assertSigningPosture(t *testing.T, prog wire.ScreenProgram, wantSigned bool) {
	t.Helper()
	for _, c := range prog.Content {
		urls := []string{c.URL}
		for _, l := range c.Layers {
			if wire.LayerFetchesContent(l.Kind) {
				urls = append(urls, l.URL)
			}
		}
		for _, u := range urls {
			if u == "" {
				continue
			}
			if signed := strings.Contains(u, "?"); signed != wantSigned {
				t.Fatalf("PREMISE FALSE: url %q is signed=%v, want %v — this case is not exercising the posture it "+
					"names, so the parity assertions below describe a configuration nobody runs", u, signed, wantSigned)
			}
		}
	}
}

// assertFetchable requires that rawURL is stated against the parity origin and
// serves 200 there. A url the two projections agree on and neither can fetch is
// agreement about a broken answer — which is the whole of HV-1.
func assertFetchable(t *testing.T, oc *origin.Store, rawURL, whatFmt string, args ...any) {
	t.Helper()
	what := fmt.Sprintf(whatFmt, args...)
	if rawURL == "" {
		return // a slide item carries no item-level url; the layers below are its substance.
	}
	if code, _ := fetchAtOrigin(t, oc, parityOrigin, rawURL); code != http.StatusOK {
		t.Errorf("%s (%q) answered %d at the content origin — the two projections agree on a url the origin refuses, "+
			"which is exactly the shape HV-1 had", what, rawURL, code)
	}
}

// leaseContentTypeOf resolves a content item's type through REL-061a's stated
// default, so the two projections' equivalent-but-differently-spelled answers
// for an ordinary image item compare equal while a genuine disagreement (image
// versus video) still fails.
func leaseContentTypeOf(contentType string) string {
	if contentType == "" {
		return "image"
	}
	return contentType
}

// TestSlidePlaylistItemDerivesToASlideContentRef proves the producer side of the
// native slide type end to end (native slide rendering, parity milestone 2): an
// authored `source:"slide"` playlist item, resolved through the store and the
// snapshot builder, becomes a REL-061 content ref of content_type "slide"
// carrying its layer stack — the image layer's fetch URL derived from the content
// origin (DAT-041), the whole stack accepted by wire.ValidateSlideLayers, and no
// item-level asset_ref/url invented for a slide.
func TestSlidePlaylistItemDerivesToASlideContentRef(t *testing.T) {
	ctx := context.Background()
	id := testIdentity(t)
	const asset = "sha256:51de51de51de51de51de51de51de51de51de51de51de51de51de51de51de51de"
	s := seededStore(t, asset)

	// Replace the seeded playlist with a single authored slide — the four v1
	// layer kinds, all inside the 1920x1080 canvas, the image reusing the seeded
	// asset (so it resolves in the origin exactly as a plain asset item does).
	pls, err := s.List(ctx, store.KindPlaylist, store.ListFilter{})
	if err != nil || len(pls) != 1 {
		t.Fatalf("list playlists: %v (got %d)", err, len(pls))
	}
	authored := []wire.Layer{
		{Kind: wire.LayerKindRect, X: 0, Y: 0, W: 1920, H: 1080, Color: "#101828"},
		{Kind: wire.LayerKindText, X: 100, Y: 80, W: 800, H: 120, Text: "Hello", FontPx: 96, Color: "#FFFFFF", Align: "left"},
		{Kind: wire.LayerKindImage, X: 200, Y: 300, W: 600, H: 400, AssetRef: asset},
		{Kind: wire.LayerKindClock, X: 1500, Y: 40, W: 360, H: 100, Text: "15:04:05", FontPx: 72},
	}
	edited := datamodel.Playlist{
		ID: pls[0].ID, ScopeNode: "01J8Z4DEM0SCREENF1RSTPH0TN", Name: "Slide Playlist",
		Items: []datamodel.PlaylistItem{{Source: "slide", Slide: &datamodel.Slide{Layers: authored}}},
	}
	body, err := json.Marshal(edited)
	if err != nil {
		t.Fatalf("marshal edited playlist: %v", err)
	}
	if _, err := s.Update(ctx, store.KindPlaylist, pls[0].ID, pls[0].Revision, body); err != nil {
		t.Fatalf("update playlist to a slide: %v", err)
	}

	snap, _, err := BuildFromStore(desiredState(t, s), contenturl.Signer{Base: "https://origin.example"}, id, contentInstant(t))
	if err != nil {
		t.Fatalf("BuildFromStore: %v", err)
	}
	prog := programForScreen(t, snap.Sections.ScreenPrograms, store.SeedScreenID)
	if len(prog.Content) != 1 {
		t.Fatalf("content = %+v, want the one authored slide", prog.Content)
	}
	ref := prog.Content[0]
	if ref.ContentType != "slide" {
		t.Errorf("content_type = %q, want slide", ref.ContentType)
	}
	if ref.AssetRef != "" || ref.URL != "" {
		t.Errorf("slide ref carries item-level asset_ref=%q url=%q, want neither (a slide's content is its layers)", ref.AssetRef, ref.URL)
	}
	if err := wire.ValidateSlideLayers(ref.Layers); err != nil {
		t.Errorf("derived slide layers do not validate: %v", err)
	}
	if len(ref.Layers) != len(authored) {
		t.Fatalf("derived %d layers, want %d", len(ref.Layers), len(authored))
	}
	img := ref.Layers[2]
	if img.Kind != wire.LayerKindImage || img.AssetRef != asset {
		t.Errorf("layer[2] = %+v, want the authored image layer referencing %q", img, asset)
	}
	wantURL := "https://origin.example/content/" + asset[len("sha256:"):]
	if img.URL != wantURL {
		t.Errorf("image layer url = %q, want the origin-derived %q", img.URL, wantURL)
	}
	if ref.Layers[1].Text != "Hello" || ref.Layers[1].Align != "left" || ref.Layers[3].Kind != wire.LayerKindClock || ref.Layers[3].Text != "15:04:05" {
		t.Errorf("text/clock layers did not survive derivation: %+v", ref.Layers)
	}
}

// TestSeededDemoSlideValidatesAndDerives pins the make-dev proof slice: the demo
// the feeder seeds (store.SeedDemo) carries one native slide, and it derives to a
// valid slide content ref — the four v1 kinds present, its image layer reusing
// the seeded asset with an origin-derived URL, the whole stack passing
// wire.ValidateSlideLayers. This is what makes a fresh box actually show a slide.
func TestSeededDemoSlideValidatesAndDerives(t *testing.T) {
	id := testIdentity(t)
	const asset = "sha256:5eed5eed5eed5eed5eed5eed5eed5eed5eed5eed5eed5eed5eed5eed5eed5eed5e"
	s := seededStore(t, asset)

	snap, _, err := BuildFromStore(desiredState(t, s), contenturl.Signer{Base: "https://origin.example"}, id, contentInstant(t))
	if err != nil {
		t.Fatalf("BuildFromStore: %v", err)
	}
	prog := programForScreen(t, snap.Sections.ScreenPrograms, store.SeedScreenID)
	if len(prog.Content) == 0 {
		t.Fatalf("seeded program has no content")
	}
	slide := prog.Content[len(prog.Content)-1] // the seed appends the slide last
	if slide.ContentType != "slide" {
		t.Fatalf("last content ref content_type = %q, want the seeded slide", slide.ContentType)
	}
	if err := wire.ValidateSlideLayers(slide.Layers); err != nil {
		t.Fatalf("seeded slide layers do not validate: %v", err)
	}

	kinds := map[string]bool{}
	var image wire.Layer
	for _, l := range slide.Layers {
		kinds[l.Kind] = true
		if l.Kind == wire.LayerKindImage {
			image = l
		}
	}
	for _, want := range []string{wire.LayerKindRect, wire.LayerKindText, wire.LayerKindImage, wire.LayerKindClock} {
		if !kinds[want] {
			t.Errorf("seeded slide is missing a %q layer; got kinds %v", want, kinds)
		}
	}
	if image.AssetRef != asset {
		t.Errorf("seeded slide image asset_ref = %q, want the reused seeded asset %q", image.AssetRef, asset)
	}
	if want := "https://origin.example/content/" + asset[len("sha256:"):]; image.URL != want {
		t.Errorf("seeded slide image url = %q, want the origin-derived %q", image.URL, want)
	}
}

// TestInvalidSlideItemIsSkipped: a slide whose layers do not pass
// wire.ValidateSlideLayers is DROPPED from the derived program, never emitted
// malformed — the producer half of the same refuse-don't-serve discipline the
// relay applies. A valid asset item alongside it is unaffected.
func TestInvalidSlideItemIsSkipped(t *testing.T) {
	ctx := context.Background()
	id := testIdentity(t)
	const asset = "sha256:badc0debadc0debadc0debadc0debadc0debadc0debadc0debadc0debadc0deba"
	s := seededStore(t, asset)

	pls, err := s.List(ctx, store.KindPlaylist, store.ListFilter{})
	if err != nil || len(pls) != 1 {
		t.Fatalf("list playlists: %v (got %d)", err, len(pls))
	}
	// A rect whose far edge runs past the canvas width — ValidateSlideLayers
	// rejects it, so this slide item must not survive derivation.
	badSlide := &datamodel.Slide{Layers: []wire.Layer{
		{Kind: wire.LayerKindRect, X: 1900, Y: 0, W: 100, H: 100, Color: "#ffffff"},
	}}
	edited := datamodel.Playlist{
		ID: pls[0].ID, ScopeNode: "01J8Z4DEM0SCREENF1RSTPH0TN", Name: "Playlist With A Bad Slide",
		Items: []datamodel.PlaylistItem{
			{Source: "asset", AssetRef: asset},
			{Source: "slide", Slide: badSlide},
		},
	}
	body, err := json.Marshal(edited)
	if err != nil {
		t.Fatalf("marshal playlist: %v", err)
	}
	if _, err := s.Update(ctx, store.KindPlaylist, pls[0].ID, pls[0].Revision, body); err != nil {
		t.Fatalf("update playlist: %v", err)
	}

	snap, _, err := BuildFromStore(desiredState(t, s), contenturl.Signer{Base: "https://origin.example"}, id, contentInstant(t))
	if err != nil {
		t.Fatalf("BuildFromStore: %v", err)
	}
	prog := programForScreen(t, snap.Sections.ScreenPrograms, store.SeedScreenID)
	if len(prog.Content) != 1 {
		t.Fatalf("content = %+v, want only the valid asset item (the invalid slide dropped)", prog.Content)
	}
	if prog.Content[0].AssetRef != asset {
		t.Errorf("surviving item = %+v, want the valid asset %q", prog.Content[0], asset)
	}
	for _, c := range prog.Content {
		if c.ContentType == "slide" {
			t.Errorf("an invalid slide was emitted anyway: %+v", c)
		}
	}
}
