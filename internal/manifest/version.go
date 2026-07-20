package manifest

import (
	"fmt"
	"strconv"
	"strings"
)

// Comparator is one clause of a version range (MAN-013): an operator applied to
// a major.minor value. Op is one of ">=", ">", "<=", "<", "=".
type Comparator struct {
	Op    string
	Major int
	Minor int
}

// VersionRange is a parsed MAN-013 version range: a non-empty conjunction (AND)
// of comparators evaluated against a contract's major.minor value.
type VersionRange struct {
	Comparators []Comparator
}

// ParseVersionRange parses a MAN-013 version-range string:
//
//	range      := comparator (" " comparator)*
//	comparator := (">=" | ">" | "<=" | "<" | "=") major "." minor
//
// Comparators are space-separated and AND-conjoined. A bare major.minor with no
// leading operator is shorthand for "=major.minor". A string with no comparator,
// an unrecognized operator, a component that is not major.minor, or a
// non-numeric component is rejected.
func ParseVersionRange(s string) (VersionRange, error) {
	tokens := strings.Fields(s)
	if len(tokens) == 0 {
		return VersionRange{}, fmt.Errorf("version range %q is empty", s)
	}
	vr := VersionRange{Comparators: make([]Comparator, 0, len(tokens))}
	for _, tok := range tokens {
		c, err := parseComparator(tok)
		if err != nil {
			return VersionRange{}, err
		}
		vr.Comparators = append(vr.Comparators, c)
	}
	return vr, nil
}

// parseComparator parses one comparator token, defaulting a bare major.minor to
// the "=" operator.
func parseComparator(tok string) (Comparator, error) {
	op := "="
	rest := tok
	switch {
	case strings.HasPrefix(tok, ">="):
		op, rest = ">=", tok[2:]
	case strings.HasPrefix(tok, "<="):
		op, rest = "<=", tok[2:]
	case strings.HasPrefix(tok, ">"):
		op, rest = ">", tok[1:]
	case strings.HasPrefix(tok, "<"):
		op, rest = "<", tok[1:]
	case strings.HasPrefix(tok, "="):
		op, rest = "=", tok[1:]
	}
	major, minor, err := parseMajorMinor(rest)
	if err != nil {
		return Comparator{}, fmt.Errorf("version range comparator %q: %w", tok, err)
	}
	return Comparator{Op: op, Major: major, Minor: minor}, nil
}

// parseMajorMinor splits a major.minor string into its two digits-only integer
// components, rejecting anything else.
func parseMajorMinor(s string) (major, minor int, err error) {
	majStr, minStr, ok := strings.Cut(s, ".")
	if !ok {
		return 0, 0, fmt.Errorf("%q is not a major.minor value", s)
	}
	if strings.Contains(minStr, ".") {
		return 0, 0, fmt.Errorf("%q has more than two components", s)
	}
	if !isDigits(majStr) || !isDigits(minStr) {
		return 0, 0, fmt.Errorf("%q has a non-numeric component", s)
	}
	major, err = strconv.Atoi(majStr)
	if err != nil {
		return 0, 0, fmt.Errorf("%q major component: %w", s, err)
	}
	minor, err = strconv.Atoi(minStr)
	if err != nil {
		return 0, 0, fmt.Errorf("%q minor component: %w", s, err)
	}
	return major, minor, nil
}

// isDigits reports whether s is a non-empty run of ASCII digits.
func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// Allows reports whether the given contract major.minor satisfies every
// comparator in the range (a conjunction). A range with no comparators is never
// produced by ParseVersionRange; a zero-value VersionRange allows nothing.
func (vr VersionRange) Allows(major, minor int) bool {
	if len(vr.Comparators) == 0 {
		return false
	}
	for _, c := range vr.Comparators {
		if !c.allows(major, minor) {
			return false
		}
	}
	return true
}

// allows reports whether one comparator admits the given major.minor.
func (c Comparator) allows(major, minor int) bool {
	cmp := cmpPair(major, minor, c.Major, c.Minor)
	switch c.Op {
	case ">=":
		return cmp >= 0
	case ">":
		return cmp > 0
	case "<=":
		return cmp <= 0
	case "<":
		return cmp < 0
	case "=":
		return cmp == 0
	default:
		return false
	}
}

// cmpPair orders two major.minor pairs: -1 if a < b, +1 if a > b, 0 if equal.
func cmpPair(aMaj, aMin, bMaj, bMin int) int {
	if aMaj != bMaj {
		if aMaj < bMaj {
			return -1
		}
		return 1
	}
	if aMin != bMin {
		if aMin < bMin {
			return -1
		}
		return 1
	}
	return 0
}
