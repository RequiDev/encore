package playlistcover

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"testing"

	"github.com/RequiDev/encore/internal/spotify"
)

// noisyTile returns the least compressible 640x640 image this encoder will
// ever be handed: deterministic pseudo-random pixels, which JPEG cannot find
// any structure in.
//
// Deterministic rather than random, so a failure is reproducible. A real
// four-way photographic mosaic is the input the spec calls out as "exactly the
// input that gets large"; this is that case with the volume turned up.
func noisyTile(seed uint32) image.Image {
	img := image.NewRGBA(image.Rect(0, 0, Size, Size))
	x := seed | 1
	for i := 0; i < Size*Size; i++ {
		// xorshift32: no allocation, no package state, same sequence every run.
		x ^= x << 13
		x ^= x >> 17
		x ^= x << 5
		img.Set(i%Size, i/Size, color.RGBA{uint8(x), uint8(x >> 8), uint8(x >> 16), 255})
	}
	return img
}

// decodeJPEG decodes a rendered cover and fails the test if it is not a
// valid, decodable JPEG.
func decodeJPEG(t *testing.T, jpg []byte) image.Image {
	t.Helper()
	img, err := jpeg.Decode(bytes.NewReader(jpg))
	if err != nil {
		t.Fatalf("decode cover jpeg: %v", err)
	}
	return img
}

// brightnessSum is r+g+b at (x,y), 0..765. Cheap, seed-independent way to ask
// "is anything drawn here" without caring about hue.
func brightnessSum(img image.Image, x, y int) int {
	r, g, b, _ := img.At(x, y).RGBA()
	return int(r>>8) + int(g>>8) + int(b>>8)
}

// cellCenter is the midpoint of mosaic cell i, in the same 2x2 layout Render
// uses.
func cellCenter(i int) (x, y int) {
	half := Size / 2
	return (i%2)*half + half/2, (i/2)*half + half/2
}

// TestFourPhotographMosaicFitsUnderTheCeiling pins the size guarantee, the
// square-canvas guarantee, and that all four photographs actually reach their
// quadrants -- not just that the bookkeeping says they did.
//
// A first version of this test checked only len(JPEG), Kind and Covered.
// Deleting the tile-drawing call (xdraw.CatmullRom.Scale) in Render while
// leaving `covered++` in place produces a cover that is Kind mosaic, Covered
// 4, comfortably under the ceiling -- and completely blank, because a blank
// canvas is the most compressible input there is. None of the original
// assertions notice. Fixed by sampling the center of each cell and requiring
// real content there: empirically, a drawn noise tile averages to a mid-tone
// (sums of 248-390 out of a possible 765 across the four cells here); an
// undrawn cell stays at the canvas's zero value, black, summing to 0.
//
// The quality-ladder assertion below is a second, independent thing this test
// pins and needs its own explanation: the direct encodeUnder(noisyTile(9),
// MaxBytes) call does not go through Render at all, so it says nothing about
// whether Render's own encode step took more than one attempt. What it does
// prove is that the ladder itself (shared by Render and by direct callers)
// still steps down for genuinely incompressible input, rather than having
// been quietly deleted. The base64-vs-binary ceiling confusion this comment
// used to attribute to this assertion is actually caught by
// TestTheTwoCeilingsAgree -- corrected here after finding the same mutation
// left this test's attempts check passing (the mosaic already needed 2+
// attempts at the wider ceiling, since noisyTile(9) is raw unscaled noise,
// less compressible than the CatmullRom-smoothed mosaic tiles).
//
// Fails when: a cell's drawing call is skipped while Covered still counts it
// (the cell-content checks below fire); the ladder is reduced to a single
// quality; or the images are downscaled before encoding, which would make the
// first attempt fit and the attempt count drop to 1.
func TestFourPhotographMosaicFitsUnderTheCeiling(t *testing.T) {
	tiles := [Tiles]image.Image{noisyTile(1), noisyTile(2), noisyTile(3), noisyTile(4)}

	got, err := Render("Heavy rotation", "top|plays|100|0||", tiles)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if len(got.JPEG) > MaxBytes {
		t.Fatalf("cover is %d bytes, over the %d ceiling", len(got.JPEG), MaxBytes)
	}
	if got.Kind != KindMosaic || got.Covered != 4 {
		t.Fatalf("Kind/Covered = %q/%d, want mosaic/4", got.Kind, got.Covered)
	}

	img := decodeJPEG(t, got.JPEG)
	if b := img.Bounds(); b.Dx() != Size || b.Dy() != Size {
		t.Fatalf("cover is %dx%d, want %dx%d", b.Dx(), b.Dy(), Size, Size)
	}
	// A cell that actually holds a decoded, downscaled noise tile averages to
	// a mid-tone; an undrawn cell stays at the canvas's zero value, which is
	// black. 100 sits well clear of both (observed real content: 248-390;
	// blank: 0).
	const minCellBrightness = 100
	for i := range 4 {
		x, y := cellCenter(i)
		if sum := brightnessSum(img, x, y); sum < minCellBrightness {
			t.Errorf("cell %d center (%d,%d) has brightness sum %d, want > %d -- "+
				"the tile never reached its quadrant", i, x, y, sum, minCellBrightness)
		}
	}

	// The ladder must have stepped down for this input, or it proves nothing.
	_, attempts, err := encodeUnder(noisyTile(9), MaxBytes)
	if err != nil {
		t.Fatalf("encodeUnder: %v", err)
	}
	if attempts < 2 {
		t.Fatalf("noise encoded in %d attempt(s); the quality ladder never ran, so this "+
			"test proves nothing about it", attempts)
	}
}

// TestEncodeUnderRefusesRatherThanReturningAnOversizedImage pins the
// termination guarantee and the refusal.
//
// The ladder has a floor, so it cannot loop for ever; and when even the floor
// is too large it returns an error rather than the smallest attempt. Handing
// back an oversized buffer would push the decision onto a caller whose only
// sensible response is this one -- and whose *incorrect* response is to upload
// it, which Spotify rejects after the listener has been told a cover was set.
//
// Fails when: the loop returns the last attempt regardless of size, or the
// ladder is made unbounded (this call would then not return).
func TestEncodeUnderRefusesRatherThanReturningAnOversizedImage(t *testing.T) {
	jpeg, attempts, err := encodeUnder(noisyTile(7), 100)
	if err == nil {
		t.Fatalf("encodeUnder returned %d bytes for a 100-byte ceiling, want an error", len(jpeg))
	}
	if jpeg != nil {
		t.Error("encodeUnder returned an image alongside its error")
	}
	if attempts != len(coverQualities) {
		t.Errorf("attempts = %d, want the whole ladder (%d)", attempts, len(coverQualities))
	}
}

// TestOneLostTileStillYieldsAMosaic pins the spec's "three-tile cover, not an
// error" against its own contradictory fallback sentence -- see the plan's
// resolution of that ambiguity. It also pins the package doc's promise that
// the empty cell is filled "from that same pattern", not left blank.
//
// A first version of this test checked only Kind and Covered. Deleting the
// nil-tile fallback draw (draw.Draw(canvas, cell, pattern, cell.Min,
// draw.Src)) in Render leaves the lost quadrant at the canvas's zero value --
// solid black -- while Kind and Covered are computed from the *other* three
// cells and the covered counter, so neither notices: both checks still pass
// against a cover with a black hole in it. Fixed by comparing the lost cell's
// center against patternFor's own pixel at that point: empirically identical
// up to JPEG's rounding (observed a 1-in-255 difference per channel on the
// correct implementation), and nowhere close if the cell is left black
// (pattern colour here is neither black nor within tolerance of it).
//
// Fails when: Render falls back to the pattern whenever any tile is nil
// (Covered would read 0, not 3); or the lost cell's fallback draw is skipped,
// leaving it black instead of carrying the pattern's own colour.
func TestOneLostTileStillYieldsAMosaic(t *testing.T) {
	const seed = "top|plays|100|0||"
	tiles := [Tiles]image.Image{noisyTile(1), nil, noisyTile(3), noisyTile(4)}

	got, err := Render("Heavy rotation", seed, tiles)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if got.Kind != KindMosaic {
		t.Errorf("Kind = %q, want %q", got.Kind, KindMosaic)
	}
	if got.Covered != 3 {
		t.Errorf("Covered = %d, want 3", got.Covered)
	}

	img := decodeJPEG(t, got.JPEG)
	x, y := cellCenter(1) // the nil tile
	gr, gg, gb, _ := img.At(x, y).RGBA()
	pattern := patternFor(seed)
	pr, pg, pb, _ := pattern.At(x, y).RGBA()
	const tolerance = 20 // JPEG is lossy; observed drift was 1 per channel
	if diff(gr, pr) > tolerance || diff(gg, pg) > tolerance || diff(gb, pb) > tolerance {
		t.Errorf("lost cell center (%d,%d) is (%d,%d,%d), want ~(%d,%d,%d) (the pattern's own colour) -- "+
			"the empty cell was not filled from the pattern",
			x, y, gr>>8, gg>>8, gb>>8, pr>>8, pg>>8, pb>>8)
	}
}

// diff is the absolute difference between two color.RGBA-sourced RGBA()
// component values (each still in 0..0xffff).
func diff(a, b uint32) uint32 {
	if a > b {
		return (a - b) >> 8
	}
	return (b - a) >> 8
}

// TestTheTwoCeilingsAgree pins the invariant between two packages that do not
// import each other.
//
// Fails when: either constant is changed without the other, which would either
// waste headroom or -- the dangerous direction -- let the renderer produce an
// image the uploader refuses, turning every cover into a "failed" state with a
// message nobody can act on.
func TestTheTwoCeilingsAgree(t *testing.T) {
	if MaxBytes > spotify.MaxPlaylistCoverBytes {
		t.Fatalf("playlistcover.MaxBytes (%d) exceeds spotify.MaxPlaylistCoverBytes (%d); "+
			"the renderer would produce images the uploader refuses",
			MaxBytes, spotify.MaxPlaylistCoverBytes)
	}
}

// TestALongNameIsTruncatedRatherThanOverflowing pins that a 100-rune name --
// the validator's maximum -- still produces a cover, and that it was actually
// truncated rather than merely not crashing.
//
// The brief's original version of this test asserted only "no error and
// non-zero bytes". That cannot fail in any interesting way: an implementation
// that deleted the truncation floor and drew the full 100-rune name off the
// right edge of the canvas still returns a non-empty JPEG and no error, since
// golang.org/x/image/font clips glyphs that fall outside the destination
// image rather than erroring. Verified by removing the truncation loop in
// drawName and watching this version of the test catch it (the untruncated
// draw lights up glyph pixels to the right of the text column) while the
// original assertion kept passing.
//
// Fails when: the shrink-to-fit ladder loses its truncation floor, and a name
// that does not fit at the smallest size is drawn off the edge (pixels appear
// right of textMargin+textWidth) or panics.
func TestALongNameIsTruncatedRatherThanOverflowing(t *testing.T) {
	long := ""
	for len([]rune(long)) < 100 {
		long += "Wandering "
	}
	long = string([]rune(long)[:100])

	got, err := Render(long, "top|plays|100|0||", noTiles)
	if err != nil {
		t.Fatalf("Render with a 100-rune name: %v", err)
	}
	if len(got.JPEG) == 0 {
		t.Fatal("Render produced no bytes")
	}

	img := decodeJPEG(t, got.JPEG)
	// A properly truncated name never draws right of textMargin+textWidth.
	// White text against the darkest part of the scrim is unmistakably
	// brighter than this threshold; the scrim itself never gets this bright.
	const bright = 150
	for y := scrimTop + 20; y < Size-10; y += 10 {
		for x := textMargin + textWidth + 6; x < Size-4; x += 6 {
			r, g, b, _ := img.At(x, y).RGBA()
			if r>>8 > bright || g>>8 > bright || b>>8 > bright {
				t.Fatalf("bright pixel (%d,%d,%d) at (%d,%d), right of the text column -- "+
					"the name was not truncated to fit", r>>8, g>>8, b>>8, x, y)
			}
		}
	}
}

// textRowBrightest is the brightest r+g+b sum found anywhere in the line the
// playlist name is drawn on -- the same band drawName targets, wide enough to
// catch a glyph (or a .notdef box) wherever within it a font might place one.
func textRowBrightest(img image.Image) int {
	max := 0
	for y := textBaseline - 40; y < textBaseline+10; y++ {
		for x := textMargin; x < Size-textMargin; x++ {
			if sum := brightnessSum(img, x, y); sum > max {
				max = sum
			}
		}
	}
	return max
}

// TestEmptyNameDrawsNoText pins what an empty name renders as: the mosaic (or
// pattern) and the scrim, and nothing else -- drawName's documented no-op.
//
// Reviewed as the case most likely to regress silently: nothing previously
// committed rendered an empty name at all, so a future change to the
// `if name == "" { return nil }` short-circuit could ship broken (or start
// drawing a stray glyph for the empty string) without any test noticing.
//
// The threshold is picked from the two ends actually observed for this fixed
// seed: an empty name leaves the text row at its background brightness
// (measured 337 out of 765 here, from the pattern colour showing through the
// scrim); a name that draws even one glyph -- real text, or a .notdef box --
// pushes the brightest pixel in that row toward white (measured 761-765 for
// an emoji-only and an unrenderable-script name below). 550 sits clear of
// both.
//
// Fails when: drawName's empty-string short-circuit is removed or bypassed,
// so something is drawn on an empty name after all.
func TestEmptyNameDrawsNoText(t *testing.T) {
	got, err := Render("", "top|plays|100|0||", noTiles)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	img := decodeJPEG(t, got.JPEG)
	if b := img.Bounds(); b.Dx() != Size || b.Dy() != Size {
		t.Fatalf("cover is %dx%d, want %dx%d", b.Dx(), b.Dy(), Size, Size)
	}
	const backgroundCeiling = 550
	if max := textRowBrightest(img); max > backgroundCeiling {
		t.Errorf("brightest pixel in the text row is %d, want <= %d -- "+
			"something was drawn for an empty name", max, backgroundCeiling)
	}
}

// TestEmojiOnlyNameRendersWithoutPanicking and
// TestUnrenderableScriptNameRendersWithoutPanicking pin the two cases the
// brief named as the most likely to be untested: a name Go Bold has no
// glyphs for at all. Both are covered by the same face fallback (the
// .notdef box documented on nameFace), verified here to actually draw
// something visible rather than, say, a zero-width glyph that would make the
// fallback silently invisible -- measured at 761-765 out of 765 in the text
// row for both, well above an undrawn row's ~337 (see
// TestEmptyNameDrawsNoText).
//
// Fails when: drawName panics on a rune with no glyph, produces an
// undecodable or wrongly-sized JPEG, or the .notdef fallback turns out not to
// draw anything.
func TestEmojiOnlyNameRendersWithoutPanicking(t *testing.T) {
	got, err := Render("🎧🎶🔥", "top|plays|100|0||", noTiles)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	img := decodeJPEG(t, got.JPEG)
	if b := img.Bounds(); b.Dx() != Size || b.Dy() != Size {
		t.Fatalf("cover is %dx%d, want %dx%d", b.Dx(), b.Dy(), Size, Size)
	}
	const drawnFloor = 550
	if max := textRowBrightest(img); max <= drawnFloor {
		t.Errorf("brightest pixel in the text row is %d, want > %d -- "+
			"the .notdef fallback drew nothing visible", max, drawnFloor)
	}
}

func TestUnrenderableScriptNameRendersWithoutPanicking(t *testing.T) {
	// Devanagari: Go Bold has no glyphs for it, so every rune here falls back
	// to the face's .notdef box (see nameFace).
	got, err := Render("प्लेलिस्ट", "top|plays|100|0||", noTiles)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	img := decodeJPEG(t, got.JPEG)
	if b := img.Bounds(); b.Dx() != Size || b.Dy() != Size {
		t.Fatalf("cover is %dx%d, want %dx%d", b.Dx(), b.Dy(), Size, Size)
	}
	const drawnFloor = 550
	if max := textRowBrightest(img); max <= drawnFloor {
		t.Errorf("brightest pixel in the text row is %d, want > %d -- "+
			"the .notdef fallback drew nothing visible", max, drawnFloor)
	}
}
