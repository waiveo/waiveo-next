package api

import (
	"encoding/json"
	"net/http"

	"github.com/maaxton/waiveo-next/internal/app/store"
	"github.com/maaxton/waiveo-next/internal/datamodel"
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

// derivePendingLayer is one outstanding rasterization, and it is deliberately a
// COMPLETE work order: what to draw (Spec, at W×H) and where the result belongs
// (CastID, SlideID, LayerIndex). A tool that had to re-read the cast to learn
// the geometry could read a version that had changed in between and render at
// the wrong size.
type derivePendingLayer struct {
	CastID     string           `json:"cast_id"`
	CastName   string           `json:"cast_name,omitempty"`
	SlideID    string           `json:"slide_id"`
	LayerIndex int              `json:"layer_index"`
	State      string           `json:"state"`
	SpecDigest string           `json:"spec_digest"`
	W          int              `json:"w"`
	H          int              `json:"h"`
	Spec       *wire.DeriveSpec `json:"spec"`
}

// listPendingDerives handles GET /api/v1/derive/pending.
//
// It reads every cast row the caller may READ — the same visible-set filter the
// generic list applies (scopeview.go), never a wider one: a derive work order
// carries the authored spec verbatim, so answering with layers from casts
// outside the principal's readable set would leak authored content through a
// side door the resource list closes.
func (srv *server) listPendingDerives(w http.ResponseWriter, r *http.Request) {
	rows, err := srv.store.List(r.Context(), store.KindCast, store.ListFilter{})
	if err != nil {
		writeProblem(w, r, http.StatusInternalServerError, "INTERNAL", "Internal Server Error", "An unexpected server error occurred.")
		return
	}
	view, err := srv.scopeView(r)
	if err != nil {
		writeProblem(w, r, http.StatusInternalServerError, "INTERNAL", "Internal Server Error", "An unexpected server error occurred.")
		return
	}

	out := make([]derivePendingLayer, 0)
	for _, res := range rows {
		if !view.canRead(parseFields(res.Body).ScopeNode) {
			continue
		}
		var c datamodel.Cast
		if err := json.Unmarshal(res.Body, &c); err != nil {
			// A stored row that no longer decodes is a store-level defect, not a
			// reason to fail the whole listing: the other casts' outstanding work
			// is still true and still actionable.
			continue
		}
		for _, slide := range c.Slides {
			for i, l := range slide.Layers {
				state := wire.LayerDeriveState(l)
				if state == wire.DeriveCurrent {
					continue
				}
				out = append(out, derivePendingLayer{
					CastID:     c.ID,
					CastName:   c.Name,
					SlideID:    slide.ID,
					LayerIndex: i,
					State:      state.String(),
					SpecDigest: wire.DeriveDigest(l),
					W:          l.W,
					H:          l.H,
					Spec:       l.Derive,
				})
			}
		}
	}

	writeJSONValue(w, http.StatusOK, map[string]any{"derive_jobs": out})
}
