package derive

import (
	"bytes"
	"context"
	"image"
	"image/png"
	"testing"
	"time"

	"github.com/maaxton/waiveo-next/internal/derive/qr"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// browser_test.go is the half that needs a real Chromium. It SKIPS when none is
// found, because CI must stay green on a machine with no browser — and it is
// exactly the reason every guard lives in Runner rather than in Browser, so the
// skippable part is only "does Chromium draw what we asked".
//
// Skipping is stated loudly rather than silently: a suite that quietly skipped
// the only test of the rasterizer would read as a green rasterizer.

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

	mismatches := 0
	for my := 0; my < want.Size; my++ {
		for mx := 0; mx < want.Size; mx++ {
			px := originX + (mx+4)*unit + unit/2
			py := originY + (my+4)*unit + unit/2
			if isDark(img, px, py) != want.At(mx, my) {
				mismatches++
			}
		}
	}
	if mismatches != 0 {
		t.Errorf("%d of %d modules read back wrong from the rendered PNG — the symbol did not survive the pipeline",
			mismatches, want.Size*want.Size)
	}
}

func isDark(img image.Image, x, y int) bool {
	r, g, b, _ := img.At(x, y).RGBA()
	return (299*int(r>>8)+587*int(g>>8)+114*int(b>>8))/1000 < 128
}

// TestChromiumOutputIsByteStable is the property content-addressing rests on,
// measured against the real browser rather than assumed from the flags. If two
// runs of the same spec produced different bytes, every derive pass would mint a
// new asset, every screen would refetch content that had not changed, and the
// content origin would grow without bound.
func TestChromiumOutputIsByteStable(t *testing.T) {
	b := browserOrSkip(t)
	page, err := BuildPage(Job{Key: "stable", W: 600, H: 240, Spec: gradientTextSpec()})
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
		t.Errorf("two renders of the same page produced %d and %d bytes that differ — the output is not content-addressable",
			len(first), len(second))
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
