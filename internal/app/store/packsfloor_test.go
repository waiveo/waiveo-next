package store_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/maaxton/waiveo-next/internal/app/store"
)

// These tests drive the required-pack floor (marketplace/1 MKT-093a/MKT-093b)
// and the install-time compare-and-swap at the STORE, because that is where
// MKT-093b says the rule has to live: "in the same transaction that commits the
// install or the removal ... never in a request handler above it". A test that
// only drove the HTTP handler would pass against a floor implemented purely in
// the handler, which is the shape the requirement exists to forbid.

// fixtureRoster is a host-provisioned roster (MKT-093a): pack id -> floor
// version, with the version order supplied by the caller exactly as the real
// packs.Roster supplies it. The store deliberately knows no version grammar.
type fixtureRoster map[string]string

func (r fixtureRoster) RequiredFloor(packID string) (string, bool) {
	floor, ok := r[packID]
	return floor, ok
}

// MeetsFloor is component-wise numeric over MAJOR.MINOR.PATCH (MKT-050a), kept
// deliberately independent of internal/app/packs' own implementation so this
// file exercises the store's use of the seam rather than agreeing with itself.
func (r fixtureRoster) MeetsFloor(version, floor string) bool {
	v, ok := splitVersion(version)
	if !ok {
		return false
	}
	f, ok := splitVersion(floor)
	if !ok {
		return false
	}
	for i := range v {
		if v[i] != f[i] {
			return v[i] > f[i]
		}
	}
	return true
}

func splitVersion(s string) ([3]int, bool) {
	var out [3]int
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return out, false
	}
	for i, p := range parts {
		if p == "" {
			return out, false
		}
		n := 0
		for _, c := range p {
			if c < '0' || c > '9' {
				return out, false
			}
			n = n*10 + int(c-'0')
		}
		out[i] = n
	}
	return out, true
}

// TestRequiredPackCannotBeUninstalled: MKT-093b(i). Removal is removal to NO
// version, which is below every floor, so an ordinary uninstall of a roster
// member is refused — and refused with everything it owns still present, since
// the refusal happens inside the removal transaction rather than after part of
// the cascade has run.
func TestRequiredPackCannotBeUninstalled(t *testing.T) {
	st := openMem(t)
	ctx := context.Background()
	st.SetRequiredPacks(fixtureRoster{"waiveo/system": "1.0.0"})

	pack, _, err := st.InstallPack(ctx, packSpec("waiveo/system", "2.0.0", 1))
	if err != nil {
		t.Fatalf("InstallPack: %v", err)
	}
	before := gen(t, st)

	err = st.UninstallPack(ctx, "waiveo/system", pack.Revision)
	var ferr *store.RequiredPackFloorError
	if !errors.As(err, &ferr) {
		t.Fatalf("UninstallPack err = %v, want *RequiredPackFloorError", err)
	}
	if ferr.PackID != "waiveo/system" || ferr.Floor != "1.0.0" || ferr.Version != "" {
		t.Fatalf("floor error = %+v, want {waiveo/system 1.0.0 <empty version>}", ferr)
	}

	if _, found, err := st.GetPack(ctx, "waiveo/system"); err != nil || !found {
		t.Fatalf("refused uninstall removed the pack row (found=%v err=%v)", found, err)
	}
	recs, err := st.ListPackInstalls(ctx, "waiveo/system")
	if err != nil || len(recs) != 1 {
		t.Fatalf("refused uninstall removed the install records: %d record(s), err=%v", len(recs), err)
	}
	if after := gen(t, st); after != before {
		t.Fatalf("refused uninstall bumped the generation %d -> %d", before, after)
	}
}

// TestRequiredPackRefusalIgnoresTheRevisionPrecondition: the refusal is a
// property of the pack, not of what revision the caller holds. A wrong
// If-Match against a required pack answers REQUIRED_PACK_FLOOR, not
// REVISION_CONFLICT — otherwise the endpoint would leak the current revision
// through the difference between the two answers.
func TestRequiredPackRefusalIgnoresTheRevisionPrecondition(t *testing.T) {
	st := openMem(t)
	ctx := context.Background()
	st.SetRequiredPacks(fixtureRoster{"waiveo/system": "1.0.0"})
	if _, _, err := st.InstallPack(ctx, packSpec("waiveo/system", "2.0.0", 1)); err != nil {
		t.Fatalf("InstallPack: %v", err)
	}

	err := st.UninstallPack(ctx, "waiveo/system", 999)
	var ferr *store.RequiredPackFloorError
	if !errors.As(err, &ferr) {
		t.Fatalf("UninstallPack with a stale revision err = %v, want *RequiredPackFloorError", err)
	}
	var rme *store.RevisionMismatchError
	if errors.As(err, &rme) {
		t.Fatalf("a required pack answered a revision conflict (%+v), leaking the current revision", rme)
	}
}

// TestUnrequiredPackStillUninstalls: the floor restricts exactly the packs the
// deployment named. Without this the test above would pass against an
// implementation that refused every uninstall.
func TestUnrequiredPackStillUninstalls(t *testing.T) {
	st := openMem(t)
	ctx := context.Background()
	st.SetRequiredPacks(fixtureRoster{"waiveo/system": "1.0.0"})

	pack, _, err := st.InstallPack(ctx, packSpec("acme/menu-board", "1.0.0", 1))
	if err != nil {
		t.Fatalf("InstallPack: %v", err)
	}
	if err := st.UninstallPack(ctx, "acme/menu-board", pack.Revision); err != nil {
		t.Fatalf("UninstallPack of a pack the roster does not name: %v", err)
	}
}

// TestNoRosterMakesNothingRequired: MKT-093a's default. An absent roster
// withholds a RESTRICTION, so failing closed would mean refusing to remove
// packs no deployment ever called essential.
func TestNoRosterMakesNothingRequired(t *testing.T) {
	st := openMem(t)
	ctx := context.Background()

	pack, _, err := st.InstallPack(ctx, packSpec("waiveo/system", "1.0.0", 1))
	if err != nil {
		t.Fatalf("InstallPack: %v", err)
	}
	if err := st.UninstallPack(ctx, "waiveo/system", pack.Revision); err != nil {
		t.Fatalf("UninstallPack with no roster wired: %v", err)
	}
	if floor, required := st.RequiredFloor("waiveo/system"); required {
		t.Fatalf("RequiredFloor with no roster = (%q, true), want not required", floor)
	}
}

// TestInstallBelowRequiredFloorRefused: MKT-093b(ii) at the store, which is the
// one place EVERY install path converges — a pointer resolution, an explicit
// version pin, and a direct upload all reach InstallPack, so a floor enforced
// here cannot be walked around by choosing a different route in.
func TestInstallBelowRequiredFloorRefused(t *testing.T) {
	st := openMem(t)
	ctx := context.Background()
	st.SetRequiredPacks(fixtureRoster{"waiveo/system": "2.0.0"})
	before := gen(t, st)

	_, _, err := st.InstallPack(ctx, packSpec("waiveo/system", "1.9.9", 1))
	var ferr *store.RequiredPackFloorError
	if !errors.As(err, &ferr) {
		t.Fatalf("InstallPack below the floor err = %v, want *RequiredPackFloorError", err)
	}
	if ferr.Version != "1.9.9" || ferr.Floor != "2.0.0" {
		t.Fatalf("floor error = %+v, want version 1.9.9 below floor 2.0.0", ferr)
	}
	if _, found, err := st.GetPack(ctx, "waiveo/system"); err != nil || found {
		t.Fatalf("a refused below-floor install left a pack row (found=%v err=%v)", found, err)
	}
	recs, err := st.ListPackInstalls(ctx, "waiveo/system")
	if err != nil || len(recs) != 0 {
		t.Fatalf("a refused below-floor install left %d record(s), err=%v", len(recs), err)
	}
	if after := gen(t, st); after != before {
		t.Fatalf("a refused below-floor install bumped the generation %d -> %d", before, after)
	}

	// 2.0.0 is AT the floor, not above it: the floor is a minimum, not a
	// strict lower bound, so the version the deployment declared must itself
	// install. A `>` where `>=` belongs would brick the required pack.
	if _, _, err := st.InstallPack(ctx, packSpec("waiveo/system", "2.0.0", 1)); err != nil {
		t.Fatalf("InstallPack AT the floor: %v", err)
	}
	// And a version below the floor still refuses once one is installed — the
	// floor governs a downgrade exactly as it governs a fresh install.
	if _, _, err := st.InstallPack(ctx, packSpec("waiveo/system", "1.0.0", 1)); !errors.As(err, &ferr) {
		t.Fatalf("downgrade below the floor err = %v, want *RequiredPackFloorError", err)
	}
}

// TestFloorComparisonIsNumericNotLexicographic: the floor is compared through
// the caller's MKT-050a order. A string comparison would rank 1.10.0 below
// 1.9.0 and let a below-floor version through at exactly the versions a
// long-lived required pack reaches.
func TestFloorComparisonIsNumericNotLexicographic(t *testing.T) {
	st := openMem(t)
	ctx := context.Background()
	st.SetRequiredPacks(fixtureRoster{"waiveo/system": "1.9.0"})

	if _, _, err := st.InstallPack(ctx, packSpec("waiveo/system", "1.10.0", 1)); err != nil {
		t.Fatalf("InstallPack 1.10.0 against floor 1.9.0: %v — the comparison ranked it below, i.e. it is lexicographic", err)
	}
	var ferr *store.RequiredPackFloorError
	if _, _, err := st.InstallPack(ctx, packSpec("waiveo/system", "1.8.9", 1)); !errors.As(err, &ferr) {
		t.Fatalf("InstallPack 1.8.9 against floor 1.9.0 err = %v, want *RequiredPackFloorError", err)
	}
}

// TestUnplaceableVersionNeverMeetsAFloor: MKT-050a — a version outside MAN-002's
// grammar has no position in the order, so it can never satisfy a floor. It
// fails closed rather than being compared under some fallback.
func TestUnplaceableVersionNeverMeetsAFloor(t *testing.T) {
	st := openMem(t)
	ctx := context.Background()
	st.SetRequiredPacks(fixtureRoster{"waiveo/system": "1.0.0"})

	var ferr *store.RequiredPackFloorError
	if _, _, err := st.InstallPack(ctx, packSpec("waiveo/system", "2.0", 1)); !errors.As(err, &ferr) {
		t.Fatalf("InstallPack of an unplaceable version err = %v, want *RequiredPackFloorError", err)
	}
}

// TestExpectPriorVersionIsACompareAndSwap: the revert path's premise is read
// outside the write lock, so PackInstall.ExpectPriorVersion re-asserts it inside
// the transaction. It must refuse BOTH when the row moved on and when the row is
// gone — an InstallGuard alone cannot express the second, because a guard by
// design does not run on a fresh install.
func TestExpectPriorVersionIsACompareAndSwap(t *testing.T) {
	st := openMem(t)
	ctx := context.Background()
	if _, _, err := st.InstallPack(ctx, packSpec("acme/menu-board", "2.0.0", 1)); err != nil {
		t.Fatalf("InstallPack: %v", err)
	}
	before := gen(t, st)

	// The row carries 2.0.0; an install expecting 3.0.0 refuses.
	spec := packSpec("acme/menu-board", "1.0.0", 1)
	spec.ExpectPriorVersion = "3.0.0"
	if _, _, err := st.InstallPack(ctx, spec); !errors.Is(err, store.ErrPriorVersionChanged) {
		t.Fatalf("InstallPack with a stale ExpectPriorVersion err = %v, want ErrPriorVersionChanged", err)
	}
	got, found, err := st.GetPack(ctx, "acme/menu-board")
	if err != nil || !found || got.Version != "2.0.0" {
		t.Fatalf("refused CAS changed the row: %+v found=%v err=%v", got, found, err)
	}
	if after := gen(t, st); after != before {
		t.Fatalf("refused CAS bumped the generation %d -> %d", before, after)
	}

	// The pack is not installed at all: an expectation still refuses rather
	// than resurrecting it.
	absent := packSpec("acme/other", "1.0.0", 1)
	absent.ExpectPriorVersion = "1.0.0"
	if _, _, err := st.InstallPack(ctx, absent); !errors.Is(err, store.ErrPriorVersionChanged) {
		t.Fatalf("InstallPack expecting a prior version of an absent pack err = %v, want ErrPriorVersionChanged", err)
	}
	if _, found, err := st.GetPack(ctx, "acme/other"); err != nil || found {
		t.Fatalf("a refused CAS created a pack row (found=%v err=%v)", found, err)
	}

	// A matching expectation commits.
	ok := packSpec("acme/menu-board", "1.0.0", 1)
	ok.ExpectPriorVersion = "2.0.0"
	if _, _, err := st.InstallPack(ctx, ok); err != nil {
		t.Fatalf("InstallPack with a matching ExpectPriorVersion: %v", err)
	}
}
