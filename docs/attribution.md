# Attribution and licensing

## Relationship to your_spotify

Encore was written after studying [your_spotify](https://github.com/Yooooomi/your_spotify) by
Yooooomi, which is licensed under the **GNU General Public License v3.0**.

What was taken from that project:

- Its **publicly documented feature set** — the dashboard, the statistics it shows, the two import
  methods, per-user timezones, the artist blacklist, the administrator's ability to close
  registrations, and the shape of its Prometheus support. These are ideas and user-facing
  capabilities, not expression.
- Its **documentation**, specifically the README's description of the two Spotify export formats and
  the workflow for requesting them.
- Its **public issue tracker**, which is where the awkward real-world edge cases in Spotify's export
  formats are recorded: renamed files, the `ip_addr_decrypted` to `ip_addr` change, audiobook and
  video entries appearing in music exports, and null fields where a value is expected. Several of
  Encore's parser tests exist because of bugs reported there.

What was **not** taken:

- No source code was copied, translated, or transliterated, in whole or in part.
- No database schema, query, algorithm implementation, asset, string, or configuration file was
  copied.

Encore is an independent implementation. It is written in Go rather than TypeScript, stores data in
PostgreSQL rather than MongoDB, uses a different authentication model, a different duplicate-detection
strategy, a different import architecture, and its own visual design. The two projects share a
purpose and a problem domain, not an implementation.

Because no GPL-3.0 licensed code was incorporated, Encore is not a derivative work of your_spotify and
is not obliged to adopt its licence. If you believe any part of this repository does derive from that
project, please open an issue and it will be removed or relicensed.

## Encore's licence

Encore is released under the MIT Licence. See [`LICENSE`](../LICENSE).

## Spotify

Encore is not affiliated with, endorsed by, or sponsored by Spotify AB.

"Spotify" is a trademark of Spotify AB. Encore uses the Spotify Web API under the
[Spotify Developer Terms](https://developer.spotify.com/terms) and requires each operator to register
their own Spotify application. Encore requests eight read scopes at sign-in
(`user-read-recently-played`, `user-read-private`, `user-read-email`, `user-top-read`,
`user-library-read`, `user-follow-read`, `playlist-read-private`, `user-read-playback-state`) and never
holds a grant that can modify a listener's Spotify account unless they have used a feature that needs
it. Creating or renaming a playlist, and giving it a cover, are the only such features; they ask for
their two write scopes together, at the moment one of them is first used, and Encore never asks for
permission to control playback.

Album art and artist images are served as URLs from Spotify's own CDN and are not copied or cached by
Encore.

## Third-party dependencies

Go modules used at runtime:

| Module | Licence | Why |
|---|---|---|
| `github.com/jackc/pgx/v5` | MIT | PostgreSQL driver and connection pool |
| `github.com/pressly/goose/v3` | MIT | Schema migration runner with advisory locking |
| `github.com/prometheus/client_golang` | Apache-2.0 | Metrics exposition |
| `github.com/google/uuid` | BSD-3-Clause | Identifier values |
| `golang.org/x/crypto` | BSD-3-Clause | Cryptographic primitives |
| `golang.org/x/text` | BSD-3-Clause | Unicode normalisation for name matching |
| `golang.org/x/sync` | BSD-3-Clause | Bounded concurrency helpers |

Test-only: `github.com/testcontainers/testcontainers-go` (MIT), `github.com/stretchr/testify` (MIT).

Web client dependencies and their licences are listed in `web/package.json`; all are MIT or ISC.

To audit the full dependency tree yourself:

```bash
go list -m all
npm --prefix web ls --all
```
