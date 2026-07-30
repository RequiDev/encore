# Phase 2a — Consent Migration Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Grow Encore's sign-in OAuth grant from three read scopes to eight, and give every existing account an honest, non-blocking route to re-consent.

**Architecture:** `config.DefaultScopes()` is the single source of truth for what Encore asks for; `spotify_credentials.scopes` records what each account actually granted. The server diffs the two and reports the shortfall on `/api/me`, so the client never hardcodes a scope list. A dismissible banner links to the existing relink flow. A 403 from a missing scope is classified as permanent, never retried.

**Tech Stack:** Go 1.26, pgx/v5, PostgreSQL 17, React 19 + TypeScript + TanStack Query.

**Spec:** [`docs/design/2026-07-29-phase-2-scope-expansion-design.md`](../../design/2026-07-29-phase-2-scope-expansion-design.md) §1.

## Global Constraints

- **No database migration.** `spotify_credentials.scopes text[]` already exists (`migrations/00002_users.sql:31`). If you find yourself writing DDL, stop — you have misread the plan.
- **No new Go module dependency and no new npm dependency.**
- **No feature is built in this plan.** It ships the consent change and nothing that consumes it. Resist adding an endpoint "while you're in there".
- The re-consent prompt is **dismissible and never blocks**. An account that ignores it forever keeps working exactly as it does today, minus the features it has not granted.
- `ugc-image-upload` is **not** added to the sign-in set. It is a write scope, requested with `playlist-modify-private` at playlist-creation time in a later phase.
- Test database is on port **5433**, not 5432. `make` is NOT installed on this machine, though the Makefile exists — run its recipes directly.
- `go test -race` will NOT work: there is no gcc, so cgo is unavailable. Omit `-race`. CI runs it on Linux.
- staticcheck lives at `$(go env GOPATH)/bin`; run `export PATH="$PATH:$(go env GOPATH)/bin"` before linting.
- **Check every file you write for NUL bytes:** `perl -0777 -ne 'print "NULs: ", tr/\0//, "\n"' <file>` — expect `NULs: 0`. A Phase 1 task embedded one and lint, typecheck, build and every test still passed.
- Commit style `Area: lowercase summary`, body explaining *why*, ending with `Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>`. Stage paths explicitly; never `git commit -a`.

---

## File Structure

| File | Responsibility |
|---|---|
| `internal/config/config.go:395-399` | **Modify.** `DefaultScopes()` grows from 3 to 8 |
| `internal/spotify/scopes.go` | **Create.** `MissingScopes` — the one place the shortfall is computed |
| `internal/httpapi/dto.go` | **Modify.** `missingScopes` on the `/api/me` payload |
| `internal/httpapi/me.go:72` | **Modify.** Populate it |
| `internal/sync/account.go` | **Modify.** A scope 403 must not mark `needs_reauth` |
| `web/src/lib/types.ts` | **Modify.** `missingScopes` on the session type |
| `web/src/components/layout/ReconsentBanner.tsx` | **Create.** The prompt |
| `docs/feature-parity.md`, `docs/security.md`, `docs/api.md` | **Modify.** Say what is now true |

---

### Task 1: Widen the sign-in scope set

**Files:**
- Modify: `internal/config/config.go:395-399`
- Test: `internal/config/config_test.go`

**Interfaces:**
- Produces: `config.DefaultScopes() []string` returning eight scopes in the order below. Five callers already exist (`config.go:317`, `internal/spotify/client_test.go:70`, `test/e2e/e2e_test.go:65`, `test/integration/sync_test.go:127`, and five more e2e call sites that append to it) and all pick the change up automatically.

- [ ] **Step 1: Write the failing test**

Add to `internal/config/config_test.go`. Read the file first and match its existing idiom for table tests.

```go
// TestDefaultScopesAreTheEightReadScopes pins the sign-in grant.
//
// The list is asserted exactly, in order, rather than by length or by
// membership: this is the set every listener is asked to consent to, and it
// should not be possible to widen it without a test changing to say so.
func TestDefaultScopesAreTheEightReadScopes(t *testing.T) {
	want := []string{
		"user-read-recently-played",
		"user-read-private",
		"user-read-email",
		"user-top-read",
		"user-library-read",
		"user-follow-read",
		"playlist-read-private",
		"user-read-playback-state",
	}
	got := DefaultScopes()
	if !slices.Equal(got, want) {
		t.Errorf("DefaultScopes() =\n  %v\nwant\n  %v", got, want)
	}
}

// TestDefaultScopesAreAllReadOnly is the property that matters more than the
// list itself: nothing here can alter a listener's Spotify account. Every write
// scope Encore ever requests is asked for at the point of use instead.
func TestDefaultScopesGrantNoWriteAccess(t *testing.T) {
	for _, s := range DefaultScopes() {
		if strings.Contains(s, "modify") || strings.Contains(s, "ugc-") {
			t.Errorf("%q is a write scope and must not be in the sign-in grant", s)
		}
	}
}
```

Add `"slices"` and `"strings"` to the test file's imports if absent.

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test -run TestDefaultScopes -count=1 ./internal/config/`
Expected: FAIL — got three scopes, want eight.

- [ ] **Step 3: Widen the list**

Replace `internal/config/config.go:395-399` with:

```go
// DefaultScopes is the grant Encore asks for at sign-in.
//
// Every one of these is read-only. Encore never asks, at sign-in, for
// permission to change anything about a listener's Spotify account: the two
// write scopes it can ever hold — playlist-modify-private and
// ugc-image-upload — are requested together at the moment somebody creates a
// playlist, and an account that never creates one is never asked.
//
// The read set is granted in one step rather than feature by feature. Five
// separate consent interruptions, each explaining a statistic the listener has
// not seen yet, is a worse experience than one; and every one of these is
// inert on its own — reading what somebody saved, follows, or ranked highly
// cannot alter any of it. See docs/security.md.
func DefaultScopes() []string {
	return []string{
		"user-read-recently-played",
		"user-read-private",
		"user-read-email",
		// Spotify's own ranking, to diff against Encore's.
		"user-top-read",
		// Saved tracks and albums, for saved-but-never-played.
		"user-library-read",
		// Followed artists, for followed-but-dormant.
		"user-follow-read",
		// Playlist names, so a listen's playlist context can be named.
		"playlist-read-private",
		// Device and shuffle state for the optional now-playing poller.
		"user-read-playback-state",
	}
}
```

- [ ] **Step 4: Run the tests**

Run: `go test -count=1 ./internal/config/` — expected PASS.
Run: `go test -count=1 ./...` — expected PASS. `internal/spotify/client_test.go:70` and others consume `DefaultScopes()` and must still pass.
Run: `ENCORE_TEST_DATABASE_URL="postgres://encore:encore@localhost:5433/encore?sslmode=disable" go test -tags=integration -count=1 -p 1 -timeout=20m ./test/...` — expected PASS. The e2e stub reports `config.DefaultScopes()` back as the granted set (`test/e2e/e2e_test.go:65`), so the whole suite exercises the new list end to end.

- [ ] **Step 5: Lint and commit**

```bash
export PATH="$PATH:$(go env GOPATH)/bin"
gofmt -l $(git ls-files '*.go') && go vet ./... && staticcheck ./...
git add internal/config/config.go internal/config/config_test.go
git commit -m "Auth: ask for the five read scopes the statistics need

Encore asked for three scopes and could therefore build only what
recently-played and a profile allow. Spotify's own ranking, a listener's
saved library, who they follow, and the names of their playlists are all
read-only and all behind scopes that were never requested.

Granted in one step rather than feature by feature. Five separate consent
interruptions, each explaining a statistic nobody has seen yet, is a worse
experience than one — and every one of these is inert: reading what somebody
saved or follows cannot alter it.

The property worth keeping is untouched, and is now the one the test asserts:
nothing in the sign-in grant can modify a Spotify account. The two write
scopes Encore can ever hold are still asked for at the moment a playlist is
created, and an account that never creates one is never asked.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
```

---

### Task 2: Report the shortfall on `/api/me`

**Files:**
- Create: `internal/spotify/scopes.go`, `internal/spotify/scopes_test.go`
- Modify: `internal/httpapi/dto.go`, `internal/httpapi/me.go:72`
- Test: `internal/httpapi/httpapi_test.go`

**Interfaces:**
- Consumes: `config.DefaultScopes()` from Task 1; `domain.SpotifyCredentials.Scopes []string` (`internal/domain/user.go:95`).
- Produces:
  - `func spotify.MissingScopes(granted, want []string) []string` — returns the entries of `want` absent from `granted`, in `want`'s order, never nil.
  - `SpotifyConnection.MissingScopes []string` with JSON tag `missingScopes`.

**Note the struct.** `scopes` lives on `SpotifyConnection` (`internal/httpapi/dto.go:88-95`), not on the top-level me payload, so `missingScopes` goes there beside it — the client reads it as `me.spotify.missingScopes` or whatever that object is named in the parent struct. Check the parent field name before writing the TypeScript in Task 4.

**Why the server computes this:** the client must not hold its own copy of the required list. Two lists drift, and the one in TypeScript would drift silently.

- [ ] **Step 1: Write the failing unit test**

Create `internal/spotify/scopes_test.go`:

```go
package spotify

import (
	"slices"
	"testing"
)

func TestMissingScopes(t *testing.T) {
	for _, tc := range []struct {
		name    string
		granted []string
		want    []string
		missing []string
	}{
		{"nothing granted", nil, []string{"a", "b"}, []string{"a", "b"}},
		{"all granted", []string{"a", "b"}, []string{"a", "b"}, []string{}},
		{"some granted", []string{"b"}, []string{"a", "b", "c"}, []string{"a", "c"}},
		{"extra granted is not missing", []string{"a", "b", "z"}, []string{"a"}, []string{}},
		{"nothing wanted", []string{"a"}, nil, []string{}},
		// Spotify returns granted scopes space-separated in one string, and the
		// stored column has held them that way for accounts connected before the
		// value was split. Both shapes must work.
		{"space separated", []string{"a b"}, []string{"a", "b"}, []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := MissingScopes(tc.granted, tc.want)
			if got == nil {
				t.Fatal("MissingScopes returned nil; it must return an empty slice so the JSON is [] and not null")
			}
			if !slices.Equal(got, tc.missing) {
				t.Errorf("got %v, want %v", got, tc.missing)
			}
		})
	}
}

// TestMissingScopesPreservesWantOrder keeps the prompt's wording stable: the
// order the scopes are listed in is the order they are explained to the user.
func TestMissingScopesPreservesWantOrder(t *testing.T) {
	got := MissingScopes(nil, []string{"c", "a", "b"})
	if !slices.Equal(got, []string{"c", "a", "b"}) {
		t.Errorf("got %v, want the wanted order preserved", got)
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test -run TestMissingScopes -count=1 ./internal/spotify/`
Expected: FAIL — `undefined: MissingScopes`.

- [ ] **Step 3: Implement it**

Create `internal/spotify/scopes.go`:

```go
package spotify

import "strings"

// MissingScopes reports which of want a grant does not include, in want's own
// order, and never returns nil.
//
// It tolerates the two shapes a stored grant can take. Spotify returns granted
// scopes space-separated in a single string, and an account connected before
// Encore split them has one such string in its scopes column; a newer one has
// them as separate elements. Both are flattened here rather than at each call
// site, which is the same reason HasScope splits on spaces.
func MissingScopes(granted, want []string) []string {
	have := make(map[string]struct{}, len(granted))
	for _, g := range granted {
		for f := range strings.SplitSeq(g, " ") {
			if f != "" {
				have[f] = struct{}{}
			}
		}
	}
	missing := make([]string, 0, len(want))
	for _, w := range want {
		if _, ok := have[w]; !ok {
			missing = append(missing, w)
		}
	}
	return missing
}
```

- [ ] **Step 4: Wire it into `/api/me`**

In `internal/httpapi/dto.go`, on the `SpotifyConnection` struct, beside the existing `Scopes []string \`json:"scopes"\`` field (`dto.go:94`), add:

```go
	// MissingScopes is what this account granted less than Encore now asks for.
	//
	// Computed on the server against config.DefaultScopes() rather than compared
	// in the client, because two copies of the required list would drift and the
	// TypeScript one would drift silently. Empty means the grant is current.
	MissingScopes []string `json:"missingScopes"`
```

In `internal/httpapi/me.go`, beside the existing `Scopes: nonNil(creds.Scopes)` at line 72:

```go
		MissingScopes: spotify.MissingScopes(creds.Scopes, config.DefaultScopes()),
```

And in the no-credentials branch near `me.go:62` — which already sets `Scopes: []string{}` — add `MissingScopes: []string{}`. An account with no Spotify grant at all is not "missing scopes"; it is not connected, which the client already handles separately.

Add the `config` and `spotify` imports to `me.go` if absent.

- [ ] **Step 5: Write the API test**

Add to `internal/httpapi/httpapi_test.go`. The harness is `newTestServer(t)` (line 229), which builds a signed-in user with `credentials: &fakeCredentials{err: domain.ErrNotFound}` — i.e. not connected. Both tests below replace that fake's contents. `fakeCredentials` is at line 176 with fields `creds domain.SpotifyCredentials` and `err error`; requests go through `ts.do(httptest.NewRequest(...))` (line 283).

```go
// TestMeReportsMissingScopes is what drives the re-consent prompt.
//
// An account connected before the scope set grew has a grant that will never
// widen on its own — a refresh token carries the scopes it was issued with for
// ever — and Spotify answers 403 for anything needing the new ones. The
// shortfall has to reach the client or the failure is invisible.
func TestMeReportsMissingScopes(t *testing.T) {
	ts := newTestServer(t)
	ts.credentials.err = nil
	ts.credentials.creds = domain.SpotifyCredentials{
		UserID:         ts.sessions.user.ID,
		AccessToken:    "token",
		RefreshToken:   "refresh",
		TokenExpiresAt: ts.clock.Add(time.Hour),
		// The three scopes every account connected before this change holds.
		Scopes:    []string{"user-read-recently-played", "user-read-private", "user-read-email"},
		SyncState: domain.SyncStateOK,
	}

	rec := ts.do(httptest.NewRequest(http.MethodGet, "/api/me", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/me = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}

	var body MeResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not a me payload: %v (%s)", err, rec.Body.String())
	}

	want := []string{
		"user-top-read", "user-library-read", "user-follow-read",
		"playlist-read-private", "user-read-playback-state",
	}
	if !slices.Equal(body.Spotify.MissingScopes, want) {
		t.Errorf("missingScopes =\n  %v\nwant\n  %v", body.Spotify.MissingScopes, want)
	}
}

// TestMeReportsNoMissingScopesForACurrentGrant guards the other direction. A
// freshly connected account must never be nagged, and an empty result must
// serialise as [] rather than null so the client can test its length.
func TestMeReportsNoMissingScopesForACurrentGrant(t *testing.T) {
	ts := newTestServer(t)
	ts.credentials.err = nil
	ts.credentials.creds = domain.SpotifyCredentials{
		UserID:         ts.sessions.user.ID,
		AccessToken:    "token",
		RefreshToken:   "refresh",
		TokenExpiresAt: ts.clock.Add(time.Hour),
		Scopes:         config.DefaultScopes(),
		SyncState:      domain.SyncStateOK,
	}

	rec := ts.do(httptest.NewRequest(http.MethodGet, "/api/me", nil))
	var body MeResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not a me payload: %v (%s)", err, rec.Body.String())
	}
	if len(body.Spotify.MissingScopes) != 0 {
		t.Errorf("a current grant reported missing scopes: %v", body.Spotify.MissingScopes)
	}
	if !strings.Contains(rec.Body.String(), `"missingScopes":[]`) {
		t.Error("an empty missingScopes must serialise as [] and not null")
	}
}
```

**Three things to check against the real file before writing this**, because the plan cannot see them: the exact name of the top-level me response struct (used above as `MeResponse`) and of its Spotify sub-field (used as `body.Spotify`); whether `ts.sessions.user` is the right path to the fixture user's id; and the correct `domain.SyncState` constant name. Read `internal/httpapi/me.go` and the harness, and adjust. Add `slices`, `strings` and the `config` import as needed. The assertions are the requirement; the accessors must match reality.

- [ ] **Step 6: Run the tests**

Run: `go test -count=1 ./internal/spotify/ ./internal/httpapi/` — expected PASS.
Run: `go test -count=1 ./...` — expected PASS.

- [ ] **Step 7: Lint and commit**

```bash
export PATH="$PATH:$(go env GOPATH)/bin"
gofmt -l $(git ls-files '*.go') && go vet ./... && staticcheck ./...
git add internal/spotify/scopes.go internal/spotify/scopes_test.go internal/httpapi/dto.go internal/httpapi/me.go internal/httpapi/httpapi_test.go
git commit -m "Auth: tell the client which scopes an account is short

A refresh token carries the grant it was issued with, for ever. An account
connected before the scope set grew will never widen on its own, and Spotify
answers 403 for anything needing the new scopes — a failure the listener can
act on but only if something tells them.

The shortfall is computed on the server, against DefaultScopes(). The client
holding its own copy of the required list would mean two lists, and the one
in TypeScript would drift silently.

MissingScopes tolerates both shapes a stored grant takes: Spotify returns
scopes space-separated in one string, and accounts connected before Encore
split them still have it that way.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
```

---

### Task 3: A scope 403 is permanent, not a broken grant

**Files:**
- Modify: `internal/sync/account.go` (around line 299)
- Test: `internal/sync/sync_test.go`

**Interfaces:**
- Consumes: the existing `unauthorised(err)` and `forbidden(err)` predicates in `internal/sync/account.go:288-300`.
- Produces: no new exported surface. This task adds a documented rule and the comment that carries it. **It probably changes no logic at all.**

**Read this before touching anything — the obvious change here is wrong.**

`internal/sync/account.go:153-165` currently does:

```go
	case unauthorised(err), forbidden(err):
		return nil, p.markNeedsReauth(ctx, userID, "…")
```

It is tempting to read that as "a 403 wrongly marks the account broken" and loosen it. **Do not.** That branch guards exactly one call: `RecentlyPlayed`, which needs `user-read-recently-played` — one of the *original* three scopes, and the one Encore's entire ingestion depends on. A 403 there really does mean the grant no longer carries a scope Encore cannot work without, and parking the account instead of polling it into the rate limit every minute is correct. Leave it alone.

The rule this task establishes is about the endpoints that **do not exist yet**. When plans 2c, 2d and 2e add calls for the library, follows, top items and playlists, a 403 from any of them means "this account did not grant an *optional* scope" — and routing that into `markNeedsReauth` would stop ingestion for an account whose recently-played access is perfect, turning a missing optional statistic into a total outage.

So: pin the current behaviour with tests, and write the rule down where the next implementer will hit it.

- [ ] **Step 1: Characterise the current behaviour and report it**

Read `internal/sync/account.go:140-170` and `:285-300`. State in your report: which call the 403 branch guards, which scope that call needs, and whether it is one of the original three. If your reading contradicts the paragraph above, **stop and report** rather than proceeding — the plan would be wrong and I need to know.

- [ ] **Step 2: Write the tests that pin it**

`internal/sync/sync_test.go:370-380` already covers the `unauthorised`/`forbidden` predicates in isolation. What is missing is the *consequence*. `fakeSpotify` (line 27) has `plays` and `err` fields and stubs `RecentlyPlayed` and `RefreshToken`; `testDeps()` (line 42) builds the minimum a `Poller` needs.

```go
// TestRecentlyPlayedForbiddenParksTheAccount pins behaviour that looks wrong
// and is right.
//
// A 403 from recently-played is not "an optional feature was not granted" — it
// is the one scope Encore cannot work without having gone away. Parking the
// account is correct; polling it into the rate limit every minute would not be.
//
// The rule for the scopes added in this branch is the opposite, and it belongs
// to the endpoints that use them, not here: see the comment in account.go.
func TestRecentlyPlayedForbiddenParksTheAccount(t *testing.T) {
	// fakeSpotify{err: a 403 *spotify.APIError} -> fetch -> needs_reauth.
}

func TestRecentlyPlayedUnauthorizedParksTheAccount(t *testing.T) {
	// 401 -> needs_reauth, unchanged.
}
```

Fill the bodies from the real harness in that file. If constructing a `Poller` and observing `markNeedsReauth` turns out to need more scaffolding than the file provides, say so in your report and pin the behaviour at whatever level the file *does* support rather than building a new harness for it — the documented rule in step 3 is the more valuable half of this task.

- [ ] **Step 3: Write the rule down where it will be read**

Extend the comment on the `forbidden` helper at `internal/sync/account.go:296-300`:

```go
// forbidden reports a 403: the token is valid but does not carry the scope the
// endpoint needs.
//
// What that means depends entirely on which endpoint asked, and the two cases
// must not be conflated.
//
// For recently-played, below, a 403 means the grant lost user-read-recently-played
// — a scope Encore cannot function without — so the account is parked.
//
// For anything reading the library, follows, top items or playlists, a 403 means
// only that the listener did not grant an optional read scope, which is the
// ordinary state of every account connected before Encore began asking for them.
// Those callers must NOT reach markNeedsReauth: doing so would stop ingesting an
// account whose listening history still reads perfectly. They must not retry
// either — a scope failure spends quota to fail identically. They mark their own
// feature unavailable and let /api/me's missingScopes surface the prompt.
func forbidden(err error) bool {
```

- [ ] **Step 4: Run the tests**

Run: `go test -count=1 ./internal/sync/` — expected PASS.

- [ ] **Step 5: Run the suites**

Run: `go test -count=1 ./...` — expected PASS.
Run: `ENCORE_TEST_DATABASE_URL="postgres://encore:encore@localhost:5433/encore?sslmode=disable" go test -tags=integration -count=1 -p 1 -timeout=20m ./test/...` — expected PASS.

- [ ] **Step 6: Lint and commit**

```bash
export PATH="$PATH:$(go env GOPATH)/bin"
gofmt -l $(git ls-files '*.go') && go vet ./... && staticcheck ./...
git add internal/sync/account.go internal/sync/sync_test.go
git commit -m "Sync: say which 403s park an account and which must not

A 403 from recently-played means the grant lost user-read-recently-played,
which Encore cannot work without, so the account is parked rather than
polled into the rate limit every minute. That is existing behaviour, it is
correct, and it now has a test saying so.

A 403 from the endpoints this branch is about to add means something else
entirely: the listener did not grant an optional read scope, which is the
ordinary state of every account connected before Encore began asking. Those
callers must not park the account — that would stop ingesting a history that
still reads perfectly — and must not retry, which spends quota to fail
identically.

The two cases were about to become easy to conflate, so the rule is written
on the predicate both of them go through.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
```

---

### Task 4: The re-consent banner

**Files:**
- Modify: `web/src/lib/types.ts`
- Create: `web/src/components/layout/ReconsentBanner.tsx`
- Modify: whichever layout component wraps the authenticated pages — **find it by reading `web/src/App.tsx`** rather than assuming a filename
- Test: `web/src/test/` — follow the existing test file idiom there

**Interfaces:**
- Consumes: `missingScopes: string[]` on the session/me payload from Task 2.
- Produces: a banner rendered once, above the page content, for any signed-in account with a non-empty `missingScopes`.

**Behaviour requirements — these are the task:**

1. **Dismissible, and the dismissal sticks.** Persist it in `localStorage` keyed by the *set of missing scopes*, so dismissing today's prompt does not suppress a future one for a different scope. A `sessionStorage` dismissal that returns on every tab is nagging; a permanent one that hides a later, different request is worse.
2. **Never blocks.** No modal, no overlay, no route guard. The page beneath it is fully usable.
3. **Explains what it is asking for, in plain language, not scope strings.** Map each scope to a phrase:
   - `user-top-read` → "compare Spotify's own ranking to yours"
   - `user-library-read` → "see what you saved but never played"
   - `user-follow-read` → "see which artists you follow but stopped playing"
   - `playlist-read-private` → "name the playlist a listen came from"
   - `user-read-playback-state` → "show what's playing now"
   Any scope not in the map falls back to the raw string rather than vanishing.
4. **States that it grants no write access**, because that is the honest and reassuring fact: "None of these let Encore change anything on your Spotify account."
5. The action links to `/api/auth/spotify/relink` — a top-level navigation, not a `fetch`, because it is an OAuth redirect.

Follow the visual idiom of the existing layout components. Do not invent a new banner component if one already exists — check `web/src/components/ui/` first.

- [ ] **Step 1: Add the type**

In `web/src/lib/types.ts`, on whichever interface models the `/api/me` payload (find it — it has `scopes: string[]`):

```ts
  /**
   * Scopes this account granted less than Encore now asks for.
   *
   * Computed server-side against the required list; the client deliberately
   * holds no copy of that list, because two copies drift.
   */
  missingScopes: string[]
```

- [ ] **Step 2: Write the failing component test**

In `web/src/test/`, following the idiom of the files already there (see `share.test.tsx` for how a page is rendered with a fixture):

```tsx
describe('ReconsentBanner', () => {
  it('says nothing when the grant is current', () => {
    // missingScopes: [] -> renders nothing
  })

  it('explains each missing scope in plain language', () => {
    // missingScopes: ['user-library-read'] -> the text mentions
    // 'saved but never played', and does NOT show the raw scope string
  })

  it('promises no write access', () => {
    // the copy contains the reassurance sentence
  })

  it('stays dismissed for the same set of scopes', () => {
    // dismiss, re-render -> nothing
  })

  it('returns for a different set of scopes', () => {
    // dismiss for ['user-library-read'], then render with
    // ['user-top-read'] -> the banner is back
  })
})
```

Fill in the bodies against the real testing-library helpers that file uses.

- [ ] **Step 3: Run it to verify it fails**

Run: `cd web && npm run test`
Expected: FAIL — the component does not exist.

- [ ] **Step 4: Build the banner and mount it**

Create the component to satisfy the five behaviours above, then mount it in the authenticated layout you identified.

- [ ] **Step 5: Verify**

Run: `cd web && npm run lint && npm run typecheck && npm run build && npm run test` — all must pass.
Run the NUL check on every file you created or modified.

- [ ] **Step 6: Commit**

```bash
git add web/src/lib/types.ts web/src/components/layout/ReconsentBanner.tsx <layout file> <test file>
git commit -m "Web: ask existing accounts to re-consent, once, without blocking

A refresh token carries the grant it was issued with for ever, so every
account that existed before the scope set grew is now short five scopes and
will get a 403 from anything that needs them.

The prompt is dismissible and the dismissal is keyed by the set of scopes
being asked for, so dismissing today does not silently suppress a different
request later. It never blocks: an account that ignores it forever keeps
working exactly as before, minus the features it has not granted.

It explains what each scope buys in plain language rather than printing
scope strings at somebody, and says outright that none of them let Encore
change anything on their account — which is true, and is the fact a person
deciding whether to click actually wants.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
```

---

### Task 5: Make the documentation true

**Files:**
- Modify: `docs/feature-parity.md` (deviation #6 and the read-only-scopes row)
- Modify: `docs/security.md:154-158`
- Modify: `docs/api.md` (the `/api/me` payload)

**Interfaces:** none.

Three documents currently assert that Encore asks for three read scopes at sign-in and defers everything else. After Task 1 that is false. **A document asserting a property the code no longer has is worse than no document.**

- [ ] **Step 1: Rewrite `feature-parity.md` deviation #6**

Replace the existing item 6 under "Deliberate deviations, summarised" with:

```markdown
6. **Read scopes at sign-in, write scopes at the point of use.** Encore asks for
   eight read scopes when somebody signs in, and for a write scope only at the
   moment they use the feature that needs it. The narrower claim is the one that
   matters and it is unchanged: **Encore never holds a grant that can modify a
   listener's Spotify account unless they have used a feature that needs it.**
   `playlist-modify-private` and `ugc-image-upload` are requested together when a
   playlist is created; an account that never creates one holds a grant that
   cannot alter anything, even if the instance is compromised. Playback control
   is still declined outright.
```

- [ ] **Step 2: Correct the scopes row in the same file**

Find the row asserting read-only scopes in the accounts table and make it name all eight, keeping the read-only property explicit.

- [ ] **Step 3: Rewrite `docs/security.md:154-158`**

Replace the three-scope claim with the eight, and keep the sentence about `playlist-modify-private` being requested at the point of use. Add one sentence recording that accounts connected before the change keep their old grant until they relink, and that Encore reports the shortfall on `/api/me` rather than failing opaquely.

- [ ] **Step 4: Document `missingScopes` in `docs/api.md`**

Add the field to the `/api/me` payload description in that file's existing style, noting it is computed server-side and empty means current.

- [ ] **Step 5: Verify the docs match the code**

Run: `grep -rn "user-read-recently-played" docs/ | grep -v design/`
Every hit must list all eight scopes or explicitly be describing history. Read each one.

Run the full suite one last time — this is the plan's final evidence:
```
export PATH="$PATH:$(go env GOPATH)/bin"
gofmt -l $(git ls-files '*.go') && go vet ./... && staticcheck ./...
go test -count=1 ./...
ENCORE_TEST_DATABASE_URL="postgres://encore:encore@localhost:5433/encore?sslmode=disable" go test -tags=integration -count=1 -p 1 -timeout=20m ./test/...
cd web && npm run lint && npm run typecheck && npm run build && npm run test
```
**Report the real output. Do not claim a pass on a command you did not run.**

- [ ] **Step 6: Commit**

```bash
git add docs/feature-parity.md docs/security.md docs/api.md
git commit -m "Docs: Encore asks for eight scopes now, so say eight

feature-parity.md deviation #6 and security.md both asserted a three-scope
read-only grant deferred feature by feature. That stopped being true in this
branch, and a document asserting a property the code no longer has is worse
than no document at all.

The claim that survives intact is the narrower one, and it is the one that
was always doing the work: Encore never holds a grant that can modify a
listener's Spotify account unless they have used a feature that needs it.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
```

---

## Definition of done

- [ ] `gofmt -l`, `go vet`, `staticcheck` all clean.
- [ ] `go test -count=1 ./...` passes.
- [ ] Full integration suite passes against port 5433.
- [ ] `cd web && npm run lint && npm run typecheck && npm run build && npm run test` passes.
- [ ] A signed-in account with the old three-scope grant sees the banner; one with all eight does not.
- [ ] A 403 does not move an account to `needs_reauth` and is not retried.
- [ ] No migration added. `go.mod` and `web/package.json` unchanged.
- [ ] No feature endpoint was added — this plan ships consent and nothing that consumes it.
