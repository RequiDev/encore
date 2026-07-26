package main

import (
	"sync"
	"time"

	"github.com/requi/encore/internal/domain"
	"github.com/requi/encore/internal/metrics"
)

// The three adapters below are the only place that knows both the background
// packages' telemetry interfaces and the Prometheus registry. Those packages
// take their metrics as an interface precisely so that they do not depend on
// Prometheus, which is what lets them be tested with the zero value; the
// translation belongs here, in the composition root.

// importJobs publishes how many jobs this process holds in each lifecycle state.
//
// A runner reports a change — +1 running when it claims a job, -1 when it lets
// go — because a change is all one runner knows. Prometheus publishes a level,
// so the deltas are accumulated here. Every runner in the process shares one of
// these: the gauge describes the process, not the last runner to speak.
type importJobs struct {
	reg *metrics.Registry
	mu  sync.Mutex
	n   map[domain.ImportStatus]int
}

func newImportJobs(reg *metrics.Registry) *importJobs {
	return &importJobs{reg: reg, n: make(map[domain.ImportStatus]int)}
}

// level applies a delta and returns the new count for the state.
func (g *importJobs) level(status domain.ImportStatus, delta int) int {
	g.mu.Lock()
	defer g.mu.Unlock()

	n := g.n[status] + delta
	if n < 0 {
		// A negative level would be a bug in the accounting rather than a real
		// reading, and publishing it would make the series useless.
		n = 0
	}
	g.n[status] = n
	return n
}

// importMetrics adapts one import runner's telemetry onto the registry.
//
// There is one per runner because the byte accounting is per file, and a runner
// only ever reads one file at a time.
type importMetrics struct {
	reg  *metrics.Registry
	jobs *importJobs

	mu sync.Mutex
	// offset is the last absolute file position this runner reported.
	offset int64
}

func newImportMetrics(reg *metrics.Registry, jobs *importJobs) *importMetrics {
	return &importMetrics{reg: reg, jobs: jobs}
}

func (m *importMetrics) ImportRecords(format, outcome string, n int) {
	m.reg.AddImportRecords(domain.ImportFormat(format), metrics.Outcome(outcome), int64(n))
}

// ImportBatch records one flush of a batch.
//
// The importer's "retry" signal is deliberately dropped. It reports an attempt
// that is about to be made again and carries no duration, so feeding it to the
// latency histogram would pull the distribution towards zero exactly when the
// database is struggling. Nothing is lost: the flush it belongs to still ends in
// an "ok" or a "failed" whose duration includes every retry, and the retry
// itself is logged at warn.
func (m *importMetrics) ImportBatch(result string, d time.Duration) {
	switch result {
	case "ok":
		m.reg.ObserveImportBatch(metrics.ResultSuccess, d)
	case "failed":
		m.reg.ObserveImportBatch(metrics.ResultFailure, d)
	}
}

// ImportBytesRead records progress through the file being read.
func (m *importMetrics) ImportBytesRead(offset int64) {
	m.reg.AddImportBytesRead(m.delta(offset))
}

// delta converts the importer's absolute file position into the increment a
// counter needs.
//
// The importer reports how far into the file the checkpoint has reached, which
// is the right thing for it to know and the wrong thing to add up: summing the
// readings would count the first megabyte once per batch. A position that has
// gone backwards means a different file, or a job that resumed from an earlier
// checkpoint, and is counted from zero rather than as unread bytes.
func (m *importMetrics) delta(offset int64) int64 {
	if offset < 0 {
		return 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	delta := offset - m.offset
	if delta < 0 {
		delta = offset
	}
	m.offset = offset
	return delta
}

func (m *importMetrics) ImportThroughput(recordsPerSecond float64) {
	m.reg.SetImportRecordsPerSecond(recordsPerSecond)
}

func (m *importMetrics) ImportJobStatus(status string, delta int) {
	s := domain.ImportStatus(status)
	if !s.Valid() {
		// Never invent a series for a state that does not exist.
		return
	}
	m.reg.SetImportJobs(s, m.jobs.level(s, delta))
}

// enrichMetrics adapts internal/enrich's telemetry onto the registry. Its kind
// labels — "track", "album", "artist", "alias" — are the registry's own.
type enrichMetrics struct{ reg *metrics.Registry }

func (m enrichMetrics) EnrichPending(kind string, n int64) {
	m.reg.SetEnrichPending(metrics.Kind(kind), n)
}

func (m enrichMetrics) EnrichResolved(kind string, n int64) {
	m.reg.AddEnrichResolved(metrics.Kind(kind), n)
}

func (m enrichMetrics) EnrichFailed(kind string, n int64) {
	m.reg.AddEnrichFailed(metrics.Kind(kind), n)
}

// EnrichRateLimited lands on the Spotify rate-limit counter rather than one of
// its own: the pause it reports is the shared client's, and counting it twice
// would suggest enrichment had a limit separate from everything else.
func (m enrichMetrics) EnrichRateLimited() { m.reg.IncSpotifyRateLimited() }

// syncMetrics adapts internal/sync's telemetry onto the registry.
type syncMetrics struct{ reg *metrics.Registry }

func (m syncMetrics) SyncRun(result string)       { m.reg.ObserveSyncRun(metrics.Result(result)) }
func (m syncMetrics) SyncListens(n int64)         { m.reg.AddSyncListens(n) }
func (m syncMetrics) SyncLastSuccess(t time.Time) { m.reg.SetSyncLastSuccess(t) }
