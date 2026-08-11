package api_test

import (
	"encoding/json"
	"net/http"
	"testing"

	apiv1 "github.com/maaxton/waiveo-next/api/gen/go"

	"github.com/maaxton/waiveo-next/internal/datamodel"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// video_test.go drives the AUTHORING surface for video (parity rows 2.5 and
// 1.5-video) over the real mounted mux.
//
// It earns its place separately from the projection tests because the gate it
// exercises is a different one, and it is the gate that would have made the
// whole feature unreachable while every projection test passed: api/openapi.yaml
// declares these request bodies, and internal/app/api enforces that declaration
// at request time with `additionalProperties: false` and per-field enums. A
// `content_type` the document does not declare is not ignored — it is a 422. So
// a playlist item or a slide layer the wire supports and the document does not
// is a capability that exists everywhere except where an operator can reach it.

// TestAPlaylistItemMayDeclareItsContentType is parity row 2.5's authoring half:
// POST /playlists carrying `content_type: "video"` is accepted and reads back
// carrying it, so the value the projections read is a value an operator can
// actually store.
func TestAPlaylistItemMayDeclareItsContentType(t *testing.T) {
	e := newEnv(t)
	screenID := seedSchedulingScope(t, e)

	pl := playlistFixture(screenID, nil)
	pl.Items = []datamodel.PlaylistItem{
		{Source: datamodel.PlaylistSourceAsset, AssetRef: playlistFixtureAssetRef, ContentType: datamodel.PlaylistContentTypeVideo},
	}
	raw := e.createOK(t, "/api/v1/playlists", rowCreateBody(t, pl))

	var stored datamodel.Playlist
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatalf("decode the created playlist: %v (body %s)", err, raw)
	}
	if len(stored.Items) != 1 {
		t.Fatalf("stored playlist has %d items, want 1; body %s", len(stored.Items), raw)
	}
	if stored.Items[0].ContentType != datamodel.PlaylistContentTypeVideo {
		t.Errorf("items[0].content_type read back as %q, want %q — a value the surface accepts but does not persist is a setting that silently does nothing",
			stored.Items[0].ContentType, datamodel.PlaylistContentTypeVideo)
	}
}

// TestAPlaylistItemContentTypeIsAClosedVocabulary: a value outside image/video
// is refused at the surface. It rides untouched onto the Lease `type`, where the
// relay's content-type filter would drop the item for a type no player declares
// — a screen showing nothing, diagnosable only from a Lease no operator reads.
func TestAPlaylistItemContentTypeIsAClosedVocabulary(t *testing.T) {
	e := newEnv(t)
	screenID := seedSchedulingScope(t, e)

	body := mustJSON(t, map[string]any{
		"scope_node": screenID,
		"name":       "Demo Playlist",
		"items": []map[string]any{
			{"source": "asset", "asset_ref": playlistFixtureAssetRef, "content_type": "audio"},
		},
	})
	resp, raw := e.do(t, http.MethodPost, "/api/v1/playlists", body, nil)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("POST an unknown content_type: status %d, want 422 (body %s)", resp.StatusCode, raw)
	}
	assertProblem(t, resp, raw, "VALIDATION_FAILED")
}

// TestACastSlideMayCarryAVideoLayer is parity row 1.5-video's authoring half:
// POST /casts carrying a `video` layer is accepted and reads back with the layer
// intact.
//
// This is the case that fails if api/openapi.yaml's SlideLayer `kind` enum is
// not extended alongside the wire's closed kind set — a failure mode with no
// symptom in any Go test of the projections, because they never pass through the
// declared-schema gate. The layer would validate, project, serve and render, and
// no operator could ever create one.
func TestACastSlideMayCarryAVideoLayer(t *testing.T) {
	e := newEnv(t)
	screenID := seedSchedulingScope(t, e)

	cast := datamodel.Cast{
		ScopeNode: screenID,
		Name:      "Promo",
		Slides: []datamodel.CastSlide{{
			ID: "promo", DurationMS: 15000,
			Layers: []wire.Layer{
				{Kind: wire.LayerKindRect, X: 0, Y: 0, W: 1920, H: 1080, Color: "#000000"},
				{Kind: wire.LayerKindVideo, X: 160, Y: 90, W: 1600, H: 900, AssetRef: playlistFixtureAssetRef},
			},
		}},
	}
	resp, raw := e.do(t, http.MethodPost, "/api/v1/casts", rowCreateBody(t, cast), nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST a cast with a video layer: status %d, want 201 (body %s)", resp.StatusCode, raw)
	}
	stored := decodeCast(t, raw)
	if len(stored.Slides) != 1 || len(stored.Slides[0].Layers) != 2 {
		t.Fatalf("stored cast shape unexpected: %+v", stored.Slides)
	}
	video := stored.Slides[0].Layers[1]
	if video.Kind != wire.LayerKindVideo {
		t.Errorf("slides[0].layers[1].kind read back as %q, want %q", video.Kind, wire.LayerKindVideo)
	}
	if video.AssetRef != playlistFixtureAssetRef {
		t.Errorf("the video layer's asset_ref read back as %q, want %q", video.AssetRef, playlistFixtureAssetRef)
	}
}

// TestACastVideoLayerMustNameUploadedContent proves the video layer is covered by
// the SAME asset-reference guard an image layer is — the guard whose blind spots
// have twice been the expensive kind (a cast's images were once invisible to it,
// which made them both un-checked at write time and reclaimable by the retention
// sweep while a screen played them).
//
// It is a property of store.RowAssetReferences being kind-agnostic rather than of
// anything written for video, which is exactly why it is worth an assertion: the
// claim "a new content-bearing kind is covered for free" is only true while
// nothing narrows that projection to `image`.
func TestACastVideoLayerMustNameUploadedContent(t *testing.T) {
	e := newEnv(t)
	screenID := seedSchedulingScope(t, e)

	cast := datamodel.Cast{
		ScopeNode: screenID,
		Name:      "Promo",
		Slides: []datamodel.CastSlide{{
			ID: "promo",
			Layers: []wire.Layer{
				{Kind: wire.LayerKindVideo, X: 0, Y: 0, W: 1920, H: 1080, AssetRef: "sha256:0000000000000000000000000000000000000000000000000000000000000000"},
			},
		}},
	}
	resp, raw := e.do(t, http.MethodPost, "/api/v1/casts", rowCreateBody(t, cast), nil)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("POST a cast whose video layer names un-uploaded content: status %d, want 422 (body %s)", resp.StatusCode, raw)
	}
	p := assertProblem(t, resp, raw, "VALIDATION_FAILED")
	if !problemNamesField(p, "slides[0].layers[0].asset_ref") {
		t.Errorf("the refusal does not name the unresolvable video reference: %s", raw)
	}
}

// TestTheDeclaredSurfaceCarriesTheVideoVocabulary pins the DOCUMENT, and it
// exists because the two surfaces this feature crosses are enforced very
// differently — a gap that is easy to mistake for coverage.
//
// A cast runs the FULL declared-schema gate, so TestACastSlideMayCarryAVideoLayer
// above genuinely fails if `video` is missing from SlideLayer's `kind` enum. A
// playlist runs only the MEMBER half (scheduling.go: its per-field rules live in
// the datamodel validators, which report every failing member at once), so a
// nested `content_type` is accepted by the handler whether or not the document
// declares it — every behavioural test above passes with the declaration
// removed.
//
// That does not make the declaration optional. api/openapi.yaml is the single
// source every generated client is built from, so an undeclared field is a field
// no typed client can send: the console would have no `content_type` on its
// PlaylistItem, and scheduling a video would be reachable only by hand-rolling
// JSON. Asserting against the GENERATED types rather than the YAML text is
// deliberate — it is the artifact clients actually consume, so it proves both
// that the document declares the field and that the codegen output in the tree
// is regenerated rather than stale.
func TestTheDeclaredSurfaceCarriesTheVideoVocabulary(t *testing.T) {
	video := apiv1.PlaylistItemContentTypeVideo
	item := apiv1.PlaylistItem{
		Source:      apiv1.PlaylistItemSourceAsset,
		AssetRef:    &[]string{playlistFixtureAssetRef}[0],
		ContentType: &video,
	}
	if item.ContentType == nil || *item.ContentType != "video" {
		t.Fatalf("the generated PlaylistItem's content_type does not carry `video`: %+v", item.ContentType)
	}
	if !video.Valid() {
		t.Error("the generated content_type enum does not accept `video`")
	}
	if !apiv1.PlaylistItemContentTypeImage.Valid() {
		t.Error("the generated content_type enum does not accept `image`")
	}
	if apiv1.PlaylistItemContentType("audio").Valid() {
		t.Error("the generated content_type enum accepts a value outside the closed set")
	}

	// The slide layer's kind vocabulary, from the same generated surface: a
	// `video` kind a client cannot name is a layer no Studio could ever author.
	if !apiv1.SlideLayerKindVideo.Valid() {
		t.Error("the generated SlideLayer kind enum does not accept `video`")
	}
}
