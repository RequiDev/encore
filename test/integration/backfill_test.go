//go:build integration

package integration

import (
	"testing"
	"time"

	"github.com/RequiDev/encore/internal/domain"
	"github.com/RequiDev/encore/internal/spotify"
	"github.com/RequiDev/encore/internal/store/accounts"
	"github.com/RequiDev/encore/internal/store/listens"
	"github.com/RequiDev/encore/test/harness"
)

// backfillRig is one user, one catalogue track, and helpers for the two rows
// this whole feature is about.
type backfillRig struct {
	e    *harness.Env
	user domain.User
}

const backfillTrackID = "spotifytrack00000001"

func newBackfillRig(t *testing.T, name string) *backfillRig {
	t.Helper()
	e := harness.New(t)
	r := &backfillRig{e: e, user: e.NewUser(name)}
	if err := e.Listens.EnsureTracks(e.Ctx(), e.Store.DB(),
		[]listens.TrackSeed{{ID: backfillTrackID, Name: "The Wheel"}}); err != nil {
		t.Fatalf("EnsureTracks: %v", err)
	}
	return r
}

// listen inserts one live-synced play of the shared track, starting at
// playedAt and lasting durationMs, with shuffle and device_type left NULL.
func (r *backfillRig) listen(t *testing.T, playedAt time.Time, durationMs int32) {
	t.Helper()
	l := domain.Listen{
		UserID:    r.user.ID,
		PlayedAt:  playedAt.UTC(),
		Precision: domain.PrecisionMillisecond,
		Identity:  domain.TrackIdentityFromID(backfillTrackID),
		MsPlayed:  durationMs,
		Source:    domain.SourceSync,
	}
	if _, err := r.e.Listens.InsertListens(
		r.e.Ctx(), r.e.Store.DB(), []listens.StagedListen{listens.Stage(l, nil)}, "UTC"); err != nil {
		t.Fatalf("InsertListens: %v", err)
	}
}

// observe logs one observation of the shared track.
func (r *backfillRig) observe(t *testing.T, at time.Time, shuffle *bool, deviceType string) {
	t.Helper()
	if err := r.e.Accounts.PlaybackObservations.Log(r.e.Ctx(), r.e.Store.DB(), r.user.ID,
		domain.PlaybackObservation{
			TrackID: backfillTrackID, ObservedAt: at.UTC(),
			Shuffle: shuffle, DeviceType: deviceType,
		}); err != nil {
		t.Fatalf("Log: %v", err)
	}
}

// backfill runs one pass and reports how many rows it annotated.
func (r *backfillRig) backfill(t *testing.T, now time.Time) int64 {
	t.Helper()
	n, err := r.e.Listens.BackfillPlaybackContext(r.e.Ctx(), r.e.Store.DB(), r.user.ID, now)
	if err != nil {
		t.Fatalf("BackfillPlaybackContext: %v", err)
	}
	return n
}

// state reads the two columns the backfill may write. It expects exactly one
// listen, which is every case that calls it.
func (r *backfillRig) state(t *testing.T) (shuffle *bool, deviceType *string) {
	t.Helper()
	if err := r.e.Store.DB().QueryRow(r.e.Ctx(),
		`SELECT shuffle, device_type FROM listens WHERE user_id = $1`,
		r.user.ID.String()).Scan(&shuffle, &deviceType); err != nil {
		t.Fatalf("read listen state: %v", err)
	}
	return shuffle, deviceType
}

// forgetDirtyDays empties rollup_dirty_days for this user.
//
// Seeding a listen goes through InsertListens, whose `dirty` CTE marks the
// local day of every row it writes — so the table is never empty by the time a
// test reaches its assertions. Clearing it after the fixture is built is what
// makes "the backfill marked no dirty days" an assertion about the backfill
// rather than about the fixture that preceded it.
func (r *backfillRig) forgetDirtyDays() {
	r.e.Exec(`DELETE FROM rollup_dirty_days WHERE user_id = $1`, r.user.ID.String())
}

// dirtyDays counts the rollup work outstanding for this user.
func (r *backfillRig) dirtyDays() int64 {
	return r.e.ScalarInt(`SELECT count(*) FROM rollup_dirty_days WHERE user_id = $1`, r.user.ID.String())
}

// TestAnObservationInsideTheWindowFillsTheListenAndOneOutsideLeavesItNull is the
// match rule, asserted from both sides of the boundary the named constant sets.
//
// The two instants are derived from listens.ObservationTolerance rather than
// written as literals. A fixture with hard-coded seconds passes for whichever
// tolerance happens to be in force, which is precisely the shape of test this
// project has shipped unable to fail before: retuning the constant — the one
// thing §2.5 says is most likely to need tuning against real data — would leave
// it green while the behaviour it claims to pin changed underneath it.
//
// Fails when: the window's upper bound stops adding ms_played, or stops adding
// the tolerance, or the tolerance is widened past the gap between the two
// fixtures below — the "outside" case then matches and its assertion fires.
func TestAnObservationInsideTheWindowFillsTheListenAndOneOutsideLeavesItNull(t *testing.T) {
	played := time.Date(2026, time.August, 1, 20, 0, 0, 0, time.UTC)
	const durationMs int32 = 255000
	end := played.Add(time.Duration(durationMs) * time.Millisecond)
	yes := true

	t.Run("inside", func(t *testing.T) {
		r := newBackfillRig(t, "bf-inside")
		r.listen(t, played, durationMs)
		// One second inside the far edge of the window.
		r.observe(t, end.Add(listens.ObservationTolerance-time.Second), &yes, "Speaker")

		if n := r.backfill(t, end.Add(time.Hour)); n != 1 {
			t.Fatalf("backfill annotated %d rows, want 1", n)
		}
		shuffle, deviceType := r.state(t)
		if shuffle == nil || !*shuffle {
			t.Errorf("shuffle = %v, want true", shuffle)
		}
		if deviceType == nil || *deviceType != "Speaker" {
			t.Errorf("device_type = %v, want Speaker", deviceType)
		}
	})

	t.Run("outside", func(t *testing.T) {
		r := newBackfillRig(t, "bf-outside")
		r.listen(t, played, durationMs)
		// One second past the far edge.
		r.observe(t, end.Add(listens.ObservationTolerance+time.Second), &yes, "Speaker")

		if n := r.backfill(t, end.Add(time.Hour)); n != 0 {
			t.Fatalf("backfill annotated %d rows, want 0 for an observation outside the window", n)
		}
		shuffle, deviceType := r.state(t)
		if shuffle != nil {
			t.Errorf("shuffle = %v, want NULL: an unmatched listen must not be labelled", *shuffle)
		}
		if deviceType != nil {
			t.Errorf("device_type = %v, want NULL", *deviceType)
		}
	})

	t.Run("before the play started", func(t *testing.T) {
		r := newBackfillRig(t, "bf-before")
		r.listen(t, played, durationMs)
		r.observe(t, played.Add(-time.Second), &yes, "Speaker")

		if n := r.backfill(t, end.Add(time.Hour)); n != 0 {
			t.Fatalf("backfill annotated %d rows, want 0 for an observation before played_at", n)
		}
	})
}

// TestTheBackfillNeverInventsAFalse is the "unknown and false are different
// facts" rule, at the last place it can still be broken.
//
// An observation that carries a device and no shuffle state must fill
// device_type and leave shuffle NULL. A boolean coerced anywhere upstream, or a
// SET that writes COALESCE(l.shuffle, m.shuffle, false), would state on
// somebody's history that a play was not shuffled about a fact nobody ever
// reported.
//
// The protection is the COALESCE chain and nothing else, which is worth being
// precise about: the final WHERE's "m.shuffle IS NOT NULL" does *not* guard
// this. Dropping it lets the row be updated, but the SET still writes
// COALESCE(NULL, NULL) and shuffle stays NULL — verified by mutation, against an
// earlier draft of this comment that claimed otherwise. What that clause
// actually guards is the row count; see
// TestAPassThatWouldChangeNothingReportsZero.
//
// Fails when: the SET grows a default — COALESCE(..., false) — or *bool becomes
// bool anywhere between Spotify's JSON and this column. shuffle then reads false
// instead of NULL.
func TestTheBackfillNeverInventsAFalse(t *testing.T) {
	played := time.Date(2026, time.August, 1, 20, 0, 0, 0, time.UTC)
	const durationMs int32 = 255000

	r := newBackfillRig(t, "bf-unknown")
	r.listen(t, played, durationMs)
	r.observe(t, played.Add(time.Minute), nil, "Computer")

	if n := r.backfill(t, played.Add(time.Hour)); n != 1 {
		t.Fatalf("backfill annotated %d rows, want 1", n)
	}
	shuffle, deviceType := r.state(t)
	if shuffle != nil {
		t.Errorf("shuffle = %v, want NULL: nobody reported a shuffle state", *shuffle)
	}
	if deviceType == nil || *deviceType != "Computer" {
		t.Errorf("device_type = %v, want Computer", deviceType)
	}
}

// TestRunningTheBackfillTwiceChangesNothingAndCreatesNothing is the idempotence
// property, asserted three ways because one way is not enough.
//
// A count that did not move proves nothing was created. A second call returning
// zero proves nothing was written. Identical values prove nothing was moved.
// Encore's core guarantee is that re-running ingestion adds exactly zero rows,
// and DedupeKey(UserID, Identity, PlayedAt) deliberately excludes context, so a
// backfill that wrote through an INSERT would multiply a listener's history by
// however many times it ran.
//
// It also pins the absent side effect at the one place it could actually
// appear: a pass that *matched* something. listen_daily_rollup is keyed
// (user, day, track) and carries no context columns, so nothing this statement
// writes can make an aggregate stale — but "mark the day dirty after annotating
// it" is exactly the helpful-looking line a later edit would add, and only a
// matching pass can catch it.
//
// Fails when: the statement gains an INSERT or an ON CONFLICT (the count moves);
// the candidate CTE stops filtering on "shuffle IS NULL OR device_type IS NULL"
// AND the final WHERE stops requiring a change (the second call reports 1);
// COALESCE is dropped so an existing value is rewritten; or the statement starts
// marking dirty days.
func TestRunningTheBackfillTwiceChangesNothingAndCreatesNothing(t *testing.T) {
	played := time.Date(2026, time.August, 1, 20, 0, 0, 0, time.UTC)
	const durationMs int32 = 255000
	yes := true

	r := newBackfillRig(t, "bf-twice")
	r.listen(t, played, durationMs)
	r.observe(t, played.Add(time.Minute), &yes, "Speaker")

	before := r.e.CountListens(r.user.ID)
	// Seeding the listen marked its local day; from here on any dirty day is
	// the backfill's doing.
	r.forgetDirtyDays()

	if n := r.backfill(t, played.Add(time.Hour)); n != 1 {
		t.Fatalf("the first pass annotated %d rows, want 1", n)
	}
	firstShuffle, firstDevice := r.state(t)

	if dirty := r.dirtyDays(); dirty != 0 {
		t.Errorf("a pass that annotated a row marked %d dirty days; shuffle and "+
			"device_type appear in no rollup, so a backfill can never make one stale", dirty)
	}

	if n := r.backfill(t, played.Add(time.Hour)); n != 0 {
		t.Fatalf("the second pass annotated %d rows, want 0: a backfill must be idempotent", n)
	}
	if after := r.e.CountListens(r.user.ID); after != before {
		t.Fatalf("listens went from %d to %d; a backfill may only annotate rows that "+
			"already exist", before, after)
	}
	secondShuffle, secondDevice := r.state(t)
	if secondShuffle == nil || *secondShuffle != *firstShuffle {
		t.Errorf("shuffle changed between passes: %v then %v", firstShuffle, secondShuffle)
	}
	if secondDevice == nil || *secondDevice != *firstDevice {
		t.Errorf("device_type changed between passes: %v then %v", firstDevice, secondDevice)
	}
}

// TestAPassThatWouldChangeNothingReportsZero pins the last of the five
// idempotence mechanisms, and is the only test that reaches it.
//
// A listen missing only its shuffle state, and an observation that knows only
// what it played on: what each lacks, the other cannot supply. The pass must
// decline to write rather than update the row to the values it already holds.
//
// That state is reachable rather than contrived — it is exactly what a first
// pass leaves behind when Spotify reported a device and omitted shuffle_state —
// and without this clause every pass after it, for ever, reports work it did not
// do. The count is what the poller logs and what any future consumer would read
// as "there was something to attach".
//
// This is what the final WHERE's "m.shuffle IS NOT NULL" is for. Nothing else in
// this file fails when it is dropped, because COALESCE already prevents a wrong
// *value* being written; the row count is the only observable difference.
//
// Fails when: the final WHERE drops "AND m.shuffle IS NOT NULL" — the row is
// then rewritten with its own values and the pass reports 1.
func TestAPassThatWouldChangeNothingReportsZero(t *testing.T) {
	played := time.Date(2026, time.August, 1, 20, 0, 0, 0, time.UTC)
	const durationMs int32 = 255000

	r := newBackfillRig(t, "bf-noop")
	r.listen(t, played, durationMs)
	// The state a first pass leaves when the observation carried a device and no
	// shuffle state.
	r.e.Exec(`UPDATE listens SET device_type = 'Computer' WHERE user_id = $1`, r.user.ID.String())
	r.observe(t, played.Add(time.Minute), nil, "Computer")

	if n := r.backfill(t, played.Add(time.Hour)); n != 0 {
		t.Fatalf("a pass with nothing left to add annotated %d rows, want 0", n)
	}
	shuffle, deviceType := r.state(t)
	if shuffle != nil {
		t.Errorf("shuffle = %v, want NULL: the observation never reported one", *shuffle)
	}
	if deviceType == nil || *deviceType != "Computer" {
		t.Errorf("device_type = %v, want Computer, unchanged", deviceType)
	}
}

// TestTheBackfillNeverOverwritesAnExport pins which record wins when both have
// an opinion.
//
// An extended export carries a real, first-hand shuffle flag for the play it
// describes. An observation is a point sample matched to it by a fuzzy window.
// The export is simply the better record, and a backfill that could overwrite it
// would degrade a history every time it ran.
//
// Fails when: COALESCE(l.shuffle, m.shuffle) becomes COALESCE(m.shuffle,
// l.shuffle), or the candidate CTE stops requiring the column to be NULL.
func TestTheBackfillNeverOverwritesAnExport(t *testing.T) {
	played := time.Date(2026, time.August, 1, 20, 0, 0, 0, time.UTC)
	const durationMs int32 = 255000
	yes, no := true, false

	r := newBackfillRig(t, "bf-export")
	r.listen(t, played, durationMs)
	// The export's own answer, already on the row.
	r.e.Exec(`UPDATE listens SET shuffle = $1 WHERE user_id = $2`, no, r.user.ID.String())
	r.observe(t, played.Add(time.Minute), &yes, "Speaker")

	if n := r.backfill(t, played.Add(time.Hour)); n != 1 {
		t.Fatalf("backfill annotated %d rows, want 1: device_type was still missing", n)
	}
	shuffle, deviceType := r.state(t)
	if shuffle == nil || *shuffle {
		t.Errorf("shuffle = %v, want false: the export's answer must survive", shuffle)
	}
	if deviceType == nil || *deviceType != "Speaker" {
		t.Errorf("device_type = %v, want Speaker: the column the export had nothing for", deviceType)
	}
}

// TestTheMostRecentObservationInTheWindowWins pins the tie-break the spec names,
// and is the test that documents this rule's known imprecision.
//
// Two observations inside one play's window disagree about the device. The
// later one is taken. That is §2.4's "takes the most recent match", and its
// false-positive mode is exactly what this fixture looks like from the other
// side: had the second observation belonged to a *following* play of the same
// track in the tolerance tail, this listen would carry that play's device.
//
// Fails when: ORDER BY o.observed_at DESC becomes ASC, or the DISTINCT ON is
// dropped and the UPDATE picks an arbitrary matching row.
func TestTheMostRecentObservationInTheWindowWins(t *testing.T) {
	played := time.Date(2026, time.August, 1, 20, 0, 0, 0, time.UTC)
	const durationMs int32 = 255000
	yes, no := true, false

	r := newBackfillRig(t, "bf-latest")
	r.listen(t, played, durationMs)
	r.observe(t, played.Add(30*time.Second), &no, "Computer")
	r.observe(t, played.Add(2*time.Minute), &yes, "Speaker")

	if n := r.backfill(t, played.Add(time.Hour)); n != 1 {
		t.Fatalf("backfill annotated %d rows, want 1", n)
	}
	shuffle, deviceType := r.state(t)
	if shuffle == nil || !*shuffle {
		t.Errorf("shuffle = %v, want true from the later observation", shuffle)
	}
	if deviceType == nil || *deviceType != "Speaker" {
		t.Errorf("device_type = %v, want Speaker from the later observation", deviceType)
	}
}

// TestAnotherUsersObservationNeverReachesThisListen is the tenancy check, and it
// is here because the match has three keys and only two of them are visible in
// the join clause.
//
// track and instant are joined explicitly; the account is not. It is enforced
// twice over by scoping, once on each side — the driving CTE selects only
// observations for $1, and the candidate filter selects only listens for $1 —
// so the two relations can never be about different people by the time they
// meet. That is why there is no o.user_id = l.user_id to read in the ON clause,
// and why removing either scope is the mutation this test exists to catch.
//
// Fails when: the obs CTE drops "WHERE o.user_id = $1" (the other account's
// observation then reaches this listen through the track and instant alone), or
// the candidate filter drops "l.user_id = $1".
func TestAnotherUsersObservationNeverReachesThisListen(t *testing.T) {
	played := time.Date(2026, time.August, 1, 20, 0, 0, 0, time.UTC)
	const durationMs int32 = 255000
	yes := true

	r := newBackfillRig(t, "bf-mine")
	other := r.e.NewUser("bf-theirs")
	r.listen(t, played, durationMs)
	if err := r.e.Accounts.PlaybackObservations.Log(r.e.Ctx(), r.e.Store.DB(), other.ID,
		domain.PlaybackObservation{
			TrackID: backfillTrackID, ObservedAt: played.Add(time.Minute),
			Shuffle: &yes, DeviceType: "Speaker",
		}); err != nil {
		t.Fatalf("Log for the other user: %v", err)
	}

	if n := r.backfill(t, played.Add(time.Hour)); n != 0 {
		t.Fatalf("backfill annotated %d rows from another account's observation, want 0", n)
	}
}

// TestAReapedObservationCanNeverMatch pins the retention rule from both sides,
// and is the one test that exercises the assumption the whole 24-hour figure
// rests on.
//
// The reap is a pure age predicate with no signal from any consumer, which is
// deliberate — reconciliation against a supplied set is how this repository has
// lost data before, and an observation's own timestamp already says whether it
// has outlived its usefulness. What that leaves unproven is the other half:
// that retention always outlasts the backfill's reach. This is where the two
// constants are made to meet.
//
// The stale play sits *in the gap between them*: older than
// accounts.ObservationRetention, so its observation is reaped, but newer than
// listens.BackfillLookback, so the backfill still considers the play itself.
// That arrangement is the whole test. Put the stale play outside the lookback
// instead — as an earlier draft of this fixture did — and the final assertion
// passes because the backfill never looked at the row, not because the
// observation was reaped, and the test quietly stops being about retention at
// all. The guard below fails loudly if retuning either constant closes the gap.
//
// This is also the worker-was-down case, which is the only way it arises in
// production: the poller and the sync loop are the same process, so an
// observation can only outlive its usefulness while nothing at all is running.
// What it costs is exactly this — the columns stay NULL, indistinguishable from
// never having looked, which is the benign direction.
//
// Fails when: DeleteExpired's predicate becomes <= now, or the caller passes
// time.Now() instead of now minus ObservationRetention (the fresh observation
// disappears and the backfill that follows annotates nothing); or the reap stops
// happening at all, at which case the stale listen below is annotated.
func TestAReapedObservationCanNeverMatch(t *testing.T) {
	now := time.Date(2026, time.August, 2, 12, 0, 0, 0, time.UTC)
	const durationMs int32 = 255000
	const staleBy = 2 * time.Hour
	yes := true

	if gap := listens.BackfillLookback - accounts.ObservationRetention; gap <= staleBy {
		t.Fatalf("this fixture needs BackfillLookback to exceed ObservationRetention by more "+
			"than %s, and the gap is %s. The stale play below no longer sits inside the "+
			"lookback, so the backfill would skip it for the wrong reason and this test "+
			"would assert nothing about retention.", staleBy, gap)
	}
	stalePlay := now.Add(-(accounts.ObservationRetention + staleBy))
	freshPlay := now.Add(-time.Hour)

	r := newBackfillRig(t, "bf-reap")
	r.listen(t, stalePlay, durationMs)
	r.listen(t, freshPlay, durationMs)
	r.observe(t, stalePlay.Add(time.Minute), &yes, "Speaker")
	r.observe(t, freshPlay.Add(time.Minute), &yes, "Computer")

	gone, err := r.e.Accounts.PlaybackObservations.DeleteExpired(
		r.e.Ctx(), r.e.Store.DB(), now.Add(-accounts.ObservationRetention))
	if err != nil {
		t.Fatalf("DeleteExpired: %v", err)
	}
	if gone != 1 {
		t.Fatalf("DeleteExpired removed %d rows, want exactly 1", gone)
	}
	if left := r.e.ScalarInt(`SELECT count(*) FROM playback_observations WHERE user_id = $1`,
		r.user.ID.String()); left != 1 {
		t.Fatalf("%d observations survived, want 1: the reaper took a row it should not have", left)
	}

	if n := r.backfill(t, now); n != 1 {
		t.Fatalf("backfill annotated %d rows, want 1 — the surviving observation only", n)
	}
	var stale *bool
	if err := r.e.Store.DB().QueryRow(r.e.Ctx(),
		`SELECT shuffle FROM listens WHERE user_id = $1 AND played_at = $2`,
		r.user.ID.String(), stalePlay).Scan(&stale); err != nil {
		t.Fatalf("read the stale listen: %v", err)
	}
	if stale != nil {
		t.Errorf("the stale listen was labelled %v from a reaped observation", *stale)
	}
}

// TestABackfillPassWithNoObservationsAtAllIsANoOp is the disabled-instance case,
// which is most instances.
//
// With ENCORE_NOWPLAYING_INTERVAL unset the log is empty for ever, and the
// backfill must cost one probe and change nothing rather than being something an
// operator has to switch off separately. This is why this phase adds no
// configuration key.
//
// Fails when: the statement gains a side effect that does not depend on a match
// — a dirty-day insert, a touched updated_at — or starts writing NULL over
// existing values.
func TestABackfillPassWithNoObservationsAtAllIsANoOp(t *testing.T) {
	played := time.Date(2026, time.August, 1, 20, 0, 0, 0, time.UTC)
	const durationMs int32 = 255000

	r := newBackfillRig(t, "bf-empty")
	r.listen(t, played, durationMs)
	before := r.e.CountListens(r.user.ID)
	r.forgetDirtyDays()

	if n := r.backfill(t, played.Add(time.Hour)); n != 0 {
		t.Fatalf("backfill annotated %d rows with an empty observation log, want 0", n)
	}
	if after := r.e.CountListens(r.user.ID); after != before {
		t.Fatalf("listens went from %d to %d on an empty log", before, after)
	}
	if dirty := r.dirtyDays(); dirty != 0 {
		t.Errorf("%d dirty days were marked on a pass that matched nothing", dirty)
	}
	shuffle, deviceType := r.state(t)
	if shuffle != nil || deviceType != nil {
		t.Errorf("(shuffle, device_type) = (%v, %v) with an empty observation log, want NULL in both",
			shuffle, deviceType)
	}
}

// TestTheBackfillLeavesAnImportedListenAlone keeps the annotation on the rows it
// is about.
//
// Only a live-synced row is missing this by construction; an extended-export row
// has the export's own first-hand answer, and an account-data row has neither
// the columns nor a played_at precise enough for the window to mean anything.
//
// Fails when: the candidate CTE drops l.source = 0.
func TestTheBackfillLeavesAnImportedListenAlone(t *testing.T) {
	played := time.Date(2026, time.August, 1, 20, 0, 0, 0, time.UTC)
	const durationMs int32 = 255000
	yes := true

	r := newBackfillRig(t, "bf-import")
	r.listen(t, played, durationMs)
	r.e.Exec(`UPDATE listens SET source = 2 WHERE user_id = $1`, r.user.ID.String())
	r.observe(t, played.Add(time.Minute), &yes, "Speaker")

	if n := r.backfill(t, played.Add(time.Hour)); n != 0 {
		t.Fatalf("backfill annotated %d imported rows, want 0", n)
	}
}

// TestASyncPollAnnotatesWhatThePollerSaw is the seam: an observation logged by
// one loop reaches a listen written by another, through the database and
// nothing else.
//
// It drives the real Poller rather than calling the repository directly,
// because the repository passing and the poller never calling it is exactly the
// failure a store-level test cannot see. Every other case in this file would
// stay green if internal/sync had never been touched.
//
// The order the fixture is built in is the order production reaches it: the
// observation is logged while the track is playing, before recently-played has
// heard of the play and before the catalogue has a row for the track. That is
// why playback_observations.track_id is not a foreign key.
//
// Fails when: the call in poll is removed (the listen keeps NULL in both
// columns); or it is moved above commit, at which point the listen it is meant
// to annotate does not exist yet and the pass matches nothing.
func TestASyncPollAnnotatesWhatThePollerSaw(t *testing.T) {
	const trackID = "sync0000000000000000c7"
	yes := true

	env := harness.New(t)
	user := env.NewUser("bf-syncseam")
	connect(t, env, user.ID, time.Now().Add(time.Hour))

	// Within BackfillLookback of the poller's own clock, which newPoller leaves
	// as the real one.
	played := time.Now().Add(-2 * time.Hour).Truncate(time.Second)

	// What the now-playing poller saw, one minute into the play — logged before
	// the play is ingested, as it is in production.
	if err := env.Accounts.PlaybackObservations.Log(env.Ctx(), env.Store.DB(), user.ID,
		domain.PlaybackObservation{
			TrackID: trackID, ObservedAt: played.Add(time.Minute),
			Shuffle: &yes, DeviceType: "Speaker", DeviceName: "the kitchen",
		}); err != nil {
		t.Fatalf("log the observation: %v", err)
	}

	page := []spotify.PlayHistory{syncPlay(trackID, played)}
	// The same page twice: the second poll is the idempotence half of this test.
	api := &fakeRecentlyPlayed{pages: [][]spotify.PlayHistory{page, page}}
	poller := newPoller(t, env, api)

	res, err := poller.SyncUser(env.Ctx(), user.ID)
	if err != nil {
		t.Fatalf("first sync: %v", err)
	}
	if res.Imported != 1 {
		t.Fatalf("the poll imported %d plays, want 1", res.Imported)
	}

	var shuffle *bool
	var deviceType *string
	read := func(t *testing.T) (*bool, *string) {
		t.Helper()
		if err := env.Pool.QueryRow(env.Ctx(),
			`SELECT shuffle, device_type FROM listens WHERE user_id = $1`,
			user.ID.String()).Scan(&shuffle, &deviceType); err != nil {
			t.Fatalf("read the listen: %v", err)
		}
		return shuffle, deviceType
	}

	shuffle, deviceType = read(t)
	if shuffle == nil || !*shuffle {
		t.Fatalf("shuffle = %v, want true: the poll ingested the play but never attached "+
			"what the now-playing poller saw during it", shuffle)
	}
	if deviceType == nil || *deviceType != "Speaker" {
		t.Fatalf("device_type = %v, want Speaker", deviceType)
	}

	// device_name stays in the log. It is the one field that never becomes
	// durable on the fact table, and a column added to listens for it would be
	// the easiest thing in this phase to add by accident.
	if n := env.ScalarInt(
		`SELECT count(*) FROM information_schema.columns
          WHERE table_name = 'listens' AND column_name LIKE 'device_name%'`); n != 0 {
		t.Errorf("listens grew %d device_name column(s); the player's human name is "+
			"deliberately confined to playback_observations, which expires within a day", n)
	}

	res, err = poller.SyncUser(env.Ctx(), user.ID)
	if err != nil {
		t.Fatalf("second sync: %v", err)
	}
	if res.Imported != 0 {
		t.Fatalf("re-polling the same page imported %d plays, want 0", res.Imported)
	}
	if got := env.CountListens(user.ID); got != 1 {
		t.Fatalf("database holds %d listens after two polls of one play, want 1", got)
	}
	secondShuffle, secondDevice := read(t)
	if secondShuffle == nil || !*secondShuffle || secondDevice == nil || *secondDevice != "Speaker" {
		t.Fatalf("a second poll changed the annotation to (%v, %v), want (true, Speaker)",
			secondShuffle, secondDevice)
	}
}
