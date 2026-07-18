// Package grant mints relay/1 pairing-grant records (REL-121) for the
// feeder's Wave-1 first-photon path: one grant, minted once, that rides
// the signed desired-state snapshot's `pairing_grants` section (REL-067)
// to the relay, for the relay to later resolve and redeem on a screen's
// behalf.
//
// Shape note: REL-121 defines the grant record itself as `{grant_id,
// purpose, resulting_principal_kind, ttl, redemption_mode, issued_at}` —
// nothing more. REL-126's pairing-code fields (`grant_selector`,
// `fingerprint_commitment`) are a distinct, later, relay-side concern:
// `grant_selector` is a value the relay resolves against `pairing_grants`
// to find the grant this package mints (by `grant_id`), and
// `fingerprint_commitment` is computed by the relay over its own current
// trust-anchor certificate at pairing-code-formation time — this package
// never computes or carries either field, so a grant record it mints stays
// a pure REL-121 shape.
package grant

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// Wave 1 first-photon placeholders: one grant, minted once, always for a
// screen-pairing purpose. A later Player-credential-authority task may
// widen Mint to accept these as parameters once more than one
// purpose/principal-kind is ever minted.
const (
	firstPhotonPurpose                = "pairing"
	firstPhotonResultingPrincipalKind = "screen"
	firstPhotonTTLSeconds             = 900 // matches the relay/1 REL-121 worked example
)

// Mint mints one relay/1 REL-121 pairing-grant record: a fresh, unique
// `grant_id`, `redemption_mode: "one-time"`, and `issued_at` set to now
// (epoch milliseconds, relay/1's Timestamp grammar). The returned record
// carries exactly REL-121's 6 fields — no REL-126 pairing-code fields (see
// package doc).
func Mint() wire.PairingGrant {
	return wire.PairingGrant{
		GrantID:                newGrantID(),
		Purpose:                firstPhotonPurpose,
		ResultingPrincipalKind: firstPhotonResultingPrincipalKind,
		TTL:                    firstPhotonTTLSeconds,
		RedemptionMode:         "one-time",
		IssuedAt:               time.Now().UnixMilli(),
	}
}

// newGrantID returns a fresh, crypto-random unique grant identifier.
// REL-121 gives grant_id no specific grammar beyond "an identifier" (the
// contract's own worked example uses a ULID, but does not mandate one) —
// this package's own choice is a random hex token, prefixed for
// readability in logs/traces.
func newGrantID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand.Reader failing is a fatal environment problem
		// (entropy source unavailable); there is no meaningful error
		// to propagate to callers of a value-returning constructor.
		panic("grant: newGrantID: " + err.Error())
	}
	return fmt.Sprintf("grant-%s", hex.EncodeToString(b[:]))
}
