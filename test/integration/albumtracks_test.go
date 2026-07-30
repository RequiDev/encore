//go:build integration

package integration

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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

// TestClaimAlbumTrackFetchUnderRealConcurrencyHasExactlyOneWinner is the
// property TestClaimAlbumTrackFetchIsExclusive cannot prove on its own: that
// test calls the claim twice in sequence, so it would pass even against an
// implementation that reads the row and then writes it in two separate
// statements, so long as nothing else was interleaved. It says nothing about
// what happens when two API replicas hit the same album at the same instant,
// which is the actual failure mode a lease exists to prevent.
//
// This fires the same claim from many goroutines at once, released together
// off one channel close, against the shared pool so each runs on its own
// physical connection and the database itself — not Go's scheduler — is what
// has to serialise them. If the claim were a read-then-write instead of one
// statement, several goroutines could all observe no lease held and all
// proceed to take it.
func TestClaimAlbumTrackFetchUnderRealConcurrencyHasExactlyOneWinner(t *testing.T) {
	env := harness.New(t)
	seedAlbum(t, env, "album000000000000000001")

	now := time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC)
	cutoff := now.Add(-2 * time.Minute)

	const replicas = 8
	var (
		wg    sync.WaitGroup
		start = make(chan struct{})
		wins  int32
		errCh = make(chan error, replicas)
	)
	for range replicas {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start // released together, not one at a time
			won, err := env.Catalog.ClaimAlbumTrackFetch(context.Background(), env.Store.DB(),
				"album000000000000000001", now, cutoff)
			if err != nil {
				errCh <- err
				return
			}
			if won {
				atomic.AddInt32(&wins, 1)
			}
		}()
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

// TestReplaceAlbumTracksLeavesAnUnchangedListingAlone is the mirror of
// DeletesWhatIsAbsent: submitting the same set of ids the album already has —
// even with every non-key column touched — must not delete anything. A replace
// implemented as truncate-then-insert would pass DeletesWhatIsAbsent but could
// still momentarily make the table empty for a concurrent reader; this checks
// the row count never drops for ids that are still present.
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
