# Environment variable reference

Every setting Encore reads is an environment variable prefixed `ENCORE_`. There is no configuration
file. [`.env.example`](../.env.example) is a copy-and-edit starting point.

Parsing collects **all** problems before failing, so a misconfigured deployment tells you everything
that is wrong in one startup attempt rather than one variable per restart.

Durations accept Go syntax (`30s`, `5m`, `2h`, `720h`) or a bare integer meaning seconds. Byte sizes
accept a plain number or a suffix (`512kb`, `64mb`, `4gb`). Booleans accept `true`/`false`/`1`/`0`.

## Required

| Variable | Description |
|---|---|
| `ENCORE_PUBLIC_URL` | Absolute URL at which browsers reach the API. Used to build the OAuth redirect URI, so it must match what you registered with Spotify. |
| `ENCORE_WEB_URL` | Absolute URL at which browsers reach the web client. OAuth journeys end here. |
| `ENCORE_DATABASE_URL` | PostgreSQL connection string. Set automatically inside the compose stack. |
| `ENCORE_SPOTIFY_CLIENT_ID` | From the Spotify developer dashboard. |
| `ENCORE_SPOTIFY_CLIENT_SECRET` | From the Spotify developer dashboard. Never commit it. |
| `ENCORE_ENCRYPTION_KEY` | 32 bytes, as base64 (standard or URL-safe, padded or not) or 64 hex characters. Generate with `openssl rand -base64 32`. Encrypts Spotify tokens at rest. **Back this up with your database**: losing it forces every user to reconnect. |

## Instance

| Variable | Default | Description |
|---|---|---|
| `ENCORE_ENV` | `production` | `development` relaxes cookie security and switches logs to text. |
| `ENCORE_DEFAULT_TIMEZONE` | `UTC` | IANA zone seeding new users. Each user can change their own. |
| `ENCORE_REGISTRATIONS_DEFAULT` | `true` | Seeds `registrations_enabled` on a **brand-new database only**. Afterwards an administrator controls it from the Settings page. |

## HTTP

| Variable | Default | Description |
|---|---|---|
| `ENCORE_HTTP_ADDR` | `:8080` | Listen address. |
| `ENCORE_HTTP_READ_TIMEOUT` | `30s` | |
| `ENCORE_HTTP_WRITE_TIMEOUT` | `60s` | |
| `ENCORE_HTTP_IDLE_TIMEOUT` | `120s` | |
| `ENCORE_HTTP_SHUTDOWN_TIMEOUT` | `20s` | Grace period for in-flight requests on SIGTERM. |
| `ENCORE_HTTP_MAX_REQUEST_BYTES` | `1mb` | Cap on non-upload request bodies. Uploads use `ENCORE_IMPORT_MAX_UPLOAD_BYTES`. |
| `ENCORE_CORS_ORIGINS` | *(unset)* | Comma-separated exact origins. Leave unset with the bundled nginx, which serves the UI from the API's own origin and therefore needs no CORS at all. |
| `ENCORE_TRUST_PROXY_HEADERS` | `false` | Believe `X-Forwarded-For` / `X-Forwarded-Proto`. Enable **only** behind a proxy you control; otherwise a client can forge its own address. |
| `ENCORE_FRAME_ANCESTORS` | *(unset)* | Sites permitted to frame Encore, for the CSP directive. Unset means none. |

## Database

| Variable | Default | Description |
|---|---|---|
| `ENCORE_DATABASE_MAX_CONNS` | `10` | Pool ceiling. This is also the importer's backpressure valve: when every connection is busy the file reader blocks, which is why memory cannot grow when the database is slow. |
| `ENCORE_DATABASE_MIN_CONNS` | `0` | |
| `ENCORE_DATABASE_CONNECT_TIMEOUT` | `10s` | |
| `ENCORE_DATABASE_STATEMENT_TIMEOUT` | `60s` | Applied per session; stops a runaway statistics query pinning a connection. |
| `ENCORE_DATABASE_MIGRATE_ON_START` | `false` | Run pending migrations during API startup. Off by default because migrations should be a deliberate, separately observable step; the compose stack runs `encore-migrate` as its own service. |

## Sessions and cookies

| Variable | Default | Description |
|---|---|---|
| `ENCORE_SESSION_TTL` | `720h` (30 days) | |
| `ENCORE_COOKIE_NAME` | `encore_session` | |
| `ENCORE_COOKIE_DOMAIN` | *(unset)* | Leave unset for a host-only cookie, which is the safer choice. |
| `ENCORE_COOKIE_PATH` | `/` | |
| `ENCORE_COOKIE_SECURE` | `true` (`false` when `ENCORE_ENV=development`) | Must be true whenever Encore is reachable over HTTPS. |
| `ENCORE_COOKIE_SAMESITE` | `lax` | `strict`, `lax` or `none`. `none` requires `ENCORE_COOKIE_SECURE=true`; browsers reject the combination otherwise, and Encore refuses to start on it. |

## Spotify API

| Variable | Default | Description |
|---|---|---|
| `ENCORE_SPOTIFY_REDIRECT_URL` | `${ENCORE_PUBLIC_URL}/api/auth/spotify/callback` | Override only if you terminate the callback elsewhere. Must match the dashboard exactly. |
| `ENCORE_SPOTIFY_RATE_LIMIT` | `2` | Sustained requests per second across the whole process. Deliberately low: a Spotify app starts in development mode, whose daily quota is small, and exhausting it returns a 429 with a `Retry-After` of nearly a day during which no metadata can be fetched at all. Enriching a sixteen-thousand-track backlog is only about three hundred requests, so speed buys nothing. Raise it only with [extended quota mode](https://developer.spotify.com/documentation/web-api/concepts/quota-modes). |
| `ENCORE_SPOTIFY_RATE_BURST` | `4` | |
| `ENCORE_SPOTIFY_TIMEOUT` | `20s` | Per-request HTTP timeout. |
| `ENCORE_SPOTIFY_MAX_RETRIES` | `5` | Bounded retries per request, exponential with full jitter. `Retry-After` on a 429 always wins over the computed delay. |
| `ENCORE_SPOTIFY_API_BASE_URL` | `https://api.spotify.com` | Overridden by the test suite. |
| `ENCORE_SPOTIFY_AUTH_BASE_URL` | `https://accounts.spotify.com` | Overridden by the test suite. |

## Synchronisation

| Variable | Default | Description |
|---|---|---|
| `ENCORE_SYNC_ENABLED` | `true` | |
| `ENCORE_SYNC_INTERVAL` | `60s` | Spotify's recently-played endpoint returns at most the last 50 plays, so polling much less often risks missing history on a heavy listening day. |
| `ENCORE_SYNC_CONCURRENCY` | `4` | Accounts polled at once. |
| `ENCORE_SYNC_INITIAL_LOOKBACK` | `336h` (14 days) | How far back the very first poll for a new account reaches. |

## Import

| Variable | Default | Description |
|---|---|---|
| `ENCORE_IMPORT_DIR` | `/var/lib/encore/imports` | Durable storage shared by the API and the worker. A resumed import re-reads the original upload, so this **must** survive a restart and be visible to whichever worker claims the job. |
| `ENCORE_IMPORT_BATCH_SIZE` | `1000` | Records per transaction. The main lever on peak memory, which is O(batch × record size). Raising it trades memory for throughput; see [`docs/benchmarks.md`](benchmarks.md). |
| `ENCORE_IMPORT_MAX_UPLOAD_BYTES` | `4gb` | Per-upload cap. The bundled nginx allows 8 GB, so raise both if you need more. |
| `ENCORE_IMPORT_MIN_MS` | `1000` | Plays shorter than this are counted as **skipped**, not stored. Set `30000` to match Spotify's own definition of a stream, or `0` to keep every event including zero-length scrubs. |
| `ENCORE_IMPORT_MAX_REJECTS_PER_FILE` | `1000` | Cap on stored diagnostics so one pathological export cannot fill the disk. The reject *count* keeps rising past the cap. |
| `ENCORE_IMPORT_WORKERS` | `1` | Jobs one worker process runs concurrently. Several worker containers can also run against the same database; leases keep them from colliding. |
| `ENCORE_IMPORT_LEASE_TTL` | `60s` | How long a claim survives without a heartbeat. A crashed worker's job becomes claimable this long after it dies. |
| `ENCORE_IMPORT_BATCH_RETRIES` | `6` | Retries of a single failing batch before the job fails. The checkpoint survives either way. |
| `ENCORE_IMPORT_RETAIN_FILES` | `true` | Keep uploads after a job completes so it can be re-run. Set false to reclaim disk automatically. |

## Enrichment

| Variable | Default | Description |
|---|---|---|
| `ENCORE_ENRICH_ENABLED` | `true` | Turning this off stops metadata being filled in. Imports still work; tracks stay unnamed. |
| `ENCORE_ENRICH_INTERVAL` | `5s` | How often an idle worker looks for new work. |
| `ENCORE_ENRICH_BATCH_SIZE` | `50` | Capped by Spotify: 50 tracks, 50 artists, 20 albums. |
| `ENCORE_ENRICH_ALIAS_ENABLED` | `true` | Resolves names-only account-data listens through the search API. Costs one request per distinct pair. |
| `ENCORE_ENRICH_ALIAS_RATE` | `2` | Requests per second reserved for alias resolution. |
| `ENCORE_ENRICH_REPAIR_INTERVAL` | `6h` | How often permanently failed catalogue rows are revisited. |
| `ENCORE_ROLLUP_INTERVAL` | `30s` | How often dirty statistics rollup days are recomputed. |

## Metadata fallback

Optional. A second source of catalogue metadata, consulted while Spotify is rate limiting the
instance and for ids Spotify will not serve at all. Off unless the URL is set. The full contract —
three Spotify-shaped endpoints — is in [metadata-fallback.md](metadata-fallback.md).

| Variable | Default | Description |
|---|---|---|
| `ENCORE_METADATA_FALLBACK_URL` | *(unset)* | Base URL of a Spotify-shaped API, the part before `/v1/tracks`. Setting it turns the feature on; an unusable value is a startup error. |
| `ENCORE_METADATA_FALLBACK_TOKEN` | *(unset)* | Sent as `Authorization: Bearer …`. Setting it without a URL is a startup error. |
| `ENCORE_METADATA_FALLBACK_TIMEOUT` | `10s` | Per-request HTTP timeout. |
| `ENCORE_METADATA_FALLBACK_BATCH_SIZE` | `50` | Ids per request, capped at Spotify's own limit. |
| `ENCORE_METADATA_FALLBACK_RATE_LIMIT` | *(unlimited)* | Requests per second against the fallback. |
| `ENCORE_METADATA_FALLBACK_RATE_BURST` | `1` | Burst allowance, only meaningful with a rate limit. |
| `ENCORE_METADATA_FALLBACK_PREFER` | `true` | Ask the fallback before Spotify, spending the Spotify quota only on what it lacks. `false` keeps Spotify first. |

Encore ships no fallback and endorses none; it ships the interface. What an operator serves from
their own endpoint is their affair.

## Metrics

| Variable | Default | Description |
|---|---|---|
| `ENCORE_METRICS_ENABLED` | `true` | Serves `/metrics` in Prometheus text format. |
| `ENCORE_METRICS_USERNAME` | *(unset)* | Set both username and password to require basic auth. Setting only one is a configuration error. |
| `ENCORE_METRICS_PASSWORD` | *(unset)* | |

## Logging

| Variable | Default | Description |
|---|---|---|
| `ENCORE_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error`. |
| `ENCORE_LOG_FORMAT` | `json` (`text` in development) | |
| `ENCORE_LOG_SOURCE` | `false` (`true` in development) | Adds `file:line` to every record. |

## Worker

| Variable | Default | Description |
|---|---|---|
| `ENCORE_WORKER_ID` | hostname | Identifies this process in import job leases. Give each worker container a distinct value if you run several. |

## Compose-only variables

These are read by `docker-compose.yml` rather than by Encore itself.

| Variable | Default | Description |
|---|---|---|
| `POSTGRES_PASSWORD` | *(required)* | Password for the bundled database. |
| `ENCORE_API_PORT` | `8080` | Host port for the API. |
| `ENCORE_WEB_PORT` | `3000` | Host port for the web client. |
| `ENCORE_VERSION` | `latest` | Image tag. |
