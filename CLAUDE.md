# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

Cotton is an events ingestion platform built with Go, using PostgreSQL, ClickHouse, and NATS for data storage and messaging. It also provides push notification capabilities (campaigns, delivery, devices).

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
./bin/cotton worker profile identify
./bin/cotton worker profile alias
./bin/cotton worker profile upsert
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
- **`WithSDKAuth`** — API key auth (public or private key). Sets `Principal.Project` only. `Principal.Customer` is nil.
- **`WithDualAuth`** — Private API key or JWT fallback. API key path sets `Principal.Project` only; JWT path behaves like `WithJWTAuth`.

`Principal.Customer` is `*dbread.Customer` — it is nil for API key auth paths. Always use the appropriate extractor:

- **`MustGetPrincipalWithCustomer`** — use in dashboard handlers that access `principal.Customer`. Returns `CodeUnauthenticated` if Customer is nil.
- **`MustGetPrincipalWithProject`** — use in handlers that require a project context (`x-project-id` header). Returns `CodeUnauthenticated` if Project is nil.

Never call `getPrincipalFromContext` directly in handlers.

### Proto/RPC

Services defined in `proto/` directory, organized by auth boundary (`public/`, `sdk/`, `dashboard/`, `shared/`). Generated code goes to `internal/gen/proto/`. Uses Connect RPC with gRPC reflection enabled. Profiles is split into `ProfilesSDKService` (sdk — Identify) and `ProfilesService` (shared — Get, GetByExternalId, List, Delete). SDK profiles uses Go import alias `sdkprofilesv1` to avoid collision with shared `profilesv1`.

**Validation:** Prefer `buf/validate` (protovalidate) annotations in `.proto` files over hand-written Go validation code. The `validate.NewInterceptor()` in the server enforces all proto annotations before handlers run. Use CEL expressions for cross-field constraints (e.g., `this.from < this.to`, operator-dependent required fields). Only add Go-side validation as defense-in-depth for public functions in shared packages that may be called outside the RPC chain.

**Proto directory layout mirrors the handler auth boundary:**

- **`proto/public/`** — no auth (e.g., auth service)
- **`proto/sdk/`** — API key auth (public or private). Write-only — never expose read endpoints or return sensitive data. Public keys are extractable from client apps, so SDK endpoints must assume an untrusted caller regardless of key type.
- **`proto/dashboard/`** — JWT only (e.g., orgs, projects, insights)
- **`proto/shared/`** — private API key or JWT (e.g., campaigns, delivery, profiles read/delete)
- **`proto/common/v1/`** — shared message types with no service definitions, accessible from any auth level. Only put types here if they are needed across auth boundaries. If a message is only used behind private key + JWT, it belongs in `shared/`.

### Insights Filter Model

- Top-level insights filters are **group-based only**. In `shared.insights.v1`, use `filter_groups` and `filter_groups_operator` on `QueryRequest` and `SegmentUsersRequest`.
- Legacy top-level `filters` fields are removed/reserved in `proto/shared/insights/v1/insights.proto`. Do not reintroduce them.
- Group semantics:
  - Within a group, conditions are combined using `FilterGroup.operator` (`AND` by default when unspecified).
  - Between groups, conditions are combined using `filter_groups_operator` (`AND` by default when unspecified).
- Per-event filters remain on `EventQuery.event.filters` and are independent of top-level filter groups.

### Retention Insight

- `shared.insights.v1.InsightType` supports `INSIGHT_TYPE_RETENTION`.
- Retention query semantics in `QueryRequest.events`:
  - `events[0]` = cohort/start event (required)
  - `events[1]` = return event (optional; defaults to `events[0]` when omitted)
- Retention responses use `QueryResponse.retention` (a `RetentionResult`):
  - one `RetentionCohort` per cohort bucket
  - `RetentionCohort.cohort` stores the cohort timestamp (RFC3339)
  - `RetentionCohort.cohort_size` stores the number of users in the cohort
  - `RetentionCohort.points[].value` is retention percentage (`0..100`) across time buckets

### Event Enrichment

Incoming events are enriched with auto-properties before being published to NATS:

- **`internal/geo/`** — resolves geo properties (`$country`, `$city`, `$ip`, etc.) from proxy-injected HTTP headers. `geo.Provider` is an interface; the Cloudflare implementation reads from `CF-Connecting-IP` and `CF-*` headers. Geo properties are **always overwritten** (CDN-injected values are trusted).
- **`internal/useragent/`** — parses the `User-Agent` header using `ua-parser/uap-go` into properties: `$browser`, `$browserVersion`, `$os`, `$osVersion`, `$device`. Both `browserVersion` and `osVersion` use the major version only (e.g. `"17"` not `"17.2.1"`) to avoid analytics fragmentation. UA properties are only written if not already present in `event.AutoProperties` (client-supplied values win). `$device` is only set when the parser identifies a specific device (e.g., "iPhone", "Pixel 8"); desktop browsers typically yield no `$device` property. The parser is initialized once at startup via `useragent.NewParser()` to avoid reloading regex definitions per request.

### Profile Properties

Profiles store properties as a single JSONB field (`properties`) rather than separate `auto_properties` and `custom_properties` fields. This simplifies the data model while preserving the ability to distinguish between auto-populated and custom properties at the application level through property naming conventions (e.g., `$` prefix for auto-properties).

### Profile Soft-Delete

Profiles use soft-delete via a `deletion_time timestamptz` column (NULL = active). All read queries filter `deletion_time IS NULL`. The `SoftDeleteProfileByIDAndProjectID` query sets `deletion_time = now()` — it never hard-deletes. The `deletion_time IS NULL` guard makes soft-delete idempotent (returns 0 rows if already deleted).

ClickHouse profiles use `is_deleted UInt8` for the same purpose. The identify worker and dashboard delete handler both publish `ProfileUpsertMessage` with `is_deleted=true` to sync soft-deletes to ClickHouse.

### Device Subscriptions

`profile_devices.profile_id` is nullable. Devices can be registered anonymously (no profile exists yet). When the SDK later calls Subscribe with a `profile_id` or `profile_external_id`, the upsert links the device via `coalesce(excluded.profile_id, profile_devices.profile_id)` — a re-subscribe with a profile never unlinks an already-linked device.

The FK uses `ON DELETE SET NULL` — if a profile row is ever hard-deleted (e.g. project cascade), devices become unlinked rather than silently destroyed.

### ClickHouse Query Builder

Use `internal/core/clickhouse/query.go` for building ClickHouse queries. It provides a type-safe query builder with parameterized arguments:

```go
import "github.com/fivebitsio/cotton/internal/core/clickhouse"

q := clickhouse.NewQuery().
    Select("project_id", "kind", "count() AS event_count").
    From("events").
    Where(clickhouse.Eq("project_id", projectID)).
    GroupBy("project_id", "kind").
    OrderBy("event_count DESC")

sql, args, err := q.Build()
```

Key types and functions:
- **`clickhouse.NewQuery()`** — creates a new query builder
- **`Condition`** — represents a WHERE clause with SQL + args; use builders like `Eq()`, `Neq()`, `Gt()`, `Lt()`, `Gte()`, `Lte()`, `RawCond()`
- **`And()`**, **`Or()`** — combine conditions (skip zero-value conditions)
- **`Query.Select()`**, **`Query.From()`**, **`Query.Where()`**, **`Query.GroupBy()`**, **`Query.OrderBy()`**, **`Query.Limit()`** — chain query parts
- **`Query.Build()`** — returns SQL string, args, and error

### ClickHouse Events Table

- **Engine:** `ReplacingMergeTree(insert_time)` — on merge, keeps the row with the highest `insert_time` per dedup key. Avoid `SELECT ... FINAL` — it forces synchronous deduplication at query time and is expensive. Background merges provide eventual consistency, which is sufficient for all current queries including per-user event history. Only use `FINAL` if a query has a hard correctness requirement that cannot tolerate transient duplicates.
- **Dedup key (ORDER BY):** `(project_id, toStartOfMinute(occur_time), kind, event_id)` — minute granularity matches the finest time resolution dashboards use (per-minute charts). Full-precision `occur_time` is stored in the column.
- **Partitioning:** `PARTITION BY toYYYYMM(occur_time)` — ReplacingMergeTree **never** deduplicates across partitions.
- **occur_time stability:** `occur_time` is required (enforced by proto validation). Clients must send a stable value on retries — a different value that crosses a minute boundary lands in a different sort-key bucket (dedup fails); if it crosses a month boundary it lands in a different partition (permanent duplicate).

### ClickHouse Query Builder Conventions

- Prefer `internal/core/clickhouse` query builder for ClickHouse query construction in core packages (`insights`, `events`, filters-related query helpers).
- Use parameterized limits (`LIMIT ?`) through `Query.Limit(...)` and pass `int64` values consistently.
- Use `RawCond(...)` only for expression-level fragments that are awkward to model otherwise (for example `occur_time >= now() - INTERVAL 30 DAY` or `IN ?` tuple bindings). Keep full query structure (`SELECT/FROM/WHERE/GROUP/ORDER/LIMIT`) in the builder.
- For property-values query helpers, query builder methods now return build errors; callers must propagate those errors instead of relying on raw-SQL fallbacks.

## Code Style

- Standard Go conventions. Use slog for logging. Run `go fmt ./...` after each change. A PostToolUse hook auto-runs `goimports` on every `.go` file edit.
- Always use context-aware slog variants (`slog.InfoContext`, `slog.ErrorContext`, `slog.WarnContext`, `slog.DebugContext`) instead of `slog.Info`, `slog.Error`, etc.
- Always use `slogx.Error(err)` (from `internal/slogx`) for logging errors. Never use `slog.Any("error", err)` or `slog.Any("err", err)`.
- Never pass sentinel errors directly to `connect.NewError`. Always create an explicit client-facing message with `errors.New("...")`. Sentinel errors are internal and their strings must not leak to API consumers.
