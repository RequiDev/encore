// Package artistalbums fills and serves the cached listing of an artist's own
// releases, which is what lets the artist page say "you have heard 4 of this
// artist's 11 albums".
//
// Nothing here is a background loop. A sweep over every artist in a history is
// rejected explicitly, so a discography is read the first time somebody opens
// that artist's page and then kept for the configured TTL. The page request
// itself never waits for Spotify: Discography answers from the database, and
// internal/lazyfetch decides whether a walk is due and runs it detached.
//
// # What is here
//
// The parts a discography does not share with anything else: reading it from
// Spotify, deciding that an empty response is a failure, storing the rows and
// their 'ok' in one transaction, and knowing which album_group counts. The
// lease, the schedule, the concurrency bound and the shutdown ordering are
// internal/lazyfetch's, behind the Fill seam.
//
// One rule here is worth stating because it is exactly where this package and
// its sibling internal/albumtracks differ, and why the shared machinery stops
// where it does. There is no such record as an album with no tracks, so an empty
// track listing is a failure. There *is* such an artist as one who has only
// released singles, so an empty album-group set is an ordinary success. The
// emptiness guard below is on the whole response and never on the filtered
// subset — and a Gate that knew that rule for one caller would be wrong for the
// other.
package artistalbums

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
	// maxPages bounds one artist's walk. Twenty pages of fifty is a thousand
	// releases, which no artist in a personal listening history approaches even
	// counting every appearance; it exists so a paging bug cannot spend the
	// instance's quota on one record.
	//
	// The same page count as albumtracks' and much less headroom, because a
	// discography includes every single, compilation and appears_on credit. That
	// is also why the bound must not be tightened casually: a truncated walk is
	// recorded as a failure and never stored, so a bound below a real artist's
	// release count leaves their panel reading "could not be read" for ever.
	maxPages = 20
	// concurrency is how many discographies this process walks at once. Small on
	// purpose: these start inside page requests, and they draw on the same quota
	// enrichment needs to do its job.
	concurrency = 4
	// leaseTTL is how long a 'fetching' row holds other callers off. Longer than
	// fetchTimeout — lazyfetch.New refuses the pair otherwise — so a live walk
	// never loses its own lease, and short enough that a process killed mid-walk
	// does not strand the artist for long.
	leaseTTL = 3 * time.Minute
	// fetchTimeout bounds one artist's whole walk — every page, every retry and
	// every rate-limit wait inside it. Longer than albumtracks' ninety seconds
	// because this walk is up to twenty sequential requests rather than one.
	fetchTimeout = 120 * time.Second
	// failedRetryAfter is how long a failed discography is left alone. Failures
	// here are timeouts and rate limits, which clear in minutes; making somebody
	// wait out the seven-day TTL would turn one bad minute into a broken panel.
	failedRetryAfter = 15 * time.Minute
	// recordTimeout bounds the write that records a failure, including during
	// shutdown.
	recordTimeout = 5 * time.Second
)

// CountedGroup is the one album_group discography completion counts.
//
// Singles, compilations and appearances are excluded, because "you have heard 4
// of 340 releases" is not a useful sentence. It is a named constant with one
// definition so the service, the API and anything that follows cannot each
// decide the predicate for themselves — and so that a group Spotify adds later
// joins the *excluded* side by default rather than silently entering the
// denominator.
const CountedGroup = catalog.AlbumGroupAlbum

// errEmptyDiscography is a 200 that carried no releases.
//
// An artist is in this catalogue because somebody played a track by them, so
// they have released something. An empty response means the artist is invisible
// to this application's market, or Spotify has withdrawn them. Storing it as a
// success would make the page say "you have played something from every album by
// this artist", which is the exact overclaim this feature exists to avoid.
//
// Note the level this applies at: the *whole* response. A response carrying
// forty singles and no albums is a complete success with an empty counted set,
// and the page has its own words for that.
var errEmptyDiscography = errors.New("artistalbums: spotify returned no releases for this artist")

// State is what the page can say about the discography.
//
// An alias of lazyfetch.Outcome rather than a second declaration: the four words
// are an API contract this endpoint shares with the album tracklist, and one
// definition is what stops them forking.
type State = lazyfetch.Outcome

const (
	// StateReady means a discography is stored and can be reasoned about. It is
	// also the state for an artist whose every release is a single: nothing is
	// counted, and that is an answer rather than an absence.
	StateReady = lazyfetch.OutcomeReady
	// StatePending means nothing is stored yet and a walk is running, or is due
	// and nothing has recorded a reason it should not be.
	StatePending = lazyfetch.OutcomePending
	// StateUnavailable means nothing is stored and the last attempt failed. It is
	// emphatically not "this artist has released nothing".
	StateUnavailable = lazyfetch.OutcomeUnavailable
	// StateDisabled means nothing is stored and this instance will not fetch it,
	// because its operator turned that off.
	StateDisabled = lazyfetch.OutcomeDisabled
)

// Release is one entry of a discography.
type Release struct {
	AlbumID string
	Name    string
	// Group is Spotify's album_group, carried through unchanged. Filtering to
	// CountedGroup is the caller's job, because the caller is also the thing that
	// has to say what it set aside.
	Group            string
	ReleaseDate      *time.Time
	ReleasePrecision string
}

// Discography is what one artist's page is told.
type Discography struct {
	State State
	// Releases is everything Spotify listed, in release order, not just the
	// album-group part and not just the unheard part. Which of them were played
	// is the caller's question to ask, because only the caller knows whose
	// history it is asking about; which of them count is the caller's to apply,
	// because only the caller has to write the sentence naming the rest.
	Releases []Release
	// FetchedAt is when the discography was read. Zero when none has succeeded.
	FetchedAt time.Time
}

// CountedIDs is the album ids discography completion is taken over: the
// CountedGroup ones, in the order they were listed.
//
// The one definition of the filter. A caller that wants to know which of these
// the listener has played asks with exactly this set, so the numerator and the
// denominator can never be taken over different populations.
func (d Discography) CountedIDs() []string {
	out := make([]string, 0, len(d.Releases))
	for _, r := range d.Releases {
		if r.Group == CountedGroup {
			out = append(out, r.AlbumID)
		}
	}
	return out
}

// Fetcher is the slice of the Spotify client this package uses.
type Fetcher interface {
	ArtistAlbums(ctx context.Context, artistID string, maxPages int) ([]spotify.ArtistAlbum, error)
}

// Store is the slice of the catalogue repository this package uses. An interface
// so the fill above can be exercised without a database.
type Store interface {
	ArtistAlbumState(ctx context.Context, q store.Querier, artistID string) (catalog.ArtistAlbumState, error)
	ArtistAlbums(ctx context.Context, q store.Querier, artistID string) ([]catalog.ArtistAlbum, error)
	ClaimArtistAlbumFetch(ctx context.Context, q store.Querier, artistID string, now, leaseCutoff time.Time) (bool, error)
	ReplaceArtistAlbums(ctx context.Context, q store.Querier, artistID string, items []catalog.ArtistAlbum) error
	MarkArtistAlbumsFetched(ctx context.Context, q store.Querier, artistID string, at time.Time) error
	FailArtistAlbumFetch(ctx context.Context, q store.Querier, artistID string, at time.Time, reason string) error
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
// package that names pgx.
type StoreWriter struct{ Store *store.Store }

// InTx runs fn inside one transaction.
func (w StoreWriter) InTx(ctx context.Context, fn func(ctx context.Context, q store.Querier) error) error {
	return w.Store.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error { return fn(ctx, tx) })
}

// DB returns the pool as a Querier.
func (w StoreWriter) DB() store.Querier { return w.Store.DB() }

// leases adapts the catalogue's two lease statements to lazyfetch.Leases. It is
// the whole of what the Gate knows about artist_album_fetches.
type leases struct{ cat Store }

func (l leases) Claim(
	ctx context.Context, q store.Querier, artistID string, now, leaseCutoff time.Time,
) (bool, error) {
	return l.cat.ClaimArtistAlbumFetch(ctx, q, artistID, now, leaseCutoff)
}

func (l leases) Fail(
	ctx context.Context, q store.Querier, artistID string, at time.Time, reason string,
) error {
	return l.cat.FailArtistAlbumFetch(ctx, q, artistID, at, reason)
}

// Deps is everything the service needs.
type Deps struct {
	Catalog Store
	Spotify Fetcher
	Writer  Writer
	Logger  *slog.Logger
	// Now is the clock. Tests replace it; production leaves it nil.
	Now func() time.Time
}

// Service fills and serves artist discographies.
type Service struct {
	cat  Store
	sp   Fetcher
	w    Writer
	log  *slog.Logger
	now  func() time.Time
	gate *lazyfetch.Gate
}

// New validates the dependencies and builds the service.
//
// The caller owns Close and must call it during shutdown, before closing the
// database pool: walks run detached from any request, so nothing else will ever
// wait for them, and a walk cancelled at shutdown still needs the pool to record
// that it failed. Constructing this after the pool and deferring Close
// immediately puts both in the right order.
func New(cfg config.ArtistAlbums, deps Deps) (*Service, error) {
	switch {
	case deps.Catalog == nil:
		return nil, errors.New("artistalbums: catalog repository is required")
	case deps.Spotify == nil:
		return nil, errors.New("artistalbums: spotify client is required")
	case deps.Writer == nil:
		return nil, errors.New("artistalbums: writer is required")
	case cfg.TTL <= 0:
		return nil, errors.New("artistalbums: a positive TTL is required")
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
		log: lg.With("component", "artistalbums"),
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
		Subject: "artist",
		Logger:  s.log,
		Now:     now,
	})
	if err != nil {
		return nil, err
	}
	s.gate = gate
	if !cfg.Enabled {
		// Said once, at startup, rather than on every page view: an operator who
		// wonders why the artist page reports this as turned off can find it here
		// and in the configuration line the process logs beside it.
		lg.Info("artist discographies are turned off; this instance will not ask spotify " +
			"what an artist has released. Discographies already cached are still shown.")
	}
	return s, nil
}

// Discography returns the stored discography for one artist, and starts a
// refresh when one is due.
//
// It never blocks on Spotify and it never fails because Spotify did: a
// third-party outage is a state the page renders, not a 500 it shows.
func (s *Service) Discography(ctx context.Context, q store.Querier, artistID string) (Discography, error) {
	st, err := s.cat.ArtistAlbumState(ctx, q, artistID)
	if err != nil {
		return Discography{}, err
	}

	var releases []Release
	if !st.FetchedAt.IsZero() {
		rows, err := s.cat.ArtistAlbums(ctx, q, artistID)
		if err != nil {
			return Discography{}, err
		}
		releases = make([]Release, 0, len(rows))
		for _, r := range rows {
			releases = append(releases, Release{
				AlbumID: r.AlbumID, Name: r.Name, Group: r.Group,
				ReleaseDate: r.ReleaseDate, ReleasePrecision: r.ReleasePrecision,
			})
		}
	}

	out := Discography{Releases: releases, FetchedAt: st.FetchedAt}
	// The stored predicate is !FetchedAt.IsZero(), deliberately, and not
	// len(releases) > 0 — this is the one argument this service passes the Gate
	// that differs in substance from its sibling's. A successful read always
	// stores at least one row, because an empty response is recorded as a
	// failure, so a successful read with no *counted* releases is an artist whose
	// every release is a single. That is ready, and it has its own copy. Passing
	// a row count here would be identical today and would quietly become wrong
	// the moment anything filters before this point.
	out.State = s.gate.Resolve(ctx, q, artistID, lazyfetch.State{
		Status:      st.Status,
		FetchedAt:   st.FetchedAt,
		AttemptedAt: st.AttemptedAt,
		Attempts:    st.Attempts,
	}, !st.FetchedAt.IsZero())
	return out, nil
}

// fill reads one artist's discography and stores it. It is the Gate's Fill:
// whether and when it runs is the Gate's decision, and everything it does is
// this package's.
func (s *Service) fill(ctx context.Context, artistID string) error {
	items, err := s.sp.ArtistAlbums(ctx, artistID, maxPages)
	if err != nil {
		// Every failure lands here, ErrTruncated included — and that one arrives
		// with a partial discography attached. The partial must never reach the
		// write: ReplaceArtistAlbums deletes whatever the incoming set does not
		// contain, so a prefix would delete the tail of a listing that was correct
		// and then mark the result authoritative. This project has hit that trap
		// three times; internal/spotify/library.go's ErrTruncated comment is the
		// record of it. There is no exception clause here on purpose.
		return err
	}
	if len(items) == 0 {
		// A 200 carrying no items, or one whose every item had a blank id, both of
		// which spotify.ArtistAlbums reports as (nil, nil). Checked here rather
		// than left to ReplaceArtistAlbums' own refusal, because by then the intent
		// would already be "make this artist's discography empty" and only an error
		// would stop it; returned as a failure, the stored listing and its date are
		// untouched.
		//
		// This is the *whole* response, and deliberately not the CountedGroup
		// subset. An artist whose every release is a single has an empty counted
		// set and a perfectly good discography, and recording that as a failure
		// would tell them Spotify would not answer about an artist Spotify answered
		// about at length.
		return errEmptyDiscography
	}

	rows := make([]catalog.ArtistAlbum, 0, len(items))
	for i, it := range items {
		rows = append(rows, catalog.ArtistAlbum{
			AlbumID: it.ID, Name: it.Name, Group: it.Group,
			ReleaseDate: it.ReleaseDate, ReleasePrecision: it.ReleasePrecision,
			// The index in the walk, kept only to break ties in the read order.
			Position: i,
		})
	}
	at := s.now()
	return s.w.InTx(ctx, func(ctx context.Context, q store.Querier) error {
		if err := s.cat.ReplaceArtistAlbums(ctx, q, artistID, rows); err != nil {
			return err
		}
		// In the same transaction as the rows: the discography and the claim that
		// it is authoritative commit together, so a reader can never see a
		// half-replaced listing marked 'ok'.
		return s.cat.MarkArtistAlbumsFetched(ctx, q, artistID, at)
	})
}

// Close cancels every walk in flight and waits for them. It is safe to call more
// than once, and safe to call while requests are still in Discography.
//
// The ordering that makes it correct lives in lazyfetch.Gate.Close; this is the
// delegation, and TestCloseEndsAWalkInFlight is what proves the delegation is
// actually wired. It must be called *before* the database pool closes: a
// cancelled walk still records its failure, and a closed pool would lose that
// write, leaving the artist 'fetching' until its lease expires.
func (s *Service) Close() { s.gate.Close() }
