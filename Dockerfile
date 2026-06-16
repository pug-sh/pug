# syntax=docker/dockerfile:1
#
# Single image for the whole pug platform. cmd/pug is a multitool — the role is
# chosen by the container's args, e.g.
#   pug server
#   pug worker events
#   pug postgres migrate   (also: nats migrate, clickhouse migrate)
#
# Multi-arch (amd64 + arm64) via Go cross-compilation: the build stage always
# runs on the native builder arch ($BUILDPLATFORM) and cross-compiles to the
# requested $TARGETARCH, so no QEMU emulation is needed.

# ---- build ----
FROM --platform=$BUILDPLATFORM golang:1.26.3-bookworm AS build
WORKDIR /src

# Module download as its own cached layer (only re-runs when go.mod/go.sum change).
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY . .

ARG TARGETOS
ARG TARGETARCH
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o /out/pug ./cmd/pug

# ---- runtime ----
FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /app

# Runtime-needed schema assets only, all read relative to the working directory:
#   - postgres|clickhouse migrate roles read schema/{postgres,clickhouse}/migrations
#   - every worker reads schema/nats/consumers.yaml at startup (GetConsumerConfigByName)
# The sqlc query files under schema/postgres/queries are build-time only and
# deliberately omitted; the server itself reads nothing from schema/. WORKDIR must
# stay /app — do not override the container's workingDir, or these won't resolve.
COPY --from=build /src/schema/postgres/migrations ./schema/postgres/migrations
COPY --from=build /src/schema/clickhouse/migrations ./schema/clickhouse/migrations
COPY --from=build /src/schema/nats ./schema/nats
COPY --from=build /out/pug /app/pug

# Documents the default server port (PUG_SERVER_PORT). Informational only.
EXPOSE 3000

USER nonroot:nonroot

# No ENTRYPOINT/CMD by design: every k8s workload sets `command: ["/app/pug"]`
# plus `args` (e.g. server | worker events | postgres migrate). WORKDIR stays
# /app so the migrate roles resolve schema/. To run the image directly:
#   docker run <image> /app/pug server
