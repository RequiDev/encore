package httpapi

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/RequiDev/encore/internal/albumtracks"
	"github.com/RequiDev/encore/internal/artistalbums"
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

// TestArtistDiscographyStatesSerialiseDistinctly is the mechanical guard on the
// four-way distinction the whole feature rests on: a client that cannot tell
// "disabled" from "unavailable" blames Spotify for a local decision.
func TestArtistDiscographyStatesSerialiseDistinctly(t *testing.T) {
	states := []artistalbums.State{
		artistalbums.StateReady, artistalbums.StatePending,
		artistalbums.StateUnavailable, artistalbums.StateDisabled,
	}
	seen := make(map[string]artistalbums.State, len(states))
	for _, st := range states {
		out := toArtistDiscography(artistalbums.Discography{State: st}, nil)
		if out.State != string(st) {
			t.Fatalf("artistalbums.State %q serialised as %q", st, out.State)
		}
		if prev, dup := seen[out.State]; dup {
			t.Fatalf("states %q and %q both serialise as %q", prev, st, out.State)
		}
		seen[out.State] = st
	}
}

// TestTheLazyFetchStatesKeepTheirWireValues pins the four strings both
// endpoints put on the wire.
//
// After Task 4 the two services alias one set of constants, so they cannot fork
// from each other — an earlier draft of this plan had a test comparing them
// pair by pair, and against aliases that test cannot fail, which makes it worse
// than no test. What *can* still break is the value itself: editing
// lazyfetch.OutcomeReady to "done" compiles, passes every distinctness check,
// and silently breaks every deployed client at once, because these strings are
// a published contract (docs/api.md names all four). So this asserts the
// literals.
func TestTheLazyFetchStatesKeepTheirWireValues(t *testing.T) {
	for want, got := range map[string]string{
		"ready":       string(artistalbums.StateReady),
		"pending":     string(artistalbums.StatePending),
		"unavailable": string(artistalbums.StateUnavailable),
		"disabled":    string(artistalbums.StateDisabled),
	} {
		if got != want {
			t.Errorf("a state serialises as %q, want %q: these four strings are published in "+
				"docs/api.md and branched on by every client", got, want)
		}
	}
	// And the album endpoint puts the same four on the wire, which is true by
	// construction now that both alias lazyfetch.Outcome — asserted anyway,
	// because "by construction" lasts exactly until somebody re-declares one.
	if string(albumtracks.StateReady) != string(artistalbums.StateReady) ||
		string(albumtracks.StatePending) != string(artistalbums.StatePending) ||
		string(albumtracks.StateUnavailable) != string(artistalbums.StateUnavailable) ||
		string(albumtracks.StateDisabled) != string(artistalbums.StateDisabled) {
		t.Error("the two endpoints' state vocabularies have forked; one of them has stopped " +
			"aliasing lazyfetch.Outcome and a client branching on one is now wrong about the other")
	}
}

// discography builds a stored, ready listing with every group represented, so
// the derivation below has something to filter.
func discographyFixture() artistalbums.Discography {
	day := func(y int) *time.Time {
		at := time.Date(y, time.January, 1, 0, 0, 0, 0, time.UTC)
		return &at
	}
	return artistalbums.Discography{
		State:     artistalbums.StateReady,
		FetchedAt: time.Date(2026, time.July, 20, 9, 0, 0, 0, time.UTC),
		Releases: []artistalbums.Release{
			{AlbumID: "alb-3", Name: "Third", Group: "album", ReleaseDate: day(2022), ReleasePrecision: "year"},
			{AlbumID: "alb-2", Name: "Second", Group: "album", ReleaseDate: day(2019), ReleasePrecision: "year"},
			{AlbumID: "alb-1", Name: "First", Group: "album", ReleaseDate: day(2016), ReleasePrecision: "year"},
			{AlbumID: "sng-1", Name: "A Single", Group: "single"},
			{AlbumID: "sng-2", Name: "Another Single", Group: "single"},
			{AlbumID: "cmp-1", Name: "Best Of", Group: "compilation"},
			{AlbumID: "app-1", Name: "Somebody Else's Record", Group: "appears_on"},
			{AlbumID: "epx-1", Name: "An EP", Group: "ep"},
		},
	}
}

// TestArtistDiscographyCountsOnlyAlbums is the arithmetic §5.2 asks for: the
// denominator is album_group 'album' and nothing else, because "you have heard 4
// of 340 releases" is not a useful sentence.
func TestArtistDiscographyCountsOnlyAlbums(t *testing.T) {
	out := toArtistDiscography(discographyFixture(), []string{"alb-2", "sng-1"})

	if out.Coverage.Total != 3 {
		t.Fatalf("coverage.total = %d, want 3: only album_group 'album' counts", out.Coverage.Total)
	}
	// The played single is not counted, and it is not "missing" either: it is not
	// in the population at all.
	if out.Coverage.Covered != 1 {
		t.Fatalf("coverage.covered = %d, want 1", out.Coverage.Covered)
	}
	if len(out.Missing) != 2 {
		t.Fatalf("missing has %d entries, want 2", len(out.Missing))
	}
	for _, m := range out.Missing {
		if m.ID == "sng-1" || m.ID == "cmp-1" || m.ID == "app-1" || m.ID == "epx-1" {
			t.Fatalf("missing names %q, which is not an album and is not in the denominator", m.ID)
		}
	}
	// Newest first, as stored.
	if out.Missing[0].ID != "alb-3" || out.Missing[1].ID != "alb-1" {
		t.Fatalf("missing = %v, want the stored order preserved", out.Missing)
	}
	if out.Missing[0].ReleaseDate == nil || *out.Missing[0].ReleaseDate != "2022" {
		t.Fatalf("release date = %v, want the year-precision \"2022\"", out.Missing[0].ReleaseDate)
	}
}

// TestArtistDiscographyNamesWhatItExcluded is the copy problem's whole
// mechanism. "4 of 11 albums" over 340 unmentioned releases is an overclaim by
// omission, and the page can only say otherwise if these numbers travel with the
// response.
func TestArtistDiscographyNamesWhatItExcluded(t *testing.T) {
	out := toArtistDiscography(discographyFixture(), nil)

	want := DiscographyExcluded{Singles: 2, Compilations: 1, AppearsOn: 1, Other: 1}
	if out.Excluded != want {
		t.Fatalf("excluded = %+v, want %+v", out.Excluded, want)
	}
}

// TestArtistDiscographyExclusionsAccountForEveryRelease pins the property that
// makes the excluded breakdown trustworthy rather than decorative: the four
// buckets plus the counted albums equal the number of releases stored. Without
// `Other`, a group Spotify adds later would vanish from both the numerator and
// the breakdown, and the page's "Spotify also lists…" sentence would quietly
// undercount.
func TestArtistDiscographyExclusionsAccountForEveryRelease(t *testing.T) {
	d := discographyFixture()
	out := toArtistDiscography(d, []string{"alb-1"})

	sum := out.Coverage.Total + out.Excluded.Singles + out.Excluded.Compilations +
		out.Excluded.AppearsOn + out.Excluded.Other
	if sum != int64(len(d.Releases)) {
		t.Fatalf("counted %d + excluded = %d, want the %d releases stored; a release is in neither "+
			"bucket, so the page's breakdown undercounts", out.Coverage.Total, sum, len(d.Releases))
	}
	if out.Coverage.Covered+int64(len(out.Missing)) != out.Coverage.Total {
		t.Fatalf("covered %d + missing %d != total %d; every counted album must be exactly one of the two",
			out.Coverage.Covered, len(out.Missing), out.Coverage.Total)
	}
}

// TestArtistDiscographyMissingIsNeverNull keeps a client from needing a guard,
// and the assertion is made against a Go value rather than against decoded JSON
// on purpose: encoding/json decodes `[]` to a non-nil slice, so a test that
// round-tripped through the wire would pass against a nil field and prove
// nothing.
func TestArtistDiscographyMissingIsNeverNull(t *testing.T) {
	for _, st := range []artistalbums.State{
		artistalbums.StatePending, artistalbums.StateUnavailable, artistalbums.StateDisabled,
	} {
		out := toArtistDiscography(artistalbums.Discography{State: st}, nil)
		if out.Missing == nil {
			t.Errorf("missing is nil on state %q; a client iterating it needs a guard it should not need", st)
		}
	}
}

// TestArtistDiscographyAllSinglesIsReadyWithNothingCounted is the state with no
// counterpart on the album endpoint. It must not look like a failure, and it
// must not look like "you have played everything" either — the page tells those
// apart by coverage.total being zero on a ready listing.
func TestArtistDiscographyAllSinglesIsReadyWithNothingCounted(t *testing.T) {
	out := toArtistDiscography(artistalbums.Discography{
		State:     artistalbums.StateReady,
		FetchedAt: time.Date(2026, time.July, 20, 9, 0, 0, 0, time.UTC),
		Releases: []artistalbums.Release{
			{AlbumID: "sng-1", Name: "One", Group: "single"},
			{AlbumID: "sng-2", Name: "Two", Group: "single"},
		},
	}, nil)

	if out.State != "ready" {
		t.Fatalf("state = %q, want \"ready\": an artist who has only released singles was read "+
			"successfully", out.State)
	}
	if out.Coverage.Total != 0 || len(out.Missing) != 0 {
		t.Fatalf("coverage = %+v with %d missing, want nothing counted", out.Coverage, len(out.Missing))
	}
	if out.Excluded.Singles != 2 {
		t.Fatalf("excluded.singles = %d, want 2: the page has nothing else to describe the artist with",
			out.Excluded.Singles)
	}
	if out.FetchedAt == nil {
		t.Fatal("fetchedAt is absent on a ready listing")
	}
}
