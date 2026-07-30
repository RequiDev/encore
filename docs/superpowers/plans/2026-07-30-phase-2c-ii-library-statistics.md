# Phase 2c-ii — Library Statistics Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Cross what a listener saved and follows against what they actually played — "saved three years ago, never played" and "played 214 times, never saved".

**Architecture:** Phase 2c-i populates `user_saved_tracks`, `user_saved_albums` and `user_followed_artists` from a daily worker, and records `spotify_credentials.library_synced_at`. This plan joins those against `listens` and renders the result.

**Tech Stack:** Go 1.26, pgx/v5, PostgreSQL 17, React 19 + TypeScript + TanStack Query.

**Spec:** [`docs/design/2026-07-29-phase-2-scope-expansion-design.md`](../../design/2026-07-29-phase-2-scope-expansion-design.md) §3.4.

## Global Constraints

- **No migration.** Phase 2c-i's `00010_library.sql` created everything. No DDL.
- **No Spotify API call and no OAuth scope change.** Everything here reads local tables. If you find yourself in `internal/spotify/` or touching `DefaultScopes()`, stop.
- **No new Go module dependency and no new npm dependency.**
- **`library_synced_at IS NULL` means never enumerated, not "you saved nothing".** Every account is NULL until the worker's first successful run, so this is the *common* state on an upgraded instance, not an edge case. It must reach the UI as its own state and never as zeroes.
- Test DB on port **5433**, not 5432. `make` is NOT installed.
- `go test -race` will NOT work: no gcc, cgo unavailable. Omit it. CI runs it on Linux.
- staticcheck at `$(go env GOPATH)/bin`; `export PATH="$PATH:$(go env GOPATH)/bin"` first.
- **NUL check every file you write:** `perl -0777 -ne 'print "NULs: ", tr/\0//, "\n"' <file>` — expect 0.
- Commit style `Area: lowercase summary`, body explaining *why*, ending `Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>`. Stage paths explicitly; never `git commit -a`.

## The scoping split, again — and it is three ways this time

Phases 2b and 2c-i both had to distinguish all-time figures from range-scoped ones. This plan has both plus a third thing, and the UI must make all three legible:

- **Saved but never played is ALL-TIME.** "Never played" means never. Range-scoping it would list every saved track you did not happen to play last week and call it neglected.
- **Played but never saved is RANGE-SCOPED.** "What am I playing a lot lately and haven't saved" is a question about a period.
- **Followed but dormant is RANGE-SCOPED.** Same reasoning: dormant *within the window you are looking at*.
- **The counts and `syncedAt` are neither** — they describe the last enumeration, which has no relationship to the range picker at all.

---

## File Structure

| File | Responsibility |
|---|---|
| `internal/stats/library.go` | **Create.** The watermark, the counts, three cross-reference queries |
| `internal/stats/stats_test.go` | **Modify.** Register the statements |
| `internal/httpapi/dto.go`, `stats.go`, `router.go` | **Modify.** `GET /api/stats/library` |
| `test/integration/librarystats_test.go` | **Create.** |
| `web/src/lib/types.ts`, `query.ts`, `App.tsx`, nav | **Modify.** |
| `web/src/pages/Library.tsx` | **Create.** |
| `docs/api.md`, `docs/feature-parity.md` | **Modify.** |

---

### Task 1: The statistics

**Files:**
- Create: `internal/stats/library.go`
- Modify: `internal/stats/stats_test.go`
- Test: `test/integration/librarystats_test.go`

**Interfaces:**
- Consumes: `rangeFilter`, `blacklistFilter`, `scope`, `clampLimit` from `internal/stats/stats.go`; `store.UUIDArg`, `postgres.Classify`.
- Produces:
  - `type LibraryStats struct { SyncedAt *time.Time; SavedTracks, SavedAlbums, FollowedArtists int64; SavedNeverPlayed []SavedTrackEntry; PlayedNeverSaved []PlayedTrackEntry; DormantFollows []DormantArtistEntry }`
  - `type SavedTrackEntry struct { TrackID string; AddedAt *time.Time }`
  - `type PlayedTrackEntry struct { TrackID string; Plays int64; MsPlayed int64 }`
  - `type DormantArtistEntry struct { ArtistID string; LastPlayedAt *time.Time }`
  - `func (s *Service) Library(ctx context.Context, q store.Querier, userID uuid.UUID, r domain.TimeRange, tz string, limit int) (LibraryStats, error)`

`SyncedAt` is nil when `library_synced_at` is NULL. **Return it faithfully — do not substitute a zero time**, because the whole point is that the caller can tell "never enumerated" from "enumerated and empty".

The entries carry ids only, not names. The handler resolves them through the existing `resolveRefs` machinery the other endpoints use, exactly as `handleAlbum` does — do not join the catalogue here.

- [ ] **Step 1: Write the failing integration tests**

Create `test/integration/librarystats_test.go`. The shared fixture in `test/integration/stats_test.go` seeds `trk-a`..`trk-d` with plays, and `art-x`/`art-y`/`art-z`. Your tests add library rows with `f.env.Exec(...)`.

Cover:
- **Never synced** — with no `library_synced_at`, `SyncedAt` is nil and all three counts are zero. This is the common state and must not error.
- **Counts** reflect the three tables.
- **Saved but never played is all-time**: save `trk-a` (played) and a `trk-zzz` (never played); only `trk-zzz` comes back — and it still comes back when the range is narrowed to a window containing no plays at all, because "never" is not range-scoped.
- **Played but never saved is range-scoped**: with nothing saved, `trk-a` appears; narrow the range to exclude its plays and it does not.
- **Followed but dormant**: follow `art-x` (played in range) and `art-z2` (never played); only `art-z2` comes back. Widen/narrow the range and confirm it tracks.
- **Blacklist applies**: blacklisting an artist removes its tracks from played-never-saved, and removes the artist from dormant-follows entirely rather than listing it as dormant.
- **`limit` is honoured** on all three lists.
- **Empty range is not an error** — use a valid window containing no listens, NOT a zero-width one (`scope()` rejects `from == to` by design).

- [ ] **Step 2: Run to verify they fail**

Run: `ENCORE_TEST_DATABASE_URL="postgres://encore:encore@localhost:5433/encore?sslmode=disable" go test -tags=integration -run TestLibraryStats -count=1 -v ./test/integration/`
Expected: FAIL, `f.svc.Library undefined`.

**Name every test with a `TestLibraryStats` prefix** so that command matches them — an earlier task had to rename eleven tests for exactly this reason.

- [ ] **Step 3: Implement**

Create `internal/stats/library.go`. Four statements:

1. **`librarySnapshotSQL`** — `library_synced_at` plus the three counts, in one round trip. Parameters `$1` user. Reads `spotify_credentials` and the three library tables. **This one does not touch `listens`**, so `TestBlacklistIsAppliedEverywhere` will not require the fragment — verify that test's condition to be sure.

2. **`savedNeverPlayedSQL`** — `user_saved_tracks` LEFT JOIN `listens` on `(user_id, track_id)`, keeping the misses, newest `added_at` first. **No range predicate** — user and blacklist only, the same shape as the `ever` CTE in `entityStatsSQL` (`internal/stats/entity.go:119-123`). Parameters `$1` user, `$2` limit.

3. **`playedNeverSavedSQL`** — in-range listens grouped by track, excluding tracks in `user_saved_tracks`, ranked by plays. Composes `rangeFilter`. Parameters `$1` user, `$2` from, `$3` to, `$4` limit.

4. **`dormantFollowsSQL`** — `user_followed_artists` minus artists with any in-range listen, carrying each artist's last play ever so the UI can say how long it has been. Composes `rangeFilter` for the exclusion. Parameters `$1` user, `$2` from, `$3` to, `$4` limit.

**On the blacklist in `dormantFollowsSQL`:** a blacklisted artist must be **absent entirely**, not reported as dormant. Think about which side of the query the fragment belongs on — excluding a blacklisted artist from the "has plays in range" set would *promote* it into the dormant list, which is exactly backwards. A test covers this; make it pass for the right reason.

Method shape: follow `internal/stats/genre.go`'s `TopGenres` — validate with `scope`, clamp the limit with `clampLimit`, run the queries, scan into the struct, wrap errors with `postgres.Classify`.

- [ ] **Step 4: Register the statements**

Add all four to `statements()` in `internal/stats/stats_test.go` with their exact parameter counts. `TestParameterNumberingIsContiguous` and `TestBlacklistIsAppliedEverywhere` will catch a mistake — that is their purpose.

- [ ] **Step 5: Run, lint, commit**

Run `go test -count=1 ./internal/stats/`, the targeted integration run, then `go test -count=1 ./...` and the full integration suite. Lint. Commit with a message explaining the three-way scoping split and why a blacklisted artist must not surface as dormant.

---

### Task 2: The endpoint

**Files:**
- Modify: `internal/httpapi/dto.go`, `internal/httpapi/stats.go`, `internal/httpapi/router.go`
- Test: the auth-required route table in `internal/httpapi/httpapi_test.go`; wiring in `test/e2e/e2e_test.go` if the unit harness has no seam

**Interfaces:**
- Produces: `GET /api/stats/library` returning `syncedAt` (nullable), `savedTracks`, `savedAlbums`, `followedArtists`, and the three lists with entities resolved.

**`syncedAt` must serialise as JSON `null` when never synced**, not as `"0001-01-01T00:00:00Z"` and not omitted — the client branches on it, and an omitted field is indistinguishable from an older server.

Resolve entity names through the existing `resolveRefs` used by `handleAlbum` and the top-N handlers, so the lists carry names and artwork like every other list in the API.

**Note from Phase 2b:** `catalog` and `stats` on `Server` are concrete `*catalog.Repo` / `*stats.Service`, not interfaces, so `internal/httpapi`'s unit harness has no fake seam for them. **Check before building scaffolding** — if a unit test would need new harness machinery, put the wiring test in `test/e2e/e2e_test.go` instead and say so, as 2b did.

- [ ] **Steps:** DTOs; handler; route registration beside the other `/api/stats/*` routes; add the path to the auth table test; run everything; lint; commit.

---

### Task 3: The page

**Files:**
- Create: `web/src/pages/Library.tsx`
- Modify: `web/src/lib/types.ts`, `web/src/lib/query.ts`, `web/src/App.tsx`, the nav, and `web/src/components/ui/Icon.tsx` if a new glyph is needed

**Required states — these are the task, and three of them are more likely than the happy path on an upgraded instance:**

1. **Missing scope.** If `/api/me`'s `missingScopes` contains `user-library-read` or `user-follow-read`, the page says the library has not been shared and links to `/api/auth/spotify/relink` — it does **not** show an empty library. Phase 2a already provides `missingScopes`; read it from the session rather than adding a query.
2. **Never synced** (`syncedAt` null, scopes granted): "Encore has not read your Spotify library yet. It checks once a day; this page will fill in after the next run." **Do not render zero counts.**
3. **Synced**: say when — "Last read from Spotify 3 hours ago." Somebody looking at "saved but never played" needs to know how fresh it is, and it can be up to a day stale by design.
4. **Synced but empty**: an account that genuinely saved nothing gets "You have not saved anything on Spotify" — distinct from state 2, which is the whole reason `syncedAt` is nullable.

**The three-way scoping must be legible.** Label "saved but never played" as all-time; the other two follow the range picker. Reuse whatever convention `AlbumDetail.tsx` settled on for its all-time figure — 2b matched `EntityFigures`' `· all time` hint, and a third variant would be one too many.

Constraints as ever: `format.ts` helpers only, no raw `toLocaleString`; `SERIES_LIMIT = 4` if anything is categorically coloured; `useSearchParams` not `useState`+effect for range-dependent state; plain UTF-8, NUL-checked; no new npm dependency.

- [ ] **Steps:** types; query keys; a failing component test covering all four states; the page; route and nav; `npm run lint && typecheck && build && test`; NUL check; commit.

---

### Task 4: Documentation

**Files:** `docs/api.md`, `docs/feature-parity.md`

Document the endpoint with all three scopings stated explicitly and what `syncedAt: null` means. Add the parity rows. Say plainly that the library is a **snapshot from the last enumeration, not live** — a track saved a minute ago will not appear until the next daily run.

- [ ] **Full verification — the plan's final evidence:**

```
export PATH="$PATH:$(go env GOPATH)/bin"
gofmt -l $(git ls-files '*.go')
go vet ./...
staticcheck ./...
go test -count=1 ./...
ENCORE_TEST_DATABASE_URL="postgres://encore:encore@localhost:5433/encore?sslmode=disable" go test -tags=integration -count=1 -p 1 -timeout=20m ./test/...
cd web && npm run lint && npm run typecheck && npm run build && npm run test
```

**Report real output. Do not claim a pass on a command you did not run.**

---

## Definition of done

- [ ] All lint, unit, integration and web gates pass.
- [ ] A never-synced account renders its own state, never zero counts.
- [ ] An account missing the scopes is told so and offered relink, not shown an empty library.
- [ ] "Saved but never played" does not change when the range picker changes; the other two do.
- [ ] A blacklisted artist is absent from dormant follows, not listed as dormant.
- [ ] No migration, no Spotify call, no scope change. `go.mod` and `web/package.json` unchanged.
