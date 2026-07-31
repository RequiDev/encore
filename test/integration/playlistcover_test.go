//go:build integration

package integration

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/RequiDev/encore/internal/domain"
	"github.com/RequiDev/encore/internal/playlistcover"
	"github.com/RequiDev/encore/internal/stats"
	"github.com/RequiDev/encore/test/harness"
)

// newCoverPlaylist inserts one managed playlist and returns it.
func newCoverPlaylist(t *testing.T, e *harness.Env, userID uuid.UUID, name string) domain.Playlist {
	t.Helper()
	p, err := e.Accounts.Playlists.Create(e.Ctx(), e.Store.DB(), domain.Playlist{
		UserID:     userID,
		Name:       name,
		SpotifyID:  "spotifyplaylist000001",
		SpotifyURL: "https://open.spotify.com/playlist/spotifyplaylist000001",
		Definition: domain.PlaylistDefinition{
			Mode: domain.PlaylistModeTop, Sort: domain.SortByPlays, Limit: 100,
		},
		TrackCount: 10,
		BuiltAt:    time.Date(2026, time.July, 31, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("create playlist: %v", err)
	}
	return p
}

// TestPlaylistCoverDefaultsToNone pins that a playlist made before covers
// existed reads back as "never attempted" rather than as a failure.
//
// Fails when: the migration's DEFAULT is dropped, or scanPlaylist reads the new
// columns in the wrong positional order (cover_tiles would land in cover_state
// and the scan errors).
func TestPlaylistCoverDefaultsToNone(t *testing.T) {
	e := harness.New(t)
	user := e.NewUser("cover-default")
	p := newCoverPlaylist(t, e, user.ID, "Heavy rotation")

	if p.Cover.State != domain.CoverNone {
		t.Fatalf("Cover.State = %q, want %q", p.Cover.State, domain.CoverNone)
	}
	if p.Cover.Tiles != 0 || p.Cover.Error != "" || !p.Cover.At.IsZero() {
		t.Fatalf("Cover = %+v, want a zero cover", p.Cover)
	}
}

// TestSetCoverRoundTrips pins that every field survives a write and a read, and
// that the state is scoped by owner.
//
// Fails when: SetCover drops the user_id predicate (the foreign user's write
// would succeed and RowsAffected would be 1), or playlistColumns and
// scanPlaylist disagree about column order.
func TestSetCoverRoundTrips(t *testing.T) {
	e := harness.New(t)
	user := e.NewUser("cover-roundtrip")
	other := e.NewUser("cover-stranger")
	p := newCoverPlaylist(t, e, user.ID, "Heavy rotation")

	at := time.Date(2026, time.July, 31, 9, 30, 0, 0, time.UTC)
	want := domain.PlaylistCover{State: domain.CoverReady, Tiles: 3, At: at}
	if err := e.Accounts.Playlists.SetCover(e.Ctx(), e.Store.DB(), user.ID, p.ID, want); err != nil {
		t.Fatalf("SetCover: %v", err)
	}

	got, err := e.Accounts.Playlists.Get(e.Ctx(), e.Store.DB(), user.ID, p.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Cover.State != domain.CoverReady || got.Cover.Tiles != 3 {
		t.Fatalf("Cover = %+v, want state ready and 3 tiles", got.Cover)
	}
	if !got.Cover.At.Equal(at) {
		t.Fatalf("Cover.At = %v, want %v", got.Cover.At, at)
	}
	if !got.Cover.Mosaic() {
		t.Fatal("Mosaic() = false for a ready cover with three tiles")
	}

	// Another account cannot write to it.
	err = e.Accounts.Playlists.SetCover(e.Ctx(), e.Store.DB(), other.ID, p.ID,
		domain.PlaylistCover{State: domain.CoverFailed, At: at})
	if err == nil {
		t.Fatal("SetCover: a stranger's write succeeded")
	}
}

// TestSetCoverTruncatesTheReason pins the rune-safe cut on the one text column
// this feature writes.
//
// Fails when: store.Truncate is replaced by a plain slice — the multi-byte
// runes below then cut mid-rune and PostgreSQL rejects the write outright, so
// SetCover returns an error instead of storing a bounded string.
func TestSetCoverTruncatesTheReason(t *testing.T) {
	e := harness.New(t)
	user := e.NewUser("cover-truncate")
	p := newCoverPlaylist(t, e, user.ID, "Heavy rotation")

	// A one-byte prefix followed by 200 repeats of an 8-byte rune group: past
	// the limit, and — because 200 is a multiple of 8 — a plain byte slice at
	// exactly 200 bytes would land squarely on a rune boundary without the
	// prefix, making a naive cut indistinguishable from a rune-safe one. The
	// single leading ASCII byte shifts every later boundary by one, so byte
	// offset 200 falls inside the third byte of a rune instead of before it.
	reason := "x" + strings.Repeat("é—中", 200)
	err := e.Accounts.Playlists.SetCover(e.Ctx(), e.Store.DB(), user.ID, p.ID,
		domain.PlaylistCover{State: domain.CoverFailed, Error: reason, At: time.Now().UTC()})
	if err != nil {
		t.Fatalf("SetCover: %v", err)
	}

	got, err := e.Accounts.Playlists.Get(e.Ctx(), e.Store.DB(), user.ID, p.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	// coverErrorLimit is 200, and Truncate appends "..." (3 bytes) on top of
	// that, so the real ceiling is about 203 bytes. 210 leaves headroom for
	// the rune-boundary backtrack without being loose enough that a
	// regression widening the limit to, say, 250 would still pass.
	if len(got.Cover.Error) > 210 {
		t.Fatalf("stored reason is %d bytes, want it bounded near coverErrorLimit (200)", len(got.Cover.Error))
	}
	if !utf8.ValidString(got.Cover.Error) {
		t.Fatal("stored reason is not valid UTF-8")
	}
}

// TestCoverAtMustMatchState pins playlists_cover_at_matches_state (00016): a
// row claiming cover_state = 'ready' with no timestamp would tell a listener
// an attempt succeeded at an unknown, unrecorded time, which is exactly the
// silent misrepresentation this phase's rules forbid. The write goes straight
// through SQL rather than SetCover, because SetCover always supplies both
// columns together and could never produce this row itself — the constraint
// is what stops a future, buggier caller from doing it instead.
//
// Fails when: migration 00016 is not applied, or its CHECK is dropped — the
// UPDATE below then succeeds instead of being rejected.
func TestCoverAtMustMatchState(t *testing.T) {
	e := harness.New(t)
	user := e.NewUser("cover-consistency")
	p := newCoverPlaylist(t, e, user.ID, "Heavy rotation")

	_, err := e.Store.DB().Exec(e.Ctx(),
		`UPDATE playlists SET cover_state = 'ready', cover_at = NULL WHERE id = $1`, p.ID)
	if err == nil {
		t.Fatal("UPDATE set cover_state = 'ready' with a null cover_at and nothing rejected it")
	}
}

// TestRenameUpdatesTheStoredName pins that Rename writes the name and returns
// the full playlist row, scoped by owner like every other write here.
//
// Fails when: Rename drops the user_id predicate (a stranger's rename would
// succeed), or the RETURNING list is out of step with scanPlaylist so the read
// back after a successful write errors instead of reflecting the new name.
func TestRenameUpdatesTheStoredName(t *testing.T) {
	e := harness.New(t)
	user := e.NewUser("rename-owner")
	other := e.NewUser("rename-stranger")
	p := newCoverPlaylist(t, e, user.ID, "Heavy rotation")

	renamed, err := e.Accounts.Playlists.Rename(e.Ctx(), e.Store.DB(), user.ID, p.ID, "Summer 2026")
	if err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if renamed.Name != "Summer 2026" {
		t.Fatalf("Rename returned name %q, want %q", renamed.Name, "Summer 2026")
	}

	got, err := e.Accounts.Playlists.Get(e.Ctx(), e.Store.DB(), user.ID, p.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Name != "Summer 2026" {
		t.Fatalf("stored name = %q, want %q", got.Name, "Summer 2026")
	}

	// A stranger cannot rename somebody else's playlist.
	if _, err := e.Accounts.Playlists.Rename(e.Ctx(), e.Store.DB(), other.ID, p.ID, "Stolen"); err == nil {
		t.Fatal("Rename: a stranger's write succeeded")
	}
	if got, err := e.Accounts.Playlists.Get(e.Ctx(), e.Store.DB(), user.ID, p.ID); err != nil || got.Name != "Summer 2026" {
		t.Fatalf("stranger's rename attempt changed the name: got %+v, err %v", got, err)
	}
}

// seedCoverCatalogue inserts one album (with the given image url) and however
// many tracks are named, all pointing at it. Only the columns CoverArtURLs's
// join touches: tracks.album_id and albums.image_url.
func seedCoverCatalogue(t *testing.T, e *harness.Env, albumID, imageURL string, trackIDs ...string) {
	t.Helper()
	e.Exec(`INSERT INTO albums (id, image_url) VALUES ($1, $2)`, albumID, imageURL)
	for _, id := range trackIDs {
		e.Exec(`INSERT INTO tracks (id, album_id) VALUES ($1, $2)`, id, albumID)
	}
}

// TestCoverArtURLsPicksTheTopFourAlbums pins the selection and its tie-break.
//
// Six albums contribute tracks to one playlist selection, laid out so that
// sorting by count and sorting by rank alone disagree -- which is what makes
// this fixture actually exercise the two-key ORDER BY rather than passing by
// coincidence on either key on its own:
//
//	X: 1 track  at rank 1          (the earliest track of anyone, but rarest)
//	Y: 3 tracks, earliest at rank 2
//	Z: 3 tracks, earliest at rank 5   -- ties Y on count; loses the tie on rank
//	W: 2 tracks, earliest at rank 8
//	V: 2 tracks, earliest at rank 10  -- ties W on count; loses the tie on rank
//	U: 6 tracks, earliest at rank 12  -- never enriched (image_url is empty)
//
// A plain "sort by rank" would answer X, Y, Z, W (X has the single earliest
// track). A plain "sort by count, arbitrary tie order" could answer either of
// Y/Z first and either of W/V second. Only "count DESC, then min(rank) ASC"
// answers Y, Z, W, V -- which is what is asserted below. U has the highest
// count of anyone and must still not appear, at any position.
//
// The trackIDs slice below *is* the selection order a real playlist definition
// would have produced (best-ranked track first), so its index doubles as the
// rank WITH ORDINALITY reads.
//
// Fails when: the ORDER BY loses its count(*) DESC (X and U would then
// outrank everyone), loses its min(ordinality) tie-break (Z could come back
// ahead of Y, or V ahead of W, and the mosaic would change on every rebuild
// even though the playlist did not), or stops filtering out an empty
// image_url (U takes every slot and the mosaic is silently built from four
// empty tiles).
func TestCoverArtURLsPicksTheTopFourAlbums(t *testing.T) {
	e := harness.New(t)
	svc := stats.New(e.Store)

	seedCoverCatalogue(t, e, "alb-x", "https://img.test/x", "trk-x1")
	seedCoverCatalogue(t, e, "alb-y", "https://img.test/y", "trk-y1", "trk-y2", "trk-y3")
	seedCoverCatalogue(t, e, "alb-z", "https://img.test/z", "trk-z1", "trk-z2", "trk-z3")
	seedCoverCatalogue(t, e, "alb-w", "https://img.test/w", "trk-w1", "trk-w2")
	seedCoverCatalogue(t, e, "alb-v", "https://img.test/v", "trk-v1", "trk-v2")
	// U: six tracks, no artwork. The highest count of any album here, and it
	// must not win a single slot.
	seedCoverCatalogue(t, e, "alb-u", "",
		"trk-u1", "trk-u2", "trk-u3", "trk-u4", "trk-u5", "trk-u6")

	trackIDs := []string{
		"trk-x1",                                                   // rank 1
		"trk-y1",                                                   // rank 2 -- Y's earliest
		"trk-y2",                                                   // rank 3
		"trk-y3",                                                   // rank 4
		"trk-z1",                                                   // rank 5 -- Z's earliest
		"trk-z2",                                                   // rank 6
		"trk-z3",                                                   // rank 7
		"trk-w1",                                                   // rank 8 -- W's earliest
		"trk-w2",                                                   // rank 9
		"trk-v1",                                                   // rank 10 -- V's earliest
		"trk-v2",                                                   // rank 11
		"trk-u1", "trk-u2", "trk-u3", "trk-u4", "trk-u5", "trk-u6", // ranks 12-17
	}
	want := []string{
		"https://img.test/y", "https://img.test/z", "https://img.test/w", "https://img.test/v",
	}

	got, err := svc.CoverArtURLs(e.Ctx(), e.Store.DB(), trackIDs, playlistcover.Tiles)
	if err != nil {
		t.Fatalf("CoverArtURLs: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v (position %d: %q != %q)", got, want, i, got[i], want[i])
		}
	}
}

// TestCoverArtURLsReturnsEmptyOnAFreshCatalogue pins the ordinary
// fresh-instance case: every album pending, no image_url anywhere.
//
// Fails when: the empty-set short circuit is removed and the nil Querier
// panics, or the query returns rows with empty urls instead of none.
func TestCoverArtURLsReturnsEmptyOnAFreshCatalogue(t *testing.T) {
	e := harness.New(t)
	svc := stats.New(e.Store)

	// No ids at all. The nil Querier is the assertion: reaching the database
	// would panic, so a caller that selected nothing (a fresh install, or a
	// definition matching nothing) never touches it.
	got, err := svc.CoverArtURLs(e.Ctx(), nil, nil, playlistcover.Tiles)
	if err != nil {
		t.Fatalf("CoverArtURLs with no ids: %v", err)
	}
	if got == nil || len(got) != 0 {
		t.Fatalf("got %v, want an empty, non-nil slice", got)
	}

	// Real tracks whose albums exist but have not been enriched yet -- the
	// ordinary state of a fresh instance, where every album row is 'pending'
	// and image_url is still the empty default.
	seedCoverCatalogue(t, e, "alb-pending-1", "", "trk-pending-1")
	seedCoverCatalogue(t, e, "alb-pending-2", "", "trk-pending-2")

	got2, err := svc.CoverArtURLs(e.Ctx(), e.Store.DB(),
		[]string{"trk-pending-1", "trk-pending-2"}, playlistcover.Tiles)
	if err != nil {
		t.Fatalf("CoverArtURLs: %v", err)
	}
	if len(got2) != 0 {
		t.Fatalf("got %v, want none: neither album has been enriched", got2)
	}
}
