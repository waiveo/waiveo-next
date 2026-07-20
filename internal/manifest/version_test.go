package manifest

import "testing"

// TestParseVersionRangeConjunction covers MAN-013: a space-separated list of
// comparators evaluated as a conjunction against a contract's major.minor.
func TestParseVersionRangeConjunction(t *testing.T) {
	vr, err := ParseVersionRange(">=1.0 <2.0")
	if err != nil {
		t.Fatalf("ParseVersionRange(%q) unexpected error: %v", ">=1.0 <2.0", err)
	}
	if !vr.Allows(1, 5) {
		t.Errorf("Allows(1,5) = false, want true (>=1.0 <2.0)")
	}
	if !vr.Allows(1, 0) {
		t.Errorf("Allows(1,0) = false, want true (>=1.0 boundary)")
	}
	if vr.Allows(2, 0) {
		t.Errorf("Allows(2,0) = true, want false (<2.0 boundary is exclusive)")
	}
	if vr.Allows(0, 9) {
		t.Errorf("Allows(0,9) = true, want false (below >=1.0)")
	}
}

// TestParseVersionRangeBare covers MAN-013's shorthand: a bare major.minor with
// no leading operator means =major.minor.
func TestParseVersionRangeBare(t *testing.T) {
	vr, err := ParseVersionRange("1.0")
	if err != nil {
		t.Fatalf("ParseVersionRange(%q) unexpected error: %v", "1.0", err)
	}
	if !vr.Allows(1, 0) {
		t.Errorf("Allows(1,0) = false, want true (bare 1.0 == =1.0)")
	}
	if vr.Allows(1, 1) {
		t.Errorf("Allows(1,1) = true, want false (bare 1.0 is exact)")
	}
	if vr.Allows(2, 0) {
		t.Errorf("Allows(2,0) = true, want false (bare 1.0 is exact)")
	}
}

// TestParseVersionRangeOperators exercises every comparator form.
func TestParseVersionRangeOperators(t *testing.T) {
	cases := []struct {
		rng   string
		major int
		minor int
		want  bool
	}{
		{">=1.2", 1, 2, true},
		{">=1.2", 1, 1, false},
		{">1.2", 1, 3, true},
		{">1.2", 1, 2, false},
		{"<=1.2", 1, 2, true},
		{"<=1.2", 1, 3, false},
		{"<1.2", 1, 1, true},
		{"<1.2", 1, 2, false},
		{"=1.2", 1, 2, true},
		{"=1.2", 2, 2, false},
	}
	for _, c := range cases {
		vr, err := ParseVersionRange(c.rng)
		if err != nil {
			t.Fatalf("ParseVersionRange(%q) unexpected error: %v", c.rng, err)
		}
		if got := vr.Allows(c.major, c.minor); got != c.want {
			t.Errorf("ParseVersionRange(%q).Allows(%d,%d) = %v, want %v", c.rng, c.major, c.minor, got, c.want)
		}
	}
}

// TestParseVersionRangeInvalid: malformed range strings MUST be rejected.
func TestParseVersionRangeInvalid(t *testing.T) {
	bad := []string{
		"",        // empty
		"   ",     // whitespace only
		"garbage", // not a comparator
		">=1",     // missing minor
		">=1.2.3", // three components is not major.minor
		">=1.x",   // non-numeric minor
		">=x.2",   // non-numeric major
		"~1.2",    // unsupported operator
		">= 1.2",  // operator split from its version by a space
	}
	for _, s := range bad {
		if _, err := ParseVersionRange(s); err == nil {
			t.Errorf("ParseVersionRange(%q) = nil error, want error", s)
		}
	}
}
