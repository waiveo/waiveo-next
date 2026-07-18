package compile

import (
	"testing"

	"github.com/maaxton/waiveo-next/internal/rules/model"
)

func mustRule(t *testing.T, raw string) model.Rule {
	t.Helper()
	r, err := model.ParseRule([]byte(raw))
	if err != nil {
		t.Fatalf("ParseRule: %v", err)
	}
	return r
}

func TestValidateRejectsUnknownTriggerType(t *testing.T) {
	r := mustRule(t, `{"id":"01J8Z3K4N5P6Q7R8S9T0V1RUL1","mode":"single","triggers":[{"type":"geofence","entity_id":"01J8Z3K4N5P6Q7R8S9T0V1W2Z2","region":"home"}],"conditions":[],"actions":[{"type":"log","message":"x"}]}`)
	e := Validate(r)
	if e == nil || e.Code != "UNKNOWN_VOCABULARY_MEMBER" || e.Field != "triggers[0].type" {
		t.Fatalf("got %+v, want UNKNOWN_VOCABULARY_MEMBER at triggers[0].type", e)
	}
}

func TestValidateRejectsAmbiguousEntityRef(t *testing.T) {
	r := mustRule(t, `{"id":"01J8Z3K4N5P6Q7R8S9T0V1RUL1","mode":"single","triggers":[{"type":"state","entity_id":"01J8Z3K4N5P6Q7R8S9T0V1W2Z2","device_class":"media-player"}],"conditions":[],"actions":[{"type":"log","message":"x"}]}`)
	e := Validate(r)
	if e == nil || e.Code != "ENTITY_REF_AMBIGUOUS" {
		t.Fatalf("got %+v, want ENTITY_REF_AMBIGUOUS", e)
	}
}

func TestValidateModeMax(t *testing.T) {
	missing := mustRule(t, `{"id":"01J8Z3K4N5P6Q7R8S9T0V1RUL1","mode":"parallel","triggers":[{"type":"state","entity_id":"01J8Z3K4N5P6Q7R8S9T0V1W2Z2"}],"conditions":[],"actions":[{"type":"log","message":"x"}]}`)
	if e := Validate(missing); e == nil || e.Code != "MODE_MAX_MISSING" {
		t.Fatalf("parallel-without-max got %+v, want MODE_MAX_MISSING", e)
	}
	notApplicable := mustRule(t, `{"id":"01J8Z3K4N5P6Q7R8S9T0V1RUL1","mode":"single","max":3,"triggers":[{"type":"state","entity_id":"01J8Z3K4N5P6Q7R8S9T0V1W2Z2"}],"conditions":[],"actions":[{"type":"log","message":"x"}]}`)
	if e := Validate(notApplicable); e == nil || e.Code != "MODE_MAX_NOT_APPLICABLE" {
		t.Fatalf("single-with-max got %+v, want MODE_MAX_NOT_APPLICABLE", e)
	}
}

func TestValidateRejectsUnknownMode(t *testing.T) {
	r := mustRule(t, `{"id":"01J8Z3K4N5P6Q7R8S9T0V1RUL1","mode":"turbo","triggers":[{"type":"state","entity_id":"01J8Z3K4N5P6Q7R8S9T0V1W2Z2"}],"conditions":[],"actions":[{"type":"log","message":"x"}]}`)
	e := Validate(r)
	if e == nil || e.Code != "UNKNOWN_VOCABULARY_MEMBER" || e.Field != "mode" {
		t.Fatalf("got %+v, want UNKNOWN_VOCABULARY_MEMBER at mode", e)
	}
}

func TestValidateRejectsInvalidMisfire(t *testing.T) {
	for _, kind := range []string{
		`{"type":"time","at":"08:00:00","misfire":"always"}`,
		`{"type":"time_pattern","minutes":"/15","misfire":"catch_up_twice"}`,
		`{"type":"sun","event":"sunrise","misfire":"nope"}`,
	} {
		r := mustRule(t, `{"id":"01J8Z3K4N5P6Q7R8S9T0V1RUL1","mode":"single","triggers":[`+kind+`],"conditions":[],"actions":[{"type":"log","message":"x"}]}`)
		e := Validate(r)
		if e == nil || e.Code != "MISFIRE_INVALID" || e.Field != "triggers[0].misfire" {
			t.Fatalf("trigger %s: got %+v, want MISFIRE_INVALID at triggers[0].misfire", kind, e)
		}
	}
}

func TestValidateAcceptsValidAndAbsentMisfire(t *testing.T) {
	for _, kind := range []string{
		`{"type":"time","at":"08:00:00"}`,                  // absent -> default skip, valid
		`{"type":"time","at":"08:00:00","misfire":"skip"}`, // explicit skip
		`{"type":"time_pattern","minutes":"/15","misfire":"catch_up_once"}`,
		`{"type":"sun","event":"sunset","misfire":"fire_each"}`,
	} {
		r := mustRule(t, `{"id":"01J8Z3K4N5P6Q7R8S9T0V1RUL1","mode":"single","triggers":[`+kind+`],"conditions":[],"actions":[{"type":"log","message":"x"}]}`)
		if e := Validate(r); e != nil {
			t.Fatalf("trigger %s: unexpected error %+v", kind, e)
		}
	}
}

func TestValidateFindsUnknownNestedInChooseDefault(t *testing.T) {
	r := mustRule(t, `{"id":"01J8Z3K4N5P6Q7R8S9T0V1RUL1","mode":"single","triggers":[{"type":"state","entity_id":"01J8Z3K4N5P6Q7R8S9T0V1W2Z2"}],"conditions":[],"actions":[{"type":"choose","branches":[{"condition":{"type":"state","entity_id":"01J8Z3K4N5P6Q7R8S9T0V1W2Z2","state":"on"},"actions":[{"type":"log","message":"x"}]}],"default":[{"type":"teleport"}]}]}`)
	e := Validate(r)
	if e == nil || e.Code != "UNKNOWN_VOCABULARY_MEMBER" || e.Field != "actions[0].default[0].type" {
		t.Fatalf("got %+v, want UNKNOWN_VOCABULARY_MEMBER at actions[0].default[0].type", e)
	}
}

// --- RUL-282: edge expression cross-entity restriction ---

// same-entity sub-case of RUL-282-edge-expression-cross-entity-rejected: an
// edge rule whose log-message expression sources its own trigger subject
// compiles, as an edge rule.
func TestValidateEdgeExpressionSameEntityCompiles(t *testing.T) {
	raw := `{"id":"01J8Z3K4N5P6Q7R8S9T0V1RUL1","mode":"single","triggers":[{"type":"state","entity_id":"01J8Z3K4N5P6Q7R8S9T0V1W2Z2","to":["on"]}],"conditions":[],"actions":[{"type":"log","message":{"expr":"state('01J8Z3K4N5P6Q7R8S9T0V1W2Z2')"}}]}`
	entry, cerr := Compile([]byte(raw))
	if cerr != nil {
		t.Fatalf("same-entity: unexpected compile error %+v", cerr)
	}
	if entry.ExecutionClass != "edge" {
		t.Fatalf("same-entity: execution_class = %q, want edge", entry.ExecutionClass)
	}
}

// other-entity sub-case: the same rule shape sourcing a DIFFERENT entity fails
// compilation with EDGE_EXPRESSION_CROSS_ENTITY_REFERENCE at actions[0].message.
func TestValidateEdgeExpressionOtherEntityRejected(t *testing.T) {
	raw := `{"id":"01J8Z3K4N5P6Q7R8S9T0V1RUL1","mode":"single","triggers":[{"type":"state","entity_id":"01J8Z3K4N5P6Q7R8S9T0V1W2Z2","to":["on"]}],"conditions":[],"actions":[{"type":"log","message":{"expr":"state('01J8Z3K4N5P6Q7R8S9T0V1W2Z6')"}}]}`
	_, cerr := Compile([]byte(raw))
	if cerr == nil {
		t.Fatalf("other-entity: expected compile failure, got success")
	}
	if cerr.Code != "EDGE_EXPRESSION_CROSS_ENTITY_REFERENCE" || cerr.Field != "actions[0].message" {
		t.Fatalf("other-entity: got %+v, want EDGE_EXPRESSION_CROSS_ENTITY_REFERENCE at actions[0].message", cerr)
	}
}

// A cross-entity source inside an action's params (RUL-393) is caught at the
// params value's field path, same restriction as a log message.
func TestValidateEdgeExpressionParamsCrossEntity(t *testing.T) {
	raw := `{"id":"01J8Z3K4N5P6Q7R8S9T0V1RUL1","mode":"single","triggers":[{"type":"state","entity_id":"01J8Z3K4N5P6Q7R8S9T0V1W2Z2","to":["on"]}],"conditions":[],"actions":[{"type":"device_command","entity_id":"01J8Z3K4N5P6Q7R8S9T0V1W2Z2","command":"launch","params":{"channel":{"expr":"state('01J8Z3K4N5P6Q7R8S9T0V1W2Z6')"}}}]}`
	_, cerr := Compile([]byte(raw))
	if cerr == nil || cerr.Code != "EDGE_EXPRESSION_CROSS_ENTITY_REFERENCE" || cerr.Field != "actions[0].params.channel" {
		t.Fatalf("params cross-entity: got %+v, want EDGE_EXPRESSION_CROSS_ENTITY_REFERENCE at actions[0].params.channel", cerr)
	}
}

// A literal params value (not an Expression sourcing an entity) is unaffected —
// mirrors the RUL-101 device_command params shape.
func TestValidateEdgeExpressionLiteralParamsOK(t *testing.T) {
	raw := `{"id":"01J8Z3K4N5P6Q7R8S9T0V1RUL1","mode":"single","triggers":[{"type":"state","entity_id":"01J8Z3K4N5P6Q7R8S9T0V1W2Z2","to":["on"]}],"conditions":[],"actions":[{"type":"device_command","entity_id":"01J8Z3K4N5P6Q7R8S9T0V1W2Z2","command":"launch","params":{"channel":"dev"}}]}`
	if _, cerr := Compile([]byte(raw)); cerr != nil {
		t.Fatalf("literal params: unexpected compile error %+v", cerr)
	}
}

// App-class rules are exempt from RUL-282: an app rule (here forced app by a
// queued mode) may source any entity in its expressions.
func TestValidateAppClassExpressionExemptFromCrossEntity(t *testing.T) {
	raw := `{"id":"01J8Z3K4N5P6Q7R8S9T0V1RUL1","mode":"queued","triggers":[{"type":"state","entity_id":"01J8Z3K4N5P6Q7R8S9T0V1W2Z2","to":["on"]}],"conditions":[],"actions":[{"type":"log","message":{"expr":"state('01J8Z3K4N5P6Q7R8S9T0V1W2Z6')"}}]}`
	entry, cerr := Compile([]byte(raw))
	if cerr != nil {
		t.Fatalf("app-class: unexpected compile error %+v", cerr)
	}
	if entry.ExecutionClass != "app" {
		t.Fatalf("app-class: execution_class = %q, want app", entry.ExecutionClass)
	}
}

// When a trigger's subject is dynamic (a selector/device_class EntityRef, whose
// firing entity is only known at runtime, RUL-011), the cross-entity source
// cannot be statically decided — compile defers to eval-time enforcement rather
// than risk a false positive.
func TestValidateDynamicTriggerDefersCrossEntity(t *testing.T) {
	raw := `{"id":"01J8Z3K4N5P6Q7R8S9T0V1RUL1","mode":"single","triggers":[{"type":"state","selector":"device_class==media-player","to":["on"]}],"conditions":[],"actions":[{"type":"log","message":{"expr":"state('01J8Z3K4N5P6Q7R8S9T0V1W2Z6')"}}]}`
	if _, cerr := Compile([]byte(raw)); cerr != nil {
		t.Fatalf("dynamic trigger: unexpected compile error %+v (should defer to eval)", cerr)
	}
}

// A filter outside the RUL-290 closed list is UNKNOWN_FILTER at the expression's
// field path, regardless of the rule's class.
func TestValidateUnknownFilterInExpression(t *testing.T) {
	raw := `{"id":"01J8Z3K4N5P6Q7R8S9T0V1RUL1","mode":"single","triggers":[{"type":"state","entity_id":"01J8Z3K4N5P6Q7R8S9T0V1W2Z2","to":["on"]}],"conditions":[],"actions":[{"type":"log","message":{"expr":"state('01J8Z3K4N5P6Q7R8S9T0V1W2Z2') | bogus"}}]}`
	_, cerr := Compile([]byte(raw))
	if cerr == nil || cerr.Code != "UNKNOWN_FILTER" || cerr.Field != "actions[0].message" {
		t.Fatalf("unknown filter: got %+v, want UNKNOWN_FILTER at actions[0].message", cerr)
	}
}

func TestValidateAcceptsAWellFormedRule(t *testing.T) {
	r := mustRule(t, `{"id":"01J8Z3K4N5P6Q7R8S9T0V1RUL1","mode":"single","max":null,"triggers":[{"type":"state","entity_id":"01J8Z3K4N5P6Q7R8S9T0V1W2Z2","from":["off"],"to":["on"]}],"conditions":[],"actions":[{"type":"device_command","entity_id":"01J8Z3K4N5P6Q7R8S9T0V1W2Z2","command":"launch"}]}`)
	if e := Validate(r); e != nil {
		t.Fatalf("well-formed rule rejected: %+v", e)
	}
}
