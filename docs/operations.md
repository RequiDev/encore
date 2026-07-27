# Backup, restore and upgrade

Encore keeps state in exactly two places:

1. **PostgreSQL** — users, sessions, the music catalogue, every listening record, and all import
   bookkeeping.
2. **`ENCORE_IMPORT_DIR`** — uploaded Spotify export files, kept so an interrupted import can resume
   and a completed one can be re-run.

Plus one secret that is *not* stored anywhere Encore controls:

3. **`ENCORE_ENCRYPTION_KEY`** — without it the Spotify access and refresh tokens in a restored
   database cannot be decrypted, and every user has to reconnect their account. Back it up with your
   database, not next to it.

A database backup alone is enough to recover all listening history. The import directory is
convenience, not data of record: a completed import's records already live in the database.

---

## Backup

### With the compose stack

```bash
# Whole-database logical dump, compressed, to the host.
docker compose exec -T db pg_dump -U encore -d encore --format=custom \
  | gzip > encore-$(date +%F).dump.gz

# Uploaded exports, if you want to keep them.
docker run --rm -v encore_encore-imports:/data -v "$PWD":/backup alpine \
  tar czf /backup/encore-imports-$(date +%F).tar.gz -C /data .
```

`--format=custom` is worth the extra flag: it lets you restore selectively and in parallel, and it
compresses on the way out.

### Without Docker

```bash
pg_dump "$ENCORE_DATABASE_URL" --format=custom --file=encore-$(date +%F).dump
tar czf encore-imports-$(date +%F).tar.gz -C "$ENCORE_IMPORT_DIR" .
```

### What to keep

| Item | Frequency | Why |
|---|---|---|
| Database dump | Daily | The only irreplaceable data. Spotify's recently-played endpoint only reaches back 50 plays, so a lost week is lost permanently. |
| `ENCORE_ENCRYPTION_KEY` | Once, on a change | Losing it costs every user a reconnect but no history. |
| `ENCORE_IMPORT_DIR` | Optional | Only useful for resuming an in-flight import across a restore. |
| `.env` | On a change | Everything else is reproducible from the repository. |

### Verifying a backup

A backup you have not restored is a hypothesis. Test it:

```bash
docker run -d --name encore-restore-test -e POSTGRES_PASSWORD=x -e POSTGRES_USER=encore \
  -e POSTGRES_DB=encore postgres:17-alpine
gunzip -c encore-2026-07-26.dump.gz | \
  docker exec -i encore-restore-test pg_restore -U encore -d encore --clean --if-exists
docker exec encore-restore-test psql -U encore -d encore \
  -c "SELECT count(*) AS listens FROM listens; SELECT count(*) AS users FROM users;"
docker rm -f encore-restore-test
```

---

## Restore

```bash
docker compose stop api worker

gunzip -c encore-2026-07-26.dump.gz \
  | docker compose exec -T db pg_restore -U encore -d encore --clean --if-exists --no-owner

# Bring the schema up to whatever the current binaries expect. Safe to run
# against an already-current database: it is a no-op.
docker compose run --rm migrate

docker compose start api worker
```

Restore `ENCORE_ENCRYPTION_KEY` into `.env` **before** starting the API. If you cannot, users will see
"Spotify connection needs reauthorising" and can fix it themselves with one click; no listening
history is affected.

### After a restore

Check that the application agrees with the database:

```bash
curl -fsS http://localhost:8080/readyz | jq
```

`readyz` reports not-ready while migrations are pending, so a restore of an older dump against newer
binaries is visible rather than mysterious.

An import that was mid-flight when the backup was taken will resume by itself: its lease has long
expired, so a worker re-claims it and continues from the last committed checkpoint. If the uploaded
file is gone (because the import directory was not restored) the job fails with `file_unreadable`, and
the records it had already committed are still there — re-upload and re-import is safe, because
re-importing is idempotent.

---

## Upgrade

Encore's migrations are **forward-only** and additive. The supported path is:

```bash
git pull
docker compose build
docker compose run --rm migrate     # applies pending migrations, then exits
docker compose up -d
```

The compose stack already encodes this ordering: `api` and `worker` declare
`depends_on: migrate: condition: service_completed_successfully`, so a plain
`docker compose up -d --build` does the right thing on its own.

### Zero-downtime notes

- Migrations take a PostgreSQL session-level advisory lock, so starting several replicas at once
  applies each migration exactly once. Without it two processes issue the same `CREATE TABLE` and one
  fails with a duplicate-key error against a system catalogue, which reads like corruption rather than
  a race. `TestConcurrentMigrateIsSerialised` guards it.
- `/readyz` fails while migrations are pending, which is what you want a load balancer to see.
- Stopping a worker mid-import is safe at any moment: the lease expires and another worker resumes
  from the checkpoint. There is no drain step, and no import state lives in process memory.

### Rolling back

Migrations have `down` sections, exercised in CI, but rolling *back* a production database is not a
supported upgrade path — a down migration that drops a column loses the data in it. To go back a
version, restore the dump you took before the upgrade and check out the matching tag.

Always take a dump before upgrading:

```bash
docker compose exec -T db pg_dump -U encore -d encore --format=custom > pre-upgrade.dump
```

---

## What is this instance doing?

Most of the answer is in the browser. **Settings → Music metadata** shows how much of the catalogue
has a name, how much is still queued, and — when it applies — that Spotify has rate limited the
instance and when it resumes. It refreshes itself while work is outstanding and stops once there is
nothing left to fetch. That panel is enough for the question people actually ask, and it needs no
shell access, so point users at it rather than at a terminal.

For the fuller picture — listens held, users, per-user sync state, import jobs — one command,
read-only, safe at any time:

```bash
docker compose run --rm worker /usr/local/bin/encore-worker status
```

It reports how many listens are held, how much of the catalogue has been resolved and how much is
still queued, whether Spotify is currently rate limiting the application and until when, each user's
sync state, and the import jobs. It is the first thing to run when something looks wrong, and it
answers the two most common questions — "are the blank artists ever going to fill in?" and "is
anything actually happening?" — without knowing the schema.

## Troubleshooting

### `502 Bad Gateway` from nginx on every `/api` request

The web container reaches the API by name over the Compose network. If nginx has cached a stale
address, every API call fails while the API itself is perfectly healthy — `docker compose ps` shows
everything up, `curl http://localhost:8080/healthz` works, and only requests through port 3000 fail.

Confirm it by comparing what nginx tried against where the API actually is:

```bash
docker compose logs web | grep 'connect() failed'    # shows the upstream address nginx used
docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' encore-api-1
```

Encore's nginx configuration resolves the upstream per request precisely so this cannot happen, and
`go test ./test/deploy/...` guards it. If you have edited `deploy/nginx.conf` and reintroduced a
literal hostname in `proxy_pass`, that test will tell you. The immediate unblock is
`docker compose restart web`; the fix is to name the upstream through a variable.

### The API answers `404` for calls that should work

If `proxy_pass` names the upstream through a variable, nginx stops forwarding the request URI on its
own and every call arrives at the upstream's root. The `/api/` location must end in `$request_uri`.
This is also covered by `go test ./test/deploy/...`.

### Tracks and artists have no names, and the log says `spotify daily quota exhausted`

A Spotify application starts in **development mode**, whose daily request quota is small and
undocumented. Exhausting it does not earn a short pause: Spotify answers `429` with
`"reason":"QUOTA_EXCEEDED"` and a `Retry-After` of most of a day, and Encore honours it — continuing
to ask would only extend the ban.

```bash
docker compose logs worker | grep -i 'quota exhausted'
```

**It recovers on its own.** The worker waits out the `Retry-After` and then drains the queue; a
sixteen-thousand-track backlog takes a few minutes once the quota resets. Nothing is lost and nothing
needs restarting — in fact restarting used to make it worse, because the pause lived only in memory
and a fresh process would immediately spend requests against a quota that had not reset. It is now
recorded in `app_settings` and restored at startup, so a restart simply waits out the remainder:

```bash
docker compose logs worker | grep -i 'stays paused'   # after a restart, if still banned
```

Users see the same thing without a shell: **Settings → Music metadata** shows a *Rate limited* chip
and the instant fetching resumes, and says in as many words that the listening figures are already
complete. It is worth pointing people there before they go restarting containers.

**Signing in keeps working throughout.** Authentication draws on a rate budget of its own, and the
token exchange does not even use the same Spotify service — it is `accounts.spotify.com`, which never
rate limited anything. A quota exhausted by enrichment on `api.spotify.com` has no bearing on it.
Only these are held back by a pause:

| Held back | Unaffected |
|---|---|
| Metadata enrichment: names, artwork, genres | Signing in and existing sessions |
| Background recently-played polling | Imports, and every statistic, chart and export |
| A manual **Sync now**, which is refused with an explanation rather than left to hang | Everything already in the database |

Listening data is never affected; only the names, artwork and genres are. What to do:

1. Keep `ENCORE_SPOTIFY_RATE_LIMIT` low. The default of 2/s is deliberate — a sixteen-thousand-track
   backlog is only about three hundred requests, so speed buys nothing and risks a day of blank
   metadata.
2. Recover the track names immediately, without Spotify, from the exports you already uploaded:

   ```bash
   docker compose run --rm worker /usr/local/bin/encore-worker backfill-names
   ```

   Both export formats print the track title, the artist and the album beside the URI, so this fills
   in every track a retained import references **and builds the artist and album catalogue** from
   those names — see §10 of [import.md](import.md). It touches nothing but empty names and
   uncredited tracks — no listens, no job state, no counters — and is safe to run twice.

   Run it after upgrading to a version that keeps these names: a history imported earlier has no
   artists at all, and this is what recovers them without re-importing. Artwork, genres and release
   dates still need Spotify and arrive on their own once the quota resets.
3. If you have many users, apply for
   [extended quota mode](https://developer.spotify.com/documentation/web-api/concepts/quota-modes).

### A user cannot sign in and the browser shows `INVALID_CLIENT: Invalid redirect URI`

`ENCORE_PUBLIC_URL` and the redirect URI registered in the Spotify dashboard have to match exactly,
including scheme and port. Encore logs the one it will use at startup:

```bash
docker compose logs api | head -1 | grep -o '"spotify_redirect":"[^"]*"'
```

## Routine maintenance

Encore does its own housekeeping — expired sessions and OAuth states are reaped by the worker, and
statistics rollups are recomputed continuously — but PostgreSQL benefits from the usual attention on
a large history:

```sql
-- After a very large import, refresh the planner's statistics so the top-N
-- queries pick the right plan.
ANALYZE listens;

-- Once a year, or after deleting a user with a lot of history.
VACUUM (ANALYZE) listens;

-- Where the time is going.
SELECT relname, pg_size_pretty(pg_total_relation_size(relid)) AS total
FROM pg_catalog.pg_statio_user_tables ORDER BY pg_total_relation_size(relid) DESC LIMIT 10;
```

Autovacuum handles the day-to-day case; an explicit `ANALYZE listens` immediately after importing a
million records is the one manual step actually worth doing, because the table's shape changes
dramatically in a few minutes.

### Reclaiming disk

Completed imports keep their uploaded files by default so a job can be re-run. Set
`ENCORE_IMPORT_RETAIN_FILES=false` to delete each file once its job has been verified, or clean up by
hand:

```bash
docker compose exec api sh -c 'du -sh /var/lib/encore/imports/*'
```

Deleting an import job from the UI removes its files and its bookkeeping, but **not** the listening
records it created: those are the user's history, and `listens.import_file_id` is set to `NULL` rather
than cascading.
