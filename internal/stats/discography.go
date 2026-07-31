package stats

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/RequiDev/encore/internal/domain"
	"github.com/RequiDev/encore/internal/postgres"
	"github.com/RequiDev/encore/internal/store"
)

// heardAlbumsSQL reports which of a given set of albums the user has ever
// played anything from.
//
// The set comes in as a parameter rather than being derived from a join,
// because the albums in question are Spotify's list of what an artist released
// and most of them are not in `albums` at all — that table is minted from
// listening. A join would answer "which albums by this artist have you played",
// which is the numerator masquerading as the denominator.
//
// Deliberately the same predicates as albumHeardTracksSQL: the user, the
// blacklist, and no range. A record heard five years ago is not one this
// listener has never played, whatever window the page happens to be showing.
//
// The blacklist filter is kept for consistency with every other read of
// `listens`, and it does have a visible consequence: a blacklisted artist's own
// page reports 0 of 11 heard. That is the same answer every other figure on
// that page gives, and the page already says the artist is excluded.
//
// Parameters are $1 user, $2 the album ids.
var heardAlbumsSQL = fmt.Sprintf(`
SELECT DISTINCT t.album_id
FROM listens l
JOIN tracks t ON t.id = l.track_id
WHERE l.user_id = $1 AND t.album_id = ANY($2) AND %s`, blacklistFilter("l"))

// HeardAlbums reports which of albumIDs the user has ever played anything from.
//
// "Played" is per album, not per track: one play of one track puts the album in
// this set. The caller says so in its copy, because "you have heard 4 of their
// 11 albums" would otherwise be read as having heard those four in full.
//
// It is returned as ids rather than as a count because only the caller knows
// which discography it is diffing against, and a count would not survive the two
// disagreeing about what the artist released.
func (s *Service) HeardAlbums(
	ctx context.Context,
	q store.Querier,
	userID uuid.UUID,
	albumIDs []string,
) ([]string, error) {
	// No range to validate, but a nil user id must not reach SQL looking like a
	// legitimate parameter — it would match nothing rather than fail.
	if userID == uuid.Nil {
		return nil, fmt.Errorf("%w: a user is required", domain.ErrValidation)
	}
	if len(albumIDs) == 0 {
		// An artist whose every release is a single has nothing counted, which is
		// an ordinary answer rather than an edge case. Sending `= ANY('{}')` would
		// spend a round trip to be told what is already known.
		return []string{}, nil
	}
	rows, err := q.Query(ctx, heardAlbumsSQL, store.UUIDArg(userID), albumIDs)
	if err != nil {
		return nil, postgres.Classify("heard albums", err)
	}
	defer rows.Close()

	out := make([]string, 0, len(albumIDs))
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, postgres.Classify("heard albums", err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, postgres.Classify("heard albums", err)
	}
	return out, nil
}
