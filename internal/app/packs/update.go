// update.go is the update check and its one sanctioned backward move.
//
// # What an update check does (marketplace/1 MKT-090)
//
// An install's default update policy is channel auto-tracking: the install's own
// PINNED trust channel is re-resolved, on each update check, to whatever version
// that registry source's channel pointer currently names. The pin is not a
// parameter the caller supplies — it is read off the install-record history
// (MKT-094's "the pack's newest record"), which is exactly what makes the record
// load-bearing rather than decorative. A caller names a pack; everything about
// HOW it is re-resolved comes from what was recorded when it was installed.
//
// An install with no trust channel pinned — the direct, out-of-band artifact
// path (MKT-094a) — is NOT auto-tracked and is refused here rather than
// defaulted onto a channel. Supplying one would be MKT-060a(b)'s prohibited
// silent tier selection arriving by another route: the host would be choosing
// how much review the code it is about to install has had.
//
// # What "a failed update rolls back" actually means here
//
// Nothing in this file compensates for a partially-applied install, because
// there is no way to produce one. Install is one transaction with one generation
// bump (store.InstallPack), and it UPSERTS: the prior version's row, bundle and
// records are replaced in place, never deleted first. So every refusal — a
// malformed artifact, a bad signature, a mislabeled identity, a manifest the
// engine rejects, a guard firing inside the transaction — leaves the previously
// installed version fully intact and running, with its own records and its own
// bundle. The rollback is structural; there is nothing to undo. That claim is
// not an assertion in a comment: TestUpdateFailureAtEveryStageLeavesPriorVersion
// drives a refusal at each stage and asserts the prior version, its files, its
// records and the store generation are all unchanged.
//
// The case that genuinely needs a backward move is a different one, and it is
// the one MKT-093 names: the version currently applied has itself become
// non-resolvable. Nothing about the box changed; the registry withdrew the
// version underneath it. That is the sole circumstance MKT-050 permits a
// channel's resolved version to decrease in, and for a required pack MKT-093
// mandates it.
//
// # What triggers a revert, and what deliberately does not
//
// ONLY a `yanked` entry for the currently-applied version (channel-index/1
// CHI-072, MKT-048). MKT-050's exception enumerates three causes — yanked,
// key-revoked, verified-attestation-revoked — and the latter two live in the
// revocation feed (CHI-070-074), which does not exist in this repository; this
// file does not invent a fetch of an unverified one.
//
// Everything else that can make a resolution fail is deliberately NOT a revert
// trigger: an index that will not load, a source that is gone, an entry that has
// simply disappeared, a hold that has not elapsed. Those are indistinguishable
// from "whoever serves the index chose not to answer", and treating them as
// revert triggers would hand that party a downgrade primitive: withhold the
// index, and the box walks itself back to an older version — every step of which
// succeeds. A yank is different in kind: it is a positive, published statement
// about a specific version inside the document, not the absence of one.
package packs

import (
	"context"
	"errors"

	"github.com/maaxton/waiveo-next/internal/app/store"
)

// UpdateAction is what an update check actually did.
type UpdateAction string

const (
	// UpdateUnchanged: the pinned channel still points at the installed
	// version. Nothing was written — deliberately: reinstalling the same
	// version on every check would bump the revision and append an install
	// record each time, silting up the very history MKT-093's last-known-good
	// reads its target out of.
	UpdateUnchanged UpdateAction = "unchanged"
	// UpdateApplied: the pointer named a different version and it installed.
	UpdateApplied UpdateAction = "updated"
	// UpdateReverted: the applied version had been yanked, and the most recent
	// still-resolvable version this install itself had previously applied was
	// reinstalled in its place (MKT-093, MKT-050's exception).
	UpdateReverted UpdateAction = "reverted"
)

// UpdateResult is one update check's outcome: what happened, the versions it
// moved between, and — when something was installed — the ordinary install
// summary.
type UpdateResult struct {
	Action      UpdateAction
	FromVersion string
	ToVersion   string
	Result      Result
}

// Update runs one update check for an installed pack (MKT-090), reverting to
// its last known good version if the applied one has been yanked (MKT-093).
//
// A pack that is not installed is store.ErrNotFound. Everything else it can
// return is a typed *ArtifactError the api layer already renders.
func (in *Installer) Update(ctx context.Context, packID string) (UpdateResult, error) {
	pack, found, err := in.store.GetPack(ctx, packID)
	if err != nil {
		return UpdateResult{}, err
	}
	if !found {
		return UpdateResult{}, store.ErrNotFound
	}
	if in.market == nil {
		return UpdateResult{}, artifactErr("MARKETPLACE_REF_UNRESOLVED",
			"this deployment has no registry sources configured, so %s cannot be re-resolved for an update", packID)
	}

	records, err := in.store.ListPackInstalls(ctx, packID)
	if err != nil {
		return UpdateResult{}, err
	}
	if len(records) == 0 {
		// Structurally unreachable: InstallPack refuses a spec with no record, so
		// an installed pack always has at least one. Refusing rather than
		// inventing a reference keeps that an invariant instead of a comment.
		return UpdateResult{}, artifactErr("MARKETPLACE_REF_UNRESOLVED",
			"%s carries no install record, so nothing names the channel it would be re-resolved through (marketplace/1 MKT-094)", packID)
	}
	pin := records[len(records)-1] // MKT-094: the newest record IS the pin.

	if pin.TrustChannel == "" {
		// MKT-094a: a direct install has no (pack, trust channel) pair to
		// re-resolve, and a host MUST NOT supply a default channel for it. The
		// reference an update check would build is therefore missing its own
		// required member (MKT-060a(b)), which is exactly TRUST_CHANNEL_UNKNOWN.
		return UpdateResult{}, artifactErr("TRUST_CHANNEL_UNKNOWN",
			"%s was installed from %q with no trust channel pinned, so it is not channel auto-tracked and a host never supplies a default one (marketplace/1 MKT-094a, MKT-060a)",
			packID, pin.Source)
	}

	ref := Ref{Source: pin.Source, PackID: packID, TrustChannel: pin.TrustChannel}
	if err := ref.validate(); err != nil {
		return UpdateResult{}, err
	}

	res, resolveErr := in.market.resolve(ctx, ref, in.channelMarkReader(ctx), withArtifact)
	if resolveErr != nil {
		return in.revertIfYanked(ctx, pack, records, ref, resolveErr)
	}

	if res.entry.Version == pack.Version {
		return UpdateResult{
			Action: UpdateUnchanged, FromVersion: pack.Version, ToVersion: pack.Version,
		}, nil
	}

	prov := provenanceOf(res, ref.TrustChannel)
	// An update is a compare-and-swap on the version it read: everything above
	// happened outside the store write lock. Without this a concurrent uninstall
	// would turn the update into a resurrection (the upsert would simply insert
	// the pack again), and a concurrent install would be overwritten by a
	// candidate chosen against a version that is no longer there.
	out, err := in.installExpecting(ctx, res.artifact, &prov, pack.Version)
	if err != nil {
		// Nothing landed. The prior version is still installed, in full — see
		// this file's header: install upserts inside one transaction, so a
		// refusal has nothing to compensate for.
		return UpdateResult{}, err
	}
	return UpdateResult{
		Action: UpdateApplied, FromVersion: pack.Version, ToVersion: out.Version, Result: out,
	}, nil
}

// revertIfYanked is the update check's failure branch. It performs MKT-093's
// revert-to-known-good ONLY when the currently-applied version's own entry is
// yanked; for every other resolution failure it returns that failure unchanged
// and leaves the installed version alone (see this file's header for why the
// distinction is load-bearing rather than fussy).
func (in *Installer) revertIfYanked(ctx context.Context, pack store.Pack, records []store.PackInstallRecord, ref Ref, cause error) (UpdateResult, error) {
	if !in.appliedVersionYanked(ctx, ref, pack.Version) {
		return UpdateResult{}, cause
	}

	target, ok := in.lastKnownGood(ctx, pack, records)
	if !ok {
		return UpdateResult{}, artifactErr("MARKETPLACE_REF_UNRESOLVED",
			"%s version %s has been yanked and no earlier version this deployment successfully applied is still resolvable, so there is no last-known-good to revert to (marketplace/1 MKT-093)",
			pack.ID, pack.Version)
	}

	prov := provenanceOf(target.res, target.record.TrustChannel)
	// ExpectPriorVersion is the in-transaction half of this decision: everything
	// above was read outside the write lock, so the store re-asserts at commit
	// time that the pack row STILL carries the yanked version this revert was
	// decided against. If another writer moved it — or removed the pack — the
	// revert refuses rather than installing an older version over whatever
	// arrived meanwhile.
	out, err := in.installExpecting(ctx, target.res.artifact, &prov, pack.Version)
	if err != nil {
		return UpdateResult{}, err
	}
	return UpdateResult{
		Action: UpdateReverted, FromVersion: pack.Version, ToVersion: out.Version, Result: out,
	}, nil
}

// appliedVersionYanked reports whether the entry for the version currently
// applied is now `yanked` — the one revert trigger this deployment can observe
// (MKT-048/CHI-072; the revocation feed MKT-050's other two causes live in does
// not exist here).
//
// It resolves entry-only: the question is about the entry's status, and the
// bytes are irrelevant to it.
func (in *Installer) appliedVersionYanked(ctx context.Context, ref Ref, appliedVersion string) bool {
	if !ValidVersion(appliedVersion) {
		// A version with no position in MKT-050a's order cannot be looked up or
		// compared. Not yanked as far as this deployment can tell — no revert,
		// and the update check's own failure stands.
		return false
	}
	pinned := ref
	pinned.Version = appliedVersion
	_, err := in.market.resolve(ctx, pinned, in.channelMarkReader(ctx), entryOnly)
	if err == nil {
		return false
	}
	var aerr *ArtifactError
	return asArtifactError(err, &aerr) && aerr.Code == "ARTIFACT_YANKED"
}

// revertTarget is one candidate last-known-good version: the record that proves
// it was successfully applied, and the resolution that proves it still is.
type revertTarget struct {
	record store.PackInstallRecord
	res    resolution
}

// lastKnownGood picks MKT-093's revert target: "the most recent version of that
// same pack_id that was itself successfully applied and has not itself since
// been revoked".
//
//   - *successfully applied* comes from the install-record history, which is
//     append-only and holds one row per applied install (MKT-094b) — never from
//     whatever the index currently offers, which is the substitution MKT-050
//     forbids;
//   - *not since revoked* is decided by resolving the candidate now, through the
//     record's own source and trust channel: a yanked entry refuses, and so does
//     one that has been withdrawn outright;
//   - a candidate below a required pack's floor is skipped (MKT-093b) — the
//     store would refuse it in the transaction anyway, and skipping it here
//     lets the walk reach a target that IS admissible instead of failing on the
//     first one that is not.
func (in *Installer) lastKnownGood(ctx context.Context, pack store.Pack, records []store.PackInstallRecord) (revertTarget, bool) {
	floor, required := in.store.RequiredFloor(pack.ID)
	tried := map[string]bool{pack.Version: true}
	for i := len(records) - 1; i >= 0; i-- {
		rec := records[i]
		if tried[rec.ResolvedVersion] {
			continue
		}
		tried[rec.ResolvedVersion] = true
		if rec.TrustChannel == "" || rec.Source == store.SourceDirect {
			// A direct install's bytes were never in a registry, so there is
			// nothing to re-resolve them from. It is evidence a version once ran,
			// not a version this deployment can obtain again.
			continue
		}
		if required && !meetsFloor(rec.ResolvedVersion, floor) {
			continue
		}
		ref := Ref{Source: rec.Source, PackID: pack.ID, TrustChannel: rec.TrustChannel, Version: rec.ResolvedVersion}
		if err := ref.validate(); err != nil {
			continue
		}
		res, err := in.market.resolve(ctx, ref, in.channelMarkReader(ctx), withArtifact)
		if err != nil {
			continue
		}
		return revertTarget{record: rec, res: res}, true
	}
	return revertTarget{}, false
}

// provenanceOf builds the install provenance a resolution contributes
// (MKT-094/094a), identically for an update, a revert, and a first install —
// there is one install path and it is fed the same way from all three.
func provenanceOf(res resolution, trustChannel string) provenance {
	return provenance{
		Source:       res.source.ID,
		TrustChannel: trustChannel,
		EntryID:      res.entry.ArtifactID,
		EntryKind:    res.entry.Kind,
		EntrySubtype: res.entry.Subtype,
		EntryVersion: res.entry.Version,
		EntryDigest:  res.entry.Digest,
		StaleSource:  res.source.StaleSource,
		ViaPointer:   res.viaPointer,
	}
}

// requiredFloorRefusal maps the store's typed floor refusal onto this pipeline's
// stable REQUIRED_PACK_FLOOR ArtifactError (MKT-093b), so the api layer renders
// it through the same errors[] discriminant every other PACK_* refusal uses.
func requiredFloorRefusal(err error) error {
	var ferr *store.RequiredPackFloorError
	if !errors.As(err, &ferr) {
		return err
	}
	if ferr.Version == "" {
		return artifactErr("REQUIRED_PACK_FLOOR",
			"%s is a required pack on this deployment (floor %s) and cannot be uninstalled (marketplace/1 MKT-093b)",
			ferr.PackID, ferr.Floor)
	}
	return artifactErr("REQUIRED_PACK_FLOOR",
		"%s is a required pack on this deployment whose floor version is %s; version %s is below it and MUST NOT be installed on any path (marketplace/1 MKT-093b)",
		ferr.PackID, ferr.Floor, ferr.Version)
}

// priorVersionChangedRefusal maps the store's compare-and-swap refusal onto the
// same POINTER_ROLLBACK_REJECTED a sequential downgrade yields: from the
// caller's side the outcome is identical — a version chosen against an older
// reading did not replace what is actually installed.
func priorVersionChangedRefusal(packID, expected string) error {
	return artifactErr("POINTER_ROLLBACK_REJECTED",
		"the update of %s was refused: it was resolved against version %s, which is no longer what is installed (marketplace/1 MKT-050)",
		packID, expected)
}
