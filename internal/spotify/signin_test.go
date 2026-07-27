package spotify

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/RequiDev/encore/internal/config"
)

// The regression tests for the failure that locks people out of their own
// instance.
//
// A large import exhausts a development-mode application's daily Spotify quota.
// Spotify answers with a 429 carrying a Retry-After of most of a day, and the
// limiter honours it for the whole process — which was the right call for
// catalogue reads and quite wrong for the two calls a person is sitting in front
// of. Signing in would then block in Limiter.Wait until the browser or the
// reverse proxy gave up, so an import taking metadata offline for a day also
// took authentication offline for a day.
//
// Nothing about a background backoff should be able to lock a human out.

// signinStub answers the two calls the OAuth callback makes.
type signinStub struct {
	server *httptest.Server
	tokens int
	me     int
}

func newSigninStub(t *testing.T) *signinStub {
	t.Helper()
	s := &signinStub{}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/token", func(w http.ResponseWriter, r *http.Request) {
		s.tokens++
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "user-token", "refresh_token": "refresh",
			"token_type": "Bearer", "expires_in": 3600,
		})
	})
	mux.HandleFunc("/v1/me", func(w http.ResponseWriter, r *http.Request) {
		s.me++
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "listener", "display_name": "Listener"})
	})
	s.server = httptest.NewServer(mux)
	t.Cleanup(s.server.Close)
	return s
}

func signinClient(t *testing.T, s *signinStub) *Client {
	t.Helper()
	return NewClient(config.Spotify{
		ClientID: "id", ClientSecret: "secret",
		APIBaseURL: s.server.URL, AuthBaseURL: s.server.URL,
		RateLimit: 2, RateBurst: 4, Timeout: 5 * time.Second, MaxRetries: 2,
	}, discardLogger(), WithHTTPClient(s.server.Client()))
}

// TestSignInSurvivesACatalogueQuotaBan is the whole point.
//
// The limiter is paused for hours, exactly as an exhausted daily quota leaves
// it. Both calls the OAuth callback makes must still go through, promptly.
func TestSignInSurvivesACatalogueQuotaBan(t *testing.T) {
	stub := newSigninStub(t)
	client := signinClient(t, stub)

	// A catalogue read has just been told to come back in twenty hours.
	client.Limiter().Pause(time.Now().Add(20 * time.Hour))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		token, err := client.ExchangeCode(ctx, "auth-code", "verifier")
		if err != nil {
			done <- err
			return
		}
		_, err = client.CurrentUser(ctx, token.AccessToken)
		done <- err
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("signing in during a quota ban failed: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("signing in blocked behind the catalogue rate limit; a background " +
			"backoff must never be able to lock a person out of their own instance")
	}

	if stub.tokens != 1 || stub.me != 1 {
		t.Fatalf("token exchanges=%d profile reads=%d, want 1 each", stub.tokens, stub.me)
	}
}

// TestCatalogueReadsStillHonourThePause: the split must not weaken the thing the
// shared limiter was built for. Background work stays held back.
func TestCatalogueReadsStillHonourThePause(t *testing.T) {
	stub := newSigninStub(t)
	client := signinClient(t, stub)
	client.Limiter().Pause(time.Now().Add(20 * time.Hour))

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	// GetTracks needs an application token first, and even that must wait: the
	// whole point of the pause is to stop spending requests against a quota that
	// has not reset.
	if _, err := client.GetTracks(ctx, []string{"trackaaaaaaaaaaaaaaaaa"}); err == nil {
		t.Fatal("a catalogue read went through while the application was rate limited")
	}
}

// TestAnInteractive429DoesNotStopTheCatalogue and its converse keep the two
// budgets genuinely separate, so neither can be used to starve the other.
func TestAnInteractive429PausesOnlyTheInteractivePath(t *testing.T) {
	stub := newSigninStub(t)
	// The accounts service refuses, with a long Retry-After.
	stub.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "3600")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"status":429,"message":"slow down"}}`))
	})
	client := signinClient(t, stub)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := client.ExchangeCode(ctx, "code", "verifier"); err == nil {
		t.Fatal("a 429 on the token endpoint produced no error")
	}

	// The catalogue budget is untouched: a rejected sign-in says nothing about
	// whether enrichment may proceed.
	if until := client.Limiter().PausedUntil(); !until.IsZero() {
		t.Fatalf("a sign-in 429 paused the catalogue limiter until %s", until.UTC())
	}
}

// TestARefusedSignInFailsFastAndSaysWhy: when Spotify refuses the sign-in path
// itself, the answer must arrive while somebody is still looking at the screen.
//
// A browser spinner that resolves in twenty hours is indistinguishable from a
// broken instance, so the bounded wait matters as much as the separate budget:
// the first sign-in reports the 429, and the ones behind it fail immediately
// with the instant it lifts rather than queueing for it.
func TestARefusedSignInFailsFastAndSaysWhy(t *testing.T) {
	stub := newSigninStub(t)
	stub.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "3600")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"status":429}}`))
	})
	client := signinClient(t, stub)

	ctx := context.Background()
	if _, err := client.ExchangeCode(ctx, "code", "verifier"); err == nil {
		t.Fatal("a 429 produced no error")
	}

	// The sign-in budget is now paused for an hour. The next attempt must not
	// wait for it.
	start := time.Now()
	_, err := client.ExchangeCode(ctx, "code", "verifier")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("a sign-in during the pause reported success")
	}
	if elapsed > 30*time.Second {
		t.Fatalf("a refused sign-in took %s to say so", elapsed)
	}
	var paused *PausedError
	if !errors.As(err, &paused) {
		t.Fatalf("error is %v, want a PausedError the interface can explain", err)
	}
	if paused.RetryAfter() <= 0 {
		t.Fatalf("PausedError reports %s remaining", paused.RetryAfter())
	}
}
