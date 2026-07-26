package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/requi/encore/internal/config"
	"github.com/requi/encore/internal/metrics"
)

func testConfig(t *testing.T, extra map[string]string) *config.Config {
	t.Helper()

	env := map[string]string{
		"ENCORE_PUBLIC_URL":            "https://encore.example.com",
		"ENCORE_WEB_URL":               "https://encore.example.com",
		"ENCORE_DATABASE_URL":          "postgres://encore:secret@db:5432/encore",
		"ENCORE_ENCRYPTION_KEY":        "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8=",
		"ENCORE_SPOTIFY_CLIENT_ID":     "client-id",
		"ENCORE_SPOTIFY_CLIENT_SECRET": "client-secret",
	}
	for k, v := range extra {
		env[k] = v
	}

	cfg, err := config.LoadFrom(env)
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	return cfg
}

func quiet() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// Liveness must answer without a database: restarting a worker because Postgres
// is down would turn an outage into a crash loop. The nil pool is the assertion.
func TestHealthzNeverTouchesTheDatabase(t *testing.T) {
	h := healthHandler(testConfig(t, nil), nil, metrics.New(), quiet())

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json; charset=utf-8" {
		t.Fatalf("content type %q", got)
	}

	var body struct {
		Status string            `json:"status"`
		Checks map[string]string `json:"checks"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body.String())
	}
	if body.Status != "ok" {
		t.Fatalf("status %q, want ok", body.Status)
	}
	if len(body.Checks) != 0 {
		t.Fatalf("liveness reported checks it cannot have made: %v", body.Checks)
	}
}

func TestWorkerServesNothingButHealthAndMetrics(t *testing.T) {
	h := healthHandler(testConfig(t, nil), nil, metrics.New(), quiet())

	for _, c := range []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/api/me"},
		{http.MethodPost, "/api/imports"},
		{http.MethodGet, "/api/stats/summary"},
		{http.MethodGet, "/"},
		{http.MethodPost, "/healthz"},
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(c.method, c.path, nil))
		if rec.Code == http.StatusOK {
			t.Errorf("%s %s answered 200 on the worker's listener", c.method, c.path)
		}
	}
}

func TestMetricsAreAbsentWhenDisabled(t *testing.T) {
	cfg := testConfig(t, map[string]string{"ENCORE_METRICS_ENABLED": "false"})
	h := healthHandler(cfg, nil, metrics.New(), quiet())

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status %d, want 404 when metrics are disabled", rec.Code)
	}
}

func TestMetricsHonourBasicAuth(t *testing.T) {
	cfg := testConfig(t, map[string]string{
		"ENCORE_METRICS_USERNAME": "scraper",
		"ENCORE_METRICS_PASSWORD": "s3cret",
	})
	h := healthHandler(cfg, nil, metrics.New(), quiet())

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status %d, want 401 without credentials", rec.Code)
	}

	authed := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	authed.SetBasicAuth("scraper", "s3cret")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, authed)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200 with credentials", rec.Code)
	}
}

func TestReadyCacheOnlyRemembersSuccess(t *testing.T) {
	c := &readyCache{}
	now := time.Unix(1_700_000_000, 0)

	if c.fresh(now) {
		t.Fatal("a fresh cache claimed a result it never had")
	}
	c.markFresh(now)
	if !c.fresh(now.Add(readyCacheTTL - time.Second)) {
		t.Fatal("the cached result expired early")
	}
	if c.fresh(now.Add(readyCacheTTL)) {
		t.Fatal("the cached result outlived its ttl")
	}
}
