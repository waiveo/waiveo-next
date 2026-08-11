package snapshot

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/maaxton/waiveo-next/internal/app/store"
	"github.com/maaxton/waiveo-next/internal/datamodel"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// casts_test.go drives the cast side of this package's content projection: a
// playlist item that REFERENCES a cast (data-model/1 DAT-043) expands into one
// REL-061 slide content reference per slide of that cast, in authored order.
//
// The fan-out is what makes a cast worth testing separately from the inline
// `slide` item beside it. Every other playlist item projects to exactly one
// content reference, so an off-by-one or a lost order is invisible in the
// aggregate; a cast is the one shape where "the projection ran" and "the
// projection produced the right playback" are different claims.

// castScopeNode is the node every cast these tests write is placed at — the same
// node the demo seed places its playlist and screen at, because a cast's
// placement has to resolve in the seeded tree for the store to accept it.
const castScopeNode = "01J8Z4DEM0SCREENF1RSTPH0TN"

// castID is the id every cast these tests write carries. A fixed ULID rather
// than a minted one keeps the playlist item that references it writable in the
// same literal.
const castID = "01J8ZCAST00000000000000001"

// intPtr is the addressable-int helper a playlist item's optional
// `duration_seconds` override needs.
func intPtr(v int) *int { return &v }

// castWithThreeSlides is the fixture cast: three slides whose per-slide dwell
// times deliberately differ — one stated, one stated differently, one absent —
// so a projection that dropped the per-slide duration, or applied one slide's to
// all three, is visible rather than merely plausible. The middle slide's image
// layer reuses the seeded asset so it resolves in the content origin exactly as
// a plain asset item does (DAT-041), and carries NO url: that is derived at
// projection time, which is the behaviour under test.
func castWithThreeSlides(assetRef string) datamodel.Cast {
	return datamodel.Cast{
		ID: castID, ScopeNode: castScopeNode, Name: "Lunch Menu",
		Slides: []datamodel.CastSlide{
			{ID: "title", DurationMS: 4000, Layers: []wire.Layer{
				{Kind: wire.LayerKindRect, X: 0, Y: 0, W: 1920, H: 1080, Color: "#101828"},
				{Kind: wire.LayerKindText, X: 120, Y: 100, W: 1000, H: 140, Text: "Lunch", FontPx: 96, Color: "#FFFFFF", Align: "left"},
			}},
			{ID: "photo", DurationMS: 9000, Layers: []wire.Layer{
				{Kind: wire.LayerKindImage, X: 200, Y: 200, W: 800, H: 600, AssetRef: assetRef},
			}},
			// No duration of its own: it inherits the referencing playlist
			// item's `duration_seconds` override (DAT-042).
			{ID: "clock", Layers: []wire.Layer{
				{Kind: wire.LayerKindClock, X: 1400, Y: 60, W: 460, H: 120, Text: "15:04:05", FontPx: 72, Color: "#F0F0F0"},
			}},
		},
	}
}

// writeCast stores one cast row through the real store write path — validation,
// referential integrity and all — and returns its id.
func writeCast(t *testing.T, s *store.Store, cast datamodel.Cast) string {
	t.Helper()
	body, err := json.Marshal(cast)
	if err != nil {
		t.Fatalf("marshal cast: %v", err)
	}
	res, err := s.Create(context.Background(), store.KindCast, body)
	if err != nil {
		t.Fatalf("create cast: %v", err)
	}
	return res.ID
}

// replaceSeedPlaylistItems rewrites the seeded demo playlist's items, so a test
// states exactly what the demo screen plays without seeding a second schedule.
func replaceSeedPlaylistItems(t *testing.T, s *store.Store, items []datamodel.PlaylistItem) {
	t.Helper()
	ctx := context.Background()
	pls, err := s.List(ctx, store.KindPlaylist, store.ListFilter{})
	if err != nil || len(pls) != 1 {
		t.Fatalf("list playlists: %v (got %d)", err, len(pls))
	}
	patch, err := json.Marshal(map[string]any{"items": items})
	if err != nil {
		t.Fatalf("marshal playlist items: %v", err)
	}
	if _, err := s.Update(ctx, store.KindPlaylist, pls[0].ID, pls[0].Revision, patch); err != nil {
		t.Fatalf("update playlist items: %v", err)
	}
}

// TestCastPlaylistItemExpandsToOneSlideRefPerSlide is the app-side projection of
// a cast reference, end to end: authored rows in the real store, through the
// real snapshot build, to the REL-061 content array a screen is handed.
//
// It asserts the four things a cast expansion can get wrong independently —
// how many items, in what order, with which layers, and at which dwell times —
// rather than only that a slide came out.
func TestCastPlaylistItemExpandsToOneSlideRefPerSlide(t *testing.T) {
	const asset = "sha256:c0stc0stc0stc0stc0stc0stc0stc0stc0stc0stc0stc0stc0stc0stc0stc0st"
	s := seededStore(t, asset)
	id := writeCast(t, s, castWithThreeSlides(asset))
	replaceSeedPlaylistItems(t, s, []datamodel.PlaylistItem{
		{Source: datamodel.PlaylistSourceCast, CastID: id, DurationSeconds: intPtr(7)},
	})

	prog := programForScreen(t, buildSnapshot(t, s).Sections.ScreenPrograms, store.SeedScreenID)
	if len(prog.Content) != 3 {
		t.Fatalf("content = %d items, want one per cast slide; got %+v", len(prog.Content), prog.Content)
	}

	// Order and identity: slide 1 is the title (a rect under a text), slide 2 the
	// image, slide 3 the clock. Reading the FIRST layer's kind off each is enough
	// to tell them apart and would catch a reversal or a duplicate.
	wantFirstKinds := []string{wire.LayerKindRect, wire.LayerKindImage, wire.LayerKindClock}
	// Durations: the two slides that state one keep it; the third inherits the
	// playlist item's 7s override (DAT-042), which is what proves the fallback is
	// per-slide rather than applied to the whole cast.
	wantDurations := []int64{4000, 9000, 7000}

	for i, ref := range prog.Content {
		if ref.ContentType != "slide" {
			t.Errorf("content[%d].content_type = %q, want slide", i, ref.ContentType)
		}
		if ref.AssetRef != "" || ref.URL != "" {
			t.Errorf("content[%d] carries item-level asset_ref=%q url=%q, want neither (a slide's content is its layers)", i, ref.AssetRef, ref.URL)
		}
		if len(ref.Layers) == 0 {
			t.Fatalf("content[%d] carries no layers", i)
		}
		if got := ref.Layers[0].Kind; got != wantFirstKinds[i] {
			t.Errorf("content[%d].layers[0].kind = %q, want %q (slides out of authored order?)", i, got, wantFirstKinds[i])
		}
		if ref.DurationMS != wantDurations[i] {
			t.Errorf("content[%d].duration_ms = %d, want %d", i, ref.DurationMS, wantDurations[i])
		}
		if err := wire.ValidateSlideLayers(ref.Layers); err != nil {
			t.Errorf("content[%d] layers do not validate as served: %v", i, err)
		}
	}

	// The image layer's fetch URL is DERIVED here, never authored — the same
	// `<base>/content/<hex>` grammar a plain asset item gets.
	img := prog.Content[1].Layers[0]
	wantURL := parityOrigin + "/content/" + asset[len("sha256:"):]
	if img.URL != wantURL {
		t.Errorf("image layer url = %q, want %q (derived from the content origin)", img.URL, wantURL)
	}
	if img.AssetRef != asset {
		t.Errorf("image layer asset_ref = %q, want the authored %q", img.AssetRef, asset)
	}
}

// TestCastSectionCarriesTheCastRows proves the snapshot carries the cast rows
// themselves rather than only their expansion. The relay re-resolves a screen's
// program from this section at every daypart boundary, so a section that carried
// the referencing playlist but not the cast would leave the relay dereferencing
// nothing — a screen that plays the cast until the first boundary and then stops.
func TestCastSectionCarriesTheCastRows(t *testing.T) {
	const asset = "sha256:c0stc0stc0stc0stc0stc0stc0stc0stc0stc0stc0stc0stc0stc0stc0stc0st"
	s := seededStore(t, asset)
	id := writeCast(t, s, castWithThreeSlides(asset))
	replaceSeedPlaylistItems(t, s, []datamodel.PlaylistItem{
		{Source: datamodel.PlaylistSourceCast, CastID: id},
	})

	sec := buildSnapshot(t, s).Sections.Schedule
	if len(sec.Casts) != 1 {
		t.Fatalf("schedule.casts = %d rows, want the one authored cast; got %s", len(sec.Casts), sec.Casts)
	}
	rows, errs := datamodel.ValidateRows(rawRowsFromSection(sec))
	if len(errs) != 0 {
		t.Fatalf("the carried section does not re-validate: %+v", errs)
	}
	if len(rows.Casts) != 1 || rows.Casts[0].ID != id || len(rows.Casts[0].Slides) != 3 {
		t.Fatalf("carried cast rows = %+v, want the 3-slide cast %s", rows.Casts, id)
	}
}

// TestCastWithAnUndrawableSlideDropsOnlyThatSlide pins the degrade. A slide
// whose layers do not pass the shared gate is skipped rather than served
// malformed — and skipping it costs its own slot alone, not the rest of the
// cast, because an operator who mis-sized one slide should not lose the whole
// rotation.
//
// The bad slide is written through the store's own validator being bypassed
// deliberately: the api and the store both refuse to STORE this cast, which is
// the point of the authoring gate, so the only way a projection ever meets one
// is a row that predates a rule or arrives from elsewhere. Projecting a
// hand-built RowStore is how that state is reachable at all.
func TestCastWithAnUndrawableSlideDropsOnlyThatSlide(t *testing.T) {
	rowStore := datamodel.RowStore{Rows: datamodel.RowSet{
		Playlists: []datamodel.Playlist{{
			ID: "01J8ZP1AY11ST0000000000001", ScopeNode: castScopeNode, Name: "Mixed",
			Items: []datamodel.PlaylistItem{{Source: datamodel.PlaylistSourceCast, CastID: castID}},
		}},
		Casts: []datamodel.Cast{{
			ID: castID, ScopeNode: castScopeNode, Name: "Mixed",
			Slides: []datamodel.CastSlide{
				{ID: "good", Layers: []wire.Layer{{Kind: wire.LayerKindText, X: 0, Y: 0, W: 400, H: 100, Text: "ok"}}},
				// Runs off the right edge of the 1920x1080 canvas.
				{ID: "offcanvas", Layers: []wire.Layer{{Kind: wire.LayerKindText, X: 1900, Y: 0, W: 400, H: 100, Text: "clipped"}}},
				{ID: "alsogood", Layers: []wire.Layer{{Kind: wire.LayerKindRect, X: 0, Y: 0, W: 100, H: 100, Color: "#ffffff"}}},
			},
		}},
	}}

	content := playlistContent(rowStore, "01J8ZP1AY11ST0000000000001", parityOrigin)
	if len(content) != 2 {
		t.Fatalf("content = %d items, want the 2 drawable slides; got %+v", len(content), content)
	}
	if content[0].Layers[0].Text != "ok" || content[1].Layers[0].Kind != wire.LayerKindRect {
		t.Errorf("the surviving slides are not the two drawable ones, in order: %+v", content)
	}
}

// TestCastDefaultDurationReachesTheAppSignedBaseline is the app-side half of the
// cast-level default dwell time (DAT-043 `default_duration_ms`), driven through
// the REAL store write path and the real snapshot build rather than over a
// hand-built row set — so it also proves the store ACCEPTS the field (an
// authoring surface that took the setting and dropped it on the floor would
// otherwise look identical from the console).
//
// The relay-side twin is schedulehost.TestCastDefaultDurationFillsSlidesThatStateNone.
// They are separate because a screen must see the same dwell times whether it is
// playing this baseline or the relay's re-resolution of a daypart boundary, and
// a single test on either side cannot say that.
func TestCastDefaultDurationReachesTheAppSignedBaseline(t *testing.T) {
	const asset = "sha256:c0stc0stc0stc0stc0stc0stc0stc0stc0stc0stc0stc0stc0stc0stc0stc0st"
	s := seededStore(t, asset)
	cast := castWithThreeSlides(asset)
	cast.DefaultDurationMS = 5000
	id := writeCast(t, s, cast)
	// No `duration_seconds` on the item: the cast's own default is what the
	// third slide (which states none) must reach.
	replaceSeedPlaylistItems(t, s, []datamodel.PlaylistItem{
		{Source: datamodel.PlaylistSourceCast, CastID: id},
	})

	prog := programForScreen(t, buildSnapshot(t, s).Sections.ScreenPrograms, store.SeedScreenID)
	if len(prog.Content) != 3 {
		t.Fatalf("content = %d items, want one per cast slide", len(prog.Content))
	}
	// All three at once: the two slides that state their own dwell time must be
	// untouched by the default, which is the mistake "apply the cast default"
	// most easily becomes.
	want := []int64{4000, 9000, 5000}
	for i, ref := range prog.Content {
		if ref.DurationMS != want[i] {
			t.Errorf("content[%d].duration_ms = %d, want %d", i, ref.DurationMS, want[i])
		}
	}
}
