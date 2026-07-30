//go:build integration

package integration

import (
	"context"
	"errors"
	"fmt"
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

	// topArtists/topArtistsErr and topTracks/topTracksErr are keyed by time
	// range, mirroring internal/library/library_test.go's own fakeSpotify, so
	// a test can fail or supply data for exactly one of the six top-item
	// calls without disturbing the other five.
	topArtists    map[spotify.TopTimeRange][]spotify.Artist
	topArtistsErr map[spotify.TopTimeRange]error
	topTracks     map[spotify.TopTimeRange][]spotify.Track
	topTracksErr  map[spotify.TopTimeRange]error

	playlists    []spotify.UserPlaylist
	playlistsErr error

	trackCalls, albumCalls, artistCalls int
	topArtistCalls, topTrackCalls       int
	playlistCalls                       int
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

func (f *fakeLibraryAPI) TopArtists(_ context.Context, _ string, tr spotify.TopTimeRange, _ int) ([]spotify.Artist, error) {
	f.topArtistCalls++
	return f.topArtists[tr], f.topArtistsErr[tr]
}

func (f *fakeLibraryAPI) TopTracks(_ context.Context, _ string, tr spotify.TopTimeRange, _ int) ([]spotify.Track, error) {
	f.topTrackCalls++
	return f.topTracks[tr], f.topTracksErr[tr]
}

func (f *fakeLibraryAPI) UserPlaylists(context.Context, string, int) ([]spotify.UserPlaylist, error) {
	f.playlistCalls++
	return f.playlists, f.playlistsErr
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

// connectWithScopes is connect (test/integration/sync_test.go) with an
// explicit scope list, for a test that needs a grant narrower than, or
// otherwise different from, config.DefaultScopes().
func connectWithScopes(t *testing.T, env *harness.Env, userID uuid.UUID, expiresAt time.Time, scopes []string) {
	t.Helper()
	if err := env.Accounts.Credentials.Upsert(env.Ctx(), env.Store.DB(), domain.SpotifyCredentials{
		UserID:         userID,
		AccessToken:    "initial-access-token",
		RefreshToken:   "the-refresh-token",
		TokenExpiresAt: expiresAt,
		Scopes:         scopes,
		SyncState:      domain.SyncStateOK,
		ConnectedAt:    time.Now(),
	}); err != nil {
		t.Fatalf("connect spotify account with custom scopes: %v", err)
	}
}

// topSnapshotRow is one row read back from spotify_top_snapshots, in position
// order, for asserting a captured ranking against what the fake returned.
type topSnapshotRow struct {
	entityID   string
	capturedAt time.Time
}

func topSnapshotRows(t *testing.T, env *harness.Env, userID uuid.UUID, kind, timeRange string) []topSnapshotRow {
	t.Helper()
	rows, err := env.Pool.Query(env.Ctx(),
		`SELECT entity_id, captured_at FROM spotify_top_snapshots
		 WHERE user_id = $1 AND kind = $2 AND time_range = $3 ORDER BY position`,
		userID.String(), kind, timeRange)
	if err != nil {
		t.Fatalf("read top snapshot rows: %v", err)
	}
	defer rows.Close()
	var out []topSnapshotRow
	for rows.Next() {
		var r topSnapshotRow
		if err := rows.Scan(&r.entityID, &r.capturedAt); err != nil {
			t.Fatalf("scan top snapshot row: %v", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read top snapshot rows: %v", err)
	}
	return out
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

// TestLibraryWorkerCapturesAllSixTopSnapshotsAndAdvancesTheWatermark is the
// happy path for the six top-item enumerations added alongside the three
// library ones: every (kind, time range) set lands in spotify_top_snapshots,
// in rank order, in the same transaction as the library reconciliation and
// the watermark.
func TestLibraryWorkerCapturesAllSixTopSnapshotsAndAdvancesTheWatermark(t *testing.T) {
	env := harness.New(t)
	user := env.NewUser("libworker-topsix")
	connect(t, env, user.ID, time.Now().Add(time.Hour))

	api := &fakeLibraryAPI{
		tracks: []spotify.SavedTrack{{Track: spotify.Track{ID: "libw-top-track-0", Name: "Saved Track"}}},
		topArtists: map[spotify.TopTimeRange][]spotify.Artist{
			spotify.TopShortTerm:  {{ID: "libw-ta-1", Name: "Short Artist"}},
			spotify.TopMediumTerm: {{ID: "libw-ta-2", Name: "Medium Artist"}, {ID: "libw-ta-1", Name: "Short Artist"}},
			spotify.TopLongTerm:   {{ID: "libw-ta-3", Name: "Long Artist"}},
		},
		topTracks: map[spotify.TopTimeRange][]spotify.Track{
			spotify.TopShortTerm:  {{ID: "libw-tt-1", Name: "Short Track"}},
			spotify.TopMediumTerm: {{ID: "libw-tt-2", Name: "Medium Track"}},
			spotify.TopLongTerm:   {{ID: "libw-tt-3", Name: "Long Track"}, {ID: "libw-tt-1", Name: "Short Track"}},
		},
	}
	w := newLibraryWorker(t, env, api)

	if err := w.SyncAccount(env.Ctx(), user.ID); err != nil {
		t.Fatalf("sync account: %v", err)
	}

	if api.topArtistCalls != 3 || api.topTrackCalls != 3 {
		t.Fatalf("top calls = %d/%d, want 3/3", api.topArtistCalls, api.topTrackCalls)
	}

	// Rank order is preserved for a set with more than one entry.
	medium := topSnapshotRows(t, env, user.ID, "artist", "medium_term")
	if len(medium) != 2 || medium[0].entityID != "libw-ta-2" || medium[1].entityID != "libw-ta-1" {
		t.Fatalf("medium-term top artists = %+v, want [libw-ta-2 libw-ta-1] in that rank order", medium)
	}
	long := topSnapshotRows(t, env, user.ID, "track", "long_term")
	if len(long) != 2 || long[0].entityID != "libw-tt-3" || long[1].entityID != "libw-tt-1" {
		t.Fatalf("long-term top tracks = %+v, want [libw-tt-3 libw-tt-1] in that rank order", long)
	}
	// All six sets exist, including the two single-entry ones.
	if got := topSnapshotRows(t, env, user.ID, "artist", "short_term"); len(got) != 1 || got[0].entityID != "libw-ta-1" {
		t.Fatalf("short-term top artists = %+v, want [libw-ta-1]", got)
	}
	if got := topSnapshotRows(t, env, user.ID, "artist", "long_term"); len(got) != 1 || got[0].entityID != "libw-ta-3" {
		t.Fatalf("long-term top artists = %+v, want [libw-ta-3]", got)
	}
	if got := topSnapshotRows(t, env, user.ID, "track", "short_term"); len(got) != 1 || got[0].entityID != "libw-tt-1" {
		t.Fatalf("short-term top tracks = %+v, want [libw-tt-1]", got)
	}
	if got := topSnapshotRows(t, env, user.ID, "track", "medium_term"); len(got) != 1 || got[0].entityID != "libw-tt-2" {
		t.Fatalf("medium-term top tracks = %+v, want [libw-tt-2]", got)
	}

	// Every one of the six top-item entities was minted into the catalogue,
	// resolved since the fake returned full detail — the same path a
	// followed artist or saved track already takes.
	for _, id := range []string{"libw-ta-1", "libw-ta-2", "libw-ta-3"} {
		if got := env.ScalarInt(`SELECT count(*) FROM artists WHERE id = $1 AND metadata_state = 'resolved'`, id); got != 1 {
			t.Fatalf("top artist %s must be minted resolved, got count %d", id, got)
		}
	}
	// libw-ta-1 is a top artist in two ranges but must not have been marked
	// as followed by that alone.
	if got := env.ScalarInt(
		`SELECT count(*) FROM user_followed_artists WHERE user_id = $1 AND artist_id = 'libw-ta-1'`,
		user.ID.String()); got != 0 {
		t.Fatal("a top artist must not be recorded as followed merely for appearing in a top-artists ranking")
	}
	for _, id := range []string{"libw-tt-1", "libw-tt-2", "libw-tt-3"} {
		if got := env.ScalarInt(`SELECT count(*) FROM tracks WHERE id = $1 AND metadata_state = 'resolved'`, id); got != 1 {
			t.Fatalf("top track %s must be minted resolved, got count %d", id, got)
		}
	}
	// The saved track from the library enumeration still landed too — the
	// six top requests must not have displaced it.
	if got := env.ScalarInt(
		`SELECT count(*) FROM user_saved_tracks WHERE user_id = $1 AND track_id = 'libw-top-track-0'`,
		user.ID.String()); got != 1 {
		t.Fatal("the library enumeration's own saved track must still have been reconciled")
	}

	if at := librarySyncedAt(t, env, user.ID); at == nil {
		t.Fatal("library_synced_at was never set")
	}
}

// TestLibraryWorkerWithoutTopReadStillSyncsLibraryWithZeroTopRequests pins
// the separation the brief calls out specifically: an account that granted
// the library scopes but not user-top-read must still get its library fully
// enumerated and reconciled, spending zero requests on the six top-item
// calls. The naive reading — fold scopeTopRead into the check that already
// gates the library three — would instead skip this account's library sync
// entirely, which this test would catch.
func TestLibraryWorkerWithoutTopReadStillSyncsLibraryWithZeroTopRequests(t *testing.T) {
	env := harness.New(t)
	user := env.NewUser("libworker-notopread")
	connectWithScopes(t, env, user.ID, time.Now().Add(time.Hour),
		[]string{"user-library-read", "user-follow-read"})

	api := &fakeLibraryAPI{
		tracks: []spotify.SavedTrack{{Track: spotify.Track{ID: "libw-notop-track-1", Name: "Track"}}},
		// Present but must never be read: proof positive is api.topArtistCalls
		// and api.topTrackCalls below, not merely an empty result.
		topArtists: map[spotify.TopTimeRange][]spotify.Artist{
			spotify.TopShortTerm: {{ID: "libw-notop-artist-1", Name: "Should Not Be Fetched"}},
		},
	}
	w := newLibraryWorker(t, env, api)

	if err := w.SyncAccount(env.Ctx(), user.ID); err != nil {
		t.Fatalf("sync account = %v, want nil", err)
	}

	if api.topArtistCalls != 0 || api.topTrackCalls != 0 {
		t.Fatalf("top calls = %d/%d, want 0/0: an account without user-top-read must spend zero "+
			"requests on top items", api.topArtistCalls, api.topTrackCalls)
	}
	if got := env.ScalarInt(`SELECT count(*) FROM spotify_top_snapshots WHERE user_id = $1`, user.ID.String()); got != 0 {
		t.Fatalf("top snapshot rows = %d, want 0", got)
	}
	if got := env.ScalarInt(
		`SELECT count(*) FROM user_saved_tracks WHERE user_id = $1 AND track_id = 'libw-notop-track-1'`,
		user.ID.String()); got != 1 {
		t.Fatal("the library enumeration must still have run and reconciled in full, " +
			"despite the account lacking user-top-read")
	}
	if at := librarySyncedAt(t, env, user.ID); at == nil {
		t.Fatal("library_synced_at must still be set: the library half of the run is independent of scopeTopRead")
	}
}

// TestLibraryWorkerForbiddenOnTopCallLeavesEverythingUntouched mirrors
// TestLibraryWorkerForbiddenLeavesTheAccountNeverSyncedAndSyncStateUntouched
// for a 403 on one of the six top-item calls: per the brief, a failure on
// any of the six abandons the whole run exactly as a library failure does,
// so nothing — not even the library three, which already succeeded before
// this one failed — may reach the database.
func TestLibraryWorkerForbiddenOnTopCallLeavesEverythingUntouched(t *testing.T) {
	env := harness.New(t)
	user := env.NewUser("libworker-topforbidden")
	connect(t, env, user.ID, time.Now().Add(time.Hour))

	api := &fakeLibraryAPI{
		tracks: []spotify.SavedTrack{{Track: spotify.Track{ID: "libw-topforbid-track-1", Name: "Track"}}},
		topArtistsErr: map[spotify.TopTimeRange]error{
			spotify.TopMediumTerm: &spotify.APIError{StatusCode: http.StatusForbidden},
		},
	}
	w := newLibraryWorker(t, env, api)

	if err := w.SyncAccount(env.Ctx(), user.ID); err != nil {
		t.Fatalf("sync account = %v, want nil: an optional scope Spotify still refuses is not a failure to report", err)
	}

	if at := librarySyncedAt(t, env, user.ID); at != nil {
		t.Fatalf("library_synced_at = %v, want nil: the account must read as never synced", at)
	}
	if got := env.ScalarInt(
		`SELECT count(*) FROM user_saved_tracks WHERE user_id = $1`, user.ID.String()); got != 0 {
		t.Fatalf("saved tracks = %d, want 0: the library three must not commit even though they "+
			"already succeeded before the top-item call that failed", got)
	}
	if got := env.ScalarInt(`SELECT count(*) FROM spotify_top_snapshots WHERE user_id = $1`, user.ID.String()); got != 0 {
		t.Fatalf("top snapshot rows = %d, want 0", got)
	}
	if api.topArtistCalls != 2 {
		t.Fatalf("top artist calls = %d, want 2 (short term, then the failing medium term)", api.topArtistCalls)
	}
	if api.topTrackCalls != 0 {
		t.Fatalf("top track calls = %d, want 0: never reached after the top-artist failure", api.topTrackCalls)
	}

	creds, err := env.Accounts.Credentials.Get(env.Ctx(), env.Store.DB(), user.ID)
	if err != nil {
		t.Fatalf("read credentials: %v", err)
	}
	if creds.SyncState != domain.SyncStateOK {
		t.Fatalf("sync state = %q, want ok: a 403 from an optional scope must not touch "+
			"recently-played sync's own state", creds.SyncState)
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

// TestLibraryWorkerTruncatedEnumerationDeletesNothingAndLeavesTheWatermark
// pins the fix for the whole-branch review's truncation finding: an
// enumeration that hits its page cap while Spotify still had more to return
// (spotify.ErrTruncated) must be treated the same as a failed enumeration for
// the purposes of the database, not as a complete listing. Committing the
// partial set that fakeLibraryAPI returns here would call ReplaceSavedTracks
// against a two-item prefix of what libw-track-3's account actually has,
// deleting the row the first sync stored.
//
// Unlike a generic enumeration error, this one is not reported as a failure —
// SyncAccount returns nil, exactly as it does for a 403 on an optional scope —
// because a truncated run is skipped by design (see warnTruncated), logged at
// warn so an operator can raise ENCORE_LIBRARY_SYNC_MAX_PAGES, and retried
// wholesale on the next tick rather than surfaced as an account-level error.
func TestLibraryWorkerTruncatedEnumerationDeletesNothingAndLeavesTheWatermark(t *testing.T) {
	env := harness.New(t)
	user := env.NewUser("libworker-truncated")
	connect(t, env, user.ID, time.Now().Add(time.Hour))

	// A first, successful sync seeds the table and the watermark.
	first := &fakeLibraryAPI{
		tracks: []spotify.SavedTrack{{Track: spotify.Track{ID: "libw-track-trunc-1", Name: "Track One"}}},
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

	// A second run's saved-tracks enumeration hit its page cap: it still
	// carries the pages it did read (a different track than before, to prove
	// it did not merely no-op), wrapped in ErrTruncated.
	truncated := &fakeLibraryAPI{
		tracks: []spotify.SavedTrack{
			{Track: spotify.Track{ID: "libw-track-trunc-2", Name: "Track Two"}},
		},
		tracksErr: fmt.Errorf("spotify: saved tracks: %w", spotify.ErrTruncated),
	}
	if err := newLibraryWorker(t, env, truncated).SyncAccount(env.Ctx(), user.ID); err != nil {
		t.Fatalf("sync account = %v, want nil: a truncated enumeration is skipped, not reported as a failure", err)
	}

	after := librarySyncedAt(t, env, user.ID)
	if after == nil || !after.Equal(*before) {
		t.Fatalf("library_synced_at moved from %v to %v; a truncated run must not advance it", before, after)
	}
	if afterCount := env.ScalarInt(
		`SELECT count(*) FROM user_saved_tracks WHERE user_id = $1`, user.ID.String()); afterCount != beforeCount {
		t.Fatalf("saved tracks changed from %d to %d rows on a truncated run", beforeCount, afterCount)
	}
	if got := env.ScalarInt(
		`SELECT count(*) FROM user_saved_tracks WHERE user_id = $1 AND track_id = 'libw-track-trunc-1'`,
		user.ID.String()); got != 1 {
		t.Fatal("the first sync's track was deleted by a truncated second run that never actually saw it go missing")
	}
	if got := env.ScalarInt(
		`SELECT count(*) FROM user_saved_tracks WHERE user_id = $1 AND track_id = 'libw-track-trunc-2'`,
		user.ID.String()); got != 0 {
		t.Fatal("the truncated run's partial result leaked into the table despite being skipped")
	}
	if truncated.albumCalls != 0 || truncated.artistCalls != 0 {
		t.Fatalf("album calls = %d, artist calls = %d, want 0/0: truncation must stop enumeration at the endpoint it hit",
			truncated.albumCalls, truncated.artistCalls)
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

// userPlaylistRow reads one row back from user_playlists for the field-level
// assertions the playlist happy-path test needs beyond a plain count.
type userPlaylistRow struct {
	name        string
	ownerID     string
	snapshotID  string
	totalTracks int
	fetchedAt   time.Time
}

func readUserPlaylist(t *testing.T, env *harness.Env, userID uuid.UUID, playlistID string) userPlaylistRow {
	t.Helper()
	var r userPlaylistRow
	if err := env.Pool.QueryRow(env.Ctx(),
		`SELECT name, owner_id, snapshot_id, total_tracks, fetched_at
		 FROM user_playlists WHERE user_id = $1 AND playlist_id = $2`,
		userID.String(), playlistID,
	).Scan(&r.name, &r.ownerID, &r.snapshotID, &r.totalTracks, &r.fetchedAt); err != nil {
		t.Fatalf("read user playlist %s: %v", playlistID, err)
	}
	return r
}

// TestLibraryWorkerPlaylistsSyncAndAdvanceTheWatermark is the happy path for
// the playlist enumeration: the fetched detail lands in user_playlists in the
// same run as the library three, and library_synced_at advances.
func TestLibraryWorkerPlaylistsSyncAndAdvanceTheWatermark(t *testing.T) {
	env := harness.New(t)
	user := env.NewUser("libworker-playlists-happy")
	connect(t, env, user.ID, time.Now().Add(time.Hour))

	api := &fakeLibraryAPI{
		tracks: []spotify.SavedTrack{{Track: spotify.Track{ID: "libw-pl-track-1", Name: "Track"}}},
		playlists: []spotify.UserPlaylist{
			{ID: "libw-pl-1", Name: "Road Trip", OwnerID: "libw-pl-owner-1", SnapshotID: "libw-pl-snap-1", TotalTracks: 42},
		},
	}
	w := newLibraryWorker(t, env, api)

	if err := w.SyncAccount(env.Ctx(), user.ID); err != nil {
		t.Fatalf("sync account: %v", err)
	}

	if api.playlistCalls != 1 {
		t.Fatalf("playlist calls = %d, want 1", api.playlistCalls)
	}
	if got := env.ScalarInt(
		`SELECT count(*) FROM user_playlists WHERE user_id = $1`, user.ID.String()); got != 1 {
		t.Fatalf("playlists = %d, want 1", got)
	}
	row := readUserPlaylist(t, env, user.ID, "libw-pl-1")
	if row.name != "Road Trip" || row.ownerID != "libw-pl-owner-1" || row.snapshotID != "libw-pl-snap-1" || row.totalTracks != 42 {
		t.Fatalf("playlist row = %+v, want name/owner/snapshot/total from the fake's response", row)
	}
	// The library enumeration must still have landed too — the playlist
	// request must not have displaced it.
	if got := env.ScalarInt(
		`SELECT count(*) FROM user_saved_tracks WHERE user_id = $1`, user.ID.String()); got != 1 {
		t.Fatal("the library enumeration's own saved track must still have been reconciled")
	}
	if at := librarySyncedAt(t, env, user.ID); at == nil {
		t.Fatal("library_synced_at was never set")
	}
}

// TestLibraryWorkerWithoutPlaylistReadStillSyncsLibraryAndTopWithZeroPlaylistRequests
// pins the separation the brief calls out specifically for the third scope:
// an account that granted the library scopes and user-top-read but not
// playlist-read-private must still get its library and top items fully
// synced, spending zero requests on playlists.
func TestLibraryWorkerWithoutPlaylistReadStillSyncsLibraryAndTopWithZeroPlaylistRequests(t *testing.T) {
	env := harness.New(t)
	user := env.NewUser("libworker-noplaylistread")
	connectWithScopes(t, env, user.ID, time.Now().Add(time.Hour),
		[]string{"user-library-read", "user-follow-read", "user-top-read"})

	api := &fakeLibraryAPI{
		tracks: []spotify.SavedTrack{{Track: spotify.Track{ID: "libw-noplread-track-1", Name: "Track"}}},
		topArtists: map[spotify.TopTimeRange][]spotify.Artist{
			spotify.TopShortTerm: {{ID: "libw-noplread-artist-1", Name: "Top Artist"}},
		},
		// Present but must never be read: proof positive is api.playlistCalls
		// below, not merely an empty result.
		playlists: []spotify.UserPlaylist{{ID: "libw-noplread-playlist-1", Name: "Should Not Be Fetched"}},
	}
	w := newLibraryWorker(t, env, api)

	if err := w.SyncAccount(env.Ctx(), user.ID); err != nil {
		t.Fatalf("sync account = %v, want nil", err)
	}

	if api.playlistCalls != 0 {
		t.Fatalf("playlist calls = %d, want 0: an account without playlist-read-private must spend zero "+
			"requests on playlists", api.playlistCalls)
	}
	if got := env.ScalarInt(`SELECT count(*) FROM user_playlists WHERE user_id = $1`, user.ID.String()); got != 0 {
		t.Fatalf("playlist rows = %d, want 0", got)
	}
	if got := env.ScalarInt(
		`SELECT count(*) FROM user_saved_tracks WHERE user_id = $1 AND track_id = 'libw-noplread-track-1'`,
		user.ID.String()); got != 1 {
		t.Fatal("the library enumeration must still have run and reconciled in full")
	}
	if api.topArtistCalls != 3 || api.topTrackCalls != 3 {
		t.Fatalf("top calls = %d/%d, want 3/3: the six top-item requests must still have run "+
			"despite the missing playlist scope", api.topArtistCalls, api.topTrackCalls)
	}
	if got := env.ScalarInt(
		`SELECT count(*) FROM spotify_top_snapshots WHERE user_id = $1 AND kind = 'artist' AND time_range = 'short_term'`,
		user.ID.String()); got != 1 {
		t.Fatalf("short-term top artist rows = %d, want 1", got)
	}
	if at := librarySyncedAt(t, env, user.ID); at == nil {
		t.Fatal("library_synced_at must still be set: the library half of the run is independent of playlist-read-private")
	}
}

// TestLibraryWorkerForbiddenOnPlaylistCallLeavesEverythingUntouched mirrors
// TestLibraryWorkerForbiddenOnTopCallLeavesEverythingUntouched for the
// playlist request: a 403 abandons the whole run, so nothing — not even the
// library three, which already succeeded before this call failed — may reach
// the database.
func TestLibraryWorkerForbiddenOnPlaylistCallLeavesEverythingUntouched(t *testing.T) {
	env := harness.New(t)
	user := env.NewUser("libworker-plforbidden")
	connect(t, env, user.ID, time.Now().Add(time.Hour))

	api := &fakeLibraryAPI{
		tracks:       []spotify.SavedTrack{{Track: spotify.Track{ID: "libw-plforbid-track-1", Name: "Track"}}},
		playlistsErr: &spotify.APIError{StatusCode: http.StatusForbidden},
	}
	w := newLibraryWorker(t, env, api)

	if err := w.SyncAccount(env.Ctx(), user.ID); err != nil {
		t.Fatalf("sync account = %v, want nil: an optional scope Spotify still refuses is not a failure to report", err)
	}

	if at := librarySyncedAt(t, env, user.ID); at != nil {
		t.Fatalf("library_synced_at = %v, want nil: the account must read as never synced", at)
	}
	if got := env.ScalarInt(
		`SELECT count(*) FROM user_saved_tracks WHERE user_id = $1`, user.ID.String()); got != 0 {
		t.Fatalf("saved tracks = %d, want 0: the library three must not commit even though they "+
			"already succeeded before the playlist call that failed", got)
	}
	if got := env.ScalarInt(`SELECT count(*) FROM user_playlists WHERE user_id = $1`, user.ID.String()); got != 0 {
		t.Fatalf("playlist rows = %d, want 0", got)
	}
	if api.playlistCalls != 1 {
		t.Fatalf("playlist calls = %d, want 1: a 403 must not be retried", api.playlistCalls)
	}

	creds, err := env.Accounts.Credentials.Get(env.Ctx(), env.Store.DB(), user.ID)
	if err != nil {
		t.Fatalf("read credentials: %v", err)
	}
	if creds.SyncState != domain.SyncStateOK {
		t.Fatalf("sync state = %q, want ok: a 403 from an optional scope must not touch "+
			"recently-played sync's own state", creds.SyncState)
	}
}

// TestLibraryWorkerTruncatedPlaylistEnumerationDeletesNothingAndLeavesTheWatermark
// pins the rule the brief calls out as mattering most of all: a playlist
// enumeration that hits its page cap while Spotify still had more to return
// must not reach the delete-absent reconciliation, or a listener with many
// playlists would lose the tail on every run. Committing the partial set
// fakeLibraryAPI returns here would call ReplaceUserPlaylists against a
// one-item prefix of what the account actually has, deleting the row the
// first sync stored.
func TestLibraryWorkerTruncatedPlaylistEnumerationDeletesNothingAndLeavesTheWatermark(t *testing.T) {
	env := harness.New(t)
	user := env.NewUser("libworker-pltruncated")
	connect(t, env, user.ID, time.Now().Add(time.Hour))

	// A first, successful sync seeds the table and the watermark.
	first := &fakeLibraryAPI{
		tracks:    []spotify.SavedTrack{{Track: spotify.Track{ID: "libw-pltrunc-track-1", Name: "Track"}}},
		playlists: []spotify.UserPlaylist{{ID: "libw-pltrunc-playlist-1", Name: "Kept"}},
	}
	if err := newLibraryWorker(t, env, first).SyncAccount(env.Ctx(), user.ID); err != nil {
		t.Fatalf("seed sync: %v", err)
	}
	before := librarySyncedAt(t, env, user.ID)
	if before == nil {
		t.Fatal("the seeding sync did not set library_synced_at")
	}
	beforeCount := env.ScalarInt(`SELECT count(*) FROM user_playlists WHERE user_id = $1`, user.ID.String())

	// A second run's playlist enumeration hit its page cap: it still carries
	// the page it did read (a different playlist than before, to prove it did
	// not merely no-op), wrapped in ErrTruncated.
	truncated := &fakeLibraryAPI{
		tracks:       []spotify.SavedTrack{{Track: spotify.Track{ID: "libw-pltrunc-track-2", Name: "Track Two"}}},
		playlists:    []spotify.UserPlaylist{{ID: "libw-pltrunc-playlist-2", Name: "Should Not Land"}},
		playlistsErr: fmt.Errorf("spotify: user playlists: %w", spotify.ErrTruncated),
	}
	if err := newLibraryWorker(t, env, truncated).SyncAccount(env.Ctx(), user.ID); err != nil {
		t.Fatalf("sync account = %v, want nil: a truncated enumeration is skipped, not reported as a failure", err)
	}

	after := librarySyncedAt(t, env, user.ID)
	if after == nil || !after.Equal(*before) {
		t.Fatalf("library_synced_at moved from %v to %v; a truncated run must not advance it", before, after)
	}
	if afterCount := env.ScalarInt(
		`SELECT count(*) FROM user_playlists WHERE user_id = $1`, user.ID.String()); afterCount != beforeCount {
		t.Fatalf("playlists changed from %d to %d rows on a truncated run", beforeCount, afterCount)
	}
	if got := env.ScalarInt(
		`SELECT count(*) FROM user_playlists WHERE user_id = $1 AND playlist_id = 'libw-pltrunc-playlist-1'`,
		user.ID.String()); got != 1 {
		t.Fatal("the first sync's playlist was deleted by a truncated second run that never actually saw it go missing")
	}
	if got := env.ScalarInt(
		`SELECT count(*) FROM user_playlists WHERE user_id = $1 AND playlist_id = 'libw-pltrunc-playlist-2'`,
		user.ID.String()); got != 0 {
		t.Fatal("the truncated run's partial result leaked into the table despite being skipped")
	}
	// The library three ran normally — only the playlist call, which comes
	// after them, was truncated — but none of it may commit either, since the
	// whole run is abandoned rather than just the playlist reconciliation.
	if truncated.albumCalls != 1 || truncated.artistCalls != 1 {
		t.Fatalf("album calls = %d, artist calls = %d, want 1/1: they run before the playlist "+
			"request and must still have completed normally", truncated.albumCalls, truncated.artistCalls)
	}
	if truncated.playlistCalls != 1 {
		t.Fatalf("playlist calls = %d, want 1: truncation must not be retried", truncated.playlistCalls)
	}
	if got := env.ScalarInt(
		`SELECT count(*) FROM user_saved_tracks WHERE user_id = $1 AND track_id = 'libw-pltrunc-track-2'`,
		user.ID.String()); got != 0 {
		t.Fatal("the truncated run's own saved track leaked into the table despite the whole run being abandoned")
	}
}

// TestLibraryWorkerPlaylistScopeRevokedLeavesPreviouslyStoredPlaylistsUntouched
// covers the guard in commit that distinguishes "the grant lacks
// playlist-read-private, so nothing about playlists is known this run" from
// "the scope is present and the listener genuinely has none": a listener who
// revokes only that scope after playlists were already captured must keep
// reading the last known set, not have it silently reconciled down to zero
// the next time the library half of their account still syncs successfully.
func TestLibraryWorkerPlaylistScopeRevokedLeavesPreviouslyStoredPlaylistsUntouched(t *testing.T) {
	env := harness.New(t)
	user := env.NewUser("libworker-plrevoked")
	connect(t, env, user.ID, time.Now().Add(time.Hour))

	first := &fakeLibraryAPI{
		tracks:    []spotify.SavedTrack{{Track: spotify.Track{ID: "libw-plrevoked-track-1", Name: "Track"}}},
		playlists: []spotify.UserPlaylist{{ID: "libw-plrevoked-playlist-1", Name: "Still Here"}},
	}
	if err := newLibraryWorker(t, env, first).SyncAccount(env.Ctx(), user.ID); err != nil {
		t.Fatalf("seed sync: %v", err)
	}
	before := librarySyncedAt(t, env, user.ID)
	if before == nil {
		t.Fatal("the seeding sync did not set library_synced_at")
	}

	// The listener revokes playlist-read-private but keeps the library scopes,
	// so the account is still connected and still due for library sync — this
	// is not connectWithScopes seeding a brand new grant, it is the same
	// account's row being overwritten with a narrower one, exactly what a
	// Spotify re-consent narrowing an existing grant looks like.
	connectWithScopes(t, env, user.ID, time.Now().Add(time.Hour),
		[]string{"user-library-read", "user-follow-read"})

	second := &fakeLibraryAPI{
		tracks: []spotify.SavedTrack{{Track: spotify.Track{ID: "libw-plrevoked-track-2", Name: "Track Two"}}},
		// Present but must never be read now that the scope is gone.
		playlists: []spotify.UserPlaylist{{ID: "libw-plrevoked-playlist-2", Name: "Should Not Be Fetched"}},
	}
	if err := newLibraryWorker(t, env, second).SyncAccount(env.Ctx(), user.ID); err != nil {
		t.Fatalf("second sync = %v, want nil", err)
	}

	if second.playlistCalls != 0 {
		t.Fatalf("playlist calls = %d, want 0: the scope is gone, so the request must never be made", second.playlistCalls)
	}
	if got := env.ScalarInt(
		`SELECT count(*) FROM user_playlists WHERE user_id = $1 AND playlist_id = 'libw-plrevoked-playlist-1'`,
		user.ID.String()); got != 1 {
		t.Fatal("the previously captured playlist was deleted merely because the scope was later revoked; " +
			"a run that never asked Spotify about playlists must not reconcile that table at all")
	}
	if got := env.ScalarInt(`SELECT count(*) FROM user_playlists WHERE user_id = $1`, user.ID.String()); got != 1 {
		t.Fatalf("playlist rows = %d, want exactly the one row the first, scoped sync stored", got)
	}
	// The library half of the run is unaffected by the revoked playlist scope:
	// it must still have run and advanced the watermark past the seed.
	if got := env.ScalarInt(
		`SELECT count(*) FROM user_saved_tracks WHERE user_id = $1 AND track_id = 'libw-plrevoked-track-2'`,
		user.ID.String()); got != 1 {
		t.Fatal("the second sync's own library enumeration must still have run and reconciled")
	}
	after := librarySyncedAt(t, env, user.ID)
	if after == nil || !after.After(*before) {
		t.Fatalf("library_synced_at = %v, want an instant after the seed %v: the second sync must still have committed", after, before)
	}
}

// TestListDueForLibrarySyncOrdersNullsFirstAndExcludesBrokenOrInactiveAccounts
// pins the scheduling query added to the credentials repository for this
// worker: never-enumerated accounts sort first, a recently enumerated one is
// not due yet, an account parked for re-authorisation is excluded exactly as
// it is from recently-played sync's own queue, and an account that predates
// Phase 2a's consent change — carrying neither user-library-read nor
// user-follow-read — is excluded too, rather than sitting at the head of the
// queue forever because nothing ever advances its (permanently NULL)
// library_synced_at.
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

	// A pre-Phase-2a account: connected, active, never library-synced (so it
	// would otherwise sort first, forever, under NULLS FIRST), but its grant
	// carries none of the three read scopes this worker did not exist to ask
	// for at the time. Without the scopes predicate this row starves every
	// account that actually can sync once enough of them accumulate.
	noscope := env.NewUser("libdue-noscope")
	if err := env.Accounts.Credentials.Upsert(env.Ctx(), env.Store.DB(), domain.SpotifyCredentials{
		UserID:         noscope.ID,
		AccessToken:    "initial-access-token",
		RefreshToken:   "the-refresh-token",
		TokenExpiresAt: time.Now().Add(time.Hour),
		Scopes:         []string{"user-read-recently-played", "user-read-private", "user-read-email"},
		SyncState:      domain.SyncStateOK,
	}); err != nil {
		t.Fatalf("connect scope-less spotify account: %v", err)
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
		if id == noscope.ID {
			t.Fatal("an account missing user-library-read or user-follow-read must never be queued: " +
				"it can only be skipped, and would otherwise pin the head of the queue forever")
		}
	}
}

// TestListDueForLibrarySyncBreaksTiesByUserID pins the tiebreak added
// alongside the scopes predicate: two accounts with the identical
// library_synced_at value (the residual case where an account has both
// scopes but Spotify still 403s the endpoint every day, so the watermark
// never moves and the two ties forever) must still come back in a stable,
// rotating order rather than one permanently ahead of the other with nothing
// to break the tie.
func TestListDueForLibrarySyncBreaksTiesByUserID(t *testing.T) {
	env := harness.New(t)

	tiedAt := time.Now().Add(-48 * time.Hour)

	a := env.NewUser("libdue-tie-a")
	connect(t, env, a.ID, time.Now().Add(time.Hour))
	if err := env.Accounts.Credentials.MarkLibrarySynced(env.Ctx(), env.Store.DB(), a.ID, tiedAt); err != nil {
		t.Fatalf("mark library synced: %v", err)
	}

	b := env.NewUser("libdue-tie-b")
	connect(t, env, b.ID, time.Now().Add(time.Hour))
	if err := env.Accounts.Credentials.MarkLibrarySynced(env.Ctx(), env.Store.DB(), b.ID, tiedAt); err != nil {
		t.Fatalf("mark library synced: %v", err)
	}

	due, err := env.Accounts.Credentials.ListDueForLibrarySync(
		env.Ctx(), env.Store.DB(), time.Now().Add(-24*time.Hour), 10)
	if err != nil {
		t.Fatalf("list due for library sync: %v", err)
	}

	var ia, ib = -1, -1
	for i, id := range due {
		if id == a.ID {
			ia = i
		}
		if id == b.ID {
			ib = i
		}
	}
	if ia < 0 || ib < 0 {
		t.Fatalf("due = %v, want both tied accounts %s and %s present", due, a.ID, b.ID)
	}
	wantFirst, wantSecond := a.ID, b.ID
	if wantFirst.String() > wantSecond.String() {
		wantFirst, wantSecond = wantSecond, wantFirst
	}
	if due[min(ia, ib)] != wantFirst || due[max(ia, ib)] != wantSecond {
		t.Fatalf("tied accounts ordered %s then %s, want user_id order %s then %s",
			due[min(ia, ib)], due[max(ia, ib)], wantFirst, wantSecond)
	}
}
