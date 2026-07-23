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

// ---- pack_rows CRUD -------------------------------------------------------

const testScopeNode = "01J8Z2Q1M8H8N4T0V1W2X3Y4Z5"

func rowIn(scopeNode, externalID string, body string) store.PackRow {
	return store.PackRow{
		LifecycleState: "published",
		ScopeNode:      scopeNode,
		ExternalID:     externalID,
		Body:           json.RawMessage(body),
	}
}

// TestCreatePackRowAssignsEnvelopeAndBumpsGeneration: a created row gets a
// host-assigned entity_id (a ULID), revision 1, the timestamps, and advances the
// generation once.
func TestCreatePackRowAssignsEnvelopeAndBumpsGeneration(t *testing.T) {
	st := openMem(t)
	ctx := context.Background()
	if _, _, err := st.InstallPack(ctx, packSpec("acme/menu-board", "1.0.0", 1)); err != nil {
		t.Fatalf("install: %v", err)
	}
	before := gen(t, st)

	row, err := st.CreatePackRow(ctx, "acme/menu-board", "menu_items", rowIn(testScopeNode, "", `{"name":"Burger"}`))
	if err != nil {
		t.Fatalf("CreatePackRow: %v", err)
	}
	if len(row.EntityID) != 26 {
		t.Fatalf("entity_id = %q, want a 26-char ULID", row.EntityID)
	}
	if row.Revision != 1 {
		t.Fatalf("revision = %d, want 1", row.Revision)
	}
	if row.CreatedAt == 0 || row.UpdatedAt == 0 {
		t.Fatalf("timestamps unset: created=%d updated=%d", row.CreatedAt, row.UpdatedAt)
	}
	if after := gen(t, st); after != before+1 {
		t.Fatalf("generation = %d, want %d (one bump)", after, before+1)
	}

	got, found, err := st.GetPackRow(ctx, "acme/menu-board", "menu_items", row.EntityID)
	if err != nil || !found {
		t.Fatalf("GetPackRow: found=%v err=%v", found, err)
	}
	if got.ScopeNode != testScopeNode || string(got.Body) != `{"name":"Burger"}` {
		t.Fatalf("round-tripped row = %+v", got)
	}
	if got.Labels == nil || len(got.Labels) != 0 {
		t.Fatalf("labels = %v, want [] (never nil)", got.Labels)
	}
}

// TestUpdatePackRowOptimistic: an update at the current revision bumps it, keeps
// the immutable entity_id + created_at, and refreshes updated_at.
func TestUpdatePackRowOptimistic(t *testing.T) {
	st := openMem(t)
	ctx := context.Background()
	_, _, _ = st.InstallPack(ctx, packSpec("acme/menu-board", "1.0.0", 1))
	row, err := st.CreatePackRow(ctx, "acme/menu-board", "menu_items", rowIn(testScopeNode, "", `{"name":"Burger"}`))
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	next := store.PackRow{LifecycleState: "published", ScopeNode: testScopeNode, Body: json.RawMessage(`{"name":"Cheeseburger"}`)}
	updated, err := st.UpdatePackRow(ctx, "acme/menu-board", "menu_items", row.EntityID, row.Revision, next)
	if err != nil {
		t.Fatalf("UpdatePackRow: %v", err)
	}
	if updated.Revision != 2 || updated.EntityID != row.EntityID {
		t.Fatalf("update: revision=%d entity_id=%q; want 2/%q", updated.Revision, updated.EntityID, row.EntityID)
	}
	if updated.CreatedAt != row.CreatedAt {
		t.Fatalf("created_at changed: %d -> %d", row.CreatedAt, updated.CreatedAt)
	}
	if string(updated.Body) != `{"name":"Cheeseburger"}` {
		t.Fatalf("body = %s", updated.Body)
	}

	// A stale expected revision is refused, nothing changed.
	_, err = st.UpdatePackRow(ctx, "acme/menu-board", "menu_items", row.EntityID, 1, next)
	var rme *store.RevisionMismatchError
	if !errors.As(err, &rme) || rme.Current != 2 {
		t.Fatalf("stale update = %v; want RevisionMismatchError current=2", err)
	}
}

// TestDeletePackRowOptimistic: delete at the current revision removes the row and
// bumps the generation; a stale revision is refused; a missing row is ErrNotFound.
func TestDeletePackRowOptimistic(t *testing.T) {
	st := openMem(t)
	ctx := context.Background()
	_, _, _ = st.InstallPack(ctx, packSpec("acme/menu-board", "1.0.0", 1))
	row, _ := st.CreatePackRow(ctx, "acme/menu-board", "menu_items", rowIn(testScopeNode, "", `{"name":"Burger"}`))

	if err := st.DeletePackRow(ctx, "acme/menu-board", "menu_items", row.EntityID, 99); err == nil {
		t.Fatal("stale-revision delete succeeded")
	}
	if err := st.DeletePackRow(ctx, "acme/menu-board", "menu_items", row.EntityID, row.Revision); err != nil {
		t.Fatalf("DeletePackRow: %v", err)
	}
	if _, found, _ := st.GetPackRow(ctx, "acme/menu-board", "menu_items", row.EntityID); found {
		t.Fatal("row survived delete")
	}
	if err := st.DeletePackRow(ctx, "acme/menu-board", "menu_items", row.EntityID, 1); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("delete missing = %v; want ErrNotFound", err)
	}
}

// TestListPackRowsOrderedByEntityID: ListPackRows returns rows entity_id-ascending
// (the keyset order the cursor pages over) and scopes to the given pack+collection.
func TestListPackRowsOrderedByEntityID(t *testing.T) {
	st := openMem(t)
	ctx := context.Background()
	_, _, _ = st.InstallPack(ctx, packSpec("acme/menu-board", "1.0.0", 1))
	for i := 0; i < 5; i++ {
		if _, err := st.CreatePackRow(ctx, "acme/menu-board", "menu_items", rowIn(testScopeNode, "", `{"name":"x"}`)); err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
	}
	// A row in a different collection must not appear in menu_items.
	_, _ = st.CreatePackRow(ctx, "acme/menu-board", "other", rowIn(testScopeNode, "", `{}`))

	rows, err := st.ListPackRows(ctx, "acme/menu-board", "menu_items")
	if err != nil {
		t.Fatalf("ListPackRows: %v", err)
	}
	if len(rows) != 5 {
		t.Fatalf("len = %d, want 5 (collection-scoped)", len(rows))
	}
	for i := 1; i < len(rows); i++ {
		if rows[i-1].EntityID >= rows[i].EntityID {
			t.Fatalf("not entity_id-ascending at %d: %q >= %q", i, rows[i-1].EntityID, rows[i].EntityID)
		}
	}
}

// TestPackRowGuardRunsInsideWrite: a guard returning an error rolls the create back
// (no row, no generation bump) and propagates the error verbatim.
func TestPackRowGuardRunsInsideWrite(t *testing.T) {
	st := openMem(t)
	ctx := context.Background()
	_, _, _ = st.InstallPack(ctx, packSpec("acme/menu-board", "1.0.0", 1))
	before := gen(t, st)

	sentinel := errors.New("guard refused")
	_, err := st.CreatePackRow(ctx, "acme/menu-board", "menu_items", rowIn(testScopeNode, "dup", `{}`),
		func(existing []store.PackRow) error { return sentinel })
	if !errors.Is(err, sentinel) {
		t.Fatalf("create = %v; want sentinel guard error", err)
	}
	if after := gen(t, st); after != before {
		t.Fatalf("generation bumped despite a guard refusal: %d -> %d", before, after)
	}
	if rows, _ := st.ListPackRows(ctx, "acme/menu-board", "menu_items"); len(rows) != 0 {
		t.Fatalf("row written despite a guard refusal: %d", len(rows))
	}
}

// TestUninstallCascadesPackRows: uninstalling a pack removes its rows too (the
// dev-POC destructive uninstall).
func TestUninstallCascadesPackRows(t *testing.T) {
	st := openMem(t)
	ctx := context.Background()
	pack, _, _ := st.InstallPack(ctx, packSpec("acme/menu-board", "1.0.0", 1))
	if _, err := st.CreatePackRow(ctx, "acme/menu-board", "menu_items", rowIn(testScopeNode, "", `{"name":"Burger"}`)); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := st.UninstallPack(ctx, "acme/menu-board", pack.Revision); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	if rows, _ := st.ListPackRows(ctx, "acme/menu-board", "menu_items"); len(rows) != 0 {
		t.Fatalf("rows survived uninstall: %d", len(rows))
	}
}
