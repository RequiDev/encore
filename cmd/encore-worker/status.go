package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"text/tabwriter"
	"time"

	"github.com/RequiDev/encore/internal/config"
	"github.com/RequiDev/encore/internal/crypto"
	"github.com/RequiDev/encore/internal/postgres"
	"github.com/RequiDev/encore/internal/store"
	"github.com/RequiDev/encore/internal/store/accounts"
)

// reportStatus prints what this instance currently holds and what it is waiting
// for.
//
// It exists because the most common question about a self-hosted Encore is
// "why are the artists blank, and is it ever going to fix itself?" — and the
// honest answer lives in four tables and one setting. Reading them by hand means
// knowing the schema; this does not.
//
// It only reads. Safe to run at any time, including while the worker is busy.
func reportStatus(ctx context.Context, cfg *config.Config, lg *slog.Logger) error {
	pool, err := postgres.Connect(ctx, cfg.Database, lg)
	if err != nil {
		return err
	}
	defer pool.Close()

	sealer, err := crypto.NewSealer(cfg.Security.EncryptionKey)
	if err != nil {
		return err
	}
	db, err := store.New(pool, sealer)
	if err != nil {
		return err
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	defer w.Flush()

	scalar := func(query string) int64 {
		var n int64
		if err := pool.QueryRow(ctx, query).Scan(&n); err != nil {
			return -1
		}
		return n
	}

	fmt.Fprintln(w, "Listening data")
	fmt.Fprintf(w, "  listens\t%d\n", scalar(`SELECT count(*) FROM listens`))
	fmt.Fprintf(w, "  users\t%d\n", scalar(`SELECT count(*) FROM users`))
	fmt.Fprintf(w, "  awaiting a track id\t%d\n",
		scalar(`SELECT count(*) FROM listens WHERE track_id IS NULL`))

	var first, last *time.Time
	_ = pool.QueryRow(ctx, `SELECT min(played_at), max(played_at) FROM listens`).Scan(&first, &last)
	if first != nil && last != nil {
		fmt.Fprintf(w, "  covering\t%s to %s\n",
			first.Format("2006-01-02"), last.Format("2006-01-02"))
	}

	fmt.Fprintln(w, "\nCatalogue")
	for _, t := range []struct{ label, table string }{
		{"tracks", "tracks"}, {"artists", "artists"}, {"albums", "albums"},
	} {
		total := scalar(`SELECT count(*) FROM ` + t.table)
		resolved := scalar(`SELECT count(*) FROM ` + t.table + ` WHERE metadata_state = 'resolved'`)
		pending := scalar(`SELECT count(*) FROM ` + t.table + ` WHERE metadata_state = 'pending'`)
		failed := scalar(`SELECT count(*) FROM ` + t.table + ` WHERE metadata_state = 'failed'`)
		unavailable := scalar(`SELECT count(*) FROM ` + t.table + ` WHERE metadata_state = 'unavailable'`)
		named := scalar(`SELECT count(*) FROM ` + t.table + ` WHERE name <> ''`)
		local := scalar(`SELECT count(*) FROM ` + t.table + ` WHERE metadata_state = 'local'`)
		fmt.Fprintf(w,
			"  %s\t%d total\t%d resolved\t%d pending\t%d failed\t%d unavailable\t%d with a name\t%d named by an import\n",
			t.label, total, resolved, pending, failed, unavailable, named, local)
	}
	fmt.Fprintf(w, "  aliases\t%d total\t%d resolved\t%d pending\n",
		scalar(`SELECT count(*) FROM track_aliases`),
		scalar(`SELECT count(*) FROM track_aliases WHERE state = 'resolved'`),
		scalar(`SELECT count(*) FROM track_aliases WHERE state = 'pending'`))

	// The single most useful line when metadata is not filling in.
	fmt.Fprintln(w, "\nSpotify")
	settings := accounts.NewSettings(db)
	paused, err := settings.SpotifyPausedUntil(ctx, db.DB())
	switch {
	case err != nil:
		fmt.Fprintf(w, "  rate limit\tcould not be read: %v\n", err)
	case paused.IsZero():
		fmt.Fprintln(w, "  rate limit\tnot rate limited")
	case time.Until(paused) <= 0:
		fmt.Fprintf(w, "  rate limit\tcleared at %s\n", paused.UTC().Format(time.RFC3339))
	default:
		fmt.Fprintf(w, "  rate limit\tPAUSED until %s (%s remaining)\n",
			paused.UTC().Format(time.RFC3339), time.Until(paused).Round(time.Minute))
		fmt.Fprintln(w, "  \tlistening data is unaffected; names and artwork resume by themselves")
	}

	rows, err := pool.Query(ctx,
		`SELECT u.display_name, c.sync_state, c.last_sync_at, c.last_sync_error
         FROM users u LEFT JOIN spotify_credentials c ON c.user_id = u.id ORDER BY u.created_at`)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var name string
			var state, syncErr *string
			var at *time.Time
			if err := rows.Scan(&name, &state, &at, &syncErr); err != nil {
				break
			}
			when := "never"
			if at != nil {
				when = at.UTC().Format(time.RFC3339)
			}
			s := "not connected"
			if state != nil {
				s = *state
			}
			line := fmt.Sprintf("  %s\t%s\tlast sync %s", name, s, when)
			if syncErr != nil && *syncErr != "" {
				line += "\t" + *syncErr
			}
			fmt.Fprintln(w, line)
		}
	}

	fmt.Fprintln(w, "\nImports")
	imports, err := pool.Query(ctx,
		`SELECT status, count(*) FROM import_jobs GROUP BY status ORDER BY status`)
	if err == nil {
		defer imports.Close()
		any := false
		for imports.Next() {
			var status string
			var n int64
			if err := imports.Scan(&status, &n); err != nil {
				break
			}
			fmt.Fprintf(w, "  %s\t%d\n", status, n)
			any = true
		}
		if !any {
			fmt.Fprintln(w, "  none\t")
		}
	}

	pendingNames := scalar(`SELECT count(*) FROM tracks WHERE name = ''`)
	if pendingNames > 0 {
		fmt.Fprintf(w, "\n%d tracks have no name yet. If they came from an import whose upload is\n", pendingNames)
		fmt.Fprintln(w, "still on disk, `encore-worker backfill-names` recovers them without Spotify.")
	}
	return nil
}
