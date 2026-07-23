package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/maaxton/waiveo-next/internal/app/store"
)

func packSpec(id, version string, dataModelVersion int, files ...store.PackFile) store.PackInstall {
	return store.PackInstall{
		ID:               id,
		Version:          version,
		DataModelVersion: dataModelVersion,
		Manifest:         json.RawMessage(`{"id":"` + id + `","version":"` + version + `"}`),
		Files:            files,
	}
}

// TestInstallPackWritesRowsAndBumpsGenerationOnce: a fresh install lands the pack
// row + every bundled file and advances the store generation exactly once (the
// whole install is one transaction, not one bump per file).
func TestInstallPackWritesRowsAndBumpsGenerationOnce(t *testing.T) {
	st := openMem(t)
	ctx := context.Background()
	before := gen(t, st)

	spec := packSpec("acme/menu-board", "1.0.0", 1,
		store.PackFile{Kind: store.PackFilePage, Name: "menu-items", Body: json.RawMessage(`{"pageType":"list-detail"}`)},
		store.PackFile{Kind: store.PackFilePage, Name: "settings", Body: json.RawMessage(`{"pageType":"settings-form"}`)},
		store.PackFile{Kind: store.PackFileLocale, Name: "en", Body: json.RawMessage(`{"k":"v"}`)},
	)
	pack, created, err := st.InstallPack(ctx, spec)
	if err != nil {
		t.Fatalf("InstallPack: %v", err)
	}
	if !created || pack.Revision != 1 {
		t.Fatalf("install: created=%v revision=%d; want true/1", created, pack.Revision)
	}
	if after := gen(t, st); after != before+1 {
		t.Fatalf("generation = %d, want %d (exactly one bump)", after, before+1)
	}

	pages, err := st.PackFileNames(ctx, "acme/menu-board", store.PackFilePage)
	if err != nil {
		t.Fatalf("PackFileNames: %v", err)
	}
	if len(pages) != 2 || pages[0] != "menu-items" || pages[1] != "settings" {
		t.Fatalf("page files = %v; want [menu-items settings]", pages)
	}
	locale, ok, err := st.GetPackFile(ctx, "acme/menu-board", store.PackFileLocale, "en")
	if err != nil || !ok || string(locale) != `{"k":"v"}` {
		t.Fatalf("locale en = %q,%v,%v", locale, ok, err)
	}
}

// TestReinstallReplacesFilesAndBumpsRevision: a reinstall bumps the pack revision
// and REPLACES the whole file bundle — an orphaned file from the prior manifest
// must not survive.
func TestReinstallReplacesFilesAndBumpsRevision(t *testing.T) {
	st := openMem(t)
	ctx := context.Background()

	_, _, err := st.InstallPack(ctx, packSpec("acme/menu-board", "1.0.0", 1,
		store.PackFile{Kind: store.PackFilePage, Name: "old-page", Body: json.RawMessage(`{}`)},
	))
	if err != nil {
		t.Fatalf("first install: %v", err)
	}

	pack, created, err := st.InstallPack(ctx, packSpec("acme/menu-board", "2.0.0", 2,
		store.PackFile{Kind: store.PackFilePage, Name: "new-page", Body: json.RawMessage(`{}`)},
	))
	if err != nil {
		t.Fatalf("reinstall: %v", err)
	}
	if created || pack.Revision != 2 || pack.Version != "2.0.0" {
		t.Fatalf("reinstall: created=%v revision=%d version=%s; want false/2/2.0.0", created, pack.Revision, pack.Version)
	}
	if _, ok, _ := st.GetPackFile(ctx, "acme/menu-board", store.PackFilePage, "old-page"); ok {
		t.Fatal("orphaned old-page survived a reinstall")
	}
	if _, ok, _ := st.GetPackFile(ctx, "acme/menu-board", store.PackFilePage, "new-page"); !ok {
		t.Fatal("new-page missing after reinstall")
	}
}

// TestUninstallPackRemovesEverythingAtomically: uninstall removes the pack row,
// its files, and its rows in one transaction and bumps the generation once.
func TestUninstallPackRemovesEverything(t *testing.T) {
	st := openMem(t)
	ctx := context.Background()

	pack, _, err := st.InstallPack(ctx, packSpec("acme/menu-board", "1.0.0", 1,
		store.PackFile{Kind: store.PackFilePage, Name: "menu-items", Body: json.RawMessage(`{}`)},
		store.PackFile{Kind: store.PackFileLocale, Name: "en", Body: json.RawMessage(`{}`)},
	))
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	beforeUninstall := gen(t, st)

	if err := st.UninstallPack(ctx, "acme/menu-board", pack.Revision); err != nil {
		t.Fatalf("UninstallPack: %v", err)
	}
	if after := gen(t, st); after != beforeUninstall+1 {
		t.Fatalf("generation = %d, want %d (one bump)", after, beforeUninstall+1)
	}
	if _, found, _ := st.GetPack(ctx, "acme/menu-board"); found {
		t.Fatal("pack row survived uninstall")
	}
	if names, _ := st.PackFileNames(ctx, "acme/menu-board", store.PackFilePage); len(names) != 0 {
		t.Fatalf("page files survived uninstall: %v", names)
	}
	if _, ok, _ := st.GetPackFile(ctx, "acme/menu-board", store.PackFileLocale, "en"); ok {
		t.Fatal("locale file survived uninstall")
	}
}

// TestUninstallStaleRevisionRejected: an If-Match revision mismatch is refused and
// the pack is untouched.
func TestUninstallStaleRevisionRejected(t *testing.T) {
	st := openMem(t)
	ctx := context.Background()
	_, _, err := st.InstallPack(ctx, packSpec("acme/menu-board", "1.0.0", 1))
	if err != nil {
		t.Fatalf("install: %v", err)
	}

	err = st.UninstallPack(ctx, "acme/menu-board", 99)
	var rme *store.RevisionMismatchError
	if !errors.As(err, &rme) {
		t.Fatalf("UninstallPack(stale) = %v; want RevisionMismatchError", err)
	}
	if rme.Current != 1 {
		t.Fatalf("mismatch current = %d, want 1", rme.Current)
	}
	if _, found, _ := st.GetPack(ctx, "acme/menu-board"); !found {
		t.Fatal("pack removed despite a stale-revision refusal")
	}
}

// TestUninstallMissingPack: uninstalling a pack that does not exist is ErrNotFound.
func TestUninstallMissingPack(t *testing.T) {
	st := openMem(t)
	if err := st.UninstallPack(context.Background(), "nobody/here", 1); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("UninstallPack(missing) = %v; want ErrNotFound", err)
	}
}

// TestListPacksOrderedByID: ListPacks returns packs id-ascending (the keyset order).
func TestListPacksOrderedByID(t *testing.T) {
	st := openMem(t)
	ctx := context.Background()
	for _, id := range []string{"z/pack", "a/pack", "m/pack"} {
		if _, _, err := st.InstallPack(ctx, packSpec(id, "1.0.0", 1)); err != nil {
			t.Fatalf("install %s: %v", id, err)
		}
	}
	list, err := st.ListPacks(ctx)
	if err != nil {
		t.Fatalf("ListPacks: %v", err)
	}
	if len(list) != 3 || list[0].ID != "a/pack" || list[1].ID != "m/pack" || list[2].ID != "z/pack" {
		t.Fatalf("list order = %v; want [a m z]", []string{list[0].ID, list[1].ID, list[2].ID})
	}
}
