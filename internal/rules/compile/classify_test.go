package compile

import "testing"

func TestClassifyEdgeRule(t *testing.T) {
	r := mustRule(t, `{"id":"01J8Z3K4N5P6Q7R8S9T0V1RUL1","mode":"single","triggers":[{"type":"state","entity_id":"01J8Z3K4N5P6Q7R8S9T0V1W2Z2","to":["on"]}],"conditions":[{"type":"numeric","entity_id":"01J8Z3K4N5P6Q7R8S9T0V1W2Z3","below":80}],"actions":[{"type":"device_command","entity_id":"01J8Z3K4N5P6Q7R8S9T0V1W2Z2","command":"launch"}]}`)
	e := Classify(r)
	if e.ExecutionClass != "edge" || len(e.AppClassReasons) != 0 {
		t.Fatalf("classify = %+v, want edge with no reasons", e)
	}
	if e.RuleID != "01J8Z3K4N5P6Q7R8S9T0V1RUL1" {
		t.Fatalf("rule_id = %q", e.RuleID)
	}
}

func TestClassifyAppWhenChooseDefaultHasNotify(t *testing.T) {
	// RUL-182: an app-class action anywhere (a choose default's notify) forces
	// the whole rule app; RUL-006: the reason names the exact member.
	r := mustRule(t, `{"id":"01J8Z3K4N5P6Q7R8S9T0V1RUL8","mode":"queued","triggers":[{"type":"state","entity_id":"01J8Z3K4N5P6Q7R8S9T0V1W2Z2"}],"conditions":[],"actions":[{"type":"choose","branches":[{"condition":{"type":"state","entity_id":"01J8Z3K4N5P6Q7R8S9T0V1W2Z2","state":"on"},"actions":[{"type":"log","message":"x"}]}],"default":[{"type":"notify","template":"t"}]}]}`)
	e := Classify(r)
	if e.ExecutionClass != "app" {
		t.Fatalf("classify = %+v, want app", e)
	}
	var sawNotify, sawMode bool
	for _, rr := range e.AppClassReasons {
		if rr.Field == "actions[0].default[0]" && rr.Type == "notify" {
			sawNotify = true
		}
		if rr.Field == "mode" && rr.Value == "queued" {
			sawMode = true
		}
	}
	if !sawNotify || !sawMode {
		t.Fatalf("reasons = %+v, want both the notify member and the queued mode", e.AppClassReasons)
	}
}

func TestClassifyAppWhenTemplateConditionNestedInComposition(t *testing.T) {
	// A template condition (app) nested inside an and-composition forces app,
	// named by its composition field path.
	r := mustRule(t, `{"id":"01J8Z3K4N5P6Q7R8S9T0V1RUL9","mode":"single","triggers":[{"type":"state","entity_id":"01J8Z3K4N5P6Q7R8S9T0V1W2Z2"}],"conditions":[{"and":[{"type":"state","entity_id":"01J8Z3K4N5P6Q7R8S9T0V1W2Z4","state":"on"},{"type":"template","expression":{"expr":"true"}}]}],"actions":[{"type":"log","message":"x"}]}`)
	e := Classify(r)
	if e.ExecutionClass != "app" {
		t.Fatalf("classify = %+v, want app", e)
	}
	found := false
	for _, rr := range e.AppClassReasons {
		if rr.Field == "conditions[0].and[1]" && rr.Type == "template" {
			found = true
		}
	}
	if !found {
		t.Fatalf("reasons = %+v, want the nested template condition named", e.AppClassReasons)
	}
}
