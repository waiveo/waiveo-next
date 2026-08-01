package auth

import (
	"crypto/subtle"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestCsrfRefusesAnEmptyStoredToken pins the guard that stops a session minted
// WITHOUT a CSRF token from passing the double-submit check by presenting an
// empty header.
//
// The guard reads like belt-and-braces in front of a constant-time compare, and
// it is not: crypto/subtle.ConstantTimeCompare of two EMPTY slices returns 1.
// Measured, because reasoning got it wrong first — "the compare would reject it
// anyway" is false, and deleting either early return leaves every test in the
// tree green while opening the check to exactly the caller its comment names.
//
// csrfOK's own doc states the property ("An empty stored token never matches, so
// a session minted without one (an API key) can never pass this check by
// presenting an empty header"). It was a comment and nothing else.
func TestCsrfRefusesAnEmptyStoredToken(t *testing.T) {
	// The fact that makes the guards load-bearing rather than decorative,
	// asserted here so this test explains itself if it ever fails.
	if subtle.ConstantTimeCompare([]byte(""), []byte("")) != 1 {
		t.Fatal("two empty slices no longer compare equal; the guards below may have become redundant, " +
			"but check before deleting them")
	}

	for _, tc := range []struct {
		name       string
		stored     string
		header     string
		wantAccept bool
	}{
		{"no stored token, no header — the API-key session", "", "", false},
		{"no stored token, a header supplied", "", "anything", false},
		{"a stored token, no header", "tok-abc", "", false},
		{"a stored token, the wrong header", "tok-abc", "tok-xyz", false},
		// The control. Without it a csrfOK that refused everything satisfies
		// every row above while breaking every browser session.
		{"a stored token and its match", "tok-abc", "tok-abc", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/", nil)
			if tc.header != "" {
				r.Header.Set(CSRFHeaderName, tc.header)
			}
			if got := csrfOK(r, tc.stored); got != tc.wantAccept {
				t.Errorf("csrfOK(header=%q, stored=%q) = %v, want %v", tc.header, tc.stored, got, tc.wantAccept)
			}
		})
	}
}
