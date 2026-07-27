package catalog

import (
	"context"

	"github.com/RequiDev/encore/internal/postgres"
	"github.com/RequiDev/encore/internal/store"
)

// MergeLocalArtists folds artists an import named into the Spotify artists that
// turn out to be the same person.
//
// An import can only key an artist by name, so it mints a local row. Enrichment
// later resolves some of that artist's tracks and gets a real Spotify id for
// them. Without this, both rows survive: the same name appears twice in every
// chart with the plays split between them, and the split grows more confusing
// the further enrichment gets.
//
// Matching is on the normalised name, which is the only thing the two rows have
// in common — it is exactly the key the local id was derived from, so a match
// here means the import would have produced this id for this artist.
//
// The local row's credits are copied to the real one and the row is deleted; its
// remaining links go with it by cascade. Hidden-artist choices move too, because
// a user who hid an artist meant the artist and not the row.
//
// Rollups are keyed by track rather than by artist, so nothing needs
// recomputing: the listens never moved.
//
// Returns how many local rows were folded away.
func (r *Repo) MergeLocalArtists(ctx context.Context, q store.Querier, resolvedIDs []string) (int64, error) {
	if len(resolvedIDs) == 0 {
		return 0, nil
	}
	var merged int64
	if err := q.QueryRow(ctx, mergeLocalArtistsSQL, resolvedIDs).Scan(&merged); err != nil {
		return 0, postgres.Classify("merge local artists", err)
	}
	return merged, nil
}

// mergeLocalArtistsSQL does the whole fold in one statement.
//
// victims picks one target per local row with DISTINCT ON, because two Spotify
// artists can normalise to the same name — "Prince" and "PRINCE" — and a local
// row must be folded into exactly one of them rather than duplicated into both.
const mergeLocalArtistsSQL = `
WITH resolved AS (
    SELECT id, name_norm FROM artists
    WHERE id = ANY($1::text[]) AND metadata_state = 'resolved' AND name_norm <> ''
),
victims AS (
    SELECT DISTINCT ON (a.id) a.id AS local_id, r.id AS real_id
    FROM artists a
    JOIN resolved r ON r.name_norm = a.name_norm
    WHERE a.metadata_state = 'local' AND a.id <> r.id
    ORDER BY a.id, r.id
),
moved_tracks AS (
    INSERT INTO track_artists (track_id, artist_id, position)
    SELECT ta.track_id, v.real_id, ta.position
    FROM track_artists ta JOIN victims v ON v.local_id = ta.artist_id
    ON CONFLICT DO NOTHING
    RETURNING 1
),
moved_albums AS (
    INSERT INTO album_artists (album_id, artist_id, position)
    SELECT aa.album_id, v.real_id, aa.position
    FROM album_artists aa JOIN victims v ON v.local_id = aa.artist_id
    ON CONFLICT DO NOTHING
    RETURNING 1
),
moved_hidden AS (
    INSERT INTO user_blacklisted_artists (user_id, artist_id, created_at)
    SELECT b.user_id, v.real_id, b.created_at
    FROM user_blacklisted_artists b JOIN victims v ON v.local_id = b.artist_id
    ON CONFLICT DO NOTHING
    RETURNING 1
),
deleted AS (
    DELETE FROM artists
    WHERE id IN (SELECT local_id FROM victims)
    RETURNING 1
)
SELECT count(*) FROM deleted`

// DeleteOrphanedLocalAlbums removes local albums nothing points at any more.
//
// Albums need no merge: a track that resolves gets its real album id from the
// upsert, which moves it off the local row on its own. What is left behind is an
// album with no tracks, which would otherwise sit in the catalogue for ever
// looking like a real one with nothing on it.
//
// Only local rows are considered. A Spotify album with no tracks yet is normal —
// enrichment simply has not reached them.
func (r *Repo) DeleteOrphanedLocalAlbums(ctx context.Context, q store.Querier) (int64, error) {
	tag, err := q.Exec(ctx, `
        DELETE FROM albums a
        WHERE a.metadata_state = 'local'
          AND NOT EXISTS (SELECT 1 FROM tracks t WHERE t.album_id = a.id)`)
	if err != nil {
		return 0, postgres.Classify("delete orphaned local albums", err)
	}
	return tag.RowsAffected(), nil
}
