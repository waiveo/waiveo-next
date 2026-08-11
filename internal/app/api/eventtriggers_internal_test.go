package api

import (
	"encoding/json"
	"testing"

	"github.com/maaxton/waiveo-next/internal/events"
	"github.com/maaxton/waiveo-next/internal/rules/model"
)

// Unit-level guards for the two decisions the end-to-end cases in
// eventtriggers_e2e_test.go cannot separate: what an automation row's MISSING
// `enabled` flag means, and how a `match` constraint compares values.

// TestAutomationEnabledFailsClosed pins the direction of the default.
//
// Treating an ABSENT flag as enabled would make any row this decoder cannot
// understand FIRE — the wrong way round for a mechanism whose only failure mode
// is acting when it should not. The explicit-false case is what an operator's
// "disable" writes; the absent and unparseable cases are what a row from
// anywhere else looks like.
func TestAutomationEnabledFailsClosed(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"explicitly enabled", `{"enabled":true}`, true},
		{"explicitly disabled", `{"enabled":false}`, false},
		{"flag absent", `{"name":"x"}`, false},
		{"unparseable row", `not json`, false},
		{"flag null", `{"enabled":null}`, false},
	}
	for _, c := range cases {
		if got := automationEnabled([]byte(c.body)); got != c.want {
			t.Errorf("%s: automationEnabled(%s) = %v, want %v", c.name, c.body, got, c.want)
		}
	}
}

// TestMatchConstraintsCompareByJSONValue: a constraint holds when the payload's
// TOP-LEVEL field equals it as a JSON value — so a number written `2` in a rule
// matches `2` in a payload without this file having an opinion about float64,
// and an absent field never satisfies a constraint.
func TestMatchConstraintsCompareByJSONValue(t *testing.T) {
	payload := json.RawMessage(`{"interaction":"call_service","count":2,"nested":{"a":1}}`)

	hold := func(match string) bool {
		var m map[string]json.RawMessage
		if err := json.Unmarshal([]byte(match), &m); err != nil {
			t.Fatalf("bad match fixture %s: %v", match, err)
		}
		return matchConstraintsHold(m, payload)
	}

	if !hold(`{}`) {
		t.Error("no constraints must match any event of that name")
	}
	if !hold(`{"interaction":"call_service"}`) {
		t.Error("an exact string constraint must hold")
	}
	if !hold(`{"count":2}`) {
		t.Error("a numeric constraint must hold by JSON value, not by Go type")
	}
	if !hold(`{"nested":{"a":1}}`) {
		t.Error("an object constraint must compare structurally")
	}
	if hold(`{"interaction":"other"}`) {
		t.Error("a different value must not match")
	}
	if hold(`{"interation":"call_service"}`) {
		t.Error("a constraint on a field the payload does not carry must FAIL, not be ignored — ignoring it silently widens the rule to every event of that schema")
	}
	if hold(`{"interaction":"call_service","count":3}`) {
		t.Error("EVERY constraint must hold, not merely one")
	}
}

// TestRuleMatchesEventOnlyOnItsOwnSchema: a rule fires once per matching event,
// and only on the durable event name it names.
func TestRuleMatchesEventOnlyOnItsOwnSchema(t *testing.T) {
	rule, err := model.ParseRule([]byte(`{
	  "id":"01J8Z3K4N5P6Q7R8S9T0V1W2Z2",
	  "triggers":[{"type":"event","event":"screen.interaction","match":{"interaction":"call_service"}}],
	  "actions":[{"type":"log","message":"x"}]
	}`))
	if err != nil {
		t.Fatalf("ParseRule: %v", err)
	}
	env := func(schema, interaction string) events.Envelope {
		return events.Envelope{Schema: schema, Payload: json.RawMessage(`{"interaction":"` + interaction + `"}`)}
	}
	if !ruleMatchesEvent(rule, env(events.SchemaScreenInteraction, "call_service")) {
		t.Error("the named event with a satisfied match must fire")
	}
	if ruleMatchesEvent(rule, env(events.SchemaScreenInteraction, "other")) {
		t.Error("an unsatisfied match must not fire")
	}
	if ruleMatchesEvent(rule, env(events.SchemaDeviceHeartbeat, "call_service")) {
		t.Error("another schema must not fire a rule that names screen.interaction")
	}

	// A rule with no event trigger at all — the ordinary state-triggered rule
	// every deployment is full of — must never be fired by an event.
	stateRule, err := model.ParseRule([]byte(`{
	  "id":"01J8Z3K4N5P6Q7R8S9T0V1W2Z3",
	  "triggers":[{"type":"state","entity_id":"01J8Z3K4N5P6Q7R8S9T0V1W2Z4","to":["on"]}],
	  "actions":[{"type":"log","message":"x"}]
	}`))
	if err != nil {
		t.Fatalf("ParseRule: %v", err)
	}
	if ruleMatchesEvent(stateRule, env(events.SchemaScreenInteraction, "call_service")) {
		t.Error("a rule with no event trigger must never be fired by an event")
	}
}
