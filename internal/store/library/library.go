// Package library reconciles what Spotify's account enumeration returned
// against what Encore has stored: which tracks and albums a listener has
// saved, and which artists they follow.
//
// Spotify has no "what changed" feed for any of these — only a full listing —
// so every sync reconciles the whole set against what is already here. An
// incoming id is upserted; a stored id the incoming set no longer contains is
// deleted. Nothing is ever truncated first: a truncate-then-insert would,
// however briefly, make a real library look empty to a concurrent reader — a
// statistics query running mid-sync would see nothing saved rather than
// either the old set or the new one.
//
// Every method takes an explicit store.Querier rather than opening its own
// transaction, so the worker can run all three replacements and the
// library_synced_at watermark update inside one Store.InTx: if any step
// fails, the whole run commits nothing.
package library

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/RequiDev/encore/internal/postgres"
	"github.com/RequiDev/encore/internal/store"
)

// Repo reconciles a user's saved tracks, saved albums and followed artists.
type Repo struct{ db *store.Store }

// New builds the repository.
func New(db *store.Store) *Repo { return &Repo{db: db} }

// SavedItem is one entry of a saved-tracks or saved-albums listing: the
// catalogue id together with when Spotify says it was saved.
//
// AddedAt is a pointer because the column is nullable: Spotify's endpoints
// currently always report it, but the schema does not depend on that holding
// forever, and a future absent value must not be forced into some made-up
// time.
type SavedItem struct {
	ID      string
	AddedAt *time.Time
}

// Counts is how many rows each of a user's three library tables holds.
type Counts struct {
	SavedTracks     int64
	SavedAlbums     int64
	FollowedArtists int64
}

// ---- saved tracks -----------------------------------------------------------

// replaceSavedTracksSQL deletes whatever the incoming set no longer contains
// and upserts the rest, in one statement.
//
// The delete and the upsert cover disjoint rows — a track_id is either in the
// incoming set or it is not — so running both against the same table in one
// statement is safe even though Postgres does not order the two CTEs relative
// to each other.
//
// DISTINCT ON collapses a duplicate id within one call: Postgres refuses to
// let ON CONFLICT touch the same row twice inside a single statement, and
// pages of the same enumeration could in principle repeat an id at a
// boundary.
const replaceSavedTracksSQL = `
WITH input AS (
    SELECT DISTINCT ON (track_id) *
    FROM unnest($2::text[], $3::timestamptz[]) AS t(track_id, added_at)
    ORDER BY track_id
),
stale AS (
    DELETE FROM user_saved_tracks
    WHERE user_id = $1 AND track_id <> ALL($2::text[])
)
INSERT INTO user_saved_tracks (user_id, track_id, added_at)
SELECT $1, track_id, added_at FROM input
ON CONFLICT (user_id, track_id) DO UPDATE SET added_at = EXCLUDED.added_at`

// ReplaceSavedTracks makes items the user's complete set of saved tracks.
//
// This is delete-absent-plus-upsert-present, not truncate-then-insert: a
// concurrent reader must never be able to observe the library as empty
// partway through a sync. Passing an empty items removes every row — an
// emptied library is a real, representable state, distinct from a user who
// has never been synced at all (that distinction lives in
// spotify_credentials.library_synced_at, not in whether these tables hold
// rows).
func (r *Repo) ReplaceSavedTracks(ctx context.Context, q store.Querier, userID uuid.UUID, items []SavedItem) error {
	ids, addedAt := savedItemRows(items)
	if _, err := q.Exec(ctx, replaceSavedTracksSQL, store.UUIDArg(userID), ids, addedAt); err != nil {
		return postgres.Classify("replace saved tracks", err)
	}
	return nil
}

// ---- saved albums -----------------------------------------------------------

// replaceSavedAlbumsSQL is ReplaceSavedTracks's statement, over the albums
// table.
const replaceSavedAlbumsSQL = `
WITH input AS (
    SELECT DISTINCT ON (album_id) *
    FROM unnest($2::text[], $3::timestamptz[]) AS t(album_id, added_at)
    ORDER BY album_id
),
stale AS (
    DELETE FROM user_saved_albums
    WHERE user_id = $1 AND album_id <> ALL($2::text[])
)
INSERT INTO user_saved_albums (user_id, album_id, added_at)
SELECT $1, album_id, added_at FROM input
ON CONFLICT (user_id, album_id) DO UPDATE SET added_at = EXCLUDED.added_at`

// ReplaceSavedAlbums makes items the user's complete set of saved albums. See
// ReplaceSavedTracks: the same delete-absent-plus-upsert-present reasoning
// applies here.
func (r *Repo) ReplaceSavedAlbums(ctx context.Context, q store.Querier, userID uuid.UUID, items []SavedItem) error {
	ids, addedAt := savedItemRows(items)
	if _, err := q.Exec(ctx, replaceSavedAlbumsSQL, store.UUIDArg(userID), ids, addedAt); err != nil {
		return postgres.Classify("replace saved albums", err)
	}
	return nil
}

// ---- followed artists -------------------------------------------------------

// replaceFollowedArtistsSQL is the same reconciliation, over artist ids that
// carry no per-row data of their own — Spotify reports no "followed at" for an
// artist — so the present side is DO NOTHING rather than an update.
const replaceFollowedArtistsSQL = `
WITH input AS (
    SELECT DISTINCT artist_id FROM unnest($2::text[]) AS t(artist_id)
),
stale AS (
    DELETE FROM user_followed_artists
    WHERE user_id = $1 AND artist_id <> ALL($2::text[])
)
INSERT INTO user_followed_artists (user_id, artist_id)
SELECT $1, artist_id FROM input
ON CONFLICT (user_id, artist_id) DO NOTHING`

// ReplaceFollowedArtists makes ids the user's complete set of followed
// artists, deleting whichever previously-followed artist is no longer among
// them.
func (r *Repo) ReplaceFollowedArtists(ctx context.Context, q store.Querier, userID uuid.UUID, ids []string) error {
	ids = filterIDs(ids)
	if _, err := q.Exec(ctx, replaceFollowedArtistsSQL, store.UUIDArg(userID), ids); err != nil {
		return postgres.Classify("replace followed artists", err)
	}
	return nil
}

// ---- counts ------------------------------------------------------------------

// countsSQL reads all three counts in one round trip rather than three,
// since a caller wanting one invariably wants the others alongside it (an
// account overview, a sync summary).
const countsSQL = `
SELECT
    (SELECT count(*) FROM user_saved_tracks      WHERE user_id = $1),
    (SELECT count(*) FROM user_saved_albums      WHERE user_id = $1),
    (SELECT count(*) FROM user_followed_artists  WHERE user_id = $1)`

// Counts reports how large a user's saved-tracks, saved-albums and
// followed-artists sets currently are. A user who has never been synced reads
// the same as one whose library is genuinely empty; distinguishing those two
// is what spotify_credentials.library_synced_at is for.
func (r *Repo) Counts(ctx context.Context, q store.Querier, userID uuid.UUID) (Counts, error) {
	var c Counts
	if err := q.QueryRow(ctx, countsSQL, store.UUIDArg(userID)).Scan(
		&c.SavedTracks, &c.SavedAlbums, &c.FollowedArtists,
	); err != nil {
		return Counts{}, postgres.Classify("count library", err)
	}
	return c, nil
}

// savedItemRows transposes a batch of SavedItem into the parallel arrays
// unnest expects, dropping entries with a blank id. The result is always a
// non-nil (possibly empty) pair, so an empty or nil items still reaches the
// statement as an empty array rather than encoding as SQL NULL — the
// difference between "delete everything" and "touch nothing".
func savedItemRows(items []SavedItem) (ids []string, addedAt []*time.Time) {
	ids = make([]string, 0, len(items))
	addedAt = make([]*time.Time, 0, len(items))
	for _, it := range items {
		if it.ID == "" {
			continue
		}
		ids = append(ids, it.ID)
		if it.AddedAt == nil {
			addedAt = append(addedAt, nil)
			continue
		}
		t := it.AddedAt.UTC()
		addedAt = append(addedAt, &t)
	}
	return ids, addedAt
}

// filterIDs drops blank ids while preserving order and any repeats; the SQL's
// own DISTINCT handles repeats among what is left. It always returns a
// non-nil slice, for the same reason savedItemRows does.
func filterIDs(ids []string) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if id == "" {
			continue
		}
		out = append(out, id)
	}
	return out
}
