package main

import (
	"context"
	cryptorand "crypto/rand"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/requi/encore/internal/config"
	"github.com/requi/encore/internal/crypto"
	"github.com/requi/encore/internal/postgres"
	"github.com/requi/encore/internal/store"
)

// benchMaxConns matches the ENCORE_DATABASE_MAX_CONNS default. The pool size is
// the importer's backpressure valve, so the benchmark has to use the same one a
// deployment does or it would be measuring a different system.
const benchMaxConns = 10

// openDatabase connects and builds the Store the benchmark needs.
//
// The configuration is assembled by hand rather than through config.Load,
// because a benchmark of the importer must run with nothing but
// ENCORE_DATABASE_URL set: requiring a Spotify client id and an encryption key
// to measure a code path that never touches Spotify would be absurd.
func openDatabase(ctx context.Context, dsn string, lg *slog.Logger) (*postgres.Pool, *store.Store, error) {
	pool, err := postgres.Connect(ctx, config.Database{
		URL:            dsn,
		MaxConns:       benchMaxConns,
		MinConns:       1,
		ConnectTimeout: 10 * time.Second,
		// More generous than the API's 60 seconds. The batch statements are
		// bounded by the batch size, but the row counts this tool reads back are
		// full aggregates over a table it has just filled with a million rows, and
		// on a cold cache that is a slow query rather than a broken one.
		StatementTimeout: 5 * time.Minute,
	}, lg)
	if err != nil {
		return nil, nil, err
	}

	// The Store insists on a sealer because Spotify credentials are encrypted at
	// rest. The benchmark never reads or writes one, so an ephemeral key is not
	// merely sufficient: it is the safer choice, because a throwaway process
	// cannot then leave behind a sealed value that a real deployment would later
	// fail to open.
	key := make([]byte, 32)
	if _, err := cryptorand.Read(key); err != nil {
		pool.Close()
		return nil, nil, fmt.Errorf("generate an ephemeral encryption key: %w", err)
	}
	sealer, err := crypto.NewSealer(key)
	if err != nil {
		pool.Close()
		return nil, nil, err
	}
	st, err := store.New(pool, sealer)
	if err != nil {
		pool.Close()
		return nil, nil, err
	}
	return pool, st, nil
}

// requireSchema refuses to run against a database whose migrations are behind.
//
// Without this the failure would be a confusing "relation listens does not
// exist" from somewhere deep inside the importer, several seconds after a
// dataset was generated.
func requireSchema(ctx context.Context, dsn string) error {
	st, err := postgres.Status(ctx, dsn)
	if err != nil {
		return fmt.Errorf("read the schema version: %w", err)
	}
	if !st.UpToDate() {
		return fmt.Errorf(
			"the database schema is at version %d and %d migration(s) are pending; run `go run ./cmd/encore-migrate up` first",
			st.Current, st.Pending)
	}
	return nil
}

// rowCounts is what the database itself says about a user's history.
//
// Every figure here is a live aggregate, never a running tally kept by the
// importer. That is the whole point: the benchmark's job is to check the
// importer's arithmetic against the rows that actually exist.
type rowCounts struct {
	Listens int64 `json:"listens"`
	// ListensWithTrack is how many listens are anchored to a catalogue track. An
	// extended import resolves all of them; an account-data import resolves none
	// until the alias resolver has run, which is expected and not a fault.
	ListensWithTrack int64      `json:"listens_with_track_id"`
	DistinctTracks   int64      `json:"distinct_tracks"`
	DistinctAliases  int64      `json:"distinct_name_pairs"`
	TracksTotal      int64      `json:"tracks_in_catalogue"`
	FirstPlayedAt    *time.Time `json:"first_played_at,omitempty"`
	LastPlayedAt     *time.Time `json:"last_played_at,omitempty"`
}

const rowCountsSQL = `
    SELECT count(*),
           count(track_id),
           count(DISTINCT track_id),
           count(DISTINCT (alias_artist, alias_title)) FILTER (WHERE track_id IS NULL),
           min(played_at),
           max(played_at)
    FROM listens
    WHERE user_id = $1`

// readRowCounts counts one user's history straight from the tables.
func readRowCounts(ctx context.Context, q store.Querier, userID uuid.UUID) (rowCounts, error) {
	var c rowCounts
	err := q.QueryRow(ctx, rowCountsSQL, store.UUIDArg(userID)).Scan(
		&c.Listens, &c.ListensWithTrack, &c.DistinctTracks, &c.DistinctAliases,
		&c.FirstPlayedAt, &c.LastPlayedAt)
	if err != nil {
		return rowCounts{}, postgres.Classify("count listens", err)
	}
	if err := q.QueryRow(ctx, `SELECT count(*) FROM tracks`).Scan(&c.TracksTotal); err != nil {
		return rowCounts{}, postgres.Classify("count tracks", err)
	}
	return c, nil
}

const listensForJobSQL = `
    SELECT count(*)
    FROM listens l
    JOIN import_files f ON f.id = l.import_file_id
    WHERE f.job_id = $1`

// countListensForJob is the committed row count the benchmark holds the
// importer's `imported` counter against.
func countListensForJob(ctx context.Context, q store.Querier, jobID uuid.UUID) (int64, error) {
	var n int64
	if err := q.QueryRow(ctx, listensForJobSQL, store.UUIDArg(jobID)).Scan(&n); err != nil {
		return 0, postgres.Classify("count listens for job", err)
	}
	return n, nil
}

// jobStatusCounts summarises a user's import jobs by status, which is all the
// verify command needs to say whether their history is fully imported.
func jobStatusCounts(ctx context.Context, q store.Querier, userID uuid.UUID) (map[string]int64, error) {
	rows, err := q.Query(ctx,
		`SELECT status, count(*) FROM import_jobs WHERE user_id = $1 GROUP BY status ORDER BY status`,
		store.UUIDArg(userID))
	if err != nil {
		return nil, postgres.Classify("count import jobs", err)
	}
	defer rows.Close()

	out := map[string]int64{}
	for rows.Next() {
		var (
			status string
			n      int64
		)
		if err := rows.Scan(&status, &n); err != nil {
			return nil, postgres.Classify("scan import job counts", err)
		}
		out[status] = n
	}
	if err := rows.Err(); err != nil {
		return nil, postgres.Classify("count import jobs", err)
	}
	return out, nil
}

// removeOrphanedCatalogue deletes the catalogue rows this run created and
// nothing else.
//
// Ingestion writes a pending tracks row for every unseen track id and a pending
// track_aliases row for every unseen name pair, and neither is owned by a user,
// so deleting the benchmark's user does not take them with it. Both conditions
// below matter: `created_at >= since` limits the deletion to rows this run
// created, and the NOT EXISTS clause guarantees that a row some other listener's
// history depends on is never touched even if the timestamps overlap.
func removeOrphanedCatalogue(ctx context.Context, q store.Querier, since time.Time) (int64, error) {
	tracks, err := q.Exec(ctx, `
        DELETE FROM tracks t
        WHERE t.metadata_state = 'pending'
          AND t.created_at >= $1
          AND NOT EXISTS (SELECT 1 FROM listens l WHERE l.track_id = t.id)`, since)
	if err != nil {
		return 0, postgres.Classify("remove benchmark tracks", err)
	}

	aliases, err := q.Exec(ctx, `
        DELETE FROM track_aliases a
        WHERE a.state = 'pending'
          AND a.created_at >= $1
          AND NOT EXISTS (
              SELECT 1 FROM listens l
              WHERE l.track_id IS NULL
                AND l.alias_artist = a.artist_norm
                AND l.alias_title = a.title_norm)`, since)
	if err != nil {
		return tracks.RowsAffected(), postgres.Classify("remove benchmark aliases", err)
	}
	return tracks.RowsAffected() + aliases.RowsAffected(), nil
}
