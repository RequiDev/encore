// Command encore-api serves Encore's HTTP API.
//
// It owns the lifetime of the process and therefore the lifetime of everything
// that has one: the connection pool, the Spotify client and the HTTP server.
// Nothing below cmd constructs its own dependencies — that is what keeps the
// wiring in one readable place and lets every package underneath be exercised
// with fakes — so this file is the whole of the composition root.
//
// The API runs no scheduled loop: polling, imports and enrichment all belong to
// encore-worker. It does start three kinds of work on demand — a sync poller so
// the "sync now" button can poll one account, an album track fetch when
// somebody opens an album page, and an artist discography walk when somebody
// opens an artist page — all three triggered by a request and all three
// cancelled at shutdown.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"slices"
	"syscall"
	"time"

	"github.com/google/uuid"

	"github.com/RequiDev/encore/internal/albumtracks"
	"github.com/RequiDev/encore/internal/artistalbums"
	"github.com/RequiDev/encore/internal/config"
	"github.com/RequiDev/encore/internal/crypto"
	"github.com/RequiDev/encore/internal/httpapi"
	"github.com/RequiDev/encore/internal/importer"
	"github.com/RequiDev/encore/internal/logging"
	"github.com/RequiDev/encore/internal/metrics"
	"github.com/RequiDev/encore/internal/postgres"
	"github.com/RequiDev/encore/internal/spotify"
	"github.com/RequiDev/encore/internal/stats"
	"github.com/RequiDev/encore/internal/store"
	"github.com/RequiDev/encore/internal/store/accounts"
	"github.com/RequiDev/encore/internal/store/catalog"
	"github.com/RequiDev/encore/internal/store/imports"
	"github.com/RequiDev/encore/internal/store/listens"
	"github.com/RequiDev/encore/internal/sync"
	"github.com/RequiDev/encore/internal/worker"
)

// version is set at build time with -ldflags. See deploy/Dockerfile.
var version = "dev"

func main() {
	if err := run(); err != nil {
		// Nothing is logged structurally here: a process that failed to start may
		// never have had a logger, and the operator reading this is looking at a
		// terminal or a `docker compose logs` tail either way.
		fmt.Fprintln(os.Stderr, "encore-api:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		// The parser reports every problem at once, so this is the complete list
		// of what has to change. Saying where the answers live turns it into
		// something an operator can act on without reading the source.
		return fmt.Errorf("%w\n\nevery setting is documented in docs/configuration.md and .env.example", err)
	}

	lg := logging.New(logging.Options{
		Level:   cfg.Log.Level,
		Format:  cfg.Log.Format,
		Source:  cfg.Log.Source,
		Service: "api",
		Version: version,
	})
	// The single most useful line in a self-hosted deployment's logs: what this
	// process actually believes its configuration to be. Every secret in it is
	// replaced rather than shortened, so it is safe to paste into a bug report.
	lg.Info("encore-api starting", configAttrs(cfg)...)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if cfg.Database.MigrateOnStart {
		// Off by default: migrations are a deliberate, separately observable step
		// run by encore-migrate. This exists for single-container deployments
		// that have nowhere to run it.
		lg.Info("applying pending migrations before serving")
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

	// One client per process means one rate-limit budget, which is what keeps an
	// OAuth exchange and a manual sync from being throttled by each other.
	client := spotify.NewClient(cfg.Spotify, lg, worker.SpotifyPauseOptions(ctx, cfg.Spotify, accountsRepo.Settings, db, lg)...)

	intake, err := importer.NewIntake(cfg.Import, db, importsRepo, lg)
	if err != nil {
		return err
	}

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

	// The API runs no background *loop* — that is still encore-worker's job — but
	// it does start detached work on demand, the same way the "sync now" button
	// does. An album's track list is read when somebody opens that album's page,
	// and Close cancels anything still in flight at shutdown.
	albumTracks, err := albumtracks.New(cfg.AlbumTracks, albumtracks.Deps{
		Catalog: catalogRepo,
		Spotify: client,
		Writer:  albumtracks.StoreWriter{Store: db},
		Logger:  lg,
	})
	if err != nil {
		return err
	}
	defer albumTracks.Close()

	// The artist page's discography cache, on the same terms as the album track
	// cache above: read when somebody opens that artist's page, and Close cancels
	// anything still in flight at shutdown. Deferred here, after the pool is
	// open, so LIFO runs it before the pool closes — a cancelled walk still needs
	// the pool to record that it failed.
	artistAlbums, err := artistalbums.New(cfg.ArtistAlbums, artistalbums.Deps{
		Catalog: catalogRepo,
		Spotify: client,
		Writer:  artistalbums.StoreWriter{Store: db},
		Logger:  lg,
	})
	if err != nil {
		return err
	}
	defer artistAlbums.Close()

	api, err := httpapi.New(httpapi.Deps{
		Config:   cfg,
		Store:    db,
		Accounts: accountsRepo,
		Catalog:  catalogRepo,
		Listens:  listensRepo,
		Imports:  importsRepo,
		Stats:    statsSvc,
		Intake:   intake,
		Spotify:  client,
		Metrics:  reg,
		Logger:   lg,
		Version:  version,
		SyncNow:  syncNow(poller),
		// Playlists act on the listener's own account, so they need the
		// listener's own token, and refreshing one belongs to the poller.
		UserToken:    poller.AccessToken,
		AlbumTracks:  albumTracks,
		ArtistAlbums: artistAlbums,
	})
	if err != nil {
		return err
	}

	// Pool utilisation is published from here rather than from a scrape hook so
	// that the gauge is fresh even between scrapes, and so a process that is
	// wedged on the database still reports its last reading. Its own context
	// stops it even when serve returns for a reason other than a signal.
	statsCtx, stopStats := context.WithCancel(ctx)
	defer stopStats()
	go publishPoolStats(statsCtx, reg, pool)

	return serve(ctx, stop, cfg, api.Handler(), lg)
}

// syncNow adapts the poller to what POST /api/sync/now expects.
//
// The three sentinels the poller distinguishes are the ones a listener can do
// something about, so they are translated into the API's error envelope here.
// Everything else is left alone and becomes a logged 500, which is the right
// answer for a failure the caller cannot act on.
func syncNow(p *sync.Poller) func(context.Context, uuid.UUID) (httpapi.SyncOutcome, error) {
	return func(ctx context.Context, userID uuid.UUID) (httpapi.SyncOutcome, error) {
		res, err := p.SyncUser(ctx, userID)
		out := httpapi.SyncOutcome{
			Fetched:    res.Fetched,
			Imported:   res.Imported,
			Duplicates: res.Duplicates,
			Skipped:    res.Skipped,
		}
		if !res.NewestPlayedAt.IsZero() {
			newest := res.NewestPlayedAt.UTC()
			out.NewestAt = &newest
		}
		switch {
		case err == nil:
			return out, nil
		case errors.Is(err, sync.ErrAlreadyRunning):
			return out, httpapi.ErrConflictf("A synchronisation is already running for your account.")
		case errors.Is(err, sync.ErrNotConnected):
			return out, httpapi.ErrConflictf("Your account is not connected to Spotify. Connect it and try again.")
		case errors.Is(err, sync.ErrNeedsReauth):
			return out, httpapi.ErrConflictf("Spotify has rejected the stored authorisation. Reconnect your account to resume syncing.")
		default:
			return out, err
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

// poolStatsInterval is how often connection-pool utilisation is republished.
// Acquired against max is the importer's backpressure signal, and it is the
// first thing worth looking at when the API has gone slow, so it is refreshed
// often enough to see a spike rather than only its average.
const poolStatsInterval = 15 * time.Second

// publishPoolStats keeps the pool gauges current until ctx is cancelled.
func publishPoolStats(ctx context.Context, reg *metrics.Registry, pool *postgres.Pool) {
	t := time.NewTicker(poolStatsInterval)
	defer t.Stop()

	for {
		reg.SetPoolStats(postgres.PoolStats(pool))
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// syncMetrics adapts internal/sync's telemetry interface onto the registry. The
// package deliberately does not depend on Prometheus, so the translation lives
// here, in the only place that knows both.
type syncMetrics struct{ reg *metrics.Registry }

func (m syncMetrics) SyncRun(result string)       { m.reg.ObserveSyncRun(metrics.Result(result)) }
func (m syncMetrics) SyncListens(n int64)         { m.reg.AddSyncListens(n) }
func (m syncMetrics) SyncLastSuccess(t time.Time) { m.reg.SetSyncLastSuccess(t) }
