# syntax=docker/dockerfile:1
#
# One parameterized image per pug role. `CMD` selects which ./cmd/<CMD> main to
# compile; the runtime target selects the filesystem shape:
#   --build-arg CMD=server                 --target app      -> pug-server
#   --build-arg CMD=workers/events         --target app      -> pug-worker-events
#   --build-arg CMD=migrate/postgres       --target migrate  -> pug-migrate-postgres
#
# app target     = distroless + binary only (server + workers read nothing from schema/).
# migrate target = distroless + binary + schema/ (migrate roles read these from WORKDIR).
#
# Multi-arch (amd64 + arm64) via Go cross-compilation: the build stage runs on the
# native builder arch ($BUILDPLATFORM) and cross-compiles to $TARGETARCH (no QEMU).

ARG CMD=server

# ---- build ----
FROM --platform=$BUILDPLATFORM golang:1.26.3-bookworm AS build
WORKDIR /src

# Module download as its own cached layer (only re-runs when go.mod/go.sum change).
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY . .

ARG CMD
ARG TARGETOS
ARG TARGETARCH
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o /out/app ./cmd/${CMD}

# ---- app runtime: server + workers, binary only ----
FROM gcr.io/distroless/static-debian12:nonroot AS app
WORKDIR /app
COPY --from=build /out/app /app/app
# Documents the default server port (PUG_SERVER_PORT). Informational only.
EXPOSE 3000
USER nonroot:nonroot
ENTRYPOINT ["/app/app"]

# ---- migrate runtime: binary + schema assets ----
# WORKDIR must stay /app so the migrate roles resolve schema/ relative to cwd.
FROM gcr.io/distroless/static-debian12:nonroot AS migrate
WORKDIR /app
COPY --from=build /src/schema/postgres/migrations   ./schema/postgres/migrations
COPY --from=build /src/schema/clickhouse/migrations ./schema/clickhouse/migrations
COPY --from=build /src/schema/nats                  ./schema/nats
COPY --from=build /out/app /app/app
USER nonroot:nonroot
ENTRYPOINT ["/app/app"]
