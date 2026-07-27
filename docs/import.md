# Import design and failure recovery

Importing history is the hardest thing Encore does. A Spotify extended-streaming-history export for a
long-standing account is routinely hundreds of megabytes and can exceed a million records, and it
arrives on a home server that may be rebooted, run out of disk, or lose its database connection
halfway through. This document describes how Encore stays correct through all of that.

The two properties everything else follows from:

1. **Committed records never exceed the checkpoint.** A batch of listens and the checkpoint that
   describes it are written in the same database transaction. Either both survive a crash or neither
   does.
2. **Ingestion never calls the Spotify API.** A listening record is stored with whatever identity the
   export gave it. Catalogue metadata is filled in later by a separate subsystem. A Spotify outage,
   a rate limit, or an expired token cannot lose or delay a single listen.

---

## 1. Supported inputs

| Input | Detected as | Notes |
|---|---|---|
| `Streaming_History_Audio_2015-2017_0.json` | extended | The current name for the extended export |
| `endsong_0.json` | extended | The name Spotify used until roughly 2022 |
| `StreamingHistory0.json` | account data | The one-year "Account data" export |
| `StreamingHistory_music_0.json` | account data | Newer name for the same format |
| `my_spotify_data.zip` | archive | Opened directly; each entry is detected on its own |
| `*.json.gz` | either | Transparently decompressed |
| NDJSON / concatenated objects | either | Accepted even though Spotify does not produce it |

Everything else inside an export — `Playlist1.json`, `SearchQueries.json`, `Userdata.json`,
`YourLibrary.json`, `Marquee.json`, the PDF read-me — is recognised and **skipped**, not treated as a
failure. Uploading the whole zip is the supported happy path.

Format detection prefers the file's *content* over its name, because people rename files. The first
4 KiB is sniffed for the characteristic keys of each format (`ms_played` and `spotify_track_uri` for
extended, `endTime` and `msPlayed` for account data).

### Field-level tolerance

Spotify's export format has changed repeatedly and is not self-consistent. The parser accepts, for
every field where it has been observed in the wild:

- `ms_played` as a number **or** a string.
- `offline_timestamp` as `null`, `0`, a number, or a string.
- `shuffle`, `skipped`, `offline`, `incognito_mode` as `null`, a boolean, or the strings `"true"` /
  `"false"`. A `null` stays `nil` and is stored as SQL `NULL`; it is never coerced to `false`,
  because "Spotify did not tell us" and "the user was not shuffling" are different facts.
- `skipped` carrying an old-style string such as `"trackdone"`, which is treated as unknown.
- Either `ip_addr_decrypted` (pre-2024) or `ip_addr` (2024 onward). Encore reads neither into the
  database; both are deliberately discarded.
- `username` and `user_agent_decrypted`, present only in older exports, likewise discarded.

Podcast episodes (`episode_name`, `spotify_episode_uri`) and audiobook chapters (`audiobook_*`) are
**skipped**, not rejected: they are valid records that are simply not music.

---

## 2. Pipeline

```
POST /api/imports  (multipart)
      │  streamed straight to durable storage, never buffered in memory
      ▼
 ENCORE_IMPORT_DIR/<job>/<file>         + SHA-256 computed while streaming
      │
      ▼
 import_jobs (queued) ── import_files (pending, format detected, sha256)
      │
      ▼  worker claims the job:
      │  UPDATE ... WHERE status='queued' OR (status='running' AND lease_expires_at < now())
      │  ORDER BY created_at LIMIT 1 FOR UPDATE SKIP LOCKED
      │  (+ heartbeat renews the lease every LeaseTTL/3)
      ▼
 for each file:
      open → seek to checkpoint → stream-decode one JSON element at a time
                │
                ├─ malformed         → import_rejects (index, reason, excerpt); continue
                ├─ podcast/too short → counted as skipped; continue
                └─ valid             → accumulate into a batch of ENCORE_IMPORT_BATCH_SIZE
                                              │
                                              ▼  ONE transaction:
                                  1. resolve known aliases for this batch
                                  2. upsert unseen track ids   → tracks(metadata_state='pending')
                                  3. upsert unseen name pairs  → track_aliases(state='pending')
                                  4. INSERT ... SELECT into listens with the dedupe rules
                                  5. mark affected local days dirty for rollups
                                  6. UPDATE import_files SET record_offset, byte_offset, counters
                                  COMMIT
      EOF → import_files.status = 'completed', records_total set
      │
      ▼
 verification (see §6) → import_jobs.status = 'completed'  or  'failed'
```

Metadata enrichment runs entirely outside this picture, on its own schedule, against the `pending`
rows that ingestion created.

**Names are kept at ingest time.** Both export formats print the track title and artist beside the
URI, so the importer records the title on the `tracks` row it creates and the names on the listen
itself. The row still goes into the enrichment queue for its album, artwork, duration and genres —
but a freshly imported history is readable straight away instead of only once several hundred Spotify
requests have completed. That matters more than it sounds: a development-mode application can exhaust
its daily quota, and without this the entire library would show as blank for a day.

For a history imported before this existed, `encore-worker backfill-names` reads the names back out
of the retained export files. It is not a re-import — a completed job could never verify a second
time, because its listens already exist and would all count as duplicates.

---

## 3. Streaming and memory

The reader is an `encoding/json.Decoder` driven token by token: consume the opening `[`, then
`Decode` one element at a time into a `json.RawMessage`. Nothing ever holds more than one record plus
the current batch.

Peak resident memory is therefore **O(batch size × record size)** and is independent of file size. A
128 GiB export and a 1 MiB export use the same memory.

> **Documented target: the import worker stays below 256 MiB RSS for any input size at the default
> batch size of 1000.**

Measured figures for a one-million-record synthetic export are in [§9](#9-benchmark) and are produced
by `go run ./cmd/encore-bench run`, so you can reproduce them on your own hardware.

### Backpressure

There is no in-memory queue between the reader and the database. The reader calls the flush
synchronously; the flush waits for a pooled connection; the pool is bounded by
`ENCORE_DATABASE_MAX_CONNS`. When PostgreSQL is slow, the flush is slow, the reader blocks, and file
reading stops. Memory cannot grow in response to a slow database, because the only thing that could
grow is the batch, and the batch is fixed size.

This is deliberate. A channel between reader and writer would have been faster in the common case and
would have made memory unbounded exactly when the database is struggling — which is precisely when
the machine can least afford it.

---

## 4. Checkpoints and resume

Each `import_files` row carries:

| Column | Meaning |
|---|---|
| `record_offset` | Records fully accounted for. Always maintained. |
| `byte_offset` | Decoder input offset after the last accounted record. `NULL` for non-seekable sources. |
| `imported`, `duplicates`, `skipped`, `rejected` | Running counters. |

All of these are updated **in the same transaction as the batch they describe**. That gives the
invariant stated at the top: the database can never hold a checkpoint claiming progress that was not
committed. The update is guarded by `WHERE record_offset < $new` — strictly less than, because every
batch advances the offset by at least one record. Requiring real progress makes the statement
idempotent as well as monotonic: a stale retry cannot wind the checkpoint backwards, and a batch whose
commit acknowledgement was lost adds its counters once rather than twice when it is retried.

**Resume** takes one of two paths:

- **Seekable source** (a plain or gzip-free file on disk): seek to `byte_offset`, skip whitespace,
  consume one optional `,`, and carry on decoding array elements. This is O(1) regardless of how far
  in the crash happened.
- **Non-seekable source** (a `.gz` stream, an entry inside a `.zip`): reopen from the start and decode
  and discard `record_offset` elements. This is O(n) but needs no decompression state to be
  persisted, and decode-and-discard is far cheaper than the full ingest path.

Because insertion is idempotent, re-processing the records between the last checkpoint and the crash
is harmless — at worst they are counted again as duplicates, and §6's verification is written to
tolerate that.

**Crash recovery needs no special code path.** A worker that dies stops renewing its lease. After
`ENCORE_IMPORT_LEASE_TTL` the job matches the claim query again and any worker picks it up from the
checkpoint. `Heartbeat` returns false if another worker has stolen the lease, at which point the
original worker stops immediately rather than writing over someone else's progress.

A *graceful* shutdown is faster still. A worker being stopped finishes its in-flight batch and parks
the job as `paused`, which the claim query also treats as a candidate: nobody is working on it and
nothing is wrong with it, so there is no reason to wait out a lease the departing worker already knew
it would not renew. A job the **user** stopped becomes `cancelled`, not `paused`, and is deliberately
not a candidate — as is any job with `cancel_requested` set, so a cancellation cannot be undone by
another worker picking the job straight back up.

---

## 5. Duplicate prevention

Three layers. Each exists because the layer above it cannot cover a specific case.

### Layer 1 — exact key, enforced by the database

```
identity_key = sha256("t:" ‖ spotify_track_id)                       when a track URI is known
             = sha256("n:" ‖ norm(artist) ‖ 0x00 ‖ norm(title))      otherwise

dedupe_key   = sha256(user_id ‖ identity_key ‖ floor(unix(played_at) / 60))

UNIQUE (user_id, dedupe_key)
```

`played_at` is the **start** of playback, normalised across the three sources, because that is the
only anchor all three can agree on:

| Source | Reports | Normalised as |
|---|---|---|
| `/me/player/recently-played` | `played_at` | used directly |
| Extended streaming history | `ts` (stream end, second precision) | `ts − ms_played` |
| Account data | `endTime` (stream end, minute precision) | `endTime − msPlayed` |

`norm()` is NFKC composition, lowercasing, apostrophe folding, punctuation-to-space, whitespace
collapsing, and removal of *edition* suffixes such as `- Remastered 2011` or `(Deluxe Edition)`.
Markers that denote a genuinely different recording — live, remix, acoustic, instrumental, demo — are
deliberately **not** stripped; merging those would silently corrupt the statistics.

The consequence: **re-importing the same file inserts exactly zero rows.** This is enforced by a
`UNIQUE` constraint, not by application logic, so it holds even if two workers race.

`DedupeBucketSeconds` is 60 because that is the coarsest precision Encore ingests. Changing it
invalidates every stored key and would need a migration that recomputes them.

### Layer 2 — cross-source fuzzy suppression

Layer 1 fails when the *same* event arrives from two sources that disagree about the exact second: an
account-data `endTime` is truncated to the minute, so `endTime − msPlayed` can be up to 59 seconds
away from the extended export's answer, landing in a different bucket.

So before inserting, the batch is anti-joined against what is already stored:

```sql
NOT EXISTS (
  SELECT 1 FROM listens l
  WHERE l.user_id     = incoming.user_id
    AND l.identity_key = incoming.identity_key
    AND l.played_at BETWEEN incoming.played_at - interval '60 seconds'
                        AND incoming.played_at + interval '60 seconds'
    AND l.source <> incoming.source
    AND abs(extract(epoch FROM (l.played_at - incoming.played_at))) <= GREATEST(
          CASE l.ts_precision        WHEN 2 THEN 60 ELSE 10 END,
          CASE incoming.ts_precision WHEN 2 THEN 60 ELSE 10 END)
)
```

The constant 60-second `BETWEEN` is what the `(user_id, identity_key, played_at)` index can drive; the
precise per-precision tolerance is applied afterwards on the handful of candidate rows.

**The window only applies across different sources.** Within one source, the exact key is
authoritative. That matters: a listener who plays a track, skips it after three seconds and plays it
again has produced two genuine listens three seconds apart, and a same-source window would silently
delete one of them.

Duplicates *within* a single incoming batch are collapsed first with `DISTINCT ON (user_id,
dedupe_key)`, keeping the longer play, so one malformed file cannot trip the unique constraint on
every row.

### Upgrading a weaker record

Suppression keeps whichever row arrived first, with one deliberate exception. The recently-played
endpoint reports *what* was played and *when* but not for how long, so a synced listen records the
track's full duration. When an export later describes the same event, it carries a real `ms_played`
and the surrounding playback context, and it is simply the better record.

So an incoming *import* record that matches an existing *sync* record updates it in place: `ms_played`,
`ts_precision`, `source` and the context columns are taken from the export, while the row keeps its id
and its dedupe key. No row is inserted, and the record is still counted as a duplicate. Without this,
listening time would stay permanently overstated for every period that happened to be synced live.

It converges: a second run of the same import finds the row already carrying the export's source and
changes nothing.

### Layer 3 — relink reconciliation

An account-data export has no track URIs, so its listens start with a names-only identity. If the
equivalent extended-export listen is already stored under a URI-based identity, layers 1 and 2 cannot
see that they are the same event: the identity keys differ.

Encore closes that gap over time. Every distinct `(artist, title)` pair from a names-only import
becomes a row in `track_aliases`. The alias resolver looks each pair up through Spotify's search API,
rate-limited and in the background. When a pair resolves:

1. Every listen carrying the old names-only identity is found through the partial index
   `listens (identity_key) WHERE track_id IS NULL` — not user-scoped, so one lookup repairs every
   user's history at once.
2. Each row's `track_id`, `identity_key` and `dedupe_key` are recomputed for the resolved track.
3. A row whose new key collides with an existing listen is **deleted** and counted as a reconciled
   duplicate: the collision proves the same event was already recorded through a source that knew the
   URI.
4. Surviving rows are updated in place, keeping `alias_artist` / `alias_title` as provenance.

The importer also consults already-resolved aliases *before* computing keys for each batch, so the
common case — importing account data after the aliases are known — converges immediately rather than
needing the relink pass at all.

**Ordering does not matter.** Extended-then-account-data, account-data-then-extended, both twice, and
either interleaved with live sync all converge on the same set of listens.

---

## 6. Failure taxonomy

| Class | Examples | Handling |
|---|---|---|
| **Transient** | database connection reset, deadlock, serialisation failure, `57P03` shutdown, network blip | The batch is retried with bounded exponential backoff and full jitter (`ENCORE_IMPORT_BATCH_RETRIES`, default 6). The checkpoint is untouched, so a retry re-reads from a consistent point. |
| **Permanent record error** | missing `ts`, unparseable date, non-numeric `ms_played`, no usable track identity | The record is written to `import_rejects` with its index, a stable reason code, and a truncated excerpt. The import continues. Rejects never fail a job. |
| **Intentional skip** | podcast, audiobook, `spotify:local:` file, play shorter than `ENCORE_IMPORT_MIN_MS` | Counted as `skipped`. Not an error. |
| **Job-level failure** | file unreadable, content is not JSON at all, retries exhausted, verification failed | The job is marked `failed` with a stable `error_code` and a human-readable message. The checkpoint survives, so `POST /api/imports/:id/retry` resumes rather than restarting. |

Rejected diagnostics are capped at `ENCORE_IMPORT_MAX_REJECTS_PER_FILE` (default 1000) so one
pathological export cannot fill the disk; the *count* keeps rising past the cap.

---

## 7. Counters and honest completion

Every processed record lands in exactly one bucket:

```
imported + duplicates + skipped + rejected == records processed
```

The API exposes, per file and summed per job: `imported`, `duplicates`, `skipped`, `rejected`,
`pending` (`records_total − record_offset`, or `null` while the total is still unknown), and `failed`.

**A job is never reported successful on the strength of its own counters.** Before a job may become
`completed`, `domain.VerifyJob` asserts:

1. every file is `completed` or `skipped` — none left `pending`, `running` or `failed`;
2. each file's counters account for exactly the records its checkpoint says it processed;
3. each file reached its record total, where the total is known;
4. **the number of listens actually in the database for each file equals the number the importer
   claims it inserted** — a real `SELECT count(*) FROM listens WHERE import_file_id = ...`, not a
   running tally.

Assertion 4 is the one that matters. It is what catches the failure mode where a job looks finished
but its rows were never committed — a lost transaction, a restored-from-backup database, a
hand-edited status. When it fails, the job becomes `failed` with `error_code = verification_failed`
and a message naming the offending file and the shortfall. It is never silently reported as a success.

There is a regression test for exactly this: it forges a `completed` job whose rows have been deleted
underneath it and asserts the job is reported as failed.

---

## 8. Cancellation and retry

`POST /api/imports/:id/cancel` sets `cancel_requested`. The worker observes it **at a batch
boundary**, so the in-flight transaction always completes first and the checkpoint stays consistent.
The job becomes `cancelled` and the lease is released. Records already committed are kept — they are
the user's listening history, not scratch state.

`POST /api/imports/:id/retry` moves a `failed`, `cancelled` or `paused` job back to `queued`, clears
the error fields, and resets any `failed` or `running` file to `pending` **without touching
`record_offset`, `byte_offset` or the counters**. The next worker to claim it carries on from where
the last one stopped.

---

## 9. Benchmark

`cmd/encore-bench` generates a synthetic export, imports it through the real pipeline, and reports
throughput, peak memory and the resulting row counts read back from the database.

```bash
make bench                     # one million extended-format records
# or, directly:
ENCORE_DATABASE_URL=postgres://encore:encore@localhost:5432/encore?sslmode=disable \
  go run ./cmd/encore-bench run --records 1000000 --format extended --report bench.json
```

Measured results are recorded in [`docs/benchmarks.md`](benchmarks.md), which the benchmark command
regenerates.

---

## 10. What the export already tells us

An import is not a stream of ids. Both formats print the names beside them, and Encore keeps all
three:

| Field | Extended | Account data | Becomes |
|---|---|---|---|
| Track title | `master_metadata_track_name` | `trackName` | `tracks.name` |
| Artist | `master_metadata_album_artist_name` | `artistName` | a **local** `artists` row |
| Album | `master_metadata_album_album_name` | — | a **local** `albums` row |

Neither format identifies an artist or an album. There is a `spotify_track_uri` and nothing else, so
those two cannot be keyed the way the catalogue keys everything else. Encore mints an id from the
**normalised name** instead:

```
local:artist:<16 hex of sha256(name_norm)>
local:album:<16 hex of sha256(artist_norm ‖ NUL ‖ album_norm)>
```

Deriving the id from the name is what makes the row stable: the same artist in another year's file,
in a second export, or in a re-import lands on the same row and the same statistics. Albums are keyed
on the artist as well as the title because album titles collide badly — a history of any size holds
several unrelated *Greatest Hits*.

A colon cannot occur in a base-62 Spotify id, so the two kinds are unmistakable in the database, in a
URL and in a log — and the client's id filter rejects local ids on sight, so one can never be sent to
an endpoint that would only answer 400 for it.

These rows are marked `metadata_state = 'local'`, which the enrichment queues do not claim. They are
a **floor, never a ceiling**:

- a track that already has credits keeps them; enrichment's answer is authoritative
- a track that already has an album keeps it
- a name is only ever written into an empty one

When enrichment later resolves a Spotify artist whose normalised name matches a local row, the local
row's credits — including which users had hidden it — move to the Spotify row and the local row is
deleted, in the same transaction. Without that fold the same name would appear twice in every chart
with the plays split between them. Local albums need no fold: a resolved track takes its real
`album_id` from the upsert and leaves the local row orphaned.

The effect on a real 144,000-record export: **3,482 named artists and 8,899 named albums, with every
listen credited, before a single Spotify request is made.** Previously that catalogue was empty until
enrichment drained — which on a development-mode application whose daily quota is exhausted meant
indefinitely.

What is still missing without Spotify: artwork, genres, popularity, release dates and durations.

## 11. Enrichment, kept separate on purpose

Ingestion writes `tracks` rows in the `pending` state, plus the local rows above. Four background
workers then fill them in:

| Worker | Source | Batch | Token |
|---|---|---|---|
| Tracks | `GET /v1/tracks?ids=` | 50 | client credentials |
| Albums | `GET /v1/albums?ids=` | 20 | client credentials |
| Artists | `GET /v1/artists?ids=` | 50 | client credentials |
| Aliases | `GET /v1/search` | 1 | client credentials |

Using a **client-credentials** token rather than a user's token means enrichment works with zero users
connected and does not spend a listener's own rate budget.

Work is claimed with `FOR UPDATE SKIP LOCKED` and an advancing `next_attempt_at`, so several workers
never fetch the same id. Failures increment `fetch_attempts` and push `next_attempt_at` out
exponentially; after `domain.BackoffAttempts` (6) the row is parked as `failed` and revisited by the
repair job every `ENCORE_ENRICH_REPAIR_INTERVAL`. An id Spotify authoritatively has nothing for
becomes `unavailable`.

On HTTP 429 the client pauses **globally** for the duration of `Retry-After` rather than letting each
goroutine back off separately, which is what stops a rate limit turning into a thundering herd.

Nothing in this section can affect a listening record. If it were all switched off permanently, every
import would still complete, and every statistic that does not need a track name would still be
correct.
