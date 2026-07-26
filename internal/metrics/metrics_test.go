package metrics

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"

	"github.com/requi/encore/internal/domain"
	"github.com/requi/encore/internal/postgres"
)

func TestNewGathers(t *testing.T) {
	r := New()
	families, err := r.reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	if len(families) == 0 {
		t.Fatal("gather returned no metric families")
	}

	// The Go collector proves the runtime collectors reached the private
	// registry; the Encore families prove the finite label sets are materialised
	// before anything has been recorded.
	for _, want := range []string{
		"go_goroutines",
		"encore_http_in_flight_requests",
		"encore_import_records_processed_total",
		"encore_import_jobs",
		"encore_enrich_pending",
		"encore_db_pool_conns",
	} {
		if !hasFamily(families, want) {
			t.Errorf("metric family %q missing from exposition", want)
		}
	}

	// Method and route are unbounded, so those families deliberately stay absent
	// until a request actually produces a series.
	if hasFamily(families, "encore_http_requests_total") {
		t.Error("encore_http_requests_total was pre-created; its labels are unbounded")
	}
}

func TestNewIsSafeToCallTwice(t *testing.T) {
	// A package-level registry would panic here on duplicate registration.
	first, second := New(), New()
	for i, r := range []*Registry{first, second} {
		if _, err := r.reg.Gather(); err != nil {
			t.Fatalf("registry %d: gather: %v", i, err)
		}
	}
	first.AddSyncListens(3)
	if got := value(t, second, "encore_sync_listens_total", nil); got != 0 {
		t.Fatalf("registries share state: second saw %v", got)
	}
}

func TestExpositionNames(t *testing.T) {
	r := New()
	r.ObserveHTTP(http.MethodGet, "/api/listens", http.StatusOK, 12*time.Millisecond)
	r.ObserveImportBatch(ResultSuccess, 250*time.Millisecond)
	r.ObserveSpotifyRequest("recently-played", http.StatusOK, 90*time.Millisecond)
	r.IncSpotifyRateLimited()
	r.SetSpotifyRetryAfter(3 * time.Second)
	r.AddEnrichResolved(KindTrack, 2)
	r.AddEnrichFailed(KindAlbum, 1)
	r.SetEnrichPending(KindAlias, 7)
	r.ObserveSyncRun(ResultSuccess)
	r.AddSyncListens(5)
	r.SetSyncLastSuccess(time.Unix(1700000000, 0))
	r.SetImportRecordsPerSecond(1234)
	r.SetImportQueueDepth(2)
	r.AddImportBytesRead(4096)
	r.SetPoolStats(postgres.Stats{TotalConns: 4, IdleConns: 3, AcquiredConns: 1, MaxConns: 10})

	body := scrape(t, r, "", "")
	for _, want := range []string{
		"encore_http_requests_total",
		"encore_http_request_duration_seconds",
		"encore_http_in_flight_requests",
		"encore_import_records_processed_total",
		"encore_import_batches_total",
		"encore_import_batch_duration_seconds",
		"encore_import_jobs",
		"encore_import_records_per_second",
		"encore_import_queue_depth",
		"encore_import_bytes_read_total",
		"encore_spotify_requests_total",
		"encore_spotify_request_duration_seconds",
		"encore_spotify_rate_limited_total",
		"encore_spotify_retry_after_seconds",
		"encore_enrich_pending",
		"encore_enrich_resolved_total",
		"encore_enrich_failed_total",
		"encore_sync_runs_total",
		"encore_sync_listens_total",
		"encore_sync_last_success_timestamp",
		"encore_db_pool_conns",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("exposition is missing %q", want)
		}
	}
}

func TestObserveHTTPLabels(t *testing.T) {
	r := New()
	r.ObserveHTTP(http.MethodGet, "/api/listens", http.StatusOK, 5*time.Millisecond)
	r.ObserveHTTP(http.MethodGet, "/api/listens", http.StatusOK, 7*time.Millisecond)
	r.ObserveHTTP(http.MethodPost, "/api/imports", http.StatusCreated, time.Millisecond)

	got := value(t, r, "encore_http_requests_total", map[string]string{
		"method": "GET", "route": "/api/listens", "status": "200",
	})
	if got != 2 {
		t.Fatalf("GET /api/listens count = %v, want 2", got)
	}
	got = value(t, r, "encore_http_requests_total", map[string]string{
		"method": "POST", "route": "/api/imports", "status": "201",
	})
	if got != 1 {
		t.Fatalf("POST /api/imports count = %v, want 1", got)
	}
	if n := histogramCount(t, r, "encore_http_request_duration_seconds", map[string]string{
		"method": "GET", "route": "/api/listens",
	}); n != 2 {
		t.Fatalf("duration observations = %d, want 2", n)
	}
}

func TestStatusLabel(t *testing.T) {
	cases := map[int]string{
		200: "200",
		404: "404",
		599: "599",
		0:   "error",
		-1:  "error",
		99:  "error",
		600: "error",
	}
	for in, want := range cases {
		if got := statusLabel(in); got != want {
			t.Errorf("statusLabel(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestUnknownLabelValuesAreFolded(t *testing.T) {
	r := New()
	r.ObserveHTTP("", "", http.StatusOK, time.Millisecond)
	if got := value(t, r, "encore_http_requests_total", map[string]string{
		"method": "unknown", "route": "unknown", "status": "200",
	}); got != 1 {
		t.Fatalf("empty method and route were not folded into \"unknown\": %v", got)
	}

	// An out-of-band enum value must not create a series of its own.
	r.AddImportRecords(domain.ImportFormat("nonsense"), OutcomeImported, 1)
	if got := value(t, r, "encore_import_records_processed_total", map[string]string{
		"format": "unknown", "outcome": "imported",
	}); got != 1 {
		t.Fatalf("unknown format was not folded: %v", got)
	}
	r.AddEnrichResolved(Kind("nonsense"), 1)
	if got := value(t, r, "encore_enrich_resolved_total", map[string]string{"kind": "unknown"}); got != 1 {
		t.Fatalf("unknown kind was not folded: %v", got)
	}
	r.ObserveSyncRun(Result("nonsense"))
	if got := value(t, r, "encore_sync_runs_total", map[string]string{"result": "failure"}); got != 1 {
		t.Fatalf("unknown result was not folded onto failure: %v", got)
	}
}

func TestAddImportCounters(t *testing.T) {
	r := New()
	r.AddImportCounters(domain.FormatExtended, domain.Counters{
		Imported: 10, Duplicates: 4, Skipped: 2, Rejected: 1,
	})
	for outcome, want := range map[Outcome]float64{
		OutcomeImported: 10, OutcomeDuplicate: 4, OutcomeSkipped: 2, OutcomeRejected: 1,
	} {
		got := value(t, r, "encore_import_records_processed_total", map[string]string{
			"format": "extended", "outcome": string(outcome),
		})
		if got != want {
			t.Errorf("%s = %v, want %v", outcome, got, want)
		}
	}
	// Negative or zero deltas must be ignored rather than panic the counter.
	r.AddImportCounters(domain.FormatExtended, domain.Counters{Imported: -5})
	if got := value(t, r, "encore_import_records_processed_total", map[string]string{
		"format": "extended", "outcome": "imported",
	}); got != 10 {
		t.Errorf("negative delta changed the counter: %v", got)
	}
}

func TestSetImportJobCountsZeroesAbsentStates(t *testing.T) {
	r := New()
	r.SetImportJobCounts(map[domain.ImportStatus]int{domain.ImportRunning: 3, domain.ImportQueued: 5})
	if got := value(t, r, "encore_import_jobs", map[string]string{"status": "running"}); got != 3 {
		t.Fatalf("running = %v, want 3", got)
	}

	r.SetImportJobCounts(map[domain.ImportStatus]int{domain.ImportCompleted: 1})
	for status, want := range map[string]float64{
		"queued": 0, "running": 0, "paused": 0, "completed": 1, "failed": 0, "cancelled": 0,
	} {
		if got := value(t, r, "encore_import_jobs", map[string]string{"status": status}); got != want {
			t.Errorf("%s = %v, want %v", status, got, want)
		}
	}
}

func TestSetPoolStats(t *testing.T) {
	r := New()
	r.SetPoolStats(postgres.Stats{TotalConns: 7, IdleConns: 2, AcquiredConns: 5, MaxConns: 10})
	for state, want := range map[string]float64{"total": 7, "idle": 2, "acquired": 5, "max": 10} {
		if got := value(t, r, "encore_db_pool_conns", map[string]string{"state": state}); got != want {
			t.Errorf("%s = %v, want %v", state, got, want)
		}
	}
}

func TestSetSyncLastSuccess(t *testing.T) {
	r := New()
	if got := value(t, r, "encore_sync_last_success_timestamp", nil); got != 0 {
		t.Fatalf("initial value = %v, want 0", got)
	}
	r.SetSyncLastSuccess(time.Unix(1700000000, 0))
	if got := value(t, r, "encore_sync_last_success_timestamp", nil); got != 1700000000 {
		t.Fatalf("timestamp = %v, want 1700000000", got)
	}
	r.SetSyncLastSuccess(time.Time{})
	if got := value(t, r, "encore_sync_last_success_timestamp", nil); got != 0 {
		t.Fatalf("zero time = %v, want 0", got)
	}
}

func TestHandlerWithoutAuth(t *testing.T) {
	r := New()
	rec := httptest.NewRecorder()
	r.Handler("", "").ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "encore_import_jobs") {
		t.Fatal("exposition does not contain Encore metrics")
	}
}

func TestHandlerBasicAuth(t *testing.T) {
	r := New()
	h := r.Handler("scraper", "s3cret")

	cases := []struct {
		name     string
		user     string
		pass     string
		withAuth bool
		want     int
	}{
		{name: "correct credentials", user: "scraper", pass: "s3cret", withAuth: true, want: http.StatusOK},
		{name: "wrong password", user: "scraper", pass: "wrong", withAuth: true, want: http.StatusUnauthorized},
		{name: "wrong username", user: "nobody", pass: "s3cret", withAuth: true, want: http.StatusUnauthorized},
		{name: "empty credentials", user: "", pass: "", withAuth: true, want: http.StatusUnauthorized},
		{name: "password as prefix", user: "scraper", pass: "s3cre", withAuth: true, want: http.StatusUnauthorized},
		{name: "no header at all", withAuth: false, want: http.StatusUnauthorized},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
			if c.withAuth {
				req.SetBasicAuth(c.user, c.pass)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != c.want {
				t.Fatalf("status = %d, want %d", rec.Code, c.want)
			}
			if c.want == http.StatusUnauthorized {
				if got := rec.Header().Get("WWW-Authenticate"); !strings.HasPrefix(got, "Basic ") {
					t.Fatalf("WWW-Authenticate = %q, want a Basic challenge", got)
				}
				if strings.Contains(rec.Body.String(), "s3cret") {
					t.Fatal("the response echoed the configured password")
				}
			}
		})
	}
}

func TestInstrumentHandler(t *testing.T) {
	r := New()
	var inFlightDuringRequest float64
	h := r.InstrumentHandler("/api/stats/top-tracks", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		inFlightDuringRequest = value(t, r, "encore_http_in_flight_requests", nil)
		w.WriteHeader(http.StatusTeapot)
		_, _ = io.WriteString(w, "ok")
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/stats/top-tracks?range=year", nil))

	if rec.Code != http.StatusTeapot {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusTeapot)
	}
	if rec.Body.String() != "ok" {
		t.Fatalf("body = %q, want %q", rec.Body.String(), "ok")
	}
	if inFlightDuringRequest != 1 {
		t.Fatalf("in-flight during request = %v, want 1", inFlightDuringRequest)
	}
	if got := value(t, r, "encore_http_in_flight_requests", nil); got != 0 {
		t.Fatalf("in-flight after request = %v, want 0", got)
	}
	// The route label must come from the template, not from the query string.
	if got := value(t, r, "encore_http_requests_total", map[string]string{
		"method": "GET", "route": "/api/stats/top-tracks", "status": "418",
	}); got != 1 {
		t.Fatalf("request count = %v, want 1", got)
	}
	if n := histogramCount(t, r, "encore_http_request_duration_seconds", map[string]string{
		"method": "GET", "route": "/api/stats/top-tracks",
	}); n != 1 {
		t.Fatalf("duration observations = %d, want 1", n)
	}
}

func TestInstrumentHandlerImplicitStatus(t *testing.T) {
	r := New()
	h := r.InstrumentHandler("/healthz", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "alive")
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if got := value(t, r, "encore_http_requests_total", map[string]string{
		"method": "GET", "route": "/healthz", "status": "200",
	}); got != 1 {
		t.Fatalf("implicit 200 was not recorded: %v", got)
	}
}

func TestInstrumentHandlerRecordsPanickingRequest(t *testing.T) {
	r := New()
	h := r.InstrumentHandler("/boom", http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("handler exploded")
	}))

	func() {
		defer func() {
			if recover() == nil {
				t.Error("panic was swallowed; recovery belongs to the HTTP middleware, not to metrics")
			}
		}()
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/boom", nil))
	}()

	if got := value(t, r, "encore_http_in_flight_requests", nil); got != 0 {
		t.Fatalf("in-flight after a panic = %v, want 0", got)
	}
	if got := value(t, r, "encore_http_requests_total", map[string]string{
		"method": "GET", "route": "/boom", "status": "200",
	}); got != 1 {
		t.Fatalf("panicking request was not counted: %v", got)
	}
}

func TestTimer(t *testing.T) {
	now := time.Unix(0, 0)
	clock := func() time.Time { return now }

	var observed []time.Duration
	tm := newTimer(clock, func(d time.Duration) { observed = append(observed, d) })

	now = now.Add(250 * time.Millisecond)
	if got := tm.Elapsed(); got != 250*time.Millisecond {
		t.Fatalf("Elapsed = %v, want 250ms", got)
	}
	if got := tm.Stop(); got != 250*time.Millisecond {
		t.Fatalf("Stop = %v, want 250ms", got)
	}

	// A second Stop must neither move the reading nor record again, so that a
	// deferred Stop is safe next to an explicit one.
	now = now.Add(time.Second)
	if got := tm.Stop(); got != 250*time.Millisecond {
		t.Fatalf("second Stop = %v, want 250ms", got)
	}
	if got := tm.Elapsed(); got != 250*time.Millisecond {
		t.Fatalf("Elapsed after Stop = %v, want 250ms", got)
	}
	if len(observed) != 1 {
		t.Fatalf("observer called %d times, want 1", len(observed))
	}
}

func TestTimerWithoutObserver(t *testing.T) {
	tm := NewTimer(nil)
	if tm.Stop() < 0 {
		t.Fatal("Stop returned a negative duration")
	}
}

// --- helpers ---------------------------------------------------------------

func hasFamily(families []*dto.MetricFamily, name string) bool {
	for _, f := range families {
		if f.GetName() == name {
			return true
		}
	}
	return false
}

// value returns the counter or gauge sample of name carrying exactly labels.
func value(t *testing.T, r *Registry, name string, labels map[string]string) float64 {
	t.Helper()
	m := find(t, r, name, labels)
	switch {
	case m.GetCounter() != nil:
		return m.GetCounter().GetValue()
	case m.GetGauge() != nil:
		return m.GetGauge().GetValue()
	default:
		t.Fatalf("metric %q is neither a counter nor a gauge", name)
		return 0
	}
}

// histogramCount returns the number of observations recorded by a histogram.
func histogramCount(t *testing.T, r *Registry, name string, labels map[string]string) uint64 {
	t.Helper()
	m := find(t, r, name, labels)
	if m.GetHistogram() == nil {
		t.Fatalf("metric %q is not a histogram", name)
	}
	return m.GetHistogram().GetSampleCount()
}

func find(t *testing.T, r *Registry, name string, labels map[string]string) *dto.Metric {
	t.Helper()
	families, err := r.reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
		for _, m := range f.GetMetric() {
			if matches(m, labels) {
				return m
			}
		}
	}
	t.Fatalf("no sample of %q with labels %v", name, labels)
	return nil
}

func matches(m *dto.Metric, labels map[string]string) bool {
	if len(m.GetLabel()) != len(labels) {
		return false
	}
	for _, lp := range m.GetLabel() {
		want, ok := labels[lp.GetName()]
		if !ok || want != lp.GetValue() {
			return false
		}
	}
	return true
}

func scrape(t *testing.T, r *Registry, username, password string) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	if username != "" {
		req.SetBasicAuth(username, password)
	}
	rec := httptest.NewRecorder()
	r.Handler(username, password).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("scrape status = %d, want 200", rec.Code)
	}
	return rec.Body.String()
}
