package harness

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/requi/encore/internal/config"
	"github.com/requi/encore/internal/domain"
	"github.com/requi/encore/internal/importer"
)

// Rig is an Env plus a real importer: the same Intake the HTTP handler uses and
// the same Runner the worker process runs.
//
// The tests drive the production code path rather than a test-only shortcut,
// because the behaviour under examination — checkpointing, resume, verification
// — lives in that path and nowhere else.
type Rig struct {
	*Env
	Cfg    config.Import
	Intake *importer.Intake
	Runner *importer.Runner
}

// DefaultImportConfig is a small-batch configuration. The batch size is
// deliberately tiny so a modest fixture still produces many checkpoints, which
// is what makes the interruption tests meaningful.
func DefaultImportConfig(dir string) config.Import {
	return config.Import{
		Dir:               dir,
		BatchSize:         50,
		MaxUploadBytes:    4 << 30,
		MinMsPlayed:       1000,
		MaxRejectsPerFile: 1000,
		Workers:           1,
		LeaseTTL:          10 * time.Second,
		BatchRetries:      3,
		RetainFiles:       true,
	}
}

// NewRig builds an import rig. mutate may adjust the configuration before the
// importer is constructed.
func NewRig(t *testing.T, mutate func(*config.Import)) *Rig {
	t.Helper()
	env := New(t)
	return NewRigFor(t, env, "worker-test", mutate)
}

// NewRigFor builds a second rig over an existing Env, which is how the
// resume tests get a *different* worker to pick up an abandoned job.
func NewRigFor(t *testing.T, env *Env, workerID string, mutate func(*config.Import)) *Rig {
	t.Helper()
	cfg := DefaultImportConfig(filepath.Join(env.Dir, "imports"))
	if mutate != nil {
		mutate(&cfg)
	}
	if err := os.MkdirAll(cfg.Dir, 0o750); err != nil {
		t.Fatalf("create import dir: %v", err)
	}

	intake, err := importer.NewIntake(cfg, env.Store, env.Imports, Discard())
	if err != nil {
		t.Fatalf("build intake: %v", err)
	}
	runner, err := importer.NewRunner(cfg, workerID, importer.Deps{
		Store:    env.Store,
		Jobs:     env.Imports,
		Listens:  env.Listens,
		Accounts: env.Accounts,
		Logger:   Discard(),
	})
	if err != nil {
		t.Fatalf("build runner: %v", err)
	}
	return &Rig{Env: env, Cfg: cfg, Intake: intake, Runner: runner}
}

// Submit uploads files exactly as the HTTP handler would: as streams.
func (r *Rig) Submit(userID uuid.UUID, note string, paths ...string) domain.ImportJob {
	r.T.Helper()

	uploads := make([]importer.Upload, 0, len(paths))
	closers := make([]*os.File, 0, len(paths))
	defer func() {
		for _, f := range closers {
			_ = f.Close()
		}
	}()

	for _, p := range paths {
		f, err := os.Open(p)
		if err != nil {
			r.T.Fatalf("open fixture %s: %v", p, err)
		}
		closers = append(closers, f)
		uploads = append(uploads, importer.Upload{Filename: filepath.Base(p), Body: f})
	}

	job, _, err := r.Intake.Create(r.Ctx(), userID, note, uploads)
	if err != nil {
		r.T.Fatalf("submit import: %v", err)
	}
	return job
}

// SubmitExpectingError is Submit for the cases where creation should be refused.
func (r *Rig) SubmitExpectingError(userID uuid.UUID, paths ...string) ([]importer.Warning, error) {
	r.T.Helper()
	uploads := make([]importer.Upload, 0, len(paths))
	for _, p := range paths {
		f, err := os.Open(p)
		if err != nil {
			r.T.Fatalf("open fixture %s: %v", p, err)
		}
		defer f.Close()
		uploads = append(uploads, importer.Upload{Filename: filepath.Base(p), Body: f})
	}
	_, warnings, err := r.Intake.Create(r.Ctx(), userID, "", uploads)
	return warnings, err
}

// SubmitWithWarnings returns both the job and any warnings raised.
func (r *Rig) SubmitWithWarnings(userID uuid.UUID, paths ...string) (domain.ImportJob, []importer.Warning) {
	r.T.Helper()
	uploads := make([]importer.Upload, 0, len(paths))
	for _, p := range paths {
		f, err := os.Open(p)
		if err != nil {
			r.T.Fatalf("open fixture %s: %v", p, err)
		}
		defer f.Close()
		uploads = append(uploads, importer.Upload{Filename: filepath.Base(p), Body: f})
	}
	job, warnings, err := r.Intake.Create(r.Ctx(), userID, "", uploads)
	if err != nil {
		r.T.Fatalf("submit import: %v", err)
	}
	return job, warnings
}

// Drain claims and runs jobs until there is nothing left to do.
func (r *Rig) Drain(ctx context.Context) {
	r.T.Helper()
	for i := 0; i < 1000; i++ {
		worked, err := r.Runner.RunOnce(ctx)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return
			}
			r.T.Fatalf("run import: %v", err)
		}
		if !worked {
			return
		}
	}
	r.T.Fatal("import loop did not settle after 1000 iterations")
}

// Job reloads a job with its files and aggregate counters.
func (r *Rig) Job(id uuid.UUID) domain.ImportJob {
	r.T.Helper()
	job, err := r.Imports.GetJob(r.Ctx(), r.Store.DB(), id)
	if err != nil {
		r.T.Fatalf("load job: %v", err)
	}
	return job
}

// RequireStatus fails the test unless the job reached the expected status,
// quoting the recorded error so a failure explains itself.
func (r *Rig) RequireStatus(id uuid.UUID, want domain.ImportStatus) domain.ImportJob {
	r.T.Helper()
	job := r.Job(id)
	if job.Status != want {
		r.T.Fatalf("job status = %q, want %q (code=%q message=%q)",
			job.Status, want, job.ErrorCode, job.ErrorMessage)
	}
	return job
}

// RequireCommitted asserts that the number of rows actually in the fact table
// matches the number the importer claims it inserted.
//
// Every import test ends here. The counters are the thing under test, so they
// are never allowed to be the evidence as well.
func (r *Rig) RequireCommitted(job domain.ImportJob) {
	r.T.Helper()
	var claimed int64
	for _, f := range job.Files {
		claimed += f.Counters.Imported
		if got := r.CountListensForFile(f.ID); got != f.Counters.Imported {
			r.T.Fatalf("file %q claims %d imported listens but the database holds %d",
				f.Name, f.Counters.Imported, got)
		}
	}
	if claimed != job.Counters.Imported {
		r.T.Fatalf("job counters say %d imported but its files sum to %d", job.Counters.Imported, claimed)
	}
}

// RequireAccounted asserts the counter identity every file must satisfy:
// imported + duplicates + skipped + rejected equals the records processed.
func (r *Rig) RequireAccounted(job domain.ImportJob) {
	r.T.Helper()
	for _, f := range job.Files {
		if got, want := f.Counters.Processed(), f.RecordOffset; got != want {
			r.T.Fatalf("file %q: counters account for %d records but the checkpoint says %d were processed "+
				"(imported=%d duplicates=%d skipped=%d rejected=%d)",
				f.Name, got, want, f.Counters.Imported, f.Counters.Duplicates, f.Counters.Skipped, f.Counters.Rejected)
		}
		if f.RecordsTotal != nil && f.RecordOffset != *f.RecordsTotal {
			r.T.Fatalf("file %q processed %d of %d records", f.Name, f.RecordOffset, *f.RecordsTotal)
		}
	}
}
