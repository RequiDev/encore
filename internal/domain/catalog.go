package domain

import (
	"fmt"
	"time"
)

// MetadataState tracks how far catalogue enrichment has got with an entity.
// Ingestion always inserts entities as Pending and never blocks on Spotify.
type MetadataState string

const (
	// MetadataPending: the id is known, the details are not yet fetched.
	MetadataPending MetadataState = "pending"
	// MetadataResolved: details fetched successfully.
	MetadataResolved MetadataState = "resolved"
	// MetadataUnavailable: Spotify authoritatively has nothing for this id
	// (deleted, region-locked, or a relinked id that no longer exists). Retrying
	// will not help, but a repair job may revisit it after a long delay.
	MetadataUnavailable MetadataState = "unavailable"
	// MetadataFailed: enrichment exhausted its retries. Eligible for the repair job.
	MetadataFailed MetadataState = "failed"
)

func (m MetadataState) Valid() bool {
	switch m {
	case MetadataPending, MetadataResolved, MetadataUnavailable, MetadataFailed:
		return true
	}
	return false
}

// NeedsFetch reports whether an entity in this state should be queued.
func (m MetadataState) NeedsFetch() bool {
	return m == MetadataPending || m == MetadataFailed
}

// Artist is a Spotify artist.
type Artist struct {
	ID            string
	Name          string
	NameNorm      string
	Genres        []string
	Popularity    int32
	Followers     int64
	ImageURL      string
	MetadataState MetadataState
	FetchAttempts int32
	NextAttemptAt *time.Time
	FetchedAt     *time.Time
}

// Album is a Spotify album, single or compilation.
type Album struct {
	ID               string
	Name             string
	NameNorm         string
	AlbumType        string
	ReleaseDate      *time.Time
	ReleasePrecision string // "year" | "month" | "day"
	TotalTracks      int32
	ImageURL         string
	ArtistIDs        []string
	MetadataState    MetadataState
	FetchAttempts    int32
	NextAttemptAt    *time.Time
	FetchedAt        *time.Time
}

// ReleaseYear returns the release year, or 0 when unknown.
func (a Album) ReleaseYear() int {
	if a.ReleaseDate == nil {
		return 0
	}
	return a.ReleaseDate.Year()
}

// Track is a Spotify track. AlbumID and ArtistIDs are empty until enrichment runs.
type Track struct {
	ID            string
	Name          string
	NameNorm      string
	AlbumID       string
	ArtistIDs     []string
	DurationMs    int32
	Explicit      bool
	Popularity    int32
	ISRC          string
	MetadataState MetadataState
	FetchAttempts int32
	NextAttemptAt *time.Time
	FetchedAt     *time.Time
}

// TrackAlias maps a normalised (artist, title) pair from a names-only export onto
// a real catalogue track. It is the mechanism that lets an account-data import and
// an extended import of the same period converge on one identity.
type TrackAlias struct {
	ArtistNorm    string
	TitleNorm     string
	TrackID       string
	State         MetadataState
	FetchAttempts int32
	NextAttemptAt *time.Time
	ResolvedAt    *time.Time
}

// AliasKey is the composite key of a TrackAlias.
type AliasKey struct {
	ArtistNorm string
	TitleNorm  string
}

func (k AliasKey) String() string { return fmt.Sprintf("%s\x00%s", k.ArtistNorm, k.TitleNorm) }

// IsZero reports whether the key carries no information.
func (k AliasKey) IsZero() bool { return k.ArtistNorm == "" && k.TitleNorm == "" }

// AliasKeyFor builds an alias key from raw names.
func AliasKeyFor(artist, title string) AliasKey {
	return AliasKey{ArtistNorm: NormalizeArtist(artist), TitleNorm: NormalizeTitle(title)}
}

// AliasKeyOf returns the alias key an unresolved identity looks up under.
func (t TrackIdentity) AliasKeyOf() AliasKey {
	if t.IsResolved() {
		return AliasKey{}
	}
	return AliasKey{ArtistNorm: t.Artist, TitleNorm: t.Title}
}

// BackoffAttempts is the number of enrichment attempts before an entity is parked
// in MetadataFailed for the repair job to pick up later.
const BackoffAttempts = 6

// NextMetadataAttempt returns the delay before retry number `attempt`
// (1-based), capped. Jitter is applied by the caller so this stays deterministic
// and testable.
func NextMetadataAttempt(attempt int32) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	const base = 30 * time.Second
	const max = 6 * time.Hour
	d := base
	for range attempt - 1 {
		d *= 3
		if d >= max {
			return max
		}
	}
	return d
}
