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

# OTLP collector + HyperDX (optional; for telemetry export)
make clickstack

# Run database migrations
./bin/pug postgres migrate
./bin/pug nats migrate
./bin/pug clickhouse migrate

# Seed the demo project for local dev. Resets Postgres + ClickHouse (pass
# --no-reset to keep the schema and truncate the demo tables instead), then runs
# the same event-gated flow as `pug worker demo`: ensure the demo account,
# backfill events, seed Postgres profiles for ONLY the users that produced
# events, then copy them to ClickHouse. A profile therefore never exists without
# events. (Tunables: --count, --batch.)
./bin/pug seed

# Start development server + workers together
./bin/pug dev

# Start server only
./bin/pug server

# Start individual workers
./bin/pug worker events
./bin/pug worker profile identify
./bin/pug worker profile alias
./bin/pug worker profile upsert

# Rolling demo-traffic generator. Gated by PUG_DEMO_ENABLED everywhere: when off
# (default), `pug dev` skips it and the standalone `pug worker demo` idles (stays
# running but generates nothing, so a k8s Deployment doesn't restart-loop on
# exit); when on, it runs. It derives the demo
# project from the demo user (woof@pug.sh) — creating the customer/org/project
# on a fresh DB, resolving it otherwise — then, if the project has no events
# yet, backfills ~4 months of "Pug & Pals" history and seeds a profile only for
# the users those events belong to (never for a user with no events). Most of
# the pool stays profile-less: the ~half whose join date is still in the future
# (they sign up live as the wall clock crosses their join) plus past users who
# churned before the backfill window. It then plays sessions out in real time.
# Both the backfill and the live stream write straight to ClickHouse via the
# same insert path — the worker owns its ClickHouse connection and uses no NATS,
# so it depends on no other worker (the rollup MV still fires on the direct
# inserts). New signups keep appearing as the wall clock crosses each user's
# join date and the worker creates their profile on first sight.
# Self-bootstrapping so a single k8s
# deployment seeds-then-streams with no manual seed step and no project id to
# configure.
./bin/pug worker demo
```

Environment variables are documented in `.env.example`. **Telemetry export is auto-detected** (decided once on first `SetupSDK` in server/workers): if any standard OTLP endpoint var is set (`OTEL_EXPORTER_OTLP_ENDPOINT`, or a per-signal `OTEL_EXPORTER_OTLP_{TRACES,METRICS,LOGS}_ENDPOINT`), pug exports via OTLP (`otelslog`; needs a collector, e.g. `make clickstack`); otherwise it falls back to application logs as text on stdout with noop trace/metric export (use for deploys without a collector). There is no `PUG_OTEL` switch, and a present-but-blank endpoint counts as unset. Set `OTEL_SERVICE_NAME` when exporting via OTLP.

### Code Generation

```bash
# Generate sqlc queries (after modifying SQL files)
make sqlc

# Generate protobuf code (after modifying .proto files)
# Runs three buf plugins: protoc-gen-go, protoc-gen-connect-go, and
# protoc-gen-mcp (an in-repo wrapper — cmd/protoc-gen-mcp — that adds editions
# support to redpanda-data/protoc-gen-go-mcp, which upstream
# lacks). The MCP plugin emits a `<pkg>mcp` subpackage for every proto package (buf
# has no per-plugin path filter, so every proto is fed to it); only the
# insights/activity/profiles packages are linked into the /mcp endpoint (see MCP
# subsystem), the rest go unused. An RPC's leading comment becomes its MCP tool
# description — for those three services a proto comment is shipped to the model, so
# treat it as runtime behavior. Delete the wrapper and point buf at upstream once it
# declares editions.
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

### Testing

Integration tests run against real Postgres/ClickHouse/NATS/Redis via testcontainers (`internal/testutil`). **Containers are shared per package, not per test** — one per test binary, started lazily on the first `Setup*` call. Isolation is per-test but comes from a namespace inside the shared container, never from a fresh container:

- **`SetupPostgres`** — migrations run **once** into a `pug_template` database; each test gets its own DB via `create database ... template pug_template` (milliseconds, vs seconds for a container start + migration run). `sealTemplate` bars connections to the template afterwards, because `CREATE DATABASE ... TEMPLATE` fails while any session is attached to the source.
- **`SetupClickHouse`** — ClickHouse has no `TEMPLATE`, so each test gets a fresh database that is migrated on creation.
- **`SetupNATS`** — JetStream has no per-test namespace, so each call drops every stream (which drops its consumers' ack state too) and recreates them from `schema/nats/*.yaml`.
- **`SetupRedis`** — each test gets one of the 16 logical databases, flushed on entry and exit.

Every package calling a `Setup*` helper **must** declare `func TestMain(m *testing.M) { testutil.Main(m) }` — the container has no owning test to hang a `t.Cleanup` on, so without it containers outlive the run; `TestSetupCallersWireMain` fails if a package forgets. That teardown only covers a normal finish, so `Main` also **force-enables testcontainers' Ryuk reaper**, overriding a `~/.testcontainers.properties` that disables it: a panicking test, an expired `-timeout` or a Ctrl-C kills the process without `m.Run` ever returning, and Ryuk is the only thing that reaps a package's containers then. Export `TESTCONTAINERS_RYUK_DISABLED` explicitly to opt back out (Ryuk cannot drive every Docker setup). A failed container start is deliberately **not** memoized (`lazyContainer.get`): starts time out when the Docker daemon is saturated, and caching that error would fail every remaining test in the package against a stale message.

Every helper hands out an **empty** namespace, so a test may still assume an empty database — that part of the contract did not change. What the shared container newly forbids is **concurrency**: a package calling a `Setup*` helper must not call `t.Parallel()`. Postgres and ClickHouse would survive it (each test holds a private database), but `SetupNATS` rebuilds streams container-wide and `SetupRedis` recycles its 16 indexes with a flush on entry, so a concurrent test gets its state deleted mid-run — and then either fails somewhere unrelated or passes having asserted over nothing. `TestSetupCallersDoNotUseParallel` fails the build on one. For the same reason a client or goroutine must not outlive the test that created it: the next `Setup*` reclaims the namespace. `go test -short ./...` skips every container test for fast unit-only feedback.

### Org Hierarchy

- **Org** is the top-level entity. Each customer belongs to one or more orgs via `org_members` (role: `ORG_ROLE_ADMIN` | `ORG_ROLE_MEMBER` | `ORG_ROLE_VIEWER`).
- **Projects** belong to an org (`org_id`). A project is always created within an org context.
- **Invitations** (`org_invitations`) are pending membership records that expire after 7 days and transition from `INVITATION_STATUS_PENDING` → `INVITATION_STATUS_ACCEPTED`. The redeemable invite secret is stored only as a hash in `email_action_tokens` (`purpose = emailaction.PurposeOrgInvite`, value `"org_invite"` — deliberately distinct from the auth login purpose `emailaction.PurposeMagicLink` / `"magic_link"` so that issuing or superseding a passwordless login link, which invalidates active tokens by `(email, purpose)`, can never consume a pending invite token); `org_invitations.token` is a non-redeemable storage value (rotated on resend via `RefreshOrgInvitationDelivery`) and is never returned by any RPC. Expiry is checked at accept time, not via a status transition. `ResendInvite` rotates the storage token, refreshes `expires_at`, invalidates any prior `email_action_tokens` for the invitation, and issues a fresh redeemable token — it does **not** change `status`; only acceptance flips PENDING → ACCEPTED. Acceptance for a customer who is **already** a member still flips `status` to ACCEPTED (and returns `ErrAlreadyMember`) rather than leaving a stranded PENDING row whose token has been consumed.
- Admin-only operations: `UpdateDisplayName`, `RemoveMember`, `UpdateMemberRole`, `InviteMember`, `ResendInvite`, `ListInvitations`. Other member-scoped org reads require membership; `List`/`Create`/`Leave` are self-service. `InviteMember` and `UpdateMemberRole` may assign any of the three roles. `UpdateMemberRole` permits **any** transition (promotion or demotion) **except demoting the org's last admin** to a non-admin role (`ErrLastAdmin` → `FailedPrecondition`/`CANNOT_DEMOTE_LAST_ADMIN`), enforced race-free by `UpdateOrgMemberRoleIfNotLastAdmin`'s locked-CTE admin-count guard (the same `FOR UPDATE` pattern as `DeleteOrgMemberIfNotLastAdmin`); a blocked guard and a missing member both surface as "no row updated" and are disambiguated by a follow-up read.
- New accounts are created by completing a magic link or OAuth sign-in — there is no password-signup endpoint (`SignUpWithEmail` was removed). Completing a *plain* (non-invite) magic link for a new email provisions a default org + project atomically (in `CompleteMagicLink`); an *invite* magic link instead joins the inviting org with the invitation's role. `CompleteMagicLink` looks the token up by hash (unique) and dispatches on its `purpose`: it honors only `"magic_link"` and `"org_invite"`, rejecting any other purpose with `ErrInvalidToken` so a token minted for a future flow can't be redeemed as a login. `CompleteOAuthSignIn` accepts the Google `id_token` credential from `@react-oauth/google` `GoogleLogin`, verifies it server-side via OIDC (`PUG_OAUTH_GOOGLE_CLIENT_ID`), resolves the IdP identity against `customer_identities` (link by verified email or create), then runs the same shared provisioning path as magic link (`FinishSignup` + `FinalizeVerifiedCustomer` in `provision.go`) in a single transaction (with fresh-transaction retries on signup races) via `oauth.WithIdentityTx`. A verified IdP email links to an existing email/password or magic-link account without clearing `password_hash`. Password login (`SignInWithEmail`) and authenticated `SetPassword` remain, so a magic-link account can opt into a password and then sign in with it.

### Dashboards

Dashboards belong to a **project** (`dashboard.dashboards.v1.DashboardsService`, JWT + `x-project-id`). Tiles are stored in PostgreSQL (`dashboard_tiles`).

- **Tile kinds:** `insight` (persisted `insight_query` JSONB holding a `shared.insights.v1.InsightQuerySpec` + `view_mode` column) or `markdown` (`markdown_body`). Insight `view_mode` includes line/area/bar/table/KPI/**Sankey** (`DASHBOARD_TILE_VIEW_MODE_SANKEY`) — a client rendering hint only; the server stores and echoes it without enforcing insight↔view_mode pairing. The time window and granularity are **dashboard-level**, not per-tile — a tile only stores *what* it measures.
- **Tile customization columns:** `compare` (text proto enum name for `ComparePeriod`, FE renders the comparison via a second `Query` with a shifted window), `thresholds` (jsonb array — typed as `[]byte` via a per-column sqlc override since the global jsonb-as-map mapping can't hold a top-level array, serialized via `protojson`), `header`, `visualization`, and `position` (jsonb objects round-tripped via `MessageToMap` / `MapToMessage`; `position` holds the tile's `GridPosition` — integer `x/y/w/h` placement on the uniform dashboard grid). All are evaluated client-side; the server only stores and returns them.
- **Dashboard-level window:** `dashboards.default_time_range` (a `common.v1.TimeRangePreset` enum name) and `default_granularity` (a `shared.insights.v1.Granularity` enum name) are dedicated columns; one time picker drives the whole board. `view_mode` stays per-tile (each chart's visualization). `Create` / `Update` full-replace the dashboard window + display fields — except `Update` preserves the existing `description` when the request sends `""` (`coalesce(nullif(@description, ''), description)`). `Upsert` full-replaces every field including `description`, so a client migrating between paths must not send an empty `description` unless it really intends to clear the field.
- **`Upsert` is the only RPC that directly mutates tiles.** (Deleting the parent dashboard cascades to its tiles via the FK.) There are no per-tile RPCs — `CreateTile`/`UpdateTile`/`DeleteTile` were deliberately removed so every tile write reconciles against the full draft. The handler runs in a single transaction with a `SELECT FOR UPDATE` row lock on the dashboard for true last-write-wins semantics under concurrent edits. Reconciliation has four branches: tiles with empty `id` are inserted; tiles with a matching `id` are updated; existing tiles whose `id` is omitted from the request are deleted; a populated `id` that does NOT match any existing tile is rejected with `ErrDashboardTileNotFound` → `CodeNotFound` (no insert-as-new fallback). Duplicate non-empty ids in one request are rejected (`ErrDuplicateUpsertTileID` → `CodeInvalidArgument`). DELETE runs **before** INSERT/UPDATE so a swap-replace (drop tile A and insert a new tile reusing A's display_name) doesn't trip the partial unique index. UPDATE statements are gated in SQL on `payload_hash <> $new_hash`, so byte-identical writes don't fire the `moddatetime` trigger and `update_time` stays meaningful. The dashboard metadata UPDATE is gated symmetrically on `tiles_changed OR metadata changed`. The response returns tiles in **request order** so clients can map server-assigned ids back to a local draft.
- **`payload_hash`:** sha256 over `proto.MarshalOptions{Deterministic: true}` of a `DashboardTileInput` built from the **normalized** tile fields, with `id` left zero. Normalization is split: `Encode` normalizes `view_mode` and `compare` (via their `normalized*` helpers) before passing the values into `computeTilePayloadHash`, which collapses zero-valued `Header`/`Visualization`/`Position` to nil (storage stores them as `{}` either way) and hashes each nested message **whole** rather than field-by-field — `protojson`'s presence round-trip keeps the Get→Upsert echo stable, and a new sub-field on any of those messages is covered automatically (no per-field hash wiring). Together these keep the hash equal to a re-Upsert of what Get returns. Written on every insert/update. A contract test in `tile_payload_test.go` asserts every populated `DashboardTileInput` field changes the hash, catching silent exclusion if a new proto field is added without a hash decision.
- **`InsightQuerySpec` vs `QueryRequest`:** `QueryRequest = InsightQuerySpec spec + time_range + granularity`. A tile stores only the `InsightQuerySpec` (insight_type, events, breakdowns, filters, conversion_window, include_step_timing); the per-granularity range caps live on `QueryRequest` and are enforced once the window is attached at render time.
- **`QueryDashboard`:** returns one self-contained `RenderedDashboard` via `coredashboards.RenderDashboard` → `coreinsights.ExecuteQuery` (same execution path as `shared.insights.v1.InsightsService.Query`). Each `RenderedTile` embeds the full `DashboardTile` plus, for insight tiles, a `oneof outcome { result | error_message }`; markdown tiles carry no outcome. All tiles are returned in dashboard order (markdown included). The effective `(time_range, granularity)` is resolved **once** — request override → dashboard default — and applied to every tile; the assembled per-tile `QueryRequest` is re-validated (`protovalidate.Validate`) so the granularity range caps apply per tile, surfacing a violation as that tile's `error_message` without failing the whole RPC.
- **Rollup fast path:** `ExecuteQuery` serves unfiltered, single-/no-dimension, count/visitor (`TOTAL`/`UNIQUE_USERS`/`PER_USER_AVG`), day+ trends and segmentation tiles **over day-aligned windows** from a pre-aggregated `dashboard_event_rollup_daily` ClickHouse rollup (incremental MV, migration 006; extended by 009 to 20 dims total — 10 web-analytics dims added to 006's 10 — the session rollup gained 5 entry/exit dims via 010, plus the `AVG_EVENTS_PER_SESSION` metric served from migration 007's existing session `event_count_state`) instead of scanning raw events; **top K** tiles ride the same rollup via their own predicate (`canUseTopKRollup`: materialized-dim property or event-kind dimension, count/visitor/per-user-avg metric, no filters, kind-only scope, day-aligned window — granularity not consulted since top K has no time bucketing); everything else (filtered, multi-dim, sub-day, numeric-agg, funnel/retention/**user flow**, **USER-dimension top K**, **non-day-aligned windows**) falls back transparently to the raw builders. The window guard (`rollupWindowAligned`) requires `from` at midnight UTC and `to` at midnight UTC or ≥ `now`: the rollup is day-keyed, so a mid-day boundary would widen the partial boundary day and over-count vs the raw instant filter (eligibility threads the request's `now` so a live preset's `to == now` stays eligible). Accepted tradeoff: the rollup can't dedup duplicate event deliveries (key omits `event_id`; raw `ReplacingMergeTree` dedups on merge), so rollup-served `TOTAL`/`PER_USER_AVG` over-count vs raw by the redelivery rate (`UNIQUE_USERS` immune) — a bounded, documented inaccuracy pinned by `TestIntegration/rollup_duplicate_overcount_documented`. `BuildTrendsQuery`/`BuildSegmentationQuery`/`BuildTopKQuery` stay pure raw builders; the predicates `canUseEventRollup`/`canUseTopKRollup` + `rollupWindowAligned` + rollup builders + execution dispatchers live in `internal/core/insights/rollup.go`. → [`docs/architecture/insights.md`](docs/architecture/insights.md), [`docs/architecture/clickhouse.md`](docs/architecture/clickhouse.md)
- **Preset normalization:** the dashboard window round-trips through `dashboardDefaultTimeRangeDBName` / `dashboardGranularityDBName` (write) and `DashboardDefaultTimeRangePresetFromDB` / `DashboardGranularityFromDB` (read), normalizing unknown/`UNSPECIFIED` to `LAST_30_DAYS` / `GRANULARITY_DAY`.
- **Handler wiring:** `dashboardsrpc.NewServer(dashboardsSvc, insightsExecutor)` — the dashboards handler needs the dashboards service and insights executor.
- Deeper query/filter conventions → [`docs/architecture/insights.md`](docs/architecture/insights.md)

### Auth & Principal

RPC handlers authenticate via `connectrpc.com/authn` middleware. Four auth modes are supported:

- **`WithJWTAuth`** — Dashboard auth. Sets `Principal.Customer` (always non-nil). Optionally sets `Principal.Project` if `x-project-id` header is provided and the customer is an org member.
- **`WithSDKAuth`** — API key auth (public or private key). Sets `Principal.Project` only. `Principal.Customer` is nil.
- **`WithDualAuth`** — Private API key or JWT fallback. API key path sets `Principal.Project` only; JWT path behaves like `WithJWTAuth`.
- **`WithPrivateKeyAuth`** — Private (`prv_`) API key in `x-api-key` only: no public-key branch, no `api_key` query-param fallback, no JWT fallback. The auth boundary for `/mcp` (MCP clients hold a static credential, so an expiring access JWT is useless there and a public key is extractable from client apps). Sets `Principal.Project` only; `Principal.Customer` is nil. Not passed around as an `authn.AuthFunc` — `mcp.Mount` constructs it internally from the projects repo, so `/mcp` cannot be wired to any other auth mode.

`resolvePrivateKeyPrincipal` is the shared private-key → `Principal` resolution behind `WithDualAuth` and `WithPrivateKeyAuth`. **`RecoverHandlerPanic`** (wired once via `connect.WithRecover` in `handlerOpts`) converts a panic escaping any handler into `CodeInternal` with a log + `telemetry.RecordError`, never leaking the panic value; it is required, not cosmetic, because the `/mcp` loopback invokes handlers off net/http's panic-recovering goroutine (see MCP subsystem).

**API keys (`api_keys` table, migration 017):** a project has **many** keys, each a row keyed by `token` — the lookup value auth resolves against. A **private** (`prv_`) key is stored **only as its sha256 hex digest**; `coreprojects.hashKey` (mirroring `coreauth.hashToken`, and migration 017's `encode(sha256(convert_to(key,'UTF8')),'hex')`) hashes the presented key inside `coreprojects.Repo` before the cache/DB lookup, so `auth.go`, `resolvePrivateKeyPrincipal` and `mcp.Mount` are unchanged and never see a digest. A **public** (`pub_`) key ships inside client apps and is extractable by design, so it is stored whole and echoed back by `ListApiKeys`. `masked` ("prv_...3f9c") is all a private key ever shows again. Creating a project mints **one public key** as a second statement in the same transaction (`CreateProjectInTx` → `CreateApiKeyInTx`), so a project can never commit without a key to send events with; private keys are created **explicitly** via `CreateApiKey`, whose response is the only time the raw value exists outside the caller. Keys are **hard-deleted** (`DeleteApiKey`, project-scoped in SQL) — the `api_keys` row is the only place a key exists, so deleting it is the whole revocation; the Redis project cache (`project:prvkey:<digest>` / `project:pubkey:<key>`) is invalidated per token on a **detached** context (the write has already committed, so an admin closing the tab must not leave a revoked key cached) and its TTL is **1h** (was 30d — it now bounds how long a revoked key outlives a lost invalidation). `Project` carries **no** key fields — proto 5/6 are `reserved`; `ListApiKeys` is the only way to read a project's keys. Authorization: `authz.ResourceAPIKey` — every role reads (`ListApiKeys`), **admin-only** create/delete; there is no update action.

**Session tokens (access + refresh):** every sign-in path (`SignInWithEmail`, `CompleteMagicLink`, `CompleteOAuthSignIn`) returns a `coreauth.Session{AccessToken, RefreshToken}`, not a bare JWT. The access JWT is short-lived (`accessTokenTTL`, 24h — sized by refresh cost, not leak risk; see the constant's comment) — `WithJWTAuth` still verifies it exactly as before. The refresh token is a long-lived (`refreshTokenTTL`, sliding 90 days) opaque crypto-random secret stored **only as a sha256 hash** in the `refresh_tokens` table (migration 014), mirroring `email_action_tokens`. `RefreshSession` (public RPC — it runs *after* the access token has expired, so it can't sit behind JWT auth) exchanges a refresh token for a new pair inside a `FOR UPDATE`-locked tx, **rotating** it: the presented token is consumed and a successor is minted in the same `family_id`. Reuse-detection: presenting an already-consumed token (replay of a leaked token, or a client double-refreshing) revokes the **entire family** and returns `ErrInvalidToken` → both attacker and legitimate client must re-auth. `SignOut` revokes the token's family (best-effort; a stale token is a no-op). For magic-link/OAuth the refresh row commits **inside** the provisioning tx (`issueSessionTx`) so a new account and its first session are atomic; password sign-in uses a standalone insert (`issueSession`). The FE (`../app`) treats refresh-token presence as the authentication signal (`isAuthenticatedAtom`) — NOT access-token expiry — and its transport interceptor silently refreshes on expiry or a `401`, so active users are never logged out at the access-token boundary. That refresh is single-flight **across tabs** (`navigator.locks` + a re-read of localStorage inside the lock), not just within one — tabs share one localStorage refresh token, so without it two tabs waking together would both rotate it and trip reuse-detection, revoking the family and hard-logging the user out everywhere.

**Demo login:** `DemoSignIn` (public, no credentials) mints a full `Session` for the seeded read-only viewer account (`coreauth.DemoViewerEmail` = `snoop@pug.sh`, the demo seeder's single source of truth for that email) plus the demo `project_id` the client scopes to via `x-project-id`, so a visitor landing on the public demo page is authenticated in viewer mode with zero FE auth changes (the returned refresh token rides the same silent-refresh path). Gated by `PUG_DEMO_ENABLED`, now read by `pug server` too — the one flag is the whole demo switch (server demo-login + worker traffic), so set it `true` on every pod of a demo deployment and leave it `false` everywhere else; `DemoSignIn` returns `CodeUnavailable` when off or when the demo account isn't seeded. There is **no** demo-specific authorization path — the minted principal is a genuine `ORG_ROLE_VIEWER` org member, so the existing Casbin RBAC makes it read-only end to end; as defense-in-depth on this credential-less endpoint, `DemoSignIn` additionally refuses to mint unless the resolved account really is a viewer, so a mis-seed or a later promotion of the demo account fails closed.

`Principal.Customer` is `*dbread.Customer` — it is nil for API key auth paths. Always use the appropriate extractor:

- **`MustGetPrincipalWithCustomer`** — use in dashboard handlers that access `principal.Customer`. Returns `CodeUnauthenticated` if Customer is nil.
- **`MustGetPrincipalWithProject`** — use in handlers that require a project context (`x-project-id` header). Returns `CodeUnauthenticated` if Project is nil.

Never call `getPrincipalFromContext` directly in handlers.

### Authorization (RBAC via Casbin)

Authentication (above) establishes *who* the caller is; **authorization** (*may this role do this?*) is a Casbin policy in `internal/core/authz/` — all Go, no `.conf`/`.csv`/`go:embed`.

- **Casbin holds only the role→permission matrix + role hierarchy** (`model` const + `policy.go` `[][]string`). Role **assignment stays in Postgres** (`org_members.role`), resolved fresh per request via `coreorgs.GetMemberRole`, then that single role is passed into `Authorizer.Authorize(role, resource, action)`. There is no Casbin↔DB sync and no policy cache to invalidate. The `*authz.Authorizer` is built once in `newDeps` (`authz.NewAuthorizer`; a malformed static policy fails startup via a returned error — no panic, no global) and injected through `server.start` into the single `rpc.AuthzInterceptor` (the dashboard handlers themselves hold no authorizer).
- **Resources/actions are typed consts** (`authz.Resource` / `authz.Action`). `manage` is authoring sugar that expands to CRUD at load time; the specials `send`/`export`/`erase` are never implied by `manage`. Active roles form a hierarchy (`groupingRules`): `ORG_ROLE_ADMIN` (org administration + API-key create/delete) inherits `ORG_ROLE_MEMBER` (full CRUD on project-scoped resources) inherits `ORG_ROLE_VIEWER` (read-only floor: project-scoped reads + the read-only org view — org/member/project/api_key). The viewer floor is enforced uniformly by the one interceptor across both the org control plane (orgs / projects-lifecycle / email) and the project-data plane (dashboards/insights/activity/profiles) — so a viewer is genuinely read-only on the JWT path: denied dashboard writes and profile erasure, allowed every read. The **API-key path stays coarse** project-scoped (no customer ⇒ no role), so a private key keeps full data access. member and admin keep the **exact** effective permissions they had before viewer existed (viewer only factors the shared reads into a named floor — zero behavior change for those two roles). Every role-gated RPC resolves+parses the stored role and fails **closed** on an unrecognized value, where the prior `IsOrgMember` check passed on row existence alone. `owner`/feature roles remain reserved in comments for the roadmap.
- **Enforcement is one registry-driven interceptor** (`rpc.AuthzInterceptor`, wired into `handlerOpts`), driven by `internal/app/server/rpc/authz_registry.go`, which maps every served RPC to an `authzspec.Spec` (a reflection-based contract test fails if any served RPC lacks an entry or an entry goes stale). Each Spec is built by a constructor in the `authzspec` package — `Public`/`Self`/`Project`/`SDKKey`, or the role-gated `OrgGated`/`ProjGated`; because `Spec`'s fields are unexported, the "role-gated ⟺ resource+action+orgSource" invariant holds **by construction** (a malformed role-gated entry won't compile, not merely fail a test; a bare `Spec{}` is caught by `Defined()`). For each role-gated entry the interceptor resolves the caller's org — `authzspec.OrgFromMessage` (org control plane: orgs / projects `BatchGet`+`Create` / email — read via the generated `GetOrgId()` accessor) or `OrgFromProject` (project data plane + projects lifecycle writes `Delete`/`UpdateMeta`/`UpdateFCMServiceJSON` — `principal.Project.OrgID`) — then enforces the recorded `(resource, action)` against the policy. The registry's pairs are the **enforced** source of truth everywhere: a new role-gated RPC is gated the moment its entry is added (it can't ship unguarded), and the interceptor tests (`TestAuthzInterceptorRegistryEntriesEnforced`, `TestRoleGatedAdminOnlyRPCs`) catch a drifted pair. **Handlers carry no authorization of their own** — they assume the request reaching them is already authorized; the `require*` helpers and the per-handler `*authz.Authorizer` are gone.
  - Denials are uniform: a non-member → `PermissionDenied(ORG_NOT_A_MEMBER)`, an insufficient role → `PermissionDenied(ORG_ROLE_FORBIDDEN)`. A non-member is reported identically whether the org exists or not (the role lookup finds no row either way), so existence is never leaked. On the API-key path (no customer) the interceptor is a deliberate **no-op** — API-key access stays coarse project-scoped, exactly as before Casbin.
  - Two RPCs sit outside the role gate by design: `projects.Get` is `domainProject` (no role check — auth already established membership and the only read it allows is in every role's floor), and `projects.Create`'s admin check is **additionally** enforced race-safe in the `CreateProjectAsAdmin` CTE (the interceptor is the coarse gate; the CTE is the authoritative, atomic one).
  - Casbin governs the **JWT/customer path only**; API-key (SDK/shared) access stays coarse project-scoped as before.
- **Role cache:** `coreorgs.GetMemberRole` is Redis-cached when the orgs service is built via `coreorgs.NewServiceWithRoleCache` (wired in `server.start`). It is **positive-only** (non-members are never cached, so member *adds* — incl. the cross-package auth provisioning path — need no invalidation) and invalidated on every role change / removal (`UpdateMemberRole`, `RemoveMemberSafe`, `Leave`); a short TTL backstops a lost invalidation. Best-effort, mirroring `projects.InvalidateProjectKeys`.

### Proto/RPC

Services defined in `proto/` directory, organized by auth boundary (`public/`, `sdk/`, `dashboard/`, `shared/`). Generated code goes to `internal/gen/proto/`. Uses Connect RPC with gRPC reflection enabled. Profiles is split into `ProfilesSDKService` (sdk — Identify) and `ProfilesService` (shared — Get, GetByExternalId, List, Delete, DeleteDataSubject, GetDeletionRequest). SDK profiles uses Go import alias `sdkprofilesv1` to avoid collision with shared `profilesv1`. `Delete`/`DeleteDataSubject` enqueue GDPR/DPDP erasure handled by the compliance worker — see Subsystem Reference.

**Validation:** Always use `buf/validate` (protovalidate) annotations in `.proto` files for request validation. The `validate.NewInterceptor()` in the server enforces all proto annotations before handlers run. Use CEL expressions for cross-field constraints (e.g., `this.from < this.to`, operator-dependent required fields, ordered values in repeated fields, map-key prefix checks via `map.all(k, k.startsWith('$'))`). Do **not** duplicate proto validations in Go code — if protovalidate already enforces a constraint, trust it. Redundant checks add maintenance burden and drift risk without meaningful safety gain. Only add Go-side validation for constraints CEL cannot express — for example, batch-level cross-element checks on repeated fields, since CEL on `repeated` evaluates per-element. Concrete example: `internal/core/events/service.go::ValidateExternalEvents` deduplicates `event_id` across a batch.

**Proto directory layout mirrors the handler auth boundary:**

- **`proto/public/`** — no auth (e.g., auth service)
- **`proto/sdk/`** — API key auth (public or private). Write-only — never expose read endpoints or return sensitive data. Public keys are extractable from client apps, so SDK endpoints must assume an untrusted caller regardless of key type.
- **`proto/dashboard/`** — JWT only (e.g., orgs, projects, dashboards)
- **`proto/shared/`** — private API key or JWT (e.g., campaigns, delivery, profiles read/delete)
- **`proto/common/v1/`** — shared message types with no service definitions, accessible from any auth level. Put types here when (a) they are needed across auth boundaries, or (b) they are reused across multiple services within the same auth boundary and copying would create drift risk (e.g., `GetFilterSchemaRequest`/`Response` is consumed by both `shared.activity` and `shared.insights`). A message used by exactly one service belongs in that service's package, not `common/v1/`.

### Subsystem Reference

Deep per-subsystem documentation lives in [`docs/architecture/`](docs/architecture/). These are **not** loaded by default — read the relevant file when working in that area:

- **Insights** — trends/funnel/retention/segmentation/**user flow (Sankey)**/**top K (ranked dimension + optional $others bucket)** queries; breakdowns, granularity caps, filter model, funnel timing stats, type-specific query builders; **`include_cookieless` spec toggle** (default false: user-counting metrics UNIQUE_USERS/PER_USER_AVG and person-based insights funnel/retention/user-flow/USER-top-K exclude `cookieless-` ids; TOTAL, numeric aggregations (SUM/AVG/MIN/MAX) and all session metrics always count all traffic; migration 011's rollup `cookieless` key column keeps both toggle states on the fast path) → [`docs/architecture/insights.md`](docs/architecture/insights.md), user flow plan → [`docs/architecture/user-flow.md`](docs/architecture/user-flow.md)
- **ClickHouse** — type-safe query builder, events table (dedup key, partitioning, `FINAL` policy), materialized-view flavors, query conventions → [`docs/architecture/clickhouse.md`](docs/architecture/clickhouse.md)
- **Profiles** — read API (ClickHouse-backed) serving identified profiles ∪ **derived anonymous persons** (unclaimed `distinct_id`s synthesized from the activity rollup — no rows written; claim exclusion via aliases/profile-ids/external-ids; `Get` redirects claimed anon ids to the canonical profile), activity summary, property model, soft-delete, device subscriptions → [`docs/architecture/profiles.md`](docs/architecture/profiles.md)
- **Compliance (GDPR/DPDP)** — data-subject erasure: a synchronous prelude + the generalized `compliance` worker that hard-deletes events, derived rollups, and the profile; the unified `compliance_requests` DSAR ledger; idempotent re-drive on retry, NATS retry-to-DLQ for the async hard delete
- **Event ingestion enrichment** — geo, user-agent, bot-management, and web-attribution auto-properties (`internal/attribution`: pathname/hostname/referrer-domain/channel/UTM/locale/screen-size derivation); **cookieless visitor identity** (`internal/cookieless`: server-derived daily-rotating HMAC ids prefixed `cookieless-` for consent-rejecting GDPR/DPDP visitors — Redis daily salt with TTL-deletion as the privacy guarantee, Redis-stitched sessions, resolved first in the ingest chain; the prefix is the downstream single source of truth) → [`docs/architecture/ingestion.md`](docs/architecture/ingestion.md)
- **Web analytics** — promoted web columns, event/session rollup dimensions, channel taxonomy, live-rollup-extension migrations 008–010 → [`docs/architecture/web-analytics.md`](docs/architecture/web-analytics.md)
- **Email templating** — templ + go-premailer rendering, frozen brand tokens, preview CLI → [`docs/architecture/email.md`](docs/architecture/email.md)
- **OpenTelemetry** — `internal/deps/telemetry/` (`SetupSDK`; OTLP-vs-stdout auto-detected from the `OTEL_EXPORTER_OTLP_*` endpoint vars, no `PUG_OTEL`), per-component instrumentation, slog bridge vs stdout handler, error-recording convention and exceptions → [`docs/architecture/telemetry.md`](docs/architecture/telemetry.md)
- **MCP server** — the read-only shared analytics API (insights + activity + profile reads = 12 tools) exposed as Model Context Protocol tools at `/mcp`. A thin adapter in `internal/app/server/mcp/`: every tool call re-enters the real Connect stack in-process via a loopback client, so validation/auth/authz run identically to an external API call (no duplicated logic).
  - **`mcp.Mount(mux, loopback, repo)` is the single entry point** — it builds the tool handler, builds its own private-key-only auth boundary (`WithPrivateKeyAuth`) from the repo, registers both `/mcp` and `/mcp/`, and fails startup on codegen drift. It does **not** accept an `authn.AuthFunc`: admitting a dashboard JWT or a public key is unrepresentable by construction, and `server.start` and the test harness go through the identical call so the endpoint can't be wired one way in tests and another in production.
  - **`toolPolicy` (`rename.go`) is the single source of truth for the tool surface** — one row per generated tool, either `expose("curated_name")` or `hide("why")`. Naming and exclusion are the same cell, so they cannot disagree. Keyed off the generated `Tool` vars, so an upstream rename/removal breaks it at **compile** time; a tool with no row, a stale row, a duplicate curated name, and a hidden tool that reaches registration all fail **startup** (`registerRenamed` → `checkComplete`; the `*renamer` never escapes, so the check can't be skipped). Hidden today: the GDPR erasure RPCs (irreversible — never LLM-callable) and the WIP insights `SegmentUsers` (still served as a Connect RPC). `List` (server-streaming) is never generated.
  - **An RPC's leading proto comment becomes its MCP tool description**, shipped verbatim to the model — proto comments on these three services are runtime behavior, not documentation. The server's `instructions` string is pinned against `toolPolicy` by a test, since prose has no compile-time link to the table.
  - **Panics must be contained** (`connect.WithRecover` + a backstop `recover` in `loopbackClient.Do`) and **tool calls are deadline-bounded** (`toolCallTimeout`): the go-sdk runs tool handlers on a jsonrpc2 goroutine with no panic recovery and with cancellation detached, so an uncontained panic would kill the process for every tenant and an abandoned call would leave its ClickHouse query running. `Stateless: true` is load-bearing — it is what binds each tool call's API key to its own request context.

## Code Style

- Standard Go conventions. Use slog for logging. Run `goimports -w` (a strict superset of `go fmt`) on edited files. A PostToolUse hook auto-runs `goimports` on every `Edit`/`Write` tool use, so manual invocation is only needed when edits bypass the hook (batch refactors, IDE edits, merge resolutions).
- Always use context-aware slog variants (`slog.InfoContext`, `slog.ErrorContext`, `slog.WarnContext`, `slog.DebugContext`) instead of `slog.Info`, `slog.Error`, etc.
- Always use `slogx.Error(err)` (from `internal/slogx`) for logging errors. Never use `slog.Any("error", err)` or `slog.Any("err", err)`.
- Never pass sentinel errors directly to `connect.NewError`. Always create an explicit client-facing message with `errors.New("...")`. Sentinel errors are internal and their strings must not leak to API consumers.
- Pair `slog.ErrorContext` with `telemetry.RecordError(ctx, err)` at the layer that **detects** the error (executor / service / worker / query helper); downstream handlers and wrappers only translate to `connect.NewError(...)` — never re-log or re-record. Full exceptions (client-input errors, defer-cleanup, wrapper disposition logs, pure-passthrough services) → [`docs/architecture/telemetry.md`](docs/architecture/telemetry.md).
