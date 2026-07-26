// Package spotify is Encore's client for the Spotify Web API: the OAuth flows,
// the shared rate limiter, and the handful of endpoints Encore reads.
//
// The package deliberately depends on nothing but the standard library,
// internal/retry, internal/config and internal/domain. It performs no database
// work and holds no user state beyond one cached application token, so an
// outage here can slow enrichment down but can never touch ingestion.
//
// Three properties matter more than the endpoint coverage:
//
//   - every request passes through one process-wide token bucket, and a 429
//     pauses that bucket for everyone rather than each goroutine backing off
//     alone;
//   - retries are bounded, jittered and driven by internal/retry, so the policy
//     is the same one the rest of Encore uses;
//   - no credential ever reaches an error string or a log record.
package spotify

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/requi/encore/internal/config"
	"github.com/requi/encore/internal/retry"
)

const (
	// maxResponseBytes caps a decoded body. A 50-track batch is a few hundred
	// kilobytes; the cap exists so a broken or hostile response cannot exhaust
	// memory on a worker that is already under load.
	maxResponseBytes = 8 << 20
	// defaultRetryAfter applies to a 429 that arrives without the header. Spotify
	// always sends one, but a proxy in between might not.
	defaultRetryAfter = 5 * time.Second
	// maxRetryDelay caps how long the retry loop itself sleeps. A longer
	// Retry-After is still honoured in full, because the limiter stays paused and
	// the next attempt blocks in Wait until it clears.
	maxRetryDelay      = 30 * time.Second
	defaultTimeout     = 20 * time.Second
	defaultAPIBaseURL  = "https://api.spotify.com"
	defaultAuthBaseURL = "https://accounts.spotify.com"
)

// Client talks to the Spotify Web API. It is safe for concurrent use and is
// meant to be shared: one client per process keeps one rate limit budget.
type Client struct {
	cfg     config.Spotify
	lg      *slog.Logger
	http    *http.Client
	limiter *Limiter
	clock   Clock
	baseURL string
	policy  retry.Policy

	// app holds the cached client-credentials token and the in-flight refresh.
	app appTokenCache
}

// Option customises a Client at construction.
type Option func(*Client)

// WithHTTPClient replaces the HTTP client, which is how tests point the client
// at an httptest server with its own transport.
func WithHTTPClient(h *http.Client) Option {
	return func(c *Client) {
		if h != nil {
			c.http = h
		}
	}
}

// WithLimiter shares an existing limiter, so several clients (the sync poller
// and the enrichment workers, say) draw on one rate budget.
func WithLimiter(l *Limiter) Option {
	return func(c *Client) {
		if l != nil {
			c.limiter = l
		}
	}
}

// WithClock replaces the clock used for backoff, pauses and token expiry.
func WithClock(cl Clock) Option {
	return func(c *Client) {
		if cl != nil {
			c.clock = cl
		}
	}
}

// WithBaseURL overrides the API base URL, without a trailing slash.
func WithBaseURL(u string) Option {
	return func(c *Client) {
		if u = strings.TrimRight(strings.TrimSpace(u), "/"); u != "" {
			c.baseURL = u
		}
	}
}

// NewClient builds a client from configuration. lg may be nil.
func NewClient(cfg config.Spotify, lg *slog.Logger, opts ...Option) *Client {
	if lg == nil {
		lg = slog.Default()
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	baseURL := strings.TrimRight(strings.TrimSpace(cfg.APIBaseURL), "/")
	if baseURL == "" {
		baseURL = defaultAPIBaseURL
	}

	c := &Client{
		cfg:     cfg,
		lg:      lg.With("component", "spotify"),
		http:    &http.Client{Timeout: timeout},
		clock:   SystemClock{},
		baseURL: baseURL,
		// MaxRetries counts retries, so the attempt budget is one more than that.
		policy: retry.API().WithAttempts(cfg.MaxRetries + 1),
	}
	for _, o := range opts {
		if o != nil {
			o(c)
		}
	}
	if c.limiter == nil {
		// Built after the options so it shares whichever clock the client ended up
		// with, which is what lets a test drive both without real waiting.
		c.limiter = NewLimiterWithClock(cfg.RateLimit, cfg.RateBurst, c.clock)
	}
	return c
}

// Limiter exposes the rate limiter, for sharing with another client and for
// reporting how long a 429 has paused the process.
func (c *Client) Limiter() *Limiter { return c.limiter }

// Clock exposes the clock the client runs on.
func (c *Client) Clock() Clock { return c.clock }

// authBaseURL is the accounts service, which is a different host from the API.
func (c *Client) authBaseURL() string {
	if b := strings.TrimRight(strings.TrimSpace(c.cfg.AuthBaseURL), "/"); b != "" {
		return b
	}
	return defaultAuthBaseURL
}

// tokenURL is where every grant type is redeemed.
func (c *Client) tokenURL() string {
	if u := strings.TrimSpace(c.cfg.TokenURL); u != "" {
		return u
	}
	return c.authBaseURL() + "/api/token"
}

// endpoint builds an absolute API URL.
func (c *Client) endpoint(path string, query url.Values) string {
	u := c.baseURL + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	return u
}

// request is one HTTP exchange with Spotify.
type request struct {
	method string
	url    string
	// label names the endpoint in logs and error messages. It never contains
	// query parameters, which can carry a listener's search terms.
	label  string
	bearer string
	basic  bool
	form   url.Values
	out    any
}

// get issues an authenticated GET against the Web API.
func (c *Client) get(ctx context.Context, path, label string, query url.Values, accessToken string, out any) error {
	if accessToken == "" {
		return fmt.Errorf("%s: no access token", label)
	}
	return c.do(ctx, request{
		method: http.MethodGet,
		url:    c.endpoint(path, query),
		label:  label,
		bearer: accessToken,
		out:    out,
	})
}

// do runs a request under the retry policy.
//
// Every classification decision is made in attempt: this function only supplies
// the schedule, the jitter and the context-aware sleep.
func (c *Client) do(ctx context.Context, r request) error {
	hooks := retry.Hooks{
		Sleep: c.clock.Sleep,
		OnRetry: func(attempt int, delay time.Duration, err error) {
			c.lg.Debug("retrying spotify request",
				"endpoint", r.label,
				"attempt", attempt,
				"delay", delay,
				"error", err.Error())
		},
	}
	return retry.Do(ctx, c.policy, hooks, func(ctx context.Context, _ int) error {
		return c.attempt(ctx, r)
	})
}

// attempt performs exactly one HTTP exchange and classifies the outcome for the
// retry loop: a permanent failure is wrapped in retry.Stop, a rate limit in
// retry.After, and anything else is returned bare so the policy's backoff runs.
func (c *Client) attempt(ctx context.Context, r request) error {
	if err := c.limiter.Wait(ctx); err != nil {
		// A finished context is the caller's decision, not a failure to retry.
		return retry.Stop(err)
	}

	var body io.Reader
	if r.form != nil {
		body = strings.NewReader(r.form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, r.method, r.url, body)
	if err != nil {
		return retry.Stop(fmt.Errorf("%s: build request: %w", r.label, err))
	}
	req.Header.Set("Accept", "application/json")
	if r.form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	switch {
	case r.bearer != "":
		req.Header.Set("Authorization", "Bearer "+r.bearer)
	case r.basic:
		// The client secret travels in the Authorization header rather than in the
		// form body, so it is never part of anything that could be echoed back.
		req.SetBasicAuth(c.cfg.ClientID, c.cfg.ClientSecret)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return retry.Stop(ctxErr)
		}
		// A transport failure is worth another attempt: dropped connections and DNS
		// blips are routine over the lifetime of a worker process.
		return fmt.Errorf("%s: %w", r.label, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return c.decode(resp, r)
	}
	return c.classify(resp, r)
}

// decode reads a successful response into r.out through a LimitReader.
func (c *Client) decode(resp *http.Response, r request) error {
	if r.out == nil || resp.StatusCode == http.StatusNoContent {
		drain(resp.Body)
		return nil
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(r.out); err != nil {
		// A body Spotify cannot have produced usually means an intercepting proxy
		// or a connection cut mid-response, both of which another attempt may miss.
		return fmt.Errorf("%s: decode response: %w", r.label, err)
	}
	drain(resp.Body)
	return nil
}

// classify turns a non-2xx response into the error the retry loop expects.
func (c *Client) classify(resp *http.Response, r request) error {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
	apiErr := &APIError{
		StatusCode: resp.StatusCode,
		Message:    errorMessage(raw),
		Body:       string(raw),
	}

	now := c.clock.Now()
	retryAfter, hasRetryAfter := parseRetryAfter(resp.Header.Get("Retry-After"), now)

	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		if !hasRetryAfter || retryAfter <= 0 {
			retryAfter = defaultRetryAfter
		}
		apiErr.RetryAfter = retryAfter
		// One 429 stops the whole client. The quota belongs to the application, so
		// a goroutine that backed off privately would only be queueing up the next
		// round of rejections on behalf of its neighbours.
		c.limiter.Pause(now.Add(retryAfter))
		c.lg.Warn("spotify rate limited", "endpoint", r.label, "retry_after", retryAfter)
		// The limiter now holds the real delay, so the loop needs only a bounded
		// nudge: the next attempt blocks in Wait until the pause has elapsed.
		return retry.After(min(retryAfter, maxRetryDelay), apiErr)

	case resp.StatusCode >= 500:
		if hasRetryAfter && retryAfter > 0 {
			apiErr.RetryAfter = retryAfter
			return retry.After(min(retryAfter, maxRetryDelay), apiErr)
		}
		return apiErr

	default:
		// Every other 4xx is the caller's problem: a bad id, a missing scope, an
		// expired token. Retrying it would spend quota to be told the same thing.
		return retry.Stop(apiErr)
	}
}

// drain reads and discards what is left of a body so the connection can be
// reused, without trusting the sender about how much is left.
func drain(body io.Reader) {
	_, _ = io.Copy(io.Discard, io.LimitReader(body, maxResponseBytes))
}
