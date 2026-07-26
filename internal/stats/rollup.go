package stats

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/requi/encore/internal/domain"
	"github.com/requi/encore/internal/postgres"
	"github.com/requi/encore/internal/store"
)

// RollupMinRange is how wide a requested range must be before a top-N query is
// allowed to read listen_daily_rollup instead of the fact table. Below this the
// fact table is fast enough that the pre-aggregate buys nothing worth the risk of
// answering from stale data.
const RollupMinRange = 90 * 24 * time.Hour

// DefaultRollupChunk is how many dirty (user, day) pairs one RefreshDirtyDays
// call takes when the caller does not say. It bounds both the transaction's
// duration and how much work is lost if the worker dies mid-chunk.
const DefaultRollupChunk = 500

// isLocalMidnight reports whether an instant is exactly a local day boundary.
func isLocalMidnight(t time.Time, loc *time.Location) bool {
	l := t.In(loc)
	return l.Hour() == 0 && l.Minute() == 0 && l.Second() == 0 && l.Nanosecond() == 0
}

// rollupCheckRange is the span whose rollups must be clean before a top-N query
// may use them: the requested period plus the preceding one it is compared
// against.
func rollupCheckRange(r domain.TimeRange) domain.TimeRange {
	return domain.TimeRange{From: r.Previous().From, To: r.To}
}

// rollupEligible is the half of the decision that costs nothing to evaluate. It
// is checked before the dirty-day query so that the common case, a short range,
// never pays for that query at all.
//
// listen_daily_rollup counts whole local days, so it can only answer a range
// whose own bounds are local midnights; anything else would silently round the
// endpoints of the headline numbers. The preceding period's bounds are
// deliberately not required to align: a daylight-saving change inside the range
// shifts them off midnight, and they feed nothing but the rank-movement
// indicator, where a comparison window that begins an hour early is invisible.
func rollupEligible(r domain.TimeRange, loc *time.Location) bool {
	return r.Duration() > RollupMinRange && isLocalMidnight(r.From, loc) && isLocalMidnight(r.To, loc)
}

// useRollup is the whole decision: a wide, day-aligned range whose rollups are
// known to be up to date. When in doubt the answer is no, and the caller scans
// the fact table.
func useRollup(r domain.TimeRange, loc *time.Location, dirty bool) bool {
	return !dirty && rollupEligible(r, loc)
}

// hasDirtyDaysSQL asks whether any local day touching the range is awaiting
// recomputation. The end of the range is compared inclusively: a range ending at
// local midnight cannot contain listens from that final day, but treating one
// extra day as dirty only costs a fact-table scan, whereas missing a dirty day
// would return wrong numbers.
const hasDirtyDaysSQL = `
SELECT EXISTS (
    SELECT 1 FROM rollup_dirty_days d
    WHERE d.user_id = $1
      AND d.day >= (($2::timestamptz) AT TIME ZONE $4::text)::date
      AND d.day <= (($3::timestamptz) AT TIME ZONE $4::text)::date)`

// HasDirtyDays reports whether the user's daily rollup is known to be out of date
// anywhere in the range.
func (s *Service) HasDirtyDays(ctx context.Context, q store.Querier, userID uuid.UUID, r domain.TimeRange, tz string) (bool, error) {
	if _, err := scope(userID, r, tz); err != nil {
		return false, err
	}
	var dirty bool
	err := q.QueryRow(ctx, hasDirtyDaysSQL,
		store.UUIDArg(userID), r.From.UTC(), r.To.UTC(), tzArg(tz)).Scan(&dirty)
	if err != nil {
		return false, postgres.Classify("check dirty rollup days", err)
	}
	return dirty, nil
}

// refreshDirtyDaysSQL recomputes a chunk of dirty days in one statement.
//
// The claim uses FOR UPDATE SKIP LOCKED so several workers can drain the queue
// at once without ever handing the same day to two of them. Each day's window is
// resolved through the owner's current timezone, which is correct because
// changing a timezone marks that user's whole history dirty.
//
// Deleting from and inserting into listen_daily_rollup in a single statement is
// only safe because the two touch disjoint rows: the delete explicitly excludes
// every row the insert is about to produce, and the insert upserts the rest. That
// removes track rows that no longer have listens on the day without ever racing
// the insert against the delete.
const refreshDirtyDaysSQL = `
WITH claimed AS (
    SELECT d.user_id, d.day
    FROM rollup_dirty_days d
    ORDER BY d.created_at
    LIMIT $1
    FOR UPDATE SKIP LOCKED
),
fresh AS (
    SELECT c.user_id, c.day, l.track_id,
           count(*)::int AS plays,
           coalesce(sum(l.ms_played), 0)::bigint AS ms
    FROM claimed c
    JOIN users u ON u.id = c.user_id
    JOIN listens l
      ON l.user_id = c.user_id
     AND l.played_at >= (c.day::timestamp AT TIME ZONE u.timezone)
     AND l.played_at <  ((c.day + 1)::timestamp AT TIME ZONE u.timezone)
    WHERE l.track_id IS NOT NULL
    GROUP BY c.user_id, c.day, l.track_id
),
removed AS (
    DELETE FROM listen_daily_rollup r
    USING claimed c
    WHERE r.user_id = c.user_id AND r.day = c.day
      AND NOT EXISTS (
          SELECT 1 FROM fresh f
          WHERE f.user_id = r.user_id AND f.day = r.day AND f.track_id = r.track_id)
),
upserted AS (
    INSERT INTO listen_daily_rollup (user_id, day, track_id, plays, ms)
    SELECT user_id, day, track_id, plays, ms FROM fresh
    ON CONFLICT (user_id, day, track_id) DO UPDATE
        SET plays = EXCLUDED.plays, ms = EXCLUDED.ms
),
cleared AS (
    DELETE FROM rollup_dirty_days d
    USING claimed c
    WHERE d.user_id = c.user_id AND d.day = c.day
)
SELECT count(*)::bigint FROM claimed`

// RefreshDirtyDays recomputes up to limit dirty (user, day) pairs from the fact
// table and clears their markers.
//
// It owns its transaction rather than taking a Querier because the claim, the
// recomputation and the clearing of the markers must commit together: a marker
// cleared without its rollup rewritten would leave a wrong answer that nothing
// would ever come back to fix. A worker calls this on a timer until the queue is
// drained.
func (s *Service) RefreshDirtyDays(ctx context.Context, limit int) error {
	if limit <= 0 {
		limit = DefaultRollupChunk
	}
	return s.db.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		// The row count is discarded: the statement's data-modifying CTEs are
		// executed to completion whether or not the primary query is read, and
		// callers only need to know that the chunk committed.
		if _, err := tx.Exec(ctx, refreshDirtyDaysSQL, limit); err != nil {
			return postgres.Classify("refresh dirty rollup days", err)
		}
		return nil
	})
}
