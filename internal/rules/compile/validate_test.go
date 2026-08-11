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

// signageRule wraps one signage action in an otherwise-minimal valid rule.
func signageRule(t *testing.T, action string) model.Rule {
	t.Helper()
	return mustRule(t, `{"id":"01J8Z3K4N5P6Q7R8S9T0V1RUL1","mode":"single","triggers":[{"type":"state","entity_id":"01J8Z3K4N5P6Q7R8S9T0V1W2Z2"}],"conditions":[],"actions":[`+action+`]}`)
}

// TestValidateRejectsASignageActionThatDeclaresNoContent is RUL-234/235's
// required-member half at the gate that is supposed to enforce it.
//
// It was enforced NOWHERE. The executor guarded against both shapes and then
// returned silently, its own comment asserting "the compile gate is where an
// author is told" — so an author stored a play_cast naming no cast (201),
// pressed Run, and was told the rule `ran` with an empty effect report and an
// unchanged screen. A refusal at authoring is the only point at which the person
// who made the mistake is still looking at it.
func TestValidateRejectsASignageActionThatDeclaresNoContent(t *testing.T) {
	cases := []struct {
		name   string
		action string
		field  string
	}{
		{
			"play_cast with no cast_id",
			`{"type":"play_cast","screen_id":"01J8Z0D0000000000000000000"}`,
			"actions[0].cast_id",
		},
		{
			"show_alert with neither cast_id nor message",
			`{"type":"show_alert","selector":"zone=lobby"}`,
			"actions[0].cast_id",
		},
		{
			"show_alert with both cast_id and message",
			`{"type":"show_alert","screen_id":"01J8Z0D0000000000000000000","cast_id":"01J8Z0C0000000000000000000","message":"evacuate"}`,
			"actions[0].cast_id",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := Validate(signageRule(t, tc.action))
			if e == nil {
				t.Fatalf("%s compiled clean, want SIGNAGE_CONTENT_AMBIGUOUS", tc.name)
			}
			if e.Code != "SIGNAGE_CONTENT_AMBIGUOUS" {
				t.Fatalf("code = %q, want SIGNAGE_CONTENT_AMBIGUOUS (%+v)", e.Code, e)
			}
			// RUL-006 addressing: the author is told WHICH member of WHICH action.
			if e.Field != tc.field {
				t.Errorf("field = %q, want %q — a refusal that does not name the member makes the author hunt for it", e.Field, tc.field)
			}
		})
	}
}

// TestValidateAcceptsAWellFormedSignageAction is the other side, and the guard
// against a check that refuses everything: each legal shape still compiles,
// including dismiss_alert, which declares NEITHER content member on purpose (it
// clears whatever is there) and must not be held to show_alert's rule.
func TestValidateAcceptsAWellFormedSignageAction(t *testing.T) {
	for _, action := range []string{
		`{"type":"play_cast","screen_id":"01J8Z0D0000000000000000000","cast_id":"01J8Z0C0000000000000000000"}`,
		`{"type":"show_alert","selector":"zone=lobby","cast_id":"01J8Z0C0000000000000000000"}`,
		`{"type":"show_alert","screen_id":"01J8Z0D0000000000000000000","message":"evacuate","ttl_seconds":60}`,
		`{"type":"dismiss_alert","selector":"zone=lobby"}`,
	} {
		if e := Validate(signageRule(t, action)); e != nil {
			t.Errorf("%s was refused: %+v", action, e)
		}
	}
}

// TestValidateRejectsAWrongTypedSignageMember is the other half of RUL-234/235's
// required members, and it is the same defect entering through a different door.
//
// The required-member check read `cast_id` and `message` out of a probe struct
// of Go strings and returned nil on a decode failure ("malformed JSON is not
// this check's concern"). A member written at the WRONG TYPE — `cast_id` as a
// number, `message` as an object — fails exactly that decode, so it satisfied
// the check by breaking it: stored 201, then run as a completely silent no-op,
// `{"disposition":"ran","signage":[]}` against an unchanged screen. Presence was
// enforced; type was not, and to an author the two are one requirement.
//
// The `{"cast_id": 5, "message": "…"}` case is the reason a wrong-typed member
// cannot simply be READ AS ABSENT: doing that would compile it as a perfectly
// legal message-only alert with the author's own cast_id silently discarded.
func TestValidateRejectsAWrongTypedSignageMember(t *testing.T) {
	cases := []struct {
		name   string
		action string
		field  string
	}{
		{
			"play_cast whose cast_id is a number",
			`{"type":"play_cast","screen_id":"01J8Z0D0000000000000000000","cast_id":5}`,
			"actions[0].cast_id",
		},
		{
			"show_alert whose message is an object",
			`{"type":"show_alert","screen_id":"01J8Z0D0000000000000000000","message":{"text":"evacuate"}}`,
			"actions[0].message",
		},
		{
			"show_alert whose message is a number",
			`{"type":"show_alert","screen_id":"01J8Z0D0000000000000000000","message":42}`,
			"actions[0].message",
		},
		{
			"show_alert naming a wrong-typed cast_id ALONGSIDE a legal message",
			`{"type":"show_alert","screen_id":"01J8Z0D0000000000000000000","cast_id":5,"message":"evacuate"}`,
			"actions[0].cast_id",
		},
		{
			"play_cast whose screen_id is a number",
			`{"type":"play_cast","screen_id":7,"cast_id":"01J8Z0C0000000000000000000"}`,
			"actions[0].screen_id",
		},
		// `selector` is not listed here on purpose: it IS a member of the shared
		// leaf decode (model.decodeMember), so a wrong-typed one never reaches
		// Validate at all — ParseRule refuses the whole rule first. `screen_id`,
		// `cast_id`, `message` and `ttl_seconds` are the members that decode
		// carries no knowledge of, which is exactly why they were the ones that
		// slipped through.
		{
			"show_alert whose ttl_seconds is a string",
			`{"type":"show_alert","screen_id":"01J8Z0D0000000000000000000","message":"evacuate","ttl_seconds":"60"}`,
			"actions[0].ttl_seconds",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := Validate(signageRule(t, tc.action))
			if e == nil {
				t.Fatalf("%s compiled clean, want MEMBER_TYPE_INVALID", tc.name)
			}
			if e.Code != "MEMBER_TYPE_INVALID" {
				t.Fatalf("code = %q, want MEMBER_TYPE_INVALID (%+v)", e.Code, e)
			}
			if e.Field != tc.field {
				t.Errorf("field = %q, want %q — a refusal that does not name the member makes the author hunt for it", e.Field, tc.field)
			}
		})
	}
}

// TestValidateReachesAWrongTypedSignageMemberInsideAChooseBranch: the type check
// walks wherever an action can appear, exactly as the required-member check does
// — a `choose` branch is where a real fleet rule puts its signage.
func TestValidateReachesAWrongTypedSignageMemberInsideAChooseBranch(t *testing.T) {
	r := signageRule(t, `{"type":"choose","branches":[{"condition":{"type":"state","entity_id":"01J8Z3K4N5P6Q7R8S9T0V1W2Z2","state":"on"},"actions":[{"type":"play_cast","screen_id":"01J8Z0D0000000000000000000","cast_id":5}]}],"default":[]}`)
	e := Validate(r)
	if e == nil || e.Code != "MEMBER_TYPE_INVALID" {
		t.Fatalf("got %+v, want MEMBER_TYPE_INVALID", e)
	}
	if e.Field != "actions[0].branches[0].actions[0].cast_id" {
		t.Errorf("field = %q, want the branch action's own address", e.Field)
	}
}

// TestValidateRejectsAnEntityIDThatIsNotAULID pins RUL-010's TYPE half: the
// entity_id form is "a single `entity_id` (ULID)", and the arity check next to
// this one says which member is declared while saying nothing about what is in
// it.
//
// The consequence was not confined to the rule: a manual run reports every
// target it dispatched to, and api/1 declares AutomationRunCommand.entity_id as
// a `Ulid` (`^[0-9A-HJKMNP-TV-Z]{26}$`, required). An authored "kitchen-tv"
// travelled verbatim into that member, so the run report violated its own
// declared schema on the error path — the path such an id inevitably takes,
// since an id that names no entity is precisely the one that fails to dispatch.
// Every other id in that report comes from the device registry, which validates
// its own; this was the only unvalidated source.
func TestValidateRejectsAnEntityIDThatIsNotAULID(t *testing.T) {
	cases := []struct {
		name  string
		rule  string
		field string
	}{
		{
			"an action target",
			`{"id":"01J8Z3K4N5P6Q7R8S9T0V1RUL1","mode":"single","triggers":[{"type":"state","entity_id":"01J8Z3K4N5P6Q7R8S9T0V1W2Z2"}],"conditions":[],"actions":[{"type":"device_command","entity_id":"kitchen-tv","command":"launch"}]}`,
			"actions[0].entity_id",
		},
		{
			"a trigger subject",
			`{"id":"01J8Z3K4N5P6Q7R8S9T0V1RUL1","mode":"single","triggers":[{"type":"state","entity_id":"lobby tv"}],"conditions":[],"actions":[{"type":"log","message":"x"}]}`,
			"triggers[0].entity_id",
		},
		{
			"a nested condition leaf",
			`{"id":"01J8Z3K4N5P6Q7R8S9T0V1RUL1","mode":"single","triggers":[{"type":"state","entity_id":"01J8Z3K4N5P6Q7R8S9T0V1W2Z2"}],"conditions":[{"and":[{"type":"state","entity_id":"01J8Z3K4N5P6Q7R8S9T0V1W2Z2","state":"on"},{"type":"state","entity_id":"nope","state":"on"}]}],"actions":[{"type":"log","message":"x"}]}`,
			"conditions[0].and[1].entity_id",
		},
		{
			// 26 characters, so a length check alone passes it — but I/L/O/U are
			// not Crockford symbols, and the api/1 Ulid pattern excludes them too.
			"a 26-character id using a non-Crockford symbol",
			`{"id":"01J8Z3K4N5P6Q7R8S9T0V1RUL1","mode":"single","triggers":[{"type":"state","entity_id":"01J8Z3K4N5P6Q7R8S9T0SCHEDU"}],"conditions":[],"actions":[{"type":"log","message":"x"}]}`,
			"triggers[0].entity_id",
		},
		{
			"a lowercase id",
			`{"id":"01J8Z3K4N5P6Q7R8S9T0V1RUL1","mode":"single","triggers":[{"type":"state","entity_id":"01j8z3k4n5p6q7r8s9t0v1w2z2"}],"conditions":[],"actions":[{"type":"log","message":"x"}]}`,
			"triggers[0].entity_id",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := Validate(mustRule(t, tc.rule))
			if e == nil {
				t.Fatalf("%s compiled clean, want MEMBER_TYPE_INVALID", tc.name)
			}
			if e.Code != "MEMBER_TYPE_INVALID" {
				t.Fatalf("code = %q, want MEMBER_TYPE_INVALID (%+v)", e.Code, e)
			}
			if e.Field != tc.field {
				t.Errorf("field = %q, want %q", e.Field, tc.field)
			}
		})
	}
}

// TestValidateAcceptsTheOtherTwoEntityRefForms is the guard against a ULID check
// that refuses everything: `selector` and `device_class` name no id at all and
// must not be held to a ULID rule, and a canonical entity_id still compiles.
func TestValidateAcceptsTheOtherTwoEntityRefForms(t *testing.T) {
	for _, ref := range []string{
		`"entity_id":"01J8Z3K4N5P6Q7R8S9T0V1W2Z2"`,
		`"selector":"zone=lobby"`,
		`"device_class":"media-player"`,
	} {
		r := mustRule(t, `{"id":"01J8Z3K4N5P6Q7R8S9T0V1RUL1","mode":"single","triggers":[{"type":"state","entity_id":"01J8Z3K4N5P6Q7R8S9T0V1W2Z2"}],"conditions":[],"actions":[{"type":"device_command",`+ref+`,"command":"launch"}]}`)
		if e := Validate(r); e != nil {
			t.Errorf("%s was refused: %+v", ref, e)
		}
	}
}

// TestValidateReachesASignageActionInsideAChooseBranch: the content check runs
// wherever an action can appear, not only at the top of the sequence. A `choose`
// branch is where a real fleet rule puts its signage ("if the alarm is on, show
// the evacuation cast"), so a check that only walked actions[] would miss the
// rules most worth catching.
func TestValidateReachesASignageActionInsideAChooseBranch(t *testing.T) {
	r := signageRule(t, `{"type":"choose","branches":[{"condition":{"type":"state","entity_id":"01J8Z3K4N5P6Q7R8S9T0V1W2Z2","state":"on"},"actions":[{"type":"play_cast","screen_id":"01J8Z0D0000000000000000000"}]}],"default":[]}`)
	e := Validate(r)
	if e == nil || e.Code != "SIGNAGE_CONTENT_AMBIGUOUS" {
		t.Fatalf("got %+v, want SIGNAGE_CONTENT_AMBIGUOUS", e)
	}
	if e.Field != "actions[0].branches[0].actions[0].cast_id" {
		t.Errorf("field = %q, want the branch action's own address", e.Field)
	}
}
