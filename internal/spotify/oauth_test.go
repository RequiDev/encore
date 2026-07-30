package spotify

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestAuthorizeURL(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()

	c := newTestClient(t, srv, newFakeClock())
	raw := c.AuthorizeURL("state-value", "challenge-value")

	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse authorize url: %v", err)
	}
	if u.Path != "/authorize" {
		t.Errorf("path = %q, want /authorize", u.Path)
	}
	q := u.Query()
	want := map[string]string{
		"client_id":             "client-id",
		"response_type":         "code",
		"redirect_uri":          "https://encore.example.com/api/auth/spotify/callback",
		"state":                 "state-value",
		"code_challenge_method": "S256",
		"code_challenge":        "challenge-value",
		"scope":                 "user-read-recently-played user-read-private user-read-email user-top-read user-library-read user-follow-read playlist-read-private user-read-playback-state",
	}
	for k, v := range want {
		if got := q.Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	if q.Has("client_secret") {
		t.Error("authorize url carries the client secret")
	}
}

func TestExchangeCode(t *testing.T) {
	var got url.Values
	var user, pass string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse form: %v", err)
		}
		got = r.PostForm
		user, pass, _ = r.BasicAuth()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"access_token":"access-1","refresh_token":"refresh-1","token_type":"Bearer","scope":"user-read-recently-played user-read-email","expires_in":3600}`)
	}))
	defer srv.Close()

	clock := newFakeClock()
	c := newTestClient(t, srv, clock)

	tok, err := c.ExchangeCode(context.Background(), "auth-code", "verifier-value")
	if err != nil {
		t.Fatalf("ExchangeCode: %v", err)
	}
	if tok.AccessToken != "access-1" || tok.RefreshToken != "refresh-1" || tok.TokenType != "Bearer" {
		t.Fatalf("token = %+v", tok)
	}
	if want := fakeStart.Add(time.Hour); !tok.ExpiresAt.Equal(want) {
		t.Errorf("ExpiresAt = %s, want %s", tok.ExpiresAt, want)
	}
	if scopes := tok.Scopes(); len(scopes) != 2 || scopes[0] != "user-read-recently-played" {
		t.Errorf("Scopes = %v", scopes)
	}
	if user != "client-id" || pass != "client-secret" {
		t.Errorf("basic auth = %q/%q, want the configured client credentials", user, pass)
	}
	if got.Get("grant_type") != "authorization_code" {
		t.Errorf("grant_type = %q", got.Get("grant_type"))
	}
	if got.Get("code") != "auth-code" || got.Get("code_verifier") != "verifier-value" {
		t.Errorf("form = %v", got)
	}
	if got.Has("client_secret") {
		t.Error("form body carries the client secret; it belongs in the Authorization header")
	}
}

func TestRefreshToken(t *testing.T) {
	tests := []struct {
		name             string
		status           int
		body             string
		calls            int32
		wantErrIs        error
		wantAccessToken  string
		wantRefreshToken string
	}{
		{
			name:             "rotated refresh token is returned",
			status:           http.StatusOK,
			body:             `{"access_token":"access-2","refresh_token":"refresh-2","token_type":"Bearer","expires_in":3600}`,
			calls:            1,
			wantAccessToken:  "access-2",
			wantRefreshToken: "refresh-2",
		},
		{
			// Spotify usually omits refresh_token; the caller keeps the one it has,
			// so an empty value here must not be mistaken for a rotation.
			name:            "omitted refresh token stays empty",
			status:          http.StatusOK,
			body:            `{"access_token":"access-3","token_type":"Bearer","expires_in":3600}`,
			calls:           1,
			wantAccessToken: "access-3",
		},
		{
			name:      "revoked grant is permanent",
			status:    http.StatusBadRequest,
			body:      `{"error":"invalid_grant","error_description":"Refresh token revoked"}`,
			calls:     1,
			wantErrIs: ErrInvalidGrant,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer srv.Close()

			c := newTestClient(t, srv, newFakeClock())
			tok, err := c.RefreshToken(context.Background(), "old-refresh")

			if tc.wantErrIs != nil {
				if !errors.Is(err, tc.wantErrIs) {
					t.Fatalf("error = %v, want it to wrap %v", err, tc.wantErrIs)
				}
				apiErr, ok := AsAPIError(err)
				if !ok || apiErr.StatusCode != http.StatusBadRequest {
					t.Fatalf("error = %v, want an APIError carrying the 400", err)
				}
			} else {
				if err != nil {
					t.Fatalf("RefreshToken: %v", err)
				}
				if tok.AccessToken != tc.wantAccessToken {
					t.Errorf("AccessToken = %q, want %q", tok.AccessToken, tc.wantAccessToken)
				}
				if tok.RefreshToken != tc.wantRefreshToken {
					t.Errorf("RefreshToken = %q, want %q", tok.RefreshToken, tc.wantRefreshToken)
				}
			}
			if got := calls.Load(); got != tc.calls {
				t.Fatalf("token endpoint calls = %d, want %d", got, tc.calls)
			}
		})
	}
}

func TestRefreshTokenRequiresAToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Error("token endpoint called with an empty refresh token")
	}))
	defer srv.Close()

	c := newTestClient(t, srv, newFakeClock())
	if _, err := c.RefreshToken(context.Background(), "  "); err == nil {
		t.Fatal("RefreshToken accepted an empty refresh token")
	}
}

func TestAppTokenIsCachedAndSingleFlight(t *testing.T) {
	var calls atomic.Int32
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		<-release
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"access_token":"app-token","token_type":"Bearer","expires_in":3600}`)
	}))
	defer srv.Close()

	c := newTestClient(t, srv, newFakeClock())

	const callers = 8
	var ready, done sync.WaitGroup
	start := make(chan struct{})
	tokens := make([]string, callers)
	errs := make([]error, callers)
	ready.Add(callers)
	done.Add(callers)
	for i := range callers {
		go func() {
			defer done.Done()
			ready.Done()
			<-start
			tokens[i], errs[i] = c.AppToken(context.Background())
		}()
	}
	ready.Wait()
	close(start)
	// Give every caller time to reach AppToken before the one in flight returns,
	// so the test really exercises the shared wait rather than a warm cache.
	time.Sleep(50 * time.Millisecond)
	close(release)
	done.Wait()

	for i := range callers {
		if errs[i] != nil {
			t.Fatalf("caller %d: %v", i, errs[i])
		}
		if tokens[i] != "app-token" {
			t.Fatalf("caller %d token = %q", i, tokens[i])
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("token endpoint calls = %d, want 1: refreshes must be single-flight", got)
	}

	// A cached token is reused until it nears expiry.
	if _, err := c.AppToken(context.Background()); err != nil {
		t.Fatalf("AppToken: %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("token endpoint calls = %d, want the cached token to be reused", got)
	}

	c.InvalidateAppToken()
	if _, err := c.AppToken(context.Background()); err != nil {
		t.Fatalf("AppToken after invalidation: %v", err)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("token endpoint calls = %d, want a fresh fetch after invalidation", got)
	}
}

func TestTokenExpiry(t *testing.T) {
	now := fakeStart
	tests := []struct {
		name      string
		token     Token
		wantValid bool
	}{
		{"fresh", Token{AccessToken: "a", ExpiresAt: now.Add(time.Hour)}, true},
		{"inside the safety margin", Token{AccessToken: "a", ExpiresAt: now.Add(30 * time.Second)}, false},
		{"expired", Token{AccessToken: "a", ExpiresAt: now.Add(-time.Second)}, false},
		{"no expiry recorded", Token{AccessToken: "a"}, false},
		{"no token", Token{ExpiresAt: now.Add(time.Hour)}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.token.Valid(now); got != tc.wantValid {
				t.Fatalf("Valid = %t, want %t", got, tc.wantValid)
			}
		})
	}
}
