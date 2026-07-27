package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"sort"
	"text/tabwriter"
	"time"

	"github.com/google/uuid"

	"github.com/RequiDev/encore/internal/domain"
	"github.com/RequiDev/encore/internal/logging"
	"github.com/RequiDev/encore/internal/store/accounts"
	"github.com/RequiDev/encore/internal/store/imports"
)

// runVerify implements `encore-bench verify`.
//
// It prints what the database holds for one user, read with plain aggregates
// over the tables themselves. Nothing here consults a counter the importer
// maintained: the value of this command is precisely that it is a second
// opinion, usable after a --keep run, after a crash, or against a history
// imported months ago.
func runVerify(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	userID := fs.String("user", "", "the user whose rows to count")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *userID == "" {
		return errors.New("--user is required")
	}
	id, err := uuid.Parse(*userID)
	if err != nil {
		return fmt.Errorf("--user must be a UUID: %w", err)
	}

	dsn := os.Getenv("ENCORE_DATABASE_URL")
	if dsn == "" {
		return errors.New("ENCORE_DATABASE_URL is required")
	}
	lg := logging.New(logging.Options{
		Level:   envOr("ENCORE_LOG_LEVEL", "warn"),
		Format:  envOr("ENCORE_LOG_FORMAT", "text"),
		Service: "bench",
		Version: version,
	})

	pool, st, err := openDatabase(ctx, dsn, lg)
	if err != nil {
		return err
	}
	defer pool.Close()

	db := st.DB()
	user, err := accounts.NewUsers(st).GetByID(ctx, db, id)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return fmt.Errorf("no user with id %s", id)
		}
		return err
	}

	counts, err := readRowCounts(ctx, db, id)
	if err != nil {
		return err
	}
	byStatus, err := jobStatusCounts(ctx, db, id)
	if err != nil {
		return err
	}
	jobs, _, err := imports.New(st).ListJobsForUser(ctx, db, id, imports.MaxPageSize, 0)
	if err != nil {
		return err
	}

	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	row := func(label string, value any) { fmt.Fprintf(tw, "  %s\t%v\n", label, value) }

	fmt.Fprintf(tw, "User %s (%s, timezone %s)\n", user.ID, user.DisplayName, user.Timezone)

	fmt.Fprintf(tw, "\nListens\n")
	row("rows", humanInt(counts.Listens))
	row("resolved to a track", humanInt(counts.ListensWithTrack))
	row("awaiting alias resolution", humanInt(counts.Listens-counts.ListensWithTrack))
	row("distinct tracks", humanInt(counts.DistinctTracks))
	row("distinct name pairs", humanInt(counts.DistinctAliases))
	row("tracks in the catalogue", humanInt(counts.TracksTotal))
	if counts.FirstPlayedAt != nil && counts.LastPlayedAt != nil {
		row("first play", counts.FirstPlayedAt.UTC().Format(time.RFC3339))
		row("last play", counts.LastPlayedAt.UTC().Format(time.RFC3339))
	}

	fmt.Fprintf(tw, "\nImport jobs\n")
	if len(byStatus) == 0 {
		row("none", "")
	}
	for _, status := range sortedKeys(byStatus) {
		row(status, humanInt(byStatus[status]))
	}

	var totals domain.Counters
	for _, job := range jobs {
		totals.Add(job.Counters)
	}
	if len(jobs) > 0 {
		// The listing is paged, so say which jobs these totals cover rather than
		// implying they are the whole history.
		fmt.Fprintf(tw, "\nCounters recorded by the %d most recent jobs\n", len(jobs))
		row("imported", humanInt(totals.Imported))
		row("duplicates", humanInt(totals.Duplicates))
		row("skipped", humanInt(totals.Skipped))
		row("rejected", humanInt(totals.Rejected))
		// The two numbers come from different places on purpose. They can differ
		// legitimately -- a user may have listens from live synchronisation, or
		// from a job whose rows were later removed with the job -- so this is
		// reported rather than asserted.
		row("listens claimed vs held", fmt.Sprintf("%s claimed, %s in the table",
			humanInt(totals.Imported), humanInt(counts.Listens)))
	}

	fmt.Fprintln(tw)
	return tw.Flush()
}

// sortedKeys keeps the output stable between runs, which matters when the point
// of the command is to compare two of them.
func sortedKeys(m map[string]int64) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
