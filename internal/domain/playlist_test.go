package domain

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

var (
	descFrom = time.Date(2025, time.January, 1, 0, 0, 0, 0, time.UTC)
	// descTo is 1 January 2026, not 31 December 2025: TimeRange is the
	// half-open interval [From, To), matching how the only UI that builds a
	// ranged playlist actually sends it (web/src/pages/Settings.tsx builds a
	// year as From = 1 Jan of the year, To = 1 Jan of the year after). Describe
	// must print the last included instant, so the expected strings below say
	// "31 December 2025" — the day before descTo, not descTo itself.
	descTo    = time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	descBuilt = time.Date(2026, time.July, 31, 14, 22, 0, 0, time.UTC)
)

// TestDescribeCoversEveryModeAndBothRanges is the copy, asserted in full.
//
// Nothing in this project has ever been opened in a browser, and this is the
// one sentence Encore writes into somebody else's Spotify account, where it
// outlives the session that made it. A substring assertion would pass on a
// sentence with a clause missing.
//
// Fails when: any branch's wording changes; the ranking clause stops varying
// with Sort; the built-on date stops being included; the date format changes;
// the exclusive upper bound is printed as though it were included (descTo is
// one instant past the last day these strings name — that instant, not the
// day before it, is what a naive Describe would print); the MinPlays branches
// go back to claiming "every" matching track rather than "up to" the limit;
// or any singular form is removed or added where it should not be — Limit at
// 1 ("single most played track", "Up to 1 track"), MinPlays at 1 or below
// ("at least once"), and their plural counterparts, are each exercised on
// their own and crossed with each other in the MinPlays cases.
func TestDescribeCoversEveryModeAndBothRanges(t *testing.T) {
	tests := []struct {
		name string
		def  PlaylistDefinition
		want string
	}{
		{
			name: "top, a range, by plays",
			def:  PlaylistDefinition{Mode: PlaylistModeTop, Sort: SortByPlays, Limit: 100, From: descFrom, To: descTo},
			want: "Your 100 most played tracks between 1 January 2025 and 31 December 2025, " +
				"ranked by play count. Built by Encore on 31 July 2026.",
		},
		{
			name: "top, all time, by plays",
			def:  PlaylistDefinition{Mode: PlaylistModeTop, Sort: SortByPlays, Limit: 100},
			want: "Your 100 most played tracks of all time, ranked by play count. " +
				"Built by Encore on 31 July 2026.",
		},
		{
			name: "top, a single track, is not pluralised",
			def:  PlaylistDefinition{Mode: PlaylistModeTop, Sort: SortByPlays, Limit: 1, From: descFrom, To: descTo},
			want: "Your single most played track between 1 January 2025 and 31 December 2025, " +
				"ranked by play count. Built by Encore on 31 July 2026.",
		},
		{
			name: "top, a range, by listening time",
			def:  PlaylistDefinition{Mode: PlaylistModeTop, Sort: SortByTime, Limit: 100, From: descFrom, To: descTo},
			want: "Your 100 most played tracks between 1 January 2025 and 31 December 2025, " +
				"ranked by listening time. Built by Encore on 31 July 2026.",
		},
		{
			// stats/playlist.go applies LIMIT to every mode, MinPlays included, so
			// this can never honestly say "every track": at most 500 (the widest
			// Limit here) of however many tracks clear the threshold.
			name: "a minimum play count, over a range, capped at the limit",
			def:  PlaylistDefinition{Mode: PlaylistModeMinPlays, Sort: SortByPlays, Limit: 500, MinPlays: 10, From: descFrom, To: descTo},
			want: "Up to 500 tracks you played at least 10 times between 1 January 2025 and " +
				"31 December 2025, ranked by play count. Built by Encore on 31 July 2026.",
		},
		{
			name: "a minimum play count, all time, capped at the limit",
			def:  PlaylistDefinition{Mode: PlaylistModeMinPlays, Sort: SortByPlays, Limit: 500, MinPlays: 10},
			want: "Up to 500 tracks you have ever played at least 10 times, ranked by play count. " +
				"Built by Encore on 31 July 2026.",
		},
		{
			name: "a minimum of one is not 1 times",
			def:  PlaylistDefinition{Mode: PlaylistModeMinPlays, Sort: SortByPlays, Limit: 500, MinPlays: 1, From: descFrom, To: descTo},
			want: "Up to 500 tracks you played at least once between 1 January 2025 and " +
				"31 December 2025, ranked by play count. Built by Encore on 31 July 2026.",
		},
		{
			name: "a minimum of one, all time",
			def:  PlaylistDefinition{Mode: PlaylistModeMinPlays, Sort: SortByPlays, Limit: 500, MinPlays: 1},
			want: "Up to 500 tracks you have ever played at least once, ranked by play count. " +
				"Built by Encore on 31 July 2026.",
		},
		{
			// Crosses the other singular: a limit of 1 alongside a plural
			// MinPlays, so "Up to 1 track" is checked independently of
			// "at least once".
			name: "a minimum play count, but only a single track allowed",
			def:  PlaylistDefinition{Mode: PlaylistModeMinPlays, Sort: SortByPlays, Limit: 1, MinPlays: 10, From: descFrom, To: descTo},
			want: "Up to 1 track you played at least 10 times between 1 January 2025 and " +
				"31 December 2025, ranked by play count. Built by Encore on 31 July 2026.",
		},
		{
			// Both singular forms at once: Limit 1 and MinPlays 1, all time.
			name: "a single track, at a minimum of one, all time",
			def:  PlaylistDefinition{Mode: PlaylistModeMinPlays, Sort: SortByPlays, Limit: 1, MinPlays: 1},
			want: "Up to 1 track you have ever played at least once, ranked by play count. " +
				"Built by Encore on 31 July 2026.",
		},
		{
			// MinPlays: 0 cannot come from Validate (it requires at least 1), but
			// migrations/00009_playlists.sql's check allows it, and a rebuild
			// loads the stored definition without re-validating it. The query's
			// HAVING count(*) >= n selects the same tracks for n <= 1 as for
			// n == 1 — a track cannot be grouped at all without one listen — so
			// flooring to "at least once" is the accurate description, not just
			// a safe-looking default.
			name: "a minimum play count of zero floors to the singular",
			def:  PlaylistDefinition{Mode: PlaylistModeMinPlays, Sort: SortByPlays, Limit: 100, MinPlays: 0, From: descFrom, To: descTo},
			want: "Up to 100 tracks you played at least once between 1 January 2025 and " +
				"31 December 2025, ranked by play count. Built by Encore on 31 July 2026.",
		},
		{
			name: "discoveries, over a range",
			def:  PlaylistDefinition{Mode: PlaylistModeDiscoveries, Sort: SortByPlays, Limit: 100, From: descFrom, To: descTo},
			want: "Tracks you heard for the first time between 1 January 2025 and " +
				"31 December 2025, ranked by play count. Built by Encore on 31 July 2026.",
		},
		{
			name: "discoveries, all time",
			def:  PlaylistDefinition{Mode: PlaylistModeDiscoveries, Sort: SortByPlays, Limit: 100},
			want: "Tracks you heard for the first time, across your whole history, " +
				"ranked by play count. Built by Encore on 31 July 2026.",
		},
		{
			name: "forgotten favourites",
			def:  PlaylistDefinition{Mode: PlaylistModeForgotten, Sort: SortByPlays, Limit: 100, From: descFrom, To: descTo},
			want: "Tracks you played heavily before 1 January 2025 and not once between " +
				"1 January 2025 and 31 December 2025, ranked by play count. " +
				"Built by Encore on 31 July 2026.",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.def.Describe(descBuilt); got != tc.want {
				t.Errorf("Describe()\n got: %q\nwant: %q", got, tc.want)
			}
		})
	}
}

// TestDescribeStaysUnderSpotifysCeiling pins the bound on the one string that
// leaves this process for somebody else's account.
//
// Spotify caps a description at 300 characters and rejects a longer one, so
// the widest definition the validator will accept must still fit. Measured
// across every mode at maximum field sizes, forgotten (ranged, by listening
// time, the longest ranking clause, over the longest month names, mentioning
// the range's edges three times rather than the two every other ranged mode
// uses) is the widest at 175 characters — 125 characters of headroom below
// the ceiling.
//
// That headroom means this test is a backstop against a gross structural
// mistake — a clause duplicated, an extra sentence appended, a loop that
// repeats a fragment — not against the kind of defect that actually ships
// here: a wrong word, a dropped plural, a swapped verb, or a date format that
// merely grows (measured: adding a full weekday name to every date pushes the
// widest case to 212 characters, still comfortably under 300). Those are
// TestDescribeCoversEveryModeAndBothRanges's job, via full-string equality.
//
// Fails when: a clause of more than 125 characters is added to any branch
// without checking this (verified: appending ~143 characters to the forgotten
// branch pushes it to 318 and trips it; the 300-character format-growth
// mutation above stays well clear at 212).
func TestDescribeStaysUnderSpotifysCeiling(t *testing.T) {
	widest := PlaylistDefinition{
		Mode: PlaylistModeForgotten, Sort: SortByTime, Limit: PlaylistMaxTracks,
		MinPlays: PlaylistMaxMinPlays,
		From:     time.Date(2025, time.September, 30, 0, 0, 0, 0, time.UTC),
		// One day past 28 December: To is exclusive, so the printed end date is
		// the day before it. See descTo above for the same reasoning.
		To: time.Date(2025, time.December, 29, 0, 0, 0, 0, time.UTC),
	}
	got := widest.Describe(time.Date(2026, time.September, 30, 0, 0, 0, 0, time.UTC))
	if n := utf8.RuneCountInString(got); n > 300 {
		t.Fatalf("description is %d characters, over Spotify's 300: %q", n, got)
	}
	if strings.Contains(got, "  ") {
		t.Errorf("description has a doubled space: %q", got)
	}
}
