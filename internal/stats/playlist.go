package stats

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/RequiDev/encore/internal/domain"
	"github.com/RequiDev/encore/internal/postgres"
	"github.com/RequiDev/encore/internal/store"
)

// PlaylistEntry is one selected track and why it was selected.
type PlaylistEntry struct {
	TrackID  string
	Plays    int64
	MsPlayed int64
}

// PlaylistSelection is the ordered set of tracks a definition resolves to.
type PlaylistSelection struct {
	// Tracks are best first, ready to send.
	Tracks []PlaylistEntry
	// Matched is how many distinct tracks met the criteria before the limit was
	// applied, so the interface can say "100 of 412" rather than only "100".
	Matched int64
}

// IDs are the Spotify ids in selection order, which is what a playlist write
// takes.
func (s PlaylistSelection) IDs() []string {
	out := make([]string, 0, len(s.Tracks))
	for _, t := range s.Tracks {
		out = append(out, t.TrackID)
	}
	return out
}

// localIDExclusion keeps Encore's own catalogue ids out of a playlist.
//
// Today it can never match: local ids are minted for artists and albums, which
// the exports name without identifying, and a track always carries the Spotify
// id its URI gave it. It is here because the invariant it protects — that every
// id Encore sends to Spotify is one Spotify issued — should hold by construction
// rather than by remembering which tables happen to be involved. One local id in
// a request fails the whole request, not that one track.
const localIDExclusion = `l.track_id IS NOT NULL AND l.track_id NOT LIKE 'local:%'`

// SelectPlaylistTracks resolves a definition into Spotify track ids.
//
// The four modes are four different questions, so they are four statements
// rather than one with holes in it. What they share — the user scope, the
// blacklist rule, the half-open range, the exclusion of local ids — comes from
// the same fragments every other statistic uses, which is what keeps a hidden
// artist hidden here too.
func (s *Service) SelectPlaylistTracks(
	ctx context.Context,
	q store.Querier,
	userID uuid.UUID,
	def domain.PlaylistDefinition,
	r domain.TimeRange,
) (PlaylistSelection, error) {
	if err := checkScope(userID, r); err != nil {
		return PlaylistSelection{}, err
	}
	if err := def.Validate(); err != nil {
		return PlaylistSelection{}, err
	}

	sql, args := playlistQuery(userID, def, r)

	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return PlaylistSelection{}, postgres.Classify("select playlist tracks", err)
	}
	defer rows.Close()

	out := PlaylistSelection{Tracks: make([]PlaylistEntry, 0, def.Limit)}
	for rows.Next() {
		var e PlaylistEntry
		var matched int64
		if err := rows.Scan(&e.TrackID, &e.Plays, &e.MsPlayed, &matched); err != nil {
			return PlaylistSelection{}, postgres.Classify("scan playlist track", err)
		}
		out.Tracks = append(out.Tracks, e)
		out.Matched = matched
	}
	if err := rows.Err(); err != nil {
		return PlaylistSelection{}, postgres.Classify("select playlist tracks", err)
	}
	return out, nil
}

// playlistQuery builds the statement for one mode.
//
// Every branch selects the same four columns: the track, why it qualified, and
// the number of tracks that matched before the limit — a window count, so the
// caller learns how much was left on the table without a second round trip.
func playlistQuery(userID uuid.UUID, def domain.PlaylistDefinition, r domain.TimeRange) (string, []any) {
	order := def.OrderColumn()
	args := []any{store.UUIDArg(userID), r.From.UTC(), r.To.UTC(), def.Limit}

	switch def.Mode {
	case domain.PlaylistModeMinPlays:
		args = append(args, def.MinPlays)
		return fmt.Sprintf(`
            WITH matched AS (
                SELECT l.track_id, count(*) AS plays, sum(l.ms_played)::bigint AS ms
                FROM listens l
                WHERE %s AND %s
                GROUP BY l.track_id
                HAVING count(*) >= $5
            )
            SELECT track_id, plays, ms, count(*) OVER () AS matched
            FROM matched ORDER BY %s DESC, track_id LIMIT $4`,
			rangeFilter("l", "$1", "$2", "$3"), localIDExclusion, order), args

	case domain.PlaylistModeDiscoveries:
		// "First ever", not "first in this window": otherwise every track
		// qualifies for a range that starts at the beginning of the history.
		return fmt.Sprintf(`
            WITH matched AS (
                SELECT l.track_id, count(*) AS plays, sum(l.ms_played)::bigint AS ms
                FROM listens l
                WHERE %s AND %s
                  AND NOT EXISTS (
                    SELECT 1 FROM listens pre
                    WHERE pre.user_id = l.user_id AND pre.track_id = l.track_id
                      AND pre.played_at < $2::timestamptz)
                GROUP BY l.track_id
            )
            SELECT track_id, plays, ms, count(*) OVER () AS matched
            FROM matched ORDER BY %s DESC, track_id LIMIT $4`,
			rangeFilter("l", "$1", "$2", "$3"), localIDExclusion, order), args

	case domain.PlaylistModeForgotten:
		// Ranked over everything before the range, then anything played inside it
		// is dropped. The range is the period they are absent from, not the period
		// they are counted over — which is why an all-time range is refused.
		return fmt.Sprintf(`
            WITH matched AS (
                SELECT l.track_id, count(*) AS plays, sum(l.ms_played)::bigint AS ms
                FROM listens l
                WHERE l.user_id = $1
                  AND l.played_at < $2::timestamptz
                  AND %s AND %s
                  AND NOT EXISTS (
                    SELECT 1 FROM listens recent
                    WHERE recent.user_id = l.user_id AND recent.track_id = l.track_id
                      AND recent.played_at >= $2::timestamptz
                      AND recent.played_at < $3::timestamptz)
                GROUP BY l.track_id
            )
            SELECT track_id, plays, ms, count(*) OVER () AS matched
            FROM matched ORDER BY %s DESC, track_id LIMIT $4`,
			blacklistFilter("l"), localIDExclusion, order), args

	default: // domain.PlaylistModeTop
		return fmt.Sprintf(`
            WITH matched AS (
                SELECT l.track_id, count(*) AS plays, sum(l.ms_played)::bigint AS ms
                FROM listens l
                WHERE %s AND %s
                GROUP BY l.track_id
            )
            SELECT track_id, plays, ms, count(*) OVER () AS matched
            FROM matched ORDER BY %s DESC, track_id LIMIT $4`,
			rangeFilter("l", "$1", "$2", "$3"), localIDExclusion, order), args
	}
}
