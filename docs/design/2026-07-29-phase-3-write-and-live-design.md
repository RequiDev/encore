# Phase 3 — Write and live

**Date:** 2026-07-29
**Status:** Design / plan of record
**Context:** [Overview and phase map](2026-07-29-spotify-api-expansion-overview.md)
**Depends on:** Phase 2's consent migration, for `user-read-playback-state`

Two features, and the only genuinely new machinery in the plan: an image encoder and a second poll
loop. Both are additive — an instance that never generates a cover and never enables the poller is
unaffected by this phase entirely.

---

## 1. Feature 7 — playlist rename and cover art

### 1.1 What rebuild does today, and what it misses

`internal/spotify/playlists.go` has `CreatePlaylist` and `ReplacePlaylistItems`. A rebuild replaces
the tracks and nothing else. The playlist keeps the name it was given when it was first created and
the grey placeholder Spotify assigns, even though `playlists.name` and the full definition — mode,
sort, range, limits — are stored locally and could describe it exactly.

Two calls close this:

| Call | Scope | Effect |
|---|---|---|
| `PUT /v1/playlists/{id}` | `playlist-modify-private` (held) | Name and description |
| `PUT /v1/playlists/{id}/images` | `ugc-image-upload` **+** `playlist-modify-private` | Cover |

`ugc-image-upload` is requested together with `playlist-modify-private`, at the existing
playlist-creation consent moment. No new interruption, and an account that never makes a playlist
still holds a grant that cannot write anything.

The description is regenerated from the stored definition — what the playlist is, over what range,
ranked how, rebuilt when — so a playlist explains itself inside Spotify, away from Encore.

### 1.2 The cover

640 × 640 JPEG. A mosaic of the top four album covers from the playlist's own tracks, with the
playlist name set over a scrim across the lower third.

**Size limit, precisely.** The spec says "Base64 encoded JPEG image data, maximum payload size is
256 KB". The limit applies to the *encoded* payload, so the binary JPEG ceiling is
`256 × 3/4 ≈ 192 KB`. Encoding starts at quality 90 and steps down until the binary is under
190 KB. At 640 × 640 the first attempt is normally 60–100 KB, so the loop rarely runs twice — but
it exists, because a four-way photographic mosaic is exactly the input that gets large.

**Dependency.** `golang.org/x/image` for `draw` (quality downscaling, which `image/draw` cannot do)
and `font/opentype`. The typeface is `golang.org/x/image/font/gofont/gobold` — BSD-3, shipped as Go
source, so no font file and no licence file enter the repository. It does not match the web app's
Inter, and that is accepted: nobody sees a 640 px playlist cover beside the dashboard. Embedding
Inter later, for brand consistency, is a one-line change plus an OFL notice.

This is the only new module dependency in the whole plan. `go.mod` currently has six direct
requirements and `golang.org/x/text` is already among them, so `golang.org/x/image` is a sibling
rather than a new neighbourhood.

**Fetching the art.** `albums.image_url` points at Spotify's CDN. This costs no API quota, but it is
outbound HTTP from the server and is treated as such:

- At most four fetches per cover.
- 5 s timeout and a 2 MB `io.LimitReader` cap each.
- Content type must decode as an image; a failure drops that tile rather than the cover.
- **The host is checked against an allowlist before the request is made.** The URL comes from a
  database column, and a stored URL is a stored URL whatever wrote it. This is a plain SSRF guard on
  a server-side fetch of a user-influenced address, and it costs one comparison.

**Fallback.** Fewer than four usable covers — the normal state on a fresh instance whose catalogue
is still enriching — falls back to a deterministic geometric cover seeded by a hash of the playlist
definition. Same definition, same cover, every time. The name is still drawn over it, so the result
is informative rather than merely decorative.

### 1.3 Failure is not failure

**Cover generation and upload are best-effort and can never fail the playlist operation.** A
playlist that exists with a grey cover is a far better outcome than a rebuild that reports failure
because `i.scdn.co` was slow. Failures are logged and surfaced as a small "cover not generated"
state on the playlist row, retryable from the UI.

Generation runs synchronously within the create/rebuild request, which already makes several API
calls and is already something the user waits on. If it proves slow enough to matter it moves to the
worker — the failure semantics above are what make that a safe change to defer rather than a
rewrite.

---

## 2. Feature 8 — now playing

> **Status:** §2.1–§2.3 shipped as Phase 3b (merged `2cca3a9`). §2.4 and §2.5
> shipped as Phase 3c, on the authorisation §2.5 records in advance.

`GET /v1/me/player` returns the active device, `shuffle_state`, `repeat_state`, `progress_ms`,
`is_playing`, the item and the context. **It answers `204 No Content` when nothing is playing**,
which is the common case and is not an error.

### 2.1 Off unless asked for

`ENCORE_NOWPLAYING_INTERVAL`, unset means disabled — the same shape as the metadata-fallback
feature: ship the mechanism, default it off, document the cost.

The cost is recurring and worth stating in `docs/configuration.md`:

| Users | Interval | Calls/day |
|---|---|---|
| 1 | 30 s | ≈ 2,880 |
| 5 | 30 s | ≈ 14,400 |
| 5 | 60 s | ≈ 7,200 |

A development-mode Spotify application already exhausts its quota during a large import; a poller
that silently doubles baseline consumption would make that worse for everyone on the instance,
which is why this is opt-in rather than a default with a tuning knob.

It draws on the **interactive budget**, so a catalogue 429 cannot stall it and it cannot stall
enrichment. Only users whose grant includes `user-read-playback-state` and who are not
`needs_reauth` are polled; everybody else is skipped without a request.

### 2.2 The poller never writes listens

**`/me/player` must not create rows in `listens`.** `GET /me/player/recently-played` remains the
sole ingestion path.

This is not a stylistic preference. The sync poller's correctness rests on the cursor advancing in
the same transaction that commits the listens it covers; a second writer with a different view of
what has been played would produce duplicates that the dedupe key catches by accident rather than by
design, and would break the property that re-running ingestion adds exactly zero rows. The
now-playing poller is a **read-only observer**.

### 2.3 Part one — the live card

`GET /api/nowplaying` returns the last observation for the requesting user, with its age. The client
polls it; no streaming transport is introduced for this. Renders as a card on the dashboard showing
what is playing, on what device, with progress.

Self-contained, and most of the visible value of the feature.

**Never on a share link.** Real-time presence is exactly the concern `internal/domain/share.go` was
written around — a share exposes what somebody listens to, never when they are awake.

### 2.4 Part two — shuffle and platform backfill

The documented limitation this addresses: a listen recorded by live sync cannot know whether it was
shuffled or what device it played on, because `/me/player/recently-played` reports neither. Only an
extended export fills those columns, so a synced listen is permanently lower-fidelity than an
imported one covering the same moment.

The poller sees precisely what is missing. A short-lived observation log bridges them.

**The block below is the design's shape; `migrations/00018_playback_observations.sql`
is what shipped.** It adds two CHECK constraints — `track_id <> ''`, and "a row
must say something" (`shuffle IS NOT NULL OR device_type IS NOT NULL`) — and the
`listens.device_type` column the paragraph above describes. It adds no index
beyond the primary key, which is also the join's access path.

```sql
CREATE TABLE playback_observations (
    user_id     uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    track_id    text NOT NULL,
    observed_at timestamptz NOT NULL,
    device_type text,
    device_name text,
    shuffle     boolean,
    PRIMARY KEY (user_id, track_id, observed_at)
);
```

Rows expire after 24 hours — long enough for any sync interval, short enough that the table stays
small and never becomes a second history nobody meant to keep.

When sync ingests a listen, it looks for an observation of the same track whose `observed_at` falls
within `[played_at, played_at + duration_ms + tolerance]`, takes the most recent match, and fills
`shuffle` and `platform` from it. No match leaves both NULL, exactly as today.

**As built, it fills `shuffle` and a new `device_type` column — not `platform`.**
`listens.platform` holds an export's free text (`"Android OS 10 API 29 (samsung,
SM-G970F)"`, `"web_player"`) and `PlatformFamily` is a substring classifier built
for those shapes; Spotify Connect's `device.type` is a different vocabulary
(`"Computer"`, `"Smartphone"`, `"Speaker"`). Writing the second into the first
would have made every historical platform figure change meaning without changing
shape, which is the same error as letting "unknown" and "false" share a column.
`device_name` is kept in the observation log for a day and never copied onto
`listens`.

### 2.5 Why this is sequenced last, and is droppable

It is a fuzzy temporal join against a best-effort log, and it is the most intricate thing in all
three phases for the least visible payoff. A user cannot see it working; they can only see it
wrong — a listen labelled as shuffled that was not.

So it ships **after** the live card, as a separate commit, and if it fights back it gets cut without
losing the feature. That is a design decision recorded in advance rather than a concession made
under pressure later.

The tolerance window is the part most likely to need tuning against real data. It is a named
constant with its reasoning in a comment, not a literal buried in a query.

---

## 3. Testing

| Test | Asserts |
|---|---|
| Cover size ceiling | A four-photograph mosaic encodes under 190 KB binary; the quality loop terminates |
| Cover determinism | The same definition with fewer than four covers produces a byte-identical fallback image |
| Tile failure | One unreachable art URL yields a three-tile cover, not an error |
| Host allowlist | An `image_url` pointing at a non-Spotify host is not fetched |
| Rebuild resilience | Cover failure leaves the playlist created and its tracks correct |
| Description regeneration | Each of the four playlist modes produces its documented description |
| Poller disabled | With `ENCORE_NOWPLAYING_INTERVAL` unset, no request is ever issued |
| 204 handling | "Nothing playing" is recorded as idle, not as an error, and does not trip retry |
| Scope skip | A user without `user-read-playback-state` is skipped without a request |
| No phantom listens | Running the poller across a listening session adds zero rows to `listens` |
| Backfill window | An observation inside the window fills `shuffle`; one outside it leaves NULL |
| Observation expiry | Rows older than 24 h are removed and never match |

---

## 4. Documentation

- `docs/configuration.md` — `ENCORE_NOWPLAYING_INTERVAL` and `ENCORE_LIBRARY_SYNC_INTERVAL`, with
  the quota table from §2.1.
- `.env.example` — both, commented, defaulted off.
- `docs/feature-parity.md` — **done (Phase 3c).** Note the premise was wrong: this limitation was
  not a "known gap" anywhere. Neither `README.md`'s limitations nor `docs/feature-parity.md`'s known
  gaps carried a bullet for it, so Phase 3c *added* the statement rather than moving one — and said
  which half the poller closes and which half no endpoint can ever close. Playback **control** stays
  declined, which is a different thing and was not conflated by the edit.
- `README.md` — **done (Phase 3c).** Same correction, added rather than moved, for the same reason.

---

## 5. Out of this phase

- **Playback control.** `user-modify-playback-state` is still not requested. Reading a listening
  history does not require the ability to interrupt it.
- **Public playlists.** `playlist-modify-public` remains unrequested; Encore publishes nothing to a
  listener's followers.
- **`repeat_state` and volume**, available from `/me/player` and stored by nothing. No question
  asked for them.
