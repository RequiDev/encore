package domain

import (
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ImportStatus is the lifecycle state of an import job.
type ImportStatus string

const (
	// ImportQueued: accepted and durable on disk, not yet claimed by a worker.
	ImportQueued ImportStatus = "queued"
	// ImportRunning: a worker holds a lease and is making progress.
	ImportRunning ImportStatus = "running"
	// ImportPaused: cancellation was requested and the worker stopped at a batch
	// boundary, or the lease expired. Resumable from the checkpoint.
	ImportPaused ImportStatus = "paused"
	// ImportCompleted: every file finished and post-import verification passed.
	ImportCompleted ImportStatus = "completed"
	// ImportFailed: a job-level error, exhausted retries, or failed verification.
	ImportFailed ImportStatus = "failed"
	// ImportCancelled: the user cancelled. Committed records are kept.
	ImportCancelled ImportStatus = "cancelled"
)

// Terminal reports whether the job has stopped moving on its own.
func (s ImportStatus) Terminal() bool {
	switch s {
	case ImportCompleted, ImportFailed, ImportCancelled:
		return true
	}
	return false
}

// Resumable reports whether a user may retry the job from its checkpoint.
func (s ImportStatus) Resumable() bool {
	switch s {
	case ImportFailed, ImportCancelled, ImportPaused:
		return true
	}
	return false
}

// Active reports whether a worker should be, or is, processing the job.
func (s ImportStatus) Active() bool {
	return s == ImportQueued || s == ImportRunning
}

func (s ImportStatus) Valid() bool {
	switch s {
	case ImportQueued, ImportRunning, ImportPaused, ImportCompleted, ImportFailed, ImportCancelled:
		return true
	}
	return false
}

// ImportFileStatus is the lifecycle state of one file within a job.
type ImportFileStatus string

const (
	FilePending   ImportFileStatus = "pending"
	FileRunning   ImportFileStatus = "running"
	FileCompleted ImportFileStatus = "completed"
	FileFailed    ImportFileStatus = "failed"
	// FileSkipped covers archive entries that are not streaming history at all
	// (README files, playlists, search queries) and exact-duplicate uploads.
	FileSkipped ImportFileStatus = "skipped"
)

func (s ImportFileStatus) Valid() bool {
	switch s {
	case FilePending, FileRunning, FileCompleted, FileFailed, FileSkipped:
		return true
	}
	return false
}

// ImportFormat identifies which Spotify export a file came from.
type ImportFormat string

const (
	// FormatExtended is Streaming_History_Audio_*.json / endsong_*.json.
	FormatExtended ImportFormat = "extended"
	// FormatAccountData is StreamingHistory*.json / StreamingHistory_music_*.json.
	FormatAccountData ImportFormat = "account_data"
	// FormatUnknown means detection has not run or found nothing importable.
	FormatUnknown ImportFormat = "unknown"
)

func (f ImportFormat) Valid() bool {
	switch f {
	case FormatExtended, FormatAccountData, FormatUnknown:
		return true
	}
	return false
}

// Source maps a file format onto the listen source recorded on every row.
func (f ImportFormat) Source() Source {
	if f == FormatAccountData {
		return SourceAccountData
	}
	return SourceExtended
}

// Counters are the per-file and per-job outcome tallies exposed to the user.
//
// Every processed record lands in exactly one bucket, so
// Imported + Duplicates + Skipped + Rejected == records processed. That identity
// is what post-import verification asserts.
type Counters struct {
	// Imported: new rows durably committed to listens.
	Imported int64 `json:"imported"`
	// Duplicates: valid records already present, suppressed by the dedupe rules.
	Duplicates int64 `json:"duplicates"`
	// Skipped: well-formed records intentionally not stored (podcasts, too short).
	Skipped int64 `json:"skipped"`
	// Rejected: malformed records, recorded with diagnostics.
	Rejected int64 `json:"rejected"`
}

// Processed is the number of records accounted for.
func (c Counters) Processed() int64 {
	return c.Imported + c.Duplicates + c.Skipped + c.Rejected
}

// Add accumulates another set of counters.
func (c *Counters) Add(o Counters) {
	c.Imported += o.Imported
	c.Duplicates += o.Duplicates
	c.Skipped += o.Skipped
	c.Rejected += o.Rejected
}

// ImportFile is one streaming-history file inside a job, and carries the durable
// checkpoint that makes the import resumable.
type ImportFile struct {
	ID      uuid.UUID
	JobID   uuid.UUID
	Ordinal int
	// Name is what the user uploaded. ContainerPath is the entry path when the
	// file was found inside a .zip.
	Name          string
	ContainerPath string
	Format        ImportFormat
	SizeBytes     int64
	SHA256        []byte
	Status        ImportFileStatus

	// RecordsTotal is known only once a file has been read to the end; nil means
	// "still unknown", which the API reports as a null `pending` count.
	RecordsTotal *int64

	// RecordOffset is the number of records fully accounted for. It is written in
	// the same transaction as the batch it describes, so committed records never
	// exceed the checkpoint.
	RecordOffset int64
	// ByteOffset is the decoder's input offset after the last accounted record.
	// It is only meaningful for seekable sources; nil forces a replay-and-discard
	// resume, which is what compressed archive entries need.
	ByteOffset *int64

	Counters     Counters
	ErrorCode    string
	ErrorMessage string
	StartedAt    *time.Time
	FinishedAt   *time.Time
}

// Pending is the number of records left to read, or nil while the total is unknown.
func (f ImportFile) Pending() *int64 {
	if f.RecordsTotal == nil {
		return nil
	}
	p := *f.RecordsTotal - f.RecordOffset
	if p < 0 {
		p = 0
	}
	return &p
}

// ImportJob is a user-initiated import of one or more files.
type ImportJob struct {
	ID           uuid.UUID
	UserID       uuid.UUID
	Status       ImportStatus
	Note         string
	CreatedAt    time.Time
	StartedAt    *time.Time
	FinishedAt   *time.Time
	ErrorCode    string
	ErrorMessage string

	LeaseOwner      string
	LeaseExpiresAt  *time.Time
	CancelRequested bool

	FilesTotal int
	FilesDone  int
	Counters   Counters
	Files      []ImportFile
}

// Failed job error codes. These are stable identifiers the frontend can map to
// help text, unlike the free-text message.
const (
	ErrCodeUnreadable         = "file_unreadable"
	ErrCodeUnrecognisedFormat = "unrecognised_format"
	ErrCodeRetriesExhausted   = "retries_exhausted"
	ErrCodeVerificationFailed = "verification_failed"
	ErrCodeInternal           = "internal_error"
	ErrCodeEmptyUpload        = "empty_upload"
)

// FileVerification is the evidence used to decide whether an import may be
// declared successful. ListensInDatabase is an actual COUNT(*) against the
// listens table, not a running tally, which is the point: a job may only be
// reported complete when the database agrees with the counters.
type FileVerification struct {
	FileID            uuid.UUID
	Name              string
	Status            ImportFileStatus
	RecordOffset      int64
	RecordsTotal      *int64
	Counters          Counters
	ListensInDatabase int64
}

// VerificationError describes exactly which invariant a job violated.
type VerificationError struct {
	Problems []string
}

func (e *VerificationError) Error() string {
	return "import verification failed: " + strings.Join(e.Problems, "; ")
}

// VerifyJob asserts every invariant that must hold before an import job may be
// reported as completed:
//
//  1. no file is still pending, running or failed;
//  2. each file's counters account for exactly the records it processed;
//  3. each file reached its record total, when the total is known;
//  4. the number of listens actually present in the database for each file
//     matches the number the importer claims it inserted.
//
// Invariant 4 is what catches the "job looks complete but records were never
// committed" failure mode: a lost transaction shows up as a shortfall here, and
// the job is failed rather than silently reported as a success.
func VerifyJob(files []FileVerification) error {
	var problems []string
	for _, f := range files {
		label := f.Name
		if label == "" {
			label = f.FileID.String()
		}
		switch f.Status {
		case FileCompleted, FileSkipped:
		default:
			problems = append(problems, fmt.Sprintf("%s: status is %q, expected completed", label, f.Status))
			continue
		}
		if f.Status == FileSkipped {
			continue
		}
		if got, want := f.Counters.Processed(), f.RecordOffset; got != want {
			problems = append(problems, fmt.Sprintf(
				"%s: counters account for %d records but checkpoint says %d were processed", label, got, want))
		}
		if f.RecordsTotal != nil && f.RecordOffset != *f.RecordsTotal {
			problems = append(problems, fmt.Sprintf(
				"%s: processed %d of %d records", label, f.RecordOffset, *f.RecordsTotal))
		}
		if f.ListensInDatabase != f.Counters.Imported {
			problems = append(problems, fmt.Sprintf(
				"%s: importer claims %d inserted listens but the database holds %d",
				label, f.Counters.Imported, f.ListensInDatabase))
		}
	}
	if len(problems) > 0 {
		return &VerificationError{Problems: problems}
	}
	return nil
}
