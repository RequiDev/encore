package lazyfetch

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/RequiDev/encore/internal/store"
)

// fakeLeases stands in for the two lease statements. It is deliberately not a
// database: this package's decisions are about *when* to fetch, which no amount
// of SQL will tell you.
type fakeLeases struct {
	mu         sync.Mutex
	claims     int
	fails      int
	claimOK    bool
	claimErr   error
	claimPanic bool
	lastReason string
	failCtxErr error
}

func (f *fakeLeases) Claim(_ context.Context, _ store.Querier, _ string, _, _ time.Time) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.claims++
	if f.claimPanic {
		panic("a nil Querier reaching QueryRow does exactly this")
	}
	if f.claimErr != nil {
		return false, f.claimErr
	}
	return f.claimOK, nil
}

func (f *fakeLeases) Fail(ctx context.Context, _ store.Querier, _ string, _ time.Time, reason string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fails++
	f.lastReason = reason
	f.failCtxErr = ctx.Err()
	return nil
}

func (f *fakeLeases) counts() (claims, fails int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.claims, f.fails
}

func (f *fakeLeases) reason() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastReason
}

func (f *fakeLeases) recordContextErr() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.failCtxErr
}

// fill records what the Gate asked it to do and can be held open or made to
// fail, which is every shape a caller's Fill has from the Gate's point of view.
type fill struct {
	mu    sync.Mutex
	calls int
	err   error
	block chan struct{}
	// heedCtx makes the fill behave the way a real one does: it gives up when its
	// context ends, and reports the state of that context on return.
	//
	// Off by default, and that is not laziness — it is what makes
	// TestCloseRefusesNewFetchesBeforeItWaits an assertion rather than a race.
	// Close both cancels and waits. A fill that always noticed the cancellation
	// would be released by Close's own cancel, hand back its WaitGroup
	// registration, and let Close out of wg.Wait — closing the very window that
	// test opens to assert inside. Measured: with this on for that test, Mutation
	// A (closing = true moved below wg.Wait) was caught 18 times in 20 rather
	// than every time, and CI runs each package once. With it off, the fill
	// cannot end until the test says so, so Close is provably parked and
	// `closing` is provably unset for the whole window.
	//
	// The two tests that are *about* which context the fill was given turn it on;
	// everything else uses block purely as a barrier — "the fill is in flight,
	// now assert" — and closes it explicitly.
	heedCtx bool
	// sawCtx is the state of the fill's context at the moment it gave up, which
	// is how a test sees *which* context it was handed.
	sawCtx error
}

func (f *fill) run(ctx context.Context, _ string) error {
	f.mu.Lock()
	f.calls++
	block, heed, err := f.block, f.heedCtx, f.err
	f.mu.Unlock()
	if block != nil {
		if !heed {
			<-block
		} else {
			select {
			case <-block:
			case <-ctx.Done():
				f.mu.Lock()
				f.sawCtx = ctx.Err()
				f.mu.Unlock()
				return ctx.Err()
			}
		}
	}
	return err
}

func (f *fill) called() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fill) ctxErr() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sawCtx
}

func discard() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

func policy(enabled bool) Policy {
	return Policy{
		Enabled:          enabled,
		TTL:              30 * 24 * time.Hour,
		LeaseTTL:         2 * time.Minute,
		FailedRetryAfter: 15 * time.Minute,
		FetchTimeout:     90 * time.Second,
		RecordTimeout:    5 * time.Second,
		Concurrency:      4,
	}
}

func newGate(t *testing.T, p Policy, l *fakeLeases, f *fill, now time.Time) *Gate {
	t.Helper()
	g, err := New(p, Deps{
		Leases:  l,
		Fill:    f.run,
		DB:      func() store.Querier { return nil },
		Subject: "thing",
		Logger:  discard(),
		Now:     func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(g.Close)
	return g
}

var at = time.Date(2026, time.July, 31, 12, 0, 0, 0, time.UTC)

// TestResolveStartsTheFirstFillAndReportsPending: nothing attempted is the state
// every entity starts in, and it is the caller's cue to fill.
func TestResolveStartsTheFirstFillAndReportsPending(t *testing.T) {
	l := &fakeLeases{claimOK: true}
	f := &fill{block: make(chan struct{})}
	g := newGate(t, policy(true), l, f, at)
	// The fill ignores its context, so only this test can end it. Registered
	// after the gate, so it runs before Cleanup's Close: a failing assertion
	// below must report itself rather than hang the package on a stuck Cleanup.
	var once sync.Once
	release := func() { once.Do(func() { close(f.block) }) }
	t.Cleanup(release)

	got := g.Resolve(context.Background(), nil, "id-1", State{}, false)
	if got != OutcomePending {
		t.Fatalf("outcome = %q, want %q", got, OutcomePending)
	}
	release()
	g.Close()
	if f.called() != 1 {
		t.Fatalf("fill ran %d times, want 1", f.called())
	}
}

// TestResolveDoesNotWaitForTheFill is the load-bearing property: the page
// request answers while the fill is still running.
func TestResolveDoesNotWaitForTheFill(t *testing.T) {
	l := &fakeLeases{claimOK: true}
	f := &fill{block: make(chan struct{})}
	g := newGate(t, policy(true), l, f, at)
	// See TestResolveStartsTheFirstFillAndReportsPending: the fill ignores its
	// context, so the rescue goes in before Cleanup's Close.
	var once sync.Once
	release := func() { once.Do(func() { close(f.block) }) }
	t.Cleanup(release)

	done := make(chan struct{})
	go func() {
		defer close(done)
		g.Resolve(context.Background(), nil, "id-1", State{}, false)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Resolve did not return while the fill was in flight; the page waits on a third party")
	}
	release()
}

// TestDuePolicy pins all four arms of the schedule at once, including their
// boundaries. Each row varies exactly one input from a row that decides the
// other way, so no row passes for a reason another row already covers.
func TestDuePolicy(t *testing.T) {
	p := policy(true)
	g := newGate(t, p, &fakeLeases{}, &fill{}, at)

	cases := []struct {
		name string
		st   State
		want bool
	}{
		{"never attempted", State{}, true},
		{"ok, one day old", State{Status: StatusOK, FetchedAt: at.Add(-24 * time.Hour)}, false},
		{"ok, exactly at the TTL", State{Status: StatusOK, FetchedAt: at.Add(-p.TTL)}, true},
		{"ok, one second short of the TTL", State{Status: StatusOK, FetchedAt: at.Add(-p.TTL + time.Second)}, false},
		{"failed, inside the backoff", State{Status: StatusFailed, AttemptedAt: at.Add(-time.Minute)}, false},
		{"failed, exactly at the backoff", State{Status: StatusFailed, AttemptedAt: at.Add(-p.FailedRetryAfter)}, true},
		{"fetching, live lease", State{Status: StatusFetching, AttemptedAt: at.Add(-time.Second)}, false},
		{"fetching, exactly at the lease", State{Status: StatusFetching, AttemptedAt: at.Add(-p.LeaseTTL)}, true},
		{"an unknown status is never due", State{Status: "wat"}, false},
	}
	for _, c := range cases {
		if got := g.due(c.st, at); got != c.want {
			t.Errorf("due(%s) = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestEnabledIsCheckedBeforeDue pins an ordering, not a value. A switched-off
// instance must not resume making requests the moment its cache expires, and
// checking `enabled` first is also what stops the claim — a *write* — from being
// attempted at all, which is what an operator asking for no unattended traffic
// actually asked for.
func TestEnabledIsCheckedBeforeDue(t *testing.T) {
	l := &fakeLeases{claimOK: true}
	f := &fill{}
	g := newGate(t, policy(false), l, f, at)

	// Long past the TTL: `due` would say yes, and it must never be asked.
	got := g.Resolve(context.Background(), nil, "id-1", State{
		Status: StatusOK, FetchedAt: at.Add(-400 * 24 * time.Hour),
	}, true)
	g.Close()

	if got != OutcomeReady {
		t.Fatalf("outcome = %q, want %q: turning off fetching does not hide what is on disk", got, OutcomeReady)
	}
	if claims, _ := l.counts(); claims != 0 {
		t.Errorf("a disabled gate claimed the lease %d times, want 0", claims)
	}
	if f.called() != 0 {
		t.Errorf("a disabled gate filled %d times, want 0", f.called())
	}
}

// TestDisabledWithNothingStoredIsDisabledNotUnavailable keeps the two facts
// apart at the source. "Spotify would not answer" and "nobody asked Spotify" are
// different, and a page that renders the first for the second blames a third
// party for a local decision.
func TestDisabledWithNothingStoredIsDisabledNotUnavailable(t *testing.T) {
	g := newGate(t, policy(false), &fakeLeases{}, &fill{}, at)

	got := g.Resolve(context.Background(), nil, "id-1", State{}, false)
	if got != OutcomeDisabled {
		t.Fatalf("outcome = %q, want %q", got, OutcomeDisabled)
	}
}

// TestARecordedFailureInsideItsBackoffIsUnavailable is the only path to
// unavailable, which is what lets a page treat it as a reason to stop polling.
func TestARecordedFailureInsideItsBackoffIsUnavailable(t *testing.T) {
	l := &fakeLeases{claimOK: true}
	f := &fill{}
	g := newGate(t, policy(true), l, f, at)

	got := g.Resolve(context.Background(), nil, "id-1", State{
		Status: StatusFailed, AttemptedAt: at.Add(-time.Minute),
	}, false)
	g.Close()

	if got != OutcomeUnavailable {
		t.Fatalf("outcome = %q, want %q", got, OutcomeUnavailable)
	}
	if f.called() != 0 {
		t.Fatalf("filled %d times inside the backoff, want 0", f.called())
	}
}

// TestALiveLeaseIsNotEvenClaimed stops a polling browser attempting a write on
// every tick. It is checked even on a disabled gate: another replica may have
// started a fill before the switch was flipped.
func TestALiveLeaseIsNotEvenClaimed(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		l := &fakeLeases{claimOK: true}
		g := newGate(t, policy(enabled), l, &fill{}, at)

		got := g.Resolve(context.Background(), nil, "id-1", State{
			Status: StatusFetching, AttemptedAt: at.Add(-time.Second),
		}, false)
		g.Close()

		if got != OutcomePending {
			t.Errorf("enabled=%v: outcome = %q, want %q while another replica holds the lease",
				enabled, got, OutcomePending)
		}
		if claims, _ := l.counts(); claims != 0 {
			t.Errorf("enabled=%v: a live lease was claimed %d times, want 0", enabled, claims)
		}
	}
}

// TestALostClaimAndAClaimErrorAreBothPending. Neither records an outcome, so
// neither may report one: the next request re-enters the same branch. Reporting
// either as unavailable would blame a third party for a local condition and tell
// a page whose job is to stop polling on unavailable to give up on a fill that
// was still coming.
func TestALostClaimAndAClaimErrorAreBothPending(t *testing.T) {
	for name, l := range map[string]*fakeLeases{
		"lost claim":  {claimOK: false},
		"claim error": {claimErr: errors.New("read-only transaction")},
	} {
		f := &fill{}
		g := newGate(t, policy(true), l, f, at)

		got := g.Resolve(context.Background(), nil, "id-1", State{}, false)
		g.Close()

		if got != OutcomePending {
			t.Errorf("%s: outcome = %q, want %q", name, got, OutcomePending)
		}
		if _, fails := l.counts(); fails != 0 {
			t.Errorf("%s: recorded %d failures, want 0", name, fails)
		}
		if f.called() != 0 {
			t.Errorf("%s: filled %d times, want 0", name, f.called())
		}
	}
}

// TestNoFreeSlotIsPendingAndClaimsNothing. Refusing is the point: queueing here
// would be queueing people behind a third party.
func TestNoFreeSlotIsPendingAndClaimsNothing(t *testing.T) {
	p := policy(true)
	p.Concurrency = 1
	l := &fakeLeases{claimOK: true}
	f := &fill{block: make(chan struct{})}
	g := newGate(t, p, l, f, at)
	// See TestResolveStartsTheFirstFillAndReportsPending: the fill ignores its
	// context, so the rescue goes in before Cleanup's Close.
	var once sync.Once
	release := func() { once.Do(func() { close(f.block) }) }
	t.Cleanup(release)

	if got := g.Resolve(context.Background(), nil, "id-1", State{}, false); got != OutcomePending {
		t.Fatalf("first outcome = %q, want %q", got, OutcomePending)
	}
	// The one slot is taken and its fill cannot finish.
	if got := g.Resolve(context.Background(), nil, "id-2", State{}, false); got != OutcomePending {
		t.Fatalf("second outcome = %q, want %q: local backpressure records no outcome, so it must "+
			"not report one", got, OutcomePending)
	}
	if claims, _ := l.counts(); claims != 1 {
		t.Fatalf("claimed %d leases with one slot, want 1: a lease taken with no slot to fill it "+
			"strands the entity for the whole LeaseTTL", claims)
	}
	release()
}

// TestAFailingFillIsRecordedAndTheErrorNeverReturned: a third-party outage is a
// state the page renders, not a 500 it shows.
func TestAFailingFillIsRecordedAndTheErrorNeverReturned(t *testing.T) {
	l := &fakeLeases{claimOK: true}
	f := &fill{err: errors.New("upstream: 503 service unavailable")}
	g := newGate(t, policy(true), l, f, at)

	if got := g.Resolve(context.Background(), nil, "id-1", State{}, false); got != OutcomePending {
		t.Fatalf("outcome = %q, want %q", got, OutcomePending)
	}
	g.Close()

	if _, fails := l.counts(); fails != 1 {
		t.Fatalf("recorded %d failures, want 1", fails)
	}
	// Handed over whole: the repository bounds it with store.Truncate, which cuts
	// on a rune boundary, and truncating again here would risk two ellipses.
	if got := l.reason(); got != "upstream: 503 service unavailable" {
		t.Fatalf("recorded reason = %q, want the cause whole", got)
	}
}

// TestTheFillOutlivesTheRequestContext. The request has already been answered,
// and cancelling when the browser navigated away would mean the answer never
// arrives however many times the page is opened.
func TestTheFillOutlivesTheRequestContext(t *testing.T) {
	l := &fakeLeases{claimOK: true}
	release := make(chan struct{})
	// heedCtx, because this test is *about* which context the fill was handed: a
	// fill that ignored it could not tell a request's cancellation from anything
	// else, and there would be nothing to observe.
	f := &fill{block: release, heedCtx: true}
	g := newGate(t, policy(true), l, f, at)

	ctx, cancel := context.WithCancel(context.Background())
	g.Resolve(ctx, nil, "id-1", State{}, false)
	cancel()       // the browser navigated away
	close(release) // and the fill is let go the legitimate way

	// Wait for the fill itself, not for Close. Close cancels g.base, and a fill
	// sitting in a select over its block channel and its context would then have
	// both arms ready — so half the runs would report the *shutdown's*
	// cancellation as the request's and fail an assertion about something else
	// entirely. track has already raised this counter synchronously inside
	// Resolve, so by here it is exactly the fill and this cannot deadlock. The
	// original of this test bought the same barrier with an `ended` channel on
	// its fake (see albumtracks_test.go:895); in-package, the WaitGroup is the
	// same statement with nothing extra to maintain.
	g.wg.Wait()
	g.Close()

	if _, fails := l.counts(); fails != 0 {
		t.Fatalf("recorded %d failures, want 0: the fill died with the request that started it", fails)
	}
	if f.ctxErr() != nil {
		t.Fatalf("the fill saw %v; it must not inherit the request's cancellation", f.ctxErr())
	}
}

// TestAPanicInTheClaimDoesNotLeakASlot is why the slot goes back on a defer.
// Released by explicit calls instead, a panic below the acquisition keeps the
// slot for the life of the process — a nil store.Querier reaching QueryRow is one
// line of a future caller away — and Concurrency of those stop this process
// filling anything again, silently, because recovery middleware keeps serving
// pages.
func TestAPanicInTheClaimDoesNotLeakASlot(t *testing.T) {
	p := policy(true)
	p.Concurrency = 1
	l := &fakeLeases{claimOK: true, claimPanic: true}
	f := &fill{}
	g := newGate(t, p, l, f, at)

	func() {
		defer func() { _ = recover() }()
		g.Resolve(context.Background(), nil, "id-1", State{}, false)
	}()

	l.mu.Lock()
	l.claimPanic = false
	l.mu.Unlock()

	if got := g.Resolve(context.Background(), nil, "id-2", State{}, false); got != OutcomePending {
		t.Fatalf("outcome after a panic = %q, want %q", got, OutcomePending)
	}
	g.Close()
	if f.called() != 1 {
		t.Fatalf("filled %d times after a panicking claim, want 1: the slot leaked and this process "+
			"will never fill anything again", f.called())
	}
}

// TestNewRejectsAnUnusablePolicy. A half-configured gate answers some entities
// and strands others.
func TestNewRejectsAnUnusablePolicy(t *testing.T) {
	ok := Deps{Leases: &fakeLeases{}, Fill: (&fill{}).run, DB: func() store.Querier { return nil }}

	for name, deps := range map[string]Deps{
		"no leases": {Fill: ok.Fill, DB: ok.DB},
		"no fill":   {Leases: ok.Leases, DB: ok.DB},
		"no db":     {Leases: ok.Leases, Fill: ok.Fill},
	} {
		if _, err := New(policy(true), deps); err == nil {
			t.Errorf("New with %s succeeded", name)
		}
	}

	bad := map[string]func(p *Policy){
		"zero TTL":           func(p *Policy) { p.TTL = 0 },
		"zero lease":         func(p *Policy) { p.LeaseTTL = 0 },
		"zero backoff":       func(p *Policy) { p.FailedRetryAfter = 0 },
		"zero fetch timeout": func(p *Policy) { p.FetchTimeout = 0 },
		"zero record":        func(p *Policy) { p.RecordTimeout = 0 },
		"zero concurrency":   func(p *Policy) { p.Concurrency = 0 },
	}
	for name, mutate := range bad {
		p := policy(true)
		mutate(&p)
		if _, err := New(p, ok); err == nil {
			t.Errorf("New with %s succeeded", name)
		}
	}
}

// TestNewRefusesALeaseShorterThanTheFetchTimeout is an invariant that was a
// comment in each caller and checked by nobody. A fill that can outlive its own
// lease lets a second replica reclaim the entity and start a duplicate walk
// against a quota the whole application shares — and the two then race to
// replace the same rows.
func TestNewRefusesALeaseShorterThanTheFetchTimeout(t *testing.T) {
	p := policy(true)
	p.LeaseTTL = time.Minute
	p.FetchTimeout = time.Minute // equal is not enough: the lease must outlast the fill
	ok := Deps{Leases: &fakeLeases{}, Fill: (&fill{}).run, DB: func() store.Querier { return nil }}

	if _, err := New(p, ok); err == nil {
		t.Fatal("New accepted a lease no longer than the fetch timeout; a live fill loses its own lease")
	}

	p.LeaseTTL = time.Minute + time.Nanosecond
	if _, err := New(p, ok); err != nil {
		t.Fatalf("New rejected a lease longer than the fetch timeout: %v", err)
	}
}

// --- the detached fill and the shutdown race -------------------------------

// TestCloseEndsAFillInFlight is the other side of detaching: a goroutine on a
// context nobody cancels is a leak, and a process that cannot shut down.
func TestCloseEndsAFillInFlight(t *testing.T) {
	l := &fakeLeases{claimOK: true}
	block := make(chan struct{}) // deliberately never closed by the test body
	// heedCtx, because the fill ending when Close cancels it is the whole
	// assertion: with it off, Close would wait for a fill nothing can release.
	f := &fill{block: block, heedCtx: true}
	g := newGate(t, policy(true), l, f, at)

	// A rescue, registered after the gate so it runs *before* Cleanup's Close: if
	// Close turns out not to cancel anything, the assertion below reports that
	// rather than hanging the whole package on a stuck Cleanup.
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(block) }) })

	if got := g.Resolve(context.Background(), nil, "id-1", State{}, false); got != OutcomePending {
		t.Fatalf("outcome = %q, want %q", got, OutcomePending)
	}

	closed := make(chan struct{})
	go func() {
		defer close(closed)
		g.Close()
	}()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return while a fill was in flight; the fill is on a context " +
			"nothing cancels, so it outlives the gate")
	}

	if err := f.ctxErr(); err == nil {
		t.Fatal("the fill's context was still live when Close returned")
	}
	// The write that records the failure must NOT inherit the cancellation that
	// just ended the fill. Passing g.base straight to record looks like a tidy-up
	// — it is already the fill's parent — and every other assertion here survives
	// it, because the fill is cancelled either way. What does not survive is
	// production: pgx refuses a cancelled context, so the failure is never
	// written, the entity stays 'fetching' from the claim, and after the restart
	// every viewer polls uselessly for the full LeaseTTL.
	if err := l.recordContextErr(); err != nil {
		t.Errorf("the failure was recorded on a context already in error (%v); it must be "+
			"detached from the one Close cancels, or nothing durable records the failure "+
			"and the entity stays 'fetching' until its lease expires", err)
	}
	// And it was recorded at all, which is what stops the assertion above passing
	// on a Fail that never ran: this fake keeps no cancelled-context refusal of
	// its own, so the count is the proof the write was attempted.
	if _, fails := l.counts(); fails != 1 {
		t.Fatalf("recorded %d failures for a cancelled fill, want 1", fails)
	}
}

// TestNothingIsStartedAfterClose is the shutdown race, asserted on the one
// operation that must not run concurrently with Close.
//
// http.Server.Shutdown returns on timeout with handlers still running, so a
// request can be inside Resolve while Close is waiting. Registering a fill then
// is at best a goroutine nothing waits for — which is exactly what Close
// promises cannot happen — and at worst raises the WaitGroup counter from zero
// with a waiter already parked, which panics "Add called concurrently with Wait"
// and takes the process down on its way out.
func TestNothingIsStartedAfterClose(t *testing.T) {
	l := &fakeLeases{claimOK: true}
	f := &fill{}
	g := newGate(t, policy(true), l, f, at)

	if !g.track() {
		t.Fatal("track refused a fill before Close")
	}
	g.wg.Done() // hand the registration straight back

	g.Close()

	if g.track() {
		g.wg.Done()
		t.Error("track registered a fill after Close; wg.Add can then run concurrently " +
			"with wg.Wait, which panics at shutdown, and when it does not, leaves a " +
			"goroutine nothing waits for")
	}
	// And the whole path declines, without so much as taking a lease it would
	// only abandon.
	if got := g.Resolve(context.Background(), nil, "id-1", State{}, false); got != OutcomePending {
		t.Errorf("outcome after Close = %q, want %q: nothing was recorded, so it is still "+
			"\"not yet\"", got, OutcomePending)
	}
	if claims, _ := l.counts(); claims != 0 {
		t.Errorf("claimed %d leases after Close, want 0: the entity would sit 'fetching' "+
			"for the whole LeaseTTL while this process exits", claims)
	}
	if f.called() != 0 {
		t.Errorf("filled %d times after Close, want 0", f.called())
	}
}

// TestCloseRefusesNewFetchesBeforeItWaits pins the *ordering* inside Close,
// which TestNothingIsStartedAfterClose does not.
//
// That test asserts the post-condition — track refuses once Close has returned
// — and a Close written as cancel(); wg.Wait(); closing = true satisfies it
// perfectly while reintroducing the whole M1 race: for as long as Close is
// parked in Wait, track still says yes, so a request already inside Resolve
// can raise the WaitGroup counter with a waiter registered. Neither -race nor
// -count catches that, because the window needs a concurrent Resolve to be
// open at all. So this test opens one.
//
// The invariant being pinned is that closing is set before wg.Wait. Moving the
// assignment after the Wait fails this test every time.
//
// Two things about the mechanism, for whoever edits Close next. It reads
// g.base.Done() as the signal that Close has begun, which is a *sound* signal
// only because Close sets closing before it cancels. Moving the assignment to
// between the cancel and the Wait leaves Close correct — the invariant still
// holds — but silently costs this test its detector: base.Done() would then
// fire while closing is still unset, and the assertion below would be racing
// rather than asserting. Verified: that variant passes 30 runs out of 30. So
// keep the assignment first, not because Close needs it there, but because
// this test stops meaning anything if it moves.
// TestCloseSetsClosingBeforeItCancels in ordering_test.go is what catches that
// move, and it is a source-order assertion precisely because no outcome-based
// test can be.
func TestCloseRefusesNewFetchesBeforeItWaits(t *testing.T) {
	l := &fakeLeases{claimOK: true}
	block := make(chan struct{})
	// heedCtx deliberately off, and it is the whole mechanism of this test. A
	// fill that noticed its context would be released by Close's own cancel,
	// hand its registration back, and let Close out of wg.Wait — so the window
	// this test asserts inside would be closing while it asserted, and the
	// assertion would be sampling a race. Ignoring the context makes the fill
	// end only when this test says so, which is what makes "Close is parked"
	// a fact rather than a likelihood.
	f := &fill{block: block}
	g := newGate(t, policy(true), l, f, at)
	var once sync.Once
	release := func() { once.Do(func() { close(block) }) }
	t.Cleanup(release)

	// A fill that genuinely cannot finish — nothing but release() can end it —
	// so Close is guaranteed to park in wg.Wait rather than running straight
	// through it.
	if got := g.Resolve(context.Background(), nil, "id-1", State{}, false); got != OutcomePending {
		t.Fatalf("outcome = %q, want %q", got, OutcomePending)
	}

	closed := make(chan struct{})
	go func() {
		defer close(closed)
		g.Close()
	}()

	// Close has reached at least its cancel, and cannot get past Wait until this
	// test releases the fill. Everything below happens inside that window.
	<-g.base.Done()

	if g.track() {
		// Undo it, or the Close parked above never returns and the failure below
		// is buried under a timeout.
		g.wg.Done()
		t.Error("track accepted a fill while Close was waiting: Close marks itself " +
			"closing only after wg.Wait returns, so a request already inside Resolve " +
			"can still call wg.Add against a registered waiter — the panic M1 fixed")
	}

	release()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return after the fill was released")
	}
}
