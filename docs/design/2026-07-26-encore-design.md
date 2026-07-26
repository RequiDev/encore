# Encore — Implementation Plan

**Date:** 2026-07-26
**Status:** Design / plan of record

Encore is a self-hosted Spotify listening-history tracker and statistics dashboard. It is an
independent implementation inspired by the user-facing capabilities of
[your_spotify](https://github.com/Yooooomi/your_spotify) (GPL-3.0). No source is copied; only
publicly documented behaviour, data formats and workflows were studied. See
[`docs/attribution.md`](../attribution.md).

---

## 1. Feature parity scope

Derived from the reference project's documented features and UI surface.

### Accounts & auth
- **Spotify is the only identity provider.** Signing in *is* the OAuth flow; there are no local
  passwords. A user row is created on first successful callback.
- Admin role; the first user ever created becomes admin.
- Admin can enable/disable new registrations (an unknown Spotify identity is refused while
  disabled; existing users can always sign in), promote/demote, deactivate and delete users.
- "Relink to Spotify" flow when a refresh token is revoked or scopes change.
- Per-user timezone affecting all bucketed statistics.
- Account deletion / data export (privacy controls).

### Ingestion
- Periodic polling of `GET /v1/me/player/recently-played` per connected account.
- Import of **account data** exports (`StreamingHistory*.json`, `StreamingHistory_music_*.json`).
- Import of **extended streaming history** exports (`Streaming_History_Audio_*.json`, legacy `endsong_*.json`).
- Direct `.zip` upload of the whole Spotify export; entries discovered and imported individually.
- Duplicate-safe re-imports, overlapping imports, and mixed-source overlap.
- Import history, live progress, per-file counters, rejected-record diagnostics, cancel + resume + retry.

### Statistics (all date-range filtered)
- Dashboard summary: listens, distinct tracks/artists/albums, total listening time, top artist/track.
- Top tracks / albums / artists, paginated, with rank change vs the preceding equal-length period.
- Timelines: listens and listening time per hour/day/week/month/year.
- Repartition: by hour-of-day, by weekday, by hour × weekday heatmap.
- Artist detail: rank, first & last listen, day repartition, top tracks/albums, listening share.
- Album and track detail pages with equivalent stats.
- Longest listening sessions.
- Different-artists-per-period, average album release date, average artists per track.
- Listening history browser (cursor paginated).
- Discovery metrics (new artists/tracks per period), listening streaks, year-in-review.
- Period-vs-period comparison.
- Artist blacklist that excludes an artist from all statistics.
- Affinity / benchmark comparison between two users of the same instance.

### Operations
- Configuration exclusively via environment variables, `.env.example` provided.
- Docker Compose deployment (postgres + api + worker + web).
- `/healthz` liveness, `/readyz` readiness, `/metrics` Prometheus (optional basic auth).
- Structured JSON logs (`log/slog`) with request IDs.
- Forward-only SQL migrations applied by a dedicated command and verified by readiness.
- Documented backup/restore/upgrade.

Full item-by-item status lives in [`docs/feature-parity.md`](../feature-parity.md).

---

## 2. Technology choice

**Backend: Go 1.26, standard-library `net/http`.**

Rationale (recorded in `docs/architecture.md`):

- The dominant risk in this project is the importer: streaming a multi-hundred-megabyte JSON
  document with bounded memory, applying backpressure from the database, and checkpointing
  atomically. Go's `encoding/json` `Decoder` streams natively with a byte-accurate
  `InputOffset()`, which is exactly what a resumable checkpoint needs, and `pgx`'s
  `CopyFrom` pulls rows from an iterator, which gives backpressure for free rather than
  requiring an explicit bounded queue.
- Go 1.22+ `net/http.ServeMux` supports method+path patterns, so no HTTP framework
  dependency is required at all. The total third-party dependency set is five modules,
  which matters for a self-hosted app that people are expected to trust and upgrade.
- Static binaries produce ~30 MB distroless images and start in milliseconds, which suits
  a Compose deployment on a NAS or home server.
- Fast compile/test cycles keep the (large) test matrix — including the synthetic 1M-record
  import — practical to run in CI.

C# on .NET 10 was the other candidate and would have been a defensible choice
(`DeserializeAsyncEnumerable`, `Npgsql` binary COPY, bounded `System.Threading.Channels`,
EF Core migrations). It was not chosen because EF Core's value is mostly in the areas this
app does not need (change tracking, LINQ over an object graph) while the statistics layer is
hand-written analytic SQL either way, and the runtime/image footprint is larger.

**Database: PostgreSQL 17.** Every read is an aggregation over a large append-only fact table;
that is precisely what a relational engine with partial/covering indexes and
`GROUP BY`/window functions is for. The reference project uses MongoDB; this is a
deliberate deviation.

**Frontend: React 19 + TypeScript + Vite + TanStack Query + Recharts + Tailwind v4.**
Served by nginx in Compose. Dark mode, responsive, keyboard accessible.

**Third-party Go modules (all actively maintained):**
`jackc/pgx/v5`, `pressly/goose/v3`, `golang.org/x/crypto` (argon2id),
`prometheus/client_golang`, `google/uuid`. Tests add `testcontainers-go`.

---

## 3. Database schema

PostgreSQL 17. Times are `timestamptz` stored in UTC; bucketing applies the user's timezone at
query time via `AT TIME ZONE`.

### Identity
```
users              (id uuid pk, spotify_user_id text unique, display_name, email citext null,
                    avatar_url, product, role user|admin, is_active, timezone,
                    created_at, updated_at, last_login_at)
sessions           (id uuid pk, user_id fk, token_hash bytea unique, csrf_token,
                    expires_at, created_at, last_seen_at, user_agent, ip inet)
spotify_credentials(user_id uuid pk fk, access_token_enc bytea, refresh_token_enc bytea,
                    token_expires_at, scopes text[], sync_state ok|needs_reauth|error,
                    sync_cursor_at timestamptz, last_sync_at, last_sync_error, connected_at)
oauth_states       (state_hash bytea pk, code_verifier_enc bytea, redirect_to, created_at,
                    expires_at)
app_settings       (key text pk, value jsonb, updated_at)      -- registrations_enabled, etc.
```
Spotify tokens and PKCE verifiers are encrypted at rest with AES-256-GCM using
`ENCORE_ENCRYPTION_KEY`. Credentials live in their own table so a revoked grant can be cleared
without touching the user's history.

### Catalog (global, shared between users)
```
artists       (id text pk, name, name_norm, genres text[], popularity, followers, image_url,
               metadata_state pending|resolved|unavailable|failed, fetch_attempts,
               next_attempt_at, fetched_at)
albums        (id text pk, name, album_type, release_date date, release_precision,
               total_tracks, image_url, metadata_state, fetch_attempts, next_attempt_at, fetched_at)
album_artists (album_id, artist_id, position, pk(album_id, artist_id))
tracks        (id text pk, name, name_norm, album_id fk null, duration_ms, explicit,
               popularity, isrc, metadata_state, fetch_attempts, next_attempt_at, fetched_at)
track_artists (track_id, artist_id, position, pk(track_id, artist_id))

track_aliases (artist_norm text, title_norm text, track_id text fk,
               state pending|resolved|unavailable|failed, fetch_attempts, next_attempt_at,
               pk(artist_norm, title_norm))
```
`track_aliases` is the bridge that lets name-only (account-data) listens converge onto real
catalog tracks; see §5.

### Facts
```
listens (
  id            bigint  generated always as identity primary key,
  user_id       uuid    not null,
  played_at     timestamptz not null,          -- normalised START of playback, UTC
  ts_precision  smallint not null,             -- 0=ms, 1=second, 2=minute
  track_id      text     null references tracks,
  alias_artist  text     null,                 -- set when track_id is unknown
  alias_title   text     null,
  identity_key  bytea    not null,             -- sha256 of the track identity, see §5
  dedupe_key    bytea    not null,             -- sha256(user, identity, minute bucket)
  ms_played     integer  not null,
  source        smallint not null,             -- 0 sync, 1 account-data, 2 extended
  import_file_id uuid    null references import_files on delete set null,
  platform, conn_country, reason_start, reason_end text null,
  shuffle, skipped, offline, incognito boolean null,
  created_at    timestamptz not null default now(),
  constraint listens_uk unique (user_id, dedupe_key)
)

user_blacklisted_artists (user_id, artist_id, created_at, pk(user_id, artist_id))
```

### Import bookkeeping
```
import_jobs  (id uuid pk, user_id fk, status queued|running|paused|completed|failed|cancelled,
              kind, created_at, started_at, finished_at, error_code, error_message,
              lease_owner text, lease_expires_at timestamptz,
              files_total, files_done)
import_files (id uuid pk, job_id fk, ordinal int, name text, container_path text null,
              format extended|account_data|unknown, size_bytes bigint, sha256 bytea,
              status pending|running|completed|failed|skipped,
              records_total bigint null,        -- known only after a first pass or at EOF
              record_offset bigint not null default 0,
              byte_offset  bigint null,
              imported, duplicates, skipped_cnt, rejected bigint not null default 0,
              error_code, error_message, started_at, finished_at)
import_rejects (id bigint identity pk, file_id fk, record_index bigint, reason text,
                detail text, raw_excerpt text, created_at)
```
`import_files.sha256` gives file-level duplicate detection ("you already imported this file").

### Indexing strategy

| Index | Justification |
|---|---|
| `listens (user_id, played_at DESC)` | history browsing, every range filter, sync cursor |
| `listens (user_id, track_id, played_at DESC)` where `track_id is not null` | track detail, top-tracks range scans, cross-source fuzzy dedupe probe |
| `listens (user_id, identity_key, played_at)` | ±window duplicate probe during import |
| `listens (user_id, dedupe_key)` unique | idempotency, enforced by the database |
| `listens (import_file_id)` where not null | per-job verification and rollback |
| `listens (user_id, played_at) include (ms_played, track_id)` | covering index for timeline aggregation |
| `track_artists (artist_id, track_id)` | top-artists and artist-detail joins |
| `tracks (metadata_state, next_attempt_at)` where state in (pending, failed) | enrichment queue scan |
| `albums`/`artists` same partial index | enrichment queue scan |
| `track_aliases (state, next_attempt_at)` where state in (pending, failed) | alias resolution queue |
| `import_jobs (status, lease_expires_at)` | worker job claiming with `FOR UPDATE SKIP LOCKED` |
| `sessions (token_hash)` unique, `sessions (expires_at)` | auth lookup + reaping |

Top-N artist/album statistics require joining `listens → track_artists`. For instances with
very large histories a `listen_daily_rollup (user_id, day, track_id, plays, ms)` materialised
table is maintained incrementally and used when the requested range is wider than a
configurable threshold (default 90 days). This is the pagination/responsiveness strategy.

---

## 4. Historical-import design

### Pipeline

```
upload ──► spooled to disk (streamed, never buffered)  ──► import_files rows (+ sha256)
                                                              │
                                          job queued ─────────┘
                                                              │
   worker claims job (UPDATE ... FOR UPDATE SKIP LOCKED, lease + heartbeat)
                                                              │
   per file:  open → seek to checkpoint → stream-decode JSON array element by element
                                                              │
              validate/normalise each record ── invalid ──► import_rejects (reason + excerpt)
                                                              │
              accumulate into a bounded batch (default 1000 records)
                                                              │
              flush batch in ONE transaction:
                 1. COPY batch into a TEMP staging table          (backpressure: COPY blocks)
                 2. upsert newly-seen track ids  → tracks(metadata_state='pending')
                 3. upsert newly-seen name pairs → track_aliases(state='pending')
                 4. INSERT ... SELECT into listens with dedupe    (returns imported count)
                 5. UPDATE import_files SET record_offset, byte_offset, counters
                 COMMIT   ← records and their checkpoint are committed atomically
                                                              │
              EOF → import_files.status='completed'
                                                              │
   all files done → verification query → import_jobs.status='completed'
```

Metadata enrichment is a **completely separate** subsystem (§6). An import never calls the
Spotify API. A total Spotify outage cannot lose or delay a single listening record.

### Streaming and memory

`encoding/json.Decoder` is driven token-by-token: read the opening `[`, then `Decode` one
element at a time. Peak resident memory is `O(batch_size × record_size)` plus the decoder's
buffer, independent of file size. NDJSON and gzip are auto-detected; `.zip` archives are
read entry-by-entry through `archive/zip`.

**Documented memory target: the import worker stays below 256 MiB RSS for any input size at
the default batch size of 1000.** Verified by `cmd/encore-bench`, results in
`docs/import.md`.

### Checkpoints and resume

`import_files.record_offset` (always) and `byte_offset` (when the source is seekable, i.e. a
plain or uncompressed file) are written **inside the same transaction as the batch**. So the
invariant is: *committed records ≤ checkpoint is always exactly true*. On resume the worker
seeks to `byte_offset` and re-enters array-element state, or replays and discards
`record_offset` elements for non-seekable sources (gzip/zip entries).

Crash recovery needs no special path: a job's lease expires and any worker re-claims it,
starting from the last committed checkpoint. Re-processing the records between the last
checkpoint and the crash is safe because insertion is idempotent.

### Deterministic duplicate prevention

Two layers, both documented and tested.

**Layer 1 — exact key, enforced by the database.**
```
identity_key = sha256( "t:" || spotify_track_id )                 when a track URI is known
             = sha256( "n:" || norm(artist) || 0x00 || norm(title) )  otherwise
dedupe_key   = sha256( user_id || identity_key || floor(unix(played_at_start) / 60) )
UNIQUE (user_id, dedupe_key)
```
`played_at_start` is normalised across sources:
- extended history: `ts - ms_played` (`ts` is the stream **end** time)
- account data: `endTime - msPlayed`
- recently-played sync: `played_at` from the API (documented as the play time; treated as start)

`norm()` = NFKC, casefold, strip bracketed suffixes like `(Remastered 2011)`/`- Live`,
collapse whitespace. Re-importing the same file therefore inserts exactly zero new rows.

**Layer 2 — cross-source fuzzy suppression.** A minute bucket has an edge case at boundaries,
and the same listen arrives from sources with different timestamp precision. Before inserting,
the batch is anti-joined against existing rows:
```sql
NOT EXISTS (SELECT 1 FROM listens l
            WHERE l.user_id = s.user_id
              AND l.identity_key = s.identity_key
              AND l.played_at BETWEEN s.played_at - w AND s.played_at + w)
```
where `w = max(precision(s), precision(l))` seconds — 60 s when either side is minute-precision
account data, 10 s otherwise, and 0 for two records of identical source+precision (which Layer 1
already covers exactly). Duplicates within the incoming batch are removed by `DISTINCT ON
(user_id, dedupe_key)` before the anti-join.

**Layer 3 — relink reconciliation.** When a `track_alias` resolves, name-only listens are
relinked to the real `track_id`; their `identity_key`/`dedupe_key` are recomputed, and rows that
then collide with an existing URI-based listen are deleted and counted as reconciled duplicates.
This is what makes an account-data import followed by an extended import converge.

### Failure taxonomy

| Class | Example | Handling |
|---|---|---|
| Transient | DB connection reset, deadlock, 5xx | batch retried with exponential backoff + jitter, bounded (default 6 attempts); then the *job* is marked `failed` with the checkpoint intact and is retryable |
| Permanent record error | missing `ts`, non-numeric `ms_played`, unparseable date, wrong object shape | row written to `import_rejects` with record index, reason and a truncated raw excerpt; import continues |
| Skipped (valid, not a music listen) | podcast/audiobook entry, `ms_played` below `ENCORE_IMPORT_MIN_MS` | counted as `skipped`, not an error |
| Job failure | file unreadable, not JSON at all, retries exhausted | job `failed`, error code + message surfaced in the UI, `POST /api/imports/:id/retry` resumes from the checkpoint |

### Counters and honest completion

Per file and aggregated per job: `imported`, `skipped`, `duplicates`, `rejected`, `pending`
(= `records_total − processed`, or "unknown" until EOF), `failed`.

A job is only `completed` after a verification step that asserts, in SQL:
`imported + duplicates + skipped + rejected == records_processed` for every file, every file is
`completed`, and `count(listens where import_file_id = f.id) == f.imported`. If the assertion
fails the job becomes `failed` with `verification_failed` — never a false success.

### Cancellation

`POST /api/imports/:id/cancel` sets a flag; the worker finishes the in-flight batch (so the
checkpoint stays consistent), marks the job `cancelled`, and releases the lease.
`POST /api/imports/:id/retry` resumes a `failed`/`cancelled` job from its checkpoint.

---

## 5. Spotify integration

- **OAuth**: authorization code flow with PKCE, `state` bound to an HMAC-signed short-lived
  cookie, scopes `user-read-recently-played user-read-private user-read-email`.
  Tokens encrypted at rest; refresh handled centrally with single-flight.
- **Sync**: every `ENCORE_SYNC_INTERVAL` (default 60s, jittered) each connected account is
  polled with `recently-played?limit=50&after=<cursor_ms>`; results run through the same
  dedupe path as imports. `sync_cursor_at` advances only after the batch commits.
- **Rate limiting**: a per-instance token bucket plus strict `Retry-After` compliance on 429.
  Retries are bounded, exponential, with full jitter. Repeated 429/5xx opens a circuit breaker
  that pauses enrichment without touching ingestion.
- **Enrichment workers**: `tracks` (50 ids/req), `albums` (20), `artists` (50), and alias
  resolution via `/v1/search` (1/req). Catalog reads use a **client-credentials** token, so
  enrichment works even when no user is currently connected. Failures increment
  `fetch_attempts` and set `next_attempt_at` with backoff; after the cap the row becomes
  `unavailable` (deleted/regional) or `failed`, both visible in admin diagnostics and repairable
  by a background repair job.

---

## 6. Solution structure

```
encore/
├─ cmd/
│  ├─ encore-api/          HTTP server
│  ├─ encore-worker/       import + enrichment + sync workers
│  ├─ encore-migrate/      migration runner
│  └─ encore-bench/        synthetic history generator + import benchmark
├─ internal/
│  ├─ domain/              pure types & rules: normalisation, identity/dedupe keys,
│  │                       listen validation, session/streak maths. No I/O, no imports
│  │                       outside the standard library.
│  ├─ config/              environment parsing + validation
│  ├─ logging/             slog wiring, request-scoped loggers
│  ├─ crypto/              AES-GCM token sealing, argon2id passwords
│  ├─ postgres/            pool, tx helpers, migration embedding, health probes
│  ├─ store/               repositories: users, sessions, spotify accounts, listens,
│  │                       catalog, imports, settings
│  ├─ stats/               analytic queries + rollup maintenance
│  ├─ spotify/             API client, oauth, rate limiter, retry/backoff, circuit breaker
│  ├─ importer/            job runner, batching, checkpointing, verification
│  │  └─ formats/          extended, account-data, detection, jsonstream reader
│  ├─ enrich/              catalog + alias enrichment workers, repair job
│  ├─ sync/                recently-played poller
│  ├─ worker/              supervisor, leases, graceful shutdown
│  ├─ httpapi/             router, middleware (auth, csrf, request id, recovery, cors),
│  │                       handlers, DTOs. Contains no SQL and no business rules.
│  └─ metrics/             Prometheus collectors
├─ migrations/             0001_*.sql … (goose, forward-only)
├─ web/                    React + TS + Vite client
├─ deploy/                 Dockerfiles, docker-compose.yml, nginx.conf, backup scripts
├─ test/
│  ├─ integration/         real Postgres via testcontainers
│  ├─ importrig/           fault-injecting harness (kill worker, drop DB, fake 429)
│  └─ testdata/            small fixtures + generators
└─ docs/
```

`domain` depends on nothing. `store`/`spotify`/`importer` depend on `domain`. `httpapi` depends
on services, never on `pgx`. Enforced by an `internal/domain` import-cycle test.

---

## 7. Testing plan

- **Unit**: normalisation, identity/dedupe key derivation, both format parsers (including the
  2024+ field renames `ip_addr_decrypted`→`ip_addr` and audiobook entries), timestamp
  normalisation, session/streak/rollup maths, backoff+jitter, rate limiter, config validation.
- **Integration** (testcontainers Postgres): migrations up, repositories, statistics queries
  against a seeded history, auth/CSRF/authorisation, admin controls.
- **Import validation** — one test per required scenario:
  duplicate file, overlapping imports, worker restart mid-import, database interruption
  mid-import, Spotify 429 during enrichment, malformed + partially valid file, multi-file job,
  unknown/unavailable tracks, 1M-record synthetic history, and a forged "complete" job with
  uncommitted records (must be reported `failed`, not success).
- **E2E**: Playwright over Compose for login → connect (mock Spotify) → sync → import → stats.
- **Static analysis**: `gofmt`, `go vet`, `staticcheck`, `golangci-lint`; `tsc`, `eslint`,
  `prettier`. CI builds, runs migrations, and runs the full suite.

---

## 8. Deliberate deviations from the reference

| Deviation | Reason |
|---|---|
| PostgreSQL instead of MongoDB | Every query is an aggregation over an append-only fact table |
| Go instead of Node/TypeScript | Streaming import with bounded memory and backpressure; small images |
| Separate `encore-worker` process | Import and enrichment must not compete with request latency, and the worker can be restarted independently — required for the resume tests |
| Enrichment fully decoupled from ingestion | Requirement: Spotify outages must not lose listens |
| Client-credentials token for catalog enrichment | Works with zero connected users; avoids burning user rate limit |
| `.zip` upload of the raw Spotify export | Removes the most common import support burden |
| Own visual design, Tailwind + Recharts | Not required to match; accessibility and dark mode are first-class |
| No playlist creation from stats (deferred) | Requires playlist write scopes; documented in the parity checklist |

Optional enhancements in scope (built after the required functionality): Prometheus metrics
including import throughput and queue depth, year-in-review, listening streaks, discovery
metrics, period comparison, data export, hard account deletion, dark mode and PWA support.

---

## 9. Milestones

1. Repo skeleton, config, logging, migrations, Postgres plumbing, CI.
2. Domain + normalisation + dedupe keys (TDD).
3. Store layer + integration tests.
4. Auth, sessions, CSRF, admin controls.
5. Spotify client, OAuth, rate limiter, sync.
6. Importer: streaming formats, checkpointing, batching, verification.
7. Enrichment + relink reconciliation.
8. Statistics queries + rollups.
9. HTTP API + OpenAPI-ish reference.
10. React frontend.
11. Import validation matrix + fault-injection harness + benchmark.
12. Docker Compose, docs, parity checklist, backup/restore.
