package derive

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"testing"
)

func encodePNG(t *testing.T, img image.Image) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode fixture png: %v", err)
	}
	return buf.Bytes()
}

func solidRGBA(w, h int, c color.NRGBA) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetNRGBA(x, y, c)
		}
	}
	return img
}

// TestNormalizePNGRefusesAMissizedCapture. The PNG becomes an `image` layer of
// the AUTHORED geometry, so a capture at the wrong size is silently rescaled on
// the panel — blurred text, an unscannable QR — with nothing anywhere reporting
// a fault. The usual cause is a host whose device scale factor the emulation
// override failed to pin, which produces a perfectly valid 2x image.
func TestNormalizePNGRefusesAMissizedCapture(t *testing.T) {
	raw := encodePNG(t, solidRGBA(200, 100, color.NRGBA{R: 255, A: 255}))
	if _, _, err := NormalizePNG(raw, 100, 50); err == nil {
		t.Fatal("a 200x100 capture was accepted for a 100x50 layer")
	}
	if _, _, err := NormalizePNG(raw, 200, 100); err != nil {
		t.Fatalf("the correctly-sized capture was refused: %v", err)
	}
}

// TestNormalizePNGDetectsABlankCapture, in both directions. Reporting blank is
// what triggers the recapture that recovers a font race; reporting blank
// WRONGLY would make every fully-painted opaque layer take a second capture and
// then fail, which is the mirror-direction mistake this repo keeps making when
// it adds a guard.
func TestNormalizePNGDetectsABlankCapture(t *testing.T) {
	transparent := encodePNG(t, solidRGBA(20, 20, color.NRGBA{}))
	if _, blank, err := NormalizePNG(transparent, 20, 20); err != nil || !blank {
		t.Errorf("a fully transparent capture was not reported blank (blank=%v err=%v)", blank, err)
	}

	painted := encodePNG(t, solidRGBA(20, 20, color.NRGBA{R: 1, A: 1}))
	if _, blank, err := NormalizePNG(painted, 20, 20); err != nil || blank {
		t.Errorf("a capture with one barely-visible pixel value was reported blank (blank=%v err=%v)", blank, err)
	}

	// An opaque image with no alpha channel at all — a Gray or an RGB PNG —
	// must never read as blank, because "no alpha" means "fully opaque".
	gray := image.NewGray(image.Rect(0, 0, 20, 20))
	if _, blank, err := NormalizePNG(encodePNG(t, gray), 20, 20); err != nil || blank {
		t.Errorf("an alpha-less black image was reported blank (blank=%v err=%v)", blank, err)
	}

	// One opaque pixel in an otherwise transparent field is NOT blank: the guard
	// must not round "almost empty" up to "empty".
	nearly := solidRGBA(20, 20, color.NRGBA{})
	nearly.SetNRGBA(19, 19, color.NRGBA{R: 255, G: 255, B: 255, A: 255})
	if _, blank, err := NormalizePNG(encodePNG(t, nearly), 20, 20); err != nil || blank {
		t.Errorf("an image with one painted pixel was reported blank (blank=%v err=%v)", blank, err)
	}
}

// TestNormalizePNGIsDeterministic: the same pixels must re-encode to the same
// bytes, because those bytes ARE the asset's identity. Re-encoding is what
// removes the browser's own encoder settings and any metadata from the digest.
func TestNormalizePNGIsDeterministic(t *testing.T) {
	img := solidRGBA(64, 64, color.NRGBA{R: 12, G: 200, B: 90, A: 255})
	img.SetNRGBA(3, 4, color.NRGBA{R: 255, A: 128})
	raw := encodePNG(t, img)

	first, _, err := NormalizePNG(raw, 64, 64)
	if err != nil {
		t.Fatalf("NormalizePNG: %v", err)
	}
	for i := 0; i < 5; i++ {
		again, _, err := NormalizePNG(raw, 64, 64)
		if err != nil {
			t.Fatalf("NormalizePNG (repeat %d): %v", i, err)
		}
		if !bytes.Equal(first, again) {
			t.Fatalf("repeat %d re-encoded to different bytes — the asset digest is not stable", i)
		}
	}
	// And the pixels survive the round trip.
	back, err := png.Decode(bytes.NewReader(first))
	if err != nil {
		t.Fatalf("decode the normalised png: %v", err)
	}
	r, g, b, a := back.At(3, 4).RGBA()
	if a == 0 || r == 0 || g != 0 || b != 0 {
		t.Errorf("the normalised image lost its pixels: got rgba(%d,%d,%d,%d)", r, g, b, a)
	}
}

// TestNormalizePNGRejectsNonPNGBytes: a DevTools reply that is not an image
// must be reported, not stored. Uploading it would put unopenable bytes behind
// an asset_ref a screen then tries to draw.
func TestNormalizePNGRejectsNonPNGBytes(t *testing.T) {
	if _, _, err := NormalizePNG([]byte("not a png"), 10, 10); err == nil {
		t.Fatal("arbitrary bytes were accepted as a capture")
	}
}
