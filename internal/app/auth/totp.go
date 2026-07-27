package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"net/url"
	"strings"
)

// TOTP (RFC 6238 over RFC 4226's HOTP) is security-model/1's guaranteed
// second-factor floor: SEC-004 requires it be "supported as the guaranteed
// second-factor floor, available WITHOUT a secure-context precondition", in
// explicit contrast to `passkey`, which SEC-102 admits only once self-hosted TLS
// is live and trusted by the requesting client. Nothing in this file, and
// nothing on the routes that call it, consults the TLS state of the request —
// that absence is the requirement, not an oversight: a box on the self-signed
// fallback (SEC-110) must still be able to enroll and verify a second factor.
//
// The parameters below are RFC 6238's defaults and the ones every authenticator
// app implements without configuration: HMAC-SHA1, 30-second steps, 6 digits.
// SHA1 is not a lapse — HOTP's security rests on the HMAC construction and the
// truncation, not on collision resistance of the underlying hash, and choosing
// SHA-256 here would buy nothing while making the credential unusable in most
// authenticator apps. They are constants rather than per-credential columns
// because a deployment that could vary them would be a deployment whose
// enrolled authenticators silently stop matching.
const (
	totpPeriodSeconds int64 = 30
	totpDigits              = 6

	// totpSecretBytes is the shared secret's length: 20 bytes = 160 bits, which
	// is RFC 4226 section 4 R6's recommendation and the width every
	// authenticator app's base32 import path expects.
	totpSecretBytes = 20
)

// totpSkewSteps is the acceptance tolerance in either direction: exactly ONE
// step, so a code is accepted within roughly [-30s, +60s) of its own window —
// RFC 6238 section 5.2's own guidance ("at most one time step").
//
// The number is argued from SEC-066–068 rather than picked from folklore. Those
// requirements have the app maintain a persisted monotonic clock floor and a
// `trusted`/`untrusted` accuracy assessment derived from it, and SEC-068 defines
// `untrusted` as the state "while it holds no independently verified time above
// the floor" — which is precisely this deployment's state, since no app-tier
// floor is implemented yet (audit.go's clockAssessment reports `untrusted` for
// exactly that reason). Under an untrusted clock, every additional step of
// tolerance is another window an adversary who can move the host clock, or who
// captured a code seconds ago, gets to land in. So the tolerance stays at the
// minimum that covers ordinary phone-vs-server drift and the seconds a human
// spends typing, and does not widen "to be safe" — widening is the opposite of
// safe here.
//
// The backward half of that window is additionally clamped by the per-credential
// step floor (Store.ConsumeTOTPStep): once a step has been consumed, no step at
// or below it is ever accepted again, whatever the host clock now says. That is
// SEC-066's "never moves backward" property realized at credential grain — a
// rolled-back host clock cannot re-open an already-consumed window — and it is
// the reason this tolerance is safe to state as a constant rather than as
// something that must shrink when the clock is distrusted.
const totpSkewSteps = 1

// totpBase32 is the encoding a TOTP secret is exchanged in: RFC 4648 base32,
// UPPERCASE, and UNPADDED. Padding is stripped because the `otpauth://` URI
// scheme every authenticator app reads carries the secret unpadded and several
// apps refuse a secret with '=' in it.
var totpBase32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// NewTOTPSecret returns a fresh 160-bit shared secret.
func NewTOTPSecret() ([]byte, error) {
	b := make([]byte, totpSecretBytes)
	if _, err := rand.Read(b); err != nil {
		return nil, fmt.Errorf("auth: generate totp secret: %w", err)
	}
	return b, nil
}

// EncodeTOTPSecret renders a secret in the base32 form an authenticator app
// imports (manually, or out of the QR code an `otpauth://` URI encodes).
func EncodeTOTPSecret(secret []byte) string {
	return totpBase32.EncodeToString(secret)
}

// DecodeTOTPSecret reverses EncodeTOTPSecret, tolerating the lowercase and
// space-separated forms a human retypes a key in.
func DecodeTOTPSecret(encoded string) ([]byte, error) {
	cleaned := strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(encoded), " ", ""))
	cleaned = strings.TrimRight(cleaned, "=")
	b, err := totpBase32.DecodeString(cleaned)
	if err != nil {
		// The offending value is NOT echoed: it is secret material, and an error
		// string is exactly the sort of place a secret ends up in a log.
		return nil, fmt.Errorf("auth: totp secret is not valid base32")
	}
	return b, nil
}

// TOTPStep returns the RFC 6238 time-step counter for a millisecond reading.
//
// It takes the reading rather than calling time.Now, like every other
// time-dependent path in this tree: the store's injected clock is the single
// source, so a test pins a step exactly instead of sleeping toward one, and the
// app-tier clock floor (SEC-066) has one place to be consulted from when it
// lands.
func TOTPStep(nowMs int64) int64 {
	return nowMs / 1000 / totpPeriodSeconds
}

// TOTPCode returns the 6-digit code for a secret at a given step — RFC 4226's
// HOTP with the dynamic truncation, keyed on the step counter RFC 6238
// substitutes for HOTP's event counter.
func TOTPCode(secret []byte, step int64) string {
	var counter [8]byte
	binary.BigEndian.PutUint64(counter[:], uint64(step))
	mac := hmac.New(sha1.New, secret)
	mac.Write(counter[:])
	sum := mac.Sum(nil)
	// RFC 4226 section 5.3 dynamic truncation: the low nibble of the last byte
	// selects a 4-byte window, whose high bit is masked off so the result is
	// always a positive 31-bit integer.
	offset := sum[len(sum)-1] & 0x0f
	trunc := binary.BigEndian.Uint32(sum[offset:offset+4]) & 0x7fffffff
	return fmt.Sprintf("%0*d", totpDigits, trunc%pow10(totpDigits))
}

// pow10 returns 10^n for the small n TOTP uses.
func pow10(n int) uint32 {
	out := uint32(1)
	for i := 0; i < n; i++ {
		out *= 10
	}
	return out
}

// MatchTOTPCode reports which step of secret produced code at nowMs, searching
// the tolerance window totpSkewSteps defines, or ok=false when no step in that
// window matches.
//
// Two properties are deliberate:
//
//   - The comparison is CONSTANT-TIME against every candidate step, and every
//     candidate is evaluated even after one matches. A short-circuit would leak,
//     through response timing, both whether an early candidate matched and how
//     many digits of a near-miss were right.
//   - A match reports the STEP, not merely true, because the caller must consume
//     that exact step to make the code single-use (Store.ConsumeTOTPStep).
//     Returning a bare boolean is what makes a TOTP implementation replayable:
//     the window stays open for its full duration and every request inside it
//     succeeds with the same digits.
func MatchTOTPCode(secret []byte, code string, nowMs int64) (step int64, ok bool) {
	code = strings.TrimSpace(code)
	if len(code) != totpDigits {
		return 0, false
	}
	now := TOTPStep(nowMs)
	for d := -totpSkewSteps; d <= totpSkewSteps; d++ {
		candidate := now + int64(d)
		if subtle.ConstantTimeCompare([]byte(TOTPCode(secret, candidate)), []byte(code)) == 1 {
			// No break: the remaining candidates are still computed below so the
			// loop's cost does not depend on WHICH step matched.
			step, ok = candidate, true
		}
	}
	return step, ok
}

// TOTPURI renders the `otpauth://totp/...` URI an authenticator app imports,
// usually by scanning it as a QR code.
//
// issuer and account are label material, not secrets; the secret itself rides
// the `secret` query parameter, which is why this value is returned in a
// RESPONSE BODY and never placed in a URL this server logs, requests, or
// redirects to. The house rule against secrets in query strings is about the
// query strings this deployment ITSELF emits — an `otpauth://` URI is a string
// handed to the operator's phone, and its scheme is not routable here at all.
func TOTPURI(issuer, account string, secret []byte) string {
	label := url.PathEscape(issuer + ":" + account)
	q := url.Values{}
	q.Set("secret", EncodeTOTPSecret(secret))
	q.Set("issuer", issuer)
	q.Set("algorithm", "SHA1")
	q.Set("digits", fmt.Sprint(totpDigits))
	q.Set("period", fmt.Sprint(totpPeriodSeconds))
	return "otpauth://totp/" + label + "?" + q.Encode()
}
