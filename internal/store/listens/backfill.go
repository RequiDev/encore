package listens

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/RequiDev/encore/internal/postgres"
	"github.com/RequiDev/encore/internal/store"
)

// ObservationTolerance is how far past a play's end an observation may fall and
// still be taken as evidence about it.
//
// A named constant with its reasoning here rather than a literal buried in the
// query, because §2.5 of the phase design says this is the part most likely to
// need tuning against real data, and a number nobody can find is a number
// nobody tunes.
//
// Sixty seconds, and the number is borrowed rather than invented:
// insertListensSQL's cross-source duplicate probe already uses
// interval '60 seconds' as this repository's statement of how far apart two
// records of the same event can be. Deriving a second figure would let two
// definitions of temporal proximity drift apart without anything noticing.
//
// It bounds the *end* of the window, not the start. An observation before
// played_at cannot be of this play — played_at is the start of playback for
// every source (see migrations/00005) — so the window's lower bound is exact and
// only the upper one needs slack: for the request round trip, for the clock skew
// between Spotify's timestamp and Encore's own, and for a listener who paused for
// a moment mid-track.
//
// Widening it is not free. The tail is where this rule's one false-positive mode
// lives: two plays of the same track back to back — repeat-one, or a replay —
// have overlapping windows, and the most-recent observation inside the first
// play's window may have been taken during the second. Both plays share a device
// and a shuffle setting unless the listener changed one inside the tolerance, so
// the label is wrong only in that case; a wider tolerance makes that case more
// likely and buys nothing a poll interval below sixty seconds does not already
// give.
const ObservationTolerance = 60 * time.Second

// BackfillLookback bounds how far back one pass looks.
//
// Observations live twenty-four hours (accounts.ObservationRetention), and an
// observation at instant T can only match a play that started at or after
// T - duration - ObservationTolerance. Six hours of slack past the retention
// covers any single play a person can plausibly have — a DJ set, a full opera
// act, an audiobook chapter — so no observation that still exists is ever out of
// reach, while the scan stays on listens_user_played_idx and does not walk a
// decade of history on every sync tick.
//
// The slack is what makes the retention figure safe rather than merely lucky.
// Retention decides when an observation stops existing; this decides when a play
// stops being reachable. Keeping the second strictly larger means the only way
// evidence is ever lost is by ageing out — never by the play drifting out of
// reach while its observation is still on disk. That gap is what
// TestAReapedObservationCanNeverMatch puts a fixture in, and it fails loudly if
// a later edit closes it.
//
// Not derived from accounts.ObservationRetention by import: internal/store/listens
// must not depend on internal/store/accounts, and a constant that says thirty
// hours with the arithmetic written out is clearer than one that says
// "retention plus slack" and hides which is which.
const BackfillLookback = 30 * time.Hour

// backfillPlaybackContextSQL attaches what the now-playing poller saw to the
// plays it saw them during.
//
// It is an UPDATE and it will stay one. Encore's core guarantee is that
// re-running ingestion adds exactly zero rows, and domain.DedupeKey is computed
// from (user_id, identity_key, played_at) with playback context deliberately
// excluded — so a backfill that wrote through an INSERT would not be caught by
// the duplicate rules at all. Five things keep this idempotent, and they are
// deliberately redundant:
//
//  1. There is no INSERT and no ON CONFLICT. It cannot create a row.
//  2. The SET list is two columns. played_at, dedupe_key, identity_key,
//     track_id, ms_played and source are not among them, so nothing can move.
//  3. COALESCE(l.<col>, m.<col>) — a value already on the row always wins, so an
//     extended export's first-hand answer can never be overwritten by a fuzzy
//     match.
//  4. The candidate CTE only considers a row still missing one of the two
//     columns, so a second pass does not even look at an annotated row.
//  5. The final WHERE requires that this pass would actually change something,
//     so a row whose only gap the observation cannot fill reports zero rather
//     than writing a no-op.
//
// The match has three keys and only two of them appear in the ON clause. Track
// and instant are joined; the account is enforced by scoping both relations to
// $1 independently — once in obs, once in the candidate filter — so the two can
// never be about different people by the time they meet. Removing either scope
// is what TestAnotherUsersObservationNeverReachesThisListen exists to catch.
//
// The driving relation is the observation log, not listens. On an instance that
// never set ENCORE_NOWPLAYING_INTERVAL that CTE is empty after one index probe
// and the whole statement collapses — which is why this feature needs no switch
// of its own.
//
// DISTINCT ON (l.id) ... ORDER BY l.id, o.observed_at DESC is §2.4's "takes the
// most recent match", made deterministic: without it two observations inside one
// window would let the planner choose, and the same data would annotate
// differently on different days. One row is taken whole rather than each column
// being filled from the newest observation that happened to carry it, because
// the two columns describe one look at one player and recombining them across
// instants would report a state that never existed.
//
// rollup_dirty_days is deliberately not touched. listen_daily_rollup is keyed
// (user, day, track) and carries no context columns at all, so nothing this
// statement writes can make an aggregate stale.
//
// Parameters are $1 user, $2 tolerance in seconds, $3 the earliest played_at to
// consider.
const backfillPlaybackContextSQL = `
WITH obs AS (
    SELECT o.track_id, o.observed_at, o.shuffle, o.device_type
      FROM playback_observations o
     WHERE o.user_id = $1
),
matched AS (
    SELECT DISTINCT ON (l.id) l.id, o.shuffle, o.device_type
      FROM listens l
      JOIN obs o
        ON o.track_id = l.track_id
       AND o.observed_at >= l.played_at
       AND o.observed_at <= l.played_at
                          + (l.ms_played * interval '1 millisecond')
                          + ($2::double precision * interval '1 second')
     WHERE l.user_id = $1
       AND l.source = 0
       AND l.track_id IS NOT NULL
       AND l.played_at >= $3
       AND (l.shuffle IS NULL OR l.device_type IS NULL)
     ORDER BY l.id, o.observed_at DESC
)
UPDATE listens l
   SET shuffle     = COALESCE(l.shuffle, m.shuffle),
       device_type = COALESCE(l.device_type, m.device_type)
  FROM matched m
 WHERE l.id = m.id
   AND (   (l.shuffle     IS NULL AND m.shuffle     IS NOT NULL)
        OR (l.device_type IS NULL AND m.device_type IS NOT NULL))`

// BackfillPlaybackContext annotates one listener's recent live-synced plays with
// what the now-playing poller saw, and reports how many rows it changed.
//
// It creates nothing, moves nothing and duplicates nothing: see the statement's
// own comment for the five mechanisms that make that structural rather than
// conventional.
//
// A row with no matching observation keeps NULL in both columns, which is what
// migrations/00005 already means by NULL — "not reported", deliberately distinct
// from false. An observation that carries a device and no shuffle state fills
// one column and leaves the other NULL; it cannot state that a play was not
// shuffled about a fact nobody reported.
//
// now is passed rather than read so the caller's clock is the only one in play,
// and so a test can put a fixture on either side of BackfillLookback without
// sleeping.
func (r *Repo) BackfillPlaybackContext(
	ctx context.Context, q store.Querier, userID uuid.UUID, now time.Time,
) (int64, error) {
	tag, err := q.Exec(ctx, backfillPlaybackContextSQL,
		store.UUIDArg(userID),
		ObservationTolerance.Seconds(),
		now.UTC().Add(-BackfillLookback))
	if err != nil {
		return 0, postgres.Classify("backfill playback context", err)
	}
	return tag.RowsAffected(), nil
}
