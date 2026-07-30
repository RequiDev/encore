# Phase 2b — Album Completion Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Answer "how much of this album have I actually heard?" from data already on disk.

**Architecture:** `albums.total_tracks` has been stored and populated since the catalogue existed and feeds nothing. Counting distinct played tracks against it gives completion with no Spotify call, no scope and no new table. Two statistics: per-album completion (all-time, on the album page) and a range-scoped aggregate (in extras).

**Tech Stack:** Go 1.26, pgx/v5, PostgreSQL 17, React 19 + TypeScript + TanStack Query.

**Spec:** [`docs/design/2026-07-29-phase-2-scope-expansion-design.md`](../../design/2026-07-29-phase-2-scope-expansion-design.md) §5.1.

## Global Constraints

- **No database migration.** `albums.total_tracks integer NOT NULL DEFAULT 0` already exists (`migrations/00003_catalog.sql:34`) and is already surfaced as `totalTracks` on the album DTO (`internal/httpapi/dto.go:186`).
- **No Spotify API call and no OAuth scope change.** This is the half of the completion feature that needs neither. Naming *which* tracks are missing needs `GET /albums/{id}/tracks` and is Phase 2e.
- **No new Go module dependency and no new npm dependency.**
- **`total_tracks = 0` means "not resolved yet", not "an album with no tracks."** Such albums are excluded from both numerator and denominator everywhere. Reporting one as 0% complete would be wrong, and on a freshly imported instance it would be wrong for almost every album.
- Test DB on port **5433**, not 5432. `make` is NOT installed — run Makefile recipes directly.
- `go test -race` will NOT work: no gcc, cgo unavailable. Omit it. CI runs it on Linux.
- staticcheck at `$(go env GOPATH)/bin`; `export PATH="$PATH:$(go env GOPATH)/bin"` before linting.
- **NUL check every file you write:** `perl -0777 -ne 'print "NULs: ", tr/\0//, "\n"' <file>` — expect 0. An earlier phase embedded one and every automated gate passed.
- Commit style `Area: lowercase summary`, body explaining *why*, ending `Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>`. Stage paths explicitly; never `git commit -a`.

## The two definitions, and why they differ

They are deliberately scoped differently, and the difference must reach the UI copy.

**Per-album completion is ALL-TIME.** "You have heard 9 of 12 tracks on this album" is a property of a listening lifetime, not of a date range. Range-scoping it would mean opening an album with a seven-day window showed "1 of 12" and called it completion, which is false. There is precedent on that exact page: `EntityStats.DiscoveredAt` and `LastPlayedAt` (`internal/stats/entity.go:45-46`) are already all-time while everything beside them is range-scoped — see the `ever` CTE in `entityStatsSQL` at `internal/stats/entity.go:119-123`, which drops the range predicate and keeps the user and blacklist ones.

**The aggregate is RANGE-SCOPED, and names its own denominator.** "Of the 87 albums you played in this range, you have heard every track on 12." It sits in `/api/stats/extras`, where everything is range-scoped, so a range-scoped figure is the consistent choice — and stating the denominator in the sentence is the convention Phase 1 established for exactly this reason.

---

## File Structure

| File | Responsibility |
|---|---|
| `internal/stats/completion.go` | **Create.** Both statistics and their SQL |
| `internal/stats/stats_test.go` | **Modify.** Register the two statements |
| `internal/httpapi/dto.go` | **Modify.** `completion` on the album detail, `albumsCompleted` on extras |
| `internal/httpapi/entities.go` | **Modify.** Populate album completion |
| `internal/httpapi/stats.go` | **Modify.** Populate the extras aggregate |
| `test/integration/completion_test.go` | **Create.** Integration tests |
| `web/src/lib/types.ts` | **Modify.** Both response shapes |
| `web/src/pages/AlbumDetail.tsx` | **Modify.** Show completion, labelled all-time |
| `web/src/pages/Dashboard.tsx` or wherever extras render | **Modify.** Show the aggregate |
| `docs/api.md`, `docs/feature-parity.md` | **Modify.** |

---

### Task 1: The two completion statistics

**Files:**
- Create: `internal/stats/completion.go`
- Modify: `internal/stats/stats_test.go` (the `statements()` registry)
- Test: `test/integration/completion_test.go`

**Interfaces:**
- Consumes: `rangeFilter`, `blacklistFilter`, `scope` from `internal/stats/stats.go`; `store.UUIDArg`, `postgres.Classify`.
- Produces:
  - `type AlbumCompletion struct { Heard int64; Total int64; Known bool }`
  - `type CompletedAlbums struct { Complete int64; Albums int64 }`
  - `func (s *Service) AlbumCompletion(ctx context.Context, q store.Querier, userID uuid.UUID, albumID string) (AlbumCompletion, error)`
  - `func (s *Service) CompletedAlbums(ctx context.Context, q store.Querier, userID uuid.UUID, r domain.TimeRange) (CompletedAlbums, error)`
  - Package vars `albumCompletionSQL`, `completedAlbumsSQL`.

`Known` is false when the album's `total_tracks` is 0 — enrichment has not resolved it, so there is no denominator and the caller must say "not known yet" rather than render a ratio.

- [ ] **Step 1: Write the failing integration tests**

Create `test/integration/completion_test.go`. The shared fixture in `test/integration/stats_test.go` seeds: `alb-1` holding `trk-a` and `trk-b`, `alb-2` holding `trk-c`, `alb-3` holding `trk-d`; all four tracks are played. `seedCatalog` does not set `total_tracks`, so it defaults to 0 — each test sets what it needs.

```go
//go:build integration

package integration

import (
	"testing"
	"time"

	"github.com/RequiDev/encore/internal/domain"
)

// TestAlbumCompletionCountsDistinctTracks is the core arithmetic. Playing one
// track of an album five times is still one track heard.
func TestAlbumCompletionCountsDistinctTracks(t *testing.T) {
	f := seedStats(t)
	f.env.Exec(`UPDATE albums SET total_tracks = 12 WHERE id = 'alb-1'`)

	got, err := f.svc.AlbumCompletion(f.env.Ctx(), f.env.Store.DB(), f.user.ID, "alb-1")
	if err != nil {
		t.Fatalf("album completion: %v", err)
	}
	if !got.Known {
		t.Fatal("total_tracks is 12, so completion is knowable")
	}
	// trk-a plays four times and trk-b once: two distinct tracks of twelve.
	if got.Heard != 2 || got.Total != 12 {
		t.Errorf("got %d of %d, want 2 of 12", got.Heard, got.Total)
	}
}

// TestAlbumCompletionIsAllTime is the design decision this statistic rests on.
//
// Completion is a property of a listening lifetime, not of whatever range the
// page happens to be showing. A range-scoped completion would tell somebody
// opening an album with a seven-day window that they had heard one of twelve
// tracks, which is false.
func TestAlbumCompletionIsAllTime(t *testing.T) {
	f := seedStats(t)
	f.env.Exec(`UPDATE albums SET total_tracks = 12 WHERE id = 'alb-1'`)

	// AlbumCompletion takes no range at all — this test exists to pin that its
	// answer does not move when the fixture's plays fall outside any window a
	// caller might have been looking at.
	got, err := f.svc.AlbumCompletion(f.env.Ctx(), f.env.Store.DB(), f.user.ID, "alb-1")
	if err != nil {
		t.Fatalf("album completion: %v", err)
	}
	if got.Heard != 2 {
		t.Errorf("heard = %d, want 2 regardless of any range", got.Heard)
	}
}

// TestAlbumCompletionUnknownWhenUnresolved guards the state a freshly imported
// instance is in for almost every album: total_tracks defaults to 0 because
// enrichment has not run. Zero is "we do not know", never "an album with no
// tracks", and it must not render as 0%.
func TestAlbumCompletionUnknownWhenUnresolved(t *testing.T) {
	f := seedStats(t)
	// alb-1's total_tracks is left at its 0 default.

	got, err := f.svc.AlbumCompletion(f.env.Ctx(), f.env.Store.DB(), f.user.ID, "alb-1")
	if err != nil {
		t.Fatalf("album completion: %v", err)
	}
	if got.Known {
		t.Error("total_tracks is 0, so completion cannot be known")
	}
}

// TestAlbumCompletionRespectsTheBlacklist keeps the one rule that applies to
// every statistic in this package.
func TestAlbumCompletionRespectsTheBlacklist(t *testing.T) {
	f := seedStats(t)
	f.env.Exec(`UPDATE albums SET total_tracks = 12 WHERE id = 'alb-1'`)
	// art-x is credited on both trk-a and trk-b, the two tracks of alb-1.
	f.env.Exec(`INSERT INTO user_blacklisted_artists (user_id, artist_id) VALUES ($1, 'art-x')`, f.user.ID)

	got, err := f.svc.AlbumCompletion(f.env.Ctx(), f.env.Store.DB(), f.user.ID, "alb-1")
	if err != nil {
		t.Fatalf("album completion: %v", err)
	}
	if got.Heard != 0 {
		t.Errorf("heard = %d, want 0 — blacklisted listens still counted", got.Heard)
	}
}

// TestCompletedAlbumsIsRangeScopedAndNamesItsDenominator pins the aggregate's
// shape: both numbers describe albums played inside the range, so the sentence
// "of the N you played, you have heard every track on M" is true as written.
func TestCompletedAlbumsIsRangeScoped(t *testing.T) {
	f := seedStats(t)
	// alb-2 holds only trk-c, which is played: complete.
	// alb-3 holds only trk-d, which is played: complete.
	// alb-1 holds trk-a and trk-b, both played, but claims twelve tracks.
	f.env.Exec(`UPDATE albums SET total_tracks = 12 WHERE id = 'alb-1'`)
	f.env.Exec(`UPDATE albums SET total_tracks = 1  WHERE id = 'alb-2'`)
	f.env.Exec(`UPDATE albums SET total_tracks = 1  WHERE id = 'alb-3'`)

	got, err := f.svc.CompletedAlbums(f.env.Ctx(), f.env.Store.DB(), f.user.ID, f.fullRange())
	if err != nil {
		t.Fatalf("completed albums: %v", err)
	}
	if got.Albums != 3 {
		t.Errorf("albums = %d, want 3 played in range", got.Albums)
	}
	if got.Complete != 2 {
		t.Errorf("complete = %d, want 2 (alb-2 and alb-3)", got.Complete)
	}
}

// TestCompletedAlbumsExcludesUnresolvedAlbums keeps an unenriched album from
// counting as an incomplete one and dragging the figure down.
func TestCompletedAlbumsExcludesUnresolvedAlbums(t *testing.T) {
	f := seedStats(t)
	f.env.Exec(`UPDATE albums SET total_tracks = 1 WHERE id = 'alb-2'`)
	// alb-1 and alb-3 keep total_tracks = 0.

	got, err := f.svc.CompletedAlbums(f.env.Ctx(), f.env.Store.DB(), f.user.ID, f.fullRange())
	if err != nil {
		t.Fatalf("completed albums: %v", err)
	}
	if got.Albums != 1 {
		t.Errorf("albums = %d, want 1 — only alb-2 has a known track count", got.Albums)
	}
	if got.Complete != 1 {
		t.Errorf("complete = %d, want 1", got.Complete)
	}
}

// TestCompletedAlbumsEmptyRangeIsNotAnError guards the state a new instance is
// in. Note this is a valid window containing no listens, NOT a zero-width one —
// scope() rejects from == to as a caller error by design.
func TestCompletedAlbumsEmptyRangeIsNotAnError(t *testing.T) {
	f := seedStats(t)
	from := time.Date(2025, time.January, 1, 0, 0, 0, 0, f.loc)
	empty := domain.TimeRange{From: from, To: from.AddDate(0, 0, 10)}

	got, err := f.svc.CompletedAlbums(f.env.Ctx(), f.env.Store.DB(), f.user.ID, empty)
	if err != nil {
		t.Fatalf("completed albums over an empty range: %v", err)
	}
	if got.Albums != 0 || got.Complete != 0 {
		t.Errorf("expected zeroes, got %+v", got)
	}
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `ENCORE_TEST_DATABASE_URL="postgres://encore:encore@localhost:5433/encore?sslmode=disable" go test -tags=integration -run 'TestAlbumCompletion|TestCompletedAlbums' -count=1 -v ./test/integration/`
Expected: FAIL — `f.svc.AlbumCompletion undefined`.

- [ ] **Step 3: Implement**

Create `internal/stats/completion.go`:

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

// AlbumCompletion is how much of one album somebody has heard, ever.
//
// Known is false when the album's total_tracks is still 0, which means
// enrichment has not resolved it rather than that the album is empty. Without a
// denominator there is no ratio to render, and a freshly imported instance is in
// that state for nearly every album it holds.
type AlbumCompletion struct {
	Heard int64
	Total int64
	Known bool
}

// CompletedAlbums is the range-scoped aggregate: of the albums played inside the
// range whose track count is known, how many were heard in full.
//
// Both numbers describe the same population, so "of the N albums you played in
// this range, you have heard every track on M" is true as written. Albums whose
// total_tracks is unknown are in neither.
type CompletedAlbums struct {
	Complete int64
	Albums   int64
}

// albumCompletionSQL counts the distinct tracks of one album the user has ever
// played, against the album's own track count.
//
// Deliberately not range-filtered. Completion is a property of a listening
// lifetime; scoping it to whatever window the page is showing would report "1 of
// 12" to somebody looking at a week and call it completion. The user and
// blacklist predicates still apply — the same shape as the `ever` CTE in
// entityStatsSQL, which drops the range for first- and last-listen.
//
// Parameters are $1 user, $2 album id.
var albumCompletionSQL = fmt.Sprintf(`
SELECT
    (SELECT count(DISTINCT l.track_id)
     FROM listens l
     JOIN tracks t ON t.id = l.track_id
     WHERE l.user_id = $1 AND t.album_id = $2 AND %s)::bigint,
    (SELECT coalesce(max(a.total_tracks), 0) FROM albums a WHERE a.id = $2)::bigint`,
	blacklistFilter("l"))

// completedAlbumsSQL counts, within the range, albums heard in full.
//
// An album whose total_tracks is 0 is excluded from both counts rather than
// treated as incomplete: 0 means enrichment has not resolved it, and counting it
// as an album with no tracks heard would drag the figure down for a reason that
// has nothing to do with listening.
//
// Parameters are $1 user, $2 from, $3 to.
var completedAlbumsSQL = fmt.Sprintf(`
WITH played AS (
    SELECT t.album_id, count(DISTINCT l.track_id) AS heard
    FROM listens l
    JOIN tracks t ON t.id = l.track_id
    WHERE %s AND t.album_id IS NOT NULL
    GROUP BY t.album_id
)
SELECT count(*)::bigint,
       count(*) FILTER (WHERE p.heard >= a.total_tracks)::bigint
FROM played p
JOIN albums a ON a.id = p.album_id
WHERE a.total_tracks > 0`, rangeFilter("l", "$1", "$2", "$3"))

// AlbumCompletion reports how much of one album the user has ever heard.
func (s *Service) AlbumCompletion(
	ctx context.Context,
	q store.Querier,
	userID uuid.UUID,
	albumID string,
) (AlbumCompletion, error) {
	// No range to validate, but a nil user id must not reach SQL looking like a
	// legitimate parameter — it would silently match nothing rather than fail.
	if userID == uuid.Nil {
		return AlbumCompletion{}, fmt.Errorf("%w: a user is required", domain.ErrValidation)
	}
	var out AlbumCompletion
	err := q.QueryRow(ctx, albumCompletionSQL, store.UUIDArg(userID), albumID).
		Scan(&out.Heard, &out.Total)
	if err != nil {
		return AlbumCompletion{}, postgres.Classify("album completion", err)
	}
	out.Known = out.Total > 0
	if !out.Known {
		// Without a denominator the numerator says nothing useful, and shipping
		// it invites a caller to render "3 of 0".
		out.Heard = 0
	}
	return out, nil
}

// CompletedAlbums reports how many of the range's albums were heard in full.
func (s *Service) CompletedAlbums(
	ctx context.Context,
	q store.Querier,
	userID uuid.UUID,
	r domain.TimeRange,
) (CompletedAlbums, error) {
	if err := checkScope(userID, r); err != nil {
		return CompletedAlbums{}, err
	}
	var out CompletedAlbums
	err := q.QueryRow(ctx, completedAlbumsSQL,
		store.UUIDArg(userID), r.From.UTC(), r.To.UTC()).
		Scan(&out.Albums, &out.Complete)
	if err != nil {
		return CompletedAlbums{}, postgres.Classify("completed albums", err)
	}
	return out, nil
}
```

`checkScope(userID uuid.UUID, r domain.TimeRange) error` is at `internal/stats/stats.go:75` — verified. `AverageAlbumReleaseYear` (`internal/stats/dashboard.go:122-127`) is the precedent for a range-scoped statistic that needs no timezone: it calls `checkScope` and then passes `store.UUIDArg(userID), r.From.UTC(), r.To.UTC()`. Match that shape exactly.

`AlbumCompletion` takes no range at all, so it needs no `checkScope` — but it must still reject a nil user id. Guard it explicitly rather than letting `uuid.Nil` reach SQL as a valid-looking parameter.

- [ ] **Step 4: Register the statements**

In `statements()` in `internal/stats/stats_test.go`:

```go
		{name: "albumCompletion", sql: albumCompletionSQL, params: 2},
		{name: "completedAlbums", sql: completedAlbumsSQL, params: 3},
```

**Note:** `TestBlacklistIsAppliedEverywhere` requires any statement matching `FROM listens`/`JOIN listens` to contain `user_blacklisted_artists`. Both do — `albumCompletionSQL` via `blacklistFilter("l")` directly, `completedAlbumsSQL` via `rangeFilter`. If either test fails, the fragment is missing, which is the point of the test.

- [ ] **Step 5: Run everything**

Run: `go test -count=1 ./internal/stats/` — expected PASS (the two registry tests).
Run: `ENCORE_TEST_DATABASE_URL="postgres://encore:encore@localhost:5433/encore?sslmode=disable" go test -tags=integration -run 'TestAlbumCompletion|TestCompletedAlbums' -count=1 -v ./test/integration/` — expected PASS, all seven.
Run: `go test -count=1 ./...` — expected PASS.

- [ ] **Step 6: Lint and commit**

```bash
export PATH="$PATH:$(go env GOPATH)/bin"
gofmt -l $(git ls-files '*.go') && go vet ./... && staticcheck ./...
git add internal/stats/completion.go internal/stats/stats_test.go test/integration/completion_test.go
git commit -m "Stats: how much of an album you have actually heard

albums.total_tracks has been stored and populated since the catalogue
existed and fed nothing. Counting distinct played tracks against it answers
a question no Spotify call is needed for.

Per-album completion is all-time on purpose. It is a property of a listening
lifetime, and scoping it to whatever window the page happens to show would
tell somebody looking at a week that they had heard one of twelve tracks and
call that completion. The album page already carries all-time fields beside
range-scoped ones — first and last listen come from a CTE that drops the
range the same way.

The aggregate is range-scoped instead, and names its own denominator: of the
albums you played in this range, how many you heard in full. Both numbers
describe one population, so the sentence is true as written.

total_tracks = 0 means enrichment has not resolved the album, never that the
album is empty. Such albums are in neither count. Treating one as nothing-
heard would drag the figure down for a reason that has nothing to do with
listening, and on a freshly imported instance it would do so for nearly
every album.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
```

---

### Task 2: Serve both figures

**Files:**
- Modify: `internal/httpapi/dto.go`, `internal/httpapi/entities.go`, `internal/httpapi/stats.go`
- Test: `internal/httpapi/httpapi_test.go`

**Interfaces:**
- Consumes: `AlbumCompletion`, `CompletedAlbums`, and the two service methods from Task 1.
- Produces:
  - `AlbumCompletionResponse{Heard int64 \`json:"heard"\`; Total int64 \`json:"total"\`; Known bool \`json:"known"\`}`
  - `AlbumDetail.Completion *AlbumCompletionResponse \`json:"completion,omitempty"\``
  - `StatsExtras.AlbumsCompleted *CompletedAlbumsResponse \`json:"albumsCompleted,omitempty"\`` where `CompletedAlbumsResponse{Complete int64 \`json:"complete"\`; Albums int64 \`json:"albums"\`}`

- [ ] **Step 1: Add the DTOs**

Append to `internal/httpapi/dto.go`:

```go
// AlbumCompletionResponse is how much of an album somebody has heard, ever.
//
// Known is false when the album's track count has not been enriched yet. The
// client must render "not known yet" rather than a ratio in that case — a
// freshly imported instance is in it for nearly every album.
type AlbumCompletionResponse struct {
	Heard int64 `json:"heard"`
	Total int64 `json:"total"`
	Known bool  `json:"known"`
}

// CompletedAlbumsResponse is the range-scoped aggregate. Both numbers describe
// albums played inside the range whose track count is known.
type CompletedAlbumsResponse struct {
	Complete int64 `json:"complete"`
	Albums   int64 `json:"albums"`
}

func toAlbumCompletion(c stats.AlbumCompletion) AlbumCompletionResponse {
	return AlbumCompletionResponse{Heard: c.Heard, Total: c.Total, Known: c.Known}
}

func toCompletedAlbums(c stats.CompletedAlbums) CompletedAlbumsResponse {
	return CompletedAlbumsResponse{Complete: c.Complete, Albums: c.Albums}
}
```

Add `Completion *AlbumCompletionResponse \`json:"completion,omitempty"\`` to the `AlbumDetail` struct (`dto.go:508`), and `AlbumsCompleted *CompletedAlbumsResponse \`json:"albumsCompleted,omitempty"\`` to `StatsExtras`.

- [ ] **Step 2: Populate them**

In `handleAlbum` (`internal/httpapi/entities.go:275`), after the existing `AlbumStats` call and before `writeJSON`:

```go
	completion, err := s.stats.AlbumCompletion(ctx, s.querier, user.ID, id)
	if err != nil {
		writeError(w, r, err)
		return
	}
```

then set `Completion: &c` where `c := toAlbumCompletion(completion)` on the `AlbumDetail` literal.

In `handleExtras` (`internal/httpapi/stats.go:468`), after the existing `AverageArtistsPerTrack` call:

```go
	albums, err := s.stats.CompletedAlbums(ctx, s.querier, user.ID, tr)
	if err != nil {
		writeError(w, r, err)
		return
	}
	completed := toCompletedAlbums(albums)
	out.AlbumsCompleted = &completed
```

- [ ] **Step 3: Write the API test**

Follow the idiom of the existing tests in `internal/httpapi/httpapi_test.go` — `newTestServer(t)`, `ts.signedIn(...)`, `ts.do(httptest.NewRequest(...))`. Read the file first; the `/api/albums/{id}` path may need a fake catalog and stats seam that the harness does not currently provide, in which case **say so in your report and cover this at the integration level instead** rather than building a new harness. The integration tests from Task 1 already cover the arithmetic; what an API test adds here is the wiring, and that is also observable end to end.

- [ ] **Step 4: Run everything**

Run: `go test -count=1 ./...` — expected PASS.
Run: `ENCORE_TEST_DATABASE_URL="postgres://encore:encore@localhost:5433/encore?sslmode=disable" go test -tags=integration -count=1 -p 1 -timeout=20m ./test/...` — expected PASS.

- [ ] **Step 5: Lint and commit**

```bash
export PATH="$PATH:$(go env GOPATH)/bin"
gofmt -l $(git ls-files '*.go') && go vet ./... && staticcheck ./...
git add internal/httpapi/dto.go internal/httpapi/entities.go internal/httpapi/stats.go internal/httpapi/httpapi_test.go
git commit -m "API: serve album completion and the completed-album count

Completion rides on the album detail response rather than getting its own
endpoint, because it is one more fact about an album somebody is already
looking at and a second round trip would buy nothing.

Both fields are pointers with omitempty so that a response which could not
compute them is distinguishable from one reporting zero.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
```

---

### Task 3: Show it

**Files:**
- Modify: `web/src/lib/types.ts`, `web/src/pages/AlbumDetail.tsx`
- Modify: whichever page renders `StatsExtras` — **find it** by grepping for `qk.extras`
- Test: `web/src/test/` if the touched pages have coverage there

**Interfaces:**
- Consumes: `completion` on the album detail payload, `albumsCompleted` on extras.

**The copy is the deliverable here.** These two figures are scoped differently and the interface must say so, or somebody will reasonably assume both follow the range picker.

1. **On the album page**, beside the existing statistics: `Heard 9 of 12 tracks`. Because every other number on that page is range-scoped and this one is not, it must carry a short qualifier — "all time", in the same register the page already uses for first and last listen. Check how those two are labelled and match them; they have the same property and the solution should look the same.
2. **When `known` is false**, render "Track count not known yet" — never a ratio, never 0%. Link to `/settings` where enrichment progress already lives, matching what the Genres page does at zero coverage.
3. **When `heard >= total`**, say so distinctly — "Heard every track" reads better than "12 of 12" and is the state worth noticing.
4. **For the aggregate**, the sentence names its denominator: "Heard every track on 12 of the 87 albums you played in this range."

Follow the existing component idiom; do not invent primitives. Read `web/src/pages/AlbumDetail.tsx` and a Phase 1 page (`Genres.tsx` or `Habits.tsx`) for how coverage-style qualifiers were handled there.

- [ ] **Step 1: Add the types**

```ts
/** How much of an album has been heard, ever — not scoped to the selected range. */
export interface AlbumCompletion {
  heard: number
  total: number
  /** False when the album's track count has not been enriched yet. */
  known: boolean
}

/** Range-scoped: both numbers describe albums played inside the selected range. */
export interface CompletedAlbums {
  complete: number
  albums: number
}
```

Add `completion?: AlbumCompletion` to the album detail type and `albumsCompleted?: CompletedAlbums` to the extras type.

- [ ] **Step 2: Render both**

Per the four copy requirements above.

- [ ] **Step 3: Verify**

Run: `cd web && npm run lint && npm run typecheck && npm run build && npm run test` — all must pass.
NUL-check every touched file.

- [ ] **Step 4: Commit**

```bash
git add web/src/lib/types.ts web/src/pages/AlbumDetail.tsx <extras page> <any test>
git commit -m "Web: show how much of an album you have heard

Labelled all time, because it is, and because every other number on that
page follows the range picker — the same qualifier first and last listen
already carry, for the same reason.

An album whose track count has not been enriched says so rather than
rendering a ratio it cannot compute. Hearing every track gets its own
wording, because that is the state worth noticing and 12 of 12 buries it.

The aggregate names its denominator in the sentence, so nobody has to guess
which albums the count is out of.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
```

---

### Task 4: Documentation

**Files:**
- Modify: `docs/api.md`, `docs/feature-parity.md`

- [ ] **Step 1: Document the fields**

In `docs/api.md`, add `completion` to the `/api/albums/{id}` payload and `albumsCompleted` to `/api/stats/extras`, in that file's existing style. State both scopings explicitly — completion all-time, the aggregate range-scoped — and what `known: false` means.

- [ ] **Step 2: Add the parity row**

In `docs/feature-parity.md`, in the statistics table:

```markdown
| Album completion | **Implemented** | "Heard 9 of 12 tracks", from `albums.total_tracks`, which needs no Spotify call. All-time rather than range-scoped, because completion is a property of a listening lifetime. An album whose track count has not been enriched reports that rather than a ratio. Naming *which* tracks are missing needs `GET /albums/{id}/tracks` and is not built. |
```

- [ ] **Step 3: Full verification — the plan's final evidence**

```
export PATH="$PATH:$(go env GOPATH)/bin"
gofmt -l $(git ls-files '*.go')
go vet ./...
staticcheck ./...
go test -count=1 ./...
ENCORE_TEST_DATABASE_URL="postgres://encore:encore@localhost:5433/encore?sslmode=disable" go test -tags=integration -count=1 -p 1 -timeout=20m ./test/...
cd web && npm run lint && npm run typecheck && npm run build && npm run test
```

**Report the real output. Do not claim a pass on a command you did not run.**

- [ ] **Step 4: Commit**

```bash
git add docs/api.md docs/feature-parity.md
git commit -m "Docs: album completion, and which half of it is built

Records the scoping difference, because it is the thing somebody reading the
JSON would otherwise get wrong: completion is all-time, the aggregate beside
it is range-scoped.

Also says plainly that naming the missing tracks is not built. The row
claiming completion without that caveat would read as more than shipped.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
```

---

## Definition of done

- [ ] `gofmt -l`, `go vet`, `staticcheck` clean.
- [ ] `go test -count=1 ./...` passes.
- [ ] Full integration suite passes against port 5433.
- [ ] `cd web && npm run lint && npm run typecheck && npm run build && npm run test` passes.
- [ ] An album with `total_tracks = 0` renders "not known yet", never a ratio or 0%.
- [ ] Completion does not change when the range picker changes; the aggregate does.
- [ ] No migration. No Spotify call. No scope change. `go.mod` and `web/package.json` unchanged.
