// Command encore-bench is Encore's repeatable import load test.
//
// It generates a synthetic Spotify export, imports it through the real pipeline
// — internal/importer, the same code an uploaded export goes through — and
// reports throughput, peak memory and the row counts read back from the
// database. Nothing here shortcuts the importer: when the benchmark says a
// million records were imported, a million rows are in `listens` and the job
// passed the same post-import verification a listener's own upload must pass.
//
// Only ENCORE_DATABASE_URL is required. The benchmark deliberately does not call
// config.Load, because measuring the importer must not depend on a Spotify
// client id or an encryption key being present on the machine doing the
// measuring.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

// version is set at build time with -ldflags.
var version = "dev"

const usage = `encore-bench - generate a synthetic Spotify export and benchmark the real import pipeline

Usage:
  encore-bench generate --records N [--format extended|account_data] --out PATH [--seed N]
        Write a synthetic export and exit. Nothing is imported and no database is needed.

  encore-bench run --records N [--format extended|account_data] [--batch-size N]
                   [--file PATH] [--seed N] [--max-heap-mb N] [--keep] [--report PATH]
        Generate a dataset (unless --file is given), import it through
        internal/importer, and report throughput, peak memory, the importer's
        counters and the row counts read back from the database.

  encore-bench verify --user ID
        Print the row counts one user's history occupies, straight from the database.

  encore-bench version
        Print the build version.

The database is read from ENCORE_DATABASE_URL, and the spool directory from
ENCORE_IMPORT_DIR when it is set; otherwise a temporary directory is used and
removed afterwards. No Spotify credentials are required.
`

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "encore-bench:", err)
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		return errors.New("a command is required")
	}

	// Interrupting a benchmark must leave the database tidy rather than half a
	// job behind, so the signal cancels the context every phase honours.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	command, args := os.Args[1], os.Args[2:]
	switch command {
	case "generate":
		return runGenerate(ctx, args)
	case "run":
		return runBenchmark(ctx, args)
	case "verify":
		return runVerify(ctx, args)
	case "version":
		fmt.Println(version)
		return nil
	case "help", "-h", "--help":
		fmt.Print(usage)
		return nil
	default:
		fmt.Fprint(os.Stderr, usage)
		return fmt.Errorf("unknown command %q", command)
	}
}

// envOr reads an environment variable with a fallback.
func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
