# Spotify API expansion — overview and phase map

**Date:** 2026-07-29
**Status:** Design / plan of record
**Phases:** [Phase 1](2026-07-29-phase-1-latent-statistics-design.md) ·
[Phase 2](2026-07-29-phase-2-scope-expansion-design.md) ·
[Phase 3](2026-07-29-phase-3-write-and-live-design.md)

This document records what Encore currently takes from the Spotify Web API, what it leaves on the
table, and how the work of closing that gap is divided. The two phase documents beneath it are the
implementable specifications; this one holds the decisions all three share, so they are stated once.

---

## 1. Where Encore stands

Spotify's published OpenAPI schema describes **70 paths and 96 operations**. As of Phase 3b, Encore
calls **19 operations across 18 paths** — stale counts get corrected here rather than left as a
snapshot of the day this document was written, for the same reason the ordinals in `docs/api.md`
don't survive a second unattended request:

| Operation | Caller | Purpose |
|---|---|---|
| `GET /v1/me` | `spotify/recentlyplayed.go` | Profile, at sign-in |
| `GET /v1/me/player/recently-played` | `spotify/recentlyplayed.go` | The sync poller |
| `GET /v1/tracks` | `spotify/catalog.go` | Batch enrichment |
| `GET /v1/artists` | `spotify/catalog.go` | Batch enrichment |
| `GET /v1/albums` | `spotify/catalog.go` | Batch enrichment |
| `GET /v1/search` | `spotify/search.go` | Alias resolution for names-only imports |
| `POST /v1/users/{id}/playlists` | `spotify/playlists.go` | Playlist creation |
| `PUT`/`POST /v1/playlists/{id}/tracks` | `spotify/playlists.go` | Playlist fill and rebuild |
| `PUT /v1/playlists/{id}` | `spotify/playlists.go` | Playlist rename and description (Phase 3a) |
| `PUT /v1/playlists/{id}/images` | `spotify/playlists.go` | Playlist cover (Phase 3a) |
| `GET /v1/me/top/{type}` | `spotify/topitems.go` | Top artists/tracks diff (Phase 2) |
| `GET /v1/me/tracks` | `spotify/library.go` | Saved tracks (Phase 2) |
| `GET /v1/me/albums` | `spotify/library.go` | Saved albums (Phase 2) |
| `GET /v1/me/following` | `spotify/library.go` | Followed artists (Phase 2) |
| `GET /v1/me/playlists` | `spotify/playlists.go` | Playlist listening-context naming (Phase 2) |
| `GET /v1/albums/{id}/tracks` | `spotify/albumtracks.go` | Album completion (Phase 2) |
| `GET /v1/artists/{id}/albums` | `spotify/artistalbums.go` | Discography completion (Phase 2) |
| `GET /v1/me/player/currently-playing` | `spotify/nowplaying.go` | Now playing card (Phase 3b) |

Eighteen rows, nineteen operations: the one path with two methods (`PUT`/`POST
/v1/playlists/{id}/tracks`) is the only row counted twice.

## 2. What is permanently out of reach

A third of the schema is documented but unusable here, and this is not a gap to be closed. On
2024-11-27 Spotify restricted the following to applications already in extended quota mode on that
date:

- `GET /audio-features`, `GET /audio-features/{id}`
- `GET /audio-analysis/{id}`
- `GET /recommendations`, `GET /recommendations/available-genre-seeds`
- `GET /artists/{id}/related-artists`
- `GET /browse/featured-playlists`
- `GET /browse/categories/{category_id}/playlists`

Extended quota mode now requires 250,000 monthly active users. Encore is distributed as software
people run themselves against **their own Spotify application**, so every installation is a new
application in development mode. Every one of these endpoints returns 403, permanently, for every
Encore instance that will ever exist.

This rules out the obvious ideas — tempo and valence charts, danceability profiles, "similar
artists", recommendation seeding — and it rules them out for good rather than until somebody
finds time. `docs/feature-parity.md` records this as a permanent constraint, not an outstanding item.

The same change removed `preview_url` from track responses. `internal/spotify/models.go:103` still
declares a `PreviewURL` field that is now always empty and is referenced nowhere else; Phase 1
deletes it.

## 3. The eight features

| # | Feature | Spotify calls | New scope | Phase |
|---|---|---|---|---|
| 1 | Genre statistics | none | — | 1 |
| 2 | Playback-context statistics | none | — | 1 |
| 3 | `/me/top` diff | `GET /me/top/{type}` | `user-top-read` | 2 |
| 4 | Library and follows | `GET /me/tracks`, `/me/albums`, `/me/following` | `user-library-read`, `user-follow-read` | 2 |
| 5 | Playlist listening context | `GET /me/playlists` | `playlist-read-private` | 2 |
| 6 | Album and discography completion | `GET /albums/{id}/tracks`, `/artists/{id}/albums` | — (app token) | 2 |
| 7 | Playlist rename and cover art | `PUT /playlists/{id}`, `PUT /playlists/{id}/images` | `ugc-image-upload` | 3 |
| 8a | Now playing, the live card | `GET /me/player/currently-playing` | `user-read-playback-state` | 3b — shipped |
| 8b | Shuffle and platform backfill | `GET /me/player` | `user-read-playback-state` | 3c — deferred, see the 3b plan |

Podcast and audiobook support — `GET /episodes`, `/shows/{id}`, `/audiobooks/{id}`, `/chapters/{id}`
— is deliberately **excluded from this plan**. It remains the largest single gap in Encore's
coverage: `importer/formats/extended.go:65-72` reads `spotify_episode_uri`, `episode_show_name`,
`audiobook_uri` and `audiobook_chapter_uri`, then routes every one of them to `SkipNotMusic`. Whole
categories of listening are discarded. It is out of scope here because it touches the importer,
the catalogue schema, enrichment and the UI at once, and deserves its own design.

## 4. Decisions that span every phase

### 4.1 The sign-in scope set grows from three to eight

`internal/config/config.go:398` currently returns:

```
user-read-recently-played  user-read-private  user-read-email
```

It gains `user-top-read`, `user-library-read`, `user-follow-read`, `playlist-read-private` and
`user-read-playback-state`.

This **reverses deviation #6** in `docs/feature-parity.md`, which states that Encore asks for
read-only scopes at sign-in and defers everything else to the point of use. That claim, and the
matching text at `docs/security.md:154`, are rewritten in Phase 2, in the commit that changes the
behaviour. A document asserting a property the code no longer has is worse than no document.

`ugc-image-upload` is **not** in the sign-in set. It is a write scope, and it is requested together
with `playlist-modify-private` at the moment somebody creates a playlist — the existing incremental
consent moment, with no new interruption.

### 4.2 Existing users need re-consent

Stored refresh tokens carry the grant that was current when they were issued. A token minted before
this change will never carry the new scopes, and Spotify will answer 403 for the features that need
them.

Encore already has both halves of the fix: `spotify.HasScope` compares a granted set against a
required one, and `GET /api/auth/spotify/relink` re-runs authorisation without detaching the
identity. The new behaviour is a **dismissible banner**, shown when the stored grant is missing
scopes, linking to relink. It is never a hard block: an account that ignores it keeps working
exactly as it does today, minus the features it has not granted.

Nothing in Phase 1 depends on this.

### 4.3 Every statistic reports its own denominator

This is the rule that shapes more of the work than any other.

Several of the new statistics are computed over a **subset** of the fact table, and the subsets are
large:

- **Genres** exist only on `artists` rows that enrichment has resolved. A freshly imported history
  holds thousands of locally-minted artists with `genres = '{}'`. Coverage climbs from near zero to
  near complete over hours or days.
- **Playback context** — `shuffle`, `skipped`, `reason_start`, `reason_end`, `platform`,
  `conn_country`, `offline`, `incognito` — exists **only on `source = 2`**, rows from an extended
  export. Sync rows and account-data rows carry NULL in all eight columns.
- **Playlist context** will exist only on `source = 0`, rows recorded live by Encore. No export
  format carries it.

A skip rate computed over whichever rows happen to have the column, and then presented as a fact,
is wrong. An empty genre chart on a fresh instance is indistinguishable from a broken one.

So: **every response carries `covered` and `total` beside its numbers, and every view states its
coverage in words.** Not a tooltip, not a footnote — a line of text under the chart. This is not
defensive polish; it is the difference between a statistic and a plausible-looking number.

### 4.4 Share links stay narrow, and stay fixed in code

`internal/domain/share.go:20-24` is explicit:

> What a share can expose is fixed by the feature rather than by the row [...] There is no field
> here that could widen it, which is deliberate — a privacy boundary that depends on a boolean being
> set correctly is one that will eventually be set incorrectly.

That holds. No new field appears on `ShareLink`, and no share gains a toggle. What a share exposes
is decided once, here:

| Statistic | On a share? |
|---|---|
| Genres, obscurity score | **Yes** |
| Album completion | **Yes**\* |
| Library and follows counts | **Yes**\* |
| Shuffle share, skip rate | No |
| Platform, country, offline, incognito | No |
| Playlist listening context | No |
| Now playing | No |

Device and country reveal what hardware somebody owns and where they have travelled. Now-playing is
real-time presence, which is precisely the concern the share design was written around.

\* **Decided, not yet built.** These two rows record the decision made here, not the current state of
the code. Neither has been added to `handleSharedStats` (`internal/httpapi/share.go`) or to
`SharedStatsResponse` — the fixed set that handler composes today includes neither. Album completion
has been outstanding since the Phase 2b branch merged; library and follows counts became outstanding
when the Phase 2c-ii branch shipped the `/library` page without touching share links. Both are
tracked as deferred work in [Phase 2's "Deferred from Phase
2c"](2026-07-29-phase-2-scope-expansion-design.md#9-deferred-from-phase-2c).

### 4.5 Phase boundaries

The phases are cut by **what each needs from the user**, not by subject matter:

- **Phase 1 needs nothing.** No Spotify call, no scope, no migration, no dependency. It can merge
  before Phase 2 is designed.
- **Phase 2 needs one consent migration**, paid once for four features.
- **Phase 3 needs new machinery** — an image encoder and a new poll loop with its own quota.

Each phase gets its own implementation plan and its own merge.

---

## 5. Out of scope

Recorded so they are not rediscovered as omissions:

- **Podcasts and audiobooks** — §3 above.
- **Playback control** (`user-modify-playback-state`) — still declined. It is the one write scope
  with no read-only equivalent, and reading a listening history does not require the ability to
  interrupt it.
- **`tracks.isrc` and `tracks.explicit` as statistics** — both are stored and unused. Neither
  suggested a question worth answering. They stay stored.
- **Service worker / offline PWA** — unchanged, for the reasons in `docs/feature-parity.md`.
