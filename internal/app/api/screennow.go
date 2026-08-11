package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/maaxton/waiveo-next/internal/app/store"
)

// screennow.go is the operator's PUSH-NOW surface — "show this on that screen,
// now" — as two operations on one singleton subresource of a screen row:
//
//	PUT    /api/v1/screens/{screen_id}/now   set (or replace) the override
//	DELETE /api/v1/screens/{screen_id}/now   clear it; the screen falls back
//	                                         to its schedule
//
// It is the api/1 half of internal/app/store's screen-override subsystem, whose
// own header owns WHY the override is durable desired state rather than a live
// command. What this file owns is the operator-facing contract around it.
//
// # Why PUT and DELETE rather than two POSTs
//
// A screen has AT MOST ONE override, addressed at a fixed path, and pushing
// twice means the second push wins. That is a PUT: idempotent by method, so a
// console retrying a timed-out request cannot double-apply, with no
// Idempotency-Key needed to make it safe (API-050's convention exists for POSTs
// whose repetition would create a second thing; there is no second thing here).
// Clearing is the DELETE of that same singleton, and is likewise safely
// repeatable — clearing an override that is not there succeeds.
//
// # How "now" is actually achieved, and why nothing here pushes
//
// This handler writes one row and returns. The write bumps the desired-state
// generation, whose post-commit hook nudges every live relay (REL-057); each
// relay re-pulls, applies the generation, and installs the screen's new
// `preempt` program; the screen picks it up on its next ordinary program poll
// (~10s, PLY-082) and — because a preempt Lease interrupts rather than waits
// (PLY-100/101) — swaps immediately rather than at the end of the current item.
//
// No new push channel was invented for this, deliberately. The poll and the
// nudge already exist, they already carry every other change to a screen, and a
// second delivery path for one operation would be a second thing to keep
// working, a second thing to secure, and a second thing to be wrong during an
// outage.
//
// # What this operation deliberately does not do
//
// It does not report that the screen IS showing the pushed content — only that
// the platform now intends it to. Delivery is a poll away and the screen may be
// unreachable; a console that wants the other half reads the screen-status
// surface (screenstatus.go), which reports what the relay has actually observed.
// Claiming success for an unconfirmed delivery is exactly the "surface that
// accepts work it never performs" shape this codebase keeps having to remove.

// screenNowRequest is PUT /screens/{screen_id}/now's body (openapi
// ScreenNowRequest): exactly one of cast_id/playlist_id.
//
// Decoded with DisallowUnknownFields, as every strict-bodied operation in this
// package is: the two members differ by one word, and silently ignoring a
// misspelled `cast_id` would leave the request naming NOTHING and be refused as
// "name a cast or a playlist" — a message pointing at the field the caller
// thought they had set.
type screenNowRequest struct {
	CastID     string `json:"cast_id"`
	PlaylistID string `json:"playlist_id"`
}

// screenNow is the operation's response body (openapi ScreenNow): the override
// as it now stands. `source` names which of the two id members is populated, so
// a consumer switches on one closed value instead of inferring intent from which
// string happens to be empty.
type screenNow struct {
	ScreenID   string `json:"screen_id"`
	Source     string `json:"source"`
	CastID     string `json:"cast_id,omitempty"`
	PlaylistID string `json:"playlist_id,omitempty"`
	PushedAt   int64  `json:"pushed_at"`
}

// Override source discriminators (openapi ScreenNow.source).
const (
	screenNowSourceCast     = "cast"
	screenNowSourcePlaylist = "playlist"
)

// mountScreenNow registers the push-now pair. Both hang off the screens family's
// own base path at a three-segment shape the generic mount registers nothing on
// (it registers only GET/PATCH/DELETE on `screens/{id}`), so neither can shadow
// or be shadowed by ordinary screen CRUD.
func (srv *server) mountScreenNow(rt *router) {
	rt.HandleFunc("PUT "+apiPrefix+"/screens/{screen_id}/now", srv.setScreenNow)
	rt.HandleFunc("DELETE "+apiPrefix+"/screens/{screen_id}/now", srv.clearScreenNow)
}

// setScreenNow implements PUT /screens/{screen_id}/now (openapi setScreenNow).
func (srv *server) setScreenNow(w http.ResponseWriter, r *http.Request) {
	raw, ok := readBody(w, r)
	if !ok {
		return
	}
	var req screenNowRequest
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeProblem(w, r, http.StatusUnprocessableEntity, "VALIDATION_FAILED", "Validation Failed",
			"The request body must be a JSON object carrying exactly one of `cast_id` or `playlist_id`.")
		return
	}
	// Exactly one, checked here as well as in the store. The API check is what
	// produces a 422 an operator can read; the store's own check is what makes
	// the invariant true for every caller, including a future one that does not
	// come through this handler (a recurring defect shape in this codebase is a
	// rule enforced at exactly one layer, so the second caller silently breaks
	// it).
	if (req.CastID == "") == (req.PlaylistID == "") {
		writeProblem(w, r, http.StatusUnprocessableEntity, "VALIDATION_FAILED", "Validation Failed",
			"Set exactly one of `cast_id` or `playlist_id` — a push names one thing to show.")
		return
	}

	id, ok := srv.authorizeScreenWrite(w, r)
	if !ok {
		return
	}

	written, err := srv.store.SetScreenOverride(r.Context(), id, req.CastID, req.PlaylistID)
	if err != nil {
		// The screen vanished between the authorization read above and the
		// write's own in-transaction existence check — the same delete race
		// issuePairingCode handles, answered the same way.
		if errors.Is(err, store.ErrScreenOverrideScreenUnknown) {
			writeProblem(w, r, http.StatusNotFound, "NOT_FOUND", "Not Found", "No screen exists with this identifier.")
			return
		}
		// The cast or playlist named does not exist. A 422 rather than a 404:
		// the SCREEN in the path is real and was found, so the request is not
		// addressed at a missing resource — it carries a body member naming one,
		// which is a validation fault about that member.
		if errors.Is(err, store.ErrScreenOverrideTargetUnknown) {
			writeProblem(w, r, http.StatusUnprocessableEntity, "VALIDATION_FAILED", "Validation Failed",
				"No cast or playlist exists with the identifier this push names.")
			return
		}
		writeProblem(w, r, http.StatusInternalServerError, "INTERNAL", "Internal Server Error", "An unexpected server error occurred.")
		return
	}

	// The store hands back the row it wrote, `pushed_at` included: a handler that
	// re-read the table here would be racing a concurrent clear for a value the
	// write already produced.
	writeJSONValue(w, http.StatusOK, screenNowOf(written))
}

// clearScreenNow implements DELETE /screens/{screen_id}/now (openapi
// clearScreenNow): the screen returns to whatever its schedule resolves.
//
// It answers 204 whether or not an override was actually removed. A DELETE's
// contract is about the resulting state — no override is in force — and that
// state is reached identically either way; answering 404 for the second of two
// clicks would make a console show an error for an operation that did exactly
// what the operator wanted.
func (srv *server) clearScreenNow(w http.ResponseWriter, r *http.Request) {
	id, ok := srv.authorizeScreenWrite(w, r)
	if !ok {
		return
	}

	if _, err := srv.store.ClearScreenOverride(r.Context(), id); err != nil {
		writeProblem(w, r, http.StatusInternalServerError, "INTERNAL", "Internal Server Error", "An unexpected server error occurred.")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// authorizeScreenWrite resolves {screen_id}, checks it against the caller's
// visible set, and requires write authority at the screen row's own placement,
// returning the id.
//
// It is the same read-then-write split issuePairingCode performs, factored out
// because both push-now operations need it identically and because the ORDER is
// load-bearing rather than stylistic: a row outside the caller's visible set
// answers 404, exactly as an id naming nothing does, so this surface cannot be
// used to probe which screens exist elsewhere in the tree; only a row the caller
// can SEE but may not WRITE answers 403.
//
// Push-now is a write, not a read, and that is not a formality: it changes what
// a physical display in a physical room shows. Authorizing it at the screen row's
// placement (SEC-005) is what keeps an operator scoped to one site from putting
// content on another site's wall.
func (srv *server) authorizeScreenWrite(w http.ResponseWriter, r *http.Request) (id string, ok bool) {
	id = r.PathValue("screen_id")
	res, found, err := srv.store.Get(r.Context(), store.KindScreen, id)
	if err != nil {
		writeProblem(w, r, http.StatusInternalServerError, "INTERNAL", "Internal Server Error", "An unexpected server error occurred.")
		return "", false
	}
	if !found {
		writeProblem(w, r, http.StatusNotFound, "NOT_FOUND", "Not Found", "No screen exists with this identifier.")
		return "", false
	}
	view, verr := srv.scopeView(r)
	if verr != nil {
		writeProblem(w, r, http.StatusInternalServerError, "INTERNAL", "Internal Server Error", "An unexpected server error occurred.")
		return "", false
	}
	node := parseFields(res.Body).ScopeNode
	if !view.canRead(node) {
		writeProblem(w, r, http.StatusNotFound, "NOT_FOUND", "Not Found", "No screen exists with this identifier.")
		return "", false
	}
	if !view.canWrite(node) {
		writeProblem(w, r, http.StatusForbidden, "FORBIDDEN", "Forbidden", unauthorizedWriteDetail)
		return "", false
	}
	return id, true
}

// screenNowOf projects a stored override onto its served representation — the
// ONE place the `source` discriminator is derived, so the PUT response and the
// screen-status list can never disagree about what a given row is.
func screenNowOf(o store.ScreenOverride) screenNow {
	out := screenNow{
		ScreenID:   o.ScreenID,
		Source:     screenNowSourcePlaylist,
		CastID:     o.CastID,
		PlaylistID: o.PlaylistID,
		PushedAt:   o.CreatedAt,
	}
	if o.CastID != "" {
		out.Source = screenNowSourceCast
	}
	return out
}
