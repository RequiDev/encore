# Encore Go services: api, worker and migrate.
#
# One build stage compiles all three binaries so the (slow) dependency download
# and the (large) build cache are shared. Each service then selects its
# entrypoint from the same final image, which keeps the three images layer
# identical and makes a deployment atomic: they cannot drift apart.

# --- build -----------------------------------------------------------------
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build

WORKDIR /src

# Dependencies first so a source-only change does not re-download the module cache.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY . .

ARG VERSION=dev

# Cross-compile rather than emulate. Buildx sets these for whichever platform it
# is producing; the build stage itself always runs natively, so building the
# arm64 image on an amd64 runner costs nothing beyond a second `go build`.
# Emulating the toolchain under QEMU instead would take tens of minutes.
ARG TARGETOS
ARG TARGETARCH

# CGO is not needed: pgx is pure Go. A static binary lets the runtime image be
# distroless-small without a libc, and cross-compiles without a cross toolchain.
ENV CGO_ENABLED=0
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" \
        -o /out/encore-api     ./cmd/encore-api  && \
    GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" \
        -o /out/encore-worker  ./cmd/encore-worker && \
    GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" \
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

# CMD rather than ENTRYPOINT, deliberately. All three binaries live in this one
# image, and an ENTRYPOINT would turn `command:` in docker-compose.yml — and any
# argument to `docker run` — into arguments *for encore-api* instead of into a
# choice of binary. That failure is quiet and confusing: the migrate service
# starts the API and then complains about unrelated missing configuration.
CMD ["/usr/local/bin/encore-api"]
