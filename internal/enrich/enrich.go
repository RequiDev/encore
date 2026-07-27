// Package enrich fills in catalogue metadata after ingestion and converges
// names-only listens onto real Spotify tracks.
//
// Everything here is deliberately downstream of the import path. Ingestion
// writes tracks, albums, artists and (artist, title) aliases in the 'pending'
// state and never calls Spotify; this package is what turns those rows into
// names, artwork and durations later, on its own schedule. If it were switched
// off permanently every import would still complete and no listening record
// would be lost — only the names would be missing. Nothing in this package may
// ever fail an import.
//
// Seven loops run concurrently and independently, each supervised so that a loop
// which cannot reach Spotify, or a database that is briefly unreachable, never
// stops the others:
//
//	tracks   claim pending ids, GET /v1/tracks in batches of fifty
//	albums   the same, in batches of twenty, which is Spotify's limit
//	artists  the same, in batches of fifty
//	aliases  resolve one (artist, title) pair per /v1/search request
//	relink   repoint the names-only listens of a freshly resolved alias
//	repair   return rows parked in the 'failed' state to the queue
//	rollups  recompute the statistics days ingestion marked dirty
//
// Catalogue reads use the client-credentials application token, so enrichment
// works on an instance where nobody is connected and never spends a listener's
// own rate budget. Every loop also has a RunXxxOnce method that performs exactly
// one step and returns, so a test can drive the subsystem deterministically
// instead of waiting on timers.
//
// Failure handling follows docs/import.md §10. An id Spotify answers for with
// null is unavailable and leaves the queue; any other failure increments
// fetch_attempts and pushes next_attempt_at out by domain.NextMetadataAttempt
// plus jitter, and after domain.BackoffAttempts the row is parked as failed for
// the repair job. A 429 is not backed off here at all: the Spotify client
// already pauses every request in the process for the duration of Retry-After,
// and a second backoff on top of that would only make the pause longer than
// Spotify asked for.
package enrich

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/RequiDev/encore/internal/config"
	"github.com/RequiDev/encore/internal/domain"
	"github.com/RequiDev/encore/internal/logging"
	"github.com/RequiDev/encore/internal/retry"
	"github.com/RequiDev/encore/internal/spotify"
	"github.com/RequiDev/encore/internal/stats"
	"github.com/RequiDev/encore/internal/store"
	"github.com/RequiDev/encore/internal/store/accounts"
	"github.com/RequiDev/encore/internal/store/catalog"
	"github.com/RequiDev/encore/internal/store/listens"
)

// kindAlias labels alias telemetry. The three catalogue kinds label themselves
// through catalog.Kind.String(); an alias is not a catalogue table, so its label
// is spelled out here to match the metrics registry.
const kindAlias = "alias"

// attemptJitter is the fraction of a computed backoff that is randomised.
//
// Fifty ids claimed in one batch fail together and would otherwise all become
// claimable in the same second, which turns one failed batch into a synchronised
// retry for ever after. A fifth of the delay is enough to break that up without
// materially changing how long a row waits.
const attemptJitter = 0.2

// claimLease is how long a claimed batch stays invisible to other workers.
//
// It has to outlast a fetch queued behind the shared rate limiter, its retries,
// and a 429 pause. A worker that dies mid-batch costs exactly one lease of
// delay and nothing else, so the lease is generous rather than tight.
const claimLease = 5 * time.Minute

// loopBackoff is the schedule a failing loop backs off on before trying again.
// It is bounded and jittered like every other retry in Encore; the loop itself
// never gives up, because the condition it is waiting out — Spotify down, the
// database restarting — is one that resolves on its own.
var loopBackoff = retry.Policy{Base: time.Second, Max: time.Minute, Multiplier: 2, Jitter: 1}

// Metrics receives enrichment telemetry.
//
// It is an interface so this package does not depend on Prometheus: cmd wires
// the real collector in and tests use the zero value.
type Metrics interface {
	// EnrichPending publishes the backlog for one kind of entity.
	EnrichPending(kind string, n int64)
	// EnrichResolved records n entities that gained metadata.
	EnrichResolved(kind string, n int64)
	// EnrichFailed records n entities enrichment could not resolve, whether
	// because Spotify has nothing for them or because the attempt failed.
	EnrichFailed(kind string, n int64)
	// EnrichRateLimited records that Spotify asked the process to slow down.
	EnrichRateLimited()
}

// NopMetrics discards telemetry.
type NopMetrics struct{}

func (NopMetrics) EnrichPending(string, int64)  {}
func (NopMetrics) EnrichResolved(string, int64) {}
func (NopMetrics) EnrichFailed(string, int64)   {}
func (NopMetrics) EnrichRateLimited()           {}

// Deps are the collaborators a Worker needs.
//
// Accounts is optional: without it the relink pass marks dirty rollup days in
// UTC instead of in each listener's own timezone, which costs a recomputation
// rather than a wrong answer.
type Deps struct {
	Store    *store.Store
	Catalog  *catalog.Repo
	Listens  *listens.Repo
	Accounts *accounts.Repo
	Stats    *stats.Service
	Spotify  *spotify.Client
	Logger   *slog.Logger
	Metrics  Metrics
	// Now is injectable so tests can control the backoff schedule without waiting.
	Now func() time.Time
	// Rand supplies jitter in [0,1). Injected by tests for determinism.
	Rand func() float64
}

// Worker runs the enrichment loops.
type Worker struct {
	cfg  config.Enrich
	dep  Deps
	log  *slog.Logger
	stat Metrics
	now  func() time.Time
	rand func() float64

	// aliasRate paces /v1/search independently of the client's own limiter, so
	// alias resolution cannot crowd out the catalogue batches with which it
	// shares the application's quota.
	aliasRate *spotify.Limiter

	// relink carries freshly resolved aliases to the relink loop.
	relink chan relinkJob
}

// New builds a Worker.
//
// Run honours cfg.Enabled itself rather than leaving it to the caller: with
// enrichment disabled only the rollup loop runs, because the daily rollup is a
// statistics concern that has nothing to do with Spotify and must keep up even
// on an instance that never talks to it.
func New(cfg config.Enrich, deps Deps) (*Worker, error) {
	switch {
	case deps.Store == nil:
		return nil, errors.New("enrich: a store is required")
	case deps.Catalog == nil || deps.Listens == nil:
		return nil, errors.New("enrich: the catalog and listens repositories are required")
	case deps.Stats == nil:
		return nil, errors.New("enrich: the stats service is required")
	case deps.Spotify == nil:
		return nil, errors.New("enrich: a spotify client is required")
	}

	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	if deps.Metrics == nil {
		deps.Metrics = NopMetrics{}
	}
	if deps.Now == nil {
		deps.Now = time.Now
	}
	if deps.Rand == nil {
		deps.Rand = defaultRand
	}

	cfg = withDefaults(cfg)
	return &Worker{
		cfg:       cfg,
		dep:       deps,
		log:       deps.Logger.With("component", "enrich"),
		stat:      deps.Metrics,
		now:       deps.Now,
		rand:      deps.Rand,
		aliasRate: spotify.NewLimiter(cfg.AliasRate, 1),
		relink:    make(chan relinkJob, relinkQueueDepth),
	}, nil
}

// withDefaults fills in intervals a hand-built configuration may have left at
// zero. config.Load never produces zeroes, but a caller constructing a
// config.Enrich directly would otherwise get a loop that spins.
func withDefaults(cfg config.Enrich) config.Enrich {
	if cfg.Interval <= 0 {
		cfg.Interval = 5 * time.Second
	}
	if cfg.RepairInterval <= 0 {
		cfg.RepairInterval = 6 * time.Hour
	}
	if cfg.RollupInterval <= 0 {
		cfg.RollupInterval = 30 * time.Second
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = spotify.MaxTrackIDsPerRequest
	}
	if cfg.AliasRate <= 0 {
		cfg.AliasRate = 2
	}
	return cfg
}

// Run starts every loop and blocks until ctx is cancelled.
//
// Each loop is supervised independently: a step that fails is logged and retried
// after a bounded backoff, and no loop can end another. Run returns nil on
// shutdown, because a cancelled context is the operator's decision rather than a
// failure.
func (w *Worker) Run(ctx context.Context) error {
	type namedLoop struct {
		name     string
		interval time.Duration
		step     func(context.Context) (bool, error)
	}

	// The rollup loop runs whatever cfg.Enabled says; see New.
	loops := []namedLoop{
		{"rollups", w.cfg.RollupInterval, w.rollupStep()},
	}
	if w.cfg.Enabled {
		loops = append(loops,
			namedLoop{"tracks", w.cfg.Interval, counted(w.RunTracksOnce)},
			namedLoop{"albums", w.cfg.Interval, counted(w.RunAlbumsOnce)},
			namedLoop{"artists", w.cfg.Interval, counted(w.RunArtistsOnce)},
			namedLoop{"repair", w.cfg.RepairInterval, timed(w.RunRepairOnce)},
		)
		if w.cfg.AliasEnabled {
			loops = append(loops,
				namedLoop{"aliases", w.cfg.Interval, counted(w.RunAliasesOnce)},
				namedLoop{"relink", w.cfg.Interval, w.RunRelinkOnce},
			)
		}
	}

	names := make([]string, 0, len(loops))
	for _, l := range loops {
		names = append(names, l.name)
	}
	w.log.Info("enrichment started", "loops", names, "enabled", w.cfg.Enabled, "aliases", w.cfg.AliasEnabled)

	var wg sync.WaitGroup
	for _, l := range loops {
		wg.Add(1)
		go func(l namedLoop) {
			defer wg.Done()
			w.loop(ctx, l.name, l.interval, l.step)
		}(l)
	}
	wg.Wait()

	w.log.Info("enrichment stopped")
	return nil
}

// loop runs one step function until ctx is done.
//
// A step that found work runs again immediately: a backlog then drains at
// whatever rate the shared Spotify limiter allows rather than at the poll
// interval, which for a freshly imported million-record history is the
// difference between hours and days. An idle step waits out the interval, and a
// failing one backs off.
func (w *Worker) loop(ctx context.Context, name string, interval time.Duration, step func(context.Context) (bool, error)) {
	log := w.log.With("loop", name)
	failures := 0

	for {
		if ctx.Err() != nil {
			return
		}
		busy, err := step(ctx)
		switch {
		case err != nil && ctx.Err() != nil:
			// Shutting down mid-step is not a failure worth logging.
			return
		case err != nil:
			failures++
			delay := loopBackoff.Jittered(failures+1, w.rand)
			log.Error("enrichment step failed", "retry_in", delay.String(), logging.Err(err))
			if !w.sleep(ctx, delay) {
				return
			}
		case busy:
			failures = 0
		default:
			failures = 0
			if !w.sleep(ctx, interval) {
				return
			}
		}
	}
}

// counted adapts a step that reports how many items it handled: while it keeps
// finding work, the loop keeps running it.
func counted(step func(context.Context) (int, error)) func(context.Context) (bool, error) {
	return func(ctx context.Context) (bool, error) {
		n, err := step(ctx)
		return n > 0, err
	}
}

// timed adapts a step that runs on a fixed cadence: however much it did, the
// loop waits out its interval before running it again.
func timed(step func(context.Context) (int, error)) func(context.Context) (bool, error) {
	return func(ctx context.Context) (bool, error) {
		_, err := step(ctx)
		return false, err
	}
}

// sleep waits for d, reporting false if ctx ended first.
func (w *Worker) sleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// nextAttempt is when a row that has just failed becomes claimable again: the
// domain's deterministic backoff for that attempt number, plus jitter.
func (w *Worker) nextAttempt(attempts int32) time.Time {
	return w.now().Add(jitterDelay(domain.NextMetadataAttempt(attempts), attemptJitter, w.rand))
}

// defaultRand is the jitter source used when Deps.Rand is not supplied. The
// global generator is used rather than a seeded one because nothing here needs
// reproducibility outside a test, which injects its own.
func defaultRand() float64 { return rand.Float64() }

// jitterDelay spreads a delay forward by up to frac of itself. It only ever adds
// time, so a backoff can be later than the schedule says but never earlier.
func jitterDelay(d time.Duration, frac float64, rnd func() float64) time.Duration {
	if d <= 0 || frac <= 0 {
		return d
	}
	if rnd == nil {
		rnd = defaultRand
	}
	return d + time.Duration(rnd()*frac*float64(d))
}

// rateLimited reports whether a failure is Spotify's 429.
//
// It is deliberately the only thing this package does about rate limiting. The
// client has already paused every request in the process for the duration of
// Retry-After by the time this error surfaces, so the claim is simply left to
// expire and the ids come back once the pause has cleared.
func rateLimited(err error) bool {
	apiErr, ok := spotify.AsAPIError(err)
	return ok && apiErr.IsRateLimited()
}

// missingIDs returns the requested ids that are absent from got, preserving the
// order they were requested in. Those are the ids Spotify answered for with
// null: deleted, region-locked, or relinked to something that no longer exists.
func missingIDs(requested, got []string) []string {
	if len(requested) == 0 {
		return nil
	}
	have := make(map[string]struct{}, len(got))
	for _, id := range got {
		have[id] = struct{}{}
	}
	out := make([]string, 0, len(requested))
	for _, id := range requested {
		if _, ok := have[id]; ok {
			continue
		}
		out = append(out, id)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// db is the pool as a Querier, for the statements that need no transaction.
func (w *Worker) db() store.Querier { return w.dep.Store.DB() }

// wrap adds the operation to an error without hiding its chain, so callers can
// still test it with errors.Is and errors.As.
func wrap(op string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", op, err)
}
