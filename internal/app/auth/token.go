package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

// Token kinds. A session token is the browser cookie value; an API-key token is
// the `Authorization: Bearer` value. Both are minted, hashed, revoked, and
// looked up through the SAME session relation (SEC-020: "An API-key credential
// MUST be revocable through the same mechanism"), so the kind is a column on
// that one relation rather than a second table.
const (
	TokenKindSession = "session"
	TokenKindAPIKey  = "api-key"
)

// Token prefixes. `wv_` is the API-key form `api/openapi.yaml`'s ApiKey scheme
// already publishes as `wv_<random>`; `wvs_` distinguishes a session cookie
// value at a glance in a log or a leak report, so the two are never confused for
// one another when triaging.
const (
	sessionTokenPrefix = "wvs"
	apiKeyTokenPrefix  = "wv"
)

// tokenSecretBytes is the length of a token's random segment: 32 bytes = 256
// bits of crypto/rand entropy, comfortably above SEC-032's 128-bit floor (which
// governs grant codes) and above any brute-force reach for a bearer secret.
const tokenSecretBytes = 32

// aalSegment renders an AAL as the fixed, three-character segment a token
// carries it under. The segment IS the token's own format-level AAL claim
// (SEC-021): a resource server reads the assurance level from the presented
// token itself, without first consulting a database row.
//
// The claim is not merely readable but UNFORGEABLE, and by construction rather
// than by a signature: the stored token hash covers the WHOLE token string,
// segment included, so flipping `std` to `rec` changes the hash and the
// tampered token simply resolves to no session at all. That is why this
// package can satisfy SEC-021 with an opaque random token instead of a signed
// assertion.
func aalSegment(a AAL) (string, error) {
	switch a {
	case AALStandard:
		return "std", nil
	case AALRecovery:
		return "rec", nil
	default:
		return "", fmt.Errorf("auth: unknown aal %q", a)
	}
}

// aalFromSegment is aalSegment's inverse.
func aalFromSegment(seg string) (AAL, bool) {
	switch seg {
	case "std":
		return AALStandard, true
	case "rec":
		return AALRecovery, true
	default:
		return "", false
	}
}

// MintToken returns a fresh opaque bearer token of the given kind carrying aal
// in its own format:
//
//	wvs_std_<64 hex chars>   a standard-AAL session cookie value
//	wv_rec_<64 hex chars>    a recovery-AAL API key
//
// The returned string is the ONLY time the raw value exists on this side of the
// wire — the store persists HashToken(token), never this.
func MintToken(kind string, aal AAL) (string, error) {
	prefix, err := tokenPrefix(kind)
	if err != nil {
		return "", err
	}
	seg, err := aalSegment(aal)
	if err != nil {
		return "", err
	}
	secret := make([]byte, tokenSecretBytes)
	if _, err := rand.Read(secret); err != nil {
		return "", fmt.Errorf("auth: mint token: %w", err)
	}
	return prefix + "_" + seg + "_" + hex.EncodeToString(secret), nil
}

// ParseToken reads a token's self-carried kind and AAL claim (SEC-021) without
// any database access, reporting ok=false for anything that is not one of this
// package's two token shapes. A true return says only that the token is
// WELL-FORMED; whether it names a live, unrevoked session is a store lookup on
// HashToken(token).
func ParseToken(token string) (kind string, aal AAL, ok bool) {
	parts := strings.Split(token, "_")
	if len(parts) != 3 {
		return "", "", false
	}
	switch parts[0] {
	case sessionTokenPrefix:
		kind = TokenKindSession
	case apiKeyTokenPrefix:
		kind = TokenKindAPIKey
	default:
		return "", "", false
	}
	aal, ok = aalFromSegment(parts[1])
	if !ok {
		return "", "", false
	}
	// The secret segment must be the exact hex width MintToken produces; a
	// truncated or padded value is refused here rather than reaching a lookup.
	if len(parts[2]) != hex.EncodedLen(tokenSecretBytes) {
		return "", "", false
	}
	if _, err := hex.DecodeString(parts[2]); err != nil {
		return "", "", false
	}
	return kind, aal, true
}

// HashToken returns the at-rest form of a bearer token: lowercase hex SHA-256
// over the whole token string. A token is a high-entropy random value, so a
// plain cryptographic hash (not a password KDF) is the right primitive — there
// is no low-entropy guess space for an attacker holding the database to search,
// and a login-rate KDF on every single request would be a self-inflicted denial
// of service.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func tokenPrefix(kind string) (string, error) {
	switch kind {
	case TokenKindSession:
		return sessionTokenPrefix, nil
	case TokenKindAPIKey:
		return apiKeyTokenPrefix, nil
	default:
		return "", fmt.Errorf("auth: unknown token kind %q", kind)
	}
}

// MintGrantCode returns a fresh grant code: 32 crypto/rand bytes rendered as 64
// hex characters. That is 256 bits, above SEC-032's "at least 128 bits of
// entropy" floor. Like a token, the raw code is returned exactly once and the
// store persists only HashToken(code).
func MintGrantCode() (string, error) {
	b := make([]byte, tokenSecretBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("auth: mint grant code: %w", err)
	}
	return hex.EncodeToString(b), nil
}
