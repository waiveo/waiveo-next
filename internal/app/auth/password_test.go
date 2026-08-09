package auth

import (
	"errors"
	"strings"
	"testing"
)

// The password hash decoder is the boundary between a stored credential row and
// the verifier. decodeHash/parseParams refuse a PHC string this build cannot
// faithfully reproduce so that a corrupt or tampered row fails LOUDLY — a
// distinct error a caller can tell apart from a wrong password — rather than
// deriving a key under wrong parameters and reporting a mismatch, or worse
// absorbing a malformed field and verifying against garbage.
//
// A mutation sweep of internal/app/auth found every one of these negative
// branches unheld tree-wide: the package had no password_test.go at all, so the
// decoder's happy path (a hash this package itself wrote) was the only input any
// test ever fed it. That is the store.go:104 shape — a guard that cannot fire
// from the trusted producer today, over a value whose type still permits the
// fault, standing on the integrity of a persisted row. This file drives the
// decoder with what a corrupted row permits and pins each specific refusal.

// TestVerifyPasswordRoundTripsAndRejectsAWrongPassword is the control: the
// decoder's happy path, so the negative cases below are about malformed hashes
// and not about the KDF being broken generally.
func TestVerifyPasswordRoundTripsAndRejectsAWrongPassword(t *testing.T) {
	enc, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if err := VerifyPassword(enc, "correct horse battery staple"); err != nil {
		t.Fatalf("verify of the right password failed: %v", err)
	}
	if err := VerifyPassword(enc, "Correct Horse Battery Staple"); !errors.Is(err, ErrPasswordMismatch) {
		t.Fatalf("wrong password gave %v, want ErrPasswordMismatch", err)
	}
}

// TestDecodeHashRefusesAMalformedStructure pins the field-shape and header
// refusals. Each input is a real PHC string with exactly one thing wrong, so a
// refusal can only come from the branch that thing violates. Every case must
// fail with a NON-mismatch error (a corrupt row is distinct from a wrong
// password), AND carry the distinctive message of its OWN guard.
//
// The message assertion is load-bearing, not decoration. The decoder is a
// cascade: an empty-salt check backstops the salt base64 decode, a
// version-range check backstops the "v=<n>" scan, and so on. Asserting only
// "some error" lets the backstop stand in for the specific guard, and the
// intermediate mutant survives — which is exactly what the sweep observed. Here
// the specific guard gives the MORE informative refusal (the #179 rule cuts
// this way round: pin the guard when it, not the downstream, gives the better
// answer), so each case pins the substring that only its own guard emits.
func TestDecodeHashRefusesAMalformedStructure(t *testing.T) {
	good, err := HashPassword("pw")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	parts := strings.Split(good, "$") // ["", "argon2id", "v=19", "m=...,t=,p=", salt, key]
	if len(parts) != 6 {
		t.Fatalf("HashPassword produced %d PHC fields, not 6: %q", len(parts), good)
	}
	with := func(mut func([]string) []string) string {
		c := append([]string(nil), parts...)
		return strings.Join(mut(c), "$")
	}

	for _, tc := range []struct {
		name, hash, wantMsg string
	}{
		{"too few fields", strings.Join(parts[:5], "$"), "malformed password hash"},
		{"no leading empty field", strings.TrimPrefix(good, "$"), "malformed password hash"},
		{"wrong algorithm", with(func(c []string) []string { c[1] = "argon2i"; return c }), "is not argon2id"},
		{"version not v=<n>", with(func(c []string) []string { c[2] = "version-nineteen"; return c }), "malformed password hash version"},
		{"unsupported version", with(func(c []string) []string { c[2] = "v=16"; return c }), "is not 19"},
		{"salt not base64", with(func(c []string) []string { c[4] = "!!!not-base64!!!"; return c }), "decode password salt"},
		{"key not base64", with(func(c []string) []string { c[5] = "!!!not-base64!!!"; return c }), "decode password hash"},
		{"empty salt", with(func(c []string) []string { c[4] = ""; return c }), "empty salt or key"},
		{"empty key", with(func(c []string) []string { c[5] = ""; return c }), "empty salt or key"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := VerifyPassword(tc.hash, "pw")
			if err == nil {
				t.Fatalf("a malformed hash (%s) verified successfully", tc.name)
			}
			if errors.Is(err, ErrPasswordMismatch) {
				t.Fatalf("a malformed hash (%s) reported ErrPasswordMismatch — a corrupt row must be "+
					"distinguishable from a wrong password", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Fatalf("%s refused with %q, want the guard's own message containing %q — a generic "+
					"backstop error means the specific guard was bypassed", tc.name, err, tc.wantMsg)
			}
		})
	}
}

// TestDecodeHashRefusesMalformedParameters pins parseParams: the m/t/p field is
// hand-parsed precisely so a field in the wrong order, a missing key=value, a
// non-numeric value, an unknown key, or an out-of-range value is refused rather
// than partially absorbed. Each maps to one survivor the sweep flagged.
func TestDecodeHashRefusesMalformedParameters(t *testing.T) {
	good, err := HashPassword("pw")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	parts := strings.Split(good, "$")
	withParams := func(field string) string {
		c := append([]string(nil), parts...)
		c[3] = field
		return strings.Join(c, "$")
	}

	for _, tc := range []struct {
		name, params, wantMsg string
	}{
		{"too few params", "m=65536,t=3", "malformed password hash parameters"},
		{"too many params", "m=65536,t=3,p=4,x=1", "malformed password hash parameters"},
		{"pair without =", "m=65536,t=3,p", `malformed password hash parameter "p"`},
		{"non-numeric value", "m=lots,t=3,p=4", `password hash parameter "m"`},
		{"unknown key", "m=65536,t=3,q=4", `unknown password hash parameter "q"`},
		{"zero memory", "m=0,t=3,p=4", "out-of-range password hash parameters"},
		{"zero time", "m=65536,t=0,p=4", "out-of-range password hash parameters"},
		{"zero threads", "m=65536,t=3,p=0", "out-of-range password hash parameters"},
		{"threads over 255", "m=65536,t=3,p=256", "out-of-range password hash parameters"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := VerifyPassword(withParams(tc.params), "pw")
			if err == nil {
				t.Fatalf("a hash with malformed params (%s) verified successfully", tc.name)
			}
			if errors.Is(err, ErrPasswordMismatch) {
				t.Fatalf("malformed params (%s) reported ErrPasswordMismatch, want a decode error", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Fatalf("%s refused with %q, want %q — a generic backstop error means the specific "+
					"parseParams guard was bypassed", tc.name, err, tc.wantMsg)
			}
		})
	}

	// The control: the real parameter field still decodes and verifies, so the
	// refusals above are about the malformations and not about parseParams
	// rejecting everything.
	if err := VerifyPassword(good, "pw"); err != nil {
		t.Fatalf("the unmodified parameter field failed to verify: %v", err)
	}
}
