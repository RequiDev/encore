package stats

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/requi/encore/internal/domain"
	"github.com/requi/encore/internal/postgres"
	"github.com/requi/encore/internal/store"
)

// TopEntry is one row of a ranked list. PreviousRank is the place the same entity
// held in the immediately preceding equal-length period, which is what lets the
// UI draw "up 3" or "new" beside an entry; it is zero when the entity was not
// listened to at all in that period.
type TopEntry struct {
	ID           string
	Plays        int64
	MsPlayed     int64
	Rank         int
	PreviousRank int
}

// IsNew reports whether the entry was absent from the preceding period.
func (e TopEntry) IsNew() bool { return e.PreviousRank == 0 }

// Movement is how many places the entry has gained since the preceding period.
// It is negative for a fall and zero for a new entry, which has nothing to move
// from.
func (e TopEntry) Movement() int {
	if e.PreviousRank == 0 {
		return 0
	}
	return e.PreviousRank - e.Rank
}

// TopPage is one page of a ranked list together with the size of the whole list,
// so the frontend can paginate without a second call.
type TopPage struct {
	Entries []TopEntry
	Total   int64
}

// topKind selects which entity a ranked list is about.
type topKind int

const (
	topTracks topKind = iota
	topArtists
	topAlbums
)

func (k topKind) String() string {
	switch k {
	case topArtists:
		return "top artists"
	case topAlbums:
		return "top albums"
	default:
		return "top tracks"
	}
}

// topSourceSQL builds the aggregate a ranked list is computed from: one row per
// entity with its play count and listening time.
//
// A track carries every artist credited on it, so a collaboration counts a play
// for each of its artists; that is what "top artists" means to a listener.
//
// The rollup variant reads whole local days out of listen_daily_rollup and
// therefore needs the timezone; the fact variant does not, which is why the
// timezone placeholder is passed in rather than fixed. Postgres refuses a
// statement with a gap in its parameter numbering, so an unused placeholder is
// not an option.
func topSourceSQL(kind topKind, userArg, fromArg, toArg, tzArg string, rollup bool) string {
	if rollup {
		day := func(arg string) string {
			return fmt.Sprintf("((%s::timestamptz AT TIME ZONE %s::text)::date)", arg, tzArg)
		}
		where := fmt.Sprintf("r.user_id = %s AND r.day >= %s AND r.day < %s AND %s",
			userArg, day(fromArg), day(toArg), blacklistFilter("r"))
		switch kind {
		case topArtists:
			return fmt.Sprintf(`SELECT bta2.artist_id AS id, sum(r.plays)::bigint AS plays, sum(r.ms)::bigint AS ms
            FROM listen_daily_rollup r
            JOIN track_artists bta2 ON bta2.track_id = r.track_id
            WHERE %s
            GROUP BY 1`, where)
		case topAlbums:
			return fmt.Sprintf(`SELECT t.album_id AS id, sum(r.plays)::bigint AS plays, sum(r.ms)::bigint AS ms
            FROM listen_daily_rollup r
            JOIN tracks t ON t.id = r.track_id
            WHERE %s AND t.album_id IS NOT NULL
            GROUP BY 1`, where)
		default:
			return fmt.Sprintf(`SELECT r.track_id AS id, sum(r.plays)::bigint AS plays, sum(r.ms)::bigint AS ms
            FROM listen_daily_rollup r
            WHERE %s
            GROUP BY 1`, where)
		}
	}

	where := rangeFilter("l", userArg, fromArg, toArg)
	switch kind {
	case topArtists:
		return fmt.Sprintf(`SELECT bta2.artist_id AS id, count(*)::bigint AS plays, coalesce(sum(l.ms_played), 0)::bigint AS ms
            FROM listens l
            JOIN track_artists bta2 ON bta2.track_id = l.track_id
            WHERE %s
            GROUP BY 1`, where)
	case topAlbums:
		return fmt.Sprintf(`SELECT t.album_id AS id, count(*)::bigint AS plays, coalesce(sum(l.ms_played), 0)::bigint AS ms
            FROM listens l
            JOIN tracks t ON t.id = l.track_id
            WHERE %s AND t.album_id IS NOT NULL
            GROUP BY 1`, where)
	default:
		return fmt.Sprintf(`SELECT l.track_id AS id, count(*)::bigint AS plays, coalesce(sum(l.ms_played), 0)::bigint AS ms
            FROM listens l
            WHERE %s AND l.track_id IS NOT NULL
            GROUP BY 1`, where)
	}
}

// topSQL ranks the requested period, ranks the preceding period, and pages the
// first joined to the second.
//
// The result is deliberately produced by a LEFT JOIN from the one-row total onto
// the page: a page beyond the end of the list must still report how long the list
// is, and a query that returned nothing at all could not. Rows whose id is NULL
// are that empty-page marker and are skipped by the scanner.
//
// Parameters are $1 user, $2 from, $3 to, $4 limit, $5 offset, $6 previous from,
// $7 previous to, and, for the rollup variant only, $8 timezone.
func topSQL(kind topKind, rollup bool) string {
	tz := ""
	if rollup {
		tz = "$8"
	}
	return fmt.Sprintf(`
WITH cur AS (%s),
prev AS (%s),
ranked AS (
    SELECT id, plays, ms, row_number() OVER (ORDER BY plays DESC, ms DESC, id) AS rank
    FROM cur
),
prev_ranked AS (
    SELECT id, row_number() OVER (ORDER BY plays DESC, ms DESC, id) AS rank
    FROM prev
),
page AS (
    SELECT r.id, r.plays, r.ms, r.rank, coalesce(p.rank, 0) AS prev_rank
    FROM ranked r
    LEFT JOIN prev_ranked p ON p.id = r.id
    ORDER BY r.rank
    LIMIT $4 OFFSET $5
),
total AS (SELECT count(*)::bigint AS n FROM cur)
SELECT t.n, p.id, p.plays, p.ms, p.rank, p.prev_rank
FROM total t
LEFT JOIN page p ON true
ORDER BY p.rank NULLS LAST`,
		topSourceSQL(kind, "$1", "$2", "$3", tz, rollup),
		topSourceSQL(kind, "$1", "$6", "$7", tz, rollup))
}

// The six statements are built once at start-up rather than per request: the
// composition is pure string work, but it is pointless to repeat it.
var (
	topTracksFactSQL    = topSQL(topTracks, false)
	topTracksRollupSQL  = topSQL(topTracks, true)
	topArtistsFactSQL   = topSQL(topArtists, false)
	topArtistsRollupSQL = topSQL(topArtists, true)
	topAlbumsFactSQL    = topSQL(topAlbums, false)
	topAlbumsRollupSQL  = topSQL(topAlbums, true)
)

func topStatement(kind topKind, rollup bool) string {
	switch {
	case kind == topArtists && rollup:
		return topArtistsRollupSQL
	case kind == topArtists:
		return topArtistsFactSQL
	case kind == topAlbums && rollup:
		return topAlbumsRollupSQL
	case kind == topAlbums:
		return topAlbumsFactSQL
	case rollup:
		return topTracksRollupSQL
	default:
		return topTracksFactSQL
	}
}

// TopTracks ranks the user's tracks in the range, most played first.
func (s *Service) TopTracks(ctx context.Context, q store.Querier, userID uuid.UUID, r domain.TimeRange, tz string, limit, offset int) (TopPage, error) {
	return s.top(ctx, q, topTracks, userID, r, tz, limit, offset)
}

// TopArtists ranks the user's artists in the range, resolving each listen through
// track_artists.
func (s *Service) TopArtists(ctx context.Context, q store.Querier, userID uuid.UUID, r domain.TimeRange, tz string, limit, offset int) (TopPage, error) {
	return s.top(ctx, q, topArtists, userID, r, tz, limit, offset)
}

// TopAlbums ranks the user's albums in the range, resolving each listen through
// its track's album. Listens whose track has no album yet (metadata still
// pending) are not counted, because there is nothing to count them against.
func (s *Service) TopAlbums(ctx context.Context, q store.Querier, userID uuid.UUID, r domain.TimeRange, tz string, limit, offset int) (TopPage, error) {
	return s.top(ctx, q, topAlbums, userID, r, tz, limit, offset)
}

func (s *Service) top(ctx context.Context, q store.Querier, kind topKind, userID uuid.UUID, r domain.TimeRange, tz string, limit, offset int) (TopPage, error) {
	loc, err := scope(userID, r, tz)
	if err != nil {
		return TopPage{}, err
	}
	limit, offset = clampLimit(limit), clampOffset(offset)
	prev := r.Previous()

	// Both halves of the answer come from the same source, so the dirty check has
	// to cover the preceding period as well as the requested one.
	rollup := false
	if rollupEligible(r, loc) {
		dirty, err := s.HasDirtyDays(ctx, q, userID, rollupCheckRange(r), tz)
		if err != nil {
			return TopPage{}, err
		}
		rollup = useRollup(r, loc, dirty)
	}

	args := []any{
		store.UUIDArg(userID), r.From.UTC(), r.To.UTC(),
		limit, offset,
		prev.From.UTC(), prev.To.UTC(),
	}
	if rollup {
		args = append(args, tzArg(tz))
	}

	rows, err := q.Query(ctx, topStatement(kind, rollup), args...)
	if err != nil {
		return TopPage{}, postgres.Classify(kind.String(), err)
	}
	defer rows.Close()

	var out TopPage
	for rows.Next() {
		var (
			total                    int64
			id                       *string
			plays, ms, rank, prevRnk *int64
		)
		if err := rows.Scan(&total, &id, &plays, &ms, &rank, &prevRnk); err != nil {
			return TopPage{}, postgres.Classify("scan "+kind.String(), err)
		}
		out.Total = total
		if id == nil {
			continue
		}
		out.Entries = append(out.Entries, TopEntry{
			ID:           *id,
			Plays:        store.Deref(plays),
			MsPlayed:     store.Deref(ms),
			Rank:         int(store.Deref(rank)),
			PreviousRank: int(store.Deref(prevRnk)),
		})
	}
	if err := rows.Err(); err != nil {
		return TopPage{}, postgres.Classify(kind.String(), err)
	}
	return out, nil
}
