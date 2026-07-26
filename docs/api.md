# HTTP API reference

This is the contract between `internal/httpapi` and `web/`. The TypeScript mirror of every payload
lives in [`web/src/lib/types.ts`](../web/src/lib/types.ts) and is kept in step by hand — if you change
one, change the other.

## Conventions

- All application endpoints are under `/api`. Operational endpoints (`/healthz`, `/readyz`,
  `/metrics`) are not, so they can be scraped and probed without touching the API surface.
- JSON in, JSON out. Field names are `camelCase`. Timestamps are RFC 3339 with a `Z` offset.
  Durations are milliseconds as integers, named `msPlayed` or `durationMs`.
- Authentication is the `encore_session` cookie. No bearer tokens, no API keys.
- Every `POST`, `PUT`, `PATCH` and `DELETE` requires the `X-CSRF-Token` header to match the
  `encore_csrf` cookie.
- Date ranges are `?from=<RFC3339>&to=<RFC3339>`, half-open `[from, to)`. Omitting both gives the
  trailing 30 days in the user's timezone. Bucketing always happens in the user's own timezone.
- Pagination is `?limit=&offset=` except for the listening history, which is keyset paginated with an
  opaque `?cursor=`.

### Errors

```json
{ "error": { "code": "not_found", "message": "That import job does not exist." } }
```

| Code | Status | Meaning |
|---|---|---|
| `invalid_request` | 400 | Malformed or out-of-range parameters. `details` may name the field. |
| `unauthenticated` | 401 | No session, or an expired one. |
| `csrf` | 403 | Missing or mismatched CSRF token. |
| `forbidden` | 403 | Authenticated but not permitted. |
| `not_found` | 404 | Includes objects that exist but belong to someone else. |
| `conflict` | 409 | State conflict, e.g. retrying a job that is already running. |
| `payload_too_large` | 413 | Upload exceeded `ENCORE_IMPORT_MAX_UPLOAD_BYTES`. |
| `registrations_disabled` | 403 | An unknown Spotify identity signed in while registration is closed. |
| `account_disabled` | 403 | The account has been deactivated by an administrator. |
| `rate_limited` | 429 | Too many requests. |
| `internal` | 500 | Something unexpected. The message is deliberately vague; the log has detail. |

Error messages never contain tokens, credentials, connection strings or SQL.

---

## Authentication

| Method | Path | Description |
|---|---|---|
| `GET` | `/api/auth/spotify/login` | Starts the OAuth flow. `?redirect_to=` is validated against `ENCORE_WEB_URL` and ignored if it points anywhere else. Responds `302` to Spotify. |
| `GET` | `/api/auth/spotify/callback` | Consumes the single-use `state`, exchanges the code, creates or finds the user, sets the session cookie, and redirects into the web client. Errors redirect to `${ENCORE_WEB_URL}/login?error=<code>` rather than rendering an API error page. |
| `GET` | `/api/auth/spotify/relink` | Re-runs OAuth for the signed-in user, for when a refresh token has been revoked. Refuses to attach a different Spotify identity to an existing account. |
| `POST` | `/api/auth/logout` | Deletes the session row and clears the cookie. |

## Session and account

### `GET /api/me`

The bootstrap call the client makes on load. Returns `401` when signed out.

```json
{
  "user": {
    "id": "0f1c…", "spotifyUserId": "someone", "displayName": "Someone",
    "email": "someone@example.com", "avatarUrl": "https://i.scdn.co/…",
    "role": "admin", "isActive": true, "timezone": "Europe/Berlin",
    "createdAt": "2026-01-04T10:00:00Z", "lastLoginAt": "2026-07-26T08:12:00Z"
  },
  "spotify": {
    "connected": true, "syncState": "ok",
    "lastSyncAt": "2026-07-26T08:11:03Z", "lastSyncError": "",
    "scopes": ["user-read-recently-played", "user-read-private", "user-read-email"]
  },
  "csrfToken": "…",
  "instance": { "registrationsEnabled": false, "version": "1.0.0" }
}
```

| Method | Path | Description |
|---|---|---|
| `PATCH` | `/api/me` | `{ "timezone": "Europe/Berlin" }`. Changing it marks the user's rollups dirty so statistics re-bucket. |
| `DELETE` | `/api/me` | Hard-deletes the account and all its data. Body must be `{ "confirm": "<spotifyUserId>" }`. |
| `GET` | `/api/me/export?format=json\|csv` | Streams the caller's full listening history. Chunked, never buffered. |
| `POST` | `/api/sync/now` | Triggers an immediate recently-played poll. `409` if one is already running. |

## Users and administration

| Method | Path | Description |
|---|---|---|
| `GET` | `/api/users` | Other users on the instance — id and display name only. Powers the comparison page. |
| `GET` | `/api/admin/settings` | `{ "registrationsEnabled": true }`. Admin only. |
| `PATCH` | `/api/admin/settings` | `{ "registrationsEnabled": false }`. Admin only. |
| `GET` | `/api/admin/users?limit=&offset=` | Full user list with listen counts and sync state. Admin only. |
| `PATCH` | `/api/admin/users/{id}` | `{ "role": "user", "isActive": true }`. Refuses to demote or deactivate the last administrator. Admin only. |
| `DELETE` | `/api/admin/users/{id}` | Refuses to delete the last administrator. Admin only. |

## Statistics

All accept `from` and `to`.

| Method | Path | Description |
|---|---|---|
| `GET` | `/api/stats/summary` | Listens, distinct tracks/artists/albums, total ms, active days, first and last listen. |
| `GET` | `/api/stats/top/tracks` | `?limit=&offset=`. Entries carry the resolved entity, `plays`, `msPlayed`, `rank`, and `previousRank` (`null` if absent last period). |
| `GET` | `/api/stats/top/artists` | As above. |
| `GET` | `/api/stats/top/albums` | As above. |
| `GET` | `/api/stats/timeline` | `?interval=hour\|day\|week\|month\|year`. Omitting it picks the finest interval under the bucket cap. Empty buckets are present with zeroes. |
| `GET` | `/api/stats/repartition/hour` | 24 buckets, local hour of day. |
| `GET` | `/api/stats/repartition/weekday` | 7 buckets, Monday first. |
| `GET` | `/api/stats/repartition/heatmap` | 7 × 24 grid. |
| `GET` | `/api/stats/sessions` | `?limit=`. Longest listening sessions with their track lists. |
| `GET` | `/api/stats/discovery` | `?interval=`. Artists and tracks heard for the first time ever, per bucket. |
| `GET` | `/api/stats/streaks` | Current and longest runs of consecutive listening days. |
| `GET` | `/api/stats/compare` | `?aFrom=&aTo=&bFrom=&bTo=`. Two summaries plus deltas. |
| `GET` | `/api/stats/year-in-review?year=2026` | Wrapped-style yearly summary. |
| `GET` | `/api/stats/extras` | Different artists per period, average album release year, average artists per track. |
| `GET` | `/api/stats/affinity/{userId}` | Shared artists, albums and tracks with another user, and a similarity score. |

## Entities

| Method | Path | Description |
|---|---|---|
| `GET` | `/api/tracks/{id}` | Track, its album and artists, plus the caller's stats for it over the range. |
| `GET` | `/api/artists/{id}` | Artist, the caller's stats, top tracks, top albums, day repartition, first and last listen, share of total listening. |
| `GET` | `/api/albums/{id}` | Album, its artists and tracks, plus the caller's stats. |
| `GET` | `/api/search?q=&limit=` | Catalogue search across artists, albums and tracks. |

## Listening history

`GET /api/history?from=&to=&cursor=&limit=`

Keyset paginated on `(playedAt, id)`, never `OFFSET`, because a user may have millions of rows.

```json
{
  "items": [
    { "id": "918273", "playedAt": "2026-07-26T07:58:11Z", "msPlayed": 214000,
      "source": "extended", "track": { "id": "4uLU…", "name": "…", "artists": […], "album": {…} } }
  ],
  "nextCursor": "eyJ0IjoiMjAyNi0wNy0yNlQwNzo1ODoxMVoiLCJpIjo5MTgyNzN9",
  "hasMore": true
}
```

`nextCursor` is opaque. Pass it back verbatim; do not construct one.

## Artist blacklist

| Method | Path | Description |
|---|---|---|
| `GET` | `/api/blacklist` | Artists excluded from every statistic for the caller. |
| `POST` | `/api/blacklist` | `{ "artistId": "…" }`. |
| `DELETE` | `/api/blacklist/{artistId}` | |

## Imports

### `POST /api/imports`

`multipart/form-data`, one or more `files` parts plus an optional `note`. Accepts `.json`, `.json.gz`
and `.zip`; a zip is expanded and each streaming-history entry becomes its own file in the job.
Bodies are streamed to durable storage, never buffered.

Responds `202 Accepted` with the created job. A file whose SHA-256 matches one already imported by
this user is still accepted — re-importing is idempotent — but is flagged so the UI can warn.

```json
{
  "job": { "id": "…", "status": "queued", "filesTotal": 12, "filesDone": 0,
           "counters": { "imported": 0, "duplicates": 0, "skipped": 0, "rejected": 0 },
           "files": [ … ] },
  "warnings": [ { "file": "Streaming_History_Audio_2015-2017_0.json",
                  "code": "already_imported",
                  "message": "Imported before on 2026-06-02. Re-importing is safe and adds nothing." } ]
}
```

### The rest

| Method | Path | Description |
|---|---|---|
| `GET` | `/api/imports?limit=&offset=` | The caller's jobs, newest first. |
| `GET` | `/api/imports/{id}` | One job with per-file progress. Poll this for live updates. |
| `POST` | `/api/imports/{id}/cancel` | Stops at the next batch boundary; committed records are kept. |
| `POST` | `/api/imports/{id}/retry` | Resumes a failed, cancelled or paused job **from its checkpoint**. `409` if the job is not resumable. |
| `DELETE` | `/api/imports/{id}` | Removes the job and its uploaded files. **Listening records are kept**; their `importFileId` becomes null. |
| `GET` | `/api/imports/{id}/rejects?limit=&offset=` | Malformed records with record index, reason code, detail and a truncated excerpt. |

### Job payload

```json
{
  "id": "…", "status": "running", "note": "full export june 2026",
  "createdAt": "…", "startedAt": "…", "finishedAt": null,
  "errorCode": "", "errorMessage": "",
  "filesTotal": 12, "filesDone": 7,
  "counters": { "imported": 812934, "duplicates": 41, "skipped": 9022, "rejected": 3 },
  "files": [
    { "id": "…", "name": "Streaming_History_Audio_2015-2017_0.json",
      "containerPath": "Spotify Extended Streaming History/…",
      "format": "extended", "status": "completed", "sizeBytes": 41234567,
      "recordsTotal": 89211, "recordOffset": 89211, "pending": 0,
      "counters": { "imported": 88104, "duplicates": 5, "skipped": 1102, "rejected": 0 },
      "errorCode": "", "errorMessage": "" }
  ]
}
```

`status` is one of `queued`, `running`, `paused`, `completed`, `failed`, `cancelled`. `pending` is
`null` while `recordsTotal` is still unknown — the UI shows "counting" rather than inventing a
denominator.

**A job only reaches `completed` after verification.** See [`docs/import.md`](import.md) §7: the
importer compares its own counters against a real `count(*)` over `listens` per file, and a shortfall
produces `failed` with `errorCode = "verification_failed"` rather than a false success.

---

## Operational endpoints

| Method | Path | Description |
|---|---|---|
| `GET` | `/healthz` | Liveness. `200 {"status":"ok"}` whenever the process is running. Never touches the database, so a database outage does not cause a restart loop. |
| `GET` | `/readyz` | Readiness. `200` only when the database answers **and** every embedded migration is applied. `503` with `{"status":"not_ready","checks":{…}}` otherwise. |
| `GET` | `/metrics` | Prometheus text format. Behind basic auth when `ENCORE_METRICS_USERNAME` and `ENCORE_METRICS_PASSWORD` are set. |
