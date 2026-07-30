package library

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/RequiDev/encore/internal/postgres"
	"github.com/RequiDev/encore/internal/store"
)

// UserPlaylistRow is one entry of a listener's own playlist library, ready to
// reconcile against user_playlists: a bare id, plus enough bookkeeping detail
// (see migrations/00012_playlist_context.sql) to give a bare context_id
// recorded against a play a name.
type UserPlaylistRow struct {
	ID          string
	Name        string
	OwnerID     string
	SnapshotID  string
	TotalTracks int
}

// replaceUserPlaylistsSQL deletes whatever the incoming set no longer
// contains and upserts the rest, in one statement — the same
// delete-absent-plus-upsert-present shape as ReplaceSavedTracks and
// ReplaceSavedAlbums above. Unlike those two, every column here besides the
// key can legitimately change under a playlist Encore already knows about — a
// rename, tracks added or removed elsewhere, an owner transfer of a
// collaborative playlist — so ON CONFLICT refreshes all of them, not merely
// the timestamp.
//
// DISTINCT ON collapses a duplicate id within one call for the same reason it
// does in replaceSavedTracksSQL: Postgres refuses to let ON CONFLICT touch the
// same row twice inside a single statement, and a page boundary could in
// principle repeat one.
const replaceUserPlaylistsSQL = `
WITH input AS (
    SELECT DISTINCT ON (playlist_id) *
    FROM unnest($2::text[], $3::text[], $4::text[], $5::int[], $6::text[])
        AS t(playlist_id, name, owner_id, total_tracks, snapshot_id)
    ORDER BY playlist_id
),
stale AS (
    DELETE FROM user_playlists
    WHERE user_id = $1 AND playlist_id <> ALL($2::text[])
)
INSERT INTO user_playlists (user_id, playlist_id, name, owner_id, total_tracks, snapshot_id, fetched_at)
SELECT $1, playlist_id, name, owner_id, total_tracks, snapshot_id, $7 FROM input
ON CONFLICT (user_id, playlist_id) DO UPDATE SET
    name         = EXCLUDED.name,
    owner_id     = EXCLUDED.owner_id,
    total_tracks = EXCLUDED.total_tracks,
    snapshot_id  = EXCLUDED.snapshot_id,
    fetched_at   = EXCLUDED.fetched_at`

// ReplaceUserPlaylists makes items the user's complete set of playlists.
//
// This is delete-absent-plus-upsert-present, not truncate-then-insert, for
// the same reason ReplaceSavedTracks is: a concurrent reader must never
// observe the set as empty partway through a sync. Passing an empty items
// removes every row — a listener who deleted or left every playlist they had
// is a real, representable state, distinct from one who has never had this
// enumerated at all (that distinction belongs to the caller's watermark, not
// to whether this table holds rows).
//
// fetchedAt is one instant applied to every row this call writes, mirroring
// ReplaceTopSnapshot's capturedAt: the caller reads its clock once and reuses
// it, so one run reports one consistent "as of" time rather than one per row.
func (r *Repo) ReplaceUserPlaylists(ctx context.Context, q store.Querier, userID uuid.UUID, items []UserPlaylistRow, fetchedAt time.Time) error {
	ids, names, ownerIDs, totalTracks, snapshotIDs := userPlaylistRows(items)
	if _, err := q.Exec(ctx, replaceUserPlaylistsSQL,
		store.UUIDArg(userID), ids, names, ownerIDs, totalTracks, snapshotIDs, fetchedAt.UTC(),
	); err != nil {
		return postgres.Classify("replace user playlists", err)
	}
	return nil
}

// userPlaylistRows transposes a batch of UserPlaylistRow into the parallel
// arrays unnest expects, dropping entries with a blank id — the same guard
// savedItemRows applies, since a nameless key has no row for ON CONFLICT to
// place. Every slice is always non-nil, even when items is empty, so an empty
// batch still reaches the statement as an empty array rather than SQL NULL:
// the difference between "delete everything" and "touch nothing".
func userPlaylistRows(items []UserPlaylistRow) (ids, names, ownerIDs []string, totalTracks []int32, snapshotIDs []string) {
	ids = make([]string, 0, len(items))
	names = make([]string, 0, len(items))
	ownerIDs = make([]string, 0, len(items))
	totalTracks = make([]int32, 0, len(items))
	snapshotIDs = make([]string, 0, len(items))
	for _, it := range items {
		if it.ID == "" {
			continue
		}
		ids = append(ids, it.ID)
		names = append(names, it.Name)
		ownerIDs = append(ownerIDs, it.OwnerID)
		totalTracks = append(totalTracks, int32(it.TotalTracks))
		snapshotIDs = append(snapshotIDs, it.SnapshotID)
	}
	return ids, names, ownerIDs, totalTracks, snapshotIDs
}
