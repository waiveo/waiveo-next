package playerserver

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/maaxton/waiveo-next/internal/relay/identity"
)

// TestPairingSessionSurvivesSimulatedRelayRestart is defect A's own oracle: a
// channel token minted before a simulated relay restart (the durable store
// closed and reopened from the same on-disk file, then handed to a BRAND NEW
// Server with no in-memory carryover at all — exactly what cmd/waiveo-relay's
// main() builds on every boot) still resolves via LookupChannelToken
// afterward. Before EnablePersistence existed, this failed every time: a
// fresh Server's s.tokens map is always empty, and relay/1's own error
// taxonomy has no way to attribute the resulting 401 to "the process
// restarted" rather than "this token is genuinely unknown" (relay/1 REL-120's
// sole-issuer/sole-verifier role).
func TestPairingSessionSurvivesSimulatedRelayRestart(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "relay.db")
	store, err := identity.Open(dbPath)
	if err != nil {
		t.Fatalf("identity.Open(%q): %v", dbPath, err)
	}

	grant := testGrant()
	srv, _, _ := newTestServer(t, grant)
	srv.EnablePersistence(store)

	resp, raw := doPair(t, srv, PairingRequest{
		HardwareID:    "hw-restart-0001",
		GrantSelector: grant.GrantID,
		Capabilities:  Capabilities{ContentTypes: []string{"image"}, PlayerVersion: "1.0.0"},
	})
	if resp.StatusCode != 200 {
		t.Fatalf("pairing before restart: status = %d, want 200; body = %v", resp.StatusCode, raw)
	}
	var pr PairingResponse
	remarshal(t, raw, &pr)
	if pr.ChannelToken == "" {
		t.Fatal("pairing did not mint a channel token")
	}

	// Simulate a relay process restart: close the store (as a real process
	// exit would) and reopen the SAME on-disk file, then build a fresh
	// Server over it — no grants needed, since the token was already minted
	// before "restart" and this test only exercises token lookup.
	if err := store.Close(); err != nil {
		t.Fatalf("store.Close(): %v", err)
	}
	reopened, err := identity.Open(dbPath)
	if err != nil {
		t.Fatalf("identity.Open(%q) (reopen): %v", dbPath, err)
	}
	t.Cleanup(func() { _ = reopened.Close() })

	restarted, _, _ := newTestServer(t)
	restarted.EnablePersistence(reopened)

	screenID, expiresAt, ok := restarted.LookupChannelToken(pr.ChannelToken)
	if !ok {
		t.Fatal("LookupChannelToken after a simulated relay restart = not found, want the pre-restart session to still validate")
	}
	if screenID != pr.ScreenID {
		t.Errorf("screen_id after restart = %q, want %q", screenID, pr.ScreenID)
	}
	if expiresAt != pr.ExpiresAt {
		t.Errorf("expires_at after restart = %d, want %d", expiresAt, pr.ExpiresAt)
	}
}

// TestPairingWithoutPersistenceDoesNotSurviveRestart pins the OTHER half of
// the same behavior: a Server that never calls EnablePersistence keeps
// today's in-memory-only semantics byte-for-byte — a token minted against it
// is simply gone once that Server value is discarded, exactly as every other
// pre-existing pairing_test.go case assumes.
func TestPairingWithoutPersistenceDoesNotSurviveRestart(t *testing.T) {
	grant := testGrant()
	srv, _, _ := newTestServer(t, grant)

	resp, raw := doPair(t, srv, PairingRequest{
		HardwareID:    "hw-no-persist-0001",
		GrantSelector: grant.GrantID,
		Capabilities:  Capabilities{ContentTypes: []string{"image"}, PlayerVersion: "1.0.0"},
	})
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200; body = %v", resp.StatusCode, raw)
	}
	var pr PairingResponse
	remarshal(t, raw, &pr)

	restarted, _, _ := newTestServer(t) // a fresh Server, standing in for a restarted process
	if _, _, ok := restarted.LookupChannelToken(pr.ChannelToken); ok {
		t.Fatal("a fresh Server (no EnablePersistence) resolved a token it never minted, want not found")
	}
}

// TestPairingRedeemedOneTimeGrantStaysConsumedAcrossRestart pins REL-121's
// redemption-count-never-exceeds-one guarantee ACROSS a relay restart, not
// merely within one process's lifetime: a one-time grant redeemed before a
// simulated restart must still read as redeemed afterward, even though the
// restarted Server's own redeemedGrants cache starts empty. Without
// grantAlreadyRedeemedLocked's durable fallback, a restart would silently
// re-open every already-consumed one-time grant for a second redemption.
func TestPairingRedeemedOneTimeGrantStaysConsumedAcrossRestart(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "relay.db")
	store, err := identity.Open(dbPath)
	if err != nil {
		t.Fatalf("identity.Open(%q): %v", dbPath, err)
	}

	grant := testGrant() // RedemptionMode: "one-time"
	srv, _, _ := newTestServer(t, grant)
	srv.EnablePersistence(store)

	req := PairingRequest{
		HardwareID:    "hw-redeemed-restart-0001",
		GrantSelector: grant.GrantID,
		Capabilities:  Capabilities{ContentTypes: []string{"image"}, PlayerVersion: "1.0.0"},
	}
	if resp, raw := doPair(t, srv, req); resp.StatusCode != 200 {
		t.Fatalf("first redemption: status = %d, want 200; body = %v", resp.StatusCode, raw)
	}

	if err := store.Close(); err != nil {
		t.Fatalf("store.Close(): %v", err)
	}
	reopened, err := identity.Open(dbPath)
	if err != nil {
		t.Fatalf("identity.Open(%q) (reopen): %v", dbPath, err)
	}
	t.Cleanup(func() { _ = reopened.Close() })

	// The restarted Server is built with the SAME grant record still present
	// in its boot-time grant set — modeling the app peer handing back the
	// identical pairing_grants entry on the next desired-state pull after a
	// relay restart, before any newer generation would supersede it.
	restarted, _, _ := newTestServer(t, grant)
	restarted.EnablePersistence(reopened)

	resp, raw := doPair(t, restarted, req)
	assertTypedError(t, resp, raw, "PAIRING_CODE_INVALID")
}

// TestChannelTokenExpiresAtIsUnaffectedByPersistence is a narrow sanity check
// that persisting a session records the SAME expires_at the response
// already carried (PLY-071's 24h bound) — a copy/paste of the wrong field
// into SetPlayerSession would silently corrupt a persisted session's expiry
// without any of the other tests in this file noticing, since they read
// expires_at only from the pairing response itself.
func TestChannelTokenExpiresAtIsUnaffectedByPersistence(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "relay.db")
	store, err := identity.Open(dbPath)
	if err != nil {
		t.Fatalf("identity.Open(%q): %v", dbPath, err)
	}
	t.Cleanup(func() { _ = store.Close() })

	grant := testGrant()
	srv, _, _ := newTestServer(t, grant)
	srv.EnablePersistence(store)

	before := time.Now()
	_, raw := doPair(t, srv, PairingRequest{
		HardwareID:    "hw-expiry-0001",
		GrantSelector: grant.GrantID,
		Capabilities:  Capabilities{ContentTypes: []string{"image"}, PlayerVersion: "1.0.0"},
	})
	var pr PairingResponse
	remarshal(t, raw, &pr)

	sess, ok, err := store.PlayerSession(identity.HashToken(pr.ChannelToken))
	if err != nil {
		t.Fatalf("store.PlayerSession: %v", err)
	}
	if !ok {
		t.Fatal("persisted session not found in store immediately after redemption")
	}
	expiresAt := sess.ExpiresAt
	wantExpiresAt := before.Add(24 * time.Hour).UnixMilli()
	if diff := expiresAt - wantExpiresAt; diff < -1000 || diff > 1000 {
		t.Errorf("persisted expires_at = %d, want approximately %d (PLY-071 24h bound)", expiresAt, wantExpiresAt)
	}
	if expiresAt != pr.ExpiresAt {
		t.Errorf("persisted expires_at = %d, want it to match the response's own expires_at %d", expiresAt, pr.ExpiresAt)
	}
}
