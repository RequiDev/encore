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

// Coverage is the denominator every partial statistic carries.
//
// Both bodies of data this file and context.go read are incomplete by nature —
// genres exist only where enrichment has resolved the artist — so a bare
// percentage would be a plausible-looking lie. Covered and Total travel with the
// value all the way to the page, which states them in words.
type Coverage struct {
	Covered int64
	Total   int64
}

// Genre is one row of the ranked list.
type Genre struct {
	Genre    string
	Plays    int64
	MsPlayed int64
}

// GenrePage is one page of the ranking, the length of the whole list, and how
// much of the range the ranking could see.
type GenrePage struct {
	Genres   []Genre
	Total    int64
	Coverage Coverage
}

// trackGenreCTE maps every catalogue track to the distinct genres of its
// credited artists.
//
// The DISTINCT is the counting rule: genres belong to artists, a track may
// credit several, and a track whose two credited artists are both tagged
// "indie rock" must contribute one play to it rather than two. Deduplicating
// here rather than per listen is equivalent and far cheaper, because every play
// of a track has the same genre set by construction.
const trackGenreCTE = `
track_genre AS (
    SELECT DISTINCT ta.track_id, g.genre
    FROM track_artists ta
    JOIN artists a ON a.id = ta.artist_id
    CROSS JOIN LATERAL unnest(a.genres) AS g(genre)
)`

// Parameters are $1 user, $2 from, $3 to, $4 limit, $5 offset.
var topGenresFactSQL = fmt.Sprintf(`
WITH %s,
agg AS (
    SELECT tg.genre,
           count(*)::bigint                      AS plays,
           coalesce(sum(l.ms_played), 0)::bigint AS ms
    FROM listens l
    JOIN track_genre tg ON tg.track_id = l.track_id
    WHERE %s
    GROUP BY tg.genre
),
total AS (SELECT count(*)::bigint AS n FROM agg)
SELECT t.n, a.genre, a.plays, a.ms
FROM total t
LEFT JOIN (
    SELECT genre, plays, ms FROM agg ORDER BY plays DESC, ms DESC, genre LIMIT $4 OFFSET $5
) a ON true
ORDER BY a.plays DESC NULLS LAST, a.ms DESC, a.genre`,
	trackGenreCTE, rangeFilter("l", "$1", "$2", "$3"))

// The rollup variant reads whole local days and therefore needs the timezone.
// Parameters are $1 user, $2 from, $3 to, $4 limit, $5 offset, $6 timezone.
var topGenresRollupSQL = fmt.Sprintf(`
WITH %s,
agg AS (
    SELECT tg.genre,
           sum(r.plays)::bigint             AS plays,
           coalesce(sum(r.ms), 0)::bigint   AS ms
    FROM listen_daily_rollup r
    JOIN track_genre tg ON tg.track_id = r.track_id
    WHERE r.user_id = $1
      AND r.day >= (($2::timestamptz AT TIME ZONE $6::text)::date)
      AND r.day <  (($3::timestamptz AT TIME ZONE $6::text)::date)
      AND %s
    GROUP BY tg.genre
),
total AS (SELECT count(*)::bigint AS n FROM agg)
SELECT t.n, a.genre, a.plays, a.ms
FROM total t
LEFT JOIN (
    SELECT genre, plays, ms FROM agg ORDER BY plays DESC, ms DESC, genre LIMIT $4 OFFSET $5
) a ON true
ORDER BY a.plays DESC NULLS LAST, a.ms DESC, a.genre`,
	trackGenreCTE, blacklistFilter("r"))

// genreCoverageSQL counts how many in-range listens resolve to at least one
// artist carrying a genre.
//
// A LEFT JOIN onto the distinct set of genred tracks, rather than an EXISTS per
// row, so the planner sees one hash join instead of a correlated subquery.
// Parameters are $1 user, $2 from, $3 to.
var genreCoverageSQL = fmt.Sprintf(`
WITH genred_track AS (
    SELECT DISTINCT ta.track_id
    FROM track_artists ta
    JOIN artists a ON a.id = ta.artist_id
    WHERE cardinality(a.genres) > 0
)
SELECT count(*)::bigint, count(gt.track_id)::bigint
FROM listens l
LEFT JOIN genred_track gt ON gt.track_id = l.track_id
WHERE %s`, rangeFilter("l", "$1", "$2", "$3"))

// TopGenres ranks the genres of the artists behind the range's listening.
//
// Genre plays sum to more than total plays, because a track counts toward each
// of its genres. That is stated on the page rather than normalised away: dividing
// a play across its genres produces fractional counts nobody can reason about.
func (s *Service) TopGenres(
	ctx context.Context,
	q store.Querier,
	userID uuid.UUID,
	r domain.TimeRange,
	tz string,
	limit, offset int,
) (GenrePage, error) {
	loc, err := scope(userID, r, tz)
	if err != nil {
		return GenrePage{}, err
	}
	limit, offset = clampLimit(limit), clampOffset(offset)

	// rollupEligible is checked first because it costs nothing to evaluate: for
	// the common case of a narrow range it is false, and the dirty-day query,
	// which can only ever pull the answer back toward the fact table, would not
	// change that. See top() in top.go for the same gate.
	rollup := false
	if rollupEligible(r, loc) {
		dirty, err := s.HasDirtyDays(ctx, q, userID, r, tz)
		if err != nil {
			return GenrePage{}, err
		}
		rollup = useRollup(r, loc, dirty)
	}

	var (
		sql  = topGenresFactSQL
		args = []any{store.UUIDArg(userID), r.From.UTC(), r.To.UTC(), limit, offset}
	)
	if rollup {
		sql = topGenresRollupSQL
		args = append(args, tzArg(tz))
	}

	page := GenrePage{Genres: make([]Genre, 0, limit)}
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return GenrePage{}, postgres.Classify("top genres", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			g     Genre
			name  *string
			plays *int64
			ms    *int64
		)
		if err := rows.Scan(&page.Total, &name, &plays, &ms); err != nil {
			return GenrePage{}, postgres.Classify("scan top genres", err)
		}
		// A page beyond the end of the list still reports the list's length, so
		// the row carries a NULL genre rather than not existing.
		if name == nil {
			continue
		}
		g.Genre, g.Plays, g.MsPlayed = *name, *plays, *ms
		page.Genres = append(page.Genres, g)
	}
	if err := rows.Err(); err != nil {
		return GenrePage{}, postgres.Classify("top genres", err)
	}

	if err := q.QueryRow(ctx, genreCoverageSQL,
		store.UUIDArg(userID), r.From.UTC(), r.To.UTC(),
	).Scan(&page.Coverage.Total, &page.Coverage.Covered); err != nil {
		return GenrePage{}, postgres.Classify("genre coverage", err)
	}
	return page, nil
}

// GenrePoint is one bucket of one genre's share of a timeline.
type GenrePoint struct {
	Bucket   time.Time
	Genre    string
	Plays    int64
	MsPlayed int64
}

// genreTimelineSQL buckets the requested genres over the range.
//
// The series are cross-joined from the caller's list rather than derived from
// the data, for the same reason the bucket grid is generated rather than
// grouped: a genre with no plays in a bucket must appear as a zero. Passing the
// list in also keeps a chart's series stable while the ranking beneath it is
// paged or re-ranged.
//
// Parameters are $1 user, $2 from, $3 to, $4 timezone, $5 interval, $6 genres.
var genreTimelineSQL = fmt.Sprintf(`
WITH %s,
bounds AS (
    SELECT date_trunc($5::text, ($2::timestamptz AT TIME ZONE $4::text)) AS lo,
           ($3::timestamptz AT TIME ZONE $4::text) AS hi
),
buckets AS (
    SELECT generate_series(b.lo, b.hi - interval '1 microsecond', ('1 ' || $5::text)::interval) AS bucket
    FROM bounds b
),
series AS (SELECT g.genre FROM unnest($6::text[]) AS g(genre)),
agg AS (
    SELECT date_trunc($5::text, (l.played_at AT TIME ZONE $4::text)) AS bucket,
           tg.genre,
           count(*)::bigint                      AS plays,
           coalesce(sum(l.ms_played), 0)::bigint AS ms
    FROM listens l
    JOIN track_genre tg ON tg.track_id = l.track_id
    WHERE %s AND tg.genre = ANY($6::text[])
    GROUP BY 1, 2
)
SELECT b.bucket, s.genre, coalesce(a.plays, 0)::bigint, coalesce(a.ms, 0)::bigint
FROM buckets b
CROSS JOIN series s
LEFT JOIN agg a ON a.bucket = b.bucket AND a.genre = s.genre
ORDER BY b.bucket, s.genre`,
	trackGenreCTE, rangeFilter("l", "$1", "$2", "$3"))

// GenreTimeline buckets the named genres across the range, so taste drift is
// visible rather than inferred from two rankings side by side.
//
// The caller names the genres. The page passes the range's top eight, which is
// where a stacked area chart stops being readable.
func (s *Service) GenreTimeline(
	ctx context.Context,
	q store.Querier,
	userID uuid.UUID,
	r domain.TimeRange,
	tz string,
	interval domain.Interval,
	genres []string,
) ([]GenrePoint, error) {
	loc, err := scope(userID, r, tz)
	if err != nil {
		return nil, err
	}
	if err := checkInterval(r, interval); err != nil {
		return nil, err
	}
	if len(genres) == 0 {
		return nil, nil
	}

	rows, err := q.Query(ctx, genreTimelineSQL,
		store.UUIDArg(userID), r.From.UTC(), r.To.UTC(), tzArg(tz), string(interval), genres)
	if err != nil {
		return nil, postgres.Classify("genre timeline", err)
	}
	defer rows.Close()

	out := make([]GenrePoint, 0, len(genres)*16)
	for rows.Next() {
		var p GenrePoint
		if err := rows.Scan(&p.Bucket, &p.Genre, &p.Plays, &p.MsPlayed); err != nil {
			return nil, postgres.Classify("scan genre timeline", err)
		}
		// date_trunc returns a bucket boundary as a wall-clock timestamp with no
		// zone; inLocation reattaches the user's, which is what makes the point
		// mean the local midnight it was computed as.
		p.Bucket = inLocation(p.Bucket, loc)
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, postgres.Classify("genre timeline", err)
	}
	return out, nil
}
