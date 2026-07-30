package stats

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/RequiDev/encore/internal/domain"
	"github.com/RequiDev/encore/internal/postgres"
	"github.com/RequiDev/encore/internal/store"
)

// Taste is the pair of derived scores that describe *what kind* of catalogue
// somebody listens to, rather than which of it.
type Taste struct {
	// Obscurity is the play-weighted mean of Spotify's artist popularity, 0 to
	// 100. High means mainstream. It is named for the question people ask.
	Obscurity         float64
	ObscurityCoverage Coverage

	// ReleaseLagYears is the play-weighted mean gap between when an album came
	// out and when it was played.
	ReleaseLagYears    float64
	ReleaseLagCoverage Coverage
}

// obscuritySQL averages artist popularity over listens, weighting by play.
//
// Only artists enrichment has resolved contribute. popularity defaults to 0 for
// a pending row, and counting that as "not popular at all" would drag every
// freshly imported instance's score toward zero and call it a taste for the
// obscure. The denominator is therefore listens having at least one resolved
// credited artist, which is what the coverage half of the result reports.
//
// Parameters are $1 user, $2 from, $3 to.
var obscuritySQL = fmt.Sprintf(`
WITH scoped AS (
    SELECT l.id, l.track_id FROM listens l WHERE %s
),
weighted AS (
    SELECT s.id, avg(a.popularity)::float8 AS pop
    FROM scoped s
    JOIN track_artists ta ON ta.track_id = s.track_id
    JOIN artists a ON a.id = ta.artist_id AND a.metadata_state = 'resolved'
    GROUP BY s.id
)
SELECT coalesce(avg(w.pop), 0)::float8,
       (SELECT count(*) FROM scoped)::bigint,
       count(w.id)::bigint
FROM weighted w`, rangeFilter("l", "$1", "$2", "$3"))

// releaseLagSQL averages the gap between release and play.
//
// Parameters are $1 user, $2 from, $3 to, $4 timezone. The play year is read in
// the listener's own timezone, because a play at 00:30 on 1 January belongs to
// the year they were living in, not the one UTC was.
var releaseLagSQL = fmt.Sprintf(`
WITH scoped AS (
    SELECT l.id, l.track_id, l.played_at FROM listens l WHERE %s
),
lagged AS (
    SELECT s.id,
           (extract(year FROM (s.played_at AT TIME ZONE $4::text))
              - extract(year FROM al.release_date))::float8 AS lag
    FROM scoped s
    JOIN tracks t  ON t.id = s.track_id
    JOIN albums al ON al.id = t.album_id AND al.release_date IS NOT NULL
)
SELECT coalesce(avg(l2.lag), 0)::float8,
       (SELECT count(*) FROM scoped)::bigint,
       count(l2.id)::bigint
FROM lagged l2`, rangeFilter("l", "$1", "$2", "$3"))

// Taste computes both scores. They are one endpoint because they answer one
// question — what kind of catalogue this is — and neither is worth a page.
func (s *Service) Taste(
	ctx context.Context,
	q store.Querier,
	userID uuid.UUID,
	r domain.TimeRange,
	tz string,
) (Taste, error) {
	if _, err := scope(userID, r, tz); err != nil {
		return Taste{}, err
	}

	var out Taste
	if err := q.QueryRow(ctx, obscuritySQL,
		store.UUIDArg(userID), r.From.UTC(), r.To.UTC(),
	).Scan(&out.Obscurity, &out.ObscurityCoverage.Total, &out.ObscurityCoverage.Covered); err != nil {
		return Taste{}, postgres.Classify("obscurity score", err)
	}

	if err := q.QueryRow(ctx, releaseLagSQL,
		store.UUIDArg(userID), r.From.UTC(), r.To.UTC(), tzArg(tz),
	).Scan(&out.ReleaseLagYears, &out.ReleaseLagCoverage.Total, &out.ReleaseLagCoverage.Covered); err != nil {
		return Taste{}, postgres.Classify("release lag", err)
	}
	return out, nil
}
