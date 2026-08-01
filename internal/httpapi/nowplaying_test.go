package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/RequiDev/encore/internal/domain"
)

// TestNowPlayingRequiresASession pins that presence is never public.
//
// Fails when: the route is moved outside the /api subtree, or the handler stops
// calling requireUser.
func TestNowPlayingRequiresASession(t *testing.T) {
	srv := newTestServer(t, testDeps{})
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/nowplaying", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

// TestNowPlayingReportsAnInstanceWithThePollerOff pins the answer a client uses
// to decide the card should not exist at all.
//
// Fails when: enabled is computed from anything but cfg.NowPlaying.Enabled(), or
// intervalSeconds reports a default the poller does not run on — the client
// would then poll an endpoint whose answer can never change.
func TestNowPlayingReportsAnInstanceWithThePollerOff(t *testing.T) {
	got := getNowPlaying(t, testDeps{}) // no interval configured
	if got.Enabled {
		t.Error("enabled = true with no interval configured")
	}
	if got.IntervalSeconds != 0 {
		t.Errorf("intervalSeconds = %d, want 0", got.IntervalSeconds)
	}
	if got.Observation != nil {
		t.Errorf("observation = %+v, want null", got.Observation)
	}
}

// TestNowPlayingReportsAMissingScope pins the per-account gate, computed on the
// server for the reason /api/me computes missingScopes there: two copies of the
// required scope would drift, and the TypeScript one would drift silently.
//
// Fails when: scopeGranted is hard-coded true, or is computed from the presence
// of a row — an account that has simply never been polled would then be told to
// reconnect Spotify for no reason.
func TestNowPlayingReportsAMissingScope(t *testing.T) {
	got := getNowPlaying(t, testDeps{
		interval: 30 * time.Second,
		scopes:   []string{"user-read-recently-played"},
	})
	if !got.Enabled {
		t.Fatal("enabled = false with an interval configured")
	}
	if got.ScopeGranted {
		t.Error("scopeGranted = true for a grant without user-read-playback-state")
	}
}

// TestNowPlayingReportsScopeGrantedForACurrentGrant is the positive side of
// TestNowPlayingReportsAMissingScope, which only ever exercises an account
// missing the scope. Both a fresh connection the poller has not reached yet and
// an account it has already checked must read scopeGranted true, because the
// two rows carry the same grant and differ only in whether a check has landed.
//
// Fails when: scopeGranted is computed from the presence of a now-playing row
// instead of the stored grant. The no-row case below is what kills that
// mutation — it would read false for an account that granted the scope but
// simply has not been polled yet, telling somebody to reconnect Spotify for
// nothing. Also fails when scopeGranted is hard-coded false.
func TestNowPlayingReportsScopeGrantedForACurrentGrant(t *testing.T) {
	fresh := getNowPlaying(t, testDeps{
		interval: 30 * time.Second,
		scopes:   []string{"user-read-playback-state"},
	})
	if !fresh.ScopeGranted {
		t.Error("scopeGranted = false for a granted scope with no row yet; an account " +
			"connected but not yet polled must not be told to reconnect")
	}

	at := time.Date(2026, time.July, 31, 9, 30, 0, 0, time.UTC)
	withRow := getNowPlaying(t, testDeps{
		interval: 30 * time.Second,
		scopes:   []string{"user-read-playback-state"},
		row: domain.NowPlaying{
			ObservedAt: at, State: domain.PlaybackIdle, Kind: domain.PlaybackItemNone,
			CheckedAt: at,
		},
	})
	if !withRow.ScopeGranted {
		t.Error("scopeGranted = false for a granted scope with a row present")
	}
}

// TestNowPlayingSeparatesNeverCheckedFromNothingPlaying is the distinction the
// whole feature turns on, asserted at the boundary the client reads.
//
// Fails when: the handler maps a never-observed row to an observation with state
// "idle" — the two payloads below then become identical and the card cannot
// tell "we have not looked" from "your player is silent".
func TestNowPlayingSeparatesNeverCheckedFromNothingPlaying(t *testing.T) {
	at := time.Date(2026, time.July, 31, 9, 30, 0, 0, time.UTC)

	never := getNowPlaying(t, testDeps{
		interval: 30 * time.Second,
		scopes:   []string{"user-read-playback-state"},
		row: domain.NowPlaying{
			State: domain.PlaybackUnknown, Kind: domain.PlaybackItemNone,
			CheckedAt: at, Failed: true,
		},
	})
	if never.Observation != nil {
		t.Fatalf("observation = %+v for an account never successfully checked, want null",
			never.Observation)
	}
	if never.CheckedAt == nil || !never.CheckedAt.Equal(at) {
		t.Errorf("checkedAt = %v, want %v", never.CheckedAt, at)
	}
	if !never.Failed {
		t.Error("failed = false after a failed check")
	}

	idle := getNowPlaying(t, testDeps{
		interval: 30 * time.Second,
		scopes:   []string{"user-read-playback-state"},
		row: domain.NowPlaying{
			ObservedAt: at, State: domain.PlaybackIdle, Kind: domain.PlaybackItemNone,
			CheckedAt: at,
		},
	})
	if idle.Observation == nil {
		t.Fatal("observation = null for an account whose player was seen idle")
	}
	if idle.Observation.State != string(domain.PlaybackIdle) {
		t.Errorf("state = %q, want idle", idle.Observation.State)
	}
	if idle.Failed {
		t.Error("failed = true after a successful check")
	}
}

// TestNowPlayingOnlyLinksATrackTheCatalogueHolds pins that trackId is a promise
// the client can act on.
//
// Fails when: the handler copies TrackID regardless of TrackKnown — the card
// then renders a link to /tracks/{id} for a track no page exists for.
func TestNowPlayingOnlyLinksATrackTheCatalogueHolds(t *testing.T) {
	at := time.Date(2026, time.July, 31, 9, 30, 0, 0, time.UTC)
	row := domain.NowPlaying{
		ObservedAt: at, State: domain.PlaybackPlaying, Kind: domain.PlaybackItemTrack,
		TrackID: "track-1", Title: "The Wheel", Artist: "SOHN",
		CheckedAt: at, TrackKnown: false,
	}
	got := getNowPlaying(t, testDeps{
		interval: 30 * time.Second,
		scopes:   []string{"user-read-playback-state"},
		row:      row,
	})
	if got.Observation == nil {
		t.Fatal("observation = null")
	}
	if got.Observation.TrackID != "" {
		t.Errorf("trackId = %q for a track the catalogue has never seen; a link "+
			"to a page that does not exist is worse than no link", got.Observation.TrackID)
	}
	if got.Observation.Title != "The Wheel" {
		t.Errorf("title = %q; the name is still shown, only the link is withheld",
			got.Observation.Title)
	}

	row.TrackKnown = true
	known := getNowPlaying(t, testDeps{
		interval: 30 * time.Second,
		scopes:   []string{"user-read-playback-state"},
		row:      row,
	})
	if known.Observation == nil || known.Observation.TrackID != "track-1" {
		t.Errorf("trackId = %+v, want track-1 once the catalogue holds it", known.Observation)
	}
}

// TestNowPlayingShowsAStaleObservationAfterAFailedPoll pins the fourth
// situation the brief names: the last poll failed, but a previous observation
// survives it and is still worth showing as stale. This is the combination
// Task 3's column-preserving RecordFailure exists to support, and the one
// Task 6's card renders as "this is what was playing, N minutes ago".
//
// Fails when: Failed and Observation are treated as mutually exclusive in the
// mapping — a failed check would then discard a true, still-relevant thing, or
// the card would have no observedAt to compute "N minutes ago" from.
func TestNowPlayingShowsAStaleObservationAfterAFailedPoll(t *testing.T) {
	observed := time.Date(2026, time.July, 31, 9, 30, 0, 0, time.UTC)
	failedAt := observed.Add(4 * time.Minute)

	got := getNowPlaying(t, testDeps{
		interval: 30 * time.Second,
		scopes:   []string{"user-read-playback-state"},
		row: domain.NowPlaying{
			ObservedAt: observed, State: domain.PlaybackPlaying, Kind: domain.PlaybackItemTrack,
			TrackID: "track-1", Title: "The Wheel", Artist: "SOHN",
			CheckedAt: failedAt, Failed: true, TrackKnown: true,
		},
	})
	if !got.Failed {
		t.Error("failed = false after a check that failed")
	}
	if got.CheckedAt == nil || !got.CheckedAt.Equal(failedAt) {
		t.Errorf("checkedAt = %v, want the failed check's own instant %v", got.CheckedAt, failedAt)
	}
	if got.Observation == nil {
		t.Fatal("observation = null; a failed poll must not discard the last real one")
	}
	if got.Observation.Title != "The Wheel" || got.Observation.TrackID != "track-1" {
		t.Errorf("observation = %+v, want the prior observation intact", got.Observation)
	}
	if !got.Observation.ObservedAt.Equal(observed) {
		t.Errorf("observedAt = %v, want the earlier %v: a failed check must not move it",
			got.Observation.ObservedAt, observed)
	}
}

// TestNowPlayingMakesNoSpotifyRequest pins that a browser cannot make Encore
// call Spotify.
//
// The card polls this endpoint on the instance's own interval, from every open
// tab. If the handler ever fetched, a dashboard left open in three tabs would
// triple the feature's cost and put that traffic on whichever budget the handler
// happened to use.
//
// Fails when: the handler grows a refresh-on-read, which is the natural-looking
// way to make the card feel fresher.
func TestNowPlayingMakesNoSpotifyRequest(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomicAdd(&calls, 1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	_ = getNowPlaying(t, testDeps{
		interval:    30 * time.Second,
		scopes:      []string{"user-read-playback-state"},
		spotifyBase: srv.URL,
	})
	if calls != 0 {
		t.Fatalf("%d Spotify requests were made serving GET /api/nowplaying, want 0", calls)
	}
}
