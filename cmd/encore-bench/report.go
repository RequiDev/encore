package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/RequiDev/encore/internal/domain"
)

// Report is the whole result of one benchmark run. It is what --report writes,
// and the field names are part of that file's contract: a later run's numbers
// are only worth anything if they can be compared with an earlier one's.
type Report struct {
	Version   string          `json:"version"`
	StartedAt time.Time       `json:"started_at"`
	Host      hostInfo        `json:"host"`
	Settings  benchSettings   `json:"settings"`
	Dataset   datasetStats    `json:"dataset"`
	Timing    timings         `json:"timing"`
	Memory    memoryPeak      `json:"memory"`
	Batches   batchStats      `json:"batches"`
	Counters  domain.Counters `json:"importer_counters"`
	Database  rowCounts       `json:"database"`
	Job       jobResult       `json:"job"`
	Passed    bool            `json:"passed"`
	Failures  []string        `json:"failures,omitempty"`
}

// hostInfo records what the numbers were measured on, without which they mean
// nothing to anyone reading them later.
type hostInfo struct {
	GOOS      string `json:"goos"`
	GOARCH    string `json:"goarch"`
	NumCPU    int    `json:"num_cpu"`
	GoVersion string `json:"go_version"`
}

func currentHost() hostInfo {
	return hostInfo{
		GOOS:      runtime.GOOS,
		GOARCH:    runtime.GOARCH,
		NumCPU:    runtime.NumCPU(),
		GoVersion: runtime.Version(),
	}
}

// benchSettings are the knobs that were in force.
type benchSettings struct {
	Format      string `json:"format"`
	Records     int    `json:"records"`
	BatchSize   int    `json:"batch_size"`
	MinMsPlayed int32  `json:"min_ms_played"`
	Seed        uint64 `json:"seed"`
	MaxHeapMB   int    `json:"max_heap_mb"`
	Generated   bool   `json:"generated"`
	Keep        bool   `json:"keep"`
}

// timings separate the phases so that a slow disk cannot be mistaken for a slow
// importer. Only the import phase feeds the headline throughput.
type timings struct {
	GenerateSeconds float64 `json:"generate_seconds"`
	SpoolSeconds    float64 `json:"spool_seconds"`
	ImportSeconds   float64 `json:"import_seconds"`
	// RecordsPerSecond counts every record the importer accounted for, including
	// the ones it skipped: they were still read, parsed and classified.
	RecordsPerSecond float64 `json:"records_per_second"`
	// RowsPerSecond counts only rows committed to listens.
	RowsPerSecond  float64 `json:"rows_per_second"`
	BytesPerSecond float64 `json:"bytes_per_second"`
}

// jobResult is what became of the import job, including the verdict of the same
// post-import verification a listener's own upload has to pass.
type jobResult struct {
	ID                   string   `json:"id"`
	UserID               string   `json:"user_id"`
	Status               string   `json:"status"`
	ErrorCode            string   `json:"error_code,omitempty"`
	ErrorMessage         string   `json:"error_message,omitempty"`
	Files                int      `json:"files"`
	ListensCommitted     int64    `json:"listens_committed"`
	Verified             bool     `json:"verified"`
	VerificationProblems []string `json:"verification_problems,omitempty"`
}

// evaluate applies the benchmark's pass criteria and records why it failed.
//
// These are the three ways a run can look successful and not be: the job did not
// actually finish, the rows the importer claims to have written are not in the
// database, or the import held more memory than the design says it may. Each is
// a hard failure with a non-zero exit, because a benchmark nobody can fail is
// not a benchmark.
func evaluate(rep *Report, maxHeapMB int) {
	var failures []string

	if rep.Job.Status != string(domain.ImportCompleted) {
		detail := rep.Job.Status
		if rep.Job.ErrorCode != "" {
			detail += " (" + rep.Job.ErrorCode + ": " + rep.Job.ErrorMessage + ")"
		}
		failures = append(failures, "the import job did not reach 'completed'; it is "+detail)
	}
	if !rep.Job.Verified {
		problem := "post-import verification did not pass"
		if len(rep.Job.VerificationProblems) > 0 {
			problem += ": " + rep.Job.VerificationProblems[0]
		}
		failures = append(failures, problem)
	}
	if rep.Job.ListensCommitted != rep.Counters.Imported {
		failures = append(failures, fmt.Sprintf(
			"the importer counted %s inserted listens but the database holds %s for this job",
			humanInt(rep.Counters.Imported), humanInt(rep.Job.ListensCommitted)))
	}
	if maxHeapMB > 0 {
		limit := uint64(maxHeapMB) << 20
		if rep.Memory.HeapAllocBytes > limit {
			failures = append(failures, fmt.Sprintf(
				"peak heap %s exceeded the %d MiB limit; import memory must stay O(batch size), not O(file size)",
				humanBytes(int64(rep.Memory.HeapAllocBytes)), maxHeapMB))
		}
	}

	rep.Failures = failures
	rep.Passed = len(failures) == 0
}

// WriteJSON writes the report to path, creating its directory.
func (r *Report) WriteJSON(path string) error {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return fmt.Errorf("create report directory: %w", err)
		}
	}
	body, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return fmt.Errorf("encode report: %w", err)
	}
	body = append(body, '\n')
	if err := os.WriteFile(path, body, 0o640); err != nil {
		return fmt.Errorf("write report: %w", err)
	}
	return nil
}

// WriteTable renders the report for a terminal.
func (r *Report) WriteTable(w io.Writer) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)

	section := func(name string) { fmt.Fprintf(tw, "\n%s\n", name) }
	row := func(label string, value any) { fmt.Fprintf(tw, "  %s\t%v\n", label, value) }

	fmt.Fprintf(tw, "Encore import benchmark (%s, %s/%s, %d CPU, %s)\n",
		r.Version, r.Host.GOOS, r.Host.GOARCH, r.Host.NumCPU, r.Host.GoVersion)

	section("Dataset")
	row("format", r.Dataset.Format)
	row("records", humanInt(r.Dataset.Records))
	row("bytes", fmt.Sprintf("%s (%s per record)", humanBytes(r.Dataset.Bytes),
		humanBytes(divide(r.Dataset.Bytes, r.Dataset.Records))))
	if r.Dataset.Path != "" {
		row("file", r.Dataset.Path)
	}
	if !r.Dataset.FirstPlay.IsZero() {
		row("covers", fmt.Sprintf("%s to %s",
			r.Dataset.FirstPlay.Format(time.DateOnly), r.Dataset.LastPlay.Format(time.DateOnly)))
		row("catalogue", fmt.Sprintf("%s tracks by %s artists",
			humanInt(r.Dataset.Tracks), humanInt(r.Dataset.Artists)))
		row("not music", fmt.Sprintf("%s podcast, %s local-file, %s below-minimum plays",
			humanInt(r.Dataset.Podcasts), humanInt(r.Dataset.LocalFiles), humanInt(r.Dataset.ShortPlays)))
	}
	row("batch size", r.Settings.BatchSize)
	row("seed", r.Settings.Seed)

	section("Throughput")
	if r.Timing.GenerateSeconds > 0 {
		row("generate", humanSeconds(r.Timing.GenerateSeconds))
	}
	row("spool (intake)", humanSeconds(r.Timing.SpoolSeconds))
	row("import", humanSeconds(r.Timing.ImportSeconds))
	row("records/s", humanInt(int64(r.Timing.RecordsPerSecond)))
	row("rows/s", humanInt(int64(r.Timing.RowsPerSecond)))
	row("bytes/s", humanBytes(int64(r.Timing.BytesPerSecond)))

	section(fmt.Sprintf("Memory during the import (%d samples, every %s)",
		r.Memory.Samples, sampleInterval))
	row("peak heap (HeapAlloc)", humanBytes(int64(r.Memory.HeapAllocBytes)))
	row("peak Sys (RSS bound)", humanBytes(int64(r.Memory.SysBytes)))
	row("allocated in total", humanBytes(int64(r.Memory.TotalAllocBytes)))
	row("GC cycles", humanInt(int64(r.Memory.GCCycles)))

	section("Batches (one transaction each)")
	row("committed", humanInt(r.Batches.Committed))
	row("retried", humanInt(r.Batches.Retried))
	row("failed", humanInt(r.Batches.Failed))
	row("mean latency", humanSeconds(r.Batches.MeanSeconds))
	row("slowest", humanSeconds(r.Batches.MaxSeconds))
	row("bytes checkpointed", humanBytes(r.Batches.BytesRead))

	section("Importer counters")
	row("imported", humanInt(r.Counters.Imported))
	row("duplicates", humanInt(r.Counters.Duplicates))
	row("skipped", humanInt(r.Counters.Skipped))
	row("rejected", humanInt(r.Counters.Rejected))
	row("processed", humanInt(r.Counters.Processed()))

	section("Rows read back from the database")
	row("listens for this job", humanInt(r.Job.ListensCommitted))
	row("listens for this user", humanInt(r.Database.Listens))
	row("resolved to a track", humanInt(r.Database.ListensWithTrack))
	row("distinct tracks", humanInt(r.Database.DistinctTracks))
	row("distinct name pairs", humanInt(r.Database.DistinctAliases))
	row("tracks in the catalogue", humanInt(r.Database.TracksTotal))
	if r.Database.FirstPlayedAt != nil && r.Database.LastPlayedAt != nil {
		row("played between", fmt.Sprintf("%s and %s",
			r.Database.FirstPlayedAt.UTC().Format(time.DateOnly),
			r.Database.LastPlayedAt.UTC().Format(time.DateOnly)))
	}

	section("Result")
	row("job", r.Job.ID)
	row("user", r.Job.UserID)
	row("status", r.Job.Status)
	if r.Job.ErrorCode != "" {
		row("error", r.Job.ErrorCode+": "+r.Job.ErrorMessage)
	}
	row("verified against the database", yesNo(r.Job.Verified))
	// The counts above were read before the run tidied up after itself, so say so
	// rather than leave someone querying an empty table wondering.
	row("rows kept afterwards", yesNo(r.Settings.Keep))
	row("peak heap limit", fmt.Sprintf("%d MiB", r.Settings.MaxHeapMB))
	row("verdict", verdict(r.Passed))
	for _, f := range r.Failures {
		row("failure", f)
	}

	fmt.Fprintln(tw)
	return tw.Flush()
}

func verdict(passed bool) string {
	if passed {
		return "PASS"
	}
	return "FAIL"
}

func yesNo(v bool) string {
	if v {
		return "yes"
	}
	return "no"
}

// --- formatting helpers ----------------------------------------------------

// perSecond is a rate that reports zero rather than an infinity for a phase too
// fast to measure.
func perSecond(n int64, d time.Duration) float64 {
	if d <= 0 {
		return 0
	}
	return float64(n) / d.Seconds()
}

// divide is integer division that tolerates a zero denominator.
func divide(a, b int64) int64 {
	if b == 0 {
		return 0
	}
	return a / b
}

// humanInt groups thousands so that six- and seven-figure counts can be told
// apart at a glance, which is the whole reason this report exists.
func humanInt(n int64) string {
	s := strconv.FormatInt(n, 10)
	sign := ""
	if strings.HasPrefix(s, "-") {
		sign, s = "-", s[1:]
	}
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	return sign + b.String()
}

// humanBytes renders a byte count in binary units.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit && exp < 4; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTP"[exp])
}

// humanDuration keeps a duration readable without pretending to nanosecond
// precision on a measurement that took minutes.
func humanDuration(d time.Duration) string {
	const day = 24 * time.Hour
	switch {
	case d >= 2*365*day:
		return fmt.Sprintf("%.1f years", d.Hours()/(365*24))
	case d >= 2*day:
		return fmt.Sprintf("%.0f days", d.Hours()/24)
	case d >= time.Hour:
		return d.Round(time.Second).String()
	case d >= time.Minute:
		return d.Round(100 * time.Millisecond).String()
	case d >= time.Second:
		return d.Round(time.Millisecond).String()
	default:
		return d.Round(time.Microsecond).String()
	}
}

func humanSeconds(seconds float64) string {
	return humanDuration(time.Duration(seconds * float64(time.Second)))
}
