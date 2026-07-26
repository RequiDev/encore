package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/requi/encore/internal/metrics"
)

// exposition scrapes a registry the way Prometheus would, which is the only way
// to read a private registry's values from outside internal/metrics.
func exposition(t *testing.T, reg *metrics.Registry) string {
	t.Helper()

	rec := httptest.NewRecorder()
	reg.Handler("", "").ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("scrape returned %d, want 200", rec.Code)
	}
	return rec.Body.String()
}

func requireSample(t *testing.T, body, sample string) {
	t.Helper()

	for line := range strings.SplitSeq(body, "\n") {
		if strings.TrimSpace(line) == sample {
			return
		}
	}
	t.Fatalf("the exposition does not contain %q", sample)
}

func TestImportBytesReadConvertsOffsetsToDeltas(t *testing.T) {
	m := newImportMetrics(metrics.New(), nil)

	cases := []struct {
		offset int64
		want   int64
		why    string
	}{
		{100, 100, "the first checkpoint counts the whole offset"},
		{250, 150, "a later checkpoint counts only what it added"},
		{250, 0, "a repeated offset adds nothing"},
		{40, 40, "a new file starts counting from its own offset"},
		{-1, 0, "a nonsensical offset is ignored"},
		{90, 50, "and the new file carries on from there"},
	}
	for _, c := range cases {
		if got := m.delta(c.offset); got != c.want {
			t.Errorf("delta(%d) = %d, want %d: %s", c.offset, got, c.want, c.why)
		}
	}
}

func TestImportBytesReadIsCounted(t *testing.T) {
	reg := metrics.New()
	m := newImportMetrics(reg, newImportJobs(reg))

	for _, offset := range []int64{100, 250, 250, 40} {
		m.ImportBytesRead(offset)
	}
	requireSample(t, exposition(t, reg), "encore_import_bytes_read_total 290")
}

// The job gauge is a level for the whole process, so two runners each holding a
// job must read as two rather than as whoever spoke last.
func TestImportJobGaugeCountsEveryRunner(t *testing.T) {
	reg := metrics.New()
	shared := newImportJobs(reg)
	first := newImportMetrics(reg, shared)
	second := newImportMetrics(reg, shared)

	first.ImportJobStatus("running", 1)
	second.ImportJobStatus("running", 1)
	requireSample(t, exposition(t, reg), `encore_import_jobs{status="running"} 2`)

	first.ImportJobStatus("running", -1)
	requireSample(t, exposition(t, reg), `encore_import_jobs{status="running"} 1`)

	second.ImportJobStatus("running", -1)
	requireSample(t, exposition(t, reg), `encore_import_jobs{status="running"} 0`)
}

func TestImportJobGaugeNeverGoesNegativeAndIgnoresUnknownStates(t *testing.T) {
	reg := metrics.New()
	m := newImportMetrics(reg, newImportJobs(reg))

	m.ImportJobStatus("running", -1)
	requireSample(t, exposition(t, reg), `encore_import_jobs{status="running"} 0`)

	m.ImportJobStatus("levitating", 1)
	if body := exposition(t, reg); strings.Contains(body, "levitating") {
		t.Fatal("an unknown lifecycle state created a series")
	}
}

func TestImportBatchKeepsRetriesOutOfTheLatencyHistogram(t *testing.T) {
	reg := metrics.New()
	m := newImportMetrics(reg, newImportJobs(reg))

	m.ImportBatch("retry", 0)
	m.ImportBatch("retry", 0)
	m.ImportBatch("ok", 250*time.Millisecond)
	m.ImportBatch("failed", time.Second)

	body := exposition(t, reg)
	requireSample(t, body, `encore_import_batches_total{result="success"} 1`)
	requireSample(t, body, `encore_import_batches_total{result="failure"} 1`)
	requireSample(t, body, "encore_import_batch_duration_seconds_count 2")
}

func TestImportRecordsUseTheImporterOwnLabels(t *testing.T) {
	reg := metrics.New()
	m := newImportMetrics(reg, newImportJobs(reg))

	m.ImportRecords("extended", "imported", 3)
	m.ImportRecords("extended", "duplicate", 1)
	m.ImportRecords("account_data", "rejected", 2)

	body := exposition(t, reg)
	requireSample(t, body, `encore_import_records_processed_total{format="extended",outcome="imported"} 3`)
	requireSample(t, body, `encore_import_records_processed_total{format="extended",outcome="duplicate"} 1`)
	requireSample(t, body, `encore_import_records_processed_total{format="account_data",outcome="rejected"} 2`)
}

func TestEnrichAndSyncAdaptersPublishTheirSeries(t *testing.T) {
	reg := metrics.New()
	e := enrichMetrics{reg: reg}
	s := syncMetrics{reg: reg}

	e.EnrichPending("track", 12)
	e.EnrichResolved("album", 4)
	e.EnrichFailed("artist", 1)
	e.EnrichRateLimited()

	s.SyncRun("success")
	s.SyncRun("skipped")
	s.SyncListens(7)
	s.SyncLastSuccess(time.Unix(1_700_000_000, 0))

	body := exposition(t, reg)
	requireSample(t, body, `encore_enrich_pending{kind="track"} 12`)
	requireSample(t, body, `encore_enrich_resolved_total{kind="album"} 4`)
	requireSample(t, body, `encore_enrich_failed_total{kind="artist"} 1`)
	requireSample(t, body, "encore_spotify_rate_limited_total 1")
	requireSample(t, body, `encore_sync_runs_total{result="success"} 1`)
	requireSample(t, body, `encore_sync_runs_total{result="skipped"} 1`)
	requireSample(t, body, "encore_sync_listens_total 7")
	requireSample(t, body, "encore_sync_last_success_timestamp 1.7e+09")
}
