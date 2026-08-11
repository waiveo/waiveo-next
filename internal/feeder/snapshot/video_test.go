package snapshot

import (
	"strings"
	"testing"

	"github.com/maaxton/waiveo-next/internal/app/store"
	"github.com/maaxton/waiveo-next/internal/datamodel"
	"github.com/maaxton/waiveo-next/internal/feeder/contenturl"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// video_test.go drives the two halves of "a video reaches a screen" on the
// app-signed side of the projection (parity rows 2.5 and 1.5-video):
//
//  1. a plain playlist item whose asset is a VIDEO projects to a content
//     reference that SAYS so, so the relay stamps `type: "video"` on the Lease
//     and the player plays it instead of drawing it as a still;
//  2. a slide's `video` layer gets its fetch url derived exactly as an image
//     layer's does, so the stack passes the shared serve-time gate instead of
//     being silently dropped.
//
// Both are regressions waiting to happen in the same way — by omission, with no
// error anywhere — which is why each asserts the projected value rather than
// merely that the projection ran.

// videoAsset is the content-addressed ref these cases schedule. The bytes behind
// it are irrelevant to a projection (nothing here reads content), so this is a
// fixed digest rather than a real encode.
const videoAsset = "sha256:c0ffee1111111111111111111111111111111111111111111111111111111111"

// TestVideoPlaylistItemProjectsAVideoContentRef is parity row 2.5's producer
// half: an authored `content_type: "video"` asset item, written through the REAL
// store and resolved through the REAL scheduling engine, reaches the REL-061
// screen-program as a video reference with its content-origin url derived.
//
// Before the field existed this was structurally impossible — every asset item
// projected with no content_type, which every consumer reads as `image`. The
// assertion that matters is therefore the content_type itself: reverting the
// projection to drop it leaves an otherwise perfectly healthy program that plays
// an MP4 as a still frame.
func TestVideoPlaylistItemProjectsAVideoContentRef(t *testing.T) {
	s := seededStore(t, videoAsset)
	replaceSeedPlaylistItems(t, s, []datamodel.PlaylistItem{
		{Source: datamodel.PlaylistSourceAsset, AssetRef: videoAsset, ContentType: datamodel.PlaylistContentTypeVideo},
	})

	programs, errs := DeriveScreenPrograms(desiredState(t, s), contenturl.Signer{Base: parityOrigin}, contentInstant(t))
	if len(errs) != 0 {
		t.Fatalf("DeriveScreenPrograms reported %+v", errs)
	}
	prog := programForScreen(t, programs, store.SeedScreenID)

	if len(prog.Content) != 1 {
		t.Fatalf("content = %d items, want 1; got %+v", len(prog.Content), prog.Content)
	}
	item := prog.Content[0]
	if item.ContentType != datamodel.PlaylistContentTypeVideo {
		t.Errorf("content[0].content_type = %q, want %q — a scheduled video that does not say it is a video is served, and drawn, as a still image",
			item.ContentType, datamodel.PlaylistContentTypeVideo)
	}
	if item.AssetRef != videoAsset {
		t.Errorf("content[0].asset_ref = %q, want %q", item.AssetRef, videoAsset)
	}
	wantURL := parityOrigin + "/content/" + strings.TrimPrefix(videoAsset, "sha256:")
	if item.URL != wantURL {
		t.Errorf("content[0].url = %q, want %q", item.URL, wantURL)
	}
}

// TestAssetItemWithNoContentTypeStillProjectsWithNone is the additive half: the
// field changed nothing for content authored before it existed. An item stating
// no content_type must project none at all — not `image` spelled out — because
// the REL-061 reference's own content_type is `omitempty` and a newly-appearing
// key would change the canonical bytes, and therefore the snapshot hash and
// every program_revision, of workspaces nobody touched.
func TestAssetItemWithNoContentTypeStillProjectsWithNone(t *testing.T) {
	s := seededStore(t, videoAsset)
	replaceSeedPlaylistItems(t, s, []datamodel.PlaylistItem{
		{Source: datamodel.PlaylistSourceAsset, AssetRef: videoAsset},
	})

	programs, errs := DeriveScreenPrograms(desiredState(t, s), contenturl.Signer{Base: parityOrigin}, contentInstant(t))
	if len(errs) != 0 {
		t.Fatalf("DeriveScreenPrograms reported %+v", errs)
	}
	prog := programForScreen(t, programs, store.SeedScreenID)
	if len(prog.Content) != 1 {
		t.Fatalf("content = %d items, want 1; got %+v", len(prog.Content), prog.Content)
	}
	if prog.Content[0].ContentType != "" {
		t.Errorf("content[0].content_type = %q, want \"\" (absent) for an item that states none", prog.Content[0].ContentType)
	}
}

// TestSlideVideoLayerGetsItsFetchURLDerived is parity row 1.5-video's producer
// half. A `video` layer is authored with only its content-addressed asset_ref
// (its url does not exist until a projection mints one against the content
// origin), so a projection that derives urls for image layers alone leaves the
// video layer url-less — wire.ValidateSlideLayers then rejects the whole stack
// and the slide is DROPPED from the program.
//
// That failure is completely silent: the screen simply plays one fewer slide.
// So the test asserts both halves — the slide survived at all, and its video
// layer carries the derived url — because the second without the first would
// pass over an empty content array.
func TestSlideVideoLayerGetsItsFetchURLDerived(t *testing.T) {
	s := seededStore(t, videoAsset)
	castID := writeCast(t, s, datamodel.Cast{
		ID: "01J8ZCASTV1DE0000000000001", ScopeNode: castScopeNode, Name: "Promo",
		Slides: []datamodel.CastSlide{{ID: "promo", DurationMS: 15000, Layers: []wire.Layer{
			{Kind: wire.LayerKindRect, X: 0, Y: 0, W: 1920, H: 1080, Color: "#000000"},
			{Kind: wire.LayerKindVideo, X: 160, Y: 90, W: 1600, H: 900, AssetRef: videoAsset},
			{Kind: wire.LayerKindText, X: 160, Y: 40, W: 800, H: 60, Text: "Now showing", FontPx: 48, Color: "#FFFFFF"},
		}}},
	})
	replaceSeedPlaylistItems(t, s, []datamodel.PlaylistItem{
		{Source: datamodel.PlaylistSourceCast, CastID: castID},
	})

	programs, errs := DeriveScreenPrograms(desiredState(t, s), contenturl.Signer{Base: parityOrigin}, contentInstant(t))
	if len(errs) != 0 {
		t.Fatalf("DeriveScreenPrograms reported %+v", errs)
	}
	prog := programForScreen(t, programs, store.SeedScreenID)

	if len(prog.Content) != 1 {
		t.Fatalf("content = %d items, want 1 (the cast's single slide); a video layer whose url was never derived makes the whole slide fail validation and vanish. Got %+v", len(prog.Content), prog.Content)
	}
	layers := prog.Content[0].Layers
	if len(layers) != 3 {
		t.Fatalf("slide carried %d layers, want 3; got %+v", len(layers), layers)
	}
	video := layers[1]
	if video.Kind != wire.LayerKindVideo {
		t.Fatalf("layers[1].kind = %q, want %q (layer order is z-order and must be preserved)", video.Kind, wire.LayerKindVideo)
	}
	wantURL := parityOrigin + "/content/" + strings.TrimPrefix(videoAsset, "sha256:")
	if video.URL != wantURL {
		t.Errorf("the video layer's derived url = %q, want %q", video.URL, wantURL)
	}
	// The stack the screen is handed must pass the same gate the relay re-applies
	// on the way to a Lease; anything else is a slide that validates here and is
	// dropped there.
	if err := wire.ValidateSlideLayers(layers); err != nil {
		t.Errorf("the projected slide layers do not pass the serve-time gate: %v", err)
	}
}

// TestSlideVideoLayerIsDroppedWhenNoContentOriginIsStated is the honest-degrade
// half, and it is asserted for video for the same reason it holds for image: a
// deployment with no content origin has no url to state, and this projection
// fabricates none. The slide is dropped rather than served pointing at nothing —
// a screen that plays one fewer slide is recoverable; a screen stuck on a video
// element that can never load is not.
func TestSlideVideoLayerIsDroppedWhenNoContentOriginIsStated(t *testing.T) {
	s := seededStore(t, videoAsset)
	castID := writeCast(t, s, datamodel.Cast{
		ID: "01J8ZCASTV1DE0000000000002", ScopeNode: castScopeNode, Name: "Promo",
		Slides: []datamodel.CastSlide{{ID: "promo", Layers: []wire.Layer{
			{Kind: wire.LayerKindVideo, X: 0, Y: 0, W: 1920, H: 1080, AssetRef: videoAsset},
		}}},
	})
	replaceSeedPlaylistItems(t, s, []datamodel.PlaylistItem{
		{Source: datamodel.PlaylistSourceCast, CastID: castID},
	})

	programs, errs := DeriveScreenPrograms(desiredState(t, s), contenturl.Signer{Base: ""}, contentInstant(t))
	if len(errs) != 0 {
		t.Fatalf("DeriveScreenPrograms reported %+v", errs)
	}
	prog := programForScreen(t, programs, store.SeedScreenID)
	if len(prog.Content) != 0 {
		t.Errorf("content = %+v, want none: with no content origin the video layer has no url and the slide must be dropped, never served with a dead reference", prog.Content)
	}
}
