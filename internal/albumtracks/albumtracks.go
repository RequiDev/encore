// Package albumtracks fills and serves the cached listing of an album's own
// tracks, which is what lets the album page name the tracks somebody has never
// played.
//
// Nothing here is a background loop. A sweep over every album in a history is
// rejected explicitly, so a listing is read the first time somebody opens that
// album's page and then kept for the configured TTL. What this package
// guarantees is that the page request itself never waits for Spotify: Listing
// answers from the database and, when a fetch is due, hands the walk to a
// goroutine on a context of its own.
//
// Two guards keep that from becoming a stampede, and they answer different
// questions. A bounded slot channel is this process asking "am I already busy?"
// A conditional write against album_track_fetches is the whole deployment
// asking "is anybody busy?" — and only the second survives two browser tabs,
// two API replicas, or a page that polls.
package albumtracks

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/RequiDev/encore/internal/config"
	"github.com/RequiDev/encore/internal/logging"
	"github.com/RequiDev/encore/internal/spotify"
	"github.com/RequiDev/encore/internal/store"
	"github.com/RequiDev/encore/internal/store/catalog"
)

const (
	// maxPages bounds one album's listing. Twenty pages of fifty is a thousand
	// tracks, which no released album approaches; it exists so a paging bug
	// cannot spend the instance's quota on one record.
	maxPages = 20
	// concurrency is how many listings this process reads at once. Small on
	// purpose: these start inside page requests, and they draw on the same quota
	// enrichment needs to do its job.
	concurrency = 4
	// leaseTTL is how long a 'fetching' row holds other callers off. Longer than
	// fetchTimeout, so a live fetch never loses its own lease, and short enough
	// that a process killed mid-fetch does not strand the album for long.
	leaseTTL = 2 * time.Minute
	// fetchTimeout bounds one album's whole walk — every page, every retry and
	// every rate-limit wait inside it.
	fetchTimeout = 90 * time.Second
	// failedRetryAfter is how long a failed listing is left alone. Failures here
	// are timeouts and rate limits, which clear in minutes; making somebody wait
	// out the thirty-day TTL would turn one bad minute into a broken panel.
	failedRetryAfter = 15 * time.Minute
	// recordTimeout bounds the write that records a failure, including during
	// shutdown.
	recordTimeout = 5 * time.Second
)

// errEmptyListing is a 200 that carried no tracks.
//
// There is no such record as an album with no tracks. An empty listing means
// the album is invisible to this application's market, or Spotify has withdrawn
// it. Storing it as a success would make the page say "you have played every
// track on this album", which is the exact overclaim this feature exists to
// avoid.
var errEmptyListing = errors.New("albumtracks: spotify returned no tracks for this album")

// State is what the page can say about the listing.
type State string

const (
	// StateReady means a listing is stored and can be reasoned about. It may be
	// older than the TTL, in which case a refresh is already running behind it.
	StateReady State = "ready"
	// StatePending means nothing is stored yet and a fetch is running, or is due
	// and will be started by the next view.
	//
	// Everything that merely *delays* a fetch reports this: a lease somebody else
	// holds, no free local slot, a claim that errored, a shutdown in progress.
	// None of them records an outcome, so the listing is still due and the very
	// next view starts it — which is what makes "keep polling" the right advice.
	StatePending State = "pending"
	// StateUnavailable means nothing is stored and the last attempt to read a
	// listing failed, recently enough that no new one has been started.
	//
	// It is emphatically not "this album has no tracks", and it is deliberately
	// not "this process was too busy to ask" either: only a *recorded* failure
	// reaches here, which is what lets the page treat it as a reason to stop
	// polling and say so. Reporting local backpressure as unavailable would tell
	// somebody Spotify would not answer when Spotify was never asked — the same
	// category error that keeps StateDisabled separate.
	StateUnavailable State = "unavailable"
	// StateDisabled means nothing is stored and this instance will not fetch it,
	// because its operator turned that off.
	//
	// Deliberately not folded into StateUnavailable. "Spotify would not answer"
	// and "nobody asked Spotify" are different facts, and a page that renders the
	// first for the second blames a third party for a local decision.
	StateDisabled State = "disabled"
)

// Track is one entry of a listing.
type Track struct {
	ID          string
	Name        string
	DiscNumber  int
	TrackNumber int
}

// Listing is what one album's page is told.
type Listing struct {
	State State
	// Tracks is the whole listing in disc and track order, not just the unheard
	// part: which of them were played is the caller's question to ask, because
	// only the caller knows whose history it is asking about.
	Tracks []Track
	// FetchedAt is when the listing was read. Zero when none has succeeded.
	FetchedAt time.Time
}

// Fetcher is the slice of the Spotify client this package uses.
type Fetcher interface {
	AlbumTracks(ctx context.Context, albumID string, maxPages int) ([]spotify.AlbumTrack, error)
}

// Store is the slice of the catalogue repository this package uses. An
// interface so the policy above can be exercised without a database — these
// decisions are about *when* to fetch, which no amount of SQL will tell you.
type Store interface {
	AlbumTrackState(ctx context.Context, q store.Querier, albumID string) (catalog.AlbumTrackState, error)
	AlbumTracks(ctx context.Context, q store.Querier, albumID string) ([]catalog.AlbumTrack, error)
	ClaimAlbumTrackFetch(ctx context.Context, q store.Querier, albumID string, now, leaseCutoff time.Time) (bool, error)
	ReplaceAlbumTracks(ctx context.Context, q store.Querier, albumID string, items []catalog.AlbumTrack) error
	MarkAlbumTracksFetched(ctx context.Context, q store.Querier, albumID string, at time.Time) error
	FailAlbumTrackFetch(ctx context.Context, q store.Querier, albumID string, at time.Time, reason string) error
}

// Writer runs the one transaction this package needs and hands out the handle
// for everything outside it. *store.Store satisfies it through StoreWriter
// below; a test satisfies it without a pool.
type Writer interface {
	// InTx runs fn inside one transaction.
	InTx(ctx context.Context, fn func(ctx context.Context, q store.Querier) error) error
	// DB is the pool as a Querier, for single statements.
	DB() store.Querier
}

// StoreWriter adapts *store.Store to Writer. It is the only place in this
// package that names pgx, which keeps the policy above testable.
type StoreWriter struct{ Store *store.Store }

// InTx runs fn inside one transaction.
func (w StoreWriter) InTx(ctx context.Context, fn func(ctx context.Context, q store.Querier) error) error {
	return w.Store.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error { return fn(ctx, tx) })
}

// DB returns the pool as a Querier.
func (w StoreWriter) DB() store.Querier { return w.Store.DB() }

// Deps is everything the service needs.
type Deps struct {
	Catalog Store
	Spotify Fetcher
	Writer  Writer
	Logger  *slog.Logger
	// Now is the clock. Tests replace it; production leaves it nil.
	Now func() time.Time
}

// Service fills and serves album track listings.
type Service struct {
	cat Store
	sp  Fetcher
	w   Writer
	log *slog.Logger
	now func() time.Time
	// enabled is the operator's switch. False means this instance never asks
	// Spotify anything — see config.AlbumTracks.Enabled.
	enabled bool
	ttl     time.Duration
	slots   chan struct{}
	// base is the parent of every detached fetch, so Close can end them all. It
	// is a context in a struct on purpose: these fetches outlive the request
	// that started them, so there is no incoming context for them to inherit.
	base   context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	// mu guards closing, which is the only thing that may not race wg.Add
	// against wg.Wait. See track.
	mu      sync.Mutex
	closing bool
}

// New validates the dependencies and builds the service.
//
// The caller owns Close and must call it during shutdown, before closing the
// database pool: fetches run detached from any request, so nothing else will
// ever wait for them, and a fetch cancelled at shutdown still needs the pool to
// record that it failed. Constructing this after the pool and deferring Close
// immediately puts both in the right order.
func New(cfg config.AlbumTracks, deps Deps) (*Service, error) {
	switch {
	case deps.Catalog == nil:
		return nil, errors.New("albumtracks: catalog repository is required")
	case deps.Spotify == nil:
		return nil, errors.New("albumtracks: spotify client is required")
	case deps.Writer == nil:
		return nil, errors.New("albumtracks: writer is required")
	case cfg.TTL <= 0:
		return nil, errors.New("albumtracks: a positive TTL is required")
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
	if !cfg.Enabled {
		// Said once, at startup, rather than on every page view: an operator who
		// wonders why the album page reports this as turned off can find it here
		// and in the configuration line the process logs beside it.
		lg.Info("album track listings are turned off; this instance will not ask spotify " +
			"what is on an album. Listings already cached are still shown.")
	}
	return &Service{
		cat:     deps.Catalog,
		sp:      deps.Spotify,
		w:       deps.Writer,
		log:     lg.With("component", "albumtracks"),
		now:     now,
		enabled: cfg.Enabled,
		ttl:     cfg.TTL,
		slots:   make(chan struct{}, concurrency),
		base:    base,
		cancel:  cancel,
	}, nil
}

// Listing returns the stored listing for one album, and starts a refresh when
// one is due.
//
// It never blocks on Spotify and it never fails because Spotify did: a
// third-party outage is a state the page renders, not a 500 it shows.
func (s *Service) Listing(ctx context.Context, q store.Querier, albumID string) (Listing, error) {
	st, err := s.cat.AlbumTrackState(ctx, q, albumID)
	if err != nil {
		return Listing{}, err
	}

	var tracks []Track
	if !st.FetchedAt.IsZero() {
		rows, err := s.cat.AlbumTracks(ctx, q, albumID)
		if err != nil {
			return Listing{}, err
		}
		tracks = make([]Track, 0, len(rows))
		for _, r := range rows {
			tracks = append(tracks, Track{
				ID: r.TrackID, Name: r.Name,
				DiscNumber: r.DiscNumber, TrackNumber: r.TrackNumber,
			})
		}
	}

	now := s.now()
	// A live lease means somebody is fetching this album right now. Checking it
	// before deciding anything is what keeps a polling browser from attempting a
	// write on every tick. It is checked even when this instance has fetching
	// turned off: another replica may have started one before the switch was
	// flipped, and reporting that accurately costs nothing.
	pending := st.Status == catalog.AlbumTrackFetching && now.Sub(st.AttemptedAt) < leaseTTL

	// s.enabled is checked *before* s.due, and that order is load-bearing: a
	// switched-off instance must not resume making requests the moment its cache
	// expires. Guarding here rather than inside start also means the claim — a
	// write — is never even attempted, which is what an operator asking for no
	// unattended traffic actually asked for.
	if !pending && s.enabled && s.due(st, now) {
		s.start(ctx, q, albumID, now)
		// Pending regardless of what start managed to do, and that is not
		// optimism. Every way start can decline — no free slot, a lease somebody
		// else holds, a claim that errored — leaves the row untouched, so this
		// listing is still due and the next view starts it. Reporting those as
		// unavailable would blame Spotify for local backpressure and, worse, would
		// tell a page whose job is to stop polling on unavailable to give up on an
		// album that one more poll would have fetched.
		pending = true
	}

	out := Listing{Tracks: tracks, FetchedAt: st.FetchedAt}
	switch {
	case len(tracks) > 0:
		// A listing read successfully once is worth showing while a refresh runs
		// behind it — and worth showing when no refresh is coming at all, because
		// turning off fetching is not the same as forgetting what is on disk.
		// Withholding it would replace a true answer that is old with no answer.
		// FetchedAt travels with it so the page can say how old, which is the only
		// honesty this case needs: a date claims nothing about freshness.
		out.State = StateReady
	case pending:
		out.State = StatePending
	case !s.enabled:
		// Nothing stored, and this instance will not go and find out. That is the
		// operator's decision, not a Spotify failure, and the page says so in its
		// own words rather than reporting an outage that never happened.
		out.State = StateDisabled
	default:
		// Nothing stored, nothing running, and nothing due: the last attempt
		// failed and its backoff has not elapsed. The page must not read that as
		// "this album has no tracks you have missed".
		out.State = StateUnavailable
	}
	return out, nil
}

// due reports whether a fetch should be started now.
//
// It deliberately knows nothing about s.enabled. Whether this instance fetches
// at all is a different question from whether this listing is old, and its
// caller asks them in that order.
func (s *Service) due(st catalog.AlbumTrackState, now time.Time) bool {
	switch st.Status {
	case "":
		// Never attempted: the lazy first fill.
		return true
	case catalog.AlbumTrackOK:
		return now.Sub(st.FetchedAt) >= s.ttl
	case catalog.AlbumTrackFailed:
		// Much sooner than the TTL, and deliberately so — see failedRetryAfter.
		return now.Sub(st.AttemptedAt) >= failedRetryAfter
	case catalog.AlbumTrackFetching:
		// The lease has expired: whatever process held it is gone.
		return now.Sub(st.AttemptedAt) >= leaseTTL
	default:
		return false
	}
}

// start begins a detached fetch if it can.
//
// It reports nothing, on purpose. Every way it can decline is a "not yet"
// rather than a "no": none of them writes anything, so due is still true and
// the next view tries again. There is no outcome here the page should be told
// about beyond "keep polling", which its caller says unconditionally.
func (s *Service) start(ctx context.Context, q store.Querier, albumID string, now time.Time) {
	if s.base.Err() != nil {
		// Close has already been called. Claiming a lease this process is about
		// to abandon would strand the album for the whole leaseTTL for nothing.
		// Not the guard that makes Close safe — track does that, atomically —
		// just the one that keeps the common case from being wasteful.
		return
	}
	select {
	case s.slots <- struct{}{}:
	default:
		// Every slot is busy. Refusing is the point: queueing here would be
		// queueing people behind a third party.
		s.log.Debug("album track fetch not started; all slots busy", "album", albumID)
		return
	}
	// The slot goes back unless the fetch goroutine takes ownership of it.
	//
	// A defer rather than a release() on each path: a panic anywhere below —
	// a nil Querier reaching q.QueryRow would do it — would otherwise keep the
	// slot for ever, and four of those stop this process fetching another
	// listing for the rest of its life. Silently, because recovery middleware
	// keeps serving pages.
	started := false
	defer func() {
		if !started {
			<-s.slots
		}
	}()

	claimed, err := s.cat.ClaimAlbumTrackFetch(ctx, q, albumID, now, now.Add(-leaseTTL))
	if err != nil {
		s.log.Warn("could not claim an album track fetch", "album", albumID, logging.Err(err))
		return
	}
	if !claimed {
		// Somebody else holds the lease. A second request would be a wasted one
		// against a quota the whole application shares — but a fetch *is* running,
		// so the page is right to keep polling.
		return
	}

	if !s.track() {
		// Close began between the claim and here. Nothing is stranded that the
		// lease does not already cover: this is the same state a process killed
		// mid-fetch leaves behind, and leaseTTL exists for exactly that.
		s.log.Debug("album track fetch abandoned; shutting down", "album", albumID)
		return
	}
	started = true
	go func() {
		defer s.wg.Done()
		defer func() { <-s.slots }()
		s.fetch(albumID)
	}()
}

// track registers a fetch with the WaitGroup unless Close has begun, reporting
// whether the caller may now spawn it.
//
// The mutex is what makes Close correct rather than merely likely. Without it,
// a handler still inside Listing when the process shuts down races Close two
// ways: commonly Wait sees zero, returns, and the goroutine started a moment
// later is one nothing waits for — which is precisely what Close promises not
// to allow; and rarely the Add raising the counter from zero with a waiter
// already registered panics with "WaitGroup misuse: Add called concurrently
// with Wait", taking the process down on its way out. Both are reachable
// because http.Server.Shutdown returns on timeout with handlers still running.
func (s *Service) track() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing {
		return false
	}
	s.wg.Add(1)
	return true
}

// fetch reads one album's listing and stores it, on a context of its own.
//
// Deliberately not the request's context: that request has already been
// answered, and cancelling when the browser navigated away would mean the
// listing never arrives however many times the page is opened. fetchTimeout
// bounds it instead, and Close cancels it at shutdown.
func (s *Service) fetch(albumID string) {
	ctx, cancel := context.WithTimeout(s.base, fetchTimeout)
	defer cancel()

	items, err := s.sp.AlbumTracks(ctx, albumID, maxPages)
	if err != nil {
		// Every failure lands here, ErrTruncated included — and that one arrives
		// with a partial listing attached. The partial must never reach the write:
		// ReplaceAlbumTracks deletes whatever the incoming set does not contain,
		// so a prefix would delete the tail of a listing that was correct and then
		// mark the result authoritative. This project has hit that trap three
		// times; internal/spotify/library.go's ErrTruncated comment is the record
		// of it. There is no exception clause here on purpose.
		s.record(albumID, err)
		return
	}
	if len(items) == 0 {
		// A 200 carrying no items, or one whose every item had a blank id, both of
		// which spotify.AlbumTracks reports as (nil, nil). Checked here rather than
		// left to ReplaceAlbumTracks' own refusal, because by then the intent would
		// already be "make this album's listing empty" and only an error would stop
		// it; recorded as a failure, the stored listing and its date are untouched.
		s.record(albumID, errEmptyListing)
		return
	}

	rows := make([]catalog.AlbumTrack, 0, len(items))
	for _, it := range items {
		rows = append(rows, catalog.AlbumTrack{
			TrackID: it.ID, Name: it.Name,
			DiscNumber: it.DiscNumber, TrackNumber: it.TrackNumber,
		})
	}
	at := s.now()
	err = s.w.InTx(ctx, func(ctx context.Context, q store.Querier) error {
		if err := s.cat.ReplaceAlbumTracks(ctx, q, albumID, rows); err != nil {
			return err
		}
		// In the same transaction as the listing: the rows and the claim that they
		// are authoritative commit together, so a reader can never see a
		// half-replaced listing marked 'ok'.
		return s.cat.MarkAlbumTracksFetched(ctx, q, albumID, at)
	})
	if err != nil {
		s.record(albumID, err)
		return
	}
	s.log.Debug("stored an album track listing", "album", albumID, "tracks", len(rows))
}

// record writes a failure on its own context, so a fetch cancelled at shutdown
// still leaves the album out of 'fetching' rather than waiting out the lease.
//
// The reason is handed over whole. catalog.FailAlbumTrackFetch bounds it with
// store.Truncate, which cuts on a rune boundary; truncating again here would
// only risk two ellipses and a message cut twice.
func (s *Service) record(albumID string, cause error) {
	s.log.Warn("could not read an album track listing", "album", albumID, logging.Err(cause))

	ctx, cancel := context.WithTimeout(context.WithoutCancel(s.base), recordTimeout)
	defer cancel()
	if err := s.cat.FailAlbumTrackFetch(ctx, s.w.DB(), albumID, s.now(), cause.Error()); err != nil {
		// Nothing further can be done, and nothing is stuck: the lease expires on
		// its own, so the next page view after leaseTTL tries again.
		s.log.Error("could not record an album track failure", "album", albumID, logging.Err(err))
	}
}

// Close cancels every fetch in flight and waits for them. It is safe to call
// more than once, and safe to call while requests are still in Listing.
//
// Bounded by recordTimeout per fetch, because a cancelled fetch still records
// its failure — which is why the composition root must call this *before* it
// closes the database pool. That write goes out on a context of its own
// precisely so shutdown does not lose it, and a closed pool would lose it
// anyway, leaving the album 'fetching' until its lease expires. Deferring
// Close after the pool is opened gets the LIFO order right.
func (s *Service) Close() {
	s.mu.Lock()
	s.closing = true
	s.mu.Unlock()

	s.cancel()
	// Safe now: no wg.Add can follow, because track takes the same mutex and
	// sees closing.
	s.wg.Wait()
}
