package api

import (
	"encoding/json"
	"net/http"

	"github.com/maaxton/waiveo-next/internal/app/screens"
	"github.com/maaxton/waiveo-next/internal/app/store"
)

// screenstatus.go is the api/1 read surface for LIVE screen status (parity row
// 5.8): `GET /api/v1/screen-status`, one entry per authored screen row, joining
// three things a console has to show together and could not previously show at
// all —
//
//   - the authored screen row (its name and placement, from the store);
//   - what the relays have OBSERVED it doing (internal/app/screens);
//   - whether an operator currently has a push-now override on it
//     (screennow.go's own store subsystem).
//
// # Why a list of its own rather than members on `/screens`
//
// `/screens` serves an authored resource: it has a revision, an ETag, and a
// PATCH, and everything in its representation is something somebody wrote.
// Liveness is none of those — it is a cache of what relays reported, it changes
// with no write, and it would make a screen's ETag meaningless (the row would
// "change" every ten seconds without anyone editing it). The device plane's own
// read model is kept separate from `/adopted-devices` for exactly this reason
// and this follows it.
//
// # Why the join happens here and not in the read model
//
// The read model holds only what relays said, keyed by screen_id. It has no
// screen rows, so it cannot answer "which screens exist", which is the half that
// matters most: a screen NO relay has ever mentioned is the most alarming row on
// the page, and a list built from reports alone is silent about exactly it. The
// authored rows drive the list; the reports fill it in; a report naming a screen
// no row exists for is dropped, because there is nothing for an operator to act
// on and a console cannot even name it.
//
// # Scope
//
// Every entry is filtered through the caller's visible set at the screen row's
// own placement, exactly as `/screens` itself is. Liveness is not less
// confidential than the row it describes — "the lobby screen at the other site
// went dark an hour ago" is precisely the kind of fact scoping exists to
// contain.

// screenStatusResourceType is the api/1 resource-type tag this list's cursor is
// scoped by (API-033: a cursor carries no meaning across resource types).
const screenStatusResourceType = "screen-status"

// screenStatusRow is one entry of the response (openapi ScreenStatus).
//
// Ages are milliseconds and `-1` means "never", carried through unchanged from
// the read model rather than translated to null: a consumer switching on one
// sentinel is simpler than one distinguishing null from 0, and the value has the
// same meaning at every hop (see internal/app/screens).
type screenStatusRow struct {
	ScreenID  string `json:"screen_id"`
	Name      string `json:"name,omitempty"`
	ScopeNode string `json:"scope_node,omitempty"`
	RelayID   string `json:"relay_id,omitempty"`

	// Reachability is `live` | `fetching` | `stale` | `never_seen` — never
	// "offline". This surface cannot tell a screen that is switched off from one
	// whose network dropped from one whose player crashed, and each sends an
	// operator somewhere different (internal/app/screens' package doc).
	Reachability string `json:"reachability"`
	// LiveWindowMs is the threshold `reachability` was decided by, published
	// alongside the judgement so a consumer that wants a different line can draw
	// it from the raw ages rather than being stuck with this one.
	LiveWindowMs int64 `json:"live_window_ms"`
	// ContentTransferWindowMs is the SECOND threshold — how far past
	// LiveWindowMs an unacknowledged pull still reads `fetching`. Published for
	// the same reason as the first: a judgement without the lines it was drawn
	// at is a number nobody can check, and a consumer that wants to treat
	// `fetching` as `stale` needs to know which line it is disagreeing with.
	ContentTransferWindowMs int64 `json:"content_transfer_window_ms"`
	// FetchingMaxUnackedPulls is the THIRD line `reachability` was drawn at: the
	// most outstanding pulls a screen may have and still be called `fetching`
	// rather than `stale`. Published with the other two because it is the one
	// that distinguishes a screen downloading a video from a screen re-asking
	// forever and confirming nothing, and a consumer given the count below with
	// no line to read it against cannot tell those apart either.
	FetchingMaxUnackedPulls int64 `json:"fetching_max_unacked_pulls"`

	Paired bool `json:"paired"`

	LastPullAgeMs        int64 `json:"last_pull_age_ms"`
	LastAckAgeMs         int64 `json:"last_ack_age_ms"`
	LastRenderStartAgeMs int64 `json:"last_render_start_age_ms"`
	// UnackedPulls is how many program pulls the relay has served this screen
	// since the last acknowledgement it saw: 0 up to date, 1 materialising the
	// Lease it holds, climbing for one that keeps asking and never confirms. It
	// is the most directly actionable number on the row for a screen that is not
	// live — "this wall has failed twenty pulls in a row" is a different call-out
	// than "this wall is downloading a video".
	UnackedPulls int   `json:"unacked_pulls"`
	ReportAgeMs  int64 `json:"report_age_ms"`

	ProgramRevision string `json:"program_revision,omitempty"`
	Priority        string `json:"priority,omitempty"`
	Display         string `json:"display,omitempty"`
	ContentCount    int    `json:"content_count"`
	RenderAssetRef  string `json:"render_asset_ref,omitempty"`

	// Now is the operator's active push-now override for this screen, or absent
	// when the screen is following its schedule. It rides THIS list rather than
	// a read of its own so a console renders the whole row — what the screen was
	// told to show, what it is observed showing, and whether a human overrode it
	// — from one request, and so those three can never be a refresh apart.
	Now *screenNow `json:"now,omitempty"`

	// labels is the AUTHORED screen row's label map, carried so the standard
	// `selector` parameter this operation declares actually filters on it
	// (API-032's label selector; listRegistryPage calls labelsOf per row).
	//
	// Unexported, so it is not serialized: the openapi ScreenStatus schema does
	// not publish labels, and a filter input does not have to be an output. What
	// it must not be is absent — a labels function returning nil makes EVERY
	// label selector match nothing, which is a filter that answers "no screens"
	// instead of "these screens", and an operator reads an empty fleet page as an
	// outage rather than as a typo.
	labels map[string]string
}

// ScreenStatusSource is the seam this operation reads relay-observed status
// from. internal/app/screens.Registry satisfies it directly via Statuses — the
// identical method signature, no adapter — so the api layer depends on the READ
// capability rather than on the read model's concrete type, and a deployment
// with no relay plane simply wires none.
type ScreenStatusSource interface {
	Statuses() []screens.Status
}

// WithScreenStatus wires the live screen-status read model. Optional, with the
// same degrade-not-404 posture WithDevicePlane takes: without it the route still
// mounts and answers truthfully — every authored screen listed, every one
// reported `never_seen`, which is exactly what is true of a deployment whose
// relays report nothing.
func WithScreenStatus(src ScreenStatusSource) Option {
	return func(srv *server) { srv.screenStatus = src }
}

// mountScreenStatus registers the read. A distinct top-level path rather than a
// subpath of `screens`, so the generic screens family's own `GET /screens/{id}`
// cannot be shadowed by it (`screen-status` is not a screen id, but a route that
// relied on that would be a route waiting for the first screen named
// "screen-status").
func (srv *server) mountScreenStatus(rt *router) {
	rt.HandleFunc("GET "+apiPrefix+"/screen-status", srv.listScreenStatus)
}

// listScreenStatus handles GET /api/v1/screen-status (openapi listScreenStatus).
func (srv *server) listScreenStatus(w http.ResponseWriter, r *http.Request) {
	rows, err := srv.store.List(r.Context(), store.KindScreen, store.ListFilter{})
	if err != nil {
		writeProblem(w, r, http.StatusInternalServerError, "INTERNAL", "Internal Server Error", "An unexpected server error occurred.")
		return
	}
	observed := map[string]screens.Status{}
	if srv.screenStatus != nil {
		for _, st := range srv.screenStatus.Statuses() {
			observed[st.ScreenID] = st
		}
	}

	out := make([]screenStatusRow, 0, len(rows))
	for _, row := range rows {
		out = append(out, screenStatusRowOf(row, observed[row.ID], srv.nowMs()))
	}

	// Paged, scoped and selector-filtered through the SAME helper the device
	// plane's own read model uses, rather than a second implementation of the
	// list conventions: the cursor is resource-scoped (API-033), a bad
	// limit/cursor is a 400 (API-030/031/035), and the visible-set filter runs
	// before the page is cut so page sizes stay honest.
	listRegistryPage(srv, w, r, screenStatusResourceType, out,
		func(s screenStatusRow) string { return s.ScreenID },
		func(s screenStatusRow) map[string]string { return s.labels },
		func(s screenStatusRow) string { return s.ScopeNode },
	)
}

// screenStatusRowOf joins one authored screen row with whatever the relays have
// observed of it and whatever override is on it.
//
// A screen with NO observation is not omitted and is not left half-filled: it is
// reported `never_seen` with every age at the never sentinel, which is the true
// statement about it and the one an operator most needs to see. Reporting it as
// live-with-zero-ages, or leaving it out of the list, are the two ways this row
// could lie, and both were available for free.
func screenStatusRowOf(row store.Resource, st screens.Status, nowMs int64) screenStatusRow {
	f := parseFields(row.Body)
	out := screenStatusRow{
		ScreenID:                row.ID,
		Name:                    screenNameOf(row.Body),
		ScopeNode:               f.ScopeNode,
		labels:                  f.Labels,
		Reachability:            string(screens.ReachabilityNeverSeen),
		LiveWindowMs:            screens.LiveWindowMs,
		ContentTransferWindowMs: screens.ContentTransferWindowMs,
		FetchingMaxUnackedPulls: screens.MaxFetchingUnackedPulls,
		// Never observed, on every axis, until a report says otherwise. Spelled
		// out rather than left at the struct's zero value, because the zero value
		// of an age field is 0 — "just now" — which is the single most misleading
		// thing this row could say about a screen nobody has heard from.
		LastPullAgeMs:        screens.NeverObserved,
		LastAckAgeMs:         screens.NeverObserved,
		LastRenderStartAgeMs: screens.NeverObserved,
		ReportAgeMs:          screens.NeverObserved,
	}
	if st.ScreenID != "" {
		out.RelayID = st.RelayID
		out.Reachability = string(st.Reachability)
		out.Paired = st.Paired
		out.LastPullAgeMs = st.LastPullAgeMs
		out.LastAckAgeMs = st.LastAckAgeMs
		out.LastRenderStartAgeMs = st.LastRenderStartAgeMs
		out.UnackedPulls = st.UnackedPulls
		out.ReportAgeMs = st.ReportAgeMs
		out.ProgramRevision = st.ProgramRevision
		out.Priority = st.Priority
		out.Display = st.Display
		out.ContentCount = st.ContentCount
		out.RenderAssetRef = st.RenderAssetRef
	}
	// The override is read off the SCREEN ROW (DAT-004c) — the one place it
	// lives — and reported only while it APPLIES (DAT-004d). A lapsed override
	// is not shown, because the projection has already stopped honouring it:
	// reporting it here would tell an operator a screen is pinned when its
	// program has already fallen back to the schedule.
	if o := screenOverrideOf(row.Body); o.Applies(nowMs) {
		n := screenNowOf(row.ID, o)
		out.Now = &n
	}
	return out
}

// screenNameOf reads a screen row's display name out of its stored body. The
// resource baseline (parseFields) does not carry `name` — it is a per-kind
// member, not an api/1 convention — so it is read here rather than widened into
// a projection every other family would also have to answer for.
func screenNameOf(body []byte) string {
	var raw struct {
		Name string `json:"name"`
	}
	_ = json.Unmarshal(body, &raw)
	return raw.Name
}
