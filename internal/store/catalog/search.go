package catalog

import (
	"context"
	"strings"

	"github.com/requi/encore/internal/domain"
	"github.com/requi/encore/internal/postgres"
	"github.com/requi/encore/internal/store"
)

// Search limits. The box in the header shows a short list per entity type, and
// the contains arm of the query is not index-driven, so the ceiling is low.
const (
	defaultSearchLimit = 10
	maxSearchLimit     = 50
)

// SearchResults are the catalogue matches for one query, grouped by entity type.
// Each list is already ordered by relevance and is at most the requested limit.
type SearchResults struct {
	Artists []domain.Artist
	Albums  []domain.Album
	Tracks  []domain.Track
}

// Empty reports whether the query matched nothing at all.
func (s SearchResults) Empty() bool {
	return len(s.Artists) == 0 && len(s.Albums) == 0 && len(s.Tracks) == 0
}

// The search statements are built in two stages against the normalised name
// column.
//
// The prefix arm ("beat%") is a range predicate that the text_pattern_ops index
// on name_norm can satisfy directly, which is what keeps type-ahead responsive
// on a catalogue with millions of tracks. The contains arm ("%beat%") cannot use
// that index and has to scan, so it is guarded by NOT EXISTS on the prefix arm:
// Postgres evaluates that once and skips the scan entirely whenever the fast
// path already found something. Mid-word matches therefore still work, but only
// cost anything when nothing better exists.
//
// Ordering by name length puts the tightest match first: searching "beat" ranks
// "Beat" above "Beatles Forever Karaoke Tribute".
const searchArtistsSQL = `
WITH prefix_match AS (
    SELECT id FROM artists
    WHERE name_norm LIKE $1
    ORDER BY length(name_norm), name_norm
    LIMIT $3
),
contains_match AS (
    SELECT id FROM artists
    WHERE NOT EXISTS (SELECT 1 FROM prefix_match) AND name_norm LIKE $2
    ORDER BY length(name_norm), name_norm
    LIMIT $3
),
matched AS (
    SELECT id FROM prefix_match
    UNION
    SELECT id FROM contains_match
)` + artistSelect + `
JOIN matched m ON m.id = a.id
ORDER BY length(a.name_norm), a.name_norm, a.id
LIMIT $3`

const searchAlbumsSQL = `
WITH prefix_match AS (
    SELECT id FROM albums
    WHERE name_norm LIKE $1
    ORDER BY length(name_norm), name_norm
    LIMIT $3
),
contains_match AS (
    SELECT id FROM albums
    WHERE NOT EXISTS (SELECT 1 FROM prefix_match) AND name_norm LIKE $2
    ORDER BY length(name_norm), name_norm
    LIMIT $3
),
matched AS (
    SELECT id FROM prefix_match
    UNION
    SELECT id FROM contains_match
)` + albumSelect + `
JOIN matched m ON m.id = al.id
GROUP BY al.id
ORDER BY length(al.name_norm), al.name_norm, al.id
LIMIT $3`

const searchTracksSQL = `
WITH prefix_match AS (
    SELECT id FROM tracks
    WHERE name_norm LIKE $1
    ORDER BY length(name_norm), name_norm
    LIMIT $3
),
contains_match AS (
    SELECT id FROM tracks
    WHERE NOT EXISTS (SELECT 1 FROM prefix_match) AND name_norm LIKE $2
    ORDER BY length(name_norm), name_norm
    LIMIT $3
),
matched AS (
    SELECT id FROM prefix_match
    UNION
    SELECT id FROM contains_match
)` + trackSelect + `
JOIN matched m ON m.id = t.id
GROUP BY t.id
ORDER BY length(t.name_norm), t.name_norm, t.id
LIMIT $3`

// Search finds artists, albums and tracks whose normalised name matches the
// query. The query is folded with the same normaliser that produced the stored
// name_norm columns, so accents, curly apostrophes and punctuation do not have
// to be typed exactly.
//
// A query that normalises to nothing - punctuation only, or empty - returns no
// results rather than every row in the catalogue.
func (r *Repo) Search(ctx context.Context, q store.Querier, query string, limit int) (SearchResults, error) {
	var out SearchResults

	norm := domain.NormalizeText(query)
	if norm == "" {
		return out, nil
	}
	limit = clampLimit(limit, defaultSearchLimit, maxSearchLimit)

	esc := escapeLike(norm)
	prefix := esc + "%"
	contains := "%" + esc + "%"

	artists, err := searchRows(ctx, q, searchArtistsSQL, "artists", prefix, contains, limit, scanArtist)
	if err != nil {
		return SearchResults{}, err
	}
	albums, err := searchRows(ctx, q, searchAlbumsSQL, "albums", prefix, contains, limit, scanAlbum)
	if err != nil {
		return SearchResults{}, err
	}
	tracks, err := searchRows(ctx, q, searchTracksSQL, "tracks", prefix, contains, limit, scanTrack)
	if err != nil {
		return SearchResults{}, err
	}

	out.Artists, out.Albums, out.Tracks = artists, albums, tracks
	return out, nil
}

// searchRows runs one of the three search statements. The three differ only in
// their projection, so the scan loop is written once and parameterised by the
// row decoder.
func searchRows[T any](ctx context.Context, q store.Querier, sql, what, prefix, contains string, limit int, scan func(scanner) (T, error)) ([]T, error) {
	rows, err := q.Query(ctx, sql, prefix, contains, limit)
	if err != nil {
		return nil, postgres.Classify("search "+what, err)
	}
	defer rows.Close()

	var out []T
	for rows.Next() {
		v, err := scan(rows)
		if err != nil {
			return nil, postgres.Classify("scan searched "+what, err)
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, postgres.Classify("search "+what, err)
	}
	return out, nil
}

// escapeLike neutralises the LIKE metacharacters so that a query typed into the
// search box is matched literally.
//
// domain.NormalizeText already folds '%', '_' and '\' into separators, so today
// this can never fire; it is here so that a future relaxation of the normaliser
// cannot turn the search box into a way to ask for a full table scan.
func escapeLike(s string) string {
	if !strings.ContainsAny(s, `%_\`) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 4)
	for _, r := range s {
		switch r {
		case '%', '_', '\\':
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}
