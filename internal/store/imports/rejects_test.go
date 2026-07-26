package imports

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestTruncateExcerpt(t *testing.T) {
	short := `{"ts":"2019-01-01T00:00:00Z"}`
	if got := truncateExcerpt(short); got != short {
		t.Fatalf("truncateExcerpt() shortened a short excerpt: %q", got)
	}

	long := strings.Repeat("a", MaxRawExcerptBytes+100)
	got := truncateExcerpt(long)
	if len(got) != MaxRawExcerptBytes {
		t.Fatalf("len(truncateExcerpt()) = %d, want %d", len(got), MaxRawExcerptBytes)
	}
}

// TestTruncateExcerptKeepsValidUTF8 covers the case that actually bites: a
// multi-byte rune straddling the limit. Postgres rejects invalid UTF-8 outright,
// so a naive byte slice would turn a diagnostic into a failed insert.
func TestTruncateExcerptKeepsValidUTF8(t *testing.T) {
	for pad := range 4 {
		// "é" is two bytes, so some padding length puts the cut mid-rune.
		s := strings.Repeat("x", MaxRawExcerptBytes-1+pad) + strings.Repeat("é", 8)
		got := truncateExcerpt(s)
		if !utf8.ValidString(got) {
			t.Fatalf("pad %d: truncateExcerpt() produced invalid UTF-8", pad)
		}
		if len(got) > MaxRawExcerptBytes {
			t.Fatalf("pad %d: len = %d, want at most %d", pad, len(got), MaxRawExcerptBytes)
		}
	}
}
