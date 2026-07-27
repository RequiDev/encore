package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"

	"github.com/requi/encore/internal/config"
	"github.com/requi/encore/internal/crypto"
	"github.com/requi/encore/internal/domain"
	"github.com/requi/encore/internal/importer/formats"
	"github.com/requi/encore/internal/logging"
	"github.com/requi/encore/internal/postgres"
	"github.com/requi/encore/internal/store"
	"github.com/requi/encore/internal/store/catalog"
	"github.com/requi/encore/internal/store/imports"
)

// backfillBatch is how many names are written per statement.
const backfillBatch = 1000

// backfillTrackNames fills in the names of tracks imported before Encore learned
// to keep them.
//
// Both Spotify export formats print the track title beside the URI, and the
// importer now records it as the row is created. A history imported before that
// has sixteen thousand nameless rows waiting on the catalogue queue, and if the
// application's daily Spotify quota is exhausted that wait is most of a day.
// The names are still sitting in the uploaded files, so this reads them back.
//
// It is deliberately not a re-import. Re-running a completed job could never
// verify: its listens already exist and would all be counted as duplicates, so
// the importer would rightly refuse to call the result a success. This touches
// nothing but empty track names — no listens, no job state, no counters — and it
// never overwrites a name enrichment has already established.
//
// Safe to run at any time, and safe to run twice.
func backfillTrackNames(ctx context.Context, cfg *config.Config, lg *slog.Logger) error {
	pool, err := postgres.Connect(ctx, cfg.Database, lg)
	if err != nil {
		return err
	}
	defer pool.Close()

	sealer, err := crypto.NewSealer(cfg.Security.EncryptionKey)
	if err != nil {
		return err
	}
	db, err := store.New(pool, sealer)
	if err != nil {
		return err
	}

	jobs := imports.New(db)
	cat := catalog.New(db)

	files, err := jobs.AllFilesWithStorage(ctx, db.DB())
	if err != nil {
		return fmt.Errorf("list import files: %w", err)
	}
	if len(files) == 0 {
		lg.Info("no imported files are retained, so there is nothing to read names from")
		return nil
	}

	var scanned, named int64
	for _, f := range files {
		n, err := backfillFile(ctx, cat, db, f, lg)
		if err != nil {
			// One unreadable file must not stop the rest: the point of the
			// exercise is to recover as many names as possible.
			lg.Warn("could not read names from a file", "file", f.Name, logging.Err(err))
			continue
		}
		scanned++
		named += n
	}
	lg.Info("track name backfill complete", "files_read", scanned, "names_written", named)
	return nil
}

func backfillFile(ctx context.Context, cat *catalog.Repo, db *store.Store, f imports.StoredFile, lg *slog.Logger) (int64, error) {
	if f.StoragePath == "" {
		return 0, nil
	}
	if _, err := os.Stat(f.StoragePath); err != nil {
		return 0, fmt.Errorf("the uploaded file is no longer on disk: %w", err)
	}

	var rc io.ReadCloser
	var err error
	if f.ContainerPath != "" {
		rc, err = formats.OpenArchiveEntry(f.StoragePath, f.ContainerPath)
	} else {
		rc, _, err = formats.OpenMaybeCompressed(f.StoragePath)
	}
	if err != nil {
		return 0, err
	}
	defer rc.Close()

	parser, ok := formats.ParserFor(f.Format)
	if !ok {
		return 0, nil
	}
	reader, err := formats.NewArrayReader(rc)
	if err != nil {
		return 0, err
	}

	var written int64
	batch := make([]domain.Track, 0, backfillBatch)
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		if err := cat.SeedTrackNames(ctx, db.DB(), batch); err != nil {
			return err
		}
		written += int64(len(batch))
		batch = batch[:0]
		return nil
	}

	for {
		var raw json.RawMessage
		more, err := reader.Next(&raw)
		if err != nil || !more {
			// A damaged tail is not worth failing over; everything read so far
			// is already good.
			break
		}
		// minMsPlayed is zero here on purpose: a play too short to store is still
		// a perfectly good source of the track's name.
		rec, perr := parser.Parse(raw, 0)
		if perr != nil || rec.Skip != nil {
			continue
		}
		if !rec.Listen.Identity.IsResolved() || rec.Listen.TrackName == "" {
			continue
		}
		batch = append(batch, domain.Track{
			ID: rec.Listen.Identity.TrackID, Name: rec.Listen.TrackName,
		})
		if len(batch) >= backfillBatch {
			if err := flush(); err != nil {
				return written, err
			}
		}
	}
	if err := flush(); err != nil {
		return written, err
	}
	lg.Info("read names from an imported file", "file", f.Name, "names", written)
	return written, nil
}
