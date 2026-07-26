package spotify

import (
	"context"
	"sync"
	"time"
)

// Clock is the small slice of time this package depends on. Injecting it is what
// makes the retry schedule and the rate limiter testable without real waiting.
type Clock interface {
	// Now returns the current instant.
	Now() time.Time
	// Sleep blocks for d, returning ctx.Err() when the context finishes first.
	Sleep(ctx context.Context, d time.Duration) error
}

// SystemClock is the real clock, used by every client that does not inject one.
type SystemClock struct{}

// Now returns the current wall-clock time.
func (SystemClock) Now() time.Time { return time.Now() }

// Sleep waits for d or for ctx, whichever comes first.
func (SystemClock) Sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Limiter is the token bucket every request made by a Client passes through.
//
// Two mechanisms live here. The bucket paces ordinary traffic at the configured
// sustained rate while allowing a burst, and Pause stops the whole client after
// a 429. Pausing centrally is the point: Spotify's quota belongs to the
// application rather than to a goroutine, so letting each worker back off on its
// own would simply spend the next window on another burst of rejections.
//
// A Limiter is safe for concurrent use and may be shared between clients.
type Limiter struct {
	clock Clock
	rate  float64
	burst float64

	mu     sync.Mutex
	tokens float64
	last   time.Time
	paused time.Time
}

// NewLimiter builds a limiter on the real clock.
func NewLimiter(ratePerSecond float64, burst int) *Limiter {
	return NewLimiterWithClock(ratePerSecond, burst, SystemClock{})
}

// NewLimiterWithClock builds a limiter on an injected clock.
//
// The bucket starts full, so a freshly started worker may spend its whole burst
// immediately; that is the allowance being granted, not a bug.
func NewLimiterWithClock(ratePerSecond float64, burst int, clock Clock) *Limiter {
	if clock == nil {
		clock = SystemClock{}
	}
	if ratePerSecond <= 0 {
		ratePerSecond = 1
	}
	if burst < 1 {
		burst = 1
	}
	return &Limiter{
		clock:  clock,
		rate:   ratePerSecond,
		burst:  float64(burst),
		tokens: float64(burst),
		last:   clock.Now(),
	}
}

// Wait blocks until one token is available and no pause is in force.
//
// It returns ctx.Err() as soon as the context is done, even in the middle of a
// long pause, and hands the reserved token back so a cancelled caller does not
// consume quota.
func (l *Limiter) Wait(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	delay := l.reserve()
	refunded := false
	for {
		if delay > 0 {
			if err := l.clock.Sleep(ctx, delay); err != nil {
				if !refunded {
					l.refund()
					refunded = true
				}
				return err
			}
		}
		// A pause may have been declared while this caller was queued behind the
		// bucket. Re-checking here is what makes a 429 hold back requests that were
		// already waiting, not merely the ones that arrive after it.
		remaining := l.pauseRemaining()
		if remaining <= 0 {
			if err := ctx.Err(); err != nil {
				if !refunded {
					l.refund()
				}
				return err
			}
			return nil
		}
		delay = remaining
	}
}

// Pause holds every caller back until t. A pause is never shortened, so the
// longest Retry-After observed by any goroutine is the one that applies.
func (l *Limiter) Pause(until time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if until.After(l.paused) {
		l.paused = until
	}
}

// PausedUntil reports the instant the current pause ends. The zero time means
// the limiter has never been paused.
func (l *Limiter) PausedUntil() time.Time {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.paused
}

// reserve takes one token and reports how long the caller must wait for it.
func (l *Limiter) reserve() time.Duration {
	now := l.clock.Now()

	l.mu.Lock()
	defer l.mu.Unlock()
	l.advance(now)

	// The balance is allowed to go negative so that concurrent callers queue
	// behind one another instead of all waking at the same instant and firing a
	// burst the bucket was meant to smooth out.
	l.tokens--
	var d time.Duration
	if l.tokens < 0 {
		d = time.Duration(-l.tokens / l.rate * float64(time.Second))
	}
	if p := l.paused.Sub(now); p > d {
		d = p
	}
	return d
}

// advance credits the tokens earned since the last operation.
func (l *Limiter) advance(now time.Time) {
	if l.last.IsZero() {
		l.last = now
		return
	}
	elapsed := now.Sub(l.last)
	if elapsed <= 0 {
		return
	}
	l.tokens += elapsed.Seconds() * l.rate
	if l.tokens > l.burst {
		l.tokens = l.burst
	}
	l.last = now
}

// refund returns an unused token after a cancelled wait.
func (l *Limiter) refund() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.tokens++
	if l.tokens > l.burst {
		l.tokens = l.burst
	}
}

// pauseRemaining is how much of the current pause is still to run.
func (l *Limiter) pauseRemaining() time.Duration {
	now := l.clock.Now()

	l.mu.Lock()
	defer l.mu.Unlock()
	if l.paused.IsZero() {
		return 0
	}
	return l.paused.Sub(now)
}
