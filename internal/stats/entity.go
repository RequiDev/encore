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

// EntityTopLimit is the default size of the sub-lists on a detail page.
const EntityTopLimit = 10

// EntryCount is a plain ranked count, used where a rank delta has no meaning:
// the top tracks *of an artist* are not a list the user's own history has ever
// ranked, so there is no previous position to compare against.
type EntryCount struct {
	ID       string
	Plays    int64
	MsPlayed int64
}

// EntityStats is what every detail page shows about one track, artist or album.
//
// Everything is scoped to the requested range except DiscoveredAt, which answers
// "when did I first hear this at all" and would be meaningless if it were
// re-derived from a window the caller happened to ask for.
type EntityStats struct {
	ID            string
	Plays         int64
	MsPlayed      int64
	FirstListenAt *time.Time
	LastListenAt  *time.Time
	DiscoveredAt  *time.Time
	PlayShare     float64
	MsShare       float64
	Daily         []TimelinePoint
}

// TrackStats is the track detail page.
type TrackStats struct {
	EntityStats
}

// ArtistStats is the artist detail page: the shared statistics plus the artist's
// own top tracks and albums within the range.
type ArtistStats struct {
	EntityStats
	TopTracks []EntryCount
	TopAlbums []EntryCount
}

// AlbumStats is the album detail page.
type AlbumStats struct {
	EntityStats
	TopTracks []EntryCount
}

// entityKind selects which detail page a query serves.
type entityKind int

const (
	entityTrack entityKind = iota
	entityArtist
	entityAlbum
)

func (k entityKind) String() string {
	switch k {
	case entityArtist:
		return "artist statistics"
	case entityAlbum:
		return "album statistics"
	default:
		return "track statistics"
	}
}

// entityFilter restricts a listen to one entity. Artists and albums are reached
// with EXISTS rather than a join so that the predicate cannot duplicate a listen
// that credits the same artist twice.
func entityFilter(kind entityKind, idArg string) string {
	switch kind {
	case entityArtist:
		return fmt.Sprintf("EXISTS (SELECT 1 FROM track_artists eta WHERE eta.track_id = l.track_id AND eta.artist_id = %s)", idArg)
	case entityAlbum:
		return fmt.Sprintf("EXISTS (SELECT 1 FROM tracks et WHERE et.id = l.track_id AND et.album_id = %s)", idArg)
	default:
		return fmt.Sprintf("l.track_id = %s", idArg)
	}
}

// entityStatsSQL answers the entity's own totals, the user's totals for the same
// range (so the share can be computed without a second round trip) and the
// all-time first listen, in one statement whose result is always one row.
func entityStatsSQL(kind entityKind) string {
	return fmt.Sprintf(`
WITH base AS (
    SELECT l.ms_played, l.played_at
    FROM listens l
    WHERE %[1]s AND %[2]s
),
tot AS (
    SELECT l.ms_played FROM listens l WHERE %[1]s
),
ever AS (
    SELECT min(l.played_at) AS first_at
    FROM listens l
    WHERE l.user_id = $1 AND %[3]s AND %[2]s
)
SELECT
    (SELECT count(*) FROM base)::bigint,
    (SELECT coalesce(sum(ms_played), 0) FROM base)::bigint,
    (SELECT min(played_at) FROM base),
    (SELECT max(played_at) FROM base),
    (SELECT count(*) FROM tot)::bigint,
    (SELECT coalesce(sum(ms_played), 0) FROM tot)::bigint,
    (SELECT first_at FROM ever)`,
		rangeFilter("l", "$1", "$2", "$3"), entityFilter(kind, "$4"), blacklistFilter("l"))
}

var (
	trackStatsSQL  = entityStatsSQL(entityTrack)
	artistStatsSQL = entityStatsSQL(entityArtist)
	albumStatsSQL  = entityStatsSQL(entityAlbum)

	trackTimelineSQL  = timelineSQL(entityFilter(entityTrack, "$6"))
	artistTimelineSQL = timelineSQL(entityFilter(entityArtist, "$6"))
	albumTimelineSQL  = timelineSQL(entityFilter(entityAlbum, "$6"))

	artistTopTracksSQL = fmt.Sprintf(`
SELECT l.track_id AS id, count(*)::bigint AS plays, coalesce(sum(l.ms_played), 0)::bigint AS ms
FROM listens l
JOIN track_artists eta ON eta.track_id = l.track_id AND eta.artist_id = $4
WHERE %s AND l.track_id IS NOT NULL
GROUP BY 1
ORDER BY plays DESC, ms DESC, id
LIMIT $5`, rangeFilter("l", "$1", "$2", "$3"))

	artistTopAlbumsSQL = fmt.Sprintf(`
SELECT t.album_id AS id, count(*)::bigint AS plays, coalesce(sum(l.ms_played), 0)::bigint AS ms
FROM listens l
JOIN tracks t ON t.id = l.track_id
JOIN track_artists eta ON eta.track_id = l.track_id AND eta.artist_id = $4
WHERE %s AND t.album_id IS NOT NULL
GROUP BY 1
ORDER BY plays DESC, ms DESC, id
LIMIT $5`, rangeFilter("l", "$1", "$2", "$3"))

	albumTopTracksSQL = fmt.Sprintf(`
SELECT l.track_id AS id, count(*)::bigint AS plays, coalesce(sum(l.ms_played), 0)::bigint AS ms
FROM listens l
JOIN tracks t ON t.id = l.track_id AND t.album_id = $4
WHERE %s AND l.track_id IS NOT NULL
GROUP BY 1
ORDER BY plays DESC, ms DESC, id
LIMIT $5`, rangeFilter("l", "$1", "$2", "$3"))
)

// dailyInterval is the width of a detail page's own timeline: one day, unless the
// range is wide enough that a daily series would blow the bucket cap, in which
// case the finest interval that does fit is used. Degrading the resolution is
// better than refusing to draw the page.
func dailyInterval(r domain.TimeRange) domain.Interval {
	if checkInterval(r, domain.IntervalDay) == nil {
		return domain.IntervalDay
	}
	return domain.SuggestInterval(r)
}

// TrackStats is the track detail page for one Spotify track id.
func (s *Service) TrackStats(ctx context.Context, q store.Querier, userID uuid.UUID, r domain.TimeRange, tz string, trackID string) (TrackStats, error) {
	base, err := s.entityStats(ctx, q, entityTrack, userID, r, tz, trackID, trackStatsSQL, trackTimelineSQL)
	if err != nil {
		return TrackStats{}, err
	}
	return TrackStats{EntityStats: base}, nil
}

// ArtistStats is the artist detail page, including the artist's top tracks and
// albums in the range and the share of the user's listening they account for.
func (s *Service) ArtistStats(ctx context.Context, q store.Querier, userID uuid.UUID, r domain.TimeRange, tz string, artistID string, topLimit int) (ArtistStats, error) {
	base, err := s.entityStats(ctx, q, entityArtist, userID, r, tz, artistID, artistStatsSQL, artistTimelineSQL)
	if err != nil {
		return ArtistStats{}, err
	}
	out := ArtistStats{EntityStats: base}
	if out.TopTracks, err = s.entityTop(ctx, q, artistTopTracksSQL, "artist top tracks", userID, r, artistID, topLimit); err != nil {
		return ArtistStats{}, err
	}
	if out.TopAlbums, err = s.entityTop(ctx, q, artistTopAlbumsSQL, "artist top albums", userID, r, artistID, topLimit); err != nil {
		return ArtistStats{}, err
	}
	return out, nil
}

// AlbumStats is the album detail page, including the album's most played tracks
// and the share of the user's listening it accounts for.
func (s *Service) AlbumStats(ctx context.Context, q store.Querier, userID uuid.UUID, r domain.TimeRange, tz string, albumID string, topLimit int) (AlbumStats, error) {
	base, err := s.entityStats(ctx, q, entityAlbum, userID, r, tz, albumID, albumStatsSQL, albumTimelineSQL)
	if err != nil {
		return AlbumStats{}, err
	}
	out := AlbumStats{EntityStats: base}
	if out.TopTracks, err = s.entityTop(ctx, q, albumTopTracksSQL, "album top tracks", userID, r, albumID, topLimit); err != nil {
		return AlbumStats{}, err
	}
	return out, nil
}

func (s *Service) entityStats(ctx context.Context, q store.Querier, kind entityKind, userID uuid.UUID, r domain.TimeRange, tz, id, statsSQL, tlSQL string) (EntityStats, error) {
	loc, err := scope(userID, r, tz)
	if err != nil {
		return EntityStats{}, err
	}
	if id == "" {
		return EntityStats{}, fmt.Errorf("%w: an id is required", domain.ErrValidation)
	}

	out := EntityStats{ID: id}
	var totalPlays, totalMs int64
	err = q.QueryRow(ctx, statsSQL,
		store.UUIDArg(userID), r.From.UTC(), r.To.UTC(), id,
	).Scan(&out.Plays, &out.MsPlayed, &out.FirstListenAt, &out.LastListenAt,
		&totalPlays, &totalMs, &out.DiscoveredAt)
	if err != nil {
		return EntityStats{}, postgres.Classify(kind.String(), err)
	}
	out.FirstListenAt = toLocation(out.FirstListenAt, loc)
	out.LastListenAt = toLocation(out.LastListenAt, loc)
	out.DiscoveredAt = toLocation(out.DiscoveredAt, loc)
	out.PlayShare = share(out.Plays, totalPlays)
	out.MsShare = share(out.MsPlayed, totalMs)

	out.Daily, err = s.timeline(ctx, q, tlSQL, loc,
		store.UUIDArg(userID), r.From.UTC(), r.To.UTC(), tzArg(tz), string(dailyInterval(r)), id)
	if err != nil {
		return EntityStats{}, err
	}
	return out, nil
}

func (s *Service) entityTop(ctx context.Context, q store.Querier, sql, op string, userID uuid.UUID, r domain.TimeRange, id string, limit int) ([]EntryCount, error) {
	if limit <= 0 {
		limit = EntityTopLimit
	} else if limit > MaxPageSize {
		limit = MaxPageSize
	}

	rows, err := q.Query(ctx, sql, store.UUIDArg(userID), r.From.UTC(), r.To.UTC(), id, limit)
	if err != nil {
		return nil, postgres.Classify(op, err)
	}
	defer rows.Close()

	var out []EntryCount
	for rows.Next() {
		var e EntryCount
		if err := rows.Scan(&e.ID, &e.Plays, &e.MsPlayed); err != nil {
			return nil, postgres.Classify("scan "+op, err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, postgres.Classify(op, err)
	}
	return out, nil
}
