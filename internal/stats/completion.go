package stats

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/RequiDev/encore/internal/domain"
	"github.com/RequiDev/encore/internal/postgres"
	"github.com/RequiDev/encore/internal/store"
)

// AlbumCompletion is how much of one album somebody has heard, ever.
//
// Known is false when the album's total_tracks is still 0, which means
// enrichment has not resolved it rather than that the album is empty. Without a
// denominator there is no ratio to render, and a freshly imported instance is in
// that state for nearly every album it holds.
type AlbumCompletion struct {
	Heard int64
	Total int64
	Known bool
}

// CompletedAlbums is the range-scoped aggregate: of the albums played inside the
// range whose track count is known, how many were heard in full.
//
// Both numbers describe the same population, so "of the N albums you played in
// this range, you have heard every track on M" is true as written. Albums whose
// total_tracks is unknown are in neither.
type CompletedAlbums struct {
	Complete int64
	Albums   int64
}

// albumCompletionSQL counts the distinct tracks of one album the user has ever
// played, against the album's own track count.
//
// Deliberately not range-filtered. Completion is a property of a listening
// lifetime; scoping it to whatever window the page is showing would report "1 of
// 12" to somebody looking at a week and call it completion. The user and
// blacklist predicates still apply — the same shape as the `ever` CTE in
// entityStatsSQL, which drops the range for first- and last-listen.
//
// Parameters are $1 user, $2 album id.
var albumCompletionSQL = fmt.Sprintf(`
SELECT
    (SELECT count(DISTINCT l.track_id)
     FROM listens l
     JOIN tracks t ON t.id = l.track_id
     WHERE l.user_id = $1 AND t.album_id = $2 AND %s)::bigint,
    (SELECT coalesce(max(a.total_tracks), 0) FROM albums a WHERE a.id = $2)::bigint`,
	blacklistFilter("l"))

// completedAlbumsSQL counts, within the range, albums heard in full.
//
// An album whose total_tracks is 0 is excluded from both counts rather than
// treated as incomplete: 0 means enrichment has not resolved it, and counting it
// as an album with no tracks heard would drag the figure down for a reason that
// has nothing to do with listening.
//
// Parameters are $1 user, $2 from, $3 to.
var completedAlbumsSQL = fmt.Sprintf(`
WITH played AS (
    SELECT t.album_id, count(DISTINCT l.track_id) AS heard
    FROM listens l
    JOIN tracks t ON t.id = l.track_id
    WHERE %s AND t.album_id IS NOT NULL
    GROUP BY t.album_id
)
SELECT count(*)::bigint,
       count(*) FILTER (WHERE p.heard >= a.total_tracks)::bigint
FROM played p
JOIN albums a ON a.id = p.album_id
WHERE a.total_tracks > 0`, rangeFilter("l", "$1", "$2", "$3"))

// AlbumCompletion reports how much of one album the user has ever heard.
func (s *Service) AlbumCompletion(
	ctx context.Context,
	q store.Querier,
	userID uuid.UUID,
	albumID string,
) (AlbumCompletion, error) {
	// No range to validate, but a nil user id must not reach SQL looking like a
	// legitimate parameter — it would silently match nothing rather than fail.
	if userID == uuid.Nil {
		return AlbumCompletion{}, fmt.Errorf("%w: a user is required", domain.ErrValidation)
	}
	var out AlbumCompletion
	err := q.QueryRow(ctx, albumCompletionSQL, store.UUIDArg(userID), albumID).
		Scan(&out.Heard, &out.Total)
	if err != nil {
		return AlbumCompletion{}, postgres.Classify("album completion", err)
	}
	out.Known = out.Total > 0
	if !out.Known {
		// Without a denominator the numerator says nothing useful, and shipping
		// it invites a caller to render "3 of 0".
		out.Heard = 0
	}
	return out, nil
}

// CompletedAlbums reports how many of the range's albums were heard in full.
func (s *Service) CompletedAlbums(
	ctx context.Context,
	q store.Querier,
	userID uuid.UUID,
	r domain.TimeRange,
) (CompletedAlbums, error) {
	if err := checkScope(userID, r); err != nil {
		return CompletedAlbums{}, err
	}
	var out CompletedAlbums
	err := q.QueryRow(ctx, completedAlbumsSQL,
		store.UUIDArg(userID), r.From.UTC(), r.To.UTC()).
		Scan(&out.Albums, &out.Complete)
	if err != nil {
		return CompletedAlbums{}, postgres.Classify("completed albums", err)
	}
	return out, nil
}
