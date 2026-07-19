package snapshot

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/maaxton/waiveo-next/internal/datamodel"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// buildDemoRowStore parses a built snapshot's carried `schedule` section
// (REL-065) through the exact two data-model/1 functions a relay's
// schedulehost (a later task) will call — datamodel.BuildScopeTree and
// datamodel.ValidateRows — asserting ZERO errors from either. The relay
// DERIVES (it does not re-implement, data-model/1 line 391), so the demo
// rows this package authors MUST be genuinely valid data-model/1 rows, not
// merely well-formed opaque JSON.
func buildDemoRowStore(t *testing.T, sec wire.ScheduleSection) datamodel.RowStore {
	t.Helper()

	var nodes []datamodel.ScopeNode
	for _, raw := range sec.ScopeNodes {
		var n datamodel.ScopeNode
		if err := json.Unmarshal(raw, &n); err != nil {
			t.Fatalf("unmarshal scope node: %v; raw=%s", err, raw)
		}
		nodes = append(nodes, n)
	}
	tree, treeErrs := datamodel.BuildScopeTree(nodes)
	if len(treeErrs) != 0 {
		t.Fatalf("BuildScopeTree returned errors, want zero: %+v", treeErrs)
	}

	raw := datamodel.RawRows{
		Playlists:       sec.Playlists,
		Schedules:       sec.Schedules,
		ValidityWindows: sec.ValidityWindows,
		Dayparts:        sec.Dayparts,
		Fallbacks:       sec.Fallbacks,
		PresetBatches:   sec.PresetBatches,
	}
	rows, rowErrs := datamodel.ValidateRows(raw)
	if len(rowErrs) != 0 {
		t.Fatalf("ValidateRows returned errors, want zero: %+v", rowErrs)
	}

	return datamodel.RowStore{Tree: tree, Rows: rows}
}

// demoInstant computes a deterministic Unix-ms instant at the given
// local hour/minute in the demo site's own effective tz (America/Chicago),
// on a fixed DST-quiet date (2026-01-15, deep winter) — never hard-coded
// UTC math that would silently drift if the fixture's tz ever changed.
func demoInstant(t *testing.T, hour, minute int) int64 {
	t.Helper()
	loc, err := time.LoadLocation(firstPhotonSiteEffective.TZ)
	if err != nil {
		t.Fatalf("load location %q: %v", firstPhotonSiteEffective.TZ, err)
	}
	return time.Date(2026, time.January, 15, hour, minute, 0, 0, loc).UnixMilli()
}

// TestBuildDemoScheduleValidatesCleanly asserts the built snapshot's
// carried schedule section (REL-065) parses through BuildScopeTree +
// ValidateRows with ZERO errors, and that its two dayparts do not overlap
// (DAT-073) — the demo schedule a relay's schedulehost resolves must be a
// genuinely valid data-model/1 row set.
func TestBuildDemoScheduleValidatesCleanly(t *testing.T) {
	img := loadTestImage(t)
	id := testIdentity(t)

	snap, err := Build(img, "https://origin.example", id, nil)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	store := buildDemoRowStore(t, snap.Sections.Schedule)

	if len(store.Rows.Dayparts) != 2 {
		t.Fatalf("len(Dayparts) = %d, want exactly 2 (a content daypart + a blank overnight daypart)", len(store.Rows.Dayparts))
	}
	if e := datamodel.ValidateNoOverlap(store.Rows.Dayparts); e != nil {
		t.Fatalf("the two demo dayparts overlap (DAT-073): %+v", e)
	}
}

// TestBuildDemoScheduleResolvesContentVsBlankByTimeOfDay asserts
// datamodel.Resolve varies display:content <-> display:blank across the
// demo schedule's two dayparts depending on time-of-day (DAT-111/113-115),
// and that the content daypart's resolved playlist is the demo playlist.
func TestBuildDemoScheduleResolvesContentVsBlankByTimeOfDay(t *testing.T) {
	img := loadTestImage(t)
	id := testIdentity(t)

	snap, err := Build(img, "https://origin.example", id, nil)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	store := buildDemoRowStore(t, snap.Sections.Schedule)

	midDay := demoInstant(t, 12, 0)
	state, err := datamodel.Resolve(store, demoScreenScopeNodeID, midDay)
	if err != nil {
		t.Fatalf("Resolve(mid-day): %v", err)
	}
	if state.Display != "content" {
		t.Errorf("mid-day Display = %q, want content", state.Display)
	}
	if state.PlaylistID != demoPlaylistID {
		t.Errorf("mid-day PlaylistID = %q, want %q", state.PlaylistID, demoPlaylistID)
	}

	overnight := demoInstant(t, 2, 0)
	state, err = datamodel.Resolve(store, demoScreenScopeNodeID, overnight)
	if err != nil {
		t.Fatalf("Resolve(overnight): %v", err)
	}
	if state.Display != "blank" {
		t.Errorf("overnight Display = %q, want blank", state.Display)
	}
	if state.PlaylistID != "" {
		t.Errorf("overnight PlaylistID = %q, want empty (blank projects no content)", state.PlaylistID)
	}
}

// TestBuildDemoScheduleContentAssetRefMatchesScreenProgram asserts the demo
// playlist's single item carries the SAME asset_ref the existing
// screen_programs item shows, so the schedule-driven content and the
// app-authored content agree for the demo.
func TestBuildDemoScheduleContentAssetRefMatchesScreenProgram(t *testing.T) {
	img := loadTestImage(t)
	id := testIdentity(t)

	snap, err := Build(img, "https://origin.example", id, nil)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	if len(snap.Sections.ScreenPrograms) != 1 || len(snap.Sections.ScreenPrograms[0].Content) != 1 {
		t.Fatalf("unexpected screen_programs shape: %+v", snap.Sections.ScreenPrograms)
	}
	wantAssetRef := snap.Sections.ScreenPrograms[0].Content[0].AssetRef

	store := buildDemoRowStore(t, snap.Sections.Schedule)
	var playlist *datamodel.Playlist
	for i := range store.Rows.Playlists {
		if store.Rows.Playlists[i].ID == demoPlaylistID {
			playlist = &store.Rows.Playlists[i]
		}
	}
	if playlist == nil {
		t.Fatalf("demo playlist %q not found in parsed RowSet", demoPlaylistID)
	}
	if len(playlist.Items) != 1 {
		t.Fatalf("len(playlist.Items) = %d, want 1", len(playlist.Items))
	}
	if playlist.Items[0].AssetRef != wantAssetRef {
		t.Errorf("demo playlist item AssetRef = %q, want %q (the screen_programs item's asset_ref)", playlist.Items[0].AssetRef, wantAssetRef)
	}
}

// TestBuildDemoScheduleRowsHaveNoPackOwnershipField asserts none of the
// authored rows carries a row-level pack-identifying field (DAT-101) —
// ValidateRows itself would have already rejected one, but this test names
// the requirement explicitly per the task's stated intent.
func TestBuildDemoScheduleRowsHaveNoPackOwnershipField(t *testing.T) {
	img := loadTestImage(t)
	id := testIdentity(t)

	snap, err := Build(img, "https://origin.example", id, nil)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	sec := snap.Sections.Schedule
	allRows := append([]json.RawMessage{}, sec.Playlists...)
	allRows = append(allRows, sec.Schedules...)
	allRows = append(allRows, sec.ValidityWindows...)
	allRows = append(allRows, sec.Dayparts...)
	allRows = append(allRows, sec.Fallbacks...)
	allRows = append(allRows, sec.PresetBatches...)

	for _, raw := range allRows {
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(raw, &obj); err != nil {
			t.Fatalf("unmarshal row: %v; raw=%s", err, raw)
		}
		if _, ok := obj["pack_id"]; ok {
			t.Errorf("row carries a top-level pack_id (DAT-101 forbidden): %s", raw)
		}
		if _, ok := obj["owner_pack"]; ok {
			t.Errorf("row carries a top-level owner_pack (DAT-101 forbidden): %s", raw)
		}
	}
}
