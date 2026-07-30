package library

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"runtime"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/RequiDev/encore/internal/config"
	"github.com/RequiDev/encore/internal/domain"
	"github.com/RequiDev/encore/internal/spotify"
	"github.com/RequiDev/encore/internal/store"
	"github.com/RequiDev/encore/internal/store/accounts"
	"github.com/RequiDev/encore/internal/store/catalog"
	libstore "github.com/RequiDev/encore/internal/store/library"
)

// now is the fixed clock every test runs on, so nothing here depends on when
// it is executed.
var now = time.Date(2026, time.July, 26, 12, 0, 0, 0, time.UTC)

// fakeSpotify satisfies SpotifyAPI without a network, and counts how many
// times each endpoint was actually called. The call count is the decisive
// evidence for the scope check and the forbidden handling: both exist
// specifically so an account never spends a request it will only be refused.
type fakeSpotify struct {
	tracks     []spotify.SavedTrack
	tracksErr  error
	albums     []spotify.SavedAlbum
	albumsErr  error
	artists    []spotify.Artist
	artistsErr error

	trackCalls, albumCalls, artistCalls int
}

func (f *fakeSpotify) SavedTracks(context.Context, string, int) ([]spotify.SavedTrack, error) {
	f.trackCalls++
	return f.tracks, f.tracksErr
}

func (f *fakeSpotify) SavedAlbums(context.Context, string, int) ([]spotify.SavedAlbum, error) {
	f.albumCalls++
	return f.albums, f.albumsErr
}

func (f *fakeSpotify) FollowedArtists(context.Context, string, int) ([]spotify.Artist, error) {
	f.artistCalls++
	return f.artists, f.artistsErr
}

func (f *fakeSpotify) calls() int { return f.trackCalls + f.albumCalls + f.artistCalls }

// fakeTokens satisfies Tokens without a network or a database.
type fakeTokens struct {
	token string
	err   error
}

func (f *fakeTokens) AccessToken(context.Context, uuid.UUID) (string, error) {
	return f.token, f.err
}

// testDeps builds the minimum a Worker needs to be constructed. The
// repositories are never called by a test in this file that does not go
// through reachesCommit — see that helper for why.
func testDeps() Deps {
	db := &store.Store{}
	return Deps{
		Store:    db,
		Accounts: &accounts.Repo{Credentials: &accounts.Credentials{}},
		Catalog:  &catalog.Repo{},
		Library:  &libstore.Repo{},
		Spotify:  &fakeSpotify{},
		Tokens:   &fakeTokens{token: "token"},
		Now:      func() time.Time { return now },
	}
}

func testWorker(t *testing.T, cfg config.Library) *Worker {
	t.Helper()
	w, err := New(cfg, testDeps())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return w
}

// savedTrack builds a saved-tracks entry with a full track object, the way the
// real endpoint returns one.
func savedTrack(id, name string, addedAt time.Time) spotify.SavedTrack {
	return spotify.SavedTrack{
		AddedAt: addedAt,
		Track: spotify.Track{
			ID:      id,
			Name:    name,
			Artists: []spotify.Artist{{ID: "artist-" + id, Name: "Artist " + id}},
			Album: spotify.Album{
				ID: "album-" + id, Name: "Album " + name, AlbumType: "album",
				ReleaseDate: "2011-03-04", ReleaseDatePrecision: "day",
				Artists: []spotify.Artist{{ID: "artist-" + id, Name: "Artist " + id}},
			},
		},
	}
}

func TestNewWorkerRequiresItsCollaborators(t *testing.T) {
	tests := []struct {
		name string
		mut  func(*Deps)
	}{
		{"no store", func(d *Deps) { d.Store = nil }},
		{"no accounts", func(d *Deps) { d.Accounts = nil }},
		{"no credentials repository", func(d *Deps) { d.Accounts = &accounts.Repo{} }},
		{"no catalog", func(d *Deps) { d.Catalog = nil }},
		{"no library repository", func(d *Deps) { d.Library = nil }},
		{"no spotify client", func(d *Deps) { d.Spotify = nil }},
		{"no token source", func(d *Deps) { d.Tokens = nil }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			deps := testDeps()
			tc.mut(&deps)
			if _, err := New(config.Library{}, deps); err == nil {
				t.Fatal("expected an error when a required dependency is missing")
			}
		})
	}
}

func TestNewWorkerFillsInDefaults(t *testing.T) {
	w := testWorker(t, config.Library{})
	if w.cfg.Interval != defaultInterval {
		t.Errorf("interval = %s, want %s", w.cfg.Interval, defaultInterval)
	}
	if w.cfg.Concurrency != 1 {
		t.Errorf("concurrency = %d, want 1", w.cfg.Concurrency)
	}
	if w.cfg.MaxPages != defaultMaxPages {
		t.Errorf("max pages = %d, want %d", w.cfg.MaxPages, defaultMaxPages)
	}
	if w.log == nil || w.rnd == nil || w.now == nil {
		t.Error("worker was built with a nil collaborator")
	}
}

func TestRunReturnsImmediatelyWhenDisabled(t *testing.T) {
	w := testWorker(t, config.Library{Enabled: false, Interval: time.Hour})
	fake := &fakeSpotify{}
	w.dep.Spotify = fake

	if err := w.Run(context.Background()); err != nil {
		// A disabled worker must not block the supervisor, and must not need
		// its context cancelled to come back.
		t.Errorf("Run = %v, want nil", err)
	}
	if got := fake.calls(); got != 0 {
		t.Errorf("spotify calls = %d, want 0: a disabled worker must not issue a request", got)
	}
}

func TestRunStopsWithTheContext(t *testing.T) {
	w := testWorker(t, config.Library{Enabled: true, Interval: time.Hour, Concurrency: 1})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := w.Run(ctx); err != nil {
		t.Errorf("Run = %v, want nil on a cancelled context", err)
	}
}

func TestDelayJitter(t *testing.T) {
	const interval = 60 * time.Second
	w := testWorker(t, config.Library{Enabled: true, Interval: interval, Concurrency: 4})

	// The first delay spreads a freshly started fleet across a whole interval.
	for _, draw := range []float64{0, 0.5, 0.999} {
		w.rnd = func() float64 { return draw }
		got := w.firstDelay()
		if got < 0 || got >= interval {
			t.Errorf("firstDelay with rnd=%v = %s, want [0, %s)", draw, got, interval)
		}
	}

	// Subsequent delays stay within the jitter band, centred on the interval
	// so the long-run rate is the configured one.
	spread := time.Duration(float64(interval) * tickJitter)
	lo, hi := interval-spread/2, interval+spread/2
	w.rnd = func() float64 { return 0 }
	if got := w.nextDelay(); got != lo {
		t.Errorf("nextDelay at the bottom of the band = %s, want %s", got, lo)
	}
	w.rnd = func() float64 { return 1 }
	if got := w.nextDelay(); got != hi {
		t.Errorf("nextDelay at the top of the band = %s, want %s", got, hi)
	}
	w.rnd = func() float64 { return 0.5 }
	if got := w.nextDelay(); got != interval {
		t.Errorf("nextDelay at the middle of the band = %s, want %s", got, interval)
	}
}

func TestNextDelayNeverBusyLoops(t *testing.T) {
	w := testWorker(t, config.Library{Enabled: true, Interval: time.Millisecond})
	w.rnd = func() float64 { return 0 }
	if got := w.nextDelay(); got < time.Second {
		t.Errorf("nextDelay = %s, want at least a second whatever the interval", got)
	}
}

func TestBatchLimit(t *testing.T) {
	tests := []struct {
		concurrency int
		want        int
	}{
		{1, accountsPerWorker},
		{4, 4 * accountsPerWorker},
		{64, maxAccountsPerTick},
	}
	for _, tc := range tests {
		w := testWorker(t, config.Library{Concurrency: tc.concurrency})
		if got := w.batchLimit(); got != tc.want {
			t.Errorf("batchLimit at concurrency %d = %d, want %d", tc.concurrency, got, tc.want)
		}
	}
}

func TestSyncAccountRejectsTheNilUser(t *testing.T) {
	w := testWorker(t, config.Library{})
	if err := w.SyncAccount(context.Background(), uuid.Nil); !errors.Is(err, domain.ErrValidation) {
		t.Errorf("err = %v, want a validation error", err)
	}
}

// reachesCommit runs fn and reports whether it reached a real database write
// through Worker.commit, which testDeps() cannot back: Store.DB() and
// Store.InTx resolve to a nil connection pool there, and pgx panics reaching
// for it. Faking a pool or a repository to avoid that would be a new harness,
// which this package does not have and this task does not add; the full
// consequence — three tables written, library_synced_at advanced — is
// exercised with a real database by test/integration/library_test.go.
//
// That panic is this helper's evidence, not its bug: in this harness it is the
// only observable proof that execution actually reached the point where the
// worker starts writing the reconciled library, rather than stopping earlier
// at the scope check, a forbidden response, or some other enumeration error.
func reachesCommit(t *testing.T, fn func() error) (reached bool, err error) {
	t.Helper()
	defer func() {
		r := recover()
		if r == nil {
			return
		}
		if _, ok := r.(runtime.Error); !ok {
			panic(r) // not the database-shaped panic this helper exists to catch
		}
		reached = true
	}()
	err = fn()
	return false, err
}

func TestSyncSkipsAccountsMissingEitherScope(t *testing.T) {
	tests := []struct {
		name   string
		scopes []string
	}{
		{"no scopes at all", nil},
		{"only library-read", []string{scopeLibraryRead}},
		{"only follow-read", []string{scopeFollowRead}},
		{"an unrelated scope", []string{"user-read-email"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := testWorker(t, config.Library{})
			fake := &fakeSpotify{}
			w.dep.Spotify = fake

			reached, err := reachesCommit(t, func() error {
				return w.sync(context.Background(), uuid.New(), domain.SpotifyCredentials{Scopes: tc.scopes})
			})
			if reached {
				t.Fatal("an account missing a required scope must never reach the database write")
			}
			if err != nil {
				t.Fatalf("sync = %v, want nil for a skipped account", err)
			}
			if got := fake.calls(); got != 0 {
				t.Fatalf("spotify calls = %d, want 0: discovering a missing scope by asking must never happen", got)
			}
		})
	}
}

func TestSyncReachesCommitWhenBothScopesArePresentAndEnumerationSucceeds(t *testing.T) {
	w := testWorker(t, config.Library{})
	fake := &fakeSpotify{tracks: []spotify.SavedTrack{savedTrack("t1", "Song", now)}}
	w.dep.Spotify = fake
	creds := domain.SpotifyCredentials{Scopes: []string{scopeLibraryRead, scopeFollowRead}}

	reached, _ := reachesCommit(t, func() error {
		return w.sync(context.Background(), uuid.New(), creds)
	})
	if !reached {
		t.Fatal("a full, successful enumeration must reach the commit that writes the reconciled " +
			"library and advances library_synced_at")
	}
	if fake.trackCalls != 1 || fake.albumCalls != 1 || fake.artistCalls != 1 {
		t.Fatalf("calls = %d/%d/%d, want exactly one to each endpoint",
			fake.trackCalls, fake.albumCalls, fake.artistCalls)
	}
}

func TestSyncStopsAtAForbiddenEndpointWithoutTouchingTheAccount(t *testing.T) {
	forbiddenErr := &spotify.APIError{StatusCode: http.StatusForbidden}
	tests := []struct {
		name                                            string
		mutate                                          func(*fakeSpotify)
		wantTrackCalls, wantAlbumCalls, wantArtistCalls int
	}{
		// Each case's own endpoint must be called exactly once (not retried),
		// every endpoint before it ran normally, and every endpoint after it
		// was never reached at all.
		{"saved tracks", func(f *fakeSpotify) { f.tracksErr = forbiddenErr }, 1, 0, 0},
		{"saved albums", func(f *fakeSpotify) { f.albumsErr = forbiddenErr }, 1, 1, 0},
		{"followed artists", func(f *fakeSpotify) { f.artistsErr = forbiddenErr }, 1, 1, 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := testWorker(t, config.Library{})
			fake := &fakeSpotify{}
			tc.mutate(fake)
			w.dep.Spotify = fake
			creds := domain.SpotifyCredentials{Scopes: []string{scopeLibraryRead, scopeFollowRead}}

			reached, err := reachesCommit(t, func() error {
				return w.sync(context.Background(), uuid.New(), creds)
			})
			// This is the behaviour that looks wrong and is right: recently-played
			// sync's forbidden (internal/sync/account.go) parks the account on a
			// 403 because that scope is one Encore cannot function without. These
			// three scopes are optional, so the same status code here must do the
			// opposite — leave the account and its watermark exactly as they were.
			if reached {
				t.Fatal("a 403 despite the granted scope must not reach the database write: " +
					"SyncState and library_synced_at must stay untouched")
			}
			if err != nil {
				t.Fatalf("sync = %v, want nil: an optional scope Spotify still refuses is not a failure to report", err)
			}
			if fake.trackCalls != tc.wantTrackCalls || fake.albumCalls != tc.wantAlbumCalls || fake.artistCalls != tc.wantArtistCalls {
				t.Fatalf("calls = %d/%d/%d, want %d/%d/%d: a 403 must not be retried and must not reach a later endpoint",
					fake.trackCalls, fake.albumCalls, fake.artistCalls,
					tc.wantTrackCalls, tc.wantAlbumCalls, tc.wantArtistCalls)
			}
		})
	}
}

func TestSyncStopsWhenTheAccessTokenCannotBeObtained(t *testing.T) {
	w := testWorker(t, config.Library{})
	fake := &fakeSpotify{}
	w.dep.Spotify = fake
	w.dep.Tokens = &fakeTokens{err: errors.New("refresh failed")}
	creds := domain.SpotifyCredentials{Scopes: []string{scopeLibraryRead, scopeFollowRead}}

	reached, err := reachesCommit(t, func() error {
		return w.sync(context.Background(), uuid.New(), creds)
	})
	if reached {
		t.Fatal("a token failure must not reach the database write")
	}
	if err == nil {
		t.Fatal("a token failure must be reported, not silently ignored")
	}
	if got := fake.calls(); got != 0 {
		t.Fatalf("spotify calls = %d, want 0 when no usable token was ever obtained", got)
	}
}

// TestSyncStopsOnAGenericEnumerationErrorWithoutCallingLaterEndpoints pins the
// "enumeration error part-way" case: a non-scope failure from the second
// endpoint must not reach the third, and must not reach the database write
// that would otherwise leave the previous contents disturbed.
func TestSyncStopsOnAGenericEnumerationErrorWithoutCallingLaterEndpoints(t *testing.T) {
	w := testWorker(t, config.Library{})
	fake := &fakeSpotify{albumsErr: errors.New("connection reset by peer")}
	w.dep.Spotify = fake
	creds := domain.SpotifyCredentials{Scopes: []string{scopeLibraryRead, scopeFollowRead}}

	reached, err := reachesCommit(t, func() error {
		return w.sync(context.Background(), uuid.New(), creds)
	})
	if reached {
		t.Fatal("a failed enumeration must not reach the database write; the previous contents must stay untouched")
	}
	if err == nil {
		t.Fatal("a non-scope enumeration failure must be reported")
	}
	if fake.trackCalls != 1 || fake.albumCalls != 1 || fake.artistCalls != 0 {
		t.Fatalf("calls = %d/%d/%d, want 1/1/0: an error part-way must not continue to the next endpoint",
			fake.trackCalls, fake.albumCalls, fake.artistCalls)
	}
}

// TestSyncStopsAtATruncatedEndpointWithoutTouchingTheAccount mirrors
// TestSyncStopsAtAForbiddenEndpointWithoutTouchingTheAccount: a truncated
// enumeration — the page cap reached while Spotify still had more to give —
// must not reach the database write either. Committing a truncated result
// would run ReplaceSavedTracks/ReplaceSavedAlbums/ReplaceFollowedArtists
// against a prefix of the real set and delete every row past the cap that the
// enumeration never actually reached.
func TestSyncStopsAtATruncatedEndpointWithoutTouchingTheAccount(t *testing.T) {
	truncatedErr := fmt.Errorf("spotify: saved tracks: %w", spotify.ErrTruncated)
	tests := []struct {
		name                                            string
		mutate                                          func(*fakeSpotify)
		wantTrackCalls, wantAlbumCalls, wantArtistCalls int
	}{
		// As with the forbidden case, each endpoint before the truncated one
		// still ran, and nothing after it was ever reached.
		{"saved tracks", func(f *fakeSpotify) { f.tracksErr = truncatedErr }, 1, 0, 0},
		{"saved albums", func(f *fakeSpotify) { f.albumsErr = truncatedErr }, 1, 1, 0},
		{"followed artists", func(f *fakeSpotify) { f.artistsErr = truncatedErr }, 1, 1, 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := testWorker(t, config.Library{})
			// A truncated enumeration still returns the pages it did read, per
			// spotify.ErrTruncated's contract, so the fake carries both.
			fake := &fakeSpotify{tracks: []spotify.SavedTrack{savedTrack("t1", "Song", now)}}
			tc.mutate(fake)
			w.dep.Spotify = fake
			creds := domain.SpotifyCredentials{Scopes: []string{scopeLibraryRead, scopeFollowRead}}

			reached, err := reachesCommit(t, func() error {
				return w.sync(context.Background(), uuid.New(), creds)
			})
			if reached {
				t.Fatal("a truncated enumeration must not reach the database write: " +
					"library_synced_at and the three tables must stay untouched")
			}
			if err != nil {
				t.Fatalf("sync = %v, want nil: a truncated run is skipped, not reported as a failure", err)
			}
			if fake.trackCalls != tc.wantTrackCalls || fake.albumCalls != tc.wantAlbumCalls || fake.artistCalls != tc.wantArtistCalls {
				t.Fatalf("calls = %d/%d/%d, want %d/%d/%d: truncation must not be retried and must not reach a later endpoint",
					fake.trackCalls, fake.albumCalls, fake.artistCalls,
					tc.wantTrackCalls, tc.wantAlbumCalls, tc.wantArtistCalls)
			}
		})
	}
}

func TestTruncatedClassification(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"truncated", fmt.Errorf("spotify: saved tracks: %w", spotify.ErrTruncated), true},
		{"wrapped", errors.Join(errors.New("enumerate"), spotify.ErrTruncated), true},
		{"forbidden is not truncated", &spotify.APIError{StatusCode: http.StatusForbidden}, false},
		{"not from spotify", errors.New("database is down"), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := truncated(tc.err); got != tc.want {
				t.Errorf("truncated(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestForbiddenClassification(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"forbidden", &spotify.APIError{StatusCode: http.StatusForbidden}, true},
		{"unauthorised", &spotify.APIError{StatusCode: http.StatusUnauthorized}, false},
		{"server error", &spotify.APIError{StatusCode: http.StatusBadGateway}, false},
		{"wrapped", errors.Join(errors.New("enumerate"), &spotify.APIError{StatusCode: http.StatusForbidden}), true},
		{"not from spotify", errors.New("database is down"), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := forbidden(tc.err); got != tc.want {
				t.Errorf("forbidden(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestBuildRegistersAndResolvesSavedTracks(t *testing.T) {
	at := now.Add(-time.Hour)
	b := build([]spotify.SavedTrack{savedTrack("t1", "Song", at)}, nil, nil)

	if len(b.trackItems) != 1 || b.trackItems[0].ID != "t1" {
		t.Fatalf("track items = %+v, want one for t1", b.trackItems)
	}
	if b.trackItems[0].AddedAt == nil || !b.trackItems[0].AddedAt.Equal(at) {
		t.Fatalf("added_at = %v, want %s", b.trackItems[0].AddedAt, at)
	}
	if len(b.tracks) != 1 || b.tracks[0].ID != "t1" || b.tracks[0].Name != "Song" {
		t.Fatalf("tracks = %+v, want the full detail the saved-tracks response carried", b.tracks)
	}
	if len(b.albums) != 1 || b.albums[0].ID != "album-t1" {
		t.Fatalf("albums = %+v, want the one album t1 belongs to", b.albums)
	}
}

// TestBuildSkipsCatalogueDetailForATrackWithoutAName mirrors
// internal/sync/ingest.go's prepare: a track object without a name is not the
// full object, so it must still be registered (browsable, and left for
// enrichment) but never marked resolved with nothing in it.
func TestBuildSkipsCatalogueDetailForATrackWithoutAName(t *testing.T) {
	b := build([]spotify.SavedTrack{savedTrack("t2", "", now)}, nil, nil)

	if len(b.trackItems) != 1 {
		t.Fatalf("track items = %d, want 1 even without a name", len(b.trackItems))
	}
	if len(b.tracks) != 0 {
		t.Fatalf("tracks upserted = %d, want 0: a nameless object must not be marked resolved", len(b.tracks))
	}
}

func TestBuildDedupesAnAlbumSharedByTwoSavedTracks(t *testing.T) {
	album := spotify.Album{
		ID: "shared-album", Name: "Shared", AlbumType: "album",
		ReleaseDate: "2011-03-04", ReleaseDatePrecision: "day",
	}
	b := build([]spotify.SavedTrack{
		{Track: spotify.Track{ID: "t1", Name: "One", Album: album}},
		{Track: spotify.Track{ID: "t2", Name: "Two", Album: album}},
	}, nil, nil)

	count := 0
	for _, a := range b.albums {
		if a.ID == "shared-album" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("shared-album appeared %d times across two tracks, want 1", count)
	}
}

func TestBuildDedupesAnAlbumAcrossTracksAndSavedAlbums(t *testing.T) {
	albumDetail := spotify.Album{
		ID: "alb1", Name: "Album", AlbumType: "album",
		ReleaseDate: "2011-03-04", ReleaseDatePrecision: "day",
	}
	tracks := []spotify.SavedTrack{{Track: spotify.Track{ID: "t1", Name: "Song", Album: albumDetail}}}
	albums := []spotify.SavedAlbum{{Album: albumDetail}}

	b := build(tracks, albums, nil)

	count := 0
	for _, a := range b.albums {
		if a.ID == "alb1" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("alb1 appeared %d times, want 1 even though a saved track and a saved album both referenced it", count)
	}
	if len(b.albumItems) != 1 || b.albumItems[0].ID != "alb1" {
		t.Fatalf("album items = %+v, want exactly the one saved album", b.albumItems)
	}
}

// TestBuildSortsTracksAndAlbumsByID pins the deadlock-avoidance ordering:
// commit's loops call ReplaceTrackArtists/ReplaceAlbumArtists once per row in
// whatever order b.tracks/b.albums come back in, and two accounts sharing a
// track or album walking those rows in different orders is how concurrent
// transactions deadlock. build must hand commit a fixed order (by id) rather
// than the enumeration's added-at-desc order, which differs per account.
func TestBuildSortsTracksAndAlbumsByID(t *testing.T) {
	tracks := []spotify.SavedTrack{
		savedTrack("t9", "Nine", now),
		savedTrack("t5", "Five", now.Add(-time.Minute)),
		savedTrack("t7", "Seven", now.Add(-2*time.Minute)),
	}
	b := build(tracks, nil, nil)

	if len(b.tracks) != 3 {
		t.Fatalf("tracks = %d, want 3", len(b.tracks))
	}
	for i := 1; i < len(b.tracks); i++ {
		if b.tracks[i-1].ID >= b.tracks[i].ID {
			t.Fatalf("tracks not sorted by id: %+v", b.tracks)
		}
	}
	if b.tracks[0].ID != "t5" || b.tracks[1].ID != "t7" || b.tracks[2].ID != "t9" {
		t.Fatalf("tracks = %v, want [t5 t7 t9]", []string{b.tracks[0].ID, b.tracks[1].ID, b.tracks[2].ID})
	}

	albums := []spotify.SavedAlbum{
		{Album: spotify.Album{ID: "alb9", Name: "Nine", AlbumType: "album", ReleaseDate: "2011-03-04", ReleaseDatePrecision: "day"}},
		{Album: spotify.Album{ID: "alb5", Name: "Five", AlbumType: "album", ReleaseDate: "2011-03-04", ReleaseDatePrecision: "day"}},
		{Album: spotify.Album{ID: "alb7", Name: "Seven", AlbumType: "album", ReleaseDate: "2011-03-04", ReleaseDatePrecision: "day"}},
	}
	b2 := build(nil, albums, nil)
	if len(b2.albums) != 3 {
		t.Fatalf("albums = %d, want 3", len(b2.albums))
	}
	for i := 1; i < len(b2.albums); i++ {
		if b2.albums[i-1].ID >= b2.albums[i].ID {
			t.Fatalf("albums not sorted by id: %+v", b2.albums)
		}
	}
	if b2.albums[0].ID != "alb5" || b2.albums[1].ID != "alb7" || b2.albums[2].ID != "alb9" {
		t.Fatalf("albums = %v, want [alb5 alb7 alb9]",
			[]string{b2.albums[0].ID, b2.albums[1].ID, b2.albums[2].ID})
	}
}

// TestBuildFollowedArtistsAreUpsertedAsResolved distinguishes the followed-
// artists endpoint from the simplified artist objects embedded in a track or
// album: the dedicated endpoint returns full detail, so it earns a resolved
// row rather than merely being registered.
func TestBuildFollowedArtistsAreUpsertedAsResolved(t *testing.T) {
	full := spotify.Artist{
		ID: "art1", Name: "Artist", Genres: []string{"rock"}, Popularity: 42,
		Followers: spotify.Followers{Total: 1000},
	}
	b := build(nil, nil, []spotify.Artist{full})

	if len(b.artistIDs) != 1 || b.artistIDs[0] != "art1" {
		t.Fatalf("artist ids = %v, want [art1]", b.artistIDs)
	}
	if len(b.artists) != 1 || len(b.artists[0].Genres) != 1 || b.artists[0].Genres[0] != "rock" || b.artists[0].Followers != 1000 {
		t.Fatalf("artists = %+v, want the full detail the following endpoint returns", b.artists)
	}
}

func TestBuildRegistersAnArtistWithoutAName(t *testing.T) {
	b := build(nil, nil, []spotify.Artist{{ID: "art2"}})
	if len(b.artistIDs) != 1 {
		t.Fatalf("artist ids = %d, want 1 even without a name", len(b.artistIDs))
	}
	if len(b.artists) != 0 {
		t.Fatalf("artists upserted = %d, want 0 for a nameless object", len(b.artists))
	}
}

func TestSavedItemMapsAZeroAddedAtToNil(t *testing.T) {
	if got := savedItem("id1", time.Time{}); got.AddedAt != nil {
		t.Errorf("added_at = %v, want nil for a zero time", got.AddedAt)
	}

	got := savedItem("id1", now)
	if got.AddedAt == nil || !got.AddedAt.Equal(now) || got.AddedAt.Location() != time.UTC {
		t.Errorf("saved item = %+v, want %s in UTC", got, now)
	}
}

// spotifyClientSatisfiesTheInterface fails to compile if the real client ever
// stops matching what the worker asks of it.
var _ SpotifyAPI = (*spotify.Client)(nil)
