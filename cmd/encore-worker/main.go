// Command encore-worker runs Encore's background work: import jobs, catalogue
// enrichment, the recently-played poller, rollup maintenance and the reaper that
// clears expired sessions and OAuth states.
//
// It is a separate process from the API on purpose. A one-million-record import
// saturates a database connection for minutes, and letting that compete with a
// dashboard request would make the interface unusable exactly when the user most
// wants to watch the progress bar. It also makes crash recovery testable: this
// process can be killed at any instant, because every piece of state worth
// keeping — leases, checkpoints, sync cursors — lives in the database.
//
// Every loop runs under internal/worker's supervisor, so a failure in one is
// contained to that loop. The process also serves a small HTTP listener with
// nothing on it but /healthz, /readyz and /metrics, because the Compose
// healthcheck needs somewhere to knock.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"syscall"
	"time"

	"github.com/requi/encore/internal/config"
	"github.com/requi/encore/internal/crypto"
	"github.com/requi/encore/internal/enrich"
	"github.com/requi/encore/internal/importer"
	"github.com/requi/encore/internal/logging"
	"github.com/requi/encore/internal/metrics"
	"github.com/requi/encore/internal/postgres"
	"github.com/requi/encore/internal/spotify"
	"github.com/requi/encore/internal/stats"
	"github.com/requi/encore/internal/store"
	"github.com/requi/encore/internal/store/accounts"
	"github.com/requi/encore/internal/store/catalog"
	"github.com/requi/encore/internal/store/imports"
	"github.com/requi/encore/internal/store/listens"
	"github.com/requi/encore/internal/sync"
	"github.com/requi/encore/internal/worker"
)

// version is set at build time with -ldflags. See deploy/Dockerfile.
var version = "dev"

// healthShutdownTimeout bounds stopping the health listener once the loops have
// stopped. Nothing but a probe is ever in flight on it, so it does not deserve a
// share of the operator's grace period; that budget belongs to the loops.
const healthShutdownTimeout = 5 * time.Second

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "encore-worker:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		// The parser reports every problem at once, so this is the complete list
		// of what has to change.
		return fmt.Errorf("%w\n\nevery setting is documented in docs/configuration.md and .env.example", err)
	}

	lg := logging.New(logging.Options{
		Level:   cfg.Log.Level,
		Format:  cfg.Log.Format,
		Source:  cfg.Log.Source,
		Service: "worker",
		Version: version,
	})
	// The single most useful line in a self-hosted deployment's logs: what this
	// process actually believes its configuration to be, secrets replaced.
	lg.Info("encore-worker starting", configAttrs(cfg)...)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if cfg.Database.MigrateOnStart {
		// goose takes a session advisory lock, so an API and a worker starting at
		// the same instant still apply each migration exactly once.
		lg.Info("applying pending migrations before starting work")
		if err := postgres.Migrate(ctx, cfg.Database.URL, lg); err != nil {
			return err
		}
	}

	pool, err := postgres.Connect(ctx, cfg.Database, lg)
	if err != nil {
		return err
	}
	defer pool.Close()

	sealer, err := crypto.NewSealer(cfg.Security.EncryptionKey)
	if err != nil {
		return fmt.Errorf("ENCORE_ENCRYPTION_KEY is not usable: %w", err)
	}
	db, err := store.New(pool, sealer)
	if err != nil {
		return err
	}

	reg := metrics.New()
	accountsRepo := accounts.New(db)
	catalogRepo := catalog.New(db)
	listensRepo := listens.New(db)
	importsRepo := imports.New(db)
	statsSvc := stats.New(db)

	// One client for the whole process, so enrichment and the poller draw on a
	// single rate budget and a 429 pauses both rather than only the loop that
	// provoked it.
	client := spotify.NewClient(cfg.Spotify, lg)

	sup := worker.New(lg).WithGrace(cfg.HTTP.ShutdownTimeout)

	// The job gauge is shared by every runner because it describes the process;
	// the byte accounting is not, because it describes one file at a time.
	jobGauge := newImportJobs(reg)
	for i := 1; i <= cfg.Import.Workers; i++ {
		// Each runner needs a lease owner of its own: two runners sharing an id
		// would each accept the other's heartbeat as proof it still held the
		// lease, and the lease would stop meaning "exactly one worker is on this
		// job". ENCORE_WORKER_ID distinguishes the container; the suffix
		// distinguishes the runner inside it.
		owner := fmt.Sprintf("%s/%d", cfg.Worker.ID, i)
		runner, err := importer.NewRunner(cfg.Import, owner, importer.Deps{
			Store:    db,
			Jobs:     importsRepo,
			Listens:  listensRepo,
			Accounts: accountsRepo,
			Logger:   lg,
			Metrics:  newImportMetrics(reg, jobGauge),
		})
		if err != nil {
			return err
		}
		sup.Add(fmt.Sprintf("import-%d", i), runner.Run)
	}

	enricher, err := enrich.New(cfg.Enrich, enrich.Deps{
		Store:    db,
		Catalog:  catalogRepo,
		Listens:  listensRepo,
		Accounts: accountsRepo,
		Stats:    statsSvc,
		Spotify:  client,
		Logger:   lg,
		Metrics:  enrichMetrics{reg: reg},
	})
	if err != nil {
		return err
	}
	// Started unconditionally: Run honours ENCORE_ENRICH_ENABLED itself, and with
	// enrichment off it still runs the rollup loop, because recomputing dirty
	// statistics days is a database concern that has nothing to do with Spotify.
	sup.Add("enrich", enricher.Run)

	poller, err := sync.NewPoller(cfg.Sync, sync.Deps{
		Store:    db,
		Accounts: accountsRepo,
		Listens:  listensRepo,
		Catalog:  catalogRepo,
		Spotify:  client,
		Logger:   lg,
		Metrics:  syncMetrics{reg: reg},
	})
	if err != nil {
		return err
	}
	// Also unconditional: with ENCORE_SYNC_ENABLED false, Run says so once and
	// returns nil, which the supervisor treats as a loop that has finished.
	sup.Add("sync", poller.Run)

	sup.Add("reaper", reaper(db, accountsRepo, lg))
	sup.Add("telemetry", publishPoolStats(reg, pool))

	// Bound the listener before anything else starts: a busy ENCORE_HTTP_ADDR is
	// a startup failure, not something to discover from a failing healthcheck.
	ln, err := net.Listen("tcp", cfg.HTTP.Addr)
	if err != nil {
		return fmt.Errorf("listen on ENCORE_HTTP_ADDR %s: %w", cfg.HTTP.Addr, err)
	}

	srv := &http.Server{
		Handler:           healthHandler(cfg, pool, reg, lg),
		ReadHeaderTimeout: cfg.HTTP.ReadTimeout,
		ReadTimeout:       cfg.HTTP.ReadTimeout,
		WriteTimeout:      cfg.HTTP.WriteTimeout,
		IdleTimeout:       cfg.HTTP.IdleTimeout,
		ErrorLog:          slog.NewLogLogger(lg.Handler(), slog.LevelWarn),
	}

	// A listener that dies takes the loops with it. The healthcheck would fail
	// either way, and an orderly stop is better than a container that reports
	// itself unhealthy while quietly continuing to import.
	loopCtx, cancelLoops := context.WithCancel(ctx)
	defer cancelLoops()
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			lg.Error("the health listener stopped", logging.Err(err))
			cancelLoops()
		}
	}()
	lg.Info("health listener started", "addr", ln.Addr().String())

	runErr := sup.Run(loopCtx)
	// An impatient second signal should kill the process rather than be absorbed
	// by the shutdown the first one started.
	stop()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), healthShutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		lg.Warn("the health listener did not stop cleanly", logging.Err(err))
	}

	if runErr != nil {
		return runErr
	}
	lg.Info("encore-worker stopped")
	return nil
}

// reapInterval is how often expired sessions and OAuth states are deleted.
//
// Neither is a security boundary: both carry an expiry column and every read
// filters on it, so an expired row is already unusable. This is disk hygiene,
// and a few minutes is far more often than it needs to be for two indexed
// deletes.
const reapInterval = 5 * time.Minute

// reaper deletes expired sessions and OAuth states until ctx is cancelled.
func reaper(db *store.Store, repo *accounts.Repo, lg *slog.Logger) func(context.Context) error {
	log := lg.With("component", "reaper")

	return func(ctx context.Context) error {
		t := time.NewTicker(reapInterval)
		defer t.Stop()

		for {
			// A failed reap is logged and left to the next tick rather than
			// returned: five minutes is already a better backoff than a restart
			// would give it, and nothing downstream depends on the rows going.
			if n, err := repo.Sessions.DeleteExpired(ctx, db.DB(), time.Now()); err != nil {
				if ctx.Err() == nil {
					log.Warn("could not delete expired sessions", logging.Err(err))
				}
			} else if n > 0 {
				log.Info("expired sessions deleted", "count", n)
			}

			if n, err := repo.OAuthStates.DeleteExpired(ctx, db.DB(), time.Now()); err != nil {
				if ctx.Err() == nil {
					log.Warn("could not delete expired oauth states", logging.Err(err))
				}
			} else if n > 0 {
				log.Info("expired oauth states deleted", "count", n)
			}

			select {
			case <-ctx.Done():
				return nil
			case <-t.C:
			}
		}
	}
}

// poolStatsInterval is how often connection-pool utilisation is republished.
// Acquired against max is the importer's backpressure signal — when they meet,
// the file reader is waiting on the database rather than the other way round —
// so it is refreshed often enough to see a spike rather than only its average.
const poolStatsInterval = 15 * time.Second

// publishPoolStats keeps the pool gauges current until ctx is cancelled.
func publishPoolStats(reg *metrics.Registry, pool *postgres.Pool) func(context.Context) error {
	return func(ctx context.Context) error {
		t := time.NewTicker(poolStatsInterval)
		defer t.Stop()

		for {
			reg.SetPoolStats(postgres.PoolStats(pool))
			select {
			case <-ctx.Done():
				return nil
			case <-t.C:
			}
		}
	}
}

// configAttrs renders the redacted configuration as sorted log attributes, so
// that two restarts of the same deployment produce diffable lines.
func configAttrs(cfg *config.Config) []any {
	redacted := cfg.Redacted()
	keys := make([]string, 0, len(redacted))
	for k := range redacted {
		keys = append(keys, k)
	}
	slices.Sort(keys)

	attrs := make([]any, 0, len(keys)*2)
	for _, k := range keys {
		attrs = append(attrs, k, redacted[k])
	}
	return attrs
}
