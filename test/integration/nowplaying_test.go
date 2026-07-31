//go:build integration

package integration

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/RequiDev/encore/internal/domain"
	"github.com/RequiDev/encore/test/harness"
)

// playingTrack is one healthy observation, so each test varies only what it is
// about.
func playingTrack(at time.Time) domain.NowPlaying {
	progress, duration := 161000, 255000
	return domain.NowPlaying{
		ObservedAt: at,
		State:      domain.PlaybackPlaying,
		Kind:       domain.PlaybackItemTrack,
		TrackID:    "spotifytrack00000001",
		Title:      "The Wheel",
		Artist:     "SOHN",
		ProgressMs: &progress,
		DurationMs: &duration,
		DeviceName: "Kitchen speaker",
		CheckedAt:  at,
	}
}

// TestNowPlayingIsAbsentUntilSomethingIsObserved pins the distinction the whole
// feature turns on: never looked is not nothing playing.
//
// Fails when: Get invents a zero-valued row instead of reporting ErrNotFound, or
// RecordFailure inserts a row claiming 'idle' — in either case an account
// nobody has checked would read as one whose player is silent.
func TestNowPlayingIsAbsentUntilSomethingIsObserved(t *testing.T) {
	e := harness.New(t)
	user := e.NewUser("np-absent")

	if _, err := e.Accounts.NowPlaying.Get(e.Ctx(), e.Store.DB(), user.ID); err == nil {
		t.Fatal("Get returned a row for an account that has never been checked")
	}

	at := time.Date(2026, time.July, 31, 9, 0, 0, 0, time.UTC)
	if err := e.Accounts.NowPlaying.RecordFailure(e.Ctx(), e.Store.DB(), user.ID, at); err != nil {
		t.Fatalf("RecordFailure: %v", err)
	}

	got, err := e.Accounts.NowPlaying.Get(e.Ctx(), e.Store.DB(), user.ID)
	if err != nil {
		t.Fatalf("Get after a failure: %v", err)
	}
	if got.Observed() {
		t.Fatalf("Observed() = true after nothing but a failed check: %+v", got)
	}
	if got.State != domain.PlaybackUnknown {
		t.Errorf("State = %q, want %q", got.State, domain.PlaybackUnknown)
	}
	if got.Kind != domain.PlaybackItemNone {
		t.Errorf("Kind = %q, want %q", got.Kind, domain.PlaybackItemNone)
	}
	if !got.Failed {
		t.Error("Failed = false after a failed check")
	}
	if !got.CheckedAt.Equal(at) {
		t.Errorf("CheckedAt = %v, want %v", got.CheckedAt, at)
	}
}

// TestRecordRoundTripsEveryColumn pins that nothing is dropped between the
// classifier and the card.
//
// Fails when: a column is added to the INSERT and not to the SELECT, or the two
// disagree about order — the scan then lands progress in duration, and a
// listener sees "4:15 of 2:41".
func TestRecordRoundTripsEveryColumn(t *testing.T) {
	e := harness.New(t)
	user := e.NewUser("np-roundtrip")
	at := time.Date(2026, time.July, 31, 9, 30, 0, 0, time.UTC)

	want := playingTrack(at)
	if err := e.Accounts.NowPlaying.Record(e.Ctx(), e.Store.DB(), user.ID, want); err != nil {
		t.Fatalf("Record: %v", err)
	}

	got, err := e.Accounts.NowPlaying.Get(e.Ctx(), e.Store.DB(), user.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !got.Observed() {
		t.Fatal("Observed() = false after a successful record")
	}
	if got.State != domain.PlaybackPlaying || got.Kind != domain.PlaybackItemTrack {
		t.Errorf("State/Kind = %q/%q, want playing/track", got.State, got.Kind)
	}
	if got.Title != "The Wheel" || got.Artist != "SOHN" {
		t.Errorf("Title/Artist = %q/%q, want The Wheel/SOHN", got.Title, got.Artist)
	}
	if got.TrackID != "spotifytrack00000001" {
		t.Errorf("TrackID = %q", got.TrackID)
	}
	if got.ProgressMs == nil || *got.ProgressMs != 161000 {
		t.Errorf("ProgressMs = %v, want 161000", got.ProgressMs)
	}
	if got.DurationMs == nil || *got.DurationMs != 255000 {
		t.Errorf("DurationMs = %v, want 255000", got.DurationMs)
	}
	if got.DeviceName != "Kitchen speaker" {
		t.Errorf("DeviceName = %q", got.DeviceName)
	}
	if got.Failed {
		t.Error("Failed = true after a successful record")
	}
	if !got.ObservedAt.Equal(at) || !got.CheckedAt.Equal(at) {
		t.Errorf("ObservedAt/CheckedAt = %v/%v, want both %v", got.ObservedAt, got.CheckedAt, at)
	}
}

// TestAFailureKeepsTheLastObservation pins that a failed check does not erase
// what Encore already knew.
//
// This is what lets the card say "the last check failed; this is what you were
// playing four minutes ago" instead of falling back to "we do not know", which
// would throw away a true thing because a later request went wrong.
//
// Fails when: RecordFailure is written as a full upsert that resets the
// observation columns to their defaults — Observed() then goes false and the
// title is gone.
func TestAFailureKeepsTheLastObservation(t *testing.T) {
	e := harness.New(t)
	user := e.NewUser("np-stale")
	observed := time.Date(2026, time.July, 31, 9, 30, 0, 0, time.UTC)
	failedAt := observed.Add(4 * time.Minute)

	if err := e.Accounts.NowPlaying.Record(e.Ctx(), e.Store.DB(), user.ID, playingTrack(observed)); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if err := e.Accounts.NowPlaying.RecordFailure(e.Ctx(), e.Store.DB(), user.ID, failedAt); err != nil {
		t.Fatalf("RecordFailure: %v", err)
	}

	got, err := e.Accounts.NowPlaying.Get(e.Ctx(), e.Store.DB(), user.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !got.Failed {
		t.Error("Failed = false after a failed check")
	}
	if !got.CheckedAt.Equal(failedAt) {
		t.Errorf("CheckedAt = %v, want %v", got.CheckedAt, failedAt)
	}
	if !got.ObservedAt.Equal(observed) {
		t.Errorf("ObservedAt = %v, want the earlier %v: a failure must not move it",
			got.ObservedAt, observed)
	}
	if got.Title != "The Wheel" {
		t.Errorf("Title = %q, want the last observation to survive a failure", got.Title)
	}
}

// TestAnIdleObservationCannotCarryATitle proves the constraint bites rather than
// merely existing.
//
// A leftover title behind "Nothing is playing." is the exact stale-claim defect
// this phase exists to rule out, and the database is the only layer that can
// refuse it unconditionally.
//
// Fails when: now_playing_nothing_carries_nothing is dropped from the migration
// — the write then succeeds and no test anywhere notices.
func TestAnIdleObservationCannotCarryATitle(t *testing.T) {
	e := harness.New(t)
	user := e.NewUser("np-idle-title")
	at := time.Date(2026, time.July, 31, 9, 30, 0, 0, time.UTC)

	err := e.Accounts.NowPlaying.Record(e.Ctx(), e.Store.DB(), user.ID, domain.NowPlaying{
		ObservedAt: at,
		State:      domain.PlaybackIdle,
		Kind:       domain.PlaybackItemNone,
		Title:      "The Wheel",
		CheckedAt:  at,
	})
	if err == nil {
		t.Fatal("an idle observation carrying a title was accepted")
	}
}

// TestNowPlayingTextIsTruncatedRuneSafely pins the bound on the three text
// columns Spotify's own strings reach.
//
// Fails when: store.Truncate is replaced by a plain slice — the multi-byte runes
// below then cut mid-rune and PostgreSQL rejects the write outright, so Record
// returns an error instead of storing a bounded string.
func TestNowPlayingTextIsTruncatedRuneSafely(t *testing.T) {
	e := harness.New(t)
	user := e.NewUser("np-truncate")
	at := time.Date(2026, time.July, 31, 9, 30, 0, 0, time.UTC)

	// A one-byte prefix followed by 200 repeats of an 8-byte rune group: past
	// the limit, and — because 200 is a multiple of 8 — a plain byte slice at
	// exactly 200 bytes would land squarely on a rune boundary without the
	// prefix, making a naive cut indistinguishable from a rune-safe one (see
	// TestSetCoverTruncatesTheReason in playlistcover_test.go, which hits the
	// same arithmetic against the same limit). The single leading ASCII byte
	// shifts every later boundary by one, so byte offset 200 falls inside the
	// third byte of a rune instead of before it.
	long := "x" + strings.Repeat("é—中", 200)
	obs := playingTrack(at)
	obs.Title, obs.Artist, obs.DeviceName = long, long, long

	if err := e.Accounts.NowPlaying.Record(e.Ctx(), e.Store.DB(), user.ID, obs); err != nil {
		t.Fatalf("Record: %v", err)
	}
	got, err := e.Accounts.NowPlaying.Get(e.Ctx(), e.Store.DB(), user.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	for name, value := range map[string]string{
		"Title": got.Title, "Artist": got.Artist, "DeviceName": got.DeviceName,
	} {
		if len(value) > 256 {
			t.Errorf("%s is %d bytes, want it bounded", name, len(value))
		}
		if !utf8.ValidString(value) {
			t.Errorf("%s is not valid UTF-8", name)
		}
	}
}

// TestTrackKnownIsFalseForATrackTheCatalogueHasNeverSeen pins the join that
// decides whether the title is a link.
//
// Spotify names a track the instant it starts playing; Encore's catalogue
// learns about it when enrichment gets round to it. Linking before then would
// be a dead link wearing a working one's clothes.
//
// Fails when: the LEFT JOIN is dropped and TrackKnown is hard-coded to
// TrackID != "" — the assertion below then reports true for a track nothing
// holds.
func TestTrackKnownIsFalseForATrackTheCatalogueHasNeverSeen(t *testing.T) {
	e := harness.New(t)
	user := e.NewUser("np-unknown-track")
	at := time.Date(2026, time.July, 31, 9, 30, 0, 0, time.UTC)

	if err := e.Accounts.NowPlaying.Record(e.Ctx(), e.Store.DB(), user.ID, playingTrack(at)); err != nil {
		t.Fatalf("Record: %v", err)
	}
	got, err := e.Accounts.NowPlaying.Get(e.Ctx(), e.Store.DB(), user.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.TrackKnown {
		t.Fatal("TrackKnown = true for a track the catalogue has never seen")
	}
}

// TestDeletingAUserRemovesTheirNowPlayingRow pins the cascade, which is also
// what lets the integration harness leave now_playing out of truncatedTables.
//
// Fails when: the REFERENCES clause loses ON DELETE CASCADE — DeleteUser then
// fails outright on a foreign-key violation, which is a louder failure than
// this test but a failure the test still names.
func TestDeletingAUserRemovesTheirNowPlayingRow(t *testing.T) {
	e := harness.New(t)
	user := e.NewUser("np-cascade")
	at := time.Date(2026, time.July, 31, 9, 30, 0, 0, time.UTC)

	if err := e.Accounts.NowPlaying.Record(e.Ctx(), e.Store.DB(), user.ID, playingTrack(at)); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if err := e.Accounts.Users.DeleteUser(e.Ctx(), e.Store.DB(), user.ID); err != nil {
		t.Fatalf("DeleteUser: %v", err)
	}

	var count int
	row := e.Store.DB().QueryRow(e.Ctx(), `SELECT count(*) FROM now_playing WHERE user_id = $1`, user.ID)
	if err := row.Scan(&count); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if count != 0 {
		t.Fatalf("%d now_playing rows survived the account's deletion", count)
	}
}
