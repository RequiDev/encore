package enrich

import (
	"context"
	"errors"
	"time"

	"github.com/RequiDev/encore/internal/domain"
	"github.com/RequiDev/encore/internal/store/catalog"
)

// repairBatch is how many ids of one kind a repair pass inspects. The pass makes
// no Spotify request at all, so the batch is bounded only by how much work one
// short-lived claim should take out of circulation at a time.
const repairBatch = 200

// repairAliasBatch is smaller than repairBatch because each alias costs its own
// round trip to inspect.
const repairAliasBatch = 50

// repairLease is short because the repair pass does not fetch anything: it looks
// at what it claimed and hands most of it straight back. Anything it claims that
// was not parked waits out this lease and nothing more.
const repairLease = 15 * time.Second

// repairAttempts is the attempt count a requeued row starts again from. It is
// one rather than zero because the row has failed before, and the schedule that
// follows should be the second attempt's, not the first's.
const repairAttempts = 1

// RunRepairOnce returns catalogue rows parked in the failed state to the queue
// with a fresh attempt budget, and reports how many it requeued.
//
// A row is parked once it has spent domain.BackoffAttempts attempts, which on a
// long Spotify outage or a bad deployment can happen to a whole catalogue at
// once. Without this pass those rows would be retried for ever at the six-hour
// cap of the backoff; with it, every ENCORE_ENRICH_REPAIR_INTERVAL they get a
// complete retry ladder again, which is what turns a transient outage into a
// delay rather than a permanent hole in the catalogue.
//
// The requeue is a MarkFetchFailed with the attempt count reset and the next
// attempt due immediately: that is precisely the write a requeue needs, and the
// state guard on the statement means a row another worker has since resolved is
// left alone.
func (w *Worker) RunRepairOnce(ctx context.Context) (int, error) {
	requeued := 0
	for _, kind := range catalog.Kinds {
		n, err := w.repairKind(ctx, kind)
		requeued += n
		if err != nil {
			return requeued, err
		}
	}
	n, err := w.repairAliases(ctx)
	return requeued + n, err
}

// repairKind requeues the parked rows of one catalogue kind.
//
// The queue hands out pending and failed rows together and offers no way to ask
// for one of them, so the pass claims a batch, reads back which of those rows
// are parked, and requeues only those. Anything claimed that was merely pending
// is left to its short lease.
func (w *Worker) repairKind(ctx context.Context, kind catalog.Kind) (int, error) {
	ids, err := w.dep.Catalog.ClaimPending(ctx, w.db(), kind, repairBatch, repairLease)
	if err != nil {
		return 0, wrap("claim "+kind.String()+"s for repair", err)
	}
	if len(ids) == 0 {
		return 0, nil
	}

	rows, err := w.catalogueRows(ctx, kind, ids)
	if err != nil {
		return 0, wrap("read "+kind.String()+"s for repair", err)
	}

	parked := make([]string, 0, len(ids))
	for _, id := range ids {
		if rows[id].state == domain.MetadataFailed {
			parked = append(parked, id)
		}
	}
	if len(parked) == 0 {
		return 0, nil
	}

	if err := w.dep.Catalog.MarkFetchFailed(ctx, w.db(), kind, parked, repairAttempts, w.now()); err != nil {
		return 0, wrap("requeue parked "+kind.String()+"s", err)
	}
	w.log.Info("requeued parked catalogue rows", "kind", kind.String(), "count", len(parked))
	return len(parked), nil
}

// repairAliases requeues name pairs parked after their searches failed.
//
// Aliases are read one at a time because their key is composite and the
// repository has no batch loader for them. That is affordable precisely because
// this runs on the repair interval rather than the enrichment one.
func (w *Worker) repairAliases(ctx context.Context) (int, error) {
	if !w.cfg.AliasEnabled {
		return 0, nil
	}

	keys, err := w.dep.Catalog.ClaimPendingAliases(ctx, w.db(), repairAliasBatch, repairLease)
	if err != nil {
		return 0, wrap("claim aliases for repair", err)
	}

	requeued := 0
	for _, key := range keys {
		if err := ctx.Err(); err != nil {
			return requeued, err
		}
		alias, err := w.dep.Catalog.GetAlias(ctx, w.db(), key)
		if err != nil {
			if errors.Is(err, domain.ErrNotFound) {
				continue
			}
			return requeued, wrap("read alias for repair", err)
		}
		if alias.State != domain.MetadataFailed {
			continue
		}
		if err := w.dep.Catalog.MarkAliasFailed(ctx, w.db(), key, repairAttempts, w.now()); err != nil {
			return requeued, wrap("requeue parked alias", err)
		}
		requeued++
	}
	if requeued > 0 {
		w.log.Info("requeued parked aliases", "count", requeued)
	}
	return requeued, nil
}
