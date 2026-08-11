package api

import (
	"testing"
	"unsafe"

	"github.com/maaxton/waiveo-next/internal/app/store"
)

// derive_internal_test.go pins ONE property of the derive work queue that no
// behavioural test can reach: the set of row kinds it scans is not a list this
// package maintains.
//
// The queue's whole reason for existing correctly is that it sees every authored
// layer stack. store.RowLayerStacks was made the one enumeration of those after
// an inline-slide derive layer spent a release accepted, projected onto screens
// and protected from the content sweep while being invisible to the tool that
// would render it — and the queue then re-opened the same gap by spelling its
// own list of kinds to call it with. A hand-maintained copy of a single source
// of truth is not a single source of truth.
//
// A behavioural test cannot catch that. It can only assert the kinds that ARE
// scanned, and both lists agree today; the failure appears when a THIRD kind is
// added to store.RowLayerStacks and this file is not touched, which is precisely
// the state no existing test is in a position to notice.

// TestTheQueueScansTheStoresOwnLayerStackKinds asserts the two slices share a
// backing array — i.e. that deriveLayerKinds IS store.LayerStackKinds rather
// than a slice with the same contents. Contents are what drift; identity cannot.
func TestTheQueueScansTheStoresOwnLayerStackKinds(t *testing.T) {
	if len(deriveLayerKinds) == 0 {
		t.Fatal("deriveLayerKinds is empty: GET /derive/pending would scan nothing and report every layer as done")
	}
	if len(deriveLayerKinds) != len(store.LayerStackKinds) {
		t.Fatalf("deriveLayerKinds has %d kind(s) and store.LayerStackKinds has %d: the queue is scanning its own list",
			len(deriveLayerKinds), len(store.LayerStackKinds))
	}
	if unsafe.SliceData(deriveLayerKinds) != unsafe.SliceData(store.LayerStackKinds) {
		t.Error("deriveLayerKinds is a COPY of store.LayerStackKinds, not an alias of it. " +
			"A copy agrees today and diverges the first time a kind is added to store.RowLayerStacks: " +
			"a derive layer authored into the new kind is accepted, projected onto screens and held " +
			"against the content sweep while never being reported to the renderer that would draw it.")
	}
}
