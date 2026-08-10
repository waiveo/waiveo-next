package store_test

import (
	"encoding/json"
	"testing"

	"github.com/maaxton/waiveo-next/internal/app/store"
	"github.com/maaxton/waiveo-next/internal/datamodel"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// assetrefs_test.go pins the ONE projection three subsystems read (the api's
// authoring guard, the retention sweep, the workspace export). Each of them was
// previously a hand-written copy that read a playlist item's `asset_ref` and
// nothing else, so the shapes asserted here — a cast's slide layers and an
// inline `source: "slide"` item's layers — are exactly the ones that were
// invisible in all three at once.

// imageLayer is a full-canvas image layer naming ref.
func imageLayer(ref string) wire.Layer {
	return wire.Layer{Kind: wire.LayerKindImage, X: 0, Y: 0, W: 1920, H: 1080, AssetRef: ref}
}

// TestRowAssetReferencesReadsEveryAssetBearingShapeOfAPlaylist covers the two
// ways one playlist row names content: an item's own asset_ref, and the image
// layers of an inline slide item, whose content IS its layer stack and which
// carries no item-level asset_ref at all.
func TestRowAssetReferencesReadsEveryAssetBearingShapeOfAPlaylist(t *testing.T) {
	body, err := json.Marshal(datamodel.Playlist{
		ID: refsPlaylistA, ScopeNode: refsScopeNode, Name: "Mixed",
		Items: []datamodel.PlaylistItem{
			{Source: "asset", AssetRef: "sha256:aa11"},
			{Source: "playable", PackID: "acme.signage", ContentID: "hero"},
			{Source: "slide", Slide: &datamodel.Slide{Layers: []wire.Layer{
				{Kind: wire.LayerKindText, X: 0, Y: 0, W: 100, H: 100, Text: "Hi"},
				imageLayer("sha256:bb22"),
			}}},
		},
	})
	if err != nil {
		t.Fatalf("marshal playlist: %v", err)
	}

	refs, err := store.RowAssetReferences(store.KindPlaylist, body)
	if err != nil {
		t.Fatalf("RowAssetReferences: %v", err)
	}
	want := []store.AssetReference{
		{Field: "items[0].asset_ref", Ref: "sha256:aa11"},
		{Field: "items[2].slide.layers[1].asset_ref", Ref: "sha256:bb22"},
	}
	assertAssetRefs(t, refs, want)
	if got := refs[1].HexDigest(); got != "bb22" {
		t.Errorf("HexDigest = %q, want the origin's own key %q", got, "bb22")
	}
}

// TestRowAssetReferencesReadsACastsSlideImages is B1/B2/B3's shared blind spot
// stated as one assertion: a cast names content through its slides' image
// layers, and a projection that cannot see them makes the api accept an
// unresolvable image, the sweep delete a played one, and the export omit it.
func TestRowAssetReferencesReadsACastsSlideImages(t *testing.T) {
	body, err := json.Marshal(datamodel.Cast{
		ID: refsCastA, ScopeNode: refsScopeNode, Name: "Lunch Menu",
		Slides: []datamodel.CastSlide{
			{ID: "title", Layers: []wire.Layer{
				{Kind: wire.LayerKindRect, X: 0, Y: 0, W: 1920, H: 1080, Color: "#101828"},
			}},
			{ID: "photo", Layers: []wire.Layer{
				imageLayer("sha256:cc33"),
				imageLayer("sha256:dd44"),
			}},
		},
	})
	if err != nil {
		t.Fatalf("marshal cast: %v", err)
	}

	refs, err := store.RowAssetReferences(store.KindCast, body)
	if err != nil {
		t.Fatalf("RowAssetReferences: %v", err)
	}
	assertAssetRefs(t, refs, []store.AssetReference{
		{Field: "slides[1].layers[0].asset_ref", Ref: "sha256:cc33"},
		{Field: "slides[1].layers[1].asset_ref", Ref: "sha256:dd44"},
	})
}

// TestRowAssetReferencesReportsAnUndecodableBody pins that an unreadable row is
// an ERROR and not an empty answer. The three consumers resolve it differently
// on purpose — the sweep aborts, the export skips, the api defers to the store's
// own failure — and an empty slice would silently make the destructive choice
// for the sweep.
func TestRowAssetReferencesReportsAnUndecodableBody(t *testing.T) {
	for _, kind := range store.AssetBearingKinds {
		if _, err := store.RowAssetReferences(kind, []byte(`{"items": 3, "slides": 3}`)); err == nil {
			t.Errorf("a %s body that will not decode returned no error; a sweep would treat its references as absent and reclaim them", kind)
		}
	}
}

// TestAssetBearingKindsCoversEveryKindWithReferences guards the list itself. A
// kind that can name content but is missing from it contributes nothing to the
// retention sweep or the export manifest — which is the exact state a cast was
// in — and the omission is invisible in every behavioural test of the kinds that
// ARE listed.
func TestAssetBearingKindsCoversEveryKindWithReferences(t *testing.T) {
	listed := map[store.Kind]bool{}
	for _, k := range store.AssetBearingKinds {
		listed[k] = true
	}
	for _, k := range []store.Kind{store.KindPlaylist, store.KindCast} {
		if !listed[k] {
			t.Errorf("kind %q names content-origin assets but is absent from AssetBearingKinds: "+
				"the retention sweep will reclaim its assets and the workspace export will omit them", k)
		}
	}
}

// assertAssetRefs compares a projection against the exact references expected,
// in document order — order is part of the contract, because the export's
// manifest is required to be stable across exports of an unchanged workspace.
func assertAssetRefs(t *testing.T, got, want []store.AssetReference) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("asset references = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("asset reference %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}
