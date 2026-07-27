package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"

	"github.com/RequiDev/encore/internal/config"
	"github.com/RequiDev/encore/internal/crypto"
	"github.com/RequiDev/encore/internal/domain"
	"github.com/RequiDev/encore/internal/importer/formats"
	"github.com/RequiDev/encore/internal/logging"
	"github.com/RequiDev/encore/internal/postgres"
	"github.com/RequiDev/encore/internal/store"
	"github.com/RequiDev/encore/internal/store/catalog"
	"github.com/RequiDev/encore/internal/store/imports"
	"github.com/RequiDev/encore/internal/store/listens"
)

// backfillBatch is how many names are written per statement.
const backfillBatch = 1000

// backfillTrackNames recovers from the uploaded files everything an older import
// parsed and threw away.
//
// Both Spotify export formats print the track title, the artist and the album
// beside the URI. The importer now keeps all three — the title on the track, the
// other two as local catalogue rows keyed by their normalised names — but a
// history imported before that has nameless tracks and no artists at all, and if
// the application's daily Spotify quota is exhausted, no prospect of getting
// them. The names are still sitting in the uploaded files, so this reads them
// back and builds the catalogue that would be built today.
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
	lis := listens.New(db)

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
		n, err := backfillFile(ctx, cat, lis, db, f, lg)
		if err != nil {
			// One unreadable file must not stop the rest: the point of the
			// exercise is to recover as many names as possible.
			lg.Warn("could not read names from a file", "file", f.Name, logging.Err(err))
			continue
		}
		scanned++
		named += n
	}
	lg.Info("backfill complete", "files_read", scanned, "records_written", named)
	return nil
}

func backfillFile(
	ctx context.Context,
	cat *catalog.Repo,
	lis *listens.Repo,
	db *store.Store,
	f imports.StoredFile,
	lg *slog.Logger,
) (int64, error) {
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
	names := make([]domain.Track, 0, backfillBatch)
	seeds := make([]listens.TrackSeed, 0, backfillBatch)
	flush := func() error {
		if len(names) == 0 {
			return nil
		}
		if err := cat.SeedTrackNames(ctx, db.DB(), names); err != nil {
			return err
		}
		// Credits and albums for anything still uncredited. Never overwrites what
		// enrichment established, and skips tracks the import did not store.
		if err := lis.EnsureLocalCatalogue(ctx, db.DB(), seeds); err != nil {
			return err
		}
		written += int64(len(names))
		names, seeds = names[:0], seeds[:0]
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
		names = append(names, domain.Track{
			ID: rec.Listen.Identity.TrackID, Name: rec.Listen.TrackName,
		})
		seeds = append(seeds, listens.TrackSeed{
			ID:         rec.Listen.Identity.TrackID,
			Name:       rec.Listen.TrackName,
			ArtistName: rec.Listen.ArtistName,
			AlbumName:  rec.Listen.AlbumName,
		})
		if len(names) >= backfillBatch {
			if err := flush(); err != nil {
				return written, err
			}
		}
	}
	if err := flush(); err != nil {
		return written, err
	}
	lg.Info("read metadata from an imported file", "file", f.Name, "records", written)
	return written, nil
}
