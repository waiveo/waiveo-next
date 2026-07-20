package events

import (
	"encoding/json"
	"errors"
	"testing"
)

// validVariableChangedPayload: a committed change to a platform-owned typed
// variable, its previous value unset (null) and its new value a number
// (EVT-084).
func validVariableChangedPayload() json.RawMessage {
	return json.RawMessage(`{
		"variable": "front_door_armed",
		"old_value": null,
		"new_value": 42
	}`)
}

// TestVariableChanged_NullOldNumberNewValid: old_value null (previously
// unset) and new_value a number both validate (EVT-084).
func TestVariableChanged_NullOldNumberNewValid(t *testing.T) {
	if err := validateVariableChanged(validVariableChangedPayload()); err != nil {
		t.Fatalf("old_value:null / new_value:42 must validate (EVT-084); got %v", err)
	}
}

// TestVariableChanged_StringAndBooleanValuesValid: old_value/new_value also
// accept string and boolean scalars (EVT-084).
func TestVariableChanged_StringAndBooleanValuesValid(t *testing.T) {
	strings := json.RawMessage(`{"variable": "theme", "old_value": "dark", "new_value": "light"}`)
	if err := validateVariableChanged(strings); err != nil {
		t.Fatalf("string old_value/new_value must validate (EVT-084); got %v", err)
	}

	booleans := json.RawMessage(`{"variable": "away_mode", "old_value": false, "new_value": true}`)
	if err := validateVariableChanged(booleans); err != nil {
		t.Fatalf("boolean old_value/new_value must validate (EVT-084); got %v", err)
	}

	unsetting := json.RawMessage(`{"variable": "override_temp", "old_value": 68, "new_value": null}`)
	if err := validateVariableChanged(unsetting); err != nil {
		t.Fatalf("new_value:null (unsetting) must validate (EVT-084); got %v", err)
	}
}

// TestVariableChanged_ObjectValueRejected: a value that is a JSON object is
// never a valid old_value/new_value (EVT-084) — only string, number, boolean,
// or null.
func TestVariableChanged_ObjectValueRejected(t *testing.T) {
	p := json.RawMessage(`{"variable": "front_door_armed", "old_value": null, "new_value": {"nested": true}}`)
	err := validateVariableChanged(p)
	var ve ValidationError
	if !errors.As(err, &ve) || ve.Field != "new_value" {
		t.Fatalf("an object new_value must be rejected on new_value (EVT-084); got %v", err)
	}
}

// TestVariableChanged_ArrayValueRejected: a value that is a JSON array is
// never a valid old_value/new_value (EVT-084) — only string, number, boolean,
// or null.
func TestVariableChanged_ArrayValueRejected(t *testing.T) {
	p := json.RawMessage(`{"variable": "front_door_armed", "old_value": [1, 2], "new_value": 3}`)
	err := validateVariableChanged(p)
	var ve ValidationError
	if !errors.As(err, &ve) || ve.Field != "old_value" {
		t.Fatalf("an array old_value must be rejected on old_value (EVT-084); got %v", err)
	}
}

// TestVariableChanged_EmptyVariableNameRejected: variable is a required
// NON-EMPTY string (EVT-084) — a variable's own name is never blank.
func TestVariableChanged_EmptyVariableNameRejected(t *testing.T) {
	p := json.RawMessage(`{"variable": "", "old_value": null, "new_value": 1}`)
	err := validateVariableChanged(p)
	var ve ValidationError
	if !errors.As(err, &ve) || ve.Field != "variable" {
		t.Fatalf("an empty variable name must be rejected on variable (EVT-084); got %v", err)
	}
}

// TestVariableChanged_MissingVariableRejected: variable is required
// (EVT-084).
func TestVariableChanged_MissingVariableRejected(t *testing.T) {
	p := json.RawMessage(`{"old_value": null, "new_value": 1}`)
	err := validateVariableChanged(p)
	var ve ValidationError
	if !errors.As(err, &ve) || ve.Field != "variable" {
		t.Fatalf("a missing variable must be rejected on variable (EVT-084); got %v", err)
	}
}

// TestVariableChanged_MissingValueFieldsRejected: old_value/new_value MUST be
// present (null when unset/unsetting) — outright absence is an EVT-013
// rejection, distinct from an explicit null (EVT-084).
func TestVariableChanged_MissingValueFieldsRejected(t *testing.T) {
	missingOld := json.RawMessage(`{"variable": "front_door_armed", "new_value": 1}`)
	err := validateVariableChanged(missingOld)
	var ve ValidationError
	if !errors.As(err, &ve) || ve.Field != "old_value" {
		t.Fatalf("a missing old_value must be rejected on old_value (EVT-084); got %v", err)
	}

	missingNew := json.RawMessage(`{"variable": "front_door_armed", "old_value": null}`)
	err = validateVariableChanged(missingNew)
	if !errors.As(err, &ve) || ve.Field != "new_value" {
		t.Fatalf("a missing new_value must be rejected on new_value (EVT-084); got %v", err)
	}
}

// TestVariableChanged_DeliversThroughValidate: a well-formed variable.changed
// envelope passes the full EVT-013 gate (delivered).
func TestVariableChanged_DeliversThroughValidate(t *testing.T) {
	env := validEnvelope()
	env.Schema = SchemaVariableChanged
	env.Payload = validVariableChangedPayload()
	if err := Validate(env); err != nil {
		t.Fatalf("a well-formed variable.changed envelope must deliver (EVT-013); got %v", err)
	}
}
