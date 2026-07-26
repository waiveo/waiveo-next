//go:build ignore

// Command gen generates this directory's three committed demo-cast PNG
// assets (demo-1.png, demo-2.png, demo-3.png) — real, distinct, decodable
// images (not the 2x2 placeholder fixture the rest of this package's tests
// use), for feeder-side seeding of a multi-item demo cast
// (snapshot.DemoCastItems). Each is a solid gradient background with a large
// numeral drawn from a tiny hand-rolled 5x7 bitmap font, so the three are
// visually distinguishable at a glance on a real screen.
//
// Regenerate with: go run gen.go   (run from this directory)
//
// This uses only the Go standard library (image/image/color/image/png) —
// no external dependencies, no cgo, matching this codebase's demo-asset
// generation elsewhere (cmd/waiveo-feeder's placeholderImage).
package main

import (
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
)

const (
	width  = 400
	height = 225
)

// digitFont is a 5x7 bitmap font for the digits this generator draws
// ('1' by '3' by all three; the map is generic so a later regenerate can
// draw any digit 0-9). '1' in a row means that cell is lit.
var digitFont = map[rune][7]string{
	'0': {
		"01110",
		"10001",
		"10011",
		"10101",
		"11001",
		"10001",
		"01110",
	},
	'1': {
		"00100",
		"01100",
		"00100",
		"00100",
		"00100",
		"00100",
		"01110",
	},
	'2': {
		"01110",
		"10001",
		"00001",
		"00010",
		"00100",
		"01000",
		"11111",
	},
	'3': {
		"11111",
		"00010",
		"00100",
		"00010",
		"00001",
		"10001",
		"01110",
	},
}

// gradient describes one image's background: a linear blend from c0 (at t=0)
// to c1 (at t=1), sampled along either the x axis (horizontal) or the y axis
// (vertical), t in [0,1].
type gradient struct {
	c0, c1     color.RGBA
	horizontal bool
}

func (g gradient) at(x, y int) color.RGBA {
	var t float64
	if g.horizontal {
		t = float64(x) / float64(width-1)
	} else {
		t = float64(y) / float64(height-1)
	}
	lerp := func(a, b uint8) uint8 {
		return uint8(float64(a) + t*(float64(b)-float64(a)))
	}
	return color.RGBA{
		R: lerp(g.c0.R, g.c1.R),
		G: lerp(g.c0.G, g.c1.G),
		B: lerp(g.c0.B, g.c1.B),
		A: 0xff,
	}
}

// demoImage renders one canvas: grad fills the background, digit is drawn
// centered in digitColor at blockSize px per font cell.
func demoImage(grad gradient, digit rune, digitColor color.RGBA, blockSize int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			img.SetRGBA(x, y, grad.at(x, y))
		}
	}

	font, ok := digitFont[digit]
	if !ok {
		panic(fmt.Sprintf("no font entry for digit %q", digit))
	}
	digitW := 5 * blockSize
	digitH := 7 * blockSize
	x0 := (width - digitW) / 2
	y0 := (height - digitH) / 2
	for row := 0; row < 7; row++ {
		for col := 0; col < 5; col++ {
			if font[row][col] != '1' {
				continue
			}
			for py := 0; py < blockSize; py++ {
				for px := 0; px < blockSize; px++ {
					img.SetRGBA(x0+col*blockSize+px, y0+row*blockSize+py, digitColor)
				}
			}
		}
	}
	return img
}

func writePNG(path string, img *image.RGBA) {
	f, err := os.Create(path)
	if err != nil {
		panic(err)
	}
	defer f.Close()
	if err := png.Encode(f, img); err != nil {
		panic(err)
	}
}

func main() {
	white := color.RGBA{R: 0xff, G: 0xff, B: 0xff, A: 0xff}

	writePNG("demo-1.png", demoImage(gradient{
		c0: color.RGBA{R: 0x1e, G: 0x1b, B: 0x4d, A: 0xff}, // deep indigo
		c1: color.RGBA{R: 0x7c, G: 0x3a, B: 0xed, A: 0xff}, // violet
	}, '1', white, 22))

	writePNG("demo-2.png", demoImage(gradient{
		c0:         color.RGBA{R: 0xb9, G: 0x1d, B: 0x1d, A: 0xff}, // deep red
		c1:         color.RGBA{R: 0xf5, G: 0x9e, B: 0x0b, A: 0xff}, // amber
		horizontal: true,
	}, '2', white, 22))

	writePNG("demo-3.png", demoImage(gradient{
		c0: color.RGBA{R: 0x06, G: 0x4e, B: 0x3b, A: 0xff}, // deep teal
		c1: color.RGBA{R: 0x34, G: 0xd3, B: 0x99, A: 0xff}, // mint
	}, '3', white, 22))

	fmt.Println("wrote demo-1.png, demo-2.png, demo-3.png")
}
