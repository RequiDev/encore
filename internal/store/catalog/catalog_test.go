package catalog

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/RequiDev/encore/internal/domain"
)

func TestKindTable(t *testing.T) {
	cases := map[Kind]string{
		KindTrack:  "tracks",
		KindAlbum:  "albums",
		KindArtist: "artists",
	}
	for kind, want := range cases {
		got, err := kind.table()
		if err != nil {
			t.Fatalf("table(%q): unexpected error: %v", kind, err)
		}
		if got != want {
			t.Errorf("table(%q) = %q, want %q", kind, got, want)
		}
		if !kind.Valid() {
			t.Errorf("Valid(%q) = false, want true", kind)
		}
		if kind.String() != string(kind) {
			t.Errorf("String(%q) = %q", kind, kind.String())
		}
	}
}

// An unknown kind must never reach a statement: the table name is interpolated,
// so the closed switch is the whole of the injection defence.
func TestKindTableRejectsUnknown(t *testing.T) {
	for _, kind := range []Kind{"", "playlist", "tracks; DROP TABLE listens"} {
		if kind.Valid() {
			t.Errorf("Valid(%q) = true, want false", kind)
		}
		got, err := kind.table()
		if err == nil {
			t.Fatalf("table(%q) = %q, want an error", kind, got)
		}
		if !errors.Is(err, domain.ErrValidation) {
			t.Errorf("table(%q) error = %v, want domain.ErrValidation", kind, err)
		}
		if got != "" {
			t.Errorf("table(%q) = %q, want empty", kind, got)
		}
	}
}

// The claim statement is assembled per table; check the pieces that make it safe
// under concurrency survive formatting.
func TestClaimPendingSQLShape(t *testing.T) {
	sql := fmt.Sprintf(claimPendingSQL, "tracks")
	for _, want := range []string{
		"UPDATE tracks AS x",
		"FROM tracks AS s",
		"FOR UPDATE SKIP LOCKED",
		"ORDER BY s.next_attempt_at NULLS FIRST, s.id",
		"metadata_state IN ('pending', 'failed')",
		"RETURNING x.id",
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("claim statement is missing %q:\n%s", want, sql)
		}
	}
	if strings.Contains(sql, "%!") {
		t.Errorf("claim statement has an unsatisfied verb:\n%s", sql)
	}
}

func TestMarkStatementsGuardState(t *testing.T) {
	// A stale failure report must not disturb a row another worker resolved.
	for _, sql := range []string{
		fmt.Sprintf(markUnavailableSQL, "albums"),
		fmt.Sprintf(markFetchFailedSQL, "albums"),
	} {
		if !strings.Contains(sql, "metadata_state IN ('pending', 'failed')") {
			t.Errorf("mark statement is missing its state guard:\n%s", sql)
		}
	}
	if !strings.Contains(markAliasFailedSQL, "state IN ('pending', 'failed')") {
		t.Errorf("alias failure statement is missing its state guard:\n%s", markAliasFailedSQL)
	}
	if !strings.Contains(markAliasUnavailableSQL, "state IN ('pending', 'failed')") {
		t.Errorf("alias unavailable statement is missing its state guard:\n%s", markAliasUnavailableSQL)
	}
}

func TestDedupeIDs(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{"nil", nil, nil},
		{"empties only", []string{"", ""}, []string{}},
		{"order preserved", []string{"b", "a", "c"}, []string{"b", "a", "c"}},
		{"repeats collapsed", []string{"a", "b", "a"}, []string{"a", "b"}},
		{"blanks dropped", []string{"a", "", "b"}, []string{"a", "b"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := dedupeIDs(c.in)
			if len(got) != len(c.want) {
				t.Fatalf("dedupeIDs(%v) = %v, want %v", c.in, got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Fatalf("dedupeIDs(%v) = %v, want %v", c.in, got, c.want)
				}
			}
		})
	}
}

func TestClampLimit(t *testing.T) {
	cases := []struct{ limit, fallback, max, want int }{
		{0, 10, 50, 10},
		{-1, 10, 50, 10},
		{5, 10, 50, 5},
		{500, 10, 50, 50},
		{50, 10, 50, 50},
	}
	for _, c := range cases {
		if got := clampLimit(c.limit, c.fallback, c.max); got != c.want {
			t.Errorf("clampLimit(%d, %d, %d) = %d, want %d", c.limit, c.fallback, c.max, got, c.want)
		}
	}
}

func TestMetadataState(t *testing.T) {
	for _, s := range []string{"pending", "resolved", "unavailable", "failed"} {
		if got := metadataState(s); string(got) != s {
			t.Errorf("metadataState(%q) = %q", s, got)
		}
	}
	// A value this binary does not know must degrade to "fetch it again", never
	// to an invalid state that leaks into the API.
	if got := metadataState("relinked"); got != domain.MetadataPending {
		t.Errorf("metadataState(unknown) = %q, want pending", got)
	}
}

func TestJoinGenres(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want string
	}{
		{"none", nil, ""},
		{"one", []string{"indie pop"}, "indie pop"},
		{"several", []string{"indie pop", "shoegaze"}, "indie pop" + genreSep + "shoegaze"},
		{"blanks dropped", []string{"", "  ", "rock"}, "rock"},
		{"separator neutralised", []string{"a" + genreSep + "b"}, "a b"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := joinGenres(c.in); got != c.want {
				t.Errorf("joinGenres(%v) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestEscapeLike(t *testing.T) {
	cases := map[string]string{
		"beatles":  "beatles",
		"100%":     `100\%`,
		"a_b":      `a\_b`,
		`back\\sl`: `back\\\\sl`,
	}
	for in, want := range cases {
		if got := escapeLike(in); got != want {
			t.Errorf("escapeLike(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestBuildArtistRows(t *testing.T) {
	rows := buildArtistRows([]domain.Artist{
		{ID: "", Name: "dropped"},
		{ID: "a1", Name: "Sigur Rós", Genres: []string{"post-rock"}, Popularity: 61, Followers: 12345, ImageURL: "https://i/1"},
		{ID: "a2", Name: "Björk", NameNorm: "stale value"},
	})

	if len(rows.ids) != 2 {
		t.Fatalf("ids = %v, want the empty id dropped", rows.ids)
	}
	if rows.ids[0] != "a1" || rows.ids[1] != "a2" {
		t.Fatalf("ids = %v", rows.ids)
	}
	// name_norm is always re-derived so the search column cannot drift from the
	// normaliser the alias resolver uses.
	if want := domain.NormalizeArtist("Sigur Rós"); rows.norms[0] != want {
		t.Errorf("norms[0] = %q, want %q", rows.norms[0], want)
	}
	if want := domain.NormalizeArtist("Björk"); rows.norms[1] != want {
		t.Errorf("norms[1] = %q, want %q (caller value must be ignored)", rows.norms[1], want)
	}
	if rows.genres[0] != "post-rock" || rows.genres[1] != "" {
		t.Errorf("genres = %q", rows.genres)
	}
	if rows.popularity[0] != 61 || rows.followers[0] != 12345 || rows.images[0] != "https://i/1" {
		t.Errorf("scalar columns not transposed: %+v", rows)
	}
	if len(rows.names) != 2 || len(rows.norms) != 2 || len(rows.genres) != 2 ||
		len(rows.images) != 2 || len(rows.popularity) != 2 || len(rows.followers) != 2 {
		t.Errorf("parallel arrays have different lengths: %+v", rows)
	}
}

func TestBuildTrackRows(t *testing.T) {
	rows := buildTrackRows([]domain.Track{
		{ID: "t1", Name: "Karma Police - Remastered 2011", AlbumID: "al1", ArtistIDs: []string{"ar1", "ar2"}, DurationMs: 261000, Explicit: true, Popularity: 77, ISRC: "GBAYE0100850"},
		{ID: "t2", Name: "No Surprises", ArtistIDs: []string{"ar1", "", "ar1"}},
		{ID: ""},
	})

	if len(rows.ids) != 2 {
		t.Fatalf("ids = %v, want the empty id dropped", rows.ids)
	}
	// Edition markers are stripped, which is what lets an account-data export row
	// carrying "- Remastered 2011" find this track through its alias.
	if want := domain.NormalizeTitle("Karma Police - Remastered 2011"); rows.norms[0] != want {
		t.Errorf("norms[0] = %q, want %q", rows.norms[0], want)
	}
	if rows.norms[0] != "karma police" {
		t.Errorf("norms[0] = %q, want %q", rows.norms[0], "karma police")
	}
	if rows.albumIDs[0] == nil || *rows.albumIDs[0] != "al1" {
		t.Errorf("albumIDs[0] = %v, want a pointer to al1", rows.albumIDs[0])
	}
	// An unknown album must be NULL, not the empty string: the column is a
	// foreign key.
	if rows.albumIDs[1] != nil {
		t.Errorf("albumIDs[1] = %v, want nil", *rows.albumIDs[1])
	}
	if got, want := len(rows.artistIDs), 2; got != want {
		t.Fatalf("artistIDs = %v, want the repeat and the blank removed", rows.artistIDs)
	}
	if rows.artistIDs[0] != "ar1" || rows.artistIDs[1] != "ar2" {
		t.Errorf("artistIDs = %v", rows.artistIDs)
	}
	if rows.durations[0] != 261000 || !rows.explicit[0] || rows.popularity[0] != 77 || rows.isrcs[0] != "GBAYE0100850" {
		t.Errorf("scalar columns not transposed: %+v", rows)
	}
}

func TestBuildAlbumRows(t *testing.T) {
	released := time.Date(1997, time.May, 21, 0, 0, 0, 0, time.UTC)
	rows := buildAlbumRows([]domain.Album{
		{ID: "al1", Name: "OK Computer", AlbumType: "album", ReleaseDate: &released, ReleasePrecision: "day", TotalTracks: 12, ImageURL: "https://i/2", ArtistIDs: []string{"ar1"}},
		{ID: "al2", Name: "Kid A", ArtistIDs: []string{"ar1"}},
		{ID: ""},
	})

	if len(rows.ids) != 2 {
		t.Fatalf("ids = %v, want the empty id dropped", rows.ids)
	}
	if rows.releaseDates[0] == nil || !rows.releaseDates[0].Equal(released) {
		t.Errorf("releaseDates[0] = %v, want %v", rows.releaseDates[0], released)
	}
	if rows.releaseDates[1] != nil {
		t.Errorf("releaseDates[1] = %v, want nil", *rows.releaseDates[1])
	}
	if rows.albumTypes[0] != "album" || rows.precisions[0] != "day" || rows.totalTracks[0] != 12 || rows.images[0] != "https://i/2" {
		t.Errorf("scalar columns not transposed: %+v", rows)
	}
	// The two albums credit the same artist; only one pending row is worth
	// creating.
	if len(rows.artistIDs) != 1 || rows.artistIDs[0] != "ar1" {
		t.Errorf("artistIDs = %v, want [ar1]", rows.artistIDs)
	}
}

// The release date is copied before its address is taken, so a caller reusing
// one time.Time across a batch cannot alias every row onto the same value.
func TestBuildAlbumRowsCopiesReleaseDate(t *testing.T) {
	d := time.Date(2001, time.October, 2, 0, 0, 0, 0, time.UTC)
	rows := buildAlbumRows([]domain.Album{{ID: "al1", ReleaseDate: &d}})
	if rows.releaseDates[0] == &d {
		t.Error("release date aliases the caller's value")
	}
	if !rows.releaseDates[0].Equal(d) {
		t.Errorf("release date = %v, want %v", rows.releaseDates[0], d)
	}
}

func TestPendingTotal(t *testing.T) {
	p := Pending{Tracks: 3, Albums: 5, Artists: 7, Aliases: 11}
	if got := p.Total(); got != 26 {
		t.Errorf("Total() = %d, want 26", got)
	}
}

func TestSearchResultsEmpty(t *testing.T) {
	var empty SearchResults
	if !empty.Empty() {
		t.Error("zero SearchResults should be empty")
	}
	if (SearchResults{Tracks: []domain.Track{{ID: "t1"}}}).Empty() {
		t.Error("results with a track should not be empty")
	}
}
