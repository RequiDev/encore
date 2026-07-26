package spotify

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestLimiterPacesAfterBurst(t *testing.T) {
	clock := newFakeClock()
	l := NewLimiterWithClock(10, 1, clock)

	for range 3 {
		if err := l.Wait(context.Background()); err != nil {
			t.Fatalf("Wait: %v", err)
		}
	}

	// The first call spends the burst; each further call waits one token's worth
	// of time at ten per second.
	want := []time.Duration{100 * time.Millisecond, 100 * time.Millisecond}
	got := clock.sleeps()
	if len(got) != len(want) {
		t.Fatalf("slept %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("slept %v, want %v", got, want)
		}
	}
}

func TestLimiterBurstIsNotDelayed(t *testing.T) {
	clock := newFakeClock()
	l := NewLimiterWithClock(1, 5, clock)

	for range 5 {
		if err := l.Wait(context.Background()); err != nil {
			t.Fatalf("Wait: %v", err)
		}
	}
	if got := clock.sleeps(); len(got) != 0 {
		t.Fatalf("slept %v, want nothing while the burst lasts", got)
	}
}

func TestLimiterPause(t *testing.T) {
	clock := newFakeClock()
	l := NewLimiterWithClock(1000, 100, clock)

	l.Pause(fakeStart.Add(30 * time.Second))
	// A shorter pause must not shorten the one already in force.
	l.Pause(fakeStart.Add(5 * time.Second))
	if got, want := l.PausedUntil(), fakeStart.Add(30*time.Second); !got.Equal(want) {
		t.Fatalf("PausedUntil = %s, want %s", got, want)
	}

	if err := l.Wait(context.Background()); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	got := clock.sleeps()
	if len(got) != 1 || got[0] != 30*time.Second {
		t.Fatalf("slept %v, want [30s]", got)
	}

	// Once the pause has elapsed the limiter serves immediately again.
	if err := l.Wait(context.Background()); err != nil {
		t.Fatalf("Wait after pause: %v", err)
	}
	if len(clock.sleeps()) != 1 {
		t.Fatalf("slept %v, want no further wait", clock.sleeps())
	}
}

// hookClock runs a callback once, immediately after the first sleep finishes,
// which is how a test lands an event in the middle of a Wait.
type hookClock struct {
	*fakeClock
	once sync.Once
	on   func()
}

func (h *hookClock) Sleep(ctx context.Context, d time.Duration) error {
	err := h.fakeClock.Sleep(ctx, d)
	h.once.Do(h.on)
	return err
}

func TestLimiterPauseDeclaredWhileWaiting(t *testing.T) {
	clock := &hookClock{fakeClock: newFakeClock(), on: func() {}}
	l := NewLimiterWithClock(10, 1, clock)
	// The pause lands after the second caller has already reserved its token and
	// gone to sleep. It must still be honoured: a 429 has to hold back requests
	// that are already queued, not only the ones that arrive afterwards.
	clock.on = func() { l.Pause(clock.Now().Add(20 * time.Second)) }

	for range 2 {
		if err := l.Wait(context.Background()); err != nil {
			t.Fatalf("Wait: %v", err)
		}
	}

	got := clock.sleeps()
	want := []time.Duration{100 * time.Millisecond, 20 * time.Second}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("slept %v, want %v", got, want)
	}
}

func TestLimiterWaitReturnsOnCancelledContext(t *testing.T) {
	l := NewLimiter(1, 1)

	// Spend the burst so the next call has a full second to wait for.
	if err := l.Wait(context.Background()); err != nil {
		t.Fatalf("Wait: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	err := l.Wait(ctx)
	elapsed := time.Since(start)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Wait error = %v, want context.Canceled", err)
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("Wait took %s to notice cancellation", elapsed)
	}
}

func TestLimiterWaitReturnsImmediatelyWhenContextAlreadyDone(t *testing.T) {
	l := NewLimiter(1, 1)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	if err := l.Wait(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Wait error = %v, want context.Canceled", err)
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("Wait took %s on an already cancelled context", elapsed)
	}
}

// interruptedClock reports every sleep as cancelled, standing in for a context
// that finishes while a caller is queued.
type interruptedClock struct{ *fakeClock }

func (interruptedClock) Sleep(context.Context, time.Duration) error { return context.Canceled }

func TestLimiterCancelledWaitRefundsItsToken(t *testing.T) {
	l := NewLimiterWithClock(1, 1, interruptedClock{newFakeClock()})

	if err := l.Wait(context.Background()); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if err := l.Wait(context.Background()); !errors.Is(err, context.Canceled) {
		t.Fatalf("Wait error = %v, want context.Canceled", err)
	}

	// The interrupted caller spent no quota, so the balance is back where the
	// first successful call left it rather than a token in debt.
	l.mu.Lock()
	tokens := l.tokens
	l.mu.Unlock()
	if tokens != 0 {
		t.Fatalf("tokens = %v, want 0 after the reservation was handed back", tokens)
	}
}

func TestLimiterIsConcurrencySafe(t *testing.T) {
	clock := newFakeClock()
	l := NewLimiterWithClock(1000, 50, clock)

	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 5 {
				if err := l.Wait(context.Background()); err != nil {
					t.Errorf("Wait: %v", err)
					return
				}
				l.Pause(l.PausedUntil())
			}
		}()
	}
	wg.Wait()
}
