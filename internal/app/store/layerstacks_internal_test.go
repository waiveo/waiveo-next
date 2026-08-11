package store

// layerstacks_internal_test.go guards RowLayerStacks — the ONE enumeration of
// "which authored slide-layer stacks does this row carry" — from the inside,
// where it can see the closed set of kinds the store actually defines.
//
// It is an INTERNAL test for exactly that reason. The guard that matters is
// "every kind whose body RowLayerStacks reads is in LayerStackKinds", and the
// only way to state that without a second hand-written list is to drive the
// switch with every Kind the package declares (allKinds, unexported). A
// store_test-package version would have to re-type the kinds it checks, which
// makes it a copy of the thing it is guarding: adding a kind and forgetting BOTH
// lists would leave it green.
//
// The stake is a shipped defect. RowLayerStacks was introduced as the single
// enumeration behind both the retention/write-time asset projection and the
// derive work queue — and the queue then spelled its own list of kinds to call
// it with. An inline-slide derive layer had already once been accepted,
// projected onto real screens, and protected from the content sweep while being
// invisible to the tool that would render it; a second list is how that comes
// back.

import (
	"encoding/json"
	"testing"

	"github.com/maaxton/waiveo-next/internal/datamodel"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// omnibusBody carries EVERY authored layer-stack shape at once — a cast's
// `slides` and a playlist item's inline `slide` — so one body is a valid probe
// for any kind. A kind whose switch arm reads either shape answers with stacks;
// a kind with no arm answers with none.
func omnibusBody(t *testing.T) []byte {
	t.Helper()
	layer := wire.Layer{Kind: wire.LayerKindText, X: 0, Y: 0, W: 800, H: 120, Text: "probe"}
	body, err := json.Marshal(map[string]any{
		"id":     "01J8ZPROBE000000000000001",
		"slides": []datamodel.CastSlide{{ID: "s1", Layers: []wire.Layer{layer}}},
		"items": []datamodel.PlaylistItem{
			{Source: "slide", Slide: &datamodel.Slide{Layers: []wire.Layer{layer}}},
		},
	})
	if err != nil {
		t.Fatalf("marshal the probe body: %v", err)
	}
	return body
}

// TestEveryLayerStackKindIsEnumerated drives RowLayerStacks itself, for every
// kind the store defines, and requires the answer and the published list to
// agree in BOTH directions.
//
// Forward: a kind that yields stacks and is missing from LayerStackKinds is a
// kind the derive queue never scans — a `derive` layer authored into it is
// accepted, projected and swept-protected while being reported to nothing.
// Reverse: a kind listed but yielding nothing is a list that has drifted the
// other way, which is how a list stops being trustworthy at all.
func TestEveryLayerStackKindIsEnumerated(t *testing.T) {
	body := omnibusBody(t)

	listed := map[Kind]bool{}
	for _, k := range LayerStackKinds {
		listed[k] = true
	}

	for _, k := range allKinds {
		stacks, err := RowLayerStacks(k, body)
		if err != nil {
			t.Fatalf("RowLayerStacks(%s) on a well-formed probe body: %v", k, err)
		}
		switch {
		case len(stacks) > 0 && !listed[k]:
			t.Errorf("kind %q carries authored layer stacks but is absent from LayerStackKinds: "+
				"GET /derive/pending never scans it, so a derive layer authored into one is accepted, "+
				"projected onto screens and held against the content sweep while being reported to nothing", k)
		case len(stacks) == 0 && listed[k]:
			t.Errorf("kind %q is in LayerStackKinds but RowLayerStacks reads no stacks from it", k)
		}
		delete(listed, k)
	}
	for k := range listed {
		t.Errorf("LayerStackKinds names %q, which is not a kind this store defines", k)
	}
}

// TestLayerStackKindsIsTheDeriveQueuesList states the binding the api layer
// depends on from the side that owns it. The api's `deriveLayerKinds` is an
// alias of this slice rather than a copy, and a copy is what this whole file
// exists to prevent — but an alias is only as good as the list it aliases, so
// the list is required to be non-empty and to contain the two shapes that
// demonstrably reach a screen.
func TestLayerStackKindsIsTheDeriveQueuesList(t *testing.T) {
	if len(LayerStackKinds) == 0 {
		t.Fatal("LayerStackKinds is empty: the derive queue would scan nothing and report no outstanding work, ever")
	}
	listed := map[Kind]bool{}
	for _, k := range LayerStackKinds {
		listed[k] = true
	}
	for _, k := range []Kind{KindCast, KindPlaylist} {
		if !listed[k] {
			t.Errorf("kind %q is missing from LayerStackKinds", k)
		}
	}
}
