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

// LibraryStats crosses a user's Spotify library — what they have saved and
// who they follow, as of the last enumeration — against what they have
// actually listened to.
//
// The three lists below are deliberately scoped in three different ways, and
// that is not an inconsistency to tidy up:
//
//   - SavedNeverPlayed is all-time. "Never played" means never; scoping it to
//     the requested window would list every saved track the listener simply
//     did not happen to play last week.
//   - PlayedNeverSaved and DormantFollows are both range-scoped: they describe
//     what happened (or did not) inside the requested window, so narrowing or
//     widening the range changes the answer.
//   - SyncedAt and the three counts describe neither history nor a range —
//     they are a snapshot of the last enumeration itself.
//
// SyncedAt is nil until the library worker's first successful run, which is
// the state of every account on an upgraded instance. That must not be
// confused with "enumerated and found nothing", so it is never substituted
// with a zero time.
type LibraryStats struct {
	SyncedAt        *time.Time
	SavedTracks     int64
	SavedAlbums     int64
	FollowedArtists int64

	SavedNeverPlayed []SavedTrackEntry
	PlayedNeverSaved []PlayedTrackEntry
	DormantFollows   []DormantArtistEntry
}

// SavedTrackEntry is one saved track nothing in the fact table has ever
// played. AddedAt is nil when Spotify did not report it, or the listener
// saved it before Encore recorded that field.
type SavedTrackEntry struct {
	TrackID string
	AddedAt *time.Time
}

// PlayedTrackEntry is one track played inside the range that the range's
// listener has never saved.
type PlayedTrackEntry struct {
	TrackID  string
	Plays    int64
	MsPlayed int64
}

// DormantArtistEntry is one followed artist with no play inside the range.
// LastPlayedAt is the artist's last play ever, regardless of the range, so the
// UI can say how long it has actually been rather than only that it has been
// a while. It is nil when the artist has never been played at all.
type DormantArtistEntry struct {
	ArtistID     string
	LastPlayedAt *time.Time
}

// librarySnapshotSQL reads the enumeration watermark and the three library
// counts in one round trip.
//
// Every subquery is scalar and independently scoped to $1, rather than a join
// across spotify_credentials and the three library tables, because a user who
// has never connected — or never been enumerated — has no
// spotify_credentials row at all: a join would still work today (its absence
// changes nothing a join needs), but a scalar subquery makes that independence
// explicit and is what keeps this statement free of any reference to listens.
//
// Parameters are $1 user.
var librarySnapshotSQL = `
SELECT
    (SELECT c.library_synced_at FROM spotify_credentials c WHERE c.user_id = $1),
    (SELECT count(*) FROM user_saved_tracks     WHERE user_id = $1)::bigint,
    (SELECT count(*) FROM user_saved_albums     WHERE user_id = $1)::bigint,
    (SELECT count(*) FROM user_followed_artists WHERE user_id = $1)::bigint`

// savedNeverPlayedSQL is a LEFT JOIN of the saved-tracks table onto listens,
// keeping the rows with no match.
//
// Deliberately not range-filtered — no range argument appears anywhere in this
// statement — for the same reason albumCompletionSQL's ever-listened count
// is not: "never played" is a property of the whole listening history, and
// scoping it to whatever window a page happens to show would report every
// saved track the listener did not happen to play in that window, which is a
// different and much less interesting question. The join carries the
// blacklist fragment for the same reason the `ever` CTE in entityStatsSQL
// does (internal/stats/entity.go): a play that is invisible to every other
// statistic must not count as "played" here either, or a saved track heard
// only through a blacklisted artist would wrongly read as heard.
//
// Parameters are $1 user, $2 limit.
var savedNeverPlayedSQL = fmt.Sprintf(`
SELECT s.track_id, s.added_at
FROM user_saved_tracks s
LEFT JOIN listens l
       ON l.user_id = s.user_id AND l.track_id = s.track_id AND %s
WHERE s.user_id = $1 AND l.track_id IS NULL
ORDER BY s.added_at DESC NULLS LAST, s.track_id
LIMIT $2`, blacklistFilter("l"))

// playedNeverSavedSQL groups in-range listens by track, excluding any track
// already present in user_saved_tracks. rangeFilter both scopes it to the
// range and applies the blacklist, exactly as every other range-scoped
// ranking in this package does.
//
// Parameters are $1 user, $2 from, $3 to, $4 limit.
var playedNeverSavedSQL = fmt.Sprintf(`
SELECT l.track_id, count(*)::bigint AS plays, coalesce(sum(l.ms_played), 0)::bigint AS ms
FROM listens l
WHERE %s
  AND l.track_id IS NOT NULL
  AND NOT EXISTS (
        SELECT 1 FROM user_saved_tracks s
        WHERE s.user_id = l.user_id AND s.track_id = l.track_id)
GROUP BY l.track_id
ORDER BY plays DESC, ms DESC, l.track_id
LIMIT $4`, rangeFilter("l", "$1", "$2", "$3"))

// dormantFollowsSQL is user_followed_artists minus the artists with an
// in-range listen, carrying each survivor's all-time last play.
//
// The subtlety this statement exists to get right is where the blacklist
// fragment is allowed to reach. listened_in_range uses rangeFilter, which
// bakes the blacklist into "does this artist have a play in range" — exactly
// as every other range-scoped statistic in this package treats that question,
// and correctly so: a play invisible to every other chart must be invisible
// to the "has this artist been played" test too. But that same fact means a
// blacklisted artist's plays are never in listened_in_range regardless of
// whether they actually happened, so a followed, blacklisted artist always
// looks like it has "no qualifying play" — and if that were the only signal
// used, the artist would be *promoted into* the dormant list precisely
// because it was blacklisted, which is backwards: a blacklisted artist must
// disappear from the results entirely, not be reported as dormant.
//
// The fix is therefore a second, independent predicate that has nothing to do
// with listens: a direct EXISTS against user_blacklisted_artists, keyed only
// on the followed artist's own id, applied to user_followed_artists before
// the "minus listened" step ever runs. That predicate is what actually removes
// the artist from the output; listened_in_range's use of the blacklist is
// just the ordinary rule doing its ordinary job on the plays that remain.
//
// last_play is intentionally separate from listened_in_range and carries no
// range bound of its own, only the blacklist (the same "ever" shape used
// elsewhere in this package): the UI wants to say how long an artist has
// actually been dormant, which requires the true last play, not the last one
// inside whatever window is on screen.
//
// Parameters are $1 user, $2 from, $3 to, $4 limit.
var dormantFollowsSQL = fmt.Sprintf(`
WITH listened_in_range AS (
    SELECT DISTINCT ta.artist_id
    FROM listens l
    JOIN track_artists ta ON ta.track_id = l.track_id
    WHERE %s
),
last_play AS (
    SELECT ta.artist_id, max(l.played_at) AS last_at
    FROM listens l
    JOIN track_artists ta ON ta.track_id = l.track_id
    WHERE l.user_id = $1 AND %s
    GROUP BY ta.artist_id
)
SELECT f.artist_id, lp.last_at
FROM user_followed_artists f
LEFT JOIN last_play lp ON lp.artist_id = f.artist_id
WHERE f.user_id = $1
  AND NOT EXISTS (
        SELECT 1 FROM user_blacklisted_artists bl
        WHERE bl.user_id = f.user_id AND bl.artist_id = f.artist_id)
  AND NOT EXISTS (SELECT 1 FROM listened_in_range li WHERE li.artist_id = f.artist_id)
ORDER BY lp.last_at ASC NULLS FIRST, f.artist_id
LIMIT $4`, rangeFilter("l", "$1", "$2", "$3"), blacklistFilter("l"))

// Library answers the library statistics page: the last enumeration's
// snapshot, plus the three cross-referenced lists described on LibraryStats.
func (s *Service) Library(
	ctx context.Context,
	q store.Querier,
	userID uuid.UUID,
	r domain.TimeRange,
	tz string,
	limit int,
) (LibraryStats, error) {
	loc, err := scope(userID, r, tz)
	if err != nil {
		return LibraryStats{}, err
	}
	limit = clampLimit(limit)

	var out LibraryStats
	err = q.QueryRow(ctx, librarySnapshotSQL, store.UUIDArg(userID)).
		Scan(&out.SyncedAt, &out.SavedTracks, &out.SavedAlbums, &out.FollowedArtists)
	if err != nil {
		return LibraryStats{}, postgres.Classify("library snapshot", err)
	}
	out.SyncedAt = toLocation(out.SyncedAt, loc)

	if out.SavedNeverPlayed, err = s.savedNeverPlayed(ctx, q, userID, loc, limit); err != nil {
		return LibraryStats{}, err
	}
	if out.PlayedNeverSaved, err = s.playedNeverSaved(ctx, q, userID, r, limit); err != nil {
		return LibraryStats{}, err
	}
	if out.DormantFollows, err = s.dormantFollows(ctx, q, userID, r, loc, limit); err != nil {
		return LibraryStats{}, err
	}
	return out, nil
}

func (s *Service) savedNeverPlayed(ctx context.Context, q store.Querier, userID uuid.UUID, loc *time.Location, limit int) ([]SavedTrackEntry, error) {
	rows, err := q.Query(ctx, savedNeverPlayedSQL, store.UUIDArg(userID), limit)
	if err != nil {
		return nil, postgres.Classify("saved never played", err)
	}
	defer rows.Close()

	out := make([]SavedTrackEntry, 0, limit)
	for rows.Next() {
		var e SavedTrackEntry
		if err := rows.Scan(&e.TrackID, &e.AddedAt); err != nil {
			return nil, postgres.Classify("scan saved never played", err)
		}
		e.AddedAt = toLocation(e.AddedAt, loc)
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, postgres.Classify("saved never played", err)
	}
	return out, nil
}

func (s *Service) playedNeverSaved(ctx context.Context, q store.Querier, userID uuid.UUID, r domain.TimeRange, limit int) ([]PlayedTrackEntry, error) {
	rows, err := q.Query(ctx, playedNeverSavedSQL, store.UUIDArg(userID), r.From.UTC(), r.To.UTC(), limit)
	if err != nil {
		return nil, postgres.Classify("played never saved", err)
	}
	defer rows.Close()

	out := make([]PlayedTrackEntry, 0, limit)
	for rows.Next() {
		var e PlayedTrackEntry
		if err := rows.Scan(&e.TrackID, &e.Plays, &e.MsPlayed); err != nil {
			return nil, postgres.Classify("scan played never saved", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, postgres.Classify("played never saved", err)
	}
	return out, nil
}

func (s *Service) dormantFollows(ctx context.Context, q store.Querier, userID uuid.UUID, r domain.TimeRange, loc *time.Location, limit int) ([]DormantArtistEntry, error) {
	rows, err := q.Query(ctx, dormantFollowsSQL, store.UUIDArg(userID), r.From.UTC(), r.To.UTC(), limit)
	if err != nil {
		return nil, postgres.Classify("dormant follows", err)
	}
	defer rows.Close()

	out := make([]DormantArtistEntry, 0, limit)
	for rows.Next() {
		var e DormantArtistEntry
		if err := rows.Scan(&e.ArtistID, &e.LastPlayedAt); err != nil {
			return nil, postgres.Classify("scan dormant follows", err)
		}
		e.LastPlayedAt = toLocation(e.LastPlayedAt, loc)
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, postgres.Classify("dormant follows", err)
	}
	return out, nil
}
