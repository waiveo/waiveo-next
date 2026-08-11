package api

import (
	"encoding/json"
	"log"
	"net/http"

	"github.com/maaxton/waiveo-next/internal/app/store"
	"github.com/maaxton/waiveo-next/internal/datamodel"
	"github.com/maaxton/waiveo-next/internal/events"
	"github.com/maaxton/waiveo-next/internal/shared/apihttp"
)

// variables.go is the api/1 surface for data-model/1's Variables section
// (DAT-130–138): the named, scope-placed scalars a `rules/1` `variable`
// condition reads (RUL-150) and a `variable_write` action writes (RUL-220).
//
// It is an ordinary resourceConfig mount plus exactly three additions, one per
// rule the generic machinery cannot know:
//
//   - `validate` — the name grammar (DAT-131a) and the scalar rule
//     (DAT-132/133), raising VARIABLE_NAME_INVALID and VARIABLE_VALUE_INVALID.
//   - `writeGuards` — name uniqueness within a scope node (DAT-131), raising
//     VARIABLE_NAME_DUPLICATE from INSIDE the write transaction, because the
//     invariant is about the other rows and those can move under a pre-write
//     check.
//   - `afterCommit` — `variable.changed` per committed write (DAT-137,
//     events/1 EVT-084/085).
//
// The three published field-level codes were carried in
// conformance/unimplemented-error-codes.json as "declared ahead of the surface
// that raises them". This file is that surface; the allowlist group is removed
// in the same change, which is what scripts/validate-error-codes.mjs check #3
// exists to force.

// variablesConfig is the resource configuration for the variable kind.
func variablesConfig() resourceConfig {
	return resourceConfig{
		kind:         store.KindVariable,
		path:         "variables",
		resourceType: "variables",
		displayName:  "variable",
		createSchema: "VariableCreate",
		updateSchema: "VariableUpdate",
		selLabels:    func(f resourceFields) map[string]string { return f.Labels },
		placement:    func(f resourceFields) string { return f.ScopeNode },
		extScope:     func(f resourceFields) string { return f.ScopeNode },
		writeScope:   func(f resourceFields) string { return f.ScopeNode },
		validate:     validateVariableRow,
		writeGuards:  variableNameGuards,
		afterCommit:  publishVariableChanged,
	}
}

// validateVariableRow is the pre-write per-kind check: DAT-131a's name grammar
// and DAT-132/133's scalar rule, over the EFFECTIVE body (a create body, or a
// patch merged onto the current row).
//
// The rules live in internal/datamodel rather than here because they are the
// data model's, not the HTTP surface's: the same row written by a seed, a pack
// pipeline or a `variable_write` action's sink must be held to them, and a check
// implemented in a handler is a check every non-handler writer skips.
//
// It consults no stored state, so it takes no scopeView — the uniqueness rule,
// which does consult stored state, is deliberately the guard below and not part
// of this function.
func validateVariableRow(_ *server, _ scopeView, body []byte) []datamodel.Error {
	return datamodel.ValidateVariableBody(body)
}

// variableNameGuards supplies the DAT-131 name-uniqueness guard, evaluated
// inside the store's write transaction.
//
// selfID is the row being updated (empty on create), so a patch that leaves the
// name alone does not collide with itself. The name is read from the EFFECTIVE
// body the caller passes, which on a patch is the merged row — a patch that
// moves the name to one already taken at the node is refused, and a patch that
// changes only the value is not.
func variableNameGuards(_ *server, body []byte) []store.WriteGuard {
	var row struct {
		Name      string `json:"name"`
		ID        string `json:"id"`
		ScopeNode string `json:"scope_node"`
	}
	if err := json.Unmarshal(body, &row); err != nil {
		// An unparseable body never reaches the store — schemaRejected and
		// validate both answer first — so this is unreachable in practice. Adding
		// no guard is the fail-SAFE choice regardless: the write it would have
		// guarded cannot succeed either.
		return nil
	}
	return []store.WriteGuard{store.VariableNameUniqueGuard(row.Name, row.ScopeNode, row.ID)}
}

// publishVariableChanged emits one `variable.changed` per committed variable
// write (DAT-137, events/1 EVT-084/085).
//
// before is nil on a create and after is nil on a delete, and each becomes the
// event's null side: `old_value: null` means "unset beforehand", `new_value:
// null` means "unset by this change" (EVT-084). That is the whole reason DAT-133
// refuses null as a settable VALUE — if it were settable, these two payloads
// would each be ambiguous.
//
// The event's `scope_node` is the WRITTEN ROW's own placement, never the node a
// reader resolves the value at. DAT-137 says so explicitly: the event reports
// what was written, and the set of descendant nodes whose effective value
// changed follows from DAT-134 rather than being enumerated in the event. On a
// delete that placement comes from the pre-delete body, which is the only copy
// of it that still exists.
func publishVariableChanged(srv *server, r *http.Request, before, after json.RawMessage) {
	srv.publishVariableChange(apihttp.TraceID(r), before, after)
}

// publishVariableChange is the ONE emitter of `variable.changed`, shared by the
// HTTP resource handlers (through publishVariableChanged) and by the RUL-220
// `variable_write` action's sink (automations_exec.go).
//
// It is shared rather than duplicated because DAT-137 binds the event to the
// COMMITTED WRITE, not to the HTTP surface: "a committed write to a variable row
// — create, update, or delete — MUST emit variable.changed". A rule's write goes
// straight to the store and never passes through a handler, so an emitter that
// lived only in the afterCommit hook would leave exactly the writes that most
// need an event — the automatic ones, with no operator watching — unpublished.
// That is also the event an `event`-kind trigger fires on (RUL-080/EVT-085), so
// the gap would additionally mean a rule can never react to another rule's
// write.
func (srv *server) publishVariableChange(traceID string, before, after json.RawMessage) {
	if srv.variableEvents == nil {
		// No sink wired. Legitimate for a bare handler in a test; NOT legitimate
		// for a deployment, which is why cmd/waiveo-feeder passes the hub and
		// TestFeederWiresVariableEvents asserts it does.
		return
	}

	name, oldValue, ok := variableNameAndValue(before)
	newName, newValue, newOK := variableNameAndValue(after)
	if newOK {
		// A patch may RENAME a variable. The event names one variable, so a
		// rename is published as the change to the name the row now carries —
		// the name a rule reading it from here on will use. A rename that also
		// orphans the old name is visible as the old name simply ceasing to
		// resolve, which DAT-134 already describes; inventing a second synthetic
		// "unset" event for it would report a delete that never happened and
		// would carry a row id that still exists.
		name = newName
	}
	if !ok && !newOK {
		// Neither side decoded — nothing truthful to publish. Reported rather
		// than dropped silently: the write DID commit, so a missing event here is
		// a gap in the log an operator would otherwise never learn about.
		log.Printf("api: variable.changed not published: neither the pre- nor post-write body decoded")
		return
	}

	scopeNode := variableScopeNode(after)
	if scopeNode == "" {
		scopeNode = variableScopeNode(before)
	}

	srv.variableEvents.Append(events.VariableChangedEnvelope(events.VariableChange{
		ID:        srv.newID(),
		TS:        srv.nowMs(),
		ScopeNode: scopeNode,
		TraceID:   traceID,
		Variable:  name,
		OldValue:  oldValue,
		NewValue:  newValue,
	}))
}

// variableNameAndValue reads a stored variable body's name and value. ok is
// false for a nil body (the absent side of a create or a delete) and for one
// that does not decode.
func variableNameAndValue(body json.RawMessage) (name string, value any, ok bool) {
	if len(body) == 0 {
		return "", nil, false
	}
	var row struct {
		Name  string `json:"name"`
		Value any    `json:"value"`
	}
	if err := json.Unmarshal(body, &row); err != nil {
		return "", nil, false
	}
	return row.Name, row.Value, true
}

// variableScopeNode reads a stored variable body's placement, or "" for an
// absent or undecodable body.
func variableScopeNode(body json.RawMessage) string {
	if len(body) == 0 {
		return ""
	}
	return parseFields(body).ScopeNode
}
