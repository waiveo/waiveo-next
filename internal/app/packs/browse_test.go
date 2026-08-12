package packs_test

import (
	"context"
	"testing"

	"github.com/maaxton/waiveo-next/internal/app/packs"
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
