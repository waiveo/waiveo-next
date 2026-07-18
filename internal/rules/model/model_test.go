package model

import "testing"

func TestParseRuleDiscriminatesTriggerTypesAndEntityRef(t *testing.T) {
	raw := []byte(`{
	  "id":"01J8Z3K4N5P6Q7R8S9T0V1W2Z1","mode":"single","max":null,
	  "triggers":[{"type":"state","entity_id":"01J8Z3K4N5P6Q7R8S9T0V1W2Z2","from":["off"],"to":["on"]}],
	  "conditions":[],
	  "actions":[{"type":"device_command","entity_id":"01J8Z3K4N5P6Q7R8S9T0V1W2Z2","command":"launch"}]
	}`)
	r, err := ParseRule(raw)
	if err != nil {
		t.Fatalf("ParseRule: %v", err)
	}
	if r.Mode != "single" || r.Max != nil {
		t.Fatalf("mode=%q max=%v, want single/nil", r.Mode, r.Max)
	}
	if len(r.Triggers) != 1 || r.Triggers[0].Type != "state" {
		t.Fatalf("trigger type = %+v, want one 'state'", r.Triggers)
	}
	if r.Triggers[0].EntityRef == nil || r.Triggers[0].EntityRef.Present() != 1 {
		t.Fatalf("trigger EntityRef = %+v, want exactly one field set", r.Triggers[0].EntityRef)
	}
	if len(r.Actions) != 1 || r.Actions[0].Type != "device_command" {
		t.Fatalf("action = %+v, want device_command", r.Actions)
	}
}

func TestParseRuleCompositionAndChooseNestMembers(t *testing.T) {
	raw := []byte(`{
	  "id":"01J8Z3K4N5P6Q7R8S9T0V1W2Z1","mode":"single",
	  "triggers":[{"type":"state","entity_id":"01J8Z3K4N5P6Q7R8S9T0V1W2Z2"}],
	  "conditions":[{"and":[{"type":"state","entity_id":"01J8Z3K4N5P6Q7R8S9T0V1W2Z4","state":"on"},{"not":{"type":"template","expression":{"expr":"true"}}}]}],
	  "actions":[{"type":"choose","branches":[{"condition":{"type":"state","entity_id":"01J8Z3K4N5P6Q7R8S9T0V1W2Z2","state":"playing"},"actions":[{"type":"log","message":"x"}]}],"default":[{"type":"notify","template":"t"}]}]
	}`)
	r, err := ParseRule(raw)
	if err != nil {
		t.Fatalf("ParseRule: %v", err)
	}
	c := r.Conditions[0]
	if c.Composition != "and" || len(c.Children) != 2 {
		t.Fatalf("condition composition = %q children=%d, want and/2", c.Composition, len(c.Children))
	}
	if c.Children[1].Composition != "not" || len(c.Children[1].Children) != 1 {
		t.Fatalf("not child = %+v", c.Children[1])
	}
	a := r.Actions[0]
	if a.Type != "choose" || len(a.Branches) != 1 || len(a.Default) != 1 {
		t.Fatalf("choose = %+v, want 1 branch + 1 default", a)
	}
	if a.Branches[0].Actions[0].Type != "log" || a.Default[0].Type != "notify" {
		t.Fatalf("choose nested actions = %+v", a)
	}
}
