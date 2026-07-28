package packs_test

import (
	"context"
	"errors"
	"sync"
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

// newInstaller builds an installer wired to a fresh fixture signer whose key
// the trust anchors authorize for every namespace the manifest fixtures use —
// so a manifest-engine test exercises ITS refusal, with signature verification
// genuinely on, not bypassed.
func newInstaller(t *testing.T, st *store.Store) (*packs.Installer, *testSigner) {
	t.Helper()
	s := newTestSigner(t)
	return packs.NewInstaller(st, s.anchorsFor(fixtureNamespaces...)), s
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
	in, signer := newInstaller(t, st)
	ctx := context.Background()

	before := gen(t, st)
	res, err := in.Install(ctx, signedPackZip(t, signer, baseManifest()))
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
	in, signer := newInstaller(t, st)
	_, err := in.Install(context.Background(), signedPackZip(t, signer, m))
	manifestFieldCode(t, err, "id", "MANIFEST_SCHEMA_INVALID")
}

// TestInstallUnknownCapability: a capability outside the host registry is refused.
func TestInstallUnknownCapability(t *testing.T) {
	st := openStore(t)
	m := baseManifest()
	m["capabilities"] = []any{
		map[string]any{"capability": "world.domination", "scope": "*", "reason": "msg:cap.x"},
	}
	in, signer := newInstaller(t, st)
	_, err := in.Install(context.Background(), signedPackZip(t, signer, m))
	manifestFieldCode(t, err, "capabilities[0].capability", "UNKNOWN_CAPABILITY")
}

// TestInstallUnknownPageType: a compat.renderer page-type absent from the host
// page-type registry is refused (MAN-010).
func TestInstallUnknownPageType(t *testing.T) {
	st := openStore(t)
	m := baseManifest()
	m["compat"] = map[string]any{"ctx": ">=1.0 <2.0", "renderer": []any{"list-detail", "settings-form", "tarot-spread"}}
	in, signer := newInstaller(t, st)
	_, err := in.Install(context.Background(), signedPackZip(t, signer, m))
	manifestFieldCode(t, err, "compat.renderer[2]", "UNKNOWN_PAGE_TYPE")
}

// TestInstallResourceBelowFloor: a memory request under the host floor is refused.
func TestInstallResourceBelowFloor(t *testing.T) {
	st := openStore(t)
	m := baseManifest()
	m["resources"] = map[string]any{"memory": 8, "cpuWeight": 100, "storageQuota": 16, "maxScheduledTimers": 0}
	in, signer := newInstaller(t, st)
	_, err := in.Install(context.Background(), signedPackZip(t, signer, m))
	manifestFieldCode(t, err, "resources.memory", "RESOURCE_BELOW_FLOOR")
}

// TestInstallMissingLocaleCatalog: a bundle without messages/en.json is refused
// by the manifest engine (MAN-111) — the check is REAL because the host bundle
// file set is the zip's own entries.
func TestInstallMissingLocaleCatalog(t *testing.T) {
	st := openStore(t)
	files := basePackFiles(t, baseManifest())
	delete(files, "messages/en.json")
	in, signer := newInstaller(t, st)
	_, err := in.Install(context.Background(), signedFilesZip(t, signer, files, "acme/menu-board", "1.0.0"))
	manifestFieldCode(t, err, "messages", "MANIFEST_SCHEMA_INVALID")
}

// TestInstallMissingPageDoc: a declared page whose ui-schema/1 document is absent
// from the bundle (UIS-001 path match fails) is refused with a typed artifact
// error — never installed half-formed.
func TestInstallMissingPageDoc(t *testing.T) {
	st := openStore(t)
	files := basePackFiles(t, baseManifest())
	delete(files, "ui/settings.json")
	in, signer := newInstaller(t, st)
	_, err := in.Install(context.Background(), signedFilesZip(t, signer, files, "acme/menu-board", "1.0.0"))
	artifactCode(t, err, "PACK_PAGE_DOC_MISSING")
}

// TestInstallInvalidPageDoc: a declared page whose document is not valid JSON is
// refused (parsed only to prove well-formedness — never executed).
func TestInstallInvalidPageDoc(t *testing.T) {
	st := openStore(t)
	files := basePackFiles(t, baseManifest())
	files["ui/menu-items.json"] = "{not json"
	in, signer := newInstaller(t, st)
	_, err := in.Install(context.Background(), signedFilesZip(t, signer, files, "acme/menu-board", "1.0.0"))
	artifactCode(t, err, "PACK_PAGE_DOC_INVALID")
}

// TestInstallNonStringLocaleValue: a locale catalog that is well-formed JSON but
// carries a NON-string value (MAN-110 declares a flat {key: text} map) is refused
// at install. This is defense-in-depth for the console: a non-string catalog value
// referenced by the manifest (e.g. displayName) would otherwise flow into the nav
// title unguarded. The catalog is only parsed to prove its shape — never executed.
func TestInstallNonStringLocaleValue(t *testing.T) {
	st := openStore(t)
	files := basePackFiles(t, baseManifest())
	files["messages/en.json"] = `{"pack.displayName":{"x":1},"page.menuItems.title":"Menu Items","page.settings.title":"Settings"}`
	in, signer := newInstaller(t, st)
	_, err := in.Install(context.Background(), signedFilesZip(t, signer, files, "acme/menu-board", "1.0.0"))
	artifactCode(t, err, "PACK_LOCALE_INVALID")
}

// TestInstallNonObjectLocaleCatalog: a locale catalog whose top level is not a
// JSON object (a bare array/string/number) is likewise refused — MAN-110 is a map.
func TestInstallNonObjectLocaleCatalog(t *testing.T) {
	st := openStore(t)
	files := basePackFiles(t, baseManifest())
	files["messages/en.json"] = `["not","a","map"]`
	in, signer := newInstaller(t, st)
	_, err := in.Install(context.Background(), signedFilesZip(t, signer, files, "acme/menu-board", "1.0.0"))
	artifactCode(t, err, "PACK_LOCALE_INVALID")
}

// TestInstallMissingManifest: an artifact without a manifest.json is refused.
func TestInstallMissingManifest(t *testing.T) {
	st := openStore(t)
	in, signer := newInstaller(t, st)
	z := signedFilesZip(t, signer, map[string]string{"messages/en.json": enCatalog}, "acme/menu-board", "1.0.0")
	_, err := in.Install(context.Background(), z)
	artifactCode(t, err, "PACK_MANIFEST_MISSING")
}

// TestInstallMalformedManifestJSON: a manifest.json that is not valid JSON is
// refused before the engine ever runs.
func TestInstallMalformedManifestJSON(t *testing.T) {
	st := openStore(t)
	files := basePackFiles(t, baseManifest())
	files["manifest.json"] = "{not json"
	in, signer := newInstaller(t, st)
	_, err := in.Install(context.Background(), signedFilesZip(t, signer, files, "acme/menu-board", "1.0.0"))
	artifactCode(t, err, "PACK_MANIFEST_INVALID")
}

// TestInstallMalformedArtifact: non-zip bytes are a typed artifact error, never
// a panic.
func TestInstallMalformedArtifact(t *testing.T) {
	st := openStore(t)
	in, _ := newInstaller(t, st)
	_, err := in.Install(context.Background(), []byte("not a zip"))
	artifactCode(t, err, "PACK_ARTIFACT_INVALID")
}

// TestReinstallUpdatesInPlace: reinstalling an existing pack at a non-regressing
// version updates it (revision bumps, Created=false) and does not duplicate it.
func TestReinstallUpdatesInPlace(t *testing.T) {
	st := openStore(t)
	in, signer := newInstaller(t, st)
	ctx := context.Background()

	if _, err := in.Install(ctx, signedPackZip(t, signer, baseManifest())); err != nil {
		t.Fatalf("first install: %v", err)
	}
	m := baseManifest()
	m["version"] = "1.1.0"
	m["dataModel"] = map[string]any{"version": 2, "collections": []any{
		map[string]any{"name": "menu_items", "fields": []any{
			map[string]any{"name": "name", "type": "string", "role": "title"},
		}},
	}}
	res, err := in.Install(ctx, signedPackZip(t, signer, m))
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
	in, signer := newInstaller(t, st)
	ctx := context.Background()

	m := baseManifest()
	m["dataModel"] = map[string]any{"version": 3, "collections": []any{
		map[string]any{"name": "menu_items", "fields": []any{
			map[string]any{"name": "name", "type": "string", "role": "title"},
		}},
	}}
	if _, err := in.Install(ctx, signedPackZip(t, signer, m)); err != nil {
		t.Fatalf("install v3: %v", err)
	}

	lower := baseManifest() // dataModel.version 1 < installed 3
	_, err := in.Install(ctx, signedPackZip(t, signer, lower))
	manifestFieldCode(t, err, "dataModel.version", "DATAMODEL_VERSION_REGRESSION")

	pack, _, _ := st.GetPack(ctx, "acme/menu-board")
	if pack.Revision != 1 || pack.DataModelVersion != 3 {
		t.Fatalf("installed pack changed after a refused reinstall: rev %d dmv %d; want 1/3", pack.Revision, pack.DataModelVersion)
	}
}

// TestConcurrentInstallVersionRegressionRace: two concurrent installs of a
// not-yet-installed pack id — one declaring dataModel.version 10, one declaring
// version 1 — must NEVER leave the store at the lower version. Both Install()
// calls read installedVersion=0 in their pre-transaction snapshot (so
// manifest.Validate skips the MAN-053 check for both); the store then serializes
// the two InstallPack transactions, and whichever committed last would otherwise
// overwrite the row unconditionally, silently downgrading dataModel.version. The
// in-transaction versionRegressionGuard closes that TOCTOU race: the second
// committer sees the first's row and, if it regresses, is refused — so the final
// version is always the higher one, whatever the commit order, and neither call
// returns a spurious error when it legitimately wins. Looped so the race, which
// resolves either commit order, is exercised many times.
func TestConcurrentInstallVersionRegressionRace(t *testing.T) {
	const trials = 100
	for i := 0; i < trials; i++ {
		st := openStore(t)
		in, signer := newInstaller(t, st)
		ctx := context.Background()

		hi := baseManifest()
		hi["dataModel"] = map[string]any{"version": 10, "collections": []any{
			map[string]any{"name": "menu_items", "fields": []any{
				map[string]any{"name": "name", "type": "string", "role": "title"},
			}},
		}}
		hiZip := signedPackZip(t, signer, hi)
		loZip := signedPackZip(t, signer, baseManifest()) // dataModel.version 1

		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); _, _ = in.Install(ctx, hiZip) }()
		go func() { defer wg.Done(); _, _ = in.Install(ctx, loZip) }()
		wg.Wait()

		pack, found, err := st.GetPack(ctx, "acme/menu-board")
		if err != nil || !found {
			t.Fatalf("trial %d: GetPack found=%v err=%v", i, found, err)
		}
		if pack.DataModelVersion != 10 {
			t.Fatalf("trial %d: stored dataModel.version = %d, want 10 — a concurrent lower-version install raced past MAN-053 and downgraded the pack", i, pack.DataModelVersion)
		}
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
	in, signer := newInstaller(t, st)
	if _, err := in.Install(ctx, signedPackZip(t, signer, m)); err == nil {
		t.Fatal("expected refusal")
	}
	if _, found, _ := st.GetPack(ctx, "BAD/ID"); found {
		t.Fatal("refused pack was written")
	}
	if after := gen(t, st); after != before {
		t.Fatalf("generation moved on a refused install: %d -> %d", before, after)
	}
}

// ---- signature verification at install (marketplace/1 MKT-009a/b) ---------

// TestInstallUnsignedRefused: an artifact with no signature envelope refuses
// PACK_UNSIGNED, and NOTHING persists — no pack row, no generation bump.
func TestInstallUnsignedRefused(t *testing.T) {
	st := openStore(t)
	in, _ := newInstaller(t, st)
	ctx := context.Background()
	before := gen(t, st)

	_, err := in.Install(ctx, basePackZip(t, baseManifest()))
	artifactCode(t, err, "PACK_UNSIGNED")

	if _, found, _ := st.GetPack(ctx, "acme/menu-board"); found {
		t.Fatal("an unsigned refusal wrote a pack row")
	}
	if after := gen(t, st); after != before {
		t.Fatalf("generation moved on an unsigned refusal: %d -> %d", before, after)
	}
}

// TestInstallStrippedSignatureRefused: stripping the envelope from a validly
// signed artifact is exactly the unsigned refusal — there is no "legacy
// unsigned" path to fall back into.
func TestInstallStrippedSignatureRefused(t *testing.T) {
	st := openStore(t)
	in, signer := newInstaller(t, st)

	signed := signedPackZip(t, signer, baseManifest())
	stripped := stripEntry(t, signed, "signature.json")
	_, err := in.Install(context.Background(), stripped)
	artifactCode(t, err, "PACK_UNSIGNED")
}

// TestInstallTamperedPayloadRefused: altering a PAGE DOCUMENT after signing —
// content a manifest-only signature would never notice — breaks the content
// digest and refuses PACK_SIGNATURE_INVALID, with nothing persisted.
func TestInstallTamperedPayloadRefused(t *testing.T) {
	st := openStore(t)
	in, signer := newInstaller(t, st)
	ctx := context.Background()
	before := gen(t, st)

	signed := signedPackZip(t, signer, baseManifest())
	tampered := tamperEntry(t, signed, "ui/menu-items.json",
		`{"pageType":"list-detail","list":{"source":"menu_items","display":{"type":"table"}},"injected":true}`)
	_, err := in.Install(ctx, tampered)
	artifactCode(t, err, "PACK_SIGNATURE_INVALID")

	if _, found, _ := st.GetPack(ctx, "acme/menu-board"); found {
		t.Fatal("a tampered refusal wrote a pack row")
	}
	if after := gen(t, st); after != before {
		t.Fatalf("generation moved on a tampered refusal: %d -> %d", before, after)
	}
}

// TestInstallTamperedManifestRefused: altering the MANIFEST after signing (here:
// quietly granting itself a capability) breaks the content digest too — the
// digest covers every extracted entry, the manifest included.
func TestInstallTamperedManifestRefused(t *testing.T) {
	st := openStore(t)
	in, signer := newInstaller(t, st)

	m := baseManifest()
	signed := signedPackZip(t, signer, m)
	m["capabilities"] = []any{map[string]any{"capability": "device.command", "scope": "*", "reason": "msg:cap.x"}}
	tampered := tamperEntry(t, signed, "manifest.json", string(mustJSON(t, m)))
	_, err := in.Install(context.Background(), tampered)
	artifactCode(t, err, "PACK_SIGNATURE_INVALID")
}

// TestInstallSmuggledEntryRefused: ADDING an entry to a signed artifact is
// tampering exactly like altering one — the digest covers the whole extracted
// set, so nothing installable can ride outside the signature.
func TestInstallSmuggledEntryRefused(t *testing.T) {
	st := openStore(t)
	in, signer := newInstaller(t, st)

	signed := signedPackZip(t, signer, baseManifest())
	smuggled := tamperEntry(t, signed, "messages/xx.json", `{"k":"v"}`)
	_, err := in.Install(context.Background(), smuggled)
	artifactCode(t, err, "PACK_SIGNATURE_INVALID")
}

// TestInstallWrongKeyRefused: an artifact self-consistently signed by a key the
// trust anchors do NOT authorize refuses PACK_SIGNER_UNTRUSTED.
func TestInstallWrongKeyRefused(t *testing.T) {
	st := openStore(t)
	in, _ := newInstaller(t, st)

	rogue := newTestSigner(t) // never enters the installer's anchors
	_, err := in.Install(context.Background(), signedPackZip(t, rogue, baseManifest()))
	artifactCode(t, err, "PACK_SIGNER_UNTRUSTED")
}

// TestInstallKeyIDSpoofRefused: an envelope CLAIMING the trusted key id, with a
// signature actually produced by a different key, refuses
// PACK_SIGNATURE_INVALID — the key id is a lookup hint, the anchored public key
// is what verifies.
func TestInstallKeyIDSpoofRefused(t *testing.T) {
	st := openStore(t)
	in, signer := newInstaller(t, st)

	rogue := newTestSigner(t)
	rogue.keyID = signer.keyID // claim the trusted identity
	_, err := in.Install(context.Background(), signedPackZip(t, rogue, baseManifest()))
	artifactCode(t, err, "PACK_SIGNATURE_INVALID")
}

// TestInstallNamespaceConfusionRefused: a key trusted for one publisher
// namespace cannot vouch for a pack under ANOTHER namespace, however valid the
// signature — anchors bind keys per namespace, mirroring CHI-012's
// per-namespace delegation.
func TestInstallNamespaceConfusionRefused(t *testing.T) {
	st := openStore(t)
	signer := newTestSigner(t)
	in := packs.NewInstaller(st, signer.anchorsFor("acme")) // acme only

	m := baseManifest()
	m["id"] = "waiveo/menu-board"
	_, err := in.Install(context.Background(), signedPackZip(t, signer, m))
	artifactCode(t, err, "PACK_SIGNER_UNTRUSTED")
}

// TestInstallMislabeledIdentityRefused: a signer whose envelope names a
// DIFFERENT version than the bundled manifest is refused — the signed
// (artifact_id, version) is the identity that installs, so a valid signature
// for one version can never vouch for a row claiming another. This is the
// version-binding seam pack update/rollback will lean on.
func TestInstallMislabeledIdentityRefused(t *testing.T) {
	st := openStore(t)
	in, signer := newInstaller(t, st)

	art := signer.sign(t, basePackZip(t, baseManifest()), "acme/menu-board", "2.0.0")
	_, err := in.Install(context.Background(), art)
	artifactCode(t, err, "PACK_SIGNATURE_INVALID")

	art = signer.sign(t, basePackZip(t, baseManifest()), "acme/other-board", "1.0.0")
	_, err = in.Install(context.Background(), art)
	artifactCode(t, err, "PACK_SIGNATURE_INVALID")
}

// TestInstallEnvelopeOnlyArtifactRefused: an artifact holding nothing but a
// validly signed envelope over the empty entry set clears verification (the
// empty digest is well-defined) and then refuses for having no manifest —
// trivial content installs nothing.
func TestInstallEnvelopeOnlyArtifactRefused(t *testing.T) {
	st := openStore(t)
	in, signer := newInstaller(t, st)

	art := signedFilesZip(t, signer, map[string]string{}, "acme/menu-board", "1.0.0")
	_, err := in.Install(context.Background(), art)
	artifactCode(t, err, "PACK_MANIFEST_MISSING")
}

// TestInstallNilAnchorsFailsClosed: an installer wired with NO trust-anchor
// source refuses even a validly signed artifact — an unconfigured deployment
// can never install a pack (refuse, never default-permit).
func TestInstallNilAnchorsFailsClosed(t *testing.T) {
	st := openStore(t)
	in := packs.NewInstaller(st, nil)
	signer := newTestSigner(t)

	_, err := in.Install(context.Background(), signedPackZip(t, signer, baseManifest()))
	artifactCode(t, err, "PACK_SIGNER_UNTRUSTED")
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
