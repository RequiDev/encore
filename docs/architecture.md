# Architecture

Encore is three processes and a database.

```
                    ┌──────────────┐
   browser ───────► │  web (nginx) │  static React bundle, proxies /api
                    └──────┬───────┘
                           │ same origin
                    ┌──────▼───────┐         ┌──────────────────┐
                    │  encore-api  │────────►│                  │
                    └──────────────┘         │   PostgreSQL 17  │
                    ┌──────────────┐         │                  │
                    │encore-worker │────────►│                  │
                    └──────┬───────┘         └──────────────────┘
                           │
                           ▼  outbound only
                    Spotify Web API
```

| Process | Responsibility |
|---|---|
| `encore-api` | HTTP API, OAuth, sessions, uploads, statistics queries. Calls Spotify during the OAuth exchange, when somebody creates or renames a playlist, and — unless an operator has turned them off — when somebody opens an album or artist page. It never calls Spotify to serve `/api/nowplaying`, which reads the observation the worker stored. |
| `encore-worker` | Import jobs, metadata enrichment, recently-played synchronisation, library enumeration, the optional now-playing poller, rollup maintenance and session reaping. |
| `encore-migrate` | Applies schema migrations, then exits. Runs before either of the others. |
| `encore-bench` | Generates a synthetic history and benchmarks the real import pipeline. |

Splitting the API from the worker is not decoration. A one-million-record import saturates a database
connection for minutes; letting that compete with a dashboard request would make the UI unusable
exactly when the user most wants to watch progress. It also makes the crash-recovery behaviour
testable: the integration suite kills the worker mid-import and asserts a different worker resumes
from the checkpoint.

---

## Why Go

The dominant technical risk in this project is the importer: streaming a very large JSON document
with bounded memory, applying database backpressure, and checkpointing atomically so a crash resumes
rather than restarts.

- `encoding/json.Decoder` streams natively and exposes `InputOffset()`, a byte-accurate position after
  each decoded element. That is exactly the primitive a resumable checkpoint needs, and it is in the
  standard library.
- `pgx` gives direct control over statement shape. Every ingest and every statistic is hand-written
  analytic SQL, which is where the performance actually lives; an ORM's strengths (change tracking,
  LINQ over an object graph) would have been unused overhead.
- Go 1.22+ `net/http.ServeMux` supports method and path patterns, so Encore needs no HTTP framework at
  all. For self-hosted software that people are expected to trust and upgrade themselves, a small
  dependency tree is a feature.
- Static binaries produce small images that start in milliseconds, which suits a NAS or a home server.
- Fast compile and test cycles keep a large test matrix — including the million-record import —
  practical to run in CI on every push to main.

C# on .NET 10 was the runner-up and would have been defensible: `DeserializeAsyncEnumerable`,
`Npgsql`'s binary COPY, bounded `System.Threading.Channels`, and EF Core migrations cover the same
ground. It was not chosen because the ORM's value would have gone unused while the runtime and image
footprint grew.

## Why PostgreSQL

Every read Encore performs is an aggregation over a large, append-only fact table filtered by user and
date range: top tracks, listening time per week, hour-of-day repartition, longest sessions,
gaps-and-islands streaks. That is what a relational engine with partial and covering indexes, window
functions and `GROUP BY` is for.

It also buys the property the importer depends on most: a `UNIQUE` constraint that makes duplicate
suppression an invariant of the data rather than a promise made by application code, and real
transactions so a batch and its checkpoint commit together.

The reference project uses MongoDB. This is a deliberate deviation.

---

## Package layout

```
cmd/
  encore-api/         HTTP server entrypoint
  encore-worker/      background worker entrypoint
  encore-migrate/     migration CLI (up, status, reset)
  encore-bench/       synthetic history generator and import benchmark
internal/
  domain/             pure types and rules. No I/O. Identity and dedupe keys,
                      name normalisation, listen validation, import accounting and
                      verification, sessions and streaks maths.
  config/             environment parsing and validation
  logging/            slog wiring, request-scoped loggers, redaction
  crypto/             AES-256-GCM sealing, session tokens, PKCE
  retry/              bounded exponential backoff with full jitter
  postgres/           pool, migrations, error classification. The only package that imports pgx.
  store/              database core: Store, Querier, transactions, encrypted columns
    accounts/         users, sessions, OAuth states, credentials, settings
    catalog/          artists, albums, tracks, aliases, blacklist, enrichment queue
    listens/          the ingest path and the duplicate rules
    imports/          jobs, files, rejects, leases, checkpoints, verification data
  stats/              every analytic query, plus rollup maintenance
  spotify/            API client, OAuth, rate limiter, retry, circuit behaviour
  metadata/           catalogue metadata sources: the Source interface, the optional
                      fallback client, and the chain that decides which answers
  importer/           job runner, batching, checkpointing, verification
    formats/          streaming readers, both export formats, detection, archives
  enrich/             catalogue and alias enrichment workers, repair job
  sync/               recently-played poller
  worker/             supervisor, leases, graceful shutdown
  httpapi/            router, middleware, handlers, DTOs. No SQL, no business rules.
  metrics/            Prometheus collectors
migrations/           forward-only numbered SQL, embedded into every binary
web/                  React + TypeScript client
deploy/               Dockerfiles, nginx configuration
test/                 integration, import-fault and end-to-end suites
```

### Dependency direction

`domain` depends on nothing but the standard library and two pure-function libraries
(`golang.org/x/text` for Unicode normalisation, `github.com/google/uuid` for identifier values). It
is enforced by a test that walks the package's imports.

Everything else points inward:

```
httpapi ──► importer ──► store/* ──► store ──► postgres ──► pgx
   │           │            │
   │           └──► formats─┤
   │                        ▼
   └──► stats ──────────► domain ◄──── spotify
```

The rules that matter — what makes two listens the same event, when an import may be called
successful, how a name is normalised — live in `domain`, where they can be tested exhaustively with
no database, no network and no fixtures larger than a few lines. The database layer applies them; the
HTTP layer never sees them.

`httpapi` contains no SQL and never imports `pgx`. `store` never imports `net/http`. If either of
those stops being true, something has been put in the wrong place.

---

## Request path

```
net/http.ServeMux
  └─ recoverer          panics become 500s and a logged stack, never a dropped connection
     └─ requestID       X-Request-Id, attached to the context logger
        └─ logger       one structured line per request with method, route, status, duration
           └─ metrics   request counter, duration histogram, in-flight gauge
              └─ CORS   exact-origin allow-list; a no-op in the bundled same-origin setup
                 └─ security headers  CSP, nosniff, referrer policy, frame-ancestors
                    └─ session        cookie → session → user, or anonymous
                       └─ CSRF        double-submit check on unsafe methods
                          └─ handler
```

Authorisation is **not** in the middleware chain. Every handler that touches user data asks the store
for the object *scoped to the caller* — `GetJobForUser(id, callerID)` rather than `GetJob(id)` — so a
missing check is a compile-time-visible omission rather than a silent hole. Admin-only routes are
additionally wrapped in a `requireAdmin` guard that re-reads the role from the database rather than
trusting anything in the session.

## Authentication

Spotify is the only identity provider; Encore has no passwords and no password reset flow, and
therefore no credential database worth stealing.

1. `GET /api/auth/spotify/login` mints a random `state` and a PKCE verifier, stores the SHA-256 of the
   state with the sealed verifier in `oauth_states`, and redirects to Spotify.
2. `GET /api/auth/spotify/callback` consumes the state **once** (`DELETE ... RETURNING`, so a replayed
   state cannot be reused), exchanges the code with the verifier, reads `/v1/me`, and finds or creates
   the user.
3. A session token is minted, its SHA-256 stored, and the raw value set as an `HttpOnly`, `Secure`,
   `SameSite=Lax` cookie. The database never holds a value that can be replayed as a login.
4. The first user ever created becomes the administrator, decided inside the same `INSERT` so two
   simultaneous first logins cannot both win.

While registrations are disabled, an unknown Spotify identity is refused; existing users can always
sign in.

## Background work

`encore-worker` supervises seven independent loops, each restartable and each with its own failure
isolation:

- **Import runner** — claims jobs under a lease, heartbeats, and stops immediately if the lease is
  stolen. See [`docs/import.md`](import.md).
- **Enrichment** — fills in catalogue metadata from the `pending` queues using a client-credentials
  token, plus the alias resolver and the relink pass. It also drives the rollup loop, which
  recomputes `listen_daily_rollup` for days ingestion marked dirty; that runs even with
  `ENCORE_ENRICH_ENABLED` off, because recomputing statistics is a database concern with nothing
  to do with Spotify.
- **Sync** — polls `/me/player/recently-played` for each connected account, advancing the cursor only
  after the batch commits. After each successful poll it also attaches what the now-playing poller
  observed to the plays that have just arrived — an `UPDATE` of two columns that cannot create,
  move or duplicate a row.
- **Library** — enumerates saved tracks, saved albums, followed artists, the listener's own playlists
  and Spotify's own top rankings, once a day.
- **Now playing** — checks each connected account's player every `ENCORE_NOWPLAYING_INTERVAL`, and
  does not run at all unless that is set — reading `GET /v1/me/player`, recording what it sees for
  the dashboard card and appending it to a short-lived log the sync path uses to fill in shuffle and
  device on the plays it belongs to.
- **Reaper** — clears expired sessions, OAuth states, and playback observations older than a day.
- **Telemetry** — publishes pool statistics.

A failure in any one of them does not stop the others, and none of them can lose a listening record.

## Statistics and scale

Reads follow one of two paths:

- **Fact table** for ranges up to 90 days, or any range containing a dirty rollup day. Driven by
  `listens (user_id, played_at) INCLUDE (ms_played, track_id)`, which makes the timeline and
  listening-time aggregations index-only scans.
- **Daily rollup** for wider ranges whose days are all clean, reading `listen_daily_rollup` instead of
  the fact table.

The dirty-day check makes this safe rather than merely fast: a range that might be stale reads the
fact table, which is slower and always right. Correctness first; the rollup is an optimisation that
can be skipped without changing an answer.

The listening-history feed is **keyset paginated** on `(played_at, id)`, never `OFFSET`, because a
user may legitimately have millions of rows and `OFFSET 900000` re-scans everything before it.
Timelines refuse to return more than `domain.MaxTimelineBuckets` points and suggest a coarser interval
instead.

## Observability

- **Logs**: `log/slog`, JSON in production, one line per request plus per-batch import progress.
  Tokens, cookies and session ids are never logged; `logging.Redact` exists for the cases where a
  fragment of an identifier genuinely helps.
- **Metrics**: `/metrics` in Prometheus format, optionally behind basic auth. Import throughput, queue
  depth, records by outcome, Spotify request outcomes and rate-limit pauses, enrichment backlog, pool
  utilisation.
- **Health**: `/healthz` is liveness — the process is running. `/readyz` is readiness — the database
  answers *and* every embedded migration has been applied, so a process talking to an out-of-date
  schema reports itself unready instead of failing in confusing ways.
