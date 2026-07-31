package playlistcover

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"testing"
)

// noTiles is the fresh-instance case: a catalogue that has not enriched yet
// has no artwork at all.
var noTiles [Tiles]image.Image

// TestPatternCoverIsDeterministic pins that the same definition always
// produces the same picture.
//
// This is two assertions and it needs both. Byte-identity alone would pass for
// a function that returned a constant image, so the second half proves the
// seed is actually an input. Together they pin exactly what the feature
// promises: same definition, same cover, for ever — a cover that changed on
// each rebuild would make a playlist look as though it had been tampered with.
//
// Fails when: the seed stops reaching the pattern (the two definitions then
// render identically and the difference assertion fires); or any
// non-deterministic input creeps in — time.Now, an unseeded rand, or map
// iteration order (the repeat assertion then fires).
func TestPatternCoverIsDeterministic(t *testing.T) {
	const name = "Heavy rotation"

	first, err := Render(name, "top|plays|100|0||", noTiles)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	again, err := Render(name, "top|plays|100|0||", noTiles)
	if err != nil {
		t.Fatalf("Render (second call): %v", err)
	}
	if !bytes.Equal(first.JPEG, again.JPEG) {
		t.Fatalf("the same seed produced two different images (%d and %d bytes)",
			len(first.JPEG), len(again.JPEG))
	}

	other, err := Render(name, "discoveries|time|50|0|2025-01-01|2025-12-31", noTiles)
	if err != nil {
		t.Fatalf("Render (other definition): %v", err)
	}
	if bytes.Equal(first.JPEG, other.JPEG) {
		t.Fatal("two different definitions produced the same image; the seed is not an input")
	}
}

// TestPatternCoverReportsItselfHonestly pins that a cover with no artwork in
// it says so, so the interface can say so too.
//
// Fails when: Kind is hardcoded to mosaic, or Covered is derived from
// len(tiles) rather than from how many were non-nil.
func TestPatternCoverReportsItselfHonestly(t *testing.T) {
	got, err := Render("Heavy rotation", "top|plays|100|0||", noTiles)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if got.Kind != KindPattern {
		t.Errorf("Kind = %q, want %q", got.Kind, KindPattern)
	}
	if got.Covered != 0 {
		t.Errorf("Covered = %d, want 0", got.Covered)
	}
}

// TestRenamingDoesNotReshuffleThePattern pins that the name is drawn on top of
// the pattern rather than being part of its seed.
//
// A rename must change the words on the cover and nothing else; folding the
// name into the seed would make every rename produce an unrecognisably
// different picture, which is the opposite of what a rename is for.
//
// The brief's original version of this test asserted only that a and b (two
// direct patternFor calls with an identical literal seed) matched, and that
// the two rendered JPEGs differed. Neither actually pins the claim: a and b
// never involve the name, so they pass regardless of whether Render folds the
// name into the seed; and two different names produce two different JPEGs
// either way, because the *text* differs, whether or not the background does
// too. Verified by concatenating name into the seed inside Render and
// confirming both original assertions still passed. The corner-pixel check
// below is the one that actually distinguishes the two: it inspects a patch
// that the scrim and the text never touch, so it can only differ if the
// picture underneath changed.
//
// Fails when: name is concatenated into the seed before hashing — the corner
// pixel then differs between the two names.
func TestRenamingDoesNotReshuffleThePattern(t *testing.T) {
	a := patternFor("top|plays|100|0||")
	b := patternFor("top|plays|100|0||")
	if !bytes.Equal(a.Pix, b.Pix) {
		t.Fatal("patternFor is not deterministic")
	}
	// The rendered covers differ because the words differ...
	one, err := Render("Heavy rotation", "top|plays|100|0||", noTiles)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	two, err := Render("Light rotation", "top|plays|100|0||", noTiles)
	if err != nil {
		t.Fatalf("Render (other name): %v", err)
	}
	if bytes.Equal(one.JPEG, two.JPEG) {
		t.Fatal("two names produced the same cover; the name is not being drawn")
	}
	// ...but the pattern underneath does not. (10, 10) sits in the top-left
	// tile, comfortably above the scrim (which starts at y = 2*Size/3) and to
	// the left of the text (which starts at x = textMargin, near the bottom),
	// so it reflects only the pattern, never the name.
	onePx := decodeCorner(t, one.JPEG)
	twoPx := decodeCorner(t, two.JPEG)
	if onePx != twoPx {
		t.Fatalf("pattern pixel at (10,10) is %v for one name and %v for the other; "+
			"the name is reaching the seed", onePx, twoPx)
	}
}

// decodeCorner decodes a JPEG cover and returns the pixel at (10, 10), a point
// neither the scrim nor the name ever draws over.
func decodeCorner(t *testing.T, jpg []byte) color.RGBA {
	t.Helper()
	img, err := jpeg.Decode(bytes.NewReader(jpg))
	if err != nil {
		t.Fatalf("decode cover jpeg: %v", err)
	}
	r, g, b, a := img.At(10, 10).RGBA()
	return color.RGBA{R: uint8(r >> 8), G: uint8(g >> 8), B: uint8(b >> 8), A: uint8(a >> 8)}
}
