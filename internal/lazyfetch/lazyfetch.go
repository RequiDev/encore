// Package lazyfetch is the machinery behind Encore's lazily filled upstream
// caches: the album page's track listing and the artist page's discography.
//
// Both answer the same shape of question. Something a page wants is held by a
// third party, is expensive to ask for, and must never be asked for on the
// request that needs it. So it is read the first time somebody opens the
// relevant page, kept for a TTL, and refreshed by a later view. A sweep over
// every entity in a history is rejected explicitly in both cases: most are never
// viewed, and enumerating them all would spend the instance's quota on questions
// nobody asked.
//
// What this package guarantees is that the page request never waits: Resolve
// answers from what the caller already read out of the database and, when a fill
// is due, hands the work to a goroutine on a context of its own.
//
// Two guards keep that from becoming a stampede, and they answer different
// questions. A bounded slot channel is this process asking "am I already busy?"
// A conditional write against the caller's fetch table is the whole deployment
// asking "is anybody busy?" — and only the second survives two browser tabs, two
// API replicas, or a page that polls.
//
// # What is here and what is not
//
// Everything in this package is about *whether and when* to fill. Nothing in it
// is about what filling means. Pagination, response decoding, what counts as an
// empty answer, truncation, filtering and the transaction that stores the result
// are all the caller's, behind the single Fill seam — and they genuinely differ:
// one caller treats an empty response as a failure and an empty *filtered* set
// as impossible, while the other treats an empty response as a failure and an
// empty filtered set as an ordinary success. Putting that rule here would make
// this package wrong for its second caller.
//
// No copy lives here either. Outcome names four situations; the words a page
// says about them belong to the page.
package lazyfetch

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/RequiDev/encore/internal/logging"
	"github.com/RequiDev/encore/internal/store"
)

// The three states of one entity's fill, as stored in the caller's fetch table.
//
// They are string constants rather than a type so a caller's own column
// constants can be compared to them without conversion; the callers'
// repositories declare the same three values against their own tables, and each
// caller's tests fail if either set drifts — due would stop recognising the
// status, fall through to its default, and never refresh anything.
const (
	// StatusFetching is a lease: somebody is filling this entity now.
	StatusFetching = "fetching"
	// StatusOK means the caller's table holds a complete answer.
	StatusOK = "ok"
	// StatusFailed means the last attempt did not produce one. Whatever the
	// caller stores is from an earlier, successful attempt.
	StatusFailed = "failed"
)

// Outcome is what a page can say. Four values, and the distinctions between them
// are the whole reason this package exists rather than a boolean.
type Outcome string

const (
	// OutcomeReady means an answer is stored and can be reasoned about. It may be
	// older than the TTL, in which case a refresh is already running behind it.
	OutcomeReady Outcome = "ready"
	// OutcomePending means nothing is stored yet and a fill is running, or is due
	// and nothing has recorded a reason it should not be.
	//
	// Everything that merely *delays* a fill reports this: a lease somebody else
	// holds, no free local slot, a claim that errored, a shutdown in progress.
	// None of them records an *outcome*, which is what makes "keep polling" the
	// right advice — but they do not all resolve the same way. Most leave the row
	// untouched, so the entity is still due and the very next view starts it. A
	// claim this process wins and then abandons at shutdown leaves the row
	// 'fetching' with a fresh attempted_at, so the next view waits out LeaseTTL
	// before anybody reclaims it. Still pending, just not immediately.
	//
	// Nothing here bounds how long that can go on. A claim that errors records
	// nothing, so the next request re-enters the same branch; a client polling
	// this must cap itself.
	OutcomePending Outcome = "pending"
	// OutcomeUnavailable means nothing is stored and the last attempt failed,
	// recently enough that no new one has been started.
	//
	// It is emphatically not "this entity has nothing", and it is deliberately
	// not "this process was too busy to ask" either: only a *recorded* failure
	// reaches here, which is what lets a page treat it as a reason to stop
	// polling and say so. Reporting local backpressure as unavailable would tell
	// somebody the upstream would not answer when it was never asked — the same
	// category error that keeps OutcomeDisabled separate.
	OutcomeUnavailable Outcome = "unavailable"
	// OutcomeDisabled means nothing is stored and this instance will not fetch
	// it, because its operator turned that off.
	//
	// Deliberately not folded into OutcomeUnavailable. "The upstream would not
	// answer" and "nobody asked the upstream" are different facts, and a page
	// that renders the first for the second blames a third party for a local
	// decision.
	OutcomeDisabled Outcome = "disabled"
)

// State is the caller's bookkeeping row, in the only terms this package needs.
//
// The zero value — Status "" and both instants zero — is an entity that has
// never been attempted, which is an ordinary state rather than an error: every
// entity is in it until somebody first opens its page.
type State struct {
	Status      string
	FetchedAt   time.Time
	AttemptedAt time.Time
	// Attempts is the count of consecutive failures. Nothing here reads it yet:
	// it is carried because both callers already store it and an escalating
	// backoff — the obvious next use — would be decided here rather than in a
	// caller.
	Attempts int
}

// Policy is the timing, all of it supplied by the caller because the right
// values depend on what is being fetched: an album's track list is one request
// and effectively immutable after release, while an artist's discography is up
// to forty requests and grows.
type Policy struct {
	// Enabled is the operator's switch. False means this instance never asks the
	// upstream anything — and note that it does not mean "forget what is on
	// disk": a stored answer is still reported ready.
	Enabled bool
	// TTL is how long a stored answer is trusted. Ignored entirely when Enabled
	// is false: nothing refreshes, so nothing expires.
	TTL time.Duration
	// LeaseTTL is how long a 'fetching' row holds other callers off. It must
	// exceed FetchTimeout, so a live fill never loses its own lease, and it
	// should be short enough that a process killed mid-fill does not strand the
	// entity for long.
	LeaseTTL time.Duration
	// FailedRetryAfter is how long a failed entity is left alone. Much shorter
	// than a TTL in both callers, and deliberately: failures here are timeouts
	// and rate limits, which clear in minutes, and making somebody wait out a
	// month-long TTL would turn one bad minute into a broken panel.
	FailedRetryAfter time.Duration
	// FetchTimeout bounds one entity's whole fill — every page, every retry and
	// every rate-limit wait inside it.
	FetchTimeout time.Duration
	// RecordTimeout bounds the write that records a failure, including during
	// shutdown.
	RecordTimeout time.Duration
	// Concurrency is how many fills this process runs at once. Small on purpose
	// in both callers: these start inside page requests and draw on a quota the
	// whole application shares.
	Concurrency int
}

// Leases is the caller's two lease statements.
//
// Only two, and neither of them is the one that stores the answer: this package
// never learns what a caller's rows look like. Claim must be a single statement
// whose RETURNING is empty for the loser — a read followed by a write is not a
// lease, and neither is catching a uniqueness violation in Go.
type Leases interface {
	// Claim takes the lease, reporting whether this caller won it. leaseCutoff is
	// now minus LeaseTTL; a row already 'fetching' may be reclaimed only if its
	// attempted_at is strictly older than that.
	Claim(ctx context.Context, q store.Querier, id string, now, leaseCutoff time.Time) (bool, error)
	// Fail records that the last attempt did not produce an answer. It must touch
	// neither the stored rows nor their fetched_at: a timeout today is no reason
	// to throw away an answer that was correct last month.
	Fail(ctx context.Context, q store.Querier, id string, at time.Time, reason string) error
}

// Fill does one entity's caller-specific work: read it from upstream and store
// it. Anything it returns is recorded as a failed attempt, with no exceptions —
// in particular a truncation error that arrives *with* usable-looking data is
// still an error, and its partial payload must never reach a delete-absent
// replace.
//
// It receives a context bounded by FetchTimeout and descended from the Gate's
// own, not from any request: the request that triggered this has already been
// answered.
type Fill func(ctx context.Context, id string) error

// Deps is everything the Gate needs that is not timing.
type Deps struct {
	Leases Leases
	Fill   Fill
	// DB hands over a Querier for the failure write, which happens outside any
	// request and therefore has no querier of its own to borrow.
	DB func() store.Querier
	// Subject names what is being filled, for log messages: "album", "artist".
	Subject string
	Logger  *slog.Logger
	// Now is the clock. Tests replace it; production leaves it nil.
	Now func() time.Time
}

// Gate decides whether and when to fill, and owns the fills it starts.
type Gate struct {
	leases  Leases
	fill    Fill
	db      func() store.Querier
	subject string
	log     *slog.Logger
	now     func() time.Time
	p       Policy
	slots   chan struct{}
	// base is the parent of every detached fill, so Close can end them all. It is
	// a context in a struct on purpose: these fills outlive the request that
	// started them, so there is no incoming context for them to inherit.
	base   context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	// mu guards closing, which is the only thing that may not race wg.Add against
	// wg.Wait. See track.
	mu      sync.Mutex
	closing bool
}

// New validates the policy and builds the Gate.
//
// The caller owns Close and must call it during shutdown, before closing the
// database pool: fills run detached from any request, so nothing else will ever
// wait for them, and a fill cancelled at shutdown still needs the pool to record
// that it failed.
func New(p Policy, deps Deps) (*Gate, error) {
	switch {
	case deps.Leases == nil:
		return nil, errors.New("lazyfetch: a lease repository is required")
	case deps.Fill == nil:
		return nil, errors.New("lazyfetch: a fill function is required")
	case deps.DB == nil:
		return nil, errors.New("lazyfetch: a database handle is required to record failures")
	case p.TTL <= 0:
		return nil, errors.New("lazyfetch: a positive TTL is required")
	case p.LeaseTTL <= 0:
		return nil, errors.New("lazyfetch: a positive lease TTL is required")
	case p.FailedRetryAfter <= 0:
		return nil, errors.New("lazyfetch: a positive failure backoff is required")
	case p.FetchTimeout <= 0:
		return nil, errors.New("lazyfetch: a positive fetch timeout is required")
	case p.RecordTimeout <= 0:
		return nil, errors.New("lazyfetch: a positive record timeout is required")
	case p.Concurrency <= 0:
		return nil, errors.New("lazyfetch: a positive concurrency is required")
	case p.LeaseTTL <= p.FetchTimeout:
		// Checked here because it was a comment in each caller and enforced by
		// nobody. A fill that can outlive its own lease lets a second replica
		// reclaim the entity and start a duplicate walk against a shared quota,
		// and the two then race to replace the same rows.
		return nil, fmt.Errorf("lazyfetch: the lease (%s) must outlast the fetch timeout (%s), "+
			"or a live fill loses its own lease", p.LeaseTTL, p.FetchTimeout)
	}
	lg := deps.Logger
	if lg == nil {
		lg = slog.Default()
	}
	now := deps.Now
	if now == nil {
		now = time.Now
	}
	base, cancel := context.WithCancel(context.Background())
	return &Gate{
		leases:  deps.Leases,
		fill:    deps.Fill,
		db:      deps.DB,
		subject: deps.Subject,
		log:     lg,
		now:     now,
		p:       p,
		slots:   make(chan struct{}, p.Concurrency),
		base:    base,
		cancel:  cancel,
	}, nil
}

// Now is the Gate's clock, so a caller timestamping its own writes uses the same
// one the schedule is computed against.
//
// No caller needs it yet: albumtracks holds its own clock from before the
// extraction. It is here so the next caller has one obvious clock to reach for
// rather than a second one that can disagree with the schedule by a test's worth
// of drift.
func (g *Gate) Now() time.Time { return g.now() }

// Resolve decides what a page can say about one entity, and starts a fill when
// one is due.
//
// stored is the caller's own answer to "do I already hold something worth
// showing". It is the caller's because only the caller knows what its rows mean:
// one caller has an answer whenever it has rows, and another can have a
// perfectly good answer with no countable rows at all.
//
// It never blocks on the upstream and it never fails because the upstream did: a
// third-party outage is a state the page renders, not a 500 it shows. That is
// why it returns an Outcome rather than an error.
func (g *Gate) Resolve(ctx context.Context, q store.Querier, id string, st State, stored bool) Outcome {
	now := g.now()
	// A live lease means somebody is filling this entity right now. Checking it
	// before deciding anything is what keeps a polling browser from attempting a
	// write on every tick. It is checked even when this instance has fetching
	// turned off: another replica may have started one before the switch was
	// flipped, and reporting that accurately costs nothing.
	pending := st.Status == StatusFetching && now.Sub(st.AttemptedAt) < g.p.LeaseTTL

	// p.Enabled is checked *before* due, and that order is load-bearing: a
	// switched-off instance must not resume making requests the moment its cache
	// expires. Guarding here rather than inside start also means the claim — a
	// write — is never even attempted, which is what an operator asking for no
	// unattended traffic actually asked for.
	if !pending && g.p.Enabled && g.due(st, now) {
		g.start(ctx, q, id, now)
		// Pending regardless of what start managed to do, and that is not
		// optimism: not one of its decline paths records an outcome. Most — no
		// free slot, a lease somebody else holds, a claim that errored — leave the
		// row untouched, so the entity is still due and the next view starts it.
		// The one that does not is a claim won and then abandoned at shutdown,
		// which leaves the row 'fetching' and resolves when that lease expires.
		// Both are "not yet". Reporting either as unavailable would blame the
		// upstream for a local condition and, worse, would tell a page whose job
		// is to stop polling on unavailable to give up on an answer that was still
		// coming.
		pending = true
	}

	switch {
	case stored:
		// An answer read successfully once is worth showing while a refresh runs
		// behind it — and worth showing when no refresh is coming at all, because
		// turning off fetching is not the same as forgetting what is on disk.
		// Withholding it would replace a true answer that is old with no answer.
		// The caller reports when it was read, which is the only honesty this case
		// needs: a date claims nothing about freshness.
		return OutcomeReady
	case pending:
		return OutcomePending
	case !g.p.Enabled:
		// Nothing stored, and this instance will not go and find out. That is the
		// operator's decision, not an upstream failure, and the page says so in its
		// own words rather than reporting an outage that never happened.
		return OutcomeDisabled
	default:
		// Nothing stored, nothing running, and nothing due: the last attempt failed
		// and its backoff has not elapsed.
		return OutcomeUnavailable
	}
}

// due reports whether a fill should be started now.
//
// It deliberately knows nothing about p.Enabled. Whether this instance fetches
// at all is a different question from whether this answer is old, and Resolve
// asks them in that order.
func (g *Gate) due(st State, now time.Time) bool {
	switch st.Status {
	case "":
		// Never attempted: the lazy first fill.
		return true
	case StatusOK:
		return now.Sub(st.FetchedAt) >= g.p.TTL
	case StatusFailed:
		// Much sooner than the TTL, and deliberately so — see FailedRetryAfter.
		return now.Sub(st.AttemptedAt) >= g.p.FailedRetryAfter
	case StatusFetching:
		// The lease has expired: whatever process held it is gone.
		return now.Sub(st.AttemptedAt) >= g.p.LeaseTTL
	default:
		return false
	}
}

// start begins a detached fill if it can.
//
// It reports nothing, on purpose. Every way it can decline is a "not yet" rather
// than a "no", because none of them records an outcome: the ones that return
// before the claim leave the row untouched, so it is still due and the next view
// starts it, and the one that returns after a won claim leaves the row
// 'fetching', so it resolves when that lease expires.
func (g *Gate) start(ctx context.Context, q store.Querier, id string, now time.Time) {
	if g.base.Err() != nil {
		// Close has already been called. Claiming a lease this process is about to
		// abandon would strand the entity for the whole LeaseTTL for nothing. Not
		// the guard that makes Close safe — track does that, atomically — just the
		// one that keeps the common case from being wasteful.
		return
	}
	select {
	case g.slots <- struct{}{}:
	default:
		// Every slot is busy. Refusing is the point: queueing here would be
		// queueing people behind a third party. Taken *before* the claim, so a
		// lease is never won that no slot can fill.
		g.log.Debug("fill not started; all slots busy", "subject", g.subject, "id", id)
		return
	}
	// The slot goes back unless the fill goroutine takes ownership of it.
	//
	// A defer rather than a release() on each path: a panic anywhere below — a nil
	// Querier reaching q.QueryRow would do it — would otherwise keep the slot for
	// ever, and Concurrency of those stop this process filling anything for the
	// rest of its life. Silently, because recovery middleware keeps serving pages.
	started := false
	defer func() {
		if !started {
			<-g.slots
		}
	}()

	claimed, err := g.leases.Claim(ctx, q, id, now, now.Add(-g.p.LeaseTTL))
	if err != nil {
		g.log.Warn("could not claim a fill", "subject", g.subject, "id", id, logging.Err(err))
		return
	}
	if !claimed {
		// Somebody else holds the lease. A second fill would be wasted requests
		// against a quota the whole application shares — but a fill *is* running,
		// so the page is right to keep polling.
		return
	}

	if !g.track() {
		// Close began between the claim and here. Nothing is stranded that the
		// lease does not already cover: this is the same state a process killed
		// mid-fill leaves behind, and LeaseTTL exists for exactly that.
		g.log.Debug("fill abandoned; shutting down", "subject", g.subject, "id", id)
		return
	}
	// Nothing may go between these two statements. track has already done
	// wg.Add(1), and only the goroutine below calls the matching Done — so
	// anything inserted here that can panic leaves the WaitGroup one short and
	// Close waits on it for ever, which is a worse failure than the slot leak the
	// defer above exists to prevent. Whatever a future change needs to do, it
	// belongs before track or inside the goroutine.
	started = true
	go func() {
		defer g.wg.Done()
		defer func() { <-g.slots }()
		g.run(id)
	}()
}

// track registers a fill with the WaitGroup unless Close has begun, reporting
// whether the caller may now spawn it.
//
// The mutex is what makes Close correct rather than merely likely. Without it, a
// handler still inside Resolve when the process shuts down races Close two ways:
// commonly Wait sees zero, returns, and the goroutine started a moment later is
// one nothing waits for — which is precisely what Close promises not to allow;
// and rarely the Add raising the counter from zero with a waiter already
// registered panics with "WaitGroup misuse: Add called concurrently with Wait",
// taking the process down on its way out. Both are reachable because
// http.Server.Shutdown returns on timeout with handlers still running.
func (g *Gate) track() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closing {
		return false
	}
	g.wg.Add(1)
	return true
}

// run performs one fill on a context of its own and records whatever it returns.
//
// Deliberately not the request's context: that request has already been
// answered, and cancelling when the browser navigated away would mean the answer
// never arrives however many times the page is opened. FetchTimeout bounds it
// instead, and Close cancels it at shutdown.
func (g *Gate) run(id string) {
	ctx, cancel := context.WithTimeout(g.base, g.p.FetchTimeout)
	defer cancel()

	if err := g.fill(ctx, id); err != nil {
		g.record(id, err)
		return
	}
	g.log.Debug("filled", "subject", g.subject, "id", id)
}

// record writes a failure on its own context, so a fill cancelled at shutdown
// still leaves the entity out of 'fetching' rather than waiting out the lease.
//
// The reason is handed over whole. The caller's Fail bounds it with
// store.Truncate, which cuts on a rune boundary; truncating again here would
// only risk two ellipses and a message cut twice.
func (g *Gate) record(id string, cause error) {
	g.log.Warn("could not fill", "subject", g.subject, "id", id, logging.Err(cause))

	ctx, cancel := context.WithTimeout(context.WithoutCancel(g.base), g.p.RecordTimeout)
	defer cancel()
	if err := g.leases.Fail(ctx, g.db(), id, g.now(), cause.Error()); err != nil {
		// Nothing further can be done, and nothing is stuck: the lease expires on
		// its own, so the next page view after LeaseTTL tries again.
		g.log.Error("could not record a failure", "subject", g.subject, "id", id, logging.Err(err))
	}
}

// Close cancels every fill in flight and waits for them. It is safe to call more
// than once, and safe to call while requests are still in Resolve.
//
// Bounded by RecordTimeout per fill, because a cancelled fill still records its
// failure — which is why the composition root must call this *before* it closes
// the database pool. That write goes out on a context of its own precisely so
// shutdown does not lose it, and a closed pool would lose it anyway.
func (g *Gate) Close() {
	// Before the Wait, necessarily: that is what stops a wg.Add landing against a
	// registered waiter. Before the cancel too, which Close does not need but
	// TestCloseRefusesNewFetchesBeforeItWaits does — it takes base.Done() as the
	// signal that Close has begun, and that is only sound while this comes first.
	// Moving it below the cancel leaves Close correct and quietly turns that test
	// into one that cannot fail; TestCloseSetsClosingBeforeItCancels in
	// ordering_test.go exists because that is invisible to every other test here.
	g.mu.Lock()
	g.closing = true
	g.mu.Unlock()

	g.cancel()
	// Safe now: no wg.Add can follow, because track takes the same mutex and sees
	// closing.
	g.wg.Wait()
}
