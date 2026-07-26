package main

import (
	"sync"
	"time"
)

// batchStats summarise the flushes the importer performed.
//
// A batch is one transaction: the listens, the reject diagnostics and the
// checkpoint that describes them. Their latency distribution is where a slow
// benchmark is almost always explained, because everything else in the pipeline
// is a bounded amount of parsing between two of them.
type batchStats struct {
	Committed   int64   `json:"committed"`
	Failed      int64   `json:"failed"`
	Retried     int64   `json:"retried"`
	MeanSeconds float64 `json:"mean_seconds"`
	MaxSeconds  float64 `json:"max_seconds"`
	// BytesRead is the furthest the reader got through the file, which is how
	// much of the export the checkpoints actually cover.
	BytesRead int64 `json:"bytes_read"`
}

// collector implements importer.Metrics and keeps the parts of the telemetry
// that belong in a benchmark report.
//
// The importer takes its metrics sink as an interface precisely so that it does
// not depend on Prometheus; this is that seam being used for its second purpose.
type collector struct {
	mu    sync.Mutex
	stats batchStats
	total time.Duration
}

func newCollector() *collector { return &collector{} }

func (c *collector) ImportBatch(result string, d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch result {
	case "ok":
		c.stats.Committed++
		c.total += d
		if s := d.Seconds(); s > c.stats.MaxSeconds {
			c.stats.MaxSeconds = s
		}
	case "retry":
		c.stats.Retried++
	case "failed":
		c.stats.Failed++
	}
}

func (c *collector) ImportBytesRead(n int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	// The importer reports the checkpoint's absolute offset, so the furthest one
	// seen is the number that means anything.
	if n > c.stats.BytesRead {
		c.stats.BytesRead = n
	}
}

// The remaining signals are already answered, more authoritatively, by the
// counters read back from the database at the end of the run.
func (c *collector) ImportRecords(string, string, int) {}
func (c *collector) ImportThroughput(float64)          {}
func (c *collector) ImportJobStatus(string, int)       {}

// Snapshot returns the accumulated statistics.
func (c *collector) Snapshot() batchStats {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.stats
	if s.Committed > 0 {
		s.MeanSeconds = c.total.Seconds() / float64(s.Committed)
	}
	return s
}
