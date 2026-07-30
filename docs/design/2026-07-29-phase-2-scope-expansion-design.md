# Phase 2 — Scope expansion and read-only Spotify features

**Date:** 2026-07-29
**Status:** Design / plan of record
**Context:** [Overview and phase map](2026-07-29-spotify-api-expansion-overview.md)
**Depends on:** Phase 1 merged (shares the coverage-reporting convention, not its code)

Four features that each need something from the listener's Spotify grant. They are grouped because
they share one consent migration, and paying that cost once is the entire reason this is a phase.

---

## 1. The consent migration lands first, alone

**This is a separate commit, merged and observed before any feature depends on it.**

`internal/config/config.go:398` returns the sign-in scope set. It grows from three to eight:

```go
user-read-recently-played  user-read-private  user-read-email
user-top-read  user-library-read  user-follow-read
playlist-read-private  user-read-playback-state
```

`user-read-playback-state` is included here even though only Phase 3 uses it, so that the
re-consent happens once rather than twice.

### 1.1 What this reverses

`docs/feature-parity.md` deviation #6 and `docs/security.md:154` both currently state that Encore
asks for three read scopes at sign-in and defers everything else to the point of use. After this
commit that is false. Both are rewritten **in the same commit**, not afterwards. The revised
position is honest about the trade: read scopes are granted at sign-in because five separate consent
interruptions is worse than one, and every write scope is still deferred to the moment it is used.

The narrower claim that survives intact, and should be stated as the one that mattered: **Encore
never holds a grant that can modify a listener's Spotify account unless they have used a feature
that needs it.**

### 1.2 Existing users

A refresh token minted before this change carries the old grant forever. Spotify answers 403 for
anything needing a new scope.

Both halves of the fix exist already: `spotify.HasScope` compares granted against required, and
`GET /api/auth/spotify/relink` re-authorises without detaching the identity — and already refuses
to attach a *different* Spotify identity to an existing account, which is the failure mode worth
guarding.

The new behaviour is a **dismissible banner** shown when the stored grant is missing scopes, linking
to relink. Never a hard block. An account that dismisses it forever keeps working exactly as it does
today, minus the features it has not granted. Each Phase 2 view independently renders a
"needs permission" empty state rather than an error, because a user may relink at any time or never.

### 1.3 Failure handling

A 403 from any endpoint in this phase is classified as **missing scope, not transient**. It must not
enter the retry-with-backoff path — retrying a scope failure spends quota to fail identically. It
marks the feature unavailable for that user and surfaces the relink prompt.

---

## 2. Feature 3 — the `/me/top` diff

`GET /v1/me/top/{type}` where type is `artists` or `tracks`, with `time_range` of `short_term`
(~4 weeks), `medium_term` (~6 months) or `long_term` (~1 year), offset-paginated, 50 maximum.

Six calls fill the whole picture: two types × three ranges.

### 2.1 Storage

```sql
CREATE TABLE spotify_top_snapshots (
    user_id     uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    kind        text NOT NULL CHECK (kind IN ('artist', 'track')),
    time_range  text NOT NULL CHECK (time_range IN ('short_term', 'medium_term', 'long_term')),
    position    integer NOT NULL,
    entity_id   text NOT NULL,
    captured_at timestamptz NOT NULL,
    PRIMARY KEY (user_id, kind, time_range, position)
);
```

Only the **latest** capture per `(user, kind, time_range)` is retained; a refresh replaces the set
in one transaction. **Refreshed on the daily library worker tick, not on demand** — this diverges
from the on-demand, six-hour-staleness refresh originally planned here, for the same reason §3.1
rules out an on-demand refresh for the library sync: six sequential calls made synchronously from a
page load would add 1-2 seconds to opening it, and a person opening a page must never trigger that.
`internal/library` already carries the reviewed scope-check, 403-classification and
single-transaction commit machinery a second on-demand path would have had to duplicate, so the six
top-item calls run inside that same daily worker pass instead, alongside the library enumeration.
Spotify itself computes these rankings over four weeks to a year, so a capture that is up to a day
old is ample freshness for a number that already moves that slowly — the cost, stated plainly on the
page rather than implied away, is that the snapshot shown can be up to a day stale, the same as the
library.

Retaining history — which would enable a "how Spotify's view of you drifted" view — is deliberately
not built. The table is shaped so that adding it later is a primary-key migration rather than a
rewrite, but nothing asked for it yet.

Entity ids not already in the catalogue are minted as `pending` rows, so the diff page can name
things the listener has never played on this instance.

### 2.2 The view

Spotify's rank beside Encore's own rank over the matching window, and the delta. The interesting
rows are the disagreements in both directions.

**The UI must state plainly that Spotify's ranking is opaque and is not a play count.** Spotify
describes it as "calculated affinity"; the time ranges are approximate; it reflects listening across
every device and account session, including any that predates this Encore instance. Without that
sentence every discrepancy reads as an Encore bug, and the whole feature becomes a support burden
instead of an interesting comparison.

---

## 3. Feature 4 — library and follows

| Endpoint | Pagination | Scope |
|---|---|---|
| `GET /v1/me/tracks` | offset, 50/page | `user-library-read` |
| `GET /v1/me/albums` | offset, 50/page | `user-library-read` |
| `GET /v1/me/following?type=artist` | **cursor** (`after`), 50/page | `user-follow-read` |

### 3.1 This is a background job, never on demand

A 5,000-track library is 100 requests. A person opening a page must never trigger that.

A worker job on a configurable interval (`ENCORE_LIBRARY_SYNC_INTERVAL`, default 24h) refreshes each
user's library. There is no delta endpoint, so each run is a **full enumeration reconciled against
the stored set** — insert new, delete absent, in one transaction so a partial run never presents a
half-empty library as fact. A run that fails partway commits nothing and retries on the next tick.

It draws on the background budget, so it queues behind and alongside catalogue enrichment rather
than competing with sign-in.

### 3.2 Storage

```sql
CREATE TABLE user_saved_tracks (
    user_id  uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    track_id text NOT NULL,
    added_at timestamptz,
    PRIMARY KEY (user_id, track_id)
);
-- user_saved_albums  (user_id, album_id, added_at)
-- user_followed_artists (user_id, artist_id)   -- no added_at; Spotify does not report one
```

Deliberately **not** foreign-keyed to the catalogue tables. A saved track may not be in the
catalogue yet, and the reconciliation transaction must not be ordered behind catalogue inserts.

### 3.3 A useful side-effect

Saved and followed items absent from the catalogue are minted as `pending` rows. Enrichment then
resolves them like anything else, so **things you saved but never played become browsable for
free** — names, artwork, genres — feeding Phase 1's genre statistics with no extra call.

### 3.4 The statistics

- Saved but never played, and how long ago they were saved.
- Played heavily but never saved — ranked by plays, the inverse question.
- Followed artists you have not played inside the range.
- Library coverage: what share of listening is of saved material. **Deferred** — Phase 2c-ii shipped
  the first three of these but not this one. See "Deferred from Phase 2c" (§9) below.

The library-sync state itself follows the Phase 1 discipline: `syncedAt` is nullable, and a sync
that has never run renders as "not read yet" rather than as "you have saved nothing" — those are
different facts and the page must not collapse them. That part shipped. What did **not** ship is a
`covered`/`total` pair on the response and a coverage line on the page, in the §4.3 sense that every
other Phase 2 statistic carries — also tracked in §9 below.

---

## 4. Feature 5 — playlist listening context

### 4.1 Sync stops discarding what it already receives

`internal/spotify/models.go:166` parses `PlayHistory.Context` — the playlist, album or artist the
listener was playing from — and nothing persists it.

```sql
ALTER TABLE listens ADD COLUMN context_type text;
ALTER TABLE listens ADD COLUMN context_id   text;
```

Both nullable with no default, so this is a metadata-only operation in PostgreSQL and instant even
on a table with millions of rows. NULL means "not reported", which is deliberately distinct from
"played from nothing", matching the convention the existing context columns already follow.

`GET /v1/me/playlists` (paginated, `playlist-read-private`) populates `user_playlists (user_id,
spotify_id, name, owner_id, total_tracks, snapshot_id, fetched_at)` so the ids can be named.
Refreshed on the same worker tick as the library sync.

### 4.2 The limitation, stated prominently

**Only `source = 0` rows can ever carry context.** No export format includes it. For an instance
whose history came mostly from an import, this statistic describes a small and unrepresentative
slice — and on a brand-new instance it describes nothing at all, growing only as live sync
accumulates.

The view therefore leads with its denominator: "based on the 3.1% of your listening Encore recorded
live". This is the same discipline as Phase 1, applied to a case where the covered fraction starts
at zero and may stay small for months. If that sentence makes the feature look thin, that is
accurate information about the feature.

---

## 5. Feature 6 — album and discography completion

This splits into a free half and a paid half, and they ship in that order.

### 5.1 Free: album completion needs no Spotify call

`albums.total_tracks` is **already stored** (`migrations/00003_catalog.sql:34`) and already surfaced
at `internal/httpapi/dto.go:180`. So:

```
completion = count(distinct l.track_id) / albums.total_tracks
```

"You have heard 9 of 12 tracks on this album" is pure SQL over data on disk today. It works on every
instance immediately, needs no scope, no call and no new table. It goes on the album detail page and
aggregates into "you have heard every track on 34 albums".

Coverage caveat: `total_tracks` is 0 for albums enrichment has not resolved, and those are excluded
from both numerator and denominator rather than counted as incomplete.

### 5.2 Paid: which tracks are missing, and discography coverage

Naming the *missing* tracks needs `GET /v1/albums/{id}/tracks`. Discography coverage — "you have
heard 4 of this artist's 11 albums" — needs `GET /v1/artists/{id}/albums`, because no stored field
counts an artist's releases.

Both are **fetched lazily when the relevant detail page is first opened**, and cached:

```sql
-- album_tracks   (album_id, track_id, disc_number, track_number, fetched_at)
-- artist_albums  (artist_id, album_id, album_group, position, fetched_at)
```

with a TTL after which the next page view refreshes them. A background sweep is explicitly rejected:
most artists in a large history are never viewed, and enumerating every discography would spend the
instance's quota on questions nobody asked. Both use the app token, so neither needs a user scope.

`GET /artists/{id}/albums` is paginated and `album_group` distinguishes albums from singles,
compilations and appearances. Completion counts `album` and excludes the rest by default, because
"you have heard 4 of 340 releases" is not a useful sentence.

---

## 6. Share links

**Decided, not yet built.** The design intent — recorded in the overview §4.4 as an explicit choice
by the project's owner — is that album completion and the library and follows counts appear on a
share, while the `/me/top` diff and playlist context do not: the first is a comparison against a
third party's opaque model of somebody, and the second describes when Encore was watching.

At head, neither has been added. `internal/httpapi/share.go`'s `handleSharedStats` composes a fixed
set of aggregates that includes neither album completion nor the library and follows counts, and
`SharedStatsResponse` carries no field for either. Album completion has been outstanding since the
Phase 2b branch merged; the library and follows counts became outstanding when the Phase 2c-ii branch
shipped the `/library` page without touching share links. Neither absence is a reversal of the
decision above — both are "not yet built", tracked in "Deferred from Phase 2c" (§9) below.

Once built, this is fixed in `handleSharedStats` and adds no field to `ShareLink`, per the boundary
described in §4.4 of the overview.

---

## 7. Testing

| Test | Asserts |
|---|---|
| Scope banner | A user whose stored grant lacks a scope sees the prompt; one who has it does not |
| 403 classification | A scope 403 does not enter retry-with-backoff and does not mark the account `needs_reauth` |
| Library reconciliation | An item removed on Spotify disappears locally; a partial failure commits nothing |
| Catalogue minting | A saved track absent from the catalogue arrives as `pending` and enriches normally |
| Cursor pagination | `/me/following` follows `after` to exhaustion and terminates on a repeated cursor, as `RecentlyPlayed` already does |
| Snapshot replacement | Refreshing a top snapshot replaces the set atomically, never interleaving two captures |
| Context persistence | A synced listen with a playlist context stores it; an imported listen stores NULL |
| Completion arithmetic | An album with `total_tracks = 0` is excluded from completion, not reported as 0% |
| Relink identity | Relinking with a different Spotify account is still refused |

---

## 8. Out of this phase

- **Writes of any kind.** Every endpoint here is a read. `user-library-modify` and
  `user-follow-modify` exist and are not requested: Encore does not save or follow on somebody's
  behalf.
- **Retaining top-snapshot history**, §2.1.
- **Background discography enumeration**, §5.2.

---

## 9. Deferred from Phase 2c

Phase 2c-i (library ingestion) and Phase 2c-ii (library statistics and the `/library` page) shipped
against this design, but four things it called for are not built, and Phase 2c-ii's own whole-branch
review found two further behavioural gaps. Recorded together here so the follow-up work has one place
to look, rather than each being rediscovered later as a silent omission. None of these is a decision
reversed — each is either "planned, not yet built" or "a real bug, deferred pending a design call":

1. **Library coverage** (§3.4) — "what share of listening is of saved material" was never built.
   Nothing in `LibraryStatsResponse` computes it, and the `/library` page has no such line.
2. **Per-figure coverage denominators on the library statistics** (§3.4, §4.3 of the overview) — the
   shipped response carries no `covered`/`total` pair and the page states no coverage fraction for
   any of its three lists. This is separate from the `syncedAt`-null-vs-empty handling, which did
   ship correctly.
3. **Library and follows counts on share links** (§6; §4.4 of the overview) — decided, not built.
   `handleSharedStats` (`internal/httpapi/share.go`) composes a fixed set of aggregates that does not
   include them.
4. **Album completion on share links** (§6; §4.4 of the overview) — also decided, not built. This gap
   predates Phase 2c: it has been outstanding since the Phase 2b branch merged.

Two more, found in the same review, are code behaviour rather than documentation drift. Both are
real and both need a design decision rather than a one-line patch, so they are carried forward
unchanged rather than fixed here:

- **`dormantFollows` inverts on missing enrichment.** A followed artist whose plays are all on
  tracks that enrichment has not yet credited has no `track_artists` rows. It falls through both
  `NOT EXISTS` predicates used to build the list and sorts *first*, as "Never played" — the opposite
  of what "dormant" is supposed to mean for that artist. Needs a decision between surfacing an
  enrichment-coverage line and excluding such artists outright.
- **`playedNeverSaved` ignores `user_saved_albums`.** Saving an album does not add its tracks to
  `/v1/me/tracks`, so someone who saves albums rather than individual tracks sees those tracks listed
  as "not in your saved library" even though the containing album is saved. Needs a decision between
  narrowing the panel's copy and making the predicate album-aware.
