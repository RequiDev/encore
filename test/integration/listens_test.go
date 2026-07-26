//go:build integration

package integration

import (
	"context"

	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/requi/encore/internal/domain"
	"github.com/requi/encore/internal/store/listens"
	"github.com/requi/encore/test/harness"
)

func base() time.Time { return time.Date(2024, time.March, 15, 12, 0, 0, 0, time.UTC) }

// stage builds a staged listen the way the importer does, so these tests
// exercise the same key derivation the real path uses.
func stage(userID uuid.UUID, id domain.TrackIdentity, at time.Time, ms int32, src domain.Source, p domain.Precision) listens.StagedListen {
	return listens.Stage(domain.Listen{
		UserID:    userID,
		PlayedAt:  at,
		Precision: p,
		Identity:  id,
		MsPlayed:  ms,
		Source:    src,
	}, nil)
}

func ensure(t *testing.T, e *harness.Env, ids ...string) {
	t.Helper()
	if err := e.Listens.EnsureTracks(e.Ctx(), e.Store.DB(), ids); err != nil {
		t.Fatalf("ensure tracks: %v", err)
	}
}

// TestInsertListensIsIdempotent is the property the whole import design rests
// on: running the same batch twice must add nothing the second time, and it must
// be the database that enforces it rather than application bookkeeping.
func TestInsertListensIsIdempotent(t *testing.T) {
	e := harness.New(t)
	user := e.NewUser("idempotent")
	ensure(t, e, "track-a", "track-b")

	batch := []listens.StagedListen{
		stage(user.ID, domain.TrackIdentityFromID("track-a"), base(), 200_000, domain.SourceExtended, domain.PrecisionSecond),
		stage(user.ID, domain.TrackIdentityFromID("track-b"), base().Add(5*time.Minute), 180_000, domain.SourceExtended, domain.PrecisionSecond),
	}

	first, err := e.Listens.InsertListens(e.Ctx(), e.Store.DB(), batch, "UTC")
	if err != nil {
		t.Fatalf("first insert: %v", err)
	}
	if first != 2 {
		t.Fatalf("first insert = %d, want 2", first)
	}

	second, err := e.Listens.InsertListens(e.Ctx(), e.Store.DB(), batch, "UTC")
	if err != nil {
		t.Fatalf("second insert: %v", err)
	}
	if second != 0 {
		t.Fatalf("re-inserting the same batch added %d rows, want 0", second)
	}
	if got := e.CountListens(user.ID); got != 2 {
		t.Fatalf("database holds %d listens, want 2", got)
	}
}

// TestInsertListensCollapsesDuplicatesWithinOneBatch covers a file that repeats
// the same event; without the in-batch collapse the unique constraint would
// reject the whole statement rather than the offending row.
func TestInsertListensCollapsesDuplicatesWithinOneBatch(t *testing.T) {
	e := harness.New(t)
	user := e.NewUser("inbatch")
	ensure(t, e, "track-a")

	id := domain.TrackIdentityFromID("track-a")
	batch := []listens.StagedListen{
		stage(user.ID, id, base(), 200_000, domain.SourceExtended, domain.PrecisionSecond),
		stage(user.ID, id, base().Add(2*time.Second), 210_000, domain.SourceExtended, domain.PrecisionSecond),
		stage(user.ID, id, base().Add(3*time.Second), 190_000, domain.SourceExtended, domain.PrecisionSecond),
	}

	n, err := e.Listens.InsertListens(e.Ctx(), e.Store.DB(), batch, "UTC")
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if n != 1 {
		t.Fatalf("inserted %d rows for one event repeated three times in a minute, want 1", n)
	}
	// The longest play is kept, because a truncated duplicate should not
	// overwrite a complete record of the same listen.
	if ms := e.ScalarInt(`SELECT ms_played FROM listens WHERE user_id = $1`, user.ID.String()); ms != 210_000 {
		t.Fatalf("kept ms_played = %d, want the longest play 210000", ms)
	}
}

// TestCrossSourceSuppression is the case layer 1 cannot reach: an account-data
// export timestamps to the minute, so the same event lands in a different bucket
// from the extended export's answer.
func TestCrossSourceSuppression(t *testing.T) {
	e := harness.New(t)
	user := e.NewUser("crosssource")
	ensure(t, e, "track-a")

	id := domain.TrackIdentityFromID("track-a")

	// One play, seen twice. The extended export gives the stream end to the
	// second, so the derived start is 12:00:10. The account-data export truncates
	// the same stream end to the minute, so its derived start is 37 seconds
	// earlier, at 11:59:33 — deliberately on the other side of a minute boundary,
	// which is exactly the case the exact key cannot catch.
	extended := []listens.StagedListen{
		stage(user.ID, id, base().Add(10*time.Second), 200_000, domain.SourceExtended, domain.PrecisionSecond),
	}
	if n, err := e.Listens.InsertListens(e.Ctx(), e.Store.DB(), extended, "UTC"); err != nil || n != 1 {
		t.Fatalf("seed extended listen: n=%d err=%v", n, err)
	}

	accountData := []listens.StagedListen{
		stage(user.ID, id, base().Add(-27*time.Second), 200_000, domain.SourceAccountData, domain.PrecisionMinute),
	}
	if extended[0].DedupeKey == nil || accountData[0].DedupeKey == nil {
		t.Fatal("staged listens must carry dedupe keys")
	}
	if string(extended[0].DedupeKey) == string(accountData[0].DedupeKey) {
		t.Fatal("the two records landed in the same bucket, so this test is not exercising layer 2")
	}

	n, err := e.Listens.InsertListens(e.Ctx(), e.Store.DB(), accountData, "UTC")
	if err != nil {
		t.Fatalf("insert account data: %v", err)
	}
	if n != 0 {
		t.Fatalf("the same play arriving from a second source inserted %d rows, want 0", n)
	}
	if got := e.CountListens(user.ID); got != 1 {
		t.Fatalf("database holds %d listens, want 1", got)
	}
}

// TestImportUpgradesASyncedListen: the live feed cannot report how long a track
// was played, so a synced listen records the track's whole duration. When an
// export later describes the same event with a real ms_played, the stored row
// must take the better value rather than keeping the first one that arrived.
func TestImportUpgradesASyncedListen(t *testing.T) {
	e := harness.New(t)
	user := e.NewUser("upgrade")
	ensure(t, e, "track-a")

	id := domain.TrackIdentityFromID("track-a")

	// Live sync: full track length, because the feed reports nothing better.
	synced := stage(user.ID, id, base(), 240_000, domain.SourceSync, domain.PrecisionMillisecond)
	if n, err := e.Listens.InsertListens(e.Ctx(), e.Store.DB(), []listens.StagedListen{synced}, "UTC"); err != nil || n != 1 {
		t.Fatalf("seed synced listen: n=%d err=%v", n, err)
	}

	// The same play from an extended export: it was actually skipped after 31s,
	// and it carries the playback context sync never had.
	imported := stage(user.ID, id, base().Add(4*time.Second), 31_000, domain.SourceExtended, domain.PrecisionSecond)
	imported.ReasonEnd = "fwdbtn"
	imported.Platform = "android"
	imported.Skipped = domain.BoolPtr(true)

	n, err := e.Listens.InsertListens(e.Ctx(), e.Store.DB(), []listens.StagedListen{imported}, "UTC")
	if err != nil {
		t.Fatalf("import over the synced listen: %v", err)
	}
	if n != 0 {
		t.Fatalf("the import inserted %d rows, want 0: it is the same event", n)
	}
	if got := e.CountListens(user.ID); got != 1 {
		t.Fatalf("database holds %d listens, want 1", got)
	}

	var ms int32
	var source int16
	var reasonEnd, platform *string
	var skipped *bool
	if err := e.Pool.QueryRow(e.Ctx(),
		`SELECT ms_played, source, reason_end, platform, skipped FROM listens WHERE user_id = $1`,
		user.ID.String()).Scan(&ms, &source, &reasonEnd, &platform, &skipped); err != nil {
		t.Fatalf("read the surviving listen: %v", err)
	}
	if ms != 31_000 {
		t.Fatalf("ms_played = %d, want the export's 31000: listening time would otherwise stay "+
			"overstated for every period that was synced live", ms)
	}
	if source != int16(domain.SourceExtended) {
		t.Fatalf("source = %d, want the export's %d, so the row says where its data came from",
			source, domain.SourceExtended)
	}
	if reasonEnd == nil || *reasonEnd != "fwdbtn" {
		t.Fatal("the export's playback context was not carried over")
	}
	if platform == nil || *platform != "android" {
		t.Fatal("the export's platform was not carried over")
	}
	if skipped == nil || !*skipped {
		t.Fatal("the export's skipped flag was not carried over")
	}

	// Running the same import again must change nothing.
	if n, err := e.Listens.InsertListens(e.Ctx(), e.Store.DB(), []listens.StagedListen{imported}, "UTC"); err != nil || n != 0 {
		t.Fatalf("re-import: n=%d err=%v, want 0 and no error", n, err)
	}
	var msAgain int32
	if err := e.Pool.QueryRow(e.Ctx(),
		`SELECT ms_played FROM listens WHERE user_id = $1`, user.ID.String()).Scan(&msAgain); err != nil {
		t.Fatalf("re-read: %v", err)
	}
	if msAgain != 31_000 {
		t.Fatalf("ms_played drifted to %d on a repeated import", msAgain)
	}
}

// TestSameSourceRepeatIsKept guards against over-suppression. Playing a track,
// skipping it, and playing it again is two genuine listens seconds apart, and a
// window applied within a single source would silently delete one of them.
func TestSameSourceRepeatIsKept(t *testing.T) {
	e := harness.New(t)
	user := e.NewUser("repeat")
	ensure(t, e, "track-a")

	id := domain.TrackIdentityFromID("track-a")

	// Deliberately either side of a minute boundary: same source, seconds apart,
	// two real events.
	first := stage(user.ID, id, base().Add(59*time.Second), 3_000, domain.SourceExtended, domain.PrecisionSecond)
	second := stage(user.ID, id, base().Add(62*time.Second), 210_000, domain.SourceExtended, domain.PrecisionSecond)

	if n, err := e.Listens.InsertListens(e.Ctx(), e.Store.DB(), []listens.StagedListen{first}, "UTC"); err != nil || n != 1 {
		t.Fatalf("insert first: n=%d err=%v", n, err)
	}
	n, err := e.Listens.InsertListens(e.Ctx(), e.Store.DB(), []listens.StagedListen{second}, "UTC")
	if err != nil {
		t.Fatalf("insert second: %v", err)
	}
	if n != 1 {
		t.Fatalf("a genuine rapid repeat from the same source inserted %d rows, want 1", n)
	}
	if got := e.CountListens(user.ID); got != 2 {
		t.Fatalf("database holds %d listens, want 2", got)
	}
}

// TestListensAreScopedPerUser: two people listening to the same track at the
// same instant are two listens, because the user id is part of the key.
func TestListensAreScopedPerUser(t *testing.T) {
	e := harness.New(t)
	alice := e.NewUser("alice")
	bob := e.NewUser("bob")
	ensure(t, e, "track-a")

	id := domain.TrackIdentityFromID("track-a")
	batch := []listens.StagedListen{
		stage(alice.ID, id, base(), 200_000, domain.SourceExtended, domain.PrecisionSecond),
		stage(bob.ID, id, base(), 200_000, domain.SourceExtended, domain.PrecisionSecond),
	}
	n, err := e.Listens.InsertListens(e.Ctx(), e.Store.DB(), batch, "UTC")
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if n != 2 {
		t.Fatalf("inserted %d rows for two users, want 2", n)
	}
}

// TestInsertMarksRollupDaysDirty: statistics correctness depends on the dirty
// marker being written in the same transaction as the rows that dirtied it.
func TestInsertMarksRollupDaysDirty(t *testing.T) {
	e := harness.New(t)
	user := e.NewUser("dirty")
	ensure(t, e, "track-a")

	// 00:30 UTC on the 16th is still the 15th in New York, which is the point:
	// the marker must be the user's local day, not the UTC one.
	at := time.Date(2024, time.March, 16, 0, 30, 0, 0, time.UTC)
	batch := []listens.StagedListen{
		stage(user.ID, domain.TrackIdentityFromID("track-a"), at, 200_000, domain.SourceExtended, domain.PrecisionSecond),
	}
	if _, err := e.Listens.InsertListens(e.Ctx(), e.Store.DB(), batch, "America/New_York"); err != nil {
		t.Fatalf("insert: %v", err)
	}

	var day time.Time
	if err := e.Pool.QueryRow(e.Ctx(),
		`SELECT day FROM rollup_dirty_days WHERE user_id = $1`, user.ID.String()).Scan(&day); err != nil {
		t.Fatalf("read dirty day: %v", err)
	}
	if got := day.Format("2006-01-02"); got != "2024-03-15" {
		t.Fatalf("marked %s dirty, want the local day 2024-03-15", got)
	}
}

// TestRelinkConvergesNamesOnlyListens exercises layer 3: an account-data import
// stored under a names-only identity is repointed at the real track once the
// alias resolves, and a row that then collides with a URI-based listen is
// removed rather than double-counted.
func TestRelinkConvergesNamesOnlyListens(t *testing.T) {
	e := harness.New(t)
	user := e.NewUser("relink")
	ensure(t, e, "track-a")

	names := domain.TrackIdentityFromNames("Radiohead", "Weird Fishes")
	uri := domain.TrackIdentityFromID("track-a")

	// A names-only listen that duplicates a URI-based one, and a second that does not.
	seed := []listens.StagedListen{
		stage(user.ID, uri, base(), 200_000, domain.SourceExtended, domain.PrecisionSecond),
	}
	if n, err := e.Listens.InsertListens(e.Ctx(), e.Store.DB(), seed, "UTC"); err != nil || n != 1 {
		t.Fatalf("seed: n=%d err=%v", n, err)
	}

	namesOnly := []listens.StagedListen{
		stage(user.ID, names, base(), 200_000, domain.SourceAccountData, domain.PrecisionMinute),
		stage(user.ID, names, base().Add(2*time.Hour), 200_000, domain.SourceAccountData, domain.PrecisionMinute),
	}
	if n, err := e.Listens.InsertListens(e.Ctx(), e.Store.DB(), namesOnly, "UTC"); err != nil || n != 2 {
		t.Fatalf("names-only insert: n=%d err=%v, want 2 (identities differ, so nothing is suppressed yet)", n, err)
	}
	if got := e.CountListens(user.ID); got != 3 {
		t.Fatalf("before relink the database holds %d listens, want 3", got)
	}

	rows, err := e.Listens.UnresolvedListensForIdentity(e.Ctx(), e.Store.DB(), names.Key(), 0, 100)
	if err != nil {
		t.Fatalf("list unresolved: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("found %d unresolved listens, want 2", len(rows))
	}

	var result listens.RelinkResult
	err = e.Store.InTx(e.Ctx(), func(ctx context.Context, tx pgx.Tx) error {
		var err error
		result, err = e.Listens.ApplyRelink(ctx, tx, rows, "track-a", "UTC")
		return err
	})
	if err != nil {
		t.Fatalf("relink: %v", err)
	}
	if result.Removed != 1 {
		t.Fatalf("removed %d colliding rows, want 1", result.Removed)
	}
	if result.Relinked != 1 {
		t.Fatalf("relinked %d rows, want 1", result.Relinked)
	}
	if got := e.CountListens(user.ID); got != 2 {
		t.Fatalf("after relink the database holds %d listens, want 2", got)
	}
	if unresolved := e.ScalarInt(
		`SELECT count(*) FROM listens WHERE user_id = $1 AND track_id IS NULL`, user.ID.String()); unresolved != 0 {
		t.Fatalf("%d listens are still unresolved, want 0", unresolved)
	}
}
