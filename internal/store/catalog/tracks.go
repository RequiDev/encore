package catalog

import (
	"context"
	"fmt"

	"github.com/RequiDev/encore/internal/domain"
	"github.com/RequiDev/encore/internal/postgres"
	"github.com/RequiDev/encore/internal/store"
)

// trackSelect reads a whole track row plus its artist ids in credit order.
// album_id is coalesced because domain.Track carries an empty string, not a
// pointer, for "no album known yet".
const trackSelect = `
SELECT t.id, t.name, t.name_norm, coalesce(t.album_id, ''), t.duration_ms, t.explicit,
       t.popularity, t.isrc, t.metadata_state, t.fetch_attempts, t.next_attempt_at, t.fetched_at,
       coalesce(array_agg(ta.artist_id ORDER BY ta.position, ta.artist_id)
                FILTER (WHERE ta.artist_id IS NOT NULL), '{}')
FROM tracks t
LEFT JOIN track_artists ta ON ta.track_id = t.id`

func scanTrack(s scanner) (domain.Track, error) {
	var (
		t     domain.Track
		state string
	)
	if err := s.Scan(&t.ID, &t.Name, &t.NameNorm, &t.AlbumID, &t.DurationMs, &t.Explicit,
		&t.Popularity, &t.ISRC, &state, &t.FetchAttempts, &t.NextAttemptAt, &t.FetchedAt,
		&t.ArtistIDs); err != nil {
		return domain.Track{}, err
	}
	t.MetadataState = metadataState(state)
	return t, nil
}

// GetTrack reads one track with its artist ids populated. A track that exists
// but is still pending enrichment is returned with empty names, which is exactly
// what the API needs in order to render "unknown track" rather than a 404.
func (r *Repo) GetTrack(ctx context.Context, q store.Querier, id string) (domain.Track, error) {
	if id == "" {
		return domain.Track{}, fmt.Errorf("%w: track id is required", domain.ErrValidation)
	}
	const sql = trackSelect + ` WHERE t.id = $1 GROUP BY t.id`
	t, err := scanTrack(q.QueryRow(ctx, sql, id))
	if err != nil {
		return domain.Track{}, postgres.Classify("get track", err)
	}
	return t, nil
}

// GetTracks reads many tracks at once, keyed by id. It is the batch loader
// behind every list endpoint: one query per page rather than one per row.
func (r *Repo) GetTracks(ctx context.Context, q store.Querier, ids []string) (map[string]domain.Track, error) {
	ids = dedupeIDs(ids)
	if len(ids) == 0 {
		return map[string]domain.Track{}, nil
	}
	const sql = trackSelect + ` WHERE t.id = ANY($1::text[]) GROUP BY t.id`
	rows, err := q.Query(ctx, sql, ids)
	if err != nil {
		return nil, postgres.Classify("get tracks", err)
	}
	defer rows.Close()

	out := make(map[string]domain.Track, len(ids))
	for rows.Next() {
		t, err := scanTrack(rows)
		if err != nil {
			return nil, postgres.Classify("scan track", err)
		}
		out[t.ID] = t
	}
	if err := rows.Err(); err != nil {
		return nil, postgres.Classify("get tracks", err)
	}
	return out, nil
}

// upsertTracksSQL writes a batch of tracks and seeds the queue with everything
// they reference.
//
// The album and artist rows are created in the same statement for two reasons:
// tracks.album_id is a foreign key, so the album has to exist before the track
// can point at it, and creating those rows as pending is what makes the
// enrichment queue self-feeding - one resolved track pulls its album and its
// artists in behind it without any orchestration outside the database.
const upsertTracksSQL = `
WITH input AS (
    SELECT DISTINCT ON (id) *
    FROM unnest($1::text[], $2::text[], $3::text[], $4::text[], $5::int[], $6::bool[],
                $7::int[], $8::text[])
        AS t(id, name, name_norm, album_id, duration_ms, explicit, popularity, isrc)
    ORDER BY id
),
ensure_albums AS (
    INSERT INTO albums (id, metadata_state)
    SELECT DISTINCT album_id, 'pending' FROM input WHERE album_id IS NOT NULL
    ON CONFLICT (id) DO NOTHING
),
ensure_artists AS (
    INSERT INTO artists (id, metadata_state)
    SELECT DISTINCT a, 'pending' FROM unnest($9::text[]) AS u(a)
    ON CONFLICT (id) DO NOTHING
)
INSERT INTO tracks (
    id, name, name_norm, album_id, duration_ms, explicit, popularity, isrc,
    metadata_state, fetch_attempts, next_attempt_at, fetched_at)
SELECT id, name, name_norm, album_id, duration_ms, explicit, popularity, isrc,
       'resolved', 0, NULL, now()
FROM input
ON CONFLICT (id) DO UPDATE SET
    name            = EXCLUDED.name,
    name_norm       = EXCLUDED.name_norm,
    album_id        = EXCLUDED.album_id,
    duration_ms     = EXCLUDED.duration_ms,
    explicit        = EXCLUDED.explicit,
    popularity      = EXCLUDED.popularity,
    isrc            = EXCLUDED.isrc,
    metadata_state  = 'resolved',
    fetch_attempts  = 0,
    next_attempt_at = NULL,
    fetched_at      = now()`

// UpsertTracks records fetched track metadata and takes the rows out of the
// enrichment queue. The album and the credited artists are ensured as pending
// rows; the track_artists links themselves are written by ReplaceTrackArtists.
func (r *Repo) UpsertTracks(ctx context.Context, q store.Querier, tracks []domain.Track) error {
	rows := buildTrackRows(tracks)
	if len(rows.ids) == 0 {
		return nil
	}
	_, err := q.Exec(ctx, upsertTracksSQL,
		rows.ids, rows.names, rows.norms, rows.albumIDs, rows.durations,
		rows.explicit, rows.popularity, rows.isrcs, rows.artistIDs)
	if err != nil {
		return postgres.Classify("upsert tracks", err)
	}
	return nil
}

// trackRows is the batch transposed into the parallel arrays the upsert takes.
type trackRows struct {
	ids        []string
	names      []string
	norms      []string
	isrcs      []string
	albumIDs   []*string
	durations  []int32
	popularity []int32
	explicit   []bool
	// artistIDs is the flattened set of credited artists, used only to create
	// their pending rows.
	artistIDs []string
}

// buildTrackRows transposes a batch.
//
// name_norm is derived with domain.NormalizeTitle rather than taken from the
// caller, because the alias resolver compares tracks.name_norm against the
// title_norm half of an alias key and the two must come from the same function
// or names-only listens would never converge.
func buildTrackRows(tracks []domain.Track) trackRows {
	out := trackRows{
		ids:        make([]string, 0, len(tracks)),
		names:      make([]string, 0, len(tracks)),
		norms:      make([]string, 0, len(tracks)),
		isrcs:      make([]string, 0, len(tracks)),
		albumIDs:   make([]*string, 0, len(tracks)),
		durations:  make([]int32, 0, len(tracks)),
		popularity: make([]int32, 0, len(tracks)),
		explicit:   make([]bool, 0, len(tracks)),
		artistIDs:  make([]string, 0, len(tracks)),
	}
	seenArtist := make(map[string]struct{})
	for _, t := range tracks {
		if t.ID == "" {
			continue
		}
		out.ids = append(out.ids, t.ID)
		out.names = append(out.names, t.Name)
		out.norms = append(out.norms, domain.NormalizeTitle(t.Name))
		out.isrcs = append(out.isrcs, t.ISRC)
		out.albumIDs = append(out.albumIDs, store.Nullable(t.AlbumID))
		out.durations = append(out.durations, t.DurationMs)
		out.popularity = append(out.popularity, t.Popularity)
		out.explicit = append(out.explicit, t.Explicit)
		for _, id := range t.ArtistIDs {
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

// replaceTrackArtistsSQL rewrites one track's credits in a single statement.
// The delete and the insert cover disjoint sets of rows, so running both against
// track_artists inside one statement is well defined.
const replaceTrackArtistsSQL = `
WITH ensure_artists AS (
    INSERT INTO artists (id, metadata_state)
    SELECT DISTINCT a, 'pending' FROM unnest($2::text[]) AS u(a)
    ON CONFLICT (id) DO NOTHING
),
ensure_track AS (
    INSERT INTO tracks (id, metadata_state) VALUES ($1, 'pending')
    ON CONFLICT (id) DO NOTHING
),
stale AS (
    DELETE FROM track_artists
    WHERE track_id = $1 AND artist_id <> ALL($2::text[])
)
INSERT INTO track_artists (track_id, artist_id, position)
SELECT $1, a, pos - 1
FROM unnest($2::text[]) WITH ORDINALITY AS t(a, pos)
ON CONFLICT (track_id, artist_id) DO UPDATE SET position = EXCLUDED.position`

// ReplaceTrackArtists makes artistIDs the track's complete credit list, in the
// given order, removing any credit that is no longer present. Passing an empty
// list clears the credits.
//
// Both the track and the artists are created as pending rows first, so a caller
// that has only seen the ids and not yet the metadata cannot trip a foreign key.
func (r *Repo) ReplaceTrackArtists(ctx context.Context, q store.Querier, trackID string, artistIDs []string) error {
	if trackID == "" {
		return fmt.Errorf("%w: track id is required", domain.ErrValidation)
	}
	ids := dedupeIDs(artistIDs)
	if ids == nil {
		// An explicit empty list still has to reach the statement so the stale
		// credits are removed; a nil slice would encode as SQL NULL.
		ids = []string{}
	}
	if _, err := q.Exec(ctx, replaceTrackArtistsSQL, trackID, ids); err != nil {
		return postgres.Classify("replace track artists", err)
	}
	return nil
}
