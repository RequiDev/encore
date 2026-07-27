//go:build integration

package integration

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/RequiDev/encore/internal/config"
	"github.com/RequiDev/encore/internal/domain"
	"github.com/RequiDev/encore/internal/spotify"
	encoresync "github.com/RequiDev/encore/internal/sync"
	"github.com/RequiDev/encore/test/harness"
)

// fakeRecentlyPlayed stands in for the recently-played endpoint and the token
// endpoint. It is an interface implementation rather than an HTTP server,
// because what these tests examine is the poller's own logic — the watermark,
// the refresh path, the reauthorisation path — not JSON decoding, which the
// spotify package's own tests already cover.
type fakeRecentlyPlayed struct {
	pages [][]spotify.PlayHistory
	calls atomic.Int32
	// lastAfter records the cursor the poller asked for.
	lastAfter atomic.Value

	// failWith, when set, is returned instead of a page.
	failWith error
	// unauthorisedOnce makes the first call fail with 401, so the refresh path
	// is exercised.
	unauthorisedOnce atomic.Bool

	refreshCalls  atomic.Int32
	refreshResult *spotify.Token
	refreshErr    error
}

func (f *fakeRecentlyPlayed) RecentlyPlayed(_ context.Context, _ string, after time.Time, _, _ int) ([]spotify.PlayHistory, error) {
	n := f.calls.Add(1)
	f.lastAfter.Store(after)

	if f.unauthorisedOnce.Load() {
		f.unauthorisedOnce.Store(false)
		return nil, &spotify.APIError{StatusCode: 401, Message: "The access token expired"}
	}
	if f.failWith != nil {
		return nil, f.failWith
	}
	idx := int(n) - 1
	if idx >= len(f.pages) {
		return nil, nil
	}
	return f.pages[idx], nil
}

func (f *fakeRecentlyPlayed) RefreshToken(context.Context, string) (*spotify.Token, error) {
	f.refreshCalls.Add(1)
	if f.refreshErr != nil {
		return nil, f.refreshErr
	}
	if f.refreshResult != nil {
		return f.refreshResult, nil
	}
	return &spotify.Token{
		AccessToken: "refreshed-access-token",
		// Deliberately empty: Spotify omits refresh_token on most refreshes, and
		// the stored one must survive.
		RefreshToken: "",
		ExpiresAt:    time.Now().Add(time.Hour),
	}, nil
}

// syncPlay builds one play-history entry with a full track object, the way the
// real endpoint returns it.
func syncPlay(trackID string, at time.Time) spotify.PlayHistory {
	return spotify.PlayHistory{
		PlayedAt: at,
		Track: spotify.Track{
			ID:         trackID,
			Name:       "Track " + trackID,
			DurationMs: 210_000,
			Album: spotify.Album{
				ID: albumIDFor(trackID), Name: "Album", AlbumType: "album",
				ReleaseDate: "2011-03-04", ReleaseDatePrecision: "day",
				Artists: []spotify.Artist{{ID: artistIDFor(trackID), Name: "Artist"}},
			},
			Artists: []spotify.Artist{{ID: artistIDFor(trackID), Name: "Artist"}},
		},
	}
}

func newPoller(t *testing.T, env *harness.Env, api encoresync.SpotifyAPI) *encoresync.Poller {
	t.Helper()
	p, err := encoresync.NewPoller(config.Sync{
		Enabled:         true,
		Interval:        time.Minute,
		Concurrency:     1,
		InitialLookback: 14 * 24 * time.Hour,
	}, encoresync.Deps{
		Store:    env.Store,
		Accounts: env.Accounts,
		Listens:  env.Listens,
		Catalog:  env.Catalog,
		Spotify:  api,
		Logger:   harness.Discard(),
	})
	if err != nil {
		t.Fatalf("build poller: %v", err)
	}
	return p
}

// connect gives a user a Spotify grant so the poller has something to poll.
func connect(t *testing.T, env *harness.Env, userID uuid.UUID, expiresAt time.Time) {
	t.Helper()
	err := env.Accounts.Credentials.Upsert(env.Ctx(), env.Store.DB(), domain.SpotifyCredentials{
		UserID:         userID,
		AccessToken:    "initial-access-token",
		RefreshToken:   "the-refresh-token",
		TokenExpiresAt: expiresAt,
		Scopes:         config.DefaultScopes(),
		SyncState:      domain.SyncStateOK,
		ConnectedAt:    time.Now(),
	})
	if err != nil {
		t.Fatalf("connect spotify account: %v", err)
	}
}

func TestSyncIngestsPlaysAndAdvancesTheCursor(t *testing.T) {
	env := harness.New(t)
	user := env.NewUser("syncuser")
	connect(t, env, user.ID, time.Now().Add(time.Hour))

	at := time.Now().Add(-2 * time.Hour).Truncate(time.Second)
	api := &fakeRecentlyPlayed{pages: [][]spotify.PlayHistory{{
		syncPlay("sync00000000000000001a", at),
		syncPlay("sync00000000000000002b", at.Add(5*time.Minute)),
	}}}
	poller := newPoller(t, env, api)

	res, err := poller.SyncUser(env.Ctx(), user.ID)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if res.Imported != 2 {
		t.Fatalf("imported %d plays, want 2", res.Imported)
	}
	if got := env.CountListens(user.ID); got != 2 {
		t.Fatalf("database holds %d listens, want 2", got)
	}

	// The catalogue detail the endpoint gave away for free must have been kept,
	// so enrichment does not have to fetch it all over again.
	if got := env.ScalarInt(
		`SELECT count(*) FROM tracks WHERE metadata_state = 'resolved'`); got != 2 {
		t.Fatalf("%d tracks resolved from the play-history payload, want 2", got)
	}
	if got := env.ScalarInt(`SELECT count(*) FROM track_artists`); got != 2 {
		t.Fatalf("%d artist credits recorded, want 2", got)
	}
	// Artists are only *registered*, not resolved: the objects embedded in a
	// track carry no genres, followers or images.
	if got := env.ScalarInt(
		`SELECT count(*) FROM artists WHERE metadata_state = 'pending'`); got != 2 {
		t.Fatalf("%d artists queued for enrichment, want 2", got)
	}

	creds, err := env.Accounts.Credentials.Get(env.Ctx(), env.Store.DB(), user.ID)
	if err != nil {
		t.Fatalf("read credentials: %v", err)
	}
	if creds.SyncCursorAt == nil {
		t.Fatal("the sync cursor was never set")
	}
	if !creds.SyncCursorAt.Equal(at.Add(5 * time.Minute).UTC()) {
		t.Fatalf("cursor = %s, want the newest play at %s",
			creds.SyncCursorAt.UTC(), at.Add(5*time.Minute).UTC())
	}
	if creds.SyncState != domain.SyncStateOK {
		t.Fatalf("sync state = %q, want ok", creds.SyncState)
	}
}

func TestSyncIsDuplicateSafe(t *testing.T) {
	env := harness.New(t)
	user := env.NewUser("syncdupe")
	connect(t, env, user.ID, time.Now().Add(time.Hour))

	at := time.Now().Add(-3 * time.Hour).Truncate(time.Second)
	page := []spotify.PlayHistory{
		syncPlay("sync00000000000000003c", at),
		syncPlay("sync00000000000000004d", at.Add(10*time.Minute)),
	}
	// The same page twice: exactly what happens when a poll's commit is lost and
	// the next poll asks for the same window again.
	api := &fakeRecentlyPlayed{pages: [][]spotify.PlayHistory{page, page}}
	poller := newPoller(t, env, api)

	if _, err := poller.SyncUser(env.Ctx(), user.ID); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	first := env.CountListens(user.ID)

	res, err := poller.SyncUser(env.Ctx(), user.ID)
	if err != nil {
		t.Fatalf("second sync: %v", err)
	}
	if res.Imported != 0 {
		t.Fatalf("re-polling the same page imported %d plays, want 0", res.Imported)
	}
	if got := env.CountListens(user.ID); got != first {
		t.Fatalf("row count moved from %d to %d on a repeated poll", first, got)
	}
}

func TestSyncRefreshesAnExpiredTokenAndKeepsTheRefreshToken(t *testing.T) {
	env := harness.New(t)
	user := env.NewUser("syncrefresh")
	// An access token that expired an hour ago forces the refresh path.
	connect(t, env, user.ID, time.Now().Add(-time.Hour))

	at := time.Now().Add(-30 * time.Minute).Truncate(time.Second)
	api := &fakeRecentlyPlayed{pages: [][]spotify.PlayHistory{{
		syncPlay("sync00000000000000005e", at),
	}}}
	poller := newPoller(t, env, api)

	if _, err := poller.SyncUser(env.Ctx(), user.ID); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if api.refreshCalls.Load() == 0 {
		t.Fatal("an expired access token did not trigger a refresh")
	}

	creds, err := env.Accounts.Credentials.Get(env.Ctx(), env.Store.DB(), user.ID)
	if err != nil {
		t.Fatalf("read credentials: %v", err)
	}
	if creds.AccessToken != "refreshed-access-token" {
		t.Fatalf("access token was not replaced (got %q)", redactForTest(creds.AccessToken))
	}
	// The decisive assertion: Spotify omitted refresh_token, and the stored one
	// must have survived. Losing it here would silently break the account until
	// the user noticed and re-authorised.
	if creds.RefreshToken != "the-refresh-token" {
		t.Fatal("the stored refresh token was cleared by a refresh that omitted one")
	}
}

func TestSyncMarksNeedsReauthOnInvalidGrant(t *testing.T) {
	env := harness.New(t)
	user := env.NewUser("syncreauth")
	connect(t, env, user.ID, time.Now().Add(-time.Hour))

	api := &fakeRecentlyPlayed{refreshErr: spotify.ErrInvalidGrant}
	poller := newPoller(t, env, api)

	_, err := poller.SyncUser(env.Ctx(), user.ID)
	if err == nil {
		t.Fatal("a revoked grant should surface as an error")
	}

	creds, cerr := env.Accounts.Credentials.Get(env.Ctx(), env.Store.DB(), user.ID)
	if cerr != nil {
		t.Fatalf("read credentials: %v", cerr)
	}
	if creds.SyncState != domain.SyncStateNeedsReauth {
		t.Fatalf("sync state = %q, want needs_reauth: only the user can fix a revoked grant, "+
			"so retrying forever would just burn rate limit", creds.SyncState)
	}

	// An account in that state must be left out of the polling rotation.
	due, err := env.Accounts.Credentials.ListDueForSync(
		env.Ctx(), env.Store.DB(), time.Now().Add(time.Hour), 10)
	if err != nil {
		t.Fatalf("list due for sync: %v", err)
	}
	for _, id := range due {
		if id == user.ID {
			t.Fatal("an account needing re-authorisation is still being polled")
		}
	}
}

func TestSyncFailureLeavesTheCursorAlone(t *testing.T) {
	env := harness.New(t)
	user := env.NewUser("syncfail")
	connect(t, env, user.ID, time.Now().Add(time.Hour))

	at := time.Now().Add(-time.Hour).Truncate(time.Second)
	good := &fakeRecentlyPlayed{pages: [][]spotify.PlayHistory{{
		syncPlay("sync00000000000000006f", at),
	}}}
	if _, err := newPoller(t, env, good).SyncUser(env.Ctx(), user.ID); err != nil {
		t.Fatalf("seed sync: %v", err)
	}
	before, err := env.Accounts.Credentials.Get(env.Ctx(), env.Store.DB(), user.ID)
	if err != nil {
		t.Fatalf("read credentials: %v", err)
	}

	broken := &fakeRecentlyPlayed{failWith: errors.New("connection reset by peer")}
	if _, err := newPoller(t, env, broken).SyncUser(env.Ctx(), user.ID); err == nil {
		t.Fatal("a failing poll should return an error")
	}

	after, err := env.Accounts.Credentials.Get(env.Ctx(), env.Store.DB(), user.ID)
	if err != nil {
		t.Fatalf("read credentials: %v", err)
	}
	if before.SyncCursorAt == nil || after.SyncCursorAt == nil {
		t.Fatal("the cursor should be set after the successful poll")
	}
	if !after.SyncCursorAt.Equal(*before.SyncCursorAt) {
		t.Fatalf("a failed poll moved the cursor from %s to %s; the next poll would skip those plays",
			before.SyncCursorAt, after.SyncCursorAt)
	}
	if after.SyncState != domain.SyncStateError {
		t.Fatalf("sync state = %q, want error", after.SyncState)
	}
	if after.LastSyncError == "" {
		t.Fatal("a failed poll should record why it failed")
	}
}

func TestSyncUsesTheStoredCursorAsTheAfterParameter(t *testing.T) {
	env := harness.New(t)
	user := env.NewUser("synccursor")
	connect(t, env, user.ID, time.Now().Add(time.Hour))

	at := time.Now().Add(-4 * time.Hour).Truncate(time.Second)
	api := &fakeRecentlyPlayed{pages: [][]spotify.PlayHistory{
		{syncPlay("sync0000000000000000a1", at)},
		{},
	}}
	poller := newPoller(t, env, api)

	if _, err := poller.SyncUser(env.Ctx(), user.ID); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	if _, err := poller.SyncUser(env.Ctx(), user.ID); err != nil {
		t.Fatalf("second sync: %v", err)
	}

	got, _ := api.lastAfter.Load().(time.Time)
	if !got.Equal(at.UTC()) {
		t.Fatalf("the second poll asked for plays after %s, want the stored cursor %s",
			got.UTC(), at.UTC())
	}
}

// redactForTest keeps a failing assertion from printing a whole token.
func redactForTest(s string) string {
	if len(s) <= 4 {
		return "****"
	}
	return fmt.Sprintf("****%s", s[len(s)-4:])
}
