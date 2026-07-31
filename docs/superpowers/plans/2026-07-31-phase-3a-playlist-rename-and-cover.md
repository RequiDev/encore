# Phase 3a — Playlist Rename and Generated Cover Art

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let a listener rename a playlist Encore made, give it a description that explains itself inside Spotify, and put a generated cover on it instead of Spotify's grey placeholder.

**Architecture:** Two new Spotify writes (`PUT /v1/playlists/{id}` and `PUT /v1/playlists/{id}/images`) behind the existing playlist-creation consent moment, which now asks for `ugc-image-upload` alongside `playlist-modify-private`. A new pure package `internal/playlistcover` renders a 640×640 JPEG — a 2×2 mosaic of album art with the name over a scrim — and fetches that art through an SSRF allowlist. Cover generation is structurally best-effort: it returns a state to record, never an error to propagate, so it can never fail the playlist operation that asked for it. Four new columns on `playlists` hold that state so the interface can say "the tracks are right, the picture is not" and offer a retry.

**Tech Stack:** Go 1.26, pgx/v5, PostgreSQL 17, `golang.org/x/image` (the one new module dependency), React 19 + TypeScript + Vite + TanStack Query v5.

**Spec:** [`docs/design/2026-07-29-phase-3-write-and-live-design.md`](../../design/2026-07-29-phase-3-write-and-live-design.md) **§1 only**. §2 (the now-playing poller) is a separate plan — see "Why this is one of two plans" below.

**Task count: 8.**

---

## Why this is one of two plans

Phase 3 holds two features that share **no code, no table, no config key, no scope, no endpoint prefix and no risk profile**:

| | Feature 7 — rename and cover | Feature 8 — now playing |
|---|---|---|
| Gate | Per-user scope, at point of use | Instance-wide config key, unset means off |
| New scope | `ugc-image-upload` | none — `user-read-playback-state` shipped in Phase 2a |
| Trigger | A button press, inside an HTTP handler | A repeating background loop |
| Defining risk | The project's first writes to a real Spotify account | A repeating request against a shared, instance-wide limiter |
| New machinery | An image encoder, a CDN fetcher | A poll loop, a TTL table, a fuzzy temporal join |
| Config keys | **none** (see below) | `ENCORE_NOWPLAYING_INTERVAL`, in five places |
| Files touched in common | `migrations/` (a number), `docs/api.md` (a section) | — |

Combined they are 14–15 tasks. Phase 2 was split five ways and every split improved it. **This plan is feature 7 only.** Feature 8 is described in a paragraph at the end and gets its own plan and its own merge.

**Migration numbering:** this plan takes `00015_`. Feature 8's plan takes `00016_` if this merges first, and swaps if it does not. Whichever executes second re-checks `ls migrations/` before writing its file.

---

## The property that defines this whole plan

**These are the project's first Spotify writes.** Everything Encore has done until now reads: a bad read wastes quota and a stale cache re-fetches. A bad write lands in a listener's own Spotify account, where there is nothing to re-fetch and no undo.

Three rules follow, and every task is built on them:

1. **Spotify first, Encore second, and never the other way round.** The listener's Spotify account is the authority on what their playlist is called. Encore's row is a copy of that fact, written only after Spotify has confirmed it. A local row that ran ahead would make Encore's copy a claim about somebody else's account that nobody had checked.

2. **No message claims a state Encore has not confirmed — in either direction.** "Renamed" is only ever said after a 2xx. "Nothing has changed" is only ever said when Encore knows nothing changed. A transport error, where the request may or may not have reached Spotify, gets its own sentence saying exactly that; it must not be flattened into either of the other two. Claiming an unconfirmed *failure* is the same defect as claiming an unconfirmed success, and it is the easier one to write by accident.

3. **The Spotify playlist id is never accepted from the client.** It is read from `playlists.Get(ctx, q, userID, id)`, which is scoped by owner. No request body can widen what a write addresses.

---

## Decisions taken here, so they are not relitigated mid-execution

### No configuration key, and why

`internal/config/config.go`'s comment on `AlbumTracks.Enabled` draws the line already:

> It has a switch at all because, unlike signing in, "sync now" and **playlist creation** — each an action a person took — this fires as a side effect of *viewing a page*. Unattended egress is an operator's decision.

Cover generation fires from a button press, on the named side of that line. `go.mod` and `go.sum` gain `golang.org/x/image`; `web/package.json` gains nothing; `.env.example`, `docker-compose.yml`, `docs/configuration.md` and `docker-compose.portainer.yml` gain **nothing**, and `scripts/gen-portainer-stack.sh` does not need to be run.

**The counter-argument, considered and rejected:** the cover fetches from `i.scdn.co`, which is outbound HTTP an operator might want to forbid. It is rejected because the guard that matters is the host allowlist (Task 6), which is unconditional and cannot be misconfigured, and because a switch would fail *open* in the worst way — an operator who set it would keep a feature that silently produces a "cover not generated" state for ever with no explanation on the row. If a switch is wanted later it is `ENCORE_PLAYLIST_COVERS_ENABLED`, added as its own task with all five places done at once.

**Do not add a config key. If you find yourself adding one, stop and re-read this section.**

### The spec contradicts itself about the fallback, and this is the resolution

`docs/design/2026-07-29-phase-3-write-and-live-design.md` §1.2 says *"Fewer than four usable covers … falls back to a deterministic geometric cover."* §3's test table says *"One unreachable art URL yields a three-tile cover, not an error."* Both cannot hold: three is fewer than four.

**Resolution, and it is the reading that makes both statements meaningful:**

- **Zero usable images → the pattern cover.** `Kind = pattern`, `Covered = 0`. This is the fresh-instance case §1.2 is actually describing, and it is the case the determinism test exercises.
- **One to four usable images → a mosaic, with every empty cell filled from that same pattern.** `Kind = mosaic`, `Covered = n`. This is the lost-tile case §3 is describing, and the cover still shows the artwork that was found rather than discarding it.

Both report `Covered` honestly and the interface states it in words. Record this resolution in `internal/playlistcover`'s package comment so the next reader does not re-derive it from a contradiction.

### The mosaic's denominator is always 4

`Covered` is out of **4**, always — not out of the number of distinct albums the playlist happens to contain. "Built from 2 of 4 album covers" is the honest report of a grid that asked for four and got two. "2 of 2" would describe a full mosaic that was never built. This also fixes the plural: the noun agrees with 4, so there is no singular form of the sentence to get wrong.

### The reconsent banner must not learn about `ugc-image-upload`

`web/src/components/layout/ReconsentBanner.tsx` is driven by `spotify.missingScopes`, which `internal/httpapi/me.go:76` computes as `spotify.MissingScopes(creds.Scopes, config.DefaultScopes())`. `ugc-image-upload` is **not** in `DefaultScopes()` and must not be added to it, so it can never appear in `missingScopes` and the banner can never show it.

That is deliberate, and it protects a sentence: the banner closes with

> None of these let Encore change anything on your Spotify account.

Adding a write scope to `SCOPE_EXPLANATIONS` would make that sentence false the first time it rendered. **Do not add `ugc-image-upload` to `SCOPE_EXPLANATIONS`, to `DefaultScopes()`, or to `ReconsentBanner.tsx` in any form.** The consent surface for this scope is the existing `/api/auth/spotify/playlists` link on the Settings page, and nowhere else.

---

## Global Constraints

- **No new npm dependency.** `web/package.json` is unchanged at the end.
- **Exactly one new Go module dependency: `golang.org/x/image`.** It is **not in the local module cache** — verify network access with step 1 of Task 5 before starting that task. Nothing else may be added; `go mod tidy` output is diffed by CI's `lint` job.
- **No new configuration key.** See the decision above.
- **`user-read-playback-state` already shipped** in `DefaultScopes()` (`internal/config/config.go:560-561`). `TestDefaultScopesAreTheEightReadScopes` and `TestDefaultScopesGrantNoWriteAccess` in `internal/config/config_test.go` pin the eight. **Do not change `DefaultScopes()`.**
- **A 403 is not a broken grant.** Read `internal/sync/account.go:296`. `ugc-image-upload` is an optional write scope in exactly that sense: a 403 from the image endpoint must never reach `markNeedsReauth` and must never be retried. `Client.classify` already wraps every non-429 4xx in `retry.Stop`, so the no-retry half is structural; the no-reauth half is yours to keep.
- **Every write in this plan is `interactive: true`.** That is deliberate and load-bearing — see "The shared limiter" below.
- **Anything reaching a `text` column goes through `store.Truncate`** (`internal/store/store.go:193`, rune-safe, appends `...`). In this plan that is exactly one column: `playlists.cover_error`.
- **Never send a zero-length or partially encoded image.** This is the delete-absent rule wearing a different hat: a cover upload *replaces* whatever cover the playlist has, so a short buffer reaching `PUT /images` is a partial input reaching a replace. Guarded at two layers (Tasks 3 and 5).
- **`internal/httpapi` contains no SQL and never imports pgx.** It reaches repositories through the narrow interfaces in `server.go`; `playlistStore` (`server.go:128-134`) must gain every new method or the concrete repo stops satisfying it.
- **The DTO exists in three places and they are kept in step by hand:** `internal/httpapi/dto.go`, `web/src/lib/types.ts`, `docs/api.md`. Change one, change all three.
- Next migration number is **`00015_`**. House style is goose `Up` **and** `Down`, both directions working, reasoning in comments including what was considered and rejected.
- Test DB on port **5433**, not 5432. `make` is **NOT installed** — run the commands directly.
- `go test -race` will **NOT** work locally: no gcc. Omit `-race`. CI runs it, on `pull_request` and `workflow_dispatch` only — **not on branch pushes**, so a race is invisible until the PR opens.
- Tagged suites share one database: `-p 1`, one package at a time.
- staticcheck at `$(go env GOPATH)/bin`; `export PATH="$PATH:$(go env GOPATH)/bin"` first.
- **CI has eight jobs, not nine** (`lint`, `unit`, `migrations`, `integration`, `benchmark`, `web`, `images` ×2 matrix, `smoke`) — the ninth is the `images` matrix counted twice.
- **CI does not run `npm run test`.** `.github/workflows/ci.yml`'s `web` job runs `typecheck`, `lint`, `build` and stops. Every copy assertion in this plan is therefore *unguarded by CI* until Task 8 adds the step. The suite is green today: 17 files, 198 tests, verified at `7292614`.
- **`vi.spyOn(window.sessionStorage, …)` silently does nothing** in this project's jsdom. Spy on `Storage.prototype` instead. (No task here needs it; it is recorded so nobody rediscovers it.)
- **NUL check every file you write:** `perl -0777 -ne 'print "NULs: ", tr/\0//, "\n"' <file>` — expect 0.
- Commit style `Area: lowercase summary`, body explaining *why*, ending `Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>`. Stage paths explicitly; never `git commit -a`.
- **Every test in this plan carries a "Fails when:" line** naming the exact change that breaks it. If you add a test of your own and cannot write that line, the test cannot fail and must be replaced.

---

## The shared limiter, and why this feature cannot pause the instance

`internal/spotify/client.go:273` — `budget()` — sends `interactive: true` requests to `c.signin`, a limiter that is **never shared and never restored from a recorded pause**. `classify` (line 388) then does two things that matter here:

```go
resumeAt := now.Add(retryAfter)
limiter, _ := c.budget(r)
limiter.Pause(resumeAt)
if c.onPause != nil && !r.interactive {
    c.onPause(resumeAt)
}
```

So a 429 on any write in this plan pauses **the sign-in budget only**. It does not call `onPause`, so it never reaches `app_settings.spotify_paused_until`, never 409s "sync now" for every user on the instance, never stops the enrichment worker and never stops the five library enumerations. It also returns `retry.Stop(&PausedError{})` immediately rather than sleeping through a retry the listener did not ask for.

**The trade-off, stated rather than discovered:** a burst of cover uploads can pause *sign-in* for the window Spotify asks for. That is accepted, because the alternative is worse in both directions — putting a button press on the catalogue budget means a large import's 429 makes somebody's rename hang for most of a day, and it means the rename's own 429 pauses enrichment. A cover upload is one request, bounded by `PlaylistLimitPerUser = 50` and by a human pressing a button.

**The CDN fetch does not touch either limiter.** `i.scdn.co` is not the Web API, spends no quota, and goes through `internal/playlistcover`'s own `http.Client`. Do not route it through `spotify.Client`.

---

## File Structure

| File | Responsibility |
|---|---|
| `migrations/00015_playlist_cover.sql` | **Create.** Four columns on `playlists`. |
| `internal/domain/playlist.go` | **Modify.** `CoverState`, `PlaylistCover`, `Describe`. |
| `internal/store/accounts/playlists.go` | **Modify.** `Rename`, `SetCover`, wider `playlistColumns`. |
| `internal/spotify/client.go` | **Modify.** A raw-body field on `request`. |
| `internal/spotify/playlists.go` | **Modify.** `ScopeImageUpload`, `UpdatePlaylistDetails`, `SetPlaylistCover`. |
| `internal/playlistcover/cover.go` | **Create.** The renderer: mosaic, scrim, name, quality ladder. |
| `internal/playlistcover/pattern.go` | **Create.** The deterministic fallback. |
| `internal/playlistcover/fetch.go` | **Create.** The CDN fetcher and its allowlist. |
| `internal/stats/playlist.go` | **Modify.** `CoverArtURLs`. |
| `internal/httpapi/auth.go` | **Modify.** Ask for both write scopes at once. |
| `internal/httpapi/playlists.go` | **Modify.** Rename, cover orchestration, cover retry. |
| `internal/httpapi/dto.go`, `router.go`, `server.go` | **Modify.** DTO, two routes, two interface methods. |
| `web/src/lib/types.ts`, `web/src/pages/Settings.tsx` | **Modify.** |
| `web/src/test/playlist-cover.test.tsx` | **Create.** |
| `docs/api.md`, `docs/security.md`, `docs/feature-parity.md`, `README.md` | **Modify.** |
| `.github/workflows/ci.yml` | **Modify.** Run the web suite. |

---

## Task 1: The cover state, on disk and in the domain

**Files:**
- Create: `migrations/00015_playlist_cover.sql`
- Modify: `internal/domain/playlist.go`
- Modify: `internal/store/accounts/playlists.go`
- Modify: `internal/httpapi/server.go` (`playlistStore`)
- Test: `test/integration/playlistcover_test.go` (create)

**Interfaces produced:**
- `domain.CoverState` with `CoverNone`, `CoverReady`, `CoverFailed`, `CoverUnauthorised`
- `const domain.CoverTileTotal = 4`
- `domain.PlaylistCover{State CoverState; Tiles int; Error string; At time.Time}` and `func (PlaylistCover) Mosaic() bool`
- `domain.Playlist.Cover PlaylistCover`
- `func (r *Playlists) Rename(ctx context.Context, q store.Querier, userID, id uuid.UUID, name string) (domain.Playlist, error)`
- `func (r *Playlists) SetCover(ctx context.Context, q store.Querier, userID, id uuid.UUID, cover domain.PlaylistCover) error`

`playlists` is **not** in `test/harness/harness.go`'s `truncatedTables` — it cascades from `users`. No harness change is needed. Verify that before assuming it.

- [ ] **Step 1: Write the migration**

```sql
-- +goose Up

-- What happened the last time Encore tried to give this playlist a cover.
--
-- Cover generation is best-effort and can never fail the playlist operation
-- that triggered it: a playlist that exists with Spotify's grey placeholder is
-- a far better outcome than a create that reports failure because a CDN was
-- slow. So the outcome has to live somewhere durable, or the interface has no
-- way to say "the tracks are right, the picture is not" and no way to offer a
-- retry.
--
-- Four states, and the fourth is why this is not a boolean:
--
--   none          -> never attempted; every row that predates this migration
--   ready         -> Spotify accepted an uploaded cover
--   failed        -> an attempt was made and did not finish; cover_error says
--                    why, in words aimed at the listener
--   unauthorised  -> the account has not granted ugc-image-upload
--
-- 'unauthorised' is kept apart from 'failed' because the fix differs: one is a
-- retry, the other is a trip through Spotify's consent screen. Rendering the
-- second as the first tells somebody to press a button that cannot work.
--
-- A CHECK constraint is right here, unlike artist_albums.album_group in 00014
-- where one was rejected at length. That column holds a value *Spotify* mints
-- and could extend without warning, so a CHECK would turn a new Spotify group
-- into a permanent write failure. These four are minted by Encore's own code
-- from a closed Go enum, so a value outside the set is a bug in this
-- repository and failing the write is how it gets found.
ALTER TABLE playlists ADD COLUMN cover_state text NOT NULL DEFAULT 'none'
    CHECK (cover_state IN ('none', 'ready', 'failed', 'unauthorised'));

-- How many of the mosaic's four tiles came from real album artwork.
--
-- The denominator is always 4 -- the grid wants four tiles however many
-- distinct albums the playlist happens to contain -- so this is the numerator
-- of the coverage figure the playlist row states in words. 0 means the cover
-- is the generated pattern rather than a mosaic; the interface derives that
-- from this column rather than storing a second one that could disagree.
ALTER TABLE playlists ADD COLUMN cover_tiles integer NOT NULL DEFAULT 0
    CHECK (cover_tiles BETWEEN 0 AND 4);

-- Why the last attempt failed, in the listener's own terms. Empty in every
-- other state. Bounded at the call site by store.Truncate, rune-safely: this
-- string can carry a Spotify error body, and a byte-boundary cut through a
-- multi-byte rune would make the write that records the failure itself fail.
ALTER TABLE playlists ADD COLUMN cover_error text NOT NULL DEFAULT '';

-- When the state above was last written. NULL while cover_state is 'none',
-- which is distinct from "attempted at an unknown time" and stays distinct.
ALTER TABLE playlists ADD COLUMN cover_at timestamptz;

-- No index. Cover state is only ever read as part of a playlist row the
-- listener already asked for by (user_id) or (id, user_id), both of which
-- playlists_user_idx and the primary key already lead. Nothing asks "which
-- playlists have a failed cover" -- there is no sweep and no retry worker,
-- because a retry is a button.

-- +goose Down
ALTER TABLE playlists DROP COLUMN cover_at;
ALTER TABLE playlists DROP COLUMN cover_error;
ALTER TABLE playlists DROP COLUMN cover_tiles;
ALTER TABLE playlists DROP COLUMN cover_state;
```

- [ ] **Step 2: Run the migration cycle**

```bash
export ENCORE_DATABASE_URL="postgres://encore:encore@localhost:5433/encore?sslmode=disable"
go run ./cmd/encore-migrate up && go run ./cmd/encore-migrate status
go run ./cmd/encore-migrate up
go run ./cmd/encore-migrate reset --yes && go run ./cmd/encore-migrate up
```

All four must succeed. The reset/up cycle is what CI's `migrations` job runs.

- [ ] **Step 3: Add the domain types**

In `internal/domain/playlist.go`, after the `PlaylistSort` block:

```go
// CoverState is what happened the last time Encore tried to give a playlist a
// cover image.
type CoverState string

const (
	// CoverNone means no attempt has been made. Every playlist made before
	// covers existed is in this state, and stays in it until somebody asks.
	CoverNone CoverState = "none"
	// CoverReady means Spotify accepted an uploaded cover.
	CoverReady CoverState = "ready"
	// CoverFailed means an attempt was made and did not finish.
	CoverFailed CoverState = "failed"
	// CoverUnauthorised means the account has not granted ugc-image-upload.
	//
	// Deliberately not CoverFailed. The fix is a trip through Spotify's
	// consent screen, not a retry, and offering a retry button for it would
	// invite somebody to press a thing that cannot work.
	CoverUnauthorised CoverState = "unauthorised"
)

// CoverTileTotal is how many tiles the mosaic asks for, and so the denominator
// of every sentence about a cover's coverage.
//
// A constant rather than the number of distinct albums in the playlist: "built
// from 2 of 4 album covers" is the honest report of a grid that wanted four and
// got two, whereas "2 of 2" would describe a full mosaic that was never built.
const CoverTileTotal = 4

// PlaylistCover records the outcome of the last cover attempt.
type PlaylistCover struct {
	State CoverState
	// Tiles is how many of CoverTileTotal came from real album artwork. Zero
	// means the cover is the generated pattern.
	Tiles int
	// Error is why the last attempt failed, in the listener's own terms. Empty
	// in every state but CoverFailed.
	Error string
	// At is when State was last written. Zero while State is CoverNone.
	At time.Time
}

// Mosaic reports whether the stored cover is built from real artwork rather
// than being the generated pattern.
func (c PlaylistCover) Mosaic() bool { return c.State == CoverReady && c.Tiles > 0 }
```

And on `Playlist`, after `BuiltAt`:

```go
	// Cover is the outcome of the last attempt to give this playlist a picture.
	// It is not part of the definition: two playlists with the same recipe can
	// have different covers, because one of them was made when the catalogue
	// had less artwork in it.
	Cover PlaylistCover
```

- [ ] **Step 4: Write the failing store test**

Create `test/integration/playlistcover_test.go`:

```go
//go:build integration

package integration

import (
	"strings"
	"testing"
	"time"

	"github.com/RequiDev/encore/internal/domain"
	"github.com/RequiDev/encore/test/harness"
)

// newCoverPlaylist inserts one managed playlist and returns it.
func newCoverPlaylist(t *testing.T, e *harness.Env, userID any, name string) domain.Playlist {
	t.Helper()
	p, err := e.Accounts.Playlists.Create(e.Ctx(), e.Store.DB(), domain.Playlist{
		UserID:     userID.(uuidValue),
		Name:       name,
		SpotifyID:  "spotifyplaylist000001",
		SpotifyURL: "https://open.spotify.com/playlist/spotifyplaylist000001",
		Definition: domain.PlaylistDefinition{
			Mode: domain.PlaylistModeTop, Sort: domain.SortByPlays, Limit: 100,
		},
		TrackCount: 10,
		BuiltAt:    time.Date(2026, time.July, 31, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("create playlist: %v", err)
	}
	return p
}

// TestPlaylistCoverDefaultsToNone pins that a playlist made before covers
// existed reads back as "never attempted" rather than as a failure.
//
// Fails when: the migration's DEFAULT is dropped, or scanPlaylist reads the new
// columns in the wrong positional order (cover_tiles would land in cover_state
// and the scan errors).
func TestPlaylistCoverDefaultsToNone(t *testing.T) {
	e := harness.New(t)
	user := e.NewUser("cover-default")
	p := newCoverPlaylist(t, e, user.ID, "Heavy rotation")

	if p.Cover.State != domain.CoverNone {
		t.Fatalf("Cover.State = %q, want %q", p.Cover.State, domain.CoverNone)
	}
	if p.Cover.Tiles != 0 || p.Cover.Error != "" || !p.Cover.At.IsZero() {
		t.Fatalf("Cover = %+v, want a zero cover", p.Cover)
	}
}

// TestSetCoverRoundTrips pins that every field survives a write and a read, and
// that the state is scoped by owner.
//
// Fails when: SetCover drops the user_id predicate (the foreign user's write
// would succeed and RowsAffected would be 1), or playlistColumns and
// scanPlaylist disagree about column order.
func TestSetCoverRoundTrips(t *testing.T) {
	e := harness.New(t)
	user := e.NewUser("cover-roundtrip")
	other := e.NewUser("cover-stranger")
	p := newCoverPlaylist(t, e, user.ID, "Heavy rotation")

	at := time.Date(2026, time.July, 31, 9, 30, 0, 0, time.UTC)
	want := domain.PlaylistCover{State: domain.CoverReady, Tiles: 3, At: at}
	if err := e.Accounts.Playlists.SetCover(e.Ctx(), e.Store.DB(), user.ID, p.ID, want); err != nil {
		t.Fatalf("SetCover: %v", err)
	}

	got, err := e.Accounts.Playlists.Get(e.Ctx(), e.Store.DB(), user.ID, p.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Cover.State != domain.CoverReady || got.Cover.Tiles != 3 {
		t.Fatalf("Cover = %+v, want state ready and 3 tiles", got.Cover)
	}
	if !got.Cover.At.Equal(at) {
		t.Fatalf("Cover.At = %v, want %v", got.Cover.At, at)
	}
	if !got.Cover.Mosaic() {
		t.Fatal("Mosaic() = false for a ready cover with three tiles")
	}

	// Another account cannot write to it.
	err = e.Accounts.Playlists.SetCover(e.Ctx(), e.Store.DB(), other.ID, p.ID,
		domain.PlaylistCover{State: domain.CoverFailed, At: at})
	if err == nil {
		t.Fatal("SetCover: a stranger's write succeeded")
	}
}

// TestSetCoverTruncatesTheReason pins the rune-safe cut on the one text column
// this feature writes.
//
// Fails when: store.Truncate is replaced by a plain slice — the multi-byte
// runes below then cut mid-rune and PostgreSQL rejects the write outright, so
// SetCover returns an error instead of storing a bounded string.
func TestSetCoverTruncatesTheReason(t *testing.T) {
	e := harness.New(t)
	user := e.NewUser("cover-truncate")
	p := newCoverPlaylist(t, e, user.ID, "Heavy rotation")

	// 400 three-byte runes: past the limit, and every boundary is a trap.
	reason := strings.Repeat("é—中", 200)
	err := e.Accounts.Playlists.SetCover(e.Ctx(), e.Store.DB(), user.ID, p.ID,
		domain.PlaylistCover{State: domain.CoverFailed, Error: reason, At: time.Now().UTC()})
	if err != nil {
		t.Fatalf("SetCover: %v", err)
	}

	got, err := e.Accounts.Playlists.Get(e.Ctx(), e.Store.DB(), user.ID, p.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(got.Cover.Error) > 256 {
		t.Fatalf("stored reason is %d bytes, want it bounded", len(got.Cover.Error))
	}
	if !utf8ValidString(got.Cover.Error) {
		t.Fatal("stored reason is not valid UTF-8")
	}
}
```

Replace `uuidValue` with `uuid.UUID` and add the import; add a tiny `utf8ValidString` wrapper around `utf8.ValidString` or use it directly. Follow the file conventions of the integration suite you find, not these placeholders.

- [ ] **Step 5: Run it and watch it fail**

```bash
export ENCORE_TEST_DATABASE_URL="postgres://encore:encore@localhost:5433/encore?sslmode=disable"
go test -tags=integration -count=1 -run 'TestPlaylistCover|TestSetCover' ./test/integration/
```

Expected: FAIL — `p.Cover` undefined, `SetCover` undefined.

- [ ] **Step 6: Widen the store**

In `internal/store/accounts/playlists.go`:

```go
const playlistColumns = `id, user_id, name, spotify_id, spotify_url,
                         mode, sort, track_limit, min_plays, range_from, range_to,
                         track_count, built_at, created_at,
                         cover_state, cover_tiles, cover_error, cover_at`

// coverErrorLimit bounds what reaches playlists.cover_error. Long enough for a
// sentence a listener reads, short enough that a Spotify error body cannot fill
// the column.
const coverErrorLimit = 200

// Rename records a name Spotify has already accepted.
//
// Called only after PUT /v1/playlists/{id} has returned 2xx, never before. The
// listener's Spotify account is the authority on what their playlist is
// called; a local row that ran ahead of it would make every screen in Encore
// a claim about somebody else's account that nobody had confirmed.
func (r *Playlists) Rename(
	ctx context.Context, q store.Querier, userID, id uuid.UUID, name string,
) (domain.Playlist, error) {
	row := q.QueryRow(ctx,
		`UPDATE playlists SET name = $3 WHERE id = $1 AND user_id = $2
         RETURNING `+playlistColumns, id, userID, name)

	p, err := scanPlaylist(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Playlist{}, fmt.Errorf("%w: no such playlist", domain.ErrNotFound)
	}
	if err != nil {
		return domain.Playlist{}, postgres.Classify("rename playlist", err)
	}
	return p, nil
}

// SetCover records the outcome of a cover attempt.
//
// Scoped by owner like every other write here, and the reason is not
// hypothetical: this is reached from a handler that has already resolved the
// playlist, and a second predicate costs nothing to keep the invariant true at
// the only layer that can enforce it.
func (r *Playlists) SetCover(
	ctx context.Context, q store.Querier, userID, id uuid.UUID, cover domain.PlaylistCover,
) error {
	tag, err := q.Exec(ctx,
		`UPDATE playlists
            SET cover_state = $3, cover_tiles = $4, cover_error = $5, cover_at = $6
          WHERE id = $1 AND user_id = $2`,
		id, userID, string(cover.State), cover.Tiles,
		store.Truncate(cover.Error, coverErrorLimit), nullTime(cover.At))
	if err != nil {
		return postgres.Classify("record playlist cover", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: no such playlist", domain.ErrNotFound)
	}
	return nil
}
```

And `scanPlaylist` gains four reads, in `playlistColumns` order:

```go
func scanPlaylist(row rowScanner) (domain.Playlist, error) {
	var (
		p                          domain.Playlist
		mode, sortBy, coverState   string
		from, to, builtAt, coverAt *time.Time
	)
	if err := row.Scan(&p.ID, &p.UserID, &p.Name, &p.SpotifyID, &p.SpotifyURL,
		&mode, &sortBy, &p.Definition.Limit, &p.Definition.MinPlays,
		&from, &to, &p.TrackCount, &builtAt, &p.CreatedAt,
		&coverState, &p.Cover.Tiles, &p.Cover.Error, &coverAt); err != nil {
		return domain.Playlist{}, err
	}
	p.Definition.Mode = domain.PlaylistMode(mode)
	p.Definition.Sort = domain.PlaylistSort(sortBy)
	p.Cover.State = domain.CoverState(coverState)
	if from != nil {
		p.Definition.From = from.UTC()
	}
	if to != nil {
		p.Definition.To = to.UTC()
	}
	if builtAt != nil {
		p.BuiltAt = builtAt.UTC()
	}
	if coverAt != nil {
		p.Cover.At = coverAt.UTC()
	}
	return p, nil
}
```

`Create`'s `INSERT` list is **unchanged** — the four columns take their defaults — but its `RETURNING playlistColumns` now returns them, which is exactly what makes the defaults observable.

- [ ] **Step 7: Extend the narrow interface**

`internal/httpapi/server.go`, on `playlistStore`:

```go
	Rename(ctx context.Context, q store.Querier, userID, id uuid.UUID, name string) (domain.Playlist, error)
	SetCover(ctx context.Context, q store.Querier, userID, id uuid.UUID, cover domain.PlaylistCover) error
```

- [ ] **Step 8: Run everything**

```bash
export PATH="$PATH:$(go env GOPATH)/bin"
gofmt -l $(git ls-files '*.go'); go vet ./...; staticcheck ./...
go test -count=1 ./...
ENCORE_TEST_DATABASE_URL="postgres://encore:encore@localhost:5433/encore?sslmode=disable" \
  go test -tags=integration -count=1 -p 1 -timeout=20m ./test/integration/
```

Expected: PASS.

- [ ] **Step 9: Commit**

```bash
git add migrations/00015_playlist_cover.sql internal/domain/playlist.go \
        internal/store/accounts/playlists.go internal/httpapi/server.go \
        test/integration/playlistcover_test.go
git commit -m "$(cat <<'EOF'
Playlists: record what happened to the cover

Cover generation is best effort and can never fail the playlist operation that
asked for it, which means the outcome has to live somewhere durable or the
interface has no way to say "the tracks are right, the picture is not" and no
way to offer a retry.

Four states rather than a boolean, because "you have not granted
ugc-image-upload" is fixed by a trip through Spotify's consent screen and
"the CDN was slow" is fixed by pressing the button again. Rendering the second
as the first tells somebody to press a thing that cannot work.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 2: What a playlist says about itself

**Files:**
- Modify: `internal/domain/playlist.go`
- Test: `internal/domain/playlist_test.go`

**Interfaces:**
- Consumes: `domain.PlaylistDefinition` (Task 0, already merged).
- Produces: `func (d PlaylistDefinition) Describe(builtAt time.Time) string`.

Today `internal/httpapi/playlists.go:22` has a four-line `playlistDescription` that names the mode and nothing else. The spec wants the description to say *what the playlist is, over what range, ranked how, and rebuilt when*, so a playlist explains itself inside Spotify months later, away from Encore.

It moves to `domain` because it is a pure function of the definition and because it is the one piece of prose Encore writes into somebody else's account — it deserves a test file of its own, not a corner of an HTTP handler's.

**Eleven exact strings. These are the deliverable.** Dates use `2 January 2006`: unambiguous, no ordinal suffix, no locale, because a Spotify description is read by whoever the listener shows it to.

| # | Definition | Exact description |
|---|---|---|
| 1 | top, limit 100, ranged, plays | `Your 100 most played tracks between 1 January 2025 and 31 December 2025, ranked by play count. Built by Encore on 31 July 2026.` |
| 2 | top, limit 100, all time, plays | `Your 100 most played tracks of all time, ranked by play count. Built by Encore on 31 July 2026.` |
| 3 | top, limit 1, ranged, plays | `Your single most played track between 1 January 2025 and 31 December 2025, ranked by play count. Built by Encore on 31 July 2026.` |
| 4 | top, limit 100, ranged, time | `Your 100 most played tracks between 1 January 2025 and 31 December 2025, ranked by listening time. Built by Encore on 31 July 2026.` |
| 5 | min_plays 10, ranged | `Every track you played at least 10 times between 1 January 2025 and 31 December 2025, ranked by play count. Built by Encore on 31 July 2026.` |
| 6 | min_plays 10, all time | `Every track you have ever played at least 10 times, ranked by play count. Built by Encore on 31 July 2026.` |
| 7 | min_plays 1, ranged | `Every track you played at least once between 1 January 2025 and 31 December 2025, ranked by play count. Built by Encore on 31 July 2026.` |
| 8 | min_plays 1, all time | `Every track you have ever played at least once, ranked by play count. Built by Encore on 31 July 2026.` |
| 9 | discoveries, ranged | `Tracks you heard for the first time between 1 January 2025 and 31 December 2025, ranked by play count. Built by Encore on 31 July 2026.` |
| 10 | discoveries, all time | `Tracks you heard for the first time, across your whole history, ranked by play count. Built by Encore on 31 July 2026.` |
| 11 | forgotten, ranged | `Tracks you played heavily before 1 January 2025 and not once between 1 January 2025 and 31 December 2025, ranked by play count. Built by Encore on 31 July 2026.` |

`forgotten` has no all-time form: `Validate` already refuses it (`internal/domain/playlist.go:134`).

- [ ] **Step 1: Write the failing test**

Create `internal/domain/playlist_test.go` (or extend it if it exists):

```go
package domain

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

var (
	descFrom  = time.Date(2025, time.January, 1, 0, 0, 0, 0, time.UTC)
	descTo    = time.Date(2025, time.December, 31, 0, 0, 0, 0, time.UTC)
	descBuilt = time.Date(2026, time.July, 31, 14, 22, 0, 0, time.UTC)
)

// TestDescribeCoversEveryModeAndBothRanges is the copy, asserted in full.
//
// Nothing in this project has ever been opened in a browser, and this is the
// one sentence Encore writes into somebody else's Spotify account, where it
// outlives the session that made it. A substring assertion would pass on a
// sentence with a clause missing.
//
// Fails when: any branch's wording changes; the ranking clause stops varying
// with Sort; the built-on date stops being included; the singular forms are
// removed (cases 3, 7 and 8 then read "1 most played tracks" and "at least 1
// times"); or the date format changes.
func TestDescribeCoversEveryModeAndBothRanges(t *testing.T) {
	tests := []struct {
		name string
		def  PlaylistDefinition
		want string
	}{
		{
			name: "top, a range, by plays",
			def:  PlaylistDefinition{Mode: PlaylistModeTop, Sort: SortByPlays, Limit: 100, From: descFrom, To: descTo},
			want: "Your 100 most played tracks between 1 January 2025 and 31 December 2025, " +
				"ranked by play count. Built by Encore on 31 July 2026.",
		},
		{
			name: "top, all time, by plays",
			def:  PlaylistDefinition{Mode: PlaylistModeTop, Sort: SortByPlays, Limit: 100},
			want: "Your 100 most played tracks of all time, ranked by play count. " +
				"Built by Encore on 31 July 2026.",
		},
		{
			name: "top, a single track, is not pluralised",
			def:  PlaylistDefinition{Mode: PlaylistModeTop, Sort: SortByPlays, Limit: 1, From: descFrom, To: descTo},
			want: "Your single most played track between 1 January 2025 and 31 December 2025, " +
				"ranked by play count. Built by Encore on 31 July 2026.",
		},
		{
			name: "top, a range, by listening time",
			def:  PlaylistDefinition{Mode: PlaylistModeTop, Sort: SortByTime, Limit: 100, From: descFrom, To: descTo},
			want: "Your 100 most played tracks between 1 January 2025 and 31 December 2025, " +
				"ranked by listening time. Built by Encore on 31 July 2026.",
		},
		{
			name: "a minimum play count, over a range",
			def:  PlaylistDefinition{Mode: PlaylistModeMinPlays, Sort: SortByPlays, Limit: 500, MinPlays: 10, From: descFrom, To: descTo},
			want: "Every track you played at least 10 times between 1 January 2025 and " +
				"31 December 2025, ranked by play count. Built by Encore on 31 July 2026.",
		},
		{
			name: "a minimum play count, all time",
			def:  PlaylistDefinition{Mode: PlaylistModeMinPlays, Sort: SortByPlays, Limit: 500, MinPlays: 10},
			want: "Every track you have ever played at least 10 times, ranked by play count. " +
				"Built by Encore on 31 July 2026.",
		},
		{
			name: "a minimum of one is not 1 times",
			def:  PlaylistDefinition{Mode: PlaylistModeMinPlays, Sort: SortByPlays, Limit: 500, MinPlays: 1, From: descFrom, To: descTo},
			want: "Every track you played at least once between 1 January 2025 and " +
				"31 December 2025, ranked by play count. Built by Encore on 31 July 2026.",
		},
		{
			name: "a minimum of one, all time",
			def:  PlaylistDefinition{Mode: PlaylistModeMinPlays, Sort: SortByPlays, Limit: 500, MinPlays: 1},
			want: "Every track you have ever played at least once, ranked by play count. " +
				"Built by Encore on 31 July 2026.",
		},
		{
			name: "discoveries, over a range",
			def:  PlaylistDefinition{Mode: PlaylistModeDiscoveries, Sort: SortByPlays, Limit: 100, From: descFrom, To: descTo},
			want: "Tracks you heard for the first time between 1 January 2025 and " +
				"31 December 2025, ranked by play count. Built by Encore on 31 July 2026.",
		},
		{
			name: "discoveries, all time",
			def:  PlaylistDefinition{Mode: PlaylistModeDiscoveries, Sort: SortByPlays, Limit: 100},
			want: "Tracks you heard for the first time, across your whole history, " +
				"ranked by play count. Built by Encore on 31 July 2026.",
		},
		{
			name: "forgotten favourites",
			def:  PlaylistDefinition{Mode: PlaylistModeForgotten, Sort: SortByPlays, Limit: 100, From: descFrom, To: descTo},
			want: "Tracks you played heavily before 1 January 2025 and not once between " +
				"1 January 2025 and 31 December 2025, ranked by play count. " +
				"Built by Encore on 31 July 2026.",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.def.Describe(descBuilt); got != tc.want {
				t.Errorf("Describe()\n got: %q\nwant: %q", got, tc.want)
			}
		})
	}
}

// TestDescribeStaysUnderSpotifysCeiling pins the bound on the one string that
// leaves this process for somebody else's account.
//
// Spotify caps a description at 300 characters and rejects a longer one, so
// the widest definition the validator will accept must still fit. The widest
// is forgotten (two dates plus a third), at the maximum limit and minimum
// count, over the longest month names.
//
// Fails when: a clause is added to any branch without checking this, or the
// date format grows (a full weekday name would add ~10 characters per date and
// there are three of them in the forgotten branch).
func TestDescribeStaysUnderSpotifysCeiling(t *testing.T) {
	widest := PlaylistDefinition{
		Mode: PlaylistModeForgotten, Sort: SortByTime, Limit: PlaylistMaxTracks,
		MinPlays: PlaylistMaxMinPlays,
		From:     time.Date(2025, time.September, 30, 0, 0, 0, 0, time.UTC),
		To:       time.Date(2025, time.December, 28, 0, 0, 0, 0, time.UTC),
	}
	got := widest.Describe(time.Date(2026, time.September, 30, 0, 0, 0, 0, time.UTC))
	if n := utf8.RuneCountInString(got); n > 300 {
		t.Fatalf("description is %d characters, over Spotify's 300: %q", n, got)
	}
	if strings.Contains(got, "  ") {
		t.Errorf("description has a doubled space: %q", got)
	}
}
```

- [ ] **Step 2: Run it and watch it fail**

Run: `go test -count=1 -run TestDescribe ./internal/domain/`
Expected: FAIL — `def.Describe undefined`.

- [ ] **Step 3: Implement `Describe`**

Append to `internal/domain/playlist.go`:

```go
// Describe renders the sentence Spotify shows beneath a playlist's name.
//
// It is regenerated on every rename and every rebuild, and never on a
// schedule — the same rule migrations/00009_playlists.sql states for the
// tracks. A playlist that changed under its owner would be worse than one that
// is merely out of date.
//
// Every branch is written out rather than assembled from interchangeable
// fragments. This is the only prose Encore writes into somebody else's Spotify
// account, where it outlives the session that made it and is read by whoever
// they show the playlist to; a sentence stitched together from clauses reads
// like one, and the two singular cases below cannot be got right at all
// without branching.
func (d PlaylistDefinition) Describe(builtAt time.Time) string {
	ranged := !d.From.IsZero() && !d.To.IsZero()
	period := "between " + playlistDate(d.From) + " and " + playlistDate(d.To)

	rank := "ranked by play count"
	if d.Sort == SortByTime {
		rank = "ranked by listening time"
	}
	built := "Built by Encore on " + playlistDate(builtAt) + "."

	var what string
	switch {
	case d.Mode == PlaylistModeMinPlays && ranged:
		what = "Every track you played " + atLeastTimes(d.MinPlays) + " " + period
	case d.Mode == PlaylistModeMinPlays:
		what = "Every track you have ever played " + atLeastTimes(d.MinPlays)
	case d.Mode == PlaylistModeDiscoveries && ranged:
		what = "Tracks you heard for the first time " + period
	case d.Mode == PlaylistModeDiscoveries:
		what = "Tracks you heard for the first time, across your whole history"
	case d.Mode == PlaylistModeForgotten:
		// Validate refuses this mode without a range, so period is always real
		// here. The first date is the same From: "before it, and not during it".
		what = "Tracks you played heavily before " + playlistDate(d.From) + " and not once " + period
	case ranged:
		what = "Your " + mostPlayed(d.Limit) + " " + period
	default:
		what = "Your " + mostPlayed(d.Limit) + " of all time"
	}
	return what + ", " + rank + ". " + built
}

// atLeastTimes avoids "at least 1 times".
func atLeastTimes(n int) string {
	if n == 1 {
		return "at least once"
	}
	return "at least " + strconv.Itoa(n) + " times"
}

// mostPlayed avoids "Your 1 most played tracks".
func mostPlayed(limit int) string {
	if limit == 1 {
		return "single most played track"
	}
	return strconv.Itoa(limit) + " most played tracks"
}

// playlistDate is the one date format Encore writes into a Spotify account:
// unambiguous between the two hemispheres of date convention, no ordinal
// suffix to get wrong, and no locale, because the reader is whoever the
// listener shows the playlist to rather than the listener's own browser.
func playlistDate(t time.Time) string { return t.UTC().Format("2 January 2006") }
```

Add `"strconv"` to the imports.

- [ ] **Step 4: Run it and watch it pass**

Run: `go test -count=1 -run TestDescribe ./internal/domain/ -v`
Expected: PASS, all 13 subtests.

- [ ] **Step 5: Point the existing handler at it**

In `internal/httpapi/playlists.go`, replace `playlistDescription` entirely:

```go
// maxPlaylistDescription is Spotify's ceiling, minus the three bytes
// store.Truncate appends when it cuts.
//
// domain.Describe is already bounded well under this by its own test, so the
// clamp is a guard rather than a working part: it exists so that a future
// clause added to a description without re-running that test is truncated
// rather than silently rejected by Spotify, which would fail a rename for a
// reason nobody could see.
const maxPlaylistDescription = 297

// playlistDescription is what Spotify shows under the name.
func playlistDescription(def domain.PlaylistDefinition, builtAt time.Time) string {
	return store.Truncate(def.Describe(builtAt), maxPlaylistDescription)
}
```

Add `"github.com/RequiDev/encore/internal/store"` to the imports, and update the one existing call site in `handleCreatePlaylist`:

```go
	created, err := s.spotify.CreatePlaylist(ctx, token, user.SpotifyUserID, name,
		playlistDescription(def, s.now()))
```

- [ ] **Step 6: Run everything and commit**

```bash
export PATH="$PATH:$(go env GOPATH)/bin"
gofmt -l $(git ls-files '*.go'); go vet ./...; staticcheck ./...
go test -count=1 ./...
```

```bash
git add internal/domain/playlist.go internal/domain/playlist_test.go internal/httpapi/playlists.go
git commit -m "$(cat <<'EOF'
Playlists: a description that explains the playlist

The old one named the mode and nothing else, so a playlist found in a Spotify
library months later said "Built by Encore: most played in this period" without
saying which period, how many tracks, or what "most" was measured in.

It moves to internal/domain because it is a pure function of the definition,
and because it is the only prose Encore writes into somebody else's Spotify
account: it earns a test file rather than a corner of an HTTP handler.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 3: The two Spotify writes

**Files:**
- Modify: `internal/spotify/client.go` (the `request` struct and `attempt`)
- Modify: `internal/spotify/playlists.go`
- Modify: `internal/httpapi/auth.go` (`handleAuthorizePlaylists`)
- Test: `internal/spotify/playlists_test.go`, `internal/spotify/oauth_test.go`

**Interfaces:**
- Consumes: `spotify.Client.do`, `request`, `newTestClient(t, srv, clock, opts...)`, `newFakeClock()`.
- Produces:
  - `const spotify.ScopeImageUpload = "ugc-image-upload"`
  - `const spotify.MaxPlaylistCoverBytes = 190 * 1024`
  - `func (c *Client) UpdatePlaylistDetails(ctx context.Context, accessToken, playlistID, name, description string) error`
  - `func (c *Client) SetPlaylistCover(ctx context.Context, accessToken, playlistID string, jpeg []byte) error`

`request` today carries `form url.Values` and `json any` and nothing else. `PUT /v1/playlists/{id}/images` takes **base64 of the JPEG** under `Content-Type: image/jpeg`, which is neither shape. That is why `request` grows a third body form.

- [ ] **Step 1: Write the failing client test**

Append to `internal/spotify/playlists_test.go`:

```go
// TestUpdatePlaylistDetailsSendsBothFields pins that one request carries the
// name and the description together, so there is no state in which Spotify has
// the new name and the old description.
//
// Fails when: the two are split into separate requests, or either key is
// dropped from the body.
func TestUpdatePlaylistDetailsSendsBothFields(t *testing.T) {
	var (
		gotMethod, gotPath, gotAuth string
		gotBody                     map[string]any
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotAuth = r.Method, r.URL.Path, r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := newTestClient(t, srv, newFakeClock())
	err := c.UpdatePlaylistDetails(context.Background(), "user-token", "playlist01",
		"Heavy rotation", "Your 100 most played tracks of all time.")
	if err != nil {
		t.Fatalf("UpdatePlaylistDetails: %v", err)
	}

	if gotMethod != http.MethodPut {
		t.Errorf("method = %q, want PUT", gotMethod)
	}
	if gotPath != "/v1/playlists/playlist01" {
		t.Errorf("path = %q, want /v1/playlists/playlist01", gotPath)
	}
	if gotAuth != "Bearer user-token" {
		t.Errorf("authorization = %q, want the listener's own token", gotAuth)
	}
	if gotBody["name"] != "Heavy rotation" {
		t.Errorf("name = %v, want Heavy rotation", gotBody["name"])
	}
	if gotBody["description"] != "Your 100 most played tracks of all time." {
		t.Errorf("description = %v, want the generated sentence", gotBody["description"])
	}
}

// TestSetPlaylistCoverSendsBase64UnderImageJPEG pins the body shape Spotify
// documents: base64 text, Content-Type image/jpeg, not multipart and not JSON.
//
// Fails when: the raw JPEG is sent instead of its base64 (the decoded bytes
// then differ), or the content type reverts to application/json.
func TestSetPlaylistCoverSendsBase64UnderImageJPEG(t *testing.T) {
	want := []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10, 'J', 'F', 'I', 'F'}

	var gotType string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotType = r.Header.Get("Content-Type")
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	c := newTestClient(t, srv, newFakeClock())
	if err := c.SetPlaylistCover(context.Background(), "user-token", "playlist01", want); err != nil {
		t.Fatalf("SetPlaylistCover: %v", err)
	}

	if gotType != "image/jpeg" {
		t.Errorf("content-type = %q, want image/jpeg", gotType)
	}
	decoded, err := base64.StdEncoding.DecodeString(string(gotBody))
	if err != nil {
		t.Fatalf("body is not base64: %v", err)
	}
	if !bytes.Equal(decoded, want) {
		t.Errorf("decoded body = %v, want %v", decoded, want)
	}
}

// TestSetPlaylistCoverRefusesAnEmptyImage is the delete-absent rule wearing a
// different hat: this call *replaces* whatever cover the playlist has, so a
// zero-length body is a partial input reaching a replace.
//
// Fails when: the length guard is removed — the request then reaches Spotify,
// which answers 400 *after* the listener has been told a cover was set.
func TestSetPlaylistCoverRefusesAnEmptyImage(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	c := newTestClient(t, srv, newFakeClock())
	for _, empty := range [][]byte{nil, {}} {
		if err := c.SetPlaylistCover(context.Background(), "user-token", "playlist01", empty); err == nil {
			t.Errorf("SetPlaylistCover(%v): want an error, got nil", empty)
		}
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("%d requests reached Spotify, want 0", got)
	}
}

// TestSetPlaylistCoverRebuildsTheBodyOnRetry pins that the raw body survives a
// retry.
//
// This is the specific bug a raw []byte body invites: an io.Reader built once
// and handed to a retry loop is drained by the first attempt, so the second
// sends nothing — and "nothing" here means replacing a cover with an empty
// image. attempt() must build the reader from r.raw on every call.
//
// Fails when: the body reader is hoisted out of attempt() into do(), which is
// the natural-looking refactor that breaks it.
func TestSetPlaylistCoverRebuildsTheBodyOnRetry(t *testing.T) {
	want := []byte{0xFF, 0xD8, 0xFF, 0xDB, 1, 2, 3, 4}

	var calls atomic.Int32
	var lastLen atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		lastLen.Store(int32(len(body)))
		if calls.Add(1) < 3 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	c := newTestClient(t, srv, newFakeClock())
	if err := c.SetPlaylistCover(context.Background(), "user-token", "playlist01", want); err != nil {
		t.Fatalf("SetPlaylistCover: %v", err)
	}
	if got := calls.Load(); got != 3 {
		t.Fatalf("attempts = %d, want 3", got)
	}
	if got := int(lastLen.Load()); got != base64.StdEncoding.EncodedLen(len(want)) {
		t.Fatalf("the last attempt sent %d bytes, want %d — the body was not rebuilt",
			got, base64.StdEncoding.EncodedLen(len(want)))
	}
}

// TestPlaylistWritesDrawOnTheSignInBudget is the instance-wide safety property
// of this whole feature.
//
// A 429 on a background request pauses the catalogue limiter *and* records
// app_settings.spotify_paused_until, which 409s "sync now" for every user on
// the instance and stops enrichment. Both playlist writes are interactive, so
// a 429 on either pauses only the sign-in budget and never reaches the pause
// observer at all.
//
// Fails when: interactive: true is dropped from either request — the pause
// observer then fires and the assertion below catches it.
func TestPlaylistWritesDrawOnTheSignInBudget(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "3600")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	var paused atomic.Int32
	c := newTestClient(t, srv, newFakeClock(),
		WithPauseObserver(func(time.Time) { paused.Add(1) }))

	if err := c.UpdatePlaylistDetails(context.Background(), "user-token", "playlist01", "n", "d"); err == nil {
		t.Fatal("UpdatePlaylistDetails: want an error on a 429")
	}
	if err := c.SetPlaylistCover(context.Background(), "user-token", "playlist01", []byte{1, 2, 3}); err == nil {
		t.Fatal("SetPlaylistCover: want an error on a 429")
	}
	if got := paused.Load(); got != 0 {
		t.Fatalf("the pause observer fired %d times; a playlist write must never "+
			"pause Spotify instance-wide", got)
	}
}

// TestPlaylistWriteForbiddenIsNotRetried pins that a missing scope costs one
// request, not six.
//
// Fails when: classify stops wrapping non-429 4xx in retry.Stop, or a caller
// grows its own retry around these methods.
func TestPlaylistWriteForbiddenIsNotRetried(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	c := newTestClient(t, srv, newFakeClock())
	err := c.SetPlaylistCover(context.Background(), "user-token", "playlist01", []byte{1, 2, 3})
	apiErr, ok := AsAPIError(err)
	if !ok || !apiErr.IsForbidden() {
		t.Fatalf("error = %v, want a 403 APIError", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("attempts = %d, want 1: a scope failure spends quota to fail identically", got)
	}
}
```

Add `"bytes"`, `"encoding/base64"` and `"time"` to that file's imports.

- [ ] **Step 2: Run it and watch it fail**

Run: `go test -count=1 -run 'TestUpdatePlaylistDetails|TestSetPlaylistCover|TestPlaylistWrite' ./internal/spotify/`
Expected: FAIL — `UpdatePlaylistDetails` and `SetPlaylistCover` undefined.

- [ ] **Step 3: Give `request` a raw body**

In `internal/spotify/client.go`, on the `request` struct after `json`:

```go
	// raw is a body sent verbatim under contentType. The playlist cover upload
	// is the only caller: PUT /v1/playlists/{id}/images takes base64 of a JPEG
	// under Content-Type: image/jpeg, which is neither of the two shapes above.
	//
	// It is []byte rather than an io.Reader on purpose. attempt() runs once per
	// retry and must build a fresh reader each time; a Reader stored here would
	// be drained by the first attempt and the second would send an empty body,
	// which for an endpoint that *replaces* a cover means replacing it with
	// nothing.
	raw         []byte
	contentType string
```

In `attempt`, the body switch:

```go
	var body io.Reader
	switch {
	case r.form != nil:
		body = strings.NewReader(r.form.Encode())
	case r.raw != nil:
		body = bytes.NewReader(r.raw)
	case r.json != nil:
		raw, err := json.Marshal(r.json)
		if err != nil {
			return retry.Stop(fmt.Errorf("%s: encode body: %w", r.label, err))
		}
		body = bytes.NewReader(raw)
	}
```

and the content-type switch:

```go
	switch {
	case r.form != nil:
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	case r.raw != nil:
		req.Header.Set("Content-Type", r.contentType)
	case r.json != nil:
		req.Header.Set("Content-Type", "application/json")
	}
```

- [ ] **Step 4: Add the two methods and the scope**

In `internal/spotify/playlists.go`, beside `ScopePlaylistPrivate`:

```go
// ScopeImageUpload is the grant needed to set a playlist's cover image.
//
// Requested together with ScopePlaylistPrivate at the existing
// playlist-creation consent moment, so there is no new interruption: an
// account that never makes a playlist is never asked for either.
//
// Deliberately absent from config.DefaultScopes(), and it must stay absent for
// two reasons. It is a write scope, and the sign-in grant is read-only by
// design. And the reconsent banner is driven by MissingScopes against
// DefaultScopes(), so a scope that is not in that list can never appear there
// — which keeps the banner's closing sentence, "None of these let Encore
// change anything on your Spotify account", true.
const ScopeImageUpload = "ugc-image-upload"

// MaxPlaylistCoverBytes is the largest binary JPEG Spotify will accept.
//
// The documented limit is 256 KB of *base64*, and base64 is four bytes out for
// every three in, so the binary ceiling is 256 x 3/4 = 192 KB. Encore aims
// below that rather than at it: the encoder measures the JPEG, not its base64,
// and 2 KB of headroom is cheaper than a rejected upload.
const MaxPlaylistCoverBytes = 190 * 1024

// UpdatePlaylistDetails renames a playlist and rewrites its description.
//
// One request sets both, so there is no state in which Spotify has the new
// name beside a description describing the old one.
//
// Interactive: somebody pressed a button and is waiting. That also means a 429
// here pauses the sign-in budget rather than the catalogue one, and never
// records an instance-wide pause — see the comment on Client.signin.
func (c *Client) UpdatePlaylistDetails(
	ctx context.Context, accessToken, playlistID, name, description string,
) error {
	if accessToken == "" {
		return fmt.Errorf("update playlist details: no access token")
	}
	if playlistID == "" {
		return fmt.Errorf("update playlist details: no playlist id")
	}
	if err := c.do(ctx, request{
		method:      http.MethodPut,
		url:         c.endpoint("/v1/playlists/"+playlistID, nil),
		label:       "update playlist details",
		bearer:      accessToken,
		json:        map[string]any{"name": name, "description": description},
		interactive: true,
	}); err != nil {
		return fmt.Errorf("spotify: update playlist details: %w", err)
	}
	return nil
}

// SetPlaylistCover replaces a playlist's cover image.
//
// An empty image is refused here rather than sent. Spotify would answer 400,
// but a write path whose only guard against a zero-length body is the remote
// server's opinion of it is one refactor away from replacing a listener's
// cover with nothing at all — and this call replaces, it does not add.
func (c *Client) SetPlaylistCover(
	ctx context.Context, accessToken, playlistID string, jpeg []byte,
) error {
	if accessToken == "" {
		return fmt.Errorf("set playlist cover: no access token")
	}
	if playlistID == "" {
		return fmt.Errorf("set playlist cover: no playlist id")
	}
	if len(jpeg) == 0 {
		return fmt.Errorf("set playlist cover: refusing to upload an empty image")
	}
	if len(jpeg) > MaxPlaylistCoverBytes {
		return fmt.Errorf("set playlist cover: image is %d bytes, over the %d ceiling",
			len(jpeg), MaxPlaylistCoverBytes)
	}

	encoded := make([]byte, base64.StdEncoding.EncodedLen(len(jpeg)))
	base64.StdEncoding.Encode(encoded, jpeg)

	if err := c.do(ctx, request{
		method:      http.MethodPut,
		url:         c.endpoint("/v1/playlists/"+playlistID+"/images", nil),
		label:       "set playlist cover",
		bearer:      accessToken,
		raw:         encoded,
		contentType: "image/jpeg",
		interactive: true,
	}); err != nil {
		return fmt.Errorf("spotify: set playlist cover: %w", err)
	}
	return nil
}
```

Add `"encoding/base64"` to the imports.

- [ ] **Step 5: Ask for both scopes at the existing consent moment**

In `internal/httpapi/auth.go`, `handleAuthorizePlaylists`:

```go
// handleAuthorizePlaylists answers GET /api/auth/spotify/playlists.
//
// The same journey as a relink, asking for two extra scopes. Encore's default
// grant is read-only and stays that way for anybody who never uses playlists:
// demanding write access from every listener on every instance, for a feature
// most will not touch, would be a poor trade for a statistics application.
//
// Both are asked for at once rather than one now and one later. ugc-image-upload
// can only ever be used on a playlist Encore made, which means it can only ever
// be used by an account that has already granted playlist-modify-private, so
// splitting them would buy a second consent screen and no additional safety.
// Spotify issues a token carrying the union of what was granted, so an account
// that already holds the first keeps it.
func (s *Server) handleAuthorizePlaylists(w http.ResponseWriter, r *http.Request) {
	user, err := requireUser(r)
	if err != nil {
		s.redirectWithError(w, r, oauthErrUnauthenticated)
		return
	}
	s.startOAuth(w, r, &user.ID, []string{spotify.ScopePlaylistPrivate, spotify.ScopeImageUpload})
}
```

- [ ] **Step 6: Pin the consent URL**

Add to `internal/spotify/oauth_test.go`:

```go
// TestAuthorizeURLAddsBothPlaylistWriteScopes pins the consent moment.
//
// Fails when: either scope is dropped from handleAuthorizePlaylists' list, or
// AuthorizeURLWithScopes stops appending extras (the eight read scopes would
// still be present and a substring check on one of them would still pass).
func TestAuthorizeURLAddsBothPlaylistWriteScopes(t *testing.T) {
	c := NewClient(config.Spotify{
		ClientID: "client-id", RedirectURL: "https://encore.example.com/cb",
		Scopes: config.DefaultScopes(), AuthBaseURL: "https://accounts.example.com",
	}, discardLogger())

	got := c.AuthorizeURLWithScopes("state", "challenge",
		[]string{ScopePlaylistPrivate, ScopeImageUpload})

	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	scopes := strings.Fields(u.Query().Get("scope"))
	for _, want := range []string{ScopePlaylistPrivate, ScopeImageUpload} {
		if !slices.Contains(scopes, want) {
			t.Errorf("scope %q missing from %v", want, scopes)
		}
	}
	// And it must not have leaked into the sign-in set.
	if slices.Contains(config.DefaultScopes(), ScopeImageUpload) {
		t.Error("ugc-image-upload is in DefaultScopes(); the sign-in grant must stay read-only " +
			"and the reconsent banner must never be able to show a write scope")
	}
}
```

- [ ] **Step 7: Run everything and commit**

```bash
export PATH="$PATH:$(go env GOPATH)/bin"
gofmt -l $(git ls-files '*.go'); go vet ./...; staticcheck ./...
go test -count=1 ./...
```

```bash
git add internal/spotify/client.go internal/spotify/playlists.go internal/spotify/playlists_test.go \
        internal/spotify/oauth_test.go internal/httpapi/auth.go
git commit -m "$(cat <<'EOF'
Spotify: the two playlist writes, and a raw request body

The image endpoint takes base64 under Content-Type: image/jpeg, which is
neither of the two body shapes this package sent, so request grew a raw []byte
field. It is a byte slice rather than a Reader because attempt() runs once per
retry: a Reader stored on the request would be drained by the first attempt and
the second would send an empty body, which on an endpoint that replaces a cover
means replacing it with nothing.

Both writes are interactive, so a 429 on either pauses the sign-in budget alone
and never records app_settings.spotify_paused_until. A button press must not be
able to 409 "sync now" for every user on the instance.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 4: The rename endpoint — the project's first write to somebody's account

**Files:**
- Modify: `internal/httpapi/playlists.go`, `dto.go`, `router.go`
- Modify: `docs/api.md` (the Playlists table)
- Test: `internal/httpapi/playlists_test.go` (create or extend)

**Interfaces:**
- Consumes: `spotify.UpdatePlaylistDetails` (Task 3), `playlists.Rename` (Task 1), `playlistDescription(def, builtAt)` (Task 2).
- Produces: `PATCH /api/playlists/{id}`, `httpapi.RenamePlaylistRequest{Name *string}`.

**The three-way outcome, and it is the whole task:**

| What happened | Status | Message |
|---|---|---|
| Spotify accepted, Encore recorded it | `200` | the updated `Playlist` |
| Spotify refused | `403`/`404`/`409` | names the refusal **and** says the old name is still in place |
| Spotify did not answer | `409` | says Encore **cannot tell** whether it went through |
| Spotify accepted, Encore's write failed | `409` | says Spotify has the new name and Encore does not |

The fourth row is the one that is normally got wrong, and the third is the one that is normally *not written at all* — a transport failure gets flattened into "nothing has changed", which is a claim about somebody else's account that Encore is in no position to make.

**Exact copy, all six strings:**

1. Not a UUID → `That is not a valid playlist id.`
2. `name` absent → `"name" is required.`
3. Empty or too long → the existing `domain.ValidatePlaylistName` messages: `a playlist needs a name`, `the name may be at most 100 characters`.
4. Scope missing → `Encore needs permission to create and change playlists on your Spotify account. Grant it from Settings — nothing else changes, and you can revoke it in Spotify.`
   **This replaces the existing string in `playlistToken`**, which says "create playlists" and is now shown for a rename too. Grep for the old text before changing it: `grep -rn "permission to create playlists" .`
5. Spotify 403 → `Spotify refused the rename. The permission may have been revoked; granting it again from Settings restores it. The playlist still has the name it had before.`
6. Spotify 429 → `Spotify is rate limiting this instance until <RFC3339>, so it would not accept the rename. Your listening data is unaffected and the playlist still has the name it had before; try again after that.`
7. Spotify 404 → `Spotify no longer has that playlist — it may have been deleted from your account. Encore still has the definition, so you can build it again.`
8. No answer → `Encore did not get an answer from Spotify, so it cannot tell whether the rename went through. Open the playlist in Spotify to check — renaming it again is safe either way.`
9. Spotify accepted, local write failed → `Spotify has the new name, but Encore could not record it. The playlist itself is correct; reload this page to see the current state.`

- [ ] **Step 1: Write the failing handler tests**

Follow whatever server-construction helper `internal/httpapi`'s existing tests use; the four cases are what matter:

```go
// TestRenameWritesToSpotifyBeforeEncore pins the ordering that is the whole
// safety story of the first write this project makes to a real account.
//
// Fails when: the local Rename moves above the Spotify call — the fake below
// refuses, and the stored name would then already have changed.
func TestRenameWritesToSpotifyBeforeEncore(t *testing.T) { /* ... */ }

// TestRenameKeepsTheOldNameWhenSpotifyRefuses pins both the status and the
// sentence, because the sentence is the deliverable: a listener who is told
// only "forbidden" does not know whether their playlist is now called
// something they did not choose.
//
// Fails when: the "still has the name it had before" clause is dropped, or the
// handler records the rename locally anyway.
func TestRenameKeepsTheOldNameWhenSpotifyRefuses(t *testing.T) { /* ... */ }

// TestRenameSaysItCannotTellWhenSpotifyDoesNotAnswer is the copy defect this
// endpoint exists to avoid.
//
// A transport failure is not a refusal. The request may have reached Spotify
// and the response may have been lost, so "nothing has changed" is a claim
// about somebody else's account that Encore cannot support — and it is the
// sentence somebody will write by accident, because it reads like the safe one.
//
// Fails when: the transport branch is merged into the refusal branch, or its
// message gains the words "has not been renamed" / "nothing has changed".
func TestRenameSaysItCannotTellWhenSpotifyDoesNotAnswer(t *testing.T) {
	// ... assert the body contains "cannot tell whether the rename went through"
	// ... and assert it does NOT contain "nothing has changed"
}

// TestRenameReportsASpotifySuccessEncoreCouldNotRecord pins the fourth
// outcome. Both systems are now real and they disagree; saying "the rename
// failed" would send somebody to do it a second time, and saying nothing would
// leave every Encore screen showing a name the playlist no longer has.
//
// Fails when: the store error is returned bare through writeError, which would
// surface a generic internal error naming neither system.
func TestRenameReportsASpotifySuccessEncoreCouldNotRecord(t *testing.T) { /* ... */ }

// TestRenameNeverTakesTheSpotifyIdFromTheRequest pins that a body cannot widen
// what this writes to.
//
// Fails when: the handler reads a spotifyId from the request body, or resolves
// the playlist by anything other than Get(ctx, q, user.ID, id).
func TestRenameNeverTakesTheSpotifyIdFromTheRequest(t *testing.T) { /* ... */ }
```

Write them out in full against the package's existing test scaffolding — a fake with a scripted `UpdatePlaylistDetails` and a `playlistStore` whose `Rename` can be made to fail. If `internal/httpapi` has no seam for a fake Spotify client (`s.spotify` is the concrete `*spotify.Client`), point the client at an `httptest.Server` with `spotify.WithBaseURL`, exactly as `internal/spotify`'s own tests do; **do not** introduce an interface for it in this task.

- [ ] **Step 2: Run and watch them fail**

Run: `go test -count=1 -run TestRename ./internal/httpapi/`
Expected: FAIL — no such route, `handleRenamePlaylist` undefined.

- [ ] **Step 3: Add the DTO**

`internal/httpapi/dto.go`, beside `CreatePlaylistRequest`:

```go
// RenamePlaylistRequest is the body of PATCH /api/playlists/{id}.
//
// Name is a pointer so that an absent field and an empty string are different
// requests: the first is a malformed call, the second is somebody trying to
// clear the name, and both must be refused with their own message.
type RenamePlaylistRequest struct {
	Name *string `json:"name"`
}
```

`Playlist` gains a cover block — added here so Task 7 does not have to touch the DTO again:

```go
// PlaylistCover is what happened the last time Encore tried to give this
// playlist a picture.
type PlaylistCover struct {
	// State is "none", "ready", "failed" or "unauthorised". "unauthorised" is
	// separate from "failed" because the fix is a consent journey rather than a
	// retry, and a client must not offer the same button for both.
	State string `json:"state"`
	// Kind is "mosaic" or "pattern", derived from Covered rather than stored,
	// so the two can never disagree. Empty unless State is "ready".
	Kind string `json:"kind"`
	// Covered and Total are the denominator every partial figure in Encore
	// carries. Total is always 4: the grid asks for four tiles however many
	// distinct albums the playlist happens to contain.
	Covered int `json:"covered"`
	Total   int `json:"total"`
	// Reason is why the last attempt failed, in the listener's own terms.
	// Empty unless State is "failed".
	Reason string `json:"reason"`
	// At is when State was last written. Null while State is "none".
	At *time.Time `json:"at"`
}
```

and on `Playlist`, after `BuiltAt`:

```go
	Cover PlaylistCover `json:"cover"`
```

and in `toPlaylist`:

```go
	out.Cover = PlaylistCover{
		State:   string(p.Cover.State),
		Covered: p.Cover.Tiles,
		Total:   domain.CoverTileTotal,
		Reason:  p.Cover.Error,
	}
	if p.Cover.State == domain.CoverReady {
		out.Cover.Kind = "pattern"
		if p.Cover.Mosaic() {
			out.Cover.Kind = "mosaic"
		}
	}
	if !p.Cover.At.IsZero() {
		at := p.Cover.At.UTC()
		out.Cover.At = &at
	}
```

- [ ] **Step 4: Write the handler**

In `internal/httpapi/playlists.go`:

```go
// handleRenamePlaylist answers PATCH /api/playlists/{id}.
//
// The project's first write to a listener's real Spotify account that is not
// the creation of a new object, and the ordering below is the whole of its
// safety story:
//
//  1. Spotify first. PUT /v1/playlists/{id} sets the name and the description
//     in one request, so there is no half-renamed state to reconcile.
//  2. Encore second, and only on a 2xx. The listener's Spotify account is the
//     authority on what their playlist is called.
//  3. Every message names what is true of the playlist right now. A refusal
//     says the old name is still in place, because it is. A transport failure
//     says Encore cannot tell, because it cannot — flattening that into
//     "nothing has changed" would be a claim about somebody else's account
//     that this process is in no position to make. And the one case where
//     Spotify accepted and the local write did not says exactly that, rather
//     than reporting a failure that would send somebody to rename it again.
//
// The Spotify playlist id comes from the stored row, never from the request
// body: Get is scoped by user, so a caller cannot address a playlist that is
// not theirs and no field can widen what this writes to.
func (s *Server) handleRenamePlaylist(w http.ResponseWriter, r *http.Request) {
	user, err := requireUser(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeError(w, r, ErrInvalidRequest("That is not a valid playlist id.", nil))
		return
	}

	var body RenamePlaylistRequest
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, r, err)
		return
	}
	if body.Name == nil {
		writeError(w, r, ErrFieldInvalid("name", `"name" is required.`))
		return
	}
	name := strings.TrimSpace(*body.Name)
	if err := domain.ValidatePlaylistName(name); err != nil {
		writeError(w, r, err)
		return
	}

	ctx := r.Context()
	stored, err := s.playlists.Get(ctx, s.querier, user.ID, id)
	if err != nil {
		writeError(w, r, err)
		return
	}
	token, err := s.playlistToken(ctx, user)
	if err != nil {
		writeError(w, r, err)
		return
	}

	// The description is rewritten alongside the name, because it is derived
	// from the definition and the last build rather than from the name, and
	// because one request that sets both is one fewer state to be in.
	description := playlistDescription(stored.Definition, stored.BuiltAt)
	if err := s.spotify.UpdatePlaylistDetails(ctx, token, stored.SpotifyID, name, description); err != nil {
		writeError(w, r, renameError(err))
		return
	}

	updated, err := s.playlists.Rename(ctx, s.querier, user.ID, stored.ID, name)
	if err != nil {
		logging.FromContext(ctx).Error("spotify accepted a rename that could not be recorded",
			"playlist", stored.SpotifyID, logging.Err(err))
		writeError(w, r, ErrConflictf(
			"Spotify has the new name, but Encore could not record it. "+
				"The playlist itself is correct; reload this page to see the current state."))
		return
	}
	writeJSON(w, r, http.StatusOK, toPlaylist(updated))
}

// renameError turns a Spotify refusal into something a person can act on, and
// every branch states what is true of the playlist afterwards.
//
// The last branch is the one that matters most and is the easiest to get
// wrong. A transport error means the request may have reached Spotify and the
// answer may have been lost; "the playlist has not been renamed" reads like
// the cautious thing to say and is in fact an unverified claim about somebody
// else's account. Encore says what it knows, which is nothing, and says that
// trying again is safe — which is true, because a rename is idempotent.
func renameError(err error) error {
	var paused *spotify.PausedError
	if errors.As(err, &paused) {
		return ErrConflictf(
			"Spotify is rate limiting this instance until %s, so it would not accept the "+
				"rename. Your listening data is unaffected and the playlist still has the "+
				"name it had before; try again after that.",
			paused.Until.UTC().Format(time.RFC3339))
	}
	if apiErr, ok := spotify.AsAPIError(err); ok {
		switch {
		case apiErr.IsForbidden():
			return ErrForbiddenf(
				"Spotify refused the rename. The permission may have been revoked; granting " +
					"it again from Settings restores it. The playlist still has the name it had before.")
		case apiErr.StatusCode == http.StatusNotFound:
			return ErrNotFoundf(
				"Spotify no longer has that playlist — it may have been deleted from your " +
					"account. Encore still has the definition, so you can build it again.")
		}
	}
	return ErrConflictf(
		"Encore did not get an answer from Spotify, so it cannot tell whether the rename " +
			"went through. Open the playlist in Spotify to check — renaming it again is safe " +
			"either way.")
}
```

And widen `playlistToken`'s message:

```go
	if !spotify.HasScope(creds.Scopes, spotify.ScopePlaylistPrivate) {
		return "", ErrForbiddenf(
			"Encore needs permission to create and change playlists on your Spotify account. " +
				"Grant it from Settings — nothing else changes, and you can revoke it in Spotify.")
	}
```

- [ ] **Step 5: Register the route**

`internal/httpapi/router.go`, in the playlists block:

```go
	// A rename writes to the listener's own Spotify account. The id in the path
	// is Encore's; the Spotify id is read from the stored row, never sent.
	s.route(mux, "PATCH /api/playlists/{id}", s.handleRenamePlaylist)
```

- [ ] **Step 6: Refresh the description on a rebuild too**

In `handleRebuildPlaylist`, after `RecordBuild` succeeds:

```go
	// The description names the date of the last build, so a rebuild makes the
	// stored one stale. Refreshed best-effort: the tracks are already replaced
	// and recorded, and failing a rebuild that succeeded — over a sentence —
	// would be a worse outcome than a description that is one build behind.
	if err := s.spotify.UpdatePlaylistDetails(ctx, token, stored.SpotifyID, stored.Name,
		playlistDescription(stored.Definition, now)); err != nil {
		logging.FromContext(ctx).Warn("could not refresh a rebuilt playlist's description",
			"playlist", stored.SpotifyID, logging.Err(err))
	}
```

- [ ] **Step 7: Document the endpoint**

In `docs/api.md`'s Playlists table, after the `POST /api/playlists` row:

```markdown
| `PATCH` | `/api/playlists/{id}` | `{ "name" }`. Renames it **on Spotify first**, then records it here, and rewrites the description from the stored definition in the same request. `403` when the scope has been revoked; `404` when Spotify no longer has the playlist; `409` when Spotify is rate limiting, when Encore got no answer at all, or when Spotify accepted the rename and Encore could not record it — each with its own message saying what is true of the playlist afterwards. A rename is idempotent, so retrying is always safe. |
```

And amend the section's opening paragraph:

```markdown
Builds a Spotify playlist from the caller's own listening, and keeps its name,
description and cover in step. Requires `playlist-modify-private`, and
`ugc-image-upload` for the cover — Encore asks for both at once and **only when
somebody uses this feature**, so the sign-in grant stays read-only for everyone
else.
```

- [ ] **Step 8: Run everything and commit**

```bash
export PATH="$PATH:$(go env GOPATH)/bin"
gofmt -l $(git ls-files '*.go'); go vet ./...; staticcheck ./...
go test -count=1 ./...
ENCORE_TEST_DATABASE_URL="postgres://encore:encore@localhost:5433/encore?sslmode=disable" \
  go test -tags=integration -count=1 -p 1 -timeout=20m ./test/...
```

```bash
git add internal/httpapi/playlists.go internal/httpapi/dto.go internal/httpapi/router.go \
        internal/httpapi/playlists_test.go docs/api.md
git commit -m "$(cat <<'EOF'
Playlists: rename, and say only what Encore has confirmed

The first write this project makes to a listener's existing Spotify object, so
the ordering is the feature: Spotify first, Encore second, and only on a 2xx.

Four outcomes rather than two. A refusal says the old name is still in place. A
transport failure says Encore cannot tell whether it went through, because it
cannot -- "nothing has changed" reads like the cautious sentence and is in fact
an unverified claim about somebody else's account. And a Spotify success that
Encore failed to record says exactly that, rather than reporting a failure that
would send somebody to rename it a second time.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 5: The cover image

**Files:**
- Create: `internal/playlistcover/cover.go`, `internal/playlistcover/pattern.go`
- Create: `internal/playlistcover/cover_test.go`, `internal/playlistcover/pattern_test.go`
- Modify: `go.mod`, `go.sum`

**Interfaces:**
- Consumes: `spotify.MaxPlaylistCoverBytes` (Task 3) — only in the assertion test, not in the package itself.
- Produces:
  - `const playlistcover.Size = 640`, `const playlistcover.Tiles = 4`, `const playlistcover.MaxBytes = 190 * 1024`
  - `type Kind string` with `KindMosaic`, `KindPattern`
  - `type Cover struct { JPEG []byte; Kind Kind; Covered int }`
  - `func Render(name, seed string, tiles [Tiles]image.Image) (Cover, error)`

**Nothing in this package touches the network or the database.** `Render` takes decoded images and returns bytes. Fetching is Task 6's job, uploading is Task 7's.

- [ ] **Step 1: Confirm the dependency can be fetched, before writing anything**

`golang.org/x/image` is **not in the local module cache** — check with `ls "$(go env GOMODCACHE)/golang.org/x/"`. It needs network.

```bash
go get golang.org/x/image@latest
go mod tidy
git diff go.mod
```

Expected: `golang.org/x/image vX.Y.Z` appears in the **first** `require` block (a direct dependency), and `golang.org/x/text` is unchanged. If this fails for lack of network, **stop and report it** — every remaining step of this task depends on it, and CI's `lint` job diffs `go mod tidy`, so a partially-added dependency fails the build for everyone.

- [ ] **Step 2: Write the failing determinism test**

Create `internal/playlistcover/pattern_test.go`:

```go
package playlistcover

import (
	"bytes"
	"image"
	"testing"
)

// noTiles is the fresh-instance case: a catalogue that has not enriched yet
// has no artwork at all.
var noTiles [Tiles]image.Image

// TestPatternCoverIsDeterministic pins that the same definition always
// produces the same picture.
//
// This is two assertions and it needs both. Byte-identity alone would pass for
// a function that returned a constant image, so the second half proves the
// seed is actually an input. Together they pin exactly what the feature
// promises: same definition, same cover, for ever — a cover that changed on
// each rebuild would make a playlist look as though it had been tampered with.
//
// Fails when: the seed stops reaching the pattern (the two definitions then
// render identically and the difference assertion fires); or any
// non-deterministic input creeps in — time.Now, an unseeded rand, or map
// iteration order (the repeat assertion then fires).
func TestPatternCoverIsDeterministic(t *testing.T) {
	const name = "Heavy rotation"

	first, err := Render(name, "top|plays|100|0||", noTiles)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	again, err := Render(name, "top|plays|100|0||", noTiles)
	if err != nil {
		t.Fatalf("Render (second call): %v", err)
	}
	if !bytes.Equal(first.JPEG, again.JPEG) {
		t.Fatalf("the same seed produced two different images (%d and %d bytes)",
			len(first.JPEG), len(again.JPEG))
	}

	other, err := Render(name, "discoveries|time|50|0|2025-01-01|2025-12-31", noTiles)
	if err != nil {
		t.Fatalf("Render (other definition): %v", err)
	}
	if bytes.Equal(first.JPEG, other.JPEG) {
		t.Fatal("two different definitions produced the same image; the seed is not an input")
	}
}

// TestPatternCoverReportsItselfHonestly pins that a cover with no artwork in
// it says so, so the interface can say so too.
//
// Fails when: Kind is hardcoded to mosaic, or Covered is derived from
// len(tiles) rather than from how many were non-nil.
func TestPatternCoverReportsItselfHonestly(t *testing.T) {
	got, err := Render("Heavy rotation", "top|plays|100|0||", noTiles)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if got.Kind != KindPattern {
		t.Errorf("Kind = %q, want %q", got.Kind, KindPattern)
	}
	if got.Covered != 0 {
		t.Errorf("Covered = %d, want 0", got.Covered)
	}
}

// TestRenamingDoesNotReshuffleThePattern pins that the name is drawn on top of
// the pattern rather than being part of its seed.
//
// A rename must change the words on the cover and nothing else; folding the
// name into the seed would make every rename produce an unrecognisably
// different picture, which is the opposite of what a rename is for.
//
// Fails when: name is concatenated into the seed before hashing — the two
// patterns below then differ.
func TestRenamingDoesNotReshuffleThePattern(t *testing.T) {
	a := patternFor("top|plays|100|0||")
	b := patternFor("top|plays|100|0||")
	if !bytes.Equal(a.Pix, b.Pix) {
		t.Fatal("patternFor is not deterministic")
	}
	// The rendered covers differ because the words differ...
	one, _ := Render("Heavy rotation", "top|plays|100|0||", noTiles)
	two, _ := Render("Light rotation", "top|plays|100|0||", noTiles)
	if bytes.Equal(one.JPEG, two.JPEG) {
		t.Fatal("two names produced the same cover; the name is not being drawn")
	}
	// ...but the pattern underneath does not, which is what patternFor pins above.
}
```

- [ ] **Step 3: Write the failing size-ceiling test**

Create `internal/playlistcover/cover_test.go`:

```go
package playlistcover

import (
	"image"
	"image/color"
	"testing"

	"github.com/RequiDev/encore/internal/spotify"
)

// noisyTile returns the least compressible 640x640 image this encoder will
// ever be handed: deterministic pseudo-random pixels, which JPEG cannot find
// any structure in.
//
// Deterministic rather than random, so a failure is reproducible. A real
// four-way photographic mosaic is the input the spec calls out as "exactly the
// input that gets large"; this is that case with the volume turned up.
func noisyTile(seed uint32) image.Image {
	img := image.NewRGBA(image.Rect(0, 0, Size, Size))
	x := seed | 1
	for i := 0; i < Size*Size; i++ {
		// xorshift32: no allocation, no package state, same sequence every run.
		x ^= x << 13
		x ^= x >> 17
		x ^= x << 5
		img.Set(i%Size, i/Size, color.RGBA{uint8(x), uint8(x >> 8), uint8(x >> 16), 255})
	}
	return img
}

// TestFourPhotographMosaicFitsUnderTheCeiling pins the size guarantee and
// proves the quality ladder is doing work.
//
// The second assertion is what makes this test able to fail. Without it, an
// implementation that encoded once at quality 90 and returned whatever came
// out would pass on any input that happened to be small, and the ladder could
// be deleted entirely without anything going red.
//
// Fails when: the ceiling is raised to the base64 limit (256 KB) instead of the
// binary one; the ladder is reduced to a single quality; or the images are
// downscaled before encoding, which would make the first attempt fit and the
// attempt count drop to 1.
func TestFourPhotographMosaicFitsUnderTheCeiling(t *testing.T) {
	tiles := [Tiles]image.Image{noisyTile(1), noisyTile(2), noisyTile(3), noisyTile(4)}

	got, err := Render("Heavy rotation", "top|plays|100|0||", tiles)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if len(got.JPEG) > MaxBytes {
		t.Fatalf("cover is %d bytes, over the %d ceiling", len(got.JPEG), MaxBytes)
	}
	if got.Kind != KindMosaic || got.Covered != 4 {
		t.Fatalf("Kind/Covered = %q/%d, want mosaic/4", got.Kind, got.Covered)
	}

	// The ladder must have stepped down for this input, or it proves nothing.
	_, attempts, err := encodeUnder(noisyTile(9), MaxBytes)
	if err != nil {
		t.Fatalf("encodeUnder: %v", err)
	}
	if attempts < 2 {
		t.Fatalf("noise encoded in %d attempt(s); the quality ladder never ran, so this "+
			"test proves nothing about it", attempts)
	}
}

// TestEncodeUnderRefusesRatherThanReturningAnOversizedImage pins the
// termination guarantee and the refusal.
//
// The ladder has a floor, so it cannot loop for ever; and when even the floor
// is too large it returns an error rather than the smallest attempt. Handing
// back an oversized buffer would push the decision onto a caller whose only
// sensible response is this one -- and whose *incorrect* response is to upload
// it, which Spotify rejects after the listener has been told a cover was set.
//
// Fails when: the loop returns the last attempt regardless of size, or the
// ladder is made unbounded (this call would then not return).
func TestEncodeUnderRefusesRatherThanReturningAnOversizedImage(t *testing.T) {
	jpeg, attempts, err := encodeUnder(noisyTile(7), 100)
	if err == nil {
		t.Fatalf("encodeUnder returned %d bytes for a 100-byte ceiling, want an error", len(jpeg))
	}
	if jpeg != nil {
		t.Error("encodeUnder returned an image alongside its error")
	}
	if attempts != len(coverQualities) {
		t.Errorf("attempts = %d, want the whole ladder (%d)", attempts, len(coverQualities))
	}
}

// TestOneLostTileStillYieldsAMosaic pins the spec's "three-tile cover, not an
// error" against its own contradictory fallback sentence -- see the plan's
// resolution of that ambiguity.
//
// Fails when: Render falls back to the pattern whenever any tile is nil, which
// is the other reading of the spec and the one that discards artwork Encore
// already fetched.
func TestOneLostTileStillYieldsAMosaic(t *testing.T) {
	tiles := [Tiles]image.Image{noisyTile(1), nil, noisyTile(3), noisyTile(4)}

	got, err := Render("Heavy rotation", "top|plays|100|0||", tiles)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if got.Kind != KindMosaic {
		t.Errorf("Kind = %q, want %q", got.Kind, KindMosaic)
	}
	if got.Covered != 3 {
		t.Errorf("Covered = %d, want 3", got.Covered)
	}
}

// TestTheTwoCeilingsAgree pins the invariant between two packages that do not
// import each other.
//
// Fails when: either constant is changed without the other, which would either
// waste headroom or -- the dangerous direction -- let the renderer produce an
// image the uploader refuses, turning every cover into a "failed" state with a
// message nobody can act on.
func TestTheTwoCeilingsAgree(t *testing.T) {
	if MaxBytes > spotify.MaxPlaylistCoverBytes {
		t.Fatalf("playlistcover.MaxBytes (%d) exceeds spotify.MaxPlaylistCoverBytes (%d); "+
			"the renderer would produce images the uploader refuses",
			MaxBytes, spotify.MaxPlaylistCoverBytes)
	}
}

// TestALongNameIsTruncatedRatherThanOverflowing pins that a 100-rune name --
// the validator's maximum -- still produces a cover.
//
// Fails when: the shrink-to-fit ladder loses its truncation floor, and a name
// that does not fit at the smallest size is drawn off the edge or panics.
func TestALongNameIsTruncatedRatherThanOverflowing(t *testing.T) {
	long := ""
	for len([]rune(long)) < 100 {
		long += "Wandering "
	}
	long = string([]rune(long)[:100])

	got, err := Render(long, "top|plays|100|0||", noTiles)
	if err != nil {
		t.Fatalf("Render with a 100-rune name: %v", err)
	}
	if len(got.JPEG) == 0 {
		t.Fatal("Render produced no bytes")
	}
}
```

- [ ] **Step 4: Run them and watch them fail**

Run: `go test -count=1 ./internal/playlistcover/`
Expected: FAIL — no such package.

- [ ] **Step 5: Write the pattern**

Create `internal/playlistcover/pattern.go`:

```go
package playlistcover

import (
	"crypto/sha256"
	"image"
	"image/color"
	"image/draw"
)

// patternFor derives a deterministic background from a seed.
//
// The same definition must produce the same picture every time, on every
// instance, for ever: a cover that changed on each rebuild would make a
// playlist look as though something had tampered with it. So the only input is
// a SHA-256 of the seed, walked byte by byte — no map iteration, no clock, no
// package-level rand, and nothing that varies with Go's version.
//
// The playlist *name* is deliberately not part of the seed. It is drawn on top
// by the caller, so a rename changes the words on the cover and leaves the
// picture underneath alone, which is what a rename should do.
func patternFor(seed string) *image.RGBA {
	sum := sha256.Sum256([]byte(seed))

	base := hsv(float64(sum[0])/256*360, 0.52, 0.26)
	accent := hsv(float64(sum[1])/256*360, 0.66, 0.58)

	img := image.NewRGBA(image.Rect(0, 0, Size, Size))
	// A diagonal ramp, so the cover reads as designed rather than as a fill.
	for y := 0; y < Size; y++ {
		for x := 0; x < Size; x++ {
			img.SetRGBA(x, y, mix(base, accent, float64(x+y)/float64(2*Size)))
		}
	}
	// Eight vertical bands, each positioned and sized from its own digest byte,
	// so two definitions differ visibly rather than only in hue. Drawn with a
	// fixed low alpha so they read as texture rather than as content.
	for i := range 8 {
		w := 20 + int(sum[8+i])%70
		x := int(sum[16+i]) * Size / 256
		band := image.Rect(x, 0, min(x+w, Size), Size)
		shade := color.RGBA{accent.R, accent.G, accent.B, 40}
		draw.Draw(img, band, &image.Uniform{shade}, image.Point{}, draw.Over)
	}
	return img
}

// mix blends two opaque colours.
func mix(a, b color.RGBA, t float64) color.RGBA {
	if t < 0 {
		t = 0
	}
	if t > 1 {
		t = 1
	}
	lerp := func(x, y uint8) uint8 { return uint8(float64(x) + (float64(y)-float64(x))*t) }
	return color.RGBA{lerp(a.R, b.R), lerp(a.G, b.G), lerp(a.B, b.B), 255}
}

// hsv converts to RGB. Hue in degrees, saturation and value in 0..1.
//
// Written out rather than pulled from a dependency: it is twenty lines, it is
// the only colour maths in the project, and go.mod is already gaining one entry
// this phase.
func hsv(h, s, v float64) color.RGBA {
	h = math.Mod(math.Mod(h, 360)+360, 360)
	c := v * s
	x := c * (1 - math.Abs(math.Mod(h/60, 2)-1))
	m := v - c

	var r, g, b float64
	switch {
	case h < 60:
		r, g, b = c, x, 0
	case h < 120:
		r, g, b = x, c, 0
	case h < 180:
		r, g, b = 0, c, x
	case h < 240:
		r, g, b = 0, x, c
	case h < 300:
		r, g, b = x, 0, c
	default:
		r, g, b = c, 0, x
	}
	return color.RGBA{
		uint8((r + m) * 255), uint8((g + m) * 255), uint8((b + m) * 255), 255,
	}
}
```

Add `"math"` to the imports.

- [ ] **Step 6: Write the renderer**

Create `internal/playlistcover/cover.go`:

```go
// Package playlistcover renders the image Encore puts on a playlist it made.
//
// 640x640 JPEG: a 2x2 mosaic of album covers from the playlist's own tracks,
// with the playlist name over a scrim across the lower third.
//
// The spec this implements contradicts itself once, and the resolution is
// recorded here so nobody re-derives it from the contradiction. §1.2 says
// "fewer than four usable covers falls back to a deterministic geometric
// cover"; §3's test table says "one unreachable art URL yields a three-tile
// cover, not an error". Both cannot hold. What holds:
//
//   - zero usable images -> the pattern, byte-identical for a given definition.
//     This is the fresh-instance case §1.2 is describing.
//   - one to four -> a mosaic, with every empty cell filled from that same
//     pattern. This is the lost-tile case §3 is describing, and it keeps the
//     artwork that was found rather than discarding it.
//
// Both report Covered honestly, out of a denominator that is always Tiles.
//
// Nothing here touches the network or the database. Render takes decoded
// images and returns bytes; fetching them is fetch.go's job and uploading the
// result is the caller's.
package playlistcover

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"

	xdraw "golang.org/x/image/draw"
	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/gobold"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
)

const (
	// Size is the edge of the square cover, in pixels. Spotify shows a playlist
	// cover at many sizes and derives them all from one upload.
	Size = 640
	// Tiles is how many album covers the mosaic asks for, and the denominator
	// of every coverage sentence about a cover.
	Tiles = 4
	// MaxBytes is the binary JPEG ceiling. It must stay at or below
	// spotify.MaxPlaylistCoverBytes; a test in this package pins that, and the
	// constant is duplicated rather than imported so a pure image package does
	// not depend on the API client.
	MaxBytes = 190 * 1024
)

// Kind is how a cover was made.
type Kind string

const (
	// KindMosaic means at least one cell holds real album artwork.
	KindMosaic Kind = "mosaic"
	// KindPattern means no artwork was available and the whole cover is the
	// deterministic fallback.
	KindPattern Kind = "pattern"
)

// Cover is a finished cover and an honest account of how it was made.
type Cover struct {
	JPEG []byte
	Kind Kind
	// Covered is how many of Tiles came from real album artwork.
	Covered int
}

// Render builds the cover.
//
// seed is the canonical form of the playlist definition; the same seed always
// produces the same pattern. name is drawn over the result and is deliberately
// not part of the seed, so a rename changes the words without reshuffling the
// picture underneath them.
//
// A nil entry in tiles is a cell whose artwork could not be read. It is filled
// from the pattern rather than left blank, and it is not an error: the cover is
// best-effort, and one slow CDN must not cost a listener their playlist.
func Render(name, seed string, tiles [Tiles]image.Image) (Cover, error) {
	pattern := patternFor(seed)
	canvas := image.NewRGBA(image.Rect(0, 0, Size, Size))

	covered := 0
	half := Size / 2
	for i, src := range tiles {
		cell := image.Rect((i%2)*half, (i/2)*half, (i%2+1)*half, (i/2+1)*half)
		if src == nil {
			draw.Draw(canvas, cell, pattern, cell.Min, draw.Src)
			continue
		}
		// CatmullRom rather than image/draw, which cannot downscale: it samples
		// rather than filters, so a 640px album cover reduced to 320 comes out
		// aliased into visible stair-stepping. This is the reason
		// golang.org/x/image is a dependency at all.
		xdraw.CatmullRom.Scale(canvas, cell, src, src.Bounds(), xdraw.Over, nil)
		covered++
	}

	drawScrim(canvas)
	if err := drawName(canvas, name); err != nil {
		return Cover{}, err
	}

	raw, _, err := encodeUnder(canvas, MaxBytes)
	if err != nil {
		return Cover{}, err
	}

	kind := KindMosaic
	if covered == 0 {
		kind = KindPattern
	}
	return Cover{JPEG: raw, Kind: kind, Covered: covered}, nil
}

const (
	// scrimTop is where the darkening band begins: the lower third, so the name
	// stays legible over artwork of any brightness.
	scrimTop = Size * 2 / 3
	// textMargin, textWidth and textBaseline place the name inside the scrim.
	textMargin   = 36
	textWidth    = Size - 2*textMargin
	textBaseline = Size - 58
)

// drawScrim darkens the lower third with a vertical ramp, so the band has no
// visible edge across the artwork above it.
func drawScrim(canvas *image.RGBA) {
	for y := scrimTop; y < Size; y++ {
		t := float64(y-scrimTop) / float64(Size-scrimTop)
		alpha := uint8(40 + t*175)
		row := image.Rect(0, y, Size, y+1)
		draw.Draw(canvas, row, &image.Uniform{color.RGBA{0, 0, 0, alpha}}, image.Point{}, draw.Over)
	}
}

// nameSizes are tried largest first; the first that fits within textWidth wins.
var nameSizes = []float64{60, 50, 42, 36, 30, 25}

// drawName writes the playlist name across the scrim on one line.
//
// One line, shrink to fit, then truncate with an ellipsis. Word wrapping is
// deliberately out of scope: two lines of variable height would move the
// baseline, which would move the scrim, and a 100-rune name is already served
// by shrinking and cutting. A cover is an identifier, not a paragraph.
func drawName(canvas *image.RGBA, name string) error {
	if name == "" {
		return nil
	}
	for _, points := range nameSizes {
		face, err := nameFace(points)
		if err != nil {
			return err
		}
		if advance := font.MeasureString(face, name); advance.Ceil() <= textWidth {
			write(canvas, face, name)
			return face.Close()
		}
		if points != nameSizes[len(nameSizes)-1] {
			_ = face.Close()
			continue
		}
		// The smallest size still does not fit: drop runes from the end until
		// the name plus an ellipsis does. Runes, not bytes — a cut through a
		// multi-byte rune would render a replacement glyph.
		runes := []rune(name)
		for len(runes) > 1 {
			runes = runes[:len(runes)-1]
			candidate := string(runes) + "…"
			if font.MeasureString(face, candidate).Ceil() <= textWidth {
				write(canvas, face, candidate)
				return face.Close()
			}
		}
		write(canvas, face, "…")
		return face.Close()
	}
	return nil
}

// write draws one line at the fixed baseline.
func write(canvas *image.RGBA, face font.Face, text string) {
	d := &font.Drawer{
		Dst:  canvas,
		Src:  image.NewUniform(color.RGBA{255, 255, 255, 255}),
		Face: face,
		Dot:  fixed.P(textMargin, textBaseline),
	}
	d.DrawString(text)
}

// nameFace builds the type used for the playlist name.
//
// golang.org/x/image/font/gofont/gobold ships as Go source, so no font file and
// no licence file enters this repository. It is not the web client's Inter, and
// that is accepted: nobody sees a 640px playlist cover beside the dashboard.
// Embedding Inter later is this function plus an OFL notice.
//
// A name in a script Go Bold has no glyphs for renders as .notdef boxes rather
// than failing. That is the correct trade for a decorative image: a cover with
// boxes on it is better than a playlist with no cover and an error state.
func nameFace(points float64) (font.Face, error) {
	parsed, err := opentype.Parse(gobold.TTF)
	if err != nil {
		return nil, fmt.Errorf("parse the cover typeface: %w", err)
	}
	face, err := opentype.NewFace(parsed, &opentype.FaceOptions{
		Size: points, DPI: 72, Hinting: font.HintingFull,
	})
	if err != nil {
		return nil, fmt.Errorf("build the cover typeface at %.0fpt: %w", points, err)
	}
	return face, nil
}

// coverQualities is the JPEG quality ladder, tried in order.
//
// 90 first because at 640x640 a four-way photographic mosaic normally lands at
// 60-100 KB and never needs a second attempt. The rungs below exist because
// four photographs is exactly the input that does not compress, and it is also
// the common one.
var coverQualities = []int{90, 80, 70, 60, 50, 40}

// encodeUnder encodes img at the highest quality whose output fits maxBytes,
// and reports how many attempts that took.
//
// It returns an error rather than the smallest attempt when even the floor is
// too large. Handing back an oversized buffer would push the decision onto the
// caller, whose only sensible response is this one — and whose likeliest
// mistaken response is to upload it, which Spotify rejects *after* the listener
// has been told a cover was set.
func encodeUnder(img image.Image, maxBytes int) ([]byte, int, error) {
	var buf bytes.Buffer
	for i, q := range coverQualities {
		buf.Reset()
		if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: q}); err != nil {
			return nil, i + 1, fmt.Errorf("encode cover at quality %d: %w", q, err)
		}
		if buf.Len() <= maxBytes {
			return bytes.Clone(buf.Bytes()), i + 1, nil
		}
	}
	return nil, len(coverQualities), fmt.Errorf(
		"cover is %d bytes at quality %d, over the %d ceiling",
		buf.Len(), coverQualities[len(coverQualities)-1], maxBytes)
}
```

- [ ] **Step 7: Run the suite and watch it pass**

```bash
go test -count=1 ./internal/playlistcover/ -v
```

Expected: PASS, seven tests. If `TestFourPhotographMosaicFitsUnderTheCeiling` reports `attempts < 2`, the noise generator is producing something compressible — fix the generator, **not** the assertion: dropping it would leave the ladder untested.

- [ ] **Step 8: Lint and commit**

```bash
export PATH="$PATH:$(go env GOPATH)/bin"
gofmt -l $(git ls-files '*.go'); go vet ./...; staticcheck ./...
go test -count=1 ./...
```

```bash
git add go.mod go.sum internal/playlistcover/
git commit -m "$(cat <<'EOF'
Covers: render a playlist cover, deterministically

A 2x2 mosaic with the name over a scrim, and a pattern seeded by a hash of the
definition when there is no artwork to mosaic -- which is the ordinary state of
a fresh instance whose catalogue is still enriching.

The name is not part of the seed, so a rename changes the words and leaves the
picture underneath alone.

The quality ladder has a floor and refuses rather than returning an oversized
image. Spotify's documented 256 KB applies to the base64, so the binary ceiling
is 192 KB; this aims at 190 because the encoder measures the JPEG, not its
encoding.

golang.org/x/image is the one new dependency in this phase: image/draw cannot
downscale, and a 640px album cover sampled down to 320 comes out visibly
aliased.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 6: Fetching the art, safely

**Files:**
- Create: `internal/playlistcover/fetch.go`, `internal/playlistcover/fetch_test.go`

**Interfaces:**
- Consumes: `playlistcover.Tiles` (Task 5).
- Produces:
  - `func NewFetcher() *Fetcher`
  - `func (f *Fetcher) Fetch(ctx context.Context, urls [Tiles]string) [Tiles]image.Image`

`albums.image_url` is a database column, and a stored URL is a stored URL whatever wrote it. Anything that can ever put a row in `albums` — a metadata fallback an operator runs, a future import path, a bug — can make the API container issue a GET to an address it chooses, and because the response is decoded as an image it can do it blind. The allowlist is one comparison and it is not optional.

- [ ] **Step 1: Write the failing allowlist test**

Create `internal/playlistcover/fetch_test.go`:

```go
package playlistcover

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
)

// tinyJPEG is a valid 8x8 image, small enough to inline anywhere.
func tinyJPEG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	for i := range 64 {
		img.Set(i%8, i/8, color.RGBA{uint8(i * 4), 20, 60, 255})
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatalf("encode: %v", err)
	}
	return buf.Bytes()
}

// TestAllowedArtHostAcceptsOnlySpotifysCDNs is the SSRF guard, table-driven.
//
// Fails when: the leading dot is dropped from either suffix (evilscdn.co and
// notspotifycdn.com then pass); the https requirement is dropped; the port or
// userinfo checks are dropped; or the check stops lowercasing the host.
func TestAllowedArtHostAcceptsOnlySpotifysCDNs(t *testing.T) {
	tests := []struct {
		raw  string
		want bool
	}{
		{"https://i.scdn.co/image/ab67616d0000b273", true},
		{"https://mosaic.scdn.co/640/abc", true},
		{"https://scdn.co/image/abc", true},
		{"https://image-cdn-ak.spotifycdn.com/image/abc", true},
		{"https://I.SCDN.CO/image/abc", true},

		{"http://i.scdn.co/image/abc", false},               // not https
		{"https://i.scdn.co:8080/image/abc", false},         // explicit port
		{"https://user:pw@i.scdn.co/image/abc", false},      // userinfo
		{"https://evilscdn.co/image/abc", false},            // no leading dot
		{"https://notspotifycdn.com/image/abc", false},      // no leading dot
		{"https://i.scdn.co.evil.example/abc", false},       // suffix is not a suffix
		{"https://169.254.169.254/latest/meta-data", false}, // the address this guard exists for
		{"https://localhost/image/abc", false},
		{"https://10.0.0.5/image/abc", false},
	}

	for _, tc := range tests {
		u, err := url.Parse(tc.raw)
		if err != nil {
			t.Fatalf("parse %q: %v", tc.raw, err)
		}
		if got := allowedArtHost(u); got != tc.want {
			t.Errorf("allowedArtHost(%q) = %v, want %v", tc.raw, got, tc.want)
		}
	}
}

// TestFetchNeverRequestsADisallowedHost pins that the guard runs before the
// request, not after it.
//
// A guard that rejects the *response* has already made the request, which is
// the entire attack: an internal service that acts on a GET has acted by then.
//
// Fails when: allowedArtHost is called after f.http.Do, or not at all — the
// counter below then reads 1.
func TestFetchNeverRequestsADisallowedHost(t *testing.T) {
	var hits atomic.Int32
	internal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer internal.Close()

	f := NewFetcher()
	got := f.Fetch(context.Background(), [Tiles]string{internal.URL + "/image/abc", "", "", ""})

	if hits.Load() != 0 {
		t.Fatalf("%d requests reached a disallowed host, want 0", hits.Load())
	}
	for i, img := range got {
		if img != nil {
			t.Errorf("tile %d is non-nil for a refused fetch", i)
		}
	}
}

// TestFetchRefusesARedirectOffTheAllowlist is the hole a host check alone
// leaves open.
//
// net/http follows redirects by default, so a CDN URL that 302s to an internal
// address defeats a check performed only on the original URL. CheckRedirect
// must re-apply the allowlist on every hop.
//
// Fails when: CheckRedirect is removed from the client, or stops calling
// allowedArtHost — the redirect target below then records a hit.
func TestFetchRefusesARedirectOffTheAllowlist(t *testing.T) {
	var internalHits atomic.Int32
	internal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		internalHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer internal.Close()

	// The fetcher is pointed at a stub standing in for the CDN by overriding the
	// allowlist for this test only, so the redirect hop is the thing under test
	// rather than the first hop.
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, internal.URL+"/image/abc", http.StatusFound)
	}))
	defer cdn.Close()

	f := NewFetcher()
	f.allow = func(u *url.URL) bool { return u.Host == mustHost(t, cdn.URL) }

	got := f.Fetch(context.Background(), [Tiles]string{cdn.URL + "/image/abc", "", "", ""})

	if internalHits.Load() != 0 {
		t.Fatalf("%d requests followed a redirect off the allowlist, want 0", internalHits.Load())
	}
	if got[0] != nil {
		t.Error("a refused redirect produced a tile")
	}
}

// TestFetchDropsAnOversizedBody pins the byte cap by length rather than by
// whether a truncated JPEG happens to fail decoding.
//
// Fails when: the LimitReader or the explicit length check is removed — the
// 3 MB body below then decodes and the tile is kept.
func TestFetchDropsAnOversizedBody(t *testing.T) {
	huge := bytes.Repeat([]byte{0xFF}, 3<<20)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(huge)
	}))
	defer srv.Close()

	f := NewFetcher()
	f.allow = func(u *url.URL) bool { return u.Host == mustHost(t, srv.URL) }

	got := f.Fetch(context.Background(), [Tiles]string{srv.URL + "/image/abc", "", "", ""})
	if got[0] != nil {
		t.Error("an oversized body produced a tile")
	}
}

// TestFetchDropsANonImage pins that the check is a decode rather than a
// content-type header, which any server can lie about.
//
// Fails when: the decode check is replaced by a Content-Type comparison — the
// stub below sets image/jpeg and serves HTML.
func TestFetchDropsANonImage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write([]byte("<!doctype html><html><body>not an image</body></html>"))
	}))
	defer srv.Close()

	f := NewFetcher()
	f.allow = func(u *url.URL) bool { return u.Host == mustHost(t, srv.URL) }

	got := f.Fetch(context.Background(), [Tiles]string{srv.URL + "/x", "", "", ""})
	if got[0] != nil {
		t.Error("an HTML body decoded as an image")
	}
}

// TestFetchKeepsPositionsWhenOneTileFails pins that a failed fetch leaves a
// hole rather than shifting the others.
//
// A mosaic whose cells silently reorder when a CDN is slow would put a
// different picture on the same playlist on each rebuild.
//
// Fails when: Fetch appends successes to a slice instead of writing to the
// index it was given — tile 0 would then hold what tile 1 fetched.
func TestFetchKeepsPositionsWhenOneTileFails(t *testing.T) {
	body := tinyJPEG(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/bad" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	f := NewFetcher()
	f.allow = func(u *url.URL) bool { return u.Host == mustHost(t, srv.URL) }

	got := f.Fetch(context.Background(), [Tiles]string{
		srv.URL + "/bad", srv.URL + "/a", srv.URL + "/b", srv.URL + "/c",
	})
	if got[0] != nil {
		t.Error("tile 0 is non-nil for a 404")
	}
	for i := 1; i < Tiles; i++ {
		if got[i] == nil {
			t.Errorf("tile %d is nil; a neighbour's failure shifted the results", i)
		}
	}
}

func mustHost(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return u.Host
}
```

- [ ] **Step 2: Run and watch them fail**

Run: `go test -count=1 -run 'TestAllowed|TestFetch' ./internal/playlistcover/`
Expected: FAIL — `allowedArtHost`, `NewFetcher` undefined.

- [ ] **Step 3: Write the fetcher**

Create `internal/playlistcover/fetch.go`:

```go
package playlistcover

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg" // album art is JPEG
	_ "image/png"  // and occasionally PNG
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	// artTimeout bounds the whole set of tile fetches rather than each one.
	// Four sequential five-second waits inside an HTTP handler somebody is
	// watching is most of a browser's patience spent on decoration; the fetches
	// run concurrently and share this budget.
	artTimeout = 6 * time.Second
	// maxArtBytes caps one downloaded image. Spotify's largest cover is well
	// under this.
	maxArtBytes = 2 << 20
	// maxArtPixels caps a decoded image's edge, checked from the header before
	// any pixels are allocated. A 30000x30000 JPEG is a few hundred kilobytes
	// on the wire and 3.6 GB in memory.
	maxArtPixels = 4000
	// maxArtRedirects bounds a CDN's redirect chain. Every hop is re-checked
	// against the allowlist.
	maxArtRedirects = 3
)

var (
	errHostNotAllowed   = errors.New("host is not a Spotify CDN")
	errTooManyRedirects = errors.New("too many redirects")
)

// allowedArtHost reports whether a stored image URL may be fetched.
//
// The URL comes out of albums.image_url, a database column, and a stored URL is
// a stored URL whatever wrote it. This is a plain SSRF guard on a server-side
// fetch of an address Encore did not choose: without it, anything that can ever
// put a row in `albums` — a metadata source an operator runs, a future import
// path, a bug — could make the API container issue a GET to a cloud metadata
// endpoint or an internal service, and because the response is decoded as an
// image it could do it blind.
//
// Suffix matching on Spotify's two CDN domains, https only, no explicit port
// and no userinfo. The leading dot is load-bearing: "evilscdn.co" must not pass
// a check that "i.scdn.co" does.
func allowedArtHost(u *url.URL) bool {
	if u == nil || u.Scheme != "https" || u.User != nil || u.Port() != "" {
		return false
	}
	host := strings.ToLower(u.Hostname())
	return host == "scdn.co" ||
		strings.HasSuffix(host, ".scdn.co") ||
		strings.HasSuffix(host, ".spotifycdn.com")
}

// Fetcher reads album artwork from Spotify's CDN.
//
// It has its own http.Client and does not go through internal/spotify. The CDN
// is not the Web API: it spends no quota, needs no token, and must not be able
// to pause the rate limiter that enrichment depends on.
type Fetcher struct {
	http *http.Client
	// allow is the host predicate. A field rather than a direct call so a test
	// can point the fetcher at an httptest server and still exercise the
	// redirect check, which is the part that cannot be tested any other way.
	// Production never replaces it.
	allow func(*url.URL) bool
}

// NewFetcher builds a fetcher with the allowlist and every limit in place.
func NewFetcher() *Fetcher {
	f := &Fetcher{allow: allowedArtHost}
	f.http = &http.Client{
		Timeout: artTimeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxArtRedirects {
				return errTooManyRedirects
			}
			// Re-checked on every hop. net/http follows redirects by default,
			// so a check performed only on the original URL is defeated by a
			// CDN response that 302s somewhere else — which is exactly the
			// shape of the attack the allowlist exists to stop.
			if !f.allow(req.URL) {
				return fmt.Errorf("%w: %s", errHostNotAllowed, req.URL.Hostname())
			}
			return nil
		},
	}
	return f
}

// Fetch reads up to Tiles album covers concurrently and returns them in the
// order given, with nil in the place of any that could not be read.
//
// A tile that fails is a tile that is missing, never an error: the cover is
// best-effort and one slow CDN must not cost a listener their playlist. An
// empty URL is a slot the caller had nothing for and is skipped silently.
//
// Concurrent rather than sequential because this runs inside an HTTP request
// somebody is waiting on: four sequential timeouts is twenty-four seconds, and
// four concurrent ones is six.
func (f *Fetcher) Fetch(ctx context.Context, urls [Tiles]string) [Tiles]image.Image {
	ctx, cancel := context.WithTimeout(ctx, artTimeout)
	defer cancel()

	var (
		out [Tiles]image.Image
		wg  sync.WaitGroup
	)
	for i, raw := range urls {
		if strings.TrimSpace(raw) == "" {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Written to its own index, never appended, so a neighbour's
			// failure cannot shift the mosaic and put a different picture on
			// the same playlist at the next rebuild.
			if img, err := f.fetchOne(ctx, raw); err == nil {
				out[i] = img
			}
		}()
	}
	wg.Wait()
	return out
}

// fetchOne reads and decodes one image, or explains why it did not.
func (f *Fetcher) fetchOne(ctx context.Context, raw string) (image.Image, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, fmt.Errorf("parse artwork url: %w", err)
	}
	// Before the request, not after it. A guard that inspects the response has
	// already made the call, and an internal service that acts on a GET has
	// already acted.
	if !f.allow(u) {
		return nil, fmt.Errorf("%w: %s", errHostNotAllowed, u.Hostname())
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("build artwork request: %w", err)
	}
	resp, err := f.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch artwork: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch artwork: status %d", resp.StatusCode)
	}

	// One byte past the cap, so an oversized body is caught by its length
	// rather than by whether a truncated JPEG happens to fail to decode.
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxArtBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read artwork: %w", err)
	}
	if len(body) > maxArtBytes {
		return nil, fmt.Errorf("artwork is larger than %d bytes", maxArtBytes)
	}

	// The header alone, before any pixels are allocated. Content-Type is not
	// consulted at all: a server can claim anything, and decoding is the only
	// check that means what it says.
	cfg, _, err := image.DecodeConfig(bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("artwork is not an image: %w", err)
	}
	if cfg.Width > maxArtPixels || cfg.Height > maxArtPixels {
		return nil, fmt.Errorf("artwork is %dx%d, over the %d cap",
			cfg.Width, cfg.Height, maxArtPixels)
	}

	img, _, err := image.Decode(bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("decode artwork: %w", err)
	}
	return img, nil
}
```

- [ ] **Step 4: Run and watch them pass**

Run: `go test -count=1 ./internal/playlistcover/ -v`
Expected: PASS, thirteen tests.

- [ ] **Step 5: Lint and commit**

```bash
export PATH="$PATH:$(go env GOPATH)/bin"
gofmt -l $(git ls-files '*.go'); go vet ./...; staticcheck ./...
go test -count=1 ./...
```

```bash
git add internal/playlistcover/fetch.go internal/playlistcover/fetch_test.go
git commit -m "$(cat <<'EOF'
Covers: fetch album art behind an allowlist

albums.image_url is a database column, and a stored URL is a stored URL
whatever wrote it. Fetching one server-side without checking the host is an
SSRF against anything that can reach the API container -- and because the
response is decoded as an image, it can be done blind.

The check runs before the request and again on every redirect hop: net/http
follows redirects by default, so a guard applied only to the original URL is
defeated by a 302. Two megabytes, four thousand pixels an edge, six seconds for
the whole set, and a decode rather than a Content-Type header, which any server
can lie about.

A tile that fails is a missing tile, never an error. One slow CDN must not cost
somebody their playlist.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 7: Generating and uploading a cover, best-effort by construction

**Files:**
- Modify: `internal/stats/playlist.go`
- Modify: `internal/httpapi/playlists.go`, `router.go`, `server.go`
- Modify: `cmd/encore-api/main.go` (construct the fetcher once)
- Modify: `docs/api.md`
- Test: `test/integration/playlistcover_test.go`, `internal/httpapi/playlists_test.go`

**Interfaces:**
- Consumes: `playlistcover.Render`, `playlistcover.NewFetcher`, `playlistcover.Tiles` (Tasks 5–6); `spotify.SetPlaylistCover`, `spotify.ScopeImageUpload` (Task 3); `playlists.SetCover` (Task 1).
- Produces:
  - `func (s *Stats) CoverArtURLs(ctx context.Context, q store.Querier, trackIDs []string) ([]string, error)` — up to `playlistcover.Tiles` image URLs.
  - `func (s *Server) coverFor(ctx context.Context, user domain.User, p domain.Playlist, trackIDs []string) domain.PlaylistCover`
  - `POST /api/playlists/{id}/cover`

**Best-effort is structural here, not a convention.** `coverFor` returns a `domain.PlaylistCover` and **no error**. There is no error value for a caller to propagate by accident, so a create or a rebuild physically cannot be failed by a cover.

- [ ] **Step 1: Write the failing album-selection test**

The four tiles are the four albums contributing the most tracks to the playlist, ties broken by the highest-ranked track. Deterministic, so the same playlist gives the same mosaic.

Add to `test/integration/playlistcover_test.go`:

```go
// TestCoverArtURLsPicksTheTopFourAlbums pins the selection and its tie-break.
//
// Fails when: the ORDER BY loses its count(*) DESC (the albums come back in id
// order), loses its min(ordinality) tie-break (two albums with equal counts
// swap between runs and the mosaic changes on every rebuild), or stops
// filtering image_url <> '' (an unenriched album takes a slot and the tile is
// silently empty).
func TestCoverArtURLsPicksTheTopFourAlbums(t *testing.T) { /* ... */ }

// TestCoverArtURLsReturnsEmptyOnAFreshCatalogue pins the ordinary
// fresh-instance case: every album pending, no image_url anywhere.
//
// Fails when: the empty-set short circuit is removed and the nil Querier
// panics, or the query returns rows with empty urls instead of none.
func TestCoverArtURLsReturnsEmptyOnAFreshCatalogue(t *testing.T) { /* ... */ }
```

- [ ] **Step 2: Add the query**

In `internal/stats/playlist.go`:

```go
// coverArtURLsSQL picks the artwork for a playlist's mosaic.
//
// The four albums contributing the most tracks to the playlist, ties broken by
// the highest-ranked track, so the same playlist always yields the same four
// pictures in the same order and a rebuild does not reshuffle the cover.
//
// WITH ORDINALITY is what carries the ranking through: the caller passes the
// track ids in the order the definition selected them, and ordinality is that
// rank. Without it there is no second key and equal-count albums come back in
// whatever order the planner chooses.
const coverArtURLsSQL = `
SELECT a.image_url
FROM unnest($1::text[]) WITH ORDINALITY AS sel(track_id, rank)
JOIN tracks t ON t.id = sel.track_id
JOIN albums a ON a.id = t.album_id
WHERE a.image_url <> ''
GROUP BY a.id, a.image_url
ORDER BY count(*) DESC, min(sel.rank)
LIMIT $2`

// CoverArtURLs returns up to n album covers for a playlist's tracks.
//
// An empty result is the ordinary state of a fresh instance whose catalogue has
// not enriched yet — every album row exists but none has an image_url — and it
// is a success, not a failure. The renderer turns it into the deterministic
// pattern.
func (s *Stats) CoverArtURLs(
	ctx context.Context, q store.Querier, trackIDs []string, n int,
) ([]string, error) {
	if len(trackIDs) == 0 || n <= 0 {
		// Short circuit before touching q, which the caller may not have when
		// a definition selected nothing.
		return []string{}, nil
	}
	rows, err := q.Query(ctx, coverArtURLsSQL, trackIDs, n)
	if err != nil {
		return nil, postgres.Classify("select cover art", err)
	}
	defer rows.Close()

	out := make([]string, 0, n)
	for rows.Next() {
		var u string
		if err := rows.Scan(&u); err != nil {
			return nil, postgres.Classify("scan cover art", err)
		}
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		return nil, postgres.Classify("select cover art", err)
	}
	return out, nil
}
```

If `internal/stats` registers its statements in a `statements()` table with a pinned parameter count (as `internal/stats/context.go` does), register this one too, with **two** parameters. `TestParameterNumberingIsContiguous` will fail otherwise.

**No blacklist filter here, deliberately.** The track ids arrive from `SelectPlaylistTracks`, which already applied the blacklist; re-applying it would be a second filter over an already-filtered set, and forgetting it is impossible because there is no unfiltered path in.

- [ ] **Step 3: Write the failing best-effort handler test**

```go
// TestAFailingCoverDoesNotFailTheCreate is the property the whole feature
// rests on: a playlist that exists with a grey cover is a far better outcome
// than a create that reports failure because a CDN was slow.
//
// Fails when: coverFor is given an error return and a caller propagates it, or
// the create's SetCover call is allowed to fail the request. Point the fetcher
// at a server that never answers and assert 201 with the right track count.
func TestAFailingCoverDoesNotFailTheCreate(t *testing.T) { /* ... */ }

// TestAMissingImageScopeIsUnauthorisedNotFailed pins the two states apart, and
// pins that no request is spent discovering it.
//
// Fails when: the scope check is dropped and the 403 is classified as
// CoverFailed — the row then offers a retry button for a state a retry cannot
// fix, and one Spotify request is spent per attempt to be told the same thing.
func TestAMissingImageScopeIsUnauthorisedNotFailed(t *testing.T) { /* ... */ }

// TestAnImageScope403NeverParksTheAccount pins the rule at
// internal/sync/account.go:296 for a write scope.
//
// Fails when: the cover path reaches MarkNeedsReauth, or retries — an account
// whose listening history reads perfectly would stop being ingested because a
// decorative image was refused.
func TestAnImageScope403NeverParksTheAccount(t *testing.T) { /* ... */ }
```

- [ ] **Step 4: Write the orchestration**

In `internal/httpapi/playlists.go`:

```go
// coverFor builds and uploads a cover, and reports what happened.
//
// It returns a state to record and **no error**, which is what makes best
// effort structural rather than a convention somebody has to remember: there
// is no error value for a caller to propagate by accident, so a create or a
// rebuild cannot be failed by a picture. Every failure below becomes a state
// the playlist row renders and offers a retry for.
//
// The scope is checked before anything is fetched or encoded. A 403 from the
// image endpoint is an optional-scope refusal in exactly the sense
// internal/sync/account.go:296 describes: it must never park the account and
// must never be retried, so it is far better not to spend the request at all.
func (s *Server) coverFor(
	ctx context.Context, user domain.User, p domain.Playlist, trackIDs []string,
) domain.PlaylistCover {
	now := s.now()
	lg := logging.FromContext(ctx)

	if s.covers == nil || s.userToken == nil {
		return domain.PlaylistCover{State: domain.CoverNone, At: now}
	}

	creds, err := s.credentials.Get(ctx, s.querier, user.ID)
	if err != nil {
		lg.Warn("could not read credentials for a playlist cover", logging.Err(err))
		return domain.PlaylistCover{
			State: domain.CoverFailed, At: now,
			Error: "Encore could not check its Spotify permissions.",
		}
	}
	if !spotify.HasScope(creds.Scopes, spotify.ScopeImageUpload) {
		return domain.PlaylistCover{State: domain.CoverUnauthorised, At: now}
	}

	urls, err := s.stats.CoverArtURLs(ctx, s.querier, trackIDs, playlistcover.Tiles)
	if err != nil {
		lg.Warn("could not select cover art", logging.Err(err))
		return domain.PlaylistCover{
			State: domain.CoverFailed, At: now,
			Error: "Encore could not work out which album covers to use.",
		}
	}
	var slots [playlistcover.Tiles]string
	copy(slots[:], urls)

	tiles := s.covers.Fetch(ctx, slots)
	rendered, err := playlistcover.Render(p.Name, coverSeed(p.Definition), tiles)
	if err != nil {
		lg.Warn("could not render a playlist cover", "playlist", p.SpotifyID, logging.Err(err))
		return domain.PlaylistCover{
			State: domain.CoverFailed, At: now,
			Error: "Encore could not build a cover image.",
		}
	}

	token, err := s.userToken(ctx, user.ID)
	if err != nil {
		lg.Warn("could not get a token for a playlist cover", logging.Err(err))
		return domain.PlaylistCover{
			State: domain.CoverFailed, At: now,
			Error: "Encore could not reach Spotify to set the cover.",
		}
	}
	if err := s.spotify.SetPlaylistCover(ctx, token, p.SpotifyID, rendered.JPEG); err != nil {
		return domain.PlaylistCover{
			State: domain.CoverFailed, At: now, Error: coverFailureReason(err),
		}
	}
	return domain.PlaylistCover{State: domain.CoverReady, Tiles: rendered.Covered, At: now}
}

// coverFailureReason turns a Spotify refusal into a sentence a listener can
// act on. It is stored, so it is bounded by store.Truncate at the repository.
//
// A 403 here is recorded as a failure rather than as CoverUnauthorised on
// purpose: the scope check above already handled "never granted", so a 403 at
// this point means the grant was revoked between the check and the call, and
// the row should say the permission may need granting again rather than
// silently reverting to the never-asked state.
func coverFailureReason(err error) string {
	var paused *spotify.PausedError
	if errors.As(err, &paused) {
		return "Spotify is rate limiting this instance, so it would not accept the cover."
	}
	if apiErr, ok := spotify.AsAPIError(err); ok && apiErr.IsForbidden() {
		return "Spotify refused the cover. The permission may have been revoked."
	}
	return "Spotify would not accept the cover."
}

// coverSeed is the canonical form of a definition, and the only input to the
// fallback pattern.
//
// Written out field by field rather than derived from the struct, so adding a
// field to PlaylistDefinition does not silently change every existing
// playlist's cover the next time it is rebuilt.
func coverSeed(d domain.PlaylistDefinition) string {
	from, to := "", ""
	if !d.From.IsZero() {
		from = d.From.UTC().Format(time.RFC3339)
	}
	if !d.To.IsZero() {
		to = d.To.UTC().Format(time.RFC3339)
	}
	return fmt.Sprintf("%s|%s|%d|%d|%s|%s", d.Mode, d.Sort, d.Limit, d.MinPlays, from, to)
}

// recordCover stores a cover outcome and never fails the request that produced
// it. The playlist and its tracks are already correct; losing the record of a
// picture is not worth a 500.
func (s *Server) recordCover(ctx context.Context, userID, id uuid.UUID, cover domain.PlaylistCover) {
	if err := s.playlists.SetCover(ctx, s.querier, userID, id, cover); err != nil {
		logging.FromContext(ctx).Warn("could not record a playlist cover state",
			"playlist", id.String(), logging.Err(err))
	}
}

// handlePlaylistCover answers POST /api/playlists/{id}/cover.
//
// The retry the playlist row offers. It re-selects the tracks rather than
// storing them, because the cover should reflect what is in the playlist now:
// a rebuild between the failure and the retry changed the answer.
//
// It always returns 200 with the playlist. A cover attempt cannot fail this
// endpoint any more than it can fail a create — the state is the result, and
// the row renders it.
func (s *Server) handlePlaylistCover(w http.ResponseWriter, r *http.Request) {
	user, err := requireUser(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeError(w, r, ErrInvalidRequest("That is not a valid playlist id.", nil))
		return
	}

	ctx := r.Context()
	stored, err := s.playlists.Get(ctx, s.querier, user.ID, id)
	if err != nil {
		writeError(w, r, err)
		return
	}
	sel, err := s.selectPlaylistTracks(ctx, user, stored.Definition)
	if err != nil {
		writeError(w, r, err)
		return
	}

	cover := s.coverFor(ctx, user, stored, sel.IDs())
	s.recordCover(ctx, user.ID, stored.ID, cover)
	stored.Cover = cover

	writeJSON(w, r, http.StatusOK, toPlaylist(stored))
}
```

Wire it into `handleCreatePlaylist`, after `s.playlists.Create` succeeds:

```go
	// Best effort, and it can never fail what has already been made. coverFor
	// returns a state rather than an error precisely so this line cannot be
	// written any other way.
	cover := s.coverFor(ctx, user, stored, ids)
	s.recordCover(ctx, user.ID, stored.ID, cover)
	stored.Cover = cover
```

and into `handleRebuildPlaylist`, after `RecordBuild` succeeds — same three lines, with `stored` already carrying the new `TrackCount` and `BuiltAt`.

- [ ] **Step 5: Add the field, the interface entry and the route**

`internal/httpapi/server.go`:

```go
// coverFetcher reads album artwork for a playlist cover.
//
// An interface rather than the concrete *playlistcover.Fetcher so a handler
// test can supply one that fails every tile, which is the case that proves a
// cover cannot fail a create.
type coverFetcher interface {
	Fetch(ctx context.Context, urls [playlistcover.Tiles]string) [playlistcover.Tiles]image.Image
}
```

Add `Covers coverFetcher` to `Deps`, `covers coverFetcher` to `Server`, and the assignment in the `&Server{...}` literal. It is **optional**: a nil `covers` means this process does not generate covers, which `coverFor` already handles by returning `CoverNone`.

`internal/httpapi/router.go`:

```go
	// The retry the playlist row offers when a cover did not come out. Always
	// 200: a cover attempt is not something that can fail this request.
	s.route(mux, "POST /api/playlists/{id}/cover", s.handlePlaylistCover)
```

`cmd/encore-api/main.go`, beside the other service construction:

```go
	// One fetcher for the process: it holds an http.Client, and a client per
	// request would discard every keep-alive to a CDN four requests are about
	// to hit.
	covers := playlistcover.NewFetcher()
```

and `Covers: covers` in the `httpapi.Deps` literal.

- [ ] **Step 6: Document it**

`docs/api.md`, in the Playlists table:

```markdown
| `POST` | `/api/playlists/{id}/cover` | Builds and uploads a cover, and returns the playlist with the outcome in `cover`. Always `200`: cover generation is best-effort and cannot fail, so the outcome is the `cover.state` field rather than the status code. Needs `ugc-image-upload`; without it the state is `unauthorised`, which is not `failed` and is fixed by the consent link rather than by retrying. |
```

and a narrative subsection after the mode table:

```markdown
### The cover

Every playlist Encore creates or rebuilds gets a generated cover: a 2×2 mosaic
of the four albums contributing most of its tracks, with the name across a
scrim over the lower third, uploaded as a 640×640 JPEG.

`cover` travels on every playlist and carries its own denominator:

```json
{
  "state": "ready",
  "kind": "mosaic",
  "covered": 3,
  "total": 4,
  "reason": "",
  "at": "2026-07-31T14:22:05Z"
}
```

| `state` | Means | What a client must render |
|---|---|---|
| `none` | No attempt has been made. Every playlist made before covers existed. | An offer to add one. |
| `ready` | Spotify accepted an uploaded cover. | `covered` of `total` album covers, in words; `kind` is `pattern` when `covered` is 0. |
| `failed` | An attempt was made and did not finish. | `reason`, and a retry. |
| `unauthorised` | The account has not granted `ugc-image-upload`. | The consent link — **not** a retry, which cannot work. |

`total` is always 4: the grid asks for four tiles however many distinct albums
the playlist happens to contain, so `covered: 2` reports a mosaic that wanted
four and got two rather than a full one built from two.

**Generation cannot fail the operation that triggered it.** A create returns
`201` with its tracks correct whether or not a cover came out, and a rebuild
returns `200` the same way. A playlist that exists with Spotify's grey
placeholder is a far better outcome than a create that reports failure because
a CDN was slow.

**The artwork is fetched from Spotify's CDN, not from the Web API.** It spends
no quota and passes through a host allowlist before the request is made, because
`albums.image_url` is a database column and a stored URL is a stored URL
whatever wrote it.
```

- [ ] **Step 7: Run everything and commit**

```bash
export PATH="$PATH:$(go env GOPATH)/bin"
gofmt -l $(git ls-files '*.go'); go vet ./...; staticcheck ./...
go test -count=1 ./...
ENCORE_TEST_DATABASE_URL="postgres://encore:encore@localhost:5433/encore?sslmode=disable" \
  go test -tags=integration -count=1 -p 1 -timeout=20m ./test/...
```

```bash
git add internal/stats/playlist.go internal/httpapi/ cmd/encore-api/main.go \
        test/integration/playlistcover_test.go docs/api.md
git commit -m "$(cat <<'EOF'
Playlists: give a playlist the cover it earned

coverFor returns a state and no error. That is the design, not an oversight:
with no error value there is nothing for a caller to propagate by accident, so
a create or a rebuild physically cannot be failed by a picture.

The scope is checked before anything is fetched, encoded or uploaded. A 403
from the image endpoint is an optional-scope refusal in the sense
internal/sync/account.go:296 sets out -- it must never park the account and
must never be retried -- so the best outcome is not to spend the request.

"Not granted" and "did not work" are separate states because their fixes are
separate: one is a consent screen, the other is a button.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 8: The playlist row, the documents that are now wrong, and the gate

**Files:**
- Modify: `web/src/lib/types.ts`, `web/src/pages/Settings.tsx`
- Create: `web/src/test/playlist-cover.test.tsx`
- Modify: `.github/workflows/ci.yml`
- Modify: `docs/security.md`, `docs/feature-parity.md`, `README.md`

**Interfaces:**
- Consumes: `PATCH /api/playlists/{id}` (Task 4), `POST /api/playlists/{id}/cover` (Task 7), the `cover` DTO block (Task 4).

**There is no `Playlists.tsx`.** The whole playlist interface is a `<Panel>` inside `web/src/pages/Settings.tsx`, lines 563–762. Work there; do not create a page.

### The copy, in full. This is the deliverable.

**Rename — inline, because this application has no modal.** The row's name becomes an `<Input>` with two buttons when rename is active.

| State | Exact copy |
|---|---|
| Trigger button | `Rename` — `aria-label={`Rename ${playlist.name}`}` |
| Field label | `New name` |
| Field hint | `This renames it in your Spotify account too.` |
| Confirm button | `Save` |
| Cancel button | `Cancel` |
| In flight | the `Button`'s own `busy` state; plus `<p role="status">` reading `Renaming…` |
| Success toast title | `` `Renamed to ${updated.name}` `` |
| Success toast description | `Spotify has the new name and the description has been rewritten.` |
| Any failure | `<p role="alert" className="mt-2 text-sm text-ember">{errorMessage(renamePlaylist.error)}</p>` — the server's sentence, unmodified. **Do not add a client-side fallback message**: every branch of `renameError` already states what is true of the playlist, and a generic "Something went wrong" would replace a precise sentence with a vague one. |

**Cover — four states plus in-flight.** Rendered under the existing `{tracks} · {mode} · built {when}` line.

| `cover.state` | Sub-line | Button |
|---|---|---|
| `none` | *(no line)* | `Add cover` |
| `ready`, `kind: "mosaic"`, `covered === 4` | `Cover built from 4 of 4 album covers.` | `Replace cover` |
| `ready`, `kind: "mosaic"`, `covered < 4` | `` `Cover built from ${covered} of 4 album covers; the rest is a generated pattern.` `` | `Replace cover` |
| `ready`, `kind: "pattern"` | `Cover is a generated pattern — Encore does not have artwork for these tracks yet.` | `Replace cover` |
| `failed` | `` `Cover not generated. ${cover.reason}` `` | `Try again` |
| `unauthorised` | `Cover not generated. Encore has not been given permission to set playlist covers.` | a link, `Allow Encore to set covers` → `/api/auth/spotify/playlists` |
| in flight | `<p role="status">` reading `Building the cover…` | the button's `busy` state |

Notes that are part of the copy, not decoration:

- `4` is written out rather than interpolated from `cover.total`. It is a constant on both sides and interpolating it invites a future `2 of 2`.
- `Replace cover`, never `Rebuild cover` — the row already has a `Rebuild` button for the tracks, and two buttons a word apart is how somebody replaces a playlist's contents when they wanted a new picture.
- The `unauthorised` row shows a **link**, not a `Button`, because it navigates out of the application — matching the existing `Allow Encore to create playlists` anchor at Settings.tsx:655.
- The thumbnail uses the existing `<Artwork>` from `web/src/pages/top/TopList.tsx` with `kind="album"` and `size={40}`, whose missing-art fallback is already a tile rather than a hole. Its `src` is `playlist.spotifyUrl`-adjacent artwork the API does not return, so **pass `src=""`** and let the fallback tile stand: adding a cover image URL to the DTO would mean storing and refreshing Spotify's CDN URL for an image Encore uploaded, which is a cache nobody asked for. The row states the cover's provenance in words instead.

### Coverage convention

`cover` ships `{kind, covered, total}` — `kind` is the `value` (which of the two kinds was produced), `covered`/`total` the denominator. The prose line above is the required "every view states its coverage in words". Both halves are asserted.

- [ ] **Step 1: Add the types**

`web/src/lib/types.ts`, in the playlists block:

```ts
/** What happened the last time Encore tried to give a playlist a picture. */
export type PlaylistCoverState = 'none' | 'ready' | 'failed' | 'unauthorised'

/**
 * The cover, with its own denominator.
 *
 * `unauthorised` is deliberately not `failed`: one is fixed by a trip through
 * Spotify's consent screen and the other by pressing the button again, and
 * offering the same control for both invites somebody to press a thing that
 * cannot work.
 *
 * `total` is always 4 — the mosaic asks for four tiles however many distinct
 * albums the playlist happens to contain, so `covered: 2` reports a grid that
 * wanted four and got two.
 */
export interface PlaylistCover {
  state: PlaylistCoverState
  /** 'mosaic' or 'pattern'; empty unless state is 'ready'. */
  kind: string
  covered: number
  total: number
  /** Why the last attempt failed, in the listener's terms. Empty unless failed. */
  reason: string
  at: string | null
}
```

and `cover: PlaylistCover` on `Playlist`.

- [ ] **Step 2: Write the failing component tests**

Create `web/src/test/playlist-cover.test.tsx`. Use the file conventions of `web/src/test/settings-status.test.tsx` verbatim — the local `stubRoutes`, the copy-pasted `ME` fixture, `createMemoryRouter(routes, { initialEntries: ['/settings'] })`, and `beforeEach(() => { vi.unstubAllGlobals() })`. There is no shared render helper; do not create one in this task.

```tsx
/**
 * The playlist row's rename control and its four cover states.
 *
 * Nothing in this project has ever been opened in a browser, and a rename is a
 * write to somebody's real Spotify account. These tests are the only thing
 * standing between a green suite and a row that offers a retry button for a
 * missing permission, or that says "renamed" for a request that got no answer.
 */

// TestID-style names are not this project's convention; these are `it` blocks.

// Fails when: the cover line for a full mosaic changes wording, or `4` starts
// being interpolated from cover.total and the total drifts.
it('says how many album covers a full mosaic used', async () => {
  stubRoutes({ '/api/me': ME, '/api/blacklist': [], '/api/status': status(),
    '/api/playlists': [playlist({ cover: cover({ state: 'ready', kind: 'mosaic', covered: 4 }) })] })
  render(mountSettings())
  const section = await playlistPanel()
  expect(await within(section).findByText('Cover built from 4 of 4 album covers.'))
    .toBeInTheDocument()
})

// Fails when: the partial-mosaic branch is merged into the full one, which
// would claim four covers were used when three were.
it('says when part of the mosaic is pattern', async () => {
  // ... covered: 3
  expect(await within(section).findByText(
    'Cover built from 3 of 4 album covers; the rest is a generated pattern.',
  )).toBeInTheDocument()
})

// Fails when: a pattern cover is described as a mosaic, which would tell
// somebody Encore found artwork it did not find.
it('says a pattern cover is a pattern, and why', async () => {
  // ... kind: 'pattern', covered: 0
  expect(await within(section).findByText(
    'Cover is a generated pattern — Encore does not have artwork for these tracks yet.',
  )).toBeInTheDocument()
})

// Fails when: the failed and unauthorised states share a branch — the retry
// button then appears for a missing permission, which is the exact defect the
// two states exist to prevent.
it('offers a retry for a failure and a consent link for a missing permission', async () => {
  stubRoutes({ /* two playlists: one failed, one unauthorised */ })
  render(mountSettings())
  const section = await playlistPanel()

  expect(await within(section).findByText(
    'Cover not generated. Spotify would not accept the cover.')).toBeInTheDocument()
  expect(within(section).getByRole('button', { name: /try again/i })).toBeInTheDocument()

  expect(within(section).getByText(
    'Cover not generated. Encore has not been given permission to set playlist covers.',
  )).toBeInTheDocument()
  const consent = within(section).getByRole('link', { name: 'Allow Encore to set covers' })
  expect(consent).toHaveAttribute('href', '/api/auth/spotify/playlists')

  // And the two must not be interchangeable.
  const rows = within(section).getAllByRole('listitem')
  expect(within(rows[1]).queryByRole('button', { name: /try again/i })).not.toBeInTheDocument()
})

// Fails when: the rename hint is dropped. It is the one sentence telling
// somebody this control writes to their Spotify account rather than to a label
// inside Encore.
it('says a rename reaches Spotify before it is confirmed', async () => {
  // ... click Rename
  expect(await within(section).findByText('This renames it in your Spotify account too.'))
    .toBeInTheDocument()
})

// Fails when: the client adds its own error fallback over the server's
// sentence. The server's four rename branches each say what is true of the
// playlist afterwards, and "Something went wrong" replaces all four with
// nothing.
it('shows the server’s own sentence when a rename is refused', async () => {
  // ... PATCH stub answering 409 with the "cannot tell whether the rename went
  // through" message
  expect(await within(section).findByRole('alert')).toHaveTextContent(
    /cannot tell whether the rename went through/)
  expect(within(section).queryByText(/something went wrong/i)).not.toBeInTheDocument()
})

// Fails when: the success toast fires on anything but a 200, or is moved to
// onMutate — the copy would then claim a write Spotify has not confirmed.
it('only says “renamed” after Spotify has confirmed it', async () => { /* ... */ })
```

Write these out fully, following `settings-status.test.tsx`'s exact import block and helper shapes.

- [ ] **Step 3: Run and watch them fail**

```bash
cd web && npm run test -- playlist-cover
```

Expected: FAIL — none of the copy exists.

- [ ] **Step 4: Build the row**

In `Settings.tsx`, add the two mutations beside the existing three:

```tsx
  const [renaming, setRenaming] = useState<string | null>(null)
  const [renameDraft, setRenameDraft] = useState('')

  const renamePlaylist = useMutation({
    mutationFn: ({ id, name }: { id: string; name: string }) =>
      api.patch<Playlist>(`/playlists/${id}`, { name }),
    onSuccess: (updated) => {
      setRenaming(null)
      setRenameDraft('')
      void queryClient.invalidateQueries({ queryKey: qk.playlists() })
      toast.notify({
        tone: 'success',
        title: `Renamed to ${updated.name}`,
        description: 'Spotify has the new name and the description has been rewritten.',
      })
    },
  })

  const rebuildCover = useMutation({
    mutationFn: (id: string) => api.post<Playlist>(`/playlists/${id}/cover`),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: qk.playlists() })
    },
  })
```

`rebuildCover` deliberately raises **no toast**: the endpoint always answers 200 and the outcome is the state on the refreshed row, so a "success" toast would fire for a cover that failed.

And the cover sentence, as a pure function exported for its own unit test:

```tsx
/**
 * coverLine is the row's coverage prose, and the honest report of what the
 * mosaic managed.
 *
 * `4` is written out rather than taken from cover.total: it is a constant on
 * both sides of the API, and interpolating it is how a future change produces
 * "2 of 2", which describes a full mosaic that was never built.
 */
export function coverLine(cover: PlaylistCover): string {
  switch (cover.state) {
    case 'ready':
      if (cover.kind === 'pattern' || cover.covered === 0) {
        return 'Cover is a generated pattern — Encore does not have artwork for these tracks yet.'
      }
      if (cover.covered >= 4) {
        return 'Cover built from 4 of 4 album covers.'
      }
      return `Cover built from ${cover.covered} of 4 album covers; the rest is a generated pattern.`
    case 'failed':
      return `Cover not generated. ${cover.reason}`
    case 'unauthorised':
      return 'Cover not generated. Encore has not been given permission to set playlist covers.'
    default:
      return ''
  }
}
```

Render it beneath the existing metadata line, with the state-appropriate control. Follow the panel's existing class conventions (`text-xs text-ink-faint` for the line, `<Button size="sm">` for the controls, `buttonClass('primary')` for the consent anchor).

- [ ] **Step 5: Make CI run the web suite**

`.github/workflows/ci.yml`, in the `web` job after the `Lint` step:

```yaml
      # Every copy assertion in this repository lives in this suite, and until
      # now nothing ran it but a developer remembering to. Phase 2 shipped
      # twelve copy defects that a green Go suite could not have caught; these
      # are the tests that would have.
      - name: Test
        run: npm run test
```

Verified green at `7292614`: 17 files, 198 tests. If it is red when you get here, **fix the tests before adding the step** — a step that lands red is a step somebody deletes.

- [ ] **Step 6: Correct the documents that this change makes wrong**

Three claims are now false or nearly so. Each is quoted with its file so you can find it exactly.

**`docs/security.md:158`** currently reads:

> `playlist-modify-private` is requested at the moment somebody uses the feature,

It becomes:

> `playlist-modify-private` and `ugc-image-upload` are requested together, at the moment somebody uses the feature — the first to create and rename a playlist, the second to put a cover on it. Neither is in the sign-in grant, so an account that never makes a playlist holds no scope that can alter anything, even if the instance is compromised.

**`docs/feature-parity.md:152-159`**, deviation #6. The sentence *"Encore never holds a grant that can modify a listener's Spotify account unless they have used a feature that needs it"* stays exactly as it is — it is still true, and it is the claim that matters. What changes is the enumeration, from one write scope to two:

> `playlist-modify-private` and `ugc-image-upload` are requested only when a playlist is created; an account that never creates one holds a grant that cannot alter anything, even if the instance is compromised. Playback control is still declined outright.

**`docs/feature-parity.md:95`**, the playlist row, ends with a list of the eight read scopes. It gains, before that list:

> Encore also renames the playlist and its description on request, and generates a cover for it — neither of which the reference project does.

**`README.md:61`** currently says the write scope is requested *"when you create one, so an account that never makes a playlist keeps a read-only grant."* That remains true; make it plural — *"the two write scopes"* — so the README and `docs/security.md` do not disagree about how many there are.

**Do not touch** `docs/feature-parity.md:96` and `:172-173`, or `README.md:337-339`: those are about playback **control**, which stays declined and is a different thing entirely. Conflating them is the specific edit the spec's §4 warns against.

**Do not touch** `docs/api.md:195` or `:275` — the "first"/"second unattended Spotify request" sentences. Nothing in this plan makes an unattended request: every fetch here follows a button press. Those two sentences are Plan B's problem.

- [ ] **Step 7: The full gate**

```bash
export PATH="$PATH:$(go env GOPATH)/bin"
gofmt -l $(git ls-files '*.go'); go vet ./...; staticcheck ./...
go mod tidy && git diff --exit-code go.mod go.sum
go test -count=1 ./...
ENCORE_TEST_DATABASE_URL="postgres://encore:encore@localhost:5433/encore?sslmode=disable" \
  go test -tags=integration -count=1 -p 1 -timeout=20m ./test/...
cd web && npm run lint && npm run typecheck && npm run build && npm run test
```

And the migration cycle, which CI runs separately:

```bash
ENCORE_DATABASE_URL="postgres://encore:encore@localhost:5433/encore?sslmode=disable" \
  sh -c 'go run ./cmd/encore-migrate up && go run ./cmd/encore-migrate status && \
         go run ./cmd/encore-migrate reset --yes && go run ./cmd/encore-migrate up'
```

**Report real output. Do not claim a pass on a command you did not run.** `go test -race` is not in this list because there is no gcc locally; CI runs it on the pull request, and this feature adds one concurrent construct — `Fetcher.Fetch`'s WaitGroup writing to distinct indices of a fixed array — which is exactly the shape a race detector is for. Open the PR and read the `unit` job before calling this done.

- [ ] **Step 8: Commit**

```bash
git add web/src/lib/types.ts web/src/pages/Settings.tsx web/src/test/playlist-cover.test.tsx \
        .github/workflows/ci.yml docs/security.md docs/feature-parity.md README.md
git commit -m "$(cat <<'EOF'
Playlists: show the cover state, and rename from the row

Four cover states with four sentences, because "we have not been given
permission" and "it did not work" are different facts with different fixes, and
a row that offers the same retry button for both invites somebody to press a
thing that cannot work.

The rename control says it writes to Spotify before it does, and shows the
server's own sentence when it fails: each of those four branches states what is
true of the playlist afterwards, and a client-side "Something went wrong" would
replace all four with nothing.

CI now runs the web suite. Every copy assertion in this repository lives there
and until now nothing ran it but somebody remembering to; the last phase shipped
twelve copy defects that no Go test could have caught.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Definition of done

- [ ] Every gate in Task 8 step 7 passes, on real output.
- [ ] The migration applies, rolls back and re-applies.
- [ ] **Spotify is always written before Encore**, and a local write that fails after a Spotify success says so in its own sentence.
- [ ] **A transport failure says Encore cannot tell**, and nowhere claims "nothing has changed".
- [ ] **A failing cover never fails a create or a rebuild** — pinned by a test with a fetcher that never answers.
- [ ] **`unauthorised` and `failed` are separate everywhere**: two states, two sentences, two controls.
- [ ] The same definition renders a byte-identical pattern, and two definitions do not.
- [ ] A four-photograph mosaic encodes under 190 KB **and the quality ladder is proved to have run**.
- [ ] No request is ever made to a host off the allowlist, including through a redirect.
- [ ] `ugc-image-upload` is **not** in `DefaultScopes()`, not in `ReconsentBanner.tsx`, and not in `SCOPE_EXPLANATIONS`.
- [ ] `go.mod` gained exactly one direct dependency; `web/package.json` is unchanged.
- [ ] No `ENCORE_` key was added, so `.env.example`, `docker-compose.yml`, `docs/configuration.md` and `docker-compose.portainer.yml` are untouched.
- [ ] `docs/api.md`, `docs/security.md`, `docs/feature-parity.md` and `README.md` say two write scopes, and still say playback control is declined.
- [ ] CI's `web` job runs `npm run test`.

---

## Self-review

**1. Spec coverage — §1 of the design document, requirement by requirement.**

| Spec | Task |
|---|---|
| §1.1 `PUT /v1/playlists/{id}` for name and description | 3, 4 |
| §1.1 `PUT /v1/playlists/{id}/images` for the cover | 3, 7 |
| §1.1 `ugc-image-upload` requested with `playlist-modify-private` at the existing moment | 3 |
| §1.1 description regenerated from the stored definition | 2, 4 |
| §1.2 640×640 JPEG, mosaic of four album covers, name over a scrim | 5 |
| §1.2 256 KB base64 → 190 KB binary, quality ladder | 3, 5 |
| §1.2 `golang.org/x/image` for `draw` and `font/opentype`, `gobold` | 5 |
| §1.2 at most four fetches, 5 s timeout, 2 MB cap, decode check, **host allowlist** | 6 |
| §1.2 deterministic fallback seeded by the definition | 5 |
| §1.3 best-effort, never fails the operation, surfaced as a state, retryable | 1, 7, 8 |
| §1.3 runs synchronously inside the create/rebuild request | 7 |
| §3 all six of the feature-7 test rows | 5 (three), 6 (one), 7 (one), 2 (one) |
| §4 documentation | 4, 7, 8 |

**Gaps found and closed while reviewing:**
- §3's *"Rebuild resilience: cover failure leaves the playlist created and its tracks correct"* was implied but not asserted. It is now Task 7 step 3's first test, and it is the property `coverFor`'s errorless signature exists to guarantee.
- §1.1's description regeneration on *rebuild* (not only on rename) had no home. Added as Task 4 step 6, best-effort with its own reasoning.

**Gaps found and deliberately left open:**
- §4 lists `docs/configuration.md` and `.env.example` changes. Both belong entirely to feature 8 (`ENCORE_NOWPLAYING_INTERVAL`, `ENCORE_LIBRARY_SYNC_INTERVAL`, the quota table). This plan adds no key and correctly touches neither. Task 8 step 6 says so explicitly so nobody "helpfully" edits them.

**2. Placeholder scan.** Task 4 step 1, Task 7 steps 1 and 3, and Task 8 step 2 give test names, doc comments and "Fails when:" lines but not full bodies, deferring to a named existing file for the scaffolding. That is a real weakness and I am recording it rather than hiding it: the copy assertions in Task 8 and the exact strings in Task 4 are written out in full, which is where the defects live, but an executor working from Task 7 step 1 will have to read `test/integration/libraryworker_test.go` for the harness idiom. Every other step carries complete code.

**3. Type consistency.** Checked end to end:
- `domain.CoverTileTotal` (Go) = `cover.total` (DTO) = `4` written out (TSX). Deliberately not interpolated in the copy — reasoning recorded in Task 8.
- `playlistcover.Covered` → `domain.PlaylistCover.Tiles` → `cover.covered`. Three names for one number, because each layer's word is right for it; the mapping is stated in Task 4's `toPlaylist` and Task 7's `coverFor`.
- `playlistcover.MaxBytes` and `spotify.MaxPlaylistCoverBytes` are two constants that must agree, pinned by `TestTheTwoCeilingsAgree`.
- `CoverState` values are the same four strings in the CHECK constraint, the Go constants and the TS union.
- `playlistDescription(def, builtAt)` gained a parameter; the one existing call site is updated in Task 2 step 5 and the two new ones are in Task 4.
- `CoverArtURLs(ctx, q, trackIDs, n)` — four parameters, matching Task 7's call.

**4. One thing I could not verify and the executor must.** `internal/stats`'s statement registry: `internal/stats/context.go` is described as registering statements with a pinned parameter count and a blacklist-filter assertion (`TestParameterNumberingIsContiguous`, `TestBlacklistIsAppliedEverywhere`). I did not read that registry, so Task 7 step 2 instructs registration conditionally. **Read `internal/stats` before writing `CoverArtURLs`** and follow whatever it actually requires; if `TestBlacklistIsAppliedEverywhere` demands a filter, the reasoning for omitting one (the ids are pre-filtered) belongs in a comment beside the exemption rather than in a bypass.

---

## The second plan: Phase 3b — the now-playing poller

**Not written here.** It covers §2 of the same design document and is roughly six tasks: the config key `ENCORE_NOWPLAYING_INTERVAL` in all five places (`internal/config/config.go`, `docker-compose.yml`, `.env.example`, `docs/configuration.md` and the **regenerated** `docker-compose.portainer.yml` — CI's `lint` job diffs the last one and `test/deploy/composeenv_test.go` enforces the first three the moment the literal string appears anywhere in `config.go`, comments included); `spotify.CurrentPlayback` with **204 as a first-class "nothing playing"**, which `Client.decode` already treats as a success with a zero-value result rather than an error; a poller loop modelled on `internal/library/library.go`'s `Run`/`RunOnce`/`tick`, gated on the interval being set and skipping any account without `user-read-playback-state` or in `needs_reauth` **before** a request is made; `GET /api/nowplaying` and a dashboard card, never on a share link; `migrations/0001N_playback_observations.sql` plus a reaper entry beside the existing session reaper in `cmd/encore-worker/main.go`; and the shuffle/platform backfill as a **separate, droppable final commit**.

Three things that plan must settle that this one did not:

**The shared limiter is its whole risk.** The design document says the poller draws on the interactive budget so a catalogue 429 cannot stall it and it cannot stall enrichment. That is right for the *first* half and dangerous for the second: `budget()` sends every `interactive` request to `c.signin`, a limiter sized `signinRate 5 / signinBurst 10` for a handful of sign-ins a day. A poller at 30 s across five users is 14,400 requests a day through the limiter that authentication depends on, and a 429 on any of them pauses **sign-in** for the whole instance for the Retry-After — which for an exhausted daily quota is most of a day, and which would lock every user out of an instance whose listening data is perfectly fine. That plan must either give the poller a **third limiter of its own** (the honest answer, and the one `signin` itself exists as precedent for) or justify at length why the sign-in budget can absorb it. Whichever it picks, it needs a test in the shape of this plan's `TestPlaylistWritesDrawOnTheSignInBudget`, asserting which limiter is paused and that `onPause` does not fire.

**The doc staleness this plan deliberately left.** `docs/api.md:195` calls the album-tracks fetch *"the first Spotify request `encore-api` makes that nobody clicked for"* and `:275` calls the discography walk *"the second"*. A poller makes a third — and it lives in `encore-worker`, not `encore-api`, which makes both sentences technically survivable and therefore exactly the kind of thing that ships stale twice. `.env.example:160-161` says `ENCORE_ARTIST_ALBUMS_ENABLED` is *"the other unattended request"*, which becomes plainly false; `.env.example:135` already forward-references *"the pollers below"*, which today refers to nothing. Replace the ordinals with a list, in the same commit that adds the poller.

**A config-shape mismatch.** The design document says *"unset means disabled"*, but every existing worker config (`Sync`, `Library`, `Enrich`) uses an explicit `Enabled bool` plus a separate interval, and `internal/config`'s parser has **no `optionalDuration`** — `p.duration` requires a default and errors on a non-positive value. `MetadataFallback` is the only precedent for unset-means-off, via `optionalURL` plus an `Enabled()` method. That plan must pick one and say which, because the loop template it copies (`library.Run`) gates on `w.cfg.Enabled` in its first line.
