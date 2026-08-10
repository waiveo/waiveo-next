package playerserver

import (
	"net/http"
	"testing"

	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// slideScreenID is the screen identity row (DAT-004a) these slide cases bind
// their pairing grant to and install their persisted screen_programs entry for
// — the same-id discipline offlineScreenID documents (a grant redeeming into a
// different screen would be served the terminal default).
const slideScreenID = "01J8Z3K4N5P6Q7R8S9T0V1SLID"

// validSlideLayers is a well-formed slide layer stack (a full-canvas rect
// backdrop, a title, an image, and a live clock) that wire.ValidateSlideLayers
// accepts — the shape the milestone's seeded proof slide carries.
func validSlideLayers() []wire.Layer {
	return []wire.Layer{
		{Kind: wire.LayerKindRect, X: 0, Y: 0, W: wire.SlideCanvasWidth, H: wire.SlideCanvasHeight, Color: "#101020"},
		{Kind: wire.LayerKindText, X: 120, Y: 100, W: 1000, H: 160, Text: "The Hanger", FontPx: 120, Color: "#FFFFFF", Align: "left"},
		{Kind: wire.LayerKindImage, X: 120, Y: 300, W: 640, H: 360, AssetRef: "sha256:" + "ab", URL: "https://198.51.100.20/cas/logo"},
		{Kind: wire.LayerKindClock, X: 1400, Y: 60, W: 460, H: 140, Text: "3:04 PM", FontPx: 88, Color: "#F0F0F0"},
	}
}

// newSlideServer builds a Server with one redeemable grant and a persisted
// screen_programs entry carrying a single `slide` content item whose layers are
// `layers`, redeems a channel token, and returns everything a slide case drives
// through. The served ContentRef's ContentType is "slide", so SetServedProgram
// runs it through wire.ValidateSlideLayers.
func newSlideServer(t *testing.T, layers []wire.Layer) (srv *Server, token string) {
	t.Helper()

	certPEM, _, priv, _ := testRelaySigningIdentity(t)
	grant := testGrantForScreen(slideScreenID)

	srv, err := NewServer(certPEM, []wire.PairingGrant{grant}, WallClockMs)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	srv.SetSigningKey(priv)

	served := wire.ScreenProgram{
		ScreenID:        slideScreenID,
		ProgramRevision: "rev-slide-1",
		Priority:        "scheduled",
		Display:         "content",
		Content: []wire.ContentRef{{
			ContentType: "slide",
			ExpiresAt:   1752545000000,
			Layers:      layers,
		}},
	}
	srv.SetServedProgram(1, served)

	_, raw := doPair(t, srv, PairingRequest{
		HardwareID:    "hw-slide-0001",
		GrantSelector: grant.GrantID,
		Capabilities:  Capabilities{ContentTypes: []string{"image", "video", "slide"}, PlayerVersion: "1.0.0"},
	})
	var pairResp PairingResponse
	remarshal(t, raw, &pairResp)
	if pairResp.ChannelToken == "" {
		t.Fatalf("pairing did not yield a channel_token: %+v", pairResp)
	}
	return srv, pairResp.ChannelToken
}

// TestSlideContentRefRoundTripsToLeaseWithLayers is touch-point 3's core
// assertion (native slide rendering, parity milestone 2): a persisted `slide`
// ContentRef converts through SetServedProgram to a Lease content item whose
// `type` is "slide" and whose `layers` are the authored stack, unmodified —
// served only when the requesting player declares "slide".
func TestSlideContentRefRoundTripsToLeaseWithLayers(t *testing.T) {
	srv, token := newSlideServer(t, validSlideLayers())

	resp, body := doProgram(t, srv, token, []string{"image", "video", "slide"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %v", resp.StatusCode, body)
	}

	var lease LeaseResponse
	remarshal(t, body, &lease)

	if len(lease.Content) != 1 {
		t.Fatalf("content has %d items, want 1 (the slide); got %+v", len(lease.Content), lease.Content)
	}
	item := lease.Content[0]
	if item.Type != "slide" {
		t.Errorf("content[0].type = %q, want %q", item.Type, "slide")
	}
	want := validSlideLayers()
	if len(item.Layers) != len(want) {
		t.Fatalf("content[0].layers has %d entries, want %d; got %+v", len(item.Layers), len(want), item.Layers)
	}
	for i := range want {
		if item.Layers[i] != want[i] {
			t.Errorf("content[0].layers[%d] = %+v, want %+v (carried unmodified)", i, item.Layers[i], want[i])
		}
	}
	// The layers must survive the SIGNED scope, not just the response struct:
	// re-validate them exactly as the producer did.
	if err := wire.ValidateSlideLayers(item.Layers); err != nil {
		t.Errorf("served slide layers no longer validate: %v", err)
	}
}

// TestSlideNotServedToIncapablePlayer confirms the capability gate
// (PLY-013/096, filterContentTypes): a player whose declared content_types omit
// "slide" is served NO slide item — an older player transparently never
// receives one, even though the screen's persisted program is a slide.
func TestSlideNotServedToIncapablePlayer(t *testing.T) {
	srv, token := newSlideServer(t, validSlideLayers())

	// The program request declares only image/video — no "slide".
	resp, body := doProgram(t, srv, token, []string{"image", "video"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %v", resp.StatusCode, body)
	}

	var lease LeaseResponse
	remarshal(t, body, &lease)

	if len(lease.Content) != 0 {
		t.Errorf("content has %d items, want 0 (slide excluded for a player that did not declare it); got %+v", len(lease.Content), lease.Content)
	}
}

// TestMalformedSlideIsNotServed confirms SetServedProgram DROPS a slide item
// whose layers do not validate (native slide rendering, parity milestone 2): a
// malformed slide never reaches the wire, so even a slide-capable player is
// served no content for it — a player has no defined behavior for a bad layer.
func TestMalformedSlideIsNotServed(t *testing.T) {
	// A rect layer with no color — wire.ValidateSlideLayers rejects it.
	malformed := []wire.Layer{
		{Kind: wire.LayerKindRect, X: 0, Y: 0, W: 200, H: 100},
	}
	// Guard the test's own premise: these layers really are invalid.
	if err := wire.ValidateSlideLayers(malformed); err == nil {
		t.Fatal("precondition: expected the malformed layers to fail ValidateSlideLayers")
	}

	srv, token := newSlideServer(t, malformed)

	resp, body := doProgram(t, srv, token, []string{"image", "video", "slide"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %v", resp.StatusCode, body)
	}

	var lease LeaseResponse
	remarshal(t, body, &lease)

	if len(lease.Content) != 0 {
		t.Errorf("content has %d items, want 0 (malformed slide dropped at conversion, not served); got %+v", len(lease.Content), lease.Content)
	}
}
