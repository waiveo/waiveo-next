package api

import (
	"fmt"

	"github.com/maaxton/waiveo-next/internal/app/store"
	"github.com/maaxton/waiveo-next/internal/datamodel"
)

// assetrefs.go is the authoring half of the one asset-reference rule: EVERY
// asset a written row names MUST already be present in the shared content
// origin (data-model/1 DAT-041). You cannot schedule content that was never
// uploaded, so a resolved Lease can never point a screen at bytes this origin
// 404s.
//
// It is written once, over store.RowAssetReferences, and mounted by every
// asset-bearing kind, because the rule was previously written once per kind and
// promptly stopped being one rule: a playlist item's `asset_ref` was refused
// 422 while the SAME digest, named by a cast's image layer or by an inline
// `source: "slide"` item, was accepted 201 and then minted into a fetch URL by
// both projections. The kind that carries most of a signage workspace's images
// was the kind with no gate.
//
// Both halves are here for the same reason they are both needed on a playlist:
//
//   - validateRowAssets is the PRE-write check, run before the store write is
//     opened, and it is what produces the API-013 multi-field `errors[]` naming
//     each unresolvable reference by its own JSON path.
//   - rowAssetGuards is the SAME check re-run INSIDE the store's write
//     transaction. Between the pre-write check and the write, the content
//     retention sweep (internal/feeder/contentgc) can reclaim an asset that was
//     present when the check ran; that interleaving stores a row whose content
//     resolves to a 404 while the client is told 201. The sweep holds the
//     store's write lock across its reference read AND its deletions
//     (store.WithContentReferences) and this guard runs under that same lock, so
//     the two are mutually exclusive: either the row commits first and the sweep
//     then sees its assets referenced and keeps them, or the sweep deletes first
//     and this guard refuses the row with the REFERENCE_INVALID the client would
//     have got a moment earlier. There is no interleaving in which a stored row
//     references reclaimed bytes.
//
// The check is deliberately scoped to the row BEING WRITTEN rather than to the
// resulting full row set (which is how the datamodel validators judge a write).
// An asset that goes missing outside this path — a corrupted file Open declines
// to load — would, under a whole-set check, make every subsequent write anywhere
// in the workspace fail on account of an unrelated row: a write-dead store, from
// a fault this rule was not written to detect.

// validateRowAssets is the pre-write asset check for one asset-bearing kind,
// wired as resourceConfig.validate. Each unresolvable reference yields a
// per-field REFERENCE_INVALID error naming the offending asset_ref AND the path
// that carries it (`items[2].asset_ref`, `slides[0].layers[1].asset_ref`), which
// writeValidationFailed renders as the api/1 `errors` extension — so the
// create/update is refused 422 before it reaches the store, with every failing
// reference reported at once rather than one per round trip.
//
// A body that will not decode returns no errors: its real failure surfaces on
// the store write, and inventing a second diagnosis for it here would answer the
// client with whichever of the two the api happened to ask first.
func validateRowAssets(srv *server, kind store.Kind, body []byte) []datamodel.Error {
	refs, err := store.RowAssetReferences(kind, body)
	if err != nil {
		return nil
	}
	var errs []datamodel.Error
	for _, ref := range refs {
		if srv.content != nil && srv.content.Has(ref.HexDigest()) {
			continue
		}
		errs = append(errs, datamodel.Error{
			Field: ref.Field,
			Code:  "REFERENCE_INVALID",
			Message: fmt.Sprintf(
				"asset_ref %s is not present in the content origin; upload the asset before scheduling it",
				ref.Ref),
		})
	}
	return errs
}

// rowAssetGuards re-checks, INSIDE the store's write transaction, that every
// asset the row being written names is still present in the content origin. It
// is the same rule validateRowAssets applies before the write, and it is not a
// redundant second copy of it — see this file's header for the interleaving it
// closes and why the pre-write check alone cannot.
//
// The existing row set is ignored: presence is a fact about the content origin,
// not about the other rows. The parameter is the WriteGuard contract's, and
// taking it is what buys the in-transaction position.
func rowAssetGuards(srv *server, kind store.Kind, body []byte) []store.WriteGuard {
	return []store.WriteGuard{func([]store.Resource) error {
		if errs := validateRowAssets(srv, kind, body); len(errs) > 0 {
			return &store.ValidationError{Errors: errs}
		}
		return nil
	}}
}
