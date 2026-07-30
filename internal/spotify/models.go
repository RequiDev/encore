package spotify

import (
	"strconv"
	"strings"
	"time"

	"github.com/RequiDev/encore/internal/domain"
)

// Image is one size of a piece of artwork. Spotify returns several sizes per
// entity, widest first, but the order is not guaranteed by the documentation.
type Image struct {
	URL    string `json:"url"`
	Height int    `json:"height"`
	Width  int    `json:"width"`
}

// ExternalIDs carries the industry identifiers of a track. ISRC is the useful
// one: it is stable across releases and territories, so it survives Spotify
// relinking a track id.
type ExternalIDs struct {
	ISRC string `json:"isrc"`
	EAN  string `json:"ean"`
	UPC  string `json:"upc"`
}

// ExternalURLs holds the open.spotify.com links of an entity.
type ExternalURLs struct {
	Spotify string `json:"spotify"`
}

// Followers is Spotify's follower count object.
type Followers struct {
	Href  string `json:"href"`
	Total int64  `json:"total"`
}

// Artist is a Spotify artist. Genres, Popularity and Followers are populated
// only by the full artist object; the simplified artist embedded in a track or
// album carries just the identity fields.
type Artist struct {
	ID           string       `json:"id"`
	Name         string       `json:"name"`
	URI          string       `json:"uri"`
	Href         string       `json:"href"`
	Type         string       `json:"type"`
	Genres       []string     `json:"genres"`
	Popularity   int          `json:"popularity"`
	Followers    Followers    `json:"followers"`
	Images       []Image      `json:"images"`
	ExternalURLs ExternalURLs `json:"external_urls"`
}

// Album is a Spotify album, single or compilation.
//
// ReleaseDate is deliberately left as the string Spotify sent: it may be a bare
// year or a year and month, and only ParseReleaseDate knows what to do with
// that. ReleaseDatePrecision says which of the three forms was intended.
type Album struct {
	ID                   string       `json:"id"`
	Name                 string       `json:"name"`
	URI                  string       `json:"uri"`
	Href                 string       `json:"href"`
	Type                 string       `json:"type"`
	AlbumType            string       `json:"album_type"`
	AlbumGroup           string       `json:"album_group"`
	ReleaseDate          string       `json:"release_date"`
	ReleaseDatePrecision string       `json:"release_date_precision"`
	TotalTracks          int          `json:"total_tracks"`
	AvailableMarkets     []string     `json:"available_markets"`
	Images               []Image      `json:"images"`
	Artists              []Artist     `json:"artists"`
	ExternalURLs         ExternalURLs `json:"external_urls"`
}

// LinkedTrack is the original id of a track that Spotify has relinked for the
// market the token resolves to. A recently-played item can report a market
// specific id in Track.ID while LinkedFrom holds the id the listener's client
// actually played.
type LinkedTrack struct {
	ID           string       `json:"id"`
	URI          string       `json:"uri"`
	Href         string       `json:"href"`
	Type         string       `json:"type"`
	ExternalURLs ExternalURLs `json:"external_urls"`
}

// Track is a Spotify track.
type Track struct {
	ID           string       `json:"id"`
	Name         string       `json:"name"`
	URI          string       `json:"uri"`
	Href         string       `json:"href"`
	Type         string       `json:"type"`
	DurationMs   int          `json:"duration_ms"`
	Explicit     bool         `json:"explicit"`
	Popularity   int          `json:"popularity"`
	DiscNumber   int          `json:"disc_number"`
	TrackNumber  int          `json:"track_number"`
	IsLocal      bool         `json:"is_local"`
	IsPlayable   bool         `json:"is_playable"`
	Album        Album        `json:"album"`
	Artists      []Artist     `json:"artists"`
	ExternalIDs  ExternalIDs  `json:"external_ids"`
	ExternalURLs ExternalURLs `json:"external_urls"`
	LinkedFrom   *LinkedTrack `json:"linked_from"`
}

// IsMusic reports whether the item is a catalogue track rather than a podcast
// episode or a local file. Recently-played interleaves all three, and only the
// first has a catalogue identity Encore can enrich.
func (t Track) IsMusic() bool {
	if t.IsLocal || t.ID == "" {
		return false
	}
	return t.Type == "" || t.Type == "track"
}

// PrimaryArtist is the first credited artist, which is the one Encore attributes
// a listen to when it needs a single name.
func (t Track) PrimaryArtist() string {
	if len(t.Artists) == 0 {
		return ""
	}
	return t.Artists[0].Name
}

// UserProfile is the Spotify account behind an Encore user. Email and Product
// are present only when the grant includes user-read-email and
// user-read-private respectively.
type UserProfile struct {
	ID           string       `json:"id"`
	DisplayName  string       `json:"display_name"`
	Email        string       `json:"email"`
	Country      string       `json:"country"`
	Product      string       `json:"product"`
	URI          string       `json:"uri"`
	Href         string       `json:"href"`
	Images       []Image      `json:"images"`
	Followers    Followers    `json:"followers"`
	ExternalURLs ExternalURLs `json:"external_urls"`
}

// AvatarURL is the largest profile picture, or "" when the account has none.
func (p UserProfile) AvatarURL() string { return ImageURL(p.Images) }

// PlayContext is what the listener was playing from: a playlist, an album, an
// artist page. Spotify reports it as null often enough that it is a pointer.
type PlayContext struct {
	Type         string       `json:"type"`
	URI          string       `json:"uri"`
	Href         string       `json:"href"`
	ExternalURLs ExternalURLs `json:"external_urls"`
}

// PlayHistory is one entry of the recently-played feed.
//
// PlayedAt is the moment playback *started*, which is exactly the anchor
// domain.Listen wants, so no conversion from an end time is needed for this
// source.
type PlayHistory struct {
	Track    Track        `json:"track"`
	PlayedAt time.Time    `json:"played_at"`
	Context  *PlayContext `json:"context"`
}

// ImageURL picks the largest image, falling back to the first entry when the
// dimensions are not reported.
func ImageURL(images []Image) string {
	best := ""
	bestArea := -1
	for _, img := range images {
		if img.URL == "" {
			continue
		}
		area := img.Width * img.Height
		if area > bestArea {
			best, bestArea = img.URL, area
		}
	}
	return best
}

// ToDomainTrack converts a Spotify track into the catalogue's own type.
//
// The normalised name is produced by the same function the importer applies to
// export files, which is what lets a names-only listen and a catalogue entry
// converge on one identity.
func (t Track) ToDomainTrack() domain.Track {
	return domain.Track{
		ID:            t.ID,
		Name:          t.Name,
		NameNorm:      domain.NormalizeTitle(t.Name),
		AlbumID:       t.Album.ID,
		ArtistIDs:     artistIDs(t.Artists),
		DurationMs:    clampInt32(t.DurationMs),
		Explicit:      t.Explicit,
		Popularity:    clampInt32(t.Popularity),
		ISRC:          strings.ToUpper(strings.TrimSpace(t.ExternalIDs.ISRC)),
		MetadataState: domain.MetadataResolved,
	}
}

// ToDomainArtist converts a Spotify artist into the catalogue's own type.
func (a Artist) ToDomainArtist() domain.Artist {
	return domain.Artist{
		ID:            a.ID,
		Name:          a.Name,
		NameNorm:      domain.NormalizeArtist(a.Name),
		Genres:        cleanStrings(a.Genres),
		Popularity:    clampInt32(a.Popularity),
		Followers:     a.Followers.Total,
		ImageURL:      ImageURL(a.Images),
		MetadataState: domain.MetadataResolved,
	}
}

// ToDomainAlbum converts a Spotify album into the catalogue's own type,
// resolving its partial release date on the way.
func (a Album) ToDomainAlbum() domain.Album {
	date, precision := ParseReleaseDate(a.ReleaseDate, a.ReleaseDatePrecision)
	return domain.Album{
		ID:               a.ID,
		Name:             a.Name,
		NameNorm:         domain.NormalizeTitle(a.Name),
		AlbumType:        strings.ToLower(strings.TrimSpace(a.AlbumType)),
		ReleaseDate:      date,
		ReleasePrecision: precision,
		TotalTracks:      clampInt32(a.TotalTracks),
		ImageURL:         ImageURL(a.Images),
		ArtistIDs:        artistIDs(a.Artists),
		MetadataState:    domain.MetadataResolved,
	}
}

// Release date precisions, matching the values stored in albums.release_precision.
const (
	PrecisionYear  = "year"
	PrecisionMonth = "month"
	PrecisionDay   = "day"
)

// ParseReleaseDate turns Spotify's partial release date into an instant plus the
// precision that instant is trustworthy to. Spotify sends "2011", "2011-03" or
// "2011-03-04", and an unknown date as "" or a zero year.
//
// The value's own shape decides the precision, except that a declared precision
// which is *coarser* than the value wins: a 1969 reissue frequently arrives as
// "1969-01-01" with precision "year", and recording January the 1st as if it
// were a real release day would put a bogus spike in the statistics. The date is
// truncated to match, so the stored value never claims more than it knows.
//
// An unparseable or absent date returns (nil, "").
func ParseReleaseDate(value, precision string) (*time.Time, string) {
	v := strings.TrimSpace(value)
	if v == "" {
		return nil, ""
	}
	// Tolerate a full timestamp, which some proxies and older clients produce.
	if i := strings.IndexAny(v, "T "); i > 0 {
		v = v[:i]
	}

	parts := strings.Split(v, "-")
	year, err := strconv.Atoi(parts[0])
	if err != nil || year < 1000 || year > 9999 {
		return nil, ""
	}

	month, day := 1, 1
	got := PrecisionYear
	if len(parts) > 1 {
		if m, err := strconv.Atoi(parts[1]); err == nil && m >= 1 && m <= 12 {
			month, got = m, PrecisionMonth
		}
	}
	if got == PrecisionMonth && len(parts) > 2 {
		if d, err := strconv.Atoi(parts[2]); err == nil && d >= 1 && d <= 31 {
			day, got = d, PrecisionDay
		}
	}

	switch strings.ToLower(strings.TrimSpace(precision)) {
	case PrecisionYear:
		got, month, day = PrecisionYear, 1, 1
	case PrecisionMonth:
		if got == PrecisionDay {
			got, day = PrecisionMonth, 1
		}
	}

	t := time.Date(year, time.Month(month), day, 0, 0, 0, 0, time.UTC)
	// time.Date normalises the 30th of February into March; a date that does not
	// survive the round trip was never a real day, so fall back to its month.
	if t.Year() != year || int(t.Month()) != month || t.Day() != day {
		t = time.Date(year, time.Month(month), 1, 0, 0, 0, 0, time.UTC)
		got = PrecisionMonth
	}
	return &t, got
}

// artistIDs collects the ids of the credited artists in billing order.
func artistIDs(artists []Artist) []string {
	if len(artists) == 0 {
		return nil
	}
	out := make([]string, 0, len(artists))
	seen := make(map[string]struct{}, len(artists))
	for _, a := range artists {
		if a.ID == "" {
			continue
		}
		if _, dup := seen[a.ID]; dup {
			continue
		}
		seen[a.ID] = struct{}{}
		out = append(out, a.ID)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// cleanStrings trims and drops empty entries, and copies the slice so the
// catalogue value does not alias the decoded response.
func cleanStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// clampInt32 narrows a decoded number to the width of its catalogue column
// instead of letting an absurd value wrap around into a negative one.
func clampInt32(n int) int32 {
	const maxInt32 = 1<<31 - 1
	const minInt32 = -1 << 31
	switch {
	case n > maxInt32:
		return maxInt32
	case n < minInt32:
		return minInt32
	default:
		return int32(n)
	}
}
