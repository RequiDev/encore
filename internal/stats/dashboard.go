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

// ArtistCountPoint is the number of distinct artists heard in one bucket. It is
// not cumulative and buckets do not partition the artists between them: an artist
// heard in two months counts in both.
type ArtistCountPoint struct {
	Bucket  time.Time
	Artists int64
}

// ReleaseYearStats is the average release year of the music listened to in a
// range, weighted by plays, which is what makes it move when listening habits
// change rather than when the library grows.
type ReleaseYearStats struct {
	AverageYear    float64
	Listens        int64
	DistinctAlbums int64
}

// ArtistsPerTrackStats is the average number of credited artists per listened
// track, weighted by plays.
type ArtistsPerTrackStats struct {
	Average float64
	Listens int64
}

var differentArtistsSQL = fmt.Sprintf(`
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
           count(DISTINCT ta.artist_id)::bigint AS artists
    FROM listens l
    JOIN track_artists ta ON ta.track_id = l.track_id
    WHERE %s
    GROUP BY 1
)
SELECT b.bucket, coalesce(a.artists, 0)::bigint
FROM buckets b
LEFT JOIN agg a ON a.bucket = b.bucket
ORDER BY b.bucket`, rangeFilter("l", "$1", "$2", "$3"))

// Albums whose metadata has not resolved yet have no release date and are simply
// absent from the average; counting them as year zero would be a lie that moves
// with enrichment progress rather than with listening.
var releaseYearSQL = fmt.Sprintf(`
WITH rel AS (
    SELECT al.id AS album_id, extract(year FROM al.release_date)::int AS year
    FROM listens l
    JOIN tracks t ON t.id = l.track_id
    JOIN albums al ON al.id = t.album_id
    WHERE %s AND al.release_date IS NOT NULL
)
SELECT coalesce(avg(year), 0)::float8, count(*)::bigint, count(DISTINCT album_id)::bigint
FROM rel`, rangeFilter("l", "$1", "$2", "$3"))

// Tracks with no credited artists are tracks whose metadata has not resolved
// yet, so they are excluded rather than averaged in as zero.
var artistsPerTrackSQL = fmt.Sprintf(`
WITH counts AS (
    SELECT (SELECT count(*) FROM track_artists ta WHERE ta.track_id = l.track_id)::int AS artists
    FROM listens l
    WHERE %s AND l.track_id IS NOT NULL
)
SELECT coalesce(avg(artists) FILTER (WHERE artists > 0), 0)::float8,
       count(*) FILTER (WHERE artists > 0)::bigint
FROM counts`, rangeFilter("l", "$1", "$2", "$3"))

// DifferentArtistsPerPeriod counts how many distinct artists the user heard in
// each bucket of the range.
func (s *Service) DifferentArtistsPerPeriod(ctx context.Context, q store.Querier, userID uuid.UUID, r domain.TimeRange, tz string, interval domain.Interval) ([]ArtistCountPoint, error) {
	loc, err := scope(userID, r, tz)
	if err != nil {
		return nil, err
	}
	if err := checkInterval(r, interval); err != nil {
		return nil, err
	}

	rows, err := q.Query(ctx, differentArtistsSQL,
		store.UUIDArg(userID), r.From.UTC(), r.To.UTC(), tzArg(tz), string(interval))
	if err != nil {
		return nil, postgres.Classify("different artists per period", err)
	}
	defer rows.Close()

	var out []ArtistCountPoint
	for rows.Next() {
		var p ArtistCountPoint
		if err := rows.Scan(&p.Bucket, &p.Artists); err != nil {
			return nil, postgres.Classify("scan artist count bucket", err)
		}
		p.Bucket = inLocation(p.Bucket, loc)
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, postgres.Classify("different artists per period", err)
	}
	return out, nil
}

// AverageAlbumReleaseYear is the play-weighted average release year of the albums
// listened to in the range.
func (s *Service) AverageAlbumReleaseYear(ctx context.Context, q store.Querier, userID uuid.UUID, r domain.TimeRange) (ReleaseYearStats, error) {
	if err := checkScope(userID, r); err != nil {
		return ReleaseYearStats{}, err
	}
	var out ReleaseYearStats
	err := q.QueryRow(ctx, releaseYearSQL, store.UUIDArg(userID), r.From.UTC(), r.To.UTC()).
		Scan(&out.AverageYear, &out.Listens, &out.DistinctAlbums)
	if err != nil {
		return ReleaseYearStats{}, postgres.Classify("average album release year", err)
	}
	return out, nil
}

// AverageArtistsPerTrack is the play-weighted average number of artists credited
// on the tracks listened to in the range.
func (s *Service) AverageArtistsPerTrack(ctx context.Context, q store.Querier, userID uuid.UUID, r domain.TimeRange) (ArtistsPerTrackStats, error) {
	if err := checkScope(userID, r); err != nil {
		return ArtistsPerTrackStats{}, err
	}
	var out ArtistsPerTrackStats
	err := q.QueryRow(ctx, artistsPerTrackSQL, store.UUIDArg(userID), r.From.UTC(), r.To.UTC()).
		Scan(&out.Average, &out.Listens)
	if err != nil {
		return ArtistsPerTrackStats{}, postgres.Classify("average artists per track", err)
	}
	return out, nil
}
