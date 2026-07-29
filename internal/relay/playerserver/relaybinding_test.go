package playerserver

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/http"
	"testing"
	"time"

	"github.com/maaxton/waiveo-next/internal/relay/identity"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// relaybinding_test.go is the oracle for relay/1 REL-121b — the requirement
// that closes the site-wide one-time-redemption gap.
//
// The gap it closes: `pairing_grants` is a section of the ONE signed snapshot
// every relay enrolled to a site applies (REL-067), and REL-122 makes a grant
// redeemable for its whole ttl with no app peer reachable — so redemption
// consumption lives entirely in the redeeming relay, and nothing tells relay B
// that relay A consumed a grant. With two relays enrolled, an unbound one-time
// grant is redeemable once PER RELAY, and because a pairing grant is
// screen-bound (REL-121a) both redemptions resolve to the SAME screen row and
// are served that screen's content.
//
// Every test here therefore uses TWO servers with DISTINCT enrolled identities.
// A single-relay test proves nothing about this defect: the single-relay shape
// passed for the whole time the gap was open.

// relayCertFor mints a certificate whose subject CommonName is relayID —
// exactly the shape internal/feeder/enroll issues an enrolled relay
// (`Subject: pkix.Name{CommonName: relayID}`), which is why NewServer reads a
// relay's own enrolled identity from the certificate it serves.
func relayCertFor(t *testing.T, relayID string) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("ecdsa.GenerateKey: %v", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		t.Fatalf("serial: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: relayID},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("x509.CreateCertificate: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// serverForRelay builds a pairing Server enrolled as relayID, holding grants.
func serverForRelay(t *testing.T, relayID string, grants ...wire.PairingGrant) *Server {
	t.Helper()
	srv, err := NewServer(relayCertFor(t, relayID), grants)
	if err != nil {
		t.Fatalf("NewServer(%s): %v", relayID, err)
	}
	return srv
}

// grantBoundTo builds a one-time, screen-bound (REL-121a), relay-bound
// (REL-121b) pairing grant — the exact record the app peer's own issuance mints
// (internal/feeder/grant.MintForScreen).
func grantBoundTo(grantID, relayID string) wire.PairingGrant {
	return wire.PairingGrant{
		GrantID:                grantID,
		Purpose:                "pairing",
		ResultingPrincipalKind: "screen",
		ScreenID:               "01J8Z4SCREENR0WAAAAAAAAAAA",
		RelayID:                relayID,
		TTL:                    900,
		RedemptionMode:         "one-time",
		IssuedAt:               time.Now().UnixMilli(),
	}
}

const (
	relayA = "01J8ZRELAYAAAAAAAAAAAAAAA1"
	relayB = "01J8ZRELAYBBBBBBBBBBBBBBB2"
)

// TestSecondRelayCannotRedeemAGrantBoundToTheFirst is THE case: two relays,
// both holding the identical signed snapshot's pairing_grants (as they must —
// one snapshot, one hash, every relay of the site applies it), and one pairing
// code observed by someone who dials the other relay with its selector.
//
// The legitimate display redeems at relay A. The observer then presents the
// SAME grant_selector at relay B — which holds that very grant, has never heard
// of A's redemption, and cannot ask anyone (REL-122). Relay B must refuse.
//
// Guard-disabled check: deleting the `grant.RelayID != "" && grant.RelayID !=
// s.relayID` clause in redeem makes relay B mint a second, fully valid channel
// token for the same screen_id, and this test fails on the status assertion —
// verified by running it with that clause removed.
func TestSecondRelayCannotRedeemAGrantBoundToTheFirst(t *testing.T) {
	g := grantBoundTo("grant-two-relay-0123456789ab", relayA)

	// One snapshot, delivered to both relays verbatim.
	srvA := serverForRelay(t, relayA, g)
	srvB := serverForRelay(t, relayB, g)

	req := PairingRequest{
		HardwareID:    "hw-legit-0001",
		GrantSelector: g.GrantID,
		Capabilities:  Capabilities{ContentTypes: []string{"image"}, PlayerVersion: "1.0.0"},
	}

	respA, rawA := doPair(t, srvA, req)
	if respA.StatusCode != http.StatusOK {
		t.Fatalf("the bound relay must redeem its own grant: status = %d, want 200; body = %v", respA.StatusCode, rawA)
	}
	var redeemedA struct {
		ChannelToken string `json:"channel_token"`
		ScreenID     string `json:"screen_id"`
	}
	remarshal(t, rawA, &redeemedA)
	if redeemedA.ChannelToken == "" {
		t.Fatal("the bound relay minted no channel token")
	}

	// The attacker, with the same selector, at the other enrolled relay.
	respB, rawB := doPair(t, srvB, PairingRequest{
		HardwareID:    "hw-attacker-0001",
		GrantSelector: g.GrantID,
		Capabilities:  Capabilities{ContentTypes: []string{"image"}, PlayerVersion: "1.0.0"},
	})
	assertTypedError(t, respB, rawB, "PAIRING_CODE_INVALID")
	if _, minted := rawB["channel_token"]; minted {
		t.Fatalf("relay B minted a channel token for a grant bound to relay A: %v", rawB)
	}
	if _, leaked := rawB["screen_id"]; leaked {
		t.Fatalf("relay B disclosed a screen_id (%s was A's) for a grant it may not consume: %v", redeemedA.ScreenID, rawB)
	}
}

// TestRefusedForeignGrantIsIndistinguishableFromAnUnknownSelector: a relay that
// may not consume a grant must not become an oracle for what its siblings hold.
// REL-121b requires "the same typed rejection an unresolvable selector draws",
// and the ttl check must not run first either — an EXPIRED foreign grant that
// answered PAIRING_EXPIRED would confirm the selector names a real record.
//
// Guard-disabled check: moving the binding check below the ttl check makes the
// expired-foreign case answer PAIRING_EXPIRED and fails the third assertion.
func TestRefusedForeignGrantIsIndistinguishableFromAnUnknownSelector(t *testing.T) {
	live := grantBoundTo("grant-foreign-live-01234567", relayA)
	expired := grantBoundTo("grant-foreign-expired-0123", relayA)
	expired.IssuedAt = time.Now().Add(-time.Hour).UnixMilli() // well past its 900s ttl

	srvB := serverForRelay(t, relayB, live, expired)

	cases := []struct{ name, selector string }{
		{"a selector naming nothing at all", "grant-does-not-exist-000000"},
		{"a live grant bound to the other relay", live.GrantID},
		{"an EXPIRED grant bound to the other relay", expired.GrantID},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, raw := doPair(t, srvB, PairingRequest{
				HardwareID:    "hw-probe-0001",
				GrantSelector: tc.selector,
				Capabilities:  Capabilities{ContentTypes: []string{"image"}, PlayerVersion: "1.0.0"},
			})
			assertTypedError(t, resp, raw, "PAIRING_CODE_INVALID")
		})
	}
}

// TestRefusalAtTheWrongRelayDoesNotConsumeTheGrant: the refusal must leave the
// grant untouched, so the legitimate display can still pair afterwards. A
// refusal that marked the grant consumed would turn this guard into a denial of
// service — anyone who observed a code could burn it by presenting it at the
// wrong relay.
//
// Guard-disabled check: recording the consumption before the binding check (or
// checking the binding after grantAlreadyRedeemedLocked marks it) makes relay
// A's own redemption fail here.
func TestRefusalAtTheWrongRelayDoesNotConsumeTheGrant(t *testing.T) {
	g := grantBoundTo("grant-not-burned-0123456789", relayA)
	srvA := serverForRelay(t, relayA, g)
	srvB := serverForRelay(t, relayB, g)

	respB, rawB := doPair(t, srvB, PairingRequest{
		HardwareID:    "hw-attacker-0002",
		GrantSelector: g.GrantID,
		Capabilities:  Capabilities{ContentTypes: []string{"image"}, PlayerVersion: "1.0.0"},
	})
	assertTypedError(t, respB, rawB, "PAIRING_CODE_INVALID")

	respA, rawA := doPair(t, srvA, PairingRequest{
		HardwareID:    "hw-legit-0002",
		GrantSelector: g.GrantID,
		Capabilities:  Capabilities{ContentTypes: []string{"image"}, PlayerVersion: "1.0.0"},
	})
	if respA.StatusCode != http.StatusOK {
		t.Fatalf("the bound relay could not redeem after a refusal elsewhere: status = %d, want 200; body = %v", respA.StatusCode, rawA)
	}
}

// TestUnboundGrantStaysRedeemableAtAnyRelay pins REL-121b's own scope: the
// binding is enforced only when it is PRESENT. A record carrying no relay_id is
// REL-121's baseline shape (the one conformance harnesses mint) and stays
// redeemable by whichever relay the snapshot reached — which is exactly why
// REL-121c puts the obligation to bind on the APP PEER, and why the app store
// refuses to persist an unbound one-time grant at all.
func TestUnboundGrantStaysRedeemableAtAnyRelay(t *testing.T) {
	g := grantBoundTo("grant-unbound-0123456789ab", "")
	g.RelayID = ""

	srvB := serverForRelay(t, relayB, g)
	resp, raw := doPair(t, srvB, PairingRequest{
		HardwareID:    "hw-baseline-0001",
		GrantSelector: g.GrantID,
		Capabilities:  Capabilities{ContentTypes: []string{"image"}, PlayerVersion: "1.0.0"},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("an unbound baseline grant must stay redeemable: status = %d, want 200; body = %v", resp.StatusCode, raw)
	}
}

// TestRedemptionOwesAnUpstreamReport is REL-124's own oracle at this layer: a
// redemption the relay performed is enqueued for upstream report, carrying the
// grant it consumed, and is cleared only when the caller says the report was
// acknowledged.
func TestRedemptionOwesAnUpstreamReport(t *testing.T) {
	g := grantBoundTo("grant-reported-0123456789ab", relayA)
	srvA := serverForRelay(t, relayA, g)

	if owed, err := srvA.PendingRedemptionReports(); err != nil || len(owed) != 0 {
		t.Fatalf("owed before any redemption = %v (err %v), want none", owed, err)
	}

	resp, raw := doPair(t, srvA, PairingRequest{
		HardwareID:    "hw-report-0001",
		GrantSelector: g.GrantID,
		Capabilities:  Capabilities{ContentTypes: []string{"image"}, PlayerVersion: "1.0.0"},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("redemption: status = %d, want 200; body = %v", resp.StatusCode, raw)
	}

	owed, err := srvA.PendingRedemptionReports()
	if err != nil {
		t.Fatalf("PendingRedemptionReports: %v", err)
	}
	if len(owed) != 1 || owed[0].GrantID != g.GrantID {
		t.Fatalf("owed after redemption = %+v, want exactly the redeemed grant %s (REL-124)", owed, g.GrantID)
	}
	if owed[0].RedeemedAt == 0 {
		t.Error("the owed report carries no redeemed_at")
	}

	if err := srvA.MarkRedemptionReported(owed[0].Seq); err != nil {
		t.Fatalf("MarkRedemptionReported: %v", err)
	}
	if after, err := srvA.PendingRedemptionReports(); err != nil || len(after) != 0 {
		t.Fatalf("owed after acknowledgement = %v (err %v), want none", after, err)
	}
}

// TestOwedRedemptionReportSurvivesARelayRestart: REL-124 requires a report "at
// the next telemetry or connection opportunity", and REL-124d requires an
// unacknowledged one be re-sent at each subsequent opportunity — which a relay
// that restarts before ever connecting can only honour from durable storage
// (REL-142a). A SECOND Server over the SAME store stands in for the restart,
// exactly as the persistence tests for channel tokens do.
//
// Guard-disabled check: dropping the sessionStore branch in
// recordRedemptionOwedLocked (in-memory only) leaves the restarted server owing
// nothing, and this fails.
func TestOwedRedemptionReportSurvivesARelayRestart(t *testing.T) {
	store, err := identity.Open(":memory:")
	if err != nil {
		t.Fatalf("identity.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	g := grantBoundTo("grant-restart-0123456789ab", relayA)
	cert := relayCertFor(t, relayA)

	before, err := NewServer(cert, []wire.PairingGrant{g})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	before.EnablePersistence(store)

	resp, raw := doPair(t, before, PairingRequest{
		HardwareID:    "hw-restart-0001",
		GrantSelector: g.GrantID,
		Capabilities:  Capabilities{ContentTypes: []string{"image"}, PlayerVersion: "1.0.0"},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("redemption: status = %d, want 200; body = %v", resp.StatusCode, raw)
	}

	// The restart: a brand-new Server, same durable store.
	after, err := NewServer(cert, []wire.PairingGrant{g})
	if err != nil {
		t.Fatalf("NewServer (restart): %v", err)
	}
	after.EnablePersistence(store)

	owed, err := after.PendingRedemptionReports()
	if err != nil {
		t.Fatalf("PendingRedemptionReports after restart: %v", err)
	}
	if len(owed) != 1 || owed[0].GrantID != g.GrantID {
		t.Fatalf("owed after restart = %+v, want the redemption performed before it (REL-124/REL-142a)", owed)
	}
	if err := after.MarkRedemptionReported(owed[0].Seq); err != nil {
		t.Fatalf("MarkRedemptionReported: %v", err)
	}
	if left, err := after.PendingRedemptionReports(); err != nil || len(left) != 0 {
		t.Fatalf("owed after acknowledgement = %v (err %v), want none", left, err)
	}
}

// TestOwedReportSeqIsNotReusedAfterAnAcknowledgement: a report's ledger
// position must stay unique for as long as any report is outstanding.
// Deriving it from the pending SLICE LENGTH reuses a position the moment an
// earlier report is acknowledged, and the next MarkRedemptionReported then
// retires two distinct owed reports on one acknowledgement — the silent loss
// REL-124d exists to forbid.
//
// Guard-disabled check: replacing s.nextReportSeq with
// int64(len(s.pendingReports))+1 in recordRedemptionOwedLocked makes the
// second and third reports share a seq, and the final assertion sees an empty
// ledger where one report is still owed.
func TestOwedReportSeqIsNotReusedAfterAnAcknowledgement(t *testing.T) {
	grants := []wire.PairingGrant{
		grantBoundTo("grant-seq-a-0123456789abcd", relayA),
		grantBoundTo("grant-seq-b-0123456789abcd", relayA),
		grantBoundTo("grant-seq-c-0123456789abcd", relayA),
	}
	srv := serverForRelay(t, relayA, grants...)

	redeem := func(g wire.PairingGrant) {
		t.Helper()
		resp, raw := doPair(t, srv, PairingRequest{
			HardwareID:    "hw-seq-" + g.GrantID,
			GrantSelector: g.GrantID,
			Capabilities:  Capabilities{ContentTypes: []string{"image"}, PlayerVersion: "1.0.0"},
		})
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("redeem %s: status = %d, want 200; body = %v", g.GrantID, resp.StatusCode, raw)
		}
	}

	redeem(grants[0])
	redeem(grants[1])

	owed, err := srv.PendingRedemptionReports()
	if err != nil || len(owed) != 2 {
		t.Fatalf("owed = %+v (err %v), want 2", owed, err)
	}
	// Acknowledge the FIRST, then redeem a third — the moment a length-derived
	// seq would collide with the still-outstanding second report.
	if err := srv.MarkRedemptionReported(owed[0].Seq); err != nil {
		t.Fatalf("MarkRedemptionReported: %v", err)
	}
	redeem(grants[2])

	owed, err = srv.PendingRedemptionReports()
	if err != nil || len(owed) != 2 {
		t.Fatalf("owed after ack+redeem = %+v (err %v), want 2", owed, err)
	}
	if owed[0].Seq == owed[1].Seq {
		t.Fatalf("two outstanding reports share ledger position %d — one acknowledgement retires both", owed[0].Seq)
	}

	// Acknowledging one must leave exactly the other owed.
	if err := srv.MarkRedemptionReported(owed[0].Seq); err != nil {
		t.Fatalf("MarkRedemptionReported: %v", err)
	}
	left, err := srv.PendingRedemptionReports()
	if err != nil || len(left) != 1 || left[0].GrantID != owed[1].GrantID {
		t.Fatalf("owed after the second acknowledgement = %+v (err %v), want exactly %s", left, err, owed[1].GrantID)
	}
}
