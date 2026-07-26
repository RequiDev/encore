package catalog

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/requi/encore/internal/domain"
	"github.com/requi/encore/internal/postgres"
	"github.com/requi/encore/internal/store"
)

// listBlacklistedSQL reads the blacklist as full artist rows so the settings
// screen can show names and images rather than bare Spotify ids.
const listBlacklistedSQL = artistSelect + `
JOIN user_blacklisted_artists b ON b.artist_id = a.id
WHERE b.user_id = $1
ORDER BY a.name_norm, a.id`

// ListBlacklisted returns the artists a user has excluded from all statistics,
// in name order. An artist that is blacklisted before enrichment has reached it
// comes back with an empty name and a pending state.
func (r *Repo) ListBlacklisted(ctx context.Context, q store.Querier, userID uuid.UUID) ([]domain.Artist, error) {
	rows, err := q.Query(ctx, listBlacklistedSQL, store.UUIDArg(userID))
	if err != nil {
		return nil, postgres.Classify("list blacklisted artists", err)
	}
	defer rows.Close()

	var out []domain.Artist
	for rows.Next() {
		a, err := scanArtist(rows)
		if err != nil {
			return nil, postgres.Classify("scan blacklisted artist", err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, postgres.Classify("list blacklisted artists", err)
	}
	return out, nil
}

// blacklistSQL excludes an artist for one user.
//
// The artist row is ensured first because the blacklist carries a foreign key to
// artists and a user may blacklist an artist the catalogue has only ever seen as
// an id on a track. Creating it pending also gets the name fetched, so the
// settings screen stops showing a bare id.
const blacklistSQL = `
WITH ensure_artist AS (
    INSERT INTO artists (id, metadata_state) VALUES ($2, 'pending')
    ON CONFLICT (id) DO NOTHING
)
INSERT INTO user_blacklisted_artists (user_id, artist_id)
VALUES ($1, $2)
ON CONFLICT (user_id, artist_id) DO NOTHING`

// Blacklist excludes an artist from every statistic for one user. It is
// idempotent, so the toggle in the UI can be pressed twice without an error.
//
// Nothing is deleted: the listens stay, and removing the artist from the
// blacklist restores them. Exclusion is applied at query time.
func (r *Repo) Blacklist(ctx context.Context, q store.Querier, userID uuid.UUID, artistID string) error {
	if artistID == "" {
		return fmt.Errorf("%w: artist id is required", domain.ErrValidation)
	}
	if _, err := q.Exec(ctx, blacklistSQL, store.UUIDArg(userID), artistID); err != nil {
		return postgres.Classify("blacklist artist", err)
	}
	return nil
}

// Unblacklist puts an artist back into a user's statistics. Like Blacklist it is
// idempotent: removing an artist that was never excluded is not an error, since
// the caller is expressing a desired end state rather than editing a record.
func (r *Repo) Unblacklist(ctx context.Context, q store.Querier, userID uuid.UUID, artistID string) error {
	if artistID == "" {
		return fmt.Errorf("%w: artist id is required", domain.ErrValidation)
	}
	const sql = `DELETE FROM user_blacklisted_artists WHERE user_id = $1 AND artist_id = $2`
	if _, err := q.Exec(ctx, sql, store.UUIDArg(userID), artistID); err != nil {
		return postgres.Classify("unblacklist artist", err)
	}
	return nil
}

// BlacklistedArtistIDs returns just the ids a user has excluded, which is what
// the statistics layer needs to build its NOT EXISTS filter without paying for
// the artist rows themselves.
func (r *Repo) BlacklistedArtistIDs(ctx context.Context, q store.Querier, userID uuid.UUID) ([]string, error) {
	const sql = `
        SELECT artist_id FROM user_blacklisted_artists
        WHERE user_id = $1
        ORDER BY artist_id`
	rows, err := q.Query(ctx, sql, store.UUIDArg(userID))
	if err != nil {
		return nil, postgres.Classify("list blacklisted artist ids", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, postgres.Classify("scan blacklisted artist id", err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, postgres.Classify("list blacklisted artist ids", err)
	}
	return out, nil
}
