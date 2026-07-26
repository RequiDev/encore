package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/requi/encore/migrations"
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

// Migrate applies every pending migration. It is safe to run concurrently from
// several processes: goose takes a session-level advisory lock, so a Compose
// stack that starts two replicas at once still applies each migration once.
func Migrate(ctx context.Context, dsn string, lg *slog.Logger) error {
	db, err := newGoose(dsn)
	if err != nil {
		return err
	}
	defer db.Close()

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
			lg.Info("database schema already current", "version", after)
		} else {
			lg.Info("database schema migrated", "from", before, "to", after)
		}
	}
	return nil
}

// Status reports the applied and available migration versions without changing
// anything.
func Status(ctx context.Context, dsn string) (MigrationStatus, error) {
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
