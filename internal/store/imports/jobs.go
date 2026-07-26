// Package imports is the durability layer for Encore's importer: the jobs a user
// starts, the files those jobs contain, the checkpoints that make them
// resumable, and the per-record diagnostics they produce.
//
// One rule governs everything here. A checkpoint is only ever written in the
// same transaction as the batch of listens it describes, so "committed records
// is less than or equal to the checkpoint" is exactly true at every instant.
// Nothing in this package advances a checkpoint on its own, and Checkpoint
// refuses to move one backwards, which is what makes re-claiming a crashed
// worker's job a completely ordinary operation rather than a repair procedure.
package imports

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/requi/encore/internal/domain"
	"github.com/requi/encore/internal/postgres"
	"github.com/requi/encore/internal/store"
)

// Repo is the imports repository.
type Repo struct{ db *store.Store }

// New builds the repository.
func New(db *store.Store) *Repo { return &Repo{db: db} }

// scanner is satisfied by both pgx.Row and pgx.Rows, so a row can be decoded by
// the same function whether it came from a QueryRow or from an iteration.
type scanner interface {
	Scan(dest ...any) error
}

// Paging bounds. A caller that asks for everything gets a page instead: these
// lists are shown in a UI, and an import history can be arbitrarily long.
const (
	// DefaultPageSize is used when a caller passes a non-positive limit.
	DefaultPageSize = 20
	// MaxPageSize caps a caller-supplied limit.
	MaxPageSize = 200
)

// clampPage normalises caller-supplied paging arguments.
func clampPage(limit, offset int) (int, int) {
	if limit <= 0 {
		limit = DefaultPageSize
	}
	if limit > MaxPageSize {
		limit = MaxPageSize
	}
	if offset < 0 {
		offset = 0
	}
	return limit, offset
}

const jobColumns = `id, user_id, status, note, created_at, started_at, finished_at,
        error_code, error_message, lease_owner, lease_expires_at, cancel_requested,
        files_total, files_done`

// scanJob decodes one import_jobs row. Enumerated columns are read as plain
// strings and converted here, so a value written by an older release cannot make
// a scan fail.
func scanJob(row scanner) (domain.ImportJob, error) {
	var (
		j      domain.ImportJob
		status string
	)
	err := row.Scan(
		&j.ID, &j.UserID, &status, &j.Note, &j.CreatedAt, &j.StartedAt, &j.FinishedAt,
		&j.ErrorCode, &j.ErrorMessage, &j.LeaseOwner, &j.LeaseExpiresAt, &j.CancelRequested,
		&j.FilesTotal, &j.FilesDone,
	)
	if err != nil {
		return domain.ImportJob{}, err
	}
	j.Status = domain.ImportStatus(status)
	return j, nil
}

// sumCounters aggregates per-file tallies into the job-level totals.
//
// The job row deliberately stores no counters of its own: a single source of
// truth per file means a resumed or retried job can never disagree with itself.
func sumCounters(files []domain.ImportFile) domain.Counters {
	var total domain.Counters
	for _, f := range files {
		total.Add(f.Counters)
	}
	return total
}

// NewFile describes one streaming-history file being added to a new job. It is
// the upload side of domain.ImportFile: the storage path is carried here but is
// never returned to the user, since where the instance spooled the bytes is an
// operational detail.
type NewFile struct {
	// Name is what the user uploaded, or the archive entry's base name.
	Name string
	// ContainerPath is the entry path when the file was found inside a .zip.
	ContainerPath string
	Format        domain.ImportFormat
	SizeBytes     int64
	// SHA256 is the digest of the file's bytes, used only to warn about a
	// re-upload. It may be nil when the digest was not computed.
	SHA256 []byte
	// StoragePath is where the worker will find the spooled bytes.
	StoragePath string
}

// normalise fills in defaults and rejects anything the database or the worker
// would choke on later, while the caller can still report a useful error.
func (f NewFile) normalise() (NewFile, error) {
	f.Name = strings.TrimSpace(f.Name)
	if f.Name == "" {
		return f, fmt.Errorf("%w: import file needs a name", domain.ErrValidation)
	}
	if f.Format == "" {
		f.Format = domain.FormatUnknown
	}
	if !f.Format.Valid() {
		return f, fmt.Errorf("%w: unknown import format %q for %s", domain.ErrValidation, f.Format, f.Name)
	}
	if f.SizeBytes < 0 {
		return f, fmt.Errorf("%w: import file %s has a negative size", domain.ErrValidation, f.Name)
	}
	if n := len(f.SHA256); n != 0 && n != 32 {
		return f, fmt.Errorf("%w: import file %s has a %d-byte digest, want 32", domain.ErrValidation, f.Name, n)
	}
	if strings.TrimSpace(f.StoragePath) == "" {
		// Without this the job would be claimable but unreadable, and would fail
		// only once a worker picked it up.
		return f, fmt.Errorf("%w: import file %s has no storage path", domain.ErrValidation, f.Name)
	}
	return f, nil
}

// normaliseFiles validates a whole upload before anything is written.
func normaliseFiles(files []NewFile) ([]NewFile, error) {
	if len(files) == 0 {
		// A job with nothing in it would sit in the queue, be claimed, and
		// complete having done nothing. The caller reports domain.ErrCodeEmptyUpload
		// to the user instead.
		return nil, fmt.Errorf("%w: an import job needs at least one file", domain.ErrValidation)
	}
	out := make([]NewFile, len(files))
	for i, f := range files {
		n, err := f.normalise()
		if err != nil {
			return nil, err
		}
		out[i] = n
	}
	return out, nil
}

const insertJobSQL = `
    INSERT INTO import_jobs (user_id, note, files_total)
    VALUES ($1, $2, $3)
    RETURNING ` + jobColumns

const insertFilesSQL = `
    INSERT INTO import_files (job_id, ordinal, name, container_path, format, size_bytes, sha256, storage_path)
    SELECT $1, t.ordinal, t.name, t.container_path, t.format, t.size_bytes, t.sha256, t.storage_path
    FROM unnest($2::int[], $3::text[], $4::text[], $5::text[], $6::bigint[], $7::bytea[], $8::text[])
        AS t(ordinal, name, container_path, format, size_bytes, sha256, storage_path)
    RETURNING ` + fileColumns

// CreateJob records a new import and every file it covers.
//
// It takes a transaction rather than a Querier because a job whose files were
// only half written would be claimed by a worker and would import a truncated
// history without ever reporting a problem. Ordinals are assigned from zero in
// the order the caller supplied, which is the order the worker will process them.
func (r *Repo) CreateJob(ctx context.Context, tx pgx.Tx, userID uuid.UUID, note string, files []NewFile) (domain.ImportJob, error) {
	normalised, err := normaliseFiles(files)
	if err != nil {
		return domain.ImportJob{}, err
	}

	job, err := scanJob(tx.QueryRow(ctx, insertJobSQL, store.UUIDArg(userID), note, len(normalised)))
	if err != nil {
		return domain.ImportJob{}, postgres.Classify("create import job", err)
	}

	n := len(normalised)
	var (
		ordinals   = make([]int32, n)
		names      = make([]string, n)
		containers = make([]string, n)
		formats    = make([]string, n)
		sizes      = make([]int64, n)
		shas       = make([][]byte, n)
		paths      = make([]string, n)
	)
	for i, f := range normalised {
		ordinals[i] = int32(i)
		names[i] = f.Name
		containers[i] = f.ContainerPath
		formats[i] = string(f.Format)
		sizes[i] = f.SizeBytes
		shas[i] = f.SHA256
		paths[i] = f.StoragePath
	}

	rows, err := tx.Query(ctx, insertFilesSQL,
		store.UUIDArg(job.ID), ordinals, names, containers, formats, sizes, shas, paths)
	if err != nil {
		return domain.ImportJob{}, postgres.Classify("create import files", err)
	}
	defer rows.Close()

	for rows.Next() {
		f, err := scanFile(rows)
		if err != nil {
			return domain.ImportJob{}, postgres.Classify("scan import file", err)
		}
		job.Files = append(job.Files, f)
	}
	if err := rows.Err(); err != nil {
		return domain.ImportJob{}, postgres.Classify("create import files", err)
	}
	// RETURNING makes no promise about row order, and the caller depends on the
	// ordinals it asked for.
	sort.Slice(job.Files, func(a, b int) bool { return job.Files[a].Ordinal < job.Files[b].Ordinal })
	job.Counters = sumCounters(job.Files)
	return job, nil
}

const selectJobSQL = `SELECT ` + jobColumns + ` FROM import_jobs WHERE id = $1`

// GetJob loads a job with its files and the counters summed from them.
func (r *Repo) GetJob(ctx context.Context, q store.Querier, id uuid.UUID) (domain.ImportJob, error) {
	job, err := scanJob(q.QueryRow(ctx, selectJobSQL, store.UUIDArg(id)))
	if err != nil {
		return domain.ImportJob{}, postgres.Classify("get import job", err)
	}
	if err := r.attachFiles(ctx, q, []*domain.ImportJob{&job}); err != nil {
		return domain.ImportJob{}, err
	}
	return job, nil
}

const selectJobForUserSQL = `SELECT ` + jobColumns + ` FROM import_jobs WHERE id = $1 AND user_id = $2`

// GetJobForUser loads a job that belongs to the given user.
//
// A job owned by somebody else is reported as domain.ErrNotFound rather than as
// a forbidden access, so that the endpoint cannot be used to discover which job
// ids exist on the instance.
func (r *Repo) GetJobForUser(ctx context.Context, q store.Querier, id, userID uuid.UUID) (domain.ImportJob, error) {
	job, err := scanJob(q.QueryRow(ctx, selectJobForUserSQL, store.UUIDArg(id), store.UUIDArg(userID)))
	if err != nil {
		return domain.ImportJob{}, postgres.Classify("get import job", err)
	}
	if err := r.attachFiles(ctx, q, []*domain.ImportJob{&job}); err != nil {
		return domain.ImportJob{}, err
	}
	return job, nil
}

const listJobsForUserSQL = `
    SELECT ` + jobColumns + `
    FROM import_jobs
    WHERE user_id = $1
    ORDER BY created_at DESC, id DESC
    LIMIT $2 OFFSET $3`

// ListJobsForUser returns one page of a user's import history, newest first,
// along with the unpaged total so the caller can render a pager.
func (r *Repo) ListJobsForUser(ctx context.Context, q store.Querier, userID uuid.UUID, limit, offset int) ([]domain.ImportJob, int64, error) {
	limit, offset = clampPage(limit, offset)

	var total int64
	err := q.QueryRow(ctx, `SELECT count(*)::bigint FROM import_jobs WHERE user_id = $1`,
		store.UUIDArg(userID)).Scan(&total)
	if err != nil {
		return nil, 0, postgres.Classify("count import jobs", err)
	}
	if total == 0 {
		return nil, 0, nil
	}

	rows, err := q.Query(ctx, listJobsForUserSQL, store.UUIDArg(userID), limit, offset)
	if err != nil {
		return nil, 0, postgres.Classify("list import jobs", err)
	}
	defer rows.Close()

	var jobs []domain.ImportJob
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, 0, postgres.Classify("scan import job", err)
		}
		jobs = append(jobs, j)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, postgres.Classify("list import jobs", err)
	}

	refs := make([]*domain.ImportJob, len(jobs))
	for i := range jobs {
		refs[i] = &jobs[i]
	}
	if err := r.attachFiles(ctx, q, refs); err != nil {
		return nil, 0, err
	}
	return jobs, total, nil
}

// attachFiles loads the files for a page of jobs in a single round trip and
// derives each job's aggregate counters from them.
func (r *Repo) attachFiles(ctx context.Context, q store.Querier, jobs []*domain.ImportJob) error {
	if len(jobs) == 0 {
		return nil
	}
	ids := make([]uuid.UUID, len(jobs))
	byID := make(map[uuid.UUID]*domain.ImportJob, len(jobs))
	for i, j := range jobs {
		ids[i] = j.ID
		byID[j.ID] = j
	}

	const sql = `
        SELECT ` + fileColumns + `
        FROM import_files
        WHERE job_id = ANY($1::uuid[])
        ORDER BY job_id, ordinal`
	rows, err := q.Query(ctx, sql, store.UUIDArgs(ids))
	if err != nil {
		return postgres.Classify("list import files", err)
	}
	defer rows.Close()

	for rows.Next() {
		f, err := scanFile(rows)
		if err != nil {
			return postgres.Classify("scan import file", err)
		}
		if j, ok := byID[f.JobID]; ok {
			j.Files = append(j.Files, f)
		}
	}
	if err := rows.Err(); err != nil {
		return postgres.Classify("list import files", err)
	}
	for _, j := range jobs {
		j.Counters = sumCounters(j.Files)
	}
	return nil
}

const setJobStatusSQL = `
    UPDATE import_jobs
    SET status = $2,
        error_code = $3,
        error_message = $4,
        started_at = CASE WHEN $2 = 'running' AND started_at IS NULL THEN now() ELSE started_at END,
        finished_at = CASE WHEN $2 IN ('completed', 'failed', 'cancelled') THEN now() ELSE finished_at END,
        lease_owner = CASE WHEN $2 IN ('completed', 'failed', 'cancelled') THEN '' ELSE lease_owner END,
        lease_expires_at = CASE WHEN $2 IN ('completed', 'failed', 'cancelled') THEN NULL ELSE lease_expires_at END
    WHERE id = $1`

// SetJobStatus moves a job to a new state and records the accompanying error
// code and message, which are empty for a successful transition.
//
// started_at is stamped the first time the job runs and never rewritten, so a
// resumed job still reports when the user's import actually began. A terminal
// status also drops the lease: nothing may claim the job again, and a stale
// owner in the row would only mislead whoever reads it.
func (r *Repo) SetJobStatus(ctx context.Context, q store.Querier, id uuid.UUID, status domain.ImportStatus, errCode, errMessage string) error {
	if !status.Valid() {
		return fmt.Errorf("%w: unknown import status %q", domain.ErrValidation, status)
	}
	tag, err := q.Exec(ctx, setJobStatusSQL, store.UUIDArg(id), string(status), errCode, errMessage)
	if err != nil {
		return postgres.Classify("set import job status", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("set import job status: %w", domain.ErrNotFound)
	}
	return nil
}

const requestCancelSQL = `
    UPDATE import_jobs
    SET cancel_requested = true
    WHERE id = $1 AND user_id = $2 AND status IN ('queued', 'running', 'paused')
    RETURNING id`

// RequestCancel asks the worker to stop.
//
// This only raises a flag. The worker observes it at a batch boundary and stops
// after committing the batch in flight, so the checkpoint and the records it
// describes stay in agreement and the job remains resumable.
func (r *Repo) RequestCancel(ctx context.Context, q store.Querier, id, userID uuid.UUID) error {
	var got uuid.UUID
	err := q.QueryRow(ctx, requestCancelSQL, store.UUIDArg(id), store.UUIDArg(userID)).Scan(&got)
	if err == nil {
		return nil
	}
	classified := postgres.Classify("request import cancel", err)
	if !errors.Is(classified, domain.ErrNotFound) {
		return classified
	}
	// The guarded update matched nothing. Look again without the status guard so
	// that "already finished" is reported as a conflict rather than as a 404.
	status, sErr := jobStatusForUser(ctx, q, id, userID)
	if sErr != nil {
		return sErr
	}
	return fmt.Errorf("%w: import job is %s and can no longer be cancelled", domain.ErrConflict, status)
}

// IsCancelRequested reports whether the user has asked for the job to stop. The
// worker polls it at batch boundaries.
func (r *Repo) IsCancelRequested(ctx context.Context, q store.Querier, id uuid.UUID) (bool, error) {
	var requested bool
	err := q.QueryRow(ctx, `SELECT cancel_requested FROM import_jobs WHERE id = $1`,
		store.UUIDArg(id)).Scan(&requested)
	if err != nil {
		return false, postgres.Classify("read import cancel flag", err)
	}
	return requested, nil
}

const retryJobSQL = `
    WITH j AS (
        UPDATE import_jobs
        SET status = 'queued',
            cancel_requested = false,
            error_code = '',
            error_message = '',
            finished_at = NULL,
            lease_owner = '',
            lease_expires_at = NULL,
            files_done = (
                SELECT count(*) FROM import_files x
                WHERE x.job_id = import_jobs.id AND x.status IN ('completed', 'skipped'))
        WHERE id = $1 AND user_id = $2 AND status IN ('failed', 'cancelled', 'paused')
        RETURNING id
    ),
    f AS (
        UPDATE import_files
        SET status = 'pending',
            error_code = '',
            error_message = '',
            finished_at = NULL
        WHERE job_id IN (SELECT id FROM j) AND status IN ('failed', 'running')
        RETURNING id
    )
    SELECT count(*)::bigint FROM j`

// RetryJob puts a stopped job back in the queue so a worker picks it up again.
//
// Files that were failed or still marked running are returned to pending, but
// their record_offset, byte_offset and counters are left exactly as they were.
// That is the whole point: the retry resumes from the checkpoint instead of
// re-reading the file from the beginning, and because insertion is idempotent
// the records between the checkpoint and the failure are simply re-derived.
// Completed and skipped files are not touched at all.
func (r *Repo) RetryJob(ctx context.Context, q store.Querier, id, userID uuid.UUID) error {
	var n int64
	if err := q.QueryRow(ctx, retryJobSQL, store.UUIDArg(id), store.UUIDArg(userID)).Scan(&n); err != nil {
		return postgres.Classify("retry import job", err)
	}
	if n == 0 {
		status, err := jobStatusForUser(ctx, q, id, userID)
		if err != nil {
			return err
		}
		return fmt.Errorf("%w: import job is %s and cannot be retried", domain.ErrConflict, status)
	}
	return nil
}

// DeleteJob removes a job and, by cascade, its files and reject diagnostics.
//
// The listens the job imported are kept: the schema nulls their import_file_id
// instead of cascading, because the listening data belongs to the user while the
// job record is only bookkeeping.
func (r *Repo) DeleteJob(ctx context.Context, q store.Querier, id, userID uuid.UUID) error {
	tag, err := q.Exec(ctx, `DELETE FROM import_jobs WHERE id = $1 AND user_id = $2`,
		store.UUIDArg(id), store.UUIDArg(userID))
	if err != nil {
		return postgres.Classify("delete import job", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("delete import job: %w", domain.ErrNotFound)
	}
	return nil
}

// jobStatusForUser answers "was the job missing, or was it simply in the wrong
// state?" after a guarded update matched nothing.
func jobStatusForUser(ctx context.Context, q store.Querier, id, userID uuid.UUID) (domain.ImportStatus, error) {
	var status string
	err := q.QueryRow(ctx, `SELECT status FROM import_jobs WHERE id = $1 AND user_id = $2`,
		store.UUIDArg(id), store.UUIDArg(userID)).Scan(&status)
	if err != nil {
		return "", postgres.Classify("get import job", err)
	}
	return domain.ImportStatus(status), nil
}
