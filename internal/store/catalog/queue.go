package catalog

import (
	"context"
	"fmt"
	"time"

	"github.com/requi/encore/internal/domain"
	"github.com/requi/encore/internal/postgres"
	"github.com/requi/encore/internal/store"
)

// defaultLease is used when a caller asks for a claim without saying how long it
// intends to hold it. A zero lease would leave next_attempt_at in the past and
// hand the same row to the next worker that polls, so it is never honoured.
const defaultLease = time.Minute

// maxClaim bounds a single claim. Spotify's widest batch endpoint takes 50 ids,
// so anything beyond a few hundred is a caller error rather than a workload.
const maxClaim = 500

// claimPendingSQL leases rows for enrichment.
//
// The subquery is the standard multi-worker queue pattern: SKIP LOCKED means a
// worker steps over rows another worker is already claiming instead of blocking
// behind them, and the surrounding UPDATE pushes next_attempt_at forward by the
// lease so a claimed row is invisible to the eligibility predicate until the
// lease expires. A worker that dies mid-batch therefore costs one lease of delay
// and nothing else.
//
// The ORDER BY mirrors the partial index (next_attempt_at NULLS FIRST, id)
// exactly, so the claim is an index scan over outstanding work rather than a
// sort of the whole catalogue.
const claimPendingSQL = `
UPDATE %[1]s AS x
SET next_attempt_at = now() + make_interval(secs => $2::double precision)
WHERE x.id IN (
    SELECT s.id FROM %[1]s AS s
    WHERE s.metadata_state IN ('pending', 'failed')
      AND (s.next_attempt_at IS NULL OR s.next_attempt_at <= now())
    ORDER BY s.next_attempt_at NULLS FIRST, s.id
    LIMIT $1
    FOR UPDATE SKIP LOCKED
)
RETURNING x.id`

// ClaimPending leases up to limit ids of one kind for enrichment and returns
// them. It is safe to call from several workers at once: an id is returned to at
// most one of them, and to nobody else until leaseFor has elapsed.
//
// The caller reports the outcome with UpsertTracks/UpsertAlbums/UpsertArtists,
// MarkUnavailable or MarkFetchFailed. If it reports nothing at all the row
// simply becomes claimable again when the lease expires.
func (r *Repo) ClaimPending(ctx context.Context, q store.Querier, kind Kind, limit int, leaseFor time.Duration) ([]string, error) {
	table, err := kind.table()
	if err != nil {
		return nil, err
	}
	if limit <= 0 {
		return nil, nil
	}
	limit = clampLimit(limit, maxClaim, maxClaim)
	if leaseFor <= 0 {
		leaseFor = defaultLease
	}

	rows, err := q.Query(ctx, fmt.Sprintf(claimPendingSQL, table), limit, leaseFor.Seconds())
	if err != nil {
		return nil, postgres.Classify("claim pending "+kind.String(), err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, postgres.Classify("scan claimed "+kind.String(), err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, postgres.Classify("claim pending "+kind.String(), err)
	}
	return out, nil
}

// markUnavailableSQL parks entities Spotify has authoritatively nothing for.
//
// The state guard matters: a worker may be reporting the outcome of a fetch that
// started before another worker resolved the same row, and a stale report must
// never undo a good one.
const markUnavailableSQL = `
UPDATE %[1]s
SET metadata_state = 'unavailable', next_attempt_at = NULL, fetched_at = now()
WHERE id = ANY($1::text[]) AND metadata_state IN ('pending', 'failed')`

// MarkUnavailable records that Spotify has nothing for these ids: deleted,
// region-locked, or relinked to an id that no longer exists. The rows leave the
// queue permanently; only the repair job revisits them.
func (r *Repo) MarkUnavailable(ctx context.Context, q store.Querier, kind Kind, ids []string) error {
	table, err := kind.table()
	if err != nil {
		return err
	}
	ids = dedupeIDs(ids)
	if len(ids) == 0 {
		return nil
	}
	if _, err := q.Exec(ctx, fmt.Sprintf(markUnavailableSQL, table), ids); err != nil {
		return postgres.Classify("mark "+kind.String()+" unavailable", err)
	}
	return nil
}

// markFetchFailedSQL records a failed attempt and schedules the next one.
//
// The state flips to 'failed' only once the attempt budget is spent. Both states
// stay in the queue's partial index, so a failed row is still picked up after
// its backoff; the distinction is what makes "how much of the catalogue is
// stuck" answerable in the admin diagnostics.
const markFetchFailedSQL = `
UPDATE %[1]s
SET fetch_attempts  = $2::int,
    next_attempt_at = $3::timestamptz,
    metadata_state  = CASE WHEN $2::int >= $4::int THEN 'failed' ELSE 'pending' END
WHERE id = ANY($1::text[]) AND metadata_state IN ('pending', 'failed')`

// MarkFetchFailed records that an enrichment attempt failed for these ids.
//
// attempts is the running total after this failure, and nextAttemptAt is when
// the rows become claimable again. A zero nextAttemptAt is replaced with the
// domain backoff for that attempt number, because a row whose next attempt is in
// the past is claimed again immediately and would spin the worker.
func (r *Repo) MarkFetchFailed(ctx context.Context, q store.Querier, kind Kind, ids []string, attempts int32, nextAttemptAt time.Time) error {
	table, err := kind.table()
	if err != nil {
		return err
	}
	ids = dedupeIDs(ids)
	if len(ids) == 0 {
		return nil
	}
	if attempts < 1 {
		attempts = 1
	}
	if nextAttemptAt.IsZero() {
		nextAttemptAt = time.Now().Add(domain.NextMetadataAttempt(attempts))
	}
	_, err = q.Exec(ctx, fmt.Sprintf(markFetchFailedSQL, table),
		ids, attempts, nextAttemptAt.UTC(), int32(domain.BackoffAttempts))
	if err != nil {
		return postgres.Classify("mark "+kind.String()+" fetch failed", err)
	}
	return nil
}

// Pending is the depth of each enrichment queue, exported on /metrics as the
// gauge that tells an operator whether enrichment is keeping up with ingestion.
type Pending struct {
	Tracks  int64
	Albums  int64
	Artists int64
	Aliases int64
}

// Total is the outstanding work across all four queues.
func (p Pending) Total() int64 { return p.Tracks + p.Albums + p.Artists + p.Aliases }

// pendingCountsSQL counts everything the queue will still hand out. Each count
// is covered by the partial index for that table, so this stays proportional to
// the work outstanding rather than to the size of the catalogue.
const pendingCountsSQL = `
SELECT
    (SELECT count(*) FROM tracks        WHERE metadata_state IN ('pending', 'failed'))::bigint,
    (SELECT count(*) FROM albums        WHERE metadata_state IN ('pending', 'failed'))::bigint,
    (SELECT count(*) FROM artists       WHERE metadata_state IN ('pending', 'failed'))::bigint,
    (SELECT count(*) FROM track_aliases WHERE state          IN ('pending', 'failed'))::bigint`

// PendingCounts reports how many entities of each kind are still waiting for
// enrichment, counting both the never-tried ('pending') and the exhausted
// ('failed') rows, since the queue hands out both.
func (r *Repo) PendingCounts(ctx context.Context, q store.Querier) (Pending, error) {
	var p Pending
	err := q.QueryRow(ctx, pendingCountsSQL).Scan(&p.Tracks, &p.Albums, &p.Artists, &p.Aliases)
	if err != nil {
		return Pending{}, postgres.Classify("count pending catalogue entities", err)
	}
	return p, nil
}
