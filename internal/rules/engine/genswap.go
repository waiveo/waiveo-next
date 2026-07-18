package engine

import (
	"bytes"
	"encoding/json"

	"github.com/maaxton/waiveo-next/internal/rules/closure"
	"github.com/maaxton/waiveo-next/internal/rules/model"
)

// Generation-swap changed/unchanged test (RUL-380/381).
//
// When a new generation is applied, each rule present in both the applied and
// the new generation must be classified changed or unchanged: a changed rule's
// in-flight run is canceled (RUL-380), an unchanged rule's continues untouched.
// "Changed" (RUL-381) means the rule's own compiled trigger/condition/action/
// mode structure differs from the prior generation, OR any value closed over
// into it differs. A rule whose compiled structure and every closed-over value
// are identical is NOT changed merely because the generation number advanced.
//
// The classification is a byte comparison of a canonical fingerprint
// (ruleSignature) of exactly those inputs: the rule's mode + its trigger,
// condition, and action members in canonical JSON, plus its frozen closure. The
// rule ID is deliberately excluded — it is the identity the two generations'
// rules are matched by, not part of the changed-ness test. An action's `params`
// Expression is part of the frozen structure's raw JSON here but is not itself a
// closed-over value (RUL-393); it rides the structural bytes like any other
// authored field, so a params edit is a structural change as it should be.

// ruleChanged reports whether a rule changed across a generation swap (RUL-381):
// its canonical compiled structure or any closed-over value differs. A bare
// generation-number bump with identical structure and closure is not a change.
func ruleChanged(oldRule model.Rule, oldCl closure.Closure, newRule model.Rule, newCl closure.Closure) bool {
	return !bytes.Equal(ruleSignature(oldRule, oldCl), ruleSignature(newRule, newCl))
}

// ruleSignature returns a stable canonical fingerprint of a rule's compiled
// structure (mode + triggers + conditions + actions) together with its frozen
// closure (RUL-381). Two rules with byte-different but semantically identical
// authored JSON, or with the same structure under an advanced generation
// number, share a signature; any change to the compiled structure or to a
// closed-over value (a frozen variable value, a selector's matched set, a preset
// batch's command list) yields a different one.
func ruleSignature(rule model.Rule, cl closure.Closure) []byte {
	sig := struct {
		Mode       string            `json:"mode"`
		Triggers   []json.RawMessage `json:"triggers"`
		Conditions []json.RawMessage `json:"conditions"`
		Actions    []json.RawMessage `json:"actions"`
		Closure    closure.Closure   `json:"closure"`
	}{
		Mode:       rule.Mode,
		Triggers:   canonMembers(rule.Triggers),
		Conditions: canonMembers(rule.Conditions),
		Actions:    canonMembers(rule.Actions),
		Closure:    cl,
	}
	b, err := json.Marshal(sig)
	if err != nil {
		// Unreachable: every field is a string or already-decoded JSON value, so
		// re-encoding cannot fail. Guarded for completeness only.
		return nil
	}
	return b
}

// canonMembers renders a member slice to its canonical raw-JSON forms so that a
// byte-different but semantically identical authored element compares equal.
func canonMembers(ms []model.Member) []json.RawMessage {
	out := make([]json.RawMessage, 0, len(ms))
	for _, m := range ms {
		out = append(out, canonRaw(m.Raw))
	}
	return out
}

// canonRaw normalizes one element's raw JSON to a canonical form — object keys
// sorted, insignificant whitespace removed, numbers re-rendered — by round-
// tripping it through the generic decoder (RUL-381). A raw value that does not
// parse (or is empty) is returned unchanged so it still compares byte-for-byte.
func canonRaw(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return raw
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return raw
	}
	b, err := json.Marshal(v)
	if err != nil {
		return raw
	}
	return b
}
