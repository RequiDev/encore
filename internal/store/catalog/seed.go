package catalog

import (
	"context"

	"github.com/requi/encore/internal/domain"
	"github.com/requi/encore/internal/postgres"
	"github.com/requi/encore/internal/store"
)

// Seeding records the names Encore already knows without claiming an entity has
// been enriched.
//
// A track fetched from Spotify carries simplified artist and album objects: an
// id and a name, but no genres, images or popularity. Recording those rows as
// merely 'pending' and discarding the names — which is what Encore used to do —
// means the interface shows blank artists until a second, separate round of
// requests completes. On a development-mode application that round can be a day
// away, because the daily quota is easily exhausted, and in the meantime every
// listen appears to be by nobody.
//
// So the name is written and the state is left alone. The row stays in the
// enrichment queue and still gets its images and genres later; it simply has
// something to display in the meantime.
//
// A seed never overwrites a name that is already there. Enrichment is the
// authority, and a simplified object must not be able to downgrade a resolved
// one.

const seedArtistsSQL = `
WITH input AS (
    SELECT DISTINCT ON (id) id, name, name_norm
    FROM unnest($1::text[], $2::text[], $3::text[]) AS t(id, name, name_norm)
    WHERE id <> '' AND name <> ''
    ORDER BY id, name
)
INSERT INTO artists (id, name, name_norm, metadata_state)
SELECT id, name, name_norm, 'pending' FROM input
ON CONFLICT (id) DO UPDATE
SET name      = CASE WHEN artists.name = '' THEN excluded.name      ELSE artists.name      END,
    name_norm = CASE WHEN artists.name = '' THEN excluded.name_norm ELSE artists.name_norm END`

// SeedArtists records artist names taken from a simplified object, creating the
// row in the pending state when it is new and never overwriting a name already
// present.
func (r *Repo) SeedArtists(ctx context.Context, q store.Querier, artists []domain.Artist) error {
	ids, names, norms := make([]string, 0, len(artists)), make([]string, 0, len(artists)), make([]string, 0, len(artists))
	for _, a := range artists {
		if a.ID == "" || a.Name == "" {
			continue
		}
		norm := a.NameNorm
		if norm == "" {
			norm = domain.NormalizeArtist(a.Name)
		}
		ids = append(ids, a.ID)
		names = append(names, a.Name)
		norms = append(norms, norm)
	}
	if len(ids) == 0 {
		return nil
	}
	if _, err := q.Exec(ctx, seedArtistsSQL, ids, names, norms); err != nil {
		return postgres.Classify("seed artist names", err)
	}
	return nil
}

const seedAlbumsSQL = `
WITH input AS (
    SELECT DISTINCT ON (id) id, name, name_norm
    FROM unnest($1::text[], $2::text[], $3::text[]) AS t(id, name, name_norm)
    WHERE id <> '' AND name <> ''
    ORDER BY id, name
)
INSERT INTO albums (id, name, name_norm, metadata_state)
SELECT id, name, name_norm, 'pending' FROM input
ON CONFLICT (id) DO UPDATE
SET name      = CASE WHEN albums.name = '' THEN excluded.name      ELSE albums.name      END,
    name_norm = CASE WHEN albums.name = '' THEN excluded.name_norm ELSE albums.name_norm END`

// SeedAlbums is SeedArtists for albums.
func (r *Repo) SeedAlbums(ctx context.Context, q store.Querier, albums []domain.Album) error {
	ids, names, norms := make([]string, 0, len(albums)), make([]string, 0, len(albums)), make([]string, 0, len(albums))
	for _, a := range albums {
		if a.ID == "" || a.Name == "" {
			continue
		}
		norm := a.NameNorm
		if norm == "" {
			norm = domain.NormalizeTitle(a.Name)
		}
		ids = append(ids, a.ID)
		names = append(names, a.Name)
		norms = append(norms, norm)
	}
	if len(ids) == 0 {
		return nil
	}
	if _, err := q.Exec(ctx, seedAlbumsSQL, ids, names, norms); err != nil {
		return postgres.Classify("seed album names", err)
	}
	return nil
}

const seedTrackNamesSQL = `
WITH input AS (
    SELECT DISTINCT ON (id) id, name, name_norm
    FROM unnest($1::text[], $2::text[], $3::text[]) AS t(id, name, name_norm)
    WHERE id <> '' AND name <> ''
    ORDER BY id, name
)
UPDATE tracks t
SET name = i.name, name_norm = i.name_norm
FROM input i
WHERE t.id = i.id AND t.name = ''`

// SeedTrackNames fills in the names of tracks that are still awaiting
// enrichment, from whatever the ingesting source happened to know.
//
// Both Spotify export formats carry the track title beside the URI, so an import
// already knows what almost every one of its tracks is called. Storing that at
// ingest time means a freshly imported history is readable immediately rather
// than only after the catalogue queue drains.
//
// It updates rather than inserts: the row is created by the ingest path, which
// owns the foreign key that listens depend on.
func (r *Repo) SeedTrackNames(ctx context.Context, q store.Querier, tracks []domain.Track) error {
	ids, names, norms := make([]string, 0, len(tracks)), make([]string, 0, len(tracks)), make([]string, 0, len(tracks))
	for _, t := range tracks {
		if t.ID == "" || t.Name == "" {
			continue
		}
		norm := t.NameNorm
		if norm == "" {
			norm = domain.NormalizeTitle(t.Name)
		}
		ids = append(ids, t.ID)
		names = append(names, t.Name)
		norms = append(norms, norm)
	}
	if len(ids) == 0 {
		return nil
	}
	if _, err := q.Exec(ctx, seedTrackNamesSQL, ids, names, norms); err != nil {
		return postgres.Classify("seed track names", err)
	}
	return nil
}
