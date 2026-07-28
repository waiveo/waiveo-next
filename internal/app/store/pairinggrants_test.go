package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/maaxton/waiveo-next/internal/app/store"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// boundGrant builds a screen-bound pairing-grant record (REL-121a) against the
// seeded demo screen row, issued at issuedAt with the ordinary 900s ttl.
func boundGrant(grantID, screenID string, issuedAt int64) wire.PairingGrant {
	return wire.PairingGrant{
		GrantID:                grantID,
		Purpose:                "pairing",
		ResultingPrincipalKind: "screen",
		ScreenID:               screenID,
		TTL:                    900,
		RedemptionMode:         "one-time",
		IssuedAt:               issuedAt,
	}
}

// TestAddPairingGrantRidesDesiredStateAndBumpsGeneration: a minted grant is a
// desired-state write — the generation advances (so live relays are nudged to
// re-pull, REL-057) and the very next DesiredState read carries the grant
// wire-shaped for the `pairing_grants` section (REL-067).
func TestAddPairingGrantRidesDesiredStateAndBumpsGeneration(t *testing.T) {
	s := openMem(t)
	ctx := context.Background()
	if err := s.SeedDemo(ctx, seedAssetRef); err != nil {
		t.Fatalf("SeedDemo: %v", err)
	}

	before := gen(t, s)
	g := boundGrant("grant-0123456789abcdef0123456789abcdef", store.SeedScreenID, 1752537000000)
	if err := s.AddPairingGrant(ctx, g, "01J8Z2Q1M8H8N4T0V1W2X3Y4Z5", "api"); err != nil {
		t.Fatalf("AddPairingGrant: %v", err)
	}
	if after := gen(t, s); after != before+1 {
		t.Fatalf("generation = %d after mint, want %d (a grant must ride a NEW generation)", after, before+1)
	}

	ds, err := s.DesiredState(ctx)
	if err != nil {
		t.Fatalf("DesiredState: %v", err)
	}
	if len(ds.PairingGrants) != 1 {
		t.Fatalf("DesiredState carries %d pairing grant(s), want 1", len(ds.PairingGrants))
	}
	if got := ds.PairingGrants[0]; got != g {
		t.Fatalf("stored grant round-tripped as %+v, want %+v", got, g)
	}
}

// TestAddPairingGrantRefusesUnknownScreen: the REL-121a binding must join a
// real screen row at mint time — a grant naming no row is refused with the
// typed sentinel (the api layer's 404), and NOTHING is persisted: the
// generation does not advance and no grant rides a later read.
func TestAddPairingGrantRefusesUnknownScreen(t *testing.T) {
	s := openMem(t)
	ctx := context.Background()
	if err := s.SeedDemo(ctx, seedAssetRef); err != nil {
		t.Fatalf("SeedDemo: %v", err)
	}

	before := gen(t, s)
	g := boundGrant("grant-ffffffffffffffffffffffffffffffff", "01J8Z9N0SUCHSCREENR0WXXXXX", 1752537000000)
	err := s.AddPairingGrant(ctx, g, "", "api")
	if !errors.Is(err, store.ErrPairingGrantScreenUnknown) {
		t.Fatalf("AddPairingGrant(unknown screen) = %v, want ErrPairingGrantScreenUnknown", err)
	}
	if after := gen(t, s); after != before {
		t.Fatalf("generation advanced (%d -> %d) on a REFUSED mint", before, after)
	}
	ds, err := s.DesiredState(ctx)
	if err != nil {
		t.Fatalf("DesiredState: %v", err)
	}
	if len(ds.PairingGrants) != 0 {
		t.Fatalf("a refused mint persisted %d grant(s)", len(ds.PairingGrants))
	}
}

// TestAddPairingGrantRefusesUnboundGrant: a store-minted grant is ALWAYS
// screen-bound (REL-121a) — the unbound REL-121 baseline shape is wire-legal
// for a peer but never something this app authors.
func TestAddPairingGrantRefusesUnboundGrant(t *testing.T) {
	s := openMem(t)
	ctx := context.Background()
	if err := s.SeedDemo(ctx, seedAssetRef); err != nil {
		t.Fatalf("SeedDemo: %v", err)
	}
	g := boundGrant("grant-0123456789abcdef0123456789abcdef", "", 1752537000000)
	if err := s.AddPairingGrant(ctx, g, "", "api"); err == nil {
		t.Fatal("AddPairingGrant accepted an unbound grant, want refusal")
	}
}

// TestAddPairingGrantRetiresExpiredRows: minting is the moment the table
// grows, so it is also the moment already-expired rows are swept — a store
// that pairs screens for months never accumulates dead grant rows.
func TestAddPairingGrantRetiresExpiredRows(t *testing.T) {
	s := openMem(t)
	ctx := context.Background()
	if err := s.SeedDemo(ctx, seedAssetRef); err != nil {
		t.Fatalf("SeedDemo: %v", err)
	}

	// An old grant, expired long before "now" (the store's clock is the real
	// wall clock here; issued_at 0 with a 900s ttl expired in 1970).
	old := boundGrant("grant-00000000000000000000000000000000", store.SeedScreenID, 0)
	if err := s.AddPairingGrant(ctx, old, "", "api"); err != nil {
		t.Fatalf("AddPairingGrant(old): %v", err)
	}
	fresh := boundGrant("grant-11111111111111111111111111111111", store.SeedScreenID, 1752537000000)
	if err := s.AddPairingGrant(ctx, fresh, "", "api"); err != nil {
		t.Fatalf("AddPairingGrant(fresh): %v", err)
	}

	ds, err := s.DesiredState(ctx)
	if err != nil {
		t.Fatalf("DesiredState: %v", err)
	}
	if len(ds.PairingGrants) != 1 || ds.PairingGrants[0].GrantID != fresh.GrantID {
		t.Fatalf("after the sweep the store holds %+v, want only the fresh grant", ds.PairingGrants)
	}
}
