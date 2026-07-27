package metadata

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/RequiDev/encore/internal/config"
	"github.com/RequiDev/encore/internal/retry"
	"github.com/RequiDev/encore/internal/spotify"
)

const (
	// maxResponseBytes caps a decoded body, as the Spotify client does. A mirror
	// is somebody's own server rather than Spotify's, which is a reason to be more
	// careful with what it returns, not less.
	maxResponseBytes = 8 << 20
	// defaultTimeout is generous: a mirror is likely a local process reading a
	// large database file, which can be slower than a warm CDN on a cold query.
	defaultTimeout = 10 * time.Second
	// defaultBatch is how many ids go in one request. Spotify's own limits are 50
	// (tracks, artists) and 20 (albums); a mirror is asked in the same shapes so
	// that an implementation can proxy Spotify verbatim if it wants to.
	defaultBatch = 50
)

// Mirror reads catalogue metadata from a Spotify-shaped HTTP endpoint.
//
// "Spotify-shaped" is the whole contract, and it is deliberately narrow — three
// GETs, one query parameter, one JSON envelope each. docs/metadata-fallback.md
// specifies it in full. Anything that answers those three requests works,
// whether it is a proxy in front of Spotify, a cache built from previous
// responses, or a local database.
//
// Mirror is safe for concurrent use.
type Mirror struct {
	base    string
	token   string
	http    *http.Client
	lg      *slog.Logger
	limiter *spotify.Limiter
	policy  retry.Policy
	batch   int
}

// MirrorOption customises a Mirror at construction.
type MirrorOption func(*Mirror)

// WithHTTPClient replaces the HTTP client, which is how tests point a Mirror at
// an httptest server.
func WithHTTPClient(h *http.Client) MirrorOption {
	return func(m *Mirror) {
		if h != nil {
			m.http = h
		}
	}
}

// WithLogger sets the logger.
func WithLogger(lg *slog.Logger) MirrorOption {
	return func(m *Mirror) {
		if lg != nil {
			m.lg = lg
		}
	}
}

// NewMirror builds a Mirror from configuration.
//
// It fails rather than defaulting when the URL is unusable: a fallback that
// silently does nothing is worse than one that refuses to start, because the
// symptom of the first is indistinguishable from the problem it was configured
// to solve.
func NewMirror(cfg config.MetadataFallback, opts ...MirrorOption) (*Mirror, error) {
	base := strings.TrimRight(strings.TrimSpace(cfg.URL), "/")
	if base == "" {
		return nil, errors.New("metadata: a fallback URL is required")
	}
	u, err := url.Parse(base)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("metadata: %q is not an absolute http(s) URL", cfg.URL)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("metadata: unsupported scheme %q in the fallback URL", u.Scheme)
	}

	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	batch := cfg.BatchSize
	if batch <= 0 || batch > defaultBatch {
		batch = defaultBatch
	}

	m := &Mirror{
		base:   base,
		token:  strings.TrimSpace(cfg.Token),
		http:   &http.Client{Timeout: timeout},
		lg:     slog.Default(),
		policy: retry.API().WithAttempts(3),
		batch:  batch,
	}
	// A rate of zero means "as fast as it will answer", which is the sensible
	// default for a server the operator owns. Anything positive gets the same
	// token bucket the Spotify client uses, so the two are paced by one mechanism.
	if cfg.RateLimit > 0 {
		burst := cfg.RateBurst
		if burst < 1 {
			burst = 1
		}
		m.limiter = spotify.NewLimiter(cfg.RateLimit, burst)
	}
	for _, o := range opts {
		o(m)
	}
	m.lg = m.lg.With("component", "metadata-fallback")
	return m, nil
}

// GetTracks reads track objects for ids the source knows. Unknown ids are absent
// from the result, exactly as they are from Spotify.
func (m *Mirror) GetTracks(ctx context.Context, ids []string) ([]spotify.Track, error) {
	return fetch[spotify.Track](ctx, m, "/v1/tracks", "tracks", ids)
}

// GetArtists reads artist objects. Unknown ids are absent from the result.
func (m *Mirror) GetArtists(ctx context.Context, ids []string) ([]spotify.Artist, error) {
	return fetch[spotify.Artist](ctx, m, "/v1/artists", "artists", ids)
}

// GetAlbums reads album objects. Unknown ids are absent from the result.
func (m *Mirror) GetAlbums(ctx context.Context, ids []string) ([]spotify.Album, error) {
	return fetch[spotify.Album](ctx, m, "/v1/albums", "albums", ids)
}

// entity is what the three catalogue types have in common for this package's
// purposes: the id the caller asked for.
type entity interface {
	spotify.Track | spotify.Artist | spotify.Album
}

// fetch performs the chunked read shared by all three endpoints.
//
// The envelope key is passed in rather than inferred because it is part of the
// published contract: `{"tracks":[…]}`, `{"artists":[…]}`, `{"albums":[…]}`. A
// source is free to return null in place of an entry it does not have, which is
// what Spotify does and what the decoder tolerates here.
func fetch[T entity](ctx context.Context, m *Mirror, path, key string, ids []string) ([]T, error) {
	clean := cleanIDs(ids)
	if len(clean) == 0 {
		return nil, nil
	}

	out := make([]T, 0, len(clean))
	for _, batch := range chunk(clean, m.batch) {
		var page map[string][]*T
		if err := m.get(ctx, path, batch, &page); err != nil {
			return nil, fmt.Errorf("metadata fallback: get %s: %w", key, err)
		}
		items, ok := page[key]
		if !ok {
			return nil, fmt.Errorf(
				"metadata fallback: get %s: response has no %q field; see docs/metadata-fallback.md",
				key, key)
		}
		for _, item := range items {
			if item == nil {
				continue
			}
			out = append(out, *item)
		}
	}
	return out, nil
}

// get performs one request with bounded retries.
func (m *Mirror) get(ctx context.Context, path string, ids []string, out any) error {
	target := m.base + path + "?" + url.Values{"ids": {strings.Join(ids, ",")}}.Encode()

	return retry.Do(ctx, m.policy, retry.Hooks{
		OnRetry: func(attempt int, delay time.Duration, err error) {
			m.lg.Debug("retrying metadata fallback request",
				"path", path, "attempt", attempt, "delay", delay, "error", err.Error())
		},
	}, func(ctx context.Context, _ int) error {
		if m.limiter != nil {
			if err := m.limiter.Wait(ctx); err != nil {
				return retry.Stop(err)
			}
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
		if err != nil {
			return retry.Stop(err)
		}
		req.Header.Set("Accept", "application/json")
		if m.token != "" {
			req.Header.Set("Authorization", "Bearer "+m.token)
		}

		resp, err := m.http.Do(req)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return retry.Stop(ctxErr)
			}
			return err
		}
		defer resp.Body.Close()

		switch {
		case resp.StatusCode >= 200 && resp.StatusCode < 300:
			if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(out); err != nil {
				return fmt.Errorf("decode response: %w", err)
			}
			return nil

		case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusForbidden:
			// Never retried and never carrying the token: a rejected credential is
			// a configuration error, and repeating it only makes the log longer.
			drain(resp.Body)
			return retry.Stop(fmt.Errorf(
				"the fallback rejected Encore's credentials (%d); check ENCORE_METADATA_FALLBACK_TOKEN",
				resp.StatusCode))

		case resp.StatusCode == http.StatusTooManyRequests:
			drain(resp.Body)
			return fmt.Errorf("the fallback is rate limiting Encore (429)")

		default:
			drain(resp.Body)
			return fmt.Errorf("the fallback answered %d", resp.StatusCode)
		}
	})
}

// drain reads and discards a small amount of a body so the connection can be
// reused, without reading an unbounded error page.
func drain(r io.Reader) { _, _ = io.Copy(io.Discard, io.LimitReader(r, 4<<10)) }

// cleanIDs trims, de-duplicates and drops ids that cannot be Spotify ids, the
// same filtering the Spotify client applies. A malformed id must not be able to
// spoil a batch of forty-nine good ones, whichever source is answering.
func cleanIDs(ids []string) []string {
	if len(ids) == 0 {
		return nil
	}
	out := make([]string, 0, len(ids))
	seen := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if !validID(id) {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

// validID checks the base-62 shape of a Spotify id.
func validID(s string) bool {
	if len(s) < 10 || len(s) > 64 {
		return false
	}
	for i := range len(s) {
		c := s[i]
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z':
		default:
			return false
		}
	}
	return true
}

// chunk splits s into runs of at most n.
func chunk[T any](s []T, n int) [][]T {
	if n < 1 {
		n = 1
	}
	out := make([][]T, 0, (len(s)+n-1)/n)
	for i := 0; i < len(s); i += n {
		out = append(out, s[i:min(i+n, len(s))])
	}
	return out
}
