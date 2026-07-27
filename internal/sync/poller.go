package sync

import (
	"context"
	"errors"
	"fmt"
	stdsync "sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/RequiDev/encore/internal/logging"
)

// tickJitter is the fraction of the interval each delay is randomised by.
//
// It exists because several worker containers started by the same deployment
// would otherwise poll on the same second for ever, presenting Spotify with a
// burst equal to the whole fleet and then a minute of silence. Jitter is applied
// symmetrically, so the long-run polling rate is still the configured interval.
const tickJitter = 0.2

// accountsPerWorker bounds one tick's work relative to how much of it can
// actually run at once. Accounts are handed out least-recently-polled first, so
// anything left over is simply picked up by the next tick rather than starved.
const accountsPerWorker = 50

// maxAccountsPerTick is the hard ceiling on one tick, matching the limit the
// credentials repository clamps to.
const maxAccountsPerTick = 500

// Run polls every due account, forever, until ctx is cancelled.
//
// The loop is deliberately stateless: each tick asks the database which accounts
// are due rather than keeping a schedule in memory, so two worker processes
// share the work without coordinating and a restart loses nothing.
func (p *Poller) Run(ctx context.Context) error {
	if !p.cfg.Enabled {
		p.log.Info("recently-played sync is disabled")
		return nil
	}
	p.log.Info("recently-played sync started",
		"interval", p.cfg.Interval.String(),
		"concurrency", p.cfg.Concurrency,
		"initial_lookback", p.cfg.InitialLookback.String())

	// The first delay is drawn from the whole interval rather than jittered
	// around it, which is what actually spreads a fleet that all started at
	// once; subsequent delays only keep them from converging again.
	timer := time.NewTimer(p.firstDelay())
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-timer.C:
		}

		if _, err := p.RunOnce(ctx); err != nil && ctx.Err() == nil {
			// Listing the work failed, which is an infrastructure problem rather
			// than an account problem: log it and wait for the next tick instead
			// of spinning against a database that is down.
			p.log.Error("sync tick failed", logging.Err(err))
		}
		timer.Reset(p.nextDelay())
	}
}

// RunOnce polls every account that is currently due and reports how many were
// actually polled.
//
// It is exported so a worker supervisor, or a test, can drive one tick without
// owning the schedule.
func (p *Poller) RunOnce(ctx context.Context) (int, error) {
	// Due means "not polled within the last interval". Accounts that have never
	// been polled sort first, so a freshly connected one gets its history without
	// waiting behind everybody else.
	due, err := p.dep.Accounts.Credentials.ListDueForSync(
		ctx, p.dep.Store.DB(), p.now().Add(-p.cfg.Interval), p.batchLimit())
	if err != nil {
		return 0, fmt.Errorf("list accounts due for sync: %w", err)
	}
	if len(due) == 0 {
		return 0, nil
	}

	var (
		polled atomic.Int64
		wg     stdsync.WaitGroup
		// sem admits cfg.Concurrency accounts at a time. The bound is what keeps
		// one tick from presenting the whole instance to Spotify at once.
		sem = make(chan struct{}, p.cfg.Concurrency)
	)

	// No shared error group: one account's failure must never cancel the work of
	// the others, so each poll is isolated and reports itself.
dispatch:
	for _, userID := range due {
		select {
		case <-ctx.Done():
			break dispatch
		case sem <- struct{}{}:
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			if p.syncAccount(ctx, userID) {
				polled.Add(1)
			}
		}()
	}
	wg.Wait()

	return int(polled.Load()), nil
}

// syncAccount polls one account and reports whether the poll ran.
//
// It never returns an error. Everything that can go wrong with one grant is
// logged, counted and recorded on the credential row for the account page to
// show, because one broken connection must not cost every other listener their
// synchronisation.
func (p *Poller) syncAccount(ctx context.Context, userID uuid.UUID) bool {
	log := p.log.With("user", userID.String())

	res, err := p.SyncUser(ctx, userID)
	switch {
	case err == nil:
		return true

	case errors.Is(err, ErrAlreadyRunning):
		// A previous tick is still working on this account. Leaving it alone is
		// the whole point of the claim.
		log.Debug("account is already being polled; skipping this tick")
		return false

	case errors.Is(err, ErrNeedsReauth):
		log.Warn("account parked until it is authorised again; it will not be polled")
		return true

	case errors.Is(err, ErrNotConnected):
		// The grant was deleted between the listing and the poll. Nothing to do.
		log.Debug("account is no longer connected to Spotify")
		return false

	case ctx.Err() != nil:
		// Shutting down. The next worker to run the tick picks the account up,
		// and the cursor has not moved, so nothing is lost.
		log.Debug("sync interrupted by shutdown")
		return false

	default:
		log.Error("recently-played sync failed", "fetched", res.Fetched, logging.Err(err))
		return true
	}
}

// batchLimit is how many accounts one tick will take on.
func (p *Poller) batchLimit() int {
	n := p.cfg.Concurrency * accountsPerWorker
	if n > maxAccountsPerTick {
		return maxAccountsPerTick
	}
	return n
}

// firstDelay spreads the first tick of freshly started processes across a whole
// interval.
func (p *Poller) firstDelay() time.Duration {
	return time.Duration(p.rnd() * float64(p.cfg.Interval))
}

// nextDelay is the configured interval with symmetric jitter applied, so
// processes that happen to align drift apart again instead of staying in step.
func (p *Poller) nextDelay() time.Duration {
	spread := float64(p.cfg.Interval) * tickJitter
	d := float64(p.cfg.Interval) - spread/2 + p.rnd()*spread
	if d < float64(time.Second) {
		// A pathologically small interval must still not become a busy loop.
		return time.Second
	}
	return time.Duration(d)
}
