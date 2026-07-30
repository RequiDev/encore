//go:build integration

package integration

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/RequiDev/encore/internal/config"
	"github.com/RequiDev/encore/internal/domain"
	"github.com/RequiDev/encore/internal/library"
	"github.com/RequiDev/encore/internal/spotify"
	libstore "github.com/RequiDev/encore/internal/store/library"
	"github.com/RequiDev/encore/test/harness"
)

// The tests in this file exercise internal/library.Worker itself, against a
// real database, with a fake Spotify client. internal/library/library_test.go
// pins the scope check, the 403 handling and the pure conversion logic
// without a database at all — see that file's reachesCommit for why — and
// what is left for here is exactly what that file cannot prove: that a
// successful run actually lands in all three tables and advances the
// watermark, and that a run which does not finish leaves both untouched.

// fakeLibraryAPI satisfies library.SpotifyAPI without a network, and counts
// how many times each endpoint was called.
type fakeLibraryAPI struct {
	tracks     []spotify.SavedTrack
	tracksErr  error
	albums     []spotify.SavedAlbum
	albumsErr  error
	artists    []spotify.Artist
	artistsErr error

	trackCalls, albumCalls, artistCalls int
}

func (f *fakeLibraryAPI) SavedTracks(context.Context, string, int) ([]spotify.SavedTrack, error) {
	f.trackCalls++
	return f.tracks, f.tracksErr
}

func (f *fakeLibraryAPI) SavedAlbums(context.Context, string, int) ([]spotify.SavedAlbum, error) {
	f.albumCalls++
	return f.albums, f.albumsErr
}

func (f *fakeLibraryAPI) FollowedArtists(context.Context, string, int) ([]spotify.Artist, error) {
	f.artistCalls++
	return f.artists, f.artistsErr
}

// fakeLibraryTokens satisfies library.Tokens with a fixed token. The real
// refresh dance belongs to *sync.Poller and is exercised by sync_test.go; the
// library worker only ever calls this method, so a fixed answer is all these
// tests need.
type fakeLibraryTokens struct{ token string }

func (f *fakeLibraryTokens) AccessToken(context.Context, uuid.UUID) (string, error) {
	return f.token, nil
}

func newLibraryWorker(t *testing.T, env *harness.Env, api library.SpotifyAPI) *library.Worker {
	t.Helper()
	w, err := library.New(config.Library{
		Enabled:     true,
		Interval:    time.Hour,
		Concurrency: 2,
		MaxPages:    200,
	}, library.Deps{
		Store:    env.Store,
		Accounts: env.Accounts,
		Catalog:  env.Catalog,
		Library:  libstore.New(env.Store),
		Spotify:  api,
		Tokens:   &fakeLibraryTokens{token: "library-access-token"},
		Logger:   harness.Discard(),
	})
	if err != nil {
		t.Fatalf("build library worker: %v", err)
	}
	return w
}

// librarySyncedAt reads the watermark directly: domain.SpotifyCredentials does
// not carry it (nothing outside scheduling and this worker needs to), so the
// column is read with a plain query instead.
func librarySyncedAt(t *testing.T, env *harness.Env, userID uuid.UUID) *time.Time {
	t.Helper()
	var at *time.Time
	if err := env.Pool.QueryRow(env.Ctx(),
		`SELECT library_synced_at FROM spotify_credentials WHERE user_id = $1`, userID.String(),
	).Scan(&at); err != nil {
		t.Fatalf("read library_synced_at: %v", err)
	}
	return at
}

func TestLibraryWorkerSyncsAllThreeTablesAndAdvancesTheWatermark(t *testing.T) {
	env := harness.New(t)
	user := env.NewUser("libworker-happy")
	connect(t, env, user.ID, time.Now().Add(time.Hour))

	album := spotify.Album{
		ID: "libw-album-1", Name: "Album One", AlbumType: "album",
		ReleaseDate: "2015-05-01", ReleaseDatePrecision: "day",
		Artists: []spotify.Artist{{ID: "libw-artist-1", Name: "Artist One"}},
	}
	api := &fakeLibraryAPI{
		tracks: []spotify.SavedTrack{
			{
				AddedAt: time.Now().Add(-time.Hour),
				Track: spotify.Track{
					ID: "libw-track-1", Name: "Track One", DurationMs: 200000,
					Album:   album,
					Artists: []spotify.Artist{{ID: "libw-artist-1", Name: "Artist One"}},
				},
			},
		},
		albums: []spotify.SavedAlbum{
			{AddedAt: time.Now().Add(-2 * time.Hour), Album: album},
		},
		artists: []spotify.Artist{
			{
				ID: "libw-artist-2", Name: "Followed Artist", Genres: []string{"indie"},
				Popularity: 10, Followers: spotify.Followers{Total: 500},
			},
		},
	}
	w := newLibraryWorker(t, env, api)

	if err := w.SyncAccount(env.Ctx(), user.ID); err != nil {
		t.Fatalf("sync account: %v", err)
	}

	if got := env.ScalarInt(
		`SELECT count(*) FROM user_saved_tracks WHERE user_id = $1`, user.ID.String()); got != 1 {
		t.Fatalf("saved tracks = %d, want 1", got)
	}
	if got := env.ScalarInt(
		`SELECT count(*) FROM user_saved_albums WHERE user_id = $1`, user.ID.String()); got != 1 {
		t.Fatalf("saved albums = %d, want 1", got)
	}
	if got := env.ScalarInt(
		`SELECT count(*) FROM user_followed_artists WHERE user_id = $1`, user.ID.String()); got != 1 {
		t.Fatalf("followed artists = %d, want 1", got)
	}

	if got := env.ScalarInt(
		`SELECT count(*) FROM tracks WHERE id = 'libw-track-1' AND metadata_state = 'resolved'`); got != 1 {
		t.Fatal("the saved track's own detail must have been upserted as resolved, since it was carried for free")
	}
	if got := env.ScalarInt(
		`SELECT count(*) FROM albums WHERE id = 'libw-album-1' AND metadata_state = 'resolved'`); got != 1 {
		t.Fatal("the album a saved track and a saved album both referenced must have been upserted as resolved")
	}
	if got := env.ScalarInt(
		`SELECT count(*) FROM artists WHERE id = 'libw-artist-2' AND metadata_state = 'resolved'`); got != 1 {
		t.Fatal("the followed artist's own full detail must have been upserted as resolved")
	}
	if got := env.ScalarInt(
		`SELECT count(*) FROM artists WHERE id = 'libw-artist-1' AND metadata_state = 'pending'`); got != 1 {
		t.Fatal("a track/album credit, carrying only a name, must be registered pending for enrichment, " +
			"not resolved with almost nothing in it")
	}

	if at := librarySyncedAt(t, env, user.ID); at == nil {
		t.Fatal("library_synced_at was never set")
	}
}

func TestLibraryWorkerForbiddenLeavesTheAccountNeverSyncedAndSyncStateUntouched(t *testing.T) {
	env := harness.New(t)
	user := env.NewUser("libworker-forbidden")
	connect(t, env, user.ID, time.Now().Add(time.Hour))

	api := &fakeLibraryAPI{
		tracks:    []spotify.SavedTrack{{Track: spotify.Track{ID: "libw-track-2", Name: "Track"}}},
		albumsErr: &spotify.APIError{StatusCode: http.StatusForbidden},
	}
	w := newLibraryWorker(t, env, api)

	if err := w.SyncAccount(env.Ctx(), user.ID); err != nil {
		t.Fatalf("sync account = %v, want nil: an optional scope Spotify still refuses is not a failure to report", err)
	}

	if at := librarySyncedAt(t, env, user.ID); at != nil {
		t.Fatalf("library_synced_at = %v, want nil: the account must read as never synced, not merely stale", at)
	}
	if got := env.ScalarInt(
		`SELECT count(*) FROM user_saved_tracks WHERE user_id = $1`, user.ID.String()); got != 0 {
		t.Fatalf("saved tracks = %d, want 0: nothing may commit when the enumeration did not finish", got)
	}

	creds, err := env.Accounts.Credentials.Get(env.Ctx(), env.Store.DB(), user.ID)
	if err != nil {
		t.Fatalf("read credentials: %v", err)
	}
	if creds.SyncState != domain.SyncStateOK {
		t.Fatalf("sync state = %q, want ok: a 403 from an optional scope must not touch "+
			"recently-played sync's own state", creds.SyncState)
	}
	if api.albumCalls != 1 {
		t.Fatalf("album calls = %d, want exactly 1: a 403 must not be retried", api.albumCalls)
	}
	if api.artistCalls != 0 {
		t.Fatalf("artist calls = %d, want 0: enumeration must stop at the forbidden endpoint", api.artistCalls)
	}
}

func TestLibraryWorkerEnumerationErrorPartWayLeavesThePreviousContentsIntact(t *testing.T) {
	env := harness.New(t)
	user := env.NewUser("libworker-partial")
	connect(t, env, user.ID, time.Now().Add(time.Hour))

	// A first, successful sync seeds the table and the watermark.
	first := &fakeLibraryAPI{
		tracks: []spotify.SavedTrack{{Track: spotify.Track{ID: "libw-track-3", Name: "Track Three"}}},
	}
	if err := newLibraryWorker(t, env, first).SyncAccount(env.Ctx(), user.ID); err != nil {
		t.Fatalf("seed sync: %v", err)
	}
	before := librarySyncedAt(t, env, user.ID)
	if before == nil {
		t.Fatal("the seeding sync did not set library_synced_at")
	}
	beforeCount := env.ScalarInt(
		`SELECT count(*) FROM user_saved_tracks WHERE user_id = $1`, user.ID.String())

	// A second run fails on the second endpoint with an ordinary error, not a
	// 403 — a network blip, say. Nothing it enumerated may reach the database.
	broken := &fakeLibraryAPI{
		tracks:    []spotify.SavedTrack{{Track: spotify.Track{ID: "libw-track-4", Name: "Track Four"}}},
		albumsErr: errors.New("connection reset by peer"),
	}
	if err := newLibraryWorker(t, env, broken).SyncAccount(env.Ctx(), user.ID); err == nil {
		t.Fatal("a failing enumeration should return an error")
	}

	after := librarySyncedAt(t, env, user.ID)
	if after == nil || !after.Equal(*before) {
		t.Fatalf("library_synced_at moved from %v to %v; a run that never committed must not advance it", before, after)
	}
	if afterCount := env.ScalarInt(
		`SELECT count(*) FROM user_saved_tracks WHERE user_id = $1`, user.ID.String()); afterCount != beforeCount {
		t.Fatalf("saved tracks changed from %d to %d rows on a run that never committed", beforeCount, afterCount)
	}
	if got := env.ScalarInt(
		`SELECT count(*) FROM user_saved_tracks WHERE user_id = $1 AND track_id = 'libw-track-4'`,
		user.ID.String()); got != 0 {
		t.Fatal("the second run's track leaked into the table despite its enumeration failing")
	}
}

func TestLibraryWorkerReplacingWithASmallerSetDeletesAbsentRows(t *testing.T) {
	env := harness.New(t)
	user := env.NewUser("libworker-shrink")
	connect(t, env, user.ID, time.Now().Add(time.Hour))

	first := &fakeLibraryAPI{tracks: []spotify.SavedTrack{
		{Track: spotify.Track{ID: "libw-shrink-1", Name: "One"}},
		{Track: spotify.Track{ID: "libw-shrink-2", Name: "Two"}},
	}}
	if err := newLibraryWorker(t, env, first).SyncAccount(env.Ctx(), user.ID); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	if got := env.ScalarInt(
		`SELECT count(*) FROM user_saved_tracks WHERE user_id = $1`, user.ID.String()); got != 2 {
		t.Fatalf("saved tracks after the first sync = %d, want 2", got)
	}

	second := &fakeLibraryAPI{tracks: []spotify.SavedTrack{
		{Track: spotify.Track{ID: "libw-shrink-1", Name: "One"}},
	}}
	if err := newLibraryWorker(t, env, second).SyncAccount(env.Ctx(), user.ID); err != nil {
		t.Fatalf("second sync: %v", err)
	}
	if got := env.ScalarInt(
		`SELECT count(*) FROM user_saved_tracks WHERE user_id = $1`, user.ID.String()); got != 1 {
		t.Fatalf("saved tracks after shrinking to one = %d, want 1", got)
	}
	if got := env.ScalarInt(
		`SELECT count(*) FROM user_saved_tracks WHERE user_id = $1 AND track_id = 'libw-shrink-2'`,
		user.ID.String()); got != 0 {
		t.Fatal("libw-shrink-2 survived even though the second sync omitted it")
	}
}

// TestListDueForLibrarySyncOrdersNullsFirstAndExcludesBrokenOrInactiveAccounts
// pins the scheduling query added to the credentials repository for this
// worker: never-enumerated accounts sort first, a recently enumerated one is
// not due yet, and an account parked for re-authorisation is excluded exactly
// as it is from recently-played sync's own queue.
func TestListDueForLibrarySyncOrdersNullsFirstAndExcludesBrokenOrInactiveAccounts(t *testing.T) {
	env := harness.New(t)

	never := env.NewUser("libdue-never")
	connect(t, env, never.ID, time.Now().Add(time.Hour))

	stale := env.NewUser("libdue-stale")
	connect(t, env, stale.ID, time.Now().Add(time.Hour))
	if err := env.Accounts.Credentials.MarkLibrarySynced(
		env.Ctx(), env.Store.DB(), stale.ID, time.Now().Add(-48*time.Hour)); err != nil {
		t.Fatalf("mark library synced: %v", err)
	}

	recent := env.NewUser("libdue-recent")
	connect(t, env, recent.ID, time.Now().Add(time.Hour))
	if err := env.Accounts.Credentials.MarkLibrarySynced(
		env.Ctx(), env.Store.DB(), recent.ID, time.Now()); err != nil {
		t.Fatalf("mark library synced: %v", err)
	}

	reauth := env.NewUser("libdue-reauth")
	connect(t, env, reauth.ID, time.Now().Add(time.Hour))
	if err := env.Accounts.Credentials.MarkNeedsReauth(env.Ctx(), env.Store.DB(), reauth.ID, "test"); err != nil {
		t.Fatalf("mark needs reauth: %v", err)
	}

	due, err := env.Accounts.Credentials.ListDueForLibrarySync(
		env.Ctx(), env.Store.DB(), time.Now().Add(-24*time.Hour), 10)
	if err != nil {
		t.Fatalf("list due for library sync: %v", err)
	}
	if len(due) < 2 {
		t.Fatalf("due = %v, want at least the never-synced and stale accounts", due)
	}
	if due[0] != never.ID {
		t.Fatalf("first due account = %s, want the never-synced account %s first", due[0], never.ID)
	}
	for _, id := range due {
		if id == recent.ID {
			t.Fatal("an account library-synced within the interval must not be due again yet")
		}
		if id == reauth.ID {
			t.Fatal("an account parked for re-authorisation must not be due for library sync either")
		}
	}
}
