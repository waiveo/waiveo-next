package playerserver

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/maaxton/waiveo-next/internal/app/store"
	"github.com/maaxton/waiveo-next/internal/datamodel"
	"github.com/maaxton/waiveo-next/internal/feeder/contenturl"
	"github.com/maaxton/waiveo-next/internal/feeder/snapshot"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// videoe2e_test.go is parity row 2.5's END-TO-END proof, and it is deliberately
// the only test in this package that reaches back into the authoring stack.
//
// Every other test here hands SetServedProgram a wire.ScreenProgram it built by
// hand, which proves the CONVERSION and nothing about whether anything upstream
// can produce the input. That is exactly the gap this row existed in: the player
// could render a video and the relay could serve one, and no authoring path
// could produce one, so the capability was complete at both ends and absent as a
// whole. A test that constructs the ScreenProgram itself would have passed
// throughout.
//
// So this drives the REAL chain: an operator's playlist row written through the
// real store, resolved by the real scheduling engine, projected by the real
// feeder projection, installed through the real SetServedProgram, and pulled
// over a real HTTP program request by a paired player — asserting the `type` on
// the Lease the screen actually receives.

// videoE2EAsset is the content-addressed ref the scheduled clip is stored under.
const videoE2EAsset = "sha256:c0ffee2222222222222222222222222222222222222222222222222222222222"

// seedScreenScopeNode is the `screen`-kind node the demo seed places its screen
// and playlist under. A cast has to be placed at a node that resolves in the
// seeded tree for the store to accept it, and the seed's own constant is
// unexported, so it is spelled here — the store's write validation fails loudly
// if the two ever diverge, which is the check that keeps this literal honest.
const seedScreenScopeNode = "01J8Z4DEM0SCREENF1RSTPH0TN"

// videoE2EOrigin is the content origin the projection derives fetch urls
// against — the screen fetches bytes there DIRECTLY, never through this relay
// (REL-140), which the url assertion below is also checking.
const videoE2EOrigin = "https://origin.example"

// seededStoreWithItems opens an in-memory store, seeds the demo workspace
// against videoE2EAsset, and replaces the demo playlist's items with `items`.
// It is the authoring half of the chain: everything after it reads only what
// this wrote.
func seededStoreWithItems(t *testing.T, items []datamodel.PlaylistItem) *store.Store {
	t.Helper()
	s, err := store.Open(":memory:", store.WallClockMs)
	if err != nil {
		t.Fatalf("store.Open(:memory:): %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	ctx := context.Background()
	if err := s.SeedDemo(ctx, videoE2EAsset); err != nil {
		t.Fatalf("SeedDemo: %v", err)
	}
	pls, err := s.List(ctx, store.KindPlaylist, store.ListFilter{})
	if err != nil || len(pls) != 1 {
		t.Fatalf("list playlists: %v (got %d)", err, len(pls))
	}
	patch, err := json.Marshal(map[string]any{"items": items})
	if err != nil {
		t.Fatalf("marshal playlist items: %v", err)
	}
	if _, err := s.Update(ctx, store.KindPlaylist, pls[0].ID, pls[0].Revision, patch); err != nil {
		t.Fatalf("update playlist items: %v", err)
	}
	return s
}

// seedContentInstant is midday in the seeded site's own zone — inside the demo
// seed's 06:00–22:00 content daypart, so the seeded screen resolves to
// display:content. Stated as a fixed instant rather than read from the wall
// clock: this chain's answer is time-dependent by construction (dayparts), and a
// test that ran at 03:00 would legitimately resolve to a blank screen.
func seedContentInstant(t *testing.T) int64 {
	t.Helper()
	loc, err := time.LoadLocation("America/Chicago")
	if err != nil {
		t.Fatalf("LoadLocation: %v", err)
	}
	ts, err := time.ParseInLocation("2006-01-02 15:04:05", "2026-07-15 12:00:00", loc)
	if err != nil {
		t.Fatalf("ParseInLocation: %v", err)
	}
	return ts.UnixMilli()
}

// serveProjectedProgram runs the real feeder projection over s and installs the
// seeded screen's entry through SetServedProgram, returning a paired player's
// channel token. This is the whole app→relay half of the chain, with nothing
// hand-built in between.
func serveProjectedProgram(t *testing.T, s *store.Store) (*Server, string) {
	t.Helper()

	ds, err := s.DesiredState(context.Background())
	if err != nil {
		t.Fatalf("DesiredState: %v", err)
	}
	programs, errs := snapshot.DeriveScreenPrograms(ds, contenturl.Signer{Base: videoE2EOrigin}, seedContentInstant(t))
	if len(errs) != 0 {
		t.Fatalf("DeriveScreenPrograms reported %+v", errs)
	}

	certPEM, _, priv, _ := testRelaySigningIdentity(t)
	grant := testGrantForScreen(store.SeedScreenID)
	srv, err := NewServer(certPEM, []wire.PairingGrant{grant}, WallClockMs)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	srv.SetSigningKey(priv)

	found := false
	for _, p := range programs {
		srv.SetServedProgram(1, p)
		if p.ScreenID == store.SeedScreenID {
			found = true
		}
	}
	if !found {
		t.Fatalf("the projection produced no program for the seeded screen %s; got %+v", store.SeedScreenID, programs)
	}

	_, raw := doPair(t, srv, PairingRequest{
		HardwareID:    "hw-video-0001",
		GrantSelector: grant.GrantID,
		Capabilities:  Capabilities{ContentTypes: []string{"image", "video", "slide"}, PlayerVersion: "3.0.0"},
	})
	var pairResp PairingResponse
	remarshal(t, raw, &pairResp)
	if pairResp.ChannelToken == "" {
		t.Fatalf("pairing did not yield a channel_token: %+v", pairResp)
	}
	return srv, pairResp.ChannelToken
}

// leaseAsPlayerDeclaring pulls the program over the real HTTP surface as a
// player declaring exactly `contentTypes` would, and returns the Lease. It takes
// the declared set where render_test.go's leaseFor fixes it, because the
// capability gate is itself one of the things under test here.
func leaseAsPlayerDeclaring(t *testing.T, srv *Server, token string, contentTypes []string) LeaseResponse {
	t.Helper()
	resp, body := doProgram(t, srv, token, contentTypes)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %v", resp.StatusCode, body)
	}
	var lease LeaseResponse
	remarshal(t, body, &lease)
	return lease
}

// TestScheduledVideoReachesThePlayerAsAVideoLease is parity row 2.5.
//
// An operator uploads a clip and schedules it (`content_type: "video"` on an
// asset playlist item). The Lease the screen pulls must say `video`, or the
// player draws the MP4 as a still Poster and the wall shows nothing — the
// failure this whole row exists to close, and one that produces no error
// anywhere along the way.
func TestScheduledVideoReachesThePlayerAsAVideoLease(t *testing.T) {
	s := seededStoreWithItems(t, []datamodel.PlaylistItem{
		{Source: datamodel.PlaylistSourceAsset, AssetRef: videoE2EAsset, ContentType: datamodel.PlaylistContentTypeVideo},
	})
	srv, token := serveProjectedProgram(t, s)

	lease := leaseAsPlayerDeclaring(t, srv, token, []string{"image", "video", "slide"})
	if lease.Display != "content" {
		t.Fatalf("display = %q, want content (the seeded daypart is on at this instant)", lease.Display)
	}
	if len(lease.Content) != 1 {
		t.Fatalf("content has %d items, want 1; got %+v", len(lease.Content), lease.Content)
	}
	item := lease.Content[0]
	if item.Type != "video" {
		t.Errorf("content[0].type = %q, want \"video\" — the player switches Poster/Video on this field, so an `image` here is a black screen where a clip should play", item.Type)
	}
	if item.AssetRef != videoE2EAsset {
		t.Errorf("content[0].asset_ref = %q, want %q", item.AssetRef, videoE2EAsset)
	}
	wantURL := videoE2EOrigin + "/content/" + strings.TrimPrefix(videoE2EAsset, "sha256:")
	if item.URL != wantURL {
		t.Errorf("content[0].url = %q, want %q (the content origin directly, never this relay — REL-140)", item.URL, wantURL)
	}
}

// TestScheduledVideoIsWithheldFromAPlayerThatCannotPlayOne pins the capability
// gate on the video path specifically (PLY-013/096, filterContentTypes). A
// player declaring only `image` must be served no video item — an older or
// image-only device is transparently never handed content it cannot render,
// rather than being sent a video and left to fail on-device.
func TestScheduledVideoIsWithheldFromAPlayerThatCannotPlayOne(t *testing.T) {
	s := seededStoreWithItems(t, []datamodel.PlaylistItem{
		{Source: datamodel.PlaylistSourceAsset, AssetRef: videoE2EAsset, ContentType: datamodel.PlaylistContentTypeVideo},
	})
	srv, token := serveProjectedProgram(t, s)

	lease := leaseAsPlayerDeclaring(t, srv, token, []string{"image"})
	if len(lease.Content) != 0 {
		t.Errorf("content has %d items, want 0 for a player that declared only `image`; got %+v", len(lease.Content), lease.Content)
	}
}

// TestScheduledSlideWithAVideoLayerReachesThePlayer is parity row 1.5-video's
// end-to-end proof: an authored cast whose slide carries a `video` layer reaches
// the screen as a slide item whose video layer has a fetch url and survives the
// serve-time gate.
//
// The two assertions are one claim seen from both ends. A video layer whose url
// no projection derived fails wire.ValidateSlideLayers, and SetServedProgram
// DROPS a slide that fails it — so the observable symptom of the bug is not a
// broken video, it is a Lease with no content at all. Asserting the item count
// first is what makes the layer assertion meaningful.
func TestScheduledSlideWithAVideoLayerReachesThePlayer(t *testing.T) {
	s := seededStoreWithItems(t, []datamodel.PlaylistItem{
		{Source: datamodel.PlaylistSourceAsset, AssetRef: videoE2EAsset},
	})

	// Author the cast through the real store write path (validation, referential
	// integrity, the shared authored-layer gate) and point the playlist at it.
	ctx := context.Background()
	castBody, err := json.Marshal(datamodel.Cast{
		ID: "01J8ZCASTE2EV1DE0000000001", ScopeNode: seedScreenScopeNode, Name: "Promo",
		Slides: []datamodel.CastSlide{{ID: "promo", DurationMS: 15000, Layers: []wire.Layer{
			{Kind: wire.LayerKindRect, X: 0, Y: 0, W: 1920, H: 1080, Color: "#000000"},
			{Kind: wire.LayerKindVideo, X: 160, Y: 90, W: 1600, H: 900, AssetRef: videoE2EAsset},
		}}},
	})
	if err != nil {
		t.Fatalf("marshal cast: %v", err)
	}
	created, err := s.Create(ctx, store.KindCast, castBody)
	if err != nil {
		t.Fatalf("create cast: %v", err)
	}
	pls, err := s.List(ctx, store.KindPlaylist, store.ListFilter{})
	if err != nil || len(pls) != 1 {
		t.Fatalf("list playlists: %v (got %d)", err, len(pls))
	}
	patch, err := json.Marshal(map[string]any{"items": []datamodel.PlaylistItem{
		{Source: datamodel.PlaylistSourceCast, CastID: created.ID},
	}})
	if err != nil {
		t.Fatalf("marshal playlist items: %v", err)
	}
	if _, err := s.Update(ctx, store.KindPlaylist, pls[0].ID, pls[0].Revision, patch); err != nil {
		t.Fatalf("update playlist items: %v", err)
	}

	srv, token := serveProjectedProgram(t, s)
	lease := leaseAsPlayerDeclaring(t, srv, token, []string{"image", "video", "slide"})

	if len(lease.Content) != 1 {
		t.Fatalf("content has %d items, want 1 (the cast's one slide); a video layer with no derived url makes the whole slide fail validation and be dropped. Got %+v", len(lease.Content), lease.Content)
	}
	item := lease.Content[0]
	if item.Type != "slide" {
		t.Fatalf("content[0].type = %q, want slide", item.Type)
	}
	if len(item.Layers) != 2 {
		t.Fatalf("slide carried %d layers, want 2; got %+v", len(item.Layers), item.Layers)
	}
	video := item.Layers[1]
	if video.Kind != wire.LayerKindVideo {
		t.Fatalf("layers[1].kind = %q, want %q", video.Kind, wire.LayerKindVideo)
	}
	wantURL := videoE2EOrigin + "/content/" + strings.TrimPrefix(videoE2EAsset, "sha256:")
	if video.URL != wantURL {
		t.Errorf("the served video layer's url = %q, want %q", video.URL, wantURL)
	}
	if video.AssetRef != videoE2EAsset {
		t.Errorf("the served video layer's asset_ref = %q, want %q — the player verifies the fetched bytes against it", video.AssetRef, videoE2EAsset)
	}
}
