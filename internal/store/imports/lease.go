package imports

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/requi/encore/internal/domain"
	"github.com/requi/encore/internal/postgres"
	"github.com/requi/encore/internal/store"
)

// claimJobSQL takes the oldest job that needs a worker and stamps a lease on it
// in a single statement.
//
// The inner SELECT ... FOR UPDATE SKIP LOCKED is what makes several worker
// processes safe without any coordination between them: each locks a different
// candidate row and no two workers can ever claim the same job.
//
// Three kinds of job are candidates:
//
//   - queued: never started.
//   - running with an expired lease: this is the entire crash-recovery
//     mechanism. A worker that dies stops heartbeating, and its job becomes
//     ordinary work again once the lease runs out.
//   - paused: a worker stopped it cleanly at a batch boundary while shutting
//     down. Paused means "no one is working on this and nothing is wrong with
//     it", so it is picked up immediately rather than waiting out a lease that
//     the departing worker already knew it would not renew. A job the *user*
//     stopped is 'cancelled', not 'paused', and is deliberately not a candidate.
//
// cancel_requested is excluded in every case: a job the user has asked to stop
// must not be picked straight back up by another worker.
const claimJobSQL = `
    UPDATE import_jobs
    SET status = 'running',
        lease_owner = $1,
        lease_expires_at = now() + make_interval(secs => $2),
        started_at = COALESCE(started_at, now())
    WHERE id = (
        SELECT id FROM import_jobs
        WHERE NOT cancel_requested
          AND (status = 'queued'
               OR status = 'paused'
               OR (status = 'running' AND lease_expires_at < now()))
        ORDER BY created_at
        LIMIT 1
        FOR UPDATE SKIP LOCKED
    )
    RETURNING ` + jobColumns

// ClaimJob leases the next job that needs work to owner, returning (nil, nil)
// when the queue is empty.
//
// The returned job carries its files and aggregate counters, so the worker has
// everything it needs to resume from the checkpoints without a second lookup.
// leaseFor should be comfortably longer than the heartbeat interval: it is how
// long a crashed worker's job stays untouchable.
func (r *Repo) ClaimJob(ctx context.Context, q store.Querier, owner string, leaseFor time.Duration) (*domain.ImportJob, error) {
	if owner == "" {
		// An empty owner would match the default of every unleased row, so any
		// worker could then heartbeat or release any job.
		return nil, fmt.Errorf("%w: lease owner must not be empty", domain.ErrValidation)
	}
	if leaseFor <= 0 {
		return nil, fmt.Errorf("%w: lease duration must be positive", domain.ErrValidation)
	}

	job, err := scanJob(q.QueryRow(ctx, claimJobSQL, owner, leaseFor.Seconds()))
	if err != nil {
		classified := postgres.Classify("claim import job", err)
		if errors.Is(classified, domain.ErrNotFound) {
			// Nothing to do is the normal state of an idle worker.
			return nil, nil
		}
		return nil, classified
	}
	if err := r.attachFiles(ctx, q, []*domain.ImportJob{&job}); err != nil {
		return nil, err
	}
	return &job, nil
}

const heartbeatSQL = `
    UPDATE import_jobs
    SET lease_expires_at = now() + make_interval(secs => $3)
    WHERE id = $1 AND lease_owner = $2 AND status = 'running'`

// Heartbeat extends a lease and reports whether the worker still holds it.
//
// A false result means the lease was taken over — the worker was slow, paused,
// or partitioned for longer than the lease, and another worker is now importing
// this job. The only safe response is to stop immediately: continuing would have
// two workers writing checkpoints for the same file, and the one that is behind
// would be writing over the other's progress.
func (r *Repo) Heartbeat(ctx context.Context, q store.Querier, id uuid.UUID, owner string, leaseFor time.Duration) (bool, error) {
	if owner == "" {
		return false, fmt.Errorf("%w: lease owner must not be empty", domain.ErrValidation)
	}
	if leaseFor <= 0 {
		return false, fmt.Errorf("%w: lease duration must be positive", domain.ErrValidation)
	}
	tag, err := q.Exec(ctx, heartbeatSQL, store.UUIDArg(id), owner, leaseFor.Seconds())
	if err != nil {
		return false, postgres.Classify("heartbeat import job", err)
	}
	return tag.RowsAffected() == 1, nil
}

const releaseLeaseSQL = `
    UPDATE import_jobs
    SET lease_owner = '', lease_expires_at = NULL
    WHERE id = $1 AND lease_owner = $2`

// ReleaseLease hands a job back so it can be claimed again without waiting for
// the lease to expire, which is what makes a graceful shutdown fast.
//
// Releasing a lease the caller no longer owns is a no-op rather than an error:
// this is called from shutdown paths, where the only thing a failure could
// achieve is noise, and the owner guard already prevents it from disturbing the
// worker that took over.
func (r *Repo) ReleaseLease(ctx context.Context, q store.Querier, id uuid.UUID, owner string) error {
	if owner == "" {
		return fmt.Errorf("%w: lease owner must not be empty", domain.ErrValidation)
	}
	if _, err := q.Exec(ctx, releaseLeaseSQL, store.UUIDArg(id), owner); err != nil {
		return postgres.Classify("release import job lease", err)
	}
	return nil
}
