package store_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/maaxton/waiveo-next/internal/app/store"
)

// TestInstallPackRefusesASpecWithNoProvenance: the record is a hard
// precondition of an install, not an optional extra (marketplace/1 MKT-094a).
// A row that could not say which key vouched for the bytes is worse than no row
// — it would attest to provenance nobody established — so the store refuses the
// whole install rather than writing one.
func TestInstallPackRefusesASpecWithNoProvenance(t *testing.T) {
	st := openMem(t)
	ctx := context.Background()
	before := gen(t, st)

	for _, tc := range []struct {
		name string
		rec  store.PackInstallRecord
		want string
	}{
		// Every row omits exactly ONE required member and supplies the rest, so a
		// row fails for its own reason rather than for whichever check happens to
		// run first. Before verifying_key existed these rows relied on that
		// ordering, which made them a test of the check order as much as of the
		// checks.
		{"no source", store.PackInstallRecord{ContentDigest: "sha256:x", KeyID: "k", VerifyingKey: "sha256:kk"}, "source"},
		{"no content digest", store.PackInstallRecord{Source: store.SourceDirect, KeyID: "k", VerifyingKey: "sha256:kk"}, "content digest"},
		{"no key id", store.PackInstallRecord{Source: store.SourceDirect, ContentDigest: "sha256:x", VerifyingKey: "sha256:kk"}, "key id"},
		// MKT-094a: a record naming a key_id with no key behind it reads as
		// established provenance while, under a colliding id, identifying nothing —
		// "worse than recording none".
		{"no verifying key digest", store.PackInstallRecord{Source: store.SourceDirect, ContentDigest: "sha256:x", KeyID: "k"}, "verifying key digest"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec := packSpec("acme/menu-board", "1.0.0", 1)
			spec.Record = tc.rec
			_, _, err := st.InstallPack(ctx, spec)
			if err == nil {
				t.Fatal("InstallPack accepted a spec with no verified provenance")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want one naming the missing %s", err, tc.want)
			}
			if _, found, _ := st.GetPack(ctx, "acme/menu-board"); found {
				t.Fatal("a refused install left a pack row behind")
			}
			if after := gen(t, st); after != before {
				t.Fatalf("a refused install bumped the generation %d -> %d", before, after)
			}
		})
	}
}

// TestInstallRecordsAreAppendOnly: each successful install appends one record,
// id-ascending, so the newest is the current pin (MKT-094/094b).
func TestInstallRecordsAreAppendOnly(t *testing.T) {
	st := openMem(t)
	ctx := context.Background()

	for _, v := range []string{"1.0.0", "1.1.0", "2.0.0"} {
		spec := packSpec("acme/menu-board", v, 1)
		spec.Record.ResolvedVersion = v
		if _, _, err := st.InstallPack(ctx, spec); err != nil {
			t.Fatalf("install %s: %v", v, err)
		}
	}
	recs, err := st.ListPackInstalls(ctx, "acme/menu-board")
	if err != nil {
		t.Fatalf("ListPackInstalls: %v", err)
	}
	if len(recs) != 3 {
		t.Fatalf("records = %d, want 3 (append-only history, not a single pin)", len(recs))
	}
	for i, want := range []string{"1.0.0", "1.1.0", "2.0.0"} {
		if recs[i].ResolvedVersion != want {
			t.Fatalf("record[%d] = %s, want %s (chronological order)", i, recs[i].ResolvedVersion, want)
		}
		if recs[i].PackID != "acme/menu-board" {
			t.Fatalf("record[%d] pack id = %q", i, recs[i].PackID)
		}
		if i > 0 && recs[i-1].RecordID >= recs[i].RecordID {
			t.Fatalf("record ids are not strictly ascending: %q then %q", recs[i-1].RecordID, recs[i].RecordID)
		}
		if recs[i].InstalledAt == 0 {
			t.Fatalf("record[%d] has no installed_at", i)
		}
	}
}

// TestChannelMarkAdvancesOnlyUpward: the MKT-050 high-water mark is raised by a
// higher version and never walked back by a lower one, with the caller's own
// version order (MKT-050a) doing the comparison — the store never guesses one.
func TestChannelMarkAdvancesOnlyUpward(t *testing.T) {
	st := openMem(t)
	ctx := context.Background()

	// A deliberately NUMERIC comparator, mirroring MKT-050a: 1.10.0 outranks
	// 1.9.0, which a string comparison gets backwards.
	higher := func(candidate, current string) bool {
		rank := map[string]int{"1.9.0": 1, "1.10.0": 2}
		return rank[candidate] > rank[current]
	}
	install := func(version string) {
		t.Helper()
		spec := packSpec("acme/menu-board", version, 1)
		spec.Record.ResolvedVersion = version
		spec.Record.TrustChannel = "community"
		spec.ChannelMark = &store.ChannelMarkAdvance{TrustChannel: "community", Version: version, Higher: higher}
		if _, _, err := st.InstallPack(ctx, spec); err != nil {
			t.Fatalf("install %s: %v", version, err)
		}
	}

	install("1.10.0")
	if mark, found, _ := st.PackChannelMark(ctx, "acme/menu-board", "community"); !found || mark != "1.10.0" {
		t.Fatalf("mark = %q found=%v, want 1.10.0", mark, found)
	}
	install("1.9.0") // a lower version still installs; it must not lower the mark
	if mark, _, _ := st.PackChannelMark(ctx, "acme/menu-board", "community"); mark != "1.10.0" {
		t.Fatalf("mark = %q after installing a lower version, want it unchanged at 1.10.0", mark)
	}
}

// TestChannelMarkNeedsAVersionOrder: a mark advance with no comparator is a
// programming error the store refuses rather than falling back to a string
// comparison — which is exactly the ordering MKT-050a forbids.
func TestChannelMarkNeedsAVersionOrder(t *testing.T) {
	st := openMem(t)
	spec := packSpec("acme/menu-board", "1.0.0", 1)
	spec.ChannelMark = &store.ChannelMarkAdvance{TrustChannel: "community", Version: "1.0.0"}
	if _, _, err := st.InstallPack(context.Background(), spec); err == nil {
		t.Fatal("InstallPack accepted a channel-mark advance with no version order")
	}
}

// TestUninstallRemovesRecordsButNotTheMark is MKT-094b's asymmetry at the store
// boundary: pack-owned state goes with the pack; the verifier's anti-rollback
// memory does not, or uninstall-then-reinstall becomes a downgrade path.
func TestUninstallRemovesRecordsButNotTheMark(t *testing.T) {
	st := openMem(t)
	ctx := context.Background()

	spec := packSpec("acme/menu-board", "2.0.0", 1)
	spec.Record.ResolvedVersion = "2.0.0"
	spec.Record.TrustChannel = "community"
	spec.ChannelMark = &store.ChannelMarkAdvance{
		TrustChannel: "community", Version: "2.0.0",
		Higher: func(candidate, current string) bool { return candidate > current },
	}
	pack, _, err := st.InstallPack(ctx, spec)
	if err != nil {
		t.Fatalf("InstallPack: %v", err)
	}
	if err := st.UninstallPack(ctx, "acme/menu-board", pack.Revision); err != nil {
		t.Fatalf("UninstallPack: %v", err)
	}

	if recs, _ := st.ListPackInstalls(ctx, "acme/menu-board"); len(recs) != 0 {
		t.Fatalf("uninstall left %d install record(s)", len(recs))
	}
	mark, found, err := st.PackChannelMark(ctx, "acme/menu-board", "community")
	if err != nil {
		t.Fatalf("PackChannelMark: %v", err)
	}
	if !found || mark != "2.0.0" {
		t.Fatalf("mark after uninstall = %q found=%v, want 2.0.0 preserved (MKT-094b)", mark, found)
	}
}

// TestInstallRecordsAreScopedToTheirPack: one pack's history never contains
// another's, so an install record cannot be read as evidence about a pack it
// does not belong to.
func TestInstallRecordsAreScopedToTheirPack(t *testing.T) {
	st := openMem(t)
	ctx := context.Background()
	for _, id := range []string{"acme/one", "acme/two"} {
		if _, _, err := st.InstallPack(ctx, packSpec(id, "1.0.0", 1)); err != nil {
			t.Fatalf("install %s: %v", id, err)
		}
	}
	for _, id := range []string{"acme/one", "acme/two"} {
		recs, _ := st.ListPackInstalls(ctx, id)
		if len(recs) != 1 || recs[0].PackID != id {
			t.Fatalf("%s history = %+v, want exactly its own one record", id, recs)
		}
	}
	if recs, _ := st.ListPackInstalls(ctx, "acme/three"); len(recs) != 0 {
		t.Fatalf("a never-installed pack has %d records", len(recs))
	}
}

// TestPointerMarkRefusesBelowTheMarkInsideTheTransaction: the resolver reads the
// high-water mark OUTSIDE the write lock, so two concurrent pointer resolutions
// can both pass that pre-check. The mark is therefore re-asserted inside the
// install transaction, where a strictly-lower pointer install is REFUSED rather
// than silently installed with the mark left above it — which would leave the
// box running a version every later pointer resolution is refused against.
//
// An EQUAL version still installs: re-applying the version a channel currently
// points at is the ordinary case, not a rollback.
func TestPointerMarkRefusesBelowTheMarkInsideTheTransaction(t *testing.T) {
	st := openMem(t)
	ctx := context.Background()
	higher := func(candidate, current string) bool {
		rank := map[string]int{"1.0.0": 1, "2.0.0": 2}
		return rank[candidate] > rank[current]
	}
	install := func(version string, viaPointer bool) error {
		spec := packSpec("acme/menu-board", version, 1)
		spec.Record.ResolvedVersion = version
		spec.Record.TrustChannel = "community"
		spec.ChannelMark = &store.ChannelMarkAdvance{
			TrustChannel: "community", Version: version, Higher: higher, ViaPointer: viaPointer,
		}
		_, _, err := st.InstallPack(ctx, spec)
		return err
	}

	if err := install("2.0.0", true); err != nil {
		t.Fatalf("install 2.0.0: %v", err)
	}
	gen0 := gen(t, st)

	// Strictly below the mark, via pointer: refused, and nothing moves.
	if err := install("1.0.0", true); !errors.Is(err, store.ErrChannelMarkRollback) {
		t.Fatalf("pointer install below the mark = %v, want ErrChannelMarkRollback", err)
	}
	if pack, _, _ := st.GetPack(ctx, "acme/menu-board"); pack.Version != "2.0.0" {
		t.Fatalf("installed version = %q after a refused pointer install, want 2.0.0", pack.Version)
	}
	if after := gen(t, st); after != gen0 {
		t.Fatalf("a refused pointer install bumped the generation %d -> %d", gen0, after)
	}

	// Equal to the mark, via pointer: installs (not a rollback).
	if err := install("2.0.0", true); err != nil {
		t.Fatalf("re-applying the version the pointer names was refused: %v", err)
	}
	// Strictly below, but an EXPLICIT pin: MKT-050 does not govern it.
	if err := install("1.0.0", false); err != nil {
		t.Fatalf("explicit-version pin below the mark was refused: %v", err)
	}
	if mark, _, _ := st.PackChannelMark(ctx, "acme/menu-board", "community"); mark != "2.0.0" {
		t.Fatalf("mark = %q, want it held at 2.0.0", mark)
	}
}

// TestChannelMarkAdvanceRefusesAnIncompleteAdvance pins advanceChannelMark's
// boundary checks through the public install contract. ChannelMarkAdvance is
// the store's own API surface: the packs resolver happens to always fill it
// today, but the store must not COMMIT a mark it cannot compare or key.
//
// The stakes are per-field. A nil Higher is the MKT-050a rule at the store
// boundary — the mark is meaningless without the caller's version order, and
// the store inventing a fallback order is exactly what MKT-050a forbids. An
// empty trust channel or version would commit a mark row keyed on "" — a
// phantom (pack, channel) pair no later MKT-050 comparison ever reads, so the
// anti-rollback floor the install claims to raise would not exist.
//
// The refusal must also abort the WHOLE install: the advance rides the install
// transaction, and a pack landing while its mark did not is the torn state the
// transaction exists to prevent.
func TestChannelMarkAdvanceRefusesAnIncompleteAdvance(t *testing.T) {
	st := openMem(t)
	ctx := context.Background()
	higher := func(candidate, current string) bool { return candidate > current }

	for _, tc := range []struct {
		name string
		adv  *store.ChannelMarkAdvance
	}{
		{"no version order", &store.ChannelMarkAdvance{TrustChannel: "community", Version: "1.0.0"}},
		{"no trust channel", &store.ChannelMarkAdvance{Version: "1.0.0", Higher: higher}},
		{"no version", &store.ChannelMarkAdvance{TrustChannel: "community", Higher: higher}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec := packSpec("acme/menu-board", "1.0.0", 1)
			spec.Record.ResolvedVersion = "1.0.0"
			spec.Record.TrustChannel = "community"
			spec.ChannelMark = tc.adv
			if _, _, err := st.InstallPack(ctx, spec); err == nil {
				t.Fatal("InstallPack committed with an incomplete channel-mark advance")
			}
			if _, found, _ := st.GetPack(ctx, "acme/menu-board"); found {
				t.Fatal("the refused install left the pack installed — the advance must abort the transaction, not just skip the mark")
			}
			if _, found, _ := st.PackChannelMark(ctx, "acme/menu-board", "community"); found {
				t.Fatal("the refused install left a channel mark")
			}
		})
	}

	// The control: the same spec with a complete advance commits, and the mark
	// exists — so the refusals above are about the missing fields, not about
	// marked installs generally.
	spec := packSpec("acme/menu-board", "1.0.0", 1)
	spec.Record.ResolvedVersion = "1.0.0"
	spec.Record.TrustChannel = "community"
	spec.ChannelMark = &store.ChannelMarkAdvance{TrustChannel: "community", Version: "1.0.0", Higher: higher}
	if _, _, err := st.InstallPack(ctx, spec); err != nil {
		t.Fatalf("a complete channel-mark advance failed: %v", err)
	}
	if mark, found, _ := st.PackChannelMark(ctx, "acme/menu-board", "community"); !found || mark != "1.0.0" {
		t.Fatalf("mark = %q found=%v, want 1.0.0", mark, found)
	}
}
