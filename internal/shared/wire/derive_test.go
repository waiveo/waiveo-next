package wire

import (
	"reflect"
	"strings"
	"testing"
)

// derive_test.go pins the rasterized fallback's wire contract: the closed spec
// vocabulary, the digest that decides what is stale, and the ASYMMETRY between
// the authoring gate and the serving gate that lets a player learn nothing new.

func qrLayer() Layer {
	return Layer{
		Kind: LayerKindDerive, X: 100, Y: 100, W: 400, H: 400,
		Derive: &DeriveSpec{Kind: DeriveKindQR, Data: "https://waiveo.local/pair", ECLevel: "M"},
	}
}

// TestADeriveLayerIsAuthorableButNeverServable is the asymmetry, asserted from
// BOTH sides.
//
// One side alone is not enough. If only the authoring side were tested, a
// `derive` kind accidentally admitted by the serve gate would sail through and a
// player would be handed a kind it has no node for — it would draw nothing and
// say nothing. If only the serve side were tested, `derive` being missing from
// the authoring gate would make the whole feature uncreatable, which is the
// exact defect that shipped in wave 1 with the four widget kinds.
func TestADeriveLayerIsAuthorableButNeverServable(t *testing.T) {
	stack := []Layer{qrLayer()}
	if err := ValidateAuthoredSlideLayers(stack); err != nil {
		t.Fatalf("the authoring gate refused a derive layer: %v", err)
	}
	err := ValidateSlideLayers(stack)
	if err == nil {
		t.Fatal("the SERVE gate accepted a derive layer — a projection failed to rewrite it and a player would be handed a kind it cannot draw")
	}
	if !strings.Contains(err.Error(), "unknown kind") {
		t.Errorf("the serve-time refusal does not explain itself: %v", err)
	}
	// The authoring gate's own rejection message must list `derive`, or an
	// operator who mistypes a kind is told the wrong set of legal values.
	bad := []Layer{{Kind: "gradient", X: 0, Y: 0, W: 10, H: 10}}
	aerr := ValidateAuthoredSlideLayers(bad)
	if aerr == nil || !strings.Contains(aerr.Error(), LayerKindDerive) {
		t.Errorf("the authoring gate's kind list omits %q: %v", LayerKindDerive, aerr)
	}
	if serr := ValidateSlideLayers(bad); serr == nil || strings.Contains(serr.Error(), LayerKindDerive) {
		t.Errorf("the SERVE gate's kind list advertises an authoring-only kind: %v", serr)
	}
}

// TestDeriveMembersAreRefusedOnEveryOtherKind closes the mirror of the same
// asymmetry from the members' side: a derive spec hung on a text layer would be
// ignored by every projection, so the operator styles a gradient that never
// appears and nothing anywhere reports a fault.
func TestDeriveMembersAreRefusedOnEveryOtherKind(t *testing.T) {
	spec := &DeriveSpec{Kind: DeriveKindRect, Fill: &DeriveFill{Kind: DeriveFillSolid, From: "#FFFFFF"}}
	for _, kind := range slideLayerKinds {
		l := Layer{Kind: kind, X: 0, Y: 0, W: 100, H: 100, Text: "x", Color: "#FFFFFF",
			AssetRef: "sha256:" + strings.Repeat("a", 64), URL: "https://o/x", TargetMS: 1, EntityID: "e"}
		l.Derive = spec
		if err := ValidateAuthoredSlideLayers([]Layer{l}); err == nil {
			t.Errorf("a %s layer was allowed to carry a derive spec", kind)
		}
		l.Derive = nil
		l.DerivedFrom = "deadbeef"
		if err := ValidateAuthoredSlideLayers([]Layer{l}); err == nil {
			t.Errorf("a %s layer was allowed to carry derived_from", kind)
		}
	}
}

// TestADeriveLayerNeedsItsSpecButNotItsAsset: a freshly authored derive layer
// has no PNG yet — that is its normal state, and the whole reason the work queue
// exists. Requiring an asset here would make it impossible to author the thing
// the renderer is supposed to find.
func TestADeriveLayerNeedsItsSpecButNotItsAsset(t *testing.T) {
	noSpec := Layer{Kind: LayerKindDerive, X: 0, Y: 0, W: 100, H: 100}
	if err := ValidateAuthoredSlideLayers([]Layer{noSpec}); err == nil {
		t.Error("a derive layer with no spec was accepted — there would be nothing to render")
	}
	if err := ValidateAuthoredSlideLayers([]Layer{qrLayer()}); err != nil {
		t.Errorf("a derive layer with no asset yet was refused: %v", err)
	}
	// derived_from without an asset_ref is incoherent: it claims a render that
	// produced nothing.
	orphan := qrLayer()
	orphan.DerivedFrom = "deadbeef"
	if err := ValidateAuthoredSlideLayers([]Layer{orphan}); err == nil {
		t.Error("derived_from with no asset_ref was accepted")
	}
}

// TestDeriveDigestCoversGeometry is the case a digest over the spec alone gets
// wrong, and it is the half-right shape this codebase keeps producing.
//
// The raster is rendered at exactly the layer's pixel size, so resizing a layer
// changes the picture as surely as changing its text does. Without w/h in the
// digest a resized layer reads CURRENT, never re-renders, and the panel shows a
// stretched PNG — no error, no queue entry, nothing to notice.
func TestDeriveDigestCoversGeometry(t *testing.T) {
	base := qrLayer()
	d := DeriveDigest(base)
	if d == "" || len(d) != 64 {
		t.Fatalf("DeriveDigest returned %q, want a 64-char hex digest", d)
	}

	moved := base
	moved.X, moved.Y = 900, 40
	if DeriveDigest(moved) != d {
		t.Error("moving a layer changed its digest — a reposition must not force a re-render")
	}

	resized := base
	resized.W = 520
	if DeriveDigest(resized) == d {
		t.Error("resizing a layer did NOT change its digest — the raster would be silently rescaled on the panel")
	}
	taller := base
	taller.H = 520
	if DeriveDigest(taller) == d {
		t.Error("changing only the height did not change the digest")
	}

	edited := base
	edited.Derive = &DeriveSpec{Kind: DeriveKindQR, Data: "https://waiveo.local/other", ECLevel: "M"}
	if DeriveDigest(edited) == d {
		t.Error("editing the payload did not change the digest")
	}
	// Every styling member must reach the digest, or an edit to it renders
	// nothing new. This is the loop that catches a member added to DeriveSpec
	// and forgotten by the identity.
	for name, mutate := range map[string]func(*DeriveSpec){
		"ec_level":       func(s *DeriveSpec) { s.ECLevel = "H" },
		"color":          func(s *DeriveSpec) { s.Color = "#123456" },
		"font_px":        func(s *DeriveSpec) { s.FontPx = 41 },
		"font_family":    func(s *DeriveSpec) { s.FontFamily = "Oswald" },
		"font_asset_ref": func(s *DeriveSpec) { s.FontAssetRef = "sha256:" + strings.Repeat("b", 64) },
		"fill":           func(s *DeriveSpec) { s.Fill = &DeriveFill{Kind: DeriveFillSolid, From: "#010203"} },
		"shadow":         func(s *DeriveSpec) { s.Shadow = &DeriveShadow{Blur: 3} },
		"border":         func(s *DeriveSpec) { s.Border = &DeriveBorder{Radius: 7} },
	} {
		l := qrLayer()
		spec := *l.Derive
		mutate(&spec)
		l.Derive = &spec
		if DeriveDigest(l) == d {
			t.Errorf("changing %s did not change the digest — an edit to it would never be re-rendered", name)
		}
	}

	// And it is stable: the same layer must always hash the same, or every pass
	// re-renders everything.
	for i := 0; i < 10; i++ {
		if DeriveDigest(qrLayer()) != d {
			t.Fatal("DeriveDigest is not stable across calls")
		}
	}
	// A non-derive layer has no digest at all.
	if DeriveDigest(Layer{Kind: LayerKindText, Text: "x"}) != "" {
		t.Error("a non-derive layer reported a digest")
	}
}

// TestLayerDeriveStateClassifiesAllThree, including the case that matters most
// for a wall: a layer whose spec changed still has an old picture, and that
// picture must keep being served.
func TestLayerDeriveStateClassifiesAllThree(t *testing.T) {
	pending := qrLayer()
	if got := LayerDeriveState(pending); got != DerivePending {
		t.Errorf("an unrendered layer reads %s, want pending", got)
	}
	if _, ok := DeriveProjection(pending); ok {
		t.Error("an unrendered layer projected — a player would be handed a layer with no bytes")
	}

	current := pending
	current.AssetRef = "sha256:" + strings.Repeat("c", 64)
	current.DerivedFrom = DeriveDigest(current)
	if got := LayerDeriveState(current); got != DeriveCurrent {
		t.Errorf("a freshly rendered layer reads %s, want current", got)
	}

	stale := current
	stale.W = 600
	if got := LayerDeriveState(stale); got != DeriveStale {
		t.Errorf("an edited layer reads %s, want stale", got)
	}
	p, ok := DeriveProjection(stale)
	if !ok {
		t.Fatal("a STALE layer stopped projecting — an operator nudging a font size would watch the layer vanish from every screen until an off-box tool caught up")
	}
	if p.AssetRef != current.AssetRef {
		t.Errorf("the stale projection does not carry the previous asset: %q", p.AssetRef)
	}

	// Every non-derive kind is current by definition; answering otherwise would
	// put every plain text layer in the work queue forever.
	for _, k := range slideLayerKinds {
		if got := LayerDeriveState(Layer{Kind: k}); got != DeriveCurrent {
			t.Errorf("a %s layer reads %s, want current", k, got)
		}
	}
}

// TestDeriveProjectionYieldsAnOrdinaryImageLayer is the claim the whole design
// rests on: the player learns NOTHING. What it receives is the same image layer
// it has drawn since parity milestone 2.
func TestDeriveProjectionYieldsAnOrdinaryImageLayer(t *testing.T) {
	l := qrLayer()
	l.AssetRef = "sha256:" + strings.Repeat("d", 64)
	l.URL = "https://origin.invalid/content/" + strings.Repeat("d", 64)
	l.DerivedFrom = DeriveDigest(l)

	p, ok := DeriveProjection(l)
	if !ok {
		t.Fatal("a current derive layer did not project")
	}
	if p.Kind != LayerKindImage {
		t.Fatalf("projected kind %q, want %q", p.Kind, LayerKindImage)
	}
	if p.X != l.X || p.Y != l.Y || p.W != l.W || p.H != l.H {
		t.Errorf("the projection moved or resized the layer: %+v", p)
	}
	if p.Derive != nil || p.DerivedFrom != "" {
		t.Errorf("the projection leaked authoring-only members onto the wire: %+v", p)
	}
	if err := ValidateSlideLayers([]Layer{p}); err != nil {
		t.Errorf("the serve-time gate rejected the projection: %v", err)
	}
	// A layer of any other kind must pass through untouched, so a caller can run
	// a whole stack through the projection with no kind test of its own.
	text := Layer{Kind: LayerKindText, X: 1, Y: 2, W: 3, H: 4, Text: "hi"}
	through, ok := DeriveProjection(text)
	// reflect.DeepEqual rather than `!=`: Layer carries a `nav` layer's Items
	// slice, so it is not a comparable type.
	if !ok || !reflect.DeepEqual(through, text) {
		t.Errorf("a non-derive layer did not pass through unchanged: %+v (ok=%v)", through, ok)
	}
}

// TestValidateDeriveSpecIsAClosedVocabulary. Each of these is a setting that
// would otherwise do nothing silently — the single most common shape of defect
// in this codebase — or a value the renderer could not draw.
func TestValidateDeriveSpecIsAClosedVocabulary(t *testing.T) {
	ok := func(s *DeriveSpec) *DeriveSpec { return s }
	cases := []struct {
		name    string
		spec    *DeriveSpec
		wantErr bool
	}{
		{"nil", nil, true},
		{"unknown kind", &DeriveSpec{Kind: "hologram"}, true},
		{"qr minimal", ok(&DeriveSpec{Kind: DeriveKindQR, Data: "x"}), false},
		{"qr with no data", &DeriveSpec{Kind: DeriveKindQR}, true},
		{"qr with text", &DeriveSpec{Kind: DeriveKindQR, Data: "x", Text: "y"}, true},
		{"qr oversize payload", &DeriveSpec{Kind: DeriveKindQR, Data: strings.Repeat("x", DeriveMaxQRPayload+1)}, true},
		{"qr bad ec", &DeriveSpec{Kind: DeriveKindQR, Data: "x", ECLevel: "Z"}, true},
		{"qr aligned", &DeriveSpec{Kind: DeriveKindQR, Data: "x", Align: "center"}, true},
		{"text minimal", ok(&DeriveSpec{Kind: DeriveKindText, Text: "hi"}), false},
		{"text with data", &DeriveSpec{Kind: DeriveKindText, Text: "hi", Data: "x"}, true},
		{"text oversize", &DeriveSpec{Kind: DeriveKindText, Text: strings.Repeat("x", DeriveMaxTextLen+1)}, true},
		{"text negative size", &DeriveSpec{Kind: DeriveKindText, Text: "hi", FontPx: -1}, true},
		{"ec on a text spec", &DeriveSpec{Kind: DeriveKindText, Text: "hi", ECLevel: "M"}, true},
		{"bad font ref", &DeriveSpec{Kind: DeriveKindText, Text: "hi", FontAssetRef: "not-a-ref"}, true},
		// TYPOGRAPHY on a spec that draws no text. The page builder writes
		// font-size and font-family into the `#txt` rule only and embeds the
		// @font-face for a text run only, so each of these is a control an
		// operator would set, save, re-render for, and never see — and the face
		// is worse than inert, because accepting it also pins a font file against
		// the content retention sweep for a layer that will never draw with it.
		{"font size on a qr spec", &DeriveSpec{Kind: DeriveKindQR, Data: "x", FontPx: 40}, true},
		{"font family on a qr spec", &DeriveSpec{Kind: DeriveKindQR, Data: "x", FontFamily: "Oswald"}, true},
		{"custom face on a qr spec", &DeriveSpec{Kind: DeriveKindQR, Data: "x",
			FontAssetRef: "sha256:" + strings.Repeat("a", 64)}, true},
		{"custom face on a rect spec", &DeriveSpec{Kind: DeriveKindRect,
			Fill:         &DeriveFill{Kind: DeriveFillSolid, From: "#FFFFFF"},
			FontAssetRef: "sha256:" + strings.Repeat("a", 64)}, true},
		{"custom face on a text spec", ok(&DeriveSpec{Kind: DeriveKindText, Text: "hi", FontFamily: "Oswald",
			FontAssetRef: "sha256:" + strings.Repeat("a", 64)}), false},
		// A rect's picture is its fill, border and shadow — there is no
		// foreground for a colour to paint.
		{"colour on a rect spec", &DeriveSpec{Kind: DeriveKindRect,
			Fill: &DeriveFill{Kind: DeriveFillSolid, From: "#FFFFFF"}, Color: "#112233"}, true},
		{"rect with no fill", &DeriveSpec{Kind: DeriveKindRect}, true},
		{"rect minimal", ok(&DeriveSpec{Kind: DeriveKindRect, Fill: &DeriveFill{Kind: DeriveFillSolid, From: "#FFFFFF"}}), false},
		{"solid with a second stop", &DeriveSpec{Kind: DeriveKindRect,
			Fill: &DeriveFill{Kind: DeriveFillSolid, From: "#FFFFFF", To: "#000000"}}, true},
		{"solid with an angle", &DeriveSpec{Kind: DeriveKindRect,
			Fill: &DeriveFill{Kind: DeriveFillSolid, From: "#FFFFFF", AngleDeg: 90}}, true},
		{"gradient with no second stop", &DeriveSpec{Kind: DeriveKindRect,
			Fill: &DeriveFill{Kind: DeriveFillLinear, From: "#FFFFFF"}}, true},
		{"radial with an angle", &DeriveSpec{Kind: DeriveKindRect,
			Fill: &DeriveFill{Kind: DeriveFillRadial, From: "#FFFFFF", To: "#000000", AngleDeg: 45}}, true},
		{"angle out of range", &DeriveSpec{Kind: DeriveKindRect,
			Fill: &DeriveFill{Kind: DeriveFillLinear, From: "#FFFFFF", To: "#000000", AngleDeg: 360}}, true},
		{"invisible shadow", &DeriveSpec{Kind: DeriveKindText, Text: "hi", Shadow: &DeriveShadow{}}, true},
		{"shadow opacity out of range", &DeriveSpec{Kind: DeriveKindText, Text: "hi",
			Shadow: &DeriveShadow{Blur: 4, OpacityPct: 101}}, true},
		{"border that does nothing", &DeriveSpec{Kind: DeriveKindText, Text: "hi", Border: &DeriveBorder{}}, true},
		{"stroked border with no colour", &DeriveSpec{Kind: DeriveKindText, Text: "hi", Border: &DeriveBorder{Width: 2}}, true},
		{"radius-only border", ok(&DeriveSpec{Kind: DeriveKindText, Text: "hi", Border: &DeriveBorder{Radius: 8}}), false},
		{"bad colour", &DeriveSpec{Kind: DeriveKindText, Text: "hi", Color: "white"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateDeriveSpec(tc.spec)
			if tc.wantErr && err == nil {
				t.Fatal("the gate accepted a spec it must refuse")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("the gate refused a legal spec: %v", err)
			}
		})
	}
}
