package packs_test

import (
	"context"
	"errors"
	"testing"

	"github.com/maaxton/waiveo-next/internal/app/packs"
	"github.com/maaxton/waiveo-next/internal/app/store"
)

// Catalog browse (MKT-096). The rules under test are the ones a later change is
// most likely to get wrong, because each is a case where listing MORE looks
// harmless and is not.

func browseFixture(t *testing.T) (*packs.Installer, *registry, *testSigner) {
	t.Helper()
	_, in, signer, reg := updateFixture(t)
	return in, reg, signer
}

func TestBrowseListsWhatASourceOffers(t *testing.T) {
	in, reg, signer := browseFixture(t)
	publishVersion(t, reg, signer, "1.0.0")
	publishVersion(t, reg, signer, "2.0.0")

	sources := in.BrowseCatalog(context.Background())
	if len(sources) != 1 {
		t.Fatalf("sources = %d, want 1", len(sources))
	}
	if sources[0].Unavailable != "" {
		t.Fatalf("source reported unavailable: %s", sources[0].Unavailable)
	}
	if len(sources[0].Entries) != 2 {
		t.Fatalf("entries = %d, want both published versions: %+v", len(sources[0].Entries), sources[0].Entries)
	}
	e := sources[0].Entries[0]
	if e.PackID != "acme/menu-board" {
		t.Errorf("pack id = %q — artifact_id IS the pack id, with no version suffix", e.PackID)
	}
	// Attribution is not decoration: source order is a resolution preference,
	// never a trust decision (MKT-061), so every entry has to name its source.
	if e.Source == "" {
		t.Error("entry names no source")
	}
}

// A yanked version is one resolution refuses outright. Listing it offers a
// refusal — the operator clicks, and the box says no.
func TestBrowseWithholdsAYankedVersion(t *testing.T) {
	in, reg, signer := browseFixture(t)
	publishVersion(t, reg, signer, "1.0.0")
	publishVersion(t, reg, signer, "2.0.0")
	reg.setStatus("acme/menu-board", "2.0.0", "yanked")

	for _, e := range in.BrowseCatalog(context.Background())[0].Entries {
		if e.Version == "2.0.0" {
			t.Fatalf("browse offered a yanked version: %+v", e)
		}
	}
}

// `archived` is the PUBLISHER's own withdrawal from discovery/browse (MKT-044).
// A host that lists it anyway overrides a decision that was not its to make —
// and the version stays installable by exact reference, which is the difference
// between not advertising something and refusing it.
func TestBrowseRespectsAPublishersWithdrawalFromDiscovery(t *testing.T) {
	in, reg, signer := browseFixture(t)
	publishVersion(t, reg, signer, "1.0.0")
	publishVersion(t, reg, signer, "2.0.0")
	reg.setStatus("acme/menu-board", "1.0.0", "archived")

	entries := in.BrowseCatalog(context.Background())[0].Entries
	for _, e := range entries {
		if e.Version == "1.0.0" {
			t.Fatalf("browse advertised an archived version: %+v", e)
		}
	}
	if len(entries) != 1 || entries[0].Version != "2.0.0" {
		t.Fatalf("entries = %+v, want only 2.0.0 still offered", entries)
	}
}

// An empty catalog and an unreachable registry are different facts. A browse
// that drops the failing source states the first when the second is true.
func TestBrowseReportsAnUnreadableSourceRatherThanOmittingIt(t *testing.T) {
	in, reg, signer := browseFixture(t)
	publishVersion(t, reg, signer, "1.0.0")
	reg.breakIndex()

	sources := in.BrowseCatalog(context.Background())
	if len(sources) != 1 {
		t.Fatalf("sources = %d, want the failing source still listed", len(sources))
	}
	if sources[0].Unavailable == "" {
		t.Fatal("an unreadable source reported no reason — indistinguishable from an empty catalog")
	}
	if len(sources[0].Entries) != 0 {
		t.Fatalf("an unreadable source contributed entries: %+v", sources[0].Entries)
	}
}

// ---------------------------------------------------------------------------
// MKT-097 — disabling, which must WITHDRAW without destroying.
// ---------------------------------------------------------------------------

// The property the whole operation exists for: everything an uninstall would
// remove survives being disabled. Without this, "turn it off while I look at
// this" is just a slower uninstall.
func TestDisablingPreservesEverythingAnUninstallWouldRemove(t *testing.T) {
	st, in, signer, reg := updateFixture(t)
	ctx := context.Background()
	publishVersion(t, reg, signer, "1.0.0")
	if _, err := in.InstallRef(ctx, packs.Ref{PackID: "acme/menu-board", TrustChannel: "community"}); err != nil {
		t.Fatalf("InstallRef: %v", err)
	}
	before := snapshotInstalled(t, st, "acme/menu-board", "community")

	if err := in.SetEnabled(ctx, "acme/menu-board", false); err != nil {
		t.Fatalf("SetEnabled(false): %v", err)
	}

	// The pack, its files, its rows, its records and the channel mark are all
	// exactly as they were — assertUnchanged covers every one, and the
	// enablement flag is not part of the snapshot precisely because it is the
	// one thing that MAY differ.
	before.assertUnchanged(t, st, "a disabled pack")

	p, found, err := st.GetPack(ctx, "acme/menu-board")
	if err != nil || !found {
		t.Fatalf("GetPack: found=%v err=%v", found, err)
	}
	if p.Enabled {
		t.Fatal("the pack reports enabled after being disabled")
	}
	// Not an edit to the pack: a revision bump would make a disable look like a
	// new version to anything comparing them.
	if p.Revision != before.pack.Revision {
		t.Errorf("revision moved %d -> %d on a disable", before.pack.Revision, p.Revision)
	}
}

func TestEnablingRestoresWithoutReinstalling(t *testing.T) {
	st, in, signer, reg := updateFixture(t)
	ctx := context.Background()
	publishVersion(t, reg, signer, "1.0.0")
	if _, err := in.InstallRef(ctx, packs.Ref{PackID: "acme/menu-board", TrustChannel: "community"}); err != nil {
		t.Fatalf("InstallRef: %v", err)
	}
	if err := in.SetEnabled(ctx, "acme/menu-board", false); err != nil {
		t.Fatalf("disable: %v", err)
	}
	before := snapshotInstalled(t, st, "acme/menu-board", "community")

	if err := in.SetEnabled(ctx, "acme/menu-board", true); err != nil {
		t.Fatalf("enable: %v", err)
	}
	// No install ran: no new record, no new revision, nothing re-resolved.
	before.assertUnchanged(t, st, "a re-enabled pack")
	p, _, _ := st.GetPack(ctx, "acme/menu-board")
	if !p.Enabled {
		t.Fatal("the pack is still disabled after being enabled")
	}
}

// Idempotent: PUT of a state, so a retry after a timeout re-asserts rather than
// flipping the pack back on.
func TestSettingTheStateItIsAlreadyInSucceeds(t *testing.T) {
	_, in, signer, reg := updateFixture(t)
	ctx := context.Background()
	publishVersion(t, reg, signer, "1.0.0")
	if _, err := in.InstallRef(ctx, packs.Ref{PackID: "acme/menu-board", TrustChannel: "community"}); err != nil {
		t.Fatalf("InstallRef: %v", err)
	}
	if err := in.SetEnabled(ctx, "acme/menu-board", true); err != nil {
		t.Fatalf("enabling an enabled pack: %v", err)
	}
	if err := in.SetEnabled(ctx, "acme/menu-board", false); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if err := in.SetEnabled(ctx, "acme/menu-board", false); err != nil {
		t.Fatalf("disabling a disabled pack: %v", err)
	}
}

func TestSetEnabledOfAnUninstalledPackIsNotFound(t *testing.T) {
	_, in, _, _ := updateFixture(t)
	if err := in.SetEnabled(context.Background(), "acme/nope", false); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("err = %v, want store.ErrNotFound", err)
	}
}
