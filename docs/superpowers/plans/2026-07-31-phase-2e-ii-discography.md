# Phase 2e-ii — Discography Coverage

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Encore cannot say *"you have heard 4 of this artist's 11 albums"*, because no stored field counts an artist's releases. This makes it able to.

**Architecture:** A lazily filled, TTL-refreshed cache of what Spotify lists for an artist (`artist_albums`), plus a sibling row per artist recording the outcome of the last fetch (`artist_album_fetches`). `GET /api/artists/{id}/discography` reads only the database and **never blocks on Spotify**; when a fetch is due it claims a database lease and runs it detached, and the page polls until the state resolves. Coverage counts the `album` group only and the page says so; every other group is stored, counted and named rather than silently dropped.

**Tech Stack:** Go 1.26, pgx/v5, PostgreSQL 17, React 19 + TypeScript + Vite + TanStack Query v5.

**Spec:** [`docs/design/2026-07-29-phase-2-scope-expansion-design.md`](../../design/2026-07-29-phase-2-scope-expansion-design.md) §5.2, **the artist half only**.

**Task count: 8.** One over the 5–8 target's midpoint, and the extra one is the shared-code decision (below): extracting the fetch lifecycle into `internal/lazyfetch` and moving `internal/albumtracks` onto it is **Task 4**, and it lands and is proved green on its own before Task 5 builds the new service on top of it. If the extraction breaks something it must be visible by itself, not tangled with a feature.

---

## What is lifted from 2e-i, what is new, and what is shared

Phase 2e-i (merged at `f4a1c9b`, 18 commits) built this exact shape for album track listings. Read these five files before starting anything — they are the reference implementation, and four rounds of review findings are recorded in their comments:

- `internal/albumtracks/albumtracks.go` — the fetch policy
- `internal/store/catalog/albumtracks.go` — the five statements
- `internal/spotify/albumtracks.go` — the paged client method
- `migrations/00013_album_tracks.sql` — why the outcome lives in its own table
- `internal/httpapi/entities.go` `handleAlbumTracklist` + `internal/httpapi/dto.go` `toAlbumTrackList` + `web/src/pages/AlbumDetail.tsx` `MissingTracks`

### Lifted from 2e-i essentially unchanged (structure and reasoning, with names and prose re-stated in artist terms)

| Thing | 2e-i source | 2e-ii home |
|---|---|---|
| Two tables: rows + a sibling outcome row | `migrations/00013_album_tracks.sql` | `migrations/00014_artist_albums.sql` (Task 1) |
| Single-statement lease claim, `RETURNING` empty for the loser, `attempted_at` expiry | `claimAlbumTrackFetchSQL` | `claimArtistAlbumFetchSQL` (Task 1) |
| Delete-absent + upsert-present in one statement, refusing an empty input | `replaceAlbumTracksSQL` / `ReplaceAlbumTracks` | `replaceArtistAlbumsSQL` / `ReplaceArtistAlbums` (Task 1) |
| Mark-fetched resets `attempts` and clears `last_error`; fail touches neither rows nor `fetched_at` | `markAlbumTracksFetchedSQL`, `failAlbumTrackFetchSQL` | Task 1 |
| `store.Truncate` on the failure reason | `FailAlbumTrackFetch` | `FailArtistAlbumFetch` (Task 1) |
| Offset pagination at 50, `next` to exhaustion, `ErrTruncated` *with* partial data | `spotify.AlbumTracks` | `spotify.ArtistAlbums` (Task 2) |
| Four states `ready`/`pending`/`unavailable`/`disabled`, and why `disabled` is not `unavailable` | `albumtracks.State` | **shared** `lazyfetch.Outcome` (Task 4) |
| `due()`, bounded slots, DB lease, detached context, `track()`/`Close()` WaitGroup discipline, `record()` on `context.WithoutCancel` | `albumtracks.Service` | **shared** `lazyfetch.Gate` (Task 4) |
| Enable switch + TTL, in `config.go` + `docker-compose.yml` + `.env.example` + `docs/configuration.md` + regenerated Portainer file | `config.AlbumTracks` | `config.ArtistAlbums` (Task 3) |
| Endpoint that 404s an entity not in the catalogue before spending a request | `handleAlbumTracklist` | `handleArtistDiscography` (Task 6) |
| One-pass derivation of coverage + missing in the DTO | `toAlbumTrackList` | `toArtistDiscography` (Task 6) |
| Poll cap in `sessionStorage`, keyed per entity, with a stale-window expiry | `AlbumDetail.tsx` | **shared** `web/src/lib/fetchpoll.ts` (Task 7) |

### Genuinely new — nothing in 2e-i answers these

1. **`album_group`.** Completion counts `album` and excludes `single`, `compilation`, `appears_on`. Nothing in 2e-i had a filtered denominator.
2. **An empty *counted* set is legitimate.** An album with zero tracks is impossible and is recorded as a failure. An artist with zero `album`-group releases is an ordinary artist who has only released singles. The empty-listing guard therefore applies to the **whole response**, never to the filtered subset — Task 5 pins this.
3. **`artist_albums.album_id` has no foreign key.** Most of a discography is albums nobody played, which are not in `albums` at all. `album_tracks.track_id` has no FK for the same reason; here it applies to the *majority* of rows rather than a minority.
4. **No rival number to reconcile.** 2e-i printed both Spotify's total and `albums.total_tracks` when they disagreed. Encore stores no count of an artist's releases — that absence is the entire premise of §5.2 — so there is no second number and no reconciliation line. Do not invent one.
5. **A bigger walk.** An album is ~1 request. A prolific artist with `appears_on` included is up to 7. That changes the TTL, the timeout, the lease and the poll cap (Task 3, Task 5, Task 7), and it changes the request-budget prose in `docs/configuration.md`.
6. **"Played" is per-album, not per-track.** An album counts as played when *any* of its tracks was played. The copy must say so or "you have played every album" overclaims.

### The shared-code decision

**One implementation of the hard part, on both sides. Go: extract `internal/lazyfetch.Gate` and move `internal/albumtracks` onto it (Task 4) before building `internal/artistalbums` on it (Task 5). Web: extract `web/src/lib/fetchpoll.ts` (Task 7).**

This was decided by the project's owner, overruling an earlier draft of this plan that argued for a parallel Go implementation on the grounds that two safety-net tests reach into `albumtracks`' unexported internals. That constraint has been lifted; the tests are to be *strengthened*, not preserved in amber. Touching merged, working code is accepted, and the extra task it costs is accepted.

**The alternative — a second copy of the policy in `internal/artistalbums` — was considered and rejected on maintenance cost.** The lease claim, the TTL and backoff schedule, the bounded slot channel, the detached-context fetch and the shutdown ordering are ~250 lines that took four review rounds to get right. Two copies means every future fix to any of them has to be made twice, by somebody who has to *notice* that the second copy exists. Nothing mechanical enforces that noticing, and the failure mode is silent: the two drift, one of them keeps a bug the other fixed, and only the entity whose page happens to be opened shows it. That is the definition of the duplication this project's review standard treats as a defect, and no amount of cross-referencing prose in package comments substitutes for the compiler.

**The risk the earlier draft flagged is real, and the seam is drawn to avoid it.** Two callers with genuinely different pagination, filtering and failure semantics is exactly where a premature abstraction goes wrong. So the Gate owns *only* the parts that are identical by nature, and the parts that differ stay with the caller behind a one-function seam:

| The Gate owns | The caller owns |
|---|---|
| The single-statement lease claim and its expiry cutoff | The SQL that implements the claim, and its table |
| The `due` policy: never-attempted, TTL, failed-backoff, expired-lease | The TTL and backoff *values* (passed in as `Policy`) |
| The bounded slot channel and its panic-safe release | — |
| The detached goroutine on a non-request context, and `FetchTimeout` | Everything the goroutine does: **pagination shape**, **`album_group` filtering**, **empty-listing detection**, `ErrTruncated` handling, the single transaction |
| Recording a failure on `context.WithoutCancel` within `RecordTimeout` | The SQL that records it, and what counts as a failure at all |
| `Close`, with the `closing`-under-mutex ordering | Delegating `Service.Close()` to it |
| The four `Outcome` values and the order the four-way decision is made in | Its own ready-predicate, its own struct, and **every word of copy** |

The seam is a single `Fill func(ctx context.Context, id string) error`. Everything genuinely different between the two callers happens inside that function, and the Gate's only interest in it is whether it returned an error. That is why the callers' divergence — `spotify.AlbumTracks` versus `spotify.ArtistAlbums`, `errEmptyListing` versus `errEmptyDiscography`, and above all the rule in item 2 above (an empty *counted* set is a failure for albums and an ordinary success for artists) — costs the abstraction nothing: none of it is on the Gate's side of the line.

**What the extraction buys beyond removing the copy**, and these are real rather than consolation:

- The race guard is strengthened, not merely relocated: it is asserted at **both** layers (Task 4, steps 2–3 to write them and step 10 to prove they fire), and a mutation that leaves `Close` correct while blinding the behavioural detector — which today passes 30 runs out of 30, as `albumtracks_test.go:1022` records — is caught for the first time.
- `lazyfetch.New` can enforce `LeaseTTL > FetchTimeout` centrally. Today that invariant is a comment in each package, checked by nobody; a fetch that outlives its own lease lets a second replica start a duplicate walk.
- The four state strings get one definition instead of two that a test has to keep aligned.

**What is still NOT shared, deliberately:** the SQL and its tables (two migrations, two repositories — `album_track_fetches` is merged and out of scope, and generating SQL from a table name is worse than two statements whose comments are their value); the row types; the client methods; the DTOs; and all copy.

**Do not modify** `album_tracks`, `album_track_fetches`, `internal/spotify/albumtracks.go`, `internal/store/catalog/albumtracks.go`, or the never-played panel's copy and behaviour. `internal/albumtracks` **is** modified, by Task 4 and only by Task 4, and only to delegate; its `Listing` semantics, its exported names, its error strings and its `New` signature are all unchanged, which Task 4's gate is that the rest of its suite passes untouched. `web/src/pages/AlbumDetail.tsx` is modified by Task 7, and only to delete mechanism that moves to `fetchpoll.ts`.

### The config-gate decision

**A new key of its own: `ENCORE_ARTIST_ALBUMS_ENABLED` (default `true`), with `ENCORE_ARTIST_ALBUMS_TTL` (default `168h`). `ENCORE_ALBUM_TRACKS_*` is left exactly as it is.**

Three reasons, in order of weight:

1. **A rename fails open.** Operators may already have `ENCORE_ALBUM_TRACKS_ENABLED=false` set. Renaming it to a shared key means that setting stops being read, silently falls back to the default `true`, and the instance starts making the unattended requests its operator explicitly refused. There is no deprecation machinery in `internal/config` to warn them. A config rename that fails *open* on the exact setting whose purpose is to close something is the worst available outcome.
2. **The budgets differ by nearly an order of magnitude.** `ENCORE_ALBUM_TRACKS_ENABLED`'s documented cost is `ceil(album_tracks/50)` ≈ 1 request per album viewed. A discography with `appears_on` included is `ceil(releases/50)`, up to 7 for a prolific artist. An operator who accepted the first did not thereby accept the second.
3. **Reuse removes a control.** One key means turning off discography also turns off tracklists, and vice versa. Two keys cost an operator nothing and let them keep the cheap feature.

The TTL default differs deliberately: **7 days, not 30.** An album's track list is immutable after release (a re-issue gets a new album id), which is what justifies 30 days there. A *discography grows* — a release put out today should appear in "4 of 11" within a week, not within a month. Task 3 carries that reasoning into `docs/configuration.md`.

### The `album_group` copy problem, and how it is solved

"4 of 11 albums" silently excludes singles, compilations and appearances, and for an artist with 340 releases that is an overclaim by omission. Three mechanisms, all of them required:

1. **Every group is stored, not just `album`.** `include_groups` is deliberately **not** sent, so the response carries all four. Filtering at fetch time would (a) make an artist whose every release is a single store zero rows, which is indistinguishable on disk from a failure — the exact trap `migrations/00013_album_tracks.sql` exists to avoid — and (b) leave the page with nothing to name.
2. **The API reports what it set aside.** `excluded: { singles, compilations, appearsOn, other }` travels with every `ready` response. `other` is any group Spotify sends that is none of the four documented ones; it exists so the breakdown always sums to the stored total rather than quietly undercounting if Spotify adds a group. `Task 6` pins `coverage.total + singles + compilations + appearsOn + other == listed`.
3. **The exclusion is in the panel's description, which sits above every body** — including `disabled`, `pending` and `unavailable`, where nothing has been counted. It is phrased as the panel's *rule* ("Singles, compilations and appearances are not counted"), which is true above all of them, rather than as a claim about a completed count, which would be false above three of them. Beneath the body, a second line names the actual numbers: *"Spotify also lists 42 singles, 3 compilations and 7 appearances for this artist, which this panel does not count."*

And the fourth thing that falls out of it: **an artist with no `album`-group releases is a `ready` state with `coverage.total == 0`**, not an error and not "you have played everything". It gets its own body and its own copy (Task 8, body **D6**).

---

## Global Constraints

- **No new Go module dependency and no new npm dependency.** `go.mod` and `web/package.json` are unchanged at the end.
- **No user scope.** `/v1/artists/{id}/albums` is public catalogue data and uses `getAsApp` (`internal/spotify/catalog.go:105`). **Do not add a scope to `DefaultScopes()`.**
- Next migration number is **`00014_`**. House style is `migrations/00013_album_tracks.sql`: goose `Up` *and* `Down`, both directions working, reasoning in comments including what was considered and rejected.
- `Store.InTx(ctx, func(ctx, tx pgx.Tx) error)` is the single-transaction idiom. Repository methods take the caller's `store.Querier` and never reach for `r.db`.
- `internal/httpapi` contains no SQL and never imports pgx. It reaches services and repositories through narrow interfaces.
- **Failure is never emptiness.** A `200` with an empty items array is a *failure*, never a stored "this artist has no releases". `spotify.ArtistAlbums` returns `(nil, nil)` for that case, exactly as `spotify.AlbumTracks` does — Task 2 pins the shape, Task 5 records it as `errEmptyDiscography`.
- **`ErrTruncated` is a failure with no exceptions.** It arrives *with* usable-looking data. The guard is `if err != nil` with no special case.
- **Delete-absent refuses an empty listing at the repository**, wrapping `domain.ErrValidation`.
- **Rows and status commit in ONE transaction**, enforced by the service inside `Store.InTx`. No combined repository method.
- **Anything reaching a `text` column goes through `store.Truncate`** (`internal/store/store.go`) — rune-safe.
- **The claim is a single-statement lease** whose `RETURNING` is empty for the loser. No read-then-write, no catching a uniqueness violation in Go, and an expiry so a killed process cannot strand a row.
- **`Close` must be safe against a concurrent `start`**, and `svc.Close()` must be deferred *after* `pool.Close()` so LIFO runs it first.
- **`disabled` must never read as a Spotify failure**; `unavailable` only from a recorded failure; **`pending` can persist indefinitely**, so the page caps its own polling.
- **Regenerate `docker-compose.portainer.yml` with `./scripts/gen-portainer-stack.sh`** after adding any config key. CI diffs it and `test/deploy` does not cover it. This was 2e-i's only Critical.
- **A 429 on an app-token request pauses Spotify instance-wide** (`app_settings.spotify_paused_until`), 409ing sync-now for every user. This adds a second unattended trigger; it is documented in `docs/configuration.md` and `docs/api.md` where an operator will read it (Tasks 3 and 6).
- Test DB on port **5433**, not 5432. `make` is NOT installed.
- `go test -race` will NOT work locally: no gcc. Omit it. CI runs `go test -tags=integration -race -count=1 -p 1 ./test/...` on Linux.
- **Tagged suites share one database — run one package at a time**, e.g. `go test -tags=integration -count=1 ./test/integration/ -run TestArtistAlbum`.
- staticcheck at `$(go env GOPATH)/bin`; `export PATH="$PATH:$(go env GOPATH)/bin"` first.
- **NUL check every file you write:** `perl -0777 -ne 'print "NULs: ", tr/\0//, "\n"' <file>` — expect 0.
- Commit style `Area: lowercase summary`, body explaining *why*, ending `Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>`. Stage paths explicitly; never `git commit -a`.
- **Every test in this plan carries a "Fails when:" line.** If you add a test of your own and cannot write that line, the test cannot fail and must be replaced.

---

## Task 1: The schema and the repository

**Files:**
- Create: `migrations/00014_artist_albums.sql`
- Create: `internal/store/catalog/artistalbums.go`
- Create: `test/integration/artistalbums_test.go`
- Modify: `internal/store/catalog/catalog_test.go` (append one test)

**Interfaces:**
- Consumes: `store.Querier`, `store.Truncate`, `postgres.Classify`, `domain.ErrValidation`, `domain.ErrNotFound` — all at head.
- Produces, on `*catalog.Repo`:
  - `ArtistAlbumState(ctx context.Context, q store.Querier, artistID string) (catalog.ArtistAlbumState, error)`
  - `ArtistAlbums(ctx context.Context, q store.Querier, artistID string) ([]catalog.ArtistAlbum, error)`
  - `ClaimArtistAlbumFetch(ctx context.Context, q store.Querier, artistID string, now, leaseCutoff time.Time) (bool, error)`
  - `ReplaceArtistAlbums(ctx context.Context, q store.Querier, artistID string, items []catalog.ArtistAlbum) error`
  - `MarkArtistAlbumsFetched(ctx context.Context, q store.Querier, artistID string, at time.Time) error`
  - `FailArtistAlbumFetch(ctx context.Context, q store.Querier, artistID string, at time.Time, reason string) error`
  - types `catalog.ArtistAlbum{AlbumID, Name, Group string; ReleaseDate *time.Time; ReleasePrecision string; Position int}`, `catalog.ArtistAlbumState{Status string; FetchedAt, AttemptedAt time.Time; Attempts int}`
  - constants `catalog.ArtistAlbumFetching = "fetching"`, `ArtistAlbumOK = "ok"`, `ArtistAlbumFailed = "failed"`, `AlbumGroupAlbum = "album"`, `AlbumGroupSingle = "single"`, `AlbumGroupCompilation = "compilation"`, `AlbumGroupAppearsOn = "appears_on"`

- [ ] **Step 1: Write the migration**

Create `migrations/00014_artist_albums.sql`:

```sql
-- +goose Up

-- What Spotify lists as an artist's own releases, so Encore can say "you have
-- heard 4 of this artist's 11 albums".
--
-- No stored field counts an artist's releases. albums.total_tracks answers the
-- same question one album down (§5.1), and there is no equivalent one level up:
-- `albums` holds only records somebody played, so counting rows there would
-- answer "how many of their albums have you played" with the numerator and call
-- it the denominator.
--
-- Global rather than per-user, like every other catalogue table: two listeners
-- on one instance who open the same artist share one listing and one fetch.
CREATE TABLE artist_albums (
    artist_id         text    NOT NULL REFERENCES artists (id) ON DELETE CASCADE,
    -- Deliberately without a foreign key to `albums`, and this matters more
    -- here than it did for album_tracks.track_id in 00013.
    --
    -- Most of a discography is records nobody has played, which are by
    -- definition absent from `albums`: that table is minted from listening. For
    -- a listener who knows one record by an artist, a foreign key would reject
    -- every other row in the listing — that is, almost all of it. Minting
    -- 'pending' albums for each instead would hand the enrichment worker
    -- hundreds of rows per artist view, for music nobody listened to, to learn
    -- names this very response already carried.
    album_id          text    NOT NULL,
    name              text    NOT NULL DEFAULT '',
    -- Spotify's album_group: 'album', 'single', 'compilation' or 'appears_on'.
    -- It is what the artist stands in relation to the record, not what the
    -- record is (album_type), which is why coverage is taken over it.
    --
    -- Every group is stored, not only 'album', and the filter is applied on
    -- read. Filtering at fetch time would store zero rows for an artist who has
    -- only released singles, which is indistinguishable on disk from a failed
    -- read — the ambiguity the sibling table below exists to remove — and would
    -- leave the page unable to say what it set aside, so "4 of 11 albums" would
    -- silently omit 340 other releases.
    --
    -- No CHECK constraint, on purpose. A group Spotify adds later must not make
    -- the INSERT fail: the write that stores the listing would be rejected, the
    -- row would stay 'fetching' from the claim, and the retry would be rejected
    -- the same way for ever — a permanent strand wearing the mask of a retry
    -- loop, which is the same failure mode store.Truncate exists to prevent one
    -- column over.
    album_group       text    NOT NULL DEFAULT '',
    -- NULL when Spotify gave no date. Stored as a date with its precision
    -- beside it, exactly as albums.release_date is, so "2016" and "2016-05-20"
    -- are both representable and distinguishable.
    release_date      date,
    release_precision text    NOT NULL DEFAULT '',
    -- Where the release fell in the walk. Kept only to break ties in the read
    -- order below, so a listing does not reshuffle between page views.
    position          integer NOT NULL DEFAULT 0,
    PRIMARY KEY (artist_id, album_id)
);

-- One row per artist Encore has tried to enumerate, holding the outcome of the
-- last attempt.
--
-- Separate from artist_albums for the reason 00013 gives at length: the absence
-- of rows there is ambiguous, and the difference is the whole feature:
--
--   never fetched     -> "Encore is asking Spotify for this discography"
--   the fetch failed  -> "Encore could not read this discography"
--   fetched, is empty -> impossible, and recorded as a failure; an artist in
--                        this catalogue is there because somebody played a
--                        track by them, so a 200 with no items means the artist
--                        is invisible to this application's market
--
-- Note what is *not* in that list: an artist whose releases are all singles is
-- an ordinary artist and an ordinary success. Zero rows is a failure; zero rows
-- **of album_group 'album'** is a fact about their catalogue, and the page says
-- so in its own words. The emptiness guard is on the whole listing, never on
-- the filtered subset.
--
-- 'fetching' doubles as a lease, expired by attempted_at, so a process killed
-- mid-fetch does not strand an artist in it for ever; without that, a browser
-- polling for the discography would poll for ever too.
CREATE TABLE artist_album_fetches (
    artist_id    text        PRIMARY KEY REFERENCES artists (id) ON DELETE CASCADE,
    status       text        NOT NULL
                             CHECK (status IN ('fetching', 'ok', 'failed')),
    -- When artist_albums was last replaced successfully. NULL until one
    -- succeeds, never cleared afterwards: a failure that follows a success
    -- leaves the older listing readable and says when it was read.
    fetched_at   timestamptz,
    -- When the most recent attempt started, successful or not. Drives both the
    -- lease and the retry backoff after a failure.
    attempted_at timestamptz NOT NULL,
    attempts     integer     NOT NULL DEFAULT 0,
    -- The last failure, for an operator reading the table. Never rendered to a
    -- listener.
    last_error   text        NOT NULL DEFAULT ''
);

-- No secondary index on either table.
--
-- Both are read by their own leading key and by nothing else: artist_albums by
-- (artist_id), which its primary key leads, and artist_album_fetches by
-- (artist_id), which is its primary key. Same reasoning as 00013.
--
-- In particular there is no index supporting "every artist whose discography is
-- stale", and none supporting a lookup by album_id. Nothing asks either
-- question: §5.2 rejects a background sweep explicitly, and album_id is only
-- ever read out of a listing, never searched for.

-- +goose Down
DROP TABLE artist_album_fetches;
DROP TABLE artist_albums;
```

- [ ] **Step 2: Run the migration test to verify it applies both ways**

Run: `go test -tags=integration -count=1 ./test/integration/ -run TestMigrations -v`
Expected: PASS. `test/integration/migrate_test.go` applies every migration up and down.

**Fails when:** the `Down` block is missing, or `DROP TABLE artist_albums` precedes `DROP TABLE artist_album_fetches` in a way the FKs refuse.

- [ ] **Step 3: Write the failing repository integration tests**

Create `test/integration/artistalbums_test.go`:

```go
//go:build integration

package integration

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/RequiDev/encore/internal/domain"
	"github.com/RequiDev/encore/internal/store/catalog"
	"github.com/RequiDev/encore/test/harness"
)

// seedArtist puts one artist in the catalogue so the foreign keys are
// satisfiable. Note what it does *not* do: seed any of the albums the listings
// below refer to. artist_albums deliberately has no foreign key to `albums`,
// because most of a discography is records nobody played.
func seedArtist(t *testing.T, env *harness.Env, id string) {
	t.Helper()
	if _, err := env.Store.DB().Exec(context.Background(),
		`INSERT INTO artists (id, name) VALUES ($1, 'A Test Artist')`, id); err != nil {
		t.Fatalf("seed artist: %v", err)
	}
}

func day(y int, m time.Month, d int) *time.Time {
	at := time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
	return &at
}

// TestArtistAlbumsStoresReleasesForAlbumsNobodyHasPlayed is the property that
// separates this table from album_tracks: its album_id column has no foreign
// key, and if one were added this test fails immediately, because not one of
// these three ids is in `albums`.
func TestArtistAlbumsStoresReleasesForAlbumsNobodyHasPlayed(t *testing.T) {
	env := harness.New(t)
	ctx := context.Background()
	seedArtist(t, env, "artist00000000000000001")

	in := []catalog.ArtistAlbum{
		{AlbumID: "album00000000000000001", Name: "First", Group: catalog.AlbumGroupAlbum,
			ReleaseDate: day(2016, time.May, 20), ReleasePrecision: "day", Position: 0},
		{AlbumID: "album00000000000000002", Name: "A Single", Group: catalog.AlbumGroupSingle,
			ReleaseDate: day(2018, time.March, 1), ReleasePrecision: "day", Position: 1},
		{AlbumID: "album00000000000000003", Name: "No Date", Group: catalog.AlbumGroupAlbum,
			ReleaseDate: nil, ReleasePrecision: "", Position: 2},
	}
	if err := env.Catalog.ReplaceArtistAlbums(ctx, env.Store.DB(), "artist00000000000000001", in); err != nil {
		t.Fatalf("ReplaceArtistAlbums: %v", err)
	}

	got, err := env.Catalog.ArtistAlbums(ctx, env.Store.DB(), "artist00000000000000001")
	if err != nil {
		t.Fatalf("ArtistAlbums: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d rows, want 3", len(got))
	}
	// Newest first, undated last, so a "never played" list reads as a
	// discography does.
	want := []string{"A Single", "First", "No Date"}
	for i, name := range want {
		if got[i].Name != name {
			t.Fatalf("row %d is %q, want %q — the listing is not newest-first with undated last",
				i, got[i].Name, name)
		}
	}
	if got[0].Group != catalog.AlbumGroupSingle {
		t.Fatalf("group = %q, want %q: album_group did not round-trip", got[0].Group, catalog.AlbumGroupSingle)
	}
	if got[2].ReleaseDate != nil {
		t.Fatalf("undated release came back with %v, want nil", *got[2].ReleaseDate)
	}
}

// TestReplaceArtistAlbumsRefusesAnEmptyListing is the critical property this
// file exists to enforce: album_id <> ALL('{}') is vacuously true, so an empty
// (or all-blank-id) items would otherwise delete every row the artist has.
// Spotify's own client returns exactly that shape — a 200 with an empty items
// array and no error — for a market where an artist is invisible. "This artist
// has released nothing" is not a state migrations/00014_artist_albums.sql
// allows: it must be recorded as a failed attempt, never as a successful, empty
// one.
func TestReplaceArtistAlbumsRefusesAnEmptyListing(t *testing.T) {
	env := harness.New(t)
	ctx := context.Background()
	seedArtist(t, env, "artist00000000000000001")

	seeded := []catalog.ArtistAlbum{
		{AlbumID: "album00000000000000001", Name: "First", Group: catalog.AlbumGroupAlbum, Position: 0},
		{AlbumID: "album00000000000000002", Name: "Second", Group: catalog.AlbumGroupAlbum, Position: 1},
	}
	if err := env.Catalog.ReplaceArtistAlbums(ctx, env.Store.DB(), "artist00000000000000001", seeded); err != nil {
		t.Fatalf("seed replace: %v", err)
	}

	if err := env.Catalog.ReplaceArtistAlbums(ctx, env.Store.DB(), "artist00000000000000001", nil); err == nil {
		t.Fatal("ReplaceArtistAlbums(nil) succeeded; want it refused before the good listing could be wiped")
	} else if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("ReplaceArtistAlbums(nil) error = %v, want domain.ErrValidation", err)
	}

	allBlank := []catalog.ArtistAlbum{{AlbumID: "", Name: "ghost", Group: catalog.AlbumGroupAlbum}}
	if err := env.Catalog.ReplaceArtistAlbums(ctx, env.Store.DB(), "artist00000000000000001", allBlank); err == nil {
		t.Fatal("ReplaceArtistAlbums(all blank ids) succeeded; want it refused")
	} else if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("ReplaceArtistAlbums(all blank ids) error = %v, want domain.ErrValidation", err)
	}

	got, err := env.Catalog.ArtistAlbums(ctx, env.Store.DB(), "artist00000000000000001")
	if err != nil {
		t.Fatalf("ArtistAlbums: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d rows after the refused replaces, want the original 2 untouched", len(got))
	}
}

// TestReplaceArtistAlbumsIsAllSinglesAndStillASuccess is the property with no
// counterpart in 2e-i. An album with no tracks is impossible; an artist whose
// every release is a single is ordinary. The repository must store it happily,
// because the emptiness guard is on the whole listing and never on the filtered
// subset.
func TestReplaceArtistAlbumsIsAllSinglesAndStillASuccess(t *testing.T) {
	env := harness.New(t)
	ctx := context.Background()
	seedArtist(t, env, "artist00000000000000001")

	in := []catalog.ArtistAlbum{
		{AlbumID: "album00000000000000001", Name: "One", Group: catalog.AlbumGroupSingle, Position: 0},
		{AlbumID: "album00000000000000002", Name: "Two", Group: catalog.AlbumGroupSingle, Position: 1},
	}
	if err := env.Catalog.ReplaceArtistAlbums(ctx, env.Store.DB(), "artist00000000000000001", in); err != nil {
		t.Fatalf("ReplaceArtistAlbums with no album-group release: %v", err)
	}
	got, err := env.Catalog.ArtistAlbums(ctx, env.Store.DB(), "artist00000000000000001")
	if err != nil {
		t.Fatalf("ArtistAlbums: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d rows, want 2: an all-singles discography was refused or dropped", len(got))
	}
}

// TestReplaceArtistAlbumsDeletesWhatIsAbsent uses a genuinely different second
// input — one release withdrawn, one reclassified, one renamed — rather than
// re-submitting the same set, which would exercise nothing.
func TestReplaceArtistAlbumsDeletesWhatIsAbsent(t *testing.T) {
	env := harness.New(t)
	ctx := context.Background()
	seedArtist(t, env, "artist00000000000000001")

	before := []catalog.ArtistAlbum{
		{AlbumID: "album00000000000000001", Name: "First", Group: catalog.AlbumGroupAlbum,
			ReleaseDate: day(2016, time.May, 20), ReleasePrecision: "day", Position: 0},
		{AlbumID: "album00000000000000002", Name: "Second", Group: catalog.AlbumGroupAlbum,
			ReleaseDate: day(2018, time.May, 20), ReleasePrecision: "day", Position: 1},
		{AlbumID: "album00000000000000003", Name: "Third", Group: catalog.AlbumGroupAlbum,
			ReleaseDate: day(2020, time.May, 20), ReleasePrecision: "day", Position: 2},
	}
	if err := env.Catalog.ReplaceArtistAlbums(ctx, env.Store.DB(), "artist00000000000000001", before); err != nil {
		t.Fatalf("first replace: %v", err)
	}

	after := []catalog.ArtistAlbum{
		{AlbumID: "album00000000000000001", Name: "First (Remastered)", Group: catalog.AlbumGroupAlbum,
			ReleaseDate: day(2021, time.January, 1), ReleasePrecision: "day", Position: 0},
		// Reclassified from album to compilation by Spotify.
		{AlbumID: "album00000000000000003", Name: "Third", Group: catalog.AlbumGroupCompilation,
			ReleaseDate: day(2020, time.May, 20), ReleasePrecision: "day", Position: 1},
	}
	if err := env.Catalog.ReplaceArtistAlbums(ctx, env.Store.DB(), "artist00000000000000001", after); err != nil {
		t.Fatalf("second replace: %v", err)
	}

	got, err := env.Catalog.ArtistAlbums(ctx, env.Store.DB(), "artist00000000000000001")
	if err != nil {
		t.Fatalf("ArtistAlbums: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d rows, want 2: the release absent from the second listing was not deleted", len(got))
	}
	if got[0].Name != "First (Remastered)" || got[0].ReleaseDate == nil || got[0].ReleaseDate.Year() != 2021 {
		t.Fatalf("first row = %+v, want the renamed and redated one: ON CONFLICT did not refresh every column", got[0])
	}
	if got[1].Group != catalog.AlbumGroupCompilation {
		t.Fatalf("second row group = %q, want %q: ON CONFLICT did not refresh album_group, so a "+
			"reclassified release would keep counting towards completion",
			got[1].Group, catalog.AlbumGroupCompilation)
	}
}

func TestClaimArtistAlbumFetchIsExclusive(t *testing.T) {
	env := harness.New(t)
	ctx := context.Background()
	seedArtist(t, env, "artist00000000000000001")

	now := time.Date(2026, time.July, 31, 12, 0, 0, 0, time.UTC)
	cutoff := now.Add(-3 * time.Minute)

	first, err := env.Catalog.ClaimArtistAlbumFetch(ctx, env.Store.DB(), "artist00000000000000001", now, cutoff)
	if err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if !first {
		t.Fatal("the first claim lost; nothing was holding the lease")
	}

	second, err := env.Catalog.ClaimArtistAlbumFetch(ctx, env.Store.DB(), "artist00000000000000001",
		now.Add(time.Second), cutoff.Add(time.Second))
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	if second {
		t.Fatal("the second claim won as well; two tabs would each spend a discography walk")
	}
}

// TestClaimArtistAlbumFetchDoesNotReclaimExactlyAtTheCutoff pins the boundary
// an "expired by miles" test cannot: the comparison must be a strict <, because
// attempted_at equal to the cutoff is a lease that has only just reached the
// end of its window, and reclaiming it early lets a second replica take over a
// walk the first one might still finish.
func TestClaimArtistAlbumFetchDoesNotReclaimExactlyAtTheCutoff(t *testing.T) {
	env := harness.New(t)
	ctx := context.Background()
	seedArtist(t, env, "artist00000000000000001")

	start := time.Date(2026, time.July, 31, 12, 0, 0, 0, time.UTC)
	if _, err := env.Catalog.ClaimArtistAlbumFetch(ctx, env.Store.DB(), "artist00000000000000001",
		start, start.Add(-3*time.Minute)); err != nil {
		t.Fatalf("first claim: %v", err)
	}

	stillLive, err := env.Catalog.ClaimArtistAlbumFetch(ctx, env.Store.DB(), "artist00000000000000001",
		start.Add(time.Minute), start)
	if err != nil {
		t.Fatalf("claim at the exact cutoff: %v", err)
	}
	if stillLive {
		t.Fatal("a lease exactly at the cutoff was reclaimed; the comparison is <=, not the required strict <")
	}

	// A microsecond, not a nanosecond: timestamptz stores microsecond precision,
	// so a nanosecond offset would round-trip to the same stored instant and
	// prove nothing.
	expired, err := env.Catalog.ClaimArtistAlbumFetch(ctx, env.Store.DB(), "artist00000000000000001",
		start.Add(time.Minute), start.Add(time.Microsecond))
	if err != nil {
		t.Fatalf("claim one microsecond past the cutoff: %v", err)
	}
	if !expired {
		t.Fatal("a lease one microsecond past its cutoff was not reclaimed")
	}
}

func TestFailArtistAlbumFetchKeepsTheOlderListing(t *testing.T) {
	env := harness.New(t)
	ctx := context.Background()
	seedArtist(t, env, "artist00000000000000001")

	at := time.Date(2026, time.July, 31, 12, 0, 0, 0, time.UTC)
	if err := env.Catalog.ReplaceArtistAlbums(ctx, env.Store.DB(), "artist00000000000000001",
		[]catalog.ArtistAlbum{{AlbumID: "album00000000000000001", Name: "First",
			Group: catalog.AlbumGroupAlbum, Position: 0}},
	); err != nil {
		t.Fatalf("ReplaceArtistAlbums: %v", err)
	}
	if err := env.Catalog.MarkArtistAlbumsFetched(ctx, env.Store.DB(), "artist00000000000000001", at); err != nil {
		t.Fatalf("MarkArtistAlbumsFetched: %v", err)
	}

	later := at.Add(8 * 24 * time.Hour)
	if err := env.Catalog.FailArtistAlbumFetch(ctx, env.Store.DB(), "artist00000000000000001",
		later, "spotify: artist albums: context deadline exceeded"); err != nil {
		t.Fatalf("FailArtistAlbumFetch: %v", err)
	}

	rows, err := env.Catalog.ArtistAlbums(ctx, env.Store.DB(), "artist00000000000000001")
	if err != nil {
		t.Fatalf("ArtistAlbums: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows after a failure, want the 1 that was already stored", len(rows))
	}

	st, err := env.Catalog.ArtistAlbumState(ctx, env.Store.DB(), "artist00000000000000001")
	if err != nil {
		t.Fatalf("ArtistAlbumState: %v", err)
	}
	if st.Status != catalog.ArtistAlbumFailed {
		t.Fatalf("status = %q, want %q", st.Status, catalog.ArtistAlbumFailed)
	}
	if !st.FetchedAt.Equal(at) {
		t.Fatalf("fetchedAt = %v, want it untouched at %v: a failure erased when the good listing was read",
			st.FetchedAt, at)
	}
}

// TestFailArtistAlbumFetchTruncatesOnARuneBoundary pins that a long failure
// reason is cut on a rune boundary. A byte cut through a multi-byte rune
// produces invalid UTF-8, Postgres rejects the write outright, and the row
// stays at whatever status the claim left it in — a permanent strand disguised
// as a retry loop.
func TestFailArtistAlbumFetchTruncatesOnARuneBoundary(t *testing.T) {
	env := harness.New(t)
	ctx := context.Background()
	seedArtist(t, env, "artist00000000000000001")

	// 499 ASCII bytes, then a 2-byte rune whose second byte lands exactly at
	// offset 500 — the one byte a naive s[:500] would cut on.
	reason := strings.Repeat("x", 499) + "é" + strings.Repeat("y", 50)
	if err := env.Catalog.FailArtistAlbumFetch(ctx, env.Store.DB(), "artist00000000000000001",
		time.Date(2026, time.July, 31, 12, 0, 0, 0, time.UTC), reason); err != nil {
		t.Fatalf("FailArtistAlbumFetch: %v", err)
	}

	var stored string
	if err := env.Store.DB().QueryRow(ctx,
		`SELECT last_error FROM artist_album_fetches WHERE artist_id = $1`,
		"artist00000000000000001").Scan(&stored); err != nil {
		t.Fatalf("read last_error: %v", err)
	}
	if !utf8.ValidString(stored) {
		t.Fatalf("stored last_error is not valid UTF-8: %q", stored)
	}
}

// TestMarkArtistAlbumsFetchedResetsAttempts pins the backoff invariant: after a
// run of failures followed by one success, the count reads back as a fresh
// success rather than as one more failed attempt.
func TestMarkArtistAlbumsFetchedResetsAttempts(t *testing.T) {
	env := harness.New(t)
	ctx := context.Background()
	seedArtist(t, env, "artist00000000000000001")

	start := time.Date(2026, time.July, 31, 12, 0, 0, 0, time.UTC)
	for i := range 3 {
		at := start.Add(time.Duration(i) * time.Minute)
		if _, err := env.Catalog.ClaimArtistAlbumFetch(ctx, env.Store.DB(),
			"artist00000000000000001", at, at); err != nil {
			t.Fatalf("claim %d: %v", i+1, err)
		}
	}
	if st, err := env.Catalog.ArtistAlbumState(ctx, env.Store.DB(), "artist00000000000000001"); err != nil {
		t.Fatalf("ArtistAlbumState before success: %v", err)
	} else if st.Attempts != 3 {
		t.Fatalf("attempts before success = %d, want 3 (setup did not reach the state this test needs)", st.Attempts)
	}

	if err := env.Catalog.ReplaceArtistAlbums(ctx, env.Store.DB(), "artist00000000000000001",
		[]catalog.ArtistAlbum{{AlbumID: "album00000000000000001", Name: "First",
			Group: catalog.AlbumGroupAlbum, Position: 0}},
	); err != nil {
		t.Fatalf("ReplaceArtistAlbums: %v", err)
	}
	if err := env.Catalog.MarkArtistAlbumsFetched(ctx, env.Store.DB(), "artist00000000000000001",
		start.Add(10*time.Minute)); err != nil {
		t.Fatalf("MarkArtistAlbumsFetched: %v", err)
	}

	st, err := env.Catalog.ArtistAlbumState(ctx, env.Store.DB(), "artist00000000000000001")
	if err != nil {
		t.Fatalf("ArtistAlbumState after success: %v", err)
	}
	if st.Attempts != 1 {
		t.Fatalf("attempts after success = %d, want 1: a healthy artist must not carry a stale "+
			"failure count into the next backoff calculation", st.Attempts)
	}
}

func TestArtistAlbumStateIsEmptyBeforeAnyAttempt(t *testing.T) {
	env := harness.New(t)
	seedArtist(t, env, "artist00000000000000001")

	st, err := env.Catalog.ArtistAlbumState(context.Background(), env.Store.DB(), "artist00000000000000001")
	if err != nil {
		t.Fatalf("ArtistAlbumState: %v", err)
	}
	if st.Status != "" {
		t.Fatalf("status = %q, want the empty string for an artist never attempted", st.Status)
	}
	if !st.FetchedAt.IsZero() {
		t.Fatalf("fetchedAt = %v, want the zero time", st.FetchedAt)
	}
}

func TestArtistAlbumsCascadeWithTheArtist(t *testing.T) {
	env := harness.New(t)
	ctx := context.Background()
	seedArtist(t, env, "artist00000000000000001")
	if err := env.Catalog.ReplaceArtistAlbums(ctx, env.Store.DB(), "artist00000000000000001",
		[]catalog.ArtistAlbum{{AlbumID: "album00000000000000001", Name: "First",
			Group: catalog.AlbumGroupAlbum, Position: 0}},
	); err != nil {
		t.Fatalf("ReplaceArtistAlbums: %v", err)
	}
	if _, err := env.Store.DB().Exec(ctx,
		`DELETE FROM artists WHERE id = $1`, "artist00000000000000001"); err != nil {
		t.Fatalf("delete artist: %v", err)
	}

	var n int
	if err := env.Store.DB().QueryRow(ctx,
		`SELECT count(*)::int FROM artist_albums WHERE artist_id = $1`,
		"artist00000000000000001").Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("%d artist_albums rows survived their artist, want 0", n)
	}
}
```

**Fails when:**
- `…StoresReleasesForAlbumsNobodyHasPlayed` — a foreign key is added on `album_id`, or the read order is changed to `position` first, or `release_date` NULL handling drops the row.
- `…RefusesAnEmptyListing` — the `len(ids) == 0` guard is removed from `ReplaceArtistAlbums`, or moved after the `Exec`.
- `…IsAllSinglesAndStillASuccess` — someone "helpfully" filters to `album_group = 'album'` before storing, or adds an emptiness guard on the filtered subset.
- `…DeletesWhatIsAbsent` — the `stale` CTE is dropped, or `ON CONFLICT DO UPDATE` stops refreshing `album_group` (which would leave a reclassified compilation counting towards completion for ever).
- `…IsExclusive` — the claim becomes a read-then-write, or the `WHERE` on the `DO UPDATE` branch is dropped.
- `…NotReclaimExactlyAtTheCutoff` — `attempted_at < $3` becomes `<=`.
- `…KeepsTheOlderListing` — `failArtistAlbumFetchSQL` starts touching `fetched_at` or `artist_albums`.
- `…TruncatesOnARuneBoundary` — `store.Truncate` is replaced by `reason[:500]`.
- `…ResetsAttempts` — `attempts = 1` is dropped from `markArtistAlbumsFetchedSQL`.
- `…IsEmptyBeforeAnyAttempt` — the `ErrNotFound` branch stops returning a zero state and starts returning an error.
- `…CascadeWithTheArtist` — `ON DELETE CASCADE` is dropped.

- [ ] **Step 4: Run the tests to verify they fail**

Run: `go test -tags=integration -count=1 ./test/integration/ -run TestArtistAlbum`
Expected: FAIL — compile error, `env.Catalog.ReplaceArtistAlbums undefined`.

- [ ] **Step 5: Write the repository**

Create `internal/store/catalog/artistalbums.go`:

```go
package catalog

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/RequiDev/encore/internal/domain"
	"github.com/RequiDev/encore/internal/postgres"
	"github.com/RequiDev/encore/internal/store"
)

// maxDiscographyErrorLen bounds what FailArtistAlbumFetch stores in last_error,
// matching maxFailureReasonLen next door and accounts.maxSyncErrorLen before
// it: a driver error carrying a whole HTTP response body has no business being
// replayed to an operator reading the table.
const maxDiscographyErrorLen = 500

// The three states of one artist's discography. See
// migrations/00014_artist_albums.sql for why the outcome lives in its own table
// rather than being inferred from whether artist_albums holds rows.
const (
	// ArtistAlbumFetching is a lease: somebody is reading this discography now.
	ArtistAlbumFetching = "fetching"
	// ArtistAlbumOK means artist_albums holds a complete listing.
	ArtistAlbumOK = "ok"
	// ArtistAlbumFailed means the last attempt did not produce one. Whatever
	// artist_albums holds is from an earlier, successful attempt.
	ArtistAlbumFailed = "failed"
)

// Spotify's four documented album_group values.
//
// They describe what the *artist* is to the record, not what the record is:
// album_type says "album" for a record this artist merely appears on, whereas
// album_group says "appears_on". Coverage is taken over the group for exactly
// that reason.
//
// These are the four Spotify documents, not a closed set the database enforces
// — see the migration's note on why artist_albums.album_group has no CHECK.
const (
	AlbumGroupAlbum       = "album"
	AlbumGroupSingle      = "single"
	AlbumGroupCompilation = "compilation"
	AlbumGroupAppearsOn   = "appears_on"
)

// ArtistAlbum is one release of a cached discography.
type ArtistAlbum struct {
	AlbumID string
	Name    string
	// Group is Spotify's album_group, stored verbatim. Every group is kept and
	// the filter is applied by the reader; see the migration.
	Group            string
	ReleaseDate      *time.Time
	ReleasePrecision string
	Position         int
}

// ArtistAlbumState is the bookkeeping for one artist's discography.
//
// The zero value — Status "" and both instants zero — is an artist who has
// never been attempted, which is an ordinary state rather than an error: every
// artist is in it until somebody first opens their page.
type ArtistAlbumState struct {
	Status      string
	FetchedAt   time.Time
	AttemptedAt time.Time
	Attempts    int
}

const artistAlbumStateSQL = `
SELECT status, coalesce(fetched_at, 'epoch'::timestamptz), attempted_at, attempts
FROM artist_album_fetches
WHERE artist_id = $1`

// ArtistAlbumState reads the outcome of the last attempt on one artist.
func (r *Repo) ArtistAlbumState(
	ctx context.Context, q store.Querier, artistID string,
) (ArtistAlbumState, error) {
	var (
		out     ArtistAlbumState
		fetched time.Time
	)
	err := q.QueryRow(ctx, artistAlbumStateSQL, artistID).
		Scan(&out.Status, &fetched, &out.AttemptedAt, &out.Attempts)
	if err != nil {
		if errors.Is(postgres.Classify("artist album state", err), domain.ErrNotFound) {
			// Never attempted. Not an error: it is the state every artist starts
			// in, and the caller's cue to start the first fetch.
			return ArtistAlbumState{}, nil
		}
		return ArtistAlbumState{}, postgres.Classify("artist album state", err)
	}
	// 'epoch' stands in for NULL so the scan needs no pointer. Anything at or
	// before it means "no successful fetch yet".
	if fetched.Year() > 1970 {
		out.FetchedAt = fetched.UTC()
	}
	out.AttemptedAt = out.AttemptedAt.UTC()
	return out, nil
}

// artistAlbumsSQL reads one artist's discography, newest first.
//
// Newest first because the question the page asks is "what of theirs have I not
// got to yet", and a reader scanning that list starts from the recent end.
// NULLS LAST puts undated releases after everything dated rather than at the
// top, where a missing date would masquerade as the newest record.
//
// position and album_id break ties so the order is total: two releases sharing
// a date would otherwise come back in whatever order the heap happened to hold
// them, and a list that reshuffles between page views looks broken.
const artistAlbumsSQL = `
SELECT album_id, name, album_group, release_date, release_precision
FROM artist_albums
WHERE artist_id = $1
ORDER BY release_date DESC NULLS LAST, position, album_id`

// ArtistAlbums reads one artist's cached discography, every group included.
//
// Filtering to album_group = 'album' is deliberately not done here. The caller
// needs the excluded groups to say what it set aside — "4 of 11 albums" with no
// mention of 340 singles is an overclaim — and a repository that dropped them
// would make that sentence unwriteable.
func (r *Repo) ArtistAlbums(
	ctx context.Context, q store.Querier, artistID string,
) ([]ArtistAlbum, error) {
	rows, err := q.Query(ctx, artistAlbumsSQL, artistID)
	if err != nil {
		return nil, postgres.Classify("artist albums", err)
	}
	defer rows.Close()

	out := make([]ArtistAlbum, 0, 32)
	for rows.Next() {
		var (
			a    ArtistAlbum
			date *time.Time
		)
		if err := rows.Scan(&a.AlbumID, &a.Name, &a.Group, &date, &a.ReleasePrecision); err != nil {
			return nil, postgres.Classify("artist albums", err)
		}
		if date != nil {
			utc := date.UTC()
			a.ReleaseDate = &utc
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, postgres.Classify("artist albums", err)
	}
	return out, nil
}

// claimArtistAlbumFetchSQL takes the lease on one artist, or returns nothing.
//
// The WHERE applies only to the DO UPDATE branch: an artist with no row at all
// is claimed by the INSERT. An artist already 'fetching' is claimed only once
// its attempt has outlived the lease, which is what stops two tabs, two
// browsers or two API replicas each spending a whole discography walk — several
// requests each, not one — on the same artist, and what stops a process killed
// mid-fetch stranding the artist for ever.
//
// This is one statement, not a read followed by a write: Postgres resolves a
// conflicting INSERT ... ON CONFLICT by taking a lock on the existing row and
// making every other transaction targeting the same key wait for it, so two
// concurrent callers are serialised by the database itself rather than by
// anything this Go code does. The loser's DO UPDATE ... WHERE evaluates to
// false once it sees the winner's committed row, so RETURNING is empty for it —
// there is no uniqueness violation to catch, and nothing here decides who won
// by inspecting an error.
//
// Parameters are $1 artist, $2 now, $3 the lease cutoff (now minus the lease).
const claimArtistAlbumFetchSQL = `
INSERT INTO artist_album_fetches (artist_id, status, attempted_at, attempts)
VALUES ($1, 'fetching', $2, 1)
ON CONFLICT (artist_id) DO UPDATE SET
    status       = 'fetching',
    attempted_at = $2,
    attempts     = artist_album_fetches.attempts + 1
WHERE artist_album_fetches.status <> 'fetching'
   OR artist_album_fetches.attempted_at < $3
RETURNING artist_id`

// ClaimArtistAlbumFetch takes the lease, reporting whether this caller won it.
func (r *Repo) ClaimArtistAlbumFetch(
	ctx context.Context, q store.Querier, artistID string, now, leaseCutoff time.Time,
) (bool, error) {
	var got string
	err := q.QueryRow(ctx, claimArtistAlbumFetchSQL, artistID, now.UTC(), leaseCutoff.UTC()).Scan(&got)
	if err != nil {
		classified := postgres.Classify("claim artist album fetch", err)
		if errors.Is(classified, domain.ErrNotFound) {
			// No row came back: somebody else holds a live lease.
			return false, nil
		}
		return false, classified
	}
	return true, nil
}

// replaceArtistAlbumsSQL deletes whatever the incoming discography no longer
// contains and upserts the rest, in one statement — the same
// delete-absent-plus-upsert-present shape as ReplaceAlbumTracks in
// internal/store/catalog/albumtracks.go and ReplaceUserPlaylists in
// internal/store/library/playlists.go.
//
// Every column besides the key can change under a release Encore already knows
// about: a re-issue renames it, a re-dating moves it, and Spotify does
// reclassify a record's album_group. ON CONFLICT therefore refreshes all of
// them — and album_group in particular, because a release reclassified from
// 'album' to 'compilation' that kept its old group would keep counting towards
// completion for ever.
//
// DISTINCT ON collapses a duplicate id within one call, because Postgres
// refuses to let ON CONFLICT touch the same row twice inside one statement and
// a page boundary could in principle repeat one.
//
// The DELETE and the INSERT share one statement, hence one implicit
// transaction: there is no instant at which a concurrent reader can observe the
// tail deleted but the rest not yet upserted.
//
// **Callers must never pass a partial listing here.** The delete is what makes
// that fatal: a prefix deletes the tail of a discography that was correct. It
// is also the caller's job to run this and MarkArtistAlbumsFetched inside the
// same Store.InTx — see the comment on that function.
//
// An empty listing is refused before this statement ever runs — see
// ReplaceArtistAlbums — because album_id <> ALL('{}') is vacuously true and
// would otherwise delete every row an artist has.
//
// Parameters are $1 artist, $2..$7 the parallel arrays.
const replaceArtistAlbumsSQL = `
WITH input AS (
    SELECT DISTINCT ON (album_id) *
    FROM unnest($2::text[], $3::text[], $4::text[], $5::date[], $6::text[], $7::int[])
        AS t(album_id, name, album_group, release_date, release_precision, position)
    ORDER BY album_id
),
stale AS (
    DELETE FROM artist_albums
    WHERE artist_id = $1 AND album_id <> ALL($2::text[])
)
INSERT INTO artist_albums
    (artist_id, album_id, name, album_group, release_date, release_precision, position)
SELECT $1, album_id, name, album_group, release_date, release_precision, position FROM input
ON CONFLICT (artist_id, album_id) DO UPDATE SET
    name              = EXCLUDED.name,
    album_group       = EXCLUDED.album_group,
    release_date      = EXCLUDED.release_date,
    release_precision = EXCLUDED.release_precision,
    position          = EXCLUDED.position`

// ReplaceArtistAlbums makes items the artist's complete discography.
//
// An empty (or all-blank-id) items is refused rather than stored: this is the
// one call that can make artist_albums disappear, and "Spotify listed nothing
// for this artist" is not a state migrations/00014_artist_albums.sql allows —
// it treats a 200 with no items the same as any other failed read. A caller
// that reached this with an empty listing should have called
// FailArtistAlbumFetch instead; refusing here means it cannot get that wrong by
// accident, even for a listing that was genuinely truncated to nothing.
//
// Note what this does *not* refuse: a discography whose every release is a
// single. That is an ordinary artist and an ordinary success. The guard is on
// the whole listing and never on the album_group-filtered subset, which is the
// one place this table's rules differ from album_tracks'.
func (r *Repo) ReplaceArtistAlbums(
	ctx context.Context, q store.Querier, artistID string, items []ArtistAlbum,
) error {
	cols := artistAlbumRows(items)
	if len(cols.ids) == 0 {
		return fmt.Errorf("replace artist albums: %w: refusing to store an empty discography for %q",
			domain.ErrValidation, artistID)
	}
	if _, err := q.Exec(ctx, replaceArtistAlbumsSQL, artistID,
		cols.ids, cols.names, cols.groups, cols.dates, cols.precisions, cols.positions); err != nil {
		return postgres.Classify("replace artist albums", err)
	}
	return nil
}

// artistAlbumColumns is the transposed form the unnest above expects.
type artistAlbumColumns struct {
	ids        []string
	names      []string
	groups     []string
	dates      []*time.Time
	precisions []string
	positions  []int32
}

// artistAlbumRows transposes a discography into parallel arrays, dropping
// entries with a blank id — a keyless row has nothing for ON CONFLICT to place.
// Every slice is non-nil even when items is empty, so an empty batch reaches
// the statement as an empty array rather than SQL NULL.
func artistAlbumRows(items []ArtistAlbum) artistAlbumColumns {
	out := artistAlbumColumns{
		ids:        make([]string, 0, len(items)),
		names:      make([]string, 0, len(items)),
		groups:     make([]string, 0, len(items)),
		dates:      make([]*time.Time, 0, len(items)),
		precisions: make([]string, 0, len(items)),
		positions:  make([]int32, 0, len(items)),
	}
	for _, it := range items {
		if it.AlbumID == "" {
			continue
		}
		out.ids = append(out.ids, it.AlbumID)
		out.names = append(out.names, it.Name)
		out.groups = append(out.groups, it.Group)
		if it.ReleaseDate != nil {
			d := it.ReleaseDate.UTC()
			out.dates = append(out.dates, &d)
		} else {
			out.dates = append(out.dates, nil)
		}
		out.precisions = append(out.precisions, it.ReleasePrecision)
		out.positions = append(out.positions, int32(it.Position))
	}
	return out
}

// markArtistAlbumsFetchedSQL records a success. It clears last_error so a stale
// message cannot be read beside a listing that is now current, and resets
// attempts to 1: a healthy artist that once failed five times before succeeding
// must not have the next transient error backed off as though it were a sixth.
const markArtistAlbumsFetchedSQL = `
INSERT INTO artist_album_fetches
    (artist_id, status, fetched_at, attempted_at, attempts, last_error)
VALUES ($1, 'ok', $2, $2, 1, '')
ON CONFLICT (artist_id) DO UPDATE SET
    status     = 'ok',
    fetched_at = $2,
    attempts   = 1,
    last_error = ''`

// MarkArtistAlbumsFetched records that the discography now stored is complete.
//
// It is deliberately separate from ReplaceArtistAlbums rather than folded into
// it: the caller runs both inside one Store.InTx, so the rows and the claim
// that they are authoritative commit together or not at all. Splitting the two
// writes across separate transactions would let a crash land between them,
// leaving a discography on disk with no 'ok' beside it, or an 'ok' beside a
// listing an interrupted replace never finished writing.
func (r *Repo) MarkArtistAlbumsFetched(
	ctx context.Context, q store.Querier, artistID string, at time.Time,
) error {
	if _, err := q.Exec(ctx, markArtistAlbumsFetchedSQL, artistID, at.UTC()); err != nil {
		return postgres.Classify("mark artist albums fetched", err)
	}
	return nil
}

// failArtistAlbumFetchSQL records a failed attempt.
//
// It touches neither artist_albums nor fetched_at. Whatever discography is
// stored stays stored and keeps saying when it was read: a timeout today is no
// reason to throw away a listing that was correct last week.
const failArtistAlbumFetchSQL = `
INSERT INTO artist_album_fetches (artist_id, status, attempted_at, attempts, last_error)
VALUES ($1, 'failed', $2, 1, $3)
ON CONFLICT (artist_id) DO UPDATE SET
    status       = 'failed',
    attempted_at = $2,
    last_error   = $3`

// FailArtistAlbumFetch records that the last attempt did not produce a listing.
func (r *Repo) FailArtistAlbumFetch(
	ctx context.Context, q store.Querier, artistID string, at time.Time, reason string,
) error {
	// store.Truncate cuts on a rune boundary. A byte offset could slice a
	// multi-byte rune in half and hand Postgres bytes it rejects outright, which
	// would fail the very write meant to record that the fetch failed — the row
	// stays 'fetching' from the claim, the lease eventually expires, a new claim
	// wins, the same fetch fails the same way, and the write is rejected again:
	// a permanent strand disguised as a retry loop.
	reason = store.Truncate(reason, maxDiscographyErrorLen)
	if _, err := q.Exec(ctx, failArtistAlbumFetchSQL, artistID, at.UTC(), reason); err != nil {
		return postgres.Classify("fail artist album fetch", err)
	}
	return nil
}
```

- [ ] **Step 6: Add the one-statement guard to the catalog package's unit tests**

Append to `internal/store/catalog/catalog_test.go`, immediately after `TestReplaceAlbumTracksSQLIsOneStatement`:

```go
// TestReplaceArtistAlbumsSQLIsOneStatement pins the property no integration
// test can: that the delete-absent and upsert-present halves of
// ReplaceArtistAlbums are one statement rather than two run back to back. A
// test built on outcomes cannot tell the two apart, because artist_albums
// carries no timestamp or version column — the state on disk after either
// implementation is identical. A semicolon here would split the string into two
// statements sent in the same Exec call, reopening exactly the window a
// concurrent reader must never see: the tail deleted with the replacement not
// yet written.
func TestReplaceArtistAlbumsSQLIsOneStatement(t *testing.T) {
	if strings.Contains(replaceArtistAlbumsSQL, ";") {
		t.Fatalf("replace statement contains a ';', which would split it into more than one statement:\n%s",
			replaceArtistAlbumsSQL)
	}
	if n := strings.Count(replaceArtistAlbumsSQL, "DELETE FROM artist_albums"); n != 1 {
		t.Errorf("replace statement has %d DELETEs on artist_albums, want exactly 1:\n%s",
			n, replaceArtistAlbumsSQL)
	}
	if n := strings.Count(replaceArtistAlbumsSQL, "INSERT INTO artist_albums"); n != 1 {
		t.Errorf("replace statement has %d INSERTs into artist_albums, want exactly 1:\n%s",
			n, replaceArtistAlbumsSQL)
	}
}

// TestArtistAlbumUpsertRefreshesTheGroup pins the one column whose staleness is
// silently wrong rather than merely cosmetic. A release Spotify reclassifies
// from 'album' to 'compilation' keeps counting towards discography completion
// for as long as the stored group says 'album', so a listener would be told
// they had not heard a record that no longer belongs in the denominator.
func TestArtistAlbumUpsertRefreshesTheGroup(t *testing.T) {
	if !strings.Contains(replaceArtistAlbumsSQL, "album_group       = EXCLUDED.album_group") {
		t.Errorf("the upsert does not refresh album_group, so a reclassified release keeps its old "+
			"group and its old effect on completion:\n%s", replaceArtistAlbumsSQL)
	}
}
```

**Fails when:** a `;` is introduced into `replaceArtistAlbumsSQL`, the DELETE is moved out of the CTE into its own `Exec`, or `album_group = EXCLUDED.album_group` is dropped from the `DO UPDATE` list.

- [ ] **Step 7: Run every test in this task**

```bash
go test -count=1 ./internal/store/catalog/
go test -tags=integration -count=1 ./test/integration/ -run 'TestArtistAlbum|TestReplaceArtistAlbums|TestClaimArtistAlbum|TestFailArtistAlbum|TestMarkArtistAlbums'
```
Expected: PASS for both.

- [ ] **Step 8: Vet, staticcheck, NUL-check and commit**

```bash
export PATH="$PATH:$(go env GOPATH)/bin"
go vet ./internal/store/catalog/ ./test/integration/
staticcheck ./internal/store/catalog/
perl -0777 -ne 'print "NULs: ", tr/\0//, "\n"' migrations/00014_artist_albums.sql
perl -0777 -ne 'print "NULs: ", tr/\0//, "\n"' internal/store/catalog/artistalbums.go
git add migrations/00014_artist_albums.sql internal/store/catalog/artistalbums.go \
        internal/store/catalog/catalog_test.go test/integration/artistalbums_test.go
git commit -m "$(cat <<'EOF'
Store: an artist's own releases, and the outcome of reading them

Nothing on disk counts an artist's releases, so Encore cannot say "you have
heard 4 of their 11 albums". artist_albums is that list; artist_album_fetches
holds the outcome of the last attempt to read it, because no rows in the first
table is ambiguous between "not asked yet", "asked and failed" and a claim no
listing can make.

album_id carries no foreign key to albums on purpose: most of a discography is
records nobody played, which are not in that table at all. Every album_group is
stored rather than only 'album', so the page can name what it set aside instead
of quietly dropping 340 singles out of a denominator.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 2: Reading a discography from Spotify

**Files:**
- Create: `internal/spotify/artistalbums.go`
- Create: `internal/spotify/artistalbums_test.go`

**Interfaces:**
- Consumes: `c.getAsApp` (`internal/spotify/catalog.go:105`), `validID` (`catalog.go:156`), `maxLibraryPageSize = 50` (`library.go:15`), `ErrTruncated` (`library.go:35`), `ParseReleaseDate(value, precision string) (*time.Time, string)` (`models.go:255`).
- Produces:
  - `type spotify.ArtistAlbum struct { ID, Name, Group string; ReleaseDate *time.Time; ReleasePrecision string }`
  - `func (c *Client) ArtistAlbums(ctx context.Context, artistID string, maxPages int) ([]ArtistAlbum, error)` — returns `(nil, nil)` for a 200 with no usable items, and `(partial, ErrTruncated)` when the page budget runs out.

- [ ] **Step 1: Write the failing client tests**

Create `internal/spotify/artistalbums_test.go`. Mirror the harness `internal/spotify/albumtracks_test.go` already uses — read it first and reuse its stub-server helper verbatim rather than inventing a second one.

```go
package spotify

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// artistAlbumServer answers /v1/artists/{id}/albums with the pages given, in
// order, and records the query each request carried.
func artistAlbumServer(t *testing.T, pages []map[string]any) (*Client, *[]string) {
	t.Helper()
	queries := make([]string, 0, len(pages))
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/api/token") {
			w.Header().Set("content-type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token": "app-token", "token_type": "Bearer", "expires_in": 3600,
			})
			return
		}
		queries = append(queries, r.URL.RawQuery)
		body := map[string]any{"items": []any{}, "next": nil}
		if n < len(pages) {
			body = pages[n]
		}
		n++
		w.Header().Set("content-type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(srv.Close)
	return testClient(t, srv.URL), &queries
}

func release(id, name, group, date, precision string) map[string]any {
	return map[string]any{
		"id": id, "name": name, "album_group": group,
		"release_date": date, "release_date_precision": precision,
	}
}

// TestArtistAlbumsFollowsEveryPage walks two pages and stops on the one with no
// next, which is what a discography longer than fifty releases needs.
func TestArtistAlbumsFollowsEveryPage(t *testing.T) {
	c, queries := artistAlbumServer(t, []map[string]any{
		{"items": []any{release("a1", "First", "album", "2016-05-20", "day")}, "next": "http://next"},
		{"items": []any{release("a2", "A Single", "single", "2018", "year")}, "next": nil},
	})

	got, err := c.ArtistAlbums(context.Background(), "1BBBBBBBBBBBBBBBBBBBBB", 20)
	if err != nil {
		t.Fatalf("ArtistAlbums: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d releases, want 2: the walk stopped before the last page", len(got))
	}
	if got[1].Group != "single" {
		t.Fatalf("group = %q, want \"single\": album_group did not survive decoding", got[1].Group)
	}
	if got[1].ReleasePrecision != "year" || got[1].ReleaseDate == nil || got[1].ReleaseDate.Year() != 2018 {
		t.Fatalf("release = %+v, want a year-precision 2018", got[1])
	}
	if len(*queries) != 2 {
		t.Fatalf("made %d requests, want 2", len(*queries))
	}
	if !strings.Contains((*queries)[1], "offset=50") {
		t.Fatalf("second request query = %q, want offset=50", (*queries)[1])
	}
	// include_groups is deliberately absent: every group is fetched so the page
	// can name what it set aside.
	for i, q := range *queries {
		if strings.Contains(q, "include_groups") {
			t.Fatalf("request %d sent include_groups (%q); every group must be fetched, or the "+
				"page cannot say what it excluded from the count", i, q)
		}
	}
}

// TestArtistAlbumsReportsTruncation pins that a spent page budget is an error
// carrying real data. The caller must treat it as a failure: ReplaceArtistAlbums
// is delete-absent, so writing this prefix would delete the tail of a
// discography that was correct.
func TestArtistAlbumsReportsTruncation(t *testing.T) {
	full := make([]any, 0, 50)
	for i := range 50 {
		full = append(full, release("a"+string(rune('a'+i)), "R", "album", "2016", "year"))
	}
	c, _ := artistAlbumServer(t, []map[string]any{
		{"items": full, "next": "http://next"},
		{"items": full, "next": "http://next"},
	})

	got, err := c.ArtistAlbums(context.Background(), "1BBBBBBBBBBBBBBBBBBBBB", 2)
	if !errors.Is(err, ErrTruncated) {
		t.Fatalf("error = %v, want ErrTruncated", err)
	}
	if len(got) == 0 {
		t.Fatal("ErrTruncated came back with no data; the partial listing is what makes it dangerous, " +
			"and a caller test that never sees it cannot prove the caller drops it")
	}
}

// TestArtistAlbumsReturnsNilNilOnAnEmptyResponse pins the exact shape the
// service depends on. A 200 with no items is not an error at the transport
// level and must not be: it is the service's job to record it as a failure, and
// it can only do that if this returns no items and no error.
func TestArtistAlbumsReturnsNilNilOnAnEmptyResponse(t *testing.T) {
	c, _ := artistAlbumServer(t, []map[string]any{{"items": []any{}, "next": nil}})

	got, err := c.ArtistAlbums(context.Background(), "1BBBBBBBBBBBBBBBBBBBBB", 20)
	if err != nil {
		t.Fatalf("error = %v, want nil: an empty page is not a transport error", err)
	}
	if got != nil {
		t.Fatalf("got %v, want nil so the caller can distinguish it from a stored listing", got)
	}
}

// TestArtistAlbumsSkipsItemsWithNoID keeps a keyless row out of a table whose
// primary key includes it, and pins that an all-keyless page collapses to the
// (nil, nil) shape above rather than to a listing of ghosts.
func TestArtistAlbumsSkipsItemsWithNoID(t *testing.T) {
	c, _ := artistAlbumServer(t, []map[string]any{
		{"items": []any{
			release("", "Ghost", "album", "2016", "year"),
			release("a1", "Real", "album", "2016", "year"),
		}, "next": nil},
	})

	got, err := c.ArtistAlbums(context.Background(), "1BBBBBBBBBBBBBBBBBBBBB", 20)
	if err != nil {
		t.Fatalf("ArtistAlbums: %v", err)
	}
	if len(got) != 1 || got[0].ID != "a1" {
		t.Fatalf("got %+v, want only the release that has an id", got)
	}
}

// TestArtistAlbumsRefusesANonSpotifyID stops a malformed id reaching the
// request path. Ids Encore minted locally from an export's names
// (domain.LocalArtistID) land here too, and there is no artist on Spotify for
// them to ask about.
func TestArtistAlbumsRefusesANonSpotifyID(t *testing.T) {
	c, queries := artistAlbumServer(t, nil)

	for _, id := range []string{"", "  ", "local:artist:someone", "not/a/spotify/id"} {
		if _, err := c.ArtistAlbums(context.Background(), id, 20); err == nil {
			t.Fatalf("ArtistAlbums(%q) succeeded; want it refused before the request went out", id)
		}
	}
	if len(*queries) != 0 {
		t.Fatalf("made %d requests for malformed ids, want 0", len(*queries))
	}
}
```

**Fails when:**
- `…FollowsEveryPage` — the walk stops after page one, `offset` is not advanced by 50, `album_group` is not decoded, `ParseReleaseDate` is not applied, or somebody adds `include_groups=album` to save requests (which would silently make the excluded counts unwriteable).
- `…ReportsTruncation` — the budget path returns `nil` instead of the partial, or returns no error.
- `…ReturnsNilNilOnAnEmptyResponse` — the empty case starts returning an error, or an empty non-nil slice, either of which changes what the service must check.
- `…SkipsItemsWithNoID` — the blank-id filter is dropped.
- `…RefusesANonSpotifyID` — the `validID` guard is dropped, or moved after the path is built.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -count=1 ./internal/spotify/ -run TestArtistAlbums`
Expected: FAIL — `c.ArtistAlbums undefined`.

> If `testClient(t, url)` is not the helper name `internal/spotify/albumtracks_test.go` uses, adopt whatever that file uses instead — do not add a second constructor.

- [ ] **Step 3: Write the client method**

Create `internal/spotify/artistalbums.go`:

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

// defaultArtistAlbumPages bounds one artist's discography when the caller does
// not say. Fifty a page, so twenty pages is a thousand releases.
//
// Deliberately the same page count as defaultAlbumTrackPages but a very
// different amount of headroom: no released album approaches a thousand tracks,
// while a long-running artist with every appears_on credit counted can reach a
// few hundred releases. A thousand still clears that comfortably, and a bound
// that is too tight is not harmless — a truncated walk is recorded as a failure
// and never stored, so the artist's panel would read "could not be read" for
// ever rather than showing a slightly short list.
const defaultArtistAlbumPages = 20

// ArtistAlbum is one release Spotify lists for an artist.
//
// Group is Spotify's album_group, which says what the *artist* is to the record
// — 'album', 'single', 'compilation' or 'appears_on'. It is not album_type,
// which says what the record is: a record this artist merely guests on has
// album_type "album" and album_group "appears_on", and completion counts the
// second.
type ArtistAlbum struct {
	ID               string
	Name             string
	Group            string
	ReleaseDate      *time.Time
	ReleasePrecision string
}

// artistAlbumPage is one response from /v1/artists/{id}/albums.
type artistAlbumPage struct {
	Items []struct {
		ID                   string `json:"id"`
		Name                 string `json:"name"`
		AlbumGroup           string `json:"album_group"`
		ReleaseDate          string `json:"release_date"`
		ReleaseDatePrecision string `json:"release_date_precision"`
	} `json:"items"`
	Next string `json:"next"`
}

// ArtistAlbums reads every release Spotify lists for one artist.
//
// Offset paginated at fifty a page, the same shape as SavedTracks and
// AlbumTracks, and it reports truncation the same way: a page budget spent
// while Spotify still had a next page returns the pages already read
// *alongside* ErrTruncated. The partial listing is real data, but it is not the
// whole discography, and a caller that replaces a stored set from it deletes
// the tail. See ErrTruncated's own comment for why that has to be the caller's
// problem rather than silently handled here.
//
// **No include_groups parameter is sent**, so every group comes back. Asking
// only for 'album' would cut a prolific artist from seven requests to one, and
// it is still wrong: completion counts albums and excludes singles,
// compilations and appearances, so the page has to be able to say what it
// excluded. "You have heard 4 of 11 albums" with 340 unmentioned singles is an
// overclaim by omission, and there is nothing on disk to write the missing
// sentence from if this never fetched them.
//
// It reads with the application token rather than a listener's: an artist's
// discography is public catalogue data and needs no user scope, so one instance
// makes one walk for an artist however many of its users open them.
//
// No market parameter is sent, so the ids are Spotify's canonical ones rather
// than relinked to a market — the same choice AlbumTracks makes, and the same
// known limitation: a listener whose play was recorded under a relinked album
// id will see that album listed as never played.
func (c *Client) ArtistAlbums(ctx context.Context, artistID string, maxPages int) ([]ArtistAlbum, error) {
	id := strings.TrimSpace(artistID)
	if !validID(id) {
		// The id becomes part of the request path rather than a query parameter,
		// so a malformed one must be refused here rather than sent. Ids Encore
		// minted locally from an export's names (domain.LocalArtistID) land here
		// too, and there is no artist on Spotify for them to ask about.
		return nil, fmt.Errorf("spotify: artist albums: %q is not a spotify artist id", artistID)
	}

	path := "/v1/artists/" + id + "/albums"
	var out []ArtistAlbum
	for page := range artistAlbumBudget(maxPages) {
		q := url.Values{}
		q.Set("limit", strconv.Itoa(maxLibraryPageSize))
		q.Set("offset", strconv.Itoa(page*maxLibraryPageSize))

		var p artistAlbumPage
		if err := c.getAsApp(ctx, path, "get artist albums", q, &p); err != nil {
			return nil, fmt.Errorf("spotify: artist albums: %w", err)
		}
		for _, item := range p.Items {
			// A null or empty id is a release Spotify will not serve. It has no id
			// to compare against a listen, so it is not something this listing can
			// say anything true about.
			if item.ID == "" {
				continue
			}
			date, precision := ParseReleaseDate(item.ReleaseDate, item.ReleaseDatePrecision)
			out = append(out, ArtistAlbum{
				ID:               item.ID,
				Name:             item.Name,
				Group:            item.AlbumGroup,
				ReleaseDate:      date,
				ReleasePrecision: precision,
			})
		}
		if len(p.Items) == 0 || strings.TrimSpace(p.Next) == "" {
			return out, nil
		}
	}
	// Every page read was full and still pointed at a next one: the budget ran
	// out before the discography did, so out is a prefix, not the whole thing.
	return out, fmt.Errorf("spotify: artist albums: %w", ErrTruncated)
}

// artistAlbumBudget clamps a caller's page limit, mirroring pageBudget.
func artistAlbumBudget(maxPages int) int {
	if maxPages <= 0 {
		return defaultArtistAlbumPages
	}
	return maxPages
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test -count=1 ./internal/spotify/ -run TestArtistAlbums -v`
Expected: PASS, five tests.

- [ ] **Step 5: Vet, staticcheck, NUL-check and commit**

```bash
export PATH="$PATH:$(go env GOPATH)/bin"
go vet ./internal/spotify/ && staticcheck ./internal/spotify/
perl -0777 -ne 'print "NULs: ", tr/\0//, "\n"' internal/spotify/artistalbums.go
git add internal/spotify/artistalbums.go internal/spotify/artistalbums_test.go
git commit -m "$(cat <<'EOF'
Spotify: read what an artist has released

Discography coverage needs a count of an artist's releases, and no stored field
has one. This walks /v1/artists/{id}/albums with the application token, since a
discography is public catalogue data and needs no user scope.

It deliberately sends no include_groups. Asking only for albums would cut a
prolific artist from seven requests to one, but completion excludes singles,
compilations and appearances, and a page that cannot name what it excluded says
"4 of 11 albums" over 340 unmentioned releases.

A spent page budget returns the pages already read alongside ErrTruncated, as
the library walks do, because the store's replace is delete-absent and a prefix
would delete the tail of a listing that was correct.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 3: The switch, the TTL, and the four places they must land

**Files:**
- Modify: `internal/config/config.go` (type near `AlbumTracks` at :189, parse near :415, startup log near :534)
- Modify: `internal/config/config_test.go`
- Modify: `docker-compose.yml:91-92` region (`x-encore-env` anchor)
- Modify: `.env.example:163-167` region
- Modify: `docs/configuration.md:95-96` region
- Regenerate: `docker-compose.portainer.yml` via `./scripts/gen-portainer-stack.sh`

**Interfaces:**
- Consumes: `p.boolean(name string, def bool) bool`, `p.duration(name string, def time.Duration) time.Duration` — the parser helpers already used at `config.go:415`.
- Produces: `config.ArtistAlbums{Enabled bool; TTL time.Duration}`, reachable as `cfg.ArtistAlbums`, read from `ENCORE_ARTIST_ALBUMS_ENABLED` (default `true`) and `ENCORE_ARTIST_ALBUMS_TTL` (default `168h`).

- [ ] **Step 1: Write the failing config test**

Append to `internal/config/config_test.go` (follow the file's existing pattern for setting environment variables — reuse its helper rather than calling `t.Setenv` directly if it has one):

```go
// TestArtistAlbumsDefaults pins that the discography cache works out of the box
// and that its TTL is a week rather than the album cache's month. The two are
// deliberately different: an album's track list is immutable after release,
// because a re-issue gets a new album id, while a discography grows — a record
// put out today should appear in "4 of 11" within a week.
func TestArtistAlbumsDefaults(t *testing.T) {
	c := loadWith(t, nil)

	if !c.ArtistAlbums.Enabled {
		t.Error("ArtistAlbums.Enabled = false by default; the feature would be off out of the box")
	}
	if c.ArtistAlbums.TTL != 168*time.Hour {
		t.Errorf("ArtistAlbums.TTL = %v, want 168h", c.ArtistAlbums.TTL)
	}
	// The two caches are separately switchable, and this is the whole reason
	// 2e-ii did not reuse ENCORE_ALBUM_TRACKS_ENABLED.
	if c.ArtistAlbums.TTL == c.AlbumTracks.TTL {
		t.Error("the two caches share a TTL; a discography grows and an album's track list does not, " +
			"and a single value cannot be right for both")
	}
}

// TestArtistAlbumsSwitchIsIndependentOfAlbumTracks is the test the config-gate
// decision rests on: an operator who turns one off must not thereby turn the
// other off, and — the failure that actually motivated a separate key — an
// operator who had ENCORE_ALBUM_TRACKS_ENABLED=false must not silently start
// making the new requests.
func TestArtistAlbumsSwitchIsIndependentOfAlbumTracks(t *testing.T) {
	c := loadWith(t, map[string]string{
		"ENCORE_ARTIST_ALBUMS_ENABLED": "false",
		"ENCORE_ARTIST_ALBUMS_TTL":     "48h",
	})

	if c.ArtistAlbums.Enabled {
		t.Error("ArtistAlbums.Enabled = true after ENCORE_ARTIST_ALBUMS_ENABLED=false")
	}
	if c.ArtistAlbums.TTL != 48*time.Hour {
		t.Errorf("ArtistAlbums.TTL = %v, want 48h", c.ArtistAlbums.TTL)
	}
	if !c.AlbumTracks.Enabled {
		t.Error("turning off discographies also turned off album track listings; the two keys " +
			"exist precisely so an operator can keep the cheap one")
	}

	back := loadWith(t, map[string]string{"ENCORE_ALBUM_TRACKS_ENABLED": "false"})
	if !back.ArtistAlbums.Enabled {
		t.Error("turning off album track listings also turned off discographies")
	}
}
```

> `loadWith(t, overrides)` is whatever `config_test.go` already uses to load a config with environment overrides — read the file and use its real name. Do not add a second loader.

**Fails when:** the parse block is not added; the default is `false`; the TTL default is copied from `AlbumTracks` (720h); or the two features are wired to one key, which is the specific mistake the decision above rejects.

- [ ] **Step 2: Run it to verify it fails**

Run: `go test -count=1 ./internal/config/ -run TestArtistAlbums`
Expected: FAIL — `c.ArtistAlbums undefined`.

- [ ] **Step 3: Add the config type, parse and startup log**

In `internal/config/config.go`, add the field beside `AlbumTracks` (near :38):

```go
	// ArtistAlbums governs the cache of artist discographies.
	ArtistAlbums ArtistAlbums
```

Add the type immediately after the `AlbumTracks` type ends (after :218):

```go
// ArtistAlbums governs the cache of artist discographies that lets the artist
// page say "you have heard 4 of this artist's 11 albums".
//
// The same shape as AlbumTracks above and for the same reasons: no worker and
// no interval, because a background sweep over every artist in a history is
// rejected explicitly — most are never opened, and enumerating them all would
// spend the instance's quota on questions nobody asked. A discography is read
// the first time somebody opens that artist's page and then kept, so the cost
// is one walk per artist *viewed*, per TTL.
//
// It has its own two keys rather than sharing AlbumTracks' for three reasons.
// Renaming a key operators may already have set fails *open*: an unset key
// falls back to its default, so somebody who had turned album track listings
// off would silently start making unattended requests again. The budgets differ
// by nearly an order of magnitude — roughly one request per album viewed
// against up to seven per artist, since a discography includes every single,
// compilation and appearance. And one key would mean turning off the expensive
// feature also turns off the cheap one.
type ArtistAlbums struct {
	// Enabled controls whether this instance ever asks Spotify what an artist
	// has released. On by default, so the feature works out of the box.
	//
	// It has a switch for the reason AlbumTracks.Enabled has one: this fires a
	// Spotify request as a side effect of *viewing a page* rather than as the
	// direct consequence of somebody clicking a thing, and unattended egress is
	// an operator's decision.
	//
	// Off means "do not fetch", not "forget what is on disk": a discography
	// already stored is still served, with the date it was read, and the artist
	// page says plainly that this instance does not fetch them rather than
	// reporting a Spotify failure that did not happen.
	Enabled bool
	// TTL is how long a stored discography is trusted before the next page view
	// refreshes it. Ignored entirely when Enabled is false — nothing refreshes,
	// so nothing expires.
	//
	// Seven days, deliberately shorter than AlbumTracks.TTL's thirty. That one
	// is long because an album's track list is effectively immutable after
	// release: Spotify mints a new album id for a deluxe edition rather than
	// changing the old one. A discography has no such property — it *grows*, and
	// a record released today is exactly the kind of thing somebody opening an
	// artist page wants counted. A month's lag on new releases would be visible;
	// a week's is not.
	TTL time.Duration
}
```

Add the parse immediately after the `c.AlbumTracks = AlbumTracks{...}` block (after :418):

```go
	c.ArtistAlbums = ArtistAlbums{
		Enabled: p.boolean("ENCORE_ARTIST_ALBUMS_ENABLED", true),
		TTL:     p.duration("ENCORE_ARTIST_ALBUMS_TTL", 7*24*time.Hour),
	}
```

Add to the startup log map, immediately after the `album_tracks_ttl` entry (:534):

```go
		// Same reasoning as the two lines above: "why is the artist page saying
		// discographies are turned off" is answerable from here or from nowhere.
		"artist_albums_enabled": c.ArtistAlbums.Enabled,
		"artist_albums_ttl":     c.ArtistAlbums.TTL.String(),
```

- [ ] **Step 4: Run the config test to verify it passes**

Run: `go test -count=1 ./internal/config/ -v -run TestArtistAlbums`
Expected: PASS.

- [ ] **Step 5: Forward the keys through Compose**

In `docker-compose.yml`, in the `x-encore-env` anchor immediately after the two `ENCORE_ALBUM_TRACKS_*` lines (:91-92):

```yaml
  ENCORE_ARTIST_ALBUMS_ENABLED: ${ENCORE_ARTIST_ALBUMS_ENABLED:-}
  ENCORE_ARTIST_ALBUMS_TTL: ${ENCORE_ARTIST_ALBUMS_TTL:-}
```

- [ ] **Step 6: Document them in `.env.example`**

In `.env.example`, immediately after `#ENCORE_ALBUM_TRACKS_TTL=720h` (:167):

```
# Whether the artist page asks Spotify what an artist has released, which is
# what lets it say "you have heard 4 of their 11 albums". Same trade as the
# album setting above and a separate switch on purpose: a discography walk
# includes every single, compilation and appearance, so it costs up to seven
# requests for a prolific artist against roughly one for an album. Reading it
# fires on a page view rather than on a click, so it is the operator's call.
# Discographies already cached are still shown when it is off, with the date
# they were read; only the fetching stops.
#ENCORE_ARTIST_ALBUMS_ENABLED=true
# Seven days rather than the album setting's thirty: an album's track list does
# not change after release, but a discography grows, and a record put out today
# should be counted within a week rather than within a month. Ignored when the
# above is false. Must be positive.
#ENCORE_ARTIST_ALBUMS_TTL=168h
```

- [ ] **Step 7: Document them in `docs/configuration.md`**

Add two rows immediately after the `ENCORE_ALBUM_TRACKS_TTL` row (:96):

```markdown
| `ENCORE_ARTIST_ALBUMS_ENABLED` | `true` | Whether this instance asks Spotify what an artist has released, which is what lets the artist page say "you have heard 4 of this artist's 11 albums". Like `ENCORE_ALBUM_TRACKS_ENABLED` above it fires as a side effect of *viewing* a page rather than of a click, so unattended egress stays the operator's decision — and it is a **separate** switch on purpose, because the two cost very different amounts and because renaming a key an operator may already have set would fail open. A walk costs `ceil(releases/50)` requests per artist *viewed* per TTL, counting every single, compilation and appearance: one request for most artists, up to seven for a prolific one, and nothing at all for artists nobody opens. Set to `false` and `encore-api` makes no discography request. **Turning it off does not hide discographies already cached** — those are still shown, with the date they were read; only fetching stops, and the artist page says so plainly rather than reporting a Spotify failure that did not happen. Like the album track listing above, **a rate-limit response to this request pauses Spotify access instance-wide** for the window Spotify asks for, which 409s "sync now" for every user on the instance until it lifts; with `ENCORE_ALBUM_TRACKS_ENABLED` this is now the second unattended request that can trigger that pause, so an operator with a tight quota should weigh both. |
| `ENCORE_ARTIST_ALBUMS_TTL` | `168h` (7 days) | How long a cached discography is trusted before the next view of that artist's page refreshes it. Deliberately shorter than `ENCORE_ALBUM_TRACKS_TTL`: a released album's track list does not change, but a discography *grows*, and a record released today should be counted within a week rather than within a month. A failed fetch is retried after fifteen minutes rather than after this interval. Ignored when `ENCORE_ARTIST_ALBUMS_ENABLED` is `false`: nothing refreshes, so nothing expires. Must be positive. |
```

- [ ] **Step 8: Regenerate the Portainer stack**

This is the step 2e-i's only Critical was for. It is not optional and `test/deploy` does not cover it.

```bash
./scripts/gen-portainer-stack.sh
git diff --stat docker-compose.portainer.yml
grep -c ENCORE_ARTIST_ALBUMS docker-compose.portainer.yml
```
Expected: `docker-compose.portainer.yml` shows changes, and the grep prints **8** (two variables × three services × plus the shared `x-encore-env` anchor block = the same multiplicity `ENCORE_ALBUM_TRACKS` has — confirm with `grep -c ENCORE_ALBUM_TRACKS docker-compose.portainer.yml` and require the two counts to be equal).

- [ ] **Step 9: Run the deploy guards**

```bash
go test -count=1 ./test/deploy/
```
Expected: PASS. `TestComposeForwardsEveryConfigurationVariable` fails if step 5 was skipped; `TestEnvExampleDocumentsWhatComposeForwards` fails if step 6 was skipped.

**Fails when:** either key is added to `config.go` without being added to `docker-compose.yml` (the first test), or without `.env.example` (the second). Neither covers `docker-compose.portainer.yml`, which is why step 8 has its own explicit grep.

- [ ] **Step 10: Commit**

```bash
export PATH="$PATH:$(go env GOPATH)/bin"
go vet ./internal/config/ && staticcheck ./internal/config/
git add internal/config/config.go internal/config/config_test.go docker-compose.yml \
        docker-compose.portainer.yml .env.example docs/configuration.md
git commit -m "$(cat <<'EOF'
Config: a switch and a TTL for artist discographies

Its own two keys rather than a reuse or a rename of the album-track pair. A
rename fails open: an operator who had turned album track listings off would
have their setting silently stop being read and start making the requests they
refused. The budgets differ by nearly an order of magnitude too — a discography
counts every single, compilation and appearance — so one switch cannot be the
right answer for both.

Seven days rather than thirty, because a discography grows where an album's
track list does not.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 4: Extract the shared fetch lifecycle

**This task adds no feature.** It moves working, merged code and must leave `internal/albumtracks`
behaving identically. Nothing in Task 5 may begin until this task's suite is green and committed on
its own — if the extraction breaks something, it has to be visible by itself.

**Files:**
- Create: `internal/lazyfetch/lazyfetch.go`
- Create: `internal/lazyfetch/lazyfetch_test.go`
- Create: `internal/lazyfetch/ordering_test.go`
- Modify: `internal/albumtracks/albumtracks.go` (delete the lifecycle; delegate)
- Modify: `internal/albumtracks/albumtracks_test.go` (**two tests only** — see step 6)

**Interfaces:**
- Consumes: `store.Querier`, `logging.Err`. Nothing else — `internal/lazyfetch` imports no repository, no Spotify client and no config.
- Produces, in `internal/lazyfetch`:
  - `type Outcome string`, `OutcomeReady`/`OutcomePending`/`OutcomeUnavailable`/`OutcomeDisabled` = `"ready"`/`"pending"`/`"unavailable"`/`"disabled"`
  - `StatusFetching`/`StatusOK`/`StatusFailed` = `"fetching"`/`"ok"`/`"failed"`
  - `type State struct { Status string; FetchedAt, AttemptedAt time.Time; Attempts int }`
  - `type Policy struct { Enabled bool; TTL, LeaseTTL, FailedRetryAfter, FetchTimeout, RecordTimeout time.Duration; Concurrency int }`
  - `type Leases interface { Claim(ctx, q, id string, now, leaseCutoff time.Time) (bool, error); Fail(ctx, q, id string, at time.Time, reason string) error }`
  - `type Fill func(ctx context.Context, id string) error`
  - `type Deps struct { Leases Leases; Fill Fill; DB func() store.Querier; Subject string; Logger *slog.Logger; Now func() time.Time }`
  - `func New(p Policy, deps Deps) (*Gate, error)`
  - `func (g *Gate) Resolve(ctx context.Context, q store.Querier, id string, st State, stored bool) Outcome`
  - `func (g *Gate) Now() time.Time`
  - `func (g *Gate) Close()`
- Produces, unchanged in `internal/albumtracks`: `New(cfg config.AlbumTracks, deps Deps) (*Service, error)`, `(*Service).Listing`, `(*Service).Close`, `State`, `StateReady`/`StatePending`/`StateUnavailable`/`StateDisabled`, `Track`, `Listing`, `Fetcher`, `Store`, `Writer`, `StoreWriter`, `Deps`. **Every one of these keeps its exact name and signature**, so `internal/httpapi` and `cmd/encore-api` are untouched by this task.

### The seam, stated before any code

`Fill` is the whole of it. The Gate decides *whether and when* to run one; the caller's `Fill` is
*what running one means*. Pagination, `album_group`, empty-listing detection, `ErrTruncated` and the
one-transaction commit are all inside `Fill` and the Gate has no opinion about any of them — its only
interest is whether `Fill` returned an error, which it records as a failed attempt.

The line is drawn there because it is the line the two callers actually differ across. `albumtracks`'
`Fill` treats an empty response as a failure; `artistalbums`' treats an empty *counted* set as a
success and only an empty response as a failure. If that rule lived in the Gate, the abstraction
would be wrong on its second caller — which is the standard test for whether an abstraction is
premature, and this one passes it.

- [ ] **Step 1: Write the failing tests for the Gate's policy**

Create `internal/lazyfetch/lazyfetch_test.go`. These are new tests of moved code, so they are written
first and must fail before the package exists.

```go
package lazyfetch

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/RequiDev/encore/internal/store"
)

// fakeLeases stands in for the two lease statements. It is deliberately not a
// database: this package's decisions are about *when* to fetch, which no amount
// of SQL will tell you.
type fakeLeases struct {
	mu         sync.Mutex
	claims     int
	fails      int
	claimOK    bool
	claimErr   error
	claimPanic bool
	lastReason string
	failCtxErr error
}

func (f *fakeLeases) Claim(_ context.Context, _ store.Querier, _ string, _, _ time.Time) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.claims++
	if f.claimPanic {
		panic("a nil Querier reaching QueryRow does exactly this")
	}
	if f.claimErr != nil {
		return false, f.claimErr
	}
	return f.claimOK, nil
}

func (f *fakeLeases) Fail(ctx context.Context, _ store.Querier, _ string, _ time.Time, reason string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fails++
	f.lastReason = reason
	f.failCtxErr = ctx.Err()
	return nil
}

func (f *fakeLeases) counts() (claims, fails int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.claims, f.fails
}

func (f *fakeLeases) reason() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastReason
}

func (f *fakeLeases) recordContextErr() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.failCtxErr
}

// fill records what the Gate asked it to do and can be held open or made to
// fail, which is every shape a caller's Fill has from the Gate's point of view.
type fill struct {
	mu     sync.Mutex
	calls  int
	err    error
	block  chan struct{}
	sawCtx error
}

func (f *fill) run(ctx context.Context, _ string) error {
	f.mu.Lock()
	f.calls++
	block, err := f.block, f.err
	f.mu.Unlock()
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			f.mu.Lock()
			f.sawCtx = ctx.Err()
			f.mu.Unlock()
			return ctx.Err()
		}
	}
	return err
}

func (f *fill) called() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fill) ctxErr() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sawCtx
}

func discard() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

func policy(enabled bool) Policy {
	return Policy{
		Enabled:          enabled,
		TTL:              30 * 24 * time.Hour,
		LeaseTTL:         2 * time.Minute,
		FailedRetryAfter: 15 * time.Minute,
		FetchTimeout:     90 * time.Second,
		RecordTimeout:    5 * time.Second,
		Concurrency:      4,
	}
}

func newGate(t *testing.T, p Policy, l *fakeLeases, f *fill, now time.Time) *Gate {
	t.Helper()
	g, err := New(p, Deps{
		Leases:  l,
		Fill:    f.run,
		DB:      func() store.Querier { return nil },
		Subject: "thing",
		Logger:  discard(),
		Now:     func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(g.Close)
	return g
}

var at = time.Date(2026, time.July, 31, 12, 0, 0, 0, time.UTC)

// TestResolveStartsTheFirstFillAndReportsPending: nothing attempted is the state
// every entity starts in, and it is the caller's cue to fill.
func TestResolveStartsTheFirstFillAndReportsPending(t *testing.T) {
	l := &fakeLeases{claimOK: true}
	f := &fill{block: make(chan struct{})}
	g := newGate(t, policy(true), l, f, at)

	got := g.Resolve(context.Background(), nil, "id-1", State{}, false)
	if got != OutcomePending {
		t.Fatalf("outcome = %q, want %q", got, OutcomePending)
	}
	close(f.block)
	g.Close()
	if f.called() != 1 {
		t.Fatalf("fill ran %d times, want 1", f.called())
	}
}

// TestResolveDoesNotWaitForTheFill is the load-bearing property: the page
// request answers while the fill is still running.
func TestResolveDoesNotWaitForTheFill(t *testing.T) {
	l := &fakeLeases{claimOK: true}
	f := &fill{block: make(chan struct{})}
	g := newGate(t, policy(true), l, f, at)

	done := make(chan struct{})
	go func() {
		defer close(done)
		g.Resolve(context.Background(), nil, "id-1", State{}, false)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Resolve did not return while the fill was in flight; the page waits on a third party")
	}
	close(f.block)
}

// TestDuePolicy pins all four arms of the schedule at once, including their
// boundaries. Each row varies exactly one input from a row that decides the
// other way, so no row passes for a reason another row already covers.
func TestDuePolicy(t *testing.T) {
	p := policy(true)
	g := newGate(t, p, &fakeLeases{}, &fill{}, at)

	cases := []struct {
		name string
		st   State
		want bool
	}{
		{"never attempted", State{}, true},
		{"ok, one day old", State{Status: StatusOK, FetchedAt: at.Add(-24 * time.Hour)}, false},
		{"ok, exactly at the TTL", State{Status: StatusOK, FetchedAt: at.Add(-p.TTL)}, true},
		{"ok, one second short of the TTL", State{Status: StatusOK, FetchedAt: at.Add(-p.TTL + time.Second)}, false},
		{"failed, inside the backoff", State{Status: StatusFailed, AttemptedAt: at.Add(-time.Minute)}, false},
		{"failed, exactly at the backoff", State{Status: StatusFailed, AttemptedAt: at.Add(-p.FailedRetryAfter)}, true},
		{"fetching, live lease", State{Status: StatusFetching, AttemptedAt: at.Add(-time.Second)}, false},
		{"fetching, exactly at the lease", State{Status: StatusFetching, AttemptedAt: at.Add(-p.LeaseTTL)}, true},
		{"an unknown status is never due", State{Status: "wat"}, false},
	}
	for _, c := range cases {
		if got := g.due(c.st, at); got != c.want {
			t.Errorf("due(%s) = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestEnabledIsCheckedBeforeDue pins an ordering, not a value. A switched-off
// instance must not resume making requests the moment its cache expires, and
// checking `enabled` first is also what stops the claim — a *write* — from being
// attempted at all, which is what an operator asking for no unattended traffic
// actually asked for.
func TestEnabledIsCheckedBeforeDue(t *testing.T) {
	l := &fakeLeases{claimOK: true}
	f := &fill{}
	g := newGate(t, policy(false), l, f, at)

	// Long past the TTL: `due` would say yes, and it must never be asked.
	got := g.Resolve(context.Background(), nil, "id-1", State{
		Status: StatusOK, FetchedAt: at.Add(-400 * 24 * time.Hour),
	}, true)
	g.Close()

	if got != OutcomeReady {
		t.Fatalf("outcome = %q, want %q: turning off fetching does not hide what is on disk", got, OutcomeReady)
	}
	if claims, _ := l.counts(); claims != 0 {
		t.Errorf("a disabled gate claimed the lease %d times, want 0", claims)
	}
	if f.called() != 0 {
		t.Errorf("a disabled gate filled %d times, want 0", f.called())
	}
}

// TestDisabledWithNothingStoredIsDisabledNotUnavailable keeps the two facts
// apart at the source. "Spotify would not answer" and "nobody asked Spotify" are
// different, and a page that renders the first for the second blames a third
// party for a local decision.
func TestDisabledWithNothingStoredIsDisabledNotUnavailable(t *testing.T) {
	g := newGate(t, policy(false), &fakeLeases{}, &fill{}, at)

	got := g.Resolve(context.Background(), nil, "id-1", State{}, false)
	if got != OutcomeDisabled {
		t.Fatalf("outcome = %q, want %q", got, OutcomeDisabled)
	}
}

// TestARecordedFailureInsideItsBackoffIsUnavailable is the only path to
// unavailable, which is what lets a page treat it as a reason to stop polling.
func TestARecordedFailureInsideItsBackoffIsUnavailable(t *testing.T) {
	l := &fakeLeases{claimOK: true}
	f := &fill{}
	g := newGate(t, policy(true), l, f, at)

	got := g.Resolve(context.Background(), nil, "id-1", State{
		Status: StatusFailed, AttemptedAt: at.Add(-time.Minute),
	}, false)
	g.Close()

	if got != OutcomeUnavailable {
		t.Fatalf("outcome = %q, want %q", got, OutcomeUnavailable)
	}
	if f.called() != 0 {
		t.Fatalf("filled %d times inside the backoff, want 0", f.called())
	}
}

// TestALiveLeaseIsNotEvenClaimed stops a polling browser attempting a write on
// every tick. It is checked even on a disabled gate: another replica may have
// started a fill before the switch was flipped.
func TestALiveLeaseIsNotEvenClaimed(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		l := &fakeLeases{claimOK: true}
		g := newGate(t, policy(enabled), l, &fill{}, at)

		got := g.Resolve(context.Background(), nil, "id-1", State{
			Status: StatusFetching, AttemptedAt: at.Add(-time.Second),
		}, false)
		g.Close()

		if got != OutcomePending {
			t.Errorf("enabled=%v: outcome = %q, want %q while another replica holds the lease",
				enabled, got, OutcomePending)
		}
		if claims, _ := l.counts(); claims != 0 {
			t.Errorf("enabled=%v: a live lease was claimed %d times, want 0", enabled, claims)
		}
	}
}

// TestALostClaimAndAClaimErrorAreBothPending. Neither records an outcome, so
// neither may report one: the next request re-enters the same branch. Reporting
// either as unavailable would blame a third party for a local condition and tell
// a page whose job is to stop polling on unavailable to give up on a fill that
// was still coming.
func TestALostClaimAndAClaimErrorAreBothPending(t *testing.T) {
	for name, l := range map[string]*fakeLeases{
		"lost claim":  {claimOK: false},
		"claim error": {claimErr: errors.New("read-only transaction")},
	} {
		f := &fill{}
		g := newGate(t, policy(true), l, f, at)

		got := g.Resolve(context.Background(), nil, "id-1", State{}, false)
		g.Close()

		if got != OutcomePending {
			t.Errorf("%s: outcome = %q, want %q", name, got, OutcomePending)
		}
		if _, fails := l.counts(); fails != 0 {
			t.Errorf("%s: recorded %d failures, want 0", name, fails)
		}
		if f.called() != 0 {
			t.Errorf("%s: filled %d times, want 0", name, f.called())
		}
	}
}

// TestNoFreeSlotIsPendingAndClaimsNothing. Refusing is the point: queueing here
// would be queueing people behind a third party.
func TestNoFreeSlotIsPendingAndClaimsNothing(t *testing.T) {
	p := policy(true)
	p.Concurrency = 1
	l := &fakeLeases{claimOK: true}
	f := &fill{block: make(chan struct{})}
	g := newGate(t, p, l, f, at)

	if got := g.Resolve(context.Background(), nil, "id-1", State{}, false); got != OutcomePending {
		t.Fatalf("first outcome = %q, want %q", got, OutcomePending)
	}
	// The one slot is taken and its fill cannot finish.
	if got := g.Resolve(context.Background(), nil, "id-2", State{}, false); got != OutcomePending {
		t.Fatalf("second outcome = %q, want %q: local backpressure records no outcome, so it must "+
			"not report one", got, OutcomePending)
	}
	if claims, _ := l.counts(); claims != 1 {
		t.Fatalf("claimed %d leases with one slot, want 1: a lease taken with no slot to fill it "+
			"strands the entity for the whole LeaseTTL", claims)
	}
	close(f.block)
}

// TestAFailingFillIsRecordedAndTheErrorNeverReturned: a third-party outage is a
// state the page renders, not a 500 it shows.
func TestAFailingFillIsRecordedAndTheErrorNeverReturned(t *testing.T) {
	l := &fakeLeases{claimOK: true}
	f := &fill{err: errors.New("upstream: 503 service unavailable")}
	g := newGate(t, policy(true), l, f, at)

	if got := g.Resolve(context.Background(), nil, "id-1", State{}, false); got != OutcomePending {
		t.Fatalf("outcome = %q, want %q", got, OutcomePending)
	}
	g.Close()

	if _, fails := l.counts(); fails != 1 {
		t.Fatalf("recorded %d failures, want 1", fails)
	}
	// Handed over whole: the repository bounds it with store.Truncate, which cuts
	// on a rune boundary, and truncating again here would risk two ellipses.
	if got := l.reason(); got != "upstream: 503 service unavailable" {
		t.Fatalf("recorded reason = %q, want the cause whole", got)
	}
}

// TestTheFillOutlivesTheRequestContext. The request has already been answered,
// and cancelling when the browser navigated away would mean the answer never
// arrives however many times the page is opened.
func TestTheFillOutlivesTheRequestContext(t *testing.T) {
	l := &fakeLeases{claimOK: true}
	release := make(chan struct{})
	f := &fill{block: release}
	g := newGate(t, policy(true), l, f, at)

	ctx, cancel := context.WithCancel(context.Background())
	g.Resolve(ctx, nil, "id-1", State{}, false)
	cancel() // the browser navigated away
	close(release)
	g.Close()

	if _, fails := l.counts(); fails != 0 {
		t.Fatalf("recorded %d failures, want 0: the fill died with the request that started it", fails)
	}
	if f.ctxErr() != nil {
		t.Fatalf("the fill saw %v; it must not inherit the request's cancellation", f.ctxErr())
	}
}

// TestAPanicInTheClaimDoesNotLeakASlot is why the slot goes back on a defer.
// Released by explicit calls instead, a panic below the acquisition keeps the
// slot for the life of the process — a nil store.Querier reaching QueryRow is one
// line of a future caller away — and Concurrency of those stop this process
// filling anything again, silently, because recovery middleware keeps serving
// pages.
func TestAPanicInTheClaimDoesNotLeakASlot(t *testing.T) {
	p := policy(true)
	p.Concurrency = 1
	l := &fakeLeases{claimOK: true, claimPanic: true}
	f := &fill{}
	g := newGate(t, p, l, f, at)

	func() {
		defer func() { _ = recover() }()
		g.Resolve(context.Background(), nil, "id-1", State{}, false)
	}()

	l.mu.Lock()
	l.claimPanic = false
	l.mu.Unlock()

	if got := g.Resolve(context.Background(), nil, "id-2", State{}, false); got != OutcomePending {
		t.Fatalf("outcome after a panic = %q, want %q", got, OutcomePending)
	}
	g.Close()
	if f.called() != 1 {
		t.Fatalf("filled %d times after a panicking claim, want 1: the slot leaked and this process "+
			"will never fill anything again", f.called())
	}
}

// TestNewRejectsAnUnusablePolicy. A half-configured gate answers some entities
// and strands others.
func TestNewRejectsAnUnusablePolicy(t *testing.T) {
	ok := Deps{Leases: &fakeLeases{}, Fill: (&fill{}).run, DB: func() store.Querier { return nil }}

	for name, deps := range map[string]Deps{
		"no leases": {Fill: ok.Fill, DB: ok.DB},
		"no fill":   {Leases: ok.Leases, DB: ok.DB},
		"no db":     {Leases: ok.Leases, Fill: ok.Fill},
	} {
		if _, err := New(policy(true), deps); err == nil {
			t.Errorf("New with %s succeeded", name)
		}
	}

	bad := map[string]func(p *Policy){
		"zero TTL":           func(p *Policy) { p.TTL = 0 },
		"zero lease":         func(p *Policy) { p.LeaseTTL = 0 },
		"zero backoff":       func(p *Policy) { p.FailedRetryAfter = 0 },
		"zero fetch timeout": func(p *Policy) { p.FetchTimeout = 0 },
		"zero record":        func(p *Policy) { p.RecordTimeout = 0 },
		"zero concurrency":   func(p *Policy) { p.Concurrency = 0 },
	}
	for name, mutate := range bad {
		p := policy(true)
		mutate(&p)
		if _, err := New(p, ok); err == nil {
			t.Errorf("New with %s succeeded", name)
		}
	}
}

// TestNewRefusesALeaseShorterThanTheFetchTimeout is an invariant that was a
// comment in each caller and checked by nobody. A fill that can outlive its own
// lease lets a second replica reclaim the entity and start a duplicate walk
// against a quota the whole application shares — and the two then race to
// replace the same rows.
func TestNewRefusesALeaseShorterThanTheFetchTimeout(t *testing.T) {
	p := policy(true)
	p.LeaseTTL = time.Minute
	p.FetchTimeout = time.Minute // equal is not enough: the lease must outlast the fill
	ok := Deps{Leases: &fakeLeases{}, Fill: (&fill{}).run, DB: func() store.Querier { return nil }}

	if _, err := New(p, ok); err == nil {
		t.Fatal("New accepted a lease no longer than the fetch timeout; a live fill loses its own lease")
	}

	p.LeaseTTL = time.Minute + time.Nanosecond
	if _, err := New(p, ok); err != nil {
		t.Fatalf("New rejected a lease longer than the fetch timeout: %v", err)
	}
}
```

**Fails when:**
- `…StartsTheFirstFillAndReportsPending` — the never-attempted arm of `due` is dropped.
- `…DoesNotWaitForTheFill` — the fill is run in-request instead of detached.
- `TestDuePolicy` — any arm flips, or a boundary comparison changes from `>=` to `>`. Each row varies one input against a neighbour that decides the other way, so no row is carried by another.
- `TestEnabledIsCheckedBeforeDue` — `enabled` is checked after `due`, so a switched-off instance resumes fetching when its cache expires; or the claim is attempted before the switch is consulted.
- `…DisabledWithNothingStoredIsDisabledNotUnavailable` — `disabled` is folded into `unavailable`.
- `…RecordedFailureInsideItsBackoffIsUnavailable` — the failed arm stops backing off, or unavailable becomes reachable without a recorded failure.
- `…LiveLeaseIsNotEvenClaimed` — the pending pre-check is dropped and every poll tick attempts a write, or the check is skipped on a disabled gate.
- `…LostClaimAndAClaimErrorAreBothPending` — either decline path starts reporting `unavailable`.
- `…NoFreeSlotIsPendingAndClaimsNothing` — the slot is acquired after the claim rather than before, so a lease is taken that no slot can fill.
- `…FailingFillIsRecordedAndTheErrorNeverReturned` — `Fill`'s error escapes `Resolve`, or the reason is truncated twice.
- `…FillOutlivesTheRequestContext` — the fill is given the request's context instead of `g.base`.
- `…PanicInTheClaimDoesNotLeakASlot` — the slot release moves from a `defer` to explicit calls.
- `TestNewRejectsAnUnusablePolicy` — any validation arm is dropped.
- `…RefusesALeaseShorterThanTheFetchTimeout` — the new invariant is dropped, or written as `>=` where it must be `>`.

- [ ] **Step 2: Write the failing race-guard tests at the shared layer**

Append to `internal/lazyfetch/lazyfetch_test.go`. These are `TestCloseEndsAFetchInFlight`,
`TestNothingIsStartedAfterClose` and `TestCloseRefusesNewFetchesBeforeItWaits` from
`internal/albumtracks/albumtracks_test.go:909`, `:969` and `:1025`, mirrored at the layer that now
owns the code. **Read all three in the original before transcribing, including their comments** —
the comment block at `:1002-1024` is the record of the M1 race and is the reason two of these exist
at all. Carry the reasoning across; only the receiver and the fakes change.

```go
// TestCloseEndsAFillInFlight is the other side of detaching: a goroutine on a
// context nobody cancels is a leak, and a process that cannot shut down.
//
// Transcribe from internal/albumtracks/albumtracks_test.go:909, replacing the
// service with a Gate and the catalogue fake with fakeLeases. Keep the rescue
// Cleanup that closes `block`, registered after the gate so it runs before
// Cleanup's Close: if Close turns out not to cancel anything, the assertion
// reports that rather than hanging the whole package on a stuck Cleanup.
//
// Its three assertions, unchanged in substance:
//   - Close returns within 5s while a fill is in flight
//   - the fill's context was cancelled
//   - the failure was recorded on a context NOT in error — context.WithoutCancel
//     is what keeps that write alive through shutdown, and without it pgx
//     refuses the write, the entity stays 'fetching' from the claim, and after
//     the restart every viewer polls uselessly for the full LeaseTTL
//
// Fails when: Close stops cancelling g.base, stops waiting on g.wg, or record
// stops detaching from the cancelled context.

// TestNothingIsStartedAfterClose is the shutdown race, asserted on the one
// operation that must not run concurrently with Close.
//
// Transcribe from internal/albumtracks/albumtracks_test.go:969 **in full**,
// including the two direct pokes at the lifecycle — here they are ordinary
// package-private access rather than the reach-through they were in
// albumtracks:
//
//	if !g.track() { t.Fatal("track refused a fill before Close") }
//	g.wg.Done() // hand the registration straight back
//	g.Close()
//	if g.track() { g.wg.Done(); t.Error(...) }
//
// then the black-box half: Resolve after Close must claim no lease and run no
// fill.
//
// Fails when: track() stops consulting g.closing, so a handler still inside
// Resolve at shutdown spawns a goroutine nothing waits for — or raises the
// WaitGroup counter from zero with a waiter already parked, which panics "Add
// called concurrently with Wait" and takes the process down on its way out.

// TestCloseRefusesNewFetchesBeforeItWaits pins the *ordering* inside Close,
// which TestNothingIsStartedAfterClose does not.
//
// Transcribe from internal/albumtracks/albumtracks_test.go:1025 **in full,
// including the whole comment block at :1002-1024**, which is the only written
// record of why the assignment sits where it does. Mechanically: start a fill
// that cannot finish so Close is guaranteed to park in wg.Wait, run Close in a
// goroutine, take `<-g.base.Done()` as the signal that Close has begun, and
// assert g.track() refuses inside that window.
//
// Fails when: Close is written as cancel(); wg.Wait(); closing = true — which
// satisfies TestNothingIsStartedAfterClose perfectly while reintroducing the
// whole M1 race, because for as long as Close is parked in Wait, track still
// says yes.
//
// Does NOT fail when: the assignment moves to between the cancel and the Wait.
// That leaves Close correct and silently costs this test its detector —
// base.Done() would fire while closing is still unset, so the assertion would be
// racing rather than asserting. albumtracks_test.go:1022 records that variant
// passing 30 runs out of 30. TestCloseSetsClosingBeforeItCancels below is what
// catches it, and it is a source-order assertion precisely because no
// outcome-based test can.
```

- [ ] **Step 3: Write the failing source-order test**

Create `internal/lazyfetch/ordering_test.go`. This is the test that did not exist before the
extraction, and it is the one thing that catches a mutation which is not a defect but which blinds
every behavioural detector.

The idiom is this codebase's own: `TestReplaceAlbumTracksSQLIsOneStatement`
(`internal/store/catalog/catalog_test.go:86`) asserts the *shape of the source* rather than an
outcome, justified by "a test built on outcomes cannot tell the two apart". The justification here is
identical and is stronger, because it has been measured.

```go
package lazyfetch

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// TestCloseSetsClosingBeforeItCancels pins a statement order that no
// outcome-based test can observe.
//
// Three orderings of Close's three operations are worth distinguishing:
//
//	closing = true; cancel(); wg.Wait()   — correct, and the behavioural tests
//	                                        can detect a departure from it
//	cancel(); wg.Wait(); closing = true   — a defect: for as long as Close is
//	                                        parked in Wait, track still says
//	                                        yes, so a request already inside
//	                                        Resolve can raise the WaitGroup
//	                                        counter against a registered waiter.
//	                                        TestCloseRefusesNewFetchesBeforeItWaits
//	                                        catches this one.
//	cancel(); closing = true; wg.Wait()   — NOT a defect. The invariant Close
//	                                        needs (closing set before Wait)
//	                                        still holds. But it destroys the
//	                                        detector: that test reads
//	                                        base.Done() as the signal that Close
//	                                        has begun, which is sound only while
//	                                        the assignment comes first. Under
//	                                        this variant base.Done() fires with
//	                                        closing still unset and the
//	                                        assertion races rather than asserts.
//	                                        Measured at 30 passes out of 30 —
//	                                        see albumtracks_test.go:1022.
//
// So the third variant is invisible to every behavioural test in this package
// and leaves the second variant, which is a real defect, undetectable from then
// on. The only way to catch it is to assert the order in the source, which is
// what this does. It is not a style check: it is the guard on the guard.
func TestCloseSetsClosingBeforeItCancels(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "lazyfetch.go", nil, 0)
	if err != nil {
		t.Fatalf("parse lazyfetch.go: %v", err)
	}

	var body *ast.BlockStmt
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if ok && fn.Name.Name == "Close" && fn.Recv != nil {
			body = fn.Body
			break
		}
	}
	if body == nil {
		t.Fatal("no Close method found in lazyfetch.go; this test cannot fail and must be fixed, " +
			"not deleted")
	}

	const missing = -1
	closingAt, cancelAt, waitAt := missing, missing, missing
	ast.Inspect(body, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.AssignStmt:
			// g.closing = true
			for _, lhs := range node.Lhs {
				if sel, ok := lhs.(*ast.SelectorExpr); ok && sel.Sel.Name == "closing" && closingAt == missing {
					closingAt = fset.Position(node.Pos()).Offset
				}
			}
		case *ast.CallExpr:
			sel, ok := node.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if sel.Sel.Name == "cancel" && cancelAt == missing {
				cancelAt = fset.Position(node.Pos()).Offset
			}
			if sel.Sel.Name == "Wait" && waitAt == missing {
				waitAt = fset.Position(node.Pos()).Offset
			}
		}
		return true
	})

	for name, offset := range map[string]int{"closing = true": closingAt, "cancel()": cancelAt, "wg.Wait()": waitAt} {
		if offset == missing {
			t.Fatalf("Close no longer contains %s; this test is asserting nothing about a Close it "+
				"cannot recognise", name)
		}
	}

	if !(closingAt < cancelAt) {
		t.Errorf("Close cancels before it marks itself closing. That leaves Close correct, so every "+
			"behavioural test still passes — and it silently destroys "+
			"TestCloseRefusesNewFetchesBeforeItWaits, which reads base.Done() as the signal that "+
			"Close has begun and is sound only while the assignment comes first. From then on, "+
			"moving the assignment below wg.Wait — which IS a defect — goes undetected.")
	}
	if !(cancelAt < waitAt) {
		t.Errorf("Close waits before it cancels; a fill in flight is never told to stop, so Close " +
			"blocks until the fill's own timeout expires")
	}
	if !(closingAt < waitAt) {
		t.Errorf("Close marks itself closing only after wg.Wait returns. For as long as Close is " +
			"parked in Wait, track still says yes, so a request already inside Resolve can call " +
			"wg.Add against a registered waiter — the panic 22d0f2c/M1 fixed")
	}
}
```

**Fails when:** `closing = true` moves below `g.cancel()` (mutation B, the blinding one — this is the
*only* test that catches it), or below `g.wg.Wait()` (mutation A, the genuine defect — caught here
and by `TestCloseRefusesNewFetchesBeforeItWaits`), or `cancel()` moves below `Wait()`. It also fails
loudly rather than silently passing if `Close` is renamed or restructured beyond recognition, which
is the failure mode a source-shape test must not have.

- [ ] **Step 4: Run the new tests to verify they fail**

Run: `go test -count=1 ./internal/lazyfetch/`
Expected: FAIL — the package does not exist.

- [ ] **Step 5: Write the Gate**

Create `internal/lazyfetch/lazyfetch.go`:

```go
// Package lazyfetch is the machinery behind Encore's lazily filled upstream
// caches: the album page's track listing and the artist page's discography.
//
// Both answer the same shape of question. Something a page wants is held by a
// third party, is expensive to ask for, and must never be asked for on the
// request that needs it. So it is read the first time somebody opens the
// relevant page, kept for a TTL, and refreshed by a later view. A sweep over
// every entity in a history is rejected explicitly in both cases: most are never
// viewed, and enumerating them all would spend the instance's quota on questions
// nobody asked.
//
// What this package guarantees is that the page request never waits: Resolve
// answers from what the caller already read out of the database and, when a fill
// is due, hands the work to a goroutine on a context of its own.
//
// Two guards keep that from becoming a stampede, and they answer different
// questions. A bounded slot channel is this process asking "am I already busy?"
// A conditional write against the caller's fetch table is the whole deployment
// asking "is anybody busy?" — and only the second survives two browser tabs, two
// API replicas, or a page that polls.
//
// # What is here and what is not
//
// Everything in this package is about *whether and when* to fill. Nothing in it
// is about what filling means. Pagination, response decoding, what counts as an
// empty answer, truncation, filtering and the transaction that stores the result
// are all the caller's, behind the single Fill seam — and they genuinely differ:
// one caller treats an empty response as a failure and an empty *filtered* set
// as impossible, while the other treats an empty response as a failure and an
// empty filtered set as an ordinary success. Putting that rule here would make
// this package wrong for its second caller.
//
// No copy lives here either. Outcome names four situations; the words a page
// says about them belong to the page.
package lazyfetch

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/RequiDev/encore/internal/logging"
	"github.com/RequiDev/encore/internal/store"
)

// The three states of one entity's fill, as stored in the caller's fetch table.
//
// They are string constants rather than a type so a caller's own column
// constants can be compared to them without conversion; the callers'
// repositories declare the same three values against their own tables and a test
// pins the two sets equal.
const (
	// StatusFetching is a lease: somebody is filling this entity now.
	StatusFetching = "fetching"
	// StatusOK means the caller's table holds a complete answer.
	StatusOK = "ok"
	// StatusFailed means the last attempt did not produce one. Whatever the
	// caller stores is from an earlier, successful attempt.
	StatusFailed = "failed"
)

// Outcome is what a page can say. Four values, and the distinctions between them
// are the whole reason this package exists rather than a boolean.
type Outcome string

const (
	// OutcomeReady means an answer is stored and can be reasoned about. It may be
	// older than the TTL, in which case a refresh is already running behind it.
	OutcomeReady Outcome = "ready"
	// OutcomePending means nothing is stored yet and a fill is running, or is due
	// and nothing has recorded a reason it should not be.
	//
	// Everything that merely *delays* a fill reports this: a lease somebody else
	// holds, no free local slot, a claim that errored, a shutdown in progress.
	// None of them records an *outcome*, which is what makes "keep polling" the
	// right advice — but they do not all resolve the same way. Most leave the row
	// untouched, so the entity is still due and the very next view starts it. A
	// claim this process wins and then abandons at shutdown leaves the row
	// 'fetching' with a fresh attempted_at, so the next view waits out LeaseTTL
	// before anybody reclaims it. Still pending, just not immediately.
	//
	// Nothing here bounds how long that can go on. A claim that errors records
	// nothing, so the next request re-enters the same branch; a client polling
	// this must cap itself.
	OutcomePending Outcome = "pending"
	// OutcomeUnavailable means nothing is stored and the last attempt failed,
	// recently enough that no new one has been started.
	//
	// It is emphatically not "this entity has nothing", and it is deliberately
	// not "this process was too busy to ask" either: only a *recorded* failure
	// reaches here, which is what lets a page treat it as a reason to stop
	// polling and say so. Reporting local backpressure as unavailable would tell
	// somebody the upstream would not answer when it was never asked — the same
	// category error that keeps OutcomeDisabled separate.
	OutcomeUnavailable Outcome = "unavailable"
	// OutcomeDisabled means nothing is stored and this instance will not fetch
	// it, because its operator turned that off.
	//
	// Deliberately not folded into OutcomeUnavailable. "The upstream would not
	// answer" and "nobody asked the upstream" are different facts, and a page
	// that renders the first for the second blames a third party for a local
	// decision.
	OutcomeDisabled Outcome = "disabled"
)

// State is the caller's bookkeeping row, in the only terms this package needs.
//
// The zero value — Status "" and both instants zero — is an entity that has
// never been attempted, which is an ordinary state rather than an error: every
// entity is in it until somebody first opens its page.
type State struct {
	Status      string
	FetchedAt   time.Time
	AttemptedAt time.Time
	Attempts    int
}

// Policy is the timing, all of it supplied by the caller because the right
// values depend on what is being fetched: an album's track list is one request
// and effectively immutable after release, while an artist's discography is up
// to twenty requests and grows.
type Policy struct {
	// Enabled is the operator's switch. False means this instance never asks the
	// upstream anything — and note that it does not mean "forget what is on
	// disk": a stored answer is still reported ready.
	Enabled bool
	// TTL is how long a stored answer is trusted. Ignored entirely when Enabled
	// is false: nothing refreshes, so nothing expires.
	TTL time.Duration
	// LeaseTTL is how long a 'fetching' row holds other callers off. It must
	// exceed FetchTimeout, so a live fill never loses its own lease, and it
	// should be short enough that a process killed mid-fill does not strand the
	// entity for long.
	LeaseTTL time.Duration
	// FailedRetryAfter is how long a failed entity is left alone. Much shorter
	// than a TTL in both callers, and deliberately: failures here are timeouts
	// and rate limits, which clear in minutes, and making somebody wait out a
	// month-long TTL would turn one bad minute into a broken panel.
	FailedRetryAfter time.Duration
	// FetchTimeout bounds one entity's whole fill — every page, every retry and
	// every rate-limit wait inside it.
	FetchTimeout time.Duration
	// RecordTimeout bounds the write that records a failure, including during
	// shutdown.
	RecordTimeout time.Duration
	// Concurrency is how many fills this process runs at once. Small on purpose
	// in both callers: these start inside page requests and draw on a quota the
	// whole application shares.
	Concurrency int
}

// Leases is the caller's two lease statements.
//
// Only two, and neither of them is the one that stores the answer: this package
// never learns what a caller's rows look like. Claim must be a single statement
// whose RETURNING is empty for the loser — a read followed by a write is not a
// lease, and neither is catching a uniqueness violation in Go.
type Leases interface {
	// Claim takes the lease, reporting whether this caller won it. leaseCutoff is
	// now minus LeaseTTL; a row already 'fetching' may be reclaimed only if its
	// attempted_at is strictly older than that.
	Claim(ctx context.Context, q store.Querier, id string, now, leaseCutoff time.Time) (bool, error)
	// Fail records that the last attempt did not produce an answer. It must touch
	// neither the stored rows nor their fetched_at: a timeout today is no reason
	// to throw away an answer that was correct last month.
	Fail(ctx context.Context, q store.Querier, id string, at time.Time, reason string) error
}

// Fill does one entity's caller-specific work: read it from upstream and store
// it. Anything it returns is recorded as a failed attempt, with no exceptions —
// in particular a truncation error that arrives *with* usable-looking data is
// still an error, and its partial payload must never reach a delete-absent
// replace.
//
// It receives a context bounded by FetchTimeout and descended from the Gate's
// own, not from any request: the request that triggered this has already been
// answered.
type Fill func(ctx context.Context, id string) error

// Deps is everything the Gate needs that is not timing.
type Deps struct {
	Leases Leases
	Fill   Fill
	// DB hands over a Querier for the failure write, which happens outside any
	// request and therefore has no querier of its own to borrow.
	DB func() store.Querier
	// Subject names what is being filled, for log messages: "album", "artist".
	Subject string
	Logger  *slog.Logger
	// Now is the clock. Tests replace it; production leaves it nil.
	Now func() time.Time
}

// Gate decides whether and when to fill, and owns the fills it starts.
type Gate struct {
	leases  Leases
	fill    Fill
	db      func() store.Querier
	subject string
	log     *slog.Logger
	now     func() time.Time
	p       Policy
	slots   chan struct{}
	// base is the parent of every detached fill, so Close can end them all. It is
	// a context in a struct on purpose: these fills outlive the request that
	// started them, so there is no incoming context for them to inherit.
	base   context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	// mu guards closing, which is the only thing that may not race wg.Add against
	// wg.Wait. See track.
	mu      sync.Mutex
	closing bool
}

// New validates the policy and builds the Gate.
//
// The caller owns Close and must call it during shutdown, before closing the
// database pool: fills run detached from any request, so nothing else will ever
// wait for them, and a fill cancelled at shutdown still needs the pool to record
// that it failed.
func New(p Policy, deps Deps) (*Gate, error) {
	switch {
	case deps.Leases == nil:
		return nil, errors.New("lazyfetch: a lease repository is required")
	case deps.Fill == nil:
		return nil, errors.New("lazyfetch: a fill function is required")
	case deps.DB == nil:
		return nil, errors.New("lazyfetch: a database handle is required to record failures")
	case p.TTL <= 0:
		return nil, errors.New("lazyfetch: a positive TTL is required")
	case p.LeaseTTL <= 0:
		return nil, errors.New("lazyfetch: a positive lease TTL is required")
	case p.FailedRetryAfter <= 0:
		return nil, errors.New("lazyfetch: a positive failure backoff is required")
	case p.FetchTimeout <= 0:
		return nil, errors.New("lazyfetch: a positive fetch timeout is required")
	case p.RecordTimeout <= 0:
		return nil, errors.New("lazyfetch: a positive record timeout is required")
	case p.Concurrency <= 0:
		return nil, errors.New("lazyfetch: a positive concurrency is required")
	case p.LeaseTTL <= p.FetchTimeout:
		// Checked here because it was a comment in each caller and enforced by
		// nobody. A fill that can outlive its own lease lets a second replica
		// reclaim the entity and start a duplicate walk against a shared quota,
		// and the two then race to replace the same rows.
		return nil, fmt.Errorf("lazyfetch: the lease (%s) must outlast the fetch timeout (%s), "+
			"or a live fill loses its own lease", p.LeaseTTL, p.FetchTimeout)
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
	return &Gate{
		leases:  deps.Leases,
		fill:    deps.Fill,
		db:      deps.DB,
		subject: deps.Subject,
		log:     lg,
		now:     now,
		p:       p,
		slots:   make(chan struct{}, p.Concurrency),
		base:    base,
		cancel:  cancel,
	}, nil
}

// Now is the Gate's clock, so a caller timestamping its own writes uses the same
// one the schedule is computed against.
func (g *Gate) Now() time.Time { return g.now() }

// Resolve decides what a page can say about one entity, and starts a fill when
// one is due.
//
// stored is the caller's own answer to "do I already hold something worth
// showing". It is the caller's because only the caller knows what its rows mean:
// one caller has an answer whenever it has rows, and another can have a
// perfectly good answer with no countable rows at all.
//
// It never blocks on the upstream and it never fails because the upstream did: a
// third-party outage is a state the page renders, not a 500 it shows. That is
// why it returns an Outcome rather than an error.
func (g *Gate) Resolve(ctx context.Context, q store.Querier, id string, st State, stored bool) Outcome {
	now := g.now()
	// A live lease means somebody is filling this entity right now. Checking it
	// before deciding anything is what keeps a polling browser from attempting a
	// write on every tick. It is checked even when this instance has fetching
	// turned off: another replica may have started one before the switch was
	// flipped, and reporting that accurately costs nothing.
	pending := st.Status == StatusFetching && now.Sub(st.AttemptedAt) < g.p.LeaseTTL

	// p.Enabled is checked *before* due, and that order is load-bearing: a
	// switched-off instance must not resume making requests the moment its cache
	// expires. Guarding here rather than inside start also means the claim — a
	// write — is never even attempted, which is what an operator asking for no
	// unattended traffic actually asked for.
	if !pending && g.p.Enabled && g.due(st, now) {
		g.start(ctx, q, id, now)
		// Pending regardless of what start managed to do, and that is not
		// optimism: not one of its decline paths records an outcome. Most — no
		// free slot, a lease somebody else holds, a claim that errored — leave the
		// row untouched, so the entity is still due and the next view starts it.
		// The one that does not is a claim won and then abandoned at shutdown,
		// which leaves the row 'fetching' and resolves when that lease expires.
		// Both are "not yet". Reporting either as unavailable would blame the
		// upstream for a local condition and, worse, would tell a page whose job
		// is to stop polling on unavailable to give up on an answer that was still
		// coming.
		pending = true
	}

	switch {
	case stored:
		// An answer read successfully once is worth showing while a refresh runs
		// behind it — and worth showing when no refresh is coming at all, because
		// turning off fetching is not the same as forgetting what is on disk.
		// Withholding it would replace a true answer that is old with no answer.
		// The caller reports when it was read, which is the only honesty this case
		// needs: a date claims nothing about freshness.
		return OutcomeReady
	case pending:
		return OutcomePending
	case !g.p.Enabled:
		// Nothing stored, and this instance will not go and find out. That is the
		// operator's decision, not an upstream failure, and the page says so in its
		// own words rather than reporting an outage that never happened.
		return OutcomeDisabled
	default:
		// Nothing stored, nothing running, and nothing due: the last attempt failed
		// and its backoff has not elapsed.
		return OutcomeUnavailable
	}
}

// due reports whether a fill should be started now.
//
// It deliberately knows nothing about p.Enabled. Whether this instance fetches
// at all is a different question from whether this answer is old, and Resolve
// asks them in that order.
func (g *Gate) due(st State, now time.Time) bool {
	switch st.Status {
	case "":
		// Never attempted: the lazy first fill.
		return true
	case StatusOK:
		return now.Sub(st.FetchedAt) >= g.p.TTL
	case StatusFailed:
		// Much sooner than the TTL, and deliberately so — see FailedRetryAfter.
		return now.Sub(st.AttemptedAt) >= g.p.FailedRetryAfter
	case StatusFetching:
		// The lease has expired: whatever process held it is gone.
		return now.Sub(st.AttemptedAt) >= g.p.LeaseTTL
	default:
		return false
	}
}

// start begins a detached fill if it can.
//
// It reports nothing, on purpose. Every way it can decline is a "not yet" rather
// than a "no", because none of them records an outcome: the ones that return
// before the claim leave the row untouched, so it is still due and the next view
// starts it, and the one that returns after a won claim leaves the row
// 'fetching', so it resolves when that lease expires.
func (g *Gate) start(ctx context.Context, q store.Querier, id string, now time.Time) {
	if g.base.Err() != nil {
		// Close has already been called. Claiming a lease this process is about to
		// abandon would strand the entity for the whole LeaseTTL for nothing. Not
		// the guard that makes Close safe — track does that, atomically — just the
		// one that keeps the common case from being wasteful.
		return
	}
	select {
	case g.slots <- struct{}{}:
	default:
		// Every slot is busy. Refusing is the point: queueing here would be
		// queueing people behind a third party. Taken *before* the claim, so a
		// lease is never won that no slot can fill.
		g.log.Debug("fill not started; all slots busy", "subject", g.subject, "id", id)
		return
	}
	// The slot goes back unless the fill goroutine takes ownership of it.
	//
	// A defer rather than a release() on each path: a panic anywhere below — a nil
	// Querier reaching q.QueryRow would do it — would otherwise keep the slot for
	// ever, and Concurrency of those stop this process filling anything for the
	// rest of its life. Silently, because recovery middleware keeps serving pages.
	started := false
	defer func() {
		if !started {
			<-g.slots
		}
	}()

	claimed, err := g.leases.Claim(ctx, q, id, now, now.Add(-g.p.LeaseTTL))
	if err != nil {
		g.log.Warn("could not claim a fill", "subject", g.subject, "id", id, logging.Err(err))
		return
	}
	if !claimed {
		// Somebody else holds the lease. A second fill would be wasted requests
		// against a quota the whole application shares — but a fill *is* running,
		// so the page is right to keep polling.
		return
	}

	if !g.track() {
		// Close began between the claim and here. Nothing is stranded that the
		// lease does not already cover: this is the same state a process killed
		// mid-fill leaves behind, and LeaseTTL exists for exactly that.
		g.log.Debug("fill abandoned; shutting down", "subject", g.subject, "id", id)
		return
	}
	// Nothing may go between these two statements. track has already done
	// wg.Add(1), and only the goroutine below calls the matching Done — so
	// anything inserted here that can panic leaves the WaitGroup one short and
	// Close waits on it for ever, which is a worse failure than the slot leak the
	// defer above exists to prevent. Whatever a future change needs to do, it
	// belongs before track or inside the goroutine.
	started = true
	go func() {
		defer g.wg.Done()
		defer func() { <-g.slots }()
		g.run(id)
	}()
}

// track registers a fill with the WaitGroup unless Close has begun, reporting
// whether the caller may now spawn it.
//
// The mutex is what makes Close correct rather than merely likely. Without it, a
// handler still inside Resolve when the process shuts down races Close two ways:
// commonly Wait sees zero, returns, and the goroutine started a moment later is
// one nothing waits for — which is precisely what Close promises not to allow;
// and rarely the Add raising the counter from zero with a waiter already
// registered panics with "WaitGroup misuse: Add called concurrently with Wait",
// taking the process down on its way out. Both are reachable because
// http.Server.Shutdown returns on timeout with handlers still running.
func (g *Gate) track() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closing {
		return false
	}
	g.wg.Add(1)
	return true
}

// run performs one fill on a context of its own and records whatever it returns.
//
// Deliberately not the request's context: that request has already been
// answered, and cancelling when the browser navigated away would mean the answer
// never arrives however many times the page is opened. FetchTimeout bounds it
// instead, and Close cancels it at shutdown.
func (g *Gate) run(id string) {
	ctx, cancel := context.WithTimeout(g.base, g.p.FetchTimeout)
	defer cancel()

	if err := g.fill(ctx, id); err != nil {
		g.record(id, err)
		return
	}
	g.log.Debug("filled", "subject", g.subject, "id", id)
}

// record writes a failure on its own context, so a fill cancelled at shutdown
// still leaves the entity out of 'fetching' rather than waiting out the lease.
//
// The reason is handed over whole. The caller's Fail bounds it with
// store.Truncate, which cuts on a rune boundary; truncating again here would
// only risk two ellipses and a message cut twice.
func (g *Gate) record(id string, cause error) {
	g.log.Warn("could not fill", "subject", g.subject, "id", id, logging.Err(cause))

	ctx, cancel := context.WithTimeout(context.WithoutCancel(g.base), g.p.RecordTimeout)
	defer cancel()
	if err := g.leases.Fail(ctx, g.db(), id, g.now(), cause.Error()); err != nil {
		// Nothing further can be done, and nothing is stuck: the lease expires on
		// its own, so the next page view after LeaseTTL tries again.
		g.log.Error("could not record a failure", "subject", g.subject, "id", id, logging.Err(err))
	}
}

// Close cancels every fill in flight and waits for them. It is safe to call more
// than once, and safe to call while requests are still in Resolve.
//
// Bounded by RecordTimeout per fill, because a cancelled fill still records its
// failure — which is why the composition root must call this *before* it closes
// the database pool. That write goes out on a context of its own precisely so
// shutdown does not lose it, and a closed pool would lose it anyway.
func (g *Gate) Close() {
	// Before the Wait, necessarily: that is what stops a wg.Add landing against a
	// registered waiter. Before the cancel too, which Close does not need but
	// TestCloseRefusesNewFetchesBeforeItWaits does — it takes base.Done() as the
	// signal that Close has begun, and that is only sound while this comes first.
	// Moving it below the cancel leaves Close correct and quietly turns that test
	// into one that cannot fail; TestCloseSetsClosingBeforeItCancels in
	// ordering_test.go exists because that is invisible to every other test here.
	g.mu.Lock()
	g.closing = true
	g.mu.Unlock()

	g.cancel()
	// Safe now: no wg.Add can follow, because track takes the same mutex and sees
	// closing.
	g.wg.Wait()
}
```

- [ ] **Step 6: Run the Gate's own tests**

```bash
go test -count=1 ./internal/lazyfetch/ -v
```
Expected: PASS, every test including the three race guards and the source-order test.

- [ ] **Step 7: Move `internal/albumtracks` onto the Gate**

Rewrite `internal/albumtracks/albumtracks.go`. **Delete** `State`'s four constant declarations and
their doc blocks, `due`, `start`, `track`, `fetch`, `record`, and the `slots`/`base`/`cancel`/`wg`/
`mu`/`closing` fields — every one of them now lives in `lazyfetch`. **Keep** `maxPages`,
`errEmptyListing`, `Track`, `Listing`, `Fetcher`, `Store`, `Writer`, `StoreWriter`, `Deps`, and
`New`'s validation and its disabled-startup log verbatim. The package comment keeps its first two
paragraphs and its "two guards" paragraph is replaced by a sentence pointing at `internal/lazyfetch`.

The five timing constants stay in this package — they are this cache's numbers, not shared ones —
and are handed to the Gate as a `Policy`.

```go
// State is what the page can say about the listing.
//
// An alias of lazyfetch.Outcome rather than a second declaration: the four words
// are an API contract two endpoints share, and one definition is what stops them
// forking. The names here are unchanged, so internal/httpapi is untouched.
type State = lazyfetch.Outcome

const (
	StateReady       = lazyfetch.OutcomeReady
	StatePending     = lazyfetch.OutcomePending
	StateUnavailable = lazyfetch.OutcomeUnavailable
	StateDisabled    = lazyfetch.OutcomeDisabled
)

// Service fills and serves album track listings.
type Service struct {
	cat  Store
	sp   Fetcher
	w    Writer
	log  *slog.Logger
	now  func() time.Time
	gate *lazyfetch.Gate
}

// leases adapts the catalogue's two lease statements to lazyfetch.Leases. It is
// the whole of what the Gate knows about album_track_fetches.
type leases struct{ cat Store }

func (l leases) Claim(
	ctx context.Context, q store.Querier, albumID string, now, leaseCutoff time.Time,
) (bool, error) {
	return l.cat.ClaimAlbumTrackFetch(ctx, q, albumID, now, leaseCutoff)
}

func (l leases) Fail(
	ctx context.Context, q store.Querier, albumID string, at time.Time, reason string,
) error {
	return l.cat.FailAlbumTrackFetch(ctx, q, albumID, at, reason)
}

// New validates the dependencies and builds the service.
//
// The caller owns Close and must call it during shutdown, before closing the
// database pool: fetches run detached from any request, so nothing else will
// ever wait for them, and a fetch cancelled at shutdown still needs the pool to
// record that it failed. Constructing this after the pool and deferring Close
// immediately puts both in the right order.
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
	s := &Service{
		cat: deps.Catalog,
		sp:  deps.Spotify,
		w:   deps.Writer,
		log: lg.With("component", "albumtracks"),
		now: now,
	}
	gate, err := lazyfetch.New(lazyfetch.Policy{
		Enabled:          cfg.Enabled,
		TTL:              cfg.TTL,
		LeaseTTL:         leaseTTL,
		FailedRetryAfter: failedRetryAfter,
		FetchTimeout:     fetchTimeout,
		RecordTimeout:    recordTimeout,
		Concurrency:      concurrency,
	}, lazyfetch.Deps{
		Leases:  leases{cat: deps.Catalog},
		Fill:    s.fill,
		DB:      deps.Writer.DB,
		Subject: "album",
		Logger:  s.log,
		Now:     now,
	})
	if err != nil {
		return nil, err
	}
	s.gate = gate
	if !cfg.Enabled {
		// Said once, at startup, rather than on every page view: an operator who
		// wonders why the album page reports this as turned off can find it here
		// and in the configuration line the process logs beside it.
		lg.Info("album track listings are turned off; this instance will not ask spotify " +
			"what is on an album. Listings already cached are still shown.")
	}
	return s, nil
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

	out := Listing{Tracks: tracks, FetchedAt: st.FetchedAt}
	// len(tracks) > 0, exactly as before the extraction. A successful read of an
	// album always stores at least one row, because a 200 carrying no tracks is
	// recorded as a failure, so this and st.FetchedAt agree for every state this
	// package can produce — but it is the predicate this service has always used
	// and the refactor changes no behaviour.
	out.State = s.gate.Resolve(ctx, q, albumID, lazyfetch.State{
		Status:      st.Status,
		FetchedAt:   st.FetchedAt,
		AttemptedAt: st.AttemptedAt,
		Attempts:    st.Attempts,
	}, len(tracks) > 0)
	return out, nil
}

// fill reads one album's listing and stores it. It is the Gate's Fill: whether
// and when it runs is the Gate's decision, and everything it does is this
// package's.
func (s *Service) fill(ctx context.Context, albumID string) error {
	items, err := s.sp.AlbumTracks(ctx, albumID, maxPages)
	if err != nil {
		// Every failure lands here, ErrTruncated included — and that one arrives
		// with a partial listing attached. The partial must never reach the write:
		// ReplaceAlbumTracks deletes whatever the incoming set does not contain, so
		// a prefix would delete the tail of a listing that was correct and then
		// mark the result authoritative. This project has hit that trap three
		// times; internal/spotify/library.go's ErrTruncated comment is the record
		// of it. There is no exception clause here on purpose.
		return err
	}
	if len(items) == 0 {
		// A 200 carrying no items, or one whose every item had a blank id, both of
		// which spotify.AlbumTracks reports as (nil, nil). Checked here rather than
		// left to ReplaceAlbumTracks' own refusal, because by then the intent would
		// already be "make this album's listing empty" and only an error would stop
		// it; returned as a failure, the stored listing and its date are untouched.
		return errEmptyListing
	}

	rows := make([]catalog.AlbumTrack, 0, len(items))
	for _, it := range items {
		rows = append(rows, catalog.AlbumTrack{
			TrackID: it.ID, Name: it.Name,
			DiscNumber: it.DiscNumber, TrackNumber: it.TrackNumber,
		})
	}
	at := s.now()
	return s.w.InTx(ctx, func(ctx context.Context, q store.Querier) error {
		if err := s.cat.ReplaceAlbumTracks(ctx, q, albumID, rows); err != nil {
			return err
		}
		// In the same transaction as the listing: the rows and the claim that they
		// are authoritative commit together, so a reader can never see a
		// half-replaced listing marked 'ok'.
		return s.cat.MarkAlbumTracksFetched(ctx, q, albumID, at)
	})
}

// Close cancels every fetch in flight and waits for them. It is safe to call
// more than once, and safe to call while requests are still in Listing.
//
// The ordering that makes it correct lives in lazyfetch.Gate.Close; this is the
// delegation, and TestCloseEndsAFetchInFlight is what proves the delegation is
// actually wired.
func (s *Service) Close() { s.gate.Close() }
```

- [ ] **Step 8: Amend exactly two tests in `internal/albumtracks/albumtracks_test.go`**

Nothing else in that file may change. It is ~1,250 lines and the rest of it is black-box.

**`TestNothingIsStartedAfterClose` — becomes a delegation test.** Delete its four internal lines
(`if !s.track() {...}`, `s.wg.Done()`, `if s.track() { s.wg.Done(); t.Error(...) }`) and keep the
black-box half verbatim: after `s.Close()`, `Listing` must claim no lease and call no fetcher. Add
this to its doc comment, above the existing one:

```go
// The direct assertions on the WaitGroup registration moved to
// internal/lazyfetch, where that mechanism now lives and where they are
// package-private rather than a reach-through. What is left here is the half
// that is this package's own to prove: that Listing, after Close, declines all
// the way down — no lease claimed, no fetcher called — which is the delegation
// working rather than the mechanism working.
```

**`TestCloseRefusesNewFetchesBeforeItWaits` — moves out.** Delete it from this file entirely,
including its comment block, which is transcribed into `internal/lazyfetch/lazyfetch_test.go` in
step 2. It cannot be expressed black-box: it needs `base.Done()` as a barrier and `track()` as the
probe, and both are now in the Gate. Leave a one-line pointer in its place so a reader of this file
is not left wondering where the ordering guard went:

```go
// The ordering inside Close — that `closing` is set before wg.Wait, and before
// the cancel that makes base.Done() a sound barrier — is pinned in
// internal/lazyfetch by TestCloseRefusesNewFetchesBeforeItWaits and
// TestCloseSetsClosingBeforeItCancels. It moved with the code it guards.
```

**`TestCloseEndsAFetchInFlight` stays unchanged**, and is now doing double duty: it is the only test
proving `Service.Close()` actually reaches `gate.Close()`. If somebody deletes the delegation, this
fails by hanging until its own 5-second guard.

Every other test in the file — including `TestAPanicDoesNotLeakASlot`, `TestABusySlotIsPendingNotUnavailable`,
`TestAClaimErrorIsPendingNotUnavailable`, `TestTwoConcurrentViewsProduceOneFetch`, the four
`TestDisabled*` and the truncation and emptiness tests — **must pass with no edit at all.** That is
this task's gate.

- [ ] **Step 9: Prove the refactor changed nothing**

```bash
cd C:/Users/Requi/source/repos/Encore
git diff --stat internal/albumtracks/albumtracks_test.go
go test -count=1 ./internal/albumtracks/ -v
go test -count=1 ./internal/httpapi/ ./internal/config/
go build ./... && go vet ./...
go test -tags=integration -count=1 ./test/e2e/ -run TestAlbumTracklist
```
Expected: the `git diff --stat` shows a small number of deleted lines in **one** file and no other
test file touched; every suite passes. `internal/httpapi` and `cmd/encore-api` must compile with no
edit — if either needs one, `albumtracks`' exported surface changed and the refactor overreached.

- [ ] **Step 10: Run the two required mutations**

Neither is optional. Both are reverted immediately after being observed; the point is to see the
detector fire, which is the only way to know it is a detector.

**Mutation A — the genuine defect.** In `internal/lazyfetch/lazyfetch.go`, rewrite `Close` as:

```go
func (g *Gate) Close() {
	g.cancel()
	g.wg.Wait()
	g.mu.Lock()
	g.closing = true
	g.mu.Unlock()
}
```

Run: `go test -count=1 ./internal/lazyfetch/ -run 'TestCloseRefusesNewFetchesBeforeItWaits|TestCloseSetsClosingBeforeItCancels' -v`
Expected: **both fail.** `TestCloseRefusesNewFetchesBeforeItWaits` fails because for as long as Close
is parked in Wait, `track` still says yes; `TestCloseSetsClosingBeforeItCancels` fails on both the
`closing < cancel` and `closing < wait` assertions. Revert.

**Mutation B — correct, and blinding.** Rewrite `Close` as:

```go
func (g *Gate) Close() {
	g.cancel()
	g.mu.Lock()
	g.closing = true
	g.mu.Unlock()
	g.wg.Wait()
}
```

Run: `go test -count=1 -run 'TestCloseRefusesNewFetchesBeforeItWaits' ./internal/lazyfetch/ -count=20`
Expected: **passes.** That is the point, and it matches the measurement recorded at
`albumtracks_test.go:1022` (30 of 30). This variant is not a defect — the invariant Close needs still
holds — but it has just silently destroyed the detector for Mutation A.

Then run: `go test -count=1 ./internal/lazyfetch/ -run TestCloseSetsClosingBeforeItCancels -v`
Expected: **fails**, on the `closing < cancel` assertion, with the message explaining that the
behavioural test has been blinded. Revert.

If Mutation B does *not* fail the ordering test, the ordering test is not doing its job and must be
fixed before this task is committed — a plan step that cannot fail is worse than no step.

- [ ] **Step 11: Vet, staticcheck, NUL-check and commit**

```bash
export PATH="$PATH:$(go env GOPATH)/bin"
go vet ./... && staticcheck ./internal/... ./cmd/...
perl -0777 -ne 'print "NULs: ", tr/\0//, "\n"' internal/lazyfetch/lazyfetch.go
perl -0777 -ne 'print "NULs: ", tr/\0//, "\n"' internal/lazyfetch/ordering_test.go
git add internal/lazyfetch/ internal/albumtracks/
git commit -m "$(cat <<'EOF'
Lazy fetch: one implementation of the lease, the backoff and the shutdown

The artist page needs exactly the lifecycle the album page already has — a
single-statement lease with an expiry, a TTL and failure backoff, a bounded slot
channel, a fetch detached onto a context no request owns, and a Close whose
ordering is the difference between a clean shutdown and a WaitGroup panic. Two
hundred and fifty lines that took four review rounds to get right.

A second copy would mean every future fix to any of it made twice, by somebody
who first has to notice the other copy exists. Nothing enforces that noticing,
and the drift is silent: one copy keeps a bug the other fixed, and only the page
somebody happens to open shows it.

The seam is a single Fill func(ctx, id) error. Pagination, empty-response
detection, truncation and the storing transaction stay with the caller, because
that is where the two genuinely differ — one treats an empty filtered set as
impossible and the other as an ordinary success. A Gate that knew that rule
would be wrong on its second caller.

internal/albumtracks keeps every exported name, signature and error string, so
httpapi and the composition root are untouched, and its whole suite passes with
two tests changed: the WaitGroup pokes moved to the layer that now owns them,
and what is left behind proves the delegation rather than the mechanism.

The guard is strengthened, not moved. Both race tests are mirrored at the shared
layer, and a new source-order test catches the mutation that leaves Close
correct while blinding them — measured at 30 passes out of 30, and until now
undetectable.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 5: The discography service

**Files:**
- Create: `internal/artistalbums/artistalbums.go`
- Create: `internal/artistalbums/artistalbums_test.go`

**Interfaces:**
- Consumes: `lazyfetch.Gate`/`Policy`/`Deps`/`State`/`Outcome`/`Status*` (Task 4), `config.ArtistAlbums` (Task 3), `catalog.ArtistAlbum`/`ArtistAlbumState`/`ArtistAlbum*` constants (Task 1), `spotify.ArtistAlbum` (Task 2), `store.Querier`, `store.Store.InTx`.
- Produces:
  - `artistalbums.New(cfg config.ArtistAlbums, deps Deps) (*Service, error)`
  - `(*Service).Discography(ctx context.Context, q store.Querier, artistID string) (Discography, error)`
  - `(*Service).Close()`
  - `artistalbums.Deps{Catalog Store; Spotify Fetcher; Writer Writer; Logger *slog.Logger; Now func() time.Time}`
  - `artistalbums.StoreWriter{Store *store.Store}`
  - `type State = lazyfetch.Outcome` with `StateReady`, `StatePending`, `StateUnavailable`, `StateDisabled` — aliases of the `lazyfetch.Outcome*` constants, so the two endpoints' vocabularies are one definition rather than two that a test has to keep aligned
  - `type Release struct { AlbumID, Name, Group string; ReleaseDate *time.Time; ReleasePrecision string }`
  - `type Discography struct { State State; Releases []Release; FetchedAt time.Time }`
  - `func (d Discography) CountedIDs() []string`
  - `const CountedGroup = catalog.AlbumGroupAlbum`

**Read `internal/albumtracks/albumtracks.go` as it stands after Task 4 before writing a line of this.** It is the reference for how a service sits on the Gate: an adapter for the two lease statements, a `fill` that does everything caller-specific, a `Discography`/`Listing` that reads its own rows and calls `gate.Resolve`, and a `Close` that delegates.

**What this task does NOT contain, because Task 4 owns it:** `due`, `start`, `track`, `record`, the slot channel, the base context, the WaitGroup, the mutex, and the three race-guard tests. If you find yourself writing any of them, the seam has been crossed. The one Close-related test here is a *delegation* test, and it is the only one this package needs.

- [ ] **Step 1: Write the failing service tests — the harness**

Create `internal/artistalbums/artistalbums_test.go`. The fakes are modelled on `internal/albumtracks/albumtracks_test.go:24-315`; read that file and mirror its structure rather than inventing new shapes.

```go
package artistalbums

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/RequiDev/encore/internal/config"
	"github.com/RequiDev/encore/internal/spotify"
	"github.com/RequiDev/encore/internal/store"
	"github.com/RequiDev/encore/internal/store/catalog"
)

// storeQuerier is store.Querier under a shorter name, so the fake's signatures
// stay readable. It is the same type, so the fake satisfies Store exactly.
type storeQuerier = store.Querier

// fakeCatalog stands in for the two tables. It is deliberately not a database:
// these tests are about *when* a fetch is started, which is policy.
type fakeCatalog struct {
	mu       sync.Mutex
	state    catalog.ArtistAlbumState
	rows     []catalog.ArtistAlbum
	claims   int
	writes   int
	fails    int
	claimOK  bool
	claimErr error
	// stored is what the last successful replace wrote.
	stored []catalog.ArtistAlbum
	// lastReason is what the service asked to be stored in last_error.
	lastReason string
	// txSeq numbers the transactions inlineWriter has opened and curTx is the one
	// in force, or 0 outside any. replaceTx and markTx capture which one each
	// write ran in, which is the only way to tell "both inside the same
	// transaction" from "both happened" — the two are identical in final state.
	txSeq     int
	curTx     int
	replaceTx int
	markTx    int
	// failCtxErr is the state of the context record actually used, which is the
	// only way to see whether it was detached from the one Close cancels.
	failCtxErr error
}

func (f *fakeCatalog) enterTx() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.txSeq++
	f.curTx = f.txSeq
}

func (f *fakeCatalog) leaveTx() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.curTx = 0
}

func (f *fakeCatalog) transactions() (replace, mark int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.replaceTx, f.markTx
}

func (f *fakeCatalog) ArtistAlbumState(context.Context, storeQuerier, string) (catalog.ArtistAlbumState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state, nil
}

func (f *fakeCatalog) ArtistAlbums(context.Context, storeQuerier, string) ([]catalog.ArtistAlbum, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.rows, nil
}

func (f *fakeCatalog) ClaimArtistAlbumFetch(_ context.Context, _ storeQuerier, _ string, _, _ time.Time) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.claims++
	if f.claimErr != nil {
		return false, f.claimErr
	}
	return f.claimOK, nil
}

func (f *fakeCatalog) ReplaceArtistAlbums(_ context.Context, _ storeQuerier, _ string, items []catalog.ArtistAlbum) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writes++
	f.replaceTx = f.curTx
	f.stored = append([]catalog.ArtistAlbum(nil), items...)
	return nil
}

func (f *fakeCatalog) MarkArtistAlbumsFetched(_ context.Context, _ storeQuerier, _ string, _ time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.markTx = f.curTx
	return nil
}

func (f *fakeCatalog) FailArtistAlbumFetch(ctx context.Context, _ storeQuerier, _ string, _ time.Time, reason string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fails++
	f.lastReason = reason
	f.failCtxErr = ctx.Err()
	return nil
}

func (f *fakeCatalog) counts() (claims, writes, fails int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.claims, f.writes, f.fails
}

func (f *fakeCatalog) storedRows() []catalog.ArtistAlbum {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]catalog.ArtistAlbum(nil), f.stored...)
}

func (f *fakeCatalog) reason() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastReason
}

func (f *fakeCatalog) recordContextErr() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.failCtxErr
}

// fakeFetcher stands in for the Spotify client.
type fakeFetcher struct {
	mu    sync.Mutex
	items []spotify.ArtistAlbum
	err   error
	calls int
	// block, when non-nil, holds the fetch open until it is closed, which is how
	// a test observes the state while a walk is genuinely in flight.
	block chan struct{}
}

func (f *fakeFetcher) ArtistAlbums(ctx context.Context, _ string, _ int) ([]spotify.ArtistAlbum, error) {
	f.mu.Lock()
	f.calls++
	block, items, err := f.block, f.items, f.err
	f.mu.Unlock()
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return items, err
}

func (f *fakeFetcher) called() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// inlineWriter runs the transaction body straight through, with no pool behind
// it. The Querier it hands over is nil, which is exactly right here: the fake
// catalogue ignores it, and these tests are about the *shape* of the write —
// that the replace and the mark happen inside one InTx — not its SQL.
type inlineWriter struct{ cat *fakeCatalog }

func (w inlineWriter) InTx(ctx context.Context, fn func(ctx context.Context, q store.Querier) error) error {
	w.cat.enterTx()
	defer w.cat.leaveTx()
	return fn(ctx, nil)
}

func (w inlineWriter) DB() store.Querier { return nil }

func discard() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

func newServiceWith(t *testing.T, cfg config.ArtistAlbums, cat *fakeCatalog, fetch *fakeFetcher, now time.Time) *Service {
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

func newService(t *testing.T, cat *fakeCatalog, fetch *fakeFetcher, now time.Time) *Service {
	t.Helper()
	return newServiceWith(t, config.ArtistAlbums{Enabled: true, TTL: 7 * 24 * time.Hour}, cat, fetch, now)
}

func newDisabledService(t *testing.T, cat *fakeCatalog, fetch *fakeFetcher, now time.Time) *Service {
	t.Helper()
	return newServiceWith(t, config.ArtistAlbums{Enabled: false, TTL: 7 * 24 * time.Hour}, cat, fetch, now)
}

func album(id, name string) spotify.ArtistAlbum {
	return spotify.ArtistAlbum{ID: id, Name: name, Group: catalog.AlbumGroupAlbum, ReleasePrecision: "year"}
}
```

- [ ] **Step 2: Write the failing service tests — the policy through the seam**

Several of these look like duplicates of Task 4's Gate tests. They are not, and the difference is
worth being explicit about, because a test that can only fail if *another package* is broken is a
test this project would rather delete.

What each one below can actually catch is **this service's wiring**: that `cfg.TTL` and `cfg.Enabled`
reach the `Policy`, that the five timing constants reach it in the right fields, that
`catalog.ArtistAlbumState`'s four fields map onto `lazyfetch.State`'s four without a swap (`FetchedAt`
and `AttemptedAt` are the same type and drive different arms of the schedule — transposing them
compiles and silently turns the TTL into the failure backoff), that the `leases` adapter calls the
right two repository methods, and that `fill` is what the Gate runs. None of that is covered by
`internal/lazyfetch`, whose tests use a fake with no repository behind it at all.

Append to the same file:

```go
var at = time.Date(2026, time.July, 31, 12, 0, 0, 0, time.UTC)

func TestFirstViewStartsTheWalkAndReportsPending(t *testing.T) {
	cat := &fakeCatalog{claimOK: true}
	fetch := &fakeFetcher{block: make(chan struct{})}
	s := newService(t, cat, fetch, at)

	got, err := s.Discography(context.Background(), nil, "artist-1")
	if err != nil {
		t.Fatalf("Discography: %v", err)
	}
	if got.State != StatePending {
		t.Fatalf("state = %q, want %q on an artist nobody has enumerated", got.State, StatePending)
	}
	if len(got.Releases) != 0 {
		t.Fatalf("returned %d releases before any walk finished", len(got.Releases))
	}
	close(fetch.block)
	s.Close()
	if fetch.called() != 1 {
		t.Fatalf("the client was called %d times, want 1", fetch.called())
	}
}

// TestDiscographyDoesNotWaitForSpotify is the load-bearing property of the
// whole feature: the page request answers while the walk is still running.
func TestDiscographyDoesNotWaitForSpotify(t *testing.T) {
	cat := &fakeCatalog{claimOK: true}
	fetch := &fakeFetcher{block: make(chan struct{})}
	s := newService(t, cat, fetch, at)

	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := s.Discography(context.Background(), nil, "artist-1"); err != nil {
			t.Errorf("Discography: %v", err)
		}
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Discography did not return while the walk was still in flight; the page waits on Spotify")
	}
	close(fetch.block)
}

// TestTheDiscographyAndItsStatusCommitTogether is the single-transaction
// requirement. Nothing about the final state on disk distinguishes "both wrote"
// from "both wrote in one transaction", so the fake numbers its transactions and
// this asserts the two writes carry the same number.
func TestTheDiscographyAndItsStatusCommitTogether(t *testing.T) {
	cat := &fakeCatalog{claimOK: true}
	fetch := &fakeFetcher{items: []spotify.ArtistAlbum{album("a1", "First")}}
	s := newService(t, cat, fetch, at)

	if _, err := s.Discography(context.Background(), nil, "artist-1"); err != nil {
		t.Fatalf("Discography: %v", err)
	}
	s.Close()

	replaceTx, markTx := cat.transactions()
	if replaceTx == 0 || markTx == 0 {
		t.Fatalf("replace ran in tx %d and mark in tx %d; one of them ran outside any transaction",
			replaceTx, markTx)
	}
	if replaceTx != markTx {
		t.Fatalf("replace ran in tx %d and mark in tx %d; a crash between them leaves a listing "+
			"with no 'ok' beside it, or an 'ok' beside a listing that was never finished",
			replaceTx, markTx)
	}
}

// TestATruncatedWalkWritesNothing is the ErrTruncated rule, and it is the one
// most likely to be quietly broken, because the error arrives *with* real data.
// ReplaceArtistAlbums is delete-absent, so writing the prefix would delete the
// tail of a discography that was correct and then mark the result authoritative.
func TestATruncatedWalkWritesNothing(t *testing.T) {
	cat := &fakeCatalog{claimOK: true}
	fetch := &fakeFetcher{
		items: []spotify.ArtistAlbum{album("a1", "First"), album("a2", "Second")},
		err:   spotify.ErrTruncated,
	}
	s := newService(t, cat, fetch, at)

	if _, err := s.Discography(context.Background(), nil, "artist-1"); err != nil {
		t.Fatalf("Discography: %v", err)
	}
	s.Close()

	_, writes, fails := cat.counts()
	if writes != 0 {
		t.Fatalf("a truncated walk wrote %d times, want 0: the prefix would delete the tail of a "+
			"discography that was correct", writes)
	}
	if fails != 1 {
		t.Fatalf("a truncated walk recorded %d failures, want 1", fails)
	}
	if len(cat.storedRows()) != 0 {
		t.Fatalf("a truncated walk stored %d rows, want 0", len(cat.storedRows()))
	}
}

// TestAnEmptyResponseIsAFailure pins the emptiness rule at the level it belongs
// to: the *whole* response. An artist in this catalogue is there because
// somebody played a track by them, so a 200 with no items means the artist is
// invisible to this application's market, not that they have released nothing.
func TestAnEmptyResponseIsAFailure(t *testing.T) {
	cat := &fakeCatalog{claimOK: true}
	// The exact shape spotify.ArtistAlbums returns for a 200 carrying no items.
	fetch := &fakeFetcher{items: nil, err: nil}
	s := newService(t, cat, fetch, at)

	if _, err := s.Discography(context.Background(), nil, "artist-1"); err != nil {
		t.Fatalf("Discography: %v", err)
	}
	s.Close()

	_, writes, fails := cat.counts()
	if writes != 0 {
		t.Fatalf("an empty response wrote %d times, want 0", writes)
	}
	if fails != 1 {
		t.Fatalf("an empty response recorded %d failures, want 1: stored as a success it would make "+
			"the page say this artist has released nothing", fails)
	}
}

// TestAnAllSinglesDiscographyIsStoredAsASuccess is the one rule that differs
// from 2e-i, and the one a transcription of albumtracks.go is most likely to get
// wrong by adding a guard that does not belong. An album with no tracks is
// impossible; an artist who has only released singles is ordinary, and their
// discography must be stored, marked 'ok', and served as ready — with a counted
// set that happens to be empty.
func TestAnAllSinglesDiscographyIsStoredAsASuccess(t *testing.T) {
	cat := &fakeCatalog{claimOK: true}
	fetch := &fakeFetcher{items: []spotify.ArtistAlbum{
		{ID: "a1", Name: "One", Group: catalog.AlbumGroupSingle},
		{ID: "a2", Name: "Two", Group: catalog.AlbumGroupAppearsOn},
	}}
	s := newService(t, cat, fetch, at)

	if _, err := s.Discography(context.Background(), nil, "artist-1"); err != nil {
		t.Fatalf("Discography: %v", err)
	}
	s.Close()

	_, writes, fails := cat.counts()
	if fails != 0 {
		t.Fatalf("an all-singles discography recorded %d failures, want 0: zero *albums* is a fact "+
			"about the artist, not a failed read", fails)
	}
	if writes != 1 {
		t.Fatalf("an all-singles discography wrote %d times, want 1", writes)
	}
	if n := len(cat.storedRows()); n != 2 {
		t.Fatalf("stored %d rows, want both non-album groups kept: the page cannot say what it set "+
			"aside if the service drops it", n)
	}
}

// TestCountedIDsIsOnlyTheAlbumGroup pins the filter's one definition. Coverage
// counts album_group 'album' and nothing else, and the handler asks which ids to
// look up through this rather than re-deriving the predicate.
func TestCountedIDsIsOnlyTheAlbumGroup(t *testing.T) {
	d := Discography{Releases: []Release{
		{AlbumID: "a1", Group: catalog.AlbumGroupAlbum},
		{AlbumID: "s1", Group: catalog.AlbumGroupSingle},
		{AlbumID: "c1", Group: catalog.AlbumGroupCompilation},
		{AlbumID: "p1", Group: catalog.AlbumGroupAppearsOn},
		{AlbumID: "x1", Group: "ep"},
		{AlbumID: "a2", Group: catalog.AlbumGroupAlbum},
	}}
	got := d.CountedIDs()
	if len(got) != 2 || got[0] != "a1" || got[1] != "a2" {
		t.Fatalf("CountedIDs() = %v, want [a1 a2]: only album_group 'album' is counted, and a group "+
			"Spotify adds later must not silently join the denominator", got)
	}
}

func TestAFreshDiscographyIsNotRefetched(t *testing.T) {
	cat := &fakeCatalog{
		claimOK: true,
		state:   catalog.ArtistAlbumState{Status: catalog.ArtistAlbumOK, FetchedAt: at.Add(-24 * time.Hour)},
		rows:    []catalog.ArtistAlbum{{AlbumID: "a1", Name: "First", Group: catalog.AlbumGroupAlbum}},
	}
	fetch := &fakeFetcher{}
	s := newService(t, cat, fetch, at)

	got, err := s.Discography(context.Background(), nil, "artist-1")
	if err != nil {
		t.Fatalf("Discography: %v", err)
	}
	s.Close()

	if got.State != StateReady {
		t.Fatalf("state = %q, want %q", got.State, StateReady)
	}
	if fetch.called() != 0 {
		t.Fatalf("a one-day-old discography was refetched %d times against a seven-day TTL", fetch.called())
	}
	if claims, _, _ := cat.counts(); claims != 0 {
		t.Fatalf("a fresh discography claimed the lease %d times, want 0", claims)
	}
}

func TestAnExpiredDiscographyIsRefetchedAndStillServed(t *testing.T) {
	cat := &fakeCatalog{
		claimOK: true,
		state:   catalog.ArtistAlbumState{Status: catalog.ArtistAlbumOK, FetchedAt: at.Add(-8 * 24 * time.Hour)},
		rows:    []catalog.ArtistAlbum{{AlbumID: "a1", Name: "First", Group: catalog.AlbumGroupAlbum}},
	}
	fetch := &fakeFetcher{items: []spotify.ArtistAlbum{album("a1", "First"), album("a2", "Second")}}
	s := newService(t, cat, fetch, at)

	got, err := s.Discography(context.Background(), nil, "artist-1")
	if err != nil {
		t.Fatalf("Discography: %v", err)
	}
	// The stale listing is served *now*, not withheld until the refresh lands.
	if got.State != StateReady || len(got.Releases) != 1 {
		t.Fatalf("state = %q with %d releases, want ready with the 1 already stored", got.State, len(got.Releases))
	}
	if got.FetchedAt.IsZero() {
		t.Fatal("fetchedAt is zero on a stale ready listing; the page cannot say how old it is")
	}
	s.Close()
	if fetch.called() != 1 {
		t.Fatalf("the client was called %d times, want 1: an eight-day-old discography is past a "+
			"seven-day TTL", fetch.called())
	}
}

func TestAFailedWalkIsNotRetriedImmediately(t *testing.T) {
	cat := &fakeCatalog{
		claimOK: true,
		state:   catalog.ArtistAlbumState{Status: catalog.ArtistAlbumFailed, AttemptedAt: at.Add(-time.Minute)},
	}
	fetch := &fakeFetcher{}
	s := newService(t, cat, fetch, at)

	got, err := s.Discography(context.Background(), nil, "artist-1")
	if err != nil {
		t.Fatalf("Discography: %v", err)
	}
	s.Close()

	if got.State != StateUnavailable {
		t.Fatalf("state = %q, want %q inside the retry backoff", got.State, StateUnavailable)
	}
	if fetch.called() != 0 {
		t.Fatalf("a failure one minute old was retried %d times against a fifteen-minute backoff",
			fetch.called())
	}
}

func TestAFailedWalkIsRetriedAfterTheBackoff(t *testing.T) {
	cat := &fakeCatalog{
		claimOK: true,
		state:   catalog.ArtistAlbumState{Status: catalog.ArtistAlbumFailed, AttemptedAt: at.Add(-16 * time.Minute)},
	}
	fetch := &fakeFetcher{items: []spotify.ArtistAlbum{album("a1", "First")}}
	s := newService(t, cat, fetch, at)

	got, err := s.Discography(context.Background(), nil, "artist-1")
	if err != nil {
		t.Fatalf("Discography: %v", err)
	}
	if got.State != StatePending {
		t.Fatalf("state = %q, want %q once the backoff has elapsed", got.State, StatePending)
	}
	s.Close()
	if fetch.called() != 1 {
		t.Fatalf("the client was called %d times, want 1", fetch.called())
	}
}

// TestALiveLeaseIsNotEvenClaimed pins the guard that keeps a polling browser
// from attempting a write on every tick.
func TestALiveLeaseIsNotEvenClaimed(t *testing.T) {
	cat := &fakeCatalog{
		claimOK: true,
		state:   catalog.ArtistAlbumState{Status: catalog.ArtistAlbumFetching, AttemptedAt: at.Add(-time.Second)},
	}
	fetch := &fakeFetcher{}
	s := newService(t, cat, fetch, at)

	got, err := s.Discography(context.Background(), nil, "artist-1")
	if err != nil {
		t.Fatalf("Discography: %v", err)
	}
	s.Close()

	if got.State != StatePending {
		t.Fatalf("state = %q, want %q while another replica holds the lease", got.State, StatePending)
	}
	if claims, _, _ := cat.counts(); claims != 0 {
		t.Fatalf("a live lease was claimed %d times, want 0: a polling tab would write on every tick",
			claims)
	}
}

func TestAnExpiredLeaseIsReclaimed(t *testing.T) {
	cat := &fakeCatalog{
		claimOK: true,
		state:   catalog.ArtistAlbumState{Status: catalog.ArtistAlbumFetching, AttemptedAt: at.Add(-4 * time.Minute)},
	}
	fetch := &fakeFetcher{items: []spotify.ArtistAlbum{album("a1", "First")}}
	s := newService(t, cat, fetch, at)

	if _, err := s.Discography(context.Background(), nil, "artist-1"); err != nil {
		t.Fatalf("Discography: %v", err)
	}
	s.Close()
	if claims, _, _ := cat.counts(); claims != 1 {
		t.Fatalf("an expired lease was claimed %d times, want 1: the artist is stranded in 'fetching' "+
			"for ever without this", claims)
	}
}

// TestALostClaimStartsNoSecondWalk covers the losing side of the lease.
func TestALostClaimStartsNoSecondWalk(t *testing.T) {
	cat := &fakeCatalog{claimOK: false}
	fetch := &fakeFetcher{}
	s := newService(t, cat, fetch, at)

	got, err := s.Discography(context.Background(), nil, "artist-1")
	if err != nil {
		t.Fatalf("Discography: %v", err)
	}
	s.Close()

	if got.State != StatePending {
		t.Fatalf("state = %q, want %q: a walk *is* running, just not this one", got.State, StatePending)
	}
	if fetch.called() != 0 {
		t.Fatalf("a lost claim still walked %d times", fetch.called())
	}
}

// TestAClaimErrorIsPendingNotUnavailable is the copy-relevant one. A claim that
// errors records no outcome, so the very next request re-enters this branch;
// calling it unavailable would blame Spotify for a local fault and tell a page
// whose job is to stop polling on unavailable to give up on a walk that was
// still coming.
func TestAClaimErrorIsPendingNotUnavailable(t *testing.T) {
	cat := &fakeCatalog{claimErr: errors.New("read-only transaction")}
	fetch := &fakeFetcher{}
	s := newService(t, cat, fetch, at)

	got, err := s.Discography(context.Background(), nil, "artist-1")
	if err != nil {
		t.Fatalf("Discography: %v", err)
	}
	s.Close()

	if got.State != StatePending {
		t.Fatalf("state = %q, want %q: nothing recorded an outcome, so nothing may report one",
			got.State, StatePending)
	}
	if _, _, fails := cat.counts(); fails != 0 {
		t.Fatalf("a claim error recorded %d failures, want 0", fails)
	}
}

// TestTheErrorFromSpotifyIsNeverReturnedToTheCaller: a third-party outage is a
// state the page renders, not a 500 it shows.
func TestTheErrorFromSpotifyIsNeverReturnedToTheCaller(t *testing.T) {
	cat := &fakeCatalog{claimOK: true}
	fetch := &fakeFetcher{err: errors.New("spotify: 503")}
	s := newService(t, cat, fetch, at)

	if _, err := s.Discography(context.Background(), nil, "artist-1"); err != nil {
		t.Fatalf("Discography returned %v; a Spotify failure is a state, not a 500", err)
	}
	s.Close()
	if _, _, fails := cat.counts(); fails != 1 {
		t.Fatalf("recorded %d failures, want 1", fails)
	}
}

// TestTheFailureReasonIsRecordedWhole pins that the service hands the cause over
// intact. catalog.FailArtistAlbumFetch bounds it with store.Truncate, which cuts
// on a rune boundary; truncating again here would only risk two ellipses.
func TestTheFailureReasonIsRecordedWhole(t *testing.T) {
	cat := &fakeCatalog{claimOK: true}
	fetch := &fakeFetcher{err: errors.New("spotify: artist albums: 503 service unavailable")}
	s := newService(t, cat, fetch, at)

	if _, err := s.Discography(context.Background(), nil, "artist-1"); err != nil {
		t.Fatalf("Discography: %v", err)
	}
	s.Close()
	if got := cat.reason(); got != "spotify: artist albums: 503 service unavailable" {
		t.Fatalf("recorded reason = %q, want the cause whole", got)
	}
}

// TestTheWalkOutlivesTheRequestContext: the page request has already been
// answered, and cancelling when the browser navigated away would mean the
// discography never arrives however many times the page is opened.
func TestTheWalkOutlivesTheRequestContext(t *testing.T) {
	cat := &fakeCatalog{claimOK: true}
	release := make(chan struct{})
	fetch := &fakeFetcher{items: []spotify.ArtistAlbum{album("a1", "First")}, block: release}
	s := newService(t, cat, fetch, at)

	ctx, cancel := context.WithCancel(context.Background())
	if _, err := s.Discography(ctx, nil, "artist-1"); err != nil {
		t.Fatalf("Discography: %v", err)
	}
	cancel() // the browser navigated away
	close(release)
	s.Close()

	_, writes, fails := cat.counts()
	if writes != 1 || fails != 0 {
		t.Fatalf("writes = %d, fails = %d, want 1 and 0: the walk died with the request that started it",
			writes, fails)
	}
}

// TestAFailureIsStillRecordedWhenCloseCancelsTheWalk pins the detached record
// context. Without context.WithoutCancel the write goes out on a cancelled
// context, records nothing, and the artist stays 'fetching' until the lease
// expires — which is exactly the strand the lease exists to bound, arriving
// through a door nothing else closes.
func TestAFailureIsStillRecordedWhenCloseCancelsTheWalk(t *testing.T) {
	cat := &fakeCatalog{claimOK: true}
	fetch := &fakeFetcher{block: make(chan struct{})}
	s := newService(t, cat, fetch, at)

	if _, err := s.Discography(context.Background(), nil, "artist-1"); err != nil {
		t.Fatalf("Discography: %v", err)
	}
	s.Close() // cancels the walk, then waits for it

	if _, _, fails := cat.counts(); fails != 1 {
		t.Fatalf("recorded %d failures after Close cancelled the walk, want 1", fails)
	}
	if err := cat.recordContextErr(); err != nil {
		t.Fatalf("the failure was recorded on a context already cancelled (%v); "+
			"context.WithoutCancel is what keeps that write alive through shutdown", err)
	}
}

func TestNewRejectsAnIncompleteConfiguration(t *testing.T) {
	cat := &fakeCatalog{}
	fetch := &fakeFetcher{}
	ok := config.ArtistAlbums{Enabled: true, TTL: time.Hour}

	for name, deps := range map[string]Deps{
		"no catalog": {Spotify: fetch, Writer: inlineWriter{cat: cat}},
		"no spotify": {Catalog: cat, Writer: inlineWriter{cat: cat}},
		"no writer":  {Catalog: cat, Spotify: fetch},
	} {
		if _, err := New(ok, deps); err == nil {
			t.Errorf("New with %s succeeded; a half-wired service answers some artists and panics on others", name)
		}
	}
	if _, err := New(config.ArtistAlbums{Enabled: true, TTL: 0}, Deps{
		Catalog: cat, Spotify: fetch, Writer: inlineWriter{cat: cat},
	}); err == nil {
		t.Error("New with a zero TTL succeeded; every stored discography would be permanently due")
	}
}

// TestDisabledMakesNoRequestAndClaimsNoLease is what an operator asking for no
// unattended traffic actually asked for: not even the write.
func TestDisabledMakesNoRequestAndClaimsNoLease(t *testing.T) {
	cat := &fakeCatalog{claimOK: true}
	fetch := &fakeFetcher{}
	s := newDisabledService(t, cat, fetch, at)

	got, err := s.Discography(context.Background(), nil, "artist-1")
	if err != nil {
		t.Fatalf("Discography: %v", err)
	}
	s.Close()

	if got.State != StateDisabled {
		t.Fatalf("state = %q, want %q", got.State, StateDisabled)
	}
	if fetch.called() != 0 {
		t.Fatalf("a disabled instance made %d requests, want 0", fetch.called())
	}
	if claims, _, _ := cat.counts(); claims != 0 {
		t.Fatalf("a disabled instance claimed the lease %d times, want 0", claims)
	}
}

// TestDisabledIsNotUnavailable keeps the two facts apart at the source. A page
// that renders "Spotify would not answer" for "nobody asked Spotify" blames a
// third party for a local decision.
func TestDisabledIsNotUnavailable(t *testing.T) {
	cat := &fakeCatalog{}
	s := newDisabledService(t, cat, &fakeFetcher{}, at)

	got, err := s.Discography(context.Background(), nil, "artist-1")
	if err != nil {
		t.Fatalf("Discography: %v", err)
	}
	if got.State == StateUnavailable {
		t.Fatal("a switched-off instance reported unavailable; that is a recorded Spotify failure, " +
			"and no request was ever made")
	}
}

// TestDisabledStillServesAStaleDiscographyWithoutRefreshing: off means "do not
// fetch", not "forget what is on disk", and the TTL is not even consulted.
func TestDisabledStillServesAStaleDiscographyWithoutRefreshing(t *testing.T) {
	cat := &fakeCatalog{
		claimOK: true,
		state: catalog.ArtistAlbumState{
			Status: catalog.ArtistAlbumOK, FetchedAt: at.Add(-400 * 24 * time.Hour),
		},
		rows: []catalog.ArtistAlbum{{AlbumID: "a1", Name: "First", Group: catalog.AlbumGroupAlbum}},
	}
	fetch := &fakeFetcher{}
	s := newDisabledService(t, cat, fetch, at)

	got, err := s.Discography(context.Background(), nil, "artist-1")
	if err != nil {
		t.Fatalf("Discography: %v", err)
	}
	s.Close()

	if got.State != StateReady || len(got.Releases) != 1 {
		t.Fatalf("state = %q with %d releases, want ready with the stored one: withholding a listing "+
			"that was correct when it was read is strictly worse than showing it with its date",
			got.State, len(got.Releases))
	}
	if got.FetchedAt.IsZero() {
		t.Fatal("fetchedAt is zero; on an instance that will never refresh, the date is the only honesty available")
	}
	if fetch.called() != 0 || cat.claims != 0 {
		t.Fatalf("a disabled instance refreshed a year-old discography: %d requests, %d claims",
			fetch.called(), cat.claims)
	}
}
```

**Fails when:**
- `…StartsTheWalkAndReportsPending` — the first view stops starting a fetch, or reports something other than pending.
- `…DoesNotWaitForSpotify` — the walk is moved in-request.
- `…CommitTogether` — the replace and the mark are split across two `InTx` calls, or either is moved outside one.
- `…TruncatedWalkWritesNothing` — an `errors.Is(err, spotify.ErrTruncated)` special case is added to store the prefix.
- `…EmptyResponseIsAFailure` — the `len(items) == 0` guard is removed, or moved to after the filter.
- `…AllSinglesDiscographyIsStoredAsASuccess` — a guard on the *filtered* set is added, which is the transcription error this task is most exposed to.
- `…CountedIDsIsOnlyTheAlbumGroup` — the predicate widens to `!= appears_on`, or an unknown group starts being counted.
- `…FreshDiscographyIsNotRefetched` / `…ExpiredDiscographyIsRefetched…` — the TTL comparison flips, or `due` stops consulting `FetchedAt`.
- `…FailedWalkIsNotRetriedImmediately` / `…AfterTheBackoff` — `failedRetryAfter` is replaced by the TTL, which would turn one bad minute into a week-long broken panel.
- `…LiveLeaseIsNotEvenClaimed` — the `pending` pre-check is dropped and every poll tick attempts a write.
- `…ExpiredLeaseIsReclaimed` — the `fetching` branch of `due` is dropped, stranding the artist for ever.
- `…LostClaimStartsNoSecondWalk` / `…ClaimErrorIsPendingNotUnavailable` — a decline path starts reporting `unavailable`.
- `…ErrorFromSpotifyIsNeverReturned…` — `fetch`'s error is propagated out of `Discography`.
- `…FailureReasonIsRecordedWhole` — the service truncates before the repository does.
- `…WalkOutlivesTheRequestContext` — the service stops handing `fill` to the Gate and does its own fetching on the request's context, so navigating away loses the discography. (The detachment itself is the Gate's and is pinned there; what this catches is this service reintroducing a fetch outside it.)
- `…StillRecordedWhenCloseCancels…` — `context.WithoutCancel` is removed from `record`.
- `…NewRejects…` — a nil check or the TTL check is dropped.
- `…Disabled*` — `s.enabled` is checked after `s.due` instead of before, or `disabled` is folded into `unavailable`, or a cached listing stops being served when fetching is off.

- [ ] **Step 3: Add the one Close test this package needs**

The shutdown race, the WaitGroup discipline and the ordering inside `Close` are pinned in
`internal/lazyfetch` by `TestNothingIsStartedAfterClose`,
`TestCloseRefusesNewFetchesBeforeItWaits` and `TestCloseSetsClosingBeforeItCancels` (Task 4). Do not
re-transcribe them here — a second copy of a test against code this package does not own is a test
that passes for reasons this package cannot break.

What *is* this package's own to prove is that its `Close` is wired to the Gate's at all. Without
this, deleting the body of `Service.Close` leaves every test in `internal/lazyfetch` green and every
test here green, and the process leaks a goroutine and loses a failure write at every shutdown.

```go
// TestCloseEndsAWalkInFlight is the delegation test: Service.Close must reach
// the Gate's Close, which is what cancels a detached walk and waits for it.
//
// The mechanism it delegates to — the closing-under-mutex ordering, the
// WaitGroup, the slot channel — is pinned in internal/lazyfetch, where it lives.
// This asserts only the wiring, and it is the whole of what this package can get
// wrong about shutdown.
func TestCloseEndsAWalkInFlight(t *testing.T) {
	cat := &fakeCatalog{claimOK: true}
	block := make(chan struct{}) // deliberately never closed by the test body
	fetch := &fakeFetcher{block: block}
	s := newService(t, cat, fetch, at)

	// A rescue, registered after the service so it runs *before* Cleanup's Close:
	// if Close turns out not to cancel anything, the assertion below reports that
	// rather than hanging the whole package on a stuck Cleanup.
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(block) }) })

	if _, err := s.Discography(context.Background(), nil, "artist-1"); err != nil {
		t.Fatalf("Discography: %v", err)
	}

	closed := make(chan struct{})
	go func() {
		defer close(closed)
		s.Close()
	}()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return while a walk was in flight; Service.Close does not reach the " +
			"Gate's, so the walk is on a context nothing cancels and outlives the service")
	}

	// And the cancelled walk still recorded its failure, which is what keeps the
	// artist out of 'fetching' rather than stranded there until the lease expires.
	if _, _, fails := cat.counts(); fails != 1 {
		t.Fatalf("recorded %d failures for a cancelled walk, want 1", fails)
	}
}
```

**Fails when:** `Service.Close()` stops delegating to `s.gate.Close()` — by far the likeliest way
this package breaks shutdown, and invisible to every test on either side of the seam.

- [ ] **Step 4: Run the tests to verify they fail**

Run: `go test -count=1 ./internal/artistalbums/`
Expected: FAIL — the package does not exist.

- [ ] **Step 5: Write the service**

Create `internal/artistalbums/artistalbums.go`. Note how much of it is *not* here: no `due`, no
`start`, no `track`, no `record`, no slot channel, no base context, no WaitGroup, no mutex. All of
that is `lazyfetch.Gate`'s, and what is left is the part that is genuinely about artists.

```go
// Package artistalbums fills and serves the cached listing of an artist's own
// releases, which is what lets the artist page say "you have heard 4 of this
// artist's 11 albums".
//
// Nothing here is a background loop. A sweep over every artist in a history is
// rejected explicitly, so a discography is read the first time somebody opens
// that artist's page and then kept for the configured TTL. The page request
// itself never waits for Spotify: Discography answers from the database, and
// internal/lazyfetch decides whether a walk is due and runs it detached.
//
// # What is here
//
// The parts a discography does not share with anything else: reading it from
// Spotify, deciding that an empty response is a failure, storing the rows and
// their 'ok' in one transaction, and knowing which album_group counts. The
// lease, the schedule, the concurrency bound and the shutdown ordering are
// internal/lazyfetch's, behind the Fill seam.
//
// One rule here is worth stating because it is exactly where this package and
// its sibling internal/albumtracks differ, and why the shared machinery stops
// where it does. There is no such record as an album with no tracks, so an empty
// track listing is a failure. There *is* such an artist as one who has only
// released singles, so an empty album-group set is an ordinary success. The
// emptiness guard below is on the whole response and never on the filtered
// subset — and a Gate that knew that rule for one caller would be wrong for the
// other.
package artistalbums

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/RequiDev/encore/internal/config"
	"github.com/RequiDev/encore/internal/lazyfetch"
	"github.com/RequiDev/encore/internal/spotify"
	"github.com/RequiDev/encore/internal/store"
	"github.com/RequiDev/encore/internal/store/catalog"
)

const (
	// maxPages bounds one artist's walk. Twenty pages of fifty is a thousand
	// releases, which no artist in a personal listening history approaches even
	// counting every appearance; it exists so a paging bug cannot spend the
	// instance's quota on one record.
	//
	// The same page count as albumtracks' and much less headroom, because a
	// discography includes every single, compilation and appears_on credit. That
	// is also why the bound must not be tightened casually: a truncated walk is
	// recorded as a failure and never stored, so a bound below a real artist's
	// release count leaves their panel reading "could not be read" for ever.
	maxPages = 20
	// concurrency is how many discographies this process walks at once. Small on
	// purpose: these start inside page requests, and they draw on the same quota
	// enrichment needs to do its job.
	concurrency = 4
	// leaseTTL is how long a 'fetching' row holds other callers off. Longer than
	// fetchTimeout — lazyfetch.New refuses the pair otherwise — so a live walk
	// never loses its own lease, and short enough that a process killed mid-walk
	// does not strand the artist for long.
	leaseTTL = 3 * time.Minute
	// fetchTimeout bounds one artist's whole walk — every page, every retry and
	// every rate-limit wait inside it. Longer than albumtracks' ninety seconds
	// because this walk is up to twenty sequential requests rather than one.
	fetchTimeout = 120 * time.Second
	// failedRetryAfter is how long a failed discography is left alone. Failures
	// here are timeouts and rate limits, which clear in minutes; making somebody
	// wait out the seven-day TTL would turn one bad minute into a broken panel.
	failedRetryAfter = 15 * time.Minute
	// recordTimeout bounds the write that records a failure, including during
	// shutdown.
	recordTimeout = 5 * time.Second
)

// CountedGroup is the one album_group discography completion counts.
//
// Singles, compilations and appearances are excluded, because "you have heard 4
// of 340 releases" is not a useful sentence. It is a named constant with one
// definition so the service, the API and anything that follows cannot each
// decide the predicate for themselves — and so that a group Spotify adds later
// joins the *excluded* side by default rather than silently entering the
// denominator.
const CountedGroup = catalog.AlbumGroupAlbum

// errEmptyDiscography is a 200 that carried no releases.
//
// An artist is in this catalogue because somebody played a track by them, so
// they have released something. An empty response means the artist is invisible
// to this application's market, or Spotify has withdrawn them. Storing it as a
// success would make the page say "you have played something from every album by
// this artist", which is the exact overclaim this feature exists to avoid.
//
// Note the level this applies at: the *whole* response. A response carrying
// forty singles and no albums is a complete success with an empty counted set,
// and the page has its own words for that.
var errEmptyDiscography = errors.New("artistalbums: spotify returned no releases for this artist")

// State is what the page can say about the discography.
//
// An alias of lazyfetch.Outcome rather than a second declaration: the four words
// are an API contract this endpoint shares with the album tracklist, and one
// definition is what stops them forking.
type State = lazyfetch.Outcome

const (
	// StateReady means a discography is stored and can be reasoned about. It is
	// also the state for an artist whose every release is a single: nothing is
	// counted, and that is an answer rather than an absence.
	StateReady = lazyfetch.OutcomeReady
	// StatePending means nothing is stored yet and a walk is running, or is due
	// and nothing has recorded a reason it should not be.
	StatePending = lazyfetch.OutcomePending
	// StateUnavailable means nothing is stored and the last attempt failed. It is
	// emphatically not "this artist has released nothing".
	StateUnavailable = lazyfetch.OutcomeUnavailable
	// StateDisabled means nothing is stored and this instance will not fetch it,
	// because its operator turned that off.
	StateDisabled = lazyfetch.OutcomeDisabled
)

// Release is one entry of a discography.
type Release struct {
	AlbumID string
	Name    string
	// Group is Spotify's album_group, carried through unchanged. Filtering to
	// CountedGroup is the caller's job, because the caller is also the thing that
	// has to say what it set aside.
	Group            string
	ReleaseDate      *time.Time
	ReleasePrecision string
}

// Discography is what one artist's page is told.
type Discography struct {
	State State
	// Releases is everything Spotify listed, in release order, not just the
	// album-group part and not just the unheard part. Which of them were played
	// is the caller's question to ask, because only the caller knows whose
	// history it is asking about; which of them count is the caller's to apply,
	// because only the caller has to write the sentence naming the rest.
	Releases []Release
	// FetchedAt is when the discography was read. Zero when none has succeeded.
	FetchedAt time.Time
}

// CountedIDs is the album ids discography completion is taken over: the
// CountedGroup ones, in the order they were listed.
//
// The one definition of the filter. A caller that wants to know which of these
// the listener has played asks with exactly this set, so the numerator and the
// denominator can never be taken over different populations.
func (d Discography) CountedIDs() []string {
	out := make([]string, 0, len(d.Releases))
	for _, r := range d.Releases {
		if r.Group == CountedGroup {
			out = append(out, r.AlbumID)
		}
	}
	return out
}

// Fetcher is the slice of the Spotify client this package uses.
type Fetcher interface {
	ArtistAlbums(ctx context.Context, artistID string, maxPages int) ([]spotify.ArtistAlbum, error)
}

// Store is the slice of the catalogue repository this package uses. An interface
// so the fill above can be exercised without a database.
type Store interface {
	ArtistAlbumState(ctx context.Context, q store.Querier, artistID string) (catalog.ArtistAlbumState, error)
	ArtistAlbums(ctx context.Context, q store.Querier, artistID string) ([]catalog.ArtistAlbum, error)
	ClaimArtistAlbumFetch(ctx context.Context, q store.Querier, artistID string, now, leaseCutoff time.Time) (bool, error)
	ReplaceArtistAlbums(ctx context.Context, q store.Querier, artistID string, items []catalog.ArtistAlbum) error
	MarkArtistAlbumsFetched(ctx context.Context, q store.Querier, artistID string, at time.Time) error
	FailArtistAlbumFetch(ctx context.Context, q store.Querier, artistID string, at time.Time, reason string) error
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
// package that names pgx.
type StoreWriter struct{ Store *store.Store }

// InTx runs fn inside one transaction.
func (w StoreWriter) InTx(ctx context.Context, fn func(ctx context.Context, q store.Querier) error) error {
	return w.Store.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error { return fn(ctx, tx) })
}

// DB returns the pool as a Querier.
func (w StoreWriter) DB() store.Querier { return w.Store.DB() }

// leases adapts the catalogue's two lease statements to lazyfetch.Leases. It is
// the whole of what the Gate knows about artist_album_fetches.
type leases struct{ cat Store }

func (l leases) Claim(
	ctx context.Context, q store.Querier, artistID string, now, leaseCutoff time.Time,
) (bool, error) {
	return l.cat.ClaimArtistAlbumFetch(ctx, q, artistID, now, leaseCutoff)
}

func (l leases) Fail(
	ctx context.Context, q store.Querier, artistID string, at time.Time, reason string,
) error {
	return l.cat.FailArtistAlbumFetch(ctx, q, artistID, at, reason)
}

// Deps is everything the service needs.
type Deps struct {
	Catalog Store
	Spotify Fetcher
	Writer  Writer
	Logger  *slog.Logger
	// Now is the clock. Tests replace it; production leaves it nil.
	Now func() time.Time
}

// Service fills and serves artist discographies.
type Service struct {
	cat  Store
	sp   Fetcher
	w    Writer
	log  *slog.Logger
	now  func() time.Time
	gate *lazyfetch.Gate
}

// New validates the dependencies and builds the service.
//
// The caller owns Close and must call it during shutdown, before closing the
// database pool: walks run detached from any request, so nothing else will ever
// wait for them, and a walk cancelled at shutdown still needs the pool to record
// that it failed. Constructing this after the pool and deferring Close
// immediately puts both in the right order.
func New(cfg config.ArtistAlbums, deps Deps) (*Service, error) {
	switch {
	case deps.Catalog == nil:
		return nil, errors.New("artistalbums: catalog repository is required")
	case deps.Spotify == nil:
		return nil, errors.New("artistalbums: spotify client is required")
	case deps.Writer == nil:
		return nil, errors.New("artistalbums: writer is required")
	case cfg.TTL <= 0:
		return nil, errors.New("artistalbums: a positive TTL is required")
	}
	lg := deps.Logger
	if lg == nil {
		lg = slog.Default()
	}
	now := deps.Now
	if now == nil {
		now = time.Now
	}
	s := &Service{
		cat: deps.Catalog,
		sp:  deps.Spotify,
		w:   deps.Writer,
		log: lg.With("component", "artistalbums"),
		now: now,
	}
	gate, err := lazyfetch.New(lazyfetch.Policy{
		Enabled:          cfg.Enabled,
		TTL:              cfg.TTL,
		LeaseTTL:         leaseTTL,
		FailedRetryAfter: failedRetryAfter,
		FetchTimeout:     fetchTimeout,
		RecordTimeout:    recordTimeout,
		Concurrency:      concurrency,
	}, lazyfetch.Deps{
		Leases:  leases{cat: deps.Catalog},
		Fill:    s.fill,
		DB:      deps.Writer.DB,
		Subject: "artist",
		Logger:  s.log,
		Now:     now,
	})
	if err != nil {
		return nil, err
	}
	s.gate = gate
	if !cfg.Enabled {
		// Said once, at startup, rather than on every page view: an operator who
		// wonders why the artist page reports this as turned off can find it here
		// and in the configuration line the process logs beside it.
		lg.Info("artist discographies are turned off; this instance will not ask spotify " +
			"what an artist has released. Discographies already cached are still shown.")
	}
	return s, nil
}

// Discography returns the stored discography for one artist, and starts a
// refresh when one is due.
//
// It never blocks on Spotify and it never fails because Spotify did: a
// third-party outage is a state the page renders, not a 500 it shows.
func (s *Service) Discography(ctx context.Context, q store.Querier, artistID string) (Discography, error) {
	st, err := s.cat.ArtistAlbumState(ctx, q, artistID)
	if err != nil {
		return Discography{}, err
	}

	var releases []Release
	if !st.FetchedAt.IsZero() {
		rows, err := s.cat.ArtistAlbums(ctx, q, artistID)
		if err != nil {
			return Discography{}, err
		}
		releases = make([]Release, 0, len(rows))
		for _, r := range rows {
			releases = append(releases, Release{
				AlbumID: r.AlbumID, Name: r.Name, Group: r.Group,
				ReleaseDate: r.ReleaseDate, ReleasePrecision: r.ReleasePrecision,
			})
		}
	}

	out := Discography{Releases: releases, FetchedAt: st.FetchedAt}
	// The stored predicate is !FetchedAt.IsZero(), deliberately, and not
	// len(releases) > 0 — this is the one argument this service passes the Gate
	// that differs in substance from its sibling's. A successful read always
	// stores at least one row, because an empty response is recorded as a
	// failure, so a successful read with no *counted* releases is an artist whose
	// every release is a single. That is ready, and it has its own copy. Passing
	// a row count here would be identical today and would quietly become wrong
	// the moment anything filters before this point.
	out.State = s.gate.Resolve(ctx, q, artistID, lazyfetch.State{
		Status:      st.Status,
		FetchedAt:   st.FetchedAt,
		AttemptedAt: st.AttemptedAt,
		Attempts:    st.Attempts,
	}, !st.FetchedAt.IsZero())
	return out, nil
}

// fill reads one artist's discography and stores it. It is the Gate's Fill:
// whether and when it runs is the Gate's decision, and everything it does is
// this package's.
func (s *Service) fill(ctx context.Context, artistID string) error {
	items, err := s.sp.ArtistAlbums(ctx, artistID, maxPages)
	if err != nil {
		// Every failure lands here, ErrTruncated included — and that one arrives
		// with a partial discography attached. The partial must never reach the
		// write: ReplaceArtistAlbums deletes whatever the incoming set does not
		// contain, so a prefix would delete the tail of a listing that was correct
		// and then mark the result authoritative. This project has hit that trap
		// three times; internal/spotify/library.go's ErrTruncated comment is the
		// record of it. There is no exception clause here on purpose.
		return err
	}
	if len(items) == 0 {
		// A 200 carrying no items, or one whose every item had a blank id, both of
		// which spotify.ArtistAlbums reports as (nil, nil). Checked here rather
		// than left to ReplaceArtistAlbums' own refusal, because by then the intent
		// would already be "make this artist's discography empty" and only an error
		// would stop it; returned as a failure, the stored listing and its date are
		// untouched.
		//
		// This is the *whole* response, and deliberately not the CountedGroup
		// subset. An artist whose every release is a single has an empty counted
		// set and a perfectly good discography, and recording that as a failure
		// would tell them Spotify would not answer about an artist Spotify answered
		// about at length.
		return errEmptyDiscography
	}

	rows := make([]catalog.ArtistAlbum, 0, len(items))
	for i, it := range items {
		rows = append(rows, catalog.ArtistAlbum{
			AlbumID: it.ID, Name: it.Name, Group: it.Group,
			ReleaseDate: it.ReleaseDate, ReleasePrecision: it.ReleasePrecision,
			// The index in the walk, kept only to break ties in the read order.
			Position: i,
		})
	}
	at := s.now()
	return s.w.InTx(ctx, func(ctx context.Context, q store.Querier) error {
		if err := s.cat.ReplaceArtistAlbums(ctx, q, artistID, rows); err != nil {
			return err
		}
		// In the same transaction as the rows: the discography and the claim that
		// it is authoritative commit together, so a reader can never see a
		// half-replaced listing marked 'ok'.
		return s.cat.MarkArtistAlbumsFetched(ctx, q, artistID, at)
	})
}

// Close cancels every walk in flight and waits for them. It is safe to call more
// than once, and safe to call while requests are still in Discography.
//
// The ordering that makes it correct lives in lazyfetch.Gate.Close; this is the
// delegation, and TestCloseEndsAWalkInFlight is what proves the delegation is
// actually wired. It must be called *before* the database pool closes: a
// cancelled walk still records its failure, and a closed pool would lose that
// write, leaving the artist 'fetching' until its lease expires.
func (s *Service) Close() { s.gate.Close() }
```

- [ ] **Step 6: Run the tests to verify they pass**

```bash
go test -count=1 ./internal/artistalbums/ -v
```
Expected: PASS, every test.

- [ ] **Step 7: Vet, staticcheck, NUL-check and commit**

```bash
export PATH="$PATH:$(go env GOPATH)/bin"
go vet ./internal/artistalbums/ && staticcheck ./internal/artistalbums/
perl -0777 -ne 'print "NULs: ", tr/\0//, "\n"' internal/artistalbums/artistalbums.go
git add internal/artistalbums/
git commit -m "$(cat <<'EOF'
Discography: walk an artist's releases without making anyone wait for it

The artist page answers from the database while the Spotify walk runs behind it,
so a page that would otherwise hang on a third party does not. Whether and when
a walk starts is internal/lazyfetch's, which the album track cache has used
since the previous commit; this supplies the parts a discography does not share
with anything else.

Which is most of what is interesting here. Reading up to twenty pages, refusing
to store a truncated one, committing the rows and their 'ok' together, and
knowing that album_group 'album' is what completion counts and the rest is what
the page has to name.

Including the one rule that decided where the shared machinery stops. There is
no such record as an album with no tracks, so an empty track listing is a
failure. There is such an artist as one who has only released singles, so an
empty album-group set is an ordinary success. The emptiness guard is on the
whole response and never on the filtered subset — and a Gate that knew that
rule for one caller would be wrong for the other.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 6: The endpoint

**Files:**
- Create: `internal/stats/discography.go`
- Modify: `internal/stats/stats_test.go` (register the new statement; extend the not-range-scoped guard)
- Modify: `internal/httpapi/dto.go` (append after `toAlbumTrackList`, before `--- genres ---`)
- Modify: `internal/httpapi/dto_test.go`
- Modify: `internal/httpapi/entities.go` (append after `handleAlbumTracklist`)
- Modify: `internal/httpapi/router.go:104` region
- Modify: `internal/httpapi/server.go` (Deps field, interface, struct field, nil check, assignment)
- Modify: `cmd/encore-api/main.go:142-175` region
- Modify: `test/e2e/e2e_test.go` (stub route, counter, instance wiring, four tests)
- Modify: `docs/api.md` (new section after "Which tracks you have never played")

**Interfaces:**
- Consumes: `artistalbums.Service.Discography`, `Discography.CountedIDs()`, `artistalbums.CountedGroup`, `catalog.AlbumGroup*`, `s.catalog.GetArtist`.
- Produces:
  - `(*stats.Service).HeardAlbums(ctx context.Context, q store.Querier, userID uuid.UUID, albumIDs []string) ([]string, error)`
  - `httpapi.ArtistDiscography{State string; Coverage CoverageResponse; Missing []DiscographyAlbumRef; Excluded DiscographyExcluded; FetchedAt *time.Time}`
  - `httpapi.DiscographyAlbumRef{ID, Name string; ReleaseDate *string; ReleasePrecision string}`
  - `httpapi.DiscographyExcluded{Singles, Compilations, AppearsOn, Other int64}`
  - `httpapi.Deps.ArtistAlbums artistDiscographySource`
  - route `GET /api/artists/{id}/discography`

- [ ] **Step 1: Write the failing stats test**

Append to `internal/stats/stats_test.go` (and add `{name: "heardAlbums", sql: heardAlbumsSQL, params: 2}` to the `statements()` slice, immediately after `albumHeardTracks`):

```go
// TestHeardAlbumsSQLIsNotRangeScoped pins for HeardAlbums what
// TestAlbumHeardTracksSQLIsNotRangeScoped pins beside it, and for the same
// reason: the call takes no range argument at all, so a test that only ever
// calls it can never vary the range and show the answer is independent of one.
// What can be pinned is the composed statement — it may not reference played_at,
// the only column a range predicate could be written against. "You have never
// played this album" is a fact about a listening lifetime; scoping it to
// whatever window the page happens to be showing would report an album heard
// five years ago as one this listener has never touched.
func TestHeardAlbumsSQLIsNotRangeScoped(t *testing.T) {
	if strings.Contains(heardAlbumsSQL, "played_at") {
		t.Errorf("heardAlbums references played_at; discography coverage is all-time, like the album "+
			"completion figure it sits beside:\n%s", heardAlbumsSQL)
	}
}

// TestHeardAlbumsRejectsANilUser keeps a zero uuid from reaching SQL looking
// like a legitimate parameter, where it would match nothing rather than fail.
func TestHeardAlbumsRejectsANilUser(t *testing.T) {
	s := &Service{}
	if _, err := s.HeardAlbums(context.Background(), nil, uuid.Nil, []string{"a1"}); err == nil {
		t.Fatal("HeardAlbums with a nil user succeeded; it would silently report nothing heard")
	} else if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("error = %v, want domain.ErrValidation", err)
	}
}

// TestHeardAlbumsShortCircuitsAnEmptySet pins that no statement is sent for an
// artist with nothing counted — the all-singles case, which is an ordinary
// artist rather than an edge case. The nil Querier is the assertion: reaching
// the database at all would panic.
func TestHeardAlbumsShortCircuitsAnEmptySet(t *testing.T) {
	s := &Service{}
	got, err := s.HeardAlbums(context.Background(), nil, uuid.New(), nil)
	if err != nil {
		t.Fatalf("HeardAlbums with no ids: %v", err)
	}
	if got == nil {
		t.Fatal("HeardAlbums returned nil rather than an empty slice; a caller that ranges over it " +
			"is fine either way, but one that reports len(nil) as unknown is not")
	}
	if len(got) != 0 {
		t.Fatalf("got %v, want empty", got)
	}
}
```

`stats_test.go` already imports `strings` and `errors`; add `context` and `github.com/google/uuid` if they are not already imported.

**Fails when:** `heardAlbumsSQL` gains a range predicate; the nil-user guard is dropped; the empty-set short circuit is dropped (the nil `Querier` then panics) or is changed to return `nil` instead of an empty slice. `TestBlacklistIsAppliedEverywhere` and `TestParameterNumberingIsContiguous` in the same file fail automatically once the statement is registered without a blacklist filter or with mismatched parameter numbering.

- [ ] **Step 2: Run it to verify it fails**

Run: `go test -count=1 ./internal/stats/ -run 'TestHeardAlbums|TestBlacklistIsApplied|TestParameterNumbering'`
Expected: FAIL — `heardAlbumsSQL undefined`.

- [ ] **Step 3: Write the stats query**

Create `internal/stats/discography.go`:

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

// heardAlbumsSQL reports which of a given set of albums the user has ever
// played anything from.
//
// The set comes in as a parameter rather than being derived from a join,
// because the albums in question are Spotify's list of what an artist released
// and most of them are not in `albums` at all — that table is minted from
// listening. A join would answer "which albums by this artist have you played",
// which is the numerator masquerading as the denominator.
//
// Deliberately the same predicates as albumHeardTracksSQL: the user, the
// blacklist, and no range. A record heard five years ago is not one this
// listener has never played, whatever window the page happens to be showing.
//
// The blacklist filter is kept for consistency with every other read of
// `listens`, and it does have a visible consequence: a blacklisted artist's own
// page reports 0 of 11 heard. That is the same answer every other figure on
// that page gives, and the page already says the artist is excluded.
//
// Parameters are $1 user, $2 the album ids.
var heardAlbumsSQL = fmt.Sprintf(`
SELECT DISTINCT t.album_id
FROM listens l
JOIN tracks t ON t.id = l.track_id
WHERE l.user_id = $1 AND t.album_id = ANY($2) AND %s`, blacklistFilter("l"))

// HeardAlbums reports which of albumIDs the user has ever played anything from.
//
// "Played" is per album, not per track: one play of one track puts the album in
// this set. The caller says so in its copy, because "you have heard 4 of their
// 11 albums" would otherwise be read as having heard those four in full.
//
// It is returned as ids rather than as a count because only the caller knows
// which discography it is diffing against, and a count would not survive the two
// disagreeing about what the artist released.
func (s *Service) HeardAlbums(
	ctx context.Context,
	q store.Querier,
	userID uuid.UUID,
	albumIDs []string,
) ([]string, error) {
	// No range to validate, but a nil user id must not reach SQL looking like a
	// legitimate parameter — it would match nothing rather than fail.
	if userID == uuid.Nil {
		return nil, fmt.Errorf("%w: a user is required", domain.ErrValidation)
	}
	if len(albumIDs) == 0 {
		// An artist whose every release is a single has nothing counted, which is
		// an ordinary answer rather than an edge case. Sending `= ANY('{}')` would
		// spend a round trip to be told what is already known.
		return []string{}, nil
	}
	rows, err := q.Query(ctx, heardAlbumsSQL, store.UUIDArg(userID), albumIDs)
	if err != nil {
		return nil, postgres.Classify("heard albums", err)
	}
	defer rows.Close()

	out := make([]string, 0, len(albumIDs))
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, postgres.Classify("heard albums", err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, postgres.Classify("heard albums", err)
	}
	return out, nil
}
```

- [ ] **Step 4: Run the stats tests**

Run: `go test -count=1 ./internal/stats/`
Expected: PASS.

- [ ] **Step 5: Write the failing DTO tests**

Append to `internal/httpapi/dto_test.go`:

```go
// TestArtistDiscographyStatesSerialiseDistinctly is the mechanical guard on the
// four-way distinction the whole feature rests on: a client that cannot tell
// "disabled" from "unavailable" blames Spotify for a local decision.
func TestArtistDiscographyStatesSerialiseDistinctly(t *testing.T) {
	states := []artistalbums.State{
		artistalbums.StateReady, artistalbums.StatePending,
		artistalbums.StateUnavailable, artistalbums.StateDisabled,
	}
	seen := make(map[string]artistalbums.State, len(states))
	for _, st := range states {
		out := toArtistDiscography(artistalbums.Discography{State: st}, nil)
		if out.State != string(st) {
			t.Fatalf("artistalbums.State %q serialised as %q", st, out.State)
		}
		if prev, dup := seen[out.State]; dup {
			t.Fatalf("states %q and %q both serialise as %q", prev, st, out.State)
		}
		seen[out.State] = st
	}
}

// TestTheLazyFetchStatesKeepTheirWireValues pins the four strings both
// endpoints put on the wire.
//
// After Task 4 the two services alias one set of constants, so they cannot fork
// from each other — an earlier draft of this plan had a test comparing them
// pair by pair, and against aliases that test cannot fail, which makes it worse
// than no test. What *can* still break is the value itself: editing
// lazyfetch.OutcomeReady to "done" compiles, passes every distinctness check,
// and silently breaks every deployed client at once, because these strings are
// a published contract (docs/api.md names all four). So this asserts the
// literals.
func TestTheLazyFetchStatesKeepTheirWireValues(t *testing.T) {
	for want, got := range map[string]string{
		"ready":       string(artistalbums.StateReady),
		"pending":     string(artistalbums.StatePending),
		"unavailable": string(artistalbums.StateUnavailable),
		"disabled":    string(artistalbums.StateDisabled),
	} {
		if got != want {
			t.Errorf("a state serialises as %q, want %q: these four strings are published in "+
				"docs/api.md and branched on by every client", got, want)
		}
	}
	// And the album endpoint puts the same four on the wire, which is true by
	// construction now that both alias lazyfetch.Outcome — asserted anyway,
	// because "by construction" lasts exactly until somebody re-declares one.
	if string(albumtracks.StateReady) != string(artistalbums.StateReady) ||
		string(albumtracks.StatePending) != string(artistalbums.StatePending) ||
		string(albumtracks.StateUnavailable) != string(artistalbums.StateUnavailable) ||
		string(albumtracks.StateDisabled) != string(artistalbums.StateDisabled) {
		t.Error("the two endpoints' state vocabularies have forked; one of them has stopped " +
			"aliasing lazyfetch.Outcome and a client branching on one is now wrong about the other")
	}
}

// discography builds a stored, ready listing with every group represented, so
// the derivation below has something to filter.
func discographyFixture() artistalbums.Discography {
	day := func(y int) *time.Time {
		at := time.Date(y, time.January, 1, 0, 0, 0, 0, time.UTC)
		return &at
	}
	return artistalbums.Discography{
		State:     artistalbums.StateReady,
		FetchedAt: time.Date(2026, time.July, 20, 9, 0, 0, 0, time.UTC),
		Releases: []artistalbums.Release{
			{AlbumID: "alb-3", Name: "Third", Group: "album", ReleaseDate: day(2022), ReleasePrecision: "year"},
			{AlbumID: "alb-2", Name: "Second", Group: "album", ReleaseDate: day(2019), ReleasePrecision: "year"},
			{AlbumID: "alb-1", Name: "First", Group: "album", ReleaseDate: day(2016), ReleasePrecision: "year"},
			{AlbumID: "sng-1", Name: "A Single", Group: "single"},
			{AlbumID: "sng-2", Name: "Another Single", Group: "single"},
			{AlbumID: "cmp-1", Name: "Best Of", Group: "compilation"},
			{AlbumID: "app-1", Name: "Somebody Else's Record", Group: "appears_on"},
			{AlbumID: "epx-1", Name: "An EP", Group: "ep"},
		},
	}
}

// TestArtistDiscographyCountsOnlyAlbums is the arithmetic §5.2 asks for: the
// denominator is album_group 'album' and nothing else, because "you have heard 4
// of 340 releases" is not a useful sentence.
func TestArtistDiscographyCountsOnlyAlbums(t *testing.T) {
	out := toArtistDiscography(discographyFixture(), []string{"alb-2", "sng-1"})

	if out.Coverage.Total != 3 {
		t.Fatalf("coverage.total = %d, want 3: only album_group 'album' counts", out.Coverage.Total)
	}
	// The played single is not counted, and it is not "missing" either: it is not
	// in the population at all.
	if out.Coverage.Covered != 1 {
		t.Fatalf("coverage.covered = %d, want 1", out.Coverage.Covered)
	}
	if len(out.Missing) != 2 {
		t.Fatalf("missing has %d entries, want 2", len(out.Missing))
	}
	for _, m := range out.Missing {
		if m.ID == "sng-1" || m.ID == "cmp-1" || m.ID == "app-1" || m.ID == "epx-1" {
			t.Fatalf("missing names %q, which is not an album and is not in the denominator", m.ID)
		}
	}
	// Newest first, as stored.
	if out.Missing[0].ID != "alb-3" || out.Missing[1].ID != "alb-1" {
		t.Fatalf("missing = %v, want the stored order preserved", out.Missing)
	}
	if out.Missing[0].ReleaseDate == nil || *out.Missing[0].ReleaseDate != "2022" {
		t.Fatalf("release date = %v, want the year-precision \"2022\"", out.Missing[0].ReleaseDate)
	}
}

// TestArtistDiscographyNamesWhatItExcluded is the copy problem's whole
// mechanism. "4 of 11 albums" over 340 unmentioned releases is an overclaim by
// omission, and the page can only say otherwise if these numbers travel with the
// response.
func TestArtistDiscographyNamesWhatItExcluded(t *testing.T) {
	out := toArtistDiscography(discographyFixture(), nil)

	want := DiscographyExcluded{Singles: 2, Compilations: 1, AppearsOn: 1, Other: 1}
	if out.Excluded != want {
		t.Fatalf("excluded = %+v, want %+v", out.Excluded, want)
	}
}

// TestArtistDiscographyExclusionsAccountForEveryRelease pins the property that
// makes the excluded breakdown trustworthy rather than decorative: the four
// buckets plus the counted albums equal the number of releases stored. Without
// `Other`, a group Spotify adds later would vanish from both the numerator and
// the breakdown, and the page's "Spotify also lists…" sentence would quietly
// undercount.
func TestArtistDiscographyExclusionsAccountForEveryRelease(t *testing.T) {
	d := discographyFixture()
	out := toArtistDiscography(d, []string{"alb-1"})

	sum := out.Coverage.Total + out.Excluded.Singles + out.Excluded.Compilations +
		out.Excluded.AppearsOn + out.Excluded.Other
	if sum != int64(len(d.Releases)) {
		t.Fatalf("counted %d + excluded = %d, want the %d releases stored; a release is in neither "+
			"bucket, so the page's breakdown undercounts", out.Coverage.Total, sum, len(d.Releases))
	}
	if out.Coverage.Covered+int64(len(out.Missing)) != out.Coverage.Total {
		t.Fatalf("covered %d + missing %d != total %d; every counted album must be exactly one of the two",
			out.Coverage.Covered, len(out.Missing), out.Coverage.Total)
	}
}

// TestArtistDiscographyMissingIsNeverNull keeps a client from needing a guard,
// and the assertion is made against a Go value rather than against decoded JSON
// on purpose: encoding/json decodes `[]` to a non-nil slice, so a test that
// round-tripped through the wire would pass against a nil field and prove
// nothing.
func TestArtistDiscographyMissingIsNeverNull(t *testing.T) {
	for _, st := range []artistalbums.State{
		artistalbums.StatePending, artistalbums.StateUnavailable, artistalbums.StateDisabled,
	} {
		out := toArtistDiscography(artistalbums.Discography{State: st}, nil)
		if out.Missing == nil {
			t.Errorf("missing is nil on state %q; a client iterating it needs a guard it should not need", st)
		}
	}
}

// TestArtistDiscographyAllSinglesIsReadyWithNothingCounted is the state with no
// counterpart on the album endpoint. It must not look like a failure, and it
// must not look like "you have played everything" either — the page tells those
// apart by coverage.total being zero on a ready listing.
func TestArtistDiscographyAllSinglesIsReadyWithNothingCounted(t *testing.T) {
	out := toArtistDiscography(artistalbums.Discography{
		State:     artistalbums.StateReady,
		FetchedAt: time.Date(2026, time.July, 20, 9, 0, 0, 0, time.UTC),
		Releases: []artistalbums.Release{
			{AlbumID: "sng-1", Name: "One", Group: "single"},
			{AlbumID: "sng-2", Name: "Two", Group: "single"},
		},
	}, nil)

	if out.State != "ready" {
		t.Fatalf("state = %q, want \"ready\": an artist who has only released singles was read " +
			"successfully")
	}
	if out.Coverage.Total != 0 || len(out.Missing) != 0 {
		t.Fatalf("coverage = %+v with %d missing, want nothing counted", out.Coverage, len(out.Missing))
	}
	if out.Excluded.Singles != 2 {
		t.Fatalf("excluded.singles = %d, want 2: the page has nothing else to describe the artist with",
			out.Excluded.Singles)
	}
	if out.FetchedAt == nil {
		t.Fatal("fetchedAt is absent on a ready listing")
	}
}
```

**Fails when:**
- `…StatesSerialiseDistinctly` — two states collapse to one string.
- `…KeepTheirWireValues` — any of the four published strings changes value, which compiles and silently breaks every deployed client.
- `…CountsOnlyAlbums` — the filter widens, the stored order is re-sorted, or a played non-album starts counting.
- `…NamesWhatItExcluded` — a bucket is dropped or miscounted.
- `…ExclusionsAccountForEveryRelease` — the `Other` bucket is removed, so an unknown group falls out of both sides.
- `…MissingIsNeverNull` — `Missing` is left nil instead of `make(...)`.
- `…AllSinglesIsReadyWithNothingCounted` — the derivation starts treating an empty counted set as pending or unavailable.

Add `"github.com/RequiDev/encore/internal/artistalbums"` and `"time"` to the file's imports.

- [ ] **Step 6: Run them to verify they fail**

Run: `go test -count=1 ./internal/httpapi/ -run 'TestArtistDiscography|TestTheLazyFetchStates'`
Expected: FAIL — `toArtistDiscography undefined`.

- [ ] **Step 7: Write the DTO**

Append to `internal/httpapi/dto.go`, immediately after `toAlbumTrackList` and before the `--- genres ---` banner:

```go
// DiscographyAlbumRef is one release from an artist's own discography.
//
// Deliberately not an AlbumRef. These come from Spotify's list of what an
// artist released rather than from the catalogue, and an album nobody has played
// is not in the catalogue at all — see migrations/00014_artist_albums.sql.
// Giving it the shape of a catalogue entity would invite a client to link to an
// album page that 404s, which is precisely what most of these would do.
//
// No image: artwork would make it look more like a link than it is, and the
// listing does not need one to name a record.
type DiscographyAlbumRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// The same partial-date pair AlbumRef carries: "2016", "2016-05" or
	// "2016-05-20", with the precision beside it so a client renders only what
	// Spotify actually knew.
	ReleaseDate      *string `json:"releaseDate"`
	ReleasePrecision string  `json:"releasePrecision"`
}

// DiscographyExcluded is what coverage did *not* count.
//
// It exists so the page can say what it set aside. "You have heard 4 of 11
// albums" is true and, without this, misleading: an artist with 340 singles and
// appearances looks like an artist with 11 releases. A client MUST render these
// numbers alongside the coverage rather than treating them as diagnostics.
//
// Other is any album_group Spotify sends that is none of the four it documents.
// It is zero today and is a field rather than a silent drop so the four buckets
// plus coverage.total always account for every release stored: a group added
// upstream joins the excluded side and is counted, instead of disappearing from
// both the numerator and the sentence describing the remainder.
type DiscographyExcluded struct {
	Singles      int64 `json:"singles"`
	Compilations int64 `json:"compilations"`
	AppearsOn    int64 `json:"appearsOn"`
	Other        int64 `json:"other"`
}

// ArtistDiscography is how much of an artist's own catalogue the caller has
// played.
//
// State is one of:
//
//	"ready"       — a discography is stored; Coverage, Missing and Excluded mean
//	                something
//	"pending"     — no discography yet, and nothing has recorded a reason there
//	                should not be one
//	"unavailable" — no discography, and none is being read: the last attempt
//	                failed
//	"disabled"    — no discography, and this instance does not fetch them at all
//	                (ENCORE_ARTIST_ALBUMS_ENABLED=false)
//
// The same four words, with the same meanings, as AlbumTrackList's — pinned
// one definition since Task 4 (both alias lazyfetch.Outcome), and their wire
// values are pinned by TestTheLazyFetchStatesKeepTheirWireValues.
//
// A client MUST render all four differently, and must never read anything but
// "ready" as "you have played everything by them". Missing is empty in three of
// the four, which is exactly why State exists.
//
// **Coverage counts album_group "album" only.** Singles, compilations and
// appearances are excluded, because "you have heard 4 of 340 releases" is not a
// useful sentence — and a client that renders Coverage without also rendering
// Excluded is making a claim this payload does not support.
//
// **"Ready" with Coverage.Total == 0 is a real answer, not an empty one.** An
// artist whose every release is a single has nothing to count, and Excluded is
// then the only thing that describes them. This has no counterpart on the album
// endpoint, where an empty listing is impossible and is recorded as a failure.
//
// **Covered counts albums with any play, not albums played in full.** One track
// off a record puts it in Covered. A client must say so, or "you have heard 4 of
// their 11 albums" reads as four albums heard end to end.
//
// "pending" is deliberately not phrased as "a fetch is running": it also covers
// a lease another replica holds, no free local slot on this one, a shutdown in
// progress, and — the one that matters here — a claim against
// artist_album_fetches that errored, after which nothing was read and nothing
// was recorded, so the very next request re-enters this same branch. Nothing in
// the payload bounds how long that can go on, and every "pending" response for
// one artist is byte-identical regardless of how long the state has held. A
// client MUST cap how long it keeps polling on "pending" and render the
// "unavailable" copy once that cap is reached.
//
// "disabled" is deliberately distinct from "unavailable". The first is the
// operator's choice and the second is Spotify failing to answer; a client that
// renders the failure copy for the first blames a third party for a local
// decision.
//
// A discography already cached is still served as "ready" when fetching is
// disabled, past its TTL or not — turning off fetching does not hide what is on
// disk. FetchedAt is what keeps that honest, and it is the reason there is no
// separate "this will never refresh" field.
type ArtistDiscography struct {
	State    string           `json:"state"`
	Coverage CoverageResponse `json:"coverage"`
	// Missing is the counted albums with no play, in the order they were listed
	// — newest release first. Always present and never null, so a client can
	// iterate it without a guard; it is empty when everything was played, when
	// nothing is counted, and when there is no discography at all, which is
	// exactly why State exists.
	Missing  []DiscographyAlbumRef `json:"missing"`
	Excluded DiscographyExcluded   `json:"excluded"`
	// FetchedAt is when the discography was last read from Spotify, absent until
	// one has succeeded.
	FetchedAt *time.Time `json:"fetchedAt,omitempty"`
}

// toArtistDiscography diffs the discography against what the caller has played
// and tallies what was set aside.
//
// One pass, one classification per release: every release either counts (and is
// then Covered or Missing, never neither and never both) or lands in exactly one
// excluded bucket. That is what makes the invariant
// TestArtistDiscographyExclusionsAccountForEveryRelease asserts hold by
// construction rather than by luck.
//
// The diff is done here rather than in SQL because the two halves come from
// different places for different reasons: the discography is global catalogue
// data cached from Spotify, and the played set is one user's own history with
// their own blacklist applied.
func toArtistDiscography(d artistalbums.Discography, heard []string) ArtistDiscography {
	played := make(map[string]struct{}, len(heard))
	for _, id := range heard {
		played[id] = struct{}{}
	}

	out := ArtistDiscography{
		State:   string(d.State),
		Missing: make([]DiscographyAlbumRef, 0, len(d.Releases)),
	}
	for _, r := range d.Releases {
		if r.Group != artistalbums.CountedGroup {
			switch r.Group {
			case catalog.AlbumGroupSingle:
				out.Excluded.Singles++
			case catalog.AlbumGroupCompilation:
				out.Excluded.Compilations++
			case catalog.AlbumGroupAppearsOn:
				out.Excluded.AppearsOn++
			default:
				// A group Spotify documents but this build does not know, or a blank
				// one. Counted rather than dropped so the breakdown still accounts for
				// every release stored.
				out.Excluded.Other++
			}
			continue
		}
		out.Coverage.Total++
		if _, ok := played[r.AlbumID]; ok {
			out.Coverage.Covered++
			continue
		}
		out.Missing = append(out.Missing, DiscographyAlbumRef{
			ID:               r.AlbumID,
			Name:             r.Name,
			ReleaseDate:      partialDate(r.ReleaseDate, r.ReleasePrecision),
			ReleasePrecision: r.ReleasePrecision,
		})
	}
	if !d.FetchedAt.IsZero() {
		at := d.FetchedAt.UTC()
		out.FetchedAt = &at
	}
	return out
}

// partialDate renders a release date at the precision Spotify supplied,
// matching releaseDate() above. It is separate because that one takes a
// domain.Album and this takes the two fields directly; folding them together
// would mean building a domain.Album for a record that is not in the catalogue.
func partialDate(at *time.Time, precision string) *string {
	if at == nil {
		return nil
	}
	var s string
	switch precision {
	case "year":
		s = at.Format("2006")
	case "month":
		s = at.Format("2006-01")
	default:
		s = at.Format("2006-01-02")
	}
	return &s
}
```

Add `"github.com/RequiDev/encore/internal/artistalbums"` and `"github.com/RequiDev/encore/internal/store/catalog"` to `dto.go`'s imports if not already present.

- [ ] **Step 8: Run the DTO tests to verify they pass**

Run: `go test -count=1 ./internal/httpapi/ -run 'TestArtistDiscography|TestTheLazyFetchStates' -v`
Expected: PASS.

- [ ] **Step 9: Wire the handler, the route and the server dependency**

In `internal/httpapi/server.go`, beside `albumTrackSource`:

```go
// artistDiscographySource is the artist discography cache as the HTTP layer
// needs it.
//
// An interface rather than the concrete service, for the reason albumTrackSource
// is one: this package holds no SQL and never imports pgx, and the handler is
// exercised without a Spotify client behind it.
type artistDiscographySource interface {
	Discography(ctx context.Context, q store.Querier, artistID string) (artistalbums.Discography, error)
}
```

Add to `Deps`, beside `AlbumTracks`:

```go
	// ArtistAlbums serves the cached discography of an artist and starts a
	// refresh when one is due. Required: without it GET
	// /api/artists/{id}/discography could only answer "unavailable" for ever,
	// which is a broken instance wearing the mask of a working one.
	ArtistAlbums artistDiscographySource
```

Add to the `Server` struct beside `albumTracks`, to the `New` nil-check switch beside the `AlbumTracks` case (`return nil, errors.New("httpapi: artist discography source is required")`), and to the struct literal (`artistAlbums: deps.ArtistAlbums,`).

Add the route in `internal/httpapi/router.go`, immediately after the tracklist route (:104):

```go
	s.route(mux, "GET /api/artists/{id}/discography", s.handleArtistDiscography)
```

Append the handler to `internal/httpapi/entities.go`, after `handleAlbumTracklist`:

```go
// handleArtistDiscography answers GET /api/artists/{id}/discography.
//
// It is a separate route from GET /api/artists/{id} for the two reasons
// handleAlbumTracklist is separate from GET /api/albums/{id}. It is the only
// trigger for the lazy walk, so there is exactly one place that can start one;
// and the client polls it while a walk runs, which must not re-run the artist
// page's whole statistics on every tick.
//
// It never waits for Spotify. Everything below reads the database; the service
// starts a detached walk when one is due and this returns "pending", and the
// client asks again. A page that hangs on a third party is a defect.
func (s *Server) handleArtistDiscography(w http.ResponseWriter, r *http.Request) {
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

	// The artist must already be in the catalogue. Without this, any base-62
	// string in the URL would spend up to twenty of the instance's Spotify
	// requests on somebody nobody has listened to — the same quota argument §5.2
	// uses to reject a background sweep, arriving through a different door, and
	// costing rather more per door than the album endpoint's one request.
	if _, err := s.catalog.GetArtist(ctx, s.querier, id); err != nil {
		writeError(w, r, err)
		return
	}

	// Two independent round trips rather than one transaction, and deliberately
	// so: the discography is global catalogue data with its own TTL and the heard
	// set is one user's history, and there is no snapshot to lose by reading them
	// separately. toArtistDiscography derives Coverage, Missing and Excluded from
	// one (discography, heard) pair in a single pass — every release is counted
	// once and lands in exactly one bucket — so the response cannot disagree with
	// itself for whatever heard happens to be. A listen landing between these two
	// calls is simply included or not.
	d, err := s.artistAlbums.Discography(ctx, s.querier, id)
	if err != nil {
		writeError(w, r, err)
		return
	}
	// Asked with exactly the set the denominator is taken over, so the numerator
	// and the denominator can never be computed over different populations. An
	// artist with nothing counted asks nothing at all — see stats.HeardAlbums.
	heard, err := s.stats.HeardAlbums(ctx, s.querier, user.ID, d.CountedIDs())
	if err != nil {
		writeError(w, r, err)
		return
	}

	writeJSON(w, r, http.StatusOK, toArtistDiscography(d, heard))
}
```

In `cmd/encore-api/main.go`, after the `albumTracks` block (:142-152):

```go
	// The artist page's discography cache, on the same terms as the album track
	// cache above: read when somebody opens that artist's page, and Close cancels
	// anything still in flight at shutdown. Deferred here, after the pool is
	// open, so LIFO runs it before the pool closes — a cancelled walk still needs
	// the pool to record that it failed.
	artistAlbums, err := artistalbums.New(cfg.ArtistAlbums, artistalbums.Deps{
		Catalog: catalogRepo,
		Spotify: client,
		Writer:  artistalbums.StoreWriter{Store: db},
		Logger:  lg,
	})
	if err != nil {
		return err
	}
	defer artistAlbums.Close()
```

and add `ArtistAlbums: artistAlbums,` to the `httpapi.Deps` literal, plus the import.

- [ ] **Step 10: Write the failing e2e tests**

In `test/e2e/e2e_test.go`, add to `spotifyStub` beside `albumTracks` (:62-67):

```go
	// artistAlbums is what GET /v1/artists/{id}/albums answers for one artist.
	// An id no test told it about comes back as an empty page, which is the shape
	// that makes the discography unavailable — see the test that relies on it.
	artistAlbums   map[string][]map[string]any
	artistAlbumReqs atomic.Int64
```

initialise it in the constructor (`artistAlbums: map[string][]map[string]any{},`), register the route beside the album-tracks one (:184):

```go
	mux.HandleFunc("/v1/artists/{id}/albums", func(w http.ResponseWriter, r *http.Request) {
		s.artistAlbumReqs.Add(1)
		writeJSON(w, map[string]any{"items": s.artistAlbums[r.PathValue("id")], "next": nil})
	})
```

and add the counter beside `albumTrackCalls` (:198):

```go
// artistAlbumCalls reports how many times GET /v1/artists/{id}/albums has been
// served, which is how the tests below prove a walk happened exactly once.
func (s *spotifyStub) artistAlbumCalls() int { return int(s.artistAlbumReqs.Load()) }
```

Construct the service in `newInstanceWith` beside `albumTracks` (:277-293) and pass `ArtistAlbums: artistAlbums` to `httpapi.Deps`, with `t.Cleanup(artistAlbums.Close)`.

Then append the tests:

```go
// seedArtistWithPlays gives one artist a Spotify discography of `albums`
// album-group releases plus `singles` singles, and plays one track from the
// first `played` albums, so the discography endpoint has a real numerator and a
// real denominator to work with.
func (i *instance) seedArtistWithPlays(b *browser, artistID string, albums, singles, played int) {
	i.t.Helper()
	if played > albums {
		i.t.Fatalf("seedArtistWithPlays: played (%d) exceeds albums (%d)", played, albums)
	}
	// One play per album puts the album (and the artist) in the catalogue.
	plays := make([]map[string]any, 0, played)
	at := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	listing := make([]map[string]any, 0, albums+singles)
	for n := range albums {
		albumID := fmt.Sprintf("%salb%02d", artistID[:14], n)
		listing = append(listing, map[string]any{
			"id": albumID, "name": fmt.Sprintf("Album %d", n), "album_group": "album",
			"release_date": fmt.Sprintf("%d", 2010+n), "release_date_precision": "year",
		})
		if n < played {
			plays = append(plays, artistPlayItem(
				fmt.Sprintf("%strk%02d", artistID[:14], n), albumID, artistID, at.Add(-time.Duration(n)*time.Minute)))
		}
	}
	for n := range singles {
		listing = append(listing, map[string]any{
			"id": fmt.Sprintf("%ssng%02d", artistID[:14], n), "name": fmt.Sprintf("Single %d", n),
			"album_group": "single", "release_date": "2021", "release_date_precision": "year",
		})
	}
	i.stub.artistAlbums[artistID] = listing
	i.stub.plays = plays
	res := decode[map[string]any](i.t, b.postJSON("/api/sync/now", nil), http.StatusOK)
	if n, _ := res["imported"].(float64); int(n) != played {
		i.t.Fatalf("sync reported %v imported while seeding, want %d", res["imported"], played)
	}
}

// TestArtistDiscographyFillsInWithoutBlockingThePage walks the whole feature:
// the first request answers immediately without a discography, the walk lands
// behind it, and a later request names the albums that were never played.
func TestArtistDiscographyFillsInWithoutBlockingThePage(t *testing.T) {
	inst := newInstance(t)
	b := inst.browser()
	inst.signIn(b)
	const artistID = "e2ediscofillsin000001"
	// Eleven albums, four played, and forty singles nobody counts.
	inst.seedArtistWithPlays(b, artistID, 11, 40, 4)

	start := time.Now()
	first := decode[httpapi.ArtistDiscography](t,
		b.get("/api/artists/"+artistID+"/discography"), http.StatusOK)
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("the first request took %s; it waited for Spotify", elapsed)
	}
	if first.State != "pending" {
		t.Fatalf("first state = %q, want \"pending\"", first.State)
	}
	if len(first.Missing) != 0 {
		t.Fatalf("first response named %d missing albums before any walk finished", len(first.Missing))
	}

	var got httpapi.ArtistDiscography
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		got = decode[httpapi.ArtistDiscography](t,
			b.get("/api/artists/"+artistID+"/discography"), http.StatusOK)
		if got.State == "ready" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if got.State != "ready" {
		t.Fatalf("state never became ready; last was %q", got.State)
	}
	// The sentence §5.2 asks for, end to end.
	if got.Coverage.Total != 11 || got.Coverage.Covered != 4 {
		t.Fatalf("coverage = %d/%d, want 4/11: singles must not enter the denominator",
			got.Coverage.Covered, got.Coverage.Total)
	}
	if len(got.Missing) != 7 {
		t.Fatalf("named %d missing albums, want 7", len(got.Missing))
	}
	if got.Excluded.Singles != 40 {
		t.Fatalf("excluded.singles = %d, want 40: the page cannot say what it set aside without this",
			got.Excluded.Singles)
	}
	if got.FetchedAt == nil {
		t.Fatal("fetchedAt is absent on a ready discography; the page cannot say how old it is")
	}
	if n := inst.stub.artistAlbumCalls(); n != 1 {
		t.Fatalf("the stub served %d discography requests, want 1: the poll refetched", n)
	}
}

// TestArtistDiscographyRefusesAnArtistNobodyHasPlayed keeps an arbitrary id in
// the URL from spending up to twenty Spotify requests.
func TestArtistDiscographyRefusesAnArtistNobodyHasPlayed(t *testing.T) {
	inst := newInstance(t)
	b := inst.browser()
	inst.signIn(b)

	resp := b.get("/api/artists/1BBBBBBBBBBBBBBBBBBBBB/discography")
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for an artist not in the catalogue", resp.StatusCode)
	}
	if n := inst.stub.artistAlbumCalls(); n != 0 {
		t.Fatalf("the stub served %d requests for an unknown artist, want 0", n)
	}
}

// TestArtistDiscographyDisabledAnswersWithoutTouchingSpotify walks the
// operator's switch end to end, which is the only place the configuration, the
// service and the handler are proved to agree about it — and the only place the
// two switches are proved independent at runtime rather than in config parsing.
func TestArtistDiscographyDisabledAnswersWithoutTouchingSpotify(t *testing.T) {
	inst := newInstanceWith(t, map[string]string{"ENCORE_ARTIST_ALBUMS_ENABLED": "false"})
	b := inst.browser()
	inst.signIn(b)
	const artistID = "e2ediscodisabled00001"
	inst.seedArtistWithPlays(b, artistID, 11, 0, 4)

	got := decode[httpapi.ArtistDiscography](t,
		b.get("/api/artists/"+artistID+"/discography"), http.StatusOK)
	if got.State != "disabled" {
		t.Fatalf("state = %q, want \"disabled\"", got.State)
	}
	if n := inst.stub.artistAlbumCalls(); n != 0 {
		t.Fatalf("the stub served %d discography requests on a disabled instance, want 0", n)
	}
	var rows int
	if err := inst.env.Pool.QueryRow(inst.env.Ctx(),
		`SELECT count(*)::int FROM artist_album_fetches`).Scan(&rows); err != nil {
		t.Fatalf("count: %v", err)
	}
	if rows != 0 {
		t.Fatalf("%d artist_album_fetches rows were written on a disabled instance, want 0", rows)
	}
	// And the album endpoint is untouched by the artist switch, which is the
	// whole point of two keys.
	const albumID = "e2ediscodisabled0alb0"
	inst.seedAlbumWithPlays(b, albumID, 12, 9)
	tl := decode[httpapi.AlbumTrackList](t, b.get("/api/albums/"+albumID+"/tracklist"), http.StatusOK)
	if tl.State == "disabled" {
		t.Fatal("turning off discographies also turned off album track listings; the two keys exist " +
			"precisely so an operator can keep the cheap one")
	}
}

// TestArtistDiscographyAllSinglesIsReadyNotAFailure is the state with no
// counterpart on the album endpoint, walked end to end because it is the one
// place a guard on the filtered set — rather than on the whole response — would
// show up as a user-visible lie.
func TestArtistDiscographyAllSinglesIsReadyNotAFailure(t *testing.T) {
	inst := newInstance(t)
	b := inst.browser()
	inst.signIn(b)
	const artistID = "e2ediscoallsingles001"
	// One album so the artist reaches the catalogue at all, then the stub's
	// listing is replaced with singles only: the artist Spotify knows has
	// released nothing it calls an album.
	inst.seedArtistWithPlays(b, artistID, 1, 0, 1)
	inst.stub.artistAlbums[artistID] = []map[string]any{
		{"id": "e2ediscoallsingsng01", "name": "One", "album_group": "single",
			"release_date": "2021", "release_date_precision": "year"},
		{"id": "e2ediscoallsingsng02", "name": "Two", "album_group": "single",
			"release_date": "2022", "release_date_precision": "year"},
	}

	var got httpapi.ArtistDiscography
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		got = decode[httpapi.ArtistDiscography](t,
			b.get("/api/artists/"+artistID+"/discography"), http.StatusOK)
		if got.State == "ready" || got.State == "unavailable" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if got.State != "ready" {
		t.Fatalf("state = %q, want \"ready\": an artist who has only released singles was read "+
			"successfully, and calling that a failure tells them Spotify would not answer about "+
			"somebody Spotify answered about at length", got.State)
	}
	if got.Coverage.Total != 0 || len(got.Missing) != 0 {
		t.Fatalf("coverage = %+v with %d missing, want nothing counted", got.Coverage, len(got.Missing))
	}
	if got.Excluded.Singles != 2 {
		t.Fatalf("excluded.singles = %d, want 2", got.Excluded.Singles)
	}
}

// TestArtistDiscographyUnavailableIsA200NotAnErrorEnvelope proves the one state
// whose whole contract is "stop polling" arrives as an ordinary 200 carrying
// {"state":"unavailable",...} rather than writeError's envelope. Leaving the
// artist unseeded in stub.artistAlbums is what produces it: the stub answers
// {"items":null} for any id nobody told it about, which the service records as a
// failure because there is no such artist as one who has released nothing.
func TestArtistDiscographyUnavailableIsA200NotAnErrorEnvelope(t *testing.T) {
	inst := newInstance(t)
	b := inst.browser()
	inst.signIn(b)
	const artistID = "e2ediscounavail000001"
	inst.seedArtistWithPlays(b, artistID, 1, 0, 1)
	delete(inst.stub.artistAlbums, artistID)

	var got httpapi.ArtistDiscography
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		resp := b.get("/api/artists/" + artistID + "/discography")
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("discography answered %d while the walk was resolving, want 200 throughout every "+
				"state; body: %s", resp.StatusCode, body)
		}
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatalf("decode discography: %v; body: %s", err, body)
		}
		if got.State == "unavailable" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if got.State != "unavailable" {
		t.Fatalf("state never became unavailable; last was %q", got.State)
	}
	if got.Coverage.Total != 0 || len(got.Missing) != 0 {
		t.Fatalf("coverage = %+v with %d missing, want nothing with no discography ever stored",
			got.Coverage, len(got.Missing))
	}
	if n := inst.stub.artistAlbumCalls(); n != 1 {
		t.Fatalf("the stub served %d discography requests, want exactly 1: a recorded failure must "+
			"not be retried inside its fifteen-minute backoff", n)
	}
}
```

> `artistPlayItem(trackID, albumID, artistID string, at time.Time)` is the recently-played item builder. `samePlayItem` at `test/e2e/e2e_test.go` already builds one without a named artist — read it and add the artist-carrying variant beside it rather than duplicating the whole literal.

**Fails when:**
- `…FillsInWithoutBlockingThePage` — the handler waits for Spotify; singles enter the denominator; `excluded` is dropped; the poll refetches inside the TTL.
- `…RefusesAnArtistNobodyHasPlayed` — the `GetArtist` guard is removed, and an arbitrary URL becomes a twenty-request quota spend.
- `…DisabledAnswersWithoutTouchingSpotify` — the switch stops being consulted, a lease is claimed on a disabled instance, or the two features are wired to one key.
- `…AllSinglesIsReadyNotAFailure` — the emptiness guard is moved onto the filtered set.
- `…UnavailableIsA200…` — `unavailable` starts being served as an error envelope, or the failure backoff is dropped.

- [ ] **Step 11: Run everything**

```bash
go test -count=1 ./internal/stats/ ./internal/httpapi/
go build ./...
go test -tags=integration -count=1 ./test/e2e/ -run TestArtistDiscography
```
Expected: PASS for all three. Run the e2e package on its own; the tagged suites share one database.

- [ ] **Step 12: Document the endpoint**

Add a section to `docs/api.md` immediately after "Which tracks you have never played" (which ends around :245). Mirror that section's structure: prose, a JSON example, the four-state table, and the caveats.

```markdown
### How much of an artist you have heard

`GET /api/artists/{id}/discography`

Album completion counts the tracks on one record. This counts the records: "you have heard 4 of this
artist's 11 albums". Nothing Encore stores can answer it — `albums` holds only records somebody
played, so counting rows there would answer with the numerator — so it needs Spotify's own list of
what the artist released.

That list is read **the first time somebody opens the artist's page** and then cached for
`ENCORE_ARTIST_ALBUMS_TTL` (7 days by default; shorter than the album track listing's 30 because a
discography grows). There is no background sweep, for the reason §5.2 gives: most artists in a large
history are never opened.

It is the second Spotify request `encore-api` makes that nobody clicked for, so an operator can
switch it off with `ENCORE_ARTIST_ALBUMS_ENABLED=false` — a **separate** switch from
`ENCORE_ALBUM_TRACKS_ENABLED`, because a discography walk costs up to seven requests against roughly
one. **A rate-limit response to either pauses Spotify access instance-wide** for the window Spotify
asks for, which 409s "sync now" for every user until it lifts.

**This endpoint never waits for Spotify.** It answers from the database and starts the walk behind
it, so `state` says which of four situations you are in — the same four words, with the same
meanings, as the album tracklist endpoint's:

```json
{
  "state": "ready",
  "coverage": { "covered": 4, "total": 11 },
  "missing": [ { "id": "4uLU…", "name": "…", "releaseDate": "2022", "releasePrecision": "year" } ],
  "excluded": { "singles": 40, "compilations": 3, "appearsOn": 7, "other": 0 },
  "fetchedAt": "2026-07-20T09:00:00Z"
}
```

| `state` | Means | What a client must render |
|---|---|---|
| `ready` | A discography is stored. `coverage`, `missing` and `excluded` are meaningful. | The list, or "you have played something from every album" when `missing` is empty, or "Spotify lists no albums for this artist" when `coverage.total` is 0. |
| `pending` | No discography yet; one is being read from Spotify now, or is due and about to be. | "Encore is asking Spotify." Poll, with a cap. |
| `unavailable` | No discography, and none is being read: the last attempt failed. | "Encore could not read it." Never "you have played everything." |
| `disabled` | No discography, and this instance does not fetch them: `ENCORE_ARTIST_ALBUMS_ENABLED=false`. | "This instance does not fetch discographies." Never blame Spotify, and never promise a retry. |

**`coverage` counts `album_group = "album"` and nothing else.** Singles, compilations and appearances
are excluded, because "you have heard 4 of 340 releases" is not a useful sentence. That makes
`coverage` alone an overclaim by omission, which is why `excluded` travels with it: **a client that
renders the coverage without also naming what was set aside is making a claim this payload does not
support.** `other` is any group Spotify sends that is none of the four it documents; it is zero
today and exists so `coverage.total` plus the four buckets always account for every release stored.

**`covered` counts albums with *any* play, not albums played in full.** One track off a record puts
it in `covered`. A client must say so, or "4 of 11 albums" reads as four albums heard end to end. An
album the caller played that Spotify does not list under this artist is in neither number.

**`ready` with `coverage.total == 0` is a real answer.** An artist whose every release is a single
has nothing to count and `excluded` is the only thing that describes them. This has no counterpart on
the album endpoint, where an empty listing is impossible and is recorded as a failure — the emptiness
rule here applies to the whole response, never to the filtered subset.

**`missing` entries are not catalogue entities.** Most of them are records nobody has played, which
are not in `albums` at all, so a client must not link them to `/albums/{id}`.

**`pending` has no server-side bound**, exactly as on the album tracklist endpoint: a claim against
`artist_album_fetches` that itself errors leaves the row as it was, so the next request lands back in
the same branch. Encore's own web client caps its poll at three minutes and then says plainly that it
gave up.

**Turning fetching off does not hide a discography that is already cached.** One stored before the
switch was flipped still arrives as `ready`, past its TTL or not; `fetchedAt` is what keeps that
honest.
```

- [ ] **Step 13: Vet, staticcheck, NUL-check and commit**

```bash
export PATH="$PATH:$(go env GOPATH)/bin"
go vet ./... && staticcheck ./internal/... ./cmd/...
perl -0777 -ne 'print "NULs: ", tr/\0//, "\n"' internal/stats/discography.go
git add internal/stats/discography.go internal/stats/stats_test.go internal/httpapi/dto.go \
        internal/httpapi/dto_test.go internal/httpapi/entities.go internal/httpapi/router.go \
        internal/httpapi/server.go cmd/encore-api/main.go test/e2e/e2e_test.go docs/api.md
git commit -m "$(cat <<'EOF'
API: serve how much of an artist's catalogue you have heard

GET /api/artists/{id}/discography answers "you have heard 4 of this artist's 11
albums" from the database and starts the Spotify walk behind it, so the page
never waits on a third party. The artist must already be in the catalogue: an
arbitrary id in the URL would otherwise spend up to twenty requests on somebody
nobody listened to.

Coverage counts album_group 'album' and nothing else, which makes it an
overclaim on its own — an artist with 340 singles looks like an artist with 11
releases. So the response carries what it set aside, including a bucket for any
group Spotify adds later, and the four buckets plus the counted albums always
account for every release stored.

The heard set is asked for with exactly the ids the denominator is taken over,
so the two can never be computed over different populations.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 7: The shared poll mechanism, and the client types

This is the one task that touches a 2e-i file, and it must leave
`web/src/test/album-tracklist.test.tsx` **byte-for-byte unchanged and green**. That test is the
safety net; if it needs editing, the extraction is wrong and should be abandoned in favour of a
second copy.

**Files:**
- Create: `web/src/lib/fetchpoll.ts`
- Create: `web/src/lib/fetchpoll.test.ts`
- Modify: `web/src/pages/AlbumDetail.tsx:40-150` (delete the mechanism, re-export the two names its test imports)
- Modify: `web/src/lib/types.ts` (append after `AlbumTrackList`)
- Modify: `web/src/lib/query.ts:132` region

**Interfaces:**
- Produces, from `web/src/lib/fetchpoll.ts`:
  - `pollStartKey(prefix: string, id: string): string`
  - `pollStartedAt(key: string, now: number, windowMs: number): number`
  - `clearPollStart(key: string): void`
  - `lazyPollInterval(state: LazyFetchState | undefined, gaveUp: boolean, everyMs: number): number | false`
  - `type LazyFetchState = 'ready' | 'pending' | 'unavailable' | 'disabled'`
- Produces, from `web/src/lib/types.ts`: `ArtistDiscography`, `DiscographyAlbumRef`, `DiscographyExcluded`; `AlbumTrackListState` becomes an alias of `LazyFetchState`.
- Produces, from `web/src/lib/query.ts`: `qk.artistDiscography(id: string)`.

- [ ] **Step 1: Write the failing tests for the extracted module**

Create `web/src/lib/fetchpoll.test.ts`:

```ts
/**
 * The mechanism behind both lazy Spotify panels' polls.
 *
 * Extracted from the album page rather than copied to the artist page: it is
 * ninety lines of pure mechanism with no copy in it, both panels need every line
 * of it, and a second copy is a second place for the cap to be got wrong. The
 * copy — which is where 2e-i's six defects were — stays on the pages, where it
 * differs entirely.
 */

import { afterEach, beforeEach, describe, expect, it } from 'vitest'
import { clearPollStart, lazyPollInterval, pollStartedAt, pollStartKey } from './fetchpoll'

const WINDOW = 240_000

beforeEach(() => {
  window.sessionStorage.clear()
})

afterEach(() => {
  window.sessionStorage.clear()
})

describe('lazyPollInterval', () => {
  it('polls only while a fetch is running, and not once it has given up', () => {
    expect(lazyPollInterval('pending', false, 2000)).toBe(2000)
    expect(lazyPollInterval('pending', false, 3000)).toBe(3000)
    // The cap. Nothing on the server ends "pending", so this has to.
    expect(lazyPollInterval('pending', true, 2000)).toBe(false)
    expect(lazyPollInterval('ready', false, 2000)).toBe(false)
    expect(lazyPollInterval('unavailable', false, 2000)).toBe(false)
    // "disabled" never polls at all: there is no fetch to wait for.
    expect(lazyPollInterval('disabled', false, 2000)).toBe(false)
    expect(lazyPollInterval(undefined, false, 2000)).toBe(false)
  })
})

describe('pollStartedAt', () => {
  it('records the first instant it is asked, and returns it again afterwards', () => {
    const key = pollStartKey('encore.test.', 'thing-1')
    expect(pollStartedAt(key, 1_000_000, WINDOW)).toBe(1_000_000)
    // The second call is a re-render, or a reload: the window must not restart,
    // or a page reloaded just short of the cap never reaches it.
    expect(pollStartedAt(key, 1_050_000, WINDOW)).toBe(1_000_000)
  })

  it('opens a fresh window when the recorded start belongs to an earlier visit', () => {
    const key = pollStartKey('encore.test.', 'thing-1')
    window.sessionStorage.setItem(key, String(1_000_000))
    // Beyond the window: coming back later must mean a real attempt, not a panel
    // that reports having given up before it asked anything.
    expect(pollStartedAt(key, 1_000_000 + WINDOW + 1, WINDOW)).toBe(1_000_000 + WINDOW + 1)
  })

  it('opens a fresh window when the recorded start is in the future', () => {
    const key = pollStartKey('encore.test.', 'thing-1')
    window.sessionStorage.setItem(key, String(2_000_000))
    // A clock that moved under us. Trusting it would grant an unbounded window.
    expect(pollStartedAt(key, 1_000_000, WINDOW)).toBe(1_000_000)
  })

  it('ignores an unparseable start rather than treating it as zero', () => {
    const key = pollStartKey('encore.test.', 'thing-1')
    window.sessionStorage.setItem(key, 'not a number')
    // Number('not a number') is NaN; a Number.isFinite check is what stops it
    // becoming a start of 0, which is older than any window and would restart
    // the cap on every render — the same unbounded poll, arriving by another
    // door.
    expect(pollStartedAt(key, 1_000_000, WINDOW)).toBe(1_000_000)
    expect(window.sessionStorage.getItem(key)).toBe('1000000')
  })

  it('keys separate entities separately', () => {
    const a = pollStartKey('encore.test.', 'thing-1')
    const b = pollStartKey('encore.test.', 'thing-2')
    expect(a).not.toBe(b)
    pollStartedAt(a, 1_000_000, WINDOW)
    expect(pollStartedAt(b, 1_500_000, WINDOW)).toBe(1_500_000)
  })

  it('separates the two panels even for the same id', () => {
    // An album and an artist can share neither an id nor a stuck fetch, but the
    // prefix is what guarantees it rather than the id space happening to differ.
    const album = pollStartKey('encore.tracklist-poll-start.', 'x')
    const artist = pollStartKey('encore.discography-poll-start.', 'x')
    expect(album).not.toBe(artist)
  })
})

describe('clearPollStart', () => {
  it('forgets the window so a later pending state starts fresh', () => {
    const key = pollStartKey('encore.test.', 'thing-1')
    pollStartedAt(key, 1_000_000, WINDOW)
    clearPollStart(key)
    expect(window.sessionStorage.getItem(key)).toBeNull()
    expect(pollStartedAt(key, 1_100_000, WINDOW)).toBe(1_100_000)
  })
})
```

**Fails when:**
- `lazyPollInterval` — the `gaveUp` argument stops being consulted (the poll then never ends), `'disabled'` starts polling, or the interval is hard-coded rather than taken from the argument.
- `pollStartedAt` "records the first instant" — the write to `sessionStorage` is dropped, so every render restarts the window and the cap never arrives.
- `pollStartedAt` "earlier visit" — the window check is dropped, so a stuck panel from twenty minutes ago never asks again.
- `pollStartedAt` "in the future" — the `stored <= now` check is dropped.
- `pollStartedAt` "unparseable" — the `Number.isFinite`/`> 0` guard is dropped, and a corrupt key becomes a start of 0.
- `pollStartKey` "separates the two panels" — the prefix argument is dropped and one key space is shared.
- `clearPollStart` — the removal is dropped, so an album that resolves keeps its old window and a later `pending` inherits a spent one.

- [ ] **Step 2: Run them to verify they fail**

Run: `cd web && npx vitest run src/lib/fetchpoll.test.ts`
Expected: FAIL — cannot resolve `./fetchpoll`.

- [ ] **Step 3: Write the module**

Create `web/src/lib/fetchpoll.ts`:

```ts
/**
 * The polling mechanism shared by Encore's two lazy Spotify panels: the album
 * page's never-played track list and the artist page's discography.
 *
 * Both endpoints answer immediately and fill in behind the request, so both
 * pages poll. Both have to stop, and for a reason that is not obvious from the
 * client: `pending` is unbounded server-side. Nothing in either payload says how
 * long it has held, and a claim against the fetch table that errors records
 * nothing, so the very next request lands back in `pending` for ever. Without a
 * cap, a tab left open asks the API every couple of seconds until somebody
 * closes it.
 *
 * Only the mechanism lives here. Every interval, every cap and every word of
 * copy stays on the page that renders it: the two panels wait different lengths
 * of time, because an album's walk is one request and an artist's is up to
 * twenty, and they say entirely different things when they give up.
 */

/**
 * The four states both endpoints share. Pinned identical server-side by
 * `TestTheLazyFetchStatesKeepTheirWireValues`, and share one Go definition in
 * `internal/lazyfetch`.
 */
export type LazyFetchState = 'ready' | 'pending' | 'unavailable' | 'disabled'

/**
 * The next poll delay, or `false` to stop.
 *
 * `gaveUp` is the caller's cap having passed. Everything other than a running
 * fetch stops immediately, and `disabled` never polls at all because there is no
 * fetch to wait for.
 */
export function lazyPollInterval(
  state: LazyFetchState | undefined,
  gaveUp: boolean,
  everyMs: number,
): number | false {
  return state === 'pending' && !gaveUp ? everyMs : false
}

/** The `sessionStorage` key one panel uses for one entity. */
export function pollStartKey(prefix: string, id: string): string {
  return prefix + id
}

/**
 * When this tab first saw `pending` for this entity, in epoch milliseconds,
 * recording `now` the first time it is asked.
 *
 * In `sessionStorage` rather than component state on purpose: an in-memory clock
 * restarts with the component, so somebody who reloads a stuck page a few seconds
 * before the cap gets a fresh window every time and the cap never arrives.
 * Anything unreadable — private browsing, storage disabled — falls back to now,
 * which still caps the poll, just per page load.
 */
export function pollStartedAt(key: string, now: number, windowMs: number): number {
  try {
    const stored = Number(window.sessionStorage.getItem(key))
    // A start in the future is a clock that moved under us, and one older than
    // the window belongs to an earlier visit; both open a fresh window rather
    // than granting an unbounded one or refusing to try again for ever. The
    // finiteness check matters as much as the rest: Number(null) is 0 and
    // Number('x') is NaN, and treating either as a real start would either
    // restart the window on every render or report having given up before
    // anything was asked.
    if (Number.isFinite(stored) && stored > 0 && stored <= now && now - stored < windowMs) {
      return stored
    }
    window.sessionStorage.setItem(key, String(now))
  } catch {
    // See above: a per-load cap is a much smaller problem than no cap.
  }
  return now
}

/** Forgets a recorded window, so the next `pending` gets a full one. */
export function clearPollStart(key: string): void {
  try {
    window.sessionStorage.removeItem(key)
  } catch {
    // Nothing was stored, so there is nothing to clear.
  }
}
```

- [ ] **Step 4: Move the album page onto it, keeping its two exported names**

In `web/src/pages/AlbumDetail.tsx`, delete the local `pollStartedAt` and `clearPollStart` (:122-150) and the `TRACKLIST_POLL_WINDOW_MS` constant, and replace the exported surface with re-exports. **`TRACKLIST_POLL_START_KEY` and `tracklistPollInterval` must keep their exact names, signatures and defaults**, because `web/src/test/album-tracklist.test.tsx:25` imports both and that file does not change.

Keep `TRACKLIST_POLL_MS`, `TRACKLIST_POLL_CAP_MS` and `TRACKLIST_POLL_CAP_LABEL` and their comments exactly as they are — they are this page's numbers, not shared ones. Then:

```ts
import { clearPollStart, lazyPollInterval, pollStartKey, pollStartedAt } from '../lib/fetchpoll'

/**
 * How old a recorded poll start may be before it is treated as a different
 * visit rather than a continuation of this one. Twice the cap: comfortably
 * longer than any reload-and-read cycle near the cap, and short enough that
 * coming back later means a real attempt.
 */
const TRACKLIST_POLL_WINDOW_MS = 2 * TRACKLIST_POLL_CAP_MS

/**
 * Key prefix for when this tab first saw `pending` for an album.
 *
 * Exported for the test that reloads a page near the cap.
 */
export const TRACKLIST_POLL_START_KEY = 'encore.tracklist-poll-start.'

/**
 * The next poll delay, or `false` to stop.
 *
 * Exported so it can be tested without driving a real timer through TanStack
 * Query. The mechanism is in ../lib/fetchpoll, shared with the artist page's
 * discography panel; this is the album page's interval applied to it.
 */
export function tracklistPollInterval(
  state: AlbumTrackListState | undefined,
  gaveUp = false,
): number | false {
  return lazyPollInterval(state, gaveUp, TRACKLIST_POLL_MS)
}
```

and in `NeverPlayedPanel`'s effect (:434-447), replace the two local calls:

```ts
    const key = pollStartKey(TRACKLIST_POLL_START_KEY, albumId)
    if (state !== 'pending') {
      if (state) clearPollStart(key)
      return
    }
    const remaining = pollStartedAt(key, Date.now(), TRACKLIST_POLL_WINDOW_MS) +
      TRACKLIST_POLL_CAP_MS - Date.now()
```

Keep every surrounding comment. **Change no copy, no title, no description, no timing constant.**

- [ ] **Step 5: Prove the album page is unchanged in behaviour**

```bash
cd web && git diff --stat src/test/album-tracklist.test.tsx
npx vitest run src/test/album-tracklist.test.ts src/test/album-tracklist.test.tsx src/lib/fetchpoll.test.ts
```
Expected: `git diff --stat` prints **nothing** for the test file, and every test passes.

**Fails when:** the extraction changes the album panel's behaviour in any way. This is the whole gate on Task 7 — a green suite with an edited test file is not a pass.

- [ ] **Step 6: Add the client types and the query key**

In `web/src/lib/types.ts`, change the album state alias and append the new types after `AlbumTrackList`:

```ts
export type AlbumTrackListState = LazyFetchState
```

with `import type { LazyFetchState } from './fetchpoll'` at the top — the four words now have one definition, matching the server, where the same property is pinned by a test.

```ts
export interface DiscographyAlbumRef {
  id: string
  name: string
  /** "2016", "2016-05" or "2016-05-20", at whatever precision Spotify knew. */
  releaseDate: string | null
  releasePrecision: string
}

/**
 * What discography coverage did *not* count.
 *
 * Coverage counts `album_group = "album"` only, because "you have heard 4 of 340
 * releases" is not a useful sentence. That makes coverage alone an overclaim by
 * omission, so **a panel that renders the coverage must also name these**: an
 * artist with 340 singles otherwise looks like an artist with 11 releases.
 *
 * `other` is any group Spotify sends that is none of the four it documents. It
 * is zero today and exists so the four buckets plus `coverage.total` always
 * account for every release stored, rather than a new group vanishing from both
 * the count and the sentence describing the rest.
 */
export interface DiscographyExcluded {
  singles: number
  compilations: number
  appearsOn: number
  other: number
}

/**
 * How much of an artist's own catalogue has been played.
 *
 * Not computed from data on disk: Encore has to ask Spotify what the artist
 * released, which it does the first time somebody opens the page and then
 * caches. So `missing` being empty is ambiguous, and `state` resolves it — an
 * empty `missing` under anything but `ready` means "Encore does not know", never
 * "you have played everything".
 *
 * `disabled` is the operator having turned fetching off
 * (`ENCORE_ARTIST_ALBUMS_ENABLED=false`), separate from `unavailable` for the
 * reason it is on the album tracklist: one is a local choice and the other is
 * Spotify failing to answer.
 *
 * Two things have no counterpart on the album page. `ready` with
 * `coverage.total === 0` is a real answer — an artist whose every release is a
 * single — and `excluded` is then the only thing that describes them. And
 * `covered` counts albums with *any* play, not albums played in full, so a panel
 * must say so or "4 of 11 albums" reads as four albums heard end to end.
 *
 * `pending` carries no clock and nothing bounds it, exactly as on the album
 * tracklist; a client that polls it must cap itself.
 */
export interface ArtistDiscography {
  state: LazyFetchState
  /** Albums with any play, over albums Spotify lists in the `album` group. */
  coverage: Coverage
  /** The counted albums with no play, newest release first. Never null. */
  missing: DiscographyAlbumRef[]
  excluded: DiscographyExcluded
  /** When the discography was read. Absent until one has succeeded. */
  fetchedAt?: Timestamp
}
```

In `web/src/lib/query.ts`, after `albumTracklist` (:132):

```ts
  // Deliberately not keyed by range, exactly like albumTracklist above: the
  // discography and "have you ever played this album" are both all-time.
  artistDiscography: (id: string) => ['entity', 'artist', id, 'discography'] as const,
```

- [ ] **Step 7: Typecheck, lint and commit**

```bash
cd web && npx tsc --noEmit && npm run lint
npx vitest run
cd .. && perl -0777 -ne 'print "NULs: ", tr/\0//, "\n"' web/src/lib/fetchpoll.ts
git add web/src/lib/fetchpoll.ts web/src/lib/fetchpoll.test.ts web/src/pages/AlbumDetail.tsx \
        web/src/lib/types.ts web/src/lib/query.ts
git commit -m "$(cat <<'EOF'
Web: one poll cap for both lazy Spotify panels

The artist page needs exactly the cap the album page already has, and for
exactly the same reason: "pending" is unbounded server-side, so a claim that
errors leaves a tab asking the API every two seconds until somebody closes it.
A second copy of ninety lines of sessionStorage window logic is a second place
for that to be got wrong.

Only the mechanism moves. Every interval, every cap and every word of copy stays
on the page that renders it — the two panels wait different lengths of time,
because an album's walk is one request and an artist's is up to twenty, and they
say entirely different things when they give up.

web/src/test/album-tracklist.test.tsx is unchanged, which is the gate: the album
panel's exported names, behaviour and copy are all exactly as they were.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 8: The discography panel

**Files:**
- Modify: `web/src/pages/ArtistDetail.tsx`
- Create: `web/src/test/artist-discography.test.tsx`
- Modify: `docs/feature-parity.md:88` region

**Interfaces:**
- Consumes: `qk.artistDiscography`, `ArtistDiscography`/`DiscographyAlbumRef`/`DiscographyExcluded` from `../lib/types`, `lazyPollInterval`/`pollStartKey`/`pollStartedAt`/`clearPollStart` from `../lib/fetchpoll`, `formatCount`/`formatPlural`/`formatDate` from `../lib/format`, `formatRelease` from `./top/TopList`, `EmptyState`/`Panel`/`SkeletonText` from `../components/ui`.
- Produces: `DISCOGRAPHY_POLL_START_KEY`, `discographyPollInterval(state, gaveUp?)` — exported for the tests, mirroring the album page.

### The copy, in full

Every string below is asserted verbatim by a test. Nothing in this project has ever been opened in a
browser, so the tests are the only place the words are read.

**Panel title:** `Albums you have never played`

**Panel description** — sits above *all seven* bodies, so it may assert nothing untrue of any of
them. It carries the exclusion because that is the one thing that must never be lost, and it is
phrased as the panel's rule rather than as a claim about a completed count, which would be false
above the four bodies where nothing has been counted:

- `ready` with something missing: the summary below.
- everything else: `Which of this artist's albums have no plays in your history, all time. Singles, compilations and appearances are not counted.`

**The summary** (`ready`, `missing.length > 0`):

| Case | String |
|---|---|
| `listed === 1` | `The only album Spotify lists for this artist has no plays in your history, all time. Singles, compilations and appearances are not counted.` |
| `missing === 1`, `listed > 1` | `1 of the {listed} albums Spotify lists for this artist has no plays in your history, all time. Singles, compilations and appearances are not counted.` |
| otherwise | `{missing} of the {listed} albums Spotify lists for this artist have no plays in your history, all time. Singles, compilations and appearances are not counted.` |

`has`/`have` bends to `missing`, and `{listed}` comes from `formatPlural(listed, 'album')` so it
reads "2 albums" and never "1 albums". The single-album listing drops the ratio entirely, because
"1 of the 1 album" is not a sentence anybody writes. The count is stated **only when there is
something to count**: with nothing missing it becomes "0 of the 11 albums … have no plays", which is
the same fact the body states below it, phrased as a double negative and said twice.

**The bodies:**

| # | When | Title | Description |
|---|---|---|---|
| D1 | `state === 'disabled'` | `Artist discographies are turned off` | `This instance does not ask Spotify what an artist has released, so Encore cannot say which of their albums you have never played. Every other figure on this page comes from your own history and is unaffected. An administrator can turn this on with ENCORE_ARTIST_ALBUMS_ENABLED.` |
| D2 | request failed, or `state === 'unavailable'` | `This artist's discography could not be read` | `Encore could not get the list of what this artist has released from Spotify, so it cannot say which of their albums you have never played. Every other figure on this page comes from your own history and is unaffected. Encore tries again later.` |
| D3 | gave up on `pending` | `No discography for this artist yet` | `Encore waited three minutes for this artist's discography and has stopped for now; it may still arrive — reopen this page to check. Every other figure on this page comes from your own history and is unaffected.` |
| D4 | own request in flight, no data | *(neutral skeleton)* | screen-reader only: `Loading this artist's discography` |
| D5 | `state === 'pending'` | `Asking Spotify what this artist has released` | `Encore reads it once and keeps it, so this step is skipped on most visits. The list appears here on its own.` |
| D6 | `ready`, `coverage.total === 0` | `Spotify lists no albums for this artist` | `Everything Spotify lists for them is a single, a compilation or an appearance on someone else's record, and this panel counts none of those.` |
| D7 | `ready`, `missing.length === 0` | `You have played something from every album by this artist` | `Spotify lists {formatPlural(listed, 'album')} for this artist.` |
| D8 | `ready`, `missing.length > 0` | *(the list)* | — |

D3 never says "Spotify". Running out of patience is not a refusal, and what actually causes it is
very likely local — a claim that errors logs a warning and persists nothing, which a read-only
replica does all day — so naming Spotify would be the D1 mistake through another door. It also makes
no promise to retry, because this page cannot keep one.

D4 is a neutral skeleton rather than D5's words, for the reason the album page learned the hard way:
before the response lands, an instance with fetching turned off and one still asking Spotify look
identical, and "Asking Spotify" followed a moment later by "discographies are turned off" is two
contradictory claims in sequence on the instance whose operator asked Encore not to talk to Spotify.

D7 says "played something from every album", not "played every album". Coverage counts an album with
*any* play, and the shorter sentence claims eleven records heard end to end.

**The three footnote lines**, rendered beneath the body:

- **Excluded** — on every `ready` render where anything was excluded:
  - `coverage.total > 0`: `Spotify also lists {joined} for this artist, which this panel does not count.`
  - `coverage.total === 0` (D6): `Spotify lists {joined} for this artist.` — no "also", because nothing else was listed, and no "does not count", because the description directly above has just said so.
  - `{joined}` is `formatPlural` per non-zero bucket, omitting zeroes, joined as `a`, `a and b`, `a, b and c`, `a, b, c and d`: `formatPlural(n, 'single')`, `formatPlural(n, 'compilation')`, `formatPlural(n, 'appearance')`, `formatPlural(n, 'other release')`. Nothing is rendered when every bucket is zero.
- **What "played" means** — on every `ready` render with `coverage.total > 0`:
  `An album counts as played when you have played any track from it. Albums you played that Spotify does not list under this artist are not counted here.`
  The second sentence pre-empts the contradiction a reader would otherwise find between this panel and the "Top albums" panel on the same screen, which lists albums by play and is not restricted to the `album` group.
- **The read date** — on every `ready` render: `Discography read from Spotify on {formatDate(fetchedAt, timeZone)}.`

There is **no reconciliation line**. The album page prints both totals when Spotify's listing and
`albums.total_tracks` disagree; here there is no second number, because nothing Encore stores counts
an artist's releases — that absence is the whole premise of the feature. Do not invent one.

- [ ] **Step 1: Write the failing panel tests — the fixtures**

Create `web/src/test/artist-discography.test.tsx`. The `ME` constant, `stubRoutes`, `stubRoutesWithHeldPath` and `mountAt` helpers are identical to `web/src/test/album-tracklist.test.tsx:27-169` — copy them across; they are test scaffolding, not logic, and the album file's own reasoning for each is worth reading before you do.

```tsx
/**
 * The artist page's discography panel.
 *
 * Its job is the never-played panel's job one level up, plus one the album page
 * never had: saying what it did *not* count. Coverage counts album_group
 * "album", so "4 of 11" over an artist with 340 singles is an overclaim by
 * omission, and these tests pin the sentence that prevents it as hard as they
 * pin the number.
 *
 * Seven silences to keep apart, not four: Encore has not asked yet, asked and
 * failed, does not ask at all, waited too long, asked and you played everything,
 * asked and there are no albums to count, and its own request still in flight.
 */

import type { ReactElement } from 'react'
import { QueryClientProvider } from '@tanstack/react-query'
import { RouterProvider, createMemoryRouter } from 'react-router-dom'
import { act, render, screen, within } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { routes } from '../App'
import { createQueryClient } from '../lib/query'
import type { ArtistDetail, ArtistDiscography, MeResponse } from '../lib/types'
import { DISCOGRAPHY_POLL_START_KEY, discographyPollInterval } from '../pages/ArtistDetail'

// ME, stubRoutes, stubRoutesWithHeldPath and mountAt: copy from
// web/src/test/album-tracklist.test.tsx:27-169 unchanged.

function artistPayload(): ArtistDetail {
  return {
    artist: {
      id: 'artist-1',
      name: 'A Test Artist',
      imageUrl: '',
      genres: ['post-rock'],
      followers: 1000,
      popularity: 40,
    },
    stats: {
      plays: 20,
      msPlayed: 1_000_000,
      firstListenAt: '2026-06-01T00:00:00Z',
      lastListenAt: '2026-06-20T00:00:00Z',
      discoveredAt: '2020-01-01T00:00:00Z',
      lastPlayedAt: '2026-06-20T00:00:00Z',
      timeline: [],
    },
    share: 0.1,
    topTracks: [],
    topAlbums: [],
    hourRepartition: [],
    blacklisted: false,
  }
}

function discography(overrides: Partial<ArtistDiscography> = {}): ArtistDiscography {
  return {
    state: 'ready',
    coverage: { covered: 9, total: 11 },
    missing: [
      { id: 'alb-10', name: 'The Tenth', releaseDate: '2022', releasePrecision: 'year' },
      { id: 'alb-11', name: 'The Eleventh', releaseDate: '2024', releasePrecision: 'year' },
    ],
    excluded: { singles: 40, compilations: 3, appearsOn: 7, other: 0 },
    fetchedAt: '2026-07-20T09:00:00Z',
    ...overrides,
  }
}

/** A discography the server will never resolve, which is the state the cap exists for. */
const PENDING: Partial<ArtistDiscography> = {
  state: 'pending',
  coverage: { covered: 0, total: 0 },
  missing: [],
  excluded: { singles: 0, compilations: 0, appearsOn: 0, other: 0 },
  fetchedAt: undefined,
}

async function panel(settled: string | RegExp): Promise<HTMLElement> {
  const heading = await screen.findByRole('heading', { name: 'Albums you have never played' })
  const section = heading.closest('section')
  if (!section) throw new Error('the heading is not inside a panel')
  await within(section).findByText(settled)
  return section
}

function panelNow(): HTMLElement {
  const heading = screen.getByRole('heading', { name: 'Albums you have never played' })
  const section = heading.closest('section')
  if (!section) throw new Error('the heading is not inside a panel')
  return section
}

function discographyCalls(asked: string[]): number {
  return asked.filter((path) => path === '/api/artists/artist-1/discography').length
}

beforeEach(() => {
  vi.unstubAllGlobals()
  window.sessionStorage.clear()
})

afterEach(() => {
  vi.useRealTimers()
})
```

> If `ArtistDetail`'s payload type has fields not listed above, add them — do not delete fields from the type to make the fixture compile.

- [ ] **Step 2: Write the failing panel tests — the copy**

Append:

```tsx
describe('the discography panel', () => {
  it('names the missing albums and states what it counted', async () => {
    stubRoutes({
      '/api/me': ME,
      '/api/artists/artist-1': artistPayload(),
      '/api/artists/artist-1/discography': discography(),
    })
    render(mountAt('/artists/artist-1'))
    const section = await panel('The Tenth')

    expect(within(section).getByText('The Eleventh')).toBeInTheDocument()
    expect(
      within(section).getByText(
        '2 of the 11 albums Spotify lists for this artist have no plays in your history, all time. ' +
          'Singles, compilations and appearances are not counted.',
      ),
    ).toBeInTheDocument()
  })

  // The whole album_group problem in one assertion. Without this line "2 of 11"
  // describes an artist with fifty releases as an artist with eleven.
  it('names what it set aside, with the right plural in every bucket', async () => {
    stubRoutes({
      '/api/me': ME,
      '/api/artists/artist-1': artistPayload(),
      '/api/artists/artist-1/discography': discography(),
    })
    render(mountAt('/artists/artist-1'))
    const section = await panel('The Tenth')

    expect(
      within(section).getByText(
        'Spotify also lists 40 singles, 3 compilations and 7 appearances for this artist, ' +
          'which this panel does not count.',
      ),
    ).toBeInTheDocument()
  })

  it('says each excluded bucket in the singular when there is one of it', async () => {
    stubRoutes({
      '/api/me': ME,
      '/api/artists/artist-1': artistPayload(),
      '/api/artists/artist-1/discography': discography({
        excluded: { singles: 1, compilations: 1, appearsOn: 1, other: 1 },
      }),
    })
    render(mountAt('/artists/artist-1'))
    const section = await panel('The Tenth')

    expect(
      within(section).getByText(
        'Spotify also lists 1 single, 1 compilation, 1 appearance and 1 other release for this ' +
          'artist, which this panel does not count.',
      ),
    ).toBeInTheDocument()
  })

  it('omits an empty bucket rather than saying "0 compilations"', async () => {
    stubRoutes({
      '/api/me': ME,
      '/api/artists/artist-1': artistPayload(),
      '/api/artists/artist-1/discography': discography({
        excluded: { singles: 4, compilations: 0, appearsOn: 0, other: 0 },
      }),
    })
    render(mountAt('/artists/artist-1'))
    const section = await panel('The Tenth')

    expect(
      within(section).getByText(
        'Spotify also lists 4 singles for this artist, which this panel does not count.',
      ),
    ).toBeInTheDocument()
    expect(within(section).queryByText(/0 compilations|0 appearances/)).not.toBeInTheDocument()
  })

  it('says nothing about exclusions when there were none', async () => {
    stubRoutes({
      '/api/me': ME,
      '/api/artists/artist-1': artistPayload(),
      '/api/artists/artist-1/discography': discography({
        excluded: { singles: 0, compilations: 0, appearsOn: 0, other: 0 },
      }),
    })
    render(mountAt('/artists/artist-1'))
    const section = await panel('The Tenth')

    expect(within(section).queryByText(/Spotify also lists/)).not.toBeInTheDocument()
    // The description's rule still stands: it is what this panel counts, not a
    // claim that something was excluded.
    expect(
      within(section).getByText(/Singles, compilations and appearances are not counted\./),
    ).toBeInTheDocument()
  })

  // "4 of 11 albums" reads as four albums heard end to end. It is not.
  it('says what counts as having played an album', async () => {
    stubRoutes({
      '/api/me': ME,
      '/api/artists/artist-1': artistPayload(),
      '/api/artists/artist-1/discography': discography(),
    })
    render(mountAt('/artists/artist-1'))
    const section = await panel('The Tenth')

    expect(
      within(section).getByText(
        'An album counts as played when you have played any track from it. Albums you played that ' +
          'Spotify does not list under this artist are not counted here.',
      ),
    ).toBeInTheDocument()
  })

  it('agrees with itself when exactly one album is unplayed', async () => {
    stubRoutes({
      '/api/me': ME,
      '/api/artists/artist-1': artistPayload(),
      '/api/artists/artist-1/discography': discography({
        coverage: { covered: 10, total: 11 },
        missing: [{ id: 'alb-11', name: 'The Eleventh', releaseDate: '2024', releasePrecision: 'year' }],
      }),
    })
    render(mountAt('/artists/artist-1'))
    const section = await panel('The Eleventh')

    expect(
      within(section).getByText(
        '1 of the 11 albums Spotify lists for this artist has no plays in your history, all time. ' +
          'Singles, compilations and appearances are not counted.',
      ),
    ).toBeInTheDocument()
    expect(
      within(section).queryByText(/albums Spotify lists for this artist have/),
    ).not.toBeInTheDocument()
  })

  it('does not make a ratio out of a single-album discography', async () => {
    stubRoutes({
      '/api/me': ME,
      '/api/artists/artist-1': artistPayload(),
      '/api/artists/artist-1/discography': discography({
        coverage: { covered: 0, total: 1 },
        missing: [{ id: 'alb-1', name: 'The Only One', releaseDate: '2016', releasePrecision: 'year' }],
      }),
    })
    render(mountAt('/artists/artist-1'))
    const section = await panel('The Only One')

    expect(
      within(section).getByText(
        'The only album Spotify lists for this artist has no plays in your history, all time. ' +
          'Singles, compilations and appearances are not counted.',
      ),
    ).toBeInTheDocument()
    expect(within(section).queryByText(/of the 1 album/)).not.toBeInTheDocument()
  })

  it('says you played something from all of them rather than showing an empty list', async () => {
    stubRoutes({
      '/api/me': ME,
      '/api/artists/artist-1': artistPayload(),
      '/api/artists/artist-1/discography': discography({
        coverage: { covered: 11, total: 11 },
        missing: [],
      }),
    })
    render(mountAt('/artists/artist-1'))
    const section = await panel('You have played something from every album by this artist')

    expect(within(section).getByText('Spotify lists 11 albums for this artist.')).toBeInTheDocument()
    // Not "you have played every album": coverage counts an album with any play,
    // and the shorter sentence claims eleven records heard end to end.
    expect(within(section).queryByText(/played every album/)).not.toBeInTheDocument()
    // The count line is a double negative when the count is zero, and the body
    // already says the same fact the right way round.
    expect(within(section).queryByText(/\d+ of the \d+ albums/)).not.toBeInTheDocument()
  })

  it('counts a single-album discography correctly when it has been played', async () => {
    stubRoutes({
      '/api/me': ME,
      '/api/artists/artist-1': artistPayload(),
      '/api/artists/artist-1/discography': discography({
        coverage: { covered: 1, total: 1 },
        missing: [],
      }),
    })
    render(mountAt('/artists/artist-1'))
    const section = await panel('You have played something from every album by this artist')

    expect(within(section).getByText('Spotify lists 1 album for this artist.')).toBeInTheDocument()
    expect(within(section).queryByText(/1 albums/)).not.toBeInTheDocument()
  })

  // The state with no counterpart on the album page. It must not read as a
  // failure and must not read as "you have played everything".
  it('says there are no albums to count, and what there is instead', async () => {
    stubRoutes({
      '/api/me': ME,
      '/api/artists/artist-1': artistPayload(),
      '/api/artists/artist-1/discography': discography({
        coverage: { covered: 0, total: 0 },
        missing: [],
        excluded: { singles: 12, compilations: 0, appearsOn: 2, other: 0 },
      }),
    })
    render(mountAt('/artists/artist-1'))
    const section = await panel('Spotify lists no albums for this artist')

    expect(
      within(section).getByText(
        'Everything Spotify lists for them is a single, a compilation or an appearance on someone ' +
          "else's record, and this panel counts none of those.",
      ),
    ).toBeInTheDocument()
    // No "also": nothing else was listed for this artist.
    expect(
      within(section).getByText('Spotify lists 12 singles and 2 appearances for this artist.'),
    ).toBeInTheDocument()
    expect(within(section).queryByText(/Spotify also lists/)).not.toBeInTheDocument()
    expect(within(section).queryByText(/played something from every album/)).not.toBeInTheDocument()
    expect(within(section).queryByText(/could not/i)).not.toBeInTheDocument()
    // Nothing was counted, so the sentence about what counting means says nothing.
    expect(within(section).queryByText(/counts as played/)).not.toBeInTheDocument()
  })

  it('says this instance does not fetch discographies, and never blames Spotify', async () => {
    stubRoutes({
      '/api/me': ME,
      '/api/artists/artist-1': artistPayload(),
      '/api/artists/artist-1/discography': discography({ ...PENDING, state: 'disabled' }),
    })
    render(mountAt('/artists/artist-1'))
    const section = await panel('Artist discographies are turned off')

    expect(
      within(section).getByText(
        'This instance does not ask Spotify what an artist has released, so Encore cannot say which ' +
          'of their albums you have never played. Every other figure on this page comes from your ' +
          'own history and is unaffected. An administrator can turn this on with ' +
          'ENCORE_ARTIST_ALBUMS_ENABLED.',
      ),
    ).toBeInTheDocument()
    // An operator's choice is not a Spotify failure, and not a promise to retry.
    expect(within(section).queryByText(/could not/i)).not.toBeInTheDocument()
    expect(within(section).queryByText(/tries again/i)).not.toBeInTheDocument()
    expect(within(section).queryByText(/failed|error/i)).not.toBeInTheDocument()
    expect(within(section).queryByText(/played something from every album/)).not.toBeInTheDocument()
    expect(within(section).queryByText(/Asking Spotify/i)).not.toBeInTheDocument()
    // And it names the right variable. ENCORE_ALBUM_TRACKS_ENABLED is a
    // different switch, and telling an administrator to flip it would leave the
    // panel exactly as it is.
    expect(within(section).queryByText(/ENCORE_ALBUM_TRACKS_ENABLED/)).not.toBeInTheDocument()
  })

  it('says the discography could not be read, and that nothing else is affected', async () => {
    stubRoutes({
      '/api/me': ME,
      '/api/artists/artist-1': artistPayload(),
      '/api/artists/artist-1/discography': discography({ ...PENDING, state: 'unavailable' }),
    })
    render(mountAt('/artists/artist-1'))
    const section = await panel("This artist's discography could not be read")

    expect(
      within(section).getByText(
        'Encore could not get the list of what this artist has released from Spotify, so it cannot ' +
          'say which of their albums you have never played. Every other figure on this page comes ' +
          'from your own history and is unaffected. Encore tries again later.',
      ),
    ).toBeInTheDocument()
    expect(within(section).queryByText(/played something from every album/)).not.toBeInTheDocument()
    expect(within(section).queryByText(/turned off/i)).not.toBeInTheDocument()
  })

  it('says the discography could not be read when the request itself fails', async () => {
    stubRoutes({
      '/api/me': ME,
      '/api/artists/artist-1': artistPayload(),
    })
    render(mountAt('/artists/artist-1'))
    await panel("This artist's discography could not be read")
  })

  it('says it is still asking Spotify, and claims nothing about completeness', async () => {
    // On fake timers, because this is the one answer that looks exactly like the
    // panel's own loading frame. Advancing the clock settles the request, so what
    // is asserted is the server having said "pending" and not merely the request
    // being in flight.
    vi.useFakeTimers()
    stubRoutes({
      '/api/me': ME,
      '/api/artists/artist-1': artistPayload(),
      '/api/artists/artist-1/discography': discography(PENDING),
    })
    render(mountAt('/artists/artist-1'))
    await act(async () => {
      await vi.advanceTimersByTimeAsync(100)
    })
    const section = panelNow()

    expect(
      within(section).getByText('Asking Spotify what this artist has released'),
    ).toBeInTheDocument()
    expect(
      within(section).getByText(
        'Encore reads it once and keeps it, so this step is skipped on most visits. The list ' +
          'appears here on its own.',
      ),
    ).toBeInTheDocument()
    expect(within(section).queryByText(/played something from every album/)).not.toBeInTheDocument()
    expect(within(section).queryByText(/\d+ of the \d+ albums/)).not.toBeInTheDocument()
    expect(within(section).queryByText(/could not/i)).not.toBeInTheDocument()
    // Nothing has been counted, so nothing has been excluded either.
    expect(within(section).queryByText(/Spotify also lists/)).not.toBeInTheDocument()
  })

  it('shows a neutral skeleton while its own request is in flight, never "Asking Spotify" on a disabled instance', async () => {
    // Before the response lands, an instance with fetching turned off and one
    // still asking Spotify look identical. Claiming either is a contradiction
    // waiting one round trip to happen.
    const { resolveHeld } = stubRoutesWithHeldPath(
      { '/api/me': ME, '/api/artists/artist-1': artistPayload() },
      '/api/artists/artist-1/discography',
    )
    render(mountAt('/artists/artist-1'))

    const heading = await screen.findByRole('heading', { name: 'Albums you have never played' })
    const section = heading.closest('section')
    if (!section) throw new Error('the heading is not inside a panel')

    expect(within(section).queryByText(/Asking Spotify/i)).not.toBeInTheDocument()
    expect(within(section).queryByText(/turned off/i)).not.toBeInTheDocument()
    expect(within(section).getByRole('status')).toBeInTheDocument()

    resolveHeld({ ...discography(PENDING), state: 'disabled' })

    await within(section).findByText('Artist discographies are turned off')
    expect(within(section).queryByText(/Asking Spotify/i)).not.toBeInTheDocument()
  })

  it('serves a discography cached before fetching was turned off, with the date it was read', async () => {
    stubRoutes({
      '/api/me': ME,
      '/api/artists/artist-1': artistPayload(),
      '/api/artists/artist-1/discography': discography({ fetchedAt: '2024-03-12T09:00:00Z' }),
    })
    render(mountAt('/artists/artist-1'))
    const section = await panel('The Tenth')

    expect(
      within(section).getByText('Discography read from Spotify on 12 Mar 2024.'),
    ).toBeInTheDocument()
    expect(within(section).queryByText(/up to date|current|just now/i)).not.toBeInTheDocument()
  })

  it('carries the read date on the played-everything and no-albums states too', async () => {
    stubRoutes({
      '/api/me': ME,
      '/api/artists/artist-1': artistPayload(),
      '/api/artists/artist-1/discography': discography({
        coverage: { covered: 11, total: 11 },
        missing: [],
        fetchedAt: '2024-03-12T09:00:00Z',
      }),
    })
    const first = render(mountAt('/artists/artist-1'))
    await panel('Discography read from Spotify on 12 Mar 2024.')
    first.unmount()

    stubRoutes({
      '/api/me': ME,
      '/api/artists/artist-1': artistPayload(),
      '/api/artists/artist-1/discography': discography({
        coverage: { covered: 0, total: 0 },
        missing: [],
        excluded: { singles: 3, compilations: 0, appearsOn: 0, other: 0 },
        fetchedAt: '2024-03-12T09:00:00Z',
      }),
    })
    render(mountAt('/artists/artist-1'))
    await panel('Discography read from Spotify on 12 Mar 2024.')
  })

  it('shows the release year beside each unplayed album and links to none of them', async () => {
    // Most of these are records nobody has played, so they are not in the
    // catalogue and /albums/{id} would 404 on almost all of them.
    stubRoutes({
      '/api/me': ME,
      '/api/artists/artist-1': artistPayload(),
      '/api/artists/artist-1/discography': discography(),
    })
    render(mountAt('/artists/artist-1'))
    const section = await panel('The Tenth')

    expect(within(section).getByText('2022')).toBeInTheDocument()
    expect(within(section).getByText('2024')).toBeInTheDocument()
    const row = within(section).getByText('The Tenth').closest('li')
    if (!row) throw new Error('the album name is not in a list row')
    expect(within(row).queryByRole('link')).not.toBeInTheDocument()
  })
})

/**
 * The panel's description sits above seven different bodies, so anything it
 * asserts has to be true of all seven. This is where 2e-i lost a review round:
 * a description that quietly reinstated a Spotify provenance directly above the
 * one body that had just been careful not to blame Spotify. No negative
 * assertion inside an individual body's test can catch that, because it is a
 * positive false claim rather than a failure phrasing.
 */
describe('the panel description, read together with the body under it', () => {
  const BODIES: [string, Partial<ArtistDiscography>, string][] = [
    ['nothing asked yet', PENDING, 'Asking Spotify what this artist has released'],
    [
      'a recorded failure',
      { ...PENDING, state: 'unavailable' },
      "This artist's discography could not be read",
    ],
    [
      'fetching turned off',
      { ...PENDING, state: 'disabled' },
      'Artist discographies are turned off',
    ],
    [
      'everything played',
      { coverage: { covered: 11, total: 11 }, missing: [] },
      'You have played something from every album by this artist',
    ],
    [
      'no albums to count',
      {
        coverage: { covered: 0, total: 0 },
        missing: [],
        excluded: { singles: 3, compilations: 0, appearsOn: 0, other: 0 },
      },
      'Spotify lists no albums for this artist',
    ],
  ]

  it.each(BODIES)('claims nothing untrue above %s', async (_label, overrides, body) => {
    stubRoutes({
      '/api/me': ME,
      '/api/artists/artist-1': artistPayload(),
      '/api/artists/artist-1/discography': discography(overrides),
    })
    render(mountAt('/artists/artist-1'))
    const section = await panel(body)

    expect(
      within(section).getByText(
        "Which of this artist's albums have no plays in your history, all time. Singles, " +
          'compilations and appearances are not counted.',
      ),
    ).toBeInTheDocument()
    // Nothing has been read above four of these five, so nothing may say one was.
    expect(within(section).queryByText(/read once and kept/i)).not.toBeInTheDocument()
    expect(within(section).queryByText(/From Spotify's own list/i)).not.toBeInTheDocument()
  })
})

describe('the discography poll', () => {
  it('polls only while a walk is running, and not once it has given up', () => {
    expect(discographyPollInterval('pending')).toBe(3000)
    expect(discographyPollInterval('pending', false)).toBe(3000)
    expect(discographyPollInterval('pending', true)).toBe(false)
    expect(discographyPollInterval('ready')).toBe(false)
    expect(discographyPollInterval('unavailable')).toBe(false)
    expect(discographyPollInterval('disabled')).toBe(false)
    expect(discographyPollInterval(undefined)).toBe(false)
  })

  it('asks again every three seconds while the answer is pending', async () => {
    vi.useFakeTimers()
    const asked = stubRoutes({
      '/api/me': ME,
      '/api/artists/artist-1': artistPayload(),
      '/api/artists/artist-1/discography': discography(PENDING),
    })
    render(mountAt('/artists/artist-1'))

    await act(async () => {
      await vi.advanceTimersByTimeAsync(100)
    })
    expect(discographyCalls(asked)).toBe(1)

    await act(async () => {
      await vi.advanceTimersByTimeAsync(3_100)
    })
    expect(discographyCalls(asked)).toBe(2)
  })

  it('stops asking at the cap, and says so without blaming Spotify', async () => {
    vi.useFakeTimers()
    const asked = stubRoutes({
      '/api/me': ME,
      '/api/artists/artist-1': artistPayload(),
      '/api/artists/artist-1/discography': discography(PENDING),
    })
    render(mountAt('/artists/artist-1'))

    await act(async () => {
      await vi.advanceTimersByTimeAsync(100)
    })
    expect(
      within(panelNow()).getByText('Asking Spotify what this artist has released'),
    ).toBeVisible()

    // Just short of three minutes. Advanced in stages rather than one jump
    // because `act` holds React's updates until its callback returns, so a single
    // leap past the cap would run every interval tick before the component heard
    // that the cap passed — an artefact of the harness, not of the page.
    await act(async () => {
      await vi.advanceTimersByTimeAsync(178_000)
    })
    const beforeCap = discographyCalls(asked)
    expect(beforeCap).toBeGreaterThan(50)
    expect(
      within(panelNow()).getByText('Asking Spotify what this artist has released'),
    ).toBeInTheDocument()

    await act(async () => {
      await vi.advanceTimersByTimeAsync(3_500)
    })
    const settled = discographyCalls(asked)
    expect(settled).toBeLessThanOrEqual(beforeCap + 2)
    const capped = panelNow()
    expect(within(capped).getByText('No discography for this artist yet')).toBeInTheDocument()
    expect(
      within(capped).getByText(
        "Encore waited three minutes for this artist's discography and has stopped for now; it may " +
          'still arrive — reopen this page to check. Every other figure on this page comes from ' +
          'your own history and is unaffected.',
      ),
    ).toBeInTheDocument()
    expect(
      within(capped).queryByText('Asking Spotify what this artist has released'),
    ).not.toBeInTheDocument()
    // Running out of patience is not a refusal, and what causes it is very likely
    // local — a claim that errors persists nothing and re-enters "pending" for
    // ever — so Spotify is not named as the party that would not answer.
    expect(within(capped).queryByText(/could not/i)).not.toBeInTheDocument()
    expect(within(capped).queryByText(/tries again/i)).not.toBeInTheDocument()
    expect(within(capped).queryByText(/Spotify/)).not.toBeInTheDocument()

    // And having given up, it stays given up.
    await act(async () => {
      await vi.advanceTimersByTimeAsync(180_000)
    })
    expect(discographyCalls(asked)).toBe(settled)
  })

  it('does not restart the cap when the tab is reloaded near it', async () => {
    window.sessionStorage.setItem(
      `${DISCOGRAPHY_POLL_START_KEY}artist-1`,
      String(Date.now() - 300_000),
    )
    const asked = stubRoutes({
      '/api/me': ME,
      '/api/artists/artist-1': artistPayload(),
      '/api/artists/artist-1/discography': discography(PENDING),
    })
    render(mountAt('/artists/artist-1'))
    const section = await panel('No discography for this artist yet')

    expect(
      within(section).queryByText('Asking Spotify what this artist has released'),
    ).not.toBeInTheDocument()
    expect(discographyCalls(asked)).toBe(1)
  })

  it('forgets the cap once the artist resolves, so a later pending artist starts fresh', async () => {
    window.sessionStorage.setItem(
      `${DISCOGRAPHY_POLL_START_KEY}artist-1`,
      String(Date.now() - 1_000),
    )
    stubRoutes({
      '/api/me': ME,
      '/api/artists/artist-1': artistPayload(),
      '/api/artists/artist-1/discography': discography(),
    })
    render(mountAt('/artists/artist-1'))
    await panel('The Tenth')

    expect(window.sessionStorage.getItem(`${DISCOGRAPHY_POLL_START_KEY}artist-1`)).toBeNull()
  })

  it('does not share a cap with the album page', async () => {
    // The two panels key their windows by different prefixes, so one artist's
    // stuck walk cannot make an album with the same id report having given up.
    window.sessionStorage.setItem(
      'encore.tracklist-poll-start.artist-1',
      String(Date.now() - 300_000),
    )
    stubRoutes({
      '/api/me': ME,
      '/api/artists/artist-1': artistPayload(),
      '/api/artists/artist-1/discography': discography(PENDING),
    })
    render(mountAt('/artists/artist-1'))
    await panel('Asking Spotify what this artist has released')
  })
})
```

**Fails when** — one line per test, in order:
1. the summary's wording, plural or exclusion clause changes;
2. the excluded sentence is dropped, which is the entire fix for the `album_group` overclaim;
3. any bucket is pluralised unconditionally, giving "1 singles";
4. an empty bucket is rendered, giving "0 compilations";
5. the excluded sentence is rendered when nothing was excluded;
6. the "counts as played" line is dropped, letting "4 of 11" read as four albums heard in full;
7. the `has`/`have` agreement is hard-coded to the plural — the single-missing case is among the commonest this panel reports;
8. the single-album listing is given a ratio;
9. the played-everything body says "played every album", or the count line is printed beside it;
10. `formatPlural` is replaced with string concatenation, giving "1 albums";
11. the zero-albums state falls through to a failure body, a played-everything body, or renders "also";
12. the disabled body blames Spotify, promises a retry, or names the album switch;
13. the unavailable body borrows the disabled or played-everything wording;
14. a failed request throws instead of degrading;
15. the pending body claims anything about what was counted;
16. the request-in-flight frame renders D5's words, which contradict D1 one round trip later;
17. the read date is dropped, so an unrefreshable list reads as current;
18. the read date is rendered only on the has-missing branch — the exact defect 2e-i shipped and fixed;
19. a row gains a link to `/albums/{id}`, which 404s for records nobody played;
20. the description asserts a read above a body where none happened;
21–26. the poll: the interval, the cap, the copy at the cap, the persisted window, the clear on resolve, or the key prefix.

- [ ] **Step 3: Run them to verify they fail**

Run: `cd web && npx vitest run src/test/artist-discography.test.tsx`
Expected: FAIL — `DISCOGRAPHY_POLL_START_KEY` is not exported from `../pages/ArtistDetail`.

- [ ] **Step 4: Write the panel**

In `web/src/pages/ArtistDetail.tsx`, add the imports, the constants and the helpers at the top:

```tsx
import { useEffect, useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { formatDate, formatPlural } from '../lib/format'
import type { ArtistDiscography, DiscographyExcluded, LazyFetchState } from '../lib/types'
import { clearPollStart, lazyPollInterval, pollStartKey, pollStartedAt } from '../lib/fetchpoll'
import { EmptyState, Panel, SkeletonText } from '../components/ui'
import { formatRelease } from './top/TopList'

/**
 * How often the page asks again while Spotify is being walked.
 *
 * Three seconds rather than the album page's two: a discography is up to twenty
 * sequential requests where an album's track list is one, so the answer is
 * further away and a tighter interval buys nothing but load.
 */
const DISCOGRAPHY_POLL_MS = 3000

/**
 * How long this page keeps asking before it stops.
 *
 * The cap is not an optimisation, it is the only thing that ends the poll.
 * "pending" is unbounded server-side: a claim against `artist_album_fetches`
 * that errors records nothing, so the very next request re-enters the same
 * branch, and every "pending" response for one artist is byte-identical however
 * long the state has held. Without this, a read-only replica or a full
 * tablespace turns every open artist tab into a request every three seconds, for
 * ever.
 *
 * Three minutes is chosen against the server's own numbers rather than picked:
 * one artist's whole walk is bounded at two minutes and a lease stranded by a
 * killed process expires after three, so any walk that is genuinely going to
 * resolve has resolved by here.
 */
const DISCOGRAPHY_POLL_CAP_MS = 180_000

/**
 * The cap above, in words, for the one line of copy that has to say how long it
 * waited. Kept adjacent so changing the number cannot silently leave the
 * sentence behind.
 */
const DISCOGRAPHY_POLL_CAP_LABEL = 'three minutes'

/** Twice the cap, for the reason given on the album page's equivalent. */
const DISCOGRAPHY_POLL_WINDOW_MS = 2 * DISCOGRAPHY_POLL_CAP_MS

/**
 * Key prefix for when this tab first saw `pending` for an artist.
 *
 * Its own prefix, not the album page's: the two id spaces are disjoint in
 * practice, but the prefix is what guarantees one panel's stuck walk cannot make
 * the other report having given up.
 */
export const DISCOGRAPHY_POLL_START_KEY = 'encore.discography-poll-start.'

/** The next poll delay, or `false` to stop. Exported so it can be tested without a timer. */
export function discographyPollInterval(
  state: LazyFetchState | undefined,
  gaveUp = false,
): number | false {
  return lazyPollInterval(state, gaveUp, DISCOGRAPHY_POLL_MS)
}

/**
 * "1 single, 1 compilation, 1 appearance and 1 other release", omitting empty
 * buckets entirely.
 *
 * Returns null when nothing was excluded, so the caller renders no sentence
 * rather than one that lists nothing. `other` is any album_group Spotify sends
 * that is none of the four it documents; it is zero today and named rather than
 * dropped so this sentence still accounts for every release the response counted.
 */
function excludedList(e: DiscographyExcluded): string | null {
  const parts: string[] = []
  if (e.singles > 0) parts.push(formatPlural(e.singles, 'single'))
  if (e.compilations > 0) parts.push(formatPlural(e.compilations, 'compilation'))
  if (e.appearsOn > 0) parts.push(formatPlural(e.appearsOn, 'appearance'))
  if (e.other > 0) parts.push(formatPlural(e.other, 'other release'))
  if (parts.length === 0) return null
  if (parts.length === 1) return parts[0]
  return `${parts.slice(0, -1).join(', ')} and ${parts[parts.length - 1]}`
}

/**
 * The panel's own description when there is a list to describe.
 *
 * Both the verb and the denominator bend to the numbers. "1 of the 11 albums …
 * have no plays" disagrees with itself, and one unplayed album is among the
 * commonest things this panel reports; "1 of the 1 album" is not a sentence
 * anybody writes, so a single-album discography drops the ratio altogether.
 *
 * Every form carries the exclusion clause. It is the difference between a true
 * sentence and one that describes an artist with fifty releases as an artist
 * with eleven, and it must survive every edit to the numbers around it.
 */
function discographySummary(missing: number, listed: number): string {
  const tail = 'no plays in your history, all time. Singles, compilations and appearances are not counted.'
  if (listed === 1) {
    return `The only album Spotify lists for this artist has ${tail}`
  }
  return `${formatCount(missing)} of the ${formatPlural(listed, 'album')} Spotify lists for this artist ${
    missing === 1 ? 'has' : 'have'
  } ${tail}`
}
```

Add the panel to the page body, immediately after the two-column Top tracks / Top albums grid and
before the hour-of-day chart:

```tsx
          <DiscographyPanel artistId={id} timeZone={timeZone} />
```

and the components at the bottom of the file:

```tsx
/**
 * How much of this artist's own catalogue you have played.
 *
 * Everything else on this page is computed from listening Encore already holds.
 * This is not: it needs Spotify's own list of what the artist released, which
 * Encore reads the first time somebody opens this page and then keeps. So an
 * empty list here means one of several different things, and saying which is the
 * whole job:
 *
 *   pending     — Encore has not been told what they released yet
 *   unavailable — Encore asked and could not find out
 *   disabled    — this instance does not ask, because its operator said not to
 *   ready       — Encore knows, and either you have played something from all of
 *                 them, or there is nothing here it counts
 *
 * plus one that belongs to this page rather than to the server: "pending" that
 * has outlasted the poll's cap, which is neither a refusal nor still in progress.
 *
 * The cap lives here rather than on the page because it belongs to one artist.
 * The page mounts this keyed by artist id, so nothing about one artist's stuck
 * walk can follow a reader to the next.
 */
function DiscographyPanel({
  artistId,
  timeZone,
}: {
  artistId: string
  timeZone: string
}): ReactElement {
  const [gaveUp, setGaveUp] = useState(false)
  const query = useQuery({
    queryKey: qk.artistDiscography(artistId),
    queryFn: ({ signal }) =>
      api.get<ArtistDiscography>(
        `/artists/${encodeURIComponent(artistId)}/discography`,
        undefined,
        signal,
      ),
    enabled: artistId !== '',
    refetchInterval: (q) => discographyPollInterval(q.state.data?.state, gaveUp),
  })
  const data = query.data
  const state = data?.state

  // Stopping the requests is only half of it: the panel has to say it has
  // stopped, and once the poll ends no further response is coming to re-render
  // it. Hence a timer for the moment the cap passes, sized from the persisted
  // start so a reload resumes the same window rather than opening a new one.
  useEffect(() => {
    const key = pollStartKey(DISCOGRAPHY_POLL_START_KEY, artistId)
    if (state !== 'pending') {
      // A settled answer closes the window, so an artist that returns to
      // "pending" much later is given their own full three minutes.
      if (state) clearPollStart(key)
      return
    }
    const remaining =
      pollStartedAt(key, Date.now(), DISCOGRAPHY_POLL_WINDOW_MS) + DISCOGRAPHY_POLL_CAP_MS - Date.now()
    const timer = window.setTimeout(() => setGaveUp(true), Math.max(remaining, 0))
    return () => {
      window.clearTimeout(timer)
    }
  }, [artistId, state])

  return (
    <Panel
      title="Albums you have never played"
      description={
        // The count is stated only when there is one. With nothing missing it
        // becomes "0 of the 11 albums … have no plays", which is the same fact
        // the body states below it, phrased as a double negative and said twice.
        data?.state === 'ready' && data.missing.length > 0
          ? discographySummary(data.missing.length, data.coverage.total)
          : // This one line sits above seven different bodies, so it may assert
            // nothing that is not true of all seven. Anything about where the
            // list came from is false on "disabled", where no read has ever
            // happened and none ever will, and premature on "pending". So it
            // says only what the panel is for, carries the all-time qualifier
            // the count line opposite it carries, and states the exclusion —
            // which is a rule about what this panel counts and is therefore true
            // even where nothing has been counted.
            "Which of this artist's albums have no plays in your history, all time. Singles, compilations and appearances are not counted."
      }
      padded={false}
    >
      <MissingAlbums
        data={data}
        isPending={query.isPending}
        failed={query.isError}
        gaveUp={gaveUp}
        timeZone={timeZone}
      />
    </Panel>
  )
}

function MissingAlbums({
  data,
  isPending,
  failed,
  gaveUp,
  timeZone,
}: {
  data: ArtistDiscography | undefined
  isPending: boolean
  failed: boolean
  gaveUp: boolean
  timeZone: string
}): ReactElement {
  // The state is checked before the list is, deliberately. Branching on
  // `missing.length` first would render "you have played something from every
  // album" for an artist Encore has not even asked about yet.
  if (data?.state === 'disabled') {
    return (
      <EmptyState
        title="Artist discographies are turned off"
        description="This instance does not ask Spotify what an artist has released, so Encore cannot say which of their albums you have never played. Every other figure on this page comes from your own history and is unaffected. An administrator can turn this on with ENCORE_ARTIST_ALBUMS_ENABLED."
      />
    )
  }
  if (failed || data?.state === 'unavailable') {
    return (
      <EmptyState
        title="This artist's discography could not be read"
        description="Encore could not get the list of what this artist has released from Spotify, so it cannot say which of their albums you have never played. Every other figure on this page comes from your own history and is unaffected. Encore tries again later."
      />
    )
  }
  // Having run out of patience is not the same fact as having been refused, and
  // it gets its own words rather than borrowing the ones above. What is known
  // here is only that "pending" held for the whole window; what caused it is very
  // likely local — a claim against artist_album_fetches that errors logs a
  // warning and persists nothing — so naming Spotify as the party that would not
  // answer is the "disabled" mistake arriving through another door. The sentence
  // also makes no promise to retry, because this page cannot keep one: the
  // recorded window outlives the visit.
  if (gaveUp && data?.state === 'pending') {
    return (
      <EmptyState
        title="No discography for this artist yet"
        description={`Encore waited ${DISCOGRAPHY_POLL_CAP_LABEL} for this artist's discography and has stopped for now; it may still arrive — reopen this page to check. Every other figure on this page comes from your own history and is unaffected.`}
      />
    )
  }
  // A walk in progress (`pending`, confirmed by the server) and a request this
  // panel has not had answered yet (`isPending`) are different facts. Sharing one
  // body would have an instance with fetching turned off claim "Asking Spotify"
  // for the whole round trip and then contradict itself — on the instance whose
  // operator explicitly asked Encore not to talk to Spotify.
  if (isPending && !data) {
    return (
      <div className="px-4 py-3" role="status" aria-live="polite" aria-busy="true">
        <span className="sr-only">Loading this artist's discography</span>
        <SkeletonText lines={2} className="max-w-md" />
      </div>
    )
  }
  if (!data || data.state === 'pending') {
    return (
      <EmptyState
        title="Asking Spotify what this artist has released"
        description="Encore reads it once and keeps it, so this step is skipped on most visits. The list appears here on its own."
      />
    )
  }

  const listed = data.coverage.total
  const excluded = excludedList(data.excluded)
  // Rendered on every `ready` state, not only when something is missing. On an
  // instance with fetching turned off this list will never change again, and a
  // list with no date on it reads as though it were current.
  const readOn = data.fetchedAt
    ? `Discography read from Spotify on ${formatDate(data.fetchedAt, timeZone)}.`
    : null
  // What "played" means here, said wherever a number is shown. "4 of 11 albums"
  // otherwise reads as four records heard end to end, and the second sentence
  // pre-empts the contradiction a reader would find between this panel and the
  // Top albums panel on the same screen, which ranks by play and is not
  // restricted to the album group.
  const countsAsPlayed =
    'An album counts as played when you have played any track from it. Albums you played that Spotify does not list under this artist are not counted here.'

  if (listed === 0) {
    // Not a failure and not "you have played everything": Spotify answered, and
    // the answer is that nothing it lists for this artist is something this panel
    // counts. It has no counterpart on the album page, where a record with no
    // tracks does not exist and an empty listing is recorded as a failure.
    return (
      <EmptyState
        title="Spotify lists no albums for this artist"
        description={
          <>
            Everything Spotify lists for them is a single, a compilation or an appearance on someone
            else&rsquo;s record, and this panel counts none of those.
            {excluded ? (
              // No "also": nothing else was listed, so there is nothing for this
              // to be in addition to.
              <span className="mt-1.5 block">{`Spotify lists ${excluded} for this artist.`}</span>
            ) : null}
            {readOn ? <span className="mt-1.5 block text-xs text-ink-faint">{readOn}</span> : null}
          </>
        }
      />
    )
  }

  const excludedLine = excluded
    ? `Spotify also lists ${excluded} for this artist, which this panel does not count.`
    : null

  if (data.missing.length === 0) {
    return (
      <EmptyState
        icon="album"
        title="You have played something from every album by this artist"
        // Not "every album": coverage counts an album with any play, and the
        // shorter sentence claims eleven records heard end to end.
        description={
          <>
            {`Spotify lists ${formatPlural(listed, 'album')} for this artist.`}
            {excludedLine ? <span className="mt-1.5 block">{excludedLine}</span> : null}
            <span className="mt-1.5 block">{countsAsPlayed}</span>
            {readOn ? <span className="mt-1.5 block text-xs text-ink-faint">{readOn}</span> : null}
          </>
        }
      />
    )
  }

  return (
    <div className="px-4 py-3">
      <ul className="divide-y divide-seam">
        {data.missing.map((album) => (
          <li key={album.id} className="flex items-baseline gap-3 py-2 text-sm">
            {/* No link. Most of these are records nobody has played, so they are
                not in the catalogue and /albums/{id} would 404 on almost all of
                them. */}
            <span className="min-w-0 flex-1 truncate text-ink">{album.name}</span>
            <span className="tabular shrink-0 text-ink-faint">
              {formatRelease(album.releaseDate, album.releasePrecision)}
            </span>
          </li>
        ))}
      </ul>
      {excludedLine ? <p className="mt-3 text-sm text-ink-muted">{excludedLine}</p> : null}
      <p className="mt-2 text-sm text-ink-muted">{countsAsPlayed}</p>
      {readOn ? <p className="mt-2 text-xs text-ink-faint">{readOn}</p> : null}
    </div>
  )
}
```

Add `LazyFetchState` to the `types.ts` exports if it is not re-exported there, and extend
`ArtistDetail.tsx`'s `LoadingBody` with one more `<div className="panel h-40" />` so the page does not
jump when the panel arrives.

- [ ] **Step 5: Run the panel tests to verify they pass**

```bash
cd web && npx vitest run src/test/artist-discography.test.tsx
```
Expected: PASS, every test.

- [ ] **Step 6: Run the whole web suite and typecheck**

```bash
cd web && npx tsc --noEmit && npm run lint && npx vitest run
```
Expected: PASS, and `src/test/album-tracklist.test.tsx` still green and still unmodified.

- [ ] **Step 7: Update the feature table**

In `docs/feature-parity.md`, change the "Coverage across a whole artist's discography … is not built — see Known gaps." sentence at the end of the album-tracks row (:88) to "Coverage across a whole artist's discography is the row below.", and add a row after it:

```markdown
| How much of an artist you have heard | **Implemented** | `GET /api/artists/{id}/discography` reads Spotify's own list of what an artist released (`GET /v1/artists/{id}/albums`) and says how many of those albums have any play in the caller's history, all time. Fetched lazily the first time somebody opens that artist's page and cached for `ENCORE_ARTIST_ALBUMS_TTL` (7 days by default, shorter than the album track listing's 30 because a discography grows); there is no background sweep, for the reason §5.2 gives. Costs `ceil(releases/50)` requests per artist viewed per TTL — one for most artists, up to seven for a prolific one — and it is switchable with `ENCORE_ARTIST_ALBUMS_ENABLED`, a **separate** key from `ENCORE_ALBUM_TRACKS_ENABLED` because the two cost very different amounts. **Completion counts `album_group = "album"` only**, excluding singles, compilations and appearances, because "you have heard 4 of 340 releases" is not a useful sentence — and the page names what it set aside rather than leaving the exclusion silent. An album counts as played when any track from it was played, which the page also says. |
```

Search the file for any other "Known gaps" entry naming discography coverage and remove it in the
same edit; a gap that is now built must not still be listed as one.

- [ ] **Step 8: Full check and commit**

```bash
export PATH="$PATH:$(go env GOPATH)/bin"
go build ./... && go vet ./... && staticcheck ./internal/... ./cmd/...
go test -count=1 ./...
go test -tags=integration -count=1 ./test/integration/
go test -tags=integration -count=1 ./test/e2e/
go test -count=1 ./test/deploy/
cd web && npx tsc --noEmit && npm run lint && npx vitest run && cd ..
git status --short docker-compose.portainer.yml   # must be clean: it was regenerated in Task 3
perl -0777 -ne 'print "NULs: ", tr/\0//, "\n"' web/src/test/artist-discography.test.tsx
git add web/src/pages/ArtistDetail.tsx web/src/test/artist-discography.test.tsx docs/feature-parity.md
git commit -m "$(cat <<'EOF'
Web: how much of an artist's catalogue you have played

Seven silences to keep apart, not the album panel's four. Two of them are new:
an artist whose every release is a single has nothing this panel counts, which
is an answer rather than an absence; and a discography whose walk outlasts three
minutes has neither failed nor finished.

The count says what it counted. "4 of 11 albums" over an artist with forty
singles and seven appearances describes them as having eleven releases, so the
exclusion is in the description above every body — where it is a rule about the
panel and therefore true even where nothing has been counted — and the numbers
themselves are named beneath the list. And "played" means any track from the
record, not the whole record, which the panel says rather than letting the
figure imply otherwise.

No reconciliation line, unlike the album panel: nothing Encore stores counts an
artist's releases, so there is no second number to disagree with. That absence
is the whole premise of the feature.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Self-review

Run against the spec with fresh eyes, as the writing-plans skill requires.

**1. Spec coverage — §5.2, the artist half.**

| Spec requirement | Task |
|---|---|
| "discography coverage — 'you have heard 4 of this artist's 11 albums'" | 6 (arithmetic), 8 (the sentence) |
| `GET /v1/artists/{id}/albums`, because no stored field counts an artist's releases | 2 |
| `artist_albums (artist_id, album_id, album_group, position, fetched_at)` | 1 — with `fetched_at` moved to a sibling table, for the reason `migrations/00013` gives at length and this migration restates |
| "fetched lazily when the relevant detail page is first opened, and cached" | 4 (when), 5 (what), 6 (the trigger) |
| "with a TTL after which the next page view refreshes them" | 3 (7 days), 4 (`due`) |
| "A background sweep is explicitly rejected" | 1 (no stale index), 3 (no worker, no interval), 4 (no loop) |
| "Both use the app token, so neither needs a user scope" | 2 (`getAsApp`); Global Constraints forbids touching `DefaultScopes()` |
| "That endpoint is paginated" | 2 |
| "`album_group` distinguishes albums from singles, compilations and appearances" | 1 (stored), 6 (counted), 8 (named) |
| "Completion counts `album` and excludes the rest by default" | 5 (`CountedGroup`), 6 (`toArtistDiscography`) |
| Spec §7 testing table | No row of it belongs to the artist half — the nearest, "Completion arithmetic", is §5.1 and shipped in 2b. `TestArtistDiscographyAllSinglesIsReadyNotAFailure` is this phase's equivalent property. |

**Task 4 implements no spec requirement**, and that is correct rather than an omission: it is an
engineering decision taken by the project's owner, not a §5.2 line. Its justification is the
shared-code section above, and its acceptance criterion is behavioural identity — `internal/albumtracks`
must be indistinguishable from its merged self, which its own untouched suite is what proves.

**Gaps, stated rather than papered over:**

- **Share links (§6).** Discography coverage is not added to `handleSharedStats`. §6 lists album completion and the library counts as decided-not-built and does not mention discography at all, so adding it would be a decision this plan is not entitled to make. Not a gap in 2e-ii's scope; worth raising with the owner separately.
- **"by default" in "excludes the rest by default"** implies a toggle. None is built: the API reports the excluded counts so a client *could* offer one, and the schema stores every group so no re-fetch would be needed, but no UI control and no query parameter is planned. Making the denominator user-selectable would change what "coverage" means between two page views of the same artist, which needs its own copy design. Deliberately deferred, and the storage decision in Task 1 is what keeps it cheap later.
- **Relinked album ids.** A play recorded under a market-relinked album id counts as unheard, exactly as `spotify.AlbumTracks`' comment documents one level down. The Task 8 copy line "Albums you played that Spotify does not list under this artist are not counted here" is the disclosure; no de-relinking is attempted, matching 2e-i.

**2. Placeholder scan.** No "TBD", no "add appropriate error handling", no "similar to Task N", no "write tests for the above". Four steps direct the implementer to an existing file rather than repeating it, and each names the exact path and line range and says why: Task 4 Step 2 (the three race-guard tests, which are transcribed into `internal/lazyfetch` where their pokes at `track()`/`wg`/`base` become ordinary package-private access), Task 8 Step 1 (`ME`/`stubRoutes`/`mountAt`, which are scaffolding), Task 2 Step 1 (`testClient`), and Task 6 Step 10 (`artistPlayItem`). Reproducing 250 lines of a neighbouring test file would invite an implementer to diverge from it; pointing at it does not. Task 4 Step 2 is the one place a reader should be most suspicious of that, so it also lists, in prose, every assertion each transcribed test must end up making — an implementer who cannot find the original still knows what the test is for.

**3. Type consistency.** Checked end to end:
`lazyfetch.State{Status,FetchedAt,AttemptedAt,Attempts}` (Task 4) is populated from `catalog.AlbumTrackState` (Task 4) and `catalog.ArtistAlbumState` (Task 1), whose four fields carry the same names in the same order — the mapping is field-for-field in both services, which is what Task 5 Step 2's wiring tests exist to keep honest.
`lazyfetch.Leases`' two methods have the same signatures as the repositories' `Claim*`/`Fail*` pairs, so both `leases` adapters are one-line forwards.
`lazyfetch.Outcome` is aliased as `albumtracks.State` and `artistalbums.State`, and its four constants as `State{Ready,Pending,Unavailable,Disabled}` in both — so `internal/httpapi/dto.go`, `dto_test.go` and `cmd/encore-api` compile untouched by Task 4.
`catalog.ArtistAlbum{AlbumID,Name,Group,ReleaseDate,ReleasePrecision,Position}` (Task 1) →
`spotify.ArtistAlbum{ID,Name,Group,ReleaseDate,ReleasePrecision}` (Task 2) →
`artistalbums.Release{AlbumID,Name,Group,ReleaseDate,ReleasePrecision}` (Task 5) →
`httpapi.DiscographyAlbumRef{ID,Name,ReleaseDate,ReleasePrecision}` (Task 6) →
`web DiscographyAlbumRef{id,name,releaseDate,releasePrecision}` (Task 7).
`Position` stops at the repository, which is right: it exists only to break ties in the read order.
`CountedGroup` is defined once (Task 5) and used by `CountedIDs` and `toArtistDiscography`.
`Discography.CountedIDs()` is produced in Task 5 and consumed in Task 6's handler with the signature declared there.
`stats.HeardAlbums(ctx, q, userID, albumIDs)` is declared in Task 6's Interfaces block and called with exactly that shape.
`lazyPollInterval(state, gaveUp, everyMs)` (Task 7) is wrapped by both `tracklistPollInterval(state, gaveUp = false)` (Task 7, unchanged signature) and `discographyPollInterval(state, gaveUp = false)` (Task 8).
`catalog.AlbumTrackFetching`/`OK`/`Failed` and `catalog.ArtistAlbumFetching`/`OK`/`Failed` all equal `lazyfetch.StatusFetching`/`StatusOK`/`StatusFailed` (`"fetching"`, `"ok"`, `"failed"`), which is what makes `Gate.due` able to read a caller's status column at all. Both migrations' `CHECK (status IN (...))` use the same three literals.
`ENCORE_ARTIST_ALBUMS_ENABLED` is spelled identically in `config.go`, `docker-compose.yml`, `.env.example`, `docs/configuration.md`, `docs/api.md`, the e2e test and the D1 copy string — and Task 8's disabled test asserts the panel does *not* name `ENCORE_ALBUM_TRACKS_ENABLED`, which is the way that consistency fails in practice.

**One fix applied during review:** `discographySummary` in Task 8 uses `formatCount`, which
`ArtistDetail.tsx` already imports at line 19 — no import change needed there, but the file's
existing `formatCount`/`formatPlural` import must gain `formatDate`, which Step 4's import line does.

