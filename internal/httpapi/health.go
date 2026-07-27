package httpapi

import (
	"net/http"

	"github.com/RequiDev/encore/internal/logging"
	"github.com/RequiDev/encore/internal/postgres"
)

// handleHealthz answers GET /healthz.
//
// It never touches the database. Liveness asks whether the process should be
// restarted, and restarting an API process because Postgres is down turns a
// database outage into a crash loop that makes the outage harder to fix.
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, r, http.StatusOK, HealthResponse{Status: "ok"})
}

// handleReadyz answers GET /readyz.
//
// Readiness is a stronger claim than liveness: the database has to answer *and*
// every embedded migration has to be applied, because a process talking to an
// out-of-date schema fails in confusing ways rather than obviously.
func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	lg := logging.FromContext(ctx)
	checks := map[string]string{}
	ready := true

	if err := postgres.Health(ctx, s.pool); err != nil {
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
	case s.ready.fresh(s.now()):
		checks["migrations"] = "ok"
	default:
		status, err := postgres.Status(ctx, s.cfg.Database.URL)
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
			s.ready.markFresh(s.now())
		}
	}

	if !ready {
		writeJSON(w, r, http.StatusServiceUnavailable, HealthResponse{Status: "not_ready", Checks: checks})
		return
	}
	writeJSON(w, r, http.StatusOK, HealthResponse{Status: "ok", Checks: checks})
}

// handleMetrics answers GET /metrics in Prometheus text format.
//
// It is behind HTTP basic auth whenever ENCORE_METRICS_USERNAME and
// ENCORE_METRICS_PASSWORD are set, and absent entirely when metrics are turned
// off, so a disabled endpoint looks like one that was never mounted.
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if s.metricsHandler == nil {
		writeError(w, r, ErrNotFoundf("Metrics are not enabled on this instance."))
		return
	}
	s.metricsHandler.ServeHTTP(w, r)
}
