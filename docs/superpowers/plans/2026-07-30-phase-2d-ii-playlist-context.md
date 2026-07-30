# Phase 2d-ii — Where a Listen Came From

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Stop discarding what a listener was playing *from*, and show which playlists they actually listen to.

**Architecture:** `spotify.PlayHistory` already carries a `Context` — playlist, album, artist or collection — and `internal/sync` throws it away. Two nullable columns on `listens` keep it; the daily worker enumerates `/me/playlists` so ids can be named; one statistic groups by it.

**Tech Stack:** Go 1.26, pgx/v5, PostgreSQL 17, React 19 + TypeScript + TanStack Query.

**Spec:** [`docs/design/2026-07-29-phase-2-scope-expansion-design.md`](../../design/2026-07-29-phase-2-scope-expansion-design.md) §4.

## The property that defines this whole plan

**Only `source = 0` rows can ever carry context.** No export format records it — not the account-data export, not the extended one. So this statistic describes *only* listening Encore recorded live, and on an instance built mostly from imports that is a small and unrepresentative slice. On a brand-new instance it is nothing at all, growing only as live sync accumulates.

That is not a caveat to bury. **Every surface must lead with its denominator**, the same discipline Phase 1 established: "based on the 3.1% of your listening Encore recorded live". If that sentence makes the feature look thin, it is telling the truth about the feature.

## A safety property, verified before planning

`domain.Listen.DedupeKey()` is `DedupeKey(l.UserID, l.Identity, l.PlayedAt)` (`internal/domain/listen.go:142`) — context is **not** an input. Adding these columns therefore cannot change which rows are considered duplicates, so re-importing or re-syncing still adds exactly zero rows. **Do not change that.** If you find yourself tempted to fold context into the identity or the dedupe key, stop: it would break the project's core guarantee that ingestion is idempotent.

## Global Constraints

- **Scope already granted.** Phase 2a put `playlist-read-private` in `DefaultScopes()`. Do not change the scope list.
- **A 403 is not a broken grant.** Read the comment on `forbidden` at `internal/sync/account.go:296`. Never `markNeedsReauth`, never retry.
- **No new Go module dependency and no new npm dependency.**
- Test DB on port **5433**, not 5432. `make` is NOT installed.
- `go test -race` will NOT work: no gcc. Omit it. CI runs it on Linux.
- staticcheck at `$(go env GOPATH)/bin`; `export PATH="$PATH:$(go env GOPATH)/bin"` first.
- **NUL check every file you write:** `perl -0777 -ne 'print "NULs: ", tr/\0//, "\n"' <file>` — expect 0.
- Commit style `Area: lowercase summary`, body explaining *why*, ending `Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>`. Stage paths explicitly; never `git commit -a`.

---

## File Structure

| File | Responsibility |
|---|---|
| `migrations/00012_playlist_context.sql` | **Create.** Two `listens` columns + `user_playlists` |
| `internal/domain/listen.go` | **Modify.** Two fields |
| `internal/sync/ingest.go` | **Modify.** Stop discarding `play.Context` |
| `internal/store/listens/listens.go` | **Modify.** Carry them through the insert |
| `internal/spotify/playlists.go` | **Modify.** `UserPlaylists` |
| `internal/store/library/playlists.go` | **Create.** Reconcile `user_playlists` |
| `internal/library/library.go` | **Modify.** A fourth enumeration |
| `internal/stats/context.go` | **Modify.** Playlist-context breakdown |
| `internal/httpapi/*`, `web/src/pages/Habits.tsx` | **Modify.** |

---

### Task 1: Schema

**Files:** Create `migrations/00012_playlist_context.sql`; modify `test/harness/harness.go` (`truncatedTables`).

```sql
-- +goose Up

-- What the listener was playing from, when Encore recorded the play live.
--
-- Both nullable with no default, so this is a metadata-only ALTER in PostgreSQL
-- and instant even on a table with millions of rows.
--
-- NULL means "not reported", which is deliberately distinct from "played from
-- nothing" — and it is the ordinary case, because NO export format carries
-- context. Only rows this instance synced live (source = 0) can ever have it.
ALTER TABLE listens ADD COLUMN context_type text;
ALTER TABLE listens ADD COLUMN context_id   text;

-- The listener's own playlists, so a context_id can be named.
--
-- Enumerated on the same daily worker tick as the library, and reconciled the
-- same way: a full listing replaces what is stored, because Spotify has no
-- delta endpoint here either.
CREATE TABLE user_playlists (
    user_id      uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    playlist_id  text        NOT NULL,
    name         text        NOT NULL DEFAULT '',
    owner_id     text        NOT NULL DEFAULT '',
    total_tracks integer     NOT NULL DEFAULT 0,
    -- Spotify's opaque version marker. Stored so a later phase can tell whether
    -- a playlist changed without re-reading its tracks; nothing uses it yet.
    snapshot_id  text        NOT NULL DEFAULT '',
    fetched_at   timestamptz NOT NULL,
    PRIMARY KEY (user_id, playlist_id)
);

-- +goose Down
DROP TABLE user_playlists;
ALTER TABLE listens DROP COLUMN context_id;
ALTER TABLE listens DROP COLUMN context_type;
```

**Deliberately no index on `listens (context_id)`.** The statistic groups by context within a user's range, which the existing `listens_user_played_idx` already leads. **Verify that claim** by reading `migrations/00005_listens.sql`'s indexes before accepting it — and if you conclude an index is needed, say so with reasoning rather than adding one speculatively. An index on the fact table is not free.

- [ ] **Steps:** write it; add `user_playlists` to `truncatedTables`; run `up`, `status`, `reset --yes`, `up` (all four must succeed — the reset/up cycle is what CI runs); run the integration suite; commit.

---

### Task 2: Stop discarding the context

**Files:** `internal/domain/listen.go`, `internal/sync/ingest.go`, `internal/store/listens/listens.go`; tests in `internal/sync/` and `test/integration/`.

**This touches the fact table's write path — the most safety-critical code in the project. Read `internal/store/listens/listens.go`'s insert CTE in full before changing it.**

**Interfaces:**
- Add `ContextType string` and `ContextID string` to `domain.Listen`, beside the existing `Platform`/`ConnCountry`/`ReasonStart` block (`internal/domain/listen.go:128-130`).
- `listenFrom(userID, play)` at `internal/sync/ingest.go:151` currently drops `play.Context`. Populate the two fields from it.
- Carry both through `internal/store/listens/listens.go`'s explicit insert column list (around line 200) and its row struct.

**`spotify.PlayContext`** (`internal/spotify/models.go:150`) has `Type` and `URI`. `Type` is one of `playlist`, `album`, `artist`, `collection`. **Store all four kinds, not just playlists** — "you played this from your Liked Songs" is as interesting as a playlist, and filtering at write time throws away information the read side could use. The URI is `spotify:playlist:37i9...`; store the **bare id**, not the URI, so it joins to `user_playlists.playlist_id` and to the catalogue directly. Extract it, and handle a malformed URI by storing nothing rather than a fragment.

**The property that must not change, and that a test must pin:** `DedupeKey` is `(UserID, Identity, PlayedAt)` and context is not an input. Re-syncing the same play must still insert zero rows. **Write a test that syncs the same play twice with context and asserts the row count is 1** — that is the project's core idempotence guarantee and this is the change most likely to threaten it.

Also pin: a play with **no** context stores NULL in both columns, not empty strings — NULL means "not reported" and the distinction is load-bearing.

- [ ] **Steps:** failing tests first; implement; run the full unit and integration suites; lint; commit.

---

### Task 3: `UserPlaylists`

**Files:** `internal/spotify/playlists.go` (modify — it already exists for playlist *creation*), `internal/spotify/playlists_test.go`.

**Interfaces:**
- `type UserPlaylist struct { ID, Name, OwnerID, SnapshotID string; TotalTracks int }`
- `func (c *Client) UserPlaylists(ctx context.Context, accessToken string, maxPages int) ([]UserPlaylist, error)`

`GET /v1/me/playlists` is **offset-paginated**, 50 max — the same shape as `SavedTracks` in `internal/spotify/library.go`. **Read that and match it exactly**, including how it reports truncation: an earlier phase found that returning a partial list with a nil error let a caller delete the tail as if it were authoritative, so `library.go` returns `ErrTruncated` alongside the partial result. **This must do the same** — the reconciliation in Task 4 has the identical delete-absent shape.

Use `c.get` (background path). The owner id lives at `owner.id` in the response; the version marker at `snapshot_id`.

- [ ] **Steps:** failing tests against an `httptest` stub (pages followed to the end; `maxPages` terminates; truncation reported as `ErrTruncated` with the partial result; a 403 surfaces as `*APIError` with `IsForbidden()`; empty yields an empty slice not nil); implement; run; lint; commit.

---

### Task 4: Enumerate playlists on the daily tick

**Files:** Create `internal/store/library/playlists.go`; modify `internal/library/library.go`; tests in both plus `test/integration/`.

**Interfaces:** on the existing `library.Repo` —
- `func (r *Repo) ReplaceUserPlaylists(ctx, q store.Querier, userID uuid.UUID, items []UserPlaylistRow) error`, delete-absent plus upsert, taking the caller's `Querier` and never `r.db`.
- `type UserPlaylistRow struct { ID, Name, OwnerID, SnapshotID string; TotalTracks int }`

The worker already enumerates the library and the top snapshots and commits everything in one `Store.InTx`. **This joins that same transaction**, and every existing rule applies unchanged: the scope check happens before the request (extend the existing gate — note `playlist-read-private` is a *third* scope, and an account lacking it must still get its library and top snapshots); a 403 abandons without touching `SyncState` or the watermark; `ErrTruncated` abandons without deleting.

**Update the cost formula** in `config.Library`'s doc comment and `docs/configuration.md`, which currently reads `... + 6`. It gains `ceil(playlists/50)`.

- [ ] **Steps:** failing tests (missing `playlist-read-private` → zero playlist requests but library and top still run; truncation deletes nothing; delete-absent works; all in one transaction); implement; run; lint; commit.

---

### Task 5: The statistic

**Files:** modify `internal/stats/context.go` (playback context already lives there); `internal/stats/stats_test.go`; `test/integration/`.

**Interfaces:**
- `type PlaylistContextEntry struct { ContextType, ContextID, Name string; Plays int64 }`
- Add `Playlists []PlaylistContextEntry` and `PlaylistCoverage Coverage` to the existing `PlaybackContext` struct.

Group in-range listens by `(context_type, context_id)` where `context_type IS NOT NULL`, left-joining `user_playlists` for the name. Compose `blacklistFilter`.

**The coverage denominator is the point.** It is `count(*) FILTER (WHERE context_type IS NOT NULL)` over all in-range listens — **per column, not per source**, matching the rule the rest of that file already follows.

**A context id that names nothing must still appear**, with an empty name — a playlist the listener deleted, one owned by somebody else, or an album or artist context that `user_playlists` will never contain. **Dropping the row would silently understate the total.** The UI decides how to render an unnamed context; the statistic must not decide for it by discarding.

- [ ] **Steps:** failing integration tests (`TestPlaylistContext` prefix) covering: grouping and counts; coverage over the per-column denominator; an unknown context id surviving with an empty name; a non-playlist context type appearing; the blacklist applying; an all-NULL instance reporting zero coverage rather than erroring. Then implement, register the statement in `statements()` with its exact parameter count, run, lint, commit.

---

### Task 6: Serve and render

**Files:** `internal/httpapi/dto.go`, `stats.go`; `web/src/lib/types.ts`, `web/src/pages/Habits.tsx`.

The existing `GET /api/stats/context` already serves `PlaybackContext`. **Extend it rather than adding a route** — this is one more fact about how somebody listened, which is exactly what that endpoint and the Habits page are for.

**The required copy, and it is the deliverable:**
1. **Lead with the denominator**, in the register `Habits.tsx` already uses for its other coverage lines: something like *"Based on the 3.1% of your listening Encore recorded live. No Spotify export records what you were playing from, so imported history cannot contribute."* The second sentence matters — without it the number looks like a bug rather than a property.
2. **Zero coverage is its own state**, not an empty chart: *"Encore has not recorded any plays live yet. This fills in as it syncs."*
3. **Unnamed contexts render honestly** — "Unknown playlist" or the context type, never a raw Spotify id and never dropped.
4. **Missing scope** (`playlist-read-private`): names are unavailable but the *counts* still work, since context comes from sync rather than from `/me/playlists`. Say that precisely rather than blanking the section.

Follow `Habits.tsx`'s existing conventions; `format.ts` helpers only; plain UTF-8, NUL-checked.

- [ ] **Steps:** DTO; handler; TS types; render; a component test covering all four states; run everything; lint; commit.

---

### Task 7: Documentation

**Files:** `docs/api.md`, `docs/feature-parity.md`, `docs/configuration.md`.

State plainly: context exists **only** on live-synced rows and no export carries it, so this statistic describes a slice that is empty on a fresh instance and small on an import-dominated one; playlist names come from a daily enumeration and may lag; an unnamed context is a deleted or foreign playlist, not an error; and the updated per-account request cost.

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
- [ ] **Re-syncing the same play still inserts zero rows** — the dedupe guarantee, pinned by a test.
- [ ] A play with no context stores NULL, not empty strings.
- [ ] An account lacking `playlist-read-private` still gets its library and top snapshots, and makes zero playlist requests.
- [ ] A truncated playlist enumeration deletes nothing.
- [ ] An unknown context id appears with an empty name rather than being dropped.
- [ ] Every surface states its denominator and explains why exports cannot contribute.
- [ ] `go.mod` and `web/package.json` unchanged.
