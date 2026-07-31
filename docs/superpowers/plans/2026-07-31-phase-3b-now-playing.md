# Phase 3b — Now Playing

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Show a listener what they are playing right now, polled from Spotify by an opt-in background loop that can never take anything else down with it.

**Architecture:** A third request class in `internal/spotify` — `classNowPlaying` — with a limiter of its own, so a 429 on the poll pauses the poll and nothing else. A new worker loop in `internal/nowplaying` calls `GET /v1/me/player/currently-playing` for each connected account that granted `user-read-playback-state`, and writes one row per user to a new `now_playing` table. It imports nothing that can write a listen. `GET /api/nowplaying` reads that row and makes no Spotify request of its own; a card on the dashboard polls it. Every state the card can be in has its own sentence, and "nothing is playing" and "Encore does not know" are structurally different answers — the second is a null observation, not a state value.

**Tech Stack:** Go 1.26, pgx/v5, PostgreSQL 17, React 19 + TypeScript + Vite + TanStack Query v5. **No new module and no new npm package.**

**Spec:** [`docs/design/2026-07-29-phase-3-write-and-live-design.md`](../../design/2026-07-29-phase-3-write-and-live-design.md) **§2**. §1 shipped as Phase 3a and merged at `c939306`.

**Task count: 7.**

---

## What this plan does not build, and why that is the spec's own decision

**§2.4 and §2.5 — the shuffle/platform backfill — are not in this plan.** They become Phase 3c.

The spec pre-authorises this in §2.5, in advance and in writing:

> So it ships **after** the live card, as a separate commit, and if it fights back it gets cut without losing the feature. That is a design decision recorded in advance rather than a concession made under pressure later.

Three further reasons, all discovered while planning rather than assumed:

1. **It needs a different endpoint.** `GET /v1/me/player/currently-playing` — the endpoint this phase polls — does not reliably carry `shuffle_state`, and does not carry `device` on every account. `GET /v1/me/player` does. A backfill built on this phase's poll would fill `shuffle` from a field that is often absent, which is worse than leaving it NULL.
2. **It needs a different table.** `playback_observations` is keyed `(user_id, track_id, observed_at)` and expires after 24 hours, because a fuzzy temporal join needs a *log*. This phase's `now_playing` is keyed `(user_id)` and is overwritten every tick, because a live card needs a *latest*. One table cannot be both: a log has no "current row" and a latest has no history to join against.
3. **It is four more tasks** — the table and its reaper, the poller writing observations, the join inside `internal/sync/ingest.go`, and the documentation that says the limitation is closed. Eleven tasks is past the point where a reviewer can hold one in their head.

Phase 3c takes migration `00018_playback_observations.sql`, a `sup.Add("nowplaying-reaper", …)` beside the existing session reaper, and the named tolerance constant §2.5 asks for. **Do not start it inside this plan.**

Also deliberately out of scope, recorded so nobody adds them:

- **`repeat_state`, volume, and playback control.** §5 of the spec declines all three. `user-modify-playback-state` is never requested.
- **A device *type* column.** `device_type` is not stored. Only `device_name` is, because only the name is rendered; the type exists for Phase 3c's `platform` backfill and belongs to its table.
- **Metrics.** The recently-played poller publishes Prometheus counters. This one does not. A presence poll that fails is visible on the card, in words, to the person it concerns.
- **The card on an empty dashboard.** `Dashboard.tsx` has three whole-page early returns for an account with no listens at all; the card renders only in the populated body. An account with no history that is playing something right now sees the import prompt instead, which is the more useful thing to show them.

---

## The property that defines this whole plan

**The now-playing poll is the least important thing Encore does and by far the most frequent, so it must not be able to take down the rest.**

At 30 s across five accounts it is roughly 14,400 requests a day. Today every non-interactive request draws on one limiter, and `internal/spotify/client.go:435` records a 429 on it through `onPause`, which writes `app_settings.spotify_paused_until`. That single row 409s `POST /api/sync/now` for **every** user (`internal/httpapi/me.go:200-207`) and, at the worker's next construction (`internal/worker/pause.go:62-70`), halts enrichment, the recently-played poller and all five library enumerations.

So a 429 on the least important request in the system would today stop the most important ones.

**The fix is structural, not conventional** — see the next section. Task 1 exists only to build it, and it is Task 1 rather than Task 4 so that no later task can be tempted to route the poll through the budget that already exists.

---

## Decisions taken here, so they are not relitigated mid-execution

### The spec says the poller draws on the interactive budget. It does not. This is the resolution.

§2.1 says:

> It draws on the **interactive budget**, so a catalogue 429 cannot stall it and it cannot stall enrichment.

The first clause is right, the second is right, and the conclusion is wrong — and the code already says why. `internal/spotify/client.go:83`, on the `signin` limiter:

> Nothing a background worker does may take authentication offline.

The now-playing poller *is* a background worker. Putting 14,400 daily requests through the limiter authentication depends on means a single 429 on a presence poll pauses **sign-in** for whatever `Retry-After` Spotify names, which for an exhausted daily quota is most of a day. That locks every listener out of an instance whose listening data is perfectly fine. It is the same failure the catalogue budget was split away from `signin` to prevent, arriving from the other direction.

**So the poller gets a third budget of its own.** `signin` is the precedent: a limiter that is never shared, never restored from a recorded pause, and whose 429 is nobody else's business.

### What makes it structural rather than conventional

A `bool` cannot name three things. `request.interactive bool` is replaced by `request.class requestClass`, and **one predicate decides both consequences of a 429**:

```go
func (k requestClass) instanceWide() bool { return k == classCatalogue }
```

- `budget()` is a total switch over the class. There is no way to write a now-playing request that lands on the catalogue limiter without editing the constant it names.
- `classify()` calls `onPause` **only** when `r.class.instanceWide()`, and returns immediately with a `*PausedError` **exactly when it does not**. One predicate, both branches, so a class cannot end up recording an instance-wide pause while also refusing to wait for it, or the reverse.
- A class added later and not mentioned in `instanceWide()` defaults to `false` — the safe direction. A new caller's mistake costs a private pause, never a global one.

Contrast with what would have been conventional: a second boolean, or a comment on `CurrentlyPlaying` saying "remember to pass `interactive: true`". Both can be got wrong silently by a copy-paste. This cannot: getting it wrong means naming `classCatalogue` in the one place the class is set, which is what `TestNowPlayingRateLimitTouchesNoOtherBudget` reads.

### A 429 on this path is never recorded, even though it is true

An exhausted daily quota is an instance-wide fact, and the poller would learn it first — it makes more requests than anything else. Recording it from here is still refused, because that is precisely the mechanism by which the least important request takes down the rest. The catalogue limiter discovers the same fact on its own next request, at a cost of one request; and `internal/worker/pause.go`'s restart persistence exists to protect a *worker's* hundreds of enrichment calls, not one presence poll.

The cost of not recording it: a restart during a now-playing pause forgets the pause, and one wasted request follows. One. That is the whole downside, and it is smaller than the upside by four orders of magnitude.

### The poller runs in `encore-worker`; the endpoint is served by `encore-api`. That is why there is a migration.

These are separate processes in separate containers. An in-memory observation in the worker is unreachable from the API, so the only place the two can meet is the database. `00017_now_playing.sql` holds **one row per user** — the last observation and the last check. Not a log: a log is Phase 3c's `playback_observations`, with a different key and a 24-hour lifetime, and merging them would give the card a table it has to run `DISTINCT ON` against and the backfill a table that overwrites its own evidence.

### "We don't know" is a null observation, not a state value

The spec's most easily-conflated pair is *nothing is playing* and *Encore has not seen what you are playing*. They are kept apart structurally rather than by discipline:

- `NowPlayingResponse.Observation` is `nil` — there has never been a successful check.
- `Observation.State == "idle"` — a check succeeded and Spotify answered 204: nothing is playing.

A client branching on `observation.state` cannot reach the first case at all, because there is no observation to read a state from. On disk the same distinction is `state = 'unknown'` with `observed_at IS NULL`, and a CHECK constraint makes the pair inseparable.

### `user-read-playback-state` is already granted, and nothing about consent changes

Verified: `internal/config/config.go:560-561` has it in `DefaultScopes()`, added in Phase 2a. `internal/config/config_test.go`'s `TestDefaultScopesAreTheEightReadScopes` and `TestDefaultScopesGrantNoWriteAccess` pin the eight. `internal/httpapi/me.go:76` computes `MissingScopes(creds.Scopes, config.DefaultScopes())`, so an account whose grant predates Phase 2a already sees it in `spotify.missingScopes`, and `web/src/components/layout/ReconsentBanner.tsx:35` already explains it as `"show what's playing now"` — a promise made in Phase 2a that this phase finally makes true.

**Do not change `DefaultScopes()`. Do not change `SCOPE_EXPLANATIONS`. Do not change `ReconsentBanner.tsx`.** The banner's closing sentence — *"None of these let Encore change anything on your Spotify account"* — stays true because this phase adds no scope at all.

### A 403 from this endpoint never parks an account

`internal/sync/account.go:296-314` states the rule. `user-read-playback-state` is an optional read scope in exactly that sense: a 403 means only that this grant does not carry it, and parking the account would stop ingesting a listening history that reads perfectly.

Made structural: `nowplaying.Deps` names `NowPlaying *accounts.NowPlaying`, **not** `*accounts.Repo`. The poller therefore has no handle on `accounts.Credentials` and cannot call `MarkNeedsReauth` even by accident. The no-retry half is already structural: `Client.classify` wraps every non-429 4xx in `retry.Stop`.

### The endpoint is `/v1/me/player/currently-playing`, and `device` may not arrive

The spec's §2 header describes `GET /v1/me/player`. This phase polls `GET /v1/me/player/currently-playing`, which is narrower and cheaper and returns everything the card renders. Spotify documents the same response object for both, but `/currently-playing` is observed to omit `device` on some clients.

**Treat `device` as optional.** When it is absent the card renders no device clause at all — not "unknown device". Nothing else in the plan depends on it. Do **not** switch to `/me/player` to get it: that endpoint is what Phase 3c needs for `shuffle_state`, and reaching for it now would put this phase's poll on a payload it does not use.

`additional_types=episode` **is** sent. Without it Spotify returns `item: null` with `currently_playing_type: "episode"` for a podcast, and a named episode would render as "something Encore cannot identify".

---

## Global Constraints

- **No new Go module dependency and no new npm dependency.** `go.mod`, `go.sum` and `web/package.json` are byte-identical at the end. Phase 3a added `golang.org/x/image`; nothing further. CI's `lint` job diffs `go mod tidy` output.
- **One new configuration key: `ENCORE_NOWPLAYING_INTERVAL`. Unset means the poller never runs.** Not "defaults to off" — absent from the environment must mean the loop returns before it lists a single account.
- **That key lands in FIVE places, in ONE commit:** `internal/config/config.go`, `docker-compose.yml`, `.env.example`, `docs/configuration.md`, and the **generated** `docker-compose.portainer.yml` (regenerate with `./scripts/gen-portainer-stack.sh`; CI's `lint` job diffs the committed copy). `test/deploy/composeenv_test.go:14` regexes `config.go` for `ENCORE_[A-Z0-9]+(_[A-Z0-9]+)*` **including inside comments**, so the moment the literal string appears anywhere in `config.go` the compose file and `.env.example` must already have it. The five cannot be staged across commits.
- **`docs/configuration.md` is guarded by nothing.** Nothing in CI reads it. It is the one of the five that will be forgotten.
- **A 429 on the now-playing path must never reach `onPause`.** Pinned by `TestOnlyACatalogueRateLimitPausesTheInstance` and `TestNowPlayingRateLimitTouchesNoOtherBudget` in Task 1, modelled on Phase 3a's `TestPlaylistWritesDrawOnTheSignInBudget`.
- **The poller never writes a listen.** `GET /me/player/recently-played` stays the sole ingestion path (spec §2.2). Pinned twice: an import-graph test in Task 4 and a row-count test in Task 4's integration file.
- **A 403 never reaches `markNeedsReauth` and is never retried.** See the decision above.
- **`user-read-playback-state` already shipped in `DefaultScopes()`.** Do not touch `DefaultScopes()`, `SCOPE_EXPLANATIONS` or `ReconsentBanner.tsx`.
- Next migration number is **`00017_`**. `00015` and `00016` were Phase 3a. Re-check `ls migrations/` before writing the file. House style is goose `Up` **and** `Down`, both directions working, with the reasoning in comments including what was considered and rejected.
- **Anything reaching a `text` column goes through `store.Truncate`** (`internal/store/store.go:193`, rune-safe, appends `...`). In this plan that is four columns: `now_playing.title`, `.artist`, `.device_name`, and nothing else — all of them are Spotify's own strings.
- **`internal/httpapi` contains no SQL and never imports pgx.** It reaches repositories through the narrow interfaces in `server.go`; a new `nowPlayingStore` interface must be satisfied by the concrete repository as written.
- **The DTO exists in three places and they are kept in step by hand:** `internal/httpapi/dto.go`, `web/src/lib/types.ts`, `docs/api.md`. Change one, change all three.
- **Write the character, never the escape.** Phase 3a shipped a Critical that rendered a literal `…` on screen because the escape sat in bare JSXText, which is valid TypeScript that compiles and passes every test. Type `…`, `—` and `’` directly. Every copy string in this plan is written with real characters; copy them verbatim.
- Test DB on port **5433**, not 5432. `make` is **NOT installed** — run the commands directly.
- `go test -race` will **NOT** work locally: no gcc. Omit `-race`. CI runs it, on `pull_request` and `workflow_dispatch` only — **not on branch pushes**, so a race is invisible until the PR opens. This phase adds one concurrent construct (the poller's per-account semaphore and `atomic.Int64`), which is exactly the shape a race detector is for. Open the PR and read the `unit` job before calling this done.
- Tagged suites share one database: `-p 1`, one package at a time.
- staticcheck at `$(go env GOPATH)/bin`; `export PATH="$PATH:$(go env GOPATH)/bin"` first.
- **CI's `web` job runs `npm run test`** — Phase 3a added the step. Every copy assertion in this plan is therefore guarded. The suite is green at `c939306`.
- **`vi.spyOn(window.sessionStorage, …)` silently does nothing** in this project's jsdom. Spy on `Storage.prototype` instead. (No task here needs it; recorded so nobody rediscovers it.)
- **NUL check every file you write:** `perl -0777 -ne 'print "NULs: ", tr/\0//, "\n"' <file>` — expect 0.
- Commit style `Area: lowercase summary`, body explaining *why*, ending `Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>`. Stage paths explicitly; never `git commit -a`.
- **Every test in this plan carries a "Fails when:" line** naming the exact change that breaks it. If you add a test of your own and cannot write that line, the test cannot fail and must be replaced. For a poller in particular: never assert an interval *value*. Assert that polling **stops** — that is the property.

---

## File Structure

| File | Responsibility |
|---|---|
| `internal/spotify/client.go` | **Modify.** `requestClass`, `instanceWide()`, a third limiter, `request.status`. |
| `internal/spotify/oauth.go` | **Modify.** `token()` takes a class instead of a bool. |
| `internal/spotify/playlists.go` | **Modify.** Five `interactive: true` become `class: classInteractive`. |
| `internal/spotify/nowplaying.go` | **Create.** `Playback`, `PlaybackItem`, `Device`, `Show`, `CurrentlyPlaying`. |
| `internal/spotify/nowplaying_test.go` | **Create.** The budget-isolation tests. |
| `internal/config/config.go` | **Modify.** `NowPlaying`, `optionalDuration`, `Redacted()`. |
| `docker-compose.yml`, `.env.example`, `docs/configuration.md`, `docker-compose.portainer.yml` | **Modify.** The other four of five places. |
| `migrations/00017_now_playing.sql` | **Create.** One row per user. |
| `internal/domain/nowplaying.go` | **Create.** `NowPlaying`, `PlaybackState`, `PlaybackItemKind`. |
| `internal/store/accounts/nowplaying.go` | **Create.** `Get`, `Record`, `RecordFailure`, `ListDue`. |
| `internal/store/accounts/accounts.go` | **Modify.** `Repo.NowPlaying`. |
| `internal/nowplaying/nowplaying.go` | **Create.** The loop, the scope skip, `observe`. |
| `internal/nowplaying/nowplaying_test.go` | **Create.** |
| `cmd/encore-worker/main.go` | **Modify.** `sup.Add("nowplaying", …)`. |
| `internal/httpapi/nowplaying.go` | **Create.** `handleNowPlaying`. |
| `internal/httpapi/dto.go`, `router.go`, `server.go` | **Modify.** DTO, one route, one narrow interface. |
| `web/src/lib/types.ts`, `format.ts`, `query.ts` | **Modify.** DTO mirror, `intervalPhrase`, `qk.nowPlaying`. |
| `web/src/pages/Dashboard.tsx` | **Modify.** The card and `nowPlayingPollInterval`. |
| `web/src/pages/Settings.tsx` | **Modify.** What the instance is configured to do. |
| `web/src/test/nowplaying.test.tsx` | **Create.** Every word of copy. |
| `test/integration/nowplaying_test.go` | **Create.** |
| `docs/api.md`, `docs/architecture.md`, `docs/feature-parity.md`, `docs/operations.md`, `docs/security.md`, `README.md` | **Modify.** The staleness sweep. |
| `internal/worker/worker.go`, `cmd/encore-worker/main.go`, `cmd/encore-api/main.go` | **Modify.** Package comments that enumerate the loops. |

---

## Task 1: A rate budget of its own

**Files:**
- Modify: `internal/spotify/client.go` (the `request` struct, `getClass`, `budget`, `attempt`, `classify`, `NewClient`)
- Modify: `internal/spotify/oauth.go:153` (`token`) and its three callers at `:115`, `:136`, `:144`
- Modify: `internal/spotify/playlists.go` (five `interactive: true` at `:94`, `:128`, `:166`, `:206`, `:256`)
- Modify: `internal/spotify/recentlyplayed.go:29` (`CurrentUser`)
- Create: `internal/spotify/nowplaying.go`
- Create: `internal/spotify/nowplaying_test.go`
- Modify: `internal/spotify/playlists_test.go:519` (one comment sentence)

**Interfaces:**
- Consumes: `newTestClient(t, srv, clock, opts...)`, `newFakeClock()`, `WithPauseObserver`, `AsAPIError`, `Client.do`, `Client.endpoint` — all already in `internal/spotify`.
- Produces:
  - `type requestClass uint8` with `classCatalogue`, `classInteractive`, `classNowPlaying`
  - `func (requestClass) instanceWide() bool`
  - `spotify.Playback`, `spotify.PlaybackItem`, `spotify.Device`, `spotify.Show`
  - `func (c *Client) CurrentlyPlaying(ctx context.Context, accessToken string) (*Playback, error)` — `(nil, nil)` means nothing is playing

- [ ] **Step 1: Write the failing tests**

Create `internal/spotify/nowplaying_test.go`:

```go
package spotify

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// TestOnlyACatalogueRateLimitPausesTheInstance is the safety property of this
// whole phase, asserted across every request class at once.
//
// onPause writes app_settings.spotify_paused_until, which 409s "sync now" for
// every user on the instance and, at the worker's next construction, halts
// enrichment, the recently-played poller and all five library enumerations.
// Exactly one class of request is allowed to cause that.
//
// Fails when: instanceWide() is widened to any second class, or classify stops
// consulting it and goes back to testing a boolean.
func TestOnlyACatalogueRateLimitPausesTheInstance(t *testing.T) {
	for name, tc := range map[string]struct {
		class     requestClass
		wantPause int32
	}{
		"catalogue":   {classCatalogue, 1},
		"interactive": {classInteractive, 0},
		"now playing": {classNowPlaying, 0},
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Retry-After", "3600")
				w.WriteHeader(http.StatusTooManyRequests)
			}))
			defer srv.Close()

			var paused atomic.Int32
			c := newTestClient(t, srv, newFakeClock(),
				WithPauseObserver(func(time.Time) { paused.Add(1) }))

			err := c.do(context.Background(), request{
				method: http.MethodGet,
				url:    srv.URL + "/v1/probe",
				label:  "probe",
				bearer: "user-token",
				class:  tc.class,
			})
			if err == nil {
				t.Fatal("want an error on a 429")
			}
			if got := paused.Load(); got != tc.wantPause {
				t.Fatalf("the pause observer fired %d times, want %d", got, tc.wantPause)
			}
		})
	}
}

// TestNowPlayingRateLimitTouchesNoOtherBudget pins which limiter a 429 on the
// poll actually pauses.
//
// The test above proves nothing is recorded. This one proves nothing else is
// held back either: the catalogue budget carries enrichment and the sync
// poller, the sign-in budget carries authentication, and neither belongs to the
// least important request in the system.
//
// Fails when: budget() returns c.limiter or c.signin for classNowPlaying — the
// corresponding PausedUntil then stops being zero.
func TestNowPlayingRateLimitTouchesNoOtherBudget(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "3600")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	var paused atomic.Int32
	c := newTestClient(t, srv, newFakeClock(),
		WithPauseObserver(func(time.Time) { paused.Add(1) }))

	if _, err := c.CurrentlyPlaying(context.Background(), "user-token"); err == nil {
		t.Fatal("CurrentlyPlaying: want an error on a 429")
	}

	if got := paused.Load(); got != 0 {
		t.Errorf("the pause observer fired %d times; a now-playing 429 must never "+
			"pause Spotify instance-wide", got)
	}
	if until := c.Limiter().PausedUntil(); !until.IsZero() {
		t.Errorf("the catalogue budget is paused until %v; enrichment and the "+
			"recently-played poller draw on it", until)
	}
	if until := c.signin.PausedUntil(); !until.IsZero() {
		t.Errorf("the sign-in budget is paused until %v; nothing a background "+
			"worker does may take authentication offline", until)
	}
	if c.nowPlaying.PausedUntil().IsZero() {
		t.Error("the now-playing budget is not paused; the 429 backed nothing off at all")
	}
}

// TestNowPlayingRateLimitStopsTheNextRequestWithoutSendingIt is the "stopping is
// the property" half: a 429 must back the poller off, not merely be recorded.
//
// The server answers 429 once and 204 for ever after, so a second request that
// reached it would succeed. It must not reach it.
//
// Fails when: classNowPlaying stops getting a bounded wait — the second call
// then sleeps out the hour instead of answering and the deadline below fires;
// or the pause lands on a limiter the second call does not consult, in which
// case the request count is 2.
func TestNowPlayingRateLimitStopsTheNextRequestWithoutSendingIt(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "3600")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := newTestClient(t, srv, newFakeClock())

	if _, err := c.CurrentlyPlaying(context.Background(), "user-token"); err == nil {
		t.Fatal("the first call: want an error on a 429")
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("the first call made %d requests, want 1", got)
	}

	done := make(chan error, 1)
	go func() {
		_, err := c.CurrentlyPlaying(context.Background(), "user-token")
		done <- err
	}()
	select {
	case err := <-done:
		var paused *PausedError
		if !errors.As(err, &paused) {
			t.Fatalf("the second call returned %v, want a *PausedError", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the second call blocked; a paused poller must answer at once " +
			"rather than hold a goroutine for the whole Retry-After")
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("%d requests reached Spotify, want 1: the poller did not back off", got)
	}
}

// TestCurrentlyPlayingReportsNoContentAsNothingPlaying pins the endpoint's
// commonest answer.
//
// 204 is not an error and is not "an advert with no item". It is the state the
// card renders as "Nothing is playing.", which has to stay distinct from both
// "Encore has not checked yet" and "something Encore cannot identify".
//
// Fails when: request.status is dropped. decode() returns early on a 204
// without touching r.out, so the zero-value Playback would come back non-nil
// and every idle listener would be reported as playing something
// unidentifiable.
func TestCurrentlyPlayingReportsNoContentAsNothingPlaying(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := newTestClient(t, srv, newFakeClock())
	got, err := c.CurrentlyPlaying(context.Background(), "user-token")
	if err != nil {
		t.Fatalf("CurrentlyPlaying: %v", err)
	}
	if got != nil {
		t.Fatalf("CurrentlyPlaying = %+v, want nil for a 204", got)
	}
}

// TestCurrentlyPlayingDecodesATrack pins the fields the card renders, the
// listener's own token, and the query.
//
// additional_types=episode is not decoration: without it Spotify answers a
// podcast with item: null and currently_playing_type "episode", so a named
// episode would render as "something Encore cannot identify".
//
// Fails when: additional_types is dropped; the application token is used
// instead of the listener's, which answers for nobody; or the path changes to
// /v1/me/player, which carries a payload this phase does not use.
func TestCurrentlyPlayingDecodesATrack(t *testing.T) {
	var gotPath, gotQuery, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery, gotAuth = r.URL.Path, r.URL.RawQuery, r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
            "timestamp": 1785000000000,
            "progress_ms": 161000,
            "is_playing": true,
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
	got, err := c.CurrentlyPlaying(context.Background(), "user-token")
	if err != nil {
		t.Fatalf("CurrentlyPlaying: %v", err)
	}
	if got == nil {
		t.Fatal("CurrentlyPlaying returned nil for a 200 carrying an item")
	}

	if gotPath != "/v1/me/player/currently-playing" {
		t.Errorf("path = %q, want /v1/me/player/currently-playing", gotPath)
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
	if got.ProgressMs == nil || *got.ProgressMs != 161000 {
		t.Errorf("ProgressMs = %v, want 161000", got.ProgressMs)
	}
	if got.Device == nil || got.Device.Name != "Kitchen speaker" {
		t.Errorf("Device = %+v, want the Kitchen speaker", got.Device)
	}
	if got.Item == nil || got.Item.Name != "The Wheel" || got.Item.DurationMs != 255000 {
		t.Errorf("Item = %+v, want The Wheel at 255000 ms", got.Item)
	}
	if got.Item == nil || len(got.Item.Artists) != 1 || got.Item.Artists[0].Name != "SOHN" {
		t.Errorf("Artists = %+v, want SOHN", got.Item)
	}
}

// TestCurrentlyPlayingForbiddenIsNotRetried pins that a grant without
// user-read-playback-state costs one request, not six.
//
// Fails when: classify stops wrapping non-429 4xx in retry.Stop, or
// CurrentlyPlaying grows a retry loop of its own.
func TestCurrentlyPlayingForbiddenIsNotRetried(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	c := newTestClient(t, srv, newFakeClock())
	_, err := c.CurrentlyPlaying(context.Background(), "user-token")
	apiErr, ok := AsAPIError(err)
	if !ok || !apiErr.IsForbidden() {
		t.Fatalf("error = %v, want a 403 APIError", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("attempts = %d, want 1: a scope failure spends quota to fail identically", got)
	}
}
```

- [ ] **Step 2: Run them and watch them fail**

Run: `go test -count=1 -run 'TestOnlyACatalogue|TestNowPlayingRateLimit|TestCurrentlyPlaying' ./internal/spotify/`

Expected: FAIL to **compile** — `requestClass`, `classCatalogue`, `c.nowPlaying` and `CurrentlyPlaying` are all undefined. A compile failure is the correct first failure: the assertions cannot run until the type exists.

- [ ] **Step 3: Replace the boolean with a class**

In `internal/spotify/client.go`, after the top-of-file `const` block:

```go
// requestClass decides which rate budget a request draws on, and — through
// instanceWide below — what a 429 on it means for everybody else.
//
// It is a type rather than the boolean it replaces because there are three
// budgets and a boolean can name two. The distinction it carries is not
// "urgent or not": it is whose quota is being spent, and whose work stops when
// Spotify says no.
type requestClass uint8

const (
	// classCatalogue is the application's shared quota: enrichment, the
	// recently-played poller, the library enumerations, the album and artist
	// caches. A 429 here is a fact about the whole instance.
	classCatalogue requestClass = iota
	// classInteractive is a request somebody is sitting in front of: the OAuth
	// exchange, the profile read behind it, and the playlist writes a button
	// press makes. It waits only as long as a browser will.
	classInteractive
	// classNowPlaying is the opt-in now-playing poller, and nothing else.
	//
	// It is separate from classInteractive rather than folded into it, even
	// though both want a bounded wait and neither wants to be recorded,
	// because the sign-in limiter's own comment already forbids it: nothing a
	// background worker does may take authentication offline. At thirty
	// seconds across five accounts this loop is roughly fourteen thousand
	// requests a day, and one 429 among them would pause sign-in for whatever
	// Retry-After Spotify names — most of a day, for an exhausted quota —
	// locking every listener out of an instance whose data is perfectly fine.
	classNowPlaying
)

// instanceWide reports whether a 429 on this class is a fact about the whole
// instance rather than about one caller.
//
// One predicate decides both consequences, and that is the point of it being
// one predicate: the class that records an instance-wide pause is exactly the
// class willing to wait that pause out, and a class that is not must answer
// immediately instead. Splitting the two tests would allow a class that stops
// everybody else and then refuses to queue behind its own decision.
//
// A class added later and not named here defaults to false, which is the safe
// direction: a new caller's mistake costs a private pause, never a global one.
func (k requestClass) instanceWide() bool { return k == classCatalogue }
```

Extend the `const` block that holds `signinRate`:

```go
	// nowPlayingRate and nowPlayingBurst are the now-playing poller's own
	// budget.
	//
	// The bucket is not the interesting part — one request per account per
	// interval is already paced by the interval — the isolation is. It is
	// sized so one tick can clear a large instance without queueing: five a
	// second is a hundred and fifty accounts inside a thirty-second tick.
	nowPlayingRate  = 5
	nowPlayingBurst = 10
	// nowPlayingWait is how long a poll may queue for a token before giving up
	// and letting the next tick decide.
	//
	// Nobody is waiting on it, so the bound is not about patience: a poll that
	// sat out an hour-long pause would hold a goroutine and then report a fact
	// an hour stale. Answering at once leaves the limiter paused, so no request
	// reaches Spotify until it lifts, and the card says how long ago the last
	// check was.
	nowPlayingWait = 2 * time.Second
```

On the `Client` struct, after `signin`:

```go
	// nowPlaying is the opt-in now-playing poller's own budget.
	//
	// Never shared, never restored from a recorded pause, and never recorded:
	// see requestClass. This is the least important request Encore makes and by
	// far the most frequent, so it is the one that must not be able to stop
	// anything else.
	nowPlaying *Limiter
```

In `NewClient`, immediately after the line building `c.signin`:

```go
	c.nowPlaying = NewLimiterWithClock(nowPlayingRate, nowPlayingBurst, c.clock)
```

On the `request` struct, replace `interactive bool` and its comment with:

```go
	// class decides which rate budget this request draws on and what a 429 on
	// it means for the rest of the instance. See requestClass.
	class requestClass
	// status, when non-nil, receives the status code of a successful response.
	//
	// Exactly one caller needs it. GET /v1/me/player/currently-playing answers
	// 204 when nothing is playing, which is the commonest case and is not an
	// error; decode() returns early on a 204 without touching out, so without
	// this the zero-value body would be indistinguishable from a 200 carrying
	// an advert with no item.
	status *int
```

Rewrite `budget`:

```go
// budget picks the limiter a request draws on, and how long it may queue.
//
// Total over requestClass by construction: a class added without a case here
// falls to the catalogue budget, which is the loudest possible failure and
// therefore the right default for something nobody thought about.
func (c *Client) budget(r request) (*Limiter, time.Duration) {
	switch r.class {
	case classInteractive:
		return c.signin, signinWait
	case classNowPlaying:
		return c.nowPlaying, nowPlayingWait
	default:
		return c.limiter, 0
	}
}
```

In `attempt`, the 2xx branch:

```go
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if r.status != nil {
			*r.status = resp.StatusCode
		}
		return c.decode(resp, r)
	}
```

In `classify`, the two places that tested the boolean:

```go
		if c.onPause != nil && r.class.instanceWide() {
			c.onPause(resumeAt)
		}
```

```go
		if !r.class.instanceWide() {
			// Nobody waits half a minute to be told no, and a background poll
			// that waited out an exhausted quota would hold a goroutine for
			// most of a day to report something stale. The limiter now holds
			// the real delay, so a further attempt would fail its bounded wait
			// anyway. Answer immediately, with the instant the pause lifts.
			return retry.Stop(&PausedError{Until: resumeAt})
		}
```

Finally change `getClass`'s last parameter from `interactive bool` to `class requestClass`, set `class: class` on the request it builds, and have `get` pass `classCatalogue`.

- [ ] **Step 4: Update the six existing call sites**

- `internal/spotify/recentlyplayed.go:29` — `CurrentUser` passes `classInteractive` instead of `true`.
- `internal/spotify/oauth.go:153` — `token(ctx context.Context, label string, form url.Values, class requestClass)`, setting `class: class`. Its three callers become `classInteractive` (`:115`, the code exchange), `classCatalogue` (`:136`, the refresh) and `classCatalogue` (`:144`, the application token).
- `internal/spotify/playlists.go` — the five `interactive: true` lines at `:94`, `:128`, `:166`, `:206` and `:256` become `class: classInteractive`.

Then fix the one comment naming the old field, `internal/spotify/playlists_test.go:519`:

```go
// Fails when: class: classInteractive is dropped from either request — the
// pause observer then fires and the assertion below catches it.
```

- [ ] **Step 5: Write `CurrentlyPlaying`**

Create `internal/spotify/nowplaying.go`:

```go
package spotify

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
)

// Device is the player a listener is using.
//
// A pointer everywhere it appears: GET /v1/me/player/currently-playing is
// documented with the same response object as GET /v1/me/player but is observed
// to omit this, so a caller must be able to say "no device reported" rather
// than "a device with no name".
type Device struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Type string `json:"type"`
	// IsActive and VolumePercent are decoded but unused. They are here because
	// dropping fields from a response object makes the next reader wonder
	// whether Spotify stopped sending them.
	IsActive      bool `json:"is_active"`
	VolumePercent *int `json:"volume_percent"`
}

// Show is the podcast an episode belongs to.
type Show struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Publisher string `json:"publisher"`
}

// PlaybackItem is whatever is in the player.
//
// It is not spotify.Track, and cannot be: this endpoint returns a union of two
// object types under one key, and an episode carries a show where a track
// carries artists. Track has nowhere to put a show, so decoding an episode into
// one would silently produce a track with no artist.
type PlaybackItem struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Type is "track" or "episode". Spotify has been observed to omit it on a
	// track, so an empty value is read as a track rather than as unknown.
	Type       string `json:"type"`
	URI        string `json:"uri"`
	DurationMs int    `json:"duration_ms"`
	// IsLocal marks a file on the listener's own machine. It has no catalogue
	// identity, so Encore can neither link it nor ever record it as a listen.
	IsLocal bool     `json:"is_local"`
	Artists []Artist `json:"artists"`
	Show    *Show    `json:"show"`
}

// Playback is what Spotify says is in the player right now.
type Playback struct {
	Timestamp  int64 `json:"timestamp"`
	ProgressMs *int  `json:"progress_ms"`
	IsPlaying  bool  `json:"is_playing"`
	// CurrentlyPlayingType is "track", "episode", "ad" or "unknown". An advert
	// arrives as "ad" with a null Item, which is why the item alone cannot
	// classify a response.
	CurrentlyPlayingType string        `json:"currently_playing_type"`
	Item                 *PlaybackItem `json:"item"`
	Device               *Device       `json:"device"`
	Context              *PlayContext  `json:"context"`
}

// CurrentlyPlaying reads what the listener is playing right now, or nil when
// nothing is.
//
// A nil result with a nil error is the endpoint's commonest answer and is not a
// failure: Spotify replies 204 No Content when the player is idle. The caller
// records that as "nothing is playing", which is a different fact from "Encore
// has not managed to look", and neither is an error.
//
// additional_types=episode is required for a podcast to arrive with a name.
// Without it Spotify answers item: null with currently_playing_type "episode",
// and a named episode becomes something no interface can describe.
//
// This is the only caller of classNowPlaying, and that is the whole design: a
// 429 here pauses this budget alone. It never reaches the pause observer, so it
// never writes app_settings.spotify_paused_until, so it can never 409 "sync
// now" for every user or stop enrichment. See requestClass.
func (c *Client) CurrentlyPlaying(ctx context.Context, accessToken string) (*Playback, error) {
	if accessToken == "" {
		return nil, fmt.Errorf("spotify: currently playing: no access token")
	}

	q := url.Values{}
	q.Set("additional_types", "episode")

	var (
		body   Playback
		status int
	)
	if err := c.do(ctx, request{
		method: http.MethodGet,
		url:    c.endpoint("/v1/me/player/currently-playing", q),
		label:  "get currently playing",
		bearer: accessToken,
		out:    &body,
		status: &status,
		class:  classNowPlaying,
	}); err != nil {
		return nil, fmt.Errorf("spotify: currently playing: %w", err)
	}
	if status == http.StatusNoContent {
		return nil, nil
	}
	return &body, nil
}
```

- [ ] **Step 6: Run the whole package**

```bash
export PATH="$PATH:$(go env GOPATH)/bin"
gofmt -l $(git ls-files '*.go'); go vet ./...; staticcheck ./...
go test -count=1 ./internal/spotify/
```

Expected: PASS, including every pre-existing test in the package. The class change touches the paths `oauth_test.go`, `signin_test.go` and `playlists_test.go` exercise; if one of them fails, a call site was mapped to the wrong class, not the test.

- [ ] **Step 7: Run everything**

Run: `go test -count=1 ./...`
Expected: PASS.

- [ ] **Step 8: Commit**

```bash
git add internal/spotify/client.go internal/spotify/oauth.go internal/spotify/playlists.go \
        internal/spotify/recentlyplayed.go internal/spotify/nowplaying.go \
        internal/spotify/nowplaying_test.go internal/spotify/playlists_test.go
git commit -m "$(cat <<'MSG'
Spotify: a third request class, so a presence poll cannot stop the instance

The now-playing poll is the least important request Encore makes and by far the
most frequent. Until now every non-interactive request shared one limiter, and a
429 on it is recorded in app_settings.spotify_paused_until, which 409s "sync
now" for every user and halts enrichment, the recently-played poller and all
five library enumerations. A presence poll must not be able to do that.

The design document says the poll should draw on the interactive budget. It must
not: the sign-in limiter's own comment already says nothing a background worker
does may take authentication offline, and fourteen thousand daily requests
through it would let one 429 lock everybody out of an instance whose data is
fine.

So a boolean becomes a three-valued class, and one predicate — instanceWide —
decides both whether a 429 is recorded and whether the caller waits it out.
Splitting those two tests would allow a class that stops everybody else and then
refuses to queue behind its own decision.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
MSG
)"
```

---

## Task 2: One key, five places, and unset means never

**Files:**
- Modify: `internal/config/config.go` (`Config`, a new `NowPlaying` type, the parse block, `optionalDuration`, `Redacted`)
- Modify: `internal/config/config_test.go`
- Modify: `docker-compose.yml` (the `x-encore-env` anchor)
- Modify: `.env.example`
- Modify: `docs/configuration.md`
- Modify: `docker-compose.portainer.yml` (**generated** — do not hand-edit)

**Interfaces:**
- Consumes: `libraryTestEnv(map[string]string) map[string]string` (`internal/config/config_test.go:111`), `LoadFrom`.
- Produces:
  - `type config.NowPlaying struct { Interval time.Duration }`
  - `func (config.NowPlaying) Enabled() bool`
  - `config.Config.NowPlaying`
  - `const config.NowPlayingMinInterval = 10 * time.Second`

**All five files change in one commit.** `test/deploy/composeenv_test.go:14` regexes `internal/config/config.go` for `ENCORE_[A-Z0-9]+(_[A-Z0-9]+)*` **including inside comments**, and asserts every match is in `docker-compose.yml`, which in turn must be in `.env.example`. The moment the literal string exists in `config.go`, all three are required. `docs/configuration.md` and the generated Portainer file are not checked by that test — the first is checked by nothing at all, the second by CI's `lint` job.

- [ ] **Step 1: Write the failing tests**

Append to `internal/config/config_test.go`:

```go
// TestNowPlayingIsOffWhenTheIntervalIsUnset is the binding half of the feature's
// contract: absent from the environment means the poller never runs at all, not
// that it runs on a default.
//
// Fails when: the parse switches to p.duration with a non-zero default, which is
// what every other worker interval in this file uses and is exactly what must
// not happen here.
func TestNowPlayingIsOffWhenTheIntervalIsUnset(t *testing.T) {
	cfg, err := LoadFrom(libraryTestEnv(nil))
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	if cfg.NowPlaying.Interval != 0 {
		t.Errorf("NowPlaying.Interval = %s, want 0 when the key is unset", cfg.NowPlaying.Interval)
	}
	if cfg.NowPlaying.Enabled() {
		t.Error("NowPlaying.Enabled() = true with no ENCORE_NOWPLAYING_INTERVAL set; " +
			"an unconfigured instance must never poll Spotify for presence")
	}
}

// TestNowPlayingParsesADurationAndBareSeconds pins both spellings the parser
// accepts everywhere else, so the one opt-in key is not the odd one out.
//
// Fails when: optionalDuration stops delegating to p.duration and grows its own
// parser, which would drop the bare-integer form.
func TestNowPlayingParsesADurationAndBareSeconds(t *testing.T) {
	for _, spelling := range []string{"30s", "30"} {
		cfg, err := LoadFrom(libraryTestEnv(map[string]string{
			"ENCORE_NOWPLAYING_INTERVAL": spelling,
		}))
		if err != nil {
			t.Fatalf("LoadFrom(%q): %v", spelling, err)
		}
		if cfg.NowPlaying.Interval != 30*time.Second {
			t.Errorf("ENCORE_NOWPLAYING_INTERVAL=%q gave %s, want 30s",
				spelling, cfg.NowPlaying.Interval)
		}
		if !cfg.NowPlaying.Enabled() {
			t.Errorf("ENCORE_NOWPLAYING_INTERVAL=%q did not enable the poller", spelling)
		}
	}
}

// TestNowPlayingRefusesAnIntervalUnderTheFloor pins that a too-small interval is
// an error rather than a silent clamp.
//
// Clamping would run a poller at a rate its operator did not choose: somebody
// who typed 1s would get 10s and no indication that the number they set is not
// the number in force. The quota table in docs/configuration.md is the argument
// for the floor existing at all — one account at ten seconds is already 8,640
// requests a day.
//
// Fails when: the floor check is replaced by `if d < min { d = min }`, or the
// floor is removed and any positive value is accepted.
func TestNowPlayingRefusesAnIntervalUnderTheFloor(t *testing.T) {
	_, err := LoadFrom(libraryTestEnv(map[string]string{
		"ENCORE_NOWPLAYING_INTERVAL": "1s",
	}))
	if err == nil {
		t.Fatal("LoadFrom accepted a one-second now-playing interval")
	}
	if !strings.Contains(err.Error(), "ENCORE_NOWPLAYING_INTERVAL") {
		t.Errorf("the error does not name the key: %v", err)
	}
}

// TestNowPlayingRejectsNonsense keeps a typo from silently disabling the
// feature, which is the failure mode an unset-means-off key invites: "30x" must
// be an error, not a quiet zero.
//
// Fails when: optionalDuration swallows p.duration's recorded problem and
// returns 0 without it having been recorded.
func TestNowPlayingRejectsNonsense(t *testing.T) {
	if _, err := LoadFrom(libraryTestEnv(map[string]string{
		"ENCORE_NOWPLAYING_INTERVAL": "30x",
	})); err == nil {
		t.Fatal("LoadFrom accepted ENCORE_NOWPLAYING_INTERVAL=30x")
	}
}
```

`strings` is already imported by that file; `time` is too.

- [ ] **Step 2: Run them and watch them fail**

Run: `go test -count=1 -run TestNowPlaying ./internal/config/`
Expected: FAIL to compile — `cfg.NowPlaying` undefined.

- [ ] **Step 3: Add the type and the parser helper**

In `internal/config/config.go`, add the field to `Config` after `MetadataFallback`:

```go
	// NowPlaying is the optional poller that asks Spotify what each listener is
	// playing right now. Unset means it never runs.
	NowPlaying NowPlaying
```

After the `MetadataFallback` type and its `Enabled` method:

```go
// NowPlayingMinInterval is the shortest polling interval this instance will
// accept.
//
// A floor rather than a clamp, and it exists because the cost is per account per
// tick and recurring: one account at ten seconds is already 8,640 requests a
// day, against a development-mode quota that a single import can exhaust on its
// own. Below that the feature stops being a display and becomes the instance's
// dominant consumer of Spotify.
const NowPlayingMinInterval = 10 * time.Second

// NowPlaying configures the poller that reads GET /v1/me/player/currently-playing
// for each connected account.
//
// It is off unless Interval is set, which is a different shape from Sync,
// Library and Enrich — each of which has an explicit Enabled bool beside its
// interval — and deliberately so. Those are features an instance is expected to
// run; this one is not. A separate Enabled flag would mean an instance could be
// configured with an interval and no flag, or a flag and no interval, and the
// second of those has no correct behaviour. MetadataFallback.URL already uses
// this shape for the same reason: ship the mechanism, default it off, document
// the cost.
//
// The cost, per docs/configuration.md:
//
//	 1 account  @ 30s  ~= 2,880 requests/day
//	 5 accounts @ 30s  ~= 14,400 requests/day
//	 5 accounts @ 60s  ~= 7,200 requests/day
//
// which is why this is opt-in rather than a default with a tuning knob: a
// development-mode Spotify application already exhausts its quota during a large
// import, and a poller that silently doubled baseline consumption would make
// that worse for everybody on the instance.
//
// The poller draws on a rate budget of its own (internal/spotify's
// classNowPlaying), so a 429 on it pauses this loop and nothing else. That is
// the property that makes an opt-in poller safe to offer at all.
type NowPlaying struct {
	// Interval is how often each connected account is checked. Zero means the
	// feature is off and the loop returns before it lists a single account.
	Interval time.Duration
}

// Enabled reports whether this instance polls for playback state.
func (n NowPlaying) Enabled() bool { return n.Interval > 0 }
```

In `parse`, immediately after the `c.MetadataFallback` block and its validation:

```go
	c.NowPlaying = NowPlaying{
		Interval: p.optionalDuration("ENCORE_NOWPLAYING_INTERVAL", NowPlayingMinInterval),
	}
```

Beside `duration` in the parsing helpers:

```go
// optionalDuration parses a duration that switches a feature on by existing.
//
// Unset returns zero, which the caller reads as "off". This is the only
// duration in the file with that shape, and it is why p.duration cannot be used
// directly: every other interval has a default that makes the feature run.
//
// A value below min is an error rather than a clamp. Clamping would run a
// feature at a rate its operator did not choose and gave no sign of accepting,
// and the whole argument for this key being opt-in is that its cost is the
// operator's to weigh.
func (p *parser) optionalDuration(key string, min time.Duration) time.Duration {
	if _, ok := p.raw(key); !ok {
		return 0
	}
	// Delegated rather than reimplemented, so the bare-integer-means-seconds
	// spelling and the error wording match every other duration in this file.
	d := p.duration(key, 0)
	if d <= 0 {
		// p.duration has already recorded the problem.
		return 0
	}
	if d < min {
		p.errf("%s must be at least %s, got %s", key, min, d)
		return 0
	}
	return d
}
```

And in `Redacted`, beside `library_interval`:

```go
		// The startup log is the one line that says what this process believes
		// its configuration to be, and "why is there no now-playing card" is
		// answerable from here or from nowhere — the more so because the answer
		// is usually "the key is not set", which leaves no other trace.
		"nowplaying_enabled":  c.NowPlaying.Enabled(),
		"nowplaying_interval": c.NowPlaying.Interval.String(),
```

- [ ] **Step 4: Run the config tests**

Run: `go test -count=1 ./internal/config/`
Expected: PASS.

Then run the guard that now demands the other four places:

Run: `go test -count=1 ./test/deploy/`
Expected: **FAIL** — `internal/config reads 1 variables that docker-compose.yml does not pass`. That failure is the point of the step: it proves the guard is live before it is satisfied.

- [ ] **Step 5: `docker-compose.yml`**

In the `x-encore-env` anchor, immediately after the `ENCORE_LIBRARY_SYNC_*` group:

```yaml
  # The now-playing poller. Unset means it never runs; see docs/configuration.md
  # for what it costs per account per day before turning it on.
  ENCORE_NOWPLAYING_INTERVAL: ${ENCORE_NOWPLAYING_INTERVAL:-}
```

It goes in the shared anchor rather than on the `worker` service alone: the API reads it too, to tell the client whether a now-playing card should exist at all.

- [ ] **Step 6: `.env.example`**

After the "Library and follows synchronisation" block and before "Album track listings":

```
# ---------------------------------------------------------------------------
# Now playing
# ---------------------------------------------------------------------------
# Shows a card on the dashboard with what you are listening to right now, read
# from Spotify on a timer. Unset — the default — means Encore never asks, and
# the card does not appear.
#
# It is opt-in because the cost is recurring and per account:
#   1 account  at 30s  ~=  2,880 requests/day
#   5 accounts at 30s  ~= 14,400 requests/day
#   5 accounts at 60s  ~=  7,200 requests/day
# A development-mode Spotify application can exhaust its daily quota during a
# single large import, and a poller that quietly doubled the baseline would make
# that worse for everybody on the instance.
#
# The poller has a rate budget of its own, so a 429 on it backs off the poller
# and nothing else: enrichment, "sync now" and the recently-played poller are
# unaffected. It never writes to your listening history — that still comes only
# from the recently-played feed.
#
# Minimum 10s. Accounts that have not granted user-read-playback-state are
# skipped without a request.
#ENCORE_NOWPLAYING_INTERVAL=30s
```

While in this file, two sentences have become false and are fixed in this same commit:

- Line ~136, in the Library block: `# unlike the pollers below: a run costs roughly` — "the pollers below" now refers to something real for the first time, so it stays, but re-read it after inserting the block above and confirm the poller block is in fact below it. If the insertion point puts it above, change the phrase to `unlike the now-playing poller below`.
- Line ~160-161, in the album-tracks block: `ENCORE_ARTIST_ALBUMS_ENABLED below is the other unattended request, at its own cost and behind its own switch.` becomes:

```
# this fetch stops firing. It is one of three unattended Spotify requests, each
# with its own cost and its own switch: ENCORE_ARTIST_ALBUMS_ENABLED below is
# the discography walk, and ENCORE_NOWPLAYING_INTERVAL above is the now-playing
# poller. Listings already cached are still shown when it is off, with the date
# they were read; only the fetching stops.
```

- [ ] **Step 7: `docs/configuration.md`**

Add a section immediately after "Library and follows":

```markdown
## Now playing

| Variable | Default | Description |
|---|---|---|
| `ENCORE_NOWPLAYING_INTERVAL` | *unset* | How often Encore asks Spotify what each connected account is playing right now, which is what fills the dashboard's now-playing card. **Unset means the poller never runs and the card does not appear** — this is opt-in rather than a default with a tuning knob, because the cost is recurring and per account. Minimum `10s`. Accounts that have not granted `user-read-playback-state`, and accounts parked as `needs_reauth`, are skipped without a request. The poller **never writes to your listening history**: `GET /me/player/recently-played` remains the only path that creates a listen, and the now-playing poll is a read-only observer. It draws on a **rate budget of its own**, so unlike every other background request a 429 on it pauses this loop alone — it does not 409 "sync now" for other users, does not stop enrichment, and does not stop the recently-played poller or the library enumerations. `GET /api/nowplaying` reads the stored observation and never calls Spotify, so an open dashboard costs no quota however many tabs are on it. |

What it costs per day, which is the number to weigh before turning it on:

| Accounts | Interval | Requests/day |
|---|---|---|
| 1 | 30 s | ≈ 2,880 |
| 5 | 30 s | ≈ 14,400 |
| 5 | 60 s | ≈ 7,200 |

A development-mode Spotify application already exhausts its daily quota during a large import.
A poller that silently doubled baseline consumption would make that worse for everyone on the
instance, which is why this is off unless somebody asks for it.
```

While in this file, two existing sentences reference the poller as hypothetical and become real; fix both in this commit:

- In `ENCORE_LIBRARY_SYNC_ENABLED`: `On by default, unlike the metadata fallback's Prefer and the now-playing poller:` — correct as written, no change; confirm it still reads correctly beside the new section.
- In `ENCORE_ALBUM_TRACKS_ENABLED`: `the same reason the now-playing poller is opt-in and the discography walk below (ENCORE_ARTIST_ALBUMS_ENABLED) has its own switch` — correct as written, no change.

- [ ] **Step 8: Regenerate the Portainer stack**

```bash
./scripts/gen-portainer-stack.sh
git diff --stat docker-compose.portainer.yml
```

Expected: the file changes, with `ENCORE_NOWPLAYING_INTERVAL` appearing **four** times (the anchor is expanded once per service). If the script cannot run because `docker compose` is unavailable, **stop** — a hand-edited copy is a Critical in this repository's history, and CI's `lint` job diffs it.

- [ ] **Step 9: Run the guards**

```bash
export PATH="$PATH:$(go env GOPATH)/bin"
gofmt -l $(git ls-files '*.go'); go vet ./...; staticcheck ./...
go test -count=1 ./internal/config/ ./test/deploy/
git diff --exit-code docker-compose.portainer.yml && echo "portainer is committed and current"
```

Expected: PASS on both packages. The `git diff --exit-code` must be run **after** staging or committing the regenerated file; before that it is expected to show the change.

- [ ] **Step 10: Commit**

```bash
git add internal/config/config.go internal/config/config_test.go \
        docker-compose.yml docker-compose.portainer.yml .env.example docs/configuration.md
git commit -m "$(cat <<'MSG'
Config: ENCORE_NOWPLAYING_INTERVAL, and unset means never

Off by default is not enough for this one. The cost is recurring and per
account — five listeners at thirty seconds is fourteen thousand requests a day,
against a development-mode quota a single import can exhaust — so absent from
the environment has to mean the loop never runs, not that it runs on a default
nobody chose.

That is why this is an interval with no Enabled flag beside it, unlike Sync,
Library and Enrich. Two keys would allow an interval with no flag and a flag
with no interval, and the second has no correct behaviour. The metadata
fallback's URL already has this shape for the same reason.

A value under ten seconds is an error rather than a clamp: clamping would run a
poller at a rate its operator did not choose and gave no sign of accepting, and
the whole argument for opt-in is that the cost is theirs to weigh.

All five places in one commit, because test/deploy reads config.go's comments
too and would fail on any staging of them.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
MSG
)"
```

---

## Task 3: The observation, on disk

**Files:**
- Create: `migrations/00017_now_playing.sql`
- Create: `internal/domain/nowplaying.go`
- Create: `internal/store/accounts/nowplaying.go`
- Modify: `internal/store/accounts/accounts.go` (`Repo.NowPlaying`, `New`)
- Create: `test/integration/nowplaying_test.go`

**Interfaces:**
- Consumes: `store.Querier`, `store.Truncate` (`internal/store/store.go:193`), `postgres.Classify`, `domain.ErrNotFound`, `harness.New`, `harness.Env.NewUser`.
- Produces:
  - `domain.PlaybackState` with `PlaybackUnknown`, `PlaybackIdle`, `PlaybackPlaying`, `PlaybackPaused`
  - `domain.PlaybackItemKind` with `PlaybackItemNone`, `PlaybackItemTrack`, `PlaybackItemEpisode`, `PlaybackItemLocal`, `PlaybackItemUnknown`
  - `domain.NowPlaying` and `func (domain.NowPlaying) Observed() bool`
  - `accounts.NowPlaying` with `Get`, `Record`, `RecordFailure`, `ListDue`
  - `accounts.DueAccount{UserID uuid.UUID; Scopes []string}`

**Why there is a table at all.** The poller runs in `encore-worker` and `GET /api/nowplaying` is served by `encore-api`. They are separate processes in separate containers, so an in-memory observation in one is unreachable from the other, and the database is the only thing they share.

**Why one row per user and not a log.** A live card needs a *latest*; Phase 3c's fuzzy temporal join needs a *log*. One table cannot be both — a log has no current row and a latest has no history to join against — so `playback_observations` stays Phase 3c's, with its own key and its own 24-hour lifetime.

`now_playing` is **not** added to `test/harness/harness.go`'s `truncatedTables`: it cascades from `users`, which is in the list, and `TRUNCATE … CASCADE` reaches it. Verify that before assuming it — the last test in this task exists partly to prove it.

- [ ] **Step 1: Write the migration**

Create `migrations/00017_now_playing.sql`:

```sql
-- +goose Up

-- What Encore last saw in one listener's Spotify player, and when it last
-- looked.
--
-- One row per user, overwritten every tick. Not a log: Phase 3c's
-- playback_observations is the log, keyed (user_id, track_id, observed_at) and
-- expiring after 24 hours, because a fuzzy temporal join needs history. A live
-- card needs the opposite — a single current row — and merging the two would
-- give the card a table it has to run DISTINCT ON against and give the backfill
-- a table that overwrites its own evidence.
--
-- It exists at all because the poller runs in encore-worker and the endpoint is
-- served by encore-api. Two processes, two containers; the database is the only
-- thing they share.
CREATE TABLE now_playing (
    user_id uuid PRIMARY KEY REFERENCES users (id) ON DELETE CASCADE,

    -- The last *successful* observation. NULL until one has been made, which is
    -- the state the interface renders as "Encore has not checked yet" -- a
    -- different fact from "nothing is playing", and the pair the constraint
    -- below keeps inseparable.
    observed_at timestamptz,
    state       text NOT NULL DEFAULT 'unknown',
    kind        text NOT NULL DEFAULT 'none',

    -- The Spotify catalogue id, only ever set for a real track.
    --
    -- Deliberately NOT a foreign key to tracks. Spotify names a track the
    -- moment it starts playing; Encore's catalogue learns about it only when
    -- enrichment gets round to it, which may be hours later or never. A
    -- reference would fail the write for exactly the listener who is playing
    -- something new, which is the most interesting case there is. Whether the
    -- id names a row Encore holds is answered by a LEFT JOIN at read time, and
    -- decides only whether the title is a link.
    track_id text,

    -- What Spotify called it. Stored rather than joined, because a local file
    -- and a podcast episode have names and no catalogue identity at all, and
    -- because a track Encore has not enriched yet would otherwise display as
    -- blank.
    title  text NOT NULL DEFAULT '',
    artist text NOT NULL DEFAULT '',

    -- Progress at observed_at, and the item's length. Both nullable: an advert
    -- has neither, and a progress figure with no total says nothing.
    --
    -- Never extrapolated on read. The card states the age of the observation
    -- beside the figure rather than animating a bar from a fact that is up to
    -- one interval old, which would be a moving lie in place of a still truth.
    progress_ms integer,
    duration_ms integer,

    -- The player's name. The type is deliberately not stored: only the name is
    -- rendered, and the type is what Phase 3c's platform backfill wants, which
    -- belongs to its table. Empty when Spotify did not report a device --
    -- /v1/me/player/currently-playing is documented with the same object as
    -- /v1/me/player but is observed to omit it -- and the card then renders no
    -- device clause at all rather than inventing an unknown one.
    device_name text NOT NULL DEFAULT '',

    -- The last *attempt*, successful or not.
    --
    -- Separate from observed_at because they answer different questions. "When
    -- did Encore last look" is what makes a stale display honest; "when was
    -- this true" is what the figures above describe. Collapsing them would make
    -- a failed check look like an observation of an idle player.
    checked_at timestamptz NOT NULL,
    failed     boolean     NOT NULL DEFAULT false,

    -- A closed Go enum on both columns, so a value outside these sets is a bug
    -- in this repository and failing the write is how it gets found. This is
    -- the same judgement 00015 made for cover_state and the opposite of the one
    -- 00014 made for artist_albums.album_group -- that column holds a value
    -- Spotify mints and could extend without warning, these two are minted by
    -- Encore's own classifier.
    CONSTRAINT now_playing_state_known
        CHECK (state IN ('unknown', 'idle', 'playing', 'paused')),
    CONSTRAINT now_playing_kind_known
        CHECK (kind IN ('none', 'track', 'episode', 'local', 'unknown')),

    -- 'unknown' means exactly "never successfully observed", so it moves with
    -- observed_at's nullness in both directions. This is the constraint that
    -- makes "Encore has not checked yet" and "nothing is playing" impossible to
    -- confuse at the storage layer rather than merely by convention in the
    -- reader. 00016 enforces the same shape of pairing between playlists'
    -- cover_state and cover_at.
    CONSTRAINT now_playing_observed_at_matches_state
        CHECK ((state = 'unknown') = (observed_at IS NULL)),

    -- An idle player has no item and a playing one does. Without this, a row
    -- could claim to be playing nothing, or to be idle while naming a track,
    -- and every sentence the card can render about either would be false.
    CONSTRAINT now_playing_item_matches_state
        CHECK ((kind = 'none') = (state IN ('unknown', 'idle'))),

    -- When there is no item, nothing describes one. A title left over from the
    -- previous tick, sitting behind "Nothing is playing", is precisely the
    -- stale-claim defect this phase exists to rule out.
    CONSTRAINT now_playing_nothing_carries_nothing
        CHECK (kind <> 'none' OR (title = '' AND artist = '' AND track_id IS NULL
               AND progress_ms IS NULL AND duration_ms IS NULL AND device_name = '')),

    -- Only a real track has a catalogue id. A podcast episode's id is a show's,
    -- not a track's, and linking one to /tracks/{id} would be a dead link
    -- wearing a working one's clothes.
    CONSTRAINT now_playing_track_id_only_on_tracks
        CHECK (track_id IS NULL OR kind = 'track'),

    CONSTRAINT now_playing_progress_is_sane
        CHECK (progress_ms IS NULL OR progress_ms >= 0),
    CONSTRAINT now_playing_duration_is_sane
        CHECK (duration_ms IS NULL OR duration_ms > 0)
);

-- No index. The table holds one row per connected account, every read is by
-- primary key, and the poller's due query scans it joined to
-- spotify_credentials -- a table with the same number of rows. An index on
-- checked_at would cost a write per tick per account to save a scan of a
-- handful of rows.

-- +goose Down
DROP TABLE now_playing;
```

- [ ] **Step 2: Run the migration cycle**

```bash
export ENCORE_DATABASE_URL="postgres://encore:encore@localhost:5433/encore?sslmode=disable"
go run ./cmd/encore-migrate up && go run ./cmd/encore-migrate status
go run ./cmd/encore-migrate down && go run ./cmd/encore-migrate up
go run ./cmd/encore-migrate reset --yes && go run ./cmd/encore-migrate up
```

All must succeed. The reset/up cycle is what CI's `migrations` job runs.

- [ ] **Step 3: Add the domain types**

Create `internal/domain/nowplaying.go`:

```go
package domain

import "time"

// PlaybackState is what a listener's player was doing when Encore last managed
// to look.
type PlaybackState string

const (
	// PlaybackUnknown means no successful observation has ever been made for
	// this account.
	//
	// It is not "nothing is playing". That is PlaybackIdle, and the difference
	// is the whole reason this is an enum with four values rather than a
	// boolean: an interface that renders "we have not looked" as "nothing is
	// playing" states a fact about somebody's evening that nobody checked.
	PlaybackUnknown PlaybackState = "unknown"
	// PlaybackIdle means Encore looked and Spotify said nothing is playing.
	// The endpoint answers 204 No Content, which is the commonest case and is
	// not an error.
	PlaybackIdle PlaybackState = "idle"
	// PlaybackPlaying means something is playing.
	PlaybackPlaying PlaybackState = "playing"
	// PlaybackPaused means something is loaded and stopped.
	PlaybackPaused PlaybackState = "paused"
)

// PlaybackItemKind is what sort of thing is in the player, and therefore what
// Encore is able to say about it truthfully.
type PlaybackItemKind string

const (
	// PlaybackItemNone means there is nothing in the player at all.
	PlaybackItemNone PlaybackItemKind = "none"
	// PlaybackItemTrack is a Spotify catalogue track: the only kind that can
	// carry a TrackID, and the only kind that ever becomes a listen.
	PlaybackItemTrack PlaybackItemKind = "track"
	// PlaybackItemEpisode is a podcast episode. Encore's ingestion skips these,
	// so one will never appear in a listening history, and the interface says
	// so rather than letting somebody assume otherwise.
	PlaybackItemEpisode PlaybackItemKind = "episode"
	// PlaybackItemLocal is a file on the listener's own machine. It has a name
	// and no catalogue identity, so it can be shown and never linked.
	PlaybackItemLocal PlaybackItemKind = "local"
	// PlaybackItemUnknown is an advert, or a type this client does not know.
	//
	// Nothing descriptive is kept for one: Spotify's own label for an advert is
	// not a title, and rendering it as one would put an advertiser's name where
	// a listener expects their music.
	PlaybackItemUnknown PlaybackItemKind = "unknown"
)

// NowPlaying is the last thing Encore saw in a listener's player, and when it
// last looked.
//
// The two timestamps answer different questions and are kept apart on purpose.
// ObservedAt is when the figures below were true; CheckedAt is when Encore last
// tried at all. A display that had only one of them could not tell a stale
// truth from a fresh one, or a failure from an idle player.
type NowPlaying struct {
	// ObservedAt is when State and everything under it were true. Zero until a
	// check has succeeded.
	ObservedAt time.Time
	State      PlaybackState
	Kind       PlaybackItemKind
	// TrackID is the Spotify catalogue id, set only when Kind is
	// PlaybackItemTrack. It may name a track Encore's own catalogue has never
	// heard of; see TrackKnown.
	TrackID string
	// Title and Artist are what Spotify called it. Stored rather than joined,
	// because a local file and a podcast have names and no catalogue identity,
	// and an unenriched track would otherwise display as blank.
	Title  string
	Artist string
	// ProgressMs is progress at ObservedAt, never extrapolated. DurationMs is
	// the item's length. Both nil for an item that has neither.
	ProgressMs *int
	DurationMs *int
	// DeviceName is the player's name, empty when Spotify did not report one.
	DeviceName string
	// CheckedAt is when the poller last tried, successfully or not.
	CheckedAt time.Time
	// Failed reports that the attempt at CheckedAt did not succeed. Everything
	// above it is then the previous successful observation, or nothing.
	Failed bool
	// TrackKnown reports that TrackID names a row in Encore's own catalogue, so
	// a link to it will resolve. Computed at read time by a join; never stored,
	// because it changes when enrichment catches up rather than when the
	// listener changes track.
	TrackKnown bool
}

// Observed reports whether a successful observation has ever been recorded.
//
// The one predicate every reader should use for "does Encore know". Reading
// State directly invites the mistake this whole type is shaped to prevent:
// PlaybackUnknown is not a kind of silence, it is the absence of a look.
func (n NowPlaying) Observed() bool {
	return n.State != PlaybackUnknown && !n.ObservedAt.IsZero()
}
```

- [ ] **Step 4: Write the failing store tests**

Create `test/integration/nowplaying_test.go`:

```go
//go:build integration

package integration

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/RequiDev/encore/internal/domain"
	"github.com/RequiDev/encore/test/harness"
)

// playingTrack is one healthy observation, so each test varies only what it is
// about.
func playingTrack(at time.Time) domain.NowPlaying {
	progress, duration := 161000, 255000
	return domain.NowPlaying{
		ObservedAt: at,
		State:      domain.PlaybackPlaying,
		Kind:       domain.PlaybackItemTrack,
		TrackID:    "spotifytrack00000001",
		Title:      "The Wheel",
		Artist:     "SOHN",
		ProgressMs: &progress,
		DurationMs: &duration,
		DeviceName: "Kitchen speaker",
		CheckedAt:  at,
	}
}

// TestNowPlayingIsAbsentUntilSomethingIsObserved pins the distinction the whole
// feature turns on: never looked is not nothing playing.
//
// Fails when: Get invents a zero-valued row instead of reporting ErrNotFound, or
// RecordFailure inserts a row claiming 'idle' — in either case an account
// nobody has checked would read as one whose player is silent.
func TestNowPlayingIsAbsentUntilSomethingIsObserved(t *testing.T) {
	e := harness.New(t)
	user := e.NewUser("np-absent")

	if _, err := e.Accounts.NowPlaying.Get(e.Ctx(), e.Store.DB(), user.ID); err == nil {
		t.Fatal("Get returned a row for an account that has never been checked")
	}

	at := time.Date(2026, time.July, 31, 9, 0, 0, 0, time.UTC)
	if err := e.Accounts.NowPlaying.RecordFailure(e.Ctx(), e.Store.DB(), user.ID, at); err != nil {
		t.Fatalf("RecordFailure: %v", err)
	}

	got, err := e.Accounts.NowPlaying.Get(e.Ctx(), e.Store.DB(), user.ID)
	if err != nil {
		t.Fatalf("Get after a failure: %v", err)
	}
	if got.Observed() {
		t.Fatalf("Observed() = true after nothing but a failed check: %+v", got)
	}
	if got.State != domain.PlaybackUnknown {
		t.Errorf("State = %q, want %q", got.State, domain.PlaybackUnknown)
	}
	if got.Kind != domain.PlaybackItemNone {
		t.Errorf("Kind = %q, want %q", got.Kind, domain.PlaybackItemNone)
	}
	if !got.Failed {
		t.Error("Failed = false after a failed check")
	}
	if !got.CheckedAt.Equal(at) {
		t.Errorf("CheckedAt = %v, want %v", got.CheckedAt, at)
	}
}

// TestRecordRoundTripsEveryColumn pins that nothing is dropped between the
// classifier and the card.
//
// Fails when: a column is added to the INSERT and not to the SELECT, or the two
// disagree about order — the scan then lands progress in duration, and a
// listener sees "4:15 of 2:41".
func TestRecordRoundTripsEveryColumn(t *testing.T) {
	e := harness.New(t)
	user := e.NewUser("np-roundtrip")
	at := time.Date(2026, time.July, 31, 9, 30, 0, 0, time.UTC)

	want := playingTrack(at)
	if err := e.Accounts.NowPlaying.Record(e.Ctx(), e.Store.DB(), user.ID, want); err != nil {
		t.Fatalf("Record: %v", err)
	}

	got, err := e.Accounts.NowPlaying.Get(e.Ctx(), e.Store.DB(), user.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !got.Observed() {
		t.Fatal("Observed() = false after a successful record")
	}
	if got.State != domain.PlaybackPlaying || got.Kind != domain.PlaybackItemTrack {
		t.Errorf("State/Kind = %q/%q, want playing/track", got.State, got.Kind)
	}
	if got.Title != "The Wheel" || got.Artist != "SOHN" {
		t.Errorf("Title/Artist = %q/%q, want The Wheel/SOHN", got.Title, got.Artist)
	}
	if got.TrackID != "spotifytrack00000001" {
		t.Errorf("TrackID = %q", got.TrackID)
	}
	if got.ProgressMs == nil || *got.ProgressMs != 161000 {
		t.Errorf("ProgressMs = %v, want 161000", got.ProgressMs)
	}
	if got.DurationMs == nil || *got.DurationMs != 255000 {
		t.Errorf("DurationMs = %v, want 255000", got.DurationMs)
	}
	if got.DeviceName != "Kitchen speaker" {
		t.Errorf("DeviceName = %q", got.DeviceName)
	}
	if got.Failed {
		t.Error("Failed = true after a successful record")
	}
	if !got.ObservedAt.Equal(at) || !got.CheckedAt.Equal(at) {
		t.Errorf("ObservedAt/CheckedAt = %v/%v, want both %v", got.ObservedAt, got.CheckedAt, at)
	}
}

// TestAFailureKeepsTheLastObservation pins that a failed check does not erase
// what Encore already knew.
//
// This is what lets the card say "the last check failed; this is what you were
// playing four minutes ago" instead of falling back to "we do not know", which
// would throw away a true thing because a later request went wrong.
//
// Fails when: RecordFailure is written as a full upsert that resets the
// observation columns to their defaults — Observed() then goes false and the
// title is gone.
func TestAFailureKeepsTheLastObservation(t *testing.T) {
	e := harness.New(t)
	user := e.NewUser("np-stale")
	observed := time.Date(2026, time.July, 31, 9, 30, 0, 0, time.UTC)
	failedAt := observed.Add(4 * time.Minute)

	if err := e.Accounts.NowPlaying.Record(e.Ctx(), e.Store.DB(), user.ID, playingTrack(observed)); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if err := e.Accounts.NowPlaying.RecordFailure(e.Ctx(), e.Store.DB(), user.ID, failedAt); err != nil {
		t.Fatalf("RecordFailure: %v", err)
	}

	got, err := e.Accounts.NowPlaying.Get(e.Ctx(), e.Store.DB(), user.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !got.Failed {
		t.Error("Failed = false after a failed check")
	}
	if !got.CheckedAt.Equal(failedAt) {
		t.Errorf("CheckedAt = %v, want %v", got.CheckedAt, failedAt)
	}
	if !got.ObservedAt.Equal(observed) {
		t.Errorf("ObservedAt = %v, want the earlier %v: a failure must not move it",
			got.ObservedAt, observed)
	}
	if got.Title != "The Wheel" {
		t.Errorf("Title = %q, want the last observation to survive a failure", got.Title)
	}
}

// TestAnIdleObservationCannotCarryATitle proves the constraint bites rather than
// merely existing.
//
// A leftover title behind "Nothing is playing." is the exact stale-claim defect
// this phase exists to rule out, and the database is the only layer that can
// refuse it unconditionally.
//
// Fails when: now_playing_nothing_carries_nothing is dropped from the migration
// — the write then succeeds and no test anywhere notices.
func TestAnIdleObservationCannotCarryATitle(t *testing.T) {
	e := harness.New(t)
	user := e.NewUser("np-idle-title")
	at := time.Date(2026, time.July, 31, 9, 30, 0, 0, time.UTC)

	err := e.Accounts.NowPlaying.Record(e.Ctx(), e.Store.DB(), user.ID, domain.NowPlaying{
		ObservedAt: at,
		State:      domain.PlaybackIdle,
		Kind:       domain.PlaybackItemNone,
		Title:      "The Wheel",
		CheckedAt:  at,
	})
	if err == nil {
		t.Fatal("an idle observation carrying a title was accepted")
	}
}

// TestNowPlayingTextIsTruncatedRuneSafely pins the bound on the three text
// columns Spotify's own strings reach.
//
// Fails when: store.Truncate is replaced by a plain slice — the multi-byte runes
// below then cut mid-rune and PostgreSQL rejects the write outright, so Record
// returns an error instead of storing a bounded string.
func TestNowPlayingTextIsTruncatedRuneSafely(t *testing.T) {
	e := harness.New(t)
	user := e.NewUser("np-truncate")
	at := time.Date(2026, time.July, 31, 9, 30, 0, 0, time.UTC)

	// 600 three-byte runes: past the limit, and every boundary is a trap.
	long := strings.Repeat("é—中", 200)
	obs := playingTrack(at)
	obs.Title, obs.Artist, obs.DeviceName = long, long, long

	if err := e.Accounts.NowPlaying.Record(e.Ctx(), e.Store.DB(), user.ID, obs); err != nil {
		t.Fatalf("Record: %v", err)
	}
	got, err := e.Accounts.NowPlaying.Get(e.Ctx(), e.Store.DB(), user.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	for name, value := range map[string]string{
		"Title": got.Title, "Artist": got.Artist, "DeviceName": got.DeviceName,
	} {
		if len(value) > 256 {
			t.Errorf("%s is %d bytes, want it bounded", name, len(value))
		}
		if !utf8.ValidString(value) {
			t.Errorf("%s is not valid UTF-8", name)
		}
	}
}

// TestTrackKnownIsFalseForATrackTheCatalogueHasNeverSeen pins the join that
// decides whether the title is a link.
//
// Spotify names a track the instant it starts playing; Encore's catalogue
// learns about it when enrichment gets round to it. Linking before then would
// be a dead link wearing a working one's clothes.
//
// Fails when: the LEFT JOIN is dropped and TrackKnown is hard-coded to
// TrackID != "" — the assertion below then reports true for a track nothing
// holds.
func TestTrackKnownIsFalseForATrackTheCatalogueHasNeverSeen(t *testing.T) {
	e := harness.New(t)
	user := e.NewUser("np-unknown-track")
	at := time.Date(2026, time.July, 31, 9, 30, 0, 0, time.UTC)

	if err := e.Accounts.NowPlaying.Record(e.Ctx(), e.Store.DB(), user.ID, playingTrack(at)); err != nil {
		t.Fatalf("Record: %v", err)
	}
	got, err := e.Accounts.NowPlaying.Get(e.Ctx(), e.Store.DB(), user.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.TrackKnown {
		t.Fatal("TrackKnown = true for a track the catalogue has never seen")
	}
}

// TestDeletingAUserRemovesTheirNowPlayingRow pins the cascade, which is also
// what lets the integration harness leave now_playing out of truncatedTables.
//
// Fails when: the REFERENCES clause loses ON DELETE CASCADE — DeleteUser then
// fails outright on a foreign-key violation, which is a louder failure than
// this test but a failure the test still names.
func TestDeletingAUserRemovesTheirNowPlayingRow(t *testing.T) {
	e := harness.New(t)
	user := e.NewUser("np-cascade")
	at := time.Date(2026, time.July, 31, 9, 30, 0, 0, time.UTC)

	if err := e.Accounts.NowPlaying.Record(e.Ctx(), e.Store.DB(), user.ID, playingTrack(at)); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if err := e.Accounts.Users.DeleteUser(e.Ctx(), e.Store.DB(), user.ID); err != nil {
		t.Fatalf("DeleteUser: %v", err)
	}

	var count int
	row := e.Store.DB().QueryRow(e.Ctx(), `SELECT count(*) FROM now_playing WHERE user_id = $1`, user.ID)
	if err := row.Scan(&count); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if count != 0 {
		t.Fatalf("%d now_playing rows survived the account's deletion", count)
	}
}
```

Follow the surrounding integration files' conventions for the `uuid` argument shape (`store.UUIDArg` or the bare `uuid.UUID`, whichever the neighbouring tests use) rather than assuming this snippet's.

- [ ] **Step 5: Run them and watch them fail**

```bash
export ENCORE_TEST_DATABASE_URL="postgres://encore:encore@localhost:5433/encore?sslmode=disable"
go test -tags=integration -count=1 -p 1 -run 'TestNowPlaying|TestRecordRoundTrips|TestAFailureKeeps|TestAnIdle|TestTrackKnown|TestDeletingAUser' ./test/integration/
```

Expected: FAIL to compile — `e.Accounts.NowPlaying` undefined.

- [ ] **Step 6: Write the repository**

Create `internal/store/accounts/nowplaying.go`:

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

// nowPlayingTextLimit bounds the three columns Spotify's own strings reach.
//
// Long enough for any real title, artist list or device name, short enough that
// a malformed response cannot fill the table. Applied through store.Truncate,
// which is rune-safe: a byte-boundary cut through a multi-byte rune would make
// the write that records the observation itself fail.
const nowPlayingTextLimit = 200

// scopeReadPlaybackState is the grant this feature needs. It shipped in
// config.DefaultScopes() in Phase 2a, so an account connected since then
// already has it and one connected before does not — the ordinary state of an
// older account forever, not a fault to repair.
const scopeReadPlaybackState = "user-read-playback-state"

// NowPlaying is the repository for now_playing: one row per user holding the
// last observation of their player and the time of the last attempt.
type NowPlaying struct{ db *store.Store }

// NewNowPlaying builds the repository.
func NewNowPlaying(db *store.Store) *NowPlaying { return &NowPlaying{db: db} }

// DueAccount is one account the now-playing poller may check, with the scopes
// its grant carries.
//
// The scopes come back even though the query below already filters on them, so
// the poller can make the same check itself before spending a request. That is
// defence in depth in the shape internal/library already uses: the SQL predicate
// keeps a scopeless account out of the queue, and the in-code check is what
// still holds if somebody widens the predicate later.
type DueAccount struct {
	UserID uuid.UUID
	Scopes []string
}

// listDueSQL drives the now-playing poller's queue.
//
// Three exclusions, each for its own reason:
//
//   - needs_reauth, because a broken refresh token fails identically at every
//     endpoint and polling it would spend the instance's budget rediscovering
//     an answer only the listener can give;
//   - grants without user-read-playback-state, because the request would 403
//     and a 403 costs a request to be told something the stored grant already
//     says. The @> operator works here for the reason
//     listDueForLibrarySyncSQL documents: every write path splits granted
//     scopes into separate array elements, and the one legacy shape that holds
//     them space-joined necessarily predates this scope entirely;
//   - accounts checked within the last interval, so two worker processes share
//     the work without coordinating and a restart re-polls nobody early.
//
// NULLS FIRST puts a newly connected account at the head of the queue, so its
// card fills in on the next tick rather than behind everybody else.
const listDueSQL = `
    SELECT c.user_id, c.scopes
      FROM spotify_credentials c
      LEFT JOIN now_playing n ON n.user_id = c.user_id
     WHERE c.sync_state <> 'needs_reauth'
       AND c.scopes @> ARRAY['` + scopeReadPlaybackState + `']::text[]
       AND (n.checked_at IS NULL OR n.checked_at < $1)
     ORDER BY n.checked_at ASC NULLS FIRST
     LIMIT $2`

// ListDue returns the accounts whose player has not been checked since
// olderThan.
func (r *NowPlaying) ListDue(
	ctx context.Context, q store.Querier, olderThan time.Time, limit int,
) ([]DueAccount, error) {
	rows, err := q.Query(ctx, listDueSQL, olderThan.UTC(), clampLimit(limit))
	if err != nil {
		return nil, postgres.Classify("list accounts due for a playback check", err)
	}
	defer rows.Close()

	var out []DueAccount
	for rows.Next() {
		var a DueAccount
		if err := rows.Scan(&a.UserID, &a.Scopes); err != nil {
			return nil, postgres.Classify("scan account due for a playback check", err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, postgres.Classify("list accounts due for a playback check", err)
	}
	return out, nil
}

// getSQL reads one account's row, and answers in the same statement whether the
// track it names is one Encore's own catalogue holds.
//
// The join is here rather than in the handler because it is the same question
// as the row: "what is playing, and can it be linked". Two statements would let
// the two answers come from different instants.
const getSQL = `
    SELECT n.observed_at, n.state, n.kind, n.track_id, n.title, n.artist,
           n.progress_ms, n.duration_ms, n.device_name, n.checked_at, n.failed,
           (t.id IS NOT NULL) AS track_known
      FROM now_playing n
      LEFT JOIN tracks t ON t.id = n.track_id
     WHERE n.user_id = $1`

// Get returns one account's last observation, or domain.ErrNotFound when the
// poller has never reached it.
//
// ErrNotFound rather than a zero value on purpose: "no row" and "a row saying
// nothing is playing" are different answers, and a caller that received a zero
// value for both would have no way to tell them apart.
func (r *NowPlaying) Get(
	ctx context.Context, q store.Querier, userID uuid.UUID,
) (domain.NowPlaying, error) {
	var (
		n                    domain.NowPlaying
		state, kind          string
		trackID              *string
		observedAt           *time.Time
		progress, durationMs *int32
	)
	err := q.QueryRow(ctx, getSQL, userID).Scan(
		&observedAt, &state, &kind, &trackID, &n.Title, &n.Artist,
		&progress, &durationMs, &n.DeviceName, &n.CheckedAt, &n.Failed, &n.TrackKnown)
	if err != nil {
		return domain.NowPlaying{}, postgres.Classify("get now playing", err)
	}

	n.State = domain.PlaybackState(state)
	n.Kind = domain.PlaybackItemKind(kind)
	if observedAt != nil {
		n.ObservedAt = observedAt.UTC()
	}
	n.CheckedAt = n.CheckedAt.UTC()
	if trackID != nil {
		n.TrackID = *trackID
	}
	if progress != nil {
		v := int(*progress)
		n.ProgressMs = &v
	}
	if durationMs != nil {
		v := int(*durationMs)
		n.DurationMs = &v
	}
	return n, nil
}

// recordSQL stores a successful observation. checked_at is the observation's own
// instant: a check that succeeded happened exactly when what it saw was true.
const recordSQL = `
    INSERT INTO now_playing (user_id, observed_at, state, kind, track_id, title, artist,
                             progress_ms, duration_ms, device_name, checked_at, failed)
    VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $2, false)
    ON CONFLICT (user_id) DO UPDATE SET
        observed_at = EXCLUDED.observed_at,
        state       = EXCLUDED.state,
        kind        = EXCLUDED.kind,
        track_id    = EXCLUDED.track_id,
        title       = EXCLUDED.title,
        artist      = EXCLUDED.artist,
        progress_ms = EXCLUDED.progress_ms,
        duration_ms = EXCLUDED.duration_ms,
        device_name = EXCLUDED.device_name,
        checked_at  = EXCLUDED.checked_at,
        failed      = false`

// Record stores a successful observation, replacing whatever was there.
func (r *NowPlaying) Record(
	ctx context.Context, q store.Querier, userID uuid.UUID, n domain.NowPlaying,
) error {
	var trackID *string
	if n.TrackID != "" {
		id := n.TrackID
		trackID = &id
	}
	_, err := q.Exec(ctx, recordSQL, userID, n.ObservedAt.UTC(),
		string(n.State), string(n.Kind), trackID,
		store.Truncate(n.Title, nowPlayingTextLimit),
		store.Truncate(n.Artist, nowPlayingTextLimit),
		n.ProgressMs, n.DurationMs,
		store.Truncate(n.DeviceName, nowPlayingTextLimit))
	if err != nil {
		return postgres.Classify("record now playing", err)
	}
	return nil
}

// recordFailureSQL moves only the two columns that describe the attempt.
//
// The observation columns are deliberately untouched, which is what lets the
// interface say "the last check failed; this is what you were playing four
// minutes ago" rather than discarding a true thing because a later request went
// wrong. On a first insert they take the table's defaults, which is the
// never-observed state.
const recordFailureSQL = `
    INSERT INTO now_playing (user_id, checked_at, failed)
    VALUES ($1, $2, true)
    ON CONFLICT (user_id) DO UPDATE SET
        checked_at = EXCLUDED.checked_at,
        failed     = true`

// RecordFailure notes that a check was attempted at t and did not succeed.
func (r *NowPlaying) RecordFailure(
	ctx context.Context, q store.Querier, userID uuid.UUID, t time.Time,
) error {
	if _, err := q.Exec(ctx, recordFailureSQL, userID, t.UTC()); err != nil {
		return postgres.Classify("record a failed playback check", err)
	}
	return nil
}
```

Check `internal/store/accounts/credentials.go` for how it passes a `uuid.UUID` to pgx — if it wraps them in `store.UUIDArg`, do the same here for every `userID` argument.

- [ ] **Step 7: Add it to the bundle**

In `internal/store/accounts/accounts.go`, on `Repo`:

```go
	NowPlaying  *NowPlaying
```

and in `New`:

```go
		NowPlaying:  NewNowPlaying(db),
```

- [ ] **Step 8: Run the tests**

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
git add migrations/00017_now_playing.sql internal/domain/nowplaying.go \
        internal/store/accounts/nowplaying.go internal/store/accounts/accounts.go \
        test/integration/nowplaying_test.go
git commit -m "$(cat <<'MSG'
Store: one row per listener for what they are playing now

The poller runs in encore-worker and the endpoint is served by encore-api. Two
processes, two containers, so the observation has to go somewhere they both
reach, and the database is the only such place.

One row per user rather than a log. Phase 3c's fuzzy temporal join needs
history; a live card needs a current row. One table cannot be both — a log has
no current row and a latest has no history to join against.

Two timestamps rather than one, because "when did Encore last look" and "when
was this true" are different questions and a display holding only one of them
cannot tell a stale truth from a fresh one. A failed check moves the first and
leaves the second alone, so the card can say what you were playing four minutes
ago instead of throwing a true thing away because a later request went wrong.

A CHECK ties state = 'unknown' to observed_at IS NULL in both directions. That
pair is "Encore has not looked", and it must never be confusable with "nothing
is playing" — a constraint the reader cannot forget, rather than a convention it
can.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
MSG
)"
```

---

## Task 4: The poller, which cannot write a listen

**Files:**
- Create: `internal/nowplaying/nowplaying.go`
- Create: `internal/nowplaying/nowplaying_test.go`
- Modify: `cmd/encore-worker/main.go` (one `sup.Add`, and the package comment)
- Modify: `test/integration/nowplaying_test.go` (three more tests)

**Interfaces:**
- Consumes: `config.NowPlaying` and `Enabled()` (Task 2); `spotify.Client.CurrentlyPlaying` and `spotify.Playback` (Task 1); `accounts.NowPlaying` with `ListDue`, `Record`, `RecordFailure`, and `accounts.DueAccount` (Task 3); `domain.NowPlaying`, `domain.PlaybackState`, `domain.PlaybackItemKind` (Task 3); `*sync.Poller`'s exported `AccessToken(ctx, userID) (string, error)`.
- Produces:
  - `type nowplaying.Deps struct{ Store *store.Store; NowPlaying *accounts.NowPlaying; Spotify SpotifyAPI; Tokens Tokens; Logger *slog.Logger; Now func() time.Time }`
  - `func nowplaying.New(cfg config.NowPlaying, deps Deps) (*Watcher, error)`
  - `func (*Watcher) Run(ctx context.Context) error`
  - `func (*Watcher) RunOnce(ctx context.Context) (int, error)`

**The two structural guarantees this task installs:**

1. **`Deps.NowPlaying` is `*accounts.NowPlaying`, not `*accounts.Repo`.** The poller therefore has no handle on `accounts.Credentials` and cannot call `MarkNeedsReauth`, which is the rule `internal/sync/account.go:296` states: an optional-scope 403 must never park an account whose listening history reads perfectly.
2. **The package imports nothing that can write a listen.** Pinned by an import-graph test, not by a comment.

- [ ] **Step 1: Write the failing unit tests**

Create `internal/nowplaying/nowplaying_test.go`:

```go
package nowplaying

import (
	"context"
	"go/build"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/RequiDev/encore/internal/config"
	"github.com/RequiDev/encore/internal/domain"
	"github.com/RequiDev/encore/internal/spotify"
)

// TestThePollerCannotReachAnythingThatWritesAListen is the spec's read-only
// observer rule, made structural.
//
// §2.2: "/me/player must not create rows in listens." That is not a stylistic
// preference — the sync poller's correctness rests on its cursor advancing in
// the same transaction that commits the listens it covers, and a second writer
// with a different view of what has been played would produce duplicates the
// dedupe key catches by accident rather than by design.
//
// A comment saying so can be ignored. An import that does not exist cannot be
// used.
//
// Fails when: somebody adds a listens repository to Deps to "also record the
// play", or imports internal/sync to reuse a helper from it.
func TestThePollerCannotReachAnythingThatWritesAListen(t *testing.T) {
	pkg, err := build.ImportDir(".", 0)
	if err != nil {
		t.Fatalf("read this package's imports: %v", err)
	}
	forbidden := []string{
		"/internal/store/listens",
		"/internal/sync",
		"/internal/importer",
	}
	for _, imported := range pkg.Imports {
		for _, bad := range forbidden {
			if strings.HasSuffix(imported, bad) {
				t.Errorf("this package imports %s, which can write listens; "+
					"the now-playing poller is a read-only observer and "+
					"/me/player/recently-played is the only ingestion path", imported)
			}
		}
	}
}

// TestADisabledPollerNeverRuns is the binding half of the configuration
// contract: unset means the loop never runs at all, not that it runs and finds
// nothing to do.
//
// The context is deliberately never cancelled, so a Run that entered its loop
// would sit there until the deadline below rather than returning. That is what
// makes this test able to fail.
//
// Fails when: the Enabled() guard moves below the first timer, or is replaced by
// a default interval — Run then blocks and the deadline fires; or the guard is
// removed entirely, and the listing count below stops being zero.
func TestADisabledPollerNeverRuns(t *testing.T) {
	var checks, listings atomic.Int32
	w := newTestWatcher(t, config.NowPlaying{}, &checks, &listings, nil)

	done := make(chan error, 1)
	go func() { done <- w.Run(context.Background()) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run on a disabled poller returned %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return; with ENCORE_NOWPLAYING_INTERVAL unset the " +
			"poller must never run at all")
	}
	if got := checks.Load(); got != 0 {
		t.Errorf("%d Spotify requests were made by a disabled poller, want 0", got)
	}
	if got := listings.Load(); got != 0 {
		t.Errorf("%d account listings were made by a disabled poller, want 0", got)
	}
}

// TestAnAccountWithoutTheScopeIsSkippedWithoutARequest pins the spec's scope
// skip: the check happens before the request, not through a 403.
//
// Fails when: the HasScope guard in check() is removed — the request is then
// made, 403s, and costs a request to be told what the stored grant already
// said.
func TestAnAccountWithoutTheScopeIsSkippedWithoutARequest(t *testing.T) {
	var checks, listings atomic.Int32
	due := []accountsDue{{UserID: uuid.New(), Scopes: []string{"user-read-recently-played"}}}
	w := newTestWatcher(t, config.NowPlaying{Interval: 30 * time.Second}, &checks, &listings, due)

	polled, err := w.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if polled != 0 {
		t.Errorf("polled = %d, want 0", polled)
	}
	if got := checks.Load(); got != 0 {
		t.Fatalf("%d Spotify requests were made for an account without "+
			"user-read-playback-state, want 0", got)
	}
}

// TestObserveClassifiesEverythingSpotifyCanReturn is the single place the "what
// is playing" question is answered, so it is the single place every distinction
// the interface draws can be got wrong.
//
// Fails when: kindOf's ordering changes so an episode with no id is classified
// as a local file; the unknown-item scrub is removed and an advert's label
// leaks into a title; an empty item Type stops being read as a track; or a 204
// stops producing idle/none, which would put "nothing is playing" and "we do not
// know" into the same row shape.
func TestObserveClassifiesEverythingSpotifyCanReturn(t *testing.T) {
	at := time.Date(2026, time.July, 31, 9, 30, 0, 0, time.UTC)
	ms := func(n int) *int { return &n }

	tests := []struct {
		name string
		in   *spotify.Playback
		want domain.NowPlaying
	}{
		{
			name: "204 no content is an idle player, not a failure",
			in:   nil,
			want: domain.NowPlaying{
				ObservedAt: at, CheckedAt: at,
				State: domain.PlaybackIdle, Kind: domain.PlaybackItemNone,
			},
		},
		{
			name: "a track, playing",
			in: &spotify.Playback{
				IsPlaying: true, ProgressMs: ms(161000), CurrentlyPlayingType: "track",
				Device: &spotify.Device{Name: "Kitchen speaker", Type: "Speaker"},
				Item: &spotify.PlaybackItem{
					ID: "track-1", Name: "The Wheel", Type: "track", DurationMs: 255000,
					Artists: []spotify.Artist{{Name: "SOHN"}},
				},
			},
			want: domain.NowPlaying{
				ObservedAt: at, CheckedAt: at,
				State: domain.PlaybackPlaying, Kind: domain.PlaybackItemTrack,
				TrackID: "track-1", Title: "The Wheel", Artist: "SOHN",
				ProgressMs: ms(161000), DurationMs: ms(255000),
				DeviceName: "Kitchen speaker",
			},
		},
		{
			name: "a track, paused",
			in: &spotify.Playback{
				IsPlaying: false, ProgressMs: ms(1000), CurrentlyPlayingType: "track",
				Item: &spotify.PlaybackItem{
					ID: "track-1", Name: "The Wheel", Type: "track", DurationMs: 255000,
					Artists: []spotify.Artist{{Name: "SOHN"}, {Name: "Kwabs"}},
				},
			},
			want: domain.NowPlaying{
				ObservedAt: at, CheckedAt: at,
				State: domain.PlaybackPaused, Kind: domain.PlaybackItemTrack,
				TrackID: "track-1", Title: "The Wheel", Artist: "SOHN, Kwabs",
				ProgressMs: ms(1000), DurationMs: ms(255000),
			},
		},
		{
			name: "a local file has a name and no catalogue id",
			in: &spotify.Playback{
				IsPlaying: true, CurrentlyPlayingType: "track",
				Item: &spotify.PlaybackItem{
					Name: "demo-2004.mp3", Type: "track", IsLocal: true, DurationMs: 180000,
					Artists: []spotify.Artist{{Name: "Unreleased"}},
				},
			},
			want: domain.NowPlaying{
				ObservedAt: at, CheckedAt: at,
				State: domain.PlaybackPlaying, Kind: domain.PlaybackItemLocal,
				Title: "demo-2004.mp3", Artist: "Unreleased", DurationMs: ms(180000),
			},
		},
		{
			name: "a podcast episode names its show rather than an artist",
			in: &spotify.Playback{
				IsPlaying: true, CurrentlyPlayingType: "episode",
				Item: &spotify.PlaybackItem{
					ID: "ep-1", Name: "The one about ducks", Type: "episode",
					DurationMs: 3600000, Show: &spotify.Show{Name: "Ducks Weekly"},
				},
			},
			want: domain.NowPlaying{
				ObservedAt: at, CheckedAt: at,
				State: domain.PlaybackPlaying, Kind: domain.PlaybackItemEpisode,
				Title: "The one about ducks", Artist: "Ducks Weekly",
				DurationMs: ms(3600000),
			},
		},
		{
			name: "an advert has no item at all",
			in: &spotify.Playback{
				IsPlaying: true, CurrentlyPlayingType: "ad", Item: nil,
				Device: &spotify.Device{Name: "Kitchen speaker"},
			},
			want: domain.NowPlaying{
				ObservedAt: at, CheckedAt: at,
				State: domain.PlaybackPlaying, Kind: domain.PlaybackItemUnknown,
				DeviceName: "Kitchen speaker",
			},
		},
		{
			name: "a type this client does not know keeps none of its description",
			in: &spotify.Playback{
				IsPlaying: true, CurrentlyPlayingType: "unknown",
				Item: &spotify.PlaybackItem{
					ID: "ch-1", Name: "Chapter 4", Type: "chapter", DurationMs: 900000,
				},
			},
			want: domain.NowPlaying{
				ObservedAt: at, CheckedAt: at,
				State: domain.PlaybackPlaying, Kind: domain.PlaybackItemUnknown,
			},
		},
		{
			name: "a 200 carrying neither an item nor a type is an idle player",
			in:   &spotify.Playback{IsPlaying: false},
			want: domain.NowPlaying{
				ObservedAt: at, CheckedAt: at,
				State: domain.PlaybackIdle, Kind: domain.PlaybackItemNone,
			},
		},
		{
			name: "a track with no id is a local file however Spotify labels it",
			in: &spotify.Playback{
				IsPlaying: true, CurrentlyPlayingType: "track",
				Item: &spotify.PlaybackItem{Name: "Untitled", Type: "", DurationMs: 1000},
			},
			want: domain.NowPlaying{
				ObservedAt: at, CheckedAt: at,
				State: domain.PlaybackPlaying, Kind: domain.PlaybackItemLocal,
				Title: "Untitled", DurationMs: ms(1000),
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := observe(tc.in, at)
			if got.State != tc.want.State || got.Kind != tc.want.Kind {
				t.Fatalf("State/Kind = %q/%q, want %q/%q",
					got.State, got.Kind, tc.want.State, tc.want.Kind)
			}
			if got.Title != tc.want.Title || got.Artist != tc.want.Artist {
				t.Errorf("Title/Artist = %q/%q, want %q/%q",
					got.Title, got.Artist, tc.want.Title, tc.want.Artist)
			}
			if got.TrackID != tc.want.TrackID {
				t.Errorf("TrackID = %q, want %q", got.TrackID, tc.want.TrackID)
			}
			if got.DeviceName != tc.want.DeviceName {
				t.Errorf("DeviceName = %q, want %q", got.DeviceName, tc.want.DeviceName)
			}
			if !samePtr(got.ProgressMs, tc.want.ProgressMs) {
				t.Errorf("ProgressMs = %v, want %v", got.ProgressMs, tc.want.ProgressMs)
			}
			if !samePtr(got.DurationMs, tc.want.DurationMs) {
				t.Errorf("DurationMs = %v, want %v", got.DurationMs, tc.want.DurationMs)
			}
			if !got.ObservedAt.Equal(tc.want.ObservedAt) || !got.CheckedAt.Equal(tc.want.CheckedAt) {
				t.Errorf("ObservedAt/CheckedAt = %v/%v, want both %v",
					got.ObservedAt, got.CheckedAt, at)
			}
		})
	}
}

func samePtr(a, b *int) bool {
	switch {
	case a == nil && b == nil:
		return true
	case a == nil || b == nil:
		return false
	default:
		return *a == *b
	}
}
```

`newTestWatcher` and `accountsDue` are fakes you write in the same file. Build them against the interfaces below, which Step 3 defines: a `SpotifyAPI` that increments `checks` and returns whatever the test wants, a `Tokens` that returns `"token"`, and a store fake that increments `listings` and returns the supplied `[]accounts.DueAccount`. Alias `accountsDue = accounts.DueAccount` so the table above reads without the package qualifier. **Because `Deps.NowPlaying` is a concrete `*accounts.NowPlaying`, add a narrow `Store` interface to `Deps` instead** — see Step 3's `Observations` interface, which exists precisely so this test can run without a database.

- [ ] **Step 2: Run them and watch them fail**

Run: `go test -count=1 ./internal/nowplaying/`
Expected: FAIL — the package does not exist.

- [ ] **Step 3: Write the poller**

Create `internal/nowplaying/nowplaying.go`:

```go
// Package nowplaying polls what each connected listener is playing right now
// and records one row per account, so the dashboard can show it.
//
// Three properties define this package, and each is enforced by something other
// than a comment:
//
//  1. It never writes a listen. GET /me/player/recently-played remains the sole
//     ingestion path, because the sync poller's correctness rests on its cursor
//     advancing in the same transaction that commits the listens it covers, and
//     a second writer with a different view of what has been played would
//     produce duplicates that the dedupe rules catch by accident rather than by
//     design. This package therefore imports nothing that can write one, and a
//     test reads its own import list to say so.
//
//  2. It never parks an account. A 403 here means only that a grant does not
//     carry user-read-playback-state, which is the ordinary state of every
//     account connected before Phase 2a — see internal/sync/account.go's
//     forbidden(). Deps names *accounts.NowPlaying rather than *accounts.Repo,
//     so this package has no handle on the credentials repository and could not
//     park an account if it tried.
//
//  3. It cannot stall anything else. Every request goes out on internal/spotify's
//     classNowPlaying, a rate budget of its own: a 429 pauses this loop and is
//     never recorded, so it cannot 409 "sync now" for other users, stop
//     enrichment, or stop the recently-played poller.
//
// And it does nothing at all unless ENCORE_NOWPLAYING_INTERVAL is set. Not
// "defaults to off": Run returns before it lists a single account.
package nowplaying

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"strings"
	stdsync "sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/RequiDev/encore/internal/config"
	"github.com/RequiDev/encore/internal/domain"
	"github.com/RequiDev/encore/internal/logging"
	"github.com/RequiDev/encore/internal/spotify"
	"github.com/RequiDev/encore/internal/store"
	"github.com/RequiDev/encore/internal/store/accounts"
)

// scopeReadPlaybackState is the grant this poller needs. It shipped in
// config.DefaultScopes() in Phase 2a, so an account connected before that
// carries neither it nor the four other Phase 2 scopes, and that is the
// ordinary state of an older account forever rather than a fault to repair.
const scopeReadPlaybackState = "user-read-playback-state"

// concurrency is how many accounts one tick checks at once.
//
// Not a configuration key, deliberately: this phase adds exactly one, and the
// interval is the only lever that changes what the feature costs. Four is
// enough to clear any instance inside a thirty-second tick and small enough that
// a tick never presents the whole instance to Spotify at once.
const concurrency = 4

// accountsPerTick bounds one tick's work. Accounts are handed out
// least-recently-checked first, so anything left over is picked up next tick
// rather than starved.
const accountsPerTick = 200

// tickJitter is the fraction of the interval each delay is randomised by, for
// the reason internal/sync gives: several worker containers started by the same
// deployment would otherwise poll on the same second for ever.
const tickJitter = 0.2

// SpotifyAPI is the part of *spotify.Client this package uses.
//
// One method, and that is the whole of this package's reach into Spotify. A nil
// result with a nil error means nothing is playing.
type SpotifyAPI interface {
	CurrentlyPlaying(ctx context.Context, accessToken string) (*spotify.Playback, error)
}

// Tokens supplies a usable Spotify access token for one account, refreshing and
// persisting it when necessary.
//
// This is *sync.Poller's exported AccessToken, exactly as internal/library uses
// it. Declared as an interface rather than imported directly so this package can
// be tested without a database — and, more importantly here, so that the one
// thing in this package which *can* park an account (a refresh Spotify rejects
// outright, which is broken for every feature and not only this one) is
// somebody else's method rather than this package's.
type Tokens interface {
	AccessToken(ctx context.Context, userID uuid.UUID) (string, error)
}

// Observations is the part of accounts.NowPlaying this package uses.
//
// An interface for the ordinary reason — the loop is exercised without a
// database — and the set is deliberately narrow: list, record, record a failure.
// There is no method here that touches any other table.
type Observations interface {
	ListDue(ctx context.Context, q store.Querier, olderThan time.Time, limit int) ([]accounts.DueAccount, error)
	Record(ctx context.Context, q store.Querier, userID uuid.UUID, n domain.NowPlaying) error
	RecordFailure(ctx context.Context, q store.Querier, userID uuid.UUID, t time.Time) error
}

// Deps are the collaborators a Watcher needs.
//
// NowPlaying is the single-table repository, not *accounts.Repo. That is load
// bearing: with no handle on accounts.Credentials this package cannot call
// MarkNeedsReauth, so the rule that an optional-scope 403 never parks an account
// holds by construction rather than by review.
type Deps struct {
	Store      *store.Store
	NowPlaying Observations
	Spotify    SpotifyAPI
	Tokens     Tokens
	Logger     *slog.Logger
	// Now is injectable so tests can control timestamps without sleeping.
	Now func() time.Time
}

// Watcher polls the player of every connected account that granted
// user-read-playback-state.
//
// It holds no durable state: which accounts are due lives in
// now_playing.checked_at, so a Watcher can be killed at any instant and the next
// process simply asks the database again.
type Watcher struct {
	cfg config.NowPlaying
	dep Deps
	now func() time.Time
	log *slog.Logger

	// rnd supplies the tick jitter in [0,1). Injectable so a test can make the
	// schedule deterministic.
	rnd func() float64
}

// New builds a Watcher. Every collaborator it names is required; the logger and
// the clock default to sensible values.
func New(cfg config.NowPlaying, deps Deps) (*Watcher, error) {
	if deps.Store == nil {
		return nil, errors.New("nowplaying: a store is required")
	}
	if deps.NowPlaying == nil {
		return nil, errors.New("nowplaying: the now-playing repository is required")
	}
	if deps.Spotify == nil {
		return nil, errors.New("nowplaying: a Spotify client is required")
	}
	if deps.Tokens == nil {
		return nil, errors.New("nowplaying: a token source is required")
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	if deps.Now == nil {
		deps.Now = time.Now
	}
	// No interval default. Zero means off, and inventing one here would defeat
	// the whole point of the key being opt-in.
	return &Watcher{
		cfg: cfg,
		dep: deps,
		now: deps.Now,
		log: deps.Logger.With("component", "nowplaying"),
		rnd: rand.Float64,
	}, nil
}

// Run checks every due account, forever, until ctx is cancelled.
//
// It returns nil immediately when no interval is configured, which the worker's
// supervisor treats as a loop that has finished and leaves stopped. That is the
// whole of "unset means off": not a loop that wakes and finds nothing to do, but
// a loop that never starts.
func (w *Watcher) Run(ctx context.Context) error {
	if !w.cfg.Enabled() {
		w.log.Info("now-playing polling is disabled; ENCORE_NOWPLAYING_INTERVAL is not set")
		return nil
	}
	w.log.Info("now-playing polling started",
		"interval", w.cfg.Interval.String(), "concurrency", concurrency)

	// The first delay is drawn from the whole interval rather than jittered
	// around it, which is what actually spreads a fleet that all started at
	// once; subsequent delays only keep them from converging again.
	timer := time.NewTimer(w.firstDelay())
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-timer.C:
		}

		if _, err := w.RunOnce(ctx); err != nil && ctx.Err() == nil {
			// Listing the work failed, which is an infrastructure problem
			// rather than an account problem: log it and wait for the next
			// tick instead of spinning against a database that is down.
			w.log.Error("now-playing tick failed", logging.Err(err))
		}
		timer.Reset(w.nextDelay())
	}
}

// RunOnce checks every account that is currently due and reports how many were
// actually checked.
//
// Exported so a worker supervisor, or a test, can drive one tick without owning
// the schedule.
func (w *Watcher) RunOnce(ctx context.Context) (int, error) {
	due, err := w.dep.NowPlaying.ListDue(
		ctx, w.dep.Store.DB(), w.now().Add(-w.cfg.Interval), accountsPerTick)
	if err != nil {
		return 0, fmt.Errorf("list accounts due for a playback check: %w", err)
	}
	if len(due) == 0 {
		return 0, nil
	}

	var (
		checked atomic.Int64
		wg      stdsync.WaitGroup
		sem     = make(chan struct{}, concurrency)
	)

	// No shared error group: one account's failure must never cancel another's
	// check, so each is isolated and reports itself.
dispatch:
	for _, account := range due {
		select {
		case <-ctx.Done():
			break dispatch
		case sem <- struct{}{}:
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			if w.check(ctx, account) {
				checked.Add(1)
			}
		}()
	}
	wg.Wait()

	return int(checked.Load()), nil
}

// check reads one account's player and records what it found.
//
// It never returns an error. Everything that can go wrong with one grant is
// recorded on that account's own row, where the card that concerns that listener
// shows it in words, because one broken connection must not cost anybody else
// their display.
func (w *Watcher) check(ctx context.Context, account accounts.DueAccount) bool {
	log := w.log.With("user", account.UserID.String())

	// Before the request, not through a 403. The SQL predicate in ListDue
	// already excludes a grant without this scope; this is the check that still
	// holds if somebody widens the predicate later, and it costs nothing.
	if !hasScope(account.Scopes, scopeReadPlaybackState) {
		log.Debug("account has not granted user-read-playback-state; skipping without a request")
		return false
	}

	token, err := w.dep.Tokens.AccessToken(ctx, account.UserID)
	if err != nil {
		w.recordFailure(ctx, account.UserID, log, "could not get an access token", err)
		return false
	}

	pb, err := w.dep.Spotify.CurrentlyPlaying(ctx, token)
	if err != nil {
		if ctx.Err() != nil {
			// Shutting down. The next tick picks the account up and nothing is
			// lost, so this is not a failure to record.
			return false
		}
		w.recordFailure(ctx, account.UserID, log, "could not read the player", err)
		return false
	}

	// pb == nil is a 204: nothing is playing. Not an error, and the commonest
	// answer this endpoint gives.
	if err := w.dep.NowPlaying.Record(ctx, w.dep.Store.DB(), account.UserID, observe(pb, w.now())); err != nil {
		log.Error("could not record what is playing", logging.Err(err))
		return false
	}
	return true
}

// recordFailure notes a check that did not succeed, so the card can say the
// display is stale and how stale it is.
//
// The observation columns are untouched: a failed check must not throw away the
// last true thing Encore knew. A 403 lands here like anything else — it is
// deliberately not special-cased into parking the account, because a grant
// without user-read-playback-state still syncs a listening history perfectly.
func (w *Watcher) recordFailure(
	ctx context.Context, userID uuid.UUID, log *slog.Logger, what string, cause error,
) {
	var paused *spotify.PausedError
	switch {
	case errors.As(cause, &paused):
		// Expected and self-healing: the poller's own budget is paused, so no
		// request will reach Spotify until it lifts. Nothing else is affected,
		// which is the entire reason this budget exists.
		log.Debug("now-playing checks are paused by a rate limit",
			"resumes_at", paused.Until.UTC().Format(time.RFC3339))
	default:
		log.Warn(what, logging.Err(cause))
	}

	// Detached from ctx: an account whose check just failed must still have that
	// recorded when the process is shutting down, or the card would report a
	// success that never happened.
	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()

	if err := w.dep.NowPlaying.RecordFailure(fctx, w.dep.Store.DB(), userID, w.now()); err != nil {
		log.Error("could not record a failed playback check", logging.Err(err))
	}
}

// observe turns Spotify's answer into the row Encore stores.
//
// Pure, and the only place the "what is playing" question is answered, so every
// distinction the interface draws is decided here once rather than re-derived by
// each reader. A nil playback is a 204: nothing is playing.
func observe(pb *spotify.Playback, at time.Time) domain.NowPlaying {
	out := domain.NowPlaying{
		ObservedAt: at,
		CheckedAt:  at,
		State:      domain.PlaybackIdle,
		Kind:       domain.PlaybackItemNone,
	}
	if pb == nil {
		return out
	}

	out.Kind = kindOf(pb)
	if out.Kind == domain.PlaybackItemNone {
		// A 200 body carrying neither an item nor a type: 204 in a longer form.
		return out
	}
	if pb.IsPlaying {
		out.State = domain.PlaybackPlaying
	} else {
		out.State = domain.PlaybackPaused
	}
	if pb.Device != nil {
		out.DeviceName = pb.Device.Name
	}
	if pb.ProgressMs != nil && *pb.ProgressMs >= 0 {
		ms := *pb.ProgressMs
		out.ProgressMs = &ms
	}

	if item := pb.Item; item != nil && out.Kind != domain.PlaybackItemUnknown {
		out.Title = item.Name
		if item.DurationMs > 0 {
			d := item.DurationMs
			out.DurationMs = &d
		}
		switch out.Kind {
		case domain.PlaybackItemTrack:
			out.TrackID = item.ID
			out.Artist = artistNames(item.Artists)
		case domain.PlaybackItemLocal:
			// No id to keep: a local file has no catalogue identity, which is
			// exactly why it can be named and never linked.
			out.Artist = artistNames(item.Artists)
		case domain.PlaybackItemEpisode:
			// The show stands where an artist would. It is the same slot in the
			// interface — the line under the title — and a podcast has no
			// artist to put there.
			if item.Show != nil {
				out.Artist = item.Show.Name
			}
		}
	}
	// Nothing descriptive survives for an unknown item, which is why the branch
	// above skips it: Spotify's own label for an advert is not a title, and
	// rendering it as one would put an advertiser's name where a listener
	// expects their music. The interface has one sentence for this state and
	// needs no name to render it.
	return out
}

// kindOf classifies what is in the player.
//
// The order of the tests is load bearing. Type is read before the local-file
// check because an episode Spotify happened to send without an id would
// otherwise be reported as a local file, which carries a different sentence in
// the interface and a different claim about the listener's history.
func kindOf(pb *spotify.Playback) domain.PlaybackItemKind {
	item := pb.Item
	if item == nil {
		if pb.CurrentlyPlayingType == "" && !pb.IsPlaying {
			return domain.PlaybackItemNone
		}
		// An advert, or a type this client does not know.
		return domain.PlaybackItemUnknown
	}
	if item.Type == "episode" {
		return domain.PlaybackItemEpisode
	}
	if item.IsLocal || item.ID == "" {
		return domain.PlaybackItemLocal
	}
	if item.Type == "track" || item.Type == "" {
		// Spotify has been observed to omit the type on a track, so an empty
		// value is read as one rather than discarded as unknown.
		return domain.PlaybackItemTrack
	}
	return domain.PlaybackItemUnknown
}

// artistNames joins credited artists the way the interface reads them.
func artistNames(artists []spotify.Artist) string {
	names := make([]string, 0, len(artists))
	for _, a := range artists {
		if a.Name != "" {
			names = append(names, a.Name)
		}
	}
	return strings.Join(names, ", ")
}

// hasScope reports whether a stored grant carries a scope.
//
// It splits on spaces for the reason spotify.MissingScopes does: Spotify returns
// granted scopes space-separated in one string, and an account connected before
// Encore split them has one such string in its column.
func hasScope(granted []string, want string) bool {
	for _, g := range granted {
		for f := range strings.SplitSeq(g, " ") {
			if f == want {
				return true
			}
		}
	}
	return false
}

// firstDelay spreads the first tick of freshly started processes across a whole
// interval.
func (w *Watcher) firstDelay() time.Duration {
	return time.Duration(w.rnd() * float64(w.cfg.Interval))
}

// nextDelay is the configured interval with symmetric jitter applied, so
// processes that happen to align drift apart again instead of staying in step.
func (w *Watcher) nextDelay() time.Duration {
	spread := float64(w.cfg.Interval) * tickJitter
	d := float64(w.cfg.Interval) - spread/2 + w.rnd()*spread
	if d < float64(time.Second) {
		return time.Second
	}
	return time.Duration(d)
}
```

- [ ] **Step 4: Run the unit tests**

Run: `go test -count=1 ./internal/nowplaying/ -v`
Expected: PASS, all nine `observe` subtests and the three others.

- [ ] **Step 5: Write the failing integration tests**

Append to `test/integration/nowplaying_test.go`. These need a fake `SpotifyAPI` and a fake `Tokens`; follow `test/integration/libraryworker_test.go` for how that suite builds a worker against the harness.

```go
// TestThePollerAddsNoListens is the spec's read-only observer rule, asserted at
// runtime rather than through the import graph.
//
// §2.2: running the poller across a listening session must add exactly zero
// rows to listens.
//
// Fails when: the poller gains any write path to listens. The import-graph test
// in internal/nowplaying catches the obvious way in; this catches an indirect
// one, through a dependency that grows a write of its own.
func TestThePollerAddsNoListens(t *testing.T) {
	e := harness.New(t)
	user := e.NewUser("np-no-listens")
	connectWithPlaybackScope(t, e, user.ID)

	before := countListens(t, e, user.ID)

	w := newWatcher(t, e, playingResponse("track-1", "The Wheel"))
	for range 5 {
		if _, err := w.RunOnce(e.Ctx()); err != nil {
			t.Fatalf("RunOnce: %v", err)
		}
	}

	if after := countListens(t, e, user.ID); after != before {
		t.Fatalf("listens went from %d to %d; the now-playing poller is a "+
			"read-only observer and must never ingest", before, after)
	}
}

// TestAForbiddenCheckNeverParksTheAccount pins internal/sync/account.go:296's
// rule for this endpoint.
//
// A 403 here means only that the grant does not carry user-read-playback-state.
// Parking the account would stop ingesting a listening history that reads
// perfectly, over a feature the listener may not even have noticed.
//
// Fails when: the poller gains a credentials repository and calls
// MarkNeedsReauth on a 403 — sync_state below then reads needs_reauth.
func TestAForbiddenCheckNeverParksTheAccount(t *testing.T) {
	e := harness.New(t)
	user := e.NewUser("np-forbidden")
	connectWithPlaybackScope(t, e, user.ID)

	w := newWatcher(t, e, forbiddenResponse())
	if _, err := w.RunOnce(e.Ctx()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	creds, err := e.Accounts.Credentials.Get(e.Ctx(), e.Store.DB(), user.ID)
	if err != nil {
		t.Fatalf("Get credentials: %v", err)
	}
	if creds.SyncState == domain.SyncStateNeedsReauth {
		t.Fatal("a 403 on the now-playing check parked the account; an optional " +
			"read scope's absence must never stop ingesting a listening history")
	}
	if creds.LastSyncError != "" {
		t.Errorf("LastSyncError = %q; a now-playing failure belongs on the "+
			"now_playing row, not on the sync record", creds.LastSyncError)
	}

	got, err := e.Accounts.NowPlaying.Get(e.Ctx(), e.Store.DB(), user.ID)
	if err != nil {
		t.Fatalf("Get now playing: %v", err)
	}
	if !got.Failed {
		t.Error("the failed check was not recorded, so the card cannot say the " +
			"display is stale")
	}
}

// TestAnAccountNeedingReauthIsNeverChecked pins the other exclusion in the due
// query.
//
// Fails when: the sync_state predicate is dropped from listDueSQL — a broken
// grant is then polled every interval to be told the same thing.
func TestAnAccountNeedingReauthIsNeverChecked(t *testing.T) {
	e := harness.New(t)
	user := e.NewUser("np-parked")
	connectWithPlaybackScope(t, e, user.ID)
	if err := e.Accounts.Credentials.MarkNeedsReauth(e.Ctx(), e.Store.DB(), user.ID,
		"reconnect"); err != nil {
		t.Fatalf("MarkNeedsReauth: %v", err)
	}

	due, err := e.Accounts.NowPlaying.ListDue(e.Ctx(), e.Store.DB(), time.Now().UTC(), 100)
	if err != nil {
		t.Fatalf("ListDue: %v", err)
	}
	for _, a := range due {
		if a.UserID == user.ID {
			t.Fatal("a needs_reauth account is queued for a playback check")
		}
	}
}
```

Write `connectWithPlaybackScope`, `countListens`, `newWatcher`, `playingResponse` and `forbiddenResponse` as helpers in this file, following the conventions of the neighbouring integration tests. `connectWithPlaybackScope` upserts credentials whose `Scopes` include `"user-read-playback-state"` and whose `SyncState` is `domain.SyncStateOK`; `newWatcher` builds a `nowplaying.Watcher` with `config.NowPlaying{Interval: 30 * time.Second}`, `e.Accounts.NowPlaying`, a stub `Tokens` returning `"token"`, and the supplied fake `SpotifyAPI`.

- [ ] **Step 6: Run them and watch them fail, then pass**

```bash
export ENCORE_TEST_DATABASE_URL="postgres://encore:encore@localhost:5433/encore?sslmode=disable"
go test -tags=integration -count=1 -p 1 -run 'TestThePollerAddsNoListens|TestAForbidden|TestAnAccountNeedingReauth' ./test/integration/
```

Expected: FAIL first (helpers undefined), then PASS once they and the wiring exist.

- [ ] **Step 7: Wire it into the worker**

In `cmd/encore-worker/main.go`, after the `sup.Add("library", …)` block:

```go
	watcher, err := nowplaying.New(cfg.NowPlaying, nowplaying.Deps{
		Store:      db,
		NowPlaying: accountsRepo.NowPlaying,
		Spotify:    client,
		// The token refresh dance, including parking an account when Spotify
		// has revoked the grant outright, belongs to recently-played sync,
		// which cannot function without its own scope. This loop borrows it
		// rather than duplicating it — and borrowing it as an interface is
		// what keeps this package unable to park an account for a reason of
		// its own.
		Tokens: poller,
		Logger: lg,
	})
	if err != nil {
		return err
	}
	// Unconditional for the same reason as sync and library: with
	// ENCORE_NOWPLAYING_INTERVAL unset, Run says so once and returns nil, which
	// the supervisor treats as a loop that has finished.
	sup.Add("nowplaying", watcher.Run)
```

Add the import. Update the package comment at `cmd/encore-worker/main.go:1-3` — the loop list there is already two loops out of date, and Task 7 finishes that sweep; here, just make sure the new loop is named.

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
git add internal/nowplaying/ cmd/encore-worker/main.go test/integration/nowplaying_test.go
git commit -m "$(cat <<'MSG'
Nowplaying: a poller that observes and can write nothing

The spec says /me/player must not create rows in listens, and the reason is not
stylistic: the sync poller's correctness rests on its cursor advancing in the
same transaction that commits the listens it covers, and a second writer with a
different view of what has been played would produce duplicates the dedupe rules
catch by accident rather than by design.

So the package imports nothing that can write one, and a test reads its own
import list to say so. A comment can be ignored; an import that does not exist
cannot be used.

Deps names *accounts.NowPlaying rather than *accounts.Repo for the same kind of
reason. A 403 here means only that a grant lacks user-read-playback-state, which
is the ordinary state of every account connected before Phase 2a, and parking
one would stop ingesting a listening history that reads perfectly. With no
handle on the credentials repository the poller could not park an account if it
tried.

observe() is pure and is the only place the "what is playing" question is
answered, because every distinction the interface draws — a paused track, a
podcast, a local file, an advert, an idle player, and a check that never
happened — is a distinction that can be got wrong exactly once here instead of
once per reader.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
MSG
)"
```

---

## Task 5: `GET /api/nowplaying`, and never on a share link

**Files:**
- Create: `internal/httpapi/nowplaying.go`
- Modify: `internal/httpapi/dto.go`
- Modify: `internal/httpapi/router.go` (one route)
- Modify: `internal/httpapi/server.go` (`nowPlayingStore`, the field, the `New` check)
- Modify: `internal/httpapi/httpapi_test.go` (the fake repository set, if it has one)
- Create: `internal/httpapi/nowplaying_test.go`
- Modify: `docs/api.md`

**Interfaces:**
- Consumes: `domain.NowPlaying` and `Observed()` (Task 3); `accounts.NowPlaying.Get` (Task 3); `config.NowPlaying.Enabled()` and `.Interval` (Task 2); `requireUser`, `writeJSON`, `writeError`, `s.credentials`, `s.cfg` (existing).
- Produces:
  - `httpapi.NowPlayingResponse` and `httpapi.NowPlayingObservation`
  - route `GET /api/nowplaying` → `handleNowPlaying`
  - `nowPlayingStore` on `Server`

**The endpoint makes no Spotify request.** It reads the stored row and nothing else, so an open dashboard costs no quota however many tabs are on it, and a browser cannot make Encore call Spotify. That is asserted, not merely intended.

- [ ] **Step 1: Write the failing handler tests**

Create `internal/httpapi/nowplaying_test.go`. Follow `internal/httpapi/httpapi_test.go` for how the suite builds a `Server` with fake repositories; the snippets below name what each test must assert.

```go
package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/RequiDev/encore/internal/domain"
)

// TestNowPlayingRequiresASession pins that presence is never public.
//
// Fails when: the route is moved outside the /api subtree, or the handler stops
// calling requireUser.
func TestNowPlayingRequiresASession(t *testing.T) {
	srv := newTestServer(t, testDeps{})
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/nowplaying", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

// TestNowPlayingReportsAnInstanceWithThePollerOff pins the answer a client uses
// to decide the card should not exist at all.
//
// Fails when: enabled is computed from anything but cfg.NowPlaying.Enabled(), or
// intervalSeconds reports a default the poller does not run on — the client
// would then poll an endpoint whose answer can never change.
func TestNowPlayingReportsAnInstanceWithThePollerOff(t *testing.T) {
	got := getNowPlaying(t, testDeps{}) // no interval configured
	if got.Enabled {
		t.Error("enabled = true with no interval configured")
	}
	if got.IntervalSeconds != 0 {
		t.Errorf("intervalSeconds = %d, want 0", got.IntervalSeconds)
	}
	if got.Observation != nil {
		t.Errorf("observation = %+v, want null", got.Observation)
	}
}

// TestNowPlayingReportsAMissingScope pins the per-account gate, computed on the
// server for the reason /api/me computes missingScopes there: two copies of the
// required scope would drift, and the TypeScript one would drift silently.
//
// Fails when: scopeGranted is hard-coded true, or is computed from the presence
// of a row — an account that has simply never been polled would then be told to
// reconnect Spotify for no reason.
func TestNowPlayingReportsAMissingScope(t *testing.T) {
	got := getNowPlaying(t, testDeps{
		interval: 30 * time.Second,
		scopes:   []string{"user-read-recently-played"},
	})
	if !got.Enabled {
		t.Fatal("enabled = false with an interval configured")
	}
	if got.ScopeGranted {
		t.Error("scopeGranted = true for a grant without user-read-playback-state")
	}
}

// TestNowPlayingSeparatesNeverCheckedFromNothingPlaying is the distinction the
// whole feature turns on, asserted at the boundary the client reads.
//
// Fails when: the handler maps a never-observed row to an observation with state
// "idle" — the two payloads below then become identical and the card cannot
// tell "we have not looked" from "your player is silent".
func TestNowPlayingSeparatesNeverCheckedFromNothingPlaying(t *testing.T) {
	at := time.Date(2026, time.July, 31, 9, 30, 0, 0, time.UTC)

	never := getNowPlaying(t, testDeps{
		interval: 30 * time.Second,
		scopes:   []string{"user-read-playback-state"},
		row: domain.NowPlaying{
			State: domain.PlaybackUnknown, Kind: domain.PlaybackItemNone,
			CheckedAt: at, Failed: true,
		},
	})
	if never.Observation != nil {
		t.Fatalf("observation = %+v for an account never successfully checked, want null",
			never.Observation)
	}
	if never.CheckedAt == nil || !never.CheckedAt.Equal(at) {
		t.Errorf("checkedAt = %v, want %v", never.CheckedAt, at)
	}
	if !never.Failed {
		t.Error("failed = false after a failed check")
	}

	idle := getNowPlaying(t, testDeps{
		interval: 30 * time.Second,
		scopes:   []string{"user-read-playback-state"},
		row: domain.NowPlaying{
			ObservedAt: at, State: domain.PlaybackIdle, Kind: domain.PlaybackItemNone,
			CheckedAt: at,
		},
	})
	if idle.Observation == nil {
		t.Fatal("observation = null for an account whose player was seen idle")
	}
	if idle.Observation.State != string(domain.PlaybackIdle) {
		t.Errorf("state = %q, want idle", idle.Observation.State)
	}
	if idle.Failed {
		t.Error("failed = true after a successful check")
	}
}

// TestNowPlayingOnlyLinksATrackTheCatalogueHolds pins that trackId is a promise
// the client can act on.
//
// Fails when: the handler copies TrackID regardless of TrackKnown — the card
// then renders a link to /tracks/{id} for a track no page exists for.
func TestNowPlayingOnlyLinksATrackTheCatalogueHolds(t *testing.T) {
	at := time.Date(2026, time.July, 31, 9, 30, 0, 0, time.UTC)
	row := domain.NowPlaying{
		ObservedAt: at, State: domain.PlaybackPlaying, Kind: domain.PlaybackItemTrack,
		TrackID: "track-1", Title: "The Wheel", Artist: "SOHN",
		CheckedAt: at, TrackKnown: false,
	}
	got := getNowPlaying(t, testDeps{
		interval: 30 * time.Second,
		scopes:   []string{"user-read-playback-state"},
		row:      row,
	})
	if got.Observation == nil {
		t.Fatal("observation = null")
	}
	if got.Observation.TrackID != "" {
		t.Errorf("trackId = %q for a track the catalogue has never seen; a link "+
			"to a page that does not exist is worse than no link", got.Observation.TrackID)
	}
	if got.Observation.Title != "The Wheel" {
		t.Errorf("title = %q; the name is still shown, only the link is withheld",
			got.Observation.Title)
	}

	row.TrackKnown = true
	known := getNowPlaying(t, testDeps{
		interval: 30 * time.Second,
		scopes:   []string{"user-read-playback-state"},
		row:      row,
	})
	if known.Observation == nil || known.Observation.TrackID != "track-1" {
		t.Errorf("trackId = %+v, want track-1 once the catalogue holds it", known.Observation)
	}
}

// TestNowPlayingMakesNoSpotifyRequest pins that a browser cannot make Encore
// call Spotify.
//
// The card polls this endpoint on the instance's own interval, from every open
// tab. If the handler ever fetched, a dashboard left open in three tabs would
// triple the feature's cost and put that traffic on whichever budget the handler
// happened to use.
//
// Fails when: the handler grows a refresh-on-read, which is the natural-looking
// way to make the card feel fresher.
func TestNowPlayingMakesNoSpotifyRequest(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomicAdd(&calls, 1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	_ = getNowPlaying(t, testDeps{
		interval:     30 * time.Second,
		scopes:       []string{"user-read-playback-state"},
		spotifyBase:  srv.URL,
		countSpotify: &calls,
	})
	if calls != 0 {
		t.Fatalf("%d Spotify requests were made serving GET /api/nowplaying, want 0", calls)
	}
}

// TestASharedLinkCarriesNoNowPlaying pins the spec's §2.3 rule.
//
// "Real-time presence is exactly the concern internal/domain/share.go was
// written around — a share exposes what somebody listens to, never when they
// are awake."
//
// Asserted against the response bytes rather than against a struct field,
// because the defect this guards is somebody adding the field, and a
// struct-field assertion would have to be updated by the same person adding it.
//
// Fails when: NowPlayingObservation is added to the shared-stats payload under
// any name — the device name and title below then appear in the body.
func TestASharedLinkCarriesNoNowPlaying(t *testing.T) {
	body := getSharedStatsBody(t, domain.NowPlaying{
		State: domain.PlaybackPlaying, Kind: domain.PlaybackItemTrack,
		Title: "The Wheel", Artist: "SOHN", DeviceName: "Kitchen speaker",
	})
	for _, forbidden := range []string{"nowPlaying", "nowplaying", "Kitchen speaker", "deviceName"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("the shared payload contains %q; a share exposes what somebody "+
				"listens to, never when they are awake", forbidden)
		}
	}
}
```

`testDeps`, `newTestServer`, `getNowPlaying`, `getSharedStatsBody` and `atomicAdd` are helpers to write against whatever `internal/httpapi/httpapi_test.go` already provides — that file builds a `Server` with fake repositories today, so extend its fake set with a now-playing fake rather than inventing a second harness. `getNowPlaying` performs the request with an authenticated session and decodes into `NowPlayingResponse`.

- [ ] **Step 2: Run them and watch them fail**

Run: `go test -count=1 -run 'TestNowPlaying|TestASharedLink' ./internal/httpapi/`
Expected: FAIL to compile — `NowPlayingResponse` undefined.

- [ ] **Step 3: Add the DTO**

In `internal/httpapi/dto.go`, in the section that holds `StatusResponse`:

```go
// NowPlayingResponse is GET /api/nowplaying: what the caller is playing right
// now, as far as this instance has been able to tell.
//
// It answers three questions that are routinely conflated, and keeps them apart
// structurally rather than by convention:
//
//   - does this instance poll at all (Enabled);
//   - may it poll *this* account (ScopeGranted);
//   - has it ever managed to (Observation being non-nil).
//
// Reading Observation is therefore the only way to learn what is playing, and a
// client cannot accidentally render "nothing is playing" for an account nobody
// has looked at — there is no state value to misread, because there is no
// observation at all.
type NowPlayingResponse struct {
	// Enabled reports that this instance runs the now-playing poller.
	// ENCORE_NOWPLAYING_INTERVAL unset means false, and the client renders no
	// card at all rather than an empty one.
	Enabled bool `json:"enabled"`
	// IntervalSeconds is how often the poller checks, and therefore how often
	// it is worth asking this endpoint again. Zero when Enabled is false.
	//
	// Sent so the client polls at the instance's own rate rather than guessing
	// one: a client that polled faster than the poller would ask repeatedly for
	// an answer that cannot have changed.
	IntervalSeconds int `json:"intervalSeconds"`
	// ScopeGranted reports that this account's grant includes
	// user-read-playback-state.
	//
	// Computed on the server against the stored grant, like /api/me's
	// missingScopes and for the same reason: two copies of the required scope
	// would drift and the TypeScript one would drift silently.
	ScopeGranted bool `json:"scopeGranted"`
	// CheckedAt is when the poller last tried, successfully or not. Absent when
	// it never has.
	CheckedAt *time.Time `json:"checkedAt"`
	// Failed reports that the attempt at CheckedAt did not succeed. Observation,
	// if present, is then the last one that did — which is what lets the client
	// say how stale the display is instead of discarding a true thing.
	Failed bool `json:"failed"`
	// Observation is the last successful observation, or null when there has
	// never been one.
	//
	// Null is "Encore has not managed to look". An Observation whose State is
	// "idle" is "nothing is playing". They are different facts and must not
	// share a sentence.
	Observation *NowPlayingObservation `json:"observation"`
}

// NowPlayingObservation is one successful look at a listener's player.
type NowPlayingObservation struct {
	// ObservedAt is when everything below was true.
	ObservedAt time.Time `json:"observedAt"`
	// State is "idle", "playing" or "paused". Never "unknown": that value means
	// there was no observation, which this type's absence already says.
	State string `json:"state"`
	// Kind is "none", "track", "episode", "local" or "unknown", and decides
	// which sentence the client renders — a podcast and a local file never
	// become listens, and an advert cannot be named at all.
	Kind string `json:"kind"`
	// Title and Artist are what Spotify called it. Empty for an unknown item,
	// which carries no description by design.
	Title  string `json:"title"`
	Artist string `json:"artist"`
	// TrackID names a track in Encore's own catalogue, so the client can link
	// to it. Empty when the item is not a track, or is a track Encore has never
	// seen — a link to a page that does not exist is worse than no link.
	TrackID string `json:"trackId"`
	// ProgressMs is progress at ObservedAt and is never extrapolated. The
	// client states the observation's age beside it rather than animating a bar
	// from a fact up to one interval old.
	ProgressMs *int `json:"progressMs"`
	DurationMs *int `json:"durationMs"`
	// DeviceName is empty when Spotify reported no device, and the client then
	// renders no device clause rather than an unknown one.
	DeviceName string `json:"deviceName"`
}
```

- [ ] **Step 4: Add the narrow interface and the field**

In `internal/httpapi/server.go`, beside `playlistStore`:

```go
// nowPlayingStore is the now-playing repository as this package needs it: one
// read, and nothing that writes. The poller owns every write to that table.
type nowPlayingStore interface {
	Get(ctx context.Context, q store.Querier, userID uuid.UUID) (domain.NowPlaying, error)
}
```

Add `nowPlaying nowPlayingStore` to `Server`, set it from `deps.Accounts.NowPlaying` in `New`, and extend the existing completeness check:

```go
	if deps.Accounts.Users == nil || deps.Accounts.Sessions == nil || deps.Accounts.Credentials == nil ||
		deps.Accounts.OAuthStates == nil || deps.Accounts.Settings == nil || deps.Accounts.NowPlaying == nil {
		return nil, errors.New("httpapi: the accounts repository is incomplete")
	}
```

- [ ] **Step 5: Write the handler**

Create `internal/httpapi/nowplaying.go`:

```go
package httpapi

import (
	"errors"
	"net/http"

	"github.com/RequiDev/encore/internal/domain"
)

// handleNowPlaying answers GET /api/nowplaying.
//
// It reads the stored observation and makes no Spotify request of its own. That
// is deliberate and load bearing: the dashboard polls this endpoint from every
// open tab, and a handler that fetched would multiply the feature's cost by the
// number of tabs somebody happens to have open — and would put that traffic on
// whichever budget the handler used, which is exactly the coupling this phase
// exists to avoid.
//
// Never reachable without a session, and never composed into a share link. Real
// time presence is precisely the concern internal/domain/share.go was written
// around: a share exposes what somebody listens to, never when they are awake.
func (s *Server) handleNowPlaying(w http.ResponseWriter, r *http.Request) {
	user, err := requireUser(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	ctx := r.Context()

	out := NowPlayingResponse{Enabled: s.cfg.NowPlaying.Enabled()}
	if out.Enabled {
		out.IntervalSeconds = int(s.cfg.NowPlaying.Interval.Seconds())
	}

	// The scope is read from the stored grant rather than inferred from whether
	// a row exists. An account that simply has not been reached yet must not be
	// told to reconnect Spotify: that is a prompt to fix something that is not
	// broken.
	creds, err := s.credentials.Get(ctx, s.querier, user.ID)
	switch {
	case err == nil:
		out.ScopeGranted = creds.HasScope(scopeReadPlaybackState)
	case errors.Is(err, domain.ErrNotFound):
		// No grant at all. Not connected is a state /api/me already reports and
		// the shell already renders; here it simply means no scope.
	default:
		writeError(w, r, err)
		return
	}

	row, err := s.nowPlaying.Get(ctx, s.querier, user.ID)
	if errors.Is(err, domain.ErrNotFound) {
		// Never checked. Everything stays zero, and Observation stays nil,
		// which is the payload's way of saying so.
		writeJSON(w, r, http.StatusOK, out)
		return
	}
	if err != nil {
		writeError(w, r, err)
		return
	}

	checked := row.CheckedAt.UTC()
	out.CheckedAt = &checked
	out.Failed = row.Failed
	out.Observation = toNowPlayingObservation(row)

	writeJSON(w, r, http.StatusOK, out)
}

// scopeReadPlaybackState is the grant the now-playing poller needs. Declared
// here rather than imported so this package keeps depending only on domain and
// config; it is the same literal config.DefaultScopes() lists.
const scopeReadPlaybackState = "user-read-playback-state"

// toNowPlayingObservation maps a stored row to the payload, or nil when there
// has never been a successful look.
//
// The nil is the point. domain.PlaybackUnknown is not a kind of silence, and
// returning an observation carrying it would hand the client a state value to
// misread as one.
func toNowPlayingObservation(n domain.NowPlaying) *NowPlayingObservation {
	if !n.Observed() {
		return nil
	}
	out := &NowPlayingObservation{
		ObservedAt: n.ObservedAt.UTC(),
		State:      string(n.State),
		Kind:       string(n.Kind),
		Title:      n.Title,
		Artist:     n.Artist,
		ProgressMs: n.ProgressMs,
		DurationMs: n.DurationMs,
		DeviceName: n.DeviceName,
	}
	// Only a track Encore's own catalogue holds is offered as a link. Spotify
	// names a track the instant it starts playing; Encore learns about it when
	// enrichment gets round to it, and a link in between would 404.
	if n.TrackKnown {
		out.TrackID = n.TrackID
	}
	return out
}
```

- [ ] **Step 6: Mount the route**

In `internal/httpapi/router.go`, after the `GET /api/status` line:

```go
	// What the caller is playing right now. Reads the stored observation and
	// never calls Spotify, so an open dashboard costs no quota. Deliberately
	// absent from the share surface below: a share exposes what somebody
	// listens to, never when they are awake.
	s.route(mux, "GET /api/nowplaying", s.handleNowPlaying)
```

- [ ] **Step 7: Document it**

In `docs/api.md`, add a section after "Status" and before "Operational endpoints":

```markdown
## Now playing

`GET /api/nowplaying`

What the caller is playing right now, as far as this instance has been able to tell. Backs the
dashboard's now-playing card.

**This endpoint never calls Spotify.** It reads the observation the worker's poller stored, so an
open dashboard costs no quota however many tabs are on it.

```json
{
  "enabled": true,
  "intervalSeconds": 30,
  "scopeGranted": true,
  "checkedAt": "2026-07-31T09:30:12Z",
  "failed": false,
  "observation": {
    "observedAt": "2026-07-31T09:30:12Z",
    "state": "playing",
    "kind": "track",
    "title": "The Wheel",
    "artist": "SOHN",
    "trackId": "spotifytrack00000001",
    "progressMs": 161000,
    "durationMs": 255000,
    "deviceName": "Kitchen speaker"
  }
}
```

Three gates, answered separately because they are routinely conflated:

- `enabled` is whether this instance polls at all. `ENCORE_NOWPLAYING_INTERVAL` unset means `false`,
  and a client should render no card rather than an empty one.
- `scopeGranted` is whether *this* account's grant carries `user-read-playback-state`. Computed
  server-side against the stored grant, like `/api/me`'s `missingScopes` and for the same reason.
- `observation` being `null` is **"Encore has not managed to look"**. An `observation` whose `state`
  is `"idle"` is **"nothing is playing"**. These are different facts and a client must not render
  them with the same sentence.

`failed` reports that the check at `checkedAt` did not succeed. `observation`, if present, is then
the last one that did, so a client can say how stale the display is rather than discarding a true
thing because a later request went wrong.

`kind` decides what can truthfully be said about the item:

| `kind` | Meaning |
|---|---|
| `none` | Nothing in the player. Only ever paired with `state: "idle"`. |
| `track` | A Spotify catalogue track. The only kind that ever becomes a listen. |
| `episode` | A podcast episode. `artist` holds the show. Never enters a listening history. |
| `local` | A file on the listener's own machine. Named, never linked, never a listen. |
| `unknown` | An advert, or a type Encore does not know. Carries no title and no artist by design. |

`trackId` is present only for a track **Encore's own catalogue already holds**, so a client can link
to `/tracks/{id}` without checking. A track Spotify has just started playing is named but not linked
until enrichment catches up.

`progressMs` is progress at `observedAt` and is never extrapolated. A client should state the
observation's age beside it rather than animating a bar from a fact up to one interval old.

`deviceName` is empty when Spotify reported no device — `GET /v1/me/player/currently-playing` is
documented with the same object as `GET /v1/me/player` but is observed to omit it — and a client
should then render no device clause rather than an unknown one.

**Never on a share link.** `GET /api/share/{token}` carries none of this. Real-time presence is
exactly the concern the share design was written around: a share exposes what somebody listens to,
never when they are awake.

**The poller never writes a listen.** `GET /me/player/recently-played` remains the only path that
creates one; the now-playing poll is a read-only observer. See
[configuration.md](configuration.md#now-playing) for what it costs and how to turn it on.
```

Also add the row to the endpoint table under "Session and account" if that is where a reader would look for it — check the file's own organisation and follow it.

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
git add internal/httpapi/nowplaying.go internal/httpapi/nowplaying_test.go \
        internal/httpapi/dto.go internal/httpapi/router.go internal/httpapi/server.go \
        internal/httpapi/httpapi_test.go docs/api.md
git commit -m "$(cat <<'MSG'
Httpapi: GET /api/nowplaying, which never calls Spotify

The card polls this from every open tab. A handler that fetched on read would
multiply the feature's cost by however many tabs somebody happens to have open,
and would put that traffic on whichever budget the handler used — the exact
coupling this phase exists to avoid. It reads the stored row and nothing else.

Three gates, answered separately because they are routinely conflated: whether
the instance polls at all, whether this account granted the scope, and whether
anything has ever been observed. The third is a null observation rather than a
state value, so a client cannot render "nothing is playing" for an account
nobody has looked at — there is no state to misread.

trackId is present only for a track the catalogue already holds. Spotify names a
track the instant it starts; Encore learns about it when enrichment gets round to
it, and a link in between is a dead link wearing a working one's clothes.

Nothing here reaches a share link, and a test asserts that against the response
bytes rather than a struct field, because the defect it guards is somebody adding
the field.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
MSG
)"
```

---

## Task 6: The card, and every word of it

**Files:**
- Modify: `web/src/lib/types.ts` (the DTO mirror)
- Modify: `web/src/lib/format.ts` (`intervalPhrase`)
- Modify: `web/src/lib/query.ts` (`qk.nowPlaying`)
- Modify: `web/src/pages/Dashboard.tsx` (the card, and `nowPlayingPollInterval`)
- Modify: `web/src/pages/Settings.tsx` (what the instance is configured to do)
- Create: `web/src/test/nowplaying.test.tsx`

**Interfaces:**
- Consumes: `NowPlayingResponse` / `NowPlayingObservation` from Task 5's `dto.go`; `api.get`, `qk`, `Panel`, `EmptyState`, `ErrorState`, `Skeleton`, `Chip`, `Icon`, `formatClock`, `formatRelative` (all existing).
- Produces:
  - `web/src/lib/types.ts`: `NowPlaying`, `NowPlayingObservation`, `PlaybackState`, `PlaybackItemKind`
  - `web/src/lib/format.ts`: `export function intervalPhrase(seconds: number): string`
  - `web/src/lib/query.ts`: `qk.nowPlaying()`
  - `web/src/pages/Dashboard.tsx`: `export function nowPlayingPollInterval(data: NowPlaying | undefined): number | false`

### Where the card goes, and where it does not

The card is the **topmost** panel of the populated dashboard body, immediately after `</StatGrid>` and before the `Listening over time` `ChartCard` (`Dashboard.tsx:473-475`). A live fact belongs above historical ones.

**When the poller is off, the card is not rendered at all.** The house formula for "turned off" (`AlbumDetail.tsx:504-514`, `ArtistDetail.tsx:562-569`) renders the explanation in place, and that is right on a page somebody navigated to. The dashboard is the home screen, and a panel that says "turned off" on every load for ever is a nag about a decision the listener cannot change — the key is the operator's. So the explanation moves to **Settings**, which is already where instance configuration is explained, and the dashboard simply has one fewer card.

### The copy. This table is the deliverable.

Panel heading, every rendered state: **`Now playing`**

Panel description, constant in every rendered state so it can never contradict the body:
**`What Spotify says you are playing. Nothing here is added to your listening history.`**

| # | State | Condition | Exact copy |
|---|---|---|---|
| 1 | Off (dashboard) | `enabled === false` | *No card is rendered.* |
| 2 | Off (Settings) | `enabled === false` | Title `Now playing is turned off`<br>Body `This instance does not ask Spotify what you are playing right now, so the dashboard shows no now-playing card. Every other figure in Encore comes from your own listening history and is unaffected. An administrator can turn this on with ENCORE_NOWPLAYING_INTERVAL.` |
| 3 | On (Settings) | `enabled === true` | Title `Now playing is on`<br>Body `Encore asks Spotify what you are playing every 30 seconds. It records nothing from those checks — your listening history still comes only from the recently-played feed.` — where `30 seconds` is `intervalPhrase(intervalSeconds)` |
| 4 | Scope missing | `enabled && !scopeGranted` | `Encore cannot see what you are playing.`<br>`Your Spotify connection does not include permission to read your playback state. Reconnecting grants it, and nothing else in Encore is affected.`<br>link `Reconnect Spotify` → `/api/auth/spotify/relink` |
| 5 | Loading | first fetch in flight, no data | Skeleton, with the screen-reader label `Loading what you are playing` |
| 6 | The request failed | `query.isError` | `<ErrorState title="Now playing could not be loaded" onRetry={…} />` |
| 7 | Never looked | `observation === null && !failed` | `Encore has not checked yet.`<br>`It checks every 30 seconds.` |
| 8 | Never looked, last check failed | `observation === null && failed` | `The last check failed 3 minutes ago.`<br>`Encore has not managed to see what you are playing yet.` |
| 9 | Nothing playing | `observation.state === 'idle' && !failed` | `Nothing is playing.`<br>`Last checked just now.` |
| 10 | Stale, nothing was playing | `failed && observation.state === 'idle'` | `The last check failed 3 minutes ago.`<br>`Nothing was playing 4 minutes ago.` |
| 11 | Playing a track | `state==='playing' && kind==='track' && !failed` | Chip `Playing` (tone `lamp`)<br>Title `The Wheel` — a link to `/tracks/{trackId}` only when `trackId` is non-empty<br>`SOHN`<br>`on Kitchen speaker` (only when `deviceName` is non-empty)<br>`2:41 of 4:15` and a meter (only when both `progressMs` and `durationMs` are present)<br>`Last checked just now.` |
| 12 | Paused a track | `state==='paused' && kind==='track' && !failed` | Chip `Paused` (tone `neutral`); otherwise exactly as 11 |
| 13 | Podcast | `kind==='episode' && !failed` | Chip `Playing`/`Paused`<br>Title = episode name (never a link)<br>Show name (only when non-empty)<br>`Podcasts are not part of your listening history.`<br>device, progress and age as above |
| 14 | Local file | `kind==='local' && !failed` | Chip `Playing`/`Paused`<br>Title = file name (never a link)<br>Artist (only when non-empty)<br>`Local files are not part of your listening history.` |
| 15 | Unidentifiable | `kind==='unknown' && !failed` | Chip `Playing`/`Paused`<br>`Spotify is playing something Encore cannot identify.`<br>`It will not appear in your listening history.`<br>*no title, no artist* |
| 16 | Stale, something was playing | `failed && observation.kind !== 'none'` | `The last check failed 3 minutes ago.`<br>`This is what you were playing 4 minutes ago.`<br>then the title, the artist and the kind note — **no chip, no progress figure, no meter, no "Last checked" line** |

Six rules that hold across the table, each of which is a defect avoided:

1. **No chip in a stale state (16).** A chip reading `Playing` above a four-minute-old observation is a present-tense claim about something nobody has confirmed. The two sentences carry the state instead.
2. **No progress figure in a stale state (16).** Progress from four minutes ago is not wrong so much as meaningless, and a still bar beside a fresh-looking figure reads as a bug.
3. **The second line is whatever `artist` holds, rendered only when non-empty. There is no fallback string.** A kind-dependent fallback — `Unknown artist` for a track, something else for a podcast — is three more strings and three more ways to be wrong. An absent line says the same thing and cannot be wrong.
4. **`unknown` never shows a name.** Spotify's own label for an advert is not a title. The payload carries no title for this kind (Task 4 scrubs it), and the card would not render one anyway.
5. **The category notes are plurals with no count** — `Podcasts are…`, `Local files are…` — so there is no singular form to get wrong. That is deliberate: they describe a category, not this item.
6. **Progress is never extrapolated.** The figure is as observed; the age line above says how old it is; the meter's accessible label says the same thing where it belongs.

Two helpers carry every singular/plural form in the feature:

- `intervalPhrase(seconds)` — `second` / `N seconds` / `minute` / `N minutes` / `hour` / `N hours`. The **singular** forms are the trap: `every 1 minutes` is exactly the class of defect this project has shipped before.
- `formatRelative(timestamp)` — already in `format.ts`, already tested, already returns `just now` under 45 seconds and `3 minutes ago` above it. Reused rather than reimplemented.

- [ ] **Step 1: Write the failing pure-function tests**

Append to `web/src/lib/format.test.ts`:

```ts
import { intervalPhrase } from './format'

describe('intervalPhrase', () => {
  // Every singular and plural form the now-playing copy can produce, asserted
  // in full. "It checks every 1 minutes." is the defect this exists to stop,
  // and it is invisible to a type checker and to every other test.
  //
  // Fails when: the singular branches are removed (60 then reads "1 minutes");
  // the minute branch stops requiring an exact multiple (90 then reads
  // "1.5 minutes"); or the hour branch is dropped (3600 reads "60 minutes",
  // which is true and reads like a bug).
  it.each([
    [1, 'second'],
    [15, '15 seconds'],
    [30, '30 seconds'],
    [59, '59 seconds'],
    [60, 'minute'],
    [90, '90 seconds'],
    [120, '2 minutes'],
    [300, '5 minutes'],
    [3600, 'hour'],
    [5400, '90 minutes'],
    [7200, '2 hours'],
  ])('renders %d seconds as "%s"', (seconds, want) => {
    expect(intervalPhrase(seconds)).toBe(want)
  })

  // Zero is the poller being off, and the card that would have used this is not
  // rendered at all. An empty string rather than "0 seconds" so that a stray
  // render cannot produce "every 0 seconds", which reads as a broken instance.
  //
  // Fails when: the guard is removed and the seconds branch runs on 0.
  it('renders nothing for a poller that is off', () => {
    expect(intervalPhrase(0)).toBe('')
    expect(intervalPhrase(-30)).toBe('')
  })
})
```

- [ ] **Step 2: Write `intervalPhrase`**

Append to `web/src/lib/format.ts`:

```ts
/**
 * The period that follows the word "every": `30 seconds`, `minute`, `2 hours`.
 *
 * It returns the bare unit rather than `1 minute` for a single unit, because
 * every call site reads "every " + this, and "every 1 minute" is the wrong
 * register while "every a minute" is not English at all. The singular forms are
 * the whole reason this is a function: `every 1 minutes` is invisible to a type
 * checker and to every test that does not assert the sentence in full.
 *
 * Zero or less returns an empty string. That is the poller being off, and the
 * card that would render this is not shown at all — but a stray render must not
 * be able to produce "every 0 seconds", which reads as a broken instance.
 */
export function intervalPhrase(seconds: number): string {
  if (!Number.isFinite(seconds) || seconds <= 0) return ''
  const whole = Math.round(seconds)
  if (whole % 3600 === 0) {
    const hours = whole / 3600
    return hours === 1 ? 'hour' : `${hours} hours`
  }
  if (whole % 60 === 0) {
    const minutes = whole / 60
    return minutes === 1 ? 'minute' : `${minutes} minutes`
  }
  return whole === 1 ? 'second' : `${whole} seconds`
}
```

- [ ] **Step 3: Mirror the DTO and add the query key**

In `web/src/lib/types.ts`, in the section that holds `StatusResponse`:

```ts
/** `unknown` never crosses the wire: it is the absence of an observation. */
export type PlaybackState = 'idle' | 'playing' | 'paused'

export type PlaybackItemKind = 'none' | 'track' | 'episode' | 'local' | 'unknown'

export interface NowPlayingObservation {
  observedAt: Timestamp
  state: PlaybackState
  kind: PlaybackItemKind
  title: string
  artist: string
  /**
   * A track in Encore's own catalogue, so `/tracks/{id}` will resolve. Empty
   * when the item is not a track, or is a track Encore has never seen — the
   * name is still shown, only the link is withheld.
   */
  trackId: string
  /** Progress at `observedAt`. Never extrapolate it: say how old it is instead. */
  progressMs: number | null
  durationMs: number | null
  /** Empty when Spotify reported no device. Render no clause rather than an unknown one. */
  deviceName: string
}

export interface NowPlaying {
  /** Whether this instance polls at all. False means render no card. */
  enabled: boolean
  /** How often the poller checks, and so how often to ask again. Zero when off. */
  intervalSeconds: number
  /** Whether this account granted `user-read-playback-state`. */
  scopeGranted: boolean
  /** When the poller last tried, successfully or not. Null when it never has. */
  checkedAt: Timestamp | null
  /** The attempt at `checkedAt` failed. `observation` is then the last one that did not. */
  failed: boolean
  /**
   * The last successful observation, or null.
   *
   * Null means **Encore has not managed to look**. An observation whose `state`
   * is `idle` means **nothing is playing**. These are different facts and must
   * never share a sentence.
   */
  observation: NowPlayingObservation | null
}
```

In `web/src/lib/query.ts`, in `qk`:

```ts
  /**
   * Deliberately its own top-level namespace rather than under `stats`: an
   * import invalidates `qk.stats()` wholesale, and a live card being churned by
   * an unrelated import would refetch on a schedule nobody chose.
   */
  nowPlaying: () => ['nowplaying'] as const,
```

- [ ] **Step 4: Write the failing poll-interval test**

Append to `web/src/test/nowplaying.test.tsx` (created in full in Step 6; this block goes in it):

```tsx
describe('nowPlayingPollInterval', () => {
  // Polling has to stop, and the two reasons it must are the two states whose
  // answer can never change on its own: an instance that does not poll, and an
  // account that has not granted the scope. Asserting the *number* would pass
  // for a card that polls a disabled instance for ever.
  //
  // Fails when: either guard is dropped — the corresponding case then returns a
  // number instead of false.
  it('stops for an instance that does not poll and an account that cannot be polled', () => {
    expect(nowPlayingPollInterval(payload({ enabled: false, intervalSeconds: 0 }))).toBe(false)
    expect(nowPlayingPollInterval(payload({ scopeGranted: false }))).toBe(false)
  })

  // Fails when: the floor is removed — an instance misconfigured to one second
  // would then have every open tab asking once a second.
  it('polls at the instance interval, never faster than five seconds', () => {
    expect(nowPlayingPollInterval(payload({ intervalSeconds: 30 }))).toBe(30_000)
    expect(nowPlayingPollInterval(payload({ intervalSeconds: 300 }))).toBe(300_000)
    expect(nowPlayingPollInterval(payload({ intervalSeconds: 1 }))).toBe(5_000)
  })

  // Fails when: the undefined guard is dropped and the function reads
  // .enabled off undefined, which throws inside TanStack Query's scheduler.
  it('does not schedule anything before the first answer', () => {
    expect(nowPlayingPollInterval(undefined)).toBe(false)
  })
})
```

- [ ] **Step 5: Write the poll interval**

In `web/src/pages/Dashboard.tsx`, beside the other module constants:

```ts
/**
 * The floor on how often the card asks, whatever the instance is configured for.
 *
 * The server's interval is what the *poller* runs at, and asking faster than it
 * polls can only return the same answer again. Five seconds is not a rate this
 * ever reaches in practice; it exists so a misconfigured instance cannot have
 * every open tab asking once a second.
 */
const NOW_PLAYING_MIN_POLL_MS = 5_000

/**
 * The next poll delay for the now-playing card, or `false` to stop.
 *
 * Exported so it can be tested without driving a real timer through TanStack
 * Query — the same shape as the album page's `tracklistPollInterval`.
 *
 * It stops for the two states whose answer cannot change on its own: an
 * instance that does not poll at all, and an account that has not granted
 * `user-read-playback-state`. Polling either is asking a question that has
 * already been answered for good.
 */
export function nowPlayingPollInterval(data: NowPlaying | undefined): number | false {
  if (!data) return false
  if (!data.enabled || !data.scopeGranted) return false
  return Math.max(data.intervalSeconds * 1000, NOW_PLAYING_MIN_POLL_MS)
}
```

Note that `refetchIntervalInBackground` is left at its default of `false`, so a hidden tab stops asking entirely. That is correct for a presence display and needs no code.

- [ ] **Step 6: Add the query and the card**

In `Dashboard()`, beside the other `useQuery` calls:

```tsx
  const nowPlaying = useQuery({
    queryKey: qk.nowPlaying(),
    queryFn: ({ signal }) => api.get<NowPlaying>('/nowplaying', undefined, signal),
    refetchInterval: (query) => nowPlayingPollInterval(query.state.data),
  })
```

And in the body, immediately after `</StatGrid>`:

```tsx
      {nowPlaying.data?.enabled === true && (
        <NowPlayingCard query={nowPlaying} />
      )}
```

The card itself, as a page-local component beside `RecentStrip` — the same place `AlbumDetail.tsx` keeps `MissingTracks`:

```tsx
/**
 * What the listener is playing right now, or the reason Encore cannot say.
 *
 * Every branch below is a different fact, and the two that are easiest to
 * conflate are kept furthest apart: a null observation is "Encore has not
 * managed to look", and an observation whose state is `idle` is "nothing is
 * playing". They share no sentence and no code path.
 *
 * Nothing here is ever extrapolated. The progress figure is as observed and the
 * line above it says how old that is, because a bar animating from a fact up to
 * a whole interval old is a moving lie in place of a still truth.
 */
function NowPlayingCard({
  query,
}: {
  query: UseQueryResult<NowPlaying>
}): ReactElement {
  const data = query.data
  return (
    <Panel
      title="Now playing"
      description="What Spotify says you are playing. Nothing here is added to your listening history."
    >
      {query.isPending && !data ? (
        <div role="status" aria-live="polite" aria-busy="true">
          <span className="sr-only">Loading what you are playing</span>
          <SkeletonText lines={2} className="max-w-sm" />
        </div>
      ) : query.isError ? (
        <ErrorState
          error={query.error}
          title="Now playing could not be loaded"
          onRetry={() => {
            void query.refetch()
          }}
        />
      ) : !data ? null : !data.scopeGranted ? (
        <div>
          <p className="text-sm text-ink">Encore cannot see what you are playing.</p>
          <p className="mt-1 max-w-prose text-sm text-ink-muted">
            Your Spotify connection does not include permission to read your playback state.
            Reconnecting grants it, and nothing else in Encore is affected.
          </p>
          <a href="/api/auth/spotify/relink" className="btn btn-primary mt-3 text-sm">
            <Icon name="refresh" />
            Reconnect Spotify
          </a>
        </div>
      ) : (
        <NowPlayingBody data={data} />
      )}
    </Panel>
  )
}
```

```tsx
/** The four families of answer, once the instance polls and the account can be polled. */
function NowPlayingBody({ data }: { data: NowPlaying }): ReactElement {
  const { observation, failed, checkedAt } = data

  // Never looked. Deliberately the first branch and deliberately worded without
  // the word "nothing": this is the absence of a look, not a silent player.
  if (!observation) {
    return failed ? (
      <div>
        <p className="text-sm text-ink">
          The last check failed {checkedAt ? formatRelative(checkedAt) : EMPTY}.
        </p>
        <p className="mt-1 text-sm text-ink-muted">
          Encore has not managed to see what you are playing yet.
        </p>
      </div>
    ) : (
      <div>
        <p className="text-sm text-ink">Encore has not checked yet.</p>
        <p className="mt-1 text-sm text-ink-muted">
          It checks every {intervalPhrase(data.intervalSeconds)}.
        </p>
      </div>
    )
  }

  // A failed check on top of an observation: say so, say how stale, and drop
  // every present-tense signal. A chip reading "Playing" above a four-minute-old
  // observation claims something nobody confirmed, and a progress figure from
  // four minutes ago is meaningless beside it.
  if (failed) {
    return (
      <div>
        <p className="text-sm text-ink">
          The last check failed {checkedAt ? formatRelative(checkedAt) : EMPTY}.
        </p>
        <p className="mt-1 text-sm text-ink-muted">
          {observation.kind === 'none'
            ? `Nothing was playing ${formatRelative(observation.observedAt)}.`
            : `This is what you were playing ${formatRelative(observation.observedAt)}.`}
        </p>
        {observation.kind !== 'none' && (
          <div className="mt-3">
            <NowPlayingItem observation={observation} withProgress={false} />
          </div>
        )}
      </div>
    )
  }

  // Nothing is playing. A whole check succeeded to establish this, which is why
  // it is never the same sentence as the branch above.
  if (observation.kind === 'none') {
    return (
      <div>
        <p className="text-sm text-ink">Nothing is playing.</p>
        <p className="mt-1 text-sm text-ink-muted">
          Last checked {checkedAt ? formatRelative(checkedAt) : EMPTY}.
        </p>
      </div>
    )
  }

  return (
    <div>
      <Chip tone={observation.state === 'playing' ? 'lamp' : 'neutral'}>
        {observation.state === 'playing' ? 'Playing' : 'Paused'}
      </Chip>
      <div className="mt-2">
        <NowPlayingItem observation={observation} withProgress />
      </div>
      <p className="mt-2 text-sm text-ink-muted">
        Last checked {checkedAt ? formatRelative(checkedAt) : EMPTY}.
      </p>
    </div>
  )
}
```

```tsx
/**
 * What each kind of item can truthfully be called.
 *
 * A category sentence rather than a count, so there is no singular form to get
 * wrong: it describes podcasts and local files in general, not this one.
 * `unknown` carries no name at all — Spotify's own label for an advert is not a
 * title, and putting it where a listener expects their music would attribute
 * their evening to an advertiser.
 */
const KIND_NOTE: Record<PlaybackItemKind, string> = {
  none: '',
  track: '',
  episode: 'Podcasts are not part of your listening history.',
  local: 'Local files are not part of your listening history.',
  unknown: 'It will not appear in your listening history.',
}

function NowPlayingItem({
  observation,
  withProgress,
}: {
  observation: NowPlayingObservation
  withProgress: boolean
}): ReactElement {
  const { kind, title, artist, trackId, progressMs, durationMs, deviceName } = observation
  const showMeter = withProgress && progressMs !== null && durationMs !== null && durationMs > 0
  const share = showMeter ? Math.min(progressMs / durationMs, 1) : 0

  return (
    <div>
      {kind === 'unknown' ? (
        <p className="text-sm text-ink">Spotify is playing something Encore cannot identify.</p>
      ) : trackId ? (
        <p className="truncate text-sm font-medium text-ink">
          <Link to={`/tracks/${encodeURIComponent(trackId)}`} className="hover:text-lamp">
            {title}
          </Link>
        </p>
      ) : (
        <p className="truncate text-sm font-medium text-ink">{title}</p>
      )}

      {/* Whatever the server sent, and no fallback. A kind-dependent
          "Unknown artist" would be three more strings and three more ways to be
          wrong; an absent line says the same thing and cannot be. */}
      {artist !== '' && <p className="mt-0.5 truncate text-xs text-ink-muted">{artist}</p>}

      {KIND_NOTE[kind] !== '' && (
        <p className="mt-1 text-xs text-ink-faint">{KIND_NOTE[kind]}</p>
      )}

      {deviceName !== '' && (
        <p className="mt-1 text-xs text-ink-faint">on {deviceName}</p>
      )}

      {showMeter && (
        <>
          <p className="mt-2 text-xs text-ink-faint">
            {formatClock(progressMs)} of {formatClock(durationMs)}
          </p>
          <div
            className="meter mt-1"
            role="meter"
            aria-valuenow={Math.round(share * 100)}
            aria-valuemin={0}
            aria-valuemax={100}
            aria-label="Progress when Encore last checked"
          >
            <span style={{ width: `${share * 100}%` }} />
          </div>
        </>
      )}
    </div>
  )
}
```

Add the imports `Chip`, `SkeletonText`, `Icon`, `intervalPhrase`, `formatClock`, `EMPTY`, `NowPlaying`, `NowPlayingObservation`, `PlaybackItemKind` and `UseQueryResult`; `Link`, `formatRelative`, `Panel`, `ErrorState` and `qk` are already imported by this file.

- [ ] **Step 7: Say what the instance is configured to do, on Settings**

In `web/src/pages/Settings.tsx`, add a `useQuery` on `qk.nowPlaying()` with **no** `refetchInterval` — the configuration cannot change while the page is open — and a panel immediately after the metadata panel (find it from the `/api/status` query at `Settings.tsx:368`):

```tsx
      <Panel title="Now playing" description="What this instance asks Spotify about your player.">
        {nowPlaying.data?.enabled === false ? (
          <EmptyState
            title="Now playing is turned off"
            description="This instance does not ask Spotify what you are playing right now, so the dashboard shows no now-playing card. Every other figure in Encore comes from your own listening history and is unaffected. An administrator can turn this on with ENCORE_NOWPLAYING_INTERVAL."
          />
        ) : nowPlaying.data?.enabled === true ? (
          <div>
            <p className="text-sm text-ink">Now playing is on</p>
            <p className="mt-1 max-w-prose text-sm text-ink-muted">
              Encore asks Spotify what you are playing every{' '}
              {intervalPhrase(nowPlaying.data.intervalSeconds)}. It records nothing from those
              checks — your listening history still comes only from the recently-played feed.
            </p>
          </div>
        ) : (
          <div role="status" aria-live="polite" aria-busy="true">
            <span className="sr-only">Loading the now-playing setting</span>
            <SkeletonText lines={2} className="max-w-md" />
          </div>
        )}
      </Panel>
```

The two branches test `=== false` and `=== true` rather than truthiness, so the loading frame renders the skeleton rather than briefly claiming the feature is off — the same request-in-flight rule `AlbumDetail.tsx:540-556` states.

- [ ] **Step 8: Write the copy tests**

Create `web/src/test/nowplaying.test.tsx`. It follows `album-tracklist.test.tsx` exactly: `stubRoutes` for a path→body map, `mountAt('/')` for the dashboard, and `heading.closest('section')` to scope assertions to one card.

```tsx
import type { ReactElement } from 'react'
import { QueryClientProvider } from '@tanstack/react-query'
import { RouterProvider, createMemoryRouter } from 'react-router-dom'
import { act, render, screen, within } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { routes } from '../App'
import { createQueryClient } from '../lib/query'
import type { NowPlaying, NowPlayingObservation } from '../lib/types'
import { nowPlayingPollInterval } from '../pages/Dashboard'

/** Answers each path with its own body, and returns the log of paths asked for. */
function stubRoutes(bodies: Record<string, unknown>): string[] {
  const asked: string[] = []
  vi.stubGlobal(
    'fetch',
    vi.fn(async (input: RequestInfo | URL) => {
      const url = typeof input === 'string' ? input : input.toString()
      const path = new URL(url, 'http://encore.test').pathname
      asked.push(path)
      const body = bodies[path]
      if (body === undefined) {
        return new Response(JSON.stringify({ error: { code: 'not_found', message: 'No.' } }), {
          status: 404,
          headers: { 'content-type': 'application/json' },
        })
      }
      return new Response(JSON.stringify(body), {
        status: 200,
        headers: { 'content-type': 'application/json' },
      })
    }),
  )
  return asked
}

function mountAt(path: string): ReactElement {
  const router = createMemoryRouter(routes, { initialEntries: [path] })
  return (
    <QueryClientProvider client={createQueryClient()}>
      <RouterProvider router={router} />
    </QueryClientProvider>
  )
}

function payload(overrides: Partial<NowPlaying> = {}): NowPlaying {
  return {
    enabled: true,
    intervalSeconds: 30,
    scopeGranted: true,
    checkedAt: '2026-07-31T09:30:12Z',
    failed: false,
    observation: null,
    ...overrides,
  }
}

function observation(overrides: Partial<NowPlayingObservation> = {}): NowPlayingObservation {
  return {
    observedAt: '2026-07-31T09:30:12Z',
    state: 'playing',
    kind: 'track',
    title: 'The Wheel',
    artist: 'SOHN',
    trackId: 'spotifytrack00000001',
    progressMs: 161_000,
    durationMs: 255_000,
    deviceName: 'Kitchen speaker',
    ...overrides,
  }
}

/**
 * Renders the dashboard with a now-playing payload and returns the card.
 *
 * ME, DASHBOARD_BODIES and the shape of a populated dashboard are whatever this
 * suite already uses — copy them from an existing dashboard-rendering test
 * rather than inventing a second set. The card only renders in the populated
 * body, so the summary must report listens.
 */
async function card(np: NowPlaying): Promise<HTMLElement> {
  stubRoutes({ ...DASHBOARD_BODIES, '/api/nowplaying': np })
  render(mountAt('/'))
  const heading = await screen.findByRole('heading', { name: 'Now playing' })
  const section = heading.closest('section')
  if (!section) throw new Error('the heading is not inside a panel')
  return section
}

beforeEach(() => {
  vi.unstubAllGlobals()
})

afterEach(() => {
  vi.useRealTimers()
})

describe('the now-playing card', () => {
  // The panel's description is constant across every body below, so it can
  // never contradict one. It also states the read-only promise where somebody
  // reading the card will see it.
  //
  // Fails when: the description is made conditional, or the promise is dropped.
  it('states what it is, and that nothing here becomes history', async () => {
    const section = await card(payload({ observation: observation() }))
    expect(
      within(section).getByText(
        'What Spotify says you are playing. Nothing here is added to your listening history.',
      ),
    ).toBeInTheDocument()
  })

  // The dashboard is the home screen; a panel saying "turned off" on every load
  // for ever is a nag about a decision the listener cannot change. Settings says
  // it instead.
  //
  // Fails when: the `enabled === true` guard around the card is removed.
  it('is not rendered at all when the instance does not poll', async () => {
    stubRoutes({
      ...DASHBOARD_BODIES,
      '/api/nowplaying': payload({ enabled: false, intervalSeconds: 0 }),
    })
    render(mountAt('/'))
    await screen.findByRole('heading', { level: 1, name: 'Dashboard' })
    expect(screen.queryByRole('heading', { name: 'Now playing' })).not.toBeInTheDocument()
  })

  // Fails when: the scope branch is dropped, or is worded as a failure — a grant
  // that never included the scope is not a check that went wrong, and offering a
  // retry for it points somebody at a button that cannot work.
  it('says the connection lacks the permission, and offers the one thing that fixes it', async () => {
    const section = await card(payload({ scopeGranted: false }))
    expect(within(section).getByText('Encore cannot see what you are playing.')).toBeInTheDocument()
    expect(
      within(section).getByText(
        'Your Spotify connection does not include permission to read your playback state. Reconnecting grants it, and nothing else in Encore is affected.',
      ),
    ).toBeInTheDocument()
    expect(within(section).getByRole('link', { name: /Reconnect Spotify/ })).toHaveAttribute(
      'href',
      '/api/auth/spotify/relink',
    )
    // Not a failure, and not something to retry.
    expect(within(section).queryByText(/failed/i)).not.toBeInTheDocument()
    expect(within(section).queryByText(/nothing is playing/i)).not.toBeInTheDocument()
  })

  // The distinction the whole feature turns on. Asserted from both sides, in the
  // same test, so the two sentences cannot drift into each other.
  //
  // Fails when: a null observation is rendered with the idle copy, or the idle
  // observation is rendered with the never-checked copy — either substitution
  // trips one of the four assertions below.
  it('never says nothing is playing when it simply has not looked', async () => {
    const never = await card(payload({ observation: null, checkedAt: null }))
    expect(within(never).getByText('Encore has not checked yet.')).toBeInTheDocument()
    expect(within(never).getByText('It checks every 30 seconds.')).toBeInTheDocument()
    expect(within(never).queryByText(/nothing is playing/i)).not.toBeInTheDocument()
  })

  // Fails when: the idle branch reuses the never-checked wording, or drops the
  // age line — a display that cannot say when it last looked cannot be trusted
  // to say a player is silent.
  it('says nothing is playing, and when it last looked', async () => {
    const section = await card(
      payload({ observation: observation({ state: 'idle', kind: 'none' }) }),
    )
    expect(within(section).getByText('Nothing is playing.')).toBeInTheDocument()
    expect(within(section).getByText(/^Last checked /)).toBeInTheDocument()
    expect(within(section).queryByText(/has not checked/i)).not.toBeInTheDocument()
  })

  // Fails when: the failure branch is merged with the never-checked one — the
  // second sentence then claims Encore has not looked, when it looked and
  // failed, which are different things to tell somebody.
  it('says a first check failed without claiming nothing is playing', async () => {
    const section = await card(payload({ observation: null, failed: true }))
    expect(within(section).getByText(/^The last check failed /)).toBeInTheDocument()
    expect(
      within(section).getByText('Encore has not managed to see what you are playing yet.'),
    ).toBeInTheDocument()
    expect(within(section).queryByText(/nothing is playing/i)).not.toBeInTheDocument()
  })

  // Fails when: the stale branch keeps the chip — "Playing" above a four-minute
  // old observation is a present-tense claim nobody confirmed; or keeps the
  // progress figure, which is meaningless at that age and reads as a live one.
  it('says how stale a failed check has left the display, with no present tense', async () => {
    const section = await card(
      payload({
        failed: true,
        checkedAt: '2026-07-31T09:34:00Z',
        observation: observation({ observedAt: '2026-07-31T09:30:00Z' }),
      }),
    )
    expect(within(section).getByText(/^The last check failed /)).toBeInTheDocument()
    expect(within(section).getByText(/^This is what you were playing /)).toBeInTheDocument()
    expect(within(section).getByText('The Wheel')).toBeInTheDocument()
    expect(within(section).queryByText('Playing')).not.toBeInTheDocument()
    expect(within(section).queryByText('Paused')).not.toBeInTheDocument()
    expect(within(section).queryByText(/of 4:15/)).not.toBeInTheDocument()
    expect(within(section).queryByRole('meter')).not.toBeInTheDocument()
    expect(within(section).queryByText(/^Last checked /)).not.toBeInTheDocument()
  })

  // Fails when: the stale-idle case falls through to "This is what you were
  // playing", which would name a track for a player that was silent.
  it('says nothing was playing, when that is what the stale observation holds', async () => {
    const section = await card(
      payload({
        failed: true,
        observation: observation({ state: 'idle', kind: 'none', title: '', artist: '' }),
      }),
    )
    expect(within(section).getByText(/^Nothing was playing /)).toBeInTheDocument()
    expect(within(section).queryByText(/what you were playing/i)).not.toBeInTheDocument()
  })

  // Fails when: the chip stops varying with state, or the progress figure starts
  // using formatDuration ("2m 41s") instead of formatClock ("2:41") — the second
  // is the form people recognise from a player.
  it('shows a playing track with its device and its progress as observed', async () => {
    const section = await card(payload({ observation: observation() }))
    expect(within(section).getByText('Playing')).toBeInTheDocument()
    expect(within(section).getByRole('link', { name: 'The Wheel' })).toHaveAttribute(
      'href',
      '/tracks/spotifytrack00000001',
    )
    expect(within(section).getByText('SOHN')).toBeInTheDocument()
    expect(within(section).getByText('on Kitchen speaker')).toBeInTheDocument()
    expect(within(section).getByText('2:41 of 4:15')).toBeInTheDocument()
    expect(within(section).getByRole('meter')).toHaveAttribute(
      'aria-label',
      'Progress when Encore last checked',
    )
  })

  // Fails when: the chip is hard-coded to "Playing".
  it('says Paused when it is paused', async () => {
    const section = await card(payload({ observation: observation({ state: 'paused' }) }))
    expect(within(section).getByText('Paused')).toBeInTheDocument()
    expect(within(section).queryByText('Playing')).not.toBeInTheDocument()
  })

  // Fails when: the trackId guard is dropped and the title is always a link —
  // the assertion below then finds a link to a page that does not exist.
  it('names a track the catalogue has never seen, and does not link it', async () => {
    const section = await card(payload({ observation: observation({ trackId: '' }) }))
    expect(within(section).getByText('The Wheel')).toBeInTheDocument()
    expect(within(section).queryByRole('link', { name: 'The Wheel' })).not.toBeInTheDocument()
  })

  // Fails when: the device clause renders unconditionally — an empty deviceName
  // then produces a bare "on " with nothing after it.
  it('renders no device clause when Spotify reported no device', async () => {
    const section = await card(payload({ observation: observation({ deviceName: '' }) }))
    expect(within(section).queryByText(/^on /)).not.toBeInTheDocument()
  })

  // Fails when: the progress block renders with one of the two figures missing
  // — "2:41 of —" says nothing, and a meter with no denominator cannot be drawn.
  it('renders no progress at all when there is no duration to measure it against', async () => {
    const section = await card(
      payload({ observation: observation({ durationMs: null, progressMs: 1000 }) }),
    )
    expect(within(section).queryByRole('meter')).not.toBeInTheDocument()
    expect(within(section).queryByText(/ of /)).not.toBeInTheDocument()
  })

  // Fails when: the kind note is dropped, or a podcast is rendered with the
  // track branch — a listener would then reasonably expect it in their history.
  it('names a podcast and says it will never be in the history', async () => {
    const section = await card(
      payload({
        observation: observation({
          kind: 'episode',
          title: 'The one about ducks',
          artist: 'Ducks Weekly',
          trackId: '',
        }),
      }),
    )
    expect(within(section).getByText('The one about ducks')).toBeInTheDocument()
    expect(within(section).getByText('Ducks Weekly')).toBeInTheDocument()
    expect(
      within(section).getByText('Podcasts are not part of your listening history.'),
    ).toBeInTheDocument()
    expect(
      within(section).queryByRole('link', { name: 'The one about ducks' }),
    ).not.toBeInTheDocument()
  })

  // Fails when: local files share the podcast sentence — they are not podcasts,
  // and a listener told the wrong reason cannot act on it.
  it('names a local file and says it will never be in the history', async () => {
    const section = await card(
      payload({
        observation: observation({
          kind: 'local',
          title: 'demo-2004.mp3',
          artist: 'Unreleased',
          trackId: '',
        }),
      }),
    )
    expect(within(section).getByText('demo-2004.mp3')).toBeInTheDocument()
    expect(
      within(section).getByText('Local files are not part of your listening history.'),
    ).toBeInTheDocument()
    expect(within(section).queryByText(/Podcasts/)).not.toBeInTheDocument()
  })

  // Fails when: the unknown branch renders a title — an advert's own label would
  // then sit where a listener expects their music.
  it('says something is playing that it cannot identify, and names nothing', async () => {
    const section = await card(
      payload({
        observation: observation({
          kind: 'unknown',
          title: '',
          artist: '',
          trackId: '',
          progressMs: null,
          durationMs: null,
        }),
      }),
    )
    expect(
      within(section).getByText('Spotify is playing something Encore cannot identify.'),
    ).toBeInTheDocument()
    expect(
      within(section).getByText('It will not appear in your listening history.'),
    ).toBeInTheDocument()
    expect(within(section).queryByText('The Wheel')).not.toBeInTheDocument()
  })

  // No copy assertion can catch a literal escape sequence, because it is a valid
  // string that compiles and renders. This one can: Phase 3a shipped a Critical
  // that rendered "…" on screen.
  //
  // Fails when: any copy in this card is written with a \uXXXX escape in bare
  // JSXText.
  it('renders no literal escape sequences anywhere in the card', async () => {
    const section = await card(payload({ observation: observation() }))
    expect(section.textContent ?? '').not.toMatch(/\\u[0-9a-fA-F]{4}/)
  })
})

describe('the now-playing poll', () => {
  // Stopping is the property. Asserting the interval's value would pass for a
  // card that polls a disabled instance for ever.
  //
  // Fails when: the enabled guard is dropped from nowPlayingPollInterval — the
  // count below climbs past 1.
  it('asks once and stops, on an instance that does not poll', async () => {
    vi.useFakeTimers()
    const asked = stubRoutes({
      ...DASHBOARD_BODIES,
      '/api/nowplaying': payload({ enabled: false, intervalSeconds: 0 }),
    })
    render(mountAt('/'))

    await act(async () => {
      await vi.advanceTimersByTimeAsync(100)
    })
    await act(async () => {
      await vi.advanceTimersByTimeAsync(10 * 60 * 1000)
    })

    const calls = asked.filter((path) => path === '/api/nowplaying').length
    expect(calls).toBe(1)
  })

  // Fails when: the scopeGranted guard is dropped — an account that can never be
  // polled would have every open tab asking for ever.
  it('asks once and stops, for an account that has not granted the scope', async () => {
    vi.useFakeTimers()
    const asked = stubRoutes({
      ...DASHBOARD_BODIES,
      '/api/nowplaying': payload({ scopeGranted: false }),
    })
    render(mountAt('/'))

    await act(async () => {
      await vi.advanceTimersByTimeAsync(100)
    })
    await act(async () => {
      await vi.advanceTimersByTimeAsync(10 * 60 * 1000)
    })

    const calls = asked.filter((path) => path === '/api/nowplaying').length
    expect(calls).toBe(1)
  })

  // Fails when: refetchInterval is removed or hard-coded to false for a healthy
  // instance — the card then never updates and shows one observation for ever.
  it('keeps asking while the instance polls and the account can be polled', async () => {
    vi.useFakeTimers()
    const asked = stubRoutes({
      ...DASHBOARD_BODIES,
      '/api/nowplaying': payload({ observation: observation() }),
    })
    render(mountAt('/'))

    await act(async () => {
      await vi.advanceTimersByTimeAsync(100)
    })
    expect(asked.filter((p) => p === '/api/nowplaying').length).toBe(1)

    await act(async () => {
      await vi.advanceTimersByTimeAsync(30_100)
    })
    expect(asked.filter((p) => p === '/api/nowplaying').length).toBe(2)
  })
})
```

Plus the three `nowPlayingPollInterval` cases from Step 4, in the same file.

Two Settings tests go in `web/src/test/settings-status.test.tsx`, beside the existing metadata-panel assertions:

```tsx
  // The dashboard renders no card when the poller is off, so this is the only
  // place the state is explained. It follows the house formula the album and
  // artist pages use for a feature an operator has turned off.
  //
  // Fails when: the sentence stops naming the key — "an administrator can turn
  // this on" with no key is advice nobody can act on.
  it('says the instance does not ask, and names the key that would change it', async () => {
    // …stub /api/nowplaying with { enabled: false, intervalSeconds: 0, … }
    expect(await screen.findByText('Now playing is turned off')).toBeInTheDocument()
    expect(
      screen.getByText(
        'This instance does not ask Spotify what you are playing right now, so the dashboard shows no now-playing card. Every other figure in Encore comes from your own listening history and is unaffected. An administrator can turn this on with ENCORE_NOWPLAYING_INTERVAL.',
      ),
    ).toBeInTheDocument()
  })

  // Fails when: intervalPhrase is replaced by a raw number — "every 60 seconds"
  // for a minute reads as a machine's answer, and "every 1 minutes" is the
  // defect the helper exists to prevent.
  it('says how often it asks, and that it records nothing', async () => {
    // …stub /api/nowplaying with { enabled: true, intervalSeconds: 60, … }
    expect(await screen.findByText('Now playing is on')).toBeInTheDocument()
    expect(
      screen.getByText(
        'Encore asks Spotify what you are playing every minute. It records nothing from those checks — your listening history still comes only from the recently-played feed.',
      ),
    ).toBeInTheDocument()
  })
```

- [ ] **Step 9: Run the web suite**

```bash
cd web
npm run typecheck
npm run lint
npm run test
```

Expected: PASS on all three. `npm run test` is what CI's `web` job runs, and every assertion above is a copy assertion no Go test can make.

- [ ] **Step 10: Read it in a browser**

Nothing in this project has ever been opened in a browser, and Phase 3a shipped a Critical that a green suite could not catch. **Do this once**, and record what you saw in the PR description:

```bash
cd web && npm run dev
```

Open the dashboard against a running instance with `ENCORE_NOWPLAYING_INTERVAL=30s` and confirm three things a test cannot: that the card sits above the timeline chart and does not shift the layout as it fills, that the meter is visible and its width tracks the figure, and that no `\u`, `&amp;` or stray `—` appears anywhere on screen. If a browser is not available, say so explicitly rather than skipping the step silently.

- [ ] **Step 11: Commit**

```bash
git add web/src/lib/types.ts web/src/lib/format.ts web/src/lib/format.test.ts \
        web/src/lib/query.ts web/src/pages/Dashboard.tsx web/src/pages/Settings.tsx \
        web/src/test/nowplaying.test.tsx web/src/test/settings-status.test.tsx
git commit -m "$(cat <<'MSG'
Web: a now-playing card, with a sentence for every state it can be in

Sixteen states, and the two easiest to conflate are kept furthest apart: a null
observation is "Encore has not checked yet" and an observation whose state is
idle is "Nothing is playing". They share no sentence and no code path, because
telling somebody their player is silent when nobody has looked is a claim about
their evening that nobody checked.

A failed check drops the chip and the progress figure. "Playing" above a
four-minute-old observation is a present-tense claim nobody confirmed, and a
progress reading from four minutes ago beside it reads as a live one.

The second line is whatever the server sent, rendered only when it is not empty,
with no fallback string. A kind-dependent "Unknown artist" would be three more
strings and three more ways to be wrong; an absent line says the same thing and
cannot be.

When the poller is off the card is not rendered at all. The dashboard is the
home screen and a panel saying "turned off" on every load for ever is a nag
about a decision the listener cannot change; Settings, which is already where
instance configuration is explained, says it instead.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
MSG
)"
```

---

## Task 7: The documents this phase made stale

**Files (all Modify):**
`docs/api.md`, `docs/architecture.md`, `docs/feature-parity.md`, `docs/operations.md`, `docs/security.md`, `README.md`, `docs/design/2026-07-29-spotify-api-expansion-overview.md`, `internal/config/config.go`, `internal/worker/worker.go`, `cmd/encore-worker/main.go`, `cmd/encore-api/main.go`, `.env.example` (if Task 2 did not already finish it).

**Interfaces:** none. This task adds no code path; it makes existing prose true.

**Why this is a task and not a footnote.** Phase 2e-ii's final review found **five** documents claiming the album-tracks fetch was "the one unattended request" — true when written, false once a second existed. Phase 3a found **nine** claiming Encore only reads. The pattern is not carelessness: it is that a sentence naming a count or an ordinal is correct exactly once, and nothing in CI reads prose. So the sweep is planned, the files are named, and the verification is a grep whose output must be empty.

**Everything below was found by reading the files, not guessed.** Where a line number is given it was correct at `c939306`; re-locate by the quoted text rather than by the number.

### 7.1 — Ordinals that a third unattended request makes false

| File | Was | Becomes |
|---|---|---|
| `docs/api.md:195` | "It is the **first** Spotify request `encore-api` makes that nobody clicked for" | "It is one of two Spotify requests `encore-api` makes that nobody clicked for — the other is the discography walk in §5.4 — so an operator can switch it off with `ENCORE_ALBUM_TRACKS_ENABLED=false`." |
| `docs/api.md:275` | "It is the **second** Spotify request `encore-api` makes that nobody clicked for" | "It is the other of the two Spotify requests `encore-api` makes that nobody clicked for, so an operator can switch it off with `ENCORE_ARTIST_ALBUMS_ENABLED=false`" |
| `.env.example:160-161` | "`ENCORE_ARTIST_ALBUMS_ENABLED` below is **the other unattended request**" | Task 2 already replaced this with the three-way list. Confirm it landed. |

Both `docs/api.md` sentences say `encore-api`, and the poller lives in `encore-worker` — which is exactly why they are "technically survivable and therefore exactly the kind of thing that ships stale twice". Saying "two" rather than "first" and "second" is what makes them stop being ordinals that a third thing invalidates.

`docs/api.md:280-281` also says, of both:

> **A rate-limit response to either pauses Spotify access instance-wide** for the window Spotify asks for, which 409s "sync now" for every user until it lifts.

That remains true of those two and is now no longer true of *every* background request, so it gains one clause:

> **A rate-limit response to either pauses Spotify access instance-wide** for the window Spotify asks for, which 409s "sync now" for every user until it lifts. The now-playing poller is the one background caller this does not describe: it draws on a rate budget of its own, so a 429 on it pauses that loop alone.

### 7.2 — Loop lists, all of which are already stale and get worse

| File | Was | Becomes |
|---|---|---|
| `docs/architecture.md:181` | "`encore-worker` supervises **four independent loops**" | "`encore-worker` supervises **eight independent loops**" |
| `docs/architecture.md:183-190` | bullets naming Import runner, Enrichment, Sync, Rollups | add **Library** ("enumerates saved tracks, saved albums, followed artists, the listener's own playlists and Spotify's own top rankings, once a day"), **Now playing** ("checks each connected account's player every `ENCORE_NOWPLAYING_INTERVAL`, and does not run at all unless that is set"), **Reaper** ("clears expired sessions and OAuth states") and **Telemetry** ("publishes pool statistics") |
| `docs/architecture.md:24` | "`encore-worker` \| Import jobs, metadata enrichment, recently-played synchronisation, rollup maintenance." | "`encore-worker` \| Import jobs, metadata enrichment, recently-played synchronisation, library enumeration, the optional now-playing poller, rollup maintenance and session reaping." |
| `cmd/encore-worker/main.go:1-3` | "import jobs, catalogue enrichment, the recently-played poller, rollup maintenance and the reaper…" | add library enumeration and "the optional now-playing poller, which runs only when `ENCORE_NOWPLAYING_INTERVAL` is set" |
| `internal/worker/worker.go:3-5` | "one import runner per configured slot, the enrichment worker, the recently-played poller and the session reaper" | add the library worker and the now-playing poller |

`internal/worker/worker.go:18-20` says a loop returning nil "has decided it is finished — the poller does exactly that when `ENCORE_SYNC_ENABLED` is false". Extend it: "— the recently-played poller does exactly that when `ENCORE_SYNC_ENABLED` is false, and the now-playing poller whenever `ENCORE_NOWPLAYING_INTERVAL` is unset, which is most instances".

`docs/architecture.md:23` says of `encore-api`: **"Never calls Spotify except during the OAuth exchange."** That is *already* false — the album-tracks and discography fetches both fire from `encore-api` — and it sits three lines from the list above. Correct it in the same pass:

> `encore-api` | … Calls Spotify during the OAuth exchange, when somebody creates or renames a playlist, and — unless an operator has turned them off — when somebody opens an album or artist page. It never calls Spotify to serve `/api/nowplaying`, which reads the observation the worker stored.

### 7.3 — What a 429 pauses

`internal/spotify/client.go`'s comment at the old `interactive` field is already rewritten by Task 1. The prose that describes the same behaviour is not:

| File | Change |
|---|---|
| `docs/operations.md:219-227` | The "Held back / Unaffected" table gains a row: **"Now-playing checks"** held back / **"everything else, including signing in, syncing and enrichment"** unaffected — and the reverse row, so that a *catalogue* 429 lists the now-playing poller under **Unaffected**. Both directions, because the isolation only means something if it is stated in both. |
| `docs/operations.md:219-221` | "**Signing in keeps working throughout.** Authentication draws on a rate budget of its own" gains: "so does the optional now-playing poller, for the same reason and in both directions: nothing a background loop does may take authentication offline, and the least important request Encore makes must not be able to stop the rest." |
| `docs/feature-parity.md:60` | "A 429 pauses **every background caller** … Signing in draws on a separate budget that no catalogue 429 pauses" → "A 429 pauses every background caller **except the now-playing poller**, which has a budget of its own. Signing in draws on a third, which no catalogue 429 pauses." |
| `docs/configuration.md:95, :97` | Both say a 429 on the album/discography fetch "pauses Spotify access instance-wide". Still true of those two; leave them, and rely on the new **Now playing** section Task 2 added, which says plainly that this one does not. Re-read both after Task 2 to confirm they do not read as a claim about *every* request. |

### 7.4 — Scope enumerations, and the promise the banner has been making since Phase 2a

None of these becomes false. Every one of them stops being aspirational, and two of them are wrong for a different reason:

- `internal/config/config.go:536-540` — **already wrong today.** "the one write scope it can ever hold — `playlist-modify-private` — is requested separately". Phase 3a shipped two write scopes and corrected four Markdown files without touching this Go doc comment. Fix it: "the two write scopes it can ever hold — `playlist-modify-private` and `ugc-image-upload` — are requested together, separately from sign-in, at the moment somebody creates a playlist".
- `internal/config/config.go:542-543` — "**Five** separate consent interruptions". The read set is eight scopes covering more than five features. Replace the count with "A consent interruption per feature".
- `internal/config/config.go:560` — "Device and shuffle state for the optional now-playing poller." Shuffle state is **not** read by this phase (that is `GET /v1/me/player`, Phase 3c). Correct it to "Playback state for the optional now-playing poller, which reads `GET /v1/me/player/currently-playing` when `ENCORE_NOWPLAYING_INTERVAL` is set."
- `web/src/components/layout/ReconsentBanner.tsx:35` — `'user-read-playback-state': "show what's playing now"` — **leave exactly as it is.** It has been promising this since Phase 2a and this phase is what makes it true. Changing it now would be churn, and the banner's closing sentence stays correct because this phase adds no scope.

`README.md:152-155`, `docs/security.md:155-158`, `docs/attribution.md:46-49`, `docs/feature-parity.md:95` and `:151-152` all say "eight read scopes" and list `user-read-playback-state`. All remain true. **Do not renumber anything.** `internal/config/config_test.go`'s `TestDefaultScopesAreTheEightReadScopes` is the guard, and it should not change.

### 7.5 — The limitation that is *not* addressed, and the one that is

The spec's §4 says `docs/feature-parity.md`'s live-sync fidelity limitation "moves from known gap to addressed when the poller is enabled". **That is Phase 3c's edit, not this one** — this phase's poller writes no `shuffle` and no `platform`. Leave every one of these unchanged:

`README.md:322-329`; `docs/feature-parity.md:176-179` and `:98`; `docs/api.md:136-138` and `:371-373`; `internal/stats/context.go:8-14`; `web/src/pages/Habits.tsx:4-11` and `:330-333`; `migrations/00005_listens.sql:38-39`; `docs/import.md:255-264`.

What **is** addressed is a different limitation, and it goes in `docs/feature-parity.md` as a new row rather than as an edit to an existing one:

| Now playing | **Implemented, opt-in** | A card on the dashboard showing what you are playing right now, polled every `ENCORE_NOWPLAYING_INTERVAL`. Off unless that is set. It is a read-only observer: nothing it sees enters your listening history. |

And the thing that must **not** be conflated with it, which the spec's §4 warns about by name — playback *control* stays declined. `README.md:337-339`, `docs/feature-parity.md:96` and `:172-174`, `docs/security.md:168-169`, `docs/attribution.md:51-52` all say so. **Read each one and confirm it still reads as a refusal after the now-playing row exists.** "Encore can see what you are playing but will not change it" is the sentence that has to survive; if any of the five now reads as though seeing implies controlling, add the distinction there rather than removing the refusal.

### 7.6 — The design overview's counts

`docs/design/2026-07-29-spotify-api-expansion-overview.md:16-28` says "Encore calls **eight operations across seven paths**" and is already stale by several. Update the count and the table to what is true after this phase, including `GET /v1/me/player/currently-playing`. `:68`'s feature row `| 8 | Now playing, and shuffle/platform backfill | GET /me/player | user-read-playback-state | 3 |` splits into the shipped half and the deferred half:

> | 8a | Now playing, the live card | `GET /me/player/currently-playing` | `user-read-playback-state` | 3b — shipped |
> | 8b | Shuffle and platform backfill | `GET /me/player` | `user-read-playback-state` | 3c — deferred, see the 3b plan |

`:155-158`'s share matrix already says `| Now playing | No |` with the reasoning. Confirm it still reads correctly now that the feature exists, and that `docs/api.md`'s new section says the same thing.

### 7.7 — README

The limitations section gains one sentence, and the feature list gains one line:

> **Now playing.** With `ENCORE_NOWPLAYING_INTERVAL` set, the dashboard shows what you are listening to right now. It is off by default because it costs a Spotify request per account per interval, and it never writes to your listening history — that still comes only from the recently-played feed. It cannot be shared: a share link shows what somebody listens to, never when they are awake.

- [ ] **Step 1: Make every edit above**

Work file by file, in the order the table gives. Re-locate each sentence by its quoted text.

- [ ] **Step 2: Verify with greps whose output must be empty**

```bash
# No ordinal claims about unattended requests survive.
grep -rn "the first Spotify request\|the second Spotify request\|the one unattended\|the other unattended" \
  docs/ README.md .env.example

# No claim that a 429 pauses every background caller.
grep -rn "pauses every background caller" docs/

# No claim that encore-api never calls Spotify outside OAuth.
grep -rn "Never calls Spotify except" docs/

# No claim of one write scope.
grep -rn "the one write scope" internal/ docs/ README.md

# No stale loop count.
grep -rn "four independent loops" docs/
```

Every one of these must print nothing. If any prints a line, the edit was missed.

Then the greps that must print **something**, because a claim was replaced rather than deleted:

```bash
grep -rn "ENCORE_NOWPLAYING_INTERVAL" docs/ README.md .env.example docker-compose.yml \
  docker-compose.portainer.yml internal/config/config.go
grep -rn "budget of its own" docs/operations.md docs/feature-parity.md
```

- [ ] **Step 3: Confirm playback control is still declined in all five places**

```bash
grep -rn "user-modify-playback-state\|playback control\|control playback" \
  README.md docs/feature-parity.md docs/security.md docs/attribution.md \
  docs/design/2026-07-29-spotify-api-expansion-overview.md
```

Read each hit. Every one must still read as a refusal, and none may have been softened into "not yet".

- [ ] **Step 4: Run every gate**

```bash
export PATH="$PATH:$(go env GOPATH)/bin"
gofmt -l $(git ls-files '*.go'); go vet ./...; staticcheck ./...
go test -count=1 ./...
go test -count=1 ./test/deploy/
ENCORE_TEST_DATABASE_URL="postgres://encore:encore@localhost:5433/encore?sslmode=disable" \
  go test -tags=integration -count=1 -p 1 -timeout=20m ./test/integration/
cd web && npm run typecheck && npm run lint && npm run build && npm run test && cd ..
git diff --exit-code docker-compose.portainer.yml
for f in $(git diff --name-only HEAD~6..HEAD); do
  perl -0777 -ne 'print "NULs: ", tr/\0//, " in '"$f"'\n"' "$f" 2>/dev/null
done
```

**Report real output. Do not claim a pass on a command you did not run.** `go test -race` is absent because there is no gcc locally; CI runs it on the pull request, and this phase adds one concurrent construct — the poller's semaphore and its `atomic.Int64` — which is exactly the shape a race detector is for. Open the PR and read the `unit` job before calling this done.

- [ ] **Step 5: Commit**

```bash
git add docs/ README.md internal/config/config.go internal/worker/worker.go \
        cmd/encore-worker/main.go cmd/encore-api/main.go .env.example
git commit -m "$(cat <<'MSG'
Docs: a third unattended request, an eighth loop, and a budget of its own

Phase 2e-ii found five documents calling the album-tracks fetch "the one
unattended request" — true when written, false once a second existed. Phase 3a
found nine claiming Encore only reads. A sentence naming a count or an ordinal
is correct exactly once, and nothing in CI reads prose, so this sweep is planned
rather than remembered.

The ordinals become a list. The worker's loop count, wrong by four before this
phase, becomes eight. The "a 429 pauses every background caller" sentence gains
its exception, in both directions, because an isolation that is only stated one
way reads as an oversight the next time somebody adds a loop.

Two sentences were already wrong: config.go still said Encore holds one write
scope, which phase 3a made false in four Markdown files and never here; and
architecture.md said encore-api never calls Spotify outside the OAuth exchange,
which the album and artist page fetches made false two phases ago.

The live-sync fidelity limitation is deliberately untouched. That is phase 3c's
edit: this poller writes no shuffle and no platform, and marking a gap closed
before it is would be worse than leaving it open.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
MSG
)"
```

---

## Definition of done

- [ ] Every gate in Task 7 step 4 passes, on real output.
- [ ] The migration applies, rolls back and re-applies.
- [ ] **`ENCORE_NOWPLAYING_INTERVAL` unset means the loop returns before listing a single account** — pinned by a test with a context that is never cancelled, so a loop that started would hang rather than pass.
- [ ] **A 429 on the now-playing path never reaches `onPause`**, never pauses the catalogue limiter, and never pauses the sign-in limiter — three separate assertions in one test.
- [ ] **A 429 backs off the poller alone**: the next check makes zero HTTP requests and answers immediately.
- [ ] **The poller adds zero rows to `listens`** — asserted twice, once by import graph and once by row count.
- [ ] **A 403 never parks an account and is never retried.**
- [ ] **An account without `user-read-playback-state` is skipped without a request.**
- [ ] **"Nothing is playing" and "Encore has not checked" share no sentence and no code path**, at the database (a CHECK), at the DTO (a null observation) and in the card (two branches).
- [ ] Every one of the sixteen copy states in Task 6's table is asserted in full, not by substring.
- [ ] `intervalPhrase` has no singular defect: `60` renders `minute`, never `1 minutes`.
- [ ] `GET /api/nowplaying` makes no Spotify request, and appears on no share link.
- [ ] `go.mod`, `go.sum` and `web/package.json` are unchanged.
- [ ] The one new key is in all five places, and `docker-compose.portainer.yml` was **regenerated**, not edited.
- [ ] `DefaultScopes()`, `SCOPE_EXPLANATIONS` and `ReconsentBanner.tsx` are unchanged.
- [ ] Playback control is still declined in all five places that decline it.
- [ ] The live-sync shuffle/platform limitation is **not** marked addressed.

---

## Self-review

**1. Spec coverage — §2 of the design document, requirement by requirement.**

| Spec | Task |
|---|---|
| §2 `GET /me/player` answers 204 when nothing is playing, and that is not an error | 1 (the `status` field), 4 (`observe(nil, …)`) |
| §2.1 `ENCORE_NOWPLAYING_INTERVAL`, unset means disabled | 2, 4 |
| §2.1 the quota table in `docs/configuration.md` | 2 |
| §2.1 "it draws on the interactive budget" | 1 — **deviated from, at length, in the decisions section** |
| §2.1 only users with `user-read-playback-state` and not `needs_reauth` are polled, without a request | 3 (the SQL predicate), 4 (`hasScope`) |
| §2.2 the poller never writes listens | 4 (import graph, row count), 3 (a table with no listen columns) |
| §2.3 `GET /api/nowplaying` returns the last observation with its age | 5 |
| §2.3 the client polls it; no streaming transport | 6 |
| §2.3 a card showing what is playing, on what device, with progress | 6 |
| §2.3 never on a share link | 5 |
| §2.4, §2.5 shuffle/platform backfill | **Deferred to Phase 3c**, on the spec's own authority |
| §3 "Poller disabled: no request is ever issued" | 4 (`TestADisabledPollerNeverRuns`) |
| §3 "204 handling: idle, not an error, does not trip retry" | 1, 4 |
| §3 "Scope skip: skipped without a request" | 4 |
| §3 "No phantom listens: zero rows" | 4 |
| §3 "Backfill window", "Observation expiry" | Phase 3c |
| §4 `docs/configuration.md`, `.env.example` | 2 |
| §4 `docs/feature-parity.md`, `README.md` | 7 |
| §5 playback control, public playlists, `repeat_state` and volume all declined | 7 (confirmed, not changed) |

**Gaps found and closed while reviewing:**

- §4 also lists `ENCORE_LIBRARY_SYNC_INTERVAL` as needing documentation. It **already exists** in all five places — verified at `docs/configuration.md`'s "Library and follows" table, `.env.example:130-144`, `docker-compose.yml:87` and the generated stack. Nothing to do, and this line is here so nobody adds it twice.
- §2.3 says the card shows "what is playing, on what device". `/v1/me/player/currently-playing` may not carry `device`. Closed by making the device clause conditional in Task 6 and saying so in the copy table, rather than by switching endpoints.
- The spec never says what happens when a check *fails*. Left unspecified, it would have become "nothing is playing", which is the exact conflation §2's design is careful about elsewhere. Closed by `now_playing.failed` (Task 3), `Failed` on the DTO (Task 5) and states 8, 10 and 16 of the copy table (Task 6).

**Gaps found and deliberately left open:**

- **A persistent 403 has no distinct user-facing state.** It records a failed check, so the card says "The last check failed", which is true. Reporting it as "your grant lacks the scope" would need a second column and a seventeenth copy state, for a case that requires Spotify to contradict the grant it issued. Recorded rather than built.
- **The card does not render on an empty dashboard.** Named in the out-of-scope list at the top.
- **No metrics.** Named in the out-of-scope list at the top.

**2. Placeholder scan.** Three steps defer to an existing file for scaffolding rather than writing it out, and I am recording that rather than hiding it:

- Task 4 step 1's `newTestWatcher` fakes, and step 5's `connectWithPlaybackScope` / `newWatcher` helpers, defer to `test/integration/libraryworker_test.go`'s idiom.
- Task 5 step 1's `testDeps` / `newTestServer` / `getSharedStatsBody` defer to `internal/httpapi/httpapi_test.go`'s existing fake set.
- Task 6 step 8's `DASHBOARD_BODIES` and `ME` defer to whichever dashboard-rendering test already defines them.

In each case the *assertions* — which is where every defect this project has shipped actually lived — are written out in full. Everything else in the plan carries complete code.

**3. Type consistency.** Checked end to end:

- `domain.PlaybackState` values (`unknown`/`idle`/`playing`/`paused`) = the `now_playing.state` CHECK = the DTO's `State` string = `PlaybackState` in `types.ts`, **minus `unknown`**, which never crosses the wire because a null observation says it instead. That asymmetry is deliberate and is stated in both the DTO comment and the TS comment.
- `domain.PlaybackItemKind` values (`none`/`track`/`episode`/`local`/`unknown`) = the `kind` CHECK = the DTO's `Kind` = `PlaybackItemKind` in `types.ts` = the five keys of `KIND_NOTE`. All five, in all five places.
- `domain.NowPlaying.TrackKnown` (Go, from a join) → `NowPlayingObservation.TrackID` being non-empty (wire) → `trackId` deciding whether the title is a link (TSX). Three representations of one fact; the mapping is in `toNowPlayingObservation`.
- `accounts.DueAccount{UserID, Scopes}` is produced by `ListDue` (Task 3) and consumed by `check` (Task 4) — same field names, same order.
- `nowplaying.Observations` (Task 4's interface) names exactly `ListDue`, `Record`, `RecordFailure` with the signatures Task 3 produces.
- `config.NowPlaying.Enabled()` is used by `Watcher.Run` (Task 4), `handleNowPlaying` (Task 5) and `Redacted` (Task 2) — one method, three callers, no second definition of "on".
- `scopeReadPlaybackState` is declared in **three** packages (`accounts`, `nowplaying`, `httpapi`) with the same literal. That is deliberate — none of the three should import another for a string constant — and each declaration carries a comment saying it matches `config.DefaultScopes()`. `TestDefaultScopesAreTheEightReadScopes` is what keeps the literal honest.
- `intervalPhrase(seconds)` is called by the Dashboard card (state 7) and by Settings (state 3), both with `data.intervalSeconds`.
- `nowPlayingPollInterval(data)` takes `NowPlaying | undefined` and is called with `query.state.data`, which is exactly that type.

**4. One thing I could not verify and the executor must.** Whether `GET /v1/me/player/currently-playing` returns a `device` object on this instance's accounts. Spotify documents the same response schema for it and for `GET /v1/me/player`, but the narrower endpoint is observed to omit it. Everything in this plan treats it as optional and nothing breaks if it never arrives — the device clause simply never renders, and the copy table's rule 3 covers it. **Confirm it with one real request during Task 6 step 10** and record what you saw. If it is reliably absent, that is worth a sentence in `docs/api.md` beside `deviceName`; it is not worth switching to `/v1/me/player`, which is Phase 3c's endpoint and returns a payload this phase does not use.
