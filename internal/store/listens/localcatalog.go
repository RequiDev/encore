package listens

import (
	"context"

	"github.com/RequiDev/encore/internal/domain"
	"github.com/RequiDev/encore/internal/postgres"
	"github.com/RequiDev/encore/internal/store"
)

// EnsureLocalCatalogue records the artists and albums an import named but did
// not identify.
//
// Both Spotify export formats print the artist and the album of every play and
// give an id for neither. Those names used to be parsed and dropped, so a
// history with three and a half thousand artists in it produced a catalogue with
// none: every artist page, every top-artist chart and every "Unknown artist" in
// a session waited on the Spotify API, and on a development-mode application
// whose daily quota is gone, waited indefinitely.
//
// The rows written here are marked 'local', which the enrichment queues do not
// claim. They are a floor, never a ceiling:
//
//   - A track that already has artists keeps them. Enrichment's answer is the
//     authoritative one and a seed must not be able to overwrite it.
//   - A track that already has an album keeps it, for the same reason.
//   - A name is only ever written into an empty one.
//
// Called inside the import transaction, so a batch's listens and the catalogue
// rows they point at commit together or not at all.
func (r *Repo) EnsureLocalCatalogue(ctx context.Context, q store.Querier, seeds []TrackSeed) error {
	trackIDs := make([]string, 0, len(seeds))
	artistIDs := make([]string, 0, len(seeds))
	artistNames := make([]string, 0, len(seeds))
	artistNorms := make([]string, 0, len(seeds))
	albumIDs := make([]string, 0, len(seeds))
	albumNames := make([]string, 0, len(seeds))
	albumNorms := make([]string, 0, len(seeds))

	for _, t := range seeds {
		if t.ID == "" {
			continue
		}
		artistID := domain.LocalArtistID(t.ArtistName)
		albumID := domain.LocalAlbumID(t.ArtistName, t.AlbumName)
		if artistID == "" && albumID == "" {
			continue
		}
		trackIDs = append(trackIDs, t.ID)
		artistIDs = append(artistIDs, artistID)
		artistNames = append(artistNames, t.ArtistName)
		artistNorms = append(artistNorms, domain.NormalizeArtist(t.ArtistName))
		albumIDs = append(albumIDs, albumID)
		albumNames = append(albumNames, t.AlbumName)
		albumNorms = append(albumNorms, domain.NormalizeTitle(t.AlbumName))
	}
	if len(trackIDs) == 0 {
		return nil
	}

	if _, err := q.Exec(ctx, ensureLocalCatalogueSQL,
		trackIDs, artistIDs, artistNames, artistNorms, albumIDs, albumNames, albumNorms); err != nil {
		return postgres.Classify("ensure local catalogue", err)
	}
	return nil
}

// ensureLocalCatalogueSQL writes the artists, the albums and the links in one
// statement.
//
// The order is forced by the foreign keys — an album must exist before a track
// points at it, an artist before a link names it — and the whole thing is one
// round trip because it runs once per import batch.
const ensureLocalCatalogueSQL = `
WITH input AS (
    SELECT * FROM unnest(
        $1::text[], $2::text[], $3::text[], $4::text[], $5::text[], $6::text[], $7::text[]
    ) AS t(track_id, artist_id, artist_name, artist_norm, album_id, album_name, album_norm)
),
new_artists AS (
    INSERT INTO artists (id, name, name_norm, metadata_state)
    SELECT DISTINCT ON (artist_id) artist_id, artist_name, artist_norm, 'local'
    FROM input WHERE artist_id <> ''
    ORDER BY artist_id
    ON CONFLICT (id) DO UPDATE
    SET name      = CASE WHEN artists.name = '' THEN excluded.name      ELSE artists.name      END,
        name_norm = CASE WHEN artists.name = '' THEN excluded.name_norm ELSE artists.name_norm END
    RETURNING id
),
new_albums AS (
    INSERT INTO albums (id, name, name_norm, metadata_state)
    SELECT DISTINCT ON (album_id) album_id, album_name, album_norm, 'local'
    FROM input WHERE album_id <> ''
    ORDER BY album_id
    ON CONFLICT (id) DO UPDATE
    SET name      = CASE WHEN albums.name = '' THEN excluded.name      ELSE albums.name      END,
        name_norm = CASE WHEN albums.name = '' THEN excluded.name_norm ELSE albums.name_norm END
    RETURNING id
),
-- An album's own credit, so an album page can name its artist without Spotify.
album_credits AS (
    INSERT INTO album_artists (album_id, artist_id, position)
    SELECT DISTINCT ON (album_id, artist_id) album_id, artist_id, 0
    FROM input WHERE album_id <> '' AND artist_id <> ''
    ORDER BY album_id, artist_id
    ON CONFLICT DO NOTHING
    RETURNING album_id
),
-- Only for tracks nobody has credited yet: enrichment's answer wins, and a
-- re-import must not add a local artist beside the real one.
track_credits AS (
    INSERT INTO track_artists (track_id, artist_id, position)
    SELECT DISTINCT ON (i.track_id) i.track_id, i.artist_id, 0
    FROM input i
    WHERE i.artist_id <> ''
      -- The track has to exist: a backfill reads whole export files, including
      -- plays too short to have been stored, and a credit for a track that was
      -- never imported would fail the foreign key and take the batch with it.
      AND EXISTS (SELECT 1 FROM tracks t WHERE t.id = i.track_id)
      AND NOT EXISTS (SELECT 1 FROM track_artists ta WHERE ta.track_id = i.track_id)
    ORDER BY i.track_id
    ON CONFLICT DO NOTHING
    RETURNING track_id
)
-- Likewise the album: filled in only where the track has none.
UPDATE tracks t
SET album_id = i.album_id
FROM (SELECT DISTINCT ON (track_id) track_id, album_id FROM input WHERE album_id <> '' ORDER BY track_id) i
WHERE t.id = i.track_id AND t.album_id IS NULL`
