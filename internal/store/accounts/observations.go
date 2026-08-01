package accounts

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/RequiDev/encore/internal/domain"
	"github.com/RequiDev/encore/internal/postgres"
	"github.com/RequiDev/encore/internal/store"
)

// ObservationRetention is how long a playback observation is kept.
//
// Twenty-four hours, per the phase design: long enough that any sync interval
// an operator could reasonably set has run several times over since the play,
// short enough that the table stays small and never becomes a second listening
// history nobody meant to keep. An observation older than this can no longer
// reach any listen the backfill will look at, so keeping it costs storage and
// buys nothing.
const ObservationRetention = 24 * time.Hour

// observationTextLimit bounds the two columns Spotify's own strings reach.
//
// Applied through store.Truncate, which is rune-safe: a byte-boundary cut
// through a multi-byte rune would make the write that records the observation
// itself fail, which is a worse outcome than a shortened device name.
const observationTextLimit = 100

// PlaybackObservations is the repository for playback_observations: a
// short-lived log of what the now-playing poller saw, written by that poller
// and read by the backfill in internal/store/listens.
//
// It holds nothing that can create a listen. That is not incidental — the
// poller reaches this type through a narrow interface precisely so that its
// dependency closure never acquires anything that writes to listens.
type PlaybackObservations struct{ db *store.Store }

// NewPlaybackObservations builds the repository.
func NewPlaybackObservations(db *store.Store) *PlaybackObservations {
	return &PlaybackObservations{db: db}
}

// logSQL appends one observation.
//
// DO NOTHING rather than DO UPDATE: two writes at the same (user, track,
// instant) describe the same look at the same player, so the second has nothing
// to add, and an UPDATE would let a retry silently rewrite evidence a backfill
// may already have used.
const logSQL = `
    INSERT INTO playback_observations
        (user_id, track_id, observed_at, device_type, device_name, shuffle)
    VALUES ($1, $2, $3, $4, $5, $6)
    ON CONFLICT (user_id, track_id, observed_at) DO NOTHING`

// Log records one observation.
func (r *PlaybackObservations) Log(
	ctx context.Context, q store.Querier, userID uuid.UUID, o domain.PlaybackObservation,
) error {
	_, err := q.Exec(ctx, logSQL, store.UUIDArg(userID), o.TrackID, o.ObservedAt.UTC(),
		store.Nullable(store.Truncate(o.DeviceType, observationTextLimit)),
		store.Nullable(store.Truncate(o.DeviceName, observationTextLimit)),
		o.Shuffle)
	if err != nil {
		return postgres.Classify("log a playback observation", err)
	}
	return nil
}

// deleteExpiredSQL removes observations too old to reach any listen.
//
// Bounded by an age predicate and nothing else. There is no "delete what is not
// in this set" here, deliberately: reconciliation against a supplied set is how
// this repository has lost data three times, and an observation log needs
// nothing of the sort — a row's own timestamp already says whether it has
// outlived its usefulness.
const deleteExpiredSQL = `DELETE FROM playback_observations WHERE observed_at < $1`

// DeleteExpired removes observations made before olderThan and reports how many
// went.
//
// The caller passes now minus ObservationRetention. A zero time deletes nothing,
// which is the safe direction for a mistake; the test that pins this asserts a
// fresh observation survives a reap rather than asserting a count, because a
// count can be satisfied by a query that deleted the wrong rows.
func (r *PlaybackObservations) DeleteExpired(
	ctx context.Context, q store.Querier, olderThan time.Time,
) (int64, error) {
	tag, err := q.Exec(ctx, deleteExpiredSQL, olderThan.UTC())
	if err != nil {
		return 0, postgres.Classify("delete expired playback observations", err)
	}
	return tag.RowsAffected(), nil
}
