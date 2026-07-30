package stats

// The playback-context statistics answer "how you listened" rather than "what
// you listened to".
//
// Two properties govern every query here.
//
// The columns are partial. platform, conn_country, reason_start, reason_end,
// shuffle, skipped, offline and incognito are written only by the extended-export
// importer. Live sync and account-data rows carry NULL in all eight. So every
// figure travels with its own denominator, and the denominator is counted per
// column — count(*) FILTER (WHERE col IS NOT NULL) — never per source, because
// an export may omit an individual field and keying on source = 2 would
// silently overstate it.
//
// listen_daily_rollup cannot serve any of this. It is keyed by (user, day,
// track) and carries no context columns at all, so these always scan the fact
// table. That is a property of the rollup, not an oversight here.

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/RequiDev/encore/internal/domain"
	"github.com/RequiDev/encore/internal/postgres"
	"github.com/RequiDev/encore/internal/store"
)

// ContextSlice is one category of a breakdown.
type ContextSlice struct {
	Key   string
	Plays int64
}

// PlaybackContext is the whole "how you listen" answer for one range.
type PlaybackContext struct {
	EndReasons        []ContextSlice
	EndReasonCoverage Coverage

	SkipRate     float64
	SkipCoverage Coverage

	ShuffleRate     float64
	ShuffleCoverage Coverage

	Platforms        []ContextSlice
	PlatformCoverage Coverage

	Countries       []ContextSlice
	CountryCoverage Coverage

	OfflineRate     float64
	OfflineCoverage Coverage

	IncognitoRate     float64
	IncognitoCoverage Coverage

	Playlists        []PlaylistContextEntry
	PlaylistCoverage Coverage
}

// PlaylistContextEntry is one (context_type, context_id) group: what the
// listener was playing from, and how many times.
//
// Name is resolved against user_playlists and is empty whenever that lookup
// finds nothing — every album, artist and collection context always, and a
// playlist context whenever the id no longer names one of the listener's own
// playlists (deleted since, or never theirs to begin with). An empty name is
// not an error state: the row still counts, because dropping it would
// understate the total the coverage figure promises.
type PlaylistContextEntry struct {
	ContextType string
	ContextID   string
	Name        string
	Plays       int64
}

// contextRatesSQL computes every scalar ratio and its own denominator in one
// pass over the range.
//
// SkipRate is defined as reason_end = 'fwdbtn'. This is a judgement call and is
// recorded as one: the skipped boolean is sparsely and inconsistently populated
// across export vintages, whereas reason_end is reliably present, and 'backbtn'
// is deliberately excluded because going back is not the gesture skipping is.
//
// Parameters are $1 user, $2 from, $3 to.
var contextRatesSQL = fmt.Sprintf(`
SELECT count(*)::bigint,
       count(l.reason_end)::bigint,
       count(*) FILTER (WHERE l.reason_end = 'fwdbtn')::bigint,
       count(l.shuffle)::bigint,
       count(*) FILTER (WHERE l.shuffle)::bigint,
       count(l.offline)::bigint,
       count(*) FILTER (WHERE l.offline)::bigint,
       count(l.incognito)::bigint,
       count(*) FILTER (WHERE l.incognito)::bigint
FROM listens l
WHERE %s`, rangeFilter("l", "$1", "$2", "$3"))

// contextBreakdownSQL returns the three categorical breakdowns in one result
// set, tagged by kind, rather than in three round trips.
//
// platform is returned raw: the grouping into families happens in Go, in
// PlatformFamily, so the classifier stays testable without a database and the
// original strings are never lost to a GROUP BY.
//
// Parameters are $1 user, $2 from, $3 to.
var contextBreakdownSQL = fmt.Sprintf(`
WITH scoped AS (SELECT l.platform, l.conn_country, l.reason_end FROM listens l WHERE %s)
SELECT 'platform' AS kind, s.platform AS key, count(*)::bigint
FROM scoped s WHERE s.platform IS NOT NULL GROUP BY s.platform
UNION ALL
SELECT 'country', s.conn_country, count(*)::bigint
FROM scoped s WHERE s.conn_country IS NOT NULL GROUP BY s.conn_country
UNION ALL
SELECT 'reason_end', s.reason_end, count(*)::bigint
FROM scoped s WHERE s.reason_end IS NOT NULL GROUP BY s.reason_end`,
	rangeFilter("l", "$1", "$2", "$3"))

// playlistContextSQL groups in-range listens by what the listener was playing
// from and names each group against user_playlists.
//
// Only source = 0 rows can ever carry context (see this package's own header
// and migrations/00012's), so context_type IS NOT NULL already scopes this to
// live-synced rows without needing to say source = 0 — which matters, because
// the coverage query below must count the same column, not the source, and
// the two statements have to agree on what "has context" means.
//
// The LEFT JOIN onto user_playlists only ever matches a context_type of
// "playlist" whose id is still one of this listener's own. A playlist deleted
// since, one owned by somebody else, and every "album", "artist" and
// "collection" context all satisfy context_type IS NOT NULL and must still be
// counted, but user_playlists holds none of them — the join simply leaves
// name unmatched rather than dropping the row, and coalesce turns that into an
// empty string rather than a null reaching Go. Using JOIN instead would
// silently understate the total this same range's coverage query reports.
//
// context_id can itself be NULL here — Spotify's bare "spotify:collection" URI
// (Liked Songs with no id segment) parses to one, per ingest.go's contextID —
// so it is coalesced to "" for the same reason: a group that names nothing
// must still surface, never disappear.
//
// Parameters are $1 user, $2 from, $3 to, $4 limit.
var playlistContextSQL = fmt.Sprintf(`
SELECT l.context_type, coalesce(l.context_id, '') AS context_id,
       coalesce(up.name, '') AS name, count(*)::bigint AS plays
FROM listens l
LEFT JOIN user_playlists up
  ON up.user_id = l.user_id AND up.playlist_id = l.context_id AND l.context_type = 'playlist'
WHERE %s AND l.context_type IS NOT NULL
GROUP BY l.context_type, l.context_id, up.name
ORDER BY plays DESC, l.context_type, l.context_id
LIMIT $4`, rangeFilter("l", "$1", "$2", "$3"))

// playlistContextCoverageSQL is the denominator playlistContextSQL's rows are
// counted against: every in-range listen, and how many of those carry any
// context at all. Per column — count(*) FILTER (WHERE context_type IS NOT
// NULL) — never per source, for the reason this file's header states: keying
// on source would require re-deriving which source can carry context here
// (only 0 can) inside this query too, and the two definitions could drift.
// Asking the column directly cannot drift from itself.
//
// Parameters are $1 user, $2 from, $3 to.
var playlistContextCoverageSQL = fmt.Sprintf(`
SELECT count(*)::bigint, count(*) FILTER (WHERE l.context_type IS NOT NULL)::bigint
FROM listens l
WHERE %s`, rangeFilter("l", "$1", "$2", "$3"))

// ratio divides safely. A zero denominator is "no data", which is a zero rate
// carrying a zero coverage, not a division by zero and not an error.
func ratio(n, d int64) float64 {
	if d == 0 {
		return 0
	}
	return float64(n) / float64(d)
}

// PlaybackContext answers how the range's listening happened.
func (s *Service) PlaybackContext(
	ctx context.Context,
	q store.Querier,
	userID uuid.UUID,
	r domain.TimeRange,
	tz string,
) (PlaybackContext, error) {
	if _, err := scope(userID, r, tz); err != nil {
		return PlaybackContext{}, err
	}

	var (
		out                      PlaybackContext
		total                    int64
		endN, skipN              int64
		shuffleN, shuffleYes     int64
		offlineN, offlineYes     int64
		incognitoN, incognitoYes int64
	)
	if err := q.QueryRow(ctx, contextRatesSQL,
		store.UUIDArg(userID), r.From.UTC(), r.To.UTC(),
	).Scan(&total, &endN, &skipN, &shuffleN, &shuffleYes,
		&offlineN, &offlineYes, &incognitoN, &incognitoYes); err != nil {
		return PlaybackContext{}, postgres.Classify("playback context rates", err)
	}

	out.EndReasonCoverage = Coverage{Covered: endN, Total: total}
	out.SkipCoverage = Coverage{Covered: endN, Total: total}
	out.SkipRate = ratio(skipN, endN)
	out.ShuffleCoverage = Coverage{Covered: shuffleN, Total: total}
	out.ShuffleRate = ratio(shuffleYes, shuffleN)
	out.OfflineCoverage = Coverage{Covered: offlineN, Total: total}
	out.OfflineRate = ratio(offlineYes, offlineN)
	out.IncognitoCoverage = Coverage{Covered: incognitoN, Total: total}
	out.IncognitoRate = ratio(incognitoYes, incognitoN)

	rows, err := q.Query(ctx, contextBreakdownSQL,
		store.UUIDArg(userID), r.From.UTC(), r.To.UTC())
	if err != nil {
		return PlaybackContext{}, postgres.Classify("playback context breakdown", err)
	}
	defer rows.Close()

	families := map[string]int64{}
	var platformTotal int64
	for rows.Next() {
		var (
			kind  string
			key   string
			plays int64
		)
		if err := rows.Scan(&kind, &key, &plays); err != nil {
			return PlaybackContext{}, postgres.Classify("scan playback context breakdown", err)
		}
		switch kind {
		case "platform":
			families[PlatformFamily(key)] += plays
			platformTotal += plays
		case "country":
			out.Countries = append(out.Countries, ContextSlice{Key: key, Plays: plays})
		case "reason_end":
			out.EndReasons = append(out.EndReasons, ContextSlice{Key: key, Plays: plays})
		}
	}
	if err := rows.Err(); err != nil {
		return PlaybackContext{}, postgres.Classify("playback context breakdown", err)
	}

	out.Platforms = sortedSlices(families)
	out.PlatformCoverage = Coverage{Covered: platformTotal, Total: total}
	out.CountryCoverage = Coverage{Covered: sumSlices(out.Countries), Total: total}
	sortSlices(out.Countries)
	sortSlices(out.EndReasons)

	playlistRows, err := q.Query(ctx, playlistContextSQL,
		store.UUIDArg(userID), r.From.UTC(), r.To.UTC(), clampLimit(0))
	if err != nil {
		return PlaybackContext{}, postgres.Classify("playlist context", err)
	}
	defer playlistRows.Close()
	out.Playlists = []PlaylistContextEntry{}
	for playlistRows.Next() {
		var e PlaylistContextEntry
		if err := playlistRows.Scan(&e.ContextType, &e.ContextID, &e.Name, &e.Plays); err != nil {
			return PlaybackContext{}, postgres.Classify("scan playlist context", err)
		}
		out.Playlists = append(out.Playlists, e)
	}
	if err := playlistRows.Err(); err != nil {
		return PlaybackContext{}, postgres.Classify("playlist context", err)
	}

	var playlistTotal, playlistCovered int64
	if err := q.QueryRow(ctx, playlistContextCoverageSQL,
		store.UUIDArg(userID), r.From.UTC(), r.To.UTC(),
	).Scan(&playlistTotal, &playlistCovered); err != nil {
		return PlaybackContext{}, postgres.Classify("playlist context coverage", err)
	}
	out.PlaylistCoverage = Coverage{Covered: playlistCovered, Total: playlistTotal}

	return out, nil
}

// sortedSlices turns the family tally into a stable descending list.
func sortedSlices(m map[string]int64) []ContextSlice {
	out := make([]ContextSlice, 0, len(m))
	for k, v := range m {
		out = append(out, ContextSlice{Key: k, Plays: v})
	}
	sortSlices(out)
	return out
}

// sortSlices orders a breakdown by plays descending, then by key, so a tie
// renders the same way twice.
func sortSlices(s []ContextSlice) {
	slices.SortFunc(s, func(a, b ContextSlice) int {
		if a.Plays != b.Plays {
			return cmp.Compare(b.Plays, a.Plays)
		}
		return strings.Compare(a.Key, b.Key)
	})
}

func sumSlices(s []ContextSlice) int64 {
	var n int64
	for _, e := range s {
		n += e.Plays
	}
	return n
}
