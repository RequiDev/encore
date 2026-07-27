package catalog

import (
	"context"
	"fmt"
	"strings"

	"github.com/RequiDev/encore/internal/domain"
	"github.com/RequiDev/encore/internal/postgres"
	"github.com/RequiDev/encore/internal/store"
)

// genreSep joins an artist's genres into one text value for transport.
//
// Postgres cannot unnest a nested array one row at a time, so a set-based upsert
// cannot carry text[] per row. The genres are therefore shipped as one delimited
// string per artist and split again in SQL. The unit separator is used because
// no Spotify genre name has ever contained a control character.
const genreSep = "\x1f"

// artistSelect reads a whole artist row. Artists have no child rows to
// aggregate, so this is a plain projection.
const artistSelect = `
SELECT a.id, a.name, a.name_norm, a.genres, a.popularity, a.followers, a.image_url,
       a.metadata_state, a.fetch_attempts, a.next_attempt_at, a.fetched_at
FROM artists a`

func scanArtist(s scanner) (domain.Artist, error) {
	var (
		a     domain.Artist
		state string
	)
	if err := s.Scan(&a.ID, &a.Name, &a.NameNorm, &a.Genres, &a.Popularity, &a.Followers,
		&a.ImageURL, &state, &a.FetchAttempts, &a.NextAttemptAt, &a.FetchedAt); err != nil {
		return domain.Artist{}, err
	}
	a.MetadataState = metadataState(state)
	return a, nil
}

// GetArtist reads one artist. It returns domain.ErrNotFound when the id is not
// in the catalogue at all, which is distinct from an id that is present but
// still pending enrichment.
func (r *Repo) GetArtist(ctx context.Context, q store.Querier, id string) (domain.Artist, error) {
	if id == "" {
		return domain.Artist{}, fmt.Errorf("%w: artist id is required", domain.ErrValidation)
	}
	const sql = artistSelect + ` WHERE a.id = $1`
	a, err := scanArtist(q.QueryRow(ctx, sql, id))
	if err != nil {
		return domain.Artist{}, postgres.Classify("get artist", err)
	}
	return a, nil
}

// GetArtists reads many artists at once, keyed by id. Ids that are not in the
// catalogue are simply absent from the map, so the caller decides whether a miss
// is an error.
func (r *Repo) GetArtists(ctx context.Context, q store.Querier, ids []string) (map[string]domain.Artist, error) {
	ids = dedupeIDs(ids)
	if len(ids) == 0 {
		return map[string]domain.Artist{}, nil
	}
	const sql = artistSelect + ` WHERE a.id = ANY($1::text[])`
	rows, err := q.Query(ctx, sql, ids)
	if err != nil {
		return nil, postgres.Classify("get artists", err)
	}
	defer rows.Close()

	out := make(map[string]domain.Artist, len(ids))
	for rows.Next() {
		a, err := scanArtist(rows)
		if err != nil {
			return nil, postgres.Classify("scan artist", err)
		}
		out[a.ID] = a
	}
	if err := rows.Err(); err != nil {
		return nil, postgres.Classify("get artists", err)
	}
	return out, nil
}

// upsertArtistsSQL writes a whole batch in one statement.
//
// Duplicate ids are collapsed first: Postgres refuses to let ON CONFLICT touch
// the same row twice inside one statement, and a batch assembled from several
// tracks' artist lists routinely repeats one.
const upsertArtistsSQL = `
WITH input AS (
    SELECT DISTINCT ON (id) *
    FROM unnest($1::text[], $2::text[], $3::text[], $4::text[], $5::int[], $6::bigint[], $7::text[])
        AS t(id, name, name_norm, genres, popularity, followers, image_url)
    ORDER BY id
)
INSERT INTO artists (
    id, name, name_norm, genres, popularity, followers, image_url,
    metadata_state, fetch_attempts, next_attempt_at, fetched_at)
SELECT id, name, name_norm,
       CASE WHEN genres = '' THEN '{}'::text[] ELSE string_to_array(genres, E'\x1f') END,
       popularity, followers, image_url,
       'resolved', 0, NULL, now()
FROM input
ON CONFLICT (id) DO UPDATE SET
    name            = EXCLUDED.name,
    name_norm       = EXCLUDED.name_norm,
    genres          = EXCLUDED.genres,
    popularity      = EXCLUDED.popularity,
    followers       = EXCLUDED.followers,
    image_url       = EXCLUDED.image_url,
    metadata_state  = 'resolved',
    fetch_attempts  = 0,
    next_attempt_at = NULL,
    fetched_at      = now()`

// UpsertArtists records fetched artist metadata and takes the rows out of the
// enrichment queue: the state becomes resolved, the attempt counter is cleared
// and next_attempt_at is dropped, so the partial queue index no longer covers
// them.
func (r *Repo) UpsertArtists(ctx context.Context, q store.Querier, artists []domain.Artist) error {
	rows := buildArtistRows(artists)
	if len(rows.ids) == 0 {
		return nil
	}
	_, err := q.Exec(ctx, upsertArtistsSQL,
		rows.ids, rows.names, rows.norms, rows.genres,
		rows.popularity, rows.followers, rows.images)
	if err != nil {
		return postgres.Classify("upsert artists", err)
	}
	return nil
}

// artistRows is the batch transposed into the parallel arrays the upsert takes.
type artistRows struct {
	ids        []string
	names      []string
	norms      []string
	genres     []string
	images     []string
	popularity []int32
	followers  []int64
}

// buildArtistRows transposes a batch and normalises as it goes.
//
// name_norm is always derived here rather than trusted from the caller, because
// the search index and the alias resolver both compare against it and would
// silently stop matching if two writers disagreed about how to fold a name.
func buildArtistRows(artists []domain.Artist) artistRows {
	out := artistRows{
		ids:        make([]string, 0, len(artists)),
		names:      make([]string, 0, len(artists)),
		norms:      make([]string, 0, len(artists)),
		genres:     make([]string, 0, len(artists)),
		images:     make([]string, 0, len(artists)),
		popularity: make([]int32, 0, len(artists)),
		followers:  make([]int64, 0, len(artists)),
	}
	for _, a := range artists {
		if a.ID == "" {
			continue
		}
		out.ids = append(out.ids, a.ID)
		out.names = append(out.names, a.Name)
		out.norms = append(out.norms, domain.NormalizeArtist(a.Name))
		out.genres = append(out.genres, joinGenres(a.Genres))
		out.images = append(out.images, a.ImageURL)
		out.popularity = append(out.popularity, a.Popularity)
		out.followers = append(out.followers, a.Followers)
	}
	return out
}

// joinGenres renders a genre list for transport, dropping blanks so the split on
// the other side cannot produce an empty element.
func joinGenres(genres []string) string {
	if len(genres) == 0 {
		return ""
	}
	kept := make([]string, 0, len(genres))
	for _, g := range genres {
		g = strings.TrimSpace(g)
		if g == "" {
			continue
		}
		kept = append(kept, strings.ReplaceAll(g, genreSep, " "))
	}
	return strings.Join(kept, genreSep)
}
