package playerserver

import "testing"

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
func TestSetRevokedScreensReplacesRatherThanAccumulates(t *testing.T) {
	srv, _, token := programTestServer(t)

	screenID, _, ok := srv.LookupChannelToken(token)
	if !ok {
		t.Fatalf("freshly redeemed token %q is not known to the server", token)
	}

	srv.SetRevokedScreens(2, []string{screenID})
	resp, raw := doProgram(t, srv, token, []string{"image", "video"})
	assertTypedError(t, resp, raw, "CHANNEL_TOKEN_REVOKED")

	// Generation 3 no longer names the screen. The relay's last-synced copy IS
	// this list, so there is nothing left to enforce against it.
	srv.SetRevokedScreens(3, []string{"01J8Z3K4N5P6Q7R8S9T0V1W2ZZ"})

	resp, raw = doProgram(t, srv, token, []string{"image", "video"})
	if resp.StatusCode != 200 {
		t.Fatalf("program pull after the screen left `revoked` = %d (body %v), want 200 — a set-replace must un-revoke, not accumulate", resp.StatusCode, raw)
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

	srv.SetRevokedScreens(9, []string{screenID})

	// A generation-8 resolver that was still in flight when 9 landed. Its list
	// is the OLD truth; applying it would un-revoke a screen generation 9
	// revoked.
	srv.SetRevokedScreens(8, nil)

	resp, raw := doProgram(t, srv, token, []string{"image", "video"})
	assertTypedError(t, resp, raw, "CHANNEL_TOKEN_REVOKED")

	// A SAME-generation write is admitted (only a strictly older one is
	// dropped), so re-applying generation 9 is idempotent rather than a no-op
	// that could not repair a partial install (REL-070).
	srv.SetRevokedScreens(9, []string{screenID})
	resp, raw = doProgram(t, srv, token, []string{"image", "video"})
	assertTypedError(t, resp, raw, "CHANNEL_TOKEN_REVOKED")
}
