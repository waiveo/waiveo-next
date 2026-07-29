package packs_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/maaxton/waiveo-next/internal/app/packs"
	"github.com/maaxton/waiveo-next/internal/app/store"
)

// These tests drive Installer.Update — the real entry point — never its
// internals. The bug class they exist against is a rollback path that never
// runs: a revert branch that looks right, is never reached by any reachable
// condition, and so is only ever exercised by a test that calls it directly.
// Every revert here is produced by yanking an entry in the fixture registry and
// then asking the update check to do its ordinary job.

// ---- fixture ----------------------------------------------------------------

// reindex rewrites the fixture registry's index.json in place, so a test can
// move a channel pointer, yank an entry, or publish a new version AFTER the
// Market was configured — the Source keeps pointing at the same directory, and
// the resolver reads the document fresh on every resolution.
func (r *registry) reindex() { _ = r.source() }

// setStatus rewrites one published entry's `status` (CHI-020 + MKT-044) and
// reindexes: the yank that triggers a revert, applied the way a registry
// actually applies one.
func (r *registry) setStatus(artifactID, version, status string) {
	r.t.Helper()
	for _, e := range r.entries {
		if e["artifact_id"] == artifactID && e["version"] == version {
			e["status"] = status
			r.reindex()
			return
		}
	}
	r.t.Fatalf("setStatus: no published entry for %s %s", artifactID, version)
}

// breakIndex replaces the index document with something unreadable — the
// "whoever serves the index chose not to answer" condition, which must NOT be a
// revert trigger.
func (r *registry) breakIndex() {
	r.t.Helper()
	if err := os.WriteFile(filepath.Join(r.dir, "index.json"), []byte("not an index"), 0o644); err != nil {
		r.t.Fatalf("break index: %v", err)
	}
}

// updateFixture stands up a store, a file:// registry, and an installer wired to
// both, returning the signer whose key the trust anchors authorize.
func updateFixture(t *testing.T) (*store.Store, *packs.Installer, *testSigner, *registry) {
	t.Helper()
	st := openStore(t)
	reg := newRegistry(t, "fixture")
	src := reg.source()
	in, signer := marketInstaller(t, st, src)
	return st, in, signer, reg
}

// versionedPack builds and signs the base pack at a given version.
func versionedPack(t *testing.T, s *testSigner, version string) []byte {
	t.Helper()
	m := baseManifest()
	m["version"] = version
	return signedPackZip(t, s, m)
}

// publishVersion signs, publishes and points the community channel at version.
func publishVersion(t *testing.T, reg *registry, s *testSigner, version string) {
	t.Helper()
	reg.publish("acme/menu-board", version, versionedPack(t, s, version), nil)
	reg.point("acme/menu-board", "community", version)
	reg.reindex()
}

// installedState is everything an update must leave untouched when it refuses.
type installedState struct {
	pack    store.Pack
	pages   []string
	locales []string
	records []store.PackInstallRecord
	mark    string
	gen     int64
}

func snapshotInstalled(t *testing.T, st *store.Store, packID, channel string) installedState {
	t.Helper()
	ctx := context.Background()
	p, found, err := st.GetPack(ctx, packID)
	if err != nil || !found {
		t.Fatalf("snapshot: GetPack(%s) found=%v err=%v", packID, found, err)
	}
	pages, err := st.PackFileNames(ctx, packID, store.PackFilePage)
	if err != nil {
		t.Fatalf("snapshot: page names: %v", err)
	}
	locales, err := st.PackFileNames(ctx, packID, store.PackFileLocale)
	if err != nil {
		t.Fatalf("snapshot: locale names: %v", err)
	}
	recs, err := st.ListPackInstalls(ctx, packID)
	if err != nil {
		t.Fatalf("snapshot: install records: %v", err)
	}
	mark, _, err := st.PackChannelMark(ctx, packID, channel)
	if err != nil {
		t.Fatalf("snapshot: channel mark: %v", err)
	}
	return installedState{pack: p, pages: pages, locales: locales, records: recs, mark: mark, gen: gen(t, st)}
}

// assertUnchanged is the "a failed update rolls back to the prior version" bar,
// stated as what it actually means here: the prior version was never removed, so
// EVERYTHING about it is still exactly as it was.
func (s installedState) assertUnchanged(t *testing.T, st *store.Store, label string) {
	t.Helper()
	now := snapshotInstalled(t, st, s.pack.ID, "community")
	if now.pack.Version != s.pack.Version {
		t.Fatalf("%s: installed version moved %s -> %s", label, s.pack.Version, now.pack.Version)
	}
	if now.pack.Revision != s.pack.Revision {
		t.Fatalf("%s: pack revision moved %d -> %d (something wrote)", label, s.pack.Revision, now.pack.Revision)
	}
	if string(now.pack.Manifest) != string(s.pack.Manifest) {
		t.Fatalf("%s: the installed manifest changed", label)
	}
	if strings.Join(now.pages, ",") != strings.Join(s.pages, ",") {
		t.Fatalf("%s: page set changed %v -> %v", label, s.pages, now.pages)
	}
	if strings.Join(now.locales, ",") != strings.Join(s.locales, ",") {
		t.Fatalf("%s: locale set changed %v -> %v", label, s.locales, now.locales)
	}
	if len(now.records) != len(s.records) {
		t.Fatalf("%s: install-record count moved %d -> %d (a refused install appended one, MKT-094b)",
			label, len(s.records), len(now.records))
	}
	if now.mark != s.mark {
		t.Fatalf("%s: channel mark moved %q -> %q", label, s.mark, now.mark)
	}
	if now.gen != s.gen {
		t.Fatalf("%s: store generation moved %d -> %d", label, s.gen, now.gen)
	}
}

func refusalCode(t *testing.T, err error) string {
	t.Helper()
	var aerr *packs.ArtifactError
	if !errors.As(err, &aerr) {
		t.Fatalf("error %v (%T) is not an *ArtifactError", err, err)
	}
	return aerr.Code
}

// ---- (a) a pack can be updated in place -------------------------------------

// TestUpdateFollowsThePinnedChannelPointer is done-bar (a): the update check
// reads the pack's own pin off its newest install record (MKT-094), re-resolves
// that source's channel pointer (MKT-090), and installs what it names — in
// place, keeping the pack's identity, bumping its revision, appending a record.
func TestUpdateFollowsThePinnedChannelPointer(t *testing.T) {
	st, in, signer, reg := updateFixture(t)
	ctx := context.Background()

	publishVersion(t, reg, signer, "1.0.0")
	if _, err := in.InstallRef(ctx, packs.Ref{PackID: "acme/menu-board", TrustChannel: "community"}); err != nil {
		t.Fatalf("InstallRef 1.0.0: %v", err)
	}
	publishVersion(t, reg, signer, "2.0.0")

	out, err := in.Update(ctx, "acme/menu-board")
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if out.Action != packs.UpdateApplied || out.FromVersion != "1.0.0" || out.ToVersion != "2.0.0" {
		t.Fatalf("update = %+v, want updated 1.0.0 -> 2.0.0", out)
	}

	p, found, err := st.GetPack(ctx, "acme/menu-board")
	if err != nil || !found {
		t.Fatalf("GetPack: found=%v err=%v", found, err)
	}
	if p.Version != "2.0.0" || p.Revision != 2 {
		t.Fatalf("installed = %s rev %d, want 2.0.0 rev 2 (updated in place, not reinstalled fresh)", p.Version, p.Revision)
	}
	recs, err := st.ListPackInstalls(ctx, "acme/menu-board")
	if err != nil || len(recs) != 2 {
		t.Fatalf("install records = %d, want 2 (append-only, MKT-094b); err=%v", len(recs), err)
	}
	if recs[1].ResolvedVersion != "2.0.0" || recs[1].TrustChannel != "community" || recs[1].Source != "fixture" {
		t.Fatalf("newest record = %+v, want 2.0.0 / community / fixture", recs[1])
	}
	if mark, _, _ := st.PackChannelMark(ctx, "acme/menu-board", "community"); mark != "2.0.0" {
		t.Fatalf("channel mark = %q, want 2.0.0", mark)
	}
}

// TestUpdateIsUnchangedWhenThePointerHasNotMoved: an update check that finds
// the pointer still on the installed version writes NOTHING. Reinstalling the
// same version on every check would bump the revision and append a record each
// time, silting up the history MKT-093's last-known-good reads its target from.
func TestUpdateIsUnchangedWhenThePointerHasNotMoved(t *testing.T) {
	st, in, signer, reg := updateFixture(t)
	ctx := context.Background()

	publishVersion(t, reg, signer, "1.0.0")
	if _, err := in.InstallRef(ctx, packs.Ref{PackID: "acme/menu-board", TrustChannel: "community"}); err != nil {
		t.Fatalf("InstallRef: %v", err)
	}
	before := snapshotInstalled(t, st, "acme/menu-board", "community")

	out, err := in.Update(ctx, "acme/menu-board")
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if out.Action != packs.UpdateUnchanged || out.FromVersion != "1.0.0" || out.ToVersion != "1.0.0" {
		t.Fatalf("update = %+v, want unchanged at 1.0.0", out)
	}
	before.assertUnchanged(t, st, "a no-op update check")
}

// TestUpdateRefusesADirectInstall: MKT-094a. An install with no trust channel
// pinned has no (pack, channel) pair to re-resolve, and a host MUST NOT supply
// a default — that would be MKT-060a(b)'s prohibited silent tier selection
// arriving by another route.
func TestUpdateRefusesADirectInstall(t *testing.T) {
	st, in, signer, reg := updateFixture(t)
	ctx := context.Background()

	// Installed by artifact upload, not by reference. The registry publishes a
	// NEWER version, so the only thing standing between this pack and an
	// auto-track is the missing pin.
	if _, err := in.Install(ctx, versionedPack(t, signer, "1.0.0")); err != nil {
		t.Fatalf("Install: %v", err)
	}
	publishVersion(t, reg, signer, "2.0.0")
	before := snapshotInstalled(t, st, "acme/menu-board", "community")

	_, err := in.Update(ctx, "acme/menu-board")
	if code := refusalCode(t, err); code != "TRUST_CHANNEL_UNKNOWN" {
		t.Fatalf("Update of a direct install = %s, want TRUST_CHANNEL_UNKNOWN", code)
	}
	before.assertUnchanged(t, st, "a refused update of a direct install")
}

// TestUpdateOfAnUninstalledPackIsNotFound: nothing to re-resolve, and no
// reference is invented for it.
func TestUpdateOfAnUninstalledPackIsNotFound(t *testing.T) {
	_, in, _, _ := updateFixture(t)
	if _, err := in.Update(context.Background(), "acme/menu-board"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Update of an uninstalled pack = %v, want store.ErrNotFound", err)
	}
}

// TestUpdateWillNotWalkTheChannelBackward: MKT-090 explicitly re-applies
// MKT-050's anti-rollback on the update path — "never a weakened or
// special-cased check for an update path". A newly served index whose pointer
// names an older, non-yanked version does not downgrade the box.
func TestUpdateWillNotWalkTheChannelBackward(t *testing.T) {
	st, in, signer, reg := updateFixture(t)
	ctx := context.Background()

	publishVersion(t, reg, signer, "1.0.0")
	publishVersion(t, reg, signer, "2.0.0")
	if _, err := in.InstallRef(ctx, packs.Ref{PackID: "acme/menu-board", TrustChannel: "community"}); err != nil {
		t.Fatalf("InstallRef 2.0.0: %v", err)
	}
	before := snapshotInstalled(t, st, "acme/menu-board", "community")

	reg.point("acme/menu-board", "community", "1.0.0")
	reg.reindex()

	_, err := in.Update(ctx, "acme/menu-board")
	if code := refusalCode(t, err); code != "POINTER_ROLLBACK_REJECTED" {
		t.Fatalf("Update onto a backward pointer = %s, want POINTER_ROLLBACK_REJECTED", code)
	}
	before.assertUnchanged(t, st, "a refused backward auto-track")
}

// ---- (b) a failed update leaves the prior version ---------------------------

// TestUpdateFailureAtEveryStageLeavesThePriorVersion is done-bar (b), stated as
// what it actually means in this pipeline. Install is ONE transaction that
// UPSERTS — it never deletes the prior version first — so a refusal at any stage
// has nothing to compensate for; the previously installed version is simply
// still there, whole. This drives a refusal at each stage of the pipeline and
// asserts exactly that: same version, same revision, same manifest, same bundle,
// same records, same anti-rollback mark, same store generation.
//
// If any stage ever gained the ability to write before refusing, this test is
// what fails.
func TestUpdateFailureAtEveryStageLeavesThePriorVersion(t *testing.T) {
	stages := []struct {
		name string
		// candidate returns the 2.0.0 artifact bytes to publish, and the entry
		// version to publish them under.
		candidate func(t *testing.T, s *testSigner, other *testSigner) []byte
		entryVer  string
		wantCode  string // "" == a *ManifestError rather than an *ArtifactError
	}{
		{
			name:      "artifact is not a readable bundle",
			candidate: func(t *testing.T, s, _ *testSigner) []byte { return []byte("this is not a zip") },
			entryVer:  "2.0.0",
			wantCode:  "PACK_ARTIFACT_INVALID",
		},
		{
			name: "signature envelope stripped",
			candidate: func(t *testing.T, s, _ *testSigner) []byte {
				m := baseManifest()
				m["version"] = "2.0.0"
				return basePackZip(t, m) // unsigned
			},
			entryVer: "2.0.0",
			wantCode: "PACK_UNSIGNED",
		},
		{
			name: "payload altered after signing",
			candidate: func(t *testing.T, s, _ *testSigner) []byte {
				return tamperEntry(t, versionedPack(t, s, "2.0.0"), "messages/en.json", `{"pack.displayName":"tampered"}`)
			},
			entryVer: "2.0.0",
			wantCode: "PACK_SIGNATURE_INVALID",
		},
		{
			name: "signed by a key no anchor authorizes for the namespace",
			candidate: func(t *testing.T, _, other *testSigner) []byte {
				m := baseManifest()
				m["version"] = "2.0.0"
				return other.sign(t, basePackZip(t, m), "acme/menu-board", "2.0.0")
			},
			entryVer: "2.0.0",
			wantCode: "PACK_SIGNER_UNTRUSTED",
		},
		{
			name: "index entry claims an identity the artifact is not signed as",
			candidate: func(t *testing.T, s, _ *testSigner) []byte {
				// Genuinely signed — as 2.0.1 — but published under the 2.0.0 entry.
				return versionedPack(t, s, "2.0.1")
			},
			entryVer: "2.0.0",
			wantCode: "RESOLVED_IDENTITY_MISMATCH",
		},
		{
			name: "manifest the engine refuses",
			candidate: func(t *testing.T, s, _ *testSigner) []byte {
				m := baseManifest()
				m["version"] = "2.0.0"
				m["capabilities"] = []any{map[string]any{"capability": "root.everything", "scope": "all", "reason": "no"}}
				return signedPackZip(t, s, m)
			},
			entryVer: "2.0.0",
			wantCode: "", // *ManifestError
		},
		{
			name: "refused inside the install transaction (MAN-053 regression)",
			candidate: func(t *testing.T, s, _ *testSigner) []byte {
				m := baseManifest()
				m["version"] = "2.0.0"
				dm, _ := m["dataModel"].(map[string]any)
				dm["version"] = 1 // the installed 1.0.0 declares 2 below
				return signedPackZip(t, s, m)
			},
			entryVer: "2.0.0",
			wantCode: "", // *ManifestError, raised by the in-transaction guard
		},
	}

	for _, stage := range stages {
		t.Run(stage.name, func(t *testing.T) {
			st, in, signer, reg := updateFixture(t)
			ctx := context.Background()
			other := newTestSigner(t) // a key no anchor authorizes

			// The installed 1.0.0 declares dataModel.version 2 so the MAN-053
			// stage has something to regress from; every other stage is
			// unaffected by it (the candidates above all declare 2 as well,
			// except the one that deliberately declares 1).
			base := baseManifest()
			dm, _ := base["dataModel"].(map[string]any)
			dm["version"] = 2
			reg.publish("acme/menu-board", "1.0.0", signedPackZip(t, signer, base), nil)
			reg.point("acme/menu-board", "community", "1.0.0")
			reg.reindex()
			if _, err := in.InstallRef(ctx, packs.Ref{PackID: "acme/menu-board", TrustChannel: "community"}); err != nil {
				t.Fatalf("InstallRef 1.0.0: %v", err)
			}
			before := snapshotInstalled(t, st, "acme/menu-board", "community")

			// Publish the poisoned candidate and point the channel at it. The
			// registry digests the exact bytes it publishes, so every candidate
			// clears CHI-021/023 and reaches the stage it is meant to exercise
			// rather than dying at the download integrity check.
			reg.publish("acme/menu-board", stage.entryVer, stage.candidate(t, signer, other), nil)
			reg.point("acme/menu-board", "community", stage.entryVer)
			reg.reindex()

			_, err := in.Update(ctx, "acme/menu-board")
			if err == nil {
				t.Fatalf("Update accepted the poisoned candidate")
			}
			if stage.wantCode == "" {
				var merr *packs.ManifestError
				if !errors.As(err, &merr) {
					t.Fatalf("Update err = %v (%T), want *packs.ManifestError", err, err)
				}
			} else if code := refusalCode(t, err); code != stage.wantCode {
				t.Fatalf("Update err code = %s, want %s (%v)", code, stage.wantCode, err)
			}
			before.assertUnchanged(t, st, "a refused update ("+stage.name+")")
		})
	}
}

// ---- revert-to-known-good (MKT-093) -----------------------------------------

// TestUpdateRevertsToLastKnownGoodWhenTheAppliedVersionIsYanked: the one
// condition that genuinely needs a backward move. The box did nothing wrong —
// the registry withdrew the version running on it — and MKT-050's sole
// anti-rollback exception, which MKT-093 turns into a requirement for a required
// pack, applies.
func TestUpdateRevertsToLastKnownGoodWhenTheAppliedVersionIsYanked(t *testing.T) {
	st, in, signer, reg := updateFixture(t)
	ctx := context.Background()

	publishVersion(t, reg, signer, "1.0.0")
	if _, err := in.InstallRef(ctx, packs.Ref{PackID: "acme/menu-board", TrustChannel: "community"}); err != nil {
		t.Fatalf("InstallRef 1.0.0: %v", err)
	}
	publishVersion(t, reg, signer, "2.0.0")
	if _, err := in.Update(ctx, "acme/menu-board"); err != nil {
		t.Fatalf("Update to 2.0.0: %v", err)
	}

	reg.setStatus("acme/menu-board", "2.0.0", "yanked")

	out, err := in.Update(ctx, "acme/menu-board")
	if err != nil {
		t.Fatalf("Update after the yank: %v", err)
	}
	if out.Action != packs.UpdateReverted || out.FromVersion != "2.0.0" || out.ToVersion != "1.0.0" {
		t.Fatalf("update = %+v, want reverted 2.0.0 -> 1.0.0", out)
	}

	p, _, err := st.GetPack(ctx, "acme/menu-board")
	if err != nil || p.Version != "1.0.0" {
		t.Fatalf("installed = %s (err=%v), want the reverted 1.0.0", p.Version, err)
	}
	recs, err := st.ListPackInstalls(ctx, "acme/menu-board")
	if err != nil || len(recs) != 3 {
		t.Fatalf("install records = %d, want 3 (the revert appends one too, MKT-094b); err=%v", len(recs), err)
	}
	if recs[2].ResolvedVersion != "1.0.0" {
		t.Fatalf("newest record = %s, want the reverted 1.0.0", recs[2].ResolvedVersion)
	}
	// MKT-050: the mark records the highest version ever RESOLVED. A revert does
	// not walk it back — so a later pointer cannot use the revert as a way to
	// re-offer an already-superseded version.
	if mark, _, _ := st.PackChannelMark(ctx, "acme/menu-board", "community"); mark != "2.0.0" {
		t.Fatalf("channel mark = %q after a revert, want it held at 2.0.0", mark)
	}
}

// TestUpdateSurvivesAnUnreadableIndex: an index that will not parse leaves the
// installed version alone and refuses. It is deliberately NOT the test that
// pins down the revert trigger — with no readable index there is no revert
// target to find either, so this case cannot tell a correct trigger from a
// wrong one. The two tests below it can, and do.
func TestUpdateSurvivesAnUnreadableIndex(t *testing.T) {
	st, in, signer, reg := updateFixture(t)
	ctx := context.Background()

	publishVersion(t, reg, signer, "1.0.0")
	if _, err := in.InstallRef(ctx, packs.Ref{PackID: "acme/menu-board", TrustChannel: "community"}); err != nil {
		t.Fatalf("InstallRef 1.0.0: %v", err)
	}
	publishVersion(t, reg, signer, "2.0.0")
	if _, err := in.Update(ctx, "acme/menu-board"); err != nil {
		t.Fatalf("Update to 2.0.0: %v", err)
	}
	before := snapshotInstalled(t, st, "acme/menu-board", "community")

	reg.breakIndex()

	_, err := in.Update(ctx, "acme/menu-board")
	if err == nil {
		t.Fatal("Update against an unreadable index succeeded")
	}
	if code := refusalCode(t, err); code != "MARKETPLACE_REF_UNRESOLVED" {
		t.Fatalf("Update err code = %s, want MARKETPLACE_REF_UNRESOLVED", code)
	}
	before.assertUnchanged(t, st, "an update whose index would not load")
}

// TestUpdateDoesNotRevertWhenTheAppliedEntryIsMerelyWithdrawn is the same
// principle for the subtler — and much more dangerous — case, and it is
// deliberately set up so a wrong implementation actually reverts.
//
// The applied 2.0.0's entry is withdrawn from the index while 1.0.0 stays
// published and fully resolvable. So there IS a last-known-good sitting right
// there: an implementation that treated "the applied version no longer resolves"
// as the revert trigger would find it and downgrade the box, and every step
// would succeed. Only a `yanked` STATUS is a published statement that a version
// is bad; an entry that has simply stopped being listed is not one.
func TestUpdateDoesNotRevertWhenTheAppliedEntryIsMerelyWithdrawn(t *testing.T) {
	st, in, signer, reg := updateFixture(t)
	ctx := context.Background()

	publishVersion(t, reg, signer, "1.0.0")
	if _, err := in.InstallRef(ctx, packs.Ref{PackID: "acme/menu-board", TrustChannel: "community"}); err != nil {
		t.Fatalf("InstallRef 1.0.0: %v", err)
	}
	publishVersion(t, reg, signer, "2.0.0")
	if _, err := in.Update(ctx, "acme/menu-board"); err != nil {
		t.Fatalf("Update to 2.0.0: %v", err)
	}
	before := snapshotInstalled(t, st, "acme/menu-board", "community")

	// Drop ONLY the 2.0.0 entry; 1.0.0 stays active and resolvable, and the
	// pointer still names the now-absent 2.0.0.
	kept := reg.entries[:0]
	for _, e := range reg.entries {
		if e["version"] != "2.0.0" {
			kept = append(kept, e)
		}
	}
	reg.entries = kept
	reg.reindex()

	_, err := in.Update(ctx, "acme/menu-board")
	if err == nil {
		t.Fatal("Update against an index that withdrew the applied entry succeeded")
	}
	if code := refusalCode(t, err); code != "MARKETPLACE_REF_UNRESOLVED" {
		t.Fatalf("Update err code = %s, want MARKETPLACE_REF_UNRESOLVED", code)
	}
	before.assertUnchanged(t, st, "an update whose applied entry was withdrawn (a resolvable 1.0.0 was on offer)")
}

// TestUpdateDoesNotRevertOffAnArchivedAppliedVersion: MKT-044. `archived`
// withdraws a version from ordinary discovery — a channel pointer may not land
// on one — but it stays resolvable for an existing install. So the pointer
// resolution fails while the applied version itself is perfectly fine, and there
// is a resolvable 1.0.0 in the record history to revert onto. Reverting here
// would be a downgrade triggered by a publisher's routine housekeeping.
func TestUpdateDoesNotRevertOffAnArchivedAppliedVersion(t *testing.T) {
	st, in, signer, reg := updateFixture(t)
	ctx := context.Background()

	publishVersion(t, reg, signer, "1.0.0")
	if _, err := in.InstallRef(ctx, packs.Ref{PackID: "acme/menu-board", TrustChannel: "community"}); err != nil {
		t.Fatalf("InstallRef 1.0.0: %v", err)
	}
	publishVersion(t, reg, signer, "2.0.0")
	if _, err := in.Update(ctx, "acme/menu-board"); err != nil {
		t.Fatalf("Update to 2.0.0: %v", err)
	}
	before := snapshotInstalled(t, st, "acme/menu-board", "community")

	reg.setStatus("acme/menu-board", "2.0.0", "archived")

	_, err := in.Update(ctx, "acme/menu-board")
	if err == nil {
		t.Fatal("Update whose pointer names an archived version succeeded")
	}
	if code := refusalCode(t, err); code != "MARKETPLACE_REF_UNRESOLVED" {
		t.Fatalf("Update err code = %s, want MARKETPLACE_REF_UNRESOLVED", code)
	}
	before.assertUnchanged(t, st, "an update whose applied version was archived")
}

// TestRevertRefusesWhenNoAppliedVersionIsStillResolvable: MKT-093's target must
// come from the install-record history AND still resolve. When every earlier
// applied version has been yanked too, there is no last-known-good — and the
// answer is a refusal, not a silent install of whatever the index still offers,
// which is exactly the substitution MKT-050 forbids.
func TestRevertRefusesWhenNoAppliedVersionIsStillResolvable(t *testing.T) {
	st, in, signer, reg := updateFixture(t)
	ctx := context.Background()

	publishVersion(t, reg, signer, "1.0.0")
	if _, err := in.InstallRef(ctx, packs.Ref{PackID: "acme/menu-board", TrustChannel: "community"}); err != nil {
		t.Fatalf("InstallRef 1.0.0: %v", err)
	}
	publishVersion(t, reg, signer, "2.0.0")
	if _, err := in.Update(ctx, "acme/menu-board"); err != nil {
		t.Fatalf("Update to 2.0.0: %v", err)
	}
	// A version the deployment NEVER applied is on offer and still active. It
	// must not be chosen: it is not in this install's own record history.
	reg.publish("acme/menu-board", "0.9.0", versionedPack(t, signer, "0.9.0"), nil)
	reg.setStatus("acme/menu-board", "1.0.0", "yanked")
	reg.setStatus("acme/menu-board", "2.0.0", "yanked")
	before := snapshotInstalled(t, st, "acme/menu-board", "community")

	_, err := in.Update(ctx, "acme/menu-board")
	if code := refusalCode(t, err); code != "MARKETPLACE_REF_UNRESOLVED" {
		t.Fatalf("Update err code = %s, want MARKETPLACE_REF_UNRESOLVED", code)
	}
	before.assertUnchanged(t, st, "a revert with no known-good target")
}

// TestRevertSkipsATargetBelowTheRequiredFloor: MKT-093b applies to a revert as
// much as to an install, and the walk must SKIP an inadmissible candidate
// rather than stop at it.
//
// The record history is deliberately non-monotonic — 2.0.0, then a pinned
// downgrade to 1.0.0, then 3.0.0 — so walking newest-first meets the below-floor
// 1.0.0 BEFORE the admissible 2.0.0. An implementation that merely let the
// store's own floor refuse the install would take 1.0.0, be refused, and report
// "no known-good", stranding the box on a yanked version while a perfectly good
// 2.0.0 sat one record further back.
func TestRevertSkipsATargetBelowTheRequiredFloor(t *testing.T) {
	st, in, signer, reg := updateFixture(t)
	ctx := context.Background()
	// The roster is host configuration and may name any pack; nothing requires
	// a required pack to sit in the first-party namespace (MKT-093a).
	roster, err := packs.NewRoster(map[string]string{"acme/menu-board": "2.0.0"})
	if err != nil {
		t.Fatalf("NewRoster: %v", err)
	}

	publishVersion(t, reg, signer, "1.0.0")
	publishVersion(t, reg, signer, "2.0.0")
	if _, err := in.InstallRef(ctx, packs.Ref{PackID: "acme/menu-board", TrustChannel: "community"}); err != nil {
		t.Fatalf("InstallRef 2.0.0: %v", err)
	}
	// An explicit version pin, which MKT-050 does not govern (MKT-060a(c)) and
	// which no floor forbids yet: the history now holds 1.0.0 NEWER than 2.0.0.
	if _, err := in.InstallRef(ctx, packs.Ref{PackID: "acme/menu-board", TrustChannel: "community", Version: "1.0.0"}); err != nil {
		t.Fatalf("InstallRef pinned 1.0.0: %v", err)
	}
	publishVersion(t, reg, signer, "3.0.0")
	if _, err := in.Update(ctx, "acme/menu-board"); err != nil {
		t.Fatalf("Update to 3.0.0: %v", err)
	}
	// The floor arrives only now, so the 1.0.0 install above was legal when it
	// happened — the record history genuinely holds a below-floor version.
	st.SetRequiredPacks(roster)

	reg.setStatus("acme/menu-board", "3.0.0", "yanked")

	out, err := in.Update(ctx, "acme/menu-board")
	if err != nil {
		t.Fatalf("Update after the yank: %v", err)
	}
	if out.Action != packs.UpdateReverted || out.ToVersion != "2.0.0" {
		t.Fatalf("update = %+v, want reverted to 2.0.0 (1.0.0 is below the floor)", out)
	}

	// And when 2.0.0 is yanked too, the only remaining applied version is below
	// the floor — so there is no admissible target and the revert refuses rather
	// than installing it.
	reg.setStatus("acme/menu-board", "2.0.0", "yanked")
	before := snapshotInstalled(t, st, "acme/menu-board", "community")
	if _, err := in.Update(ctx, "acme/menu-board"); err == nil {
		t.Fatal("the revert installed a target below the required floor")
	}
	before.assertUnchanged(t, st, "a revert whose only target is below the floor")
}

// raceFetcher runs a side effect the first time a URL containing `on` is
// fetched — the real interleaving seam between the revert's decision (taken from
// reads outside the write lock) and the transaction that commits it. The
// artifact fetch is the last thing that happens before the install transaction
// opens, so a writer landing here lands exactly in the window.
type raceFetcher struct {
	inner packs.Fetcher
	on    string
	once  sync.Once
	fire  func()
}

func (f *raceFetcher) Fetch(ctx context.Context, rawURL string) ([]byte, error) {
	if strings.Contains(rawURL, f.on) && f.fire != nil {
		f.once.Do(f.fire)
	}
	return f.inner.Fetch(ctx, rawURL)
}

// TestRevertRefusesWhenTheInstalledVersionMovedUnderIt is the TOCTOU closure.
// The revert decides "2.0.0 is applied and yanked, so reinstall 1.0.0" from
// reads taken outside the store write lock. Another writer installs 3.0.0 while
// the revert is fetching its target. Without the in-transaction compare-and-swap
// the revert would then commit 1.0.0 over a 3.0.0 it never looked at — a
// downgrade in which every individual step succeeded.
func TestRevertRefusesWhenTheInstalledVersionMovedUnderIt(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	reg := newRegistry(t, "fixture")
	src := reg.source()

	// One signer, two installers over the same store: the second one is the
	// concurrent writer.
	signer := newTestSigner(t)
	anchors := signer.anchorsFor(fixtureNamespaces...)
	racer := &raceFetcher{inner: src.Fetcher, on: "menu-board-1.0.0"}
	src.Fetcher = racer
	in := packs.NewInstaller(st, anchors, packs.WithMarketplace(
		packs.NewMarket(func() int64 { return fixedNow }, src)))
	other := packs.NewInstaller(st, anchors)

	publishVersion(t, reg, signer, "1.0.0")
	if _, err := in.InstallRef(ctx, packs.Ref{PackID: "acme/menu-board", TrustChannel: "community"}); err != nil {
		t.Fatalf("InstallRef 1.0.0: %v", err)
	}
	publishVersion(t, reg, signer, "2.0.0")
	if _, err := in.Update(ctx, "acme/menu-board"); err != nil {
		t.Fatalf("Update to 2.0.0: %v", err)
	}
	reg.setStatus("acme/menu-board", "2.0.0", "yanked")

	// Fires while the revert is downloading 1.0.0 — after it has decided the
	// applied version is 2.0.0, before its transaction opens.
	racer.fire = func() {
		if _, err := other.Install(ctx, versionedPack(t, signer, "3.0.0")); err != nil {
			t.Errorf("concurrent install of 3.0.0: %v", err)
		}
	}

	_, err := in.Update(ctx, "acme/menu-board")
	if err == nil {
		t.Fatal("the revert committed over a version that arrived while it was resolving")
	}
	if code := refusalCode(t, err); code != "POINTER_ROLLBACK_REJECTED" {
		t.Fatalf("Update err code = %s, want POINTER_ROLLBACK_REJECTED", code)
	}
	p, _, err := st.GetPack(ctx, "acme/menu-board")
	if err != nil || p.Version != "3.0.0" {
		t.Fatalf("installed = %s (err=%v), want the concurrently-installed 3.0.0 left alone", p.Version, err)
	}
}

// TestUpdateRefusesToResurrectAPackUninstalledUnderIt: the same compare-and-swap
// on the ordinary update path. Install UPSERTS, so an update whose pack was
// uninstalled while it was resolving would quietly INSERT it again — an
// operator's uninstall undone by a background update check, with every step
// succeeding.
func TestUpdateRefusesToResurrectAPackUninstalledUnderIt(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	reg := newRegistry(t, "fixture")
	src := reg.source()

	signer := newTestSigner(t)
	racer := &raceFetcher{inner: src.Fetcher, on: "menu-board-2.0.0"}
	src.Fetcher = racer
	in := packs.NewInstaller(st, signer.anchorsFor(fixtureNamespaces...), packs.WithMarketplace(
		packs.NewMarket(func() int64 { return fixedNow }, src)))

	publishVersion(t, reg, signer, "1.0.0")
	if _, err := in.InstallRef(ctx, packs.Ref{PackID: "acme/menu-board", TrustChannel: "community"}); err != nil {
		t.Fatalf("InstallRef 1.0.0: %v", err)
	}
	publishVersion(t, reg, signer, "2.0.0")

	racer.fire = func() {
		p, found, err := st.GetPack(ctx, "acme/menu-board")
		if err != nil || !found {
			t.Errorf("concurrent uninstall: GetPack found=%v err=%v", found, err)
			return
		}
		if err := st.UninstallPack(ctx, "acme/menu-board", p.Revision); err != nil {
			t.Errorf("concurrent uninstall: %v", err)
		}
	}

	if _, err := in.Update(ctx, "acme/menu-board"); err == nil {
		t.Fatal("the update reinstalled a pack that was uninstalled while it resolved")
	}
	if _, found, err := st.GetPack(ctx, "acme/menu-board"); err != nil || found {
		t.Fatalf("the pack came back (found=%v err=%v)", found, err)
	}
}

// ---- (c) the required-pack floor, on every install path ---------------------

// TestRequiredFloorRefusesEveryInstallPath is done-bar (c) for the downgrade
// half: MKT-093b closes the pointer path, the explicit-pin path AND the direct
// upload path, which is broader than MKT-050 (pointer only). The last leg checks
// the narrowing that makes the broadening safe: a version AT the floor still
// installs by explicit pin even when it is `archived`, so MKT-044's reinstall
// path — which a blanket "never install older" rule would have closed — is
// still open.
func TestRequiredFloorRefusesEveryInstallPath(t *testing.T) {
	st, in, signer, reg := updateFixture(t)
	ctx := context.Background()
	roster, err := packs.NewRoster(map[string]string{"acme/menu-board": "2.0.0"})
	if err != nil {
		t.Fatalf("NewRoster: %v", err)
	}
	st.SetRequiredPacks(roster)

	reg.publish("acme/menu-board", "1.0.0", versionedPack(t, signer, "1.0.0"), nil)
	reg.publish("acme/menu-board", "2.0.0", versionedPack(t, signer, "2.0.0"), map[string]any{"status": "archived"})
	reg.point("acme/menu-board", "community", "1.0.0")
	reg.reindex()
	before := gen(t, st)

	// 1. channel pointer -> 1.0.0
	_, err = in.InstallRef(ctx, packs.Ref{PackID: "acme/menu-board", TrustChannel: "community"})
	if code := refusalCode(t, err); code != "REQUIRED_PACK_FLOOR" {
		t.Fatalf("pointer install below the floor = %s, want REQUIRED_PACK_FLOOR", code)
	}
	// 2. explicit version pin -> 1.0.0 (outside MKT-050's own scope)
	_, err = in.InstallRef(ctx, packs.Ref{PackID: "acme/menu-board", TrustChannel: "community", Version: "1.0.0"})
	if code := refusalCode(t, err); code != "REQUIRED_PACK_FLOOR" {
		t.Fatalf("pinned install below the floor = %s, want REQUIRED_PACK_FLOOR", code)
	}
	// 3. direct artifact upload -> 1.0.0 (no registry in the picture at all)
	_, err = in.Install(ctx, versionedPack(t, signer, "1.0.0"))
	if code := refusalCode(t, err); code != "REQUIRED_PACK_FLOOR" {
		t.Fatalf("direct install below the floor = %s, want REQUIRED_PACK_FLOOR", code)
	}

	noResidue(t, st, "acme/menu-board", before)

	// 4. the archived version AT the floor, by explicit pin: accepted.
	res, err := in.InstallRef(ctx, packs.Ref{PackID: "acme/menu-board", TrustChannel: "community", Version: "2.0.0"})
	if err != nil {
		t.Fatalf("archived reinstall at the floor: %v", err)
	}
	if res.Version != "2.0.0" {
		t.Fatalf("installed = %s, want 2.0.0", res.Version)
	}
}

// TestNewRosterRefusesAMalformedEntry: a roster is host configuration, and a
// malformed entry is caught at wiring time rather than degrading to "this pack
// is protected by nothing" or "this pack can never install" at runtime.
func TestNewRosterRefusesAMalformedEntry(t *testing.T) {
	if _, err := packs.NewRoster(map[string]string{"menu-board": "1.0.0"}); err == nil {
		t.Fatal("NewRoster accepted an unqualified pack id")
	}
	if _, err := packs.NewRoster(map[string]string{"acme/menu-board": "1.0"}); err == nil {
		t.Fatal("NewRoster accepted a floor outside MAN-002's grammar")
	}
	r, err := packs.NewRoster(map[string]string{"acme/menu-board": "1.2.3"})
	if err != nil {
		t.Fatalf("NewRoster: %v", err)
	}
	if floor, ok := r.RequiredFloor("acme/menu-board"); !ok || floor != "1.2.3" {
		t.Fatalf("RequiredFloor = (%q,%v), want (1.2.3,true)", floor, ok)
	}
	if _, ok := r.RequiredFloor("acme/other"); ok {
		t.Fatal("a pack the roster does not name reported as required")
	}
}
