package events

import "encoding/json"

// variable.changed (EVT-084) publishes one committed change to a
// platform-owned typed variable's value. EVT-085 requires it be emitted once
// per committed write, in write order — this file validates one such
// payload's shape only; emission ordering is a producer-side concern.

// validateVariableChanged enforces the EVT-084 variable.changed field
// definition: variable is a required non-empty string (a variable's own name
// is never blank); old_value/new_value are each required fields whose JSON
// value is a string, number, boolean, or null — null meaning "unset"
// beforehand (old_value) or "unset by this change" (new_value) — never an
// object or array. A missing, empty, or non-scalar field is an EVT-013
// delivery-gate rejection.
func validateVariableChanged(raw json.RawMessage) error {
	m, err := payloadObject(raw, "variable.changed")
	if err != nil {
		return err
	}
	if err := requireNonEmptyStringField(m, "variable"); err != nil {
		return err
	}
	if err := requireScalarOrNullField(m, "old_value"); err != nil {
		return err
	}
	if err := requireScalarOrNullField(m, "new_value"); err != nil {
		return err
	}
	return nil
}
