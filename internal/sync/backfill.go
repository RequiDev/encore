package sync

import (
	"context"

	"github.com/google/uuid"

	"github.com/RequiDev/encore/internal/logging"
)

// backfillPlaybackContext attaches what the now-playing poller saw to the plays
// this account has just finished having ingested.
//
// Deliberately outside commit's transaction, and deliberately unable to fail a
// sync. Ingestion is the product; this is a bonus. A backfill that could roll
// back an insert would let a bad join cost a listener their listening history,
// which inverts the importance of the two things entirely. The statement is a
// single UPDATE, so it is atomic on its own and needs no transaction of its own
// either.
//
// It runs after every successful poll rather than only after one that inserted
// rows, because a play can reach /me/player/recently-played on a later tick than
// the one that observed it — and because the pass is a single indexed statement
// bounded to one user and thirty hours, which costs nothing to repeat and
// nothing at all on an instance whose observation log is empty.
//
// That cadence is also what makes the observation log's twenty-four hour
// retention safe. Nothing prunes an observation for having been used, so the
// only thing keeping the log from outliving its usefulness is that a pass runs
// far more often than the retention window — every sync tick, a minute by
// default. The two loops share a process, so an observation can only age out
// while nothing at all is running; when that happens the columns stay NULL,
// which is indistinguishable from never having looked and is the benign
// direction. See listens.BackfillLookback for the other half of the argument.
func (p *Poller) backfillPlaybackContext(ctx context.Context, userID uuid.UUID) {
	n, err := p.dep.Listens.BackfillPlaybackContext(ctx, p.dep.Store.DB(), userID, p.now())
	if err != nil {
		if ctx.Err() == nil {
			p.log.Warn("could not backfill playback context",
				"user", userID.String(), logging.Err(err))
		}
		return
	}
	if n > 0 {
		p.log.Debug("playback context backfilled", "user", userID.String(), "listens", n)
	}
}
