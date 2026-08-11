// Package events is events/1's leaf schema layer: the durable-event envelope
// (EVT-010), the registered platform-schema catalog with its bare-vs-pack naming
// rule (EVT-020-022), and the EVT-013 validation gate (validate.go). It is a leaf
// — it imports stdlib and internal/shared/ulid only, never internal/rules or
// internal/relay: the automation.run constructor maps the rules-engine outcome
// from plain values so the catalog and the engine cannot drift.
package events

import (
	"encoding/json"
	"strings"
)

// Envelope is the EVT-010 durable-event envelope every event this contract
// delivers, over any binding, is wrapped in. ts is int64 epoch-ms, consistent
// with the rest of the codebase. origin_principal is optional (omitempty) —
// absent when the source carries no principal-shaped identity (EVT-010).
type Envelope struct {
	ID              string          `json:"id"`
	Schema          string          `json:"schema"`
	TS              int64           `json:"ts"`
	ScopeNode       string          `json:"scope_node"`
	TraceID         string          `json:"trace_id"`
	CostClass       string          `json:"cost_class"`
	RetentionClass  string          `json:"retention_class"`
	Origin          string          `json:"origin"`
	OriginPrincipal string          `json:"origin_principal,omitempty"`
	Payload         json.RawMessage `json:"payload"`
}

// The registered platform-schema names (EVT-020). The catalog is additive-only
// within major version 1: a name may be added by a later minor, but a published
// schema's field must not change type/meaning or be removed.
const (
	SchemaEntityStateChanged = "entity.state_changed"
	SchemaAutomationRun      = "automation.run"
	SchemaContentPlayed      = "content.played"
	SchemaDeviceHeartbeat    = "device.heartbeat"
	SchemaBoxVitals          = "box.vitals"
	SchemaAuditEvent         = "audit.event"
	SchemaVariableChanged    = "variable.changed"
)

// RegisteredSchemas is the EVT-020 catalog of platform-registered schema names.
// Every member is a bare <domain>.<name> string (EVT-021, no '/'). Its key set
// is exactly the dispatch registry's (see payloadValidators in validate.go).
var RegisteredSchemas = map[string]struct{}{
	SchemaEntityStateChanged: {},
	SchemaAutomationRun:      {},
	SchemaContentPlayed:      {},
	SchemaScreenInteraction:  {},
	SchemaDeviceHeartbeat:    {},
	SchemaBoxVitals:          {},
	SchemaAuditEvent:         {},
	SchemaVariableChanged:    {},
}

// IsRegisteredSchema reports whether name is a member of the platform-registered
// catalog (EVT-020). A pack-namespaced name (EVT-021, contains '/') is never a
// member, since the two namespaces are mutually exclusive by that '/'.
func IsRegisteredSchema(name string) bool {
	_, ok := RegisteredSchemas[name]
	return ok
}

// IsPackSchemaName reports whether name is a pack-contributed schema name — the
// pack-namespaced <publisher>/<name>.<local> form manifest/1 MAN-090 defines,
// which always contains a '/' from its owning pack ID (EVT-021). Registered
// (bare) and pack names are mutually exclusive by construction.
func IsPackSchemaName(name string) bool {
	return strings.ContainsRune(name, '/')
}

// validOrigins is the closed EVT-010 origin enum.
var validOrigins = map[string]struct{}{
	"internal": {},
	"relay":    {},
	"ingest":   {},
}

// ValidOrigin reports whether o is one of the EVT-010 origin values
// (internal, relay, ingest) — which class of source recorded the event.
func ValidOrigin(o string) bool {
	_, ok := validOrigins[o]
	return ok
}
