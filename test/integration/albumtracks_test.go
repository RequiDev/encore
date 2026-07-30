//go:build integration

package integration

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/RequiDev/encore/internal/domain"
	"github.com/RequiDev/encore/internal/store/catalog"
	"github.com/RequiDev/encore/test/harness"
)

// seedAlbum puts one album in the catalogue so the foreign keys are satisfiable.
func seedAlbum(t *testing.T, env *harness.Env, id string) {
	t.Helper()
	ctx := context.Background()
	if _, err := env.Store.DB().Exec(ctx,
		`INSERT INTO albums (id, name, total_tracks) VALUES ($1, 'A Test Record', 12)`, id); err != nil {
		t.Fatalf("seed album: %v", err)
	}
}

func TestAlbumTrackStateIsEmptyBeforeAnyAttempt(t *testing.T) {
	env := harness.New(t)
	seedAlbum(t, env, "album000000000000000001")

	st, err := env.Catalog.AlbumTrackState(context.Background(), env.Store.DB(), "album000000000000000001")
	if err != nil {
		t.Fatalf("AlbumTrackState: %v", err)
	}
	if st.Status != "" {
		t.Fatalf("status = %q, want the empty string for an album never attempted", st.Status)
	}
	if !st.FetchedAt.IsZero() {
		t.Fatalf("fetchedAt = %v, want the zero time", st.FetchedAt)
	}
}

func TestClaimAlbumTrackFetchIsExclusive(t *testing.T) {
	env := harness.New(t)
	ctx := context.Background()
	seedAlbum(t, env, "album000000000000000001")

	now := time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC)
	cutoff := now.Add(-2 * time.Minute)

	first, err := env.Catalog.ClaimAlbumTrackFetch(ctx, env.Store.DB(), "album000000000000000001", now, cutoff)
	if err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if !first {
		t.Fatal("the first claim lost; nothing was holding the lease")
	}

	// The second tab, a second later.
	second, err := env.Catalog.ClaimAlbumTrackFetch(ctx, env.Store.DB(), "album000000000000000001",
		now.Add(time.Second), cutoff.Add(time.Second))
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	if second {
		t.Fatal("the second claim won as well; two tabs would each spend a Spotify request")
	}
}

// TestClaimAlbumTrackFetchUnderRealConcurrencyHasExactlyOneWinner corroborates
// (it does not by itself prove — see below) the property
// TestClaimAlbumTrackFetchIsExclusive cannot: that test calls the claim twice
// in sequence, so it would pass even against an implementation that reads the
// row and then writes it in two separate statements, so long as nothing else
// was interleaved. It says nothing about what happens when two API replicas
// hit the same album at the same instant, which is the actual failure mode a
// lease exists to prevent.
//
// Every goroutine acquires its own physical connection from the pool *before*
// the barrier, so close(start) releases them onto connections that are
// already established rather than racing each other through a fresh TCP
// handshake, TLS and startup — a claim takes microseconds, a cold connection
// takes milliseconds, and without pre-acquiring, the first goroutine to
// finish connecting can claim, commit and release well before the last one
// has even reached the server. Pre-acquiring closes that gap so the thing
// actually racing at the barrier is the claim statement itself.
//
// This is corroboration rather than proof because a database call still
// crosses a real network and scheduler, and no fixed number of runs rules out
// an interleaving that eight goroutines merely failed to hit this time. The
// property is guaranteed by construction: claimAlbumTrackFetchSQL is one
// statement, and Postgres resolves a conflicting INSERT ... ON CONFLICT by
// locking the existing row and making every other transaction targeting the
// same key wait for it, so two concurrent callers are serialised by the
// database itself. That is what actually proves exactly one winner; this test
// exercises it under real, if not exhaustively adversarial, concurrency.
func TestClaimAlbumTrackFetchUnderRealConcurrencyHasExactlyOneWinner(t *testing.T) {
	env := harness.New(t)
	seedAlbum(t, env, "album000000000000000001")

	now := time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC)
	cutoff := now.Add(-2 * time.Minute)

	const replicas = 8
	conns := make([]*pgxpool.Conn, replicas)
	for i := range conns {
		c, err := env.Pool.Acquire(env.Ctx())
		if err != nil {
			t.Fatalf("acquire connection %d: %v", i, err)
		}
		conns[i] = c
		t.Cleanup(c.Release)
	}

	var (
		wg    sync.WaitGroup
		start = make(chan struct{})
		wins  int32
		errCh = make(chan error, replicas)
	)
	for i := range replicas {
		wg.Add(1)
		go func(conn *pgxpool.Conn) {
			defer wg.Done()
			<-start // released together, onto connections already established
			won, err := env.Catalog.ClaimAlbumTrackFetch(context.Background(), conn,
				"album000000000000000001", now, cutoff)
			if err != nil {
				errCh <- err
				return
			}
			if won {
				atomic.AddInt32(&wins, 1)
			}
		}(conns[i])
	}
	close(start)
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatalf("concurrent claim: %v", err)
	}
	if wins != 1 {
		t.Fatalf("%d of %d concurrent replicas won the claim, want exactly 1: "+
			"each winner spends a Spotify request the lease exists to prevent", wins, replicas)
	}
}

func TestClaimAlbumTrackFetchReclaimsAnExpiredLease(t *testing.T) {
	env := harness.New(t)
	ctx := context.Background()
	seedAlbum(t, env, "album000000000000000001")

	start := time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC)
	if _, err := env.Catalog.ClaimAlbumTrackFetch(ctx, env.Store.DB(), "album000000000000000001",
		start, start.Add(-2*time.Minute)); err != nil {
		t.Fatalf("first claim: %v", err)
	}

	// Ten minutes later the process that held it is long dead.
	later := start.Add(10 * time.Minute)
	got, err := env.Catalog.ClaimAlbumTrackFetch(ctx, env.Store.DB(), "album000000000000000001",
		later, later.Add(-2*time.Minute))
	if err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	if !got {
		t.Fatal("an expired lease was not reclaimed; the album is stranded in 'fetching' for ever")
	}

	st, err := env.Catalog.AlbumTrackState(ctx, env.Store.DB(), "album000000000000000001")
	if err != nil {
		t.Fatalf("AlbumTrackState: %v", err)
	}
	if st.Attempts != 2 {
		t.Fatalf("attempts = %d, want 2", st.Attempts)
	}
}

// TestClaimAlbumTrackFetchDoesNotReclaimExactlyAtTheCutoff pins the boundary
// TestClaimAlbumTrackFetchReclaimsAnExpiredLease leaves untested: that test's
// lease is ten minutes stale against a two-minute cutoff, five times clear of
// the edge, so a claim written with attempted_at <= $3 instead of the correct
// strict < would still pass it. The comparison must be strict: attempted_at
// equal to the cutoff is a lease that has not yet expired, only just reached
// the end of its window, and reclaiming it early would let a second replica
// take over a fetch the first one might still finish.
func TestClaimAlbumTrackFetchDoesNotReclaimExactlyAtTheCutoff(t *testing.T) {
	env := harness.New(t)
	ctx := context.Background()
	seedAlbum(t, env, "album000000000000000001")

	start := time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC)
	if _, err := env.Catalog.ClaimAlbumTrackFetch(ctx, env.Store.DB(), "album000000000000000001",
		start, start.Add(-2*time.Minute)); err != nil {
		t.Fatalf("first claim: %v", err)
	}

	// A cutoff exactly equal to attempted_at: the lease has not outlived it.
	stillLive, err := env.Catalog.ClaimAlbumTrackFetch(ctx, env.Store.DB(), "album000000000000000001",
		start.Add(time.Minute), start)
	if err != nil {
		t.Fatalf("claim at the exact cutoff: %v", err)
	}
	if stillLive {
		t.Fatal("a lease exactly at the cutoff was reclaimed; the comparison is <=, not the required strict <")
	}

	// One microsecond later, the same lease has outlived a cutoff of exactly
	// attempted_at, and must be reclaimable. A microsecond, not a nanosecond:
	// timestamptz stores microsecond precision, so a nanosecond offset would
	// round-trip to the same stored instant as attempted_at and prove nothing.
	expired, err := env.Catalog.ClaimAlbumTrackFetch(ctx, env.Store.DB(), "album000000000000000001",
		start.Add(time.Minute), start.Add(time.Microsecond))
	if err != nil {
		t.Fatalf("claim one microsecond past the cutoff: %v", err)
	}
	if !expired {
		t.Fatal("a lease one microsecond past its cutoff was not reclaimed")
	}
}

func TestReplaceAlbumTracksDeletesWhatIsAbsent(t *testing.T) {
	env := harness.New(t)
	ctx := context.Background()
	seedAlbum(t, env, "album000000000000000001")

	before := []catalog.AlbumTrack{
		{TrackID: "track00000000000000001", Name: "One", DiscNumber: 1, TrackNumber: 1},
		{TrackID: "track00000000000000002", Name: "Two", DiscNumber: 1, TrackNumber: 2},
		{TrackID: "track00000000000000003", Name: "Three", DiscNumber: 1, TrackNumber: 3},
	}
	if err := env.Catalog.ReplaceAlbumTracks(ctx, env.Store.DB(), "album000000000000000001", before); err != nil {
		t.Fatalf("first replace: %v", err)
	}

	// The re-issue dropped one track and renamed another. Deliberately not the
	// same input twice: re-submitting an identical set would exercise nothing.
	after := []catalog.AlbumTrack{
		{TrackID: "track00000000000000001", Name: "One (Remastered)", DiscNumber: 1, TrackNumber: 1},
		{TrackID: "track00000000000000003", Name: "Three", DiscNumber: 1, TrackNumber: 2},
	}
	if err := env.Catalog.ReplaceAlbumTracks(ctx, env.Store.DB(), "album000000000000000001", after); err != nil {
		t.Fatalf("second replace: %v", err)
	}

	got, err := env.Catalog.AlbumTracks(ctx, env.Store.DB(), "album000000000000000001")
	if err != nil {
		t.Fatalf("AlbumTracks: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d rows, want 2: the track absent from the second listing was not deleted", len(got))
	}
	if got[0].Name != "One (Remastered)" {
		t.Fatalf("first row name = %q, want the renamed one: ON CONFLICT did not refresh it", got[0].Name)
	}
	if got[1].TrackID != "track00000000000000003" || got[1].TrackNumber != 2 {
		t.Fatalf("second row = %+v, want track 3 renumbered to 2", got[1])
	}
}

// TestReplaceAlbumTracksRefusesAnEmptyListing is the critical property this
// file exists to enforce: track_id <> ALL('{}') is vacuously true, so an
// empty (or all-blank-id) items would otherwise delete every row the album
// has. Spotify's own client returns exactly that shape — a 200 with an empty
// items array and no error — for a market where an album has been withdrawn,
// and again whenever every item on a page happens to carry a blank id. Either
// way, "this album has no tracks" is not a state
// migrations/00013_album_tracks.sql allows: it must be recorded as a failed
// attempt, never as a successful, empty one, so the repository refuses to
// store it rather than trusting every caller to check first.
func TestReplaceAlbumTracksRefusesAnEmptyListing(t *testing.T) {
	env := harness.New(t)
	ctx := context.Background()
	seedAlbum(t, env, "album000000000000000001")

	seeded := []catalog.AlbumTrack{
		{TrackID: "track00000000000000001", Name: "One", DiscNumber: 1, TrackNumber: 1},
		{TrackID: "track00000000000000002", Name: "Two", DiscNumber: 1, TrackNumber: 2},
	}
	if err := env.Catalog.ReplaceAlbumTracks(ctx, env.Store.DB(), "album000000000000000001", seeded); err != nil {
		t.Fatalf("seed replace: %v", err)
	}

	if err := env.Catalog.ReplaceAlbumTracks(ctx, env.Store.DB(), "album000000000000000001", nil); err == nil {
		t.Fatal("ReplaceAlbumTracks(nil) succeeded; want it refused before the good listing could be wiped")
	} else if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("ReplaceAlbumTracks(nil) error = %v, want domain.ErrValidation", err)
	}

	// A listing that becomes empty only after blank ids are dropped must be
	// refused the same way: albumTrackRows filters them before the count that
	// matters is taken.
	allBlank := []catalog.AlbumTrack{{TrackID: "", Name: "ghost", DiscNumber: 1, TrackNumber: 1}}
	if err := env.Catalog.ReplaceAlbumTracks(ctx, env.Store.DB(), "album000000000000000001", allBlank); err == nil {
		t.Fatal("ReplaceAlbumTracks(all blank ids) succeeded; want it refused")
	} else if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("ReplaceAlbumTracks(all blank ids) error = %v, want domain.ErrValidation", err)
	}

	got, err := env.Catalog.AlbumTracks(ctx, env.Store.DB(), "album000000000000000001")
	if err != nil {
		t.Fatalf("AlbumTracks: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d rows after the refused replaces, want the original 2 untouched", len(got))
	}
}

// TestReplaceAlbumTracksLeavesAnUnchangedListingAlone is a regression guard,
// not proof of atomicity: it checks that re-submitting the same id set with
// every non-key column touched still leaves both rows present and refreshed.
// It cannot distinguish this from a truncate-then-insert implementation,
// because album_tracks carries no timestamp or version column, so the final
// state after either implementation is byte-identical — there is nothing left
// on disk for an assertion made after the fact to observe. Making the replace
// two separate statements instead of one and re-running this test still
// passes it, which is exactly why it doesn't belong here: the property that a
// concurrent reader can never observe the table momentarily emptied is pinned
// by TestReplaceAlbumTracksSQLIsOneStatement in the catalog package's own unit
// tests, which asserts the shape of replaceAlbumTracksSQL directly rather than
// inferring it from outcomes indistinguishable from a broken implementation's.
func TestReplaceAlbumTracksLeavesAnUnchangedListingAlone(t *testing.T) {
	env := harness.New(t)
	ctx := context.Background()
	seedAlbum(t, env, "album000000000000000001")

	items := []catalog.AlbumTrack{
		{TrackID: "track00000000000000001", Name: "One", DiscNumber: 1, TrackNumber: 1},
		{TrackID: "track00000000000000002", Name: "Two", DiscNumber: 1, TrackNumber: 2},
	}
	if err := env.Catalog.ReplaceAlbumTracks(ctx, env.Store.DB(), "album000000000000000001", items); err != nil {
		t.Fatalf("first replace: %v", err)
	}

	// Same ids, same disc/track numbers, only the names retouched — the shape a
	// re-fetch of an unchanged listing actually takes.
	same := []catalog.AlbumTrack{
		{TrackID: "track00000000000000001", Name: "One (re-read)", DiscNumber: 1, TrackNumber: 1},
		{TrackID: "track00000000000000002", Name: "Two (re-read)", DiscNumber: 1, TrackNumber: 2},
	}
	if err := env.Catalog.ReplaceAlbumTracks(ctx, env.Store.DB(), "album000000000000000001", same); err != nil {
		t.Fatalf("second replace: %v", err)
	}

	got, err := env.Catalog.AlbumTracks(ctx, env.Store.DB(), "album000000000000000001")
	if err != nil {
		t.Fatalf("AlbumTracks: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d rows, want 2: an unchanged listing lost a row it should have kept", len(got))
	}
	if got[0].Name != "One (re-read)" || got[1].Name != "Two (re-read)" {
		t.Fatalf("rows = %+v, want both names refreshed by the second replace", got)
	}
}

func TestAlbumTracksComeBackInDiscAndTrackOrder(t *testing.T) {
	env := harness.New(t)
	ctx := context.Background()
	seedAlbum(t, env, "album000000000000000001")

	// Inserted deliberately out of order, and with ids whose lexical order is the
	// reverse of the playing order, so an ORDER BY on the key would fail this.
	in := []catalog.AlbumTrack{
		{TrackID: "track00000000000000009", Name: "Disc two, one", DiscNumber: 2, TrackNumber: 1},
		{TrackID: "track00000000000000005", Name: "Disc one, two", DiscNumber: 1, TrackNumber: 2},
		{TrackID: "track00000000000000007", Name: "Disc one, one", DiscNumber: 1, TrackNumber: 1},
	}
	if err := env.Catalog.ReplaceAlbumTracks(ctx, env.Store.DB(), "album000000000000000001", in); err != nil {
		t.Fatalf("ReplaceAlbumTracks: %v", err)
	}

	got, err := env.Catalog.AlbumTracks(ctx, env.Store.DB(), "album000000000000000001")
	if err != nil {
		t.Fatalf("AlbumTracks: %v", err)
	}
	want := []string{"Disc one, one", "Disc one, two", "Disc two, one"}
	for i, name := range want {
		if got[i].Name != name {
			t.Fatalf("row %d is %q, want %q — the listing is not in disc and track order", i, got[i].Name, name)
		}
	}
}

func TestFailAlbumTrackFetchKeepsTheOlderListing(t *testing.T) {
	env := harness.New(t)
	ctx := context.Background()
	seedAlbum(t, env, "album000000000000000001")

	at := time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC)
	if err := env.Catalog.ReplaceAlbumTracks(ctx, env.Store.DB(), "album000000000000000001",
		[]catalog.AlbumTrack{{TrackID: "track00000000000000001", Name: "One", DiscNumber: 1, TrackNumber: 1}},
	); err != nil {
		t.Fatalf("ReplaceAlbumTracks: %v", err)
	}
	if err := env.Catalog.MarkAlbumTracksFetched(ctx, env.Store.DB(), "album000000000000000001", at); err != nil {
		t.Fatalf("MarkAlbumTracksFetched: %v", err)
	}

	later := at.Add(31 * 24 * time.Hour)
	if err := env.Catalog.FailAlbumTrackFetch(ctx, env.Store.DB(), "album000000000000000001",
		later, "spotify: album tracks: context deadline exceeded"); err != nil {
		t.Fatalf("FailAlbumTrackFetch: %v", err)
	}

	rows, err := env.Catalog.AlbumTracks(ctx, env.Store.DB(), "album000000000000000001")
	if err != nil {
		t.Fatalf("AlbumTracks: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows after a failure, want the 1 that was already stored", len(rows))
	}

	st, err := env.Catalog.AlbumTrackState(ctx, env.Store.DB(), "album000000000000000001")
	if err != nil {
		t.Fatalf("AlbumTrackState: %v", err)
	}
	if st.Status != catalog.AlbumTrackFailed {
		t.Fatalf("status = %q, want %q", st.Status, catalog.AlbumTrackFailed)
	}
	if !st.FetchedAt.Equal(at) {
		t.Fatalf("fetchedAt = %v, want it untouched at %v: a failure erased when the good listing was read",
			st.FetchedAt, at)
	}
}

// TestFailAlbumTrackFetchTruncatesOnARuneBoundary pins that a failure reason
// long enough to hit FailAlbumTrackFetch's length cap is truncated on a rune
// boundary rather than a byte offset. A byte cut through the middle of a
// multi-byte rune produces invalid UTF-8, which Postgres rejects outright —
// turning the write that was supposed to record the failure into a second,
// unrecorded failure of its own, leaving the row stuck at whatever status the
// claim left it in.
func TestFailAlbumTrackFetchTruncatesOnARuneBoundary(t *testing.T) {
	env := harness.New(t)
	ctx := context.Background()
	seedAlbum(t, env, "album000000000000000001")

	// 499 ASCII bytes, then a 2-byte rune whose second byte lands exactly at
	// offset 500 — the one byte a naive s[:500] would cut on its own, slicing
	// the rune's lead byte from its continuation byte.
	reason := strings.Repeat("x", 499) + "é" + strings.Repeat("y", 50)
	if err := env.Catalog.FailAlbumTrackFetch(ctx, env.Store.DB(), "album000000000000000001",
		time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC), reason); err != nil {
		t.Fatalf("FailAlbumTrackFetch: %v", err)
	}

	var stored string
	if err := env.Store.DB().QueryRow(ctx,
		`SELECT last_error FROM album_track_fetches WHERE album_id = $1`, "album000000000000000001",
	).Scan(&stored); err != nil {
		t.Fatalf("read last_error: %v", err)
	}
	if !utf8.ValidString(stored) {
		t.Fatalf("stored last_error is not valid UTF-8: %q", stored)
	}
}

// TestMarkAlbumTracksFetchedResetsAttempts pins the retry-backoff invariant
// migrations/00013_album_tracks.sql assigns to attempts: after a run of
// failures followed by one success, the count must read back as a fresh
// success, not as one more failed attempt. A caller building backoff from a
// stale, still-climbing count would treat a healthy album as though it kept
// failing.
func TestMarkAlbumTracksFetchedResetsAttempts(t *testing.T) {
	env := harness.New(t)
	ctx := context.Background()
	seedAlbum(t, env, "album000000000000000001")

	start := time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC)
	if _, err := env.Catalog.ClaimAlbumTrackFetch(ctx, env.Store.DB(), "album000000000000000001",
		start, start.Add(-2*time.Minute)); err != nil {
		t.Fatalf("claim 1: %v", err)
	}
	// Two more reclaims of the same, immediately-expired lease — the shape a
	// run of failures followed by a retry actually takes — bring attempts to 3.
	for i := 1; i <= 2; i++ {
		at := start.Add(time.Duration(i) * time.Minute)
		if _, err := env.Catalog.ClaimAlbumTrackFetch(ctx, env.Store.DB(), "album000000000000000001",
			at, at); err != nil {
			t.Fatalf("claim %d: %v", i+1, err)
		}
	}
	if st, err := env.Catalog.AlbumTrackState(ctx, env.Store.DB(), "album000000000000000001"); err != nil {
		t.Fatalf("AlbumTrackState before success: %v", err)
	} else if st.Attempts != 3 {
		t.Fatalf("attempts before success = %d, want 3 (setup did not reach the state this test needs)", st.Attempts)
	}

	if err := env.Catalog.ReplaceAlbumTracks(ctx, env.Store.DB(), "album000000000000000001",
		[]catalog.AlbumTrack{{TrackID: "track00000000000000001", Name: "One", DiscNumber: 1, TrackNumber: 1}},
	); err != nil {
		t.Fatalf("ReplaceAlbumTracks: %v", err)
	}
	if err := env.Catalog.MarkAlbumTracksFetched(ctx, env.Store.DB(), "album000000000000000001",
		start.Add(10*time.Minute)); err != nil {
		t.Fatalf("MarkAlbumTracksFetched: %v", err)
	}

	st, err := env.Catalog.AlbumTrackState(ctx, env.Store.DB(), "album000000000000000001")
	if err != nil {
		t.Fatalf("AlbumTrackState after success: %v", err)
	}
	if st.Attempts != 1 {
		t.Fatalf("attempts after success = %d, want 1: a healthy album must not carry a stale failure count "+
			"into the next backoff calculation", st.Attempts)
	}
}

func TestAlbumTracksCascadeWithTheAlbum(t *testing.T) {
	env := harness.New(t)
	ctx := context.Background()
	seedAlbum(t, env, "album000000000000000001")
	if err := env.Catalog.ReplaceAlbumTracks(ctx, env.Store.DB(), "album000000000000000001",
		[]catalog.AlbumTrack{{TrackID: "track00000000000000001", Name: "One", DiscNumber: 1, TrackNumber: 1}},
	); err != nil {
		t.Fatalf("ReplaceAlbumTracks: %v", err)
	}
	if _, err := env.Store.DB().Exec(ctx, `DELETE FROM albums WHERE id = $1`, "album000000000000000001"); err != nil {
		t.Fatalf("delete album: %v", err)
	}

	var n int
	if err := env.Store.DB().QueryRow(ctx,
		`SELECT count(*)::int FROM album_tracks WHERE album_id = $1`, "album000000000000000001").Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("%d album_tracks rows survived their album, want 0", n)
	}
	_ = errors.Is(nil, domain.ErrNotFound) // keeps the domain import honest if the file is trimmed
}
