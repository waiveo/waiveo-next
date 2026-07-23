package packs_test

import (
	"context"
	"errors"
	"testing"

	"github.com/maaxton/waiveo-next/internal/app/packs"
	"github.com/maaxton/waiveo-next/internal/app/store"
)

func openStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func gen(t *testing.T, st *store.Store) int64 {
	t.Helper()
	g, err := st.Generation(context.Background())
	if err != nil {
		t.Fatalf("Generation: %v", err)
	}
	return g
}

// manifestFieldCode asserts err is a *ManifestError carrying a violation at the
// given field with the given code.
func manifestFieldCode(t *testing.T, err error, field, code string) {
	t.Helper()
	var merr *packs.ManifestError
	if !errors.As(err, &merr) {
		t.Fatalf("error = %v (%T), want *packs.ManifestError", err, err)
	}
	for _, e := range merr.Errors {
		if e.Field == field {
			if e.Code != code {
				t.Fatalf("error at %s has code %q, want %q", field, e.Code, code)
			}
			return
		}
	}
	t.Fatalf("no manifest error at field %q; got %+v", field, merr.Errors)
}

// TestInstallValidMinimalPack: a valid pack installs — the pack row, its page
// documents, and its locale catalog all land, and the store generation is
// bumped EXACTLY once for the whole install (atomic, not per-file).
func TestInstallValidPack(t *testing.T) {
	st := openStore(t)
	in := packs.NewInstaller(st)
	ctx := context.Background()

	before := gen(t, st)
	res, err := in.Install(ctx, basePackZip(t, baseManifest()))
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if res.ID != "acme/menu-board" || res.Version != "1.0.0" || !res.Created {
		t.Fatalf("result = %+v; want acme/menu-board 1.0.0 created", res)
	}
	if got, want := res.Collections, []string{"menu_items"}; !equalStrings(got, want) {
		t.Fatalf("collections = %v, want %v", got, want)
	}
	if got, want := res.Pages, []string{"menu-items", "settings"}; !equalStrings(got, want) {
		t.Fatalf("pages = %v, want %v", got, want)
	}

	if after := gen(t, st); after != before+1 {
		t.Fatalf("generation = %d, want %d (bumped exactly once)", after, before+1)
	}

	pack, found, err := st.GetPack(ctx, "acme/menu-board")
	if err != nil || !found {
		t.Fatalf("GetPack: found=%v err=%v", found, err)
	}
	if pack.Revision != 1 || pack.DataModelVersion != 1 {
		t.Fatalf("pack revision=%d dataModelVersion=%d; want 1/1", pack.Revision, pack.DataModelVersion)
	}

	pages, _ := st.PackFileNames(ctx, "acme/menu-board", store.PackFilePage)
	if !equalStrings(pages, []string{"menu-items", "settings"}) {
		t.Fatalf("page files = %v; want [menu-items settings]", pages)
	}
	doc, ok, _ := st.GetPackFile(ctx, "acme/menu-board", store.PackFilePage, "menu-items")
	if !ok || string(doc) != menuItemsDoc {
		t.Fatalf("page doc menu-items = %q,%v; want the doc", doc, ok)
	}
	cat, ok, _ := st.GetPackFile(ctx, "acme/menu-board", store.PackFileLocale, "en")
	if !ok || string(cat) != enCatalog {
		t.Fatalf("locale en = %q,%v; want the catalog", cat, ok)
	}
}

// TestInstallBadID: an invalid pack id is refused by the manifest engine.
func TestInstallBadID(t *testing.T) {
	st := openStore(t)
	m := baseManifest()
	m["id"] = "Acme/Menu"
	_, err := packs.NewInstaller(st).Install(context.Background(), basePackZip(t, m))
	manifestFieldCode(t, err, "id", "MANIFEST_SCHEMA_INVALID")
}

// TestInstallUnknownCapability: a capability outside the host registry is refused.
func TestInstallUnknownCapability(t *testing.T) {
	st := openStore(t)
	m := baseManifest()
	m["capabilities"] = []any{
		map[string]any{"capability": "world.domination", "scope": "*", "reason": "msg:cap.x"},
	}
	_, err := packs.NewInstaller(st).Install(context.Background(), basePackZip(t, m))
	manifestFieldCode(t, err, "capabilities[0].capability", "UNKNOWN_CAPABILITY")
}

// TestInstallUnknownPageType: a compat.renderer page-type absent from the host
// page-type registry is refused (MAN-010).
func TestInstallUnknownPageType(t *testing.T) {
	st := openStore(t)
	m := baseManifest()
	m["compat"] = map[string]any{"ctx": ">=1.0 <2.0", "renderer": []any{"list-detail", "settings-form", "tarot-spread"}}
	_, err := packs.NewInstaller(st).Install(context.Background(), basePackZip(t, m))
	manifestFieldCode(t, err, "compat.renderer[2]", "UNKNOWN_PAGE_TYPE")
}

// TestInstallResourceBelowFloor: a memory request under the host floor is refused.
func TestInstallResourceBelowFloor(t *testing.T) {
	st := openStore(t)
	m := baseManifest()
	m["resources"] = map[string]any{"memory": 8, "cpuWeight": 100, "storageQuota": 16, "maxScheduledTimers": 0}
	_, err := packs.NewInstaller(st).Install(context.Background(), basePackZip(t, m))
	manifestFieldCode(t, err, "resources.memory", "RESOURCE_BELOW_FLOOR")
}

// TestInstallMissingLocaleCatalog: a bundle without messages/en.json is refused
// by the manifest engine (MAN-111) — the check is REAL because the host bundle
// file set is the zip's own entries.
func TestInstallMissingLocaleCatalog(t *testing.T) {
	st := openStore(t)
	files := basePackFiles(t, baseManifest())
	delete(files, "messages/en.json")
	_, err := packs.NewInstaller(st).Install(context.Background(), filesZip(t, files))
	manifestFieldCode(t, err, "messages", "MANIFEST_SCHEMA_INVALID")
}

// TestInstallMissingPageDoc: a declared page whose ui-schema/1 document is absent
// from the bundle (UIS-001 path match fails) is refused with a typed artifact
// error — never installed half-formed.
func TestInstallMissingPageDoc(t *testing.T) {
	st := openStore(t)
	files := basePackFiles(t, baseManifest())
	delete(files, "ui/settings.json")
	_, err := packs.NewInstaller(st).Install(context.Background(), filesZip(t, files))
	artifactCode(t, err, "PACK_PAGE_DOC_MISSING")
}

// TestInstallInvalidPageDoc: a declared page whose document is not valid JSON is
// refused (parsed only to prove well-formedness — never executed).
func TestInstallInvalidPageDoc(t *testing.T) {
	st := openStore(t)
	files := basePackFiles(t, baseManifest())
	files["ui/menu-items.json"] = "{not json"
	_, err := packs.NewInstaller(st).Install(context.Background(), filesZip(t, files))
	artifactCode(t, err, "PACK_PAGE_DOC_INVALID")
}

// TestInstallMissingManifest: an artifact without a manifest.json is refused.
func TestInstallMissingManifest(t *testing.T) {
	st := openStore(t)
	z := filesZip(t, map[string]string{"messages/en.json": enCatalog})
	_, err := packs.NewInstaller(st).Install(context.Background(), z)
	artifactCode(t, err, "PACK_MANIFEST_MISSING")
}

// TestInstallMalformedManifestJSON: a manifest.json that is not valid JSON is
// refused before the engine ever runs.
func TestInstallMalformedManifestJSON(t *testing.T) {
	st := openStore(t)
	files := basePackFiles(t, baseManifest())
	files["manifest.json"] = "{not json"
	_, err := packs.NewInstaller(st).Install(context.Background(), filesZip(t, files))
	artifactCode(t, err, "PACK_MANIFEST_INVALID")
}

// TestInstallMalformedArtifact: non-zip bytes are a typed artifact error, never
// a panic.
func TestInstallMalformedArtifact(t *testing.T) {
	st := openStore(t)
	_, err := packs.NewInstaller(st).Install(context.Background(), []byte("not a zip"))
	artifactCode(t, err, "PACK_ARTIFACT_INVALID")
}

// TestReinstallUpdatesInPlace: reinstalling an existing pack at a non-regressing
// version updates it (revision bumps, Created=false) and does not duplicate it.
func TestReinstallUpdatesInPlace(t *testing.T) {
	st := openStore(t)
	in := packs.NewInstaller(st)
	ctx := context.Background()

	if _, err := in.Install(ctx, basePackZip(t, baseManifest())); err != nil {
		t.Fatalf("first install: %v", err)
	}
	m := baseManifest()
	m["version"] = "1.1.0"
	m["dataModel"] = map[string]any{"version": 2, "collections": []any{
		map[string]any{"name": "menu_items", "fields": []any{
			map[string]any{"name": "name", "type": "string", "role": "title"},
		}},
	}}
	res, err := in.Install(ctx, basePackZip(t, m))
	if err != nil {
		t.Fatalf("reinstall: %v", err)
	}
	if res.Created {
		t.Fatalf("reinstall reported Created=true; want false (updated in place)")
	}
	packsList, _ := st.ListPacks(ctx)
	if len(packsList) != 1 {
		t.Fatalf("pack count = %d, want 1 (reinstall must not duplicate)", len(packsList))
	}
	pack, _, _ := st.GetPack(ctx, "acme/menu-board")
	if pack.Revision != 2 || pack.Version != "1.1.0" || pack.DataModelVersion != 2 {
		t.Fatalf("pack after reinstall = rev %d ver %s dmv %d; want 2/1.1.0/2", pack.Revision, pack.Version, pack.DataModelVersion)
	}
}

// TestReinstallLowerDataModelVersionRefused: MAN-053 — a reinstall whose
// dataModel.version is lower than the installed one is refused by the manifest
// engine, and the installed pack is untouched.
func TestReinstallLowerDataModelVersionRefused(t *testing.T) {
	st := openStore(t)
	in := packs.NewInstaller(st)
	ctx := context.Background()

	m := baseManifest()
	m["dataModel"] = map[string]any{"version": 3, "collections": []any{
		map[string]any{"name": "menu_items", "fields": []any{
			map[string]any{"name": "name", "type": "string", "role": "title"},
		}},
	}}
	if _, err := in.Install(ctx, basePackZip(t, m)); err != nil {
		t.Fatalf("install v3: %v", err)
	}

	lower := baseManifest() // dataModel.version 1 < installed 3
	_, err := in.Install(ctx, basePackZip(t, lower))
	manifestFieldCode(t, err, "dataModel.version", "DATAMODEL_VERSION_REGRESSION")

	pack, _, _ := st.GetPack(ctx, "acme/menu-board")
	if pack.Revision != 1 || pack.DataModelVersion != 3 {
		t.Fatalf("installed pack changed after a refused reinstall: rev %d dmv %d; want 1/3", pack.Revision, pack.DataModelVersion)
	}
}

// TestInstallRefusalWritesNothing: a manifest-refused install leaves the store
// exactly as it was — no pack row, no generation bump (no partial install).
func TestInstallRefusalWritesNothing(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	before := gen(t, st)

	m := baseManifest()
	m["id"] = "BAD/ID"
	if _, err := packs.NewInstaller(st).Install(ctx, basePackZip(t, m)); err == nil {
		t.Fatal("expected refusal")
	}
	if _, found, _ := st.GetPack(ctx, "BAD/ID"); found {
		t.Fatal("refused pack was written")
	}
	if after := gen(t, st); after != before {
		t.Fatalf("generation moved on a refused install: %d -> %d", before, after)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
