package library

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/RequiDev/encore/internal/postgres"
	"github.com/RequiDev/encore/internal/store"
)

// TopSnapshot is the latest capture of Spotify's own ranking for one
// (user, kind, time_range) set.
//
// CapturedAt is a pointer because "nothing has ever been captured for this
// set" is a real state distinct from any actual capture time — a listener
// who has not opened the top-diff page yet, or whose grant lacks the scope —
// and it must read as nil, never as a zero time that would print as
// 0001-01-01.
type TopSnapshot struct {
	CapturedAt *time.Time
	// EntityIDs is in Spotify's rank order: index 0 is rank 1.
	EntityIDs []string
}

// replaceTopSnapshotSQL deletes whatever the incoming ranking no longer
// covers and upserts the rest, in one statement — the same
// delete-absent-plus-upsert-present shape as library.go's Replace* methods.
//
// The library tables reconcile by id: a row survives if its id is in the
// incoming set. Here the natural key is a position, not an id, so "absent"
// means "beyond the incoming ranking's length" rather than "id not in this
// list." $4 is that length, so the tail deletion is a plain range predicate
// on a prefix of the primary key (user_id, kind, time_range, position) —
// position > $4 — rather than a NOT IN or anti-join. A top-50 shrinking to a
// top-30 must delete positions 31-50: Spotify no longer reports those ranks,
// and a naive upsert-only statement would leave them behind to render as
// stale ranks nobody asked for.
//
// WITH ORDINALITY turns the incoming, rank-ordered array back into
// (entity_id, position) pairs: position is the 1-based index the caller's
// slice already encodes by its order, not a value carried alongside it.
const replaceTopSnapshotSQL = `
WITH input AS (
    SELECT entity_id, ordinality::int AS position
    FROM unnest($5::text[]) WITH ORDINALITY AS t(entity_id, ordinality)
),
stale AS (
    DELETE FROM spotify_top_snapshots
    WHERE user_id = $1 AND kind = $2 AND time_range = $3 AND position > $4
)
INSERT INTO spotify_top_snapshots (user_id, kind, time_range, position, entity_id, captured_at)
SELECT $1, $2, $3, position, entity_id, $6 FROM input
ON CONFLICT (user_id, kind, time_range, position) DO UPDATE
SET entity_id = EXCLUDED.entity_id, captured_at = EXCLUDED.captured_at`

// ReplaceTopSnapshot makes entityIDs the complete, latest ranking for
// (userID, kind, timeRange), in the rank order the caller passed them in.
//
// This is delete-absent-plus-upsert-present, not truncate-then-insert, for
// the same reason ReplaceSavedTracks is: a concurrent reader must never see
// the set as momentarily empty mid-refresh. Passing an empty entityIDs
// removes every row for the set — an empty ranking is a real state Spotify
// can report (a brand-new account has no top items yet), not "no data";
// nothing distinguishes "captured as empty" from "never captured" here
// except that TopSnapshot's CapturedAt is only set by the former.
func (r *Repo) ReplaceTopSnapshot(ctx context.Context, q store.Querier, userID uuid.UUID, kind, timeRange string, entityIDs []string, capturedAt time.Time) error {
	ids := entityIDs
	if ids == nil {
		ids = []string{}
	}
	if _, err := q.Exec(ctx, replaceTopSnapshotSQL,
		store.UUIDArg(userID), kind, timeRange, len(ids), ids, capturedAt.UTC(),
	); err != nil {
		return postgres.Classify("replace top snapshot", err)
	}
	return nil
}

// topSnapshotSQL reads one whole set ordered by rank. The primary key
// (user_id, kind, time_range, position) is itself the btree this ORDER BY
// walks — see migrations/00011_top_snapshots.sql — so no secondary index is
// needed to serve it.
const topSnapshotSQL = `
SELECT captured_at, entity_id
FROM spotify_top_snapshots
WHERE user_id = $1 AND kind = $2 AND time_range = $3
ORDER BY position`

// TopSnapshot reads the latest captured ranking for (userID, kind,
// timeRange), in rank order. CapturedAt is nil when nothing has ever been
// captured for this set — that reads as an empty EntityIDs and no error, not
// domain.ErrNotFound, since "not captured yet" is an expected state for a
// user who has never opened the page or whose grant lacks the scope.
func (r *Repo) TopSnapshot(ctx context.Context, q store.Querier, userID uuid.UUID, kind, timeRange string) (TopSnapshot, error) {
	rows, err := q.Query(ctx, topSnapshotSQL, store.UUIDArg(userID), kind, timeRange)
	if err != nil {
		return TopSnapshot{}, postgres.Classify("get top snapshot", err)
	}
	defer rows.Close()

	var snap TopSnapshot
	for rows.Next() {
		var (
			capturedAt time.Time
			entityID   string
		)
		if err := rows.Scan(&capturedAt, &entityID); err != nil {
			return TopSnapshot{}, postgres.Classify("scan top snapshot", err)
		}
		if snap.CapturedAt == nil {
			snap.CapturedAt = &capturedAt
		}
		snap.EntityIDs = append(snap.EntityIDs, entityID)
	}
	if err := rows.Err(); err != nil {
		return TopSnapshot{}, postgres.Classify("get top snapshot", err)
	}
	return snap, nil
}
