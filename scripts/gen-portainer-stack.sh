#!/usr/bin/env bash
# Regenerate docker-compose.portainer.yml from the two files it merges.
#
# Portainer's stack web editor accepts one Compose file, so the base file and
# the server overlay have to be flattened into a single document. Doing that
# by hand guarantees it drifts, so it is generated here and CI checks that the
# committed copy still matches its sources.
#
#   ./scripts/gen-portainer-stack.sh
set -euo pipefail
cd "$(dirname "$0")/.."

# --no-interpolate keeps ${VARIABLES} as variables rather than resolving them
# against whatever happens to be in the generating shell's environment, which
# would bake real secrets into a committed file.
body=$(docker compose \
  -f docker-compose.yml \
  -f docker-compose.server.yml \
  config --no-interpolate)

# Strip the names `docker compose config` resolves on our behalf.
#
# The generator expands the project name into the network and, critically, into
# every volume: `encore-db` is emitted as `name: encore_encore-db`, pinned
# absolutely. A stack deployed from that would attach to those exact volumes
# whatever Portainer called it, so two Encore stacks on one host would silently
# share a database — and `down -v` on the second would destroy the first one's
# data. Removing the pins lets Portainer scope volumes to its own stack name,
# which is what anyone deploying it will expect.
body=$(printf '%s\n' "$body" | awk '
  /^name:/                      { next }
  /^networks:$/                 { skip_net = 1; in_vol = 0; next }
  /^volumes:$/                  { skip_net = 0; in_vol = 1; print; next }
  /^[a-z][a-zA-Z_-]*:/          { skip_net = 0; in_vol = 0 }
  skip_net                      { next }
  in_vol && /^[ \t]+name:[ \t]/ { next }
                                { print }
')

cat > docker-compose.portainer.yml <<'HEADER'
# Encore as a single Compose file, for Portainer's stack web editor.
#
# GENERATED - do not edit. Regenerate with ./scripts/gen-portainer-stack.sh
#
# This is docker-compose.yml + docker-compose.server.yml flattened, because the
# stack editor takes exactly one file. CI fails if this copy stops matching the
# two it comes from.
#
# Portainer: Stacks -> Add stack -> Web editor. Paste this in, then fill in the
# values below under "Environment variables". Nothing is built on the server:
# the images are pulled from ghcr.io, so no source checkout is needed.
#
# Required:
#   ENCORE_PUBLIC_URL, ENCORE_WEB_URL, ENCORE_SPOTIFY_CLIENT_ID,
#   ENCORE_SPOTIFY_CLIENT_SECRET, ENCORE_ENCRYPTION_KEY, POSTGRES_PASSWORD
#
# Behind a reverse proxy, also set:
#   ENCORE_BIND_ADDR=127.0.0.1  and  ENCORE_COOKIE_SECURE=true
#
# Reference: docs/configuration.md and docs/deployment.md
HEADER
printf '\n%s\n' "$body" >> docker-compose.portainer.yml

# The whole point of stripping the pins is that they end up gone. Fail loudly
# rather than shipping a file that quietly captures another stack's volumes.
if grep -qE '^[[:space:]]+name:[[:space:]]*encore_' docker-compose.portainer.yml; then
  echo "generated file still pins volume or network names" >&2
  exit 1
fi
if grep -q 'build:' docker-compose.portainer.yml; then
  echo "generated file still has a build section; Portainer has no source to build from" >&2
  exit 1
fi

echo "wrote docker-compose.portainer.yml"
