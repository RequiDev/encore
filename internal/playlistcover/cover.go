// Package playlistcover renders the image Encore puts on a playlist it made.
//
// 640x640 JPEG: a 2x2 mosaic of album covers from the playlist's own tracks,
// with the playlist name over a scrim across the lower third.
//
// The spec this implements contradicts itself once, and the resolution is
// recorded here so nobody re-derives it from the contradiction. §1.2 says
// "fewer than four usable covers falls back to a deterministic geometric
// cover"; §3's test table says "one unreachable art URL yields a three-tile
// cover, not an error". Both cannot hold. What holds:
//
//   - zero usable images -> the pattern, byte-identical for a given definition.
//     This is the fresh-instance case §1.2 is describing.
//   - one to four -> a mosaic, with every empty cell filled from that same
//     pattern. This is the lost-tile case §3 is describing, and it keeps the
//     artwork that was found rather than discarding it.
//
// Both report Covered honestly, out of a denominator that is always Tiles.
//
// Nothing here touches the network or the database. Render takes decoded
// images and returns bytes; fetching them is a later task's job and uploading
// the result is the caller's.
package playlistcover

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"

	xdraw "golang.org/x/image/draw"
	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/gobold"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
)

const (
	// Size is the edge of the square cover, in pixels. Spotify shows a playlist
	// cover at many sizes and derives them all from one upload.
	Size = 640
	// Tiles is how many album covers the mosaic asks for, and the denominator
	// of every coverage sentence about a cover.
	Tiles = 4
	// MaxBytes is the binary JPEG ceiling. It must stay at or below
	// spotify.MaxPlaylistCoverBytes; a test in this package pins that, and the
	// constant is duplicated rather than imported so a pure image package does
	// not depend on the API client.
	MaxBytes = 190 * 1024
)

// Kind is how a cover was made.
type Kind string

const (
	// KindMosaic means at least one cell holds real album artwork.
	KindMosaic Kind = "mosaic"
	// KindPattern means no artwork was available and the whole cover is the
	// deterministic fallback.
	KindPattern Kind = "pattern"
)

// Cover is a finished cover and an honest account of how it was made.
type Cover struct {
	JPEG []byte
	Kind Kind
	// Covered is how many of Tiles came from real album artwork.
	Covered int
}

// Render builds the cover.
//
// seed is the canonical form of the playlist definition; the same seed always
// produces the same pattern. name is drawn over the result and is deliberately
// not part of the seed, so a rename changes the words without reshuffling the
// picture underneath them.
//
// A nil entry in tiles is a cell whose artwork could not be read. It is filled
// from the pattern rather than left blank, and it is not an error: the cover is
// best-effort, and one slow CDN must not cost a listener their playlist.
//
// Render never returns an error for anything about the input — not an empty
// name, not an unrenderable one, not a fully-nil tiles array. The only error
// path is encodeUnder exhausting its quality ladder, which by construction
// (MaxBytes <= spotify.MaxPlaylistCoverBytes, pinned by a test) should not
// happen for images this package itself produces; callers still best-effort
// this, since a picture must never be the reason a playlist fails.
func Render(name, seed string, tiles [Tiles]image.Image) (Cover, error) {
	pattern := patternFor(seed)
	canvas := image.NewRGBA(image.Rect(0, 0, Size, Size))

	covered := 0
	half := Size / 2
	for i, src := range tiles {
		cell := image.Rect((i%2)*half, (i/2)*half, (i%2+1)*half, (i/2+1)*half)
		if src == nil {
			draw.Draw(canvas, cell, pattern, cell.Min, draw.Src)
			continue
		}
		// CatmullRom rather than image/draw, which cannot downscale: it samples
		// rather than filters, so a 640px album cover reduced to 320 comes out
		// aliased into visible stair-stepping. This is the reason
		// golang.org/x/image is a dependency at all.
		xdraw.CatmullRom.Scale(canvas, cell, src, src.Bounds(), xdraw.Over, nil)
		covered++
	}

	drawScrim(canvas)
	if err := drawName(canvas, name); err != nil {
		return Cover{}, err
	}

	raw, _, err := encodeUnder(canvas, MaxBytes)
	if err != nil {
		return Cover{}, err
	}

	kind := KindMosaic
	if covered == 0 {
		kind = KindPattern
	}
	return Cover{JPEG: raw, Kind: kind, Covered: covered}, nil
}

const (
	// scrimTop is where the darkening band begins: the lower third, so the name
	// stays legible over artwork of any brightness.
	scrimTop = Size * 2 / 3
	// textMargin, textWidth and textBaseline place the name inside the scrim.
	textMargin   = 36
	textWidth    = Size - 2*textMargin
	textBaseline = Size - 58
)

// drawScrim darkens the lower third with a vertical ramp, so the band has no
// visible edge across the artwork above it.
func drawScrim(canvas *image.RGBA) {
	for y := scrimTop; y < Size; y++ {
		t := float64(y-scrimTop) / float64(Size-scrimTop)
		alpha := uint8(40 + t*175)
		row := image.Rect(0, y, Size, y+1)
		draw.Draw(canvas, row, &image.Uniform{C: color.RGBA{A: alpha}}, image.Point{}, draw.Over)
	}
}

// nameSizes are tried largest first; the first that fits within textWidth wins.
var nameSizes = []float64{60, 50, 42, 36, 30, 25}

// drawName writes the playlist name across the scrim on one line.
//
// One line, shrink to fit, then truncate with an ellipsis. Word wrapping is
// deliberately out of scope: two lines of variable height would move the
// baseline, which would move the scrim, and a 100-rune name is already served
// by shrinking and cutting. A cover is an identifier, not a paragraph.
//
// An empty name draws nothing. A name in a script the embedded face has no
// glyphs for (or one that is only emoji) draws the face's .notdef fallback
// glyph for each such rune rather than failing — see nameFace. Neither case
// can panic or overflow the canvas: MeasureString is consulted at every
// candidate size before anything is drawn, and the truncation loop below has a
// floor of one rune plus an ellipsis.
func drawName(canvas *image.RGBA, name string) error {
	if name == "" {
		return nil
	}
	parsed, err := opentype.Parse(gobold.TTF)
	if err != nil {
		return fmt.Errorf("parse the cover typeface: %w", err)
	}
	for _, points := range nameSizes {
		face, err := nameFace(parsed, points)
		if err != nil {
			return err
		}
		if advance := font.MeasureString(face, name); advance.Ceil() <= textWidth {
			write(canvas, face, name)
			return face.Close()
		}
		if points != nameSizes[len(nameSizes)-1] {
			_ = face.Close()
			continue
		}
		// The smallest size still does not fit: drop runes from the end until
		// the name plus an ellipsis does. Runes, not bytes — a cut through a
		// multi-byte rune would render a replacement glyph.
		runes := []rune(name)
		for len(runes) > 1 {
			runes = runes[:len(runes)-1]
			candidate := string(runes) + "…"
			if font.MeasureString(face, candidate).Ceil() <= textWidth {
				write(canvas, face, candidate)
				return face.Close()
			}
		}
		write(canvas, face, "…")
		return face.Close()
	}
	return nil
}

// write draws one line at the fixed baseline.
func write(canvas *image.RGBA, face font.Face, text string) {
	d := &font.Drawer{
		Dst:  canvas,
		Src:  image.NewUniform(color.RGBA{R: 255, G: 255, B: 255, A: 255}),
		Face: face,
		Dot:  fixed.P(textMargin, textBaseline),
	}
	d.DrawString(text)
}

// nameFace builds one size of the type used for the playlist name, from an
// already-parsed font. Parsing happens once per drawName call rather than once
// per candidate size, since nameSizes tries up to six.
//
// golang.org/x/image/font/gofont/gobold ships as Go source, so no font file and
// no licence file enters this repository. It is not the web client's Inter, and
// that is accepted: nobody sees a 640px playlist cover beside the dashboard.
// Embedding Inter later is this function plus an OFL notice.
//
// A name in a script Go Bold has no glyphs for renders as .notdef boxes rather
// than failing. That is the correct trade for a decorative image: a cover with
// boxes on it is better than a playlist with no cover and an error state.
func nameFace(parsed *opentype.Font, points float64) (font.Face, error) {
	face, err := opentype.NewFace(parsed, &opentype.FaceOptions{
		Size: points, DPI: 72, Hinting: font.HintingFull,
	})
	if err != nil {
		return nil, fmt.Errorf("build the cover typeface at %.0fpt: %w", points, err)
	}
	return face, nil
}

// coverQualities is the JPEG quality ladder, tried in order.
//
// 90 first because at 640x640 a four-way photographic mosaic normally lands at
// 60-100 KB and never needs a second attempt. The rungs below exist because
// four photographs is exactly the input that does not compress, and it is also
// the common one.
var coverQualities = []int{90, 80, 70, 60, 50, 40}

// encodeUnder encodes img at the highest quality whose output fits maxBytes,
// and reports how many attempts that took.
//
// It returns an error rather than the smallest attempt when even the floor is
// too large. Handing back an oversized buffer would push the decision onto the
// caller, whose only sensible response is this one — and whose likeliest
// mistaken response is to upload it, which Spotify rejects *after* the listener
// has been told a cover was set.
func encodeUnder(img image.Image, maxBytes int) ([]byte, int, error) {
	var buf bytes.Buffer
	for i, q := range coverQualities {
		buf.Reset()
		if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: q}); err != nil {
			return nil, i + 1, fmt.Errorf("encode cover at quality %d: %w", q, err)
		}
		if buf.Len() <= maxBytes {
			return bytes.Clone(buf.Bytes()), i + 1, nil
		}
	}
	return nil, len(coverQualities), fmt.Errorf(
		"cover is %d bytes at quality %d, over the %d ceiling",
		buf.Len(), coverQualities[len(coverQualities)-1], maxBytes)
}
