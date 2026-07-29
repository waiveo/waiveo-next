package packs_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/maaxton/waiveo-next/internal/app/packs"
	"github.com/maaxton/waiveo-next/internal/app/store"
)

// These tests drive Installer.InstallRef — the REAL entry point — never the
// resolver's internals. That is deliberate: the failure mode this whole file
// exists to catch is a surface that accepts a resolution it never actually
// performs, or performs without the verification the direct-upload path does.
// A test that called the resolver directly and asserted its return value would
// pass against exactly that bug.

// ---- fixture registry -------------------------------------------------------

// registry is a local file:// registry source: a directory holding one index
// document and the artifacts it names. It is the whole transport this
// deployment needs — a self-hosted box installs packs with no internet, so the
// pipeline is proven against a local index and the network fetcher slots in at
// the same Fetcher interface.
type registry struct {
	t       *testing.T
	dir     string
	id      string
	channel string
	entries []map[string]any
	ptrs    map[string]map[string]string
}

func newRegistry(t *testing.T, id string) *registry {
	t.Helper()
	return &registry{t: t, dir: t.TempDir(), id: id, channel: "marketplace/stable", ptrs: map[string]map[string]string{}}
}

// publish writes an artifact into the registry and adds its index entry. extra
// members (status, hold_hours, published_at, security_flagged, …) override the
// defaults, so a test states only what it is exercising.
func (r *registry) publish(artifactID, version string, artifact []byte, extra map[string]any) {
	r.t.Helper()
	name := filepath.Base(artifactID) + "-" + version + ".pack.zip"
	if err := os.WriteFile(filepath.Join(r.dir, name), artifact, 0o644); err != nil {
		r.t.Fatalf("write artifact: %v", err)
	}
	sum := sha256.Sum256(artifact)
	entry := map[string]any{
		"artifact_id":  artifactID,
		"kind":         "pack",
		"version":      version,
		"status":       "active",
		"digest":       "sha256:" + hex.EncodeToString(sum[:]),
		"size":         len(artifact),
		"download_url": "file:///" + name,
	}
	for k, v := range extra {
		entry[k] = v
	}
	r.entries = append(r.entries, entry)
}

// point sets the channel pointer for (packID, trustChannel).
func (r *registry) point(packID, trustChannel, version string) {
	if r.ptrs[packID] == nil {
		r.ptrs[packID] = map[string]string{}
	}
	r.ptrs[packID][trustChannel] = version
}

// source writes the index document and returns the configured Source.
func (r *registry) source(reserved ...string) packs.Source {
	r.t.Helper()
	pointers := []map[string]any{}
	for packID, channels := range r.ptrs {
		pointers = append(pointers, map[string]any{"pack_id": packID, "channels": channels})
	}
	doc := map[string]any{
		"signed": map[string]any{
			"role":             "targets",
			"format_version":   "1.0",
			"channel":          r.channel,
			"version":          1,
			"signing_time":     0,
			"artifacts":        r.entries,
			"channel_pointers": pointers,
		},
		"signatures": []any{},
	}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		r.t.Fatalf("marshal index: %v", err)
	}
	if err := os.WriteFile(filepath.Join(r.dir, "index.json"), raw, 0o644); err != nil {
		r.t.Fatalf("write index: %v", err)
	}
	return packs.Source{
		ID:                 r.id,
		Channel:            r.channel,
		IndexURL:           "file:///index.json",
		Fetcher:            packs.FileFetcher{Root: r.dir},
		ReservedNamespaces: reserved,
		StaleSource:        true, // a file:// source (MKT-063)
	}
}

// fixedNow is the resolution-time clock every timing-sensitive case here is
// evaluated against — hold_hours eligibility is a resolution-time decision
// (CHI-030), exercised against an injected instant rather than a sleep.
const fixedNow = int64(1_800_000_000_000)

func marketInstaller(t *testing.T, st *store.Store, sources ...packs.Source) (*packs.Installer, *testSigner) {
	t.Helper()
	s := newTestSigner(t)
	market := packs.NewMarket(func() int64 { return fixedNow }, sources...)
	return packs.NewInstaller(st, s.anchorsFor(fixtureNamespaces...), packs.WithMarketplace(market)), s
}

// noResidue asserts a refused install left nothing behind: no pack row, no
// install record, and the store generation untouched. This is the
// "refusals leave no residue" bar, checked on every refusal path below.
func noResidue(t *testing.T, st *store.Store, packID string, genBefore int64) {
	t.Helper()
	ctx := context.Background()
	if _, found, err := st.GetPack(ctx, packID); err != nil || found {
		t.Fatalf("refused install left a pack row for %s (found=%v err=%v)", packID, found, err)
	}
	recs, err := st.ListPackInstalls(ctx, packID)
	if err != nil {
		t.Fatalf("ListPackInstalls: %v", err)
	}
	if len(recs) != 0 {
		t.Fatalf("refused install left %d install record(s): %+v", len(recs), recs)
	}
	if after := gen(t, st); after != genBefore {
		t.Fatalf("refused install bumped the generation %d -> %d", genBefore, after)
	}
}

// ---- the done bar: resolve by reference, record the install -----------------

// TestInstallRefResolvesRecordsAndLists is the end-to-end shape of this work:
// a reference resolves through a local registry, the artifact installs through
// the real signature+manifest gate, and the install record answers what landed
// and which key vouched for it (MKT-094/094a).
func TestInstallRefResolvesRecordsAndLists(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	reg := newRegistry(t, "local-fixture")

	// Sign first, publish the SIGNED bytes: the digest in the index is over the
	// artifact as it will be fetched.
	signer := newTestSigner(t)
	art := signer.sign(t, basePackZip(t, baseManifest()), "acme/menu-board", "1.0.0")
	reg.publish("acme/menu-board", "1.0.0", art, nil)
	reg.point("acme/menu-board", "community", "1.0.0")

	market := packs.NewMarket(func() int64 { return fixedNow }, reg.source())
	in := packs.NewInstaller(st, signer.anchorsFor(fixtureNamespaces...), packs.WithMarketplace(market))

	before := gen(t, st)
	res, err := in.InstallRef(ctx, packs.Ref{PackID: "acme/menu-board", TrustChannel: "community"})
	if err != nil {
		t.Fatalf("InstallRef: %v", err)
	}
	if res.ID != "acme/menu-board" || res.Version != "1.0.0" || !res.Created {
		t.Fatalf("result = %+v, want acme/menu-board 1.0.0 created", res)
	}
	if after := gen(t, st); after != before+1 {
		t.Fatalf("generation = %d, want %d (the whole install is one bump)", after, before+1)
	}

	recs, err := st.ListPackInstalls(ctx, "acme/menu-board")
	if err != nil {
		t.Fatalf("ListPackInstalls: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("install records = %d, want 1", len(recs))
	}
	rec := recs[0]
	if rec.ResolvedVersion != "1.0.0" || rec.TrustChannel != "community" || rec.Source != "local-fixture" {
		t.Fatalf("record pin = %+v, want 1.0.0/community/local-fixture", rec)
	}
	if !rec.StaleSource {
		t.Fatal("a file:// resolved install must be marked stale_source (MKT-063)")
	}
	// The provenance half (MKT-094a): the key that actually verified, and the
	// content digest it signed — not anything the index or the reference said.
	if rec.KeyID != signer.keyID {
		t.Fatalf("record key_id = %q, want the verifying key %q", rec.KeyID, signer.keyID)
	}
	if rec.ContentDigest == "" || rec.ArtifactDigest == "" {
		t.Fatalf("record digests = %q / %q, want both populated", rec.ContentDigest, rec.ArtifactDigest)
	}
	if rec.ContentDigest == rec.ArtifactDigest {
		t.Fatal("content_digest (the signed extracted-entry-set digest) and artifact_digest (the entry's own transport digest) are different quantities; they must not be conflated")
	}

	// A second install appends rather than replaces (MKT-094b): history, not a pin.
	if _, err := in.InstallRef(ctx, packs.Ref{PackID: "acme/menu-board", TrustChannel: "community"}); err != nil {
		t.Fatalf("reinstall: %v", err)
	}
	recs, _ = st.ListPackInstalls(ctx, "acme/menu-board")
	if len(recs) != 2 {
		t.Fatalf("install records after reinstall = %d, want 2 (append-only)", len(recs))
	}
	if recs[0].RecordID >= recs[1].RecordID {
		t.Fatal("install records must be id-ascending so the newest record is the current pin")
	}
}

// TestDirectInstallRecordsProvenanceWithNoChannel: the record is a property of
// EVERY install, not only a resolved one (MKT-094a's direct-path clause). A
// direct upload records its verified provenance, names the direct path, and
// pins no trust channel — so nothing can later auto-track it.
func TestDirectInstallRecordsProvenanceWithNoChannel(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	in, signer := newInstaller(t, st)

	if _, err := in.Install(ctx, signedPackZip(t, signer, baseManifest())); err != nil {
		t.Fatalf("Install: %v", err)
	}
	recs, err := st.ListPackInstalls(ctx, "acme/menu-board")
	if err != nil || len(recs) != 1 {
		t.Fatalf("ListPackInstalls = %d records, err=%v; want 1", len(recs), err)
	}
	rec := recs[0]
	if rec.Source != store.SourceDirect {
		t.Fatalf("record source = %q, want %q", rec.Source, store.SourceDirect)
	}
	if rec.TrustChannel != "" {
		t.Fatalf("record trust_channel = %q; a direct install pins none (MKT-094a)", rec.TrustChannel)
	}
	if rec.ArtifactDigest != "" {
		t.Fatalf("record artifact_digest = %q; no index entry named this install", rec.ArtifactDigest)
	}
	if rec.KeyID != signer.keyID || rec.ContentDigest == "" {
		t.Fatalf("record provenance = %q/%q, want the verifying key and its signed content digest", rec.KeyID, rec.ContentDigest)
	}
	// No channel pinned means no anti-rollback mark for any channel.
	for _, ch := range []string{"community", "verified", "first-party", "dev"} {
		if _, found, err := st.PackChannelMark(ctx, "acme/menu-board", ch); err != nil || found {
			t.Fatalf("direct install marked the %s channel (found=%v err=%v)", ch, found, err)
		}
	}
}

// ---- the index is a locator, not a trust root -------------------------------

// TestIndexCannotSmuggleAnUntrustedSigner: the registry vouches for an artifact
// signed by a key no trust anchor authorizes. The index digest matches, the
// entry is well-formed, the source is configured — and the install still
// refuses, because the packsig gate runs on the resolved bytes exactly as it
// does on an uploaded one. This is the whole "resolution never grants trust"
// claim, driven rather than asserted in a comment.
func TestIndexCannotSmuggleAnUntrustedSigner(t *testing.T) {
	st := openStore(t)
	reg := newRegistry(t, "local-fixture")

	// The registry's artifact is signed by a DIFFERENT publisher key than the
	// one the host anchors.
	rogue := newTestSigner(t)
	art := rogue.sign(t, basePackZip(t, baseManifest()), "acme/menu-board", "1.0.0")
	reg.publish("acme/menu-board", "1.0.0", art, nil)
	reg.point("acme/menu-board", "community", "1.0.0")

	in, _ := marketInstaller(t, st, reg.source()) // anchors a different key
	before := gen(t, st)
	_, err := in.InstallRef(context.Background(), packs.Ref{PackID: "acme/menu-board", TrustChannel: "community"})
	artifactCode(t, err, "PACK_SIGNER_UNTRUSTED")
	noResidue(t, st, "acme/menu-board", before)
}

// TestIndexCannotSmuggleAnUnsignedArtifact: an index entry pointing at an
// artifact with no signature envelope refuses PACK_UNSIGNED — there is no
// "the registry vouched for it" downgrade path.
func TestIndexCannotSmuggleAnUnsignedArtifact(t *testing.T) {
	st := openStore(t)
	reg := newRegistry(t, "local-fixture")
	reg.publish("acme/menu-board", "1.0.0", basePackZip(t, baseManifest()), nil)
	reg.point("acme/menu-board", "community", "1.0.0")

	in, _ := marketInstaller(t, st, reg.source())
	before := gen(t, st)
	_, err := in.InstallRef(context.Background(), packs.Ref{PackID: "acme/menu-board", TrustChannel: "community"})
	artifactCode(t, err, "PACK_UNSIGNED")
	noResidue(t, st, "acme/menu-board", before)
}

// TestEntryDigestMustMatchTheArtifact: the entry says digest X, the bytes at
// download_url are Y. Refused before the artifact ever reaches the install gate
// (CHI-021).
func TestEntryDigestMustMatchTheArtifact(t *testing.T) {
	st := openStore(t)
	reg := newRegistry(t, "local-fixture")
	signer := newTestSigner(t)
	art := signer.sign(t, basePackZip(t, baseManifest()), "acme/menu-board", "1.0.0")
	reg.publish("acme/menu-board", "1.0.0", art, map[string]any{
		"digest": "sha256:" + hex.EncodeToString(make([]byte, 32)),
	})
	reg.point("acme/menu-board", "community", "1.0.0")

	market := packs.NewMarket(func() int64 { return fixedNow }, reg.source())
	in := packs.NewInstaller(st, signer.anchorsFor(fixtureNamespaces...), packs.WithMarketplace(market))
	before := gen(t, st)
	_, err := in.InstallRef(context.Background(), packs.Ref{PackID: "acme/menu-board", TrustChannel: "community"})
	artifactCode(t, err, "DIGEST_MISMATCH")
	noResidue(t, st, "acme/menu-board", before)
}

// TestEntrySizeMustMatchTheArtifact: size and digest are two checks, not one
// (CHI-023) — a wrong size refuses on its own terms.
func TestEntrySizeMustMatchTheArtifact(t *testing.T) {
	st := openStore(t)
	reg := newRegistry(t, "local-fixture")
	signer := newTestSigner(t)
	art := signer.sign(t, basePackZip(t, baseManifest()), "acme/menu-board", "1.0.0")
	reg.publish("acme/menu-board", "1.0.0", art, map[string]any{"size": len(art) + 1})
	reg.point("acme/menu-board", "community", "1.0.0")

	market := packs.NewMarket(func() int64 { return fixedNow }, reg.source())
	in := packs.NewInstaller(st, signer.anchorsFor(fixtureNamespaces...), packs.WithMarketplace(market))
	before := gen(t, st)
	_, err := in.InstallRef(context.Background(), packs.Ref{PackID: "acme/menu-board", TrustChannel: "community"})
	artifactCode(t, err, "SIZE_MISMATCH")
	noResidue(t, st, "acme/menu-board", before)
}

// TestResolvedIdentityMustMatchTheSignedIdentity is MKT-040a's attack: the
// registry lists a GENUINE, correctly-anchored artifact for acme/menu-board
// 1.0.0 under an entry claiming acme/other-pack 9.9.9. The digest matches (they
// are the same bytes), the signature verifies, the key is anchored — every
// check that exists without MKT-040a passes. What would land is a pack whose
// identity is not the one the reference, the record and the mark were keyed on.
func TestResolvedIdentityMustMatchTheSignedIdentity(t *testing.T) {
	st := openStore(t)
	reg := newRegistry(t, "local-fixture")
	signer := newTestSigner(t)
	art := signer.sign(t, basePackZip(t, baseManifest()), "acme/menu-board", "1.0.0")

	// Publish the real artifact's bytes under a DIFFERENT claimed identity.
	reg.publish("acme/other-pack", "9.9.9", art, nil)
	reg.point("acme/other-pack", "community", "9.9.9")

	market := packs.NewMarket(func() int64 { return fixedNow }, reg.source())
	in := packs.NewInstaller(st, signer.anchorsFor(fixtureNamespaces...), packs.WithMarketplace(market))
	before := gen(t, st)
	_, err := in.InstallRef(context.Background(), packs.Ref{PackID: "acme/other-pack", TrustChannel: "community"})
	artifactCode(t, err, "RESOLVED_IDENTITY_MISMATCH")
	// NEITHER identity installed — not the claimed one, not the signed one.
	noResidue(t, st, "acme/other-pack", before)
	noResidue(t, st, "acme/menu-board", before)
}

// ---- anti-rollback (MKT-050 / MKT-050a / MKT-094b) --------------------------

// TestChannelPointerCannotWalkAVersionBack: a validly-signed index whose
// pointer names an older version is refused, not followed.
func TestChannelPointerCannotWalkAVersionBack(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	signer := newTestSigner(t)

	m2 := baseManifest()
	m2["version"] = "2.0.0"
	newer := signer.sign(t, basePackZip(t, m2), "acme/menu-board", "2.0.0")
	older := signer.sign(t, basePackZip(t, baseManifest()), "acme/menu-board", "1.0.0")

	reg := newRegistry(t, "local-fixture")
	reg.publish("acme/menu-board", "2.0.0", newer, nil)
	reg.publish("acme/menu-board", "1.0.0", older, nil)
	reg.point("acme/menu-board", "community", "2.0.0")
	src := reg.source()

	market := packs.NewMarket(func() int64 { return fixedNow }, src)
	in := packs.NewInstaller(st, signer.anchorsFor(fixtureNamespaces...), packs.WithMarketplace(market))
	if _, err := in.InstallRef(ctx, packs.Ref{PackID: "acme/menu-board", TrustChannel: "community"}); err != nil {
		t.Fatalf("install 2.0.0: %v", err)
	}
	if mark, found, _ := st.PackChannelMark(ctx, "acme/menu-board", "community"); !found || mark != "2.0.0" {
		t.Fatalf("high-water mark = %q found=%v, want 2.0.0", mark, found)
	}

	// The registry now points the channel BACK at 1.0.0 — a fresh, entirely
	// valid index naming a still-signed, non-yanked older version.
	reg.point("acme/menu-board", "community", "1.0.0")
	market2 := packs.NewMarket(func() int64 { return fixedNow }, reg.source())
	in2 := packs.NewInstaller(st, signer.anchorsFor(fixtureNamespaces...), packs.WithMarketplace(market2))
	_, err := in2.InstallRef(ctx, packs.Ref{PackID: "acme/menu-board", TrustChannel: "community"})
	artifactCode(t, err, "POINTER_ROLLBACK_REJECTED")

	pack, _, _ := st.GetPack(ctx, "acme/menu-board")
	if pack.Version != "2.0.0" {
		t.Fatalf("installed version = %q after a refused downgrade, want 2.0.0", pack.Version)
	}
}

// TestUninstallDoesNotClearTheAntiRollbackMark is MKT-094b's asymmetry, and the
// attack it closes: uninstall removes the pack and its records, but NOT the
// verifier's memory of what it has ever resolved — otherwise
// uninstall-then-reinstall is a downgrade path in which every step succeeds.
func TestUninstallDoesNotClearTheAntiRollbackMark(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	signer := newTestSigner(t)

	m2 := baseManifest()
	m2["version"] = "2.0.0"
	reg := newRegistry(t, "local-fixture")
	reg.publish("acme/menu-board", "2.0.0", signer.sign(t, basePackZip(t, m2), "acme/menu-board", "2.0.0"), nil)
	reg.publish("acme/menu-board", "1.0.0", signer.sign(t, basePackZip(t, baseManifest()), "acme/menu-board", "1.0.0"), nil)
	reg.point("acme/menu-board", "community", "2.0.0")

	market := packs.NewMarket(func() int64 { return fixedNow }, reg.source())
	in := packs.NewInstaller(st, signer.anchorsFor(fixtureNamespaces...), packs.WithMarketplace(market))
	if _, err := in.InstallRef(ctx, packs.Ref{PackID: "acme/menu-board", TrustChannel: "community"}); err != nil {
		t.Fatalf("install 2.0.0: %v", err)
	}

	pack, _, _ := st.GetPack(ctx, "acme/menu-board")
	if err := st.UninstallPack(ctx, "acme/menu-board", pack.Revision); err != nil {
		t.Fatalf("UninstallPack: %v", err)
	}
	// Pack-owned state went with the pack.
	if recs, _ := st.ListPackInstalls(ctx, "acme/menu-board"); len(recs) != 0 {
		t.Fatalf("uninstall left %d install record(s)", len(recs))
	}
	// Verifier state did not.
	if mark, found, _ := st.PackChannelMark(ctx, "acme/menu-board", "community"); !found || mark != "2.0.0" {
		t.Fatalf("high-water mark after uninstall = %q found=%v, want 2.0.0 (MKT-094b)", mark, found)
	}

	reg.point("acme/menu-board", "community", "1.0.0")
	market2 := packs.NewMarket(func() int64 { return fixedNow }, reg.source())
	in2 := packs.NewInstaller(st, signer.anchorsFor(fixtureNamespaces...), packs.WithMarketplace(market2))
	_, err := in2.InstallRef(ctx, packs.Ref{PackID: "acme/menu-board", TrustChannel: "community"})
	artifactCode(t, err, "POINTER_ROLLBACK_REJECTED")
}

// TestVersionOrderIsNumericNotLexicographic pins MKT-050a directly: the exact
// comparison a string implementation gets wrong.
func TestVersionOrderIsNumericNotLexicographic(t *testing.T) {
	if !packs.VersionHigher("1.10.0", "1.9.0") {
		t.Fatal("1.10.0 must rank above 1.9.0 (component-wise numeric, MKT-050a)")
	}
	if packs.VersionHigher("1.9.0", "1.10.0") {
		t.Fatal("1.9.0 must NOT rank above 1.10.0 — that is the lexicographic inversion MKT-050a forbids")
	}
	if packs.VersionHigher("1.2.3", "1.2.3") {
		t.Fatal("an equal version is not higher")
	}
	if packs.VersionLower("1.2.3", "1.2.3") {
		t.Fatal("an equal version is not lower (MKT-050 refuses only a LOWER version)")
	}
	for _, bad := range []string{"1.2", "1.2.3.4", "v1.2.3", "1.2.3-rc1", "", "1.2.x"} {
		if packs.ValidVersion(bad) {
			t.Fatalf("%q must not be a valid pack version (MAN-002)", bad)
		}
		if packs.VersionHigher(bad, "1.0.0") {
			t.Fatalf("%q has no position in the version order and must never rank above anything", bad)
		}
	}
}

// TestPointerNamingANonManifestVersionIsUnresolvable: a pointer value outside
// MAN-002's grammar has no relation to the high-water mark, so it is not
// resolvable rather than compared under some fallback (MKT-050a).
func TestPointerNamingANonManifestVersionIsUnresolvable(t *testing.T) {
	st := openStore(t)
	reg := newRegistry(t, "local-fixture")
	signer := newTestSigner(t)
	reg.publish("acme/menu-board", "1.0.0", signer.sign(t, basePackZip(t, baseManifest()), "acme/menu-board", "1.0.0"), nil)
	reg.point("acme/menu-board", "community", "1.0.0-rc1")

	market := packs.NewMarket(func() int64 { return fixedNow }, reg.source())
	in := packs.NewInstaller(st, signer.anchorsFor(fixtureNamespaces...), packs.WithMarketplace(market))
	_, err := in.InstallRef(context.Background(), packs.Ref{PackID: "acme/menu-board", TrustChannel: "community"})
	artifactCode(t, err, "MARKETPLACE_REF_UNRESOLVED")
}

// ---- entry status, holds, kinds ---------------------------------------------

func TestYankedEntryIsNotResolvable(t *testing.T) {
	st := openStore(t)
	reg := newRegistry(t, "local-fixture")
	signer := newTestSigner(t)
	reg.publish("acme/menu-board", "1.0.0", signer.sign(t, basePackZip(t, baseManifest()), "acme/menu-board", "1.0.0"),
		map[string]any{"status": "yanked"})
	reg.point("acme/menu-board", "community", "1.0.0")

	market := packs.NewMarket(func() int64 { return fixedNow }, reg.source())
	in := packs.NewInstaller(st, signer.anchorsFor(fixtureNamespaces...), packs.WithMarketplace(market))
	before := gen(t, st)
	_, err := in.InstallRef(context.Background(), packs.Ref{PackID: "acme/menu-board", TrustChannel: "community"})
	artifactCode(t, err, "ARTIFACT_YANKED")
	noResidue(t, st, "acme/menu-board", before)
}

// TestArchivedEntryResolvesOnlyByExplicitVersion: MKT-044's distinction —
// withdrawn from discovery (so a pointer may not land on it), still resolvable
// for a reinstall of that exact version.
func TestArchivedEntryResolvesOnlyByExplicitVersion(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	reg := newRegistry(t, "local-fixture")
	signer := newTestSigner(t)
	reg.publish("acme/menu-board", "1.0.0", signer.sign(t, basePackZip(t, baseManifest()), "acme/menu-board", "1.0.0"),
		map[string]any{"status": "archived"})
	reg.point("acme/menu-board", "community", "1.0.0")

	market := packs.NewMarket(func() int64 { return fixedNow }, reg.source())
	in := packs.NewInstaller(st, signer.anchorsFor(fixtureNamespaces...), packs.WithMarketplace(market))

	_, err := in.InstallRef(ctx, packs.Ref{PackID: "acme/menu-board", TrustChannel: "community"})
	artifactCode(t, err, "MARKETPLACE_REF_UNRESOLVED")

	if _, err := in.InstallRef(ctx, packs.Ref{PackID: "acme/menu-board", TrustChannel: "community", Version: "1.0.0"}); err != nil {
		t.Fatalf("explicit-version resolve of an archived entry: %v", err)
	}
}

// TestUnrecognizedStatusIsNotTreatedAsActive pins CHI-031's floor: an unknown
// status is not-resolvable, never quietly `active`.
func TestUnrecognizedStatusIsNotTreatedAsActive(t *testing.T) {
	st := openStore(t)
	reg := newRegistry(t, "local-fixture")
	signer := newTestSigner(t)
	reg.publish("acme/menu-board", "1.0.0", signer.sign(t, basePackZip(t, baseManifest()), "acme/menu-board", "1.0.0"),
		map[string]any{"status": "promoted"})
	reg.point("acme/menu-board", "community", "1.0.0")

	market := packs.NewMarket(func() int64 { return fixedNow }, reg.source())
	in := packs.NewInstaller(st, signer.anchorsFor(fixtureNamespaces...), packs.WithMarketplace(market))
	_, err := in.InstallRef(context.Background(), packs.Ref{PackID: "acme/menu-board", TrustChannel: "community"})
	artifactCode(t, err, "MARKETPLACE_REF_UNRESOLVED")
}

// TestHoldHoursGatesResolutionAndSecurityFlagDoesNotZeroIt covers CHI-029/030
// via MKT-042, plus MKT-043's tightening: a `security_flagged` marking carried
// by nothing but the routine publish path does NOT skip the hold.
func TestHoldHoursGatesResolutionAndSecurityFlagDoesNotZeroIt(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	signer := newTestSigner(t)
	art := signer.sign(t, basePackZip(t, baseManifest()), "acme/menu-board", "1.0.0")

	// Published one hour ago with a 24-hour hold: inside the hold at fixedNow.
	held := map[string]any{"published_at": fixedNow - 3600_000, "hold_hours": 24}

	reg := newRegistry(t, "local-fixture")
	reg.publish("acme/menu-board", "1.0.0", art, held)
	reg.point("acme/menu-board", "community", "1.0.0")
	market := packs.NewMarket(func() int64 { return fixedNow }, reg.source())
	in := packs.NewInstaller(st, signer.anchorsFor(fixtureNamespaces...), packs.WithMarketplace(market))
	_, err := in.InstallRef(ctx, packs.Ref{PackID: "acme/menu-board", TrustChannel: "community"})
	artifactCode(t, err, "MARKETPLACE_REF_UNRESOLVED")

	// Same entry, now marked security_flagged by the routine publish path.
	flagged := newRegistry(t, "local-fixture")
	held["security_flagged"] = true
	flagged.publish("acme/menu-board", "1.0.0", art, held)
	flagged.point("acme/menu-board", "community", "1.0.0")
	market2 := packs.NewMarket(func() int64 { return fixedNow }, flagged.source())
	in2 := packs.NewInstaller(st, signer.anchorsFor(fixtureNamespaces...), packs.WithMarketplace(market2))
	_, err = in2.InstallRef(ctx, packs.Ref{PackID: "acme/menu-board", TrustChannel: "community"})
	artifactCode(t, err, "HOLD_HOURS_ZERO_UNAUTHENTICATED")

	// Once the hold has genuinely elapsed the same entry resolves.
	elapsed := newRegistry(t, "local-fixture")
	elapsed.publish("acme/menu-board", "1.0.0", art, map[string]any{
		"published_at": fixedNow - 25*3600_000, "hold_hours": 24,
	})
	elapsed.point("acme/menu-board", "community", "1.0.0")
	market3 := packs.NewMarket(func() int64 { return fixedNow }, elapsed.source())
	in3 := packs.NewInstaller(st, signer.anchorsFor(fixtureNamespaces...), packs.WithMarketplace(market3))
	if _, err := in3.InstallRef(ctx, packs.Ref{PackID: "acme/menu-board", TrustChannel: "community"}); err != nil {
		t.Fatalf("install after the hold elapsed: %v", err)
	}
}

func TestUnknownKindIsRefused(t *testing.T) {
	st := openStore(t)
	reg := newRegistry(t, "local-fixture")
	signer := newTestSigner(t)
	reg.publish("acme/menu-board", "1.0.0", signer.sign(t, basePackZip(t, baseManifest()), "acme/menu-board", "1.0.0"),
		map[string]any{"kind": "firmware-blob"})
	reg.point("acme/menu-board", "community", "1.0.0")

	market := packs.NewMarket(func() int64 { return fixedNow }, reg.source())
	in := packs.NewInstaller(st, signer.anchorsFor(fixtureNamespaces...), packs.WithMarketplace(market))
	_, err := in.InstallRef(context.Background(), packs.Ref{PackID: "acme/menu-board", TrustChannel: "community"})
	artifactCode(t, err, "PACK_KIND_UNKNOWN")
}

// ---- reference validation (MKT-060a) ----------------------------------------

func TestRefValidationRefusesBeforeAnySourceIsConsulted(t *testing.T) {
	st := openStore(t)
	// No sources at all: every case below must refuse on the reference itself,
	// which is only possible if validation runs first.
	in, _ := marketInstaller(t, st)
	ctx := context.Background()

	for _, tc := range []struct {
		name string
		ref  packs.Ref
		code string
	}{
		{"no trust channel", packs.Ref{PackID: "acme/menu-board"}, "TRUST_CHANNEL_UNKNOWN"},
		{"unknown trust channel", packs.Ref{PackID: "acme/menu-board", TrustChannel: "beta"}, "TRUST_CHANNEL_UNKNOWN"},
		{"unqualified pack id", packs.Ref{PackID: "menu-board", TrustChannel: "community"}, "CROSS_PACK_REFERENCE_UNQUALIFIED"},
		{"waiveo must be first-party", packs.Ref{PackID: "waiveo/menu-board", TrustChannel: "community"}, "FIRST_PARTY_CHANNEL_NAMESPACE_MISMATCH"},
		{"first-party is waiveo's alone", packs.Ref{PackID: "acme/menu-board", TrustChannel: "first-party"}, "FIRST_PARTY_CHANNEL_NAMESPACE_MISMATCH"},
		{"dev channel needs a dev namespace", packs.Ref{PackID: "acme/menu-board", TrustChannel: "dev"}, "DEV_CHANNEL_RESERVED_NAMESPACE"},
		{"non-MAN-002 pinned version", packs.Ref{PackID: "acme/menu-board", TrustChannel: "community", Version: "1.0"}, "MARKETPLACE_REF_INVALID"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := in.InstallRef(ctx, tc.ref)
			artifactCode(t, err, tc.code)
		})
	}
}

func TestUnknownSourceIsRefusedRatherThanFetched(t *testing.T) {
	st := openStore(t)
	reg := newRegistry(t, "local-fixture")
	signer := newTestSigner(t)
	reg.publish("acme/menu-board", "1.0.0", signer.sign(t, basePackZip(t, baseManifest()), "acme/menu-board", "1.0.0"), nil)
	reg.point("acme/menu-board", "community", "1.0.0")

	market := packs.NewMarket(func() int64 { return fixedNow }, reg.source())
	in := packs.NewInstaller(st, signer.anchorsFor(fixtureNamespaces...), packs.WithMarketplace(market))
	_, err := in.InstallRef(context.Background(), packs.Ref{
		PackID: "acme/menu-board", TrustChannel: "community", Source: "somewhere-else"})
	artifactCode(t, err, "MARKETPLACE_REF_INVALID")
}

// ---- reserved namespaces + source order (MKT-061/062) -----------------------

// TestReservedNamespaceNeedsAnAuthorizedSource: a source is not entitled to a
// reserved namespace merely by being configured (MKT-062), and authorizing it
// is a host decision, not something the index can assert about itself.
func TestReservedNamespaceNeedsAnAuthorizedSource(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	signer := newTestSigner(t)
	m := baseManifest()
	m["id"] = "waiveo/menu-board"
	art := signer.sign(t, basePackZip(t, m), "waiveo/menu-board", "1.0.0")

	reg := newRegistry(t, "local-fixture")
	reg.publish("waiveo/menu-board", "1.0.0", art, nil)
	reg.point("waiveo/menu-board", "first-party", "1.0.0")

	// Not authorized for `waiveo`.
	market := packs.NewMarket(func() int64 { return fixedNow }, reg.source())
	in := packs.NewInstaller(st, signer.anchorsFor(fixtureNamespaces...), packs.WithMarketplace(market))
	_, err := in.InstallRef(ctx, packs.Ref{PackID: "waiveo/menu-board", TrustChannel: "first-party"})
	artifactCode(t, err, "REGISTRY_SOURCE_NOT_DELEGATED")

	// Authorized by host configuration: the same index now resolves.
	market2 := packs.NewMarket(func() int64 { return fixedNow }, reg.source("waiveo"))
	in2 := packs.NewInstaller(st, signer.anchorsFor(fixtureNamespaces...), packs.WithMarketplace(market2))
	if _, err := in2.InstallRef(ctx, packs.Ref{PackID: "waiveo/menu-board", TrustChannel: "first-party"}); err != nil {
		t.Fatalf("install from an authorized source: %v", err)
	}
}

// TestSourceOrderIsPreferenceNotTrust: a source that simply has nothing falls
// through to the next (order as preference), but a source that REFUSES for a
// security reason stops the walk — otherwise a later source could answer a
// question an earlier one refused, which is source-order-as-trust.
func TestSourceOrderIsPreferenceNotTrust(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	signer := newTestSigner(t)
	art := signer.sign(t, basePackZip(t, baseManifest()), "acme/menu-board", "1.0.0")

	empty := newRegistry(t, "empty-first")
	stocked := newRegistry(t, "stocked-second")
	stocked.publish("acme/menu-board", "1.0.0", art, nil)
	stocked.point("acme/menu-board", "community", "1.0.0")

	market := packs.NewMarket(func() int64 { return fixedNow }, empty.source(), stocked.source())
	in := packs.NewInstaller(st, signer.anchorsFor(fixtureNamespaces...), packs.WithMarketplace(market))
	if _, err := in.InstallRef(ctx, packs.Ref{PackID: "acme/menu-board", TrustChannel: "community"}); err != nil {
		t.Fatalf("resolve past an empty first source: %v", err)
	}
	recs, _ := st.ListPackInstalls(ctx, "acme/menu-board")
	if len(recs) != 1 || recs[0].Source != "stocked-second" {
		t.Fatalf("record source = %+v, want the source that actually served it", recs)
	}

	// Now the FIRST source yanks the version. A fall-through would let the
	// second source serve it anyway.
	st2 := openStore(t)
	yanking := newRegistry(t, "yanking-first")
	yanking.publish("acme/menu-board", "1.0.0", art, map[string]any{"status": "yanked"})
	yanking.point("acme/menu-board", "community", "1.0.0")
	market2 := packs.NewMarket(func() int64 { return fixedNow }, yanking.source(), stocked.source())
	in2 := packs.NewInstaller(st2, signer.anchorsFor(fixtureNamespaces...), packs.WithMarketplace(market2))
	_, err := in2.InstallRef(ctx, packs.Ref{PackID: "acme/menu-board", TrustChannel: "community"})
	artifactCode(t, err, "ARTIFACT_YANKED")
}

// ---- transport --------------------------------------------------------------

// TestFileFetcherRefusesEscapingTheRegistryRoot: download_url is an
// index-author-chosen value, and this fetcher is where it becomes a filesystem
// path. Unconfined, that is a read-any-file primitive for anyone who can write
// an index.
func TestFileFetcherRefusesEscapingTheRegistryRoot(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("not yours"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "inside.txt"), []byte("ok"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	f := packs.FileFetcher{Root: root}

	if got, err := f.Fetch(context.Background(), "file:///inside.txt"); err != nil || string(got) != "ok" {
		t.Fatalf("in-root fetch = %q, %v; want ok", got, err)
	}
	for _, bad := range []string{
		"file:///../" + filepath.Base(filepath.Dir(outside)) + "/secret.txt",
		"file://" + outside,
		"file:///../../etc/passwd",
		"https://example.invalid/artifact.zip",
	} {
		if _, err := f.Fetch(context.Background(), bad); err == nil {
			t.Fatalf("fetch %q succeeded; it must be refused", bad)
		}
	}
}

// TestNoMarketplaceConfiguredRefusesTheReference: a deployment with no registry
// does not invent one.
func TestNoMarketplaceConfiguredRefusesTheReference(t *testing.T) {
	st := openStore(t)
	in, _ := newInstaller(t, st) // no WithMarketplace
	_, err := in.InstallRef(context.Background(), packs.Ref{PackID: "acme/menu-board", TrustChannel: "community"})
	artifactCode(t, err, "MARKETPLACE_REF_UNRESOLVED")
}

// TestChannelSwitchCannotWalkTheInstalledPackBack: the MKT-050 mark is keyed
// (pack, trust channel), but there is only ONE installed pack row per id — so a
// per-channel-only check leaves a downgrade path in which every step succeeds.
// Install 2.0.0 from one channel, then follow a DIFFERENT channel's pointer at
// 1.0.0: that pair has no mark, so the per-channel check passes vacuously.
// The installed version is the floor that actually protects the box.
func TestChannelSwitchCannotWalkTheInstalledPackBack(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	signer := newTestSigner(t)

	m2 := baseManifest()
	m2["version"] = "2.0.0"
	newer := signer.sign(t, basePackZip(t, m2), "acme/menu-board", "2.0.0")
	older := signer.sign(t, basePackZip(t, baseManifest()), "acme/menu-board", "1.0.0")

	reg := newRegistry(t, "local-fixture")
	reg.publish("acme/menu-board", "2.0.0", newer, nil)
	reg.publish("acme/menu-board", "1.0.0", older, nil)
	reg.point("acme/menu-board", "verified", "2.0.0")
	reg.point("acme/menu-board", "community", "1.0.0")

	market := packs.NewMarket(func() int64 { return fixedNow }, reg.source())
	in := packs.NewInstaller(st, signer.anchorsFor(fixtureNamespaces...), packs.WithMarketplace(market))

	if _, err := in.InstallRef(ctx, packs.Ref{PackID: "acme/menu-board", TrustChannel: "verified"}); err != nil {
		t.Fatalf("install 2.0.0 on verified: %v", err)
	}

	// The community pointer names a genuine, signed, non-yanked 1.0.0, and that
	// pair carries no mark at all.
	_, err := in.InstallRef(ctx, packs.Ref{PackID: "acme/menu-board", TrustChannel: "community"})
	artifactCode(t, err, "POINTER_ROLLBACK_REJECTED")

	pack, _, _ := st.GetPack(ctx, "acme/menu-board")
	if pack.Version != "2.0.0" {
		t.Fatalf("installed version = %q after a refused cross-channel pointer, want 2.0.0", pack.Version)
	}
}

// TestConcurrentPointerInstallsCannotLandBelowTheMark: the resolver reads the
// mark OUTSIDE the write lock, so two concurrent pointer resolutions can both
// pass that check. Without an in-transaction re-assertion the later committer
// leaves the box running a version BELOW the mark — and every subsequent
// pointer resolution to that version is then refused against a floor the
// running install is already under. Same race class the dataModel guard closes
// one function away.
func TestConcurrentPointerInstallsCannotLandBelowTheMark(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	signer := newTestSigner(t)

	m2 := baseManifest()
	m2["version"] = "2.0.0"
	newer := signer.sign(t, basePackZip(t, m2), "acme/menu-board", "2.0.0")
	older := signer.sign(t, basePackZip(t, baseManifest()), "acme/menu-board", "1.0.0")

	regNew := newRegistry(t, "local-fixture")
	regNew.publish("acme/menu-board", "2.0.0", newer, nil)
	regNew.point("acme/menu-board", "community", "2.0.0")
	regOld := newRegistry(t, "local-fixture")
	regOld.publish("acme/menu-board", "1.0.0", older, nil)
	regOld.point("acme/menu-board", "community", "1.0.0")

	anchors := signer.anchorsFor(fixtureNamespaces...)
	inNew := packs.NewInstaller(st, anchors, packs.WithMarketplace(packs.NewMarket(func() int64 { return fixedNow }, regNew.source())))
	inOld := packs.NewInstaller(st, anchors, packs.WithMarketplace(packs.NewMarket(func() int64 { return fixedNow }, regOld.source())))

	// Repeated: the interleaving that exposes the race (both resolutions read
	// the mark before either commits) is timing-dependent, so a single pass can
	// miss it even against an unguarded implementation. Each round starts from a
	// clean pack row; the mark deliberately PERSISTS across rounds, exactly as it
	// does across an uninstall.
	ref := packs.Ref{PackID: "acme/menu-board", TrustChannel: "community"}
	for round := 0; round < 25; round++ {
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); _, _ = inNew.InstallRef(ctx, ref) }()
		go func() { defer wg.Done(); _, _ = inOld.InstallRef(ctx, ref) }()
		wg.Wait()

		pack, found, _ := st.GetPack(ctx, "acme/menu-board")
		if !found {
			t.Fatalf("round %d: neither concurrent install landed", round)
		}
		mark, _, _ := st.PackChannelMark(ctx, "acme/menu-board", "community")
		// Whichever won, the invariant is the same: the running install is never
		// below the mark the next resolution will be judged against.
		if packs.VersionHigher(mark, pack.Version) {
			t.Fatalf("round %d: installed %q is BELOW the mark %q — a later pointer resolution to the installed version would now be refused",
				round, pack.Version, mark)
		}
		if err := st.UninstallPack(ctx, "acme/menu-board", pack.Revision); err != nil {
			t.Fatalf("round %d: reset: %v", round, err)
		}
	}
}
