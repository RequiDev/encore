package spotify

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/requi/encore/internal/domain"
)

func TestParseReleaseDate(t *testing.T) {
	date := func(y int, m time.Month, d int) *time.Time {
		v := time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
		return &v
	}

	tests := []struct {
		name          string
		value         string
		precision     string
		want          *time.Time
		wantPrecision string
	}{
		{"day", "2011-03-04", "day", date(2011, time.March, 4), PrecisionDay},
		{"month", "2011-03", "month", date(2011, time.March, 1), PrecisionMonth},
		{"year", "2011", "year", date(2011, time.January, 1), PrecisionYear},
		{"precision absent falls back to the shape", "2011-03-04", "", date(2011, time.March, 4), PrecisionDay},
		{
			// A reissue dated the first of January but declared as year precision
			// must not put a bogus spike on that day in the statistics.
			name:          "declared precision is coarser than the value",
			value:         "1969-01-01",
			precision:     "year",
			want:          date(1969, time.January, 1),
			wantPrecision: PrecisionYear,
		},
		{"declared month with a day value", "1969-06-15", "month", date(1969, time.June, 1), PrecisionMonth},
		{"declared day with a year value stays a year", "1969", "day", date(1969, time.January, 1), PrecisionYear},
		{"impossible day falls back to its month", "2011-02-30", "day", date(2011, time.February, 1), PrecisionMonth},
		{"month out of range", "2011-13", "month", date(2011, time.January, 1), PrecisionYear},
		{"full timestamp", "2011-03-04T00:00:00Z", "day", date(2011, time.March, 4), PrecisionDay},
		{"empty", "", "day", nil, ""},
		{"whitespace", "   ", "day", nil, ""},
		{"zero year", "0000", "year", nil, ""},
		{"nonsense", "unknown", "day", nil, ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, precision := ParseReleaseDate(tc.value, tc.precision)
			switch {
			case tc.want == nil && got != nil:
				t.Fatalf("ParseReleaseDate(%q) = %s, want nil", tc.value, got)
			case tc.want != nil && got == nil:
				t.Fatalf("ParseReleaseDate(%q) = nil, want %s", tc.value, tc.want)
			case tc.want != nil && !got.Equal(*tc.want):
				t.Fatalf("ParseReleaseDate(%q) = %s, want %s", tc.value, got, tc.want)
			}
			if precision != tc.wantPrecision {
				t.Fatalf("precision = %q, want %q", precision, tc.wantPrecision)
			}
		})
	}
}

func TestToDomainTrack(t *testing.T) {
	const raw = `{
        "id": "track0000000000000001",
        "name": "Song - Remastered 2011",
        "type": "track",
        "duration_ms": 214000,
        "explicit": true,
        "popularity": 73,
        "external_ids": {"isrc": "gbayE0601498"},
        "album": {"id": "album0000000000000001", "name": "Record"},
        "artists": [
            {"id": "artist000000000000001", "name": "Band"},
            {"id": "artist000000000000002", "name": "Guest"},
            {"id": "artist000000000000001", "name": "Band"}
        ]
    }`

	var track Track
	if err := json.Unmarshal([]byte(raw), &track); err != nil {
		t.Fatalf("decode track: %v", err)
	}

	got := track.ToDomainTrack()
	if got.ID != "track0000000000000001" || got.AlbumID != "album0000000000000001" {
		t.Fatalf("track = %+v", got)
	}
	// The edition marker is stripped by the same normaliser the importer uses, so
	// a names-only listen for "Song" resolves to this catalogue row.
	if got.NameNorm != "song" {
		t.Errorf("NameNorm = %q, want %q", got.NameNorm, "song")
	}
	if got.ISRC != "GBAYE0601498" {
		t.Errorf("ISRC = %q, want it upper-cased", got.ISRC)
	}
	if len(got.ArtistIDs) != 2 || got.ArtistIDs[0] != "artist000000000000001" {
		t.Errorf("ArtistIDs = %v, want the credits in order without repeats", got.ArtistIDs)
	}
	if got.DurationMs != 214000 || got.Popularity != 73 || !got.Explicit {
		t.Errorf("track = %+v", got)
	}
	if got.MetadataState != domain.MetadataResolved {
		t.Errorf("MetadataState = %q, want resolved", got.MetadataState)
	}
}

func TestToDomainArtistAndAlbum(t *testing.T) {
	artist := Artist{
		ID:         "artist000000000000001",
		Name:       "Sigur Rós",
		Genres:     []string{"post-rock", "  ", "icelandic"},
		Popularity: 61,
		Followers:  Followers{Total: 1234567},
		Images:     []Image{{URL: "small.jpg", Width: 160, Height: 160}, {URL: "large.jpg", Width: 640, Height: 640}},
	}
	da := artist.ToDomainArtist()
	if da.NameNorm != domain.NormalizeArtist("Sigur Rós") {
		t.Errorf("NameNorm = %q", da.NameNorm)
	}
	if len(da.Genres) != 2 {
		t.Errorf("Genres = %v, want the blank entry dropped", da.Genres)
	}
	if da.Followers != 1234567 || da.ImageURL != "large.jpg" {
		t.Errorf("artist = %+v", da)
	}

	album := Album{
		ID:                   "album0000000000000001",
		Name:                 "Record (Deluxe Edition)",
		AlbumType:            "ALBUM",
		ReleaseDate:          "2011-03",
		ReleaseDatePrecision: "month",
		TotalTracks:          12,
		Images:               []Image{{URL: "cover.jpg", Width: 640, Height: 640}},
		Artists:              []Artist{{ID: "artist000000000000001"}},
	}
	dl := album.ToDomainAlbum()
	if dl.NameNorm != "record" {
		t.Errorf("NameNorm = %q, want the edition marker stripped", dl.NameNorm)
	}
	if dl.AlbumType != "album" {
		t.Errorf("AlbumType = %q, want it lower-cased", dl.AlbumType)
	}
	if dl.ReleasePrecision != PrecisionMonth || dl.ReleaseYear() != 2011 {
		t.Errorf("album = %+v", dl)
	}
	if len(dl.ArtistIDs) != 1 || dl.TotalTracks != 12 || dl.ImageURL != "cover.jpg" {
		t.Errorf("album = %+v", dl)
	}
}

func TestTrackIsMusic(t *testing.T) {
	tests := []struct {
		name  string
		track Track
		want  bool
	}{
		{"track", Track{ID: "track0000000000000001", Type: "track"}, true},
		{"type omitted", Track{ID: "track0000000000000001"}, true},
		{"episode", Track{ID: "episode00000000000001", Type: "episode"}, false},
		{"local file", Track{ID: "track0000000000000001", Type: "track", IsLocal: true}, false},
		{"no id", Track{Type: "track"}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.track.IsMusic(); got != tc.want {
				t.Fatalf("IsMusic = %t, want %t", got, tc.want)
			}
		})
	}
}

func TestImageURL(t *testing.T) {
	tests := []struct {
		name   string
		images []Image
		want   string
	}{
		{"none", nil, ""},
		{"largest wins", []Image{{URL: "a.jpg", Width: 64, Height: 64}, {URL: "b.jpg", Width: 300, Height: 300}}, "b.jpg"},
		{"dimensions missing", []Image{{URL: "a.jpg"}, {URL: "b.jpg"}}, "a.jpg"},
		{"blank urls skipped", []Image{{URL: "", Width: 640, Height: 640}, {URL: "b.jpg", Width: 64, Height: 64}}, "b.jpg"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ImageURL(tc.images); got != tc.want {
				t.Fatalf("ImageURL = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestPlayHistoryDecodesMillisecondTimestamps(t *testing.T) {
	const raw = `{"played_at":"2026-07-20T10:15:04.589Z","track":{"id":"track0000000000000001","name":"Song","type":"track"}}`
	var ph PlayHistory
	if err := json.Unmarshal([]byte(raw), &ph); err != nil {
		t.Fatalf("decode play history: %v", err)
	}
	want := time.Date(2026, time.July, 20, 10, 15, 4, 589_000_000, time.UTC)
	if !ph.PlayedAt.Equal(want) {
		t.Fatalf("PlayedAt = %s, want %s", ph.PlayedAt, want)
	}
}
