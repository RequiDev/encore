# Phase 1 — Latent Statistics Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Turn two bodies of data Encore already stores but never reads — `artists.genres` and the eight playback-context columns on `listens` — into statistics, endpoints and pages.

**Architecture:** Three new files in `internal/stats` compose SQL from the package's existing `rangeFilter`/`blacklistFilter` fragments, four new handlers in `internal/httpapi` follow the existing `callerAndRange` prologue, and two new React pages follow the existing TanStack Query patterns. Every statistic returns a coverage denominator alongside its value because both data sets are partial.

**Tech Stack:** Go 1.26, pgx/v5, PostgreSQL 17, React 19 + TypeScript + TanStack Query + Vite.

**Spec:** [`docs/design/2026-07-29-phase-1-latent-statistics-design.md`](../../design/2026-07-29-phase-1-latent-statistics-design.md)

## Global Constraints

- **No migration.** This phase adds no table and alters none. If you find yourself writing SQL DDL, stop — you have misread the plan.
- **No new Go module dependency.** `go.mod` gains nothing.
- **No Spotify API call and no OAuth scope change.** `internal/config/config.go:398` is not touched in this phase.
- **Every new SQL statement must be registered** in `statements()` in `internal/stats/stats_test.go` with its exact parameter count. `TestParameterNumberingIsContiguous` and `TestBlacklistIsAppliedEverywhere` will fail otherwise, and that is the point of them.
- **Every statement that reads `listens` or `listen_daily_rollup` must compose `blacklistFilter`**, via `rangeFilter` for the fact table or directly for the rollup.
- **Every coverage-bearing payload uses one JSON shape:** `{"value": <number>, "covered": <int>, "total": <int>}`.
- **Coverage denominators for context statistics are per column** — `count(*) FILTER (WHERE <column> IS NOT NULL)`, never `WHERE source = 2`.
- Run `make lint` before every commit. It runs `gofmt -l`, `go vet`, `staticcheck`, and on the web side `eslint`, `prettier` and `tsc`.
- Unit tests: `make test` (`go test -race -count=1 ./...`).
- Integration tests: `make test-integration`, which needs a database. Start one with `make db-up && make migrate`.
- Commit style: `Area: lowercase summary`, with a body explaining *why*. End every commit message with `Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>`.

---

## File Structure

| File | Responsibility |
|---|---|
| `internal/stats/genre.go` | **Create.** Genre aggregation: `TopGenres`, `GenreTimeline`, genre coverage |
| `internal/stats/taste.go` | **Create.** Obscurity score and release-year lag |
| `internal/stats/platform.go` | **Create.** `platformFamily` — pure string classifier, no SQL |
| `internal/stats/context.go` | **Create.** Playback-context aggregation |
| `internal/stats/stats_test.go` | **Modify.** Register the new statements |
| `internal/httpapi/stats.go` | **Modify.** Four handlers |
| `internal/httpapi/dto.go` | **Modify.** Response types |
| `internal/httpapi/router.go` | **Modify.** Four routes |
| `internal/httpapi/share.go` | **Modify.** Genres and obscurity on a share |
| `internal/spotify/models.go` | **Modify.** Delete the dead `PreviewURL` field |
| `test/integration/genrestats_test.go` | **Create.** Genre and taste integration tests |
| `test/integration/contextstats_test.go` | **Create.** Playback-context integration tests |
| `web/src/lib/types.ts` | **Modify.** Response types |
| `web/src/lib/query.ts` | **Modify.** Query keys |
| `web/src/pages/Genres.tsx` | **Create.** Top genres |
| `web/src/pages/Habits.tsx` | **Create.** How you listen |
| `web/src/App.tsx` | **Modify.** Routes |
| `web/src/pages/Dashboard.tsx` | **Modify.** Two cards |

---

### Task 1: Genre aggregation

**Files:**
- Create: `internal/stats/genre.go`
- Modify: `internal/stats/stats_test.go` (the `statements()` registry)
- Test: `test/integration/genrestats_test.go`

**Interfaces:**
- Consumes: `rangeFilter`, `blacklistFilter`, `scope`, `tzArg` from `internal/stats/stats.go`; `useRollup`, `HasDirtyDays` from `internal/stats/rollup.go`; `store.UUIDArg`, `postgres.Classify`.
- Produces:
  - `type Coverage struct { Covered, Total int64 }`
  - `type Genre struct { Genre string; Plays, MsPlayed int64 }`
  - `type GenrePage struct { Genres []Genre; Total int64; Coverage Coverage }`
  - `func (s *Service) TopGenres(ctx context.Context, q store.Querier, userID uuid.UUID, r domain.TimeRange, tz string, limit, offset int) (GenrePage, error)`
  - Package constants `trackGenreCTE`, and vars `topGenresFactSQL`, `topGenresRollupSQL`, `genreCoverageSQL`.

- [ ] **Step 1: Write the failing integration test**

Create `test/integration/genrestats_test.go`. The existing `seedStats` fixture in `test/integration/stats_test.go` already seeds exactly what this needs: `art-x` is tagged `rock`, `art-y` is `jazz`, `art-z` is `folk`, and `trk-a` credits **both** `art-x` and `art-y`.

```go
//go:build integration

package integration

import (
	"testing"
)

// TestTopGenresCountsEachListenOncePerGenre pins the counting rule: a listen
// contributes one play to each distinct genre across all of its credited
// artists, and a genre shared by two credited artists is still one play.
//
// From the shared fixture: trk-a plays four times and credits art-x (rock) and
// art-y (jazz), trk-b twice-over-one play credits art-x, trk-c plays twice
// crediting art-y, trk-d once crediting art-z.
//
//	rock: trk-a x4 + trk-b x1 = 5
//	jazz: trk-a x4 + trk-c x2 = 6
//	folk: trk-d x1            = 1
func TestTopGenresCountsEachListenOncePerGenre(t *testing.T) {
	f := seedStats(t)

	page, err := f.svc.TopGenres(f.env.Ctx(), f.env.Store.DB(), f.user.ID, f.fullRange(), f.tz, 10, 0)
	if err != nil {
		t.Fatalf("top genres: %v", err)
	}

	want := map[string]int64{"rock": 5, "jazz": 6, "folk": 1}
	if len(page.Genres) != len(want) {
		t.Fatalf("got %d genres, want %d: %+v", len(page.Genres), len(want), page.Genres)
	}
	for _, g := range page.Genres {
		if want[g.Genre] != g.Plays {
			t.Errorf("%s: got %d plays, want %d", g.Genre, g.Plays, want[g.Genre])
		}
	}
	if page.Total != 3 {
		t.Errorf("total genres = %d, want 3", page.Total)
	}
}

// TestTopGenresDeduplicatesASharedGenre is the case the DISTINCT in
// trackGenreCTE exists for. Retagging art-y as rock means trk-a credits two
// artists who are both rock; the four plays of it must add four to rock, not
// eight.
func TestTopGenresDeduplicatesASharedGenre(t *testing.T) {
	f := seedStats(t)
	f.env.Exec(`UPDATE artists SET genres = ARRAY['rock'] WHERE id = 'art-y'`)

	page, err := f.svc.TopGenres(f.env.Ctx(), f.env.Store.DB(), f.user.ID, f.fullRange(), f.tz, 10, 0)
	if err != nil {
		t.Fatalf("top genres: %v", err)
	}

	var rock int64
	for _, g := range page.Genres {
		if g.Genre == "rock" {
			rock = g.Plays
		}
	}
	// trk-a x4 (both artists rock, counted once) + trk-b x1 + trk-c x2 = 7
	if rock != 7 {
		t.Errorf("rock = %d plays, want 7 — a genre shared by two credited artists was double counted", rock)
	}
}

// TestGenreCoverageExcludesUnenrichedArtists is what stops a fresh instance from
// rendering an empty chart that looks like a bug. Stripping art-x's genres
// leaves every listen of trk-b uncovered; trk-a stays covered because art-y
// still supplies one.
func TestGenreCoverageExcludesUnenrichedArtists(t *testing.T) {
	f := seedStats(t)
	f.env.Exec(`UPDATE artists SET genres = '{}' WHERE id = 'art-x'`)

	page, err := f.svc.TopGenres(f.env.Ctx(), f.env.Store.DB(), f.user.ID, f.fullRange(), f.tz, 10, 0)
	if err != nil {
		t.Fatalf("top genres: %v", err)
	}

	// Eight listens total; the one play of trk-b is the only one with no genred artist.
	if page.Coverage.Total != 8 {
		t.Errorf("coverage total = %d, want 8", page.Coverage.Total)
	}
	if page.Coverage.Covered != 7 {
		t.Errorf("coverage covered = %d, want 7", page.Coverage.Covered)
	}
}

// TestTopGenresRespectsTheBlacklist checks the fragment did its job. Blacklisting
// art-x removes every listen of any track crediting it — trk-a and trk-b — so
// rock disappears entirely and jazz keeps only trk-c's two plays.
func TestTopGenresRespectsTheBlacklist(t *testing.T) {
	f := seedStats(t)
	f.env.Exec(`INSERT INTO user_blacklisted_artists (user_id, artist_id) VALUES ($1, 'art-x')`, f.user.ID)

	page, err := f.svc.TopGenres(f.env.Ctx(), f.env.Store.DB(), f.user.ID, f.fullRange(), f.tz, 10, 0)
	if err != nil {
		t.Fatalf("top genres: %v", err)
	}

	got := map[string]int64{}
	for _, g := range page.Genres {
		got[g.Genre] = g.Plays
	}
	if _, ok := got["rock"]; ok {
		t.Error("rock survived blacklisting its only artist")
	}
	if got["jazz"] != 2 {
		t.Errorf("jazz = %d plays, want 2 (trk-c only)", got["jazz"])
	}
	if got["folk"] != 1 {
		t.Errorf("folk = %d plays, want 1", got["folk"])
	}
}

// TestTopGenresRollupMatchesTheFactTable is the test that makes the rollup path
// safe to have at all. Two statements answering one question must agree, or a
// wide range silently returns different numbers from a narrow one.
//
// The range is deliberately wide and aligned to local midnight so useRollup says
// yes; refreshing the rollup first is what makes the comparison meaningful,
// because a dirty rollup would send both calls down the fact-table path and the
// test would pass without ever exercising the rollup SQL.
func TestTopGenresRollupMatchesTheFactTable(t *testing.T) {
	f := seedStats(t)

	// Drain the dirty queue so the rollup is current and eligible.
	if err := f.svc.RefreshDirtyDays(f.env.Ctx(), 1000); err != nil {
		t.Fatalf("refresh rollups: %v", err)
	}

	// RollupMinRange is 90 days, so a shorter range would take the fact-table
	// path in both calls and the comparison would prove nothing. Six months,
	// starting at a local midnight, clears it.
	wide := f.fullRange()
	wide.To = wide.From.AddDate(0, 6, 0)

	dirty, err := f.svc.HasDirtyDays(f.env.Ctx(), f.env.Store.DB(), f.user.ID, wide, f.tz)
	if err != nil {
		t.Fatalf("dirty check: %v", err)
	}
	if dirty {
		t.Fatal("rollups are still dirty after a refresh; this test would not exercise the rollup path")
	}

	viaRollup, err := f.svc.TopGenres(f.env.Ctx(), f.env.Store.DB(), f.user.ID, wide, f.tz, 50, 0)
	if err != nil {
		t.Fatalf("top genres via rollup: %v", err)
	}

	// Force the fact-table path by dirtying a day inside the range.
	f.env.Exec(`INSERT INTO rollup_dirty_days (user_id, day) VALUES ($1, DATE '2024-01-01')
	            ON CONFLICT DO NOTHING`, f.user.ID)
	viaFacts, err := f.svc.TopGenres(f.env.Ctx(), f.env.Store.DB(), f.user.ID, wide, f.tz, 50, 0)
	if err != nil {
		t.Fatalf("top genres via facts: %v", err)
	}

	if viaRollup.Total != viaFacts.Total {
		t.Fatalf("totals differ: rollup %d, facts %d", viaRollup.Total, viaFacts.Total)
	}
	if len(viaRollup.Genres) != len(viaFacts.Genres) {
		t.Fatalf("row counts differ: rollup %d, facts %d", len(viaRollup.Genres), len(viaFacts.Genres))
	}
	for i := range viaRollup.Genres {
		if viaRollup.Genres[i] != viaFacts.Genres[i] {
			t.Errorf("row %d differs: rollup %+v, facts %+v", i, viaRollup.Genres[i], viaFacts.Genres[i])
		}
	}
}

// TestTopGenresEmptyRangeIsNotAnError guards the state every new instance is in.
func TestTopGenresEmptyRangeIsNotAnError(t *testing.T) {
	f := seedStats(t)
	empty := f.fullRange()
	empty.From = empty.To

	page, err := f.svc.TopGenres(f.env.Ctx(), f.env.Store.DB(), f.user.ID, empty, f.tz, 10, 0)
	if err != nil {
		t.Fatalf("top genres over an empty range: %v", err)
	}
	if len(page.Genres) != 0 || page.Total != 0 {
		t.Errorf("expected no genres, got %+v", page)
	}
	if page.Coverage.Covered != 0 || page.Coverage.Total != 0 {
		t.Errorf("expected zero coverage, got %+v", page.Coverage)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `make db-up && make migrate` (once), then
`ENCORE_TEST_DATABASE_URL="postgres://encore:encore@localhost:5432/encore?sslmode=disable" go test -tags=integration -run TestTopGenres -count=1 ./test/integration/`

Expected: FAIL — `f.svc.TopGenres undefined (type *stats.Service has no field or method TopGenres)`.

- [ ] **Step 3: Write the implementation**

Create `internal/stats/genre.go`:

```go
package stats

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/RequiDev/encore/internal/domain"
	"github.com/RequiDev/encore/internal/postgres"
	"github.com/RequiDev/encore/internal/store"
)

// Coverage is the denominator every partial statistic carries.
//
// Both bodies of data this file and context.go read are incomplete by nature —
// genres exist only where enrichment has resolved the artist — so a bare
// percentage would be a plausible-looking lie. Covered and Total travel with the
// value all the way to the page, which states them in words.
type Coverage struct {
	Covered int64
	Total   int64
}

// Genre is one row of the ranked list.
type Genre struct {
	Genre    string
	Plays    int64
	MsPlayed int64
}

// GenrePage is one page of the ranking, the length of the whole list, and how
// much of the range the ranking could see.
type GenrePage struct {
	Genres   []Genre
	Total    int64
	Coverage Coverage
}

// trackGenreCTE maps every catalogue track to the distinct genres of its
// credited artists.
//
// The DISTINCT is the counting rule: genres belong to artists, a track may
// credit several, and a track whose two credited artists are both tagged
// "indie rock" must contribute one play to it rather than two. Deduplicating
// here rather than per listen is equivalent and far cheaper, because every play
// of a track has the same genre set by construction.
const trackGenreCTE = `
track_genre AS (
    SELECT DISTINCT ta.track_id, g.genre
    FROM track_artists ta
    JOIN artists a ON a.id = ta.artist_id
    CROSS JOIN LATERAL unnest(a.genres) AS g(genre)
)`

// Parameters are $1 user, $2 from, $3 to, $4 limit, $5 offset.
var topGenresFactSQL = fmt.Sprintf(`
WITH %s,
agg AS (
    SELECT tg.genre,
           count(*)::bigint                      AS plays,
           coalesce(sum(l.ms_played), 0)::bigint AS ms
    FROM listens l
    JOIN track_genre tg ON tg.track_id = l.track_id
    WHERE %s
    GROUP BY tg.genre
),
total AS (SELECT count(*)::bigint AS n FROM agg)
SELECT t.n, a.genre, a.plays, a.ms
FROM total t
LEFT JOIN (
    SELECT genre, plays, ms FROM agg ORDER BY plays DESC, ms DESC, genre LIMIT $4 OFFSET $5
) a ON true
ORDER BY a.plays DESC NULLS LAST, a.ms DESC, a.genre`,
	trackGenreCTE, rangeFilter("l", "$1", "$2", "$3"))

// The rollup variant reads whole local days and therefore needs the timezone.
// Parameters are $1 user, $2 from, $3 to, $4 limit, $5 offset, $6 timezone.
var topGenresRollupSQL = fmt.Sprintf(`
WITH %s,
agg AS (
    SELECT tg.genre,
           sum(r.plays)::bigint             AS plays,
           coalesce(sum(r.ms), 0)::bigint   AS ms
    FROM listen_daily_rollup r
    JOIN track_genre tg ON tg.track_id = r.track_id
    WHERE r.user_id = $1
      AND r.day >= (($2::timestamptz AT TIME ZONE $6::text)::date)
      AND r.day <  (($3::timestamptz AT TIME ZONE $6::text)::date)
      AND %s
    GROUP BY tg.genre
),
total AS (SELECT count(*)::bigint AS n FROM agg)
SELECT t.n, a.genre, a.plays, a.ms
FROM total t
LEFT JOIN (
    SELECT genre, plays, ms FROM agg ORDER BY plays DESC, ms DESC, genre LIMIT $4 OFFSET $5
) a ON true
ORDER BY a.plays DESC NULLS LAST, a.ms DESC, a.genre`,
	trackGenreCTE, blacklistFilter("r"))

// genreCoverageSQL counts how many in-range listens resolve to at least one
// artist carrying a genre.
//
// A LEFT JOIN onto the distinct set of genred tracks, rather than an EXISTS per
// row, so the planner sees one hash join instead of a correlated subquery.
// Parameters are $1 user, $2 from, $3 to.
var genreCoverageSQL = fmt.Sprintf(`
WITH genred_track AS (
    SELECT DISTINCT ta.track_id
    FROM track_artists ta
    JOIN artists a ON a.id = ta.artist_id
    WHERE cardinality(a.genres) > 0
)
SELECT count(*)::bigint, count(gt.track_id)::bigint
FROM listens l
LEFT JOIN genred_track gt ON gt.track_id = l.track_id
WHERE %s`, rangeFilter("l", "$1", "$2", "$3"))

// TopGenres ranks the genres of the artists behind the range's listening.
//
// Genre plays sum to more than total plays, because a track counts toward each
// of its genres. That is stated on the page rather than normalised away: dividing
// a play across its genres produces fractional counts nobody can reason about.
func (s *Service) TopGenres(
	ctx context.Context,
	q store.Querier,
	userID uuid.UUID,
	r domain.TimeRange,
	tz string,
	limit, offset int,
) (GenrePage, error) {
	loc, err := scope(userID, r, tz)
	if err != nil {
		return GenrePage{}, err
	}
	limit, offset = clampLimit(limit), clampOffset(offset)

	dirty, err := s.HasDirtyDays(ctx, q, userID, r, tz)
	if err != nil {
		return GenrePage{}, err
	}

	var (
		sql  = topGenresFactSQL
		args = []any{store.UUIDArg(userID), r.From.UTC(), r.To.UTC(), limit, offset}
	)
	if useRollup(r, loc, dirty) {
		sql = topGenresRollupSQL
		args = append(args, tzArg(tz))
	}

	page := GenrePage{Genres: make([]Genre, 0, limit)}
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return GenrePage{}, postgres.Classify("top genres", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			g     Genre
			name  *string
			plays *int64
			ms    *int64
		)
		if err := rows.Scan(&page.Total, &name, &plays, &ms); err != nil {
			return GenrePage{}, postgres.Classify("scan top genres", err)
		}
		// A page beyond the end of the list still reports the list's length, so
		// the row carries a NULL genre rather than not existing.
		if name == nil {
			continue
		}
		g.Genre, g.Plays, g.MsPlayed = *name, *plays, *ms
		page.Genres = append(page.Genres, g)
	}
	if err := rows.Err(); err != nil {
		return GenrePage{}, postgres.Classify("top genres", err)
	}

	if err := q.QueryRow(ctx, genreCoverageSQL,
		store.UUIDArg(userID), r.From.UTC(), r.To.UTC(),
	).Scan(&page.Coverage.Total, &page.Coverage.Covered); err != nil {
		return GenrePage{}, postgres.Classify("genre coverage", err)
	}
	return page, nil
}
```

`clampLimit` and `clampOffset` already exist in `internal/stats/stats.go:137,147` and are what every other paginated statistic uses. Do not add a third paging helper.

- [ ] **Step 4: Register the new statements**

In `internal/stats/stats_test.go`, add to the slice returned by `statements()`:

```go
		{name: "topGenresFact", sql: topGenresFactSQL, params: 5},
		{name: "topGenresRollup", sql: topGenresRollupSQL, params: 6},
		{name: "genreCoverage", sql: genreCoverageSQL, params: 3},
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `make test` — expected PASS, including `TestParameterNumberingIsContiguous` and `TestBlacklistIsAppliedEverywhere` for the three new entries.

Run: `ENCORE_TEST_DATABASE_URL="postgres://encore:encore@localhost:5432/encore?sslmode=disable" go test -tags=integration -run TestTopGenres -count=1 ./test/integration/` — expected PASS, all five tests.

- [ ] **Step 6: Lint and commit**

```bash
make lint
git add internal/stats/genre.go internal/stats/stats.go internal/stats/stats_test.go test/integration/genrestats_test.go
git commit -m "Stats: rank the genres already stored on every artist

artists.genres has been enriched, stored and rendered as chips on one page
since the catalogue existed. Nothing aggregated it.

The counting rule is the only real decision. Genres belong to artists and a
track may credit several, so a listen contributes one play to each distinct
genre across all of them — deduplicated, or a track whose two artists are
both tagged indie rock would count twice for it. The dedup lives in a CTE
keyed on track rather than on listen, which is equivalent because every play
of a track has the same genre set, and much cheaper.

Genre plays therefore sum to more than total plays. The page says so. The
alternative, dividing each play across its genres, produces a 0.31 in a
tooltip and explains nothing.

Coverage travels with the ranking because a freshly imported history has
thousands of locally-minted artists with no genres at all, and an empty
chart is indistinguishable from a broken one.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
```

---

### Task 2: Genre timeline

**Files:**
- Modify: `internal/stats/genre.go`
- Modify: `internal/stats/stats_test.go`
- Test: `test/integration/genrestats_test.go`

**Interfaces:**
- Consumes: `trackGenreCTE`, `Coverage` from Task 1; `checkInterval` from `internal/stats/timeline.go`.
- Produces:
  - `type GenrePoint struct { Bucket time.Time; Genre string; Plays, MsPlayed int64 }`
  - `func (s *Service) GenreTimeline(ctx context.Context, q store.Querier, userID uuid.UUID, r domain.TimeRange, tz string, interval domain.Interval, genres []string) ([]GenrePoint, error)`

- [ ] **Step 1: Write the failing test**

Append to `test/integration/genrestats_test.go`:

```go
// TestGenreTimelineReturnsACompleteGrid checks the property every chart in
// Encore relies on: an empty bucket is a zero, never a missing point, and the
// caller's genre list fixes the series so they stay stable while paging.
//
// 2024-01-04 is silent in the fixture, so it is the bucket that matters.
func TestGenreTimelineReturnsACompleteGrid(t *testing.T) {
	f := seedStats(t)

	points, err := f.svc.GenreTimeline(f.env.Ctx(), f.env.Store.DB(), f.user.ID,
		f.fullRange(), f.tz, domain.IntervalDay, []string{"rock", "jazz"})
	if err != nil {
		t.Fatalf("genre timeline: %v", err)
	}

	// Ten days in fullRange, two genres.
	if len(points) != 20 {
		t.Fatalf("got %d points, want 20", len(points))
	}

	type key struct {
		day   string
		genre string
	}
	got := map[key]int64{}
	for _, p := range points {
		got[key{p.Bucket.In(f.loc).Format("2006-01-02"), p.Genre}] = p.Plays
	}

	// rock is art-x: trk-a x3 and trk-b x1 on the 1st, trk-a x1 on the 3rd.
	if got[key{"2024-01-01", "rock"}] != 4 {
		t.Errorf("rock on the 1st = %d, want 4", got[key{"2024-01-01", "rock"}])
	}
	// jazz is art-y, credited on trk-a and trk-c: three on the 1st, two on the 2nd.
	if got[key{"2024-01-02", "jazz"}] != 2 {
		t.Errorf("jazz on the 2nd = %d, want 2", got[key{"2024-01-02", "jazz"}])
	}
	if v, ok := got[key{"2024-01-04", "rock"}]; !ok || v != 0 {
		t.Errorf("the silent day is missing or non-zero for rock: %v %v", v, ok)
	}
}

// TestGenreTimelineWithNoGenresIsEmpty guards the degenerate call rather than
// letting it build a grid of nothing.
func TestGenreTimelineWithNoGenresIsEmpty(t *testing.T) {
	f := seedStats(t)

	points, err := f.svc.GenreTimeline(f.env.Ctx(), f.env.Store.DB(), f.user.ID,
		f.fullRange(), f.tz, domain.IntervalDay, nil)
	if err != nil {
		t.Fatalf("genre timeline: %v", err)
	}
	if len(points) != 0 {
		t.Errorf("got %d points for no genres, want 0", len(points))
	}
}
```

Add `"github.com/RequiDev/encore/internal/domain"` to that file's imports.

- [ ] **Step 2: Run the test to verify it fails**

Run: `ENCORE_TEST_DATABASE_URL="postgres://encore:encore@localhost:5432/encore?sslmode=disable" go test -tags=integration -run TestGenreTimeline -count=1 ./test/integration/`
Expected: FAIL — `f.svc.GenreTimeline undefined`.

- [ ] **Step 3: Write the implementation**

Append to `internal/stats/genre.go`:

```go
// GenrePoint is one bucket of one genre's share of a timeline.
type GenrePoint struct {
	Bucket   time.Time
	Genre    string
	Plays    int64
	MsPlayed int64
}

// genreTimelineSQL buckets the requested genres over the range.
//
// The series are cross-joined from the caller's list rather than derived from
// the data, for the same reason the bucket grid is generated rather than
// grouped: a genre with no plays in a bucket must appear as a zero. Passing the
// list in also keeps a chart's series stable while the ranking beneath it is
// paged or re-ranged.
//
// Parameters are $1 user, $2 from, $3 to, $4 timezone, $5 interval, $6 genres.
var genreTimelineSQL = fmt.Sprintf(`
WITH %s,
bounds AS (
    SELECT date_trunc($5::text, ($2::timestamptz AT TIME ZONE $4::text)) AS lo,
           ($3::timestamptz AT TIME ZONE $4::text) AS hi
),
buckets AS (
    SELECT generate_series(b.lo, b.hi - interval '1 microsecond', ('1 ' || $5::text)::interval) AS bucket
    FROM bounds b
),
series AS (SELECT g.genre FROM unnest($6::text[]) AS g(genre)),
agg AS (
    SELECT date_trunc($5::text, (l.played_at AT TIME ZONE $4::text)) AS bucket,
           tg.genre,
           count(*)::bigint                      AS plays,
           coalesce(sum(l.ms_played), 0)::bigint AS ms
    FROM listens l
    JOIN track_genre tg ON tg.track_id = l.track_id
    WHERE %s AND tg.genre = ANY($6::text[])
    GROUP BY 1, 2
)
SELECT b.bucket, s.genre, coalesce(a.plays, 0)::bigint, coalesce(a.ms, 0)::bigint
FROM buckets b
CROSS JOIN series s
LEFT JOIN agg a ON a.bucket = b.bucket AND a.genre = s.genre
ORDER BY b.bucket, s.genre`,
	trackGenreCTE, rangeFilter("l", "$1", "$2", "$3"))

// GenreTimeline buckets the named genres across the range, so taste drift is
// visible rather than inferred from two rankings side by side.
//
// The caller names the genres. The page passes the range's top eight, which is
// where a stacked area chart stops being readable.
func (s *Service) GenreTimeline(
	ctx context.Context,
	q store.Querier,
	userID uuid.UUID,
	r domain.TimeRange,
	tz string,
	interval domain.Interval,
	genres []string,
) ([]GenrePoint, error) {
	loc, err := scope(userID, r, tz)
	if err != nil {
		return nil, err
	}
	if err := checkInterval(r, interval); err != nil {
		return nil, err
	}
	if len(genres) == 0 {
		return nil, nil
	}

	rows, err := q.Query(ctx, genreTimelineSQL,
		store.UUIDArg(userID), r.From.UTC(), r.To.UTC(), tzArg(tz), string(interval), genres)
	if err != nil {
		return nil, postgres.Classify("genre timeline", err)
	}
	defer rows.Close()

	out := make([]GenrePoint, 0, len(genres)*16)
	for rows.Next() {
		var p GenrePoint
		if err := rows.Scan(&p.Bucket, &p.Genre, &p.Plays, &p.MsPlayed); err != nil {
			return nil, postgres.Classify("scan genre timeline", err)
		}
		// date_trunc returns a bucket boundary as a wall-clock timestamp with no
		// zone; inLocation reattaches the user's, which is what makes the point
		// mean the local midnight it was computed as.
		p.Bucket = inLocation(p.Bucket, loc)
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, postgres.Classify("genre timeline", err)
	}
	return out, nil
}
```

Add `"time"` to the file's imports. `inLocation` is at `internal/stats/stats.go:121` and takes a `*time.Location` — the `loc` returned by `scope`, **not** the `tz` string.

- [ ] **Step 4: Register the statement**

In `statements()`:

```go
		{name: "genreTimeline", sql: genreTimelineSQL, params: 6},
```

- [ ] **Step 5: Run the tests**

Run: `make test` — expected PASS.
Run: `ENCORE_TEST_DATABASE_URL="postgres://encore:encore@localhost:5432/encore?sslmode=disable" go test -tags=integration -run TestGenreTimeline -count=1 ./test/integration/` — expected PASS.

- [ ] **Step 6: Lint and commit**

```bash
make lint
git add internal/stats/genre.go internal/stats/stats_test.go test/integration/genrestats_test.go
git commit -m "Stats: bucket genres over time

Two rankings side by side show that taste changed; they do not show when.

The series come from the caller's genre list rather than from the data, for
the same reason the bucket grid is generated rather than grouped — a genre
with no plays in a week has to be a zero, not a gap the chart guesses at.
Passing the list in also keeps the series stable while the ranking beneath
is paged, which grouping would not.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
```

---

### Task 3: Taste statistics

**Files:**
- Create: `internal/stats/taste.go`
- Modify: `internal/stats/stats_test.go`
- Test: `test/integration/genrestats_test.go`

**Interfaces:**
- Consumes: `Coverage` from Task 1; `rangeFilter`, `scope`, `tzArg`.
- Produces:
  - `type Taste struct { Obscurity float64; ObscurityCoverage Coverage; ReleaseLagYears float64; ReleaseLagCoverage Coverage }`
  - `func (s *Service) Taste(ctx context.Context, q store.Querier, userID uuid.UUID, r domain.TimeRange, tz string) (Taste, error)`

- [ ] **Step 1: Write the failing test**

Append to `test/integration/genrestats_test.go`:

```go
// TestTasteObscurityIsPlayWeighted checks the mean is over listens rather than
// over artists: an artist played ten times must pull the score ten times as hard
// as one played once.
func TestTasteObscurityIsPlayWeighted(t *testing.T) {
	f := seedStats(t)
	f.env.Exec(`UPDATE artists SET popularity = 90 WHERE id = 'art-x'`)
	f.env.Exec(`UPDATE artists SET popularity = 30 WHERE id = 'art-y'`)
	f.env.Exec(`UPDATE artists SET popularity = 0  WHERE id = 'art-z'`)

	got, err := f.svc.Taste(f.env.Ctx(), f.env.Store.DB(), f.user.ID, f.fullRange(), f.tz)
	if err != nil {
		t.Fatalf("taste: %v", err)
	}

	// Per listen, over every credited artist:
	//   trk-a x4 credits art-x(90) and art-y(30) -> 8 artist-plays
	//   trk-b x1 credits art-x(90)               -> 1
	//   trk-c x2 credits art-y(30)               -> 2
	//   trk-d x1 credits art-z(0)                -> 1
	// (4*90 + 4*30 + 1*90 + 2*30 + 1*0) / 12 = 630/12 = 52.5
	if diff := got.Obscurity - 52.5; diff > 0.001 || diff < -0.001 {
		t.Errorf("obscurity = %v, want 52.5", got.Obscurity)
	}
	if got.ObscurityCoverage.Total != 8 || got.ObscurityCoverage.Covered != 8 {
		t.Errorf("obscurity coverage = %+v, want 8/8", got.ObscurityCoverage)
	}
}

// TestTasteObscurityExcludesUnresolvedArtists is the coverage half: an artist
// enrichment has not resolved carries popularity 0 by column default, and
// counting that as "not popular" would drag every fresh instance's score to zero.
func TestTasteObscurityExcludesUnresolvedArtists(t *testing.T) {
	f := seedStats(t)
	f.env.Exec(`UPDATE artists SET popularity = 80, metadata_state = 'resolved' WHERE id = 'art-x'`)
	f.env.Exec(`UPDATE artists SET popularity = 0,  metadata_state = 'pending'  WHERE id = 'art-y'`)
	f.env.Exec(`UPDATE artists SET popularity = 0,  metadata_state = 'pending'  WHERE id = 'art-z'`)

	got, err := f.svc.Taste(f.env.Ctx(), f.env.Store.DB(), f.user.ID, f.fullRange(), f.tz)
	if err != nil {
		t.Fatalf("taste: %v", err)
	}
	if diff := got.Obscurity - 80.0; diff > 0.001 || diff < -0.001 {
		t.Errorf("obscurity = %v, want 80 — an unresolved artist was counted as popularity 0", got.Obscurity)
	}
	// trk-a and trk-b credit art-x; that is five listens of eight.
	if got.ObscurityCoverage.Covered != 5 || got.ObscurityCoverage.Total != 8 {
		t.Errorf("obscurity coverage = %+v, want 5/8", got.ObscurityCoverage)
	}
}

// TestTasteReleaseLag answers "how old is the music you listen to".
func TestTasteReleaseLag(t *testing.T) {
	f := seedStats(t)

	got, err := f.svc.Taste(f.env.Ctx(), f.env.Store.DB(), f.user.ID, f.fullRange(), f.tz)
	if err != nil {
		t.Fatalf("taste: %v", err)
	}

	// All plays are in 2024. alb-1 (2010) carries trk-a x4 and trk-b x1;
	// alb-2 (2020) carries trk-c x2; alb-3 (2000) carries trk-d x1.
	// (5*14 + 2*4 + 1*24) / 8 = 102/8 = 12.75
	if diff := got.ReleaseLagYears - 12.75; diff > 0.001 || diff < -0.001 {
		t.Errorf("release lag = %v, want 12.75", got.ReleaseLagYears)
	}
	if got.ReleaseLagCoverage.Covered != 8 || got.ReleaseLagCoverage.Total != 8 {
		t.Errorf("release lag coverage = %+v, want 8/8", got.ReleaseLagCoverage)
	}
}

// TestTasteEmptyRangeIsNotAnError guards the same state as the genre case.
func TestTasteEmptyRangeIsNotAnError(t *testing.T) {
	f := seedStats(t)
	empty := f.fullRange()
	empty.From = empty.To

	got, err := f.svc.Taste(f.env.Ctx(), f.env.Store.DB(), f.user.ID, empty, f.tz)
	if err != nil {
		t.Fatalf("taste over an empty range: %v", err)
	}
	if got.Obscurity != 0 || got.ObscurityCoverage.Total != 0 {
		t.Errorf("expected a zeroed taste, got %+v", got)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `ENCORE_TEST_DATABASE_URL="postgres://encore:encore@localhost:5432/encore?sslmode=disable" go test -tags=integration -run TestTaste -count=1 ./test/integration/`
Expected: FAIL — `f.svc.Taste undefined`.

- [ ] **Step 3: Write the implementation**

Create `internal/stats/taste.go`:

```go
package stats

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/RequiDev/encore/internal/domain"
	"github.com/RequiDev/encore/internal/postgres"
	"github.com/RequiDev/encore/internal/store"
)

// Taste is the pair of derived scores that describe *what kind* of catalogue
// somebody listens to, rather than which of it.
type Taste struct {
	// Obscurity is the play-weighted mean of Spotify's artist popularity, 0 to
	// 100. High means mainstream. It is named for the question people ask.
	Obscurity         float64
	ObscurityCoverage Coverage

	// ReleaseLagYears is the play-weighted mean gap between when an album came
	// out and when it was played.
	ReleaseLagYears    float64
	ReleaseLagCoverage Coverage
}

// obscuritySQL averages artist popularity over listens, weighting by play.
//
// Only artists enrichment has resolved contribute. popularity defaults to 0 for
// a pending row, and counting that as "not popular at all" would drag every
// freshly imported instance's score toward zero and call it a taste for the
// obscure. The denominator is therefore listens having at least one resolved
// credited artist, which is what the coverage half of the result reports.
//
// Parameters are $1 user, $2 from, $3 to.
var obscuritySQL = fmt.Sprintf(`
WITH scoped AS (
    SELECT l.id, l.track_id FROM listens l WHERE %s
),
weighted AS (
    SELECT s.id, avg(a.popularity)::float8 AS pop
    FROM scoped s
    JOIN track_artists ta ON ta.track_id = s.track_id
    JOIN artists a ON a.id = ta.artist_id AND a.metadata_state = 'resolved'
    GROUP BY s.id
)
SELECT coalesce(avg(w.pop), 0)::float8,
       (SELECT count(*) FROM scoped)::bigint,
       count(w.id)::bigint
FROM weighted w`, rangeFilter("l", "$1", "$2", "$3"))

// releaseLagSQL averages the gap between release and play.
//
// Parameters are $1 user, $2 from, $3 to, $4 timezone. The play year is read in
// the listener's own timezone, because a play at 00:30 on 1 January belongs to
// the year they were living in, not the one UTC was.
var releaseLagSQL = fmt.Sprintf(`
WITH scoped AS (
    SELECT l.id, l.track_id, l.played_at FROM listens l WHERE %s
),
lagged AS (
    SELECT s.id,
           (extract(year FROM (s.played_at AT TIME ZONE $4::text))
              - extract(year FROM al.release_date))::float8 AS lag
    FROM scoped s
    JOIN tracks t  ON t.id = s.track_id
    JOIN albums al ON al.id = t.album_id AND al.release_date IS NOT NULL
)
SELECT coalesce(avg(l2.lag), 0)::float8,
       (SELECT count(*) FROM scoped)::bigint,
       count(l2.id)::bigint
FROM lagged l2`, rangeFilter("l", "$1", "$2", "$3"))

// Taste computes both scores. They are one endpoint because they answer one
// question — what kind of catalogue this is — and neither is worth a page.
func (s *Service) Taste(
	ctx context.Context,
	q store.Querier,
	userID uuid.UUID,
	r domain.TimeRange,
	tz string,
) (Taste, error) {
	if _, err := scope(userID, r, tz); err != nil {
		return Taste{}, err
	}

	var out Taste
	if err := q.QueryRow(ctx, obscuritySQL,
		store.UUIDArg(userID), r.From.UTC(), r.To.UTC(),
	).Scan(&out.Obscurity, &out.ObscurityCoverage.Total, &out.ObscurityCoverage.Covered); err != nil {
		return Taste{}, postgres.Classify("obscurity score", err)
	}

	if err := q.QueryRow(ctx, releaseLagSQL,
		store.UUIDArg(userID), r.From.UTC(), r.To.UTC(), tzArg(tz),
	).Scan(&out.ReleaseLagYears, &out.ReleaseLagCoverage.Total, &out.ReleaseLagCoverage.Covered); err != nil {
		return Taste{}, postgres.Classify("release lag", err)
	}
	return out, nil
}
```

- [ ] **Step 4: Register the statements**

```go
		{name: "obscurity", sql: obscuritySQL, params: 3},
		{name: "releaseLag", sql: releaseLagSQL, params: 4},
```

- [ ] **Step 5: Run the tests**

Run: `make test` — expected PASS.
Run: `ENCORE_TEST_DATABASE_URL="postgres://encore:encore@localhost:5432/encore?sslmode=disable" go test -tags=integration -run TestTaste -count=1 ./test/integration/` — expected PASS, four tests.

- [ ] **Step 6: Lint and commit**

```bash
make lint
git add internal/stats/taste.go internal/stats/stats_test.go test/integration/genrestats_test.go
git commit -m "Stats: how mainstream, and how old

artists.popularity and albums.release_date have both been stored since the
catalogue existed. Popularity fed nothing at all; release_date fed one
average that nobody could act on.

Both scores are play-weighted rather than averaged over the catalogue,
because the question is about listening and not about the library: an artist
played once should not weigh the same as one played two hundred times.

The obscurity score counts only artists enrichment has resolved. popularity
defaults to 0 on a pending row, and averaging that in would drag every fresh
instance toward zero and then describe it as a taste for the obscure. What
is excluded is reported rather than hidden, which is the same rule the genre
ranking follows.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
```

---

### Task 4: Platform classifier

**Files:**
- Create: `internal/stats/platform.go`
- Create: `internal/stats/platform_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces:
  - Exported constants `PlatformAndroid`, `PlatformIOS`, `PlatformWindows`, `PlatformMacOS`, `PlatformLinux`, `PlatformWeb`, `PlatformCast`, `PlatformPartner`, `PlatformOther` (all `string`).
  - `func PlatformFamily(raw string) string`

This is a pure function with no database, so it is a plain unit test and needs no integration run.

- [ ] **Step 1: Write the failing test**

Create `internal/stats/platform_test.go`:

```go
package stats

import "testing"

// TestPlatformFamily pins the classifier against the shapes Spotify's exports
// actually contain. The strings below are the real formats, not invented ones.
func TestPlatformFamily(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want string
	}{
		{"Android OS 10 API 29 (samsung, SM-G970F)", PlatformAndroid},
		{"android", PlatformAndroid},
		{"iOS 14.4.2 (iPhone12,1)", PlatformIOS},
		{"iOS 9.3.5 (iPad4,1)", PlatformIOS},
		{"Windows 10 (10.0.19042; x64)", PlatformWindows},
		{"windows", PlatformWindows},
		{"OS X 10.15.7 [x86 8]", PlatformMacOS},
		{"macos", PlatformMacOS},
		{"Linux [x86_64 0]", PlatformLinux},
		{"web_player", PlatformWeb},
		{"WebPlayer", PlatformWeb},
		{"cast", PlatformCast},
		{"Google Cast", PlatformCast},
		{"Partner sonos_inc bridge", PlatformPartner},
		{"not_applicable", PlatformOther},
		{"unknown", PlatformOther},
		{"", PlatformOther},
		{"something nobody has seen", PlatformOther},
	} {
		if got := PlatformFamily(tc.raw); got != tc.want {
			t.Errorf("PlatformFamily(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}
}

// TestPlatformFamilyPrefersTheMoreSpecificMatch is why the switch is ordered.
// A partner integration string can name the underlying OS, and it is a partner
// device first.
func TestPlatformFamilyPrefersTheMoreSpecificMatch(t *testing.T) {
	if got := PlatformFamily("Partner android_auto"); got != PlatformPartner {
		t.Errorf("a partner string naming Android classified as %q, want %q", got, PlatformPartner)
	}
	if got := PlatformFamily("Partner google cast"); got != PlatformPartner {
		t.Errorf("a partner string naming Cast classified as %q, want %q", got, PlatformPartner)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test -run TestPlatformFamily -count=1 ./internal/stats/`
Expected: FAIL — `undefined: PlatformFamily`.

- [ ] **Step 3: Write the implementation**

Create `internal/stats/platform.go`:

```go
package stats

import "strings"

// The platform families a listen is grouped into.
//
// listens.platform is free text straight from the export — "Android OS 10 API 29
// (samsung, SM-G970F)", "OS X 10.15.7 [x86 8]", "Partner sonos_inc" — and no two
// vintages agree on its shape. Grouping happens at read time rather than at
// import, so adding a family reclassifies the whole history without a backfill
// and without ever having thrown the original string away.
const (
	PlatformAndroid = "android"
	PlatformIOS     = "ios"
	PlatformWindows = "windows"
	PlatformMacOS   = "macos"
	PlatformLinux   = "linux"
	PlatformWeb     = "web"
	PlatformCast    = "cast"
	PlatformPartner = "partner"
	PlatformOther   = "other"
)

// PlatformFamily groups one raw platform string.
//
// The order of the cases is the whole design. A partner integration string can
// name the operating system underneath it, and it is a partner device first, so
// that case comes before every OS. Anything unrecognised is Other rather than
// dropped: a family nobody has seen yet must still be counted, or the
// denominators stop adding up.
func PlatformFamily(raw string) string {
	s := strings.ToLower(strings.TrimSpace(raw))
	switch {
	case s == "" || s == "not_applicable" || s == "unknown":
		return PlatformOther
	case strings.Contains(s, "partner"):
		return PlatformPartner
	case strings.Contains(s, "cast"):
		return PlatformCast
	case strings.Contains(s, "web_player"), strings.Contains(s, "webplayer"), strings.Contains(s, "web player"):
		return PlatformWeb
	case strings.Contains(s, "android"):
		return PlatformAndroid
	// Prefix rather than substring: "ios" appears inside ordinary words, and a
	// real iOS platform string always begins with it.
	case strings.HasPrefix(s, "ios"), strings.Contains(s, "iphone"), strings.Contains(s, "ipad"):
		return PlatformIOS
	case strings.Contains(s, "windows"):
		return PlatformWindows
	case strings.Contains(s, "os x"), strings.Contains(s, "macos"), strings.Contains(s, "osx"):
		return PlatformMacOS
	case strings.Contains(s, "linux"):
		return PlatformLinux
	default:
		return PlatformOther
	}
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test -run TestPlatformFamily -count=1 ./internal/stats/`
Expected: PASS, both tests.

- [ ] **Step 5: Lint and commit**

```bash
make lint
git add internal/stats/platform.go internal/stats/platform_test.go
git commit -m "Stats: group the export's platform strings

listens.platform is free text and no two export vintages agree on its shape.
Grouping at read time rather than at import means a family added later
reclassifies the whole history without a backfill, and the original string is
never thrown away.

The case order is the design: a partner integration string can name the OS
underneath it and is a partner device first. Anything unrecognised is Other
rather than dropped, because a family nobody has seen yet still has to be
counted or the denominators stop adding up.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
```

---

### Task 5: Playback-context statistics

**Files:**
- Create: `internal/stats/context.go`
- Modify: `internal/stats/stats_test.go`
- Test: `test/integration/contextstats_test.go`

**Interfaces:**
- Consumes: `Coverage` from Task 1; `PlatformFamily` from Task 4; `rangeFilter`, `scope`.
- Produces:
  - `type ContextSlice struct { Key string; Plays int64 }`
  - `type PlaybackContext struct { EndReasons []ContextSlice; EndReasonCoverage Coverage; SkipRate float64; SkipCoverage Coverage; ShuffleRate float64; ShuffleCoverage Coverage; Platforms []ContextSlice; PlatformCoverage Coverage; Countries []ContextSlice; CountryCoverage Coverage; OfflineRate float64; OfflineCoverage Coverage; IncognitoRate float64; IncognitoCoverage Coverage }`
  - `func (s *Service) PlaybackContext(ctx context.Context, q store.Querier, userID uuid.UUID, r domain.TimeRange, tz string) (PlaybackContext, error)`

- [ ] **Step 1: Write the failing test**

Create `test/integration/contextstats_test.go`:

```go
//go:build integration

package integration

import (
	"testing"

	"github.com/RequiDev/encore/internal/stats"
)

// seedContext adds playback detail to the shared fixture's listens.
//
// The fixture's eight plays are all source 0 (sync) by default, which carry no
// context at all. This marks five of them as source 2 with detail, and leaves
// one of those five with a NULL shuffle so the per-column denominator rule has
// something to prove.
func seedContext(t *testing.T, f *statsFixture) {
	t.Helper()
	f.env.Exec(`
        WITH ordered AS (
            SELECT id, row_number() OVER (ORDER BY played_at) AS n
            FROM listens WHERE user_id = $1
        )
        UPDATE listens l SET
            source       = 2,
            reason_end   = CASE o.n WHEN 1 THEN 'trackdone' WHEN 2 THEN 'fwdbtn'
                                    WHEN 3 THEN 'fwdbtn'    WHEN 4 THEN 'backbtn'
                                    ELSE 'trackdone' END,
            shuffle      = CASE o.n WHEN 5 THEN NULL ELSE (o.n % 2 = 0) END,
            platform     = CASE o.n WHEN 1 THEN 'Android OS 10 API 29 (samsung, SM-G970F)'
                                    WHEN 2 THEN 'Android OS 11 API 30 (google, Pixel 5)'
                                    ELSE 'Windows 10 (10.0.19042; x64)' END,
            conn_country = CASE o.n WHEN 1 THEN 'DE' ELSE 'GB' END,
            offline      = false,
            incognito    = false
        FROM ordered o
        WHERE l.id = o.id AND o.n <= 5`, f.user.ID)
}

// TestContextDenominatorsArePerColumn is the rule the whole file rests on. An
// extended export can omit an individual field, so keying the denominator on
// source = 2 would overstate it for whichever field the export did not write.
func TestContextDenominatorsArePerColumn(t *testing.T) {
	f := seedStats(t)
	seedContext(t, f)

	got, err := f.svc.PlaybackContext(f.env.Ctx(), f.env.Store.DB(), f.user.ID, f.fullRange(), f.tz)
	if err != nil {
		t.Fatalf("playback context: %v", err)
	}

	// Five rows carry reason_end; only four of those carry shuffle.
	if got.EndReasonCoverage.Covered != 5 || got.EndReasonCoverage.Total != 8 {
		t.Errorf("reason_end coverage = %+v, want 5/8", got.EndReasonCoverage)
	}
	if got.ShuffleCoverage.Covered != 4 || got.ShuffleCoverage.Total != 8 {
		t.Errorf("shuffle coverage = %+v, want 4/8 — the denominator keyed on source, not on the column", got.ShuffleCoverage)
	}
}

// TestSkipRateCountsForwardOnly pins the definition. Going back is not the same
// gesture as skipping forward, and only fwdbtn counts.
func TestSkipRateCountsForwardOnly(t *testing.T) {
	f := seedStats(t)
	seedContext(t, f)

	got, err := f.svc.PlaybackContext(f.env.Ctx(), f.env.Store.DB(), f.user.ID, f.fullRange(), f.tz)
	if err != nil {
		t.Fatalf("playback context: %v", err)
	}

	// Of five rows with reason_end: two fwdbtn, one backbtn, two trackdone.
	if diff := got.SkipRate - 0.4; diff > 0.001 || diff < -0.001 {
		t.Errorf("skip rate = %v, want 0.4 (2 of 5) — backbtn may have been counted", got.SkipRate)
	}
	if got.SkipCoverage.Covered != 5 {
		t.Errorf("skip coverage = %+v, want 5 covered", got.SkipCoverage)
	}
}

// TestPlatformsAreGroupedByFamily checks the classifier is actually applied and
// that two different Android strings collapse to one slice.
func TestPlatformsAreGroupedByFamily(t *testing.T) {
	f := seedStats(t)
	seedContext(t, f)

	got, err := f.svc.PlaybackContext(f.env.Ctx(), f.env.Store.DB(), f.user.ID, f.fullRange(), f.tz)
	if err != nil {
		t.Fatalf("playback context: %v", err)
	}

	by := map[string]int64{}
	for _, p := range got.Platforms {
		by[p.Key] = p.Plays
	}
	if by[stats.PlatformAndroid] != 2 {
		t.Errorf("android = %d, want 2 — two distinct Android strings should collapse into one family", by[stats.PlatformAndroid])
	}
	if by[stats.PlatformWindows] != 3 {
		t.Errorf("windows = %d, want 3", by[stats.PlatformWindows])
	}
	if len(got.Platforms) != 2 {
		t.Errorf("got %d platform families, want 2: %+v", len(got.Platforms), got.Platforms)
	}
}

// TestContextWithNoExtendedRowsIsZeroNotAnError is the state of an instance whose
// history came entirely from live sync or an account-data export.
func TestContextWithNoExtendedRowsIsZeroNotAnError(t *testing.T) {
	f := seedStats(t)

	got, err := f.svc.PlaybackContext(f.env.Ctx(), f.env.Store.DB(), f.user.ID, f.fullRange(), f.tz)
	if err != nil {
		t.Fatalf("playback context with no extended rows: %v", err)
	}
	if got.SkipCoverage.Covered != 0 {
		t.Errorf("skip coverage covered = %d, want 0", got.SkipCoverage.Covered)
	}
	if got.SkipCoverage.Total != 8 {
		t.Errorf("skip coverage total = %d, want 8 — the total is all in-range listens", got.SkipCoverage.Total)
	}
	if got.SkipRate != 0 {
		t.Errorf("skip rate = %v, want 0", got.SkipRate)
	}
	if len(got.Platforms) != 0 {
		t.Errorf("expected no platforms, got %+v", got.Platforms)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `ENCORE_TEST_DATABASE_URL="postgres://encore:encore@localhost:5432/encore?sslmode=disable" go test -tags=integration -run 'TestContext|TestSkipRate|TestPlatformsAre' -count=1 ./test/integration/`
Expected: FAIL — `f.svc.PlaybackContext undefined`.

- [ ] **Step 3: Write the implementation**

Create `internal/stats/context.go`:

```go
// The playback-context statistics answer "how you listened" rather than "what
// you listened to".
//
// Two properties govern every query here.
//
// The columns are partial. platform, conn_country, reason_start, reason_end,
// shuffle, skipped, offline and incognito are written only by the extended-export
// importer. Live sync and account-data rows carry NULL in all eight. So every
// figure travels with its own denominator, and the denominator is counted per
// column — count(*) FILTER (WHERE col IS NOT NULL) — never per source, because
// an export may omit an individual field and keying on source = 2 would
// silently overstate it.
//
// listen_daily_rollup cannot serve any of this. It is keyed by (user, day,
// track) and carries no context columns at all, so these always scan the fact
// table. That is a property of the rollup, not an oversight here.
package stats

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/RequiDev/encore/internal/domain"
	"github.com/RequiDev/encore/internal/postgres"
	"github.com/RequiDev/encore/internal/store"
)

// ContextSlice is one category of a breakdown.
type ContextSlice struct {
	Key   string
	Plays int64
}

// PlaybackContext is the whole "how you listen" answer for one range.
type PlaybackContext struct {
	EndReasons        []ContextSlice
	EndReasonCoverage Coverage

	SkipRate     float64
	SkipCoverage Coverage

	ShuffleRate     float64
	ShuffleCoverage Coverage

	Platforms        []ContextSlice
	PlatformCoverage Coverage

	Countries       []ContextSlice
	CountryCoverage Coverage

	OfflineRate     float64
	OfflineCoverage Coverage

	IncognitoRate     float64
	IncognitoCoverage Coverage
}

// contextRatesSQL computes every scalar ratio and its own denominator in one
// pass over the range.
//
// SkipRate is defined as reason_end = 'fwdbtn'. This is a judgement call and is
// recorded as one: the skipped boolean is sparsely and inconsistently populated
// across export vintages, whereas reason_end is reliably present, and 'backbtn'
// is deliberately excluded because going back is not the gesture skipping is.
//
// Parameters are $1 user, $2 from, $3 to.
var contextRatesSQL = fmt.Sprintf(`
SELECT count(*)::bigint,
       count(l.reason_end)::bigint,
       count(*) FILTER (WHERE l.reason_end = 'fwdbtn')::bigint,
       count(l.shuffle)::bigint,
       count(*) FILTER (WHERE l.shuffle)::bigint,
       count(l.offline)::bigint,
       count(*) FILTER (WHERE l.offline)::bigint,
       count(l.incognito)::bigint,
       count(*) FILTER (WHERE l.incognito)::bigint
FROM listens l
WHERE %s`, rangeFilter("l", "$1", "$2", "$3"))

// contextBreakdownSQL returns the three categorical breakdowns in one result
// set, tagged by kind, rather than in three round trips.
//
// platform is returned raw: the grouping into families happens in Go, in
// PlatformFamily, so the classifier stays testable without a database and the
// original strings are never lost to a GROUP BY.
//
// Parameters are $1 user, $2 from, $3 to.
var contextBreakdownSQL = fmt.Sprintf(`
WITH scoped AS (SELECT l.platform, l.conn_country, l.reason_end FROM listens l WHERE %s)
SELECT 'platform' AS kind, s.platform AS key, count(*)::bigint
FROM scoped s WHERE s.platform IS NOT NULL GROUP BY s.platform
UNION ALL
SELECT 'country', s.conn_country, count(*)::bigint
FROM scoped s WHERE s.conn_country IS NOT NULL GROUP BY s.conn_country
UNION ALL
SELECT 'reason_end', s.reason_end, count(*)::bigint
FROM scoped s WHERE s.reason_end IS NOT NULL GROUP BY s.reason_end`,
	rangeFilter("l", "$1", "$2", "$3"))

// ratio divides safely. A zero denominator is "no data", which is a zero rate
// carrying a zero coverage, not a division by zero and not an error.
func ratio(n, d int64) float64 {
	if d == 0 {
		return 0
	}
	return float64(n) / float64(d)
}

// PlaybackContext answers how the range's listening happened.
func (s *Service) PlaybackContext(
	ctx context.Context,
	q store.Querier,
	userID uuid.UUID,
	r domain.TimeRange,
	tz string,
) (PlaybackContext, error) {
	if _, err := scope(userID, r, tz); err != nil {
		return PlaybackContext{}, err
	}

	var (
		out                          PlaybackContext
		total                        int64
		endN, skipN                  int64
		shuffleN, shuffleYes         int64
		offlineN, offlineYes         int64
		incognitoN, incognitoYes     int64
	)
	if err := q.QueryRow(ctx, contextRatesSQL,
		store.UUIDArg(userID), r.From.UTC(), r.To.UTC(),
	).Scan(&total, &endN, &skipN, &shuffleN, &shuffleYes,
		&offlineN, &offlineYes, &incognitoN, &incognitoYes); err != nil {
		return PlaybackContext{}, postgres.Classify("playback context rates", err)
	}

	out.EndReasonCoverage = Coverage{Covered: endN, Total: total}
	out.SkipCoverage = Coverage{Covered: endN, Total: total}
	out.SkipRate = ratio(skipN, endN)
	out.ShuffleCoverage = Coverage{Covered: shuffleN, Total: total}
	out.ShuffleRate = ratio(shuffleYes, shuffleN)
	out.OfflineCoverage = Coverage{Covered: offlineN, Total: total}
	out.OfflineRate = ratio(offlineYes, offlineN)
	out.IncognitoCoverage = Coverage{Covered: incognitoN, Total: total}
	out.IncognitoRate = ratio(incognitoYes, incognitoN)

	rows, err := q.Query(ctx, contextBreakdownSQL,
		store.UUIDArg(userID), r.From.UTC(), r.To.UTC())
	if err != nil {
		return PlaybackContext{}, postgres.Classify("playback context breakdown", err)
	}
	defer rows.Close()

	families := map[string]int64{}
	var platformTotal int64
	for rows.Next() {
		var (
			kind  string
			key   string
			plays int64
		)
		if err := rows.Scan(&kind, &key, &plays); err != nil {
			return PlaybackContext{}, postgres.Classify("scan playback context breakdown", err)
		}
		switch kind {
		case "platform":
			families[PlatformFamily(key)] += plays
			platformTotal += plays
		case "country":
			out.Countries = append(out.Countries, ContextSlice{Key: key, Plays: plays})
		case "reason_end":
			out.EndReasons = append(out.EndReasons, ContextSlice{Key: key, Plays: plays})
		}
	}
	if err := rows.Err(); err != nil {
		return PlaybackContext{}, postgres.Classify("playback context breakdown", err)
	}

	out.Platforms = sortedSlices(families)
	out.PlatformCoverage = Coverage{Covered: platformTotal, Total: total}
	out.CountryCoverage = Coverage{Covered: sumSlices(out.Countries), Total: total}
	sortSlices(out.Countries)
	sortSlices(out.EndReasons)
	return out, nil
}

// sortedSlices turns the family tally into a stable descending list.
func sortedSlices(m map[string]int64) []ContextSlice {
	out := make([]ContextSlice, 0, len(m))
	for k, v := range m {
		out = append(out, ContextSlice{Key: k, Plays: v})
	}
	sortSlices(out)
	return out
}

// sortSlices orders a breakdown by plays descending, then by key, so a tie
// renders the same way twice.
func sortSlices(s []ContextSlice) {
	slices.SortFunc(s, func(a, b ContextSlice) int {
		if a.Plays != b.Plays {
			return int(b.Plays - a.Plays)
		}
		return strings.Compare(a.Key, b.Key)
	})
}

func sumSlices(s []ContextSlice) int64 {
	var n int64
	for _, e := range s {
		n += e.Plays
	}
	return n
}
```

Add `"slices"` and `"strings"` to the imports. Note the package doc comment must go **above** the `package stats` line; since `internal/stats/stats.go` already carries the package doc, move the block comment above `package stats` in `context.go` to sit *below* it as a plain file comment instead — Go allows only one package doc, and `staticcheck` will flag a second.

- [ ] **Step 4: Register the statements**

```go
		{name: "contextRates", sql: contextRatesSQL, params: 3},
		{name: "contextBreakdown", sql: contextBreakdownSQL, params: 3},
```

- [ ] **Step 5: Run the tests**

Run: `make test` — expected PASS.
Run: `ENCORE_TEST_DATABASE_URL="postgres://encore:encore@localhost:5432/encore?sslmode=disable" go test -tags=integration -run 'TestContext|TestSkipRate|TestPlatformsAre' -count=1 ./test/integration/` — expected PASS, four tests.

- [ ] **Step 6: Lint and commit**

```bash
make lint
git add internal/stats/context.go internal/stats/stats_test.go test/integration/contextstats_test.go
git commit -m "Stats: how you listened, not just what to

Eight columns on listens describe every play an extended export produced —
platform, country, why it started, why it ended, shuffle, skipped, offline,
incognito. stats/history.go selected all of them for the raw feed and dropped
them before the DTO, so no client has ever seen one.

The denominator is counted per column rather than per source. An export can
omit an individual field, and keying on source = 2 would quietly overstate
the denominator for whichever field it left out — a rate over a subset,
presented as a fact.

Skip rate is reason_end = 'fwdbtn'. The skipped boolean is sparse and
inconsistent across export vintages; reason_end is reliably there. backbtn is
excluded on purpose: going back is not the gesture skipping is.

The rollup cannot serve any of this — it is keyed by track and day and holds
no context — so these always scan the fact table. Said in the file so it is
not later mistaken for something left undone.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
```

---

### Task 6: Genre HTTP endpoints

**Files:**
- Modify: `internal/httpapi/dto.go`, `internal/httpapi/stats.go`, `internal/httpapi/router.go`
- Test: `internal/httpapi/httpapi_test.go`

**Interfaces:**
- Consumes: `TopGenres`, `GenreTimeline`, `GenrePage`, `GenrePoint`, `Coverage` from Tasks 1–2; `callerAndRange`, `parseLimit`, `parseOffset`, `writeJSON`, `writeError`.
- Produces: routes `GET /api/stats/genres` and `GET /api/stats/genres/timeline`; DTOs `CoverageResponse`, `GenreEntry`, `GenresResponse`, `GenreTimelineResponse`.

- [ ] **Step 1: Write the DTOs**

Append to `internal/httpapi/dto.go`:

```go
// CoverageResponse is the shape every partial statistic carries.
//
// One shape across every endpoint so the client renders it with one component,
// and so a reader of the JSON never has to work out which denominator a given
// percentage was taken over.
type CoverageResponse struct {
	Covered int64 `json:"covered"`
	Total   int64 `json:"total"`
}

// RateResponse is a ratio and the coverage it was computed over.
type RateResponse struct {
	Value    float64 `json:"value"`
	Covered  int64   `json:"covered"`
	Total    int64   `json:"total"`
}

// GenreEntry is one row of the genre ranking.
type GenreEntry struct {
	Genre    string `json:"genre"`
	Plays    int64  `json:"plays"`
	MsPlayed int64  `json:"msPlayed"`
}

// GenresResponse is one page of the ranking.
//
// Plays across genres sum to more than the range's total plays, because a track
// counts toward each of its genres. The client says so on the page.
type GenresResponse struct {
	Genres   []GenreEntry     `json:"genres"`
	Total    int64            `json:"total"`
	Coverage CoverageResponse `json:"coverage"`
}

// GenreTimelinePoint is one genre in one bucket.
type GenreTimelinePoint struct {
	Bucket   time.Time `json:"bucket"`
	Genre    string    `json:"genre"`
	Plays    int64     `json:"plays"`
	MsPlayed int64     `json:"msPlayed"`
}

// GenreTimelineResponse carries the interval so the client formats the axis
// without re-deriving it.
type GenreTimelineResponse struct {
	Interval string               `json:"interval"`
	Points   []GenreTimelinePoint `json:"points"`
}

func toCoverage(c stats.Coverage) CoverageResponse {
	return CoverageResponse{Covered: c.Covered, Total: c.Total}
}

func toRate(v float64, c stats.Coverage) RateResponse {
	return RateResponse{Value: v, Covered: c.Covered, Total: c.Total}
}

func toGenres(p stats.GenrePage) GenresResponse {
	out := GenresResponse{
		Genres:   make([]GenreEntry, 0, len(p.Genres)),
		Total:    p.Total,
		Coverage: toCoverage(p.Coverage),
	}
	for _, g := range p.Genres {
		out.Genres = append(out.Genres, GenreEntry{Genre: g.Genre, Plays: g.Plays, MsPlayed: g.MsPlayed})
	}
	return out
}
```

- [ ] **Step 2: Write the handlers**

Append to `internal/httpapi/stats.go`:

```go
// genreTimelineMaxSeries bounds how many genres one timeline may carry. Eight is
// where a stacked area chart stops being readable and the ninth series is noise.
const genreTimelineMaxSeries = 8

// handleGenres answers GET /api/stats/genres.
func (s *Server) handleGenres(w http.ResponseWriter, r *http.Request) {
	user, tr, err := s.callerAndRange(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	limit, offset, err := parsePage(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	page, err := s.stats.TopGenres(r.Context(), s.querier, user.ID, tr, user.Timezone, limit, offset)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, toGenres(page))
}

// handleGenreTimeline answers GET /api/stats/genres/timeline.
//
// The genres are a repeated query parameter rather than the server picking them,
// so a chart's series stay stable while the ranking beneath it is paged. Asking
// for none means "the range's top ones", which is what a first page load wants.
func (s *Server) handleGenreTimeline(w http.ResponseWriter, r *http.Request) {
	user, tr, err := s.callerAndRange(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	interval, err := parseInterval(r, tr)
	if err != nil {
		writeError(w, r, err)
		return
	}
	ctx := r.Context()

	genres := r.URL.Query()["genre"]
	if len(genres) > genreTimelineMaxSeries {
		writeError(w, r, fmt.Errorf("%w: at most %d genres may be charted at once",
			domain.ErrValidation, genreTimelineMaxSeries))
		return
	}
	if len(genres) == 0 {
		page, err := s.stats.TopGenres(ctx, s.querier, user.ID, tr, user.Timezone, genreTimelineMaxSeries, 0)
		if err != nil {
			writeError(w, r, err)
			return
		}
		for _, g := range page.Genres {
			genres = append(genres, g.Genre)
		}
	}

	points, err := s.stats.GenreTimeline(ctx, s.querier, user.ID, tr, user.Timezone, interval, genres)
	if err != nil {
		writeError(w, r, err)
		return
	}

	out := GenreTimelineResponse{
		Interval: string(interval),
		Points:   make([]GenreTimelinePoint, 0, len(points)),
	}
	for _, p := range points {
		out.Points = append(out.Points, GenreTimelinePoint{
			Bucket: p.Bucket, Genre: p.Genre, Plays: p.Plays, MsPlayed: p.MsPlayed,
		})
	}
	writeJSON(w, r, http.StatusOK, out)
}
```

`parsePage` (`internal/httpapi/params.go:168`) and `parseInterval` (`params.go:86`) both already exist with exactly these signatures. Add `"fmt"` and the `domain` import to `stats.go` if they are not already there.

- [ ] **Step 3: Register the routes**

In `internal/httpapi/router.go`, beside the existing `GET /api/stats/*` registrations:

```go
	s.route(mux, "GET /api/stats/genres", s.handleGenres)
	s.route(mux, "GET /api/stats/genres/timeline", s.handleGenreTimeline)
```

- [ ] **Step 4: Write the route test**

Find the existing table of routes in `internal/httpapi/httpapi_test.go` that asserts authentication is required, and add the two new paths to it. If no such table exists, add:

```go
// TestNewStatsRoutesRequireASession keeps the new endpoints inside the same
// session and CSRF envelope as every other statistic.
func TestNewStatsRoutesRequireASession(t *testing.T) {
	srv := newTestServer(t)
	for _, path := range []string{
		"/api/stats/genres",
		"/api/stats/genres/timeline",
	} {
		rec := srv.do(t, "GET", path, nil)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("GET %s = %d, want 401", path, rec.Code)
		}
	}
}
```

Match `newTestServer` and `srv.do` to whatever the existing tests in that file actually use — read it first and copy the local idiom exactly.

- [ ] **Step 5: Run the tests**

Run: `make test` — expected PASS.

- [ ] **Step 6: Lint and commit**

```bash
make lint
git add internal/httpapi/dto.go internal/httpapi/stats.go internal/httpapi/router.go internal/httpapi/httpapi_test.go
git commit -m "API: serve the genre ranking and its timeline

Coverage travels in one shape on every endpoint that has it, so the client
renders it with one component and a reader of the JSON never has to work out
which denominator a percentage was taken over.

The timeline takes its genres as a repeated query parameter rather than
picking them itself. A chart whose series change when the ranking under it
is paged is a chart nobody can read, and passing them in is also what makes
the endpoint linkable.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
```

---

### Task 7: Taste and context HTTP endpoints

**Files:**
- Modify: `internal/httpapi/dto.go`, `internal/httpapi/stats.go`, `internal/httpapi/router.go`
- Test: `internal/httpapi/httpapi_test.go`

**Interfaces:**
- Consumes: `Taste`, `PlaybackContext`, `ContextSlice` from Tasks 3 and 5; `toRate`, `toCoverage` from Task 6.
- Produces: routes `GET /api/stats/taste` and `GET /api/stats/context`; DTOs `TasteResponse`, `ContextSliceEntry`, `PlaybackContextResponse`.

- [ ] **Step 1: Write the DTOs**

Append to `internal/httpapi/dto.go`:

```go
// TasteResponse carries both scores with their own coverage.
type TasteResponse struct {
	Obscurity  RateResponse `json:"obscurity"`
	ReleaseLag RateResponse `json:"releaseLag"`
}

// ContextSliceEntry is one category of a breakdown.
type ContextSliceEntry struct {
	Key   string `json:"key"`
	Plays int64  `json:"plays"`
}

// PlaybackContextResponse is the whole "how you listen" payload.
//
// Every rate carries its own denominator because the underlying columns are
// written only by the extended-export importer, and an export may omit any one
// of them independently.
type PlaybackContextResponse struct {
	EndReasons        []ContextSliceEntry `json:"endReasons"`
	EndReasonCoverage CoverageResponse    `json:"endReasonCoverage"`
	SkipRate          RateResponse        `json:"skipRate"`
	ShuffleRate       RateResponse        `json:"shuffleRate"`
	Platforms         []ContextSliceEntry `json:"platforms"`
	PlatformCoverage  CoverageResponse    `json:"platformCoverage"`
	Countries         []ContextSliceEntry `json:"countries"`
	CountryCoverage   CoverageResponse    `json:"countryCoverage"`
	OfflineRate       RateResponse        `json:"offlineRate"`
	IncognitoRate     RateResponse        `json:"incognitoRate"`
}

func toContextSlices(in []stats.ContextSlice) []ContextSliceEntry {
	out := make([]ContextSliceEntry, 0, len(in))
	for _, s := range in {
		out = append(out, ContextSliceEntry{Key: s.Key, Plays: s.Plays})
	}
	return out
}

func toTaste(t stats.Taste) TasteResponse {
	return TasteResponse{
		Obscurity:  toRate(t.Obscurity, t.ObscurityCoverage),
		ReleaseLag: toRate(t.ReleaseLagYears, t.ReleaseLagCoverage),
	}
}

func toPlaybackContext(c stats.PlaybackContext) PlaybackContextResponse {
	return PlaybackContextResponse{
		EndReasons:        toContextSlices(c.EndReasons),
		EndReasonCoverage: toCoverage(c.EndReasonCoverage),
		SkipRate:          toRate(c.SkipRate, c.SkipCoverage),
		ShuffleRate:       toRate(c.ShuffleRate, c.ShuffleCoverage),
		Platforms:         toContextSlices(c.Platforms),
		PlatformCoverage:  toCoverage(c.PlatformCoverage),
		Countries:         toContextSlices(c.Countries),
		CountryCoverage:   toCoverage(c.CountryCoverage),
		OfflineRate:       toRate(c.OfflineRate, c.OfflineCoverage),
		IncognitoRate:     toRate(c.IncognitoRate, c.IncognitoCoverage),
	}
}
```

- [ ] **Step 2: Write the handlers**

Append to `internal/httpapi/stats.go`:

```go
// handleTaste answers GET /api/stats/taste.
func (s *Server) handleTaste(w http.ResponseWriter, r *http.Request) {
	user, tr, err := s.callerAndRange(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	t, err := s.stats.Taste(r.Context(), s.querier, user.ID, tr, user.Timezone)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, toTaste(t))
}

// handlePlaybackContext answers GET /api/stats/context.
func (s *Server) handlePlaybackContext(w http.ResponseWriter, r *http.Request) {
	user, tr, err := s.callerAndRange(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	c, err := s.stats.PlaybackContext(r.Context(), s.querier, user.ID, tr, user.Timezone)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, toPlaybackContext(c))
}
```

- [ ] **Step 3: Register the routes**

```go
	s.route(mux, "GET /api/stats/taste", s.handleTaste)
	s.route(mux, "GET /api/stats/context", s.handlePlaybackContext)
```

- [ ] **Step 4: Extend the route test**

Add `"/api/stats/taste"` and `"/api/stats/context"` to the path list in `TestNewStatsRoutesRequireASession` from Task 6.

- [ ] **Step 5: Run the tests**

Run: `make test` — expected PASS.

- [ ] **Step 6: Lint and commit**

```bash
make lint
git add internal/httpapi/dto.go internal/httpapi/stats.go internal/httpapi/router.go internal/httpapi/httpapi_test.go
git commit -m "API: serve the taste scores and the playback context

Four endpoints rather than one bag, matching the granularity the repartition
routes already set: a page that wants the skip rate should not fetch the
country breakdown to get it.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
```

---

### Task 8: Genres and obscurity on a share link

**Files:**
- Modify: `internal/httpapi/share.go`, `internal/httpapi/dto.go`
- Test: `test/e2e/e2e_test.go` — the share endpoint is exercised there, not in the `httpapi` unit tests.

**Interfaces:**
- Consumes: `TopGenres`, `Taste`, `toGenres`, `toTaste` from Tasks 1, 3, 6, 7.
- Produces: two new fields on `SharedStatsResponse`.

- [ ] **Step 1: Write the failing test**

In `test/e2e/e2e_test.go`, immediately after `TestSharedLinkShowsAggregatesToAnybodyHoldingIt` (line ~962), add:

```go
// TestSharedLinkCarriesGenresAndObscurity checks the two aggregate additions
// reach a shared page. They are the same data class as the top lists beside
// them: what somebody's taste is, never when they were listening.
func TestSharedLinkCarriesGenresAndObscurity(t *testing.T) {
	inst := newInstance(t)
	owner := inst.browser()
	inst.signIn(owner)

	at := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	inst.stub.plays = []map[string]any{playItem("shr00000000000000004a", at)}
	decode[map[string]any](t, owner.postJSON("/api/sync/now", nil), http.StatusOK)

	created := decode[map[string]any](t, owner.postJSON("/api/shares", map[string]any{}), http.StatusCreated)
	token := created["token"].(string)

	shared := decode[map[string]any](t, inst.browser().get("/api/share/"+token), http.StatusOK)
	if _, ok := shared["genres"].(map[string]any); !ok {
		t.Errorf("the shared payload carries no genre ranking: %v", shared)
	}
	if _, ok := shared["taste"].(map[string]any); !ok {
		t.Errorf("the shared payload carries no taste scores: %v", shared)
	}
}
```

Then extend the `forbidden` list in the **existing** `TestASharedLinkCannotReachTheListeningHistory` (line ~983) from:

```go
	for _, forbidden := range []string{"history", "listens", "plays", "items"} {
```

to:

```go
	// The playback-context statistics are withheld deliberately: device and
	// country say what hardware somebody owns and where they have travelled,
	// which is a different disclosure from a favourite artist.
	for _, forbidden := range []string{
		"history", "listens", "plays", "items",
		"skipRate", "shuffleRate", "platforms", "countries", "offlineRate", "incognitoRate",
	} {
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `ENCORE_TEST_DATABASE_URL="postgres://encore:encore@localhost:5432/encore?sslmode=disable" go test -tags=integration -run 'TestSharedLinkCarries' -count=1 ./test/e2e/`
Expected: FAIL — "the shared payload carries no genre ranking".

- [ ] **Step 3: Add the fields**

On `SharedStatsResponse` in `internal/httpapi/dto.go`:

```go
	// Genres and Taste are aggregate taste, the same data class as the top
	// lists above them. Playback context is deliberately absent: device and
	// country say what hardware somebody owns and where they have travelled,
	// which is not what a share is for.
	Genres *GenresResponse `json:"genres,omitempty"`
	Taste  *TasteResponse  `json:"taste,omitempty"`
```

- [ ] **Step 4: Populate them**

In `handleSharedStats` in `internal/httpapi/share.go`, after the existing `out.Albums = ...` assignment and before the timeline block:

```go
	genres, err := s.stats.TopGenres(ctx, s.querier, owner.ID, rng, tz, shareTopLimit, 0)
	if err != nil {
		writeError(w, r, err)
		return
	}
	sharedGenres := toGenres(genres)
	out.Genres = &sharedGenres

	taste, err := s.stats.Taste(ctx, s.querier, owner.ID, rng, tz)
	if err != nil {
		writeError(w, r, err)
		return
	}
	sharedTaste := toTaste(taste)
	out.Taste = &sharedTaste
```

Do **not** add a field to `domain.ShareLink` and do **not** add a toggle. What a share exposes is fixed by the feature, per the comment at `internal/domain/share.go:20-24`.

- [ ] **Step 5: Run the tests**

Run: `make test` — expected PASS.
Run: `make test-integration` — expected PASS; the whole suite, since this is the last Go change before the web work. Both new e2e assertions must pass, including the extended `forbidden` list.

- [ ] **Step 6: Lint and commit**

```bash
make lint
git add internal/httpapi/share.go internal/httpapi/dto.go test/e2e/e2e_test.go
git commit -m "Share: genres and the obscurity score

Both are the same data class a share already carries — what somebody's taste
is, never when they were listening — so they go on the shared page beside the
top lists.

The playback-context statistics do not, and a test asserts their field names
never appear in a shared response. Device and country say what hardware
somebody owns and where they have travelled, which is a different thing to
disclose than a favourite artist.

No field on ShareLink and no toggle, per the note the type carries: a privacy
boundary that depends on a boolean being set correctly is one that will
eventually be set incorrectly.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
```

---

### Task 9: Web — types, query keys and the Genres page

**Files:**
- Modify: `web/src/lib/types.ts`, `web/src/lib/query.ts`, `web/src/lib/api.ts`, `web/src/App.tsx`
- Create: `web/src/pages/Genres.tsx`

**Interfaces:**
- Consumes: `GET /api/stats/genres`, `GET /api/stats/genres/timeline`.
- Produces: TS types `Coverage`, `Rate`, `GenreEntry`, `GenresResponse`, `GenreTimelinePoint`, `GenreTimelineResponse`; query keys `qk.genres`, `qk.genreTimeline`; route `/genres`; array support in `buildQuery`.

- [ ] **Step 1: Add the types**

In `web/src/lib/types.ts`:

```ts
/** The denominator every partial statistic carries. */
export interface Coverage {
  covered: number
  total: number
}

/** A ratio and the coverage it was computed over. */
export interface Rate extends Coverage {
  value: number
}

export interface GenreEntry {
  genre: string
  plays: number
  msPlayed: number
}

/**
 * One page of the genre ranking.
 *
 * Plays across genres sum to more than the range's total plays: a track counts
 * toward each of its genres. The page says so rather than normalising it away.
 */
export interface GenresResponse {
  genres: GenreEntry[]
  total: number
  coverage: Coverage
}

export interface GenreTimelinePoint {
  bucket: string
  genre: string
  plays: number
  msPlayed: number
}

export interface GenreTimelineResponse {
  interval: Interval
  points: GenreTimelinePoint[]
}
```

- [ ] **Step 2: Add the query keys**

In the `qk` object in `web/src/lib/query.ts`, beside the other `stats` keys:

```ts
  genres: (range: DateRange, page: PageKey) => ['stats', 'genres', range, page] as const,
  genreTimeline: (range: DateRange, interval: string | null, genres: string[]) =>
    ['stats', 'genres', 'timeline', range, interval, genres] as const,
```

- [ ] **Step 3: Write the page**

Create `web/src/pages/Genres.tsx`.

The data layer is specified exactly below. The **layout** is specified as requirements rather than as JSX on purpose: `web/src/components/ui` and `web/src/components/charts` are an established design system with props this plan has not enumerated, and inventing JSX against them would produce code that does not compile. Read `web/src/pages/Discovery.tsx` and `web/src/pages/top/TopList.tsx` and follow their structure exactly.

The two queries:

```tsx
const { range } = useRange()
const [page, setPage] = useState(0)

const genres = useQuery({
  queryKey: qk.genres(range, { limit: PAGE_SIZE, offset: page * PAGE_SIZE }),
  queryFn: ({ signal }) =>
    api.get<GenresResponse>('/stats/genres', {
      from: range.from,
      to: range.to,
      limit: PAGE_SIZE,
      offset: page * PAGE_SIZE,
    }, signal),
})

/** The chart's series are the range's top eight, fixed so paging the table below does not reshape it. */
const series = useMemo(
  () => (genres.data?.genres ?? []).slice(0, 8).map((g) => g.genre),
  [genres.data],
)

const timeline = useQuery({
  enabled: series.length > 0,
  queryKey: qk.genreTimeline(range, interval, series),
  queryFn: ({ signal }) =>
    api.get<GenreTimelineResponse>('/stats/genres/timeline', {
      from: range.from,
      to: range.to,
      interval,
      genre: series,
    }, signal),
})
```

**`buildQuery` cannot express this yet and must be extended first.** `web/src/lib/api.ts:63-74` types `QueryValues` as `Record<string, string | number | boolean | null | undefined>` and serialises with `params.set`, so an array would arrive as one comma-joined value. The server reads `r.URL.Query()["genre"]`, which needs repetition. Change both:

```ts
export type QueryValues = Record<
  string,
  string | number | boolean | null | undefined | readonly string[]
>

export function buildQuery(query: QueryValues | undefined): string {
  if (!query) return ''
  const params = new URLSearchParams()
  for (const [key, value] of Object.entries(query)) {
    if (value === null || value === undefined || value === '') continue
    // An array becomes a repeated parameter rather than one comma-joined
    // value, because that is what Go's r.URL.Query()[key] reads back.
    if (Array.isArray(value)) {
      for (const v of value) params.append(key, String(v))
      continue
    }
    params.set(key, String(value))
  }
  const s = params.toString()
  return s ? `?${s}` : ''
}
```

`web/src/lib/api.test.ts` may not exist; if there is an existing test file for `api.ts`, add a case asserting `buildQuery({ genre: ['rock', 'jazz'] })` returns `'?genre=rock&genre=jazz'`.

The page must contain, in this order:

1. `PageHeader` with a `RangePicker`.
2. A **coverage sentence** rendered from `data.coverage`, always, before the chart. Wording:
   - Full coverage: *"Genres are known for all of your listening in this range."*
   - Partial: *"Genres are known for 87% of your listening in this range — 1,140,203 of 1,310,556 plays."*
   - Zero: an `EmptyState` reading *"No genres yet. Encore learns them from Spotify while it fills in your catalogue; check Settings for progress."* with a link to `/settings`.
3. A `BarChart` of the top genres by plays.
4. The stacked timeline from `qk.genreTimeline`, its series fixed to the top eight.
5. A `Ledger` table of the full ranking, paginated.
6. A one-line note beneath the table: *"A track counts toward each of its genres, so these add up to more than your total plays."*

The coverage sentence and the note are **requirements, not decoration** — they are the difference between a statistic and a plausible-looking number. Do not omit them.

- [ ] **Step 4: Add the route**

In `web/src/App.tsx`, beside the existing statistics routes, and in whatever navigation component lists them:

```tsx
<Route path="/genres" element={<Genres />} />
```

- [ ] **Step 5: Verify**

Run: `cd web && npm run lint && npm run typecheck && npm run build`
Expected: all pass, no TypeScript errors.

Then run the stack (`make db-up && make migrate`, `make run-api`, `make run-web`) and open `/genres`. Confirm the coverage sentence renders, and that stripping genres from an artist in the database (`UPDATE artists SET genres = '{}'`) changes the percentage rather than emptying the page silently.

- [ ] **Step 6: Lint and commit**

```bash
make lint
git add web/src/lib/types.ts web/src/lib/query.ts web/src/lib/api.ts web/src/pages/Genres.tsx web/src/App.tsx
git commit -m "Web: a genres page

The coverage sentence sits above the chart rather than in a tooltip, because
a fresh instance mid-enrichment genuinely knows the genres of almost nothing,
and an empty chart with no explanation is indistinguishable from a broken
one.

The note under the table — a track counts toward each of its genres — exists
because the numbers add up to more than the total plays and somebody will
otherwise reasonably conclude the page is wrong.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
```

---

### Task 10: Web — the Habits page

**Files:**
- Modify: `web/src/lib/types.ts`, `web/src/lib/query.ts`, `web/src/App.tsx`
- Create: `web/src/pages/Habits.tsx`

**Interfaces:**
- Consumes: `GET /api/stats/context`, `GET /api/stats/taste`; `Rate` and `Coverage` from Task 9.
- Produces: TS types `ContextSlice`, `PlaybackContextResponse`, `TasteResponse`; query keys `qk.playbackContext`, `qk.taste`; route `/habits`.

- [ ] **Step 1: Add the types**

In `web/src/lib/types.ts`:

```ts
export interface ContextSlice {
  key: string
  plays: number
}

export interface TasteResponse {
  obscurity: Rate
  releaseLag: Rate
}

/**
 * How listening happened, as opposed to what was listened to.
 *
 * Every rate carries its own denominator: the underlying columns are written
 * only by the extended-export importer, and an export may omit any one of them
 * independently of the others.
 */
export interface PlaybackContextResponse {
  endReasons: ContextSlice[]
  endReasonCoverage: Coverage
  skipRate: Rate
  shuffleRate: Rate
  platforms: ContextSlice[]
  platformCoverage: Coverage
  countries: ContextSlice[]
  countryCoverage: Coverage
  offlineRate: Rate
  incognitoRate: Rate
}
```

- [ ] **Step 2: Add the query keys**

```ts
  playbackContext: (range: DateRange) => ['stats', 'context', range] as const,
  taste: (range: DateRange) => ['stats', 'taste', range] as const,
```

- [ ] **Step 3: Write the page**

Create `web/src/pages/Habits.tsx`, following the same structural conventions as `Discovery.tsx`.

Content, in order:

1. `PageHeader` with a `RangePicker` and a subtitle distinguishing it from the top lists: *"How you listened, rather than what you listened to."*
2. A **prominent coverage banner** above everything, from `skipRate.covered` / `skipRate.total`: *"Based on the 62% of your listening that carries playback detail. Only Spotify's extended streaming history export records it — plays recorded live by Encore, and plays from the one-year account-data export, do not."* When covered is 0, render an `EmptyState` explaining that an extended export has not been imported, linking to `/imports`.
3. A `StatGrid` of the four rates: skip, shuffle, offline, incognito — each showing its own percentage **and its own coverage**, because they differ.
4. A `BarChart` of end reasons, with the raw keys mapped to readable labels: `trackdone` → "Played to the end", `fwdbtn` → "Skipped forward", `backbtn` → "Went back", `endplay` → "Stopped", `logout` → "Signed out", `remote` → "Changed remotely", `trackerror` → "Playback error", `unknown` → "Unknown", `other` → "Other".
5. A `BarChart` of platform families, labels: `android` → "Android", `ios` → "iOS", `windows` → "Windows", `macos` → "macOS", `linux` → "Linux", `web` → "Web player", `cast` → "Cast", `partner` → "Partner device", `other` → "Other".
6. A `BarChart` or `Ledger` of countries.
7. A footnote defining the skip rate: *"A skip is a track ended with the forward button. Going back is counted separately."*

- [ ] **Step 4: Add the route**

```tsx
<Route path="/habits" element={<Habits />} />
```

- [ ] **Step 5: Verify**

Run: `cd web && npm run lint && npm run typecheck && npm run build` — expected all pass.

With the stack running, open `/habits` on an instance whose history came only from live sync and confirm it shows the "no extended export" empty state rather than four zero percentages presented as fact.

- [ ] **Step 6: Lint and commit**

```bash
make lint
git add web/src/lib/types.ts web/src/lib/query.ts web/src/pages/Habits.tsx web/src/App.tsx
git commit -m "Web: a habits page

Separate from the top lists because it answers a different question. Those
are what you listen to; this is how — skipped or finished, shuffled or
chosen, on what, from where.

The coverage banner is the first thing on the page rather than a footnote.
These columns exist only on rows from an extended streaming history export,
so an instance built from live sync alone has none of them, and four zero
percentages presented as fact would be worse than an empty page.

Each rate carries its own denominator because an export can omit any one
field independently — the shuffle percentage and the skip percentage are
genuinely computed over different numbers of plays.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
```

---

### Task 11: Web — dashboard cards

**Files:**
- Modify: `web/src/pages/Dashboard.tsx`

**Interfaces:**
- Consumes: `qk.genres`, `qk.taste` from Tasks 9–10.
- Produces: nothing new.

- [ ] **Step 1: Add the cards**

Add two compact panels to `Dashboard.tsx`, following whatever card pattern the file already uses:

1. **Top genres** — the top five as a `BarChart`, linking to `/genres`. When coverage is zero, the card renders a one-line "not known yet" state rather than disappearing, so the dashboard layout does not shift as enrichment progresses.
2. **Obscurity** — the score as a `Stat` with a plain-language reading, linking to `/habits`. Suggested bands: 0–24 "deep cuts", 25–49 "off the beaten track", 50–74 "broadly popular", 75–100 "chart music". Show the coverage beneath.

Both must degrade to a stated empty state, never to a blank or a zero presented as a measurement.

- [ ] **Step 2: Verify**

Run: `cd web && npm run lint && npm run typecheck && npm run build` — expected all pass.

Open the dashboard and confirm both cards render, and that on an instance with no enriched artists they show their empty states without shifting the surrounding layout.

- [ ] **Step 3: Commit**

```bash
make lint
git add web/src/pages/Dashboard.tsx
git commit -m "Web: genres and obscurity on the dashboard

Both cards hold their space when they have nothing to show. A card that
disappears while enrichment runs makes the dashboard reflow under somebody
who is reading it, and an obscurity score of zero rendered as a measurement
rather than as an absence is simply wrong.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
```

---

### Task 12: Dead field removal and documentation

**Files:**
- Modify: `internal/spotify/models.go`
- Modify: `docs/feature-parity.md`, `docs/api.md`, `README.md`

**Interfaces:**
- Consumes: nothing.
- Produces: nothing.

- [ ] **Step 1: Confirm the field is dead**

Run: `grep -rn "PreviewURL\|preview_url" --include=*.go --include=*.ts --include=*.tsx . | grep -v node_modules`
Expected: exactly one hit, `internal/spotify/models.go:103`. If there are more, stop and report — the field is in use and this step is wrong.

- [ ] **Step 2: Delete it**

Remove the `PreviewURL string \`json:"preview_url"\`` line from the `Track` struct in `internal/spotify/models.go`.

- [ ] **Step 3: Run the tests**

Run: `make test` — expected PASS.

- [ ] **Step 4: Update the documentation**

In `docs/feature-parity.md`, add to the statistics table:

```markdown
| Genre statistics | **Implemented** | Top genres, genre timeline and an obscurity score, from `artists.genres` and `artists.popularity`. Not in the reference project. Every figure reports what share of the range's listening it could see, because genres exist only where enrichment has resolved the artist. |
| Playback-context statistics | **Implemented** | Skip rate, shuffle share, how tracks ended, platform and country. Not in the reference project. Only extended-export rows carry these columns, so each figure reports its own denominator. |
```

In the same file, add to the "Deliberate deviations" or "Known gaps" section:

```markdown
- **Audio features, analysis, recommendations and related artists are permanently unavailable**, not
  deferred. Spotify restricted them on 2024-11-27 to applications already in extended quota mode,
  which now requires 250,000 monthly active users. Encore runs against the operator's own Spotify
  application, so every instance is a new application in development mode and receives 403 from all
  of them. See [`docs/design/2026-07-29-spotify-api-expansion-overview.md`](design/2026-07-29-spotify-api-expansion-overview.md).
```

In `docs/api.md`, document the four new endpoints in the style the file already uses, including the shared coverage shape.

In `README.md`, add genres and listening habits to the "Shows you the statistics" bullet.

- [ ] **Step 5: Full verification**

Run: `make lint` — expected clean.
Run: `make test` — expected PASS.
Run: `make test-integration` — expected PASS.
Run: `cd web && npm run lint && npm run typecheck && npm run build` — expected PASS.

Report the actual output of each. Do not claim the phase is complete on any command you have not run.

- [ ] **Step 6: Commit**

```bash
git add internal/spotify/models.go docs/feature-parity.md docs/api.md README.md
git commit -m "Docs: record what the new statistics can and cannot see

Also deletes Track.PreviewURL. Spotify removed preview_url from track
responses in the same November 2024 change that took audio features away, so
the field has been decoded as empty and read by nothing since.

The parity doc gains the deprecations as a permanent constraint rather than
an outstanding item. Encore runs against the operator's own Spotify
application, which makes every instance a new application in development
mode, which means 403 from all seven of those endpoints for as long as the
250,000-user threshold stands. Writing it down stops it being rediscovered
as a gap every few months.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
```

---

## Definition of done

- [ ] `make lint` clean.
- [ ] `make test` passes.
- [ ] `make test-integration` passes.
- [ ] `cd web && npm run lint && npm run typecheck && npm run build` passes.
- [ ] `/genres` and `/habits` render against a real instance, including their low-coverage and zero-coverage states.
- [ ] A shared link carries genres and obscurity, and a test asserts it carries no playback-context field names.
- [ ] No migration was added. No `go.mod` entry was added. `internal/config/config.go:398` is unchanged.
