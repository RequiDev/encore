package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/RequiDev/encore/internal/config"
)

func TestIsStreamingRoute(t *testing.T) {
	cases := []struct {
		method string
		path   string
		want   bool
	}{
		{http.MethodPost, "/api/imports", true},
		{http.MethodGet, "/api/me/export", true},
		{http.MethodGet, "/api/imports", false},
		{http.MethodDelete, "/api/imports", false},
		{http.MethodPost, "/api/imports/2f1c/retry", false},
		{http.MethodPost, "/api/me/export", false},
		{http.MethodGet, "/api/me", false},
		{http.MethodGet, "/api/history", false},
		{http.MethodPost, "/api/importsX", false},
	}

	for _, c := range cases {
		r := httptest.NewRequest(c.method, c.path, nil)
		if got := isStreamingRoute(r); got != c.want {
			t.Errorf("isStreamingRoute(%s %s) = %v, want %v", c.method, c.path, got, c.want)
		}
	}
}

// deadlineRecorder is a ResponseWriter that reports what deadlines were asked
// for, which is what http.ResponseController reaches for through the wrapper.
type deadlineRecorder struct {
	http.ResponseWriter
	read  *time.Time
	write *time.Time
}

func (d *deadlineRecorder) SetReadDeadline(t time.Time) error  { d.read = &t; return nil }
func (d *deadlineRecorder) SetWriteDeadline(t time.Time) error { d.write = &t; return nil }

func TestStreamingDeadlinesAreClearedOnlyForStreams(t *testing.T) {
	lg := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := withStreamingDeadlines(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), lg)

	for _, c := range []struct {
		name    string
		request *http.Request
		cleared bool
	}{
		{"upload", httptest.NewRequest(http.MethodPost, "/api/imports", nil), true},
		{"export", httptest.NewRequest(http.MethodGet, "/api/me/export", nil), true},
		{"ordinary", httptest.NewRequest(http.MethodGet, "/api/stats/summary", nil), false},
	} {
		t.Run(c.name, func(t *testing.T) {
			rec := &deadlineRecorder{ResponseWriter: httptest.NewRecorder()}
			h.ServeHTTP(rec, c.request)

			if !c.cleared {
				if rec.read != nil || rec.write != nil {
					t.Fatal("deadlines were touched on a route that is not a stream")
				}
				return
			}
			if rec.read == nil || !rec.read.IsZero() {
				t.Fatalf("read deadline is %v, want the zero time", rec.read)
			}
			if rec.write == nil || !rec.write.IsZero() {
				t.Fatalf("write deadline is %v, want the zero time", rec.write)
			}
		})
	}
}

func TestConfigAttrsAreSortedPairsAndCarryNoSecrets(t *testing.T) {
	cfg, err := config.LoadFrom(map[string]string{
		"ENCORE_PUBLIC_URL":            "https://encore.example.com",
		"ENCORE_WEB_URL":               "https://encore.example.com",
		"ENCORE_DATABASE_URL":          "postgres://encore:hunter2@db:5432/encore",
		"ENCORE_ENCRYPTION_KEY":        "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8=",
		"ENCORE_SPOTIFY_CLIENT_ID":     "client-id-value",
		"ENCORE_SPOTIFY_CLIENT_SECRET": "client-secret-value",
	})
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}

	attrs := configAttrs(cfg)
	if len(attrs) == 0 || len(attrs)%2 != 0 {
		t.Fatalf("configAttrs returned %d values, want an even, non-zero number", len(attrs))
	}

	previous := ""
	for i := 0; i < len(attrs); i += 2 {
		key, ok := attrs[i].(string)
		if !ok {
			t.Fatalf("attribute %d is not a key: %#v", i, attrs[i])
		}
		if key <= previous {
			t.Fatalf("keys are not sorted: %q came after %q", key, previous)
		}
		previous = key

		if text, ok := attrs[i+1].(string); ok {
			for _, secret := range []string{"hunter2", "client-secret-value"} {
				if text == secret {
					t.Fatalf("the startup log line carries a secret under %q", key)
				}
			}
		}
	}
}

// The deadline middleware must not swallow a request when the transport cannot
// carry a deadline at all, which is the case for HTTP/2 and for a bare recorder.
func TestStreamingDeadlinesToleratesAnUnsupportedTransport(t *testing.T) {
	lg := slog.New(slog.NewTextHandler(io.Discard, nil))
	called := false
	h := withStreamingDeadlines(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}), lg)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/imports", nil).WithContext(context.Background()))
	if !called {
		t.Fatal("the handler was not reached")
	}
}
