// Package derive is the RASTERIZED FALLBACK (parity row 2.4): the off-appliance
// renderer that turns a `derive` slide layer into a content-addressed PNG.
//
// # Why it is a separate package, reachable only from cmd/waiveo-derive
//
// Legacy solved "the Studio can draw things SceneGraph cannot" by rasterizing
// slides with a headless Chromium living inside the signage service
// (slidecast/render-worker.js). That browser is NOT coming to this appliance.
// waiveo-next's feeder and relay are deliberately Go-only, and the box this
// replaces already hosts the whole legacy stack; a second Chromium on a Pi is
// out of the question. So the renderer is a SEPARATE UNIT an operator or a dev
// box runs out of band, and the appliance's entire share of the loop is a
// read-only work queue (GET /derive/pending).
//
// Nothing in internal/feeder, internal/relay or internal/app imports this
// package, and TestTheFeederNeverReachesTheRasterizer proves it from the actual
// import graph rather than from intent.
//
// # What was ported, and why those things specifically
//
// The mechanism is a rewrite, but four behaviours are transplanted verbatim in
// substance because they were paid for in production on real hardware, and each
// of them is a failure that looks like a hang rather than an error:
//
//  1. A PER-JOB WALL-CLOCK TIMEOUT with a PROCESS-TREE KILL. A wedged Chromium
//     does not exit when its parent gives up on it, and `Process.Kill` reaches
//     only the launcher — the renderer, GPU and zygote children survive, hold
//     the profile directory, and accumulate until the machine is out of memory.
//     See browser.go's killTree.
//  2. The TWO-rAF IDLE-FRAME FORCE. A headless page that has finished loading
//     and gone idle may never commit another compositor frame, and
//     Page.captureScreenshot then blocks for its FULL timeout waiting for one
//     that is not coming — legacy's #1154, where an immediate retry succeeded in
//     ~35 ms. Forcing a layout read and two nested requestAnimationFrames before
//     every capture makes the compositor produce the frame.
//  3. A PER-ELEMENT CIRCUIT BREAKER with exponential backoff. Without it one
//     layer that cannot render is retried on every pass forever, and it starves
//     the layers that CAN render behind it.
//  4. A BOUNDED CONCURRENCY CLAMP. Legacy ran a pool of two on Pi-class
//     hardware; unbounded parallel Chromium pages is how a render host is taken
//     down by its own queue.
//
// Guards 1, 3 and 4 live in runner.go, deliberately independent of the browser,
// so they are exercised by tests that need no Chromium at all. Guard 2 lives in
// browser.go, where the page is.
package derive

import (
	"fmt"
	"html"
	"strconv"
	"strings"

	"github.com/maaxton/waiveo-next/internal/derive/qr"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// Job is one rasterization: a spec, the pixel box it must fill, and an optional
// embedded font. Key identifies the job for the circuit breaker and for logs —
// the caller passes the spec digest, so a layer that keeps failing is recognised
// as the same layer across passes.
type Job struct {
	Key      string
	W        int
	H        int
	Spec     *wire.DeriveSpec
	FontData []byte // the raw bytes of Spec.FontAssetRef, already fetched
}

// Page is the complete, self-contained HTML document one job renders, plus the
// content box the capture is clipped to.
//
// The document embeds EVERYTHING — no network fetches, no external stylesheet,
// no remote font. That is not only a determinism property: it means the render
// host makes no outbound request while displaying operator-authored content, so
// a spec cannot be used to make the render machine call somewhere.
type Page struct {
	HTML string
	// ClipW/ClipH are the capture region, which is always exactly the layer's
	// authored W×H — the PNG drops into an `image` layer of the same geometry
	// with no scaling anywhere.
	ClipW int
	ClipH int
}

// BuildPage renders a job into its page. It is a PURE function of the job: same
// job in, byte-identical HTML out, which is half of what makes the resulting PNG
// content-addressable (the other half is the browser's own determinism flags,
// see browser.go).
func BuildPage(j Job) (Page, error) {
	if j.Spec == nil {
		return Page{}, fmt.Errorf("derive: job %s: no spec", j.Key)
	}
	if err := wire.ValidateDeriveSpec(j.Spec); err != nil {
		return Page{}, fmt.Errorf("derive: job %s: %w", j.Key, err)
	}
	if j.W <= 0 || j.H <= 0 {
		return Page{}, fmt.Errorf("derive: job %s: geometry must be positive, got %dx%d", j.Key, j.W, j.H)
	}

	insetL, insetT, insetR, insetB := ShadowInset(j.Spec.Shadow)
	boxW := j.W - insetL - insetR
	boxH := j.H - insetT - insetB
	if boxW <= 0 || boxH <= 0 {
		return Page{}, fmt.Errorf(
			"derive: job %s: a %dx%d layer has no room left for its own shadow (needs %d horizontally and %d vertically) — enlarge the layer or reduce the shadow",
			j.Key, j.W, j.H, insetL+insetR, insetT+insetB)
	}

	var b strings.Builder
	b.WriteString("<!doctype html>\n<meta charset=\"utf-8\">\n<style>\n")
	b.WriteString("html,body{margin:0;padding:0;background:transparent;overflow:hidden;}\n")
	// Text rendering is pinned rather than left to the host's defaults:
	// ligatures and kerning that vary with a font-feature default would make the
	// same spec render differently on two machines and mint two content
	// addresses for one picture.
	b.WriteString("*{font-kerning:normal;font-variant-ligatures:none;text-rendering:geometricPrecision;}\n")
	if len(j.FontData) > 0 {
		b.WriteString("@font-face{font-family:'")
		b.WriteString(cssIdent(j.Spec.FontFamily, "DeriveFont"))
		b.WriteString("';src:url(data:font/ttf;base64,")
		b.WriteString(base64Std(j.FontData))
		b.WriteString(");font-display:block;}\n")
	}

	b.WriteString("#box{position:absolute;box-sizing:border-box;")
	fmt.Fprintf(&b, "left:%dpx;top:%dpx;width:%dpx;height:%dpx;", insetL, insetT, boxW, boxH)
	b.WriteString(fillCSS(j.Spec.Fill))
	b.WriteString(borderCSS(j.Spec.Border))
	b.WriteString(shadowCSS(j.Spec.Shadow))
	switch j.Spec.Kind {
	case wire.DeriveKindText:
		b.WriteString("display:flex;")
		b.WriteString("justify-content:" + flexMain(j.Spec.Align) + ";")
		b.WriteString("align-items:" + flexCross(j.Spec.VAlign) + ";")
	case wire.DeriveKindQR:
		// A symbol is ALWAYS centred in its box, which is why ValidateDeriveSpec
		// refuses an alignment on a qr spec. Centring is also what the
		// module-grid readback in browser_test.go locates the symbol by, so the
		// placement is pinned by a test rather than by convention.
		b.WriteString("display:flex;justify-content:center;align-items:center;")
	}
	b.WriteString("}\n")

	if j.Spec.Kind == wire.DeriveKindText {
		b.WriteString("#txt{")
		fmt.Fprintf(&b, "font-size:%dpx;", fontPx(j.Spec.FontPx))
		b.WriteString("line-height:1.15;")
		b.WriteString("color:" + colorOr(j.Spec.Color, wire.DeriveDefaultForeground) + ";")
		b.WriteString("text-align:" + textAlign(j.Spec.Align) + ";")
		b.WriteString("white-space:pre-wrap;word-break:break-word;")
		if fam := cssIdent(j.Spec.FontFamily, ""); fam != "" {
			b.WriteString("font-family:'" + fam + "',sans-serif;")
		}
		b.WriteString("}\n")
	}
	b.WriteString("</style>\n")

	b.WriteString("<div id=\"box\">")
	switch j.Spec.Kind {
	case wire.DeriveKindText:
		b.WriteString("<span id=\"txt\">")
		b.WriteString(html.EscapeString(j.Spec.Text))
		b.WriteString("</span>")
	case wire.DeriveKindQR:
		svg, err := qrSVG(j.Spec, boxW, boxH, j.Spec.Border)
		if err != nil {
			return Page{}, fmt.Errorf("derive: job %s: %w", j.Key, err)
		}
		b.WriteString(svg)
	case wire.DeriveKindRect:
		// Nothing inside: the box IS the picture.
	}
	b.WriteString("</div>\n")

	return Page{HTML: b.String(), ClipW: j.W, ClipH: j.H}, nil
}

// ShadowInset is the ENVELOPE RULE, and it is the one piece of legacy's
// paint-outside-the-box arithmetic that this rebuild keeps as a contract rather
// than as a workaround.
//
// A CSS box-shadow paints OUTSIDE the element that casts it. Legacy captured the
// element's own rectangle and then discovered, in production, that every shadow
// and blur was being clipped at the element bounds — the "blur clip-expansion"
// item still open on that codebase's audit ledger. The fix there was to grow the
// capture; the fix here is to shrink the CONTENT, because this renderer's output
// must be exactly the authored layer's W×H (it becomes an `image` layer of that
// geometry, and anything else would be silently rescaled on the panel).
//
// So the authored layer box is the shadow's BOUNDING box, and the styled element
// is inset inside it by the shadow's own extent on each side. A shadow is never
// clipped, the PNG is never rescaled, and the rule is stated once, here, where
// both the page builder and its tests read it.
func ShadowInset(s *wire.DeriveShadow) (left, top, right, bottom int) {
	if s == nil {
		return 0, 0, 0, 0
	}
	return max0(s.Blur - s.DX), max0(s.Blur - s.DY), max0(s.Blur + s.DX), max0(s.Blur + s.DY)
}

func max0(v int) int {
	if v < 0 {
		return 0
	}
	return v
}

// fillCSS renders the backing. An absent fill is TRANSPARENT rather than
// defaulted to a colour: a derive layer composites over the native layers under
// it, and inventing an opaque backing would hide them.
func fillCSS(f *wire.DeriveFill) string {
	if f == nil {
		return "background:transparent;"
	}
	switch f.Kind {
	case wire.DeriveFillLinear:
		return fmt.Sprintf("background:linear-gradient(%ddeg,%s 0%%,%s 100%%);", f.AngleDeg, f.From, f.To)
	case wire.DeriveFillRadial:
		return fmt.Sprintf("background:radial-gradient(circle at 50%% 50%%,%s 0%%,%s 100%%);", f.From, f.To)
	default:
		return "background:" + f.From + ";"
	}
}

// borderCSS renders the stroke and the corner radius. A radius with no width is
// legal and rounds the fill — see wire.DeriveBorder.
func borderCSS(b *wire.DeriveBorder) string {
	if b == nil {
		return ""
	}
	var out strings.Builder
	if b.Width > 0 {
		fmt.Fprintf(&out, "border:%dpx solid %s;", b.Width, b.Color)
	}
	if b.Radius > 0 {
		fmt.Fprintf(&out, "border-radius:%dpx;", b.Radius)
	}
	return out.String()
}

// shadowCSS renders the drop shadow, with the opacity folded into an rgba()
// colour. Opacity is an integer percent on the wire (see wire.DeriveShadow) and
// is formatted here to three decimals from integer arithmetic, so the CSS text —
// and therefore the page, and therefore the PNG — is byte-stable.
func shadowCSS(s *wire.DeriveShadow) string {
	if s == nil {
		return ""
	}
	c := s.Color
	if c == "" {
		c = wire.DeriveDefaultShadow
	}
	op := s.OpacityPct
	if op == 0 {
		// A shadow that asked for no opacity would be invisible, which is never
		// what authoring one means; ValidateDeriveSpec already refuses a shadow
		// with no offset and no blur, so the remaining zero here is "unset".
		op = 50
	}
	r, g, bl := hexRGB(c)
	return fmt.Sprintf("box-shadow:%dpx %dpx %dpx rgba(%d,%d,%d,%s);",
		s.DX, s.DY, s.Blur, r, g, bl, pctString(op))
}

// pctString formats an integer percent as a fixed three-decimal fraction using
// integer arithmetic only. Formatting through a float would make the CSS text
// depend on floating-point printing, which is exactly the kind of invisible
// input a content address must not have.
func pctString(pct int) string {
	whole := pct / 100
	frac := pct % 100
	return strconv.Itoa(whole) + "." + pad2(frac) + "0"
}

func pad2(v int) string {
	if v < 10 {
		return "0" + strconv.Itoa(v)
	}
	return strconv.Itoa(v)
}

// hexRGB splits a validated `#RRGGBB` into its components. The spec gate has
// already refused anything else, so a malformed value here is a programming
// error and reads as black rather than propagating an error into page building.
func hexRGB(s string) (int, int, int) {
	if len(s) != 7 {
		return 0, 0, 0
	}
	v, err := strconv.ParseUint(s[1:], 16, 32)
	if err != nil {
		return 0, 0, 0
	}
	return int(v>>16) & 0xFF, int(v>>8) & 0xFF, int(v) & 0xFF
}

func colorOr(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

func fontPx(v int) int {
	if v <= 0 {
		return wire.DeriveDefaultFontPx
	}
	return v
}

func textAlign(a string) string {
	if a == "" {
		return "left"
	}
	return a
}

func flexMain(a string) string {
	switch a {
	case "center":
		return "center"
	case "right":
		return "flex-end"
	default:
		return "flex-start"
	}
}

func flexCross(v string) string {
	switch v {
	case "middle":
		return "center"
	case "bottom":
		return "flex-end"
	default:
		return "flex-start"
	}
}

// cssIdent sanitises a family name down to characters that cannot terminate the
// quoted CSS string or open a new declaration. The spec vocabulary is closed
// precisely so authored text never becomes markup, and the font family is the
// one member that is otherwise free-form.
func cssIdent(name, fallback string) string {
	var out strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == ' ', r == '-', r == '_':
			out.WriteRune(r)
		}
	}
	s := strings.TrimSpace(out.String())
	if s == "" {
		return fallback
	}
	return s
}

// qrSVG encodes the payload and emits the symbol as an inline SVG of unit
// rectangles scaled by an INTEGER module size.
//
// Integer modules matter: a symbol drawn at a fractional module size lands on
// half-pixels, the rasterizer antialiases every module edge, and a scanner
// reading it off a panel at an angle starts failing. Choosing the largest
// integer size that fits, then centring the result, keeps every module edge on a
// pixel boundary.
func qrSVG(s *wire.DeriveSpec, boxW, boxH int, border *wire.DeriveBorder) (string, error) {
	level := wire.DeriveDefaultECLevel
	if s.ECLevel != "" {
		level = s.ECLevel
	}
	lv, err := qr.ParseLevel(level)
	if err != nil {
		return "", err
	}
	m, err := qr.Encode([]byte(s.Data), lv)
	if err != nil {
		return "", err
	}

	// The stroke sits inside the border-box, so the drawable area is the box
	// less twice the border width.
	inner := 0
	if border != nil {
		inner = 2 * border.Width
	}
	availW, availH := boxW-inner, boxH-inner
	// The four-module quiet zone is part of the symbol, not decoration: a QR
	// without it is materially harder to acquire, and against a gradient backing
	// it may not be acquirable at all.
	const quiet = 4
	span := m.Size + 2*quiet
	unit := availW / span
	if availH/span < unit {
		unit = availH / span
	}
	if unit < 1 {
		return "", fmt.Errorf(
			"qr: a %d-module symbol with its quiet zone needs at least %d px, but only %dx%d px are available — enlarge the layer or shorten the payload",
			m.Size, span, availW, availH)
	}

	side := span * unit
	fg := colorOr(s.Color, wire.DeriveDefaultForeground)
	var b strings.Builder
	fmt.Fprintf(&b, `<svg width="%d" height="%d" viewBox="0 0 %d %d" shape-rendering="crispEdges" xmlns="http://www.w3.org/2000/svg">`,
		side, side, span, span)
	// The quiet zone must be the symbol's LIGHT colour, not the layer's
	// background: over a gradient the modules would otherwise sit on a varying
	// backdrop and the contrast a scanner needs would not be there at the edges.
	fmt.Fprintf(&b, `<rect width="%d" height="%d" fill="%s"/>`, span, span, quietZoneColor(s))
	b.WriteString(`<path fill="` + fg + `" d="`)
	for y := 0; y < m.Size; y++ {
		for x := 0; x < m.Size; x++ {
			if m.At(x, y) {
				fmt.Fprintf(&b, "M%d %dh1v1h-1z", x+quiet, y+quiet)
			}
		}
	}
	b.WriteString(`"/></svg>`)
	return b.String(), nil
}

// quietZoneColor is the symbol's LIGHT colour. It is derived from the
// foreground rather than authored: a QR is only scannable as a two-tone image,
// so the pair is one decision, and letting an operator pick both independently
// is letting them author an unscannable sign.
func quietZoneColor(s *wire.DeriveSpec) string {
	r, g, b := hexRGB(colorOr(s.Color, wire.DeriveDefaultForeground))
	// Rec. 601 luma, integer arithmetic. A dark foreground gets a white field; a
	// light foreground (white modules on a dark slide) gets a black one.
	if (299*r+587*g+114*b)/1000 < 128 {
		return "#FFFFFF"
	}
	return "#000000"
}
