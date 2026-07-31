//go:build integration

package integration

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/RequiDev/encore/internal/domain"
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
	if len(got.Cover.Error) > 256 {
		t.Fatalf("stored reason is %d bytes, want it bounded", len(got.Cover.Error))
	}
	if !utf8.ValidString(got.Cover.Error) {
		t.Fatal("stored reason is not valid UTF-8")
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
