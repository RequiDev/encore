package domain

import (
	"fmt"
	"time"

	"github.com/google/uuid"
)

// PlaylistMode is how a definition chooses its tracks.
type PlaylistMode string

const (
	// PlaylistModeTop is the N most-played tracks in the range. The plain
	// "my 2025".
	PlaylistModeTop PlaylistMode = "top"
	// PlaylistModeMinPlays is every track played at least MinPlays times in the
	// range. Not a leaderboard: what was actually on repeat, however many that
	// turns out to be.
	PlaylistModeMinPlays PlaylistMode = "min_plays"
	// PlaylistModeDiscoveries is tracks first ever heard inside the range. What
	// was new, as opposed to what was played most.
	PlaylistModeDiscoveries PlaylistMode = "discoveries"
	// PlaylistModeForgotten is tracks played heavily before the range and not at
	// all inside it — what dropped out of rotation.
	PlaylistModeForgotten PlaylistMode = "forgotten"
)

// PlaylistSort is what a mode ranks by.
type PlaylistSort string

const (
	// SortByPlays ranks by how often a track was played.
	SortByPlays PlaylistSort = "plays"
	// SortByTime ranks by how long it was played for, which favours long tracks
	// listened to fully over short ones skipped through.
	SortByTime PlaylistSort = "time"
)

// CoverState is what happened the last time Encore tried to give a playlist a
// cover image.
type CoverState string

const (
	// CoverNone means no attempt has been made. Every playlist made before
	// covers existed is in this state, and stays in it until somebody asks.
	CoverNone CoverState = "none"
	// CoverReady means Spotify accepted an uploaded cover.
	CoverReady CoverState = "ready"
	// CoverFailed means an attempt was made and did not finish.
	CoverFailed CoverState = "failed"
	// CoverUnauthorised means the account has not granted ugc-image-upload.
	//
	// Deliberately not CoverFailed. The fix is a trip through Spotify's
	// consent screen, not a retry, and offering a retry button for it would
	// invite somebody to press a thing that cannot work.
	CoverUnauthorised CoverState = "unauthorised"
)

// CoverTileTotal is how many tiles the mosaic asks for, and so the denominator
// of every sentence about a cover's coverage.
//
// A constant rather than the number of distinct albums in the playlist: "built
// from 2 of 4 album covers" is the honest report of a grid that wanted four and
// got two, whereas "2 of 2" would describe a full mosaic that was never built.
const CoverTileTotal = 4

// PlaylistCover records the outcome of the last cover attempt.
type PlaylistCover struct {
	State CoverState
	// Tiles is how many of CoverTileTotal came from real album artwork. Zero
	// means the cover is the generated pattern.
	Tiles int
	// Error is why the last attempt failed, in the listener's own terms. Empty
	// in every state but CoverFailed.
	Error string
	// At is when State was last written. Zero while State is CoverNone.
	At time.Time
}

// Mosaic reports whether the stored cover is built from real artwork rather
// than being the generated pattern.
func (c PlaylistCover) Mosaic() bool { return c.State == CoverReady && c.Tiles > 0 }

// Playlist bounds. Spotify's own ceiling is 10,000 items; the lower cap here is
// about what a playlist stays useful at, and keeps a rebuild to a handful of
// requests rather than a hundred.
const (
	PlaylistMaxTracks       = 500
	PlaylistDefaultTracks   = 100
	PlaylistMaxNameLength   = 100
	PlaylistMaxMinPlays     = 10000
	PlaylistDefaultMinPlays = 10
)

// PlaylistDefinition is the recipe for a playlist: what to select and how many.
//
// It is stored, so that "rebuild" means re-running the same question rather than
// asking the user to describe it again. It is not run on a schedule — a playlist
// that changed under its owner, or silently overwrote an edit they made in
// Spotify, would be worse than one that is simply out of date.
type PlaylistDefinition struct {
	Mode PlaylistMode
	Sort PlaylistSort
	// Limit caps the tracks selected. Always applied, including for MinPlays,
	// where the count is otherwise unbounded.
	Limit int
	// MinPlays is the threshold for PlaylistModeMinPlays. Ignored otherwise.
	MinPlays int
	// From and To pin the range. Both zero means all time.
	From time.Time
	To   time.Time
}

// Playlist is a definition together with what it produced on Spotify.
type Playlist struct {
	ID     uuid.UUID
	UserID uuid.UUID
	Name   string
	// SpotifyID is the playlist Encore created and rebuilds in place.
	SpotifyID  string
	SpotifyURL string
	Definition PlaylistDefinition
	TrackCount int
	BuiltAt    time.Time
	// Cover is the outcome of the last attempt to give this playlist a picture.
	// It is not part of the definition: two playlists with the same recipe can
	// have different covers, because one of them was made when the catalogue
	// had less artwork in it.
	Cover     PlaylistCover
	CreatedAt time.Time
}

// Valid reports whether m is a mode Encore implements.
func (m PlaylistMode) Valid() bool {
	switch m {
	case PlaylistModeTop, PlaylistModeMinPlays, PlaylistModeDiscoveries, PlaylistModeForgotten:
		return true
	}
	return false
}

// Valid reports whether s is a supported ranking.
func (s PlaylistSort) Valid() bool {
	return s == SortByPlays || s == SortByTime
}

// OrderColumn is the aggregate a definition ranks by.
//
// It returns one of two fixed identifiers chosen inside this package and never
// anything derived from input, so composing it into a statement is not an
// injection vector.
func (d PlaylistDefinition) OrderColumn() string {
	if d.Sort == SortByTime {
		return "ms"
	}
	return "plays"
}

// Validate checks a definition before it is stored or run.
func (d PlaylistDefinition) Validate() error {
	switch {
	case !d.Mode.Valid():
		return fmt.Errorf("%w: %q is not a playlist mode", ErrValidation, d.Mode)
	case !d.Sort.Valid():
		return fmt.Errorf("%w: %q is not a way to rank tracks", ErrValidation, d.Sort)
	case d.Limit < 1 || d.Limit > PlaylistMaxTracks:
		return fmt.Errorf("%w: a playlist holds between 1 and %d tracks",
			ErrValidation, PlaylistMaxTracks)
	case d.Mode == PlaylistModeMinPlays && (d.MinPlays < 1 || d.MinPlays > PlaylistMaxMinPlays):
		return fmt.Errorf("%w: the minimum play count must be between 1 and %d",
			ErrValidation, PlaylistMaxMinPlays)
	}
	if !d.From.IsZero() || !d.To.IsZero() {
		if d.From.IsZero() || d.To.IsZero() {
			return fmt.Errorf("%w: a range needs both a start and an end", ErrValidation)
		}
		if !d.From.Before(d.To) {
			return fmt.Errorf("%w: the start must be before the end", ErrValidation)
		}
	}
	// Forgotten favourites need something before the range to look back at, so an
	// all-time range would select nothing and say nothing about why.
	if d.Mode == PlaylistModeForgotten && d.From.IsZero() {
		return fmt.Errorf(
			"%w: forgotten favourites need a range to have dropped out of; pick a period",
			ErrValidation)
	}
	return nil
}

// Range resolves the definition's window, anchoring an open range at the user's
// first listen so every statistics query gets two real bounds.
func (d PlaylistDefinition) Range(now, firstListen time.Time) TimeRange {
	if !d.From.IsZero() && !d.To.IsZero() {
		return TimeRange{From: d.From, To: d.To}
	}
	from := firstListen
	if from.IsZero() {
		from = now.Add(-24 * time.Hour)
	}
	return TimeRange{From: from, To: now}
}

// ValidatePlaylistName checks the name Spotify will show.
func ValidatePlaylistName(name string) error {
	switch {
	case name == "":
		return fmt.Errorf("%w: a playlist needs a name", ErrValidation)
	case len([]rune(name)) > PlaylistMaxNameLength:
		return fmt.Errorf("%w: the name may be at most %d characters",
			ErrValidation, PlaylistMaxNameLength)
	}
	return nil
}
