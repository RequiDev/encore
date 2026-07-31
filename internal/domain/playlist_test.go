package domain

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

var (
	descFrom  = time.Date(2025, time.January, 1, 0, 0, 0, 0, time.UTC)
	descTo    = time.Date(2025, time.December, 31, 0, 0, 0, 0, time.UTC)
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
// with Sort; the built-on date stops being included; the singular forms are
// removed (cases 3, 7 and 8 then read "1 most played tracks" and "at least 1
// times"); or the date format changes.
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
			name: "a minimum play count, over a range",
			def:  PlaylistDefinition{Mode: PlaylistModeMinPlays, Sort: SortByPlays, Limit: 500, MinPlays: 10, From: descFrom, To: descTo},
			want: "Every track you played at least 10 times between 1 January 2025 and " +
				"31 December 2025, ranked by play count. Built by Encore on 31 July 2026.",
		},
		{
			name: "a minimum play count, all time",
			def:  PlaylistDefinition{Mode: PlaylistModeMinPlays, Sort: SortByPlays, Limit: 500, MinPlays: 10},
			want: "Every track you have ever played at least 10 times, ranked by play count. " +
				"Built by Encore on 31 July 2026.",
		},
		{
			name: "a minimum of one is not 1 times",
			def:  PlaylistDefinition{Mode: PlaylistModeMinPlays, Sort: SortByPlays, Limit: 500, MinPlays: 1, From: descFrom, To: descTo},
			want: "Every track you played at least once between 1 January 2025 and " +
				"31 December 2025, ranked by play count. Built by Encore on 31 July 2026.",
		},
		{
			name: "a minimum of one, all time",
			def:  PlaylistDefinition{Mode: PlaylistModeMinPlays, Sort: SortByPlays, Limit: 500, MinPlays: 1},
			want: "Every track you have ever played at least once, ranked by play count. " +
				"Built by Encore on 31 July 2026.",
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
// the widest definition the validator will accept must still fit. The widest
// is forgotten (two dates plus a third), at the maximum limit and minimum
// count, over the longest month names.
//
// Fails when: a clause is added to any branch without checking this, or the
// date format grows (a full weekday name would add ~10 characters per date and
// there are three of them in the forgotten branch).
func TestDescribeStaysUnderSpotifysCeiling(t *testing.T) {
	widest := PlaylistDefinition{
		Mode: PlaylistModeForgotten, Sort: SortByTime, Limit: PlaylistMaxTracks,
		MinPlays: PlaylistMaxMinPlays,
		From:     time.Date(2025, time.September, 30, 0, 0, 0, 0, time.UTC),
		To:       time.Date(2025, time.December, 28, 0, 0, 0, 0, time.UTC),
	}
	got := widest.Describe(time.Date(2026, time.September, 30, 0, 0, 0, 0, time.UTC))
	if n := utf8.RuneCountInString(got); n > 300 {
		t.Fatalf("description is %d characters, over Spotify's 300: %q", n, got)
	}
	if strings.Contains(got, "  ") {
		t.Errorf("description has a doubled space: %q", got)
	}
}
