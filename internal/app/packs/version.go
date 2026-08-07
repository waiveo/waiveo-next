package packs

import (
	"strconv"
	"strings"
)

// versionOrder implements marketplace/1 MKT-050a: the order MKT-050's
// resolved-version high-water mark, MKT-090's auto-tracking, and MKT-093's
// last-known-good all compare pack versions under.
//
// The order is COMPONENT-WISE NUMERIC over manifest/1 MAN-002's grammar
// (MAJOR.MINOR.PATCH, digits only), never lexicographic. This is not a
// stylistic preference: "1.9.0" > "1.10.0" as strings, so a string comparison
// silently inverts the anti-rollback guarantee at exactly the versions a
// long-lived pack reaches — the implementation would perform the downgrade
// MKT-050 exists to prevent, with no attacker step involved at all.
//
// A version outside MAN-002's grammar has NO position in this order. It is not
// coerced, not zero-padded, and not compared under a fallback: parseVersion
// refuses it, and the caller turns that refusal into a resolution refusal
// (MKT-050a). Admitting it would mean comparing a high-water mark against
// something that has no defined relation to it.
//
// MAN-002 refuses a leading zero in a component (except `0` itself), which
// makes the grammar INJECTIVE onto ordered triples: exactly one spelling per
// version. Without that rule "1.0.05" and "1.0.5" are two distinct strings
// comparing EQUAL — a pointer moved between them is not a rollback under
// MKT-050 and not below a floor under MKT-093b, so the string that identifies
// an artifact and the triple that orders it disagree. The refusal here is the
// parser-level closure of that hole; do not relax it as cosmetic tolerance.

// maxVersionComponent bounds a single version component so parsing is total:
// a component that does not fit an int64 has no numeric position either, and
// silently wrapping one would produce an ordering an attacker chooses.
const maxVersionComponentDigits = 18

// parseVersion splits a MAN-002 version into its three numeric components, or
// reports that s is not a MAN-002 version at all.
func parseVersion(s string) ([3]int64, bool) {
	var out [3]int64
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return out, false
	}
	for i, p := range parts {
		if p == "" || len(p) > maxVersionComponentDigits {
			return out, false
		}
		// MAN-002's injectivity rule: no leading zero unless the component is
		// exactly "0" — one spelling per version, see the file header.
		if len(p) > 1 && p[0] == '0' {
			return out, false
		}
		for _, r := range p {
			if r < '0' || r > '9' {
				return out, false
			}
		}
		n, err := strconv.ParseInt(p, 10, 64)
		if err != nil {
			return out, false
		}
		out[i] = n
	}
	return out, true
}

// ValidVersion reports whether s is a MAN-002 pack version — the only versions
// this resolver will pin, record, or compare (MKT-050a).
func ValidVersion(s string) bool {
	_, ok := parseVersion(s)
	return ok
}

// VersionHigher reports whether candidate ranks strictly above current under
// MKT-050a's order. An UNPARSEABLE input ranks nowhere: candidate never wins,
// which fails closed — an unparseable candidate cannot raise a high-water mark,
// and an unparseable stored mark is never quietly overwritten by whatever comes
// next. Callers refuse an unparseable version outright before reaching here;
// this is the second line, not the check.
func VersionHigher(candidate, current string) bool {
	c, ok := parseVersion(candidate)
	if !ok {
		return false
	}
	cur, ok := parseVersion(current)
	if !ok {
		return false
	}
	for i := range c {
		if c[i] != cur[i] {
			return c[i] > cur[i]
		}
	}
	return false
}

// VersionLower is VersionHigher with the operands swapped — the direction
// MKT-050's refusal is stated in ("a pack version LOWER than that high-water
// mark"). Kept explicit rather than written as !VersionHigher at each call site:
// the negation would also swallow the equal case, which MKT-050 permits.
func VersionLower(candidate, current string) bool { return VersionHigher(current, candidate) }
