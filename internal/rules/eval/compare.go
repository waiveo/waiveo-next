package eval

import "github.com/maaxton/waiveo-next/internal/rules/registry"

// TypedEqual implements rules/1's typed value comparison (RUL-260–263) for
// an attribute-scoped equality check (RUL-022: an attribute's own value,
// compared using typed equality rather than state-string matching). t is the
// registry-declared type governing the comparison (registry.Registry's
// AttributeType); a and b are the two values being compared — typically an
// entity's current attribute value and a trigger/condition's declared bound.
//
// Comparison never coerces across Go types beyond what RUL-270's number
// parsing rule itself defines for the Number case: a String comparison
// requires both values already be strings (an int is never treated as its
// string form, RUL-260); a Boolean comparison requires both already be
// bools; a Number comparison parses both sides per ParseNumber (RUL-270
// governs "every numeric comparison", so a grammar-conforming numeric string
// legitimately compares equal to a JSON number under a Number-typed
// attribute) and fails closed (false) if either side is not-a-number
// (RUL-271). Unknown — no registry type declaration for the attribute —
// always evaluates false (RUL-262): an eval-time type mismatch is
// not-matching, never an error.
func TypedEqual(a, b any, t registry.ValueType) bool {
	switch t {
	case registry.String:
		as, aok := a.(string)
		bs, bok := b.(string)
		return aok && bok && as == bs // RUL-263: exact, case-sensitive
	case registry.Number:
		af, aok := ParseNumber(a)
		bf, bok := ParseNumber(b)
		return aok && bok && af == bf
	case registry.Boolean:
		ab, aok := a.(bool)
		bb, bok := b.(bool)
		return aok && bok && ab == bb
	default: // registry.Unknown
		return false
	}
}
