package events

import (
	"errors"
	"testing"
)

// validEnvelope is a well-formed EVT-010 envelope for a registered schema, using
// the frozen corpus fixture ULIDs (conformance/corpora/events-1). Its payload is
// the EVT-010 corpus entity.state_changed payload, so it validates end-to-end
// once the entity.state_changed validator is registered (Task 2).
func validEnvelope() Envelope {
	return Envelope{
		ID:              "01J8Z3K4N5P6Q7R8S9T0V1W2Y7",
		Schema:          "entity.state_changed",
		TS:              1752537600000,
		ScopeNode:       "01J8Z2Q1M8H8N4T0V1W2X3Y4Z5",
		TraceID:         "01J8Z3K4N5P6Q7R8S9T0V1W2Y7",
		CostClass:       "telemetry",
		RetentionClass:  "telemetry-standard",
		Origin:          "relay",
		OriginPrincipal: "01J8Z3K4N5P6Q7R8S9T0V1W2Y8",
		Payload:         validEntityStateChangedPayload(),
	}
}

// TestValidate_RegisteredSchemaValidPayloadDelivers: a well-formed envelope for a
// registered schema whose payload conforms to that schema validates, so the
// event is deliverable (EVT-010/013).
func TestValidate_RegisteredSchemaValidPayloadDelivers(t *testing.T) {
	if err := Validate(validEnvelope()); err != nil {
		t.Fatalf("a well-formed envelope for a registered schema must validate (EVT-010/013); got %v", err)
	}
}

// TestValidate_UnregisteredBareSchemaRejected: an unregistered bare schema is an
// EVT-013 rejection (never delivered), distinct from the EVT-022 pack sentinel.
func TestValidate_UnregisteredBareSchemaRejected(t *testing.T) {
	env := validEnvelope()
	env.Schema = "unknown.schema"
	err := Validate(env)
	if err == nil {
		t.Fatal("an unregistered bare schema must be rejected so it is never delivered (EVT-013)")
	}
	if errors.Is(err, ErrPackSchema) {
		t.Fatalf("an unregistered bare schema is EVT-013, not the EVT-022 pack sentinel; got %v", err)
	}
	var ve ValidationError
	if !errors.As(err, &ve) || ve.Field != "schema" {
		t.Fatalf("expected a ValidationError on the schema field (EVT-013); got %v", err)
	}
}

// TestValidate_PackSchemaSentinel: a '/'-bearing pack schema returns the distinct
// EVT-022 sentinel — out of this catalog, neither a registered match nor a
// registered-schema validation failure.
func TestValidate_PackSchemaSentinel(t *testing.T) {
	env := validEnvelope()
	env.Schema = "acme/widget.pinged"
	err := Validate(env)
	if !errors.Is(err, ErrPackSchema) {
		t.Fatalf("a slash-bearing pack schema must return the distinct EVT-022 sentinel; got %v", err)
	}
}

// TestValidate_EnvelopeFieldViolations: each missing/malformed required envelope
// field fails validation (EVT-010), so the event is never delivered (EVT-013).
func TestValidate_EnvelopeFieldViolations(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Envelope)
	}{
		{"bad id ULID", func(e *Envelope) { e.ID = "nope" }},
		{"empty schema", func(e *Envelope) { e.Schema = "" }},
		{"ts absent", func(e *Envelope) { e.TS = 0 }},
		{"bad scope_node ULID", func(e *Envelope) { e.ScopeNode = "not-a-ulid" }},
		{"bad trace_id ULID", func(e *Envelope) { e.TraceID = "nope" }},
		{"empty cost_class", func(e *Envelope) { e.CostClass = "" }},
		{"empty retention_class", func(e *Envelope) { e.RetentionClass = "" }},
		{"empty origin", func(e *Envelope) { e.Origin = "" }},
		{"origin spork out of enum", func(e *Envelope) { e.Origin = "spork" }},
		{"bad origin_principal ULID when present", func(e *Envelope) { e.OriginPrincipal = "nope" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := validEnvelope()
			tc.mutate(&env)
			if err := Validate(env); err == nil {
				t.Fatalf("%s must fail envelope validation (EVT-010) so the event is not delivered (EVT-013)", tc.name)
			}
		})
	}
}

// TestValidate_OptionalOriginPrincipalAbsentOK: origin_principal is optional
// (EVT-010) — an empty value is accepted.
func TestValidate_OptionalOriginPrincipalAbsentOK(t *testing.T) {
	env := validEnvelope()
	env.OriginPrincipal = ""
	if err := Validate(env); err != nil {
		t.Fatalf("origin_principal is optional (EVT-010); an absent value must validate; got %v", err)
	}
}
