// Package harness provides the shared scaffolding for Encore's integration and
// import tests: a real PostgreSQL, a migrated schema, and fixtures.
//
// There is no in-memory or mocked database anywhere in these suites. The whole
// point of the import design is how it behaves against a real transactional
// engine — checkpoints committing with their batch, unique constraints deciding
// duplicates, SKIP LOCKED handing work to exactly one worker — and none of that
// can be verified against a fake.
package harness

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/RequiDev/encore/internal/config"
	"github.com/RequiDev/encore/internal/crypto"
	"github.com/RequiDev/encore/internal/domain"
	"github.com/RequiDev/encore/internal/postgres"
	"github.com/RequiDev/encore/internal/store"
	"github.com/RequiDev/encore/internal/store/accounts"
	"github.com/RequiDev/encore/internal/store/catalog"
	"github.com/RequiDev/encore/internal/store/imports"
	"github.com/RequiDev/encore/internal/store/listens"
)

// TestDatabaseEnv names the connection string the suites use.
const TestDatabaseEnv = "ENCORE_TEST_DATABASE_URL"

var (
	once      sync.Once
	sharedDSN string
	setupErr  error
)

// DSN resolves the test database connection string, migrating the schema the
// first time it is asked for.
//
// When the variable is unset the suite skips rather than fails: a contributor
// without Docker should still be able to run the unit tests with a plain
// `go test ./...`, and CI always sets it.
func DSN(t *testing.T) string {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv(TestDatabaseEnv))
	if dsn == "" {
		t.Skipf("set %s to run integration tests, for example "+
			"postgres://encore:encore@localhost:5432/encore?sslmode=disable", TestDatabaseEnv)
	}

	once.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		setupErr = postgres.Migrate(ctx, dsn, slog.New(slog.DiscardHandler))
		sharedDSN = dsn
	})
	if setupErr != nil {
		t.Fatalf("migrate test database: %v", setupErr)
	}
	return sharedDSN
}

// Pool opens a connection pool bound to the test's lifetime.
func Pool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := DSN(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool, err := postgres.Connect(ctx, config.Database{
		URL:              dsn,
		MaxConns:         8,
		ConnectTimeout:   10 * time.Second,
		StatementTimeout: 5 * time.Minute,
	}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("connect to test database: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// Env is everything a test needs: a clean database and every repository.
type Env struct {
	T        *testing.T
	Pool     *pgxpool.Pool
	Store    *store.Store
	Accounts *accounts.Repo
	Catalog  *catalog.Repo
	Imports  *imports.Repo
	Listens  *listens.Repo
	Dir      string
}

// New builds an Env with an empty database.
//
// Tests share one database and truncate between runs rather than creating a
// database each time: truncation is milliseconds where creation is seconds, and
// the schema is identical either way.
//
// The cost is that nothing may run concurrently against it. Within a package
// that is handled by not calling t.Parallel. Across packages it is not: `go test
// ./test/...` runs each package in its own process, in parallel, and two of them
// truncating between each other's assertions produces failures that look like
// application bugs. Run the suite with -p 1; the Makefile and CI both do.
func New(t *testing.T) *Env {
	t.Helper()
	pool := Pool(t)
	Truncate(t, pool)

	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i * 7)
	}
	sealer, err := crypto.NewSealer(key)
	if err != nil {
		t.Fatalf("build sealer: %v", err)
	}
	st, err := store.New(pool, sealer)
	if err != nil {
		t.Fatalf("build store: %v", err)
	}

	return &Env{
		T:        t,
		Pool:     pool,
		Store:    st,
		Accounts: accounts.New(st),
		Catalog:  catalog.New(st),
		Imports:  imports.New(st),
		Listens:  listens.New(st),
		Dir:      t.TempDir(),
	}
}

// Ctx returns a context bounded by the test's deadline.
func (e *Env) Ctx() context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	e.T.Cleanup(cancel)
	return ctx
}

// truncatedTables is every table holding test state. app_settings is handled
// separately below, because it is emptied and re-seeded rather than simply
// truncated; goose's own version table must obviously not be touched.
var truncatedTables = []string{
	"listens",
	"import_rejects",
	"import_files",
	"import_jobs",
	"listen_daily_rollup",
	"rollup_dirty_days",
	"user_blacklisted_artists",
	"track_aliases",
	"track_artists",
	"album_artists",
	"tracks",
	"albums",
	"artists",
	"sessions",
	"oauth_states",
	"spotify_credentials",
	"users",
}

// Truncate empties every table a test could have written to.
func Truncate(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	sql := fmt.Sprintf("TRUNCATE TABLE %s RESTART IDENTITY CASCADE", strings.Join(truncatedTables, ", "))
	if _, err := pool.Exec(ctx, sql); err != nil {
		t.Fatalf("truncate test database: %v", err)
	}
	// app_settings is emptied and re-seeded rather than left alone. It holds more
	// than the registration flag — the recorded Spotify rate-limit pause, for one
	// — and a value left behind by an earlier test silently changes the next
	// one's behaviour, which is a confusing failure to diagnose.
	if _, err := pool.Exec(ctx, `DELETE FROM app_settings`); err != nil {
		t.Fatalf("clear settings: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO app_settings (key, value) VALUES ('registrations_enabled', 'true'::jsonb)`); err != nil {
		t.Fatalf("reset settings: %v", err)
	}
}

// NewUser inserts a user and returns it.
func (e *Env) NewUser(name string) domain.User {
	e.T.Helper()
	user, _, err := e.Accounts.Users.UpsertFromSpotify(e.Ctx(), e.Store.DB(), accounts.SpotifyProfile{
		SpotifyUserID: name,
		DisplayName:   name,
		Email:         name + "@example.test",
	}, "UTC", true)
	if err != nil {
		e.T.Fatalf("create user %q: %v", name, err)
	}
	return user
}

// CountListens reads the number of rows a user actually has, straight from the
// fact table. Every assertion about an import's outcome goes through here rather
// than through the importer's own counters: the counters are the thing under
// test, so they cannot also be the evidence.
func (e *Env) CountListens(userID uuid.UUID) int64 {
	e.T.Helper()
	var n int64
	if err := e.Pool.QueryRow(e.Ctx(),
		`SELECT count(*) FROM listens WHERE user_id = $1`, userID.String()).Scan(&n); err != nil {
		e.T.Fatalf("count listens: %v", err)
	}
	return n
}

// CountListensForFile is the same question scoped to one import file.
func (e *Env) CountListensForFile(fileID uuid.UUID) int64 {
	e.T.Helper()
	var n int64
	if err := e.Pool.QueryRow(e.Ctx(),
		`SELECT count(*) FROM listens WHERE import_file_id = $1`, fileID.String()).Scan(&n); err != nil {
		e.T.Fatalf("count listens for file: %v", err)
	}
	return n
}

// ScalarInt runs a query returning one integer. Tests use it for ad-hoc checks
// that would not earn a helper of their own.
func (e *Env) ScalarInt(query string, args ...any) int64 {
	e.T.Helper()
	var n int64
	if err := e.Pool.QueryRow(e.Ctx(), query, args...).Scan(&n); err != nil {
		e.T.Fatalf("query %q: %v", query, err)
	}
	return n
}

// Logger returns a logger that writes into the test's output, so a failing test
// carries the worker's own account of what it did.
func Logger(t *testing.T) *slog.Logger {
	return slog.New(slog.NewTextHandler(testWriter{t}, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Log(strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

// Discard is a logger for tests that only care about the outcome.
func Discard() *slog.Logger { return slog.New(slog.DiscardHandler) }
