package events

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"
)

// TestAutomationRun_CorpusPayloadsValidate: the four first-half automation.run
// corpus payloads (ran/restarted/skipped/misfire-caught) each validate against
// the schema's field definition (EVT-040/041).
func TestAutomationRun_CorpusPayloadsValidate(t *testing.T) {
	payloads := map[string]json.RawMessage{
		"ran":       json.RawMessage(`{"rule_id":"01J8Z3K4N5P6Q7R8S9T0V1W2YC","rule_revision":4,"trigger_snapshot":{"kind":"state"},"condition_results":[{"passed":true}],"action_outcomes":[{"status":"ok"}],"mode_disposition":"ran","misfire_caught":false}`),
		"restarted": json.RawMessage(`{"rule_id":"01J8Z3K4N5P6Q7R8S9T0V1W2YC","rule_revision":5,"trigger_snapshot":{"kind":"state"},"condition_results":[{"passed":true}],"action_outcomes":[{"status":"ok"}],"mode_disposition":"restarted","misfire_caught":false}`),
		"skipped":   json.RawMessage(`{"rule_id":"01J8Z3K4N5P6Q7R8S9T0V1W2YC","rule_revision":5,"trigger_snapshot":{"kind":"state"},"condition_results":[],"action_outcomes":[],"mode_disposition":"skipped","misfire_caught":false}`),
		"misfire":   json.RawMessage(`{"rule_id":"01J8Z3K4N5P6Q7R8S9T0V1W2YC","rule_revision":5,"trigger_snapshot":{"kind":"time"},"condition_results":[{"passed":true}],"action_outcomes":[{"status":"ok"}],"mode_disposition":"ran","misfire_caught":true}`),
	}
	for name, p := range payloads {
		if err := validateAutomationRun(p); err != nil {
			t.Fatalf("%s: the corpus automation.run payload must validate (EVT-040); got %v", name, err)
		}
	}
}

// TestAutomationRun_ModeDispositionEnumBites: mode_disposition is a closed enum
// {ran,skipped,restarted} (EVT-041) — a value outside it is rejected.
func TestAutomationRun_ModeDispositionEnumBites(t *testing.T) {
	p := json.RawMessage(`{"rule_id":"01J8Z3K4N5P6Q7R8S9T0V1W2YC","rule_revision":4,"trigger_snapshot":{},"condition_results":[],"action_outcomes":[],"mode_disposition":"exploded","misfire_caught":false}`)
	err := validateAutomationRun(p)
	var ve ValidationError
	if !errors.As(err, &ve) || ve.Field != "mode_disposition" {
		t.Fatalf("mode_disposition outside {ran,skipped,restarted} must be rejected on mode_disposition (EVT-041); got %v", err)
	}
}

// TestAutomationRunEnvelope_MapsEvaluatorToOrigin: the constructor derives the
// envelope origin from the evaluator — relay for the edge evaluator, internal
// for any app-side evaluator (EVT-042).
func TestAutomationRunEnvelope_MapsEvaluatorToOrigin(t *testing.T) {
	relay := AutomationRunEnvelope("01J8Z2Q1M8H8N4T0V1W2X3Y4Z5", "01J8Z3K4N5P6Q7R8S9T0V1W2Y7", "01J8Z3K4N5P6Q7R8S9T0V1W2YC", 4,
		json.RawMessage(`{"kind":"state"}`), json.RawMessage(`[]`), json.RawMessage(`[]`), "ran", false, "relay", "01J8Z3K4N5P6Q7R8S9T0V1W2Y7", 1752537600000)
	if relay.Origin != "relay" {
		t.Fatalf("evaluator relay must map to origin relay (EVT-042); got %q", relay.Origin)
	}
	internal := AutomationRunEnvelope("01J8Z2Q1M8H8N4T0V1W2X3Y4Z5", "01J8Z3K4N5P6Q7R8S9T0V1W2Y7", "01J8Z3K4N5P6Q7R8S9T0V1W2YC", 5,
		json.RawMessage(`{"kind":"state"}`), json.RawMessage(`[]`), json.RawMessage(`[]`), "skipped", false, "internal", "01J8Z3K4N5P6Q7R8S9T0V1W2Y7", 1752537600000)
	if internal.Origin != "internal" {
		t.Fatalf("evaluator internal must map to origin internal (EVT-042); got %q", internal.Origin)
	}
	app := AutomationRunEnvelope("01J8Z2Q1M8H8N4T0V1W2X3Y4Z5", "01J8Z3K4N5P6Q7R8S9T0V1W2Y7", "01J8Z3K4N5P6Q7R8S9T0V1W2YC", 5,
		json.RawMessage(`{"kind":"state"}`), json.RawMessage(`[]`), json.RawMessage(`[]`), "ran", false, "app-eval-7", "01J8Z3K4N5P6Q7R8S9T0V1W2Y7", 1752537600000)
	if app.Origin != "internal" {
		t.Fatalf("a non-relay evaluator must map to origin internal (EVT-042); got %q", app.Origin)
	}
}

// TestAutomationRunEnvelope_DispositionVerbatimMisfireOrthogonal: the
// constructor maps disposition verbatim onto mode_disposition and passes
// misfire_caught through unchanged — misfire is orthogonal, never a fourth
// disposition (EVT-041/RUL-246): disposition ran + misfire true still yields
// mode_disposition ran.
func TestAutomationRunEnvelope_DispositionVerbatimMisfireOrthogonal(t *testing.T) {
	var got struct {
		ModeDisposition string `json:"mode_disposition"`
		MisfireCaught   bool   `json:"misfire_caught"`
	}

	ranMisfire := AutomationRunEnvelope("01J8Z2Q1M8H8N4T0V1W2X3Y4Z5", "01J8Z3K4N5P6Q7R8S9T0V1W2Y7", "01J8Z3K4N5P6Q7R8S9T0V1W2YC", 5,
		json.RawMessage(`{"kind":"time"}`), json.RawMessage(`[{"passed":true}]`), json.RawMessage(`[{"status":"ok"}]`), "ran", true, "relay", "01J8Z3K4N5P6Q7R8S9T0V1W2Y7", 1752537600000)
	if err := json.Unmarshal(ranMisfire.Payload, &got); err != nil {
		t.Fatalf("payload must unmarshal; got %v", err)
	}
	if got.ModeDisposition != "ran" {
		t.Fatalf("a caught misfire must still resolve to its own mode_disposition ran, never a fourth disposition (EVT-041); got %q", got.ModeDisposition)
	}
	if !got.MisfireCaught {
		t.Fatalf("misfire_caught must pass through unchanged (EVT-041); got %v", got.MisfireCaught)
	}

	restarted := AutomationRunEnvelope("01J8Z2Q1M8H8N4T0V1W2X3Y4Z5", "01J8Z3K4N5P6Q7R8S9T0V1W2Y7", "01J8Z3K4N5P6Q7R8S9T0V1W2YC", 5,
		json.RawMessage(`{"kind":"state"}`), json.RawMessage(`[{"passed":true}]`), json.RawMessage(`[{"status":"ok"}]`), "restarted", false, "relay", "01J8Z3K4N5P6Q7R8S9T0V1W2Y7", 1752537600000)
	if err := json.Unmarshal(restarted.Payload, &got); err != nil {
		t.Fatalf("payload must unmarshal; got %v", err)
	}
	if got.ModeDisposition != "restarted" {
		t.Fatalf("disposition restarted must map verbatim to mode_disposition restarted; got %q", got.ModeDisposition)
	}
}

// TestAutomationRunEnvelope_MatchesCorpusPayloadAndDelivers: the constructor
// produces exactly the EVT-040 corpus payload and an envelope that passes the
// EVT-013 gate (delivered).
func TestAutomationRunEnvelope_MatchesCorpusPayloadAndDelivers(t *testing.T) {
	env := AutomationRunEnvelope(
		"01J8Z2Q1M8H8N4T0V1W2X3Y4Z5",
		"01J8Z3K4N5P6Q7R8S9T0V1W2Y7",
		"01J8Z3K4N5P6Q7R8S9T0V1W2YC",
		4,
		json.RawMessage(`{"kind":"state","entity_id":"01J8Z3K4N5P6Q7R8S9T0V1W2Y9","to":"playing"}`),
		json.RawMessage(`[{"kind":"state","passed":true}]`),
		json.RawMessage(`[{"kind":"device-command","status":"ok"}]`),
		"ran", false, "relay",
		"01J8Z3K4N5P6Q7R8S9T0V1W2Y7", 1752537600000,
	)
	if env.Schema != SchemaAutomationRun {
		t.Fatalf("schema must be automation.run; got %q", env.Schema)
	}
	if env.Origin != "relay" {
		t.Fatalf("origin must be relay (EVT-042); got %q", env.Origin)
	}

	wantPayload := `{
		"rule_id": "01J8Z3K4N5P6Q7R8S9T0V1W2YC",
		"rule_revision": 4,
		"trigger_snapshot": { "kind": "state", "entity_id": "01J8Z3K4N5P6Q7R8S9T0V1W2Y9", "to": "playing" },
		"condition_results": [{ "kind": "state", "passed": true }],
		"action_outcomes": [{ "kind": "device-command", "status": "ok" }],
		"mode_disposition": "ran",
		"misfire_caught": false
	}`
	var got, want interface{}
	if err := json.Unmarshal(env.Payload, &got); err != nil {
		t.Fatalf("produced payload must be valid JSON; got %v", err)
	}
	if err := json.Unmarshal([]byte(wantPayload), &want); err != nil {
		t.Fatalf("want payload parse; got %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("produced automation.run payload must match the EVT-040 corpus payload\n got: %v\nwant: %v", got, want)
	}

	if err := Validate(env); err != nil {
		t.Fatalf("a constructor-built automation.run envelope must deliver through the EVT-013 gate; got %v", err)
	}
}
