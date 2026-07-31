package playlistcover

import (
	"crypto/sha256"
	"image"
	"image/color"
	"image/draw"
	"math"
)

// patternFor derives a deterministic background from a seed.
//
// The same definition must produce the same picture every time, on every
// instance, for ever: a cover that changed on each rebuild would make a
// playlist look as though something had tampered with it. So the only input is
// a SHA-256 of the seed, walked byte by byte — no map iteration, no clock, no
// package-level rand, and nothing that varies with Go's version.
//
// The playlist *name* is deliberately not part of the seed. It is drawn on top
// by the caller, so a rename changes the words on the cover and leaves the
// picture underneath alone, which is what a rename should do.
func patternFor(seed string) *image.RGBA {
	sum := sha256.Sum256([]byte(seed))

	base := hsv(float64(sum[0])/256*360, 0.52, 0.26)
	accent := hsv(float64(sum[1])/256*360, 0.66, 0.58)

	img := image.NewRGBA(image.Rect(0, 0, Size, Size))
	// A diagonal ramp, so the cover reads as designed rather than as a fill.
	for y := 0; y < Size; y++ {
		for x := 0; x < Size; x++ {
			img.SetRGBA(x, y, mix(base, accent, float64(x+y)/float64(2*Size)))
		}
	}
	// Eight vertical bands, each positioned and sized from its own digest byte,
	// so two definitions differ visibly rather than only in hue. Drawn with a
	// fixed low alpha so they read as texture rather than as content.
	for i := range 8 {
		w := 20 + int(sum[8+i])%70
		x := int(sum[16+i]) * Size / 256
		band := image.Rect(x, 0, min(x+w, Size), Size)
		shade := color.RGBA{R: accent.R, G: accent.G, B: accent.B, A: 40}
		draw.Draw(img, band, &image.Uniform{C: shade}, image.Point{}, draw.Over)
	}
	return img
}

// mix blends two opaque colours.
func mix(a, b color.RGBA, t float64) color.RGBA {
	if t < 0 {
		t = 0
	}
	if t > 1 {
		t = 1
	}
	lerp := func(x, y uint8) uint8 { return uint8(float64(x) + (float64(y)-float64(x))*t) }
	return color.RGBA{R: lerp(a.R, b.R), G: lerp(a.G, b.G), B: lerp(a.B, b.B), A: 255}
}

// hsv converts to RGB. Hue in degrees, saturation and value in 0..1.
//
// Written out rather than pulled from a dependency: it is twenty lines, it is
// the only colour maths in the project, and go.mod is already gaining one entry
// this phase.
func hsv(h, s, v float64) color.RGBA {
	h = math.Mod(math.Mod(h, 360)+360, 360)
	c := v * s
	x := c * (1 - math.Abs(math.Mod(h/60, 2)-1))
	m := v - c

	var r, g, b float64
	switch {
	case h < 60:
		r, g, b = c, x, 0
	case h < 120:
		r, g, b = x, c, 0
	case h < 180:
		r, g, b = 0, c, x
	case h < 240:
		r, g, b = 0, x, c
	case h < 300:
		r, g, b = x, 0, c
	default:
		r, g, b = c, 0, x
	}
	return color.RGBA{
		R: uint8((r + m) * 255), G: uint8((g + m) * 255), B: uint8((b + m) * 255), A: 255,
	}
}
