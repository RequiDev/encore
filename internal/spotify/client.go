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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/RequiDev/encore/internal/config"
	"github.com/RequiDev/encore/internal/retry"
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
	maxRetryDelay = 30 * time.Second
	// quotaExhaustedAfter is the Retry-After beyond which a 429 is not pacing but
	// the application's daily quota having run out.
	quotaExhaustedAfter = 5 * time.Minute
	defaultTimeout      = 20 * time.Second
	// signinRate and signinBurst are the sign-in path's own budget.
	//
	// It is small but separate. Authenticating costs two requests and happens a
	// handful of times a day; it is not what exhausts a quota, and it must not
	// queue behind the thing that did.
	signinRate  = 5
	signinBurst = 10
	// signinWait is how long a person's request may queue for a token before
	// Encore gives up and says why. Long enough to absorb ordinary pacing, far
	// short of a browser's patience.
	signinWait         = 5 * time.Second
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
	// signin is the rate budget for the calls a person is waiting on: the OAuth
	// token exchange and the profile read behind it.
	//
	// It is deliberately not the limiter above. A 429 on a catalogue read pauses
	// that one for as long as Spotify asks, which for an exhausted daily quota is
	// most of a day — and sharing it meant a large import could lock everybody
	// out of their own instance until the quota reset. Worse, the token exchange
	// is not even the same service: it is accounts.spotify.com, which never
	// rate limited anybody here.
	//
	// Nothing a background worker does may take authentication offline.
	signin *Limiter
	// onPause reports a newly declared pause so it can be recorded somewhere
	// that survives a restart.
	onPause func(until time.Time)
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

// WithPauseObserver is called whenever a 429 makes the client hold everything
// back, with the instant it will resume.
//
// The limiter's pause lives in memory, so without somewhere to record it a
// restart forgets it entirely and the next process immediately spends requests
// against a quota that is still exhausted — earning a fresh ban and, on a
// development-mode application, another day without metadata. The observer is
// how the worker persists it; internal/spotify stays free of any database.
func WithPauseObserver(fn func(until time.Time)) Option {
	return func(c *Client) {
		if fn != nil {
			c.onPause = fn
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
	// Never shared and never restored from a recorded pause: a quota ban recorded
	// yesterday says nothing about whether somebody may sign in today.
	c.signin = NewLimiterWithClock(signinRate, signinBurst, c.clock)
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
	// json is a body marshalled as application/json. The Web API takes form
	// bodies only at the accounts service; everything under /v1 that writes takes
	// JSON.
	json any
	// raw is a body sent verbatim under contentType. The playlist cover upload
	// is the only caller: PUT /v1/playlists/{id}/images takes base64 of a JPEG
	// under Content-Type: image/jpeg, which is neither of the two shapes above.
	//
	// It is []byte rather than an io.Reader on purpose. attempt() runs once per
	// retry and must build a fresh reader each time; a Reader stored here would
	// be drained by the first attempt and the second would send an empty body,
	// which for an endpoint that *replaces* a cover means replacing it with
	// nothing.
	raw         []byte
	contentType string
	out         any
	// interactive marks a request a person is waiting on. Those draw on the
	// sign-in budget rather than the application's catalogue quota, and they
	// refuse to queue indefinitely.
	interactive bool
}

// get issues an authenticated GET against the Web API.
func (c *Client) get(ctx context.Context, path, label string, query url.Values, accessToken string, out any) error {
	return c.getClass(ctx, path, label, query, accessToken, out, false)
}

// getClass is get with the request class spelled out.
func (c *Client) getClass(
	ctx context.Context,
	path, label string,
	query url.Values,
	accessToken string,
	out any,
	interactive bool,
) error {
	if accessToken == "" {
		return fmt.Errorf("%s: no access token", label)
	}
	return c.do(ctx, request{
		method:      http.MethodGet,
		url:         c.endpoint(path, query),
		label:       label,
		bearer:      accessToken,
		out:         out,
		interactive: interactive,
	})
}

// budget picks the limiter a request draws on, and how long it may queue.
func (c *Client) budget(r request) (*Limiter, time.Duration) {
	if r.interactive {
		return c.signin, signinWait
	}
	return c.limiter, 0
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
	limiter, wait := c.budget(r)
	if err := limiter.WaitMax(ctx, wait); err != nil {
		// A finished context is the caller's decision, not a failure to retry, and
		// a pause that outlasts an interactive budget will not have cleared by the
		// next attempt either.
		return retry.Stop(err)
	}

	var body io.Reader
	switch {
	case r.form != nil:
		body = strings.NewReader(r.form.Encode())
	case r.raw != nil:
		body = bytes.NewReader(r.raw)
	case r.json != nil:
		raw, err := json.Marshal(r.json)
		if err != nil {
			return retry.Stop(fmt.Errorf("%s: encode body: %w", r.label, err))
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, r.method, r.url, body)
	if err != nil {
		return retry.Stop(fmt.Errorf("%s: build request: %w", r.label, err))
	}
	req.Header.Set("Accept", "application/json")
	switch {
	case r.form != nil:
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	case r.raw != nil:
		req.Header.Set("Content-Type", r.contentType)
	case r.json != nil:
		req.Header.Set("Content-Type", "application/json")
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
		resumeAt := now.Add(retryAfter)
		limiter, _ := c.budget(r)
		limiter.Pause(resumeAt)
		// Only a catalogue pause is recorded. What is persisted is restored into
		// the catalogue limiter at startup and reported to users as "metadata is
		// waiting"; a refused sign-in is neither of those things, and recording it
		// would hold enrichment back over something that never touched its quota.
		if c.onPause != nil && !r.interactive {
			c.onPause(resumeAt)
		}
		// A short pause is ordinary pacing. A long one means the application's
		// daily quota is gone, which is a different situation entirely: nothing
		// will be fetched until it resets, so say so in terms an operator can act
		// on rather than logging a duration in nanoseconds and moving on.
		if retryAfter >= quotaExhaustedAfter || strings.Contains(apiErr.Body, "QUOTA_EXCEEDED") {
			c.lg.Error("spotify daily quota exhausted; metadata enrichment is paused",
				"endpoint", r.label,
				"resumes_at", resumeAt.UTC().Format(time.RFC3339),
				"paused_for", retryAfter.Round(time.Minute).String(),
				"hint", "a development-mode app has a small daily quota; lower ENCORE_SPOTIFY_RATE_LIMIT "+
					"or apply for extended quota mode. Listening data is unaffected.")
		} else {
			c.lg.Warn("spotify rate limited",
				"endpoint", r.label, "retry_after", retryAfter.String())
		}
		if r.interactive {
			// Nobody waits half a minute to be told no. The limiter now holds the
			// real delay, so a further attempt would fail its bounded wait anyway —
			// and would do it after sleeping through a retry the person did not ask
			// for. Answer immediately, with the instant the pause lifts, so the
			// interface can say something true and specific.
			return retry.Stop(&PausedError{Until: resumeAt})
		}
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
