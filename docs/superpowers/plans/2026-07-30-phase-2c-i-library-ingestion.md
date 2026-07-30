# Phase 2c-i — Library Ingestion Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Read what each listener has saved and who they follow, and keep three local tables in step with Spotify.

**Architecture:** A background worker enumerates `/me/tracks`, `/me/albums` and `/me/following` on an interval and reconciles each against a local table. Spotify has no delta endpoint, so every run is a full enumeration, applied in one transaction with the watermark so a partial run commits nothing.

**Tech Stack:** Go 1.26, pgx/v5, PostgreSQL 17.

**Spec:** [`docs/design/2026-07-29-phase-2-scope-expansion-design.md`](../../design/2026-07-29-phase-2-scope-expansion-design.md) §3.

**Scope note:** this plan ends when the tables are correctly populated. The statistics, endpoint and page that read them are **Phase 2c-ii**. Nothing here is user-visible, which is deliberate — it makes the ingestion provable on its own.

## Global Constraints

- **Scopes are already granted.** Phase 2a put `user-library-read` and `user-follow-read` in `DefaultScopes()`. Do not change `internal/config/config.go`'s scope list.
- **A 403 is NOT a broken grant.** Read the comment on `forbidden` at `internal/sync/account.go:296` before writing any error handling. A 403 here means an optional scope was not granted — the ordinary state of every account connected before 2a. It must **not** reach `markNeedsReauth` (that would stop ingesting a listening history which still reads perfectly) and must **not** be retried (a scope failure spends quota to fail identically).
- **No new Go module dependency.** `go.mod` unchanged.
- **Nothing under `web/`.** There is no UI in this plan.
- Test DB on port **5433**, not 5432. `make` is NOT installed — run Makefile recipes directly.
- `go test -race` will NOT work: no gcc, cgo unavailable. Omit it. CI runs it on Linux.
- staticcheck at `$(go env GOPATH)/bin`; `export PATH="$PATH:$(go env GOPATH)/bin"` first.
- **NUL check every file you write:** `perl -0777 -ne 'print "NULs: ", tr/\0//, "\n"' <file>` — expect 0.
- Commit style `Area: lowercase summary`, body explaining *why*, ending `Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>`. Stage paths explicitly; never `git commit -a`.

## The decisions this plan pins

1. **Every run is a full enumeration reconciled in one transaction.** No delta endpoint exists. Insert what is new, delete what is absent, and advance the watermark, all together — a run that fails halfway commits nothing rather than presenting a half-empty library as fact.
2. **The watermark is a column on `spotify_credentials`**, `library_synced_at`, beside the existing `last_sync_at`. NULL means never enumerated, which is distinct from enumerated-and-empty and must stay distinct: every account is NULL the moment this ships.
3. **The three tables are not foreign-keyed to the catalogue.** A saved track need not be in `tracks` yet, and making these inserts wait on catalogue rows would order the reconciliation behind unrelated work. They cascade from `users` only.
4. **Saved and followed items absent from the catalogue are minted as `pending`**, so enrichment resolves them like anything else and things somebody saved but never played become browsable for free.
5. **Never on demand.** A 5,000-track library is 100 requests. Nothing a page load does may trigger enumeration.

---

## File Structure

| File | Responsibility |
|---|---|
| `migrations/00010_library.sql` | **Create.** Three tables + the watermark column |
| `internal/spotify/library.go` | **Create.** Three paginated client methods |
| `internal/store/library/library.go` | **Create.** Transactional reconciliation |
| `internal/config/config.go` | **Modify.** `config.Library` |
| `internal/library/library.go` | **Create.** The worker loop |
| `cmd/encore-worker/main.go` | **Modify.** Register it |

---

### Task 1: Schema

**Files:**
- Create: `migrations/00010_library.sql`
- Modify: `test/harness/harness.go` (the `truncatedTables` list)

**Interfaces:**
- Produces: `user_saved_tracks`, `user_saved_albums`, `user_followed_artists`, and `spotify_credentials.library_synced_at`.

- [ ] **Step 1: Write the migration**

Create `migrations/00010_library.sql`. Read `migrations/00009_playlists.sql` first and match its goose annotations and comment style.

```sql
-- +goose Up

-- What a listener has saved and who they follow, as of the last enumeration.
--
-- Spotify has no "what changed" endpoint for any of these, so every sync is a
-- full enumeration reconciled against what is already here. That is why there
-- is no updated_at: a row either reflects the last successful run or was
-- deleted by it.
--
-- Deliberately NOT foreign-keyed to the catalogue. A saved track need not be in
-- `tracks` yet — enrichment mints and resolves it afterwards — and making these
-- inserts wait on catalogue rows would order the reconciliation transaction
-- behind work that has nothing to do with it.
CREATE TABLE user_saved_tracks (
    user_id  uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    track_id text        NOT NULL,
    -- When Spotify says it was saved. Nullable: the field is not guaranteed and
    -- an older grant may not carry it.
    added_at timestamptz,
    PRIMARY KEY (user_id, track_id)
);

CREATE TABLE user_saved_albums (
    user_id  uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    album_id text        NOT NULL,
    added_at timestamptz,
    PRIMARY KEY (user_id, album_id)
);

-- Spotify reports no "followed at" for artists, so there is nothing to record
-- beyond the fact itself.
CREATE TABLE user_followed_artists (
    user_id   uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    artist_id text NOT NULL,
    PRIMARY KEY (user_id, artist_id)
);

-- The primary keys serve the user-scoped direction. These serve the reverse:
-- "who else saved this track", and the joins from a catalogue id back to a
-- library that Phase 2c-ii's statistics walk.
CREATE INDEX user_saved_tracks_track_idx      ON user_saved_tracks (track_id);
CREATE INDEX user_saved_albums_album_idx      ON user_saved_albums (album_id);
CREATE INDEX user_followed_artists_artist_idx ON user_followed_artists (artist_id);

-- When this account's library was last enumerated in full.
--
-- NULL means never, which is distinct from "enumerated and found empty" and
-- must stay distinct: every account is NULL the moment this ships, and
-- reporting that as "you have saved nothing" would be a plausible-looking lie.
ALTER TABLE spotify_credentials ADD COLUMN library_synced_at timestamptz;

-- +goose Down
ALTER TABLE spotify_credentials DROP COLUMN library_synced_at;
DROP TABLE user_followed_artists;
DROP TABLE user_saved_albums;
DROP TABLE user_saved_tracks;
```

- [ ] **Step 2: Add the tables to the test harness truncation list**

`test/harness/harness.go` has `var truncatedTables = []string{...}` at line 154, truncated at the start of every `harness.New(t)`. **Add all three new tables.** Without this a later test inherits an earlier one's library — the same leak class that was checked for in Phase 1's rollup-parity test. `spotify_credentials` is presumably already there; verify, because the new column rides on it.

- [ ] **Step 3: Verify up, down and up again**

```bash
DSN="postgres://encore:encore@localhost:5433/encore?sslmode=disable"
ENCORE_DATABASE_URL="$DSN" go run ./cmd/encore-migrate up
ENCORE_DATABASE_URL="$DSN" go run ./cmd/encore-migrate status
ENCORE_DATABASE_URL="$DSN" go run ./cmd/encore-migrate reset --yes
ENCORE_DATABASE_URL="$DSN" go run ./cmd/encore-migrate up
```

All four must succeed. The reset/up cycle is what CI's migrations job runs; a `Down` that does not undo cleanly fails there rather than here.

- [ ] **Step 4: Run the integration suite**

Run: `ENCORE_TEST_DATABASE_URL="postgres://encore:encore@localhost:5433/encore?sslmode=disable" go test -tags=integration -count=1 -p 1 -timeout=20m ./test/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add migrations/00010_library.sql test/harness/harness.go
git commit -m "Schema: what a listener saved, and who they follow

Three tables and a watermark. No updated_at on any of them: Spotify has no
'what changed' endpoint for a library, so every sync is a full enumeration
and a row either reflects the last successful run or was deleted by it.

Not foreign-keyed to the catalogue on purpose. A saved track need not be in
tracks yet — enrichment mints it afterwards — and making these inserts wait
on catalogue rows would order the reconciliation transaction behind work
that has nothing to do with it.

library_synced_at is NULL until the first successful enumeration, which is
distinct from having enumerated and found nothing. Every account is NULL the
moment this ships, and reporting that as 'you have saved nothing' would be a
plausible-looking lie.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
```

---

### Task 2: The Spotify client methods

**Files:**
- Create: `internal/spotify/library.go`, `internal/spotify/library_test.go`

**Interfaces:**
- Consumes: `(*Client).get(ctx, path, label string, query url.Values, accessToken string, out any) error` at `internal/spotify/client.go:246`; the `Track`, `Album`, `Artist` models in `internal/spotify/models.go`.
- Produces:
  - `type SavedTrack struct { Track Track; AddedAt time.Time }`
  - `type SavedAlbum struct { Album Album; AddedAt time.Time }`
  - `func (c *Client) SavedTracks(ctx context.Context, accessToken string, maxPages int) ([]SavedTrack, error)`
  - `func (c *Client) SavedAlbums(ctx context.Context, accessToken string, maxPages int) ([]SavedAlbum, error)`
  - `func (c *Client) FollowedArtists(ctx context.Context, accessToken string, maxPages int) ([]Artist, error)`

**Two pagination shapes, and getting them confused is the main hazard.**
- `/v1/me/tracks` and `/v1/me/albums` are **offset**-paginated: `limit` (max 50) and `offset`, with `next` null on the last page.
- `/v1/me/following?type=artist` is **cursor**-paginated *and* nests everything under an `artists` object: `{"artists": {"items": [...], "cursors": {"after": "..."}, "next": ...}}`. **`RecentlyPlayed` at `internal/spotify/recentlyplayed.go:61` already does cursor pagination — read it and match, including its repeat-cursor guard**, because a cursor that comes back round would page forever.

All three are **background** calls (`c.get`, not the interactive path), so they queue behind the catalogue budget rather than the sign-in one.

- [ ] **Step 1: Write the failing tests**

Follow the idiom of `internal/spotify/endpoints_test.go` — an `httptest` server standing in for Spotify. Read it first for how a client is pointed at the stub.

Cover, for each of the three methods:
- one page, exhausted correctly;
- three pages followed to the end, in order;
- **`maxPages` respected** — a stub that always returns another page must terminate, not hang;
- a 403 surfaced as an `*APIError` with `IsForbidden()` true, returned unchanged rather than retried or swallowed.

And specifically for `FollowedArtists`:
- the response nests under `artists`, so a flat `items` at the top level yields nothing;
- **a repeated `after` cursor terminates** rather than looping.

- [ ] **Step 2: Run to verify they fail**

Run: `go test -run 'TestSavedTracks|TestSavedAlbums|TestFollowedArtists' -count=1 ./internal/spotify/`
Expected: FAIL, undefined.

- [ ] **Step 3: Implement**

Create `internal/spotify/library.go`:

```go
package spotify

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	// maxLibraryPageSize is Spotify's cap on all three of these endpoints.
	maxLibraryPageSize = 50
	// defaultLibraryPages bounds an enumeration whose caller did not say how far
	// it is willing to walk. Fifty a page, so 200 pages is 10,000 items — larger
	// than any personal library, and small enough that a misbehaving upstream
	// cannot spend the whole quota here.
	defaultLibraryPages = 200
)

// SavedTrack is one entry of a listener's saved tracks.
type SavedTrack struct {
	Track   Track
	AddedAt time.Time
}

// SavedAlbum is one entry of a listener's saved albums.
type SavedAlbum struct {
	Album   Album
	AddedAt time.Time
}

// savedTrackPage is one response from /v1/me/tracks.
type savedTrackPage struct {
	Items []struct {
		AddedAt time.Time `json:"added_at"`
		Track   Track     `json:"track"`
	} `json:"items"`
	Next string `json:"next"`
}

// savedAlbumPage is one response from /v1/me/albums.
type savedAlbumPage struct {
	Items []struct {
		AddedAt time.Time `json:"added_at"`
		Album   Album     `json:"album"`
	} `json:"items"`
	Next string `json:"next"`
}

// followedArtistPage is one response from /v1/me/following.
//
// Note the extra nesting: unlike every other paged endpoint Encore reads, this
// one wraps its page in an object named for the type being followed.
type followedArtistPage struct {
	Artists struct {
		Items   []Artist `json:"items"`
		Next    string   `json:"next"`
		Cursors struct {
			After string `json:"after"`
		} `json:"cursors"`
	} `json:"artists"`
}

// pageBudget clamps a caller's page limit.
func pageBudget(maxPages int) int {
	if maxPages <= 0 {
		return defaultLibraryPages
	}
	return maxPages
}

// SavedTracks reads every track in the listener's library.
//
// Offset paginated. Background rather than interactive: this runs on a worker
// tick, so it queues behind the catalogue budget rather than competing with
// somebody signing in.
func (c *Client) SavedTracks(ctx context.Context, accessToken string, maxPages int) ([]SavedTrack, error) {
	var out []SavedTrack
	for page := range pageBudget(maxPages) {
		q := url.Values{}
		q.Set("limit", strconv.Itoa(maxLibraryPageSize))
		q.Set("offset", strconv.Itoa(page*maxLibraryPageSize))

		var p savedTrackPage
		if err := c.get(ctx, "/v1/me/tracks", "get saved tracks", q, accessToken, &p); err != nil {
			return nil, fmt.Errorf("spotify: saved tracks: %w", err)
		}
		for _, item := range p.Items {
			out = append(out, SavedTrack{Track: item.Track, AddedAt: item.AddedAt})
		}
		if len(p.Items) == 0 || strings.TrimSpace(p.Next) == "" {
			break
		}
	}
	return out, nil
}

// SavedAlbums reads every album in the listener's library. Offset paginated,
// exactly as SavedTracks.
func (c *Client) SavedAlbums(ctx context.Context, accessToken string, maxPages int) ([]SavedAlbum, error) {
	var out []SavedAlbum
	for page := range pageBudget(maxPages) {
		q := url.Values{}
		q.Set("limit", strconv.Itoa(maxLibraryPageSize))
		q.Set("offset", strconv.Itoa(page*maxLibraryPageSize))

		var p savedAlbumPage
		if err := c.get(ctx, "/v1/me/albums", "get saved albums", q, accessToken, &p); err != nil {
			return nil, fmt.Errorf("spotify: saved albums: %w", err)
		}
		for _, item := range p.Items {
			out = append(out, SavedAlbum{Album: item.Album, AddedAt: item.AddedAt})
		}
		if len(p.Items) == 0 || strings.TrimSpace(p.Next) == "" {
			break
		}
	}
	return out, nil
}

// FollowedArtists reads every artist the listener follows.
//
// Cursor paginated rather than offset, and nested under an "artists" object —
// the only endpoint Encore reads that does either. The repeat-cursor guard is
// the same one RecentlyPlayed carries: a cursor that comes back round would
// page for ever, and the page budget alone would spend the whole allowance
// discovering it.
func (c *Client) FollowedArtists(ctx context.Context, accessToken string, maxPages int) ([]Artist, error) {
	var out []Artist
	seen := make(map[string]struct{}, pageBudget(maxPages))
	cursor := ""

	for range pageBudget(maxPages) {
		q := url.Values{}
		q.Set("type", "artist")
		q.Set("limit", strconv.Itoa(maxLibraryPageSize))
		if cursor != "" {
			q.Set("after", cursor)
		}

		var p followedArtistPage
		if err := c.get(ctx, "/v1/me/following", "get followed artists", q, accessToken, &p); err != nil {
			return nil, fmt.Errorf("spotify: followed artists: %w", err)
		}
		out = append(out, p.Artists.Items...)

		next := strings.TrimSpace(p.Artists.Cursors.After)
		if len(p.Artists.Items) == 0 || next == "" || next == cursor {
			break
		}
		if _, repeat := seen[next]; repeat {
			break
		}
		seen[next] = struct{}{}
		cursor = next
	}
	return out, nil
}
```

**Check `for page := range pageBudget(maxPages)` compiles** — Go 1.22+ range-over-int. `internal/spotify/recentlyplayed.go:74` already uses `for range maxPages`, so the idiom is established in this file's neighbour; match whichever form it uses.

- [ ] **Step 4: Run, lint, commit**

Run: `go test -count=1 ./internal/spotify/` — expected PASS.
Run: `go test -count=1 ./...` — expected PASS.
Lint: `export PATH="$PATH:$(go env GOPATH)/bin"` then `gofmt -l $(git ls-files '*.go')`, `go vet ./...`, `staticcheck ./...`.

```bash
git add internal/spotify/library.go internal/spotify/library_test.go
git commit -m "Spotify: read the saved library and followed artists

Three endpoints, two pagination shapes. Saved tracks and albums are offset
paginated; followed artists is cursor paginated and nests its page under an
'artists' object, the only endpoint Encore reads that does either.

The repeat-cursor guard is the one RecentlyPlayed already carries. A cursor
that comes back round would page for ever, and the page budget alone would
spend the whole allowance discovering that.

All three are background calls. They run on a worker tick, so they queue
behind the catalogue budget rather than competing with somebody signing in.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
```

---

### Task 3: The reconciliation store

**Files:**
- Create: `internal/store/library/library.go`
- Test: `test/integration/library_test.go`

**Interfaces:**
- Consumes: `store.Querier`, `store.UUIDArg`, `postgres.Classify`. The repository shape in `internal/store/accounts/playlists.go` is the model — a struct holding `*store.Store`, methods taking an explicit `Querier` so the same code runs inside and outside a transaction.
- Produces:
  - `type Repo struct{ db *store.Store }`, `func New(db *store.Store) *Repo`
  - `func (r *Repo) ReplaceSavedTracks(ctx context.Context, q store.Querier, userID uuid.UUID, items []SavedItem) error`
  - `func (r *Repo) ReplaceSavedAlbums(ctx context.Context, q store.Querier, userID uuid.UUID, items []SavedItem) error`
  - `func (r *Repo) ReplaceFollowedArtists(ctx context.Context, q store.Querier, userID uuid.UUID, ids []string) error`
  - `type SavedItem struct { ID string; AddedAt *time.Time }`
  - `type Counts struct { SavedTracks, SavedAlbums, FollowedArtists int64 }`
  - `func (r *Repo) Counts(ctx context.Context, q store.Querier, userID uuid.UUID) (Counts, error)`

**Each `Replace*` is delete-absent plus upsert-present.** It takes a `Querier` rather than opening its own transaction, because the worker calls all three plus the watermark update inside one `Store.InTx` — that is what makes a partial run commit nothing.

- [ ] **Step 1: Write the failing integration tests**

Create `test/integration/library_test.go`, using `harness.New(t)` and `env.NewUser(...)`:

- Replacing an empty set with three ids inserts three.
- **Replacing three ids with two deletes the absent one** — the whole point of reconciliation.
- Replacing with the same set twice is idempotent and leaves `added_at` unchanged.
- Replacing with an empty slice removes everything — an emptied library is a real state and must not be mistaken for "no data".
- **A failure inside the transaction commits nothing**: inside one `Store.InTx`, call `ReplaceSavedTracks` successfully, then return an error from the callback; assert afterwards that the table is unchanged.
- `Counts` returns zeroes for a user with no rows, and the right numbers otherwise.
- Deleting a user cascades all three tables away.
- Two users' libraries do not interfere.

- [ ] **Step 2: Run to verify they fail; Step 3: implement; Step 4: run, lint, commit**

For the implementation, prefer one statement per table using `unnest` for the incoming set — the codebase already passes arrays this way (see `internal/store/catalog/artists.go:94`, which builds a row set from parallel arrays). A delete of `NOT IN (incoming)` followed by an upsert is also acceptable; what matters is that both happen under the caller's `Querier`.

Commit message should explain why reconciliation is delete-plus-upsert rather than truncate-and-insert: the latter would briefly empty a library that a concurrent reader could observe.

---

### Task 4: Configuration

**Files:**
- Modify: `internal/config/config.go`
- Test: `internal/config/config_test.go`

**Interfaces:**
- Produces: `type Library struct { Enabled bool; Interval time.Duration; Concurrency int; MaxPages int }` and a `Library` field on `Config`.

Model it on `type Sync struct` at `internal/config/config.go:138-145`. The parser helpers you need all exist and are verified: `p.boolean(key, def)` at `config.go:573`, `p.intRange(key, def, min, max)` at `config.go:586`, and `p.duration(key, def)` at `config.go:616` (which accepts a bare integer as seconds). Use `intRange` for `Concurrency` and `MaxPages` so an absurd value is a config error rather than a runtime surprise.

**Defaults: enabled, 24h, concurrency 2, maxPages 200.**

Justify the default-on in the doc comment: unlike the Phase 3 now-playing poller, this costs roughly `ceil(saved_tracks/50) + ceil(saved_albums/50) + ceil(followed/50)` requests per account **per day** — a few hundred for a large library, against a quota a single import can exhaust. That is affordable, and a feature that defaults off is a feature most instances never see. `ENCORE_LIBRARY_SYNC_ENABLED=false` turns it off.

- [ ] **Steps:** failing test asserting the four defaults and that a non-positive interval is a config error; implement; `go test ./internal/config/`; lint; commit.

---

### Task 5: The worker loop

**Files:**
- Create: `internal/library/library.go`, `internal/library/library_test.go`
- Modify: `cmd/encore-worker/main.go`, and the credentials repository for a due-list query

**Interfaces:**
- Consumes: Task 2's client methods, Task 3's `Repo`, Task 4's config, `accounts.Credentials`, `catalog` for minting, `store.Store.InTx`.
- Produces: `func New(cfg config.Library, deps Deps) (*Worker, error)`, `func (w *Worker) Run(ctx context.Context) error`, `func (w *Worker) RunOnce(ctx context.Context) (int, error)`.

**Follow `internal/sync/poller.go` closely** — same shape, and reading it will answer most questions this plan does not:
- `Run` returns nil immediately with one log line when `!cfg.Enabled` (`poller.go:38-42`).
- The first delay is drawn from the *whole* interval, not jittered around it, so a fleet that started together spreads out (`poller.go:48-51`).
- `RunOnce` lists due accounts and processes them. `ListDueForSync` at `poller.go:80` is the model for the `ListDueForLibrarySync` you will add, keyed on `library_synced_at` with NULLs first so a newly connected account is enumerated promptly.

**Per account, in this order:**

1. **Check the stored scopes first.** If the grant lacks `user-library-read` or `user-follow-read`, skip **without issuing a request**. `domain.SpotifyCredentials.HasScope` at `internal/domain/user.go:111` does this. Discovering it by 403 would waste a request per account per day, for ever, on every account that predates Phase 2a.
2. Enumerate all three via the client.
3. Mint catalogue rows for ids not already present, as `pending`, so enrichment resolves them. **Reuse the catalogue repository — do not reimplement minting.** Find how the importer or sync ingest does it and follow that.
4. In **one** `Store.InTx`: all three `Replace*` calls, then set `library_synced_at`. The watermark must not advance unless the data landed.
5. **On a 403 despite the scope check:** log at warn with the endpoint label; do **not** retry; do **not** touch `SyncState`; leave `library_synced_at` unchanged so the account reads as never-synced rather than stale. See the `forbidden` comment at `internal/sync/account.go:296`.

- [ ] **Step 1: Write the failing tests**

`internal/library/library_test.go`, with a fake client following `internal/sync/sync_test.go:27`'s `fakeSpotify`:
- An account whose grant lacks `user-library-read` is skipped and **the fake client records zero calls**.
- The happy path writes all three tables and advances `library_synced_at`.
- A 403 from any of the three leaves `library_synced_at` NULL **and** `SyncState` untouched, and does not retry.
- An enumeration error part-way leaves the previous contents intact.
- `Run` with `Enabled: false` returns nil without issuing a request.

If driving the full path needs more scaffolding than the package provides, **say so and pin what you can** — the same judgement Phase 2a's Task 3 made. Do not build a large new harness.

- [ ] **Step 2–5:** run to verify failure; implement; register in `cmd/encore-worker/main.go` beside `sup.Add("sync", poller.Run)` (around line 205), following how `enrich` and `sync` are constructed and added; run the full suites; lint; commit.

- [ ] **Step 6: Full verification — the plan's final evidence**

```
export PATH="$PATH:$(go env GOPATH)/bin"
gofmt -l $(git ls-files '*.go')
go vet ./...
staticcheck ./...
go test -count=1 ./...
ENCORE_TEST_DATABASE_URL="postgres://encore:encore@localhost:5433/encore?sslmode=disable" go test -tags=integration -count=1 -p 1 -timeout=20m ./test/...
```

**Report real output. Do not claim a pass on a command you did not run.**

---

## Definition of done

- [ ] `gofmt -l`, `go vet`, `staticcheck` clean; `go test ./...` and the full integration suite pass.
- [ ] Migration applies, rolls back and re-applies cleanly.
- [ ] An account without the scopes is skipped with **zero** Spotify requests.
- [ ] A 403 leaves `SyncState` and `library_synced_at` untouched and is not retried.
- [ ] A reconciliation that fails part-way commits nothing, and the watermark does not advance.
- [ ] Replacing a library with a smaller set deletes the absent rows.
- [ ] Nothing under `web/` was touched; no page load can trigger enumeration.
- [ ] `go.mod` unchanged.
