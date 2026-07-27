package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"sync"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/RequiDev/encore/migrations"
)

// MigrationStatus describes how far the database has been migrated.
type MigrationStatus struct {
	Current int64 `json:"current"`
	Latest  int64 `json:"latest"`
	Pending int   `json:"pending"`
}

// UpToDate reports whether every embedded migration has been applied. Readiness
// depends on this: an API process talking to a database with pending migrations
// would fail in confusing ways, so it reports itself not-ready instead.
func (s MigrationStatus) UpToDate() bool { return s.Pending == 0 && s.Current == s.Latest }

// gooseMu serialises everything that touches goose within this process.
//
// goose keeps its filesystem, dialect and logger in package-level variables, so
// two goroutines configuring it at once is a data race — one the detector finds
// immediately and which would otherwise let a migration run against a
// half-applied configuration. The advisory lock in Migrate solves the
// cross-process half of the same problem; this solves the in-process half.
//
// Serialising here costs nothing: every caller is either a one-shot command or a
// single startup step.
var gooseMu sync.Mutex

func newGoose(dsn string) (*sql.DB, error) {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse database url: %w", redactErr(err))
	}
	db := stdlib.OpenDB(*cfg)
	goose.SetBaseFS(migrations.FS)
	if err := goose.SetDialect("postgres"); err != nil {
		db.Close()
		return nil, fmt.Errorf("set dialect: %w", err)
	}
	return db, nil
}

// migrationLockID identifies the advisory lock that serialises migration runs.
//
// Any constant works as long as every Encore process agrees on it; this one is
// an arbitrary value chosen to be unlikely to collide with another application
// sharing the database.
const migrationLockID int64 = 0x454E434F5245_01

// Migrate applies every pending migration.
//
// It is safe to run concurrently from several processes. goose's own migration
// runner does not lock, so two processes reaching UpContext at the same moment
// both try to create the same table and one dies with a duplicate-key error
// against a system catalogue — an error that reads like corruption and is not
// obviously a race. A session-level advisory lock is taken first so the second
// process waits and then finds nothing left to do.
//
// This matters wherever more than one process can start at once: a Compose stack
// with several replicas, or any deployment using
// ENCORE_DATABASE_MIGRATE_ON_START. The lock is released when the connection
// closes, so a process killed mid-migration does not leave it held.
func Migrate(ctx context.Context, dsn string, lg *slog.Logger) error {
	gooseMu.Lock()
	defer gooseMu.Unlock()

	db, err := newGoose(dsn)
	if err != nil {
		return err
	}
	defer db.Close()

	// A dedicated connection, because an advisory lock belongs to the session
	// that took it and the pool must not hand this one to anybody else.
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquire migration connection: %w", redactErr(err))
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, "SELECT pg_advisory_lock($1)", migrationLockID); err != nil {
		return fmt.Errorf("take migration lock: %w", redactErr(err))
	}
	defer func() {
		// Best effort: closing the connection releases it regardless.
		_, _ = conn.ExecContext(context.WithoutCancel(ctx),
			"SELECT pg_advisory_unlock($1)", migrationLockID)
	}()

	goose.SetLogger(gooseLogger{lg: lg})
	before, _ := goose.GetDBVersionContext(ctx, db)
	if err := goose.UpContext(ctx, db, "."); err != nil {
		return fmt.Errorf("apply migrations: %w", redactErr(err))
	}
	after, err := goose.GetDBVersionContext(ctx, db)
	if err != nil {
		return fmt.Errorf("read schema version: %w", redactErr(err))
	}
	if lg != nil {
		if before == after {
			lg.Info("database schema already current", "schema_version", after)
		} else {
			lg.Info("database schema migrated", "schema_from", before, "schema_to", after)
		}
	}
	return nil
}

// Status reports the applied and available migration versions without changing
// anything.
func Status(ctx context.Context, dsn string) (MigrationStatus, error) {
	gooseMu.Lock()
	defer gooseMu.Unlock()

	db, err := newGoose(dsn)
	if err != nil {
		return MigrationStatus{}, err
	}
	defer db.Close()

	current, err := goose.GetDBVersionContext(ctx, db)
	if err != nil {
		return MigrationStatus{}, fmt.Errorf("read schema version: %w", redactErr(err))
	}
	all, err := goose.CollectMigrations(".", 0, goose.MaxVersion)
	if err != nil {
		return MigrationStatus{}, fmt.Errorf("collect migrations: %w", err)
	}
	st := MigrationStatus{Current: current}
	for _, m := range all {
		if m.Version > st.Latest {
			st.Latest = m.Version
		}
		if m.Version > current {
			st.Pending++
		}
	}
	return st, nil
}

// Reset rolls the schema all the way down. It exists for integration tests and
// is never wired into a command that a production deployment can reach.
func Reset(ctx context.Context, dsn string) error {
	gooseMu.Lock()
	defer gooseMu.Unlock()

	db, err := newGoose(dsn)
	if err != nil {
		return err
	}
	defer db.Close()
	goose.SetLogger(goose.NopLogger())
	if err := goose.DownToContext(ctx, db, ".", 0); err != nil {
		return fmt.Errorf("reset schema: %w", redactErr(err))
	}
	return nil
}

// gooseLogger bridges goose's printf-style logging onto slog.
type gooseLogger struct{ lg *slog.Logger }

func (g gooseLogger) Fatalf(format string, v ...any) {
	if g.lg != nil {
		g.lg.Error("migration failed", "detail", fmt.Sprintf(format, v...))
	}
}

func (g gooseLogger) Printf(format string, v ...any) {
	if g.lg != nil {
		g.lg.Debug("goose", "detail", fmt.Sprintf(format, v...))
	}
}
