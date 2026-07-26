package stats

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/requi/encore/internal/domain"
)

func TestCursorRoundTrip(t *testing.T) {
	// Microseconds are the resolution Postgres stores, so the cursor has to
	// survive exactly that much precision: a rounded cursor would re-deliver or
	// skip the row it points at.
	original := Cursor{
		PlayedAt: time.Date(2026, time.July, 26, 21, 14, 5, 123456000, time.UTC),
		ID:       9007199254740993,
	}

	token := original.Encode()
	if token == "" {
		t.Fatal("a populated cursor must encode to a token")
	}
	if strings.ContainsAny(token, "+/=") {
		t.Errorf("token %q is not URL-safe", token)
	}

	got, err := DecodeCursor(token)
	if err != nil {
		t.Fatalf("decoding a token we produced failed: %v", err)
	}
	if !got.PlayedAt.Equal(original.PlayedAt) {
		t.Errorf("played_at round-tripped as %s, want %s", got.PlayedAt, original.PlayedAt)
	}
	if got.ID != original.ID {
		t.Errorf("id round-tripped as %d, want %d", got.ID, original.ID)
	}
}

func TestCursorRoundTripKeepsTheInstantNotTheZone(t *testing.T) {
	loc := time.FixedZone("Test", 5*60*60)
	original := Cursor{PlayedAt: time.Date(2026, time.July, 26, 3, 0, 0, 0, loc), ID: 42}

	got, err := DecodeCursor(original.Encode())
	if err != nil {
		t.Fatalf("decode failed: %v", err)
	}
	if !got.PlayedAt.Equal(original.PlayedAt) {
		t.Errorf("instant changed: %s, want %s", got.PlayedAt, original.PlayedAt)
	}
	if got.PlayedAt.Location() != time.UTC {
		t.Errorf("decoded cursor should be UTC, got %s", got.PlayedAt.Location())
	}
}

func TestEmptyCursorIsTheFirstPage(t *testing.T) {
	got, err := DecodeCursor("")
	if err != nil {
		t.Fatalf("an absent cursor is not an error: %v", err)
	}
	if !got.IsZero() {
		t.Errorf("an absent cursor must be the zero cursor, got %+v", got)
	}
	if (Cursor{}).Encode() != "" {
		t.Error("the zero cursor must encode to an empty token")
	}
}

func TestDecodeCursorRejectsGarbage(t *testing.T) {
	for _, tc := range []struct {
		name  string
		token string
	}{
		{"not base64", "!!!!"},
		{"padded base64", base64.StdEncoding.EncodeToString([]byte("v1:1:1")) + "=="},
		{"wrong shape", base64.RawURLEncoding.EncodeToString([]byte("1758000000000000:5"))},
		{"wrong version", base64.RawURLEncoding.EncodeToString([]byte("v2:1758000000000000:5"))},
		{"unparsable timestamp", base64.RawURLEncoding.EncodeToString([]byte("v1:soon:5"))},
		{"unparsable id", base64.RawURLEncoding.EncodeToString([]byte("v1:1758000000000000:last"))},
		{"zero id", base64.RawURLEncoding.EncodeToString([]byte("v1:1758000000000000:0"))},
		{"negative id", base64.RawURLEncoding.EncodeToString([]byte("v1:1758000000000000:-3"))},
	} {
		got, err := DecodeCursor(tc.token)
		if err == nil {
			t.Errorf("%s: expected a rejection, got %+v", tc.name, got)
			continue
		}
		if !errors.Is(err, domain.ErrValidation) {
			t.Errorf("%s: expected a validation error, got %v", tc.name, err)
		}
		if strings.Contains(err.Error(), tc.token) {
			t.Errorf("%s: the error echoes the token back", tc.name)
		}
	}
}

// TestHistoryKeysetPredicate pins the shape the pagination depends on: a row
// comparison against the last delivered row, ordered the same way, and never an
// OFFSET.
func TestHistoryKeysetPredicate(t *testing.T) {
	if !strings.Contains(historyNextPageSQL, "(l.played_at, l.id) < ($4::timestamptz, $5::bigint)") {
		t.Errorf("the keyset predicate is missing:\n%s", historyNextPageSQL)
	}
	for _, sql := range []string{historyFirstPageSQL, historyNextPageSQL} {
		if !strings.Contains(sql, "ORDER BY l.played_at DESC, l.id DESC") {
			t.Errorf("the feed must be ordered by the keyset:\n%s", sql)
		}
		if strings.Contains(sql, "OFFSET") {
			t.Errorf("the feed must never use OFFSET:\n%s", sql)
		}
	}
}
