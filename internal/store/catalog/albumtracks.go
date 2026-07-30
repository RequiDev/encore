package catalog

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/RequiDev/encore/internal/domain"
	"github.com/RequiDev/encore/internal/postgres"
	"github.com/RequiDev/encore/internal/store"
)

// maxFailureReasonLen bounds what FailAlbumTrackFetch stores in last_error.
// The column is unbounded text, but a driver error carrying a whole HTTP
// response body has no business being replayed to an operator reading the
// table, matching accounts.maxSyncErrorLen's reasoning for the same figure.
const maxFailureReasonLen = 500

// The three states of one album's listing. See
// migrations/00013_album_tracks.sql for why the outcome lives in its own table
// rather than being inferred from whether album_tracks holds rows.
const (
	// AlbumTrackFetching is a lease: somebody is reading this listing now.
	AlbumTrackFetching = "fetching"
	// AlbumTrackOK means album_tracks holds a complete listing.
	AlbumTrackOK = "ok"
	// AlbumTrackFailed means the last attempt did not produce one. Whatever
	// album_tracks holds is from an earlier, successful attempt.
	AlbumTrackFailed = "failed"
)

// AlbumTrack is one row of a cached listing.
type AlbumTrack struct {
	TrackID     string
	Name        string
	DiscNumber  int
	TrackNumber int
}

// AlbumTrackState is the bookkeeping for one album's listing.
//
// The zero value — Status "" and both instants zero — is an album that has
// never been attempted, which is an ordinary state rather than an error: every
// album is in it until somebody first opens its page.
type AlbumTrackState struct {
	Status      string
	FetchedAt   time.Time
	AttemptedAt time.Time
	Attempts    int
}

const albumTrackStateSQL = `
SELECT status, coalesce(fetched_at, 'epoch'::timestamptz), attempted_at, attempts
FROM album_track_fetches
WHERE album_id = $1`

// AlbumTrackState reads the outcome of the last attempt on one album.
func (r *Repo) AlbumTrackState(ctx context.Context, q store.Querier, albumID string) (AlbumTrackState, error) {
	var (
		out     AlbumTrackState
		fetched time.Time
	)
	err := q.QueryRow(ctx, albumTrackStateSQL, albumID).
		Scan(&out.Status, &fetched, &out.AttemptedAt, &out.Attempts)
	if err != nil {
		if errors.Is(postgres.Classify("album track state", err), domain.ErrNotFound) {
			// Never attempted. Not an error: it is the state every album starts in,
			// and the caller's cue to start the first fetch.
			return AlbumTrackState{}, nil
		}
		return AlbumTrackState{}, postgres.Classify("album track state", err)
	}
	// 'epoch' stands in for NULL so the scan needs no pointer. Anything at or
	// before it means "no successful fetch yet".
	if fetched.Year() > 1970 {
		out.FetchedAt = fetched.UTC()
	}
	out.AttemptedAt = out.AttemptedAt.UTC()
	return out, nil
}

// albumTracksSQL reads one album's listing in playing order.
//
// track_id breaks ties so the order is total: two rows sharing a disc and track
// number would otherwise come back in whatever order the heap happened to hold
// them, and a list that reshuffles between page views looks broken.
const albumTracksSQL = `
SELECT track_id, name, disc_number, track_number
FROM album_tracks
WHERE album_id = $1
ORDER BY disc_number, track_number, track_id`

// AlbumTracks reads one album's cached listing.
func (r *Repo) AlbumTracks(ctx context.Context, q store.Querier, albumID string) ([]AlbumTrack, error) {
	rows, err := q.Query(ctx, albumTracksSQL, albumID)
	if err != nil {
		return nil, postgres.Classify("album tracks", err)
	}
	defer rows.Close()

	out := make([]AlbumTrack, 0, 16)
	for rows.Next() {
		var t AlbumTrack
		if err := rows.Scan(&t.TrackID, &t.Name, &t.DiscNumber, &t.TrackNumber); err != nil {
			return nil, postgres.Classify("album tracks", err)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, postgres.Classify("album tracks", err)
	}
	return out, nil
}

// claimAlbumTrackFetchSQL takes the lease on one album, or returns nothing.
//
// The WHERE applies only to the DO UPDATE branch: an album with no row at all
// is claimed by the INSERT. An album already 'fetching' is claimed only once
// its attempt has outlived the lease, which is what stops two tabs, two
// browsers or two API replicas each spending a request on the same album — and
// what stops a process killed mid-fetch stranding the album for ever.
//
// This is one statement, not a read followed by a write: Postgres resolves a
// conflicting INSERT ... ON CONFLICT by taking a lock on the existing row and
// making every other transaction targeting the same key wait for it, so two
// concurrent callers are serialised by the database itself rather than by
// anything this Go code does. The loser's DO UPDATE ... WHERE evaluates to
// false once it sees the winner's committed row, so RETURNING is empty for it
// — there is no uniqueness violation to catch, and nothing here decides who
// won by inspecting an error.
//
// Parameters are $1 album, $2 now, $3 the lease cutoff (now minus the lease).
const claimAlbumTrackFetchSQL = `
INSERT INTO album_track_fetches (album_id, status, attempted_at, attempts)
VALUES ($1, 'fetching', $2, 1)
ON CONFLICT (album_id) DO UPDATE SET
    status       = 'fetching',
    attempted_at = $2,
    attempts     = album_track_fetches.attempts + 1
WHERE album_track_fetches.status <> 'fetching'
   OR album_track_fetches.attempted_at < $3
RETURNING album_id`

// ClaimAlbumTrackFetch takes the lease, reporting whether this caller won it.
func (r *Repo) ClaimAlbumTrackFetch(
	ctx context.Context, q store.Querier, albumID string, now, leaseCutoff time.Time,
) (bool, error) {
	var got string
	err := q.QueryRow(ctx, claimAlbumTrackFetchSQL, albumID, now.UTC(), leaseCutoff.UTC()).Scan(&got)
	if err != nil {
		classified := postgres.Classify("claim album track fetch", err)
		if errors.Is(classified, domain.ErrNotFound) {
			// No row came back: somebody else holds a live lease.
			return false, nil
		}
		return false, classified
	}
	return true, nil
}

// replaceAlbumTracksSQL deletes whatever the incoming listing no longer
// contains and upserts the rest, in one statement — the same
// delete-absent-plus-upsert-present shape as ReplaceUserPlaylists in
// internal/store/library/playlists.go. Every column besides the key can change
// under a track Encore already knows about (a remaster renames it, a re-issue
// renumbers it), so ON CONFLICT refreshes all of them.
//
// DISTINCT ON collapses a duplicate id within one call, because Postgres
// refuses to let ON CONFLICT touch the same row twice inside one statement and
// a page boundary could in principle repeat one.
//
// The DELETE and the INSERT share one statement, hence one implicit
// transaction: there is no instant at which a concurrent reader can observe
// the tail deleted but the rest not yet upserted, and no instant between the
// two halves for a crash to land in.
//
// **Callers must never pass a partial listing here.** The delete is what makes
// that fatal: a prefix deletes the tail of a listing that was correct. It is
// also the caller's job to run this and MarkAlbumTracksFetched inside the same
// Store.InTx — see the comment on that function.
//
// An empty listing is refused before this statement ever runs — see
// ReplaceAlbumTracks — because track_id <> ALL('{}') is vacuously true and
// would otherwise delete every row an album has, which is indistinguishable
// on disk from "Spotify says this album has no tracks", a claim
// migrations/00013_album_tracks.sql says has no representation and must be
// recorded as a failure instead.
//
// Parameters are $1 album, $2..$5 the parallel arrays.
const replaceAlbumTracksSQL = `
WITH input AS (
    SELECT DISTINCT ON (track_id) *
    FROM unnest($2::text[], $3::text[], $4::int[], $5::int[])
        AS t(track_id, name, disc_number, track_number)
    ORDER BY track_id
),
stale AS (
    DELETE FROM album_tracks
    WHERE album_id = $1 AND track_id <> ALL($2::text[])
)
INSERT INTO album_tracks (album_id, track_id, name, disc_number, track_number)
SELECT $1, track_id, name, disc_number, track_number FROM input
ON CONFLICT (album_id, track_id) DO UPDATE SET
    name         = EXCLUDED.name,
    disc_number  = EXCLUDED.disc_number,
    track_number = EXCLUDED.track_number`

// ReplaceAlbumTracks makes items the album's complete listing.
//
// An empty (or all-blank-id) items is refused rather than stored: this is the
// one call that can make album_tracks disappear, and "Spotify listed zero
// tracks for this album" is not a state migrations/00013_album_tracks.sql
// allows — it treats a 200 with no items the same as any other failed read.
// A caller that reached this with an empty listing should have called
// FailAlbumTrackFetch instead; refusing here means it cannot get that wrong
// by accident, even for a listing that was genuinely truncated to nothing.
func (r *Repo) ReplaceAlbumTracks(
	ctx context.Context, q store.Querier, albumID string, items []AlbumTrack,
) error {
	ids, names, discs, numbers := albumTrackRows(items)
	if len(ids) == 0 {
		return fmt.Errorf("replace album tracks: %w: refusing to store an empty listing for %q",
			domain.ErrValidation, albumID)
	}
	if _, err := q.Exec(ctx, replaceAlbumTracksSQL, albumID, ids, names, discs, numbers); err != nil {
		return postgres.Classify("replace album tracks", err)
	}
	return nil
}

// albumTrackRows transposes a listing into the parallel arrays unnest expects,
// dropping entries with a blank id — a keyless row has nothing for ON CONFLICT
// to place. Every slice is non-nil even when items is empty, so an empty batch
// reaches the statement as an empty array rather than SQL NULL.
func albumTrackRows(items []AlbumTrack) (ids, names []string, discs, numbers []int32) {
	ids = make([]string, 0, len(items))
	names = make([]string, 0, len(items))
	discs = make([]int32, 0, len(items))
	numbers = make([]int32, 0, len(items))
	for _, it := range items {
		if it.TrackID == "" {
			continue
		}
		ids = append(ids, it.TrackID)
		names = append(names, it.Name)
		discs = append(discs, int32(it.DiscNumber))
		numbers = append(numbers, int32(it.TrackNumber))
	}
	return ids, names, discs, numbers
}

// markAlbumTracksFetchedSQL records a success. It clears last_error so a stale
// message cannot be read beside a listing that is now current, and resets
// attempts to 1: migrations/00013_album_tracks.sql names attempts as what
// drives the retry backoff after a failure, and a healthy album that once
// failed five times before succeeding must not have the next transient error
// backed off as though it were a sixth.
const markAlbumTracksFetchedSQL = `
INSERT INTO album_track_fetches (album_id, status, fetched_at, attempted_at, attempts, last_error)
VALUES ($1, 'ok', $2, $2, 1, '')
ON CONFLICT (album_id) DO UPDATE SET
    status     = 'ok',
    fetched_at = $2,
    attempts   = 1,
    last_error = ''`

// MarkAlbumTracksFetched records that the listing now stored is complete.
//
// It is deliberately separate from ReplaceAlbumTracks rather than folded into
// it: the caller runs both inside one Store.InTx, so the listing and the claim
// that it is authoritative commit together or not at all. Splitting the two
// writes across separate transactions would let a crash land between them,
// leaving a listing on disk with no 'ok' beside it, or an 'ok' beside a
// listing an interrupted replace never finished writing; a single transaction
// makes that interval unobservable.
func (r *Repo) MarkAlbumTracksFetched(
	ctx context.Context, q store.Querier, albumID string, at time.Time,
) error {
	if _, err := q.Exec(ctx, markAlbumTracksFetchedSQL, albumID, at.UTC()); err != nil {
		return postgres.Classify("mark album tracks fetched", err)
	}
	return nil
}

// failAlbumTrackFetchSQL records a failed attempt.
//
// It touches neither album_tracks nor fetched_at. Whatever listing is stored
// stays stored and keeps saying when it was read: a timeout today is no reason
// to throw away a listing that was correct last month, and an empty listing is
// exactly the "this album has no tracks" claim this feature must never make.
const failAlbumTrackFetchSQL = `
INSERT INTO album_track_fetches (album_id, status, attempted_at, attempts, last_error)
VALUES ($1, 'failed', $2, 1, $3)
ON CONFLICT (album_id) DO UPDATE SET
    status       = 'failed',
    attempted_at = $2,
    last_error   = $3`

// FailAlbumTrackFetch records that the last attempt did not produce a listing.
func (r *Repo) FailAlbumTrackFetch(
	ctx context.Context, q store.Querier, albumID string, at time.Time, reason string,
) error {
	// store.Truncate cuts on a rune boundary. A byte offset could slice a
	// multi-byte rune in half and hand Postgres bytes it rejects outright,
	// which would fail the very write meant to record that the fetch failed —
	// the row stays 'fetching' from the claim, the lease eventually expires,
	// a new claim wins, the same fetch fails the same way, and the write is
	// rejected again: a permanent strand disguised as a retry loop.
	reason = store.Truncate(reason, maxFailureReasonLen)
	if _, err := q.Exec(ctx, failAlbumTrackFetchSQL, albumID, at.UTC(), reason); err != nil {
		return postgres.Classify("fail album track fetch", err)
	}
	return nil
}
