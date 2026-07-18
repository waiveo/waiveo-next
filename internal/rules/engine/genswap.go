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

// triggerSig is one trigger's per-(trigger,entity) fingerprint (RUL-303/304):
// the canonical JSON of its authored member joined with its resolved subject
// entity. It is the trigger-level analogue of ruleSignature — the identical
// changed/unchanged test RUL-381 defines at the rule level, applied here at an
// individual trigger's granularity. A trigger whose authored member and subject
// entity both match a prior generation's trigger shares its signature and so
// carries its first-observation baseline forward across a generation swap; any
// change to the member (or a selector-membership change that fans it out to a
// different entity) yields a different signature and resets that baseline. The
// NUL separator cannot appear in canonical JSON or a ULID entity id, so the two
// fields cannot collide across the join.
func triggerSig(raw json.RawMessage, entityID string) string {
	return string(canonRaw(raw)) + "\x00" + entityID
}

// carryUnchangedTriggers preserves first-observation state at trigger
// granularity across a generation swap (RUL-303/304). It is applied when a rule
// present in both generations is CHANGED as a whole (so its fresh instance was
// rebuilt and its in-flight run canceled, RUL-380/381) but some of its
// individual triggers are untouched by the edit: each fresh trigger whose
// per-(trigger,entity) signature matches a prior trigger reuses that prior
// runtime, carrying its durable baseline/hold/seen state forward exactly as if
// no generation had been applied — so a routine edit elsewhere in the same rule
// (or generation) neither resets its baseline nor suppresses a genuine
// subsequent transition. A fresh trigger with no signature match is new or
// changed and keeps its reset runtime. Each prior runtime is consumed on first
// match so two identical fresh triggers never alias one prior runtime's state.
func carryUnchangedTriggers(fresh, old *ruleInstance) {
	if len(old.triggers) == 0 || len(fresh.triggers) == 0 {
		return
	}
	bySig := make(map[string]*triggerRuntime, len(old.triggers))
	for _, tr := range old.triggers {
		if _, seen := bySig[tr.sig]; !seen {
			bySig[tr.sig] = tr
		}
	}
	for i, tr := range fresh.triggers {
		if prev, ok := bySig[tr.sig]; ok {
			fresh.triggers[i] = prev
			delete(bySig, tr.sig)
		}
	}
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
