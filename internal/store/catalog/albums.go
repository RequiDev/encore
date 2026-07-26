package catalog

import (
	"context"
	"fmt"
	"time"

	"github.com/requi/encore/internal/domain"
	"github.com/requi/encore/internal/postgres"
	"github.com/requi/encore/internal/store"
)

// albumSelect reads a whole album row plus its artist ids in credit order. The
// aggregate is filtered so an album with no known artists yields an empty array
// rather than a one-element array of NULL.
const albumSelect = `
SELECT al.id, al.name, al.name_norm, al.album_type, al.release_date, al.release_precision,
       al.total_tracks, al.image_url, al.metadata_state, al.fetch_attempts,
       al.next_attempt_at, al.fetched_at,
       coalesce(array_agg(aa.artist_id ORDER BY aa.position, aa.artist_id)
                FILTER (WHERE aa.artist_id IS NOT NULL), '{}')
FROM albums al
LEFT JOIN album_artists aa ON aa.album_id = al.id`

func scanAlbum(s scanner) (domain.Album, error) {
	var (
		a     domain.Album
		state string
	)
	if err := s.Scan(&a.ID, &a.Name, &a.NameNorm, &a.AlbumType, &a.ReleaseDate,
		&a.ReleasePrecision, &a.TotalTracks, &a.ImageURL, &state, &a.FetchAttempts,
		&a.NextAttemptAt, &a.FetchedAt, &a.ArtistIDs); err != nil {
		return domain.Album{}, err
	}
	a.MetadataState = metadataState(state)
	return a, nil
}

// GetAlbum reads one album with its artist ids populated.
func (r *Repo) GetAlbum(ctx context.Context, q store.Querier, id string) (domain.Album, error) {
	if id == "" {
		return domain.Album{}, fmt.Errorf("%w: album id is required", domain.ErrValidation)
	}
	const sql = albumSelect + ` WHERE al.id = $1 GROUP BY al.id`
	a, err := scanAlbum(q.QueryRow(ctx, sql, id))
	if err != nil {
		return domain.Album{}, postgres.Classify("get album", err)
	}
	return a, nil
}

// GetAlbums reads many albums at once, keyed by id. Unknown ids are absent from
// the map.
func (r *Repo) GetAlbums(ctx context.Context, q store.Querier, ids []string) (map[string]domain.Album, error) {
	ids = dedupeIDs(ids)
	if len(ids) == 0 {
		return map[string]domain.Album{}, nil
	}
	const sql = albumSelect + ` WHERE al.id = ANY($1::text[]) GROUP BY al.id`
	rows, err := q.Query(ctx, sql, ids)
	if err != nil {
		return nil, postgres.Classify("get albums", err)
	}
	defer rows.Close()

	out := make(map[string]domain.Album, len(ids))
	for rows.Next() {
		a, err := scanAlbum(rows)
		if err != nil {
			return nil, postgres.Classify("scan album", err)
		}
		out[a.ID] = a
	}
	if err := rows.Err(); err != nil {
		return nil, postgres.Classify("get albums", err)
	}
	return out, nil
}

// upsertAlbumsSQL writes a batch of albums and creates a pending row for every
// artist they credit.
//
// The artist rows are created here, and not only in ReplaceAlbumArtists, because
// an album's credits are learned in the same response as the album itself; the
// pending rows put those artists in front of the artist worker immediately
// instead of waiting for the link table to be written.
const upsertAlbumsSQL = `
WITH input AS (
    SELECT DISTINCT ON (id) *
    FROM unnest($1::text[], $2::text[], $3::text[], $4::text[], $5::date[], $6::text[],
                $7::int[], $8::text[])
        AS t(id, name, name_norm, album_type, release_date, release_precision,
             total_tracks, image_url)
    ORDER BY id
),
ensure_artists AS (
    INSERT INTO artists (id, metadata_state)
    SELECT DISTINCT a, 'pending' FROM unnest($9::text[]) AS u(a)
    ON CONFLICT (id) DO NOTHING
)
INSERT INTO albums (
    id, name, name_norm, album_type, release_date, release_precision, total_tracks,
    image_url, metadata_state, fetch_attempts, next_attempt_at, fetched_at)
SELECT id, name, name_norm, album_type, release_date, release_precision, total_tracks,
       image_url, 'resolved', 0, NULL, now()
FROM input
ON CONFLICT (id) DO UPDATE SET
    name              = EXCLUDED.name,
    name_norm         = EXCLUDED.name_norm,
    album_type        = EXCLUDED.album_type,
    release_date      = EXCLUDED.release_date,
    release_precision = EXCLUDED.release_precision,
    total_tracks      = EXCLUDED.total_tracks,
    image_url         = EXCLUDED.image_url,
    metadata_state    = 'resolved',
    fetch_attempts    = 0,
    next_attempt_at   = NULL,
    fetched_at        = now()`

// UpsertAlbums records fetched album metadata and takes the rows out of the
// enrichment queue. The credited artists are ensured as pending rows; the link
// table itself is written by ReplaceAlbumArtists.
func (r *Repo) UpsertAlbums(ctx context.Context, q store.Querier, albums []domain.Album) error {
	rows := buildAlbumRows(albums)
	if len(rows.ids) == 0 {
		return nil
	}
	_, err := q.Exec(ctx, upsertAlbumsSQL,
		rows.ids, rows.names, rows.norms, rows.albumTypes, rows.releaseDates,
		rows.precisions, rows.totalTracks, rows.images, rows.artistIDs)
	if err != nil {
		return postgres.Classify("upsert albums", err)
	}
	return nil
}

// albumRows is the batch transposed into the parallel arrays the upsert takes.
type albumRows struct {
	ids          []string
	names        []string
	norms        []string
	albumTypes   []string
	precisions   []string
	images       []string
	releaseDates []*time.Time
	totalTracks  []int32
	// artistIDs is the flattened set of credited artists, used only to create
	// their pending rows.
	artistIDs []string
}

// buildAlbumRows transposes a batch, deriving name_norm with the same folding
// the search index and the alias resolver use.
func buildAlbumRows(albums []domain.Album) albumRows {
	out := albumRows{
		ids:          make([]string, 0, len(albums)),
		names:        make([]string, 0, len(albums)),
		norms:        make([]string, 0, len(albums)),
		albumTypes:   make([]string, 0, len(albums)),
		precisions:   make([]string, 0, len(albums)),
		images:       make([]string, 0, len(albums)),
		releaseDates: make([]*time.Time, 0, len(albums)),
		totalTracks:  make([]int32, 0, len(albums)),
		artistIDs:    make([]string, 0, len(albums)),
	}
	seenArtist := make(map[string]struct{})
	for _, a := range albums {
		if a.ID == "" {
			continue
		}
		out.ids = append(out.ids, a.ID)
		out.names = append(out.names, a.Name)
		out.norms = append(out.norms, domain.NormalizeTitle(a.Name))
		out.albumTypes = append(out.albumTypes, a.AlbumType)
		out.precisions = append(out.precisions, a.ReleasePrecision)
		out.images = append(out.images, a.ImageURL)
		out.totalTracks = append(out.totalTracks, a.TotalTracks)
		if a.ReleaseDate != nil {
			d := a.ReleaseDate.UTC()
			out.releaseDates = append(out.releaseDates, &d)
		} else {
			out.releaseDates = append(out.releaseDates, nil)
		}
		for _, id := range a.ArtistIDs {
			if id == "" {
				continue
			}
			if _, dup := seenArtist[id]; dup {
				continue
			}
			seenArtist[id] = struct{}{}
			out.artistIDs = append(out.artistIDs, id)
		}
	}
	return out
}

// replaceAlbumArtistsSQL rewrites one album's credits in a single statement.
//
// The delete and the insert cover disjoint sets of rows, which is what makes it
// safe to run both against the same table inside one statement.
const replaceAlbumArtistsSQL = `
WITH ensure_artists AS (
    INSERT INTO artists (id, metadata_state)
    SELECT DISTINCT a, 'pending' FROM unnest($2::text[]) AS u(a)
    ON CONFLICT (id) DO NOTHING
),
ensure_album AS (
    INSERT INTO albums (id, metadata_state) VALUES ($1, 'pending')
    ON CONFLICT (id) DO NOTHING
),
stale AS (
    DELETE FROM album_artists
    WHERE album_id = $1 AND artist_id <> ALL($2::text[])
)
INSERT INTO album_artists (album_id, artist_id, position)
SELECT $1, a, pos - 1
FROM unnest($2::text[]) WITH ORDINALITY AS t(a, pos)
ON CONFLICT (album_id, artist_id) DO UPDATE SET position = EXCLUDED.position`

// ReplaceAlbumArtists makes artistIDs the album's complete credit list, in the
// given order, removing any credit that is no longer present.
//
// Both the album and the artists are created as pending rows first, so a caller
// that has only seen the ids and not yet the metadata cannot trip a foreign key.
func (r *Repo) ReplaceAlbumArtists(ctx context.Context, q store.Querier, albumID string, artistIDs []string) error {
	if albumID == "" {
		return fmt.Errorf("%w: album id is required", domain.ErrValidation)
	}
	ids := dedupeIDs(artistIDs)
	if ids == nil {
		// An explicit empty list still has to reach the statement so the stale
		// credits are removed; a nil slice would encode as SQL NULL.
		ids = []string{}
	}
	if _, err := q.Exec(ctx, replaceAlbumArtistsSQL, albumID, ids); err != nil {
		return postgres.Classify("replace album artists", err)
	}
	return nil
}
