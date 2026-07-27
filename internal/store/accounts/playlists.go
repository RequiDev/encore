package accounts

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/RequiDev/encore/internal/domain"
	"github.com/RequiDev/encore/internal/postgres"
	"github.com/RequiDev/encore/internal/store"
)

// PlaylistLimitPerUser caps how many playlists Encore will manage for one
// account. A rebuild is several Spotify requests, and an account with hundreds
// of definitions would spend a quota on playlists nobody opens.
const PlaylistLimitPerUser = 50

// Playlists stores playlist definitions and what they produced.
type Playlists struct{ db *store.Store }

// NewPlaylists builds the repository.
func NewPlaylists(db *store.Store) *Playlists { return &Playlists{db: db} }

const playlistColumns = `id, user_id, name, spotify_id, spotify_url,
                         mode, sort, track_limit, min_plays, range_from, range_to,
                         track_count, built_at, created_at`

// Create records a playlist Encore has just made on Spotify.
func (r *Playlists) Create(ctx context.Context, q store.Querier, p domain.Playlist) (domain.Playlist, error) {
	const sql = `
        INSERT INTO playlists
            (user_id, name, spotify_id, spotify_url, mode, sort, track_limit, min_plays,
             range_from, range_to, track_count, built_at)
        SELECT $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12
        WHERE (SELECT count(*) FROM playlists WHERE user_id = $1) < $13
        RETURNING ` + playlistColumns

	row := q.QueryRow(ctx, sql,
		p.UserID, p.Name, p.SpotifyID, p.SpotifyURL,
		string(p.Definition.Mode), string(p.Definition.Sort),
		p.Definition.Limit, p.Definition.MinPlays,
		nullTime(p.Definition.From), nullTime(p.Definition.To),
		p.TrackCount, nullTime(p.BuiltAt), PlaylistLimitPerUser)

	out, err := scanPlaylist(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Playlist{}, fmt.Errorf(
			"%w: Encore manages at most %d playlists per account; delete one first",
			domain.ErrValidation, PlaylistLimitPerUser)
	}
	if err != nil {
		return domain.Playlist{}, postgres.Classify("create playlist", err)
	}
	return out, nil
}

// ListForUser returns a user's playlists, newest first.
func (r *Playlists) ListForUser(ctx context.Context, q store.Querier, userID uuid.UUID) ([]domain.Playlist, error) {
	rows, err := q.Query(ctx, `SELECT `+playlistColumns+`
        FROM playlists WHERE user_id = $1 ORDER BY created_at DESC`, userID)
	if err != nil {
		return nil, postgres.Classify("list playlists", err)
	}
	defer rows.Close()

	out := make([]domain.Playlist, 0, 8)
	for rows.Next() {
		p, err := scanPlaylist(rows)
		if err != nil {
			return nil, postgres.Classify("scan playlist", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, postgres.Classify("list playlists", err)
	}
	return out, nil
}

// Get reads one of a user's playlists. Scoped by owner, so another account's id
// is simply not found.
func (r *Playlists) Get(ctx context.Context, q store.Querier, userID, id uuid.UUID) (domain.Playlist, error) {
	row := q.QueryRow(ctx, `SELECT `+playlistColumns+`
        FROM playlists WHERE id = $1 AND user_id = $2`, id, userID)
	p, err := scanPlaylist(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Playlist{}, fmt.Errorf("%w: no such playlist", domain.ErrNotFound)
	}
	if err != nil {
		return domain.Playlist{}, postgres.Classify("read playlist", err)
	}
	return p, nil
}

// RecordBuild updates what a rebuild produced.
func (r *Playlists) RecordBuild(
	ctx context.Context, q store.Querier, id uuid.UUID, trackCount int, at time.Time,
) error {
	_, err := q.Exec(ctx,
		`UPDATE playlists SET track_count = $2, built_at = $3 WHERE id = $1`,
		id, trackCount, at.UTC())
	if err != nil {
		return postgres.Classify("record playlist build", err)
	}
	return nil
}

// Forget removes Encore's record of a playlist.
//
// It deliberately does not delete anything on Spotify. The playlist belongs to
// the listener's account, and an application that made things disappear from it
// would be exceeding what "stop managing this" can reasonably mean.
func (r *Playlists) Forget(ctx context.Context, q store.Querier, userID, id uuid.UUID) error {
	tag, err := q.Exec(ctx, `DELETE FROM playlists WHERE id = $1 AND user_id = $2`, id, userID)
	if err != nil {
		return postgres.Classify("forget playlist", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: no such playlist", domain.ErrNotFound)
	}
	return nil
}

func scanPlaylist(row rowScanner) (domain.Playlist, error) {
	var (
		p                 domain.Playlist
		mode, sortBy      string
		from, to, builtAt *time.Time
	)
	if err := row.Scan(&p.ID, &p.UserID, &p.Name, &p.SpotifyID, &p.SpotifyURL,
		&mode, &sortBy, &p.Definition.Limit, &p.Definition.MinPlays,
		&from, &to, &p.TrackCount, &builtAt, &p.CreatedAt); err != nil {
		return domain.Playlist{}, err
	}
	p.Definition.Mode = domain.PlaylistMode(mode)
	p.Definition.Sort = domain.PlaylistSort(sortBy)
	if from != nil {
		p.Definition.From = from.UTC()
	}
	if to != nil {
		p.Definition.To = to.UTC()
	}
	if builtAt != nil {
		p.BuiltAt = builtAt.UTC()
	}
	return p, nil
}
