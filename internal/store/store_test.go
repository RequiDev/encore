package store

import (
	"testing"
	"unicode/utf8"
)

// TestTruncateKeepsValidUTF8 pins the property every caller storing a
// caller-supplied message (a sync error, a fetch failure reason) depends on:
// cutting on a byte offset instead of a rune boundary can slice a multi-byte
// rune in half and hand Postgres bytes it rejects outright, which turns the
// write meant to record a failure into a second failure of its own.
func TestTruncateKeepsValidUTF8(t *testing.T) {
	if got := Truncate("short", 10); got != "short" {
		t.Fatalf("Truncate did not leave a short string alone: %q", got)
	}
	if got := Truncate("abcdefgh", 4); got != "abcd..." {
		t.Fatalf("Truncate(%q, 4) = %q", "abcdefgh", got)
	}
	// Cutting mid-rune would produce bytes Postgres rejects for a text column.
	got := Truncate("aé😀b", 3)
	if !utf8.ValidString(got) {
		t.Fatalf("Truncate produced invalid UTF-8: %q", got)
	}
	if got != "aé..." {
		t.Fatalf("Truncate cut on the wrong boundary: %q", got)
	}
}
