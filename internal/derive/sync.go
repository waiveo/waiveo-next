package derive

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/maaxton/waiveo-next/internal/datamodel"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// sync.go is the whole loop, in one place: find the outstanding layers, render
// them, upload the PNGs, and write the references back.
//
// # Why the listing is only used to pick CASTS
//
// GET /derive/pending answers with a full work order — the spec and the geometry
// — and this code deliberately does NOT render from it. It renders from the cast
// it then reads for itself.
//
// The reason is the gap between the two calls. A render takes seconds; an
// operator editing in the Studio takes less. If the tool rendered the LISTED
// spec and wrote the result back onto whatever the layer had become, it would
// stamp a PNG of the old spec with a digest claiming it matched the new one —
// and the layer would then read as CURRENT forever while showing the wrong
// picture, which is worse than never rendering it at all. Reading the cast and
// rendering what is actually stored, then writing under that read's If-Match,
// makes the render and the write agree or fail; it cannot half-succeed.
//
// The listed order is still what the tool reports on, so an operator who watched
// the queue sees every job they were shown accounted for.

// JobOutcome is what happened to one layer.
type JobOutcome struct {
	CastID     string
	CastName   string
	SlideID    string
	LayerIndex int
	AssetRef   string
	Digest     string
	Bytes      int
	// Err is nil on success. A per-job error never stops the run: one layer that
	// cannot render must not hold up the ones that can, which is the same reason
	// the projections drop a single underived layer rather than the slide.
	Err error
}

// SyncReport is the run's result.
type SyncReport struct {
	Listed    int
	Rendered  []JobOutcome
	Failed    []JobOutcome
	CastsRead int
	// PatchErrs holds the casts whose write-back failed after their layers
	// rendered — most often a 409 because a human edited the cast mid-run, which
	// is the precondition working, not a fault.
	PatchErrs map[string]error
}

// Sync runs one pass: everything outstanding, once. It is deliberately not a
// daemon. The renderer is an off-appliance tool an operator or a CI step
// invokes; a resident service would need a supervisor, a health surface and a
// story for running two of them, none of which this loop needs to be correct.
func Sync(ctx context.Context, c *Client, r *Runner) (SyncReport, error) {
	rep := SyncReport{PatchErrs: map[string]error{}}

	listed, err := c.ListPending(ctx)
	if err != nil {
		return rep, err
	}
	rep.Listed = len(listed)
	if len(listed) == 0 {
		return rep, nil
	}

	// One pass per cast, in a stable order, and ONE write per cast. A write per
	// layer would invalidate its own If-Match on the second one.
	castIDs := make([]string, 0, len(listed))
	seen := map[string]bool{}
	for _, j := range listed {
		if !seen[j.CastID] {
			seen[j.CastID] = true
			castIDs = append(castIDs, j.CastID)
		}
	}
	sort.Strings(castIDs)

	fonts := map[string][]byte{}
	for _, castID := range castIDs {
		cast, etag, err := c.GetCast(ctx, castID)
		if err != nil {
			rep.PatchErrs[castID] = err
			continue
		}
		rep.CastsRead++

		changed := false
		for si := range cast.Slides {
			for li := range cast.Slides[si].Layers {
				layer := cast.Slides[si].Layers[li]
				if wire.LayerDeriveState(layer) == wire.DeriveCurrent {
					continue
				}
				out := JobOutcome{
					CastID:     cast.ID,
					CastName:   cast.Name,
					SlideID:    cast.Slides[si].ID,
					LayerIndex: li,
					Digest:     wire.DeriveDigest(layer),
				}
				ref, n, err := renderAndUpload(ctx, c, r, layer, out.Digest, fonts)
				if err != nil {
					out.Err = err
					rep.Failed = append(rep.Failed, out)
					continue
				}
				out.AssetRef, out.Bytes = ref, n
				if err := ApplyRendered(cast.Slides, si, li, ref, out.Digest); err != nil {
					out.Err = err
					rep.Failed = append(rep.Failed, out)
					continue
				}
				changed = true
				rep.Rendered = append(rep.Rendered, out)
			}
		}

		if !changed {
			continue
		}
		if err := c.PatchCastSlides(ctx, cast.ID, cast.Slides, etag); err != nil {
			rep.PatchErrs[cast.ID] = err
		}
	}

	return rep, nil
}

// renderAndUpload renders one layer and stores the PNG, returning the
// server-computed asset_ref.
func renderAndUpload(ctx context.Context, c *Client, r *Runner, layer wire.Layer, digest string, fonts map[string][]byte) (string, int, error) {
	job := Job{Key: digest, W: layer.W, H: layer.H, Spec: layer.Derive}

	if ref := layer.Derive.FontAssetRef; ref != "" {
		data, ok := fonts[ref]
		if !ok {
			var err error
			data, err = c.FetchAsset(ctx, ref)
			if err != nil {
				return "", 0, fmt.Errorf("custom font %s: %w", ref, err)
			}
			fonts[ref] = data
		}
		job.FontData = data
	}

	png, err := r.Render(ctx, job)
	if err != nil {
		return "", 0, err
	}
	ref, err := c.UploadContent(ctx, png)
	if err != nil {
		return "", 0, err
	}
	return ref, len(png), nil
}

// RenderOne is the offline half of the tool: render a single spec to PNG bytes
// with no server involved at all.
//
// It exists because the loop above cannot be exercised without a feeder, and an
// author iterating on a gradient should not need one. It is the same Runner, the
// same guards and the same page builder — not a second rendering path — so what
// it produces is byte-identical to what a sync run would upload for the same
// spec and geometry.
func RenderOne(ctx context.Context, r *Runner, spec *wire.DeriveSpec, w, h int, font []byte) ([]byte, error) {
	if spec == nil {
		return nil, errors.New("derive: no spec")
	}
	layer := wire.Layer{Kind: wire.LayerKindDerive, W: w, H: h, Derive: spec}
	return r.Render(ctx, Job{Key: wire.DeriveDigest(layer), W: w, H: h, Spec: spec, FontData: font})
}

// ApplyRendered stamps one layer with the reference and the digest it was
// rendered FROM. Sync calls it — it is not a parallel path — and it is a named
// function rather than two assignments inline because the PAIR is the invariant
// the whole staleness model rests on, and it fails in both directions: an
// asset_ref without its derived_from reads as permanently stale (re-rendered
// every pass, forever), and a derived_from without its asset_ref is refused by
// the layer gate outright. Writing them in one place that refuses half a pair is
// what makes "you cannot do this by halves" true rather than merely intended.
func ApplyRendered(slides []datamodel.CastSlide, slideIdx, layerIdx int, assetRef, digest string) error {
	if slideIdx < 0 || slideIdx >= len(slides) {
		return fmt.Errorf("derive: slide index %d is outside the cast", slideIdx)
	}
	layers := slides[slideIdx].Layers
	if layerIdx < 0 || layerIdx >= len(layers) {
		return fmt.Errorf("derive: layer index %d is outside slide %q", layerIdx, slides[slideIdx].ID)
	}
	if assetRef == "" || digest == "" {
		return fmt.Errorf("derive: a rendered layer needs both an asset_ref and the digest it was rendered from, got %q/%q", assetRef, digest)
	}
	layers[layerIdx].AssetRef = assetRef
	layers[layerIdx].DerivedFrom = digest
	return nil
}
