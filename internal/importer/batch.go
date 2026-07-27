package importer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/RequiDev/encore/internal/domain"
	"github.com/RequiDev/encore/internal/logging"
	"github.com/RequiDev/encore/internal/retry"
	"github.com/RequiDev/encore/internal/store/imports"
	"github.com/RequiDev/encore/internal/store/listens"
)

// batch is a fixed-capacity accumulation of one flush's worth of work.
//
// Holding domain listens rather than staged rows matters: alias resolution runs
// at flush time inside the transaction, and it can change a listen's identity —
// and therefore its dedupe key — so the keys must not be computed until then.
type batch struct {
	records []domain.Listen
	rejects []imports.Reject
	delta   domain.Counters
}

func newBatch(capacity int) *batch {
	return &batch{
		records: make([]domain.Listen, 0, capacity),
		rejects: make([]imports.Reject, 0, 16),
	}
}

// size is how many records this batch accounts for, which is what the batch-size
// limit applies to. Skips and rejects count: a file that is 90% podcasts should
// still checkpoint regularly rather than reading to the end before saving.
func (b *batch) size() int {
	return len(b.records) + int(b.delta.Skipped) + int(b.delta.Rejected)
}

func (b *batch) empty() bool { return b.size() == 0 }

func (b *batch) reset() {
	b.records = b.records[:0]
	b.rejects = b.rejects[:0]
	b.delta = domain.Counters{}
}

// flushBatch commits one batch and its checkpoint in a single transaction.
//
// Everything about the import's durability lives in this function. The listens,
// the reject diagnostics and the checkpoint that says how far the file has been
// read all commit together, so there is no interleaving in which a crash could
// leave a checkpoint ahead of the data it claims to describe.
func (r *Runner) flushBatch(
	ctx context.Context,
	file domain.ImportFile,
	b *batch,
	recordOffset int64,
	byteOffset *int64,
	timezone string,
	log *slog.Logger,
) error {
	started := r.now()
	format := string(file.Format)

	policy := retry.Default().WithAttempts(r.cfg.BatchRetries + 1)
	err := retry.Do(ctx, policy, retry.Hooks{
		OnRetry: func(attempt int, delay time.Duration, err error) {
			r.stat.ImportBatch("retry", 0)
			log.Warn("import batch failed; retrying",
				"attempt", attempt, "retry_in", delay.String(), logging.Err(err))
		},
	}, func(ctx context.Context, _ int) error {
		return r.commitBatch(ctx, file, b, recordOffset, byteOffset, timezone)
	})

	elapsed := r.now().Sub(started)
	if err != nil {
		r.stat.ImportBatch("failed", elapsed)
		if domain.IsTransient(err) {
			// The checkpoint is intact, so the retry endpoint resumes rather
			// than restarting. Say so, because "the import failed" on its own
			// reads as "start again from the beginning".
			return &jobError{
				code:    domain.ErrCodeRetriesExhausted,
				message: "The database could not be written to after several attempts. Progress up to the last checkpoint was saved; retrying will continue from there.",
				err:     err,
			}
		}
		return fmt.Errorf("commit import batch: %w", err)
	}

	r.stat.ImportBatch("ok", elapsed)
	r.stat.ImportRecords(format, "imported", int(b.delta.Imported))
	r.stat.ImportRecords(format, "duplicate", int(b.delta.Duplicates))
	r.stat.ImportRecords(format, "skipped", int(b.delta.Skipped))
	r.stat.ImportRecords(format, "rejected", int(b.delta.Rejected))
	if byteOffset != nil {
		r.stat.ImportBytesRead(*byteOffset)
	}
	return nil
}

// errCheckpointStale means the checkpoint had already moved past this batch, so
// the transaction must be abandoned rather than committed.
var errCheckpointStale = errors.New("checkpoint already past this batch")

func (r *Runner) commitBatch(
	ctx context.Context,
	file domain.ImportFile,
	b *batch,
	recordOffset int64,
	byteOffset *int64,
	timezone string,
) error {
	// The delta is recomputed on every attempt, because Imported and Duplicates
	// depend on what the insert actually did and a retry may find a different
	// answer. Skipped and Rejected are decided by the parser and do not change.
	delta := domain.Counters{Skipped: b.delta.Skipped, Rejected: b.delta.Rejected}

	err := r.dep.Store.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		staged, trackSeeds, aliasKeys, err := r.stage(ctx, tx, file, b.records, timezone)
		if err != nil {
			return err
		}

		// Track and alias rows must exist before the listens that reference
		// them. They are created in the 'pending' state and never fetched here:
		// keeping Spotify out of the ingest path is what stops an API outage
		// from costing a user their history.
		// The catalogue rows a batch's listens point at, written in the same
		// transaction as the listens themselves. Tracks first: the credits and the
		// album link the next call writes both reference them.
		if err := r.dep.Listens.EnsureTracks(ctx, tx, trackSeeds); err != nil {
			return err
		}
		if err := r.dep.Listens.EnsureLocalCatalogue(ctx, tx, trackSeeds); err != nil {
			return err
		}
		if err := r.dep.Listens.EnsureAliases(ctx, tx, aliasKeys); err != nil {
			return err
		}

		inserted, err := r.dep.Listens.InsertListens(ctx, tx, staged, timezone)
		if err != nil {
			return err
		}
		delta.Imported = inserted
		delta.Duplicates = int64(len(staged)) - inserted

		if len(b.rejects) > 0 {
			if err := r.dep.Jobs.AddRejects(ctx, tx, file.ID, b.rejects); err != nil {
				return err
			}
		}

		applied, err := r.dep.Jobs.Checkpoint(ctx, tx, file.ID, recordOffset, byteOffset, delta)
		if err != nil {
			return err
		}
		if !applied {
			// A previous attempt committed after all and its acknowledgement was
			// lost, or another worker owns this file. Either way, committing now
			// would double-count. Abandon the transaction; the rows this attempt
			// tried to insert were already there, so nothing is lost.
			return errCheckpointStale
		}
		return nil
	})

	if errors.Is(err, errCheckpointStale) {
		return nil
	}
	return err
}

// stage resolves aliases and converts domain listens into insertable rows.
//
// The alias lookup happens here, inside the transaction and before any key is
// computed, because a names-only record whose (artist, title) pair is already
// known must be stored under the *track* identity. That is what makes an
// account-data import and an extended import of the same period converge on one
// set of listens instead of double-counting the overlap.
func (r *Runner) stage(
	ctx context.Context,
	tx pgx.Tx,
	file domain.ImportFile,
	records []domain.Listen,
	_ string,
) (staged []listens.StagedListen, trackSeeds []listens.TrackSeed, aliasKeys []domain.AliasKey, err error) {
	if len(records) == 0 {
		return nil, nil, nil, nil
	}

	// Collect the distinct unresolved name pairs so the lookup is one query.
	lookup := make([]domain.AliasKey, 0, len(records))
	seenKey := make(map[domain.AliasKey]struct{}, len(records))
	for _, l := range records {
		if l.Identity.IsResolved() {
			continue
		}
		k := l.Identity.AliasKeyOf()
		if k.IsZero() {
			continue
		}
		if _, dup := seenKey[k]; dup {
			continue
		}
		seenKey[k] = struct{}{}
		lookup = append(lookup, k)
	}

	resolved := map[domain.AliasKey]string{}
	if len(lookup) > 0 {
		resolved, err = r.dep.Listens.ResolvedAliases(ctx, tx, lookup)
		if err != nil {
			return nil, nil, nil, err
		}
	}

	fileID := file.ID
	staged = make([]listens.StagedListen, 0, len(records))
	seenTrack := make(map[string]struct{}, len(records))
	seenAlias := make(map[domain.AliasKey]struct{}, len(lookup))

	for _, l := range records {
		if !l.Identity.IsResolved() {
			if trackID, ok := resolved[l.Identity.AliasKeyOf()]; ok && trackID != "" {
				// Keep the original names on the row as provenance; only the
				// identity that decides duplication changes.
				alias := l.Identity
				l.Identity = domain.TrackIdentityFromID(trackID)
				s := listens.Stage(l, &fileID)
				s.AliasArtist, s.AliasTitle = alias.Artist, alias.Title
				staged = append(staged, s)
				if _, dup := seenTrack[trackID]; !dup {
					seenTrack[trackID] = struct{}{}
					trackSeeds = append(trackSeeds, listens.TrackSeed{
						ID: trackID, Name: l.TrackName,
						ArtistName: l.ArtistName, AlbumName: l.AlbumName,
					})
				}
				continue
			}
			k := l.Identity.AliasKeyOf()
			if _, dup := seenAlias[k]; !dup && !k.IsZero() {
				seenAlias[k] = struct{}{}
				aliasKeys = append(aliasKeys, k)
			}
			staged = append(staged, listens.Stage(l, &fileID))
			continue
		}

		if _, dup := seenTrack[l.Identity.TrackID]; !dup {
			seenTrack[l.Identity.TrackID] = struct{}{}
			trackSeeds = append(trackSeeds, listens.TrackSeed{
				ID: l.Identity.TrackID, Name: l.TrackName,
				ArtistName: l.ArtistName, AlbumName: l.AlbumName,
			})
		}
		staged = append(staged, listens.Stage(l, &fileID))
	}
	return staged, trackSeeds, aliasKeys, nil
}
