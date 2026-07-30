// Package library enumerates what a listener has saved, who they follow, and
// Spotify's own ranking of their top artists and tracks, and reconciles or
// captures each against a local table, once a day rather than on every tick.
//
// Spotify exposes no "what changed" feed for saved tracks, saved albums or
// followed artists — only a full listing — so every run reads all three
// endpoints and reconciles the whole set against what is already stored. The
// same run also captures Spotify's own top-artists and top-tracks rankings,
// one call per (kind, time range) for six more requests total, as a latest-
// capture snapshot rather than a reconciliation — see
// internal/store/library/topsnapshots.go for why "absent" means past the end
// of a rank-ordered list there rather than "id not in this set." The tables
// both write to are internal/store/library's; the writing half lives here,
// alongside the scheduling that decides which account runs when and the scope
// checks that keep an account from spending a request on an endpoint it never
// granted.
//
// Nothing in this package is read by anything user-facing yet. The statistics
// and the page that surface saved-but-never-played tracks, followed-but-
// dormant artists, and how Spotify's own top rankings differ from Encore's
// are a later phase; this one only has to get the tables right.
package library

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"slices"
	stdsync "sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/RequiDev/encore/internal/config"
	"github.com/RequiDev/encore/internal/domain"
	"github.com/RequiDev/encore/internal/logging"
	"github.com/RequiDev/encore/internal/spotify"
	"github.com/RequiDev/encore/internal/store"
	"github.com/RequiDev/encore/internal/store/accounts"
	"github.com/RequiDev/encore/internal/store/catalog"
	libstore "github.com/RequiDev/encore/internal/store/library"
)

// Scopes the three library endpoints this worker reads require. Both were
// added to DefaultScopes() in Phase 2a; an account connected before that
// shipped carries neither, and that is the ordinary case this worker has to
// expect forever, not a fault to repair.
const (
	scopeLibraryRead = "user-library-read"
	scopeFollowRead  = "user-follow-read"
)

// scopeTopRead is user-top-read, required by the six top-items enumerations
// this worker also runs. It was added to DefaultScopes() alongside
// scopeLibraryRead and scopeFollowRead, but it is checked on its own,
// immediately before those six requests rather than folded into the check
// above, because the two halves of one account's run are independent: a
// grant carrying the library scopes but not this one must still get its
// library enumerated, spending zero requests on top items, and the check
// above must keep gating the library three on its own two scopes regardless
// of whether this one is present. Nothing about the library reconciliation
// should ever depend on a scope it does not read.
const scopeTopRead = "user-top-read"

// ErrNotConnected reports that the user has no Spotify grant at all, so there
// is nothing to enumerate. It mirrors internal/sync's sentinel of the same
// name for the same reason: the account was deleted, or disconnected, between
// being listed as due and being processed.
var ErrNotConnected = errors.New("library: the account is not connected to Spotify")

// Defaults applied when configuration carries a nonsensical value. config.Load
// should have already caught these; a worker handed a zero interval must still
// not spin.
const (
	defaultInterval = 24 * time.Hour
	defaultMaxPages = 200
)

// tickJitter is the fraction of the interval each delay after the first is
// randomised by, exactly as internal/sync/poller.go's constant of the same
// name: several worker containers started together must not keep polling on
// the same second forever.
const tickJitter = 0.2

// accountsPerWorker and maxAccountsPerTick bound one tick's work the same way
// the recently-played poller's do: accounts are handed out least-recently
// enumerated first, so anything left over is simply picked up by the next
// tick rather than starved.
const (
	accountsPerWorker  = 50
	maxAccountsPerTick = 500
)

// SpotifyAPI is the part of *spotify.Client this worker uses.
//
// Declaring it here documents exactly how much of the client the worker
// depends on, and lets the scheduling and reconciliation behaviour be
// exercised with a fake rather than a network.
type SpotifyAPI interface {
	SavedTracks(ctx context.Context, accessToken string, maxPages int) ([]spotify.SavedTrack, error)
	SavedAlbums(ctx context.Context, accessToken string, maxPages int) ([]spotify.SavedAlbum, error)
	FollowedArtists(ctx context.Context, accessToken string, maxPages int) ([]spotify.Artist, error)
	TopArtists(ctx context.Context, accessToken string, tr spotify.TopTimeRange, limit int) ([]spotify.Artist, error)
	TopTracks(ctx context.Context, accessToken string, tr spotify.TopTimeRange, limit int) ([]spotify.Track, error)
}

// topLimit asks TopArtists/TopTracks for the largest page they will ever
// return. Unlike maxPages above, there is no cap to configure here: one page
// of 50 is the whole picture these two calls exist to capture, by Spotify's
// own design — see topitems.go's comment on why pagination was deliberately
// left out of that client method.
const topLimit = 50

// topTimeRanges is every rolling window this worker captures, for both
// top-item kinds: three ranges times two kinds is the six extra requests one
// account's run costs. sync walks it once per kind, in this order, so a 403
// or a failure on any one of the six stops exactly there — the same
// abandon-the-run contract the three library endpoints already keep.
var topTimeRanges = []spotify.TopTimeRange{spotify.TopShortTerm, spotify.TopMediumTerm, spotify.TopLongTerm}

// Tokens supplies a usable Spotify access token for one account, refreshing
// and persisting it when necessary.
//
// This is exactly *sync.Poller's exported AccessToken method: the refresh
// dance, including parking an account as needs_reauth when Spotify has
// revoked the grant, already exists there and belongs to recently-played sync,
// which cannot function without its own scope. Declaring the dependency as an
// interface rather than importing internal/sync directly keeps this package
// free to be tested with a fake, and free of a dependency cycle risk should
// internal/sync ever want the reverse.
type Tokens interface {
	AccessToken(ctx context.Context, userID uuid.UUID) (string, error)
}

// Deps are the collaborators a Worker needs.
type Deps struct {
	Store    *store.Store
	Accounts *accounts.Repo
	Catalog  *catalog.Repo
	Library  *libstore.Repo
	Spotify  SpotifyAPI
	Tokens   Tokens
	Logger   *slog.Logger
	// Now is injectable so tests can control timestamps without sleeping.
	Now func() time.Time
}

// Worker enumerates the saved tracks, saved albums, followed artists, and
// (scope permitting) top artists and top tracks of every connected account,
// once an interval, and reconciles or captures each against its local table.
//
// It holds no durable state: which accounts are due lives in
// spotify_credentials.library_synced_at, so a Worker can be killed at any
// instant and another process, or the next tick, simply asks the database
// again.
type Worker struct {
	cfg config.Library
	dep Deps
	now func() time.Time
	log *slog.Logger

	// rnd supplies the tick jitter in [0,1). Injectable so a test can make the
	// schedule deterministic.
	rnd func() float64
}

// New builds a Worker. Every collaborator it names is required; the logger and
// the clock default to sensible values.
func New(cfg config.Library, deps Deps) (*Worker, error) {
	if deps.Store == nil {
		return nil, errors.New("library: a store is required")
	}
	if deps.Accounts == nil || deps.Accounts.Credentials == nil {
		return nil, errors.New("library: the credentials repository is required")
	}
	if deps.Catalog == nil {
		return nil, errors.New("library: the catalog repository is required")
	}
	if deps.Library == nil {
		return nil, errors.New("library: the library repository is required")
	}
	if deps.Spotify == nil {
		return nil, errors.New("library: a Spotify client is required")
	}
	if deps.Tokens == nil {
		return nil, errors.New("library: a token source is required")
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	if deps.Now == nil {
		deps.Now = time.Now
	}
	if cfg.Interval <= 0 {
		cfg.Interval = defaultInterval
	}
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = 1
	}
	if cfg.MaxPages <= 0 {
		cfg.MaxPages = defaultMaxPages
	}

	return &Worker{
		cfg: cfg,
		dep: deps,
		now: deps.Now,
		log: deps.Logger.With("component", "library"),
		rnd: rand.Float64,
	}, nil
}

// Run enumerates every due account's library, forever, until ctx is cancelled.
func (w *Worker) Run(ctx context.Context) error {
	if !w.cfg.Enabled {
		w.log.Info("library sync is disabled")
		return nil
	}
	w.log.Info("library sync started",
		"interval", w.cfg.Interval.String(),
		"concurrency", w.cfg.Concurrency,
		"max_pages", w.cfg.MaxPages)

	// The first delay is drawn from the whole interval rather than jittered
	// around it, which is what actually spreads a fleet that all started at
	// once; subsequent delays only keep them from converging again.
	timer := time.NewTimer(w.firstDelay())
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-timer.C:
		}

		if _, err := w.RunOnce(ctx); err != nil && ctx.Err() == nil {
			// Listing the work failed, which is an infrastructure problem
			// rather than an account problem: log it and wait for the next
			// tick instead of spinning against a database that is down.
			w.log.Error("library sync tick failed", logging.Err(err))
		}
		timer.Reset(w.nextDelay())
	}
}

// RunOnce enumerates every account that is currently due and reports how many
// were actually processed.
//
// It is exported so a worker supervisor, or a test, can drive one tick without
// owning the schedule.
func (w *Worker) RunOnce(ctx context.Context) (int, error) {
	// Due means "not enumerated within the last interval". Accounts that have
	// never been enumerated sort first, so a freshly connected one becomes
	// browsable without waiting behind everybody else.
	due, err := w.dep.Accounts.Credentials.ListDueForLibrarySync(
		ctx, w.dep.Store.DB(), w.now().Add(-w.cfg.Interval), w.batchLimit())
	if err != nil {
		return 0, fmt.Errorf("list accounts due for library sync: %w", err)
	}
	if len(due) == 0 {
		return 0, nil
	}

	var (
		processed atomic.Int64
		wg        stdsync.WaitGroup
		// sem admits cfg.Concurrency accounts at a time, so one tick cannot
		// present the whole instance's saved libraries to Spotify at once.
		sem = make(chan struct{}, w.cfg.Concurrency)
	)

	// No shared error group: one account's failure must never cancel the work
	// of the others, so each enumeration is isolated and reports itself.
dispatch:
	for _, userID := range due {
		select {
		case <-ctx.Done():
			break dispatch
		case sem <- struct{}{}:
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			if w.tick(ctx, userID) {
				processed.Add(1)
			}
		}()
	}
	wg.Wait()

	return int(processed.Load()), nil
}

// tick enumerates one account and reports whether the tick counted as having
// run it.
//
// It never returns an error: everything that can go wrong with one grant is
// logged and contained here, because one broken or under-scoped connection
// must not cost every other listener's library sync.
func (w *Worker) tick(ctx context.Context, userID uuid.UUID) bool {
	log := w.log.With("user", userID.String())

	err := w.SyncAccount(ctx, userID)
	switch {
	case err == nil:
		return true
	case errors.Is(err, ErrNotConnected):
		// The grant was deleted between the listing and the enumeration.
		// Nothing to do.
		log.Debug("account is no longer connected to Spotify")
		return false
	case ctx.Err() != nil:
		// Shutting down. The next tick's listing picks the account up again,
		// and library_synced_at has not moved, so nothing is lost.
		log.Debug("library sync interrupted by shutdown")
		return false
	default:
		log.Error("library sync failed", logging.Err(err))
		return true
	}
}

// SyncAccount enumerates and reconciles one account's library.
//
// It is exported so a test, or a future manual trigger, can drive a single
// account without owning the schedule.
func (w *Worker) SyncAccount(ctx context.Context, userID uuid.UUID) error {
	if userID == uuid.Nil {
		return fmt.Errorf("%w: library sync needs a user", domain.ErrValidation)
	}

	creds, err := w.dep.Accounts.Credentials.Get(ctx, w.dep.Store.DB(), userID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return ErrNotConnected
		}
		return fmt.Errorf("load spotify credentials: %w", err)
	}
	return w.sync(ctx, userID, creds)
}

// sync is SyncAccount without the credentials load, so a test can drive it
// directly with an in-memory grant and no database.
func (w *Worker) sync(ctx context.Context, userID uuid.UUID, creds domain.SpotifyCredentials) error {
	log := w.log.With("user", userID.String())

	// Checked before any request is made, and before a token is even asked
	// for. An account that predates Phase 2a's consent change carries neither
	// scope, and that is every account that connected before this shipped —
	// discovering it by a 403 would waste one request per account per day, for
	// ever, rather than the one check below.
	if !creds.HasScope(scopeLibraryRead) || !creds.HasScope(scopeFollowRead) {
		log.Debug("account has not granted the library or follow scope; skipping")
		return nil
	}

	token, err := w.dep.Tokens.AccessToken(ctx, userID)
	if err != nil {
		return fmt.Errorf("access token: %w", err)
	}

	tracks, err := w.dep.Spotify.SavedTracks(ctx, token, w.cfg.MaxPages)
	if forbidden(err) {
		// A 403 here, despite both scopes being present in the stored grant,
		// means Spotify itself disagrees — see the comment on forbidden
		// below. It is not the account's recently-played scope, so the
		// account is not touched: no retry, no SyncState, no watermark.
		log.Warn("spotify refused saved tracks despite the granted scope", logging.Err(err))
		return nil
	}
	if truncated(err) {
		w.warnTruncated(log, "saved tracks")
		return nil
	}
	if err != nil {
		return fmt.Errorf("enumerate saved tracks: %w", err)
	}

	albums, err := w.dep.Spotify.SavedAlbums(ctx, token, w.cfg.MaxPages)
	if forbidden(err) {
		log.Warn("spotify refused saved albums despite the granted scope", logging.Err(err))
		return nil
	}
	if truncated(err) {
		w.warnTruncated(log, "saved albums")
		return nil
	}
	if err != nil {
		return fmt.Errorf("enumerate saved albums: %w", err)
	}

	artists, err := w.dep.Spotify.FollowedArtists(ctx, token, w.cfg.MaxPages)
	if forbidden(err) {
		log.Warn("spotify refused followed artists despite the granted scope", logging.Err(err))
		return nil
	}
	if truncated(err) {
		w.warnTruncated(log, "followed artists")
		return nil
	}
	if err != nil {
		return fmt.Errorf("enumerate followed artists: %w", err)
	}

	// The six top-item requests are gated on their own scope, separately from
	// the library check above: an account that granted the library scopes but
	// not this one must still reach the commit below with its library fully
	// enumerated, having spent zero requests here. Nothing accumulated is
	// reported as a snapshot in that case — snapshots stay nil, and commit's
	// loop over them simply runs zero times.
	var (
		topSnapshots  []topSnapshot
		topArtistsAll []spotify.Artist
		topTracksAll  []spotify.Track
	)
	if creds.HasScope(scopeTopRead) {
		for _, tr := range topTimeRanges {
			got, err := w.dep.Spotify.TopArtists(ctx, token, tr, topLimit)
			if forbidden(err) {
				log.Warn("spotify refused top artists despite the granted scope",
					"time_range", string(tr), logging.Err(err))
				return nil
			}
			if err != nil {
				return fmt.Errorf("enumerate top artists (%s): %w", tr, err)
			}
			topSnapshots = append(topSnapshots, topSnapshot{
				kind: topKindArtist, timeRange: string(tr), entityIDs: topArtistIDs(got),
			})
			topArtistsAll = append(topArtistsAll, got...)
		}
		for _, tr := range topTimeRanges {
			got, err := w.dep.Spotify.TopTracks(ctx, token, tr, topLimit)
			if forbidden(err) {
				log.Warn("spotify refused top tracks despite the granted scope",
					"time_range", string(tr), logging.Err(err))
				return nil
			}
			if err != nil {
				return fmt.Errorf("enumerate top tracks (%s): %w", tr, err)
			}
			topSnapshots = append(topSnapshots, topSnapshot{
				kind: topKindTrack, timeRange: string(tr), entityIDs: topTrackIDs(got),
			})
			topTracksAll = append(topTracksAll, got...)
		}
	} else {
		log.Debug("account has not granted the top-items scope; skipping the six top-item requests")
	}

	b := build(tracks, albums, artists, topTracksAll, topArtistsAll)
	b.topSnapshots = topSnapshots
	if err := w.commit(ctx, userID, b); err != nil {
		return fmt.Errorf("commit library sync: %w", err)
	}
	return nil
}

// commit writes one account's reconciled library and captured top snapshots
// in a single transaction and advances its watermark.
//
// Everything here is in one Store.InTx: the catalogue rows the enumeration
// referenced, the three library Replace* calls, up to six ReplaceTopSnapshot
// calls, and the library_synced_at update. If any step fails the whole run
// commits nothing, so a partial reconciliation never presents a half-empty
// library — or a half-written top ranking — as fact, and never advances the
// watermark past data that was not actually stored.
func (w *Worker) commit(ctx context.Context, userID uuid.UUID, b batch) error {
	// Read once and reused for both the six snapshots and the watermark below,
	// so a single run reports one consistent instant for "when this was
	// captured" and "when this account was last synced", rather than two
	// clock reads that could straddle a tick boundary.
	now := w.now()

	return w.dep.Store.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		// Catalogue detail first, exactly as internal/sync/ingest.go's commit
		// does: the tracks and albums this run already has full metadata for
		// are upserted as resolved, which also mints a pending row for every
		// album and artist id they reference but do not carry detail for.
		// Followed artists arrive with full detail of their own, from the
		// endpoint dedicated to them, so they are upserted directly rather
		// than merely registered.
		if err := w.dep.Catalog.UpsertTracks(ctx, tx, b.tracks); err != nil {
			return err
		}
		for _, t := range b.tracks {
			if err := w.dep.Catalog.ReplaceTrackArtists(ctx, tx, t.ID, t.ArtistIDs); err != nil {
				return err
			}
		}
		if err := w.dep.Catalog.UpsertAlbums(ctx, tx, b.albums); err != nil {
			return err
		}
		for _, a := range b.albums {
			if err := w.dep.Catalog.ReplaceAlbumArtists(ctx, tx, a.ID, a.ArtistIDs); err != nil {
				return err
			}
		}
		if err := w.dep.Catalog.UpsertArtists(ctx, tx, b.artists); err != nil {
			return err
		}

		if err := w.dep.Library.ReplaceSavedTracks(ctx, tx, userID, b.trackItems); err != nil {
			return err
		}
		if err := w.dep.Library.ReplaceSavedAlbums(ctx, tx, userID, b.albumItems); err != nil {
			return err
		}
		if err := w.dep.Library.ReplaceFollowedArtists(ctx, tx, userID, b.artistIDs); err != nil {
			return err
		}

		// Zero, when the account's grant lacks scopeTopRead: b.topSnapshots is
		// only ever populated alongside the six requests in sync, never on its
		// own, so an account skipped for top items writes none of these and
		// still reaches the watermark update below.
		for _, snap := range b.topSnapshots {
			if err := w.dep.Library.ReplaceTopSnapshot(
				ctx, tx, userID, snap.kind, snap.timeRange, snap.entityIDs, now,
			); err != nil {
				return err
			}
		}

		return w.dep.Accounts.Credentials.MarkLibrarySynced(ctx, tx, userID, now)
	})
}

// batchLimit is how many accounts one tick will take on.
func (w *Worker) batchLimit() int {
	n := w.cfg.Concurrency * accountsPerWorker
	if n > maxAccountsPerTick {
		return maxAccountsPerTick
	}
	return n
}

// firstDelay spreads the first tick of freshly started processes across a
// whole interval.
func (w *Worker) firstDelay() time.Duration {
	return time.Duration(w.rnd() * float64(w.cfg.Interval))
}

// nextDelay is the configured interval with symmetric jitter applied, so
// processes that happen to align drift apart again instead of staying in
// step.
func (w *Worker) nextDelay() time.Duration {
	spread := float64(w.cfg.Interval) * tickJitter
	d := float64(w.cfg.Interval) - spread/2 + w.rnd()*spread
	if d < float64(time.Second) {
		// A pathologically small interval must still not become a busy loop.
		return time.Second
	}
	return time.Duration(d)
}

// forbidden reports a 403: the token is valid but does not carry the scope the
// endpoint needs.
//
// Unlike recently-played sync's forbidden (internal/sync/account.go), which
// parks the account because that endpoint's scope is one Encore cannot
// function without, a 403 from any of these endpoints — the three library
// ones or the six top-item ones — means only that the listener never granted
// an optional read scope, the ordinary state of every account connected
// before Phase 2a added user-library-read, user-follow-read and
// user-top-read to DefaultScopes(). It must not reach MarkNeedsReauth, which
// would stop ingesting a listening history that still reads perfectly, and it
// must not be retried, because a scope failure spends quota to fail
// identically every time.
func forbidden(err error) bool {
	apiErr, ok := spotify.AsAPIError(err)
	return ok && apiErr.IsForbidden()
}

// truncated reports that an enumeration stopped at its page cap while
// Spotify still had more to return (spotify.ErrTruncated), rather than
// exhausting the listing.
//
// Unlike forbidden, this says nothing about the account's grant: it means the
// result just read is a prefix, not the whole of it, and reconciling it would
// treat everything past the cap as removed. sync abandons the run instead —
// see warnTruncated for why that, rather than upserting the prefix without
// deleting, is the choice made here.
func truncated(err error) bool {
	return errors.Is(err, spotify.ErrTruncated)
}

// warnTruncated records that one endpoint's enumeration was truncated and the
// run is being abandoned as a result.
//
// Skipping the whole run, rather than upserting the partial set without
// deleting, is chosen because a library that large is already unusual enough
// that raising ENCORE_LIBRARY_SYNC_MAX_PAGES once is simpler than reasoning
// about a table that is correct on adds but stale on removals until some
// future run finally clears the cap — and the account is not left worse off:
// nothing here is deleted or advanced, so today's stored library keeps
// reading exactly as it did before this run started, and tomorrow's run
// tries again.
func (w *Worker) warnTruncated(log *slog.Logger, endpoint string) {
	log.Warn("library enumeration hit the page cap with more remaining; skipping this sync "+
		"rather than deleting what the cap did not reach",
		"endpoint", endpoint, "max_pages", w.cfg.MaxPages)
}

// topSnapshot is one (kind, time range) ranking ready to write with
// libstore.Repo.ReplaceTopSnapshot, kept ready-to-write rather than raw
// Spotify types so commit's loop over the six of them needs no further
// conversion.
type topSnapshot struct {
	// kind and timeRange are ReplaceTopSnapshot's own literal parameters:
	// libstore has no typed constants for them, so these come from
	// topKindArtist/topKindTrack below and spotify.TopTimeRange stringified.
	kind, timeRange string
	// entityIDs is in Spotify's own rank order: index 0 is rank 1.
	entityIDs []string
}

// topKindArtist and topKindTrack are the only two values
// libstore.Repo.ReplaceTopSnapshot's kind parameter accepts (see the CHECK
// constraint in migrations/00011_top_snapshots.sql); declared here, next to
// where they are used, since the store package exports no typed constants of
// its own for them.
const (
	topKindArtist = "artist"
	topKindTrack  = "track"
)

// batch is one account's enumerated library and captured top rankings,
// converted and ready to reconcile or write.
type batch struct {
	// trackItems, albumItems and artistIDs are what the library repository
	// reconciles the three library tables against.
	trackItems []libstore.SavedItem
	albumItems []libstore.SavedItem
	artistIDs  []string

	// tracks, albums and artists are the catalogue detail this run already
	// carries — from the library enumeration and, when the account granted
	// scopeTopRead, the six top-item calls too — upserted so enrichment does
	// not have to fetch it again.
	tracks  []domain.Track
	albums  []domain.Album
	artists []domain.Artist

	// topSnapshots is the up-to-six rankings this run captured, empty when
	// the account's grant lacks scopeTopRead. commit writes each with
	// ReplaceTopSnapshot in the same transaction as everything else here.
	topSnapshots []topSnapshot
}

// build converts one account's raw enumeration into a batch.
//
// It is a pure function of its inputs, so the decisions that matter here —
// which items get full catalogue detail and which are merely registered —
// are testable without a database or a network, the same reason
// internal/sync/ingest.go's prepare is.
//
// topTracks and topArtists are the flattened results of all three top-item
// time ranges for each kind — order does not matter here, unlike the rank
// order sync itself extracts into each topSnapshot.entityIDs before calling
// build, because this function's only job for them is to mint catalogue
// detail, the exact path a saved track or a followed artist already takes.
// Passing nil for both leaves that behaviour exactly as it was before this
// worker read top items at all.
func build(
	tracks []spotify.SavedTrack, albums []spotify.SavedAlbum, artists []spotify.Artist,
	topTracks []spotify.Track, topArtists []spotify.Artist,
) batch {
	var b batch
	seenTrack := make(map[string]struct{}, len(tracks)+len(topTracks))
	seenAlbum := make(map[string]struct{}, len(tracks)+len(albums))
	seenArtist := make(map[string]struct{}, len(artists)+len(topArtists))

	b.trackItems = make([]libstore.SavedItem, 0, len(tracks))
	for _, st := range tracks {
		if st.Track.ID == "" {
			continue
		}
		b.trackItems = append(b.trackItems, savedItem(st.Track.ID, st.AddedAt))

		// A track object without a name is not the full object; it is
		// registered as a saved item above but left for enrichment to resolve,
		// the same reasoning internal/sync/ingest.go's prepare applies.
		if st.Track.Name == "" {
			continue
		}
		if _, dup := seenTrack[st.Track.ID]; !dup {
			seenTrack[st.Track.ID] = struct{}{}
			b.tracks = append(b.tracks, st.Track.ToDomainTrack())
		}

		album := st.Track.Album
		if album.ID == "" || album.Name == "" {
			continue
		}
		if _, dup := seenAlbum[album.ID]; !dup {
			seenAlbum[album.ID] = struct{}{}
			b.albums = append(b.albums, album.ToDomainAlbum())
		}
	}

	// Top tracks carry no saved-item concept of their own — appearing in a
	// listener's top fifty says nothing about whether it is still saved — so
	// this loop only ever feeds catalogue detail, never b.trackItems. It is
	// otherwise the same object shape (a full spotify.Track, embedded album
	// and all) that a saved track's own Track field already is, so the same
	// name-presence and dedup rules apply unchanged.
	for _, t := range topTracks {
		if t.ID == "" || t.Name == "" {
			continue
		}
		if _, dup := seenTrack[t.ID]; !dup {
			seenTrack[t.ID] = struct{}{}
			b.tracks = append(b.tracks, t.ToDomainTrack())
		}

		album := t.Album
		if album.ID == "" || album.Name == "" {
			continue
		}
		if _, dup := seenAlbum[album.ID]; !dup {
			seenAlbum[album.ID] = struct{}{}
			b.albums = append(b.albums, album.ToDomainAlbum())
		}
	}

	b.albumItems = make([]libstore.SavedItem, 0, len(albums))
	for _, sa := range albums {
		if sa.Album.ID == "" {
			continue
		}
		b.albumItems = append(b.albumItems, savedItem(sa.Album.ID, sa.AddedAt))

		if sa.Album.Name == "" {
			continue
		}
		if _, dup := seenAlbum[sa.Album.ID]; !dup {
			seenAlbum[sa.Album.ID] = struct{}{}
			b.albums = append(b.albums, sa.Album.ToDomainAlbum())
		}
	}

	b.artistIDs = make([]string, 0, len(artists))
	for _, a := range artists {
		if a.ID == "" {
			continue
		}
		b.artistIDs = append(b.artistIDs, a.ID)
		// The followed-artists endpoint returns the full artist object —
		// genres, popularity, followers, images — unlike the simplified form
		// embedded in a track or album, so these are upserted as resolved
		// rather than merely registered.
		if a.Name == "" {
			continue
		}
		if _, dup := seenArtist[a.ID]; !dup {
			seenArtist[a.ID] = struct{}{}
			b.artists = append(b.artists, a.ToDomainArtist())
		}
	}

	// Top artists are, like followed artists, the full object rather than the
	// simplified form embedded in a track or album — but being a listener's
	// top artist says nothing about whether they follow them, so unlike the
	// loop above this one never touches b.artistIDs. seenArtist is shared
	// with that loop purely to avoid upserting the same artist twice in one
	// batch when a listener's top artist is also one they follow; the SQL
	// itself (upsertArtistsSQL's DISTINCT ON) would tolerate the duplicate
	// either way, so this is a cleanliness choice, not a correctness one.
	for _, a := range topArtists {
		if a.ID == "" || a.Name == "" {
			continue
		}
		if _, dup := seenArtist[a.ID]; !dup {
			seenArtist[a.ID] = struct{}{}
			b.artists = append(b.artists, a.ToDomainArtist())
		}
	}

	// Sorted by id, not left in enumeration order (added_at desc, which differs
	// per account): commit's two loops below call ReplaceTrackArtists and
	// ReplaceAlbumArtists once per row, each taking row locks that survive to
	// the end of the transaction. Two accounts sharing a track or album — a
	// household, the ordinary self-hosted case — walking those rows in
	// different orders is exactly how two concurrent transactions deadlock:
	// each holds what the other wants next. A shared order removes that,
	// the same reason UpsertTracks/UpsertAlbums's own input CTE sorts by id.
	slices.SortFunc(b.tracks, func(x, y domain.Track) int { return cmp.Compare(x.ID, y.ID) })
	slices.SortFunc(b.albums, func(x, y domain.Album) int { return cmp.Compare(x.ID, y.ID) })

	return b
}

// savedItem builds a library.SavedItem, mapping a zero AddedAt onto nil: the
// column is nullable, and Spotify not reporting a value is different from
// Spotify reporting the epoch.
func savedItem(id string, addedAt time.Time) libstore.SavedItem {
	if addedAt.IsZero() {
		return libstore.SavedItem{ID: id}
	}
	at := addedAt.UTC()
	return libstore.SavedItem{ID: id, AddedAt: &at}
}

// topArtistIDs extracts one top-artists response's ids in the rank order
// Spotify returned them, for a topSnapshot's entityIDs: index 0 is rank 1.
// Unlike build's catalogue-detail merge above, a blank name does not exclude
// an id here — the ranking still has to say what Spotify put at that
// position even when this run cannot yet mint its detail — only a blank id
// is dropped, since ReplaceTopSnapshot has no row to place it at otherwise.
func topArtistIDs(artists []spotify.Artist) []string {
	ids := make([]string, 0, len(artists))
	for _, a := range artists {
		if a.ID == "" {
			continue
		}
		ids = append(ids, a.ID)
	}
	return ids
}

// topTrackIDs is topArtistIDs for a top-tracks response.
func topTrackIDs(tracks []spotify.Track) []string {
	ids := make([]string, 0, len(tracks))
	for _, t := range tracks {
		if t.ID == "" {
			continue
		}
		ids = append(ids, t.ID)
	}
	return ids
}
