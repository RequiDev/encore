package catalog

import (
	"context"
	"fmt"
	"time"

	"github.com/RequiDev/encore/internal/domain"
	"github.com/RequiDev/encore/internal/postgres"
	"github.com/RequiDev/encore/internal/store"
)

// aliasSelect reads a whole alias row. track_id is coalesced because
// domain.TrackAlias carries an empty string for "not resolved yet".
const aliasSelect = `
SELECT artist_norm, title_norm, coalesce(track_id, ''), state, fetch_attempts,
       next_attempt_at, resolved_at
FROM track_aliases`

func scanAlias(s scanner) (domain.TrackAlias, error) {
	var (
		a     domain.TrackAlias
		state string
	)
	if err := s.Scan(&a.ArtistNorm, &a.TitleNorm, &a.TrackID, &state, &a.FetchAttempts,
		&a.NextAttemptAt, &a.ResolvedAt); err != nil {
		return domain.TrackAlias{}, err
	}
	a.State = metadataState(state)
	return a, nil
}

// GetAlias reads one alias. It returns domain.ErrNotFound when the name pair has
// never been seen by an import.
func (r *Repo) GetAlias(ctx context.Context, q store.Querier, key domain.AliasKey) (domain.TrackAlias, error) {
	if key.IsZero() {
		return domain.TrackAlias{}, fmt.Errorf("%w: alias key is empty", domain.ErrValidation)
	}
	const sql = aliasSelect + ` WHERE artist_norm = $1 AND title_norm = $2`
	a, err := scanAlias(q.QueryRow(ctx, sql, key.ArtistNorm, key.TitleNorm))
	if err != nil {
		return domain.TrackAlias{}, postgres.Classify("get track alias", err)
	}
	return a, nil
}

// claimPendingAliasesSQL leases name pairs for resolution against Spotify's
// search API. It is the queue pattern from ClaimPending applied to the alias
// table, whose key is composite rather than a single id.
//
// The ORDER BY carries no tie-breaker on purpose: the partial index is on
// next_attempt_at alone, and adding a column would force a sort of every
// outstanding alias just to pick the oldest few. Ties are arbitrary, which
// SKIP LOCKED already makes harmless.
const claimPendingAliasesSQL = `
UPDATE track_aliases AS x
SET next_attempt_at = now() + make_interval(secs => $2::double precision)
WHERE (x.artist_norm, x.title_norm) IN (
    SELECT s.artist_norm, s.title_norm FROM track_aliases AS s
    WHERE s.state IN ('pending', 'failed')
      AND (s.next_attempt_at IS NULL OR s.next_attempt_at <= now())
    ORDER BY s.next_attempt_at NULLS FIRST
    LIMIT $1
    FOR UPDATE SKIP LOCKED
)
RETURNING x.artist_norm, x.title_norm`

// ClaimPendingAliases leases up to limit unresolved name pairs and returns their
// keys. Alias resolution costs one search request per pair, so callers keep the
// limit small and the lease generous.
func (r *Repo) ClaimPendingAliases(ctx context.Context, q store.Querier, limit int, leaseFor time.Duration) ([]domain.AliasKey, error) {
	if limit <= 0 {
		return nil, nil
	}
	limit = clampLimit(limit, maxClaim, maxClaim)
	if leaseFor <= 0 {
		leaseFor = defaultLease
	}

	rows, err := q.Query(ctx, claimPendingAliasesSQL, limit, leaseFor.Seconds())
	if err != nil {
		return nil, postgres.Classify("claim pending aliases", err)
	}
	defer rows.Close()

	var out []domain.AliasKey
	for rows.Next() {
		var k domain.AliasKey
		if err := rows.Scan(&k.ArtistNorm, &k.TitleNorm); err != nil {
			return nil, postgres.Classify("scan claimed alias", err)
		}
		out = append(out, k)
	}
	if err := rows.Err(); err != nil {
		return nil, postgres.Classify("claim pending aliases", err)
	}
	return out, nil
}

// resolveAliasSQL points a name pair at a real catalogue track.
//
// The track row is created first, in the same statement, because the resolver
// learns the id from /v1/search and the catalogue may never have seen it: the
// alias carries a foreign key to tracks, and the pending row also puts the new
// track in front of the track worker. The insert is conditional on the alias
// existing so that resolving an unknown key leaves nothing behind.
const resolveAliasSQL = `
WITH ensure_track AS (
    INSERT INTO tracks (id, metadata_state)
    SELECT $3, 'pending'
    WHERE EXISTS (SELECT 1 FROM track_aliases WHERE artist_norm = $1 AND title_norm = $2)
    ON CONFLICT (id) DO NOTHING
)
UPDATE track_aliases
SET state = 'resolved', track_id = $3, resolved_at = now(),
    next_attempt_at = NULL, fetch_attempts = 0
WHERE artist_norm = $1 AND title_norm = $2`

// ResolveAlias records that a name pair maps to trackID.
//
// It does not touch the listens that carry the alias; relinking them is a
// separate, resumable pass (see listens.ApplyRelink), because one alias can
// cover hundreds of thousands of rows across every user on the instance.
func (r *Repo) ResolveAlias(ctx context.Context, q store.Querier, key domain.AliasKey, trackID string) error {
	if key.IsZero() {
		return fmt.Errorf("%w: alias key is empty", domain.ErrValidation)
	}
	if trackID == "" {
		return fmt.Errorf("%w: track id is required to resolve an alias", domain.ErrValidation)
	}
	tag, err := q.Exec(ctx, resolveAliasSQL, key.ArtistNorm, key.TitleNorm, trackID)
	if err != nil {
		return postgres.Classify("resolve track alias", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("resolve track alias: %w", domain.ErrNotFound)
	}
	return nil
}

// markAliasUnavailableSQL parks a name pair Spotify's search cannot match. The
// state guard keeps a stale report from undoing a resolution another worker has
// since made.
const markAliasUnavailableSQL = `
UPDATE track_aliases
SET state = 'unavailable', next_attempt_at = NULL
WHERE artist_norm = $1 AND title_norm = $2 AND state IN ('pending', 'failed')`

// MarkAliasUnavailable records that Spotify's catalogue has no track for this
// name pair. The listens keep their names and stay unresolved, which is the
// honest outcome: they are real plays of something the catalogue cannot name.
func (r *Repo) MarkAliasUnavailable(ctx context.Context, q store.Querier, key domain.AliasKey) error {
	if key.IsZero() {
		return fmt.Errorf("%w: alias key is empty", domain.ErrValidation)
	}
	if _, err := q.Exec(ctx, markAliasUnavailableSQL, key.ArtistNorm, key.TitleNorm); err != nil {
		return postgres.Classify("mark alias unavailable", err)
	}
	return nil
}

// markAliasFailedSQL records a failed resolution attempt and schedules the next.
const markAliasFailedSQL = `
UPDATE track_aliases
SET fetch_attempts  = $3::int,
    next_attempt_at = $4::timestamptz,
    state           = CASE WHEN $3::int >= $5::int THEN 'failed' ELSE 'pending' END
WHERE artist_norm = $1 AND title_norm = $2 AND state IN ('pending', 'failed')`

// MarkAliasFailed records that a resolution attempt failed.
//
// attempts is the running total after this failure, and nextAttemptAt is when
// the alias becomes claimable again; a zero value is replaced with the domain
// backoff for that attempt number so the worker cannot spin on it.
func (r *Repo) MarkAliasFailed(ctx context.Context, q store.Querier, key domain.AliasKey, attempts int32, nextAttemptAt time.Time) error {
	if key.IsZero() {
		return fmt.Errorf("%w: alias key is empty", domain.ErrValidation)
	}
	if attempts < 1 {
		attempts = 1
	}
	if nextAttemptAt.IsZero() {
		nextAttemptAt = time.Now().Add(domain.NextMetadataAttempt(attempts))
	}
	_, err := q.Exec(ctx, markAliasFailedSQL, key.ArtistNorm, key.TitleNorm,
		attempts, nextAttemptAt.UTC(), int32(domain.BackoffAttempts))
	if err != nil {
		return postgres.Classify("mark alias failed", err)
	}
	return nil
}
