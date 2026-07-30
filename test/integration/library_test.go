//go:build integration

package integration

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/RequiDev/encore/internal/store/library"
	"github.com/RequiDev/encore/test/harness"
)

// The tests for reconciling Spotify's library enumeration against what Encore
// has stored.
//
// Spotify has no "what changed" feed for saved tracks, saved albums or
// followed artists — only a full listing — so every sync reconciles the whole
// set: an incoming id is upserted, and a stored id the incoming set no longer
// contains is deleted. The headline risk is a naive upsert-only
// implementation that never deletes anything, so the tests below deliberately
// shrink a library and check that the dropped id is actually gone, not just
// that the kept ones are still there.

// libraryBase is a fixed added_at so a re-replace with the same items can
// assert the stored value did not move.
var libraryBase = time.Date(2024, time.January, 1, 12, 0, 0, 0, time.UTC)

// savedItems builds a SavedItem for each id, with added_at spaced an hour
// apart from libraryBase so the ids do not share a timestamp.
func savedItems(ids ...string) []library.SavedItem {
	items := make([]library.SavedItem, len(ids))
	for i, id := range ids {
		at := libraryBase.Add(time.Duration(i) * time.Hour)
		items[i] = library.SavedItem{ID: id, AddedAt: &at}
	}
	return items
}

// TestReplaceSavedTracksInsertsIntoEmptyLibrary is the base case: nothing
// stored, three ids offered, three rows land.
func TestLibraryReplaceSavedTracksInsertsIntoEmptyLibrary(t *testing.T) {
	env := harness.New(t)
	user := env.NewUser("tracks-insert")
	repo := library.New(env.Store)

	if err := repo.ReplaceSavedTracks(env.Ctx(), env.Store.DB(), user.ID,
		savedItems("trk1", "trk2", "trk3")); err != nil {
		t.Fatalf("replace saved tracks: %v", err)
	}

	if got := env.ScalarInt(
		`SELECT count(*) FROM user_saved_tracks WHERE user_id = $1`, user.ID.String()); got != 3 {
		t.Fatalf("%d saved tracks after replacing an empty library with three, want 3", got)
	}
}

// TestReplacingSavedTracksDeletesTheAbsentID is the whole point of
// reconciliation: an id present in the first replace and missing from the
// second must not survive the second.
func TestLibraryReplacingSavedTracksDeletesTheAbsentID(t *testing.T) {
	env := harness.New(t)
	user := env.NewUser("tracks-shrink")
	repo := library.New(env.Store)

	if err := repo.ReplaceSavedTracks(env.Ctx(), env.Store.DB(), user.ID,
		savedItems("trk1", "trk2", "trk3")); err != nil {
		t.Fatalf("first replace: %v", err)
	}
	if err := repo.ReplaceSavedTracks(env.Ctx(), env.Store.DB(), user.ID,
		savedItems("trk1", "trk2")); err != nil {
		t.Fatalf("second replace: %v", err)
	}

	if got := env.ScalarInt(
		`SELECT count(*) FROM user_saved_tracks WHERE user_id = $1`, user.ID.String()); got != 2 {
		t.Fatalf("%d saved tracks after dropping one of three, want 2", got)
	}
	if got := env.ScalarInt(
		`SELECT count(*) FROM user_saved_tracks WHERE user_id = $1 AND track_id = 'trk3'`,
		user.ID.String()); got != 0 {
		t.Fatal("trk3 survived even though the second replace omitted it — a naive " +
			"upsert-only implementation would get this wrong")
	}
}

// TestReplacingSavedTracksTwiceIsIdempotent: replaying the same set changes
// nothing, including the added_at each row already carried.
func TestLibraryReplacingSavedTracksTwiceIsIdempotent(t *testing.T) {
	env := harness.New(t)
	user := env.NewUser("tracks-idempotent")
	repo := library.New(env.Store)
	items := savedItems("trk1", "trk2")

	if err := repo.ReplaceSavedTracks(env.Ctx(), env.Store.DB(), user.ID, items); err != nil {
		t.Fatalf("first replace: %v", err)
	}
	if err := repo.ReplaceSavedTracks(env.Ctx(), env.Store.DB(), user.ID, items); err != nil {
		t.Fatalf("second replace: %v", err)
	}

	if got := env.ScalarInt(
		`SELECT count(*) FROM user_saved_tracks WHERE user_id = $1`, user.ID.String()); got != 2 {
		t.Fatalf("%d saved tracks after replacing the same set twice, want 2", got)
	}
	var addedAt time.Time
	if err := env.Pool.QueryRow(env.Ctx(),
		`SELECT added_at FROM user_saved_tracks WHERE user_id = $1 AND track_id = 'trk1'`,
		user.ID.String()).Scan(&addedAt); err != nil {
		t.Fatalf("read added_at: %v", err)
	}
	if want := *items[0].AddedAt; !addedAt.Equal(want) {
		t.Fatalf("added_at is %v after a second identical replace, want %v", addedAt, want)
	}
}

// TestReplacingSavedTracksWithEmptyRemovesEverything: an emptied library is a
// real state, not "no data" — that distinction belongs to
// spotify_credentials.library_synced_at, not to whether these rows exist.
func TestLibraryReplacingSavedTracksWithEmptyRemovesEverything(t *testing.T) {
	env := harness.New(t)
	user := env.NewUser("tracks-empty")
	repo := library.New(env.Store)

	if err := repo.ReplaceSavedTracks(env.Ctx(), env.Store.DB(), user.ID,
		savedItems("trk1", "trk2")); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := repo.ReplaceSavedTracks(env.Ctx(), env.Store.DB(), user.ID, nil); err != nil {
		t.Fatalf("replace with empty: %v", err)
	}

	if got := env.ScalarInt(
		`SELECT count(*) FROM user_saved_tracks WHERE user_id = $1`, user.ID.String()); got != 0 {
		t.Fatalf("%d saved tracks survived a replace with an empty set, want 0", got)
	}
}

// TestReplaceSavedAlbumsDeletesTheAbsentID mirrors the tracks case for the
// albums table, so the same reconciliation is proven on both statements.
func TestLibraryReplaceSavedAlbumsDeletesTheAbsentID(t *testing.T) {
	env := harness.New(t)
	user := env.NewUser("albums-shrink")
	repo := library.New(env.Store)

	if err := repo.ReplaceSavedAlbums(env.Ctx(), env.Store.DB(), user.ID,
		savedItems("alb1", "alb2", "alb3")); err != nil {
		t.Fatalf("first replace: %v", err)
	}
	if err := repo.ReplaceSavedAlbums(env.Ctx(), env.Store.DB(), user.ID,
		savedItems("alb1", "alb2")); err != nil {
		t.Fatalf("second replace: %v", err)
	}

	if got := env.ScalarInt(
		`SELECT count(*) FROM user_saved_albums WHERE user_id = $1`, user.ID.String()); got != 2 {
		t.Fatalf("%d saved albums after dropping one of three, want 2", got)
	}
	if got := env.ScalarInt(
		`SELECT count(*) FROM user_saved_albums WHERE user_id = $1 AND album_id = 'alb3'`,
		user.ID.String()); got != 0 {
		t.Fatal("alb3 survived even though the second replace omitted it")
	}
}

// TestReplaceFollowedArtistsDeletesTheAbsentID mirrors the same reconciliation
// for followed artists, which carry no added_at of their own.
func TestLibraryReplaceFollowedArtistsDeletesTheAbsentID(t *testing.T) {
	env := harness.New(t)
	user := env.NewUser("artists-shrink")
	repo := library.New(env.Store)

	if err := repo.ReplaceFollowedArtists(env.Ctx(), env.Store.DB(), user.ID,
		[]string{"art1", "art2", "art3"}); err != nil {
		t.Fatalf("first replace: %v", err)
	}
	if err := repo.ReplaceFollowedArtists(env.Ctx(), env.Store.DB(), user.ID,
		[]string{"art1", "art2"}); err != nil {
		t.Fatalf("second replace: %v", err)
	}

	if got := env.ScalarInt(
		`SELECT count(*) FROM user_followed_artists WHERE user_id = $1`, user.ID.String()); got != 2 {
		t.Fatalf("%d followed artists after dropping one of three, want 2", got)
	}
	if got := env.ScalarInt(
		`SELECT count(*) FROM user_followed_artists WHERE user_id = $1 AND artist_id = 'art3'`,
		user.ID.String()); got != 0 {
		t.Fatal("art3 survived even though the second replace omitted it")
	}

	// Idempotent: replaying the same two ids again must not error, even though
	// the table has no added_at for ON CONFLICT to update.
	if err := repo.ReplaceFollowedArtists(env.Ctx(), env.Store.DB(), user.ID,
		[]string{"art1", "art2"}); err != nil {
		t.Fatalf("third replace (idempotent): %v", err)
	}
	if got := env.ScalarInt(
		`SELECT count(*) FROM user_followed_artists WHERE user_id = $1`, user.ID.String()); got != 2 {
		t.Fatalf("%d followed artists after replaying the same set, want 2", got)
	}
}

// TestFailureInsideTheTransactionCommitsNothing proves the Querier threading:
// a successful ReplaceSavedTracks followed by an error from the same
// Store.InTx callback must leave the table exactly as it was before the
// transaction started. This is what lets the worker run all three
// replacements and the watermark update as one atomic unit.
func TestLibraryFailureInsideTheTransactionCommitsNothing(t *testing.T) {
	env := harness.New(t)
	user := env.NewUser("tx-failure")
	repo := library.New(env.Store)

	boom := errors.New("boom")
	err := env.Store.InTx(env.Ctx(), func(ctx context.Context, tx pgx.Tx) error {
		if err := repo.ReplaceSavedTracks(ctx, tx, user.ID,
			savedItems("trk1", "trk2", "trk3")); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("InTx returned %v, want the callback's own error", err)
	}

	if got := env.ScalarInt(
		`SELECT count(*) FROM user_saved_tracks WHERE user_id = $1`, user.ID.String()); got != 0 {
		t.Fatalf("%d saved tracks survived a transaction that returned an error, want 0 — "+
			"the write must not have committed", got)
	}
}

// TestCountsIsZeroForANewUser: a user nobody has synced yet reads as zero
// everywhere, not an error.
func TestLibraryCountsIsZeroForANewUser(t *testing.T) {
	env := harness.New(t)
	user := env.NewUser("counts-empty")
	repo := library.New(env.Store)

	got, err := repo.Counts(env.Ctx(), env.Store.DB(), user.ID)
	if err != nil {
		t.Fatalf("counts: %v", err)
	}
	if want := (library.Counts{}); got != want {
		t.Fatalf("counts = %+v for a user with no rows, want all zero", got)
	}
}

// TestCountsReflectsAllThreeTables checks the non-zero case across all three
// tables at once, since a caller wanting one invariably wants the others.
func TestLibraryCountsReflectsAllThreeTables(t *testing.T) {
	env := harness.New(t)
	user := env.NewUser("counts-full")
	repo := library.New(env.Store)

	if err := repo.ReplaceSavedTracks(env.Ctx(), env.Store.DB(), user.ID,
		savedItems("trk1", "trk2", "trk3")); err != nil {
		t.Fatalf("replace saved tracks: %v", err)
	}
	if err := repo.ReplaceSavedAlbums(env.Ctx(), env.Store.DB(), user.ID,
		savedItems("alb1", "alb2")); err != nil {
		t.Fatalf("replace saved albums: %v", err)
	}
	if err := repo.ReplaceFollowedArtists(env.Ctx(), env.Store.DB(), user.ID,
		[]string{"art1", "art2", "art3", "art4"}); err != nil {
		t.Fatalf("replace followed artists: %v", err)
	}

	got, err := repo.Counts(env.Ctx(), env.Store.DB(), user.ID)
	if err != nil {
		t.Fatalf("counts: %v", err)
	}
	if want := (library.Counts{SavedTracks: 3, SavedAlbums: 2, FollowedArtists: 4}); got != want {
		t.Fatalf("counts = %+v, want %+v", got, want)
	}
}

// TestDeletingUserCascadesLibraryTables: the foreign keys are ON DELETE
// CASCADE, so removing a user must not leave any of the three tables holding
// an orphaned row behind.
func TestLibraryDeletingUserCascadesAllThreeTables(t *testing.T) {
	env := harness.New(t)
	user := env.NewUser("cascade")
	repo := library.New(env.Store)

	if err := repo.ReplaceSavedTracks(env.Ctx(), env.Store.DB(), user.ID, savedItems("trk1")); err != nil {
		t.Fatalf("replace saved tracks: %v", err)
	}
	if err := repo.ReplaceSavedAlbums(env.Ctx(), env.Store.DB(), user.ID, savedItems("alb1")); err != nil {
		t.Fatalf("replace saved albums: %v", err)
	}
	if err := repo.ReplaceFollowedArtists(env.Ctx(), env.Store.DB(), user.ID, []string{"art1"}); err != nil {
		t.Fatalf("replace followed artists: %v", err)
	}

	env.Exec(`DELETE FROM users WHERE id = $1`, user.ID.String())

	if got := env.ScalarInt(
		`SELECT count(*) FROM user_saved_tracks WHERE user_id = $1`, user.ID.String()); got != 0 {
		t.Fatalf("%d saved tracks survived the user's deletion, want 0", got)
	}
	if got := env.ScalarInt(
		`SELECT count(*) FROM user_saved_albums WHERE user_id = $1`, user.ID.String()); got != 0 {
		t.Fatalf("%d saved albums survived the user's deletion, want 0", got)
	}
	if got := env.ScalarInt(
		`SELECT count(*) FROM user_followed_artists WHERE user_id = $1`, user.ID.String()); got != 0 {
		t.Fatalf("%d followed artists survived the user's deletion, want 0", got)
	}
}

// playlistItems builds a UserPlaylistRow for each id, with distinct detail
// per row so a test can tell rows apart by more than just id.
func playlistItems(ids ...string) []library.UserPlaylistRow {
	items := make([]library.UserPlaylistRow, len(ids))
	for i, id := range ids {
		items[i] = library.UserPlaylistRow{
			ID:          id,
			Name:        "Playlist " + id,
			OwnerID:     "owner-" + id,
			SnapshotID:  "snap-" + id,
			TotalTracks: i + 1,
		}
	}
	return items
}

// playlistFetchedAt is a fixed instant every playlist test writes with, so a
// re-replace can assert the stored value moved (or did not) deliberately
// rather than by accident of wall-clock time.
var playlistFetchedAt = time.Date(2025, time.June, 1, 9, 0, 0, 0, time.UTC)

// TestLibraryReplaceUserPlaylistsInsertsIntoEmptySet is the base case: nothing
// stored, three playlists offered, three rows land with their full detail.
func TestLibraryReplaceUserPlaylistsInsertsIntoEmptySet(t *testing.T) {
	env := harness.New(t)
	user := env.NewUser("playlists-insert")
	repo := library.New(env.Store)

	if err := repo.ReplaceUserPlaylists(env.Ctx(), env.Store.DB(), user.ID,
		playlistItems("pl1", "pl2", "pl3"), playlistFetchedAt); err != nil {
		t.Fatalf("replace user playlists: %v", err)
	}

	if got := env.ScalarInt(
		`SELECT count(*) FROM user_playlists WHERE user_id = $1`, user.ID.String()); got != 3 {
		t.Fatalf("%d playlists after replacing an empty set with three, want 3", got)
	}

	var name, ownerID, snapshotID string
	var totalTracks int
	var fetchedAt time.Time
	if err := env.Pool.QueryRow(env.Ctx(),
		`SELECT name, owner_id, snapshot_id, total_tracks, fetched_at
		 FROM user_playlists WHERE user_id = $1 AND playlist_id = 'pl1'`,
		user.ID.String()).Scan(&name, &ownerID, &snapshotID, &totalTracks, &fetchedAt); err != nil {
		t.Fatalf("read playlist row: %v", err)
	}
	if name != "Playlist pl1" || ownerID != "owner-pl1" || snapshotID != "snap-pl1" || totalTracks != 1 {
		t.Fatalf("playlist row = (%q, %q, %q, %d), want (%q, %q, %q, %d)",
			name, ownerID, snapshotID, totalTracks, "Playlist pl1", "owner-pl1", "snap-pl1", 1)
	}
	if !fetchedAt.Equal(playlistFetchedAt) {
		t.Fatalf("fetched_at = %v, want %v", fetchedAt, playlistFetchedAt)
	}
}

// TestLibraryReplacingUserPlaylistsDeletesTheAbsentID is the delete-absent
// property the brief calls out specifically: a playlist present in the first
// replace and missing from the second must not survive the second, or a
// listener who deletes or leaves a playlist would see it linger forever.
func TestLibraryReplacingUserPlaylistsDeletesTheAbsentID(t *testing.T) {
	env := harness.New(t)
	user := env.NewUser("playlists-shrink")
	repo := library.New(env.Store)

	if err := repo.ReplaceUserPlaylists(env.Ctx(), env.Store.DB(), user.ID,
		playlistItems("pl1", "pl2", "pl3"), playlistFetchedAt); err != nil {
		t.Fatalf("first replace: %v", err)
	}
	if err := repo.ReplaceUserPlaylists(env.Ctx(), env.Store.DB(), user.ID,
		playlistItems("pl1", "pl2"), playlistFetchedAt.Add(time.Hour)); err != nil {
		t.Fatalf("second replace: %v", err)
	}

	if got := env.ScalarInt(
		`SELECT count(*) FROM user_playlists WHERE user_id = $1`, user.ID.String()); got != 2 {
		t.Fatalf("%d playlists after dropping one of three, want 2", got)
	}
	if got := env.ScalarInt(
		`SELECT count(*) FROM user_playlists WHERE user_id = $1 AND playlist_id = 'pl3'`,
		user.ID.String()); got != 0 {
		t.Fatal("pl3 survived even though the second replace omitted it — a naive " +
			"upsert-only implementation would get this wrong")
	}
}

// TestLibraryReplacingUserPlaylistsRefreshesEveryColumnOnConflict proves that,
// unlike ReplaceFollowedArtists' DO NOTHING, a playlist already known gets
// every one of its columns refreshed — a rename, a total-track-count change
// and a new snapshot_id all take effect on the row that already existed
// rather than silently keeping the first values ever written.
func TestLibraryReplacingUserPlaylistsRefreshesEveryColumnOnConflict(t *testing.T) {
	env := harness.New(t)
	user := env.NewUser("playlists-refresh")
	repo := library.New(env.Store)

	if err := repo.ReplaceUserPlaylists(env.Ctx(), env.Store.DB(), user.ID, []library.UserPlaylistRow{
		{ID: "pl1", Name: "Old Name", OwnerID: "owner-old", SnapshotID: "snap-old", TotalTracks: 5},
	}, playlistFetchedAt); err != nil {
		t.Fatalf("first replace: %v", err)
	}
	second := playlistFetchedAt.Add(24 * time.Hour)
	if err := repo.ReplaceUserPlaylists(env.Ctx(), env.Store.DB(), user.ID, []library.UserPlaylistRow{
		{ID: "pl1", Name: "New Name", OwnerID: "owner-new", SnapshotID: "snap-new", TotalTracks: 9},
	}, second); err != nil {
		t.Fatalf("second replace: %v", err)
	}

	var name, ownerID, snapshotID string
	var totalTracks int
	var fetchedAt time.Time
	if err := env.Pool.QueryRow(env.Ctx(),
		`SELECT name, owner_id, snapshot_id, total_tracks, fetched_at
		 FROM user_playlists WHERE user_id = $1 AND playlist_id = 'pl1'`,
		user.ID.String()).Scan(&name, &ownerID, &snapshotID, &totalTracks, &fetchedAt); err != nil {
		t.Fatalf("read playlist row: %v", err)
	}
	if name != "New Name" || ownerID != "owner-new" || snapshotID != "snap-new" || totalTracks != 9 {
		t.Fatalf("playlist row after refresh = (%q, %q, %q, %d), want (%q, %q, %q, %d)",
			name, ownerID, snapshotID, totalTracks, "New Name", "owner-new", "snap-new", 9)
	}
	if !fetchedAt.Equal(second) {
		t.Fatalf("fetched_at = %v, want %v", fetchedAt, second)
	}
}

// TestLibraryReplacingUserPlaylistsWithEmptyRemovesEverything: a listener who
// deleted or left every playlist they had is a real, representable state,
// distinct from one who has never had this enumerated at all.
func TestLibraryReplacingUserPlaylistsWithEmptyRemovesEverything(t *testing.T) {
	env := harness.New(t)
	user := env.NewUser("playlists-empty")
	repo := library.New(env.Store)

	if err := repo.ReplaceUserPlaylists(env.Ctx(), env.Store.DB(), user.ID,
		playlistItems("pl1", "pl2"), playlistFetchedAt); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := repo.ReplaceUserPlaylists(env.Ctx(), env.Store.DB(), user.ID, nil, playlistFetchedAt.Add(time.Hour)); err != nil {
		t.Fatalf("replace with empty: %v", err)
	}

	if got := env.ScalarInt(
		`SELECT count(*) FROM user_playlists WHERE user_id = $1`, user.ID.String()); got != 0 {
		t.Fatalf("%d playlists survived a replace with an empty set, want 0", got)
	}
}

// TestLibraryDeletingUserCascadesUserPlaylists mirrors
// TestLibraryDeletingUserCascadesAllThreeTables for the fourth table this
// package now reconciles.
func TestLibraryDeletingUserCascadesUserPlaylists(t *testing.T) {
	env := harness.New(t)
	user := env.NewUser("playlists-cascade")
	repo := library.New(env.Store)

	if err := repo.ReplaceUserPlaylists(env.Ctx(), env.Store.DB(), user.ID,
		playlistItems("pl1"), playlistFetchedAt); err != nil {
		t.Fatalf("replace user playlists: %v", err)
	}

	env.Exec(`DELETE FROM users WHERE id = $1`, user.ID.String())

	if got := env.ScalarInt(
		`SELECT count(*) FROM user_playlists WHERE user_id = $1`, user.ID.String()); got != 0 {
		t.Fatalf("%d playlists survived the user's deletion, want 0", got)
	}
}

// TestLibraryTwoUsersPlaylistsDoNotInterfere guards the scoping in the DELETE
// half of the reconciliation statement: without "WHERE user_id = $1" a shrink
// for one user would also erase a playlist id another user happens to share
// (a household following the same public playlist, for instance).
func TestLibraryTwoUsersPlaylistsDoNotInterfere(t *testing.T) {
	env := harness.New(t)
	a := env.NewUser("playlistuser-a")
	b := env.NewUser("playlistuser-b")
	repo := library.New(env.Store)

	if err := repo.ReplaceUserPlaylists(env.Ctx(), env.Store.DB(), a.ID,
		playlistItems("pl1", "pl2"), playlistFetchedAt); err != nil {
		t.Fatalf("replace for a: %v", err)
	}
	if err := repo.ReplaceUserPlaylists(env.Ctx(), env.Store.DB(), b.ID,
		playlistItems("pl1"), playlistFetchedAt); err != nil {
		t.Fatalf("replace for b: %v", err)
	}

	if err := repo.ReplaceUserPlaylists(env.Ctx(), env.Store.DB(), a.ID,
		playlistItems("pl2"), playlistFetchedAt.Add(time.Hour)); err != nil {
		t.Fatalf("shrink a: %v", err)
	}

	if got := env.ScalarInt(
		`SELECT count(*) FROM user_playlists WHERE user_id = $1`, a.ID.String()); got != 1 {
		t.Fatalf("user a has %d playlists, want 1", got)
	}
	if got := env.ScalarInt(
		`SELECT count(*) FROM user_playlists WHERE user_id = $1 AND playlist_id = 'pl1'`,
		b.ID.String()); got != 1 {
		t.Fatal("user a's replace deleted user b's playlist sharing the same playlist id")
	}
}

// TestTwoUsersLibrariesDoNotInterfere guards the scoping in the DELETE half of
// the reconciliation statement: without "WHERE user_id = $1" a shrink for one
// user would also erase a track id another user happens to share.
func TestLibraryTwoUsersDoNotInterfere(t *testing.T) {
	env := harness.New(t)
	a := env.NewUser("libuser-a")
	b := env.NewUser("libuser-b")
	repo := library.New(env.Store)

	if err := repo.ReplaceSavedTracks(env.Ctx(), env.Store.DB(), a.ID,
		savedItems("trk1", "trk2")); err != nil {
		t.Fatalf("replace for a: %v", err)
	}
	if err := repo.ReplaceSavedTracks(env.Ctx(), env.Store.DB(), b.ID,
		savedItems("trk1")); err != nil {
		t.Fatalf("replace for b: %v", err)
	}

	// Shrinking a's library to omit trk1 must not touch b's row for the same
	// track id, because the delete is scoped by user, not by track alone.
	if err := repo.ReplaceSavedTracks(env.Ctx(), env.Store.DB(), a.ID,
		savedItems("trk2")); err != nil {
		t.Fatalf("shrink a: %v", err)
	}

	if got := env.ScalarInt(
		`SELECT count(*) FROM user_saved_tracks WHERE user_id = $1`, a.ID.String()); got != 1 {
		t.Fatalf("user a has %d saved tracks, want 1", got)
	}
	if got := env.ScalarInt(
		`SELECT count(*) FROM user_saved_tracks WHERE user_id = $1 AND track_id = 'trk1'`,
		b.ID.String()); got != 1 {
		t.Fatal("user a's replace deleted user b's saved track sharing the same track id")
	}
}
