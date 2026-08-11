package snapshot

import (
	"testing"

	"github.com/maaxton/waiveo-next/internal/datamodel"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// TestCastSlideIDRidesTheProjection pins the field a `nav` layer's jump target
// resolves against.
//
// A nav item names its target by the CAST-LOCAL slide id, so the projection has
// to stamp that id onto the content reference or the player is handed a menu it
// can draw, focus and press — and never resolve. The failure would be entirely
// silent: the slide renders, the ring moves, OK does nothing.
//
// Matching by id rather than by array position is the point: a cast's slides
// share the content array with everything else the playlist carries, so an index
// authored against the cast addresses the wrong item the moment an entry is
// inserted before it. This test therefore also places an ASSET item first, so a
// projection that had leaned on position would visibly disagree.
func TestCastSlideIDRidesTheProjection(t *testing.T) {
	const asset = "sha256:c0stc0stc0stc0stc0stc0stc0stc0stc0stc0stc0stc0stc0stc0stc0stc0st"
	rowStore := datamodel.RowStore{Rows: datamodel.RowSet{
		Playlists: []datamodel.Playlist{{
			ID: "01J8ZP1AY11ST0000000000001", ScopeNode: castScopeNode, Name: "Mixed",
			Items: []datamodel.PlaylistItem{
				{Source: datamodel.PlaylistSourceAsset, AssetRef: asset},
				{Source: datamodel.PlaylistSourceCast, CastID: castID},
			},
		}},
		Casts: []datamodel.Cast{{
			ID: castID, ScopeNode: castScopeNode, Name: "Menu deck",
			Slides: []datamodel.CastSlide{
				{ID: "home", Layers: []wire.Layer{{
					Kind: wire.LayerKindNav, X: 0, Y: 900, W: 1200, H: 120,
					Items: []wire.NavItem{{Label: "Rooms", TargetSlideID: "rooms"}},
				}}},
				{ID: "rooms", Layers: []wire.Layer{{Kind: wire.LayerKindText, X: 0, Y: 0, W: 400, H: 100, Text: "Rooms"}}},
			},
		}},
	}}

	content := playlistContent(rowStore, "01J8ZP1AY11ST0000000000001", parityOrigin)
	if len(content) != 3 {
		t.Fatalf("content = %d items, want the asset plus both cast slides; got %+v", len(content), content)
	}
	if content[0].SlideID != "" {
		t.Errorf("a plain asset item must carry no cast-local slide id, got %q", content[0].SlideID)
	}
	if content[1].SlideID != "home" || content[2].SlideID != "rooms" {
		t.Fatalf("cast slide ids = %q/%q, want home/rooms — a nav target resolves against these",
			content[1].SlideID, content[2].SlideID)
	}
	// The nav item's target must name one of the ids actually on the wire, or
	// the menu dead-ends on the device.
	target := content[1].Layers[0].Items[0].TargetSlideID
	found := false
	for _, c := range content {
		if c.SlideID == target {
			found = true
		}
	}
	if !found {
		t.Fatalf("nav target %q names no projected item; the menu would dead-end on the device", target)
	}
}

// TestInlineSlideCarriesNoCastLocalID: only a slide projected FROM A CAST has a
// cast-local id. An inline `slide` playlist item has none, so it must emit no
// `slide_id` key at all — which is what keeps every pre-existing program's bytes
// (and therefore its signature and its program_revision) identical to before
// this field existed.
func TestInlineSlideCarriesNoCastLocalID(t *testing.T) {
	rowStore := datamodel.RowStore{Rows: datamodel.RowSet{
		Playlists: []datamodel.Playlist{{
			ID: "01J8ZP1AY11ST0000000000001", ScopeNode: castScopeNode, Name: "Inline",
			Items: []datamodel.PlaylistItem{{
				Source: "slide",
				Slide:  &datamodel.Slide{Layers: []wire.Layer{{Kind: wire.LayerKindText, X: 0, Y: 0, W: 400, H: 100, Text: "hi"}}},
			}},
		}},
	}}
	content := playlistContent(rowStore, "01J8ZP1AY11ST0000000000001", parityOrigin)
	if len(content) != 1 {
		t.Fatalf("content = %d items, want 1", len(content))
	}
	if content[0].SlideID != "" {
		t.Errorf("an inline slide item must carry no cast-local slide id, got %q", content[0].SlideID)
	}
}
