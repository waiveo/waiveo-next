package events

import (
	"encoding/json"
	"errors"
	"testing"
)

// validEntityStateChangedPayload is the EVT-010 corpus payload: a significant
// attribute changes with the canonical state string unchanged, so
// attribute_change is true and the changed attribute rides in attributes_delta.
func validEntityStateChangedPayload() json.RawMessage {
	return json.RawMessage(`{
		"entity_id": "01J8Z3K4N5P6Q7R8S9T0V1W2Y9",
		"device_id": "01J8Z3K4N5P6Q7R8S9T0V1W2YA",
		"old_state": "playing",
		"new_state": "playing",
		"attribute_change": true,
		"attributes_delta": { "active_app_id": { "old": null, "new": "slidecast" } }
	}`)
}

// TestEntityStateChanged_CorpusPayloadValidates: the EVT-010 corpus payload
// validates against its own field definition (EVT-030).
func TestEntityStateChanged_CorpusPayloadValidates(t *testing.T) {
	if err := validateEntityStateChanged(validEntityStateChangedPayload()); err != nil {
		t.Fatalf("the EVT-010 corpus entity.state_changed payload must validate (EVT-030); got %v", err)
	}
}

// TestEntityStateChanged_DeltaPresentWhenAttributeChangeFalse: attributes_delta
// is present only when attribute_change is true (EVT-030) — a delta with
// attribute_change false is a violation.
func TestEntityStateChanged_DeltaPresentWhenAttributeChangeFalse(t *testing.T) {
	p := json.RawMessage(`{
		"entity_id": "01J8Z3K4N5P6Q7R8S9T0V1W2Y9",
		"device_id": "01J8Z3K4N5P6Q7R8S9T0V1W2YA",
		"old_state": "playing",
		"new_state": "playing",
		"attribute_change": false,
		"attributes_delta": { "active_app_id": { "old": null, "new": "slidecast" } }
	}`)
	err := validateEntityStateChanged(p)
	var ve ValidationError
	if !errors.As(err, &ve) || ve.Field != "attributes_delta" {
		t.Fatalf("attributes_delta present with attribute_change false must be an EVT-030 violation on attributes_delta; got %v", err)
	}
}

// TestEntityStateChanged_DeltaAbsentWhenAttributeChangeTrue: the other half of
// the present-iff rule — a true attribute_change with no attributes_delta is a
// violation (EVT-030).
func TestEntityStateChanged_DeltaAbsentWhenAttributeChangeTrue(t *testing.T) {
	p := json.RawMessage(`{
		"entity_id": "01J8Z3K4N5P6Q7R8S9T0V1W2Y9",
		"device_id": "01J8Z3K4N5P6Q7R8S9T0V1W2YA",
		"old_state": "playing",
		"new_state": "idle",
		"attribute_change": true
	}`)
	err := validateEntityStateChanged(p)
	var ve ValidationError
	if !errors.As(err, &ve) || ve.Field != "attributes_delta" {
		t.Fatalf("attributes_delta absent with attribute_change true must be an EVT-030 violation on attributes_delta; got %v", err)
	}
}

// TestEntityStateChanged_MissingOldStateRejected mirrors the
// EVT-013-invalid-registered-schema-malformed-payload corpus case: a recognized
// schema name whose payload omits a required field is never delivered.
func TestEntityStateChanged_MissingOldStateRejected(t *testing.T) {
	p := json.RawMessage(`{
		"entity_id": "01J8Z3K4N5P6Q7R8S9T0V1W2Y9",
		"device_id": "01J8Z3K4N5P6Q7R8S9T0V1W2YA",
		"new_state": "playing",
		"attribute_change": false
	}`)
	err := validateEntityStateChanged(p)
	var ve ValidationError
	if !errors.As(err, &ve) || ve.Field != "old_state" {
		t.Fatalf("a missing old_state must be an EVT-013 violation on old_state; got %v", err)
	}
}

// TestEntityStateChanged_NullAttributeChangeRejected: a JSON null for the
// required boolean attribute_change is not a boolean and must be an EVT-013
// rejection — encoding/json unmarshals null as a no-op, so a null must never
// silently pass as a zero-value false (EVT-030 requires a boolean here).
func TestEntityStateChanged_NullAttributeChangeRejected(t *testing.T) {
	p := json.RawMessage(`{
		"entity_id": "01J8Z3K4N5P6Q7R8S9T0V1W2Y9",
		"device_id": "01J8Z3K4N5P6Q7R8S9T0V1W2YA",
		"old_state": "playing",
		"new_state": "idle",
		"attribute_change": null
	}`)
	err := validateEntityStateChanged(p)
	var ve ValidationError
	if !errors.As(err, &ve) || ve.Field != "attribute_change" {
		t.Fatalf("a null attribute_change must be an EVT-013 rejection on attribute_change, not a silent false; got %v", err)
	}
}

// TestEntityStateChanged_BadEntityIDULIDRejected: entity_id/device_id are ULID
// fields (EVT-030).
func TestEntityStateChanged_BadEntityIDULIDRejected(t *testing.T) {
	p := json.RawMessage(`{
		"entity_id": "not-a-ulid",
		"device_id": "01J8Z3K4N5P6Q7R8S9T0V1W2YA",
		"old_state": "playing",
		"new_state": "playing",
		"attribute_change": false
	}`)
	if err := validateEntityStateChanged(p); err == nil {
		t.Fatal("a non-ULID entity_id must be rejected (EVT-030)")
	}
}

// TestEntityStateChanged_DeliversThroughValidate: a well-formed
// entity.state_changed envelope passes the full EVT-013 gate (delivered).
func TestEntityStateChanged_DeliversThroughValidate(t *testing.T) {
	env := validEnvelope()
	env.Schema = SchemaEntityStateChanged
	env.Payload = validEntityStateChangedPayload()
	if err := Validate(env); err != nil {
		t.Fatalf("a well-formed entity.state_changed envelope must deliver (EVT-013); got %v", err)
	}
}
