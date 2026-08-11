package datamodel

import (
	"encoding/json"
	"fmt"
	"testing"
)

// playlistslidelayers_test.go drives DAT-041's rule that a `source: "slide"`
// item's INLINE layer stack passes the same authoring gate a cast slide's does.
//
// It is written as a PARITY test on purpose, and the parity is the assertion. A
// test that only checked the inline side would pass just as happily if the two
// gates were two hand-written copies that agreed today — and this repo has
// already paid for that arrangement twice. What is pinned here is that ONE stack
// of layers gets ONE answer, whichever of the two authored shapes it is written
// into, so a rule added to wire.ValidateAuthoredSlideLayers reaches both or
// neither.
//
// The gap this closes was not "the inline path was more permissive". It was that
// the inline path ran NO layer validation at all: `checkPlaylistItems` inspected
// `content_type` and nothing else. Everything downstream — both content
// projections, the retention sweep, the derive work queue — already treated the
// two shapes identically, so the authoring surface was the only place they
// differed, and it differed in the direction that lets malformed rows into a
// system whose consumers are entitled to assume they cannot exist. One of them,
// a `derive` layer with no spec, then crashed the off-appliance renderer mid
// pass and discarded every other row's completed work with it.

// castWithLayers and playlistWithInlineSlide wrap ONE layer stack in each of the
// two authored shapes, as the raw JSON ValidateRows actually consumes.
func castWithLayers(layers string) json.RawMessage {
	return json.RawMessage(`{"id":"01J8ZCAST00000000000000001","scope_node":"01J8ZNODE0000000000000001","name":"c",` +
		`"slides":[{"id":"s1","layers":` + layers + `}],"revision":1,"created_at":1,"updated_at":1}`)
}

func playlistWithInlineSlide(layers string) json.RawMessage {
	return playlistWithItem(`{"source":"slide","slide":{"layers":` + layers + `}}`)
}

// TestAnInlineSlideAndACastSlideGetTheSameAnswer is the whole rule, driven from
// both sides of the shape it governs.
func TestAnInlineSlideAndACastSlideGetTheSameAnswer(t *testing.T) {
	const goodText = `[{"kind":"text","x":0,"y":0,"w":800,"h":120,"text":"Welcome"}]`

	cases := []struct {
		name    string
		layers  string
		wantErr bool
	}{
		{"a well-formed text layer", goodText, false},
		{
			// The case that crashed waiveo-derive. It was 422 on a cast and 201
			// inline; the renderer then dereferenced the nil spec and the process
			// died holding every other layer's finished PNG.
			"a derive layer with no spec",
			`[{"kind":"derive","x":0,"y":0,"w":400,"h":400}]`,
			true,
		},
		{
			// 422 on a cast, 201 inline: font_px is a text-only member, and a
			// control an operator sets that nothing reads is this codebase's
			// signature defect.
			"font_px on a qr spec",
			`[{"kind":"derive","x":0,"y":0,"w":400,"h":400,"derive":{"kind":"qr","data":"https://waiveo.local/x","font_px":64}}]`,
			true,
		},
		{
			"geometry off the canvas",
			`[{"kind":"rect","x":1900,"y":0,"w":100,"h":100,"color":"#ffffff"}]`,
			true,
		},
		{"an unknown layer kind", `[{"kind":"hologram","x":0,"y":0,"w":100,"h":100}]`, true},
		{"a zero-layer slide", `[]`, true},
		{
			// The authoring form of the gate, not the serve-time one: only the
			// content-addressed ref is authored, and the url is derived at
			// projection. An inline slide must not be held to a stricter rule than
			// a cast slide, either.
			"an image layer with an asset_ref and no url",
			`[{"kind":"image","x":0,"y":0,"w":640,"h":360,"asset_ref":"sha256:aa"}]`,
			false,
		},
		{
			"an image layer with no asset_ref",
			`[{"kind":"image","x":0,"y":0,"w":640,"h":360}]`,
			true,
		},
		{
			// A pending derive layer is the NORMAL first state of one: the
			// off-appliance rasterizer has not run yet. Refusing it would make the
			// thing the renderer exists to find unauthorable.
			"a well-formed derive layer with no asset yet",
			`[{"kind":"derive","x":0,"y":0,"w":400,"h":400,"derive":{"kind":"qr","data":"https://waiveo.local/pair/ABCD"}}]`,
			false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, castErrs := ValidateRows(RawRows{Casts: []json.RawMessage{castWithLayers(tc.layers)}})
			gotCast := hasErr(castErrs, "CAST_SLIDE_LAYERS_INVALID", "slides[0].layers")

			_, listErrs := ValidateRows(RawRows{Playlists: []json.RawMessage{playlistWithInlineSlide(tc.layers)}})
			gotInline := hasErr(listErrs, "PLAYLIST_ITEM_SLIDE_LAYERS_INVALID", "items[0].slide.layers")

			if gotCast != tc.wantErr {
				t.Errorf("cast slide rejected = %v, want %v; errs = %+v", gotCast, tc.wantErr, castErrs)
			}
			if gotInline != tc.wantErr {
				t.Errorf("inline slide rejected = %v, want %v; errs = %+v", gotInline, tc.wantErr, listErrs)
			}
			if gotCast != gotInline {
				t.Errorf("the two authored shapes disagreed about one layer stack "+
					"(cast rejected = %v, inline rejected = %v) — every consumer downstream treats them as one shape",
					gotCast, gotInline)
			}
		})
	}
}

// TestEveryFailingInlineSlideIsReported: the multi-error answer API-013 promises
// applies to inline slides too. An editor that had to re-submit once per bad
// item to discover the next one is that answer thrown away.
func TestEveryFailingInlineSlideIsReported(t *testing.T) {
	bad := `{"source":"slide","slide":{"layers":[{"kind":"hologram","x":0,"y":0,"w":10,"h":10}]}}`
	good := `{"source":"asset","asset_ref":"sha256:aa","content_type":"image"}`
	row := json.RawMessage(`{"id":"01J8ZPLAYLIST00000000000001","scope_node":"01J8ZNODE0000000000000001","name":"p","items":[` +
		bad + `,` + good + `,` + bad + `],"revision":1,"created_at":1,"updated_at":1}`)

	_, errs := ValidateRows(RawRows{Playlists: []json.RawMessage{row}})
	for _, i := range []int{0, 2} {
		field := fmt.Sprintf("items[%d].slide.layers", i)
		if !hasErr(errs, "PLAYLIST_ITEM_SLIDE_LAYERS_INVALID", field) {
			t.Errorf("no failure reported against %s; errs = %+v", field, errs)
		}
	}
	if hasErr(errs, "PLAYLIST_ITEM_SLIDE_LAYERS_INVALID", "items[1].slide.layers") {
		t.Errorf("the valid asset item was reported as a bad slide; errs = %+v", errs)
	}
}
