package events

import (
	"encoding/json"
	"testing"
)

// The EVT-055 screen.interaction payload gate.
//
// Every required field is asserted individually, because the ONE thing this
// event is for is being matched on: an automation triggers on `interaction`, and
// a payload missing it is an event that can never fire anything while still
// looking, in a console, like a press that was recorded.

func screenInteractionPayload(over map[string]any) json.RawMessage {
	m := map[string]any{
		"screen_id":   "01J8Z3K4N5P6Q7R8S9T0V1W2ZE",
		"interaction": "call_service",
		"lease_id":    "01J8Z3K4N5P6Q7R8S9T0V1W2ZL",
		"at":          1752537000000,
	}
	for k, v := range over {
		if v == nil {
			delete(m, k)
			continue
		}
		m[k] = v
	}
	raw, err := json.Marshal(m)
	if err != nil {
		panic(err)
	}
	return raw
}

func TestValidateScreenInteraction(t *testing.T) {
	if err := validateScreenInteraction(screenInteractionPayload(nil)); err != nil {
		t.Fatalf("a complete payload must validate, got %v", err)
	}
	// slide_id is OPTIONAL: a slide outside a cast genuinely has no cast-local
	// id, and requiring one would drop every press made on an alert or an inline
	// slide.
	if err := validateScreenInteraction(screenInteractionPayload(map[string]any{"slide_id": "front-desk"})); err != nil {
		t.Fatalf("an optional slide_id must be accepted, got %v", err)
	}

	for _, field := range []string{"screen_id", "interaction", "lease_id", "at"} {
		if err := validateScreenInteraction(screenInteractionPayload(map[string]any{field: nil})); err == nil {
			t.Errorf("a payload with no %s must be refused (EVT-013)", field)
		}
	}
	if err := validateScreenInteraction(screenInteractionPayload(map[string]any{"screen_id": "not-a-ulid"})); err == nil {
		t.Error("screen_id must be a ULID")
	}
	if err := validateScreenInteraction(screenInteractionPayload(map[string]any{"interaction": 7})); err == nil {
		t.Error("interaction must be a string")
	}
	if err := validateScreenInteraction(screenInteractionPayload(map[string]any{"at": "soon"})); err == nil {
		t.Error("at must be a Timestamp")
	}
	if err := validateScreenInteraction(screenInteractionPayload(map[string]any{"slide_id": 7})); err == nil {
		t.Error("slide_id, when present, must be a string")
	}
}

// TestScreenInteractionIsCarriedVerbatim pins EVT-057: no producer or consumer
// on the path may normalise the matched value. An automation matches it exactly
// (RUL-081), so a hop that trimmed or case-folded it would make a rule that
// reads correct never fire — with the console showing both the event and the
// rule looking perfectly healthy.
func TestScreenInteractionIsCarriedVerbatim(t *testing.T) {
	raw := screenInteractionPayload(map[string]any{"interaction": "call_service"})
	if err := validateScreenInteraction(raw); err != nil {
		t.Fatalf("validate: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["interaction"] != "call_service" {
		t.Errorf("interaction = %v; the delivery gate must not rewrite it", got["interaction"])
	}
}
