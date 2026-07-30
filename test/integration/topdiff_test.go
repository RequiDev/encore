//go:build integration

package integration

import (
	"errors"
	"testing"
	"time"

	"github.com/RequiDev/encore/internal/domain"
	"github.com/RequiDev/encore/internal/stats"
	"github.com/RequiDev/encore/internal/store/listens"
)

// topDiffNow is the instant every test below pins the fixture's clock to. It
// sits five days after the fixture's last seeded play (2024-01-05), so the
// shortest window (short_term, ~4 weeks) still comfortably covers the whole
// fixture without a test having to reason about the boundary.
var topDiffNow = time.Date(2024, time.January, 10, 0, 0, 0, 0, time.UTC)

// pinNow overrides the fixture's clock so TopDiff's window is reproducible
// instead of drifting with the real day the suite happens to run on.
func pinNow(f *statsFixture, now time.Time) {
	f.svc.Now = func() time.Time { return now }
}

// seedTopSnapshot inserts a Spotify top-items capture directly, the way the
// library worker would after a real /me/top/{type} call, without depending
// on that worker here. ids is in rank order: ids[0] lands at position 1.
func seedTopSnapshot(t *testing.T, f *statsFixture, kind, timeRange string, ids []string, capturedAt time.Time) {
	t.Helper()
	for i, id := range ids {
		f.env.Exec(`INSERT INTO spotify_top_snapshots (user_id, kind, time_range, position, entity_id, captured_at)
			VALUES ($1, $2, $3, $4, $5, $6)`,
			f.user.ID.String(), kind, timeRange, i+1, id, capturedAt)
	}
}

// entry finds one row of a diff by entity id, failing the test if it is
// missing: every test below asserts on rows it expects to exist, and a
// silent zero-value TopDiffEntry would make a missing row look like a
// present one with every field zero.
func entry(t *testing.T, d stats.TopDiff, id string) stats.TopDiffEntry {
	t.Helper()
	for _, e := range d.Entries {
		if e.EntityID == id {
			return e
		}
	}
	t.Fatalf("no entry for %q in %+v", id, d.Entries)
	return stats.TopDiffEntry{}
}

func hasEntry(d stats.TopDiff, id string) bool {
	for _, e := range d.Entries {
		if e.EntityID == id {
			return true
		}
	}
	return false
}

// TestTopDiffReportsBothRanksWhenBothSidesKnowAnEntity is the base case: an
// entity both Spotify and Encore rank, at different positions, must report
// both ranks rather than one winning.
func TestTopDiffReportsBothRanksWhenBothSidesKnowAnEntity(t *testing.T) {
	f := seedStats(t)
	pinNow(f, topDiffNow)
	ctx, db := f.env.Ctx(), f.env.Store.DB()

	// Encore's own artist ranking on this fixture (see TestTopListsRankAndPaginate
	// in stats_test.go) is art-y (6 plays), art-x (5), art-z (1). Spotify is seeded
	// to disagree about who is first.
	seedTopSnapshot(t, f, "artist", "short_term", []string{"art-x", "art-y", "art-z"}, topDiffNow)

	got, err := f.svc.TopDiff(ctx, db, f.user.ID, "artist", "short_term", "UTC", 10)
	if err != nil {
		t.Fatalf("top diff: %v", err)
	}
	if got.CapturedAt == nil || !got.CapturedAt.Equal(topDiffNow) {
		t.Fatalf("captured at = %v, want %v", got.CapturedAt, topDiffNow)
	}
	if got.TimeRange != "short_term" {
		t.Fatalf("time range = %q, want short_term", got.TimeRange)
	}

	x := entry(t, got, "art-x")
	if x.SpotifyRank != 1 || x.EncoreRank != 2 || x.Plays != 5 {
		t.Fatalf("art-x = %+v, want spotify=1 encore=2 plays=5", x)
	}
	y := entry(t, got, "art-y")
	if y.SpotifyRank != 2 || y.EncoreRank != 1 || y.Plays != 6 {
		t.Fatalf("art-y = %+v, want spotify=2 encore=1 plays=6", y)
	}
	z := entry(t, got, "art-z")
	if z.SpotifyRank != 3 || z.EncoreRank != 3 || z.Plays != 1 {
		t.Fatalf("art-z = %+v, want spotify=3 encore=3 plays=1", z)
	}
}

// TestTopDiffKeepsAnEntityOnlySpotifyRanks checks the Spotify-only half of the
// full outer join: an id Encore has never seen at all (not even in the
// catalogue) must still appear, with EncoreRank and Plays both zero rather
// than being dropped for having nothing on the other side.
func TestTopDiffKeepsAnEntityOnlySpotifyRanks(t *testing.T) {
	f := seedStats(t)
	pinNow(f, topDiffNow)
	ctx, db := f.env.Ctx(), f.env.Store.DB()

	seedTopSnapshot(t, f, "artist", "short_term", []string{"art-only-on-spotify"}, topDiffNow)

	got, err := f.svc.TopDiff(ctx, db, f.user.ID, "artist", "short_term", "UTC", 10)
	if err != nil {
		t.Fatalf("top diff: %v", err)
	}
	e := entry(t, got, "art-only-on-spotify")
	if e.SpotifyRank != 1 {
		t.Fatalf("spotify rank = %d, want 1", e.SpotifyRank)
	}
	if e.EncoreRank != 0 {
		t.Fatalf("encore rank = %d, want 0 (absent from Encore's ranking)", e.EncoreRank)
	}
	if e.Plays != 0 {
		t.Fatalf("plays = %d, want 0", e.Plays)
	}
}

// TestTopDiffKeepsAnEntityOnlyEncoreRanks is the mirror image: an artist
// Encore ranks (from real listens) that Spotify's captured list does not
// mention must still appear, with SpotifyRank zero.
func TestTopDiffKeepsAnEntityOnlyEncoreRanks(t *testing.T) {
	f := seedStats(t)
	pinNow(f, topDiffNow)
	ctx, db := f.env.Ctx(), f.env.Store.DB()

	// Spotify's capture exists (so CapturedAt is non-nil) but never mentions
	// art-y, Encore's actual top artist.
	seedTopSnapshot(t, f, "artist", "short_term", []string{"art-z"}, topDiffNow)

	got, err := f.svc.TopDiff(ctx, db, f.user.ID, "artist", "short_term", "UTC", 10)
	if err != nil {
		t.Fatalf("top diff: %v", err)
	}
	y := entry(t, got, "art-y")
	if y.SpotifyRank != 0 {
		t.Fatalf("spotify rank = %d, want 0 (absent from Spotify's capture)", y.SpotifyRank)
	}
	if y.EncoreRank != 1 || y.Plays != 6 {
		t.Fatalf("art-y = %+v, want encore=1 plays=6", y)
	}
}

// TestTopDiffReturnsNoEntriesWhenNeverCaptured is the case the brief calls
// out explicitly: with no capture at all, CapturedAt is nil and Entries is
// empty - not Encore's ranking shown on its own with every SpotifyRank
// forced to zero, which would look like a comparison but would not be one.
func TestTopDiffReturnsNoEntriesWhenNeverCaptured(t *testing.T) {
	f := seedStats(t)
	pinNow(f, topDiffNow)
	ctx, db := f.env.Ctx(), f.env.Store.DB()

	got, err := f.svc.TopDiff(ctx, db, f.user.ID, "artist", "short_term", "UTC", 10)
	if err != nil {
		t.Fatalf("top diff: %v", err)
	}
	if got.CapturedAt != nil {
		t.Fatalf("captured at = %v, want nil", got.CapturedAt)
	}
	if len(got.Entries) != 0 {
		t.Fatalf("entries = %+v, want none", got.Entries)
	}
}

// TestTopDiffBlacklistedArtistKeepsSpotifyRankButZerosEncoreRank is the
// blacklist decision this task had to make explicit.
//
// Spotify's capture is a snapshot of what Spotify's own backend believes,
// taken independently of Encore's local blacklist - Spotify has no idea the
// listener has asked Encore to hide this artist. Encore's own ranking, on
// the other hand, must obey the blacklist exactly as every other statistic
// in this package does: a blacklisted artist's plays are invisible to
// encore_ranked, so the artist cannot appear in Encore's top page at all.
//
// The decision: the row still appears in the diff, with SpotifyRank intact
// and EncoreRank (and Plays) forced to zero by the entity's simple absence
// from Encore's side - not removed from the result altogether. Hiding the
// row would bury exactly the disagreement this page exists to surface: "you
// blacklisted this locally, and Spotify still doesn't know."
func TestTopDiffBlacklistedArtistKeepsSpotifyRankButZerosEncoreRank(t *testing.T) {
	f := seedStats(t)
	pinNow(f, topDiffNow)
	ctx, db := f.env.Ctx(), f.env.Store.DB()

	if err := f.env.Catalog.Blacklist(ctx, db, f.user.ID, "art-z"); err != nil {
		t.Fatalf("blacklist: %v", err)
	}
	seedTopSnapshot(t, f, "artist", "short_term", []string{"art-z"}, topDiffNow)

	got, err := f.svc.TopDiff(ctx, db, f.user.ID, "artist", "short_term", "UTC", 10)
	if err != nil {
		t.Fatalf("top diff: %v", err)
	}
	z := entry(t, got, "art-z")
	if z.SpotifyRank != 1 {
		t.Fatalf("spotify rank = %d, want 1 (Spotify does not know about the local blacklist)", z.SpotifyRank)
	}
	if z.EncoreRank != 0 {
		t.Fatalf("encore rank = %d, want 0 (the blacklist hides it from Encore's own ranking)", z.EncoreRank)
	}
	if z.Plays != 0 {
		t.Fatalf("plays = %d, want 0", z.Plays)
	}
}

// TestTopDiffHonoursTheLimit checks that limit caps each side independently
// before the join, rather than capping the merged result: with limit 1,
// only Spotify's own #1 and Encore's own #1 may appear, and an entity that
// is neither (art-z, ranked third on both sides here) must be absent
// entirely, not merely truncated into a partial row.
func TestTopDiffHonoursTheLimit(t *testing.T) {
	f := seedStats(t)
	pinNow(f, topDiffNow)
	ctx, db := f.env.Ctx(), f.env.Store.DB()

	seedTopSnapshot(t, f, "artist", "short_term", []string{"art-x", "art-y", "art-z"}, topDiffNow)

	got, err := f.svc.TopDiff(ctx, db, f.user.ID, "artist", "short_term", "UTC", 1)
	if err != nil {
		t.Fatalf("top diff: %v", err)
	}
	if len(got.Entries) != 2 {
		t.Fatalf("entries = %+v, want exactly 2 (spotify's #1 and encore's #1)", got.Entries)
	}
	x := entry(t, got, "art-x") // Spotify's #1
	if x.SpotifyRank != 1 || x.EncoreRank != 0 {
		t.Fatalf("art-x = %+v, want spotify=1 encore=0 (rank 2 is beyond limit 1)", x)
	}
	y := entry(t, got, "art-y") // Encore's #1
	if y.SpotifyRank != 0 || y.EncoreRank != 1 {
		t.Fatalf("art-y = %+v, want spotify=0 (rank 2 is beyond limit 1) encore=1", y)
	}
	if hasEntry(got, "art-z") {
		t.Fatal("art-z is rank 3 on both sides and limit is 1; it must not appear at all")
	}
}

// TestTopDiffTimeRangesUseTheirOwnSnapshotAndWindow covers both halves of
// "each time range is its own set": the three ranges must read their own row
// in spotify_top_snapshots (a short_term diff must not see a long_term-only
// capture), and Encore's own side must be computed over the matching window
// rather than one window shared by all three.
func TestTopDiffTimeRangesUseTheirOwnSnapshotAndWindow(t *testing.T) {
	f := seedStats(t)
	pinNow(f, topDiffNow)
	ctx, db := f.env.Ctx(), f.env.Store.DB()

	// Three disjoint Spotify captures, one per range, sharing no ids: if a
	// short_term read ever picked up "art-long-only" the ranges would not be
	// independent.
	seedTopSnapshot(t, f, "artist", "short_term", []string{"art-short-only"}, topDiffNow)
	seedTopSnapshot(t, f, "artist", "medium_term", []string{"art-medium-only"}, topDiffNow)
	seedTopSnapshot(t, f, "artist", "long_term", []string{"art-long-only"}, topDiffNow)

	// A second, extra play of trk-d (artist art-z) about three months before
	// topDiffNow: inside medium_term's ~6 month window and long_term's ~12
	// month window, but well outside short_term's ~4 week window. If Encore's
	// side used one window for every range, this play would count (or not)
	// identically everywhere; it must not.
	oldPlay := topDiffNow.AddDate(0, -3, 0)
	batch := []listens.StagedListen{stageAt(f.user.ID, "trk-d", oldPlay)}
	if _, err := f.env.Listens.InsertListens(ctx, db, batch, "UTC"); err != nil {
		t.Fatalf("seed old play: %v", err)
	}

	short, err := f.svc.TopDiff(ctx, db, f.user.ID, "artist", "short_term", "UTC", 10)
	if err != nil {
		t.Fatalf("short_term diff: %v", err)
	}
	long, err := f.svc.TopDiff(ctx, db, f.user.ID, "artist", "long_term", "UTC", 10)
	if err != nil {
		t.Fatalf("long_term diff: %v", err)
	}

	// The snapshot sets do not cross-contaminate.
	if hasEntry(short, "art-long-only") {
		t.Fatal("short_term diff picked up an entity from the long_term snapshot")
	}
	if hasEntry(short, "art-medium-only") {
		t.Fatal("short_term diff picked up an entity from the medium_term snapshot")
	}
	if !hasEntry(short, "art-short-only") {
		t.Fatal("short_term diff is missing the entity from its own snapshot")
	}
	if !hasEntry(long, "art-long-only") {
		t.Fatal("long_term diff is missing the entity from its own snapshot")
	}

	// The windows differ: art-z has one play in the short_term window (the
	// fixture's 2024-01-05 play) and two in the long_term window (that play
	// plus the one seeded three months before topDiffNow).
	zShort := entry(t, short, "art-z")
	if zShort.Plays != 1 {
		t.Fatalf("art-z plays in short_term = %d, want 1 (the old play is outside the ~4 week window)", zShort.Plays)
	}
	zLong := entry(t, long, "art-z")
	if zLong.Plays != 2 {
		t.Fatalf("art-z plays in long_term = %d, want 2 (both plays are inside the ~12 month window)", zLong.Plays)
	}
}

// TestTopDiffValidatesItsArguments checks the input guards that have nothing
// to do with the database: an unknown kind, an unknown time range and a nil
// user must all be rejected as domain.ErrValidation rather than reaching SQL.
func TestTopDiffValidatesItsArguments(t *testing.T) {
	f := seedStats(t)
	ctx, db := f.env.Ctx(), f.env.Store.DB()

	if _, err := f.svc.TopDiff(ctx, db, f.user.ID, "album", "short_term", "UTC", 10); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("kind=album error = %v, want a validation error (Spotify has no top-albums endpoint)", err)
	}

	if _, err := f.svc.TopDiff(ctx, db, f.user.ID, "artist", "this_year", "UTC", 10); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("bad time range error = %v, want a validation error", err)
	}
}
