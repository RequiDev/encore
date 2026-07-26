package importer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	"github.com/requi/encore/internal/domain"
	"github.com/requi/encore/internal/importer/formats"
	"github.com/requi/encore/internal/logging"
	"github.com/requi/encore/internal/store/imports"
)

// cancelCheckEvery bounds how often the worker asks the database whether the
// user has cancelled. Checking at every batch would be one extra round trip per
// batch for a question whose answer almost never changes.
const cancelCheckEvery = 10

// processFile streams one file into the database, checkpointing as it goes.
//
// Peak memory here is O(batch size × record size) and nothing more: the decoder
// holds one raw record at a time and the batch is fixed size. That is true for a
// 1 MiB file and for a 40 GiB one.
func (r *Runner) processFile(ctx context.Context, job *domain.ImportJob, file domain.ImportFile, timezone string, log *slog.Logger) error {
	log = log.With("file", file.Name)

	parser, ok := formats.ParserFor(file.Format)
	if !ok {
		// Not streaming history at all. Skipping is the correct outcome: a
		// Spotify export is full of files Encore has no use for, and failing the
		// job over a playlist dump would be absurd.
		log.Info("skipping file: not a streaming-history format")
		return r.dep.Jobs.SkipFile(ctx, r.dep.Store.DB(), file.ID, "not a recognised streaming-history format")
	}

	if err := r.dep.Jobs.StartFile(ctx, r.dep.Store.DB(), file.ID); err != nil {
		return fmt.Errorf("mark file running: %w", err)
	}

	reader, closeFn, seekable, err := r.openAtCheckpoint(ctx, file, log)
	if err != nil {
		var je *jobError
		if errors.As(err, &je) {
			_ = r.dep.Jobs.FailFile(ctx, r.dep.Store.DB(), file.ID, je.code, je.message)
		}
		return err
	}
	defer closeFn()

	b := newBatch(r.cfg.BatchSize)
	rejectBudget := r.cfg.MaxRejectsPerFile - int(file.Counters.Rejected)
	started := r.now()
	recordsThisRun := int64(0)
	batchesSinceCancelCheck := 0

	flush := func() error {
		if b.empty() {
			return nil
		}
		offset := reader.Index()
		var byteOffset *int64
		if seekable {
			o := reader.InputOffset()
			byteOffset = &o
		}
		if err := r.flushBatch(ctx, file, b, offset, byteOffset, timezone, log); err != nil {
			return err
		}
		b.reset()
		return nil
	}

	for {
		var raw json.RawMessage
		more, err := reader.Next(&raw)
		if err != nil {
			// A stream-level syntax error means the rest of the file cannot be
			// trusted. Everything committed so far stays; the checkpoint records
			// exactly how far it got.
			if flushErr := flush(); flushErr != nil {
				return flushErr
			}
			var se *formats.SyntaxError
			if errors.As(err, &se) {
				je := &jobError{
					code:    domain.ErrCodeUnreadable,
					message: fmt.Sprintf("%s is not valid JSON at byte %d. Re-download the export from Spotify and try again.", file.Name, se.Offset),
					err:     err,
				}
				_ = r.dep.Jobs.FailFile(ctx, r.dep.Store.DB(), file.ID, je.code, je.message)
				return je
			}
			return fmt.Errorf("read %s: %w", file.Name, err)
		}
		if !more {
			break
		}

		// Index() is the count of records decoded including this one, so the
		// record's own position is one less. It is absolute across a resume,
		// which is what lets it line up with import_rejects.record_index.
		index := reader.Index() - 1
		recordsThisRun++

		record, perr := parser.Parse(raw, r.cfg.MinMsPlayed)
		switch {
		case perr != nil:
			b.delta.Rejected++
			if rejectBudget > 0 {
				rejectBudget--
				reason, detail := rejectDetail(perr)
				b.rejects = append(b.rejects, imports.Reject{
					RecordIndex: index,
					Reason:      reason,
					Detail:      detail,
					RawExcerpt:  string(raw),
				})
			}
		case record.Skip != nil:
			b.delta.Skipped++
		default:
			listen := record.Listen
			listen.UserID = job.UserID
			if err := listen.Validate(r.now()); err != nil {
				// The parser produced something the domain rejects. That is a
				// per-record problem, never a job failure.
				b.delta.Rejected++
				if rejectBudget > 0 {
					rejectBudget--
					reason, detail := rejectDetail(err)
					b.rejects = append(b.rejects, imports.Reject{
						RecordIndex: index,
						Reason:      reason,
						Detail:      detail,
						RawExcerpt:  string(raw),
					})
				}
				break
			}
			b.records = append(b.records, listen)
		}

		if b.size() < r.cfg.BatchSize {
			continue
		}
		if err := flush(); err != nil {
			return err
		}

		batchesSinceCancelCheck++
		if batchesSinceCancelCheck >= cancelCheckEvery {
			batchesSinceCancelCheck = 0
			cancelled, err := r.dep.Jobs.IsCancelRequested(ctx, r.dep.Store.DB(), job.ID)
			if err != nil {
				log.Warn("could not check for cancellation", logging.Err(err))
			} else if cancelled {
				return errCancelled
			}
		}
		if err := ctx.Err(); err != nil {
			return err
		}
	}

	if err := flush(); err != nil {
		return err
	}

	total := reader.Index()
	if err := r.dep.Jobs.FinishFile(ctx, r.dep.Store.DB(), file.ID, total); err != nil {
		return fmt.Errorf("mark file completed: %w", err)
	}

	elapsed := r.now().Sub(started)
	if elapsed > 0 && recordsThisRun > 0 {
		r.stat.ImportThroughput(float64(recordsThisRun) / elapsed.Seconds())
	}
	log.Info("file imported",
		"records", total,
		"records_this_run", recordsThisRun,
		"elapsed", elapsed.Round(time.Millisecond).String())
	return nil
}

// openAtCheckpoint opens a file and positions it at the stored checkpoint.
//
// A seekable source jumps straight to the recorded byte offset, which is O(1)
// however far in the crash happened. A compressed source or an archive entry
// cannot seek, so it replays and discards the records already accounted for —
// O(n), but decode-and-discard is far cheaper than the full ingest path and it
// avoids having to persist decompression state.
func (r *Runner) openAtCheckpoint(ctx context.Context, file domain.ImportFile, log *slog.Logger) (*formats.ArrayReader, func(), bool, error) {
	path, err := r.dep.Jobs.StoragePath(ctx, r.dep.Store.DB(), file.ID)
	if err != nil {
		return nil, nil, false, fmt.Errorf("look up file storage path: %w", err)
	}

	missing := &jobError{
		code:    domain.ErrCodeUnreadable,
		message: fmt.Sprintf("The uploaded file for %s is no longer on disk. Upload it again to continue.", file.Name),
	}
	if path == "" {
		return nil, nil, false, missing
	}
	if _, statErr := os.Stat(path); statErr != nil {
		if os.IsNotExist(statErr) {
			return nil, nil, false, missing
		}
		return nil, nil, false, &jobError{
			code:    domain.ErrCodeUnreadable,
			message: fmt.Sprintf("%s could not be opened.", file.Name),
			err:     statErr,
		}
	}

	var (
		rc       io.ReadCloser
		seekable bool
	)
	if file.ContainerPath != "" {
		rc, err = formats.OpenArchiveEntry(path, file.ContainerPath)
	} else {
		rc, seekable, err = formats.OpenMaybeCompressed(path)
	}
	if err != nil {
		return nil, nil, false, &jobError{
			code:    domain.ErrCodeUnreadable,
			message: fmt.Sprintf("%s could not be opened: it may be corrupt or truncated.", file.Name),
			err:     err,
		}
	}
	closeFn := func() { _ = rc.Close() }

	rs, canSeek := rc.(io.ReadSeeker)
	seekable = seekable && canSeek

	fail := func(err error) (*formats.ArrayReader, func(), bool, error) {
		closeFn()
		return nil, nil, false, &jobError{
			code:    domain.ErrCodeUnreadable,
			message: fmt.Sprintf("%s could not be read as JSON.", file.Name),
			err:     err,
		}
	}

	// Fresh start.
	if file.RecordOffset == 0 {
		reader, err := formats.NewArrayReader(rc)
		if err != nil {
			return fail(err)
		}
		return reader, closeFn, seekable, nil
	}

	// Resume by seeking, when we can.
	if seekable && file.ByteOffset != nil && *file.ByteOffset > 0 {
		reader, err := formats.NewArrayReaderAt(rs, *file.ByteOffset, file.RecordOffset)
		if err != nil {
			return fail(err)
		}
		log.Info("resuming import from checkpoint", "records", file.RecordOffset, "byte_offset", *file.ByteOffset)
		return reader, closeFn, true, nil
	}

	// Resume by replaying. Nothing is inserted, so this is safe to do as often
	// as a flaky machine demands.
	reader, err := formats.NewArrayReader(rc)
	if err != nil {
		return fail(err)
	}
	var discard json.RawMessage
	for reader.Index() < file.RecordOffset {
		more, err := reader.Next(&discard)
		if err != nil {
			return fail(err)
		}
		if !more {
			// The file is shorter than the checkpoint claims, which means it was
			// replaced by a different file between attempts.
			closeFn()
			return nil, nil, false, &jobError{
				code:    domain.ErrCodeUnreadable,
				message: fmt.Sprintf("%s is shorter than the recorded progress for it; the file appears to have changed since the import started.", file.Name),
			}
		}
	}
	log.Info("resuming import by replaying to checkpoint", "records", file.RecordOffset)
	return reader, closeFn, false, nil
}

// rejectDetail extracts the stable reason code and human-readable detail from a
// per-record failure.
func rejectDetail(err error) (domain.RejectReason, string) {
	if re, ok := domain.AsReject(err); ok {
		return re.Reason, re.Detail
	}
	return domain.RejectMalformedRecord, err.Error()
}
