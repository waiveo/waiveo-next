package store

import (
	"context"
	"fmt"
)

// ContentReferences is the store's answer to "which content is this workspace
// actually using, and as of when": the set of content hashes every persisted
// asset-bearing row names, and the generation that set was read at.
//
// "Asset-bearing" is AssetBearingKinds and the projection is RowAssetReferences
// (assetrefs.go) — the SAME one the api's authoring guard and the workspace
// export use. It covers a playlist item's `asset_ref`, the image layers of an
// inline `source: "slide"` item, and the image layers of every slide of a cast.
// Reading only playlist items' `asset_ref` here — which is what this did — made
// the sweep blind to the images a cast plays, so a scheduled cast's content was
// reclaimable while a screen was showing it.
//
// Digests are HEX, with the `sha256:` prefix stripped — the content origin's own
// key (origin.Store.Has/Serve), not the `asset_ref` grammar the row carries — so
// the caller never re-derives the mapping between the two and cannot get it
// subtly wrong in one of the two places.
//
// SourceRows is the number of asset-bearing rows the set was derived from. It is
// reported because "no content is referenced" and "no row was read" look
// identical in Digests and are entirely different facts: the first is a workspace
// with nothing scheduled, the second is a read that saw nothing it should have.
//
// It cannot GATE that difference, and pretending otherwise would break the
// sweep's main job. Zero rows is a legitimate workspace state — content uploaded
// and not yet scheduled — and reclaiming those assets is exactly what the sweeper
// exists to do. A guard on this number would refuse the ordinary case to defend
// against a hypothetical one.
//
// What it can do is make the difference visible AT THE MOMENT IT STOPS BEING
// recoverable. Reclamation is permanent, so a sweep that deletes content while
// its reference set came from zero rows is the one combination where a silent
// read fault and an empty workspace produce the same irreversible outcome.
// The feeder logs precisely that pair (cmd/waiveo-feeder/contentsweep.go), which
// is the only place the two can be told apart — by a human who knows whether that
// workspace had anything scheduled.
//
// The other half of that fault is already closed at the source rather than here:
// a body that will not decode ABORTS the read (see the failure posture below), so
// the residue this number covers is a read that returned no rows without error.
type ContentReferences struct {
	Digests    map[string]bool
	Generation int64
	SourceRows int
}

// WithContentReferences runs use with the workspace's content-reference set,
// while HOLDING THE STORE'S WRITE LOCK. It exists for exactly one caller: the
// retention sweep that reclaims unreferenced content.
//
// # Why the write lock, and not a plain read
//
// A sweep that merely READ the reference set would be unsafe no matter how
// careful the rest of it was, because a playlist naming an asset can be authored
// at any instant. The api layer checks that an asset_ref resolves in the content
// origin BEFORE opening the store write (api/1's pre-write validation), so this
// interleaving is available to any concurrent sweep:
//
//	api:    asset X is present in the origin  ✓
//	sweep:  X is referenced by nothing → delete X
//	api:    commit a playlist referencing X   ✓ 201 Created
//
// The client is told its playlist was accepted, and every screen that plays it
// fetches a 404. Holding the write lock across the reference read AND the
// caller's deletion makes that interleaving unrepresentable: the api's write
// transaction and this callback are mutually exclusive, so either the playlist
// commits first (and its asset is in Digests, so the sweep keeps it) or the
// deletion happens first (and the write's in-transaction asset guard refuses the
// playlist, 422, storing nothing). The in-transaction guard is the other half of
// this and is not optional — without it the api's pre-write check can still be
// stale by the time the row is written.
//
// # What use MUST NOT do
//
// use runs under the write lock, so it MUST NOT call back into this Store —
// any Create/Update/Delete/Get/List deadlocks. It MUST also be bounded: every
// api write on this box waits behind it. The sweep caps its own per-run
// deletions for this reason.
//
// # Failure posture
//
// An asset-bearing row body (a playlist or a cast) that will not decode ABORTS
// with an error and never reaches use. It is tempting to skip it — the workspace
// export does exactly that, and for an export it is right — but here a skipped row is a row whose references
// are silently treated as absent, which is how a sweep deletes content that is
// very much in use. A reference set this store cannot vouch for is not a
// reference set.
func (s *Store) WithContentReferences(ctx context.Context, use func(ContentReferences) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	gen, err := readGeneration(ctx, s.db)
	if err != nil {
		return err
	}
	refs := ContentReferences{Digests: map[string]bool{}, Generation: gen}
	// Every kind that can name content, through the one shared projection —
	// never a hand-written per-kind reader here. A kind read by the export or
	// guarded on write but not swept is content this deletes while a screen is
	// playing it, and that asymmetry is only avoidable if all three read the
	// same list.
	for _, kind := range AssetBearingKinds {
		bodies, err := readBodies(ctx, s.db, string(kind))
		if err != nil {
			return err
		}
		refs.SourceRows += len(bodies)
		for i, body := range bodies {
			rowRefs, err := RowAssetReferences(kind, body)
			if err != nil {
				return fmt.Errorf("store: %s row %d: %w", kind, i, err)
			}
			for _, r := range rowRefs {
				refs.Digests[r.HexDigest()] = true
			}
		}
	}
	return use(refs)
}
