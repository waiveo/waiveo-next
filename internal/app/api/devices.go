package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/maaxton/waiveo-next/internal/app/devices"
	"github.com/maaxton/waiveo-next/internal/app/store"
	feederrelayconn "github.com/maaxton/waiveo-next/internal/feeder/relayconn"
	"github.com/maaxton/waiveo-next/internal/shared/apihttp"
	"github.com/maaxton/waiveo-next/internal/shared/apiselector"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// devices.go is the device plane's api/1 surface: the two read families
// (openapi listDevices / listEntities) and the two mutating operations —
// sendEntityCommand, which carries an operator's command down the relay/1
// persistent connection the target entity's relay already holds open, and
// adoptDevice, which turns a discovered device into one this platform controls.
//
// Neither read family goes through the generic resourceConfig mount: that
// machinery is built for authored, store-backed resources with a revision and a
// full CRUD verb set, and a device has none of those — it is a projection of
// what the relay discovered and adopted, read-only here by design (openapi
// Device/Entity). What they DO share is every api/1 convention that applies to a
// list: the same apihttp pagination helpers, the same resource-scoped cursor,
// and the same apiselector engine, reused rather than re-derived.

// deviceCommandTimeout bounds one command exchange with a relay. It is the
// backstop for a relay that accepts the frame and never answers — the request's
// own context (a client that disconnects) cancels sooner and dominates. Chosen
// generously: the relay must reach a physical device on its LAN and, per
// REL-115, may be queueing behind an earlier command to that same device.
const deviceCommandTimeout = 15 * time.Second

// Resource-type tags the devices and entities list cursors are scoped by
// (API-033: a cursor carries no meaning across resource types, even when two
// values are byte-identical).
const (
	devicesResourceType  = "devices"
	entitiesResourceType = "entities"
)

// CommandDispatcher carries one device command to the relay that owns the target
// entity and returns that relay's own correlated result (relay/1 REL-112).
// internal/feeder/relayconn.Server satisfies it directly — the identical method
// signature, no adapter — so the api layer depends on the dispatch CAPABILITY
// rather than on the connection server's concrete type, and a deployment with no
// relay plane simply wires none.
//
// traceID is the originating operation's own trace id, carried onto the wire so
// one identifier correlates the operation across the app peer, the relay, and
// any record it produces (REL-006).
type CommandDispatcher interface {
	SendDeviceCommand(ctx context.Context, relayID, traceID string, body wire.DeviceCommandBody) (wire.DeviceCommandResultBody, error)
}

// Option configures the api handler beyond its required dependencies.
type Option func(*server)

// WithDevicePlane wires the device registry the devices/entities list operations
// read and the dispatcher an entity command travels down. Both are optional: a
// deployment without them still MOUNTS all three routes, and they behave
// truthfully rather than disappearing — the lists serve an empty page, and a
// command against an entity nobody has adopted is a 404, exactly as it would be
// against a registry that simply does not contain it.
func WithDevicePlane(registry *devices.Registry, dispatcher CommandDispatcher) Option {
	return func(srv *server) {
		srv.devices = registry
		srv.dispatch = dispatcher
	}
}

// mountDevicePlane registers the device plane's routes. The action paths'
// literal trailing segments are unambiguous against the lists — the generic
// lists register only `GET /devices` and `GET /entities`, and each of these is a
// POST on a distinct three-segment shape.
func (srv *server) mountDevicePlane(rt *router) {
	rt.HandleFunc("GET "+apiPrefix+"/devices", srv.listDevices)
	rt.HandleFunc("POST "+apiPrefix+"/devices/{id}/adopt", srv.adoptDevice)
	rt.HandleFunc("GET "+apiPrefix+"/entities", srv.listEntities)
	rt.HandleFunc("POST "+apiPrefix+"/entities/{id}/commands", srv.sendEntityCommand)
}

// ---- list -----------------------------------------------------------------

// listDevices handles GET /api/v1/devices (openapi listDevices).
func (srv *server) listDevices(w http.ResponseWriter, r *http.Request) {
	var rows []devices.Device
	if srv.devices != nil {
		rows = srv.devices.Devices()
	}
	listRegistryPage(srv, w, r, devicesResourceType, rows,
		func(d devices.Device) string { return d.ID },
		func(d devices.Device) map[string]string { return d.Labels },
		func(d devices.Device) string { return d.ScopeNode },
	)
}

// listEntities handles GET /api/v1/entities (openapi listEntities).
func (srv *server) listEntities(w http.ResponseWriter, r *http.Request) {
	var rows []devices.Entity
	if srv.devices != nil {
		rows = srv.devices.Entities()
	}
	listRegistryPage(srv, w, r, entitiesResourceType, rows,
		func(e devices.Entity) string { return e.ID },
		func(e devices.Entity) map[string]string { return e.Labels },
		func(e devices.Entity) string { return e.ScopeNode },
	)
}

// listRegistryPage serves one registry-backed list operation under the api/1
// list conventions: `limit`/`cursor` validation (API-030/031/035 — a bad limit
// or cursor is 400, the query-parameter half of API-013a), the resource-scoped
// keyset advance (API-033/034), and selector filtering over the row's own labels
// and scope-node placement (API-040/044). rows MUST already be in id order,
// which is what makes the byte comparison below the keyset order.
func listRegistryPage[T any](
	srv *server,
	w http.ResponseWriter,
	r *http.Request,
	resourceType string,
	rows []T,
	idOf func(T) string,
	labelsOf func(T) map[string]string,
	placementOf func(T) string,
) {
	q := r.URL.Query()

	cursor, limit, pperr := apihttp.ParsePageParams(q.Get("cursor"), q.Get("limit"))
	if pperr != nil {
		writeProblem(w, r, pperr.Status, pperr.Code, pperr.Title, pperr.Detail)
		return
	}
	sel, serr := apiselector.Parse(q.Get("selector"))
	if serr != nil {
		writeProblem(w, r, serr.Status, serr.Code, serr.Title, serr.Detail)
		return
	}
	var afterID string
	if cursor != "" {
		lastID, cerr := apihttp.DecodeCursor(resourceType, cursor)
		if cerr != nil {
			writeProblem(w, r, cerr.Status, cerr.Code, cerr.Title, cerr.Detail)
			return
		}
		afterID = lastID
	}

	view, err := srv.scopeView(r)
	if err != nil {
		writeProblem(w, r, http.StatusInternalServerError, "INTERNAL", "Internal Server Error", "An unexpected server error occurred.")
		return
	}

	// The visible-set filter, then the selector, both BEFORE apihttp.Page cuts
	// the page — the same ordering (and for the same two reasons: honest page
	// sizes, and a selector that can only narrow) the generic resource list
	// applies. A device is a projection of what a relay adopted, but it is
	// placed at a scope node like any other resource, so it is scoped like one.
	window := make([]T, 0, len(rows))
	for _, row := range rows {
		if afterID != "" && idOf(row) <= afterID {
			continue
		}
		if !view.canRead(placementOf(row)) {
			continue
		}
		if !sel.Matches(labelsOf(row), placementOf(row), view.inSubtree) {
			continue
		}
		window = append(window, row)
	}

	writeJSONValue(w, http.StatusOK, apihttp.Page(resourceType, window, limit, idOf))
}

// ---- adopt ------------------------------------------------------------------

// adoptDevice handles POST /api/v1/devices/{id}/adopt (openapi adoptDevice): it
// promotes a device the relays have DISCOVERED into one this platform CONTROLS,
// by creating the durable adoption record that compiles into the signed
// desired-state `device_inventory` its relay is sent (relay/1 REL-063).
//
// The two halves of the device plane meet here, and the split is the whole point
// (see the header of identityrows.go): the relay is authoritative for what
// exists on its LAN, this API is authoritative for what has been adopted, and
// nothing a relay reports can adopt anything. This is the one operation that
// crosses from the first to the second, and it is an operator action every time.
//
// Idempotent by construction — adopting an adopted device changes nothing and
// answers 200 — and it additionally honors Idempotency-Key like every other
// mutating POST (API-050/052), so a client that retries a timed-out request
// replays the recorded outcome rather than re-deriving it.
func (srv *server) adoptDevice(w http.ResponseWriter, r *http.Request) {
	// No request body is declared for this operation, so nothing is read or
	// validated from one. The empty slice is still what the Idempotency-Key
	// machinery hashes as this request's body (API-051's fingerprint), which is
	// correct: two adopts of the same device by the same principal on the same
	// path ARE the same request.
	srv.idempotent(w, r, nil, func(w http.ResponseWriter) { srv.adoptDeviceExec(w, r) })
}

// adoptDeviceExec is the adoption's actual work, executed once per fresh
// (non-replayed) request under the Idempotency-Key guard in adoptDevice.
func (srv *server) adoptDeviceExec(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	// Resolve against the read model first, for the same reason a command does:
	// a device nobody has reported is a 404 before anything is written, and the
	// visible-set check answers an out-of-scope device identically so that the
	// refusal itself cannot be used to confirm the device exists (scopeview.go).
	var device devices.Device
	found := false
	if srv.devices != nil {
		device, found = srv.devices.Device(id)
	}
	if !found {
		writeProblem(w, r, http.StatusNotFound, "NOT_FOUND", "Not Found", "No device exists with this identifier.")
		return
	}
	view, viewErr := srv.scopeView(r)
	if viewErr != nil {
		writeProblem(w, r, http.StatusInternalServerError, "INTERNAL", "Internal Server Error", "An unexpected server error occurred.")
		return
	}
	if !view.canRead(device.ScopeNode) {
		writeProblem(w, r, http.StatusNotFound, "NOT_FOUND", "Not Found", "No device exists with this identifier.")
		return
	}
	// Adoption WRITES a durable row and changes what the relay is told to do, so
	// it is authorized as a write at the device's own placement (SEC-005). 403
	// rather than 404 — the caller has just been shown they may see this device,
	// so the refusal reports only their own authority.
	if !view.canWrite(device.ScopeNode) {
		writeProblem(w, r, http.StatusForbidden, "FORBIDDEN", "Forbidden", unauthorizedWriteDetail)
		return
	}

	if _, err := srv.store.AdoptDiscoveredDevice(r.Context(), id); err != nil {
		if errors.Is(err, store.ErrDiscoveredDeviceUnknown) {
			// The read model knows this device but nothing durable mirrors it —
			// the narrow window between a relay's first report and the mirror
			// write, or a deployment running no mirror at all. Answered as the
			// retryable UNAVAILABLE rather than a 404: the device demonstrably
			// exists (it is in the list the caller just read it from), so
			// reporting it as absent would be a lie the client would cache.
			writeProblem(w, r, http.StatusServiceUnavailable, "UNAVAILABLE", "Service Unavailable",
				"This device has been reported but not yet recorded; retry shortly.")
			return
		}
		var verr *store.ValidationError
		if errors.As(err, &verr) {
			// The adoption record the discovered facts build did not validate —
			// in practice a device reported under an identity another adopted row
			// already claims (DEVICE_IDENTITY_DUPLICATE). 422 with the field
			// errors, the same answer the /adopted-devices family gives for the
			// same body, since it is the same body.
			apihttp.WriteProblemExt(w, r, apihttp.TraceID(r), http.StatusUnprocessableEntity,
				"VALIDATION_FAILED", "Validation Failed",
				"One or more fields failed validation.", validationExtra(verr.Errors))
			return
		}
		writeProblem(w, r, http.StatusInternalServerError, "INTERNAL", "Internal Server Error", "An unexpected server error occurred.")
		return
	}

	// The durable record is committed; teach the read model so the very response
	// to this request reports the device as adopted. Ordering matters: marking
	// first would leave the list claiming an adoption that a failed write never
	// made.
	srv.devices.MarkAdopted(id)
	adopted, _ := srv.devices.Device(id)
	writeJSONValue(w, http.StatusOK, adopted)
}

// ---- command --------------------------------------------------------------

// entityCommandRequest is the openapi EntityCommandRequest body. Command is a
// pointer so an ABSENT field is distinguishable from an explicit empty string —
// both are refused, but only a present-and-blank one is the client's own value.
type entityCommandRequest struct {
	Command *string        `json:"command"`
	Params  map[string]any `json:"params"`
}

// sendEntityCommand handles POST /api/v1/entities/{id}/commands: it resolves the
// target entity to the relay that owns it and dispatches one `device.command`
// down that relay's EXISTING persistent connection, returning the relay's own
// correlated `device.command_result` (relay/1 REL-112).
//
// Synchronous by contract shape: REL-112 pairs the command with a single result
// frame on the connection the relay already holds open, so the work genuinely
// completes within this request/response cycle and API-111's 202-plus-Job rule
// — which governs work that CANNOT — does not apply.
//
// It is a mutating POST tagged mcp:act, so it honors Idempotency-Key
// (API-050/052): a client's retry-on-timeout replays the retained result rather
// than firing the command at the physical device a second time. The key handling
// reuses srv.idempotent, the same mechanism the other mcp:act POSTs use.
func (srv *server) sendEntityCommand(w http.ResponseWriter, r *http.Request) {
	// The members this operation declares, and nothing else. These action-style
	// POSTs are not resource families, so they reach none of the resource
	// pipeline; each guards the fields it needs by hand, and none bounded the ones
	// it does not declare.
	//
	// Ahead of srv.idempotent deliberately: a refused body must not be recorded as
	// this key's outcome, or the retry that fixes the body replays the refusal.
	raw, ok := readBody(w, r)
	if !ok {
		return
	}
	if srv.undeclaredMemberRejected(w, r, "EntityCommandRequest", raw) {
		return
	}
	srv.idempotent(w, r, raw, func(w http.ResponseWriter) { srv.sendEntityCommandExec(w, r, raw) })
}

// sendEntityCommandExec is the dispatch's actual work, executed once per fresh
// (non-replayed) request under the Idempotency-Key guard in sendEntityCommand.
func (srv *server) sendEntityCommandExec(w http.ResponseWriter, r *http.Request, raw []byte) {
	id := r.PathValue("id")

	// Resolve the target first: an entity nobody has adopted has no relay to
	// dispatch through, so it is a 404 before the body is even considered — the
	// same answer any unresolvable resource identifier draws (API-014 registry's
	// NOT_FOUND), named by the resource's own noun.
	var entity devices.Entity
	found := false
	if srv.devices != nil {
		entity, found = srv.devices.Entity(id)
	}
	if !found {
		writeProblem(w, r, http.StatusNotFound, "NOT_FOUND", "Not Found", "No entity exists with this identifier.")
		return
	}
	// Resolving an entity is a READ, so it is scoped like one: an entity placed
	// outside the caller's visible set is answered exactly as an entity nobody
	// adopted — the same 404, the same detail. Commanding a device you are not
	// entitled to see would otherwise be reachable by id alone, and the refusal
	// itself would confirm the entity exists (scopeview.go).
	view, viewErr := srv.scopeView(r)
	if viewErr != nil {
		writeProblem(w, r, http.StatusInternalServerError, "INTERNAL", "Internal Server Error", "An unexpected server error occurred.")
		return
	}
	if !view.canRead(entity.ScopeNode) {
		writeProblem(w, r, http.StatusNotFound, "NOT_FOUND", "Not Found", "No entity exists with this identifier.")
		return
	}
	// Resolving it is a read; DISPATCHING to it changes the state of a physical
	// device, so the command itself is authorized as a write at the entity's own
	// scope node (SEC-005). 403 rather than 404 — the caller has just been shown
	// they may see this entity, so the refusal reports only their own authority.
	if !view.canWrite(entity.ScopeNode) {
		writeProblem(w, r, http.StatusForbidden, "FORBIDDEN", "Forbidden", unauthorizedWriteDetail)
		return
	}

	req, verr := decodeEntityCommand(raw)
	if verr != "" {
		// API-013a: a body validation failure is 422, never the 400 a
		// query-parameter failure carries.
		writeProblem(w, r, http.StatusUnprocessableEntity, "VALIDATION_FAILED", "Validation Failed", verr)
		return
	}

	if srv.dispatch == nil {
		// The registry knows this entity but nothing can carry a command to it.
		// Refusing explicitly is the point: a dropped command is never an
		// acceptable outcome, and UNAVAILABLE is the registry's retryable
		// "a dependency cannot serve this right now".
		writeProblem(w, r, http.StatusServiceUnavailable, "UNAVAILABLE", "Service Unavailable",
			"No relay connection plane is available to carry commands.")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), deviceCommandTimeout)
	defer cancel()

	// The command's params are passed straight to the transport and never
	// logged or stored here (REL-114): they MAY carry credential material
	// scoped to this one dispatch.
	result, err := srv.dispatch.SendDeviceCommand(ctx, entity.RelayID, apihttp.TraceID(r), wire.DeviceCommandBody{
		EntityID: entity.ID,
		Command:  *req.Command,
		Params:   req.Params,
	})
	if err != nil {
		status, code, title, detail := classifyDispatchError(err)
		writeProblem(w, r, status, code, title, detail)
		return
	}

	// The relay answered. `ok:false` is a completed exchange whose command did
	// not succeed — the relay's own typed outcome, carried through verbatim
	// with its relay/1 taxonomy code rather than remapped onto an api/1 one
	// (the same reuse-by-name discipline API-013 applies to per-field codes).
	out := map[string]any{"ok": result.OK}
	if result.Error != nil {
		out["error"] = map[string]string{
			"code":    result.Error.Code,
			"message": result.Error.Message,
		}
	}
	writeJSONValue(w, http.StatusOK, out)
}

// decodeEntityCommand parses and validates the EntityCommandRequest body,
// returning a human-readable reason when it is not acceptable. Unknown members
// are rejected, enforcing the schema's own `additionalProperties: false` at
// runtime — which is also what stops a client smuggling an `id` or `entity_id`
// into the body to redirect the dispatch away from the addressed entity.
func decodeEntityCommand(raw []byte) (entityCommandRequest, string) {
	var req entityCommandRequest
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		return req, "The request body could not be parsed: it must be an object carrying `command` and an optional `params`."
	}
	if req.Command == nil || strings.TrimSpace(*req.Command) == "" {
		return req, "`command` is required."
	}
	return req, ""
}

// classifyDispatchError maps a transport-level dispatch failure onto its api/1
// Problem. Every branch resolves to a registry code (API-011) — the relay/1
// taxonomy governs the relay's own ANSWER, not this API's inability to obtain
// one.
func classifyDispatchError(err error) (status int, code, title, detail string) {
	switch {
	case errors.Is(err, feederrelayconn.ErrRelayNotConnected):
		return http.StatusServiceUnavailable, "UNAVAILABLE", "Service Unavailable",
			"The relay that owns this entity has no live connection; the command was not dispatched."
	case errors.Is(err, context.DeadlineExceeded):
		return http.StatusServiceUnavailable, "UNAVAILABLE", "Service Unavailable",
			"The relay did not answer the command within the allowed time."
	case errors.Is(err, context.Canceled):
		return http.StatusServiceUnavailable, "UNAVAILABLE", "Service Unavailable",
			"The command exchange was canceled before the relay answered."
	default:
		// Includes a relay/1 typed refusal of the request itself: the app peer
		// built a frame its own relay rejected, which is a defect on this side,
		// not something the client can correct.
		return http.StatusInternalServerError, "INTERNAL", "Internal Server Error",
			"An unexpected server error occurred."
	}
}
