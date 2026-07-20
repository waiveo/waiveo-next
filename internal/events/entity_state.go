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

// requireInt64Field checks that field is present in m and is a JSON integer
// (a fractional number fails, and never null), returning its value as an
// int64. content.played's t_start/t_end are epoch-ms Timestamps that can
// exceed a 32-bit range, so this is distinct from requireIntField. The
// explicit null pre-check matters because encoding/json unmarshals a JSON
// null into an existing value as a no-op that returns a nil error, which
// would otherwise let a null slip through as a zero-value 0 (EVT-013).
func requireInt64Field(m map[string]json.RawMessage, field string) (int64, error) {
	raw, ok := m[field]
	if !ok {
		return 0, ValidationError{Field: field, Detail: "required"}
	}
	var n int64
	if string(raw) == "null" || json.Unmarshal(raw, &n) != nil {
		return 0, ValidationError{Field: field, Detail: "must be an integer"}
	}
	return n, nil
}

// requireNumberField checks that field is present in m and is a JSON number
// (never null), returning its value as a float64. box.vitals.cpu_temp is a
// fractional Celsius reading, so (unlike requireIntField/requireInt64Field) a
// fractional value is accepted here. The explicit null pre-check matters
// because encoding/json unmarshals a JSON null into an existing value as a
// no-op that returns a nil error, which would otherwise let a null slip
// through as a zero-value 0 (EVT-013).
func requireNumberField(m map[string]json.RawMessage, field string) (float64, error) {
	raw, ok := m[field]
	if !ok {
		return 0, ValidationError{Field: field, Detail: "required"}
	}
	var n float64
	if string(raw) == "null" || json.Unmarshal(raw, &n) != nil {
		return 0, ValidationError{Field: field, Detail: "must be a number"}
	}
	return n, nil
}

// requireStringArrayField checks that field is present in m and is a JSON
// array of strings — an empty array is valid and distinct from absence
// (box.vitals.throttled_flags, EVT-071: present-even-empty, never absent).
func requireStringArrayField(m map[string]json.RawMessage, field string) ([]string, error) {
	raw, ok := m[field]
	if !ok {
		return nil, ValidationError{Field: field, Detail: "required"}
	}
	var arr []string
	if string(raw) == "null" || json.Unmarshal(raw, &arr) != nil {
		return nil, ValidationError{Field: field, Detail: "must be an array of strings"}
	}
	return arr, nil
}

// requireNullableStringField checks that field is present in m (it MUST be —
// only its value may be absent-in-spirit) and is either a JSON string or a
// JSON null (device.heartbeat.now_playing_content_id: null when nothing is
// currently playing). Outright field absence is still an EVT-013 rejection,
// distinct from an explicit null.
func requireNullableStringField(m map[string]json.RawMessage, field string) error {
	raw, ok := m[field]
	if !ok {
		return ValidationError{Field: field, Detail: "required (send null when there is no value)"}
	}
	if string(raw) == "null" {
		return nil
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return ValidationError{Field: field, Detail: "must be a string or null"}
	}
	return nil
}

// optionalObjectField checks that, when field is present in m, it is a JSON
// object — absence is fine (the field itself is optional: content.played's
// power_evidence, box.vitals's sd_health).
func optionalObjectField(m map[string]json.RawMessage, field string) error {
	raw, present := m[field]
	if !present {
		return nil
	}
	if !isJSONObject(raw) {
		return ValidationError{Field: field, Detail: "must be an object when present"}
	}
	return nil
}

// requireEnumField checks that field is present in m, is a JSON string, and is
// a member of valid — returning the value. A value outside the closed enum is
// an EVT-013 rejection named for the field (e.g. content.played's cause/
// completion, EVT-050).
func requireEnumField(m map[string]json.RawMessage, field string, valid map[string]struct{}, allowed string) (string, error) {
	raw, ok := m[field]
	if !ok {
		return "", ValidationError{Field: field, Detail: "required"}
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return "", ValidationError{Field: field, Detail: "must be a string"}
	}
	if _, ok := valid[s]; !ok {
		return "", ValidationError{Field: field, Detail: "must be one of " + allowed}
	}
	return s, nil
}

// requireNonEmptyStringField checks that field is present in m, is a JSON
// string (never null), and is non-empty (variable.changed.variable, EVT-084:
// a variable's own name is never blank).
func requireNonEmptyStringField(m map[string]json.RawMessage, field string) error {
	if err := requireStringField(m, field); err != nil {
		return err
	}
	var s string
	_ = json.Unmarshal(m[field], &s)
	if s == "" {
		return ValidationError{Field: field, Detail: "must be non-empty"}
	}
	return nil
}

// optionalULIDField checks that, when field is present and non-null in m, it
// is a valid ULID string. Outright absence and an explicit JSON null are both
// accepted as "no value" (audit.event.on_behalf_of, EVT-080: a ULID, optional
// — absent for a self-attributed action; a producer that always writes the
// key writes null rather than omitting it, as the EVT-080 corpus does). A
// present non-null value that is not a valid ULID is an EVT-013 rejection.
func optionalULIDField(m map[string]json.RawMessage, field string) error {
	raw, present := m[field]
	if !present || string(raw) == "null" {
		return nil
	}
	var s string
	if json.Unmarshal(raw, &s) != nil || !ulid.Valid(s) {
		return ValidationError{Field: field, Detail: "must be a valid ULID or null when present"}
	}
	return nil
}

// requireScalarOrNullField checks that field is present in m (required — only
// its value may be null) and that its JSON value decodes to one of string,
// number, boolean, or null — never an object or array
// (variable.changed.old_value/new_value, EVT-084: a platform variable's value
// is always a scalar, or unset). Outright field absence is still an EVT-013
// rejection, distinct from an explicit null.
func requireScalarOrNullField(m map[string]json.RawMessage, field string) error {
	raw, ok := m[field]
	if !ok {
		return ValidationError{Field: field, Detail: "required (send null when unset)"}
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return ValidationError{Field: field, Detail: "must be a string, number, boolean, or null"}
	}
	switch v.(type) {
	case nil, string, float64, bool:
		return nil
	default:
		return ValidationError{Field: field, Detail: "must be a string, number, boolean, or null"}
	}
}
