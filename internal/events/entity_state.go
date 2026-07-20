package events

import (
	"encoding/json"

	"github.com/maaxton/waiveo-next/internal/shared/ulid"
)

// entity.state_changed (EVT-030) publishes one transition observed on a
// device-plane entity — a canonical state string change and/or a
// significant-class attribute change (EVT-031, device-class-registry/1 REG-044).
// Its distinguishing rule is EVT-030's present-iff: attributes_delta is present
// exactly when attribute_change is true. This file validates one such payload;
// it never resolves the entity's device class (that vocabulary check is a
// producer-side concern).

// validateEntityStateChanged enforces the EVT-030 entity.state_changed field
// definition: entity_id/device_id are ULIDs; old_state/new_state are required
// strings; attribute_change is a required boolean; and attributes_delta is a
// present-object iff attribute_change is true, absent otherwise (EVT-030). A
// missing required field is an EVT-013 delivery-gate rejection.
func validateEntityStateChanged(raw json.RawMessage) error {
	m, err := payloadObject(raw, "entity.state_changed")
	if err != nil {
		return err
	}
	if err := requireULIDField(m, "entity_id"); err != nil {
		return err
	}
	if err := requireULIDField(m, "device_id"); err != nil {
		return err
	}
	if err := requireStringField(m, "old_state"); err != nil {
		return err
	}
	if err := requireStringField(m, "new_state"); err != nil {
		return err
	}
	attrChange, err := requireBoolField(m, "attribute_change")
	if err != nil {
		return err
	}
	// EVT-030: attributes_delta present iff attribute_change is true.
	delta, present := m["attributes_delta"]
	switch {
	case attrChange && !present:
		return ValidationError{Field: "attributes_delta", Detail: "must be present when attribute_change is true (EVT-030)"}
	case !attrChange && present:
		return ValidationError{Field: "attributes_delta", Detail: "must be omitted when attribute_change is false (EVT-030)"}
	case present:
		if !isJSONObject(delta) {
			return ValidationError{Field: "attributes_delta", Detail: "must be an object (EVT-030)"}
		}
	}
	return nil
}

// --- shared registered-schema payload-field helpers (EVT-013) ---
//
// Every registered-schema validator (entity_state, automation_run, and the
// content.played/device.heartbeat/box.vitals/audit.event/variable.changed
// validators added alongside) reads its payload as a raw field map so a
// required field's ABSENCE is distinguishable from a present zero value, which
// the EVT-013 delivery gate depends on.

// payloadObject decodes a registered schema's payload into its top-level field
// map. A payload that is not a JSON object is an EVT-013 rejection named for the
// schema.
func payloadObject(raw json.RawMessage, schema string) (map[string]json.RawMessage, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil || m == nil {
		return nil, ValidationError{Field: "payload", Detail: "not a valid " + schema + " object"}
	}
	return m, nil
}

// requireStringField checks that field is present in m and is a JSON string
// (never null). Absence or a non-string value is an EVT-013 rejection.
func requireStringField(m map[string]json.RawMessage, field string) error {
	raw, ok := m[field]
	if !ok {
		return ValidationError{Field: field, Detail: "required"}
	}
	var s string
	if string(raw) == "null" || json.Unmarshal(raw, &s) != nil {
		return ValidationError{Field: field, Detail: "must be a string"}
	}
	return nil
}

// requireULIDField checks that field is present in m, is a JSON string, and is a
// valid ULID (shared/ulid).
func requireULIDField(m map[string]json.RawMessage, field string) error {
	raw, ok := m[field]
	if !ok {
		return ValidationError{Field: field, Detail: "required"}
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return ValidationError{Field: field, Detail: "must be a string"}
	}
	if !ulid.Valid(s) {
		return ValidationError{Field: field, Detail: "must be a valid ULID"}
	}
	return nil
}

// requireBoolField checks that field is present in m and is a JSON boolean
// (never null), returning its value. The explicit null pre-check matters because
// encoding/json unmarshals a JSON null into an existing value as a no-op that
// returns a nil error, which would otherwise let a null slip through as a
// zero-value false (EVT-013).
func requireBoolField(m map[string]json.RawMessage, field string) (bool, error) {
	raw, ok := m[field]
	if !ok {
		return false, ValidationError{Field: field, Detail: "required"}
	}
	var b bool
	if string(raw) == "null" || json.Unmarshal(raw, &b) != nil {
		return false, ValidationError{Field: field, Detail: "must be a boolean"}
	}
	return b, nil
}

// requireIntField checks that field is present in m and is a JSON integer
// (a fractional number fails, and never null), returning its value. The explicit
// null pre-check matters because encoding/json unmarshals a JSON null into an
// existing value as a no-op that returns a nil error, which would otherwise let a
// null slip through as a zero-value 0 (EVT-013).
func requireIntField(m map[string]json.RawMessage, field string) (int, error) {
	raw, ok := m[field]
	if !ok {
		return 0, ValidationError{Field: field, Detail: "required"}
	}
	var n int
	if string(raw) == "null" || json.Unmarshal(raw, &n) != nil {
		return 0, ValidationError{Field: field, Detail: "must be an integer"}
	}
	return n, nil
}

// requireObjectField checks that field is present in m and is a JSON object.
func requireObjectField(m map[string]json.RawMessage, field string) error {
	raw, ok := m[field]
	if !ok {
		return ValidationError{Field: field, Detail: "required"}
	}
	if !isJSONObject(raw) {
		return ValidationError{Field: field, Detail: "must be an object"}
	}
	return nil
}

// requireArrayField checks that field is present in m and is a JSON array
// (an empty array is valid).
func requireArrayField(m map[string]json.RawMessage, field string) error {
	raw, ok := m[field]
	if !ok {
		return ValidationError{Field: field, Detail: "required"}
	}
	var arr []json.RawMessage
	if string(raw) == "null" || json.Unmarshal(raw, &arr) != nil {
		return ValidationError{Field: field, Detail: "must be an array"}
	}
	return nil
}

// isJSONObject reports whether raw is a JSON object (not null, not an array).
func isJSONObject(raw json.RawMessage) bool {
	if string(raw) == "null" {
		return false
	}
	var obj map[string]json.RawMessage
	return json.Unmarshal(raw, &obj) == nil && obj != nil
}
