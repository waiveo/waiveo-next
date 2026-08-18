package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/maaxton/waiveo-next/internal/app/auth"
	"github.com/maaxton/waiveo-next/internal/app/platformlog"
	"github.com/maaxton/waiveo-next/internal/app/store"
	"github.com/maaxton/waiveo-next/internal/events"
	"github.com/maaxton/waiveo-next/internal/manifest"
	"github.com/maaxton/waiveo-next/internal/shared/apihttp"
)

// The actions plane's HTTP surface: the host queues work for a pack, and the
// pack leases it and answers.
//
// Three routes and one asymmetry worth stating up front. The PRODUCER route is
// addressed by pack — anyone with authority may invoke `waiveo/backups`'s
// action. The two CONSUMER routes are not addressed at all: which queue they
// serve is resolved from the caller's own principal. A pack that could name a
// queue could lease another pack's work and answer on its behalf, so the route
// simply gives it no way to ask.

// invocationLeaseMs is how long a leased invocation is held before it expires.
//
// Long enough for a real handler (a backup, an upload), short enough that a pack
// killed mid-handler does not strand its queue for minutes. The lease is not the
// action's deadline — a pack that needs longer re-leases nothing, it simply
// answers, and the expiry only decides what happens when it never does.
const invocationLeaseMs int64 = 120_000

// leasePollInterval is how often a long-poll re-checks the queue. The queue is a
// local SQLite read; the cost of polling it is far below the cost of a channel
// registry that has to survive a pack restarting mid-poll.
const leasePollInterval = 250 * time.Millisecond

// invokePackAction queues a call into a pack (MAN-101).
func (srv *server) invokePackAction(w http.ResponseWriter, r *http.Request) {
	traceID := apihttp.TraceID(r)
	packID := packIDFromPath(r)
	actionName := r.PathValue("action")

	pack, found, err := srv.store.GetPack(r.Context(), packID)
	if err != nil {
		srv.packProblem(w, r, http.StatusInternalServerError, "INTERNAL", "Internal Server Error", "An unexpected server error occurred.")
		return
	}
	if !found {
		srv.packProblem(w, r, http.StatusNotFound, "NOT_FOUND", "Not Found", "No pack of that id is installed.")
		return
	}

	// The action must be one the pack DECLARED. Queuing an undeclared name would
	// hand the pack work it never advertised and cannot be expected to handle,
	// and the invocation would sit until its lease expired.
	action, ok := declaredAction(pack.Manifest, actionName)
	if !ok {
		srv.packProblem(w, r, http.StatusNotFound, "NOT_FOUND", "Not Found",
			"This pack declares no action of that name.")
		return
	}

	var body struct {
		Params json.RawMessage `json:"params"`
	}
	raw, ok := readBody(w, r)
	if !ok {
		return
	}
	// An action taking no parameters may send no body at all, so an empty one is
	// not a malformed one. A body that IS present is checked against the declared
	// schema rather than by hand.
	if len(raw) > 0 {
		if srv.schemaRejected(w, r, "PackActionInvokeRequest", raw) {
			return
		}
		if err := json.Unmarshal(raw, &body); err != nil {
			srv.packProblem(w, r, http.StatusUnprocessableEntity, "VALIDATION_FAILED", "Validation Failed",
				"The request body could not be parsed.")
			return
		}
	}

	inv, err := srv.store.EnqueuePackInvocation(r.Context(), store.PackInvocation{
		PackID: packID, Action: actionName, Params: body.Params,
		// Copied from the manifest AT ENQUEUE, so this invocation's fate on a
		// lease expiry is decided by the promise in force when it was accepted
		// rather than by whatever a later update says (MAN-103).
		Idempotency: action.IdempotencyClass,
		TraceID:     traceID,
	})
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			srv.packProblem(w, r, http.StatusNotFound, "NOT_FOUND", "Not Found", "No pack of that id is installed.")
			return
		}
		srv.packProblem(w, r, http.StatusInternalServerError, "INTERNAL", "Internal Server Error", "An unexpected server error occurred.")
		return
	}
	writeJSONValue(w, http.StatusAccepted, invocationBody(inv))
}

// leasePackInvocation serves the calling pack its next piece of work.
func (srv *server) leasePackInvocation(w http.ResponseWriter, r *http.Request) {
	packID, ok := srv.callerPackID(w, r)
	if !ok {
		return
	}

	deadline := time.Now().Add(longPollBudget(r))
	for {
		inv, found, err := srv.store.LeasePackInvocation(r.Context(), packID, invocationLeaseMs)
		if err != nil {
			srv.packProblem(w, r, http.StatusInternalServerError, "INTERNAL", "Internal Server Error", "An unexpected server error occurred.")
			return
		}
		if found {
			writeJSONValue(w, http.StatusOK, invocationBody(inv))
			return
		}
		if time.Now().After(deadline) {
			// 204, not an error: a pack polling an idle host is the common case,
			// and an error here would make "nothing to do" indistinguishable from
			// a broken queue.
			w.WriteHeader(http.StatusNoContent)
			return
		}
		select {
		case <-time.After(leasePollInterval):
		case <-r.Context().Done():
			// The pack hung up — its own shutdown, or a swap. Nothing to write.
			return
		}
	}
}

// reportPackInvocationResult records the outcome of work the caller leased.
func (srv *server) reportPackInvocationResult(w http.ResponseWriter, r *http.Request) {
	packID, ok := srv.callerPackID(w, r)
	if !ok {
		return
	}
	invocationID := r.PathValue("invocation_id")

	inv, err := srv.store.GetPackInvocation(r.Context(), invocationID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			srv.packProblem(w, r, http.StatusNotFound, "NOT_FOUND", "Not Found", "No invocation of that id exists.")
			return
		}
		srv.packProblem(w, r, http.StatusInternalServerError, "INTERNAL", "Internal Server Error", "An unexpected server error occurred.")
		return
	}
	// A pack may only answer its OWN work. Reported as 404 rather than 403: the
	// existence of another pack's invocation is not this caller's to learn, and
	// a 403 would confirm the id is real.
	if inv.PackID != packID {
		srv.packProblem(w, r, http.StatusNotFound, "NOT_FOUND", "Not Found", "No invocation of that id exists.")
		return
	}

	var body struct {
		Result       json.RawMessage `json:"result"`
		ErrorCode    string          `json:"error_code"`
		ErrorMessage string          `json:"error_message"`
	}
	raw, ok := readBody(w, r)
	if !ok {
		return
	}
	if srv.schemaRejected(w, r, "PackInvocationResultRequest", raw) {
		return
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		srv.packProblem(w, r, http.StatusUnprocessableEntity, "VALIDATION_FAILED", "Validation Failed",
			"The request body could not be parsed.")
		return
	}

	done, err := srv.store.CompletePackInvocation(r.Context(), invocationID, body.Result, body.ErrorCode, body.ErrorMessage)
	if err != nil {
		if errors.Is(err, store.ErrInvocationNotLeased) {
			// 409 rather than 404: the invocation exists and the caller owns it,
			// but its lease is gone. The remedy is not to retry this call — the
			// queue has already recorded a verdict — so the message says so.
			srv.packProblem(w, r, http.StatusConflict, "INVOCATION_NOT_LEASED", "Conflict",
				"This invocation is no longer leased to you; its lease expired and the queue has already recorded an outcome.")
			return
		}
		srv.packProblem(w, r, http.StatusInternalServerError, "INTERNAL", "Internal Server Error", "An unexpected server error occurred.")
		return
	}
	writeJSONValue(w, http.StatusOK, invocationBody(done))
}

// callerPackID resolves which pack is calling, refusing anyone who is not one.
//
// 403 rather than 404: these two routes exist and the caller reached them, they
// are simply not for humans. Telling an operator who hit one with a console
// session that it does not exist would send them looking for a typo.
func (srv *server) callerPackID(w http.ResponseWriter, r *http.Request) (string, bool) {
	p, err := auth.RequirePrincipal(r.Context())
	if err != nil {
		srv.packProblem(w, r, http.StatusUnauthorized, "UNAUTHENTICATED", "Unauthorized", "No credential was presented.")
		return "", false
	}
	packID, err := srv.authn.Store().PackIDForPrincipal(r.Context(), p.ID)
	if err != nil {
		srv.packProblem(w, r, http.StatusForbidden, "FORBIDDEN", "Forbidden",
			"This operation is for an installed extension's own process; it serves the caller's own queue and there is nothing to serve a human session.")
		return "", false
	}
	return packID, true
}

// longPollBudget reads `wait`, clamped to the range the contract publishes. An
// unparseable or absent value is zero — return immediately — because a pack that
// meant to wait can say so, and guessing a long hold for a caller that did not
// ask ties up a connection nobody wanted held.
func longPollBudget(r *http.Request) time.Duration {
	raw := r.URL.Query().Get("wait")
	if raw == "" {
		return 0
	}
	secs, err := strconv.Atoi(raw)
	if err != nil || secs < 0 {
		return 0
	}
	if secs > 60 {
		secs = 60
	}
	return time.Duration(secs) * time.Second
}

// declaredAction finds the named action in a pack's manifest (MAN-100).
func declaredAction(raw json.RawMessage, name string) (manifest.Action, bool) {
	var m struct {
		Actions []manifest.Action `json:"actions"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return manifest.Action{}, false
	}
	for _, a := range m.Actions {
		if a.Name == name {
			return a, true
		}
	}
	return manifest.Action{}, false
}

// getPackInvocation reports ONE invocation's current state and, once it has
// finished, its outcome — the read an operator needs and did not have.
//
// # Why this exists
//
// Invoking an action answers 202 with an invocation id, because the work
// outlives the request. Without this route that id led nowhere: an operator
// could start a backup or a scan and had no way to learn whether it succeeded,
// which makes "it ran" unfalsifiable. The two invocation routes that existed are
// both EXTENSION-facing — one leases work, one reports a result — and neither
// answers the person who pressed the button.
//
// # Who may read one
//
// Any authenticated operator, and an extension only for its OWN invocations.
// The isolation matters for the same reason an extension may not ANSWER another
// extension's invocation: a result is whatever that extension chose to report,
// and one extension reading another's outcomes is a side channel between two
// things the host otherwise keeps apart. A human session is not subject to that
// rule — it is the party the outcome is for.
//
// An unknown id is a plain 404. It is not an existence oracle worth protecting:
// invocation ids are server-minted ULIDs nobody can guess, and an operator who
// mistypes one is better served by "no such invocation" than by silence.
func (srv *server) getPackInvocation(w http.ResponseWriter, r *http.Request) {
	invocationID := r.PathValue("invocation_id")

	inv, err := srv.store.GetPackInvocation(r.Context(), invocationID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeProblem(w, r, http.StatusNotFound, "NOT_FOUND", "Not Found", "No invocation exists with this identifier.")
			return
		}
		writeProblem(w, r, http.StatusInternalServerError, "INTERNAL", "Internal Server Error", "An unexpected server error occurred.")
		return
	}

	// If the caller IS an extension, it may only read its own. A human session
	// resolves to no extension id, which is not an error here — it is the
	// ordinary case — so this asks WITHOUT writing a refusal.
	if p, perr := auth.RequirePrincipal(r.Context()); perr == nil {
		if callerPack, cerr := srv.authn.Store().PackIDForPrincipal(r.Context(), p.ID); cerr == nil && callerPack != inv.PackID {
			writeProblem(w, r, http.StatusNotFound, "NOT_FOUND", "Not Found", "No invocation exists with this identifier.")
			return
		}
	}

	writeJSONValue(w, http.StatusOK, invocationBody(inv))
}

// invocationBody renders an invocation for the wire.
func invocationBody(inv store.PackInvocation) map[string]any {
	out := map[string]any{
		"invocation_id": inv.InvocationID,
		"pack_id":       inv.PackID,
		"action":        inv.Action,
		"params":        json.RawMessage(inv.Params),
		"idempotency":   inv.Idempotency,
		"state":         inv.State,
		"created_at":    inv.CreatedAt,
		"updated_at":    inv.UpdatedAt,
	}
	// The optional members are present only when they mean something. A result
	// key on a pending invocation, or an error code on a successful one, would
	// each be a field a client has to learn to ignore.
	if inv.State == store.InvocationLeased {
		out["lease_expires"] = inv.LeaseExpires
	}
	if inv.State == store.InvocationSucceeded {
		out["result"] = json.RawMessage(inv.Result)
	}
	if inv.ErrorCode != "" {
		out["error_code"] = inv.ErrorCode
		out["error_message"] = inv.ErrorMessage
	}
	return out
}

// appendPackLog records a line from the calling extension into the platform log.
//
// The pack's own stderr already reaches the host's log, inherited by the
// supervisor — but undifferentiated, and only for a pack the host started. This
// gives an extension a way to say something ATTRIBUTED, which is what makes
// `/platform-logs?source=pack:waiveo/backups` a question an operator can ask.
func (srv *server) appendPackLog(w http.ResponseWriter, r *http.Request) {
	packID, ok := srv.callerPackID(w, r)
	if !ok {
		return
	}
	if srv.platformLog == nil {
		// The degrade-not-404 posture the read side takes: a deployment that
		// captures nothing accepts the line and drops it, rather than making a
		// pack's logging fail on a box configured without a buffer.
		w.WriteHeader(http.StatusNoContent)
		return
	}
	sink, ok := srv.platformLog.(interface {
		Append(source string, level platformlog.Level, message string)
	})
	if !ok {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	raw, ok := readBody(w, r)
	if !ok {
		return
	}
	// Checked against the DECLARED schema — required members, the level enum, and
	// additionalProperties at every depth — rather than restated here. A
	// hand-written restatement is one that can be incomplete, and this file
	// already carried one: `message` was checked and `level`'s enum was not, so a
	// pack could report a level the document forbids.
	if srv.schemaRejected(w, r, "PackLogAppendRequest", raw) {
		return
	}
	var body struct {
		Message string `json:"message"`
		Level   string `json:"level"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		srv.packProblem(w, r, http.StatusUnprocessableEntity, "VALIDATION_FAILED", "Validation Failed",
			"The request body could not be parsed.")
		return
	}

	// The source is the CALLER's pack, never anything in the body.
	sink.Append(packLogSource(packID), platformlog.Level(body.Level), body.Message)
	w.WriteHeader(http.StatusNoContent)
}

// packLogSource is how an extension's lines are attributed in the platform log.
// Namespaced so a pack can never collide with a first-party component's source,
// and so `source=pack:...` reads as one obvious filter to an operator.
func packLogSource(packID string) string { return "pack:" + packID }

// emitPackEvent publishes a durable event the calling pack declared
// (manifest/1 MAN-090, events/1 EVT-021/022).
//
// This is what makes `contributes.automation.events` more than a declaration:
// an emitted event lands in the same durable log every first-party event does,
// so an automation rule can fire on it through the machinery that already reads
// that log — eventtriggers.go anticipates the pack-namespaced name by name.
//
// The event must be one the pack DECLARED. An undeclared name would put a
// schema into the durable log that no manifest describes, so nothing could ever
// validate its payload and an operator reading the log would find records whose
// shape has no owner.
func (srv *server) emitPackEvent(w http.ResponseWriter, r *http.Request) {
	packID, ok := srv.callerPackID(w, r)
	if !ok {
		return
	}
	// The principal itself, for the envelope's origin_principal and for the
	// scope the event is placed at. Both come from the identity the tier-grant
	// ceremony issued, never from the request: an event a pack could place at a
	// scope of its choosing is an event it could hide from the operators who can
	// see that scope.
	p, err := auth.RequirePrincipal(r.Context())
	if err != nil {
		srv.packProblem(w, r, http.StatusUnauthorized, "UNAUTHENTICATED", "Unauthorized", "No credential was presented.")
		return
	}
	scope := principalScopeNode(p)
	if scope == "" {
		srv.packProblem(w, r, http.StatusForbidden, "FORBIDDEN", "Forbidden",
			"This extension's principal is bound at no scope, so its events have nowhere to be placed.")
		return
	}
	raw, ok := readBody(w, r)
	if !ok {
		return
	}
	if srv.schemaRejected(w, r, "PackEventEmitRequest", raw) {
		return
	}
	var body struct {
		Name    string          `json:"name"`
		Payload json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		srv.packProblem(w, r, http.StatusUnprocessableEntity, "VALIDATION_FAILED", "Validation Failed",
			"The request body could not be parsed.")
		return
	}

	pack, found, err := srv.store.GetPack(r.Context(), packID)
	if err != nil || !found {
		srv.packProblem(w, r, http.StatusNotFound, "NOT_FOUND", "Not Found", "No pack of that id is installed.")
		return
	}
	if !declaresEvent(pack.Manifest, body.Name) {
		srv.packProblem(w, r, http.StatusUnprocessableEntity, "EVENT_NOT_DECLARED", "Validation Failed",
			"This pack declares no automation event of that name (manifest/1 MAN-090).")
		return
	}

	if srv.runEvents == nil {
		// No durable log wired. Accepted and dropped, matching the degrade posture
		// every other optional sink on this surface takes — a pack's emission must
		// not fail because a deployment captures no events.
		w.WriteHeader(http.StatusNoContent)
		return
	}
	cost, retention, classed := events.ClassFor(body.Name)
	if !classed {
		// Unreachable for a declared pack event — MAN-090 requires the name be
		// pack-namespaced, and events/1 classes every such name by shape. Checked
		// anyway: minting an envelope with an empty class is the one thing
		// EVT-013 forbids, and "cannot happen" is not a reason to write one.
		srv.packProblem(w, r, http.StatusUnprocessableEntity, "VALIDATION_FAILED", "Validation Failed",
			"This event name cannot be classed for durable storage.")
		return
	}
	payload := body.Payload
	if len(payload) == 0 {
		// An event with no payload is legal; an envelope with a nil one is not
		// (Validate refuses it). An empty object is the same statement in a shape
		// the log can hold.
		payload = json.RawMessage(`{}`)
	}
	env := events.Envelope{
		ID:        srv.newID(),
		Schema:    body.Name,
		TS:        srv.nowMs(),
		ScopeNode: scope,
		TraceID:   apihttp.TraceID(r),
		CostClass: cost, RetentionClass: retention,
		// `internal`, because EVT-010's origin enum is closed to
		// internal|relay|ingest and this record was written by THIS process on
		// the pack's behalf — the pack did not reach the log itself.
		//
		// Which extension it was rides on origin_principal, which the contract
		// requires to be a ULID: the pack-service principal's own id. That is a
		// better answer than a name anyway — it joins to the audit trail and to
		// the install records, where a display string would not.
		Origin:          "internal",
		OriginPrincipal: p.ID,
		Payload:         payload,
	}
	// ErrPackSchema is SUCCESS here, not failure. Validate runs every structural
	// check — ULIDs, classes, the origin enum — before dispatching on the schema
	// name, and returns that sentinel for a pack-namespaced one because EVT-022
	// puts the payload's shape in the PACK's manifest rather than this catalog.
	// So the envelope is fully checked; only the payload is out of scope, and
	// treating the sentinel as a rejection would make every pack event
	// unemittable.
	if err := events.Validate(env); err != nil && !errors.Is(err, events.ErrPackSchema) {
		// Anything else is refused rather than appended: EVT-013 is explicit that
		// an emission failing validation never becomes a deliverable durable
		// event, and a half-valid record in the log is worse than a rejected call.
		srv.packProblem(w, r, http.StatusUnprocessableEntity, "VALIDATION_FAILED", "Validation Failed",
			"The event did not validate: "+err.Error())
		return
	}
	srv.runEvents.Append(env)
	w.WriteHeader(http.StatusNoContent)
}

// declaresEvent reports whether the manifest declares name under
// contributes.automation.events (MAN-090).
func declaresEvent(rawManifest json.RawMessage, name string) bool {
	var m struct {
		Contributes struct {
			Automation struct {
				Events []struct {
					Name string `json:"name"`
				} `json:"events"`
			} `json:"automation"`
		} `json:"contributes"`
	}
	if err := json.Unmarshal(rawManifest, &m); err != nil {
		return false
	}
	for _, e := range m.Contributes.Automation.Events {
		if e.Name == name {
			return true
		}
	}
	return false
}

// principalScopeNode is the scope an extension's events are placed at: the one
// its role binding was issued at (SEC-037). A principal with several bindings
// yields the first by id, deterministically — a pack-service principal has
// exactly one today, and picking by map order would place the same pack's events
// at different scopes between restarts.
func principalScopeNode(p auth.Principal) string {
	best := ""
	for _, b := range p.Bindings {
		if best == "" || b.ScopeNode < best {
			best = b.ScopeNode
		}
	}
	return best
}
