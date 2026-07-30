# Phase 2d-i — Spotify's Ranking vs. Yours

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Put Spotify's own top artists and tracks beside Encore's, and show where they disagree.

**Architecture:** The existing daily library worker gains a fourth enumeration: `GET /me/top/{type}` across three time ranges. Snapshots land in one table, latest capture only. The endpoint is a pure read that joins them against Encore's own ranking over the matching window.

**Tech Stack:** Go 1.26, pgx/v5, PostgreSQL 17, React 19 + TypeScript + TanStack Query.

**Spec:** [`docs/design/2026-07-29-phase-2-scope-expansion-design.md`](../../design/2026-07-29-phase-2-scope-expansion-design.md) §2.

## Deviation from the spec, decided up front

**The spec says "refreshed on demand when the stored capture is older than six hours". This plan refreshes it in the daily library worker instead.** Reasons, in order of weight:

1. Six sequential Spotify calls on a page load adds one to two seconds to the request. The library endpoint is already a pure read and this would be a second, slower pattern.
2. `internal/library` already does the things this needs and got them reviewed: the scope check *before* the request, the 403-is-not-a-broken-grant rule, the due-account listing, the single-transaction commit. Duplicating that on an HTTP path would duplicate the ways it can be got wrong.
3. Spotify computes these over ~4 weeks, ~6 months and ~1 year. They do not move meaningfully in six hours, so daily is ample.

The consequence, which the UI must state: **the snapshot is up to a day old.** Same as the library page.

## Global Constraints

- **Scope already granted.** Phase 2a put `user-top-read` in `DefaultScopes()`. Do not change the scope list.
- **A 403 is not a broken grant.** Read the comment on `forbidden` at `internal/sync/account.go:296`. Never `markNeedsReauth`, never retry, leave the watermark alone.
- **No new Go module dependency and no new npm dependency.**
- **Spotify's ranking is opaque and must be labelled as such.** It is "calculated affinity", not a play count; its time ranges are approximate; it reflects listening across every device and session, including before this Encore instance existed. **Without that sentence on the page, every disagreement reads as an Encore bug.** This is the single most important piece of copy in the plan.
- Test DB on port **5433**, not 5432. `make` is NOT installed.
- `go test -race` will NOT work: no gcc. Omit it. CI runs it on Linux.
- staticcheck at `$(go env GOPATH)/bin`; `export PATH="$PATH:$(go env GOPATH)/bin"` first.
- **NUL check every file you write:** `perl -0777 -ne 'print "NULs: ", tr/\0//, "\n"' <file>` — expect 0.
- Commit style `Area: lowercase summary`, body explaining *why*, ending `Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>`. Stage paths explicitly; never `git commit -a`.

---

## File Structure

| File | Responsibility |
|---|---|
| `migrations/00011_top_snapshots.sql` | **Create.** One table |
| `internal/spotify/topitems.go` | **Create.** `TopItems` |
| `internal/store/library/topsnapshots.go` | **Create.** Replace-in-transaction + read |
| `internal/library/library.go` | **Modify.** A fourth enumeration in the same transaction |
| `internal/stats/topdiff.go` | **Create.** The comparison |
| `internal/httpapi/*` | **Modify.** `GET /api/stats/top-diff` |
| `web/src/pages/TopDiff.tsx` | **Create.** |
| `docs/*` | **Modify.** |

---

### Task 1: Schema

**Files:**
- Create: `migrations/00011_top_snapshots.sql`
- Modify: `test/harness/harness.go` (`truncatedTables`)

Read `migrations/00010_library.sql` first and match its style — it explains *why* each choice was made, and this one should too.

```sql
-- +goose Up

-- Spotify's own ranking of a listener's top artists and tracks, as of the last
-- capture.
--
-- Only the latest capture per (user, kind, time_range) is kept: a refresh
-- replaces the whole set in one transaction. Retaining history would enable a
-- "how Spotify's view of you drifted" view and is deliberately not built — but
-- the primary key is shaped so adding captured_at to it later is a migration
-- rather than a rewrite.
--
-- Not foreign-keyed to the catalogue, for the same reason the library tables are
-- not: Spotify can rank an entity this instance has never seen, and enrichment
-- mints it afterwards.
CREATE TABLE spotify_top_snapshots (
    user_id     uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    kind        text        NOT NULL CHECK (kind IN ('artist', 'track')),
    time_range  text        NOT NULL CHECK (time_range IN ('short_term', 'medium_term', 'long_term')),
    -- Spotify's own rank, 1-based, as returned.
    position    integer     NOT NULL CHECK (position > 0),
    entity_id   text        NOT NULL,
    -- When this capture was taken. Identical across one (user, kind, range) set,
    -- because a refresh writes the whole set at one instant; carried per row so
    -- reading one set needs no join.
    captured_at timestamptz NOT NULL,
    PRIMARY KEY (user_id, kind, time_range, position)
);

-- The diff reads one whole set at a time, in rank order.
CREATE INDEX spotify_top_snapshots_set_idx
    ON spotify_top_snapshots (user_id, kind, time_range, position);

-- +goose Down
DROP TABLE spotify_top_snapshots;
```

**Note:** the primary key already covers `(user_id, kind, time_range, position)`, so decide whether the extra index earns its write cost or is redundant — **check whether the PK's btree already serves the read, and drop the index if it does.** Say which you concluded. A redundant index is a real cost, not a free safety net.

- [ ] **Steps:** write the migration; add `spotify_top_snapshots` to `truncatedTables`; run `up`, `status`, `reset --yes`, `up` against port 5433 (all four must succeed — the reset/up cycle is what CI runs); run the integration suite; commit.

---

### Task 2: The client method

**Files:**
- Create: `internal/spotify/topitems.go`, `internal/spotify/topitems_test.go`

**Interfaces:**
- Produces:
  - `type TopTimeRange string` with constants `TopShortTerm = "short_term"`, `TopMediumTerm = "medium_term"`, `TopLongTerm = "long_term"`.
  - `func (c *Client) TopArtists(ctx context.Context, accessToken string, tr TopTimeRange, limit int) ([]Artist, error)`
  - `func (c *Client) TopTracks(ctx context.Context, accessToken string, tr TopTimeRange, limit int) ([]Track, error)`

`GET /v1/me/top/{type}` is offset-paginated, but **50 is the whole picture** — nobody needs a rank-51 comparison, and one page per call keeps six calls at six requests. Take a `limit` (max 50), request one page, and do not paginate. Say so in a comment so the next reader does not think pagination was forgotten.

Use `c.get` — the **background** path, since this runs on a worker tick.

- [ ] **Steps:** failing tests against an `httptest` stub following `internal/spotify/endpoints_test.go` (cover: the three time ranges producing the right `time_range` query parameter; `limit` clamped to 50; a 403 surfacing as `*APIError` with `IsForbidden()` true and not retried; an empty `items` yielding an empty slice, not nil); run; implement; run; lint; commit.

---

### Task 3: The snapshot store

**Files:**
- Create: `internal/store/library/topsnapshots.go`
- Test: `test/integration/topsnapshots_test.go`

**Interfaces:**
- Produces on the existing `library.Repo`:
  - `func (r *Repo) ReplaceTopSnapshot(ctx context.Context, q store.Querier, userID uuid.UUID, kind, timeRange string, entityIDs []string, capturedAt time.Time) error`
  - `func (r *Repo) TopSnapshot(ctx context.Context, q store.Querier, userID uuid.UUID, kind, timeRange string) (TopSnapshot, error)` returning `type TopSnapshot struct { CapturedAt *time.Time; EntityIDs []string }` — `CapturedAt` nil when never captured, ids in rank order.

`entityIDs` arrives in rank order; position is its 1-based index. Follow `internal/store/library/library.go`'s existing `Replace*` shape exactly — delete-absent plus upsert, taking the caller's `Querier`, never `r.db` internally, so the worker can put all of it in one transaction.

**Replacing with a shorter list must delete the tail.** If a listener's top-50 becomes a top-30, positions 31–50 must go. That is the same delete-absent property the library tables have, and it is easy to get wrong here because the natural key is a position rather than an id.

- [ ] **Steps:** failing integration tests (`TestTopSnapshot` prefix so `-run TestTopSnapshot` matches them — an earlier task had to rename eleven tests for this); include: replace into empty; replace shorter deletes the tail; replace is idempotent; `CapturedAt` nil when absent; ids come back in rank order; two users do not interfere; user deletion cascades. Then implement, run, lint, commit.

---

### Task 4: Fold it into the library worker

**Files:**
- Modify: `internal/library/library.go`, `internal/library/library_test.go`
- Test: `test/integration/libraryworker_test.go`

The worker already enumerates three endpoints, mints catalogue rows, and commits everything with the watermark in one `Store.InTx`. This adds six more enumerations to the same per-account step.

**All of the existing rules apply unchanged, and the tests must prove they still do:**
- **Skip without any request** if the grant lacks `user-top-read` — extend the existing scope check, do not add a second one.
- A 403 leaves everything untouched and is not retried.
- **Truncation does not apply** here — one page is the whole picture by design — but a *failure* on any of the six must abandon the run exactly as an enumeration failure does today.
- All six `ReplaceTopSnapshot` calls go in the **same** transaction as the three library reconciliations and the watermark.
- Mint absent entity ids as `pending`, reusing the same path the library items use.

**Six extra requests per account per day.** Update the cost formula in `config.Library`'s doc comment and in `docs/configuration.md`, which currently says `ceil(saved_tracks/50) + ceil(saved_albums/50) + ceil(followed/50)`.

- [ ] **Steps:** failing tests (missing `user-top-read` skips the six calls but **still does the library three** if those scopes are present — that separation matters; a 403 on a top call abandons the whole run; the happy path writes all six sets); implement; run everything; lint; commit.

---

### Task 5: The comparison

**Files:**
- Create: `internal/stats/topdiff.go`
- Modify: `internal/stats/stats_test.go`
- Test: `test/integration/topdiff_test.go`

**Interfaces:**
- Produces:
  - `type TopDiffEntry struct { EntityID string; SpotifyRank int; EncoreRank int; Plays int64 }` — rank 0 meaning absent from that side.
  - `type TopDiff struct { CapturedAt *time.Time; TimeRange string; Entries []TopDiffEntry }`
  - `func (s *Service) TopDiff(ctx context.Context, q store.Querier, userID uuid.UUID, kind, timeRange string, tz string, limit int) (TopDiff, error)`

**The window is derived from the time range, not from the range picker.** `short_term` ≈ 4 weeks, `medium_term` ≈ 6 months, `long_term` ≈ 1 year. Encore's side must be computed over the matching window or the comparison is meaningless. **The page has no range picker for this reason** — say so in the doc comment, because a reader will otherwise wonder why.

Encore's ranking must use the same definition the existing top-N statistics use, so the two are comparable — read `internal/stats/top.go` and reuse `topSourceSQL`'s shape rather than writing a third ranking.

A full outer join in spirit: entities in Spotify's list but not Encore's, and vice versa, both appear with `0` on the missing side. Compose `blacklistFilter` on Encore's side.

- [ ] **Steps:** failing integration tests (`TestTopDiff` prefix) covering: an entity in both with different ranks; one only Spotify knows; one only Encore knows; `CapturedAt` nil when never captured, with no entries rather than an error; the blacklist removing an entity from Encore's side entirely; `limit` honoured. Then implement, register the statements in `statements()` with exact parameter counts, run, lint, commit.

---

### Task 6: Endpoint, page and docs

**Files:**
- Modify: `internal/httpapi/dto.go`, `stats.go`, `router.go`; `web/src/lib/types.ts`, `query.ts`, `App.tsx`, nav
- Create: `web/src/pages/TopDiff.tsx`
- Modify: `docs/api.md`, `docs/feature-parity.md`, `docs/configuration.md`

`GET /api/stats/top-diff?kind=artist|track&range=short_term|medium_term|long_term`. Validate both parameters against the allowed sets and 400 on anything else — do not pass caller input into SQL as a bare string.

`capturedAt` nullable, serialising as JSON `null`, with a raw-body test — the same technique used for `syncedAt`.

**The page's required copy, in order of importance:**

1. **Spotify's ranking is opaque.** Something like: "Spotify calls this 'calculated affinity'. It is not a play count, its time ranges are approximate, and it covers your listening everywhere — including before this instance existed. Disagreement with Encore's ranking is expected." Without this, every difference reads as a bug. **This is the whole reason the feature is defensible.**
2. **Never captured** (`capturedAt === null`, scope granted): its own state, not an empty table.
3. **Missing scope** (`user-top-read` in `missingScopes`): say so, link to relink. Read `missingScopes` from the session as `Library.tsx` does.
4. **Say when it was captured** — up to a day old by design.
5. **No range picker**, and a line explaining that the window comes from Spotify's own time range.

Follow `Library.tsx` — it solved the same four-state problem and its conventions are reviewed. Reuse the em-dash appositive for any range label rather than inventing a construction; reuse `format.ts` helpers; plain UTF-8, NUL-checked.

Docs must state: the snapshot is up to a day stale; `capturedAt: null` means never captured; the window is Spotify's, not the range picker's; and the updated per-account request cost.

- [ ] **Full verification — the plan's final evidence:**

```
export PATH="$PATH:$(go env GOPATH)/bin"
gofmt -l $(git ls-files '*.go'); go vet ./...; staticcheck ./...
go test -count=1 ./...
ENCORE_TEST_DATABASE_URL="postgres://encore:encore@localhost:5433/encore?sslmode=disable" go test -tags=integration -count=1 -p 1 -timeout=20m ./test/...
cd web && npm run lint && npm run typecheck && npm run build && npm run test
```

**Report real output. Do not claim a pass on a command you did not run.**

---

## Definition of done

- [ ] All gates pass; migration applies, rolls back and re-applies.
- [ ] An account without `user-top-read` is skipped for the six top calls with zero requests, and still gets its library enumerated.
- [ ] A 403 leaves `SyncState` and the watermark untouched and is not retried.
- [ ] A never-captured account renders its own state, never an empty table.
- [ ] The page states plainly that Spotify's ranking is opaque and disagreement is expected.
- [ ] Replacing a top-50 with a top-30 deletes positions 31–50.
- [ ] `go.mod` and `web/package.json` unchanged.
