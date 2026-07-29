package playerserver

import (
	"bytes"
	"log"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/maaxton/waiveo-next/internal/relay/identity"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// revocationsessions_test.go covers what installing a revocation set does to
// credentials ALREADY minted for the revoked screen. Refusing only future
// issuance leaves a revocation undone by its own withdrawal: `revoked` is a set
// the app peer restates in full (REL-066), so the moment it restates one
// without that screen, a token minted before the revocation would authorize
// again — never re-issued, never re-paired.

// TestWithdrawnRevocationDoesNotResurrectAToken is that property directly.
// Revoke at generation 2 (the token is refused), withdraw at generation 3, and
// the SAME token must stay refused. player/1 PLY-071's own rationale for the
// 24-hour bound is that it bounds "a revoked-but-still-cached token's natural
// lifetime", which presupposes the token is dead at the relay; PLY-073 makes an
// honest player clear it and re-pair on the first refusal, so after
// revoke-then-withdraw the only party still holding it kept a copy.
//
// The two refusals carry DIFFERENT codes on purpose, and the difference is the
// oracle: CHANNEL_TOKEN_REVOKED while the screen is revoked (PLY-072 names that
// code for a token whose screen_id is in `revoked`), CHANNEL_TOKEN_INVALID once
// it is not (the credential is simply dead, and PLY-136 makes a player clear it
// and re-pair on that too). A resurrecting implementation answers 200 here; an
// accumulating one — which never withdraws at all — answers REVOKED.
func TestWithdrawnRevocationDoesNotResurrectAToken(t *testing.T) {
	srv, _, token := programTestServer(t)

	screenID, _, ok := srv.LookupChannelToken(token)
	if !ok {
		t.Fatalf("freshly redeemed token %q is not known to the server", token)
	}
	resp, raw := doProgram(t, srv, token, []string{"image", "video"})
	if resp.StatusCode != 200 {
		t.Fatalf("pre-revocation program pull = %d (body %v), want 200 — the token must be known-good before anything revokes it", resp.StatusCode, raw)
	}

	if err := srv.SetRevokedScreens(2, []string{screenID}); err != nil {
		t.Fatalf("SetRevokedScreens(revoke): %v", err)
	}
	resp, raw = doProgram(t, srv, token, []string{"image", "video"})
	assertTypedError(t, resp, raw, "CHANNEL_TOKEN_REVOKED")

	if err := srv.SetRevokedScreens(3, nil); err != nil {
		t.Fatalf("SetRevokedScreens(withdraw): %v", err)
	}
	resp, raw = doProgram(t, srv, token, []string{"image", "video"})
	assertTypedError(t, resp, raw, "CHANNEL_TOKEN_INVALID")

	// And the accessor agrees: a dropped session authorizes nothing, so it is
	// not a session LookupChannelToken hands back.
	if _, _, ok := srv.LookupChannelToken(token); ok {
		t.Error("LookupChannelToken still resolves a session the relay dropped — a withdrawn revocation must not restore a credential it already voided")
	}
}

// TestDroppedSessionStaysDroppedAcrossRestart is the durable half. The
// in-memory sweep alone would leave the resurrection one restart away: a fresh
// Server's token map starts empty and resolves a presented token from the
// durable store, so a session dropped only in this process's memory comes back
// alive on the next boot — and by then the revocation may already have been
// withdrawn, leaving nothing to refuse it with.
//
// Every step here goes through the durable store: the token is minted with
// persistence enabled, the drop happens while the screen is revoked, the store
// is CLOSED and reopened from the same on-disk file (a real process exit), and
// a brand-new Server over it is handed the withdrawn (empty) set a later
// generation carries.
func TestDroppedSessionStaysDroppedAcrossRestart(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "relay.db")
	store, err := identity.Open(dbPath)
	if err != nil {
		t.Fatalf("identity.Open(%q): %v", dbPath, err)
	}

	certPEM, _, priv, _ := testRelaySigningIdentity(t)
	grant := testGrantForScreen(testScreenIDA)
	srv, err := NewServer(certPEM, []wire.PairingGrant{grant}, WallClockMs)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	srv.SetSigningKey(priv)
	srv.SetProgram(1, testScreenIDA, "rev-17", "scheduled", "content", testImageContent())
	srv.EnablePersistence(store)

	_, raw := doPair(t, srv, PairingRequest{
		HardwareID:    "hw-drop-restart",
		GrantSelector: grant.GrantID,
		Capabilities:  Capabilities{ContentTypes: []string{"image"}, PlayerVersion: "1.0.0"},
	})
	var pr PairingResponse
	remarshal(t, raw, &pr)
	if pr.ChannelToken == "" {
		t.Fatalf("pairing minted no channel token: %v", raw)
	}

	if err := srv.SetRevokedScreens(2, []string{testScreenIDA}); err != nil {
		t.Fatalf("SetRevokedScreens(revoke): %v", err)
	}

	// Restart: close the store as a process exit would, reopen the same file,
	// and build a fresh Server with no in-memory carryover whatsoever.
	if err := store.Close(); err != nil {
		t.Fatalf("store.Close(): %v", err)
	}
	reopened, err := identity.Open(dbPath)
	if err != nil {
		t.Fatalf("identity.Open (reopen): %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })

	restarted, err := NewServer(certPEM, nil, WallClockMs)
	if err != nil {
		t.Fatalf("NewServer (restarted): %v", err)
	}
	restarted.SetSigningKey(priv)
	restarted.SetProgram(3, testScreenIDA, "rev-17", "scheduled", "content", testImageContent())
	restarted.EnablePersistence(reopened)

	// Generation 3 withdraws the revocation. The screen may pair again; the
	// pre-revocation token may not serve again.
	if err := restarted.SetRevokedScreens(3, nil); err != nil {
		t.Fatalf("SetRevokedScreens(withdraw): %v", err)
	}

	resp, body := doProgram(t, restarted, pr.ChannelToken, []string{"image", "video"})
	assertTypedError(t, resp, body, "CHANNEL_TOKEN_INVALID")
}

// TestDurableSessionForARevokedScreenIsNeverCachedAsLive constructs, without a
// race, the end state a race would produce.
//
// lookupSession reads the durable store with s.mu RELEASED. So a
// SetRevokedScreens can complete in between: its in-memory sweep does not find
// the token (not cached yet), and its durable UPDATE can land after the read
// returned. A naive backfill would then install a LIVE cache entry for a
// session the relay had just dropped — and the damage outlives the revocation,
// because that entry is only ever consulted once the screen is un-revoked, at
// which point it resurrects the credential.
//
// The state is built directly instead of raced for: a durable session row that
// says LIVE, on a server that already holds the screen in its revocation view.
// That is exactly what the racing lookup would have in hand at the moment it
// backfills.
func TestDurableSessionForARevokedScreenIsNeverCachedAsLive(t *testing.T) {
	store, err := identity.Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatalf("identity.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	certPEM, _, priv, _ := testRelaySigningIdentity(t)
	srv, err := NewServer(certPEM, nil, WallClockMs)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	srv.SetSigningKey(priv)
	srv.SetProgram(1, testScreenIDA, "rev-17", "scheduled", "content", testImageContent())

	// The revocation is installed BEFORE the store is wired, so its durable
	// sweep cannot reach the row written below — the same asymmetry the race
	// produces, and the reason the backfill has to re-check rather than trust
	// what it read.
	if err := srv.SetRevokedScreens(2, []string{testScreenIDA}); err != nil {
		t.Fatalf("SetRevokedScreens(revoke): %v", err)
	}
	const token = "ct-live-row-for-a-revoked-screen"
	if err := store.SetPlayerSession(identity.HashToken(token), testScreenIDA, time.Now().Add(time.Hour).UnixMilli()); err != nil {
		t.Fatalf("SetPlayerSession: %v", err)
	}
	srv.EnablePersistence(store)

	// The lookup that backfills. While the screen is revoked this is refused
	// either way, so the assertion that matters comes after the withdrawal.
	resp, raw := doProgram(t, srv, token, []string{"image", "video"})
	assertTypedError(t, resp, raw, "CHANNEL_TOKEN_REVOKED")

	if err := srv.SetRevokedScreens(3, nil); err != nil {
		t.Fatalf("SetRevokedScreens(withdraw): %v", err)
	}
	resp, raw = doProgram(t, srv, token, []string{"image", "video"})
	assertTypedError(t, resp, raw, "CHANNEL_TOKEN_INVALID")
}

// TestReinstallingTheSameRevocationRunsNoSideEffect is REL-070's own rule
// applied to this method: a relay applying a snapshot whose hash equals its
// persisted last-applied hash "MUST NOT re-run any apply-time side effect", and
// that holds "regardless of whether generation itself advanced". So it is
// asserted under BOTH shapes of a repeat — the same generation number, and a
// HIGHER one carrying the same set, which is the shape the binary's own
// generation fence would let through.
//
// The observable is the durable termination stamp: dropping a session is the
// one apply-time side effect installing a revocation has, and re-running it
// would walk the stamp forward on every later snapshot that restated the set —
// which REL-066 says is every snapshot.
func TestReinstallingTheSameRevocationRunsNoSideEffect(t *testing.T) {
	store, err := identity.Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatalf("identity.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	certPEM, _, priv, _ := testRelaySigningIdentity(t)
	grant := testGrantForScreen(testScreenIDA)

	// A settable clock, so a re-run of the drop would be VISIBLE as a moved
	// stamp rather than hidden behind two readings of the same millisecond.
	nowMs := time.Now().UnixMilli()
	srv, err := NewServer(certPEM, []wire.PairingGrant{grant}, func() int64 { return nowMs })
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	srv.SetSigningKey(priv)
	srv.EnablePersistence(store)

	_, raw := doPair(t, srv, PairingRequest{
		HardwareID:    "hw-reinstall",
		GrantSelector: grant.GrantID,
		Capabilities:  Capabilities{ContentTypes: []string{"image"}, PlayerVersion: "1.0.0"},
	})
	var pr PairingResponse
	remarshal(t, raw, &pr)
	if pr.ChannelToken == "" {
		t.Fatalf("pairing minted no channel token: %v", raw)
	}
	tokenHash := identity.HashToken(pr.ChannelToken)

	if err := srv.SetRevokedScreens(5, []string{testScreenIDA}); err != nil {
		t.Fatalf("SetRevokedScreens(5): %v", err)
	}
	first, ok, err := store.PlayerSession(tokenHash)
	if err != nil || !ok {
		t.Fatalf("PlayerSession after the revocation: ok=%v err=%v", ok, err)
	}
	if !first.Terminated() {
		t.Fatal("installing the revocation did not drop the session — precondition for this case")
	}

	for _, gen := range []int64{5, 6} {
		nowMs += 60_000
		if err := srv.SetRevokedScreens(gen, []string{testScreenIDA}); err != nil {
			t.Fatalf("SetRevokedScreens(%d, same set): %v", gen, err)
		}
		again, ok, err := store.PlayerSession(tokenHash)
		if err != nil || !ok {
			t.Fatalf("PlayerSession after re-installing at generation %d: ok=%v err=%v", gen, ok, err)
		}
		if again.TerminatedAt != first.TerminatedAt {
			t.Errorf("re-installing the same revocation set at generation %d re-ran the session drop (terminated_at %d -> %d) — REL-070 forbids re-running an apply-time side effect", gen, first.TerminatedAt, again.TerminatedAt)
		}
	}
}

// TestPairRefusalDoesNotDistinguishRevokedFromExpired pins the ordering of the
// revocation check against the ttl check in redeem, which is a disclosure
// property rather than a sequencing preference.
//
// The two checks answer DIFFERENT codes — PAIRING_CODE_INVALID for a revoked
// screen, PAIRING_EXPIRED for an elapsed ttl — so whichever runs first decides
// what an EXPIRED grant for a REVOKED screen reports. With the revocation check
// first, one and the same expired grant reported PAIRING_CODE_INVALID when its
// screen was revoked and PAIRING_EXPIRED when it was not: anyone holding a
// pairing code learned the screen's revocation state just by waiting for the
// ttl to elapse.
//
// So the oracle is a comparison, not a constant: the expired grant must draw
// the SAME code whether or not its screen is revoked.
func TestPairRefusalDoesNotDistinguishRevokedFromExpired(t *testing.T) {
	expiredGrant := func() wire.PairingGrant {
		g := testGrantForScreen(testScreenIDA)
		g.IssuedAt = time.Now().Add(-2 * time.Hour).UnixMilli()
		g.TTL = 900 // 15 minutes, elapsed well before now
		return g
	}

	codeFor := func(t *testing.T, revoked []string) string {
		t.Helper()
		certPEM, _, priv, _ := testRelaySigningIdentity(t)
		g := expiredGrant()
		srv, err := NewServer(certPEM, []wire.PairingGrant{g}, WallClockMs)
		if err != nil {
			t.Fatalf("NewServer: %v", err)
		}
		srv.SetSigningKey(priv)
		if err := srv.SetRevokedScreens(1, revoked); err != nil {
			t.Fatalf("SetRevokedScreens: %v", err)
		}
		resp, raw := doPair(t, srv, PairingRequest{
			HardwareID:    "hw-expired",
			GrantSelector: g.GrantID,
			Capabilities:  Capabilities{ContentTypes: []string{"image"}, PlayerVersion: "1.0.0"},
		})
		if resp.StatusCode < 400 {
			t.Fatalf("redeeming an expired grant returned %d, want a 4xx refusal", resp.StatusCode)
		}
		return decodeString(t, raw["code"])
	}

	unrevokedCode := codeFor(t, nil)
	revokedCode := codeFor(t, []string{testScreenIDA})

	if unrevokedCode != "PAIRING_EXPIRED" {
		t.Fatalf("expired grant for an UNREVOKED screen drew %q, want PAIRING_EXPIRED", unrevokedCode)
	}
	if revokedCode != unrevokedCode {
		t.Errorf("expired grant drew %q for a revoked screen and %q for an unrevoked one — one and the same expired grant must not report the screen's revocation state; a holder of the code could read it off by waiting for the ttl", revokedCode, unrevokedCode)
	}
}

// TestRevokedPairRefusalIsLoggedServerSide pins the one thing that MAY differ
// between the two cases above: the relay's own log. The wire answer is
// deliberately indistinguishable, which leaves a field tech at the screen with
// a bare "Pairing Code Invalid" and — before this line existed — nothing
// anywhere in the system saying why, since no authoring surface for revocation
// exists yet either.
//
// It asserts both halves: the log names the screen and the reason, and the
// RESPONSE is unchanged (the same code, no channel token, no screen_id).
func TestRevokedPairRefusalIsLoggedServerSide(t *testing.T) {
	certPEM, _, priv, _ := testRelaySigningIdentity(t)
	grant := testGrantForScreen(testScreenIDA)
	srv, err := NewServer(certPEM, []wire.PairingGrant{grant}, WallClockMs)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	srv.SetSigningKey(priv)
	if err := srv.SetRevokedScreens(7, []string{testScreenIDA}); err != nil {
		t.Fatalf("SetRevokedScreens: %v", err)
	}

	var logged bytes.Buffer
	restore := log.Writer()
	flags := log.Flags()
	log.SetOutput(&logged)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(restore)
		log.SetFlags(flags)
	})

	resp, raw := doPair(t, srv, PairingRequest{
		HardwareID:    "hw-logged",
		GrantSelector: grant.GrantID,
		Capabilities:  Capabilities{ContentTypes: []string{"image"}, PlayerVersion: "1.0.0"},
	})

	// The response is exactly what an unresolvable selector draws — the log
	// must not have bought its readability with a distinguishable wire answer.
	if resp.StatusCode != 400 {
		t.Errorf("refusal status = %d, want 400", resp.StatusCode)
	}
	if got := decodeString(t, raw["code"]); got != "PAIRING_CODE_INVALID" {
		t.Errorf("refusal code = %q, want PAIRING_CODE_INVALID", got)
	}
	if _, minted := raw["channel_token"]; minted {
		t.Error("the refusal minted a channel token")
	}
	if _, disclosed := raw["screen_id"]; disclosed {
		t.Error("the refusal disclosed a screen_id")
	}

	line := logged.String()
	if line == "" {
		t.Fatal("a revoked screen's pairing attempt logged nothing server-side — the refusal is indistinguishable on the wire by design, so the relay's own log is the only place an operator can read why")
	}
	if !strings.Contains(line, testScreenIDA) {
		t.Errorf("log line %q does not name the screen it refused (%s)", line, testScreenIDA)
	}
	if !strings.Contains(line, "revo") {
		t.Errorf("log line %q does not give revocation as the reason", line)
	}
}
