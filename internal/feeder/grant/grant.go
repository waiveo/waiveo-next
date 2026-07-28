// Package grant mints relay/1 pairing-grant records (REL-121/REL-121a): the
// records that ride the signed desired-state snapshot's `pairing_grants`
// section (REL-067) to the relay, for the relay to later resolve and redeem
// on a screen's behalf. The running app mints one per operator "pair this
// screen" request (MintForScreen, persisted by store.AddPairingGrant so it
// rides the next generation); Mint remains the unbound baseline for
// harnesses exercising REL-121's minimal shape.
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

// Every grant this package mints is for the screen-pairing purpose — the one
// purpose/principal-kind pairing has.
const (
	pairingPurpose                = "pairing"
	pairingResultingPrincipalKind = "screen"

	// TTLSeconds is how long a minted pairing grant stays redeemable —
	// matching the relay/1 REL-121 worked example, and exported so the api
	// layer's issuance response can state the same window it actually minted.
	TTLSeconds = 900
)

// Mint mints one relay/1 REL-121 baseline pairing-grant record: a fresh,
// unique `grant_id`, `redemption_mode: "one-time"`, and `issued_at` set to
// now (epoch milliseconds, relay/1's Timestamp grammar). The returned record
// carries exactly REL-121's 6 fields — no screen binding (REL-121a) and no
// REL-126 pairing-code fields (see package doc). It remains for harnesses
// that exercise the baseline wire shape; the running app mints screen-bound
// grants (MintForScreen).
func Mint() wire.PairingGrant {
	return wire.PairingGrant{
		GrantID:                newGrantID(),
		Purpose:                pairingPurpose,
		ResultingPrincipalKind: pairingResultingPrincipalKind,
		TTL:                    TTLSeconds,
		RedemptionMode:         "one-time",
		IssuedAt:               time.Now().UnixMilli(),
	}
}

// MintForScreen mints a screen-bound pairing-grant record (REL-121a): Mint's
// exact record plus `screen_id` — the screen identity row (data-model/1
// DAT-004a) whose id the relay hands back as the paired screen's identity at
// redemption. screenID's existence is the CALLER's concern (the store checks
// it against its own screen rows at persist time); this constructor only
// refuses the empty string, which could never name a row.
func MintForScreen(screenID string) (wire.PairingGrant, error) {
	if screenID == "" {
		return wire.PairingGrant{}, fmt.Errorf("grant: MintForScreen: screenID must not be empty (REL-121a)")
	}
	g := Mint()
	g.ScreenID = screenID
	return g, nil
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
