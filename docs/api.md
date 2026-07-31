# HTTP API reference

This is the contract between `internal/httpapi` and `web/`. The TypeScript mirror of every payload
lives in [`web/src/lib/types.ts`](../web/src/lib/types.ts) and is kept in step by hand — if you change
one, change the other.

## Conventions

- All application endpoints are under `/api`. Operational endpoints (`/healthz`, `/readyz`,
  `/metrics`) are not, so they can be scraped and probed without touching the API surface.
- JSON in, JSON out. Field names are `camelCase`. Timestamps are RFC 3339 with a `Z` offset.
  Durations are milliseconds as integers, named `msPlayed` or `durationMs`.
- Authentication is the `encore_session` cookie. No bearer tokens, no API keys.
- Every `POST`, `PUT`, `PATCH` and `DELETE` requires the `X-CSRF-Token` header to match the
  `encore_csrf` cookie.
- Date ranges are `?from=<RFC3339>&to=<RFC3339>`, half-open `[from, to)`. Omitting both gives the
  trailing 30 days in the user's timezone. Bucketing always happens in the user's own timezone.
- Pagination is `?limit=&offset=` except for the listening history, which is keyset paginated with an
  opaque `?cursor=`.

### Errors

```json
{ "error": { "code": "not_found", "message": "That import job does not exist." } }
```

| Code | Status | Meaning |
|---|---|---|
| `invalid_request` | 400 | Malformed or out-of-range parameters. `details` may name the field. |
| `unauthenticated` | 401 | No session, or an expired one. |
| `csrf` | 403 | Missing or mismatched CSRF token. |
| `forbidden` | 403 | Authenticated but not permitted. |
| `not_found` | 404 | Includes objects that exist but belong to someone else. |
| `conflict` | 409 | State conflict, e.g. retrying a job that is already running. |
| `payload_too_large` | 413 | Upload exceeded `ENCORE_IMPORT_MAX_UPLOAD_BYTES`. |
| `registrations_disabled` | 403 | An unknown Spotify identity signed in while registration is closed. |
| `account_disabled` | 403 | The account has been deactivated by an administrator. |
| `rate_limited` | 429 | Too many requests. |
| `internal` | 500 | Something unexpected. The message is deliberately vague; the log has detail. |

Error messages never contain tokens, credentials, connection strings or SQL.

---

## Authentication

| Method | Path | Description |
|---|---|---|
| `GET` | `/api/auth/spotify/login` | Starts the OAuth flow. `?redirect_to=` is validated against `ENCORE_WEB_URL` and ignored if it points anywhere else. Responds `302` to Spotify. |
| `GET` | `/api/auth/spotify/callback` | Consumes the single-use `state`, exchanges the code, creates or finds the user, sets the session cookie, and redirects into the web client. Errors redirect to `${ENCORE_WEB_URL}/login?error=<code>` rather than rendering an API error page. |
| `GET` | `/api/auth/spotify/relink` | Re-runs OAuth for the signed-in user, for when a refresh token has been revoked. Refuses to attach a different Spotify identity to an existing account. |
| `POST` | `/api/auth/logout` | Deletes the session row and clears the cookie. |

## Session and account

### `GET /api/me`

The bootstrap call the client makes on load. Returns `401` when signed out.

```json
{
  "user": {
    "id": "0f1c…", "spotifyUserId": "someone", "displayName": "Someone",
    "email": "someone@example.com", "avatarUrl": "https://i.scdn.co/…",
    "role": "admin", "isActive": true, "timezone": "Europe/Berlin",
    "createdAt": "2026-01-04T10:00:00Z", "lastLoginAt": "2026-07-26T08:12:00Z"
  },
  "spotify": {
    "connected": true, "syncState": "ok",
    "lastSyncAt": "2026-07-26T08:11:03Z", "lastSyncError": "",
    "scopes": ["user-read-recently-played", "user-read-private", "user-read-email",
      "user-top-read", "user-library-read", "user-follow-read",
      "playlist-read-private", "user-read-playback-state"],
    "missingScopes": []
  },
  "csrfToken": "…",
  "instance": { "registrationsEnabled": false, "version": "1.0.0" },
  "listening": {
    "firstListenAt": "2019-03-04T12:00:00Z", "lastListenAt": "2026-07-26T09:00:00Z"
  }
}
```

`missingScopes` is what the account's stored grant lacks against the scopes Encore currently
asks for at sign-in, computed server-side rather than in the client so the two copies of the
required list cannot drift. Empty means the grant is current. An account with no Spotify
connection at all reports `[]`, not the full scope list — that state is `connected: false`, not a
scope shortfall.

| Method | Path | Description |
|---|---|---|
| `PATCH` | `/api/me` | `{ "timezone": "Europe/Berlin" }`. Changing it marks the user's rollups dirty so statistics re-bucket. |
| `DELETE` | `/api/me` | Hard-deletes the account and all its data. Body must be `{ "confirm": "<spotifyUserId>" }`. |
| `GET` | `/api/me/export?format=json\|csv` | Streams the caller's full listening history. Chunked, never buffered. |
| `POST` | `/api/sync/now` | Triggers an immediate recently-played poll and reports what it found: `{ "fetched", "imported", "duplicates", "skipped", "newestAt" }`. `409` if one is already running, if the account is not connected, if the grant needs re-authorising, or while Spotify is rate limiting the instance — that last one is refused immediately rather than queued behind a pause that can last most of a day. Mostly-duplicate results are normal — Spotify's feed only reaches back fifty plays — so the UI should present "nothing new" as a result rather than a failure. |

## Users and administration

| Method | Path | Description |
|---|---|---|
| `GET` | `/api/users` | Other users on the instance — id and display name only. Powers the comparison page. |
| `GET` | `/api/admin/settings` | `{ "registrationsEnabled": true }`. Admin only. |
| `PATCH` | `/api/admin/settings` | `{ "registrationsEnabled": false }`. Admin only. |
| `GET` | `/api/admin/users?limit=&offset=` | Full user list with listen counts and sync state. Admin only. |
| `PATCH` | `/api/admin/users/{id}` | `{ "role": "user", "isActive": true }`. Refuses to demote or deactivate the last administrator. Admin only. |
| `DELETE` | `/api/admin/users/{id}` | Refuses to delete the last administrator. Admin only. |

## Statistics

All accept `from` and `to`.

| Method | Path | Description |
|---|---|---|
| `GET` | `/api/stats/summary` | Listens, distinct tracks/artists/albums, total ms, active days, first and last listen. |
| `GET` | `/api/stats/top/tracks` | `?limit=&offset=`. Entries carry the resolved entity, `plays`, `msPlayed`, `rank`, and `previousRank` (`null` if absent last period). |
| `GET` | `/api/stats/top/artists` | As above. |
| `GET` | `/api/stats/top/albums` | As above. |
| `GET` | `/api/stats/timeline` | `?interval=hour\|day\|week\|month\|year`. Omitting it picks the finest interval under the bucket cap. Empty buckets are present with zeroes. |
| `GET` | `/api/stats/repartition/hour` | 24 buckets, local hour of day. |
| `GET` | `/api/stats/repartition/weekday` | 7 buckets, Monday first. |
| `GET` | `/api/stats/repartition/heatmap` | 7 × 24 grid. |
| `GET` | `/api/stats/sessions` | `?limit=`. Longest listening sessions with their track lists. |
| `GET` | `/api/stats/discovery` | `?interval=`. Artists and tracks heard for the first time ever, per bucket. |
| `GET` | `/api/stats/streaks` | Current and longest runs of consecutive listening days. |
| `GET` | `/api/stats/compare` | `?aFrom=&aTo=&bFrom=&bTo=`. Two summaries plus deltas. |
| `GET` | `/api/stats/year-in-review?year=2026` | Wrapped-style yearly summary. |
| `GET` | `/api/stats/extras` | Different artists per period, average album release year, average artists per track, albums completed in the range. |
| `GET` | `/api/stats/affinity/{userId}` | Shared artists, albums and tracks with another user, and a similarity score. |
| `GET` | `/api/stats/genres` | `?limit=&offset=`. Ranked genres of the artists behind the range's listening, with each genre's `plays` and `msPlayed`. Genre plays sum to more than the range's total plays, because a track counts toward every genre of every credited artist. Carries `coverage`: how many of the range's listens resolved to at least one genred artist. |
| `GET` | `/api/stats/genres/timeline` | `?interval=&genre=`, with `genre` **repeated** (`?genre=rock&genre=jazz`), never comma-joined. Buckets the named genres across the range — one point per bucket per genre, zeroes included. Omitting `genre` charts the range's current top eight; at most **eight** genres may be requested in one call, matching what a stacked area chart can still show as distinct series. |
| `GET` | `/api/stats/taste` | `{ "obscurity": <rate>, "releaseLag": <rate> }`. Obscurity is the play-weighted mean of Spotify's own artist popularity, 0–100, where **higher means more mainstream**. `releaseLag` is the play-weighted mean gap, in years, between an album's release and the play. |
| `GET` | `/api/stats/context` | How the range was listened to, not what. `endReasons` (why a track stopped), `skipRate` (`reason_end = 'fwdbtn'` — going back is deliberately not counted as a skip), `shuffleRate`, `platforms`, `countries`, `offlineRate`, `incognitoRate`, and `playlists`/`playlistCoverage` — what the range was listened **from**. See "Playback context: what you were playing from" below. |
| `GET` | `/api/stats/library` | `?limit=`, default 10. The last enumeration's snapshot of the account's saved and followed Spotify library, crossed against what has actually been played. See "Library" below — its three lists are deliberately scoped three different ways, and the endpoint is a snapshot, not a live query. |
| `GET` | `/api/stats/top-diff` | `?kind=track\|artist&range=short_term\|medium_term\|long_term`, both required. Spotify's own top ranking against Encore's, for the same window. See "Top diff" below — it takes no `from`/`to` at all. |

Genre, taste and playback-context statistics are partial by construction — genres exist only where
enrichment has resolved the credited artist, and the playback-context columns exist only on rows an
extended-history import wrote. `playlists`/`playlistCoverage` is the one figure on this whole page
with the **reversed** lineage: it exists only on rows Encore's own live sync wrote, and no import of
either format ever carries it — see "Playback context: what you were playing from" below. Every
figure above therefore ships its own denominator, in one of two shapes:

```json
{ "covered": 812, "total": 900 }
{ "value": 42.7, "covered": 812, "total": 900 }
```

`total` is the range's relevant row count and `covered` is how many of those rows the statistic could
actually compute from. A ranking or breakdown carries the pair as a `coverage`/`*Coverage` field
alongside its list; a single figure carries it inline next to `value`. Nothing divides a partial
count by the whole range and calls the result complete.

### Album completion

`/api/albums/{id}` and `/api/stats/extras` both report album completion, and they deliberately
answer different questions.

`completion` on the album payload is **all-time**: how much of this one album the caller has ever
heard, ignoring `from`/`to` entirely, because completion is a property of a listening lifetime, not
of whichever window the page happens to be showing.

```json
{ "heard": 9, "total": 12, "known": true }
```

`albumsCompleted` on `/api/stats/extras` is the ordinary **range-scoped** aggregate: of the albums
played inside `from`/`to`, how many were heard in full.

```json
{ "complete": 41, "albums": 63 }
```

The two are expected to disagree. A listener who finished an album years before the selected range
still reports `completion` as complete (`known: true`, `heard` at least `total`), while
`albumsCompleted` only credits that album if the plays that completed it fall inside the range.

`known` is false when the album's `total_tracks` is still 0 — enrichment has not resolved the track
count yet, not that the album has no tracks. A client renders "track count not known yet" rather
than a ratio or `0%` in that case, and `albumsCompleted` excludes such albums from both `complete`
and `albums` for the same reason: crediting or penalising a listener for a number Spotify has not
supplied yet would not describe their listening.

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
| `pending` | No listing yet; one is being read from Spotify now, or is due and about to be. | "Encore is asking Spotify." Poll. |
| `unavailable` | No listing, and none is being read: the last attempt failed. | "Encore could not read the list." Never "you have played everything." |
| `disabled` | No listing, and this instance does not fetch them: `ENCORE_ALBUM_TRACKS_ENABLED=false`. | "This instance does not fetch album track lists." Never blame Spotify, and never promise a retry. |

`missing` is empty in three of the four, so **a client must branch on `state` before it branches on
`missing`.** Only `ready` with `coverage.covered == coverage.total` means the album was played in
full.

`disabled` is deliberately not folded into `unavailable`. "Your operator chose not to ask" and
"Spotify would not answer" are different facts, and a page that renders the second for the first
blames a third party for a local decision.

**`pending` has no server-side bound.** Most ways a fetch declines to start — no free local slot,
another replica already holding the lease, a claim against `album_track_fetches` that itself errors —
leave the row exactly as it was, so the very next request lands back in the same branch. A read-only
replica or a full tablespace can hold an album at `pending` indefinitely; nothing here times it out.
Encore's own web client caps its poll at two minutes and then says plainly that it gave up, rather
than implying either an outcome or a promise to keep trying — reopening the page afterwards costs one
request and shows `ready` if the listing landed in the meantime.

**Turning fetching off does not hide a listing that is already cached.** One stored before the
switch was flipped still arrives as `ready`, past its TTL or not: what was turned off is *fetching*,
not the album page, and withholding a listing that was correct when it was read would be strictly
worse than showing it. `fetchedAt` is what keeps that honest, and it is why there is no separate
"this will never refresh" field — a date says how old an answer is without claiming anything about
how current it is, and a second field expressing one fact is a field that drifts. The web client
renders that date on every `ready` state, which is also the only honesty available on an instance
that has fetching turned off altogether: nothing will ever refresh the date again, so the date is
what a reader has to judge it by.

`coverage.total` is the number of tracks **Spotify returned**, which is not necessarily the album's
`totalTracks`: they come from different reads at different times and can legitimately disagree. The
web client prints both numbers when they do. `total_tracks` is *not* back-filled from this listing —
enrichment owns that column, and an album with `total_tracks = 0` is still excluded from completion
rather than reported as 0%, exactly as before.

A failed fetch is retried after fifteen minutes rather than after the TTL, because failures here are
timeouts and rate limits. A failure never replaces or empties a listing that was read successfully
earlier: the older listing stays readable and `fetchedAt` keeps saying how old it is.

**One known limitation.** The listing is requested without a market, so the ids are Spotify's
canonical ones. A play recorded under a *relinked* id — the same recording, a different id in a
different market — will not match, and that track appears as never played. Encore does not guess at
equivalences here.

The album must already be in your catalogue; an id that is not answers 404 without touching Spotify.

### Playback context: what you were playing from

`playlists` and `playlistCoverage`, part of `/api/stats/context`'s payload, answer what the range
was listened **from** — a playlist, an album, an artist, or "collection" (Spotify's own encoding
for Liked Songs) — as opposed to everything else on that endpoint, which answers *how*.

```json
{
  "playlists": [
    { "contextType": "playlist", "contextId": "37i9dQZF1E39…", "name": "Evening commute", "plays": 41 },
    { "contextType": "collection", "contextId": "", "name": "", "plays": 18 },
    { "contextType": "playlist", "contextId": "1a2b3c…", "name": "", "plays": 3 }
  ],
  "playlistCoverage": { "covered": 62, "total": 900 }
}
```

**This carries the narrowest coverage of anything this endpoint returns, and for a different reason
than the other six figures.** `endReasons`, the two rates, `platforms` and `countries` are partial
because an extended-history import may omit an individual column; `playlists` is partial because
**`context_type` and `context_id` are written only by Encore's own live sync of the recently-played
feed, and no Spotify export — account-data or extended, of any vintage — ever records what a play
came from at all.** A fresh instance with nothing synced live yet reports `playlistCoverage` as
`{ "covered": 0, "total": <n> }`; an instance built mostly from an import reports a small, honest
slice that grows only as live sync accumulates more history alongside the import. This is expected,
not a bug to chase, and the single most common question this feature will raise: **imported history
can never contribute to this statistic, no matter which export format or how much of it there is.**

Each entry's `name` is resolved against the listener's own enumerated playlists
(`user_playlists`, populated by the daily library-sync worker — see
[`docs/configuration.md`](configuration.md) and "Library" below) and is empty
whenever that lookup finds nothing. That happens for three distinct, all-ordinary reasons, and an
empty name is never itself an error:

- **The context isn't a playlist at all.** `album`, `artist` and `collection` contexts are never in
  `user_playlists` — nothing will ever name them, by construction, not because a lookup failed.
- **The playlist enumeration hasn't caught up yet.** `user_playlists` is a once-a-day snapshot, the
  same staleness contract as the library and top-diff snapshots below: a playlist created an hour
  ago has no row yet and shows unnamed until the worker's next run.
- **The id no longer resolves to one of the listener's own playlists.** Deleted since, or a
  playlist that was never theirs to enumerate in the first place (someone else's, played via a
  shared link). The row still counts — dropping it would understate the total `playlistCoverage`
  promises — it is just permanently unnamed rather than temporarily so.

`contextId` is a bare Spotify id, never a track, album or artist id resolved through the catalogue,
so it is not a candidate for a client's usual entity-linking logic. A `collection` context, and any
context whose URI Encore could not parse into `spotify:<type>:<id>`, carries `contextId: ""`.

### Library

`GET /api/stats/library` answers what the account has saved and followed on Spotify, crossed against
what it has actually played. Reading it requires the `user-library-read` and `user-follow-read`
scopes; an account that connected before Encore asked for them answers with those two scopes present
in `missingScopes` (see `GET /api/me`), and the client is expected to offer relink rather than render
an empty library.

**This is a snapshot, not a live query.** Encore never asks Spotify for the library on request. A
background worker enumerates saved tracks, saved albums and followed artists once a day
(`ENCORE_LIBRARY_SYNC_INTERVAL`, default `24h`; see [`docs/configuration.md`](configuration.md)) and
reconciles the result into `user_saved_tracks`, `user_saved_albums` and `user_followed_artists`. A
track saved a minute ago will not appear here until that worker's next run.

`syncedAt` is nullable, and **null means "never enumerated", not "nothing saved"**. Every account is
`null` until the library worker's first successful run for it — including every account that existed
before this feature shipped — and a client must never substitute a zero timestamp for it or infer
empty counts from it.

The three lists are deliberately scoped in three different ways:

| List | Scope |
|---|---|
| `savedNeverPlayed` | **All time**, and does not move when `from`/`to` change. "Never played" means never; scoping it to the requested range would list every saved track the listener simply did not happen to play in that window, a different and far less useful question. |
| `playedNeverSaved` | **Range-scoped**, like every other ranked list in the API: tracks played inside `from`/`to` that are not in the saved library. |
| `dormantFollows` | **Range-scoped**: followed artists with no play inside `from`/`to`. Its `lastPlayedAt` is still the artist's all-time last play regardless of the range, so a client can say how long a follow has actually been dormant rather than only that it was quiet in the current window. |

`syncedAt` and the three counts (`savedTracks`, `savedAlbums`, `followedArtists`) describe neither
history nor a range — they describe the last enumeration itself.

Each list entry wraps its resolved entity under an `entity` key, the same convention `TopEntry` and
`AffinityEntry` use elsewhere on this page:

```json
{
  "syncedAt": "2026-07-29T04:00:00Z",
  "savedTracks": 812, "savedAlbums": 64, "followedArtists": 210,
  "savedNeverPlayed": [
    { "entity": { "id": "…", "name": "…", "artists": […], "album": {…} },
      "addedAt": "2026-06-01T00:00:00Z" }
  ],
  "playedNeverSaved": [
    { "entity": { "id": "…", "name": "…", "artists": […], "album": {…} },
      "plays": 14, "msPlayed": 3200000 }
  ],
  "dormantFollows": [
    { "entity": { "id": "…", "name": "…" }, "lastPlayedAt": "2025-11-02T00:00:00Z" }
  ]
}
```

`addedAt` and `lastPlayedAt` are themselves nullable independently of `syncedAt`: `addedAt` is null
when Spotify did not report it, or the track was saved before Encore recorded that field; a dormant
follow's `lastPlayedAt` is null when the artist has never been played at all.

The blacklist applies here too: a blacklisted artist never appears in `dormantFollows`, and none of
its plays count toward whether a followed artist looks dormant or a saved track looks played.

### Top diff

`GET /api/stats/top-diff?kind=track|artist&range=short_term|medium_term|long_term` compares
Spotify's own top ranking of the account's tracks or artists against Encore's ranking of the same
entities, over the matching window. Both parameters are required and are validated against these
exact sets — anything else, including an omitted one, is `400`.

**Spotify's ranking is opaque.** Spotify calls it "calculated affinity"; it is not a play count, its
time ranges are approximate, and it is computed over the account's whole Spotify listening history,
including everywhere else the account has ever been used and everything before this instance
existed. Disagreement between the two columns is the expected, ordinary case, not a bug in either
side.

`range` addresses Spotify's own rolling window, not the `from`/`to` pair every other statistic
takes — this endpoint accepts no range of its own at all. Encore's side is computed over the
matching approximate window (`short_term` ~ the last 4 weeks, `medium_term` ~ 6 months, `long_term`
~ 12 months; see `topDiffWindow` in `internal/stats/topdiff.go`), because comparing Spotify's ranking
against some other window the caller happened to pick would make the two sides answers to different
questions rather than a real disagreement about the same one.

Reading it needs the `user-top-read` scope; an account that connected before Encore asked for it
answers with that scope present in `missingScopes` (see `GET /api/me`), and the client is expected to
offer relink rather than render an empty comparison.

**This reads a snapshot, not a live query.** The same daily background worker that enumerates the
library (`ENCORE_LIBRARY_SYNC_INTERVAL`, default `24h`; see
[`docs/configuration.md`](configuration.md)) also captures Spotify's own top artists and tracks
across all three time ranges, and this endpoint only ever reads that capture back. `capturedAt` is
nullable, and **null means "never captured", not "captured and empty"** — every `(kind, range)` set
is `null` until that worker's first successful run for it, including every account that existed
before this feature shipped. A client must never substitute a zero timestamp for it. Once captured,
the snapshot is up to a day stale by design, the same as the library.

```json
{
  "capturedAt": "2026-07-29T04:00:00Z",
  "timeRange": "short_term",
  "entries": [
    { "entity": { "id": "…", "name": "…" }, "spotifyRank": 2, "encoreRank": 1, "plays": 41 },
    { "entity": { "id": "…", "name": "…" }, "spotifyRank": 1, "encoreRank": null, "plays": 0 },
    { "entity": { "id": "…", "name": "…" }, "spotifyRank": null, "encoreRank": 2, "plays": 12 }
  ]
}
```

Each list entry wraps its resolved entity under an `entity` key, the same convention `TopEntry`,
`AffinityEntry` and the library lists above all use. `spotifyRank` and `encoreRank` are **null when
the entity is absent from that side, never zero** — zero would read as tied for last place rather
than missing, and an entity present on only one side is precisely the disagreement this statistic
exists to surface. `plays` is Encore's own play count for the window and is meaningless when
`encoreRank` is null: Spotify's side of this comparison carries no play count at all, only a rank.

A blacklisted artist — or, for `kind=track`, a track credited to one — is removed from **both**
sides before ranking, with the surviving ranks on each side closed up to fill the gap, rather than
kept at their original position with a hole where the blacklisted entry used to be. The two lists
this endpoint compares are therefore exhaustive only *after* the blacklist is applied, the same as
every other ranked list in this API.

## Entities

| Method | Path | Description |
|---|---|---|
| `GET` | `/api/tracks/{id}` | Track, its album and artists, plus the caller's stats for it over the range. |
| `GET` | `/api/artists/{id}` | Artist, the caller's stats, top tracks, top albums, day repartition, first and last listen, share of total listening. |
| `GET` | `/api/albums/{id}` | Album, its artists and tracks, plus the caller's stats and how much of it has ever been heard. |
| `GET` | `/api/albums/{id}/tracklist` | Which tracks of the album have never been played, from Spotify's own track list. Cached, lazily filled, and never blocking. |
| `GET` | `/api/search?q=&limit=` | Catalogue search across artists, albums and tracks. |

## Listening history

`GET /api/history?from=&to=&cursor=&limit=`

Keyset paginated on `(playedAt, id)`, never `OFFSET`, because a user may have millions of rows.

```json
{
  "items": [
    { "id": "918273", "playedAt": "2026-07-26T07:58:11Z", "msPlayed": 214000,
      "source": "extended", "track": { "id": "4uLU…", "name": "…", "artists": […], "album": {…} } }
  ],
  "nextCursor": "eyJ0IjoiMjAyNi0wNy0yNlQwNzo1ODoxMVoiLCJpIjo5MTgyNzN9",
  "hasMore": true
}
```

`nextCursor` is opaque. Pass it back verbatim; do not construct one.

## Artist blacklist

| Method | Path | Description |
|---|---|---|
| `GET` | `/api/blacklist` | Artists excluded from every statistic for the caller. |
| `POST` | `/api/blacklist` | `{ "artistId": "…" }`. |
| `DELETE` | `/api/blacklist/{artistId}` | |

## Imports

### `POST /api/imports`

`multipart/form-data`, one or more `files` parts plus an optional `note`. Accepts `.json`, `.json.gz`
and `.zip`; a zip is expanded and each streaming-history entry becomes its own file in the job.
Bodies are streamed to durable storage, never buffered.

Responds `202 Accepted` with the created job. A file whose SHA-256 matches one already imported by
this user is still accepted — re-importing is idempotent — but is flagged so the UI can warn.

```json
{
  "job": { "id": "…", "status": "queued", "filesTotal": 12, "filesDone": 0,
           "counters": { "imported": 0, "duplicates": 0, "skipped": 0, "rejected": 0 },
           "files": [ … ] },
  "warnings": [ { "file": "Streaming_History_Audio_2015-2017_0.json",
                  "code": "already_imported",
                  "message": "Imported before on 2026-06-02. Re-importing is safe and adds nothing." } ]
}
```

### The rest

| Method | Path | Description |
|---|---|---|
| `GET` | `/api/imports?limit=&offset=` | The caller's jobs, newest first. |
| `GET` | `/api/imports/{id}` | One job with per-file progress. Poll this for live updates. |
| `POST` | `/api/imports/{id}/cancel` | Stops at the next batch boundary; committed records are kept. |
| `POST` | `/api/imports/{id}/retry` | Resumes a failed, cancelled or paused job **from its checkpoint**. `409` if the job is not resumable. |
| `DELETE` | `/api/imports/{id}` | Removes the job and its uploaded files. **Listening records are kept**; their `importFileId` becomes null. |
| `GET` | `/api/imports/{id}/rejects?limit=&offset=` | Malformed records with record index, reason code, detail and a truncated excerpt. |

### Job payload

```json
{
  "id": "…", "status": "running", "note": "full export june 2026",
  "createdAt": "…", "startedAt": "…", "finishedAt": null,
  "errorCode": "", "errorMessage": "",
  "filesTotal": 12, "filesDone": 7,
  "counters": { "imported": 812934, "duplicates": 41, "skipped": 9022, "rejected": 3 },
  "files": [
    { "id": "…", "name": "Streaming_History_Audio_2015-2017_0.json",
      "containerPath": "Spotify Extended Streaming History/…",
      "format": "extended", "status": "completed", "sizeBytes": 41234567,
      "recordsTotal": 89211, "recordOffset": 89211, "pending": 0,
      "counters": { "imported": 88104, "duplicates": 5, "skipped": 1102, "rejected": 0 },
      "errorCode": "", "errorMessage": "" }
  ]
}
```

`status` is one of `queued`, `running`, `paused`, `completed`, `failed`, `cancelled`. `pending` is
`null` while `recordsTotal` is still unknown — the UI shows "counting" rather than inventing a
denominator.

**A job only reaches `completed` after verification.** See [`docs/import.md`](import.md) §7: the
importer compares its own counters against a real `count(*)` over `listens` per file, and a shortfall
produces `failed` with `errorCode = "verification_failed"` rather than a false success.

---

## Sharing

Read-only links to a user's aggregate statistics.

| Method | Path | Description |
|---|---|---|
| `GET` | `/api/shares` | The caller's links. Never returns a token or a URL: only a hash is stored, so neither can be reconstructed. |
| `POST` | `/api/shares` | `{ "label", "from", "to", "days", "expiresAt" }` — a fixed range (`from`+`to`) **or** a rolling window (`days`), never both; all omitted means everything. Answers `201` with the link **including `token` and `url`, the only time they exist**. Capped at 25 live links per account. |
| `DELETE` | `/api/shares/{id}` | Revokes immediately. Scoped by owner, so another user's id yields `404`. |

### `GET /api/share/{token}`

**Unauthenticated.** The only endpoint in Encore that answers with a user's data
and no session.

It composes a fixed payload — summary, top 25 tracks/artists/albums, genres,
taste, timeline, hour and weekday repartition — using the same shapes the
ordinary statistics endpoints return. There is no listening history in it and
no parameter that could reach one: what a share exposes is a property of the
feature, not a per-link setting.

Genres and taste describe *what* somebody listens to, which a share has always
disclosed in the top lists, so they are included. The playback-context
statistics describe *how and where* — device and country say what hardware
somebody owns and where they have travelled — and are deliberately left out of
every share payload; an end-to-end test asserts their field names never
appear in a shared response.

The range comes from the link, never the query string. Revoked, expired,
belonging to a deactivated user, and never-existed all answer `404` alike.
Responses carry `X-Robots-Tag: noindex, nofollow, noarchive`.

## Playlists

Builds a Spotify playlist from the caller's own listening. Requires the
`playlist-modify-private` scope, which Encore asks for **only when somebody uses
this feature** — the sign-in grant stays read-only for everyone else.

| Method | Path | Description |
|---|---|---|
| `GET` | `/api/auth/spotify/playlists` | Starts an OAuth journey asking for one extra scope. A browser redirect, like relink. |
| `GET` | `/api/playlists` | The caller's managed playlists. |
| `POST` | `/api/playlists` | `{ "name", "mode", "sort", "limit", "minPlays", "from", "to" }`. Creates it on Spotify and fills it in one request. `403` when the scope has not been granted, with a message naming the fix; `400` when the definition matches nothing. |
| `POST` | `/api/playlists/preview` | The same body. Returns the tracks a definition **would** select — ranked, named, with the plays that qualified each — plus `matched` and `limit`. Touches Spotify not at all and **does not require the write scope**: seeing the selection is how somebody decides whether to grant it. |
| `POST` | `/api/playlists/{id}/rebuild` | Re-runs the stored definition and replaces the contents in place, keeping the same Spotify playlist. |
| `DELETE` | `/api/playlists/{id}` | Encore stops managing it. **The playlist stays in the listener's Spotify library.** |

`mode` is one of:

| Mode | Selects |
|---|---|
| `top` | The `limit` most-played tracks in the range. |
| `min_plays` | Every track played at least `minPlays` times in the range — not a top-N. |
| `discoveries` | Tracks whose **first ever** listen falls in the range. |
| `forgotten` | Tracks played heavily *before* the range and not during it. Requires a range. |

`sort` is `plays` (default) or `time`. Omitting `from`/`to` means the whole
history, except for `forgotten`, which has nothing to be absent from without one.

The blacklist applies: a hidden artist's tracks never reach a playlist.

### Entity statistics

Track, artist and album detail responses carry two pairs of timestamps, and they
answer different questions:

| Field | Meaning |
|---|---|
| `firstListenAt`, `lastListenAt` | First and last play **inside the selected range**. |
| `discoveredAt`, `lastPlayedAt` | First and last play **ever**, ignoring the range. |

A figure labelled "first listen" wants the second pair. Reading it from a window
the viewer happened to select makes a track someone has loved for a decade claim
to have been discovered last month.

## Instance status

### `GET /api/status`

How far metadata enrichment has got, and whether anything is stopping it. Any
signed-in user may read it: the payload is instance-wide operational state and
holds nobody's listening data.

```json
{
  "catalogue": {
    "tracks":  { "total": 16503, "resolved": 50, "pending": 16453,
                 "failed": 0, "unavailable": 0, "named": 16503, "local": 0 },
    "artists": { "total": 3482, "resolved": 0, "pending": 0,
                 "failed": 0, "unavailable": 0, "named": 3482, "local": 3482 },
    "albums":  { "total": 8899, "resolved": 0, "pending": 0,
                 "failed": 0, "unavailable": 0, "named": 8899, "local": 8899 },
    "aliasesTotal": 0, "aliasesPending": 0
  },
  "metadata": {
    "outstanding": 16484, "complete": false,
    "paused": true, "pausedUntil": "2026-07-28T04:00:00Z",
    "fallbackConfigured": false
  }
}
```

`named` is reported separately from `resolved` because the two genuinely differ.
An imported export carries track and artist names, so most of the catalogue is
readable long before Spotify has supplied albums, artwork and genres. A client
that measured progress by `resolved` alone would report a screen full of legible
music as empty.

`local` counts rows an import named but could not identify — see §10 of
[import.md](import.md). They are readable but carry no artwork, genres or release dates, and no queue
can fetch them: an artist gains those only when a track of theirs resolves and the merge folds the
two rows together. A client that treated `local` rows as outstanding work would report a permanent
backlog that nothing can drain.

`paused` is the state this endpoint exists for. Spotify answers an exhausted
daily quota with a `Retry-After` of most of a day; Encore records the instant and
waits it out across restarts. `pausedUntil` is absent unless `paused` is true,
and an elapsed window is never reported as a pause. Listening data is unaffected
throughout — every play is already counted.

`fallbackConfigured` reports that a [metadata fallback](metadata-fallback.md) is set up, which
changes what a pause means: enrichment keeps going through a second source rather than stopping. It
is read from the API process's own environment, so it says the deployment is configured for one, not
that the worker has reached it.

Backs the metadata panel on the Settings page. The command-line equivalent,
which additionally reports listens, users, per-user sync state and import job
counts, is `encore-worker status`.

## Operational endpoints

| Method | Path | Description |
|---|---|---|
| `GET` | `/healthz` | Liveness. `200 {"status":"ok"}` whenever the process is running. Never touches the database, so a database outage does not cause a restart loop. |
| `GET` | `/readyz` | Readiness. `200` only when the database answers **and** every embedded migration is applied. `503` with `{"status":"not_ready","checks":{…}}` otherwise. |
| `GET` | `/metrics` | Prometheus text format. Behind basic auth when `ENCORE_METRICS_USERNAME` and `ENCORE_METRICS_PASSWORD` are set. |
