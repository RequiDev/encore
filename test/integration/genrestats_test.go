//go:build integration

package integration

import (
	"testing"
	"time"

	"github.com/RequiDev/encore/internal/domain"
)

// TestTopGenresCountsEachListenOncePerGenre pins the counting rule: a listen
// contributes one play to each distinct genre across all of its credited
// artists, and a genre shared by two credited artists is still one play.
//
// From the shared fixture: trk-a plays four times and credits art-x (rock) and
// art-y (jazz), trk-b twice-over-one play credits art-x, trk-c plays twice
// crediting art-y, trk-d once crediting art-z.
//
//	rock: trk-a x4 + trk-b x1 = 5
//	jazz: trk-a x4 + trk-c x2 = 6
//	folk: trk-d x1            = 1
func TestTopGenresCountsEachListenOncePerGenre(t *testing.T) {
	f := seedStats(t)

	page, err := f.svc.TopGenres(f.env.Ctx(), f.env.Store.DB(), f.user.ID, f.fullRange(), f.tz, 10, 0)
	if err != nil {
		t.Fatalf("top genres: %v", err)
	}

	want := map[string]int64{"rock": 5, "jazz": 6, "folk": 1}
	if len(page.Genres) != len(want) {
		t.Fatalf("got %d genres, want %d: %+v", len(page.Genres), len(want), page.Genres)
	}
	for _, g := range page.Genres {
		if want[g.Genre] != g.Plays {
			t.Errorf("%s: got %d plays, want %d", g.Genre, g.Plays, want[g.Genre])
		}
	}
	if page.Total != 3 {
		t.Errorf("total genres = %d, want 3", page.Total)
	}
}

// TestTopGenresDeduplicatesASharedGenre is the case the DISTINCT in
// trackGenreCTE exists for. Retagging art-y as rock means trk-a credits two
// artists who are both rock; the four plays of it must add four to rock, not
// eight.
func TestTopGenresDeduplicatesASharedGenre(t *testing.T) {
	f := seedStats(t)
	f.env.Exec(`UPDATE artists SET genres = ARRAY['rock'] WHERE id = 'art-y'`)

	page, err := f.svc.TopGenres(f.env.Ctx(), f.env.Store.DB(), f.user.ID, f.fullRange(), f.tz, 10, 0)
	if err != nil {
		t.Fatalf("top genres: %v", err)
	}

	var rock int64
	for _, g := range page.Genres {
		if g.Genre == "rock" {
			rock = g.Plays
		}
	}
	// trk-a x4 (both artists rock, counted once) + trk-b x1 + trk-c x2 = 7
	if rock != 7 {
		t.Errorf("rock = %d plays, want 7 — a genre shared by two credited artists was double counted", rock)
	}
}

// TestGenreCoverageExcludesUnenrichedArtists is what stops a fresh instance from
// rendering an empty chart that looks like a bug. Stripping art-x's genres
// leaves every listen of trk-b uncovered; trk-a stays covered because art-y
// still supplies one.
func TestGenreCoverageExcludesUnenrichedArtists(t *testing.T) {
	f := seedStats(t)
	f.env.Exec(`UPDATE artists SET genres = '{}' WHERE id = 'art-x'`)

	page, err := f.svc.TopGenres(f.env.Ctx(), f.env.Store.DB(), f.user.ID, f.fullRange(), f.tz, 10, 0)
	if err != nil {
		t.Fatalf("top genres: %v", err)
	}

	// Eight listens total; the one play of trk-b is the only one with no genred artist.
	if page.Coverage.Total != 8 {
		t.Errorf("coverage total = %d, want 8", page.Coverage.Total)
	}
	if page.Coverage.Covered != 7 {
		t.Errorf("coverage covered = %d, want 7", page.Coverage.Covered)
	}
}

// TestTopGenresRespectsTheBlacklist checks the fragment did its job. Blacklisting
// art-x removes every listen of any track crediting it — trk-a and trk-b — so
// rock disappears entirely and jazz keeps only trk-c's two plays.
func TestTopGenresRespectsTheBlacklist(t *testing.T) {
	f := seedStats(t)
	f.env.Exec(`INSERT INTO user_blacklisted_artists (user_id, artist_id) VALUES ($1, 'art-x')`, f.user.ID)

	page, err := f.svc.TopGenres(f.env.Ctx(), f.env.Store.DB(), f.user.ID, f.fullRange(), f.tz, 10, 0)
	if err != nil {
		t.Fatalf("top genres: %v", err)
	}

	got := map[string]int64{}
	for _, g := range page.Genres {
		got[g.Genre] = g.Plays
	}
	if _, ok := got["rock"]; ok {
		t.Error("rock survived blacklisting its only artist")
	}
	if got["jazz"] != 2 {
		t.Errorf("jazz = %d plays, want 2 (trk-c only)", got["jazz"])
	}
	if got["folk"] != 1 {
		t.Errorf("folk = %d plays, want 1", got["folk"])
	}
}

// TestTopGenresRollupMatchesTheFactTable is the test that makes the rollup path
// safe to have at all. Two statements answering one question must agree, or a
// wide range silently returns different numbers from a narrow one.
//
// The range is deliberately wide and aligned to local midnight so useRollup says
// yes; refreshing the rollup first is what makes the comparison meaningful,
// because a dirty rollup would send both calls down the fact-table path and the
// test would pass without ever exercising the rollup SQL.
func TestTopGenresRollupMatchesTheFactTable(t *testing.T) {
	f := seedStats(t)

	// Drain the dirty queue so the rollup is current and eligible.
	if err := f.svc.RefreshDirtyDays(f.env.Ctx(), 1000); err != nil {
		t.Fatalf("refresh rollups: %v", err)
	}

	// RollupMinRange is 90 days, so a shorter range would take the fact-table
	// path in both calls and the comparison would prove nothing. Six months,
	// starting at a local midnight, clears it.
	wide := f.fullRange()
	wide.To = wide.From.AddDate(0, 6, 0)

	dirty, err := f.svc.HasDirtyDays(f.env.Ctx(), f.env.Store.DB(), f.user.ID, wide, f.tz)
	if err != nil {
		t.Fatalf("dirty check: %v", err)
	}
	if dirty {
		t.Fatal("rollups are still dirty after a refresh; this test would not exercise the rollup path")
	}

	viaRollup, err := f.svc.TopGenres(f.env.Ctx(), f.env.Store.DB(), f.user.ID, wide, f.tz, 50, 0)
	if err != nil {
		t.Fatalf("top genres via rollup: %v", err)
	}

	// Force the fact-table path by dirtying a day inside the range.
	f.env.Exec(`INSERT INTO rollup_dirty_days (user_id, day) VALUES ($1, DATE '2024-01-01')
	            ON CONFLICT DO NOTHING`, f.user.ID)
	viaFacts, err := f.svc.TopGenres(f.env.Ctx(), f.env.Store.DB(), f.user.ID, wide, f.tz, 50, 0)
	if err != nil {
		t.Fatalf("top genres via facts: %v", err)
	}

	if viaRollup.Total != viaFacts.Total {
		t.Fatalf("totals differ: rollup %d, facts %d", viaRollup.Total, viaFacts.Total)
	}
	if len(viaRollup.Genres) != len(viaFacts.Genres) {
		t.Fatalf("row counts differ: rollup %d, facts %d", len(viaRollup.Genres), len(viaFacts.Genres))
	}
	for i := range viaRollup.Genres {
		if viaRollup.Genres[i] != viaFacts.Genres[i] {
			t.Errorf("row %d differs: rollup %+v, facts %+v", i, viaRollup.Genres[i], viaFacts.Genres[i])
		}
	}
}

// TestTopGenresEmptyRangeIsNotAnError guards the state every new instance is in:
// a valid window that simply contains no listens.
//
// Note it is NOT a zero-width range. scope() rejects from == to as
// domain.ErrValidation by deliberate design, so a zero-width range can only ever
// error and would be testing the wrong thing.
func TestTopGenresEmptyRangeIsNotAnError(t *testing.T) {
	f := seedStats(t)
	from := time.Date(2025, time.January, 1, 0, 0, 0, 0, f.loc)
	empty := domain.TimeRange{From: from, To: from.AddDate(0, 0, 10)}

	page, err := f.svc.TopGenres(f.env.Ctx(), f.env.Store.DB(), f.user.ID, empty, f.tz, 10, 0)
	if err != nil {
		t.Fatalf("top genres over an empty range: %v", err)
	}
	if len(page.Genres) != 0 || page.Total != 0 {
		t.Errorf("expected no genres, got %+v", page)
	}
	if page.Coverage.Covered != 0 || page.Coverage.Total != 0 {
		t.Errorf("expected zero coverage, got %+v", page.Coverage)
	}
}

// TestGenreTimelineReturnsACompleteGrid checks the property every chart in
// Encore relies on: an empty bucket is a zero, never a missing point, and the
// caller's genre list fixes the series so they stay stable while paging.
//
// 2024-01-04 is silent in the fixture, so it is the bucket that matters.
func TestGenreTimelineReturnsACompleteGrid(t *testing.T) {
	f := seedStats(t)

	points, err := f.svc.GenreTimeline(f.env.Ctx(), f.env.Store.DB(), f.user.ID,
		f.fullRange(), f.tz, domain.IntervalDay, []string{"rock", "jazz"})
	if err != nil {
		t.Fatalf("genre timeline: %v", err)
	}

	// Ten days in fullRange, two genres.
	if len(points) != 20 {
		t.Fatalf("got %d points, want 20", len(points))
	}

	type key struct {
		day   string
		genre string
	}
	got := map[key]int64{}
	for _, p := range points {
		got[key{p.Bucket.In(f.loc).Format("2006-01-02"), p.Genre}] = p.Plays
	}

	// rock is art-x: trk-a x3 and trk-b x1 on the 1st, trk-a x1 on the 3rd.
	if got[key{"2024-01-01", "rock"}] != 4 {
		t.Errorf("rock on the 1st = %d, want 4", got[key{"2024-01-01", "rock"}])
	}
	// jazz is art-y, credited on trk-a and trk-c: three on the 1st, two on the 2nd.
	if got[key{"2024-01-02", "jazz"}] != 2 {
		t.Errorf("jazz on the 2nd = %d, want 2", got[key{"2024-01-02", "jazz"}])
	}
	if v, ok := got[key{"2024-01-04", "rock"}]; !ok || v != 0 {
		t.Errorf("the silent day is missing or non-zero for rock: %v %v", v, ok)
	}
}

// TestGenreTimelineWithNoGenresIsEmpty guards the degenerate call rather than
// letting it build a grid of nothing.
func TestGenreTimelineWithNoGenresIsEmpty(t *testing.T) {
	f := seedStats(t)

	points, err := f.svc.GenreTimeline(f.env.Ctx(), f.env.Store.DB(), f.user.ID,
		f.fullRange(), f.tz, domain.IntervalDay, nil)
	if err != nil {
		t.Fatalf("genre timeline: %v", err)
	}
	if len(points) != 0 {
		t.Errorf("got %d points for no genres, want 0", len(points))
	}
}

// TestTasteObscurityIsPlayWeighted checks the mean is over listens rather than
// over artists: an artist played ten times must pull the score ten times as hard
// as one played once.
func TestTasteObscurityIsPlayWeighted(t *testing.T) {
	f := seedStats(t)
	f.env.Exec(`UPDATE artists SET popularity = 90 WHERE id = 'art-x'`)
	f.env.Exec(`UPDATE artists SET popularity = 30 WHERE id = 'art-y'`)
	f.env.Exec(`UPDATE artists SET popularity = 0  WHERE id = 'art-z'`)

	got, err := f.svc.Taste(f.env.Ctx(), f.env.Store.DB(), f.user.ID, f.fullRange(), f.tz)
	if err != nil {
		t.Fatalf("taste: %v", err)
	}

	// Per listen, the mean is taken over that listen's own credited artists
	// first (the inner CTE), and only then averaged across listens (the outer
	// query) — a listen of trk-a, which credits two artists, is one unit of
	// weight, not two:
	//   trk-a x4, each listen averaging art-x(90) and art-y(30) -> 60 x4 = 240
	//   trk-b x1, averaging art-x(90) alone                     -> 90
	//   trk-c x2, averaging art-y(30) alone                     -> 30 x2 = 60
	//   trk-d x1, averaging art-z(0) alone                      -> 0
	// (240 + 90 + 60 + 0) / 8 = 390/8 = 48.75
	if diff := got.Obscurity - 48.75; diff > 0.001 || diff < -0.001 {
		t.Errorf("obscurity = %v, want 48.75", got.Obscurity)
	}
	if got.ObscurityCoverage.Total != 8 || got.ObscurityCoverage.Covered != 8 {
		t.Errorf("obscurity coverage = %+v, want 8/8", got.ObscurityCoverage)
	}
}

// TestTasteObscurityExcludesUnresolvedArtists is the coverage half: an artist
// enrichment has not resolved carries popularity 0 by column default, and
// counting that as "not popular" would drag every fresh instance's score to zero.
func TestTasteObscurityExcludesUnresolvedArtists(t *testing.T) {
	f := seedStats(t)
	f.env.Exec(`UPDATE artists SET popularity = 80, metadata_state = 'resolved' WHERE id = 'art-x'`)
	f.env.Exec(`UPDATE artists SET popularity = 0,  metadata_state = 'pending'  WHERE id = 'art-y'`)
	f.env.Exec(`UPDATE artists SET popularity = 0,  metadata_state = 'pending'  WHERE id = 'art-z'`)

	got, err := f.svc.Taste(f.env.Ctx(), f.env.Store.DB(), f.user.ID, f.fullRange(), f.tz)
	if err != nil {
		t.Fatalf("taste: %v", err)
	}
	if diff := got.Obscurity - 80.0; diff > 0.001 || diff < -0.001 {
		t.Errorf("obscurity = %v, want 80 — an unresolved artist was counted as popularity 0", got.Obscurity)
	}
	// trk-a and trk-b credit art-x; that is five listens of eight.
	if got.ObscurityCoverage.Covered != 5 || got.ObscurityCoverage.Total != 8 {
		t.Errorf("obscurity coverage = %+v, want 5/8", got.ObscurityCoverage)
	}
}

// TestTasteReleaseLag answers "how old is the music you listen to".
func TestTasteReleaseLag(t *testing.T) {
	f := seedStats(t)

	got, err := f.svc.Taste(f.env.Ctx(), f.env.Store.DB(), f.user.ID, f.fullRange(), f.tz)
	if err != nil {
		t.Fatalf("taste: %v", err)
	}

	// All plays are in 2024. alb-1 (2010) carries trk-a x4 and trk-b x1;
	// alb-2 (2020) carries trk-c x2; alb-3 (2000) carries trk-d x1.
	// (5*14 + 2*4 + 1*24) / 8 = 102/8 = 12.75
	if diff := got.ReleaseLagYears - 12.75; diff > 0.001 || diff < -0.001 {
		t.Errorf("release lag = %v, want 12.75", got.ReleaseLagYears)
	}
	if got.ReleaseLagCoverage.Covered != 8 || got.ReleaseLagCoverage.Total != 8 {
		t.Errorf("release lag coverage = %+v, want 8/8", got.ReleaseLagCoverage)
	}
}

// TestTasteEmptyRangeIsNotAnError guards the same state as the genre case: a
// valid window that simply contains no listens.
//
// Note it is NOT a zero-width range. scope() rejects from == to as
// domain.ErrValidation by deliberate design, so a zero-width range can only
// ever error and would be testing the wrong thing.
func TestTasteEmptyRangeIsNotAnError(t *testing.T) {
	f := seedStats(t)
	from := time.Date(2025, time.January, 1, 0, 0, 0, 0, f.loc)
	empty := domain.TimeRange{From: from, To: from.AddDate(0, 0, 10)}

	got, err := f.svc.Taste(f.env.Ctx(), f.env.Store.DB(), f.user.ID, empty, f.tz)
	if err != nil {
		t.Fatalf("taste over an empty range: %v", err)
	}
	if got.Obscurity != 0 || got.ObscurityCoverage.Total != 0 {
		t.Errorf("expected a zeroed taste, got %+v", got)
	}
}
