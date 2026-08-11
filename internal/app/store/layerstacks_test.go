package store_test

import (
	"encoding/json"
	"testing"

	"github.com/maaxton/waiveo-next/internal/app/store"
	"github.com/maaxton/waiveo-next/internal/datamodel"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// layerstacks_test.go pins RowLayerStacks' own answer, directly.
//
// It had none. Every assertion about it was made THROUGH a consumer — the asset
// projection, the derive queue — which is enough to catch a stack that is
// missing entirely and nothing else. The three members a consumer writes back
// through (Field, SlideID, ItemIndex) are precisely the ones a consumer test
// does not look at: a swapped SlideID sends a rasterized PNG to the wrong slide,
// an ItemIndex off by one writes it into the wrong playlist item, and a wrong
// Field points an operator's error message at a layer that is fine. All three
// produce a green consumer test and a wrong write.

func stackLayer(text string) wire.Layer {
	return wire.Layer{Kind: wire.LayerKindText, X: 0, Y: 0, W: 800, H: 120, Text: text}
}

func mustBody(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal the row body: %v", err)
	}
	return b
}

// TestRowLayerStacksReadsACastsSlidesInDocumentOrder: every slide, in order,
// each addressed by its own document-local id and by the JSON path a write-back
// or an error message uses.
func TestRowLayerStacksReadsACastsSlidesInDocumentOrder(t *testing.T) {
	body := mustBody(t, datamodel.Cast{
		ID: "01J8ZCAST00000000000000001", Name: "Lobby",
		Slides: []datamodel.CastSlide{
			{ID: "first", Layers: []wire.Layer{stackLayer("a"), stackLayer("b")}},
			{ID: "second", Layers: []wire.Layer{stackLayer("c")}},
		},
	})

	got, err := store.RowLayerStacks(store.KindCast, body)
	if err != nil {
		t.Fatalf("RowLayerStacks: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("stacks = %d, want 2: %+v", len(got), got)
	}
	// ItemIndex is -1 on a cast slide, which is the discriminator: a cast slide
	// belongs to no playlist item, and -1 is an index no items array produces.
	// A consumer that read a zero here would write into item 0 of a playlist.
	if got[0].Field != "slides[0]" || got[0].SlideID != "first" || got[0].ItemIndex != -1 {
		t.Errorf("stack 0 = %+v, want slides[0]/first/-1", store.LayerStack{Field: got[0].Field, SlideID: got[0].SlideID, ItemIndex: got[0].ItemIndex})
	}
	if got[1].Field != "slides[1]" || got[1].SlideID != "second" || got[1].ItemIndex != -1 {
		t.Errorf("stack 1 = %+v, want slides[1]/second/-1", store.LayerStack{Field: got[1].Field, SlideID: got[1].SlideID, ItemIndex: got[1].ItemIndex})
	}
	if len(got[0].Layers) != 2 || got[0].Layers[1].Text != "b" {
		t.Errorf("stack 0 layers = %+v, want the authored stack in z-order", got[0].Layers)
	}
}

// TestRowLayerStacksReadsInlineSlidesByTheirItemIndex: an inline slide has no
// id, so ItemIndex is the ONLY locator a write-back can use — and it must be the
// index into the items array, not the index among the slide items. The fixture
// puts the inline slides at items 1 and 3 deliberately: an implementation that
// counted the stacks it emitted rather than the items it walked would write
// slide 3's raster into item 1.
func TestRowLayerStacksReadsInlineSlidesByTheirItemIndex(t *testing.T) {
	body := mustBody(t, datamodel.Playlist{
		ID: "01J8ZPLAYLIST00000000000001", Name: "Foyer",
		Items: []datamodel.PlaylistItem{
			{Source: "asset", AssetRef: "sha256:aa"},
			{Source: "slide", Slide: &datamodel.Slide{Layers: []wire.Layer{stackLayer("inline-1")}}},
			{Source: "asset", AssetRef: "sha256:bb"},
			{Source: "slide", Slide: &datamodel.Slide{Layers: []wire.Layer{stackLayer("inline-3")}}},
		},
	})

	got, err := store.RowLayerStacks(store.KindPlaylist, body)
	if err != nil {
		t.Fatalf("RowLayerStacks: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("stacks = %d, want the 2 inline slides: %+v", len(got), got)
	}
	for i, want := range []struct {
		field string
		index int
		text  string
	}{
		{"items[1].slide", 1, "inline-1"},
		{"items[3].slide", 3, "inline-3"},
	} {
		if got[i].Field != want.field || got[i].ItemIndex != want.index {
			t.Errorf("stack %d = %s/%d, want %s/%d", i, got[i].Field, got[i].ItemIndex, want.field, want.index)
		}
		if got[i].SlideID != "" {
			t.Errorf("stack %d carries SlideID %q; an inline slide has no id, and a non-empty one would make a consumer address it as a cast slide", i, got[i].SlideID)
		}
		if len(got[i].Layers) != 1 || got[i].Layers[0].Text != want.text {
			t.Errorf("stack %d layers = %+v, want the layers of %s", i, got[i].Layers, want.field)
		}
	}
}

// TestRowLayerStacksAnswersNothingForAKindThatCarriesNone — asking is legitimate
// (a caller iterating kinds), and a kind that carries no stacks is not a fault.
// An error here would make the derive queue drop rows it had every right to skip.
func TestRowLayerStacksAnswersNothingForAKindThatCarriesNone(t *testing.T) {
	stacks, err := store.RowLayerStacks(store.KindSchedule, []byte(`{"id":"01J8ZSCHED0000000000000001"}`))
	if err != nil {
		t.Errorf("RowLayerStacks on a stack-less kind returned an error: %v", err)
	}
	if len(stacks) != 0 {
		t.Errorf("RowLayerStacks on a stack-less kind returned %+v", stacks)
	}
}

// TestRowLayerStacksReportsAnUndecodableBody: an unreadable row is an ERROR, not
// an empty answer, for the reason RowAssetReferences' own doc gives — the
// callers differ in what they do with it and every one of those is a decision
// the caller has to make deliberately.
func TestRowLayerStacksReportsAnUndecodableBody(t *testing.T) {
	for _, kind := range store.LayerStackKinds {
		if _, err := store.RowLayerStacks(kind, []byte(`{"items": 3, "slides": 3}`)); err == nil {
			t.Errorf("an undecodable %s body returned no error", kind)
		}
	}
}
