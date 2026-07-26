package metrics

import "time"

// Timer measures the wall-clock duration of one operation.
//
// It exists so that a caller can time work without importing prometheus: start a
// timer, do the work, hand Stop's result to the matching Observe method. The
// optional observer is what lets middleware bind a timer to a histogram at the
// point the measurement begins instead of at the point it ends.
type Timer struct {
	// now is injectable so the timer's own behaviour can be tested without
	// sleeping.
	now     func() time.Time
	start   time.Time
	observe func(time.Duration)
	elapsed time.Duration
	stopped bool
}

// NewTimer starts a timer. When observe is non-nil it receives the elapsed time
// the first time Stop is called; a nil observer makes the timer a plain
// stopwatch.
func NewTimer(observe func(time.Duration)) *Timer {
	return newTimer(time.Now, observe)
}

func newTimer(now func() time.Time, observe func(time.Duration)) *Timer {
	return &Timer{now: now, start: now(), observe: observe}
}

// Elapsed reports how long the timer has been running. It records nothing, and
// keeps returning the final duration once the timer has been stopped.
func (t *Timer) Elapsed() time.Duration {
	if t.stopped {
		return t.elapsed
	}
	return t.now().Sub(t.start)
}

// Stop returns the elapsed time, reporting it to the observer exactly once.
// Stopping a stopped timer records nothing further and returns the same
// duration, so a deferred Stop is safe alongside an explicit one on a path that
// already ended the measurement.
func (t *Timer) Stop() time.Duration {
	if t.stopped {
		return t.elapsed
	}
	t.elapsed = t.now().Sub(t.start)
	t.stopped = true
	if t.observe != nil {
		t.observe(t.elapsed)
	}
	return t.elapsed
}
