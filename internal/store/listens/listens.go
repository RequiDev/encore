// Package listens is the ingestion path: it turns validated domain listens into
// rows and enforces Encore's duplicate policy.
//
// Everything here is idempotent. Running the same batch twice inserts nothing
// the second time, which is what lets an interrupted import simply resume from
// its last checkpoint instead of reasoning about what it had already done.
package listens

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/requi/encore/internal/domain"
	"github.com/requi/encore/internal/postgres"
	"github.com/requi/encore/internal/store"
)

// Repo is the listens repository.
type Repo struct{ db *store.Store }

// New builds the repository.
func New(db *store.Store) *Repo { return &Repo{db: db} }

// StagedListen is a listen prepared for insertion: names already normalised,
// identity and dedupe keys already computed. Building it is pure domain work, so
// the duplicate rules are unit-testable without a database.
type StagedListen struct {
	UserID       uuid.UUID
	PlayedAt     time.Time
	Precision    domain.Precision
	TrackID      string
	AliasArtist  string
	AliasTitle   string
	IdentityKey  []byte
	DedupeKey    []byte
	MsPlayed     int32
	Source       domain.Source
	ImportFileID *uuid.UUID

	Platform    string
	ConnCountry string
	ReasonStart string
	ReasonEnd   string
	Shuffle     *bool
	Skipped     *bool
	Offline     *bool
	Incognito   *bool
}

// Stage turns a domain.Listen into its persistable form.
func Stage(l domain.Listen, importFileID *uuid.UUID) StagedListen {
	return StagedListen{
		UserID:       l.UserID,
		PlayedAt:     l.PlayedAt.UTC(),
		Precision:    l.Precision,
		TrackID:      l.Identity.TrackID,
		AliasArtist:  l.Identity.Artist,
		AliasTitle:   l.Identity.Title,
		IdentityKey:  l.IdentityKey(),
		DedupeKey:    l.DedupeKey(),
		MsPlayed:     l.MsPlayed,
		Source:       l.Source,
		ImportFileID: importFileID,
		Platform:     l.Platform,
		ConnCountry:  l.ConnCountry,
		ReasonStart:  l.ReasonStart,
		ReasonEnd:    l.ReasonEnd,
		Shuffle:      l.Shuffle,
		Skipped:      l.Skipped,
		Offline:      l.Offline,
		Incognito:    l.Incognito,
	}
}

// insertListensSQL implements the whole duplicate policy in one round trip.
//
//	input   the incoming batch, transposed into parallel arrays
//	deduped collapses duplicates *within* the batch
//	fresh   suppresses events already recorded through a different source
//	ins     the insert, with the unique constraint as the final authority
//	dirty   marks the affected local days for statistics rollup recomputation
//
// The three layers are deliberately redundant. `ins` alone would be correct for
// re-imports of the same file; `fresh` is what makes an account-data export and
// an extended export of the same period agree; `deduped` keeps a single
// malformed file from tripping the unique constraint on every row.
const insertListensSQL = `
WITH input AS (
    SELECT * FROM unnest(
        $1::uuid[], $2::timestamptz[], $3::smallint[],
        $4::text[], $5::text[], $6::text[],
        $7::bytea[], $8::bytea[],
        $9::int[], $10::smallint[], $11::uuid[],
        $12::text[], $13::text[], $14::text[], $15::text[],
        $16::bool[], $17::bool[], $18::bool[], $19::bool[]
    ) AS t(
        user_id, played_at, ts_precision,
        track_id, alias_artist, alias_title,
        identity_key, dedupe_key,
        ms_played, source, import_file_id,
        platform, conn_country, reason_start, reason_end,
        shuffle, skipped, offline, incognito
    )
),
deduped AS (
    SELECT DISTINCT ON (user_id, dedupe_key) *
    FROM input
    ORDER BY user_id, dedupe_key, ms_played DESC
),
fresh AS (
    SELECT d.* FROM deduped d
    WHERE NOT EXISTS (
        SELECT 1 FROM listens l
        WHERE l.user_id = d.user_id
          AND l.identity_key = d.identity_key
          -- Constant bound so the (user_id, identity_key, played_at) index can
          -- drive the probe; the exact tolerance is applied below.
          AND l.played_at >= d.played_at - interval '60 seconds'
          AND l.played_at <= d.played_at + interval '60 seconds'
          AND l.source <> d.source
          AND abs(extract(epoch FROM (l.played_at - d.played_at))) <= GREATEST(
                CASE l.ts_precision WHEN 2 THEN 60 ELSE 10 END,
                CASE d.ts_precision WHEN 2 THEN 60 ELSE 10 END)
    )
),
ins AS (
    INSERT INTO listens (
        user_id, played_at, ts_precision, track_id, alias_artist, alias_title,
        identity_key, dedupe_key, ms_played, source, import_file_id,
        platform, conn_country, reason_start, reason_end,
        shuffle, skipped, offline, incognito)
    SELECT
        user_id, played_at, ts_precision, track_id, alias_artist, alias_title,
        identity_key, dedupe_key, ms_played, source, import_file_id,
        platform, conn_country, reason_start, reason_end,
        shuffle, skipped, offline, incognito
    FROM fresh
    ON CONFLICT (user_id, dedupe_key) DO NOTHING
    RETURNING user_id, played_at
),
dirty AS (
    INSERT INTO rollup_dirty_days (user_id, day)
    SELECT DISTINCT user_id, (played_at AT TIME ZONE $20::text)::date FROM ins
    ON CONFLICT DO NOTHING
)
SELECT count(*)::bigint FROM ins`

// InsertListens writes a batch and reports how many rows were new.
//
// Every record that is not inserted was a duplicate by one of the three rules,
// so the caller accounts for len(batch) - inserted as duplicates. The call is
// idempotent: running it twice with the same batch inserts nothing the second
// time.
//
// timezone is the owning user's IANA timezone, used only to decide which local
// day to mark dirty for statistics rollups.
func (r *Repo) InsertListens(ctx context.Context, q store.Querier, batch []StagedListen, timezone string) (inserted int64, err error) {
	if len(batch) == 0 {
		return 0, nil
	}
	if timezone == "" {
		timezone = "UTC"
	}

	n := len(batch)
	var (
		userIDs      = make([]string, n)
		playedAt     = make([]time.Time, n)
		precision    = make([]int16, n)
		trackIDs     = make([]*string, n)
		aliasArtists = make([]*string, n)
		aliasTitles  = make([]*string, n)
		identityKeys = make([][]byte, n)
		dedupeKeys   = make([][]byte, n)
		msPlayed     = make([]int32, n)
		sources      = make([]int16, n)
		fileIDs      = make([]*string, n)
		platforms    = make([]*string, n)
		countries    = make([]*string, n)
		reasonStart  = make([]*string, n)
		reasonEnd    = make([]*string, n)
		shuffle      = make([]*bool, n)
		skipped      = make([]*bool, n)
		offline      = make([]*bool, n)
		incognito    = make([]*bool, n)
	)

	for i, l := range batch {
		userIDs[i] = l.UserID.String()
		playedAt[i] = l.PlayedAt.UTC()
		precision[i] = int16(l.Precision)
		trackIDs[i] = store.Nullable(l.TrackID)
		aliasArtists[i] = store.Nullable(l.AliasArtist)
		aliasTitles[i] = store.Nullable(l.AliasTitle)
		identityKeys[i] = l.IdentityKey
		dedupeKeys[i] = l.DedupeKey
		msPlayed[i] = l.MsPlayed
		sources[i] = int16(l.Source)
		if l.ImportFileID != nil {
			fileIDs[i] = store.Ptr(l.ImportFileID.String())
		}
		platforms[i] = store.Nullable(l.Platform)
		countries[i] = store.Nullable(l.ConnCountry)
		reasonStart[i] = store.Nullable(l.ReasonStart)
		reasonEnd[i] = store.Nullable(l.ReasonEnd)
		shuffle[i] = l.Shuffle
		skipped[i] = l.Skipped
		offline[i] = l.Offline
		incognito[i] = l.Incognito
	}

	err = q.QueryRow(ctx, insertListensSQL,
		userIDs, playedAt, precision,
		trackIDs, aliasArtists, aliasTitles,
		identityKeys, dedupeKeys,
		msPlayed, sources, fileIDs,
		platforms, countries, reasonStart, reasonEnd,
		shuffle, skipped, offline, incognito,
		timezone,
	).Scan(&inserted)
	if err != nil {
		return 0, postgres.Classify("insert listens", err)
	}
	return inserted, nil
}

// EnsureTracks records track ids the importer has seen but knows nothing about.
//
// This is the seam that keeps ingestion independent of Spotify: the row is
// created in the 'pending' state and the enrichment workers fill it in later, so
// an API outage or a rate limit cannot delay or lose a listening record.
func (r *Repo) EnsureTracks(ctx context.Context, q store.Querier, trackIDs []string) error {
	if len(trackIDs) == 0 {
		return nil
	}
	const sql = `
        INSERT INTO tracks (id, metadata_state)
        SELECT DISTINCT id, 'pending' FROM unnest($1::text[]) AS t(id)
        ON CONFLICT (id) DO NOTHING`
	if _, err := q.Exec(ctx, sql, trackIDs); err != nil {
		return postgres.Classify("ensure tracks", err)
	}
	return nil
}

// EnsureAliases records normalised (artist, title) pairs from names-only exports
// so the alias resolver can look them up against Spotify's search API later.
func (r *Repo) EnsureAliases(ctx context.Context, q store.Querier, keys []domain.AliasKey) error {
	if len(keys) == 0 {
		return nil
	}
	artists := make([]string, 0, len(keys))
	titles := make([]string, 0, len(keys))
	for _, k := range keys {
		if k.IsZero() {
			continue
		}
		artists = append(artists, k.ArtistNorm)
		titles = append(titles, k.TitleNorm)
	}
	if len(artists) == 0 {
		return nil
	}
	const sql = `
        INSERT INTO track_aliases (artist_norm, title_norm, state)
        SELECT DISTINCT artist_norm, title_norm, 'pending'
        FROM unnest($1::text[], $2::text[]) AS t(artist_norm, title_norm)
        ON CONFLICT (artist_norm, title_norm) DO NOTHING`
	if _, err := q.Exec(ctx, sql, artists, titles); err != nil {
		return postgres.Classify("ensure aliases", err)
	}
	return nil
}

// ResolvedAliases looks up which of the given name pairs already map to a
// catalogue track.
//
// The importer calls this once per batch, before computing identity keys, so
// that a names-only record whose alias is already known is stored under the same
// identity as the equivalent record from an extended export. That is what makes
// the two export formats converge instead of double-counting.
func (r *Repo) ResolvedAliases(ctx context.Context, q store.Querier, keys []domain.AliasKey) (map[domain.AliasKey]string, error) {
	if len(keys) == 0 {
		return nil, nil
	}
	artists := make([]string, 0, len(keys))
	titles := make([]string, 0, len(keys))
	for _, k := range keys {
		if k.IsZero() {
			continue
		}
		artists = append(artists, k.ArtistNorm)
		titles = append(titles, k.TitleNorm)
	}
	if len(artists) == 0 {
		return nil, nil
	}
	const sql = `
        SELECT a.artist_norm, a.title_norm, a.track_id
        FROM track_aliases a
        JOIN unnest($1::text[], $2::text[]) AS t(artist_norm, title_norm)
          ON a.artist_norm = t.artist_norm AND a.title_norm = t.title_norm
        WHERE a.state = 'resolved' AND a.track_id IS NOT NULL`

	rows, err := q.Query(ctx, sql, artists, titles)
	if err != nil {
		return nil, postgres.Classify("resolve aliases", err)
	}
	defer rows.Close()

	out := make(map[domain.AliasKey]string)
	for rows.Next() {
		var k domain.AliasKey
		var trackID string
		if err := rows.Scan(&k.ArtistNorm, &k.TitleNorm, &trackID); err != nil {
			return nil, postgres.Classify("scan alias", err)
		}
		out[k] = trackID
	}
	if err := rows.Err(); err != nil {
		return nil, postgres.Classify("resolve aliases", err)
	}
	return out, nil
}

// CountListensForFile counts the rows a given import file actually put in the
// database. Post-import verification compares this against the importer's own
// tally, which is how a job whose records were never committed is caught.
func (r *Repo) CountListensForFile(ctx context.Context, q store.Querier, fileID uuid.UUID) (int64, error) {
	var n int64
	err := q.QueryRow(ctx,
		`SELECT count(*)::bigint FROM listens WHERE import_file_id = $1`, fileID.String()).Scan(&n)
	if err != nil {
		return 0, postgres.Classify("count listens for file", err)
	}
	return n, nil
}

// CountListensForUser is used by tests and the data-export endpoint.
func (r *Repo) CountListensForUser(ctx context.Context, q store.Querier, userID uuid.UUID) (int64, error) {
	var n int64
	err := q.QueryRow(ctx,
		`SELECT count(*)::bigint FROM listens WHERE user_id = $1`, userID.String()).Scan(&n)
	if err != nil {
		return 0, postgres.Classify("count listens for user", err)
	}
	return n, nil
}

// LatestListenAt returns the newest played_at for a user, which seeds the
// recently-played sync cursor for an account that has just imported history.
func (r *Repo) LatestListenAt(ctx context.Context, q store.Querier, userID uuid.UUID) (*time.Time, error) {
	var t *time.Time
	err := q.QueryRow(ctx,
		`SELECT max(played_at) FROM listens WHERE user_id = $1`, userID.String()).Scan(&t)
	if err != nil {
		return nil, postgres.Classify("latest listen", err)
	}
	return t, nil
}

// UnresolvedListen is one row awaiting relink to a catalogue track.
type UnresolvedListen struct {
	ID       int64
	UserID   uuid.UUID
	PlayedAt time.Time
}

// UnresolvedListensForIdentity pages through the listens that carry a given
// names-only identity. It is not user-scoped on purpose: resolving one alias
// repairs every user's history at once.
func (r *Repo) UnresolvedListensForIdentity(ctx context.Context, q store.Querier, identityKey []byte, afterID int64, limit int) ([]UnresolvedListen, error) {
	const sql = `
        SELECT id, user_id, played_at
        FROM listens
        WHERE identity_key = $1 AND track_id IS NULL AND id > $2
        ORDER BY id
        LIMIT $3`
	rows, err := q.Query(ctx, sql, identityKey, afterID, limit)
	if err != nil {
		return nil, postgres.Classify("list unresolved listens", err)
	}
	defer rows.Close()

	var out []UnresolvedListen
	for rows.Next() {
		var u UnresolvedListen
		if err := rows.Scan(&u.ID, &u.UserID, &u.PlayedAt); err != nil {
			return nil, postgres.Classify("scan unresolved listen", err)
		}
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		return nil, postgres.Classify("list unresolved listens", err)
	}
	return out, nil
}

// RelinkResult reports what a relink batch did.
type RelinkResult struct {
	Relinked int64
	Removed  int64
}

// ApplyRelink points a set of names-only listens at a real catalogue track,
// rewriting their identity and dedupe keys.
//
// A row whose new dedupe key collides with an existing listen is deleted rather
// than updated: the collision means the same event was already recorded through
// a source that knew the track URI, so keeping both would double-count it. This
// is layer three of the duplicate strategy described in docs/import.md.
func (r *Repo) ApplyRelink(ctx context.Context, tx pgx.Tx, rows []UnresolvedListen, trackID string, timezone string) (RelinkResult, error) {
	var res RelinkResult
	if len(rows) == 0 {
		return res, nil
	}
	if timezone == "" {
		timezone = "UTC"
	}

	newIdentity := domain.TrackIdentityFromID(trackID)
	newIdentityKey := newIdentity.Key()

	ids := make([]int64, len(rows))
	newKeys := make([][]byte, len(rows))
	for i, r := range rows {
		ids[i] = r.ID
		newKeys[i] = domain.DedupeKey(r.UserID, newIdentity, r.PlayedAt)
	}

	// Drop the rows that turn out to be duplicates once the identity is known.
	//
	// Two tests, mirroring exactly what the insert path would have concluded had
	// the track id been known at ingest time. The first is the exact key. The
	// second is the cross-source window, and it is not optional: an account-data
	// export truncates the stream end to the minute, so the derived start can sit
	// in a neighbouring bucket from the extended export's answer for the very
	// same play. Without it, relinking would leave that pair double-counted.
	const delSQL = `
        DELETE FROM listens l
        USING unnest($1::bigint[], $2::bytea[]) AS i(id, new_key)
        WHERE l.id = i.id
          AND l.track_id IS NULL
          AND (
              EXISTS (
                  SELECT 1 FROM listens e
                  WHERE e.user_id = l.user_id AND e.dedupe_key = i.new_key AND e.id <> l.id)
              OR EXISTS (
                  SELECT 1 FROM listens e
                  WHERE e.user_id = l.user_id
                    AND e.identity_key = $3
                    AND e.id <> l.id
                    AND e.source <> l.source
                    AND e.played_at >= l.played_at - interval '60 seconds'
                    AND e.played_at <= l.played_at + interval '60 seconds'
                    AND abs(extract(epoch FROM (e.played_at - l.played_at))) <= GREATEST(
                          CASE e.ts_precision WHEN 2 THEN 60 ELSE 10 END,
                          CASE l.ts_precision WHEN 2 THEN 60 ELSE 10 END))
          )`
	tag, err := tx.Exec(ctx, delSQL, ids, newKeys, newIdentityKey)
	if err != nil {
		return res, postgres.Classify("remove relink duplicates", err)
	}
	res.Removed = tag.RowsAffected()

	// Everything still standing is relinked in place, preserving the original
	// alias columns as provenance.
	const updSQL = `
        WITH upd AS (
            UPDATE listens l
            SET track_id = $3, identity_key = $4, dedupe_key = i.new_key
            FROM unnest($1::bigint[], $2::bytea[]) AS i(id, new_key)
            WHERE l.id = i.id AND l.track_id IS NULL
            RETURNING l.user_id, l.played_at
        ),
        dirty AS (
            INSERT INTO rollup_dirty_days (user_id, day)
            SELECT DISTINCT user_id, (played_at AT TIME ZONE $5::text)::date FROM upd
            ON CONFLICT DO NOTHING
        )
        SELECT count(*)::bigint FROM upd`
	if err := tx.QueryRow(ctx, updSQL, ids, newKeys, trackID, newIdentityKey, timezone).Scan(&res.Relinked); err != nil {
		return res, postgres.Classify("relink listens", err)
	}
	return res, nil
}

// DeleteListensForUser removes a user's entire history. Used by account deletion
// and by integration tests that need a clean slate.
func (r *Repo) DeleteListensForUser(ctx context.Context, q store.Querier, userID uuid.UUID) (int64, error) {
	tag, err := q.Exec(ctx, `DELETE FROM listens WHERE user_id = $1`, userID.String())
	if err != nil {
		return 0, postgres.Classify("delete listens", err)
	}
	return tag.RowsAffected(), nil
}
