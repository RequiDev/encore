package importer

import (
	"context"
	"errors"
	"log/slog"

	"github.com/RequiDev/encore/internal/domain"
	"github.com/RequiDev/encore/internal/logging"
)

// verify decides whether a job may call itself successful.
//
// This is the last gate before a job is reported as completed, and it is
// deliberately suspicious of the importer's own bookkeeping. The counters say
// how many rows the importer believes it inserted; VerificationData answers the
// same question by counting the listens table. When the two disagree the job is
// failed, not completed.
//
// That is what catches the failure this application must never have: a job that
// looks finished but whose records were never committed. A lost transaction, a
// database restored from an older backup, or a hand-edited status all show up
// here as a shortfall.
//
// verify returns an error only when it could not reach a verdict; a *failed*
// verification is recorded on the job and reported as nil, because the job has
// been dealt with.
func (r *Runner) verify(ctx context.Context, job *domain.ImportJob, log *slog.Logger) error {
	db := r.dep.Store.DB()

	data, err := r.dep.Jobs.VerificationData(ctx, db, job.ID)
	if err != nil {
		// Without evidence, the honest thing is to refuse to claim success.
		log.Error("could not verify import", logging.Err(err))
		if setErr := r.dep.Jobs.SetJobStatus(ctx, db, job.ID, domain.ImportFailed,
			domain.ErrCodeVerificationFailed,
			"The import finished but could not be verified against the database. Retry to check it again.",
		); setErr != nil {
			log.Error("could not record verification failure", logging.Err(setErr))
		}
		return err
	}

	if err := domain.VerifyJob(data); err != nil {
		var ve *domain.VerificationError
		message := "The import could not be verified: " + err.Error()
		if errors.As(err, &ve) && len(ve.Problems) > 0 {
			message = "The import finished but the database does not match what was recorded. " +
				"Retrying will re-check and re-import anything missing. Details: " + ve.Problems[0]
		}
		log.Error("import verification failed", logging.Err(err))
		if setErr := r.dep.Jobs.SetJobStatus(ctx, db, job.ID, domain.ImportFailed,
			domain.ErrCodeVerificationFailed, message); setErr != nil {
			log.Error("could not record verification failure", logging.Err(setErr))
		}
		return err
	}

	var totals domain.Counters
	for _, f := range data {
		totals.Add(f.Counters)
	}
	log.Info("import verified against the database",
		"imported", totals.Imported,
		"duplicates", totals.Duplicates,
		"skipped", totals.Skipped,
		"rejected", totals.Rejected)
	return nil
}
