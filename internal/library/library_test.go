package library

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"runtime"
	"slices"
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

	// topArtists/topArtistsErr and topTracks/topTracksErr are keyed by time
	// range, so a test can make exactly one of the six top-item calls fail or
	// return data without disturbing the other five — the six calls are not
	// otherwise distinguishable from one another by signature alone.
	topArtists    map[spotify.TopTimeRange][]spotify.Artist
	topArtistsErr map[spotify.TopTimeRange]error
	topTracks     map[spotify.TopTimeRange][]spotify.Track
	topTracksErr  map[spotify.TopTimeRange]error

	trackCalls, albumCalls, artistCalls int
	topArtistCalls, topTrackCalls       int
	// topArtistRanges and topTrackRanges record every range actually
	// requested, in call order, so a test can pin both which of the six ran
	// and in what order, not merely how many.
	topArtistRanges, topTrackRanges []spotify.TopTimeRange
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

func (f *fakeSpotify) TopArtists(_ context.Context, _ string, tr spotify.TopTimeRange, _ int) ([]spotify.Artist, error) {
	f.topArtistCalls++
	f.topArtistRanges = append(f.topArtistRanges, tr)
	return f.topArtists[tr], f.topArtistsErr[tr]
}

func (f *fakeSpotify) TopTracks(_ context.Context, _ string, tr spotify.TopTimeRange, _ int) ([]spotify.Track, error) {
	f.topTrackCalls++
	f.topTrackRanges = append(f.topTrackRanges, tr)
	return f.topTracks[tr], f.topTracksErr[tr]
}

func (f *fakeSpotify) calls() int {
	return f.trackCalls + f.albumCalls + f.artistCalls + f.topArtistCalls + f.topTrackCalls
}

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

// TestSyncSkipsAccountsMissingEitherScope pins the library-and-follow scope
// check unchanged: neither test case here grants both required scopes, so
// every one of them must be skipped before any request at all — including
// the six top-item ones.
//
// The "only top-read" case is the one this task added: it proves what the
// library-and-follow check actually does now that a third, independent scope
// exists. That check was never extended to know about scopeTopRead (see its
// own comment), so an account carrying only user-top-read still fails it and
// the whole account is skipped — the existing skip is all-or-nothing on its
// own two scopes, and granting the new one does not carve out an exception.
// This is "whatever the existing behaviour is", not a new feature: nothing
// in this task makes top items enumerable independently of the library
// scopes.
func TestSyncSkipsAccountsMissingEitherScope(t *testing.T) {
	tests := []struct {
		name   string
		scopes []string
	}{
		{"no scopes at all", nil},
		{"only library-read", []string{scopeLibraryRead}},
		{"only follow-read", []string{scopeFollowRead}},
		{"an unrelated scope", []string{"user-read-email"}},
		{"only top-read", []string{scopeTopRead}},
		{"library-read and top-read but not follow-read", []string{scopeLibraryRead, scopeTopRead}},
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
				t.Fatalf("spotify calls = %d, want 0: discovering a missing scope by asking must never happen "+
					"(including the six top-item calls, even when scopeTopRead is present)", got)
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
	// This grant carries the library scopes but not scopeTopRead: the six
	// top-item calls must not happen at all, and — this is the separation the
	// brief calls out specifically — that must not stop the library three
	// above from running and reaching commit. Folding scopeTopRead into the
	// check above, or gating the whole function on all three scopes, would
	// silently stop library sync for every account that granted less than
	// all three; this assertion is what would catch that regression.
	if fake.topArtistCalls != 0 || fake.topTrackCalls != 0 {
		t.Fatalf("top calls = %d/%d, want 0/0: an account without scopeTopRead must spend zero "+
			"requests on top items while still getting its library enumerated",
			fake.topArtistCalls, fake.topTrackCalls)
	}
}

// TestSyncEnumeratesAllSixTopRequestsAndReachesCommitWhenTopReadIsGranted is
// the happy path for the six top-item enumerations this task adds: every
// time range is requested exactly once for each kind, in the fixed order
// sync.go's topTimeRanges loop walks them, alongside the three library
// endpoints, and the whole run still reaches commit.
func TestSyncEnumeratesAllSixTopRequestsAndReachesCommitWhenTopReadIsGranted(t *testing.T) {
	w := testWorker(t, config.Library{})
	fake := &fakeSpotify{
		tracks: []spotify.SavedTrack{savedTrack("t1", "Song", now)},
		topArtists: map[spotify.TopTimeRange][]spotify.Artist{
			spotify.TopShortTerm:  {{ID: "ta-short", Name: "Short Artist"}},
			spotify.TopMediumTerm: {{ID: "ta-medium", Name: "Medium Artist"}},
			spotify.TopLongTerm:   {{ID: "ta-long", Name: "Long Artist"}},
		},
		topTracks: map[spotify.TopTimeRange][]spotify.Track{
			spotify.TopShortTerm:  {{ID: "tt-short", Name: "Short Track"}},
			spotify.TopMediumTerm: {{ID: "tt-medium", Name: "Medium Track"}},
			spotify.TopLongTerm:   {{ID: "tt-long", Name: "Long Track"}},
		},
	}
	w.dep.Spotify = fake
	creds := domain.SpotifyCredentials{Scopes: []string{scopeLibraryRead, scopeFollowRead, scopeTopRead}}

	reached, err := reachesCommit(t, func() error {
		return w.sync(context.Background(), uuid.New(), creds)
	})
	if err != nil {
		t.Fatalf("sync = %v, want nil for a fully successful run", err)
	}
	if !reached {
		t.Fatal("a full, successful run — library and all six top-item requests — must reach commit")
	}
	if fake.trackCalls != 1 || fake.albumCalls != 1 || fake.artistCalls != 1 {
		t.Fatalf("library calls = %d/%d/%d, want 1/1/1", fake.trackCalls, fake.albumCalls, fake.artistCalls)
	}
	if fake.topArtistCalls != 3 || fake.topTrackCalls != 3 {
		t.Fatalf("top calls = %d/%d, want 3/3: one request per time range per kind",
			fake.topArtistCalls, fake.topTrackCalls)
	}
	wantRanges := []spotify.TopTimeRange{spotify.TopShortTerm, spotify.TopMediumTerm, spotify.TopLongTerm}
	if !slices.Equal(fake.topArtistRanges, wantRanges) {
		t.Fatalf("top artist ranges = %v, want %v in that order", fake.topArtistRanges, wantRanges)
	}
	if !slices.Equal(fake.topTrackRanges, wantRanges) {
		t.Fatalf("top track ranges = %v, want %v in that order", fake.topTrackRanges, wantRanges)
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

// TestSyncStopsAtAForbiddenTopCallWithoutTouchingTheAccount mirrors
// TestSyncStopsAtAForbiddenEndpointWithoutTouchingTheAccount for the six
// top-item requests: a 403 from any one of them must abandon the whole run,
// exactly as a library enumeration failure does today, per the brief's rule
// that a failure on any of the six is handled identically to one of the
// three. Because the library three already succeeded by the time these run,
// this is also evidence that "abandon the run" really does mean abandoning
// everything, not merely the top half — the library calls already spent
// their request, but nothing from this run, library included, may reach
// commit.
func TestSyncStopsAtAForbiddenTopCallWithoutTouchingTheAccount(t *testing.T) {
	forbiddenErr := &spotify.APIError{StatusCode: http.StatusForbidden}
	tests := []struct {
		name                                  string
		mutate                                func(*fakeSpotify)
		wantTopArtistCalls, wantTopTrackCalls int
	}{
		// sync.go walks top artists (short, medium, long) then top tracks
		// (short, medium, long); each case's own call runs exactly once, every
		// call before it ran normally, and every call after it never happens.
		{"top artists, short term", func(f *fakeSpotify) {
			f.topArtistsErr = map[spotify.TopTimeRange]error{spotify.TopShortTerm: forbiddenErr}
		}, 1, 0},
		{"top artists, long term (the third)", func(f *fakeSpotify) {
			f.topArtistsErr = map[spotify.TopTimeRange]error{spotify.TopLongTerm: forbiddenErr}
		}, 3, 0},
		{"top tracks, medium term", func(f *fakeSpotify) {
			f.topTracksErr = map[spotify.TopTimeRange]error{spotify.TopMediumTerm: forbiddenErr}
		}, 3, 2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := testWorker(t, config.Library{})
			fake := &fakeSpotify{tracks: []spotify.SavedTrack{savedTrack("t1", "Song", now)}}
			tc.mutate(fake)
			w.dep.Spotify = fake
			creds := domain.SpotifyCredentials{Scopes: []string{scopeLibraryRead, scopeFollowRead, scopeTopRead}}

			reached, err := reachesCommit(t, func() error {
				return w.sync(context.Background(), uuid.New(), creds)
			})
			if reached {
				t.Fatal("a 403 on a top-item call must not reach the database write, even though the " +
					"three library endpoints before it already succeeded: SyncState and " +
					"library_synced_at must stay untouched, and nothing this run enumerated commits")
			}
			if err != nil {
				t.Fatalf("sync = %v, want nil: an optional scope Spotify still refuses is not a failure to report", err)
			}
			if fake.trackCalls != 1 || fake.albumCalls != 1 || fake.artistCalls != 1 {
				t.Fatalf("library calls = %d/%d/%d, want 1/1/1: the library three must still have run in full",
					fake.trackCalls, fake.albumCalls, fake.artistCalls)
			}
			if fake.topArtistCalls != tc.wantTopArtistCalls || fake.topTrackCalls != tc.wantTopTrackCalls {
				t.Fatalf("top calls = %d/%d, want %d/%d: a 403 must not be retried and must not reach a later top-item call",
					fake.topArtistCalls, fake.topTrackCalls, tc.wantTopArtistCalls, tc.wantTopTrackCalls)
			}
		})
	}
}

// TestSyncStopsOnAGenericTopCallErrorWithoutReachingCommit is
// TestSyncStopsOnAGenericEnumerationErrorWithoutCallingLaterEndpoints for a
// top-item call: an ordinary error (not a 403) from one of the six must be
// reported, not swallowed, and must still abandon the whole run.
func TestSyncStopsOnAGenericTopCallErrorWithoutReachingCommit(t *testing.T) {
	w := testWorker(t, config.Library{})
	fake := &fakeSpotify{
		tracks:        []spotify.SavedTrack{savedTrack("t1", "Song", now)},
		topArtistsErr: map[spotify.TopTimeRange]error{spotify.TopMediumTerm: errors.New("connection reset by peer")},
	}
	w.dep.Spotify = fake
	creds := domain.SpotifyCredentials{Scopes: []string{scopeLibraryRead, scopeFollowRead, scopeTopRead}}

	reached, err := reachesCommit(t, func() error {
		return w.sync(context.Background(), uuid.New(), creds)
	})
	if reached {
		t.Fatal("a failed top-item call must not reach the database write")
	}
	if err == nil {
		t.Fatal("a non-scope top-item failure must be reported, not treated like a 403")
	}
	if fake.topArtistCalls != 2 {
		t.Fatalf("top artist calls = %d, want 2 (short, then the failing medium): "+
			"an error part-way must not continue to long term or to any top track call", fake.topArtistCalls)
	}
	if fake.topTrackCalls != 0 {
		t.Fatalf("top track calls = %d, want 0: never reached after the top-artist failure", fake.topTrackCalls)
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
	b := build([]spotify.SavedTrack{savedTrack("t1", "Song", at)}, nil, nil, nil, nil)

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
	b := build([]spotify.SavedTrack{savedTrack("t2", "", now)}, nil, nil, nil, nil)

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
	}, nil, nil, nil, nil)

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

	b := build(tracks, albums, nil, nil, nil)

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
	b := build(tracks, nil, nil, nil, nil)

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
	b2 := build(nil, albums, nil, nil, nil)
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
	b := build(nil, nil, []spotify.Artist{full}, nil, nil)

	if len(b.artistIDs) != 1 || b.artistIDs[0] != "art1" {
		t.Fatalf("artist ids = %v, want [art1]", b.artistIDs)
	}
	if len(b.artists) != 1 || len(b.artists[0].Genres) != 1 || b.artists[0].Genres[0] != "rock" || b.artists[0].Followers != 1000 {
		t.Fatalf("artists = %+v, want the full detail the following endpoint returns", b.artists)
	}
}

func TestBuildRegistersAnArtistWithoutAName(t *testing.T) {
	b := build(nil, nil, []spotify.Artist{{ID: "art2"}}, nil, nil)
	if len(b.artistIDs) != 1 {
		t.Fatalf("artist ids = %d, want 1 even without a name", len(b.artistIDs))
	}
	if len(b.artists) != 0 {
		t.Fatalf("artists upserted = %d, want 0 for a nameless object", len(b.artists))
	}
}

// TestBuildMergesTopArtistsIntoCatalogueDetailButNotFollowedIDs pins the
// distinction the brief calls "reusing the same catalogue path": a top
// artist is upserted as resolved exactly like a followed artist, but being a
// listener's top artist says nothing about whether they follow them, so it
// must never appear in b.artistIDs — the set ReplaceFollowedArtists
// reconciles user_followed_artists against.
func TestBuildMergesTopArtistsIntoCatalogueDetailButNotFollowedIDs(t *testing.T) {
	topArtists := []spotify.Artist{
		{ID: "top-art-1", Name: "Top Artist", Genres: []string{"pop"}, Popularity: 77, Followers: spotify.Followers{Total: 42}},
	}
	b := build(nil, nil, nil, nil, topArtists)

	if len(b.artistIDs) != 0 {
		t.Fatalf("artist ids (followed) = %v, want none: a top artist is not a followed artist", b.artistIDs)
	}
	if len(b.artists) != 1 || b.artists[0].ID != "top-art-1" || len(b.artists[0].Genres) != 1 || b.artists[0].Followers != 42 {
		t.Fatalf("artists = %+v, want the one top artist's full detail upserted as resolved", b.artists)
	}
}

// TestBuildSkipsCatalogueDetailForATopArtistWithoutAName mirrors
// TestBuildRegistersAnArtistWithoutAName for a top artist: without a name
// there is no full object to mark resolved, so build must leave it out of
// b.artists (and, since a top artist never touches b.artistIDs at all,
// nothing here registers it either — build mints no bare row for it, per
// the package doc's note that build's only job for top items is catalogue
// detail, not a fallback minting path of its own).
func TestBuildSkipsCatalogueDetailForATopArtistWithoutAName(t *testing.T) {
	b := build(nil, nil, nil, nil, []spotify.Artist{{ID: "top-art-2"}})
	if len(b.artists) != 0 {
		t.Fatalf("artists upserted = %d, want 0 for a nameless top artist", len(b.artists))
	}
}

// TestBuildDedupesATopArtistAlreadyFollowed proves the seenArtist map is
// shared between the followed-artists loop and the top-artists loop: an
// artist a listener both follows and has as a top artist must be upserted
// once in the batch, not twice.
func TestBuildDedupesATopArtistAlreadyFollowed(t *testing.T) {
	shared := spotify.Artist{ID: "shared-artist", Name: "Shared", Genres: []string{"rock"}}
	b := build(nil, nil, []spotify.Artist{shared}, nil, []spotify.Artist{shared})

	count := 0
	for _, a := range b.artists {
		if a.ID == "shared-artist" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("shared-artist appeared %d times, want 1 even though it is both followed and a top artist", count)
	}
	// Still exactly one followed artist, from the followed-artists list alone.
	if len(b.artistIDs) != 1 || b.artistIDs[0] != "shared-artist" {
		t.Fatalf("artist ids (followed) = %v, want exactly [shared-artist]", b.artistIDs)
	}
}

// TestBuildMergesTopTracksAndTheirAlbumsIntoCatalogueDetail mirrors
// TestBuildRegistersAndResolvesSavedTracks for a top track: it is upserted
// as resolved and its album is minted too, but — unlike a saved track —
// never adds a trackItems entry, since appearing in a top-fifty says
// nothing about whether the track is saved.
func TestBuildMergesTopTracksAndTheirAlbumsIntoCatalogueDetail(t *testing.T) {
	topTracks := []spotify.Track{
		{
			ID: "top-trk-1", Name: "Top Track",
			Album: spotify.Album{
				ID: "top-alb-1", Name: "Top Album", AlbumType: "album",
				ReleaseDate: "2015-06-01", ReleaseDatePrecision: "day",
			},
		},
	}
	b := build(nil, nil, nil, topTracks, nil)

	if len(b.trackItems) != 0 {
		t.Fatalf("track items = %d, want 0: a top track is not a saved track", len(b.trackItems))
	}
	if len(b.tracks) != 1 || b.tracks[0].ID != "top-trk-1" || b.tracks[0].Name != "Top Track" {
		t.Fatalf("tracks = %+v, want the one top track's full detail upserted as resolved", b.tracks)
	}
	if len(b.albums) != 1 || b.albums[0].ID != "top-alb-1" {
		t.Fatalf("albums = %+v, want the one album the top track belongs to", b.albums)
	}
}

// TestBuildSkipsCatalogueDetailForATopTrackWithoutAName mirrors
// TestBuildSkipsCatalogueDetailForATrackWithoutAName for a top track.
func TestBuildSkipsCatalogueDetailForATopTrackWithoutAName(t *testing.T) {
	b := build(nil, nil, nil, []spotify.Track{{ID: "top-trk-2"}}, nil)
	if len(b.tracks) != 0 {
		t.Fatalf("tracks upserted = %d, want 0 for a nameless top track", len(b.tracks))
	}
	if len(b.trackItems) != 0 {
		t.Fatalf("track items = %d, want 0: a top track never adds one regardless of name", len(b.trackItems))
	}
}

// TestBuildDedupesATopTrackAcrossTimeRanges proves the same track appearing
// in more than one of the three time ranges — the ordinary case for anyone
// whose short-term favourite is also their medium-term one — is upserted
// once, not once per range: sync flattens all three ranges into one
// topTracks slice before calling build, and this is what that flattening
// relies on.
func TestBuildDedupesATopTrackAcrossTimeRanges(t *testing.T) {
	track := spotify.Track{ID: "repeat-trk", Name: "Repeat"}
	b := build(nil, nil, nil, []spotify.Track{track, track, track}, nil)

	count := 0
	for _, t := range b.tracks {
		if t.ID == "repeat-trk" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("repeat-trk appeared %d times across three time ranges, want 1", count)
	}
}

// TestBuildDedupesATopTrackAlreadySaved proves the seenTrack map is shared
// between the saved-tracks loop and the top-tracks loop, the same way
// seenArtist is shared for artists: a track that is both saved and a top
// track must be upserted once.
func TestBuildDedupesATopTrackAlreadySaved(t *testing.T) {
	shared := spotify.SavedTrack{Track: spotify.Track{ID: "shared-trk", Name: "Shared Track"}}
	b := build([]spotify.SavedTrack{shared}, nil, nil, []spotify.Track{shared.Track}, nil)

	count := 0
	for _, t := range b.tracks {
		if t.ID == "shared-trk" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("shared-trk appeared %d times, want 1 even though it is both saved and a top track", count)
	}
	if len(b.trackItems) != 1 || b.trackItems[0].ID != "shared-trk" {
		t.Fatalf("track items = %+v, want exactly the one saved track", b.trackItems)
	}
}

func TestTopArtistIDsPreservesRankOrderAndDropsBlankIDs(t *testing.T) {
	got := topArtistIDs([]spotify.Artist{{ID: "a1"}, {ID: ""}, {ID: "a2"}, {ID: "a3"}})
	want := []string{"a1", "a2", "a3"}
	if !slices.Equal(got, want) {
		t.Fatalf("topArtistIDs = %v, want %v", got, want)
	}
}

func TestTopArtistIDsDropsARepeatedIDKeepingItsFirstBestRankedOccurrence(t *testing.T) {
	got := topArtistIDs([]spotify.Artist{{ID: "a1"}, {ID: "a2"}, {ID: "a1"}, {ID: "a3"}})
	want := []string{"a1", "a2", "a3"}
	if !slices.Equal(got, want) {
		t.Fatalf("topArtistIDs = %v, want %v: a repeated id must collapse to its first, "+
			"best-ranked position rather than produce a second row ReplaceTopSnapshot's "+
			"position-keyed primary key would not otherwise deduplicate", got, want)
	}
}

func TestTopArtistIDsOfAnEmptyRankingIsEmptyNotNil(t *testing.T) {
	got := topArtistIDs(nil)
	if got == nil {
		t.Fatal("topArtistIDs(nil) = nil, want a non-nil empty slice: ReplaceTopSnapshot treats " +
			"an empty entityIDs as a real, capturable ranking (a brand-new account), and a nil " +
			"slice must not encode differently than an empty one at the SQL layer")
	}
	if len(got) != 0 {
		t.Fatalf("topArtistIDs(nil) = %v, want empty", got)
	}
}

func TestTopTrackIDsPreservesRankOrderAndDropsBlankIDs(t *testing.T) {
	got := topTrackIDs([]spotify.Track{{ID: "t1"}, {ID: ""}, {ID: "t2"}, {ID: "t3"}})
	want := []string{"t1", "t2", "t3"}
	if !slices.Equal(got, want) {
		t.Fatalf("topTrackIDs = %v, want %v", got, want)
	}
}

func TestTopTrackIDsDropsARepeatedIDKeepingItsFirstBestRankedOccurrence(t *testing.T) {
	got := topTrackIDs([]spotify.Track{{ID: "t1"}, {ID: "t2"}, {ID: "t1"}, {ID: "t3"}})
	want := []string{"t1", "t2", "t3"}
	if !slices.Equal(got, want) {
		t.Fatalf("topTrackIDs = %v, want %v: a repeated id must collapse to its first, "+
			"best-ranked position rather than produce a second row ReplaceTopSnapshot's "+
			"position-keyed primary key would not otherwise deduplicate", got, want)
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
