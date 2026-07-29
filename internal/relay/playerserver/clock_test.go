package playerserver

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// clock_test.go defends the ONE property NewServer's required clock argument
// exists to establish: every time-windowed decision this package makes reads
// the clock its constructor was given, and nothing here reaches for the host's
// reading beside it.
//
// It matters because of what a relay passes there. cmd/waiveo-relay passes a
// floor-aware reading — the latest of the host wall clock, the persisted
// advance-only clock floor (relay/1 REL-130) and the hint-adjusted runtime
// clock (REL-133). While no floor has ever been established that is
// indistinguishable from time.Now, so a package that quietly read the host
// clock would look correct in every test and on every relay that has never
// verified a time. The two readings separate the moment a floor sits above the
// host clock — which is exactly the situation REL-130 exists for, a host clock
// rolled back below a time the relay has already verified — and they separate
// in the direction that reopens a window that had closed: an elapsed pairing
// grant becomes redeemable again, an expired channel token becomes valid again.
//
// So these cases pass readings that could not have come from the host clock,
// far enough from any plausible time.Now that a regression to a hardcoded wall
// clock cannot coincidentally satisfy them.

// fixedNowMs is a reading no host clock in this test run could produce: roughly
// the year 4000, in epoch milliseconds. It stands in for a persisted clock
// floor sitting far above a rolled-back host clock.
const fixedNowMs int64 = 64_060_588_800_000

// TestNewServerRefusesWithoutAClock: the clock is REQUIRED, not defaulted.
// There is deliberately no wall-clock fallback — a default that silently reads
// the host clock is exactly the defect the argument was added to remove, and a
// seam nobody is forced to fill is a seam that stays unfilled, which is how the
// hardcoded time.Now survived here this long.
func TestNewServerRefusesWithoutAClock(t *testing.T) {
	certPEM, _, _, _ := testRelaySigningIdentity(t)
	if _, err := NewServer(certPEM, nil, nil); err == nil {
		t.Fatal("NewServer with a nil clock succeeded; a server that can be built without naming its clock will eventually be built without one")
	}
}

// TestGrantTTLIsEnforcedOnTheInjectedClock is the defect this fix closes. A
// grant issued and elapsed in the past is presented while the HOST clock has
// been rolled back to before its issuance — the concrete REL-130 scenario, a
// box whose RTC reset or was set backwards.
//
// Read on the host clock, the grant looks fresh and redeems. Read on the
// relay's floor-aware clock, it is elapsed and MUST be refused PAIRING_EXPIRED:
// "on restart it MUST NOT adopt a wall-clock reading earlier than this
// persisted floor" is only true if the checks that gate a time window actually
// read the floored value.
func TestGrantTTLIsEnforcedOnTheInjectedClock(t *testing.T) {
	certPEM, _, _, _ := testRelaySigningIdentity(t)

	// Issued at the injected clock's own instant, with a 15-minute ttl; the
	// injected clock now reads an hour past that, so the ttl has elapsed.
	grant := testGrantForScreen(testScreenIDA)
	grant.IssuedAt = fixedNowMs - int64(time.Hour/time.Millisecond)
	grant.TTL = 900

	srv, err := NewServer(certPEM, []wire.PairingGrant{grant}, func() int64 { return fixedNowMs })
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	if _, err := srv.redeem(grant.GrantID); err == nil {
		t.Fatal("an elapsed grant redeemed — the ttl check is reading a clock other than the server's, so a rolled-back host clock re-extends an expired grant (REL-130)")
	} else if err != errPairingExpired {
		t.Fatalf("redeem error = %v, want errPairingExpired", err)
	}

	// The converse, so this is not passing merely because redemption is broken:
	// the SAME grant, still inside its ttl on the server's clock, redeems.
	live := testGrantForScreen(testScreenIDB)
	live.IssuedAt = fixedNowMs - 1000
	live.TTL = 900
	srv2, err := NewServer(certPEM, []wire.PairingGrant{live}, func() int64 { return fixedNowMs })
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	if _, err := srv2.redeem(live.GrantID); err != nil {
		t.Fatalf("a live grant failed to redeem on the injected clock: %v", err)
	}
}

// TestMintedChannelTokenIsStampedFromTheInjectedClock: a redeemed credential's
// issued_at/expires_at come from the server's clock, not the host's. They are
// the values PLY-072's later expiry check compares against, so a token stamped
// from one clock and checked against another is a window of unknown width.
func TestMintedChannelTokenIsStampedFromTheInjectedClock(t *testing.T) {
	certPEM, _, _, _ := testRelaySigningIdentity(t)
	grant := testGrantForScreen(testScreenIDA)
	grant.IssuedAt = fixedNowMs

	srv, err := NewServer(certPEM, []wire.PairingGrant{grant}, func() int64 { return fixedNowMs })
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	rec, err := srv.redeem(grant.GrantID)
	if err != nil {
		t.Fatalf("redeem: %v", err)
	}
	if rec.IssuedAt != fixedNowMs {
		t.Errorf("channel token issued_at = %d, want the injected reading %d — minted from a clock the server was not given", rec.IssuedAt, fixedNowMs)
	}
	if want := fixedNowMs + channelTokenTTL.Milliseconds(); rec.ExpiresAt != want {
		t.Errorf("channel token expires_at = %d, want %d (issued_at + the PLY-071 bound)", rec.ExpiresAt, want)
	}
}

// TestChannelTokenExpiryAndLeaseStampsReadTheInjectedClock covers the same
// defect class on the program path: PLY-072's expiry comparison, and the Lease's
// own issued_at/valid_until (PLY-092).
//
// The expiry check is the security-relevant one — a rolled-back host clock must
// not revive an expired credential — and the stamps ride the same reading
// because a Lease whose window came from a different clock than the check that
// admitted it describes a validity period the relay does not actually believe
// in.
func TestChannelTokenExpiryAndLeaseStampsReadTheInjectedClock(t *testing.T) {
	certPEM, _, priv, _ := testRelaySigningIdentity(t)
	grant := testGrantForScreen(testScreenIDA)
	grant.IssuedAt = fixedNowMs

	// The clock is settable so one server can be moved past its own token's
	// expiry without any host-clock involvement at all.
	now := fixedNowMs
	srv, err := NewServer(certPEM, []wire.PairingGrant{grant}, func() int64 { return now })
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	srv.SetProgram(1, testScreenIDA, "rev-clock", "scheduled", "content", testImageContent(), priv)

	rec, err := srv.redeem(grant.GrantID)
	if err != nil {
		t.Fatalf("redeem: %v", err)
	}

	// While the server's clock is inside the token's window, the Lease is issued
	// and its stamps come from that same clock.
	lease := pullLease(t, srv, rec.ChannelToken)
	if lease.IssuedAt != now {
		t.Errorf("Lease issued_at = %d, want the injected reading %d — stamped from a clock the server was not given (PLY-092)", lease.IssuedAt, now)
	}
	if want := now + leaseValidity.Milliseconds(); lease.ValidUntil != want {
		t.Errorf("Lease valid_until = %d, want %d", lease.ValidUntil, want)
	}

	// Move the SERVER's clock past the token's expiry. The host clock has not
	// moved at all, so a handler reading time.Now still sees a valid token.
	now = rec.ExpiresAt + 1
	resp, raw := doProgram(t, srv, rec.ChannelToken, []string{"image"})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("program pull after expiry status = %d, want 401 CHANNEL_TOKEN_EXPIRED — the expiry check is reading the host clock, so a rolled-back host clock revives an expired token (PLY-072)", resp.StatusCode)
	}
	if code := problemCode(t, raw); code != "CHANNEL_TOKEN_EXPIRED" {
		t.Errorf("problem code = %q, want CHANNEL_TOKEN_EXPIRED", code)
	}
}

// problemCode reads the `code` member out of a decoded Problem body.
func problemCode(t *testing.T, raw map[string]json.RawMessage) string {
	t.Helper()
	var code string
	if v, ok := raw["code"]; ok {
		if err := json.Unmarshal(v, &code); err != nil {
			t.Fatalf("decode problem code: %v", err)
		}
	}
	return code
}
