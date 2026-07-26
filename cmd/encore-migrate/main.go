// Command encore-migrate applies Encore's database migrations.
//
// It is a separate binary, and the Compose stack runs it as its own service that
// must exit successfully before the API or the worker start. Migrations are a
// deliberate, separately observable step rather than a side effect of a server
// booting: when a schema change fails you want to see it fail on its own, not
// buried in a service that then crash-loops.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/requi/encore/internal/logging"
	"github.com/requi/encore/internal/postgres"
)

// version is set at build time with -ldflags.
var version = "dev"

const usage = `encore-migrate - apply Encore's database migrations

Usage:
  encore-migrate up                 Apply every pending migration (safe to re-run)
  encore-migrate status             Report the applied and available schema versions
  encore-migrate reset --yes        Roll the schema all the way down. DESTROYS ALL DATA.
  encore-migrate version            Print the build version

The database is read from ENCORE_DATABASE_URL.
`

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "encore-migrate:", err)
		os.Exit(1)
	}
}

func run() error {
	confirm := flag.Bool("yes", false, "confirm a destructive reset")
	flag.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	flag.Parse()

	command := flag.Arg(0)
	if command == "" {
		flag.Usage()
		return errors.New("a command is required")
	}
	if command == "version" {
		fmt.Println(version)
		return nil
	}

	// Only the database URL is needed, so this binary deliberately does not load
	// the full configuration: running migrations must not require a Spotify
	// client id or an encryption key to be present.
	dsn := os.Getenv("ENCORE_DATABASE_URL")
	if dsn == "" {
		return errors.New("ENCORE_DATABASE_URL is required")
	}

	lg := logging.New(logging.Options{
		Level:   envOr("ENCORE_LOG_LEVEL", "info"),
		Format:  envOr("ENCORE_LOG_FORMAT", "json"),
		Service: "migrate",
		Version: version,
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch command {
	case "up":
		return postgres.Migrate(ctx, dsn, lg)

	case "status":
		st, err := postgres.Status(ctx, dsn)
		if err != nil {
			return err
		}
		fmt.Printf("applied version: %d\nlatest version:  %d\npending:         %d\n",
			st.Current, st.Latest, st.Pending)
		if !st.UpToDate() {
			// A non-zero exit makes this usable as a deployment gate.
			return fmt.Errorf("%d migration(s) pending", st.Pending)
		}
		return nil

	case "reset":
		if !*confirm {
			return errors.New("reset destroys every table and all data; pass --yes to confirm")
		}
		lg.Warn("rolling the schema all the way down; all data will be lost")
		return postgres.Reset(ctx, dsn)

	default:
		flag.Usage()
		return fmt.Errorf("unknown command %q", command)
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
