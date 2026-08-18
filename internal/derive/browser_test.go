package derive

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/maaxton/waiveo-next/internal/derive/qr"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// browser_test.go is the half that needs a real Chromium. It SKIPS when none is
// found, so a contributor without a browser can still run the suite — and it is
// exactly the reason every guard lives in Runner rather than in Browser, so the
// skippable part is only "does Chromium draw what we asked".
//
// CI is NOT that machine and these tests are not decorative there: the
// merge-tier runner (ubuntu-latest) ships both Chromium and Chrome, the workflow
// clears the unprivileged-userns restriction so Chromium's sandbox can start,
// and all four tests below run on every merge.
//
// The skip is worth distrusting anyway, because `go test` without -v prints
// `ok` for a package whose assertions all skipped. That is not hypothetical: the
// pre-merge gate ran an image with no browser and reported this package green in
// 0.3s while CI spent 28s actually rendering. Run the gate on an image that HAS
// a Chromium, and treat a suspiciously fast `internal/derive` as a skipped one.

func browserOrSkip(t *testing.T) *Browser {
	t.Helper()
	path, err := FindChromium()
	if err != nil {
		t.Skipf("no Chromium on this machine, skipping the real-render tests: %v", err)
	}
	// A launch bound generous enough for a loaded shared runner. The production
	// default is 30s, which is right for an appliance rendering on its own box
	// and wrong here: these tests first ran in CI on 2026-08-12 (the sandbox fix
	// that made them runnable at all), and the FIRST slow runner they met took
	// 34s just to announce a DevTools endpoint — the package had taken 16.6s in
	// total two runs earlier.
	//
	// Raising it costs the suite nothing and weakens no assertion. What these
	// tests check is whether Chromium DRAWS what was asked — the QR reads back,
	// the bytes are stable, the shadow stays inside the clip — and none of that
	// is a statement about launch latency. The bound is here so a wedged browser
	// fails instead of hanging; 120s still does that, well inside the job's own
	// 15-minute limit, while a 30s bound turns "the runner was busy" into a red
	// main.
	b, err := NewBrowser(BrowserOptions{ExecPath: path, LaunchTimeout: 120 * time.Second})
	if err != nil {
		t.Skipf("Chromium at %s is not usable: %v", path, err)
	}
	return b
}

// TestChromiumRendersAScannableQR is the strongest assertion available for this
// feature, and it is worth the browser it costs.
//
// It renders a QR layer through the REAL pipeline — page builder, Chromium, clip,
// PNG normalisation — and then reads the module grid back OUT of the resulting
// pixels and compares it to what the encoder produced. That proves every step in
// between preserved the symbol: the integer module sizing, the quiet zone, the
// clip geometry, the device-scale pin, and the re-encode. An assertion that the
// PNG is merely "the right size and not blank" would pass on a picture no phone
// could read.
func TestChromiumRendersAScannableQR(t *testing.T) {
	b := browserOrSkip(t)
	const payload = "https://waiveo.local/pair/ABCD-1234"
	const w, h = 420, 420

	spec := &wire.DeriveSpec{
		Kind: wire.DeriveKindQR, Data: payload, ECLevel: "M", Color: "#000000",
		Fill: &wire.DeriveFill{Kind: wire.DeriveFillSolid, From: "#FFFFFF"},
	}
	page, err := BuildPage(Job{Key: "qr", W: w, H: h, Spec: spec})
	if err != nil {
		t.Fatalf("BuildPage: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	raw, err := b.Render(ctx, page)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	img, err := png.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("decode the rendered PNG: %v", err)
	}
	if img.Bounds().Dx() != w || img.Bounds().Dy() != h {
		t.Fatalf("the PNG is %dx%d, want %dx%d", img.Bounds().Dx(), img.Bounds().Dy(), w, h)
	}

	want, err := qr.Encode([]byte(payload), qr.LevelM)
	if err != nil {
		t.Fatalf("encode the expected symbol: %v", err)
	}
	// The page centres a `span = size + 8` module grid at the largest integer
	// module size that fits, so the same arithmetic locates each module's centre.
	span := want.Size + 8
	unit := w / span
	side := span * unit
	originX := (w - side) / 2
	originY := (h - side) / 2

	// Mismatches are counted BY DIRECTION, because the count alone cannot tell
	// the two failure modes apart and this test has already failed once on CI in
	// a way nobody could attribute afterwards.
	//
	// A PARTIALLY COMPOSITED capture — the frame grabbed before every rect
	// painted — reads dark modules as light, so the misses pile up almost
	// entirely in darkReadLight and the image's overall dark fraction comes in
	// under what the symbol requires. A GEOMETRIC OFFSET — wrong scale, wrong
	// origin, fractional module size — flips modules in both directions at
	// roughly the rate their neighbours differ, so the two counts are
	// comparable and the dark fraction is about right.
	//
	// Reporting the geometry alongside them means the next failure names its own
	// cause instead of restarting the investigation.
	var darkReadLight, lightReadDark int
	for my := 0; my < want.Size; my++ {
		for mx := 0; mx < want.Size; mx++ {
			px := originX + (mx+4)*unit + unit/2
			py := originY + (my+4)*unit + unit/2
			got, expect := isDark(img, px, py), want.At(mx, my)
			if got == expect {
				continue
			}
			if expect {
				darkReadLight++
			} else {
				lightReadDark++
			}
		}
	}
	if n := darkReadLight + lightReadDark; n != 0 {
		wantDark := 0
		for my := 0; my < want.Size; my++ {
			for mx := 0; mx < want.Size; mx++ {
				if want.At(mx, my) {
					wantDark++
				}
			}
		}
		t.Errorf("%d of %d modules read back wrong from the rendered PNG — the symbol did not survive the pipeline\n"+
			"  expected-dark read light: %d   expected-light read dark: %d   (symbol has %d dark of %d)\n"+
			"  geometry: span=%d unit=%dpx side=%dpx origin=(%d,%d) in a %dx%d image\n"+
			"  a lopsided darkReadLight with a too-light image is a capture taken before the frame committed;\n"+
			"  comparable counts point at scale/origin geometry instead",
			n, want.Size*want.Size, darkReadLight, lightReadDark, wantDark, want.Size*want.Size,
			span, unit, side, originX, originY, w, h)
	}
}

func isDark(img image.Image, x, y int) bool {
	r, g, b, _ := img.At(x, y).RGBA()
	return (299*int(r>>8)+587*int(g>>8)+114*int(b>>8))/1000 < 128
}

// Ink thresholds for the repeatability comparison. Both specs under test fill
// with the #7C3AED -> #0EA5E9 gradient, whose brightest point is luma 128, so a
// floor just above it separates painted text from the background beneath. The
// ink>0 assertion in the caller is what fails loudly if that ever stops holding.
const (
	inkLuma    = 200 // bright enough to be a glyph rather than the gradient
	coverFloor = 130 // just above the gradient, for the weighted coverage sum
)

// pictureDiff is what a divergence between two renders looks like, expressed in
// the terms that say WHICH defect it is.
//
// The assertion this replaces printed two byte counts and nothing else, which is
// why the single CI failure it ever produced left no evidence: there was no way
// to tell whether the text, the gradient or the shadow had moved. Everything
// here exists to make the next divergence self-describing.
type pictureDiff struct {
	w, h       int
	nDiff      int // pixels differing at all
	maxDelta   int // largest single-channel difference, 0-255
	diffBox    [4]int
	inkA, inkB int   // pixels bright enough to be glyph coverage
	coverA     int64 // weighted ink, robust to antialiasing redistributing an edge
	coverB     int64
	boxA, boxB [4]int // extent of the ink in each render

	// The captures themselves, held so that a failure can hand over the actual
	// images. They are written to disk only by explain, and explain is only
	// reached once an assertion has already failed.
	rawA, rawB   []byte
	pathA, pathB string
}

// coverageDeltaPct is how much of the painted ink changed. It is a weighted sum
// rather than a thresholded count precisely so that a glyph edge sharing its
// coverage differently between two pixels does not register as a change: that
// motion is the wobble, and counting pixels above a cutoff would score it as a
// 1.2% difference when the ink is in fact conserved.
func (d *pictureDiff) coverageDeltaPct() float64 {
	if d.coverA == 0 {
		return 100
	}
	delta := d.coverA - d.coverB
	if delta < 0 {
		delta = -delta
	}
	return 100 * float64(delta) / float64(d.coverA)
}

func (d *pictureDiff) diffPixelPct() float64 {
	if d.w*d.h == 0 {
		return 0
	}
	return 100 * float64(d.nDiff) / float64(d.w*d.h)
}

// edgeShift is the largest movement of any single edge of the ink extent
// between the two renders. Each index is the SAME edge in both boxes, so a run
// that translated moves two of them together and a run whose advance changed
// moves one of them alone — either way the number is the distance the text
// travelled, in pixels.
func edgeShift(a, b [4]int) int {
	worst := 0
	for i := range a {
		d := a[i] - b[i]
		if d < 0 {
			d = -d
		}
		if d > worst {
			worst = d
		}
	}
	return worst
}

// explain is the failure message, and writing the two PNGs is part of it rather
// than a side errand.
//
// It is reached ONLY from a branch that has already decided to fail, which is
// what makes writing files here legitimate: a green run leaves nothing on disk,
// and a red one leaves the two images that disagree.
//
// They go to a plain os.MkdirTemp that is deliberately NOT cleaned up. An
// earlier version used t.TempDir, whose entire contract is that `go test`
// removes it on the way out — so the message named a path that had already been
// deleted by the time a developer read the message and tried to open it. The
// files now outlive the run; the OS reaps that directory on its own schedule.
func (d *pictureDiff) explain(t *testing.T) string {
	t.Helper()
	d.save(t)
	box := func(b [4]int) string {
		if b[2] <= b[0] || b[3] <= b[1] {
			return "empty"
		}
		return fmt.Sprintf("(%d,%d)-(%d,%d)", b[0], b[1], b[2], b[3])
	}
	where := fmt.Sprintf("  the two captures are on disk and are NOT cleaned up:\n    %s\n    %s", d.pathA, d.pathB)
	if d.pathA == "" {
		where = "  the two captures could not be written to disk (see the log line above)"
	}
	return fmt.Sprintf(
		"  canvas %dx%d: %d pixels differ (%.3f%%), largest channel delta %d/255, within %s\n"+
			"  ink: %d vs %d px, coverage %d vs %d (%.3f%%), extent %s vs %s (worst edge moved %dpx)\n%s",
		d.w, d.h, d.nDiff, d.diffPixelPct(), d.maxDelta, box(d.diffBox),
		d.inkA, d.inkB, d.coverA, d.coverB, d.coverageDeltaPct(), box(d.boxA), box(d.boxB),
		edgeShift(d.boxA, d.boxB), where)
}

// save writes the two captures once, memoised so that a test failing two
// assertions reports one pair of paths rather than two.
func (d *pictureDiff) save(t *testing.T) {
	t.Helper()
	if d.pathA != "" || d.rawA == nil || d.rawB == nil {
		return
	}
	dir, err := os.MkdirTemp("", "waiveo-derive-diff-*")
	if err != nil {
		t.Logf("could not make a directory to save the two captures in: %v", err)
		return
	}
	a, b := filepath.Join(dir, "first.png"), filepath.Join(dir, "second.png")
	if err := os.WriteFile(a, d.rawA, 0o600); err != nil {
		t.Logf("could not save the first capture: %v", err)
		return
	}
	if err := os.WriteFile(b, d.rawB, 0o600); err != nil {
		t.Logf("could not save the second capture: %v", err)
		return
	}
	d.pathA, d.pathB = a, b
}

// comparePictures measures two captures against each other. It only MEASURES —
// the images are kept on the diff and written out by explain, on the failure
// path, so a passing run writes nothing.
func comparePictures(t *testing.T, a, b []byte) *pictureDiff {
	t.Helper()
	ia, err := png.Decode(bytes.NewReader(a))
	if err != nil {
		t.Fatalf("decode first capture: %v", err)
	}
	ib, err := png.Decode(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("decode second capture: %v", err)
	}
	ra, rb := ia.Bounds(), ib.Bounds()
	if ra != rb {
		t.Fatalf("the two captures are different sizes (%v and %v) — a device scale factor slipped", ra, rb)
	}

	d := &pictureDiff{w: ra.Dx(), h: ra.Dy(), rawA: a, rawB: b}
	d.diffBox = [4]int{ra.Max.X, ra.Max.Y, ra.Min.X, ra.Min.Y}
	d.boxA = [4]int{ra.Max.X, ra.Max.Y, ra.Min.X, ra.Min.Y}
	d.boxB = [4]int{ra.Max.X, ra.Max.Y, ra.Min.X, ra.Min.Y}
	grow := func(box *[4]int, x, y int) {
		if x < box[0] {
			box[0] = x
		}
		if y < box[1] {
			box[1] = y
		}
		if x+1 > box[2] {
			box[2] = x + 1
		}
		if y+1 > box[3] {
			box[3] = y + 1
		}
	}
	ink := func(img image.Image, x, y int, n *int, cover *int64, box *[4]int) {
		r, g, bl, _ := img.At(x, y).RGBA()
		luma := (299*int(r>>8) + 587*int(g>>8) + 114*int(bl>>8)) / 1000
		if luma > coverFloor {
			*cover += int64(luma - coverFloor)
		}
		if luma > inkLuma {
			*n++
			grow(box, x, y)
		}
	}

	for y := ra.Min.Y; y < ra.Max.Y; y++ {
		for x := ra.Min.X; x < ra.Max.X; x++ {
			ink(ia, x, y, &d.inkA, &d.coverA, &d.boxA)
			ink(ib, x, y, &d.inkB, &d.coverB, &d.boxB)

			r1, g1, b1, a1 := ia.At(x, y).RGBA()
			r2, g2, b2, a2 := ib.At(x, y).RGBA()
			delta := 0
			for _, p := range [4][2]uint32{{r1, r2}, {g1, g2}, {b1, b2}, {a1, a2}} {
				v := int(p[0]>>8) - int(p[1]>>8)
				if v < 0 {
					v = -v
				}
				if v > delta {
					delta = v
				}
			}
			if delta > 0 {
				d.nDiff++
				if delta > d.maxDelta {
					d.maxDelta = delta
				}
				grow(&d.diffBox, x, y)
			}
		}
	}

	return d
}

// stableSpec is the byte-stability subject: every rasterization path the launch
// flags actually pin — linear gradient, blurred shadow, stroked border, rounded
// corners — and NO text, because glyph rasterization is the one input they
// provably do not pin (see the Determinism section of browser.go).
//
// The split is not a hunch. Rendered on two very different hosts — macOS "Google
// Chrome" on arm64, and Chromium 151 on Debian bookworm in the CI-shaped gate
// image — THIS spec produces the identical 16201-byte PNG, digest and all, while
// gradientTextSpec produces 25381 bytes there and 26272 here. Same code, same
// flags; the only difference is the host font stack, and only the spec with text
// in it can see one.
func stableSpec() *wire.DeriveSpec {
	return &wire.DeriveSpec{
		Kind:   wire.DeriveKindRect,
		Fill:   &wire.DeriveFill{Kind: wire.DeriveFillLinear, From: "#7C3AED", To: "#0EA5E9", AngleDeg: 135},
		Shadow: &wire.DeriveShadow{DY: 12, Blur: 30, Color: "#000000", OpacityPct: 55},
		Border: &wire.DeriveBorder{Width: 6, Color: "#F8FAFC", Radius: 32},
	}
}

// TestChromiumOutputIsByteStable holds the strict property — same spec, same
// bytes — where the renderer can genuinely deliver it.
//
// It used to render gradientTextSpec, and it went red on main once in 105 CI
// runs on a 171-byte difference out of ~25KB; the same commit passed on re-run.
// The subject changed rather than the assertion: this is still bytes.Equal with
// no tolerance to argue about, because everything it draws is a pure function of
// the device coordinates once the colour profile and device scale factor are
// pinned. What left the picture is the 96px "Hello", whose glyph edges are a
// hair-trigger for any difference in hinting or subpixel origin — a quarter-pixel
// shift moves ~1% of that page's pixels by up to 52/255, and the flags cannot
// stop it under --headless=new. Text repeatability is asserted next, against a
// property text can actually hold.
//
// What bit-equality here does and does NOT catch is worth stating, because the
// intuitive reading is wrong and an earlier version of this comment printed it.
//
// This compares two renders taken with ONE argv on ONE host. A constant that is
// wrong-but-CONSISTENT therefore lands identically in both captures and the
// comparison stays green — it is a self-comparison, not a golden. Measured on
// macOS Chrome and in the Linux gate image alike: changing
// --force-device-scale-factor to 2 changes the render (stableSpec 16201 -> 16199
// bytes, different digest) and all four TestChromium* tests still pass, and
// deleting the entire determinism flag block leaves the output byte-identical on
// both. So this test says nothing about the colour profile, the device scale
// factor or the PNG re-encode, all of which are the same code on both sides of
// the equality.
//
// What it does catch is NON-determinism: anything that differs BETWEEN two
// launches of the same argv — a compositor stage that stopped being awaited, a
// capture taken before the frame committed, a field trial re-rolled from the
// fresh profile dir, an encoder that hashed a map. That is a real class of
// defect and this is the only assertion that sees it. The CONSTANTS are held by
// TestLaunchPinsTheDeterminismFlags instead, which needs no browser.
func TestChromiumOutputIsByteStable(t *testing.T) {
	b := browserOrSkip(t)
	page, err := BuildPage(Job{Key: "stable", W: 600, H: 240, Spec: stableSpec()})
	if err != nil {
		t.Fatalf("BuildPage: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	first, err := b.Render(ctx, page)
	if err != nil {
		t.Fatalf("first Render: %v", err)
	}
	second, err := b.Render(ctx, page)
	if err != nil {
		t.Fatalf("second Render: %v", err)
	}
	if !bytes.Equal(first, second) {
		d := comparePictures(t, first, second)
		t.Errorf("two renders of the same textless page produced %d and %d bytes that differ.\n%s\n"+
			"  nothing here depends on glyph rasterization, and both renders ran the same argv on this\n"+
			"  host — so the launch flags are NOT the suspect. Something varied between the two launches:\n"+
			"  a compositor stage that stopped being awaited, a capture taken before the frame committed,\n"+
			"  a re-rolled field trial, or a non-deterministic step in the PNG re-encode",
			len(first), len(second), d.explain(t))
	}
}

// TestChromiumTextRendersRepeatably is the text half, and it deliberately
// asserts the PICTURE rather than the FILE.
//
// What production actually relies on, verified in the code rather than assumed:
//
//   - Staleness is the SPEC digest, never the pixels. wire.LayerDeriveState
//     compares a layer's DerivedFrom against wire.DeriveDigest(layer), so a
//     current layer is skipped by the pending listing (app/api/derive.go) and by
//     sync (sync.go) and is never rendered a second time at all.
//   - Duplicate layers inside one pass collapse by that same digest: sync groups
//     render work by wire.DeriveDigest and uploads once per digest, so a repeated
//     spec is rendered once by construction, not because the bytes matched.
//
// So byte-equality across two SEPARATE renders buys exactly one thing: when one
// spec is rendered in two different passes, or on two different machines, the
// content store holds one object instead of two. That is a dedupe efficiency
// worth ~25KB and reclaimable by contentgc — it is not correctness. The old
// comment here claimed the opposite ("every derive pass would mint a new asset
// ... the content origin would grow without bound"); both halves are false.
//
// The bounds below are measured, not chosen to make red go green. Rendering this
// page and comparing it against deliberately damaged variants of itself
// separates the two populations cleanly, but only on THREE axes — no per-pixel
// tolerance does, because a mere quarter-pixel glyph shift already reaches
// 52/255 on ~1% of pixels, overlapping real defects:
//
//	variant                        coverage Δ   pixels differing   ink edge moved
//	glyph origin +0.1px / +0.25px      ≤0.25%       0.4% / 0.9%          0px   <- wobble
//	whole text run +1px                 0.00%         1.4%               1px   <- wobble
//	whole text run +2px                 0.01%         1.9%               2px   <- DEFECT
//	whole text run +3px                 0.01%         2.5%               3px   <- DEFECT
//	whole text run +6px                 0.02%         4.1%               6px   <- DEFECT
//	whole text run +3px vertically      0.01%         1.9%               3px   <- DEFECT
//	letter-spacing 0 -> 1px             0.27%         1.6%               3px   <- DEFECT
//	text colour #FFFFFF -> #FEFEFE      0.82%         2.9%                     <- DEFECT
//	font-size 96 -> 96.5px              0.86%         1.1%                     <- DEFECT
//	font-size 96 -> 97px                1.78%         1.2%                     <- DEFECT
//	gradient angle 135 -> 136deg        0.00%        26.0%                     <- DEFECT
//	whole image at opacity 0.99         2.54%        99.0%                     <- DEFECT
//	default font -> Helvetica          22.71%         3.4%                     <- DEFECT
//	default font -> Courier            35.63%         5.0%                     <- DEFECT
//	text not painted at all           100.00%         3.3%                     <- DEFECT
//
// Coverage catches anything that changes WHAT is painted; the differing-pixel
// fraction catches anything that shifts the whole image subtly enough to keep
// coverage flat; the ink extent catches WHERE the run was set and at what
// advance, which is the block of rows in the middle — every one of them is
// inside BOTH percentage bounds, because a run that merely moves conserves its
// ink almost exactly. Dropping the extent assertion would make this test green
// on a text layer that landed six pixels away from where it landed last time.
func TestChromiumTextRendersRepeatably(t *testing.T) {
	b := browserOrSkip(t)
	page, err := BuildPage(Job{Key: "text-stable", W: 600, H: 240, Spec: gradientTextSpec()})
	if err != nil {
		t.Fatalf("BuildPage: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	first, err := b.Render(ctx, page)
	if err != nil {
		t.Fatalf("first Render: %v", err)
	}
	second, err := b.Render(ctx, page)
	if err != nil {
		t.Fatalf("second Render: %v", err)
	}
	d := comparePictures(t, first, second)

	// Two blank renders agree perfectly and mean the rasterizer drew nothing, so
	// the comparison is only meaningful once there is text to compare. This is
	// the trap a pure difference assertion falls into.
	if d.inkA == 0 || d.inkB == 0 {
		t.Fatalf("no text was painted (ink %d and %d pixels) — the page rendered, but the glyphs did not\n%s",
			d.inkA, d.inkB, d.explain(t))
	}
	// WHERE the run was set, which neither percentage below can see: a run that
	// moves as a whole carries its ink with it, so translating "Hello" six pixels
	// right costs 0.02% of coverage and 4.1% of pixels — inside both bounds.
	//
	// One pixel of slack, and not one more. The residual this test exists to
	// tolerate is a SUBPIXEL glyph origin, which can only flip the ink threshold
	// on the boundary pixel of an edge; an edge that moved two has moved the
	// TEXT, not its antialiasing. That is the class the old bytes.Equal
	// assertion used to cover and the percentages do not — a font-metrics flush
	// race of exactly the kind forcePaintJS exists to defeat lands here.
	const edgeSlack = 1
	if shift := edgeShift(d.boxA, d.boxB); shift > edgeSlack {
		t.Errorf("the two renders set the same text in different places: an ink edge moved %dpx (bound %dpx)\n%s\n"+
			"  the ink itself is conserved, so these are the same glyphs laid out differently on the second\n"+
			"  launch — a font-metrics flush race, a changed advance, or a different face resolved for one\n"+
			"  and the same spec. It is NOT antialiasing: a subpixel origin cannot move an edge this far",
			shift, edgeSlack, d.explain(t))
	}
	if pct := d.coverageDeltaPct(); pct > 0.5 {
		t.Errorf("the two renders painted materially different text: coverage differs by %.3f%% (bound 0.5%%)\n%s\n"+
			"  coverage is conserved when antialiasing merely redistributes ink along an edge, so this is a\n"+
			"  different typeface, a different size, or text that failed to paint — not rasterization wobble",
			pct, d.explain(t))
	}
	if pct := d.diffPixelPct(); pct > 5 {
		t.Errorf("the two renders differ across %.3f%% of the canvas (bound 5%%)\n%s\n"+
			"  a difference this broad with stable coverage and a stable extent is a whole-image shift —\n"+
			"  suspect a fill that is not a pure function of the spec, or a capture taken mid-composite",
			pct, d.explain(t))
	}
}

// TestChromiumKeepsTheShadowInsideTheCapture is the envelope rule measured in
// pixels: with the content inset by the shadow's extent, the shadow lands INSIDE
// the authored box.
//
// The corner is checked as well as the edge. Legacy's still-open "blur
// clip-expansion" item is exactly this defect — a shadow clipped at the element
// bounds — and it looks like a slightly hard edge, which nobody files a bug
// about.
func TestChromiumKeepsTheShadowInsideTheCapture(t *testing.T) {
	b := browserOrSkip(t)
	const w, h = 400, 200
	page, err := BuildPage(Job{Key: "shadow", W: w, H: h, Spec: &wire.DeriveSpec{
		Kind:   wire.DeriveKindRect,
		Fill:   &wire.DeriveFill{Kind: wire.DeriveFillSolid, From: "#FFFFFF"},
		Shadow: &wire.DeriveShadow{DY: 10, Blur: 20, Color: "#000000", OpacityPct: 80},
	}})
	if err != nil {
		t.Fatalf("BuildPage: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	raw, err := b.Render(ctx, page)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	img, err := png.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}

	// Directly below the box, inside the bottom inset (30px), the shadow must be
	// painting: partly opaque, and not the white fill.
	_, _, _, aBelow := img.At(w/2, h-8).RGBA()
	if aBelow == 0 {
		t.Errorf("nothing is painted below the box — the shadow was clipped away")
	}
	// The extreme corner of the capture is outside the shadow's reach and must
	// be fully transparent, which is what lets a derive layer composite over the
	// native layers beneath it instead of punching an opaque rectangle.
	if _, _, _, aCorner := img.At(0, 0).RGBA(); aCorner != 0 {
		t.Errorf("the capture's corner is opaque (alpha %d) — a derive layer must composite, not overwrite", aCorner)
	}
}

// TestARenderThatOverrunsItsDeadlineKillsTheBrowser exercises the process-tree
// kill against a REAL browser: a job cancelled mid-render must leave nothing
// behind, and Render must return promptly rather than blocking on a browser
// nobody is waiting for any more.
func TestARenderThatOverrunsItsDeadlineKillsTheBrowser(t *testing.T) {
	b := browserOrSkip(t)
	page, err := BuildPage(Job{Key: "cancelled", W: 300, H: 300, Spec: &wire.DeriveSpec{
		Kind: wire.DeriveKindRect,
		Fill: &wire.DeriveFill{Kind: wire.DeriveFillSolid, From: "#FFFFFF"},
	}})
	if err != nil {
		t.Fatalf("BuildPage: %v", err)
	}
	// A deadline far too short for a browser to launch, let alone paint.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err = b.Render(ctx, page)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("a render given 30ms reported success")
	}
	// The kill itself allows a 2s grace before SIGKILL, so the ceiling is
	// generous — what is being checked is that it RETURNS, not that it is fast.
	if elapsed > 20*time.Second {
		t.Errorf("Render took %s to abandon a 30ms job — the cancellation is not reaching the browser", elapsed)
	}
}
