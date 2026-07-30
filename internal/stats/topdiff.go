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

// TopDiffEntry is one entity in the comparison between Spotify's own ranking
// and Encore's ranking of the same kind and window.
//
// SpotifyRank and EncoreRank are each 1-based and independent: 0 means the
// entity is absent from that side rather than tied for last place, because it
// fell outside that side's own top page (see TopDiff's limit). A blacklisted
// artist - or, for the track kind, a track credited to one - never produces
// an entry at all: the blacklist means "don't show me this artist", so it is
// removed from both sides rather than surfaced with one rank zeroed. An entry
// with both ranks set is where the two sides agree an entity belongs; an
// entry with only one set is the disagreement this statistic exists to
// surface.
type TopDiffEntry struct {
	EntityID    string
	SpotifyRank int
	EncoreRank  int
	Plays       int64
}

// TopDiff is the comparison for one (kind, time range) pair.
//
// CapturedAt is nil when Spotify's ranking for this set has never been
// captured - the listener has not granted the top-items scope, or the daily
// worker has not run since they did - and Entries is then always empty
// rather than Encore's ranking presented on its own: a list with every
// SpotifyRank forced to zero would look like a comparison but would not be
// one, since there is nothing on the other side to disagree with.
type TopDiff struct {
	CapturedAt *time.Time
	TimeRange  string
	Entries    []TopDiffEntry
}

// topDiffWindow derives the window Encore's side is computed over from
// Spotify's own time_range, and is the reason the top-diff page has no range
// picker of its own.
//
// Spotify's short_term/medium_term/long_term describe *Spotify's* rolling
// windows over the listener's whole account, not a period a picker on this
// page could stand in for. If Encore's side were computed over some other
// window - whatever range a picker happened to have selected, say - the two
// rankings would be answers to different questions, and any gap between them
// would be an artefact of the mismatched window rather than a genuine
// disagreement about what the listener has actually been playing. So the
// window is fixed by time_range alone, derived from now rather than handed
// in by a caller.
//
// Spotify documents these windows as approximate rather than exact - its own
// docs describe long_term as "calculated from ~1 year of data" - so the
// durations below are approximations too, chosen to match Spotify's own
// wording:
//
//   - short_term  ~ the last 4 weeks
//   - medium_term ~ the last 6 months
//   - long_term   ~ the last 12 months
func topDiffWindow(timeRange string, now time.Time) (from, to time.Time, err error) {
	switch timeRange {
	case "short_term":
		return now.AddDate(0, 0, -28), now, nil
	case "medium_term":
		return now.AddDate(0, -6, 0), now, nil
	case "long_term":
		return now.AddDate(-1, 0, 0), now, nil
	default:
		return time.Time{}, time.Time{}, fmt.Errorf(
			"%w: %q is not a Spotify top-items time range (want short_term, medium_term or long_term)",
			domain.ErrValidation, timeRange)
	}
}

// topDiffKind maps the entity kind spotify_top_snapshots stores (see its
// CHECK constraint in migrations/00011_top_snapshots.sql) onto the ranking
// topSourceSQL already knows how to build. Spotify has no "top albums"
// endpoint, so, unlike TopTracks, TopArtists and TopAlbums, "album" is not a
// valid kind here.
func topDiffKind(kind string) (topKind, error) {
	switch kind {
	case "track":
		return topTracks, nil
	case "artist":
		return topArtists, nil
	default:
		return 0, fmt.Errorf(
			"%w: %q is not a top-diff kind (want track or artist)", domain.ErrValidation, kind)
	}
}

// topDiffCapturedAtSQL reads the watermark of the last capture for one
// (user, kind, time_range) set on its own, without touching Encore's
// ranking at all. TopDiff queries it first and alone so the never-captured
// case described on TopDiff's doc comment can return before any of the rest
// of this statistic does work for nothing.
//
// max() over a possibly-empty set is NULL rather than no row, so this always
// returns exactly one row - the same scalar-subquery shape librarySnapshotSQL
// uses for the same reason.
//
// Parameters are $1 user, $2 kind, $3 time range.
const topDiffCapturedAtSQL = `
SELECT max(captured_at) FROM spotify_top_snapshots
WHERE user_id = $1 AND kind = $2 AND time_range = $3`

// topDiffSpotifyBlacklistFilter excludes a Spotify-captured entity from the
// comparison entirely when the owner has blacklisted the artist behind it.
// Without this, the blacklist would reach only Encore's side of the join
// (via topSourceSQL below) and a blacklisted artist Spotify still ranks
// would survive with its Spotify rank intact - which is exactly the
// behaviour the project reversed: a blacklist means "don't show me this
// artist", full stop, not "don't show me Encore's numbers for this artist."
//
// The shape differs by kind for the same reason topSourceSQL's does. An
// artist id is its own subject and is checked directly against
// user_blacklisted_artists - the same direct-EXISTS shape dormantFollowsSQL
// (library.go) uses to remove a blacklisted followed artist rather than let
// it read as merely dormant. A track id has no artist of its own, so it is
// checked through track_artists instead, the same join blacklistFilter uses
// everywhere else in this package.
//
// A snapshot track not yet in the catalogue - minted pending by the library
// worker, with no track_artists rows written for it yet - cannot be
// resolved against the blacklist at all: there is nothing to join against,
// so NOT EXISTS is vacuously true and the track is not excluded. This is not
// a new judgement call invented for this statement; it is the identical
// behaviour blacklistFilter itself already has everywhere else in this
// package (an unresolved track's absence of credits reads as "not
// blacklisted", never as "blacklisted"), so a track can still appear here
// until enrichment finishes filling in its credits, exactly as it can in
// every other statistic that reads track_artists. Hiding it instead would
// require treating "unknown" as "blacklisted", which would drop a
// legitimate row on nothing more than an enrichment race.
func topDiffSpotifyBlacklistFilter(kind topKind, alias string) string {
	if kind == topArtists {
		return fmt.Sprintf(`NOT EXISTS (
            SELECT 1 FROM user_blacklisted_artists bl
            WHERE bl.user_id = $1 AND bl.artist_id = %[1]s.entity_id)`, alias)
	}
	return fmt.Sprintf(`NOT EXISTS (
            SELECT 1 FROM track_artists bta
            JOIN user_blacklisted_artists bl
              ON bl.artist_id = bta.artist_id AND bl.user_id = $1
            WHERE bta.track_id = %[1]s.entity_id)`, alias)
}

// topDiffSQL is the full outer join in spirit described on TopDiff: Spotify's
// captured ranking and Encore's own, each independently capped to the
// requested page size, joined on entity id so a row present on only one side
// still appears, with a zero rank on the side it is missing from.
//
// encore_ranked reuses topSourceSQL's shape (internal/stats/top.go) rather
// than a third definition of "top", and therefore also its blacklist rule:
// topSourceSQL's non-rollup branch runs every request through rangeFilter,
// which composes blacklistFilter, so a blacklisted artist's plays are
// invisible to encore_ranked before row_number() ever runs over it.
//
// spotify_ranked applies topDiffSpotifyBlacklistFilter before its own
// row_number(), for the same reason: a blacklisted entity must be gone
// before ranking happens, not filtered out afterward with its original
// position left showing through a gap. That ordering is what makes the
// surviving ranks close up - the entity after a removed one moves up to
// take its place - rather than leaving Spotify's raw position numbers with a
// hole where the blacklisted entry used to be. encore_ranked already closes
// up the same way, because blacklistFilter excludes a blacklisted artist's
// plays before that CTE's own row_number() runs; spotify_ranked simply does
// the equivalent for a table read instead of an aggregate.
//
// The rollup variant of topSourceSQL is deliberately not used here: it would
// need its own dirty-day check keyed to this statistic's own window rather
// than the caller-supplied range every other rollup-eligible statistic
// checks, for a page that is read far less often than the ones the rollup
// exists to speed up. Reading the fact table directly is simpler and always
// correct, which matters more here than shaving a rarely-run query.
//
// Parameters are $1 user, $2 kind, $3 time range, $4 window from, $5 window
// to, $6 limit.
func topDiffSQL(kind topKind) string {
	return fmt.Sprintf(`
WITH encore_ranked AS (
    SELECT id, plays, ms, row_number() OVER (ORDER BY plays DESC, ms DESC, id) AS rank
    FROM (%s) src
),
encore_top AS (
    SELECT id, plays, rank FROM encore_ranked WHERE rank <= $6
),
spotify_ranked AS (
    SELECT entity_id, row_number() OVER (ORDER BY position) AS rank
    FROM (
        SELECT entity_id, position
        FROM spotify_top_snapshots
        WHERE user_id = $1 AND kind = $2 AND time_range = $3
    ) raw
    WHERE %s
),
spotify_top AS (
    SELECT entity_id, rank FROM spotify_ranked WHERE rank <= $6
)
SELECT coalesce(s.entity_id, e.id) AS entity_id,
       coalesce(s.rank, 0) AS spotify_rank,
       coalesce(e.rank, 0) AS encore_rank,
       coalesce(e.plays, 0) AS plays
FROM spotify_top s
FULL OUTER JOIN encore_top e ON e.id = s.entity_id
ORDER BY least(coalesce(s.rank, 999999999), coalesce(e.rank, 999999999)),
         coalesce(s.entity_id, e.id)`,
		topSourceSQL(kind, "$1", "$4", "$5", "", false),
		topDiffSpotifyBlacklistFilter(kind, "raw"))
}

// The two statements are built once at start-up, exactly as the six in top.go
// are, since the composition is pure string work that need not repeat per
// request.
var (
	topDiffTracksSQL  = topDiffSQL(topTracks)
	topDiffArtistsSQL = topDiffSQL(topArtists)
)

func topDiffStatement(kind topKind) string {
	if kind == topArtists {
		return topDiffArtistsSQL
	}
	return topDiffTracksSQL
}

// TopDiff compares Spotify's own ranking of the user's top kind ("track" or
// "artist") for timeRange against Encore's ranking of the same entities over
// the matching window - see topDiffWindow for why that window is derived
// from timeRange rather than accepted as a parameter of its own - capped to
// limit entries on each side.
func (s *Service) TopDiff(ctx context.Context, q store.Querier, userID uuid.UUID, kind, timeRange, tz string, limit int) (TopDiff, error) {
	if userID == uuid.Nil {
		return TopDiff{}, fmt.Errorf("%w: a user is required", domain.ErrValidation)
	}
	loc, err := location(tz)
	if err != nil {
		return TopDiff{}, err
	}
	tk, err := topDiffKind(kind)
	if err != nil {
		return TopDiff{}, err
	}
	from, to, err := topDiffWindow(timeRange, s.Now())
	if err != nil {
		return TopDiff{}, err
	}
	limit = clampLimit(limit)

	out := TopDiff{TimeRange: timeRange}

	if err := q.QueryRow(ctx, topDiffCapturedAtSQL, store.UUIDArg(userID), kind, timeRange).
		Scan(&out.CapturedAt); err != nil {
		return TopDiff{}, postgres.Classify("top diff captured at", err)
	}
	out.CapturedAt = toLocation(out.CapturedAt, loc)
	if out.CapturedAt == nil {
		// Nothing has ever been captured for this set: see TopDiff's doc comment
		// for why that means no entries rather than Encore's ranking alone.
		return out, nil
	}

	rows, err := q.Query(ctx, topDiffStatement(tk),
		store.UUIDArg(userID), kind, timeRange, from.UTC(), to.UTC(), limit)
	if err != nil {
		return TopDiff{}, postgres.Classify("top diff", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			e                       TopDiffEntry
			spotifyRank, encoreRank int64
		)
		if err := rows.Scan(&e.EntityID, &spotifyRank, &encoreRank, &e.Plays); err != nil {
			return TopDiff{}, postgres.Classify("scan top diff", err)
		}
		e.SpotifyRank = int(spotifyRank)
		e.EncoreRank = int(encoreRank)
		out.Entries = append(out.Entries, e)
	}
	if err := rows.Err(); err != nil {
		return TopDiff{}, postgres.Classify("top diff", err)
	}
	return out, nil
}
