package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/requi/encore/internal/config"
	"github.com/requi/encore/internal/httpapi"
	"github.com/requi/encore/internal/logging"
	"github.com/requi/encore/internal/metrics"
	"github.com/requi/encore/internal/postgres"
)

// readyCacheTTL is how long a successful readiness result is trusted.
//
// Only success is cached. A probe that has just been told the worker is not
// ready must see the recovery immediately, whereas asking goose for the schema
// version opens a connection of its own, and doing that on every probe would be
// a needless load spike on an instance with aggressive health checks.
const readyCacheTTL = 30 * time.Second

// healthHandler builds the worker's whole HTTP surface: liveness, readiness and,
// when it is enabled, the metrics exposition.
//
// It is deliberately not internal/httpapi. The worker serves no API, and
// mounting the real router here would give the worker's port an authentication
// surface, a session cookie and an upload endpoint that nothing should be able
// to reach. The response bodies are the API's DTO, so the two processes answer
// a probe identically.
func healthHandler(cfg *config.Config, pool *postgres.Pool, reg *metrics.Registry, lg *slog.Logger) http.Handler {
	mux := http.NewServeMux()
	ready := &readyCache{}

	// Liveness never touches the database: restarting a worker because Postgres
	// is down turns an outage into a crash loop that makes it harder to fix.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeHealth(w, r, lg, http.StatusOK, httpapi.HealthResponse{Status: "ok"})
	})

	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		status, body := readiness(r.Context(), cfg, pool, ready, lg)
		writeHealth(w, r, lg, status, body)
	})

	if cfg.Metrics.Enabled && reg != nil {
		// The handler carries the concurrency limit and the scrape timeout, so it
		// is built once rather than per request.
		mux.Handle("GET /metrics", reg.Handler(cfg.Metrics.Username, cfg.Metrics.Password))
	}
	return mux
}

// readiness answers the stronger question: the database has to respond *and*
// every embedded migration has to be applied, because a worker talking to an
// out-of-date schema fails confusingly rather than obviously.
func readiness(
	ctx context.Context,
	cfg *config.Config,
	pool *postgres.Pool,
	cache *readyCache,
	lg *slog.Logger,
) (int, httpapi.HealthResponse) {
	checks := map[string]string{}
	ready := true

	if err := postgres.Health(ctx, pool); err != nil {
		// The error is logged, never returned: a connection failure can quote the
		// host, the user and occasionally more.
		lg.Warn("readiness: the database did not answer", logging.Err(err))
		checks["database"] = "unavailable"
		ready = false
	} else {
		checks["database"] = "ok"
	}

	switch {
	case !ready:
		// Asking about migrations would only produce a second connection error.
		checks["migrations"] = "unknown"
	case cache.fresh(time.Now()):
		checks["migrations"] = "ok"
	default:
		status, err := postgres.Status(ctx, cfg.Database.URL)
		switch {
		case err != nil:
			lg.Warn("readiness: could not read the schema version", logging.Err(err))
			checks["migrations"] = "unknown"
			ready = false
		case !status.UpToDate():
			checks["migrations"] = "pending"
			ready = false
		default:
			checks["migrations"] = "ok"
			cache.markFresh(time.Now())
		}
	}

	if !ready {
		return http.StatusServiceUnavailable, httpapi.HealthResponse{Status: "not_ready", Checks: checks}
	}
	return http.StatusOK, httpapi.HealthResponse{Status: "ok", Checks: checks}
}

// writeHealth sends a probe response.
func writeHealth(w http.ResponseWriter, r *http.Request, lg *slog.Logger, status int, body httpapi.HealthResponse) {
	payload, err := json.Marshal(body)
	if err != nil {
		// Two fixed strings and a map of fixed strings cannot fail to encode, but
		// a probe must answer something rather than nothing if it ever did.
		lg.Error("could not encode the health response", logging.Err(err))
		payload, status = []byte(`{"status":"error"}`), http.StatusInternalServerError
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if r.Method == http.MethodHead {
		return
	}
	if _, err := w.Write(payload); err != nil {
		lg.Debug("could not write the health response", logging.Err(err))
	}
}

// readyCache remembers that the schema was up to date.
type readyCache struct {
	mu    sync.Mutex
	until time.Time
}

func (c *readyCache) fresh(now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return now.Before(c.until)
}

func (c *readyCache) markFresh(now time.Time) {
	c.mu.Lock()
	c.until = now.Add(readyCacheTTL)
	c.mu.Unlock()
}
