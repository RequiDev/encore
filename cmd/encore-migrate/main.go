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

	"github.com/RequiDev/encore/internal/logging"
	"github.com/RequiDev/encore/internal/postgres"
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
	// The subcommand is read before the flags are parsed, deliberately. Go's
	// flag package stops at the first non-flag argument, so a single global
	// FlagSet would silently ignore `encore-migrate reset --yes` — the flag would
	// be treated as a positional argument and the reset would refuse itself with
	// a message telling you to pass the flag you just passed.
	args := os.Args[1:]
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		return errors.New("a command is required")
	}
	command := args[0]
	if command == "version" {
		fmt.Println(version)
		return nil
	}

	fs := flag.NewFlagSet("encore-migrate "+command, flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	confirm := fs.Bool("yes", false, "confirm a destructive reset")
	if err := fs.Parse(args[1:]); err != nil {
		return err
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
		fmt.Fprint(os.Stderr, usage)
		return fmt.Errorf("unknown command %q", command)
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
