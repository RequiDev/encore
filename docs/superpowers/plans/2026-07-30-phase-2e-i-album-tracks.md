# Phase 2e-i — Which Tracks You Have Not Heard

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Encore can already say *"you have heard 9 of 12 tracks on this album"*. This makes it able to name the other three.

**Architecture:** A lazily filled, TTL-refreshed cache of what Spotify lists for an album (`album_tracks`), plus a sibling row per album recording the outcome of the last fetch (`album_track_fetches`). `GET /api/albums/{id}/tracklist` reads only the database and **never blocks on Spotify**; when a fetch is due it claims a database lease and runs it detached, and the page polls until the state resolves. The unheard list is the album's listing minus the tracks the caller has played, derived by the same predicates as the completion numerator so the two figures can never disagree.

**Tech Stack:** Go 1.26, pgx/v5, PostgreSQL 17, React 19 + TypeScript + Vite + TanStack Query v5.

**Spec:** [`docs/design/2026-07-29-phase-2-scope-expansion-design.md`](../../design/2026-07-29-phase-2-scope-expansion-design.md) §5.2, **the album half only**.

**Task count: 8.** Within the 6–9 target; no split proposed.

---

## What is already built, and must not be rebuilt

§5.1 shipped in Phase 2b and is at head:

- `internal/stats/completion.go:84` `AlbumCompletion(ctx, q, userID, albumID) (AlbumCompletion{Heard, Total, Known}, error)` — `count(distinct l.track_id) / albums.total_tracks`, all-time, blacklist-filtered.
- `internal/stats/completion.go:111` `CompletedAlbums` — the range-scoped aggregate.
- `internal/httpapi/dto.go:628` `AlbumCompletionResponse`, served from `handleAlbum` (`internal/httpapi/entities.go:275`).
- `web/src/pages/AlbumDetail.tsx`'s `CompletionFigure`, pinned by `web/src/test/album-completion.test.tsx`.

**Do not touch any of it.** 2e-i adds a second, independent panel beside it.

## Explicitly out of scope — phase 2e-ii owns all of it

`artist_albums`; `GET /v1/artists/{id}/albums`; discography coverage; `album_group`; the artist detail page. **Do not plan for it, stub it, or add a column that anticipates it.** If a schema decision here seems to want an `album_group`, it does not — that table is a different table.

---

## The five design decisions, and why

### 1. The page request never blocks on Spotify

`GET /api/albums/{id}/tracklist` reads two local tables and answers. If a fetch is due it claims the lease (one fast `INSERT … ON CONFLICT … RETURNING`) and hands the actual Spotify walk to a detached goroutine on its own context, then answers immediately with `state: "pending"`. The browser polls that one endpoint every two seconds until the state resolves.

Rejected: doing the fetch in-request with `Limiter.WaitMax` and a short budget. `WaitMax` bounds the *queueing*, not the walk: a four-page album behind a slow upstream is four sequential HTTP requests each with its own retry schedule, and `spotify.PausedError` only tells you the wait was too long *after* you have already spent it. A page that hangs on a third party is a defect, and the retry helper makes the tail longer, not shorter.

The fetch is triggered by the **tracklist route only**, not by `GET /api/albums/{id}`. One trigger point, and the poll does not re-run the album page's statistics on every tick.

**The album must already be in the catalogue.** The handler resolves it with `s.catalog.GetAlbum` and 404s first, so an arbitrary id in the URL cannot make the instance spend a Spotify request on a record nobody listened to. That is the same quota argument §5.2 uses to reject a background sweep.

### 2. Two config keys: `ENCORE_ALBUM_TRACKS_ENABLED` (default `true`) and `ENCORE_ALBUM_TRACKS_TTL` (default `720h`)

**The TTL: 30 days.** An album's track list is effectively immutable after release. Spotify mints a *new* album id for a deluxe edition or a re-issue rather than mutating the old one, so the realistic reasons to re-read are narrow: a pre-release that gained tracks, a market change, or an Encore bug. Thirty days catches all three within a month while costing at most one request per album *view* per month. A 24h TTL would cost about thirty times that for no observable benefit. It is a config key rather than a constant because the cost/freshness trade depends on an instance's quota, which only its operator knows.

**The enable flag: on by default, and it exists.** This was decided by the project's owner, overruling an earlier draft of this plan that argued no flag was needed. The reasoning that governs:

Earlier in this project the owner was asked how the now-playing poller should be controlled and chose *opt-in, config-gated*, specifically so that unattended Spotify traffic is an operator's decision rather than the application's. This request is **different in kind** from the API's other on-demand Spotify calls. Signing in, "sync now" and playlist creation are each the direct consequence of a user clicking a thing; this fires a Spotify request as a side effect of merely *viewing a page*. An operator who wants zero unattended egress from `encore-api` must be able to have it, and the earlier draft's argument — that no other on-demand call has a kill switch — does not survive that distinction.

Default `true`, so the feature works out of the box and the flag is there for the operator who wants it off.

**Still constants, not keys:** `maxPages`, `concurrency`, `leaseTTL`, `fetchTimeout`, `failedRetryAfter` and `recordTimeout` in `internal/albumtracks`. Those are mechanism, not policy.

**Disabled behaviour, at every layer.** "Off" means *do not fetch*. It does not mean *forget what is already on disk*:

| Situation with the flag off | Service | API | Page |
|---|---|---|---|
| A listing is stored | Loads and returns it. **No lease claim, no Spotify call.** | `state: "ready"`, `fetchedAt` set | The list, with its "as of" date |
| A listing is stored and past the TTL | Same. The TTL is never even consulted. | `state: "ready"`, `fetchedAt` set | Same — see the ruling below |
| Nothing is stored | Returns nothing. **No lease claim, no Spotify call.** | `state: "disabled"` | Its own copy, which never mentions Spotify failing |
| Another replica holds a live lease from before the flag was flipped | Reports it | `state: "pending"` | "Asking Spotify"; it resolves and never comes back |

`disabled` is a **fourth API state**, not an overloading of `unavailable`. "Your operator turned this off" and "Spotify would not answer" are different facts, and a page that renders the second for the first is blaming a third party for a local decision. The web client must branch on it separately.

**Ruling on a stale listing while disabled: serve it, as `ready`, and say when it was read.** Withholding a listing that is on disk and was correct when it was read would be strictly worse — the operator turned off *fetching*, not the album page. The honesty requirement is met by `fetchedAt`, which the panel renders on **every** `ready` render (both the has-missing and the played-everything branches — an earlier draft rendered it only in the first, and that is corrected in Task 7). A date is not a freshness claim, and no copy anywhere says or implies that the list is current or that a refresh is coming.

Deliberately **not** added: a second field saying "and this will never refresh". It would be a second way to express a fact `fetchedAt` already tells truthfully, and a redundant field in an API contract is a field that drifts. `state` says what Encore has; `fetchedAt` says how old it is; nothing claims more than that.

**Both keys must land in four places** or a mechanical guard fails: `internal/config/config.go`, `docker-compose.yml`'s `x-encore-env` anchor (`test/deploy/composeenv_test.go` `TestComposeForwardsEveryConfigurationVariable`), `.env.example` (`TestEnvExampleDocumentsWhatComposeForwards`), and `docs/configuration.md`. `.env.example` states a per-day request formula for the library worker, so this block carries its own accounting too — a different budget from a different process on a different trigger, and folding it into the library formula would misstate both.

### 3. Concurrency: a database lease, not an in-process lock

`album_track_fetches.status = 'fetching'` **is** the lease, claimed by a conditional upsert that returns a row only to the winner. Two tabs, two browsers, two API replicas behind one database: exactly one starts a fetch, the losers report `pending`.

An in-process mutex or `singleflight` would be wrong here, not merely weaker: Encore is deployed as `encore-api` + `encore-worker` and an operator may run several API replicas, so a lock in one process's memory says nothing about the others. `attempted_at` expires the lease after 2 minutes, so a process killed mid-fetch does not strand an album in `fetching` for ever — that is the only reason the state machine terminates, and therefore the only reason the browser's poll terminates.

A bounded slot channel (4) sits *in front of* the claim as a second, different guard: the lease answers "is anybody fetching this album?", the slots answer "is this process already busy?". Somebody opening twenty album pages must not fan out twenty concurrent Spotify walks.

### 4. Failure is never emptiness

Three separate mechanisms, because this is the requirement most likely to be quietly broken:

1. **`album_tracks` and `album_track_fetches` are separate tables.** No rows in `album_tracks` is ambiguous; a status row is not. `status` ∈ `{fetching, ok, failed}`, and `fetched_at` is set only by a success and never cleared by a later failure.
2. **A 200 with zero items is recorded as a failure, not as an empty listing.** There is no such record as an album with no tracks; an empty listing means the album is invisible to this application's market or has been withdrawn. Writing it as `ok` would make the page say "you have played every track on this album", which is precisely the overclaim this feature exists to prevent.
3. **Any error at all abandons the write.** `ErrTruncated` arrives *with* a partial listing; `ReplaceAlbumTracks` is delete-absent, so writing a prefix would delete the tail of a listing that was correct and then mark the result authoritative. The guard is `if err != nil` with no exceptions, and a test pins that a truncated fetch leaves the previous listing byte-for-byte intact.

The API's four states are `ready`, `pending`, `unavailable` and `disabled` (decision 2), and the fifth render — *fetched, and you have played everything* — is `ready` with an empty `missing` and `coverage.covered == coverage.total`. All five renders have their own copy and their own test. `missing` is empty in four of the five renders — in every state but `ready`-with-something-missing — which is exactly why `state` exists and why a client must branch on it before it branches on the list.

### 5. Pagination: 50 a page, `next` followed to exhaustion, truncation is an error

`GET /v1/albums/{id}/tracks` is offset paginated at 50, exactly like `SavedTracks`. `AlbumTracks` walks `offset` until `next` is empty or `items` is empty, bounded at 20 pages (1,000 tracks — longer than any released album). A budget spent while `next` still pointed somewhere returns the partial listing **alongside** `spotify.ErrTruncated`, matching `internal/spotify/library.go` exactly, and the caller treats it as a failure.

---

## Two conventions this phase must not break

**The coverage denominator.** The unheard list's denominator is `count(album_tracks)` for that album — what Spotify returned — **never** `albums.total_tracks`. They are different numbers from different sources and they can disagree. When they do, the panel prints both and says which one it followed. `total_tracks = 0` still means "enrichment has not resolved this album", the Heard panel still says so rather than showing 0%, and **a cached listing must not be used to back-fill `albums.total_tracks` or to compute a completion percentage** — enrichment owns that column, and a truncated listing would write a wrong total into the instance-wide `CompletedAlbums` aggregate.

**Blacklist composition.** `albumHeardTracksSQL` reads `listens`, so it composes `blacklistFilter` and is registered in `internal/stats/stats_test.go`'s `statements()` with its exact parameter count. `TestBlacklistIsAppliedEverywhere` and `TestParameterNumberingIsContiguous` then cover it mechanically. It is **not** range-filtered, for the same reason `albumCompletionSQL` is not: a track heard five years ago is not a track you have never played.

---

## Global Constraints

- **No new Go module dependency and no new npm dependency.** `go.mod` and `web/package.json` are unchanged at the end.
- **No user scope.** `/v1/albums/{id}/tracks` is public catalogue data; it uses `getAsApp` (`internal/spotify/catalog.go:105`). **Do not add a scope to `DefaultScopes()`.**
- Next migration number is **`00013_`**. House style is `migrations/00012_playlist_context.sql`: goose `Up` *and* `Down`, both directions working, reasoning in comments including what was considered and rejected.
- `Store.InTx(ctx, func(ctx, tx pgx.Tx) error)` is the single-transaction idiom. Repository methods take the caller's `store.Querier` and never reach for `r.db`.
- `internal/httpapi` contains no SQL and never imports pgx. It reaches services and repositories through narrow interfaces.
- Test DB on port **5433**, not 5432. `make` is NOT installed.
- `go test -race` will NOT work locally: no gcc. Omit it. CI runs `go test -tags=integration -race -count=1 -p 1 ./test/...` on Linux.
- staticcheck at `$(go env GOPATH)/bin`; `export PATH="$PATH:$(go env GOPATH)/bin"` first.
- **NUL check every file you write:** `perl -0777 -ne 'print "NULs: ", tr/\0//, "\n"' <file>` — expect 0.
- Commit style `Area: lowercase summary`, body explaining *why*, ending `Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>`. Stage paths explicitly; never `git commit -a`.
- **Every test in this plan carries a "Fails when:" line.** If you add a test of your own and cannot write that line, the test cannot fail and must be replaced.

---

## File Structure

| File | Responsibility |
|---|---|
| `migrations/00013_album_tracks.sql` | **Create.** `album_tracks` + `album_track_fetches` |
| `test/harness/harness.go` | **Modify.** Two entries in `truncatedTables` |
| `internal/spotify/albumtracks.go` | **Create.** `AlbumTracks` — app token, offset paginated, `ErrTruncated` |
| `internal/spotify/albumtracks_test.go` | **Create.** Its stub tests |
| `internal/store/catalog/albumtracks.go` | **Create.** Load, claim, replace, mark, fail |
| `internal/albumtracks/albumtracks.go` | **Create.** TTL / lease / backoff policy, detached fetch, bounded fan-out |
| `internal/albumtracks/albumtracks_test.go` | **Create.** Policy tests against fakes |
| `internal/config/config.go` | **Modify.** `AlbumTracks{TTL}` + `Redacted` |
| `docker-compose.yml`, `.env.example`, `docs/configuration.md` | **Modify.** The one new key |
| `internal/stats/completion.go` | **Modify.** `AlbumHeardTracks` |
| `internal/stats/stats_test.go` | **Modify.** One line in `statements()` |
| `internal/httpapi/{dto,entities,router,server}.go` | **Modify.** DTO, handler, route, dependency |
| `cmd/encore-api/main.go`, `test/e2e/e2e_test.go` | **Modify.** Construct and close the service |
| `web/src/lib/{types,query}.ts`, `web/src/pages/AlbumDetail.tsx` | **Modify.** Types, key, panel, poll |
| `web/src/test/album-tracklist.test.tsx` | **Create.** All five renders, the read date, the disagreement line |
| `test/integration/albumtracks_test.go` | **Create.** Store and service against a real database |
| `docs/api.md`, `docs/feature-parity.md` | **Modify.** |

---

### Task 1: Schema

**Files:**
- Create: `migrations/00013_album_tracks.sql`
- Modify: `test/harness/harness.go:154-177` (`truncatedTables`)
- Test: `test/integration/migrate_test.go` (existing; run it, do not edit)

**Interfaces:**
- Consumes: nothing.
- Produces: tables `album_tracks (album_id, track_id, name, disc_number, track_number)` PK `(album_id, track_id)`, and `album_track_fetches (album_id PK, status, fetched_at, attempted_at, attempts, last_error)` with `status` checked against `('fetching','ok','failed')`. Both cascade from `albums (id)`.

**A deliberate deviation from the spec's sketch.** §5.2 sketches `album_tracks (album_id, track_id, disc_number, track_number, fetched_at)`. `fetched_at` moves to the sibling table because per row it is redundant — every row of one album shares one instant — and because a per-row timestamp cannot express "the fetch failed" or "it has never been tried", which is exactly the state the page has to distinguish. Say so in the migration comment.

- [ ] **Step 1: Write the migration**

```sql
-- +goose Up

-- The tracks Spotify lists for one album, so Encore can name the ones nobody
-- has ever played.
--
-- Phase 2b already computes completion — "you have heard 9 of 12" — from
-- albums.total_tracks and the fact table, with no Spotify call at all. What it
-- cannot do is name the other three, because nothing on disk says what an
-- album's tracks are. This table is that list and nothing more; it is not a
-- second catalogue and it is not an input to completion.
--
-- Global rather than per-user, like every other catalogue table: two listeners
-- on one instance who open the same album share one listing and one fetch.
CREATE TABLE album_tracks (
    album_id     text    NOT NULL REFERENCES albums (id) ON DELETE CASCADE,
    track_id     text    NOT NULL,
    -- Denormalised on purpose, and deliberately without a foreign key to
    -- tracks.
    --
    -- A track nobody has ever played is by definition absent from `tracks`:
    -- that table is minted from listening. Minting a 'pending' row for each one
    -- would hand the enrichment worker hundreds of rows per album view, for
    -- music nobody listened to, in order to learn a name the very same response
    -- already carried. The listing is a display cache; `tracks` stays the
    -- catalogue of what was actually heard.
    name         text    NOT NULL DEFAULT '',
    disc_number  integer NOT NULL DEFAULT 1,
    track_number integer NOT NULL DEFAULT 0,
    PRIMARY KEY (album_id, track_id)
);

-- One row per album Encore has tried to list, holding the outcome of the last
-- attempt.
--
-- Separate from album_tracks because the absence of rows there is ambiguous in
-- three ways, and the difference is the whole feature:
--
--   never fetched     -> "Encore is asking Spotify for this list"
--   the fetch failed  -> "Encore could not read this list"
--   fetched, is empty -> impossible, and recorded as a failure; there is no
--                        such record as an album with no tracks, so a 200 with
--                        no items means the album is invisible to this
--                        application's market or has been withdrawn
--
-- The design sketch in §5.2 puts a fetched_at on album_tracks instead. That
-- cannot express any of the three: it is per row, so it says nothing at all
-- when there are no rows, which is exactly the case that needs explaining. It
-- would also repeat one instant across every row of one album.
--
-- 'fetching' doubles as a lease. A second page view — another tab, another
-- browser, another API replica sharing this database — sees it and does not
-- start a duplicate request against a quota the whole application shares.
-- attempted_at is what expires that lease, so a process killed mid-fetch does
-- not strand an album in 'fetching' for ever; without that, a browser polling
-- for the listing would poll for ever too.
CREATE TABLE album_track_fetches (
    album_id     text        PRIMARY KEY REFERENCES albums (id) ON DELETE CASCADE,
    status       text        NOT NULL
                             CHECK (status IN ('fetching', 'ok', 'failed')),
    -- When the listing in album_tracks was last replaced successfully. NULL
    -- until one succeeds, and never cleared afterwards: a failure that follows
    -- a success leaves the older listing readable and says when it was read,
    -- rather than discarding a good answer because a later request timed out.
    fetched_at   timestamptz,
    -- When the most recent attempt started, successful or not. Drives both the
    -- lease above and the retry backoff after a failure.
    attempted_at timestamptz NOT NULL,
    attempts     integer     NOT NULL DEFAULT 0,
    -- The last failure, so an operator reading the table can see why without
    -- correlating logs. Never rendered to a listener.
    last_error   text        NOT NULL DEFAULT ''
);

-- No secondary index on either table.
--
-- Both are read by their own leading key and by nothing else: album_tracks by
-- (album_id), which its primary key leads, and album_track_fetches by
-- (album_id), which is its primary key. That is the same reasoning 00011 gives
-- for spotify_top_snapshots and 00012 for user_playlists.
--
-- In particular there is no index supporting "every album whose listing is
-- stale". Nothing asks that question and nothing is meant to: §5.2 rejects a
-- background sweep explicitly, because enumerating albums nobody has opened
-- spends the instance's quota on questions nobody asked. The only reader is one
-- album's own page view, by primary key.

-- +goose Down
DROP TABLE album_track_fetches;
DROP TABLE album_tracks;
```

- [ ] **Step 2: Add both tables to the test harness**

In `test/harness/harness.go`, `truncatedTables`, insert **above** `"tracks"` and `"albums"` — the slice is ordered child-before-parent:

```go
	"spotify_top_snapshots",
	"user_playlists",
	"album_track_fetches",
	"album_tracks",
	"track_aliases",
```

- [ ] **Step 3: Apply, roll back, re-apply**

```bash
export ENCORE_DATABASE_URL="postgres://encore:encore@localhost:5433/encore?sslmode=disable"
go run ./cmd/encore-migrate up
go run ./cmd/encore-migrate status
go run ./cmd/encore-migrate reset --yes
go run ./cmd/encore-migrate up
```

Expected: four successes. The reset/up cycle is what CI runs, and a `Down` that does not work fails there rather than here.

**Fails when:** `Down` drops the tables in the wrong order, or `album_track_fetches` is dropped after `album_tracks` while a FK still references it.

- [ ] **Step 4: Run the integration suite unchanged**

```bash
ENCORE_TEST_DATABASE_URL="postgres://encore:encore@localhost:5433/encore?sslmode=disable" \
  go test -tags=integration -count=1 -p 1 -timeout=20m ./test/...
```

Expected: PASS. `TRUNCATE … CASCADE` naming a table that does not exist fails every integration test at once, so this proves step 2.

- [ ] **Step 5: Commit**

```bash
git add migrations/00013_album_tracks.sql test/harness/harness.go
git commit -m "Migrations: a place to keep an album's own track list"
```

Body: completion can count what you have heard but not name what you have not, because nothing on disk lists an album's tracks; the status table exists because no-rows is ambiguous three ways.

---

### Task 2: `spotify.AlbumTracks`

**Files:**
- Create: `internal/spotify/albumtracks.go`
- Create: `internal/spotify/albumtracks_test.go`

**Interfaces:**
- Consumes: `Client.getAsApp(ctx, path, label string, query url.Values, out any) error` (`catalog.go:105`); `validID(string) bool` (`catalog.go:156`); `maxLibraryPageSize = 50` and `ErrTruncated` (`library.go:15,35`).
- Produces:
  - `type AlbumTrack struct { ID, Name string; DiscNumber, TrackNumber int }`
  - `func (c *Client) AlbumTracks(ctx context.Context, albumID string, maxPages int) ([]AlbumTrack, error)`
  - `const defaultAlbumTrackPages = 20`

**Read `internal/spotify/library.go` in full first.** This is the same shape as `SavedTracks` and must report truncation identically: partial result *and* a wrapped `ErrTruncated`, never a partial with a nil error.

- [ ] **Step 1: Write the failing tests**

`internal/spotify/albumtracks_test.go`:

```go
package spotify

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

// albumTrackMux serves an album of n tracks, fifty a page, counting requests.
func albumTrackMux(albumID string, n int, calls *atomic.Int32) *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle("/api/token", appTokenHandler(nil))
	mux.HandleFunc("/v1/albums/"+albumID+"/tracks", func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))

		var b strings.Builder
		b.WriteString(`{"items":[`)
		count := 0
		for i := offset; i < n && i < offset+50; i++ {
			if count > 0 {
				b.WriteByte(',')
			}
			fmt.Fprintf(&b, `{"id":"track%04d0000000000000","name":"Song %d","disc_number":1,"track_number":%d}`,
				i, i+1, i+1)
			count++
		}
		b.WriteString(`]`)
		if offset+50 < n {
			fmt.Fprintf(&b, `,"next":"https://api.spotify.com/v1/albums/%s/tracks?offset=%d&limit=50"`,
				albumID, offset+50)
		} else {
			b.WriteString(`,"next":null`)
		}
		b.WriteString(`}`)
		_, _ = w.Write([]byte(b.String()))
	})
	return mux
}

// TestAlbumTracksFollowsEveryPage is the pagination guard. A 120-track album is
// three pages, and stopping after the first is the failure mode that would make
// the missing-track list quietly claim two thirds of a record was never played.
func TestAlbumTracksFollowsEveryPage(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(albumTrackMux("4aawyAB9vmqN3uQ7FjRGTy", 120, &calls))
	defer srv.Close()

	c := newTestClient(t, srv, newFakeClock())
	got, err := c.AlbumTracks(context.Background(), "4aawyAB9vmqN3uQ7FjRGTy", 0)
	if err != nil {
		t.Fatalf("AlbumTracks: %v", err)
	}
	if len(got) != 120 {
		t.Fatalf("got %d tracks, want 120", len(got))
	}
	if n := calls.Load(); n != 3 {
		t.Fatalf("made %d requests, want 3 (50 + 50 + 20)", n)
	}
	if got[0].Name != "Song 1" || got[119].Name != "Song 120" {
		t.Fatalf("first/last are %q/%q, want the first and last of the album",
			got[0].Name, got[119].Name)
	}
	if got[0].TrackNumber != 1 || got[0].DiscNumber != 1 {
		t.Fatalf("first track is disc %d track %d, want disc 1 track 1",
			got[0].DiscNumber, got[0].TrackNumber)
	}
}

// TestAlbumTracksReportsTruncation pins the property that keeps a partial
// listing away from a delete-absent write.
func TestAlbumTracksReportsTruncation(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(albumTrackMux("4aawyAB9vmqN3uQ7FjRGTy", 120, &calls))
	defer srv.Close()

	c := newTestClient(t, srv, newFakeClock())
	got, err := c.AlbumTracks(context.Background(), "4aawyAB9vmqN3uQ7FjRGTy", 1)
	if !errors.Is(err, ErrTruncated) {
		t.Fatalf("error = %v, want one wrapping ErrTruncated", err)
	}
	if len(got) != 50 {
		t.Fatalf("got %d tracks with the partial result, want the 50 already read", len(got))
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("made %d requests, want 1 under a one-page budget", n)
	}
}

// TestAlbumTracksStopsOnAnEmptyPage guards the loop's other exit. A page with no
// items must end the walk even if `next` is present, or a misbehaving upstream
// pages to the budget on every album it serves.
func TestAlbumTracksStopsOnAnEmptyPage(t *testing.T) {
	var calls atomic.Int32
	mux := http.NewServeMux()
	mux.Handle("/api/token", appTokenHandler(nil))
	mux.HandleFunc("/v1/albums/4aawyAB9vmqN3uQ7FjRGTy/tracks", func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte(`{"items":[],"next":"https://api.spotify.com/v1/albums/x/tracks?offset=50"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := newTestClient(t, srv, newFakeClock())
	got, err := c.AlbumTracks(context.Background(), "4aawyAB9vmqN3uQ7FjRGTy", 0)
	if err != nil {
		t.Fatalf("AlbumTracks: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %d tracks from an empty page, want 0", len(got))
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("made %d requests, want 1: an empty page ends the walk", n)
	}
}

// TestAlbumTracksSkipsItemsWithNoID keeps a local or unplayable entry out of the
// listing. It has no id to compare against a listen, so it is not something this
// listing can say anything true about.
func TestAlbumTracksSkipsItemsWithNoID(t *testing.T) {
	mux := http.NewServeMux()
	mux.Handle("/api/token", appTokenHandler(nil))
	mux.HandleFunc("/v1/albums/4aawyAB9vmqN3uQ7FjRGTy/tracks", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"items":[
			{"id":null,"name":"A local file","disc_number":1,"track_number":1},
			{"id":"5aawyAB9vmqN3uQ7FjRGTy","name":"Real","disc_number":1,"track_number":2}
		],"next":null}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := newTestClient(t, srv, newFakeClock())
	got, err := c.AlbumTracks(context.Background(), "4aawyAB9vmqN3uQ7FjRGTy", 0)
	if err != nil {
		t.Fatalf("AlbumTracks: %v", err)
	}
	if len(got) != 1 || got[0].ID != "5aawyAB9vmqN3uQ7FjRGTy" {
		t.Fatalf("got %+v, want only the entry that has an id", got)
	}
}

// TestAlbumTracksRefusesANonSpotifyID keeps a locally minted id out of a URL
// path. Unlike the batch endpoints it cannot be filtered by cleanIDs, because
// the id is the path rather than a parameter.
func TestAlbumTracksRefusesANonSpotifyID(t *testing.T) {
	var calls atomic.Int32
	mux := http.NewServeMux()
	mux.Handle("/api/token", appTokenHandler(&calls))
	mux.HandleFunc("/", func(http.ResponseWriter, *http.Request) { calls.Add(1) })
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := newTestClient(t, srv, newFakeClock())
	if _, err := c.AlbumTracks(context.Background(), "local:album:x/../../v1/me", 0); err == nil {
		t.Fatal("AlbumTracks accepted an id that is not a Spotify id")
	}
	if n := calls.Load(); n != 0 {
		t.Fatalf("made %d requests for a malformed id, want 0", n)
	}
}

// TestAlbumTracksSurfacesANotFound keeps "Spotify does not have this album"
// distinguishable from "the request failed", so the caller can log it usefully.
func TestAlbumTracksSurfacesANotFound(t *testing.T) {
	mux := http.NewServeMux()
	mux.Handle("/api/token", appTokenHandler(nil))
	mux.HandleFunc("/v1/albums/4aawyAB9vmqN3uQ7FjRGTy/tracks", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"status":404,"message":"non existing id"}}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := newTestClient(t, srv, newFakeClock())
	_, err := c.AlbumTracks(context.Background(), "4aawyAB9vmqN3uQ7FjRGTy", 0)
	apiErr, ok := AsAPIError(err)
	if !ok || !apiErr.IsNotFound() {
		t.Fatalf("error = %v, want an *APIError with IsNotFound()", err)
	}
}
```

**Fails when:**
- *FollowsEveryPage* — the loop returns after the first page, or ignores `next`, or sends the same `offset` every time.
- *ReportsTruncation* — a budget-exhausted walk returns a nil error, or drops the partial result, or ignores `maxPages`.
- *StopsOnAnEmptyPage* — the exit condition checks only `next` and not `len(items)`.
- *SkipsItemsWithNoID* — a null id becomes an empty-string row that can never match a listen and so shows for ever as "never played".
- *RefusesANonSpotifyID* — the id is interpolated into the path unvalidated (path traversal, and a guaranteed 400 for every local album id an import minted).
- *SurfacesANotFound* — the error is flattened to `errors.New` and the status is lost.

- [ ] **Step 2: Run them and watch them fail**

```
export PATH="$PATH:$(go env GOPATH)/bin"
go test -count=1 -run TestAlbumTracks ./internal/spotify/
```
Expected: FAIL — `c.AlbumTracks undefined`.

- [ ] **Step 3: Implement**

`internal/spotify/albumtracks.go`:

```go
package spotify

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// defaultAlbumTrackPages bounds one album's listing when the caller does not
// say. Fifty a page, so twenty pages is a thousand tracks — longer than any
// released album — and short enough that a paging bug cannot spend the
// instance's whole quota on a single record.
const defaultAlbumTrackPages = 20

// AlbumTrack is one entry of an album's own track listing.
//
// Spotify answers /v1/albums/{id}/tracks with "simplified" track objects: no
// album, no popularity, no ISRC. Everything needed to name a track nobody has
// ever played is present, which is the whole purpose of reading it.
type AlbumTrack struct {
	ID          string
	Name        string
	DiscNumber  int
	TrackNumber int
}

// albumTrackPage is one response from /v1/albums/{id}/tracks.
type albumTrackPage struct {
	Items []struct {
		ID          string `json:"id"`
		Name        string `json:"name"`
		DiscNumber  int    `json:"disc_number"`
		TrackNumber int    `json:"track_number"`
	} `json:"items"`
	Next string `json:"next"`
}

// AlbumTracks reads every track Spotify lists for one album.
//
// Offset paginated at fifty a page, the same shape as SavedTracks, and it
// reports truncation the same way: a page budget spent while Spotify still had
// a next page returns the pages already read *alongside* ErrTruncated. The
// partial listing is real data, but it is not the whole listing, and a caller
// that replaces a stored set from it deletes the tail. See ErrTruncated's own
// comment for why that has to be the caller's problem rather than silently
// handled here.
//
// It reads with the application token rather than a listener's: an album's
// track list is public catalogue data and needs no user scope, so one instance
// makes one request for an album however many of its users open it.
//
// No market parameter is sent, so the ids are Spotify's canonical ones rather
// than relinked to a market. A listener whose play was recorded under a
// relinked id will therefore see that track listed as never played; that is a
// known limitation, documented in docs/api.md, not something to paper over by
// guessing at equivalences.
func (c *Client) AlbumTracks(ctx context.Context, albumID string, maxPages int) ([]AlbumTrack, error) {
	id := strings.TrimSpace(albumID)
	if !validID(id) {
		// The id becomes part of the request path rather than a query parameter,
		// so a malformed one must be refused here rather than sent. Ids Encore
		// minted locally from an export's names (domain.LocalAlbumID) land here
		// too, and there is no album on Spotify for them to ask about.
		return nil, fmt.Errorf("spotify: album tracks: %q is not a spotify album id", albumID)
	}

	path := "/v1/albums/" + id + "/tracks"
	var out []AlbumTrack
	for page := range albumTrackBudget(maxPages) {
		q := url.Values{}
		q.Set("limit", strconv.Itoa(maxLibraryPageSize))
		q.Set("offset", strconv.Itoa(page*maxLibraryPageSize))

		var p albumTrackPage
		if err := c.getAsApp(ctx, path, "get album tracks", q, &p); err != nil {
			return nil, fmt.Errorf("spotify: album tracks: %w", err)
		}
		for _, item := range p.Items {
			// A null or empty id is a local file, or a track Spotify will not
			// serve. It has no id to compare against a listen, so it is not
			// something this listing can say anything true about.
			if item.ID == "" {
				continue
			}
			out = append(out, AlbumTrack{
				ID:          item.ID,
				Name:        item.Name,
				DiscNumber:  item.DiscNumber,
				TrackNumber: item.TrackNumber,
			})
		}
		if len(p.Items) == 0 || strings.TrimSpace(p.Next) == "" {
			return out, nil
		}
	}
	// Every page read was full and still pointed at a next one: the budget ran
	// out before the album did, so out is a prefix, not the whole thing.
	return out, fmt.Errorf("spotify: album tracks: %w", ErrTruncated)
}

// albumTrackBudget clamps a caller's page limit, mirroring pageBudget.
func albumTrackBudget(maxPages int) int {
	if maxPages <= 0 {
		return defaultAlbumTrackPages
	}
	return maxPages
}
```

- [ ] **Step 4: Run the package's whole suite**

```
go test -count=1 ./internal/spotify/
gofmt -l internal/spotify/; go vet ./internal/spotify/; staticcheck ./internal/spotify/
```
Expected: PASS, no output from the three linters.

- [ ] **Step 5: Commit**

```bash
git add internal/spotify/albumtracks.go internal/spotify/albumtracks_test.go
git commit -m "Spotify: read an album's own track list"
```

Body: needed to name the tracks a listener has not played; app token because the endpoint needs no user scope; truncation is reported as `ErrTruncated` because the caller reconciles delete-absent.

---

### Task 3: The store

**Files:**
- Create: `internal/store/catalog/albumtracks.go`
- Create: `test/integration/albumtracks_test.go`

**Interfaces:**
- Consumes: `catalog.Repo` (`internal/store/catalog/catalog.go:24`); `store.Querier`; `postgres.Classify(op string, err error) error`, which maps `pgx.ErrNoRows` to `domain.ErrNotFound`; the tables from Task 1.
- Produces, all methods on `*catalog.Repo`, all taking the caller's `store.Querier`:
  - `type AlbumTrack struct { TrackID, Name string; DiscNumber, TrackNumber int }`
  - `type AlbumTrackState struct { Status string; FetchedAt, AttemptedAt time.Time; Attempts int }`
  - `const (AlbumTrackFetching = "fetching"; AlbumTrackOK = "ok"; AlbumTrackFailed = "failed")`
  - `func (r *Repo) AlbumTrackState(ctx context.Context, q store.Querier, albumID string) (AlbumTrackState, error)` — a zero value with `Status == ""` when never attempted, **not** `domain.ErrNotFound`
  - `func (r *Repo) AlbumTracks(ctx context.Context, q store.Querier, albumID string) ([]AlbumTrack, error)`
  - `func (r *Repo) ClaimAlbumTrackFetch(ctx context.Context, q store.Querier, albumID string, now, leaseCutoff time.Time) (bool, error)`
  - `func (r *Repo) ReplaceAlbumTracks(ctx context.Context, q store.Querier, albumID string, items []AlbumTrack) error`
  - `func (r *Repo) MarkAlbumTracksFetched(ctx context.Context, q store.Querier, albumID string, at time.Time) error`
  - `func (r *Repo) FailAlbumTrackFetch(ctx context.Context, q store.Querier, albumID string, at time.Time, reason string) error`

**Read `internal/store/library/playlists.go` before writing `ReplaceAlbumTracks`.** It is the same delete-absent-plus-upsert-present shape, including the `DISTINCT ON` guard that stops `ON CONFLICT` touching one row twice in a single statement.

- [ ] **Step 1: Write the failing integration tests**

`test/integration/albumtracks_test.go`:

```go
//go:build integration

package integration

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/RequiDev/encore/internal/domain"
	"github.com/RequiDev/encore/internal/store/catalog"
	"github.com/RequiDev/encore/test/harness"
)

// seedAlbum puts one album in the catalogue so the foreign keys are satisfiable.
func seedAlbum(t *testing.T, env *harness.Env, id string) {
	t.Helper()
	ctx := context.Background()
	if _, err := env.Store.DB().Exec(ctx,
		`INSERT INTO albums (id, name, total_tracks) VALUES ($1, 'A Test Record', 12)`, id); err != nil {
		t.Fatalf("seed album: %v", err)
	}
}

func TestAlbumTrackStateIsEmptyBeforeAnyAttempt(t *testing.T) {
	env := harness.NewEnv(t)
	seedAlbum(t, env, "album000000000000000001")

	st, err := env.Catalog.AlbumTrackState(context.Background(), env.Store.DB(), "album000000000000000001")
	if err != nil {
		t.Fatalf("AlbumTrackState: %v", err)
	}
	if st.Status != "" {
		t.Fatalf("status = %q, want the empty string for an album never attempted", st.Status)
	}
	if !st.FetchedAt.IsZero() {
		t.Fatalf("fetchedAt = %v, want the zero time", st.FetchedAt)
	}
}

func TestClaimAlbumTrackFetchIsExclusive(t *testing.T) {
	env := harness.NewEnv(t)
	ctx := context.Background()
	seedAlbum(t, env, "album000000000000000001")

	now := time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC)
	cutoff := now.Add(-2 * time.Minute)

	first, err := env.Catalog.ClaimAlbumTrackFetch(ctx, env.Store.DB(), "album000000000000000001", now, cutoff)
	if err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if !first {
		t.Fatal("the first claim lost; nothing was holding the lease")
	}

	// The second tab, a second later.
	second, err := env.Catalog.ClaimAlbumTrackFetch(ctx, env.Store.DB(), "album000000000000000001",
		now.Add(time.Second), cutoff.Add(time.Second))
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	if second {
		t.Fatal("the second claim won as well; two tabs would each spend a Spotify request")
	}
}

func TestClaimAlbumTrackFetchReclaimsAnExpiredLease(t *testing.T) {
	env := harness.NewEnv(t)
	ctx := context.Background()
	seedAlbum(t, env, "album000000000000000001")

	start := time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC)
	if _, err := env.Catalog.ClaimAlbumTrackFetch(ctx, env.Store.DB(), "album000000000000000001",
		start, start.Add(-2*time.Minute)); err != nil {
		t.Fatalf("first claim: %v", err)
	}

	// Ten minutes later the process that held it is long dead.
	later := start.Add(10 * time.Minute)
	got, err := env.Catalog.ClaimAlbumTrackFetch(ctx, env.Store.DB(), "album000000000000000001",
		later, later.Add(-2*time.Minute))
	if err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	if !got {
		t.Fatal("an expired lease was not reclaimed; the album is stranded in 'fetching' for ever")
	}

	st, err := env.Catalog.AlbumTrackState(ctx, env.Store.DB(), "album000000000000000001")
	if err != nil {
		t.Fatalf("AlbumTrackState: %v", err)
	}
	if st.Attempts != 2 {
		t.Fatalf("attempts = %d, want 2", st.Attempts)
	}
}

func TestReplaceAlbumTracksDeletesWhatIsAbsent(t *testing.T) {
	env := harness.NewEnv(t)
	ctx := context.Background()
	seedAlbum(t, env, "album000000000000000001")

	before := []catalog.AlbumTrack{
		{TrackID: "track00000000000000001", Name: "One", DiscNumber: 1, TrackNumber: 1},
		{TrackID: "track00000000000000002", Name: "Two", DiscNumber: 1, TrackNumber: 2},
		{TrackID: "track00000000000000003", Name: "Three", DiscNumber: 1, TrackNumber: 3},
	}
	if err := env.Catalog.ReplaceAlbumTracks(ctx, env.Store.DB(), "album000000000000000001", before); err != nil {
		t.Fatalf("first replace: %v", err)
	}

	// The re-issue dropped one track and renamed another. Deliberately not the
	// same input twice: re-submitting an identical set would exercise nothing.
	after := []catalog.AlbumTrack{
		{TrackID: "track00000000000000001", Name: "One (Remastered)", DiscNumber: 1, TrackNumber: 1},
		{TrackID: "track00000000000000003", Name: "Three", DiscNumber: 1, TrackNumber: 2},
	}
	if err := env.Catalog.ReplaceAlbumTracks(ctx, env.Store.DB(), "album000000000000000001", after); err != nil {
		t.Fatalf("second replace: %v", err)
	}

	got, err := env.Catalog.AlbumTracks(ctx, env.Store.DB(), "album000000000000000001")
	if err != nil {
		t.Fatalf("AlbumTracks: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d rows, want 2: the track absent from the second listing was not deleted", len(got))
	}
	if got[0].Name != "One (Remastered)" {
		t.Fatalf("first row name = %q, want the renamed one: ON CONFLICT did not refresh it", got[0].Name)
	}
	if got[1].TrackID != "track00000000000000003" || got[1].TrackNumber != 2 {
		t.Fatalf("second row = %+v, want track 3 renumbered to 2", got[1])
	}
}

func TestAlbumTracksComeBackInDiscAndTrackOrder(t *testing.T) {
	env := harness.NewEnv(t)
	ctx := context.Background()
	seedAlbum(t, env, "album000000000000000001")

	// Inserted deliberately out of order, and with ids whose lexical order is the
	// reverse of the playing order, so an ORDER BY on the key would fail this.
	in := []catalog.AlbumTrack{
		{TrackID: "track00000000000000009", Name: "Disc two, one", DiscNumber: 2, TrackNumber: 1},
		{TrackID: "track00000000000000005", Name: "Disc one, two", DiscNumber: 1, TrackNumber: 2},
		{TrackID: "track00000000000000007", Name: "Disc one, one", DiscNumber: 1, TrackNumber: 1},
	}
	if err := env.Catalog.ReplaceAlbumTracks(ctx, env.Store.DB(), "album000000000000000001", in); err != nil {
		t.Fatalf("ReplaceAlbumTracks: %v", err)
	}

	got, err := env.Catalog.AlbumTracks(ctx, env.Store.DB(), "album000000000000000001")
	if err != nil {
		t.Fatalf("AlbumTracks: %v", err)
	}
	want := []string{"Disc one, one", "Disc one, two", "Disc two, one"}
	for i, name := range want {
		if got[i].Name != name {
			t.Fatalf("row %d is %q, want %q — the listing is not in disc and track order", i, got[i].Name, name)
		}
	}
}

func TestFailAlbumTrackFetchKeepsTheOlderListing(t *testing.T) {
	env := harness.NewEnv(t)
	ctx := context.Background()
	seedAlbum(t, env, "album000000000000000001")

	at := time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC)
	if err := env.Catalog.ReplaceAlbumTracks(ctx, env.Store.DB(), "album000000000000000001",
		[]catalog.AlbumTrack{{TrackID: "track00000000000000001", Name: "One", DiscNumber: 1, TrackNumber: 1}},
	); err != nil {
		t.Fatalf("ReplaceAlbumTracks: %v", err)
	}
	if err := env.Catalog.MarkAlbumTracksFetched(ctx, env.Store.DB(), "album000000000000000001", at); err != nil {
		t.Fatalf("MarkAlbumTracksFetched: %v", err)
	}

	later := at.Add(31 * 24 * time.Hour)
	if err := env.Catalog.FailAlbumTrackFetch(ctx, env.Store.DB(), "album000000000000000001",
		later, "spotify: album tracks: context deadline exceeded"); err != nil {
		t.Fatalf("FailAlbumTrackFetch: %v", err)
	}

	rows, err := env.Catalog.AlbumTracks(ctx, env.Store.DB(), "album000000000000000001")
	if err != nil {
		t.Fatalf("AlbumTracks: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows after a failure, want the 1 that was already stored", len(rows))
	}

	st, err := env.Catalog.AlbumTrackState(ctx, env.Store.DB(), "album000000000000000001")
	if err != nil {
		t.Fatalf("AlbumTrackState: %v", err)
	}
	if st.Status != catalog.AlbumTrackFailed {
		t.Fatalf("status = %q, want %q", st.Status, catalog.AlbumTrackFailed)
	}
	if !st.FetchedAt.Equal(at) {
		t.Fatalf("fetchedAt = %v, want it untouched at %v: a failure erased when the good listing was read",
			st.FetchedAt, at)
	}
}

func TestAlbumTracksCascadeWithTheAlbum(t *testing.T) {
	env := harness.NewEnv(t)
	ctx := context.Background()
	seedAlbum(t, env, "album000000000000000001")
	if err := env.Catalog.ReplaceAlbumTracks(ctx, env.Store.DB(), "album000000000000000001",
		[]catalog.AlbumTrack{{TrackID: "track00000000000000001", Name: "One", DiscNumber: 1, TrackNumber: 1}},
	); err != nil {
		t.Fatalf("ReplaceAlbumTracks: %v", err)
	}
	if _, err := env.Store.DB().Exec(ctx, `DELETE FROM albums WHERE id = $1`, "album000000000000000001"); err != nil {
		t.Fatalf("delete album: %v", err)
	}

	var n int
	if err := env.Store.DB().QueryRow(ctx,
		`SELECT count(*)::int FROM album_tracks WHERE album_id = $1`, "album000000000000000001").Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("%d album_tracks rows survived their album, want 0", n)
	}
	_ = errors.Is(nil, domain.ErrNotFound) // keeps the domain import honest if the file is trimmed
}
```

**Fails when:**
- *StateIsEmptyBeforeAnyAttempt* — `AlbumTrackState` returns `domain.ErrNotFound` for a never-attempted album, which would make every first page view a 500.
- *ClaimIsExclusive* — the claim is an unconditional upsert (no `WHERE` on the `DO UPDATE` branch), so two tabs each fetch.
- *ReclaimsAnExpiredLease* — the `WHERE` omits the `attempted_at < cutoff` disjunct, so a killed process strands the album and the browser polls for ever.
- *DeletesWhatIsAbsent* — the write is upsert-only with no delete, or `ON CONFLICT` only touches a timestamp so a rename never lands.
- *ComeBackInDiscAndTrackOrder* — the `ORDER BY` is missing or on the primary key; the ids are chosen so key order is not playing order.
- *FailKeepsTheOlderListing* — `FailAlbumTrackFetch` deletes rows or clears `fetched_at`, turning one timeout into a permanently lost listing.
- *CascadeWithTheAlbum* — the foreign key is missing, leaving orphans behind a deleted album.

- [ ] **Step 2: Run them and watch them fail**

```
ENCORE_TEST_DATABASE_URL="postgres://encore:encore@localhost:5433/encore?sslmode=disable" \
  go test -tags=integration -count=1 -p 1 -run 'TestAlbumTrack|TestClaimAlbumTrack|TestReplaceAlbumTracks|TestFailAlbumTrack' ./test/integration/
```
Expected: FAIL to compile — the methods do not exist.

- [ ] **Step 3: Implement**

`internal/store/catalog/albumtracks.go`:

```go
package catalog

import (
	"context"
	"errors"
	"time"

	"github.com/RequiDev/encore/internal/domain"
	"github.com/RequiDev/encore/internal/postgres"
	"github.com/RequiDev/encore/internal/store"
)

// The three states of one album's listing. See
// migrations/00013_album_tracks.sql for why the outcome lives in its own table
// rather than being inferred from whether album_tracks holds rows.
const (
	// AlbumTrackFetching is a lease: somebody is reading this listing now.
	AlbumTrackFetching = "fetching"
	// AlbumTrackOK means album_tracks holds a complete listing.
	AlbumTrackOK = "ok"
	// AlbumTrackFailed means the last attempt did not produce one. Whatever
	// album_tracks holds is from an earlier, successful attempt.
	AlbumTrackFailed = "failed"
)

// AlbumTrack is one row of a cached listing.
type AlbumTrack struct {
	TrackID     string
	Name        string
	DiscNumber  int
	TrackNumber int
}

// AlbumTrackState is the bookkeeping for one album's listing.
//
// The zero value — Status "" and both instants zero — is an album that has
// never been attempted, which is an ordinary state rather than an error: every
// album is in it until somebody first opens its page.
type AlbumTrackState struct {
	Status      string
	FetchedAt   time.Time
	AttemptedAt time.Time
	Attempts    int
}

const albumTrackStateSQL = `
SELECT status, coalesce(fetched_at, 'epoch'::timestamptz), attempted_at, attempts
FROM album_track_fetches
WHERE album_id = $1`

// AlbumTrackState reads the outcome of the last attempt on one album.
func (r *Repo) AlbumTrackState(ctx context.Context, q store.Querier, albumID string) (AlbumTrackState, error) {
	var (
		out     AlbumTrackState
		fetched time.Time
	)
	err := q.QueryRow(ctx, albumTrackStateSQL, albumID).
		Scan(&out.Status, &fetched, &out.AttemptedAt, &out.Attempts)
	if err != nil {
		if errors.Is(postgres.Classify("album track state", err), domain.ErrNotFound) {
			// Never attempted. Not an error: it is the state every album starts in,
			// and the caller's cue to start the first fetch.
			return AlbumTrackState{}, nil
		}
		return AlbumTrackState{}, postgres.Classify("album track state", err)
	}
	// 'epoch' stands in for NULL so the scan needs no pointer. Anything at or
	// before it means "no successful fetch yet".
	if fetched.Year() > 1970 {
		out.FetchedAt = fetched.UTC()
	}
	out.AttemptedAt = out.AttemptedAt.UTC()
	return out, nil
}

// albumTracksSQL reads one album's listing in playing order.
//
// track_id breaks ties so the order is total: two rows sharing a disc and track
// number would otherwise come back in whatever order the heap happened to hold
// them, and a list that reshuffles between page views looks broken.
const albumTracksSQL = `
SELECT track_id, name, disc_number, track_number
FROM album_tracks
WHERE album_id = $1
ORDER BY disc_number, track_number, track_id`

// AlbumTracks reads one album's cached listing.
func (r *Repo) AlbumTracks(ctx context.Context, q store.Querier, albumID string) ([]AlbumTrack, error) {
	rows, err := q.Query(ctx, albumTracksSQL, albumID)
	if err != nil {
		return nil, postgres.Classify("album tracks", err)
	}
	defer rows.Close()

	out := make([]AlbumTrack, 0, 16)
	for rows.Next() {
		var t AlbumTrack
		if err := rows.Scan(&t.TrackID, &t.Name, &t.DiscNumber, &t.TrackNumber); err != nil {
			return nil, postgres.Classify("album tracks", err)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, postgres.Classify("album tracks", err)
	}
	return out, nil
}

// claimAlbumTrackFetchSQL takes the lease on one album, or returns nothing.
//
// The WHERE applies only to the DO UPDATE branch: an album with no row at all
// is claimed by the INSERT. An album already 'fetching' is claimed only once
// its attempt has outlived the lease, which is what stops two tabs, two
// browsers or two API replicas each spending a request on the same album — and
// what stops a process killed mid-fetch stranding the album for ever.
//
// Parameters are $1 album, $2 now, $3 the lease cutoff (now minus the lease).
const claimAlbumTrackFetchSQL = `
INSERT INTO album_track_fetches (album_id, status, attempted_at, attempts)
VALUES ($1, 'fetching', $2, 1)
ON CONFLICT (album_id) DO UPDATE SET
    status       = 'fetching',
    attempted_at = $2,
    attempts     = album_track_fetches.attempts + 1
WHERE album_track_fetches.status <> 'fetching'
   OR album_track_fetches.attempted_at < $3
RETURNING album_id`

// ClaimAlbumTrackFetch takes the lease, reporting whether this caller won it.
func (r *Repo) ClaimAlbumTrackFetch(
	ctx context.Context, q store.Querier, albumID string, now, leaseCutoff time.Time,
) (bool, error) {
	var got string
	err := q.QueryRow(ctx, claimAlbumTrackFetchSQL, albumID, now.UTC(), leaseCutoff.UTC()).Scan(&got)
	if err != nil {
		classified := postgres.Classify("claim album track fetch", err)
		if errors.Is(classified, domain.ErrNotFound) {
			// No row came back: somebody else holds a live lease.
			return false, nil
		}
		return false, classified
	}
	return true, nil
}

// replaceAlbumTracksSQL deletes whatever the incoming listing no longer
// contains and upserts the rest, in one statement — the same
// delete-absent-plus-upsert-present shape as ReplaceUserPlaylists in
// internal/store/library/playlists.go. Every column besides the key can change
// under a track Encore already knows about (a remaster renames it, a re-issue
// renumbers it), so ON CONFLICT refreshes all of them.
//
// DISTINCT ON collapses a duplicate id within one call, because Postgres
// refuses to let ON CONFLICT touch the same row twice inside one statement and
// a page boundary could in principle repeat one.
//
// **Callers must never pass a partial listing here.** The delete is what makes
// that fatal: a prefix deletes the tail of a listing that was correct.
//
// Parameters are $1 album, $2..$5 the parallel arrays.
const replaceAlbumTracksSQL = `
WITH input AS (
    SELECT DISTINCT ON (track_id) *
    FROM unnest($2::text[], $3::text[], $4::int[], $5::int[])
        AS t(track_id, name, disc_number, track_number)
    ORDER BY track_id
),
stale AS (
    DELETE FROM album_tracks
    WHERE album_id = $1 AND track_id <> ALL($2::text[])
)
INSERT INTO album_tracks (album_id, track_id, name, disc_number, track_number)
SELECT $1, track_id, name, disc_number, track_number FROM input
ON CONFLICT (album_id, track_id) DO UPDATE SET
    name         = EXCLUDED.name,
    disc_number  = EXCLUDED.disc_number,
    track_number = EXCLUDED.track_number`

// ReplaceAlbumTracks makes items the album's complete listing.
func (r *Repo) ReplaceAlbumTracks(
	ctx context.Context, q store.Querier, albumID string, items []AlbumTrack,
) error {
	ids, names, discs, numbers := albumTrackRows(items)
	if _, err := q.Exec(ctx, replaceAlbumTracksSQL, albumID, ids, names, discs, numbers); err != nil {
		return postgres.Classify("replace album tracks", err)
	}
	return nil
}

// albumTrackRows transposes a listing into the parallel arrays unnest expects,
// dropping entries with a blank id — a keyless row has nothing for ON CONFLICT
// to place. Every slice is non-nil even when items is empty, so an empty batch
// reaches the statement as an empty array rather than SQL NULL.
func albumTrackRows(items []AlbumTrack) (ids, names []string, discs, numbers []int32) {
	ids = make([]string, 0, len(items))
	names = make([]string, 0, len(items))
	discs = make([]int32, 0, len(items))
	numbers = make([]int32, 0, len(items))
	for _, it := range items {
		if it.TrackID == "" {
			continue
		}
		ids = append(ids, it.TrackID)
		names = append(names, it.Name)
		discs = append(discs, int32(it.DiscNumber))
		numbers = append(numbers, int32(it.TrackNumber))
	}
	return ids, names, discs, numbers
}

// markAlbumTracksFetchedSQL records a success. It clears last_error so a stale
// message cannot be read beside a listing that is now current.
const markAlbumTracksFetchedSQL = `
INSERT INTO album_track_fetches (album_id, status, fetched_at, attempted_at, attempts, last_error)
VALUES ($1, 'ok', $2, $2, 1, '')
ON CONFLICT (album_id) DO UPDATE SET
    status     = 'ok',
    fetched_at = $2,
    last_error = ''`

// MarkAlbumTracksFetched records that the listing now stored is complete.
//
// It is deliberately separate from ReplaceAlbumTracks rather than folded into
// it: the caller runs both inside one Store.InTx, so the listing and the claim
// that it is authoritative commit together or not at all.
func (r *Repo) MarkAlbumTracksFetched(
	ctx context.Context, q store.Querier, albumID string, at time.Time,
) error {
	if _, err := q.Exec(ctx, markAlbumTracksFetchedSQL, albumID, at.UTC()); err != nil {
		return postgres.Classify("mark album tracks fetched", err)
	}
	return nil
}

// failAlbumTrackFetchSQL records a failed attempt.
//
// It touches neither album_tracks nor fetched_at. Whatever listing is stored
// stays stored and keeps saying when it was read: a timeout today is no reason
// to throw away a listing that was correct last month, and an empty listing is
// exactly the "this album has no tracks" claim this feature must never make.
const failAlbumTrackFetchSQL = `
INSERT INTO album_track_fetches (album_id, status, attempted_at, attempts, last_error)
VALUES ($1, 'failed', $2, 1, $3)
ON CONFLICT (album_id) DO UPDATE SET
    status       = 'failed',
    attempted_at = $2,
    last_error   = $3`

// FailAlbumTrackFetch records that the last attempt did not produce a listing.
func (r *Repo) FailAlbumTrackFetch(
	ctx context.Context, q store.Querier, albumID string, at time.Time, reason string,
) error {
	if len(reason) > 500 {
		reason = reason[:500]
	}
	if _, err := q.Exec(ctx, failAlbumTrackFetchSQL, albumID, at.UTC(), reason); err != nil {
		return postgres.Classify("fail album track fetch", err)
	}
	return nil
}
```

- [ ] **Step 4: Run them and watch them pass**

```
ENCORE_TEST_DATABASE_URL="postgres://encore:encore@localhost:5433/encore?sslmode=disable" \
  go test -tags=integration -count=1 -p 1 -run 'AlbumTrack' ./test/integration/
gofmt -l internal/store/catalog/ test/integration/; go vet ./...; staticcheck ./...
```
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/store/catalog/albumtracks.go test/integration/albumtracks_test.go
git commit -m "Store: an album's track listing, and the outcome of reading it"
```

Body: the claim is a lease because two tabs must not each spend a request; the failure path touches neither the listing nor `fetched_at`, because a failure is not an empty album.

---

### Task 4: The service — policy and the detached fetch

**Files:**
- Create: `internal/albumtracks/albumtracks.go`
- Create: `internal/albumtracks/albumtracks_test.go`
- Modify: `internal/config/config.go` (new `AlbumTracks` struct, its parse block, `Redacted`)
- Modify: `internal/config/config_test.go` (two tests)
- Modify: `docker-compose.yml` (the `x-encore-env` anchor), `.env.example`, `docs/configuration.md`

**Interfaces:**
- Consumes: everything Task 3 produced; `spotify.AlbumTracks` and `spotify.ErrTruncated` from Task 2; `store.Store.InTx` and `store.Store.DB()`; `logging.Err(err)`.
- Produces:
  - `type State string` with `StateReady = "ready"`, `StatePending = "pending"`, `StateUnavailable = "unavailable"`
  - `type Track struct { ID, Name string; DiscNumber, TrackNumber int }`
  - `type Listing struct { State State; Tracks []Track; FetchedAt time.Time }`
  - `type Fetcher interface { AlbumTracks(ctx context.Context, albumID string, maxPages int) ([]spotify.AlbumTrack, error) }`
  - `type Deps struct { Store *store.Store; Catalog *catalog.Repo; Spotify Fetcher; Logger *slog.Logger; Now func() time.Time }`
  - `func New(cfg config.AlbumTracks, deps Deps) (*Service, error)`
  - `func (s *Service) Listing(ctx context.Context, q store.Querier, albumID string) (Listing, error)`
  - `func (s *Service) Close()`
  - `config.AlbumTracks{ Enabled bool; TTL time.Duration }`, read from `ENCORE_ALBUM_TRACKS_ENABLED` (default `true`) and `ENCORE_ALBUM_TRACKS_TTL` (default `720h`)
  - a fourth state: `StateDisabled = "disabled"`

- [ ] **Step 1: Add the configuration**

`internal/config/config.go` — the struct, beside `Library`:

```go
// AlbumTracks governs the cache of album track listings that lets the album
// page name the tracks somebody has never played.
//
// Unlike Library there is no worker and no interval: §5.2 rejects a background
// sweep explicitly, because most albums in a large history are never opened and
// enumerating them all would spend the instance's quota on questions nobody
// asked. A listing is read the first time somebody opens that album's page and
// then kept, so the cost is one request per album *viewed*, per TTL.
type AlbumTracks struct {
	// Enabled controls whether this instance ever asks Spotify what is on an
	// album. On by default, so the feature works out of the box.
	//
	// It has a switch at all because this is the one Spotify request Encore's
	// API makes that is not the direct consequence of somebody clicking a thing:
	// signing in, "sync now" and playlist creation are each an action a person
	// took, whereas this fires as a side effect of *viewing a page*. Unattended
	// egress is an operator's decision, the same judgement that made the
	// now-playing poller opt-in.
	//
	// Off means "do not fetch", not "forget what is on disk": a listing already
	// stored is still served, with the date it was read, and the album page says
	// plainly that this instance does not fetch them rather than reporting a
	// Spotify failure that did not happen.
	Enabled bool
	// TTL is how long a stored listing is trusted before the next page view
	// refreshes it. Ignored entirely when Enabled is false — nothing refreshes,
	// so nothing expires.
	//
	// Thirty days by default, and long on purpose: an album's track list is
	// effectively immutable once released, because Spotify mints a new album id
	// for a deluxe edition or a re-issue rather than changing the old one. The
	// cases a refresh does catch — a pre-release that gained tracks, a market
	// change — are all caught within a month, and a shorter TTL would multiply
	// the request count for no observable gain.
	TTL time.Duration
}
```

Add to `Config`: `AlbumTracks AlbumTracks`. Parse block, after `c.Library`:

```go
	c.AlbumTracks = AlbumTracks{
		Enabled: p.boolean("ENCORE_ALBUM_TRACKS_ENABLED", true),
		TTL:     p.duration("ENCORE_ALBUM_TRACKS_TTL", 30*24*time.Hour),
	}
```

`Redacted`, beside `"library_enabled"` and `"library_interval"` — both keys, because the startup log is the one line that says what this process believes its configuration to be, and "why is the album page saying it is turned off" is answerable from it or from nowhere:

```go
		"album_tracks_enabled": c.AlbumTracks.Enabled,
		"album_tracks_ttl":     c.AlbumTracks.TTL.String(),
```

`docker-compose.yml`, in the `x-encore-env` anchor after the `ENCORE_LIBRARY_SYNC_*` group:

```yaml
  ENCORE_ALBUM_TRACKS_ENABLED: ${ENCORE_ALBUM_TRACKS_ENABLED:-}
  ENCORE_ALBUM_TRACKS_TTL: ${ENCORE_ALBUM_TRACKS_TTL:-}
```

`.env.example`, after the library block. The library block above it states its own per-day request formula, so this one carries its own accounting — a different budget, from a different process, on a different trigger:

```
# ---------------------------------------------------------------------------
# Album track listings
# ---------------------------------------------------------------------------
# The album page names the tracks you have never played, which needs Spotify's
# own track list for that album. It is read the first time somebody opens that
# album's page and then cached, so one instance spends roughly
#   ceil(album_tracks/50)
# requests per album *viewed*, per TTL — one request for almost every album,
# and nothing at all for albums nobody opens. There is deliberately no
# background sweep: most albums in a large history are never looked at, and
# enumerating them all would spend the quota on questions nobody asked.
#
# This is the one Spotify request the API makes that nobody clicked for — it
# fires as a side effect of viewing a page — so it has a switch. Set to false
# for an encore-api that makes no unattended requests at all. Listings already
# cached are still shown when it is off, with the date they were read; only the
# fetching stops.
#ENCORE_ALBUM_TRACKS_ENABLED=true
# Thirty days is long on purpose: a released album's track list does not
# change, since a re-issue gets a new album id rather than editing the old one.
# Ignored when the above is false. Must be positive.
#ENCORE_ALBUM_TRACKS_TTL=720h
```

`docs/configuration.md`, two rows in the same table as `ENCORE_LIBRARY_SYNC_*`:

```
| `ENCORE_ALBUM_TRACKS_ENABLED` | `true` | Whether this instance asks Spotify what is on an album, which is what lets the album page name the tracks you have never played. It is the one Spotify request `encore-api` makes that nobody clicked for — signing in, "sync now" and playlist creation are each a direct consequence of a user's action, while this fires as a side effect of *viewing* an album page — so it is switchable, for the same reason the now-playing poller is opt-in: unattended egress is the operator's decision. A run costs roughly `ceil(album_tracks/50)` requests per album *viewed* per TTL, which is one request for almost every album, and nothing for albums nobody opens. Set to `false` and `encore-api` makes no unattended Spotify request at all. **Turning it off does not hide listings already cached** — those are still shown, with the date they were read; only fetching stops, and the album page says so plainly rather than reporting a Spotify failure that did not happen. |
| `ENCORE_ALBUM_TRACKS_TTL` | `720h` (30 days) | How long a cached album track listing is trusted before the next view of that album's page refreshes it. Long by default because a released album's track list does not change — Spotify mints a new album id for a re-issue rather than editing the old one — so a shorter TTL multiplies requests without making anything fresher. A failed fetch is retried after fifteen minutes rather than after this interval. Ignored when `ENCORE_ALBUM_TRACKS_ENABLED` is `false`: nothing refreshes, so nothing expires. Must be positive. |
```

`internal/config/config_test.go`:

```go
func TestAlbumTracksDefaults(t *testing.T) {
	cfg, err := LoadFrom(libraryTestEnv(nil))
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	want := AlbumTracks{Enabled: true, TTL: 30 * 24 * time.Hour}
	if cfg.AlbumTracks != want {
		t.Errorf("AlbumTracks = %+v, want %+v", cfg.AlbumTracks, want)
	}
}

// TestAlbumTracksCanBeTurnedOff is the operator's switch, round-tripped. The
// default is true, so a parser that never read the key at all would pass a test
// that only checked the default.
func TestAlbumTracksCanBeTurnedOff(t *testing.T) {
	cfg, err := LoadFrom(libraryTestEnv(map[string]string{
		"ENCORE_ALBUM_TRACKS_ENABLED": "false",
	}))
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	if cfg.AlbumTracks.Enabled {
		t.Error("AlbumTracks.Enabled = true after ENCORE_ALBUM_TRACKS_ENABLED=false")
	}
	if _, ok := cfg.Redacted()["album_tracks_enabled"]; !ok {
		t.Error(`Redacted() has no "album_tracks_enabled"; the startup log is the ` +
			`only place an operator can confirm the switch took effect`)
	}
}

// TestAlbumTracksTTLRejectsNonPositive matches every other duration: a zero TTL
// would refetch an album's listing on every single page view, which is a quota
// bill discovered in production rather than a configuration error at startup.
func TestAlbumTracksTTLRejectsNonPositive(t *testing.T) {
	_, err := LoadFrom(libraryTestEnv(map[string]string{"ENCORE_ALBUM_TRACKS_TTL": "0"}))
	if err == nil {
		t.Fatal("LoadFrom: want an error for a non-positive TTL, got nil")
	}
	if !strings.Contains(err.Error(), "ENCORE_ALBUM_TRACKS_TTL") {
		t.Errorf("error %q does not mention ENCORE_ALBUM_TRACKS_TTL", err)
	}
}
```

**Fails when:**
- *Defaults* — either key is never read, so `TTL` stays zero or `Enabled` stays false and the feature is off out of the box.
- *CanBeTurnedOff* — the parse block hard-codes `Enabled: true` instead of calling `p.boolean`, which the defaults test alone cannot catch; or `Redacted` omits the key, leaving an operator with no way to confirm the switch landed.
- *TTLRejectsNonPositive* — `p.str`/`p.intRange` is used instead of `p.duration`, so a zero is accepted.

- [ ] **Step 2: Run the configuration guards**

```
go test -count=1 ./internal/config/
go test -count=1 ./test/deploy/
```
Expected: PASS. `./test/deploy/` is the guard that fails when the key is read by `config.go` but missing from `docker-compose.yml` or `.env.example` — the failure mode where a documented setting silently does nothing.

- [ ] **Step 3: Write the failing service tests**

`internal/albumtracks/albumtracks_test.go`. These use a fake catalogue and a fake fetcher, so they run without a database.

```go
package albumtracks

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/RequiDev/encore/internal/config"
	"github.com/RequiDev/encore/internal/spotify"
	"github.com/RequiDev/encore/internal/store/catalog"
)

// fakeCatalog stands in for the two tables. It is deliberately not a database:
// these tests are about *when* a fetch is started, which is policy.
type fakeCatalog struct {
	mu      sync.Mutex
	state   catalog.AlbumTrackState
	tracks  []catalog.AlbumTrack
	claims  int
	writes  int
	fails   int
	claimOK bool
}

func (f *fakeCatalog) AlbumTrackState(context.Context, storeQuerier, string) (catalog.AlbumTrackState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state, nil
}

func (f *fakeCatalog) AlbumTracks(context.Context, storeQuerier, string) ([]catalog.AlbumTrack, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]catalog.AlbumTrack(nil), f.tracks...), nil
}

func (f *fakeCatalog) ClaimAlbumTrackFetch(_ context.Context, _ storeQuerier, _ string, _, _ time.Time) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.claims++
	return f.claimOK, nil
}

func (f *fakeCatalog) ReplaceAlbumTracks(_ context.Context, _ storeQuerier, _ string, items []catalog.AlbumTrack) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writes++
	f.tracks = append([]catalog.AlbumTrack(nil), items...)
	return nil
}

func (f *fakeCatalog) MarkAlbumTracksFetched(_ context.Context, _ storeQuerier, _ string, at time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.state = catalog.AlbumTrackState{Status: catalog.AlbumTrackOK, FetchedAt: at, AttemptedAt: at}
	return nil
}

func (f *fakeCatalog) FailAlbumTrackFetch(_ context.Context, _ storeQuerier, _ string, at time.Time, reason string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fails++
	f.state.Status = catalog.AlbumTrackFailed
	f.state.AttemptedAt = at
	f.state.Attempts++
	_ = reason
	return nil
}

func (f *fakeCatalog) counts() (claims, writes, fails int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.claims, f.writes, f.fails
}

// fakeFetcher answers with whatever the test set, and counts calls.
type fakeFetcher struct {
	mu     sync.Mutex
	tracks []spotify.AlbumTrack
	err    error
	calls  int
	block  chan struct{}
}

func (f *fakeFetcher) AlbumTracks(context.Context, string, int) ([]spotify.AlbumTrack, error) {
	f.mu.Lock()
	f.calls++
	block := f.block
	tracks, err := f.tracks, f.err
	f.mu.Unlock()
	if block != nil {
		<-block
	}
	return tracks, err
}

func (f *fakeFetcher) called() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func discard() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// newServiceWith builds a service on the given configuration. Close is
// deferred by the caller or by Cleanup; the fetch runs in a goroutine, so every
// assertion about it comes after an explicit s.Close().
func newServiceWith(t *testing.T, cfg config.AlbumTracks, cat *fakeCatalog, fetch *fakeFetcher, now time.Time) *Service {
	t.Helper()
	s, err := New(cfg, Deps{
		Catalog: cat,
		Spotify: fetch,
		Writer:  inlineWriter{cat: cat},
		Logger:  discard(),
		Now:     func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(s.Close)
	return s
}

// newService is the ordinary, enabled service.
func newService(t *testing.T, cat *fakeCatalog, fetch *fakeFetcher, now time.Time) *Service {
	t.Helper()
	return newServiceWith(t, config.AlbumTracks{Enabled: true, TTL: 30 * 24 * time.Hour}, cat, fetch, now)
}

// newDisabledService is an instance whose operator turned fetching off.
func newDisabledService(t *testing.T, cat *fakeCatalog, fetch *fakeFetcher, now time.Time) *Service {
	t.Helper()
	return newServiceWith(t, config.AlbumTracks{Enabled: false, TTL: 30 * 24 * time.Hour}, cat, fetch, now)
}

func TestFirstViewStartsTheFetchAndReportsPending(t *testing.T) {
	cat := &fakeCatalog{claimOK: true}
	fetch := &fakeFetcher{block: make(chan struct{})}
	now := time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC)
	s := newService(t, cat, fetch, now)

	got, err := s.Listing(context.Background(), nil, "album000000000000000001")
	if err != nil {
		t.Fatalf("Listing: %v", err)
	}
	if got.State != StatePending {
		t.Fatalf("state = %q, want %q: nothing is stored and a fetch has begun", got.State, StatePending)
	}
	if len(got.Tracks) != 0 {
		t.Fatalf("got %d tracks before any fetch finished", len(got.Tracks))
	}
	close(fetch.block)
	s.Close()
	if n := fetch.called(); n != 1 {
		t.Fatalf("fetcher called %d times, want 1", n)
	}
}

// TestListingDoesNotWaitForSpotify is the whole point of the design: a page
// request must answer while the fetch is still running.
func TestListingDoesNotWaitForSpotify(t *testing.T) {
	cat := &fakeCatalog{claimOK: true}
	block := make(chan struct{})
	fetch := &fakeFetcher{block: block}
	now := time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC)
	s := newService(t, cat, fetch, now)

	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := s.Listing(context.Background(), nil, "album000000000000000001"); err != nil {
			t.Errorf("Listing: %v", err)
		}
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Listing blocked on the Spotify call; the album page would hang")
	}
	close(block)
}

func TestAFreshListingIsNotRefetched(t *testing.T) {
	now := time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC)
	cat := &fakeCatalog{
		claimOK: true,
		state: catalog.AlbumTrackState{
			Status:      catalog.AlbumTrackOK,
			FetchedAt:   now.Add(-29 * 24 * time.Hour),
			AttemptedAt: now.Add(-29 * 24 * time.Hour),
		},
		tracks: []catalog.AlbumTrack{{TrackID: "t1", Name: "One", DiscNumber: 1, TrackNumber: 1}},
	}
	fetch := &fakeFetcher{}
	s := newService(t, cat, fetch, now)

	got, err := s.Listing(context.Background(), nil, "album000000000000000001")
	if err != nil {
		t.Fatalf("Listing: %v", err)
	}
	if got.State != StateReady {
		t.Fatalf("state = %q, want %q", got.State, StateReady)
	}
	s.Close()
	if n := fetch.called(); n != 0 {
		t.Fatalf("fetcher called %d times for a 29-day-old listing under a 30-day TTL, want 0", n)
	}
}

// TestAnExpiredListingIsRefetchedAndStillServed pins both halves of the TTL: it
// refetches, and it does not withhold the listing it already has while doing so.
func TestAnExpiredListingIsRefetchedAndStillServed(t *testing.T) {
	now := time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC)
	cat := &fakeCatalog{
		claimOK: true,
		state: catalog.AlbumTrackState{
			Status:      catalog.AlbumTrackOK,
			FetchedAt:   now.Add(-31 * 24 * time.Hour),
			AttemptedAt: now.Add(-31 * 24 * time.Hour),
		},
		tracks: []catalog.AlbumTrack{{TrackID: "t1", Name: "One", DiscNumber: 1, TrackNumber: 1}},
	}
	fetch := &fakeFetcher{tracks: []spotify.AlbumTrack{
		{ID: "t1", Name: "One", DiscNumber: 1, TrackNumber: 1},
		{ID: "t2", Name: "Two", DiscNumber: 1, TrackNumber: 2},
	}}
	s := newService(t, cat, fetch, now)

	got, err := s.Listing(context.Background(), nil, "album000000000000000001")
	if err != nil {
		t.Fatalf("Listing: %v", err)
	}
	if got.State != StateReady || len(got.Tracks) != 1 {
		t.Fatalf("state/tracks = %q/%d, want %q with the stored listing still served",
			got.State, len(got.Tracks), StateReady)
	}
	s.Close()
	if n := fetch.called(); n != 1 {
		t.Fatalf("fetcher called %d times for a 31-day-old listing under a 30-day TTL, want 1", n)
	}
	if _, writes, _ := cat.counts(); writes != 1 {
		t.Fatalf("wrote %d times, want 1", writes)
	}
}

// TestATruncatedFetchWritesNothing is the delete-absent guard. The partial
// listing is real, and writing it would delete the tail of a correct one.
func TestATruncatedFetchWritesNothing(t *testing.T) {
	now := time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC)
	cat := &fakeCatalog{
		claimOK: true,
		state: catalog.AlbumTrackState{
			Status: catalog.AlbumTrackOK, FetchedAt: now.Add(-31 * 24 * time.Hour),
			AttemptedAt: now.Add(-31 * 24 * time.Hour),
		},
		tracks: []catalog.AlbumTrack{
			{TrackID: "t1", Name: "One", DiscNumber: 1, TrackNumber: 1},
			{TrackID: "t2", Name: "Two", DiscNumber: 1, TrackNumber: 2},
		},
	}
	// A partial listing *and* ErrTruncated, exactly as spotify.AlbumTracks
	// returns it.
	fetch := &fakeFetcher{
		tracks: []spotify.AlbumTrack{{ID: "t1", Name: "One", DiscNumber: 1, TrackNumber: 1}},
		err:    fmt.Errorf("spotify: album tracks: %w", spotify.ErrTruncated),
	}
	s := newService(t, cat, fetch, now)

	if _, err := s.Listing(context.Background(), nil, "album000000000000000001"); err != nil {
		t.Fatalf("Listing: %v", err)
	}
	s.Close()

	_, writes, fails := cat.counts()
	if writes != 0 {
		t.Fatalf("wrote %d times on a truncated fetch, want 0: the partial deleted the tail", writes)
	}
	if fails != 1 {
		t.Fatalf("recorded %d failures, want 1", fails)
	}
	if len(cat.tracks) != 2 {
		t.Fatalf("%d tracks survived, want the 2 that were already correct", len(cat.tracks))
	}
}

// TestAnEmptyListingIsAFailure keeps "Spotify will not show me this album" from
// being stored as "this album has no tracks", which the page would render as
// "you have played every track".
func TestAnEmptyListingIsAFailure(t *testing.T) {
	now := time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC)
	cat := &fakeCatalog{claimOK: true}
	fetch := &fakeFetcher{tracks: nil, err: nil} // a 200 with no items
	s := newService(t, cat, fetch, now)

	if _, err := s.Listing(context.Background(), nil, "album000000000000000001"); err != nil {
		t.Fatalf("Listing: %v", err)
	}
	s.Close()

	_, writes, fails := cat.counts()
	if writes != 0 {
		t.Fatalf("wrote %d times for an empty listing, want 0", writes)
	}
	if fails != 1 {
		t.Fatalf("recorded %d failures for an empty listing, want 1", fails)
	}
}

// TestAFailedFetchIsNotRetriedImmediately keeps a broken upstream from turning
// every page view into another request against a quota it is already refusing.
func TestAFailedFetchIsNotRetriedImmediately(t *testing.T) {
	now := time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC)
	cat := &fakeCatalog{
		claimOK: true,
		state: catalog.AlbumTrackState{
			Status: catalog.AlbumTrackFailed, AttemptedAt: now.Add(-time.Minute), Attempts: 1,
		},
	}
	fetch := &fakeFetcher{}
	s := newService(t, cat, fetch, now)

	got, err := s.Listing(context.Background(), nil, "album000000000000000001")
	if err != nil {
		t.Fatalf("Listing: %v", err)
	}
	if got.State != StateUnavailable {
		t.Fatalf("state = %q, want %q: the fetch failed and the backoff has not elapsed",
			got.State, StateUnavailable)
	}
	s.Close()
	if n := fetch.called(); n != 0 {
		t.Fatalf("fetcher called %d times one minute after a failure, want 0", n)
	}
}

// TestAFailedFetchIsRetriedAfterTheBackoff is the other half: fifteen minutes
// later the page tries again, rather than waiting out the thirty-day TTL.
func TestAFailedFetchIsRetriedAfterTheBackoff(t *testing.T) {
	now := time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC)
	cat := &fakeCatalog{
		claimOK: true,
		state: catalog.AlbumTrackState{
			Status: catalog.AlbumTrackFailed, AttemptedAt: now.Add(-16 * time.Minute), Attempts: 1,
		},
	}
	fetch := &fakeFetcher{tracks: []spotify.AlbumTrack{{ID: "t1", Name: "One", DiscNumber: 1, TrackNumber: 1}}}
	s := newService(t, cat, fetch, now)

	got, err := s.Listing(context.Background(), nil, "album000000000000000001")
	if err != nil {
		t.Fatalf("Listing: %v", err)
	}
	if got.State != StatePending {
		t.Fatalf("state = %q, want %q", got.State, StatePending)
	}
	s.Close()
	if n := fetch.called(); n != 1 {
		t.Fatalf("fetcher called %d times sixteen minutes after a failure, want 1", n)
	}
}

// TestALostClaimStartsNoSecondFetch is the two-tabs case as this process sees
// it: the claim went to somebody else, so this one reports pending and stops.
func TestALostClaimStartsNoSecondFetch(t *testing.T) {
	now := time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC)
	cat := &fakeCatalog{claimOK: false}
	fetch := &fakeFetcher{}
	s := newService(t, cat, fetch, now)

	got, err := s.Listing(context.Background(), nil, "album000000000000000001")
	if err != nil {
		t.Fatalf("Listing: %v", err)
	}
	if got.State != StatePending {
		t.Fatalf("state = %q, want %q: somebody else is fetching it", got.State, StatePending)
	}
	s.Close()
	if n := fetch.called(); n != 0 {
		t.Fatalf("fetcher called %d times after losing the claim, want 0", n)
	}
}

// TestALiveLeaseIsNotEvenClaimed keeps the browser's poll from writing to the
// database twice a second for as long as a fetch is running.
func TestALiveLeaseIsNotEvenClaimed(t *testing.T) {
	now := time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC)
	cat := &fakeCatalog{
		claimOK: true,
		state: catalog.AlbumTrackState{
			Status: catalog.AlbumTrackFetching, AttemptedAt: now.Add(-10 * time.Second), Attempts: 1,
		},
	}
	fetch := &fakeFetcher{}
	s := newService(t, cat, fetch, now)

	got, err := s.Listing(context.Background(), nil, "album000000000000000001")
	if err != nil {
		t.Fatalf("Listing: %v", err)
	}
	if got.State != StatePending {
		t.Fatalf("state = %q, want %q", got.State, StatePending)
	}
	s.Close()
	claims, _, _ := cat.counts()
	if claims != 0 {
		t.Fatalf("attempted %d claims against a ten-second-old lease, want 0", claims)
	}
	if n := fetch.called(); n != 0 {
		t.Fatalf("fetcher called %d times against a live lease, want 0", n)
	}
}

func TestAnExpiredLeaseIsReclaimed(t *testing.T) {
	now := time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC)
	cat := &fakeCatalog{
		claimOK: true,
		state: catalog.AlbumTrackState{
			Status: catalog.AlbumTrackFetching, AttemptedAt: now.Add(-10 * time.Minute), Attempts: 1,
		},
	}
	fetch := &fakeFetcher{tracks: []spotify.AlbumTrack{{ID: "t1", Name: "One", DiscNumber: 1, TrackNumber: 1}}}
	s := newService(t, cat, fetch, now)

	if _, err := s.Listing(context.Background(), nil, "album000000000000000001"); err != nil {
		t.Fatalf("Listing: %v", err)
	}
	s.Close()
	if n := fetch.called(); n != 1 {
		t.Fatalf("fetcher called %d times against a ten-minute-old lease, want 1: it is stranded", n)
	}
}

func TestTheErrorFromSpotifyIsNeverReturnedToTheCaller(t *testing.T) {
	now := time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC)
	cat := &fakeCatalog{claimOK: true}
	fetch := &fakeFetcher{err: errors.New("spotify: album tracks: 502 bad gateway")}
	s := newService(t, cat, fetch, now)

	if _, err := s.Listing(context.Background(), nil, "album000000000000000001"); err != nil {
		t.Fatalf("Listing returned %v; a Spotify failure must not fail the page request", err)
	}
}

// --- the operator's switch -------------------------------------------------

// TestDisabledMakesNoRequestAndClaimsNoLease is the switch doing the one thing
// an operator turned it off for. Both counters matter: a claim is a write to
// album_track_fetches, which an instance told to make no unattended requests
// should not be making either.
func TestDisabledMakesNoRequestAndClaimsNoLease(t *testing.T) {
	now := time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC)
	cat := &fakeCatalog{claimOK: true}
	fetch := &fakeFetcher{}
	s := newDisabledService(t, cat, fetch, now)

	got, err := s.Listing(context.Background(), nil, "album000000000000000001")
	if err != nil {
		t.Fatalf("Listing: %v", err)
	}
	if got.State != StateDisabled {
		t.Fatalf("state = %q, want %q", got.State, StateDisabled)
	}
	s.Close()
	if n := fetch.called(); n != 0 {
		t.Fatalf("fetcher called %d times on a disabled instance, want 0", n)
	}
	if claims, _, fails := cat.counts(); claims != 0 || fails != 0 {
		t.Fatalf("claims=%d fails=%d on a disabled instance, want 0 and 0", claims, fails)
	}
}

// TestDisabledIsNotUnavailable keeps an operator's choice from being reported as
// a Spotify failure. They are different facts and the page renders them
// differently; collapsing them makes Encore blame a third party for a local
// decision.
func TestDisabledIsNotUnavailable(t *testing.T) {
	now := time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC)
	s := newDisabledService(t, &fakeCatalog{claimOK: true}, &fakeFetcher{}, now)

	got, err := s.Listing(context.Background(), nil, "album000000000000000001")
	if err != nil {
		t.Fatalf("Listing: %v", err)
	}
	if got.State == StateUnavailable {
		t.Fatal("a disabled instance reported \"unavailable\"; the page would blame Spotify " +
			"for something the operator chose")
	}
	if got.State != StateDisabled {
		t.Fatalf("state = %q, want %q", got.State, StateDisabled)
	}
}

// TestDisabledStillServesACachedListing is the other half of "off": it stops
// fetching, it does not blind the page to what is already on disk.
func TestDisabledStillServesACachedListing(t *testing.T) {
	now := time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC)
	fetchedAt := now.Add(-3 * 24 * time.Hour)
	cat := &fakeCatalog{
		claimOK: true,
		state: catalog.AlbumTrackState{
			Status: catalog.AlbumTrackOK, FetchedAt: fetchedAt, AttemptedAt: fetchedAt,
		},
		tracks: []catalog.AlbumTrack{
			{TrackID: "t1", Name: "One", DiscNumber: 1, TrackNumber: 1},
			{TrackID: "t2", Name: "Two", DiscNumber: 1, TrackNumber: 2},
		},
	}
	fetch := &fakeFetcher{}
	s := newDisabledService(t, cat, fetch, now)

	got, err := s.Listing(context.Background(), nil, "album000000000000000001")
	if err != nil {
		t.Fatalf("Listing: %v", err)
	}
	if got.State != StateReady {
		t.Fatalf("state = %q, want %q: a stored listing is still a listing", got.State, StateReady)
	}
	if len(got.Tracks) != 2 {
		t.Fatalf("got %d tracks, want the 2 already on disk", len(got.Tracks))
	}
	if !got.FetchedAt.Equal(fetchedAt) {
		t.Fatalf("fetchedAt = %v, want %v: the page cannot say how old the listing is",
			got.FetchedAt, fetchedAt)
	}
	s.Close()
	if n := fetch.called(); n != 0 {
		t.Fatalf("fetcher called %d times on a disabled instance, want 0", n)
	}
}

// TestDisabledServesAStaleListingWithoutRefreshing is the case the plan rules on
// explicitly: past the TTL, with the switch off. The listing is served as it
// stands, with its date, and nothing is fetched. Withholding it would be
// strictly worse — the operator turned off fetching, not the album page.
func TestDisabledServesAStaleListingWithoutRefreshing(t *testing.T) {
	now := time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC)
	fetchedAt := now.Add(-400 * 24 * time.Hour) // far past the thirty-day TTL
	cat := &fakeCatalog{
		claimOK: true,
		state: catalog.AlbumTrackState{
			Status: catalog.AlbumTrackOK, FetchedAt: fetchedAt, AttemptedAt: fetchedAt,
		},
		tracks: []catalog.AlbumTrack{{TrackID: "t1", Name: "One", DiscNumber: 1, TrackNumber: 1}},
	}
	fetch := &fakeFetcher{}
	s := newDisabledService(t, cat, fetch, now)

	got, err := s.Listing(context.Background(), nil, "album000000000000000001")
	if err != nil {
		t.Fatalf("Listing: %v", err)
	}
	if got.State != StateReady || len(got.Tracks) != 1 {
		t.Fatalf("state/tracks = %q/%d, want %q with the stale listing still served",
			got.State, len(got.Tracks), StateReady)
	}
	if !got.FetchedAt.Equal(fetchedAt) {
		t.Fatalf("fetchedAt = %v, want %v", got.FetchedAt, fetchedAt)
	}
	s.Close()
	if n := fetch.called(); n != 0 {
		t.Fatalf("fetcher called %d times for an expired listing on a disabled instance, want 0: "+
			"the TTL check ran before the switch", n)
	}
	if claims, _, _ := cat.counts(); claims != 0 {
		t.Fatalf("attempted %d claims, want 0", claims)
	}
}
```

Note the two seams these tests need, both of which the implementation below provides: `Deps.Catalog` is an interface (`Store` in the code below), not the concrete `*catalog.Repo`, and `Deps.Writer` abstracts the one transaction so the fake needs no pool. Name them exactly as written here.

**Fails when:**
- *FirstViewStartsTheFetchAndReportsPending* — nothing is triggered on first view, so the listing never appears however many times the page is opened; or the empty state is reported as `ready`.
- *ListingDoesNotWaitForSpotify* — the fetch is made synchronous. This is the defect the whole design exists to prevent, and it is the one test that would catch a "simplification" back to a blocking call.
- *AFreshListingIsNotRefetched* — the TTL comparison is inverted or ignored, so every page view spends a request.
- *AnExpiredListingIsRefetchedAndStillServed* — the TTL is never checked (nothing ever refreshes), or a stale listing is withheld so an expiring cache blanks a working panel.
- *ATruncatedFetchWritesNothing* — the guard becomes `if err != nil && !errors.Is(err, spotify.ErrTruncated)`, which is exactly the mistake this project has made three times.
- *AnEmptyListingIsAFailure* — the `len(items) == 0` guard is removed, and the page then claims every track was played.
- *AFailedFetchIsNotRetriedImmediately* — the failed branch of `due` returns true unconditionally, hammering an upstream that is already refusing.
- *AFailedFetchIsRetriedAfterTheBackoff* — the failed branch is folded into the TTL branch, so one bad minute breaks the panel for a month.
- *ALostClaimStartsNoSecondFetch* — the claim's result is ignored and the fetch runs anyway.
- *ALiveLeaseIsNotEvenClaimed* — the live-lease check is dropped, so a polling browser writes to `album_track_fetches` twice a second.
- *AnExpiredLeaseIsReclaimed* — `fetching` is treated as never due, stranding the album and making the browser poll for ever.
- *TheErrorFromSpotifyIsNeverReturnedToTheCaller* — the fetch error is propagated out of `Listing`, turning a third-party outage into a 500 on the album page.
- *DisabledMakesNoRequestAndClaimsNoLease* — the `Enabled` check is missing, or is placed *inside* `start` after the claim, so a switched-off instance still writes to `album_track_fetches` on every album page view.
- *DisabledIsNotUnavailable* — `disabled` is folded into `unavailable`, and the page then tells the operator that Spotify would not answer when in fact nobody asked it.
- *DisabledStillServesACachedListing* — the disabled branch short-circuits before loading the stored listing, so turning off *fetching* also blinds the page to data already on disk.
- *DisabledServesAStaleListingWithoutRefreshing* — the TTL check runs before the `Enabled` check, so a switched-off instance resumes making requests the moment its cache expires. This is the exact ordering bug the switch exists to prevent.

- [ ] **Step 4: Run them and watch them fail**

```
go test -count=1 ./internal/albumtracks/
```
Expected: FAIL to build — the package does not exist.

- [ ] **Step 5: Implement**

`internal/albumtracks/albumtracks.go`:

```go
// Package albumtracks fills and serves the cached listing of an album's own
// tracks, which is what lets the album page name the tracks somebody has never
// played.
//
// Nothing here is a background loop. §5.2 rejects a sweep over every album in a
// history explicitly, so a listing is read the first time somebody opens that
// album's page and then kept for the configured TTL. What this package
// guarantees is that the page request itself never waits for Spotify: Listing
// answers from the database and, when a fetch is due, hands the walk to a
// goroutine on a context of its own.
//
// Two guards keep that from becoming a stampede, and they answer different
// questions. A bounded slot channel is this process asking "am I already busy?"
// A conditional write against album_track_fetches is the whole deployment
// asking "is anybody busy?" — and only the second survives two browser tabs,
// two API replicas, or a page that polls.
package albumtracks

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/RequiDev/encore/internal/config"
	"github.com/RequiDev/encore/internal/logging"
	"github.com/RequiDev/encore/internal/spotify"
	"github.com/RequiDev/encore/internal/store"
	"github.com/RequiDev/encore/internal/store/catalog"
)

const (
	// maxPages bounds one album's listing. Twenty pages of fifty is a thousand
	// tracks, which no released album approaches; it exists so a paging bug
	// cannot spend the instance's quota on one record.
	maxPages = 20
	// concurrency is how many listings this process reads at once. Small on
	// purpose: these start inside page requests, and they draw on the same quota
	// enrichment needs to do its job.
	concurrency = 4
	// leaseTTL is how long a 'fetching' row holds other callers off. Longer than
	// fetchTimeout, so a live fetch never loses its own lease, and short enough
	// that a process killed mid-fetch does not strand the album for long.
	leaseTTL = 2 * time.Minute
	// fetchTimeout bounds one album's whole walk — every page, every retry and
	// every rate-limit wait inside it.
	fetchTimeout = 90 * time.Second
	// failedRetryAfter is how long a failed listing is left alone. Failures here
	// are timeouts and rate limits, which clear in minutes; making somebody wait
	// out the thirty-day TTL would turn one bad minute into a broken panel.
	failedRetryAfter = 15 * time.Minute
	// recordTimeout bounds the write that records a failure, including during
	// shutdown.
	recordTimeout = 5 * time.Second
)

// errEmptyListing is a 200 that carried no tracks.
//
// There is no such record as an album with no tracks. An empty listing means
// the album is invisible to this application's market, or Spotify has withdrawn
// it. Storing it as a success would make the page say "you have played every
// track on this album", which is the exact overclaim this feature exists to
// avoid.
var errEmptyListing = errors.New("albumtracks: spotify returned no tracks for this album")

// State is what the page can say about the listing.
type State string

const (
	// StateReady means a listing is stored and can be reasoned about. It may be
	// older than the TTL, in which case a refresh is already running behind it.
	StateReady State = "ready"
	// StatePending means nothing is stored yet and a fetch is running.
	StatePending State = "pending"
	// StateUnavailable means nothing is stored and nothing is running: the last
	// attempt failed, or this process could not start one. It is emphatically not
	// "this album has no tracks".
	StateUnavailable State = "unavailable"
	// StateDisabled means nothing is stored and this instance will not fetch it,
	// because its operator turned that off.
	//
	// Deliberately not folded into StateUnavailable. "Spotify would not answer"
	// and "nobody asked Spotify" are different facts, and a page that renders the
	// first for the second blames a third party for a local decision.
	StateDisabled State = "disabled"
)

// Track is one entry of a listing.
type Track struct {
	ID          string
	Name        string
	DiscNumber  int
	TrackNumber int
}

// Listing is what one album's page is told.
type Listing struct {
	State State
	// Tracks is the whole listing in disc and track order, not just the unheard
	// part: which of them were played is the caller's question to ask, because
	// only the caller knows whose history it is asking about.
	Tracks []Track
	// FetchedAt is when the listing was read. Zero when none has succeeded.
	FetchedAt time.Time
}

// Fetcher is the slice of the Spotify client this package uses.
type Fetcher interface {
	AlbumTracks(ctx context.Context, albumID string, maxPages int) ([]spotify.AlbumTrack, error)
}

// Store is the slice of the catalogue repository this package uses. An
// interface so the policy above can be exercised without a database — these
// decisions are about *when* to fetch, which no amount of SQL will tell you.
type Store interface {
	AlbumTrackState(ctx context.Context, q store.Querier, albumID string) (catalog.AlbumTrackState, error)
	AlbumTracks(ctx context.Context, q store.Querier, albumID string) ([]catalog.AlbumTrack, error)
	ClaimAlbumTrackFetch(ctx context.Context, q store.Querier, albumID string, now, leaseCutoff time.Time) (bool, error)
	ReplaceAlbumTracks(ctx context.Context, q store.Querier, albumID string, items []catalog.AlbumTrack) error
	MarkAlbumTracksFetched(ctx context.Context, q store.Querier, albumID string, at time.Time) error
	FailAlbumTrackFetch(ctx context.Context, q store.Querier, albumID string, at time.Time, reason string) error
}

// Writer runs the one transaction this package needs and hands out the handle
// for everything outside it. *store.Store satisfies it through StoreWriter
// below; a test satisfies it without a pool.
type Writer interface {
	// InTx runs fn inside one transaction.
	InTx(ctx context.Context, fn func(ctx context.Context, q store.Querier) error) error
	// DB is the pool as a Querier, for single statements.
	DB() store.Querier
}

// StoreWriter adapts *store.Store to Writer. It is the only place in this
// package that names pgx, which keeps the policy above testable.
type StoreWriter struct{ Store *store.Store }

func (w StoreWriter) InTx(ctx context.Context, fn func(ctx context.Context, q store.Querier) error) error {
	return w.Store.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error { return fn(ctx, tx) })
}

func (w StoreWriter) DB() store.Querier { return w.Store.DB() }

// Deps is everything the service needs.
type Deps struct {
	Catalog Store
	Spotify Fetcher
	Writer  Writer
	Logger  *slog.Logger
	// Now is the clock. Tests replace it; production leaves it nil.
	Now func() time.Time
}

// Service fills and serves album track listings.
type Service struct {
	cat Store
	sp  Fetcher
	w   Writer
	log *slog.Logger
	now func() time.Time
	// enabled is the operator's switch. False means this instance never asks
	// Spotify anything — see config.AlbumTracks.Enabled.
	enabled bool
	ttl     time.Duration
	slots   chan struct{}
	base    context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup
}

// New validates the dependencies and builds the service.
func New(cfg config.AlbumTracks, deps Deps) (*Service, error) {
	switch {
	case deps.Catalog == nil:
		return nil, errors.New("albumtracks: catalog repository is required")
	case deps.Spotify == nil:
		return nil, errors.New("albumtracks: spotify client is required")
	case deps.Writer == nil:
		return nil, errors.New("albumtracks: writer is required")
	case cfg.TTL <= 0:
		return nil, errors.New("albumtracks: a positive TTL is required")
	}
	lg := deps.Logger
	if lg == nil {
		lg = slog.Default()
	}
	now := deps.Now
	if now == nil {
		now = time.Now
	}
	base, cancel := context.WithCancel(context.Background())
	if !cfg.Enabled {
		// Said once, at startup, rather than on every page view: an operator who
		// wonders why the album page reports this as turned off can find it here
		// and in the configuration line the process logs beside it.
		lg.Info("album track listings are turned off; this instance will not ask spotify " +
			"what is on an album. Listings already cached are still shown.")
	}
	return &Service{
		cat:     deps.Catalog,
		sp:      deps.Spotify,
		w:       deps.Writer,
		log:     lg.With("component", "albumtracks"),
		now:     now,
		enabled: cfg.Enabled,
		ttl:     cfg.TTL,
		slots:   make(chan struct{}, concurrency),
		base:    base,
		cancel:  cancel,
	}, nil
}

// Listing returns the stored listing for one album, and starts a refresh when
// one is due.
//
// It never blocks on Spotify and it never fails because Spotify did: a
// third-party outage is a state the page renders, not a 500 it shows.
func (s *Service) Listing(ctx context.Context, q store.Querier, albumID string) (Listing, error) {
	st, err := s.cat.AlbumTrackState(ctx, q, albumID)
	if err != nil {
		return Listing{}, err
	}

	var tracks []Track
	if !st.FetchedAt.IsZero() {
		rows, err := s.cat.AlbumTracks(ctx, q, albumID)
		if err != nil {
			return Listing{}, err
		}
		tracks = make([]Track, 0, len(rows))
		for _, r := range rows {
			tracks = append(tracks, Track{
				ID: r.TrackID, Name: r.Name,
				DiscNumber: r.DiscNumber, TrackNumber: r.TrackNumber,
			})
		}
	}

	now := s.now()
	// A live lease means somebody is fetching this album right now. Checking it
	// before deciding anything is what keeps a polling browser from attempting a
	// write on every tick. It is checked even when this instance has fetching
	// turned off: another replica may have started one before the switch was
	// flipped, and reporting that accurately costs nothing.
	running := st.Status == catalog.AlbumTrackFetching && now.Sub(st.AttemptedAt) < leaseTTL

	// s.enabled is checked *before* s.due, and that order is load-bearing: a
	// switched-off instance must not resume making requests the moment its cache
	// expires. Guarding here rather than inside start also means the claim — a
	// write — is never even attempted, which is what an operator asking for no
	// unattended traffic actually asked for.
	if !running && s.enabled && s.due(st, now) {
		running = s.start(ctx, q, albumID, now)
	}

	out := Listing{Tracks: tracks, FetchedAt: st.FetchedAt}
	switch {
	case len(tracks) > 0:
		// A listing read successfully once is worth showing while a refresh runs
		// behind it — and worth showing when no refresh is coming at all, because
		// turning off fetching is not the same as forgetting what is on disk.
		// Withholding it would replace a true answer that is old with no answer.
		// FetchedAt travels with it so the page can say how old, which is the only
		// honesty this case needs: a date claims nothing about freshness.
		out.State = StateReady
	case running:
		out.State = StatePending
	case !s.enabled:
		// Nothing stored, and this instance will not go and find out. That is the
		// operator's decision, not a Spotify failure, and the page says so in its
		// own words rather than reporting an outage that never happened.
		out.State = StateDisabled
	default:
		// Nothing stored and nothing running. The page must not read that as "this
		// album has no tracks you have missed".
		out.State = StateUnavailable
	}
	return out, nil
}

// due reports whether a fetch should be started now.
//
// It deliberately knows nothing about s.enabled. Whether this instance fetches
// at all is a different question from whether this listing is old, and its
// caller asks them in that order.
func (s *Service) due(st catalog.AlbumTrackState, now time.Time) bool {
	switch st.Status {
	case "":
		// Never attempted: the lazy fill §5.2 asks for.
		return true
	case catalog.AlbumTrackOK:
		return now.Sub(st.FetchedAt) >= s.ttl
	case catalog.AlbumTrackFailed:
		// Much sooner than the TTL, and deliberately so — see failedRetryAfter.
		return now.Sub(st.AttemptedAt) >= failedRetryAfter
	case catalog.AlbumTrackFetching:
		// The lease has expired: whatever process held it is gone.
		return now.Sub(st.AttemptedAt) >= leaseTTL
	default:
		return false
	}
}

// start begins a detached fetch, reporting whether one is now running anywhere.
func (s *Service) start(ctx context.Context, q store.Querier, albumID string, now time.Time) bool {
	select {
	case s.slots <- struct{}{}:
	default:
		// Every slot is busy. Refusing is the point: queueing here would be
		// queueing people behind a third party.
		s.log.Debug("album track fetch not started; all slots busy", "album", albumID)
		return false
	}
	release := func() { <-s.slots }

	claimed, err := s.cat.ClaimAlbumTrackFetch(ctx, q, albumID, now, now.Add(-leaseTTL))
	if err != nil {
		release()
		s.log.Warn("could not claim an album track fetch", "album", albumID, logging.Err(err))
		return false
	}
	if !claimed {
		// Somebody else holds the lease. A second request would be a wasted one
		// against a quota the whole application shares — but a fetch *is* running,
		// so the page is right to keep polling.
		release()
		return true
	}

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer release()
		s.fetch(albumID)
	}()
	return true
}

// fetch reads one album's listing and stores it, on a context of its own.
//
// Deliberately not the request's context: that request has already been
// answered, and cancelling when the browser navigated away would mean the
// listing never arrives however many times the page is opened. fetchTimeout
// bounds it instead, and Close cancels it at shutdown.
func (s *Service) fetch(albumID string) {
	ctx, cancel := context.WithTimeout(s.base, fetchTimeout)
	defer cancel()

	items, err := s.sp.AlbumTracks(ctx, albumID, maxPages)
	if err != nil {
		// Every failure lands here, ErrTruncated included — and that one arrives
		// with a partial listing attached. The partial must never reach the write:
		// ReplaceAlbumTracks deletes whatever the incoming set does not contain,
		// so a prefix would delete the tail of a listing that was correct and then
		// mark the result authoritative. This project has hit that trap three
		// times; internal/spotify/library.go's ErrTruncated comment is the record
		// of it. There is no exception clause here on purpose.
		s.record(albumID, err)
		return
	}
	if len(items) == 0 {
		s.record(albumID, errEmptyListing)
		return
	}

	rows := make([]catalog.AlbumTrack, 0, len(items))
	for _, it := range items {
		rows = append(rows, catalog.AlbumTrack{
			TrackID: it.ID, Name: it.Name,
			DiscNumber: it.DiscNumber, TrackNumber: it.TrackNumber,
		})
	}
	at := s.now()
	err = s.w.InTx(ctx, func(ctx context.Context, q store.Querier) error {
		if err := s.cat.ReplaceAlbumTracks(ctx, q, albumID, rows); err != nil {
			return err
		}
		// In the same transaction as the listing: the rows and the claim that they
		// are authoritative commit together, so a reader can never see a
		// half-replaced listing marked 'ok'.
		return s.cat.MarkAlbumTracksFetched(ctx, q, albumID, at)
	})
	if err != nil {
		s.record(albumID, err)
		return
	}
	s.log.Debug("stored an album track listing", "album", albumID, "tracks", len(rows))
}

// record writes a failure on its own context, so a fetch cancelled at shutdown
// still leaves the album out of 'fetching' rather than waiting out the lease.
func (s *Service) record(albumID string, cause error) {
	s.log.Warn("could not read an album track listing", "album", albumID, logging.Err(cause))

	ctx, cancel := context.WithTimeout(context.WithoutCancel(s.base), recordTimeout)
	defer cancel()
	if err := s.cat.FailAlbumTrackFetch(ctx, s.w.DB(), albumID, s.now(), cause.Error()); err != nil {
		// Nothing further can be done, and nothing is stuck: the lease expires on
		// its own, so the next page view after leaseTTL tries again.
		s.log.Error("could not record an album track failure", "album", albumID, logging.Err(err))
	}
}

// Close cancels every fetch in flight and waits for them.
//
// Bounded by recordTimeout per fetch, because a cancelled fetch still records
// its failure. The composition root defers this.
func (s *Service) Close() {
	s.cancel()
	s.wg.Wait()
}

var _ = fmt.Sprintf // keep fmt if trimmed during editing; remove if unused
```

Remove the trailing `var _ = fmt.Sprintf` line and the `fmt` import if `fmt` ends up unused — `staticcheck` will say so. `StoreWriter` needs `"github.com/jackc/pgx/v5"` imported; it is the only pgx reference in the package and it lives in the adapter deliberately.

- [ ] **Step 6: Run everything**

```
go test -count=1 ./internal/albumtracks/ ./internal/config/ ./test/deploy/
gofmt -l internal/albumtracks/ internal/config/; go vet ./...; staticcheck ./...
```
Expected: PASS, no linter output.

- [ ] **Step 7: Commit**

```bash
git add internal/albumtracks/ internal/config/config.go internal/config/config_test.go \
        docker-compose.yml .env.example docs/configuration.md
git commit -m "Album tracks: fetch a listing without making anyone wait for it"
```

Body: the page request must not block on a third party; the lease is in the database because two tabs and two replicas are the same problem; a truncated or empty fetch is a failure, never a listing; the switch exists because this is the one request nobody clicked for, and turning it off stops fetching without hiding what is already stored.

---

### Task 5: `stats.AlbumHeardTracks`

**Files:**
- Modify: `internal/stats/completion.go`
- Modify: `internal/stats/stats_test.go:29-86` (`statements()`)
- Modify: `test/integration/completion_test.go`

**Interfaces:**
- Consumes: `blacklistFilter(alias string) string` (`internal/stats/stats.go:69`); `store.UUIDArg`; `postgres.Classify`.
- Produces: `func (s *Service) AlbumHeardTracks(ctx context.Context, q store.Querier, userID uuid.UUID, albumID string) ([]string, error)` and `var albumHeardTracksSQL string`.

**Read `internal/stats/stats_test.go:88-136` before writing the SQL.** `TestParameterNumberingIsContiguous` and `TestBlacklistIsAppliedEverywhere` are mechanical guards over `statements()`; a new statement that reads `listens` and is not registered escapes them, and one that is registered with the wrong count fails immediately.

- [ ] **Step 1: Write the failing integration tests**

Append to `test/integration/completion_test.go`:

```go
// TestAlbumHeardTracksMatchesTheCompletionNumerator is the consistency property
// the album page depends on. The count and the set are two readings of the same
// question, and a page that shows "9 of 12 heard" beside four tracks it calls
// unheard is worse than one that shows neither.
func TestAlbumHeardTracksMatchesTheCompletionNumerator(t *testing.T) {
	env := harness.NewEnv(t)
	ctx := context.Background()
	user := harness.SeedUser(t, env)

	// Nine of the album's twelve tracks played, at various times.
	seedAlbumWithPlays(t, env, user.ID, "album000000000000000001", 12, 9)

	svc := stats.New(env.Store)
	completion, err := svc.AlbumCompletion(ctx, env.Store.DB(), user.ID, "album000000000000000001")
	if err != nil {
		t.Fatalf("AlbumCompletion: %v", err)
	}
	heard, err := svc.AlbumHeardTracks(ctx, env.Store.DB(), user.ID, "album000000000000000001")
	if err != nil {
		t.Fatalf("AlbumHeardTracks: %v", err)
	}
	if int64(len(heard)) != completion.Heard {
		t.Fatalf("AlbumHeardTracks returned %d ids but AlbumCompletion counted %d; "+
			"the page would contradict itself", len(heard), completion.Heard)
	}
	if completion.Heard != 9 {
		t.Fatalf("completion.Heard = %d, want 9", completion.Heard)
	}
}

// TestAlbumHeardTracksRespectsTheBlacklist keeps an excluded artist excluded
// here too. Without it the album page would name tracks by an artist the
// listener has told Encore to forget.
func TestAlbumHeardTracksRespectsTheBlacklist(t *testing.T) {
	env := harness.NewEnv(t)
	ctx := context.Background()
	user := harness.SeedUser(t, env)
	seedAlbumWithPlays(t, env, user.ID, "album000000000000000001", 12, 9)

	svc := stats.New(env.Store)
	before, err := svc.AlbumHeardTracks(ctx, env.Store.DB(), user.ID, "album000000000000000001")
	if err != nil {
		t.Fatalf("AlbumHeardTracks: %v", err)
	}
	if len(before) == 0 {
		t.Fatal("no tracks were heard before blacklisting; the fixture is wrong")
	}

	if err := env.Catalog.AddBlacklistedArtist(ctx, env.Store.DB(), user.ID, "artist00000000000000001"); err != nil {
		t.Fatalf("AddBlacklistedArtist: %v", err)
	}
	after, err := svc.AlbumHeardTracks(ctx, env.Store.DB(), user.ID, "album000000000000000001")
	if err != nil {
		t.Fatalf("AlbumHeardTracks after blacklisting: %v", err)
	}
	if len(after) != 0 {
		t.Fatalf("%d tracks still counted as heard after the artist was blacklisted, want 0", len(after))
	}
}

// TestAlbumHeardTracksIgnoresTheRange matches AlbumCompletion. A track heard
// five years ago is not a track you have never played, whatever the range
// picker says.
func TestAlbumHeardTracksIgnoresTheRange(t *testing.T) {
	env := harness.NewEnv(t)
	ctx := context.Background()
	user := harness.SeedUser(t, env)
	// Every play is in 2019; nothing is within any recent range.
	seedAlbumWithPlays(t, env, user.ID, "album000000000000000001", 12, 9)

	svc := stats.New(env.Store)
	heard, err := svc.AlbumHeardTracks(ctx, env.Store.DB(), user.ID, "album000000000000000001")
	if err != nil {
		t.Fatalf("AlbumHeardTracks: %v", err)
	}
	if len(heard) != 9 {
		t.Fatalf("got %d heard tracks for plays outside every recent range, want 9: "+
			"the query is range-scoped and must not be", len(heard))
	}
}
```

`seedAlbumWithPlays(t, env, userID, albumID string, total, played int)` is a helper to write in the same file: insert one artist (`artist00000000000000001`), one album with `total_tracks = total`, `total` tracks linked to it, `track_artists` rows for each, and one listen in 2019 for the first `played` of them. Model it on the fixtures already in `test/integration/completion_test.go`; reuse that file's existing helpers if one already does this.

**Fails when:**
- *MatchesTheCompletionNumerator* — the set is derived differently from the count (e.g. joining `album_tracks` rather than `tracks.album_id`, or dropping `DISTINCT`).
- *RespectsTheBlacklist* — `blacklistFilter` is not composed into the SQL. `TestBlacklistIsAppliedEverywhere` also catches this, but only once the statement is registered; this catches it even if registration is forgotten.
- *IgnoresTheRange* — somebody threads `rangeFilter` through "for consistency", which would silently redefine "never played" as "not played in this window".

- [ ] **Step 2: Run them and watch them fail**

```
ENCORE_TEST_DATABASE_URL="postgres://encore:encore@localhost:5433/encore?sslmode=disable" \
  go test -tags=integration -count=1 -p 1 -run TestAlbumHeardTracks ./test/integration/
```
Expected: FAIL to compile — `svc.AlbumHeardTracks` undefined.

- [ ] **Step 3: Implement**

Append to `internal/stats/completion.go`:

```go
// albumHeardTracksSQL lists which of one album's tracks the user has ever
// played.
//
// Deliberately the same shape and the same predicates as albumCompletionSQL's
// numerator above: that count and this set are two readings of one question,
// and they must never disagree. A page showing "9 of 12 heard" beside four
// tracks it calls unheard would be worse than one showing neither.
//
// Not range-filtered, for the reason given on albumCompletionSQL: a track heard
// five years ago is not a track you have never played, whatever window the page
// happens to be showing. The user predicate and the blacklist still apply.
//
// It deliberately does not read album_tracks. That cache is what Spotify says
// the album contains; this is what the listener actually played, and mixing the
// two here would make the answer depend on whether a fetch had happened yet.
//
// Parameters are $1 user, $2 album id.
var albumHeardTracksSQL = fmt.Sprintf(`
SELECT DISTINCT l.track_id
FROM listens l
JOIN tracks t ON t.id = l.track_id
WHERE l.user_id = $1 AND t.album_id = $2 AND %s`, blacklistFilter("l"))

// AlbumHeardTracks reports which of an album's tracks the user has ever played.
//
// The caller diffs this against the cached listing to name the rest. It is
// returned as ids rather than as a count because only the caller knows which
// listing it is diffing against, and a count would not survive the two
// disagreeing about what the album contains.
func (s *Service) AlbumHeardTracks(
	ctx context.Context,
	q store.Querier,
	userID uuid.UUID,
	albumID string,
) ([]string, error) {
	// No range to validate, but a nil user id must not reach SQL looking like a
	// legitimate parameter — it would match nothing rather than fail.
	if userID == uuid.Nil {
		return nil, fmt.Errorf("%w: a user is required", domain.ErrValidation)
	}
	rows, err := q.Query(ctx, albumHeardTracksSQL, store.UUIDArg(userID), albumID)
	if err != nil {
		return nil, postgres.Classify("album heard tracks", err)
	}
	defer rows.Close()

	out := make([]string, 0, 16)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, postgres.Classify("album heard tracks", err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, postgres.Classify("album heard tracks", err)
	}
	return out, nil
}
```

- [ ] **Step 4: Register the statement**

In `internal/stats/stats_test.go`'s `statements()`, beside the two completion entries:

```go
		{name: "albumCompletion", sql: albumCompletionSQL, params: 2},
		{name: "albumHeardTracks", sql: albumHeardTracksSQL, params: 2},
		{name: "completedAlbums", sql: completedAlbumsSQL, params: 3},
```

- [ ] **Step 5: Run both suites**

```
go test -count=1 ./internal/stats/
ENCORE_TEST_DATABASE_URL="postgres://encore:encore@localhost:5433/encore?sslmode=disable" \
  go test -tags=integration -count=1 -p 1 -run 'TestAlbum' ./test/integration/
```
Expected: PASS. `TestBlacklistIsAppliedEverywhere/albumHeardTracks` and `TestParameterNumberingIsContiguous/albumHeardTracks` now cover the new statement.

- [ ] **Step 6: Commit**

```bash
git add internal/stats/completion.go internal/stats/stats_test.go test/integration/completion_test.go
git commit -m "Stats: which of an album's tracks have been played"
```

Body: same predicates as the completion numerator so the page cannot contradict itself; not range-scoped, because "never played" is not a property of a window.

---

### Task 6: The endpoint

**Files:**
- Modify: `internal/httpapi/dto.go` (after `AlbumCompletionResponse`, ~line 632)
- Modify: `internal/httpapi/entities.go` (a new handler after `handleAlbum`, ~line 320)
- Modify: `internal/httpapi/router.go:103` (one route)
- Modify: `internal/httpapi/server.go` (a `Deps` field, an interface, a `Server` field, a check in `New`)
- Modify: `cmd/encore-api/main.go` (construct and `defer Close`)
- Modify: `test/e2e/e2e_test.go` (construct it; add a stub route; a new test)

**Interfaces:**
- Consumes: `albumtracks.Service.Listing` and `albumtracks.Listing/State/Track` (Task 4); `stats.Service.AlbumHeardTracks` (Task 5); `catalog.Repo.GetAlbum`; `parseSpotifyIDPath(r, "id")` (`internal/httpapi/params.go:192`); `CoverageResponse` (`dto.go:656`).
- Produces:
  - route `GET /api/albums/{id}/tracklist`
  - `type AlbumTrackRef struct { ID, Name string; DiscNumber, TrackNumber int }`
  - `type AlbumTrackList struct { State string; Coverage CoverageResponse; Missing []AlbumTrackRef; FetchedAt *time.Time }`
  - `httpapi.Deps.AlbumTracks albumTrackSource` — **required**, `New` returns an error when nil

- [ ] **Step 1: Write the DTO**

`internal/httpapi/dto.go`:

```go
// AlbumTrackRef is one track of an album's own listing.
//
// Deliberately not a TrackRef. These come from Spotify's listing rather than
// from the catalogue, and a track nobody has played is not in the catalogue at
// all — see migrations/00013_album_tracks.sql. Giving it the shape of a
// catalogue entity would invite a client to link to a track page that does not
// exist.
type AlbumTrackRef struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	DiscNumber  int    `json:"discNumber"`
	TrackNumber int    `json:"trackNumber"`
}

// AlbumTrackList is which tracks of an album the caller has never played.
//
// State is one of:
//
//	"ready"       — a listing is stored; Coverage and Missing mean something
//	"pending"     — no listing yet, and one is being read from Spotify now
//	"unavailable" — no listing, and none is being read: the last attempt failed
//	"disabled"    — no listing, and this instance does not fetch them at all
//	                (ENCORE_ALBUM_TRACKS_ENABLED=false)
//
// A client MUST render all four differently, and must never read anything but
// "ready" as "you have played everything". Missing is empty in three of the
// four, which is exactly why State exists. Only "ready" with
// Coverage.Covered == Coverage.Total means the album was played in full.
//
// "disabled" is deliberately distinct from "unavailable". The first is the
// operator's choice and the second is Spotify failing to answer; a client that
// renders the failure copy for the first blames a third party for a local
// decision.
//
// A listing already cached is still served as "ready" when fetching is
// disabled, past its TTL or not — turning off fetching does not hide what is on
// disk. FetchedAt is what keeps that honest, and it is the reason there is no
// separate "this will never refresh" field: a date says how old the answer is
// without claiming anything about how fresh it is, and a second field
// expressing the same fact is a field that drifts.
//
// Coverage's denominator is the listing Spotify returned, which is not
// necessarily the album's total_tracks: those come from different reads at
// different times and can disagree. The client states which one it followed.
type AlbumTrackList struct {
	State    string           `json:"state"`
	Coverage CoverageResponse `json:"coverage"`
	// Missing is the listed tracks with no play, in disc and track order. Always
	// present and never null, so a client can iterate it without a guard; it is
	// empty both when everything was played and when there is no listing, which
	// is exactly why State exists.
	Missing []AlbumTrackRef `json:"missing"`
	// FetchedAt is when the listing was last read from Spotify, absent until one
	// has succeeded. A listing older than the TTL is still served while a refresh
	// runs, so this is what says how old the answer is.
	FetchedAt *time.Time `json:"fetchedAt,omitempty"`
}

// toAlbumTrackList diffs the listing against what the caller has played.
//
// The diff is done here rather than in SQL because the two halves come from
// different places for different reasons: the listing is global catalogue data
// cached from Spotify, and the played set is one user's own history with their
// own blacklist applied. Joining them in one statement would tie a per-user
// answer to a table that is shared between users.
func toAlbumTrackList(l albumtracks.Listing, heard []string) AlbumTrackList {
	played := make(map[string]struct{}, len(heard))
	for _, id := range heard {
		played[id] = struct{}{}
	}

	out := AlbumTrackList{
		State:   string(l.State),
		Missing: make([]AlbumTrackRef, 0, len(l.Tracks)),
	}
	for _, t := range l.Tracks {
		if _, ok := played[t.ID]; ok {
			out.Coverage.Covered++
			continue
		}
		out.Missing = append(out.Missing, AlbumTrackRef{
			ID: t.ID, Name: t.Name,
			DiscNumber: t.DiscNumber, TrackNumber: t.TrackNumber,
		})
	}
	// The denominator is the listing, not albums.total_tracks. A listener who has
	// played a track Spotify no longer lists under this album is counted in
	// neither, which is honest: this panel can only speak about the listing it
	// has.
	out.Coverage.Total = int64(len(l.Tracks))
	if !l.FetchedAt.IsZero() {
		at := l.FetchedAt.UTC()
		out.FetchedAt = &at
	}
	return out
}
```

- [ ] **Step 2: Wire the dependency**

`internal/httpapi/server.go` — beside the other narrow interfaces:

```go
// albumTrackSource is the album track cache as the HTTP layer needs it.
//
// An interface rather than the concrete service, so this package keeps its rule
// of holding no SQL and never importing pgx, and so the handler can be
// exercised without a Spotify client behind it.
type albumTrackSource interface {
	Listing(ctx context.Context, q store.Querier, albumID string) (albumtracks.Listing, error)
}
```

`Deps` gains:

```go
	// AlbumTracks serves the cached listing of an album's tracks and starts a
	// refresh when one is due. Required: without it GET
	// /api/albums/{id}/tracklist could only answer "unavailable" for ever, which
	// is a broken instance wearing the mask of a working one.
	AlbumTracks albumTrackSource
```

`Server` gains `albumTracks albumTrackSource`; `New`'s switch gains:

```go
	case deps.AlbumTracks == nil:
		return nil, errors.New("httpapi: album track source is required")
```

and the struct literal gains `albumTracks: deps.AlbumTracks,`.

- [ ] **Step 3: Write the handler and the route**

`internal/httpapi/entities.go`, after `handleAlbum`:

```go
// handleAlbumTracklist answers GET /api/albums/{id}/tracklist.
//
// It is a separate route from GET /api/albums/{id} for two reasons. It is the
// only trigger for the lazy fetch, so there is exactly one place that can start
// one; and the client polls it while a fetch runs, which must not re-run the
// album page's whole statistics on every tick.
//
// It never waits for Spotify. Everything below reads the database; the service
// starts a detached fetch when one is due and this returns "pending", and the
// client asks again. A page that hangs on a third party is a defect.
func (s *Server) handleAlbumTracklist(w http.ResponseWriter, r *http.Request) {
	user, err := requireUser(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	id, err := parseSpotifyIDPath(r, "id")
	if err != nil {
		writeError(w, r, err)
		return
	}
	ctx := r.Context()

	// The album must already be in the catalogue. Without this, any base-62
	// string in the URL would spend one of the instance's Spotify requests on a
	// record nobody has listened to — the same quota argument §5.2 uses to reject
	// a background sweep, arriving through a different door.
	if _, err := s.catalog.GetAlbum(ctx, s.querier, id); err != nil {
		writeError(w, r, err)
		return
	}

	listing, err := s.albumTracks.Listing(ctx, s.querier, id)
	if err != nil {
		writeError(w, r, err)
		return
	}
	heard, err := s.stats.AlbumHeardTracks(ctx, s.querier, user.ID, id)
	if err != nil {
		writeError(w, r, err)
		return
	}

	writeJSON(w, r, http.StatusOK, toAlbumTrackList(listing, heard))
}
```

`internal/httpapi/router.go`, beside the album route:

```go
	s.route(mux, "GET /api/albums/{id}", s.handleAlbum)
	s.route(mux, "GET /api/albums/{id}/tracklist", s.handleAlbumTracklist)
```

- [ ] **Step 4: Wire the composition root**

`cmd/encore-api/main.go` — after `client` is built and before `httpapi.New`:

```go
	// The API runs no background *loop* — that is still encore-worker's job — but
	// it does start detached work on demand, the same way the "sync now" button
	// does. An album's track list is read when somebody opens that album's page,
	// and Close cancels anything still in flight at shutdown.
	albumTracks, err := albumtracks.New(cfg.AlbumTracks, albumtracks.Deps{
		Catalog: catalogRepo,
		Spotify: client,
		Writer:  albumtracks.StoreWriter{Store: db},
		Logger:  lg,
	})
	if err != nil {
		return err
	}
	defer albumTracks.Close()
```

and `httpapi.Deps` gains `AlbumTracks: albumTracks,`.

Update the package comment at `cmd/encore-api/main.go:9-11`, which currently says "The API never runs a background loop":

```go
// The API runs no scheduled loop: polling, imports and enrichment all belong to
// encore-worker. It does start two kinds of work on demand — a sync poller so
// the "sync now" button can poll one account, and an album track fetch when
// somebody opens an album page — both of which are triggered by a request and
// both of which are cancelled at shutdown.
```

`test/e2e/e2e_test.go` — `newInstance` gains the same construction (with `harness.Discard()` as the logger and `t.Cleanup(albumTracks.Close)`), and passes it in `httpapi.Deps`.

- [ ] **Step 5: Write the failing end-to-end test**

Add a route to the e2e Spotify stub serving `/v1/albums/{id}/tracks` (12 tracks, one page, `"next":null`), then in `test/e2e/e2e_test.go`:

```go
// TestAlbumTracklistFillsInWithoutBlockingThePage walks the whole feature: the
// first request answers immediately without a listing, the fetch lands behind
// it, and a later request names the tracks that were never played.
func TestAlbumTracklistFillsInWithoutBlockingThePage(t *testing.T) {
	inst := newInstance(t)
	inst.signIn()
	// An album of twelve tracks, of which the listener has played nine.
	inst.seedAlbumWithPlays("4aawyAB9vmqN3uQ7FjRGTy", 12, 9)

	start := time.Now()
	var first httpapi.AlbumTrackList
	inst.getJSON("/api/albums/4aawyAB9vmqN3uQ7FjRGTy/tracklist", &first)
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("the first request took %s; it waited for Spotify", elapsed)
	}
	if first.State != "pending" {
		t.Fatalf("first state = %q, want \"pending\"", first.State)
	}
	if len(first.Missing) != 0 {
		t.Fatalf("first response named %d missing tracks before any fetch finished", len(first.Missing))
	}

	var got httpapi.AlbumTrackList
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		inst.getJSON("/api/albums/4aawyAB9vmqN3uQ7FjRGTy/tracklist", &got)
		if got.State == "ready" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if got.State != "ready" {
		t.Fatalf("state never became ready; last was %q", got.State)
	}
	if got.Coverage.Total != 12 || got.Coverage.Covered != 9 {
		t.Fatalf("coverage = %d/%d, want 9/12", got.Coverage.Covered, got.Coverage.Total)
	}
	if len(got.Missing) != 3 {
		t.Fatalf("named %d missing tracks, want 3", len(got.Missing))
	}
	if got.FetchedAt == nil {
		t.Fatal("fetchedAt is absent on a ready listing; the page cannot say how old it is")
	}
	if n := inst.stub.albumTrackCalls(); n != 1 {
		t.Fatalf("the stub served %d album-track requests, want 1: the poll refetched", n)
	}
}

// TestAlbumTracklistRefusesAnAlbumNobodyHasPlayed keeps an arbitrary id in the
// URL from spending a Spotify request.
func TestAlbumTracklistRefusesAnAlbumNobodyHasPlayed(t *testing.T) {
	inst := newInstance(t)
	inst.signIn()

	rec := inst.get("/api/albums/1BBBBBBBBBBBBBBBBBBBBB/tracklist")
	if rec.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for an album not in the catalogue", rec.StatusCode)
	}
	if n := inst.stub.albumTrackCalls(); n != 0 {
		t.Fatalf("the stub served %d requests for an unknown album, want 0", n)
	}
}
```

```go
// TestAlbumTracklistDisabledAnswersWithoutTouchingSpotify walks the operator's
// switch end to end, which is the only place the configuration, the service and
// the handler are proved to agree about it.
func TestAlbumTracklistDisabledAnswersWithoutTouchingSpotify(t *testing.T) {
	inst := newInstanceWith(t, map[string]string{"ENCORE_ALBUM_TRACKS_ENABLED": "false"})
	inst.signIn()
	inst.seedAlbumWithPlays("4aawyAB9vmqN3uQ7FjRGTy", 12, 9)

	var got httpapi.AlbumTrackList
	rec := inst.getJSONStatus("/api/albums/4aawyAB9vmqN3uQ7FjRGTy/tracklist", &got)
	if rec != http.StatusOK {
		t.Fatalf("status = %d, want 200: a disabled feature still answers", rec)
	}
	if got.State != "disabled" {
		t.Fatalf("state = %q, want \"disabled\"", got.State)
	}
	if n := inst.stub.albumTrackCalls(); n != 0 {
		t.Fatalf("the stub served %d album-track requests on a disabled instance, want 0", n)
	}

	// And the lease was never even claimed: no unattended write, either.
	var rows int
	if err := inst.env.Store.DB().QueryRow(context.Background(),
		`SELECT count(*)::int FROM album_track_fetches`).Scan(&rows); err != nil {
		t.Fatalf("count: %v", err)
	}
	if rows != 0 {
		t.Fatalf("%d album_track_fetches rows were written on a disabled instance, want 0", rows)
	}
}
```

Use whatever request helpers `test/e2e/e2e_test.go` already provides in place of `inst.getJSON`/`inst.getJSONStatus`/`inst.get`/`inst.signIn` if they are named differently; read the file and match it. `inst.seedAlbumWithPlays` and `inst.stub.albumTrackCalls` are new helpers to add alongside the existing ones.

`newInstanceWith(t, overrides map[string]string)` is a **new** extraction, not a signature change: move the body of `newInstance` into it, merge `overrides` over the existing `config.LoadFrom` map, and leave `newInstance(t)` as a one-line call passing `nil`. Every existing call site is untouched.

**Fails when:**
- *FillsInWithoutBlocking* — the fetch is made synchronous (the first request would return `ready` and take as long as Spotify does); or the poll re-triggers a fetch on every request (`albumTrackCalls` > 1), which is what the lease exists to prevent; or the diff uses `album.totalTracks` as the denominator; or `fetchedAt` is dropped from the DTO.
- *RefusesAnAlbumNobodyHasPlayed* — the `GetAlbum` guard is removed and any base-62 string becomes a Spotify request.
- *DisabledAnswersWithoutTouchingSpotify* — `cfg.AlbumTracks` is not threaded from `config.Load` through `albumtracks.New` (a wiring bug no unit test can see, because every unit test constructs the config by hand); or the handler 404s/500s instead of answering; or the switch is checked after the lease claim, leaving rows in `album_track_fetches` on an instance asked to make no unattended requests.

- [ ] **Step 6: Run everything**

```
go test -count=1 ./...
ENCORE_TEST_DATABASE_URL="postgres://encore:encore@localhost:5433/encore?sslmode=disable" \
  go test -tags=integration -count=1 -p 1 -timeout=20m ./test/...
gofmt -l $(git ls-files '*.go'); go vet ./...; staticcheck ./...
```
Expected: PASS throughout.

- [ ] **Step 7: Commit**

```bash
git add internal/httpapi/ cmd/encore-api/main.go test/e2e/e2e_test.go
git commit -m "API: serve which tracks of an album were never played"
```

Body: its own route so the poll does not re-run the album's statistics and so there is one trigger point; the catalogue check keeps an arbitrary id from spending a request; the denominator is the listing, not `total_tracks`.

---

### Task 7: The page

**Files:**
- Modify: `web/src/lib/types.ts` (after `AlbumCompletion`, ~line 710)
- Modify: `web/src/lib/query.ts` (beside `qk.album`, line 129)
- Modify: `web/src/pages/AlbumDetail.tsx`
- Create: `web/src/test/album-tracklist.test.tsx`

**Interfaces:**
- Consumes: `AlbumTrackList` as Task 6 defined it; `Coverage` (`types.ts:377`); `formatCount`, `formatPlural`, `formatDate` (`web/src/lib/format.ts`); `Panel`, `EmptyState` (`web/src/components/ui`).
- Produces: `qk.albumTracklist(id)`; `tracklistPollInterval(state)` exported from `AlbumDetail.tsx` for its own test.

- [ ] **Step 1: Types and the query key**

`web/src/lib/types.ts`:

```ts
/**
 * Which tracks of an album have never been played.
 *
 * Unlike everything else on the album page this is not computed from data on
 * disk: Encore has to ask Spotify what the album contains, which it does the
 * first time somebody opens the page and then caches. So `missing` being empty
 * is ambiguous, and `state` is what resolves it — an empty `missing` under
 * anything but `ready` means "Encore does not know", never "you have played
 * everything".
 *
 * `disabled` is the instance's operator having turned fetching off
 * (`ENCORE_ALBUM_TRACKS_ENABLED=false`). It is separate from `unavailable` on
 * purpose: one is a local choice and the other is Spotify failing to answer,
 * and rendering the second for the first blames a third party for a decision
 * somebody here made. A listing cached before the switch was flipped still
 * arrives as `ready`.
 */
export type AlbumTrackListState = 'ready' | 'pending' | 'unavailable' | 'disabled'

export interface AlbumTrackRef {
  id: string
  name: string
  discNumber: number
  trackNumber: number
}

export interface AlbumTrackList {
  state: AlbumTrackListState
  /**
   * Played over listed. The denominator is the listing Spotify returned, which
   * is not necessarily `album.totalTracks`: they come from different reads and
   * can disagree, so the panel says which one it followed.
   */
  coverage: Coverage
  missing: AlbumTrackRef[]
  /**
   * When the listing was read. Absent until one has succeeded.
   *
   * The panel renders it on every `ready` state. It is the only thing keeping a
   * cached listing honest on an instance where fetching has been turned off and
   * no refresh is ever coming — a date says how old an answer is without
   * claiming anything about how current it is.
   */
  fetchedAt?: Timestamp
}
```

`web/src/lib/query.ts`, beside `album`:

```ts
  album: (id: string, range: DateRange) => ['entity', 'album', id, range] as const,
  // Deliberately not keyed by range: the listing and "have you ever played
  // this" are both all-time, exactly like the completion figure beside them.
  albumTracklist: (id: string) => ['entity', 'album', id, 'tracklist'] as const,
```

- [ ] **Step 2: Write the failing component tests**

`web/src/test/album-tracklist.test.tsx`. Copy `ME`, `albumPayload`, `stubRoutes` and `mountAt` from `web/src/test/album-completion.test.tsx` — the fixtures are the same shape and the engineer may be reading these files out of order.

```tsx
/**
 * The album page's "never played" panel.
 *
 * Its whole job is to keep four different silences apart: Encore has not asked
 * Spotify yet, Encore asked and failed, this instance does not ask at all, and
 * Encore asked and you have played everything. An empty list means all four, so
 * these tests pin the copy rather than the emptiness.
 */

import { describe, expect, it, beforeEach, vi } from 'vitest'
import { render, screen, within } from '@testing-library/react'
import type { AlbumTrackList } from '../lib/types'
import { tracklistPollInterval } from '../pages/AlbumDetail'
// ...ME, albumPayload, stubRoutes, mountAt copied from album-completion.test.tsx

function tracklist(overrides: Partial<AlbumTrackList> = {}): AlbumTrackList {
  return {
    state: 'ready',
    coverage: { covered: 10, total: 12 },
    missing: [
      { id: 'track-11', name: 'The Eleventh', discNumber: 1, trackNumber: 11 },
      { id: 'track-12', name: 'The Twelfth', discNumber: 1, trackNumber: 12 },
    ],
    fetchedAt: '2026-07-20T09:00:00Z',
    ...overrides,
  }
}

/** The panel, found the way a person finds it: by its heading. */
async function panel(): Promise<HTMLElement> {
  const heading = await screen.findByRole('heading', { name: 'Tracks you have never played' })
  const section = heading.closest('section')
  if (!section) throw new Error('the heading is not inside a panel')
  return section
}

beforeEach(() => {
  vi.unstubAllGlobals()
})

describe('the never-played panel', () => {
  it('names the missing tracks and states the listing as its denominator', async () => {
    stubRoutes({
      '/api/me': ME,
      '/api/albums/album-1': albumPayload({ heard: 10, total: 12, known: true }),
      '/api/albums/album-1/tracklist': tracklist(),
    })
    render(mountAt('/albums/album-1'))
    const section = await panel()

    expect(within(section).getByText('The Eleventh')).toBeInTheDocument()
    expect(within(section).getByText('The Twelfth')).toBeInTheDocument()
    expect(
      within(section).getByText(
        '2 of the 12 tracks Spotify lists for this album have no plays in your history, all time.',
      ),
    ).toBeInTheDocument()
  })

  it('says you played everything rather than showing an empty list', async () => {
    stubRoutes({
      '/api/me': ME,
      '/api/albums/album-1': albumPayload({ heard: 12, total: 12, known: true }),
      '/api/albums/album-1/tracklist': tracklist({
        coverage: { covered: 12, total: 12 },
        missing: [],
      }),
    })
    render(mountAt('/albums/album-1'))
    const section = await panel()

    expect(
      within(section).getByText('You have played every track on this album'),
    ).toBeInTheDocument()
    expect(within(section).getByText(/All 12 tracks Spotify lists/)).toBeInTheDocument()
  })

  it('says it is still asking Spotify, and claims nothing about completeness', async () => {
    stubRoutes({
      '/api/me': ME,
      '/api/albums/album-1': albumPayload({ heard: 10, total: 12, known: true }),
      '/api/albums/album-1/tracklist': tracklist({
        state: 'pending',
        coverage: { covered: 0, total: 0 },
        missing: [],
        fetchedAt: undefined,
      }),
    })
    render(mountAt('/albums/album-1'))
    const section = await panel()

    expect(
      within(section).getByText("Asking Spotify for this album's track list"),
    ).toBeInTheDocument()
    expect(within(section).queryByText(/played every track/i)).not.toBeInTheDocument()
    expect(within(section).queryByText(/have no plays/i)).not.toBeInTheDocument()
  })

  it('says this instance does not fetch track lists, and never blames Spotify', async () => {
    stubRoutes({
      '/api/me': ME,
      '/api/albums/album-1': albumPayload({ heard: 10, total: 12, known: true }),
      '/api/albums/album-1/tracklist': tracklist({
        state: 'disabled',
        coverage: { covered: 0, total: 0 },
        missing: [],
        fetchedAt: undefined,
      }),
    })
    render(mountAt('/albums/album-1'))
    const section = await panel()

    expect(within(section).getByText('Album track lists are turned off')).toBeInTheDocument()
    expect(
      within(section).getByText(/An administrator can turn this on with ENCORE_ALBUM_TRACKS_ENABLED/),
    ).toBeInTheDocument()
    // An operator's choice is not a Spotify failure, and not a promise to retry.
    expect(within(section).queryByText(/could not/i)).not.toBeInTheDocument()
    expect(within(section).queryByText(/tries again/i)).not.toBeInTheDocument()
    expect(within(section).queryByText(/played every track/i)).not.toBeInTheDocument()
  })

  it('serves a listing cached before fetching was turned off, with the date it was read', async () => {
    // The server reports "ready" whenever a listing exists, switch or no switch;
    // the date is what stops an unrefreshable list from reading as current.
    stubRoutes({
      '/api/me': ME,
      '/api/albums/album-1': albumPayload({ heard: 10, total: 12, known: true }),
      '/api/albums/album-1/tracklist': tracklist({ fetchedAt: '2024-03-12T09:00:00Z' }),
    })
    render(mountAt('/albums/album-1'))
    const section = await panel()

    expect(within(section).getByText('The Eleventh')).toBeInTheDocument()
    expect(within(section).getByText(/Track list read from Spotify on/)).toBeInTheDocument()
    expect(within(section).queryByText(/up to date|current|just now/i)).not.toBeInTheDocument()
  })

  it('carries the read date on the played-everything state too', async () => {
    stubRoutes({
      '/api/me': ME,
      '/api/albums/album-1': albumPayload({ heard: 12, total: 12, known: true }),
      '/api/albums/album-1/tracklist': tracklist({
        coverage: { covered: 12, total: 12 },
        missing: [],
        fetchedAt: '2024-03-12T09:00:00Z',
      }),
    })
    render(mountAt('/albums/album-1'))
    const section = await panel()

    expect(within(section).getByText(/Track list read from Spotify on/)).toBeInTheDocument()
  })

  it('says the list could not be read, and that nothing else on the page is affected', async () => {
    stubRoutes({
      '/api/me': ME,
      '/api/albums/album-1': albumPayload({ heard: 10, total: 12, known: true }),
      '/api/albums/album-1/tracklist': tracklist({
        state: 'unavailable',
        coverage: { covered: 0, total: 0 },
        missing: [],
        fetchedAt: undefined,
      }),
    })
    render(mountAt('/albums/album-1'))
    const section = await panel()

    expect(
      within(section).getByText("This album's track list could not be read"),
    ).toBeInTheDocument()
    expect(within(section).getByText(/Every other figure on this page/)).toBeInTheDocument()
    expect(within(section).queryByText(/played every track/i)).not.toBeInTheDocument()
  })

  it('prints both numbers when the listing and the album record disagree', async () => {
    stubRoutes({
      '/api/me': ME,
      // album.totalTracks is 12 in the shared fixture.
      '/api/albums/album-1': albumPayload({ heard: 10, total: 12, known: true }),
      '/api/albums/album-1/tracklist': tracklist({
        coverage: { covered: 10, total: 14 },
        missing: [
          { id: 'track-11', name: 'The Eleventh', discNumber: 1, trackNumber: 11 },
          { id: 'track-12', name: 'The Twelfth', discNumber: 1, trackNumber: 12 },
          { id: 'track-13', name: 'A Bonus', discNumber: 1, trackNumber: 13 },
          { id: 'track-14', name: 'Another Bonus', discNumber: 1, trackNumber: 14 },
        ],
      }),
    })
    render(mountAt('/albums/album-1'))
    const section = await panel()

    expect(
      within(section).getByText(
        'Spotify lists 14 tracks for this album; the album record says 12. This panel follows the list.',
      ),
    ).toBeInTheDocument()
  })

  it('leaves the completion figure alone when the track count is unknown', async () => {
    stubRoutes({
      '/api/me': ME,
      '/api/albums/album-1': albumPayload({ heard: 0, total: 0, known: false }),
      '/api/albums/album-1/tracklist': tracklist({
        coverage: { covered: 10, total: 12 },
      }),
    })
    render(mountAt('/albums/album-1'))

    const heard = (await screen.findByRole('heading', { name: 'Heard' })).closest('section')
    if (!heard) throw new Error('no Heard panel')
    // An unresolved track count is still unresolved. A cached listing is not a
    // licence to compute a percentage the enrichment has not earned.
    expect(within(heard).getByText(/track count not known yet/i)).toBeInTheDocument()
    expect(within(heard).queryByText(/%/)).not.toBeInTheDocument()
    expect(within(heard).queryByText(/\d+ of \d+/)).not.toBeInTheDocument()
  })
})

describe('the tracklist poll', () => {
  it('polls only while a fetch is running', () => {
    expect(tracklistPollInterval('pending')).toBe(2000)
    expect(tracklistPollInterval('ready')).toBe(false)
    expect(tracklistPollInterval('unavailable')).toBe(false)
    expect(tracklistPollInterval('disabled')).toBe(false)
    expect(tracklistPollInterval(undefined)).toBe(false)
  })
})
```

**Fails when:**
- *names the missing tracks* — the denominator is taken from `album.totalTracks`, or the panel lists every track rather than the unplayed ones.
- *says you played everything* — the empty `missing` falls through to a generic empty state, making success indistinguishable from failure.
- *still asking Spotify* — the component branches on `missing.length` before `state`, so pending renders as "you played everything". This is the single most likely defect in the whole task.
- *does not fetch track lists* — `disabled` shares a branch with `unavailable`, so the page tells somebody that Spotify would not answer when in fact their own instance never asked; or the copy promises a retry that is never coming.
- *serves a listing cached before fetching was turned off* — the client refuses to render a listing it cannot refresh, or the date line is dropped and an unrefreshable list reads as current.
- *carries the read date on the played-everything state too* — the date line is rendered only in the has-missing branch, which is what an earlier draft of this plan did.
- *could not be read* — `unavailable` shares a branch with `pending` or with the complete case.
- *both numbers when they disagree* — the reconciliation line is dropped and the page silently shows two different totals in two panels.
- *leaves the completion figure alone* — somebody back-fills `total` from `coverage.total`, or the panel computes a percentage from the listing. This is the spec's §7 row, guarded on the surface that most tempts it.
- *the poll* — the interval never stops (a tab left open polls the API for ever), never starts (the listing appears only on a manual reload), or polls a `disabled` instance for a fetch that will never happen.

- [ ] **Step 3: Run them and watch them fail**

```
cd web && npx vitest run src/test/album-tracklist.test.tsx
```
Expected: FAIL — `tracklistPollInterval` is not exported, and no panel has that heading.

- [ ] **Step 4: Implement**

`web/src/pages/AlbumDetail.tsx`. Add the import of `AlbumTrackList`/`AlbumTrackListState` to the existing `types` import and `formatDate` to the `format` import, then:

```tsx
/**
 * How often the page asks again while Spotify is being read.
 *
 * Exported so it can be tested without driving a real timer through TanStack
 * Query. Two seconds is short enough that the list appears while somebody is
 * still looking at the page and long enough not to be a load generator.
 *
 * It terminates because the server's states do: a failed fetch is recorded as
 * failed rather than left pending, and a lease that outlives its holder
 * expires, so "pending" always becomes "ready" or "unavailable". "disabled"
 * never polls at all — there is no fetch to wait for.
 */
export function tracklistPollInterval(state: AlbumTrackListState | undefined): number | false {
  return state === 'pending' ? 2000 : false
}
```

In the component, beside the existing `query`:

```tsx
  const tracklist = useQuery({
    queryKey: qk.albumTracklist(id),
    queryFn: ({ signal }) =>
      api.get<AlbumTrackList>(`/albums/${encodeURIComponent(id)}/tracklist`, undefined, signal),
    enabled: id !== '',
    refetchInterval: (query) => tracklistPollInterval(query.state.data?.state),
  })
```

The panel, placed after the existing "Heard" panel and before "Tracks you played":

```tsx
          <Panel
            title="Tracks you have never played"
            description={
              tracklist.data?.state === 'ready'
                ? `${formatCount(tracklist.data.missing.length)} of the ${formatPlural(
                    tracklist.data.coverage.total,
                    'track',
                  )} Spotify lists for this album have no plays in your history, all time.`
                : // Deliberately free of any promise. This same line sits above
                  // "pending", "unavailable" and "disabled", and only one of the
                  // three is going to resolve into a list.
                  "From Spotify's own list of what is on this album, read once and kept."
            }
            padded={false}
          >
            <MissingTracks
              list={tracklist.data}
              failed={tracklist.isError}
              totalTracks={album.totalTracks}
              timeZone={timeZone}
            />
          </Panel>
```

and the component itself, beside `CompletionFigure`:

```tsx
/**
 * Which tracks on this record have never been played.
 *
 * Everything else on this page is computed from listening Encore already holds.
 * This is not: it needs Spotify's own list of what is on the album, which Encore
 * reads the first time somebody opens this page and then keeps for a month. So
 * an empty list here means one of four different things, and saying which is
 * the whole job:
 *
 *   pending     — Encore has not been told what is on the album yet
 *   unavailable — Encore asked and could not find out
 *   disabled    — this instance does not ask, because its operator said not to
 *   ready       — Encore knows, and you have played all of it
 *
 * `disabled` and `unavailable` are kept apart deliberately: one is somebody
 * here choosing not to ask and the other is Spotify not answering, and telling
 * a person the second when the first is true blames a third party for a local
 * decision. A listing cached before the switch was turned off still arrives as
 * `ready` — turning off fetching does not hide what is already on disk — and
 * the date it was read is rendered on every `ready` state, which is what keeps
 * a list that will never refresh from reading as though it were current.
 *
 * Its denominator is the list Spotify returned, never `album.totalTracks`.
 * Those are two different readings taken at two different times and they can
 * disagree; when they do, this says both rather than quietly picking one.
 */
function MissingTracks({
  list,
  failed,
  totalTracks,
  timeZone,
}: {
  list: AlbumTrackList | undefined
  failed: boolean
  totalTracks: number
  timeZone: string
}): ReactElement {
  // The state is checked before the list is, deliberately. Branching on
  // `missing.length` first would render "you have played every track" for an
  // album Encore has not even asked about yet.
  if (list?.state === 'disabled') {
    return (
      <EmptyState
        title="Album track lists are turned off"
        description="This instance does not ask Spotify what is on an album, so Encore cannot say which tracks you have never played. Everything else on this page comes from your own listening and is unaffected. An administrator can turn this on with ENCORE_ALBUM_TRACKS_ENABLED."
      />
    )
  }
  if (failed || list?.state === 'unavailable') {
    return (
      <EmptyState
        title="This album's track list could not be read"
        description="Encore could not get the list of what is on this album from Spotify, so it cannot say which tracks you have never played. Every other figure on this page comes from your own history and is unaffected. Encore tries again later."
      />
    )
  }
  if (!list || list.state === 'pending') {
    return (
      <EmptyState
        title="Asking Spotify for this album's track list"
        description="Encore reads it once and keeps it, so this happens only the first time somebody opens this album. The list appears here on its own."
      />
    )
  }

  const listed = list.coverage.total
  const disagrees = totalTracks > 0 && totalTracks !== listed
  const reconciliation = disagrees
    ? `Spotify lists ${formatPlural(listed, 'track')} for this album; the album record says ${formatCount(totalTracks)}. This panel follows the list.`
    : null
  // Rendered on every `ready` state, not only when something is missing. On an
  // instance with fetching turned off this list will never change again, and a
  // list with no date on it reads as though it were current.
  const readOn = list.fetchedAt ? (
    <p className="mt-2 text-xs text-ink-faint">
      Track list read from Spotify on {formatDate(list.fetchedAt, timeZone)}.
    </p>
  ) : null

  if (list.missing.length === 0) {
    return (
      <div className="px-4 py-3">
        <EmptyState
          icon="track"
          title="You have played every track on this album"
          description={`All ${formatPlural(listed, 'track')} Spotify lists for it.${
            reconciliation ? ` ${reconciliation}` : ''
          }`}
        />
        {readOn}
      </div>
    )
  }

  return (
    <div className="px-4 py-3">
      <ul className="divide-y divide-line">
        {list.missing.map((track) => (
          <li key={track.id} className="flex items-baseline gap-3 py-2 text-sm">
            <span className="tabular w-10 shrink-0 text-right text-ink-faint">
              {track.discNumber > 1 ? `${track.discNumber}.` : ''}
              {track.trackNumber}
            </span>
            <span className="min-w-0 flex-1 truncate text-ink">{track.name}</span>
          </li>
        ))}
      </ul>
      {reconciliation ? <p className="mt-3 text-sm text-ink-muted">{reconciliation}</p> : null}
      {readOn}
    </div>
  )
}
```

Add a `<div className="panel h-40" />` to `LoadingBody` so the page's skeleton keeps its shape.

Check `divide-line`, `text-ink-faint` and `tabular` against `web/src/index.css` and the classes already used in this file; substitute the project's own names if any differ. Do not invent a class.

- [ ] **Step 5: Run the whole frontend**

```
cd web && npm run lint && npm run typecheck && npm run test && npm run build
```
Expected: PASS throughout, including the existing `album-completion.test.tsx` — the album page now issues a second request, and that file's `stubRoutes` 404s unknown paths, so the completion tests prove the new panel degrades to "could not be read" without breaking anything above it.

- [ ] **Step 6: NUL check and commit**

```bash
perl -0777 -ne 'print "NULs: ", tr/\0//, "\n"' web/src/pages/AlbumDetail.tsx
perl -0777 -ne 'print "NULs: ", tr/\0//, "\n"' web/src/test/album-tracklist.test.tsx
git add web/src/lib/types.ts web/src/lib/query.ts web/src/pages/AlbumDetail.tsx web/src/test/album-tracklist.test.tsx
git commit -m "Web: name the tracks on an album you have never played"
```

Body: the three silences are different and the panel says which one it is in; the denominator is Spotify's listing, and when it disagrees with the album record the page prints both.

---

### Task 8: Documentation and full verification

**Files:**
- Modify: `docs/api.md` (the "Album completion" section, ~line 153; the entities table, line 353)
- Modify: `docs/feature-parity.md`

**Interfaces:** consumes everything above; produces no code.

- [ ] **Step 1: Document the endpoint**

In `docs/api.md`, after the album-completion section, add:

```markdown
### Which tracks you have never played

`GET /api/albums/{id}/tracklist`

Completion (above) counts what you have heard. This names what you have not — which needs Spotify's
own list of what is on the album, because nothing Encore stores says what an album contains.

That list is read **the first time somebody opens the album's page** and then cached for
`ENCORE_ALBUM_TRACKS_TTL` (30 days by default). There is no background sweep: most albums in a large
history are never opened, and enumerating them all would spend the instance's quota on questions
nobody asked.

It is also the one Spotify request `encore-api` makes that nobody clicked for — it fires as a side
effect of *viewing* a page — so an operator can switch it off with
`ENCORE_ALBUM_TRACKS_ENABLED=false`. This endpoint still answers when they have.

**This endpoint never waits for Spotify.** It answers from the database and starts the fetch behind
it, so `state` says which of four situations you are in:

```json
{
  "state": "ready",
  "coverage": { "covered": 9, "total": 12 },
  "missing": [ { "id": "4uLU…", "name": "…", "discNumber": 1, "trackNumber": 4 } ],
  "fetchedAt": "2026-07-20T09:00:00Z"
}
```

| `state` | Means | What a client must render |
|---|---|---|
| `ready` | A listing is stored. `coverage` and `missing` are meaningful. | The list, or "you have played every track" when `missing` is empty. |
| `pending` | No listing yet; one is being read from Spotify now. | "Encore is asking Spotify." Poll; every `pending` resolves. |
| `unavailable` | No listing, and none is being read: the last attempt failed. | "Encore could not read the list." Never "you have played everything." |
| `disabled` | No listing, and this instance does not fetch them: `ENCORE_ALBUM_TRACKS_ENABLED=false`. | "This instance does not fetch album track lists." Never blame Spotify, and never promise a retry. |

`missing` is empty in three of the four, so **a client must branch on `state` before it branches on
`missing`.** Only `ready` with `coverage.covered == coverage.total` means the album was played in
full.

`disabled` is deliberately not folded into `unavailable`. "Your operator chose not to ask" and
"Spotify would not answer" are different facts, and a page that renders the second for the first
blames a third party for a local decision.

**Turning fetching off does not hide a listing that is already cached.** One stored before the
switch was flipped still arrives as `ready`, past its TTL or not: what was turned off is *fetching*,
not the album page, and withholding a listing that was correct when it was read would be strictly
worse than showing it. `fetchedAt` is what keeps that honest, and it is why there is no separate
"this will never refresh" field — a date says how old an answer is without claiming anything about
how current it is, and a second field expressing one fact is a field that drifts. The web client
renders that date on every `ready` state.

`coverage.total` is the number of tracks **Spotify returned**, which is not necessarily the album's
`totalTracks`: they come from different reads at different times and can disagree. The web client
prints both numbers when they do. `total_tracks` is *not* back-filled from this listing — enrichment
owns that column, and an album with `total_tracks = 0` is still excluded from completion rather than
reported as 0%, exactly as before.

A failed fetch is retried after fifteen minutes rather than after the TTL, because failures here are
timeouts and rate limits. A failure never replaces or empties a listing that was read successfully
earlier: the older listing stays readable and `fetchedAt` keeps saying how old it is.

**One known limitation.** The listing is requested without a market, so the ids are Spotify's
canonical ones. A play recorded under a *relinked* id — the same recording, a different id in a
different market — will not match, and that track appears as never played. Encore does not guess at
equivalences here.

The album must already be in your catalogue; an id that is not answers 404 without touching Spotify.
```

Add to the entities table:

```
| `GET` | `/api/albums/{id}/tracklist` | Which tracks of the album have never been played, from Spotify's own track list. Cached, lazily filled, and never blocking. |
```

- [ ] **Step 2: Update the parity table**

In `docs/feature-parity.md`, record that the missing-track list ships, that it costs one request per album viewed per TTL, that it is lazily filled on first page view with no background sweep, that it can be switched off with `ENCORE_ALBUM_TRACKS_ENABLED` (and that listings already cached keep being shown when it is), and that discography coverage (`GET /v1/artists/{id}/albums`) remains outstanding for phase 2e-ii.

- [ ] **Step 3: Full verification — the plan's final evidence**

```
export PATH="$PATH:$(go env GOPATH)/bin"
gofmt -l $(git ls-files '*.go'); go vet ./...; staticcheck ./...
go test -count=1 ./...
ENCORE_TEST_DATABASE_URL="postgres://encore:encore@localhost:5433/encore?sslmode=disable" \
  go test -tags=integration -count=1 -p 1 -timeout=20m ./test/...
cd web && npm run lint && npm run typecheck && npm run build && npm run test
git diff --stat b0a1234 -- go.mod go.sum web/package.json web/package-lock.json
```

The last command must print **nothing**: no new dependency in either ecosystem.

**Report real output. Do not claim a pass on a command you did not run.**

- [ ] **Step 4: Commit**

```bash
git add docs/api.md docs/feature-parity.md
git commit -m "Docs: what the never-played list can and cannot tell you"
```

Body: the four states and why a client must branch on `state` first; why `disabled` is not `unavailable`, and that a cached listing survives the switch being turned off; the listing's denominator is not `total_tracks`; relinked ids are a known gap.

---

## Definition of done

- [ ] All gates pass; `00013` applies, rolls back and re-applies.
- [ ] **No page request ever waits for a Spotify call.** Pinned by `TestListingDoesNotWaitForSpotify` and by the e2e timing assertion.
- [ ] Two concurrent viewers cause **one** fetch. Pinned by `TestClaimAlbumTrackFetchIsExclusive` and by the e2e call count.
- [ ] A lease outliving its holder is reclaimed, so `pending` always terminates and the browser's poll always stops.
- [ ] **A truncated fetch writes nothing** and leaves the previous listing intact.
- [ ] **A 200 with no items is recorded as a failure**, never as an empty album.
- [ ] A failure never clears `fetched_at` or deletes rows.
- [ ] An expired listing is refetched *and* still served while the refresh runs.
- [ ] An album with `total_tracks = 0` still reports "track count not known yet" and no percentage, listing or no listing.
- [ ] The panel's denominator is the stored listing; when it disagrees with `album.totalTracks` the page prints both.
- [ ] `pending`, `unavailable`, `disabled`, `ready`-with-missing and `ready`-with-nothing-missing each render distinctly, each pinned by its own test.
- [ ] **With `ENCORE_ALBUM_TRACKS_ENABLED=false`: zero Spotify requests and zero rows in `album_track_fetches`**, proven end to end.
- [ ] **A listing cached before the switch was turned off is still served**, past its TTL or not, and the date it was read is rendered on every `ready` state.
- [ ] The disabled copy never says "could not", never mentions Spotify failing, and never promises a retry.
- [ ] `albumHeardTracks` is in `statements()` and both mechanical guards cover it.
- [ ] `ENCORE_ALBUM_TRACKS_ENABLED` and `ENCORE_ALBUM_TRACKS_TTL` are both in `config.go`, `docker-compose.yml`, `.env.example` and `docs/configuration.md`, and both appear in `Redacted()`; `./test/deploy/` passes.
- [ ] `go.mod`, `go.sum` and `web/package.json` are unchanged.
- [ ] Nothing named `artist_albums`, `album_group`, or "discography" exists anywhere in the diff.

---

## Self-review

**1. Spec coverage (§5.2, album half).**

| Spec requirement | Task |
|---|---|
| "Naming the *missing* tracks needs `GET /v1/albums/{id}/tracks`" | 2 |
| `album_tracks (album_id, track_id, disc_number, track_number, fetched_at)` | 1 — with `fetched_at` moved to `album_track_fetches` and the deviation argued in the migration comment |
| "fetched lazily when the relevant detail page is first opened" | 4 (policy), 6 (the trigger) |
| "cached … with a TTL after which the next page view refreshes them" | 4 |
| "A background sweep is explicitly rejected" | Honoured: no worker loop, no sweep, and the reasoning is repeated in the migration, the config comment and `docs/api.md` |
| "Both use the app token, so neither needs a user scope" | 2 — `getAsApp`, and `DefaultScopes()` untouched |
| "`/albums/{id}/tracks` is paginated" | 2 |
| §7 "An album with `total_tracks = 0` is excluded from completion, not reported as 0%" | 7, and reinforced by *not* back-filling `total_tracks` |
| `artist_albums`, `/artists/{id}/albums`, `album_group`, discography | Out of scope, explicitly, and in the definition of done |

No gaps. §5.1 is untouched by design.

**2. Placeholder scan.** No "TBD", no "add error handling", no "similar to Task N", no test described without its code. Three places deliberately say "read the existing file and match it": the e2e request helpers (Task 6 step 5), the `seedAlbumWithPlays` fixture (Task 5 step 1) and the Tailwind class names (Task 7 step 4). Each names the exact file to read and what to match, because inventing a helper that already exists under another name is the more likely failure than not knowing one is needed.

**3. Type consistency, checked end to end.**
- `spotify.AlbumTrack{ID,Name,DiscNumber,TrackNumber}` (T2) → `catalog.AlbumTrack{TrackID,Name,DiscNumber,TrackNumber}` (T3; **`TrackID`, not `ID`** — the field renames at the store boundary because `album_id` is also present there) → `albumtracks.Track{ID,...}` (T4) → `httpapi.AlbumTrackRef{ID,Name,DiscNumber,TrackNumber}` (T6) → `AlbumTrackRef` in TS (T7). The one rename is deliberate and appears in both tasks' Interfaces blocks.
- `catalog.AlbumTrackState{Status,FetchedAt,AttemptedAt,Attempts}` is produced in T3 and consumed by `Service.due` in T4 with those exact names.
- The status constants `AlbumTrackFetching/OK/Failed` are declared in T3 and used in T4; the *API* states `ready/pending/unavailable` are declared in T4 and used in T6 and T7. They are different vocabularies on purpose — one is storage, one is presentation — and neither leaks into the other.
- `ClaimAlbumTrackFetch(ctx, q, albumID, now, leaseCutoff)` has the same five parameters in T3's implementation, T4's `Store` interface and T4's fake.
- `Listing(ctx, q, albumID) (Listing, error)` matches between T4's method, T6's `albumTrackSource` interface and T6's handler.
- `AlbumHeardTracks(ctx, q, userID, albumID) ([]string, error)` matches between T5 and T6's handler.
- `CoverageResponse{Covered,Total}` (existing, `dto.go:656`) serialises as `{covered,total}`, which is exactly the existing TS `Coverage`. No new coverage shape is introduced.
- `qk.albumTracklist(id)` is declared in T7 step 1 and used in T7 step 4 with the same signature.

**One issue found and fixed inline:** an earlier draft had `Deps.Store *store.Store` on the service, which would have forced `internal/albumtracks`' tests to stand up a pool just to exercise the TTL arithmetic. Replaced with the `Writer` interface plus the `StoreWriter` adapter, so pgx is named in exactly one four-line type and the policy is testable without a database. The `Deps` block in Task 4's Interfaces and the composition-root wiring in Task 6 both reflect the final shape.

**4. Amendment: `ENCORE_ALBUM_TRACKS_ENABLED` (added after review).**

The project's owner overruled the paragraph in this plan's **decision 2** that argued no enable flag was needed. (The review referred to it as "decision 5"; in this document's own numbering, decision 5 is pagination and the no-flag argument lived in decision 2, which is where the rewrite went.) The reasoning is recorded there and is sound: this is the only Spotify request `encore-api` makes as a side effect of *viewing* a page rather than as the consequence of a click, so whether it happens at all is an operator's call — the same judgement that made the now-playing poller opt-in.

**Task count is unchanged at 8.** The flag folds into the tasks whose deliverables need it (4, 6, 7, 8) rather than becoming one of its own; splitting a boolean across a task boundary would not give a reviewer anything they could accept or reject independently.

Re-checked for consistency after the amendment:
- The state vocabulary is now four-valued in **six** places and they agree: `albumtracks.StateDisabled` (T4), the `Listing` switch (T4), the `AlbumTrackList` doc comment (T6), `AlbumTrackListState` in TS (T7), `MissingTracks`' branch order (T7), and the `docs/api.md` table (T8). The panel's branch order is `disabled` → `unavailable`/`failed` → `pending` → `ready`, and `disabled` is first because it is the only one of the four that is not about Spotify at all.
- `config.AlbumTracks` gained a field, so its *value* comparison in `TestAlbumTracksDefaults` compares the whole struct — which is why that test is written against the struct rather than field by field, and why it would catch a third field being added without a default.
- `s.enabled` is read in exactly one place, at the `Listing` call site, ahead of `s.due`. `due` and `start` were left ignorant of it deliberately, so there is one ordering to get right rather than three.
- Decision 4's tally moved from "three states, fourth render" to "four states, fifth render"; the DTO comment, the api.md prose and the definition of done were all updated to match, and `missing` is now empty in three of four states rather than two of three.
- One real bug in the original draft surfaced while writing the disabled-with-stale-cache case: `fetchedAt` was rendered only in the has-missing branch of the panel, so an album where everything had been played showed a listing with no date at all. Fixed in Task 7 by hoisting `readOn` out of both branches, and pinned by its own test — which is the second time in this plan that thinking about the honesty requirement found a defect rather than merely documenting one.
