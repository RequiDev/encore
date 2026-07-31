package artistalbums

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/RequiDev/encore/internal/config"
	"github.com/RequiDev/encore/internal/lazyfetch"
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
	mu       sync.Mutex
	state    catalog.ArtistAlbumState
	rows     []catalog.ArtistAlbum
	claims   int
	writes   int
	fails    int
	claimOK  bool
	claimErr error
	// stored is what the last successful replace wrote.
	stored []catalog.ArtistAlbum
	// lastReason is what the service asked to be stored in last_error.
	lastReason string
	// txSeq numbers the transactions inlineWriter has opened and curTx is the one
	// in force, or 0 outside any. replaceTx and markTx capture which one each
	// write ran in, which is the only way to tell "both inside the same
	// transaction" from "both happened" — the two are identical in final state.
	txSeq     int
	curTx     int
	replaceTx int
	markTx    int
	// failCtxErr is the state of the context record actually used, which is the
	// only way to see whether it was detached from the one Close cancels.
	failCtxErr error
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

func (f *fakeCatalog) transactions() (replace, mark int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.replaceTx, f.markTx
}

func (f *fakeCatalog) ArtistAlbumState(context.Context, storeQuerier, string) (catalog.ArtistAlbumState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state, nil
}

func (f *fakeCatalog) ArtistAlbums(context.Context, storeQuerier, string) ([]catalog.ArtistAlbum, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.rows, nil
}

func (f *fakeCatalog) ClaimArtistAlbumFetch(_ context.Context, _ storeQuerier, _ string, _, _ time.Time) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.claims++
	if f.claimErr != nil {
		return false, f.claimErr
	}
	return f.claimOK, nil
}

func (f *fakeCatalog) ReplaceArtistAlbums(_ context.Context, _ storeQuerier, _ string, items []catalog.ArtistAlbum) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writes++
	f.replaceTx = f.curTx
	f.stored = append([]catalog.ArtistAlbum(nil), items...)
	return nil
}

func (f *fakeCatalog) MarkArtistAlbumsFetched(_ context.Context, _ storeQuerier, _ string, _ time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.markTx = f.curTx
	return nil
}

func (f *fakeCatalog) FailArtistAlbumFetch(ctx context.Context, _ storeQuerier, _ string, _ time.Time, reason string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fails++
	f.lastReason = reason
	f.failCtxErr = ctx.Err()
	return nil
}

func (f *fakeCatalog) counts() (claims, writes, fails int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.claims, f.writes, f.fails
}

func (f *fakeCatalog) storedRows() []catalog.ArtistAlbum {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]catalog.ArtistAlbum(nil), f.stored...)
}

func (f *fakeCatalog) reason() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastReason
}

func (f *fakeCatalog) recordContextErr() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.failCtxErr
}

// fakeFetcher stands in for the Spotify client.
type fakeFetcher struct {
	mu    sync.Mutex
	items []spotify.ArtistAlbum
	err   error
	calls int
	// block, when non-nil, holds the fetch open until it is closed, which is how
	// a test observes the state while a walk is genuinely in flight.
	block chan struct{}
	// heedCtx makes the fake behave the way a real client does: it gives up as
	// soon as its context ends, returning that context's error instead of
	// waiting for block.
	//
	// Off by default, and that is not laziness. Close both cancels and waits,
	// and most tests here use block purely as a barrier — "the walk is
	// genuinely in flight, now assert" — with Close called once the test is
	// done with it. A fake that always raced ctx.Done() against block would let
	// Close's own cancellation win that race before the goroutine has consumed
	// the legitimate release, intermittently recording a failure instead of a
	// write; TestTheWalkOutlivesTheRequestContext hit exactly that before this
	// field existed. The two tests that are genuinely about a walk dying with
	// its context — a cancelled walk still recording its failure — turn it on
	// explicitly. Same shape as internal/albumtracks' fakeFetcher.heedCtx and
	// internal/lazyfetch's own fake.
	heedCtx bool
	// entered is closed once the call has begun, so a test knows the fetch
	// goroutine has already derived its context. ended is closed as it returns,
	// so a test knows nothing further will read that context.
	//
	// Both nil by default, and every test that does not set them gets the same
	// behaviour as before they existed. The one test that needs them,
	// TestTheWalkOutlivesTheRequestContext, leaves heedCtx off and waits for
	// ended before calling Close purely so the write it asserts on has
	// deterministically landed by then — with heedCtx off there is no ctx.Done
	// case for Close's cancellation to win a race against in the first place,
	// which is the point of that field rather than a second guard against the
	// same hazard. This mirrors internal/albumtracks' fakeFetcher, which carries
	// the identical fields for the identical reason.
	entered     chan struct{}
	ended       chan struct{}
	enteredOnce sync.Once
	endedOnce   sync.Once
	// gotMaxPages is the page budget the service actually passed on the last
	// call, which is the only way to see whether fill states its own page count
	// or defers to spotify.Client.ArtistAlbums' default.
	gotMaxPages int
}

func (f *fakeFetcher) ArtistAlbums(ctx context.Context, _ string, maxPages int) ([]spotify.ArtistAlbum, error) {
	f.mu.Lock()
	f.calls++
	f.gotMaxPages = maxPages
	block, heed := f.block, f.heedCtx
	items, err := f.items, f.err
	entered, ended := f.entered, f.ended
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
				return nil, ctx.Err()
			}
		} else {
			<-block
		}
	}
	return items, err
}

func (f *fakeFetcher) called() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeFetcher) maxPagesRequested() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.gotMaxPages
}

// inlineWriter runs the transaction body straight through, with no pool behind
// it. The Querier it hands over is nil, which is exactly right here: the fake
// catalogue ignores it, and these tests are about the *shape* of the write —
// that the replace and the mark happen inside one InTx — not its SQL.
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

func newServiceWith(t *testing.T, cfg config.ArtistAlbums, cat *fakeCatalog, fetch *fakeFetcher, now time.Time) *Service {
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

func newService(t *testing.T, cat *fakeCatalog, fetch *fakeFetcher, now time.Time) *Service {
	t.Helper()
	return newServiceWith(t, config.ArtistAlbums{Enabled: true, TTL: 7 * 24 * time.Hour}, cat, fetch, now)
}

func newDisabledService(t *testing.T, cat *fakeCatalog, fetch *fakeFetcher, now time.Time) *Service {
	t.Helper()
	return newServiceWith(t, config.ArtistAlbums{Enabled: false, TTL: 7 * 24 * time.Hour}, cat, fetch, now)
}

func album(id, name string) spotify.ArtistAlbum {
	return spotify.ArtistAlbum{ID: id, Name: name, Group: catalog.AlbumGroupAlbum, ReleasePrecision: "year"}
}

// TestArtistAlbumStatusConstantsMatchLazyfetch pins the property this package
// depends on but does not enforce anywhere in the type system: due compares
// catalog.ArtistAlbumState.Status, read out of Postgres as the literal text
// this package's repository writes, against lazyfetch's own Status* string
// constants. Nothing stops the two sets of literals drifting apart — they are
// declared in two different packages for a reason: lazyfetch must not import a
// caller, so it cannot reach into catalog to declare "these must match" for
// itself. If they ever disagree, due silently falls through to its default
// case for every status this repository can report, and every discography in
// this catalogue stops being refreshed, forever, with no error anywhere.
//
// This was flagged as a gap during Task 4's review — the sibling package
// internal/albumtracks carries the identical assertion — and it belongs in the
// caller's test file rather than lazyfetch's for exactly the reason lazyfetch
// cannot check it itself.
func TestArtistAlbumStatusConstantsMatchLazyfetch(t *testing.T) {
	if catalog.ArtistAlbumFetching != lazyfetch.StatusFetching {
		t.Fatalf("catalog.ArtistAlbumFetching = %q, lazyfetch.StatusFetching = %q: they must be the "+
			"same literal or due stops recognising a live lease", catalog.ArtistAlbumFetching, lazyfetch.StatusFetching)
	}
	if catalog.ArtistAlbumOK != lazyfetch.StatusOK {
		t.Fatalf("catalog.ArtistAlbumOK = %q, lazyfetch.StatusOK = %q: they must be the same literal "+
			"or due stops recognising a stored success", catalog.ArtistAlbumOK, lazyfetch.StatusOK)
	}
	if catalog.ArtistAlbumFailed != lazyfetch.StatusFailed {
		t.Fatalf("catalog.ArtistAlbumFailed = %q, lazyfetch.StatusFailed = %q: they must be the same "+
			"literal or due stops recognising a recorded failure", catalog.ArtistAlbumFailed, lazyfetch.StatusFailed)
	}
}

var at = time.Date(2026, time.July, 31, 12, 0, 0, 0, time.UTC)

func TestFirstViewStartsTheWalkAndReportsPending(t *testing.T) {
	cat := &fakeCatalog{claimOK: true}
	fetch := &fakeFetcher{block: make(chan struct{})}
	s := newService(t, cat, fetch, at)

	got, err := s.Discography(context.Background(), nil, "artist-1")
	if err != nil {
		t.Fatalf("Discography: %v", err)
	}
	if got.State != StatePending {
		t.Fatalf("state = %q, want %q on an artist nobody has enumerated", got.State, StatePending)
	}
	if len(got.Releases) != 0 {
		t.Fatalf("returned %d releases before any walk finished", len(got.Releases))
	}
	close(fetch.block)
	s.Close()
	if fetch.called() != 1 {
		t.Fatalf("the client was called %d times, want 1", fetch.called())
	}
}

// TestDiscographyDoesNotWaitForSpotify is the load-bearing property of the
// whole feature: the page request answers while the walk is still running.
func TestDiscographyDoesNotWaitForSpotify(t *testing.T) {
	cat := &fakeCatalog{claimOK: true}
	fetch := &fakeFetcher{block: make(chan struct{})}
	s := newService(t, cat, fetch, at)

	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := s.Discography(context.Background(), nil, "artist-1"); err != nil {
			t.Errorf("Discography: %v", err)
		}
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Discography did not return while the walk was still in flight; the page waits on Spotify")
	}
	close(fetch.block)
}

// TestFillDefersThePageBudgetToTheClient pins a defect a review of this
// package caught before it ever shipped: fill once passed a local maxPages
// constant of 20, silently overriding spotify.Client.ArtistAlbums' own
// forty-page default — artistAlbumBudget only falls back to that default when
// the caller passes zero or less. That default was deliberately raised from
// twenty to forty upstream (internal/spotify/artistalbums.go), specifically
// because a walk pinned at twenty never stores anything for an artist whose
// combined releases exceed it — truncation is a failure and the replace is
// all-or-nothing, so their panel reads "could not be read" forever. A second,
// locally chosen number in this package can fall out of step with that
// decision the moment either one changes without the other; passing zero
// means there is only ever one number to agree with.
func TestFillDefersThePageBudgetToTheClient(t *testing.T) {
	cat := &fakeCatalog{claimOK: true}
	fetch := &fakeFetcher{items: []spotify.ArtistAlbum{album("a1", "First")}}
	s := newService(t, cat, fetch, at)

	if _, err := s.Discography(context.Background(), nil, "artist-1"); err != nil {
		t.Fatalf("Discography: %v", err)
	}
	s.Close()

	if got := fetch.maxPagesRequested(); got > 0 {
		t.Fatalf("fill asked for %d pages, want <= 0: a positive number here overrides "+
			"spotify.Client.ArtistAlbums' own forty-page default with a smaller one, which is "+
			"exactly the regression this test exists to catch", got)
	}
}

// TestTheDiscographyAndItsStatusCommitTogether is the single-transaction
// requirement. Nothing about the final state on disk distinguishes "both wrote"
// from "both wrote in one transaction", so the fake numbers its transactions and
// this asserts the two writes carry the same number.
func TestTheDiscographyAndItsStatusCommitTogether(t *testing.T) {
	cat := &fakeCatalog{claimOK: true}
	fetch := &fakeFetcher{items: []spotify.ArtistAlbum{album("a1", "First")}}
	s := newService(t, cat, fetch, at)

	if _, err := s.Discography(context.Background(), nil, "artist-1"); err != nil {
		t.Fatalf("Discography: %v", err)
	}
	s.Close()

	replaceTx, markTx := cat.transactions()
	if replaceTx == 0 || markTx == 0 {
		t.Fatalf("replace ran in tx %d and mark in tx %d; one of them ran outside any transaction",
			replaceTx, markTx)
	}
	if replaceTx != markTx {
		t.Fatalf("replace ran in tx %d and mark in tx %d; a crash between them leaves a listing "+
			"with no 'ok' beside it, or an 'ok' beside a listing that was never finished",
			replaceTx, markTx)
	}
}

// TestATruncatedWalkWritesNothing is the ErrTruncated rule, and it is the one
// most likely to be quietly broken, because the error arrives *with* real data.
// ReplaceArtistAlbums is delete-absent, so writing the prefix would delete the
// tail of a discography that was correct and then mark the result authoritative.
func TestATruncatedWalkWritesNothing(t *testing.T) {
	cat := &fakeCatalog{claimOK: true}
	fetch := &fakeFetcher{
		items: []spotify.ArtistAlbum{album("a1", "First"), album("a2", "Second")},
		err:   spotify.ErrTruncated,
	}
	s := newService(t, cat, fetch, at)

	if _, err := s.Discography(context.Background(), nil, "artist-1"); err != nil {
		t.Fatalf("Discography: %v", err)
	}
	s.Close()

	_, writes, fails := cat.counts()
	if writes != 0 {
		t.Fatalf("a truncated walk wrote %d times, want 0: the prefix would delete the tail of a "+
			"discography that was correct", writes)
	}
	if fails != 1 {
		t.Fatalf("a truncated walk recorded %d failures, want 1", fails)
	}
	if len(cat.storedRows()) != 0 {
		t.Fatalf("a truncated walk stored %d rows, want 0", len(cat.storedRows()))
	}
}

// TestAnEmptyResponseIsAFailure pins the emptiness rule at the level it belongs
// to: the *whole* response. An artist in this catalogue is there because
// somebody played a track by them, so a 200 with no items means the artist is
// invisible to this application's market, not that they have released nothing.
func TestAnEmptyResponseIsAFailure(t *testing.T) {
	cat := &fakeCatalog{claimOK: true}
	// The exact shape spotify.ArtistAlbums returns for a 200 carrying no items.
	fetch := &fakeFetcher{items: nil, err: nil}
	s := newService(t, cat, fetch, at)

	if _, err := s.Discography(context.Background(), nil, "artist-1"); err != nil {
		t.Fatalf("Discography: %v", err)
	}
	s.Close()

	_, writes, fails := cat.counts()
	if writes != 0 {
		t.Fatalf("an empty response wrote %d times, want 0", writes)
	}
	if fails != 1 {
		t.Fatalf("an empty response recorded %d failures, want 1: stored as a success it would make "+
			"the page say this artist has released nothing", fails)
	}
}

// TestAnAllSinglesDiscographyIsStoredAsASuccess is the one rule that differs
// from 2e-i, and the one a transcription of albumtracks.go is most likely to get
// wrong by adding a guard that does not belong. An album with no tracks is
// impossible; an artist who has only released singles is ordinary, and their
// discography must be stored, marked 'ok', and served as ready — with a counted
// set that happens to be empty.
func TestAnAllSinglesDiscographyIsStoredAsASuccess(t *testing.T) {
	cat := &fakeCatalog{claimOK: true}
	fetch := &fakeFetcher{items: []spotify.ArtistAlbum{
		{ID: "a1", Name: "One", Group: catalog.AlbumGroupSingle},
		{ID: "a2", Name: "Two", Group: catalog.AlbumGroupAppearsOn},
	}}
	s := newService(t, cat, fetch, at)

	if _, err := s.Discography(context.Background(), nil, "artist-1"); err != nil {
		t.Fatalf("Discography: %v", err)
	}
	s.Close()

	_, writes, fails := cat.counts()
	if fails != 0 {
		t.Fatalf("an all-singles discography recorded %d failures, want 0: zero *albums* is a fact "+
			"about the artist, not a failed read", fails)
	}
	if writes != 1 {
		t.Fatalf("an all-singles discography wrote %d times, want 1", writes)
	}
	if n := len(cat.storedRows()); n != 2 {
		t.Fatalf("stored %d rows, want both non-album groups kept: the page cannot say what it set "+
			"aside if the service drops it", n)
	}
}

// TestCountedIDsIsOnlyTheAlbumGroup pins the filter's one definition. Coverage
// counts album_group 'album' and nothing else, and the handler asks which ids to
// look up through this rather than re-deriving the predicate.
func TestCountedIDsIsOnlyTheAlbumGroup(t *testing.T) {
	d := Discography{Releases: []Release{
		{AlbumID: "a1", Group: catalog.AlbumGroupAlbum},
		{AlbumID: "s1", Group: catalog.AlbumGroupSingle},
		{AlbumID: "c1", Group: catalog.AlbumGroupCompilation},
		{AlbumID: "p1", Group: catalog.AlbumGroupAppearsOn},
		{AlbumID: "x1", Group: "ep"},
		{AlbumID: "a2", Group: catalog.AlbumGroupAlbum},
	}}
	got := d.CountedIDs()
	if len(got) != 2 || got[0] != "a1" || got[1] != "a2" {
		t.Fatalf("CountedIDs() = %v, want [a1 a2]: only album_group 'album' is counted, and a group "+
			"Spotify adds later must not silently join the denominator", got)
	}
}

func TestAFreshDiscographyIsNotRefetched(t *testing.T) {
	cat := &fakeCatalog{
		claimOK: true,
		state:   catalog.ArtistAlbumState{Status: catalog.ArtistAlbumOK, FetchedAt: at.Add(-24 * time.Hour)},
		rows:    []catalog.ArtistAlbum{{AlbumID: "a1", Name: "First", Group: catalog.AlbumGroupAlbum}},
	}
	fetch := &fakeFetcher{}
	s := newService(t, cat, fetch, at)

	got, err := s.Discography(context.Background(), nil, "artist-1")
	if err != nil {
		t.Fatalf("Discography: %v", err)
	}
	s.Close()

	if got.State != StateReady {
		t.Fatalf("state = %q, want %q", got.State, StateReady)
	}
	if fetch.called() != 0 {
		t.Fatalf("a one-day-old discography was refetched %d times against a seven-day TTL", fetch.called())
	}
	if claims, _, _ := cat.counts(); claims != 0 {
		t.Fatalf("a fresh discography claimed the lease %d times, want 0", claims)
	}
}

func TestAnExpiredDiscographyIsRefetchedAndStillServed(t *testing.T) {
	cat := &fakeCatalog{
		claimOK: true,
		state:   catalog.ArtistAlbumState{Status: catalog.ArtistAlbumOK, FetchedAt: at.Add(-8 * 24 * time.Hour)},
		rows:    []catalog.ArtistAlbum{{AlbumID: "a1", Name: "First", Group: catalog.AlbumGroupAlbum}},
	}
	fetch := &fakeFetcher{items: []spotify.ArtistAlbum{album("a1", "First"), album("a2", "Second")}}
	s := newService(t, cat, fetch, at)

	got, err := s.Discography(context.Background(), nil, "artist-1")
	if err != nil {
		t.Fatalf("Discography: %v", err)
	}
	// The stale listing is served *now*, not withheld until the refresh lands.
	if got.State != StateReady || len(got.Releases) != 1 {
		t.Fatalf("state = %q with %d releases, want ready with the 1 already stored", got.State, len(got.Releases))
	}
	if got.FetchedAt.IsZero() {
		t.Fatal("fetchedAt is zero on a stale ready listing; the page cannot say how old it is")
	}
	s.Close()
	if fetch.called() != 1 {
		t.Fatalf("the client was called %d times, want 1: an eight-day-old discography is past a "+
			"seven-day TTL", fetch.called())
	}
}

func TestAFailedWalkIsNotRetriedImmediately(t *testing.T) {
	cat := &fakeCatalog{
		claimOK: true,
		state:   catalog.ArtistAlbumState{Status: catalog.ArtistAlbumFailed, AttemptedAt: at.Add(-time.Minute)},
	}
	fetch := &fakeFetcher{}
	s := newService(t, cat, fetch, at)

	got, err := s.Discography(context.Background(), nil, "artist-1")
	if err != nil {
		t.Fatalf("Discography: %v", err)
	}
	s.Close()

	if got.State != StateUnavailable {
		t.Fatalf("state = %q, want %q inside the retry backoff", got.State, StateUnavailable)
	}
	if fetch.called() != 0 {
		t.Fatalf("a failure one minute old was retried %d times against a fifteen-minute backoff",
			fetch.called())
	}
}

func TestAFailedWalkIsRetriedAfterTheBackoff(t *testing.T) {
	cat := &fakeCatalog{
		claimOK: true,
		state:   catalog.ArtistAlbumState{Status: catalog.ArtistAlbumFailed, AttemptedAt: at.Add(-16 * time.Minute)},
	}
	fetch := &fakeFetcher{items: []spotify.ArtistAlbum{album("a1", "First")}}
	s := newService(t, cat, fetch, at)

	got, err := s.Discography(context.Background(), nil, "artist-1")
	if err != nil {
		t.Fatalf("Discography: %v", err)
	}
	if got.State != StatePending {
		t.Fatalf("state = %q, want %q once the backoff has elapsed", got.State, StatePending)
	}
	s.Close()
	if fetch.called() != 1 {
		t.Fatalf("the client was called %d times, want 1", fetch.called())
	}
}

// TestALiveLeaseIsNotEvenClaimed pins the guard that keeps a polling browser
// from attempting a write on every tick.
func TestALiveLeaseIsNotEvenClaimed(t *testing.T) {
	cat := &fakeCatalog{
		claimOK: true,
		state:   catalog.ArtistAlbumState{Status: catalog.ArtistAlbumFetching, AttemptedAt: at.Add(-time.Second)},
	}
	fetch := &fakeFetcher{}
	s := newService(t, cat, fetch, at)

	got, err := s.Discography(context.Background(), nil, "artist-1")
	if err != nil {
		t.Fatalf("Discography: %v", err)
	}
	s.Close()

	if got.State != StatePending {
		t.Fatalf("state = %q, want %q while another replica holds the lease", got.State, StatePending)
	}
	if claims, _, _ := cat.counts(); claims != 0 {
		t.Fatalf("a live lease was claimed %d times, want 0: a polling tab would write on every tick",
			claims)
	}
}

func TestAnExpiredLeaseIsReclaimed(t *testing.T) {
	cat := &fakeCatalog{
		claimOK: true,
		state:   catalog.ArtistAlbumState{Status: catalog.ArtistAlbumFetching, AttemptedAt: at.Add(-4 * time.Minute)},
	}
	fetch := &fakeFetcher{items: []spotify.ArtistAlbum{album("a1", "First")}}
	s := newService(t, cat, fetch, at)

	if _, err := s.Discography(context.Background(), nil, "artist-1"); err != nil {
		t.Fatalf("Discography: %v", err)
	}
	s.Close()
	if claims, _, _ := cat.counts(); claims != 1 {
		t.Fatalf("an expired lease was claimed %d times, want 1: the artist is stranded in 'fetching' "+
			"for ever without this", claims)
	}
}

// TestALostClaimStartsNoSecondWalk covers the losing side of the lease.
func TestALostClaimStartsNoSecondWalk(t *testing.T) {
	cat := &fakeCatalog{claimOK: false}
	fetch := &fakeFetcher{}
	s := newService(t, cat, fetch, at)

	got, err := s.Discography(context.Background(), nil, "artist-1")
	if err != nil {
		t.Fatalf("Discography: %v", err)
	}
	s.Close()

	if got.State != StatePending {
		t.Fatalf("state = %q, want %q: a walk *is* running, just not this one", got.State, StatePending)
	}
	if fetch.called() != 0 {
		t.Fatalf("a lost claim still walked %d times", fetch.called())
	}
}

// TestAClaimErrorIsPendingNotUnavailable is the copy-relevant one. A claim that
// errors records no outcome, so the very next request re-enters this branch;
// calling it unavailable would blame Spotify for a local fault and tell a page
// whose job is to stop polling on unavailable to give up on a walk that was
// still coming.
func TestAClaimErrorIsPendingNotUnavailable(t *testing.T) {
	cat := &fakeCatalog{claimErr: errors.New("read-only transaction")}
	fetch := &fakeFetcher{}
	s := newService(t, cat, fetch, at)

	got, err := s.Discography(context.Background(), nil, "artist-1")
	if err != nil {
		t.Fatalf("Discography: %v", err)
	}
	s.Close()

	if got.State != StatePending {
		t.Fatalf("state = %q, want %q: nothing recorded an outcome, so nothing may report one",
			got.State, StatePending)
	}
	if _, _, fails := cat.counts(); fails != 0 {
		t.Fatalf("a claim error recorded %d failures, want 0", fails)
	}
}

// TestTheErrorFromSpotifyIsNeverReturnedToTheCaller: a third-party outage is a
// state the page renders, not a 500 it shows.
func TestTheErrorFromSpotifyIsNeverReturnedToTheCaller(t *testing.T) {
	cat := &fakeCatalog{claimOK: true}
	fetch := &fakeFetcher{err: errors.New("spotify: 503")}
	s := newService(t, cat, fetch, at)

	if _, err := s.Discography(context.Background(), nil, "artist-1"); err != nil {
		t.Fatalf("Discography returned %v; a Spotify failure is a state, not a 500", err)
	}
	s.Close()
	if _, _, fails := cat.counts(); fails != 1 {
		t.Fatalf("recorded %d failures, want 1", fails)
	}
}

// TestTheFailureReasonIsRecordedWhole pins that the service hands the cause over
// intact. catalog.FailArtistAlbumFetch bounds it with store.Truncate, which cuts
// on a rune boundary; truncating again here would only risk two ellipses.
func TestTheFailureReasonIsRecordedWhole(t *testing.T) {
	cat := &fakeCatalog{claimOK: true}
	fetch := &fakeFetcher{err: errors.New("spotify: artist albums: 503 service unavailable")}
	s := newService(t, cat, fetch, at)

	if _, err := s.Discography(context.Background(), nil, "artist-1"); err != nil {
		t.Fatalf("Discography: %v", err)
	}
	s.Close()
	if got := cat.reason(); got != "spotify: artist albums: 503 service unavailable" {
		t.Fatalf("recorded reason = %q, want the cause whole", got)
	}
}

// TestTheWalkOutlivesTheRequestContext: the page request has already been
// answered, and cancelling when the browser navigated away would mean the
// discography never arrives however many times the page is opened.
//
// The fetch must actually have returned — <-fetch.ended, not just close(release)
// — before Close runs. Without that wait, Close's own cancellation of the
// walk's context can reach fakeFetcher.ArtistAlbums' select before the goroutine
// has consumed the legitimate release, and the two race for which case fires;
// this test found that race in its earlier, unsynchronised form failing about
// half the time.
func TestTheWalkOutlivesTheRequestContext(t *testing.T) {
	cat := &fakeCatalog{claimOK: true}
	release := make(chan struct{})
	fetch := &fakeFetcher{
		items: []spotify.ArtistAlbum{album("a1", "First")}, block: release,
		entered: make(chan struct{}), ended: make(chan struct{}),
	}
	s := newService(t, cat, fetch, at)

	ctx, cancel := context.WithCancel(context.Background())
	if _, err := s.Discography(ctx, nil, "artist-1"); err != nil {
		t.Fatalf("Discography: %v", err)
	}
	// Wait until the walk has begun, so its context is already derived from
	// whatever it was going to be derived from, and only then end the request.
	<-fetch.entered
	cancel() // the browser navigated away
	close(release)
	// Nothing reads the walk's context after this, so Close cannot race it.
	<-fetch.ended
	s.Close()

	_, writes, fails := cat.counts()
	if writes != 1 || fails != 0 {
		t.Fatalf("writes = %d, fails = %d, want 1 and 0: the walk died with the request that started it",
			writes, fails)
	}
}

// TestAFailureIsStillRecordedWhenCloseCancelsTheWalk pins the detached record
// context. Without context.WithoutCancel the write goes out on a cancelled
// context, records nothing, and the artist stays 'fetching' until the lease
// expires — which is exactly the strand the lease exists to bound, arriving
// through a door nothing else closes.
func TestAFailureIsStillRecordedWhenCloseCancelsTheWalk(t *testing.T) {
	cat := &fakeCatalog{claimOK: true}
	// heedCtx: true because this test is genuinely about the walk dying with
	// its context: block is never closed, so the only way the fetch call ends
	// is Close's cancellation reaching ctx.Done.
	fetch := &fakeFetcher{block: make(chan struct{}), heedCtx: true}
	s := newService(t, cat, fetch, at)

	if _, err := s.Discography(context.Background(), nil, "artist-1"); err != nil {
		t.Fatalf("Discography: %v", err)
	}
	s.Close() // cancels the walk, then waits for it

	if _, _, fails := cat.counts(); fails != 1 {
		t.Fatalf("recorded %d failures after Close cancelled the walk, want 1", fails)
	}
	if err := cat.recordContextErr(); err != nil {
		t.Fatalf("the failure was recorded on a context already cancelled (%v); "+
			"context.WithoutCancel is what keeps that write alive through shutdown", err)
	}
}

func TestNewRejectsAnIncompleteConfiguration(t *testing.T) {
	cat := &fakeCatalog{}
	fetch := &fakeFetcher{}
	ok := config.ArtistAlbums{Enabled: true, TTL: time.Hour}

	for name, deps := range map[string]Deps{
		"no catalog": {Spotify: fetch, Writer: inlineWriter{cat: cat}},
		"no spotify": {Catalog: cat, Writer: inlineWriter{cat: cat}},
		"no writer":  {Catalog: cat, Spotify: fetch},
	} {
		if _, err := New(ok, deps); err == nil {
			t.Errorf("New with %s succeeded; a half-wired service answers some artists and panics on others", name)
		}
	}
	if _, err := New(config.ArtistAlbums{Enabled: true, TTL: 0}, Deps{
		Catalog: cat, Spotify: fetch, Writer: inlineWriter{cat: cat},
	}); err == nil {
		t.Error("New with a zero TTL succeeded; every stored discography would be permanently due")
	}
}

// TestDisabledMakesNoRequestAndClaimsNoLease is what an operator asking for no
// unattended traffic actually asked for: not even the write.
func TestDisabledMakesNoRequestAndClaimsNoLease(t *testing.T) {
	cat := &fakeCatalog{claimOK: true}
	fetch := &fakeFetcher{}
	s := newDisabledService(t, cat, fetch, at)

	got, err := s.Discography(context.Background(), nil, "artist-1")
	if err != nil {
		t.Fatalf("Discography: %v", err)
	}
	s.Close()

	if got.State != StateDisabled {
		t.Fatalf("state = %q, want %q", got.State, StateDisabled)
	}
	if fetch.called() != 0 {
		t.Fatalf("a disabled instance made %d requests, want 0", fetch.called())
	}
	if claims, _, _ := cat.counts(); claims != 0 {
		t.Fatalf("a disabled instance claimed the lease %d times, want 0", claims)
	}
}

// TestDisabledIsNotUnavailable keeps the two facts apart at the source. A page
// that renders "Spotify would not answer" for "nobody asked Spotify" blames a
// third party for a local decision.
func TestDisabledIsNotUnavailable(t *testing.T) {
	cat := &fakeCatalog{}
	s := newDisabledService(t, cat, &fakeFetcher{}, at)

	got, err := s.Discography(context.Background(), nil, "artist-1")
	if err != nil {
		t.Fatalf("Discography: %v", err)
	}
	if got.State == StateUnavailable {
		t.Fatal("a switched-off instance reported unavailable; that is a recorded Spotify failure, " +
			"and no request was ever made")
	}
}

// TestDisabledStillServesAStaleDiscographyWithoutRefreshing: off means "do not
// fetch", not "forget what is on disk", and the TTL is not even consulted.
func TestDisabledStillServesAStaleDiscographyWithoutRefreshing(t *testing.T) {
	cat := &fakeCatalog{
		claimOK: true,
		state: catalog.ArtistAlbumState{
			Status: catalog.ArtistAlbumOK, FetchedAt: at.Add(-400 * 24 * time.Hour),
		},
		rows: []catalog.ArtistAlbum{{AlbumID: "a1", Name: "First", Group: catalog.AlbumGroupAlbum}},
	}
	fetch := &fakeFetcher{}
	s := newDisabledService(t, cat, fetch, at)

	got, err := s.Discography(context.Background(), nil, "artist-1")
	if err != nil {
		t.Fatalf("Discography: %v", err)
	}
	s.Close()

	if got.State != StateReady || len(got.Releases) != 1 {
		t.Fatalf("state = %q with %d releases, want ready with the stored one: withholding a listing "+
			"that was correct when it was read is strictly worse than showing it with its date",
			got.State, len(got.Releases))
	}
	if got.FetchedAt.IsZero() {
		t.Fatal("fetchedAt is zero; on an instance that will never refresh, the date is the only honesty available")
	}
	if fetch.called() != 0 || cat.claims != 0 {
		t.Fatalf("a disabled instance refreshed a year-old discography: %d requests, %d claims",
			fetch.called(), cat.claims)
	}
}

// TestCloseEndsAWalkInFlight is the delegation test: Service.Close must reach
// the Gate's Close, which is what cancels a detached walk and waits for it.
//
// The mechanism it delegates to — the closing-under-mutex ordering, the
// WaitGroup, the slot channel — is pinned in internal/lazyfetch, where it lives.
// This asserts only the wiring, and it is the whole of what this package can get
// wrong about shutdown.
func TestCloseEndsAWalkInFlight(t *testing.T) {
	cat := &fakeCatalog{claimOK: true}
	block := make(chan struct{}) // deliberately never closed by the test body
	// heedCtx: true because this test's whole point is that Close's cancellation
	// is what ends the walk; without it, nothing here would ever unblock the
	// fetch, and the test would hang until its own 5-second timeout.
	fetch := &fakeFetcher{block: block, heedCtx: true}
	s := newService(t, cat, fetch, at)

	// A rescue, registered after the service so it runs *before* Cleanup's Close:
	// if Close turns out not to cancel anything, the assertion below reports that
	// rather than hanging the whole package on a stuck Cleanup.
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(block) }) })

	if _, err := s.Discography(context.Background(), nil, "artist-1"); err != nil {
		t.Fatalf("Discography: %v", err)
	}

	closed := make(chan struct{})
	go func() {
		defer close(closed)
		s.Close()
	}()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return while a walk was in flight; Service.Close does not reach the " +
			"Gate's, so the walk is on a context nothing cancels and outlives the service")
	}

	// And the cancelled walk still recorded its failure, which is what keeps the
	// artist out of 'fetching' rather than stranded there until the lease expires.
	if _, _, fails := cat.counts(); fails != 1 {
		t.Fatalf("recorded %d failures for a cancelled walk, want 1", fails)
	}
}
