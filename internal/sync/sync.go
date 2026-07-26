// Package sync polls Spotify's recently-played feed for every connected account
// and ingests what it finds through the same duplicate-safe path the importer
// uses.
//
// Three properties decide everything else here:
//
//  1. The cursor advances only after the batch commits. The listens and the new
//     sync_cursor_at are written in one transaction, so a process that dies
//     between them simply re-fetches on the next poll and the duplicate rules
//     make that a no-op. Advancing first could lose plays permanently, because
//     Spotify only ever returns the last fifty.
//  2. A failure for one account never stops the others. An expired token, a
//     revoked grant or a throttled request is contained to the account it
//     belongs to, recorded on its credential row, and the tick carries on.
//  3. A grant Spotify has rejected outright is parked as needs_reauth rather
//     than retried. Only the listener can fix it, and polling it every minute
//     until they do would spend the instance's rate limit on a certain failure.
package sync

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	stdsync "sync"
	"time"

	"github.com/google/uuid"

	"github.com/requi/encore/internal/config"
	"github.com/requi/encore/internal/spotify"
	"github.com/requi/encore/internal/store"
	"github.com/requi/encore/internal/store/accounts"
	"github.com/requi/encore/internal/store/catalog"
	"github.com/requi/encore/internal/store/listens"
)

// Sentinel failures a caller is expected to distinguish. Everything else that
// can go wrong is an ordinary wrapped error.
var (
	// ErrNotConnected reports that the user has no Spotify grant at all, so
	// there is nothing to poll.
	ErrNotConnected = errors.New("sync: the account is not connected to Spotify")

	// ErrNeedsReauth reports that the grant is broken in a way only its owner
	// can repair, by authorising again. The account has been parked; the poller
	// will not touch it until a new grant arrives.
	ErrNeedsReauth = errors.New("sync: the Spotify authorisation has to be renewed by its owner")

	// ErrAlreadyRunning reports that a poll for this account is already in
	// flight. POST /api/sync/now answers 409 on it rather than starting a second
	// reader of the same feed.
	ErrAlreadyRunning = errors.New("sync: a poll for this account is already running")
)

// Defaults applied when configuration carries a nonsensical value. Config
// validation should have caught these already; a poller that has been handed a
// zero interval must still not spin.
const (
	defaultInterval        = 60 * time.Second
	defaultInitialLookback = 14 * 24 * time.Hour
)

const (
	// recentlyPlayedLimit is Spotify's maximum page size, and the whole of what
	// the endpoint retains, so asking for less only costs round trips.
	recentlyPlayedLimit = 50
	// recentlyPlayedPages bounds how far one poll will follow the forward
	// cursor. The feed holds fifty plays, so a single page is the normal case
	// and the allowance exists only for a cursor that pages unexpectedly.
	recentlyPlayedPages = 4
)

// Metric result labels. They match the values internal/metrics publishes, so the
// adapter that wires the two together is a straight pass-through.
const (
	resultSuccess = "success"
	resultFailure = "failure"
	resultSkipped = "skipped"
)

// markTimeout bounds the bookkeeping writes that must happen even while the
// process is shutting down: parking an account as needs_reauth, and recording a
// failed poll.
const markTimeout = 10 * time.Second

// Metrics receives sync telemetry.
//
// It is an interface so this package does not depend on Prometheus: cmd wires
// the real collector in, and tests use the zero value.
type Metrics interface {
	// SyncRun records one finished poll, labelled success, failure or skipped.
	SyncRun(result string)
	// SyncListens records listens newly committed by the poller.
	SyncListens(n int64)
	// SyncLastSuccess publishes when a poll last succeeded, which is what makes
	// a sync that has quietly stopped running alertable.
	SyncLastSuccess(t time.Time)
}

// NopMetrics discards telemetry.
type NopMetrics struct{}

func (NopMetrics) SyncRun(string)            {}
func (NopMetrics) SyncListens(int64)         {}
func (NopMetrics) SyncLastSuccess(time.Time) {}

// SpotifyAPI is the part of *spotify.Client the poller uses.
//
// Declaring it here documents exactly how much of the client this package
// depends on, and lets the conversion and scheduling behaviour be exercised
// without a network.
type SpotifyAPI interface {
	RecentlyPlayed(ctx context.Context, accessToken string, after time.Time, limit, maxPages int) ([]spotify.PlayHistory, error)
	RefreshToken(ctx context.Context, refreshToken string) (*spotify.Token, error)
}

// Deps are the collaborators a Poller needs.
type Deps struct {
	Store    *store.Store
	Accounts *accounts.Repo
	Listens  *listens.Repo
	Catalog  *catalog.Repo
	Spotify  SpotifyAPI
	Logger   *slog.Logger
	Metrics  Metrics
	// Now is injectable so tests can control timestamps without sleeping.
	Now func() time.Time
}

// Poller polls the recently-played feed of every connected account.
//
// It holds no durable state: the watermark, the health of each grant and the
// duplicate rules all live in the database, so a Poller can be killed at any
// instant and another process picks up exactly where it was.
type Poller struct {
	cfg  config.Sync
	dep  Deps
	now  func() time.Time
	stat Metrics
	log  *slog.Logger

	// rnd supplies the tick jitter in [0,1). Injectable so a test can make the
	// schedule deterministic.
	rnd func() float64

	// running guards against two concurrent polls of the same account, which
	// would fetch the same page twice and race on the cursor.
	mu      stdsync.Mutex
	running map[uuid.UUID]struct{}
}

// NewPoller builds a Poller. Every repository it names is required; the logger,
// the metrics sink and the clock default to sensible values.
func NewPoller(cfg config.Sync, deps Deps) (*Poller, error) {
	if deps.Store == nil {
		return nil, errors.New("sync: a store is required")
	}
	if deps.Accounts == nil || deps.Accounts.Credentials == nil || deps.Accounts.Users == nil {
		return nil, errors.New("sync: the users and credentials repositories are required")
	}
	if deps.Listens == nil {
		return nil, errors.New("sync: the listens repository is required")
	}
	if deps.Catalog == nil {
		return nil, errors.New("sync: the catalog repository is required")
	}
	if deps.Spotify == nil {
		return nil, errors.New("sync: a Spotify client is required")
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	if deps.Metrics == nil {
		deps.Metrics = NopMetrics{}
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
	if cfg.InitialLookback <= 0 {
		cfg.InitialLookback = defaultInitialLookback
	}

	return &Poller{
		cfg:     cfg,
		dep:     deps,
		now:     deps.Now,
		stat:    deps.Metrics,
		log:     deps.Logger.With("component", "sync"),
		rnd:     rand.Float64,
		running: make(map[uuid.UUID]struct{}),
	}, nil
}

// SyncResult is the outcome of one account's poll.
//
// The four counters partition every play the API returned:
//
//	Imported + Duplicates + Skipped + Invalid == Fetched
//
// so a caller can always account for the whole page rather than inferring what
// happened to the difference.
type SyncResult struct {
	// Fetched is how many play-history entries Spotify returned.
	Fetched int
	// Imported is how many rows were newly committed to listens.
	Imported int
	// Duplicates is how many valid plays the duplicate rules suppressed. On a
	// healthy instance this is normal: a poll re-reads whatever the previous one
	// could not confirm.
	Duplicates int
	// Skipped is how many entries were not music: podcast episodes, audiobook
	// chapters and local files all share this feed.
	Skipped int
	// Invalid is how many entries failed validation and were counted rather than
	// failing the run.
	Invalid int
	// NewestPlayedAt is the newest play the poll accounted for, and therefore the
	// value the cursor was advanced to. Zero when the poll found nothing usable.
	NewestPlayedAt time.Time
}

// claim reserves an account for the calling goroutine, reporting false when a
// poll for it is already in flight.
//
// Two concurrent readers of one feed would fetch the same page and race to move
// the same cursor, so an overlapping tick and an impatient POST /api/sync/now
// are both turned away here rather than allowed to duplicate work.
func (p *Poller) claim(userID uuid.UUID) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, busy := p.running[userID]; busy {
		return false
	}
	p.running[userID] = struct{}{}
	return true
}

// release ends a claim taken by claim.
func (p *Poller) release(userID uuid.UUID) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.running, userID)
}
