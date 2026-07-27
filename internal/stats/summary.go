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

// Summary is the dashboard headline for one range.
type Summary struct {
	Listens         int64
	DistinctTracks  int64
	DistinctArtists int64
	DistinctAlbums  int64
	MsPlayed        int64
	FirstListenAt   *time.Time
	LastListenAt    *time.Time
	ActiveDays      int64
}

// Minutes is the listening time in whole minutes, which is the unit the UI shows.
func (s Summary) Minutes() int64 { return s.MsPlayed / 60000 }

// Hours is the listening time in fractional hours.
func (s Summary) Hours() float64 { return float64(s.MsPlayed) / 3600000 }

// summarySQL computes every headline number from one pass over the range.
//
// The base CTE is referenced several times, which makes Postgres materialise it,
// so the fact table is scanned once and the artist and album counts are answered
// from that result rather than from three more index scans. The whole statement
// is a bare SELECT of scalar subqueries, so it returns exactly one row even for a
// user with no listening at all.
var summarySQL = fmt.Sprintf(`
WITH base AS (
    SELECT l.track_id, l.ms_played, l.played_at
    FROM listens l
    WHERE %s
)
SELECT
    (SELECT count(*) FROM base)::bigint,
    (SELECT count(DISTINCT track_id) FROM base)::bigint,
    (SELECT count(DISTINCT bta.artist_id)
       FROM base s JOIN track_artists bta ON bta.track_id = s.track_id)::bigint,
    (SELECT count(DISTINCT t.album_id)
       FROM base s JOIN tracks t ON t.id = s.track_id
      WHERE t.album_id IS NOT NULL)::bigint,
    (SELECT coalesce(sum(ms_played), 0) FROM base)::bigint,
    (SELECT min(played_at) FROM base),
    (SELECT max(played_at) FROM base),
    (SELECT count(DISTINCT (played_at AT TIME ZONE $4::text)::date) FROM base)::bigint`,
	rangeFilter("l", "$1", "$2", "$3"))

// Summary aggregates a range into the numbers the dashboard header shows.
func (s *Service) Summary(ctx context.Context, q store.Querier, userID uuid.UUID, r domain.TimeRange, tz string) (Summary, error) {
	loc, err := scope(userID, r, tz)
	if err != nil {
		return Summary{}, err
	}

	var out Summary
	err = q.QueryRow(ctx, summarySQL,
		store.UUIDArg(userID), r.From.UTC(), r.To.UTC(), tzArg(tz),
	).Scan(
		&out.Listens, &out.DistinctTracks, &out.DistinctArtists, &out.DistinctAlbums,
		&out.MsPlayed, &out.FirstListenAt, &out.LastListenAt, &out.ActiveDays,
	)
	if err != nil {
		return Summary{}, postgres.Classify("listening summary", err)
	}

	out.FirstListenAt = toLocation(out.FirstListenAt, loc)
	out.LastListenAt = toLocation(out.LastListenAt, loc)
	return out, nil
}
