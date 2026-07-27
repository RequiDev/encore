# Metadata fallback

Encore describes your listening with data from Spotify: track titles, artist names, album artwork,
genres, durations. Two things get in the way of that on a self-hosted instance.

**The quota.** A Spotify application starts in *development mode*, whose daily request quota is small
and undocumented. A first import of a real listening history exhausts it within the hour, and Spotify
answers an exhausted quota with a `Retry-After` of most of a day. Encore honours it — see
[operations](operations.md) — so the listens are all there and the names arrive tomorrow.

**The gaps.** Some ids Spotify will not serve at any rate limit. A track delisted since you played
it, a regional removal, a relink to something that no longer exists: Spotify returns `null` and
Encore marks the row `unavailable`, which is terminal. Those never fill in, however long you wait.

A metadata fallback addresses both. You point Encore at a second HTTP endpoint that speaks the same
shape Spotify does, and Encore consults it when — and only when — Spotify cannot help.

Encore ships no fallback and recommends none. It ships the interface. **What you serve from your own
endpoint, and whether you hold the rights to serve it, is your business and not Encore's.**

## When the fallback is consulted

| Situation | What happens |
|---|---|
| Spotify is rate limiting the instance | The whole batch goes to the fallback. Spotify is not called at all — its limiter *blocks* rather than failing, so asking would stall the batch for the length of the pause. |
| Spotify answered, but had nothing for some ids | Those ids, and only those, go to the fallback. |
| Spotify answered everything | The fallback is not called. |
| The fallback is unreachable | Logged; Spotify's answer stands. An instance whose fallback is down behaves like one that never had a fallback. |
| Spotify failed for some other reason | The batch fails and retries on the usual backoff, as it did before. |

One rule governs the whole design, and it is worth stating on its own:

> An id is marked permanently unavailable only when **Spotify** explicitly declined it.

While Spotify is paused it has not spoken, so anything the fallback happens not to know stays queued
and is asked again later. A fallback can therefore only ever add metadata. It can never cause a track
to be written off, no matter how incomplete it is.

## The contract

Three `GET` endpoints under the base URL you configure. That is the entire interface.

```
GET {base}/v1/tracks?ids=<comma-separated>
GET {base}/v1/artists?ids=<comma-separated>
GET {base}/v1/albums?ids=<comma-separated>
```

- Ids are Spotify ids, base-62, at most 50 per request (configurable lower).
- If `ENCORE_METADATA_FALLBACK_TOKEN` is set, every request carries
  `Authorization: Bearer <token>`. Reject requests without it however you like; a `401` or `403`
  is reported to the operator as a configuration error and is not retried.
- Answer `200` with a JSON object whose single key is `tracks`, `artists` or `albums`, holding an
  array.
- For an id you do not have, return `null` in its place **or** simply omit it. Both are fine —
  Encore compares what it asked for against what it got.
- Any other status is treated as a fallback failure: logged, retried a few times, then ignored for
  that batch.

This is deliberately Spotify's own response shape. Proxying Spotify verbatim is a valid
implementation, and so is `SELECT` from a local database.

### Objects

Encore reads a small subset of Spotify's object fields and ignores the rest. Everything is optional
except `id` — a missing field simply means Encore has nothing to show for it.

**Track** (`/v1/tracks`)

| Field | Type | Used for |
|---|---|---|
| `id` | string | **Required.** Must match the id that was requested; anything else is discarded. |
| `name` | string | The track title. |
| `duration_ms` | number | Track length. |
| `explicit` | bool | The explicit marker. |
| `popularity` | number | Spotify's 0–100 popularity. |
| `external_ids.isrc` | string | Recording identity, upper-cased. |
| `album.id` | string | Links the track to its album, and queues that album for enrichment. |
| `artists[].id`, `artists[].name` | string | Credits. The names are used immediately, so a track response alone makes the interface readable without waiting for `/v1/artists`. |

**Artist** (`/v1/artists`)

| Field | Type | Used for |
|---|---|---|
| `id` | string | **Required.** |
| `name` | string | The artist name. |
| `genres` | string[] | Genre statistics. |
| `popularity` | number | 0–100. |
| `followers.total` | number | Follower count. |
| `images[].url`, `.width`, `.height` | | Artist artwork. Encore picks one by size. |

**Album** (`/v1/albums`)

| Field | Type | Used for |
|---|---|---|
| `id` | string | **Required.** |
| `name` | string | The album title. |
| `album_type` | string | `album`, `single`, `compilation`. |
| `release_date` | string | `YYYY`, `YYYY-MM` or `YYYY-MM-DD`. |
| `release_date_precision` | string | `year`, `month` or `day`. Encore stores the precision, so a year-only date is never silently presented as 1 January. |
| `total_tracks` | number | |
| `images[].url`, `.width`, `.height` | | Album artwork. |
| `artists[].id` | string | Credits. |

### A minimal example

```
GET /v1/tracks?ids=4cOdK2wGLETKBW3PvgPWqT,0000000000000000000000
Authorization: Bearer hunter2
```

```json
{
  "tracks": [
    {
      "id": "4cOdK2wGLETKBW3PvgPWqT",
      "name": "Never Gonna Give You Up",
      "duration_ms": 213573,
      "explicit": false,
      "popularity": 78,
      "external_ids": { "isrc": "GBARL9300135" },
      "album": { "id": "6N9PS4QXF1D0OWPk0Sxtb4" },
      "artists": [{ "id": "0gxyHStUsqpMadRV0Di1Qt", "name": "Rick Astley" }]
    },
    null
  ]
}
```

## Configuration

| Variable | Default | Meaning |
|---|---|---|
| `ENCORE_METADATA_FALLBACK_URL` | *(unset)* | Base URL, the part before `/v1/tracks`. **Setting this is what turns the feature on.** Must be absolute `http(s)`. |
| `ENCORE_METADATA_FALLBACK_TOKEN` | *(unset)* | Sent as `Authorization: Bearer …`. Setting it without a URL is a startup error. |
| `ENCORE_METADATA_FALLBACK_TIMEOUT` | `10s` | Per-request timeout. Generous by default: a source reading a large local database can be slower than a CDN. |
| `ENCORE_METADATA_FALLBACK_BATCH_SIZE` | `50` | Ids per request, capped at Spotify's own 50. |
| `ENCORE_METADATA_FALLBACK_RATE_LIMIT` | *(unlimited)* | Requests per second, if you want Encore to tread lightly on your own server. |
| `ENCORE_METADATA_FALLBACK_RATE_BURST` | `1` | Burst allowance, only meaningful with a rate limit. |

Only the **worker** container reads metadata, so only it needs the URL to work. Give it to the API
container as well and the Settings page will say that a fallback is configured, which is what turns
"rate limited, everything has stopped" into "rate limited, still filling in".

A URL that is not an absolute `http(s)` URL is a startup error rather than a warning. A fallback that
silently does nothing produces exactly the symptom it was turned on to cure, with the operator
believing it is handled.

## Checking that it works

Enrichment logs when the fallback contributes:

```bash
docker compose logs worker | grep -i 'fallback'
```

```
a metadata fallback is configured  url=https://metadata.internal
primary metadata source is rate limited; served from the fallback  kind=artists requested=50 served=48
filled metadata the primary source does not serve  kind=tracks ids=3 filled=2
```

The first line appears at startup. The second means Spotify is paused and your source is carrying
enrichment. The third means your source filled ids Spotify would never have served — the holes that
were previously permanent.

**Settings → Music metadata** in the browser shows the same progress without a shell.

## Writing a source

The contract is small enough that a working implementation is a few dozen lines in any language: bind
three routes, split `ids` on commas, look each one up, emit the envelope. Ids you do not have become
`null`.

Two things are worth getting right:

- **Return the id you were asked for.** Encore discards any object whose id it did not request, so a
  source that normalises or relinks ids will appear to return nothing.
- **Do not invent data.** An id you do not know should be `null`, not a row with an empty name. A
  blank name is stored and displayed; a `null` leaves the id queued for Spotify to answer later.

`internal/metadata/mirror_test.go` contains a stub implementation of this contract in about forty
lines, written against this document rather than against the client. If you are building a source, it
is the shortest complete example.
