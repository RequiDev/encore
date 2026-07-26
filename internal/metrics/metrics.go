// Package metrics publishes Encore's Prometheus instrumentation.
//
// Every collector lives in a private registry owned by a Registry value rather
// than in prometheus.DefaultRegisterer. That keeps the exposition to metrics
// Encore actually defines, and it makes New safe to call more than once: a test,
// or a process that runs the API and a worker side by side, would otherwise trip
// the duplicate-registration panic a package-level registry produces.
//
// No caller outside this package touches a prometheus type. Each metric is
// reached through a method on Registry, so the shape of the instrumentation can
// change here without editing a single call site.
package metrics

import (
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"

	"github.com/requi/encore/internal/domain"
	"github.com/requi/encore/internal/postgres"
)

// Outcome is the bucket a processed import record falls into. The four values
// partition every record the importer reads, mirroring domain.Counters, so
// summing the series recovers the number of records processed.
type Outcome string

const (
	// OutcomeImported is a new row durably committed to listens.
	OutcomeImported Outcome = "imported"
	// OutcomeDuplicate is a valid record suppressed by the dedupe rules.
	OutcomeDuplicate Outcome = "duplicate"
	// OutcomeSkipped is a well-formed record intentionally not stored.
	OutcomeSkipped Outcome = "skipped"
	// OutcomeRejected is a malformed record recorded with diagnostics.
	OutcomeRejected Outcome = "rejected"
)

// Result labels how a unit of background work ended.
type Result string

const (
	// ResultSuccess is work that completed and did what it set out to do.
	ResultSuccess Result = "success"
	// ResultFailure is work that ended in an error.
	ResultFailure Result = "failure"
	// ResultSkipped is a run that found nothing to do. It is kept apart from
	// ResultSuccess because an instance that is idle and an instance that is
	// working look identical otherwise.
	ResultSkipped Result = "skipped"
)

// Kind identifies which entity an enrichment metric refers to.
type Kind string

const (
	// KindTrack counts catalogue tracks awaiting or receiving metadata.
	KindTrack Kind = "track"
	// KindArtist counts catalogue artists.
	KindArtist Kind = "artist"
	// KindAlbum counts catalogue albums.
	KindAlbum Kind = "album"
	// KindAlias counts names-only (artist, title) pairs awaiting search resolution.
	KindAlias Kind = "alias"
)

// Label sets that are finite and known up front. Every combination is
// pre-created at construction so that a dashboard or an alert rule sees a zero
// rather than a missing series before the first event of that kind occurs.
var (
	outcomes = []Outcome{OutcomeImported, OutcomeDuplicate, OutcomeSkipped, OutcomeRejected}
	results  = []Result{ResultSuccess, ResultFailure, ResultSkipped}
	kinds    = []Kind{KindTrack, KindArtist, KindAlbum, KindAlias}
	formats  = []domain.ImportFormat{domain.FormatExtended, domain.FormatAccountData, domain.FormatUnknown}
	statuses = []domain.ImportStatus{
		domain.ImportQueued, domain.ImportRunning, domain.ImportPaused,
		domain.ImportCompleted, domain.ImportFailed, domain.ImportCancelled,
	}
	poolStates = []string{"total", "idle", "acquired", "max"}
)

// Bucket layouts. They are explicit rather than prometheus.DefBuckets because
// the three histograms measure work with very different natural durations, and a
// bucket set that does not straddle the interesting range makes a quantile
// meaningless.
var (
	// httpBuckets favour the sub-second range an API request belongs in, with
	// enough headroom above it to see a statistics query that has gone wrong.
	httpBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}
	// importBatchBuckets span one flush of a batch: milliseconds when the pool is
	// healthy, tens of seconds when it is starved and the reader is being pushed back.
	importBatchBuckets = []float64{0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60}
	// spotifyBuckets reach a full minute because a throttled call that honours a
	// long Retry-After is a normal, and interesting, outcome.
	spotifyBuckets = []float64{0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 20, 60}
)

// Registry owns Encore's collectors and the private prometheus registry they
// are published through.
type Registry struct {
	reg *prometheus.Registry

	httpRequests *prometheus.CounterVec
	httpDuration *prometheus.HistogramVec
	httpInFlight prometheus.Gauge

	importRecords  *prometheus.CounterVec
	importBatches  *prometheus.CounterVec
	importBatchDur prometheus.Histogram
	importJobs     *prometheus.GaugeVec
	importRPS      prometheus.Gauge
	importQueue    prometheus.Gauge
	importBytes    prometheus.Counter

	spotifyRequests    *prometheus.CounterVec
	spotifyDuration    prometheus.Histogram
	spotifyRateLimited prometheus.Counter
	spotifyRetryAfter  prometheus.Gauge

	enrichPending  *prometheus.GaugeVec
	enrichResolved *prometheus.CounterVec
	enrichFailed   *prometheus.CounterVec

	syncRuns        *prometheus.CounterVec
	syncListens     prometheus.Counter
	syncLastSuccess prometheus.Gauge

	dbPoolConns *prometheus.GaugeVec
}

// New builds a Registry with every Encore collector plus the runtime and process
// collectors registered. It may be called any number of times; each call yields
// an independent registry, so nothing is ever registered twice.
func New() *Registry {
	r := &Registry{reg: prometheus.NewRegistry()}

	r.httpRequests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "encore_http_requests_total",
		Help: "HTTP requests served, by method, route template and response status.",
	}, []string{"method", "route", "status"})

	r.httpDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "encore_http_request_duration_seconds",
		Help:    "Time spent serving an HTTP request, by method and route template.",
		Buckets: httpBuckets,
	}, []string{"method", "route"})

	r.httpInFlight = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "encore_http_in_flight_requests",
		Help: "HTTP requests currently being served.",
	})

	r.importRecords = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "encore_import_records_processed_total",
		Help: "Import records accounted for, by export format and outcome.",
	}, []string{"format", "outcome"})

	r.importBatches = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "encore_import_batches_total",
		Help: "Import batch flushes attempted, by result.",
	}, []string{"result"})

	r.importBatchDur = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "encore_import_batch_duration_seconds",
		Help:    "Time spent flushing one batch of import records to the database.",
		Buckets: importBatchBuckets,
	})

	r.importJobs = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "encore_import_jobs",
		Help: "Import jobs currently in each lifecycle state.",
	}, []string{"status"})

	r.importRPS = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "encore_import_records_per_second",
		Help: "Records per second the running import is currently sustaining.",
	})

	r.importQueue = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "encore_import_queue_depth",
		Help: "Import jobs waiting to be claimed by a worker.",
	})

	r.importBytes = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "encore_import_bytes_read_total",
		Help: "Bytes read from uploaded export files.",
	})

	r.spotifyRequests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "encore_spotify_requests_total",
		Help: "Requests made to the Spotify API, by endpoint and response status.",
	}, []string{"endpoint", "status"})

	r.spotifyDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "encore_spotify_request_duration_seconds",
		Help:    "Round-trip time of a Spotify API request, including retries of that request.",
		Buckets: spotifyBuckets,
	})

	r.spotifyRateLimited = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "encore_spotify_rate_limited_total",
		Help: "Spotify responses that asked Encore to slow down.",
	})

	r.spotifyRetryAfter = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "encore_spotify_retry_after_seconds",
		Help: "Seconds requested by the most recent Spotify Retry-After header.",
	})

	r.enrichPending = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "encore_enrich_pending",
		Help: "Catalogue entities awaiting enrichment, by kind.",
	}, []string{"kind"})

	r.enrichResolved = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "encore_enrich_resolved_total",
		Help: "Catalogue entities successfully enriched, by kind.",
	}, []string{"kind"})

	r.enrichFailed = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "encore_enrich_failed_total",
		Help: "Catalogue entities that could not be enriched, by kind.",
	}, []string{"kind"})

	r.syncRuns = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "encore_sync_runs_total",
		Help: "Recently-played sync runs, by result.",
	}, []string{"result"})

	r.syncListens = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "encore_sync_listens_total",
		Help: "Listens ingested by the recently-played sync.",
	})

	r.syncLastSuccess = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "encore_sync_last_success_timestamp",
		Help: "Unix time of the last successful sync run, zero if there has not been one.",
	})

	r.dbPoolConns = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "encore_db_pool_conns",
		Help: "Database connections in the pool, by state.",
	}, []string{"state"})

	r.reg.MustRegister(
		// The runtime and process collectors are what turn a report of "the
		// importer is slow" into an answer, so they are not optional.
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),

		r.httpRequests, r.httpDuration, r.httpInFlight,
		r.importRecords, r.importBatches, r.importBatchDur, r.importJobs,
		r.importRPS, r.importQueue, r.importBytes,
		r.spotifyRequests, r.spotifyDuration, r.spotifyRateLimited, r.spotifyRetryAfter,
		r.enrichPending, r.enrichResolved, r.enrichFailed,
		r.syncRuns, r.syncListens, r.syncLastSuccess,
		r.dbPoolConns,
	)

	r.initSeries()
	return r
}

// initSeries materialises every label combination that is known in advance.
func (r *Registry) initSeries() {
	for _, f := range formats {
		for _, o := range outcomes {
			r.importRecords.WithLabelValues(string(f), string(o))
		}
	}
	for _, res := range results {
		r.importBatches.WithLabelValues(string(res))
		r.syncRuns.WithLabelValues(string(res))
	}
	for _, s := range statuses {
		r.importJobs.WithLabelValues(string(s))
	}
	for _, k := range kinds {
		r.enrichPending.WithLabelValues(string(k))
		r.enrichResolved.WithLabelValues(string(k))
		r.enrichFailed.WithLabelValues(string(k))
	}
	for _, s := range poolStates {
		r.dbPoolConns.WithLabelValues(s)
	}
}

// --- HTTP ------------------------------------------------------------------

// ObserveHTTP records one finished HTTP request.
//
// route must be a route template such as "/api/stats/top-tracks", never a
// concrete path: a metric labelled with request-specific values grows a series
// per user and eventually costs more than the application it observes.
func (r *Registry) ObserveHTTP(method, route string, status int, d time.Duration) {
	m := label(method, "unknown")
	rt := label(route, "unknown")
	r.httpRequests.WithLabelValues(m, rt, statusLabel(status)).Inc()
	r.httpDuration.WithLabelValues(m, rt).Observe(d.Seconds())
}

// IncInFlight records that a request has started.
func (r *Registry) IncInFlight() { r.httpInFlight.Inc() }

// DecInFlight records that a request has finished. Every IncInFlight needs a
// matching DecInFlight, which is why InstrumentHandler pairs them with a defer.
func (r *Registry) DecInFlight() { r.httpInFlight.Dec() }

// --- import ----------------------------------------------------------------

// AddImportRecords records n records that reached the given outcome.
func (r *Registry) AddImportRecords(format domain.ImportFormat, outcome Outcome, n int64) {
	if n <= 0 {
		return
	}
	r.importRecords.WithLabelValues(formatLabel(format), string(outcome)).Add(float64(n))
}

// AddImportCounters records a whole domain.Counters delta in one call, which is
// the shape the importer already accumulates its per-batch tallies in.
func (r *Registry) AddImportCounters(format domain.ImportFormat, c domain.Counters) {
	r.AddImportRecords(format, OutcomeImported, c.Imported)
	r.AddImportRecords(format, OutcomeDuplicate, c.Duplicates)
	r.AddImportRecords(format, OutcomeSkipped, c.Skipped)
	r.AddImportRecords(format, OutcomeRejected, c.Rejected)
}

// ObserveImportBatch records one batch flush and how long it took.
func (r *Registry) ObserveImportBatch(result Result, d time.Duration) {
	r.importBatches.WithLabelValues(resultLabel(result)).Inc()
	r.importBatchDur.Observe(d.Seconds())
}

// SetImportJobs publishes the number of jobs in a single lifecycle state.
func (r *Registry) SetImportJobs(status domain.ImportStatus, n int) {
	r.importJobs.WithLabelValues(statusName(status)).Set(float64(n))
}

// SetImportJobCounts publishes the whole distribution at once.
//
// States missing from counts are set to zero rather than left alone, so a job
// leaving a state is visible immediately instead of leaving a stale reading
// behind until the next process restart.
func (r *Registry) SetImportJobCounts(counts map[domain.ImportStatus]int) {
	for _, s := range statuses {
		r.importJobs.WithLabelValues(string(s)).Set(float64(counts[s]))
	}
}

// SetImportRecordsPerSecond publishes the throughput of the running import.
func (r *Registry) SetImportRecordsPerSecond(v float64) {
	if v < 0 {
		v = 0
	}
	r.importRPS.Set(v)
}

// SetImportQueueDepth publishes how many jobs are waiting to be claimed.
func (r *Registry) SetImportQueueDepth(n int) { r.importQueue.Set(float64(n)) }

// AddImportBytesRead records progress through an export file in bytes, which is
// the only progress signal available before a file's record total is known.
func (r *Registry) AddImportBytesRead(n int64) {
	if n <= 0 {
		return
	}
	r.importBytes.Add(float64(n))
}

// --- Spotify ---------------------------------------------------------------

// ObserveSpotifyRequest records one Spotify API call.
//
// endpoint is a stable short name for the route ("recently-played", "tracks",
// "search"), not the request URL. A status of zero, or anything outside the
// range of HTTP codes, is recorded as "error", which is how a request that never
// produced a response is kept out of the success rate.
func (r *Registry) ObserveSpotifyRequest(endpoint string, status int, d time.Duration) {
	r.spotifyRequests.WithLabelValues(label(endpoint, "unknown"), statusLabel(status)).Inc()
	r.spotifyDuration.Observe(d.Seconds())
}

// IncSpotifyRateLimited records that Spotify asked Encore to slow down.
func (r *Registry) IncSpotifyRateLimited() { r.spotifyRateLimited.Inc() }

// SetSpotifyRetryAfter publishes the delay Spotify most recently demanded.
func (r *Registry) SetSpotifyRetryAfter(d time.Duration) {
	if d < 0 {
		d = 0
	}
	r.spotifyRetryAfter.Set(d.Seconds())
}

// --- enrichment ------------------------------------------------------------

// SetEnrichPending publishes the backlog for one kind of entity.
func (r *Registry) SetEnrichPending(kind Kind, n int64) {
	r.enrichPending.WithLabelValues(kindLabel(kind)).Set(float64(n))
}

// SetEnrichPendingCounts publishes the whole backlog, zeroing kinds that are not
// present so a drained queue reads as zero rather than as its last value.
func (r *Registry) SetEnrichPendingCounts(counts map[Kind]int64) {
	for _, k := range kinds {
		r.enrichPending.WithLabelValues(string(k)).Set(float64(counts[k]))
	}
}

// AddEnrichResolved records n entities that gained metadata.
func (r *Registry) AddEnrichResolved(kind Kind, n int64) {
	if n <= 0 {
		return
	}
	r.enrichResolved.WithLabelValues(kindLabel(kind)).Add(float64(n))
}

// AddEnrichFailed records n entities enrichment could not resolve.
func (r *Registry) AddEnrichFailed(kind Kind, n int64) {
	if n <= 0 {
		return
	}
	r.enrichFailed.WithLabelValues(kindLabel(kind)).Add(float64(n))
}

// --- sync ------------------------------------------------------------------

// ObserveSyncRun records one completed poll of the recently-played endpoint.
func (r *Registry) ObserveSyncRun(result Result) {
	r.syncRuns.WithLabelValues(resultLabel(result)).Inc()
}

// AddSyncListens records listens ingested by the sync loop.
func (r *Registry) AddSyncListens(n int64) {
	if n <= 0 {
		return
	}
	r.syncListens.Add(float64(n))
}

// SetSyncLastSuccess publishes when the sync last succeeded. Alerting on the age
// of this gauge is what catches a sync that has quietly stopped running, which a
// counter of failures cannot show.
func (r *Registry) SetSyncLastSuccess(t time.Time) {
	if t.IsZero() {
		r.syncLastSuccess.Set(0)
		return
	}
	r.syncLastSuccess.Set(float64(t.Unix()))
}

// --- database --------------------------------------------------------------

// SetPoolStats publishes a snapshot of the connection pool. Acquired against max
// is the importer's backpressure signal: when they meet, the file reader is
// waiting on the database rather than the other way round.
func (r *Registry) SetPoolStats(s postgres.Stats) {
	r.dbPoolConns.WithLabelValues("total").Set(float64(s.TotalConns))
	r.dbPoolConns.WithLabelValues("idle").Set(float64(s.IdleConns))
	r.dbPoolConns.WithLabelValues("acquired").Set(float64(s.AcquiredConns))
	r.dbPoolConns.WithLabelValues("max").Set(float64(s.MaxConns))
}

// --- label helpers ---------------------------------------------------------

// statusLabel renders a response status. Anything that is not a real HTTP code
// becomes "error" so that a failed round trip is never mistaken for a response.
func statusLabel(status int) string {
	if status < 100 || status > 599 {
		return "error"
	}
	return strconv.Itoa(status)
}

func label(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

// The remaining helpers keep an unexpected value out of the exposition rather
// than creating a new series for it, since these labels come from typed
// constants and an unknown value means a bug, not a new category.
func formatLabel(f domain.ImportFormat) string {
	if !f.Valid() {
		return string(domain.FormatUnknown)
	}
	return string(f)
}

func statusName(s domain.ImportStatus) string {
	if !s.Valid() {
		return "unknown"
	}
	return string(s)
}

func resultLabel(res Result) string {
	switch res {
	case ResultSuccess, ResultFailure, ResultSkipped:
		return string(res)
	default:
		return string(ResultFailure)
	}
}

func kindLabel(k Kind) string {
	switch k {
	case KindTrack, KindArtist, KindAlbum, KindAlias:
		return string(k)
	default:
		return "unknown"
	}
}
