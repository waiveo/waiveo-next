package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/maaxton/waiveo-next/internal/datamodel"
)

// ContentReferences is the store's answer to "which content is this workspace
// actually using, and as of when": the set of content hashes every persisted
// playlist row names, and the generation that set was read at.
//
// Digests are HEX, with the `sha256:` prefix stripped — the content origin's own
// key (origin.Store.Has/Serve), not the `asset_ref` grammar the row carries — so
// the caller never re-derives the mapping between the two and cannot get it
// subtly wrong in one of the two places.
//
// PlaylistRows is the number of playlist rows the set was derived from. It is
// reported because "no content is referenced" and "no playlist row was read" look
// identical in Digests and are entirely different facts: the first is a workspace
// with nothing scheduled, the second is a read that saw nothing it should have.
type ContentReferences struct {
	Digests      map[string]bool
	Generation   int64
	PlaylistRows int
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
// A playlist body that will not decode ABORTS with an error and never reaches
// use. It is tempting to skip it — the workspace export does exactly that, and
// for an export it is right — but here a skipped row is a row whose references
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
	bodies, err := readBodies(ctx, s.db, string(KindPlaylist))
	if err != nil {
		return err
	}
	refs := ContentReferences{Digests: map[string]bool{}, Generation: gen, PlaylistRows: len(bodies)}
	for i, body := range bodies {
		var pl struct {
			Items []datamodel.PlaylistItem `json:"items"`
		}
		if err := json.Unmarshal(body, &pl); err != nil {
			return fmt.Errorf("store: decode playlist body %d for its content references: %w", i, err)
		}
		for _, it := range pl.Items {
			if it.AssetRef == "" {
				continue // a pack `playable` item resolves in the pack, not the origin.
			}
			refs.Digests[strings.TrimPrefix(it.AssetRef, "sha256:")] = true
		}
	}
	return use(refs)
}
