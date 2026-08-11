package events

import "encoding/json"

// variable.changed (EVT-084) publishes one committed change to a
// platform-owned typed variable's value. EVT-085 requires it be emitted once
// per committed write, in write order — this file validates one such
// payload's shape and builds its envelope; emission ordering is a
// producer-side concern (internal/app/api/variables.go).

// variable.changed's EVT-010 classification: its OWN tier, between telemetry
// and audit, and it is that for a reason on each side.
//
// Longer than telemetry, because of DAT-137 plus EVT-084 together. A variable
// write is a configuration change — someone or some rule altered platform state
// that retargets which rules fire — and the event carries `old_value`, which
// makes this log the ONLY place a variable's previous value survives (the row
// holds one value at a time and this platform deliberately keeps no second
// history table). On the telemetry window the sole record of "what it used to
// be" expires in a week.
//
// Strictly shorter than audit, because EVT-082 makes audit.event long-lived
// RELATIVE to every other registered schema. Putting this on `audit-long` does
// not merely blur a tier — it breaks that relation outright, which is not a
// judgement call: retention_test.go's
// TestRetentionPolicy_AuditOutlivesEveryOtherRegisteredSchema walks classBySchema
// and fails, and it failed on exactly this when this schema was first classed.
// A variable write is also not a security-relevant flow in SEC-150's sense —
// nobody authenticated and nothing was granted — so the tier that exists to
// hold those is the wrong home for it on the merits too.
//
// See configChangeWindow (retention.go) for the window itself.
const (
	variableChangedCostClass      = "config"
	variableChangedRetentionClass = "config-change"
)

// VariableChange is one committed write to a variable row, as DAT-137 requires
// it be published: the row's own placement (NOT the node a reader resolves the
// value at — DAT-137 is explicit that the event reports what was WRITTEN, and
// the set of nodes whose effective value changed follows from DAT-134 rather
// than being enumerated here), the variable's name, and the values either side
// of the write.
//
// OldValue and NewValue are `any` and each is nil for "unset": nil on OldValue
// means the variable did not exist at this node beforehand (a create), nil on
// NewValue means this write unset it (a delete). That is exactly EVT-084's
// published reading of null in those two fields, and it is why DAT-133 refuses
// null as a SETTABLE value — admitting it would make `new_value: null`
// ambiguous between "set to null" and "deleted", two facts a rule must tell
// apart arriving as the same bytes.
type VariableChange struct {
	ID        string
	TS        int64
	ScopeNode string
	TraceID   string

	Variable string
	OldValue any
	NewValue any
}

// variableChangedPayload is the EVT-084 wire payload. old_value/new_value carry
// no `omitempty`: both members are REQUIRED and null is a meaningful value for
// each, so omitting them on unset would serve a payload the schema's own
// validator (requireScalarOrNullField) refuses.
type variableChangedPayload struct {
	Variable string `json:"variable"`
	OldValue any    `json:"old_value"`
	NewValue any    `json:"new_value"`
}

// VariableChangedEnvelope builds the full EVT-010 envelope for one committed
// variable write (EVT-084/085, DAT-137). origin is "internal": a variable change
// is produced by the platform's own store, never relayed or ingested from a
// peer.
func VariableChangedEnvelope(c VariableChange) Envelope {
	payload, _ := json.Marshal(variableChangedPayload{
		Variable: c.Variable,
		OldValue: c.OldValue,
		NewValue: c.NewValue,
	})
	return Envelope{
		ID:             c.ID,
		Schema:         SchemaVariableChanged,
		TS:             c.TS,
		ScopeNode:      c.ScopeNode,
		TraceID:        c.TraceID,
		CostClass:      variableChangedCostClass,
		RetentionClass: variableChangedRetentionClass,
		Origin:         "internal",
		Payload:        payload,
	}
}

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
