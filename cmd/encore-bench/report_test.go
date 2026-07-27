package main

import (
	"bytes"
	"encoding/json"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RequiDev/encore/internal/domain"
)

// passingReport is a run that went exactly as it should: the job completed, the
// database holds the rows the importer claims, and the heap stayed well inside
// the documented target.
func passingReport() *Report {
	return &Report{
		Version:  "test",
		Counters: domain.Counters{Imported: 900, Skipped: 100},
		Memory:   memoryPeak{HeapAllocBytes: 32 << 20, SysBytes: 64 << 20, Samples: 10},
		Job: jobResult{
			ID:               "11111111-1111-1111-1111-111111111111",
			Status:           string(domain.ImportCompleted),
			Verified:         true,
			ListensCommitted: 900,
		},
	}
}

func TestEvaluateAcceptsACleanRun(t *testing.T) {
	rep := passingReport()
	evaluate(rep, defaultMaxHeapMB)
	if !rep.Passed {
		t.Fatalf("a clean run failed: %v", rep.Failures)
	}
}

func TestEvaluateFailsAnUnfinishedJob(t *testing.T) {
	rep := passingReport()
	rep.Job.Status = string(domain.ImportFailed)
	rep.Job.ErrorCode = domain.ErrCodeRetriesExhausted
	rep.Job.ErrorMessage = "the database was unreachable"

	evaluate(rep, defaultMaxHeapMB)
	if rep.Passed {
		t.Fatal("a failed job passed the benchmark")
	}
	if !strings.Contains(rep.Failures[0], domain.ErrCodeRetriesExhausted) {
		t.Fatalf("the failure does not name the error code: %q", rep.Failures[0])
	}
}

func TestEvaluateFailsWhenTheRowsAreNotThere(t *testing.T) {
	// The failure this whole tool exists to catch: the job says it imported
	// nine hundred listens and the table holds eight hundred.
	rep := passingReport()
	rep.Job.ListensCommitted = 800

	evaluate(rep, defaultMaxHeapMB)
	if rep.Passed {
		t.Fatal("a run whose rows were never committed passed")
	}
	if !strings.Contains(rep.Failures[0], "800") {
		t.Fatalf("the failure does not report the shortfall: %q", rep.Failures[0])
	}
}

func TestEvaluateFailsAnUnverifiedJob(t *testing.T) {
	rep := passingReport()
	rep.Job.Verified = false
	rep.Job.VerificationProblems = []string{"history.json: processed 10 of 20 records"}

	evaluate(rep, defaultMaxHeapMB)
	if rep.Passed {
		t.Fatal("an unverified job passed")
	}
	if !strings.Contains(rep.Failures[0], "processed 10 of 20") {
		t.Fatalf("the failure does not carry the verification problem: %q", rep.Failures[0])
	}
}

func TestEvaluateFailsAnOversizedHeap(t *testing.T) {
	rep := passingReport()
	rep.Memory.HeapAllocBytes = 300 << 20

	evaluate(rep, defaultMaxHeapMB)
	if rep.Passed {
		t.Fatal("a run that used 300 MiB passed a 256 MiB limit")
	}
	if !strings.Contains(rep.Failures[0], "peak heap") {
		t.Fatalf("unexpected failure: %q", rep.Failures[0])
	}

	// Zero disables the check, for measuring a configuration deliberately run
	// outside the documented target.
	rep = passingReport()
	rep.Memory.HeapAllocBytes = 300 << 20
	evaluate(rep, 0)
	if !rep.Passed {
		t.Fatalf("--max-heap-mb 0 should disable the limit: %v", rep.Failures)
	}
}

func TestReportRendersAndRoundTrips(t *testing.T) {
	rep := passingReport()
	rep.Dataset = datasetStats{
		Format: "extended", Records: 1000, Bytes: 545_000,
		FirstPlay: fixedNow().AddDate(-10, 0, 0), LastPlay: fixedNow(),
	}
	rep.Settings = benchSettings{BatchSize: 1000, MaxHeapMB: defaultMaxHeapMB}
	rep.Timing = timings{ImportSeconds: 2, RecordsPerSecond: 500, RowsPerSecond: 450}
	evaluate(rep, defaultMaxHeapMB)

	var buf bytes.Buffer
	if err := rep.WriteTable(&buf); err != nil {
		t.Fatalf("WriteTable: %v", err)
	}
	for _, want := range []string{"Dataset", "Throughput", "Importer counters", "verdict", "PASS"} {
		if !strings.Contains(buf.String(), want) {
			t.Fatalf("the table does not mention %q:\n%s", want, buf.String())
		}
	}

	path := filepath.Join(t.TempDir(), "nested", "bench.json")
	if err := rep.WriteJSON(path); err != nil {
		t.Fatalf("WriteJSON: %v", err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the report: %v", err)
	}
	var back Report
	if err := json.Unmarshal(body, &back); err != nil {
		t.Fatalf("the report is not valid JSON: %v", err)
	}
	if back.Counters.Imported != rep.Counters.Imported || !back.Passed {
		t.Fatalf("the report did not round-trip: %+v", back)
	}
}

func TestCollectorSummarisesBatches(t *testing.T) {
	c := newCollector()
	c.ImportBatch("ok", 100*time.Millisecond)
	c.ImportBatch("ok", 300*time.Millisecond)
	c.ImportBatch("retry", 0)
	c.ImportBatch("failed", 0)
	// The importer reports the checkpoint's absolute offset, so the furthest one
	// is what counts, not the sum.
	c.ImportBytesRead(4096)
	c.ImportBytesRead(1024)

	got := c.Snapshot()
	if got.Committed != 2 || got.Retried != 1 || got.Failed != 1 {
		t.Fatalf("unexpected tallies: %+v", got)
	}
	if got.MeanSeconds != 0.2 {
		t.Fatalf("mean latency %v, want 0.2", got.MeanSeconds)
	}
	if got.MaxSeconds != 0.3 {
		t.Fatalf("slowest batch %v, want 0.3", got.MaxSeconds)
	}
	if got.BytesRead != 4096 {
		t.Fatalf("bytes read %d, want 4096", got.BytesRead)
	}
}

func TestPlayClockKeepsPlaysApartAndInsideTheWindow(t *testing.T) {
	const records = 20_000
	now := fixedNow()
	clock, err := newPlayClock(records, now, rand.New(rand.NewPCG(1, 2)))
	if err != nil {
		t.Fatalf("newPlayClock: %v", err)
	}

	minGap := time.Duration(minPlayGapSeconds) * time.Second
	previous := time.Time{}
	var first, last time.Time
	for i := range records {
		t0 := clock.next()
		if i == 0 {
			first = t0
		} else if gap := t0.Sub(previous); gap < minGap {
			t.Fatalf("plays %d and %d are only %s apart, minimum is %s", i-1, i, gap, minGap)
		}
		previous, last = t0, t0
	}

	if first.Before(domain.EarliestPlausibleListen) {
		t.Fatalf("the history starts at %s, before the earliest plausible listen", first)
	}
	if !last.Before(now) {
		t.Fatalf("the history ends at %s, which is not before %s", last, now)
	}
}

func TestPlayClockRefusesMoreRecordsThanTheWindowHolds(t *testing.T) {
	now := fixedNow()
	window := now.Add(-24 * time.Hour).Sub(domain.EarliestPlausibleListen.AddDate(1, 0, 0))
	feasible := int(window / (minPlayGapSeconds * time.Second))

	if _, err := newPlayClock(feasible, now, rand.New(rand.NewPCG(1, 2))); err != nil {
		t.Fatalf("the largest feasible history was refused: %v", err)
	}
	// One more than the window holds, and a count large enough that the duration
	// arithmetic would overflow if it were attempted, must both be refused rather
	// than silently producing plays that collide.
	for _, records := range []int{feasible + 1, 500_000_000} {
		if _, err := newPlayClock(records, now, rand.New(rand.NewPCG(1, 2))); err == nil {
			t.Fatalf("%d records were accepted; the window holds %d", records, feasible)
		}
	}
}

func TestHumanInt(t *testing.T) {
	for in, want := range map[int64]string{
		0: "0", 1: "1", 999: "999", 1000: "1,000",
		1_234_567: "1,234,567", -12_345: "-12,345",
	} {
		if got := humanInt(in); got != want {
			t.Errorf("humanInt(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestHumanBytes(t *testing.T) {
	for in, want := range map[int64]string{
		0: "0 B", 512: "512 B", 1024: "1.0 KiB",
		1536: "1.5 KiB", 1 << 20: "1.0 MiB", 3 << 30: "3.0 GiB",
	} {
		if got := humanBytes(in); got != want {
			t.Errorf("humanBytes(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestPerSecondToleratesAnInstantPhase(t *testing.T) {
	if got := perSecond(100, 0); got != 0 {
		t.Fatalf("perSecond with no elapsed time = %v, want 0", got)
	}
	if got := perSecond(100, 2*time.Second); got != 50 {
		t.Fatalf("perSecond = %v, want 50", got)
	}
}
