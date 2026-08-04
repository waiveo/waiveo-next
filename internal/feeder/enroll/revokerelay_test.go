package enroll

import (
	"testing"
)

// TestRevokeRelayReachesEveryIssuance is the difference between the
// operator-level act and the per-serial one.
//
// A relay that has re-enrolled holds more than one issuance. Revoking the serial
// an operator happened to name leaves the others valid, the relay reconnects on
// the next one, and the revocation reads as having silently failed — which is
// indistinguishable, from the operator's side, from the control not working at
// all.
func TestRevokeRelayReachesEveryIssuance(t *testing.T) {
	s := &Server{issuances: map[string][]issuance{}}
	const relayID = "relay-0f1e2d3c4b5a69788796a5b4c3d2e1f0"
	s.issuances[relayID] = []issuance{{serial: "first"}, {serial: "second"}, {serial: "third"}}
	// A different relay, to prove the act is scoped to its subject.
	const other = "relay-1a2b3c4d5e6f78899a8b7c6d5e4f3021"
	s.issuances[other] = []issuance{{serial: "other-first"}}

	if n := s.RevokeRelay(relayID); n != 3 {
		t.Fatalf("RevokeRelay marked %d issuance(s), want all 3 — one serial left valid is a relay that reconnects", n)
	}
	for _, serial := range []string{"first", "second", "third"} {
		if !s.IsRevoked(relayID, serial) {
			t.Errorf("serial %q survived a relay-level revocation", serial)
		}
	}
	if s.IsRevoked(other, "other-first") {
		t.Error("revoking one relay revoked another's certificate")
	}
}

// TestRevokeRelayIsIdempotentAndCountsOnlyWhatItChanged: a second call marks
// nothing new and says so, so a caller cannot read "2 revoked" twice and
// believe two acts occurred.
func TestRevokeRelayIsIdempotentAndCountsOnlyWhatItChanged(t *testing.T) {
	s := &Server{issuances: map[string][]issuance{}}
	const relayID = "relay-0f1e2d3c4b5a69788796a5b4c3d2e1f0"
	s.issuances[relayID] = []issuance{{serial: "first"}, {serial: "second"}}

	if n := s.RevokeRelay(relayID); n != 2 {
		t.Fatalf("first call marked %d, want 2", n)
	}
	if n := s.RevokeRelay(relayID); n != 0 {
		t.Errorf("second call marked %d, want 0 — the count is what it CHANGED, not what is revoked", n)
	}
}

// TestRevokeRelayOnAnUnknownRelayIsZeroNotAFailure: a relay that never enrolled
// has nothing to revoke, and the api/1 operation records the decision anyway —
// so it is refused if it later tries.
func TestRevokeRelayOnAnUnknownRelayIsZeroNotAFailure(t *testing.T) {
	s := &Server{issuances: map[string][]issuance{}}
	if n := s.RevokeRelay("relay-never-enrolled"); n != 0 {
		t.Errorf("RevokeRelay on an unknown relay marked %d, want 0", n)
	}
}
