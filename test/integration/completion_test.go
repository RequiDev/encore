//go:build integration

package integration

import (
	"testing"
	"time"

	"github.com/RequiDev/encore/internal/domain"
)

// TestAlbumCompletionCountsDistinctTracks is the core arithmetic. Playing one
// track of an album five times is still one track heard.
func TestAlbumCompletionCountsDistinctTracks(t *testing.T) {
	f := seedStats(t)
	f.env.Exec(`UPDATE albums SET total_tracks = 12 WHERE id = 'alb-1'`)

	got, err := f.svc.AlbumCompletion(f.env.Ctx(), f.env.Store.DB(), f.user.ID, "alb-1")
	if err != nil {
		t.Fatalf("album completion: %v", err)
	}
	if !got.Known {
		t.Fatal("total_tracks is 12, so completion is knowable")
	}
	// trk-a plays four times and trk-b once: two distinct tracks of twelve.
	if got.Heard != 2 || got.Total != 12 {
		t.Errorf("got %d of %d, want 2 of 12", got.Heard, got.Total)
	}
}

// TestAlbumCompletionIsAllTime is the design decision this statistic rests on.
//
// Completion is a property of a listening lifetime, not of whatever range the
// page happens to be showing. A range-scoped completion would tell somebody
// opening an album with a seven-day window that they had heard one of twelve
// tracks, which is false.
func TestAlbumCompletionIsAllTime(t *testing.T) {
	f := seedStats(t)
	f.env.Exec(`UPDATE albums SET total_tracks = 12 WHERE id = 'alb-1'`)

	// AlbumCompletion takes no range at all — this test exists to pin that its
	// answer does not move when the fixture's plays fall outside any window a
	// caller might have been looking at.
	got, err := f.svc.AlbumCompletion(f.env.Ctx(), f.env.Store.DB(), f.user.ID, "alb-1")
	if err != nil {
		t.Fatalf("album completion: %v", err)
	}
	if got.Heard != 2 {
		t.Errorf("heard = %d, want 2 regardless of any range", got.Heard)
	}
}

// TestAlbumCompletionUnknownWhenUnresolved guards the state a freshly imported
// instance is in for almost every album: total_tracks defaults to 0 because
// enrichment has not run. Zero is "we do not know", never "an album with no
// tracks", and it must not render as 0%.
func TestAlbumCompletionUnknownWhenUnresolved(t *testing.T) {
	f := seedStats(t)
	// alb-1's total_tracks is left at its 0 default.

	got, err := f.svc.AlbumCompletion(f.env.Ctx(), f.env.Store.DB(), f.user.ID, "alb-1")
	if err != nil {
		t.Fatalf("album completion: %v", err)
	}
	if got.Known {
		t.Error("total_tracks is 0, so completion cannot be known")
	}
}

// TestAlbumCompletionRespectsTheBlacklist keeps the one rule that applies to
// every statistic in this package.
func TestAlbumCompletionRespectsTheBlacklist(t *testing.T) {
	f := seedStats(t)
	f.env.Exec(`UPDATE albums SET total_tracks = 12 WHERE id = 'alb-1'`)
	// art-x is credited on both trk-a and trk-b, the two tracks of alb-1.
	f.env.Exec(`INSERT INTO user_blacklisted_artists (user_id, artist_id) VALUES ($1, 'art-x')`, f.user.ID)

	got, err := f.svc.AlbumCompletion(f.env.Ctx(), f.env.Store.DB(), f.user.ID, "alb-1")
	if err != nil {
		t.Fatalf("album completion: %v", err)
	}
	if got.Heard != 0 {
		t.Errorf("heard = %d, want 0 — blacklisted listens still counted", got.Heard)
	}
}

// TestCompletedAlbumsIsRangeScopedAndNamesItsDenominator pins the aggregate's
// shape: both numbers describe albums played inside the range, so the sentence
// "of the N you played, you have heard every track on M" is true as written.
func TestCompletedAlbumsIsRangeScoped(t *testing.T) {
	f := seedStats(t)
	// alb-2 holds only trk-c, which is played: complete.
	// alb-3 holds only trk-d, which is played: complete.
	// alb-1 holds trk-a and trk-b, both played, but claims twelve tracks.
	f.env.Exec(`UPDATE albums SET total_tracks = 12 WHERE id = 'alb-1'`)
	f.env.Exec(`UPDATE albums SET total_tracks = 1  WHERE id = 'alb-2'`)
	f.env.Exec(`UPDATE albums SET total_tracks = 1  WHERE id = 'alb-3'`)

	got, err := f.svc.CompletedAlbums(f.env.Ctx(), f.env.Store.DB(), f.user.ID, f.fullRange())
	if err != nil {
		t.Fatalf("completed albums: %v", err)
	}
	if got.Albums != 3 {
		t.Errorf("albums = %d, want 3 played in range", got.Albums)
	}
	if got.Complete != 2 {
		t.Errorf("complete = %d, want 2 (alb-2 and alb-3)", got.Complete)
	}
}

// TestCompletedAlbumsExcludesUnresolvedAlbums keeps an unenriched album from
// counting as an incomplete one and dragging the figure down.
func TestCompletedAlbumsExcludesUnresolvedAlbums(t *testing.T) {
	f := seedStats(t)
	f.env.Exec(`UPDATE albums SET total_tracks = 1 WHERE id = 'alb-2'`)
	// alb-1 and alb-3 keep total_tracks = 0.

	got, err := f.svc.CompletedAlbums(f.env.Ctx(), f.env.Store.DB(), f.user.ID, f.fullRange())
	if err != nil {
		t.Fatalf("completed albums: %v", err)
	}
	if got.Albums != 1 {
		t.Errorf("albums = %d, want 1 — only alb-2 has a known track count", got.Albums)
	}
	if got.Complete != 1 {
		t.Errorf("complete = %d, want 1", got.Complete)
	}
}

// TestCompletedAlbumsEmptyRangeIsNotAnError guards the state a new instance is
// in. Note this is a valid window containing no listens, NOT a zero-width one —
// scope() rejects from == to as a caller error by design.
func TestCompletedAlbumsEmptyRangeIsNotAnError(t *testing.T) {
	f := seedStats(t)
	from := time.Date(2025, time.January, 1, 0, 0, 0, 0, f.loc)
	empty := domain.TimeRange{From: from, To: from.AddDate(0, 0, 10)}

	got, err := f.svc.CompletedAlbums(f.env.Ctx(), f.env.Store.DB(), f.user.ID, empty)
	if err != nil {
		t.Fatalf("completed albums over an empty range: %v", err)
	}
	if got.Albums != 0 || got.Complete != 0 {
		t.Errorf("expected zeroes, got %+v", got)
	}
}
