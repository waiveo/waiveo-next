package packs_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/maaxton/waiveo-next/internal/app/packs"
	"github.com/maaxton/waiveo-next/internal/app/store"
	"github.com/maaxton/waiveo-next/internal/packsig"
	"github.com/maaxton/waiveo-next/internal/shared/signhash"
)

func openStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(":memory:", store.WallClockMs)
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
	if got, want := res.Collections, []string{"menu_items", "settings"}; !equalStrings(got, want) {
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
		settingsCollection(),
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
		settingsCollection(),
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
			settingsCollection(),
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

// TestInstallRefusesSurfacePointedAtTheSignatureEnvelope: the signature
// envelope is the one entry the content digest cannot cover, so an artifact
// that reaches install carrying attacker-chosen envelope bytes must not be able
// to expose them. The envelope is dropped from the bundle the moment
// verification returns, so a manifest naming it as a UI surface entry no longer
// resolves — MAN-063 refuses it exactly as it would refuse a surface pointed at
// any file the bundle does not contain.
//
// Without the drop, this manifest installs: a validly signed artifact whose
// signature.json a mirror rewrote after signing would then be serving
// attacker-controlled bytes from inside a "verified" pack.
func TestInstallRefusesSurfacePointedAtTheSignatureEnvelope(t *testing.T) {
	st := openStore(t)
	in, signer := newInstaller(t, st)

	m := baseManifest()
	ui, _ := m["ui"].(map[string]any)
	ui["surfaces"] = []any{
		map[string]any{"name": "smuggled", "entry": packsig.EnvelopeName},
	}

	before := gen(t, st)
	_, err := in.Install(context.Background(), signedPackZip(t, signer, m))
	if err == nil {
		t.Fatal("a surface entry pointing at the signature envelope installed — the envelope is still visible to MAN-063")
	}
	var merr *packs.ManifestError
	if !errors.As(err, &merr) {
		t.Fatalf("Install error = %v (%T), want a ManifestError refusing the surface entry", err, err)
	}
	if after := gen(t, st); after != before {
		t.Fatalf("generation advanced (%d -> %d) on a refused install", before, after)
	}
}

// TestInstallRecordNamesTheKeyThatVerifiedNotJustItsLabel closes the record end
// of MKT-094a: the persisted provenance must identify the key that vouched for
// the running bytes, and key_id alone cannot.
//
// key_id is a truncated, publisher-supplied label, and packsig verifies against
// EVERY anchored key bearing it — deliberately, so a colliding id cannot let a
// shadow anchor refuse the genuine publisher. So the anchor set here holds a
// decoy and the real signer under ONE id, with the decoy first. A record built
// from the label would be satisfied by both; only the full key digest says which
// one actually verified.
//
// MKT-094a is explicit that this is the point: "a record able to name a key that
// did not verify would attest to provenance the host never established, which is
// worse than recording none."
func TestInstallRecordNamesTheKeyThatVerifiedNotJustItsLabel(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	signer := newTestSigner(t)
	decoyPub, _ := signhash.GenerateKey()

	// One id, two keys, decoy first: an installer that recorded the first
	// candidate rather than the verifying one names the wrong key.
	anchors := packsig.StaticAnchors{}
	for _, ns := range fixtureNamespaces {
		anchors[ns] = []packsig.TrustedKey{
			{KeyID: signer.keyID, PublicKey: decoyPub},
			{KeyID: signer.keyID, PublicKey: signer.pub},
		}
	}
	in := packs.NewInstaller(st, anchors)

	res, err := in.Install(ctx, signedPackZip(t, signer, baseManifest()))
	if err != nil {
		t.Fatalf("a colliding key_id must not stop the genuine publisher installing: %v", err)
	}

	recs, err := st.ListPackInstalls(ctx, res.ID)
	if err != nil {
		t.Fatalf("ListPackInstalls: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("expected exactly one install record (MKT-094b); got %d", len(recs))
	}
	rec := recs[0]

	if want := packsig.KeyDigest(signer.pub); rec.VerifyingKey != want {
		t.Errorf("install record's verifying key = %q, want the signer's digest %q", rec.VerifyingKey, want)
	}
	if rec.VerifyingKey == packsig.KeyDigest(decoyPub) {
		t.Error("the record names the DECOY anchor — provenance the host never established (MKT-094a)")
	}
	// It must be the DIGEST, not the label restated under a new column name: the
	// label is what fails to disambiguate, so recording it twice fixes nothing.
	if rec.VerifyingKey == rec.KeyID {
		t.Errorf("verifying key is the key_id %q restated, not the full key digest — the collision this exists for is unresolved", rec.KeyID)
	}
	// And key_id is still pinned, because that is what the contract mandates and
	// what a publisher and an index speak in.
	if rec.KeyID != signer.keyID {
		t.Errorf("install record's key_id = %q, want %q", rec.KeyID, signer.keyID)
	}
}

// ── MAN-064: a settings-form's source must be a declared singleton ──────────
//
// The defect this closes was observed on a running box: the example pack's
// settings page rendered, an operator filled it in, pressed Save, and got
// "Saving isn't available for this page yet." The page bound a `source` the
// manifest declared no collection for, and nothing on the install path looked.
// These tests hold the refusal at publish time, where it is cheap.

// settingsSourcePack builds a signed artifact whose settings-form page binds
// `source`, over a manifest whose collections are exactly `collections`.
func settingsSourcePack(t *testing.T, s *testSigner, source string, collections []any) []byte {
	t.Helper()
	m := baseManifest()
	m["dataModel"] = map[string]any{"version": 1, "collections": collections}
	files := basePackFiles(t, m)
	files["ui/settings.json"] = `{"pageType":"settings-form","source":"` + source + `",` +
		`"sections":[{"fields":[{"type":"text","value":"x"}]}],` +
		`"actions":[{"type":"button","on":{"press":{"verb":"submit"}}}]}`
	return signedFilesZip(t, s, files, "acme/menu-board", "1.0.0")
}

// menuItemsOnly is the collection set every arm below shares: a plain,
// non-singleton list.
func menuItemsOnly() []any {
	return []any{map[string]any{"name": "menu_items", "fields": []any{
		map[string]any{"name": "name", "type": "string", "role": "title"},
	}}}
}

func TestSettingsFormSourceNamingNoCollectionIsRefused(t *testing.T) {
	st := openStore(t)
	in, signer := newInstaller(t, st)
	_, err := in.Install(context.Background(), settingsSourcePack(t, signer, "nowhere", menuItemsOnly()))
	artifactCode(t, err, "SETTINGS_SOURCE_NOT_SINGLETON")
}

func TestSettingsFormSourceNamingAnOrdinaryCollectionIsRefused(t *testing.T) {
	st := openStore(t)
	in, signer := newInstaller(t, st)
	// The source RESOLVES — it is simply not a singleton. This arm separates "you
	// named nothing" from "you named a list", and it is the one a publisher is
	// likeliest to hit: pointing a settings-form at the same collection its
	// list-detail page pages through.
	_, err := in.Install(context.Background(), settingsSourcePack(t, signer, "menu_items", menuItemsOnly()))
	artifactCode(t, err, "SETTINGS_SOURCE_NOT_SINGLETON")
}

func TestSettingsFormSourceNamingASingletonInstalls(t *testing.T) {
	st := openStore(t)
	in, signer := newInstaller(t, st)
	cols := append(menuItemsOnly(), settingsCollection())
	if _, err := in.Install(context.Background(), settingsSourcePack(t, signer, "settings", cols)); err != nil {
		t.Fatalf("a settings-form bound to a declared singleton must install: %v", err)
	}
}

// ── MAN-065: the code-carrying tier ─────────────────────────────────────────
//
// A pack may now ship a file the host runs. The rules that matter are that the
// entry has to BE there, that the argv says how to run it rather than the host
// guessing from a file extension, and that a purely declarative pack is still
// perfectly valid — this tier is added beside that one, not over it.

// runtimePack builds an artifact declaring a runtime block.
func runtimePack(t *testing.T, s *testSigner, runtime map[string]any, withEntryFile bool) []byte {
	t.Helper()
	m := baseManifest()
	if runtime != nil {
		m["runtime"] = runtime
	}
	files := basePackFiles(t, m)
	if withEntryFile {
		files["run.js"] = "// the pack's code\n"
	}
	return signedFilesZip(t, s, files, "acme/menu-board", "1.0.0")
}

func TestACodeCarryingPackStoresItsEntryFile(t *testing.T) {
	st := openStore(t)
	in, signer := newInstaller(t, st)
	art := runtimePack(t, signer, map[string]any{"entry": "run.js", "exec": []any{"node", "$entry"}}, true)

	if _, err := in.Install(context.Background(), art); err != nil {
		t.Fatalf("a code-carrying pack must install: %v", err)
	}
	body, ok, err := st.GetPackFile(context.Background(), "acme/menu-board", store.PackFileCode, "run.js")
	if err != nil || !ok {
		t.Fatalf("the entry file was not stored: ok=%v err=%v", ok, err)
	}
	if string(body) != "// the pack's code\n" {
		t.Fatalf("stored entry = %q, want the bundled bytes verbatim", body)
	}
}

// A declarative pack is still valid. The new tier sits beside the old one; if
// omitting `runtime` ever became an error, every pack shipped so far would stop
// installing.
func TestAPackWithoutARuntimeStillInstalls(t *testing.T) {
	st := openStore(t)
	in, signer := newInstaller(t, st)
	if _, err := in.Install(context.Background(), runtimePack(t, signer, nil, false)); err != nil {
		t.Fatalf("a declarative pack must still install: %v", err)
	}
	if _, ok, _ := st.GetPackFile(context.Background(), "acme/menu-board", store.PackFileCode, "run.js"); ok {
		t.Fatal("a declarative pack stored a code file")
	}
}

// An entry naming no bundled file is refused — a pack that would install and
// have nothing to run.
func TestARuntimeEntryThatNamesNoBundledFileIsRefused(t *testing.T) {
	st := openStore(t)
	in, signer := newInstaller(t, st)
	art := runtimePack(t, signer, map[string]any{"entry": "run.js", "exec": []any{"node", "$entry"}}, false)
	_, err := in.Install(context.Background(), art)
	manifestFieldCode(t, err, "runtime.entry", "MANIFEST_SCHEMA_INVALID")
}

// The argv must say how to run the entry, and must say it unambiguously.
// Without the placeholder the host would run something other than the file the
// pack shipped; with two, the substitution has no defined meaning.
func TestTheExecArgvMustNameTheEntryExactlyOnce(t *testing.T) {
	for _, tc := range []struct {
		name string
		exec []any
	}{
		{"no placeholder at all", []any{"node", "server.js"}},
		{"placeholder twice", []any{"node", "$entry", "$entry"}},
		{"empty argv", []any{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := openStore(t)
			in, signer := newInstaller(t, st)
			art := runtimePack(t, signer, map[string]any{"entry": "run.js", "exec": tc.exec}, true)
			_, err := in.Install(context.Background(), art)
			manifestFieldCode(t, err, "runtime.exec", "MANIFEST_SCHEMA_INVALID")
		})
	}
}

// The contract is language-neutral: an argv that runs the entry directly is as
// valid as one that hands it to an interpreter. Which of the two a deployment
// can actually execute is that deployment's property, not manifest/1's.
func TestTheExecArgvIsLanguageNeutral(t *testing.T) {
	st := openStore(t)
	in, signer := newInstaller(t, st)
	art := runtimePack(t, signer, map[string]any{"entry": "run.js", "exec": []any{"$entry"}}, true)
	if _, err := in.Install(context.Background(), art); err != nil {
		t.Fatalf("a directly-executed entry must be valid (MAN-065): %v", err)
	}
}
