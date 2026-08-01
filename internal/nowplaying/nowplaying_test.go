package nowplaying

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os/exec"
	"strings"
	stdsync "sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/RequiDev/encore/internal/config"
	"github.com/RequiDev/encore/internal/domain"
	"github.com/RequiDev/encore/internal/spotify"
	"github.com/RequiDev/encore/internal/store"
	"github.com/RequiDev/encore/internal/store/accounts"
)

// TestThePollerCannotReachAnythingThatWritesAListen is the spec's read-only
// observer rule, made structural.
//
// §2.2: "/me/player must not create rows in listens." That is not a stylistic
// preference — the sync poller's correctness rests on its cursor advancing in
// the same transaction that commits the listens it covers, and a second writer
// with a different view of what has been played would produce duplicates the
// dedupe key catches by accident rather than by design.
//
// A comment saying so can be ignored. An import that does not exist cannot be
// used.
//
// The closure, not the direct imports. A deny-list over this package's own
// import statements answers a weaker question than the one being asked: reaching
// internal/store/listens through a package that merely re-exports a constructor
// is the same reachability by a longer path, and Go's linker cares no more about
// the length of it than a mistaken caller would. `go list -deps` is what the
// property actually means, so it is what is asserted — the direct-import form
// held at head only because nothing had yet added the intermediate package that
// would have made it wrong silently.
//
// Fails when: somebody adds a listens repository to Deps to "also record the
// play", imports internal/sync to reuse a helper from it, or pulls in any
// package that reaches one of those itself.
func TestThePollerCannotReachAnythingThatWritesAListen(t *testing.T) {
	// -deps of this package, one import path per line, the whole transitive
	// closure including the standard library. The test binary is not built, so
	// this file's own imports are not part of the answer — which is correct:
	// what a test may reach says nothing about what ships.
	out, err := exec.Command("go", "list", "-deps", ".").Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			t.Fatalf("go list -deps: %v: %s", err, exit.Stderr)
		}
		t.Fatalf("go list -deps: %v", err)
	}

	forbidden := []string{
		"github.com/RequiDev/encore/internal/store/listens",
		"github.com/RequiDev/encore/internal/sync",
		"github.com/RequiDev/encore/internal/importer",
	}
	var closure int
	for _, dep := range strings.Fields(string(out)) {
		closure++
		for _, bad := range forbidden {
			if dep == bad {
				t.Errorf("this package depends on %s, which can write listens; "+
					"the now-playing poller is a read-only observer and "+
					"/me/player/recently-played is the only ingestion path", dep)
			}
		}
	}
	// A `go list` that answered nothing would pass the loop above without
	// asserting anything at all.
	if closure == 0 {
		t.Fatal("go list -deps named no packages; the closure was never examined")
	}
}

// TestADisabledPollerNeverRuns is the binding half of the configuration
// contract: unset means the loop never runs at all, not that it runs and finds
// nothing to do.
//
// The context is deliberately never cancelled, so a Run that entered its loop
// would sit there until the deadline below rather than returning. That is what
// makes this test able to fail.
//
// Fails when: the Enabled() guard moves below the first timer, or is replaced by
// a default interval — Run then blocks and the deadline fires; or the guard is
// removed entirely, and the listing count below stops being zero.
func TestADisabledPollerNeverRuns(t *testing.T) {
	var checks, listings atomic.Int32
	w := newTestWatcher(t, config.NowPlaying{}, &checks, &listings, nil)

	done := make(chan error, 1)
	go func() { done <- w.Run(context.Background()) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run on a disabled poller returned %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return; with ENCORE_NOWPLAYING_INTERVAL unset the " +
			"poller must never run at all")
	}
	if got := checks.Load(); got != 0 {
		t.Errorf("%d Spotify requests were made by a disabled poller, want 0", got)
	}
	if got := listings.Load(); got != 0 {
		t.Errorf("%d account listings were made by a disabled poller, want 0", got)
	}
}

// TestAnAccountWithoutTheScopeIsSkippedWithoutARequest pins the spec's scope
// skip: the check happens before the request, not through a 403.
//
// Fails when: the HasScope guard in check() is removed — the request is then
// made, 403s, and costs a request to be told what the stored grant already
// said.
func TestAnAccountWithoutTheScopeIsSkippedWithoutARequest(t *testing.T) {
	var checks, listings atomic.Int32
	due := []accountsDue{{UserID: uuid.New(), Scopes: []string{"user-read-recently-played"}}}
	w := newTestWatcher(t, config.NowPlaying{Interval: 30 * time.Second}, &checks, &listings, due)

	polled, err := w.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if polled != 0 {
		t.Errorf("polled = %d, want 0", polled)
	}
	if got := checks.Load(); got != 0 {
		t.Fatalf("%d Spotify requests were made for an account without "+
			"user-read-playback-state, want 0", got)
	}
}

// TestObserveClassifiesEverythingSpotifyCanReturn is the single place the "what
// is playing" question is answered, so it is the single place every distinction
// the interface draws can be got wrong.
//
// Fails when: kindOf's ordering changes so an episode with no id is classified
// as a local file; the unknown-item scrub is removed and an advert's label
// leaks into a title; an empty item Type stops being read as a track; or a 204
// stops producing idle/none, which would put "nothing is playing" and "we do not
// know" into the same row shape.
func TestObserveClassifiesEverythingSpotifyCanReturn(t *testing.T) {
	at := time.Date(2026, time.July, 31, 9, 30, 0, 0, time.UTC)
	ms := func(n int) *int { return &n }

	tests := []struct {
		name string
		in   *spotify.Playback
		want domain.NowPlaying
	}{
		{
			name: "204 no content is an idle player, not a failure",
			in:   nil,
			want: domain.NowPlaying{
				ObservedAt: at, CheckedAt: at,
				State: domain.PlaybackIdle, Kind: domain.PlaybackItemNone,
			},
		},
		{
			name: "a track, playing",
			in: &spotify.Playback{
				IsPlaying: true, ProgressMs: ms(161000), CurrentlyPlayingType: "track",
				Device: &spotify.Device{Name: "Kitchen speaker", Type: "Speaker"},
				Item: &spotify.PlaybackItem{
					ID: "track-1", Name: "The Wheel", Type: "track", DurationMs: 255000,
					Artists: []spotify.Artist{{Name: "SOHN"}},
				},
			},
			want: domain.NowPlaying{
				ObservedAt: at, CheckedAt: at,
				State: domain.PlaybackPlaying, Kind: domain.PlaybackItemTrack,
				TrackID: "track-1", Title: "The Wheel", Artist: "SOHN",
				ProgressMs: ms(161000), DurationMs: ms(255000),
				DeviceName: "Kitchen speaker",
			},
		},
		{
			name: "a track, paused",
			in: &spotify.Playback{
				IsPlaying: false, ProgressMs: ms(1000), CurrentlyPlayingType: "track",
				Item: &spotify.PlaybackItem{
					ID: "track-1", Name: "The Wheel", Type: "track", DurationMs: 255000,
					Artists: []spotify.Artist{{Name: "SOHN"}, {Name: "Kwabs"}},
				},
			},
			want: domain.NowPlaying{
				ObservedAt: at, CheckedAt: at,
				State: domain.PlaybackPaused, Kind: domain.PlaybackItemTrack,
				TrackID: "track-1", Title: "The Wheel", Artist: "SOHN, Kwabs",
				ProgressMs: ms(1000), DurationMs: ms(255000),
			},
		},
		{
			name: "a local file has a name and no catalogue id",
			in: &spotify.Playback{
				IsPlaying: true, CurrentlyPlayingType: "track",
				Item: &spotify.PlaybackItem{
					Name: "demo-2004.mp3", Type: "track", IsLocal: true, DurationMs: 180000,
					Artists: []spotify.Artist{{Name: "Unreleased"}},
				},
			},
			want: domain.NowPlaying{
				ObservedAt: at, CheckedAt: at,
				State: domain.PlaybackPlaying, Kind: domain.PlaybackItemLocal,
				Title: "demo-2004.mp3", Artist: "Unreleased", DurationMs: ms(180000),
			},
		},
		{
			name: "a podcast episode names its show rather than an artist",
			in: &spotify.Playback{
				IsPlaying: true, CurrentlyPlayingType: "episode",
				Item: &spotify.PlaybackItem{
					ID: "ep-1", Name: "The one about ducks", Type: "episode",
					DurationMs: 3600000, Show: &spotify.Show{Name: "Ducks Weekly"},
				},
			},
			want: domain.NowPlaying{
				ObservedAt: at, CheckedAt: at,
				State: domain.PlaybackPlaying, Kind: domain.PlaybackItemEpisode,
				Title: "The one about ducks", Artist: "Ducks Weekly",
				DurationMs: ms(3600000),
			},
		},
		{
			name: "an advert has no item at all",
			in: &spotify.Playback{
				IsPlaying: true, CurrentlyPlayingType: "ad", Item: nil,
				Device: &spotify.Device{Name: "Kitchen speaker"},
			},
			want: domain.NowPlaying{
				ObservedAt: at, CheckedAt: at,
				State: domain.PlaybackPlaying, Kind: domain.PlaybackItemUnknown,
				DeviceName: "Kitchen speaker",
			},
		},
		{
			name: "a type this client does not know keeps none of its description",
			in: &spotify.Playback{
				IsPlaying: true, CurrentlyPlayingType: "unknown",
				Item: &spotify.PlaybackItem{
					ID: "ch-1", Name: "Chapter 4", Type: "chapter", DurationMs: 900000,
				},
			},
			want: domain.NowPlaying{
				ObservedAt: at, CheckedAt: at,
				State: domain.PlaybackPlaying, Kind: domain.PlaybackItemUnknown,
			},
		},
		{
			name: "a 200 carrying neither an item nor a type is an idle player",
			in:   &spotify.Playback{IsPlaying: false},
			want: domain.NowPlaying{
				ObservedAt: at, CheckedAt: at,
				State: domain.PlaybackIdle, Kind: domain.PlaybackItemNone,
			},
		},
		{
			name: "a track with no id is a local file however Spotify labels it",
			in: &spotify.Playback{
				IsPlaying: true, CurrentlyPlayingType: "track",
				Item: &spotify.PlaybackItem{Name: "Untitled", Type: "", DurationMs: 1000},
			},
			want: domain.NowPlaying{
				ObservedAt: at, CheckedAt: at,
				State: domain.PlaybackPlaying, Kind: domain.PlaybackItemLocal,
				Title: "Untitled", DurationMs: ms(1000),
			},
		},
		// The two cases below are not in the brief's table and are the reason
		// this comment exists: the "Fails when" line above names two mutations
		// that the nine cases above do not actually catch.
		//
		// Every episode above carries an id, so moving kindOf's local-file test
		// ahead of its episode test changes nothing for any of them; and the only
		// item above with an empty Type also has no id, so it is classified local
		// before the empty-Type-is-a-track branch is ever consulted. Each of the
		// two orderings kindOf's own comment calls load bearing was therefore
		// unpinned. These two cases pin them.
		{
			name: "an episode without an id is still an episode, not a local file",
			in: &spotify.Playback{
				IsPlaying: true, CurrentlyPlayingType: "episode",
				Item: &spotify.PlaybackItem{
					Name: "The one about ducks", Type: "episode",
					DurationMs: 3600000, Show: &spotify.Show{Name: "Ducks Weekly"},
				},
			},
			want: domain.NowPlaying{
				ObservedAt: at, CheckedAt: at,
				State: domain.PlaybackPlaying, Kind: domain.PlaybackItemEpisode,
				Title: "The one about ducks", Artist: "Ducks Weekly",
				DurationMs: ms(3600000),
			},
		},
		{
			name: "a catalogue track Spotify sent without a type is still a track",
			in: &spotify.Playback{
				IsPlaying: true, CurrentlyPlayingType: "track",
				Item: &spotify.PlaybackItem{
					ID: "track-2", Name: "Artifice", Type: "", DurationMs: 240000,
					Artists: []spotify.Artist{{Name: "SOHN"}},
				},
			},
			want: domain.NowPlaying{
				ObservedAt: at, CheckedAt: at,
				State: domain.PlaybackPlaying, Kind: domain.PlaybackItemTrack,
				TrackID: "track-2", Title: "Artifice", Artist: "SOHN",
				DurationMs: ms(240000),
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := observe(tc.in, at)
			if got.State != tc.want.State || got.Kind != tc.want.Kind {
				t.Fatalf("State/Kind = %q/%q, want %q/%q",
					got.State, got.Kind, tc.want.State, tc.want.Kind)
			}
			if got.Title != tc.want.Title || got.Artist != tc.want.Artist {
				t.Errorf("Title/Artist = %q/%q, want %q/%q",
					got.Title, got.Artist, tc.want.Title, tc.want.Artist)
			}
			if got.TrackID != tc.want.TrackID {
				t.Errorf("TrackID = %q, want %q", got.TrackID, tc.want.TrackID)
			}
			if got.DeviceName != tc.want.DeviceName {
				t.Errorf("DeviceName = %q, want %q", got.DeviceName, tc.want.DeviceName)
			}
			if !samePtr(got.ProgressMs, tc.want.ProgressMs) {
				t.Errorf("ProgressMs = %v, want %v", got.ProgressMs, tc.want.ProgressMs)
			}
			if !samePtr(got.DurationMs, tc.want.DurationMs) {
				t.Errorf("DurationMs = %v, want %v", got.DurationMs, tc.want.DurationMs)
			}
			if !got.ObservedAt.Equal(tc.want.ObservedAt) || !got.CheckedAt.Equal(tc.want.CheckedAt) {
				t.Errorf("ObservedAt/CheckedAt = %v/%v, want both %v",
					got.ObservedAt, got.CheckedAt, at)
			}
		})
	}
}

func samePtr(a, b *int) bool {
	switch {
	case a == nil && b == nil:
		return true
	case a == nil || b == nil:
		return false
	default:
		return *a == *b
	}
}

// TestARateLimitedPollIsNotRetriedAndKeepsWhatWasAlreadyKnown is the third
// structural guarantee, at the level this package controls.
//
// internal/spotify already gives this endpoint a budget of its own, so a 429
// here pauses nothing but this loop. What is left for this package is the other
// half: that it does not route around the pause by retrying, and that it does
// not throw the last observation away over it. A *spotify.PausedError is the
// answer a paused budget returns immediately, and the only correct response to
// "no request will reach Spotify for the next while" is to stop asking.
//
// Fails when: check() grows a retry around CurrentlyPlaying — the request count
// below stops being one; or the failure path calls Record instead of
// RecordFailure, which would replace a true observation with an empty one.
func TestARateLimitedPollIsNotRetriedAndKeepsWhatWasAlreadyKnown(t *testing.T) {
	user := uuid.New()
	api := &fakeSpotify{err: &spotify.PausedError{Until: time.Now().Add(time.Hour)}}
	obs := &fakeObservations{due: []accountsDue{playbackAccount(user)}}
	w := newWatcherWith(t, config.NowPlaying{Interval: 30 * time.Second}, api, obs, &fakeTokens{})

	polled, err := w.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if polled != 0 {
		t.Errorf("polled = %d, want 0: a rate-limited check observed nothing", polled)
	}
	if got := api.calls.Load(); got != 1 {
		t.Errorf("%d Spotify requests, want exactly 1: a paused budget must not be "+
			"retried against, which would only queue up the next rejection", got)
	}
	if got := obs.recordCount(); got != 0 {
		t.Errorf("Record was called %d times on a rate-limited check; a failure must "+
			"never overwrite the last known playback", got)
	}
	if got := obs.failureCount(user); got != 1 {
		t.Errorf("RecordFailure calls = %d, want 1", got)
	}
}

// TestAForbiddenPollIsRecordedOnTheRowAndNotRetried pins the rule
// internal/sync/account.go's forbidden() states for an optional read scope.
//
// This package cannot park an account — Deps names Observations, whose three
// methods touch one table — so what is left to pin here is the rest of the rule:
// no retry, and the failure lands on that listener's own row rather than
// anywhere the recently-played sync would notice.
//
// Fails when: a 403 is special-cased into anything other than an ordinary failed
// check, or is retried.
func TestAForbiddenPollIsRecordedOnTheRowAndNotRetried(t *testing.T) {
	user := uuid.New()
	api := &fakeSpotify{err: &spotify.APIError{StatusCode: http.StatusForbidden}}
	obs := &fakeObservations{due: []accountsDue{playbackAccount(user)}}
	w := newWatcherWith(t, config.NowPlaying{Interval: 30 * time.Second}, api, obs, &fakeTokens{})

	if _, err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce = %v, want nil: one account's refused grant is not the tick's failure", err)
	}
	if got := api.calls.Load(); got != 1 {
		t.Errorf("%d Spotify requests, want exactly 1: a scope failure spends quota to "+
			"fail identically every time", got)
	}
	if got := obs.failureCount(user); got != 1 {
		t.Errorf("RecordFailure calls = %d, want 1", got)
	}
	if got := obs.recordCount(); got != 0 {
		t.Errorf("Record was called %d times for an account Spotify refused", got)
	}
}

// TestOneAccountsFailureDoesNotStopTheOthers pins the isolation RunOnce's
// comment claims: no shared error group, so a broken grant costs its owner their
// card and costs nobody else theirs.
//
// Fails when: RunOnce is rewritten with an errgroup, or check() starts returning
// an error that aborts the loop — the two healthy accounts below then go
// unchecked depending on dispatch order.
func TestOneAccountsFailureDoesNotStopTheOthers(t *testing.T) {
	broken, first, second := uuid.New(), uuid.New(), uuid.New()
	api := &fakeSpotify{
		respond: func(_ context.Context, token string) (*spotify.Playback, error) {
			if token == tokenFor(broken) {
				return nil, errors.New("connection reset by peer")
			}
			return playing("track-1", "The Wheel"), nil
		},
	}
	obs := &fakeObservations{due: []accountsDue{
		playbackAccount(first), playbackAccount(broken), playbackAccount(second),
	}}
	w := newWatcherWith(t, config.NowPlaying{Interval: 30 * time.Second}, api, obs, &fakeTokens{})

	polled, err := w.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce = %v, want nil", err)
	}
	if polled != 2 {
		t.Errorf("polled = %d, want 2: one account's failure must not cost the others theirs", polled)
	}
	if got := api.calls.Load(); got != 3 {
		t.Errorf("%d Spotify requests, want 3: every due account is checked", got)
	}
	for _, id := range []uuid.UUID{first, second} {
		if _, ok := obs.recorded(id); !ok {
			t.Errorf("account %s was never recorded", id)
		}
	}
	if got := obs.failureCount(broken); got != 1 {
		t.Errorf("RecordFailure calls for the broken account = %d, want 1", got)
	}
	if _, ok := obs.recorded(broken); ok {
		t.Error("the broken account had an observation recorded despite its check failing")
	}
}

// TestATickNeverPresentsMoreThanItsConcurrencyAtOnce pins the semaphore, and
// with it the reason a tick is safe to run across a whole instance: a hundred
// accounts becoming due at the same second must not become a hundred
// simultaneous requests.
//
// It also pins that RunOnce joins every goroutine it starts: in flight is read
// after RunOnce has returned, so a non-zero value means one is still running
// with nobody waiting on it.
//
// Fails when: the semaphore is removed or widened, or wg.Wait() is dropped.
func TestATickNeverPresentsMoreThanItsConcurrencyAtOnce(t *testing.T) {
	// Enough accounts that an unbounded dispatch is obvious, and a response slow
	// enough that overlapping calls actually overlap.
	const accounts = 40
	due := make([]accountsDue, 0, accounts)
	for range accounts {
		due = append(due, playbackAccount(uuid.New()))
	}
	api := &fakeSpotify{
		respond: func(context.Context, string) (*spotify.Playback, error) {
			time.Sleep(2 * time.Millisecond)
			return playing("track-1", "The Wheel"), nil
		},
	}
	obs := &fakeObservations{due: due}
	w := newWatcherWith(t, config.NowPlaying{Interval: 30 * time.Second}, api, obs, &fakeTokens{})

	polled, err := w.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if polled != accounts {
		t.Errorf("polled = %d, want %d", polled, accounts)
	}
	if got := api.peak.Load(); got > concurrency {
		t.Errorf("%d requests were in flight at once, want at most %d", got, concurrency)
	}
	if got := api.inFlight.Load(); got != 0 {
		t.Errorf("%d checks were still running after RunOnce returned; every goroutine "+
			"a tick starts must be joined before it reports", got)
	}
}

// TestASlowPollDoesNotWedgeShutdown is the shutdown half of the same property.
//
// The fake below blocks until its context is cancelled, which is what a real
// *spotify.Client call does: the request, the retry sleep and the rate limiter's
// own wait are all context-aware. RunOnce must therefore return once the process
// is stopping rather than sitting out an interval's worth of in-flight requests,
// and it must have joined every goroutine before it does.
//
// A check interrupted this way records nothing at all: the account is simply
// left due, and the next process picks it up. Marking it failed would report a
// fault of Encore's own shutdown as a fault of the listener's connection.
//
// Fails when: check() drops its ctx.Err() guards and records a failure for every
// account that was in flight when the process stopped, or RunOnce stops waiting
// on its WaitGroup and returns with checks still running.
func TestASlowPollDoesNotWedgeShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	entered := make(chan struct{}, concurrency)
	api := &fakeSpotify{
		respond: func(ctx context.Context, _ string) (*spotify.Playback, error) {
			select {
			case entered <- struct{}{}:
			default:
			}
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}
	due := make([]accountsDue, 0, 20)
	for range 20 {
		due = append(due, playbackAccount(uuid.New()))
	}
	obs := &fakeObservations{due: due}
	w := newWatcherWith(t, config.NowPlaying{Interval: 30 * time.Second}, api, obs, &fakeTokens{})

	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := w.RunOnce(ctx); err != nil {
			t.Errorf("RunOnce = %v, want nil", err)
		}
	}()

	// Wait until a check is genuinely blocked inside Spotify before stopping, so
	// this is a shutdown mid-poll rather than one that beat the dispatch loop.
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("no check ever reached the Spotify call")
	}
	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("RunOnce did not return after its context was cancelled; a poll in " +
			"flight must never hold up shutdown")
	}
	if got := api.inFlight.Load(); got != 0 {
		t.Errorf("%d checks were still running after RunOnce returned", got)
	}
	if got := obs.totalFailures(); got != 0 {
		t.Errorf("%d failures were recorded for checks that were interrupted by shutdown; "+
			"the account is not at fault and the next tick picks it up", got)
	}
	if got := obs.recordCount(); got != 0 {
		t.Errorf("%d observations were recorded by interrupted checks", got)
	}
}

// TestAShutdownDuringATokenRefreshRecordsNothingEither is the same rule one step
// earlier in the check.
//
// A token refresh reads and writes the credentials row, so a process stopping
// mid-refresh gets a context error back from *sync.Poller.AccessToken exactly as
// it does from a Spotify call. Recording that as a failed check would report
// Encore's own shutdown as a fault of the listener's connection, and would move
// checked_at far enough forward that the account waits a whole extra interval
// after the restart for no reason.
//
// Fails when: the ctx.Err() guard on the token path is removed, leaving the two
// halves of check() disagreeing about what a shutdown is.
func TestAShutdownDuringATokenRefreshRecordsNothingEither(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	entered := make(chan struct{}, concurrency)
	api := &fakeSpotify{}
	tokens := &fakeTokens{respond: func(ctx context.Context) (string, error) {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-ctx.Done()
		return "", ctx.Err()
	}}
	due := make([]accountsDue, 0, 20)
	for range 20 {
		due = append(due, playbackAccount(uuid.New()))
	}
	obs := &fakeObservations{due: due}
	w := newWatcherWith(t, config.NowPlaying{Interval: 30 * time.Second}, api, obs, tokens)

	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := w.RunOnce(ctx); err != nil {
			t.Errorf("RunOnce = %v, want nil", err)
		}
	}()

	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("no check ever reached the token refresh")
	}
	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("RunOnce did not return after its context was cancelled")
	}
	if got := obs.totalFailures(); got != 0 {
		t.Errorf("%d failures were recorded for token refreshes that shutdown interrupted; "+
			"the account is not at fault and the next tick picks it up", got)
	}
	if got := api.calls.Load(); got != 0 {
		t.Errorf("%d Spotify requests were made without a token", got)
	}
}

// TestRunStopsWhenItsContextIsCancelled pins that the loop is stoppable at any
// point in its schedule, not only while it happens to be between ticks.
//
// The assertion is that Run returns, not that it returned after any particular
// delay: the property is that a cancelled worker stops, and pinning an interval
// value here would pass just as happily against a loop that ignored ctx and
// simply had a short timer.
//
// Fails when: Run's select loses its ctx.Done() case, or RunOnce is called
// without a cancellable context — Run then keeps polling after shutdown and the
// deadline below fires.
func TestRunStopsWhenItsContextIsCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	obs := &fakeObservations{due: []accountsDue{playbackAccount(uuid.New())}}
	api := &fakeSpotify{playback: playing("track-1", "The Wheel")}
	w := newWatcherWith(t, config.NowPlaying{Interval: 30 * time.Second}, api, obs, &fakeTokens{})
	// The first delay is drawn from the whole interval; a zero draw makes the
	// first tick immediate so this test observes a running loop rather than one
	// still waiting to start.
	w.rnd = func() float64 { return 0 }

	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()

	// One completed tick is the proof the loop was actually running when it was
	// stopped, rather than cancelled before it began.
	deadline := time.After(5 * time.Second)
	for api.calls.Load() == 0 {
		select {
		case <-deadline:
			t.Fatal("the loop never made its first check")
		case <-time.After(time.Millisecond):
		}
	}
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v, want nil on cancellation", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after its context was cancelled")
	}
}

// TestTheQueueIsAskedWithTheSameToleranceTheScheduleIsDrawnWith pins that the
// two halves of the schedule agree.
//
// nextDelay draws each tick from a band around the interval. A due predicate
// demanding a whole interval since the last check would reject every tick from
// the lower half of that band, so half of all ticks would do nothing and the
// account would wait for the one after — a mean effective period of about one
// and a half intervals, and up to two. An operator who asked for thirty seconds
// would silently get forty-five.
//
// The assertion is a relation between the two, not a number: whatever the
// soonest tick nextDelay can produce is, the cut-off RunOnce asks the queue for
// must be no earlier than that. Anything earlier is a band of delays that find
// nobody due.
//
// The two meet exactly at the boundary, and ListDue's predicate is a strict
// checked_at < olderThan, so an account checked at precisely the soonest instant
// is not due — a case of measure zero against a delay drawn from a continuous
// distribution, and pinned below as "a hair over the soonest is due" rather than
// papered over with a fudge factor in dueSlack.
//
// Fails when: dueSlack is dropped from RunOnce's olderThan, or the jitter widens
// past the slack the queue is asked with.
func TestTheQueueIsAskedWithTheSameToleranceTheScheduleIsDrawnWith(t *testing.T) {
	const interval = 30 * time.Second
	obs := &fakeObservations{}
	w := newWatcherWith(t, config.NowPlaying{Interval: interval}, &fakeSpotify{}, obs, &fakeTokens{})

	// The soonest a tick can arrive after the previous one.
	w.rnd = func() float64 { return 0 }
	soonest := w.nextDelay()
	if soonest >= interval {
		t.Fatalf("the soonest delay is %s, which is not below the interval %s; this "+
			"test has nothing to say unless the jitter can fire early", soonest, interval)
	}

	if _, err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	olderThan := obs.lastOlderThan()

	if olderThan.Before(testNow.Add(-soonest)) {
		t.Fatalf("RunOnce asked for checks older than %s, but a tick can arrive as soon as "+
			"%s after the last one (at %s). Every tick in that band finds nobody due and "+
			"the account waits for the tick after, so the real refresh period is around "+
			"1.5 intervals rather than one.",
			olderThan, soonest, testNow.Add(-soonest))
	}
	// The consequence, stated as the queue itself would answer it.
	if lastChecked := testNow.Add(-soonest - time.Millisecond); !lastChecked.Before(olderThan) {
		t.Fatalf("an account last checked %s ago is not due, though a tick can fire at %s",
			testNow.Sub(lastChecked), soonest)
	}
}

// TestADisabledPollerListsNobodyEvenWhenDrivenDirectly is the exported half of
// the configuration contract.
//
// Run's guard covers the loop; this covers everything else that can reach
// RunOnce — a later phase's "refresh now" control, a supervisor, a test. Unset
// means the instance never opted in, and a zero interval also degenerates the
// due predicate into "every connected account, every time", so a caller that got
// through here would poll the whole instance at once.
//
// Fails when: the Enabled() guard is only in Run.
func TestADisabledPollerListsNobodyEvenWhenDrivenDirectly(t *testing.T) {
	var checks, listings atomic.Int32
	due := []accountsDue{playbackAccount(uuid.New())}
	w := newTestWatcher(t, config.NowPlaying{}, &checks, &listings, due)

	polled, err := w.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce on a disabled poller: %v", err)
	}
	if polled != 0 {
		t.Errorf("polled = %d, want 0", polled)
	}
	if got := listings.Load(); got != 0 {
		t.Errorf("%d account listings were made by a disabled poller driven directly, want 0", got)
	}
	if got := checks.Load(); got != 0 {
		t.Errorf("%d Spotify requests were made by a disabled poller driven directly, want 0", got)
	}
}

// TestAShutdownBetweenTheResponseAndTheWriteIsNotAnError is the third of the
// three places a check can be interrupted, and the one the other two guards do
// not cover: Spotify answered, and the process stopped before the row was
// written.
//
// Nothing is lost — the account keeps its previous observation and stays due —
// so this belongs at the same severity as the other two interruptions, which is
// none. Error is what an operator is asked to act on, and a line every clean
// shutdown produces teaches them to ignore the level.
//
// Fails when: the ctx.Err() guard on the Record path is removed, leaving the
// success path disagreeing with the two above it about what a shutdown is.
func TestAShutdownBetweenTheResponseAndTheWriteIsNotAnError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var logs lockedBuffer
	obs := &fakeObservations{due: []accountsDue{playbackAccount(uuid.New())}}
	// A pool query fails exactly this way once its context is gone.
	obs.recFn = func(ctx context.Context) error { return ctx.Err() }
	api := &fakeSpotify{respond: func(context.Context, string) (*spotify.Playback, error) {
		// The process stops between Spotify answering and Encore writing.
		cancel()
		return playing("track-1", "The Wheel"), nil
	}}
	w := newWatcherLogging(t, config.NowPlaying{Interval: 30 * time.Second}, api, obs, &fakeTokens{},
		slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))

	if _, err := w.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce = %v, want nil", err)
	}
	if got := logs.String(); strings.Contains(got, "level=ERROR") {
		t.Fatalf("a shutdown between the response and the write was logged at Error:\n%s", got)
	}
}

// TestATickThatCannotListItsWorkReportsItRatherThanSpinning pins that a database
// failure is the tick's error, not an account's: nothing is recorded against
// anybody, and Run logs it and waits rather than retrying immediately.
//
// Fails when: RunOnce swallows the listing error and returns (0, nil), which
// would make a database outage indistinguishable from an idle instance in the
// logs.
func TestATickThatCannotListItsWorkReportsItRatherThanSpinning(t *testing.T) {
	obs := &fakeObservations{listErr: errors.New("connection refused")}
	api := &fakeSpotify{}
	w := newWatcherWith(t, config.NowPlaying{Interval: 30 * time.Second}, api, obs, &fakeTokens{})

	polled, err := w.RunOnce(context.Background())
	if err == nil {
		t.Fatal("RunOnce = nil, want the listing error reported")
	}
	if polled != 0 {
		t.Errorf("polled = %d, want 0", polled)
	}
	if got := api.calls.Load(); got != 0 {
		t.Errorf("%d Spotify requests were made by a tick that never listed its work", got)
	}
}

// TestAnAccountWhoseTokenCannotBeRefreshedIsRecordedWithoutARequest pins that
// the failure is contained on the row: no Spotify request is spent on an account
// whose token could not be obtained, and the failure is noted so the card can
// say the display is stale.
//
// Fails when: check() asks Spotify anyway with an empty token.
func TestAnAccountWhoseTokenCannotBeRefreshedIsRecordedWithoutARequest(t *testing.T) {
	user := uuid.New()
	api := &fakeSpotify{}
	obs := &fakeObservations{due: []accountsDue{playbackAccount(user)}}
	w := newWatcherWith(t, config.NowPlaying{Interval: 30 * time.Second}, api, obs,
		&fakeTokens{err: errors.New("spotify rejected the refresh token")})

	if _, err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce = %v, want nil", err)
	}
	if got := api.calls.Load(); got != 0 {
		t.Errorf("%d Spotify requests were made without a token", got)
	}
	if got := obs.failureCount(user); got != 1 {
		t.Errorf("RecordFailure calls = %d, want 1", got)
	}
}

// TestNewRefusesToBuildAWatcherThatCannotWork keeps a missing collaborator from
// becoming a nil dereference inside a background loop, where the only symptom is
// a supervisor restarting something forever.
func TestNewRefusesToBuildAWatcherThatCannotWork(t *testing.T) {
	full := func() Deps {
		return Deps{
			Store:      &store.Store{},
			NowPlaying: &fakeObservations{},
			Spotify:    &fakeSpotify{},
			Tokens:     &fakeTokens{},
			Logger:     slog.New(slog.DiscardHandler),
		}
	}
	tests := map[string]func(*Deps){
		"no store":      func(d *Deps) { d.Store = nil },
		"no repository": func(d *Deps) { d.NowPlaying = nil },
		"no spotify":    func(d *Deps) { d.Spotify = nil },
		"no tokens":     func(d *Deps) { d.Tokens = nil },
	}
	for name, break_ := range tests {
		t.Run(name, func(t *testing.T) {
			deps := full()
			break_(&deps)
			if _, err := New(config.NowPlaying{Interval: time.Minute}, deps); err == nil {
				t.Fatal("New accepted a Deps it cannot work with")
			}
		})
	}

	// A missing logger and clock are defaulted rather than refused, since both
	// have a sensible answer.
	w, err := New(config.NowPlaying{Interval: time.Minute}, Deps{
		Store:      &store.Store{},
		NowPlaying: &fakeObservations{},
		Spotify:    &fakeSpotify{},
		Tokens:     &fakeTokens{},
	})
	if err != nil {
		t.Fatalf("New without a logger or clock: %v", err)
	}
	if w.now == nil || w.log == nil || w.rnd == nil {
		t.Fatal("New left a defaulted field nil")
	}
}

// TestHasScopeReadsALegacySpaceJoinedGrant pins the one storage shape a grant
// can take besides one scope per element, for the reason spotify.MissingScopes
// gives: Spotify returns granted scopes space-separated in a single string, and
// an account connected before Encore split them has exactly that in its column.
//
// Fails when: hasScope compares whole elements — a legacy account then reads as
// having granted nothing and is skipped forever.
func TestHasScopeReadsALegacySpaceJoinedGrant(t *testing.T) {
	tests := []struct {
		name    string
		granted []string
		want    bool
	}{
		{"one scope per element", []string{"user-read-email", scopeReadPlaybackState}, true},
		{"a legacy space-joined grant", []string{"user-read-email user-read-playback-state"}, true},
		{"absent", []string{"user-read-recently-played", "user-top-read"}, false},
		{"nothing granted", nil, false},
		{"a prefix is not a grant", []string{"user-read-playback"}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := hasScope(tc.granted, scopeReadPlaybackState); got != tc.want {
				t.Errorf("hasScope(%q) = %v, want %v", tc.granted, got, tc.want)
			}
		})
	}
}

// accountsDue is accounts.DueAccount under a shorter name, so the tables above
// read without the package qualifier.
type accountsDue = accounts.DueAccount

// testNow is the fixed clock every test runs on, so nothing here depends on when
// it is executed.
var testNow = time.Date(2026, time.July, 31, 9, 30, 0, 0, time.UTC)

// playbackAccount is one account whose grant carries the scope this poller
// needs, so each test varies only what it is about.
func playbackAccount(id uuid.UUID) accountsDue {
	return accountsDue{UserID: id, Scopes: []string{"user-read-recently-played", scopeReadPlaybackState}}
}

// playing is one healthy Spotify answer.
func playing(id, title string) *spotify.Playback {
	progress := 161000
	return &spotify.Playback{
		IsPlaying: true, ProgressMs: &progress, CurrentlyPlayingType: "track",
		Item: &spotify.PlaybackItem{
			ID: id, Name: title, Type: "track", DurationMs: 255000,
			Artists: []spotify.Artist{{Name: "SOHN"}},
		},
	}
}

// tokenFor is the token fakeTokens mints for one account. Tokens are per-account
// so a fake Spotify can tell its callers apart: CurrentlyPlaying is handed a
// token and nothing else, exactly as the real client is.
func tokenFor(id uuid.UUID) string { return "token-" + id.String() }

// fakeSpotify satisfies SpotifyAPI without a network, and counts what was
// actually asked of it. The call count is the decisive evidence for the scope
// check and the no-retry rules: both exist so that a request is never spent.
type fakeSpotify struct {
	// mirror is incremented alongside calls, for a test handed only a counter.
	mirror *atomic.Int32
	calls  atomic.Int32
	// inFlight and peak record concurrency: peak is the most that were ever
	// running at once, inFlight what is running now.
	inFlight, peak atomic.Int32

	// respond answers one request when set, keyed by access token so a test can
	// make one account fail while its neighbours succeed. Otherwise every
	// account gets playback and err below.
	respond  func(ctx context.Context, token string) (*spotify.Playback, error)
	playback *spotify.Playback
	err      error
}

func (f *fakeSpotify) CurrentlyPlaying(ctx context.Context, token string) (*spotify.Playback, error) {
	f.calls.Add(1)
	if f.mirror != nil {
		f.mirror.Add(1)
	}
	n := f.inFlight.Add(1)
	for {
		p := f.peak.Load()
		if n <= p || f.peak.CompareAndSwap(p, n) {
			break
		}
	}
	defer f.inFlight.Add(-1)

	if f.respond != nil {
		return f.respond(ctx, token)
	}
	return f.playback, f.err
}

// fakeTokens satisfies Tokens with a per-account token. The real refresh dance
// belongs to *sync.Poller and is exercised by its own tests; this package only
// ever calls this one method.
type fakeTokens struct {
	err   error
	calls atomic.Int32
	// respond answers instead of the fixed token when set, so a test can hold a
	// refresh open the way a real one holds a database round trip open.
	respond func(ctx context.Context) (string, error)
}

func (f *fakeTokens) NowPlayingAccessToken(ctx context.Context, userID uuid.UUID) (string, error) {
	f.calls.Add(1)
	if f.respond != nil {
		return f.respond(ctx)
	}
	if f.err != nil {
		return "", f.err
	}
	return tokenFor(userID), nil
}

// fakeObservations satisfies Observations without a database, and records what
// each account was written.
//
// It deliberately implements nothing else: the whole point of Deps naming this
// interface is that there is no other table within reach, and a fake with more
// methods than the interface would quietly invite one.
type fakeObservations struct {
	due     []accountsDue
	listErr error
	recErr  error
	// recFn fails a write the way the pool does when the caller's context is
	// already gone, which a fixed error cannot express.
	recFn func(ctx context.Context) error

	mu        stdsync.Mutex
	listings  int
	olderThan time.Time
	mirror    *atomic.Int32
	successes map[uuid.UUID]domain.NowPlaying
	failures  map[uuid.UUID]int
}

func (f *fakeObservations) ListDue(
	_ context.Context, _ store.Querier, olderThan time.Time, limit int,
) ([]accountsDue, error) {
	f.mu.Lock()
	f.listings++
	f.olderThan = olderThan
	f.mu.Unlock()
	if f.mirror != nil {
		f.mirror.Add(1)
	}
	if f.listErr != nil {
		return nil, f.listErr
	}
	if limit < len(f.due) {
		return f.due[:limit], nil
	}
	return f.due, nil
}

// lastOlderThan is the cut-off the poller last asked the queue for, which is
// where the schedule and the due predicate meet.
func (f *fakeObservations) lastOlderThan() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.olderThan
}

func (f *fakeObservations) Record(
	ctx context.Context, _ store.Querier, userID uuid.UUID, n domain.NowPlaying,
) error {
	if f.recFn != nil {
		if err := f.recFn(ctx); err != nil {
			return err
		}
	}
	if f.recErr != nil {
		return f.recErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.successes == nil {
		f.successes = make(map[uuid.UUID]domain.NowPlaying)
	}
	f.successes[userID] = n
	return nil
}

func (f *fakeObservations) RecordFailure(
	_ context.Context, _ store.Querier, userID uuid.UUID, _ time.Time,
) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failures == nil {
		f.failures = make(map[uuid.UUID]int)
	}
	f.failures[userID]++
	return nil
}

func (f *fakeObservations) recorded(userID uuid.UUID) (domain.NowPlaying, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n, ok := f.successes[userID]
	return n, ok
}

func (f *fakeObservations) recordCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.successes)
}

func (f *fakeObservations) failureCount(userID uuid.UUID) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.failures[userID]
}

func (f *fakeObservations) totalFailures() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	var n int
	for _, c := range f.failures {
		n += c
	}
	return n
}

// newTestWatcher builds a Watcher over counters alone, for the tests whose whole
// assertion is that a number stayed at zero.
func newTestWatcher(
	t *testing.T, cfg config.NowPlaying, checks, listings *atomic.Int32, due []accountsDue,
) *Watcher {
	t.Helper()
	return newWatcherWith(t, cfg,
		&fakeSpotify{mirror: checks},
		&fakeObservations{due: due, mirror: listings},
		&fakeTokens{})
}

// newWatcherWith builds a Watcher over fakes the caller keeps a handle on.
//
// Store is a zero *store.Store: every method this package calls on it is DB(),
// whose result is handed straight to Observations, and the fake above never
// looks at it. That is what lets the whole loop be exercised without a database.
func newWatcherWith(
	t *testing.T, cfg config.NowPlaying, api *fakeSpotify, obs *fakeObservations, tokens *fakeTokens,
) *Watcher {
	t.Helper()
	return newWatcherLogging(t, cfg, api, obs, tokens, slog.New(slog.DiscardHandler))
}

// newWatcherLogging is newWatcherWith for the one test whose assertion is about
// what was logged and at what level.
func newWatcherLogging(
	t *testing.T, cfg config.NowPlaying, api *fakeSpotify, obs *fakeObservations,
	tokens *fakeTokens, log *slog.Logger,
) *Watcher {
	t.Helper()
	w, err := New(cfg, Deps{
		Store:      &store.Store{},
		NowPlaying: obs,
		Spotify:    api,
		Tokens:     tokens,
		Logger:     log,
		Now:        func() time.Time { return testNow },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return w
}

// lockedBuffer collects log output. The handler is written to from the check
// goroutines, and a bare bytes.Buffer is not safe for that.
type lockedBuffer struct {
	mu  stdsync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
