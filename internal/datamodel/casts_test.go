package datamodel

import (
	"encoding/json"
	"testing"

	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// casts_test.go drives DAT-043's cast row through ValidateRows — the one gate
// every writer passes (the api handlers, a seed, a restore, a pack pipeline), so
// a rule proved here is a rule enforced on all of them.

const (
	castTestID        = "01J8ZCAST00000000000000001"
	castTestNode      = "01J8ZN0DE000000000000000S1"
	castTestPlaylist  = "01J8ZP1AY11ST0000000000001"
	castTestAssetRef  = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	castTestOtherCast = "01J8ZCAST00000000000000002"
)

// castRow marshals a Cast into the raw JSON ValidateRows consumes.
func castRow(t *testing.T, c Cast) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("marshal cast: %v", err)
	}
	return b
}

// validCast is the baseline every case below mutates one thing away from: one
// named, placed cast with a single drawable slide.
func validCast() Cast {
	return Cast{
		ID: castTestID, ScopeNode: castTestNode, Name: "Lunch Menu",
		Slides: []CastSlide{{ID: "s1", DurationMS: 5000, Layers: []wire.Layer{
			{Kind: wire.LayerKindText, X: 0, Y: 0, W: 800, H: 100, Text: "Lunch"},
		}}},
	}
}

// TestAValidCastPasses is the accept case, and it earns its place: every
// rejection below is only meaningful if the baseline it mutates is itself
// accepted, and an over-strict validator that refused everything would otherwise
// pass every negative test in this file.
func TestAValidCastPasses(t *testing.T) {
	rows, errs := ValidateRows(RawRows{Casts: []json.RawMessage{castRow(t, validCast())}})
	if len(errs) != 0 {
		t.Fatalf("a well-formed cast was rejected: %+v", errs)
	}
	if len(rows.Casts) != 1 || len(rows.Casts[0].Slides) != 1 {
		t.Fatalf("the accepted cast did not decode into the row set: %+v", rows.Casts)
	}
}

// TestACastWithNoSlidesIsRefused pins DAT-043's non-empty rule. A cast with no
// slides plays nothing, and a playlist item referencing it would contribute no
// content at all — a screen quietly playing less than its playlist states.
func TestACastWithNoSlidesIsRefused(t *testing.T) {
	c := validCast()
	c.Slides = nil
	_, errs := ValidateRows(RawRows{Casts: []json.RawMessage{castRow(t, c)}})
	if !hasErr(errs, "CAST_SLIDES_EMPTY", "slides") {
		t.Fatalf("want CAST_SLIDES_EMPTY on slides, got %+v", errs)
	}
}

// TestACastWithNoNameIsRefused pins the rule that makes the api document's own
// `minLength: 1` true for every writer, not only for an HTTP request body.
func TestACastWithNoNameIsRefused(t *testing.T) {
	c := validCast()
	c.Name = "   "
	_, errs := ValidateRows(RawRows{Casts: []json.RawMessage{castRow(t, c)}})
	if !hasErr(errs, "CAST_NAME_MISSING", "name") {
		t.Fatalf("want CAST_NAME_MISSING on name, got %+v", errs)
	}
}

// TestACastSlideIDMustBePresentAndUnique pins the document-local identity rule.
// A duplicate id makes "the slide the operator moved" ambiguous to any editor
// that addresses slides by it.
func TestACastSlideIDMustBePresentAndUnique(t *testing.T) {
	layers := []wire.Layer{{Kind: wire.LayerKindText, X: 0, Y: 0, W: 100, H: 50, Text: "x"}}

	missing := validCast()
	missing.Slides = []CastSlide{{ID: "", Layers: layers}}
	if _, errs := ValidateRows(RawRows{Casts: []json.RawMessage{castRow(t, missing)}}); !hasErr(errs, "CAST_SLIDE_ID_INVALID", "slides[0].id") {
		t.Errorf("an unnamed slide: want CAST_SLIDE_ID_INVALID on slides[0].id, got %+v", errs)
	}

	dup := validCast()
	dup.Slides = []CastSlide{{ID: "same", Layers: layers}, {ID: "same", Layers: layers}}
	if _, errs := ValidateRows(RawRows{Casts: []json.RawMessage{castRow(t, dup)}}); !hasErr(errs, "CAST_SLIDE_ID_INVALID", "slides[1].id") {
		t.Errorf("a duplicate slide id: want CAST_SLIDE_ID_INVALID on slides[1].id, got %+v", errs)
	}
}

// TestACastSlideWhoseLayersWouldNotDrawIsRefused proves the authoring gate is
// the SHARED slide-layer gate rather than a private copy: each case below is a
// rule wire.ValidateAuthoredSlideLayers owns, and none of them is restated in
// this package.
func TestACastSlideWhoseLayersWouldNotDrawIsRefused(t *testing.T) {
	cases := map[string][]wire.Layer{
		"no layers at all":        {},
		"unknown kind":            {{Kind: "hologram", X: 0, Y: 0, W: 10, H: 10}},
		"zero area":               {{Kind: wire.LayerKindText, X: 0, Y: 0, W: 0, H: 10, Text: "x"}},
		"off canvas":              {{Kind: wire.LayerKindText, X: 1900, Y: 0, W: 100, H: 10, Text: "x"}},
		"text with no text":       {{Kind: wire.LayerKindText, X: 0, Y: 0, W: 100, H: 10}},
		"clock with no format":    {{Kind: wire.LayerKindClock, X: 0, Y: 0, W: 100, H: 10}},
		"image with no asset_ref": {{Kind: wire.LayerKindImage, X: 0, Y: 0, W: 100, H: 10}},
		"rect with no color":      {{Kind: wire.LayerKindRect, X: 0, Y: 0, W: 100, H: 10}},
		"unparseable color":       {{Kind: wire.LayerKindRect, X: 0, Y: 0, W: 100, H: 10, Color: "rebeccapurple"}},
	}
	for name, layers := range cases {
		t.Run(name, func(t *testing.T) {
			c := validCast()
			c.Slides = []CastSlide{{ID: "s1", Layers: layers}}
			_, errs := ValidateRows(RawRows{Casts: []json.RawMessage{castRow(t, c)}})
			if !hasErr(errs, "CAST_SLIDE_LAYERS_INVALID", "slides[0].layers") {
				t.Fatalf("want CAST_SLIDE_LAYERS_INVALID on slides[0].layers, got %+v", errs)
			}
		})
	}
}

// TestAnAuthoredImageLayerNeedsNoURL is the other half of that reuse, and the
// reason the authoring gate is a distinct entry point rather than the serving
// one: an image layer's fetch url is DERIVED at projection, so requiring it here
// would make every image layer unstorable.
func TestAnAuthoredImageLayerNeedsNoURL(t *testing.T) {
	c := validCast()
	c.Slides = []CastSlide{{ID: "s1", Layers: []wire.Layer{
		{Kind: wire.LayerKindImage, X: 0, Y: 0, W: 400, H: 300, AssetRef: castTestAssetRef},
	}}}
	if _, errs := ValidateRows(RawRows{Casts: []json.RawMessage{castRow(t, c)}}); len(errs) != 0 {
		t.Fatalf("an authored image layer with an asset_ref and no url was rejected: %+v", errs)
	}
	// The SERVING gate still requires the url the projection mints, so nothing
	// here loosened what a player is handed.
	if err := wire.ValidateSlideLayers(c.Slides[0].Layers); err == nil {
		t.Error("the serving gate accepted an image layer with no url; the two gates must differ only on that field")
	}
}

// TestACastSlideDurationMustBePositive pins the one numeric rule: a stated dwell
// time nothing can honour is a defect, while an ABSENT one is the sanctioned way
// to inherit the playlist item's own override.
func TestACastSlideDurationMustBePositive(t *testing.T) {
	c := validCast()
	c.Slides[0].DurationMS = -1
	if _, errs := ValidateRows(RawRows{Casts: []json.RawMessage{castRow(t, c)}}); !hasErr(errs, "CAST_SLIDE_DURATION_INVALID", "slides[0].duration_ms") {
		t.Fatalf("want CAST_SLIDE_DURATION_INVALID, got %+v", errs)
	}

	c = validCast()
	c.Slides[0].DurationMS = 0
	if _, errs := ValidateRows(RawRows{Casts: []json.RawMessage{castRow(t, c)}}); len(errs) != 0 {
		t.Fatalf("an omitted duration was rejected: %+v", errs)
	}
}

// TestEveryFailingSlideIsReported pins the multi-error answer API-013 publishes.
// A cast is a document an operator edits as a whole, so an editor that had to
// re-submit once per bad slide to discover the next one is that answer thrown
// away — which is exactly the trade this codebase refuses elsewhere in writing.
func TestEveryFailingSlideIsReported(t *testing.T) {
	c := validCast()
	c.Slides = []CastSlide{
		{ID: "a", Layers: []wire.Layer{{Kind: wire.LayerKindText, X: 0, Y: 0, W: 100, H: 10}}},
		{ID: "b", Layers: []wire.Layer{{Kind: wire.LayerKindRect, X: 0, Y: 0, W: 100, H: 10}}},
	}
	_, errs := ValidateRows(RawRows{Casts: []json.RawMessage{castRow(t, c)}})
	if !hasErr(errs, "CAST_SLIDE_LAYERS_INVALID", "slides[0].layers") || !hasErr(errs, "CAST_SLIDE_LAYERS_INVALID", "slides[1].layers") {
		t.Fatalf("both failing slides must be named at once, got %+v", errs)
	}
}

// TestACastMustCarryItsPlacement pins DAT-006's presence half for the new kind,
// through the same shared helper every other scheduling-core row uses.
func TestACastMustCarryItsPlacement(t *testing.T) {
	c := validCast()
	c.ScopeNode = ""
	if _, errs := ValidateRows(RawRows{Casts: []json.RawMessage{castRow(t, c)}}); !hasErr(errs, "ROW_SCOPE_NODE_MISSING", "scope_node") {
		t.Fatalf("want ROW_SCOPE_NODE_MISSING, got %+v", errs)
	}
}

// playlistRow marshals a playlist carrying exactly the given items.
func playlistRow(t *testing.T, items []PlaylistItem) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(Playlist{
		ID: castTestPlaylist, ScopeNode: castTestNode, Name: "Lobby", Items: items,
	})
	if err != nil {
		t.Fatalf("marshal playlist: %v", err)
	}
	return b
}

// TestAPlaylistCastReferenceMustResolve pins the referential rule in both of the
// directions it fires: a playlist naming a cast that is not there, and — the
// half only a full-row-set validator can see — the SAME playlist once the cast
// it names is removed, which is what makes deleting a referenced cast a refusal
// rather than a screen quietly losing slides.
func TestAPlaylistCastReferenceMustResolve(t *testing.T) {
	items := []PlaylistItem{{Source: PlaylistSourceCast, CastID: castTestID}}

	// Both rows present: resolves.
	if _, errs := ValidateRows(RawRows{
		Playlists: []json.RawMessage{playlistRow(t, items)},
		Casts:     []json.RawMessage{castRow(t, validCast())},
	}); len(errs) != 0 {
		t.Fatalf("a playlist naming a present cast was rejected: %+v", errs)
	}

	// The cast removed (the state a DELETE would leave behind): refused.
	if _, errs := ValidateRows(RawRows{Playlists: []json.RawMessage{playlistRow(t, items)}}); !hasErr(errs, "REFERENCE_INVALID", "items[0].cast_id") {
		t.Fatalf("want REFERENCE_INVALID on items[0].cast_id, got %+v", errs)
	}

	// A different cast present: still refused — presence of SOME cast is not
	// resolution of THIS reference.
	other := validCast()
	other.ID = castTestOtherCast
	if _, errs := ValidateRows(RawRows{
		Playlists: []json.RawMessage{playlistRow(t, items)},
		Casts:     []json.RawMessage{castRow(t, other)},
	}); !hasErr(errs, "REFERENCE_INVALID", "items[0].cast_id") {
		t.Fatalf("want REFERENCE_INVALID for an unrelated cast, got %+v", errs)
	}
}

// TestACastItemWithNoCastIDIsRefused closes the shape a bare source string would
// otherwise leave open: an item that declares `source: "cast"` and names none is
// an item that plays nothing, and it must not be storable.
func TestACastItemWithNoCastIDIsRefused(t *testing.T) {
	items := []PlaylistItem{{Source: PlaylistSourceCast}}
	if _, errs := ValidateRows(RawRows{Playlists: []json.RawMessage{playlistRow(t, items)}}); !hasErr(errs, "REFERENCE_INVALID", "items[0].cast_id") {
		t.Fatalf("want REFERENCE_INVALID on items[0].cast_id, got %+v", errs)
	}
}

// TestACastRowMayNotBePackOwned pins DAT-100/101 for the new kind: it goes
// through the same top-level pack-field scan every other scheduling-core row
// does, so a pack cannot come to own the content an operator authors.
func TestACastRowMayNotBePackOwned(t *testing.T) {
	raw := json.RawMessage(`{"id":"` + castTestID + `","scope_node":"` + castTestNode + `","name":"X","owner_pack":"p","slides":[]}`)
	if _, errs := ValidateRows(RawRows{Casts: []json.RawMessage{raw}}); !hasErr(errs, "SCHEDULER_ROW_PACK_OWNED", "") {
		t.Fatalf("want SCHEDULER_ROW_PACK_OWNED, got %+v", errs)
	}
}
