package wire

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestLeaseContentWithoutLayersMarshalsByteIdentical is the additive-field
// discipline guard for LeaseContent.Layers (native slide rendering, parity
// milestone 2): a LeaseContent built exactly as every pre-existing image/video
// item is — type/asset_ref/url/expires_at, no layers — must marshal with NO
// `layers` key, so it is byte-identical to before the field existed and rides
// the SAME LeaseSignedBytes signature (PLY-090) unchanged. A Lease carrying
// such an item likewise signs bytes that contain no `layers` substring at all.
func TestLeaseContentWithoutLayersMarshalsByteIdentical(t *testing.T) {
	item := LeaseContent{
		Type:      "image",
		AssetRef:  "sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		URL:       "https://origin.example/content/e3b0c4",
		ExpiresAt: 1752541200000,
	}
	raw, err := json.Marshal(item)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if _, ok := m["layers"]; ok {
		t.Errorf("a LeaseContent built without Layers marshaled a layers key; got %s", raw)
	}
	if len(m) != 4 {
		t.Errorf("bare image LeaseContent marshaled %d keys, want exactly 4 (type, asset_ref, url, expires_at); got %s", len(m), raw)
	}

	// The whole signed Lease scope must carry no `layers` bytes for a
	// no-layers content array — the signature over it is what it always was.
	lease := Lease{
		LeaseID:         "01J8Z4K4N5P6Q7R8S9T0V1W3A1",
		ScreenID:        "screen-1",
		ProgramRevision: "rev-1",
		Priority:        "scheduled",
		Display:         "content",
		Content:         []LeaseContent{item},
		IssuedAt:        1752541200000,
		ValidUntil:      1752541500000,
	}
	signed, err := LeaseSignedBytes(lease)
	if err != nil {
		t.Fatalf("LeaseSignedBytes: %v", err)
	}
	if strings.Contains(string(signed), "layers") {
		t.Errorf("LeaseSignedBytes of a no-layers Lease contains %q; the signed scope changed for an existing item shape:\n%s", "layers", signed)
	}
}

// TestSlideLeaseContentCarriesLayers asserts a `slide` LeaseContent DOES
// marshal a `layers` array carrying its authored layer stack, and that a
// wire round-trip preserves it — the positive counterpart of the omitempty
// guard above.
func TestSlideLeaseContentCarriesLayers(t *testing.T) {
	item := LeaseContent{
		Type:      "slide",
		ExpiresAt: 1752541200000,
		Layers: []Layer{
			{Kind: LayerKindRect, X: 0, Y: 0, W: 1920, H: 1080, Color: "#101020"},
			{Kind: LayerKindText, X: 100, Y: 80, W: 800, H: 120, Text: "Welcome", FontPx: 96, Color: "#FFFFFF", Align: "left"},
			{Kind: LayerKindClock, X: 1500, Y: 40, W: 360, H: 100, Text: "15:04", FontPx: 72},
		},
	}
	raw, err := json.Marshal(item)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if _, ok := m["layers"]; !ok {
		t.Fatalf("a slide LeaseContent marshaled no layers key; got %s", raw)
	}

	var back LeaseContent
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("round-trip Unmarshal: %v", err)
	}
	if len(back.Layers) != len(item.Layers) {
		t.Fatalf("round-tripped %d layers, want %d", len(back.Layers), len(item.Layers))
	}
	if back.Layers[1].Text != "Welcome" || back.Layers[1].Align != "left" || back.Layers[1].FontPx != 96 {
		t.Errorf("text layer did not round-trip: got %+v", back.Layers[1])
	}
	if back.Layers[2].Kind != LayerKindClock || back.Layers[2].Text != "15:04" {
		t.Errorf("clock layer did not round-trip: got %+v", back.Layers[2])
	}
}

// TestLayerOmitsUnusedKindFields asserts a Layer marshals its geometry and
// kind always, but only the kind-specific members it actually uses — a rect
// carries no `text`/`asset_ref`/`url`/`align`, and its geometry `0`s are
// present (a real coordinate, never dropped).
func TestLayerOmitsUnusedKindFields(t *testing.T) {
	rect := Layer{Kind: LayerKindRect, X: 0, Y: 0, W: 200, H: 100, Color: "#00ff00"}
	raw, err := json.Marshal(rect)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	for _, k := range []string{"kind", "x", "y", "w", "h", "color"} {
		if _, ok := m[k]; !ok {
			t.Errorf("rect Layer missing expected key %q; got %s", k, raw)
		}
	}
	for _, k := range []string{"text", "asset_ref", "url", "font_px", "align"} {
		if _, ok := m[k]; ok {
			t.Errorf("rect Layer marshaled unused key %q; got %s", k, raw)
		}
	}
	// The x=0 / y=0 geometry members are present as real coordinates.
	if string(m["x"]) != "0" || string(m["y"]) != "0" {
		t.Errorf("geometry zero coordinates were dropped: x=%s y=%s; got %s", m["x"], m["y"], raw)
	}
}

// TestValidateSlideLayers exercises ValidateSlideLayers across the full rule
// set (native slide rendering, parity milestone 2): the closed kind set,
// canvas-bounded geometry with positive w/h, the per-kind required fields, and
// #RRGGBB hex validation wherever a color appears.
func TestValidateSlideLayers(t *testing.T) {
	// A helper base geometry that fits the canvas, reused so each case varies
	// only the field under test.
	full := func(l Layer) Layer {
		if l.W == 0 {
			l.W = 100
		}
		if l.H == 0 {
			l.H = 100
		}
		return l
	}

	cases := []struct {
		name    string
		layers  []Layer
		wantErr bool
	}{
		// ---- valid ----
		{"text ok", []Layer{full(Layer{Kind: LayerKindText, Text: "hi"})}, false},
		{"text ok with optional hex color/font/align", []Layer{full(Layer{Kind: LayerKindText, Text: "hi", Color: "#abcdef", FontPx: 40, Align: "center"})}, false},
		{"rect ok", []Layer{full(Layer{Kind: LayerKindRect, Color: "#00FF00"})}, false},
		{"image ok", []Layer{full(Layer{Kind: LayerKindImage, AssetRef: "sha256:aa", URL: "https://o/x"})}, false},
		{"clock ok", []Layer{full(Layer{Kind: LayerKindClock, Text: "15:04:05"})}, false},
		{"multi-layer all valid", []Layer{
			full(Layer{Kind: LayerKindRect, Color: "#101020"}),
			full(Layer{Kind: LayerKindText, Text: "hi"}),
			full(Layer{Kind: LayerKindClock, Text: "3:04 PM"}),
		}, false},
		{"geometry flush to the far canvas edge", []Layer{{Kind: LayerKindRect, X: 1720, Y: 980, W: 200, H: 100, Color: "#ffffff"}}, false},

		// ---- structural ----
		{"empty layers", []Layer{}, true},
		{"nil layers", nil, true},
		{"unknown kind", []Layer{full(Layer{Kind: "video", Text: "hi"})}, true},
		{"empty kind", []Layer{full(Layer{Kind: "", Text: "hi"})}, true},

		// ---- geometry ----
		{"zero width", []Layer{{Kind: LayerKindRect, X: 0, Y: 0, W: 0, H: 100, Color: "#fff000"}}, true},
		{"zero height", []Layer{{Kind: LayerKindRect, X: 0, Y: 0, W: 100, H: 0, Color: "#fff000"}}, true},
		{"negative width", []Layer{{Kind: LayerKindRect, X: 0, Y: 0, W: -5, H: 100, Color: "#fff000"}}, true},
		{"negative x", []Layer{{Kind: LayerKindRect, X: -1, Y: 0, W: 100, H: 100, Color: "#fff000"}}, true},
		{"negative y", []Layer{{Kind: LayerKindRect, X: 0, Y: -1, W: 100, H: 100, Color: "#fff000"}}, true},
		{"far edge past canvas width", []Layer{{Kind: LayerKindRect, X: 1900, Y: 0, W: 100, H: 100, Color: "#fff000"}}, true},
		{"far edge past canvas height", []Layer{{Kind: LayerKindRect, X: 0, Y: 1000, W: 100, H: 200, Color: "#fff000"}}, true},

		// ---- per-kind required fields ----
		{"text missing text", []Layer{full(Layer{Kind: LayerKindText})}, true},
		{"clock missing format", []Layer{full(Layer{Kind: LayerKindClock})}, true},
		{"image missing asset_ref", []Layer{full(Layer{Kind: LayerKindImage, URL: "https://o/x"})}, true},
		{"image missing url", []Layer{full(Layer{Kind: LayerKindImage, AssetRef: "sha256:aa"})}, true},
		{"rect missing color", []Layer{full(Layer{Kind: LayerKindRect})}, true},

		// ---- color hex validation ----
		{"rect color not hex", []Layer{full(Layer{Kind: LayerKindRect, Color: "green"})}, true},
		{"rect color missing hash", []Layer{full(Layer{Kind: LayerKindRect, Color: "00ff00"})}, true},
		{"rect color too short", []Layer{full(Layer{Kind: LayerKindRect, Color: "#0f0"})}, true},
		{"rect color too long", []Layer{full(Layer{Kind: LayerKindRect, Color: "#00ff00ff"})}, true},
		{"rect color bad hex digit", []Layer{full(Layer{Kind: LayerKindRect, Color: "#00ff0g"})}, true},
		{"text optional color bad hex", []Layer{full(Layer{Kind: LayerKindText, Text: "hi", Color: "not-a-color"})}, true},

		// ---- a later layer's defect is caught, not just the first ----
		{"second layer invalid", []Layer{
			full(Layer{Kind: LayerKindText, Text: "hi"}),
			full(Layer{Kind: LayerKindRect}), // missing color
		}, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateSlideLayers(tc.layers)
			if tc.wantErr && err == nil {
				t.Errorf("ValidateSlideLayers(%+v) = nil, want an error", tc.layers)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("ValidateSlideLayers(%+v) = %v, want nil", tc.layers, err)
			}
		})
	}
}

// TestValidateAuthoredSlideLayersDiffersOnlyOnTheDerivedImageURL pins the exact
// and only difference between the two exported gates.
//
// The authoring gate exists because an image layer's `url` is DERIVED from the
// content origin at projection time, so requiring it of an operator would make
// every image layer unstorable. That is a narrow carve-out and it must STAY
// narrow: the danger of a second entry point is that it slowly becomes a second,
// looser rule set, and an authoring gate that accepted what the serving gate
// later drops would show an operator a stored slide that silently never reaches
// a screen.
//
// So this asserts the difference in both directions — the url case diverges, and
// a representative rule from every other family (kind, geometry, per-kind
// required field, color) answers IDENTICALLY through both.
func TestValidateAuthoredSlideLayersDiffersOnlyOnTheDerivedImageURL(t *testing.T) {
	imageNoURL := []Layer{{Kind: LayerKindImage, X: 0, Y: 0, W: 100, H: 100, AssetRef: "sha256:aa"}}
	if err := ValidateAuthoredSlideLayers(imageNoURL); err != nil {
		t.Errorf("the authoring gate rejected an image layer with no derived url: %v", err)
	}
	if err := ValidateSlideLayers(imageNoURL); err == nil {
		t.Error("the serving gate accepted an image layer with no url; a player could not fetch it")
	}

	// An image layer with NEITHER half is refused by both: the asset_ref is
	// authored, and without it there is nothing to derive a url from either.
	imageNothing := []Layer{{Kind: LayerKindImage, X: 0, Y: 0, W: 100, H: 100}}
	if err := ValidateAuthoredSlideLayers(imageNothing); err == nil {
		t.Error("the authoring gate accepted an image layer with no asset_ref")
	}

	shared := map[string][]Layer{
		"empty stack":         {},
		"unknown kind":        {{Kind: "video", X: 0, Y: 0, W: 10, H: 10}},
		"zero area":           {{Kind: LayerKindRect, X: 0, Y: 0, W: 0, H: 10, Color: "#ffffff"}},
		"off canvas":          {{Kind: LayerKindRect, X: 1900, Y: 0, W: 100, H: 10, Color: "#ffffff"}},
		"text with no text":   {{Kind: LayerKindText, X: 0, Y: 0, W: 10, H: 10}},
		"clock with no fmt":   {{Kind: LayerKindClock, X: 0, Y: 0, W: 10, H: 10}},
		"rect with no color":  {{Kind: LayerKindRect, X: 0, Y: 0, W: 10, H: 10}},
		"unparseable color":   {{Kind: LayerKindRect, X: 0, Y: 0, W: 10, H: 10, Color: "green"}},
		"a well-formed stack": {{Kind: LayerKindText, X: 0, Y: 0, W: 10, H: 10, Text: "hi"}},
	}
	for name, layers := range shared {
		t.Run(name, func(t *testing.T) {
			authored, served := ValidateAuthoredSlideLayers(layers), ValidateSlideLayers(layers)
			if (authored == nil) != (served == nil) {
				t.Errorf("the two gates disagree on a rule that is not the derived url: authored=%v served=%v", authored, served)
			}
		})
	}
}

// TestIsHexColor pins the #RRGGBB grammar isHexColor enforces.
func TestIsHexColor(t *testing.T) {
	valid := []string{"#000000", "#ffffff", "#FFFFFF", "#0a1B2c", "#123456"}
	invalid := []string{"", "#", "#fff", "000000", "#0000000", "#00000g", "#gggggg", "red", "#12345", " #123456"}
	for _, s := range valid {
		if !isHexColor(s) {
			t.Errorf("isHexColor(%q) = false, want true", s)
		}
	}
	for _, s := range invalid {
		if isHexColor(s) {
			t.Errorf("isHexColor(%q) = true, want false", s)
		}
	}
}

// TestContentRefWithoutLayersMarshalsByteIdentical is the additive-field guard
// for ContentRef.Layers (native slide rendering, parity milestone 2): a
// ContentRef built without layers marshals with no `layers` key, so an existing
// image/video screen_programs entry is byte-identical to before the field and
// its snapshot `hash` (REL-053) is unchanged; a slide entry carries the array.
func TestContentRefWithoutLayersMarshalsByteIdentical(t *testing.T) {
	bare := ContentRef{
		AssetRef:  "sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		URL:       "https://origin.example/content/e3b0c4",
		ExpiresAt: 1752541200000,
	}
	raw, err := json.Marshal(bare)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if _, ok := m["layers"]; ok {
		t.Errorf("a ContentRef built without Layers marshaled a layers key; got %s", raw)
	}

	slide := ContentRef{
		ExpiresAt:   1752541200000,
		ContentType: "slide",
		Layers:      []Layer{{Kind: LayerKindText, X: 10, Y: 10, W: 100, H: 40, Text: "hi"}},
	}
	rawSlide, err := json.Marshal(slide)
	if err != nil {
		t.Fatalf("Marshal slide: %v", err)
	}
	var back ContentRef
	if err := json.Unmarshal(rawSlide, &back); err != nil {
		t.Fatalf("round-trip Unmarshal slide: %v", err)
	}
	if len(back.Layers) != 1 || back.Layers[0].Text != "hi" {
		t.Errorf("slide ContentRef layers did not round-trip: got %+v", back.Layers)
	}
}
