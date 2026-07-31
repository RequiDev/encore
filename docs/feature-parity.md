# Feature parity checklist

Encore's target is practical parity with the user-facing capabilities of
[your_spotify](https://github.com/Yooooomi/your_spotify). This is the item-by-item status.

| Status | Meaning |
|---|---|
| **Implemented** | Present and working, verified by a test or by exercising it against the running stack. |
| **Partially implemented** | Some of it is there. What is missing, and why, is stated. |
| **Changed** | Deliberately done differently. The reason is stated. |
| **Deferred** | Not built. The reason is stated. |

---

## 1. Accounts and authentication

| Capability | Status | Notes |
|---|---|---|
| Sign in with Spotify | **Implemented** | Authorization code flow with PKCE (S256). The reference does not use PKCE. |
| Account created on first sign-in | **Implemented** | The first user ever created becomes the administrator, decided inside the `INSERT` so two simultaneous first sign-ins cannot both win. |
| Sessions and sign-out | **Implemented** | Server-side sessions; only the SHA-256 of the cookie is stored, so a database leak cannot be replayed. |
| "Relog to Spotify" when sync breaks | **Implemented** | `GET /api/auth/spotify/relink`. Refuses to attach a different Spotify identity to an existing account. |
| Multiple users on one instance | **Implemented** | Every query is scoped by user id; the catalogue is shared. |
| Admin can disable new registrations | **Implemented** | `app_settings.registrations_enabled`, toggled from Settings. An unknown identity is then refused; existing users can always sign in. |
| Admin can promote, deactivate and delete users | **Implemented** | Encore additionally refuses to demote, deactivate or delete the last administrator, so an instance cannot lock itself out. |
| Per-user timezone for statistics | **Implemented** | Every bucketed statistic converts with `AT TIME ZONE`. Changing it marks the user's rollups dirty. |
| Local username/password accounts | **Changed** | Not offered. Spotify is the only identity provider, matching the reference. No password database means no credential-stuffing surface and no reset flow. |
| Public/shared statistics links | **Implemented** | Revocable, unguessable links to aggregate statistics, with an optional rolling window and expiry. Deliberately narrower than the reference: a link reaches totals and rankings and has no path to the listening history, because a stranger learning what somebody listens to is a different thing from learning when they were awake. |

## 2. Ingestion

| Capability | Status | Notes |
|---|---|---|
| Poll recently-played tracks | **Implemented** | Every `ENCORE_SYNC_INTERVAL`, jittered. The cursor advances only in the same transaction that commits the listens. |
| Token refresh, single-flight | **Implemented** | An omitted `refresh_token` never clears the stored one. `invalid_grant` marks the account `needs_reauth` rather than retrying forever. |
| Import "Account data" export | **Implemented** | `StreamingHistory*.json`, `StreamingHistory_music_*.json`. |
| Import "Extended streaming history" export | **Implemented** | `Streaming_History_Audio_*.json` and the legacy `endsong_*.json`. |
| Upload the whole `.zip` Spotify sends | **Changed** | The reference asks for individual JSON files. Encore accepts the archive, finds the streaming-history entries and skips playlists, search queries and the read-me. It removes the most common source of import confusion. |
| `.gz` and NDJSON inputs | **Changed** | Accepted although Spotify does not produce them, because people re-compress exports before uploading. |
| Duplicate-safe re-imports | **Implemented** | Enforced by `UNIQUE (user_id, dedupe_key)`, not by application logic. Verified by `TestReimportingTheSameFileAddsNothing`. |
| Overlapping imports of both formats | **Implemented** | Three documented layers plus a relink pass. Verified by `TestOverlappingFormatsConverge`. |
| Podcasts and audiobooks | **Implemented** | Recognised and counted as *skipped*, never as errors. The reference has had repeated bug reports here. |
| 2024-era export field changes | **Implemented** | `ip_addr_decrypted` → `ip_addr`, absent `username`, audiobook fields, null `offline_timestamp`, string `ms_played`, string booleans. Each has a fixture test. |
| Import cache to reduce API calls | **Changed** | The reference caches Spotify lookups during import. Encore makes **no** Spotify calls during import at all; metadata is enriched separately afterwards. |

## 3. Import reliability

| Capability | Status | Notes |
|---|---|---|
| Streaming parse, never whole-file | **Implemented** | Token-driven `json.Decoder`. Memory is O(batch × record), independent of file size. |
| One million records on consumer hardware | **Implemented** | See [`docs/benchmarks.md`](benchmarks.md). |
| Bounded memory with a documented target | **Implemented** | Target under 256 MiB; measured peak heap is far below it. Asserted by `TestLargeSyntheticHistoryStaysWithinMemoryBudget`. |
| Durable checkpoints | **Implemented** | `record_offset` and `byte_offset` written in the same transaction as their batch. |
| Resume after a crash or restart | **Implemented** | Lease expiry plus checkpoint resume; a cleanly stopped job is `paused` and reclaimed immediately. Verified by `TestWorkerRestartResumesFromCheckpoint`. |
| Idempotent and safe to rerun | **Implemented** | Verified by re-running an import after a resumed one and asserting zero new rows. |
| Documented deterministic duplicate strategy | **Implemented** | [`docs/import.md`](import.md) §5. |
| Bounded batches | **Implemented** | `ENCORE_IMPORT_BATCH_SIZE`, default 1000. |
| Database backpressure, no unbounded queue | **Implemented** | The flush is synchronous and the pool is bounded, so a slow database stops the reader rather than growing memory. |
| Enrichment separated from ingestion | **Implemented** | Ingestion writes `pending` catalogue rows and nothing else. |
| Respect rate limits and `Retry-After` | **Implemented** | A 429 pauses every background caller for the stated duration rather than each goroutine backing off separately. Signing in draws on a separate budget that no catalogue 429 pauses, because a background import must never be able to lock somebody out of their own instance. |
| Bounded retries, exponential backoff, jitter | **Implemented** | `internal/retry`, full jitter by default. |
| Transient / permanent / job-level failure classes | **Implemented** | [`docs/import.md`](import.md) §6. |
| Rejected records recorded with diagnostics | **Implemented** | `import_rejects` holds the record index, a stable reason code, a detail and a truncated excerpt. Capped per file. |
| Never report success unless committed | **Implemented** | `domain.VerifyJob` compares the counters against a real `count(*)` over `listens`. Verified by `TestForgedCompleteJobFailsVerification`. |
| Imported / skipped / duplicate / rejected / pending / failed counts | **Implemented** | Per file and summed per job, exposed on the API and the UI. |
| Cancel and resume later | **Implemented** | Cancellation is observed at a batch boundary. Verified by `TestCancelThenRetryResumesFromCheckpoint`. |
| Retry a failed or interrupted import | **Implemented** | Resumes from the checkpoint; counters and offsets are preserved. |
| Import history and progress reporting | **Implemented** | Per-file live progress with counters. |

## 4. Statistics

| Capability | Status | Notes |
|---|---|---|
| Dashboard summary cards | **Implemented** | Listens, distinct tracks/artists/albums, listening time, active days, top artist and track. |
| Top tracks / artists / albums | **Implemented** | Paginated, with the previous period's rank so movement can be shown. |
| Listening time and play count over time | **Implemented** | Hour, day, week, month and year buckets; empty buckets appear as zeroes. |
| Hour-of-day repartition | **Implemented** | In the user's own timezone. |
| Weekday repartition | **Implemented** | Monday first. |
| Hour × weekday heatmap | **Changed** | Not in the reference; a complete 7 × 24 grid is returned. |
| Artist detail page | **Implemented** | Rank, first and last listen, day repartition, top tracks and albums, share of total listening. |
| Album detail page | **Implemented** | |
| Track detail page | **Changed** | The reference has no dedicated track page. |
| Longest listening sessions | **Implemented** | Grouped in SQL with window functions, not in Go. |
| Different artists per period | **Implemented** | |
| Average album release year | **Implemented** | |
| Average artists per track | **Implemented** | |
| Album completion | **Implemented** | "Heard 9 of 12 tracks", from `albums.total_tracks`, which needs no Spotify call. All-time rather than range-scoped, because completion is a property of a listening lifetime. An album whose track count has not been enriched reports that rather than a ratio. Naming *which* tracks are missing is a separate capability — see below. |
| Naming the tracks you have never played | **Implemented** | `GET /api/albums/{id}/tracklist` reads Spotify's own list of what is on the album (`GET /v1/albums/{id}/tracks`) and names which of those tracks have no plays in the caller's history, all time. Fetched lazily the first time somebody opens that album's page and cached for `ENCORE_ALBUM_TRACKS_TTL` (30 days by default) — there is no background sweep, since most albums in a large history are never opened and enumerating them all would spend the instance's quota on questions nobody asked. Costs roughly one Spotify request per album viewed per TTL. Like the discography walk in the row below, it fires as a side effect of *viewing* a page rather than of a click, so it is switchable with `ENCORE_ALBUM_TRACKS_ENABLED`; a listing already cached keeps being shown after the switch is turned off, with the date it was read. Coverage across a whole artist's discography is the row below. |
| How much of an artist you have heard | **Implemented** | `GET /api/artists/{id}/discography` reads Spotify's own list of what an artist released (`GET /v1/artists/{id}/albums`) and says how many of those albums have any play in the caller's history, all time. Fetched lazily the first time somebody opens that artist's page and cached for `ENCORE_ARTIST_ALBUMS_TTL` (7 days by default, shorter than the album track listing's 30 because a discography grows); there is no background sweep, since most artists in a large history are never opened and enumerating every discography would spend the instance's quota on questions nobody asked. Costs `ceil(releases/50)` requests per artist viewed per TTL — one for most artists, up to forty for an unusually prolific one, which is a hard cap rather than a typical case — and it is switchable with `ENCORE_ARTIST_ALBUMS_ENABLED`, a **separate** key from `ENCORE_ALBUM_TRACKS_ENABLED` because the two cost very different amounts. **Completion counts `album_group = "album"` only**, excluding singles, compilations and appearances, because "you have heard 4 of 340 releases" is not a useful sentence — and the page names what it set aside rather than leaving the exclusion silent. An album counts as played when any track from it was played, which the page also says. |
| Listening history feed | **Implemented** | Keyset paginated on `(played_at, id)`, never `OFFSET`. |
| Date-range filtering everywhere | **Implemented** | Half-open `[from, to)` in the URL, so any view is linkable. |
| Artist blacklist | **Implemented** | Excluded from every statistic through one shared SQL fragment. |
| Affinity / comparison between two users | **Implemented** | Shared artists, albums and tracks plus a cosine similarity score. |
| Catalogue search | **Implemented** | Artists, albums and tracks. |
| Playlist creation from statistics | **Implemented** | Four modes: most played, a minimum play count, first-heard-in-period, and forgotten favourites — each over any range, ranked by plays or by listening time. Broader than the reference. Every definition can be previewed first — the exact tracks, ranked, without touching Spotify and without the write scope, which is what lets somebody decide whether to grant it. The scope is requested only when a playlist is actually created, so an account that never makes one keeps a grant confined to the eight read scopes requested at sign-in: `user-read-recently-played`, `user-read-private`, `user-read-email`, `user-top-read`, `user-library-read`, `user-follow-read`, `playlist-read-private` and `user-read-playback-state`. |
| Play button / Spotify remote control | **Deferred** | Needs `user-modify-playback-state` and an active device; same read-only reasoning. |
| Genre statistics | **Implemented** | Top genres, genre timeline and an obscurity score, from `artists.genres` and `artists.popularity`. Not in the reference project. Every figure reports what share of the range's listening it could see, because genres exist only where enrichment has resolved the artist. |
| Playback-context statistics | **Implemented** | Skip rate, shuffle share, how tracks ended, platform and country. Not in the reference project. Only extended-export rows carry these columns, so each figure reports its own denominator. |
| Playlist / album listening context ("what you were playing from") | **Implemented** | Not in the reference project. `listens.context_type`/`context_id` record what a live-synced play came from — a playlist, an album, an artist, or "collection" (Liked Songs) — and are populated **only** by live sync: no export format, account-data or extended, has ever carried this. `GET /api/stats/context` breaks the range down by it with its own `playlistCoverage`, independent of the six export-only figures beside it — a fresh instance reports it as zero, and an import-dominated one reports a small, honest, permanently-partial slice that only grows as sync accumulates. Playlist ids are named from a `user_playlists` table enumerated on the same daily worker tick as the library and top-item snapshots, behind its own scope (`playlist-read-private`, one of the eight requested at sign-in); an unnamed playlist context is a deleted or foreign playlist, not an error, and album/artist/collection contexts are never named at all. The Habits page states the denominator, distinguishes a zero-coverage state from a low one, and separately notes when the naming scope itself is missing without blanking the counts. |
| Library vs. listening history | **Implemented** | Not in the reference project. Crosses what the account has saved and followed on Spotify against what it has actually played: tracks saved but never played (all time), tracks played heavily but never saved, and followed artists gone quiet — each over a range picker for the latter two, never for the first. Needs `user-library-read` and `user-follow-read`; an account that connected before Encore asked for them is told its library is not shared and offered relink, not shown an empty one. Reads a daily background enumeration (`ENCORE_LIBRARY_SYNC_INTERVAL`) rather than Spotify directly, so it is a snapshot up to a day stale by design, and the page says when it was last read. |
| Top diff: Spotify's own ranking vs. Encore's | **Implemented** | Not in the reference project. Compares Spotify's own top artists/tracks ranking against Encore's, for the same window. No range picker: the window is one of Spotify's own `short_term`/`medium_term`/`long_term`, since comparing against a different window would make the two sides answer different questions. The page opens by saying plainly that Spotify's ranking ("calculated affinity") is opaque and covers listening this instance has never seen, so disagreement is the expected case, not a bug — without that the feature would just generate support requests. Needs `user-top-read`; reads the same daily background capture as the library rather than Spotify directly, so `capturedAt` is nullable and up to a day stale by design. A blacklisted artist is removed from both sides with ranks closed up, the same rule every other ranked list in the API follows. |

## 5. Optional enhancements (built after the required scope)

| Capability | Status | Notes |
|---|---|---|
| Prometheus metrics | **Implemented** | `/metrics`, optional basic auth, private registry. |
| Import throughput and queue metrics | **Implemented** | Records by outcome, batch duration, records per second, queue depth, bytes read. |
| Background metadata repair job | **Implemented** | Revisits permanently failed catalogue rows every `ENCORE_ENRICH_REPAIR_INTERVAL`. |
| Data export | **Implemented** | JSON and CSV, streamed. |
| Catalogue from the export alone | **Implemented** | Not in the reference project. Both export formats name the artist and album of every play and identify neither; Encore mints local catalogue rows keyed by normalised name, so artists, albums and every chart work immediately after an import with no Spotify call. Folded into the Spotify rows when enrichment identifies them. See §10 of [import.md](import.md). |
| Optional metadata fallback | **Implemented** | Not in the reference project. A second Spotify-shaped endpoint, consulted while Spotify is rate limiting the instance and for ids Spotify will not serve at all — the only way the terminal `unavailable` state can ever be filled. Off unless configured; Encore ships the interface and no source. See [metadata-fallback.md](metadata-fallback.md). |
| Year in review | **Implemented** | |
| Listening streaks | **Implemented** | Gaps-and-islands over local days. |
| Discovery metrics | **Implemented** | First-ever listens per bucket, not first-in-range. |
| Period comparison | **Implemented** | |
| Privacy controls and account deletion | **Implemented** | Hard delete by foreign-key cascade; not a soft delete. |
| Dark mode | **Implemented** | Light, dark and system, with no flash of the wrong theme on load: the choice is applied by an inline script before first paint. |
| Progressive Web App | **Partially implemented** | Installable — there is a web app manifest, an icon and theme colours, so browsers offer "add to home screen". There is **no service worker and no offline caching**. A listening dashboard has nothing useful to show without its data, and a cache-first service worker is the most common way to strand users on a stale bundle after an upgrade. Adding one would be a deliberate decision with an update strategy, not a checkbox. |

## 6. Operations

| Capability | Status | Notes |
|---|---|---|
| Configuration entirely through environment variables | **Implemented** | [`docs/configuration.md`](configuration.md); parsing reports every problem at once. |
| `.env.example` | **Implemented** | |
| Docker Compose deployment | **Implemented** | Postgres, migrate, api, worker, web, with health-gated ordering. |
| Health and readiness endpoints | **Implemented** | `/healthz` never touches the database; `/readyz` also requires migrations to be current. |
| Structured application logs | **Implemented** | `log/slog`, JSON in production, request-scoped with request ids. |
| Database migrations | **Implemented** | Forward-only, embedded, advisory-locked, run as their own Compose service. |
| Backup and restore documentation | **Implemented** | [`docs/operations.md`](operations.md), including verifying a backup by restoring it. |
| CI that builds, tests and migrates | **Implemented** | Plus a migration up/down/up cycle and both container image builds. |
| Static analysis, formatting, linting | **Implemented** | `gofmt`, `go vet`, `staticcheck`; `tsc`, `eslint`, `prettier`. |
| MongoDB storage | **Changed** | PostgreSQL. Every read is an aggregation over an append-only fact table, which is what a relational engine is for. |
| Prometheus basic auth | **Implemented** | Matches the reference's `PROMETHEUS_USERNAME` / `PROMETHEUS_PASSWORD`. |
| `MONGO_NO_ADMIN_RIGHTS` equivalent | **Changed** | Not applicable. Encore needs no database extensions and no superuser: `gen_random_uuid()` is built into PostgreSQL 13 and later. |
| `MAX_IMPORT_CACHE_SIZE` equivalent | **Changed** | Not applicable, because imports do not call Spotify. The equivalent lever is `ENCORE_IMPORT_BATCH_SIZE`, which bounds memory directly. |
| `FRAME_ANCESTORS` | **Implemented** | `ENCORE_FRAME_ANCESTORS`, feeding the CSP directive. |
| `COOKIE_VALIDITY_MS` | **Implemented** | `ENCORE_SESSION_TTL`, defaulting to 30 days rather than the reference's one hour, because a self-hosted dashboard being signed out hourly is an annoyance rather than a security gain. |

---

## Deliberate deviations, summarised

1. **PostgreSQL rather than MongoDB.** Every query is a range-filtered aggregation.
2. **Go rather than Node/TypeScript.** Streaming import with bounded memory and real backpressure.
3. **A separate worker process.** Imports must not compete with dashboard latency, and the worker must
   be independently restartable for the resume behaviour to be testable.
4. **No Spotify call during ingestion, at all.** The requirement that an API outage cannot lose a
   listening record is only satisfiable this way.
5. **Whole-archive upload.** Removes the most common import support burden.
6. **Read scopes at sign-in, write scopes at the point of use.** Encore asks for
   eight read scopes when somebody signs in, and for a write scope only at the
   moment they use the feature that needs it. The narrower claim is the one that
   matters and it is unchanged: **Encore never holds a grant that can modify a
   listener's Spotify account unless they have used a feature that needs it.**
   `playlist-modify-private` is requested only when a playlist is created; an
   account that never creates one holds a grant that cannot alter anything, even
   if the instance is compromised. Playback control is still declined outright.
7. **Own visual design.** Not required to match, and accessibility and dark mode are first-class.

## Known gaps

Where Encore does less than the reference, or less than it could:

- **Audio features, audio analysis, recommendations, related artists and the browse playlist
  endpoints are permanently unavailable**, not deferred. Spotify restricted all eight of those
  endpoints on 2024-11-27 to applications already in extended quota mode, which now requires
  250,000 monthly active users. Encore runs against the operator's own Spotify application, so every
  instance is a new application in development mode and receives 403 from every one of them. See
  [`docs/design/2026-07-29-spotify-api-expansion-overview.md`](design/2026-07-29-spotify-api-expansion-overview.md).
- **Playback control** is deferred: it needs `user-modify-playback-state` and an active device, and
  it is the one write scope with no read-only equivalent.
- **PWA support stops at installable.** There is no service worker, for the reason given above.
- **Live sync cannot record how long a track was played**, because the recently-played endpoint does
  not report it; a synced listen counts as the track's full duration. Importing an export over the
  same period corrects it in place, and the `source` column records which rows came from where. See
  the note in the README's limitations.
- **A play recorded under a relinked album id counts as unheard.** Both `GET
  /api/albums/{id}/tracklist` and `GET /api/artists/{id}/discography` ask Spotify without a market,
  so they get canonical ids, while a listen carries whatever id Spotify returned for the listener's
  own market. On the album endpoint that misses one track; on the discography it misses a whole
  album, which is still counted in the total but never as played, so a record somebody has genuinely
  listened to can appear under "albums you have never played". Encore does not guess at those
  equivalences on either endpoint. See the limitation note in [`docs/api.md`](api.md).
