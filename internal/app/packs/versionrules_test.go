package packs_test

import (
	"strconv"
	"strings"
	"testing"

	"github.com/maaxton/waiveo-next/internal/app/packs"
)

// versionrules_test.go pins the MAN-002 version grammar, which is what every
// ordering decision on this surface is built on: MKT-050's anti-rollback mark
// and MKT-093b's required-pack floor both compare versions, and a version that
// parses wrongly makes both of them decide wrongly.
//
// A mutation sweep found the grammar's own rules unheld.

// TestVersionGrammarRejectsSignedComponents is the case that is not obvious from
// reading the code.
//
// parseVersion checks every rune is a digit BEFORE handing the component to
// strconv.ParseInt — and that check is load-bearing, because ParseInt accepts a
// leading sign. Without it "1.0.+5" parses to the same triple as "1.0.5": two
// DIFFERENT version strings that compare EQUAL. An install record pins the
// string while the mark comparison uses the triple, so a registry could publish
// a distinct artifact at a version that is not "lower" than the mark and walk
// past MKT-050 without ever going backward.
//
// A negative component is the other half: "1.0.-1" would parse below every real
// version, which is a value no MAN-002 version can hold.
func TestVersionGrammarRejectsSignedComponents(t *testing.T) {
	// The fact the rule exists for, asserted directly so this test explains
	// itself if the standard library ever changes.
	if n, err := strconv.ParseInt("+5", 10, 64); err != nil || n != 5 {
		t.Fatalf(`ParseInt("+5") = (%d, %v); the digit rule below may no longer be load-bearing, but check before removing it`, n, err)
	}

	for _, bad := range []string{"1.0.+5", "1.+0.5", "+1.0.5", "1.0.-1", "1.-0.5", "-1.0.5"} {
		if packs.ValidVersion(bad) {
			t.Errorf("%q was accepted as a MAN-002 version — ParseInt takes the sign, so this is a second spelling "+
				"of an existing version rather than a new one", bad)
		}
	}

	// The control: the unsigned forms are accepted, so the rule rejects the sign
	// rather than the digits.
	for _, ok := range []string{"1.0.5", "0.0.0", "10.20.30"} {
		if !packs.ValidVersion(ok) {
			t.Errorf("%q was refused as a MAN-002 version", ok)
		}
	}
}

// TestVersionGrammarBoundsComponentWidth pins the digit cap, whose reason
// parseVersion states: a component that does not fit an int64 has no numeric
// position either, and silently wrapping one "would produce an ordering an
// attacker chooses".
func TestVersionGrammarBoundsComponentWidth(t *testing.T) {
	// One past the cap and STILL INSIDE int64 (1e18 < 9.22e18). The value
	// matters: 19 nines would overflow, and ParseInt refuses those on its own,
	// so a test using them passes with the cap deleted and proves nothing.
	// Measured — that is exactly what my first version of this did.
	wide := "1" + strings.Repeat("0", 18)
	if n, err := strconv.ParseInt(wide, 10, 64); err != nil || n <= 0 {
		t.Fatalf("the fixture %q must be a value ParseInt ACCEPTS, or this asserts the wrong rule: (%d, %v)", wide, n, err)
	}
	if packs.ValidVersion("1.0." + wide) {
		t.Errorf("a %d-digit component was accepted — the cap is what bounds a component to a width the whole "+
			"pipeline can carry, and ParseInt does not refuse this one", len(wide))
	}

	// The control: the widest component that DOES fit is accepted, so the cap is
	// a bound rather than a blanket refusal of large numbers.
	atCap := strings.Repeat("9", 18)
	if !packs.ValidVersion("1.0." + atCap) {
		t.Errorf("an %d-digit component was refused, but it is inside the cap", len(atCap))
	}
}

// TestVersionGrammarRejectsMalformedShapes covers the rest of the grammar: three
// components, none empty, nothing but digits.
func TestVersionGrammarRejectsMalformedShapes(t *testing.T) {
	for _, bad := range []string{
		"", "1", "1.0", "1.0.0.0", // wrong component count
		"1..0", "1.0.", ".0.0", // an empty component
		"1.0.0-rc1", "1.0.0+build", "v1.0.0", "1.0.x", "1 .0.0", // not digits
	} {
		if packs.ValidVersion(bad) {
			t.Errorf("%q was accepted as a MAN-002 version", bad)
		}
	}
}

// TestVersionComparisonFailsClosedOnAnUnparseableInput pins the second line
// VersionHigher's own doc describes: an unparseable input "ranks nowhere", so a
// bad candidate can never raise a high-water mark and a bad STORED mark is never
// quietly overwritten by whatever comes next.
//
// Both argument positions matter and fail for different reasons, so both are
// driven: the first protects the mark from a malformed candidate, the second
// protects a deployment whose stored mark is already corrupt from having it
// silently replaced.
func TestVersionComparisonFailsClosedOnAnUnparseableInput(t *testing.T) {
	if packs.VersionHigher("not-a-version", "1.0.0") {
		t.Error("an unparseable CANDIDATE ranked above a real version — it could raise the high-water mark")
	}
	if packs.VersionHigher("2.0.0", "not-a-version") {
		t.Error("a real version ranked above an unparseable STORED mark — a corrupt mark would be silently replaced")
	}
	if packs.VersionHigher("bad", "worse") {
		t.Error("two unparseable versions produced an ordering")
	}

	// The control: real versions still compare, in both directions.
	if !packs.VersionHigher("1.0.1", "1.0.0") {
		t.Error("1.0.1 did not rank above 1.0.0")
	}
	if packs.VersionHigher("1.0.0", "1.0.1") {
		t.Error("1.0.0 ranked above 1.0.1")
	}
	if packs.VersionHigher("1.0.0", "1.0.0") {
		t.Error("a version ranked strictly above itself")
	}
}
