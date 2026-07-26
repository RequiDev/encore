package worker

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// quiet is a logger that keeps test output readable.
func quiet() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// instantSleep replaces the restart delay so a test never waits for one.
func instantSleep(context.Context, time.Duration) error { return nil }

func TestRunReturnsWhenEveryLoopFinishes(t *testing.T) {
	s := New(quiet())

	var ran atomic.Int32
	for range 3 {
		s.Add("done", func(context.Context) error {
			ran.Add(1)
			return nil
		})
	}

	if err := s.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := ran.Load(); got != 3 {
		t.Fatalf("ran %d loops, want 3", got)
	}
}

func TestFailingLoopIsRestarted(t *testing.T) {
	s := New(quiet())
	s.sleep = instantSleep

	var runs atomic.Int32
	s.Add("flaky", func(context.Context) error {
		if runs.Add(1) < 3 {
			return errors.New("the database went away")
		}
		return nil
	})

	if err := s.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := runs.Load(); got != 3 {
		t.Fatalf("loop ran %d times, want 3 (two failures and one clean finish)", got)
	}
}

// A failing loop must not stop its neighbours: that isolation is the reason the
// supervisor exists at all.
func TestOneFailingLoopDoesNotStopAnother(t *testing.T) {
	s := New(quiet())
	s.sleep = instantSleep

	var failures atomic.Int32
	s.Add("failing", func(context.Context) error {
		if failures.Add(1) < 4 {
			return errors.New("spotify is unreachable")
		}
		return nil
	})

	imported := make(chan struct{})
	s.Add("import", func(ctx context.Context) error {
		close(imported)
		<-ctx.Done()
		return nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		<-imported
		// Let the failing loop exhaust its restarts before shutting down.
		for failures.Load() < 4 {
			time.Sleep(time.Millisecond)
		}
		cancel()
	}()

	if err := s.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := failures.Load(); got != 4 {
		t.Fatalf("failing loop ran %d times, want 4", got)
	}
}

func TestPanicIsRecoveredLoggedAndRestarted(t *testing.T) {
	var buf bytes.Buffer
	s := New(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	s.sleep = instantSleep

	var runs atomic.Int32
	s.Add("panicky", func(context.Context) error {
		if runs.Add(1) == 1 {
			panic("assignment to entry in nil map")
		}
		return nil
	})

	if err := s.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := runs.Load(); got != 2 {
		t.Fatalf("loop ran %d times, want 2 (one panic and one clean finish)", got)
	}

	// Run has returned, so every supervised goroutine has stopped writing.
	logged := buf.String()
	for _, want := range []string{"worker loop panicked", "nil map", "stack="} {
		if !strings.Contains(logged, want) {
			t.Fatalf("the panic log is missing %q:\n%s", want, logged)
		}
	}
}

func TestLoopsStopWhenTheContextIsCancelled(t *testing.T) {
	s := New(quiet())

	started := make(chan struct{})
	var stopped atomic.Bool
	s.Add("blocking", func(ctx context.Context) error {
		close(started)
		<-ctx.Done()
		stopped.Store(true)
		return nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		<-started
		cancel()
	}()

	if err := s.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !stopped.Load() {
		t.Fatal("the loop did not observe the cancellation")
	}
}

// A loop that returns an error while the process is shutting down is not
// restarted: the failure is a consequence of the cancellation.
func TestErrorDuringShutdownDoesNotRestart(t *testing.T) {
	s := New(quiet())
	s.sleep = instantSleep

	started := make(chan struct{})
	var runs atomic.Int32
	s.Add("noisy", func(ctx context.Context) error {
		if runs.Add(1) == 1 {
			close(started)
		}
		<-ctx.Done()
		return errors.New("query cancelled")
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		<-started
		cancel()
	}()

	if err := s.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := runs.Load(); got != 1 {
		t.Fatalf("loop ran %d times, want 1", got)
	}
}

func TestRunReportsLoopsStillRunningAfterTheGrace(t *testing.T) {
	s := New(quiet()).WithGrace(20 * time.Millisecond)

	entered := make(chan struct{})
	release := make(chan struct{})
	s.Add("stuck", func(context.Context) error {
		close(entered)
		<-release
		return nil
	})
	s.Add("obedient", func(ctx context.Context) error {
		<-ctx.Done()
		return nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		<-entered
		cancel()
	}()

	err := s.Run(ctx)
	close(release)
	if err == nil {
		t.Fatal("Run returned nil with a loop still running")
	}
	if !strings.Contains(err.Error(), "stuck") {
		t.Fatalf("the error does not name the stuck loop: %v", err)
	}
	if strings.Contains(err.Error(), "obedient") {
		t.Fatalf("the error names a loop that did stop: %v", err)
	}
}

func TestRestartDelaysFollowTheBackoffSchedule(t *testing.T) {
	s := New(quiet())
	// Full jitter at its maximum draw yields exactly the computed delay, which
	// is what makes the schedule assertable.
	s.rnd = func() float64 { return 1 }

	var mu sync.Mutex
	var delays []time.Duration
	s.sleep = func(_ context.Context, d time.Duration) error {
		mu.Lock()
		delays = append(delays, d)
		mu.Unlock()
		return nil
	}

	runs := 0
	s.Add("always-fails", func(context.Context) error {
		runs++
		if runs > 8 {
			return nil
		}
		return errors.New("boom")
	})

	if err := s.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	want := []time.Duration{
		time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second,
		16 * time.Second, 30 * time.Second, 30 * time.Second, 30 * time.Second,
	}
	mu.Lock()
	defer mu.Unlock()
	if len(delays) != len(want) {
		t.Fatalf("recorded %d delays, want %d: %v", len(delays), len(want), delays)
	}
	for i, d := range delays {
		if d != want[i] {
			t.Fatalf("delay %d was %s, want %s (schedule: %v)", i+1, d, want[i], delays)
		}
	}
	if last := delays[len(delays)-1]; last > restartBackoff.Max {
		t.Fatalf("the schedule exceeded its cap: %s > %s", last, restartBackoff.Max)
	}
}

func TestFailuresAreForgottenAfterAHealthyRun(t *testing.T) {
	s := New(quiet())
	s.rnd = func() float64 { return 1 }

	// Every reading of the clock is two minutes after the last, so each run looks
	// like it stayed up well past healthyRun.
	fake := time.Unix(0, 0)
	s.now = func() time.Time {
		fake = fake.Add(2 * time.Minute)
		return fake
	}

	var mu sync.Mutex
	var delays []time.Duration
	s.sleep = func(_ context.Context, d time.Duration) error {
		mu.Lock()
		delays = append(delays, d)
		mu.Unlock()
		return nil
	}

	runs := 0
	s.Add("long-lived", func(context.Context) error {
		runs++
		if runs > 4 {
			return nil
		}
		return errors.New("spotify returned 502")
	})

	if err := s.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(delays) != 4 {
		t.Fatalf("recorded %d delays, want 4: %v", len(delays), delays)
	}
	for i, d := range delays {
		if d != restartBackoff.Base {
			t.Fatalf("delay %d was %s, want the base delay %s: a healthy run should reset the schedule",
				i+1, d, restartBackoff.Base)
		}
	}
}

func TestRunWithoutLoopsIsAnError(t *testing.T) {
	if err := New(quiet()).Run(context.Background()); err == nil {
		t.Fatal("Run returned nil with nothing registered")
	}
}

func TestAddIgnoresANilFunction(t *testing.T) {
	s := New(quiet())
	s.Add("nothing", nil)

	if len(s.loops) != 0 {
		t.Fatalf("registered %d loops, want 0", len(s.loops))
	}
	if err := s.Run(context.Background()); err == nil {
		t.Fatal("Run returned nil with nothing registered")
	}
}

func TestAddAfterRunIsRefusedAndRunIsSingleUse(t *testing.T) {
	s := New(quiet())
	s.Add("once", func(context.Context) error { return nil })

	if err := s.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	s.Add("late", func(context.Context) error { return nil })
	if len(s.loops) != 1 {
		t.Fatalf("a loop added after Run was registered: %d loops", len(s.loops))
	}
	if err := s.Run(context.Background()); err == nil {
		t.Fatal("the second Run was allowed")
	}
}

func TestWithGraceIgnoresNonPositiveDurations(t *testing.T) {
	s := New(quiet()).WithGrace(0).WithGrace(-time.Second)
	if s.grace != DefaultGrace {
		t.Fatalf("grace is %s, want the default %s", s.grace, DefaultGrace)
	}
	if got := New(quiet()).WithGrace(5 * time.Second).grace; got != 5*time.Second {
		t.Fatalf("grace is %s, want 5s", got)
	}
}
