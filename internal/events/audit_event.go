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

// The cost/retention classing for an audit.event. retention_class is
// deliberately its OWN value rather than the telemetry-standard every other
// registered schema carries: EVT-082 requires an audit event's retention be "a
// value the platform's retention configuration treats as long-lived relative to
// the other registered schemas — an audit trail outliving the operational
// telemetry recorded alongside it is the property this field exists to express."
// `audit-long` is that value; it sorts beside `telemetry-standard` in the same
// vocabulary, which events/1 itself does not enumerate (EVT-050/051).
const (
	auditEventCostClass      = "audit"
	auditEventRetentionClass = "audit-long"
)

// The closed EVT-080 audit.event.result enum, as constants a producer names
// rather than spelling the literal at each emission site.
const (
	AuditResultSuccess = "success"
	AuditResultFailure = "failure"
)

// validAuditResults is the closed EVT-080 audit.event.result enum.
var validAuditResults = map[string]struct{}{
	AuditResultSuccess: {},
	AuditResultFailure: {},
}

// auditEventPayload is the EVT-080 audit.event payload shape used to MARSHAL a
// payload in AuditEventEnvelope. on_behalf_of is omitempty — EVT-080 makes it
// optional, absent for a self-attributed action.
type auditEventPayload struct {
	ActorPrincipal string `json:"actor_principal"`
	OnBehalfOf     string `json:"on_behalf_of,omitempty"`
	Action         string `json:"action"`
	Target         string `json:"target"`
	Result         string `json:"result"`
	// The three members below are ADDITIVE payload extensions: EVT-080's own
	// field definition names none of them, and validateAuditEvent requires none
	// of them, so an audit.event from any other producer is unaffected by their
	// existence. Each exists because a security-model/1 requirement names it as
	// something that record must carry:
	//
	//   - ClockAssessment — the app's `trusted`/`untrusted` clock-accuracy
	//     assessment, which SEC-091 requires on every login-failure record
	//     "alongside its other fields, so a burst of failures coinciding with a
	//     clock-trust transition is diagnosable after the fact".
	//
	//   - Purpose / IssuedVia — SEC-034 requires every grant creation and
	//     redemption record carry "that grant's purpose and issued_via in its
	//     payload, so a recovery-purpose, console-issued redemption is
	//     distinguishable in the audit trail from a routine invite redemption
	//     months later". Encoding them into the free-text `target` instead would
	//     make that distinction a string-parsing exercise rather than a field.
	ClockAssessment string `json:"clock_assessment,omitempty"`
	Purpose         string `json:"purpose,omitempty"`
	IssuedVia       string `json:"issued_via,omitempty"`

	// Detail is EVT-080a's optional free-form context — the one field with no
	// consumer that filters on it, for the case where what an operator was SHOWN
	// is part of what the record must preserve (api/1 API-144's blast radius).
	Detail string `json:"detail,omitempty"`
}

// AuditEvent is the input to AuditEventEnvelope: EVT-080's five payload fields
// plus the envelope identity/placement the producer supplies and the additive
// members security-model/1 requires on particular records (see
// auditEventPayload). It is a struct rather than a positional argument list
// because the five same-typed string fields at its core are exactly the shape a
// caller silently transposes.
type AuditEvent struct {
	ID        string
	TS        int64
	ScopeNode string
	TraceID   string

	ActorPrincipal string
	OnBehalfOf     string
	Action         string
	Target         string
	Result         string

	ClockAssessment string
	Purpose         string
	IssuedVia       string

	// Detail is EVT-080a's optional free-form context. Empty is omitted.
	Detail string
}

// AuditEventEnvelope builds the full EVT-010 envelope for one audit.event
// (EVT-080), classed per EVT-082 so the audit trail outlives the telemetry
// recorded beside it. origin is "internal": an audit record is produced by the
// platform itself, never relayed or ingested from a peer.
func AuditEventEnvelope(e AuditEvent) Envelope {
	payload, _ := json.Marshal(auditEventPayload{
		ActorPrincipal:  e.ActorPrincipal,
		OnBehalfOf:      e.OnBehalfOf,
		Action:          e.Action,
		Target:          e.Target,
		Result:          e.Result,
		ClockAssessment: e.ClockAssessment,
		Purpose:         e.Purpose,
		IssuedVia:       e.IssuedVia,

		Detail: e.Detail})
	return Envelope{
		ID:             e.ID,
		Schema:         SchemaAuditEvent,
		TS:             e.TS,
		ScopeNode:      e.ScopeNode,
		TraceID:        e.TraceID,
		CostClass:      auditEventCostClass,
		RetentionClass: auditEventRetentionClass,
		Origin:         "internal",
		Payload:        payload,
	}
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
