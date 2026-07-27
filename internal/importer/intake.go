package importer

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash"
	"io"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/RequiDev/encore/internal/config"
	"github.com/RequiDev/encore/internal/domain"
	"github.com/RequiDev/encore/internal/importer/formats"
	"github.com/RequiDev/encore/internal/logging"
	"github.com/RequiDev/encore/internal/store"
	"github.com/RequiDev/encore/internal/store/imports"
)

// sniffBytes is how much of a file is read to identify its format. One record of
// either export format is comfortably inside this.
const sniffBytes = 4 << 10

// Intake accepts uploads and turns them into a queued import job.
//
// It streams every byte straight to durable storage. Nothing is buffered, so a
// four-gigabyte export costs the API process no more memory than a four-kilobyte
// one, and the worker — possibly in a different container — finds the file
// waiting for it on the shared volume.
type Intake struct {
	cfg  config.Import
	db   *store.Store
	jobs *imports.Repo
	log  *slog.Logger
}

// NewIntake builds an Intake and ensures the import directory exists.
func NewIntake(cfg config.Import, db *store.Store, jobs *imports.Repo, lg *slog.Logger) (*Intake, error) {
	if db == nil || jobs == nil {
		return nil, errors.New("importer: intake needs a store and an imports repository")
	}
	if cfg.Dir == "" {
		return nil, errors.New("importer: ENCORE_IMPORT_DIR must be set")
	}
	if err := os.MkdirAll(cfg.Dir, 0o750); err != nil {
		return nil, fmt.Errorf("create import directory %s: %w", cfg.Dir, err)
	}
	if lg == nil {
		lg = slog.Default()
	}
	return &Intake{cfg: cfg, db: db, jobs: jobs, log: lg.With("component", "import-intake")}, nil
}

// Upload is one file as it arrives from the client.
type Upload struct {
	// Filename is what the user called it. It is used for display and format
	// detection only, never as a path.
	Filename string
	Body     io.Reader
}

// Warning is advice about an upload that did not stop the job being created.
type Warning struct {
	File    string `json:"file"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Warning codes.
const (
	WarnAlreadyImported = "already_imported"
	WarnNoHistoryFound  = "no_history_found"
	WarnEmptyArchive    = "empty_archive"
)

// Create spools the uploads and queues a job for them.
//
// A file whose SHA-256 matches one this user has imported before is still
// accepted — re-importing is idempotent and adds nothing — but it produces a
// warning so the interface can say so rather than leaving the user wondering why
// the counters did not move.
func (i *Intake) Create(ctx context.Context, userID uuid.UUID, note string, uploads []Upload) (domain.ImportJob, []Warning, error) {
	if len(uploads) == 0 {
		return domain.ImportJob{}, nil, fmt.Errorf("%w: no files were uploaded", domain.ErrValidation)
	}

	batchID := uuid.New()
	dir := filepath.Join(i.cfg.Dir, batchID.String())
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return domain.ImportJob{}, nil, fmt.Errorf("create import directory: %w", err)
	}
	// Anything that fails from here leaves no spooled bytes behind.
	committed := false
	defer func() {
		if !committed {
			_ = os.RemoveAll(dir)
		}
	}()

	var (
		files    []imports.NewFile
		warnings []Warning
	)

	for idx, up := range uploads {
		display := safeDisplayName(up.Filename, idx)
		storedPath := filepath.Join(dir, fmt.Sprintf("%03d.bin", idx))

		size, sum, err := i.spool(storedPath, up.Body)
		if err != nil {
			return domain.ImportJob{}, nil, err
		}
		if size == 0 {
			warnings = append(warnings, Warning{File: display, Code: WarnNoHistoryFound, Message: "The file was empty."})
			continue
		}

		if jobID, prevName, found, err := i.jobs.FileSHAExists(ctx, i.db.DB(), userID, sum); err != nil {
			i.log.Warn("could not check for a previous import of this file", logging.Err(err))
		} else if found {
			_ = jobID
			warnings = append(warnings, Warning{
				File: display,
				Code: WarnAlreadyImported,
				Message: fmt.Sprintf(
					"This is the same file as %q, which has already been imported. Re-importing is safe and will add nothing new.",
					prevName),
			})
		}

		isZip, err := formats.IsZip(storedPath)
		if err != nil {
			return domain.ImportJob{}, nil, fmt.Errorf("inspect %s: %w", display, err)
		}

		if isZip {
			entries, err := formats.ListArchiveEntries(storedPath)
			if err != nil {
				warnings = append(warnings, Warning{
					File: display, Code: WarnEmptyArchive,
					Message: "The archive could not be read: " + err.Error(),
				})
				continue
			}
			if len(entries) == 0 {
				warnings = append(warnings, Warning{
					File: display, Code: WarnEmptyArchive,
					Message: "No streaming-history files were found inside the archive. Make sure you uploaded the export Spotify sent you.",
				})
				continue
			}
			for _, e := range entries {
				files = append(files, imports.NewFile{
					Name:          path.Base(e.Path),
					ContainerPath: e.Path,
					Format:        e.Format,
					SizeBytes:     e.Size,
					SHA256:        nil, // the digest belongs to the archive, not the entry
					StoragePath:   storedPath,
				})
			}
			i.log.Info("archive expanded", "file", display, "entries", len(entries))
			continue
		}

		format, err := i.detectFormat(storedPath, display)
		if err != nil {
			return domain.ImportJob{}, nil, err
		}
		if format == domain.FormatUnknown {
			warnings = append(warnings, Warning{
				File: display, Code: WarnNoHistoryFound,
				Message: "This does not look like a Spotify streaming-history file, so it was left out of the import.",
			})
			continue
		}

		files = append(files, imports.NewFile{
			Name:        display,
			Format:      format,
			SizeBytes:   size,
			SHA256:      sum,
			StoragePath: storedPath,
		})
	}

	if len(files) == 0 {
		return domain.ImportJob{}, warnings, fmt.Errorf(
			"%w: none of the uploaded files contained Spotify streaming history", domain.ErrValidation)
	}

	var job domain.ImportJob
	err := i.db.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		created, err := i.jobs.CreateJob(ctx, tx, userID, note, files)
		if err != nil {
			return err
		}
		job = created
		return nil
	})
	if err != nil {
		return domain.ImportJob{}, warnings, err
	}

	committed = true
	i.log.Info("import job queued", "job", job.ID.String(), "user", userID.String(), "files", len(files))
	return job, warnings, nil
}

// spool streams one upload to disk, hashing as it goes so the bytes are read
// exactly once.
func (i *Intake) spool(dest string, body io.Reader) (int64, []byte, error) {
	f, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640)
	if err != nil {
		return 0, nil, fmt.Errorf("create upload file: %w", err)
	}
	defer f.Close()

	var digest hash.Hash = sha256.New()
	limit := i.cfg.MaxUploadBytes
	// One byte past the limit, so exceeding it is detectable rather than silently
	// truncating a user's history.
	src := io.LimitReader(io.TeeReader(body, digest), limit+1)

	size, err := io.Copy(f, src)
	if err != nil {
		return 0, nil, fmt.Errorf("write upload: %w", err)
	}
	if size > limit {
		return 0, nil, fmt.Errorf("%w: the file is larger than the %d byte upload limit", domain.ErrValidation, limit)
	}
	if err := f.Sync(); err != nil {
		return 0, nil, fmt.Errorf("flush upload to disk: %w", err)
	}
	return size, digest.Sum(nil), nil
}

// detectFormat sniffs a spooled file, transparently handling gzip.
func (i *Intake) detectFormat(storedPath, display string) (domain.ImportFormat, error) {
	rc, _, err := formats.OpenMaybeCompressed(storedPath)
	if err != nil {
		return domain.FormatUnknown, fmt.Errorf("open %s: %w", display, err)
	}
	defer rc.Close()

	head := make([]byte, sniffBytes)
	n, err := io.ReadFull(rc, head)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return domain.FormatUnknown, fmt.Errorf("read %s: %w", display, err)
	}
	return formats.Detect(display, head[:n]), nil
}

// RemoveJobFiles deletes the spooled uploads belonging to a job. The listening
// records the job produced are untouched: those are the user's history, and the
// schema nulls their import_file_id rather than cascading.
func (i *Intake) RemoveJobFiles(ctx context.Context, jobID uuid.UUID) error {
	files, err := i.jobs.ListFiles(ctx, i.db.DB(), jobID)
	if err != nil {
		return err
	}
	dirs := map[string]struct{}{}
	for _, f := range files {
		p, err := i.jobs.StoragePath(ctx, i.db.DB(), f.ID)
		if err != nil || p == "" {
			continue
		}
		if !withinImportDir(i.cfg.Dir, p) {
			// A path outside the configured directory means the database is
			// wrong; refusing is safer than deleting whatever it points at.
			i.log.Warn("refusing to delete an import file outside the import directory")
			continue
		}
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			i.log.Warn("could not remove import file", logging.Err(err))
		}
		dirs[filepath.Dir(p)] = struct{}{}
	}
	for d := range dirs {
		if withinImportDir(i.cfg.Dir, d) && filepath.Clean(d) != filepath.Clean(i.cfg.Dir) {
			// Only succeeds when the directory is empty, which is the intent.
			_ = os.Remove(d)
		}
	}
	return nil
}

// safeDisplayName reduces an uploaded filename to something safe to store and
// show. It is never used to build a path — spooled files are named by index —
// but a name echoed back into HTML or a log line still should not carry
// separators or control characters.
func safeDisplayName(name string, idx int) string {
	name = strings.TrimSpace(name)
	name = strings.ReplaceAll(name, "\\", "/")
	if i := strings.LastIndexByte(name, '/'); i >= 0 {
		name = name[i+1:]
	}
	name = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, name)
	if name == "" || name == "." || name == ".." {
		return fmt.Sprintf("upload-%03d", idx)
	}
	const maxLen = 200
	if len(name) > maxLen {
		name = name[:maxLen]
	}
	return name
}
