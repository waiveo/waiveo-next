package playerserver

import (
	"testing"

	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// TestSetPairingGrantsSupersedesBootSet is defect B's own oracle (relay/1
// REL-122): after NewServer boots with grant A as the redeemable set,
// SetPairingGrants at a higher generation with grant B supersedes it wholesale
// — A is no longer redeemable, B is. Before SetPairingGrants existed, there
// was no setter at all: NewServer's boot-time grant set was frozen for the
// rest of the process's life, so even a live, fully-recovered desired-state
// pull could never refresh what a screen could pair against.
func TestSetPairingGrantsSupersedesBootSet(t *testing.T) {
	grantA := testGrant()
	srv, _, _ := newTestServer(t, grantA)

	grantB := testGrant()
	grantB.GrantID = "grant-superseding-0123456789"
	srv.SetPairingGrants(1, []wire.PairingGrant{grantB})

	respA, rawA := doPair(t, srv, PairingRequest{
		HardwareID:    "hw-superseded-0001",
		GrantSelector: grantA.GrantID,
		Capabilities:  Capabilities{ContentTypes: []string{"image"}, PlayerVersion: "1.0.0"},
	})
	assertTypedError(t, respA, rawA, "PAIRING_CODE_INVALID")

	respB, rawB := doPair(t, srv, PairingRequest{
		HardwareID:    "hw-superseding-0001",
		GrantSelector: grantB.GrantID,
		Capabilities:  Capabilities{ContentTypes: []string{"image"}, PlayerVersion: "1.0.0"},
	})
	if respB.StatusCode != 200 {
		t.Fatalf("redemption against the superseding generation's own grant: status = %d, want 200; body = %v", respB.StatusCode, rawB)
	}
}

// TestSetPairingGrantsIgnoresStaleGeneration mirrors SetProgram's own
// REL-052/056 fencing test posture: a SetPairingGrants call at a strictly
// older generation than the last one applied is dropped, never reverting a
// newer generation's grant set to a superseded one's.
func TestSetPairingGrantsIgnoresStaleGeneration(t *testing.T) {
	grantOld := testGrant()
	srv, _, _ := newTestServer(t) // boot with no grants at all

	grantNew := testGrant()
	grantNew.GrantID = "grant-current-0123456789012"
	srv.SetPairingGrants(5, []wire.PairingGrant{grantNew})

	// A stale, lower-generation write naming grantOld must be dropped.
	srv.SetPairingGrants(3, []wire.PairingGrant{grantOld})

	respOld, rawOld := doPair(t, srv, PairingRequest{
		HardwareID:    "hw-stale-0001",
		GrantSelector: grantOld.GrantID,
		Capabilities:  Capabilities{ContentTypes: []string{"image"}, PlayerVersion: "1.0.0"},
	})
	assertTypedError(t, respOld, rawOld, "PAIRING_CODE_INVALID")

	respNew, rawNew := doPair(t, srv, PairingRequest{
		HardwareID:    "hw-current-0001",
		GrantSelector: grantNew.GrantID,
		Capabilities:  Capabilities{ContentTypes: []string{"image"}, PlayerVersion: "1.0.0"},
	})
	if respNew.StatusCode != 200 {
		t.Fatalf("redemption against the current generation's grant after a stale write: status = %d, want 200; body = %v", respNew.StatusCode, rawNew)
	}
}

// TestSetPairingGrantsPreservesRedeemedStateAcrossSwap confirms
// SetPairingGrants's own documented invariant: replacing the grant set does
// NOT reset redeemedGrants — a one-time grant already redeemed stays
// redeemed even when a later generation's grant set still carries that same
// grant_id (an app peer that hands back the identical pairing_grants entry
// across successive generations, for instance).
func TestSetPairingGrantsPreservesRedeemedStateAcrossSwap(t *testing.T) {
	grant := testGrant()
	srv, _, _ := newTestServer(t, grant)

	req := PairingRequest{
		HardwareID:    "hw-preserved-0001",
		GrantSelector: grant.GrantID,
		Capabilities:  Capabilities{ContentTypes: []string{"image"}, PlayerVersion: "1.0.0"},
	}
	if resp, raw := doPair(t, srv, req); resp.StatusCode != 200 {
		t.Fatalf("first redemption: status = %d, want 200; body = %v", resp.StatusCode, raw)
	}

	// A newer generation's grant set still carries the SAME grant_id.
	srv.SetPairingGrants(1, []wire.PairingGrant{grant})

	resp, raw := doPair(t, srv, req)
	assertTypedError(t, resp, raw, "PAIRING_CODE_INVALID")
}
