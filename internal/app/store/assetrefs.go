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

// LayerStackKinds are the row kinds whose bodies can carry an authored SLIDE
// LAYER STACK, and therefore the complete set every whole-workspace consumer of
// layers (the derive work queue, first among them) must scan.
//
// It is the companion to AssetBearingKinds and it exists for the same reason,
// paid for the same way. RowLayerStacks was introduced as "the ONE enumeration"
// of the two authored layer shapes — and then GET /derive/pending spelled its
// own hand-maintained list of kinds to call it with, which is a SECOND
// enumeration bound to nothing. Two lists is how one blind spot stays one blind
// spot in two places, which is the defect this whole file was written to end.
//
// Adding a kind to RowLayerStacks' switch and forgetting this list is caught by
// TestEveryLayerStackKindIsEnumerated, which drives the switch itself rather
// than trusting either list.
var LayerStackKinds = []Kind{KindCast, KindPlaylist}

// LayerStack is ONE authored stack of slide layers found inside a stored row,
// together with enough of its location to write back into it.
//
// It exists because a layer stack is not a cast-only shape and never was: a
// `source: "slide"` playlist item carries one inline (datamodel.Slide), the two
// content projections run BOTH through the identical resolveLayers, and every
// consumer that walks layers has to walk both or silently ignore half the
// authored layers in a workspace. That silence is what shipped: the retention
// sweep and the write-time guard read the inline shape (layerAssetReferences,
// via RowAssetReferences) while GET /derive/pending scanned casts only — so a
// `derive` layer in an inline slide was accepted, projected and protected from
// the sweep, and could never be reported to the renderer that would draw it.
//
// One enumeration serves both now. A new row kind, or a new nesting of a layer
// stack inside an existing one, is added HERE and every consumer sees it.
type LayerStack struct {
	// Field is the JSON path of the object that owns the stack, within the row
	// body: `slides[3]` for a cast slide, `items[2].slide` for a playlist item's
	// inline slide. It is the prefix every reference path under the stack is
	// built from, and it is what an error message points an operator at.
	Field string
	// SlideID is a cast slide's document-local id. It is EMPTY for an inline
	// playlist slide, which carries no id at all (datamodel.Slide is layers and
	// nothing else) — a caller that needs to address one uses ItemIndex.
	SlideID string
	// ItemIndex is the index of the owning playlist item, or -1 when the stack
	// is a cast slide's. Together with SlideID it says which of the two shapes
	// this is, without a second discriminator to keep in step.
	ItemIndex int
	// Layers is the authored stack itself, in z-order.
	Layers []wire.Layer
}

// castSlideStack is the ItemIndex a cast slide's stack carries: a cast slide
// belongs to no playlist item, and -1 is an index no items array can produce.
const castSlideStack = -1

// RowLayerStacks returns, in document order, every authored slide-layer stack
// the body of a row of the given kind carries. A kind that carries none returns
// nothing, with no error, for the same reason RowAssetReferences does.
//
// A body that will not decode is an ERROR rather than an empty answer — see
// RowAssetReferences, whose callers make that decision deliberately and
// differently.
func RowLayerStacks(kind Kind, body []byte) ([]LayerStack, error) {
	switch kind {
	case KindPlaylist:
		var pl struct {
			Items []datamodel.PlaylistItem `json:"items"`
		}
		if err := json.Unmarshal(body, &pl); err != nil {
			return nil, fmt.Errorf("store: decode a %s row for its layer stacks: %w", kind, err)
		}
		var out []LayerStack
		for i, it := range pl.Items {
			if it.Slide == nil {
				continue
			}
			out = append(out, LayerStack{
				Field:     fmt.Sprintf("items[%d].slide", i),
				ItemIndex: i,
				Layers:    it.Slide.Layers,
			})
		}
		return out, nil
	case KindCast:
		var c struct {
			Slides []datamodel.CastSlide `json:"slides"`
		}
		if err := json.Unmarshal(body, &c); err != nil {
			return nil, fmt.Errorf("store: decode a %s row for its layer stacks: %w", kind, err)
		}
		var out []LayerStack
		for i, s := range c.Slides {
			out = append(out, LayerStack{
				Field:     fmt.Sprintf("slides[%d]", i),
				SlideID:   s.ID,
				ItemIndex: castSlideStack,
				Layers:    s.Layers,
			})
		}
		return out, nil
	default:
		return nil, nil
	}
}

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
	// Every LAYER reference — a cast slide's, and a `source: "slide"` playlist
	// item's inline stack — comes from the one shared enumeration (RowLayerStacks),
	// so the two shapes cannot be projected here and forgotten by the derive
	// queue, or the reverse. That reverse is exactly what shipped once.
	stacks, err := RowLayerStacks(kind, body)
	if err != nil {
		return nil, err
	}
	if kind != KindPlaylist {
		var refs []AssetReference
		for _, st := range stacks {
			refs = append(refs, layerAssetReferences(st.Field, st.Layers)...)
		}
		return refs, nil
	}

	// A playlist carries BOTH shapes, and they are emitted interleaved in
	// document order: an item's own asset_ref, then that item's inline layers.
	var pl struct {
		Items []datamodel.PlaylistItem `json:"items"`
	}
	if err := json.Unmarshal(body, &pl); err != nil {
		return nil, fmt.Errorf("store: decode a %s row for its asset references: %w", kind, err)
	}
	var refs []AssetReference
	next := 0
	for i, it := range pl.Items {
		if it.AssetRef != "" {
			refs = append(refs, AssetReference{
				Field: fmt.Sprintf("items[%d].asset_ref", i),
				Ref:   it.AssetRef,
			})
		}
		for next < len(stacks) && stacks[next].ItemIndex == i {
			refs = append(refs, layerAssetReferences(stacks[next].Field, stacks[next].Layers)...)
			next++
		}
	}
	return refs, nil
}

// layerAssetReferences projects one authored layer stack, prefixing each path
// with the stack's own location in its row (`slides[3]`, `items[0].slide`) so
// the reported field is a path a client can follow back to the layer it named.
func layerAssetReferences(prefix string, layers []wire.Layer) []AssetReference {
	var refs []AssetReference
	for i, l := range layers {
		if l.AssetRef != "" {
			refs = append(refs, AssetReference{
				Field: fmt.Sprintf("%s.layers[%d].asset_ref", prefix, i),
				Ref:   l.AssetRef,
			})
		}
		// A `derive` layer's CUSTOM FONT is a second reference into the same
		// content origin (wire.DeriveSpec.FontAssetRef), and it is projected here
		// for both of the reasons the layer's own asset_ref is: a font that was
		// never uploaded must be refused at write time rather than leaving the
		// layer permanently un-renderable with nothing saying why, and a font a
		// cast still names must not be reclaimed by the content retention sweep.
		// That second half is the one this projection's blind spots have already
		// cost: a cast's images were once invisible here, which made them
		// simultaneously unchecked at write time AND sweepable while a screen was
		// playing them.
		if l.Derive != nil && l.Derive.FontAssetRef != "" {
			refs = append(refs, AssetReference{
				Field: fmt.Sprintf("%s.layers[%d].derive.font_asset_ref", prefix, i),
				Ref:   l.Derive.FontAssetRef,
			})
		}
	}
	return refs
}
