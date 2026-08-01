package accounts

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/RequiDev/encore/internal/domain"
	"github.com/RequiDev/encore/internal/postgres"
	"github.com/RequiDev/encore/internal/store"
)

// nowPlayingTextLimit bounds the three columns Spotify's own strings reach.
//
// Long enough for any real title, artist list or device name, short enough that
// a malformed response cannot fill the table. Applied through store.Truncate,
// which is rune-safe: a byte-boundary cut through a multi-byte rune would make
// the write that records the observation itself fail.
const nowPlayingTextLimit = 200

// scopeReadPlaybackState is the grant this feature needs. It shipped in
// config.DefaultScopes() in Phase 2a, so an account connected since then
// already has it and one connected before does not — the ordinary state of an
// older account forever, not a fault to repair.
const scopeReadPlaybackState = "user-read-playback-state"

// NowPlaying is the repository for now_playing: one row per user holding the
// last observation of their player and the time of the last attempt.
type NowPlaying struct{ db *store.Store }

// NewNowPlaying builds the repository.
func NewNowPlaying(db *store.Store) *NowPlaying { return &NowPlaying{db: db} }

// DueAccount is one account the now-playing poller may check, with the scopes
// its grant carries.
//
// The scopes come back even though the query below already filters on them, so
// the poller can make the same check itself before spending a request. That is
// defence in depth in the shape internal/library already uses: the SQL predicate
// keeps a scopeless account out of the queue, and the in-code check is what
// still holds if somebody widens the predicate later.
type DueAccount struct {
	UserID uuid.UUID
	Scopes []string
}

// listDueSQL drives the now-playing poller's queue.
//
// Three exclusions, each for its own reason:
//
//   - needs_reauth, because a broken refresh token fails identically at every
//     endpoint and polling it would spend the instance's budget rediscovering
//     an answer only the listener can give;
//   - grants without user-read-playback-state, because the request would 403
//     and a 403 costs a request to be told something the stored grant already
//     says. The @> operator works here for the reason
//     listDueForLibrarySyncSQL documents: every write path splits granted
//     scopes into separate array elements, and the one legacy shape that holds
//     them space-joined necessarily predates this scope entirely;
//   - accounts checked within the last interval, so two worker processes share
//     the work without coordinating and a restart re-polls nobody early.
//
// NULLS FIRST puts a newly connected account at the head of the queue, so its
// card fills in on the next tick rather than behind everybody else.
const listDueSQL = `
    SELECT c.user_id, c.scopes
      FROM spotify_credentials c
      LEFT JOIN now_playing n ON n.user_id = c.user_id
     WHERE c.sync_state <> 'needs_reauth'
       AND c.scopes @> ARRAY['` + scopeReadPlaybackState + `']::text[]
       AND (n.checked_at IS NULL OR n.checked_at < $1)
     ORDER BY n.checked_at ASC NULLS FIRST
     LIMIT $2`

// ListDue returns the accounts whose player has not been checked since
// olderThan.
func (r *NowPlaying) ListDue(
	ctx context.Context, q store.Querier, olderThan time.Time, limit int,
) ([]DueAccount, error) {
	rows, err := q.Query(ctx, listDueSQL, olderThan.UTC(), clampLimit(limit))
	if err != nil {
		return nil, postgres.Classify("list accounts due for a playback check", err)
	}
	defer rows.Close()

	var out []DueAccount
	for rows.Next() {
		var a DueAccount
		if err := rows.Scan(&a.UserID, &a.Scopes); err != nil {
			return nil, postgres.Classify("scan account due for a playback check", err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, postgres.Classify("list accounts due for a playback check", err)
	}
	return out, nil
}

// getSQL reads one account's row, and answers in the same statement whether the
// track it names is one Encore's own catalogue holds.
//
// The join is here rather than in the handler because it is the same question
// as the row: "what is playing, and can it be linked". Two statements would let
// the two answers come from different instants.
const getSQL = `
    SELECT n.observed_at, n.state, n.kind, n.track_id, n.title, n.artist,
           n.progress_ms, n.duration_ms, n.device_name, n.checked_at, n.failed,
           (t.id IS NOT NULL) AS track_known
      FROM now_playing n
      LEFT JOIN tracks t ON t.id = n.track_id
     WHERE n.user_id = $1`

// Get returns one account's last observation, or domain.ErrNotFound when the
// poller has never reached it.
//
// ErrNotFound rather than a zero value on purpose: "no row" and "a row saying
// nothing is playing" are different answers, and a caller that received a zero
// value for both would have no way to tell them apart.
func (r *NowPlaying) Get(
	ctx context.Context, q store.Querier, userID uuid.UUID,
) (domain.NowPlaying, error) {
	var (
		n                    domain.NowPlaying
		state, kind          string
		trackID              *string
		observedAt           *time.Time
		progress, durationMs *int32
	)
	err := q.QueryRow(ctx, getSQL, store.UUIDArg(userID)).Scan(
		&observedAt, &state, &kind, &trackID, &n.Title, &n.Artist,
		&progress, &durationMs, &n.DeviceName, &n.CheckedAt, &n.Failed, &n.TrackKnown)
	if err != nil {
		return domain.NowPlaying{}, postgres.Classify("get now playing", err)
	}

	n.State = domain.PlaybackState(state)
	n.Kind = domain.PlaybackItemKind(kind)
	if observedAt != nil {
		n.ObservedAt = observedAt.UTC()
	}
	n.CheckedAt = n.CheckedAt.UTC()
	if trackID != nil {
		n.TrackID = *trackID
	}
	if progress != nil {
		v := int(*progress)
		n.ProgressMs = &v
	}
	if durationMs != nil {
		v := int(*durationMs)
		n.DurationMs = &v
	}
	return n, nil
}

// recordSQL stores a successful observation. checked_at is the observation's own
// instant: a check that succeeded happened exactly when what it saw was true.
const recordSQL = `
    INSERT INTO now_playing (user_id, observed_at, state, kind, track_id, title, artist,
                             progress_ms, duration_ms, device_name, checked_at, failed)
    VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $2, false)
    ON CONFLICT (user_id) DO UPDATE SET
        observed_at = EXCLUDED.observed_at,
        state       = EXCLUDED.state,
        kind        = EXCLUDED.kind,
        track_id    = EXCLUDED.track_id,
        title       = EXCLUDED.title,
        artist      = EXCLUDED.artist,
        progress_ms = EXCLUDED.progress_ms,
        duration_ms = EXCLUDED.duration_ms,
        device_name = EXCLUDED.device_name,
        checked_at  = EXCLUDED.checked_at,
        failed      = false`

// Record stores a successful observation, replacing whatever was there.
func (r *NowPlaying) Record(
	ctx context.Context, q store.Querier, userID uuid.UUID, n domain.NowPlaying,
) error {
	var trackID *string
	if n.TrackID != "" {
		id := n.TrackID
		trackID = &id
	}
	_, err := q.Exec(ctx, recordSQL, store.UUIDArg(userID), n.ObservedAt.UTC(),
		string(n.State), string(n.Kind), trackID,
		store.Truncate(n.Title, nowPlayingTextLimit),
		store.Truncate(n.Artist, nowPlayingTextLimit),
		n.ProgressMs, n.DurationMs,
		store.Truncate(n.DeviceName, nowPlayingTextLimit))
	if err != nil {
		return postgres.Classify("record now playing", err)
	}
	return nil
}

// recordFailureSQL moves only the two columns that describe the attempt.
//
// The observation columns are deliberately untouched, which is what lets the
// interface say "the last check failed; this is what you were playing four
// minutes ago" rather than discarding a true thing because a later request went
// wrong. On a first insert they take the table's defaults, which is the
// never-observed state.
const recordFailureSQL = `
    INSERT INTO now_playing (user_id, checked_at, failed)
    VALUES ($1, $2, true)
    ON CONFLICT (user_id) DO UPDATE SET
        checked_at = EXCLUDED.checked_at,
        failed     = true`

// RecordFailure notes that a check was attempted at t and did not succeed.
func (r *NowPlaying) RecordFailure(
	ctx context.Context, q store.Querier, userID uuid.UUID, t time.Time,
) error {
	if _, err := q.Exec(ctx, recordFailureSQL, store.UUIDArg(userID), t.UTC()); err != nil {
		return postgres.Classify("record a failed playback check", err)
	}
	return nil
}
