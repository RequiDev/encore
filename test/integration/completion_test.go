//go:build integration

package integration

import (
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/RequiDev/encore/internal/domain"
	"github.com/RequiDev/encore/internal/stats"
	"github.com/RequiDev/encore/internal/store/listens"
	"github.com/RequiDev/encore/test/harness"
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

// The design decision that completion is a property of a listening lifetime,
// not of whatever range the page happens to be showing, used to have a test
// here named TestAlbumCompletionIsAllTime. It called AlbumCompletion the same
// way TestAlbumCompletionCountsDistinctTracks above does — same fixture, same
// UPDATE, same assertion on got.Heard — because AlbumCompletion takes no range
// argument at all. A test that cannot vary the range cannot show the answer is
// independent of it; it can only ever re-run the same call and get the same
// number, which the neighbouring test already does. That test has been removed
// rather than kept for its name's sake. The property it claimed to pin is now
// actually exercised end-to-end by TestAlbumCompletionIgnoresTheRange in
// test/e2e/e2e_test.go, which fetches GET /api/albums/{id} under two disjoint
// from/to windows and asserts the completion payload is identical — the layer
// where a range is actually in scope and could accidentally get threaded
// through (see the comment on handleAlbum in internal/httpapi/entities.go).

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

// seedAlbumWithPlays inserts one artist (artist00000000000000001), one album
// with total_tracks = total, that many tracks linked to the album and credited
// to the artist, and one 2019 play for the first `played` of them — except the
// very first played track, which gets two plays two days apart. That repeat is
// deliberate: with only ever one listen per track, dropping DISTINCT from
// albumHeardTracksSQL would return exactly as many ids as there are tracks
// heard, and the mutation this fixture exists to catch would pass silently.
//
// It deliberately never writes to album_tracks: that cache belongs to a
// different task (the fetched Spotify listing), and AlbumHeardTracks must give
// the right answer whether or not that cache has ever been populated for this
// album. A helper that populated it "for realism" would quietly stop testing
// that, since a query that joined through album_tracks by mistake could still
// find matching rows there and pass.
func seedAlbumWithPlays(t *testing.T, env *harness.Env, userID uuid.UUID, albumID string, total, played int) {
	t.Helper()
	if played > total {
		t.Fatalf("seedAlbumWithPlays: played (%d) exceeds total (%d)", played, total)
	}
	const artistID = "artist00000000000000001"

	env.Exec(`INSERT INTO artists (id, name, name_norm, metadata_state) VALUES ($1, 'Artist', 'artist', 'resolved')`,
		artistID)
	env.Exec(`INSERT INTO albums (id, name, name_norm, album_type, metadata_state, total_tracks)
	          VALUES ($1, 'Album', 'album', 'album', 'resolved', $2)`, albumID, total)

	stage := func(trackID string, at time.Time) listens.StagedListen {
		return listens.Stage(domain.Listen{
			UserID:    userID,
			PlayedAt:  at,
			Precision: domain.PrecisionSecond,
			Identity:  domain.TrackIdentityFromID(trackID),
			MsPlayed:  200_000,
			Source:    domain.SourceExtended,
		}, nil)
	}

	wantListens := 0
	batch := make([]listens.StagedListen, 0, played+1)
	for i := 0; i < total; i++ {
		trackID := fmt.Sprintf("%s-track-%02d", albumID, i)
		env.Exec(`INSERT INTO tracks (id, name, name_norm, album_id, metadata_state)
		          VALUES ($1, 'Track', 'track', $2, 'resolved')`, trackID, albumID)
		env.Exec(`INSERT INTO track_artists (track_id, artist_id, position) VALUES ($1, $2, 0)`,
			trackID, artistID)

		if i < played {
			at := time.Date(2019, time.January, 1, 12, 0, 0, 0, time.UTC).Add(time.Duration(i) * 24 * time.Hour)
			batch = append(batch, stage(trackID, at))
			wantListens++
			if i == 0 {
				batch = append(batch, stage(trackID, at.Add(48*time.Hour)))
				wantListens++
			}
		}
	}

	n, err := env.Listens.InsertListens(env.Ctx(), env.Store.DB(), batch, "UTC")
	if err != nil {
		t.Fatalf("seed album plays: %v", err)
	}
	if int(n) != wantListens {
		t.Fatalf("seeded %d of %d listens; the fixture has an accidental duplicate", n, wantListens)
	}
}

// TestAlbumHeardTracksMatchesTheCompletionNumerator is the consistency property
// the album page depends on. The count and the set are two readings of the same
// question, and a page that shows "9 of 12 heard" beside four tracks it calls
// unheard is worse than one that shows neither.
func TestAlbumHeardTracksMatchesTheCompletionNumerator(t *testing.T) {
	env := harness.New(t)
	ctx := env.Ctx()
	user := env.NewUser("heardtracksuser")

	// Nine of the album's twelve tracks played, at various times.
	seedAlbumWithPlays(t, env, user.ID, "album000000000000000001", 12, 9)

	svc := stats.New(env.Store)
	completion, err := svc.AlbumCompletion(ctx, env.Store.DB(), user.ID, "album000000000000000001")
	if err != nil {
		t.Fatalf("AlbumCompletion: %v", err)
	}
	heard, err := svc.AlbumHeardTracks(ctx, env.Store.DB(), user.ID, "album000000000000000001")
	if err != nil {
		t.Fatalf("AlbumHeardTracks: %v", err)
	}
	if int64(len(heard)) != completion.Heard {
		t.Fatalf("AlbumHeardTracks returned %d ids but AlbumCompletion counted %d; "+
			"the page would contradict itself", len(heard), completion.Heard)
	}
	if completion.Heard != 9 {
		t.Fatalf("completion.Heard = %d, want 9", completion.Heard)
	}
}

// TestAlbumHeardTracksRespectsTheBlacklist keeps an excluded artist excluded
// here too. Without it the album page would name tracks by an artist the
// listener has told Encore to forget.
func TestAlbumHeardTracksRespectsTheBlacklist(t *testing.T) {
	env := harness.New(t)
	ctx := env.Ctx()
	user := env.NewUser("heardtracksblacklistuser")
	seedAlbumWithPlays(t, env, user.ID, "album000000000000000001", 12, 9)

	svc := stats.New(env.Store)
	before, err := svc.AlbumHeardTracks(ctx, env.Store.DB(), user.ID, "album000000000000000001")
	if err != nil {
		t.Fatalf("AlbumHeardTracks: %v", err)
	}
	if len(before) == 0 {
		t.Fatal("no tracks were heard before blacklisting; the fixture is wrong")
	}

	if err := env.Catalog.Blacklist(ctx, env.Store.DB(), user.ID, "artist00000000000000001"); err != nil {
		t.Fatalf("Blacklist: %v", err)
	}
	after, err := svc.AlbumHeardTracks(ctx, env.Store.DB(), user.ID, "album000000000000000001")
	if err != nil {
		t.Fatalf("AlbumHeardTracks after blacklisting: %v", err)
	}
	if len(after) != 0 {
		t.Fatalf("%d tracks still counted as heard after the artist was blacklisted, want 0", len(after))
	}
}

// There is deliberately no TestAlbumHeardTracksIgnoresTheRange here. The plan
// this task comes from asked for one shaped like the two tests above: seed
// plays in 2019, call AlbumHeardTracks, assert 9 came back. AlbumHeardTracks
// takes no range argument at all — the same reason AlbumCompletion has none
// (see the removed-test comment above, between
// TestAlbumCompletionCountsDistinctTracks and
// TestAlbumCompletionUnknownWhenUnresolved) — so that test could only ever
// re-run the exact call TestAlbumHeardTracksMatchesTheCompletionNumerator
// above already makes, against the same fixture, and check the same number.
// A test that cannot vary the range cannot show the answer is independent of
// one; it would have been the identical defect this file already removed a
// test for once, under a new name.
//
// What actually pins "not range-filtered" is
// TestAlbumHeardTracksSQLIsNotRangeScoped in internal/stats/stats_test.go,
// which reads the composed statement and fails if it ever mentions
// played_at — the only column a range predicate could be written against.
// That fails on a bad edit without needing a fixture or a database, which a
// test built from this function's signature never could.
