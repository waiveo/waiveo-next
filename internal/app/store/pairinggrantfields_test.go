package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/maaxton/waiveo-next/internal/app/store"
)

// Six refusals on this surface survive deletion with the whole tree green. The
// existing suite drives the screen binding, the relay binding, the moved-row
// race and the retirement attribution — every field check beside them is held by
// nothing.
//
// They are all one assertion: a grant refused here must not reach the wire. Each
// test therefore checks the refusal AND that the generation did not move and
// desired state carries nothing, because a grant that rides `pairing_grants`
// reaches EVERY relay enrolled to the site.

// TestAddPairingGrantRefusesAModeItDoesNotMint is the one that matters most, and
// its own guard's comment says why: this check exists because guarding only the
// relay binding "left the hole open by the mode field".
//
// Nothing else in this repo validates RedemptionMode. So without this check a
// caller supplying any other value gets a grant that is delivered to every relay
// AND skips consumption marking on the relay side — redeemable repeatedly at
// every one of them. That is strictly worse than the unbound-grant defect the
// binding check closes, and it is reachable by the same route.
//
// The fix landed with no test. This is that test.
func TestAddPairingGrantRefusesAModeItDoesNotMint(t *testing.T) {
	for _, mode := range []string{
		"",          // the zero value: a caller who never set the field
		"multi-use", // a plausible second mode
		"reusable",  // another
		"One-Time",  // right word, wrong case — the comparison is exact
		"one-time ", // right word, trailing space
		" one-time", // leading space
	} {
		t.Run("mode="+mode, func(t *testing.T) {
			s := openMem(t)
			ctx := context.Background()
			if err := s.SeedDemo(ctx, seedAssetRef); err != nil {
				t.Fatalf("SeedDemo: %v", err)
			}
			before := gen(t, s)

			g := boundGrant("grant-mode-000000000000000000000000000000", store.SeedScreenID, 1752537000000)
			g.RedemptionMode = mode

			if err := s.AddPairingGrant(ctx, g, "01J8Z4DEM0SCREENF1RSTPH0TN", "api"); err == nil {
				t.Fatalf("AddPairingGrant accepted redemption_mode %q — this app peer mints one-time grants only "+
					"(REL-121c), and a grant of any other mode rides pairing_grants to every relay enrolled to the "+
					"site while skipping consumption marking at each of them", mode)
			}
			assertNothingMinted(t, s, before)
		})
	}

	// The control: the mode this peer DOES mint is still accepted. Without it,
	// every case above is satisfied by a store that refuses every grant.
	s := openMem(t)
	ctx := context.Background()
	if err := s.SeedDemo(ctx, seedAssetRef); err != nil {
		t.Fatalf("SeedDemo: %v", err)
	}
	g := boundGrant("grant-mode-ok-0000000000000000000000000000", store.SeedScreenID, 1752537000000)
	if err := s.AddPairingGrant(ctx, g, "01J8Z4DEM0SCREENF1RSTPH0TN", "api"); err != nil {
		t.Fatalf("a one-time grant was refused: %v", err)
	}
}

// TestAddPairingGrantRefusesAGrantMissingItsOwnIdentity covers the two identity
// fields, and the screen case had to be written twice.
//
// An empty grant_id is the primary key of the row this mint writes: without the
// check it is stored under "", and the empty string becomes a selector any
// caller can name.
//
// THE SCREEN CASE CANNOT BE ASSERTED BY "IT FAILED". My first version checked
// only that an error came back, and the mutant survived: an empty screen_id
// matches no row, so the in-transaction join refuses it as an UNKNOWN SCREEN
// whether or not the field check exists. Two producers, one outcome — the same
// trap as a code with two emitters, arriving this time from DOWNSTREAM rather
// than upstream.
//
// The distinction is worth keeping rather than deferring to the join, because
// the two say different things. ErrPairingGrantScreenUnknown is the api layer's
// 404: it tells a caller the screen they named does not exist. A caller who
// named NO screen has not made that mistake — REL-121a's requirement is that a
// store-minted grant is screen-bound at all, and reporting it as a lookup miss
// invites someone to go looking for a row that was never referenced.
func TestAddPairingGrantRefusesAGrantMissingItsOwnIdentity(t *testing.T) {
	s := openMem(t)
	ctx := context.Background()
	if err := s.SeedDemo(ctx, seedAssetRef); err != nil {
		t.Fatalf("SeedDemo: %v", err)
	}
	before := gen(t, s)

	noID := boundGrant("grant-identity-000000000000000000000000000", store.SeedScreenID, 1752537000000)
	noID.GrantID = ""
	if err := s.AddPairingGrant(ctx, noID, "01J8Z4DEM0SCREENF1RSTPH0TN", "api"); err == nil {
		t.Error("AddPairingGrant accepted a grant with no grant_id — that is this row's primary key, so an empty " +
			"one makes the empty string a selector")
	}
	assertNothingMinted(t, s, before)

	noScreen := boundGrant("grant-identity-111111111111111111111111111", store.SeedScreenID, 1752537000000)
	noScreen.ScreenID = ""
	err := s.AddPairingGrant(ctx, noScreen, "01J8Z4DEM0SCREENF1RSTPH0TN", "api")
	if err == nil {
		t.Fatal("AddPairingGrant accepted a grant with no screen_id (REL-121a)")
	}
	if errors.Is(err, store.ErrPairingGrantScreenUnknown) {
		t.Errorf("a grant naming NO screen was refused as naming an UNKNOWN one (%v) — that error is the api "+
			"layer's 404 and sends a caller looking for a row they never referenced; the fault is that REL-121a "+
			"requires a store-minted grant to be screen-bound at all", err)
	}
	assertNothingMinted(t, s, before)

	// The control: a grant naming a screen that genuinely does not exist DOES
	// get the lookup miss, so the two remain distinguishable rather than one
	// having swallowed the other.
	ghost := boundGrant("grant-identity-222222222222222222222222222", "01J8ZGH0STSCREENN0SUCHR0W1", 1752537000000)
	if err := s.AddPairingGrant(ctx, ghost, "01J8Z4DEM0SCREENF1RSTPH0TN", "api"); !errors.Is(err, store.ErrPairingGrantScreenUnknown) {
		t.Errorf("a grant naming a nonexistent screen = %v, want ErrPairingGrantScreenUnknown", err)
	}
}

// TestAddPairingGrantRefusesANonPositiveTTL is SEC-032.
//
// A TTL is what makes a pairing credential short-lived, and the mint stores
// issued_at plus this value as the expiry. A zero or negative TTL does not
// produce a grant that expires immediately — it produces one whose expiry is at
// or BEFORE its issuance, and the expiry is consulted by the same self-cleaning
// sweep that retires old rows. A credential whose lifetime is not positive is
// not a short-lived credential; it is one whose lifetime nobody chose.
func TestAddPairingGrantRefusesANonPositiveTTL(t *testing.T) {
	for _, ttl := range []int64{0, -1, -900} {
		s := openMem(t)
		ctx := context.Background()
		if err := s.SeedDemo(ctx, seedAssetRef); err != nil {
			t.Fatalf("SeedDemo: %v", err)
		}
		before := gen(t, s)

		g := boundGrant("grant-ttl-00000000000000000000000000000000", store.SeedScreenID, 1752537000000)
		g.TTL = ttl

		if err := s.AddPairingGrant(ctx, g, "01J8Z4DEM0SCREENF1RSTPH0TN", "api"); err == nil {
			t.Errorf("AddPairingGrant accepted ttl %d — a pairing credential's lifetime must be positive (SEC-032)", ttl)
		}
		assertNothingMinted(t, s, before)
	}

	// The control: a positive ttl mints. Without it the loop above is satisfied
	// by a store that refuses every grant regardless of ttl.
	s := openMem(t)
	ctx := context.Background()
	if err := s.SeedDemo(ctx, seedAssetRef); err != nil {
		t.Fatalf("SeedDemo: %v", err)
	}
	g := boundGrant("grant-ttl-ok-000000000000000000000000000000", store.SeedScreenID, 1752537000000)
	g.TTL = 1
	if err := s.AddPairingGrant(ctx, g, "01J8Z4DEM0SCREENF1RSTPH0TN", "api"); err != nil {
		t.Fatalf("a grant with ttl 1 was refused: %v", err)
	}
}

// TestRetirePairingGrantRefusesAnUnattributedReport covers the two argument
// checks on the retirement path.
//
// The relay id is the load-bearing one. REL-124b attributes a retirement to the
// authenticated connection reporting it, and the delete is conditioned on the
// grant being bound to that relay — which is what stops one relay cancelling a
// pairing in progress at its sibling. An empty relay id is not an identity, so
// letting it through would compare every grant's binding against "" and answer
// the attribution question with a value nobody authenticated as.
//
// An empty grant id names no row. Since an unknown grant is a deliberate
// idempotent no-op on this path, letting the empty string through would report
// "not on record, nothing to do" for a call that failed to say what it meant.
func TestRetirePairingGrantRefusesAnUnattributedReport(t *testing.T) {
	s := openMem(t)
	ctx := context.Background()
	if err := s.SeedDemo(ctx, seedAssetRef); err != nil {
		t.Fatalf("SeedDemo: %v", err)
	}
	const grantID = "grant-retire-0000000000000000000000000000"
	if err := s.AddPairingGrant(ctx, boundGrant(grantID, store.SeedScreenID, 1752537000000),
		"01J8Z4DEM0SCREENF1RSTPH0TN", "api"); err != nil {
		t.Fatalf("AddPairingGrant: %v", err)
	}
	before := gen(t, s)

	if _, err := s.RetirePairingGrant(ctx, "", testRelayID); err == nil {
		t.Error("RetirePairingGrant accepted an empty grant_id — an unknown grant is a deliberate no-op on this " +
			"path, so an empty one would be reported as 'nothing to retire' rather than as a call that said nothing")
	}
	// The relay case needs the error's IDENTITY, not merely its presence: without
	// the check, "" is compared against the grant's real binding and the call
	// returns ErrPairingGrantBoundElsewhere — an error, so a presence assertion
	// passes either way. That answer is also wrong in a way that matters. It
	// reports that the grant belongs to some other relay, when what happened is
	// that the caller supplied no identity at all; an operator reading it goes
	// looking for a rogue sibling relay that does not exist.
	if _, err := s.RetirePairingGrant(ctx, grantID, ""); err == nil {
		t.Error("RetirePairingGrant accepted an empty relay_id — the delete is conditioned on the reporting " +
			"connection's own authenticated identity (REL-124b), and \"\" is not an identity anyone authenticated as")
	} else if errors.Is(err, store.ErrPairingGrantBoundElsewhere) {
		t.Errorf("an UNATTRIBUTED retirement was refused as bound-elsewhere (%v) — that reports a conflict with "+
			"another relay for a call that named no relay, and sends an operator after a sibling that does not "+
			"exist", err)
	}

	// Neither refusal wrote anything: the grant is still on record and the
	// generation has not moved.
	if after := gen(t, s); after != before {
		t.Errorf("generation moved %d -> %d on refused retirements", before, after)
	}
	ds, err := s.DesiredState(ctx)
	if err != nil {
		t.Fatalf("DesiredState: %v", err)
	}
	if len(ds.PairingGrants) != 1 {
		t.Fatalf("desired state carries %d grant(s) after two refused retirements, want the 1 still on record",
			len(ds.PairingGrants))
	}

	// The control that keeps the two distinguishable: a retirement attributed to
	// a DIFFERENT real relay still gets bound-elsewhere.
	if _, err := s.RetirePairingGrant(ctx, grantID, "01J8ZRELAYBBBBBBBBBBBBBBB2"); !errors.Is(err, store.ErrPairingGrantBoundElsewhere) {
		t.Errorf("a retirement from another relay = %v, want ErrPairingGrantBoundElsewhere", err)
	}

	// The control: a properly attributed retirement DOES retire it. Without it,
	// both refusals above are satisfied by a method that refuses everything.
	retired, err := s.RetirePairingGrant(ctx, grantID, testRelayID)
	if err != nil {
		t.Fatalf("RetirePairingGrant: %v", err)
	}
	if !retired {
		t.Error("a correctly attributed retirement did not retire the grant")
	}
}

// assertNothingMinted is the shared half of every refusal above: a grant refused
// at this boundary must not have advanced the generation, and must not ride
// desired state — where it would reach every relay enrolled to the site.
func assertNothingMinted(t *testing.T, s *store.Store, before int64) {
	t.Helper()
	if after := gen(t, s); after != before {
		t.Errorf("generation moved %d -> %d on a refused mint", before, after)
	}
	ds, err := s.DesiredState(context.Background())
	if err != nil {
		t.Fatalf("DesiredState: %v", err)
	}
	if len(ds.PairingGrants) != 0 {
		t.Fatalf("desired state carries %+v — a refused grant reached the wire", ds.PairingGrants)
	}
}
