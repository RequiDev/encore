// Package worker supervises the long-lived loops that make up encore-worker.
//
// The worker process runs several independent loops at once — one import runner
// per configured slot, the enrichment worker, the recently-played poller and the
// session reaper — and the reason they sit behind a supervisor is failure
// isolation. An enrichment loop that cannot reach Spotify, or a poller whose
// account listing failed, must not take down an import that is halfway through a
// four-gigabyte file. Each loop therefore runs in its own goroutine, and a loop
// that fails is restarted rather than allowed to end the process.
//
// Three rules describe the whole of the behaviour:
//
//   - a loop that returns an error is logged and restarted after a bounded,
//     jittered delay from internal/retry, so a database that is down for a minute
//     costs a minute of retries rather than a crash loop;
//   - a loop that panics is recovered, logged with its stack, and restarted the
//     same way, because a nil map in one loop is not a reason to stop importing;
//   - a loop that returns nil has decided it is finished — the poller does
//     exactly that when ENCORE_SYNC_ENABLED is false — and is left stopped,
//     because restarting it would only repeat the decision.
//
// Shutdown is bounded rather than indefinite. Once ctx is cancelled Run waits a
// grace period for every loop to return, then gives up and names the ones still
// running, so a stuck loop produces a diagnosable log line instead of a process
// that never exits.
package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/RequiDev/encore/internal/logging"
	"github.com/RequiDev/encore/internal/retry"
)

// DefaultGrace is how long Run waits for the loops to stop before reporting the
// ones that would not. It is deliberately longer than a healthy loop needs:
// every loop in encore-worker checks its context between units of work, and the
// longest of those units is one import batch.
const DefaultGrace = 30 * time.Second

// healthyRun is how long a loop has to stay up for its previous failures to be
// forgotten. Without it a loop that fails once an hour would inherit the delay
// of an incident that ended long ago and take half a minute to come back.
const healthyRun = time.Minute

// restartBackoff is the schedule a failing loop is restarted on. It is bounded
// and jittered like every other retry in Encore, and it never gives up: the
// conditions a loop waits out — Spotify throttling, a database restarting — are
// the kind that resolve on their own.
var restartBackoff = retry.Policy{Base: time.Second, Max: 30 * time.Second, Multiplier: 2, Jitter: 1}

// loop is one supervised unit of work.
type loop struct {
	name string
	fn   func(ctx context.Context) error
}

// Supervisor runs a fixed set of named loops under one shared lifecycle.
//
// The zero value is not usable; call New. A Supervisor runs once: Add every loop
// first, then call Run.
type Supervisor struct {
	log   *slog.Logger
	grace time.Duration

	// sleep, rnd and now are the seams that let a test drive the restart
	// schedule without waiting for it.
	sleep func(ctx context.Context, d time.Duration) error
	rnd   func() float64
	now   func() time.Time

	mu      sync.Mutex
	loops   []loop
	started bool
	// active maps the index of each running loop to its name, so the shutdown
	// message can name what is stuck even when two loops share a name.
	active map[int]string
}

// New builds a Supervisor. A nil logger falls back to the default one.
func New(logger *slog.Logger) *Supervisor {
	if logger == nil {
		logger = slog.Default()
	}
	return &Supervisor{
		log:    logger.With("component", "supervisor"),
		grace:  DefaultGrace,
		sleep:  sleepCtx,
		rnd:    rand.Float64,
		now:    time.Now,
		active: make(map[int]string),
	}
}

// WithGrace bounds how long Run waits for the loops to stop after ctx is
// cancelled. It returns the receiver so it can be chained onto New, and a
// non-positive duration leaves the default in place.
func (s *Supervisor) WithGrace(d time.Duration) *Supervisor {
	if d > 0 {
		s.mu.Lock()
		s.grace = d
		s.mu.Unlock()
	}
	return s
}

// Add registers a loop under a name that identifies it in every log line.
//
// fn is expected to run until its context is cancelled and then return nil.
// Anything else is treated as a failure and restarted.
func (s *Supervisor) Add(name string, fn func(ctx context.Context) error) {
	if fn == nil {
		s.log.Error("a loop was registered without a function and will not run", "loop", name)
		return
	}
	if name == "" {
		name = "unnamed"
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started {
		// Refusing quietly would leave a loop that never runs and no way to tell;
		// the supervisor is wired once at startup, so this is a programming error.
		s.log.Error("a loop was added after the supervisor started and will not run", "loop", name)
		return
	}
	s.loops = append(s.loops, loop{name: name, fn: fn})
}

// Run starts every registered loop and blocks until they have all stopped.
//
// It returns nil when ctx is cancelled and every loop returned within the grace
// period, or when every loop finished of its own accord. It returns an error
// naming the loops that were still running when the grace period expired, and
// when there is nothing registered to run at all.
func (s *Supervisor) Run(ctx context.Context) error {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return errors.New("worker: the supervisor has already been run")
	}
	s.started = true
	loops := slices.Clone(s.loops)
	grace := s.grace
	for i, l := range loops {
		s.active[i] = l.name
	}
	s.mu.Unlock()

	if len(loops) == 0 {
		return errors.New("worker: no loops were registered")
	}

	var wg sync.WaitGroup
	for i, l := range loops {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer s.retire(i)
			s.supervise(ctx, l)
		}()
	}
	s.log.Info("worker loops started", "loops", len(loops), "names", strings.Join(names(loops), ","))

	stopped := make(chan struct{})
	go func() {
		wg.Wait()
		close(stopped)
	}()

	select {
	case <-stopped:
		// Every loop decided it was finished before anybody asked it to stop.
		s.log.Info("every worker loop has finished")
		return nil
	case <-ctx.Done():
	}

	s.log.Info("stopping worker loops", "grace", grace.String())
	timer := time.NewTimer(grace)
	defer timer.Stop()

	select {
	case <-stopped:
		s.log.Info("every worker loop stopped")
		return nil
	case <-timer.C:
		// Naming them is the whole value of the message: "shutdown timed out" on
		// its own tells an operator nothing about which loop to look at.
		return fmt.Errorf("worker: %s did not stop within %s", strings.Join(s.running(), ", "), grace)
	}
}

// supervise runs one loop, restarting it until ctx is cancelled or it finishes.
func (s *Supervisor) supervise(ctx context.Context, l loop) {
	log := s.log.With("loop", l.name)
	failures := 0

	for {
		if ctx.Err() != nil {
			return
		}

		started := s.now()
		err := s.runOnce(ctx, l, log)

		switch {
		case ctx.Err() != nil:
			// The process is shutting down, so whatever the loop returned is a
			// consequence of that rather than a fault of its own.
			log.Info("worker loop stopped")
			return
		case err == nil:
			log.Info("worker loop finished")
			return
		}

		if s.now().Sub(started) >= healthyRun {
			failures = 0
		}
		failures++

		// Jittered counts attempt 1 as the first try, which has no delay, so the
		// first restart is one Base and the schedule grows from there.
		delay := restartBackoff.Jittered(failures+1, s.rnd)
		log.Error("worker loop failed; restarting",
			"failures", failures, "restart_in", delay.String(), logging.Err(err))
		if err := s.sleep(ctx, delay); err != nil {
			return
		}
	}
}

// runOnce calls a loop once, turning a panic into an ordinary error.
//
// Containing the panic here is what makes the isolation promise real: without
// it, a nil dereference in the enrichment worker would unwind past every other
// goroutine and take the whole process, imports included, down with it.
func (s *Supervisor) runOnce(ctx context.Context, l loop, log *slog.Logger) (err error) {
	defer func() {
		if p := recover(); p != nil {
			// The stack is captured inside the recovery, where it still describes
			// the frames that panicked rather than the supervisor's own.
			log.Error("worker loop panicked",
				"panic", fmt.Sprint(p), "stack", string(debug.Stack()))
			err = fmt.Errorf("worker loop %s panicked: %v", l.name, p)
		}
	}()
	return l.fn(ctx)
}

// retire records that the loop at index i has stopped for good.
func (s *Supervisor) retire(i int) {
	s.mu.Lock()
	delete(s.active, i)
	s.mu.Unlock()
}

// running lists the loops that have not stopped, sorted so the message is stable.
func (s *Supervisor) running() []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]string, 0, len(s.active))
	for _, name := range s.active {
		out = append(out, name)
	}
	slices.Sort(out)
	return out
}

// names lists the registered loops in registration order.
func names(loops []loop) []string {
	out := make([]string, len(loops))
	for i, l := range loops {
		out[i] = l.name
	}
	return out
}

// sleepCtx waits for d, returning early when ctx is cancelled.
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
