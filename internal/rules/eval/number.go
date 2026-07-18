// Package eval holds the rules/1 evaluation-core primitives: the comparison
// and matching functions every trigger, condition, and action evaluation
// path shares, so none of them can reimplement (and silently diverge on)
// number parsing, typed comparison, or state matching (RUL-270, RUL-260,
// RUL-320).
package eval

import (
	"regexp"
	"strconv"
)

// numberGrammar is RUL-270 case 2's exact accepted string grammar:
// -?[0-9]+(\.[0-9]+)? — an optional leading minus, one or more digits, an
// optional decimal point followed by one or more digits. No leading `+`, no
// exponent notation, no leading/trailing whitespace, no thousands
// separators, no hex/octal/binary prefixes, and no Infinity/NaN literals —
// all of those fail this pattern and so fall through to not-a-number.
var numberGrammar = regexp.MustCompile(`^-?[0-9]+(\.[0-9]+)?$`)

// ParseNumber implements rules/1's single number-parsing rule (RUL-270),
// used by every numeric comparison in this package: (1) a JSON-decoded
// number — float64 (the shape encoding/json produces), or any other Go
// numeric-literal type a caller may pass directly — is used as-is; (2) a
// string is accepted only if it matches numberGrammar exactly; (3) every
// other input (bool, nil, a slice, a map, or a non-conforming string) is
// not-a-number: ParseNumber returns (0, false).
//
// Callers MUST treat a false ok as fail-closed per RUL-271: a numeric
// trigger/condition treats it as not satisfying its bounds, never as an
// evaluation error and never by coercing.
//
// ParseNumber deliberately does not call strconv.ParseFloat directly on an
// unvalidated string: ParseFloat alone accepts exponent notation, a leading
// `+`, and `Inf`/`NaN` literals, all of which RUL-270 explicitly excludes.
func ParseNumber(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int8:
		return float64(n), true
	case int16:
		return float64(n), true
	case int32:
		return float64(n), true
	case int64:
		return float64(n), true
	case uint:
		return float64(n), true
	case uint8:
		return float64(n), true
	case uint16:
		return float64(n), true
	case uint32:
		return float64(n), true
	case uint64:
		return float64(n), true
	case string:
		if !numberGrammar.MatchString(n) {
			return 0, false
		}
		f, err := strconv.ParseFloat(n, 64)
		if err != nil {
			// The grammar match above already guarantees a parseable plain
			// decimal, so this should be unreachable; fail closed anyway
			// rather than panicking or propagating an error.
			return 0, false
		}
		return f, true
	default:
		return 0, false
	}
}
