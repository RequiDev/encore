//go:build integration

package integration

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/RequiDev/encore/internal/store/library"
	"github.com/RequiDev/encore/test/harness"
)

// The tests for persisting a capture of Spotify's own top-artists/top-tracks
// ranking.
//
// Spotify's /me/top endpoints return the listener's whole ranking, not a
// diff, so a refresh reconciles the whole (user, kind, time_range) set —
// exactly the delete-absent-plus-upsert reconciliation library_test.go
// exercises for the library tables. The twist here is that the natural key is
// a position, not an id: a top-50 shrinking to a top-30 must delete positions
// 31-50, or a naive upsert-only implementation leaves a stale tail that
// renders as ranks Spotify no longer reports. The tests below deliberately
// check for the tail's absence, not just that the kept ranks survived.

const (
	kindArtist = "artist"
	kindTrack  = "track"

	rangeShort  = "short_term"
	rangeMedium = "medium_term"
	rangeLong   = "long_term"
)

// topBase is a fixed captured_at so idempotency checks have something stable
// to compare against.
var topBase = time.Date(2024, time.January, 1, 12, 0, 0, 0, time.UTC)

// topIDs builds n distinct ids sharing prefix, numbered 1..n — Spotify never
// repeats an id within one ranking.
func topIDs(prefix string, n int) []string {
	out := make([]string, n)
	for i := 0; i < n; i++ {
		out[i] = fmt.Sprintf("%s%d", prefix, i+1)
	}
	return out
}

// TestTopSnapshotReplaceIntoEmptyInsertsInRankOrder is the base case: nothing
// stored, entity ids offered in rank order, and each lands at the position
// matching its 1-based index in the input — not sorted by id.
func TestTopSnapshotReplaceIntoEmptyInsertsInRankOrder(t *testing.T) {
	env := harness.New(t)
	user := env.NewUser("top-insert")
	repo := library.New(env.Store)

	want := []string{"art3", "art1", "art2"} // deliberately not alphabetical
	if err := repo.ReplaceTopSnapshot(env.Ctx(), env.Store.DB(), user.ID,
		kindArtist, rangeShort, want, topBase); err != nil {
		t.Fatalf("replace: %v", err)
	}

	for i, id := range want {
		pos := env.ScalarInt(
			`SELECT position FROM spotify_top_snapshots
			 WHERE user_id = $1 AND kind = $2 AND time_range = $3 AND entity_id = $4`,
			user.ID.String(), kindArtist, rangeShort, id)
		if pos != int64(i+1) {
			t.Fatalf("id %q landed at position %d, want %d", id, pos, i+1)
		}
	}
	if got := env.ScalarInt(
		`SELECT count(*) FROM spotify_top_snapshots WHERE user_id = $1 AND kind = $2 AND time_range = $3`,
		user.ID.String(), kindArtist, rangeShort); got != 3 {
		t.Fatalf("%d rows after replacing an empty set with three, want 3", got)
	}
}

// TestTopSnapshotReplaceShorterDeletesTheTail is the whole point of this
// store: a top-50 shrinking to a top-30 must not leave ranks 31-50 behind. A
// naive upsert-only implementation passes every other test here and still
// fails this one.
func TestTopSnapshotReplaceShorterDeletesTheTail(t *testing.T) {
	env := harness.New(t)
	user := env.NewUser("top-shrink")
	repo := library.New(env.Store)

	if err := repo.ReplaceTopSnapshot(env.Ctx(), env.Store.DB(), user.ID,
		kindArtist, rangeShort, topIDs("art", 50), topBase); err != nil {
		t.Fatalf("first replace (50): %v", err)
	}
	if err := repo.ReplaceTopSnapshot(env.Ctx(), env.Store.DB(), user.ID,
		kindArtist, rangeShort, topIDs("art", 30), topBase); err != nil {
		t.Fatalf("second replace (30): %v", err)
	}

	if got := env.ScalarInt(
		`SELECT count(*) FROM spotify_top_snapshots WHERE user_id = $1 AND kind = $2 AND time_range = $3`,
		user.ID.String(), kindArtist, rangeShort); got != 30 {
		t.Fatalf("%d rows after shrinking 50 to 30, want exactly 30", got)
	}
	if got := env.ScalarInt(
		`SELECT count(*) FROM spotify_top_snapshots
		 WHERE user_id = $1 AND kind = $2 AND time_range = $3 AND position > 30`,
		user.ID.String(), kindArtist, rangeShort); got != 0 {
		t.Fatal("positions 31-50 survived a shrink from 50 to 30 — the tail must be deleted, " +
			"not merely left unmatched by the new ranking")
	}
}

// TestTopSnapshotReplaceLongerGrowsCorrectly checks the opposite direction: a
// set that grows must add the new tail positions without disturbing the ones
// that were already there.
func TestTopSnapshotReplaceLongerGrowsCorrectly(t *testing.T) {
	env := harness.New(t)
	user := env.NewUser("top-grow")
	repo := library.New(env.Store)

	if err := repo.ReplaceTopSnapshot(env.Ctx(), env.Store.DB(), user.ID,
		kindArtist, rangeShort, topIDs("art", 10), topBase); err != nil {
		t.Fatalf("first replace (10): %v", err)
	}
	if err := repo.ReplaceTopSnapshot(env.Ctx(), env.Store.DB(), user.ID,
		kindArtist, rangeShort, topIDs("art", 20), topBase); err != nil {
		t.Fatalf("second replace (20): %v", err)
	}

	if got := env.ScalarInt(
		`SELECT count(*) FROM spotify_top_snapshots WHERE user_id = $1 AND kind = $2 AND time_range = $3`,
		user.ID.String(), kindArtist, rangeShort); got != 20 {
		t.Fatalf("%d rows after growing 10 to 20, want exactly 20", got)
	}
	for i, id := range topIDs("art", 20) {
		pos := env.ScalarInt(
			`SELECT position FROM spotify_top_snapshots
			 WHERE user_id = $1 AND kind = $2 AND time_range = $3 AND entity_id = $4`,
			user.ID.String(), kindArtist, rangeShort, id)
		if pos != int64(i+1) {
			t.Fatalf("id %q is at position %d after growing to 20, want %d", id, pos, i+1)
		}
	}
}

// TestTopSnapshotReplaceIsIdempotent: replaying the same set and captured_at
// changes nothing, and does not error on the repeated ON CONFLICT.
func TestTopSnapshotReplaceIsIdempotent(t *testing.T) {
	env := harness.New(t)
	user := env.NewUser("top-idempotent")
	repo := library.New(env.Store)
	want := topIDs("art", 5)

	if err := repo.ReplaceTopSnapshot(env.Ctx(), env.Store.DB(), user.ID,
		kindArtist, rangeShort, want, topBase); err != nil {
		t.Fatalf("first replace: %v", err)
	}
	if err := repo.ReplaceTopSnapshot(env.Ctx(), env.Store.DB(), user.ID,
		kindArtist, rangeShort, want, topBase); err != nil {
		t.Fatalf("second replace: %v", err)
	}

	if got := env.ScalarInt(
		`SELECT count(*) FROM spotify_top_snapshots WHERE user_id = $1 AND kind = $2 AND time_range = $3`,
		user.ID.String(), kindArtist, rangeShort); got != 5 {
		t.Fatalf("%d rows after replacing the same set twice, want 5", got)
	}

	got, err := repo.TopSnapshot(env.Ctx(), env.Store.DB(), user.ID, kindArtist, rangeShort)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !slices.Equal(got.EntityIDs, want) {
		t.Fatalf("entity ids = %v after a second identical replace, want %v", got.EntityIDs, want)
	}
	if got.CapturedAt == nil || !got.CapturedAt.Equal(topBase) {
		t.Fatalf("captured_at = %v after a second identical replace, want %v", got.CapturedAt, topBase)
	}
}

// TestTopSnapshotReplaceWithEmptyRemovesEverything: an empty ranking is a
// real state Spotify can return (a brand-new account with no top items yet),
// not "no data" — replacing with one must remove every row for the set.
func TestTopSnapshotReplaceWithEmptyRemovesEverything(t *testing.T) {
	env := harness.New(t)
	user := env.NewUser("top-empty")
	repo := library.New(env.Store)

	if err := repo.ReplaceTopSnapshot(env.Ctx(), env.Store.DB(), user.ID,
		kindArtist, rangeShort, topIDs("art", 5), topBase); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := repo.ReplaceTopSnapshot(env.Ctx(), env.Store.DB(), user.ID,
		kindArtist, rangeShort, nil, topBase); err != nil {
		t.Fatalf("replace with empty: %v", err)
	}

	if got := env.ScalarInt(
		`SELECT count(*) FROM spotify_top_snapshots WHERE user_id = $1 AND kind = $2 AND time_range = $3`,
		user.ID.String(), kindArtist, rangeShort); got != 0 {
		t.Fatalf("%d rows survived a replace with an empty ranking, want 0", got)
	}
}

// TestTopSnapshotReturnsIDsInRankOrder checks the read side directly: ids
// come back ordered by Spotify's rank (the position column), not by id and
// not by whatever order Postgres happened to store them in.
func TestTopSnapshotReturnsIDsInRankOrder(t *testing.T) {
	env := harness.New(t)
	user := env.NewUser("top-rank-order")
	repo := library.New(env.Store)

	want := []string{"zzz", "aaa", "mmm"} // rank order disagrees with alphabetical order
	if err := repo.ReplaceTopSnapshot(env.Ctx(), env.Store.DB(), user.ID,
		kindTrack, rangeMedium, want, topBase); err != nil {
		t.Fatalf("replace: %v", err)
	}

	got, err := repo.TopSnapshot(env.Ctx(), env.Store.DB(), user.ID, kindTrack, rangeMedium)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !slices.Equal(got.EntityIDs, want) {
		t.Fatalf("entity ids = %v, want %v (rank order, not alphabetical)", got.EntityIDs, want)
	}
}

// TestTopSnapshotCapturedAtNilWhenNeverCaptured checks the never-captured
// case reads as CapturedAt == nil and an empty ranking, not a zero time and
// not an error — then that capturing sets it.
func TestTopSnapshotCapturedAtNilWhenNeverCaptured(t *testing.T) {
	env := harness.New(t)
	user := env.NewUser("top-never-captured")
	repo := library.New(env.Store)

	before, err := repo.TopSnapshot(env.Ctx(), env.Store.DB(), user.ID, kindArtist, rangeLong)
	if err != nil {
		t.Fatalf("read before any capture: %v", err)
	}
	if before.CapturedAt != nil {
		t.Fatalf("captured_at = %v for a set nothing has ever captured, want nil", before.CapturedAt)
	}
	if len(before.EntityIDs) != 0 {
		t.Fatalf("entity ids = %v for a set nothing has ever captured, want empty", before.EntityIDs)
	}

	if err := repo.ReplaceTopSnapshot(env.Ctx(), env.Store.DB(), user.ID,
		kindArtist, rangeLong, topIDs("art", 3), topBase); err != nil {
		t.Fatalf("replace: %v", err)
	}

	after, err := repo.TopSnapshot(env.Ctx(), env.Store.DB(), user.ID, kindArtist, rangeLong)
	if err != nil {
		t.Fatalf("read after capture: %v", err)
	}
	if after.CapturedAt == nil || !after.CapturedAt.Equal(topBase) {
		t.Fatalf("captured_at = %v after a capture, want %v", after.CapturedAt, topBase)
	}
}

// TestTopSnapshotSetsAreIndependent guards the WHERE clause scoping kind and
// time_range together: a too-broad delete predicate (scoped by user and kind
// alone, say) would silently wipe the other time ranges, or the other item
// kind, when only one set was meant to change.
func TestTopSnapshotSetsAreIndependent(t *testing.T) {
	env := harness.New(t)
	user := env.NewUser("top-sets-independent")
	repo := library.New(env.Store)

	type set struct{ kind, timeRange string }
	sets := []set{
		{kindArtist, rangeShort}, {kindArtist, rangeMedium}, {kindArtist, rangeLong},
		{kindTrack, rangeShort}, {kindTrack, rangeMedium}, {kindTrack, rangeLong},
	}
	for _, s := range sets {
		if err := repo.ReplaceTopSnapshot(env.Ctx(), env.Store.DB(), user.ID,
			s.kind, s.timeRange, topIDs(s.kind+"-"+s.timeRange, 5), topBase); err != nil {
			t.Fatalf("seed %s/%s: %v", s.kind, s.timeRange, err)
		}
	}

	// Replace only artist/short_term, shrinking it, with a later captured_at.
	laterCapture := topBase.Add(time.Hour)
	if err := repo.ReplaceTopSnapshot(env.Ctx(), env.Store.DB(), user.ID,
		kindArtist, rangeShort, topIDs("artist-short_term", 2), laterCapture); err != nil {
		t.Fatalf("replace artist/short_term: %v", err)
	}

	if got := env.ScalarInt(
		`SELECT count(*) FROM spotify_top_snapshots WHERE user_id = $1 AND kind = $2 AND time_range = $3`,
		user.ID.String(), kindArtist, rangeShort); got != 2 {
		t.Fatalf("artist/short_term has %d rows after shrinking to 2, want 2", got)
	}

	// The other five sets must be untouched: still 5 rows, still topBase.
	for _, s := range sets {
		if s.kind == kindArtist && s.timeRange == rangeShort {
			continue
		}
		got, err := repo.TopSnapshot(env.Ctx(), env.Store.DB(), user.ID, s.kind, s.timeRange)
		if err != nil {
			t.Fatalf("read %s/%s: %v", s.kind, s.timeRange, err)
		}
		if want := topIDs(s.kind+"-"+s.timeRange, 5); !slices.Equal(got.EntityIDs, want) {
			t.Fatalf("%s/%s entity ids = %v after an unrelated replace, want untouched %v",
				s.kind, s.timeRange, got.EntityIDs, want)
		}
		if got.CapturedAt == nil || !got.CapturedAt.Equal(topBase) {
			t.Fatalf("%s/%s captured_at = %v after an unrelated replace, want untouched %v",
				s.kind, s.timeRange, got.CapturedAt, topBase)
		}
	}
}

// TestTopSnapshotTwoUsersDoNotInterfere guards the scoping in the DELETE half
// of the reconciliation: without "WHERE user_id = $1" a shrink for one user
// would also erase a position another user happens to share.
func TestTopSnapshotTwoUsersDoNotInterfere(t *testing.T) {
	env := harness.New(t)
	a := env.NewUser("top-user-a")
	b := env.NewUser("top-user-b")
	repo := library.New(env.Store)

	if err := repo.ReplaceTopSnapshot(env.Ctx(), env.Store.DB(), a.ID,
		kindArtist, rangeShort, topIDs("art", 3), topBase); err != nil {
		t.Fatalf("replace for a: %v", err)
	}
	if err := repo.ReplaceTopSnapshot(env.Ctx(), env.Store.DB(), b.ID,
		kindArtist, rangeShort, topIDs("art", 3), topBase); err != nil {
		t.Fatalf("replace for b: %v", err)
	}

	// Shrink a's set to one entry; b's three rows at the same positions must
	// survive untouched.
	if err := repo.ReplaceTopSnapshot(env.Ctx(), env.Store.DB(), a.ID,
		kindArtist, rangeShort, topIDs("art", 1), topBase); err != nil {
		t.Fatalf("shrink a: %v", err)
	}

	if got := env.ScalarInt(
		`SELECT count(*) FROM spotify_top_snapshots WHERE user_id = $1`, a.ID.String()); got != 1 {
		t.Fatalf("user a has %d rows after shrinking to 1, want 1", got)
	}
	if got := env.ScalarInt(
		`SELECT count(*) FROM spotify_top_snapshots WHERE user_id = $1`, b.ID.String()); got != 3 {
		t.Fatal("user a's shrink deleted rows belonging to user b at the same positions")
	}
}

// TestTopSnapshotDeletingUserCascadesRows: the foreign key is ON DELETE
// CASCADE, so removing a user must not leave an orphaned snapshot row behind.
func TestTopSnapshotDeletingUserCascadesRows(t *testing.T) {
	env := harness.New(t)
	user := env.NewUser("top-cascade")
	repo := library.New(env.Store)

	if err := repo.ReplaceTopSnapshot(env.Ctx(), env.Store.DB(), user.ID,
		kindArtist, rangeShort, topIDs("art", 3), topBase); err != nil {
		t.Fatalf("replace: %v", err)
	}

	env.Exec(`DELETE FROM users WHERE id = $1`, user.ID.String())

	if got := env.ScalarInt(
		`SELECT count(*) FROM spotify_top_snapshots WHERE user_id = $1`, user.ID.String()); got != 0 {
		t.Fatalf("%d snapshot rows survived the user's deletion, want 0", got)
	}
}
