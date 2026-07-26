package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/requi/encore/internal/config"
	"github.com/requi/encore/internal/logging"
)

// serve runs the HTTP server until ctx is cancelled, then shuts it down inside
// the configured grace period.
//
// stop releases the signal handler. It is called as soon as the first signal has
// been observed so that an impatient second Ctrl-C kills the process outright
// rather than being swallowed by the shutdown the first one started.
func serve(ctx context.Context, stop func(), cfg *config.Config, h http.Handler, lg *slog.Logger) error {
	srv := &http.Server{
		Handler: withStreamingDeadlines(h, lg),
		// ReadHeaderTimeout is the one deadline that applies to every request
		// without exception: a client that has not finished sending its headers
		// is not a slow client, it is a slow-loris.
		ReadHeaderTimeout: cfg.HTTP.ReadTimeout,
		ReadTimeout:       cfg.HTTP.ReadTimeout,
		WriteTimeout:      cfg.HTTP.WriteTimeout,
		IdleTimeout:       cfg.HTTP.IdleTimeout,
		// net/http's own errors (a bad TLS handshake, a malformed request line)
		// otherwise go to the standard logger and escape the structured stream.
		ErrorLog: slog.NewLogLogger(lg.Handler(), slog.LevelWarn),
	}

	// Binding before announcing anything makes "address already in use" a
	// startup error with an exit code rather than a line in a log nobody reads.
	ln, err := net.Listen("tcp", cfg.HTTP.Addr)
	if err != nil {
		return fmt.Errorf("listen on ENCORE_HTTP_ADDR %s: %w", cfg.HTTP.Addr, err)
	}

	errc := make(chan error, 1)
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
	}()
	lg.Info("http server listening", "addr", ln.Addr().String())

	select {
	case err := <-errc:
		return fmt.Errorf("serve %s: %w", cfg.HTTP.Addr, err)
	case <-ctx.Done():
	}
	stop()

	lg.Info("shutting down", "timeout", cfg.HTTP.ShutdownTimeout.String())
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.HTTP.ShutdownTimeout)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		// Shutdown fails only by running out of the operator's own grace period.
		// Say what was lost rather than exiting silently: an import upload cut
		// off here is a request the user will have to make again.
		return fmt.Errorf("requests were still in flight after %s: %w", cfg.HTTP.ShutdownTimeout, err)
	}
	lg.Info("http server stopped")
	return nil
}

// isStreamingRoute reports whether a request is a stream rather than a message.
//
// Exactly two endpoints are: the multipart import upload, which is up to
// ENCORE_IMPORT_MAX_UPLOAD_BYTES and legitimately takes minutes on a domestic
// connection, and the history export, which writes a million rows without ever
// buffering them. Both are documented in docs/api.md; if either path changes
// there, it changes here.
func isStreamingRoute(r *http.Request) bool {
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/api/imports":
		return true
	case r.Method == http.MethodGet && r.URL.Path == "/api/me/export":
		return true
	default:
		return false
	}
}

// withStreamingDeadlines lifts the server's whole-request deadlines off the two
// routes that stream.
//
// ENCORE_HTTP_READ_TIMEOUT and ENCORE_HTTP_WRITE_TIMEOUT are exactly right for a
// JSON API and exactly wrong for those two: a four-gigabyte upload would be
// truncated after thirty seconds and an export of a large history after sixty,
// in both cases mid-transfer and with no useful error. Their cost is bounded by
// size rather than by time — the upload by ENCORE_IMPORT_MAX_UPLOAD_BYTES, the
// export by the caller's own history — so clearing the deadline risks a held
// connection and never unbounded memory.
//
// The deadlines are cleared on the connection for this request only: net/http
// re-arms them from the server's own settings before the next request on a
// keep-alive connection.
func withStreamingDeadlines(h http.Handler, lg *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isStreamingRoute(r) {
			rc := http.NewResponseController(w)
			// A transport that cannot carry a deadline (HTTP/2, a test recorder)
			// is not a failure: the request simply keeps the server's own, which
			// is the conservative outcome.
			if err := rc.SetReadDeadline(time.Time{}); err != nil {
				lg.Debug("could not clear the read deadline for a streaming route", logging.Err(err))
			}
			if err := rc.SetWriteDeadline(time.Time{}); err != nil {
				lg.Debug("could not clear the write deadline for a streaming route", logging.Err(err))
			}
		}
		h.ServeHTTP(w, r)
	})
}
