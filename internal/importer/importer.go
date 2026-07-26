// Package importer runs historical import jobs.
//
// The design and its guarantees are documented in docs/import.md. The two that
// everything else follows from:
//
//  1. A batch of listens and the checkpoint describing it are written in the same
//     database transaction, so committed records can never exceed the checkpoint
//     and a crash resumes rather than restarts.
//  2. Ingestion never calls the Spotify API. Catalogue metadata is filled in
//     later by internal/enrich, so an outage or a rate limit cannot lose or delay
//     a listening record.
package importer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"

	"github.com/requi/encore/internal/config"
	"github.com/requi/encore/internal/domain"
	"github.com/requi/encore/internal/logging"
	"github.com/requi/encore/internal/store"
	"github.com/requi/encore/internal/store/accounts"
	"github.com/requi/encore/internal/store/imports"
	"github.com/requi/encore/internal/store/listens"
)

// Metrics receives import telemetry. It is an interface so that the importer
// does not depend on Prometheus; cmd wires the real collector in, and tests use
// the zero value.
type Metrics interface {
	ImportRecords(format, outcome string, n int)
	ImportBatch(result string, d time.Duration)
	ImportBytesRead(n int64)
	ImportThroughput(recordsPerSecond float64)
	ImportJobStatus(status string, delta int)
}

// NopMetrics discards telemetry.
type NopMetrics struct{}

func (NopMetrics) ImportRecords(string, string, int) {}
func (NopMetrics) ImportBatch(string, time.Duration) {}
func (NopMetrics) ImportBytesRead(int64)             {}
func (NopMetrics) ImportThroughput(float64)          {}
func (NopMetrics) ImportJobStatus(string, int)       {}

// Deps are the collaborators a Runner needs.
type Deps struct {
	Store    *store.Store
	Jobs     *imports.Repo
	Listens  *listens.Repo
	Accounts *accounts.Repo
	Logger   *slog.Logger
	Metrics  Metrics
	// Now is injectable so tests can control timestamps without sleeping.
	Now func() time.Time
}

// Runner claims import jobs and processes them to completion.
type Runner struct {
	cfg  config.Import
	dep  Deps
	id   string
	now  func() time.Time
	stat Metrics
	log  *slog.Logger
}

// errCancelled unwinds a job whose user asked for it to stop. It is not a
// failure, so it never reaches a log line as an error.
var errCancelled = errors.New("import cancelled by user")

// errLeaseLost unwinds a job whose lease was taken by another worker. Stopping
// immediately is the only safe response: continuing would write over the other
// worker's progress.
var errLeaseLost = errors.New("import lease lost to another worker")

// NewRunner builds a Runner. workerID identifies this process in job leases and
// must be distinct across worker containers.
func NewRunner(cfg config.Import, workerID string, dep Deps) (*Runner, error) {
	if dep.Store == nil || dep.Jobs == nil || dep.Listens == nil {
		return nil, errors.New("importer: store, jobs and listens repositories are required")
	}
	if workerID == "" {
		return nil, errors.New("importer: a worker id is required")
	}
	if dep.Logger == nil {
		dep.Logger = slog.Default()
	}
	if dep.Metrics == nil {
		dep.Metrics = NopMetrics{}
	}
	if dep.Now == nil {
		dep.Now = time.Now
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 1000
	}
	return &Runner{
		cfg:  cfg,
		dep:  dep,
		id:   workerID,
		now:  dep.Now,
		stat: dep.Metrics,
		log:  dep.Logger.With("component", "importer", "worker", workerID),
	}, nil
}

// Run claims and processes jobs until ctx is cancelled.
//
// The loop is intentionally simple: claim one job, run it to completion, look
// again. Everything that makes a job survivable — leases, checkpoints,
// idempotent inserts — lives in the database, so a Runner holds no state worth
// recovering and can be killed at any instant.
func (r *Runner) Run(ctx context.Context) error {
	idle := time.NewTimer(0)
	if !idle.Stop() {
		<-idle.C
	}
	defer idle.Stop()

	for {
		if err := ctx.Err(); err != nil {
			return nil
		}
		worked, err := r.RunOnce(ctx)
		switch {
		case err != nil && ctx.Err() != nil:
			return nil
		case err != nil:
			// A failure to claim is an infrastructure problem, not a job problem;
			// back off rather than spinning against a database that is down.
			r.log.Error("import loop failed", logging.Err(err))
			if !sleepCtx(ctx, 5*time.Second) {
				return nil
			}
		case !worked:
			if !sleepCtx(ctx, 2*time.Second) {
				return nil
			}
		}
	}
}

// RunOnce claims at most one job and processes it. It reports whether there was
// anything to do, which is what lets Run distinguish "idle" from "busy".
func (r *Runner) RunOnce(ctx context.Context) (bool, error) {
	job, err := r.dep.Jobs.ClaimJob(ctx, r.dep.Store.DB(), r.id, r.cfg.LeaseTTL)
	if err != nil {
		return false, fmt.Errorf("claim import job: %w", err)
	}
	if job == nil {
		return false, nil
	}

	log := r.log.With("job", job.ID.String(), "user", job.UserID.String())
	log.Info("import job claimed", "files", job.FilesTotal)
	r.stat.ImportJobStatus("running", 1)

	start := r.now()
	err = r.processJob(ctx, job, log)
	elapsed := r.now().Sub(start)

	r.stat.ImportJobStatus("running", -1)
	r.finalise(ctx, job, err, elapsed, log)

	// The job itself failing is not a loop failure: the next job may be fine.
	return true, nil
}

// finalise records the job's terminal state. It runs even when ctx is already
// cancelled, because a job whose worker is shutting down must be left in a
// state another worker can pick up rather than stuck as 'running'.
func (r *Runner) finalise(ctx context.Context, job *domain.ImportJob, runErr error, elapsed time.Duration, log *slog.Logger) {
	ctx = context.WithoutCancel(ctx)
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	db := r.dep.Store.DB()

	switch {
	case runErr == nil:
		// Only now, after every file is done, is the job allowed to claim success
		// — and only if the database agrees with the counters. See verify.go.
		if err := r.verify(ctx, job, log); err != nil {
			return
		}
		if err := r.dep.Jobs.SetJobStatus(ctx, db, job.ID, domain.ImportCompleted, "", ""); err != nil {
			log.Error("could not record job completion", logging.Err(err))
			return
		}
		log.Info("import job completed", "elapsed", elapsed.Round(time.Millisecond).String())
		r.cleanupIfRequested(ctx, job, log)

	case errors.Is(runErr, errCancelled):
		if err := r.dep.Jobs.SetJobStatus(ctx, db, job.ID, domain.ImportCancelled, "", ""); err != nil {
			log.Error("could not record job cancellation", logging.Err(err))
		}
		log.Info("import job cancelled by user")

	case errors.Is(runErr, errLeaseLost):
		// Another worker owns the job now. Touch nothing: it will finish it.
		log.Warn("import lease lost; another worker has taken over")

	case errors.Is(runErr, context.Canceled), errors.Is(runErr, context.DeadlineExceeded):
		// The process is shutting down mid-job. Park it as paused so it is
		// claimable immediately rather than waiting for the lease to expire.
		if err := r.dep.Jobs.SetJobStatus(ctx, db, job.ID, domain.ImportPaused, "", "Worker shut down; the import will resume automatically."); err != nil {
			log.Error("could not park job for resume", logging.Err(err))
		}
		log.Info("import job paused for shutdown; it will resume from its checkpoint")

	default:
		code, message := classifyJobError(runErr)
		if err := r.dep.Jobs.SetJobStatus(ctx, db, job.ID, domain.ImportFailed, code, message); err != nil {
			log.Error("could not record job failure", logging.Err(err))
		}
		log.Error("import job failed", "code", code, logging.Err(runErr))
	}

	_ = r.dep.Jobs.ReleaseLease(ctx, db, job.ID, r.id)
}

// processJob walks the job's files, holding the lease alive while it does.
func (r *Runner) processJob(ctx context.Context, job *domain.ImportJob, log *slog.Logger) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	leaseLost := make(chan struct{})
	go r.heartbeat(ctx, job.ID, leaseLost, cancel, log)

	files, err := r.dep.Jobs.ListFiles(ctx, r.dep.Store.DB(), job.ID)
	if err != nil {
		return fmt.Errorf("list import files: %w", err)
	}
	if len(files) == 0 {
		return &jobError{code: domain.ErrCodeEmptyUpload, message: "The upload contained no streaming-history files."}
	}

	timezone, err := r.userTimezone(ctx, job.UserID)
	if err != nil {
		return err
	}

	for _, file := range files {
		if file.Status == domain.FileCompleted || file.Status == domain.FileSkipped {
			continue
		}
		if err := r.processFile(ctx, job, file, timezone, log); err != nil {
			select {
			case <-leaseLost:
				return errLeaseLost
			default:
			}
			return err
		}
	}
	return nil
}

// heartbeat renews the lease and, if it has been stolen, cancels the job's
// context so the worker stops writing.
func (r *Runner) heartbeat(ctx context.Context, jobID uuid.UUID, lost chan<- struct{}, cancel context.CancelFunc, log *slog.Logger) {
	interval := r.cfg.LeaseTTL / 3
	if interval < time.Second {
		interval = time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			held, err := r.dep.Jobs.Heartbeat(ctx, r.dep.Store.DB(), jobID, r.id, r.cfg.LeaseTTL)
			if err != nil {
				// A transient database problem is not proof the lease is gone;
				// the next tick will tell us, and the lease outlives one miss.
				log.Warn("import heartbeat failed", logging.Err(err))
				continue
			}
			if !held {
				close(lost)
				cancel()
				return
			}
		}
	}
}

// userTimezone resolves the owning user's timezone, which decides which local
// day each inserted listen marks dirty for statistics rollups.
func (r *Runner) userTimezone(ctx context.Context, userID uuid.UUID) (string, error) {
	if r.dep.Accounts == nil || r.dep.Accounts.Users == nil {
		return "UTC", nil
	}
	user, err := r.dep.Accounts.Users.GetByID(ctx, r.dep.Store.DB(), userID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return "", &jobError{code: domain.ErrCodeInternal, message: "The account that started this import no longer exists."}
		}
		return "", fmt.Errorf("load importing user: %w", err)
	}
	if user.Timezone == "" {
		return "UTC", nil
	}
	return user.Timezone, nil
}

// cleanupIfRequested deletes a completed job's spooled uploads when the operator
// has asked for them not to be retained.
func (r *Runner) cleanupIfRequested(ctx context.Context, job *domain.ImportJob, log *slog.Logger) {
	if r.cfg.RetainFiles {
		return
	}
	files, err := r.dep.Jobs.ListFiles(ctx, r.dep.Store.DB(), job.ID)
	if err != nil {
		log.Warn("could not list files for cleanup", logging.Err(err))
		return
	}
	seen := map[string]struct{}{}
	for _, f := range files {
		path, err := r.dep.Jobs.StoragePath(ctx, r.dep.Store.DB(), f.ID)
		if err != nil || path == "" {
			continue
		}
		if _, done := seen[path]; done {
			continue
		}
		seen[path] = struct{}{}
		if !withinImportDir(r.cfg.Dir, path) {
			// Never delete outside the configured import directory, whatever the
			// database says: a corrupted path must not become a file deletion.
			continue
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			log.Warn("could not remove imported file", "path", filepath.Base(path), logging.Err(err))
		}
	}
}

// withinImportDir guards every filesystem removal.
func withinImportDir(dir, path string) bool {
	if dir == "" {
		return false
	}
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return false
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(absDir, absPath)
	if err != nil {
		return false
	}
	return rel != ".." && !hasDotDotPrefix(rel) && !filepath.IsAbs(rel)
}

func hasDotDotPrefix(rel string) bool {
	return len(rel) >= 3 && rel[0] == '.' && rel[1] == '.' && (rel[2] == '/' || rel[2] == '\\')
}

// jobError is a failure that should be reported to the user with a stable code.
type jobError struct {
	code    string
	message string
	err     error
}

func (e *jobError) Error() string {
	if e.err != nil {
		return fmt.Sprintf("%s: %s: %v", e.code, e.message, e.err)
	}
	return fmt.Sprintf("%s: %s", e.code, e.message)
}

func (e *jobError) Unwrap() error { return e.err }

// classifyJobError turns any error into the stable code and the human-readable
// message stored on the job. Anything unrecognised becomes a deliberately vague
// internal error, because an unexpected error's text may contain detail that
// belongs in the log rather than in the user interface.
func classifyJobError(err error) (code, message string) {
	var je *jobError
	if errors.As(err, &je) {
		return je.code, je.message
	}
	if domain.IsTransient(err) {
		return domain.ErrCodeRetriesExhausted,
			"The database was unreachable for too long. The import stopped at its last checkpoint; retrying will continue from there."
	}
	return domain.ErrCodeInternal,
		"The import stopped because of an unexpected error. The import stopped at its last checkpoint; retrying will continue from there."
}

// sleepCtx waits for d, reporting false if ctx ended first.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
