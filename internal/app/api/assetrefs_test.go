package api

// The in-transaction asset guard for the CAST kind (castAssetGuards, casts.go →
// rowAssetGuards, assetrefs.go).
//
// These are INTERNAL tests for the same reason the playlist ones beside them are
// (playlistassetguard_test.go): the interleaving the guard closes — content
// reclaimed between the api's pre-write asset check and the store write it gates
// — cannot be requested over HTTP. Reproducing it needs the guard set assembled
// the way the handler assembles it, with the reclamation placed inside the write
// transaction where a retention sweep holding the store's write lock lands it.
//
// The cast family had NEITHER half until now: no pre-write check and no
// in-transaction guard. A cast is the image-carrying authoring surface, so it is
// the family where both mattered most.

import (
	"context"
	"strings"
	"testing"

	"github.com/maaxton/waiveo-next/internal/app/store"
	"github.com/maaxton/waiveo-next/internal/datamodel"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// guardTestCastID is the cast row these cases write.
const guardTestCastID = "01J8ZG000000000000000000C1"

// guardTestCastBody is a one-slide cast whose only content is an image layer
// naming assetRef — the shape a projection turns into a fetch URL.
func guardTestCastBody(t *testing.T, assetRef string) []byte {
	t.Helper()
	return mustJSONBody(t, datamodel.Cast{
		ID: guardTestCastID, ScopeNode: guardTestScopeNode, Name: "Guarded Cast",
		Slides: []datamodel.CastSlide{{ID: "photo", Layers: []wire.Layer{
			{Kind: wire.LayerKindImage, X: 0, Y: 0, W: 1920, H: 1080, AssetRef: assetRef},
		}}},
	})
}

// TestCastAssetGuardRefusesAWriteWhoseAssetIsReclaimedMidTransaction is the
// guard doing its job on the cast kind: the write is refused and nothing is
// persisted.
func TestCastAssetGuardRefusesAWriteWhoseAssetIsReclaimedMidTransaction(t *testing.T) {
	content, st, assetRef, _ := newGuardTestFixture(t)
	srv := &server{content: content}
	body := guardTestCastBody(t, assetRef)

	guards := append([]store.WriteGuard{reclaimDuringWrite(content, assetRef)},
		castAssetGuards(srv, body)...)

	_, err := st.Create(context.Background(), store.KindCast, body, guards...)
	if err == nil {
		t.Fatal("a cast referencing content reclaimed inside its own write transaction was STORED: " +
			"the client is answered 201 and every screen that plays it fetches a 404")
	}
	var verr *store.ValidationError
	if !asValidationError(err, &verr) {
		t.Fatalf("write failed with %v, want a *store.ValidationError the api renders as 422/VALIDATION_FAILED", err)
	}
	if len(verr.Errors) != 1 || verr.Errors[0].Code != "REFERENCE_INVALID" {
		t.Fatalf("validation errors = %+v, want one REFERENCE_INVALID naming the asset", verr.Errors)
	}
	// The field names the LAYER, not just the row: a twelve-slide cast with one
	// broken image is only actionable if the error says which one.
	if verr.Errors[0].Field != "slides[0].layers[0].asset_ref" {
		t.Errorf("error field = %q, want the layer's own path slides[0].layers[0].asset_ref", verr.Errors[0].Field)
	}
	if !strings.Contains(verr.Errors[0].Message, assetRef) {
		t.Fatalf("error message %q does not name the missing asset %q", verr.Errors[0].Message, assetRef)
	}

	if _, found, err := st.Get(context.Background(), store.KindCast, guardTestCastID); err != nil || found {
		t.Fatalf("the refused write left a row behind: found=%v err=%v", found, err)
	}
}

// TestCastAssetGuardDisabledStoresADanglingReference is the same case with the
// guard REMOVED from the guard set — the state this codebase was in before it
// existed. It must succeed: a cast is stored naming content the origin cannot
// serve, which is precisely the defect.
func TestCastAssetGuardDisabledStoresADanglingReference(t *testing.T) {
	content, st, assetRef, _ := newGuardTestFixture(t)
	body := guardTestCastBody(t, assetRef)

	guards := []store.WriteGuard{reclaimDuringWrite(content, assetRef)}

	if _, err := st.Create(context.Background(), store.KindCast, body, guards...); err != nil {
		t.Fatalf("write with the guard disabled = %v, want it to succeed (that is the bug being pinned)", err)
	}
	if _, found, err := st.Get(context.Background(), store.KindCast, guardTestCastID); err != nil || !found {
		t.Fatalf("expected the unguarded write to store the row: found=%v err=%v", found, err)
	}
	if content.Has(strings.TrimPrefix(assetRef, "sha256:")) {
		t.Fatal("the stand-in reclamation did not remove the asset; this case is not reproducing the race")
	}
}

// TestCastWriteGuardsAreAssembledForTheCastKind proves the guard is reached
// through the SAME assembly the handlers use (resource.writeGuards) rather than
// merely existing as a function — the difference between a rule that is enforced
// and a rule that is written down.
func TestCastWriteGuardsAreAssembledForTheCastKind(t *testing.T) {
	content, _, assetRef, _ := newGuardTestFixture(t)
	body := guardTestCastBody(t, assetRef)
	rs := &resource{srv: &server{content: content}, cfg: castsConfig()}

	guards := rs.writeGuards(parseFields(body), "", body)
	if len(guards) != 1 {
		t.Fatalf("cast write guards = %d, want exactly 1 (the asset guard; this body carries no external_id)", len(guards))
	}
	if err := guards[0](nil); err != nil {
		t.Fatalf("the assembled guard rejected a present asset: %v", err)
	}
	if err := content.Remove(strings.TrimPrefix(assetRef, "sha256:")); err != nil {
		t.Fatalf("origin.Remove: %v", err)
	}
	if err := guards[0](nil); err == nil {
		t.Fatal("the assembled guard accepted a cast naming content the origin no longer holds")
	}
}

// TestEveryAssetBearingKindMountsBothHalvesOfTheAssetRule is the guard on the
// wiring itself, and it is what stops this defect from recurring on the next
// asset-bearing kind.
//
// store.AssetBearingKinds is the single list the retention sweep and the
// workspace export iterate. A kind on that list whose resourceConfig sets no
// `validate` and no `writeGuards` is a kind whose assets the platform protects
// and enumerates but never checks on write — which is exactly the state the cast
// family shipped in.
//
// It reads the configs the REAL mountAll registered (srv.families, the same
// source the audit middleware reads) rather than a list kept here, so it fails
// for a kind somebody adds later without anyone remembering this file exists.
func TestEveryAssetBearingKindMountsBothHalvesOfTheAssetRule(t *testing.T) {
	srv := &server{families: map[string]resourceConfig{}}
	rt, rootRT := newRouter(), newRouter()
	srv.mountAll(rt, rootRT, nil)

	configs := map[store.Kind]resourceConfig{}
	for _, cfg := range srv.families {
		configs[cfg.kind] = cfg
	}
	for _, kind := range store.AssetBearingKinds {
		cfg, ok := configs[kind]
		if !ok {
			t.Errorf("kind %q names content-origin assets but mounts no resource family, so nothing checks its references on write", kind)
			continue
		}
		if cfg.validate == nil {
			t.Errorf("kind %q mounts no pre-write validate hook: a row naming content that was never uploaded is accepted 201 and 404s on every screen", kind)
		}
		if cfg.writeGuards == nil {
			t.Errorf("kind %q mounts no writeGuards: the check-then-write race against the content retention sweep is open on it", kind)
		}
	}
}
