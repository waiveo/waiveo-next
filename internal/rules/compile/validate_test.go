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

func TestValidateAcceptsAWellFormedRule(t *testing.T) {
	r := mustRule(t, `{"id":"01J8Z3K4N5P6Q7R8S9T0V1RUL1","mode":"single","max":null,"triggers":[{"type":"state","entity_id":"01J8Z3K4N5P6Q7R8S9T0V1W2Z2","from":["off"],"to":["on"]}],"conditions":[],"actions":[{"type":"device_command","entity_id":"01J8Z3K4N5P6Q7R8S9T0V1W2Z2","command":"launch"}]}`)
	if e := Validate(r); e != nil {
		t.Fatalf("well-formed rule rejected: %+v", e)
	}
}
