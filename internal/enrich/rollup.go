package enrich

import (
	"context"
	"time"

	"github.com/requi/encore/internal/stats"
	"github.com/requi/encore/internal/store/catalog"
)

// backlogInterval is how often the queue-depth gauges are refreshed.
//
// It is deliberately slower than the rollup tick: each count scans the partial
// index of outstanding work, which is largest exactly when an operator is
// watching the gauge, and a backlog is not a quantity that needs second-level
// resolution.
const backlogInterval = time.Minute

// RunRollupsOnce recomputes one chunk of the statistics days ingestion marked
// dirty.
//
// The rollup is an optimisation rather than a source of truth — a range with a
// dirty day is answered from the fact table instead — so falling behind here
// makes wide queries slower and never makes them wrong.
func (w *Worker) RunRollupsOnce(ctx context.Context) error {
	if err := w.dep.Stats.RefreshDirtyDays(ctx, stats.DefaultRollupChunk); err != nil {
		return wrap("refresh dirty rollup days", err)
	}
	return nil
}

// PublishBacklog reads the depth of each enrichment queue and publishes it, so
// an operator can see whether enrichment is keeping up with ingestion.
func (w *Worker) PublishBacklog(ctx context.Context) (catalog.Pending, error) {
	pending, err := w.dep.Catalog.PendingCounts(ctx, w.db())
	if err != nil {
		return catalog.Pending{}, wrap("count pending catalogue entities", err)
	}
	w.stat.EnrichPending(catalog.KindTrack.String(), pending.Tracks)
	w.stat.EnrichPending(catalog.KindAlbum.String(), pending.Albums)
	w.stat.EnrichPending(catalog.KindArtist.String(), pending.Artists)
	w.stat.EnrichPending(kindAlias, pending.Aliases)
	return pending, nil
}

// rollupStep is the rollup loop's body.
//
// It carries the backlog gauges as well, on their own slower cadence, because an
// eighth goroutine whose whole job is one count query every minute would cost
// more to reason about than the throttle does. The closure owns the schedule, so
// nothing is shared with a caller driving PublishBacklog directly.
func (w *Worker) rollupStep() func(context.Context) (bool, error) {
	var lastBacklog time.Time
	return func(ctx context.Context) (bool, error) {
		if err := w.RunRollupsOnce(ctx); err != nil {
			return false, err
		}
		if now := w.now(); now.Sub(lastBacklog) >= backlogInterval {
			if _, err := w.PublishBacklog(ctx); err != nil {
				return false, err
			}
			lastBacklog = now
		}
		return false, nil
	}
}
