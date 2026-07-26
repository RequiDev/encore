package main

import (
	"runtime"
	"sync"
	"time"
)

// sampleInterval is how often the memory sampler looks at the heap. Reading
// memory statistics stops the world briefly, so this is a compromise: often
// enough to catch the peak of a batch flush, rare enough that the measurement
// does not distort what it is measuring.
const sampleInterval = 50 * time.Millisecond

// memoryPeak is the high-water mark of one measured phase.
//
// HeapAlloc is the live Go heap and is what the documented 256 MiB import target
// is about. Sys is everything the runtime has obtained from the operating system
// and has not returned, which is the closest a Go process can honestly get to
// its own resident set size without asking the kernel — it is an upper bound on
// RSS attributable to the runtime, and it is the number to watch when deciding
// how much memory to give a container.
type memoryPeak struct {
	HeapAllocBytes uint64 `json:"peak_heap_alloc_bytes"`
	SysBytes       uint64 `json:"peak_sys_bytes"`
	// TotalAllocBytes is everything allocated during the phase, live or not. A
	// figure far above the peak means the phase is churning through short-lived
	// objects rather than holding on to them, which is exactly what a streaming
	// importer should look like.
	TotalAllocBytes uint64 `json:"total_alloc_bytes"`
	GCCycles        uint32 `json:"gc_cycles"`
	Samples         int64  `json:"samples"`
}

// memorySampler tracks the peak heap of the process in the background.
type memorySampler struct {
	mu   sync.Mutex
	peak memoryPeak
	// Baselines let a phase be measured on its own: the cumulative counters are
	// reported as deltas since the last Reset.
	baseTotalAlloc uint64
	baseGC         uint32

	stop chan struct{}
	done chan struct{}
}

// startMemorySampler begins sampling until Stop is called.
func startMemorySampler() *memorySampler {
	s := &memorySampler{
		stop: make(chan struct{}),
		done: make(chan struct{}),
	}
	s.Reset()
	go s.loop()
	return s
}

func (s *memorySampler) loop() {
	defer close(s.done)
	t := time.NewTicker(sampleInterval)
	defer t.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-t.C:
			s.sample()
		}
	}
}

// sample takes one reading and folds it into the high-water marks.
func (s *memorySampler) sample() {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)

	s.mu.Lock()
	defer s.mu.Unlock()
	if m.HeapAlloc > s.peak.HeapAllocBytes {
		s.peak.HeapAllocBytes = m.HeapAlloc
	}
	if m.Sys > s.peak.SysBytes {
		s.peak.SysBytes = m.Sys
	}
	s.peak.TotalAllocBytes = m.TotalAlloc - s.baseTotalAlloc
	s.peak.GCCycles = m.NumGC - s.baseGC
	s.peak.Samples++
}

// Reset discards the marks accumulated so far and starts a new phase from the
// current state of the heap. The benchmark calls it between generating the
// dataset and importing it, so the reported peak describes the import alone.
func (s *memorySampler) Reset() {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)

	s.mu.Lock()
	defer s.mu.Unlock()
	s.baseTotalAlloc = m.TotalAlloc
	s.baseGC = m.NumGC
	s.peak = memoryPeak{HeapAllocBytes: m.HeapAlloc, SysBytes: m.Sys, Samples: 1}
}

// Snapshot returns the marks for the current phase, including one final reading
// so that a phase shorter than the sampling interval is still measured.
func (s *memorySampler) Snapshot() memoryPeak {
	s.sample()
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.peak
}

// Stop ends sampling and waits for the sampler to finish.
func (s *memorySampler) Stop() {
	select {
	case <-s.stop:
		return
	default:
	}
	close(s.stop)
	<-s.done
}
