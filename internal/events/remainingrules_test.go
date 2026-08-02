package events

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// detailOf returns a rejection's ValidationError.Detail, failing if the error is
// not one. Used because several rules below still produce SOME error when the
// rule under test is removed — a later check catches the same input for a
// different reason — so only the reason distinguishes them.
func detailOf(t *testing.T, err error) string {
	t.Helper()
	if err == nil {
		t.Fatal("expected a rejection, got none")
	}
	var ve ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("rejection is not a ValidationError: %v", err)
	}
	return ve.Detail
}

// TestEnvelopeSchemaMustBeNonEmpty pins the envelope's own schema rule.
//
// It needs the REASON, not just a rejection: an empty schema also fails the
// cost/retention class lookup further down, so removing this check still
// refuses the envelope — with a message about an unregistered schema, which
// tells a producer to go register one rather than to fill in the field.
func TestEnvelopeSchemaMustBeNonEmpty(t *testing.T) {
	env := Envelope{
		ID: "01J8Z3K4N5P6Q7R8S9T0V1W2Y7", Schema: "", TS: 1752537600000,
		ScopeNode: "01J8Z5A0B1C2D3E4F5G6H7Z5A0", Origin: "internal",
	}
	if d := detailOf(t, Validate(env)); !strings.Contains(d, "non-empty") {
		t.Errorf("an empty schema was rejected with %q, want the non-empty rule — the later class lookup refuses "+
			"it too, for a reason that sends a producer to the wrong fix", d)
	}
}

// TestAutomationRunModeDispositionIsRequired pins the presence half of
// mode_disposition, which is a different rule from its type half: with the
// presence check gone, the absent field falls through to the string check and is
// refused as "must be a string", which is not what happened.
func TestAutomationRunModeDispositionIsRequired(t *testing.T) {
	base := map[string]any{
		"rule_id": "01J8Z3K4N5P6Q7R8S9T0V1W2YC", "rule_revision": 4,
		"trigger_snapshot":  map[string]any{"kind": "state"},
		"condition_results": []any{map[string]any{"passed": true}},
		"action_outcomes":   []any{map[string]any{"status": "ok"}},
		"misfire_caught":    false,
	}
	raw, err := json.Marshal(base) // mode_disposition deliberately absent
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if d := detailOf(t, validateAutomationRun(raw)); !strings.Contains(d, "required") {
		t.Errorf("an absent mode_disposition was rejected with %q, want the required rule", d)
	}

	// The control: present and valid still passes, so the rule above is not a
	// blanket refusal of the field.
	base["mode_disposition"] = "ran"
	raw, _ = json.Marshal(base)
	if err := validateAutomationRun(raw); err != nil {
		t.Errorf("a conformant automation.run payload was refused: %v", err)
	}
}

// TestEntityStateAttributesDeltaMustBeAnObject pins EVT-030's TYPE rule for
// attributes_delta, distinct from the two presence rules beside it (must be
// present when attribute_change is true, must be omitted when it is false).
// Those two were held; the type check was not.
func TestEntityStateAttributesDeltaMustBeAnObject(t *testing.T) {
	payload := func(delta any) json.RawMessage {
		m := map[string]any{
			"entity_id": "01J8Z3K4N5P6Q7R8S9T0V1W2YD", "device_id": "01J8Z3K4N5P6Q7R8S9T0V1W2YE",
			"old_state": "off", "new_state": "on", "attribute_change": true,
			"attributes_delta": delta,
		}
		raw, err := json.Marshal(m)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		return raw
	}

	for _, notAnObject := range []any{[]any{}, "x", 3, nil} {
		if err := validateEntityStateChanged(payload(notAnObject)); err == nil {
			t.Errorf("attributes_delta = %#v was accepted; EVT-030 requires an object", notAnObject)
		}
	}
	// The control: a real object passes, so the rule is a type check rather than
	// a refusal of the field.
	if err := validateEntityStateChanged(payload(map[string]any{"brightness": 40})); err != nil {
		t.Errorf("a conformant attributes_delta object was refused: %v", err)
	}
}

// TestResolveVisibleRejectsAMalformedResumeFrom pins EVT-131/134's syntactic
// gate: a resume_from that is not a well-formed ULID is refused BEFORE any
// delivery decision, rather than being looked up and missed.
//
// The distinction matters because both paths refuse. A malformed id that
// reached the lookup would be refused as "not in the visible set", which is the
// answer for an id that EXISTS somewhere — and EVT-134a is explicit that those
// two must not be distinguishable, so the syntactic gate is what keeps the
// membership answer from being asked at all.
func TestResolveVisibleRejectsAMalformedResumeFrom(t *testing.T) {
	var l EventLog
	l.Append(scopedEnv("01J8Z3K4N5P6Q7R8S9T0V1W2Y1", mine))

	for _, bad := range []string{
		"not a ulid",                  // fails the grammar (spaces)
		"01J8Z3K4N5P6Q7R8S9T0V1W2",    // grammar-clean but too short for a ULID
		"01J8Z3K4N5P6Q7R8S9T0V1W2Y7X", // too long
		"!!!!",                        // neither
	} {
		out, rerr := ResolveVisible(&l, bad, visibleToMe)
		if rerr == nil {
			t.Errorf("resume_from %q was accepted; EVT-131 requires a syntactic refusal", bad)
			continue
		}
		if rerr.Code != ResumeFromInvalidCode {
			t.Errorf("resume_from %q refused with %q, want %q", bad, rerr.Code, ResumeFromInvalidCode)
		}
		if out.Result != "" {
			t.Errorf("a syntactically refused resume carried an outcome %+v — a refusal must not also answer", out)
		}
	}

	// The control: a well-formed id that IS in the visible set resolves, so the
	// gate above rejects on shape rather than on everything.
	if _, rerr := ResolveVisible(&l, "01J8Z3K4N5P6Q7R8S9T0V1W2Y1", visibleToMe); rerr != nil {
		t.Errorf("a well-formed, visible resume point was refused: %+v", rerr)
	}
}
