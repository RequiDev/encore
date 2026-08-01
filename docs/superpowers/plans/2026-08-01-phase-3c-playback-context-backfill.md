# Phase 3c — Playback Context Backfill

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let a live-synced listen say whether it was shuffled and what it played on, by annotating rows that already exist with what the now-playing poller saw — never creating, moving or duplicating one.

**Architecture:** The 3b poller switches from `GET /v1/me/player/currently-playing` to `GET /v1/me/player` — the same one request per account per tick, on the same private rate budget, now carrying `shuffle_state` and a reliable `device`. It appends what it sees to a new short-lived log, `playback_observations`, which is not `listens` and is reached through the same narrow interface 3b already uses, so `internal/nowplaying`'s dependency closure is unchanged and the test that pins it passes untouched. `internal/sync` — the package that already owns every write to `listens` — reads that log after each successful poll and runs one `UPDATE` whose `SET` list is two columns and whose `WHERE` clause can only match a row that is still missing them. The observation crosses from the poller to the writer through the database, exactly as 3b's `now_playing` row crosses from `encore-worker` to `encore-api`.

**Tech Stack:** Go 1.26, pgx/v5, PostgreSQL 17, React 19 + TypeScript + Vite + TanStack Query v5. **No new Go module and no new npm package.**

**Spec:** [`docs/design/2026-07-29-phase-3-write-and-live-design.md`](../../design/2026-07-29-phase-3-write-and-live-design.md) **§2.4 and §2.5** — the two halves Phase 3b deferred on the spec's own written authorisation in §2.5. §1 shipped as Phase 3a at `c939306`; §2.1–§2.3 shipped as Phase 3b and merged at `2cca3a9`.

**Task count: 6.**

---

## The property this phase inverts, and how the old one survives

**Phase 3b's poller structurally cannot write a listen.** `internal/store/listens`, `internal/sync` and `internal/importer` are absent from `internal/nowplaying`'s dependency closure at any depth, and `TestThePollerCannotReachAnythingThatWritesAListen` (`internal/nowplaying/nowplaying_test.go:49`) reads `go list -deps` to say so. That was not decoration: the sync poller's correctness rests on its cursor advancing in the same transaction that commits the listens it covers, and a second writer with a different view of what has been played would produce duplicates the dedupe key catches by accident.

**Phase 3c's entire purpose is to write to `listens`.** These are reconciled by putting the two halves in different packages and letting the database carry the observation between them.

| | Observes | Writes |
|---|---|---|
| `internal/nowplaying` | `GET /v1/me/player` | `now_playing` (3b), `playback_observations` (3c) |
| `internal/sync` | `playback_observations` | `listens` |

### Why not extend `internal/nowplaying`

Because `TestThePollerCannotReachAnythingThatWritesAListen` would have to be deleted or weakened, and it is the only mechanism in the repository that makes "the poller is a read-only observer" more than a comment. A reviewer has verified that property twice. Undoing it to save one table is a bad trade at any price, and it is not needed: the poller does not have to know that `listens` exists in order to write down what it saw.

**What `internal/nowplaying` gains in this phase:** one method on its existing `Observations` interface (`Log`), one pure classifier (`logEntry`), and one call in `check`. It gains **no new import**. `internal/domain` and `internal/store/accounts` are already in its closure. The import-graph test is not edited, and a step in Task 2 runs it specifically to prove that.

### Why `internal/sync` rather than a third package

Because the write's target is `listens`, and `internal/sync` is already the package that writes `listens`. A new package would need `internal/store/listens` in its closure — which is the same reachability by a longer path — and would give the repository two packages that can annotate a listen instead of one. The SQL itself lives in `internal/store/listens/backfill.go`, because Encore's store packages own the SQL for their own table and `internal/httpapi` and `internal/sync` contain none.

### How the observation reaches the writer

Through `playback_observations`, a table. Not a channel, not a shared struct, not a callback. The poller and the sync loop are two goroutines in `encore-worker` today, but they are two *loops* with independent schedules and independent transactions, and 3b's `now_playing` table exists for the identical reason across a process boundary. A row in a table is the only handoff that survives a restart, a crash between the observation and the play landing in `/me/player/recently-played`, and a future in which the poller moves to its own container.

---

## The endpoint decision, and exactly what it costs

**3c changes 3b's endpoint. It does not add a second call.**

`GET /v1/me/player` returns a strict superset of `GET /v1/me/player/currently-playing`: the same `timestamp`, `progress_ms`, `is_playing`, `item`, `currently_playing_type` and `context`, **plus** `shuffle_state`, `repeat_state`, `actions`, and a `device` that 3b's own plan recorded as unreliable on the narrower endpoint. Both answer `204 No Content` when nothing is playing. Both accept `additional_types=episode`. Both require exactly `user-read-playback-state`, which shipped in `config.DefaultScopes()` in Phase 2a.

| | Requests per account per tick | Budget | Refresh budget | Scope |
|---|---|---|---|---|
| Phase 3b | 1 (`/me/player/currently-playing`) | `classNowPlaying` | `RefreshNowPlaying` | `user-read-playback-state` |
| Phase 3c | 1 (`/me/player`) | `classNowPlaying` | `RefreshNowPlaying` | `user-read-playback-state` |

**The cost of this phase in Spotify requests is zero.** Not "small": zero. The quota table in `docs/configuration.md` (1 account @ 30 s ≈ 2,880/day; 5 @ 30 s ≈ 14,400/day) is unchanged and needs no edit for cost. The operator chose the interval knowing that number, and this phase does not spend a request they did not agree to.

**The alternative was rejected explicitly.** A second call to `/me/player` beside the existing `/currently-playing` would double the poller to ≈28,800 requests a day at five accounts and thirty seconds, silently, without the operator changing a single key. That is precisely the "poller that quietly doubled the baseline" `.env.example` warns against.

**Phase 3b anticipated this.** Its plan says, of `/me/player`: *"that endpoint is what Phase 3c needs for `shuffle_state`, and reaching for it now would put this phase's poll on a payload it does not use."* And the phase map already records it: `docs/design/2026-07-29-spotify-api-expansion-overview.md:84` reads `| 8b | Shuffle and platform backfill | GET /me/player | user-read-playback-state | 3c — deferred, see the 3b plan |`.

**Budget, restated because constraint 1 demands it.** Every request this phase makes is `classNowPlaying`, and every token refresh behind it is `spotify.RefreshNowPlaying`. Neither changes. `Client.Player` is the only caller of `classNowPlaying`, exactly as `Client.CurrentlyPlaying` was, so a 429 on it pauses the now-playing limiter alone, never reaches `onPause`, and never writes `app_settings.spotify_paused_until`. Task 1 keeps all four of 3b's budget-isolation tests and re-points them at the renamed method rather than deleting any of them. **Nothing in this phase introduces a request on a shared budget.**

The card gets better as a side effect: `device` now arrives on every account rather than "some clients", so the device clause 3b renders conditionally will render more often. That is a consequence, not a goal, and no copy changes for it — the conditional stays, because Spotify can still answer with no active device.

---

## The match rule, its false-positive mode, and what a row Encore did not observe looks like

### The rule

For a listen `l` with `l.source = 0` (live sync) and a non-null `l.track_id`, find observations `o` of the **same user and the same track** whose instant falls in

```
[ l.played_at , l.played_at + l.ms_played + ObservationTolerance ]
```

and take the one with the **greatest `observed_at`** — the spec's "most recent match", implemented as `DISTINCT ON (l.id) … ORDER BY l.id, o.observed_at DESC`.

`l.played_at` is the start of playback: `migrations/00005_listens.sql` says so, and `internal/sync/ingest.go:151` says `PlayedAt` is used exactly as Spotify reported it because recently-played is the one source that timestamps the start. `l.ms_played` on a `source = 0` row is the track's full duration (`internal/sync/ingest.go:144`), so `played_at + ms_played` is when the play would have ended if it ran to the end. The window is therefore the play, plus a tolerance.

`ObservationTolerance = 60 * time.Second`, named in `internal/store/listens/backfill.go` with its reasoning in a comment, per §2.5's requirement that it not be a literal buried in a query. It is sixty seconds because that is already this repository's statement of how far apart two records of the same event can be — `insertListensSQL`'s cross-source duplicate probe uses `interval '60 seconds'` — and deriving a second number would let the two definitions of temporal proximity drift apart without anything noticing.

### The false-positive mode, named

**Back-to-back plays of the same track.** Repeat-one, or a listener who replays a track immediately, produces two listens whose windows overlap in the tolerance tail. The most-recent observation inside listen A's window may have been taken during play B. The label is still right unless the listener changed device or toggled shuffle inside that sixty seconds — but in that case listen A is labelled with listen B's device or shuffle state, and Encore has no way to tell.

**Shuffle is a property of the player, not of the selection.** `shuffle_state` says the shuffle toggle was on when Encore looked. A listener who turns shuffle on halfway through a track gets that track labelled shuffled although it was not selected by shuffle. This is inherent to observing a setting rather than a decision, and no window width fixes it.

**A seek backwards past the window.** A listener who seeks to the start of a five-minute track four minutes in extends the real play beyond `played_at + ms_played + 60 s`. Later observations then fall outside the window and are simply not used. That is the safe direction: a miss leaves NULL.

**What is *not* a false-positive mode:** a track left paused. Only observations where `is_playing` is true are logged at all (see Task 2's `logEntry`), so a player parked on one track overnight writes nothing and cannot be attributed to a later play of the same track.

### The row when Encore does not know

`listens.shuffle` is `boolean` and nullable, and `migrations/00005_listens.sql` already states the convention: *"NULL means 'not reported', which is deliberately distinct from false."* A listen with no matching observation keeps `NULL`. It does not become `false`.

This is made structural in three places, not one:

1. **`spotify.Playback.ShuffleState` is `*bool`.** If Spotify omits `shuffle_state` from the JSON, a `bool` field would decode to `false` and Encore would record "not shuffled" about a fact it never received. A pointer decodes to `nil`.
2. **`playback_observations.shuffle` is nullable**, and the poller writes `nil` straight through.
3. **The backfill's final `WHERE` requires the observation's value to be non-null** before it will touch the listen: `(l.shuffle IS NULL AND m.shuffle IS NOT NULL) OR (l.device_type IS NULL AND m.device_type IS NOT NULL)`. A matched observation that knows nothing about shuffle cannot write anything into `shuffle`.

`Task 3, Step 1` contains a test whose only job is to fail if any of those three become a non-pointer, a `NOT NULL`, or a bare `COALESCE`.

### Why `device_type` and not `platform`

**The spec asks for `platform`. This plan deliberately does not use it, and this is the one place it deviates from §2.4.**

`listens.platform` holds Spotify's *export* vocabulary: `"Android OS 10 API 29 (samsung, SM-G970F)"`, `"OS X 10.15.7 [x86 8]"`, `"Partner sonos_inc"`, `"web_player"`. `internal/stats/platform.go`'s `PlatformFamily` is a substring classifier built for exactly those shapes. Spotify Connect's `device.type` holds a different vocabulary entirely: `Computer`, `Smartphone`, `Speaker`, `TV`, `CastAudio`, `GameConsole`.

Writing `"Smartphone"` into `platform` does not merely fail to classify — `PlatformFamily("Smartphone")` returns `PlatformOther` — it makes the existing platform breakdown silently start counting two incompatible vocabularies as one, so every historical platform figure changes meaning without changing shape. That is the same class of error as letting "unknown" and "false" share a column, and this repository's convention forbids it.

So Phase 3c adds `listens.device_type text` (nullable, no default — a metadata-only `ALTER` in PostgreSQL 11+, instant on a large table) and leaves `platform` untouched. `docs/design/…-phase-3-write-and-live-design.md` §2.4's sentence "fills `shuffle` and `platform` from it" is corrected in Task 6 rather than obeyed.

`device_name` is stored in `playback_observations` and **never reaches `listens`**. It is the only way an operator can tell two identical device types apart when a label looks wrong, it expires within twenty-four hours, and it is a personal string ("Requi's iPhone") that has no business becoming durable on the fact table. `docs/api.md:644` already records that device information is deliberately absent from every share payload; `/api/stats/context` is registered inside the session-guarded `/api/` group (`internal/httpapi/router.go:96`) and no share endpoint reads it, so the new breakdown inherits that. Task 4 has a step that verifies it.

---

## Idempotence, made structural

**A backfill must never create, move or duplicate a listen.** Five mechanisms, each independently sufficient, so no single edit can undo the property:

1. **The statement is an `UPDATE`.** It has no `INSERT` and no `ON CONFLICT`. It cannot create a row.
2. **The `SET` list is exactly two columns**, `shuffle` and `device_type`. It never names `played_at`, `dedupe_key`, `identity_key`, `user_id`, `track_id`, `ms_played`, `source` or `import_file_id`.
3. **`COALESCE(l.shuffle, m.shuffle)`** — an existing value always wins. An extended export's `shuffle = false` can never be replaced by an observation's `true`.
4. **The candidate CTE requires `(l.shuffle IS NULL OR l.device_type IS NULL)`**, so a fully annotated row is not even considered on a second pass.
5. **The final `WHERE` requires that this pass would actually change something**, so a row whose only missing column the observation cannot supply reports zero rows affected rather than a no-op write.

A second run therefore reports `0` rows affected and leaves every value byte-identical. Task 3 asserts all three of: unchanged `count(*)` on `listens`, `0` from the second call, and identical values.

`rollup_dirty_days` is deliberately **not** marked. `listen_daily_rollup` is keyed `(user, day, track)` and carries no context columns at all — `internal/stats/context.go`'s header says so, which is why every context statistic scans the fact table. Nothing this phase writes can change a rollup, so marking days dirty would recompute aggregates that cannot have moved.

---

## Global Constraints

- **No new Go module dependency and no new npm dependency.** `go.mod`, `go.sum` and `web/package.json` are byte-identical at the end. CI's `lint` job diffs `go mod tidy` output.
- **No new configuration key, and this is a decision rather than an omission.** The feature's switch already exists: with `ENCORE_NOWPLAYING_INTERVAL` unset the poller never runs, `playback_observations` stays empty, and the backfill's driving CTE returns nothing on one index probe. A second key would permit two incoherent states — "observations logged, never used" and "backfill enabled, nothing observed" — neither of which has a correct behaviour. **Constraint 5 (the five places) therefore does not apply to this phase.** Task 5 has a step that proves no `ENCORE_` literal was introduced, because `test/deploy/composeenv_test.go:14` regexes `internal/config/config.go` for `ENCORE_[A-Z0-9]+(_[A-Z0-9]+)*` **including inside comments**, and Task 1 edits a comment in that file.
- **Every Spotify request in this phase is `classNowPlaying`; every token refresh behind it is `spotify.RefreshNowPlaying`.** Neither changes from 3b. A 429 on this path must never reach `onPause`. Pinned by `TestOnlyACatalogueRateLimitPausesTheInstance`, `TestNowPlayingRateLimitTouchesNoOtherBudget` and `TestARefreshForTheNowPlayingPollerDrawsOnItsOwnBudget`, all of which already exist and are re-pointed rather than rewritten in Task 1.
- **The now-playing poller still never writes a listen.** `internal/nowplaying` gains no import. `TestThePollerCannotReachAnythingThatWritesAListen` (`internal/nowplaying/nowplaying_test.go:49`) is **not edited by any task in this plan** and must pass unchanged.
- **The backfill only annotates.** No `INSERT` into `listens`, ever. Pinned by a `count(*)` assertion in Task 3's integration file and by the five structural mechanisms above.
- **Nothing replaces a set.** `playback_observations` is append-only (`ON CONFLICT DO NOTHING`) and its only deletion is bounded by an age predicate. There is no delete-absent reconciliation anywhere in this phase.
- **Anything reaching a `text` column goes through `store.Truncate`** (`internal/store/store.go:193`, rune-safe, appends `...`). In this plan that is two columns: `playback_observations.device_type` and `.device_name`. `track_id` is a Spotify id and is length-bounded by Spotify; it is written raw, exactly as `now_playing.track_id` is.
- **CHECK constraints:** `playback_observations` gets two, both over values **Encore** decides (`track_id <> ''`, and "a row must say something"). It gets **none** on `device_type`'s value set, because Spotify mints that vocabulary and can extend it without warning — the same judgement `migrations/00014` made for `artist_albums.album_group`, and the opposite of the one `00016` and `00017` made for Encore-minted enums. `listens.device_type` gets no constraint for the same reason.
- Next migration number is **`00018_`**. `00017` was Phase 3b. Re-check `ls migrations/` before writing the file. House style is goose `Up` **and** `Down`, both directions working, with the reasoning in comments including what was considered and rejected.
- **`internal/httpapi` contains no SQL and never imports pgx.** It reaches repositories through the narrow interfaces in `server.go`. This phase adds no interface: the device breakdown arrives through the existing `stats.Service`.
- **The DTO exists in three places and they are kept in step by hand:** `internal/httpapi/dto.go`, `web/src/lib/types.ts`, `docs/api.md`. Change one, change all three.
- **Write the character, never the escape.** Phase 3a shipped a Critical that rendered a literal `…` because the escape sat in bare JSXText, which compiles and passes every test. Type `…`, `—` and `’` directly. Every copy string in this plan is written with real characters; copy them verbatim.
- Test DB on port **5433**, not 5432. `make` is **NOT installed** — run the commands directly.
- `go test -race` will **NOT** work locally: no gcc. Omit `-race`. CI runs it on `pull_request` and `workflow_dispatch` only — **not on branch pushes**. Open the PR and read the `unit` job before calling this done.
- Tagged suites share one database: `-p 1`, one package at a time. `go test -tags=integration -count=1 -p 1 ./test/integration/`.
- staticcheck at `$(go env GOPATH)/bin`; `export PATH="$PATH:$(go env GOPATH)/bin"` first.
- **CI's `web` job runs `npm run test`.** Every copy assertion in this plan is therefore guarded.
- **NUL check every file you write:** `perl -0777 -ne 'print "NULs: ", tr/\0//, "\n"' <file>` — expect 0.
- Commit style `Area: lowercase summary`, body explaining *why*, ending `Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>`. Stage paths explicitly; never `git commit -a`.
- **Every test in this plan carries a "Fails when:" line** naming the exact change that breaks it. If you add a test of your own and cannot write that line, the test cannot fail and must be replaced. Specific traps this project has already paid for: a fixture whose byte arithmetic made an unsafe implementation pass; a mutation anchor that matched the wrong function; `queryByText(/^on /)` which cannot match because testing-library trims; and a timing test that only passed after forty warm tests had run. **Never assert an interval value; assert that something stops.** **Never build a fixture whose numbers happen to satisfy the assertion for a reason other than the one being tested** — for the temporal window in Task 3, the in-window and out-of-window fixtures differ by seconds either side of a boundary computed from the named constant, never from a hard-coded literal.

---

## File Structure

| File | Responsibility |
|---|---|
| `internal/spotify/nowplaying.go` | **Modify.** `/v1/me/player`, `ShuffleState *bool`, `RepeatState`, `CurrentlyPlaying` → `Player`. |
| `internal/spotify/nowplaying_test.go` | **Modify.** Re-point the four budget/decoding tests; add the absent-`shuffle_state` test. |
| `internal/config/config.go` | **Modify.** One comment sentence in `DefaultScopes()` naming the old path. |
| `migrations/00018_playback_observations.sql` | **Create.** The log, and `listens.device_type`. |
| `internal/domain/playback.go` | **Create.** `PlaybackObservation`. |
| `internal/store/accounts/observations.go` | **Create.** `PlaybackObservations`: `Log`, `DeleteExpired`, `ObservationRetention`. |
| `internal/store/accounts/accounts.go` | **Modify.** `Repo.PlaybackObservations`. |
| `internal/nowplaying/nowplaying.go` | **Modify.** `SpotifyAPI.Player`, `Observations.Log`, `logEntry`, one call in `check`. |
| `internal/nowplaying/nowplaying_test.go` | **Modify.** Fake gains `Player`/`Log`; new `logEntry` table test. **`TestThePollerCannotReachAnythingThatWritesAListen` is not touched.** |
| `cmd/encore-worker/main.go` | **Modify.** Pass the observations repo; a third delete in `reaper`. |
| `internal/store/listens/backfill.go` | **Create.** The one `UPDATE`, `ObservationTolerance`, `BackfillLookback`. |
| `internal/store/listens/listens.go` | **Modify.** One comment on `improve`'s absent `device_type`. |
| `internal/sync/backfill.go` | **Create.** The call after a successful poll. |
| `internal/sync/account.go` | **Modify.** One call in `poll`. |
| `internal/sync/sync.go` | **Modify.** `Deps` doc comment, if it enumerates what the poller writes. |
| `internal/stats/device.go` | **Create.** `DeviceFamily`. |
| `internal/stats/device_test.go` | **Create.** |
| `internal/stats/context.go` | **Modify.** `Devices`, `DeviceCoverage`, the fourth `UNION ALL`, the header comment. |
| `internal/httpapi/dto.go` | **Modify.** `Devices`, `DeviceCoverage`, `toPlaybackContext`. |
| `web/src/lib/types.ts` | **Modify.** DTO mirror. |
| `web/src/pages/Habits.tsx` | **Modify.** The device chart, the corrected copy, the widened `noContext` gate. |
| `web/src/test/habits.test.tsx` | **Modify.** Every word of the new and corrected copy. |
| `test/integration/backfill_test.go` | **Create.** The window, idempotence, zero new rows, the reaper. |
| `test/integration/contextstats_test.go` | **Modify.** The device breakdown and its denominator. |
| `test/integration/nowplaying_test.go` | **Modify.** Fake gains `Player`; observations are logged end to end. |
| `docs/api.md`, `docs/feature-parity.md`, `docs/configuration.md`, `docs/architecture.md`, `README.md` | **Modify.** The sweep. |
| `docs/design/2026-07-29-phase-3-write-and-live-design.md` | **Modify.** §2 status, and §2.4's `platform` sentence. |
| `docs/design/2026-07-29-spotify-api-expansion-overview.md` | **Modify.** The operations table and its count line. |

---

## Task 1: One request, three more facts

**Files:**
- Modify: `internal/spotify/nowplaying.go`
- Modify: `internal/spotify/nowplaying_test.go`
- Modify: `internal/nowplaying/nowplaying.go` (the `SpotifyAPI` interface and its one call site in `check`)
- Modify: `internal/nowplaying/nowplaying_test.go` (the fake's method name only)
- Modify: `test/integration/nowplaying_test.go` (the fake's method name only)
- Modify: `internal/config/config.go` (one comment in `DefaultScopes()`)

**Interfaces:**
- Consumes: `newTestClient(t, srv, clock, opts...)`, `newFakeClock()`, `WithPauseObserver`, `AsAPIError`, `Client.do`, `Client.endpoint`, `request{…, class, status}`, `classNowPlaying` — all already in `internal/spotify` at head.
- Produces:
  - `func (c *Client) Player(ctx context.Context, accessToken string) (*Playback, error)` — replaces `CurrentlyPlaying`; `(nil, nil)` still means nothing is playing
  - `spotify.Playback.ShuffleState *bool` — nil means Spotify did not report it
  - `spotify.Playback.RepeatState string` — decoded, stored by nothing

- [ ] **Step 1: Write the failing tests**

In `internal/spotify/nowplaying_test.go`, rename every existing call of `c.CurrentlyPlaying(` to `c.Player(` and rename the four test functions accordingly (`TestCurrentlyPlayingReportsNoContentAsNothingPlaying` → `TestPlayerReportsNoContentAsNothingPlaying`, and so on). In `TestPlayerDecodesATrack`, change the expected path assertion and extend the fixture. The whole edited function, and the two new tests, in full:

```go
// TestPlayerDecodesATrack pins the endpoint, the fields the card renders, the
// three facts the backfill needs, the listener's own token, and the query.
//
// The path is /v1/me/player rather than /v1/me/player/currently-playing, and
// that is the whole of Phase 3c's request budget: /me/player returns a strict
// superset of the narrower endpoint — the same item, progress and playing
// state, plus shuffle_state, repeat_state and a device that the narrower one is
// observed to omit — for the same single request. A second call would have
// doubled a loop that already makes fourteen thousand requests a day at five
// accounts and thirty seconds, without the operator changing a key.
//
// additional_types=episode is not decoration: without it Spotify answers a
// podcast with item: null and currently_playing_type "episode", so a named
// episode would render as "something Encore cannot identify".
//
// Fails when: the path reverts to /v1/me/player/currently-playing, which does
// not carry shuffle_state and leaves every backfilled listen NULL for ever;
// additional_types is dropped; or the application token is used instead of the
// listener's, which answers for nobody.
func TestPlayerDecodesATrack(t *testing.T) {
	var gotPath, gotQuery, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery, gotAuth = r.URL.Path, r.URL.RawQuery, r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
            "timestamp": 1785000000000,
            "progress_ms": 161000,
            "is_playing": true,
            "shuffle_state": true,
            "repeat_state": "off",
            "currently_playing_type": "track",
            "device": {"id":"d1","name":"Kitchen speaker","type":"Speaker","is_active":true},
            "item": {
                "id": "track-1", "name": "The Wheel", "type": "track",
                "uri": "spotify:track:track-1", "duration_ms": 255000, "is_local": false,
                "artists": [{"id":"artist-1","name":"SOHN"}]
            }
        }`)
	}))
	defer srv.Close()

	c := newTestClient(t, srv, newFakeClock())
	got, err := c.Player(context.Background(), "user-token")
	if err != nil {
		t.Fatalf("Player: %v", err)
	}
	if got == nil {
		t.Fatal("Player returned nil for a 200 carrying an item")
	}

	if gotPath != "/v1/me/player" {
		t.Errorf("path = %q, want /v1/me/player", gotPath)
	}
	if gotQuery != "additional_types=episode" {
		t.Errorf("query = %q, want additional_types=episode", gotQuery)
	}
	if gotAuth != "Bearer user-token" {
		t.Errorf("authorization = %q, want the listener's own token", gotAuth)
	}
	if !got.IsPlaying {
		t.Error("IsPlaying = false, want true")
	}
	if got.ShuffleState == nil || !*got.ShuffleState {
		t.Errorf("ShuffleState = %v, want a pointer to true", got.ShuffleState)
	}
	if got.RepeatState != "off" {
		t.Errorf("RepeatState = %q, want off", got.RepeatState)
	}
	if got.ProgressMs == nil || *got.ProgressMs != 161000 {
		t.Errorf("ProgressMs = %v, want 161000", got.ProgressMs)
	}
	if got.Device == nil || got.Device.Name != "Kitchen speaker" || got.Device.Type != "Speaker" {
		t.Errorf("Device = %+v, want the Kitchen speaker, type Speaker", got.Device)
	}
	if got.Item == nil || got.Item.Name != "The Wheel" || got.Item.DurationMs != 255000 {
		t.Errorf("Item = %+v, want The Wheel at 255000 ms", got.Item)
	}
	if got.Item == nil || len(got.Item.Artists) != 1 || got.Item.Artists[0].Name != "SOHN" {
		t.Errorf("Artists = %+v, want SOHN", got.Item)
	}
}

// TestPlayerReportsAnAbsentShuffleStateAsUnknown is the "unknown is not false"
// rule at the very first place a Spotify value enters the system.
//
// A bool field would decode a missing shuffle_state to false, and Encore would
// then write "this play was not shuffled" onto a listener's history about a
// fact it never received. Every guard further down the pipeline — the nullable
// column, the IS NOT NULL in the backfill's WHERE — is downstream of this one
// and cannot recover the distinction once it has been lost here.
//
// Fails when: ShuffleState goes back to being a bool. It then decodes to false
// and the pointer check below cannot be satisfied.
func TestPlayerReportsAnAbsentShuffleStateAsUnknown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// No shuffle_state and no device: everything Encore would like and
		// Spotify did not send.
		_, _ = io.WriteString(w, `{
            "is_playing": true,
            "currently_playing_type": "track",
            "item": {"id":"track-1","name":"The Wheel","type":"track","duration_ms":255000}
        }`)
	}))
	defer srv.Close()

	c := newTestClient(t, srv, newFakeClock())
	got, err := c.Player(context.Background(), "user-token")
	if err != nil {
		t.Fatalf("Player: %v", err)
	}
	if got == nil {
		t.Fatal("Player returned nil for a 200 carrying an item")
	}
	if got.ShuffleState != nil {
		t.Errorf("ShuffleState = %v, want nil: an absent shuffle_state is "+
			"\"not reported\", which is a different fact from \"not shuffled\"",
			*got.ShuffleState)
	}
	if got.Device != nil {
		t.Errorf("Device = %+v, want nil when Spotify reported none", got.Device)
	}
}

// TestPlayerMakesExactlyOneRequest pins this phase's whole cost story.
//
// Phase 3c reads shuffle_state and a device by moving to a wider endpoint, not
// by asking twice. The operator chose ENCORE_NOWPLAYING_INTERVAL knowing the
// quota table in docs/configuration.md, and a second call per tick would double
// every figure in it silently.
//
// Fails when: Player calls /me/player/currently-playing as well "for the item",
// or grows a device lookup through /me/player/devices — the counter below then
// reads 2 rather than 1.
func TestPlayerMakesExactlyOneRequest(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := newTestClient(t, srv, newFakeClock())
	if _, err := c.Player(context.Background(), "user-token"); err != nil {
		t.Fatalf("Player: %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("one poll made %d requests, want exactly 1: a second call "+
			"doubles what the operator agreed to spend", got)
	}
}
```

- [ ] **Step 2: Run them and watch them fail**

Run: `go test -count=1 -run 'TestPlayer|TestOnlyACatalogue|TestNowPlayingRateLimit' ./internal/spotify/`

Expected: FAIL to **compile** — `c.Player` and `Playback.ShuffleState` are undefined. A compile failure is the correct first failure: no assertion can run until the method exists.

- [ ] **Step 3: Widen the type and move the endpoint**

In `internal/spotify/nowplaying.go`, add two fields to `Playback`, between `IsPlaying` and `CurrentlyPlayingType`:

```go
	// ShuffleState is the shuffle toggle at the instant of the observation, or
	// nil when Spotify did not report it.
	//
	// A pointer, and that is load bearing rather than tidy. This value is
	// backfilled onto a listener's history by Phase 3c, and listens.shuffle
	// follows migrations/00005's convention that NULL means "not reported",
	// deliberately distinct from false. A bool here would decode an absent
	// field to false and Encore would then state, on somebody's own listening
	// history, that a play was not shuffled — about a fact it never received.
	//
	// It says what the *player* was set to, not how the track was chosen. A
	// listener who turns shuffle on halfway through a track makes this true for
	// a track shuffle did not select, and no window width fixes that; the
	// backfill's own comment records it as the rule's known imprecision.
	ShuffleState *bool `json:"shuffle_state"`
	// RepeatState is "off", "track" or "context".
	//
	// Decoded and stored by nothing. §5 of the phase design declines it
	// explicitly — "repeat_state and volume, available from /me/player and
	// stored by nothing. No question asked for them" — and it is kept here for
	// the reason Device.IsActive is: dropping a field from a response object
	// makes the next reader wonder whether Spotify stopped sending it.
	RepeatState string `json:"repeat_state"`
```

Replace the `CurrentlyPlaying` method's doc comment and signature with:

```go
// Player reads what the listener is playing right now, or nil when nothing is.
//
// A nil result with a nil error is this endpoint's commonest answer and is not
// a failure: Spotify replies 204 No Content when there is no active device. The
// caller records that as "nothing is playing", which is a different fact from
// "Encore has not managed to look", and neither is an error.
//
// The path is /v1/me/player, not /v1/me/player/currently-playing. The two are
// documented with the same response object and this one is a strict superset in
// practice: the same item, progress and playing state, plus shuffle_state,
// repeat_state and a device the narrower endpoint is observed to omit. Both
// require user-read-playback-state and both cost one request, so reading the
// wider payload is free — where a second call to it, beside the narrower one,
// would have doubled a loop that already makes roughly fourteen thousand
// requests a day at five accounts and thirty seconds.
//
// additional_types=episode is required for a podcast to arrive with a name.
// Without it Spotify answers item: null with currently_playing_type "episode",
// and a named episode becomes something no interface can describe.
//
// This is the only caller of classNowPlaying, and that is the whole design: a
// 429 here pauses this budget alone. It never reaches the pause observer, so it
// never writes app_settings.spotify_paused_until, so it can never 409 "sync
// now" for every user or stop enrichment. See requestClass.
func (c *Client) Player(ctx context.Context, accessToken string) (*Playback, error) {
	if accessToken == "" {
		return nil, fmt.Errorf("spotify: player: no access token")
	}

	q := url.Values{}
	q.Set("additional_types", "episode")

	var (
		body   Playback
		status int
	)
	if err := c.do(ctx, request{
		method: http.MethodGet,
		url:    c.endpoint("/v1/me/player", q),
		label:  "get player state",
		bearer: accessToken,
		out:    &body,
		status: &status,
		class:  classNowPlaying,
	}); err != nil {
		return nil, fmt.Errorf("spotify: player: %w", err)
	}
	if status == http.StatusNoContent {
		return nil, nil
	}
	return &body, nil
}
```

- [ ] **Step 4: Follow the rename through its three callers**

`internal/nowplaying/nowplaying.go` — the interface and its doc comment:

```go
// SpotifyAPI is the part of *spotify.Client this package uses.
//
// One method, and that is the whole of this package's reach into Spotify. A nil
// result with a nil error means nothing is playing.
//
// It is GET /v1/me/player rather than the narrower /currently-playing this
// package first shipped against, for one request rather than two: the wider
// endpoint carries shuffle_state and a reliable device, which is what the
// playback-context backfill reads, at the same cost and on the same budget.
type SpotifyAPI interface {
	Player(ctx context.Context, accessToken string) (*spotify.Playback, error)
}
```

and in `check`, `pb, err := w.dep.Spotify.CurrentlyPlaying(ctx, token)` becomes:

```go
	pb, err := w.dep.Spotify.Player(ctx, token)
```

`internal/nowplaying/nowplaying_test.go` and `test/integration/nowplaying_test.go` — rename the fake's method from `CurrentlyPlaying` to `Player`. The bodies do not change.

`internal/config/config.go`, the comment above `"user-read-playback-state"` in `DefaultScopes()`:

```go
		// Playback state for the optional now-playing poller, which reads
		// GET /v1/me/player when ENCORE_NOWPLAYING_INTERVAL is set.
```

The old text named `/v1/me/player/currently-playing`. Note that `test/deploy/composeenv_test.go:14` regexes this file for `ENCORE_[A-Z0-9]+(_[A-Z0-9]+)*` **including inside comments** — the replacement keeps `ENCORE_NOWPLAYING_INTERVAL`, which is already in `docker-compose.yml` and `.env.example`, and introduces no other `ENCORE_` literal.

- [ ] **Step 5: Run every package the rename touched**

```bash
export PATH="$PATH:$(go env GOPATH)/bin"
gofmt -l $(git ls-files '*.go'); go vet ./...; staticcheck ./...
go test -count=1 ./internal/spotify/ ./internal/nowplaying/ ./internal/config/ ./test/deploy/
```

Expected: PASS on all four. `./test/deploy/` in particular must still pass: it is the guard that fires if a new `ENCORE_` string appeared in `config.go`.

- [ ] **Step 6: Run everything**

```bash
go test -count=1 ./...
go test -tags=integration -count=1 -p 1 ./test/integration/
```

Expected: PASS. If `./test/integration/` fails to build, the fake in `test/integration/nowplaying_test.go` still declares `CurrentlyPlaying`.

- [ ] **Step 7: Commit**

```bash
git add internal/spotify/nowplaying.go internal/spotify/nowplaying_test.go \
        internal/nowplaying/nowplaying.go internal/nowplaying/nowplaying_test.go \
        test/integration/nowplaying_test.go internal/config/config.go
git commit -m "$(cat <<'MSG'
Spotify: read the whole player, for the same one request

The now-playing poller has been asking /v1/me/player/currently-playing, which
answers what is in the player but not how it got there. shuffle_state and a
reliable device live on /v1/me/player, and a listen recorded by live sync can
carry neither today: only an extended export fills those columns, so a synced
listen is permanently thinner than an imported one covering the same evening.

The two endpoints are documented with the same response object, require the same
scope, and cost the same single request. So this reads the wider one instead of
asking twice. Asking twice would have doubled a loop that already makes roughly
fourteen thousand requests a day at five accounts and thirty seconds, silently,
without the operator changing a key they chose knowing that number.

shuffle_state is decoded as a *bool rather than a bool. An absent field must not
become "this play was not shuffled" — listens.shuffle already distinguishes NULL
from false, and a bool here would destroy that distinction before any of the
guards downstream of it could apply.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
MSG
)"
```

---

## Task 2: A log the poller can write and a listens writer cannot see

**Files:**
- Create: `migrations/00018_playback_observations.sql`
- Create: `internal/domain/playback.go`
- Create: `internal/store/accounts/observations.go`
- Modify: `internal/store/accounts/accounts.go` (add the field to `Repo`, build it in the constructor)
- Modify: `internal/nowplaying/nowplaying.go` (`Observations` gains `Log`; `logEntry`; one call in `check`)
- Modify: `internal/nowplaying/nowplaying_test.go` (fake gains `Log`; a new table test for `logEntry`)
- Modify: `cmd/encore-worker/main.go` (pass the repo; a third delete in `reaper`)
- Modify: `test/integration/nowplaying_test.go` (fake gains `Log`; one end-to-end test)

**Interfaces:**
- Consumes: `spotify.Playback` with `ShuffleState *bool` (Task 1); `store.Querier`, `store.Truncate`, `store.UUIDArg`, `store.Nullable`, `postgres.Classify`; `harness.New(t)`, `e.NewUser`, `e.Ctx()`, `e.Store.DB()`.
- Produces:
  - `type domain.PlaybackObservation struct { TrackID string; ObservedAt time.Time; Shuffle *bool; DeviceType, DeviceName string }`
  - `type accounts.PlaybackObservations struct{ … }` with `NewPlaybackObservations(*store.Store) *PlaybackObservations`
  - `func (*PlaybackObservations) Log(ctx, q store.Querier, userID uuid.UUID, o domain.PlaybackObservation) error`
  - `func (*PlaybackObservations) DeleteExpired(ctx, q store.Querier, olderThan time.Time) (int64, error)`
  - `const accounts.ObservationRetention = 24 * time.Hour`
  - `accounts.Repo.PlaybackObservations`
  - `func logEntry(pb *spotify.Playback, at time.Time) (domain.PlaybackObservation, bool)` — unexported, in `internal/nowplaying`

- [ ] **Step 1: Write the failing tests**

Append to `internal/nowplaying/nowplaying_test.go`:

```go
// TestLogEntryRecordsOnlyWhatCanBeMatchedAndOnlyWhatWasSaid is the whole rule
// for what enters the observation log, in one table.
//
// Two independent gates, and both matter. The first is matchability: the
// backfill joins on (user_id, track_id, observed_at), so anything without a
// catalogue track id can never match a listen and logging it would grow a table
// nothing can read. The second is that a row must say something: an observation
// with neither a shuffle state nor a device type teaches a listen nothing, and
// writing it would spend a row and a CHECK violation to record silence.
//
// is_playing is required because a paused player is not a play. A track left
// paused overnight at a thirty-second interval would otherwise write nearly
// three thousand rows all claiming the same instant-by-instant device, any of
// which could then be attributed to a later, genuinely different play of the
// same track.
//
// Fails when: the is_playing gate is dropped (the "paused" case then logs);
// kindOf's result stops being consulted (episode, local and advert log); or the
// "says something" guard is removed (the last case logs, and the CHECK in
// 00018 turns a silent observation into a failed write).
func TestLogEntryRecordsOnlyWhatCanBeMatchedAndOnlyWhatWasSaid(t *testing.T) {
	at := time.Date(2026, time.August, 1, 20, 0, 0, 0, time.UTC)
	yes, no := true, false

	track := func(mutate func(*spotify.Playback)) *spotify.Playback {
		pb := &spotify.Playback{
			IsPlaying:            true,
			ShuffleState:         &yes,
			CurrentlyPlayingType: "track",
			Device:               &spotify.Device{Name: "Kitchen speaker", Type: "Speaker"},
			Item: &spotify.PlaybackItem{
				ID: "spotifytrack00000001", Name: "The Wheel", Type: "track", DurationMs: 255000,
			},
		}
		if mutate != nil {
			mutate(pb)
		}
		return pb
	}

	for name, tc := range map[string]struct {
		pb      *spotify.Playback
		wantLog bool
		want    domain.PlaybackObservation
	}{
		"a playing catalogue track": {
			pb: track(nil), wantLog: true,
			want: domain.PlaybackObservation{
				TrackID: "spotifytrack00000001", ObservedAt: at, Shuffle: &yes,
				DeviceType: "Speaker", DeviceName: "Kitchen speaker",
			},
		},
		"shuffle off is still a fact": {
			pb: track(func(pb *spotify.Playback) { pb.ShuffleState = &no }), wantLog: true,
			want: domain.PlaybackObservation{
				TrackID: "spotifytrack00000001", ObservedAt: at, Shuffle: &no,
				DeviceType: "Speaker", DeviceName: "Kitchen speaker",
			},
		},
		"a device with no shuffle state": {
			pb: track(func(pb *spotify.Playback) { pb.ShuffleState = nil }), wantLog: true,
			want: domain.PlaybackObservation{
				TrackID: "spotifytrack00000001", ObservedAt: at, Shuffle: nil,
				DeviceType: "Speaker", DeviceName: "Kitchen speaker",
			},
		},
		"a shuffle state with no device": {
			pb: track(func(pb *spotify.Playback) { pb.Device = nil }), wantLog: true,
			want: domain.PlaybackObservation{
				TrackID: "spotifytrack00000001", ObservedAt: at, Shuffle: &yes,
			},
		},
		"nothing is playing":  {pb: nil, wantLog: false},
		"a paused player":     {pb: track(func(pb *spotify.Playback) { pb.IsPlaying = false }), wantLog: false},
		"a podcast episode":   {pb: track(func(pb *spotify.Playback) { pb.Item.Type = "episode" }), wantLog: false},
		"a local file":        {pb: track(func(pb *spotify.Playback) { pb.Item.IsLocal = true }), wantLog: false},
		"a track with no id":  {pb: track(func(pb *spotify.Playback) { pb.Item.ID = "" }), wantLog: false},
		"an advert": {
			pb:      &spotify.Playback{IsPlaying: true, CurrentlyPlayingType: "ad", ShuffleState: &yes},
			wantLog: false,
		},
		"nothing worth saying": {
			pb: track(func(pb *spotify.Playback) {
				pb.ShuffleState = nil
				pb.Device = nil
			}),
			wantLog: false,
		},
	} {
		t.Run(name, func(t *testing.T) {
			got, ok := logEntry(tc.pb, at)
			if ok != tc.wantLog {
				t.Fatalf("logEntry logged = %v, want %v (got %+v)", ok, tc.wantLog, got)
			}
			if !tc.wantLog {
				return
			}
			if got.TrackID != tc.want.TrackID || !got.ObservedAt.Equal(tc.want.ObservedAt) {
				t.Errorf("TrackID/ObservedAt = %q/%v, want %q/%v",
					got.TrackID, got.ObservedAt, tc.want.TrackID, tc.want.ObservedAt)
			}
			switch {
			case (got.Shuffle == nil) != (tc.want.Shuffle == nil):
				t.Errorf("Shuffle = %v, want %v", got.Shuffle, tc.want.Shuffle)
			case got.Shuffle != nil && *got.Shuffle != *tc.want.Shuffle:
				t.Errorf("Shuffle = %v, want %v", *got.Shuffle, *tc.want.Shuffle)
			}
			if got.DeviceType != tc.want.DeviceType || got.DeviceName != tc.want.DeviceName {
				t.Errorf("Device = %q/%q, want %q/%q",
					got.DeviceType, got.DeviceName, tc.want.DeviceType, tc.want.DeviceName)
			}
		})
	}
}

// TestAFailedLogDoesNotFailTheCheck keeps the card, which is the product, from
// being taken down by the backfill, which is a bonus.
//
// The two writes are independent facts about the same instant: now_playing is
// what the listener sees, playback_observations is evidence for a join that may
// happen minutes later. A log write that fails must be a warning on a line, not
// a check recorded as failed — a failed check makes the card say the display is
// stale when it is not.
//
// Fails when: check starts returning false on a Log error, or routes it through
// recordFailure — the observation count below then drops to zero and the
// failure count rises.
func TestAFailedLogDoesNotFailTheCheck(t *testing.T) {
	var checks, listings atomic.Int32
	obs := &recordingObservations{listings: &listings, logErr: errors.New("disk on fire")}
	w := newTestWatcherWith(t, config.NowPlaying{Interval: 30 * time.Second}, &checks, obs, playingTrackPlayback())

	got, err := w.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if got != 1 {
		t.Fatalf("RunOnce checked %d accounts, want 1: a log failure is not a failed check", got)
	}
	if n := obs.failures.Load(); n != 0 {
		t.Errorf("RecordFailure was called %d times; a card that is correct must not be "+
			"reported as stale because a bonus write went wrong", n)
	}
	if n := obs.records.Load(); n != 1 {
		t.Errorf("Record was called %d times, want 1", n)
	}
}
```

> **Note for the implementer:** `newTestWatcherWith`, `recordingObservations` and `playingTrackPlayback` are small extensions of the existing `newTestWatcher` harness at the bottom of `internal/nowplaying/nowplaying_test.go`. Add the `Log` method and a `logErr` field to whatever fake that file already uses for `Observations`, and give it `records`, `failures` and `logs` counters if it has none. Do not introduce a second fake; widen the one that is there.

- [ ] **Step 2: Run them and watch them fail**

Run: `go test -count=1 -run 'TestLogEntry|TestAFailedLog' ./internal/nowplaying/`
Expected: FAIL to compile — `logEntry` and `domain.PlaybackObservation` are undefined.

- [ ] **Step 3: Write the migration**

Re-check the number first: `ls migrations/` must show `00017_now_playing.sql` as the highest. Create `migrations/00018_playback_observations.sql`:

```sql
-- +goose Up

-- What the now-playing poller saw, kept only long enough for the
-- recently-played feed to catch up with it.
--
-- A log, not a latest. 00017's now_playing is keyed (user_id) and overwritten
-- every tick because a live card needs one current row; this is keyed
-- (user_id, track_id, observed_at) and appended to, because a fuzzy temporal
-- join needs history. One table cannot be both: a log has no "current row" and
-- a latest has no evidence to join against — it overwrites its own.
--
-- It exists because /me/player/recently-played reports what was played and
-- when, and nothing about how. Only an extended export fills shuffle and the
-- playback context columns, so a live-synced listen is permanently thinner than
-- an imported one covering the same evening. The poller sees exactly what is
-- missing, one tick at a time, and this is where it is written down until
-- internal/sync can attach it to the play it belongs to.
CREATE TABLE playback_observations (
    user_id uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,

    -- The Spotify catalogue id, and the join key.
    --
    -- Deliberately NOT a foreign key to tracks, for 00017's reason: Spotify
    -- names a track the moment it starts playing and Encore's catalogue learns
    -- of it only when enrichment gets round to it. A reference would fail the
    -- write for exactly the listener playing something new. The join in
    -- internal/store/listens/backfill.go matches this against listens.track_id,
    -- which is itself a foreign key, so an id naming nothing simply never
    -- matches.
    track_id text NOT NULL,

    -- When Encore looked. Not when the play started: this is a point sample of
    -- a player, and the backfill's window is what turns a point into a claim
    -- about a play.
    observed_at timestamptz NOT NULL,

    -- Spotify Connect's own device vocabulary -- 'Computer', 'Smartphone',
    -- 'Speaker', 'CastAudio' -- and NOT listens.platform's, which holds an
    -- export's free text such as 'Android OS 10 API 29 (samsung, SM-G970F)'.
    -- The two are different vocabularies for different questions and are
    -- deliberately never mixed: writing 'Smartphone' into platform would make
    -- every historical platform figure change meaning without changing shape.
    --
    -- No CHECK on its values. Spotify mints this set and can extend it without
    -- warning, which is exactly the judgement 00014 made for
    -- artist_albums.album_group and the opposite of the one 00016 and 00017
    -- made for values Encore's own classifiers produce.
    device_type text,

    -- The player's human name. Kept here and never copied onto listens: it is
    -- the only way an operator can tell two identical device types apart when a
    -- label looks wrong, it disappears within a day, and "Requi's iPhone" has
    -- no business becoming durable on the fact table.
    device_name text,

    -- The shuffle toggle at observed_at, or NULL when Spotify did not report
    -- one.
    --
    -- Nullable, and that is the whole point of the column's type. listens.shuffle
    -- follows the same rule (see 00005): NULL is "not reported", deliberately
    -- distinct from false. An observation that does not know must not be able
    -- to teach a listen that the answer was no.
    shuffle boolean,

    -- One row per (account, track, instant). Two ticks during the same play
    -- write two rows and both are evidence; a retried write at the same instant
    -- is a duplicate and is dropped by ON CONFLICT DO NOTHING in the
    -- repository. The key is also the join's access path: the backfill probes
    -- (user_id, track_id, observed_at range), which is exactly this prefix.
    PRIMARY KEY (user_id, track_id, observed_at),

    -- Encore only ever logs an item its own classifier called a catalogue
    -- track, which by construction has an id. An empty one would join to every
    -- listen whose track_id is also empty -- there are none, listens.track_id
    -- is a foreign key -- but it would sit in the table for a day meaning
    -- nothing, and a row that means nothing is the kind that later gets read as
    -- if it meant something.
    CONSTRAINT playback_observations_track_id_present CHECK (track_id <> ''),

    -- A row must say something. An observation with neither a shuffle state nor
    -- a device type has nothing to teach a listen, and the poller's own
    -- classifier declines to write one; this is the backstop that turns a
    -- future edit removing that guard into a loud failure rather than a table
    -- quietly filling with silence.
    CONSTRAINT playback_observations_says_something
        CHECK (shuffle IS NOT NULL OR device_type IS NOT NULL)
);

-- No index beyond the primary key.
--
-- The table is bounded by (connected accounts x 24h / ENCORE_NOWPLAYING_INTERVAL):
-- five accounts at thirty seconds is under fifteen thousand rows at its
-- absolute maximum, and every one expires within a day. The join reads the
-- primary key's leading columns; the reaper's DELETE scans, which on a table
-- that size costs less than the write amplification an observed_at index would
-- add to every single tick.

-- What a live-synced listen played on, when Encore was watching at the time.
--
-- A separate column from platform rather than a second vocabulary inside it:
-- see playback_observations.device_type above. NULL means "not observed", which
-- is the state of every row in every history that predates the poller and of
-- every row on an instance that never enables it -- which is to say, most of
-- them, for ever.
--
-- Nullable with no default, so this is a catalogue-only change in PostgreSQL 11
-- and later: no table rewrite, whatever the size of the history.
ALTER TABLE listens ADD COLUMN device_type text;

-- +goose Down
ALTER TABLE listens DROP COLUMN device_type;
DROP TABLE playback_observations;
```

- [ ] **Step 4: Write the domain type and the repository**

Create `internal/domain/playback.go`:

```go
package domain

import "time"

// PlaybackObservation is one look at a listener's player, kept only long enough
// for the recently-played feed to catch up with it.
//
// It describes an instant, not a play. Turning it into a claim about a play is
// the backfill's job and is inherently uncertain; this type carries only what
// was actually seen.
type PlaybackObservation struct {
	// TrackID is the Spotify catalogue id, and the join key. Never empty: an
	// observation of anything without one is not recorded at all.
	TrackID string
	// ObservedAt is when Encore looked.
	ObservedAt time.Time
	// Shuffle is the shuffle toggle at ObservedAt, or nil when Spotify did not
	// report it.
	//
	// A pointer for the reason listens.shuffle is nullable: nil is "not
	// reported", which is a different fact from false. An observation that does
	// not know must not be able to teach a listen that the answer was no.
	Shuffle *bool
	// DeviceType is Spotify Connect's own vocabulary — "Computer",
	// "Smartphone", "Speaker" — and is not listens.platform's, which holds an
	// export's free text. Empty when Spotify reported no device.
	DeviceType string
	// DeviceName is the player's human name. It stays in this log and never
	// reaches listens.
	DeviceName string
}

// SaysSomething reports whether this observation has anything to teach a listen.
//
// The predicate the poller consults before spending a row, and the Go half of
// the playback_observations_says_something constraint. Keeping it here rather
// than inline at the call site means the storage rule and the classifier cannot
// drift apart, because there is one sentence stating it.
func (o PlaybackObservation) SaysSomething() bool {
	return o.Shuffle != nil || o.DeviceType != ""
}
```

Create `internal/store/accounts/observations.go`:

```go
package accounts

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/RequiDev/encore/internal/domain"
	"github.com/RequiDev/encore/internal/postgres"
	"github.com/RequiDev/encore/internal/store"
)

// ObservationRetention is how long a playback observation is kept.
//
// Twenty-four hours, per the phase design: long enough that any sync interval
// an operator could reasonably set has run several times over since the play,
// short enough that the table stays small and never becomes a second listening
// history nobody meant to keep. An observation older than this can no longer
// reach any listen the backfill will look at, so keeping it costs storage and
// buys nothing.
const ObservationRetention = 24 * time.Hour

// observationTextLimit bounds the two columns Spotify's own strings reach.
//
// Applied through store.Truncate, which is rune-safe: a byte-boundary cut
// through a multi-byte rune would make the write that records the observation
// itself fail, which is a worse outcome than a shortened device name.
const observationTextLimit = 100

// PlaybackObservations is the repository for playback_observations: a
// short-lived log of what the now-playing poller saw, written by that poller
// and read by the backfill in internal/store/listens.
//
// It holds nothing that can create a listen. That is not incidental — the
// poller reaches this type through a three-method interface precisely so that
// its dependency closure never acquires anything that writes to listens.
type PlaybackObservations struct{ db *store.Store }

// NewPlaybackObservations builds the repository.
func NewPlaybackObservations(db *store.Store) *PlaybackObservations {
	return &PlaybackObservations{db: db}
}

// logSQL appends one observation.
//
// DO NOTHING rather than DO UPDATE: two writes at the same (user, track,
// instant) describe the same look at the same player, so the second has nothing
// to add, and an UPDATE would let a retry silently rewrite evidence a backfill
// may already have used.
const logSQL = `
    INSERT INTO playback_observations
        (user_id, track_id, observed_at, device_type, device_name, shuffle)
    VALUES ($1, $2, $3, $4, $5, $6)
    ON CONFLICT (user_id, track_id, observed_at) DO NOTHING`

// Log records one observation.
func (r *PlaybackObservations) Log(
	ctx context.Context, q store.Querier, userID uuid.UUID, o domain.PlaybackObservation,
) error {
	_, err := q.Exec(ctx, logSQL, store.UUIDArg(userID), o.TrackID, o.ObservedAt.UTC(),
		store.Nullable(store.Truncate(o.DeviceType, observationTextLimit)),
		store.Nullable(store.Truncate(o.DeviceName, observationTextLimit)),
		o.Shuffle)
	if err != nil {
		return postgres.Classify("log a playback observation", err)
	}
	return nil
}

// deleteExpiredSQL removes observations too old to reach any listen.
//
// Bounded by an age predicate and nothing else. There is no "delete what is not
// in this set" here, deliberately: reconciliation against a supplied set is how
// this repository has lost data three times, and an observation log needs
// nothing of the sort — a row's own timestamp already says whether it has
// outlived its usefulness.
const deleteExpiredSQL = `DELETE FROM playback_observations WHERE observed_at < $1`

// DeleteExpired removes observations made before olderThan and reports how many
// went.
//
// The caller passes now minus ObservationRetention. A zero time deletes nothing,
// which is the safe direction for a mistake; the test that pins this asserts a
// fresh observation survives a reap rather than asserting a count, because a
// count can be satisfied by a query that deleted the wrong rows.
func (r *PlaybackObservations) DeleteExpired(
	ctx context.Context, q store.Querier, olderThan time.Time,
) (int64, error) {
	tag, err := q.Exec(ctx, deleteExpiredSQL, olderThan.UTC())
	if err != nil {
		return 0, postgres.Classify("delete expired playback observations", err)
	}
	return tag.RowsAffected(), nil
}
```

In `internal/store/accounts/accounts.go`, add the field to `Repo` beside `NowPlaying` and build it in the constructor exactly as `NowPlaying` is built:

```go
	// PlaybackObservations is the short-lived log the now-playing poller
	// appends to and the playback-context backfill reads.
	PlaybackObservations *PlaybackObservations
```

- [ ] **Step 5: Teach the poller to log**

In `internal/nowplaying/nowplaying.go`, widen the interface:

```go
// Observations is the part of accounts' now-playing storage this package uses.
//
// An interface for the ordinary reason — the loop is exercised without a
// database — and the set is deliberately narrow: list, record, record a failure,
// and append to the observation log. There is no method here that touches any
// other table, and in particular none that can reach listens. The import-graph
// test at the top of this package's test file is what actually enforces that;
// this comment only says why.
type Observations interface {
	ListDue(ctx context.Context, q store.Querier, olderThan time.Time, limit int) ([]accounts.DueAccount, error)
	Record(ctx context.Context, q store.Querier, userID uuid.UUID, n domain.NowPlaying) error
	RecordFailure(ctx context.Context, q store.Querier, userID uuid.UUID, t time.Time) error
	Log(ctx context.Context, q store.Querier, userID uuid.UUID, o domain.PlaybackObservation) error
}
```

In `check`, immediately after the successful `Record` and before `return true`:

```go
	// The observation log, which is a different table with a different lifetime
	// and a different reader. Best effort by design: this is evidence for a
	// join that may happen minutes from now, where the row above is what the
	// listener is looking at. A card that is correct must not be reported as
	// stale because a bonus write went wrong, so a failure here is a line in the
	// log and nothing else.
	if obs, ok := logEntry(pb, w.now()); ok {
		if err := w.dep.NowPlaying.Log(ctx, w.dep.Store.DB(), account.UserID, obs); err != nil &&
			ctx.Err() == nil {
			log.Warn("could not log a playback observation", logging.Err(err))
		}
	}
	return true
```

And, beside `observe`:

```go
// logEntry decides whether this observation is worth keeping as evidence, and
// what of it.
//
// Pure, and separate from observe for a reason: observe answers "what does the
// card say", which has an answer for every response Spotify can give, while
// this answers "can this be attached to a play later", which has an answer for
// very few of them.
//
// Three gates, each independent:
//
//   - is_playing. A paused player is not a play. A track left paused overnight
//     at thirty seconds would otherwise write nearly three thousand rows, any
//     of which could later be attributed to a genuinely different play of the
//     same track.
//   - a catalogue track. The backfill joins on (user_id, track_id, observed_at);
//     a podcast, a local file and an advert have no id that can ever match a
//     listen, so logging one would grow a table nothing can read.
//   - something to say. An observation with neither a shuffle state nor a
//     device type teaches a listen nothing, and 00018's
//     playback_observations_says_something would refuse the write anyway.
//
// The device *name* is carried here and stops at the log: it never reaches
// listens. See migrations/00018.
func logEntry(pb *spotify.Playback, at time.Time) (domain.PlaybackObservation, bool) {
	if pb == nil || !pb.IsPlaying || pb.Item == nil {
		return domain.PlaybackObservation{}, false
	}
	if kindOf(pb) != domain.PlaybackItemTrack {
		return domain.PlaybackObservation{}, false
	}
	obs := domain.PlaybackObservation{
		TrackID:    pb.Item.ID,
		ObservedAt: at,
		Shuffle:    pb.ShuffleState,
	}
	if pb.Device != nil {
		obs.DeviceType = pb.Device.Type
		obs.DeviceName = pb.Device.Name
	}
	if !obs.SaysSomething() {
		return domain.PlaybackObservation{}, false
	}
	return obs, true
}
```

- [ ] **Step 6: Wire the worker, and reap**

In `cmd/encore-worker/main.go`, change the `nowplaying.Deps` literal's `NowPlaying:` field to the widened repository. `accountsRepo.NowPlaying` satisfies three of the four methods and not `Log`, so pass a small adapter — or, simpler and what this plan chooses, give `accounts.NowPlaying` the fourth method by embedding:

```go
		// Still the single-table repositories, not accountsRepo: with no handle
		// on the credentials repository this loop cannot park an account, and a
		// 403 from an optional read scope must never stop ingesting a listening
		// history that reads perfectly. The observation log is a second
		// single-table repository for the same reason, and neither of the two
		// can reach listens.
		NowPlaying: nowplaying.Store{
			NowPlaying:   accountsRepo.NowPlaying,
			Observations: accountsRepo.PlaybackObservations,
		},
```

with, in `internal/nowplaying/nowplaying.go`:

```go
// Store composes the two single-table repositories this package writes to.
//
// Two repositories rather than one, because they are two tables with two
// lifetimes and two readers: now_playing is one row per account read by the API
// for the live card, playback_observations is an append-only log read by the
// backfill and gone within a day. Composing them here rather than widening
// either repository keeps each one's SQL beside the table it owns, and keeps
// this package's view of storage to the four methods Observations names.
type Store struct {
	NowPlaying interface {
		ListDue(ctx context.Context, q store.Querier, olderThan time.Time, limit int) ([]accounts.DueAccount, error)
		Record(ctx context.Context, q store.Querier, userID uuid.UUID, n domain.NowPlaying) error
		RecordFailure(ctx context.Context, q store.Querier, userID uuid.UUID, t time.Time) error
	}
	Observations interface {
		Log(ctx context.Context, q store.Querier, userID uuid.UUID, o domain.PlaybackObservation) error
	}
}

func (s Store) ListDue(ctx context.Context, q store.Querier, olderThan time.Time, limit int) ([]accounts.DueAccount, error) {
	return s.NowPlaying.ListDue(ctx, q, olderThan, limit)
}

func (s Store) Record(ctx context.Context, q store.Querier, userID uuid.UUID, n domain.NowPlaying) error {
	return s.NowPlaying.Record(ctx, q, userID, n)
}

func (s Store) RecordFailure(ctx context.Context, q store.Querier, userID uuid.UUID, t time.Time) error {
	return s.NowPlaying.RecordFailure(ctx, q, userID, t)
}

func (s Store) Log(ctx context.Context, q store.Querier, userID uuid.UUID, o domain.PlaybackObservation) error {
	return s.Observations.Log(ctx, q, userID, o)
}
```

And in `reaper`, a third delete after the OAuth states one:

```go
			// Observations older than a day can no longer reach any listen the
			// backfill will look at, so keeping them costs storage and buys
			// nothing. Bounded by age and nothing else — there is no set to
			// reconcile against here, deliberately.
			cutoff := time.Now().Add(-accounts.ObservationRetention)
			if n, err := repo.PlaybackObservations.DeleteExpired(ctx, db.DB(), cutoff); err != nil {
				if ctx.Err() == nil {
					log.Warn("could not delete expired playback observations", logging.Err(err))
				}
			} else if n > 0 {
				log.Info("expired playback observations deleted", "count", n)
			}
```

> This is folded into the existing five-minute `reaper` rather than given a `sup.Add("nowplaying-reaper", …)` of its own, which is what Phase 3b's plan anticipated. A third indexed delete on a ticker that already exists does not need a loop, a name in the supervisor, or a shutdown path; the reaper's own comment already frames it as disk hygiene, and this is disk hygiene.

Also update `cmd/encore-worker/main.go`'s package comment if it enumerates what the poller writes.

- [ ] **Step 7: Run the unit tests**

```bash
export PATH="$PATH:$(go env GOPATH)/bin"
gofmt -l $(git ls-files '*.go'); go vet ./...; staticcheck ./...
go test -count=1 ./internal/nowplaying/ ./internal/domain/ ./internal/store/...
```

Expected: PASS.

- [ ] **Step 8: Prove 3b's property survives, on its own**

```bash
go test -count=1 -run TestThePollerCannotReachAnythingThatWritesAListen -v ./internal/nowplaying/
```

Expected: PASS, and the test file's diff must show **no change to that function**. Run `git diff internal/nowplaying/nowplaying_test.go | grep -c 'TestThePollerCannotReach'` — expect `0`. If this test fails, the poller acquired a path to `internal/store/listens`, `internal/sync` or `internal/importer`, and the fix is to remove that path rather than to widen the deny-list.

- [ ] **Step 9: Write the integration test and run it**

Append to `test/integration/nowplaying_test.go`:

```go
// TestAPollLogsAnObservationTheBackfillCanRead is the end-to-end half: what the
// poller writes must be exactly what the join later probes for.
//
// Fails when: Log stops truncating and a long device name overflows the column;
// the observation's instant stops being the check's instant, so the window in
// backfill.go can no longer contain it; or shuffle is written as a value rather
// than a pointer, at which point the unknown case below reads false.
func TestAPollLogsAnObservationTheBackfillCanRead(t *testing.T) {
	e := harness.New(t)
	user := e.NewUser("np-log")
	at := time.Date(2026, time.August, 1, 20, 0, 0, 0, time.UTC)
	yes := true

	if err := e.Accounts.PlaybackObservations.Log(e.Ctx(), e.Store.DB(), user.ID,
		domain.PlaybackObservation{
			TrackID: "spotifytrack00000001", ObservedAt: at, Shuffle: &yes,
			DeviceType: "Speaker", DeviceName: strings.Repeat("é", 400),
		}); err != nil {
		t.Fatalf("Log: %v", err)
	}

	var (
		gotShuffle *bool
		gotType    string
		gotName    string
	)
	if err := e.Store.DB().QueryRow(e.Ctx(),
		`SELECT shuffle, device_type, device_name FROM playback_observations
          WHERE user_id = $1 AND track_id = $2 AND observed_at = $3`,
		user.ID.String(), "spotifytrack00000001", at,
	).Scan(&gotShuffle, &gotType, &gotName); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if gotShuffle == nil || !*gotShuffle {
		t.Errorf("shuffle = %v, want true", gotShuffle)
	}
	if gotType != "Speaker" {
		t.Errorf("device_type = %q, want Speaker", gotType)
	}
	if !utf8.ValidString(gotName) {
		t.Error("device_name is not valid UTF-8; store.Truncate cut through a rune")
	}
	if len([]rune(gotName)) > 400 {
		t.Errorf("device_name kept %d runes; it was not truncated at all", len([]rune(gotName)))
	}

	// Logging the same instant twice is a duplicate, not a second observation.
	if err := e.Accounts.PlaybackObservations.Log(e.Ctx(), e.Store.DB(), user.ID,
		domain.PlaybackObservation{
			TrackID: "spotifytrack00000001", ObservedAt: at, Shuffle: &yes, DeviceType: "Speaker",
		}); err != nil {
		t.Fatalf("Log again: %v", err)
	}
	if n := e.ScalarInt(`SELECT count(*) FROM playback_observations WHERE user_id = $1`,
		user.ID.String()); n != 1 {
		t.Errorf("%d observations after logging the same instant twice, want 1", n)
	}
}

// TestAnObservationThatKnowsNothingIsRefusedByTheDatabase pins the backstop
// behind logEntry's own guard.
//
// logEntry declines to build one; this proves the storage layer would refuse it
// anyway, so removing that guard is a loud failure rather than a table quietly
// filling with rows that teach nothing.
//
// Fails when: playback_observations_says_something is dropped from 00018 — the
// insert then succeeds and the error below is nil.
func TestAnObservationThatKnowsNothingIsRefusedByTheDatabase(t *testing.T) {
	e := harness.New(t)
	user := e.NewUser("np-silent")

	err := e.Accounts.PlaybackObservations.Log(e.Ctx(), e.Store.DB(), user.ID,
		domain.PlaybackObservation{
			TrackID:    "spotifytrack00000001",
			ObservedAt: time.Date(2026, time.August, 1, 20, 0, 0, 0, time.UTC),
		})
	if err == nil {
		t.Fatal("an observation with neither a shuffle state nor a device type was stored")
	}
}
```

```bash
go test -tags=integration -count=1 -p 1 -run 'TestAPollLogs|TestAnObservationThatKnows|TestNowPlaying' ./test/integration/
go test -tags=integration -count=1 -p 1 ./test/integration/
```

Expected: PASS. `migrate_test.go` exercises `Up` and `Down` for every migration, so `00018`'s `Down` is covered by it — read that test's output rather than assuming.

- [ ] **Step 10: Commit**

```bash
git add migrations/00018_playback_observations.sql internal/domain/playback.go \
        internal/store/accounts/observations.go internal/store/accounts/accounts.go \
        internal/nowplaying/nowplaying.go internal/nowplaying/nowplaying_test.go \
        cmd/encore-worker/main.go test/integration/nowplaying_test.go
git commit -m "$(cat <<'MSG'
Playback: a log of what the poller saw, and nothing that can write a listen

The now-playing poller sees shuffle and a device on every tick; the
recently-played feed the listening history comes from sees neither. This is
where the poller writes down what it saw, so internal/sync can attach it to the
play it belongs to once that play arrives.

It is a separate table from now_playing because they are different shapes. A
live card needs one current row and overwrites it every tick; a fuzzy temporal
join needs history to join against, and a table that overwrote itself would
destroy its own evidence before the feed caught up.

The poller keeps the property it was built with. internal/nowplaying gains one
method on an interface it already had and no import at all, so its dependency
closure still contains nothing that can write a listen, and the go list -deps
test that says so is unchanged and still passing.

shuffle is nullable and the Go field is a pointer. An observation that does not
know must not be able to teach a listen that the answer was no.

device_type is a separate column from listens.platform and always will be.
platform holds an export's free text ("Android OS 10 API 29 (samsung,
SM-G970F)"); this holds Spotify Connect's own vocabulary ("Smartphone"). Mixing
them would make every historical platform figure change meaning without changing
shape.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
MSG
)"
```

---

## Task 3: The backfill — annotate, never create

**Files:**
- Create: `internal/store/listens/backfill.go`
- Modify: `internal/store/listens/listens.go` (one comment on `improve`)
- Create: `internal/sync/backfill.go`
- Modify: `internal/sync/account.go` (one call in `poll`)
- Create: `test/integration/backfill_test.go`

**Interfaces:**
- Consumes: `accounts.ObservationRetention`, `domain.PlaybackObservation` (Task 2); `store.Querier`, `store.UUIDArg`, `postgres.Classify`; `harness.New(t)`, `e.Listens`, `e.Accounts.PlaybackObservations`, `e.CountListens`, `e.ScalarInt`.
- Produces:
  - `const listens.ObservationTolerance = 60 * time.Second`
  - `const listens.BackfillLookback = 30 * time.Hour`
  - `func (*listens.Repo) BackfillPlaybackContext(ctx context.Context, q store.Querier, userID uuid.UUID, now time.Time) (int64, error)`
  - `func (p *sync.Poller) backfillPlaybackContext(ctx context.Context, userID uuid.UUID)` — unexported

- [ ] **Step 1: Write the failing tests**

Create `test/integration/backfill_test.go`:

```go
//go:build integration

package integration

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/RequiDev/encore/internal/domain"
	"github.com/RequiDev/encore/internal/store/accounts"
	"github.com/RequiDev/encore/internal/store/listens"
	"github.com/RequiDev/encore/test/harness"
)

// backfillRig is one user, one catalogue track, and helpers for the two rows
// this whole feature is about.
type backfillRig struct {
	e    *harness.Env
	user domain.User
}

const backfillTrackID = "spotifytrack00000001"

func newBackfillRig(t *testing.T, name string) *backfillRig {
	t.Helper()
	e := harness.New(t)
	r := &backfillRig{e: e, user: e.NewUser(name)}
	if err := e.Listens.EnsureTracks(e.Ctx(), e.Store.DB(),
		[]listens.TrackSeed{{ID: backfillTrackID, Name: "The Wheel"}}); err != nil {
		t.Fatalf("EnsureTracks: %v", err)
	}
	return r
}

// listen inserts one live-synced play of the shared track, starting at
// playedAt and lasting durationMs, with shuffle and device_type left NULL.
func (r *backfillRig) listen(t *testing.T, playedAt time.Time, durationMs int32) {
	t.Helper()
	l := domain.Listen{
		UserID:    r.user.ID,
		PlayedAt:  playedAt.UTC(),
		Precision: domain.PrecisionMillisecond,
		Identity:  domain.TrackIdentityFromID(backfillTrackID),
		MsPlayed:  durationMs,
		Source:    domain.SourceSync,
	}
	if _, err := r.e.Listens.InsertListens(
		r.e.Ctx(), r.e.Store.DB(), []listens.StagedListen{listens.Stage(l, nil)}, "UTC"); err != nil {
		t.Fatalf("InsertListens: %v", err)
	}
}

// observe logs one observation of the shared track.
func (r *backfillRig) observe(t *testing.T, at time.Time, shuffle *bool, deviceType string) {
	t.Helper()
	if err := r.e.Accounts.PlaybackObservations.Log(r.e.Ctx(), r.e.Store.DB(), r.user.ID,
		domain.PlaybackObservation{
			TrackID: backfillTrackID, ObservedAt: at.UTC(),
			Shuffle: shuffle, DeviceType: deviceType,
		}); err != nil {
		t.Fatalf("Log: %v", err)
	}
}

// backfill runs one pass and reports how many rows it annotated.
func (r *backfillRig) backfill(t *testing.T, now time.Time) int64 {
	t.Helper()
	n, err := r.e.Listens.BackfillPlaybackContext(r.e.Ctx(), r.e.Store.DB(), r.user.ID, now)
	if err != nil {
		t.Fatalf("BackfillPlaybackContext: %v", err)
	}
	return n
}

// state reads the two columns the backfill may write.
func (r *backfillRig) state(t *testing.T) (shuffle *bool, deviceType *string) {
	t.Helper()
	if err := r.e.Store.DB().QueryRow(r.e.Ctx(),
		`SELECT shuffle, device_type FROM listens WHERE user_id = $1`,
		r.user.ID.String()).Scan(&shuffle, &deviceType); err != nil {
		t.Fatalf("read listen state: %v", err)
	}
	return shuffle, deviceType
}

// TestAnObservationInsideTheWindowFillsTheListenAndOneOutsideLeavesItNull is the
// match rule, asserted from both sides of the boundary the named constant sets.
//
// The two instants are derived from listens.ObservationTolerance rather than
// written as literals. A fixture with hard-coded seconds passes for whichever
// tolerance happens to be in force, which is precisely the shape of test this
// project has shipped unable to fail before: retuning the constant — the one
// thing §2.5 says is most likely to need tuning against real data — would leave
// it green while the behaviour it claims to pin changed underneath it.
//
// Fails when: the window's upper bound stops adding ms_played, or stops adding
// the tolerance, or the tolerance is widened past the gap between the two
// fixtures below — the "outside" case then matches and its assertion fires.
func TestAnObservationInsideTheWindowFillsTheListenAndOneOutsideLeavesItNull(t *testing.T) {
	played := time.Date(2026, time.August, 1, 20, 0, 0, 0, time.UTC)
	const durationMs int32 = 255000
	end := played.Add(time.Duration(durationMs) * time.Millisecond)
	yes := true

	t.Run("inside", func(t *testing.T) {
		r := newBackfillRig(t, "bf-inside")
		r.listen(t, played, durationMs)
		// One second inside the far edge of the window.
		r.observe(t, end.Add(listens.ObservationTolerance-time.Second), &yes, "Speaker")

		if n := r.backfill(t, end.Add(time.Hour)); n != 1 {
			t.Fatalf("backfill annotated %d rows, want 1", n)
		}
		shuffle, deviceType := r.state(t)
		if shuffle == nil || !*shuffle {
			t.Errorf("shuffle = %v, want true", shuffle)
		}
		if deviceType == nil || *deviceType != "Speaker" {
			t.Errorf("device_type = %v, want Speaker", deviceType)
		}
	})

	t.Run("outside", func(t *testing.T) {
		r := newBackfillRig(t, "bf-outside")
		r.listen(t, played, durationMs)
		// One second past the far edge.
		r.observe(t, end.Add(listens.ObservationTolerance+time.Second), &yes, "Speaker")

		if n := r.backfill(t, end.Add(time.Hour)); n != 0 {
			t.Fatalf("backfill annotated %d rows, want 0 for an observation outside the window", n)
		}
		shuffle, deviceType := r.state(t)
		if shuffle != nil {
			t.Errorf("shuffle = %v, want NULL: an unmatched listen must not be labelled", *shuffle)
		}
		if deviceType != nil {
			t.Errorf("device_type = %v, want NULL", *deviceType)
		}
	})

	t.Run("before the play started", func(t *testing.T) {
		r := newBackfillRig(t, "bf-before")
		r.listen(t, played, durationMs)
		r.observe(t, played.Add(-time.Second), &yes, "Speaker")

		if n := r.backfill(t, end.Add(time.Hour)); n != 0 {
			t.Fatalf("backfill annotated %d rows, want 0 for an observation before played_at", n)
		}
	})
}

// TestTheBackfillNeverInventsAFalse is the "unknown and false are different
// facts" rule, at the last place it can still be broken.
//
// An observation that carries a device and no shuffle state must fill
// device_type and leave shuffle NULL. A COALESCE over a nullable column looks
// harmless and would do exactly that; a boolean coerced anywhere upstream, or a
// SET that writes coalesce(m.shuffle, false), would state on somebody's history
// that a play was not shuffled about a fact nobody ever reported.
//
// Fails when: the UPDATE's WHERE drops "m.shuffle IS NOT NULL", or the SET
// grows a default — shuffle then reads false instead of NULL.
func TestTheBackfillNeverInventsAFalse(t *testing.T) {
	played := time.Date(2026, time.August, 1, 20, 0, 0, 0, time.UTC)
	const durationMs int32 = 255000

	r := newBackfillRig(t, "bf-unknown")
	r.listen(t, played, durationMs)
	r.observe(t, played.Add(time.Minute), nil, "Computer")

	if n := r.backfill(t, played.Add(time.Hour)); n != 1 {
		t.Fatalf("backfill annotated %d rows, want 1", n)
	}
	shuffle, deviceType := r.state(t)
	if shuffle != nil {
		t.Errorf("shuffle = %v, want NULL: nobody reported a shuffle state", *shuffle)
	}
	if deviceType == nil || *deviceType != "Computer" {
		t.Errorf("device_type = %v, want Computer", deviceType)
	}
}

// TestRunningTheBackfillTwiceChangesNothingAndCreatesNothing is the idempotence
// property, asserted three ways because one way is not enough.
//
// A count that did not move proves nothing was created. A second call returning
// zero proves nothing was written. Identical values prove nothing was moved.
// Encore's core guarantee is that re-running ingestion adds exactly zero rows,
// and DedupeKey(UserID, Identity, PlayedAt) deliberately excludes context, so a
// backfill that wrote through an INSERT would multiply a listener's history by
// however many times it ran.
//
// Fails when: the statement gains an INSERT or an ON CONFLICT (the count moves);
// the candidate CTE stops filtering on "shuffle IS NULL OR device_type IS NULL"
// AND the final WHERE stops requiring a change (the second call reports 1); or
// COALESCE is dropped so an existing value is rewritten.
func TestRunningTheBackfillTwiceChangesNothingAndCreatesNothing(t *testing.T) {
	played := time.Date(2026, time.August, 1, 20, 0, 0, 0, time.UTC)
	const durationMs int32 = 255000
	yes := true

	r := newBackfillRig(t, "bf-twice")
	r.listen(t, played, durationMs)
	r.observe(t, played.Add(time.Minute), &yes, "Speaker")

	before := r.e.CountListens(r.user.ID)
	if n := r.backfill(t, played.Add(time.Hour)); n != 1 {
		t.Fatalf("the first pass annotated %d rows, want 1", n)
	}
	firstShuffle, firstDevice := r.state(t)

	if n := r.backfill(t, played.Add(time.Hour)); n != 0 {
		t.Fatalf("the second pass annotated %d rows, want 0: a backfill must be idempotent", n)
	}
	if after := r.e.CountListens(r.user.ID); after != before {
		t.Fatalf("listens went from %d to %d; a backfill may only annotate rows that "+
			"already exist", before, after)
	}
	secondShuffle, secondDevice := r.state(t)
	if secondShuffle == nil || *secondShuffle != *firstShuffle {
		t.Errorf("shuffle changed between passes: %v then %v", firstShuffle, secondShuffle)
	}
	if secondDevice == nil || *secondDevice != *firstDevice {
		t.Errorf("device_type changed between passes: %v then %v", firstDevice, secondDevice)
	}
}

// TestTheBackfillNeverOverwritesAnExport pins which record wins when both have
// an opinion.
//
// An extended export carries a real, first-hand shuffle flag for the play it
// describes. An observation is a point sample matched to it by a fuzzy window.
// The export is simply the better record, and a backfill that could overwrite it
// would degrade a history every time it ran.
//
// Fails when: COALESCE(l.shuffle, m.shuffle) becomes COALESCE(m.shuffle,
// l.shuffle), or the candidate CTE stops requiring the column to be NULL.
func TestTheBackfillNeverOverwritesAnExport(t *testing.T) {
	played := time.Date(2026, time.August, 1, 20, 0, 0, 0, time.UTC)
	const durationMs int32 = 255000
	yes, no := true, false

	r := newBackfillRig(t, "bf-export")
	r.listen(t, played, durationMs)
	// The export's own answer, already on the row.
	r.e.Exec(`UPDATE listens SET shuffle = $1 WHERE user_id = $2`, no, r.user.ID.String())
	r.observe(t, played.Add(time.Minute), &yes, "Speaker")

	if n := r.backfill(t, played.Add(time.Hour)); n != 1 {
		t.Fatalf("backfill annotated %d rows, want 1: device_type was still missing", n)
	}
	shuffle, deviceType := r.state(t)
	if shuffle == nil || *shuffle {
		t.Errorf("shuffle = %v, want false: the export's answer must survive", shuffle)
	}
	if deviceType == nil || *deviceType != "Speaker" {
		t.Errorf("device_type = %v, want Speaker: the column the export had nothing for", deviceType)
	}
}

// TestTheMostRecentObservationInTheWindowWins pins the tie-break the spec names,
// and is the test that documents this rule's known imprecision.
//
// Two observations inside one play's window disagree about the device. The
// later one is taken. That is §2.4's "takes the most recent match", and its
// false-positive mode is exactly what this fixture looks like from the other
// side: had the second observation belonged to a *following* play of the same
// track in the tolerance tail, this listen would carry that play's device.
//
// Fails when: ORDER BY o.observed_at DESC becomes ASC, or the DISTINCT ON is
// dropped and the UPDATE picks an arbitrary matching row.
func TestTheMostRecentObservationInTheWindowWins(t *testing.T) {
	played := time.Date(2026, time.August, 1, 20, 0, 0, 0, time.UTC)
	const durationMs int32 = 255000
	yes, no := true, false

	r := newBackfillRig(t, "bf-latest")
	r.listen(t, played, durationMs)
	r.observe(t, played.Add(30*time.Second), &no, "Computer")
	r.observe(t, played.Add(2*time.Minute), &yes, "Speaker")

	if n := r.backfill(t, played.Add(time.Hour)); n != 1 {
		t.Fatalf("backfill annotated %d rows, want 1", n)
	}
	shuffle, deviceType := r.state(t)
	if shuffle == nil || !*shuffle {
		t.Errorf("shuffle = %v, want true from the later observation", shuffle)
	}
	if deviceType == nil || *deviceType != "Speaker" {
		t.Errorf("device_type = %v, want Speaker from the later observation", deviceType)
	}
}

// TestAnotherUsersObservationNeverReachesThisListen is the tenancy check, and it
// is here because the join has three keys and only two of them are obvious.
//
// Fails when: the join drops o.user_id = l.user_id, or the driving CTE stops
// scoping playback_observations to $1 — one listener's device then labels
// another's history.
func TestAnotherUsersObservationNeverReachesThisListen(t *testing.T) {
	played := time.Date(2026, time.August, 1, 20, 0, 0, 0, time.UTC)
	const durationMs int32 = 255000
	yes := true

	r := newBackfillRig(t, "bf-mine")
	other := r.e.NewUser("bf-theirs")
	r.listen(t, played, durationMs)
	if err := r.e.Accounts.PlaybackObservations.Log(r.e.Ctx(), r.e.Store.DB(), other.ID,
		domain.PlaybackObservation{
			TrackID: backfillTrackID, ObservedAt: played.Add(time.Minute),
			Shuffle: &yes, DeviceType: "Speaker",
		}); err != nil {
		t.Fatalf("Log for the other user: %v", err)
	}

	if n := r.backfill(t, played.Add(time.Hour)); n != 0 {
		t.Fatalf("backfill annotated %d rows from another account's observation, want 0", n)
	}
}

// TestAReapedObservationCanNeverMatch pins the retention rule from both sides.
//
// It asserts what survives rather than only a delete count, because a count can
// be satisfied by a statement that deleted the wrong rows — including all of
// them.
//
// Fails when: DeleteExpired's predicate becomes <= now, or the caller passes
// time.Now() instead of now minus ObservationRetention — the fresh observation
// below then disappears and the backfill that follows annotates nothing.
func TestAReapedObservationCanNeverMatch(t *testing.T) {
	now := time.Date(2026, time.August, 2, 12, 0, 0, 0, time.UTC)
	stalePlay := now.Add(-40 * time.Hour)
	freshPlay := now.Add(-time.Hour)
	const durationMs int32 = 255000
	yes := true

	r := newBackfillRig(t, "bf-reap")
	r.listen(t, stalePlay, durationMs)
	r.listen(t, freshPlay, durationMs)
	r.observe(t, stalePlay.Add(time.Minute), &yes, "Speaker")
	r.observe(t, freshPlay.Add(time.Minute), &yes, "Computer")

	gone, err := r.e.Accounts.PlaybackObservations.DeleteExpired(
		r.e.Ctx(), r.e.Store.DB(), now.Add(-accounts.ObservationRetention))
	if err != nil {
		t.Fatalf("DeleteExpired: %v", err)
	}
	if gone != 1 {
		t.Fatalf("DeleteExpired removed %d rows, want exactly 1", gone)
	}
	if left := r.e.ScalarInt(`SELECT count(*) FROM playback_observations WHERE user_id = $1`,
		r.user.ID.String()); left != 1 {
		t.Fatalf("%d observations survived, want 1: the reaper took a row it should not have", left)
	}

	if n := r.backfill(t, now); n != 1 {
		t.Fatalf("backfill annotated %d rows, want 1 — the surviving observation only", n)
	}
	var stale *bool
	if err := r.e.Store.DB().QueryRow(r.e.Ctx(),
		`SELECT shuffle FROM listens WHERE user_id = $1 AND played_at = $2`,
		r.user.ID.String(), stalePlay).Scan(&stale); err != nil {
		t.Fatalf("read the stale listen: %v", err)
	}
	if stale != nil {
		t.Errorf("the stale listen was labelled %v from a reaped observation", *stale)
	}
}

// TestABackfillPassWithNoObservationsAtAllIsANoOp is the disabled-instance case,
// which is most instances.
//
// With ENCORE_NOWPLAYING_INTERVAL unset the log is empty for ever, and the
// backfill must cost one probe and change nothing rather than being something an
// operator has to switch off separately. This is why this phase adds no
// configuration key.
//
// Fails when: the statement gains a side effect that does not depend on a match
// — a dirty-day insert, a touched updated_at — or starts writing NULL over
// existing values.
func TestABackfillPassWithNoObservationsAtAllIsANoOp(t *testing.T) {
	played := time.Date(2026, time.August, 1, 20, 0, 0, 0, time.UTC)
	const durationMs int32 = 255000

	r := newBackfillRig(t, "bf-empty")
	r.listen(t, played, durationMs)
	before := r.e.CountListens(r.user.ID)

	if n := r.backfill(t, played.Add(time.Hour)); n != 0 {
		t.Fatalf("backfill annotated %d rows with an empty observation log, want 0", n)
	}
	if after := r.e.CountListens(r.user.ID); after != before {
		t.Fatalf("listens went from %d to %d on an empty log", before, after)
	}
	if dirty := r.e.ScalarInt(`SELECT count(*) FROM rollup_dirty_days WHERE user_id = $1`,
		r.user.ID.String()); dirty != 0 {
		t.Errorf("%d dirty days were marked; shuffle and device_type appear in no rollup, "+
			"so a backfill can never make one stale", dirty)
	}
}

// TestTheBackfillLeavesAnImportedListenAlone keeps the annotation on the rows it
// is about.
//
// Only a live-synced row is missing this by construction; an extended-export row
// has the export's own first-hand answer, and an account-data row has neither
// the columns nor a played_at precise enough for the window to mean anything.
//
// Fails when: the candidate CTE drops l.source = 0.
func TestTheBackfillLeavesAnImportedListenAlone(t *testing.T) {
	played := time.Date(2026, time.August, 1, 20, 0, 0, 0, time.UTC)
	const durationMs int32 = 255000
	yes := true

	r := newBackfillRig(t, "bf-import")
	r.listen(t, played, durationMs)
	r.e.Exec(`UPDATE listens SET source = 2 WHERE user_id = $1`, r.user.ID.String())
	r.observe(t, played.Add(time.Minute), &yes, "Speaker")

	if n := r.backfill(t, played.Add(time.Hour)); n != 0 {
		t.Fatalf("backfill annotated %d imported rows, want 0", n)
	}
}
```

- [ ] **Step 2: Run them and watch them fail**

Run: `go test -tags=integration -count=1 -p 1 -run 'TestAnObservationInside|TestTheBackfill|TestRunningTheBackfill|TestTheMostRecent|TestAnotherUsers|TestAReaped|TestABackfillPass' ./test/integration/`

Expected: FAIL to compile — `listens.ObservationTolerance` and `BackfillPlaybackContext` are undefined.

- [ ] **Step 3: Write the statement**

Create `internal/store/listens/backfill.go`:

```go
package listens

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/RequiDev/encore/internal/postgres"
	"github.com/RequiDev/encore/internal/store"
)

// ObservationTolerance is how far past a play's end an observation may fall and
// still be taken as evidence about it.
//
// A named constant with its reasoning here rather than a literal buried in the
// query, because §2.5 of the phase design says this is the part most likely to
// need tuning against real data, and a number nobody can find is a number
// nobody tunes.
//
// Sixty seconds, and the number is borrowed rather than invented: insertListensSQL's
// cross-source duplicate probe already uses interval '60 seconds' as this
// repository's statement of how far apart two records of the same event can be.
// Deriving a second figure would let two definitions of temporal proximity
// drift apart without anything noticing.
//
// It bounds the *end* of the window, not the start. An observation before
// played_at cannot be of this play — played_at is the start of playback for
// every source (see migrations/00005) — so the window's lower bound is exact and
// only the upper one needs slack: for the request round trip, for the clock skew
// between Spotify's timestamp and Encore's own, and for a listener who paused for
// a moment mid-track.
//
// Widening it is not free. The tail is where this rule's one false-positive mode
// lives: two plays of the same track back to back — repeat-one, or a replay —
// have overlapping windows, and the most-recent observation inside the first
// play's window may have been taken during the second. Both plays share a device
// and a shuffle setting unless the listener changed one inside the tolerance, so
// the label is wrong only in that case; a wider tolerance makes that case more
// likely and buys nothing a poll interval below sixty seconds does not already
// give.
const ObservationTolerance = 60 * time.Second

// BackfillLookback bounds how far back one pass looks.
//
// Observations live twenty-four hours (accounts.ObservationRetention), and an
// observation at instant T can only match a play that started at or after
// T - duration - ObservationTolerance. Six hours of slack past the retention
// covers any single play a person can plausibly have — a DJ set, a full opera
// act, an audiobook chapter — so no observation that still exists is ever out of
// reach, while the scan stays on listens_user_played_idx and does not walk a
// decade of history on every sync tick.
//
// Not derived from accounts.ObservationRetention by import: internal/store/listens
// must not depend on internal/store/accounts, and a constant that says thirty
// hours with the arithmetic written out is clearer than one that says
// "retention plus slack" and hides which is which.
const BackfillLookback = 30 * time.Hour

// backfillPlaybackContextSQL attaches what the now-playing poller saw to the
// plays it saw them during.
//
// It is an UPDATE and it will stay one. Encore's core guarantee is that
// re-running ingestion adds exactly zero rows, and domain.DedupeKey is computed
// from (user_id, identity_key, played_at) with playback context deliberately
// excluded — so a backfill that wrote through an INSERT would not be caught by
// the duplicate rules at all. Five things keep this idempotent, and they are
// deliberately redundant:
//
//  1. There is no INSERT and no ON CONFLICT. It cannot create a row.
//  2. The SET list is two columns. played_at, dedupe_key, identity_key,
//     track_id, ms_played and source are not among them, so nothing can move.
//  3. COALESCE(l.<col>, m.<col>) — a value already on the row always wins, so an
//     extended export's first-hand answer can never be overwritten by a fuzzy
//     match.
//  4. The candidate CTE only considers a row still missing one of the two
//     columns, so a second pass does not even look at an annotated row.
//  5. The final WHERE requires that this pass would actually change something,
//     so a row whose only gap the observation cannot fill reports zero rather
//     than writing a no-op.
//
// The driving relation is the observation log, not listens. On an instance that
// never set ENCORE_NOWPLAYING_INTERVAL that CTE is empty after one index probe
// and the whole statement collapses — which is why this feature needs no switch
// of its own.
//
// DISTINCT ON (l.id) ... ORDER BY l.id, o.observed_at DESC is §2.4's "takes the
// most recent match", made deterministic: without it two observations inside one
// window would let the planner choose, and the same data would annotate
// differently on different days.
//
// rollup_dirty_days is deliberately not touched. listen_daily_rollup is keyed
// (user, day, track) and carries no context columns at all, so nothing this
// statement writes can make an aggregate stale.
//
// Parameters are $1 user, $2 tolerance in seconds, $3 the earliest played_at to
// consider.
const backfillPlaybackContextSQL = `
WITH obs AS (
    SELECT o.track_id, o.observed_at, o.shuffle, o.device_type
      FROM playback_observations o
     WHERE o.user_id = $1
),
matched AS (
    SELECT DISTINCT ON (l.id) l.id, o.shuffle, o.device_type
      FROM listens l
      JOIN obs o
        ON o.track_id = l.track_id
       AND o.observed_at >= l.played_at
       AND o.observed_at <= l.played_at
                          + (l.ms_played * interval '1 millisecond')
                          + ($2::double precision * interval '1 second')
     WHERE l.user_id = $1
       AND l.source = 0
       AND l.track_id IS NOT NULL
       AND l.played_at >= $3
       AND (l.shuffle IS NULL OR l.device_type IS NULL)
     ORDER BY l.id, o.observed_at DESC
)
UPDATE listens l
   SET shuffle     = COALESCE(l.shuffle, m.shuffle),
       device_type = COALESCE(l.device_type, m.device_type)
  FROM matched m
 WHERE l.id = m.id
   AND (   (l.shuffle     IS NULL AND m.shuffle     IS NOT NULL)
        OR (l.device_type IS NULL AND m.device_type IS NOT NULL))`

// BackfillPlaybackContext annotates one listener's recent live-synced plays with
// what the now-playing poller saw, and reports how many rows it changed.
//
// It creates nothing, moves nothing and duplicates nothing: see the statement's
// own comment for the five mechanisms that make that structural rather than
// conventional.
//
// A row with no matching observation keeps NULL in both columns, which is what
// migrations/00005 already means by NULL — "not reported", deliberately distinct
// from false. An observation that carries a device and no shuffle state fills
// one column and leaves the other NULL; it cannot state that a play was not
// shuffled about a fact nobody reported.
func (r *Repo) BackfillPlaybackContext(
	ctx context.Context, q store.Querier, userID uuid.UUID, now time.Time,
) (int64, error) {
	tag, err := q.Exec(ctx, backfillPlaybackContextSQL,
		store.UUIDArg(userID),
		ObservationTolerance.Seconds(),
		now.UTC().Add(-BackfillLookback))
	if err != nil {
		return 0, postgres.Classify("backfill playback context", err)
	}
	return tag.RowsAffected(), nil
}
```

Then, in `internal/store/listens/listens.go`, extend the `improve` CTE's existing comment about `context_type`/`context_id` — the paragraph beginning "context_type/context_id are deliberately absent from this SET list" — with:

```
-- device_type is absent for the same reason and a stronger one: no export of
-- any vintage reports a Spotify Connect device, so d never has one to
-- contribute, and leaving the column out of the UPDATE preserves whatever the
-- playback-context backfill already attached. Adding it here would let an
-- import erase a fact it does not carry.
```

- [ ] **Step 4: Run the store tests**

```bash
go test -tags=integration -count=1 -p 1 -run 'TestAnObservationInside|TestTheBackfill|TestRunningTheBackfill|TestTheMostRecent|TestAnotherUsers|TestAReaped|TestABackfillPass' ./test/integration/
```

Expected: PASS, all cases.

- [ ] **Step 5: Call it from the sync poller**

Create `internal/sync/backfill.go`:

```go
package sync

import (
	"context"

	"github.com/google/uuid"

	"github.com/RequiDev/encore/internal/logging"
)

// backfillPlaybackContext attaches what the now-playing poller saw to the plays
// this account has just finished having ingested.
//
// Deliberately outside commit's transaction, and deliberately unable to fail a
// sync. Ingestion is the product; this is a bonus. A backfill that could roll
// back an insert would let a bad join cost a listener their listening history,
// which inverts the importance of the two things entirely. The statement is a
// single UPDATE, so it is atomic on its own and needs no transaction of its own
// either.
//
// It runs after every successful poll rather than only after one that inserted
// rows, because a play can reach /me/player/recently-played on a later tick than
// the one that observed it — and because the pass is a single indexed statement
// bounded to one user and thirty hours, which costs nothing to repeat and
// nothing at all on an instance whose observation log is empty.
func (p *Poller) backfillPlaybackContext(ctx context.Context, userID uuid.UUID) {
	n, err := p.dep.Listens.BackfillPlaybackContext(ctx, p.dep.Store.DB(), userID, p.now())
	if err != nil {
		if ctx.Err() == nil {
			p.log.Warn("could not backfill playback context",
				"user", userID.String(), logging.Err(err))
		}
		return
	}
	if n > 0 {
		p.log.Debug("playback context backfilled", "user", userID.String(), "listens", n)
	}
}
```

> Check `internal/sync`'s existing logger field name before writing this — `poller.go` will show whether it is `p.log` or reached through `p.dep.Logger`. Use whatever is already there; do not add a field.

In `internal/sync/account.go`, in `poll`, immediately after `commit` returns without error and before the result is assembled:

```go
	// After the commit, never inside it. See backfillPlaybackContext.
	p.backfillPlaybackContext(ctx, userID)
```

- [ ] **Step 6: Write the wiring test and run it**

Append to `test/integration/backfill_test.go`:

```go
// TestASyncPollAnnotatesWhatThePollerSaw is the seam: an observation logged by
// one loop reaches a listen written by another, through the database and
// nothing else.
//
// It drives the real Poller rather than calling the repository directly,
// because the repository passing and the poller never calling it is exactly the
// failure a store-level test cannot see.
//
// Fails when: the call in poll is removed or moved inside commit's transaction
// (a rollback then discards the annotation); or commit's early return for an
// empty batch starts skipping it, at which point a play that arrives on a later
// tick than its observation is never annotated.
func TestASyncPollAnnotatesWhatThePollerSaw(t *testing.T) {
	// Build the rig the other sync integration tests use — see
	// test/integration/sync_test.go for the fake recently-played server and the
	// Poller construction — stub one page of play history for the shared track,
	// log an observation inside that play's window first, then run one poll.
	//
	// Assert, in this order:
	//   1. the poll inserted exactly one listen;
	//   2. that listen carries shuffle = true and device_type = 'Speaker';
	//   3. a second poll over the same page inserts zero listens and the two
	//      columns are unchanged.
	t.Skip("replace this skip with the body described above, modelled on " +
		"test/integration/sync_test.go's existing poller rig")
}
```

> **This step is not finished until the `t.Skip` is gone.** It is written this way because `test/integration/sync_test.go`'s poller rig is the only correct model for it and must be read before it is copied; a rig invented here would drift from the one every other sync test uses. Read that file, build the test, delete the skip. A test that skips is a test that cannot fail.

```bash
export PATH="$PATH:$(go env GOPATH)/bin"
gofmt -l $(git ls-files '*.go'); go vet ./...; staticcheck ./...
go test -count=1 ./...
go test -tags=integration -count=1 -p 1 ./test/integration/
```

Expected: PASS everywhere, and `go test -tags=integration -v -run TestASyncPollAnnotates ./test/integration/` must print `PASS`, not `SKIP`.

- [ ] **Step 7: Commit**

```bash
git add internal/store/listens/backfill.go internal/store/listens/listens.go \
        internal/sync/backfill.go internal/sync/account.go test/integration/backfill_test.go
git commit -m "$(cat <<'MSG'
Sync: attach what the poller saw to the plays it saw them during

A listen from live sync has never been able to say whether it was shuffled or
what it played on, because /me/player/recently-played reports neither. The
now-playing poller sees both, and now writes them down. This is the join.

It is an UPDATE and it will stay one. Re-running ingestion adds exactly zero
rows, and DedupeKey excludes playback context on purpose, so a backfill written
as an INSERT would not be caught by the duplicate rules at all. Five things keep
this idempotent rather than one: no INSERT, a two-column SET list, COALESCE so an
existing value always wins, a candidate filter that ignores an annotated row, and
a final predicate that refuses to write when nothing would change. A second pass
reports zero rows and leaves every value identical.

A listen with no match keeps NULL, and an observation that reports a device but
no shuffle state fills one column and leaves the other NULL. NULL is "not
reported" and has been since 00005; nothing here may turn it into false.

The match is fuzzy and says so: the window is the play plus a named tolerance,
the most recent observation inside it wins, and two plays of the same track back
to back can hand the first one evidence drawn from the second. Both share a
device and a shuffle setting unless the listener changed one inside a minute, and
that is the rule's one false-positive mode, written down beside the constant.

It runs after the commit and cannot fail a sync. Ingestion is the product; this
is a bonus, and a bonus that could roll back an insert would have the importance
of the two exactly backwards.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
MSG
)"
```

---

## Task 4: The statistic, and a denominator that admits what it cannot see

**Files:**
- Create: `internal/stats/device.go`
- Create: `internal/stats/device_test.go`
- Modify: `internal/stats/context.go` (header comment, `PlaybackContext`, `contextBreakdownSQL`, the scan loop)
- Modify: `internal/httpapi/dto.go` (`PlaybackContextResponse`, `toPlaybackContext`)
- Modify: `web/src/lib/types.ts` (`PlaybackContextResponse`)
- Modify: `test/integration/contextstats_test.go`

**Interfaces:**
- Consumes: `stats.Coverage`, `stats.ContextSlice`, `stats.sortedSlices`, `stats.sumSlices`, `rangeFilter` — all already in `internal/stats`. `httpapi.CoverageResponse`, `toCoverage`, `toContextSlices`.
- Produces:
  - `func stats.DeviceFamily(raw string) string`, with `stats.DeviceUnknown = "unknown"`
  - `stats.PlaybackContext.Devices []ContextSlice`, `.DeviceCoverage Coverage`
  - `httpapi.PlaybackContextResponse.Devices []ContextSliceEntry` (`json:"devices"`), `.DeviceCoverage CoverageResponse` (`json:"deviceCoverage"`)
  - `types.ts: PlaybackContextResponse.devices: ContextSlice[]`, `.deviceCoverage: Coverage`

- [ ] **Step 1: Write the failing tests**

Create `internal/stats/device_test.go`:

```go
package stats

import "testing"

// TestDeviceFamilyNormalisesWithoutInventingAnOpinion pins the one difference
// between this classifier and PlatformFamily beside it.
//
// listens.platform is free text from an export — "Android OS 10 API 29
// (samsung, SM-G970F)", "Partner sonos_inc" — and needs a substring classifier
// to mean anything. device_type is Spotify Connect's own short enumeration, so
// the only work left is case and the absent value. Grouping "CastAudio" under
// "Speaker" would be Encore inventing an opinion about somebody's hardware, and
// a type Spotify adds tomorrow must be counted rather than dropped — for the
// reason PlatformFamily's default already gives: otherwise the denominators
// stop adding up.
//
// Fails when: the default case starts returning a bucket instead of the value
// itself, so a device type Spotify adds is silently folded into "other" and the
// breakdown stops naming what somebody actually played on; or the empty case
// stops mapping to "unknown", at which point an empty bar label reaches a chart.
func TestDeviceFamilyNormalisesWithoutInventingAnOpinion(t *testing.T) {
	for raw, want := range map[string]string{
		"Computer":     "computer",
		"Smartphone":   "smartphone",
		"Tablet":       "tablet",
		"Speaker":      "speaker",
		"TV":           "tv",
		"AVR":          "avr",
		"STB":          "stb",
		"AudioDongle":  "audiodongle",
		"GameConsole":  "gameconsole",
		"CastVideo":    "castvideo",
		"CastAudio":    "castaudio",
		"Automobile":   "automobile",
		"  Speaker  ":  "speaker",
		"Unknown":      DeviceUnknown,
		"":             DeviceUnknown,
		"HoloProjector": "holoprojector",
	} {
		if got := DeviceFamily(raw); got != want {
			t.Errorf("DeviceFamily(%q) = %q, want %q", raw, got, want)
		}
	}
}
```

Append to `test/integration/contextstats_test.go`:

```go
// TestDevicesAreCountedWithTheirOwnDenominator pins the new breakdown and the
// coverage figure beside it.
//
// The denominator is per column, exactly as this package's header requires:
// count of rows with a device_type, over every in-range listen. Not per source —
// a live-synced row may carry a device and no shuffle, or shuffle and no device,
// depending on what Spotify reported at the instant Encore looked, so keying on
// source would overstate it.
//
// Fails when: the fourth UNION ALL branch drops its "device_type IS NOT NULL"
// filter (the unobserved rows then arrive as an empty-keyed bar and deviceTotal
// overstates coverage); or deviceTotal is summed from the wrong branch, at which
// point the coverage figure reports the platform count.
func TestDevicesAreCountedWithTheirOwnDenominator(t *testing.T) {
	// Build four in-range live-synced listens for one user, then set
	// device_type directly: 'Speaker' on two, 'Computer' on one, NULL on the
	// fourth. Call the PlaybackContext statistic over a range containing all
	// four and assert:
	//
	//   got.Devices == []ContextSlice{{Key: "speaker", Plays: 2}, {Key: "computer", Plays: 1}}
	//   got.DeviceCoverage == Coverage{Covered: 3, Total: 4}
	//
	// Model the fixture and the Service construction on
	// TestPlatformsAreGroupedByFamily in this same file, which does the
	// equivalent for listens.platform.
	t.Skip("replace this skip with the body described above, modelled on " +
		"TestPlatformsAreGroupedByFamily in this file")
}
```

> **This step is not finished until the `t.Skip` is gone.** `TestPlatformsAreGroupedByFamily` is a few lines above it and is the exact shape needed.

- [ ] **Step 2: Run them and watch them fail**

```bash
go test -count=1 -run TestDeviceFamily ./internal/stats/
```
Expected: FAIL to compile — `DeviceFamily` and `DeviceUnknown` are undefined.

- [ ] **Step 3: Write the classifier**

Create `internal/stats/device.go`:

```go
package stats

import "strings"

// DeviceUnknown is the bucket a device Spotify did not name falls into.
//
// Spotify's own vocabulary includes a literal "Unknown", and an observation can
// also arrive with an empty type. They are the same fact — the player did not
// say what it is — and share one key rather than producing an empty bar label
// beside a named one.
const DeviceUnknown = "unknown"

// DeviceFamily normalises one raw Spotify Connect device type.
//
// It lowercases, trims, and does nothing else. That is the whole difference
// between this and PlatformFamily beside it, and it is deliberate:
// listens.platform is free text an export writes in a shape no two vintages
// agree on, so grouping it is the only way to make it mean anything, while
// device_type is Spotify's own short enumeration — Computer, Smartphone,
// Speaker, TV, GameConsole, CastAudio — which already means something.
// Folding "CastAudio" into "speaker" would be Encore inventing an opinion about
// somebody's hardware and hiding the answer they asked for.
//
// A type Spotify adds later passes through unchanged and is counted, which is
// the same rule PlatformFamily's default follows and for the same reason: a
// category nobody has seen yet must still be counted, or the denominators stop
// adding up.
//
// Grouping happens at read time and the raw string is never thrown away, so a
// future decision to bucket these reclassifies the whole history without a
// backfill.
func DeviceFamily(raw string) string {
	s := strings.ToLower(strings.TrimSpace(raw))
	if s == "" || s == DeviceUnknown {
		return DeviceUnknown
	}
	return s
}
```

- [ ] **Step 4: Add the breakdown to the statistic**

In `internal/stats/context.go`, first correct the header comment. The sentence beginning "The columns are partial." currently says the eight columns are *written only by the extended-export importer*. Replace that paragraph with:

```go
// The columns are partial, and they are partial in two different ways now.
//
// platform, conn_country, reason_start, reason_end, skipped, offline and
// incognito are written only by the extended-export importer; a live-synced or
// account-data row carries NULL in all seven. shuffle and device_type are the
// exception: the now-playing poller observes both while a listener is playing,
// and internal/store/listens' BackfillPlaybackContext attaches them to
// live-synced rows afterwards — so those two can be populated on an instance
// with no export at all, and are populated for nothing at all on an instance
// that never set ENCORE_NOWPLAYING_INTERVAL.
//
// Which is exactly why every figure travels with its own denominator, and why
// the denominator is counted per column — count(*) FILTER (WHERE col IS NOT
// NULL) — never per source. Keying on source = 2 was already wrong because an
// export may omit an individual field; it is now wrong twice over, because two
// of these columns have a second and entirely independent source.
```

Add the two fields to `PlaybackContext`, after `PlatformCoverage`:

```go
	// Devices is the Spotify Connect device-type breakdown, and is not
	// Platforms. The two answer the same question from different vocabularies
	// and different sources and are never merged; see migrations/00018.
	Devices        []ContextSlice
	DeviceCoverage Coverage
```

In `contextBreakdownSQL`, add `l.device_type` to the `scoped` CTE and a fourth branch:

```go
var contextBreakdownSQL = fmt.Sprintf(`
WITH scoped AS (
    SELECT l.platform, l.conn_country, l.reason_end, l.device_type FROM listens l WHERE %s
)
SELECT 'platform' AS kind, s.platform AS key, count(*)::bigint
FROM scoped s WHERE s.platform IS NOT NULL GROUP BY s.platform
UNION ALL
SELECT 'country', s.conn_country, count(*)::bigint
FROM scoped s WHERE s.conn_country IS NOT NULL GROUP BY s.conn_country
UNION ALL
SELECT 'reason_end', s.reason_end, count(*)::bigint
FROM scoped s WHERE s.reason_end IS NOT NULL GROUP BY s.reason_end
UNION ALL
SELECT 'device', s.device_type, count(*)::bigint
FROM scoped s WHERE s.device_type IS NOT NULL GROUP BY s.device_type`,
	rangeFilter("l", "$1", "$2", "$3"))
```

In `PlaybackContext`'s scan loop, beside `families` and `platformTotal`:

```go
	devices := map[string]int64{}
	var deviceTotal int64
```

and a fourth case:

```go
		case "device":
			devices[DeviceFamily(key)] += plays
			deviceTotal += plays
```

and after `out.PlatformCoverage` is set:

```go
	out.Devices = sortedSlices(devices)
	out.DeviceCoverage = Coverage{Covered: deviceTotal, Total: total}
```

- [ ] **Step 5: Carry it through the DTO and the mirror**

`internal/httpapi/dto.go` — two fields on `PlaybackContextResponse`, after `PlatformCoverage`:

```go
	Devices        []ContextSliceEntry `json:"devices"`
	DeviceCoverage CoverageResponse    `json:"deviceCoverage"`
```

and extend that type's doc comment's closing paragraph:

```go
// Devices and DeviceCoverage are the second exception, and have the opposite
// lineage from Playlists: device_type is written by the now-playing poller's
// backfill, never by any export. It is deliberately a separate figure from
// Platforms rather than folded into it — platform holds an export's free text
// and device_type holds Spotify Connect's own vocabulary, and a client that
// merged them would be counting two incompatible answers as one.
```

In `toPlaybackContext`, two lines beside the platform pair:

```go
		Devices:        toContextSlices(c.Devices),
		DeviceCoverage: toCoverage(c.DeviceCoverage),
```

`web/src/lib/types.ts` — two fields on `PlaybackContextResponse`, after `platformCoverage`:

```ts
  devices: ContextSlice[]
  deviceCoverage: Coverage
```

and add to that interface's TSDoc:

```ts
 * `devices` and `deviceCoverage` have the opposite lineage from the export-only
 * figures beside them: `listens.device_type` is filled by the now-playing
 * poller's backfill and by nothing else, so an import-only instance reads zero
 * there for ever while `platforms` reads normally. They are separate figures on
 * purpose — `platform` is an export's free text, `device_type` is Spotify
 * Connect's own vocabulary, and merging them would count two incompatible
 * answers as one.
```

- [ ] **Step 6: Confirm no share endpoint gained it**

```bash
grep -rn "PlaybackContext\|stats/context" internal/httpapi/share.go internal/httpapi/router.go
```

Expected: exactly one hit, `internal/httpapi/router.go` registering `GET /api/stats/context` inside the session-guarded `/api/` group. `docs/api.md:644` records that playback-context statistics are deliberately absent from every share payload because a device says what hardware somebody owns; if this grep returns a hit in `share.go`, stop — the device breakdown must not ship on a share link.

- [ ] **Step 7: Run it all**

```bash
export PATH="$PATH:$(go env GOPATH)/bin"
gofmt -l $(git ls-files '*.go'); go vet ./...; staticcheck ./...
go test -count=1 ./internal/stats/ ./internal/httpapi/
go test -tags=integration -count=1 -p 1 ./test/integration/
cd web && npx tsc --noEmit && npm run test && cd ..
```

Expected: PASS. `npx tsc --noEmit` will fail until `web/src/test/habits.test.tsx`'s `contextPayload` gains the two new fields — that failure is Task 5's first step and is expected here only if you have not yet added them; add them to the fixture now (`devices: [], deviceCoverage: zeroCoverage`) so this task's tree is green on its own.

- [ ] **Step 8: Commit**

```bash
git add internal/stats/device.go internal/stats/device_test.go internal/stats/context.go \
        internal/httpapi/dto.go web/src/lib/types.ts web/src/test/habits.test.tsx \
        test/integration/contextstats_test.go
git commit -m "$(cat <<'MSG'
Stats: count what people played on, beside what they played it from

Shuffle share needed no change at all: count(l.shuffle) already counts every row
that carries the fact, so a listen the backfill annotated raises the coverage
figure the page has always printed. The device breakdown is new, and it is a
separate figure from the platform one rather than an addition to it.

platform holds an export's free text — "Android OS 10 API 29 (samsung,
SM-G970F)", "Partner sonos_inc" — and PlatformFamily is a substring classifier
built for exactly those shapes. device_type holds Spotify Connect's own
vocabulary: "Computer", "Smartphone", "Speaker". Writing the second into the
first would not merely fail to classify, it would make every historical platform
figure change meaning without changing shape.

So DeviceFamily lowercases and stops. Grouping "CastAudio" under "speaker" would
be Encore inventing an opinion about somebody's hardware, and a type Spotify adds
tomorrow passes through and is counted, because a category nobody has seen yet
must still be counted or the denominators stop adding up.

This file's header said the eight context columns are written only by the
extended-export importer. Two of them are not any more, and a comment that is
false is worse than no comment, because it is the thing the next reader trusts.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
MSG
)"
```

---

## Task 5: Every sentence on the page, and the ones that stopped being true

**Files:**
- Modify: `web/src/pages/Habits.tsx`
- Modify: `web/src/test/habits.test.tsx`

**Interfaces:**
- Consumes: `PlaybackContextResponse.devices`, `.deviceCoverage` (Task 4); `formatCount`, `formatPercent`, `formatRatio`, `formatPlural` from `../lib/format`; `BarChart`, `ChartCard`; `Panel`, `EmptyState`, `Icon`; the existing `labelFor`, `toBarData`, `rateHint`, `ChartLoading`; `stubRoutes`, `mountAt`, `waitForPage`, `contextPayload`, `me`, `tastePayload` from the test file.
- Produces: nothing consumed by a later task.

**The copy problem this task exists to fix.** `Habits.tsx` currently asserts, in four separate places, that shuffle is recorded *only* by the extended export. Phase 3c makes that false. Twenty-four copy defects across four phases were caught by review and not by a green suite, and this is the exact shape of all of them: a sentence that was true when it was written, about a mechanism that has since changed, guarded by tests that assert the string rather than the fact.

**Why no `/api/nowplaying` request is added here.** It would tell the page whether the poller is on right now, which does not change what *this range's* numbers mean. It would add a loading state, an error state and a fourth stubbed route to every existing test on this page, for a sentence that can be written definitely without it. Settings already says what the instance is configured to do; this page links there.

- [ ] **Step 1: Write the failing tests**

Add the two new fields to `contextPayload` in `web/src/test/habits.test.tsx` (if Task 4 has not already):

```ts
    devices: [],
    deviceCoverage: zeroCoverage,
```

Then append a new `describe` block:

```tsx
/**
 * The device breakdown, and the sentences that stopped being true when the
 * now-playing poller started filling shuffle and device_type.
 */
describe('what you played on', () => {
  // Fails when: the ChartCard's description stops naming its denominator, or
  // stops naming the source — a listener seeing 3.1% has to be told that is a
  // property of the data rather than a bug, and that only listening Encore
  // watched live can ever be counted.
  it('states its denominator and where the fact comes from', async () => {
    stubRoutes({
      '/api/me': me([]),
      '/api/stats/context': contextPayload({
        shuffleRate: { value: 0.62, covered: 31, total: 1000 },
        deviceCoverage: { covered: 31, total: 1000 },
        devices: [
          { key: 'speaker', plays: 20 },
          { key: 'smartphone', plays: 11 },
        ],
      }),
      '/api/stats/taste': tastePayload(),
    })

    render(mountAt('/habits'))
    await waitForPage()

    expect(
      await screen.findByText(
        'Known for 3.1% of your listening in this range — 31 plays of 1,000. Encore learns this by watching your player, so only listening it saw live can be counted.',
      ),
    ).toBeInTheDocument()
  })

  // Fails when: formatPlural is replaced by a bare `${n} plays` — the singular
  // case then reads "1 plays", which is the defect this project has shipped in
  // four separate phases.
  it('says one play, not one plays', async () => {
    stubRoutes({
      '/api/me': me([]),
      '/api/stats/context': contextPayload({
        shuffleRate: { value: 1, covered: 1, total: 1000 },
        deviceCoverage: { covered: 1, total: 1000 },
        devices: [{ key: 'speaker', plays: 1 }],
      }),
      '/api/stats/taste': tastePayload(),
    })

    render(mountAt('/habits'))
    await waitForPage()

    expect(
      await screen.findByText(
        'Known for 0.1% of your listening in this range — 1 play of 1,000. Encore learns this by watching your player, so only listening it saw live can be counted.',
      ),
    ).toBeInTheDocument()
  })

  // Fails when: the zero-coverage description is dropped and the card falls back
  // to the generic "nothing to rank in this range" — which says the range is
  // empty rather than that Encore was not watching.
  it('says nothing was watched, rather than nothing was played, at zero coverage', async () => {
    stubRoutes({
      '/api/me': me([]),
      '/api/stats/context': contextPayload({
        skipRate: { value: 0.1, covered: 800, total: 800 },
        endReasonCoverage: { covered: 800, total: 800 },
        deviceCoverage: { covered: 0, total: 800 },
        devices: [],
      }),
      '/api/stats/taste': tastePayload(),
    })

    render(mountAt('/habits'))
    await waitForPage()

    expect(
      await screen.findByText(
        'Encore was not watching your player during this range, so it does not know what any of it played on.',
      ),
    ).toBeInTheDocument()
  })

  // The gate. A sync-only instance with the poller enabled has zero coverage on
  // all six export columns and real coverage on device_type — the exact case
  // `noContext` would have hidden, taking the new chart down with it.
  //
  // Fails when: deviceCoverage is not added to the noContext expression. The
  // whole block collapses to the empty state and the assertions below fire.
  it('does not hide the page when the only thing known came from the poller', async () => {
    stubRoutes({
      '/api/me': me([]),
      '/api/stats/context': contextPayload({
        deviceCoverage: { covered: 400, total: 400 },
        devices: [{ key: 'smartphone', plays: 400 }],
      }),
      '/api/stats/taste': tastePayload(),
    })

    render(mountAt('/habits'))
    await waitForPage()

    expect(await screen.findByText('What you played on')).toBeInTheDocument()
    expect(screen.queryByText('No playback detail yet')).not.toBeInTheDocument()
  })

  // Fails when: the empty state keeps the old sentence, which named shuffle as
  // export-only. The poller has filled shuffle since Phase 3c, and telling
  // somebody to import an export to see it sends them to do work they do not
  // need to do.
  it('no longer claims shuffle can only come from an export', async () => {
    stubRoutes({
      '/api/me': me([]),
      '/api/stats/context': contextPayload(),
      '/api/stats/taste': tastePayload(),
    })

    render(mountAt('/habits'))
    await waitForPage()

    expect(await screen.findByText('No playback detail yet')).toBeInTheDocument()
    expect(
      screen.getByText(/How a play ended, and whether it was offline or in a private session/),
    ).toBeInTheDocument()
    expect(
      screen.getByText(/Shuffle and the device you played on can also be filled in as you listen/),
    ).toBeInTheDocument()
    // Never the old sentence.
    expect(
      screen.queryByText(/Skip, shuffle, offline and incognito are recorded only by/),
    ).not.toBeInTheDocument()
  })
})
```

- [ ] **Step 2: Run them and watch them fail**

```bash
cd web && npm run test -- habits && cd ..
```
Expected: FAIL. The first three fail on `findByText` timing out; the fourth finds "No playback detail yet"; the fifth finds the old sentence.

- [ ] **Step 3: Correct the file's own doc comment**

In `web/src/pages/Habits.tsx`, replace the first paragraph of the header block (the sentences from "Every figure on this page is partial" to "before it says anything else.") with:

```tsx
 * Every figure on this page is partial in a way the top lists and Genres are
 * not, and they are partial in two different ways.
 *
 * Platform, country, incognito, offline and the reason a play ended are columns
 * Spotify's *extended* streaming history export writes and nothing else does —
 * a live-synced play and a one-year account-data export carry NULL in all of
 * them. Shuffle and the device are the exception: since Phase 3c the
 * now-playing poller observes both while somebody is listening and the sync
 * path attaches them afterwards, so those two can be populated on an instance
 * that has never imported anything, and are empty for ever on one that never
 * turns the poller on.
 *
 * Showing zero percentages in either situation would read as "you never skip,
 * shuffle, go offline or use incognito," which is not a measurement, it is
 * silence mistaken for a fact. The coverage banner above everything else exists
 * so the page says which situation it is in before it says anything else.
```

- [ ] **Step 4: Add the device chart, its prose, and widen the gate**

Import `formatPlural` alongside the existing format helpers:

```tsx
import { formatCount, formatPercent, formatPlural, formatRatio } from '../lib/format'
```

Add the label map beside `PLATFORM_LABELS`:

```tsx
/**
 * Spotify Connect's device types, as somebody would say them. `labelFor` falls
 * back to the raw key, so a type Spotify adds tomorrow shows up under its own
 * name rather than disappearing — which is the same rule the server's
 * `DeviceFamily` follows, and for the same reason: a category nobody has seen
 * yet must still be counted.
 */
const DEVICE_LABELS: Record<string, string> = {
  computer: 'Computer',
  smartphone: 'Phone',
  tablet: 'Tablet',
  speaker: 'Speaker',
  tv: 'TV',
  avr: 'Receiver',
  stb: 'Set-top box',
  audiodongle: 'Audio dongle',
  gameconsole: 'Games console',
  castvideo: 'Chromecast (video)',
  castaudio: 'Chromecast (audio)',
  automobile: 'Car',
  unknown: 'Unknown',
}
```

Widen the `noContext` expression with a seventh clause and extend its comment:

```tsx
  // Zero, not merely partial: *every* context column is absent from this range,
  // so every figure below would be a fabricated zero rather than a measurement.
  // deviceCoverage is in this list even though it has the opposite lineage from
  // the six beside it — the poller fills it, no export ever does — because a
  // sync-only instance with the poller on has zero coverage on all six and real
  // coverage here, and gating that case out would hide the one chart that
  // instance actually has.
  const noContext =
    context.isSuccess &&
    (data?.skipRate.covered ?? 0) === 0 &&
    (data?.shuffleRate.covered ?? 0) === 0 &&
    (data?.offlineRate.covered ?? 0) === 0 &&
    (data?.incognitoRate.covered ?? 0) === 0 &&
    (data?.platformCoverage.covered ?? 0) === 0 &&
    (data?.countryCoverage.covered ?? 0) === 0 &&
    (data?.deviceCoverage.covered ?? 0) === 0
```

Add the memo and the description beside `platformData`:

```tsx
  const deviceData = useMemo(() => toBarData(data?.devices ?? [], DEVICE_LABELS), [data])
  // The coverage sentence this figure ships with, per the project rule that a
  // statistic states its denominator in prose rather than in a tooltip. It says
  // *why* the number is what it is as well as what it is: a listener seeing 3.1%
  // has to be told that is a property of how the fact is gathered, not a bug.
  const devicesDescription =
    (data?.deviceCoverage.covered ?? 0) === 0
      ? 'Encore was not watching your player during this range, so it does not know what any of it played on.'
      : `Known for ${formatRatio(data?.deviceCoverage.covered ?? 0, data?.deviceCoverage.total ?? 0)} of your listening in this range — ${formatPlural(data?.deviceCoverage.covered ?? 0, 'play')} of ${formatCount(data?.deviceCoverage.total ?? 0)}. Encore learns this by watching your player, so only listening it saw live can be counted.`
```

Replace the standalone Countries `ChartCard` with a second two-column grid holding the device chart and Countries:

```tsx
          <div className="grid gap-4 lg:grid-cols-2">
            <ChartCard title="What you played on" description={devicesDescription}>
              {context.isPending ? (
                <ChartLoading label="Loading devices" />
              ) : (
                <BarChart
                  data={deviceData}
                  label="Plays by device"
                  valueName="plays"
                  /* SERIES_LIMIT is 4, so slots wrap; one bar chart is one
                     colour whatever it ranks, and two cards on the same hue
                     cost nothing because neither is read against the other. */
                  slot={3}
                  busy={context.isFetching}
                  emptyDescription="No play in this range recorded a device."
                />
              )}
            </ChartCard>

            <ChartCard title="Countries" description={countriesDescription}>
              {context.isPending ? (
                <ChartLoading label="Loading countries" />
              ) : (
                <BarChart
                  data={countryData}
                  label="Plays by country"
                  valueName="plays"
                  slot={2}
                  busy={context.isFetching}
                  emptyDescription="No play in this range recorded a country."
                />
              )}
            </ChartCard>
          </div>
```

- [ ] **Step 5: Correct the empty state**

Replace the `noContext` `EmptyState`'s `description` with:

```tsx
              <>
                How a play ended, and whether it was offline or in a private session, are recorded
                only by Spotify's extended streaming history export — bring one in from{' '}
                <Link to="/imports" className="text-lamp hover:underline">
                  Imports
                </Link>
                . Shuffle and the device you played on can also be filled in as you listen, by the
                now-playing poller — see{' '}
                <Link to="/settings" className="text-lamp hover:underline">
                  Settings
                </Link>{' '}
                for whether this instance runs it.
              </>
```

- [ ] **Step 6: Run the web suite and the type check**

```bash
cd web && npx tsc --noEmit && npm run lint && npm run test && cd ..
```

Expected: PASS. If a pre-existing habits test now fails on the old empty-state string, it is asserting copy this task deliberately changed — update that assertion to the new sentence rather than reverting the copy.

- [ ] **Step 7: Commit**

```bash
git add web/src/pages/Habits.tsx web/src/test/habits.test.tsx
git commit -m "$(cat <<'MSG'
Web: what you played on, and four sentences that stopped being true

This page said, in four places, that shuffle is recorded only by Spotify's
extended streaming history export. Since Phase 3c the now-playing poller
observes it and the sync path attaches it, so the sentence sent somebody off to
import a file to see a number they were already accumulating.

The device breakdown is its own chart rather than a row in the platform one.
platform holds an export's free text and device_type holds Spotify Connect's
vocabulary; a reader who saw "Speaker" and "web_player" ranked together would be
reading two incompatible answers as one figure.

It states its denominator in prose, with singular and plural, because a listener
seeing 3.1% needs to be told that is a property of how the fact is gathered
rather than a bug — and at zero it says Encore was not watching, which is a
different sentence from "you played nothing".

deviceCoverage joins the noContext gate. Without it, a sync-only instance with
the poller enabled — zero on all six export columns, full coverage on the one
the poller fills — would have had the whole block collapsed to an empty state
with its only real chart inside it.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
MSG
)"
```

---

## Task 6: The document sweep

**Adding a capability has made documents stale in every phase of this project: 2e-ii found five, 3a found nine, 3b found a table stale by several phases.** This task is that sweep, with the files named. Every one of these was located by grep at planning time; run the grep again before editing, because Tasks 1–5 have moved lines.

**Files:**
- Modify: `docs/api.md`
- Modify: `docs/feature-parity.md`
- Modify: `docs/configuration.md`
- Modify: `docs/architecture.md`
- Modify: `README.md`
- Modify: `docs/design/2026-07-29-phase-3-write-and-live-design.md`
- Modify: `docs/design/2026-07-29-spotify-api-expansion-overview.md`

**Interfaces:** none. This task produces no code.

- [ ] **Step 1: Run the greps and list what they hit**

```bash
grep -rn "currently-playing" --include='*.md' --include='*.go' --include='*.ts' --include='*.tsx' --include='*.yml' . | grep -v docs/superpowers/plans
grep -rni "shuffle\|lower-fidelity\|extended export records\|only by Spotify" --include='*.md' docs/ README.md | grep -v docs/superpowers/plans
grep -rn "extended-export importer\|written only by the extended" --include='*.go' internal/
```

Expected from the first: **zero hits outside `docs/superpowers/plans/`**. Any hit is a document still describing an endpoint Encore no longer calls. Fix each one before continuing.

- [ ] **Step 2: `docs/api.md`**

Three edits.

**(a)** In the Statistics table, the `/api/stats/context` row gains the two new keys. Append to that row's description cell:

```
`devices` and `deviceCoverage` break the range down by Spotify Connect device type — `speaker`, `smartphone`, `computer` — and are **not** `platforms`, which holds an export's own free-text platform string. The two have opposite lineages: `platforms` is written only by an extended-export import, `devices` only by the now-playing poller's backfill.
```

**(b)** In `### Playback context: what you were playing from`, the paragraph at `:375` beginning "`endReasons`, the two rates, `platforms` and `countries` are partial…" gains a following paragraph:

```markdown
`shuffleRate` and the new `devices` / `deviceCoverage` pair are the exception to
that lineage, and the only one. When `ENCORE_NOWPLAYING_INTERVAL` is set, the
now-playing poller records the shuffle state and the Spotify Connect device it
sees while somebody is listening, and the sync path attaches them to the plays
they belong to afterwards. So a sync-only instance can report a real shuffle
share and a real device breakdown with every other figure here at zero, and an
import-only instance reports the reverse. **No match leaves both columns NULL**
— never `false`, and never a device of "unknown". A listen Encore did not
observe is absent from `covered`, not counted as un-shuffled.

The match is a temporal one and is honest about being fuzzy: an observation is
attributed to a play when it falls between the play's start and its end plus a
sixty-second tolerance, and the most recent match wins. Two plays of the same
track back to back can therefore hand the first one evidence gathered during the
second; they share a device and a shuffle setting unless the listener changed one
inside that minute.
```

**(c)** In `## Now playing`, every mention of the endpoint. The poller now reads `GET /v1/me/player`. Add, beside the existing `**The poller never writes a listen.**` closing constraint:

```markdown
**The poller never *creates* a listen, and now annotates existing ones.** `GET
/me/player/recently-played` remains the only path that creates a row in
`listens`. What the poller sees is written to a separate short-lived log and
attached afterwards by a statement that is an `UPDATE` with a two-column `SET`
list: it cannot create a row, cannot move one, and cannot overwrite a value an
import already supplied. Running it twice changes nothing. See
[configuration.md](configuration.md#now-playing).
```

- [ ] **Step 3: `docs/feature-parity.md`**

Two edits.

**(a)** The `Playback-context statistics` row (currently line ~98). Its final sentence reads *"Only extended-export rows carry these columns, so each figure reports its own denominator."* Replace with:

```
Six of the eight columns are carried only by extended-export rows. Shuffle and the Spotify Connect device are the exception: when `ENCORE_NOWPLAYING_INTERVAL` is set, the now-playing poller observes both while somebody is listening and the sync path attaches them to the plays they belong to, so a sync-only instance reports a real shuffle share and a real device breakdown with every other figure at zero. Each figure reports its own denominator, per column and never per source, because the two lineages are independent. A play Encore did not observe stays NULL rather than being counted as un-shuffled.
```

**(b)** Under `## Known gaps`, immediately after the "Live sync cannot record how long a track was played" bullet (~line 180), add:

```markdown
- **Live sync knows how a play happened only while the now-playing poller is
  running.** `/me/player/recently-played` reports what was played and when, and
  nothing about shuffle, device, country, or how the play ended. Since Phase 3c,
  two of those — shuffle and the Spotify Connect device — are filled in for plays
  that happened while `ENCORE_NOWPLAYING_INTERVAL` was set, by matching what the
  poller saw against the plays that arrive afterwards. **Everything before the
  poller was switched on, and everything on an instance that never switches it
  on, stays unknown for ever.** Country, incognito, offline and the reason a play
  ended are not observable from any endpoint at all and remain extended-export
  only. Unknown is stored as NULL and reported as coverage, never as `false`: the
  statistics say what share of a range they could see.
```

- [ ] **Step 4: `docs/configuration.md`**

In `## Now playing`, the `ENCORE_NOWPLAYING_INTERVAL` description currently contains **"The poller never writes to your listening history"**. That sentence is now imprecise and must be replaced rather than deleted:

```
The poller **never creates a listen**: `GET /me/player/recently-played` remains the only path that adds a row to your history. It does now **annotate** plays that already exist — since Phase 3c it records the shuffle state and the Spotify Connect device it sees, and the sync path attaches them afterwards to the plays they belong to, which is what fills the shuffle share and the "what you played on" breakdown on Habits for listening that was never exported. That annotation is a two-column `UPDATE`: it cannot create a row, cannot move one, cannot duplicate one, and cannot overwrite a value an import supplied. Running it twice changes nothing. Listening from before the poller was switched on is never annotated, because nobody was watching, and stays NULL rather than being reported as un-shuffled. The observations themselves are deleted after 24 hours.
```

Add, after the cost table:

```markdown
Turning this on costs no additional Spotify requests beyond the table above.
The poller reads `GET /v1/me/player`, one request per account per tick, which
carries the playing item, the device and the shuffle state together; there is no
second call for the annotation.
```

**Confirm no new key was added.** Run:

```bash
git diff main -- internal/config/config.go | grep -c 'ENCORE_[A-Z0-9_]*'
go test -count=1 ./test/deploy/
```

Expected: the grep counts only lines belonging to the `ENCORE_NOWPLAYING_INTERVAL` comment Task 1 rewrote, and `./test/deploy/` PASSES. If it fails, an `ENCORE_` literal was introduced somewhere and the five-places rule applies after all — stop and go back.

- [ ] **Step 5: `docs/architecture.md` and `README.md`**

`docs/architecture.md`: the `**Now playing**` loop description at ~line 192 says it "checks each connected account's player every `ENCORE_NOWPLAYING_INTERVAL`". Append: *"— reading `GET /v1/me/player`, recording what it sees for the dashboard card and appending it to a short-lived log the sync path uses to fill in shuffle and device on the plays it belongs to."* Also check the `**Sync**` entry at ~line 188 and the loop count in the file's opening summary; Phase 3b's review found a table stale by several phases, so count the loops rather than trusting the number written there.

`README.md`, in `## Known limitations`: the "Live sync records a play's length as the track's full duration" bullet stays exactly as written — it is about `ms_played` and is unaffected. Add a new bullet immediately after it:

```markdown
- **Live sync only knows how a play happened while the now-playing poller is
  running.** The recently-played endpoint reports what was played and when, and
  nothing about shuffle, device, country or how the play ended. If you set
  `ENCORE_NOWPLAYING_INTERVAL`, Encore watches your player and fills shuffle and
  the device in afterwards, for plays that happen from then on. Everything
  before that — and everything on an instance that leaves the poller off —
  stays unknown, and the statistics say so rather than reporting it as "not
  shuffled". Country, incognito, offline and the end reason are not readable
  from any Spotify endpoint at all and still need an extended export.
```

Check the two neighbouring bullets while you are there: **"Playback control is not implemented"** and **"Now playing cannot be shared"** both stay exactly as written. Playback *control* is a different thing from playback *context* and Phase 3's own design says the edit must not conflate them.

- [ ] **Step 6: The design documents' own status**

`docs/design/2026-07-29-phase-3-write-and-live-design.md`:

- Under `## 2. Feature 8 — now playing`, add a status line directly beneath the heading:

```markdown
> **Status:** §2.1–§2.3 shipped as Phase 3b (merged `2cca3a9`). §2.4 and §2.5
> shipped as Phase 3c, on the authorisation §2.5 records in advance.
```

- §2.4's sentence *"When sync ingests a listen, it looks for an observation of the same track… and fills `shuffle` and `platform` from it"* is the one place this plan deviated from the spec. Correct it in place:

```markdown
**As built, it fills `shuffle` and a new `device_type` column — not `platform`.**
`listens.platform` holds an export's free text (`"Android OS 10 API 29 (samsung,
SM-G970F)"`, `"web_player"`) and `PlatformFamily` is a substring classifier built
for those shapes; Spotify Connect's `device.type` is a different vocabulary
(`"Computer"`, `"Smartphone"`, `"Speaker"`). Writing the second into the first
would have made every historical platform figure change meaning without changing
shape, which is the same error as letting "unknown" and "false" share a column.
`device_name` is kept in the observation log for a day and never copied onto
`listens`.
```

- §2.4's DDL block: annotate it as the design's shape, with `migrations/00018_playback_observations.sql` as what shipped (it adds two CHECK constraints and `listens.device_type`).

- §4's two documentation bullets: mark them done, and record that the "known gap" they refer to did not previously exist in `README.md` or `docs/feature-parity.md` at all — so Phase 3c *added* the statement rather than moving one.

`docs/design/2026-07-29-spotify-api-expansion-overview.md`:

- Rows `8a` and `8b` (lines ~83–84) collapse into one. `8a` says `GET /me/player/currently-playing`, which Encore no longer calls at all:

```markdown
| 8 | Now playing, and the shuffle/device backfill | `GET /me/player` | `user-read-playback-state` | 3b (card) and 3c (backfill) — shipped |
```

- The operations count at lines 17–18 says "19 operations across 18 paths, as of Phase 3b". Phase 3c **swaps** one path for another and adds none, so the totals are unchanged — but the file says explicitly that stale counts get corrected here, so **recount from the table rather than assuming**, and update the "as of" phase to 3c.

- The share table at lines ~168–171 lists `Shuffle share, skip rate` and `Platform, country, offline, incognito` as `No`. Add a row so the new figure is named rather than merely inheriting the rule:

```markdown
| Device breakdown | No |
```

and check that the closing sentence *"Device and country reveal what hardware somebody owns and where they have travelled"* still reads correctly beside it — it does, and it is now literally true of a stored column rather than only of an export's.

- [ ] **Step 7: Verify the whole tree**

```bash
export PATH="$PATH:$(go env GOPATH)/bin"
gofmt -l $(git ls-files '*.go'); go vet ./...; staticcheck ./...
go test -count=1 ./...
go test -tags=integration -count=1 -p 1 ./test/integration/
cd web && npx tsc --noEmit && npm run lint && npm run test && cd ..
git diff --exit-code docker-compose.portainer.yml && echo "portainer unchanged, as expected"
for f in $(git diff --name-only main); do perl -0777 -ne 'print "NULs in '"$f"': ", tr/\0//, "\n"' "$f"; done
```

Expected: everything PASSES; `docker-compose.portainer.yml` is unchanged because no configuration key was added; every NUL count is 0.

- [ ] **Step 8: Commit**

```bash
git add docs/api.md docs/feature-parity.md docs/configuration.md docs/architecture.md README.md \
        docs/design/2026-07-29-phase-3-write-and-live-design.md \
        docs/design/2026-07-29-spotify-api-expansion-overview.md
git commit -m "$(cat <<'MSG'
Docs: a limitation that was never written down, now written down and half closed

Phase 3's design calls the live-sync shuffle/device gap "the documented
limitation this addresses". It was documented nowhere: neither README's known
limitations nor feature-parity's known gaps ever carried a bullet for it. So
this states it, and states which half Phase 3c closed and which half no endpoint
can ever close.

Shuffle and the Spotify Connect device are now filled in for plays that happen
while the poller is running. Country, incognito, offline and the reason a play
ended are not readable from any Spotify endpoint and still need an extended
export. Everything from before the poller was switched on stays unknown, and
that is stated as unknown rather than reported as "not shuffled".

Playback *control* stays declined and its two bullets are untouched. Phase 3's
own design says this edit must not conflate the two, and control and context are
different things.

Every document that named /v1/me/player/currently-playing named an endpoint
Encore no longer calls. The phase map's 8a/8b split described work that has
merged into one call.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
MSG
)"
```

- [ ] **Step 9: Open the PR and read the race detector**

`go test -race` does not run locally (no gcc) and CI does not run it on branch pushes. This phase adds no new concurrent construct — the poller's semaphore and `atomic.Int64` are Phase 3b's and unchanged — but it adds a second write inside `check`'s goroutine and a new statement inside the sync poller's per-account path.

```bash
git push -u origin <branch>
gh pr create --fill
gh pr checks --watch
```

Read the `unit` job's output specifically. All nine jobs must be green before this is done.

---

## Self-Review

**1. Spec coverage.**

| Spec requirement | Task |
|---|---|
| §2.4 — `playback_observations` table, the shape in the DDL block | Task 2 |
| §2.4 — rows expire after 24 hours | Task 2 (`ObservationRetention`, the reaper) and Task 3 (`TestAReapedObservationCanNeverMatch`) |
| §2.4 — window `[played_at, played_at + duration_ms + tolerance]`, most recent match | Task 3 (`backfillPlaybackContextSQL`) |
| §2.4 — no match leaves both NULL, "exactly as today" | Task 3 (`TestTheBackfillNeverInventsAFalse`) |
| §2.4 — fills `shuffle` and `platform` | **Deviation, recorded.** `shuffle` and a new `device_type`; `platform` untouched. Argued under "Why `device_type` and not `platform`", and corrected in the design document itself in Task 6. |
| §2.5 — tolerance is a named constant with reasoning in a comment, not a literal in a query | Task 3 (`ObservationTolerance`) |
| §2.5 — ships after the live card, as a separate commit | Six commits, all after `2cca3a9` |
| §3 test table — "Backfill window: an observation inside the window fills `shuffle`; one outside it leaves NULL" | Task 3, `TestAnObservationInsideTheWindowFillsTheListenAndOneOutsideLeavesItNull` |
| §3 test table — "Observation expiry: rows older than 24 h are removed and never match" | Task 3, `TestAReapedObservationCanNeverMatch` |
| §3 test table — "No phantom listens" | Task 3, `TestRunningTheBackfillTwiceChangesNothingAndCreatesNothing`; Task 2 keeps the import-graph test unedited |
| §4 — `docs/feature-parity.md` limitation, `README.md` limitations | Task 6 |
| §5 — `repeat_state` and volume stored by nothing | Task 1 decodes `RepeatState` and stores it nowhere; volume is untouched |
| §5 — playback control still declined | No scope change; Task 6 leaves both control bullets exactly as written |

**Gaps I am recording rather than filling.** Nothing in §2.4 or §2.5 is unplanned. Two things adjacent to them are deliberately out of scope: the `docs/configuration.md` quota table is unchanged because the request count is unchanged, and no metric is published for the backfill, because Phase 3b declined metrics for this loop on the grounds that a failure is visible in words to the person it concerns — and a backfill that annotates nothing is indistinguishable, from the outside, from one that had nothing to annotate.

**2. Placeholder scan.** Two `t.Skip` markers exist, in Task 3 Step 6 and Task 4 Step 1. Both are deliberate and both are accompanied by the full behaviour to assert, the exact file to model on, and an explicit instruction that the step is not finished until the skip is gone plus a verification command that fails on `SKIP`. They are there because the correct model is an existing rig in a file the implementer must read; inventing a second rig here would drift from the one every neighbouring test uses. No "TBD", no "add appropriate error handling", no "similar to Task N". One instruction says "check the existing logger field name before writing this" — that is a lookup, not a placeholder, and the surrounding code is complete.

**3. Type consistency.**

- `Client.Player` — defined Task 1, consumed by `SpotifyAPI.Player` (Task 1) and `w.dep.Spotify.Player` (Task 1). Consistent.
- `Playback.ShuffleState *bool` — Task 1; read by `logEntry` (Task 2) as `pb.ShuffleState`; stored as `domain.PlaybackObservation.Shuffle *bool` (Task 2); written to a nullable column (Task 2); read by the backfill's `m.shuffle IS NOT NULL` (Task 3). Pointer-ness survives every hop.
- `domain.PlaybackObservation{TrackID, ObservedAt, Shuffle, DeviceType, DeviceName}` — Task 2; used with those exact field names in `logEntry` (Task 2), `PlaybackObservations.Log` (Task 2), and the Task 3 integration helpers.
- `accounts.ObservationRetention` — Task 2; consumed by `cmd/encore-worker` (Task 2) and `TestAReapedObservationCanNeverMatch` (Task 3).
- `listens.ObservationTolerance`, `listens.BackfillLookback`, `Repo.BackfillPlaybackContext(ctx, q, userID, now)` — Task 3; consumed by `internal/sync/backfill.go` (Task 3) and the integration rig (Task 3), which calls it with exactly four arguments.
- `stats.DeviceFamily`, `stats.DeviceUnknown` — Task 4; used in `context.go`'s scan loop and `device_test.go`, both Task 4.
- `PlaybackContext.Devices` / `.DeviceCoverage` → `PlaybackContextResponse.Devices` (`json:"devices"`) / `.DeviceCoverage` (`json:"deviceCoverage"`) → `types.ts` `devices` / `deviceCoverage` → `Habits.tsx` `data?.devices`, `data?.deviceCoverage.covered`, `data?.deviceCoverage.total` → `habits.test.tsx` `devices:`, `deviceCoverage:`. Five layers, one spelling.
- `nowplaying.Store{NowPlaying, Observations}` — Task 2; its four methods match `Observations` exactly, including `Log`'s signature `(ctx, q, userID, domain.PlaybackObservation) error`, which matches `PlaybackObservations.Log`.

**One inconsistency found and fixed while reviewing:** the plan originally had Task 4's integration test asserting `Devices` sorted by plays without saying so, while `sortedSlices` sorts descending by plays then by key. The expected value in Task 4 Step 1 now spells out the resulting order (`speaker` before `computer`) so a change to the sort breaks the test rather than being absorbed by it.
