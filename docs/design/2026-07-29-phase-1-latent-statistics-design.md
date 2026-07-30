# Phase 1 — Statistics from data already stored

**Date:** 2026-07-29
**Status:** Design / plan of record
**Context:** [Overview and phase map](2026-07-29-spotify-api-expansion-overview.md)

Encore already writes two rich bodies of data that no statistic reads. This phase turns both into
views. It makes **no Spotify call, requests no scope, runs no migration and adds no dependency**,
so it can merge independently of everything that follows.

---

## 1. What is already there and unused

### 1.1 Genres

`artists.genres text[]` (`migrations/00003_catalog.sql:15`) is populated by catalogue enrichment for
every artist Spotify resolves. It is read in exactly one place: `web/src/pages/ArtistDetail.tsx:147`
renders it as a row of chips. No statistic aggregates it.

Alongside it, `artists.popularity`, `artists.followers` and `tracks.popularity` are written,
carried through `internal/httpapi/dto.go:149-150`, and never aggregated either.

### 1.2 Playback context

Eight columns on `listens` describe *how* a play happened:

```
platform  conn_country  reason_start  reason_end
shuffle   skipped       offline       incognito
```

`internal/stats/history.go:103-104` selects all eight for the raw history feed — and they are
dropped before the DTO. Nothing in `internal/httpapi/dto.go` carries them, so the client has never
seen them. No statistic touches them.

---

## 2. The constraint that shapes the phase

Both bodies of data are **partial**, and both partialities are large.

**Genres** exist only where enrichment has resolved the artist. A freshly imported history mints
thousands of local artist rows from export names alone, all with `genres = '{}'`. Genre coverage on
a new instance starts near zero and climbs over hours as the enrichment queue drains.

**Playback context** is written only by the extended-export importer. Sync rows (`source = 0`) and
account-data rows (`source = 1`) carry NULL in all eight columns. For most instances imported
history dominates live sync by orders of magnitude, so the covered fraction is whatever share of
somebody's history came from an extended export — often most of it, sometimes none of it.

A percentage computed over "whichever rows had the column" and presented as a fact is wrong. An
empty genre chart on a fresh instance is indistinguishable from a broken one.

**Therefore: every statistic in this phase returns `covered` and `total` beside its numbers, and
every view states its coverage in prose beneath the chart.** Not a tooltip. A sentence.

### 2.1 Coverage is per column, not per source

An extended export may omit an individual field — older exports and edge-case records do. So the
denominator for each context statistic is **`count(*) FILTER (WHERE <column> IS NOT NULL)`**, never
`count(*) FILTER (WHERE source = 2)`. Keying on the source would quietly overstate the denominator
for any field the export happened not to write.

---

## 3. Statistics

### 3.1 Genres

**Counting rule.** Genres belong to artists, and a track credits one or more artists. A listen
contributes **one play to each distinct genre across all of its artists**. A track whose two
credited artists are both tagged `indie rock` counts once for `indie rock`, not twice.

The dedup is therefore a property of the *track*, not of the listen — every play of a track has the
same genre set — which makes the query a simple join:

```sql
WITH track_genre AS (
    SELECT DISTINCT ta.track_id, g.genre
    FROM track_artists ta
    JOIN artists a ON a.id = ta.artist_id
    CROSS JOIN LATERAL unnest(a.genres) AS g(genre)
)
SELECT tg.genre,
       count(*)::bigint                      AS plays,
       coalesce(sum(l.ms_played), 0)::bigint AS ms
FROM listens l
JOIN track_genre tg ON tg.track_id = l.track_id
WHERE <rangeFilter>
GROUP BY tg.genre
ORDER BY plays DESC, tg.genre
LIMIT $n OFFSET $m
```

**Consequence, stated in the UI:** genre plays sum to more than total plays, because a track counts
toward each of its genres. The alternative — dividing each play by the number of genres — produces
fractional counts nobody can reason about and a "0.31 plays" figure in a tooltip. Whole counts with
an honest label is the better trade.

**Rollup.** Because the join is keyed on `track_id`, the same query runs against
`listen_daily_rollup` when the requested range is provably clean and aligned to local midnight,
substituting `sum(r.plays)` and `sum(r.ms)`. This is exactly how top-artists already works; the
existing helper that decides rollup eligibility is reused unchanged.

**Coverage** is the share of in-range listens whose track has at least one artist with a non-empty
genre array.

**Methods** on `stats.Service`, in a new `internal/stats/genre.go`:

- `TopGenres(ctx, q, userID, r, tz, limit, offset) (GenrePage, error)`
- `GenreTimeline(ctx, q, userID, r, tz, interval, genres []string) ([]GenrePoint, error)` — share
  over time, so taste drift is visible. The caller passes an explicit genre list rather than the
  method choosing one, so the chart's series are stable while paging or re-ranging. The page passes
  the **top eight** for the range; eight is the point at which a stacked area chart stops being
  readable, and the ninth series would be noise.

### 3.2 Taste

A new `internal/stats/taste.go`:

- **Obscurity score** — the play-weighted mean of `artists.popularity` across in-range listens,
  over artists in `metadata_state = 'resolved'` only. Presented as "how mainstream your listening
  is", 0–100, with the population context that Spotify's popularity is itself relative.
- **Release-year lag** — the play-weighted mean of
  `extract(year from played_at) - extract(year from albums.release_date)`. Answers "you listen to
  music that is on average 8.4 years old". Coverage is the share of listens whose album has a
  non-null `release_date`.

Both extend the existing "average album release year" statistic rather than replacing it.

### 3.3 Playback context

A new `internal/stats/context.go`. Each returns its own `covered`/`total`.

**How tracks ended** — a breakdown of `reason_end`, which is richer than a single boolean and needs
no arguable definition. The known values are `trackdone`, `fwdbtn`, `backbtn`, `endplay`, `logout`,
`remote`, `trackerror`, `unknown`; anything unrecognised is grouped as `other` rather than dropped.

**Headline skip rate** is defined as `reason_end = 'fwdbtn'` over rows where `reason_end IS NOT
NULL`. This is a judgement call and is documented as one: the `skipped` boolean is sparse and
inconsistently populated across export vintages, whereas `reason_end` is reliably present. `backbtn`
is deliberately *not* counted as a skip — going back is not the same gesture as skipping forward.

**Shuffle share** — `shuffle = true` over `shuffle IS NOT NULL`.

**Platform breakdown.** `platform` is free text from the export, e.g.
`Android OS 10 API 29 (samsung, SM-G970F)` or `windows 10 ... [transport: ...]`. A
`platformFamily()` classifier in Go maps it to one of Android, iOS, Windows, macOS, Linux, Web
Player, Cast, Partner Device, or Other. The raw string is never discarded — the classifier is
applied at read time, so adding a family later reclassifies history without a backfill. Unmatched
strings land in Other, and a test asserts the real-world samples in the repo's fixtures classify
correctly.

**Country breakdown** — `conn_country`, a two-letter ISO code. Straightforward.

**Offline and incognito share** — two ratios, each over its own non-null denominator.

---

## 4. API

Three endpoints, matching the granular style of the existing `/api/stats/repartition/*`:

| Route | Returns |
|---|---|
| `GET /api/stats/genres` | Top genres for the range, paginated, plus coverage |
| `GET /api/stats/genres/timeline` | Genre share over time; takes `interval` and a repeated `genre` parameter, defaulting to the range's top eight |
| `GET /api/stats/taste` | Obscurity score, release-year lag, each with coverage |
| `GET /api/stats/context` | Reason-end breakdown, skip rate, shuffle share, platform, country, offline, incognito — each with its own coverage |

All four take the standard `from`/`to` range parameters and are subject to the same session and
CSRF middleware as every other statistic. `genres/timeline` additionally accepts `interval`, using
the same values and the same `domain.SuggestInterval` default as `/api/stats/timeline`.

Every coverage-bearing payload uses one shape, so the client renders it with one component:

```json
{ "value": 0.34, "covered": 812043, "total": 1310556 }
```

---

## 5. Reuse

Both new files compose `rangeFilter` and `blacklistFilter` from `internal/stats/stats.go:56,69`
exactly as every existing statistic does. Two consequences fall out for free:

- A blacklisted artist takes its genres with it. There is no second place where the blacklist has to
  be remembered, which is the whole point of that fragment existing.
- Ranges, timezone handling and the half-open `[from, to)` convention are inherited rather than
  re-derived.

Context statistics **cannot** use `listen_daily_rollup` — it is keyed by `(user, day, track)` and
carries no context columns. They always scan the fact table. This is stated in a comment at the top
of `context.go` so it is not later mistaken for an oversight.

---

## 6. UI

**`web/src/pages/Genres.tsx`** — top genres, built on the existing `pages/top/TopList.tsx`
component so ranking, movement and pagination behave identically to the other top lists. Beneath the
list, the coverage sentence and, when coverage is below a threshold, a pointer to Settings where
enrichment progress is already shown.

**`web/src/pages/Habits.tsx`** — "how you listen", carrying the context statistics. Kept separate
from the top lists because it answers a different kind of question: those are *what* you listen to,
these are *how*.

**Dashboard** gains two compact cards: top genres, and the obscurity score. Both link through.

**Empty and low-coverage states are first-class.** A fresh instance mid-enrichment shows "genres
known for 4% of your listening in this range — enrichment is still running", not an empty chart.

---

## 7. Share links

`handleSharedStats` in `internal/httpapi/share.go` gains genres and the obscurity score, called
alongside the existing top-N statistics and bounded by the same `shareTopLimit`.

Skip rate, shuffle share, platform, country, offline and incognito stay **off** a share. Device and
country reveal what hardware somebody owns and where they have been.

No field is added to `ShareLink` and no share gains a toggle, per `internal/domain/share.go:20-24`.

---

## 8. Testing

Following the existing `internal/stats/stats_test.go` patterns, against a real database:

| Test | Asserts |
|---|---|
| Multi-artist dedup | A track whose two artists share a genre contributes one play to it, not two |
| Genre coverage | With a partly-enriched catalogue, `covered` matches the hand-counted number of listens having at least one genred artist |
| Blacklist | Blacklisting an artist removes its genres from the chart, and removes only those a co-credited artist does not also supply |
| Rollup parity | `TopGenres` over a clean aligned range returns identical results from the rollup and from the fact table |
| Per-column denominators | A `source = 2` row with NULL `shuffle` is excluded from the shuffle denominator but still counted in the reason-end denominator |
| Skip definition | `backbtn` is not counted as a skip; `fwdbtn` is |
| Platform classifier | Every platform string in the repo's export fixtures maps to the expected family, and an unrecognised string maps to Other |
| Empty range | Every endpoint returns zeroed structures with `covered = total = 0` rather than an error |

---

## 9. Incidental cleanup

`internal/spotify/models.go:103` declares `PreviewURL`, populated from `preview_url`, which Spotify
removed from track responses on 2024-11-27. It is always empty and referenced nowhere. Delete it.

---

## 10. Not in this phase

- No migration. No new table, no altered table.
- No dependency. Standard library and the existing driver only.
- No Spotify call, no scope change, no consent prompt.
- Genre data for **local** catalogue rows minted from export names. Those artists have no Spotify
  identity, so they have no genres until enrichment matches them. That is a Phase 2 concern at most,
  and arguably not a problem at all.
