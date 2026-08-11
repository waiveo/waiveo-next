package api

import (
	"encoding/json"
	"net/http"

	"github.com/maaxton/waiveo-next/internal/app/store"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// derive.go serves the RASTERIZED FALLBACK's work queue (parity row 2.4): the
// one read-only operation the appliance owes the off-appliance renderer.
//
// # Why there is only a read here
//
// A `derive` slide layer names something no player node can draw — a gradient, a
// drop shadow, a rounded border, a font the device does not ship, a QR symbol —
// and legacy turned exactly those into PNGs with a headless Chromium. That
// browser is NOT coming to this appliance: the feeder and relay are deliberately
// Go-only and the box already hosts the legacy stack, so the renderer is a
// separate tool (cmd/waiveo-derive) an operator or a dev box runs. The server's
// entire share of the loop is telling that tool what is outstanding; the tool
// then uses operations that already existed — POST /content for the PNG bytes,
// PATCH /casts/{id} for the reference — so this family adds no write, and no
// second content path exists to keep in step with the first.
//
// # Why the queue is COMPUTED, never stored
//
// There is no jobs table behind this. The listing is derived from the authored
// casts on every request, through the same wire.LayerDeriveState the two content
// projections classify a layer with. A stored queue would be a second statement
// of what needs rendering, and the two would disagree the first time a cast was
// edited by anything that forgot to enqueue — the exact shape of defect this
// repo keeps paying for. Computed, a layer leaves the queue the instant its
// asset_ref and derived_from are written back, and rejoins it the instant its
// spec is edited, with nothing to reconcile.

// deriveLayerKinds are the row kinds a derive layer can be authored into, and
// therefore the complete set this queue must scan.
//
// It is BOTH shapes that carry an authored layer stack, and it must stay that
// way: store.RowLayerStacks projects a cast's slides and a `source: "slide"`
// playlist item's inline slide identically, both content projections resolve
// them through the identical resolveLayers (so both really do reach a screen as
// pictures), and store.RowAssetReferences holds a custom font in either against
// the retention sweep. A queue narrower than that set is a surface that ACCEPTS
// work it never performs — an inline-slide derive layer was accepted, projected,
// and protected from the sweep while never once being reported to the tool that
// would draw it, which is the shape this file's own header calls out.
var deriveLayerKinds = []store.Kind{store.KindCast, store.KindPlaylist}

// deriveSourceFor names the kind on the wire. The kinds' own storage names
// (`casts`, `playlists`) are not the vocabulary the api publishes, so the
// mapping is stated once here rather than spelled at each append.
func deriveSourceFor(kind store.Kind) string {
	if kind == store.KindPlaylist {
		return "playlist"
	}
	return "cast"
}

// derivePendingLayer is one outstanding rasterization, and it is deliberately a
// COMPLETE work order: what to draw (Spec, at W×H) and where the result belongs.
// A tool that had to re-read the row to learn the geometry could read a version
// that had changed in between and render at the wrong size.
//
// WHERE is two shapes because a derive layer lives in two: a cast slide is
// addressed by its document-local SlideID, and a playlist's inline slide by the
// ItemIndex of the item that carries it (datamodel.Slide has no id of its own).
// Source says which, and exactly one of the two locators is present — a single
// stringly-typed locator would have to be parsed by every consumer.
type derivePendingLayer struct {
	Source       string           `json:"source"`
	ResourceID   string           `json:"resource_id"`
	ResourceName string           `json:"resource_name,omitempty"`
	SlideID      string           `json:"slide_id,omitempty"`
	ItemIndex    *int             `json:"item_index,omitempty"`
	LayerIndex   int              `json:"layer_index"`
	State        string           `json:"state"`
	SpecDigest   string           `json:"spec_digest"`
	W            int              `json:"w"`
	H            int              `json:"h"`
	Spec         *wire.DeriveSpec `json:"spec"`
}

// listPendingDerives handles GET /api/v1/derive/pending.
//
// It reads every row the caller may READ — the same visible-set filter the
// generic list applies (scopeview.go), never a wider one: a derive work order
// carries the authored spec verbatim, so answering with layers from rows outside
// the principal's readable set would leak authored content through a side door
// the resource list closes.
func (srv *server) listPendingDerives(w http.ResponseWriter, r *http.Request) {
	view, err := srv.scopeView(r)
	if err != nil {
		writeProblem(w, r, http.StatusInternalServerError, "INTERNAL", "Internal Server Error", "An unexpected server error occurred.")
		return
	}

	out := make([]derivePendingLayer, 0)
	for _, kind := range deriveLayerKinds {
		rows, err := srv.store.List(r.Context(), kind, store.ListFilter{})
		if err != nil {
			writeProblem(w, r, http.StatusInternalServerError, "INTERNAL", "Internal Server Error", "An unexpected server error occurred.")
			return
		}
		for _, res := range rows {
			fields := parseFields(res.Body)
			if !view.canRead(fields.ScopeNode) {
				continue
			}
			stacks, err := store.RowLayerStacks(kind, res.Body)
			if err != nil {
				// A stored row that no longer decodes is a store-level defect, not a
				// reason to fail the whole listing: the other rows' outstanding work
				// is still true and still actionable.
				continue
			}
			var named struct {
				Name string `json:"name"`
			}
			_ = json.Unmarshal(res.Body, &named)
			for _, stack := range stacks {
				for i, l := range stack.Layers {
					state := wire.LayerDeriveState(l)
					if state == wire.DeriveCurrent {
						continue
					}
					job := derivePendingLayer{
						Source:       deriveSourceFor(kind),
						ResourceID:   fields.ID,
						ResourceName: named.Name,
						SlideID:      stack.SlideID,
						LayerIndex:   i,
						State:        state.String(),
						SpecDigest:   wire.DeriveDigest(l),
						W:            l.W,
						H:            l.H,
						Spec:         l.Derive,
					}
					if stack.ItemIndex >= 0 {
						idx := stack.ItemIndex
						job.ItemIndex = &idx
					}
					out = append(out, job)
				}
			}
		}
	}

	writeJSONValue(w, http.StatusOK, map[string]any{"derive_jobs": out})
}
