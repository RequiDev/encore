package httpapi

import (
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"

	"github.com/RequiDev/encore/internal/domain"
	"github.com/RequiDev/encore/internal/importer"
	"github.com/RequiDev/encore/internal/logging"
	"github.com/RequiDev/encore/internal/store/imports"
)

// maxNoteBytes bounds the free-text note attached to an import.
const maxNoteBytes = 4 << 10

// maxUploadParts bounds how many files one request may carry. A Spotify export
// is a single archive, or at worst a few dozen JSON files, so this is generous;
// it exists so that a request cannot open an unbounded number of staged files.
const maxUploadParts = 256

// stagingPrefix names the temporary directory an upload passes through. It sits
// inside ENCORE_IMPORT_DIR, which is the volume already sized for imports.
const stagingPrefix = "incoming-"

// handleCreateImport answers POST /api/imports.
//
// The body is read with r.MultipartReader rather than ParseMultipartForm: the
// latter buffers part of the upload in memory before spilling the rest to disk,
// which for a four-gigabyte export is exactly the wrong shape. Here every byte
// goes straight from the socket to the disk in fixed-size chunks, so the process
// costs the same memory whatever the size of the upload.
//
// The parts are staged in a temporary directory first because mime/multipart
// invalidates a part as soon as the next one is read, while the intake needs
// every reader at once; the staging directory is removed as soon as the job
// exists. Nothing is ever held in memory, which is the property that matters.
func (s *Server) handleCreateImport(w http.ResponseWriter, r *http.Request) {
	user, err := requireUser(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	reader, err := r.MultipartReader()
	if err != nil {
		writeError(w, r, ErrInvalidRequest(
			"Send the files as a multipart/form-data body.", nil).WithCause(err))
		return
	}
	ctx := r.Context()

	stage, err := os.MkdirTemp(s.cfg.Import.Dir, stagingPrefix)
	if err != nil {
		writeError(w, r, ErrInternal(fmt.Errorf("create upload staging directory: %w", err)))
		return
	}
	defer func() {
		if err := os.RemoveAll(stage); err != nil {
			logging.FromContext(ctx).Warn("could not clear the upload staging directory", logging.Err(err))
		}
	}()

	note, uploads, staged, err := s.stageUpload(reader, stage)
	defer func() {
		for _, f := range staged {
			_ = f.Close()
		}
	}()
	if err != nil {
		writeError(w, r, err)
		return
	}
	if len(uploads) == 0 {
		writeError(w, r, ErrInvalidRequest("The request carried no files.", nil))
		return
	}

	job, warnings, err := s.intake.Create(ctx, user.ID, note, uploads)
	if err != nil {
		writeError(w, r, err)
		return
	}

	writeJSON(w, r, http.StatusAccepted, CreateImportResponse{
		Job:      toImportJob(job),
		Warnings: toWarnings(warnings),
	})
}

// stageUpload walks the multipart body, writing each file part to the staging
// directory and collecting the note.
func (s *Server) stageUpload(reader *multipart.Reader, stage string) (note string, uploads []importer.Upload, staged []*os.File, err error) {
	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			return note, uploads, staged, nil
		}
		if err != nil {
			return "", nil, staged, uploadError(err)
		}

		// The name is read before the part is consumed, since it is the only
		// thing distinguishing a file from an ordinary form value.
		name := part.FileName()
		if name == "" {
			if part.FormName() == "note" {
				raw, err := io.ReadAll(io.LimitReader(part, maxNoteBytes))
				if err != nil {
					_ = part.Close()
					return "", nil, staged, uploadError(err)
				}
				note = strings.TrimSpace(string(raw))
			}
			// Any other value part is ignored rather than refused, so a client
			// that sends an extra field is not broken by it.
			_ = part.Close()
			continue
		}

		if len(uploads) >= maxUploadParts {
			_ = part.Close()
			return "", nil, staged, ErrInvalidRequest(fmt.Sprintf(
				"Upload at most %d files at once, or upload the archive Spotify sent you.", maxUploadParts), nil)
		}

		file, err := s.stagePart(stage, len(uploads), part)
		_ = part.Close()
		if err != nil {
			return "", nil, staged, err
		}
		staged = append(staged, file)
		uploads = append(uploads, importer.Upload{Filename: name, Body: file})
	}
}

// stagePart streams one part to disk and rewinds it ready to be read again.
func (s *Server) stagePart(stage string, index int, part *multipart.Part) (*os.File, error) {
	path := filepath.Join(stage, fmt.Sprintf("part-%03d", index))
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, ErrInternal(fmt.Errorf("create staged upload: %w", err))
	}

	limit := s.cfg.Import.MaxUploadBytes
	// One byte past the limit, so exceeding it is detectable rather than a
	// silently truncated history.
	written, err := io.Copy(file, io.LimitReader(part, limit+1))
	if err != nil {
		_ = file.Close()
		return nil, uploadError(err)
	}
	if written > limit {
		_ = file.Close()
		return nil, ErrTooLarge(limit)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		_ = file.Close()
		return nil, ErrInternal(fmt.Errorf("rewind staged upload: %w", err))
	}
	return file, nil
}

// uploadError turns a failure while reading the body into the right answer: a
// body beyond the cap is 413, and anything else malformed is a 400.
func uploadError(err error) error {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		return ErrTooLarge(tooLarge.Limit)
	}
	return ErrInvalidRequest("The upload could not be read as multipart/form-data.", nil).WithCause(err)
}

// handleListImports answers GET /api/imports.
func (s *Server) handleListImports(w http.ResponseWriter, r *http.Request) {
	user, err := requireUser(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	limit, offset, err := parsePage(r)
	if err != nil {
		writeError(w, r, err)
		return
	}

	jobs, total, err := s.imports.ListJobsForUser(r.Context(), s.querier, user.ID, limit, offset)
	if err != nil {
		writeError(w, r, err)
		return
	}
	items := make([]ImportJob, 0, len(jobs))
	for _, job := range jobs {
		items = append(items, toImportJob(job))
	}
	writeJSON(w, r, http.StatusOK, Page[ImportJob]{Items: items, Total: total})
}

// handleGetImport answers GET /api/imports/{id}.
func (s *Server) handleGetImport(w http.ResponseWriter, r *http.Request) {
	job, err := s.callerJob(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, toImportJob(job))
}

// callerJob loads an import job scoped to the caller.
//
// The scoping is the authorisation: a job owned by somebody else comes back as
// domain.ErrNotFound, so the endpoint cannot be used to discover which job ids
// exist on the instance.
func (s *Server) callerJob(r *http.Request) (domain.ImportJob, error) {
	user, err := requireUser(r)
	if err != nil {
		return domain.ImportJob{}, err
	}
	id, err := parseUUIDPath(r, "id")
	if err != nil {
		return domain.ImportJob{}, err
	}
	return s.imports.GetJobForUser(r.Context(), s.querier, id, user.ID)
}

// handleCancelImport answers POST /api/imports/{id}/cancel.
//
// It only raises a flag. The worker sees it at the next batch boundary and stops
// after committing the batch in flight, so the checkpoint and the records it
// describes stay in agreement and everything already imported is kept.
func (s *Server) handleCancelImport(w http.ResponseWriter, r *http.Request) {
	user, err := requireUser(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	id, err := parseUUIDPath(r, "id")
	if err != nil {
		writeError(w, r, err)
		return
	}
	ctx := r.Context()

	if err := s.imports.RequestCancel(ctx, s.querier, id, user.ID); err != nil {
		writeError(w, r, err)
		return
	}
	s.respondWithJob(w, r, id, user.ID)
}

// handleRetryImport answers POST /api/imports/{id}/retry.
//
// The job resumes from its checkpoint rather than restarting: completed files
// are untouched and a partly read file picks up at the record it had accounted
// for, which is what makes retrying a failed multi-gigabyte import cheap.
func (s *Server) handleRetryImport(w http.ResponseWriter, r *http.Request) {
	user, err := requireUser(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	id, err := parseUUIDPath(r, "id")
	if err != nil {
		writeError(w, r, err)
		return
	}
	ctx := r.Context()

	if err := s.imports.RetryJob(ctx, s.querier, id, user.ID); err != nil {
		writeError(w, r, err)
		return
	}
	s.respondWithJob(w, r, id, user.ID)
}

// respondWithJob re-reads a job and returns it, so the client sees the state its
// request produced instead of having to poll for it.
func (s *Server) respondWithJob(w http.ResponseWriter, r *http.Request, id, userID uuid.UUID) {
	job, err := s.imports.GetJobForUser(r.Context(), s.querier, id, userID)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusAccepted, toImportJob(job))
}

// handleDeleteImport answers DELETE /api/imports/{id}.
//
// The job and its uploaded files go; the listening records it produced stay,
// with their import_file_id nulled by the schema. The history belongs to the
// user, while the job is only bookkeeping about how it arrived.
func (s *Server) handleDeleteImport(w http.ResponseWriter, r *http.Request) {
	user, err := requireUser(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	id, err := parseUUIDPath(r, "id")
	if err != nil {
		writeError(w, r, err)
		return
	}
	ctx := r.Context()

	// Ownership is established before anything is removed from disk, because
	// RemoveJobFiles knows only about job ids and would happily delete somebody
	// else's uploads if handed one.
	if _, err := s.imports.GetJobForUser(ctx, s.querier, id, user.ID); err != nil {
		writeError(w, r, err)
		return
	}
	if err := s.intake.RemoveJobFiles(ctx, id); err != nil {
		logging.FromContext(ctx).Warn("could not remove the uploads of an import job", logging.Err(err))
	}
	if err := s.imports.DeleteJob(ctx, s.querier, id, user.ID); err != nil {
		writeError(w, r, err)
		return
	}
	writeNoContent(w)
}

// handleImportRejects answers GET /api/imports/{id}/rejects.
//
// Rejects are recorded per file, while the endpoint pages over the whole job, so
// the page is assembled by walking the job's files in order and taking from each
// only the slice the requested window needs.
func (s *Server) handleImportRejects(w http.ResponseWriter, r *http.Request) {
	job, err := s.callerJob(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	limit, offset, err := parsePage(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	ctx := r.Context()

	counts := make([]int64, len(job.Files))
	var total int64
	for i, f := range job.Files {
		n, err := s.imports.CountRejects(ctx, s.querier, f.ID)
		if err != nil {
			writeError(w, r, err)
			return
		}
		counts[i], total = n, total+n
	}

	items := make([]ImportReject, 0, limit)
	skip, take := int64(offset), int64(limit)
	for i, f := range job.Files {
		if take <= 0 {
			break
		}
		if skip >= counts[i] {
			skip -= counts[i]
			continue
		}
		var page []imports.Reject
		page, _, err = s.imports.ListRejects(ctx, s.querier, f.ID, int(take), int(skip))
		if err != nil {
			writeError(w, r, err)
			return
		}
		for _, rj := range page {
			items = append(items, toImportReject(f.Name, rj))
		}
		take -= int64(len(page))
		skip = 0
	}
	writeJSON(w, r, http.StatusOK, Page[ImportReject]{Items: items, Total: total})
}
