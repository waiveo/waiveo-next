package events

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// fieldrules_test.go pins the shared field-validation helpers every events/1
// payload validator is built from — the rules that decide whether the EVT-013
// delivery gate rejects a malformed event.
//
// They were exercised only INDIRECTLY, wherever some whole-payload test happened
// to reach one, and a mutation sweep found 23 of their branches could be deleted
// with the package, the rest of internal/..., and the events/1 conformance driver
// all still green. A field rule nothing holds is a rule a refactor can relax
// without anyone noticing, and the whole point of these is to be the boundary a
// producer's mistake stops at.
//
// Every helper is driven for three things: the ABSENT branch, the WRONG-TYPE
// branch, and a valid control. The control is not ceremony — a helper that
// rejected everything would satisfy both negative branches while rejecting every
// real event on the platform.

// obj builds the raw-message map the helpers take, from a JSON object literal.
func obj(t *testing.T, s string) map[string]json.RawMessage {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatalf("fixture %s: %v", s, err)
	}
	return m
}

// TestRequiredFieldHelpersRejectAbsentAndWrongTyped covers the helpers whose
// contract is "present, and of this type".
//
// JSON null is called out separately in several rows because it is the case
// encoding/json gets quietly wrong: unmarshalling a null into an existing value
// is a no-op that returns a NIL error, so a null would otherwise slip through as
// a zero value. Each of those helpers carries an explicit null pre-check for
// that reason, and each of those pre-checks was among the unheld branches.
func TestRequiredFieldHelpersRejectAbsentAndWrongTyped(t *testing.T) {
	for _, tc := range []struct {
		name    string
		body    string
		call    func(map[string]json.RawMessage) error
		wantErr bool
		// wantDetail is the ValidationError.Detail the rejection must carry.
		// Asserting only "some error" cannot tell the ABSENT rule from the
		// WRONG-TYPE rule: with the absence check removed, an absent field still
		// fails the type check further down, so err != nil holds either way and
		// the mutant survives. Measured — that is exactly what happened.
		wantDetail string
	}{
		// requireStringField
		{"string: absent", `{}`, func(m map[string]json.RawMessage) error { return requireStringField(m, "f") }, true, "required"},
		{"string: null", `{"f":null}`, func(m map[string]json.RawMessage) error { return requireStringField(m, "f") }, true, "must be a string"},
		{"string: a number", `{"f":1}`, func(m map[string]json.RawMessage) error { return requireStringField(m, "f") }, true, "must be a string"},
		{"string: valid", `{"f":"x"}`, func(m map[string]json.RawMessage) error { return requireStringField(m, "f") }, false, ""},

		// requireULIDField
		{"ulid: absent", `{}`, func(m map[string]json.RawMessage) error { return requireULIDField(m, "f") }, true, "required"},
		{"ulid: not a string", `{"f":7}`, func(m map[string]json.RawMessage) error { return requireULIDField(m, "f") }, true, ""},
		{"ulid: a string that is not a ULID", `{"f":"nope"}`, func(m map[string]json.RawMessage) error { return requireULIDField(m, "f") }, true, ""},
		{"ulid: valid", `{"f":"01J8Z3K4N5P6Q7R8S9T0V1W2Y7"}`, func(m map[string]json.RawMessage) error { return requireULIDField(m, "f") }, false, ""},

		// requireObjectField / requireArrayField, and the isJSONObject rule that
		// an array is NOT an object.
		{"object: absent", `{}`, func(m map[string]json.RawMessage) error { return requireObjectField(m, "f") }, true, "required"},
		{"object: null", `{"f":null}`, func(m map[string]json.RawMessage) error { return requireObjectField(m, "f") }, true, "must be an object"},
		{"object: an array", `{"f":[]}`, func(m map[string]json.RawMessage) error { return requireObjectField(m, "f") }, true, "must be an object"},
		{"object: valid", `{"f":{}}`, func(m map[string]json.RawMessage) error { return requireObjectField(m, "f") }, false, ""},
		{"array: absent", `{}`, func(m map[string]json.RawMessage) error { return requireArrayField(m, "f") }, true, "required"},
		{"array: null", `{"f":null}`, func(m map[string]json.RawMessage) error { return requireArrayField(m, "f") }, true, "must be an array"},
		{"array: an object", `{"f":{}}`, func(m map[string]json.RawMessage) error { return requireArrayField(m, "f") }, true, "must be an array"},
		{"array: empty is valid", `{"f":[]}`, func(m map[string]json.RawMessage) error { return requireArrayField(m, "f") }, false, ""},

		// requireNullableStringField: absent is a refusal, null is NOT.
		{"nullable string: absent", `{}`, func(m map[string]json.RawMessage) error { return requireNullableStringField(m, "f") }, true, "required"},
		{"nullable string: null is accepted", `{"f":null}`, func(m map[string]json.RawMessage) error { return requireNullableStringField(m, "f") }, false, ""},
		{"nullable string: a number", `{"f":1}`, func(m map[string]json.RawMessage) error { return requireNullableStringField(m, "f") }, true, "must be a string or null"},
		{"nullable string: valid", `{"f":"x"}`, func(m map[string]json.RawMessage) error { return requireNullableStringField(m, "f") }, false, ""},

		// The OPTIONAL helpers: absent must be accepted. Testing only the
		// rejection leaves an over-strict implementation passing.
		{"optional object: absent is accepted", `{}`, func(m map[string]json.RawMessage) error { return optionalObjectField(m, "f") }, false, ""},
		{"optional object: an array", `{"f":[]}`, func(m map[string]json.RawMessage) error { return optionalObjectField(m, "f") }, true, "must be an object when present"},
		{"optional object: valid", `{"f":{}}`, func(m map[string]json.RawMessage) error { return optionalObjectField(m, "f") }, false, ""},
		{"optional ulid: absent is accepted", `{}`, func(m map[string]json.RawMessage) error { return optionalULIDField(m, "f") }, false, ""},
		{"optional ulid: null is accepted", `{"f":null}`, func(m map[string]json.RawMessage) error { return optionalULIDField(m, "f") }, false, ""},
		{"optional ulid: not a ULID", `{"f":"nope"}`, func(m map[string]json.RawMessage) error { return optionalULIDField(m, "f") }, true, "must be a valid ULID"},
		{"optional ulid: valid", `{"f":"01J8Z3K4N5P6Q7R8S9T0V1W2Y7"}`, func(m map[string]json.RawMessage) error { return optionalULIDField(m, "f") }, false, ""},

		// requireNonEmptyStringField adds emptiness on top of the string rule.
		{"non-empty string: absent", `{}`, func(m map[string]json.RawMessage) error { return requireNonEmptyStringField(m, "f") }, true, "required"},
		{"non-empty string: empty", `{"f":""}`, func(m map[string]json.RawMessage) error { return requireNonEmptyStringField(m, "f") }, true, ""},
		{"non-empty string: valid", `{"f":"x"}`, func(m map[string]json.RawMessage) error { return requireNonEmptyStringField(m, "f") }, false, ""},

		// requireScalarOrNullField
		{"scalar or null: absent", `{}`, func(m map[string]json.RawMessage) error { return requireScalarOrNullField(m, "f") }, true, "required"},
		{"scalar or null: null is accepted", `{"f":null}`, func(m map[string]json.RawMessage) error { return requireScalarOrNullField(m, "f") }, false, ""},
		{"scalar or null: an object", `{"f":{}}`, func(m map[string]json.RawMessage) error { return requireScalarOrNullField(m, "f") }, true, ""},
		{"scalar or null: a scalar", `{"f":3}`, func(m map[string]json.RawMessage) error { return requireScalarOrNullField(m, "f") }, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call(obj(t, tc.body))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("accepted %s — the EVT-013 delivery gate would admit this payload", tc.body)
				}
				var ve ValidationError
				if !errors.As(err, &ve) {
					t.Fatalf("rejection of %s is not a ValidationError: %v", tc.body, err)
				}
				if tc.wantDetail != "" && !strings.Contains(ve.Detail, tc.wantDetail) {
					t.Errorf("rejected %s with detail %q, want one naming %q — a refusal for the wrong reason is "+
						"a different rule firing, not this one", tc.body, ve.Detail, tc.wantDetail)
				}
			}
			if !tc.wantErr && err != nil {
				t.Errorf("rejected the valid %s: %v — an over-strict field rule refuses real events", tc.body, err)
			}
		})
	}
}

// requiredDetail asserts a rejection carries the ABSENT rule's reason rather
// than some later type rule's. Without it these helpers still error when their
// presence check is deleted — the missing field simply fails the type check
// below — so err != nil proves nothing about which rule fired.
func requiredDetail(t *testing.T, err error, what string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: absent field accepted", what)
	}
	var ve ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("%s: rejection is not a ValidationError: %v", what, err)
	}
	if !strings.Contains(ve.Detail, "required") {
		t.Errorf("%s: absent field rejected with %q, want the \"required\" rule", what, ve.Detail)
	}
}

// TestValueReturningFieldHelpers covers the helpers that also RETURN the parsed
// value, so a wrong value is as much a defect as a wrong verdict: a rule that
// accepted the field but returned a zero would pass an error-only assertion.
func TestValueReturningFieldHelpers(t *testing.T) {
	t.Run("bool", func(t *testing.T) {
		_, err := requireBoolField(obj(t, `{}`), "f")
		requiredDetail(t, err, "requireBoolField")
		if _, err := requireBoolField(obj(t, `{"f":null}`), "f"); err == nil {
			t.Error("null accepted as a bool — encoding/json unmarshals null as a no-op, so it would read as false")
		}
		if _, err := requireBoolField(obj(t, `{"f":"true"}`), "f"); err == nil {
			t.Error(`the string "true" accepted as a bool`)
		}
		if got, err := requireBoolField(obj(t, `{"f":true}`), "f"); err != nil || !got {
			t.Errorf("valid bool = (%v, %v), want (true, nil)", got, err)
		}
	})

	t.Run("int and int64 reject fractions and null", func(t *testing.T) {
		_, ierr := requireIntField(obj(t, `{}`), "f")
		requiredDetail(t, ierr, "requireIntField")
		_, i64err := requireInt64Field(obj(t, `{}`), "f")
		requiredDetail(t, i64err, "requireInt64Field")
		for _, body := range []string{`{}`, `{"f":null}`, `{"f":1.5}`, `{"f":"3"}`} {
			if _, err := requireIntField(obj(t, body), "f"); err == nil {
				t.Errorf("requireIntField accepted %s", body)
			}
			if _, err := requireInt64Field(obj(t, body), "f"); err == nil {
				t.Errorf("requireInt64Field accepted %s", body)
			}
		}
		if got, err := requireIntField(obj(t, `{"f":42}`), "f"); err != nil || got != 42 {
			t.Errorf("requireIntField = (%d, %v), want (42, nil)", got, err)
		}
		// The reason int64 exists apart from int: epoch-ms timestamps exceed a
		// 32-bit range, so a value that only fits in 64 bits must round-trip.
		if got, err := requireInt64Field(obj(t, `{"f":1752537600000}`), "f"); err != nil || got != 1752537600000 {
			t.Errorf("requireInt64Field = (%d, %v), want (1752537600000, nil)", got, err)
		}
	})

	t.Run("number", func(t *testing.T) {
		_, nerr := requireNumberField(obj(t, `{}`), "f")
		requiredDetail(t, nerr, "requireNumberField")
		for _, body := range []string{`{}`, `{"f":null}`, `{"f":"1"}`} {
			if _, err := requireNumberField(obj(t, body), "f"); err == nil {
				t.Errorf("requireNumberField accepted %s", body)
			}
		}
		if got, err := requireNumberField(obj(t, `{"f":1.5}`), "f"); err != nil || got != 1.5 {
			t.Errorf("requireNumberField = (%v, %v), want (1.5, nil)", got, err)
		}
	})

	t.Run("string array", func(t *testing.T) {
		_, aerr := requireStringArrayField(obj(t, `{}`), "f")
		requiredDetail(t, aerr, "requireStringArrayField")
		for _, body := range []string{`{}`, `{"f":null}`, `{"f":"x"}`, `{"f":[1]}`} {
			if _, err := requireStringArrayField(obj(t, body), "f"); err == nil {
				t.Errorf("requireStringArrayField accepted %s", body)
			}
		}
		got, err := requireStringArrayField(obj(t, `{"f":["a","b"]}`), "f")
		if err != nil || len(got) != 2 || got[0] != "a" || got[1] != "b" {
			t.Errorf("requireStringArrayField = (%v, %v), want ([a b], nil)", got, err)
		}
	})

	t.Run("enum", func(t *testing.T) {
		valid := map[string]struct{}{"ran": {}, "skipped": {}}
		_, eerr := requireEnumField(obj(t, `{}`), "f", valid, "ran|skipped")
		requiredDetail(t, eerr, "requireEnumField")
		for _, body := range []string{`{}`, `{"f":1}`, `{"f":"exploded"}`} {
			if _, err := requireEnumField(obj(t, body), "f", valid, "ran|skipped"); err == nil {
				t.Errorf("requireEnumField accepted %s — membership is the whole rule", body)
			}
		}
		if got, err := requireEnumField(obj(t, `{"f":"ran"}`), "f", valid, "ran|skipped"); err != nil || got != "ran" {
			t.Errorf("requireEnumField = (%q, %v), want (ran, nil)", got, err)
		}
	})
}
