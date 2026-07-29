package enroll

import (
	"crypto/ed25519"
	"crypto/rand"
	"reflect"
	"testing"
)

// TestActiveRelayIDsListsTheUnrevokedFleet pins what "active" means: every relay
// this feeder enrolled, minus the ones whose most recent certificate is revoked.
//
// Both halves are load-bearing for the content retention sweep, in opposite
// directions. Including an enrolled relay that is merely offline is what keeps
// the sweep from reclaiming content that relay's screens are still playing.
// Excluding a revoked one is what keeps a single revocation from holding the
// fleet's generation floor down forever, so a box that ever revoked a relay would
// never reclaim anything again.
func TestActiveRelayIDsListsTheUnrevokedFleet(t *testing.T) {
	srv, _, _, _ := newTestServer(t)

	live := "01J8ZK0000000000000000LIVE"
	gone := "01J8ZK0000000000000000GONE"
	for _, relayID := range []string{live, gone} {
		pub, _, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatalf("generate key: %v", err)
		}
		srv.mu.Lock()
		srv.relayKeys[relayID] = pub
		srv.recordIssuance(relayID, "serial-"+relayID, pub, 0)
		srv.mu.Unlock()
	}

	if got, want := srv.ActiveRelayIDs(), []string{live, gone}; !reflect.DeepEqual(sorted(got), sorted(want)) {
		t.Fatalf("ActiveRelayIDs = %v, want both enrolled relays %v", got, want)
	}

	if !srv.Revoke(gone, "serial-"+gone) {
		t.Fatal("Revoke did not find the issuance it was given")
	}
	if got, want := srv.ActiveRelayIDs(), []string{live}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ActiveRelayIDs after a revocation = %v, want %v — a revoked relay must not hold the fleet's generation floor down forever", got, want)
	}
}

// TestActiveRelayIDsIsEmptyOnAFreshFeeder pins the vacuous case: a feeder nothing
// has enrolled reports no active relay, which is what lets a box with no relay
// reclaim content at all.
func TestActiveRelayIDsIsEmptyOnAFreshFeeder(t *testing.T) {
	srv, _, _, _ := newTestServer(t)
	if got := srv.ActiveRelayIDs(); len(got) != 0 {
		t.Fatalf("ActiveRelayIDs on a fresh feeder = %v, want none", got)
	}
}

func sorted(in []string) []string {
	out := append([]string(nil), in...)
	for i := range out {
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}
