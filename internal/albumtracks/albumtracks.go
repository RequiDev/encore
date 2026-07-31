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
// The lifecycle behind that — the lease, the TTL and failure backoff, the
// bounded slot channel, the detached goroutine and the shutdown — is
// internal/lazyfetch, shared with the artist page. What stays here is what a
// *track listing* means: how it is paged, when it counts as empty, and the one
// transaction that stores it.
package albumtracks

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/RequiDev/encore/internal/config"
	"github.com/RequiDev/encore/internal/lazyfetch"
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
//
// An alias of lazyfetch.Outcome rather than a second declaration: the four words
// are an API contract two endpoints share, and one definition is what stops them
// forking. The names here are unchanged, so internal/httpapi is untouched.
type State = lazyfetch.Outcome

const (
	StateReady       = lazyfetch.OutcomeReady
	StatePending     = lazyfetch.OutcomePending
	StateUnavailable = lazyfetch.OutcomeUnavailable
	StateDisabled    = lazyfetch.OutcomeDisabled
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
	cat  Store
	sp   Fetcher
	w    Writer
	log  *slog.Logger
	now  func() time.Time
	gate *lazyfetch.Gate
}

// leases adapts the catalogue's two lease statements to lazyfetch.Leases. It is
// the whole of what the Gate knows about album_track_fetches.
type leases struct{ cat Store }

func (l leases) Claim(
	ctx context.Context, q store.Querier, albumID string, now, leaseCutoff time.Time,
) (bool, error) {
	return l.cat.ClaimAlbumTrackFetch(ctx, q, albumID, now, leaseCutoff)
}

func (l leases) Fail(
	ctx context.Context, q store.Querier, albumID string, at time.Time, reason string,
) error {
	return l.cat.FailAlbumTrackFetch(ctx, q, albumID, at, reason)
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
	s := &Service{
		cat: deps.Catalog,
		sp:  deps.Spotify,
		w:   deps.Writer,
		log: lg.With("component", "albumtracks"),
		now: now,
	}
	gate, err := lazyfetch.New(lazyfetch.Policy{
		Enabled:          cfg.Enabled,
		TTL:              cfg.TTL,
		LeaseTTL:         leaseTTL,
		FailedRetryAfter: failedRetryAfter,
		FetchTimeout:     fetchTimeout,
		RecordTimeout:    recordTimeout,
		Concurrency:      concurrency,
	}, lazyfetch.Deps{
		Leases:  leases{cat: deps.Catalog},
		Fill:    s.fill,
		DB:      deps.Writer.DB,
		Subject: "album",
		Logger:  s.log,
		Now:     now,
	})
	if err != nil {
		return nil, err
	}
	s.gate = gate
	if !cfg.Enabled {
		// Said once, at startup, rather than on every page view: an operator who
		// wonders why the album page reports this as turned off can find it here
		// and in the configuration line the process logs beside it.
		lg.Info("album track listings are turned off; this instance will not ask spotify " +
			"what is on an album. Listings already cached are still shown.")
	}
	return s, nil
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

	out := Listing{Tracks: tracks, FetchedAt: st.FetchedAt}
	// len(tracks) > 0, exactly as before the extraction. A successful read of an
	// album always stores at least one row, because a 200 carrying no tracks is
	// recorded as a failure, so this and st.FetchedAt agree for every state this
	// package can produce — but it is the predicate this service has always used
	// and the refactor changes no behaviour.
	out.State = s.gate.Resolve(ctx, q, albumID, lazyfetch.State{
		Status:      st.Status,
		FetchedAt:   st.FetchedAt,
		AttemptedAt: st.AttemptedAt,
		Attempts:    st.Attempts,
	}, len(tracks) > 0)
	return out, nil
}

// fill reads one album's listing and stores it. It is the Gate's Fill: whether
// and when it runs is the Gate's decision, and everything it does is this
// package's.
func (s *Service) fill(ctx context.Context, albumID string) error {
	items, err := s.sp.AlbumTracks(ctx, albumID, maxPages)
	if err != nil {
		// Every failure lands here, ErrTruncated included — and that one arrives
		// with a partial listing attached. The partial must never reach the write:
		// ReplaceAlbumTracks deletes whatever the incoming set does not contain, so
		// a prefix would delete the tail of a listing that was correct and then
		// mark the result authoritative. This project has hit that trap three
		// times; internal/spotify/library.go's ErrTruncated comment is the record
		// of it. There is no exception clause here on purpose.
		return err
	}
	if len(items) == 0 {
		// A 200 carrying no items, or one whose every item had a blank id, both of
		// which spotify.AlbumTracks reports as (nil, nil). Checked here rather than
		// left to ReplaceAlbumTracks' own refusal, because by then the intent would
		// already be "make this album's listing empty" and only an error would stop
		// it; returned as a failure, the stored listing and its date are untouched.
		return errEmptyListing
	}

	rows := make([]catalog.AlbumTrack, 0, len(items))
	for _, it := range items {
		rows = append(rows, catalog.AlbumTrack{
			TrackID: it.ID, Name: it.Name,
			DiscNumber: it.DiscNumber, TrackNumber: it.TrackNumber,
		})
	}
	at := s.now()
	return s.w.InTx(ctx, func(ctx context.Context, q store.Querier) error {
		if err := s.cat.ReplaceAlbumTracks(ctx, q, albumID, rows); err != nil {
			return err
		}
		// In the same transaction as the listing: the rows and the claim that they
		// are authoritative commit together, so a reader can never see a
		// half-replaced listing marked 'ok'.
		return s.cat.MarkAlbumTracksFetched(ctx, q, albumID, at)
	})
}

// Close cancels every fetch in flight and waits for them. It is safe to call
// more than once, and safe to call while requests are still in Listing.
//
// The ordering that makes it correct lives in lazyfetch.Gate.Close; this is the
// delegation, and TestCloseEndsAFetchInFlight is what proves the delegation is
// actually wired.
func (s *Service) Close() { s.gate.Close() }
