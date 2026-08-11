package store_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/maaxton/waiveo-next/internal/app/store"
	"github.com/maaxton/waiveo-next/internal/datamodel"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// priorfaults_test.go is the store's answer to a shape that has bricked it once
// and would have bricked it again: a validator added AFTER rows were already
// stored.
//
// The post-write validation re-runs over the whole row-set of the written kind.
// That is what makes a cross-row rule enforceable, and — until priorfaults.go —
// it also meant one row that no current validator accepts vetoed every write of
// its kind, DELETE included. A store in that state has no repair path through
// the API at all: recovery is raw SQL against the database file.
//
// The rows planted here are the real historical case. Inline `source: "slide"`
// playlist items shipped with no authoring validation, so every layer stack the
// gate now rejects was stored with a 201 — and a restore or a seed can deliver
// the same bundle today (internal/app/api/workspacerestore.go, SeedDemo).

const (
	plantedBadPlaylistA = "01J9F1BADS11DEP1AY11STAAAA"
	plantedBadPlaylistB = "01J9F2BADS11DEP1AY11STBBBB"
	innocentPlaylistID  = "01J9F3G00DP1AY11STNN0CENTX"
	freshPlaylistID     = "01J9F4G00DP1AY11STFRESHR0W"
)

// plantPlaylistRow writes a playlist body straight into the file, bypassing
// every validator the store interposes — the only way to forge a store as an
// older build left it, since the current build refuses to create one.
func plantPlaylistRow(t *testing.T, dsn, id, scopeNode string, body any) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal planted body: %v", err)
	}
	withRawDB(t, dsn, func(db *sql.DB) {
		if _, err := db.Exec(
			`INSERT INTO playlists (id, revision, external_id, labels, scope_node, created_at, updated_at, body)
			 VALUES (?, 1, '', '{}', ?, 1, 1, ?)`,
			id, scopeNode, string(raw)); err != nil {
			t.Fatalf("plant playlist %s: %v", id, err)
		}
	})
}

// badInlineSlidePlaylist is a playlist whose single item is a `source: "slide"`
// item with an EMPTY layer stack — one of the shapes
// wire.ValidateAuthoredSlideLayers refuses and the store once accepted.
func badInlineSlidePlaylist(id, scopeNode, name string) datamodel.Playlist {
	return datamodel.Playlist{
		ID: id, ScopeNode: scopeNode, Name: name, Revision: 1, CreatedAt: 1, UpdatedAt: 1,
		Items: []datamodel.PlaylistItem{{Source: "slide", Slide: &datamodel.Slide{}}},
	}
}

func goodPlaylist(id, scopeNode, name string) datamodel.Playlist {
	return datamodel.Playlist{
		ID: id, ScopeNode: scopeNode, Name: name,
		Items: []datamodel.PlaylistItem{{
			Source: "slide",
			Slide: &datamodel.Slide{Layers: []wire.Layer{
				{Kind: wire.LayerKindRect, X: 0, Y: 0, W: 1920, H: 1080, Color: "#101828"},
			}},
		}},
	}
}

// storeWithPlantedBadRows builds a file-backed store holding: an org node, one
// perfectly valid playlist, and TWO rows the current validators reject. Two,
// not one, because one planted row was enough to refuse a CREATE while DELETE
// still worked; it took two for DELETE to fail as well, and the delete is the
// property that matters most.
func storeWithPlantedBadRows(t *testing.T) (*store.Store, string) {
	t.Helper()
	ctx := context.Background()
	dsn := filepath.Join(t.TempDir(), "feeder-store.db")

	s, err := store.Open(dsn, store.WallClockMs)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	seedOrg(t, s)
	if _, err := s.Create(ctx, store.KindPlaylist,
		mustJSON(t, goodPlaylist(innocentPlaylistID, orgParentID, "Innocent Bystander"))); err != nil {
		t.Fatalf("create the innocent playlist: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	plantPlaylistRow(t, dsn, plantedBadPlaylistA, orgParentID, badInlineSlidePlaylist(plantedBadPlaylistA, orgParentID, "Legacy A"))
	plantPlaylistRow(t, dsn, plantedBadPlaylistB, orgParentID, badInlineSlidePlaylist(plantedBadPlaylistB, orgParentID, "Legacy B"))

	return openStoreAt(t, dsn), dsn
}

// TestAPlantedInvalidRowDoesNotVetoAnUnrelatedWrite is the whole finding, in the
// three operations that were all refused: CREATE, UPDATE and DELETE of rows the
// planted ones have nothing to do with.
//
// The refusal was not merely inconvenient, it was WRONG ON ITS FACE: it came
// back naming `items[0].slide.layers` of a body that contained no slide at all,
// because the field path is row-relative and the fault belonged to a different
// row entirely.
func TestAPlantedInvalidRowDoesNotVetoAnUnrelatedWrite(t *testing.T) {
	ctx := context.Background()
	s, _ := storeWithPlantedBadRows(t)

	// CREATE.
	fresh, err := s.Create(ctx, store.KindPlaylist,
		mustJSON(t, goodPlaylist(freshPlaylistID, orgParentID, "Authored Today")))
	if err != nil {
		t.Fatalf("CREATE of an unrelated, valid playlist = %v, want success — "+
			"a row already in the store must not veto a write that did not touch it", err)
	}

	// UPDATE.
	innocent, found, err := s.Get(ctx, store.KindPlaylist, innocentPlaylistID)
	if err != nil || !found {
		t.Fatalf("get the innocent playlist: %v (found=%v)", err, found)
	}
	if _, err := s.Update(ctx, store.KindPlaylist, innocent.ID, innocent.Revision,
		json.RawMessage(`{"name":"Renamed"}`)); err != nil {
		t.Fatalf("UPDATE of an unrelated playlist = %v, want success", err)
	}

	// DELETE — the repair path. With this refused there is no way back through
	// the API at all.
	if err := s.Delete(ctx, store.KindPlaylist, fresh.ID, fresh.Revision); err != nil {
		t.Fatalf("DELETE of an unrelated playlist = %v, want success — DELETE is the only repair path and must always be available", err)
	}
}

// TestAPlantedInvalidRowIsFixableThroughTheAPI is the other half, and the reason
// the faults are REPORTED rather than quarantined: an operator has to be able to
// reach the bad row and put it right, by either of the two routes.
func TestAPlantedInvalidRowIsFixableThroughTheAPI(t *testing.T) {
	ctx := context.Background()
	s, _ := storeWithPlantedBadRows(t)

	// Route 1: PATCH the offending member.
	bad, found, err := s.Get(ctx, store.KindPlaylist, plantedBadPlaylistA)
	if err != nil || !found {
		t.Fatalf("get planted row A: %v (found=%v)", err, found)
	}
	repaired := goodPlaylist(plantedBadPlaylistA, orgParentID, "Legacy A").Items
	if _, err := s.Update(ctx, store.KindPlaylist, bad.ID, bad.Revision,
		mustJSON(t, map[string]any{"items": repaired})); err != nil {
		t.Fatalf("PATCHing a planted row's items to a valid stack = %v, want success", err)
	}

	// Route 2: delete the other one outright.
	badB, found, err := s.Get(ctx, store.KindPlaylist, plantedBadPlaylistB)
	if err != nil || !found {
		t.Fatalf("get planted row B: %v (found=%v)", err, found)
	}
	if err := s.Delete(ctx, store.KindPlaylist, badB.ID, badB.Revision); err != nil {
		t.Fatalf("DELETING a planted bad row = %v, want success", err)
	}

	// And the store is clean afterwards: the repairs actually cleared the faults
	// rather than merely being tolerated alongside them.
	faults, err := s.StoredFaults(ctx)
	if err != nil {
		t.Fatalf("StoredFaults: %v", err)
	}
	if len(faults) != 0 {
		t.Fatalf("after repairing both planted rows the store still reports %+v, want none", faults)
	}
}

// TestPlantedFaultsAreReportedNotHidden: an inherited fault stops REFUSING
// writes, which is the fix — but it must not stop being VISIBLE, or the store
// drifts into a broken state nobody knows about. StoredFaults is the read that
// tells an operator which row to go fix.
func TestPlantedFaultsAreReportedNotHidden(t *testing.T) {
	ctx := context.Background()
	s, _ := storeWithPlantedBadRows(t)

	faults, err := s.StoredFaults(ctx)
	if err != nil {
		t.Fatalf("StoredFaults: %v", err)
	}
	var slideFaults int
	for _, f := range faults {
		if f.Code == "PLAYLIST_ITEM_SLIDE_LAYERS_INVALID" {
			slideFaults++
		}
	}
	if slideFaults != 2 {
		t.Fatalf("StoredFaults reported %d PLAYLIST_ITEM_SLIDE_LAYERS_INVALID (all: %+v), want 2 — one per planted row", slideFaults, faults)
	}
}

// TestTheDiffStillRefusesWhatAWriteBreaks is the control, and it is the half a
// too-eager fix loses. Subtracting the pre-existing faults must not turn the
// post-write validation off:
//
//   - a NEW row carrying the same kind of bad slide is refused, even though a
//     byte-identical fault is already standing on two other rows (the multiset
//     count goes up, so it is attributed to this write);
//   - a DELETE that would leave a dangling reference behind is still refused,
//     because that reference error is new.
func TestTheDiffStillRefusesWhatAWriteBreaks(t *testing.T) {
	ctx := context.Background()
	s, _ := storeWithPlantedBadRows(t)

	_, err := s.Create(ctx, store.KindPlaylist,
		mustJSON(t, badInlineSlidePlaylist(freshPlaylistID, orgParentID, "Authored Broken Today")))
	var verr *store.ValidationError
	if !errors.As(err, &verr) {
		t.Fatalf("CREATE of a playlist with an invalid inline slide = %v, want a *store.ValidationError — "+
			"a fault this write introduced is this write's, however many identical ones the store already carried", err)
	}
	var sawSlideCode bool
	for _, e := range verr.Errors {
		if e.Code == "PLAYLIST_ITEM_SLIDE_LAYERS_INVALID" {
			sawSlideCode = true
		}
	}
	if !sawSlideCode {
		t.Fatalf("refusal carried %+v, want PLAYLIST_ITEM_SLIDE_LAYERS_INVALID", verr.Errors)
	}

	// The cross-row half: a cast a playlist item names cannot be deleted, and
	// that rule lives entirely in the whole-row-set revalidation the diff is
	// applied to.
	castID := "01J9F5CASTREFERENCEDBYAP11"
	if _, err := s.Create(ctx, store.KindCast, mustJSON(t, datamodel.Cast{
		ID: castID, ScopeNode: orgParentID, Name: "Referenced",
		Slides: []datamodel.CastSlide{{ID: "s1", Layers: []wire.Layer{
			{Kind: wire.LayerKindRect, X: 0, Y: 0, W: 1920, H: 1080, Color: "#101828"},
		}}},
	})); err != nil {
		t.Fatalf("create the referenced cast: %v", err)
	}
	referencing := datamodel.Playlist{
		ID: freshPlaylistID, ScopeNode: orgParentID, Name: "Plays The Cast",
		Items: []datamodel.PlaylistItem{{Source: "cast", CastID: castID}},
	}
	if _, err := s.Create(ctx, store.KindPlaylist, mustJSON(t, referencing)); err != nil {
		t.Fatalf("create the referencing playlist: %v", err)
	}
	cast, found, err := s.Get(ctx, store.KindCast, castID)
	if err != nil || !found {
		t.Fatalf("get the cast: %v (found=%v)", err, found)
	}
	err = s.Delete(ctx, store.KindCast, cast.ID, cast.Revision)
	if !errors.As(err, &verr) {
		t.Fatalf("DELETE of a cast a playlist still names = %v, want a *store.ValidationError — "+
			"the diff must not turn the whole-row-set rules off, only stop blaming this write for other rows' faults", err)
	}
	if !strings.Contains(fmt.Sprint(verr.Errors), "REFERENCE_INVALID") {
		t.Fatalf("cast-delete refusal carried %+v, want REFERENCE_INVALID", verr.Errors)
	}
}
