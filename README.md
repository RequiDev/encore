# Encore

Self-hosted Spotify listening history and statistics. Encore keeps a permanent, private record of
everything you play, imports your entire history from Spotify's own data exports, and turns it into
dashboards you actually own.

> **This repository was written entirely by [Claude Code](https://claude.com/claude-code)** —
> the design, the Go services, the React client, the SQL schema, the tests, the Docker setup and
> this documentation. It is an independent implementation inspired by the user-facing capabilities
> of [your_spotify](https://github.com/Yooooomi/your_spotify); no code was copied from it. See
> [`docs/attribution.md`](docs/attribution.md).

---

## What it looks like

![The Encore dashboard](docs/screenshots/dashboard-dark.png)

<table>
<tr>
<td width="50%"><img src="docs/screenshots/artists.png" alt="Top artists, ranked with movement against the previous period"></td>
<td width="50%"><img src="docs/screenshots/artist-detail.png" alt="An artist page in the light theme"></td>
</tr>
<tr>
<td><img src="docs/screenshots/sessions.png" alt="Longest listening sessions"></td>
<td><img src="docs/screenshots/discovery.png" alt="Discovery over time"></td>
</tr>
<tr>
<td><img src="docs/screenshots/settings.png" alt="Settings, showing metadata progress"></td>
<td><img src="docs/screenshots/dashboard-light.png" alt="The dashboard in the light theme"></td>
</tr>
</table>

*Captured from a throwaway instance seeded with an invented catalogue — not
anybody's real listening. Artwork is blank because that demo never talked to
Spotify.*

---

## What it does

- **Signs you in with Spotify.** No separate password to manage.
- **Records what you play**, polling your recently-played tracks every minute.
- **Imports your whole history** from both Spotify data exports — the one-year "Account data" export
  and the complete "Extended streaming history" export. Upload the zip Spotify sends you and Encore
  works out the rest.
- **Survives interruptions.** Imports checkpoint continuously, resume after a crash or a restart, and
  are safe to run twice: re-importing the same file adds exactly zero rows.
- **Handles a million records** on ordinary hardware, in bounded memory, without needing a Spotify API
  call per listen.
- **Shows you the statistics** — top tracks, artists and albums with rank movement, listening time
  over any date range, hour-of-day and weekday patterns, longest sessions, listening streaks, new-artist
  discovery, year in review, and period-over-period comparison.
- **Names your music from the export itself.** Both formats print the artist and album of every play
  and identify neither, so Encore mints catalogue rows from the names. A freshly imported history has
  working artist and album pages immediately, with no Spotify call at all — a 144,000-record export
  produces about 3,500 named artists before a single request is made.
- **Builds Spotify playlists** from what you actually played: most played, everything over a play
  count, first-heard-in-a-period, or forgotten favourites — over any range, ranked by plays or by
  listening time. Every definition can be previewed first, and the write permission is asked for only
  when you create one, so an account that never makes a playlist keeps a read-only grant.
- **Shares a read-only link** to your statistics with somebody who has no account here. Aggregates
  only: totals, charts and rankings, never individual plays or when they happened. Revocable, and
  optionally a rolling window that stays current.
- **Keeps working when Spotify stops answering.** A development-mode Spotify application exhausts its
  daily quota during a large import; Encore records the pause, waits it out across restarts, says so
  in the interface, and never lets it interfere with signing in. You can also point it at your own
  metadata source, which it will prefer — see [`docs/metadata-fallback.md`](docs/metadata-fallback.md).
- **Supports several people** on one instance, with an artist blacklist and a per-user timezone, and
  lets an administrator close registrations once everyone has an account.
- **Exports and deletes** your data on request.

---

## Quick start with Docker Compose

You need Docker with Compose v2, and a Spotify application (two minutes, see below).

```bash
git clone https://github.com/RequiDev/encore.git
cd encore

cp .env.example .env
```

Edit `.env` and set the five required values:

```dotenv
ENCORE_PUBLIC_URL=http://127.0.0.1:8080
ENCORE_WEB_URL=http://127.0.0.1:3000
ENCORE_SPOTIFY_CLIENT_ID=<from the Spotify dashboard>
ENCORE_SPOTIFY_CLIENT_SECRET=<from the Spotify dashboard>
ENCORE_ENCRYPTION_KEY=<openssl rand -base64 32>
POSTGRES_PASSWORD=<anything long>
```

Then, either build it yourself:

```bash
docker compose up -d --build
```

or pull the images CI publishes, which is faster and works on a machine too small
to compile on:

```bash
docker compose -f docker-compose.yml -f docker-compose.server.yml pull
docker compose -f docker-compose.yml -f docker-compose.server.yml up -d
```

Open <http://127.0.0.1:3000> and sign in with Spotify. **The first account to sign in becomes the
administrator**, so do this yourself before sharing the URL.

Check everything came up:

```bash
docker compose ps
curl -fsS http://127.0.0.1:8080/healthz     # liveness
curl -fsS http://127.0.0.1:8080/readyz      # readiness: database reachable, migrations applied
```

To stop: `docker compose down`. To stop and delete all data: `docker compose down -v`.

---

## Configuring a Spotify application

1. Go to the [Spotify developer dashboard](https://developer.spotify.com/dashboard) and sign in.
2. **Create app**. Name and description are yours to choose; users never see them.
3. Set the **Redirect URI** to exactly:

   ```
   http://127.0.0.1:8080/api/auth/spotify/callback
   ```

   **Not `localhost`.** Spotify requires redirect URIs to be HTTPS, with one exception: an explicit
   loopback literal such as `http://127.0.0.1:PORT`. `localhost` has been rejected since April 2025.
   For a deployment on anything other than the machine you browse from, you therefore need HTTPS —
   see [`docs/deployment.md`](docs/deployment.md).

   It must match `ENCORE_PUBLIC_URL` + `/api/auth/spotify/callback` character for character —
   a trailing slash, or `http` versus `https`, fails with `INVALID_CLIENT: Invalid redirect URI`.

   Use the **same host form in both URLs**. `localhost` and `127.0.0.1` are different origins as far
   as cookies are concerned, so mixing them lets the sign-in succeed and then immediately drops you
   back to the login page with no visible error.
4. Under **Which API/SDKs are you planning to use?** tick **Web API**.
5. Save, then copy the **Client ID** and **Client secret** into `.env`.

Your app starts in *development mode*, which allows up to 25 listeners that you add explicitly under
**Settings → User Management**. That is normally plenty for a self-hosted instance. Encore requests
only read scopes (`user-read-recently-played`, `user-read-private`, `user-read-email`) and never
modifies your Spotify account, library or playlists.

---

## Importing your history

Spotify offers two exports, from
[Privacy Settings](https://www.spotify.com/account/privacy/). Request both — they complement
each other and Encore de-duplicates the overlap automatically.

| Export | Covers | Arrives in | Files |
|---|---|---|---|
| **Account data** | Last 12 months | up to 5 days | `StreamingHistory*.json` |
| **Extended streaming history** | Everything since you joined | up to 30 days | `Streaming_History_Audio_*.json` |

Then, in Encore: **Settings → Import**, and upload the zip Spotify emailed you, exactly as it arrived.
Encore looks inside the archive, finds the streaming-history files, ignores everything else
(playlists, search queries, the read-me PDF), and starts importing.

The Imports page shows live progress per file, with imported, duplicate, skipped, rejected and pending
counts. You can cancel a running import and resume it later; you can retry a failed one and it
continues from where it stopped, not from the beginning.

Track names and artwork fill in over the following minutes: listening records are stored first and
Spotify metadata is fetched separately in the background, so a rate limit or an outage can never lose
a listen. Full detail in [`docs/import.md`](docs/import.md).

---

## Running locally without Docker

Requires Go 1.26, Node 22 and a PostgreSQL 17 you can reach.

```bash
# 1. A throwaway database
docker run -d --name encore-dev-db \
  -e POSTGRES_USER=encore -e POSTGRES_PASSWORD=encore -e POSTGRES_DB=encore -e PGTZ=UTC \
  -p 5432:5432 postgres:17-alpine

export ENCORE_DATABASE_URL='postgres://encore:encore@localhost:5432/encore?sslmode=disable'
export ENCORE_PUBLIC_URL='http://127.0.0.1:8080'
export ENCORE_WEB_URL='http://127.0.0.1:5173'
export ENCORE_SPOTIFY_CLIENT_ID='...'
export ENCORE_SPOTIFY_CLIENT_SECRET='...'
export ENCORE_ENCRYPTION_KEY="$(openssl rand -base64 32)"
export ENCORE_ENV=development

# 2. Schema
go run ./cmd/encore-migrate up

# 3. Three terminals
go run ./cmd/encore-api
go run ./cmd/encore-worker
cd web && npm install && npm run dev
```

The Vite dev server runs on <http://127.0.0.1:5173> and proxies `/api` to the Go API, so the browser
still sees a single origin and no CORS configuration is needed. Register
`http://127.0.0.1:8080/api/auth/spotify/callback` as your redirect URI.

On Windows PowerShell, use `$env:ENCORE_DATABASE_URL = '...'` in place of `export`.

<details>
<summary>Running the race detector on Windows</summary>

`-race` needs cgo, and cgo needs a C toolchain that a Windows Go install does not
have by default, so `go test -race` fails with `-race requires cgo`. CI runs the
suite with it, so a data race can pass locally and fail there. Reproduce CI
exactly by running the tests in a Linux container:

```powershell
docker run --rm -v "${PWD}:/src" -w /src --network host `
  -e ENCORE_TEST_DATABASE_URL='postgres://encore:encore@localhost:5432/encore?sslmode=disable' `
  golang:1.26 go test -tags=integration -race -count=1 -p 1 ./test/...
```

</details>

<details>
<summary>If <code>npm run build</code> fails with "Cannot find module @rollup/rollup-…"</summary>

Vite's bundler ships as a set of per-platform binaries installed through npm's optional dependencies,
and npm resolves those against the platform it *thinks* it is targeting. If an `os=` or `cpu=` line
is set in `~/.npmrc` — or `npm_config_os` is exported in your shell — npm silently fetches the wrong
binary and the build fails on a missing module.

Check with `npm config get os`, and reinstall with an explicit target if it is wrong:

```bash
rm -rf node_modules
npm install --os=win32   # or --os=linux, --os=darwin
```

The Docker Compose path is unaffected: the web image builds inside a Linux container.
</details>

---

## Commands

`make help` lists everything. The commands behind it:

| Task | Command |
|---|---|
| Apply migrations | `go run ./cmd/encore-migrate up` |
| Migration status | `go run ./cmd/encore-migrate status` |
| Run the API | `go run ./cmd/encore-api` |
| Run the worker | `go run ./cmd/encore-worker` |
| Run the web client | `cd web && npm run dev` |
| Unit tests | `go test -race ./...` |
| Integration tests | `ENCORE_TEST_DATABASE_URL=... go test -tags=integration -p 1 -timeout=20m ./test/...` |
| Full suite | `make test-all` |
| Coverage report | `make cover` |
| Format | `gofmt -w .` and `cd web && npm run format` |
| Lint | `go vet ./...`, `staticcheck ./...`, `cd web && npm run lint` |
| Instance status | `docker compose run --rm worker /usr/local/bin/encore-worker status` |
| Recover names from retained exports | `docker compose run --rm worker /usr/local/bin/encore-worker backfill-names` |
| Import benchmark | `make bench` (one million records) |
| Build binaries | `go build -o ./bin/ ./cmd/...` |
| Whole stack | `docker compose up -d --build` |

Integration tests need a PostgreSQL. Point `ENCORE_TEST_DATABASE_URL` at one, or leave it unset and
they will start a disposable container through Testcontainers if Docker is available; otherwise they
skip with a clear message rather than failing.

---

## Benchmark

`cmd/encore-bench` generates a synthetic Spotify export, imports it through the real pipeline —
the same code path as a genuine upload — and reports throughput, peak memory and the row counts read
back from the database.

```bash
make bench                                    # 1,000,000 extended-format records
go run ./cmd/encore-bench run --records 250000 --format account_data --report bench.json
```

Measured results, and the hardware they were measured on, are in
[`docs/benchmarks.md`](docs/benchmarks.md).

---

## Documentation

| Document | Contents |
|---|---|
| [`docs/deployment.md`](docs/deployment.md) | Deploying on your own server: reverse proxy, TLS, published images, Portainer |
| [`docs/architecture.md`](docs/architecture.md) | Processes, package layout, dependency rules, request path, scaling |
| [`docs/import.md`](docs/import.md) | Import pipeline, checkpoints, the duplicate strategy, failure recovery |
| [`docs/configuration.md`](docs/configuration.md) | Every environment variable |
| [`docs/operations.md`](docs/operations.md) | Backup, restore, upgrade, routine maintenance |
| [`docs/security.md`](docs/security.md) | Threat model, what is stored and how it is protected |
| [`docs/feature-parity.md`](docs/feature-parity.md) | Item-by-item status against the reference project |
| [`docs/benchmarks.md`](docs/benchmarks.md) | Import benchmark results |
| [`docs/api.md`](docs/api.md) | HTTP endpoint reference |
| [`docs/metadata-fallback.md`](docs/metadata-fallback.md) | Optional second metadata source: when it is used, and the contract for writing one |
| [`docs/attribution.md`](docs/attribution.md) | Relationship to your_spotify, licences |
| [`docs/design/`](docs/design/) | The original implementation plan |

---

## Known limitations

- **Spotify's recently-played endpoint returns at most the last 50 plays and reaches back no further.**
  If Encore is offline for a heavy listening day, that day is gone unless you later import a data
  export covering it. This is a Spotify constraint, not an Encore one.
- **Live sync records a play's length as the track's full duration.** The recently-played endpoint
  reports *what* was played and *when*, but not for how long. Listening time from live sync is
  therefore an upper bound: a track skipped after ten seconds still counts as its whole length.
  Imported history does not have this problem — both export formats carry a real `ms_played` — so a
  period covered by an import is exact. The `source` column records which is which, and re-importing
  an export over a period that was synced live corrects it, because the duplicate rules keep one row
  and the import is the one with the better data.
- **Account-data imports need alias resolution to merge with extended imports.** That export contains
  no track URIs, so Encore resolves each artist/title pair through Spotify's search API in the
  background. Until a pair resolves, its listens are counted under a names-only identity and may
  briefly appear alongside the URI-based ones. See layer 3 in [`docs/import.md`](docs/import.md).
- **Artwork and genres still need Spotify.** Names come from your export, but cover art, artist
  images, genres and release dates do not — those arrive through enrichment, or through a metadata
  source of your own. Until then the pages are readable but plain.
- **Playback control is not implemented.** It would need `user-modify-playback-state` and an active
  device, and it is the one write scope with no read-only equivalent. Intentionally deferred; see the
  parity checklist.
- **`listens` is a single unpartitioned table.** It handles tens of millions of rows comfortably with
  the indexes provided. Beyond that, range partitioning by year would be the next step; it is not
  implemented because no self-hosted instance is close to needing it.
- **Statistics for very wide ranges depend on the daily rollup.** Immediately after a large import the
  rollup is stale and those queries read the fact table instead — correct, but slower for a few
  minutes until the rollup catches up.
- **Encore serves plain HTTP** and expects TLS from a reverse proxy. The bundled nginx does not
  terminate TLS for you.
- **Development-mode Spotify apps are limited to 25 listeners.** Lifting that needs a quota extension
  request to Spotify, which is out of Encore's hands.

---

## Contributing

`make lint && make test` before opening a pull request. CI runs static analysis, unit tests,
integration tests against a real PostgreSQL, a migration up/down/up cycle, the web build, and both
container image builds.

## Licence

MIT — see [`LICENSE`](LICENSE). Not affiliated with Spotify AB.
