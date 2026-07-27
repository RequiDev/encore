package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"

	"github.com/RequiDev/encore/internal/config"
	"github.com/RequiDev/encore/internal/domain"
	"github.com/RequiDev/encore/internal/importer"
	"github.com/RequiDev/encore/internal/logging"
	"github.com/RequiDev/encore/internal/store"
	"github.com/RequiDev/encore/internal/store/accounts"
	"github.com/RequiDev/encore/internal/store/imports"
	"github.com/RequiDev/encore/internal/store/listens"
)

// defaultMaxHeapMB is the documented target from docs/import.md: the import
// worker stays below 256 MiB for any input size at the default batch size.
// Exceeding it fails the run, which is what makes the sentence in the design
// document a claim rather than an aspiration.
const defaultMaxHeapMB = 256

// jobPollInterval is how long the driver waits before asking again when nothing
// was claimable.
const jobPollInterval = 500 * time.Millisecond

// maxIdleRounds bounds that wait. Something else holding the lease is a real
// possibility on a shared development database and must be reported, not waited
// on for ever.
const maxIdleRounds = 60

// runOptions are the command line of `encore-bench run`.
type runOptions struct {
	Records   int
	Format    domain.ImportFormat
	BatchSize int
	Seed      uint64
	File      string
	ReportTo  string
	Keep      bool
	MaxHeapMB int
}

// runBenchmark implements `encore-bench run`.
func runBenchmark(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	records := fs.Int("records", 1_000_000, "number of records to generate and import")
	format := fs.String("format", string(domain.FormatExtended), "extended | account_data")
	batchSize := fs.Int("batch-size", 1000, "records per transaction (ENCORE_IMPORT_BATCH_SIZE)")
	seed := fs.Uint64("seed", 1, "seed for the deterministic generator")
	file := fs.String("file", "", "import this export instead of generating one")
	report := fs.String("report", "", "also write the report as JSON to this path")
	keep := fs.Bool("keep", false, "keep the generated dataset, the user and the imported rows")
	maxHeap := fs.Int("max-heap-mb", defaultMaxHeapMB, "fail the run if peak heap exceeds this many MiB")
	if err := fs.Parse(args); err != nil {
		return err
	}
	parsed, err := parseFormat(*format)
	if err != nil {
		return err
	}
	if *file != "" {
		set := map[string]bool{}
		fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
		for _, ignored := range []string{"records", "seed"} {
			if set[ignored] {
				fmt.Fprintf(os.Stderr, "encore-bench: --%s is ignored because --file was given\n", ignored)
			}
		}
		*records = 0
	}

	opts := runOptions{
		Records:   *records,
		Format:    parsed,
		BatchSize: *batchSize,
		Seed:      *seed,
		File:      *file,
		ReportTo:  *report,
		Keep:      *keep,
		MaxHeapMB: *maxHeap,
	}

	rep, err := benchmark(ctx, opts)
	if rep != nil {
		if err := rep.WriteTable(os.Stdout); err != nil {
			return err
		}
		if opts.ReportTo != "" {
			if werr := rep.WriteJSON(opts.ReportTo); werr != nil {
				return werr
			}
			fmt.Printf("report written to %s\n", opts.ReportTo)
		}
		if opts.Keep && rep.Job.UserID != "" {
			fmt.Printf("kept for inspection: dataset %s\n"+
				"  encore-bench verify --user %s\n"+
				"  remove it with: psql -c \"DELETE FROM users WHERE id = '%s'\"\n",
				rep.Dataset.Path, rep.Job.UserID, rep.Job.UserID)
		}
	}
	if err != nil {
		return err
	}
	if !rep.Passed {
		// The failures are already in the table; this is the non-zero exit.
		return errors.New("benchmark failed: " + rep.Failures[0])
	}
	return nil
}

// benchmark runs one measurement end to end.
//
// The import goes through internal/importer's own Intake and Runner, which is
// the point of the exercise: the numbers are only worth reporting if they came
// from the same code an uploaded export goes through, leases, checkpoints,
// batching, retries and post-import verification included.
func benchmark(ctx context.Context, opts runOptions) (*Report, error) {
	if opts.Records <= 0 && opts.File == "" {
		return nil, errors.New("--records must be at least 1")
	}
	if opts.BatchSize <= 0 {
		return nil, errors.New("--batch-size must be at least 1")
	}
	dsn := os.Getenv("ENCORE_DATABASE_URL")
	if dsn == "" {
		return nil, errors.New("ENCORE_DATABASE_URL is required")
	}

	lg := logging.New(logging.Options{
		Level:   envOr("ENCORE_LOG_LEVEL", "info"),
		Format:  envOr("ENCORE_LOG_FORMAT", "text"),
		Service: "bench",
		Version: version,
	})

	if err := requireSchema(ctx, dsn); err != nil {
		return nil, err
	}

	rep := &Report{
		Version:   version,
		StartedAt: time.Now().UTC(),
		Host:      currentHost(),
		Settings: benchSettings{
			Format:      string(opts.Format),
			Records:     opts.Records,
			BatchSize:   opts.BatchSize,
			MinMsPlayed: minMsPlayed,
			Seed:        opts.Seed,
			MaxHeapMB:   opts.MaxHeapMB,
			Generated:   opts.File == "",
			Keep:        opts.Keep,
		},
	}

	sampler := startMemorySampler()
	defer sampler.Stop()

	// Everything the run creates on disk is removed again unless --keep, whether
	// or not the run succeeds.
	var scratch []string
	defer func() {
		if opts.Keep {
			return
		}
		for _, dir := range scratch {
			if err := os.RemoveAll(dir); err != nil {
				lg.Warn("could not remove a scratch directory", "dir", dir, logging.Err(err))
			}
		}
	}()

	// --- the dataset --------------------------------------------------------

	var generateElapsed time.Duration
	if opts.File != "" {
		info, err := os.Stat(opts.File)
		if err != nil {
			return nil, fmt.Errorf("read --file: %w", err)
		}
		rep.Dataset = datasetStats{Path: opts.File, Format: string(opts.Format), Bytes: info.Size()}
	} else {
		dir, err := os.MkdirTemp("", "encore-bench-data-")
		if err != nil {
			return nil, fmt.Errorf("create a working directory: %w", err)
		}
		scratch = append(scratch, dir)

		started := time.Now()
		stats, err := writeExportFile(filepath.Join(dir, datasetName(opts.Format)), generateOptions{
			Records: opts.Records,
			Format:  opts.Format,
			Seed:    opts.Seed,
		})
		if err != nil {
			return nil, err
		}
		generateElapsed = time.Since(started)
		rep.Dataset = stats
		lg.Info("dataset generated",
			"records", stats.Records, "bytes", stats.Bytes,
			"elapsed", humanDuration(generateElapsed))
	}

	// --- configuration ------------------------------------------------------

	importDir := os.Getenv("ENCORE_IMPORT_DIR")
	if importDir == "" {
		dir, err := os.MkdirTemp("", "encore-bench-spool-")
		if err != nil {
			return nil, fmt.Errorf("create a spool directory: %w", err)
		}
		scratch = append(scratch, dir)
		importDir = dir
	}

	cfg := config.Import{
		Dir:       importDir,
		BatchSize: opts.BatchSize,
		// The upload cap is an API-surface concern rather than something the
		// importer is being measured on, so it is raised to fit whatever dataset
		// was asked for instead of turning a large benchmark into a 413.
		MaxUploadBytes:    max(4<<30, rep.Dataset.Bytes+(1<<20)),
		MinMsPlayed:       minMsPlayed,
		MaxRejectsPerFile: 1000,
		Workers:           1,
		LeaseTTL:          60 * time.Second,
		BatchRetries:      6,
		RetainFiles:       true,
	}

	// --- database and repositories ------------------------------------------

	pool, st, err := openDatabase(ctx, dsn, lg)
	if err != nil {
		return nil, err
	}
	defer pool.Close()

	jobs := imports.New(st)
	accountRepo := accounts.New(st)
	listenRepo := listens.New(st)

	user, err := createBenchUser(ctx, st, accountRepo)
	if err != nil {
		return nil, err
	}
	rep.Job.UserID = user.ID.String()

	// createdAfter bounds the catalogue rows this run is allowed to delete
	// afterwards. It is taken before anything is written, so no row the run
	// created can fall outside it.
	createdAfter := time.Now().UTC().Add(-time.Minute)

	intake, err := importer.NewIntake(cfg, st, jobs, lg)
	if err != nil {
		return nil, err
	}

	defer func() {
		if opts.Keep {
			return
		}
		// Cleanup must survive a Ctrl-C: leaving a million rows and a spooled
		// export behind because someone interrupted the run would be worse than
		// the interruption.
		tidyUp(context.WithoutCancel(ctx), lg, st, intake, accountRepo, rep.Job.ID, user.ID, createdAfter)
	}()

	// --- intake -------------------------------------------------------------

	f, err := os.Open(rep.Dataset.Path)
	if err != nil {
		return nil, fmt.Errorf("open the dataset: %w", err)
	}
	spoolStarted := time.Now()
	job, warnings, err := intake.Create(ctx, user.ID, "encore-bench", []importer.Upload{{
		Filename: filepath.Base(rep.Dataset.Path),
		Body:     f,
	}})
	spoolElapsed := time.Since(spoolStarted)
	_ = f.Close()
	if err != nil {
		return nil, fmt.Errorf("queue the import: %w", err)
	}
	for _, w := range warnings {
		lg.Warn("intake warning", "file", w.File, "code", w.Code, "message", w.Message)
	}
	rep.Job.ID = job.ID.String()
	rep.Job.Files = job.FilesTotal
	if len(job.Files) > 0 {
		// What the file actually is, as the importer detected it, rather than what
		// --format claimed. They differ whenever --file names someone else's export.
		rep.Dataset.Format = string(job.Files[0].Format)
	}

	// --- the import ---------------------------------------------------------

	stats := newCollector()
	runner, err := importer.NewRunner(cfg, "encore-bench-"+uuid.NewString(), importer.Deps{
		Store:    st,
		Jobs:     jobs,
		Listens:  listenRepo,
		Accounts: accountRepo,
		Logger:   lg,
		Metrics:  stats,
	})
	if err != nil {
		return nil, err
	}

	// Only the import is measured. Generating the dataset is this tool's own
	// work and has no business inflating the peak it reports for the importer.
	sampler.Reset()
	importStarted := time.Now()
	final, runErr := driveJob(ctx, runner, jobs, st, job.ID)
	importElapsed := time.Since(importStarted)
	rep.Memory = sampler.Snapshot()
	rep.Batches = stats.Snapshot()
	if runErr != nil {
		return nil, runErr
	}

	// --- what actually happened ---------------------------------------------

	rep.Counters = final.Counters
	rep.Job.Status = string(final.Status)
	rep.Job.ErrorCode = final.ErrorCode
	rep.Job.ErrorMessage = final.ErrorMessage
	rep.Job.Files = final.FilesTotal

	// The importer verifies before it declares a job complete. Verifying again
	// here is not redundant: this is the benchmark deciding for itself, from the
	// database, rather than believing the status column.
	data, err := jobs.VerificationData(ctx, st.DB(), job.ID)
	if err != nil {
		return nil, fmt.Errorf("read verification data: %w", err)
	}
	if verr := domain.VerifyJob(data); verr != nil {
		var ve *domain.VerificationError
		if errors.As(verr, &ve) {
			rep.Job.VerificationProblems = ve.Problems
		} else {
			rep.Job.VerificationProblems = []string{verr.Error()}
		}
	} else {
		rep.Job.Verified = true
	}

	if rep.Job.ListensCommitted, err = countListensForJob(ctx, st.DB(), job.ID); err != nil {
		return nil, err
	}
	if rep.Database, err = readRowCounts(ctx, st.DB(), user.ID); err != nil {
		return nil, err
	}

	// A supplied file's record count is only known once it has been read.
	if rep.Dataset.Records == 0 {
		rep.Dataset.Records = rep.Counters.Processed()
	}

	rep.Timing = timings{
		GenerateSeconds:  generateElapsed.Seconds(),
		SpoolSeconds:     spoolElapsed.Seconds(),
		ImportSeconds:    importElapsed.Seconds(),
		RecordsPerSecond: perSecond(rep.Counters.Processed(), importElapsed),
		RowsPerSecond:    perSecond(rep.Counters.Imported, importElapsed),
		BytesPerSecond:   perSecond(rep.Dataset.Bytes, importElapsed),
	}

	evaluate(rep, opts.MaxHeapMB)
	return rep, nil
}

// driveJob runs the worker until the benchmark's own job stops moving.
//
// RunOnce is the entry point encore-worker's own loop calls, so the import takes
// exactly the path a production one does. The loop around it exists because a
// Runner claims whichever job is next in the queue, which on a shared
// development database need not be this one.
func driveJob(
	ctx context.Context,
	runner *importer.Runner,
	jobs *imports.Repo,
	st *store.Store,
	jobID uuid.UUID,
) (domain.ImportJob, error) {
	var idle int
	for {
		job, err := jobs.GetJob(ctx, st.DB(), jobID)
		if err != nil {
			return domain.ImportJob{}, fmt.Errorf("read the import job: %w", err)
		}
		if job.Status.Terminal() {
			return job, nil
		}
		if err := ctx.Err(); err != nil {
			return job, fmt.Errorf("interrupted while %s: %w", job.Status, err)
		}

		worked, err := runner.RunOnce(ctx)
		if err != nil {
			return job, fmt.Errorf("run the import worker: %w", err)
		}
		if worked {
			idle = 0
			continue
		}
		idle++
		if idle > maxIdleRounds {
			return job, fmt.Errorf(
				"the import job is %q and nothing will claim it; is another encore-worker running against this database?",
				job.Status)
		}
		select {
		case <-ctx.Done():
			return job, ctx.Err()
		case <-time.After(jobPollInterval):
		}
	}
}

// createBenchUser creates the throwaway account the import belongs to.
//
// Listens are owned by a user and the dedupe key is user-scoped, so a benchmark
// needs one. A fresh identity per run means two runs against the same database
// never suppress each other's records as duplicates. On an empty database this
// account is the first and therefore becomes the administrator, which is
// harmless: it has no Spotify credential and cannot be signed in to, and unless
// --keep is given it is deleted again at the end of the run.
func createBenchUser(ctx context.Context, st *store.Store, repo *accounts.Repo) (domain.User, error) {
	user, _, err := repo.Users.UpsertFromSpotify(ctx, st.DB(), accounts.SpotifyProfile{
		SpotifyUserID: "encore-bench-" + uuid.NewString(),
		DisplayName:   "Encore benchmark",
	}, "UTC", true)
	if err != nil {
		return domain.User{}, fmt.Errorf("create the benchmark user: %w", err)
	}
	return user, nil
}

// tidyUp removes everything the run created. Failures are logged rather than
// returned: the measurement has already been taken, and a benchmark that hides
// its own results because it could not delete a temporary file would be a poor
// trade.
func tidyUp(
	ctx context.Context,
	lg *slog.Logger,
	st *store.Store,
	intake *importer.Intake,
	repo *accounts.Repo,
	jobID string,
	userID uuid.UUID,
	createdAfter time.Time,
) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	if id, err := uuid.Parse(jobID); err == nil {
		// Before the job rows go, while they still say where the bytes are.
		if err := intake.RemoveJobFiles(ctx, id); err != nil {
			lg.Warn("could not remove the spooled export", logging.Err(err))
		}
	}
	// Deleting the user cascades to their listens, import jobs, files and
	// rejects, which is every row the benchmark owns.
	if err := repo.Users.DeleteUser(ctx, st.DB(), userID); err != nil {
		lg.Warn("could not remove the benchmark user", logging.Err(err))
		return
	}
	removed, err := removeOrphanedCatalogue(ctx, st.DB(), createdAfter)
	if err != nil {
		lg.Warn("could not remove the synthetic catalogue rows", logging.Err(err))
		return
	}
	lg.Info("benchmark data removed", "catalogue_rows", removed)
}

// datasetName gives a generated file the name Spotify would have given it, so
// that the importer's name-based detection is exercised alongside its
// content-based detection.
func datasetName(format domain.ImportFormat) string {
	if format == domain.FormatAccountData {
		return "StreamingHistory0.json"
	}
	return "Streaming_History_Audio_0.json"
}
