// Package retry implements bounded exponential backoff with full jitter.
//
// Every retry loop in Encore goes through here so that the policy is uniform and
// testable: the delay sequence is a pure function of the policy and the attempt
// number, and jitter comes from an injectable source.
package retry

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"
)

// Policy describes a bounded backoff schedule.
type Policy struct {
	// MaxAttempts is the total number of tries, including the first. Zero or one
	// means "no retries".
	MaxAttempts int
	// Base is the delay before the second attempt.
	Base time.Duration
	// Max caps a single delay.
	Max time.Duration
	// Multiplier grows the delay between attempts.
	Multiplier float64
	// Jitter in [0,1] is the fraction of the computed delay that is randomised.
	// 1.0 gives "full jitter": a uniform draw from [0, delay]. Full jitter is the
	// default because it is what actually prevents synchronised retry storms when
	// many workers fail at the same instant.
	Jitter float64
}

// Default is the policy for database work: fast first retries, capped at 30s.
func Default() Policy {
	return Policy{MaxAttempts: 6, Base: 100 * time.Millisecond, Max: 30 * time.Second, Multiplier: 2, Jitter: 1}
}

// API is the policy for outbound Spotify calls: slower, and tolerant of a
// provider having a bad minute.
func API() Policy {
	return Policy{MaxAttempts: 5, Base: 500 * time.Millisecond, Max: 60 * time.Second, Multiplier: 2, Jitter: 1}
}

// WithAttempts returns a copy of p with a different attempt budget.
func (p Policy) WithAttempts(n int) Policy { p.MaxAttempts = n; return p }

// Delay returns the deterministic, un-jittered delay before attempt n, where
// attempt 1 is the first try (and therefore has no delay).
func (p Policy) Delay(attempt int) time.Duration {
	if attempt <= 1 {
		return 0
	}
	base := p.Base
	if base <= 0 {
		base = 100 * time.Millisecond
	}
	mult := p.Multiplier
	if mult < 1 {
		mult = 2
	}
	d := float64(base)
	for range attempt - 2 {
		d *= mult
		if p.Max > 0 && d >= float64(p.Max) {
			return p.Max
		}
	}
	if p.Max > 0 && d > float64(p.Max) {
		return p.Max
	}
	return time.Duration(d)
}

// Jittered applies the policy's jitter to the delay for an attempt.
func (p Policy) Jittered(attempt int, rnd func() float64) time.Duration {
	d := p.Delay(attempt)
	if d <= 0 || p.Jitter <= 0 {
		return d
	}
	j := p.Jitter
	if j > 1 {
		j = 1
	}
	if rnd == nil {
		rnd = rand.Float64
	}
	// Keep (1-j) of the delay fixed and randomise the rest, so Jitter=1 yields a
	// uniform draw from [0, d] and Jitter=0.2 keeps at least 80% of the backoff.
	fixed := float64(d) * (1 - j)
	return time.Duration(fixed + rnd()*float64(d)*j)
}

// Permanent wraps an error so Do stops retrying immediately.
type Permanent struct{ Err error }

func (e *Permanent) Error() string { return e.Err.Error() }
func (e *Permanent) Unwrap() error { return e.Err }

// Stop marks err as not worth retrying.
func Stop(err error) error {
	if err == nil {
		return nil
	}
	return &Permanent{Err: err}
}

// RetryAfter asks the caller to wait a specific duration before the next attempt,
// overriding the computed backoff. Spotify's 429 Retry-After header arrives this way.
type RetryAfter struct {
	After time.Duration
	Err   error
}

func (e *RetryAfter) Error() string { return fmt.Sprintf("retry after %s: %v", e.After, e.Err) }
func (e *RetryAfter) Unwrap() error { return e.Err }

// After marks err as retryable no sooner than d.
func After(d time.Duration, err error) error { return &RetryAfter{After: d, Err: err} }

// Hooks observe a retry loop. All fields are optional.
type Hooks struct {
	// OnRetry is called before sleeping, with the attempt that just failed.
	OnRetry func(attempt int, delay time.Duration, err error)
	// Rand supplies jitter in [0,1). Injected by tests for determinism.
	Rand func() float64
	// Sleep defaults to a context-aware timer. Injected by tests to avoid real waits.
	Sleep func(ctx context.Context, d time.Duration) error
}

// Do runs fn until it succeeds, the attempt budget is exhausted, it returns a
// Permanent error, or ctx is done.
//
// The error returned on exhaustion wraps the last failure, so callers can still
// inspect it with errors.As.
func Do(ctx context.Context, p Policy, h Hooks, fn func(ctx context.Context, attempt int) error) error {
	attempts := p.MaxAttempts
	if attempts < 1 {
		attempts = 1
	}
	sleep := h.Sleep
	if sleep == nil {
		sleep = sleepCtx
	}

	var last error
	for attempt := 1; attempt <= attempts; attempt++ {
		if err := ctx.Err(); err != nil {
			if last != nil {
				return fmt.Errorf("%w (last failure: %v)", err, last)
			}
			return err
		}

		err := fn(ctx, attempt)
		if err == nil {
			return nil
		}
		last = err

		var perm *Permanent
		if errors.As(err, &perm) {
			return perm.Err
		}
		if attempt == attempts {
			break
		}

		delay := p.Jittered(attempt+1, h.Rand)
		var ra *RetryAfter
		if errors.As(err, &ra) && ra.After > delay {
			// Honour an explicit upstream instruction over our own schedule.
			delay = ra.After
		}
		if h.OnRetry != nil {
			h.OnRetry(attempt, delay, err)
		}
		if serr := sleep(ctx, delay); serr != nil {
			return fmt.Errorf("%w (last failure: %v)", serr, last)
		}
	}
	return fmt.Errorf("gave up after %d attempts: %w", attempts, last)
}

func sleepCtx(ctx context.Context, d time.Duration) error {
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
