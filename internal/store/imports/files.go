package imports

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/RequiDev/encore/internal/domain"
	"github.com/RequiDev/encore/internal/postgres"
	"github.com/RequiDev/encore/internal/store"
)

const fileColumns = `id, job_id, ordinal, name, container_path, format, size_bytes, sha256,
        status, records_total, record_offset, byte_offset,
        imported, duplicates, skipped, rejected,
        error_code, error_message, started_at, finished_at`

// scanFile decodes one import_files row. storage_path is deliberately absent:
// the path the instance spooled the upload to is an operational detail and never
// travels with the file as it is rendered to a user.
func scanFile(row scanner) (domain.ImportFile, error) {
	var (
		f              domain.ImportFile
		format, status string
	)
	err := row.Scan(
		&f.ID, &f.JobID, &f.Ordinal, &f.Name, &f.ContainerPath, &format, &f.SizeBytes, &f.SHA256,
		&status, &f.RecordsTotal, &f.RecordOffset, &f.ByteOffset,
		&f.Counters.Imported, &f.Counters.Duplicates, &f.Counters.Skipped, &f.Counters.Rejected,
		&f.ErrorCode, &f.ErrorMessage, &f.StartedAt, &f.FinishedAt,
	)
	if err != nil {
		return domain.ImportFile{}, err
	}
	f.Format = domain.ImportFormat(format)
	f.Status = domain.ImportFileStatus(status)
	return f, nil
}

// ListFiles returns a job's files in the order the worker processes them.
func (r *Repo) ListFiles(ctx context.Context, q store.Querier, jobID uuid.UUID) ([]domain.ImportFile, error) {
	const sql = `SELECT ` + fileColumns + ` FROM import_files WHERE job_id = $1 ORDER BY ordinal`
	rows, err := q.Query(ctx, sql, store.UUIDArg(jobID))
	if err != nil {
		return nil, postgres.Classify("list import files", err)
	}
	defer rows.Close()

	var out []domain.ImportFile
	for rows.Next() {
		f, err := scanFile(rows)
		if err != nil {
			return nil, postgres.Classify("scan import file", err)
		}
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		return nil, postgres.Classify("list import files", err)
	}
	return out, nil
}

// FileByID loads a single file, including its checkpoint.
func (r *Repo) FileByID(ctx context.Context, q store.Querier, fileID uuid.UUID) (domain.ImportFile, error) {
	const sql = `SELECT ` + fileColumns + ` FROM import_files WHERE id = $1`
	f, err := scanFile(q.QueryRow(ctx, sql, store.UUIDArg(fileID)))
	if err != nil {
		return domain.ImportFile{}, postgres.Classify("get import file", err)
	}
	return f, nil
}

// StoragePath returns where the worker will find the spooled bytes for a file.
// It is a separate lookup because the path is never part of the file as it is
// shown to a user.
func (r *Repo) StoragePath(ctx context.Context, q store.Querier, fileID uuid.UUID) (string, error) {
	var path string
	err := q.QueryRow(ctx, `SELECT storage_path FROM import_files WHERE id = $1`,
		store.UUIDArg(fileID)).Scan(&path)
	if err != nil {
		return "", postgres.Classify("get import file storage path", err)
	}
	return path, nil
}

const startFileSQL = `
    UPDATE import_files
    SET status = 'running',
        started_at = COALESCE(started_at, now()),
        finished_at = NULL,
        error_code = '',
        error_message = ''
    WHERE id = $1`

// StartFile marks a file as being read.
//
// started_at is preserved across resumes so the elapsed time a user sees covers
// the whole import rather than restarting with the worker, and any error from a
// previous attempt is cleared because the file is being worked on again.
func (r *Repo) StartFile(ctx context.Context, q store.Querier, fileID uuid.UUID) error {
	tag, err := q.Exec(ctx, startFileSQL, store.UUIDArg(fileID))
	if err != nil {
		return postgres.Classify("start import file", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("start import file: %w", domain.ErrNotFound)
	}
	return nil
}

const checkpointSQL = `
    UPDATE import_files
    SET record_offset = $2,
        byte_offset = $3,
        imported = imported + $4,
        duplicates = duplicates + $5,
        skipped = skipped + $6,
        rejected = rejected + $7
    WHERE id = $1 AND record_offset < $2`

// Checkpoint advances a file's position and adds a batch's counters in one
// statement, and reports whether the update applied.
//
// This is the function the whole import design rests on. It takes a transaction
// because it must run in the same one as the batch of listens it describes: that
// is what makes "committed records is less than or equal to the checkpoint"
// exactly true, and therefore what makes a crash recoverable by simply resuming.
//
// The guard is strictly less-than, not less-than-or-equal, and that matters.
// Every batch advances the offset by at least one record, so a legitimate
// checkpoint always moves it forward. Requiring strict progress therefore makes
// the statement idempotent: if a transaction commits but its acknowledgement is
// lost and the batch is retried, the second attempt matches no row and the
// counters are added once rather than twice. It equally stops a stale call — a
// retry that overtook itself, or a worker whose lease was stolen mid-batch —
// winding the checkpoint backwards and causing records to be re-imported or, far
// worse, skipped.
//
// When applied is false the caller must abandon the transaction rather than
// commit a batch whose position was never recorded.
//
// byteOffset is written exactly as given, including nil. Keeping a previous byte
// offset alongside a newer record offset would describe a position that does not
// exist, and a resume from it would silently misalign the two.
func (r *Repo) Checkpoint(ctx context.Context, tx pgx.Tx, fileID uuid.UUID, recordOffset int64, byteOffset *int64, delta domain.Counters) (applied bool, err error) {
	if recordOffset < 0 {
		return false, fmt.Errorf("%w: record offset must not be negative", domain.ErrValidation)
	}
	if delta.Imported < 0 || delta.Duplicates < 0 || delta.Skipped < 0 || delta.Rejected < 0 {
		// Counters only ever grow; a negative delta would be a bookkeeping bug
		// that post-import verification could not distinguish from lost records.
		return false, fmt.Errorf("%w: checkpoint counters must not be negative", domain.ErrValidation)
	}

	tag, err := tx.Exec(ctx, checkpointSQL, store.UUIDArg(fileID), recordOffset, byteOffset,
		delta.Imported, delta.Duplicates, delta.Skipped, delta.Rejected)
	if err != nil {
		return false, postgres.Classify("checkpoint import file", err)
	}
	return tag.RowsAffected() == 1, nil
}

// terminateFileSQL moves a file to a terminal status and refreshes the owning
// job's files_done counter in the same statement.
//
// The count excludes the file being changed and adds one for it, because every
// part of a statement sees the same snapshot: a plain count would still observe
// this file's old status and would report one file too few.
const terminateFileSQL = `
    WITH f AS (
        UPDATE import_files
        SET status = $2,
            finished_at = now(),
            records_total = COALESCE($3::bigint, records_total),
            error_code = $4,
            error_message = $5
        WHERE id = $1
        RETURNING id, job_id
    )
    UPDATE import_jobs j
    SET files_done = 1 + (
            SELECT count(*) FROM import_files x
            WHERE x.job_id = j.id AND x.id <> f.id
              AND x.status IN ('completed', 'skipped', 'failed'))
    FROM f
    WHERE j.id = f.job_id`

// terminateFile is the shared body of FinishFile, FailFile and SkipFile.
func terminateFile(ctx context.Context, q store.Querier, op string, fileID uuid.UUID, status domain.ImportFileStatus, recordsTotal *int64, errCode, errMessage string) error {
	tag, err := q.Exec(ctx, terminateFileSQL, store.UUIDArg(fileID), string(status), recordsTotal, errCode, errMessage)
	if err != nil {
		return postgres.Classify(op, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%s: %w", op, domain.ErrNotFound)
	}
	return nil
}

// FinishFile records that a file was read to the end.
//
// recordsTotal is the number of records the file actually contained, which is
// only known at EOF; storing it is what lets verification assert that the
// checkpoint reached the end rather than merely stopping without an error.
func (r *Repo) FinishFile(ctx context.Context, q store.Querier, fileID uuid.UUID, recordsTotal int64) error {
	if recordsTotal < 0 {
		return fmt.Errorf("%w: records total must not be negative", domain.ErrValidation)
	}
	return terminateFile(ctx, q, "finish import file", fileID, domain.FileCompleted, &recordsTotal, "", "")
}

// FailFile records a per-file failure with a stable code the frontend can map to
// help text. The checkpoint is left intact so a retry resumes from it.
func (r *Repo) FailFile(ctx context.Context, q store.Querier, fileID uuid.UUID, errCode, errMessage string) error {
	return terminateFile(ctx, q, "fail import file", fileID, domain.FileFailed, nil, errCode, errMessage)
}

// SkipFile records that a file was intentionally not imported: an archive entry
// that is not streaming history at all, or an exact duplicate of an earlier
// upload.
//
// The reason is stored in the file's message but no error code is set, because a
// skip is a normal outcome and verification treats it as a success.
func (r *Repo) SkipFile(ctx context.Context, q store.Querier, fileID uuid.UUID, reason string) error {
	return terminateFile(ctx, q, "skip import file", fileID, domain.FileSkipped, nil, "", reason)
}

const fileSHAExistsSQL = `
    SELECT f.job_id, f.name
    FROM import_files f
    JOIN import_jobs j ON j.id = f.job_id
    WHERE j.user_id = $1 AND f.sha256 = $2 AND f.status = 'completed'
    ORDER BY j.created_at DESC
    LIMIT 1`

// FileSHAExists reports whether this user has already imported a file with
// exactly these bytes, so the upload endpoint can warn before doing the work.
//
// Only a completed file counts. A file that failed half way through is one the
// user most likely wants to try again, and warning about it would be actively
// unhelpful. The warning is advisory in any case: re-importing is harmless
// because the dedupe keys, not this digest, are what guarantee idempotency.
func (r *Repo) FileSHAExists(ctx context.Context, q store.Querier, userID uuid.UUID, sha []byte) (jobID *uuid.UUID, fileName string, found bool, err error) {
	if len(sha) == 0 {
		return nil, "", false, nil
	}
	var (
		id   uuid.UUID
		name string
	)
	err = q.QueryRow(ctx, fileSHAExistsSQL, store.UUIDArg(userID), sha).Scan(&id, &name)
	if err != nil {
		classified := postgres.Classify("look up imported file digest", err)
		if !errors.Is(classified, domain.ErrNotFound) {
			return nil, "", false, classified
		}
		// No match is the ordinary case, not a failure.
		return nil, "", false, nil
	}
	return &id, name, true, nil
}

const verificationDataSQL = `
    SELECT f.id, f.name, f.status, f.record_offset, f.records_total,
           f.imported, f.duplicates, f.skipped, f.rejected,
           count(l.id)::bigint AS listens_in_database
    FROM import_files f
    LEFT JOIN listens l ON l.import_file_id = f.id
    WHERE f.job_id = $1
    GROUP BY f.id
    ORDER BY f.ordinal`

// VerificationData gathers the evidence domain.VerifyJob needs before a job may
// be reported as completed.
//
// listens_in_database is a real count over the fact table rather than the
// importer's running tally, and that is the entire point of the check: a batch
// whose transaction was lost shows up here as a shortfall, and the job is failed
// instead of being declared a success it never achieved.
func (r *Repo) VerificationData(ctx context.Context, q store.Querier, jobID uuid.UUID) ([]domain.FileVerification, error) {
	rows, err := q.Query(ctx, verificationDataSQL, store.UUIDArg(jobID))
	if err != nil {
		return nil, postgres.Classify("collect import verification data", err)
	}
	defer rows.Close()

	var out []domain.FileVerification
	for rows.Next() {
		var (
			v      domain.FileVerification
			status string
		)
		err := rows.Scan(
			&v.FileID, &v.Name, &status, &v.RecordOffset, &v.RecordsTotal,
			&v.Counters.Imported, &v.Counters.Duplicates, &v.Counters.Skipped, &v.Counters.Rejected,
			&v.ListensInDatabase,
		)
		if err != nil {
			return nil, postgres.Classify("scan import verification data", err)
		}
		v.Status = domain.ImportFileStatus(status)
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, postgres.Classify("collect import verification data", err)
	}
	return out, nil
}

// StoredFile is an imported file whose uploaded bytes are still on disk.
type StoredFile struct {
	ID            uuid.UUID
	Name          string
	ContainerPath string
	Format        domain.ImportFormat
	StoragePath   string
}

// AllFilesWithStorage lists every import file whose upload was retained,
// regardless of which job or user it belongs to.
//
// It exists for maintenance that reads back what was imported — recovering track
// names that a previous version of the importer discarded, for instance —
// rather than for anything a request can reach, so it is deliberately not
// scoped to a user.
func (r *Repo) AllFilesWithStorage(ctx context.Context, q store.Querier) ([]StoredFile, error) {
	const sql = `
        SELECT id, name, container_path, format, storage_path
        FROM import_files
        WHERE storage_path <> '' AND status IN ('completed', 'running', 'pending')
        ORDER BY job_id, ordinal`
	rows, err := q.Query(ctx, sql)
	if err != nil {
		return nil, postgres.Classify("list stored import files", err)
	}
	defer rows.Close()

	var out []StoredFile
	for rows.Next() {
		var f StoredFile
		var format string
		if err := rows.Scan(&f.ID, &f.Name, &f.ContainerPath, &format, &f.StoragePath); err != nil {
			return nil, postgres.Classify("scan stored import file", err)
		}
		f.Format = domain.ImportFormat(format)
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		return nil, postgres.Classify("list stored import files", err)
	}
	return out, nil
}
