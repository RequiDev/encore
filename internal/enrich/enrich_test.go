package enrich

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/requi/encore/internal/config"
	"github.com/requi/encore/internal/domain"
	"github.com/requi/encore/internal/spotify"
	"github.com/requi/encore/internal/stats"
	"github.com/requi/encore/internal/store"
	"github.com/requi/encore/internal/store/catalog"
	"github.com/requi/encore/internal/store/listens"
)

// testNow is a fixed instant so scheduling assertions are exact.
var testNow = time.Date(2026, time.July, 26, 12, 0, 0, 0, time.UTC)

// testWorker builds a Worker without any collaborator that needs a database.
// Only the loop mechanics, the scheduling and the queue behaviour are exercised
// through it; anything that touches the store belongs in the integration suite.
func testWorker() *Worker {
	return &Worker{
		cfg:  withDefaults(config.Enrich{}),
		log:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		stat: NopMetrics{},
		now:  func() time.Time { return testNow },
		// Zero jitter keeps every backoff in this file instantaneous.
		rand:   func() float64 { return 0 },
		relink: make(chan relinkJob, 4),
	}
}

// A missing collaborator must be a construction error rather than a nil
// dereference on the first claim, hours after the process started.
func TestNewRequiresItsCollaborators(t *testing.T) {
	full := func() Deps {
		db := &store.Store{}
		return Deps{
			Store:   db,
			Catalog: catalog.New(db),
			Listens: listens.New(db),
			Stats:   stats.New(db),
			Spotify: spotify.NewClient(config.Spotify{}, slog.New(slog.NewTextHandler(io.Discard, nil))),
		}
	}

	cases := map[string]func(*Deps){
		"no store":   func(d *Deps) { d.Store = nil },
		"no catalog": func(d *Deps) { d.Catalog = nil },
		"no listens": func(d *Deps) { d.Listens = nil },
		"no stats":   func(d *Deps) { d.Stats = nil },
		"no spotify": func(d *Deps) { d.Spotify = nil },
	}
	for name, omit := range cases {
		t.Run(name, func(t *testing.T) {
			deps := full()
			omit(&deps)
			if _, err := New(config.Enrich{}, deps); err == nil {
				t.Fatalf("New with %s should fail", name)
			}
		})
	}

	w, err := New(config.Enrich{}, full())
	if err != nil {
		t.Fatalf("New with every dependency: %v", err)
	}
	// The optional collaborators are filled in rather than left nil, so nothing
	// on a loop's hot path has to check them.
	if w.stat == nil || w.now == nil || w.rand == nil || w.log == nil || w.aliasRate == nil || w.relink == nil {
		t.Errorf("New left a defaulted field unset: %+v", w)
	}
	if w.cfg.Interval <= 0 {
		t.Errorf("New did not apply the interval defaults: %+v", w.cfg)
	}
}

func TestWithDefaultsFillsZeroes(t *testing.T) {
	cfg := withDefaults(config.Enrich{})
	if cfg.Interval <= 0 || cfg.RepairInterval <= 0 || cfg.RollupInterval <= 0 {
		t.Fatalf("intervals left at zero: %+v", cfg)
	}
	if cfg.BatchSize != spotify.MaxTrackIDsPerRequest {
		t.Errorf("BatchSize = %d, want %d", cfg.BatchSize, spotify.MaxTrackIDsPerRequest)
	}
	if cfg.AliasRate <= 0 {
		t.Errorf("AliasRate = %v, want a positive rate", cfg.AliasRate)
	}

	// A configured value is never overridden.
	cfg = withDefaults(config.Enrich{Interval: time.Minute, BatchSize: 10, AliasRate: 0.5})
	if cfg.Interval != time.Minute || cfg.BatchSize != 10 || cfg.AliasRate != 0.5 {
		t.Errorf("configured values were overwritten: %+v", cfg)
	}
}

// Spotify's per-endpoint limits are the ceiling, and the configured batch size
// may only lower them: asking for fifty albums is a 400 for the whole batch.
func TestBatchSizeRespectsSpotifyLimits(t *testing.T) {
	cfg := withDefaults(config.Enrich{})
	cases := map[catalog.Kind]int{
		catalog.KindTrack:  spotify.MaxTrackIDsPerRequest,
		catalog.KindAlbum:  spotify.MaxAlbumIDsPerRequest,
		catalog.KindArtist: spotify.MaxArtistIDsPerRequest,
	}
	for kind, want := range cases {
		if got := batchSize(cfg, kind); got != want {
			t.Errorf("batchSize(%s) = %d, want %d", kind, got, want)
		}
	}

	lowered := withDefaults(config.Enrich{BatchSize: 5})
	for kind := range cases {
		if got := batchSize(lowered, kind); got != 5 {
			t.Errorf("batchSize(%s) with a batch size of 5 = %d", kind, got)
		}
	}

	// A batch size above Spotify's own limit is clamped, not honoured.
	raised := config.Enrich{BatchSize: 500}
	if got := batchSize(raised, catalog.KindAlbum); got != spotify.MaxAlbumIDsPerRequest {
		t.Errorf("batchSize(album) = %d, want it clamped to %d", got, spotify.MaxAlbumIDsPerRequest)
	}
}

func TestMissingIDs(t *testing.T) {
	cases := []struct {
		name      string
		requested []string
		got       []string
		want      []string
	}{
		{"nothing requested", nil, []string{"a"}, nil},
		{"all answered", []string{"a", "b"}, []string{"b", "a"}, nil},
		{"none answered", []string{"a", "b"}, nil, []string{"a", "b"}},
		{"one null in the array", []string{"a", "b", "c"}, []string{"a", "c"}, []string{"b"}},
		{"unrequested extras ignored", []string{"a"}, []string{"a", "z"}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := missingIDs(c.requested, c.got)
			if len(got) != len(c.want) {
				t.Fatalf("missingIDs(%v, %v) = %v, want %v", c.requested, c.got, got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Fatalf("missingIDs(%v, %v) = %v, want %v", c.requested, c.got, got, c.want)
				}
			}
		})
	}
}

func TestGroupByUser(t *testing.T) {
	a := uuid.New()
	b := uuid.New()
	rows := []listens.UnresolvedListen{
		{ID: 1, UserID: a},
		{ID: 2, UserID: b},
		{ID: 3, UserID: a},
	}

	groups := groupByUser(rows)
	if len(groups) != 2 {
		t.Fatalf("groupByUser returned %d groups, want 2", len(groups))
	}
	// First appearance decides the order, so a page is walked the same way twice.
	if groups[0].userID != a || groups[1].userID != b {
		t.Fatalf("groups are out of order: %v, %v", groups[0].userID, groups[1].userID)
	}
	if len(groups[0].rows) != 2 || groups[0].rows[0].ID != 1 || groups[0].rows[1].ID != 3 {
		t.Errorf("first group = %+v, want listens 1 and 3", groups[0].rows)
	}
	if len(groups[1].rows) != 1 || groups[1].rows[0].ID != 2 {
		t.Errorf("second group = %+v, want listen 2", groups[1].rows)
	}
	if groupByUser(nil) != nil {
		t.Error("groupByUser(nil) should return nothing")
	}
}

func TestJitterDelayOnlyAddsTime(t *testing.T) {
	const d = time.Minute
	if got := jitterDelay(d, attemptJitter, func() float64 { return 0 }); got != d {
		t.Errorf("jitterDelay with no draw = %v, want %v", got, d)
	}
	// The draw is exclusive of 1, so the upper bound is never quite reached; test
	// the boundary anyway, since it is the one that must stay bounded.
	high := jitterDelay(d, attemptJitter, func() float64 { return 1 })
	if want := d + time.Duration(attemptJitter*float64(d)); high != want {
		t.Errorf("jitterDelay with a full draw = %v, want %v", high, want)
	}
	if high <= d {
		t.Errorf("jitter should push the delay out, got %v", high)
	}
	if got := jitterDelay(0, attemptJitter, nil); got != 0 {
		t.Errorf("jitterDelay(0) = %v, want 0", got)
	}
	if got := jitterDelay(d, 0, nil); got != d {
		t.Errorf("jitterDelay with no jitter = %v, want %v", got, d)
	}
}

// The retry schedule is the domain's, not this package's: enrichment only adds
// jitter on top so that a batch which failed together does not come back in one
// synchronised burst.
func TestNextAttemptFollowsTheDomainBackoff(t *testing.T) {
	w := testWorker()
	for _, attempts := range []int32{1, 2, 6, domain.BackoffAttempts} {
		base := domain.NextMetadataAttempt(attempts)

		w.rand = func() float64 { return 0 }
		if got := w.nextAttempt(attempts); !got.Equal(testNow.Add(base)) {
			t.Errorf("nextAttempt(%d) = %v, want %v", attempts, got, testNow.Add(base))
		}

		w.rand = func() float64 { return 1 }
		latest := testNow.Add(base + time.Duration(attemptJitter*float64(base)))
		if got := w.nextAttempt(attempts); !got.Equal(latest) {
			t.Errorf("jittered nextAttempt(%d) = %v, want %v", attempts, got, latest)
		}
		if latest.Sub(testNow) > 2*base {
			t.Errorf("jitter for attempt %d more than doubled the delay", attempts)
		}
	}
}

// A 429 must be recognised so the worker leaves the pause to the client instead
// of stacking a second backoff on top of Spotify's own Retry-After.
func TestRateLimited(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"rate limited", &spotify.APIError{StatusCode: http.StatusTooManyRequests}, true},
		{"wrapped rate limit", errors.Join(errors.New("get tracks"),
			&spotify.APIError{StatusCode: http.StatusTooManyRequests}), true},
		{"server error", &spotify.APIError{StatusCode: http.StatusBadGateway}, false},
		{"not found", &spotify.APIError{StatusCode: http.StatusNotFound}, false},
		{"plain error", errors.New("connection reset"), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := rateLimited(c.err); got != c.want {
				t.Errorf("rateLimited(%v) = %v, want %v", c.err, got, c.want)
			}
		})
	}
}

// The lease has to outlast the time the claim takes to work through at the
// configured rate, or a second worker starts searching for pairs this one is
// already paying for.
func TestAliasLeaseCoversTheClaim(t *testing.T) {
	const batch = 25
	for _, rate := range []float64{0.5, 2, 10} {
		drain := time.Duration(float64(batch) / rate * float64(time.Second))
		got := aliasLease(rate, batch)
		if got <= drain {
			t.Errorf("aliasLease(%v, %d) = %v, which does not cover a drain of %v", rate, batch, got, drain)
		}
	}
	// A nonsensical rate must not produce a zero or negative lease, which the
	// repository would silently replace with its own default.
	if got := aliasLease(0, 0); got <= 0 {
		t.Errorf("aliasLease(0, 0) = %v, want a positive lease", got)
	}
}

func TestLoopBackoffIsBounded(t *testing.T) {
	for attempt := 1; attempt < 40; attempt++ {
		if got := loopBackoff.Jittered(attempt, func() float64 { return 1 }); got > loopBackoff.Max {
			t.Fatalf("backoff for attempt %d = %v, above the cap of %v", attempt, got, loopBackoff.Max)
		}
	}
}

// A step that found work runs again immediately, so a backlog drains at the rate
// Spotify allows rather than at the poll interval.
func TestLoopRunsBusyStepsBackToBack(t *testing.T) {
	w := testWorker()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	calls := 0
	w.loop(ctx, "test", time.Hour, func(context.Context) (bool, error) {
		calls++
		if calls == 3 {
			cancel()
		}
		return true, nil
	})
	if calls != 3 {
		t.Fatalf("step ran %d times, want 3 without waiting out the interval", calls)
	}
}

// A failing step must never end its loop: the conditions it waits out — Spotify
// down, the database restarting — resolve on their own, and the other loops are
// unaffected either way.
func TestLoopSurvivesFailingSteps(t *testing.T) {
	w := testWorker()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	calls := 0
	w.loop(ctx, "test", time.Millisecond, func(context.Context) (bool, error) {
		calls++
		if calls == 3 {
			cancel()
			return false, nil
		}
		return false, errors.New("spotify is unreachable")
	})
	if calls != 3 {
		t.Fatalf("step ran %d times, want the loop to have retried after each failure", calls)
	}
}

func TestLoopStopsWhenTheContextIsDone(t *testing.T) {
	w := testWorker()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	calls := 0
	w.loop(ctx, "test", time.Hour, func(context.Context) (bool, error) {
		calls++
		return true, nil
	})
	if calls != 0 {
		t.Fatalf("step ran %d times after cancellation, want 0", calls)
	}
}

func TestStepAdapters(t *testing.T) {
	busy, err := counted(func(context.Context) (int, error) { return 7, nil })(context.Background())
	if !busy || err != nil {
		t.Errorf("counted(7) = (%v, %v), want (true, nil)", busy, err)
	}
	busy, _ = counted(func(context.Context) (int, error) { return 0, nil })(context.Background())
	if busy {
		t.Error("counted(0) should report an idle step")
	}
	// A fixed-cadence step always waits out its interval, however much it did.
	busy, err = timed(func(context.Context) (int, error) { return 12, nil })(context.Background())
	if busy || err != nil {
		t.Errorf("timed(12) = (%v, %v), want (false, nil)", busy, err)
	}
}

func TestRunRelinkOnceIsIdleWithAnEmptyQueue(t *testing.T) {
	w := testWorker()
	busy, err := w.RunRelinkOnce(context.Background())
	if busy || err != nil {
		t.Fatalf("RunRelinkOnce on an empty queue = (%v, %v), want (false, nil)", busy, err)
	}
}

func TestQueueRelinkHandsWorkOver(t *testing.T) {
	w := testWorker()
	key := domain.AliasKeyFor("Sigur Rós", "Hoppípolla")
	w.queueRelink(context.Background(), key, "t1")

	select {
	case job := <-w.relink:
		if job.key != key || job.trackID != "t1" {
			t.Fatalf("queued job = %+v, want %v and t1", job, key)
		}
	default:
		t.Fatal("nothing was queued for the relink loop")
	}
}

// A cancelled context must not leave the resolver blocked on a full queue.
func TestQueueRelinkGivesUpOnCancellation(t *testing.T) {
	w := testWorker()
	for range cap(w.relink) {
		w.relink <- relinkJob{trackID: "filler"}
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		w.queueRelink(ctx, domain.AliasKeyFor("a", "b"), "t1")
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("queueRelink did not return after its context was cancelled")
	}
}

// Retries are bounded: a relink that keeps failing is eventually abandoned with
// a warning rather than cycling for ever. The alias stays resolved either way,
// so every listen imported afterwards is stored under the right identity.
func TestRequeueRelinkIsBounded(t *testing.T) {
	w := testWorker()
	job := relinkJob{key: domain.AliasKeyFor("a", "b"), trackID: "t1"}

	w.requeueRelink(job)
	select {
	case got := <-w.relink:
		if got.attempts != 1 {
			t.Fatalf("requeued attempts = %d, want 1", got.attempts)
		}
		job = got
	default:
		t.Fatal("a first failure should be requeued")
	}

	job.attempts = relinkAttempts - 1
	w.requeueRelink(job)
	select {
	case got := <-w.relink:
		t.Fatalf("job was requeued past its attempt budget: %+v", got)
	default:
	}
}

// A write that fails must not spend the catalogue rows' attempt budget: Spotify
// answered, and parking a healthy catalogue because Postgres was restarting is
// exactly the failure this distinction exists to prevent.
func TestStoreFailuresAreNotBlamedOnSpotify(t *testing.T) {
	var target *storeError

	if stored(nil) != nil {
		t.Fatal("stored(nil) should stay nil")
	}
	write := stored(errors.New("connection reset"))
	if !errors.As(write, &target) {
		t.Fatalf("stored(...) = %v, which is not recognisable as a write failure", write)
	}
	if write.Error() != "connection reset" {
		t.Errorf("stored(...).Error() = %q, want the original message", write.Error())
	}
	if !errors.Is(write, write.(*storeError).err) {
		t.Error("stored(...) does not unwrap to the error it carries")
	}

	fetch := error(&spotify.APIError{StatusCode: http.StatusBadGateway})
	if errors.As(fetch, &target) {
		t.Error("a fetch failure must not be taken for a write failure")
	}
	// Wrapping keeps the chain intact, so the classification survives the context
	// a caller adds to the message.
	if !errors.As(wrap("store tracks", write), &target) {
		t.Error("wrapping lost the write-failure classification")
	}
	if !rateLimited(wrap("get tracks", &spotify.APIError{StatusCode: http.StatusTooManyRequests})) {
		t.Error("wrapping lost the rate-limit classification")
	}
	if wrap("store tracks", nil) != nil {
		t.Error("wrap(nil) should stay nil")
	}
}

func TestNopMetricsAcceptEverything(t *testing.T) {
	var m Metrics = NopMetrics{}
	m.EnrichPending(catalog.KindTrack.String(), 3)
	m.EnrichResolved(kindAlias, 1)
	m.EnrichFailed(catalog.KindAlbum.String(), 2)
	m.EnrichRateLimited()
}
