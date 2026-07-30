package albumtracks

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/RequiDev/encore/internal/config"
	"github.com/RequiDev/encore/internal/spotify"
	"github.com/RequiDev/encore/internal/store"
	"github.com/RequiDev/encore/internal/store/catalog"
)

// storeQuerier is store.Querier under a shorter name, so the fake's signatures
// stay readable. It is the same type, so the fake satisfies Store exactly.
type storeQuerier = store.Querier

// fakeCatalog stands in for the two tables. It is deliberately not a database:
// these tests are about *when* a fetch is started, which is policy.
type fakeCatalog struct {
	mu      sync.Mutex
	state   catalog.AlbumTrackState
	tracks  []catalog.AlbumTrack
	claims  int
	writes  int
	fails   int
	claimOK bool
	// claimOnce models what the conditional write in album_track_fetches
	// actually does under contention: the first caller takes the lease and every
	// other one is refused until it expires. claimOK cannot express that, and a
	// concurrency test built on claimOK alone would be asserting nothing.
	claimOnce bool
	claimsWon int
	// lastReason is what the service asked to be stored in last_error.
	lastReason string
	// txSeq numbers the transactions inlineWriter has opened and curTx is the
	// one in force, or 0 outside any. replaceTx and markTx capture which one
	// each write ran in, which is the only way to tell "both inside the same
	// transaction" from "both happened" — the two are identical in final state.
	txSeq     int
	curTx     int
	replaceTx int
	markTx    int
}

func (f *fakeCatalog) enterTx() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.txSeq++
	f.curTx = f.txSeq
}

func (f *fakeCatalog) leaveTx() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.curTx = 0
}

// transactions reports which transaction each half of a successful fetch ran
// in. Zero means it ran outside one.
func (f *fakeCatalog) transactions() (replace, mark int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.replaceTx, f.markTx
}

func (f *fakeCatalog) AlbumTrackState(context.Context, storeQuerier, string) (catalog.AlbumTrackState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state, nil
}

func (f *fakeCatalog) AlbumTracks(context.Context, storeQuerier, string) ([]catalog.AlbumTrack, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]catalog.AlbumTrack(nil), f.tracks...), nil
}

func (f *fakeCatalog) ClaimAlbumTrackFetch(_ context.Context, _ storeQuerier, _ string, _, _ time.Time) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.claims++
	if f.claimOnce {
		if f.claimsWon > 0 {
			return false, nil
		}
		f.claimsWon++
		return true, nil
	}
	return f.claimOK, nil
}

func (f *fakeCatalog) ReplaceAlbumTracks(_ context.Context, _ storeQuerier, _ string, items []catalog.AlbumTrack) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writes++
	f.replaceTx = f.curTx
	f.tracks = append([]catalog.AlbumTrack(nil), items...)
	return nil
}

func (f *fakeCatalog) MarkAlbumTracksFetched(_ context.Context, _ storeQuerier, _ string, at time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.markTx = f.curTx
	f.state = catalog.AlbumTrackState{Status: catalog.AlbumTrackOK, FetchedAt: at, AttemptedAt: at}
	return nil
}

func (f *fakeCatalog) FailAlbumTrackFetch(_ context.Context, _ storeQuerier, _ string, at time.Time, reason string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fails++
	f.state.Status = catalog.AlbumTrackFailed
	f.state.AttemptedAt = at
	f.state.Attempts++
	f.lastReason = reason
	return nil
}

func (f *fakeCatalog) counts() (claims, writes, fails int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.claims, f.writes, f.fails
}

// stored is the listing as it stands, copied under the lock so an assertion
// after Close reads it the same way the fetch wrote it.
func (f *fakeCatalog) stored() []catalog.AlbumTrack {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]catalog.AlbumTrack(nil), f.tracks...)
}

func (f *fakeCatalog) reason() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastReason
}

// fakeFetcher answers with whatever the test set, and counts calls.
type fakeFetcher struct {
	mu     sync.Mutex
	tracks []spotify.AlbumTrack
	err    error
	calls  int
	block  chan struct{}
	// heedCtx makes the fake behave the way a real client does: it gives up when
	// its context ends, and reports the state of that context on return.
	//
	// Off by default, and that is not laziness. Close both cancels and waits, and
	// most tests here use it purely as a barrier — "the fetch has finished, now
	// assert". A fake that always noticed the cancellation would race the very
	// fetch those tests are waiting for and turn every one of them intermittent.
	// The two tests that are *about* which context the fetch got turn it on, and
	// synchronise with entered/ended so nothing is left to timing.
	heedCtx bool
	// entered is closed once the call has begun, so a test knows the fetch
	// goroutine has already derived its context. ended is closed as it returns,
	// so a test knows nothing further will read that context.
	entered     chan struct{}
	ended       chan struct{}
	enteredOnce sync.Once
	endedOnce   sync.Once
	// sawCtxErr is the state of the fetch's context at the moment the call
	// returned, which is how a test sees *which* context it was given.
	sawCtxErr error
}

func (f *fakeFetcher) AlbumTracks(ctx context.Context, _ string, _ int) ([]spotify.AlbumTrack, error) {
	f.mu.Lock()
	f.calls++
	block, heed := f.block, f.heedCtx
	entered, ended := f.entered, f.ended
	tracks, err := f.tracks, f.err
	f.mu.Unlock()

	if entered != nil {
		f.enteredOnce.Do(func() { close(entered) })
	}
	if ended != nil {
		defer f.endedOnce.Do(func() { close(ended) })
	}

	if block != nil {
		if heed {
			select {
			case <-block:
			case <-ctx.Done():
			}
		} else {
			<-block
		}
	}
	if !heed {
		return tracks, err
	}
	f.mu.Lock()
	f.sawCtxErr = ctx.Err()
	f.mu.Unlock()
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, fmt.Errorf("spotify: album tracks: %w", ctxErr)
	}
	return tracks, err
}

func (f *fakeFetcher) called() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeFetcher) ctxErr() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sawCtxErr
}

// inlineWriter runs the transaction body straight through, with no pool behind
// it. The Querier it hands over is nil, which is exactly right here: the fake
// catalogue ignores it, and these tests are about the *shape* of the write —
// that the replace and the mark happen inside one InTx — not its SQL. It tells
// the catalogue when a transaction opens and closes so that shape is
// observable.
type inlineWriter struct{ cat *fakeCatalog }

func (w inlineWriter) InTx(ctx context.Context, fn func(ctx context.Context, q store.Querier) error) error {
	w.cat.enterTx()
	defer w.cat.leaveTx()
	return fn(ctx, nil)
}

func (w inlineWriter) DB() store.Querier { return nil }

func discard() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// newServiceWith builds a service on the given configuration. Close is
// deferred by the caller or by Cleanup; the fetch runs in a goroutine, so every
// assertion about it comes after an explicit s.Close().
func newServiceWith(t *testing.T, cfg config.AlbumTracks, cat *fakeCatalog, fetch *fakeFetcher, now time.Time) *Service {
	t.Helper()
	s, err := New(cfg, Deps{
		Catalog: cat,
		Spotify: fetch,
		Writer:  inlineWriter{cat: cat},
		Logger:  discard(),
		Now:     func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(s.Close)
	return s
}

// newService is the ordinary, enabled service.
func newService(t *testing.T, cat *fakeCatalog, fetch *fakeFetcher, now time.Time) *Service {
	t.Helper()
	return newServiceWith(t, config.AlbumTracks{Enabled: true, TTL: 30 * 24 * time.Hour}, cat, fetch, now)
}

// newDisabledService is an instance whose operator turned fetching off.
func newDisabledService(t *testing.T, cat *fakeCatalog, fetch *fakeFetcher, now time.Time) *Service {
	t.Helper()
	return newServiceWith(t, config.AlbumTracks{Enabled: false, TTL: 30 * 24 * time.Hour}, cat, fetch, now)
}

func TestFirstViewStartsTheFetchAndReportsPending(t *testing.T) {
	cat := &fakeCatalog{claimOK: true}
	fetch := &fakeFetcher{block: make(chan struct{})}
	now := time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC)
	s := newService(t, cat, fetch, now)

	got, err := s.Listing(context.Background(), nil, "album000000000000000001")
	if err != nil {
		t.Fatalf("Listing: %v", err)
	}
	if got.State != StatePending {
		t.Fatalf("state = %q, want %q: nothing is stored and a fetch has begun", got.State, StatePending)
	}
	if len(got.Tracks) != 0 {
		t.Fatalf("got %d tracks before any fetch finished", len(got.Tracks))
	}
	close(fetch.block)
	s.Close()
	if n := fetch.called(); n != 1 {
		t.Fatalf("fetcher called %d times, want 1", n)
	}
}

// TestListingDoesNotWaitForSpotify is the whole point of the design: a page
// request must answer while the fetch is still running.
func TestListingDoesNotWaitForSpotify(t *testing.T) {
	cat := &fakeCatalog{claimOK: true}
	block := make(chan struct{})
	fetch := &fakeFetcher{block: block}
	now := time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC)
	s := newService(t, cat, fetch, now)

	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := s.Listing(context.Background(), nil, "album000000000000000001"); err != nil {
			t.Errorf("Listing: %v", err)
		}
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Listing blocked on the Spotify call; the album page would hang")
	}
	close(block)
}

func TestAFreshListingIsNotRefetched(t *testing.T) {
	now := time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC)
	cat := &fakeCatalog{
		claimOK: true,
		state: catalog.AlbumTrackState{
			Status:      catalog.AlbumTrackOK,
			FetchedAt:   now.Add(-29 * 24 * time.Hour),
			AttemptedAt: now.Add(-29 * 24 * time.Hour),
		},
		tracks: []catalog.AlbumTrack{{TrackID: "t1", Name: "One", DiscNumber: 1, TrackNumber: 1}},
	}
	fetch := &fakeFetcher{}
	s := newService(t, cat, fetch, now)

	got, err := s.Listing(context.Background(), nil, "album000000000000000001")
	if err != nil {
		t.Fatalf("Listing: %v", err)
	}
	if got.State != StateReady {
		t.Fatalf("state = %q, want %q", got.State, StateReady)
	}
	s.Close()
	if n := fetch.called(); n != 0 {
		t.Fatalf("fetcher called %d times for a 29-day-old listing under a 30-day TTL, want 0", n)
	}
}

// TestAnExpiredListingIsRefetchedAndStillServed pins both halves of the TTL: it
// refetches, and it does not withhold the listing it already has while doing so.
func TestAnExpiredListingIsRefetchedAndStillServed(t *testing.T) {
	now := time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC)
	cat := &fakeCatalog{
		claimOK: true,
		state: catalog.AlbumTrackState{
			Status:      catalog.AlbumTrackOK,
			FetchedAt:   now.Add(-31 * 24 * time.Hour),
			AttemptedAt: now.Add(-31 * 24 * time.Hour),
		},
		tracks: []catalog.AlbumTrack{{TrackID: "t1", Name: "One", DiscNumber: 1, TrackNumber: 1}},
	}
	fetch := &fakeFetcher{tracks: []spotify.AlbumTrack{
		{ID: "t1", Name: "One", DiscNumber: 1, TrackNumber: 1},
		{ID: "t2", Name: "Two", DiscNumber: 1, TrackNumber: 2},
	}}
	s := newService(t, cat, fetch, now)

	got, err := s.Listing(context.Background(), nil, "album000000000000000001")
	if err != nil {
		t.Fatalf("Listing: %v", err)
	}
	if got.State != StateReady || len(got.Tracks) != 1 {
		t.Fatalf("state/tracks = %q/%d, want %q with the stored listing still served",
			got.State, len(got.Tracks), StateReady)
	}
	s.Close()
	if n := fetch.called(); n != 1 {
		t.Fatalf("fetcher called %d times for a 31-day-old listing under a 30-day TTL, want 1", n)
	}
	if _, writes, _ := cat.counts(); writes != 1 {
		t.Fatalf("wrote %d times, want 1", writes)
	}
}

// TestTheListingAndItsStatusCommitTogether is the crash-window guard.
//
// The repository offers no combined call and cannot enforce this: it is the
// service that must put ReplaceAlbumTracks and MarkAlbumTracksFetched inside
// one InTx. Split across two transactions, a crash between them leaves either
// a listing nothing marks authoritative, or an 'ok' beside a listing an
// interrupted replace never finished writing.
//
// Final state cannot show the difference — both orders end with the same rows
// and the same status — so this asserts the *transaction* each write ran in.
func TestTheListingAndItsStatusCommitTogether(t *testing.T) {
	now := time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC)
	cat := &fakeCatalog{claimOK: true}
	fetch := &fakeFetcher{tracks: []spotify.AlbumTrack{
		{ID: "t1", Name: "One", DiscNumber: 1, TrackNumber: 1},
	}}
	s := newService(t, cat, fetch, now)

	if _, err := s.Listing(context.Background(), nil, "album000000000000000001"); err != nil {
		t.Fatalf("Listing: %v", err)
	}
	s.Close()

	replace, mark := cat.transactions()
	if replace == 0 {
		t.Fatal("ReplaceAlbumTracks ran outside a transaction")
	}
	if mark == 0 {
		t.Fatal("MarkAlbumTracksFetched ran outside a transaction; a crash after the rows " +
			"landed leaves a listing with nothing marking it authoritative")
	}
	if replace != mark {
		t.Fatalf("the listing was written in transaction %d and its status in %d; a crash "+
			"between them leaves the two tables disagreeing", replace, mark)
	}
}

// TestATruncatedFetchWritesNothing is the delete-absent guard. The partial
// listing is real, and writing it would delete the tail of a correct one.
func TestATruncatedFetchWritesNothing(t *testing.T) {
	now := time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC)
	before := []catalog.AlbumTrack{
		{TrackID: "t1", Name: "One", DiscNumber: 1, TrackNumber: 1},
		{TrackID: "t2", Name: "Two", DiscNumber: 1, TrackNumber: 2},
	}
	cat := &fakeCatalog{
		claimOK: true,
		state: catalog.AlbumTrackState{
			Status: catalog.AlbumTrackOK, FetchedAt: now.Add(-31 * 24 * time.Hour),
			AttemptedAt: now.Add(-31 * 24 * time.Hour),
		},
		tracks: append([]catalog.AlbumTrack(nil), before...),
	}
	// A partial listing *and* ErrTruncated, exactly as spotify.AlbumTracks
	// returns it.
	fetch := &fakeFetcher{
		tracks: []spotify.AlbumTrack{{ID: "t1", Name: "One", DiscNumber: 1, TrackNumber: 1}},
		err:    fmt.Errorf("spotify: album tracks: %w", spotify.ErrTruncated),
	}
	s := newService(t, cat, fetch, now)

	if _, err := s.Listing(context.Background(), nil, "album000000000000000001"); err != nil {
		t.Fatalf("Listing: %v", err)
	}
	s.Close()

	_, writes, fails := cat.counts()
	if writes != 0 {
		t.Fatalf("wrote %d times on a truncated fetch, want 0: the partial deleted the tail", writes)
	}
	if fails != 1 {
		t.Fatalf("recorded %d failures, want 1", fails)
	}
	// Not just the count: the whole listing, field for field. A replace from the
	// partial would leave exactly one of these two rows behind, and a length
	// check alone would pass a replace that rewrote both.
	if got := cat.stored(); !reflect.DeepEqual(got, before) {
		t.Fatalf("stored listing = %+v, want the %+v that was already correct", got, before)
	}
}

// TestAnEmptyListingIsAFailure keeps "Spotify will not show me this album" from
// being stored as "this album has no tracks", which the page would render as
// "you have played every track".
func TestAnEmptyListingIsAFailure(t *testing.T) {
	now := time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC)
	cat := &fakeCatalog{claimOK: true}
	fetch := &fakeFetcher{tracks: nil, err: nil} // a 200 with no items
	s := newService(t, cat, fetch, now)

	if _, err := s.Listing(context.Background(), nil, "album000000000000000001"); err != nil {
		t.Fatalf("Listing: %v", err)
	}
	s.Close()

	_, writes, fails := cat.counts()
	if writes != 0 {
		t.Fatalf("wrote %d times for an empty listing, want 0", writes)
	}
	if fails != 1 {
		t.Fatalf("recorded %d failures for an empty listing, want 1", fails)
	}
}

// TestAnEmptyListingDoesNotWipeAStoredOne is the same 200-with-no-items against
// an album that already has a good listing, which is the case that does damage.
//
// Leaning on ReplaceAlbumTracks' own refusal is not enough here: that returns an
// error, which this service records as a failed *attempt* — so the difference
// between guarding and not guarding would be invisible to a test that only
// counted failures. What must hold is that nothing is written at all.
func TestAnEmptyListingDoesNotWipeAStoredOne(t *testing.T) {
	now := time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC)
	before := []catalog.AlbumTrack{
		{TrackID: "t1", Name: "One", DiscNumber: 1, TrackNumber: 1},
		{TrackID: "t2", Name: "Two", DiscNumber: 1, TrackNumber: 2},
	}
	cat := &fakeCatalog{
		claimOK: true,
		state: catalog.AlbumTrackState{
			Status: catalog.AlbumTrackOK, FetchedAt: now.Add(-31 * 24 * time.Hour),
			AttemptedAt: now.Add(-31 * 24 * time.Hour),
		},
		tracks: append([]catalog.AlbumTrack(nil), before...),
	}
	fetch := &fakeFetcher{}
	s := newService(t, cat, fetch, now)

	if _, err := s.Listing(context.Background(), nil, "album000000000000000001"); err != nil {
		t.Fatalf("Listing: %v", err)
	}
	s.Close()

	if _, writes, fails := cat.counts(); writes != 0 || fails != 1 {
		t.Fatalf("writes=%d fails=%d for an empty listing over a stored one, want 0 and 1", writes, fails)
	}
	if got := cat.stored(); !reflect.DeepEqual(got, before) {
		t.Fatalf("stored listing = %+v, want the %+v that was already correct", got, before)
	}
}

// TestAFailedFetchIsNotRetriedImmediately keeps a broken upstream from turning
// every page view into another request against a quota it is already refusing.
func TestAFailedFetchIsNotRetriedImmediately(t *testing.T) {
	now := time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC)
	cat := &fakeCatalog{
		claimOK: true,
		state: catalog.AlbumTrackState{
			Status: catalog.AlbumTrackFailed, AttemptedAt: now.Add(-time.Minute), Attempts: 1,
		},
	}
	fetch := &fakeFetcher{}
	s := newService(t, cat, fetch, now)

	got, err := s.Listing(context.Background(), nil, "album000000000000000001")
	if err != nil {
		t.Fatalf("Listing: %v", err)
	}
	if got.State != StateUnavailable {
		t.Fatalf("state = %q, want %q: the fetch failed and the backoff has not elapsed",
			got.State, StateUnavailable)
	}
	s.Close()
	if n := fetch.called(); n != 0 {
		t.Fatalf("fetcher called %d times one minute after a failure, want 0", n)
	}
}

// TestAFailedFetchIsRetriedAfterTheBackoff is the other half: fifteen minutes
// later the page tries again, rather than waiting out the thirty-day TTL.
func TestAFailedFetchIsRetriedAfterTheBackoff(t *testing.T) {
	now := time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC)
	cat := &fakeCatalog{
		claimOK: true,
		state: catalog.AlbumTrackState{
			Status: catalog.AlbumTrackFailed, AttemptedAt: now.Add(-16 * time.Minute), Attempts: 1,
		},
	}
	fetch := &fakeFetcher{tracks: []spotify.AlbumTrack{{ID: "t1", Name: "One", DiscNumber: 1, TrackNumber: 1}}}
	s := newService(t, cat, fetch, now)

	got, err := s.Listing(context.Background(), nil, "album000000000000000001")
	if err != nil {
		t.Fatalf("Listing: %v", err)
	}
	if got.State != StatePending {
		t.Fatalf("state = %q, want %q", got.State, StatePending)
	}
	s.Close()
	if n := fetch.called(); n != 1 {
		t.Fatalf("fetcher called %d times sixteen minutes after a failure, want 1", n)
	}
}

// TestALostClaimStartsNoSecondFetch is the two-tabs case as this process sees
// it: the claim went to somebody else, so this one reports pending and stops.
func TestALostClaimStartsNoSecondFetch(t *testing.T) {
	now := time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC)
	cat := &fakeCatalog{claimOK: false}
	fetch := &fakeFetcher{}
	s := newService(t, cat, fetch, now)

	got, err := s.Listing(context.Background(), nil, "album000000000000000001")
	if err != nil {
		t.Fatalf("Listing: %v", err)
	}
	if got.State != StatePending {
		t.Fatalf("state = %q, want %q: somebody else is fetching it", got.State, StatePending)
	}
	s.Close()
	if n := fetch.called(); n != 0 {
		t.Fatalf("fetcher called %d times after losing the claim, want 0", n)
	}
}

// TestTwoConcurrentViewsProduceOneFetch is the two-tabs case with both tabs
// present, which is the shape the lease exists for.
//
// The fake claims the way the SQL does — first caller wins, everybody else is
// refused — so this asserts the service actually routes its decision through
// that answer rather than through anything it worked out for itself. Both
// callers reach the claim (concurrency is above two), so the claim count is
// deterministic and a service that never claimed at all would fail here too.
func TestTwoConcurrentViewsProduceOneFetch(t *testing.T) {
	now := time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC)
	cat := &fakeCatalog{claimOnce: true}
	block := make(chan struct{})
	fetch := &fakeFetcher{block: block, tracks: []spotify.AlbumTrack{
		{ID: "t1", Name: "One", DiscNumber: 1, TrackNumber: 1},
	}}
	s := newService(t, cat, fetch, now)

	const viewers = 2
	var wg sync.WaitGroup
	states := make([]State, viewers)
	for i := range viewers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := s.Listing(context.Background(), nil, "album000000000000000001")
			if err != nil {
				t.Errorf("Listing: %v", err)
				return
			}
			states[i] = got.State
		}()
	}
	wg.Wait()
	// Only now is the fetch allowed to finish, so both views overlapped it.
	close(block)
	s.Close()

	if n := fetch.called(); n != 1 {
		t.Fatalf("fetcher called %d times for %d concurrent views of one album, want 1", n, viewers)
	}
	if claims, _, _ := cat.counts(); claims != viewers {
		t.Fatalf("attempted %d claims for %d views, want %d: the decision did not go through the lease",
			claims, viewers, viewers)
	}
	for i, st := range states {
		if st != StatePending {
			t.Errorf("viewer %d saw %q, want %q: a fetch is running either way", i, st, StatePending)
		}
	}
}

// TestALiveLeaseIsNotEvenClaimed keeps the browser's poll from writing to the
// database twice a second for as long as a fetch is running.
func TestALiveLeaseIsNotEvenClaimed(t *testing.T) {
	now := time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC)
	cat := &fakeCatalog{
		claimOK: true,
		state: catalog.AlbumTrackState{
			Status: catalog.AlbumTrackFetching, AttemptedAt: now.Add(-10 * time.Second), Attempts: 1,
		},
	}
	fetch := &fakeFetcher{}
	s := newService(t, cat, fetch, now)

	got, err := s.Listing(context.Background(), nil, "album000000000000000001")
	if err != nil {
		t.Fatalf("Listing: %v", err)
	}
	if got.State != StatePending {
		t.Fatalf("state = %q, want %q", got.State, StatePending)
	}
	s.Close()
	claims, _, _ := cat.counts()
	if claims != 0 {
		t.Fatalf("attempted %d claims against a ten-second-old lease, want 0", claims)
	}
	if n := fetch.called(); n != 0 {
		t.Fatalf("fetcher called %d times against a live lease, want 0", n)
	}
}

func TestAnExpiredLeaseIsReclaimed(t *testing.T) {
	now := time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC)
	cat := &fakeCatalog{
		claimOK: true,
		state: catalog.AlbumTrackState{
			Status: catalog.AlbumTrackFetching, AttemptedAt: now.Add(-10 * time.Minute), Attempts: 1,
		},
	}
	fetch := &fakeFetcher{tracks: []spotify.AlbumTrack{{ID: "t1", Name: "One", DiscNumber: 1, TrackNumber: 1}}}
	s := newService(t, cat, fetch, now)

	if _, err := s.Listing(context.Background(), nil, "album000000000000000001"); err != nil {
		t.Fatalf("Listing: %v", err)
	}
	s.Close()
	if n := fetch.called(); n != 1 {
		t.Fatalf("fetcher called %d times against a ten-minute-old lease, want 1: it is stranded", n)
	}
}

func TestTheErrorFromSpotifyIsNeverReturnedToTheCaller(t *testing.T) {
	now := time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC)
	cat := &fakeCatalog{claimOK: true}
	fetch := &fakeFetcher{err: errors.New("spotify: album tracks: 502 bad gateway")}
	s := newService(t, cat, fetch, now)

	if _, err := s.Listing(context.Background(), nil, "album000000000000000001"); err != nil {
		t.Fatalf("Listing returned %v; a Spotify failure must not fail the page request", err)
	}
}

// TestTheFailureReasonIsRecorded is what an operator reads out of last_error
// when the panel stays empty. A failure recorded with no reason is a support
// question with nowhere to start.
func TestTheFailureReasonIsRecorded(t *testing.T) {
	now := time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC)
	cat := &fakeCatalog{claimOK: true}
	fetch := &fakeFetcher{err: errors.New("spotify: album tracks: 502 bad gateway")}
	s := newService(t, cat, fetch, now)

	if _, err := s.Listing(context.Background(), nil, "album000000000000000001"); err != nil {
		t.Fatalf("Listing: %v", err)
	}
	s.Close()
	if got := cat.reason(); got != "spotify: album tracks: 502 bad gateway" {
		t.Fatalf("last_error = %q, want the error from Spotify", got)
	}
}

// --- the detached fetch ----------------------------------------------------

// TestTheFetchOutlivesTheRequestContext is why start hands the walk a context of
// its own. The page request has already been answered by the time the fetch
// runs, so a browser that navigated away — or any client that hung up — must
// not take the listing with it. On the request's context that album would never
// acquire a listing however many times somebody opened it.
func TestTheFetchOutlivesTheRequestContext(t *testing.T) {
	now := time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC)
	cat := &fakeCatalog{claimOK: true}
	block := make(chan struct{})
	fetch := &fakeFetcher{
		block:   block,
		heedCtx: true,
		entered: make(chan struct{}),
		ended:   make(chan struct{}),
		tracks:  []spotify.AlbumTrack{{ID: "t1", Name: "One", DiscNumber: 1, TrackNumber: 1}},
	}
	s := newService(t, cat, fetch, now)

	ctx, cancel := context.WithCancel(context.Background())
	if _, err := s.Listing(ctx, nil, "album000000000000000001"); err != nil {
		t.Fatalf("Listing: %v", err)
	}
	// Wait until the fetch has begun, so its context is already derived from
	// whatever it was going to be derived from, and only then end the request.
	<-fetch.entered
	cancel()
	// Release it the legitimate way. A fetch on the request's context has
	// already ended by now, with its context in error; one on its own has not
	// noticed the cancellation at all, because nothing cancelled it.
	close(block)
	// Nothing reads the fetch's context after this, so Close cannot race it.
	<-fetch.ended
	s.Close()

	if err := fetch.ctxErr(); err != nil {
		t.Fatalf("the fetch saw its context %v after the *request* was cancelled: it was handed the "+
			"request's context, so navigating away loses the listing", err)
	}
	if _, writes, fails := cat.counts(); writes != 1 || fails != 0 {
		t.Fatalf("writes=%d fails=%d after the request was cancelled, want 1 and 0", writes, fails)
	}
}

// TestCloseEndsAFetchInFlight is the other side of detaching: a goroutine on a
// context nobody cancels is a leak, and a process that cannot shut down.
func TestCloseEndsAFetchInFlight(t *testing.T) {
	now := time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC)
	cat := &fakeCatalog{claimOK: true}
	block := make(chan struct{}) // deliberately never closed by the test body
	fetch := &fakeFetcher{block: block, heedCtx: true}
	s := newService(t, cat, fetch, now)

	// A rescue, registered after the service so it runs *before* Cleanup's
	// Close: if Close turns out not to cancel anything, the assertion below
	// reports that rather than hanging the whole package on a stuck Cleanup.
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(block) }) })

	if _, err := s.Listing(context.Background(), nil, "album000000000000000001"); err != nil {
		t.Fatalf("Listing: %v", err)
	}

	closed := make(chan struct{})
	go func() {
		defer close(closed)
		s.Close()
	}()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return while a fetch was in flight; the fetch is on a context " +
			"nothing cancels, so it outlives the service")
	}

	if err := fetch.ctxErr(); err == nil {
		t.Fatal("the fetch's context was still live when Close returned")
	}
	// A cancelled fetch still records its outcome, on a context of its own, so
	// the album is not left 'fetching' until the lease expires.
	if _, _, fails := cat.counts(); fails != 1 {
		t.Fatalf("recorded %d failures for a cancelled fetch, want 1", fails)
	}
}

// --- the constructor -------------------------------------------------------

func TestNewRejectsAnIncompleteConfiguration(t *testing.T) {
	ok := config.AlbumTracks{Enabled: true, TTL: time.Hour}
	full := func() Deps {
		cat := &fakeCatalog{}
		return Deps{Catalog: cat, Spotify: &fakeFetcher{}, Writer: inlineWriter{cat: cat}, Logger: discard()}
	}
	for name, tc := range map[string]struct {
		cfg    config.AlbumTracks
		mutate func(*Deps)
	}{
		"no catalog": {ok, func(d *Deps) { d.Catalog = nil }},
		"no spotify": {ok, func(d *Deps) { d.Spotify = nil }},
		"no writer":  {ok, func(d *Deps) { d.Writer = nil }},
		"zero ttl":   {config.AlbumTracks{Enabled: true}, func(*Deps) {}},
	} {
		t.Run(name, func(t *testing.T) {
			deps := full()
			tc.mutate(&deps)
			if _, err := New(tc.cfg, deps); err == nil {
				t.Fatal("New: want an error, got nil")
			}
		})
	}
}

// --- the operator's switch -------------------------------------------------

// TestDisabledMakesNoRequestAndClaimsNoLease is the switch doing the one thing
// an operator turned it off for. Both counters matter: a claim is a write to
// album_track_fetches, which an instance told to make no unattended requests
// should not be making either.
func TestDisabledMakesNoRequestAndClaimsNoLease(t *testing.T) {
	now := time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC)
	cat := &fakeCatalog{claimOK: true}
	fetch := &fakeFetcher{}
	s := newDisabledService(t, cat, fetch, now)

	got, err := s.Listing(context.Background(), nil, "album000000000000000001")
	if err != nil {
		t.Fatalf("Listing: %v", err)
	}
	if got.State != StateDisabled {
		t.Fatalf("state = %q, want %q", got.State, StateDisabled)
	}
	s.Close()
	if n := fetch.called(); n != 0 {
		t.Fatalf("fetcher called %d times on a disabled instance, want 0", n)
	}
	if claims, _, fails := cat.counts(); claims != 0 || fails != 0 {
		t.Fatalf("claims=%d fails=%d on a disabled instance, want 0 and 0", claims, fails)
	}
}

// TestDisabledIsNotUnavailable keeps an operator's choice from being reported as
// a Spotify failure. They are different facts and the page renders them
// differently; collapsing them makes Encore blame a third party for a local
// decision.
func TestDisabledIsNotUnavailable(t *testing.T) {
	now := time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC)
	s := newDisabledService(t, &fakeCatalog{claimOK: true}, &fakeFetcher{}, now)

	got, err := s.Listing(context.Background(), nil, "album000000000000000001")
	if err != nil {
		t.Fatalf("Listing: %v", err)
	}
	if got.State == StateUnavailable {
		t.Fatal("a disabled instance reported \"unavailable\"; the page would blame Spotify " +
			"for something the operator chose")
	}
	if got.State != StateDisabled {
		t.Fatalf("state = %q, want %q", got.State, StateDisabled)
	}
}

// TestDisabledStillServesACachedListing is the other half of "off": it stops
// fetching, it does not blind the page to what is already on disk.
func TestDisabledStillServesACachedListing(t *testing.T) {
	now := time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC)
	fetchedAt := now.Add(-3 * 24 * time.Hour)
	cat := &fakeCatalog{
		claimOK: true,
		state: catalog.AlbumTrackState{
			Status: catalog.AlbumTrackOK, FetchedAt: fetchedAt, AttemptedAt: fetchedAt,
		},
		tracks: []catalog.AlbumTrack{
			{TrackID: "t1", Name: "One", DiscNumber: 1, TrackNumber: 1},
			{TrackID: "t2", Name: "Two", DiscNumber: 1, TrackNumber: 2},
		},
	}
	fetch := &fakeFetcher{}
	s := newDisabledService(t, cat, fetch, now)

	got, err := s.Listing(context.Background(), nil, "album000000000000000001")
	if err != nil {
		t.Fatalf("Listing: %v", err)
	}
	if got.State != StateReady {
		t.Fatalf("state = %q, want %q: a stored listing is still a listing", got.State, StateReady)
	}
	if len(got.Tracks) != 2 {
		t.Fatalf("got %d tracks, want the 2 already on disk", len(got.Tracks))
	}
	if !got.FetchedAt.Equal(fetchedAt) {
		t.Fatalf("fetchedAt = %v, want %v: the page cannot say how old the listing is",
			got.FetchedAt, fetchedAt)
	}
	s.Close()
	if n := fetch.called(); n != 0 {
		t.Fatalf("fetcher called %d times on a disabled instance, want 0", n)
	}
}

// TestDisabledServesAStaleListingWithoutRefreshing is the case the plan rules on
// explicitly: past the TTL, with the switch off. The listing is served as it
// stands, with its date, and nothing is fetched. Withholding it would be
// strictly worse — the operator turned off fetching, not the album page.
func TestDisabledServesAStaleListingWithoutRefreshing(t *testing.T) {
	now := time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC)
	fetchedAt := now.Add(-400 * 24 * time.Hour) // far past the thirty-day TTL
	cat := &fakeCatalog{
		claimOK: true,
		state: catalog.AlbumTrackState{
			Status: catalog.AlbumTrackOK, FetchedAt: fetchedAt, AttemptedAt: fetchedAt,
		},
		tracks: []catalog.AlbumTrack{{TrackID: "t1", Name: "One", DiscNumber: 1, TrackNumber: 1}},
	}
	fetch := &fakeFetcher{}
	s := newDisabledService(t, cat, fetch, now)

	got, err := s.Listing(context.Background(), nil, "album000000000000000001")
	if err != nil {
		t.Fatalf("Listing: %v", err)
	}
	if got.State != StateReady || len(got.Tracks) != 1 {
		t.Fatalf("state/tracks = %q/%d, want %q with the stale listing still served",
			got.State, len(got.Tracks), StateReady)
	}
	if !got.FetchedAt.Equal(fetchedAt) {
		t.Fatalf("fetchedAt = %v, want %v", got.FetchedAt, fetchedAt)
	}
	s.Close()
	if n := fetch.called(); n != 0 {
		t.Fatalf("fetcher called %d times for an expired listing on a disabled instance, want 0: "+
			"the TTL check ran before the switch", n)
	}
	if claims, _, _ := cat.counts(); claims != 0 {
		t.Fatalf("attempted %d claims, want 0", claims)
	}
}
