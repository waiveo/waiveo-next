package snapshot

import (
	"embed"
	"fmt"
)

// demoCastAssets embeds this package's committed testdata/demo PNGs — real,
// distinct, decodable images (testdata/demo/gen.go documents how they were
// generated: solid-gradient backgrounds with a numeral drawn from a small
// hand-rolled bitmap font, stdlib image/png only, no external deps) — into
// the feeder binary, so a box deployment (which ships only the built binary,
// not this repo's source tree) can still seed the multi-item demo cast
// without a runtime file-path dependency.
//
//go:embed testdata/demo/demo-1.png testdata/demo/demo-2.png testdata/demo/demo-3.png
var demoCastAssets embed.FS

// demoCastItemDurationMS is the per-item display-duration DemoCastItems
// assigns each of its three images (REL-061a `duration_ms`) — a reasonable
// signage dwell time, not a value any contract or downstream consumer
// currently enforces (no playback-advance driver reads it yet, ContentRef's
// own doc comment).
const demoCastItemDurationMS = 8000

// demoCastAssetPaths is DemoCastItems' fixed, ordered source list — the
// exact play order the returned []CastItem carries (BuildCast preserves
// caller order into `content`, REL-061, and store.SeedDemo seeds one
// playlist item per asset ref in the same order).
var demoCastAssetPaths = []string{
	"testdata/demo/demo-1.png",
	"testdata/demo/demo-2.png",
	"testdata/demo/demo-3.png",
}

// DemoCastItems returns this package's real, committed 3-image multi-item
// demo cast, ready to pass to BuildCast (or, on the store-driven path, to
// have its asset refs seeded as the demo playlist via store.SeedDemo) — each item
// content_type "image", duration_ms demoCastItemDurationMS. A caller MUST
// still Add each item's Bytes to a content origin (internal/feeder/origin)
// before building a snapshot from these items, exactly as it must for any
// CastItem (CastItem's own doc comment).
//
// KNOWN GAP, stated plainly rather than worked around: this cast ships
// images only. A genuinely valid, playable MP4 could not be produced with
// zero external dependencies and no cgo in pure Go within this package's
// scope — every practical MP4 video track (H.264/AVC, or even Motion-JPEG)
// requires hand-encoding a real codec bitstream and a conformant sample
// table, which the Go standard library has no support for and which a
// hand-rolled implementation could easily produce a file that LOOKS
// container-shaped but is not genuinely decodable — worse than shipping
// none. Rather than fabricate a file that claims to be video but is not,
// this package ships three real images and documents the gap here. The
// content_type/duration_ms plumbing itself (CastItem, ContentRef,
// SetServedProgram's per-item type) is content-agnostic and already
// exercised end-to-end against a `video`-typed fixture item in this
// package's own tests (TestBuildCastOrderedMultiItemEachIndependentlyVerifiable)
// and internal/relay/playerserver's
// (TestSetServedProgramCarriesPerItemContentType) — only a REAL demo video
// asset is the open gap, not the mechanism that would carry one.
func DemoCastItems() ([]CastItem, error) {
	items := make([]CastItem, 0, len(demoCastAssetPaths))
	for _, path := range demoCastAssetPaths {
		b, err := demoCastAssets.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("snapshot: DemoCastItems: read embedded %s: %w", path, err)
		}
		items = append(items, CastItem{
			Bytes:       b,
			ContentType: "image",
			DurationMS:  demoCastItemDurationMS,
		})
	}
	return items, nil
}
