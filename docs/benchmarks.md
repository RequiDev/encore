# Import benchmark results

Encore's headline import claim is that memory is a function of the batch size and not of the file
size. `cmd/encore-bench` exists to check that claim rather than to assert it: it generates a synthetic
export, imports it through the **real** pipeline — the same `importer.Intake` the HTTP handler uses and
the same `importer.Runner` the worker runs — and then reads the row counts back out of the database.

Reproduce any of this with:

```bash
export ENCORE_DATABASE_URL='postgres://encore:encore@localhost:5432/encore?sslmode=disable'
go run ./cmd/encore-migrate up
go run ./cmd/encore-bench run --records 1000000 --format extended --seed 20260726 --report bench.json
```

---

## Headline result: one million records

| | |
|---|---|
| **Dataset** | 1,000,000 extended-streaming-history records, **522.7 MiB** (548 B per record) |
| **Catalogue** | 5,000 tracks by 360 artists, Zipf-skewed, spread over ten years |
| **Elapsed (import)** | **2 min 03 s** |
| **Throughput** | **8,117 records/s** (7,564 committed rows/s, 4.2 MiB/s) |
| **Peak heap (`HeapAlloc`)** | **8.0 MiB** |
| **Peak `Sys` (process memory bound)** | **22.5 MiB** |
| **Documented budget** | 256 MiB |
| **Rows committed** | **931,904** listens, verified by `SELECT count(*)` |
| **Batches** | 1,000 transactions, 0 retried, 0 failed |
| **Batch latency** | 113 ms mean, 3.69 s slowest |
| **Verdict** | **PASS** — job `completed`, verification passed |

### Full report

```
Dataset
  format      extended
  records     1,000,000
  bytes       522.7 MiB (548 B per record)
  covers      2016-07-24 to 2026-07-25
  catalogue   5,000 tracks by 360 artists
  not music   24,808 podcast, 14,885 local-file, 28,880 below-minimum plays
  batch size  1000
  seed        20260726

Throughput
  generate        1.416s
  spool (intake)  736.557ms
  import          2m3.2s
  records/s       8,117
  rows/s          7,564
  bytes/s         4.2 MiB

Memory during the import (2466 samples, every 50ms)
  peak heap (HeapAlloc)  8.0 MiB
  peak Sys (RSS bound)   22.5 MiB
  allocated in total     6.4 GiB
  GC cycles              2,304

Batches (one transaction each)
  committed           1,000
  retried             0
  failed              0
  mean latency        113.545ms
  slowest             3.694s
  bytes checkpointed  522.7 MiB

Importer counters
  imported    931,904
  duplicates  0
  skipped     68,096
  rejected    0
  processed   1,000,000

Rows read back from the database
  listens for this job     931,904
  listens for this user    931,904
  resolved to a track      931,904
  distinct tracks          5,000
  distinct name pairs      0
  tracks in the catalogue  5,001
  played between           2016-07-24 and 2026-07-25

Result
  status                         completed
  verified against the database  yes
  peak heap limit                256 MiB
  verdict                        PASS
```

The 68,096 skipped records are the podcasts, local files and sub-second plays the generator mixes in
deliberately, so the skip paths carry real traffic rather than being exercised only by unit tests.
`imported + skipped = 1,000,000` exactly, which is the counter identity post-import verification
asserts.

---

## Smaller run, for comparison

| Records | File size | Import time | Records/s | Peak heap | Rows committed |
|---:|---:|---:|---:|---:|---:|
| 25,000 | 13.0 MiB | 2.6 s | 9,600 | 6.7 MiB | 23,298 |
| 1,000,000 | 522.7 MiB | 123.2 s | 8,117 | 8.0 MiB | 931,904 |

**The file grew by 40× and peak heap grew by 1.3 MiB.** That is the property the design was built for,
and it is the reason the importer flushes synchronously instead of feeding a channel: there is no
queue that can grow when the database is slower than the reader.

---

## Hardware

Measured on the development machine:

- Windows 11, AMD64
- PostgreSQL 17 (Alpine) in Docker Desktop, default configuration, no tuning
- The importer, the database and the generator all competing for the same host

Running PostgreSQL through Docker Desktop's virtual machine on Windows is close to the worst realistic
case for write throughput. A Linux host with the database on local storage should comfortably exceed
these figures; the memory numbers will not change, because they do not depend on the database at all.

## How to read the numbers

- **Peak heap** is `runtime.MemStats.HeapAlloc`, sampled every 50 ms during the import. It is what the
  256 MiB budget in [`docs/import.md`](import.md) refers to.
- **Peak Sys** is `runtime.MemStats.Sys`, the memory the runtime has obtained from the operating
  system. It is the closest in-process bound on RSS and is the number to compare against a container
  memory limit.
- **Allocated in total** (6.4 GiB) is cumulative churn, not residency. Almost all of it is
  short-lived per-record decoding that the garbage collector reclaims immediately; it is reported
  because a sudden change in it would signal an allocation regression that peak heap alone might hide.
- **Slowest batch** (3.69 s against a 113 ms mean) is backpressure working as intended. When
  PostgreSQL checkpoints, a transaction waits; because the flush is synchronous, the reader waits too,
  and memory does not move.

## Choosing a batch size

`ENCORE_IMPORT_BATCH_SIZE` trades memory for round trips. At the default of 1000 a batch holds roughly
half a megabyte of staged rows, which is why peak heap sits in single-digit megabytes.

| Batch size | Round trips for 1M records | Approximate batch residency |
|---:|---:|---:|
| 250 | 4,000 | ~0.15 MiB |
| 1,000 (default) | 1,000 | ~0.5 MiB |
| 5,000 | 200 | ~2.5 MiB |

Raising it helps most when the database is remote and latency dominates. It does **not** help on a
local socket, where the 113 ms mean batch latency is dominated by the insert itself. Lowering it makes
checkpoints finer, which shortens the replay window after a crash.

## Related test coverage

The benchmark is a measurement, not a test. The corresponding assertions live in the suite and run in
CI:

- `test/importtest.TestLargeSyntheticHistoryStaysWithinMemoryBudget` — imports 200,000 records,
  samples the heap throughout, and **fails** if peak heap exceeds 256 MiB.
- `test/importtest.TestWorkerRestartResumesFromCheckpoint` — kills a worker mid-import and asserts
  that resuming loses and duplicates nothing.
- `test/importtest.TestDatabaseInterruptionDuringImport` — terminates Encore's database backends
  mid-import and asserts recovery to exact counts.
- `test/importtest.TestForgedCompleteJobFailsVerification` — deletes committed rows behind a
  finished job's back and asserts it is reported failed rather than successful.
