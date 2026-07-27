package worker

import (
	"context"
	"log/slog"
	"time"

	"github.com/RequiDev/encore/internal/config"
	"github.com/RequiDev/encore/internal/logging"
	"github.com/RequiDev/encore/internal/spotify"
	"github.com/RequiDev/encore/internal/store"
	"github.com/RequiDev/encore/internal/store/accounts"
)

// SpotifyPauseOptions makes a rate-limit pause survive a restart.
//
// The limiter holds its pause in memory. That is fine for the ordinary case, a
// pause of a few seconds, but Spotify answers an exhausted daily quota with a
// Retry-After of most of a day. A process restarted inside that window would
// start with a clean limiter, immediately spend requests against a quota that
// has not reset, collect a fresh 429, and quite possibly push the reset further
// out — so restarting Encore while it is banned would make the ban worse, which
// is the opposite of what anyone restarting it expects.
//
// Recording the instant in app_settings and restoring it at startup turns that
// into a no-op: the new process simply waits out whatever is left.
//
// Both halves are best effort. Failing to read or write a setting must never
// stop the process starting; the cost of losing it is one wasted request.
func SpotifyPauseOptions(
	ctx context.Context,
	cfg config.Spotify,
	settings *accounts.Settings,
	db *store.Store,
	lg *slog.Logger,
) []spotify.Option {
	if settings == nil || db == nil {
		return nil
	}
	if lg == nil {
		lg = slog.Default()
	}

	record := func(until time.Time) {
		// Detached from the request that triggered it: the pause must be written
		// even when the call that provoked the 429 is being cancelled.
		saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := settings.SetSpotifyPausedUntil(saveCtx, db.DB(), until); err != nil {
			lg.Warn("could not record the Spotify pause; a restart will retry too early",
				logging.Err(err))
		}
	}

	opts := []spotify.Option{spotify.WithPauseObserver(record)}

	stored, err := settings.SpotifyPausedUntil(ctx, db.DB())
	if err != nil {
		lg.Warn("could not read the stored Spotify pause", logging.Err(err))
		return opts
	}
	if remaining := time.Until(stored); remaining > 0 {
		limiter := spotify.NewLimiter(cfg.RateLimit, cfg.RateBurst)
		limiter.Pause(stored)
		opts = append(opts, spotify.WithLimiter(limiter))
		lg.Warn("Spotify is still rate limiting this application; metadata enrichment stays paused",
			"resumes_at", stored.UTC().Format(time.RFC3339),
			"remaining", remaining.Round(time.Minute).String(),
			"note", "listening data is unaffected; only names, artwork and genres wait")
	}
	return opts
}
