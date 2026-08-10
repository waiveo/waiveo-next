package store

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/maaxton/waiveo-next/internal/datamodel"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// assetrefs.go is the ONE answer to "which content-origin assets does this
// stored row name?".
//
// Three separate subsystems ask that question about the same rows, and each of
// them is wrong in a different, expensive way if its answer is smaller than the
// truth:
//
//   - the api's authoring guard (internal/app/api, validate + writeGuards) —
//     a reference it does not see is a row it accepts naming bytes the origin
//     cannot serve, so the screen shows a 404 where the operator was told 201;
//   - the content retention sweep (WithContentReferences, consumed by
//     internal/feeder/contentgc) — a reference it does not see is content it
//     PERMANENTLY DELETES while a screen is playing it;
//   - the workspace export's asset manifest (internal/app/api/workspacerun.go) —
//     a reference it does not see is an archive that fails ARC-064's
//     MANIFEST_INVALID on restore, or restores image-less.
//
// They were three hand-written projections, and they disagreed: all three read
// a playlist item's `asset_ref` and NONE of them read the image layers of a
// cast's slides or of an inline `source: "slide"` item — the surfaces that
// carry most of the images in a signage workspace. That is one blind spot, and
// three copies is how it stayed one blind spot in three places. There is now
// exactly one projection, and a new asset-bearing shape is added to it once.
//
// The rule for what counts as a reference is deliberately WIDER than the
// authoring validator's notion of a well-formed image layer: ANY non-empty
// `asset_ref`, on any layer kind, is a reference. A layer kind that does not
// need one should not carry one — wire.ValidateAuthoredSlideLayers has opinions
// about that, and they are enforced where they belong — but the two consumers
// that can destroy something (the sweep and the export) must err toward keeping
// bytes, and the consumer that refuses a write must refuse a reference it
// cannot resolve rather than quietly ignore it because the layer's kind did not
// look image-shaped.

// AssetReference is one content-origin reference a stored row names: the
// `asset_ref` value itself, and the JSON path inside the row body that carries
// it.
//
// Field exists because the api renders a missing reference as an API-013
// per-field error, and "items[2].asset_ref" versus "slides[0].layers[1]
// .asset_ref" is the difference between an operator who can find the broken
// image and one who is told only that something in a twelve-slide cast is
// wrong. The consumers that do not report errors ignore it.
type AssetReference struct {
	// Field is the JSON path of the reference within the row body, e.g.
	// `items[2].asset_ref`, `items[0].slide.layers[1].asset_ref` or
	// `slides[3].layers[0].asset_ref`.
	Field string
	// Ref is the authored value, in the `sha256:<hex>` asset_ref grammar.
	Ref string
}

// HexDigest is the content origin's own key for the reference: the hex digest
// with the `sha256:` prefix stripped (origin.Store.Has/Serve/Remove speak this,
// the row speaks Ref). Deriving it here rather than at each call site is the
// same single-source discipline the rest of this file exists for.
func (r AssetReference) HexDigest() string { return strings.TrimPrefix(r.Ref, "sha256:") }

// AssetBearingKinds are the row kinds whose bodies can name content-origin
// assets, and therefore the complete set every whole-workspace consumer (the
// retention sweep, the export manifest) must read. A kind absent from this list
// contributes nothing to either, which is exactly how a cast's images came to
// be reclaimable while a screen was playing them.
var AssetBearingKinds = []Kind{KindPlaylist, KindCast}

// RowAssetReferences returns, in document order, every content-origin asset the
// body of a row of the given kind names. A kind that cannot carry assets
// returns nothing, with no error: asking is legitimate (a caller iterating
// kinds), and a kind that names no content is not a fault.
//
// A body that will not decode is an ERROR rather than an empty answer. The
// three consumers differ in what they do with that error — the sweep aborts
// (an unreadable row's references reported as absent is how live content gets
// deleted), the export skips the row (an export that drops one row is still an
// export), the api's pre-write check lets the store report the real failure —
// and every one of those is a decision the CALLER has to make deliberately. An
// empty slice would make the dangerous choice for all three, silently.
func RowAssetReferences(kind Kind, body []byte) ([]AssetReference, error) {
	switch kind {
	case KindPlaylist:
		var pl struct {
			Items []datamodel.PlaylistItem `json:"items"`
		}
		if err := json.Unmarshal(body, &pl); err != nil {
			return nil, fmt.Errorf("store: decode a %s row for its asset references: %w", kind, err)
		}
		var refs []AssetReference
		for i, it := range pl.Items {
			if it.AssetRef != "" {
				refs = append(refs, AssetReference{
					Field: fmt.Sprintf("items[%d].asset_ref", i),
					Ref:   it.AssetRef,
				})
			}
			// A `source: "slide"` item's content IS its layer stack, and an
			// image layer inside it names origin bytes exactly as a plain
			// asset item's asset_ref does (DAT-041 discipline). It carries no
			// item-level asset_ref, so a projection that reads only that field
			// sees an inline slide as referencing nothing at all.
			if it.Slide != nil {
				refs = append(refs, layerAssetReferences(
					fmt.Sprintf("items[%d].slide", i), it.Slide.Layers)...)
			}
		}
		return refs, nil
	case KindCast:
		var c struct {
			Slides []datamodel.CastSlide `json:"slides"`
		}
		if err := json.Unmarshal(body, &c); err != nil {
			return nil, fmt.Errorf("store: decode a %s row for its asset references: %w", kind, err)
		}
		var refs []AssetReference
		for i, s := range c.Slides {
			refs = append(refs, layerAssetReferences(
				fmt.Sprintf("slides[%d]", i), s.Layers)...)
		}
		return refs, nil
	default:
		return nil, nil
	}
}

// layerAssetReferences projects one authored layer stack, prefixing each path
// with the stack's own location in its row (`slides[3]`, `items[0].slide`) so
// the reported field is a path a client can follow back to the layer it named.
func layerAssetReferences(prefix string, layers []wire.Layer) []AssetReference {
	var refs []AssetReference
	for i, l := range layers {
		if l.AssetRef == "" {
			continue
		}
		refs = append(refs, AssetReference{
			Field: fmt.Sprintf("%s.layers[%d].asset_ref", prefix, i),
			Ref:   l.AssetRef,
		})
	}
	return refs
}
