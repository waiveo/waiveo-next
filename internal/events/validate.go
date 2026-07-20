package events

import (
	"encoding/json"
	"errors"

	"github.com/maaxton/waiveo-next/internal/shared/ulid"
)

// ValidationError is an EVT-013 envelope/payload validation failure: the field
// that failed and why. It doubles as a Go error so Validate can return it where
// an error is expected; the delivery driver treats any Validate error as
// delivered:false.
type ValidationError struct {
	Field  string
	Detail string
}

func (e ValidationError) Error() string {
	return "events: invalid " + e.Field + ": " + e.Detail
}

// ErrPackSchema is the distinct EVT-022 sentinel Validate returns for a
// pack-namespaced schema (EVT-021, contains '/'): this catalog defines no field
// shape for it — its payload validates against the pack's own payloadSchema
// (manifest/1 MAN-090, enforced at emission by ctx/1 CTX-070), not here. It is
// neither a registered-catalog match nor a registered-schema validation failure,
// so it is kept distinct from a ValidationError for the delivery driver to treat
// per EVT-022.
var ErrPackSchema = errors.New("events: pack-namespaced schema is out of the registered catalog (EVT-022)")

// payloadValidator validates a registered schema's payload against its own field
// definition (EVT-013). Task 1 registers stubs; the real per-schema validators
// are filled in by the schema files (entity_state.go, automation_run.go, …).
type payloadValidator func(json.RawMessage) error

// stubValidator is the Task-1 placeholder: it accepts any payload. Each real
// per-schema validator replaces its entry in payloadValidators.
func stubValidator(json.RawMessage) error { return nil }

// payloadValidators dispatches a registered schema name to its payload validator
// (EVT-013). Its key set is exactly RegisteredSchemas (EVT-020) — see
// TestRegisteredCatalogAndValidatorsAgree.
var payloadValidators = map[string]payloadValidator{
	SchemaEntityStateChanged: validateEntityStateChanged,
	SchemaAutomationRun:      validateAutomationRun,
	SchemaContentPlayed:      validateContentPlayed,
	SchemaDeviceHeartbeat:    validateDeviceHeartbeat,
	SchemaBoxVitals:          validateBoxVitals,
	SchemaAuditEvent:         stubValidator,
	SchemaVariableChanged:    stubValidator,
}

// Validate is the EVT-013 delivery gate. It first checks the envelope's required
// fields (EVT-010) — ULID id/scope_node/trace_id (and origin_principal when
// present), non-empty schema/cost_class/retention_class, a present ts, and a
// valid origin — then dispatches to the payload validator registered for
// env.Schema. An unregistered bare schema is rejected (EVT-013); a
// pack-namespaced schema returns ErrPackSchema (EVT-022). A nil return means the
// event is deliverable; any error means it is never delivered.
func Validate(env Envelope) error {
	if !ulid.Valid(env.ID) {
		return ValidationError{Field: "id", Detail: "must be a valid ULID"}
	}
	if env.Schema == "" {
		return ValidationError{Field: "schema", Detail: "must be non-empty"}
	}
	if env.TS <= 0 {
		return ValidationError{Field: "ts", Detail: "must be present"}
	}
	if !ulid.Valid(env.ScopeNode) {
		return ValidationError{Field: "scope_node", Detail: "must be a valid ULID"}
	}
	if !ulid.Valid(env.TraceID) {
		return ValidationError{Field: "trace_id", Detail: "must be a valid ULID"}
	}
	if env.CostClass == "" {
		return ValidationError{Field: "cost_class", Detail: "must be non-empty"}
	}
	if env.RetentionClass == "" {
		return ValidationError{Field: "retention_class", Detail: "must be non-empty"}
	}
	if !ValidOrigin(env.Origin) {
		return ValidationError{Field: "origin", Detail: "must be one of internal, relay, ingest"}
	}
	if env.OriginPrincipal != "" && !ulid.Valid(env.OriginPrincipal) {
		return ValidationError{Field: "origin_principal", Detail: "must be a valid ULID when present"}
	}

	// Dispatch by schema name. A '/'-bearing (pack) name is out of this catalog
	// (EVT-021/022); an unregistered bare name is an EVT-013 rejection.
	if IsPackSchemaName(env.Schema) {
		return ErrPackSchema
	}
	validate, ok := payloadValidators[env.Schema]
	if !ok {
		return ValidationError{Field: "schema", Detail: "not a registered platform schema"}
	}
	return validate(env.Payload)
}
