# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

Cotton is a push notification platform built with Go, using PostgreSQL, ClickHouse, and NATS for data storage and messaging.

## Build & Run Commands

```bash
# Build the Go binary
make build

# Run tests
make test

# Start dev infrastructure (PostgreSQL, NATS, ClickHouse)
make infra

# Stop infrastructure
make infra-down

# Run database migrations
./bin/cotton postgres migrate
./bin/cotton nats migrate
./bin/cotton clickhouse migrate

# Start development server + workers together
./bin/cotton dev

# Start server only
./bin/cotton server

# Start individual workers
./bin/cotton worker device
./bin/cotton worker campaign
./bin/cotton worker events
./bin/cotton worker profile register
./bin/cotton worker profile identify
./bin/cotton worker profile alias
./bin/cotton worker scheduler
```

### Code Generation

```bash
# Generate sqlc queries (after modifying SQL files)
make sqlc

# Generate protobuf code (after modifying .proto files)
make rpc

# Lint Go code
make lint

# Lint proto files
make lint-proto
```

## Architecture

### Backend (Go)

The backend follows a layered architecture with Connect RPC (HTTP/2):

- **`internal/app/`** - CLI entry points using Cobra, split by feature (server, workers, dev, migrate)
  - `server/rpc/` - RPC handlers that map proto services to business logic
  - `workers/campaigns/`, `workers/devices/`, `workers/profiles/`, `workers/events/`, `workers/scheduler/` - NATS message consumers
- **`internal/core/`** - Business logic layer with service and repo per domain (auth, campaigns, delivery, devices, orgs, profiles, projects)
- **`internal/gen/`** - Generated code (do not edit manually)
  - `proto/` - Generated from .proto files via buf
  - `repo/dbread/`, `repo/dbwrite/` - Generated from SQL via sqlc

### Database Pattern

PostgreSQL uses read/write separation:

- Queries in `schema/postgres/queries/read/` generate to `internal/gen/repo/dbread/`
- Queries in `schema/postgres/queries/write/` generate to `internal/gen/repo/dbwrite/`

**sqlc conventions**:

- Query names: PascalCase with uppercase `ID` (e.g., `GetCampaignByID`, `GetProjectsByOrgID`)
- SQL syntax and identifiers: lowercase (e.g., `select * from campaigns where project_id = @project_id`)
- Partial updates: use `coalesce(nullif(@field, ''), field)` to preserve existing values when empty

### Org Hierarchy

- **Org** is the top-level entity. Each customer belongs to one or more orgs via `org_members` (role: `ORG_ROLE_ADMIN` | `ORG_ROLE_MEMBER`).
- **Projects** belong to an org (`org_id`). A project is always created within an org context.
- **Invitations** (`org_invitations`) are token-based, expire after 7 days, and transition from `INVITATION_STATUS_PENDING` → `INVITATION_STATUS_ACCEPTED`. Expiry is checked at accept time, not via a status transition.
- Admin-only operations: `UpdateDisplayName`, `RemoveMember`, `InviteMember`, `ListInvitations`. All other org endpoints require membership.
- On sign-up, a default org and default project are created atomically in a single transaction.

### Auth & Principal

RPC handlers authenticate via `connectrpc.com/authn` middleware. Three auth modes are supported:

- **`WithJWTAuth`** — Dashboard auth. Sets `Principal.Customer` (always non-nil). Optionally sets `Principal.Project` if `x-project-id` header is provided and the customer is an org member.
- **`WithSDKAuth`** — Public API key auth. Sets `Principal.Project` only. `Principal.Customer` is nil.
- **`WithDualAuth`** — Private API key or JWT fallback. API key path sets `Principal.Project` only; JWT path behaves like `WithJWTAuth`.

`Principal.Customer` is `*dbread.Customer` — it is nil for API key auth paths. Always use the appropriate extractor:

- **`MustGetPrincipalWithCustomer`** — use in dashboard handlers that access `principal.Customer`. Returns `CodeUnauthenticated` if Customer is nil.
- **`MustGetPrincipalWithProject`** — use in handlers that require a project context (`x-project-id` header). Returns `CodeUnauthenticated` if Project is nil.

Never call `getPrincipalFromContext` directly in handlers.

### Proto/RPC

Services defined in `proto/` directory. Generated code goes to `internal/gen/proto/`. Uses Connect RPC with gRPC reflection enabled.

**Validation:** Prefer `buf/validate` (protovalidate) annotations in `.proto` files over hand-written Go validation code. The `validate.NewInterceptor()` in the server enforces all proto annotations before handlers run. Use CEL expressions for cross-field constraints (e.g., `this.from < this.to`, operator-dependent required fields). Only add Go-side validation as defense-in-depth for public functions in shared packages that may be called outside the RPC chain.

### Event Enrichment

Incoming events are enriched with auto-properties before being published to NATS:

- **`internal/geo/`** — resolves geo properties (`$country`, `$city`, `$ip`, etc.) from proxy-injected HTTP headers. `geo.Provider` is an interface; the Cloudflare implementation reads from `CF-Connecting-IP` and `CF-*` headers. Geo properties are **always overwritten** (CDN-injected values are trusted).
- **`internal/useragent/`** — parses the `User-Agent` header using `ua-parser/uap-go` into properties: `$browser`, `$browserVersion`, `$os`, `$osVersion`, `$device`. Both `browserVersion` and `osVersion` use the major version only (e.g. `"17"` not `"17.2.1"`) to avoid analytics fragmentation. UA properties are only written if not already present in `event.AutoProperties` (client-supplied values win). `$device` is only set when the parser identifies a specific device (e.g., "iPhone", "Pixel 8"); desktop browsers typically yield no `$device` property. The parser is initialized once at startup via `useragent.NewParser()` to avoid reloading regex definitions per request.

### ClickHouse Events Table

- **Engine:** `ReplacingMergeTree(insert_time)` — on merge, keeps the row with the highest `insert_time` per dedup key. Use `SELECT ... FINAL` only when exact deduplication matters (e.g., single-user event history). Skip `FINAL` for aggregate analytics queries (trends, counts) where background merges provide eventual consistency and `FINAL` would hurt performance.
- **Dedup key (ORDER BY):** `(project_id, toStartOfMinute(occur_time), kind, event_id)` — minute granularity matches the finest time resolution dashboards use (per-minute charts). Full-precision `occur_time` is stored in the column.
- **Partitioning:** `PARTITION BY toYYYYMM(occur_time)` — ReplacingMergeTree **never** deduplicates across partitions.
- **occur_time stability:** `occur_time` is required (enforced by proto validation). Clients must send a stable value on retries — a different value that crosses a minute boundary lands in a different sort-key bucket (dedup fails); if it crosses a month boundary it lands in a different partition (permanent duplicate).

## Code Style

- Standard Go conventions. Use slog for logging. Run `go fmt ./...` after each change. A PostToolUse hook auto-runs `goimports` on every `.go` file edit.
- Always use context-aware slog variants (`slog.InfoContext`, `slog.ErrorContext`, `slog.WarnContext`, `slog.DebugContext`) instead of `slog.Info`, `slog.Error`, etc.
- Always use `slogx.Error(err)` (from `internal/slogx`) for logging errors. Never use `slog.Any("error", err)` or `slog.Any("err", err)`.
- Never pass sentinel errors directly to `connect.NewError`. Always create an explicit client-facing message with `errors.New("...")`. Sentinel errors are internal and their strings must not leak to API consumers.
