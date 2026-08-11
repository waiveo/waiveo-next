package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/maaxton/waiveo-next/internal/app/store"
	"github.com/maaxton/waiveo-next/internal/datamodel"
)

// screennow.go is the operator's PUSH-NOW surface — "show this on that screen,
// now" — as two operations on one singleton subresource of a screen row:
//
//	PUT    /api/v1/screens/{screen_id}/now   set (or replace) the override
//	DELETE /api/v1/screens/{screen_id}/now   clear it; the screen falls back
//	                                         to its schedule
//
// It is the api/1 half of the screen program override (data-model/1 DAT-004c),
// which lives as the `override` member of the screen IDENTITY ROW
// (datamodel.Screen.Override) — not in a table of its own. What this file owns
// is the operator-facing contract around it.
//
// # Why the override is a member of the screen row
//
// An override is per-screen state and the screen row already travels: it rides
// the signed desired-state snapshot the relay already applies, gets projected
// onto that screen's REL-061 entry by snapshot.overrideProgram, and is read back
// on every screen read with no join. A side table for it would be a second thing
// to project, to join, and to keep consistent with the row it is about — and the
// two would drift on the first delete that only remembered one of them.
//
// It is also what gives the override a TTL for free: `expires_at` is a member of
// the row like the rest of it, evaluated at RESOLUTION time
// (ScreenOverride.Applies), so an alert stops applying with no writer running at
// all — which is what makes "show this for sixty seconds" an operation rather
// than a reminder to come back and clean up.
//
// # Why these two routes, rather than an `override` member on PATCH /screens
//
// Imposing an override is an IMPERATIVE ACT on a physical display in a physical
// room, and `PUT /screens/{id}/now` says that. A PATCH carrying an `override`
// member says "edit this row", which buries the act inside a resource edit:
// invisible to anything that reads the route rather than the body (audit
// review, rate limiting, an authorization matrix), and it makes "clear it" an
// explicit JSON `null` instead of a DELETE. So the screen resource's create and
// update bodies deliberately declare NO `override` member — `additionalProperties:
// false` on ScreenCreate/ScreenUpdate makes a PATCH that tries it a 422 — and
// this pair is the only surface that imposes one.
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
// program; the screen picks it up on its next ordinary program poll (~10s,
// PLY-082) and — under `mode: "alert"`, whose Lease is `preempt` — swaps
// immediately rather than at the end of the current item (PLY-100/101).
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
// ScreenNowRequest): the override's own members, with `ttl_seconds` standing in
// for the row's absolute `expires_at`.
//
// Decoded with DisallowUnknownFields, as every strict-bodied operation in this
// package is: silently ignoring a misspelled `cast_id` would leave the request
// naming NOTHING and be refused as "name a cast or a message" — a message
// pointing at the field the caller thought they had set.
//
// TTLSeconds is a DURATION and the row stores an INSTANT. The conversion happens
// server-side, here, for two reasons: "show this for sixty seconds" is what the
// operator means, and an absolute instant computed on the caller's clock would
// let that clock's skew decide when a fire-drill notice comes down.
type screenNowRequest struct {
	Mode       string `json:"mode"`
	CastID     string `json:"cast_id"`
	Message    string `json:"message"`
	TTLSeconds int    `json:"ttl_seconds"`
}

// screenNow is the operation's response body (openapi ScreenNow): the override
// as it now stands. `source` names which of the two content members is
// populated, so a consumer switches on one closed value instead of inferring
// intent from which string happens to be empty.
type screenNow struct {
	ScreenID  string `json:"screen_id"`
	Mode      string `json:"mode"`
	Source    string `json:"source"`
	CastID    string `json:"cast_id,omitempty"`
	Message   string `json:"message,omitempty"`
	ExpiresAt int64  `json:"expires_at,omitempty"`
	PushedAt  int64  `json:"pushed_at,omitempty"`
}

// Override source discriminators (openapi ScreenNow.source).
const (
	screenNowSourceCast    = "cast"
	screenNowSourceMessage = "message"
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
	// The body is checked against the schema `api/openapi.yaml` DECLARES for it,
	// through the SAME gate the resource families run — not against a
	// hand-written restatement of that schema. A restatement is a second copy of
	// one rule and it was already incomplete: the document declares
	// `ttl_seconds: minimum: 1`, the hand check tested only `< 0`, and a
	// `ttl_seconds: 0` the document forbids was accepted and stored. It also
	// reaches `additionalProperties: false` at every nesting depth, which the
	// strict decode below reaches only for members this Go struct happens to
	// type — so a member nested inside a future object member cannot ride in
	// behind a flat struct's field list.
	if srv.schemaRejected(w, r, "ScreenNowRequest", raw) {
		return
	}
	var req screenNowRequest
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeProblem(w, r, http.StatusUnprocessableEntity, "VALIDATION_FAILED", "Validation Failed",
			"The request body must be a JSON object carrying `mode` and exactly one of `cast_id` or `message`.")
		return
	}

	// The override's SHAPE is checked by the one validator that owns DAT-004c
	// (datamodel.ValidateScreenOverride), not by a second hand-written copy of
	// its rules here. A copy is how the API surface and the row validator start
	// disagreeing about what a legal override is — and the row validator is the
	// one that runs on the write below, so a divergence would show up as a 500
	// from a body this handler had just approved.
	now := srv.nowMs()
	override := &datamodel.ScreenOverride{
		Mode:    req.Mode,
		CastID:  req.CastID,
		Message: req.Message,
		SetAt:   now,
	}
	if req.TTLSeconds > 0 {
		override.ExpiresAt = now + int64(req.TTLSeconds)*int64(time.Second/time.Millisecond)
	}
	if errs := datamodel.ValidateScreenOverride(override); len(errs) > 0 {
		writeProblem(w, r, http.StatusUnprocessableEntity, "VALIDATION_FAILED", "Validation Failed", errs[0].Message)
		return
	}

	id, res, ok := srv.authorizeScreenWrite(w, r)
	if !ok {
		return
	}

	// DAT-004c's reference check, on the surface IMPOSING the override — which is
	// where the contract puts it, because this surface holds both row families at
	// once and the screen row's own validator (which sees only screens and
	// devices) does not. Without it a push naming a deleted cast answers 200 and
	// pins the screen to an empty content array: a dark wall, its schedule
	// suppressed, with no error anywhere in the system.
	if override.CastID != "" {
		if err := srv.castMustExist(r, override.CastID); err != nil {
			writeProblem(w, r, http.StatusUnprocessableEntity, "VALIDATION_FAILED", "Validation Failed",
				"No cast exists with the identifier this push names.")
			return
		}
	}

	written, err := srv.writeScreenOverride(r, id, res.Revision, override)
	if err != nil {
		// The screen vanished between the authorization read above and the
		// write — the same delete race issuePairingCode handles, answered the
		// same way. A revision mismatch is the same class: something else wrote
		// this row in between, and a push is "latest wins", so it is retried
		// once inside writeScreenOverride before it can reach here.
		if errors.Is(err, store.ErrNotFound) {
			writeProblem(w, r, http.StatusNotFound, "NOT_FOUND", "Not Found", "No screen exists with this identifier.")
			return
		}
		var verr *store.ValidationError
		if errors.As(err, &verr) && len(verr.Errors) > 0 {
			writeProblem(w, r, http.StatusUnprocessableEntity, "VALIDATION_FAILED", "Validation Failed", verr.Errors[0].Message)
			return
		}
		writeProblem(w, r, http.StatusInternalServerError, "INTERNAL", "Internal Server Error", "An unexpected server error occurred.")
		return
	}

	writeJSONValue(w, http.StatusOK, screenNowOf(id, written))
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
	id, res, ok := srv.authorizeScreenWrite(w, r)
	if !ok {
		return
	}

	// Nothing to clear is not a write: skipping it keeps a repeated clear from
	// bumping the desired-state generation and nudging every relay in the site
	// for no change at all.
	if screenOverrideOf(res.Body) == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	if _, err := srv.writeScreenOverride(r, id, res.Revision, nil); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// Deleted underneath us: the resulting state an operator asked for —
			// no override on that screen — is true.
			w.WriteHeader(http.StatusNoContent)
			return
		}
		writeProblem(w, r, http.StatusInternalServerError, "INTERNAL", "Internal Server Error", "An unexpected server error occurred.")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// writeScreenOverride patches ONE member of the screen row — `override`, or an
// explicit JSON null to clear it — through the ordinary conditional resource
// update (API-022), against the revision the row was read at.
//
// A lost revision race is RETRIED once rather than reported. "Show this here
// now" is a latest-wins instruction (the store's own upsert semantics said the
// same thing), and a 412 about a row the operator never read would be an error
// message about an optimistic-concurrency mechanism they cannot act on. One
// retry, not a loop: two writers racing forever is a different fault and must
// not be hidden by a handler spinning.
func (srv *server) writeScreenOverride(r *http.Request, id string, rev int64, override *datamodel.ScreenOverride) (*datamodel.ScreenOverride, error) {
	// An explicit JSON null clears the override, which is the mergeBody
	// convention for removing an optional member. Omitting the key would leave
	// it untouched, which is exactly what a clear must not do.
	body, err := json.Marshal(map[string]any{"override": override})
	if err != nil {
		return nil, err
	}
	for attempt := 0; attempt < 2; attempt++ {
		if _, err = srv.store.Update(r.Context(), store.KindScreen, id, rev, body); err == nil {
			return override, nil
		}
		if !errors.Is(err, store.ErrRevisionMismatch) {
			return nil, err
		}
		cur, found, gerr := srv.store.Get(r.Context(), store.KindScreen, id)
		if gerr != nil {
			return nil, gerr
		}
		if !found {
			return nil, store.ErrNotFound
		}
		rev = cur.Revision
	}
	return nil, err
}

// castMustExist reports whether castID names a cast row.
func (srv *server) castMustExist(r *http.Request, castID string) error {
	_, found, err := srv.store.Get(r.Context(), store.KindCast, castID)
	if err != nil {
		return err
	}
	if !found {
		return store.ErrNotFound
	}
	return nil
}

// authorizeScreenWrite resolves {screen_id}, checks it against the caller's
// visible set, and requires write authority at the screen row's own placement,
// returning the id AND the row it read (whose revision the conditional write
// below is made against, and whose body says whether an override is even set).
//
// It is the same read-then-write split issuePairingCode performs, factored out
// because both push-now operations need it identically and because the ORDER is
// load-bearing rather than stylistic: a row outside the caller's visible set
// answers 404, exactly as an id naming nothing does, so this surface cannot be
// used to probe which screens exist elsewhere in the tree; only a row the caller
// can SEE but may not WRITE answers 403.
//
// Push-now is a write, not a formality: it changes what a physical display in a
// physical room shows. Authorizing it at the screen row's placement (SEC-005) is
// what keeps an operator scoped to one site from putting content on another
// site's wall.
func (srv *server) authorizeScreenWrite(w http.ResponseWriter, r *http.Request) (id string, res store.Resource, ok bool) {
	id = r.PathValue("screen_id")
	res, found, err := srv.store.Get(r.Context(), store.KindScreen, id)
	if err != nil {
		writeProblem(w, r, http.StatusInternalServerError, "INTERNAL", "Internal Server Error", "An unexpected server error occurred.")
		return "", store.Resource{}, false
	}
	if !found {
		writeProblem(w, r, http.StatusNotFound, "NOT_FOUND", "Not Found", "No screen exists with this identifier.")
		return "", store.Resource{}, false
	}
	view, verr := srv.scopeView(r)
	if verr != nil {
		writeProblem(w, r, http.StatusInternalServerError, "INTERNAL", "Internal Server Error", "An unexpected server error occurred.")
		return "", store.Resource{}, false
	}
	node := parseFields(res.Body).ScopeNode
	if !view.canRead(node) {
		writeProblem(w, r, http.StatusNotFound, "NOT_FOUND", "Not Found", "No screen exists with this identifier.")
		return "", store.Resource{}, false
	}
	if !view.canWrite(node) {
		writeProblem(w, r, http.StatusForbidden, "FORBIDDEN", "Forbidden", unauthorizedWriteDetail)
		return "", store.Resource{}, false
	}
	return id, res, true
}

// screenOverrideOf reads a stored screen row's `override` member. It is the ONE
// decode of that member outside the datamodel package, so the push-now surface
// and the screen-status join cannot disagree about what is set on a screen.
func screenOverrideOf(body []byte) *datamodel.ScreenOverride {
	var raw struct {
		Override *datamodel.ScreenOverride `json:"override"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil
	}
	return raw.Override
}

// screenNowOf projects a screen's override onto its served representation — the
// ONE place the `source` discriminator is derived, so the PUT response and the
// screen-status list can never disagree about what a given override is.
func screenNowOf(screenID string, o *datamodel.ScreenOverride) screenNow {
	if o == nil {
		return screenNow{ScreenID: screenID}
	}
	out := screenNow{
		ScreenID:  screenID,
		Mode:      o.Mode,
		Source:    screenNowSourceMessage,
		CastID:    o.CastID,
		Message:   o.Message,
		ExpiresAt: o.ExpiresAt,
		PushedAt:  o.SetAt,
	}
	if o.CastID != "" {
		out.Source = screenNowSourceCast
	}
	return out
}
