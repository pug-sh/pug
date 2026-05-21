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
- **`proto/dashboard/`** — JWT only (e.g., orgs, projects, insights)
- **`proto/shared/`** — private API key or JWT (e.g., campaigns, delivery, profiles read/delete)
- **`proto/common/v1/`** — shared message types with no service definitions, accessible from any auth level. Put types here when (a) they are needed across auth boundaries, or (b) they are reused across multiple services within the same auth boundary and copying would create drift risk (e.g., `GetFilterSchemaRequest`/`Response` is consumed by both `shared.activity` and `shared.insights`). A message used by exactly one service belongs in that service's package, not `common/v1/`.

### Insights Breakdown

Breakdowns are supported for trends, funnel, and retention. Segmentation does not support breakdowns.

- `QueryRequest.breakdowns` is `repeated Breakdown` — list of property keys to break down by (e.g. `[{property: "$country"}, {property: "$browser"}]`).
- **Attribution:** first-touch — each user is assigned the breakdown value(s) from their earliest matching event (`argMin(property, occur_time)`). This keeps funnel and retention per-user logic correct by not splitting a user across multiple groups.
- **Top-N bucketing:** the query builds a `top_vals` CTE and groups values outside the top N into `'$others'` to keep result sets bounded. The event scope of `top_vals` matches the query's aggregation scope:
  - Trends: `top_vals` covers all events matching any query event kind in the time range.
  - Funnel (counts and timing): `top_vals` is filtered to step-matching events.
  - Retention: `top_vals` is filtered to start-event rows only.
- **Two-phase aggregation pattern:** funnel (counts, timing) and retention breakdown queries avoid evaluating `argMin` twice by splitting into:
  1. An aggregation CTE that computes `argMin(expr, occur_time) AS raw_bd_N` once.
  2. A downstream CTE or SELECT that buckets `raw_bd_N` against `top_vals` as a plain scalar expression.
- **Response shape:** funnel and retention responses wrap their results in series objects keyed by breakdown combination:
  - `FunnelResult.series` → `repeated FunnelSeries` with `breakdown map<string,string>` + `steps repeated FunnelStep`
  - `RetentionResult.series` → `repeated RetentionSeries` with `breakdown map<string,string>` + `cohorts repeated RetentionCohort`
  - When no breakdowns are requested, a single series with an empty `breakdown` map is returned.

### Funnel Timing Statistics

When `include_step_timing` is true, each `FunnelStep` includes a `StepTiming` sub-message with per-step conversion time statistics computed in Go from per-user event timestamps (no extra ClickHouse query needed). All timing scalars are `google.protobuf.Duration`:

- `StepTiming.avg` — mean
- `StepTiming.median` — average-of-two-middles median
- `StepTiming.p95` — nearest-rank ceiling p95
- `StepTiming.distribution` — `repeated DistributionBucket` histogram across 8 fixed buckets: `0-30s`, `30s-2m`, `2-5m`, `5-15m`, `15-60m`, `1-6h`, `6-24h`, `24h+`

`FunnelStep.timing` is **absent** (nil) for step 0 (the entry step has no conversion time) and when `include_step_timing` is false. Steps with zero converters still emit `Timing` with a zero-filled distribution slice (not absent) — this distinguishes "timing not applicable" (step 0, `nil`) from "nobody converted yet" (allocated zeros). `DistributionBucket.upper_bound` (`google.protobuf.Duration`) is absent on the open-ended last bucket; clients should use `label` for display in that case.

The request-side `QueryRequest.conversion_window` is also a `google.protobuf.Duration`. Validated by two rules at the interceptor: a field-level `gte: 1s` rejects sub-second values (e.g. `500ms`), and a CEL `whole_seconds` rule rejects fractional-second values (e.g. `2.5s`, `1s + 1ns`) — `windowFunnel` accepts only integer-second windows, so anything else would silently truncate. When absent, the conversion window defaults to the full time range.

**Implementation:** `internal/core/insights/funnel_buckets.go` holds the bucket table (`funnelBucket` struct: `upper time.Duration`, `label string`, `openEnded bool`) and three pure helpers for median, percentile, and bucketing a pre-sorted slice. The funnel-timing compute function collects raw per-user deltas (`time.Duration`), sorts once, and calls the helpers; the proto-translation layer wraps the `time.Duration` results in `*durationpb.Duration` at the package boundary. Tests pin both the structural bucket invariants (strictly ascending bounds, `time.Duration(math.MaxInt64)` sentinel for the open-ended last bucket, exactly one open-ended bucket) and the user-visible nil-vs-zero-filled distribution distinction.

### Insights Granularity

`QueryRequest.granularity` controls the time-bucket size for trends and retention queries. Supported values (ordered finest → coarsest):

| Enum value             | ClickHouse function  | Max time range |
| ---------------------- | -------------------- | -------------- |
| `GRANULARITY_MINUTE`   | `toStartOfMinute`    | 6 hours        |
| `GRANULARITY_HOUR`     | `toStartOfHour`      | 14 days        |
| `GRANULARITY_DAY`      | `toStartOfDay`       | 365 days       |
| `GRANULARITY_WEEK`     | `toStartOfWeek`      | 4 years        |
| `GRANULARITY_MONTH`    | `toStartOfMonth`     | 10 years       |

- Limits are enforced by five `buf.validate.message.cel` rules on `QueryRequest` in `proto/shared/insights/v1/insights.proto` (ids `query_request.granularity_{minute,hour,day,week,month}_max_range`). The `validate.NewInterceptor()` wired on the Connect handler rejects violating requests with `CodeInvalidArgument` before the handler runs.
- The minute/hour/day limits are sized to keep per-series data point counts at ≤365 (MINUTE=360, HOUR=336, DAY=365). WEEK is capped at 1461 days (~4 years, ~209 buckets); MONTH is capped at 3652 days (~10 years, ~120 buckets), bounding partition scan range since the events table partitions monthly.
- **Retention caveat**: retention queries multiply cohort buckets × follow-up buckets (filtered to a triangular shape via `r.t >= r.cohort_time`). At WEEK granularity over the 4-year cap that's roughly (209 × 210)/2 ≈ 21,945 rows per series before breakdowns — substantially larger than the trends-equivalent ≤365 bound. The cap protects against unbounded scan cost, not against large retention result sets.
- **MINUTE granularity caveat**: charts visualize at the same boundary as the ClickHouse dedup key (`toStartOfMinute(occur_time)`), so any pre-merge transient duplicates show at full magnitude per bucket. Coarser granularities amortize duplicates across multiple minutes per bucket. See "ClickHouse Events Table" for dedup details.
- The caps fire for any `QueryRequest` regardless of `insight_type`, so funnel/segmentation requests with an oversized `granularity`/`time_range` combo are also rejected even though those insight types ignore granularity at query-build time.
- `GRANULARITY_UNSPECIFIED` is rejected at the field level via `not_in: [0]` — clients must explicitly choose a granularity. `granularityFunc` returns an error for UNSPECIFIED and any undefined enum value (e.g. a future enum added to the proto but not yet wired into the switch); the error surfaces through the `Build*Query` error path. Direct callers (workers, scripts) bypassing the interceptor must set `Granularity` explicitly.

### Insights Filter Model

- Top-level insights filters are **group-based only**. In `shared.insights.v1`, use `filter_groups` and `filter_groups_operator` on `QueryRequest` and `SegmentUsersRequest`.
- Legacy top-level `filters` fields are removed from `proto/shared/insights/v1/insights.proto`. Tags are intentionally not reserved (pre-release, never shipped). Do not reintroduce them.
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
  - `RetentionResult.series` is `repeated RetentionSeries` — one entry per breakdown combination (single entry when no breakdown)
  - `RetentionSeries.breakdown` is a `map<string, string>` of property key → value for this series
  - `RetentionSeries.cohorts` contains `repeated RetentionCohort`, one per cohort bucket
  - `RetentionCohort.cohort` stores the cohort timestamp (RFC3339)
  - `RetentionCohort.cohort_size` stores the number of users in the cohort
  - `RetentionCohort.points[].value` is retention percentage (`0..100`) across time buckets

### Event Enrichment

Incoming events are enriched with auto-properties before being published to NATS:

- **`internal/geo/`** — resolves geo properties (`$country`, `$city`, `$ip`, etc.) from proxy-injected HTTP headers. `geo.Provider` is an interface; the Cloudflare implementation reads from `CF-Connecting-IP` and `CF-*` headers. Geo properties are **always overwritten** (CDN-injected values are trusted).
- **`internal/useragent/`** — parses the `User-Agent` header using `ua-parser/uap-go` into properties: `$browser`, `$browserVersion`, `$os`, `$osVersion`, `$device`. Both `browserVersion` and `osVersion` use the major version only (e.g. `"17"` not `"17.2.1"`) to avoid analytics fragmentation. UA properties are only written if not already present in `event.AutoProperties` (client-supplied values win). `$device` is only set when the parser identifies a specific device (e.g., "iPhone", "Pixel 8"); desktop browsers typically yield no `$device` property. The parser is initialized once at startup via `useragent.NewParser()` to avoid reloading regex definitions per request.
- **bot management enrichment** — reads Cloudflare Bot Management headers (injected via Transform Rule) and sets typed auto-properties: `$bot_score` from `CF-Bot-Score` is parsed as `Int64` (0–255, lower = more bot-like) and `$verified_bot` from `CF-Verified-Bot` is parsed as `Bool` (identifies known good bots like Googlebot). Both are **always overwritten** by the server; client-supplied values are stripped before enrichment. Routing through `internal/autoprop` keeps the typed-Variant story consistent across enrichers (geo `$latitude`/`$longitude` are also stored as `Float64`).

### Email Templating

Transactional emails are rendered with `templ` components in `internal/core/email/templates/` and inlined for email clients with `go-premailer` at send time (`internal/core/email/render.go`). Each email is one `.templ` (HTML) plus a hand-written plaintext twin in `internal/core/email/text.go` — templ is HTML-only, and auto-derived plaintext reads poorly, so the twin is deliberate. The `Renderer` is built inside `NewServiceWithResolver` from `Config`, so `coreemail.Service`'s `Send*` methods just call it; transport, per-tenant routing, and the worker are untouched.

Design tokens are **frozen hex** in `templates/brand.go`, converted once from cotton-w's OKLCH design system (`src/index.css`) because no email client parses `oklch()`. The stylesheet lives as a Go const in `templates/styles.go` and is injected into the layout `<head>` via `@templ.Raw(styleTag)` (templ treats a literal `<style>` as a raw-text element, so a templ expression placed *inside* one is not interpreted). `go-premailer` then inlines those classes onto elements — tests assert the button's `background-color:#3c68d9` is inlined, that no `oklch(` ever reaches the output, and that every `Color*` constant in `brand.go` matches a literal in `styles.go` (the constants document the palette; the inlined stylesheet literals are what render).

Run `make templ` after editing `.templ` files; generated `*_templ.go` is committed (like `internal/gen`). Preview locally with `pug email preview <magic_link|invite|provider_test> [--text] [--out file]`. The header logo URL comes from `PUG_EMAIL_LOGO_URL` (empty ⇒ wordmark-only header); `sanitizeDisplay` is still applied to subjects and plaintext (templ handles HTML escaping for the HTML body).

### Profile Properties

Profiles store properties as a single JSONB field (`properties`) rather than separate `auto_properties` and `custom_properties` fields. This simplifies the data model while preserving the ability to distinguish between auto-populated and custom properties at the application level through property naming conventions (e.g., `$` prefix for auto-properties).

### Profiles Read API

`shared.profiles.v1.ProfilesService` is backed by ClickHouse for reads and PostgreSQL for delete-side effects.

- `Get`, `GetByExternalId`, and `List` all return the same `Profile` shape from `proto/shared/profiles/v1/profiles.proto`.
- `Profile` includes the base profile record (`id`, `external_id`, `properties`, timestamps, `project_id`) plus an optional nested `activity` summary.
- `Delete` is different: it is a write path that soft-deletes in PostgreSQL, deactivates devices in the same transaction, then publishes a `ProfileUpsertMessage` so ClickHouse converges asynchronously.

**Read data sources.**

- Base profile fields come from ClickHouse `profiles` (`schema/clickhouse/migrations/003_create_profiles.sql`), queried through the `latest_profiles` CTE in `internal/core/profiles/service.go`. This is the current-state projection of the `ReplacingMergeTree(insert_time)` table.
- Alias resolution comes from ClickHouse `profile_aliases` (`schema/clickhouse/migrations/002_create_profile_aliases.sql`), queried through `latest_profile_aliases`.
- Activity fields come from `distinct_id_activity_states` (`schema/clickhouse/migrations/005_create_profile_activity_summary.sql`), an incremental `AggregatingMergeTree` rollup keyed by `(project_id, distinct_id)`.
- The per-profile activity summary is built by `profileActivitySummaryCTE`: it unions the canonical profile ID and all alias IDs for that profile, joins those identities to `distinct_id_activity_states`, then re-aggregates to one row per `(project_id, profile_id)`.

**`Profile.activity` semantics.**

- `activity` is omitted (`nil`) when the profile has no recorded events in ClickHouse (`total_events == 0`).
- `first_seen`, `last_seen`, `total_events`, and `sessions` come from aggregate states over all events for the resolved identity set.
- `pageviews` is derived as `sum(kind = 'page_view')` in the ClickHouse rollup.
- `browser`, `browser_version`, `os`, `os_version`, `device`, `country`, `region`, and `city` come from the latest event per distinct ID via `argMaxState(..., occur_time)`, then are merged across aliases at the profile level.
- There is currently no `channel` field in the profile API. Do not invent one ad hoc in handlers without first defining a stable derivation rule and proto field.

**List / pagination behavior.**

- `ProfilesService.List` is a server-streaming RPC. The server emits `ListResponse` pages until exhaustion.
- Page size is server-controlled (`const pageSize = 100` in `internal/app/server/rpc/shared/profiles/handler.go`); the client cannot request a custom size.
- Pagination is keyset-based on `(create_time DESC, id DESC)`. `next_page_token` is an opaque base64url cursor carrying the last row's `create_time` and `id`.

**Profiles filter logic.**

- `ListRequest` uses group-based filters only: `filter_groups` plus `filter_groups_operator`.
- Within a group, filters are combined with the group's `operator` (`AND` default when unspecified). Between groups, filters are combined with `filter_groups_operator` (`AND` default when unspecified).
- `PROPERTY_SOURCE_PROFILE` and `PROPERTY_SOURCE_UNSPECIFIED` filter against the JSON `properties` column on the ClickHouse `profiles` row via `internal/core/clickhouse.ProfilePropertyCondition`.
- `PROPERTY_SOURCE_AUTO` on profile list requests does **not** read directly from event property maps. It filters against already-materialized summary columns from `activity_summary` via `internal/app/server/rpc/shared/profiles/filters.go`.
- Supported auto filter keys for profile list are exactly: `$browser`, `$browserVersion`, `$os`, `$osVersion`, `$device`, `$country`, `$region`, `$city`.
- Unsupported auto keys should stay explicit errors. In particular, list filters do not currently support `$ip`, channel/referrer/UTM fields, or typed numeric auto-properties such as `$bot_score`.
- Numeric auto-property operators (`<`, `<=`, `>`, `>=`, `BETWEEN`, `NOT BETWEEN`) only work for auto fields that provide a numeric expression. The current profile-list auto summary fields are string-only, so numeric operators against them must fail rather than silently coerce.

### Profile Soft-Delete

Profiles use soft-delete via a `deletion_time timestamptz` column (NULL = active). All read queries filter `deletion_time IS NULL`. The `SoftDeleteProfileByIDAndProjectID` query sets `deletion_time = now()` — it never hard-deletes. The `deletion_time IS NULL` guard makes soft-delete idempotent (returns 0 rows if already deleted).

ClickHouse profiles use `is_deleted UInt8` for the same purpose. The identify worker and dashboard delete handler both publish `ProfileUpsertMessage` with `is_deleted=true` to sync soft-deletes to ClickHouse.

### Device Subscriptions

`profile_devices.profile_id` is nullable. Devices can be registered anonymously (no profile exists yet). When the SDK later calls Subscribe with a `profile_id` or `profile_external_id`, the upsert links the device via `coalesce(excluded.profile_id, profile_devices.profile_id)` — a re-subscribe with a profile never unlinks an already-linked device. The identify worker uses `LinkDeviceToProfile` which always overwrites `profile_id` to support account switching (old profile → new profile).

The FK uses `ON DELETE SET NULL` as a safety net — if a profile row were ever hard-deleted at the database level, devices would become unlinked rather than cascade-deleted. In normal operation, profiles are soft-deleted and devices are explicitly deactivated within the same transaction.

### ClickHouse Query Builder

Use `internal/core/clickhouse/query.go` for building ClickHouse queries. It provides a type-safe query builder with parameterized arguments:

```go
import "github.com/pug-sh/pug/internal/core/clickhouse"

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
- **Unknown PropertyValue variants drop the offending property and continue.** The proto-to-Variant translator maps each `*commonv1.PropertyValue` oneof case to its typed `chcol.Variant` slot; an unsupported variant drops the offending key from the row and the rest of the batch still inserts. Drops are observable via `events.property_dropped_total{stage, reason}` (worker-side `stage="ingestion"`, enrichment-side `stage="enrichment"`). The error path is unreachable through the validated RPC ingress (`oneof.required`) and only fires on proto-future drift or nil values. The SDK is not signalled per-property; it still sees `accepted=N` for the batch.

### ClickHouse Materialized Views

Pure CH→CH aggregation work (read from CH, aggregate, write to a CH rollup table, no external side effects) belongs in a materialized view, not a Go cron worker. CH-native scheduling, lifecycle, and refresh history (`system.view_refreshes`) come for free, and refreshable MVs read `FROM ... FINAL` once per refresh instead of forcing every dashboard query to pay that cost.

Pick the MV flavor based on whether aggregates are mergeable and whether the source needs dedup:

| Flavor                                              | When to use                                                                                              | Mechanics                                                                                                                                |
| --------------------------------------------------- | -------------------------------------------------------------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------- |
| Incremental (`TO target`, no `REFRESH`)             | Source is append-only and aggregates are partial-state-mergeable (`countState`, `maxState`, `sumState`)  | Runs as an insert trigger. Rollup uses `AggregatingMergeTree` with `*State`/`*Merge` aggregates. No staleness, no watermark, no `FINAL`. |
| Refreshable rebuild (`REFRESH EVERY ... TO target`) | Whole-table rebuild from a current-state source (e.g. `profiles`) where late-binding `FINAL` matters     | Replaces the entire target on every refresh. Bounded staleness equal to the refresh interval.                                            |
| Refreshable APPEND (`REFRESH EVERY ... APPEND TO`)  | Need `FROM events FINAL` for dedup and prefer incremental scan to full rebuild                           | Requires a closed-bucket watermark (below). Rollup engine should dedup on bucket key, e.g. `ReplacingMergeTree(last_seen)`.              |

Default to incremental when both conditions hold — refreshable MVs trade refresh-interval staleness and watermark complexity for `FINAL`, only worth it when you actually need it. Concrete examples in `schema/clickhouse/migrations/004_create_filter_schema_mvs.sql`: `event_names_mv` is incremental, `property_keys_event_buckets_mv` is refreshable APPEND, `property_keys_profile_current_mv` is refreshable rebuild.

**Closed-bucket watermark pattern (refreshable APPEND).** To keep refreshes idempotent and avoid double-counting, scope the source filter to closed buckets keyed off **event time**, not insert time:

```sql
WHERE toStartOfFiveMinutes(occur_time) BETWEEN
        toStartOfFiveMinutes(now() - INTERVAL 15 MINUTE)
    AND toStartOfFiveMinutes(now() - INTERVAL 5 MINUTE)
```

The trailing 5-minute lag ensures the most recent bucket is closed before it's read; the 10-minute window covers retries and slightly-late events. Late-arriving rows still bucket by their original `occur_time`, and the rollup's `ReplacingMergeTree(last_seen)` collapses retried bucket rows on background merge.

**Use a Go worker, not an MV, when** the work has external side effects (NATS publish, API calls, Postgres writes — e.g. the campaign scheduler at `internal/app/workers/scheduler/scheduler.go`), needs multi-step orchestration, or reads from non-CH sources.

**Version requirement.** Refreshable MVs went stable in ClickHouse 24.10. Cotton's dev infra pins `clickhouse/clickhouse-server:26.3` (`infra/dev/docker-compose.yaml`), so the requirement is satisfied; verify before relying on the feature in any new environment.

New MVs go in `schema/clickhouse/migrations/` as goose migrations; pair the rollup table DDL and the `CREATE MATERIALIZED VIEW` statement in the same migration file.

**Migration editing rule.** If a migration has **never** been applied in any environment whose state must be preserved (for example production, staging, or a shared dev DB), it is acceptable to edit that migration in place. Once a migration has been applied anywhere that matters, treat it as immutable and add a new forward migration instead. Do not create a follow-up migration solely to rewrite an unapplied migration.

### ClickHouse Query Builder Conventions

- Prefer `internal/core/clickhouse` query builder for ClickHouse query construction in core packages (`insights`, `events`, filters-related query helpers).
- Use parameterized limits (`LIMIT ?`) through `Query.Limit(...)` and pass `int64` values consistently.
- Use `RawCond(...)` only for expression-level fragments that are awkward to model otherwise (for example `occur_time >= now() - INTERVAL 30 DAY` or `IN ?` tuple bindings). Keep full query structure (`SELECT/FROM/WHERE/GROUP/ORDER/LIMIT`) in the builder.
- For property-values query helpers, query builder methods now return build errors; callers must propagate those errors instead of relying on raw-SQL fallbacks.

### Insights Query Builders

Always use the type-specific builders — they provide compile-time safety between builder and executor:

| Insight type         | Builder                  | Query type          |
| -------------------- | ------------------------ | ------------------- |
| Trends               | `BuildTrendsQuery`       | `TrendsQuery`       |
| Segmentation         | `BuildSegmentationQuery` | `ScalarQuery`       |
| Funnel (counts)      | `BuildFunnelCountsQuery` | `FunnelQuery`       |
| Funnel (with timing) | `BuildFunnelTimingQuery` | `FunnelTimingQuery` |
| Retention            | `BuildRetentionQuery`    | `RetentionQuery`    |

All query types expose `.SQL()` and `.Args()`. All types except `ScalarQuery` also expose `.Properties()` and `.NumBreakdowns()`. `FunnelTimingQuery` also exposes `.Kinds()` and `.WindowSec()`.

All five emit `SETTINGS use_query_cache = 1, query_cache_ttl = 60` via `WithQueryCache(analyticsCacheTTL)` on the outermost query. Cache isolation between projects relies on `project_id` being a positional parameter on every cached builder; a builder that interpolates `project_id` into raw SQL would silently break tenant isolation. Property keys/values (including profile property keys/values), segment-users, and event-names builders intentionally omit the cache. See `analyticsCacheTTL` in `internal/core/insights/builder.go` for staleness mechanics with ReplacingMergeTree.

### OpenTelemetry Instrumentation

All telemetry is bootstrapped in `internal/deps/telemetry/`. The server initializes OpenTelemetry via `telemetry.NewOtelInterceptor(ctx)` which:

- Sets up trace, metric, and log providers exporting OTLP over gRPC (insecure, default `localhost:4317`)
- Replaces the default `slog` logger with an OTel-bridged logger — all `slog.*Context` calls are automatically correlated with the active trace
- Returns an `otelconnect.Interceptor` that is wired into every Connect RPC handler

**Instrumentation status:**

| Component      | Status                                                                                                                   |
| -------------- | ------------------------------------------------------------------------------------------------------------------------ |
| Connect RPC    | ✅ — `otelconnect.Interceptor` on all handlers                                                                           |
| slog → OTel    | ✅ — `otelslog` bridge replaces default logger                                                                           |
| PostgreSQL     | ✅ — `otelpgx` tracer on all connections                                                                                 |
| Redis          | ✅ — `redisotel` tracing + metrics on the client                                                                         |
| NATS/JetStream | Custom — `tracedJetStream` wrapper in `internal/deps/nats/otel.go`, W3C trace context propagation on publish/consume     |
| ClickHouse     | Custom — `Conn` wrapper in `internal/deps/clickhouse/clickhouse.go`, spans on Query/Exec/Select/PrepareBatch/AsyncInsert |

**Configuration:** Set `OTEL_SERVICE_NAME` (strongly recommended — telemetry data will lack a service identifier without it) and `OTEL_EXPORTER_OTLP_ENDPOINT` (default `localhost:4317`). TLS is disabled by default (`OTEL_EXPORTER_OTLP_INSECURE` defaults to `true` when unset); set `OTEL_EXPORTER_OTLP_INSECURE=false` to enable TLS for production OTLP endpoints.

**Recording errors in spans:** Use `telemetry.RecordError(ctx, err)` to record an error on the current span, set the span status to `Error`, and attach stack traces.

Pair `slog.ErrorContext` with `telemetry.RecordError` **at the layer that detects the error** — typically the executor, service, worker, or query helper where the error first surfaces. Downstream layers (handlers, wrappers) must NOT re-log or re-record the same error: `slog.ErrorContext` would emit a duplicate log line, and `telemetry.RecordError` would attach a duplicate event to the same span. Handlers that propagate an already-recorded error should only translate it to the appropriate `connect.NewError(...)` and return.

This convention is enforced by code review, not by tests — no slog/span assertions exist in CI, so a regression that re-introduces duplicate logging or drops source-layer instrumentation will not fail the build.

Exceptions:

- **Client-input errors** (`CodeInvalidArgument`, `CodeUnauthenticated`, etc.) do not need `RecordError`. The default treatment is `slog.WarnContext` at the boundary that detects them, but log level and location vary by case:
  - **Auth extraction failures** (`MustGetPrincipal*`) — log at `slog.DebugContext` at the source (`internal/app/server/rpc/auth.go`). Auth-extraction is high-volume probe noise (every unauthenticated request hits it), so Debug keeps the noise floor low. The handler boundary skips the log entirely and only translates to `connect.NewError(connect.CodeUnauthenticated, ...)`.
  - **`Build*Query` validation errors with client-supplied free-form input** (`BuildTrendsQuery`, `BuildSegmentationQuery`, `BuildFunnelTimingQuery`, `BuildFunnelCountsQuery`, `BuildRetentionQuery`, `BuildSegmentUsersQuery`) — log at `slog.WarnContext` at the boundary. Other `Build*Query` callers in `internal/core/insights/service.go` (filter-schema and property-values builders) take only `projectID` plus a validated `eventKind`/`propertyKey`; their `Build()` failures are programmer-error / proto-enum drift, not client input, so they log + record at source like internal errors.
  - **Other client-input validators** vary based on whether the failure carries diagnostic value:
    - `events.ErrInvalidFilter` — log at `slog.WarnContext` at the boundary (carries which property/operator the client got wrong).
    - `coreevents.ValidateExternalEvents`, `events.DecodeEventCursor` — no log at all at the boundary; the handler just translates to `CodeInvalidArgument`. The rejection itself is the diagnostic (malformed page tokens, batch-dedup mismatches), and the request body is already in the access log.
- **Defer-rollback / cleanup failures** (e.g. `tx.Rollback`, `rows.Close`) should pair slog + RecordError at the deferred site since no caller can see them.
- **Wrapper disposition logs.** A wrapper that emits its own log for a wrapper-specific decision (e.g. the NATS worker's "terminating poison message" / "message processing failed" lines) MAY include the underlying processor error as a `slogx.Error(err)` attribute. That log line is a *different fact* (the disposition the wrapper decided on, plus wrapper-only metadata like stream/consumer) than the processor's source log, so it is not a duplicate. The wrapper must still skip `telemetry.RecordError` on the original error — the processor already recorded it.
- **Pure-passthrough services.** When a service method is a one-line wrapper around a generated `dbread`/`dbwrite` query (no business logic, no enrichment to add), the *handler* is effectively the lowest layer with meaningful context (project_id, customer_id, etc.) — logging the DB error at the handler is acceptable in that case. Services with non-trivial logic (e.g. transactions, orchestration of multiple writes, cross-cutting validation) must log + record at source like everyone else.

## Code Style

- Standard Go conventions. Use slog for logging. Run `goimports -w` (a strict superset of `go fmt`) on edited files. A PostToolUse hook auto-runs `goimports` on every `Edit`/`Write` tool use, so manual invocation is only needed when edits bypass the hook (batch refactors, IDE edits, merge resolutions).
- Always use context-aware slog variants (`slog.InfoContext`, `slog.ErrorContext`, `slog.WarnContext`, `slog.DebugContext`) instead of `slog.Info`, `slog.Error`, etc.
- Always use `slogx.Error(err)` (from `internal/slogx`) for logging errors. Never use `slog.Any("error", err)` or `slog.Any("err", err)`.
- Never pass sentinel errors directly to `connect.NewError`. Always create an explicit client-facing message with `errors.New("...")`. Sentinel errors are internal and their strings must not leak to API consumers.
