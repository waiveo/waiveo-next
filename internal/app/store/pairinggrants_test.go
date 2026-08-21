package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/maaxton/waiveo-next/internal/app/store"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// testRelayID is the enrolled relay identity every fixture grant here binds to
// (REL-121b) — the store refuses an unbound one-time grant outright.
const testRelayID = "01J8ZRELAYAAAAAAAAAAAAAAA1"

// boundGrant builds a screen-bound (REL-121a), relay-bound (REL-121b)
// pairing-grant record against the seeded demo screen row, issued at issuedAt
// with the ordinary 900s ttl.
func boundGrant(grantID, screenID string, issuedAt int64) wire.PairingGrant {
	return wire.PairingGrant{
		GrantID:                grantID,
		Purpose:                "pairing",
		ResultingPrincipalKind: "screen",
		ScreenID:               screenID,
		RelayID:                testRelayID,
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
	// The scope node is the seeded screen ROW's own placement (seed.go), which
	// the mint re-checks in-transaction against the row — a caller-supplied
	// node the row does not sit at is refused (see the moved-screen test).
	if err := s.AddPairingGrant(ctx, g, "01J8Z4DEM0SCREENF1RSTPH0TN", "api"); err != nil {
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

// TestAddPairingGrantRefusesMovedScreen: the mint re-reads the screen row's
// placement inside its own transaction and refuses when it no longer matches
// the node the caller AUTHORIZED against — the authorize-then-mint race a
// concurrent screen move opens. Nothing is persisted on refusal: no grant, no
// generation bump, so no audit record can be filed under a node the row has
// left on authority the caller may not hold at the row's new placement.
func TestAddPairingGrantRefusesMovedScreen(t *testing.T) {
	s := openMem(t)
	ctx := context.Background()
	if err := s.SeedDemo(ctx, seedAssetRef); err != nil {
		t.Fatalf("SeedDemo: %v", err)
	}

	before := gen(t, s)
	g := boundGrant("grant-0123456789abcdef0123456789abcdef", store.SeedScreenID, 1752537000000)
	// The site node — a real node, but NOT the placement the seeded screen row
	// sits at, exactly what a caller holds after the row moved under it.
	err := s.AddPairingGrant(ctx, g, "01J8Z2Q1M8H8N4T0V1W2X3Y4Z5", "api")
	if !errors.Is(err, store.ErrPairingGrantScreenMoved) {
		t.Fatalf("AddPairingGrant(stale scope node) = %v, want ErrPairingGrantScreenMoved", err)
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
	if err := s.AddPairingGrant(ctx, old, "01J8Z4DEM0SCREENF1RSTPH0TN", "api"); err != nil {
		t.Fatalf("AddPairingGrant(old): %v", err)
	}
	fresh := boundGrant("grant-11111111111111111111111111111111", store.SeedScreenID, 1752537000000)
	if err := s.AddPairingGrant(ctx, fresh, "01J8Z4DEM0SCREENF1RSTPH0TN", "api"); err != nil {
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

// TestAddPairingGrantRefusesAnUnboundOneTimeGrant is the storage-boundary half
// of REL-121c. Every grant written here rides `pairing_grants` inside ONE
// signed snapshot to EVERY relay enrolled to the site (REL-067), and REL-122
// makes each of them able to redeem it for its whole ttl with no app peer
// reachable — so an unbound one-time grant is redeemable once per relay, and
// (being screen-bound, REL-121a) every one of those redemptions resolves to the
// same screen row. The refusal lives here as well as in the api handler so no
// future caller can reach desired state by another route.
//
// Guard-disabled check: removing the relay-binding refusal in AddPairingGrant
// makes the unbound grant persist and ride DesiredState, failing both
// assertions below.
func TestAddPairingGrantRefusesAnUnboundOneTimeGrant(t *testing.T) {
	s := openMem(t)
	ctx := context.Background()
	if err := s.SeedDemo(ctx, seedAssetRef); err != nil {
		t.Fatalf("SeedDemo: %v", err)
	}

	before := gen(t, s)
	g := boundGrant("grant-unbound-00000000000000000000000000", store.SeedScreenID, 1752537000000)
	g.RelayID = ""

	if err := s.AddPairingGrant(ctx, g, "01J8Z4DEM0SCREENF1RSTPH0TN", "api"); err == nil {
		t.Fatal("AddPairingGrant accepted an unbound one-time grant (REL-121b/REL-121c)")
	}
	if after := gen(t, s); after != before {
		t.Errorf("generation moved %d -> %d on a refused mint", before, after)
	}
	ds, err := s.DesiredState(ctx)
	if err != nil {
		t.Fatalf("DesiredState: %v", err)
	}
	if len(ds.PairingGrants) != 0 {
		t.Fatalf("desired state carries %+v — an unbound one-time grant reached every relay", ds.PairingGrants)
	}
}

// TestRetirePairingGrantOnlyForTheRelayItIsBoundTo is REL-124b's own oracle at
// the storage layer: a redemption report retires a grant, so a relay able to
// retire a grant bound to a DIFFERENT relay could cancel a pairing in progress
// at its sibling with one frame. The delete is conditioned on the reporting
// connection's own authenticated identity, and a grant naming another relay is
// refused with nothing written.
//
// It also pins the two non-refusal shapes REL-124b requires: an unknown grant
// is an idempotent no-op (a relay re-sending an owed report, or reporting one
// already retired, is doing what REL-124d requires of it), and a legitimate
// retirement bumps the generation so the spent grant stops riding snapshots.
//
// Guard-disabled check: replacing the bound-relay comparison with an
// unconditional delete makes the foreign-relay case retire relay A's grant and
// fails the "still on record" assertion.
func TestRetirePairingGrantOnlyForTheRelayItIsBoundTo(t *testing.T) {
	const relayA, relayB = "01J8ZRELAYAAAAAAAAAAAAAAA1", "01J8ZRELAYBBBBBBBBBBBBBBB2"

	s := openMem(t)
	ctx := context.Background()
	if err := s.SeedDemo(ctx, seedAssetRef); err != nil {
		t.Fatalf("SeedDemo: %v", err)
	}

	g := boundGrant("grant-retire-0000000000000000000000000000", store.SeedScreenID, 1752537000000)
	g.RelayID = relayA
	if err := s.AddPairingGrant(ctx, g, "01J8Z4DEM0SCREENF1RSTPH0TN", "api"); err != nil {
		t.Fatalf("AddPairingGrant: %v", err)
	}

	// Relay B reports a redemption of relay A's grant: refused, nothing written.
	beforeForeign := gen(t, s)
	retired, err := s.RetirePairingGrant(ctx, g.GrantID, relayB)
	if !errors.Is(err, store.ErrPairingGrantBoundElsewhere) {
		t.Fatalf("RetirePairingGrant(relay B) = (%v, %v), want ErrPairingGrantBoundElsewhere", retired, err)
	}
	if after := gen(t, s); after != beforeForeign {
		t.Errorf("generation moved %d -> %d on a refused report", beforeForeign, after)
	}
	ds, err := s.DesiredState(ctx)
	if err != nil {
		t.Fatalf("DesiredState: %v", err)
	}
	if len(ds.PairingGrants) != 1 || ds.PairingGrants[0].GrantID != g.GrantID {
		t.Fatalf("relay A's grant is no longer on record after relay B reported it: %+v", ds.PairingGrants)
	}

	// A grant this store does not hold: an idempotent no-op, never a refusal.
	beforeUnknown := gen(t, s)
	retired, err = s.RetirePairingGrant(ctx, "grant-never-minted-000000000000", relayA)
	if err != nil || retired {
		t.Fatalf("RetirePairingGrant(unknown) = (%v, %v), want (false, nil) — REL-124b makes it a no-op", retired, err)
	}
	if after := gen(t, s); after != beforeUnknown {
		t.Errorf("generation moved %d -> %d on a no-op report", beforeUnknown, after)
	}

	// The bound relay's own report retires it and advances the generation.
	beforeRetire := gen(t, s)
	retired, err = s.RetirePairingGrant(ctx, g.GrantID, relayA)
	if err != nil || !retired {
		t.Fatalf("RetirePairingGrant(relay A) = (%v, %v), want (true, nil)", retired, err)
	}
	if after := gen(t, s); after != beforeRetire+1 {
		t.Errorf("generation = %d after retirement, want %d (the spent grant must stop riding snapshots)", after, beforeRetire+1)
	}
	ds, err = s.DesiredState(ctx)
	if err != nil {
		t.Fatalf("DesiredState: %v", err)
	}
	if len(ds.PairingGrants) != 0 {
		t.Fatalf("desired state still carries %+v after retirement", ds.PairingGrants)
	}

	// Re-reporting the now-retired grant is a no-op, not a refusal (REL-124d's
	// re-send rule makes a duplicate report normal traffic).
	retired, err = s.RetirePairingGrant(ctx, g.GrantID, relayA)
	if err != nil || retired {
		t.Fatalf("re-report of a retired grant = (%v, %v), want (false, nil)", retired, err)
	}
}

// TestRetirePairingGrantMatchesTheBOUNDRelayAndNotAPosition closes the hole the
// case above leaves open.
//
// That case holds ONE grant, bound to relay A, and proves relay B is refused and
// relay A succeeds. Both assertions survive an implementation that never
// compares identities at all — one that returned the only grant on record, or
// the first row of a scan, would pass the whole corpus. The property being
// claimed is "a relay may retire the grant BOUND TO IT", and a fixture with one
// grant cannot distinguish that from "a relay may retire THE grant".
//
// So: two grants, bound to different relays, and the relay whose grant is NOT
// first redeems its own. A positional implementation now retires the wrong row —
// and retiring the wrong row is not a cosmetic failure. A pairing grant is a
// one-time credential a screen redeems to become a principal; consuming another
// relay's grant would burn a credential that screen still needs while leaving
// the reporting relay's own grant riding desired state as if unspent.
func TestRetirePairingGrantMatchesTheBOUNDRelayAndNotAPosition(t *testing.T) {
	const relayA, relayB = "01J8ZRELAYAAAAAAAAAAAAAAA1", "01J8ZRELAYBBBBBBBBBBBBBBB2"

	s := openMem(t)
	ctx := context.Background()
	if err := s.SeedDemo(ctx, seedAssetRef); err != nil {
		t.Fatalf("SeedDemo: %v", err)
	}

	// Ordered so relay B's grant is the SECOND one on record: the first-row
	// implementation this case exists to catch would reach for relay A's.
	// Issued NOW, not at the fixture's usual fixed instant. AddPairingGrant
	// deletes rows past their ttl before inserting, and the shared fixture's
	// issued_at is long past with a 900s ttl — harmless while a case holds ONE
	// grant, but adding a second sweeps the first as expired and the test then
	// proves nothing. The single-grant case above never had to notice.
	issued := time.Now().UnixMilli()
	first := boundGrant("grant-bound-to-relay-a-000000000000000000", store.SeedScreenID, issued)
	first.RelayID = relayA
	if err := s.AddPairingGrant(ctx, first, "01J8Z4DEM0SCREENF1RSTPH0TN", "api"); err != nil {
		t.Fatalf("AddPairingGrant(A): %v", err)
	}
	second := boundGrant("grant-bound-to-relay-b-000000000000000000", store.SeedScreenID, issued+1)
	second.RelayID = relayB
	if err := s.AddPairingGrant(ctx, second, "01J8Z4DEM0SCREENF1RSTPH0TN", "api"); err != nil {
		t.Fatalf("AddPairingGrant(B): %v", err)
	}

	// Relay B retires ITS OWN grant, which is not the first on record.
	retired, err := s.RetirePairingGrant(ctx, second.GrantID, relayB)
	if err != nil || !retired {
		t.Fatalf("RetirePairingGrant(B's own grant, as B) = (%v, %v), want (true, nil)", retired, err)
	}

	// And relay A's grant is untouched — the assertion a positional match fails.
	ds, err := s.DesiredState(ctx)
	if err != nil {
		t.Fatalf("DesiredState: %v", err)
	}
	if len(ds.PairingGrants) != 1 {
		t.Fatalf("desired state carries %d grant(s) after one retirement, want 1: %+v",
			len(ds.PairingGrants), ds.PairingGrants)
	}
	if ds.PairingGrants[0].GrantID != first.GrantID {
		t.Fatalf("the surviving grant is %q, want relay A's %q — relay B's report retired the "+
			"WRONG grant, burning a credential its screen still needs while leaving B's own "+
			"grant riding desired state as if unspent",
			ds.PairingGrants[0].GrantID, first.GrantID)
	}
	if ds.PairingGrants[0].RelayID != relayA {
		t.Errorf("the surviving grant is bound to %q, want relay A", ds.PairingGrants[0].RelayID)
	}
}

// TestRetiringOneGrantDoesNotBurnTheRelaysOTHERGrants closes a hole neither of
// the cases above can see.
//
// Both of them hold at most one grant per relay, so a retirement scoped to the
// RELAY rather than to the GRANT — `DELETE ... WHERE relay_id = ?` in place of
// `WHERE grant_id = ?` — removes exactly the row it should and passes. I found
// that empirically by making the mutation: the single-grant case and the
// two-relay case both stayed green.
//
// The bug it would hide is not subtle. A pairing grant is a one-time credential
// a screen redeems to become a principal, and a relay serves many screens. One
// screen completing its pairing would silently destroy every other pending
// grant on that relay, and each of those screens would fail to pair with no
// record of why — the grant it was told to redeem simply no longer exists.
func TestRetiringOneGrantDoesNotBurnTheRelaysOTHERGrants(t *testing.T) {
	const relay = "01J8ZRELAYBBBBBBBBBBBBBBB2"
	const secondScreen = "01J8Z9DEM0SCREENR0WSEC0ND2"

	s := openMem(t)
	ctx := context.Background()
	if err := s.SeedDemo(ctx, seedAssetRef); err != nil {
		t.Fatalf("SeedDemo: %v", err)
	}
	// A second screen at the seeded screen's own node, so both grants are
	// legitimately mintable and the fixture describes one relay serving two
	// screens — the ordinary case, not a contrived one.
	if _, err := s.Create(ctx, store.KindScreen, mustJSON(t, map[string]any{
		"id": secondScreen, "scope_node": "01J8Z4DEM0SCREENF1RSTPH0TN", "name": "Second Screen",
	})); err != nil {
		var verr *store.ValidationError
		if errors.As(err, &verr) {
			t.Fatalf("create the second screen: %v", verr.Errors)
		}
		t.Fatalf("create the second screen: %v", err)
	}

	issued := time.Now().UnixMilli()
	keep := boundGrant("grant-for-the-other-screen-0000000000000", secondScreen, issued)
	keep.RelayID = relay
	if err := s.AddPairingGrant(ctx, keep, "01J8Z4DEM0SCREENF1RSTPH0TN", "api"); err != nil {
		t.Fatalf("AddPairingGrant(keep): %v", err)
	}
	spend := boundGrant("grant-about-to-be-redeemed-000000000000", store.SeedScreenID, issued+1)
	spend.RelayID = relay
	if err := s.AddPairingGrant(ctx, spend, "01J8Z4DEM0SCREENF1RSTPH0TN", "api"); err != nil {
		t.Fatalf("AddPairingGrant(spend): %v", err)
	}

	retired, err := s.RetirePairingGrant(ctx, spend.GrantID, relay)
	if err != nil || !retired {
		t.Fatalf("RetirePairingGrant(spend) = (%v, %v), want (true, nil)", retired, err)
	}

	ds, err := s.DesiredState(ctx)
	if err != nil {
		t.Fatalf("DesiredState: %v", err)
	}
	if len(ds.PairingGrants) != 1 || ds.PairingGrants[0].GrantID != keep.GrantID {
		t.Fatalf("after retiring ONE of this relay's two grants, desired state carries %+v — want only "+
			"%q. Retiring by relay rather than by grant destroys every other screen's pending "+
			"credential, and each of them fails to pair with no record of why",
			ds.PairingGrants, keep.GrantID)
	}
}
