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

// maxDiscographyErrorLen bounds what FailArtistAlbumFetch stores in last_error,
// matching maxFailureReasonLen next door and accounts.maxSyncErrorLen before
// it: a driver error carrying a whole HTTP response body has no business being
// replayed to an operator reading the table.
const maxDiscographyErrorLen = 500

// The three states of one artist's discography. See
// migrations/00014_artist_albums.sql for why the outcome lives in its own table
// rather than being inferred from whether artist_albums holds rows.
const (
	// ArtistAlbumFetching is a lease: somebody is reading this discography now.
	ArtistAlbumFetching = "fetching"
	// ArtistAlbumOK means artist_albums holds a complete listing.
	ArtistAlbumOK = "ok"
	// ArtistAlbumFailed means the last attempt did not produce one. Whatever
	// artist_albums holds is from an earlier, successful attempt.
	ArtistAlbumFailed = "failed"
)

// Spotify's four documented album_group values.
//
// They describe what the *artist* is to the record, not what the record is:
// album_type says "album" for a record this artist merely appears on, whereas
// album_group says "appears_on". Coverage is taken over the group for exactly
// that reason.
//
// These are the four Spotify documents, not a closed set the database enforces
// — see the migration's note on why artist_albums.album_group has no CHECK.
const (
	AlbumGroupAlbum       = "album"
	AlbumGroupSingle      = "single"
	AlbumGroupCompilation = "compilation"
	AlbumGroupAppearsOn   = "appears_on"
)

// ArtistAlbum is one release of a cached discography.
type ArtistAlbum struct {
	AlbumID string
	Name    string
	// Group is Spotify's album_group, stored verbatim. Every group is kept and
	// the filter is applied by the reader; see the migration.
	Group            string
	ReleaseDate      *time.Time
	ReleasePrecision string
	Position         int
}

// ArtistAlbumState is the bookkeeping for one artist's discography.
//
// The zero value — Status "" and both instants zero — is an artist who has
// never been attempted, which is an ordinary state rather than an error: every
// artist is in it until somebody first opens their page.
type ArtistAlbumState struct {
	Status      string
	FetchedAt   time.Time
	AttemptedAt time.Time
	Attempts    int
}

const artistAlbumStateSQL = `
SELECT status, coalesce(fetched_at, 'epoch'::timestamptz), attempted_at, attempts
FROM artist_album_fetches
WHERE artist_id = $1`

// ArtistAlbumState reads the outcome of the last attempt on one artist.
func (r *Repo) ArtistAlbumState(
	ctx context.Context, q store.Querier, artistID string,
) (ArtistAlbumState, error) {
	var (
		out     ArtistAlbumState
		fetched time.Time
	)
	err := q.QueryRow(ctx, artistAlbumStateSQL, artistID).
		Scan(&out.Status, &fetched, &out.AttemptedAt, &out.Attempts)
	if err != nil {
		if errors.Is(postgres.Classify("artist album state", err), domain.ErrNotFound) {
			// Never attempted. Not an error: it is the state every artist starts
			// in, and the caller's cue to start the first fetch.
			return ArtistAlbumState{}, nil
		}
		return ArtistAlbumState{}, postgres.Classify("artist album state", err)
	}
	// 'epoch' stands in for NULL so the scan needs no pointer. Anything at or
	// before it means "no successful fetch yet".
	if fetched.Year() > 1970 {
		out.FetchedAt = fetched.UTC()
	}
	out.AttemptedAt = out.AttemptedAt.UTC()
	return out, nil
}

// artistAlbumsSQL reads one artist's discography, newest first.
//
// Newest first because the question the page asks is "what of theirs have I not
// got to yet", and a reader scanning that list starts from the recent end.
// NULLS LAST puts undated releases after everything dated rather than at the
// top, where a missing date would masquerade as the newest record.
//
// position and album_id break ties so the order is total: two releases sharing
// a date would otherwise come back in whatever order the heap happened to hold
// them, and a list that reshuffles between page views looks broken.
const artistAlbumsSQL = `
SELECT album_id, name, album_group, release_date, release_precision
FROM artist_albums
WHERE artist_id = $1
ORDER BY release_date DESC NULLS LAST, position, album_id`

// ArtistAlbums reads one artist's cached discography, every group included.
//
// Filtering to album_group = 'album' is deliberately not done here. The caller
// needs the excluded groups to say what it set aside — "4 of 11 albums" with no
// mention of 340 singles is an overclaim — and a repository that dropped them
// would make that sentence unwriteable.
func (r *Repo) ArtistAlbums(
	ctx context.Context, q store.Querier, artistID string,
) ([]ArtistAlbum, error) {
	rows, err := q.Query(ctx, artistAlbumsSQL, artistID)
	if err != nil {
		return nil, postgres.Classify("artist albums", err)
	}
	defer rows.Close()

	out := make([]ArtistAlbum, 0, 32)
	for rows.Next() {
		var (
			a    ArtistAlbum
			date *time.Time
		)
		if err := rows.Scan(&a.AlbumID, &a.Name, &a.Group, &date, &a.ReleasePrecision); err != nil {
			return nil, postgres.Classify("artist albums", err)
		}
		if date != nil {
			utc := date.UTC()
			a.ReleaseDate = &utc
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, postgres.Classify("artist albums", err)
	}
	return out, nil
}

// claimArtistAlbumFetchSQL takes the lease on one artist, or returns nothing.
//
// The WHERE applies only to the DO UPDATE branch: an artist with no row at all
// is claimed by the INSERT. An artist already 'fetching' is claimed only once
// its attempt has outlived the lease, which is what stops two tabs, two
// browsers or two API replicas each spending a whole discography walk — several
// requests each, not one — on the same artist, and what stops a process killed
// mid-fetch stranding the artist for ever.
//
// This is one statement, not a read followed by a write: Postgres resolves a
// conflicting INSERT ... ON CONFLICT by taking a lock on the existing row and
// making every other transaction targeting the same key wait for it, so two
// concurrent callers are serialised by the database itself rather than by
// anything this Go code does. The loser's DO UPDATE ... WHERE evaluates to
// false once it sees the winner's committed row, so RETURNING is empty for it —
// there is no uniqueness violation to catch, and nothing here decides who won
// by inspecting an error.
//
// Parameters are $1 artist, $2 now, $3 the lease cutoff (now minus the lease).
const claimArtistAlbumFetchSQL = `
INSERT INTO artist_album_fetches (artist_id, status, attempted_at, attempts)
VALUES ($1, 'fetching', $2, 1)
ON CONFLICT (artist_id) DO UPDATE SET
    status       = 'fetching',
    attempted_at = $2,
    attempts     = artist_album_fetches.attempts + 1
WHERE artist_album_fetches.status <> 'fetching'
   OR artist_album_fetches.attempted_at < $3
RETURNING artist_id`

// ClaimArtistAlbumFetch takes the lease, reporting whether this caller won it.
func (r *Repo) ClaimArtistAlbumFetch(
	ctx context.Context, q store.Querier, artistID string, now, leaseCutoff time.Time,
) (bool, error) {
	var got string
	err := q.QueryRow(ctx, claimArtistAlbumFetchSQL, artistID, now.UTC(), leaseCutoff.UTC()).Scan(&got)
	if err != nil {
		classified := postgres.Classify("claim artist album fetch", err)
		if errors.Is(classified, domain.ErrNotFound) {
			// No row came back: somebody else holds a live lease.
			return false, nil
		}
		return false, classified
	}
	return true, nil
}

// replaceArtistAlbumsSQL deletes whatever the incoming discography no longer
// contains and upserts the rest, in one statement — the same
// delete-absent-plus-upsert-present shape as ReplaceAlbumTracks in
// internal/store/catalog/albumtracks.go and ReplaceUserPlaylists in
// internal/store/library/playlists.go.
//
// Every column besides the key can change under a release Encore already knows
// about: a re-issue renames it, a re-dating moves it, and Spotify does
// reclassify a record's album_group. ON CONFLICT therefore refreshes all of
// them — and album_group in particular, because a release reclassified from
// 'album' to 'compilation' that kept its old group would keep counting towards
// completion for ever.
//
// DISTINCT ON collapses a duplicate id within one call, because Postgres
// refuses to let ON CONFLICT touch the same row twice inside one statement and
// a page boundary could in principle repeat one.
//
// The DELETE and the INSERT share one statement, hence one implicit
// transaction: there is no instant at which a concurrent reader can observe the
// tail deleted but the rest not yet upserted.
//
// **Callers must never pass a partial listing here.** The delete is what makes
// that fatal: a prefix deletes the tail of a discography that was correct. It
// is also the caller's job to run this and MarkArtistAlbumsFetched inside the
// same Store.InTx — see the comment on that function.
//
// An empty listing is refused before this statement ever runs — see
// ReplaceArtistAlbums — because album_id <> ALL('{}') is vacuously true and
// would otherwise delete every row an artist has.
//
// Parameters are $1 artist, $2..$7 the parallel arrays.
const replaceArtistAlbumsSQL = `
WITH input AS (
    SELECT DISTINCT ON (album_id) *
    FROM unnest($2::text[], $3::text[], $4::text[], $5::date[], $6::text[], $7::int[])
        AS t(album_id, name, album_group, release_date, release_precision, position)
    ORDER BY album_id
),
stale AS (
    DELETE FROM artist_albums
    WHERE artist_id = $1 AND album_id <> ALL($2::text[])
)
INSERT INTO artist_albums
    (artist_id, album_id, name, album_group, release_date, release_precision, position)
SELECT $1, album_id, name, album_group, release_date, release_precision, position FROM input
ON CONFLICT (artist_id, album_id) DO UPDATE SET
    name              = EXCLUDED.name,
    album_group       = EXCLUDED.album_group,
    release_date      = EXCLUDED.release_date,
    release_precision = EXCLUDED.release_precision,
    position          = EXCLUDED.position`

// ReplaceArtistAlbums makes items the artist's complete discography.
//
// An empty (or all-blank-id) items is refused rather than stored: this is the
// one call that can make artist_albums disappear, and "Spotify listed nothing
// for this artist" is not a state migrations/00014_artist_albums.sql allows —
// it treats a 200 with no items the same as any other failed read. A caller
// that reached this with an empty listing should have called
// FailArtistAlbumFetch instead; refusing here means it cannot get that wrong by
// accident, even for a listing that was genuinely truncated to nothing.
//
// Note what this does *not* refuse: a discography whose every release is a
// single. That is an ordinary artist and an ordinary success. The guard is on
// the whole listing and never on the album_group-filtered subset, which is the
// one place this table's rules differ from album_tracks'.
func (r *Repo) ReplaceArtistAlbums(
	ctx context.Context, q store.Querier, artistID string, items []ArtistAlbum,
) error {
	cols := artistAlbumRows(items)
	if len(cols.ids) == 0 {
		return fmt.Errorf("replace artist albums: %w: refusing to store an empty discography for %q",
			domain.ErrValidation, artistID)
	}
	if _, err := q.Exec(ctx, replaceArtistAlbumsSQL, artistID,
		cols.ids, cols.names, cols.groups, cols.dates, cols.precisions, cols.positions); err != nil {
		return postgres.Classify("replace artist albums", err)
	}
	return nil
}

// artistAlbumColumns is the transposed form the unnest above expects.
type artistAlbumColumns struct {
	ids        []string
	names      []string
	groups     []string
	dates      []*time.Time
	precisions []string
	positions  []int32
}

// artistAlbumRows transposes a discography into parallel arrays, dropping
// entries with a blank id — a keyless row has nothing for ON CONFLICT to place.
// Every slice is non-nil even when items is empty, so an empty batch reaches
// the statement as an empty array rather than SQL NULL.
func artistAlbumRows(items []ArtistAlbum) artistAlbumColumns {
	out := artistAlbumColumns{
		ids:        make([]string, 0, len(items)),
		names:      make([]string, 0, len(items)),
		groups:     make([]string, 0, len(items)),
		dates:      make([]*time.Time, 0, len(items)),
		precisions: make([]string, 0, len(items)),
		positions:  make([]int32, 0, len(items)),
	}
	for _, it := range items {
		if it.AlbumID == "" {
			continue
		}
		out.ids = append(out.ids, it.AlbumID)
		out.names = append(out.names, it.Name)
		out.groups = append(out.groups, it.Group)
		if it.ReleaseDate != nil {
			d := it.ReleaseDate.UTC()
			out.dates = append(out.dates, &d)
		} else {
			out.dates = append(out.dates, nil)
		}
		out.precisions = append(out.precisions, it.ReleasePrecision)
		out.positions = append(out.positions, int32(it.Position))
	}
	return out
}

// markArtistAlbumsFetchedSQL records a success. It clears last_error so a stale
// message cannot be read beside a listing that is now current, and resets
// attempts to 1: a healthy artist that once failed five times before succeeding
// must not have the next transient error backed off as though it were a sixth.
const markArtistAlbumsFetchedSQL = `
INSERT INTO artist_album_fetches
    (artist_id, status, fetched_at, attempted_at, attempts, last_error)
VALUES ($1, 'ok', $2, $2, 1, '')
ON CONFLICT (artist_id) DO UPDATE SET
    status     = 'ok',
    fetched_at = $2,
    attempts   = 1,
    last_error = ''`

// MarkArtistAlbumsFetched records that the discography now stored is complete.
//
// It is deliberately separate from ReplaceArtistAlbums rather than folded into
// it: the caller runs both inside one Store.InTx, so the rows and the claim
// that they are authoritative commit together or not at all. Splitting the two
// writes across separate transactions would let a crash land between them,
// leaving a discography on disk with no 'ok' beside it, or an 'ok' beside a
// listing an interrupted replace never finished writing.
func (r *Repo) MarkArtistAlbumsFetched(
	ctx context.Context, q store.Querier, artistID string, at time.Time,
) error {
	if _, err := q.Exec(ctx, markArtistAlbumsFetchedSQL, artistID, at.UTC()); err != nil {
		return postgres.Classify("mark artist albums fetched", err)
	}
	return nil
}

// failArtistAlbumFetchSQL records a failed attempt.
//
// It touches neither artist_albums nor fetched_at. Whatever discography is
// stored stays stored and keeps saying when it was read: a timeout today is no
// reason to throw away a listing that was correct last week.
const failArtistAlbumFetchSQL = `
INSERT INTO artist_album_fetches (artist_id, status, attempted_at, attempts, last_error)
VALUES ($1, 'failed', $2, 1, $3)
ON CONFLICT (artist_id) DO UPDATE SET
    status       = 'failed',
    attempted_at = $2,
    last_error   = $3`

// FailArtistAlbumFetch records that the last attempt did not produce a listing.
func (r *Repo) FailArtistAlbumFetch(
	ctx context.Context, q store.Querier, artistID string, at time.Time, reason string,
) error {
	// store.Truncate cuts on a rune boundary. A byte offset could slice a
	// multi-byte rune in half and hand Postgres bytes it rejects outright, which
	// would fail the very write meant to record that the fetch failed — the row
	// stays 'fetching' from the claim, the lease eventually expires, a new claim
	// wins, the same fetch fails the same way, and the write is rejected again:
	// a permanent strand disguised as a retry loop.
	reason = store.Truncate(reason, maxDiscographyErrorLen)
	if _, err := q.Exec(ctx, failArtistAlbumFetchSQL, artistID, at.UTC(), reason); err != nil {
		return postgres.Classify("fail artist album fetch", err)
	}
	return nil
}
