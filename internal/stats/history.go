package stats

import (
	"context"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/RequiDev/encore/internal/domain"
	"github.com/RequiDev/encore/internal/postgres"
	"github.com/RequiDev/encore/internal/store"
)

// HistoryEntry is one row of the raw listening feed.
type HistoryEntry struct {
	ID          int64
	PlayedAt    time.Time
	TrackID     string
	AliasArtist string
	AliasTitle  string
	MsPlayed    int32
	Source      domain.Source
	Platform    string
	ConnCountry string
	ReasonStart string
	ReasonEnd   string
	Shuffle     *bool
	Skipped     *bool
	Offline     *bool
	Incognito   *bool
}

// HistoryPage is one page of the feed. NextCursor is empty when the page is the
// last one.
type HistoryPage struct {
	Entries    []HistoryEntry
	NextCursor string
}

// Cursor is the keyset position of the history feed: the (played_at, id) pair of
// the last row already delivered.
//
// The feed is paginated on that pair rather than with OFFSET because a user may
// hold millions of listens, and OFFSET makes the database walk and discard every
// skipped row; the keyset predicate seeks straight into the
// (user_id, played_at) index instead. It also means a listen arriving mid-scroll
// cannot shift the rows below it.
type Cursor struct {
	PlayedAt time.Time
	ID       int64
}

// cursorVersion prefixes the encoded form so a future change of shape can be
// rejected rather than misread.
const cursorVersion = "v1"

// IsZero reports whether the cursor addresses the first page.
func (c Cursor) IsZero() bool { return c.ID == 0 && c.PlayedAt.IsZero() }

// Encode renders the cursor as an opaque, URL-safe token. Callers must treat the
// result as meaningless text: its contents are Encore's business, and the format
// is free to change behind the version prefix.
func (c Cursor) Encode() string {
	if c.IsZero() {
		return ""
	}
	raw := fmt.Sprintf("%s:%d:%d", cursorVersion, c.PlayedAt.UTC().UnixMicro(), c.ID)
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// DecodeCursor parses a token produced by Encode. An empty string is the first
// page rather than an error, because that is what an omitted query parameter
// means.
func DecodeCursor(s string) (Cursor, error) {
	if s == "" {
		return Cursor{}, nil
	}
	bad := fmt.Errorf("%w: malformed history cursor", domain.ErrValidation)
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return Cursor{}, bad
	}
	parts := strings.Split(string(raw), ":")
	if len(parts) != 3 || parts[0] != cursorVersion {
		return Cursor{}, bad
	}
	micros, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return Cursor{}, bad
	}
	id, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil || id <= 0 {
		return Cursor{}, bad
	}
	return Cursor{PlayedAt: time.UnixMicro(micros).UTC(), ID: id}, nil
}

const historyColumns = `l.id, l.played_at, l.track_id, l.alias_artist, l.alias_title,
    l.ms_played, l.source, l.platform, l.conn_country, l.reason_start, l.reason_end,
    l.shuffle, l.skipped, l.offline, l.incognito`

// The first page and subsequent pages are separate statements rather than one
// statement with a nullable cursor: an OR over a NULL parameter would cost the
// planner the index seek that is the entire point of keyset pagination.
var (
	historyFirstPageSQL = fmt.Sprintf(`
SELECT %s
FROM listens l
WHERE %s
ORDER BY l.played_at DESC, l.id DESC
LIMIT $4`, historyColumns, rangeFilter("l", "$1", "$2", "$3"))

	historyNextPageSQL = fmt.Sprintf(`
SELECT %s
FROM listens l
WHERE %s AND (l.played_at, l.id) < ($4::timestamptz, $5::bigint)
ORDER BY l.played_at DESC, l.id DESC
LIMIT $6`, historyColumns, rangeFilter("l", "$1", "$2", "$3"))
)

// History returns the raw listening feed, newest first, keyset paginated.
//
// cursor is the opaque token from the previous page, or empty for the first one.
// Timestamps come back in the user's timezone; the cursor itself is absolute, so
// changing timezone mid-scroll cannot skip or repeat a row.
func (s *Service) History(ctx context.Context, q store.Querier, userID uuid.UUID, r domain.TimeRange, tz, cursor string, limit int) (HistoryPage, error) {
	loc, err := scope(userID, r, tz)
	if err != nil {
		return HistoryPage{}, err
	}
	cur, err := DecodeCursor(cursor)
	if err != nil {
		return HistoryPage{}, err
	}
	limit = clampLimit(limit)

	// One row beyond the page is fetched to learn whether another page exists,
	// which is cheaper and more honest than counting the whole feed.
	sql, args := historyFirstPageSQL, []any{
		store.UUIDArg(userID), r.From.UTC(), r.To.UTC(), limit + 1,
	}
	if !cur.IsZero() {
		sql, args = historyNextPageSQL, []any{
			store.UUIDArg(userID), r.From.UTC(), r.To.UTC(),
			cur.PlayedAt.UTC(), cur.ID, limit + 1,
		}
	}

	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return HistoryPage{}, postgres.Classify("listening history", err)
	}
	defer rows.Close()

	var out HistoryPage
	for rows.Next() {
		var (
			e                                         HistoryEntry
			source                                    int16
			trackID, aliasArtist, aliasTitle          *string
			platform, country, reasonStart, reasonEnd *string
		)
		if err := rows.Scan(&e.ID, &e.PlayedAt, &trackID, &aliasArtist, &aliasTitle,
			&e.MsPlayed, &source, &platform, &country, &reasonStart, &reasonEnd,
			&e.Shuffle, &e.Skipped, &e.Offline, &e.Incognito); err != nil {
			return HistoryPage{}, postgres.Classify("scan history row", err)
		}
		e.Source = domain.Source(source)
		e.TrackID = store.Deref(trackID)
		e.AliasArtist = store.Deref(aliasArtist)
		e.AliasTitle = store.Deref(aliasTitle)
		e.Platform = store.Deref(platform)
		e.ConnCountry = store.Deref(country)
		e.ReasonStart = store.Deref(reasonStart)
		e.ReasonEnd = store.Deref(reasonEnd)
		out.Entries = append(out.Entries, e)
	}
	if err := rows.Err(); err != nil {
		return HistoryPage{}, postgres.Classify("listening history", err)
	}

	if len(out.Entries) > limit {
		last := out.Entries[limit-1]
		out.Entries = out.Entries[:limit]
		out.NextCursor = Cursor{PlayedAt: last.PlayedAt, ID: last.ID}.Encode()
	}
	for i := range out.Entries {
		out.Entries[i].PlayedAt = out.Entries[i].PlayedAt.In(loc)
	}
	return out, nil
}
