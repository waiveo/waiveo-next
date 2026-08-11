package derive

import (
	"bytes"
	"fmt"
	"image"
	"image/png"
)

// png.go re-encodes a capture into the bytes that get content-addressed, and
// answers the one question the blank-recapture guard asks of it.
//
// Re-encoding rather than shipping Chromium's own PNG matters for the same
// reason the launch flags do: the asset_ref is the sha256 of these bytes, so any
// input to the encoding that is not the picture is an input to the identity. A
// browser's PNG encoder settings, chunk ordering and any ancillary metadata it
// chooses to emit are exactly that. Decoding to pixels and re-encoding through
// Go's encoder at a fixed compression level leaves the picture as the only
// thing the digest depends on.

// NormalizePNG decodes a capture, checks its geometry, and re-encodes it
// deterministically. It reports whether the image is fully transparent, which is
// the signature of a capture taken before the page painted.
func NormalizePNG(raw []byte, wantW, wantH int) (out []byte, blank bool, err error) {
	img, err := png.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, false, fmt.Errorf("derive: decode capture: %w", err)
	}
	b := img.Bounds()
	// A capture at the wrong size is never acceptable, because the PNG becomes
	// an `image` layer of the AUTHORED geometry: a mismatch would be silently
	// rescaled on the panel — blurry text, an unscannable QR — with nothing
	// anywhere reporting a fault. The usual cause is a device-scale-factor the
	// emulation override failed to pin.
	if b.Dx() != wantW || b.Dy() != wantH {
		return nil, false, fmt.Errorf("derive: capture is %dx%d, want %dx%d", b.Dx(), b.Dy(), wantW, wantH)
	}

	var buf bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.BestCompression}
	if err := enc.Encode(&buf, img); err != nil {
		return nil, false, fmt.Errorf("derive: re-encode capture: %w", err)
	}
	return buf.Bytes(), fullyTransparent(img), nil
}

// fullyTransparent reports whether every pixel has zero alpha.
//
// An image with no alpha channel is never blank — it is opaque by definition —
// and answering otherwise would make the recapture guard fire on every fully
// painted opaque layer.
func fullyTransparent(img image.Image) bool {
	if !hasAlpha(img) {
		return false
	}
	b := img.Bounds()
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			if _, _, _, a := img.At(x, y).RGBA(); a != 0 {
				return false
			}
		}
	}
	return true
}

func hasAlpha(img image.Image) bool {
	switch img.(type) {
	case *image.NRGBA, *image.NRGBA64, *image.RGBA, *image.RGBA64, *image.Alpha, *image.Alpha16:
		return true
	default:
		return false
	}
}
