package stats

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/requi/encore/internal/domain"
	"github.com/requi/encore/internal/postgres"
	"github.com/requi/encore/internal/store"
)

// TimelinePoint is one bucket of a timeline. Buckets are contiguous: a period
// with no listening appears as zeroes, never as a missing point, so a chart never
// has to guess whether a gap means silence or missing data.
type TimelinePoint struct {
	Bucket          time.Time
	Plays           int64
	MsPlayed        int64
	DistinctTracks  int64
	DistinctArtists int64
}

// checkInterval rejects an interval that would produce more points than the API
// is willing to return. domain.SuggestInterval exists so callers can pick a width
// that passes this check for any range.
func checkInterval(r domain.TimeRange, i domain.Interval) error {
	if !i.Valid() {
		return fmt.Errorf("%w: unknown interval %q", domain.ErrValidation, i)
	}
	buckets := int64(r.Duration()/i.Approx()) + 1
	if buckets > domain.MaxTimelineBuckets {
		return fmt.Errorf("%w: interval %q over this range would produce about %d buckets, more than the maximum of %d",
			domain.ErrValidation, i, buckets, domain.MaxTimelineBuckets)
	}
	return nil
}

// timelineSQL composes the bucketed aggregate.
//
// generate_series over the local bounds is what makes empty buckets appear as
// zeroes: the aggregates are LEFT JOINed onto the full series rather than the
// series being derived from the data. The series stops one microsecond short of
// the upper bound so that a range ending exactly on a boundary does not emit a
// trailing bucket that is outside the half-open range.
//
// Distinct artists are counted in their own CTE because joining track_artists
// into the main aggregate would multiply a listen by its number of credited
// artists and inflate both the play count and the listening time.
//
// pred is empty for the whole library, or an extra predicate restricting the
// listens to one entity, in which case it reads its id from $6.
func timelineSQL(pred string) string {
	filter := rangeFilter("l", "$1", "$2", "$3")
	if pred != "" {
		filter += " AND " + pred
	}
	return fmt.Sprintf(`
WITH bounds AS (
    SELECT date_trunc($5::text, ($2::timestamptz AT TIME ZONE $4::text)) AS lo,
           ($3::timestamptz AT TIME ZONE $4::text) AS hi
),
buckets AS (
    SELECT generate_series(b.lo, b.hi - interval '1 microsecond', ('1 ' || $5::text)::interval) AS bucket
    FROM bounds b
),
agg AS (
    SELECT date_trunc($5::text, (l.played_at AT TIME ZONE $4::text)) AS bucket,
           count(*)::bigint AS plays,
           coalesce(sum(l.ms_played), 0)::bigint AS ms,
           count(DISTINCT l.track_id)::bigint AS tracks
    FROM listens l
    WHERE %[1]s
    GROUP BY 1
),
art AS (
    SELECT date_trunc($5::text, (l.played_at AT TIME ZONE $4::text)) AS bucket,
           count(DISTINCT ta.artist_id)::bigint AS artists
    FROM listens l
    JOIN track_artists ta ON ta.track_id = l.track_id
    WHERE %[1]s
    GROUP BY 1
)
SELECT b.bucket,
       coalesce(a.plays, 0)::bigint,
       coalesce(a.ms, 0)::bigint,
       coalesce(a.tracks, 0)::bigint,
       coalesce(x.artists, 0)::bigint
FROM buckets b
LEFT JOIN agg a ON a.bucket = b.bucket
LEFT JOIN art x ON x.bucket = b.bucket
ORDER BY b.bucket`, filter)
}

var libraryTimelineSQL = timelineSQL("")

// Timeline buckets the range by interval in the user's local time.
func (s *Service) Timeline(ctx context.Context, q store.Querier, userID uuid.UUID, r domain.TimeRange, tz string, interval domain.Interval) ([]TimelinePoint, error) {
	loc, err := scope(userID, r, tz)
	if err != nil {
		return nil, err
	}
	if err := checkInterval(r, interval); err != nil {
		return nil, err
	}
	return s.timeline(ctx, q, libraryTimelineSQL, loc,
		store.UUIDArg(userID), r.From.UTC(), r.To.UTC(), tzArg(tz), string(interval))
}

// timeline runs a statement built by timelineSQL and scans its rows.
func (s *Service) timeline(ctx context.Context, q store.Querier, sql string, loc *time.Location, args ...any) ([]TimelinePoint, error) {
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, postgres.Classify("listening timeline", err)
	}
	defer rows.Close()

	var out []TimelinePoint
	for rows.Next() {
		var p TimelinePoint
		if err := rows.Scan(&p.Bucket, &p.Plays, &p.MsPlayed, &p.DistinctTracks, &p.DistinctArtists); err != nil {
			return nil, postgres.Classify("scan timeline bucket", err)
		}
		p.Bucket = inLocation(p.Bucket, loc)
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, postgres.Classify("listening timeline", err)
	}
	return out, nil
}
