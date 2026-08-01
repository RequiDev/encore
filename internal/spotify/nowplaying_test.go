package spotify

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestOnlyACatalogueRateLimitPausesTheInstance is the safety property of this
// whole phase, asserted across every request class at once.
//
// onPause writes app_settings.spotify_paused_until, which 409s "sync now" for
// every user on the instance and, at the worker's next construction, halts
// enrichment, the recently-played poller and all five library enumerations.
// Exactly one class of request is allowed to cause that.
//
// Fails when: instanceWide() is widened to any second class, narrowed to none,
// or the onPause guard in classify stops consulting it.
//
// Scope, deliberately stated: this pins the *recording* half of instanceWide
// only — the guard around onPause. classify consults the same predicate a second
// time at its tail, to decide whether the caller waits the pause out, and that
// site is invisible here: a class wrongly made to wait still reaches onPause
// exactly as often. TestNowPlayingRateLimitStopsTheNextRequestWithoutSendingIt
// pins that one, via the clock. Neither test covers both, which is why both
// exist.
func TestOnlyACatalogueRateLimitPausesTheInstance(t *testing.T) {
	for name, tc := range map[string]struct {
		class     requestClass
		wantPause int32
	}{
		"catalogue":   {classCatalogue, 1},
		"interactive": {classInteractive, 0},
		"now playing": {classNowPlaying, 0},
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Retry-After", "3600")
				w.WriteHeader(http.StatusTooManyRequests)
			}))
			defer srv.Close()

			var paused atomic.Int32
			c := newTestClient(t, srv, newFakeClock(),
				WithPauseObserver(func(time.Time) { paused.Add(1) }))
			// One attempt, so the count below is the number of classes that
			// record a pause rather than the number of retries the policy
			// happens to allow. A catalogue 429 is retryable and the server
			// answers 429 for ever, so the default four-attempt budget would
			// report four — a true number that says nothing about which class
			// is allowed to pause the instance, and one that would change
			// whenever ENCORE_SPOTIFY_MAX_RETRIES did.
			c.policy = c.policy.WithAttempts(1)

			err := c.do(context.Background(), request{
				method: http.MethodGet,
				url:    srv.URL + "/v1/probe",
				label:  "probe",
				bearer: "user-token",
				class:  tc.class,
			})
			if err == nil {
				t.Fatal("want an error on a 429")
			}
			if got := paused.Load(); got != tc.wantPause {
				t.Fatalf("the pause observer fired %d times, want %d", got, tc.wantPause)
			}
		})
	}
}

// TestAClassWithNoBudgetIsRefusedRatherThanGivenTheSharedOne pins that the
// fourth request class somebody adds cannot quietly spend everybody else's
// quota.
//
// budget() used to end in `default: return c.limiter, 0`, which read as a
// conservative fallback and was the opposite. An unnamed class got the *shared
// catalogue* limiter, so a 429 on it paused enrichment, the recently-played
// poller and all five library enumerations for the whole Retry-After — while
// instanceWide() returned false for it, so nothing was recorded, nothing was
// logged, and no test could see it. The same false also makes classify answer
// immediately rather than wait, yet the fallback handed out an unbounded wait:
// the class declined to queue behind its own pause and then queued behind it for
// an hour.
//
// A panic is affordable here in a way it would not be for a caller-supplied
// value: requestClass is unexported, so only this package can produce one.
//
// Fails when: the switch grows a default arm again, whatever it returns.
func TestAClassWithNoBudgetIsRefusedRatherThanGivenTheSharedOne(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := newTestClient(t, srv, newFakeClock())

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("budget() answered for a class it does not know; a class with " +
				"no case must not be handed the catalogue budget every other " +
				"background request depends on")
		}
		if msg, ok := r.(string); !ok || !strings.Contains(msg, "has no budget") {
			t.Fatalf("panicked with %v, want a message naming the class with no budget", r)
		}
	}()

	// 99 stands in for a class added to the const block and forgotten here. It
	// stays unhandled however many real classes are added later, so this test
	// does not decay into asserting something about classNowPlaying.
	c.budget(request{class: requestClass(99)})
}

// TestNowPlayingRateLimitTouchesNoOtherBudget pins which limiter a 429 on the
// poll actually pauses.
//
// The test above proves nothing is recorded. This one proves nothing else is
// held back either: the catalogue budget carries enrichment and the sync
// poller, the sign-in budget carries authentication, and neither belongs to the
// least important request in the system.
//
// Fails when: budget() returns c.limiter or c.signin for classNowPlaying — the
// corresponding PausedUntil then stops being zero.
func TestNowPlayingRateLimitTouchesNoOtherBudget(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "3600")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	var paused atomic.Int32
	c := newTestClient(t, srv, newFakeClock(),
		WithPauseObserver(func(time.Time) { paused.Add(1) }))

	if _, err := c.Player(context.Background(), "user-token"); err == nil {
		t.Fatal("Player: want an error on a 429")
	}

	if got := paused.Load(); got != 0 {
		t.Errorf("the pause observer fired %d times; a now-playing 429 must never "+
			"pause Spotify instance-wide", got)
	}
	if until := c.Limiter().PausedUntil(); !until.IsZero() {
		t.Errorf("the catalogue budget is paused until %v; enrichment and the "+
			"recently-played poller draw on it", until)
	}
	if until := c.signin.PausedUntil(); !until.IsZero() {
		t.Errorf("the sign-in budget is paused until %v; nothing a background "+
			"worker does may take authentication offline", until)
	}
	if c.nowPlaying.PausedUntil().IsZero() {
		t.Error("the now-playing budget is not paused; the 429 backed nothing off at all")
	}
}

// TestNowPlayingRefreshRateLimitTouchesNoOtherBudget is the test above applied
// to the other request the poller makes.
//
// Every check is two requests, not one: a token refresh against
// accounts.spotify.com whenever the stored token has aged out, and then the poll
// itself. Only the poll was ever routed onto the poller's own budget, so the
// refresh in front of it was instance-wide — and a 429 on it did precisely what
// this whole phase exists to prevent: wrote app_settings.spotify_paused_until,
// 409d "sync now" for every user, and stopped enrichment, sync and the library
// enumerations at the worker's next construction. The least important request
// Encore makes, taking the instance down.
//
// Fails when: RefreshNowPlaying stops mapping to classNowPlaying, or the poller
// is routed back onto RefreshShared — the pause observer then fires and the
// catalogue budget is the one holding the pause.
func TestNowPlayingRefreshRateLimitTouchesNoOtherBudget(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "3600")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	var paused atomic.Int32
	c := newTestClient(t, srv, newFakeClock(),
		WithPauseObserver(func(time.Time) { paused.Add(1) }))

	_, err := c.RefreshToken(context.Background(), "old-refresh", RefreshNowPlaying)
	var pausedErr *PausedError
	if !errors.As(err, &pausedErr) {
		t.Fatalf("RefreshToken returned %v, want a *PausedError", err)
	}

	if got := paused.Load(); got != 0 {
		t.Errorf("the pause observer fired %d times; a 429 on the poller's own "+
			"token refresh must never pause Spotify instance-wide", got)
	}
	if until := c.Limiter().PausedUntil(); !until.IsZero() {
		t.Errorf("the catalogue budget is paused until %v; enrichment, the "+
			"recently-played poller and the library enumerations draw on it", until)
	}
	if until := c.signin.PausedUntil(); !until.IsZero() {
		t.Errorf("the sign-in budget is paused until %v; nothing a background "+
			"worker does may take authentication offline", until)
	}
	if c.nowPlaying.PausedUntil().IsZero() {
		t.Error("the now-playing budget is not paused; the 429 backed nothing off at all")
	}
}

// TestASharedRefreshStillPausesTheInstance pins the other side of the same
// switch: giving the poller a budget of its own must not move anybody else.
//
// Every other caller of RefreshToken — the recently-played poller, the library
// worker, and the API behind a playlist write — has always drawn on the shared
// catalogue budget, and a 429 there is a fact about the whole instance that must
// go on being recorded. A change that made *every* refresh private would leave a
// quota ban forgotten across a restart, which is the bug WithPauseObserver was
// added to fix.
//
// Fails when: RefreshShared stops mapping to classCatalogue, or is quietly made
// the same value as RefreshNowPlaying.
func TestASharedRefreshStillPausesTheInstance(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "3600")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	var paused atomic.Int32
	c := newTestClient(t, srv, newFakeClock(),
		WithPauseObserver(func(time.Time) { paused.Add(1) }))
	// One attempt, for the reason TestOnlyACatalogueRateLimitPausesTheInstance
	// gives: the count below is meant to say whether this class records a pause,
	// not how many retries the policy happens to allow.
	c.policy = c.policy.WithAttempts(1)

	if _, err := c.RefreshToken(context.Background(), "old-refresh", RefreshShared); err == nil {
		t.Fatal("RefreshToken: want an error on a 429")
	}

	if got := paused.Load(); got != 1 {
		t.Errorf("the pause observer fired %d times, want 1: a refresh on the "+
			"shared budget is instance-wide and must survive a restart", got)
	}
	if c.Limiter().PausedUntil().IsZero() {
		t.Error("the catalogue budget is not paused; every other background caller draws on it")
	}
	if until := c.nowPlaying.PausedUntil(); !until.IsZero() {
		t.Errorf("the now-playing budget is paused until %v by somebody else's "+
			"refresh; the poller must not be stopped by work it did not do", until)
	}
}

// TestACataloguePauseDoesNotStallTheNowPlayingRefresh is the isolation read in
// the direction that used to fail silently.
//
// An enrichment 429 pauses the catalogue limiter for whatever Retry-After
// Spotify names, which for an exhausted daily quota is most of a day. Within the
// hour every stored access token expires, so every check needed a refresh — and
// on the shared budget that refresh waits *unboundedly*, so the checks did not
// fail, they blocked. Four goroutines, a WaitGroup that never returns, no tick
// after that one, and nothing recorded as failed: checked_at simply stops
// advancing while the card keeps rendering a present-tense "Playing" chip over
// an observation a day old.
//
// The two halves are asserted together because the property is a difference. The
// shared arm is what the fake clock measures the old behaviour as — a full hour
// slept inside one call — and it must stay that way, because a background caller
// with all day waiting out a pause is the design.
//
// Fails when: the poller's refresh is routed back onto the shared budget. The
// now-playing arm then sleeps the hour too, and its sleep log stops being empty.
func TestACataloguePauseDoesNotStallTheNowPlayingRefresh(t *testing.T) {
	for name, tc := range map[string]struct {
		budget    RefreshBudget
		wantSlept time.Duration
	}{
		// Unchanged, and deliberately asserted: this is the hour the reviewer
		// measured, and it is correct for a caller whose work the pause is about.
		"shared waits the pause out": {RefreshShared, time.Hour},
		// The poller's refresh draws on a limiter the pause never touched, so it
		// neither queues nor fails: it simply goes.
		"now playing is not held back at all": {RefreshNowPlaying, 0},
	} {
		t.Run(name, func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w,
					`{"access_token":"fresh-access","token_type":"Bearer","expires_in":3600}`)
			}))
			defer srv.Close()

			clock := newFakeClock()
			c := newTestClient(t, srv, clock)
			// Exactly what an enrichment 429 does, without the 429: the recorded
			// pause is restored into this limiter at startup by the same call.
			c.Limiter().Pause(clock.Now().Add(time.Hour))

			tok, err := c.RefreshToken(context.Background(), "old-refresh", tc.budget)
			if err != nil {
				t.Fatalf("RefreshToken: %v", err)
			}
			if tok.AccessToken != "fresh-access" {
				t.Fatalf("AccessToken = %q, want fresh-access", tok.AccessToken)
			}
			if got := calls.Load(); got != 1 {
				t.Fatalf("the token endpoint was called %d times, want 1", got)
			}

			var slept time.Duration
			for _, d := range clock.sleeps() {
				slept += d
			}
			if slept != tc.wantSlept {
				t.Fatalf("the refresh waited %v, want %v: the poller must not queue "+
					"behind a pause declared by work it has no part in", slept, tc.wantSlept)
			}
		})
	}
}

// TestNowPlayingRateLimitStopsTheNextRequestWithoutSendingIt is the "stopping is
// the property" half: a 429 must back the poller off, not merely be recorded.
//
// The server answers 429 once and 204 for ever after, so a second request that
// reached it would succeed. It must not reach it.
//
// The sleep assertion is what pins the *second* consequence of instanceWide, the
// one at classify's tail. Both consequences are meant to hang off that single
// predicate, and until this assertion existed only the onPause half was pinned:
// reverting the tail to `if r.class == classInteractive` — a literal undo of
// this task — left the whole suite green. Nothing could see it, because a
// classNowPlaying retry sleeps on the fake clock (which advances instantly, so
// no wall-clock deadline notices) and is then stopped by WaitMax before reaching
// the wire, returning a byte-identical *PausedError. In production that same
// regression costs a real thirty-second clock.Sleep inside the poller's own
// goroutine on every 429: across N accounts on a thirty-second tick it parks ~N
// goroutines in Sleep for the whole rate-limited window, each holding its poll
// slot into the next tick, with "last checked" permanently half a minute stale.
//
// Fails when: the tail of classify stops asking instanceWide and names a class
// instead — the retry loop then sleeps before answering and slept is non-empty;
// or classNowPlaying stops getting a bounded wait in budget(), in which case the
// second call reaches the server, succeeds, and returns nil instead of a
// *PausedError; or the pause lands on a limiter the second call does not
// consult, in which case the request count is 2.
func TestNowPlayingRateLimitStopsTheNextRequestWithoutSendingIt(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "3600")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	clock := newFakeClock()
	c := newTestClient(t, srv, clock)

	if _, err := c.Player(context.Background(), "user-token"); err == nil {
		t.Fatal("the first call: want an error on a 429")
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("the first call made %d requests, want 1", got)
	}
	if slept := clock.sleeps(); len(slept) != 0 {
		t.Fatalf("the first call slept %v before answering; a 429 on a class that "+
			"does not record an instance-wide pause must answer at once", slept)
	}

	done := make(chan error, 1)
	go func() {
		_, err := c.Player(context.Background(), "user-token")
		done <- err
	}()
	select {
	case err := <-done:
		var paused *PausedError
		if !errors.As(err, &paused) {
			t.Fatalf("the second call returned %v, want a *PausedError", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the second call blocked; a paused poller must answer at once " +
			"rather than hold a goroutine for the whole Retry-After")
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("%d requests reached Spotify, want 1: the poller did not back off", got)
	}
}

// TestPlayerReportsNoContentAsNothingPlaying pins the endpoint's commonest
// answer.
//
// 204 is not an error and is not "an advert with no item". It is the state the
// card renders as "Nothing is playing.", which has to stay distinct from both
// "Encore has not checked yet" and "something Encore cannot identify".
//
// Fails when: request.status is dropped. decode() returns early on a 204
// without touching r.out, so the zero-value Playback would come back non-nil
// and every idle listener would be reported as playing something
// unidentifiable.
func TestPlayerReportsNoContentAsNothingPlaying(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := newTestClient(t, srv, newFakeClock())
	got, err := c.Player(context.Background(), "user-token")
	if err != nil {
		t.Fatalf("Player: %v", err)
	}
	if got != nil {
		t.Fatalf("Player = %+v, want nil for a 204", got)
	}
}

// TestPlayerDecodesATrack pins the endpoint, the fields the card renders, the
// three facts the backfill needs, the listener's own token, and the query.
//
// The path is /v1/me/player rather than /v1/me/player/currently-playing, and
// that is the whole of Phase 3c's request budget: /me/player returns a strict
// superset of the narrower endpoint — the same item, progress and playing
// state, plus shuffle_state, repeat_state and a device that the narrower one is
// observed to omit — for the same single request. A second call would have
// doubled a loop that already makes fourteen thousand requests a day at five
// accounts and thirty seconds, without the operator changing a key.
//
// additional_types=episode is not decoration: without it Spotify answers a
// podcast with item: null and currently_playing_type "episode", so a named
// episode would render as "something Encore cannot identify".
//
// Fails when: the path reverts to /v1/me/player/currently-playing, which does
// not carry shuffle_state and leaves every backfilled listen NULL for ever;
// additional_types is dropped; or the application token is used instead of the
// listener's, which answers for nobody.
func TestPlayerDecodesATrack(t *testing.T) {
	var gotPath, gotQuery, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery, gotAuth = r.URL.Path, r.URL.RawQuery, r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
            "timestamp": 1785000000000,
            "progress_ms": 161000,
            "is_playing": true,
            "shuffle_state": true,
            "repeat_state": "off",
            "currently_playing_type": "track",
            "device": {"id":"d1","name":"Kitchen speaker","type":"Speaker","is_active":true},
            "item": {
                "id": "track-1", "name": "The Wheel", "type": "track",
                "uri": "spotify:track:track-1", "duration_ms": 255000, "is_local": false,
                "artists": [{"id":"artist-1","name":"SOHN"}]
            }
        }`)
	}))
	defer srv.Close()

	c := newTestClient(t, srv, newFakeClock())
	got, err := c.Player(context.Background(), "user-token")
	if err != nil {
		t.Fatalf("Player: %v", err)
	}
	if got == nil {
		t.Fatal("Player returned nil for a 200 carrying an item")
	}

	if gotPath != "/v1/me/player" {
		t.Errorf("path = %q, want /v1/me/player", gotPath)
	}
	if gotQuery != "additional_types=episode" {
		t.Errorf("query = %q, want additional_types=episode", gotQuery)
	}
	if gotAuth != "Bearer user-token" {
		t.Errorf("authorization = %q, want the listener's own token", gotAuth)
	}
	if !got.IsPlaying {
		t.Error("IsPlaying = false, want true")
	}
	if got.ShuffleState == nil || !*got.ShuffleState {
		t.Errorf("ShuffleState = %v, want a pointer to true", got.ShuffleState)
	}
	if got.RepeatState != "off" {
		t.Errorf("RepeatState = %q, want off", got.RepeatState)
	}
	if got.ProgressMs == nil || *got.ProgressMs != 161000 {
		t.Errorf("ProgressMs = %v, want 161000", got.ProgressMs)
	}
	if got.Device == nil || got.Device.Name != "Kitchen speaker" || got.Device.Type != "Speaker" {
		t.Errorf("Device = %+v, want the Kitchen speaker, type Speaker", got.Device)
	}
	if got.Item == nil || got.Item.Name != "The Wheel" || got.Item.DurationMs != 255000 {
		t.Errorf("Item = %+v, want The Wheel at 255000 ms", got.Item)
	}
	if got.Item == nil || len(got.Item.Artists) != 1 || got.Item.Artists[0].Name != "SOHN" {
		t.Errorf("Artists = %+v, want SOHN", got.Item)
	}
}

// TestPlayerReportsAnAbsentShuffleStateAsUnknown is the "unknown is not false"
// rule at the very first place a Spotify value enters the system.
//
// A bool field would decode a missing shuffle_state to false, and Encore would
// then write "this play was not shuffled" onto a listener's history about a
// fact it never received. Every guard further down the pipeline — the nullable
// column, the IS NOT NULL in the backfill's WHERE — is downstream of this one
// and cannot recover the distinction once it has been lost here.
//
// Fails when: ShuffleState goes back to being a bool. It then decodes to false
// and the pointer check below cannot be satisfied.
func TestPlayerReportsAnAbsentShuffleStateAsUnknown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// No shuffle_state and no device: everything Encore would like and
		// Spotify did not send.
		_, _ = io.WriteString(w, `{
            "is_playing": true,
            "currently_playing_type": "track",
            "item": {"id":"track-1","name":"The Wheel","type":"track","duration_ms":255000}
        }`)
	}))
	defer srv.Close()

	c := newTestClient(t, srv, newFakeClock())
	got, err := c.Player(context.Background(), "user-token")
	if err != nil {
		t.Fatalf("Player: %v", err)
	}
	if got == nil {
		t.Fatal("Player returned nil for a 200 carrying an item")
	}
	if got.ShuffleState != nil {
		t.Errorf("ShuffleState = %v, want nil: an absent shuffle_state is "+
			"\"not reported\", which is a different fact from \"not shuffled\"",
			*got.ShuffleState)
	}
	if got.Device != nil {
		t.Errorf("Device = %+v, want nil when Spotify reported none", got.Device)
	}
}

// TestPlayerMakesExactlyOneRequest pins this phase's whole cost story.
//
// Phase 3c reads shuffle_state and a device by moving to a wider endpoint, not
// by asking twice. The operator chose ENCORE_NOWPLAYING_INTERVAL knowing the
// quota table in docs/configuration.md, and a second call per tick would double
// every figure in it silently.
//
// Fails when: Player calls /me/player/currently-playing as well "for the item",
// or grows a device lookup through /me/player/devices — the counter below then
// reads 2 rather than 1.
func TestPlayerMakesExactlyOneRequest(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := newTestClient(t, srv, newFakeClock())
	if _, err := c.Player(context.Background(), "user-token"); err != nil {
		t.Fatalf("Player: %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("one poll made %d requests, want exactly 1: a second call "+
			"doubles what the operator agreed to spend", got)
	}
}

// TestPlayerForbiddenIsNotRetried pins that a grant without
// user-read-playback-state costs one request, not six.
//
// Fails when: classify stops wrapping non-429 4xx in retry.Stop, or Player
// grows a retry loop of its own.
func TestPlayerForbiddenIsNotRetried(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	c := newTestClient(t, srv, newFakeClock())
	_, err := c.Player(context.Background(), "user-token")
	apiErr, ok := AsAPIError(err)
	if !ok || !apiErr.IsForbidden() {
		t.Fatalf("error = %v, want a 403 APIError", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("attempts = %d, want 1: a scope failure spends quota to fail identically", got)
	}
}
