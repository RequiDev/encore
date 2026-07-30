package httpapi

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/RequiDev/encore/internal/albumtracks"
)

// TestAlbumTrackListStatesSerialiseDistinctly pins the property the whole
// four-state design rests on: every state produces its own wire string, and
// none can be mistaken for another. A client that read "unavailable" for what
// was really "pending" would give up polling an album that was still coming; a
// client that read "unavailable" for "disabled" would blame Spotify for an
// operator's own switch. Both are the exact failure this test exists to catch.
func TestAlbumTrackListStatesSerialiseDistinctly(t *testing.T) {
	states := []albumtracks.State{
		albumtracks.StateReady, albumtracks.StatePending,
		albumtracks.StateUnavailable, albumtracks.StateDisabled,
	}
	seen := make(map[string]albumtracks.State, len(states))
	for _, st := range states {
		out := toAlbumTrackList(albumtracks.Listing{State: st}, nil)
		if out.State != string(st) {
			t.Fatalf("albumtracks.State %q serialised as %q", st, out.State)
		}
		if other, ok := seen[out.State]; ok && other != st {
			t.Fatalf("states %q and %q both serialised to the wire string %q", other, st, out.State)
		}
		seen[out.State] = st
	}
	if len(seen) != len(states) {
		t.Fatalf("only %d distinct wire strings for %d states: %v", len(seen), len(states), seen)
	}
}

// TestToAlbumTrackListDiffsAgainstHeard pins the arithmetic a page actually
// renders: the denominator is the listing's own length, Covered counts only
// the tracks that are both listed and heard, and Missing carries the rest in
// the order the listing gave them — never re-sorted, never re-derived from
// album.total_tracks, which this function never even sees.
func TestToAlbumTrackListDiffsAgainstHeard(t *testing.T) {
	fetchedAt := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	listing := albumtracks.Listing{
		State: albumtracks.StateReady,
		Tracks: []albumtracks.Track{
			{ID: "t1", Name: "One", DiscNumber: 1, TrackNumber: 1},
			{ID: "t2", Name: "Two", DiscNumber: 1, TrackNumber: 2},
			{ID: "t3", Name: "Three", DiscNumber: 1, TrackNumber: 3},
		},
		FetchedAt: fetchedAt,
	}
	// t2 heard twice over (a duplicate id in the heard set, which
	// stats.AlbumHeardTracks never produces since it is DISTINCT, but the diff
	// must not double count even if it did).
	out := toAlbumTrackList(listing, []string{"t2", "t2"})

	if out.Coverage.Total != 3 {
		t.Fatalf("coverage.total = %d, want 3 (the listing's own length)", out.Coverage.Total)
	}
	if out.Coverage.Covered != 1 {
		t.Fatalf("coverage.covered = %d, want 1", out.Coverage.Covered)
	}
	if len(out.Missing) != 2 {
		t.Fatalf("missing has %d entries, want 2", len(out.Missing))
	}
	if out.Missing[0].ID != "t1" || out.Missing[1].ID != "t3" {
		t.Fatalf("missing = %+v, want t1 then t3 in listing order", out.Missing)
	}
	if out.FetchedAt == nil || !out.FetchedAt.Equal(fetchedAt) {
		t.Fatalf("fetchedAt = %v, want %v", out.FetchedAt, fetchedAt)
	}
}

// TestToAlbumTrackListEmptyStatesNeverNilMissing pins the other half of the
// four-state contract: pending, unavailable and disabled all carry no
// listing, and Missing must still serialise as [] rather than null so a
// client can range over it unconditionally. A test that only checked
// encoding/json's own decode of "[]" back into a non-nil slice would pass
// whether or not the handler ever produced a nil slice in the first place —
// this asserts the Go value directly, and the wire text besides.
func TestToAlbumTrackListEmptyStatesNeverNilMissing(t *testing.T) {
	for _, st := range []albumtracks.State{
		albumtracks.StatePending, albumtracks.StateUnavailable, albumtracks.StateDisabled,
	} {
		out := toAlbumTrackList(albumtracks.Listing{State: st}, nil)
		if out.Missing == nil {
			t.Fatalf("state %q: Missing is nil, not an empty slice", st)
		}
		if len(out.Missing) != 0 {
			t.Fatalf("state %q: Missing has %d entries with no listing", st, len(out.Missing))
		}
		if out.Coverage.Total != 0 || out.Coverage.Covered != 0 {
			t.Fatalf("state %q: coverage = %+v, want zero on both sides with no listing", st, out.Coverage)
		}
		if out.FetchedAt != nil {
			t.Fatalf("state %q: fetchedAt = %v, want nil: nothing has ever been fetched", st, *out.FetchedAt)
		}

		raw, err := json.Marshal(out)
		if err != nil {
			t.Fatalf("state %q: marshal: %v", st, err)
		}
		if !strings.Contains(string(raw), `"missing":[]`) {
			t.Fatalf("state %q: wire form does not contain \"missing\":[], got %s", st, raw)
		}
		if strings.Contains(string(raw), `"fetchedAt"`) {
			t.Fatalf("state %q: fetchedAt was emitted with no listing ever fetched, got %s", st, raw)
		}
	}
}
