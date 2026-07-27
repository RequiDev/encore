//go:build integration

package integration

import (
	"context"
	"sync"
	"testing"

	"github.com/RequiDev/encore/internal/postgres"
	"github.com/RequiDev/encore/test/harness"
)

// TestConcurrentMigrateIsSerialised is the regression test for a race a
// single-threaded local run could never surface and CI found immediately.
//
// goose's migration runner does not lock. Two processes reaching it at the same
// instant both issued the same CREATE TABLE, and one died with a duplicate-key
// error against a system catalogue — an error that reads like database
// corruption and gives no hint that it is a race. That is not a test-only
// concern: it is what a Compose stack with several replicas does, or any
// deployment using ENCORE_DATABASE_MIGRATE_ON_START.
//
// internal/postgres now takes a session-level advisory lock first, so the losers
// wait and then find nothing left to do. Removing that lock makes this test fail
// with exactly the error above, which is how it was verified.
func TestConcurrentMigrateIsSerialised(t *testing.T) {
	dsn := harness.DSN(t)

	// Start from nothing, so every racer genuinely competes to create the schema
	// rather than all of them finding it already current.
	if err := postgres.Reset(context.Background(), dsn); err != nil {
		t.Fatalf("reset schema: %v", err)
	}

	const racers = 6
	var wg sync.WaitGroup
	errs := make([]error, racers)
	start := make(chan struct{})

	for i := range racers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			errs[i] = postgres.Migrate(context.Background(), dsn, harness.Discard())
		}(i)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("concurrent migration %d failed: %v", i, err)
		}
	}

	status, err := postgres.Status(context.Background(), dsn)
	if err != nil {
		t.Fatalf("read schema status: %v", err)
	}
	if !status.UpToDate() {
		t.Fatalf("schema is not current after concurrent migration: %+v", status)
	}
}
