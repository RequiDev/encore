package spotify

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RequiDev/encore/internal/config"
)

// fakeStart anchors every fake clock so that expectations can be written as
// offsets from a fixed instant.
var fakeStart = time.Date(2026, time.July, 26, 12, 0, 0, 0, time.UTC)

// fakeClock advances only when something sleeps, which makes a test that
// exercises backoff finish in microseconds and record exactly what was waited.
type fakeClock struct {
	mu    sync.Mutex
	now   time.Time
	slept []time.Duration
}

func newFakeClock() *fakeClock { return &fakeClock{now: fakeStart} }

func (f *fakeClock) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

func (f *fakeClock) Sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if d <= 0 {
		return nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(d)
	f.slept = append(f.slept, d)
	return nil
}

func (f *fakeClock) sleeps() []time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]time.Duration(nil), f.slept...)
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// newTestClient points a client at an httptest server, with a rate limit high
// enough that the bucket never interferes with what a test is measuring.
func newTestClient(t *testing.T, srv *httptest.Server, clock Clock, opts ...Option) *Client {
	t.Helper()
	cfg := config.Spotify{
		ClientID:     "client-id",
		ClientSecret: "client-secret",
		RedirectURL:  "https://encore.example.com/api/auth/spotify/callback",
		Scopes:       config.DefaultScopes(),
		APIBaseURL:   srv.URL,
		AuthBaseURL:  srv.URL,
		TokenURL:     srv.URL + "/api/token",
		RateLimit:    1000,
		RateBurst:    100,
		Timeout:      5 * time.Second,
		MaxRetries:   3,
	}
	all := append([]Option{WithHTTPClient(srv.Client()), WithClock(clock)}, opts...)
	return NewClient(cfg, discardLogger(), all...)
}

// appTokenHandler serves the client-credentials grant the catalogue reads need.
func appTokenHandler(calls *atomic.Int32) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		if calls != nil {
			calls.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"access_token":"app-token","token_type":"Bearer","expires_in":3600}`)
	}
}

func TestRateLimitedRequestPausesWholeClient(t *testing.T) {
	tests := []struct {
		name      string
		header    func() string
		wantDelay time.Duration
	}{
		{
			name:      "delay in seconds",
			header:    func() string { return "2" },
			wantDelay: 2 * time.Second,
		},
		{
			name:      "http date",
			header:    func() string { return fakeStart.Add(30 * time.Second).Format(http.TimeFormat) },
			wantDelay: 30 * time.Second,
		},
		{
			name:      "header absent",
			header:    func() string { return "" },
			wantDelay: defaultRetryAfter,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if calls.Add(1) == 1 {
					if h := tc.header(); h != "" {
						w.Header().Set("Retry-After", h)
					}
					w.WriteHeader(http.StatusTooManyRequests)
					_, _ = io.WriteString(w, `{"error":{"status":429,"message":"API rate limit exceeded"}}`)
					return
				}
				_, _ = io.WriteString(w, `{"items":[]}`)
			}))
			defer srv.Close()

			clock := newFakeClock()
			c := newTestClient(t, srv, clock)

			// Driven through a background call on purpose. The sign-in path has its
			// own budget and refuses to queue, so it is the wrong instrument for
			// measuring how long the application is held back.
			if _, err := c.RecentlyPlayed(
				context.Background(), "user-token", time.Time{}, 50, 1); err != nil {
				t.Fatalf("RecentlyPlayed: %v", err)
			}
			if got := calls.Load(); got != 2 {
				t.Fatalf("server calls = %d, want 2", got)
			}

			wantPause := fakeStart.Add(tc.wantDelay)
			if got := c.Limiter().PausedUntil(); !got.Equal(wantPause) {
				t.Errorf("PausedUntil = %s, want %s", got, wantPause)
			}
			slept := clock.sleeps()
			if len(slept) != 1 || slept[0] != tc.wantDelay {
				t.Errorf("slept %v, want [%s]", slept, tc.wantDelay)
			}
		})
	}
}

func TestRateLimitPauseHeldForLongRetryAfter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "3600")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"status":429,"message":"API rate limit exceeded"}}`)
	}))
	defer srv.Close()

	clock := newFakeClock()
	c := newTestClient(t, srv, clock)
	c.policy = c.policy.WithAttempts(1)

	_, err := c.RecentlyPlayed(context.Background(), "user-token", time.Time{}, 50, 1)
	apiErr, ok := AsAPIError(err)
	if !ok {
		t.Fatalf("error %v is not an APIError", err)
	}
	if !apiErr.IsRateLimited() || apiErr.RetryAfter != time.Hour {
		t.Fatalf("apiErr = %+v, want 429 with a one hour retry-after", apiErr)
	}
	// The retry loop never sleeps for an hour, but the limiter holds the full
	// delay so the next request blocks until Spotify's window has passed.
	if got, want := c.Limiter().PausedUntil(), fakeStart.Add(time.Hour); !got.Equal(want) {
		t.Fatalf("PausedUntil = %s, want %s", got, want)
	}
}

func TestServerErrorsAreRetried(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) < 3 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		_, _ = io.WriteString(w, `{"id":"listener"}`)
	}))
	defer srv.Close()

	c := newTestClient(t, srv, newFakeClock())
	if _, err := c.CurrentUser(context.Background(), "user-token"); err != nil {
		t.Fatalf("CurrentUser: %v", err)
	}
	if got := calls.Load(); got != 3 {
		t.Fatalf("server calls = %d, want 3", got)
	}
}

func TestClientErrorsAreNotRetried(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":{"status":404,"message":"Non existing id"}}`)
	}))
	defer srv.Close()

	c := newTestClient(t, srv, newFakeClock())
	_, err := c.CurrentUser(context.Background(), "user-token")
	apiErr, ok := AsAPIError(err)
	if !ok {
		t.Fatalf("error %v is not an APIError", err)
	}
	if !apiErr.IsNotFound() {
		t.Errorf("IsNotFound = false for status %d", apiErr.StatusCode)
	}
	if apiErr.Message != "Non existing id" {
		t.Errorf("Message = %q, want %q", apiErr.Message, "Non existing id")
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("server calls = %d, want 1: a 4xx must not be retried", got)
	}
}

func TestErrorNeverCarriesCredentials(t *testing.T) {
	const secret = "BQD5s3cr3t-access-token"
	err := &APIError{
		StatusCode: http.StatusUnauthorized,
		Message:    "Invalid access token",
		Body:       `{"error":{"status":401,"message":"Invalid access token ` + secret + `"}}`,
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("Error() leaked the token: %s", err.Error())
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("Error() = %q, want it to name the status", err.Error())
	}
}

func TestCancelledContextIsNotRetried(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	c := newTestClient(t, srv, newFakeClock())
	_, err := c.CurrentUser(ctx, "user-token")
	if err == nil {
		t.Fatal("CurrentUser succeeded on a cancelled context")
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("server calls = %d, want 0", got)
	}
}

func TestParseRetryAfter(t *testing.T) {
	now := fakeStart
	tests := []struct {
		name   string
		header string
		want   time.Duration
		wantOK bool
	}{
		{"absent", "", 0, false},
		{"seconds", "17", 17 * time.Second, true},
		{"zero", "0", 0, true},
		{"negative", "-5", 0, true},
		{"http date", now.Add(90 * time.Second).Format(http.TimeFormat), 90 * time.Second, true},
		{"past http date", now.Add(-90 * time.Second).Format(http.TimeFormat), 0, true},
		{"nonsense", "soon", 0, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseRetryAfter(tc.header, now)
			if got != tc.want || ok != tc.wantOK {
				t.Fatalf("parseRetryAfter(%q) = (%s, %t), want (%s, %t)", tc.header, got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

func TestErrorMessage(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{"web api shape", `{"error":{"status":404,"message":"Non existing id"}}`, "Non existing id"},
		{"web api with reason", `{"error":{"status":403,"message":"Player error","reason":"PREMIUM_REQUIRED"}}`, "Player error (PREMIUM_REQUIRED)"},
		{"oauth shape", `{"error":"invalid_grant","error_description":"Refresh token revoked"}`, "invalid_grant: Refresh token revoked"},
		{"oauth without description", `{"error":"invalid_client"}`, "invalid_client"},
		{"not json", `<html>502 Bad Gateway</html>`, ""},
		{"empty", ``, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := errorMessage([]byte(tc.body)); got != tc.want {
				t.Fatalf("errorMessage = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestRequestRefusesBothRawAndJSON pins that a request carrying both body
// shapes is a hard, immediate failure rather than a silently resolved
// precedence.
//
// This is a state no current caller reaches (UpdatePlaylistDetails sets only
// json, SetPlaylistCover sets only raw), but one the struct no longer
// prevents now that it carries three body shapes instead of two. Picking one
// silently — raw winning over json, say — is exactly the shape of this
// project's three prior data-loss incidents: a wrong body reaches the wire
// with no diagnostic signal at all. attempt() refuses to send anything in
// that state instead, and the error names the mistake and the endpoint that
// made it, so whoever copy-pastes their way into this learns immediately
// rather than at 3am from a support ticket about a clobbered field.
//
// Fails when: the guard at the top of attempt() is removed — the request
// then falls through to the body switch, the raw body is sent silently, and
// this test's error check finds nothing.
func TestRequestRefusesBothRawAndJSON(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := newTestClient(t, srv, newFakeClock())
	err := c.do(context.Background(), request{
		method:      http.MethodPut,
		url:         srv.URL + "/v1/example",
		label:       "test request with both raw and json set",
		bearer:      "user-token",
		raw:         []byte("raw-body"),
		contentType: "image/jpeg",
		json:        map[string]any{"this": "must not be sent"},
	})
	if err == nil {
		t.Fatal("do: want an error when both raw and json are set, got nil")
	}
	if !strings.Contains(err.Error(), "both a raw and a json body") {
		t.Errorf("error = %q, want it to name the double-body mistake", err.Error())
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("server calls = %d, want 0: a malformed request must never reach the wire", got)
	}
}
