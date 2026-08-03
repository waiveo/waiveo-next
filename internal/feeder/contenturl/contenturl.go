// Package contenturl mints and verifies signed, expiring content URLs — the
// capability REL-061 names when it calls a `screen_programs[].content[]` entry
// a "signed content reference", and which until now this codebase did not have.
//
// # What shipped before this, and why it was a defect
//
// The content origin served `/content/<hex>` to anyone who asked, and every
// reference stamped `expires_at: 0`. Content-addressed storage makes that
// SOUND safe — you cannot ask for bytes whose hash you do not already know —
// but the hash is not a secret. It rides the snapshot to every enrolled relay
// and onward to every screen, and a `content` array is exactly a list of the
// digests a site is currently showing. Anyone who observes one holds a
// permanent, unrevocable read capability for that asset: no expiry to wait
// out, no signature to invalidate, and no way to tell a legitimate fetch from
// a replayed one. Rotating the asset does not help, because the old digest
// keeps serving.
//
// # The shape
//
// A minted URL carries two query parameters beside the content-addressed path:
//
//	<base>/content/<hex>?exp=<unix-ms>&sig=<hex-hmac>
//
// `sig` is HMAC-SHA256 over a domain-separated statement binding the digest to
// the deadline, under a key only the minting side and the verifying origin
// hold. The path itself is untouched, so REL-061's `<base>/content/<hex>` URL
// grammar — and every existing consumer that parses it — still reads.
//
// # Why the statement is built the way it is
//
// The signed statement is a domain tag, the digest, and the deadline, joined
// by newlines. That construction is only unambiguous if no field can contain
// the separator, so Sign and Verify BOTH reject a digest that is not pure
// lowercase hex before it ever reaches the MAC. This is not defensive
// throat-clearing: the digest arrives from the request PATH, an attacker
// controls it byte for byte, and a digest permitted to carry a newline would
// let one statement be read as another — two different (digest, deadline)
// pairs producing identical signed bytes. This repo has already found that
// exact collision in its envelope signing, and the guard is the lesson.
//
// A key is likewise rejected when empty. HMAC accepts a zero-length key
// happily and produces a perfectly well-formed signature that anybody can
// reproduce, so an unset key would not fail loudly — it would authenticate
// every forgery.
//
// # What this package is NOT
//
// It is not the screen's credential. A signed URL authorizes ONE asset until
// ONE deadline; it says nothing about who is fetching. Authenticating the
// fetcher is a separate obligation, and a bearer URL is deliberately weaker:
// it is the half that can ride REL-066's relay-constructed URLs, which are
// built by a party that holds no session with the origin.
package contenturl

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strconv"
)

// Query parameter names. Kept as constants because the verifying side reads
// what the minting side wrote, and a typo in either is a signature that never
// validates — a failure that looks exactly like an attack.
const (
	QueryExpires   = "exp"
	QuerySignature = "sig"
)

// statementDomain separates this signature's input from every other MAC this
// codebase computes under any key. Without it, a key reused for a second
// purpose could have one construction's bytes accepted as the other's.
const statementDomain = "waiveo-content-url/1"

// Distinguishable refusals. A caller branches on these: an expired URL is a
// client that needs a fresh reference, while a bad signature is a forgery or a
// key mismatch, and reporting the two as one error would hide an attack inside
// ordinary staleness.
var (
	// ErrMalformed is a URL missing a parameter, or carrying one that is not
	// the shape it must be. It is NOT a signature failure — nothing was
	// checked, because there was nothing well-formed to check.
	ErrMalformed = errors.New("contenturl: malformed signed content URL")
	// ErrExpired is a well-formed, correctly signed URL presented past its
	// deadline. The signature is verified BEFORE expiry is judged, so this
	// error is only ever reached by a URL this deployment really minted.
	ErrExpired = errors.New("contenturl: signed content URL has expired")
	// ErrSignatureInvalid is a well-formed URL whose signature does not verify
	// under this key: a forgery, a tampered digest or deadline, or a key that
	// has rotated out from under a reference still in flight.
	ErrSignatureInvalid = errors.New("contenturl: signed content URL signature is invalid")
	// ErrNoKey is a Sign or Verify attempted with no key. Returned rather than
	// tolerated: HMAC under an empty key is well-formed and reproducible by
	// anyone, so a missing key must fail loudly rather than authenticate
	// everything.
	ErrNoKey = errors.New("contenturl: no signing key")
)

// isLowerHex reports whether s is a non-empty string of lowercase hex digits.
//
// This is the guard that makes the newline-joined statement unambiguous, so it
// is written out rather than delegated to hex.DecodeString: DecodeString
// accepts UPPERCASE too, and two spellings of one digest that both verify
// would be two distinct URLs for the same signed capability.
func isLowerHex(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// statement is the exact byte string the MAC is computed over.
//
// Callers reach it only through Sign and Verify, both of which validate
// hexDigest first — the separator's unambiguity depends on that having
// happened, and a third caller that forgot would reintroduce the collision.
func statement(hexDigest string, expiresAtMs int64) []byte {
	return []byte(statementDomain + "\n" + hexDigest + "\n" + strconv.FormatInt(expiresAtMs, 10))
}

// Sign returns the query string authorizing hexDigest until expiresAtMs
// (inclusive), in the form `exp=<ms>&sig=<hex>`.
//
// expiresAtMs must be positive. Zero is refused specifically because it is the
// value this codebase used to stamp on every reference to mean "no expiry
// policy": accepting it here would let that same inert value flow through a
// signing path and read as a deliberate deadline of the epoch — either
// permanently expired or, under a laxer comparison, permanently valid. Neither
// is a policy anyone chose.
func Sign(key []byte, hexDigest string, expiresAtMs int64) (string, error) {
	if len(key) == 0 {
		return "", ErrNoKey
	}
	if !isLowerHex(hexDigest) {
		return "", fmt.Errorf("%w: digest %q is not lowercase hex", ErrMalformed, hexDigest)
	}
	if expiresAtMs <= 0 {
		return "", fmt.Errorf("%w: expiry %d is not a positive instant", ErrMalformed, expiresAtMs)
	}
	mac := hmac.New(sha256.New, key)
	mac.Write(statement(hexDigest, expiresAtMs))
	q := url.Values{}
	q.Set(QueryExpires, strconv.FormatInt(expiresAtMs, 10))
	q.Set(QuerySignature, hex.EncodeToString(mac.Sum(nil)))
	return q.Encode(), nil
}

// URL returns the complete signed URL for hexDigest under base — the
// `<base>/content/<hex>` grammar REL-061 defines, with Sign's query appended.
func URL(base string, key []byte, hexDigest string, expiresAtMs int64) (string, error) {
	q, err := Sign(key, hexDigest, expiresAtMs)
	if err != nil {
		return "", err
	}
	return base + "/content/" + hexDigest + "?" + q, nil
}

// Verify checks the signed URL for hexDigest carrying query q, as of nowMs.
//
// # Order of checks, and why it is this order
//
// The signature is verified BEFORE the deadline is judged. Reversed, an
// expired-URL answer would be given to a request whose signature was never
// checked — telling an attacker that a digest they guessed and a deadline they
// invented were otherwise acceptable, and turning this into an oracle for
// which digests the origin holds. Verifying first means ErrExpired is only
// ever reached by a URL this deployment genuinely minted.
//
// # The deadline boundary
//
// A URL is valid THROUGH its deadline and expired strictly after it: nowMs
// greater than exp refuses. The inclusive reading is the one that matches what
// `expires_at` says on the wire — the instant it expires AT is the last
// instant it is good for — and the alternative silently shortens every TTL in
// the system by the clock's own resolution.
func Verify(key []byte, hexDigest string, q url.Values, nowMs int64) error {
	if len(key) == 0 {
		return ErrNoKey
	}
	if !isLowerHex(hexDigest) {
		return fmt.Errorf("%w: digest %q is not lowercase hex", ErrMalformed, hexDigest)
	}
	rawExp, rawSig := q.Get(QueryExpires), q.Get(QuerySignature)
	if rawExp == "" || rawSig == "" {
		return fmt.Errorf("%w: missing %s or %s", ErrMalformed, QueryExpires, QuerySignature)
	}
	expiresAtMs, err := strconv.ParseInt(rawExp, 10, 64)
	if err != nil {
		return fmt.Errorf("%w: expiry %q is not an integer", ErrMalformed, rawExp)
	}
	got, err := hex.DecodeString(rawSig)
	if err != nil {
		return fmt.Errorf("%w: signature %q is not hex", ErrMalformed, rawSig)
	}
	mac := hmac.New(sha256.New, key)
	mac.Write(statement(hexDigest, expiresAtMs))
	if !hmac.Equal(got, mac.Sum(nil)) {
		return ErrSignatureInvalid
	}
	if nowMs > expiresAtMs {
		return fmt.Errorf("%w: deadline %d, now %d", ErrExpired, expiresAtMs, nowMs)
	}
	return nil
}
