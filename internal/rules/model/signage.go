package model

import (
	"encoding/json"
	"fmt"
)

// signage.go decodes a signage action's OWN members — the ones the shared
// Member decode does not carry because they belong to three specific action
// types rather than to every leaf (contracts/rules-1.md RUL-233/234/235:
// `screen_id`, `selector`, `cast_id`, `message`, `ttl_seconds`).
//
// It lives here, beside the Member decoder, rather than as a struct literal
// inside each caller, because there are exactly two callers and they must agree:
// the compile gate (compile.validateScreenRef / validateSignageContent), which
// decides whether an author may store the action, and the executor
// (eval.runSignage), which decides what it does. Two hand-rolled probe structs
// is how those two came to disagree — and the way they disagreed is the reason
// this file exists.
//
// # Why the type is checked rather than assumed
//
// Both callers used `json.Unmarshal` into a struct of Go strings and treated a
// decode failure as "not this check's concern". A WRONG-TYPED member — `cast_id`
// as a number, `message` as an object — fails that decode, so it fell through
// BOTH: accepted 201 at authoring, and then executed as a silent no-op that
// reported `{"disposition":"ran","signage":[]}` against an unchanged screen.
// That is the same defect the missing-member check closed, entered through a
// different door: encoding/json's own type error was being read as "nothing to
// see here" by a probe whose entire job was to see it.
//
// So the decode is done member by member off the raw object, and a member
// written at the wrong type is REPORTED, naming itself — never skipped, and
// never silently coerced into the zero value, which would make
// `{"cast_id": 5, "message": "hi"}` compile as a legal message-only alert with
// the author's own cast_id thrown away.

// SignageMembers is one signage action's decoded members. Absent members are the
// zero value, which is what both callers' own required-member rules are stated
// against (RUL-234's "MUST declare cast_id", RUL-235's "exactly one of").
type SignageMembers struct {
	ScreenID   string
	Selector   string
	CastID     string
	Message    string
	TTLSeconds int
}

// MemberTypeError names one member whose JSON value is not of the type rules/1
// declares for it. It carries the member's own name so a caller can address the
// refusal at `actions[2].cast_id` rather than at the action — a refusal that
// does not name the member makes the author hunt for it.
type MemberTypeError struct {
	Member string // the member's own name, e.g. "cast_id"
	Want   string // the declared type, e.g. "a string"
	Got    string // what was written instead, e.g. "a number"
}

func (e *MemberTypeError) Error() string {
	return fmt.Sprintf("%s must be %s, not %s", e.Member, e.Want, e.Got)
}

// DecodeSignage reads a signage action's members off raw.
//
// A member that is absent (or an explicit `null`, which rules/1 does not
// distinguish from absence for any of these) is left at its zero value. A member
// that is PRESENT at the wrong JSON type is returned as a *MemberTypeError and
// no members are reported: an action one of whose members cannot be read is not
// half-readable, and continuing with the rest is how a wrong-typed `cast_id`
// turns into a legal-looking `message`-only alert.
//
// A raw that is not a JSON object cannot occur through decodeMember (which
// requires one to produce a Member at all) and is reported as an empty member
// set rather than an error, leaving the caller's own required-member rules to
// refuse it.
func DecodeSignage(raw json.RawMessage) (SignageMembers, *MemberTypeError) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return SignageMembers{}, nil
	}
	var out SignageMembers
	for _, f := range []struct {
		name string
		into *string
	}{
		{"screen_id", &out.ScreenID},
		{"selector", &out.Selector},
		{"cast_id", &out.CastID},
		{"message", &out.Message},
	} {
		v, terr := stringMember(obj, f.name)
		if terr != nil {
			return SignageMembers{}, terr
		}
		*f.into = v
	}
	ttl, terr := intMember(obj, "ttl_seconds")
	if terr != nil {
		return SignageMembers{}, terr
	}
	out.TTLSeconds = ttl
	return out, nil
}

// stringMember reads one string-typed member, reporting a *MemberTypeError when
// it is present at any other JSON type.
func stringMember(obj map[string]json.RawMessage, name string) (string, *MemberTypeError) {
	raw, ok := obj[name]
	if !ok || jsonKind(raw) == "null" {
		return "", nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", &MemberTypeError{Member: name, Want: "a string", Got: jsonKind(raw)}
	}
	return s, nil
}

// intMember reads one integer-typed member. A fractional number is refused for
// the same reason a string is: `ttl_seconds` is a count of seconds (RUL-235), and
// silently truncating 1.5 would make the alert lapse at an instant the author
// never wrote.
func intMember(obj map[string]json.RawMessage, name string) (int, *MemberTypeError) {
	raw, ok := obj[name]
	if !ok || jsonKind(raw) == "null" {
		return 0, nil
	}
	var n int
	if err := json.Unmarshal(raw, &n); err != nil {
		return 0, &MemberTypeError{Member: name, Want: "a whole number of seconds", Got: jsonKind(raw)}
	}
	return n, nil
}

// jsonKind names the JSON type of raw for a refusal message, in the words the
// contract uses ("a string", "a number") rather than in Go's.
func jsonKind(raw json.RawMessage) string {
	for _, b := range raw {
		switch {
		case b == ' ' || b == '\t' || b == '\n' || b == '\r':
			continue
		case b == '"':
			return "a string"
		case b == '{':
			return "an object"
		case b == '[':
			return "an array"
		case b == 't' || b == 'f':
			return "a boolean"
		case b == 'n':
			return "null"
		default:
			return "a number"
		}
	}
	return "nothing"
}
