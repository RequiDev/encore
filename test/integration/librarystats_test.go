//go:build integration

package integration

import (
	"testing"
	"time"

	"github.com/RequiDev/encore/internal/domain"
)

// The tests in this file cross the shared statsFixture's listening history
// (seedStats, in stats_test.go) against a user's Spotify library — saved
// tracks, saved albums, followed artists — which these tests seed directly
// with f.env.Exec, exactly as the library tables' own reconciliation tests do.
//
// Three scopings are deliberately different and each gets its own test:
// saved-but-never-played is all-time (TestLibraryStatsSavedNeverPlayedIsAllTime),
// played-but-never-saved and followed-but-dormant are both range-scoped
// (TestLibraryStatsPlayedNeverSavedIsRangeScoped,
// TestLibraryStatsDormantFollowsTracksTheRange).

// TestLibraryStatsNeverSyncedIsZeroAndNotAnError pins the common state on a
// freshly upgraded instance: seedStats's user has no spotify_credentials row
// at all (NewUser does not create one), so there is nothing to enumerate from
// and the snapshot must still answer rather than error.
//
// PlayedNeverSaved is deliberately not asserted empty here: the shared fixture
// always seeds eight plays, and since this test saves and follows nothing,
// every one of those played tracks correctly qualifies as "played, never
// saved" — that list answers a question about listening and saving, not
// about whether the library has ever been synced. Only the snapshot fields and
// the two lists that actually depend on a library existing are this test's
// concern.
func TestLibraryStatsNeverSyncedIsZeroAndNotAnError(t *testing.T) {
	f := seedStats(t)

	got, err := f.svc.Library(f.env.Ctx(), f.env.Store.DB(), f.user.ID, f.fullRange(), f.tz, 10)
	if err != nil {
		t.Fatalf("library: %v", err)
	}
	if got.SyncedAt != nil {
		t.Errorf("synced at = %v, want nil for an account that has never been enumerated", got.SyncedAt)
	}
	if got.SavedTracks != 0 || got.SavedAlbums != 0 || got.FollowedArtists != 0 {
		t.Errorf("counts = %+v, want all zero", got)
	}
	if len(got.SavedNeverPlayed) != 0 {
		t.Errorf("saved-never-played = %+v, want empty: nothing has ever been saved", got.SavedNeverPlayed)
	}
	if len(got.DormantFollows) != 0 {
		t.Errorf("dormant-follows = %+v, want empty: nothing is followed", got.DormantFollows)
	}
}

// TestLibraryStatsCountsReflectAllThreeTables checks the snapshot's counts and
// SyncedAt once rows actually exist, so the zero case above is not the only
// one ever exercised.
func TestLibraryStatsCountsReflectAllThreeTables(t *testing.T) {
	f := seedStats(t)
	ctx, db := f.env.Ctx(), f.env.Store.DB()

	connect(t, f.env, f.user.ID, time.Now().Add(time.Hour))
	syncedAt := time.Date(2024, time.June, 1, 12, 0, 0, 0, time.UTC)
	if err := f.env.Accounts.Credentials.MarkLibrarySynced(ctx, db, f.user.ID, syncedAt); err != nil {
		t.Fatalf("mark library synced: %v", err)
	}

	f.env.Exec(`INSERT INTO user_saved_tracks (user_id, track_id) VALUES ($1, 'trk-zzz')`, f.user.ID)
	f.env.Exec(`INSERT INTO user_saved_albums (user_id, album_id) VALUES ($1, 'alb-1'), ($1, 'alb-2')`, f.user.ID)
	f.env.Exec(`INSERT INTO user_followed_artists (user_id, artist_id) VALUES ($1, 'art-x'), ($1, 'art-z2'), ($1, 'art-z3')`, f.user.ID)

	got, err := f.svc.Library(ctx, db, f.user.ID, f.fullRange(), f.tz, 10)
	if err != nil {
		t.Fatalf("library: %v", err)
	}
	if got.SyncedAt == nil || !got.SyncedAt.Equal(syncedAt) {
		t.Errorf("synced at = %v, want %v", got.SyncedAt, syncedAt)
	}
	if got.SavedTracks != 1 {
		t.Errorf("saved tracks = %d, want 1", got.SavedTracks)
	}
	if got.SavedAlbums != 2 {
		t.Errorf("saved albums = %d, want 2", got.SavedAlbums)
	}
	if got.FollowedArtists != 3 {
		t.Errorf("followed artists = %d, want 3", got.FollowedArtists)
	}
}

// TestLibraryStatsSavedNeverPlayedIsAllTime is the scoping the brief insists on
// hardest: trk-a has been played (in the fixture's history) and trk-zzz never
// has. Only trk-zzz belongs in the list, and it must still be there when the
// range is narrowed to a window containing no plays whatsoever, because
// "never played" is a fact about the whole listening history, not about
// whatever window happens to be on screen.
func TestLibraryStatsSavedNeverPlayedIsAllTime(t *testing.T) {
	f := seedStats(t)
	ctx, db := f.env.Ctx(), f.env.Store.DB()

	f.env.Exec(`INSERT INTO user_saved_tracks (user_id, track_id) VALUES ($1, 'trk-a'), ($1, 'trk-zzz')`, f.user.ID)

	got, err := f.svc.Library(ctx, db, f.user.ID, f.fullRange(), f.tz, 10)
	if err != nil {
		t.Fatalf("library: %v", err)
	}
	if len(got.SavedNeverPlayed) != 1 || got.SavedNeverPlayed[0].TrackID != "trk-zzz" {
		t.Fatalf("saved-never-played = %+v, want exactly [trk-zzz]", got.SavedNeverPlayed)
	}

	// A window with no plays in it at all: 2024-01-06 through 2024-01-10, after
	// the fixture's last play on the 5th.
	noPlays := domain.TimeRange{
		From: time.Date(2024, time.January, 6, 0, 0, 0, 0, f.loc),
		To:   time.Date(2024, time.January, 10, 0, 0, 0, 0, f.loc),
	}
	got2, err := f.svc.Library(ctx, db, f.user.ID, noPlays, f.tz, 10)
	if err != nil {
		t.Fatalf("library over a window with no plays: %v", err)
	}
	if len(got2.SavedNeverPlayed) != 1 || got2.SavedNeverPlayed[0].TrackID != "trk-zzz" {
		t.Fatalf("saved-never-played over a playless window = %+v, want exactly [trk-zzz] still — "+
			"'never played' must not be range-scoped", got2.SavedNeverPlayed)
	}
}

// TestLibraryStatsPlayedNeverSavedIsRangeScoped is the opposite scoping:
// nothing is saved, so trk-a (played four times inside the full range) shows
// up — but narrowing the range to exclude its plays must make it disappear,
// because this list describes the requested window, not all of history.
func TestLibraryStatsPlayedNeverSavedIsRangeScoped(t *testing.T) {
	f := seedStats(t)
	ctx, db := f.env.Ctx(), f.env.Store.DB()

	got, err := f.svc.Library(ctx, db, f.user.ID, f.fullRange(), f.tz, 10)
	if err != nil {
		t.Fatalf("library: %v", err)
	}
	found := false
	for _, e := range got.PlayedNeverSaved {
		if e.TrackID == "trk-a" {
			found = true
			if e.Plays != 4 {
				t.Errorf("trk-a plays = %d, want 4", e.Plays)
			}
		}
	}
	if !found {
		t.Fatal("trk-a is played and never saved, but is missing from played-never-saved")
	}

	// 2024-01-05 only: trk-d plays here, trk-a does not.
	day5 := domain.TimeRange{
		From: time.Date(2024, time.January, 5, 0, 0, 0, 0, f.loc),
		To:   time.Date(2024, time.January, 6, 0, 0, 0, 0, f.loc),
	}
	got2, err := f.svc.Library(ctx, db, f.user.ID, day5, f.tz, 10)
	if err != nil {
		t.Fatalf("library over the 5th: %v", err)
	}
	for _, e := range got2.PlayedNeverSaved {
		if e.TrackID == "trk-a" {
			t.Fatal("trk-a appears in played-never-saved for a range that excludes every one of its plays")
		}
	}
}

// TestLibraryStatsDormantFollowsTracksTheRange follows art-x, which plays on
// the 1st and 3rd, and art-z2, which never plays at all. Only art-z2 is
// dormant across the full range; narrowing to the 4th (the fixture's
// deliberately silent day) must make art-x dormant too, and only for that
// window, proving the range actually drives the answer rather than a cached
// all-time fact.
func TestLibraryStatsDormantFollowsTracksTheRange(t *testing.T) {
	f := seedStats(t)
	ctx, db := f.env.Ctx(), f.env.Store.DB()

	f.env.Exec(`INSERT INTO user_followed_artists (user_id, artist_id) VALUES ($1, 'art-x'), ($1, 'art-z2')`, f.user.ID)

	got, err := f.svc.Library(ctx, db, f.user.ID, f.fullRange(), f.tz, 10)
	if err != nil {
		t.Fatalf("library: %v", err)
	}
	dormant := map[string]bool{}
	for _, e := range got.DormantFollows {
		dormant[e.ArtistID] = true
	}
	if dormant["art-x"] {
		t.Error("art-x played inside the full range but was reported dormant")
	}
	if !dormant["art-z2"] {
		t.Error("art-z2 never played and should be dormant")
	}

	// 2024-01-04 is silent for every artist.
	day4 := domain.TimeRange{
		From: time.Date(2024, time.January, 4, 0, 0, 0, 0, f.loc),
		To:   time.Date(2024, time.January, 5, 0, 0, 0, 0, f.loc),
	}
	got2, err := f.svc.Library(ctx, db, f.user.ID, day4, f.tz, 10)
	if err != nil {
		t.Fatalf("library over the silent day: %v", err)
	}
	dormant2 := map[string]bool{}
	for _, e := range got2.DormantFollows {
		dormant2[e.ArtistID] = true
	}
	if !dormant2["art-x"] {
		t.Error("art-x has no play on the silent day and should be dormant for that window")
	}
	if !dormant2["art-z2"] {
		t.Error("art-z2 should still be dormant")
	}
}

// TestLibraryStatsBlacklistRemovesTracksAndHidesTheArtistEntirely is the trap
// the brief calls out by name. art-z is followed and has exactly one play in
// the full range (trk-d, on the 5th); blacklisting it must make its track
// disappear from played-never-saved (the blacklist rule applies everywhere)
// and must make the artist itself vanish from dormant-follows — not appear
// there as dormant, which is what a naive implementation would produce by
// hiding the artist's only play and then reading "no qualifying play" as
// "dormant".
func TestLibraryStatsBlacklistRemovesTracksAndHidesTheArtistEntirely(t *testing.T) {
	f := seedStats(t)
	ctx, db := f.env.Ctx(), f.env.Store.DB()

	f.env.Exec(`INSERT INTO user_followed_artists (user_id, artist_id) VALUES ($1, 'art-z'), ($1, 'art-z2')`, f.user.ID)

	if err := f.env.Catalog.Blacklist(ctx, db, f.user.ID, "art-z"); err != nil {
		t.Fatalf("blacklist: %v", err)
	}

	got, err := f.svc.Library(ctx, db, f.user.ID, f.fullRange(), f.tz, 10)
	if err != nil {
		t.Fatalf("library: %v", err)
	}

	for _, e := range got.PlayedNeverSaved {
		if e.TrackID == "trk-d" {
			t.Fatal("trk-d belongs to a blacklisted artist and must not appear in played-never-saved")
		}
	}

	for _, e := range got.DormantFollows {
		if e.ArtistID == "art-z" {
			t.Fatal("art-z is blacklisted and must be absent from dormant-follows entirely, " +
				"not reported as dormant")
		}
	}
	foundZ2 := false
	for _, e := range got.DormantFollows {
		if e.ArtistID == "art-z2" {
			foundZ2 = true
		}
	}
	if !foundZ2 {
		t.Error("art-z2 is not blacklisted, never played, and should still be dormant")
	}
}

// TestLibraryStatsLimitIsHonoured checks the shared limit across all three
// lists at once, since a caller wanting one invariably wants the others capped
// the same way.
func TestLibraryStatsLimitIsHonoured(t *testing.T) {
	f := seedStats(t)
	ctx, db := f.env.Ctx(), f.env.Store.DB()

	f.env.Exec(`INSERT INTO user_saved_tracks (user_id, track_id) VALUES
	    ($1, 'trk-zzz1'), ($1, 'trk-zzz2'), ($1, 'trk-zzz3'), ($1, 'trk-zzz4'), ($1, 'trk-zzz5')`, f.user.ID)
	f.env.Exec(`INSERT INTO user_followed_artists (user_id, artist_id) VALUES
	    ($1, 'art-f1'), ($1, 'art-f2'), ($1, 'art-f3'), ($1, 'art-f4'), ($1, 'art-f5')`, f.user.ID)

	// Nothing is saved, so all four fixture tracks (trk-a..d) qualify for
	// played-never-saved — more than the limit below.
	got, err := f.svc.Library(ctx, db, f.user.ID, f.fullRange(), f.tz, 2)
	if err != nil {
		t.Fatalf("library: %v", err)
	}
	if len(got.SavedNeverPlayed) != 2 {
		t.Errorf("saved-never-played returned %d rows, want 2 (the limit)", len(got.SavedNeverPlayed))
	}
	if len(got.PlayedNeverSaved) != 2 {
		t.Errorf("played-never-saved returned %d rows, want 2 (the limit)", len(got.PlayedNeverSaved))
	}
	if len(got.DormantFollows) != 2 {
		t.Errorf("dormant-follows returned %d rows, want 2 (the limit)", len(got.DormantFollows))
	}
}

// TestLibraryStatsEmptyRangeIsNotAnError guards the state every fresh instance
// is in: a valid window that simply contains no listens.
//
// Note it is NOT a zero-width range. scope() rejects from == to as
// domain.ErrValidation by deliberate design, so a zero-width range can only
// ever error and would be testing the wrong thing.
func TestLibraryStatsEmptyRangeIsNotAnError(t *testing.T) {
	f := seedStats(t)
	from := time.Date(2025, time.January, 1, 0, 0, 0, 0, f.loc)
	empty := domain.TimeRange{From: from, To: from.AddDate(0, 0, 10)}

	f.env.Exec(`INSERT INTO user_followed_artists (user_id, artist_id) VALUES ($1, 'art-x')`, f.user.ID)

	got, err := f.svc.Library(f.env.Ctx(), f.env.Store.DB(), f.user.ID, empty, f.tz, 10)
	if err != nil {
		t.Fatalf("library over an empty range: %v", err)
	}
	if len(got.PlayedNeverSaved) != 0 {
		t.Errorf("played-never-saved over an empty range = %+v, want none", got.PlayedNeverSaved)
	}
	found := false
	for _, e := range got.DormantFollows {
		if e.ArtistID == "art-x" {
			found = true
		}
	}
	if !found {
		t.Error("art-x has no play in this range and should read as dormant")
	}
}
