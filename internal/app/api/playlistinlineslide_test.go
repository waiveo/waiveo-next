package api_test

import (
	"net/http"
	"testing"

	"github.com/maaxton/waiveo-next/internal/datamodel"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// playlistinlineslide_test.go covers the AUTHORING GATE over a `source: "slide"`
// playlist item's inline slide, over the real HTTP surface.
//
// The gap it closes is a defect CLASS, not one field. A slide is authored in two
// places — a cast's `slides[]` and this inline `slide` — and only the cast path
// was gated. The inline path stored whatever it was given on the reasoning that
// both projections revalidate at serve time, which they do: they DROP a slide
// whose layers do not validate. So an operator got a 201 and a screen one item
// short, with the only evidence in a Lease nobody reads — the same
// accepts-work-it-never-performs shape, restated one layer down.
//
// Both directions are asserted here, because a gate is only proved by the pair:
// the refusal, AND a valid inline slide still being accepted.

// inlineSlidePlaylist is a playlist whose single item is an inline slide with
// the given layer stack.
func inlineSlidePlaylist(scopeNode string, layers []wire.Layer) datamodel.Playlist {
	pl := playlistFixture(scopeNode, nil)
	pl.Items = []datamodel.PlaylistItem{{Source: "slide", Slide: &datamodel.Slide{Layers: layers}}}
	return pl
}

// TestAnInlineSlideRunsTheSameLayerGateACastsSlidesDo is the class assertion: a
// layer stack a cast would refuse is refused here too, with the playlist's own
// code, naming the item the operator typed into.
func TestAnInlineSlideRunsTheSameLayerGateACastsSlidesDo(t *testing.T) {
	for _, tc := range []struct {
		name  string
		layer wire.Layer
	}{
		// One case per rule family the shared gate enforces, so a gate wired to
		// only part of wire.ValidateAuthoredSlideLayers is still caught.
		{"an unknown kind", wire.Layer{Kind: "hologram", X: 0, Y: 0, W: 400, H: 100}},
		{"a zero-area geometry", wire.Layer{Kind: wire.LayerKindRect, X: 0, Y: 0, W: 0, H: 100, Color: "#ffffff"}},
		{"a geometry off the canvas", wire.Layer{Kind: wire.LayerKindRect, X: 1900, Y: 0, W: 400, H: 100, Color: "#ffffff"}},
		{"a rect with no color", wire.Layer{Kind: wire.LayerKindRect, X: 0, Y: 0, W: 400, H: 100}},
		{"a countdown with no target", wire.Layer{Kind: wire.LayerKindCountdown, X: 0, Y: 0, W: 400, H: 100, Text: "HH:MM:SS"}},
		{"a ping with no ping_name", wire.Layer{Kind: wire.LayerKindPing, X: 0, Y: 0, W: 400, H: 100, Text: "Call"}},
		{"items on a kind that is not nav", wire.Layer{Kind: wire.LayerKindRect, X: 0, Y: 0, W: 400, H: 100, Color: "#ffffff",
			Items: []wire.NavItem{{Label: "Rooms", TargetSlideID: "s2"}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			screenID := seedSchedulingScope(t, e)

			resp, raw := e.do(t, http.MethodPost, "/api/v1/playlists",
				rowCreateBody(t, inlineSlidePlaylist(screenID, []wire.Layer{tc.layer})), nil)
			if resp.StatusCode != http.StatusUnprocessableEntity {
				t.Fatalf("POST a playlist whose inline slide carries %s: status %d, want 422 — "+
					"the projections drop this slide at serve time, so accepting it stores a screen one item short (body %s)",
					tc.name, resp.StatusCode, raw)
			}
			p := assertProblem(t, resp, raw, "VALIDATION_FAILED")
			if !problemCarriesCode(p, "items[0].slide.layers", "PLAYLIST_ITEM_SLIDE_LAYERS_INVALID") {
				t.Errorf("want PLAYLIST_ITEM_SLIDE_LAYERS_INVALID on items[0].slide.layers, got %s", raw)
			}
		})
	}
}

// TestAnInlineSlideCannotCarryANavLayer is the instance the class fix covers:
// a `nav` item jumps by CAST-LOCAL slide id, and an inline slide is not in a
// cast — it has no siblings and no id of its own, so nothing it names can ever
// resolve.
//
// Before the gate this returned 201 with a target naming nothing anywhere, and
// the result on the wall was a menu item that takes focus, highlights, accepts a
// press and performs nothing: the player logs "nav target … matches no item in
// the current cast — ignored" and there is no other evidence at all.
func TestAnInlineSlideCannotCarryANavLayer(t *testing.T) {
	e := newEnv(t)
	screenID := seedSchedulingScope(t, e)

	nav := wire.Layer{Kind: wire.LayerKindNav, X: 0, Y: 0, W: 900, H: 100,
		Items: []wire.NavItem{{Label: "Rooms", TargetSlideID: "no-such-slide-anywhere"}}}

	resp, raw := e.do(t, http.MethodPost, "/api/v1/playlists",
		rowCreateBody(t, inlineSlidePlaylist(screenID, []wire.Layer{nav})), nil)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("POST a playlist whose inline slide carries a nav layer: status %d, want 422 (body %s)",
			resp.StatusCode, raw)
	}
	p := assertProblem(t, resp, raw, "VALIDATION_FAILED")
	if !problemCarriesCode(p, "items[0].slide.layers[0]", "PLAYLIST_ITEM_SLIDE_LAYERS_INVALID") {
		t.Errorf("want PLAYLIST_ITEM_SLIDE_LAYERS_INVALID on items[0].slide.layers[0], got %s", raw)
	}

	// The mirror direction: a nav whose target names a slide id that DOES exist
	// in some cast elsewhere in the deployment is refused just the same. The
	// refusal is about the id-space, not about the spelling — accepting this one
	// would leave the identical dead menu item behind, and would be the easy
	// half-fix (refuse the unknown target, accept the known-looking one).
	e.createCast(t, screenID) // its slides are "title" and "photo"
	navKnownLooking := wire.Layer{Kind: wire.LayerKindNav, X: 0, Y: 0, W: 900, H: 100,
		Items: []wire.NavItem{{Label: "Rooms", TargetSlideID: "photo"}}}
	resp2, raw2 := e.do(t, http.MethodPost, "/api/v1/playlists",
		rowCreateBody(t, inlineSlidePlaylist(screenID, []wire.Layer{navKnownLooking})), nil)
	if resp2.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("an inline-slide nav targeting a slide id that exists in a CAST was accepted: status %d — "+
			"an inline slide is not in that cast, so the jump still resolves to nothing (body %s)",
			resp2.StatusCode, raw2)
	}
}

// TestAValidInlineSlideIsStillAccepted is the control. Without it every case
// above would pass on a gate that refused every inline slide, which would break
// the one native-slide authoring path the platform has.
func TestAValidInlineSlideIsStillAccepted(t *testing.T) {
	e := newEnv(t)
	screenID := seedSchedulingScope(t, e)

	layers := []wire.Layer{
		{Kind: wire.LayerKindRect, X: 0, Y: 0, W: 1920, H: 1080, Color: "#101828"},
		{Kind: wire.LayerKindText, X: 120, Y: 100, W: 1680, H: 180, Text: "Welcome", FontPx: 128, Color: "#FFFFFF"},
		// A `ping` is legal on an inline slide: pressing it records a
		// screen.interaction and fires an automation, which needs no slide
		// id-space at all. Only the JUMP does.
		{Kind: wire.LayerKindPing, X: 120, Y: 900, W: 400, H: 100, Text: "Call staff", PingName: "call_service"},
	}
	resp, raw := e.do(t, http.MethodPost, "/api/v1/playlists",
		rowCreateBody(t, inlineSlidePlaylist(screenID, layers)), nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("a valid inline slide was refused: status %d (body %s)", resp.StatusCode, raw)
	}
}
