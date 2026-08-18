package derive

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// TestLaunchPinsTheDeterminismFlags is the only test that can notice a
// determinism flag being deleted or given a different value, and it exists
// because the real-render tests structurally cannot.
//
// Every render assertion in browser_test.go compares two captures taken with
// ONE argv on ONE host, so a flag that is wrong but CONSISTENT lands identically
// in both and every comparison stays green. That is measured, not argued:
//
//   - changing --force-device-scale-factor to 2 really does change the output
//     (stableSpec 16201 -> 16199 bytes, different digest; the text page likewise)
//     and all four TestChromium* tests still passed, on macOS Chrome and in the
//     Linux gate image alike;
//   - deleting the whole determinism block left the output byte-identical on
//     both, so nothing would have gone red on the way to losing every one of
//     them.
//
// Two design choices follow from that, and both are deliberate.
//
// It reads the argv the browser process ACTUALLY receives rather than the return
// value of some extracted helper: the stub records "$@" and exits, so the
// assertion covers exec.Command itself and cannot drift from what launch builds.
//
// And it needs no Chromium. That is not a convenience — it is the point. The
// pre-merge gate runs an image with no browser, where every render assertion in
// this package skips while `go test` still prints `ok`; this one runs there too,
// so the flags stay covered on the tier that has no eyes on the pixels.
//
// A flag here is a CLAIM about the renderer, so changing one means changing the
// claim: update the reason next to it, or delete both.
func TestLaunchPinsTheDeterminismFlags(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the recording stub is a POSIX shell script")
	}

	dir := t.TempDir()
	argvFile := filepath.Join(dir, "argv")
	stub := filepath.Join(dir, "recording-chromium")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"" + argvFile + "\"\n" +
		"echo 'stub browser: recorded its argv and exited' >&2\nexit 1\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatalf("write the recording stub: %v", err)
	}

	b, err := NewBrowser(BrowserOptions{ExecPath: stub, LaunchTimeout: 10 * time.Second})
	if err != nil {
		t.Fatalf("NewBrowser: %v", err)
	}
	page, err := BuildPage(Job{Key: "flags", W: 64, H: 64, Spec: &wire.DeriveSpec{
		Kind: wire.DeriveKindRect,
		Fill: &wire.DeriveFill{Kind: wire.DeriveFillSolid, From: "#FFFFFF"},
	}})
	if err != nil {
		t.Fatalf("BuildPage: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := b.Render(ctx, page); err == nil {
		t.Fatal("Render reported success against a stub that exits immediately")
	}

	raw, err := os.ReadFile(argvFile)
	if err != nil {
		t.Fatalf("the stub browser recorded no argv, so the launch never reached exec: %v", err)
	}
	var argv []string
	for _, a := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if a != "" {
			argv = append(argv, a)
		}
	}
	if len(argv) == 0 {
		t.Fatal("the stub browser recorded an empty argv")
	}

	for _, want := range []struct{ flag, why string }{
		{"--headless=new", "the headless MODE is itself a raster input: --headless=old consumes the font switches below, --headless=new does not, so the two modes antialias text differently"},
		{"--force-color-profile=srgb", "without it the capture is converted through whatever profile the host advertises; display-p3 was measured to move the bytes"},
		{"--force-device-scale-factor=1", "the LAUNCH-time scale factor is a separate raster input from the page's own; at 2 the pixels change while the PNG keeps the authored geometry, so no size guard catches it"},
		{"--disable-skia-runtime-opts", "Skia picks its SIMD path from the CPU's feature set, so without this two machines with different CPUs rasterize one page differently"},
		{"--disable-field-trial-config", "each Render gets a fresh --user-data-dir, which re-rolls field-trial assignment, so two launches on one host can take different rendering paths"},
		{"--disable-variations-seed-fetch", "the other half of the same: no seed fetched means no trial to be assigned by"},
		{"--disable-lcd-text", "subpixel-antialiased text is a function of the assumed subpixel order; live on headless_shell builds"},
		{"--font-render-hinting=none", "hinting snaps glyph outlines to the pixel grid differently per host fontconfig; live on headless_shell builds"},
		{"--disable-font-subpixel-positioning", "glyph origins on fractional pixels are the wobble this package's text test tolerates; live on headless_shell builds"},
		{"--deterministic-mode", "headless_shell's umbrella switch; its two useful implications are passed explicitly as well, so nothing depends on it alone"},
		{"--run-all-compositor-stages-before-draw", "draw only once every stage has run — this is what holds the capture stable on a loaded machine"},
		{"--disable-new-content-rendering-timeout", "never substitute a placeholder frame because content was slow, which is a blank or half-painted capture"},
		{"--disable-background-timer-throttling", "a throttled timer changes WHEN the page settles, and the capture is taken relative to that"},
		{"--disable-backgrounding-occluded-windows", "a headless window is never visible; treated as occluded it stops painting and the capture races it"},
		{"--disable-renderer-backgrounding", "same failure from the renderer's side"},
	} {
		name, _, _ := strings.Cut(want.flag, "=")
		var found []string
		for _, got := range argv {
			if got == name || strings.HasPrefix(got, name+"=") {
				found = append(found, got)
			}
		}
		switch {
		case len(found) == 0:
			t.Errorf("the browser was launched without %s\n  it is there because: %s", want.flag, want.why)
		case len(found) > 1:
			// Chromium's own precedence between two spellings of one switch is
			// not something this renderer should be relying on.
			t.Errorf("%s was passed %d times (%v) — one of them is silently winning\n  it is there because: %s",
				name, len(found), found, want.why)
		case found[0] != want.flag:
			t.Errorf("%s was passed as %q, want %q\n  it is there because: %s\n"+
				"  no render test can see this: both captures they compare are taken with the SAME argv",
				name, found[0], want.flag, want.why)
		}
	}
}
