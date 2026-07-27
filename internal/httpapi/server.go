// Package httpapi is Encore's whole HTTP surface: the router, the middleware
// chain, the handlers and the DTOs they serialise.
//
// It contains no SQL and never imports pgx. Every answer is composed from the
// repositories under internal/store and the services around them, and every
// handler that touches user data asks for the object *scoped to the caller* —
// GetJobForUser(id, caller.ID) rather than GetJob(id) — so a missing
// authorisation check shows up as a missing argument rather than as a silently
// permissive route.
//
// The payload shapes are the contract documented in docs/api.md and mirrored by
// hand in web/src/lib/types.ts. Change one and change all three.
package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/requi/encore/internal/config"
	"github.com/requi/encore/internal/domain"
	"github.com/requi/encore/internal/importer"
	"github.com/requi/encore/internal/metrics"
	"github.com/requi/encore/internal/postgres"
	"github.com/requi/encore/internal/spotify"
	"github.com/requi/encore/internal/stats"
	"github.com/requi/encore/internal/store"
	"github.com/requi/encore/internal/store/accounts"
	"github.com/requi/encore/internal/store/catalog"
	"github.com/requi/encore/internal/store/imports"
	"github.com/requi/encore/internal/store/listens"
)

// Deps is everything the HTTP layer needs. Nothing here is constructed by the
// package itself: wiring belongs to cmd/encore-api, which owns the process's
// lifetime and therefore the lifetime of the pool, the client and the workers.
type Deps struct {
	Config   *config.Config
	Store    *store.Store
	Accounts *accounts.Repo
	Catalog  *catalog.Repo
	Listens  *listens.Repo
	Imports  *imports.Repo
	Stats    *stats.Service
	Intake   *importer.Intake
	Spotify  *spotify.Client
	Metrics  *metrics.Registry
	Logger   *slog.Logger
	Version  string
	// Now is the clock. Tests replace it; production leaves it nil for time.Now.
	Now func() time.Time
	// SyncNow triggers an immediate recently-played poll for one account and
	// reports what it found, so the interface can say "12 new plays" rather than
	// only "done". It is nil on an instance that does not run the sync loop, and
	// POST /api/sync/now then says so plainly rather than pretending to have
	// started something.
	SyncNow func(ctx context.Context, userID uuid.UUID) (SyncOutcome, error)
}

// The narrow interfaces below are the only view the HTTP layer takes of the
// identity repositories. They exist so that the middleware, the CSRF check and
// the administrator guard can be exercised by httptest with a fake, without a
// live database; the concrete repositories satisfy them as they are.

// sessionStore is the part of accounts.Sessions the HTTP layer uses.
type sessionStore interface {
	Create(ctx context.Context, q store.Querier, userID uuid.UUID, tokenHash []byte, csrfToken string, expiresAt time.Time, userAgent, ip string) (domain.Session, error)
	GetByTokenHash(ctx context.Context, q store.Querier, tokenHash []byte) (domain.Session, domain.User, error)
	Touch(ctx context.Context, q store.Querier, sessionID uuid.UUID) error
	Delete(ctx context.Context, q store.Querier, sessionID uuid.UUID) error
}

// listenStore is the part of listens.Repo the HTTP layer uses. Like the identity
// interfaces below it exists so the middleware and the /api/me handler can be
// exercised by httptest with a fake, rather than needing a live database.
type listenStore interface {
	Bounds(ctx context.Context, q store.Querier, userID uuid.UUID) (first, last *time.Time, err error)
	CountListensForUser(ctx context.Context, q store.Querier, userID uuid.UUID) (int64, error)
}

// userStore is the part of accounts.Users the HTTP layer uses.
type userStore interface {
	GetByID(ctx context.Context, q store.Querier, id uuid.UUID) (domain.User, error)
	ListUsers(ctx context.Context, q store.Querier, limit, offset int) ([]domain.User, int64, error)
	UpsertFromSpotify(ctx context.Context, q store.Querier, p accounts.SpotifyProfile, defaultTimezone string, allowRegistration bool) (domain.User, bool, error)
	SetTimezone(ctx context.Context, q store.Querier, id uuid.UUID, timezone string) (domain.User, error)
	SetRole(ctx context.Context, q store.Querier, id uuid.UUID, role domain.Role) (domain.User, error)
	SetActive(ctx context.Context, q store.Querier, id uuid.UUID, active bool) (domain.User, error)
	DeleteUser(ctx context.Context, q store.Querier, id uuid.UUID) error
	CountAdmins(ctx context.Context, q store.Querier) (int64, error)
}

// credentialStore is the part of accounts.Credentials the HTTP layer uses.
type credentialStore interface {
	Get(ctx context.Context, q store.Querier, userID uuid.UUID) (domain.SpotifyCredentials, error)
	Upsert(ctx context.Context, q store.Querier, creds domain.SpotifyCredentials) error
}

// oauthStateStore is the part of accounts.OAuthStates the HTTP layer uses.
type oauthStateStore interface {
	Create(ctx context.Context, q store.Querier, stateHash []byte, codeVerifier, redirectTo string, linkUserID *uuid.UUID, expiresAt time.Time) error
	Consume(ctx context.Context, q store.Querier, stateHash []byte) (codeVerifier string, redirectTo string, linkUserID *uuid.UUID, err error)
}

// settingsStore is the part of accounts.Settings the HTTP layer uses.
type settingsStore interface {
	RegistrationsEnabled(ctx context.Context, q store.Querier) (bool, error)
	SetRegistrationsEnabled(ctx context.Context, q store.Querier, enabled bool) error
}

// Server owns the routing table and the middleware chain.
type Server struct {
	cfg *config.Config
	// querier is the pool as a Querier, which is the only database handle this
	// package holds: every write it makes is a single statement, so it never
	// needs a transaction and therefore never has to name a pgx type. It is a
	// field rather than a call so the middleware can be exercised with fake
	// repositories and no pool at all.
	querier store.Querier
	// pool is kept for the readiness probe, which asks the database whether it is
	// answering at all. It is reached through internal/postgres rather than pgx,
	// so the rule that this package never imports the driver still holds.
	pool    *postgres.Pool
	log     *slog.Logger
	version string
	now     func() time.Time

	users       userStore
	sessions    sessionStore
	credentials credentialStore
	oauthStates oauthStateStore
	settings    settingsStore

	catalog *catalog.Repo
	listens listenStore
	imports *imports.Repo
	stats   *stats.Service
	intake  *importer.Intake
	spotify *spotify.Client
	metrics *metrics.Registry

	syncNow func(ctx context.Context, userID uuid.UUID) (SyncOutcome, error)
	syncing *inFlight
	touched *touchTracker
	ready   *readyCache

	// metricsHandler is built once, because promhttp's handler carries the
	// concurrency limit and the timeout that bound a scrape.
	metricsHandler http.Handler
	handler        http.Handler
}

// New validates the dependencies and builds the server.
//
// It fails rather than tolerating a nil repository: a half-wired API that
// answers some routes and panics on others is worse than one that refuses to
// start.
func New(deps Deps) (*Server, error) {
	switch {
	case deps.Config == nil:
		return nil, errors.New("httpapi: config is required")
	case deps.Store == nil:
		return nil, errors.New("httpapi: store is required")
	case deps.Accounts == nil:
		return nil, errors.New("httpapi: accounts repository is required")
	case deps.Catalog == nil:
		return nil, errors.New("httpapi: catalog repository is required")
	case deps.Listens == nil:
		return nil, errors.New("httpapi: listens repository is required")
	case deps.Imports == nil:
		return nil, errors.New("httpapi: imports repository is required")
	case deps.Stats == nil:
		return nil, errors.New("httpapi: stats service is required")
	case deps.Intake == nil:
		return nil, errors.New("httpapi: import intake is required")
	case deps.Spotify == nil:
		return nil, errors.New("httpapi: spotify client is required")
	}
	if deps.Accounts.Users == nil || deps.Accounts.Sessions == nil || deps.Accounts.Credentials == nil ||
		deps.Accounts.OAuthStates == nil || deps.Accounts.Settings == nil {
		return nil, errors.New("httpapi: the accounts repository is incomplete")
	}

	lg := deps.Logger
	if lg == nil {
		lg = slog.Default()
	}
	now := deps.Now
	if now == nil {
		now = time.Now
	}
	syncNow := deps.SyncNow
	if syncNow == nil {
		syncNow = func(context.Context, uuid.UUID) (SyncOutcome, error) {
			return SyncOutcome{}, ErrConflictf("This instance does not run the Spotify synchronisation loop.")
		}
	}

	s := &Server{
		cfg:         deps.Config,
		querier:     deps.Store.DB(),
		pool:        deps.Store.Pool(),
		log:         lg.With("component", "httpapi"),
		version:     deps.Version,
		now:         now,
		users:       deps.Accounts.Users,
		sessions:    deps.Accounts.Sessions,
		credentials: deps.Accounts.Credentials,
		oauthStates: deps.Accounts.OAuthStates,
		settings:    deps.Accounts.Settings,
		catalog:     deps.Catalog,
		listens:     deps.Listens,
		imports:     deps.Imports,
		stats:       deps.Stats,
		intake:      deps.Intake,
		spotify:     deps.Spotify,
		metrics:     deps.Metrics,
		syncNow:     syncNow,
		syncing:     newInFlight(),
		touched:     newTouchTracker(),
		ready:       &readyCache{},
	}
	if deps.Metrics != nil && deps.Config.Metrics.Enabled {
		s.metricsHandler = deps.Metrics.Handler(deps.Config.Metrics.Username, deps.Config.Metrics.Password)
	}
	s.handler = s.buildHandler()
	return s, nil
}

// Handler returns the fully wrapped http.Handler. It is safe to serve from
// several goroutines and does not change after New.
func (s *Server) Handler() http.Handler { return s.handler }

// --- small concurrent helpers ----------------------------------------------

// touchInterval is how often a session's last_seen_at is refreshed. Writing it
// on every request would turn a read-only dashboard poll into a write, and the
// column is only ever read by a human wondering when a session was last used.
const touchInterval = time.Minute

// maxTrackedSessions bounds the touch tracker. Beyond it the map is pruned of
// entries older than the interval, which are exactly the ones whose next request
// would write anyway.
const maxTrackedSessions = 8192

// touchTracker remembers when each session's last_seen_at was last written.
type touchTracker struct {
	mu   sync.Mutex
	seen map[uuid.UUID]time.Time
}

func newTouchTracker() *touchTracker { return &touchTracker{seen: make(map[uuid.UUID]time.Time)} }

// due reports whether the session should be touched now, recording the decision.
func (t *touchTracker) due(id uuid.UUID, now time.Time) bool {
	t.mu.Lock()
	defer t.mu.Unlock()

	if last, ok := t.seen[id]; ok && now.Sub(last) < touchInterval {
		return false
	}
	if len(t.seen) >= maxTrackedSessions {
		for k, v := range t.seen {
			if now.Sub(v) >= touchInterval {
				delete(t.seen, k)
			}
		}
	}
	t.seen[id] = now
	return true
}

// forget drops a session that has just been signed out, so its id cannot pin
// memory for a minute after it stops existing.
func (t *touchTracker) forget(id uuid.UUID) {
	t.mu.Lock()
	delete(t.seen, id)
	t.mu.Unlock()
}

// inFlight is a set of users with an operation already running, so a double
// click answers 409 immediately instead of starting the work twice.
type inFlight struct {
	mu  sync.Mutex
	ids map[uuid.UUID]struct{}
}

func newInFlight() *inFlight { return &inFlight{ids: make(map[uuid.UUID]struct{})} }

// acquire claims the slot, reporting false when it was already taken.
func (f *inFlight) acquire(id uuid.UUID) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, busy := f.ids[id]; busy {
		return false
	}
	f.ids[id] = struct{}{}
	return true
}

// release gives the slot back.
func (f *inFlight) release(id uuid.UUID) {
	f.mu.Lock()
	delete(f.ids, id)
	f.mu.Unlock()
}

// readyCacheTTL is how long a positive readiness result is trusted.
//
// Only success is cached: a probe that has just been told the instance is not
// ready must see the recovery immediately, whereas re-opening a connection to
// re-read the migration table on every probe would be a needless load spike on
// an instance with aggressive health checks.
const readyCacheTTL = 30 * time.Second

// readyCache remembers that the schema was up to date.
type readyCache struct {
	mu    sync.Mutex
	until time.Time
}

func (c *readyCache) fresh(now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return now.Before(c.until)
}

func (c *readyCache) markFresh(now time.Time) {
	c.mu.Lock()
	c.until = now.Add(readyCacheTTL)
	c.mu.Unlock()
}
