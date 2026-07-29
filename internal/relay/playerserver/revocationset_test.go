package playerserver

import (
	"testing"

	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// revocationset_test.go covers SetRevokedScreens as a SET-REPLACE — the shape
// relay/1 REL-066 actually delivers, a whole list restated on every snapshot —
// rather than the one-at-a-time mark it replaced. Two properties the old shape
// could not express or could not defend: a screen leaving the set is
// un-revoked, and a strictly-older generation's late write is dropped.

// TestSetRevokedScreensReplacesRatherThanAccumulates is the property the
// one-at-a-time mutator could not express: `revoked` is a set the app peer
// restates in full, so a screen the NEWER generation omits is thereby
// un-revoked. An implementation that unioned each snapshot's list into what it
// already held would enforce a revocation its app peer had withdrawn, forever —
// REL-066 defines no negative entry that could ever take it back out.
//
// The withdrawal is observed on the CODE, not on a 200: what un-revocation
// restores is the ability to PAIR, and the credential minted before the
// revocation stays dead (TestWithdrawnRevocationDoesNotResurrectAToken). An
// accumulating implementation would still answer CHANNEL_TOKEN_REVOKED for the
// old token, so the change to CHANNEL_TOKEN_INVALID is exactly the replace, and
// the fresh redemption below is exactly what it restored.
func TestSetRevokedScreensReplacesRatherThanAccumulates(t *testing.T) {
	srv, _, token := programTestServer(t)

	screenID, _, ok := srv.LookupChannelToken(token)
	if !ok {
		t.Fatalf("freshly redeemed token %q is not known to the server", token)
	}

	if err := srv.SetRevokedScreens(2, []string{screenID}); err != nil {
		t.Fatalf("SetRevokedScreens: %v", err)
	}
	resp, raw := doProgram(t, srv, token, []string{"image", "video"})
	assertTypedError(t, resp, raw, "CHANNEL_TOKEN_REVOKED")

	// Generation 3 no longer names the screen. The relay's last-synced copy IS
	// this list, so there is nothing left to enforce against it — the screen is
	// credentialable again.
	if err := srv.SetRevokedScreens(3, []string{"01J8Z3K4N5P6Q7R8S9T0V1W2ZZ"}); err != nil {
		t.Fatalf("SetRevokedScreens: %v", err)
	}

	resp, raw = doProgram(t, srv, token, []string{"image", "video"})
	assertTypedError(t, resp, raw, "CHANNEL_TOKEN_INVALID")

	// What the withdrawal restored: a NEW pairing for the same screen succeeds
	// and its token serves. Generation 3 carries a fresh grant for it, the way
	// a real app peer would author one for a screen it has just restored.
	fresh := testGrantForScreen(screenID)
	fresh.GrantID = "grant-test-after-withdrawal"
	srv.SetPairingGrants(3, []wire.PairingGrant{fresh})

	_, pairRaw := doPair(t, srv, PairingRequest{
		HardwareID:    "hw-0001",
		GrantSelector: fresh.GrantID,
		Capabilities:  Capabilities{ContentTypes: []string{"image", "video"}, PlayerVersion: "1.0.0"},
	})
	var pr PairingResponse
	remarshal(t, pairRaw, &pr)
	if pr.ChannelToken == "" {
		t.Fatalf("pairing after the revocation was withdrawn minted no channel token: %v", pairRaw)
	}

	resp, raw = doProgram(t, srv, pr.ChannelToken, []string{"image", "video"})
	if resp.StatusCode != 200 {
		t.Fatalf("program pull on a token minted after the withdrawal = %d (body %v), want 200 — a set-replace must un-revoke, not accumulate", resp.StatusCode, raw)
	}
	if _, isProblem := raw["code"]; isProblem {
		t.Errorf("program pull after un-revocation returned a typed error %v, want an ordinary Lease", raw["code"])
	}
}

// TestSetRevokedScreensFencesOlderGeneration pins the REL-052/056 fence. A
// superseded generation's late write must not reinstate a revocation the
// current generation withdrew, nor withdraw one it added — both are credential
// decisions, and the second is a security regression no later snapshot would
// correct until the app peer happened to change the set again.
func TestSetRevokedScreensFencesOlderGeneration(t *testing.T) {
	srv, _, token := programTestServer(t)

	screenID, _, ok := srv.LookupChannelToken(token)
	if !ok {
		t.Fatalf("freshly redeemed token %q is not known to the server", token)
	}

	if err := srv.SetRevokedScreens(9, []string{screenID}); err != nil {
		t.Fatalf("SetRevokedScreens: %v", err)
	}

	// A generation-8 resolver that was still in flight when 9 landed. Its list
	// is the OLD truth; applying it would un-revoke a screen generation 9
	// revoked.
	if err := srv.SetRevokedScreens(8, nil); err != nil {
		t.Fatalf("SetRevokedScreens: %v", err)
	}

	resp, raw := doProgram(t, srv, token, []string{"image", "video"})
	assertTypedError(t, resp, raw, "CHANNEL_TOKEN_REVOKED")

	// A SAME-generation write is admitted (only a strictly older one is
	// dropped), so a deliberate re-install of generation 9 is possible — and
	// carries no apply-time side effect to re-run (REL-070): the set is
	// replaced with an equal one and no screen is NEWLY revoked, so no session
	// is dropped a second time.
	if err := srv.SetRevokedScreens(9, []string{screenID}); err != nil {
		t.Fatalf("SetRevokedScreens: %v", err)
	}
	resp, raw = doProgram(t, srv, token, []string{"image", "video"})
	assertTypedError(t, resp, raw, "CHANNEL_TOKEN_REVOKED")
}
