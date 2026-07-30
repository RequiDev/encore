# Security considerations

Encore is designed to be run by one person for a household, on a machine they own. That shapes the
threat model: the realistic risks are a stolen laptop backup, an exposed port, a hostile file upload,
and a browser tab on a site the operator did not write. It is not a multi-tenant SaaS, and it does not
pretend to be hardened against a determined attacker who already has shell access to the host.

## What Encore holds

| Data | Sensitivity | Protection |
|---|---|---|
| Spotify access & refresh tokens | High — grants read access to the listener's Spotify account | AES-256-GCM encrypted at rest with `ENCORE_ENCRYPTION_KEY`, which lives only in the environment. Never logged, never returned by any endpoint. |
| Session tokens | High — a valid one *is* a login | Only the SHA-256 is stored. A database leak cannot be replayed as a session. |
| PKCE code verifiers | Medium, short-lived | Encrypted at rest, single-use, expiring. |
| Listening history | Personal | Scoped to its owner by every query. |
| Email address, display name | Personal | From Spotify's `/v1/me`; shown only to the owner and to administrators. |
| Client secret | High | Environment only. Never in the repository, never in an error message. |

Encore deliberately **discards** the IP address and user agent fields present in extended streaming
history exports. They are the most identifying data in the file and Encore has no use for them.

## Authentication and sessions

- Spotify is the only identity provider. There are no passwords, so there is no password database, no
  reset flow, and no credential stuffing surface.
- OAuth uses the authorization-code flow with **PKCE (S256)**. The `state` parameter is random,
  stored as a hash, and **consumed exactly once** with `DELETE ... RETURNING`, so a replayed callback
  cannot be reused.
- Session cookies are `HttpOnly`, `SameSite=Lax`, and `Secure` by default (relaxed only when
  `ENCORE_ENV=development`, so a plain-HTTP localhost still works). `SameSite=none` is refused unless
  `Secure` is also set, because browsers reject that combination anyway.
- Sessions expire (`ENCORE_SESSION_TTL`, 30 days by default) and are reaped server-side. Signing out
  deletes the row, so a stolen cookie stops working immediately rather than merely being forgotten by
  the browser.

## CSRF

State-changing requests (`POST`, `PUT`, `PATCH`, `DELETE`) require a double-submit token: the value
must appear both in a non-`HttpOnly` cookie and in the `X-CSRF-Token` header, and the two are compared
in constant time against the token bound to the session row.

`SameSite=Lax` already blocks the common cross-site form post; the double-submit check is defence in
depth for the cases it does not cover, and it fails closed — a request with no token is rejected, not
allowed.

## Authorisation

Authorisation is **not** implemented in middleware. Every handler that touches user-owned data asks
the store for the object scoped to the caller:

```go
job, err := s.imports.GetJobForUser(ctx, q, jobID, caller.ID)   // not GetJob(ctx, q, jobID)
```

A missing check therefore shows up as a missing argument rather than as a silently permissive route.
Administrator-only routes are additionally guarded by `requireAdmin`, which re-reads the role from the
database rather than trusting anything carried in the session.

Encore refuses to demote or delete the last remaining administrator, so an instance cannot be locked
out of its own settings.

## Uploads

Import uploads are the largest attack surface, because they are attacker-controlled bytes that Encore
parses.

- The multipart body is **streamed to disk**, never buffered in memory, and is capped by
  `ENCORE_IMPORT_MAX_UPLOAD_BYTES`.
- Uploaded filenames are never used as paths. Each file is stored under a generated UUID inside a
  per-job directory; the original name is kept only as a display string.
- Zip entries are validated before extraction: absolute paths, `..` traversal, symlinks, directories
  and `__MACOSX` entries are rejected, and a per-entry size limit bounds a decompression bomb.
- JSON parsing is streaming and never allocates in proportion to the file, so a 40 GB array of empty
  objects costs time, not memory.
- A malformed record is recorded and skipped; it cannot fail the job or crash the worker.

## Outbound requests

Encore only ever contacts `api.spotify.com` and `accounts.spotify.com`, whose base URLs are
configurable solely so the test suite can point them at a local `httptest` server. No user input
becomes a URL, so there is no SSRF surface.

The OAuth `redirect_to` parameter is validated against `ENCORE_WEB_URL` before any redirect, so the
login flow cannot be used as an open redirect.

## Transport and headers

Encore serves plain HTTP and expects to sit behind TLS — the bundled nginx, or your own reverse proxy.
Set `ENCORE_COOKIE_SECURE=true` (the default) whenever it is reachable over HTTPS.

Responses carry `Content-Security-Policy` (with `frame-ancestors` from `ENCORE_FRAME_ANCESTORS`,
defaulting to none), `X-Content-Type-Options: nosniff`, `Referrer-Policy: strict-origin-when-cross-origin`,
and `X-Frame-Options: SAMEORIGIN`.

`ENCORE_TRUST_PROXY_HEADERS` is **off** by default. Turning it on makes Encore believe
`X-Forwarded-For`, which any client can forge unless a proxy you control overwrites it first.

## Error messages and logs

Errors are useful without being generous:

- Database errors are classified and re-worded; SQLSTATE codes and query text never reach a client.
- Connection strings are scrubbed of passwords before they appear in any message or log line.
- Decryption failures say "check `ENCORE_ENCRYPTION_KEY`" and nothing about the ciphertext, because
  distinguishing "wrong key" from "corrupt data" leaks information to anyone who can trigger it.
- Tokens, cookies and session identifiers are never logged at any level. `logging.Redact` exists for
  the rare case where a fragment genuinely aids diagnosis.
- A panic in a handler becomes a 500 and a logged stack trace, never a stack trace in the response.

## Multi-user considerations

Every user's history is private to them. Two features cross that line, both deliberately:

- **Affinity / comparison** shows aggregate overlap between two users of the same instance. It exposes
  play counts for shared artists and tracks, not raw listening timestamps.
- **Administrators** can list users, change roles, deactivate and delete accounts. They cannot read
  another user's listening history through any endpoint.

Registrations can be closed from the Settings page, which is the right thing to do as soon as everyone
who should have an account has one. While closed, an unknown Spotify identity that completes OAuth is
refused; existing users can always sign in.

## Privacy controls

- **Data export**: a user can download their complete listening history as JSON or CSV.
- **Account deletion**: hard-deletes the user, their listens, sessions and credentials by foreign-key
  cascade. It is not a soft delete and it is not reversible. The shared music catalogue is not
  touched, because it contains no personal data.

## Shared links

A share link is a bearer credential: whoever holds it can read the aggregate
statistics it points at, with no account on the instance.

- The token is 32 random bytes, URL-safe base64. Only its SHA-256 is stored, so a
  database leak yields nothing replayable — the same treatment sessions get.
- It is returned exactly once, by the request that creates it. Nothing can show
  it again, including whoever runs the instance.
- Revocation is immediate and the link is removed from the owner's list.
- A link reaches aggregates only. There is no column, parameter or flag that
  would let one expose the listening history: the endpoint composes a fixed
  payload and has no path to individual plays. A privacy boundary enforced by
  shape cannot be misconfigured.
- Holding a link grants nothing else. Every other endpoint still answers `401`.
- Responses are `noindex`, since an unguessable URL is worth nothing once a
  crawler has published it.
- Links are capped at 25 per account, so the list stays short enough to audit.

## Playlist permission

Creating a playlist is the only thing Encore can do **to** a Spotify account
rather than read from it, so the permission is handled deliberately.

- Sign-in asks for eight read scopes: `user-read-recently-played`,
  `user-read-private`, `user-read-email`, `user-top-read`, `user-library-read`,
  `user-follow-read`, `playlist-read-private` and `user-read-playback-state`.
  None of them can change anything about the account.
- `playlist-modify-private` is requested at the moment somebody uses the feature,
  through a normal OAuth journey they can decline. Spotify issues a token with
  the union of what was granted, so nothing already in place changes.
- An account connected before the scope set grew keeps its old, narrower grant
  until it relinks — nothing forces re-authorisation on it — and `/api/me`
  reports the shortfall as `missingScopes` rather than the account failing
  opaquely the next time a feature needs a scope it does not have.
- Only the *private* modify scope. Encore never asks to publish to a listener's
  followers or to modify playback.
- Permission is checked before Spotify is called, so an account without it gets
  an explanation rather than a 403 from Spotify it cannot act on.
- Revoking it in Spotify's account settings is enough; Encore reports the refusal
  and points at how to grant it again.
- Encore never deletes a playlist. "Stop managing" removes its own record and
  leaves the listener's library untouched.

## Secrets

No secret is committed to this repository. `.env` is git-ignored; `.env.example` contains only
placeholders. The CI workflow uses no secrets at all — it builds, tests and runs migrations against a
throwaway database.

Rotating `ENCORE_ENCRYPTION_KEY` is a deliberate, user-visible act: existing sealed tokens become
undecryptable and every user is asked to reconnect Spotify. No listening history is affected. There is
no automatic re-encryption, because doing it silently would mean holding both keys, which is worse.

## Reporting a vulnerability

Open a GitHub issue for anything that is already public. For something exploitable, contact the
maintainer privately first.
