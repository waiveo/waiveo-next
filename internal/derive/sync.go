package derive

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"

	"github.com/maaxton/waiveo-next/internal/datamodel"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// sync.go is the whole loop, in one place: find the outstanding layers, render
// them, upload the PNGs, and write the references back.
//
// # Why the listing is only used to pick ROWS
//
// GET /derive/pending answers with a full work order — the spec and the geometry
// — and this code deliberately does NOT render from it. It renders from the row
// it then reads for itself.
//
// The reason is the gap between the two calls. A render takes seconds; an
// operator editing in the Studio takes less. If the tool rendered the LISTED
// spec and wrote the result back onto whatever the layer had become, it would
// stamp a PNG of the old spec with a digest claiming it matched the new one —
// and the layer would then read as CURRENT forever while showing the wrong
// picture, which is worse than never rendering it at all. Reading the row and
// rendering what is actually stored, then writing under that read's If-Match,
// makes the render and the write agree or fail; it cannot half-succeed.
//
// The listed order is still what the tool reports on, so an operator who watched
// the queue sees every job they were shown accounted for.
//
// # Why there are two row kinds
//
// A derive layer lives in either shape that carries an authored layer stack: a
// cast slide, or a `source: "slide"` playlist item's inline slide. Both are
// rewritten into an ordinary `image` by the same projection, so both really do
// reach a screen — and a tool that handled only one would leave the other
// accepted, queued and never drawn.
//
// # Why the renders run at once
//
// -concurrency sizes the Runner's clamp, and a loop that rendered strictly
// sequentially would make that flag a control an operator sets and nothing
// obeys. Every outstanding layer in the whole pass goes through ONE pool sized
// by the Runner's own clamp (r.Concurrency()) — not a pool per row, which would
// leave a workspace of single-layer casts serial for the same reason.
//
// # Why identical layers are rendered ONCE
//
// wire.DeriveDigest is the identity of a layer's PIXELS: same spec, same
// geometry, same picture. Two layers that share one are therefore two requests
// for the same PNG — and the digest is also Runner.breaker's key, so rendering
// them as two units made the pass non-deterministic in a way that mattered.
// Two concurrent renders of one key each recorded a failure, so a failing layer
// backed off twice as fast as the schedule says; and which of the two errors a
// unit reported depended on which browser finished first, so the same workspace
// produced different reports run to run. Rendering each distinct digest once and
// fanning the one answer out to every unit that named it makes the report a
// function of the input alone, charges the breaker once per attempt as designed,
// and — incidentally, not as the reason — does not launch a browser twice for
// one picture.

// JobOutcome is what happened to one layer.
type JobOutcome struct {
	// Source is `cast` or `playlist` — which authored shape the layer lives in.
	Source string
	// ResourceID is the cast or playlist row's id, and ResourceName its name.
	ResourceID   string
	ResourceName string
	// Where names the layer's place inside that row in the queue's own grammar:
	// `slide poster` for a cast slide, `item 2` for a playlist item.
	Where      string
	LayerIndex int
	AssetRef   string
	Digest     string
	Bytes      int
	// Err is nil on success. A per-job error never stops the run: one layer that
	// cannot render must not hold up the ones that can, which is the same reason
	// the projections drop a single underived layer rather than the slide.
	Err error
}

// RowError is one ROW the pass could not complete: it could not be read, or its
// write-back was refused after its layers rendered. Both are per-row and neither
// is a per-layer fault, which is why they share a channel — and the commonest by
// far is a 409 because a human edited the row mid-run, which is the precondition
// working rather than anything being broken.
//
// It is a slice rather than a map so a run reports its failures in the same
// stable order twice.
type RowError struct {
	Source     string
	ResourceID string
	Err        error
}

// SyncReport is the run's result.
type SyncReport struct {
	Listed   int
	Rendered []JobOutcome
	Failed   []JobOutcome
	// RowsRead is how many of the rows the queue named were actually read. A run
	// that listed work and read nothing looks identical to a run with nothing to
	// do unless this is reported.
	RowsRead int
	RowErrs  []RowError
}

// rowRef names one authored row the run must read and write back.
type rowRef struct {
	Source string
	ID     string
}

// authoredRow is one row the tool has READ: the decoded document, its ETag, and
// the layer stacks inside it. Exactly one of cast/playlist is set.
//
// It exists so the rest of the loop never branches on the row kind again. Both
// shapes answer the same three questions — what layers do you carry, has
// anything changed, and how do I write you back — and a loop that asked those
// with an `if source == "cast"` at every step is how the two halves drift.
type authoredRow struct {
	ref     rowRef
	name    string
	etag    string
	cast    *datamodel.Cast
	list    *datamodel.Playlist
	changed bool
}

// stacks returns the row's authored layer stacks as MUTABLE views, paired with
// the locator the queue names each by. The slices alias the decoded document, so
// writing a reference into one is writing it into the row that gets patched —
// there is no second copy to forget to copy back.
func (r *authoredRow) stacks() []layerStack {
	var out []layerStack
	if r.cast != nil {
		for i := range r.cast.Slides {
			out = append(out, layerStack{
				where:  "slide " + r.cast.Slides[i].ID,
				layers: r.cast.Slides[i].Layers,
			})
		}
		return out
	}
	for i := range r.list.Items {
		if r.list.Items[i].Slide == nil {
			continue
		}
		out = append(out, layerStack{
			where:  fmt.Sprintf("item %d", i),
			layers: r.list.Items[i].Slide.Layers,
		})
	}
	return out
}

// patch writes the row back under its read's If-Match.
func (r *authoredRow) patch(ctx context.Context, c *Client) error {
	if r.cast != nil {
		return c.PatchCastSlides(ctx, r.ref.ID, r.cast.Slides, r.etag)
	}
	return c.PatchPlaylistItems(ctx, r.ref.ID, r.list.Items, r.etag)
}

// layerStack is one addressable stack of authored layers inside a read row.
type layerStack struct {
	where  string
	layers []wire.Layer
}

// renderUnit is one layer this pass will render: where it came from, and where
// the answer goes back.
type renderUnit struct {
	row     *authoredRow
	layers  []wire.Layer
	index   int
	outcome JobOutcome
	// png is the rendered image, set by the concurrent phase and consumed by the
	// serial apply phase. Each unit is written by exactly one worker and read
	// only after that phase has joined, so it needs no lock of its own.
	png []byte
	err error
}

// Sync runs one pass: everything outstanding, once. It is deliberately not a
// daemon. The renderer is an off-appliance tool an operator or a CI step
// invokes; a resident service would need a supervisor, a health surface and a
// story for running two of them, none of which this loop needs to be correct.
func Sync(ctx context.Context, c *Client, r *Runner) (SyncReport, error) {
	rep := SyncReport{}

	listed, err := c.ListPending(ctx)
	if err != nil {
		return rep, err
	}
	rep.Listed = len(listed)
	if len(listed) == 0 {
		return rep, nil
	}

	// One pass per row, in a stable order, and ONE write per row. A write per
	// layer would invalidate its own If-Match on the second one.
	refs := make([]rowRef, 0, len(listed))
	seen := map[rowRef]bool{}
	for _, j := range listed {
		ref := rowRef{Source: j.Source, ID: j.ResourceID}
		if ref.Source == "" {
			// A queue entry that does not say which shape it is in cannot be read
			// back, and guessing `cast` would write a playlist's raster onto a cast
			// that may not exist. Reported, not assumed.
			rep.RowErrs = append(rep.RowErrs, RowError{
				ResourceID: j.ResourceID,
				Err:        fmt.Errorf("derive: the queue entry for %s names no source", j.ResourceID),
			})
			continue
		}
		if !seen[ref] {
			seen[ref] = true
			refs = append(refs, ref)
		}
	}
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].Source != refs[j].Source {
			return refs[i].Source < refs[j].Source
		}
		return refs[i].ID < refs[j].ID
	})

	// ── Read every row, and collect every layer this pass owes ────────────────
	rows := make([]*authoredRow, 0, len(refs))
	var units []*renderUnit
	for _, ref := range refs {
		row, err := readRow(ctx, c, ref)
		if err != nil {
			rep.RowErrs = append(rep.RowErrs, RowError{Source: ref.Source, ResourceID: ref.ID, Err: err})
			continue
		}
		rep.RowsRead++
		rows = append(rows, row)
		for _, stack := range row.stacks() {
			for li := range stack.layers {
				if wire.LayerDeriveState(stack.layers[li]) == wire.DeriveCurrent {
					continue
				}
				units = append(units, &renderUnit{
					row:    row,
					layers: stack.layers,
					index:  li,
					outcome: JobOutcome{
						Source:       row.ref.Source,
						ResourceID:   row.ref.ID,
						ResourceName: row.name,
						Where:        stack.where,
						LayerIndex:   li,
						Digest:       wire.DeriveDigest(stack.layers[li]),
					},
				})
			}
		}
	}

	// ── Fetch the custom faces, once each, before anything renders ────────────
	fonts := fetchFonts(ctx, c, units)

	// ── Render, bounded by the Runner's own clamp ─────────────────────────────
	renderUnits(ctx, r, units, fonts)

	// ── Apply in LISTED order, then one write per row ─────────────────────────
	//
	// Serial and ordered on purpose: two units in the same row write into the
	// same slice, and a report whose order depended on which browser finished
	// first would be a different answer every run.
	//
	// Identical pictures upload once, for the same reason they render once: the
	// origin is content-addressed, so the second POST of the same bytes can only
	// mint the same reference, and a per-unit attempt would additionally let two
	// units that named ONE picture disagree about whether it uploaded.
	type uploaded struct {
		ref string
		err error
	}
	uploads := map[string]uploaded{}
	for _, u := range units {
		if u.err != nil {
			u.outcome.Err = u.err
			rep.Failed = append(rep.Failed, u.outcome)
			continue
		}
		up, done := uploads[u.outcome.Digest]
		if !done {
			up.ref, up.err = c.UploadContent(ctx, u.png)
			uploads[u.outcome.Digest] = up
		}
		ref, err := up.ref, up.err
		if err != nil {
			u.outcome.Err = err
			rep.Failed = append(rep.Failed, u.outcome)
			continue
		}
		u.outcome.AssetRef, u.outcome.Bytes = ref, len(u.png)
		if err := ApplyRendered(u.layers, u.index, ref, u.outcome.Digest); err != nil {
			u.outcome.Err = err
			rep.Failed = append(rep.Failed, u.outcome)
			continue
		}
		u.row.changed = true
		rep.Rendered = append(rep.Rendered, u.outcome)
	}

	for _, row := range rows {
		if !row.changed {
			continue
		}
		if err := row.patch(ctx, c); err != nil {
			rep.RowErrs = append(rep.RowErrs, RowError{Source: row.ref.Source, ResourceID: row.ref.ID, Err: err})
		}
	}

	return rep, nil
}

// readRow reads one authored row of either shape.
func readRow(ctx context.Context, c *Client, ref rowRef) (*authoredRow, error) {
	switch ref.Source {
	case SourceCast:
		cast, etag, err := c.GetCast(ctx, ref.ID)
		if err != nil {
			return nil, err
		}
		return &authoredRow{ref: ref, name: cast.Name, etag: etag, cast: &cast}, nil
	case SourcePlaylist:
		list, etag, err := c.GetPlaylist(ctx, ref.ID)
		if err != nil {
			return nil, err
		}
		return &authoredRow{ref: ref, name: list.Name, etag: etag, list: &list}, nil
	default:
		return nil, fmt.Errorf("derive: %q is not an authored shape this tool can read", ref.Source)
	}
}

// fetchFonts resolves every distinct custom face the pass needs, ONCE each and
// before the concurrent phase.
//
// Serial and up front rather than lazily inside the workers: a shared cache
// filled from N goroutines needs a lock and still lets two workers fetch the
// same face at once, and the fetch is a small download next to a browser
// launch. A face that cannot be fetched is recorded as an error against its
// reference, so every layer that names it fails with the real reason instead of
// rendering in a fallback face nobody asked for.
func fetchFonts(ctx context.Context, c *Client, units []*renderUnit) map[string]fontResult {
	fonts := map[string]fontResult{}
	for _, u := range units {
		spec := u.layers[u.index].Derive
		if spec == nil || spec.FontAssetRef == "" {
			continue
		}
		if _, ok := fonts[spec.FontAssetRef]; ok {
			continue
		}
		data, err := c.FetchAsset(ctx, spec.FontAssetRef)
		if err != nil {
			fonts[spec.FontAssetRef] = fontResult{err: fmt.Errorf("custom font %s: %w", spec.FontAssetRef, err)}
			continue
		}
		fonts[spec.FontAssetRef] = fontResult{data: data}
	}
	return fonts
}

// fontResult is one fetched face, or the reason it could not be fetched.
type fontResult struct {
	data []byte
	err  error
}

// renderUnits renders every DISTINCT picture the pass needs, r.Concurrency() at
// a time, and hands each answer to every unit that asked for it.
//
// The pool is sized from the Runner rather than from a second setting, so
// -concurrency has exactly ONE meaning: the Runner's clamp still bounds any
// other caller, and this pool never asks for more than that clamp would grant.
//
// The unit of work is the DIGEST, not the unit — see the file header. Groups are
// built in first-appearance order so the pool is fed in the order the rows were
// listed, which is what makes two runs over an unchanged workspace schedule the
// same work in the same order.
func renderUnits(ctx context.Context, r *Runner, units []*renderUnit, fonts map[string]fontResult) {
	if len(units) == 0 {
		return
	}

	// Distinct digests, in first-appearance order, each with the units waiting
	// on it.
	var keys []string
	groups := map[string][]*renderUnit{}
	for _, u := range units {
		k := u.outcome.Digest
		if _, seen := groups[k]; !seen {
			keys = append(keys, k)
		}
		groups[k] = append(groups[k], u)
	}

	workers := r.Concurrency()
	if workers < 1 {
		workers = 1
	}
	if workers > len(keys) {
		workers = len(keys)
	}

	queue := make(chan string)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for k := range queue {
				group := groups[k]
				png, err := renderGuarded(ctx, r, group[0].layers[group[0].index], k, fonts)
				for _, u := range group {
					u.png, u.err = png, err
				}
			}
		}()
	}
	for _, k := range keys {
		queue <- k
	}
	close(queue)
	wg.Wait()
}

// renderGuarded is renderOne with a recover() around it, and it is the reason a
// malformed row cannot cost this pass anything but its own layer.
//
// The panic that earned it was real and its blast radius was the whole run: a
// `derive` layer with no spec (accepted by the inline authoring path, which ran
// no layer validation at all) reached renderOne, which dereferenced the nil spec
// — and because the pass renders EVERYTHING before it uploads, applies and
// writes back, the process died holding every other layer's finished PNG. One
// bad row in one playlist discarded the completed work of every other row.
//
// renderOne now refuses that layer outright, so this guard is not the fix for
// it; it is the answer to the class. A renderer is a browser driver over
// third-party bytes, and a panic anywhere under it — here, in the page builder,
// in an image encoder — must cost the one unit that provoked it and nothing
// else. A per-unit failure is already a shape this loop reports and continues
// past, so the recover simply routes into it.
func renderGuarded(ctx context.Context, r *Runner, layer wire.Layer, digest string, fonts map[string]fontResult) (png []byte, err error) {
	defer func() {
		if p := recover(); p != nil {
			png = nil
			err = fmt.Errorf("derive: rendering %s panicked: %v", digest, p)
		}
	}()
	return renderOne(ctx, r, layer, digest, fonts)
}

// renderOne renders one layer to PNG bytes. It does NOT upload: the upload is a
// write against the server and belongs in the serial phase, where its order and
// its errors are the same on every run.
func renderOne(ctx context.Context, r *Runner, layer wire.Layer, digest string, fonts map[string]fontResult) ([]byte, error) {
	// A derive layer with no spec names nothing to draw. It is refused here
	// rather than dereferenced, and rather than being quietly skipped: the layer
	// IS outstanding, an operator asked for something, and reporting the row and
	// the reason is the only way they learn the layer is malformed. fetchFonts
	// five lines up already made exactly this check — the guard was written on
	// one side of a pair, which is how the nil reached the deref at all.
	if layer.Derive == nil {
		return nil, fmt.Errorf("derive: layer carries no spec, so there is nothing to render (digest %q)", digest)
	}
	job := Job{Key: digest, W: layer.W, H: layer.H, Spec: layer.Derive}
	if ref := layer.Derive.FontAssetRef; ref != "" {
		f, ok := fonts[ref]
		if !ok {
			return nil, fmt.Errorf("custom font %s was never fetched", ref)
		}
		if f.err != nil {
			return nil, f.err
		}
		job.FontData = f.data
	}
	return r.Render(ctx, job)
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
//
// It takes the LAYER STACK rather than a document, because the two authored
// shapes that carry one — a cast slide and a playlist item's inline slide —
// must be written back through the identical code.
func ApplyRendered(layers []wire.Layer, layerIdx int, assetRef, digest string) error {
	if layerIdx < 0 || layerIdx >= len(layers) {
		return fmt.Errorf("derive: layer index %d is outside a stack of %d", layerIdx, len(layers))
	}
	if assetRef == "" || digest == "" {
		return fmt.Errorf("derive: a rendered layer needs both an asset_ref and the digest it was rendered from, got %q/%q", assetRef, digest)
	}
	layers[layerIdx].AssetRef = assetRef
	layers[layerIdx].DerivedFrom = digest
	return nil
}
