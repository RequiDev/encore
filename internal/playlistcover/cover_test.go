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

// TestFourPhotographMosaicFitsUnderTheCeiling pins the size guarantee and
// proves the quality ladder is doing work.
//
// The second assertion is what makes this test able to fail. Without it, an
// implementation that encoded once at quality 90 and returned whatever came
// out would pass on any input that happened to be small, and the ladder could
// be deleted entirely without anything going red.
//
// Fails when: the ceiling is raised to the base64 limit (256 KB) instead of the
// binary one; the ladder is reduced to a single quality; or the images are
// downscaled before encoding, which would make the first attempt fit and the
// attempt count drop to 1.
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
// resolution of that ambiguity.
//
// Fails when: Render falls back to the pattern whenever any tile is nil, which
// is the other reading of the spec and the one that discards artwork Encore
// already fetched.
func TestOneLostTileStillYieldsAMosaic(t *testing.T) {
	tiles := [Tiles]image.Image{noisyTile(1), nil, noisyTile(3), noisyTile(4)}

	got, err := Render("Heavy rotation", "top|plays|100|0||", tiles)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if got.Kind != KindMosaic {
		t.Errorf("Kind = %q, want %q", got.Kind, KindMosaic)
	}
	if got.Covered != 3 {
		t.Errorf("Covered = %d, want 3", got.Covered)
	}
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

	img, err := jpeg.Decode(bytes.NewReader(got.JPEG))
	if err != nil {
		t.Fatalf("decode cover jpeg: %v", err)
	}
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
