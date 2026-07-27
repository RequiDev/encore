package stats

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/RequiDev/encore/internal/domain"
	"github.com/RequiDev/encore/internal/postgres"
	"github.com/RequiDev/encore/internal/store"
)

// longestSessionsSQL groups consecutive listens into sessions entirely in SQL.
//
// The classic gaps-and-islands shape: lag() exposes the previous listen, a flag
// marks every listen that starts a new island because the silence before it
// exceeded the gap, and a running sum of that flag numbers the islands. Doing it
// this way means a decade of history never leaves the database, which pulling the
// rows into Go to fold them would require.
//
// The silence is measured from the end of the previous listen (its start plus its
// own ms_played), not from its start, so a long track does not look like a gap.
var longestSessionsSQL = fmt.Sprintf(`
WITH base AS (
    SELECT l.id, l.played_at, l.ms_played, l.track_id
    FROM listens l
    WHERE %s
),
flagged AS (
    SELECT b.id, b.played_at, b.ms_played, b.track_id,
           CASE
               WHEN lag(b.played_at) OVER w IS NULL THEN 1
               WHEN b.played_at > (lag(b.played_at) OVER w)
                                  + make_interval(secs => (lag(b.ms_played) OVER w) / 1000.0)
                                  + make_interval(secs => $4::float8) THEN 1
               ELSE 0
           END AS is_start
    FROM base b
    WINDOW w AS (ORDER BY b.played_at, b.id)
),
grouped AS (
    SELECT f.id, f.played_at, f.ms_played, f.track_id,
           sum(f.is_start) OVER (ORDER BY f.played_at, f.id ROWS UNBOUNDED PRECEDING) AS session_id
    FROM flagged f
),
islands AS (
    SELECT min(g.played_at) AS started_at,
           max(g.played_at + make_interval(secs => g.ms_played / 1000.0)) AS ended_at,
           count(*)::bigint AS tracks,
           coalesce(sum(g.ms_played), 0)::bigint AS ms,
           array_remove(array_agg(g.track_id ORDER BY g.played_at, g.id), NULL::text) AS track_ids
    FROM grouped g
    GROUP BY g.session_id
)
SELECT started_at, ended_at, tracks, ms, track_ids
FROM islands
ORDER BY (ended_at - started_at) DESC, ms DESC
LIMIT $5`, rangeFilter("l", "$1", "$2", "$3"))

// LongestSessions returns the longest uninterrupted listening sessions in the
// range, longest first.
//
// gap is how much silence ends a session; zero or less means domain.SessionGap.
// Timestamps stay absolute because a session is a run of playback, not a calendar
// object, so no timezone is involved.
func (s *Service) LongestSessions(ctx context.Context, q store.Querier, userID uuid.UUID, r domain.TimeRange, gap time.Duration, limit int) ([]domain.ListeningSession, error) {
	if err := checkScope(userID, r); err != nil {
		return nil, err
	}
	if gap <= 0 {
		gap = domain.SessionGap
	}
	limit = clampLimit(limit)

	rows, err := q.Query(ctx, longestSessionsSQL,
		store.UUIDArg(userID), r.From.UTC(), r.To.UTC(), gap.Seconds(), limit)
	if err != nil {
		return nil, postgres.Classify("longest listening sessions", err)
	}
	defer rows.Close()

	var out []domain.ListeningSession
	for rows.Next() {
		var (
			sess   domain.ListeningSession
			tracks int64
		)
		if err := rows.Scan(&sess.StartedAt, &sess.EndedAt, &tracks, &sess.MsPlayed, &sess.TrackIDs); err != nil {
			return nil, postgres.Classify("scan listening session", err)
		}
		sess.TrackCount = int(tracks)
		out = append(out, sess)
	}
	if err := rows.Err(); err != nil {
		return nil, postgres.Classify("longest listening sessions", err)
	}
	return out, nil
}
