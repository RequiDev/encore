package stats

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/requi/encore/internal/domain"
	"github.com/requi/encore/internal/postgres"
	"github.com/requi/encore/internal/store"
)

// HourBucket is one hour of the local day, 0 to 23.
type HourBucket struct {
	Hour     int
	Plays    int64
	MsPlayed int64
}

// WeekdayBucket is one day of the local week. Weekday is 0 for Monday through 6
// for Sunday, which is how the charts read left to right in Encore's UI, rather
// than Postgres's own Sunday-first numbering.
type WeekdayBucket struct {
	Weekday  int
	Plays    int64
	MsPlayed int64
}

// HeatmapCell is one weekday/hour cell of the repartition heatmap.
type HeatmapCell struct {
	Weekday  int
	Hour     int
	Plays    int64
	MsPlayed int64
}

// Every repartition returns a complete grid, empty cells included, so that the
// frontend can render a fixed layout without filling gaps itself.
var hourRepartitionSQL = fmt.Sprintf(`
WITH grid AS (SELECT generate_series(0, 23) AS hour),
agg AS (
    SELECT extract(hour FROM (l.played_at AT TIME ZONE $4::text))::int AS hour,
           count(*)::bigint AS plays,
           coalesce(sum(l.ms_played), 0)::bigint AS ms
    FROM listens l
    WHERE %s
    GROUP BY 1
)
SELECT g.hour, coalesce(a.plays, 0)::bigint, coalesce(a.ms, 0)::bigint
FROM grid g LEFT JOIN agg a ON a.hour = g.hour
ORDER BY g.hour`, rangeFilter("l", "$1", "$2", "$3"))

// isodow is 1 for Monday through 7 for Sunday, so subtracting one gives Encore's
// Monday-first numbering directly in the index expression.
var weekdayRepartitionSQL = fmt.Sprintf(`
WITH grid AS (SELECT generate_series(0, 6) AS weekday),
agg AS (
    SELECT extract(isodow FROM (l.played_at AT TIME ZONE $4::text))::int - 1 AS weekday,
           count(*)::bigint AS plays,
           coalesce(sum(l.ms_played), 0)::bigint AS ms
    FROM listens l
    WHERE %s
    GROUP BY 1
)
SELECT g.weekday, coalesce(a.plays, 0)::bigint, coalesce(a.ms, 0)::bigint
FROM grid g LEFT JOIN agg a ON a.weekday = g.weekday
ORDER BY g.weekday`, rangeFilter("l", "$1", "$2", "$3"))

var heatmapSQL = fmt.Sprintf(`
WITH grid AS (
    SELECT d.weekday, h.hour
    FROM generate_series(0, 6) AS d(weekday)
    CROSS JOIN generate_series(0, 23) AS h(hour)
),
agg AS (
    SELECT extract(isodow FROM (l.played_at AT TIME ZONE $4::text))::int - 1 AS weekday,
           extract(hour FROM (l.played_at AT TIME ZONE $4::text))::int AS hour,
           count(*)::bigint AS plays,
           coalesce(sum(l.ms_played), 0)::bigint AS ms
    FROM listens l
    WHERE %s
    GROUP BY 1, 2
)
SELECT g.weekday, g.hour, coalesce(a.plays, 0)::bigint, coalesce(a.ms, 0)::bigint
FROM grid g LEFT JOIN agg a ON a.weekday = g.weekday AND a.hour = g.hour
ORDER BY g.weekday, g.hour`, rangeFilter("l", "$1", "$2", "$3"))

// HourRepartition splits the range across the 24 hours of the local day. It
// always returns 24 buckets.
func (s *Service) HourRepartition(ctx context.Context, q store.Querier, userID uuid.UUID, r domain.TimeRange, tz string) ([]HourBucket, error) {
	if _, err := scope(userID, r, tz); err != nil {
		return nil, err
	}
	rows, err := q.Query(ctx, hourRepartitionSQL,
		store.UUIDArg(userID), r.From.UTC(), r.To.UTC(), tzArg(tz))
	if err != nil {
		return nil, postgres.Classify("hour repartition", err)
	}
	defer rows.Close()

	out := make([]HourBucket, 0, 24)
	for rows.Next() {
		var b HourBucket
		if err := rows.Scan(&b.Hour, &b.Plays, &b.MsPlayed); err != nil {
			return nil, postgres.Classify("scan hour repartition", err)
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, postgres.Classify("hour repartition", err)
	}
	return out, nil
}

// WeekdayRepartition splits the range across the seven local weekdays, Monday
// first. It always returns 7 buckets.
func (s *Service) WeekdayRepartition(ctx context.Context, q store.Querier, userID uuid.UUID, r domain.TimeRange, tz string) ([]WeekdayBucket, error) {
	if _, err := scope(userID, r, tz); err != nil {
		return nil, err
	}
	rows, err := q.Query(ctx, weekdayRepartitionSQL,
		store.UUIDArg(userID), r.From.UTC(), r.To.UTC(), tzArg(tz))
	if err != nil {
		return nil, postgres.Classify("weekday repartition", err)
	}
	defer rows.Close()

	out := make([]WeekdayBucket, 0, 7)
	for rows.Next() {
		var b WeekdayBucket
		if err := rows.Scan(&b.Weekday, &b.Plays, &b.MsPlayed); err != nil {
			return nil, postgres.Classify("scan weekday repartition", err)
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, postgres.Classify("weekday repartition", err)
	}
	return out, nil
}

// HourWeekdayHeatmap crosses the two repartitions. It always returns 168 cells,
// ordered Monday 00:00 first.
func (s *Service) HourWeekdayHeatmap(ctx context.Context, q store.Querier, userID uuid.UUID, r domain.TimeRange, tz string) ([]HeatmapCell, error) {
	if _, err := scope(userID, r, tz); err != nil {
		return nil, err
	}
	rows, err := q.Query(ctx, heatmapSQL,
		store.UUIDArg(userID), r.From.UTC(), r.To.UTC(), tzArg(tz))
	if err != nil {
		return nil, postgres.Classify("hour/weekday heatmap", err)
	}
	defer rows.Close()

	out := make([]HeatmapCell, 0, 7*24)
	for rows.Next() {
		var c HeatmapCell
		if err := rows.Scan(&c.Weekday, &c.Hour, &c.Plays, &c.MsPlayed); err != nil {
			return nil, postgres.Classify("scan heatmap cell", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, postgres.Classify("hour/weekday heatmap", err)
	}
	return out, nil
}
