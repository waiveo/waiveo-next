package auth

import (
	"strings"
	"testing"
)

// ParseToken reads a bearer token's self-carried kind and AAL claim (SEC-021)
// with no database access, and returns ok=false for anything that is not one of
// this package's two exact token shapes. It is the cheap gate in front of the
// session lookup: a value it passes is well-formed, so a value it should reject
// but doesn't becomes a wasted hash-and-lookup at best, and at worst a token
// whose format-level AAL claim was never really validated.
//
// A mutation sweep found every one of ParseToken's reject branches unheld: the
// package's only ParseToken tests assert the happy path on a freshly minted
// token. These pin the rejections, each input malformed in exactly one way so a
// false return can only come from the branch that way violates.

func TestParseTokenAcceptsBothMintedShapes(t *testing.T) {
	for _, tc := range []struct {
		kind string
		aal  AAL
	}{
		{TokenKindSession, AALStandard},
		{TokenKindSession, AALRecovery},
		{TokenKindAPIKey, AALStandard},
		{TokenKindAPIKey, AALRecovery},
	} {
		tok, err := MintToken(tc.kind, tc.aal)
		if err != nil {
			t.Fatalf("MintToken(%s,%v): %v", tc.kind, tc.aal, err)
		}
		kind, aal, ok := ParseToken(tok)
		if !ok || kind != tc.kind || aal != tc.aal {
			t.Fatalf("ParseToken(%q) = (%q,%v,%v), want (%q,%v,true)", tok, kind, aal, ok, tc.kind, tc.aal)
		}
	}
}

func TestParseTokenRejectsMalformedTokens(t *testing.T) {
	good, err := MintToken(TokenKindSession, AALStandard) // wvs_std_<64 hex>
	if err != nil {
		t.Fatalf("MintToken: %v", err)
	}
	parts := strings.Split(good, "_")
	secret := parts[2] // 64 hex chars

	for _, tc := range []struct {
		name, token string
	}{
		{"empty", ""},
		{"two segments", "wvs_std"},
		{"four segments", good + "_extra"},
		{"unknown prefix", "xx_std_" + secret},
		{"unknown aal segment", "wvs_zzz_" + secret},
		{"empty aal segment", "wvs__" + secret},
		// valid hex, but not the exact width MintToken produces:
		{"secret too short", "wvs_std_abcd"},
		{"secret too long", "wvs_std_" + secret + "00"},
		// exact width, but not hex at all:
		{"secret not hex", "wvs_std_" + strings.Repeat("z", len(secret))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if kind, aal, ok := ParseToken(tc.token); ok {
				t.Fatalf("ParseToken(%q) accepted a malformed token as (%q,%v,true)", tc.token, kind, aal)
			}
		})
	}
}
