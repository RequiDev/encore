package enrich

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/RequiDev/encore/internal/domain"
	"github.com/RequiDev/encore/internal/logging"
)

// aliasBatch is how many name pairs one pass claims. Each costs its own search
// request, so the batch is small: a long claim held at two requests a second
// would keep work away from a second worker for no benefit.
const aliasBatch = 25

// RunAliasesOnce claims one batch of unresolved (artist, title) pairs and looks
// each of them up through /v1/search, one request at a time at cfg.AliasRate.
// It reports how many pairs it got an answer for, whether that answer was a
// track or that Spotify has none.
//
// A names-only listen is a real play of a real track; resolving its pair is what
// lets an account-data export and an extended export of the same period converge
// on one identity instead of double-counting the overlap.
func (w *Worker) RunAliasesOnce(ctx context.Context) (int, error) {
	if !w.cfg.AliasEnabled {
		return 0, nil
	}

	keys, err := w.dep.Catalog.ClaimPendingAliases(ctx, w.db(), aliasBatch, aliasLease(w.cfg.AliasRate, aliasBatch))
	if err != nil {
		return 0, wrap("claim pending aliases", err)
	}

	done := 0
	for _, key := range keys {
		// The pace is set here rather than by the client's own limiter: alias
		// resolution shares the application's quota with the catalogue batches,
		// and one search per pair would otherwise crowd them out entirely.
		if err := w.aliasRate.Wait(ctx); err != nil {
			return done, nil
		}
		if _, err := w.ResolveAlias(ctx, key); err != nil {
			if ctx.Err() != nil {
				return done, nil
			}
			if rateLimited(err) {
				// The client is already paused for the whole process. Give the rest
				// of the claim back by leaving it to expire.
				w.stat.EnrichRateLimited()
				w.log.Warn("alias resolution rate limited by spotify", "remaining", len(keys)-done)
				return done, nil
			}
			return done, wrap("resolve alias", err)
		}
		done++
	}
	return done, nil
}

// ResolveAlias looks one normalised (artist, title) pair up in Spotify's
// catalogue and records the outcome.
//
// It returns the resolved track id, or an empty string when Spotify's catalogue
// has nothing under those names — a local file, a bootleg, a track withdrawn
// since it was played. That is a normal outcome, not a failure: the listens keep
// their names and simply stay unresolved.
func (w *Worker) ResolveAlias(ctx context.Context, key domain.AliasKey) (string, error) {
	if key.IsZero() {
		return "", fmt.Errorf("%w: alias key is empty", domain.ErrValidation)
	}

	found, err := w.dep.Spotify.SearchTrack(ctx, key.ArtistNorm, key.TitleNorm)
	if err != nil {
		if ctx.Err() != nil || rateLimited(err) {
			return "", err
		}
		if markErr := w.markAliasFailed(ctx, key); markErr != nil {
			w.log.Error("could not record alias failure", logging.Err(markErr))
		}
		return "", err
	}

	if found == nil || !found.IsMusic() {
		if err := w.dep.Catalog.MarkAliasUnavailable(ctx, w.db(), key); err != nil {
			return "", wrap("mark alias unavailable", err)
		}
		w.stat.EnrichFailed(kindAlias, 1)
		return "", nil
	}

	track := found.ToDomainTrack()
	err = w.dep.Store.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		// The search answered with a full track object, so the catalogue row is
		// filled in here instead of being queued for the track worker to fetch all
		// over again. That also means the relink below points the listens at a
		// track that is already displayable.
		if err := w.dep.Catalog.UpsertTracks(ctx, tx, []domain.Track{track}); err != nil {
			return err
		}
		if err := w.dep.Catalog.ReplaceTrackArtists(ctx, tx, track.ID, track.ArtistIDs); err != nil {
			return err
		}
		return w.dep.Catalog.ResolveAlias(ctx, tx, key, track.ID)
	})
	if err != nil {
		return "", wrap("record resolved alias", err)
	}

	w.stat.EnrichResolved(kindAlias, 1)
	w.log.Debug("alias resolved", "artist", key.ArtistNorm, "title", key.TitleNorm, "track", track.ID)
	w.queueRelink(ctx, key, track.ID)
	return track.ID, nil
}

// markAliasFailed records a failed resolution attempt and schedules the next.
//
// The stored attempt count is read first because MarkAliasFailed writes it
// absolutely; an alias that has disappeared underneath us is treated as a first
// failure, which the statement's own guard then makes a no-op.
func (w *Worker) markAliasFailed(ctx context.Context, key domain.AliasKey) error {
	attempts := int32(1)
	alias, err := w.dep.Catalog.GetAlias(ctx, w.db(), key)
	switch {
	case err == nil:
		attempts = alias.FetchAttempts + 1
	case !errors.Is(err, domain.ErrNotFound):
		return wrap("read alias", err)
	}

	if err := w.dep.Catalog.MarkAliasFailed(ctx, w.db(), key, attempts, w.nextAttempt(attempts)); err != nil {
		return wrap("mark alias failed", err)
	}
	w.stat.EnrichFailed(kindAlias, 1)
	if attempts >= domain.BackoffAttempts {
		w.log.Warn("alias parked after exhausting its retries",
			"artist", key.ArtistNorm, "title", key.TitleNorm, "attempts", attempts)
	}
	return nil
}

// aliasLease is how long a claim of batch name pairs is held for.
//
// It covers the time the batch takes to work through at the configured rate,
// twice over, plus a minute of headroom for a rate-limit pause. A lease that
// expired mid-batch would hand pairs another worker is already searching for to
// a second one, which spends quota to learn the same answer.
func aliasLease(ratePerSecond float64, batch int) time.Duration {
	if ratePerSecond <= 0 {
		ratePerSecond = 1
	}
	if batch < 1 {
		batch = 1
	}
	drain := time.Duration(float64(batch) / ratePerSecond * float64(time.Second))
	return 2*drain + time.Minute
}
