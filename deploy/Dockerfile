# Encore Go services: api, worker and migrate.
#
# One build stage compiles all three binaries so the (slow) dependency download
# and the (large) build cache are shared. Each service then selects its
# entrypoint from the same final image, which keeps the three images layer
# identical and makes a deployment atomic: they cannot drift apart.

# --- build -----------------------------------------------------------------
FROM golang:1.26-alpine AS build

WORKDIR /src

# Dependencies first so a source-only change does not re-download the module cache.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY . .

ARG VERSION=dev
# CGO is not needed: pgx is pure Go. A static binary lets the runtime image be
# distroless-small without a libc.
ENV CGO_ENABLED=0
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" \
        -o /out/encore-api     ./cmd/encore-api  && \
    go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" \
        -o /out/encore-worker  ./cmd/encore-worker && \
    go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" \
        -o /out/encore-migrate ./cmd/encore-migrate

# --- runtime ---------------------------------------------------------------
FROM alpine:3.22 AS runtime

# ca-certificates for the Spotify API; tzdata because every statistic is bucketed
# in the user's own timezone and the binary loads IANA zones at runtime.
RUN apk add --no-cache ca-certificates tzdata wget && \
    addgroup -g 10001 -S encore && \
    adduser -u 10001 -S -G encore -h /var/lib/encore encore

COPY --from=build /out/encore-api     /usr/local/bin/encore-api
COPY --from=build /out/encore-worker  /usr/local/bin/encore-worker
COPY --from=build /out/encore-migrate /usr/local/bin/encore-migrate

# Uploaded exports live here and must be a shared, durable volume: a resumed
# import re-reads the original file.
RUN mkdir -p /var/lib/encore/imports && chown -R encore:encore /var/lib/encore

USER encore
WORKDIR /var/lib/encore
EXPOSE 8080

ENTRYPOINT ["/usr/local/bin/encore-api"]
