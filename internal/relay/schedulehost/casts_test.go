package schedulehost

import (
	"testing"

	"github.com/maaxton/waiveo-next/internal/datamodel"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// casts_test.go drives the relay-side half of the cast expansion (data-model/1
// DAT-043): the projection a screen actually receives every time a daypart
// boundary makes this relay re-resolve, as opposed to the app-signed baseline
// internal/feeder/snapshot produces once per generation.
//
// The two must produce the same playback from the same rows. That equality is
// pinned end-to-end by snapshot.TestDerivedContentMatchesRelaySideProjection;
// what these tests pin is this side's own behaviour in isolation, so a failure
// says WHICH side moved rather than only that the two disagree.

const (
	castFixturePlaylistID = "01J8ZVR1F1XTVREP1AY11STCST"
	castFixtureCastID     = "01J8ZVR1F1XTVRECAST0000001"
	castFixtureAsset      = "sha256:ABC"
)

// castStore builds the minimal RowStore the cast expansion reads: one playlist
// whose single item REFERENCES a cast, and the three-slide cast it names. The
// slides' dwell times differ deliberately — stated, stated, absent — so the
// per-slide resolution and its fallback to the playlist item's own
// `duration_seconds` are separable in the result.
func castStore(itemDurationSeconds *int) datamodel.RowStore {
	return datamodel.RowStore{
		Rows: datamodel.RowSet{
			Playlists: []datamodel.Playlist{{
				ID: castFixturePlaylistID, ScopeNode: "01J8ZVR1F1XTVRESC0PEN0DE01", Name: "Cast Fixture",
				Items: []datamodel.PlaylistItem{{
					Source: datamodel.PlaylistSourceCast, CastID: castFixtureCastID,
					DurationSeconds: itemDurationSeconds,
				}},
				Revision: 1, CreatedAt: 1, UpdatedAt: 1,
			}},
			Casts: []datamodel.Cast{{
				ID: castFixtureCastID, ScopeNode: "01J8ZVR1F1XTVRESC0PEN0DE01", Name: "Lunch Menu",
				Slides: []datamodel.CastSlide{
					{ID: "title", DurationMS: 4000, Layers: []wire.Layer{
						{Kind: wire.LayerKindText, X: 0, Y: 0, W: 900, H: 120, Text: "Lunch"},
					}},
					{ID: "photo", DurationMS: 9000, Layers: []wire.Layer{
						{Kind: wire.LayerKindImage, X: 100, Y: 100, W: 800, H: 600, AssetRef: castFixtureAsset},
					}},
					{ID: "clock", Layers: []wire.Layer{
						{Kind: wire.LayerKindClock, X: 1400, Y: 40, W: 460, H: 120, Text: "15:04:05"},
					}},
				},
				Revision: 1, CreatedAt: 1, UpdatedAt: 1,
			}},
		},
	}
}

// TestCastItemProjectsOneSlideLeaseItemPerSlide asserts the fan-out: ONE
// authored playlist item becomes one `type:"slide"` Lease content item per slide
// of the cast it names, in authored order, each carrying that slide's own layer
// stack and its resolved dwell time.
func TestCastItemProjectsOneSlideLeaseItemPerSlide(t *testing.T) {
	seven := 7
	store := castStore(&seven)

	content := playlistContent(store, castFixturePlaylistID, contentSigner{origin: "https://origin.example"})
	if len(content) != 3 {
		t.Fatalf("playlistContent returned %d items, want one per cast slide; got %+v", len(content), content)
	}

	wantFirstKinds := []string{wire.LayerKindText, wire.LayerKindImage, wire.LayerKindClock}
	// 4000/9000 are the slides' own; 7000 is the playlist item's
	// duration_seconds:7 override inherited by the slide that states none.
	wantDurations := []int64{4000, 9000, 7000}
	for i, item := range content {
		if item.Type != leaseContentTypeSlide {
			t.Errorf("content[%d].type = %q, want %q", i, item.Type, leaseContentTypeSlide)
		}
		if item.AssetRef != "" || item.URL != "" {
			t.Errorf("content[%d] carries item-level asset_ref=%q url=%q, want neither (a slide's content is its layers)", i, item.AssetRef, item.URL)
		}
		if len(item.Layers) == 0 {
			t.Fatalf("content[%d] carries no layers", i)
		}
		if got := item.Layers[0].Kind; got != wantFirstKinds[i] {
			t.Errorf("content[%d].layers[0].kind = %q, want %q (slides out of authored order?)", i, got, wantFirstKinds[i])
		}
		if item.DurationMS != wantDurations[i] {
			t.Errorf("content[%d].duration_ms = %d, want %d", i, item.DurationMS, wantDurations[i])
		}
	}

	// The image layer's url is MINTED here from the relay's own signer, in the
	// same `<origin>/content/<hex>` grammar a plain asset item gets — never
	// carried from the authored row, which states only the asset_ref.
	if want := "https://origin.example/content/ABC"; content[1].Layers[0].URL != want {
		t.Errorf("image layer url = %q, want %q", content[1].Layers[0].URL, want)
	}
}

// TestCastSlideWithNoDurationAndNoItemOverrideCarriesNone asserts the third arm
// of the dwell-time resolution: with neither a slide duration nor a playlist-item
// override, the Lease item carries no `duration_ms` at all (omitempty) and the
// player applies its own default — rather than a zero this side invented.
func TestCastSlideWithNoDurationAndNoItemOverrideCarriesNone(t *testing.T) {
	content := playlistContent(castStore(nil), castFixturePlaylistID, contentSigner{origin: "https://origin.example"})
	if len(content) != 3 {
		t.Fatalf("playlistContent returned %d items, want 3; got %+v", len(content), content)
	}
	if content[2].DurationMS != 0 {
		t.Errorf("content[2].duration_ms = %d, want 0 (no slide duration, no item override)", content[2].DurationMS)
	}
}

// TestCastItemWithAnUnresolvableCastContributesNothing pins the degrade for a
// carried section whose cast row is missing. The app store refuses to write such
// a playlist and refuses to delete a referenced cast, so this is a degraded
// input — and on a degraded input the honest projection of absent content is no
// content, never a placeholder item a screen would stall on.
func TestCastItemWithAnUnresolvableCastContributesNothing(t *testing.T) {
	store := castStore(nil)
	store.Rows.Casts = nil

	if content := playlistContent(store, castFixturePlaylistID, contentSigner{origin: "https://origin.example"}); len(content) != 0 {
		t.Errorf("playlistContent returned %+v, want no content for an unresolvable cast reference", content)
	}
}

// TestCastSlideThatWouldNotDrawIsSkippedNotServed asserts the same drop rule an
// inline `slide` item gets, applied per slide: a layer stack the shared gate
// rejects never reaches a player, and it costs its own slot alone rather than
// the whole cast.
func TestCastSlideThatWouldNotDrawIsSkippedNotServed(t *testing.T) {
	store := castStore(nil)
	store.Rows.Casts[0].Slides = append(store.Rows.Casts[0].Slides, datamodel.CastSlide{
		ID: "offcanvas",
		// A layer whose far edge runs past the 1920x1080 canvas.
		Layers: []wire.Layer{{Kind: wire.LayerKindRect, X: 1800, Y: 0, W: 400, H: 100, Color: "#ffffff"}},
	})

	content := playlistContent(store, castFixturePlaylistID, contentSigner{origin: "https://origin.example"})
	if len(content) != 3 {
		t.Fatalf("playlistContent returned %d items, want the 3 drawable slides; got %+v", len(content), content)
	}
	for i, item := range content {
		if err := wire.ValidateSlideLayers(item.Layers); err != nil {
			t.Errorf("content[%d] was served with layers that do not validate: %v", i, err)
		}
	}
}

// TestCastDefaultDurationFillsSlidesThatStateNone drives the cast-level default
// dwell time (DAT-043 `default_duration_ms`) through the relay-side projection.
//
// It is the arm of the resolution an operator actually authors in the Studio's
// playback settings: "hold every slide for five seconds" is one edit on the
// cast, not an edit of every slide, and every slide added afterwards inherits it
// instead of silently falling to whatever the player defaults to. The assertion
// is deliberately over ALL THREE slides at once, because the failure that would
// otherwise pass is a default applied to the whole cast (overwriting the two
// slides that state their own) rather than only to the slide that said nothing.
func TestCastDefaultDurationFillsSlidesThatStateNone(t *testing.T) {
	// No playlist-item override, so the cast's own default is what the third
	// slide must reach.
	store := castStore(nil)
	store.Rows.Casts[0].DefaultDurationMS = 5000

	content := playlistContent(store, castFixturePlaylistID, contentSigner{origin: "https://origin.example"})
	if len(content) != 3 {
		t.Fatalf("playlistContent returned %d items, want one per cast slide", len(content))
	}
	want := []int64{4000, 9000, 5000}
	for i, item := range content {
		if item.DurationMS != want[i] {
			t.Errorf("content[%d].duration_ms = %d, want %d (slide own → item override → cast default)", i, item.DurationMS, want[i])
		}
	}
}

// TestPlaylistItemOverrideOutranksTheCastDefault pins the ORDER of the two
// fallbacks against each other, which is the half a test of either one alone
// cannot see.
//
// DAT-042 puts the item's `duration_seconds` above the cast's
// `default_duration_ms` on purpose: `duration_seconds` is a statement about one
// PLACEMENT of the cast, `default_duration_ms` a statement about slides that
// said nothing. Inverting them would make the placement override unreachable for
// every cast that carries a default — an operator setting 12s on this playlist
// entry would watch the screen keep showing 5s and have nothing to look at that
// says why.
func TestPlaylistItemOverrideOutranksTheCastDefault(t *testing.T) {
	twelve := 12
	store := castStore(&twelve)
	store.Rows.Casts[0].DefaultDurationMS = 5000

	content := playlistContent(store, castFixturePlaylistID, contentSigner{origin: "https://origin.example"})
	if len(content) != 3 {
		t.Fatalf("playlistContent returned %d items, want one per cast slide", len(content))
	}
	if content[2].DurationMS != 12000 {
		t.Errorf("the slide that states no duration took %d ms; the playlist item's own 12s override must outrank the cast's 5s default", content[2].DurationMS)
	}
}

// TestCastSlideIDRidesTheRelayProjection is the relay-side twin of
// snapshot.TestCastSlideIDRidesTheProjection.
//
// The two projections must agree, and this is the field where a disagreement is
// invisible until somebody presses a menu item: the app-authored baseline and
// the relay's own daypart re-resolution both produce slide items, and a `nav`
// target only resolves if the item carries its cast-local id. If only one
// projection stamped it, a menu would work until the first schedule boundary and
// then silently stop, which is the hardest failure of all to attribute.
func TestCastSlideIDRidesTheRelayProjection(t *testing.T) {
	content := playlistContent(castStore(nil), castFixturePlaylistID, contentSigner{origin: "https://origin.example"})
	if len(content) != 3 {
		t.Fatalf("playlistContent returned %d items, want one per cast slide", len(content))
	}
	want := []string{"title", "photo", "clock"}
	for i, item := range content {
		if item.SlideID != want[i] {
			t.Errorf("content[%d].slide_id = %q, want %q — a nav target resolves against this", i, item.SlideID, want[i])
		}
	}
}
