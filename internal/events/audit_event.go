package events

import "encoding/json"

// audit.event (EVT-080) publishes one platform audit record: who did what to
// what, and whether it succeeded. EVT-081 lists this schema's mandatory
// emission triggers (auth events, enrollment/pairing, consent changes,
// session/API-key issuance and revocation, trust-bundle/signing-key changes,
// entitlement changes, pack lifecycle, and every privileged admin operation)
// — those triggers and EVT-082's long-lived retention_class requirement are
// producer/envelope concerns, not this payload validator's. EVT-083 is this
// schema's distinguishing rule: result: failure MUST still carry every other
// field — a failed action is exactly as auditable as a successful one, never
// elided for having failed.

// validAuditResults is the closed EVT-080 audit.event.result enum.
var validAuditResults = map[string]struct{}{
	"success": {},
	"failure": {},
}

// validateAuditEvent enforces the EVT-080 audit.event field definition:
// actor_principal is a required ULID; on_behalf_of is an optional ULID
// (absent, or explicitly null, for a self-attributed action); action and
// target are required strings; result is the closed EVT-080 enum. None of
// these requirements relax when result is failure — EVT-083 requires every
// field, including target, to still be present. A missing or out-of-enum
// field is an EVT-013 delivery-gate rejection.
func validateAuditEvent(raw json.RawMessage) error {
	m, err := payloadObject(raw, "audit.event")
	if err != nil {
		return err
	}
	if err := requireULIDField(m, "actor_principal"); err != nil {
		return err
	}
	if err := optionalULIDField(m, "on_behalf_of"); err != nil {
		return err
	}
	if err := requireStringField(m, "action"); err != nil {
		return err
	}
	// EVT-083: target is still required even when result is failure — a
	// failed action is never elided of its target for having failed.
	if err := requireStringField(m, "target"); err != nil {
		return err
	}
	if _, err := requireEnumField(m, "result", validAuditResults, "success, failure (EVT-080)"); err != nil {
		return err
	}
	return nil
}
