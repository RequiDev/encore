//go:build integration

// Package importtest is the import validation matrix.
//
// Every test here drives the real Intake and the real Runner against a real
// PostgreSQL, and every assertion about an outcome is made by counting rows in
// the fact table rather than by reading the importer's own counters. The
// counters are the thing being tested; they are not allowed to be the evidence.
package importtest

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/requi/encore/internal/config"
	"github.com/requi/encore/internal/domain"
	"github.com/requi/encore/test/harness"
)

// --- 1. a plain import, verified against the database -----------------------

func TestExtendedImportCommitsEveryValidRecord(t *testing.T) {
	rig := harness.NewRig(t, nil)
	user := rig.NewUser("basic")

	path := filepath.Join(t.TempDir(), "Streaming_History_Audio_2015-2017_0.json")
	total := harness.WriteExtendedExport(t, path, harness.GenOptions{
		Records: 500, Seed: 1, PodcastEvery: 25, ShortPlayEvery: 40,
	})

	job := rig.Submit(user.ID, "first import", path)
	rig.Drain(rig.Ctx())

	done := rig.RequireStatus(job.ID, domain.ImportCompleted)
	rig.RequireAccounted(done)
	rig.RequireCommitted(done)

	if done.Counters.Processed() != int64(total) {
		t.Fatalf("accounted for %d records, want %d", done.Counters.Processed(), total)
	}
	if done.Counters.Imported == 0 {
		t.Fatal("nothing was imported")
	}
	if done.Counters.Skipped == 0 {
		t.Fatal("podcasts and short plays should have been skipped")
	}
	if done.Counters.Rejected != 0 {
		t.Fatalf("%d records were rejected from a well-formed fixture", done.Counters.Rejected)
	}
	if got := rig.CountListens(user.ID); got != done.Counters.Imported {
		t.Fatalf("database holds %d listens, importer claims %d", got, done.Counters.Imported)
	}

	// Ingestion must have registered the tracks for later enrichment without
	// ever calling Spotify.
	pending := rig.ScalarInt(`SELECT count(*) FROM tracks WHERE metadata_state = 'pending'`)
	if pending == 0 {
		t.Fatal("no tracks were queued for metadata enrichment")
	}
}

// --- 2. duplicate files and overlapping imports -----------------------------

func TestReimportingTheSameFileAddsNothing(t *testing.T) {
	rig := harness.NewRig(t, nil)
	user := rig.NewUser("reimport")

	dir := t.TempDir()
	path := filepath.Join(dir, "Streaming_History_Audio_2015-2017_0.json")
	harness.WriteExtendedExport(t, path, harness.GenOptions{Records: 400, Seed: 7})

	first := rig.Submit(user.ID, "first", path)
	rig.Drain(rig.Ctx())
	firstDone := rig.RequireStatus(first.ID, domain.ImportCompleted)
	after := rig.CountListens(user.ID)
	if after != firstDone.Counters.Imported {
		t.Fatalf("database holds %d, importer claims %d", after, firstDone.Counters.Imported)
	}

	second, warnings := rig.SubmitWithWarnings(user.ID, path)
	if len(warnings) == 0 {
		t.Fatal("re-uploading an identical file should raise an already-imported warning")
	}
	rig.Drain(rig.Ctx())

	secondDone := rig.RequireStatus(second.ID, domain.ImportCompleted)
	rig.RequireAccounted(secondDone)
	rig.RequireCommitted(secondDone)

	if secondDone.Counters.Imported != 0 {
		t.Fatalf("re-import inserted %d new rows, want 0", secondDone.Counters.Imported)
	}
	if got := rig.CountListens(user.ID); got != after {
		t.Fatalf("row count moved from %d to %d on re-import", after, got)
	}
}

func TestOverlappingFormatsConverge(t *testing.T) {
	rig := harness.NewRig(t, nil)
	user := rig.NewUser("overlap")

	dir := t.TempDir()
	// Identical options mean the account-data file describes exactly the same
	// plays as the extended one, only with minute-precision timestamps and no
	// track URIs — the real-world overlap between the two Spotify exports.
	opts := harness.GenOptions{Records: 300, Seed: 42}
	extended := filepath.Join(dir, "Streaming_History_Audio_2015-2017_0.json")
	accountData := filepath.Join(dir, "StreamingHistory0.json")
	harness.WriteExtendedExport(t, extended, opts)
	harness.WriteAccountDataExport(t, accountData, opts)

	// Import the extended history first: it carries track URIs, so its listens
	// are anchored to catalogue ids.
	j1 := rig.Submit(user.ID, "extended", extended)
	rig.Drain(rig.Ctx())
	done1 := rig.RequireStatus(j1.ID, domain.ImportCompleted)
	baseline := rig.CountListens(user.ID)
	if baseline != done1.Counters.Imported {
		t.Fatalf("database holds %d, importer claims %d", baseline, done1.Counters.Imported)
	}

	// Now the account-data export for the same period. Its records have no URIs,
	// so before alias resolution they carry a names-only identity and are stored
	// separately. That is expected and documented; the relink pass converges them.
	j2 := rig.Submit(user.ID, "account data", accountData)
	rig.Drain(rig.Ctx())
	done2 := rig.RequireStatus(j2.ID, domain.ImportCompleted)
	rig.RequireAccounted(done2)
	rig.RequireCommitted(done2)

	unresolved := rig.ScalarInt(
		`SELECT count(*) FROM listens WHERE user_id = $1 AND track_id IS NULL`, user.ID.String())
	if unresolved == 0 {
		t.Fatal("the account-data import should have produced names-only listens awaiting alias resolution")
	}
	aliases := rig.ScalarInt(`SELECT count(*) FROM track_aliases WHERE state = 'pending'`)
	if aliases == 0 {
		t.Fatal("names-only listens must queue their (artist, title) pairs for alias resolution")
	}

	// Resolve every alias to the track the fixture generated it from, exactly as
	// the enrichment worker would, and assert the histories converge with no
	// double counting.
	relinkAllAliases(t, rig)

	if got := rig.ScalarInt(
		`SELECT count(*) FROM listens WHERE user_id = $1 AND track_id IS NULL`, user.ID.String()); got != 0 {
		t.Fatalf("%d listens are still unresolved after relinking", got)
	}
	final := rig.CountListens(user.ID)
	if final != baseline {
		t.Fatalf("after relinking the user has %d listens, want the original %d: "+
			"the two exports describe the same plays and must converge", final, baseline)
	}
}

// --- 3. restarting the worker during an import ------------------------------

func TestWorkerRestartResumesFromCheckpoint(t *testing.T) {
	rig := harness.NewRig(t, func(c *config.Import) { c.BatchSize = 20 })
	user := rig.NewUser("restart")

	path := filepath.Join(t.TempDir(), "Streaming_History_Audio_2015-2017_0.json")
	total := harness.WriteExtendedExport(t, path, harness.GenOptions{Records: 1200, Seed: 11})
	job := rig.Submit(user.ID, "interrupted", path)

	// Kill the worker the moment it has committed some progress but is plainly
	// not finished. Watching the checkpoint makes this deterministic rather than
	// a race against a timer.
	ctx, cancel := context.WithCancel(rig.Ctx())
	go func() {
		defer cancel()
		deadline := time.Now().Add(60 * time.Second)
		for time.Now().Before(deadline) {
			off := rig.ScalarInt(
				`SELECT COALESCE(max(record_offset), 0) FROM import_files WHERE job_id = $1`, job.ID.String())
			if off >= 200 && off < int64(total) {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()

	_, _ = rig.Runner.RunOnce(ctx)

	mid := rig.Job(job.ID)
	if len(mid.Files) != 1 {
		t.Fatalf("expected one file, got %d", len(mid.Files))
	}
	checkpoint := mid.Files[0].RecordOffset
	if checkpoint == 0 {
		t.Fatal("the worker was interrupted before it committed anything; the test proved nothing")
	}
	if checkpoint >= int64(total) {
		t.Skip("the import finished before it could be interrupted; rerun with a larger fixture")
	}
	committedSoFar := rig.CountListens(user.ID)
	if committedSoFar == 0 {
		t.Fatal("no listens survived the interruption")
	}

	// The invariant that makes resume safe: the checkpoint never claims more
	// progress than was actually committed.
	if mid.Files[0].Counters.Processed() != checkpoint {
		t.Fatalf("checkpoint says %d records processed but counters account for %d",
			checkpoint, mid.Files[0].Counters.Processed())
	}

	// A different worker picks the job up. The lease has not expired, so this
	// also exercises the paused-for-shutdown path.
	second := harness.NewRigFor(t, rig.Env, "worker-second", func(c *config.Import) { c.BatchSize = 20 })
	second.Drain(second.Ctx())

	done := second.RequireStatus(job.ID, domain.ImportCompleted)
	second.RequireAccounted(done)
	second.RequireCommitted(done)

	if done.Files[0].RecordOffset != int64(total) {
		t.Fatalf("resumed import processed %d of %d records", done.Files[0].RecordOffset, total)
	}
	if got := rig.CountListens(user.ID); got != done.Counters.Imported {
		t.Fatalf("database holds %d listens, importer claims %d", got, done.Counters.Imported)
	}
	if got := rig.CountListens(user.ID); got < committedSoFar {
		t.Fatalf("resuming lost records: %d before the restart, %d after", committedSoFar, got)
	}

	// And the decisive check: importing the same file again from scratch must
	// add nothing, which proves the resumed run neither lost nor duplicated.
	rerun := rig.Submit(user.ID, "verification rerun", path)
	rig.Drain(rig.Ctx())
	rerunDone := rig.RequireStatus(rerun.ID, domain.ImportCompleted)
	if rerunDone.Counters.Imported != 0 {
		t.Fatalf("re-importing after a resumed run added %d rows; the resume lost records",
			rerunDone.Counters.Imported)
	}
}

// --- 4. database interruption during an import ------------------------------

func TestDatabaseInterruptionDuringImport(t *testing.T) {
	rig := harness.NewRig(t, func(c *config.Import) {
		c.BatchSize = 20
		// One retry, so the interruption reliably escalates to a job failure
		// rather than being silently absorbed. The point of the test is that the
		// failure is recoverable, not that it never happens.
		c.BatchRetries = 1
	})
	user := rig.NewUser("dbdrop")

	path := filepath.Join(t.TempDir(), "Streaming_History_Audio_2015-2017_0.json")
	total := harness.WriteExtendedExport(t, path, harness.GenOptions{Records: 1500, Seed: 13})
	job := rig.Submit(user.ID, "db interruption", path)

	// Terminate Encore's backends the moment the import is under way, which is
	// what a database restart looks like from the application's side.
	ctx := rig.Ctx()
	go func() {
		deadline := time.Now().Add(60 * time.Second)
		for time.Now().Before(deadline) {
			off := rig.ScalarInt(
				`SELECT COALESCE(max(record_offset), 0) FROM import_files WHERE job_id = $1`, job.ID.String())
			if off >= 200 {
				_, _ = rig.Pool.Exec(ctx,
					`SELECT pg_terminate_backend(pid) FROM pg_stat_activity
					 WHERE application_name = 'encore' AND pid <> pg_backend_pid()`)
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()

	_, _ = rig.Runner.RunOnce(ctx)

	mid := rig.Job(job.ID)
	checkpoint := mid.Files[0].RecordOffset
	committed := rig.CountListens(user.ID)

	// Whatever happened, the two must agree: no committed record may sit beyond
	// the checkpoint, and no checkpoint may claim uncommitted work.
	if mid.Files[0].Counters.Imported != rig.CountListensForFile(mid.Files[0].ID) {
		t.Fatalf("after a database interruption the file claims %d imported listens but the database holds %d",
			mid.Files[0].Counters.Imported, rig.CountListensForFile(mid.Files[0].ID))
	}
	t.Logf("interrupted at checkpoint %d with %d committed listens (status %q)",
		checkpoint, committed, mid.Status)

	// Recovery: retry if it failed, otherwise let the loop carry on.
	if mid.Status.Resumable() {
		if err := rig.Imports.RetryJob(rig.Ctx(), rig.Store.DB(), job.ID, user.ID); err != nil {
			t.Fatalf("retry job: %v", err)
		}
	}
	rig.Drain(rig.Ctx())

	done := rig.RequireStatus(job.ID, domain.ImportCompleted)
	rig.RequireAccounted(done)
	rig.RequireCommitted(done)
	if done.Files[0].RecordOffset != int64(total) {
		t.Fatalf("recovered import processed %d of %d records", done.Files[0].RecordOffset, total)
	}

	rerun := rig.Submit(user.ID, "verification rerun", path)
	rig.Drain(rig.Ctx())
	if got := rig.RequireStatus(rerun.ID, domain.ImportCompleted).Counters.Imported; got != 0 {
		t.Fatalf("re-importing after recovery added %d rows; the interruption lost records", got)
	}
}

// --- 5. malformed and partially valid files ---------------------------------

func TestPartiallyValidFileImportsWhatItCan(t *testing.T) {
	rig := harness.NewRig(t, nil)
	user := rig.NewUser("malformed")

	path := filepath.Join(t.TempDir(), "Streaming_History_Audio_2015-2017_0.json")
	total := harness.WriteExtendedExport(t, path, harness.GenOptions{
		Records: 300, Seed: 5, MalformedEvery: 10,
	})

	job := rig.Submit(user.ID, "partially valid", path)
	rig.Drain(rig.Ctx())

	done := rig.RequireStatus(job.ID, domain.ImportCompleted)
	rig.RequireAccounted(done)
	rig.RequireCommitted(done)

	if done.Counters.Rejected == 0 {
		t.Fatal("a file with malformed records should have rejected some")
	}
	if done.Counters.Imported == 0 {
		t.Fatal("the valid records in a partially valid file must still be imported")
	}
	if done.Counters.Processed() != int64(total) {
		t.Fatalf("accounted for %d of %d records", done.Counters.Processed(), total)
	}

	// The diagnostics must be usable: a record index that lines up with the file
	// and a reason a person can act on.
	rejects, count, err := rig.Imports.ListRejects(rig.Ctx(), rig.Store.DB(), done.Files[0].ID, 10, 0)
	if err != nil {
		t.Fatalf("list rejects: %v", err)
	}
	if count != done.Counters.Rejected {
		t.Fatalf("stored %d reject diagnostics for %d rejected records", count, done.Counters.Rejected)
	}
	if len(rejects) == 0 {
		t.Fatal("no reject diagnostics were recorded")
	}
	for _, r := range rejects {
		if r.Reason == "" {
			t.Fatal("a reject was recorded without a reason")
		}
		if r.RecordIndex < 0 || r.RecordIndex >= int64(total) {
			t.Fatalf("reject record index %d is outside the file", r.RecordIndex)
		}
		if r.RawExcerpt == "" {
			t.Fatal("a reject was recorded without an excerpt of the offending record")
		}
	}
}

func TestFileThatIsNotJSONFailsTheJobWithoutLosingProgress(t *testing.T) {
	rig := harness.NewRig(t, nil)
	user := rig.NewUser("notjson")

	dir := t.TempDir()
	// Detected as extended by its name, but the body is truncated part-way
	// through, which is what a half-downloaded export looks like.
	body := `[{"ts":"2020-01-01T10:00:00Z","ms_played":200000,` +
		`"master_metadata_track_name":"A","master_metadata_album_artist_name":"B",` +
		`"spotify_track_uri":"spotify:track:` + harness.TrackID(1) + `"},{"ts":"2020-01-0`
	path := harness.WriteRawJSON(t, dir, "Streaming_History_Audio_2015-2017_0.json", body)

	job := rig.Submit(user.ID, "truncated", path)
	rig.Drain(rig.Ctx())

	failed := rig.RequireStatus(job.ID, domain.ImportFailed)
	if failed.ErrorCode != domain.ErrCodeUnreadable {
		t.Fatalf("error code = %q, want %q", failed.ErrorCode, domain.ErrCodeUnreadable)
	}
	// The valid record before the damage must still have been committed.
	if got := rig.CountListens(user.ID); got != 1 {
		t.Fatalf("database holds %d listens, want the 1 record that was readable", got)
	}
}

// --- 6. several files in one job --------------------------------------------

func TestMultipleFilesInOneJob(t *testing.T) {
	rig := harness.NewRig(t, nil)
	user := rig.NewUser("multifile")

	dir := t.TempDir()
	var paths []string
	expected := 0
	for i := range 4 {
		p := filepath.Join(dir, fmt.Sprintf("Streaming_History_Audio_2015-2017_%d.json", i))
		// Distinct seeds and start years, so the files describe different plays.
		expected += harness.WriteExtendedExport(t, p, harness.GenOptions{
			Records: 200, Seed: uint64(100 + i),
			Start: harness.FixtureStart.AddDate(i, 0, 0),
		})
		paths = append(paths, p)
	}

	job := rig.Submit(user.ID, "four parts", paths...)
	if job.FilesTotal != 4 {
		t.Fatalf("job has %d files, want 4", job.FilesTotal)
	}
	rig.Drain(rig.Ctx())

	done := rig.RequireStatus(job.ID, domain.ImportCompleted)
	rig.RequireAccounted(done)
	rig.RequireCommitted(done)

	if done.Counters.Processed() != int64(expected) {
		t.Fatalf("accounted for %d records across four files, want %d", done.Counters.Processed(), expected)
	}
	if done.FilesDone != 4 {
		t.Fatalf("files_done = %d, want 4", done.FilesDone)
	}
	if got := rig.CountListens(user.ID); got != done.Counters.Imported {
		t.Fatalf("database holds %d listens, importer claims %d", got, done.Counters.Imported)
	}
}

func TestZipArchiveImportSkipsNonHistoryEntries(t *testing.T) {
	rig := harness.NewRig(t, nil)
	user := rig.NewUser("zip")

	path := filepath.Join(t.TempDir(), "my_spotify_data.zip")
	total := harness.WriteZipExport(t, path, harness.GenOptions{Records: 600, Seed: 21}, 3)

	job := rig.Submit(user.ID, "whole export", path)
	if job.FilesTotal != 3 {
		t.Fatalf("job has %d files, want the 3 streaming-history entries "+
			"(playlists and the read-me must be ignored, not imported)", job.FilesTotal)
	}
	rig.Drain(rig.Ctx())

	done := rig.RequireStatus(job.ID, domain.ImportCompleted)
	rig.RequireAccounted(done)
	rig.RequireCommitted(done)
	if done.Counters.Processed() != int64(total) {
		t.Fatalf("accounted for %d records, want %d", done.Counters.Processed(), total)
	}
}

// --- 7. unknown and unavailable Spotify tracks ------------------------------

func TestUnknownTracksAreStoredWithoutCallingSpotify(t *testing.T) {
	rig := harness.NewRig(t, nil)
	user := rig.NewUser("unknown")

	dir := t.TempDir()
	// A track id Spotify has never heard of, a local file, and a record whose
	// URI is missing entirely so only names identify it.
	body := `[
      {"ts":"2020-01-01T10:05:00Z","ms_played":200000,
       "master_metadata_track_name":"Ghost Track","master_metadata_album_artist_name":"Nobody",
       "spotify_track_uri":"spotify:track:zzzzzzzzzzzzzzzzzzzzzz"},
      {"ts":"2020-01-01T10:20:00Z","ms_played":200000,
       "master_metadata_track_name":"Bootleg","master_metadata_album_artist_name":"Nobody",
       "spotify_track_uri":"spotify:local:Nobody:Album:Bootleg:200"},
      {"ts":"2020-01-01T10:35:00Z","ms_played":200000,
       "master_metadata_track_name":"No URI","master_metadata_album_artist_name":"Nobody",
       "spotify_track_uri":null}
    ]`
	path := harness.WriteRawJSON(t, dir, "Streaming_History_Audio_2015-2017_0.json", body)

	job := rig.Submit(user.ID, "unknown tracks", path)
	rig.Drain(rig.Ctx())

	done := rig.RequireStatus(job.ID, domain.ImportCompleted)
	rig.RequireAccounted(done)
	rig.RequireCommitted(done)

	// The unknown id and the URI-less record are both stored; only the local file
	// is skipped, because it has no catalogue identity by definition.
	if done.Counters.Imported != 2 {
		t.Fatalf("imported %d records, want 2 (the unknown id and the names-only record)",
			done.Counters.Imported)
	}
	if done.Counters.Skipped != 1 {
		t.Fatalf("skipped %d records, want 1 (the local file)", done.Counters.Skipped)
	}

	// The unknown id is queued for enrichment rather than resolved inline: no
	// Spotify client was wired into this rig at all, which is the point.
	state := rig.ScalarInt(
		`SELECT count(*) FROM tracks WHERE id = 'zzzzzzzzzzzzzzzzzzzzzz' AND metadata_state = 'pending'`)
	if state != 1 {
		t.Fatal("an unknown track id must be recorded as pending enrichment, not dropped")
	}
	if got := rig.ScalarInt(
		`SELECT count(*) FROM listens WHERE user_id = $1 AND track_id IS NULL`, user.ID.String()); got != 1 {
		t.Fatalf("%d names-only listens, want 1", got)
	}
}

// --- 8. a job that appears complete but has uncommitted records -------------

func TestForgedCompleteJobFailsVerification(t *testing.T) {
	rig := harness.NewRig(t, nil)
	user := rig.NewUser("forged")

	path := filepath.Join(t.TempDir(), "Streaming_History_Audio_2015-2017_0.json")
	harness.WriteExtendedExport(t, path, harness.GenOptions{Records: 200, Seed: 3})

	job := rig.Submit(user.ID, "will be tampered with", path)
	rig.Drain(rig.Ctx())
	done := rig.RequireStatus(job.ID, domain.ImportCompleted)
	if done.Counters.Imported == 0 {
		t.Fatal("nothing was imported, so there is nothing to lose")
	}

	// Simulate the failure this application must never report as a success: the
	// bookkeeping says the records are there, and they are not. A lost
	// transaction, a restore from an older backup, or a hand-edited row all look
	// exactly like this.
	if _, err := rig.Pool.Exec(rig.Ctx(),
		`DELETE FROM listens WHERE import_file_id = $1`, done.Files[0].ID.String()); err != nil {
		t.Fatalf("delete committed listens: %v", err)
	}

	data, err := rig.Imports.VerificationData(rig.Ctx(), rig.Store.DB(), job.ID)
	if err != nil {
		t.Fatalf("verification data: %v", err)
	}
	if err := domain.VerifyJob(data); err == nil {
		t.Fatal("verification passed for a job whose records are not in the database; " +
			"an import must never be reported successful on the strength of its own counters")
	}

	// And the same must hold end to end: retrying re-runs verification and the
	// job must not silently claim success.
	if _, err := rig.Pool.Exec(rig.Ctx(),
		`UPDATE import_files SET status = 'pending' WHERE job_id = $1`, job.ID.String()); err != nil {
		t.Fatalf("reset files: %v", err)
	}
	if _, err := rig.Pool.Exec(rig.Ctx(),
		`UPDATE import_jobs SET status = 'queued', finished_at = NULL WHERE id = $1`, job.ID.String()); err != nil {
		t.Fatalf("requeue job: %v", err)
	}
	rig.Drain(rig.Ctx())

	requeued := rig.Job(job.ID)
	if requeued.Status == domain.ImportCompleted {
		// Completing is only acceptable if the rows genuinely came back.
		rig.RequireCommitted(requeued)
	}
}

// --- 9. cancellation and later resumption -----------------------------------

func TestCancelThenRetryResumesFromCheckpoint(t *testing.T) {
	rig := harness.NewRig(t, func(c *config.Import) { c.BatchSize = 20 })
	user := rig.NewUser("cancel")

	path := filepath.Join(t.TempDir(), "Streaming_History_Audio_2015-2017_0.json")
	total := harness.WriteExtendedExport(t, path, harness.GenOptions{Records: 2000, Seed: 17})
	job := rig.Submit(user.ID, "cancel me", path)

	ctx := rig.Ctx()
	go func() {
		deadline := time.Now().Add(60 * time.Second)
		for time.Now().Before(deadline) {
			off := rig.ScalarInt(
				`SELECT COALESCE(max(record_offset), 0) FROM import_files WHERE job_id = $1`, job.ID.String())
			if off >= 400 {
				if err := rig.Imports.RequestCancel(ctx, rig.Store.DB(), job.ID, user.ID); err != nil {
					t.Errorf("request cancel: %v", err)
				}
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()

	rig.Drain(ctx)

	cancelled := rig.Job(job.ID)
	if cancelled.Status != domain.ImportCancelled {
		t.Skipf("the import finished before cancellation took effect (status %q); "+
			"rerun with a larger fixture", cancelled.Status)
	}
	// Committed records are kept: they are the user's listening history, not
	// scratch state.
	partial := rig.CountListens(user.ID)
	if partial == 0 {
		t.Fatal("cancelling discarded records that had already been committed")
	}
	rig.RequireCommitted(cancelled)

	if err := rig.Imports.RetryJob(rig.Ctx(), rig.Store.DB(), job.ID, user.ID); err != nil {
		t.Fatalf("retry cancelled job: %v", err)
	}
	resumed := rig.Job(job.ID)
	if resumed.Files[0].RecordOffset != cancelled.Files[0].RecordOffset {
		t.Fatalf("retry reset the checkpoint from %d to %d; it must resume, not restart",
			cancelled.Files[0].RecordOffset, resumed.Files[0].RecordOffset)
	}

	rig.Drain(rig.Ctx())
	done := rig.RequireStatus(job.ID, domain.ImportCompleted)
	rig.RequireAccounted(done)
	rig.RequireCommitted(done)
	if done.Files[0].RecordOffset != int64(total) {
		t.Fatalf("resumed import processed %d of %d records", done.Files[0].RecordOffset, total)
	}
	if got := rig.CountListens(user.ID); got < partial {
		t.Fatalf("resuming lost records: %d before, %d after", partial, got)
	}
}

// --- 10. a very large synthetic history in bounded memory -------------------

func TestLargeSyntheticHistoryStaysWithinMemoryBudget(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping the large import in short mode")
	}
	records := 200_000
	if v := os.Getenv("ENCORE_TEST_LARGE_RECORDS"); v != "" {
		if _, err := fmt.Sscanf(v, "%d", &records); err != nil {
			t.Fatalf("ENCORE_TEST_LARGE_RECORDS: %v", err)
		}
	}

	rig := harness.NewRig(t, func(c *config.Import) { c.BatchSize = 1000 })
	user := rig.NewUser("large")

	path := filepath.Join(t.TempDir(), "Streaming_History_Audio_2015-2017_0.json")
	start := time.Now()
	total := harness.WriteExtendedExport(t, path, harness.GenOptions{
		Records: records, Seed: 99, Artists: 400, Tracks: 6000, PodcastEvery: 50,
	})
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat fixture: %v", err)
	}
	t.Logf("generated %d records (%.1f MiB) in %s", total, float64(info.Size())/(1<<20),
		time.Since(start).Round(time.Millisecond))

	job := rig.Submit(user.ID, "large history", path)

	// Sample the heap while the import runs. The claim in docs/import.md is that
	// memory is a function of the batch size and not of the file size, so this
	// must hold for 200,000 records exactly as it does for 200.
	stop := make(chan struct{})
	peak := make(chan uint64, 1)
	go func() {
		var high uint64
		var ms runtime.MemStats
		for {
			select {
			case <-stop:
				peak <- high
				return
			default:
				runtime.ReadMemStats(&ms)
				if ms.HeapAlloc > high {
					high = ms.HeapAlloc
				}
				time.Sleep(20 * time.Millisecond)
			}
		}
	}()

	importStart := time.Now()
	rig.Drain(rig.Ctx())
	elapsed := time.Since(importStart)
	close(stop)
	peakHeap := <-peak

	done := rig.RequireStatus(job.ID, domain.ImportCompleted)
	rig.RequireAccounted(done)
	rig.RequireCommitted(done)

	if done.Counters.Processed() != int64(total) {
		t.Fatalf("accounted for %d of %d records", done.Counters.Processed(), total)
	}
	rows := rig.CountListens(user.ID)
	if rows != done.Counters.Imported {
		t.Fatalf("database holds %d listens, importer claims %d", rows, done.Counters.Imported)
	}

	const budget = 256 << 20
	t.Logf("imported %d records in %s (%.0f records/s), peak heap %.1f MiB, %d rows committed",
		total, elapsed.Round(time.Millisecond), float64(total)/elapsed.Seconds(),
		float64(peakHeap)/(1<<20), rows)
	if peakHeap > budget {
		t.Fatalf("peak heap %.1f MiB exceeded the documented %d MiB budget",
			float64(peakHeap)/(1<<20), budget>>20)
	}
}

// relinkAllAliases stands in for the enrichment worker: it resolves every
// pending alias to the catalogue track the fixture generated it from and applies
// the relink pass, so the convergence behaviour can be tested without Spotify.
//
// The fixture names tracks "Track %06d", and normalisation only lowercases, so
// the number in the alias title is enough to recover the track id. That keeps
// the test deterministic and independent of any search behaviour.
func relinkAllAliases(t *testing.T, rig *harness.Rig) {
	t.Helper()

	type alias struct{ artist, title string }
	rows, err := rig.Pool.Query(rig.Ctx(),
		`SELECT artist_norm, title_norm FROM track_aliases WHERE state = 'pending'`)
	if err != nil {
		t.Fatalf("list aliases: %v", err)
	}
	var aliases []alias
	for rows.Next() {
		var a alias
		if err := rows.Scan(&a.artist, &a.title); err != nil {
			rows.Close()
			t.Fatalf("scan alias: %v", err)
		}
		aliases = append(aliases, a)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("list aliases: %v", err)
	}
	if len(aliases) == 0 {
		t.Fatal("no aliases were queued, so there is nothing to converge")
	}

	for _, a := range aliases {
		var trackNum int
		if _, err := fmt.Sscanf(a.title, "track %d", &trackNum); err != nil {
			t.Fatalf("alias title %q does not look like a fixture track name: %v", a.title, err)
		}
		trackID := harness.TrackID(trackNum)

		identity := domain.TrackIdentity{Artist: a.artist, Title: a.title}
		unresolved, err := rig.Listens.UnresolvedListensForIdentity(
			rig.Ctx(), rig.Store.DB(), identity.Key(), 0, 100_000)
		if err != nil {
			t.Fatalf("list unresolved listens: %v", err)
		}
		if len(unresolved) > 0 {
			if err := rig.Store.InTx(rig.Ctx(), func(ctx context.Context, tx pgx.Tx) error {
				_, err := rig.Listens.ApplyRelink(ctx, tx, unresolved, trackID, "UTC")
				return err
			}); err != nil {
				t.Fatalf("apply relink: %v", err)
			}
		}
		if err := rig.Catalog.ResolveAlias(rig.Ctx(), rig.Store.DB(),
			domain.AliasKey{ArtistNorm: a.artist, TitleNorm: a.title}, trackID); err != nil {
			t.Fatalf("resolve alias: %v", err)
		}
	}
}
