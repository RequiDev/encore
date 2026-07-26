package enrich

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/requi/encore/internal/domain"
	"github.com/requi/encore/internal/store/listens"
)

// relinkPageSize is how many listens one relink transaction repoints.
//
// A single alias can cover hundreds of thousands of rows across every account on
// the instance, so the pass is paged: a few hundred rows keeps each transaction
// short enough not to hold a connection against the import path, and a crash
// costs one page rather than the whole alias.
const relinkPageSize = 500

// relinkQueueDepth is how many resolved aliases may wait for the relink loop.
const relinkQueueDepth = 1024

// relinkAttempts bounds how often one alias is retried. Beyond this the hand-off
// is dropped with a warning: the alias stays resolved, so every listen imported
// afterwards is stored correctly, and only the historical rows keep their
// names-only identity until the pair is seen again.
const relinkAttempts = 3

// relinkJob is one resolved alias waiting to have its listens repointed.
type relinkJob struct {
	key      domain.AliasKey
	trackID  string
	attempts int
}

// RelinkStats reports what a relink pass did. Removed rows are not lost listens:
// each one collided with a listen already stored under the resolved track id,
// which proves the same event had already been recorded through a source that
// knew the URI.
type RelinkStats struct {
	Relinked int64
	Removed  int64
}

// RunRelinkOnce repoints the listens of at most one freshly resolved alias. It
// reports whether there was anything queued to do.
func (w *Worker) RunRelinkOnce(ctx context.Context) (bool, error) {
	select {
	case job := <-w.relink:
		res, err := w.RelinkAlias(ctx, job.key, job.trackID)
		if err != nil {
			if ctx.Err() == nil {
				w.requeueRelink(job)
			}
			return true, wrap("relink alias", err)
		}
		if res.Relinked > 0 || res.Removed > 0 {
			w.log.Info("relinked names-only listens",
				"track", job.trackID, "relinked", res.Relinked, "reconciled", res.Removed)
		}
		return true, nil
	default:
		return false, nil
	}
}

// RelinkAlias repoints every listen carrying a names-only identity onto the
// track the alias resolved to, in pages, until there are none left.
//
// It is deliberately not user-scoped: one resolved pair repairs every account on
// the instance at once, which is the whole reason the lookup is worth a search
// request. Each page is grouped by owner so that the days it marks dirty for the
// statistics rollup are the right *local* days for each listener.
func (w *Worker) RelinkAlias(ctx context.Context, key domain.AliasKey, trackID string) (RelinkStats, error) {
	var total RelinkStats
	if key.IsZero() {
		return total, fmt.Errorf("%w: alias key is empty", domain.ErrValidation)
	}
	if trackID == "" {
		return total, fmt.Errorf("%w: a track id is required to relink an alias", domain.ErrValidation)
	}

	// The old identity is the names-only one the listens were stored under; its
	// key is what the partial index on unresolved listens is keyed by.
	identityKey := domain.TrackIdentity{Artist: key.ArtistNorm, Title: key.TitleNorm}.Key()

	// One timezone lookup per user per pass. A popular alias touches the same
	// handful of accounts on every page, and the timezone cannot change under us
	// in a way that matters: a user who changes theirs marks their whole history
	// dirty anyway.
	zones := make(map[uuid.UUID]string)
	var afterID int64

	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}

		page, err := w.dep.Listens.UnresolvedListensForIdentity(ctx, w.db(), identityKey, afterID, relinkPageSize)
		if err != nil {
			return total, wrap("list unresolved listens", err)
		}
		if len(page) == 0 {
			return total, nil
		}
		// Keyset paging on the row id: relinked and reconciled rows drop out of the
		// query's own predicate, and the cursor only ever moves forward, so this
		// terminates whatever another worker is doing at the same time.
		afterID = page[len(page)-1].ID

		groups := groupByUser(page)
		for _, g := range groups {
			if _, cached := zones[g.userID]; cached {
				continue
			}
			tz, err := w.timezone(ctx, g.userID)
			if err != nil {
				return total, err
			}
			zones[g.userID] = tz
		}

		var applied RelinkStats
		err = w.dep.Store.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			applied = RelinkStats{}
			for _, g := range groups {
				res, err := w.dep.Listens.ApplyRelink(ctx, tx, g.rows, trackID, zones[g.userID])
				if err != nil {
					return err
				}
				applied.Relinked += res.Relinked
				applied.Removed += res.Removed
			}
			return nil
		})
		if err != nil {
			return total, wrap("apply relink", err)
		}
		// Counted only once the page has committed, so an abandoned transaction
		// cannot be reported as work that happened.
		total.Relinked += applied.Relinked
		total.Removed += applied.Removed

		if len(page) < relinkPageSize {
			return total, nil
		}
	}
}

// userGroup is one page's rows for a single listener.
type userGroup struct {
	userID uuid.UUID
	rows   []listens.UnresolvedListen
}

// groupByUser splits a page by owner, preserving the order the rows arrived in
// so that the relink walks the page the same way twice.
func groupByUser(rows []listens.UnresolvedListen) []userGroup {
	if len(rows) == 0 {
		return nil
	}
	index := make(map[uuid.UUID]int, 4)
	out := make([]userGroup, 0, 4)
	for _, r := range rows {
		i, seen := index[r.UserID]
		if !seen {
			i = len(out)
			index[r.UserID] = i
			out = append(out, userGroup{userID: r.UserID})
		}
		out[i].rows = append(out[i].rows, r)
	}
	return out
}

// timezone resolves the listener's IANA timezone, which decides which local day
// a relinked listen marks dirty for the statistics rollup.
//
// A user who no longer exists falls back to UTC: their listens are on their way
// out with them, and refusing to relink would leave the pass stuck on a row that
// is about to be deleted.
func (w *Worker) timezone(ctx context.Context, userID uuid.UUID) (string, error) {
	if w.dep.Accounts == nil || w.dep.Accounts.Users == nil {
		return "UTC", nil
	}
	user, err := w.dep.Accounts.Users.GetByID(ctx, w.db(), userID)
	switch {
	case errors.Is(err, domain.ErrNotFound):
		return "UTC", nil
	case err != nil:
		return "", wrap("load user timezone", err)
	case user.Timezone == "":
		return "UTC", nil
	}
	return user.Timezone, nil
}

// queueRelink hands a freshly resolved alias to the relink loop.
//
// The send blocks rather than dropping when the queue is full. The resolver is
// rate limited to a couple of searches a second, so the relink pass is never the
// bottleneck in practice; when it is, slowing the resolver down is the right
// answer, because dropping the hand-off would leave listens carrying a
// names-only identity that nothing would ever come back to repair.
func (w *Worker) queueRelink(ctx context.Context, key domain.AliasKey, trackID string) {
	select {
	case w.relink <- relinkJob{key: key, trackID: trackID}:
	case <-ctx.Done():
	}
}

// requeueRelink puts a failed relink back at the end of the queue, up to a
// bounded number of attempts. The send is non-blocking: the loop is the only
// consumer, so blocking here on a full queue would deadlock it against itself.
func (w *Worker) requeueRelink(job relinkJob) {
	job.attempts++
	if job.attempts >= relinkAttempts {
		w.log.Warn("giving up on relinking an alias; its historical listens keep their names",
			"artist", job.key.ArtistNorm, "title", job.key.TitleNorm, "attempts", job.attempts)
		return
	}
	select {
	case w.relink <- job:
	default:
		w.log.Warn("relink queue is full; dropping a resolved alias",
			"artist", job.key.ArtistNorm, "title", job.key.TitleNorm, "depth", relinkQueueDepth)
	}
}
