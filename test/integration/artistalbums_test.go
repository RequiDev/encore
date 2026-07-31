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

// seedArtist puts one artist in the catalogue so the foreign keys are
// satisfiable. Note what it does *not* do: seed any of the albums the listings
// below refer to. artist_albums deliberately has no foreign key to `albums`,
// because most of a discography is records nobody played.
func seedArtist(t *testing.T, env *harness.Env, id string) {
	t.Helper()
	if _, err := env.Store.DB().Exec(context.Background(),
		`INSERT INTO artists (id, name) VALUES ($1, 'A Test Artist')`, id); err != nil {
		t.Fatalf("seed artist: %v", err)
	}
}

func day(y int, m time.Month, d int) *time.Time {
	at := time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
	return &at
}

// TestArtistAlbumsStoresReleasesForAlbumsNobodyHasPlayed is the property that
// separates this table from album_tracks: its album_id column has no foreign
// key, and if one were added this test fails immediately, because not one of
// these three ids is in `albums`.
func TestArtistAlbumsStoresReleasesForAlbumsNobodyHasPlayed(t *testing.T) {
	env := harness.New(t)
	ctx := context.Background()
	seedArtist(t, env, "artist00000000000000001")

	in := []catalog.ArtistAlbum{
		{AlbumID: "album00000000000000001", Name: "First", Group: catalog.AlbumGroupAlbum,
			ReleaseDate: day(2016, time.May, 20), ReleasePrecision: "day", Position: 0},
		{AlbumID: "album00000000000000002", Name: "A Single", Group: catalog.AlbumGroupSingle,
			ReleaseDate: day(2018, time.March, 1), ReleasePrecision: "day", Position: 1},
		{AlbumID: "album00000000000000003", Name: "No Date", Group: catalog.AlbumGroupAlbum,
			ReleaseDate: nil, ReleasePrecision: "", Position: 2},
	}
	if err := env.Catalog.ReplaceArtistAlbums(ctx, env.Store.DB(), "artist00000000000000001", in); err != nil {
		t.Fatalf("ReplaceArtistAlbums: %v", err)
	}

	got, err := env.Catalog.ArtistAlbums(ctx, env.Store.DB(), "artist00000000000000001")
	if err != nil {
		t.Fatalf("ArtistAlbums: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d rows, want 3", len(got))
	}
	// Newest first, undated last, so a "never played" list reads as a
	// discography does.
	want := []string{"A Single", "First", "No Date"}
	for i, name := range want {
		if got[i].Name != name {
			t.Fatalf("row %d is %q, want %q — the listing is not newest-first with undated last",
				i, got[i].Name, name)
		}
	}
	if got[0].Group != catalog.AlbumGroupSingle {
		t.Fatalf("group = %q, want %q: album_group did not round-trip", got[0].Group, catalog.AlbumGroupSingle)
	}
	if got[2].ReleaseDate != nil {
		t.Fatalf("undated release came back with %v, want nil", *got[2].ReleaseDate)
	}
}

// TestReplaceArtistAlbumsRefusesAnEmptyListing is the critical property this
// file exists to enforce: album_id <> ALL('{}') is vacuously true, so an empty
// (or all-blank-id) items would otherwise delete every row the artist has.
// Spotify's own client returns exactly that shape — a 200 with an empty items
// array and no error — for a market where an artist is invisible. "This artist
// has released nothing" is not a state migrations/00014_artist_albums.sql
// allows: it must be recorded as a failed attempt, never as a successful, empty
// one.
func TestReplaceArtistAlbumsRefusesAnEmptyListing(t *testing.T) {
	env := harness.New(t)
	ctx := context.Background()
	seedArtist(t, env, "artist00000000000000001")

	seeded := []catalog.ArtistAlbum{
		{AlbumID: "album00000000000000001", Name: "First", Group: catalog.AlbumGroupAlbum, Position: 0},
		{AlbumID: "album00000000000000002", Name: "Second", Group: catalog.AlbumGroupAlbum, Position: 1},
	}
	if err := env.Catalog.ReplaceArtistAlbums(ctx, env.Store.DB(), "artist00000000000000001", seeded); err != nil {
		t.Fatalf("seed replace: %v", err)
	}

	if err := env.Catalog.ReplaceArtistAlbums(ctx, env.Store.DB(), "artist00000000000000001", nil); err == nil {
		t.Fatal("ReplaceArtistAlbums(nil) succeeded; want it refused before the good listing could be wiped")
	} else if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("ReplaceArtistAlbums(nil) error = %v, want domain.ErrValidation", err)
	}

	allBlank := []catalog.ArtistAlbum{{AlbumID: "", Name: "ghost", Group: catalog.AlbumGroupAlbum}}
	if err := env.Catalog.ReplaceArtistAlbums(ctx, env.Store.DB(), "artist00000000000000001", allBlank); err == nil {
		t.Fatal("ReplaceArtistAlbums(all blank ids) succeeded; want it refused")
	} else if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("ReplaceArtistAlbums(all blank ids) error = %v, want domain.ErrValidation", err)
	}

	got, err := env.Catalog.ArtistAlbums(ctx, env.Store.DB(), "artist00000000000000001")
	if err != nil {
		t.Fatalf("ArtistAlbums: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d rows after the refused replaces, want the original 2 untouched", len(got))
	}
}

// TestReplaceArtistAlbumsIsAllSinglesAndStillASuccess is the property with no
// counterpart in 2e-i. An album with no tracks is impossible; an artist whose
// every release is a single is ordinary. The repository must store it happily,
// because the emptiness guard is on the whole listing and never on the filtered
// subset.
func TestReplaceArtistAlbumsIsAllSinglesAndStillASuccess(t *testing.T) {
	env := harness.New(t)
	ctx := context.Background()
	seedArtist(t, env, "artist00000000000000001")

	in := []catalog.ArtistAlbum{
		{AlbumID: "album00000000000000001", Name: "One", Group: catalog.AlbumGroupSingle, Position: 0},
		{AlbumID: "album00000000000000002", Name: "Two", Group: catalog.AlbumGroupSingle, Position: 1},
	}
	if err := env.Catalog.ReplaceArtistAlbums(ctx, env.Store.DB(), "artist00000000000000001", in); err != nil {
		t.Fatalf("ReplaceArtistAlbums with no album-group release: %v", err)
	}
	got, err := env.Catalog.ArtistAlbums(ctx, env.Store.DB(), "artist00000000000000001")
	if err != nil {
		t.Fatalf("ArtistAlbums: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d rows, want 2: an all-singles discography was refused or dropped", len(got))
	}
}

// TestReplaceArtistAlbumsDeletesWhatIsAbsent uses a genuinely different second
// input — one release withdrawn, one reclassified, one renamed — rather than
// re-submitting the same set, which would exercise nothing.
func TestReplaceArtistAlbumsDeletesWhatIsAbsent(t *testing.T) {
	env := harness.New(t)
	ctx := context.Background()
	seedArtist(t, env, "artist00000000000000001")

	before := []catalog.ArtistAlbum{
		{AlbumID: "album00000000000000001", Name: "First", Group: catalog.AlbumGroupAlbum,
			ReleaseDate: day(2016, time.May, 20), ReleasePrecision: "day", Position: 0},
		{AlbumID: "album00000000000000002", Name: "Second", Group: catalog.AlbumGroupAlbum,
			ReleaseDate: day(2018, time.May, 20), ReleasePrecision: "day", Position: 1},
		{AlbumID: "album00000000000000003", Name: "Third", Group: catalog.AlbumGroupAlbum,
			ReleaseDate: day(2020, time.May, 20), ReleasePrecision: "day", Position: 2},
	}
	if err := env.Catalog.ReplaceArtistAlbums(ctx, env.Store.DB(), "artist00000000000000001", before); err != nil {
		t.Fatalf("first replace: %v", err)
	}

	after := []catalog.ArtistAlbum{
		{AlbumID: "album00000000000000001", Name: "First (Remastered)", Group: catalog.AlbumGroupAlbum,
			ReleaseDate: day(2021, time.January, 1), ReleasePrecision: "day", Position: 0},
		// Reclassified from album to compilation by Spotify.
		{AlbumID: "album00000000000000003", Name: "Third", Group: catalog.AlbumGroupCompilation,
			ReleaseDate: day(2020, time.May, 20), ReleasePrecision: "day", Position: 1},
	}
	if err := env.Catalog.ReplaceArtistAlbums(ctx, env.Store.DB(), "artist00000000000000001", after); err != nil {
		t.Fatalf("second replace: %v", err)
	}

	got, err := env.Catalog.ArtistAlbums(ctx, env.Store.DB(), "artist00000000000000001")
	if err != nil {
		t.Fatalf("ArtistAlbums: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d rows, want 2: the release absent from the second listing was not deleted", len(got))
	}
	if got[0].Name != "First (Remastered)" || got[0].ReleaseDate == nil || got[0].ReleaseDate.Year() != 2021 {
		t.Fatalf("first row = %+v, want the renamed and redated one: ON CONFLICT did not refresh every column", got[0])
	}
	if got[1].Group != catalog.AlbumGroupCompilation {
		t.Fatalf("second row group = %q, want %q: ON CONFLICT did not refresh album_group, so a "+
			"reclassified release would keep counting towards completion",
			got[1].Group, catalog.AlbumGroupCompilation)
	}
}

func TestClaimArtistAlbumFetchIsExclusive(t *testing.T) {
	env := harness.New(t)
	ctx := context.Background()
	seedArtist(t, env, "artist00000000000000001")

	now := time.Date(2026, time.July, 31, 12, 0, 0, 0, time.UTC)
	cutoff := now.Add(-3 * time.Minute)

	first, err := env.Catalog.ClaimArtistAlbumFetch(ctx, env.Store.DB(), "artist00000000000000001", now, cutoff)
	if err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if !first {
		t.Fatal("the first claim lost; nothing was holding the lease")
	}

	second, err := env.Catalog.ClaimArtistAlbumFetch(ctx, env.Store.DB(), "artist00000000000000001",
		now.Add(time.Second), cutoff.Add(time.Second))
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	if second {
		t.Fatal("the second claim won as well; two tabs would each spend a discography walk")
	}
}

// TestClaimArtistAlbumFetchUnderRealConcurrencyHasExactlyOneWinner corroborates
// (it does not by itself prove — see below) the property
// TestClaimArtistAlbumFetchIsExclusive cannot: that test calls the claim twice
// in sequence, so it would pass even against an implementation that reads the
// row and then writes it in two separate statements, so long as nothing else
// was interleaved. It says nothing about what happens when two API replicas hit
// the same artist at the same instant, which is the actual failure mode a lease
// exists to prevent.
//
// Every goroutine acquires its own physical connection from the pool *before*
// the barrier, so close(start) releases them onto connections that are already
// established rather than racing each other through a fresh TCP handshake, TLS
// and startup — a claim takes microseconds, a cold connection takes
// milliseconds, and without pre-acquiring, the first goroutine to finish
// connecting can claim, commit and release well before the last one has even
// reached the server. Pre-acquiring closes that gap so the thing actually
// racing at the barrier is the claim statement itself.
//
// This is corroboration rather than proof because a database call still
// crosses a real network and scheduler, and no fixed number of runs rules out
// an interleaving that eight goroutines merely failed to hit this time. The
// property is guaranteed by construction: claimArtistAlbumFetchSQL is one
// statement, and Postgres resolves a conflicting INSERT ... ON CONFLICT by
// locking the existing row and making every other transaction targeting the
// same key wait for it, so two concurrent callers are serialised by the
// database itself. That is what actually proves exactly one winner; this test
// exercises it under real, if not exhaustively adversarial, concurrency.
func TestClaimArtistAlbumFetchUnderRealConcurrencyHasExactlyOneWinner(t *testing.T) {
	env := harness.New(t)
	seedArtist(t, env, "artist00000000000000001")

	now := time.Date(2026, time.July, 31, 12, 0, 0, 0, time.UTC)
	cutoff := now.Add(-3 * time.Minute)

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
			won, err := env.Catalog.ClaimArtistAlbumFetch(context.Background(), conn,
				"artist00000000000000001", now, cutoff)
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
			"each winner spends a discography walk the lease exists to prevent", wins, replicas)
	}
}

// TestClaimArtistAlbumFetchDoesNotReclaimExactlyAtTheCutoff pins the boundary
// an "expired by miles" test cannot: the comparison must be a strict <, because
// attempted_at equal to the cutoff is a lease that has only just reached the
// end of its window, and reclaiming it early lets a second replica take over a
// walk the first one might still finish.
func TestClaimArtistAlbumFetchDoesNotReclaimExactlyAtTheCutoff(t *testing.T) {
	env := harness.New(t)
	ctx := context.Background()
	seedArtist(t, env, "artist00000000000000001")

	start := time.Date(2026, time.July, 31, 12, 0, 0, 0, time.UTC)
	if _, err := env.Catalog.ClaimArtistAlbumFetch(ctx, env.Store.DB(), "artist00000000000000001",
		start, start.Add(-3*time.Minute)); err != nil {
		t.Fatalf("first claim: %v", err)
	}

	stillLive, err := env.Catalog.ClaimArtistAlbumFetch(ctx, env.Store.DB(), "artist00000000000000001",
		start.Add(time.Minute), start)
	if err != nil {
		t.Fatalf("claim at the exact cutoff: %v", err)
	}
	if stillLive {
		t.Fatal("a lease exactly at the cutoff was reclaimed; the comparison is <=, not the required strict <")
	}

	// A microsecond, not a nanosecond: timestamptz stores microsecond precision,
	// so a nanosecond offset would round-trip to the same stored instant and
	// prove nothing.
	expired, err := env.Catalog.ClaimArtistAlbumFetch(ctx, env.Store.DB(), "artist00000000000000001",
		start.Add(time.Minute), start.Add(time.Microsecond))
	if err != nil {
		t.Fatalf("claim one microsecond past the cutoff: %v", err)
	}
	if !expired {
		t.Fatal("a lease one microsecond past its cutoff was not reclaimed")
	}
}

func TestFailArtistAlbumFetchKeepsTheOlderListing(t *testing.T) {
	env := harness.New(t)
	ctx := context.Background()
	seedArtist(t, env, "artist00000000000000001")

	at := time.Date(2026, time.July, 31, 12, 0, 0, 0, time.UTC)
	if err := env.Catalog.ReplaceArtistAlbums(ctx, env.Store.DB(), "artist00000000000000001",
		[]catalog.ArtistAlbum{{AlbumID: "album00000000000000001", Name: "First",
			Group: catalog.AlbumGroupAlbum, Position: 0}},
	); err != nil {
		t.Fatalf("ReplaceArtistAlbums: %v", err)
	}
	if err := env.Catalog.MarkArtistAlbumsFetched(ctx, env.Store.DB(), "artist00000000000000001", at); err != nil {
		t.Fatalf("MarkArtistAlbumsFetched: %v", err)
	}

	later := at.Add(8 * 24 * time.Hour)
	if err := env.Catalog.FailArtistAlbumFetch(ctx, env.Store.DB(), "artist00000000000000001",
		later, "spotify: artist albums: context deadline exceeded"); err != nil {
		t.Fatalf("FailArtistAlbumFetch: %v", err)
	}

	rows, err := env.Catalog.ArtistAlbums(ctx, env.Store.DB(), "artist00000000000000001")
	if err != nil {
		t.Fatalf("ArtistAlbums: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows after a failure, want the 1 that was already stored", len(rows))
	}

	st, err := env.Catalog.ArtistAlbumState(ctx, env.Store.DB(), "artist00000000000000001")
	if err != nil {
		t.Fatalf("ArtistAlbumState: %v", err)
	}
	if st.Status != catalog.ArtistAlbumFailed {
		t.Fatalf("status = %q, want %q", st.Status, catalog.ArtistAlbumFailed)
	}
	if !st.FetchedAt.Equal(at) {
		t.Fatalf("fetchedAt = %v, want it untouched at %v: a failure erased when the good listing was read",
			st.FetchedAt, at)
	}
}

// TestFailArtistAlbumFetchTruncatesOnARuneBoundary pins that a long failure
// reason is cut on a rune boundary. A byte cut through a multi-byte rune
// produces invalid UTF-8, Postgres rejects the write outright, and the row
// stays at whatever status the claim left it in — a permanent strand disguised
// as a retry loop.
func TestFailArtistAlbumFetchTruncatesOnARuneBoundary(t *testing.T) {
	env := harness.New(t)
	ctx := context.Background()
	seedArtist(t, env, "artist00000000000000001")

	// 499 ASCII bytes, then a 2-byte rune whose second byte lands exactly at
	// offset 500 — the one byte a naive s[:500] would cut on.
	reason := strings.Repeat("x", 499) + "é" + strings.Repeat("y", 50)
	if err := env.Catalog.FailArtistAlbumFetch(ctx, env.Store.DB(), "artist00000000000000001",
		time.Date(2026, time.July, 31, 12, 0, 0, 0, time.UTC), reason); err != nil {
		t.Fatalf("FailArtistAlbumFetch: %v", err)
	}

	var stored string
	if err := env.Store.DB().QueryRow(ctx,
		`SELECT last_error FROM artist_album_fetches WHERE artist_id = $1`,
		"artist00000000000000001").Scan(&stored); err != nil {
		t.Fatalf("read last_error: %v", err)
	}
	if !utf8.ValidString(stored) {
		t.Fatalf("stored last_error is not valid UTF-8: %q", stored)
	}
}

// TestMarkArtistAlbumsFetchedResetsAttempts pins the backoff invariant: after a
// run of failures followed by one success, the count reads back as a fresh
// success rather than as one more failed attempt.
func TestMarkArtistAlbumsFetchedResetsAttempts(t *testing.T) {
	env := harness.New(t)
	ctx := context.Background()
	seedArtist(t, env, "artist00000000000000001")

	start := time.Date(2026, time.July, 31, 12, 0, 0, 0, time.UTC)
	for i := range 3 {
		at := start.Add(time.Duration(i) * time.Minute)
		if _, err := env.Catalog.ClaimArtistAlbumFetch(ctx, env.Store.DB(),
			"artist00000000000000001", at, at); err != nil {
			t.Fatalf("claim %d: %v", i+1, err)
		}
	}
	if st, err := env.Catalog.ArtistAlbumState(ctx, env.Store.DB(), "artist00000000000000001"); err != nil {
		t.Fatalf("ArtistAlbumState before success: %v", err)
	} else if st.Attempts != 3 {
		t.Fatalf("attempts before success = %d, want 3 (setup did not reach the state this test needs)", st.Attempts)
	}

	if err := env.Catalog.ReplaceArtistAlbums(ctx, env.Store.DB(), "artist00000000000000001",
		[]catalog.ArtistAlbum{{AlbumID: "album00000000000000001", Name: "First",
			Group: catalog.AlbumGroupAlbum, Position: 0}},
	); err != nil {
		t.Fatalf("ReplaceArtistAlbums: %v", err)
	}
	if err := env.Catalog.MarkArtistAlbumsFetched(ctx, env.Store.DB(), "artist00000000000000001",
		start.Add(10*time.Minute)); err != nil {
		t.Fatalf("MarkArtistAlbumsFetched: %v", err)
	}

	st, err := env.Catalog.ArtistAlbumState(ctx, env.Store.DB(), "artist00000000000000001")
	if err != nil {
		t.Fatalf("ArtistAlbumState after success: %v", err)
	}
	if st.Attempts != 1 {
		t.Fatalf("attempts after success = %d, want 1: a healthy artist must not carry a stale "+
			"failure count into the next backoff calculation", st.Attempts)
	}
}

func TestArtistAlbumStateIsEmptyBeforeAnyAttempt(t *testing.T) {
	env := harness.New(t)
	seedArtist(t, env, "artist00000000000000001")

	st, err := env.Catalog.ArtistAlbumState(context.Background(), env.Store.DB(), "artist00000000000000001")
	if err != nil {
		t.Fatalf("ArtistAlbumState: %v", err)
	}
	if st.Status != "" {
		t.Fatalf("status = %q, want the empty string for an artist never attempted", st.Status)
	}
	if !st.FetchedAt.IsZero() {
		t.Fatalf("fetchedAt = %v, want the zero time", st.FetchedAt)
	}
}

func TestArtistAlbumsCascadeWithTheArtist(t *testing.T) {
	env := harness.New(t)
	ctx := context.Background()
	seedArtist(t, env, "artist00000000000000001")
	if err := env.Catalog.ReplaceArtistAlbums(ctx, env.Store.DB(), "artist00000000000000001",
		[]catalog.ArtistAlbum{{AlbumID: "album00000000000000001", Name: "First",
			Group: catalog.AlbumGroupAlbum, Position: 0}},
	); err != nil {
		t.Fatalf("ReplaceArtistAlbums: %v", err)
	}
	if _, err := env.Store.DB().Exec(ctx,
		`DELETE FROM artists WHERE id = $1`, "artist00000000000000001"); err != nil {
		t.Fatalf("delete artist: %v", err)
	}

	var n int
	if err := env.Store.DB().QueryRow(ctx,
		`SELECT count(*)::int FROM artist_albums WHERE artist_id = $1`,
		"artist00000000000000001").Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("%d artist_albums rows survived their artist, want 0", n)
	}
}
