# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

Pug is an events ingestion platform built with Go, using PostgreSQL, ClickHouse, and NATS for data storage and messaging. It also provides push notification capabilities (campaigns, delivery, devices).

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
./bin/pug postgres migrate
./bin/pug nats migrate
./bin/pug clickhouse migrate

# Start development server + workers together
./bin/pug dev

# Start server only
./bin/pug server

# Start individual workers
./bin/pug worker device
./bin/pug worker campaign
./bin/pug worker events
./bin/pug worker profile identify
./bin/pug worker profile alias
./bin/pug worker profile upsert
./bin/pug worker scheduler
```

### Code Generation

```bash
# Generate sqlc queries (after modifying SQL files)
make sqlc

# Generate protobuf code (after modifying .proto files)
make rpc

# Generate templ email templates (after modifying .templ files)
make templ

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
- **Invitations** (`org_invitations`) are pending membership records that expire after 7 days and transition from `INVITATION_STATUS_PENDING` → `INVITATION_STATUS_ACCEPTED`. The redeemable invite secret is stored only as a hash in `email_action_tokens` (`purpose = emailaction.PurposeOrgInvite`, value `"org_invite"` — deliberately distinct from the auth login purpose `emailaction.PurposeMagicLink` / `"magic_link"` so that issuing or superseding a passwordless login link, which invalidates active tokens by `(email, purpose)`, can never consume a pending invite token); `org_invitations.token` is a non-redeemable storage value (rotated on resend via `RefreshOrgInvitationDelivery`) and is never returned by any RPC. Expiry is checked at accept time, not via a status transition. `ResendInvite` rotates the storage token, refreshes `expires_at`, invalidates any prior `email_action_tokens` for the invitation, and issues a fresh redeemable token — it does **not** change `status`; only acceptance flips PENDING → ACCEPTED. Acceptance for a customer who is **already** a member still flips `status` to ACCEPTED (and returns `ErrAlreadyMember`) rather than leaving a stranded PENDING row whose token has been consumed.
- Admin-only operations: `UpdateDisplayName`, `RemoveMember`, `InviteMember`, `ResendInvite`, `ListInvitations`. All other org endpoints require membership.
- New accounts are created by completing a magic link — there is no password-signup endpoint (`SignUpWithEmail` was removed). Completing a *plain* (non-invite) magic link for a new email provisions a default org + project atomically (in `CompleteMagicLink`); an *invite* magic link instead joins the inviting org with the invitation's role. `CompleteMagicLink` looks the token up by hash (unique) and dispatches on its `purpose`: it honors only `"magic_link"` and `"org_invite"`, rejecting any other purpose with `ErrInvalidToken` so a token minted for a future flow can't be redeemed as a login. Password login (`SignInWithEmail`) and authenticated `SetPassword` remain, so a magic-link account can opt into a password and then sign in with it.

### Dashboards

Dashboards belong to a **project** (`dashboard.dashboards.v1.DashboardsService`, JWT + `x-project-id`). Tiles are stored in PostgreSQL (`dashboard_tiles`).

- **Tile kinds:** `insight` (persisted `insight_query` JSONB + `view_mode` / `default_time_range` columns) or `markdown` (`markdown_body`). Insight tiles require a valid `shared.insights.v1.QueryRequest` payload; markdown tiles ignore time-range fields.
- **Column vs JSON split:** `view_mode` and `default_time_range` are dedicated range-checked columns mirroring `TileViewMode` / `TileDefaultTimeRange` in `internal/core/dashboards/dashboards.go`. `granularity`, absolute `time_range`, breakdowns, group-by, and filters live in `insight_query`. `UpdateTile` full-replaces `view_mode`, `default_time_range`, `layouts`, and `insight_query` — clients must send them on every update or they reset.
- **`QueryDashboard`:** batch-executes insight tiles server-side via `coredashboards.QueryDashboardTiles` → `coreinsights.ExecuteQuery` (same execution path as `shared.insights.v1.InsightsService.Query`). Markdown tiles are omitted. Results are returned in dashboard tile order, keyed by `tile_id`. Per-tile failures populate `DashboardTileQueryResult.error_message` (a `oneof` with `result`) without failing the whole RPC.
- **Effective time range priority** (when building each tile query): request `time_range_override` → stored `insight_query.time_range` when valid (`from`/`to` set, `from < to`) → `default_time_range` preset resolved through `ResolveDashboardTimeRangePreset`. Optional `granularity_override`; otherwise use the stored query granularity (defaulting to `DAY` when unspecified).
- **Preset normalization:** DB enum names round-trip through `normalizedTileDefaultTimeRange` / `tileDefaultTimeRangeDBName`; read-side mapping uses `TileDefaultTimeRangePresetFromDB`. Insight tiles normalize unknown/`UNSPECIFIED` presets to `LAST_30_DAYS`; markdown tiles coerce any preset to `UNSPECIFIED`.
- **Handler wiring:** `dashboardsrpc.NewServer(dashboardsSvc, insightsExecutor)` — the dashboards handler needs the dashboards service and insights executor.
- Deeper query/filter conventions → [`docs/architecture/insights.md`](docs/architecture/insights.md)

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

**Validation:** Always use `buf/validate` (protovalidate) annotations in `.proto` files for request validation. The `validate.NewInterceptor()` in the server enforces all proto annotations before handlers run. Use CEL expressions for cross-field constraints (e.g., `this.from < this.to`, operator-dependent required fields, ordered values in repeated fields, map-key prefix checks via `map.all(k, k.startsWith('$'))`). Do **not** duplicate proto validations in Go code — if protovalidate already enforces a constraint, trust it. Redundant checks add maintenance burden and drift risk without meaningful safety gain. Only add Go-side validation for constraints CEL cannot express — for example, batch-level cross-element checks on repeated fields, since CEL on `repeated` evaluates per-element. Concrete example: `internal/core/events/service.go::ValidateExternalEvents` deduplicates `event_id` across a batch.

**Proto directory layout mirrors the handler auth boundary:**

- **`proto/public/`** — no auth (e.g., auth service)
- **`proto/sdk/`** — API key auth (public or private). Write-only — never expose read endpoints or return sensitive data. Public keys are extractable from client apps, so SDK endpoints must assume an untrusted caller regardless of key type.
- **`proto/dashboard/`** — JWT only (e.g., orgs, projects, dashboards)
- **`proto/shared/`** — private API key or JWT (e.g., campaigns, delivery, profiles read/delete)
- **`proto/common/v1/`** — shared message types with no service definitions, accessible from any auth level. Put types here when (a) they are needed across auth boundaries, or (b) they are reused across multiple services within the same auth boundary and copying would create drift risk (e.g., `GetFilterSchemaRequest`/`Response` is consumed by both `shared.activity` and `shared.insights`). A message used by exactly one service belongs in that service's package, not `common/v1/`.

### Subsystem Reference

Deep per-subsystem documentation lives in [`docs/architecture/`](docs/architecture/). These are **not** loaded by default — read the relevant file when working in that area:

- **Insights** — trends/funnel/retention/segmentation queries; breakdowns, granularity caps, filter model, funnel timing stats, type-specific query builders → [`docs/architecture/insights.md`](docs/architecture/insights.md)
- **ClickHouse** — type-safe query builder, events table (dedup key, partitioning, `FINAL` policy), materialized-view flavors, query conventions → [`docs/architecture/clickhouse.md`](docs/architecture/clickhouse.md)
- **Profiles** — read API (ClickHouse-backed), activity summary, property model, soft-delete, device subscriptions → [`docs/architecture/profiles.md`](docs/architecture/profiles.md)
- **Event ingestion enrichment** — geo, user-agent, and bot-management auto-properties → [`docs/architecture/ingestion.md`](docs/architecture/ingestion.md)
- **Email templating** — templ + go-premailer rendering, frozen brand tokens, preview CLI → [`docs/architecture/email.md`](docs/architecture/email.md)
- **OpenTelemetry** — provider bootstrap, per-component instrumentation status, the error-recording convention and its exceptions → [`docs/architecture/telemetry.md`](docs/architecture/telemetry.md)

## Code Style

- Standard Go conventions. Use slog for logging. Run `goimports -w` (a strict superset of `go fmt`) on edited files. A PostToolUse hook auto-runs `goimports` on every `Edit`/`Write` tool use, so manual invocation is only needed when edits bypass the hook (batch refactors, IDE edits, merge resolutions).
- Always use context-aware slog variants (`slog.InfoContext`, `slog.ErrorContext`, `slog.WarnContext`, `slog.DebugContext`) instead of `slog.Info`, `slog.Error`, etc.
- Always use `slogx.Error(err)` (from `internal/slogx`) for logging errors. Never use `slog.Any("error", err)` or `slog.Any("err", err)`.
- Never pass sentinel errors directly to `connect.NewError`. Always create an explicit client-facing message with `errors.New("...")`. Sentinel errors are internal and their strings must not leak to API consumers.
- Pair `slog.ErrorContext` with `telemetry.RecordError(ctx, err)` at the layer that **detects** the error (executor / service / worker / query helper); downstream handlers and wrappers only translate to `connect.NewError(...)` — never re-log or re-record. Full exceptions (client-input errors, defer-cleanup, wrapper disposition logs, pure-passthrough services) → [`docs/architecture/telemetry.md`](docs/architecture/telemetry.md).
