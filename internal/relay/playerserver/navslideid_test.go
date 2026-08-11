package playerserver

import (
	"testing"

	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// TestSetServedProgramCarriesTheCastLocalSlideID pins the last hop of the field
// a `nav` jump target resolves against.
//
// The offline-continuity serve path rebuilds every Lease content item field by
// field from a persisted screen-program entry, so a field it forgets is a field
// that reaches the player as empty — here, a menu the player can draw and focus
// and never resolve, with nothing anywhere reporting a fault. Every other pointer
// on the reference has this same shape, which is why this is asserted rather than
// assumed.
func TestSetServedProgramCarriesTheCastLocalSlideID(t *testing.T) {
	srv, _, token := programTestServer(t)

	srv.SetServedProgram(2, wire.ScreenProgram{
		ScreenID: testScreenIDA, ProgramRevision: "rev-nav", Priority: "scheduled", Display: "content",
		Content: []wire.ContentRef{{
			ContentType: "slide",
			SlideID:     "home",
			Layers: []wire.Layer{{
				Kind: wire.LayerKindNav, X: 0, Y: 900, W: 1200, H: 120,
				Items: []wire.NavItem{{Label: "Rooms", TargetSlideID: "rooms"}},
			}},
		}, {
			ContentType: "slide",
			SlideID:     "rooms",
			Layers:      []wire.Layer{{Kind: wire.LayerKindText, X: 0, Y: 0, W: 400, H: 100, Text: "Rooms"}},
		}},
	})

	resp, raw := doProgram(t, srv, token, []string{"image", "video", "slide"})
	if resp.StatusCode != 200 {
		t.Fatalf("program status = %d, want 200", resp.StatusCode)
	}
	var lease LeaseResponse
	remarshal(t, raw, &lease)
	if len(lease.Content) != 2 {
		t.Fatalf("lease carries %d item(s), want 2", len(lease.Content))
	}
	if lease.Content[0].SlideID != "home" || lease.Content[1].SlideID != "rooms" {
		t.Fatalf("lease slide ids = %q/%q, want home/rooms", lease.Content[0].SlideID, lease.Content[1].SlideID)
	}
	if got := lease.Content[0].Layers[0].Items[0].TargetSlideID; got != "rooms" {
		t.Fatalf("the nav item's target did not survive the conversion: %q", got)
	}
	// And the target must actually resolve against what was served: a menu whose
	// target names no item in the same Lease is the dead end this whole design
	// refuses.
	if lease.Content[1].SlideID != lease.Content[0].Layers[0].Items[0].TargetSlideID {
		t.Fatal("the nav target resolves to no item in the served Lease")
	}
}

// TestInteractiveLayersSurviveTheServeGate: a `ping` layer must pass
// wire.ValidateSlideLayers on the serving side too, or the whole slide is
// DROPPED at issuance and the button never reaches the screen at all.
func TestInteractiveLayersSurviveTheServeGate(t *testing.T) {
	srv, _, token := programTestServer(t)

	srv.SetServedProgram(2, wire.ScreenProgram{
		ScreenID: testScreenIDA, ProgramRevision: "rev-ping", Priority: "scheduled", Display: "content",
		Content: []wire.ContentRef{{
			ContentType: "slide",
			Layers: []wire.Layer{
				{Kind: wire.LayerKindText, X: 0, Y: 0, W: 400, H: 100, Text: "Need help?"},
				{Kind: wire.LayerKindPing, X: 0, Y: 200, W: 400, H: 100, Text: "Press OK", PingName: "call_service"},
				// An ordinary widget made INTERACTIVE by a ping name — tracker
				// row 3.7's mechanism, which must survive the same gate.
				{Kind: wire.LayerKindEntity, X: 0, Y: 400, W: 400, H: 100,
					EntityID: "01J8Z3K4N5P6Q7R8S9T0V1W2Z3", PingName: "toggle_tv"},
			},
		}},
	})

	resp, raw := doProgram(t, srv, token, []string{"image", "video", "slide"})
	if resp.StatusCode != 200 {
		t.Fatalf("program status = %d, want 200", resp.StatusCode)
	}
	var lease LeaseResponse
	remarshal(t, raw, &lease)
	if len(lease.Content) != 1 {
		t.Fatalf("the slide was dropped at the serve gate: %d item(s)", len(lease.Content))
	}
	layers := lease.Content[0].Layers
	if len(layers) != 3 {
		t.Fatalf("served %d layer(s), want 3", len(layers))
	}
	if layers[1].PingName != "call_service" || layers[2].PingName != "toggle_tv" {
		t.Fatalf("ping names did not survive the conversion: %+v", layers)
	}
}
