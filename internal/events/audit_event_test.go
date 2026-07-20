package events

import (
	"encoding/json"
	"errors"
	"testing"
)

// validAuditEventPayload is the EVT-080 corpus payload (the expected
// constructed envelope's payload, EVT-080-valid-audit-login-failure.json): a
// failed login carries every field — on_behalf_of null (self-attributed),
// target still present — even though the action failed (EVT-083).
func validAuditEventPayload() json.RawMessage {
	return json.RawMessage(`{
		"actor_principal": "01J8Z3K4N5P6Q7R8S9T0V1W2YE",
		"on_behalf_of": null,
		"action": "login.failure",
		"target": "principal:01J8Z3K4N5P6Q7R8S9T0V1W2YE",
		"result": "failure"
	}`)
}

// TestAuditEvent_CorpusPayloadValidates: the EVT-080 corpus payload validates
// against its own field definition, including a result: failure with every
// other field still present (EVT-083).
func TestAuditEvent_CorpusPayloadValidates(t *testing.T) {
	if err := validateAuditEvent(validAuditEventPayload()); err != nil {
		t.Fatalf("the EVT-080 corpus audit.event payload must validate; got %v", err)
	}
}

// TestAuditEvent_FailureMissingTargetRejected: EVT-083 — result: failure
// MUST still carry every other field, including target; a failed action is
// never elided of its target for having failed.
func TestAuditEvent_FailureMissingTargetRejected(t *testing.T) {
	p := json.RawMessage(`{
		"actor_principal": "01J8Z3K4N5P6Q7R8S9T0V1W2YE",
		"on_behalf_of": null,
		"action": "login.failure",
		"result": "failure"
	}`)
	err := validateAuditEvent(p)
	var ve ValidationError
	if !errors.As(err, &ve) || ve.Field != "target" {
		t.Fatalf("a result:failure audit.event missing target must be rejected on target (EVT-083); got %v", err)
	}
}

// TestAuditEvent_ResultEnumBites: result is the closed EVT-080 enum
// {success,failure} — a value outside it is rejected.
func TestAuditEvent_ResultEnumBites(t *testing.T) {
	p := json.RawMessage(`{
		"actor_principal": "01J8Z3K4N5P6Q7R8S9T0V1W2YE",
		"on_behalf_of": null,
		"action": "login.failure",
		"target": "principal:01J8Z3K4N5P6Q7R8S9T0V1W2YE",
		"result": "maybe"
	}`)
	err := validateAuditEvent(p)
	var ve ValidationError
	if !errors.As(err, &ve) || ve.Field != "result" {
		t.Fatalf("result outside {success,failure} must be rejected on result (EVT-080); got %v", err)
	}
}

// TestAuditEvent_SuccessResultValid: result: success also validates, with
// every field present.
func TestAuditEvent_SuccessResultValid(t *testing.T) {
	p := json.RawMessage(`{
		"actor_principal": "01J8Z3K4N5P6Q7R8S9T0V1W2YE",
		"on_behalf_of": null,
		"action": "login.success",
		"target": "principal:01J8Z3K4N5P6Q7R8S9T0V1W2YE",
		"result": "success"
	}`)
	if err := validateAuditEvent(p); err != nil {
		t.Fatalf("a result:success audit.event with every field must validate; got %v", err)
	}
}

// TestAuditEvent_OnBehalfOfOptional: on_behalf_of is a ULID, optional (EVT-080)
// — absent, an explicit null, and a valid ULID all validate; a malformed
// non-null value is rejected.
func TestAuditEvent_OnBehalfOfOptional(t *testing.T) {
	absent := json.RawMessage(`{
		"actor_principal": "01J8Z3K4N5P6Q7R8S9T0V1W2YE",
		"action": "pack.install",
		"target": "pack:acme-widgets",
		"result": "success"
	}`)
	if err := validateAuditEvent(absent); err != nil {
		t.Fatalf("an absent on_behalf_of must validate (EVT-080, self-attributed); got %v", err)
	}

	delegated := json.RawMessage(`{
		"actor_principal": "01J8Z3K4N5P6Q7R8S9T0V1W2YE",
		"on_behalf_of": "01J8Z3K4N5P6Q7R8S9T0V1W2Y8",
		"action": "pack.install",
		"target": "pack:acme-widgets",
		"result": "success"
	}`)
	if err := validateAuditEvent(delegated); err != nil {
		t.Fatalf("a valid ULID on_behalf_of must validate (EVT-080, delegated action); got %v", err)
	}

	bad := json.RawMessage(`{
		"actor_principal": "01J8Z3K4N5P6Q7R8S9T0V1W2YE",
		"on_behalf_of": "not-a-ulid",
		"action": "pack.install",
		"target": "pack:acme-widgets",
		"result": "success"
	}`)
	err := validateAuditEvent(bad)
	var ve ValidationError
	if !errors.As(err, &ve) || ve.Field != "on_behalf_of" {
		t.Fatalf("a malformed on_behalf_of must be rejected on on_behalf_of (EVT-080); got %v", err)
	}
}

// TestAuditEvent_BadActorPrincipalULIDRejected: actor_principal is a required
// ULID (EVT-080).
func TestAuditEvent_BadActorPrincipalULIDRejected(t *testing.T) {
	p := json.RawMessage(`{
		"actor_principal": "not-a-ulid",
		"on_behalf_of": null,
		"action": "login.failure",
		"target": "principal:01J8Z3K4N5P6Q7R8S9T0V1W2YE",
		"result": "failure"
	}`)
	err := validateAuditEvent(p)
	var ve ValidationError
	if !errors.As(err, &ve) || ve.Field != "actor_principal" {
		t.Fatalf("a non-ULID actor_principal must be rejected on actor_principal (EVT-080); got %v", err)
	}
}

// TestAuditEvent_MissingActionRejected: action is a required string
// (EVT-080).
func TestAuditEvent_MissingActionRejected(t *testing.T) {
	p := json.RawMessage(`{
		"actor_principal": "01J8Z3K4N5P6Q7R8S9T0V1W2YE",
		"on_behalf_of": null,
		"target": "principal:01J8Z3K4N5P6Q7R8S9T0V1W2YE",
		"result": "failure"
	}`)
	err := validateAuditEvent(p)
	var ve ValidationError
	if !errors.As(err, &ve) || ve.Field != "action" {
		t.Fatalf("a missing action must be rejected on action (EVT-080); got %v", err)
	}
}

// TestAuditEvent_DeliversThroughValidate: a well-formed audit.event envelope
// passes the full EVT-013 gate (delivered) even with result: failure
// (EVT-083).
func TestAuditEvent_DeliversThroughValidate(t *testing.T) {
	env := validEnvelope()
	env.Schema = SchemaAuditEvent
	env.RetentionClass = "audit-long"
	env.Payload = validAuditEventPayload()
	if err := Validate(env); err != nil {
		t.Fatalf("a well-formed audit.event envelope must deliver (EVT-013), including result:failure (EVT-083); got %v", err)
	}
}
