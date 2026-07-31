package spotify

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
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
// Fails when: instanceWide() is widened to any second class, or classify stops
// consulting it and goes back to testing a boolean.
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

	if _, err := c.CurrentlyPlaying(context.Background(), "user-token"); err == nil {
		t.Fatal("CurrentlyPlaying: want an error on a 429")
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

// TestNowPlayingRateLimitStopsTheNextRequestWithoutSendingIt is the "stopping is
// the property" half: a 429 must back the poller off, not merely be recorded.
//
// The server answers 429 once and 204 for ever after, so a second request that
// reached it would succeed. It must not reach it.
//
// Fails when: classNowPlaying stops getting a bounded wait — the second call
// then sleeps out the hour instead of answering and the deadline below fires;
// or the pause lands on a limiter the second call does not consult, in which
// case the request count is 2.
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

	c := newTestClient(t, srv, newFakeClock())

	if _, err := c.CurrentlyPlaying(context.Background(), "user-token"); err == nil {
		t.Fatal("the first call: want an error on a 429")
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("the first call made %d requests, want 1", got)
	}

	done := make(chan error, 1)
	go func() {
		_, err := c.CurrentlyPlaying(context.Background(), "user-token")
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

// TestCurrentlyPlayingReportsNoContentAsNothingPlaying pins the endpoint's
// commonest answer.
//
// 204 is not an error and is not "an advert with no item". It is the state the
// card renders as "Nothing is playing.", which has to stay distinct from both
// "Encore has not checked yet" and "something Encore cannot identify".
//
// Fails when: request.status is dropped. decode() returns early on a 204
// without touching r.out, so the zero-value Playback would come back non-nil
// and every idle listener would be reported as playing something
// unidentifiable.
func TestCurrentlyPlayingReportsNoContentAsNothingPlaying(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := newTestClient(t, srv, newFakeClock())
	got, err := c.CurrentlyPlaying(context.Background(), "user-token")
	if err != nil {
		t.Fatalf("CurrentlyPlaying: %v", err)
	}
	if got != nil {
		t.Fatalf("CurrentlyPlaying = %+v, want nil for a 204", got)
	}
}

// TestCurrentlyPlayingDecodesATrack pins the fields the card renders, the
// listener's own token, and the query.
//
// additional_types=episode is not decoration: without it Spotify answers a
// podcast with item: null and currently_playing_type "episode", so a named
// episode would render as "something Encore cannot identify".
//
// Fails when: additional_types is dropped; the application token is used
// instead of the listener's, which answers for nobody; or the path changes to
// /v1/me/player, which carries a payload this phase does not use.
func TestCurrentlyPlayingDecodesATrack(t *testing.T) {
	var gotPath, gotQuery, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery, gotAuth = r.URL.Path, r.URL.RawQuery, r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
            "timestamp": 1785000000000,
            "progress_ms": 161000,
            "is_playing": true,
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
	got, err := c.CurrentlyPlaying(context.Background(), "user-token")
	if err != nil {
		t.Fatalf("CurrentlyPlaying: %v", err)
	}
	if got == nil {
		t.Fatal("CurrentlyPlaying returned nil for a 200 carrying an item")
	}

	if gotPath != "/v1/me/player/currently-playing" {
		t.Errorf("path = %q, want /v1/me/player/currently-playing", gotPath)
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
	if got.ProgressMs == nil || *got.ProgressMs != 161000 {
		t.Errorf("ProgressMs = %v, want 161000", got.ProgressMs)
	}
	if got.Device == nil || got.Device.Name != "Kitchen speaker" {
		t.Errorf("Device = %+v, want the Kitchen speaker", got.Device)
	}
	if got.Item == nil || got.Item.Name != "The Wheel" || got.Item.DurationMs != 255000 {
		t.Errorf("Item = %+v, want The Wheel at 255000 ms", got.Item)
	}
	if got.Item == nil || len(got.Item.Artists) != 1 || got.Item.Artists[0].Name != "SOHN" {
		t.Errorf("Artists = %+v, want SOHN", got.Item)
	}
}

// TestCurrentlyPlayingForbiddenIsNotRetried pins that a grant without
// user-read-playback-state costs one request, not six.
//
// Fails when: classify stops wrapping non-429 4xx in retry.Stop, or
// CurrentlyPlaying grows a retry loop of its own.
func TestCurrentlyPlayingForbiddenIsNotRetried(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	c := newTestClient(t, srv, newFakeClock())
	_, err := c.CurrentlyPlaying(context.Background(), "user-token")
	apiErr, ok := AsAPIError(err)
	if !ok || !apiErr.IsForbidden() {
		t.Fatalf("error = %v, want a 403 APIError", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("attempts = %d, want 1: a scope failure spends quota to fail identically", got)
	}
}
