//go:build integration

package integration

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/RequiDev/encore/internal/config"
	"github.com/RequiDev/encore/internal/domain"
	"github.com/RequiDev/encore/internal/nowplaying"
	"github.com/RequiDev/encore/internal/spotify"
	"github.com/RequiDev/encore/test/harness"
)

// playingTrack is one healthy observation, so each test varies only what it is
// about.
func playingTrack(at time.Time) domain.NowPlaying {
	progress, duration := 161000, 255000
	return domain.NowPlaying{
		ObservedAt: at,
		State:      domain.PlaybackPlaying,
		Kind:       domain.PlaybackItemTrack,
		TrackID:    "spotifytrack00000001",
		Title:      "The Wheel",
		Artist:     "SOHN",
		ProgressMs: &progress,
		DurationMs: &duration,
		DeviceName: "Kitchen speaker",
		CheckedAt:  at,
	}
}

// TestNowPlayingIsAbsentUntilSomethingIsObserved pins the distinction the whole
// feature turns on: never looked is not nothing playing.
//
// Fails when: Get invents a zero-valued row instead of reporting ErrNotFound, or
// RecordFailure inserts a row claiming 'idle' — in either case an account
// nobody has checked would read as one whose player is silent.
func TestNowPlayingIsAbsentUntilSomethingIsObserved(t *testing.T) {
	e := harness.New(t)
	user := e.NewUser("np-absent")

	if _, err := e.Accounts.NowPlaying.Get(e.Ctx(), e.Store.DB(), user.ID); err == nil {
		t.Fatal("Get returned a row for an account that has never been checked")
	}

	at := time.Date(2026, time.July, 31, 9, 0, 0, 0, time.UTC)
	if err := e.Accounts.NowPlaying.RecordFailure(e.Ctx(), e.Store.DB(), user.ID, at); err != nil {
		t.Fatalf("RecordFailure: %v", err)
	}

	got, err := e.Accounts.NowPlaying.Get(e.Ctx(), e.Store.DB(), user.ID)
	if err != nil {
		t.Fatalf("Get after a failure: %v", err)
	}
	if got.Observed() {
		t.Fatalf("Observed() = true after nothing but a failed check: %+v", got)
	}
	if got.State != domain.PlaybackUnknown {
		t.Errorf("State = %q, want %q", got.State, domain.PlaybackUnknown)
	}
	if got.Kind != domain.PlaybackItemNone {
		t.Errorf("Kind = %q, want %q", got.Kind, domain.PlaybackItemNone)
	}
	if !got.Failed {
		t.Error("Failed = false after a failed check")
	}
	if !got.CheckedAt.Equal(at) {
		t.Errorf("CheckedAt = %v, want %v", got.CheckedAt, at)
	}
}

// TestRecordRoundTripsEveryColumn pins that nothing is dropped between the
// classifier and the card.
//
// Fails when: a column is added to the INSERT and not to the SELECT, or the two
// disagree about order — the scan then lands progress in duration, and a
// listener sees "4:15 of 2:41".
func TestRecordRoundTripsEveryColumn(t *testing.T) {
	e := harness.New(t)
	user := e.NewUser("np-roundtrip")
	at := time.Date(2026, time.July, 31, 9, 30, 0, 0, time.UTC)

	want := playingTrack(at)
	if err := e.Accounts.NowPlaying.Record(e.Ctx(), e.Store.DB(), user.ID, want); err != nil {
		t.Fatalf("Record: %v", err)
	}

	got, err := e.Accounts.NowPlaying.Get(e.Ctx(), e.Store.DB(), user.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !got.Observed() {
		t.Fatal("Observed() = false after a successful record")
	}
	if got.State != domain.PlaybackPlaying || got.Kind != domain.PlaybackItemTrack {
		t.Errorf("State/Kind = %q/%q, want playing/track", got.State, got.Kind)
	}
	if got.Title != "The Wheel" || got.Artist != "SOHN" {
		t.Errorf("Title/Artist = %q/%q, want The Wheel/SOHN", got.Title, got.Artist)
	}
	if got.TrackID != "spotifytrack00000001" {
		t.Errorf("TrackID = %q", got.TrackID)
	}
	if got.ProgressMs == nil || *got.ProgressMs != 161000 {
		t.Errorf("ProgressMs = %v, want 161000", got.ProgressMs)
	}
	if got.DurationMs == nil || *got.DurationMs != 255000 {
		t.Errorf("DurationMs = %v, want 255000", got.DurationMs)
	}
	if got.DeviceName != "Kitchen speaker" {
		t.Errorf("DeviceName = %q", got.DeviceName)
	}
	if got.Failed {
		t.Error("Failed = true after a successful record")
	}
	if !got.ObservedAt.Equal(at) || !got.CheckedAt.Equal(at) {
		t.Errorf("ObservedAt/CheckedAt = %v/%v, want both %v", got.ObservedAt, got.CheckedAt, at)
	}
}

// TestAFailureKeepsTheLastObservation pins that a failed check does not erase
// what Encore already knew.
//
// This is what lets the card say "the last check failed; this is what you were
// playing four minutes ago" instead of falling back to "we do not know", which
// would throw away a true thing because a later request went wrong.
//
// Fails when: RecordFailure is written as a full upsert that resets the
// observation columns to their defaults — Observed() then goes false and the
// title is gone.
func TestAFailureKeepsTheLastObservation(t *testing.T) {
	e := harness.New(t)
	user := e.NewUser("np-stale")
	observed := time.Date(2026, time.July, 31, 9, 30, 0, 0, time.UTC)
	failedAt := observed.Add(4 * time.Minute)

	if err := e.Accounts.NowPlaying.Record(e.Ctx(), e.Store.DB(), user.ID, playingTrack(observed)); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if err := e.Accounts.NowPlaying.RecordFailure(e.Ctx(), e.Store.DB(), user.ID, failedAt); err != nil {
		t.Fatalf("RecordFailure: %v", err)
	}

	got, err := e.Accounts.NowPlaying.Get(e.Ctx(), e.Store.DB(), user.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !got.Failed {
		t.Error("Failed = false after a failed check")
	}
	if !got.CheckedAt.Equal(failedAt) {
		t.Errorf("CheckedAt = %v, want %v", got.CheckedAt, failedAt)
	}
	if !got.ObservedAt.Equal(observed) {
		t.Errorf("ObservedAt = %v, want the earlier %v: a failure must not move it",
			got.ObservedAt, observed)
	}
	if got.Title != "The Wheel" {
		t.Errorf("Title = %q, want the last observation to survive a failure", got.Title)
	}
}

// TestAnIdleObservationCannotCarryATitle proves the constraint bites rather than
// merely existing.
//
// A leftover title behind "Nothing is playing." is the exact stale-claim defect
// this phase exists to rule out, and the database is the only layer that can
// refuse it unconditionally.
//
// Fails when: now_playing_nothing_carries_nothing is dropped from the migration
// — the write then succeeds and no test anywhere notices.
func TestAnIdleObservationCannotCarryATitle(t *testing.T) {
	e := harness.New(t)
	user := e.NewUser("np-idle-title")
	at := time.Date(2026, time.July, 31, 9, 30, 0, 0, time.UTC)

	err := e.Accounts.NowPlaying.Record(e.Ctx(), e.Store.DB(), user.ID, domain.NowPlaying{
		ObservedAt: at,
		State:      domain.PlaybackIdle,
		Kind:       domain.PlaybackItemNone,
		Title:      "The Wheel",
		CheckedAt:  at,
	})
	if err == nil {
		t.Fatal("an idle observation carrying a title was accepted")
	}
}

// TestNowPlayingTextIsTruncatedRuneSafely pins the bound on the three text
// columns Spotify's own strings reach.
//
// Fails when: store.Truncate is replaced by a plain slice — the multi-byte runes
// below then cut mid-rune and PostgreSQL rejects the write outright, so Record
// returns an error instead of storing a bounded string.
func TestNowPlayingTextIsTruncatedRuneSafely(t *testing.T) {
	e := harness.New(t)
	user := e.NewUser("np-truncate")
	at := time.Date(2026, time.July, 31, 9, 30, 0, 0, time.UTC)

	// A one-byte prefix followed by 200 repeats of an 8-byte rune group: past
	// the limit, and — because 200 is a multiple of 8 — a plain byte slice at
	// exactly 200 bytes would land squarely on a rune boundary without the
	// prefix, making a naive cut indistinguishable from a rune-safe one (see
	// TestSetCoverTruncatesTheReason in playlistcover_test.go, which hits the
	// same arithmetic against the same limit). The single leading ASCII byte
	// shifts every later boundary by one, so byte offset 200 falls inside the
	// third byte of a rune instead of before it.
	long := "x" + strings.Repeat("é—中", 200)
	obs := playingTrack(at)
	obs.Title, obs.Artist, obs.DeviceName = long, long, long

	if err := e.Accounts.NowPlaying.Record(e.Ctx(), e.Store.DB(), user.ID, obs); err != nil {
		t.Fatalf("Record: %v", err)
	}
	got, err := e.Accounts.NowPlaying.Get(e.Ctx(), e.Store.DB(), user.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	for name, value := range map[string]string{
		"Title": got.Title, "Artist": got.Artist, "DeviceName": got.DeviceName,
	} {
		if len(value) > 256 {
			t.Errorf("%s is %d bytes, want it bounded", name, len(value))
		}
		if !utf8.ValidString(value) {
			t.Errorf("%s is not valid UTF-8", name)
		}
	}
}

// TestTrackKnownIsFalseForATrackTheCatalogueHasNeverSeen pins the join that
// decides whether the title is a link.
//
// Spotify names a track the instant it starts playing; Encore's catalogue
// learns about it when enrichment gets round to it. Linking before then would
// be a dead link wearing a working one's clothes.
//
// Fails when: the LEFT JOIN is dropped and TrackKnown is hard-coded to
// TrackID != "" — the assertion below then reports true for a track nothing
// holds.
func TestTrackKnownIsFalseForATrackTheCatalogueHasNeverSeen(t *testing.T) {
	e := harness.New(t)
	user := e.NewUser("np-unknown-track")
	at := time.Date(2026, time.July, 31, 9, 30, 0, 0, time.UTC)

	if err := e.Accounts.NowPlaying.Record(e.Ctx(), e.Store.DB(), user.ID, playingTrack(at)); err != nil {
		t.Fatalf("Record: %v", err)
	}
	got, err := e.Accounts.NowPlaying.Get(e.Ctx(), e.Store.DB(), user.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.TrackKnown {
		t.Fatal("TrackKnown = true for a track the catalogue has never seen")
	}
}

// TestDeletingAUserRemovesTheirNowPlayingRow pins the cascade, which is also
// what lets the integration harness leave now_playing out of truncatedTables.
//
// Fails when: the REFERENCES clause loses ON DELETE CASCADE — DeleteUser then
// fails outright on a foreign-key violation, which is a louder failure than
// this test but a failure the test still names.
func TestDeletingAUserRemovesTheirNowPlayingRow(t *testing.T) {
	e := harness.New(t)
	user := e.NewUser("np-cascade")
	at := time.Date(2026, time.July, 31, 9, 30, 0, 0, time.UTC)

	if err := e.Accounts.NowPlaying.Record(e.Ctx(), e.Store.DB(), user.ID, playingTrack(at)); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if err := e.Accounts.Users.DeleteUser(e.Ctx(), e.Store.DB(), user.ID); err != nil {
		t.Fatalf("DeleteUser: %v", err)
	}

	var count int
	row := e.Store.DB().QueryRow(e.Ctx(), `SELECT count(*) FROM now_playing WHERE user_id = $1`, user.ID)
	if err := row.Scan(&count); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if count != 0 {
		t.Fatalf("%d now_playing rows survived the account's deletion", count)
	}
}

// The tests from here down exercise internal/nowplaying.Watcher itself against a
// real database with a fake Spotify client. internal/nowplaying's own tests pin
// the classification, the scope skip, the concurrency and the disabled-means-off
// rule with no database at all; what is left for here is what those cannot
// prove — that a run against real tables writes only the one row it is allowed
// to, and that the due query excludes what it claims to.

// fakeNowPlayingAPI satisfies nowplaying.SpotifyAPI without a network, and
// counts what was actually asked of it.
//
// The counter is atomic because a tick calls this from up to four goroutines at
// once. Every test here happens to use one account today, which would make a
// plain int safe by accident; adding a second account would make it a data race
// that only CI's -race run would find.
type fakeNowPlayingAPI struct {
	playback *spotify.Playback
	err      error
	calls    atomic.Int32
}

func (f *fakeNowPlayingAPI) CurrentlyPlaying(context.Context, string) (*spotify.Playback, error) {
	f.calls.Add(1)
	return f.playback, f.err
}

// fakeNowPlayingTokens satisfies nowplaying.Tokens with a fixed token. The real
// refresh dance belongs to *sync.Poller and is exercised by sync_test.go; the
// watcher only ever calls this one method.
type fakeNowPlayingTokens struct{}

func (fakeNowPlayingTokens) AccessToken(context.Context, uuid.UUID) (string, error) {
	return "now-playing-access-token", nil
}

// playingResponse is a fake Spotify answering with one track, playing.
func playingResponse(trackID, title string) *fakeNowPlayingAPI {
	progress := 161000
	return &fakeNowPlayingAPI{playback: &spotify.Playback{
		IsPlaying: true, ProgressMs: &progress, CurrentlyPlayingType: "track",
		Device: &spotify.Device{Name: "Kitchen speaker", Type: "Speaker"},
		Item: &spotify.PlaybackItem{
			ID: trackID, Name: title, Type: "track", DurationMs: 255000,
			Artists: []spotify.Artist{{Name: "SOHN"}},
		},
	}}
}

// forbiddenResponse is a fake Spotify refusing the endpoint, which is what a
// grant without user-read-playback-state gets.
func forbiddenResponse() *fakeNowPlayingAPI {
	return &fakeNowPlayingAPI{err: &spotify.APIError{StatusCode: http.StatusForbidden}}
}

// newWatcher builds a watcher over the harness's real repositories and the
// supplied fake Spotify.
//
// NowPlaying is e.Accounts.NowPlaying and nothing else is passed: the Deps this
// takes have no field that could reach listens, the catalogue or the credentials
// repository, which is the same guarantee internal/nowplaying's import-graph
// test states from the other direction.
func newWatcher(t *testing.T, e *harness.Env, api nowplaying.SpotifyAPI) *nowplaying.Watcher {
	t.Helper()
	w, err := nowplaying.New(config.NowPlaying{Interval: 30 * time.Second}, nowplaying.Deps{
		Store:      e.Store,
		NowPlaying: e.Accounts.NowPlaying,
		Spotify:    api,
		Tokens:     fakeNowPlayingTokens{},
		Logger:     harness.Discard(),
	})
	if err != nil {
		t.Fatalf("build now-playing watcher: %v", err)
	}
	return w
}

// connectWithPlaybackScope gives a user a healthy grant carrying the scope this
// poller needs, spelled out rather than taken from config.DefaultScopes() so
// these tests keep saying what they depend on if that list changes again.
func connectWithPlaybackScope(t *testing.T, e *harness.Env, userID uuid.UUID) {
	t.Helper()
	connectWithScopes(t, e, userID, time.Now().Add(time.Hour), []string{
		"user-read-recently-played", "user-read-playback-state",
	})
}

// countListens is the assertion this whole package exists to make possible.
func countListens(t *testing.T, e *harness.Env, userID uuid.UUID) int64 {
	t.Helper()
	return e.ScalarInt(`SELECT count(*) FROM listens WHERE user_id = $1`, userID.String())
}

// makeDueAgain pushes an account's last check back an hour so the next tick
// picks it up, standing in for the interval actually elapsing.
func makeDueAgain(t *testing.T, e *harness.Env, userID uuid.UUID) {
	t.Helper()
	if _, err := e.Store.DB().Exec(e.Ctx(),
		`UPDATE now_playing SET checked_at = checked_at - interval '1 hour' WHERE user_id = $1`,
		userID.String()); err != nil {
		t.Fatalf("make account due again: %v", err)
	}
}

// TestThePollerAddsNoListens is the spec's read-only observer rule, asserted at
// runtime rather than through the import graph.
//
// §2.2: running the poller across a listening session must add exactly zero
// rows to listens.
//
// Fails when: the poller gains any write path to listens. The import-graph test
// in internal/nowplaying catches the obvious way in; this catches an indirect
// one, through a dependency that grows a write of its own.
func TestThePollerAddsNoListens(t *testing.T) {
	e := harness.New(t)
	user := e.NewUser("np-no-listens")
	connectWithPlaybackScope(t, e, user.ID)

	before := countListens(t, e, user.ID)

	api := playingResponse("track-1", "The Wheel")
	w := newWatcher(t, e, api)
	for range 5 {
		if _, err := w.RunOnce(e.Ctx()); err != nil {
			t.Fatalf("RunOnce: %v", err)
		}
		// Without this the account is not due again inside the interval and the
		// loop below would poll once and idle four times, which would assert
		// almost nothing.
		makeDueAgain(t, e, user.ID)
	}
	if got := api.calls.Load(); got != 5 {
		t.Fatalf("the poller made %d requests, want 5: this test is only meaningful "+
			"if a whole listening session was actually observed", got)
	}

	if after := countListens(t, e, user.ID); after != before {
		t.Fatalf("listens went from %d to %d; the now-playing poller is a "+
			"read-only observer and must never ingest", before, after)
	}
	// The observation itself did land, so the zero above is the poller declining
	// to ingest rather than the poller doing nothing at all.
	got, err := e.Accounts.NowPlaying.Get(e.Ctx(), e.Store.DB(), user.ID)
	if err != nil {
		t.Fatalf("Get now playing: %v", err)
	}
	if !got.Observed() || got.TrackID != "track-1" {
		t.Fatalf("now playing = %+v, want the observed track recorded", got)
	}
}

// TestThePollerNeverMovesTheSyncWatermark is the other half of the same rule.
//
// A listen is not only a row in listens: recently-played sync's cursor decides
// which plays Spotify is asked for next, and a second writer nudging it would
// drop real plays without ever writing a wrong one.
//
// Fails when: the poller gains a credentials repository and marks a sync result,
// or advances last_synced_at to say "we looked at this account".
func TestThePollerNeverMovesTheSyncWatermark(t *testing.T) {
	e := harness.New(t)
	user := e.NewUser("np-no-watermark")
	connectWithPlaybackScope(t, e, user.ID)

	before, err := e.Accounts.Credentials.Get(e.Ctx(), e.Store.DB(), user.ID)
	if err != nil {
		t.Fatalf("Get credentials: %v", err)
	}

	w := newWatcher(t, e, playingResponse("track-1", "The Wheel"))
	if _, err := w.RunOnce(e.Ctx()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	after, err := e.Accounts.Credentials.Get(e.Ctx(), e.Store.DB(), user.ID)
	if err != nil {
		t.Fatalf("Get credentials: %v", err)
	}
	if !sameInstant(before.SyncCursorAt, after.SyncCursorAt) {
		t.Fatalf("sync_cursor_at moved from %v to %v; the recently-played cursor "+
			"belongs to internal/sync alone", before.SyncCursorAt, after.SyncCursorAt)
	}
	if !sameInstant(before.LastSyncAt, after.LastSyncAt) {
		t.Fatalf("last_sync_at moved from %v to %v on a now-playing poll",
			before.LastSyncAt, after.LastSyncAt)
	}
	if after.SyncState != before.SyncState {
		t.Fatalf("sync_state changed from %q to %q on a now-playing poll",
			before.SyncState, after.SyncState)
	}
}

func sameInstant(a, b *time.Time) bool {
	switch {
	case a == nil && b == nil:
		return true
	case a == nil || b == nil:
		return false
	default:
		return a.Equal(*b)
	}
}

// TestAForbiddenCheckNeverParksTheAccount pins internal/sync/account.go:296's
// rule for this endpoint.
//
// A 403 here means only that the grant does not carry user-read-playback-state.
// Parking the account would stop ingesting a listening history that reads
// perfectly, over a feature the listener may not even have noticed.
//
// Fails when: the poller gains a credentials repository and calls
// MarkNeedsReauth on a 403 — sync_state below then reads needs_reauth.
func TestAForbiddenCheckNeverParksTheAccount(t *testing.T) {
	e := harness.New(t)
	user := e.NewUser("np-forbidden")
	connectWithPlaybackScope(t, e, user.ID)

	w := newWatcher(t, e, forbiddenResponse())
	if _, err := w.RunOnce(e.Ctx()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	creds, err := e.Accounts.Credentials.Get(e.Ctx(), e.Store.DB(), user.ID)
	if err != nil {
		t.Fatalf("Get credentials: %v", err)
	}
	if creds.SyncState == domain.SyncStateNeedsReauth {
		t.Fatal("a 403 on the now-playing check parked the account; an optional " +
			"read scope's absence must never stop ingesting a listening history")
	}
	if creds.LastSyncError != "" {
		t.Errorf("LastSyncError = %q; a now-playing failure belongs on the "+
			"now_playing row, not on the sync record", creds.LastSyncError)
	}

	got, err := e.Accounts.NowPlaying.Get(e.Ctx(), e.Store.DB(), user.ID)
	if err != nil {
		t.Fatalf("Get now playing: %v", err)
	}
	if !got.Failed {
		t.Error("the failed check was not recorded, so the card cannot say the " +
			"display is stale")
	}
}

// TestAFailedCheckKeepsTheLastObservationThroughThePoller is the poller-level
// statement of the rule TestAFailureKeepsTheLastObservation makes about the
// repository: no path through a failing check erases what Encore already knew.
//
// The repository test proves RecordFailure touches only two columns; this proves
// the poller reaches that method rather than Record on the way through, which is
// the mistake the repository alone cannot rule out.
//
// Fails when: check() records an empty observation on failure instead of calling
// recordFailure — the title below then disappears the first time a request
// times out.
func TestAFailedCheckKeepsTheLastObservationThroughThePoller(t *testing.T) {
	e := harness.New(t)
	user := e.NewUser("np-failure-keeps")
	connectWithPlaybackScope(t, e, user.ID)

	if _, err := newWatcher(t, e, playingResponse("track-1", "The Wheel")).RunOnce(e.Ctx()); err != nil {
		t.Fatalf("first RunOnce: %v", err)
	}
	observed, err := e.Accounts.NowPlaying.Get(e.Ctx(), e.Store.DB(), user.ID)
	if err != nil {
		t.Fatalf("Get after the good check: %v", err)
	}
	if !observed.Observed() {
		t.Fatal("the first check recorded nothing")
	}
	makeDueAgain(t, e, user.ID)

	if _, err := newWatcher(t, e, forbiddenResponse()).RunOnce(e.Ctx()); err != nil {
		t.Fatalf("second RunOnce: %v", err)
	}

	got, err := e.Accounts.NowPlaying.Get(e.Ctx(), e.Store.DB(), user.ID)
	if err != nil {
		t.Fatalf("Get after the failed check: %v", err)
	}
	if !got.Failed {
		t.Error("Failed = false after a check that failed")
	}
	if !got.Observed() || got.Title != "The Wheel" || got.TrackID != "track-1" {
		t.Fatalf("now playing = %+v, want the previous observation intact: a failed "+
			"poll must not throw away a true thing", got)
	}
	if !got.ObservedAt.Equal(observed.ObservedAt) {
		t.Errorf("ObservedAt moved from %v to %v on a failed check",
			observed.ObservedAt, got.ObservedAt)
	}
	if !got.CheckedAt.After(observed.CheckedAt) {
		t.Errorf("CheckedAt = %v, want an instant after the good check's %v",
			got.CheckedAt, observed.CheckedAt)
	}
}

// TestAnAccountNeedingReauthIsNeverChecked pins the other exclusion in the due
// query.
//
// Fails when: the sync_state predicate is dropped from listDueSQL — a broken
// grant is then polled every interval to be told the same thing.
func TestAnAccountNeedingReauthIsNeverChecked(t *testing.T) {
	e := harness.New(t)
	user := e.NewUser("np-parked")
	connectWithPlaybackScope(t, e, user.ID)
	if err := e.Accounts.Credentials.MarkNeedsReauth(e.Ctx(), e.Store.DB(), user.ID,
		"reconnect"); err != nil {
		t.Fatalf("MarkNeedsReauth: %v", err)
	}

	due, err := e.Accounts.NowPlaying.ListDue(e.Ctx(), e.Store.DB(), time.Now().UTC(), 100)
	if err != nil {
		t.Fatalf("ListDue: %v", err)
	}
	for _, a := range due {
		if a.UserID == user.ID {
			t.Fatal("a needs_reauth account is queued for a playback check")
		}
	}
}

// TestATickFiringEarlyWithinTheJitterStillFindsTheAccountDue is the jitter fix
// end to end, against the real query rather than a captured argument.
//
// The tick schedule draws each delay from a band around the interval, so a tick
// arriving at the early edge of that band must still find an account it checked
// one delay ago. Without the slack RunOnce adds, the due predicate would demand
// a whole interval, the early half of every band would poll nobody, and the card
// an operator asked to refresh every thirty seconds would refresh every
// forty-five on average.
//
// Twenty-eight seconds, rather than the twenty-seven the earliest tick can
// actually fire at: at exactly twenty-seven the two sides meet and ListDue's
// strict checked_at < olderThan decides the tie against polling, which is a
// millisecond-wide race against the wall clock rather than a property worth
// pinning. Twenty-eight is inside the jitter band either way and outside it by a
// clear second.
//
// Fails when: dueSlack is dropped — the second tick then makes no request at all.
func TestATickFiringEarlyWithinTheJitterStillFindsTheAccountDue(t *testing.T) {
	e := harness.New(t)
	user := e.NewUser("np-jitter-due")
	connectWithPlaybackScope(t, e, user.ID)

	api := playingResponse("track-1", "The Wheel")
	w := newWatcher(t, e, api)
	if _, err := w.RunOnce(e.Ctx()); err != nil {
		t.Fatalf("first RunOnce: %v", err)
	}
	if got := api.calls.Load(); got != 1 {
		t.Fatalf("the first tick made %d requests, want 1", got)
	}

	// Twenty-eight seconds of a thirty-second interval have passed: inside the
	// jitter band, short of a whole interval.
	if _, err := e.Store.DB().Exec(e.Ctx(),
		`UPDATE now_playing SET checked_at = checked_at - interval '28 seconds' WHERE user_id = $1`,
		user.ID.String()); err != nil {
		t.Fatalf("age the last check: %v", err)
	}

	polled, err := w.RunOnce(e.Ctx())
	if err != nil {
		t.Fatalf("second RunOnce: %v", err)
	}
	if polled != 1 || api.calls.Load() != 2 {
		t.Fatalf("a tick firing 28s into a 30s interval polled %d accounts over %d requests, "+
			"want 1 and 2: the schedule can fire this early, so the queue must answer this "+
			"early, or half of every tick does nothing", polled, api.calls.Load())
	}
}

// TestListDueQueuesNeverCheckedFirstAndExcludesTheRest is the coverage ListDue
// shipped without.
//
// Every clause in listDueSQL is a decision about somebody's Spotify quota, and
// each is asserted here: a newly connected account is at the head of the queue
// so its card fills in on the next tick rather than behind everybody else; an
// account checked within the interval is not asked again; and an account whose
// grant predates user-read-playback-state is never queued at all, since its
// request could only 403 and it would otherwise pin the head of the queue
// forever with a checked_at that nothing ever advances.
//
// Fails when: NULLS FIRST is dropped or the ordering reversed, and the
// never-checked account stops being first; the checked_at predicate is dropped,
// and the recently checked account reappears; or the scopes predicate is
// dropped, and the pre-Phase-2a grant is queued.
func TestListDueQueuesNeverCheckedFirstAndExcludesTheRest(t *testing.T) {
	e := harness.New(t)
	now := time.Date(2026, time.July, 31, 9, 30, 0, 0, time.UTC)

	never := e.NewUser("npdue-never")
	connectWithPlaybackScope(t, e, never.ID)

	stale := e.NewUser("npdue-stale")
	connectWithPlaybackScope(t, e, stale.ID)
	if err := e.Accounts.NowPlaying.RecordFailure(
		e.Ctx(), e.Store.DB(), stale.ID, now.Add(-time.Hour)); err != nil {
		t.Fatalf("RecordFailure: %v", err)
	}

	recent := e.NewUser("npdue-recent")
	connectWithPlaybackScope(t, e, recent.ID)
	if err := e.Accounts.NowPlaying.Record(
		e.Ctx(), e.Store.DB(), recent.ID, playingTrack(now.Add(-5*time.Second))); err != nil {
		t.Fatalf("Record: %v", err)
	}

	// A grant from before Phase 2a added user-read-playback-state to
	// DefaultScopes(): connected, healthy, never checked, and permanently
	// unpollable.
	noscope := e.NewUser("npdue-noscope")
	connectWithScopes(t, e, noscope.ID, time.Now().Add(time.Hour),
		[]string{"user-read-recently-played", "user-read-private"})

	due, err := e.Accounts.NowPlaying.ListDue(e.Ctx(), e.Store.DB(), now.Add(-30*time.Second), 100)
	if err != nil {
		t.Fatalf("ListDue: %v", err)
	}
	if len(due) != 2 {
		t.Fatalf("due = %+v, want exactly the never-checked and stale accounts", due)
	}
	if due[0].UserID != never.ID {
		t.Errorf("first due account = %s, want the never-checked account %s: a new "+
			"connection must not queue behind everybody else", due[0].UserID, never.ID)
	}
	if due[1].UserID != stale.ID {
		t.Errorf("second due account = %s, want the stale account %s", due[1].UserID, stale.ID)
	}
	for _, a := range due {
		if a.UserID == recent.ID {
			t.Error("an account checked within the interval must not be due again yet")
		}
		if a.UserID == noscope.ID {
			t.Error("an account whose grant lacks user-read-playback-state must never be " +
				"queued: the request could only 403, and nothing would ever advance its checked_at")
		}
	}
}

// TestListDueCarriesTheGrantItFilteredOn pins the other half of DueAccount:
// the scopes come back with the row.
//
// The poller re-checks them before spending a request, which is only possible
// because they are returned here — that is the defence in depth the type's own
// comment describes, and it is worthless if the column is not actually read.
//
// Fails when: DueAccount loses its Scopes field or the scan stops filling it —
// the poller's own guard then sees an empty grant and skips every account
// forever, and no other test would notice.
func TestListDueCarriesTheGrantItFilteredOn(t *testing.T) {
	e := harness.New(t)
	user := e.NewUser("npdue-scopes")
	connectWithPlaybackScope(t, e, user.ID)

	due, err := e.Accounts.NowPlaying.ListDue(e.Ctx(), e.Store.DB(), time.Now().UTC(), 100)
	if err != nil {
		t.Fatalf("ListDue: %v", err)
	}
	if len(due) != 1 {
		t.Fatalf("due = %+v, want the one connected account", due)
	}
	if !slices.Contains(due[0].Scopes, "user-read-playback-state") {
		t.Fatalf("Scopes = %v, want the stored grant, which the poller re-checks "+
			"before it spends a request", due[0].Scopes)
	}
}

// TestListDueRespectsItsLimit pins the bound one tick's work is taken under.
//
// Accounts come back least-recently-checked first, so what the limit leaves
// behind is picked up by the next tick rather than starved — but only if the
// limit is honoured at all.
//
// Fails when: the LIMIT is dropped from listDueSQL, and one tick takes on the
// whole instance at once.
func TestListDueRespectsItsLimit(t *testing.T) {
	e := harness.New(t)
	for i := range 3 {
		user := e.NewUser("npdue-limit-" + string(rune('a'+i)))
		connectWithPlaybackScope(t, e, user.ID)
	}

	due, err := e.Accounts.NowPlaying.ListDue(e.Ctx(), e.Store.DB(), time.Now().UTC(), 2)
	if err != nil {
		t.Fatalf("ListDue: %v", err)
	}
	if len(due) != 2 {
		t.Fatalf("due = %d accounts, want 2", len(due))
	}
}
