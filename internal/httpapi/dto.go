package httpapi

import (
	"strconv"
	"time"

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
	if a.ReleaseDate == nil {
		return nil
	}
	var s string
	switch a.ReleasePrecision {
	case "year":
		s = a.ReleaseDate.Format("2006")
	case "month":
		s = a.ReleaseDate.Format("2006-01")
	default:
		s = a.ReleaseDate.Format("2006-01-02")
	}
	return &s
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
	DifferentArtists        int64    `json:"differentArtists"`
	AverageAlbumReleaseYear *float64 `json:"averageAlbumReleaseYear"`
	AverageArtistsPerTrack  float64  `json:"averageArtistsPerTrack"`
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
	Album     Album                `json:"album"`
	Stats     EntityStats          `json:"stats"`
	TopTracks []TopEntry[TrackRef] `json:"topTracks"`
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
	Matched   int64      `json:"matched,omitempty"`
	BuiltAt   *time.Time `json:"builtAt"`
	CreatedAt time.Time  `json:"createdAt"`
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
	return out
}
