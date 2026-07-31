package httpapi

import (
	"strconv"
	"time"

	"github.com/RequiDev/encore/internal/albumtracks"
	"github.com/RequiDev/encore/internal/artistalbums"
	"github.com/RequiDev/encore/internal/domain"
	"github.com/RequiDev/encore/internal/importer"
	"github.com/RequiDev/encore/internal/stats"
	"github.com/RequiDev/encore/internal/store/catalog"
	"github.com/RequiDev/encore/internal/store/imports"
)

// This file is the server half of the contract in docs/api.md; the client half
// is web/src/lib/types.ts. Field names are camelCase, timestamps are RFC 3339
// with a Z offset, and durations are whole milliseconds.
//
// Every list field is emitted as [] rather than null, because a client that has
// to write `items ?? []` at each use eventually forgets one.

// --- primitives ------------------------------------------------------------

// ts renders an instant in the wire format. Everything is normalised to UTC so
// that two clients in different places compare timestamps byte for byte.
func ts(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// tsPtr renders an optional instant, preserving "never happened" as JSON null.
func tsPtr(t *time.Time) *string {
	if t == nil || t.IsZero() {
		return nil
	}
	s := ts(*t)
	return &s
}

// localDay renders a calendar day, which carries no time of day and so is not a
// timestamp.
func localDay(t time.Time) string { return t.Format("2006-01-02") }

// --- identity --------------------------------------------------------------

// User is an Encore account as its owner and the administrators see it.
type User struct {
	ID            string  `json:"id"`
	SpotifyUserID string  `json:"spotifyUserId"`
	DisplayName   string  `json:"displayName"`
	Email         string  `json:"email"`
	AvatarURL     string  `json:"avatarUrl"`
	Role          string  `json:"role"`
	IsActive      bool    `json:"isActive"`
	Timezone      string  `json:"timezone"`
	CreatedAt     string  `json:"createdAt"`
	LastLoginAt   *string `json:"lastLoginAt"`
}

// toUser converts a stored user. No token, hash or session identifier is part of
// this shape, and none may ever be added to it.
func toUser(u domain.User) User {
	return User{
		ID:            u.ID.String(),
		SpotifyUserID: u.SpotifyUserID,
		DisplayName:   u.DisplayName,
		Email:         u.Email,
		AvatarURL:     u.AvatarURL,
		Role:          string(u.Role),
		IsActive:      u.IsActive,
		Timezone:      u.Timezone,
		CreatedAt:     ts(u.CreatedAt),
		LastLoginAt:   tsPtr(u.LastLoginAt),
	}
}

// PublicUser is what one user may see of another: enough to pick them from a
// list and nothing more.
type PublicUser struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayName"`
	AvatarURL   string `json:"avatarUrl"`
}

// toPublicUser reduces a user to the fields the comparison page may show.
func toPublicUser(u domain.User) PublicUser {
	return PublicUser{ID: u.ID.String(), DisplayName: u.DisplayName, AvatarURL: u.AvatarURL}
}

// SpotifyConnection is the health of a user's Spotify grant. The tokens
// themselves never leave the database in readable form and are not represented
// here at all.
type SpotifyConnection struct {
	Connected     bool     `json:"connected"`
	SyncState     string   `json:"syncState"`
	LastSyncAt    *string  `json:"lastSyncAt"`
	LastSyncError string   `json:"lastSyncError"`
	Scopes        []string `json:"scopes"`
	// MissingScopes is what this account granted less than Encore now asks for.
	//
	// Computed on the server against config.DefaultScopes() rather than compared
	// in the client, because two copies of the required list would drift and the
	// TypeScript one would drift silently. Empty means the grant is current.
	MissingScopes []string `json:"missingScopes"`
}

// InstanceInfo is what the client needs to know about the deployment itself.
type InstanceInfo struct {
	RegistrationsEnabled bool   `json:"registrationsEnabled"`
	Version              string `json:"version"`
}

// MeResponse is the bootstrap call the client makes on load.
type MeResponse struct {
	User      User              `json:"user"`
	Spotify   SpotifyConnection `json:"spotify"`
	CSRFToken string            `json:"csrfToken"`
	Instance  InstanceInfo      `json:"instance"`
	Listening ListeningBounds   `json:"listening"`
}

// ListeningBounds is the span of history a user actually holds.
//
// The client needs it so that the "all time" range can start at the first thing
// they listened to rather than at a fixed floor, which would otherwise draw a
// chart whose left half is years of empty buckets.
type ListeningBounds struct {
	FirstListenAt *time.Time `json:"firstListenAt"`
	LastListenAt  *time.Time `json:"lastListenAt"`
}

// AdminUser is a user as the administration page sees them, with the two facts
// an administrator actually needs: how much history they hold and whether their
// Spotify connection still works.
type AdminUser struct {
	User
	ListenCount int64   `json:"listenCount"`
	SyncState   string  `json:"syncState"`
	LastSyncAt  *string `json:"lastSyncAt"`
}

// AdminSettings is the instance-wide configuration an administrator can change.
type AdminSettings struct {
	RegistrationsEnabled bool `json:"registrationsEnabled"`
}

// --- catalogue -------------------------------------------------------------

// ArtistRef is an artist as referenced from a track or a chart.
type ArtistRef struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	ImageURL string `json:"imageUrl"`
}

// Artist is the full artist record shown on the artist page.
type Artist struct {
	ArtistRef
	Genres     []string `json:"genres"`
	Popularity int32    `json:"popularity"`
	Followers  int64    `json:"followers"`
}

func toArtistRef(a domain.Artist) ArtistRef {
	return ArtistRef{ID: a.ID, Name: a.Name, ImageURL: a.ImageURL}
}

func toArtist(a domain.Artist) Artist {
	return Artist{
		ArtistRef:  toArtistRef(a),
		Genres:     nonNil(a.Genres),
		Popularity: a.Popularity,
		Followers:  a.Followers,
	}
}

// AlbumRef is an album as referenced from a track or a chart.
type AlbumRef struct {
	ID               string  `json:"id"`
	Name             string  `json:"name"`
	ImageURL         string  `json:"imageUrl"`
	ReleaseDate      *string `json:"releaseDate"`
	ReleasePrecision string  `json:"releasePrecision"`
}

// Album is the full album record shown on the album page.
type Album struct {
	AlbumRef
	AlbumType   string      `json:"albumType"`
	TotalTracks int32       `json:"totalTracks"`
	Artists     []ArtistRef `json:"artists"`
}

// releaseDate renders a release the way Spotify itself does: only as precisely
// as the catalogue actually knows it, so a year-precision release does not
// acquire an invented first of January.
func releaseDate(a domain.Album) *string {
	return partialDate(a.ReleaseDate, a.ReleasePrecision)
}

func toAlbumRef(a domain.Album) AlbumRef {
	return AlbumRef{
		ID:               a.ID,
		Name:             a.Name,
		ImageURL:         a.ImageURL,
		ReleaseDate:      releaseDate(a),
		ReleasePrecision: a.ReleasePrecision,
	}
}

// TrackRef is a track with just enough context to render a row: its album and
// its credited artists.
type TrackRef struct {
	ID         string      `json:"id"`
	Name       string      `json:"name"`
	DurationMs int32       `json:"durationMs"`
	Explicit   bool        `json:"explicit"`
	Album      *AlbumRef   `json:"album"`
	Artists    []ArtistRef `json:"artists"`
}

// SearchResponse is the catalogue search result, grouped by entity type.
type SearchResponse struct {
	Artists []ArtistRef `json:"artists"`
	Albums  []AlbumRef  `json:"albums"`
	Tracks  []TrackRef  `json:"tracks"`
}

// --- pagination ------------------------------------------------------------

// Page is one page of a list together with the size of the whole list, so the
// client can render a pager without a second call.
type Page[T any] struct {
	Items []T   `json:"items"`
	Total int64 `json:"total"`
}

// --- statistics ------------------------------------------------------------

// Summary is the headline for one range.
type Summary struct {
	Listens         int64   `json:"listens"`
	DistinctTracks  int64   `json:"distinctTracks"`
	DistinctArtists int64   `json:"distinctArtists"`
	DistinctAlbums  int64   `json:"distinctAlbums"`
	MsPlayed        int64   `json:"msPlayed"`
	ActiveDays      int64   `json:"activeDays"`
	FirstListenAt   *string `json:"firstListenAt"`
	LastListenAt    *string `json:"lastListenAt"`
}

func toSummary(s stats.Summary) Summary {
	return Summary{
		Listens:         s.Listens,
		DistinctTracks:  s.DistinctTracks,
		DistinctArtists: s.DistinctArtists,
		DistinctAlbums:  s.DistinctAlbums,
		MsPlayed:        s.MsPlayed,
		ActiveDays:      s.ActiveDays,
		FirstListenAt:   tsPtr(s.FirstListenAt),
		LastListenAt:    tsPtr(s.LastListenAt),
	}
}

// TopEntry is one row of a ranked list.
//
// PreviousRank is null when the entity did not appear in the equal-length
// preceding period, which the interface renders as "new" rather than as a rise
// from infinity.
type TopEntry[T any] struct {
	Entity       T     `json:"entity"`
	Plays        int64 `json:"plays"`
	MsPlayed     int64 `json:"msPlayed"`
	Rank         int   `json:"rank"`
	PreviousRank *int  `json:"previousRank"`
}

// previousRank maps the statistics layer's "zero means absent" onto the
// contract's explicit null.
func previousRank(n int) *int {
	if n <= 0 {
		return nil
	}
	return &n
}

// TimelineBucket is one bucket of a timeline. Empty buckets are present with
// zeroes, so a chart never has to guess whether a gap is silence or missing data.
type TimelineBucket struct {
	Bucket          string `json:"bucket"`
	Plays           int64  `json:"plays"`
	MsPlayed        int64  `json:"msPlayed"`
	DistinctTracks  int64  `json:"distinctTracks"`
	DistinctArtists int64  `json:"distinctArtists"`
}

// TimelineResponse carries the interval actually used, which matters when the
// caller omitted it and the server chose.
type TimelineResponse struct {
	Interval string           `json:"interval"`
	Buckets  []TimelineBucket `json:"buckets"`
}

func toTimeline(points []stats.TimelinePoint) []TimelineBucket {
	out := make([]TimelineBucket, 0, len(points))
	for _, p := range points {
		out = append(out, TimelineBucket{
			Bucket:          ts(p.Bucket),
			Plays:           p.Plays,
			MsPlayed:        p.MsPlayed,
			DistinctTracks:  p.DistinctTracks,
			DistinctArtists: p.DistinctArtists,
		})
	}
	return out
}

// RepartitionBucket is one cell of a one-dimensional repartition: hour 0-23, or
// weekday 0-6 with 0 meaning Monday.
type RepartitionBucket struct {
	Key      int   `json:"key"`
	Plays    int64 `json:"plays"`
	MsPlayed int64 `json:"msPlayed"`
}

// HeatmapCell is one cell of the weekday-by-hour grid.
type HeatmapCell struct {
	Weekday  int   `json:"weekday"`
	Hour     int   `json:"hour"`
	Plays    int64 `json:"plays"`
	MsPlayed int64 `json:"msPlayed"`
}

// ListeningSession is an uninterrupted run of listening with its track list.
type ListeningSession struct {
	StartedAt  string     `json:"startedAt"`
	EndedAt    string     `json:"endedAt"`
	TrackCount int        `json:"trackCount"`
	MsPlayed   int64      `json:"msPlayed"`
	Tracks     []TrackRef `json:"tracks"`
}

// DiscoveryBucket counts first encounters in one bucket.
type DiscoveryBucket struct {
	Bucket     string `json:"bucket"`
	NewArtists int64  `json:"newArtists"`
	NewTracks  int64  `json:"newTracks"`
}

func toDiscovery(points []stats.DiscoveryPoint) []DiscoveryBucket {
	out := make([]DiscoveryBucket, 0, len(points))
	for _, p := range points {
		out = append(out, DiscoveryBucket{
			Bucket:     ts(p.Bucket),
			NewArtists: p.NewArtists,
			NewTracks:  p.NewTracks,
		})
	}
	return out
}

// Streak is a run of consecutive local days with at least one listen.
type Streak struct {
	StartDay string `json:"startDay"`
	EndDay   string `json:"endDay"`
	Days     int    `json:"days"`
}

// StreaksResponse is the caller's consistency: the run they are on, the best
// they have ever managed, and the leaderboard between them.
type StreaksResponse struct {
	Current *Streak  `json:"current"`
	Longest *Streak  `json:"longest"`
	Top     []Streak `json:"top"`
}

// toStreak renders a run, or nil for the zero value that means "there is none".
func toStreak(s domain.Streak) *Streak {
	if s.Days <= 0 {
		return nil
	}
	return &Streak{StartDay: localDay(s.StartDay), EndDay: localDay(s.EndDay), Days: s.Days}
}

// ComparePeriod is one side of a period comparison.
type ComparePeriod struct {
	From    string  `json:"from"`
	To      string  `json:"to"`
	Summary Summary `json:"summary"`
}

// CompareDelta is the movement from the first period to the second.
type CompareDelta struct {
	Listens         int64 `json:"listens"`
	MsPlayed        int64 `json:"msPlayed"`
	DistinctTracks  int64 `json:"distinctTracks"`
	DistinctArtists int64 `json:"distinctArtists"`
	DistinctAlbums  int64 `json:"distinctAlbums"`
}

// CompareResponse is two summaries and the difference between them.
type CompareResponse struct {
	A     ComparePeriod `json:"a"`
	B     ComparePeriod `json:"b"`
	Delta CompareDelta  `json:"delta"`
}

// BusiestDay is the single heaviest local day of a year.
type BusiestDay struct {
	Day      string `json:"day"`
	Plays    int64  `json:"plays"`
	MsPlayed int64  `json:"msPlayed"`
}

// YearInReview is the wrapped-style retrospective for one calendar year.
type YearInReview struct {
	Year           int                   `json:"year"`
	Summary        Summary               `json:"summary"`
	TopTracks      []TopEntry[TrackRef]  `json:"topTracks"`
	TopArtists     []TopEntry[ArtistRef] `json:"topArtists"`
	TopAlbums      []TopEntry[AlbumRef]  `json:"topAlbums"`
	BusiestDay     *BusiestDay           `json:"busiestDay"`
	LongestSession *ListeningSession     `json:"longestSession"`
	NewArtists     int64                 `json:"newArtists"`
}

// StatsExtras are the smaller dashboard figures that do not warrant a chart.
type StatsExtras struct {
	DifferentArtists        int64                    `json:"differentArtists"`
	AverageAlbumReleaseYear *float64                 `json:"averageAlbumReleaseYear"`
	AverageArtistsPerTrack  float64                  `json:"averageArtistsPerTrack"`
	AlbumsCompleted         *CompletedAlbumsResponse `json:"albumsCompleted,omitempty"`
}

// AffinityEntry is one entity two users share, with each side's play count.
type AffinityEntry[T any] struct {
	Entity T     `json:"entity"`
	PlaysA int64 `json:"playsA"`
	PlaysB int64 `json:"playsB"`
}

// AffinityResponse is how much two users of the instance have in common.
type AffinityResponse struct {
	User    PublicUser                 `json:"user"`
	Score   float64                    `json:"score"`
	Artists []AffinityEntry[ArtistRef] `json:"artists"`
	Albums  []AffinityEntry[AlbumRef]  `json:"albums"`
	Tracks  []AffinityEntry[TrackRef]  `json:"tracks"`
}

// --- library -----------------------------------------------------------

// LibrarySavedTrack is one saved track nothing in the fact table has ever
// played. It is all-time regardless of the requested range: see
// stats.LibraryStats for why narrowing the window must not change this list.
// AddedAt is null when Spotify did not report it, or the listener saved the
// track before Encore recorded that field.
type LibrarySavedTrack struct {
	Entity  TrackRef `json:"entity"`
	AddedAt *string  `json:"addedAt"`
}

// LibraryPlayedTrack is one track played inside the range that the caller has
// never saved.
type LibraryPlayedTrack struct {
	Entity   TrackRef `json:"entity"`
	Plays    int64    `json:"plays"`
	MsPlayed int64    `json:"msPlayed"`
}

// LibraryDormantArtist is one followed artist with no play inside the range.
// LastPlayedAt is the artist's last play ever, regardless of the range, so the
// client can say how long it has actually been rather than only that it has
// been a while; it is null when the artist has never been played at all.
type LibraryDormantArtist struct {
	Entity       ArtistRef `json:"entity"`
	LastPlayedAt *string   `json:"lastPlayedAt"`
}

// LibraryStatsResponse answers GET /api/stats/library: the last enumeration's
// snapshot, plus the three lists of stats.LibraryStats with every identifier
// resolved to a name and artwork.
//
// SyncedAt is null until the library worker's first successful run — the
// state of every account on an upgraded instance — and must never be
// substituted with a zero time or omitted: it is how the client tells "never
// enumerated" apart from "enumerated and found nothing".
type LibraryStatsResponse struct {
	SyncedAt        *string `json:"syncedAt"`
	SavedTracks     int64   `json:"savedTracks"`
	SavedAlbums     int64   `json:"savedAlbums"`
	FollowedArtists int64   `json:"followedArtists"`

	SavedNeverPlayed []LibrarySavedTrack    `json:"savedNeverPlayed"`
	PlayedNeverSaved []LibraryPlayedTrack   `json:"playedNeverSaved"`
	DormantFollows   []LibraryDormantArtist `json:"dormantFollows"`
}

// --- top diff ----------------------------------------------------------

// TopDiffEntry is one entity in the comparison between Spotify's own top
// ranking and Encore's, for one (kind, time range) pair.
//
// SpotifyRank and EncoreRank are null exactly when the entity is absent from
// that side, never zero: see stats.TopDiffEntry's own doc comment for why
// "absent from this side" and "tied for last place" must never collapse into
// the same wire value. Plays is Encore's own play count for the window and is
// meaningless when EncoreRank is null - Spotify's side of this comparison
// carries no play count at all, only a rank.
type TopDiffEntry[T any] struct {
	Entity      T     `json:"entity"`
	SpotifyRank *int  `json:"spotifyRank"`
	EncoreRank  *int  `json:"encoreRank"`
	Plays       int64 `json:"plays"`
}

// TopDiffResponse answers GET /api/stats/top-diff.
//
// CapturedAt is null exactly when nothing has ever been captured for this
// (kind, time range) set - see stats.TopDiff's own doc comment for why - and
// Entries is then always [], never Encore's ranking rendered on its own: a
// one-sided list would look like a comparison without being one. TimeRange
// echoes back the caller's own ?range=, which exists only so a response read
// out of a stale client-side cache entry still names the window it describes.
type TopDiffResponse[T any] struct {
	CapturedAt *string           `json:"capturedAt"`
	TimeRange  string            `json:"timeRange"`
	Entries    []TopDiffEntry[T] `json:"entries"`
}

// rankOrNil maps the statistics layer's "zero means absent from this side"
// onto the wire's explicit null, the same translation previousRank makes for
// a previous-period rank.
func rankOrNil(n int) *int {
	if n <= 0 {
		return nil
	}
	return &n
}

// toTopDiffEntries renders one comparison's entries with their entities
// resolved. Built with make and append rather than returned as a bare nil
// slice, so Entries always serialises as [] per this file's own convention
// even when the comparison has nothing to show.
func toTopDiffEntries[T any](entries []stats.TopDiffEntry, entity func(string) T) []TopDiffEntry[T] {
	out := make([]TopDiffEntry[T], 0, len(entries))
	for _, e := range entries {
		out = append(out, TopDiffEntry[T]{
			Entity:      entity(e.EntityID),
			SpotifyRank: rankOrNil(e.SpotifyRank),
			EncoreRank:  rankOrNil(e.EncoreRank),
			Plays:       e.Plays,
		})
	}
	return out
}

// EntityStats is what every detail page shows about one track, artist or album.
//
// The two pairs of timestamps answer different questions and both are sent.
// firstListenAt and lastListenAt are the first and last play *in the selected
// range*; discoveredAt and lastPlayedAt ignore the range entirely.
//
// A page that labels a figure "first listen" wants the second pair. Reading it
// from a window the viewer chose makes a track they have loved for a decade
// claim to have been discovered last month, which is what this API used to
// invite by sending only the range-scoped values.
type EntityStats struct {
	Plays         int64            `json:"plays"`
	MsPlayed      int64            `json:"msPlayed"`
	FirstListenAt *string          `json:"firstListenAt"`
	LastListenAt  *string          `json:"lastListenAt"`
	DiscoveredAt  *string          `json:"discoveredAt"`
	LastPlayedAt  *string          `json:"lastPlayedAt"`
	Timeline      []TimelineBucket `json:"timeline"`
}

func toEntityStats(e stats.EntityStats) EntityStats {
	return EntityStats{
		Plays:         e.Plays,
		MsPlayed:      e.MsPlayed,
		FirstListenAt: tsPtr(e.FirstListenAt),
		LastListenAt:  tsPtr(e.LastListenAt),
		DiscoveredAt:  tsPtr(e.DiscoveredAt),
		LastPlayedAt:  tsPtr(e.LastPlayedAt),
		Timeline:      toTimeline(e.Daily),
	}
}

// TrackDetail is the track page.
type TrackDetail struct {
	Track TrackRef    `json:"track"`
	Stats EntityStats `json:"stats"`
}

// ArtistDetail is the artist page.
type ArtistDetail struct {
	Artist          Artist               `json:"artist"`
	Stats           EntityStats          `json:"stats"`
	Share           float64              `json:"share"`
	TopTracks       []TopEntry[TrackRef] `json:"topTracks"`
	TopAlbums       []TopEntry[AlbumRef] `json:"topAlbums"`
	HourRepartition []RepartitionBucket  `json:"hourRepartition"`
	Blacklisted     bool                 `json:"blacklisted"`
}

// AlbumDetail is the album page.
type AlbumDetail struct {
	Album      Album                    `json:"album"`
	Stats      EntityStats              `json:"stats"`
	TopTracks  []TopEntry[TrackRef]     `json:"topTracks"`
	Completion *AlbumCompletionResponse `json:"completion,omitempty"`
}

// AlbumCompletionResponse is how much of an album somebody has heard, ever.
//
// Known is false when the album's track count has not been enriched yet. The
// client must render "not known yet" rather than a ratio in that case — a
// freshly imported instance is in it for nearly every album.
type AlbumCompletionResponse struct {
	Heard int64 `json:"heard"`
	Total int64 `json:"total"`
	Known bool  `json:"known"`
}

// CompletedAlbumsResponse is the range-scoped aggregate. Both numbers describe
// albums played inside the range whose track count is known.
type CompletedAlbumsResponse struct {
	Complete int64 `json:"complete"`
	Albums   int64 `json:"albums"`
}

func toAlbumCompletion(c stats.AlbumCompletion) AlbumCompletionResponse {
	return AlbumCompletionResponse{Heard: c.Heard, Total: c.Total, Known: c.Known}
}

func toCompletedAlbums(c stats.CompletedAlbums) CompletedAlbumsResponse {
	return CompletedAlbumsResponse{Complete: c.Complete, Albums: c.Albums}
}

// AlbumTrackRef is one track of an album's own listing.
//
// Deliberately not a TrackRef. These come from Spotify's listing rather than
// from the catalogue, and a track nobody has played is not in the catalogue at
// all — see migrations/00013_album_tracks.sql. Giving it the shape of a
// catalogue entity would invite a client to link to a track page that does not
// exist.
type AlbumTrackRef struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	DiscNumber  int    `json:"discNumber"`
	TrackNumber int    `json:"trackNumber"`
}

// AlbumTrackList is which tracks of an album the caller has never played.
//
// State is one of:
//
//	"ready"       — a listing is stored; Coverage and Missing mean something
//	"pending"     — no listing yet, and nothing has recorded a reason there
//	                should not be one
//	"unavailable" — no listing, and none is being read: the last attempt failed
//	"disabled"    — no listing, and this instance does not fetch them at all
//	                (ENCORE_ALBUM_TRACKS_ENABLED=false)
//
// A client MUST render all four differently, and must never read anything but
// "ready" as "you have played everything". Missing is empty in three of the
// four, which is exactly why State exists. Only "ready" with
// Coverage.Covered == Coverage.Total means the album was played in full.
//
// "pending" is deliberately not phrased as "a fetch is running": it also
// covers a lease another replica holds, no free local slot on this one, a
// shutdown in progress, and — the one that matters here — a claim against
// album_track_fetches that errored, after which nothing was read and nothing
// was recorded, so the very next request re-enters this same branch. Nothing
// in the payload bounds how long that can go on: a listing only reaches
// "pending" before anything has ever been stored, so FetchedAt is always
// absent here, and every "pending" response for one album is byte-identical
// regardless of how long the state has held. A client MUST cap how long it
// keeps polling on "pending" and render the "unavailable" copy once that cap
// is reached, rather than polling an instance whose writes are failing for as
// long as the page stays open.
//
// "disabled" is deliberately distinct from "unavailable". The first is the
// operator's choice and the second is Spotify failing to answer; a client that
// renders the failure copy for the first blames a third party for a local
// decision.
//
// A listing already cached is still served as "ready" when fetching is
// disabled, past its TTL or not — turning off fetching does not hide what is on
// disk. FetchedAt is what keeps that honest, and it is the reason there is no
// separate "this will never refresh" field: a date says how old the answer is
// without claiming anything about how fresh it is, and a second field
// expressing the same fact is a field that drifts.
//
// Coverage's denominator is the listing Spotify returned, which is not
// necessarily the album's total_tracks: those come from different reads at
// different times and can disagree. The client states which one it followed.
type AlbumTrackList struct {
	State    string           `json:"state"`
	Coverage CoverageResponse `json:"coverage"`
	// Missing is the listed tracks with no play, in disc and track order. Always
	// present and never null, so a client can iterate it without a guard; it is
	// empty both when everything was played and when there is no listing, which
	// is exactly why State exists.
	Missing []AlbumTrackRef `json:"missing"`
	// FetchedAt is when the listing was last read from Spotify, absent until one
	// has succeeded. A listing older than the TTL is still served while a refresh
	// runs, so this is what says how old the answer is.
	FetchedAt *time.Time `json:"fetchedAt,omitempty"`
}

// toAlbumTrackList diffs the listing against what the caller has played.
//
// The diff is done here rather than in SQL because the two halves come from
// different places for different reasons: the listing is global catalogue data
// cached from Spotify, and the played set is one user's own history with their
// own blacklist applied. Joining them in one statement would tie a per-user
// answer to a table that is shared between users.
func toAlbumTrackList(l albumtracks.Listing, heard []string) AlbumTrackList {
	played := make(map[string]struct{}, len(heard))
	for _, id := range heard {
		played[id] = struct{}{}
	}

	out := AlbumTrackList{
		State:   string(l.State),
		Missing: make([]AlbumTrackRef, 0, len(l.Tracks)),
	}
	for _, t := range l.Tracks {
		if _, ok := played[t.ID]; ok {
			out.Coverage.Covered++
			continue
		}
		out.Missing = append(out.Missing, AlbumTrackRef{
			ID: t.ID, Name: t.Name,
			DiscNumber: t.DiscNumber, TrackNumber: t.TrackNumber,
		})
	}
	// The denominator is the listing, not albums.total_tracks. A listener who has
	// played a track Spotify no longer lists under this album is counted in
	// neither, which is honest: this panel can only speak about the listing it
	// has.
	out.Coverage.Total = int64(len(l.Tracks))
	if !l.FetchedAt.IsZero() {
		at := l.FetchedAt.UTC()
		out.FetchedAt = &at
	}
	return out
}

// DiscographyAlbumRef is one release from an artist's own discography.
//
// Deliberately not an AlbumRef. These come from Spotify's list of what an
// artist released rather than from the catalogue, and an album nobody has played
// is not in the catalogue at all — see migrations/00014_artist_albums.sql.
// Giving it the shape of a catalogue entity would invite a client to link to an
// album page that 404s, which is precisely what most of these would do.
//
// No image: artwork would make it look more like a link than it is, and the
// listing does not need one to name a record.
type DiscographyAlbumRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// The same partial-date pair AlbumRef carries: "2016", "2016-05" or
	// "2016-05-20", with the precision beside it so a client renders only what
	// Spotify actually knew.
	ReleaseDate      *string `json:"releaseDate"`
	ReleasePrecision string  `json:"releasePrecision"`
}

// DiscographyExcluded is what coverage did *not* count.
//
// It exists so the page can say what it set aside. "You have heard 4 of 11
// albums" is true and, without this, misleading: an artist with 340 singles and
// appearances looks like an artist with 11 releases. A client MUST render these
// numbers alongside the coverage rather than treating them as diagnostics.
//
// Other is any album_group Spotify sends that is none of the four it documents.
// It is zero today and is a field rather than a silent drop so the four buckets
// plus coverage.total always account for every release stored: a group added
// upstream joins the excluded side and is counted, instead of disappearing from
// both the numerator and the sentence describing the remainder.
type DiscographyExcluded struct {
	Singles      int64 `json:"singles"`
	Compilations int64 `json:"compilations"`
	AppearsOn    int64 `json:"appearsOn"`
	Other        int64 `json:"other"`
}

// ArtistDiscography is how much of an artist's own catalogue the caller has
// played.
//
// State is one of:
//
//	"ready"       — a discography is stored; Coverage, Missing and Excluded mean
//	                something
//	"pending"     — no discography yet, and nothing has recorded a reason there
//	                should not be one
//	"unavailable" — no discography, and none is being read: the last attempt
//	                failed
//	"disabled"    — no discography, and this instance does not fetch them at all
//	                (ENCORE_ARTIST_ALBUMS_ENABLED=false)
//
// The same four words, with the same meanings, as AlbumTrackList's — pinned
// one definition since Task 4 (both alias lazyfetch.Outcome), and their wire
// values are pinned by TestTheLazyFetchStatesKeepTheirWireValues.
//
// A client MUST render all four differently, and must never read anything but
// "ready" as "you have played everything by them". Missing is empty in three of
// the four, which is exactly why State exists.
//
// **Coverage counts album_group "album" only.** Singles, compilations and
// appearances are excluded, because "you have heard 4 of 340 releases" is not a
// useful sentence — and a client that renders Coverage without also rendering
// Excluded is making a claim this payload does not support.
//
// **"Ready" with Coverage.Total == 0 is a real answer, not an empty one.** An
// artist whose every release is a single has nothing to count, and Excluded is
// then the only thing that describes them. This has no counterpart on the album
// endpoint, where an empty listing is impossible and is recorded as a failure.
//
// **Covered counts albums with any play, not albums played in full.** One track
// off a record puts it in Covered. A client must say so, or "you have heard 4 of
// their 11 albums" reads as four albums heard end to end.
//
// "pending" is deliberately not phrased as "a fetch is running": it also covers
// a lease another replica holds, no free local slot on this one, a shutdown in
// progress, and — the one that matters here — a claim against
// artist_album_fetches that errored, after which nothing was read and nothing
// was recorded, so the very next request re-enters this same branch. Nothing in
// the payload bounds how long that can go on, and every "pending" response for
// one artist is byte-identical regardless of how long the state has held. A
// client MUST cap how long it keeps polling on "pending" and render the
// "unavailable" copy once that cap is reached.
//
// "disabled" is deliberately distinct from "unavailable". The first is the
// operator's choice and the second is Spotify failing to answer; a client that
// renders the failure copy for the first blames a third party for a local
// decision.
//
// A discography already cached is still served as "ready" when fetching is
// disabled, past its TTL or not — turning off fetching does not hide what is on
// disk. FetchedAt is what keeps that honest, and it is the reason there is no
// separate "this will never refresh" field.
type ArtistDiscography struct {
	State    string           `json:"state"`
	Coverage CoverageResponse `json:"coverage"`
	// Missing is the counted albums with no play, in the order they were listed
	// — newest release first. Always present and never null, so a client can
	// iterate it without a guard; it is empty when everything was played, when
	// nothing is counted, and when there is no discography at all, which is
	// exactly why State exists.
	//
	// Unlike AlbumTrackList's Missing, which a release's own track count bounds
	// naturally, this one has no ceiling: it is bounded only by how many
	// album_group "album" releases Spotify lists for the artist, which for an
	// unusually prolific one (see defaultArtistAlbumPages's own comment) can run
	// to hundreds. There is no page size and nothing here is truncated — a long
	// list is this endpoint's actual answer for that artist, not a defect to
	// guard against, and a client should render it as a scrollable list rather
	// than a fixed-height panel.
	Missing  []DiscographyAlbumRef `json:"missing"`
	Excluded DiscographyExcluded   `json:"excluded"`
	// FetchedAt is when the discography was last read from Spotify, absent until
	// one has succeeded.
	FetchedAt *time.Time `json:"fetchedAt,omitempty"`
}

// toArtistDiscography diffs the discography against what the caller has played
// and tallies what was set aside.
//
// One pass, one classification per release: every release either counts (and is
// then Covered or Missing, never neither and never both) or lands in exactly one
// excluded bucket. That is what makes the invariant
// TestArtistDiscographyExclusionsAccountForEveryRelease asserts hold by
// construction rather than by luck.
//
// The diff is done here rather than in SQL because the two halves come from
// different places for different reasons: the discography is global catalogue
// data cached from Spotify, and the played set is one user's own history with
// their own blacklist applied.
func toArtistDiscography(d artistalbums.Discography, heard []string) ArtistDiscography {
	played := make(map[string]struct{}, len(heard))
	for _, id := range heard {
		played[id] = struct{}{}
	}

	// Missing can only ever hold the counted subset, never the whole release
	// list — a prolific artist's singles and appearances routinely outnumber
	// their albums several times over, and sizing the slice on len(d.Releases)
	// would over-allocate every response by that ratio.
	counted := 0
	for _, r := range d.Releases {
		if r.Group == artistalbums.CountedGroup {
			counted++
		}
	}

	out := ArtistDiscography{
		State:   string(d.State),
		Missing: make([]DiscographyAlbumRef, 0, counted),
	}
	for _, r := range d.Releases {
		if r.Group != artistalbums.CountedGroup {
			switch r.Group {
			case catalog.AlbumGroupSingle:
				out.Excluded.Singles++
			case catalog.AlbumGroupCompilation:
				out.Excluded.Compilations++
			case catalog.AlbumGroupAppearsOn:
				out.Excluded.AppearsOn++
			default:
				// A group Spotify documents but this build does not know, or a blank
				// one. Counted rather than dropped so the breakdown still accounts for
				// every release stored.
				out.Excluded.Other++
			}
			continue
		}
		out.Coverage.Total++
		if _, ok := played[r.AlbumID]; ok {
			out.Coverage.Covered++
			continue
		}
		out.Missing = append(out.Missing, DiscographyAlbumRef{
			ID:               r.AlbumID,
			Name:             r.Name,
			ReleaseDate:      partialDate(r.ReleaseDate, r.ReleasePrecision),
			ReleasePrecision: r.ReleasePrecision,
		})
	}
	if !d.FetchedAt.IsZero() {
		at := d.FetchedAt.UTC()
		out.FetchedAt = &at
	}
	return out
}

// partialDate renders a release date at the precision Spotify actually
// supplied, so a year-precision release does not acquire an invented first of
// January. releaseDate() above is this same rendering for a domain.Album;
// this takes the two fields directly because DiscographyAlbumRef's releases
// are not always in the catalogue at all.
func partialDate(at *time.Time, precision string) *string {
	if at == nil {
		return nil
	}
	var s string
	switch precision {
	case "year":
		s = at.Format("2006")
	case "month":
		s = at.Format("2006-01")
	default:
		s = at.Format("2006-01-02")
	}
	return &s
}

// --- genres ----------------------------------------------------------------

// CoverageResponse is the shape every partial statistic carries.
//
// One shape across every endpoint so the client renders it with one component,
// and so a reader of the JSON never has to work out which denominator a given
// percentage was taken over.
type CoverageResponse struct {
	Covered int64 `json:"covered"`
	Total   int64 `json:"total"`
}

// RateResponse is a ratio and the coverage it was computed over.
//
// Declared here rather than beside its first caller because it is this
// endpoint group's coverage shape, generalised: the taste and playback-context
// endpoints below are what actually reuse it.
type RateResponse struct {
	Value   float64 `json:"value"`
	Covered int64   `json:"covered"`
	Total   int64   `json:"total"`
}

// GenreEntry is one row of the genre ranking.
type GenreEntry struct {
	Genre    string `json:"genre"`
	Plays    int64  `json:"plays"`
	MsPlayed int64  `json:"msPlayed"`
}

// GenresResponse is one page of the ranking.
//
// Plays across genres sum to more than the range's total plays, because a track
// counts toward each of its genres. The client says so on the page.
type GenresResponse struct {
	Genres   []GenreEntry     `json:"genres"`
	Total    int64            `json:"total"`
	Coverage CoverageResponse `json:"coverage"`
}

// GenreTimelinePoint is one genre in one bucket.
type GenreTimelinePoint struct {
	Bucket   time.Time `json:"bucket"`
	Genre    string    `json:"genre"`
	Plays    int64     `json:"plays"`
	MsPlayed int64     `json:"msPlayed"`
}

// GenreTimelineResponse carries the interval so the client formats the axis
// without re-deriving it.
type GenreTimelineResponse struct {
	Interval string               `json:"interval"`
	Points   []GenreTimelinePoint `json:"points"`
}

func toCoverage(c stats.Coverage) CoverageResponse {
	return CoverageResponse{Covered: c.Covered, Total: c.Total}
}

// TasteResponse carries both scores with their own coverage.
type TasteResponse struct {
	Obscurity  RateResponse `json:"obscurity"`
	ReleaseLag RateResponse `json:"releaseLag"`
}

// ContextSliceEntry is one category of a breakdown.
type ContextSliceEntry struct {
	Key   string `json:"key"`
	Plays int64  `json:"plays"`
}

// PlaylistContextEntryResponse is one (contextType, contextId) group: what the
// listener was playing from, and how many times.
//
// Name is empty whenever user_playlists has no match for the pair — every
// album, artist and collection (Liked Songs) context always, and a playlist
// context whenever its id no longer names one of the listener's own playlists.
// That is not an error: the row is still emitted, because dropping it would
// understate the total PlaylistCoverage promises. ContextID is not a track,
// album or artist id, so it is never a candidate for resolveRefs.
type PlaylistContextEntryResponse struct {
	ContextType string `json:"contextType"`
	ContextID   string `json:"contextId"`
	Name        string `json:"name"`
	Plays       int64  `json:"plays"`
}

// PlaybackContextResponse is the whole "how you listen" payload.
//
// Every rate carries its own denominator because the underlying columns are
// written only by the extended-export importer, and an export may omit any one
// of them independently. Playlists and PlaylistCoverage are the one exception
// to that rule: context_type and context_id are written only by live sync, so
// their coverage is independent of — and typically disjoint from — the six
// export-derived figures above.
type PlaybackContextResponse struct {
	EndReasons        []ContextSliceEntry `json:"endReasons"`
	EndReasonCoverage CoverageResponse    `json:"endReasonCoverage"`
	SkipRate          RateResponse        `json:"skipRate"`
	ShuffleRate       RateResponse        `json:"shuffleRate"`
	Platforms         []ContextSliceEntry `json:"platforms"`
	PlatformCoverage  CoverageResponse    `json:"platformCoverage"`
	Countries         []ContextSliceEntry `json:"countries"`
	CountryCoverage   CoverageResponse    `json:"countryCoverage"`
	OfflineRate       RateResponse        `json:"offlineRate"`
	IncognitoRate     RateResponse        `json:"incognitoRate"`

	Playlists        []PlaylistContextEntryResponse `json:"playlists"`
	PlaylistCoverage CoverageResponse               `json:"playlistCoverage"`
}

// toRate pairs a ratio with the coverage it was computed over.
//
// It lives here rather than beside toCoverage in the previous task because
// nothing called it until now, and staticcheck (U1000) rightly refuses an
// unexported function with no call sites.
func toRate(v float64, c stats.Coverage) RateResponse {
	return RateResponse{Value: v, Covered: c.Covered, Total: c.Total}
}

func toContextSlices(in []stats.ContextSlice) []ContextSliceEntry {
	out := make([]ContextSliceEntry, 0, len(in))
	for _, s := range in {
		out = append(out, ContextSliceEntry{Key: s.Key, Plays: s.Plays})
	}
	return out
}

// toPlaylistContextEntries carries each group's ids straight through. It does
// not call resolveRefs: a context id is not always a track, album or artist
// id — a playlist is none of those three — and the name this DTO reports
// already came from user_playlists inside the statistic itself.
func toPlaylistContextEntries(in []stats.PlaylistContextEntry) []PlaylistContextEntryResponse {
	out := make([]PlaylistContextEntryResponse, 0, len(in))
	for _, e := range in {
		out = append(out, PlaylistContextEntryResponse{
			ContextType: e.ContextType,
			ContextID:   e.ContextID,
			Name:        e.Name,
			Plays:       e.Plays,
		})
	}
	return out
}

func toTaste(t stats.Taste) TasteResponse {
	return TasteResponse{
		Obscurity:  toRate(t.Obscurity, t.ObscurityCoverage),
		ReleaseLag: toRate(t.ReleaseLagYears, t.ReleaseLagCoverage),
	}
}

func toPlaybackContext(c stats.PlaybackContext) PlaybackContextResponse {
	return PlaybackContextResponse{
		EndReasons:        toContextSlices(c.EndReasons),
		EndReasonCoverage: toCoverage(c.EndReasonCoverage),
		SkipRate:          toRate(c.SkipRate, c.SkipCoverage),
		ShuffleRate:       toRate(c.ShuffleRate, c.ShuffleCoverage),
		Platforms:         toContextSlices(c.Platforms),
		PlatformCoverage:  toCoverage(c.PlatformCoverage),
		Countries:         toContextSlices(c.Countries),
		CountryCoverage:   toCoverage(c.CountryCoverage),
		OfflineRate:       toRate(c.OfflineRate, c.OfflineCoverage),
		IncognitoRate:     toRate(c.IncognitoRate, c.IncognitoCoverage),
		Playlists:         toPlaylistContextEntries(c.Playlists),
		PlaylistCoverage:  toCoverage(c.PlaylistCoverage),
	}
}

func toGenres(p stats.GenrePage) GenresResponse {
	out := GenresResponse{
		Genres:   make([]GenreEntry, 0, len(p.Genres)),
		Total:    p.Total,
		Coverage: toCoverage(p.Coverage),
	}
	for _, g := range p.Genres {
		out.Genres = append(out.Genres, GenreEntry{Genre: g.Genre, Plays: g.Plays, MsPlayed: g.MsPlayed})
	}
	return out
}

// --- listening history -----------------------------------------------------

// HistoryItem is one row of the raw listening feed.
//
// Track is null while the listen is still a names-only record awaiting alias
// resolution, in which case the alias fields carry what the export said.
type HistoryItem struct {
	ID          string    `json:"id"`
	PlayedAt    string    `json:"playedAt"`
	MsPlayed    int32     `json:"msPlayed"`
	Source      string    `json:"source"`
	Track       *TrackRef `json:"track"`
	AliasArtist *string   `json:"aliasArtist"`
	AliasTitle  *string   `json:"aliasTitle"`
}

// HistoryResponse is one keyset page of the feed.
type HistoryResponse struct {
	Items []HistoryItem `json:"items"`
	// NextCursor is opaque. Pass it back verbatim; never construct one.
	NextCursor *string `json:"nextCursor"`
	HasMore    bool    `json:"hasMore"`
}

// --- imports ---------------------------------------------------------------

// Counters are the per-file and per-job outcome tallies. Every processed record
// lands in exactly one of them.
type Counters struct {
	Imported   int64 `json:"imported"`
	Duplicates int64 `json:"duplicates"`
	Skipped    int64 `json:"skipped"`
	Rejected   int64 `json:"rejected"`
}

func toCounters(c domain.Counters) Counters {
	return Counters{Imported: c.Imported, Duplicates: c.Duplicates, Skipped: c.Skipped, Rejected: c.Rejected}
}

// ImportFile is one streaming-history file inside a job.
type ImportFile struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	ContainerPath string `json:"containerPath"`
	Format        string `json:"format"`
	Status        string `json:"status"`
	SizeBytes     int64  `json:"sizeBytes"`
	RecordsTotal  *int64 `json:"recordsTotal"`
	RecordOffset  int64  `json:"recordOffset"`
	// Pending is null while RecordsTotal is unknown, so the interface can show
	// "counting" rather than inventing a denominator.
	Pending      *int64   `json:"pending"`
	Counters     Counters `json:"counters"`
	ErrorCode    string   `json:"errorCode"`
	ErrorMessage string   `json:"errorMessage"`
	StartedAt    *string  `json:"startedAt"`
	FinishedAt   *string  `json:"finishedAt"`
}

func toImportFile(f domain.ImportFile) ImportFile {
	return ImportFile{
		ID:            f.ID.String(),
		Name:          f.Name,
		ContainerPath: f.ContainerPath,
		Format:        string(f.Format),
		Status:        string(f.Status),
		SizeBytes:     f.SizeBytes,
		RecordsTotal:  f.RecordsTotal,
		RecordOffset:  f.RecordOffset,
		Pending:       f.Pending(),
		Counters:      toCounters(f.Counters),
		ErrorCode:     f.ErrorCode,
		ErrorMessage:  f.ErrorMessage,
		StartedAt:     tsPtr(f.StartedAt),
		FinishedAt:    tsPtr(f.FinishedAt),
	}
}

// ImportJob is a user-initiated import of one or more files.
type ImportJob struct {
	ID           string       `json:"id"`
	Status       string       `json:"status"`
	Note         string       `json:"note"`
	CreatedAt    string       `json:"createdAt"`
	StartedAt    *string      `json:"startedAt"`
	FinishedAt   *string      `json:"finishedAt"`
	ErrorCode    string       `json:"errorCode"`
	ErrorMessage string       `json:"errorMessage"`
	FilesTotal   int          `json:"filesTotal"`
	FilesDone    int          `json:"filesDone"`
	Counters     Counters     `json:"counters"`
	Files        []ImportFile `json:"files"`
}

func toImportJob(j domain.ImportJob) ImportJob {
	files := make([]ImportFile, 0, len(j.Files))
	for _, f := range j.Files {
		files = append(files, toImportFile(f))
	}
	return ImportJob{
		ID:           j.ID.String(),
		Status:       string(j.Status),
		Note:         j.Note,
		CreatedAt:    ts(j.CreatedAt),
		StartedAt:    tsPtr(j.StartedAt),
		FinishedAt:   tsPtr(j.FinishedAt),
		ErrorCode:    j.ErrorCode,
		ErrorMessage: j.ErrorMessage,
		FilesTotal:   j.FilesTotal,
		FilesDone:    j.FilesDone,
		Counters:     toCounters(j.Counters),
		Files:        files,
	}
}

// ImportWarning is advice about an upload that did not stop the job being
// created — most often that the same file has been imported before.
type ImportWarning struct {
	File    string `json:"file"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

func toWarnings(ws []importer.Warning) []ImportWarning {
	out := make([]ImportWarning, 0, len(ws))
	for _, w := range ws {
		out = append(out, ImportWarning{File: w.File, Code: w.Code, Message: w.Message})
	}
	return out
}

// CreateImportResponse is the 202 an accepted upload receives.
type CreateImportResponse struct {
	Job      ImportJob       `json:"job"`
	Warnings []ImportWarning `json:"warnings"`
}

// ImportReject is one record that could never be imported, with enough context
// to understand it without reopening a multi-gigabyte export.
type ImportReject struct {
	// File names the export the record came from, since a job may hold many.
	File        string `json:"file"`
	RecordIndex int64  `json:"recordIndex"`
	Reason      string `json:"reason"`
	Detail      string `json:"detail"`
	RawExcerpt  string `json:"rawExcerpt"`
	CreatedAt   string `json:"createdAt"`
}

func toImportReject(name string, r imports.Reject) ImportReject {
	return ImportReject{
		File:        name,
		RecordIndex: r.RecordIndex,
		Reason:      string(r.Reason),
		Detail:      r.Detail,
		RawExcerpt:  r.RawExcerpt,
		CreatedAt:   ts(r.CreatedAt),
	}
}

// --- operational -----------------------------------------------------------

// HealthResponse is the body of /healthz and /readyz. Checks is present only
// when something has an opinion worth reporting.
type HealthResponse struct {
	Status string            `json:"status"`
	Checks map[string]string `json:"checks,omitempty"`
}

// --- helpers ---------------------------------------------------------------

// nonNil turns a nil slice into an empty one, so JSON carries [] and not null.
func nonNil[T any](in []T) []T {
	if in == nil {
		return []T{}
	}
	return in
}

// idString renders a listen's bigint identifier. It travels as a string because
// JavaScript numbers lose precision past 2^53 and an append-only fact table is
// exactly the place that eventually notices.
func idString(id int64) string { return strconv.FormatInt(id, 10) }

// strPtr returns a pointer to s, or nil when s is empty, for the fields whose
// contract distinguishes "absent" from "empty".
func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// SyncOutcome is what POST /api/sync/now reports back.
//
// Spotify's recently-played feed only reaches back fifty plays, so a manual sync
// usually returns mostly duplicates: the counts are shown so that "nothing new"
// reads as a normal result rather than as a failure.
type SyncOutcome struct {
	Fetched    int        `json:"fetched"`
	Imported   int        `json:"imported"`
	Duplicates int        `json:"duplicates"`
	Skipped    int        `json:"skipped"`
	NewestAt   *time.Time `json:"newestAt"`
}

// StatusResponse is the instance's operational state.
type StatusResponse struct {
	Catalogue catalog.Progress `json:"catalogue"`
	Metadata  MetadataStatus   `json:"metadata"`
}

// MetadataStatus summarises the catalogue queues into the two things a person
// actually wants to know: is there work left, and is anything stopping it.
type MetadataStatus struct {
	// Outstanding is how many catalogue entities are still queued.
	Outstanding int64 `json:"outstanding"`
	// Complete is true when nothing is left to fetch.
	Complete bool `json:"complete"`
	// Paused is true when Spotify has rate limited the whole application.
	Paused bool `json:"paused"`
	// PausedUntil is when it will resume, absent when not paused. Listening data
	// is never affected by a pause; only names, artwork and genres wait.
	PausedUntil *time.Time `json:"pausedUntil"`
	// FallbackConfigured reports that a second metadata source is set up, which
	// changes what a pause means: enrichment keeps going rather than stopping.
	//
	// It is read from this process's own configuration. The API and the worker
	// are separate containers sharing one environment, so this says the
	// deployment is configured for a fallback rather than that the worker has
	// successfully reached one.
	FallbackConfigured bool `json:"fallbackConfigured"`
}

// NowPlayingResponse is GET /api/nowplaying: what the caller is playing right
// now, as far as this instance has been able to tell.
//
// It answers three questions that are routinely conflated, and keeps them apart
// structurally rather than by convention:
//
//   - does this instance poll at all (Enabled);
//   - may it poll *this* account (ScopeGranted);
//   - has it ever managed to (Observation being non-nil).
//
// Reading Observation is therefore the only way to learn what is playing, and a
// client cannot accidentally render "nothing is playing" for an account nobody
// has looked at — there is no state value to misread, because there is no
// observation at all.
type NowPlayingResponse struct {
	// Enabled reports that this instance runs the now-playing poller.
	// ENCORE_NOWPLAYING_INTERVAL unset means false, and the client renders no
	// card at all rather than an empty one.
	Enabled bool `json:"enabled"`
	// IntervalSeconds is how often the poller checks, and therefore how often
	// it is worth asking this endpoint again. Zero when Enabled is false.
	//
	// Sent so the client polls at the instance's own rate rather than guessing
	// one: a client that polled faster than the poller would ask repeatedly for
	// an answer that cannot have changed.
	IntervalSeconds int `json:"intervalSeconds"`
	// ScopeGranted reports that this account's grant includes
	// user-read-playback-state.
	//
	// Computed on the server against the stored grant, like /api/me's
	// missingScopes and for the same reason: two copies of the required scope
	// would drift and the TypeScript one would drift silently.
	ScopeGranted bool `json:"scopeGranted"`
	// CheckedAt is when the poller last tried, successfully or not. Absent when
	// it never has.
	CheckedAt *time.Time `json:"checkedAt"`
	// Failed reports that the attempt at CheckedAt did not succeed. Observation,
	// if present, is then the last one that did — which is what lets the client
	// say how stale the display is instead of discarding a true thing.
	Failed bool `json:"failed"`
	// Observation is the last successful observation, or null when there has
	// never been one.
	//
	// Null is "Encore has not managed to look". An Observation whose State is
	// "idle" is "nothing is playing". They are different facts and must not
	// share a sentence.
	Observation *NowPlayingObservation `json:"observation"`
}

// NowPlayingObservation is one successful look at a listener's player.
type NowPlayingObservation struct {
	// ObservedAt is when everything below was true.
	ObservedAt time.Time `json:"observedAt"`
	// State is "idle", "playing" or "paused". Never "unknown": that value means
	// there was no observation, which this type's absence already says.
	State string `json:"state"`
	// Kind is "none", "track", "episode", "local" or "unknown", and decides
	// which sentence the client renders — a podcast and a local file never
	// become listens, and an advert cannot be named at all.
	Kind string `json:"kind"`
	// Title and Artist are what Spotify called it. Empty for an unknown item,
	// which carries no description by design.
	Title  string `json:"title"`
	Artist string `json:"artist"`
	// TrackID names a track in Encore's own catalogue, so the client can link
	// to it. Empty when the item is not a track, or is a track Encore has never
	// seen — a link to a page that does not exist is worse than no link.
	TrackID string `json:"trackId"`
	// ProgressMs is progress at ObservedAt and is never extrapolated. The
	// client states the observation's age beside it rather than animating a bar
	// from a fact up to one interval old.
	ProgressMs *int `json:"progressMs"`
	DurationMs *int `json:"durationMs"`
	// DeviceName is empty when Spotify reported no device, and the client then
	// renders no device clause rather than an unknown one.
	DeviceName string `json:"deviceName"`
}

// --- sharing ---------------------------------------------------------------

// CreateShareRequest is the body of POST /api/shares.
//
// Either a fixed range (from and to) or a rolling window (days), never both.
// Omitting all three shares everything.
type CreateShareRequest struct {
	Label     string     `json:"label"`
	From      *time.Time `json:"from"`
	To        *time.Time `json:"to"`
	Days      int        `json:"days"`
	ExpiresAt *time.Time `json:"expiresAt"`
}

// ShareResponse describes one link to its owner.
//
// Token and URL are populated only by the response that creates it. Afterwards
// only the hash exists, so a listing that carried a URL would be offering a link
// that cannot be reconstructed.
type ShareResponse struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	// Token and URL: present exactly once, when the link is created.
	Token string `json:"token,omitempty"`
	URL   string `json:"url,omitempty"`

	Rolling   bool       `json:"rolling"`
	RangeDays int        `json:"rangeDays"`
	From      *time.Time `json:"from"`
	To        *time.Time `json:"to"`

	ExpiresAt    *time.Time `json:"expiresAt"`
	LastViewedAt *time.Time `json:"lastViewedAt"`
	ViewCount    int64      `json:"viewCount"`
	Active       bool       `json:"active"`
	CreatedAt    time.Time  `json:"createdAt"`
}

// SharedStatsResponse is everything a shared page shows.
//
// The shape is the privacy boundary. There is no listening history here and no
// way to ask for one: a share exposes what somebody listens to, never when they
// were awake.
type SharedStatsResponse struct {
	Label       string `json:"label"`
	DisplayName string `json:"displayName"`
	AvatarURL   string `json:"avatarUrl"`
	Timezone    string `json:"timezone"`

	Rolling   bool      `json:"rolling"`
	RangeDays int       `json:"rangeDays"`
	From      time.Time `json:"from"`
	To        time.Time `json:"to"`
	Interval  string    `json:"interval"`

	// The same shapes the ordinary statistics endpoints return, so the shared
	// page renders with the components and types that already exist.
	Summary  Summary                   `json:"summary"`
	Tracks   Page[TopEntry[TrackRef]]  `json:"tracks"`
	Artists  Page[TopEntry[ArtistRef]] `json:"artists"`
	Albums   Page[TopEntry[AlbumRef]]  `json:"albums"`
	Timeline []TimelineBucket          `json:"timeline"`
	Hours    []RepartitionBucket       `json:"hours"`
	Weekdays []RepartitionBucket       `json:"weekdays"`

	// Genres and Taste are aggregate taste, the same data class as the top
	// lists above them. Playback context is deliberately absent: device and
	// country say what hardware somebody owns and where they have travelled,
	// which is not what a share is for.
	Genres *GenresResponse `json:"genres,omitempty"`
	Taste  *TasteResponse  `json:"taste,omitempty"`
}

// --- playlists --------------------------------------------------------------

// CreatePlaylistRequest is the body of POST /api/playlists.
type CreatePlaylistRequest struct {
	Name     string     `json:"name"`
	Mode     string     `json:"mode"`
	Sort     string     `json:"sort"`
	Limit    int        `json:"limit"`
	MinPlays int        `json:"minPlays"`
	From     *time.Time `json:"from"`
	To       *time.Time `json:"to"`
}

// definition turns the request into a validated recipe.
//
// Defaults are filled here rather than rejected, so a client that only knows how
// to ask for "my top 100" does not have to spell out the ranking as well.
func (b CreatePlaylistRequest) definition() (domain.PlaylistDefinition, error) {
	def := domain.PlaylistDefinition{
		Mode:     domain.PlaylistMode(b.Mode),
		Sort:     domain.PlaylistSort(b.Sort),
		Limit:    b.Limit,
		MinPlays: b.MinPlays,
	}
	if def.Sort == "" {
		def.Sort = domain.SortByPlays
	}
	if def.Limit == 0 {
		def.Limit = domain.PlaylistDefaultTracks
	}
	if def.Mode == domain.PlaylistModeMinPlays && def.MinPlays == 0 {
		def.MinPlays = domain.PlaylistDefaultMinPlays
	}
	if b.From != nil {
		def.From = b.From.UTC()
	}
	if b.To != nil {
		def.To = b.To.UTC()
	}
	if err := def.Validate(); err != nil {
		return domain.PlaylistDefinition{}, err
	}
	return def, nil
}

// RenamePlaylistRequest is the body of PATCH /api/playlists/{id}.
//
// Name is a pointer so that an absent field and an empty string are different
// requests: the first is a malformed call, the second is somebody trying to
// clear the name, and both must be refused with their own message.
//
// There is deliberately no id of any kind here. The Spotify playlist a rename
// writes to comes from the stored row, looked up by the path id and scoped to
// the caller, so no field of this body can widen what the endpoint touches.
type RenamePlaylistRequest struct {
	Name *string `json:"name"`
}

// PlaylistTrack is one track a definition selected, and why.
type PlaylistTrack struct {
	Rank     int      `json:"rank"`
	Track    TrackRef `json:"track"`
	Plays    int64    `json:"plays"`
	MsPlayed int64    `json:"msPlayed"`
}

// PlaylistPreview is what a definition would produce, without producing it.
type PlaylistPreview struct {
	// Tracks in the order they would be added.
	Tracks []PlaylistTrack `json:"tracks"`
	// Matched is how many qualified before the limit; Limit is what was asked
	// for. Together they are the difference between "100 tracks" and "100 of the
	// 412 that qualified", which is the figure that tells somebody whether their
	// limit is the right one.
	Matched int64 `json:"matched"`
	Limit   int   `json:"limit"`
}

// PlaylistCover is what happened the last time Encore tried to give this
// playlist a picture.
type PlaylistCover struct {
	// State is "none", "ready", "failed" or "unauthorised". "unauthorised" is
	// separate from "failed" because the fix is a consent journey rather than a
	// retry, and a client must not offer the same button for both.
	State string `json:"state"`
	// Kind is "mosaic" or "pattern", derived from Covered rather than stored,
	// so the two can never disagree. Empty unless State is "ready".
	Kind string `json:"kind"`
	// Covered and Total are the denominator every partial figure in Encore
	// carries. Total is always 4: the grid asks for four tiles however many
	// distinct albums the playlist happens to contain.
	Covered int `json:"covered"`
	Total   int `json:"total"`
	// Reason is why the last attempt failed, in the listener's own terms.
	// Empty unless State is "failed".
	Reason string `json:"reason"`
	// At is when State was last written. Null while State is "none".
	At *time.Time `json:"at"`
}

// Playlist is one managed playlist as its owner sees it.
type Playlist struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	SpotifyID  string     `json:"spotifyId"`
	SpotifyURL string     `json:"spotifyUrl"`
	Mode       string     `json:"mode"`
	Sort       string     `json:"sort"`
	Limit      int        `json:"limit"`
	MinPlays   int        `json:"minPlays"`
	From       *time.Time `json:"from"`
	To         *time.Time `json:"to"`
	TrackCount int        `json:"trackCount"`
	// Matched is how many tracks met the criteria before the limit applied.
	// Present only on the response that built the playlist, since it is a fact
	// about that build rather than about the definition.
	Matched int64      `json:"matched,omitempty"`
	BuiltAt *time.Time `json:"builtAt"`
	// Cover is the outcome of the last attempt to give this playlist a picture.
	// Always present: "none" is a real answer, and an absent block would leave a
	// client unable to tell a playlist nobody has asked about from one whose
	// state failed to serialise.
	Cover     PlaylistCover `json:"cover"`
	CreatedAt time.Time     `json:"createdAt"`
}

func toPlaylist(p domain.Playlist) Playlist {
	out := Playlist{
		ID:         p.ID.String(),
		Name:       p.Name,
		SpotifyID:  p.SpotifyID,
		SpotifyURL: p.SpotifyURL,
		Mode:       string(p.Definition.Mode),
		Sort:       string(p.Definition.Sort),
		Limit:      p.Definition.Limit,
		MinPlays:   p.Definition.MinPlays,
		TrackCount: p.TrackCount,
		CreatedAt:  p.CreatedAt.UTC(),
	}
	if !p.Definition.From.IsZero() {
		from := p.Definition.From.UTC()
		out.From = &from
	}
	if !p.Definition.To.IsZero() {
		to := p.Definition.To.UTC()
		out.To = &to
	}
	if !p.BuiltAt.IsZero() {
		built := p.BuiltAt.UTC()
		out.BuiltAt = &built
	}
	out.Cover = PlaylistCover{
		State:   string(p.Cover.State),
		Covered: p.Cover.Tiles,
		Total:   domain.CoverTileTotal,
		Reason:  p.Cover.Error,
	}
	if p.Cover.State == domain.CoverReady {
		out.Cover.Kind = "pattern"
		if p.Cover.Mosaic() {
			out.Cover.Kind = "mosaic"
		}
	}
	if !p.Cover.At.IsZero() {
		at := p.Cover.At.UTC()
		out.Cover.At = &at
	}
	return out
}
