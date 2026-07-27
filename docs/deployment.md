# Deploying Encore on your own server

This is the production path: Encore behind a reverse proxy that terminates TLS, on an x86-64 Linux
box you control. If you are just trying it out on your laptop, the quick start in the
[README](../README.md) is what you want instead.

---

## The constraint that shapes everything

**Spotify requires the OAuth redirect URI to be HTTPS.** The only exception is an explicit loopback
literal — `http://127.0.0.1:PORT` or `http://[::1]:PORT`. `localhost` has not been accepted since
April 2025, and every existing app had to migrate by November 2025.
([Spotify's redirect URI rules](https://developer.spotify.com/documentation/web-api/concepts/redirect_uri))

Two consequences:

- `http://192.168.1.50:3000/...` will **not** work as a redirect URI. A LAN address is neither HTTPS
  nor loopback. There is no flag to turn this off; it is enforced at the dashboard and again at the
  authorization endpoint.
- Therefore any deployment you browse to from a *different* machine needs a real certificate.

If you already run nginx with Let's Encrypt, you are done thinking about this — carry on below.

---

## Recommended shape

```
              ┌──────────────────────────── your server ────────────────────────────┐
              │                                                                     │
 browser ───► │  nginx :443 (your existing one, Let's Encrypt)                       │
   HTTPS      │        │ proxy_pass                                                 │
              │        ▼                                                            │
              │  web container 127.0.0.1:3000  (built client + /api proxy)          │
              │        │                                                            │
              │        ├──► api container    :8080  (bound to 127.0.0.1)            │
              │        └──► worker container :8081  (health and metrics only)       │
              │                     │                                               │
              │                     └──► postgres (not published at all)            │
              └─────────────────────────────────────────────────────────────────────┘
```

Your nginx has **one** upstream: the web container. That container already serves the client and
forwards `/api` to the API, both of which are covered by tests. Splitting the routing across two
proxies gains nothing and gives you two places for a path to go wrong.

Everything is served from a **single origin**, which is why `ENCORE_PUBLIC_URL` and `ENCORE_WEB_URL`
are the same value below. That keeps the session cookie `SameSite=Lax` with no CORS configuration.

---

## 1. Get the code onto the server

```bash
sudo mkdir -p /opt/encore && sudo chown "$USER" /opt/encore
git clone https://github.com/RequiDev/encore.git /opt/encore
cd /opt/encore
```

You still need the checkout for the Compose files, the nginx template and the
`.env`. Whether it also *builds* the images is up to you — see step 4.

## 2. Register the Spotify redirect URI

In the [developer dashboard](https://developer.spotify.com/dashboard), set the redirect URI to
exactly:

```
https://encore.example.com/api/auth/spotify/callback
```

It must match `ENCORE_PUBLIC_URL` + `/api/auth/spotify/callback` character for character. A trailing
slash or the wrong scheme gives `INVALID_CLIENT: Invalid redirect URI`.

## 3. Configure

```bash
cp .env.example .env
```

```dotenv
# One origin for both, because your nginx serves the client and the API together.
ENCORE_PUBLIC_URL=https://encore.example.com
ENCORE_WEB_URL=https://encore.example.com

ENCORE_SPOTIFY_CLIENT_ID=...
ENCORE_SPOTIFY_CLIENT_SECRET=...
ENCORE_ENCRYPTION_KEY=<openssl rand -base64 32>
POSTGRES_PASSWORD=<openssl rand -base64 24>

# Keep the container ports off the network. Only your nginx, on the same host,
# needs to reach them.
ENCORE_BIND_ADDR=127.0.0.1

# TLS is terminated in front, so the cookie must be marked Secure, and the
# X-Forwarded-For your nginx sets can be believed.
ENCORE_COOKIE_SECURE=true
ENCORE_TRUST_PROXY_HEADERS=true
ENCORE_ENV=production

# Statistics are bucketed in each user's own timezone; this only seeds new accounts.
ENCORE_DEFAULT_TIMEZONE=Europe/Berlin

# Optional but worth it on a public instance.
ENCORE_METRICS_USERNAME=metrics
ENCORE_METRICS_PASSWORD=<openssl rand -base64 24>
```

**Back up `ENCORE_ENCRYPTION_KEY` somewhere other than the server.** Losing it does not cost you any
listening history, but every user has to reconnect their Spotify account.

## 4. Start it

Two ways, and the first is the one to use on a server.

### From the published images (recommended)

CI builds and publishes `linux/amd64` and `linux/arm64` images to the GitHub
Container Registry on every green build of `main`. They are public, so no
`docker login` is needed:

```bash
docker compose -f docker-compose.yml -f docker-compose.ghcr.yml                -f docker-compose.prod.yml pull
docker compose -f docker-compose.yml -f docker-compose.ghcr.yml                -f docker-compose.prod.yml up -d
```

That is worth an alias, or put it in `/opt/encore/up.sh`. Nothing is compiled on
the server, so a 1 GB VPS or a Raspberry Pi is enough to run Encore even though
building it needs rather more.

| Image | Tags |
|---|---|
| `ghcr.io/requidev/encore` | API, worker and migration binaries |
| `ghcr.io/requidev/encore-web` | Web client behind nginx |

Both carry `latest` (the head of `main`), `sha-<commit>` for an exact pin, and
`1.2.3` / `1.2` once a version tag exists. Pin with `ENCORE_VERSION` in `.env`:

```dotenv
ENCORE_VERSION=sha-0f1c2d3e4f5a6b7c8d9e0f1a2b3c4d5e6f7a8b9c
```

Every published image carries a signed build attestation, so you can check it
came from this repository and not from someone else:

```bash
gh attestation verify oci://ghcr.io/requidev/encore:latest --owner RequiDev
```

### Building from source

Still supported, and what you want if you have local changes:

```bash
docker compose up -d --build
```

Then, either way:

```bash
docker compose ps                       # all healthy, migrate exited 0
curl -fsS http://127.0.0.1:8080/readyz  # {"status":"ok",...}
```

## 5. Point your nginx at it

Copy [`deploy/nginx-host.conf.example`](../deploy/nginx-host.conf.example), change the hostname and
certificate paths, and enable it:

```bash
sudo cp deploy/nginx-host.conf.example /etc/nginx/sites-available/encore.conf
sudo $EDITOR /etc/nginx/sites-available/encore.conf
sudo ln -s /etc/nginx/sites-available/encore.conf /etc/nginx/sites-enabled/
sudo nginx -t && sudo systemctl reload nginx
sudo certbot --nginx -d encore.example.com
```

### The two lines people forget

```nginx
client_max_body_size 8G;
proxy_request_buffering off;
```

A Spotify extended-streaming-history export is routinely hundreds of megabytes. nginx's **1 MB**
default rejects the upload at your proxy, before Encore ever sees the request — you get a bare `413`
and nothing at all in the application log to explain it. Raise it to at least
`ENCORE_IMPORT_MAX_UPLOAD_BYTES`, and turn off request buffering so nginx streams the body through
instead of spooling the whole thing to its own disk first.

## 6. Claim the administrator account

Open `https://encore.example.com` and sign in **before** giving anyone else the URL. The first
account to sign in becomes the administrator. Then go to **Settings → Admin** and turn off new
registrations once everyone who should have an account has one.

Your Spotify app also starts in *development mode*, which caps it at 25 listeners that you add by
hand under **Settings → User Management** in the dashboard. For a household that is plenty.

---

## Keeping it running

`restart: unless-stopped` is already set on every service, so enabling Docker at boot is all the
supervision Encore needs:

```bash
sudo systemctl enable docker
```

There is no drain step. Stopping the worker mid-import is safe at any instant: the lease expires and
the job resumes from its last committed checkpoint. See [`docs/import.md`](import.md).

### Log rotation

Docker's default `json-file` driver grows without limit, which on a long-running home server
eventually fills the disk. Either set a global default in `/etc/docker/daemon.json`:

```json
{ "log-driver": "json-file", "log-opts": { "max-size": "10m", "max-file": "5" } }
```

or use the bundled override, which applies the same limits to Encore's services only:

```bash
docker compose -f docker-compose.yml -f docker-compose.prod.yml up -d
```

### Backups

The database is the only irreplaceable thing. Spotify's recently-played feed reaches back just fifty
plays, so a lost week is lost permanently.

```bash
# /etc/cron.daily/encore-backup
cd /opt/encore && docker compose exec -T db \
  pg_dump -U encore -d encore --format=custom \
  | gzip > "/var/backups/encore-$(date +%F).dump.gz"
find /var/backups -name 'encore-*.dump.gz' -mtime +30 -delete
```

Full backup, restore and upgrade instructions, including how to verify a backup by restoring it, are
in [`docs/operations.md`](operations.md).

### Upgrades

From the published images:

```bash
cd /opt/encore
docker compose exec -T db pg_dump -U encore -d encore --format=custom > /var/backups/pre-upgrade.dump
git pull            # for the Compose files and any new configuration
docker compose -f docker-compose.yml -f docker-compose.ghcr.yml                -f docker-compose.prod.yml pull
docker compose -f docker-compose.yml -f docker-compose.ghcr.yml                -f docker-compose.prod.yml up -d
```

Or, building locally:

```bash
git pull && docker compose up -d --build
```

Migrations run as their own service that the API and worker wait on, so the ordering is handled for
you. `/readyz` reports not-ready while migrations are pending, which is what you want a monitor to
see.

---

## Sizing

Encore is undemanding. Peak import memory is a function of the batch size, not of the file size: a
million-record, 523 MB export peaks at **8 MiB of heap** and completes in about two minutes
([`docs/benchmarks.md`](benchmarks.md)).

| | Suggested |
|---|---|
| CPU | 2 cores is comfortable; 1 works |
| RAM | 1 GB for the whole stack, most of it PostgreSQL's cache |
| Disk | ~1 GB per million listens including indexes, plus whatever you keep in `ENCORE_IMPORT_DIR` |

Set `ENCORE_IMPORT_RETAIN_FILES=false` if you would rather not keep uploaded exports after their job
has been verified. They exist only so a job can be re-run.

After a very large import, refresh the planner's statistics once — the table's shape changes
dramatically in a few minutes:

```bash
docker compose exec -T db psql -U encore -d encore -c 'ANALYZE listens;'
```

---

## Verifying it works end to end

```bash
# 1. The stack is healthy
curl -fsS https://encore.example.com/readyz

# 2. The API is reachable through both proxies (401 = reached Encore, not signed in)
curl -s -o /dev/null -w '%{http_code}\n' https://encore.example.com/api/me

# 3. Sign-in redirects to Spotify with the right redirect_uri
curl -s -o /dev/null -w '%{redirect_url}\n' \
  https://encore.example.com/api/auth/spotify/login
```

The third command prints the `redirect_uri` Encore will send. If it does not match the dashboard
character for character, fix `ENCORE_PUBLIC_URL` rather than the dashboard — Encore derives the
redirect from it, and they must agree.

Common failures are collected under *Troubleshooting* in [`docs/operations.md`](operations.md).

---

## If you do not already have TLS

The three approaches that work on a home server without opening router ports:

| Approach | What you get | Trade-off |
|---|---|---|
| **Tailscale** | A real certificate on `<host>.<tailnet>.ts.net` via `tailscale cert`, reachable from your own devices anywhere | Only devices on your tailnet can reach it |
| **Caddy + DNS-01** | A real certificate on a domain you own, for a host with no inbound ports open | Needs a domain and an API token for its DNS provider |
| **Cloudflare Tunnel** | A real certificate and access from anywhere, nothing exposed on your router | Your traffic passes through Cloudflare |

All three end up in the same place as the setup above: something terminates TLS and proxies to
`127.0.0.1:3000`. Only step 5 changes.
