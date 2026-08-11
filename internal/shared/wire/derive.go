package wire

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

// derive.go is the wire half of the RASTERIZED FALLBACK (parity row 2.4): the
// shape an operator authors when they want something on a slide that native
// SceneGraph layers cannot draw, and the two predicates that decide what a
// projection does with it.
//
// # Why a slide needs a fallback at all
//
// The native slide renderer draws Labels, Rectangles, Posters and Videos,
// because those are the nodes a Roku actually has. Five things the legacy Studio
// could put on a slide have no such node: a linear or radial GRADIENT, a DROP
// SHADOW, a ROUNDED or styled BORDER, CUSTOM TYPOGRAPHY (a font the device does
// not ship), and a QR CODE. Legacy solved all five the same way — it rasterized
// the element to a PNG with a headless Chromium and shipped the picture.
//
// This is that mechanism, rebuilt with the ONE constraint that governs its whole
// design: the rasterizer does NOT run on the appliance. waiveo-next's feeder and
// relay are deliberately Go-only, and the Pi already carries the legacy stack; a
// second Chromium on that box is out of the question. So the rasterizer is a
// SEPARATE, off-appliance unit (cmd/waiveo-derive), and everything in this file
// is the contract between an authored layer and that out-of-band renderer.
// Nothing here imports, spawns, or knows about a browser — a derive layer is
// data, and the appliance only ever reads it.
//
// # The shape of the mechanism
//
// A `derive` layer carries a SPEC (what to draw) and, once some run of the
// rasterizer has produced it, an ASSET (the PNG's content-addressed reference)
// plus DerivedFrom, the digest of the spec that asset was produced FROM. At
// projection time a derive layer is rewritten into a plain `image` layer
// (DeriveProjection) — so the player learns no new kind, needs no new
// capability, and the PNG rides the existing content-addressed image path that
// is already fetched, verified and cached on the device.
//
// The DerivedFrom digest is what makes the loop honest in both directions:
//   - no asset yet  -> the layer is PENDING; the projection omits it and the
//     rest of the slide still draws.
//   - asset present, digest matches -> the raster is CURRENT.
//   - asset present, digest differs -> the operator edited the spec and the
//     raster is STALE. The projection still serves the OLD asset (a screen is
//     never blanked by an edit that has not been rendered yet) and the layer is
//     reported pending so the next derive run refreshes it.

// LayerKindDerive is the AUTHORING-ONLY layer kind that names something the
// native renderer cannot draw.
//
// It is authoring-only in the strict sense: ValidateAuthoredSlideLayers accepts
// it, ValidateSlideLayers (the SERVE-time gate) does not, and the two content
// projections rewrite it into `image` before that gate ever sees it. That
// asymmetry is deliberate and it is the reason a player needs no new code — the
// wire a screen reads carries only kinds it already draws — but it is also a
// trap if it is left implicit, so it is enforced in one place
// (authoredOnlyLayerKinds) and asserted from both sides in the wire tests.
const LayerKindDerive = "derive"

// The closed set of things a derive spec can ask for. Each is a shape the
// off-appliance renderer knows how to draw and the player cannot.
const (
	// DeriveKindQR is a QR symbol encoded from Data. There is no Roku node that
	// turns a string into a scannable symbol, so this is the only way a slide can
	// carry one at all.
	DeriveKindQR = "qr"
	// DeriveKindText is a text run with un-native styling: a gradient or shadowed
	// backing, a rounded border, a font the device does not ship. A text layer
	// that needs none of those should stay a native `text` layer — it stays live,
	// costs no content, and re-styles without a render.
	DeriveKindText = "text"
	// DeriveKindRect is a filled box with un-native styling: gradient fill, drop
	// shadow, rounded corners. Same rule — a flat opaque box is a native `rect`.
	DeriveKindRect = "rect"
)

// deriveKinds is the closed set as a slice, in declaration order: the ONE
// enumeration membership is tested against and that a rejection message names,
// so a kind added above and forgotten here cannot pass validation while the
// error claims to list every legal value.
var deriveKinds = []string{DeriveKindQR, DeriveKindText, DeriveKindRect}

// Fill kinds. `solid` exists alongside the two gradients so a spec can state a
// flat backing under a shadow or a rounded border without pretending to be a
// gradient with two equal stops.
const (
	DeriveFillSolid  = "solid"
	DeriveFillLinear = "linear"
	DeriveFillRadial = "radial"
)

var deriveFillKinds = []string{DeriveFillSolid, DeriveFillLinear, DeriveFillRadial}

// DeriveSpec is the complete description of what the off-appliance rasterizer
// must draw for one layer. It is a CLOSED, declarative vocabulary rather than a
// blob of CSS or HTML on purpose: the renderer runs a browser, and a spec that
// could carry arbitrary markup would make an authored cast a script-injection
// vector into whatever machine runs the derive tool. Every member here is a
// scalar the renderer validates and then formats into markup it wrote itself.
//
// Every member is `omitempty` so a spec marshals only what its own kind uses,
// which keeps the spec digest (DeriveDigest) stable when unrelated members are
// added later.
type DeriveSpec struct {
	// Kind is one of deriveKinds. Required.
	Kind string `json:"kind"`

	// Data is the payload a `qr` spec encodes — a URL, a pairing code, whatever
	// a phone should receive. Required for `qr`, unused elsewhere.
	Data string `json:"data,omitempty"`
	// ECLevel is a `qr` spec's error-correction level: "L", "M", "Q" or "H".
	// Optional; an empty value means "M", the level a sign read at an angle in
	// bad light wants. A sign is not a shipping label — the extra capacity a
	// lower level buys is worthless when the payload is a 40-character URL.
	ECLevel string `json:"ec_level,omitempty"`

	// Text is the literal string a `text` spec draws. It is a LITERAL, never a
	// format: a rasterized layer is a frozen picture, so a clock or a countdown
	// must stay a native live layer and there is deliberately no way to ask for
	// one here. Required for `text`, unused elsewhere.
	Text string `json:"text,omitempty"`
	// FontPx is the text size in canvas pixels. Optional; an empty value means
	// DeriveDefaultFontPx.
	FontPx int `json:"font_px,omitempty"`
	// FontFamily is the CSS family name the renderer draws the text in. When
	// FontAssetRef is set this is the name the embedded face is registered
	// under; when it is not, it is a family the rendering host must already
	// have. Optional.
	FontFamily string `json:"font_family,omitempty"`
	// FontAssetRef is CUSTOM TYPOGRAPHY: a content-addressed `sha256:` reference
	// to a font file (TTF/OTF/WOFF2) in the content origin, which the renderer
	// fetches and embeds into the page before drawing. This is the member that
	// makes "a font the device does not ship" possible at all, and it is why
	// layerAssetReferences projects it — an authored font must be checked to
	// exist at write time and must not be reclaimed by the content sweep while a
	// cast still names it.
	FontAssetRef string `json:"font_asset_ref,omitempty"`
	// Align is horizontal alignment within the layer box: "left", "center" or
	// "right". Optional; empty means "left".
	Align string `json:"align,omitempty"`
	// VAlign is vertical alignment within the layer box: "top", "middle" or
	// "bottom". Optional; empty means "top". It exists because a rasterized
	// layer's box is exactly the layer geometry, so unlike a native Label there
	// is no larger canvas to centre against later.
	VAlign string `json:"valign,omitempty"`
	// Color is the foreground: the text colour for `text`, the DARK MODULE colour
	// for `qr`. `#RRGGBB`. Optional; empty means DeriveDefaultForeground.
	Color string `json:"color,omitempty"`

	// Fill is the backing behind the foreground — the gradient the native
	// renderer cannot draw. Optional; an absent fill renders a TRANSPARENT
	// background, so a derive layer composites over whatever native layers sit
	// beneath it.
	Fill *DeriveFill `json:"fill,omitempty"`
	// Shadow is a drop shadow cast by the layer box. Optional.
	Shadow *DeriveShadow `json:"shadow,omitempty"`
	// Border is a stroked and/or rounded outline. Optional.
	Border *DeriveBorder `json:"border,omitempty"`
}

// DeriveFill is a solid or gradient backing.
type DeriveFill struct {
	// Kind is one of deriveFillKinds. Required when a fill is present.
	Kind string `json:"kind"`
	// From is the solid colour, or the gradient's first stop. `#RRGGBB`.
	// Required.
	From string `json:"from"`
	// To is the gradient's second stop. `#RRGGBB`. Required for `linear` and
	// `radial`, and REJECTED for `solid` — a member the renderer would ignore is
	// a setting that silently does nothing, which is the defect shape this
	// codebase keeps producing.
	To string `json:"to,omitempty"`
	// AngleDeg is a `linear` gradient's direction in degrees, CSS convention: 0
	// points up, 90 points right, increasing clockwise. 0-359. Unused by the
	// other two kinds.
	AngleDeg int `json:"angle_deg,omitempty"`
}

// DeriveShadow is a drop shadow. Offsets and blur are in canvas pixels;
// opacity is an integer PERCENT rather than a float, deliberately: the spec is
// hashed to produce the content address, and a float would make the digest
// depend on decimal formatting.
type DeriveShadow struct {
	DX         int    `json:"dx,omitempty"`
	DY         int    `json:"dy,omitempty"`
	Blur       int    `json:"blur,omitempty"`
	Color      string `json:"color,omitempty"`
	OpacityPct int    `json:"opacity_pct,omitempty"`
}

// DeriveBorder is a stroked and/or rounded outline. A Radius with a zero Width
// is legal and useful: it rounds the FILL without stroking an edge.
type DeriveBorder struct {
	Width  int    `json:"width,omitempty"`
	Color  string `json:"color,omitempty"`
	Radius int    `json:"radius,omitempty"`
}

// Defaults the renderer applies to an omitted member. They live here, beside the
// shape, rather than in the renderer, because DeriveDigest hashes the SPEC —
// so if the renderer chose defaults privately, changing one would silently
// change every existing raster's pixels without changing any digest, and no
// screen would ever refresh.
const (
	DeriveDefaultFontPx     = 64
	DeriveDefaultForeground = "#FFFFFF"
	DeriveDefaultECLevel    = "M"
	DeriveDefaultShadow     = "#000000"
	// DeriveMaxQRPayload bounds a QR payload well below the 2,953-byte
	// theoretical ceiling. A symbol denser than this is unreadable from across a
	// room no matter how many pixels it is rendered at, so accepting one would
	// mean authoring a sign that cannot work.
	DeriveMaxQRPayload = 512
	// DeriveMaxTextLen bounds a rasterized text run. Longer than this is a
	// document, not a sign.
	DeriveMaxTextLen = 2048
)

// ValidateDeriveSpec is the ONE gate a derive spec passes. It is exported and
// lives beside the shape for the same reason ValidateSlideLayers does: the
// authoring surface, the projections and the off-appliance renderer must all
// agree on what a legal spec is, and a second copy of these rules would drift in
// the direction that hurts — a spec the store accepts and the renderer refuses
// is a layer that is pending forever with nothing saying why.
func ValidateDeriveSpec(s *DeriveSpec) error {
	if s == nil {
		return fmt.Errorf("wire: derive spec is required")
	}
	if !contains(deriveKinds, s.Kind) {
		return fmt.Errorf("wire: derive kind %q is unknown (want one of %s)", s.Kind, strings.Join(deriveKinds, "/"))
	}

	switch s.Kind {
	case DeriveKindQR:
		if s.Data == "" {
			return fmt.Errorf("wire: derive qr: data is required")
		}
		if len(s.Data) > DeriveMaxQRPayload {
			return fmt.Errorf("wire: derive qr: data is %d bytes, over the %d-byte limit", len(s.Data), DeriveMaxQRPayload)
		}
		if s.Text != "" {
			return fmt.Errorf("wire: derive qr: text is not used by a qr spec")
		}
	case DeriveKindText:
		if s.Text == "" {
			return fmt.Errorf("wire: derive text: text is required")
		}
		if len(s.Text) > DeriveMaxTextLen {
			return fmt.Errorf("wire: derive text: text is %d bytes, over the %d-byte limit", len(s.Text), DeriveMaxTextLen)
		}
		if s.Data != "" {
			return fmt.Errorf("wire: derive text: data is only used by a qr spec")
		}
	case DeriveKindRect:
		if s.Text != "" {
			return fmt.Errorf("wire: derive rect: text is not used by a rect spec")
		}
		if s.Data != "" {
			return fmt.Errorf("wire: derive rect: data is only used by a qr spec")
		}
		if s.Fill == nil {
			// A rect with no fill, no border and no shadow is an invisible layer,
			// and a rect with only a shadow is a shadow of nothing. Requiring a
			// fill is what makes "an accepted derive rect is a rect that draws
			// something" true.
			return fmt.Errorf("wire: derive rect: a fill is required (a rect with no fill draws nothing)")
		}
	}

	if s.ECLevel != "" {
		if s.Kind != DeriveKindQR {
			return fmt.Errorf("wire: derive %s: ec_level is only used by a qr spec", s.Kind)
		}
		if !contains([]string{"L", "M", "Q", "H"}, s.ECLevel) {
			return fmt.Errorf("wire: derive qr: ec_level %q is unknown (want L, M, Q or H)", s.ECLevel)
		}
	}
	if s.FontPx < 0 {
		return fmt.Errorf("wire: derive %s: font_px must not be negative, got %d", s.Kind, s.FontPx)
	}
	// Alignment places TEXT inside the layer box. A QR symbol is always centred
	// (it is a square with a mandatory quiet zone; nothing else is a sensible
	// placement) and a rect fills its box entirely, so on either of those an
	// alignment member is a control that would silently do nothing — refused
	// here for the same reason a solid fill refuses a second colour stop.
	if s.Align != "" {
		if s.Kind != DeriveKindText {
			return fmt.Errorf("wire: derive %s: align is only used by a text spec", s.Kind)
		}
		if !contains([]string{"left", "center", "right"}, s.Align) {
			return fmt.Errorf("wire: derive %s: align %q is unknown (want left, center or right)", s.Kind, s.Align)
		}
	}
	if s.VAlign != "" {
		if s.Kind != DeriveKindText {
			return fmt.Errorf("wire: derive %s: valign is only used by a text spec", s.Kind)
		}
		if !contains([]string{"top", "middle", "bottom"}, s.VAlign) {
			return fmt.Errorf("wire: derive %s: valign %q is unknown (want top, middle or bottom)", s.Kind, s.VAlign)
		}
	}
	if s.Color != "" && !isHexColor(s.Color) {
		return fmt.Errorf("wire: derive %s: color %q is not a #RRGGBB hex string", s.Kind, s.Color)
	}
	if s.FontAssetRef != "" && !isAssetRef(s.FontAssetRef) {
		return fmt.Errorf("wire: derive %s: font_asset_ref %q is not a sha256:<64 hex> reference", s.Kind, s.FontAssetRef)
	}

	if f := s.Fill; f != nil {
		if !contains(deriveFillKinds, f.Kind) {
			return fmt.Errorf("wire: derive fill: kind %q is unknown (want one of %s)", f.Kind, strings.Join(deriveFillKinds, "/"))
		}
		if !isHexColor(f.From) {
			return fmt.Errorf("wire: derive fill: from %q is not a #RRGGBB hex string", f.From)
		}
		if f.Kind == DeriveFillSolid {
			if f.To != "" {
				return fmt.Errorf("wire: derive fill: a solid fill has no second stop, got to=%q", f.To)
			}
			if f.AngleDeg != 0 {
				return fmt.Errorf("wire: derive fill: a solid fill has no angle, got angle_deg=%d", f.AngleDeg)
			}
		} else {
			if !isHexColor(f.To) {
				return fmt.Errorf("wire: derive fill: a %s gradient needs a #RRGGBB second stop, got to=%q", f.Kind, f.To)
			}
		}
		if f.Kind == DeriveFillRadial && f.AngleDeg != 0 {
			return fmt.Errorf("wire: derive fill: a radial gradient has no angle, got angle_deg=%d", f.AngleDeg)
		}
		if f.AngleDeg < 0 || f.AngleDeg > 359 {
			return fmt.Errorf("wire: derive fill: angle_deg %d is outside 0-359", f.AngleDeg)
		}
	}

	if sh := s.Shadow; sh != nil {
		if sh.Blur < 0 {
			return fmt.Errorf("wire: derive shadow: blur must not be negative, got %d", sh.Blur)
		}
		if sh.Color != "" && !isHexColor(sh.Color) {
			return fmt.Errorf("wire: derive shadow: color %q is not a #RRGGBB hex string", sh.Color)
		}
		if sh.OpacityPct < 0 || sh.OpacityPct > 100 {
			return fmt.Errorf("wire: derive shadow: opacity_pct %d is outside 0-100", sh.OpacityPct)
		}
		if sh.DX == 0 && sh.DY == 0 && sh.Blur == 0 {
			return fmt.Errorf("wire: derive shadow: a shadow with no offset and no blur is invisible")
		}
	}

	if b := s.Border; b != nil {
		if b.Width < 0 {
			return fmt.Errorf("wire: derive border: width must not be negative, got %d", b.Width)
		}
		if b.Radius < 0 {
			return fmt.Errorf("wire: derive border: radius must not be negative, got %d", b.Radius)
		}
		if b.Width == 0 && b.Radius == 0 {
			return fmt.Errorf("wire: derive border: a border with no width and no radius does nothing")
		}
		if b.Color != "" && !isHexColor(b.Color) {
			return fmt.Errorf("wire: derive border: color %q is not a #RRGGBB hex string", b.Color)
		}
		if b.Width > 0 && b.Color == "" {
			return fmt.Errorf("wire: derive border: a stroked border needs a color")
		}
	}

	return nil
}

// DeriveDigest is THE identity of a derive layer's pixels: the hex sha256 of the
// canonical JSON of everything that determines what the rasterizer draws — the
// layer's WIDTH and HEIGHT, and the spec.
//
// Geometry is in the digest and that is not incidental. The raster is produced
// at exactly the layer's pixel size, so resizing a layer in the Studio changes
// the picture as surely as changing its text does. A digest over the spec alone
// would leave a resized layer reporting itself CURRENT and a screen showing a
// stretched or cropped PNG, which is precisely the half-right shape this
// codebase keeps shipping — the enforcement written and the authoring side
// forgotten.
//
// Everything else about a layer — where it sits on the canvas, its z-order — is
// deliberately NOT in the digest: moving a layer must not force a re-render.
func DeriveDigest(l Layer) string {
	if l.Derive == nil {
		return ""
	}
	payload := struct {
		W    int         `json:"w"`
		H    int         `json:"h"`
		Spec *DeriveSpec `json:"spec"`
	}{W: l.W, H: l.H, Spec: l.Derive}
	b, err := json.Marshal(payload)
	if err != nil {
		// A DeriveSpec is a closed struct of scalars and pointers to closed
		// structs of scalars; encoding/json cannot fail on it. Returning the
		// empty digest here would read as "no spec", so panic rather than
		// silently mark every layer pending.
		panic(fmt.Sprintf("wire: DeriveDigest: marshal spec: %v", err))
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// DeriveState is what a projection (or an operator) needs to know about one
// derive layer: whether it can be drawn now, and whether it should be
// re-rendered.
type DeriveState int

// The three states a derive layer can be in. See derive.go's header for what
// each means for a screen.
const (
	// DerivePending: no asset has ever been produced. The projection omits the
	// layer; the rest of the slide draws.
	DerivePending DeriveState = iota
	// DeriveStale: an asset exists but was produced from a DIFFERENT spec or
	// geometry. The projection serves the old asset — a screen is never blanked
	// by an edit nobody has rendered yet — and the layer is reported for the next
	// derive run.
	DeriveStale
	// DeriveCurrent: the asset matches the spec.
	DeriveCurrent
)

// String names the state for a log line or an API field.
func (s DeriveState) String() string {
	switch s {
	case DerivePending:
		return "pending"
	case DeriveStale:
		return "stale"
	case DeriveCurrent:
		return "current"
	}
	return "unknown"
}

// LayerDeriveState classifies a derive layer. A layer of any other kind is
// DeriveCurrent — it needs nothing derived, and answering "pending" for a plain
// text layer would make every cast look permanently unrendered.
func LayerDeriveState(l Layer) DeriveState {
	if l.Kind != LayerKindDerive {
		return DeriveCurrent
	}
	if l.AssetRef == "" {
		return DerivePending
	}
	if l.DerivedFrom != DeriveDigest(l) {
		return DeriveStale
	}
	return DeriveCurrent
}

// DeriveProjection rewrites a derive layer into the plain `image` layer a
// player draws, or reports that it cannot be drawn yet.
//
// This is THE function both content projections call — internal/feeder/snapshot
// and internal/relay/schedulehost — and it exists as one shared function for
// exactly the reason LayerFetchesContent does: two projections that agreed today
// and diverged later would show an operator a cast slide that renders through
// one path and not the other, from the same authored layers.
//
// A non-derive layer passes through untouched, so a caller can run every layer
// through it without a kind test.
func DeriveProjection(l Layer) (Layer, bool) {
	if l.Kind != LayerKindDerive {
		return l, true
	}
	if l.AssetRef == "" {
		return Layer{}, false
	}
	// A STALE layer is projected from its existing asset on purpose. The
	// alternative — dropping it until it is re-rendered — means an operator who
	// nudges a font size watches the layer disappear from every screen until an
	// off-appliance tool they may not be running catches up. Serving the old
	// picture is the same never-wipe discipline the player applies to a program
	// it cannot refresh.
	return Layer{
		Kind:     LayerKindImage,
		X:        l.X,
		Y:        l.Y,
		W:        l.W,
		H:        l.H,
		AssetRef: l.AssetRef,
		URL:      l.URL,
	}, true
}

// isAssetRef reports whether s is a well-formed `sha256:<64 lowercase hex>`
// content reference.
func isAssetRef(s string) bool {
	const prefix = "sha256:"
	if !strings.HasPrefix(s, prefix) {
		return false
	}
	hexPart := s[len(prefix):]
	if len(hexPart) != 64 {
		return false
	}
	for i := 0; i < len(hexPart); i++ {
		c := hexPart[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// contains is a tiny closed-set membership helper. It exists so every "want one
// of" check in this file reads the same and names the same slice it tests
// against.
func contains(set []string, v string) bool {
	for _, s := range set {
		if s == v {
			return true
		}
	}
	return false
}
