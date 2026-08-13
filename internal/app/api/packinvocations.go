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
