# Replace Subscriptions with Profile Devices — Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Replace the `subscriptions` table and all associated code with a new `profile_devices` model where devices always belong to a profile.

**Architecture:** Drop subscriptions (profile-optional, single metadata blob) and introduce profile_devices (profile-required, auto/custom properties split matching the profiles pattern). The proto service renames from SubscriptionsService to DevicesService. NATS stream/consumer renames from subscriptions to devices.

**Tech Stack:** Go, PostgreSQL (goose migrations, sqlc), Protobuf (buf/Connect RPC), NATS JetStream

---

## Naming Conventions

| Old | New |
|-----|-----|
| `subscriptions` (table) | `profile_devices` (table) |
| `subscription` (variable) | `device` (variable) |
| `Subscription` (Go type) | `ProfileDevice` (Go type) |
| `SubscriptionsService` (proto) | `DevicesService` (proto) |
| `subscriptions.ops` (NATS subject) | `devices.ops` (NATS subject) |
| `subscription-processor-durable` (NATS consumer) | `device-processor-durable` (NATS consumer) |
| `SubscriptionOpsSubject` (Go const) | `DeviceOpsSubject` (Go const) |
| `internal/core/subscriptions/` | `internal/core/devices/` |
| `internal/app/workers/subscriptions/` | `internal/app/workers/devices/` |
| `internal/app/server/rpc/sdk/subscriptions/` | `internal/app/server/rpc/sdk/devices/` |
| `proto/subscriptions/v1/` | `proto/devices/v1/` |
| `subscription_id` (proto field) | `device_id` (proto field) |
| `SubscriptionToken` (proto message) | `DeviceToken` (proto message) |

## Column Mapping: subscriptions → profile_devices

| Old Column | New Column | Notes |
|------------|-----------|-------|
| `id text primary key` | `id text not null` (part of composite PK) | PK changes to `(project_id, id)` |
| `project_id char(20)` | `project_id char(20)` (part of composite PK) | Now part of PK |
| `profile_id char(20)` (nullable, SET NULL) | `profile_id char(20)` (NOT NULL, CASCADE) | Required, cascades on profile delete |
| `token text` | `token text` | Unchanged |
| `platform text` | `platform text` | Unchanged |
| `status text` | `status text` | Unchanged |
| `metadata jsonb` | `auto_properties jsonb` + `custom_properties jsonb` | Split into two, mirrors profiles pattern |
| `create_time timestamptz` | `create_time timestamptz` | Unchanged |
| `update_time timestamptz` | `update_time timestamptz` | Unchanged |
| `last_heartbeat_time timestamptz` | **DROPPED** | No longer tracked |
| `updater text` | **DROPPED** | No longer tracked |

## Proto RPC Mapping: SubscriptionsService → DevicesService

| Old RPC | New RPC | Notes |
|---------|---------|-------|
| `RegisterSubscription` | **DROPPED** | Was an alias for Upsert; redundant |
| `SetProfileExternalID` | **DROPPED** | Profile linking is now implicit in Upsert (profile_external_id required) |
| `UpdateHeartbeat` | **DROPPED** | No heartbeat column |
| `UpdateMetadata` | **DROPPED** | Replaced by auto/custom_properties in Upsert |
| `UpdateStatus` | `UpdateStatus` | Kept |
| `UpdateToken` | `UpdateToken` | Kept |
| `Upsert` | `Upsert` | Now requires `profile_external_id`; uses `auto_properties`/`custom_properties` instead of `metadata` |

## Worker Operation Mapping

| Old Operation | New Operation | Behavior Change |
|---------------|--------------|-----------------|
| `UPSERT` | `UPSERT` | Now also resolves/creates profile via `SaveProfile` before `SaveDevice` |
| `PROFILE_LINK` | **DROPPED** | Profile linking happens automatically in UPSERT |
| `UPDATE_HEARTBEAT` | **DROPPED** | No heartbeat column |
| `UPDATE_METADATA` | **DROPPED** | Properties set via UPSERT |
| `UPDATE_STATUS` | `UPDATE_STATUS` | Unchanged logic |
| `UPDATE_TOKEN` | `UPDATE_TOKEN` | Unchanged logic |

## Affected Files (20 files total)

### Files to DELETE (8)
| File | Why |
|------|-----|
| `schema/postgres/queries/read/subscriptions.sql` | Replaced by `profile_devices.sql` |
| `schema/postgres/queries/write/subscriptions.sql` | Replaced by `profile_devices.sql` |
| `proto/subscriptions/v1/subscriptions.proto` | Replaced by `devices.proto` |
| `internal/gen/proto/subscriptions/` (entire dir) | Regenerated as `devices/` |
| `internal/core/subscriptions/service.go` | Replaced by `devices/service.go` |
| `internal/app/server/rpc/sdk/subscriptions/handler.go` | Replaced by `devices/handler.go` |
| `internal/app/workers/subscriptions/processor.go` | Replaced by `devices/processor.go` |
| `internal/app/workers/subscriptions/worker.go` | Replaced by `devices/worker.go` |

### Files to CREATE (7)
| File | What |
|------|------|
| `schema/postgres/migrations/008_replace_subscriptions_with_profile_devices.sql` | Migration |
| `schema/postgres/queries/read/profile_devices.sql` | Read queries |
| `schema/postgres/queries/write/profile_devices.sql` | Write queries |
| `proto/devices/v1/devices.proto` | Proto definitions |
| `internal/core/devices/service.go` | Business logic |
| `internal/app/server/rpc/sdk/devices/handler.go` | RPC handler |
| `internal/app/workers/devices/processor.go` | NATS message processor |
| `internal/app/workers/devices/worker.go` | Worker entrypoint |

### Files to MODIFY (7)
| File | What Changes |
|------|-------------|
| `proto/delivery/v1/delivery.proto` | `subscription_id` → `device_id`, `SubscriptionToken` → `DeviceToken` |
| `schema/nats/streams.yaml` | `subscriptions` stream → `devices` |
| `schema/nats/consumers.yaml` | `subscription-processor` → `device-processor` |
| `internal/deps/nats/subjects.go` | `SubscriptionOpsSubject` → `DeviceOpsSubject` |
| `internal/core/delivery/service.go` | Interface uses `ProfileDevice` |
| `internal/core/delivery/router.go` | Uses `ProfileDevice` |
| `internal/core/delivery/fcm.go` | Uses `ProfileDevice` |
| `internal/app/server/rpc/shared/delivery/handler.go` | `SubscriptionId` → `DeviceId` |
| `internal/app/workers/campaigns/processor.go` | Uses devices service |
| `internal/app/server/server.go` | Registers devices handler |
| `cmd/cotton/main.go` | CLI command + imports |
| `cmd/workers/subscription/main.go` | Import path |
| `Makefile` | Build target name |

---

## Milestone 1: Data Layer (Tasks 1-2)
> After this milestone: migration exists, sqlc queries exist, `make sqlc` succeeds.

---

### Task 1: Write the PostgreSQL migration

**Files:**
- Create: `schema/postgres/migrations/008_replace_subscriptions_with_profile_devices.sql`

**Step 1: Create the migration file**

```sql
-- +goose Up
drop table subscriptions;

create table profile_devices (
  auto_properties jsonb default '{}'::jsonb,
  create_time timestamptz not null default now(),
  custom_properties jsonb default '{}'::jsonb,
  id text not null,
  platform text not null check (platform in ('android', 'ios', 'web')),
  profile_id char(20) not null references profiles(id) on delete cascade,
  project_id char(20) not null references projects(id) on delete cascade,
  status text not null default 'active' check (status in ('active', 'inactive')),
  token text not null,
  update_time timestamptz not null default now(),
  primary key (project_id, id)
);

create trigger update_timestamp before
update on profile_devices for each row execute procedure moddatetime(update_time);

create index idx_profile_devices_profile_id on profile_devices (profile_id);
create index idx_profile_devices_project_status_platform on profile_devices (project_id, status, platform);
create index idx_profile_devices_auto_properties on profile_devices using gin (auto_properties);
create index idx_profile_devices_custom_properties on profile_devices using gin (custom_properties);

-- +goose Down
drop table profile_devices;

create table subscriptions (
  create_time timestamptz not null default now(),
  id text primary key,
  last_heartbeat_time timestamptz not null default now(),
  metadata jsonb,
  platform text not null check (platform in ('android', 'ios', 'web')),
  profile_id char(20) references profiles(id) on delete set null,
  project_id char(20) not null references projects(id) on delete cascade,
  status text not null default 'active' check (status in ('active', 'inactive')),
  token text not null,
  update_time timestamptz not null default now(),
  updater text not null default 'system' check (updater in ('system', 'user'))
);

create trigger update_timestamp before
update on subscriptions for each row execute procedure moddatetime(update_time);

create index idx_subscriptions_profile_id on subscriptions (profile_id);
create index idx_subscriptions_project_status_platform on subscriptions (project_id, status, platform);
```

**Verification:**
- [ ] File exists at `schema/postgres/migrations/008_replace_subscriptions_with_profile_devices.sql`
- [ ] `-- +goose Up` and `-- +goose Down` markers present
- [ ] Down migration recreates original subscriptions table exactly

---

### Task 2: Delete old sqlc subscription query files

**Files:**
- Delete: `schema/postgres/queries/read/subscriptions.sql`
- Delete: `schema/postgres/queries/write/subscriptions.sql`

**Step 1: Delete both files**

```bash
rm schema/postgres/queries/read/subscriptions.sql
rm schema/postgres/queries/write/subscriptions.sql
```

**Verification:**
- [ ] No `subscriptions.sql` in `schema/postgres/queries/read/`
- [ ] No `subscriptions.sql` in `schema/postgres/queries/write/`

---

### Task 3: Write profile_devices read queries

**Files:**
- Create: `schema/postgres/queries/read/profile_devices.sql`

**Step 1: Write the read queries**

```sql
-- name: GetProfileDevice :one
select * from profile_devices
where id = @id and project_id = @project_id;

-- name: GetProfileDevicesByProfileID :many
select * from profile_devices
where profile_id = @profile_id;

-- name: GetProfileDevicesByProject :many
select * from profile_devices
where project_id = @project_id;

-- name: GetActiveProfileDevicesByProject :many
select * from profile_devices
where project_id = @project_id and status = 'active';
```

**Query reference:**

| Query Name | Returns | Used By |
|-----------|---------|---------|
| `GetProfileDevice` | One `ProfileDevice` | Devices service — single device lookup |
| `GetProfileDevicesByProfileID` | Many `ProfileDevice` | Devices service — all devices for a profile |
| `GetProfileDevicesByProject` | Many `ProfileDevice` | Admin/dashboard — list all devices in project |
| `GetActiveProfileDevicesByProject` | Many `ProfileDevice` | Campaign worker — find delivery targets |

**Verification:**
- [ ] All query names use PascalCase with uppercase `ID`
- [ ] SQL uses lowercase identifiers
- [ ] File exists at `schema/postgres/queries/read/profile_devices.sql`

---

### Task 4: Write profile_devices write queries

**Files:**
- Create: `schema/postgres/queries/write/profile_devices.sql`

**Step 1: Write the write queries**

```sql
-- name: SaveProfileDevice :one
insert into profile_devices (auto_properties, custom_properties, id, platform, profile_id, project_id, status, token)
values (coalesce(@auto_properties, '{}'), coalesce(@custom_properties, '{}'), @id, @platform, @profile_id, @project_id, @status, @token)
on conflict (project_id, id) do update set
  auto_properties = jsonb_shallow_merge(profile_devices.auto_properties, excluded.auto_properties),
  custom_properties = jsonb_shallow_merge(profile_devices.custom_properties, excluded.custom_properties),
  platform = excluded.platform,
  status = excluded.status,
  token = excluded.token
returning *;

-- name: UpdateProfileDeviceStatus :one
update profile_devices
set status = @status
where id = @id and project_id = @project_id
returning *;

-- name: UpdateProfileDeviceToken :one
update profile_devices
set token = @token
where id = @id and project_id = @project_id
returning *;

-- name: DeleteProfileDevice :exec
delete from profile_devices
where id = @id and project_id = @project_id;
```

**SaveProfileDevice upsert behavior:**

| Case | What Happens |
|------|-------------|
| New device (no conflict) | INSERT with all fields. `auto_properties`/`custom_properties` default to `'{}'` if NULL passed |
| Existing device (conflict on `project_id, id`) | UPDATE: shallow-merge both property JSONBs, overwrite `platform`, `status`, `token` |
| Empty properties passed (`'{}'`) | `jsonb_shallow_merge` with `'{}'` is a no-op — existing properties preserved |
| New property keys passed | `jsonb_shallow_merge` adds new keys, keeps existing keys not in the update |
| Existing property key overwritten | New value wins (shallow merge replaces top-level keys) |

**Verification:**
- [ ] `SaveProfileDevice` uses `on conflict (project_id, id)` matching the composite PK
- [ ] `SaveProfileDevice` uses `jsonb_shallow_merge` (same pattern as `SaveProfile` in `write/profiles.sql`)
- [ ] `coalesce(@field, '{}')` prevents NULL jsonb inserts
- [ ] File exists at `schema/postgres/queries/write/profile_devices.sql`

---

### Task 5: Run sqlc code generation

**Step 1: Run sqlc**

```bash
make sqlc
```

This runs `rm -rf internal/gen/repo && go tool sqlc generate`.

**Expected result:**
- `internal/gen/repo/dbread/subscriptions.sql.go` — **gone**
- `internal/gen/repo/dbwrite/subscriptions.sql.go` — **gone**
- `internal/gen/repo/dbread/profile_devices.sql.go` — **new**
- `internal/gen/repo/dbwrite/profile_devices.sql.go` — **new**
- `internal/gen/repo/dbread/models.go` — `Subscription` struct gone, `ProfileDevice` struct present
- `internal/gen/repo/dbwrite/models.go` — same

**Expected generated `ProfileDevice` struct:**

```go
type ProfileDevice struct {
    AutoProperties   map[string]any
    CreateTime       pgtype.Timestamptz
    CustomProperties map[string]any
    ID               string
    Platform         string
    ProfileID        string        // NOT NULL so plain string, not pgtype.Text
    ProjectID        string
    Status           string
    Token            string
    UpdateTime       pgtype.Timestamptz
}
```

**Verification:**
- [ ] `make sqlc` exits with code 0
- [ ] `internal/gen/repo/dbread/models.go` contains `ProfileDevice` struct, no `Subscription` struct
- [ ] `internal/gen/repo/dbwrite/models.go` contains `ProfileDevice` struct, no `Subscription` struct
- [ ] `ProfileDevice.ProfileID` is `string` (not `pgtype.Text`) because column is NOT NULL

**Troubleshooting:**

| Problem | Fix |
|---------|-----|
| `sqlc generate` fails with "table subscriptions not found" | Old query files still reference subscriptions — verify Task 2 deleted them |
| `sqlc generate` fails with "function jsonb_shallow_merge not found" | The function is defined in migration 007 — sqlc reads all migrations as schema |
| `ProfileID` generates as `pgtype.Text` | Column definition must be `char(20) not null` — check migration has NOT NULL |

---

### Milestone 1 Checklist

- [ ] Migration file `008_replace_subscriptions_with_profile_devices.sql` exists with Up and Down
- [ ] No `subscriptions.sql` files in `schema/postgres/queries/`
- [ ] `profile_devices.sql` files exist in both `read/` and `write/`
- [ ] `make sqlc` succeeds
- [ ] Generated models contain `ProfileDevice`, not `Subscription`
- [ ] Commit: `feat: replace subscriptions table with profile_devices`

---

## Milestone 2: Proto & NATS (Tasks 6-9)
> After this milestone: proto generates, NATS config updated, `make rpc` succeeds.

---

### Task 6: Delete old subscriptions proto

**Files:**
- Delete: `proto/subscriptions/v1/subscriptions.proto`
- Delete: `internal/gen/proto/subscriptions/` (entire directory)

**Step 1: Delete files**

```bash
rm proto/subscriptions/v1/subscriptions.proto
rmdir proto/subscriptions/v1
rmdir proto/subscriptions
rm -rf internal/gen/proto/subscriptions
```

**Verification:**
- [ ] No `proto/subscriptions/` directory
- [ ] No `internal/gen/proto/subscriptions/` directory

---

### Task 7: Write new devices proto

**Files:**
- Create: `proto/devices/v1/devices.proto`

**Step 1: Create directory and proto file**

```protobuf
edition = "2023";
package devices.v1;

import "google/protobuf/any.proto";

option features.field_presence = IMPLICIT;
option go_package = "github.com/fivebitsio/cotton/internal/gen/proto/devices/v1;devicesv1";

service DevicesService {
  rpc Upsert(UpsertRequest) returns (UpsertResponse) {}
  rpc UpdateStatus(UpdateStatusRequest) returns (UpdateStatusResponse) {}
  rpc UpdateToken(UpdateTokenRequest) returns (UpdateTokenResponse) {}
}

message UpsertRequest {
  map<string, google.protobuf.Any> auto_properties = 1;
  map<string, google.protobuf.Any> custom_properties = 2;
  string id = 3;
  string platform = 4;
  string profile_external_id = 5;
  string token = 6;
}

message UpsertResponse {}

message UpdateStatusRequest {
  string id = 1;
  string status = 2;
}

message UpdateStatusResponse {}

message UpdateTokenRequest {
  string id = 1;
  string token = 2;
}

message UpdateTokenResponse {}

enum DeviceOperationType {
  DEVICE_OPERATION_TYPE_UNSPECIFIED = 0;
  DEVICE_OPERATION_TYPE_UPSERT = 1;
  DEVICE_OPERATION_TYPE_UPDATE_STATUS = 2;
  DEVICE_OPERATION_TYPE_UPDATE_TOKEN = 3;
}

message DeviceOperationMessage {
  map<string, google.protobuf.Any> auto_properties = 1;
  map<string, google.protobuf.Any> custom_properties = 2;
  string device_id = 3;
  DeviceOperationType operation_type = 4;
  string platform = 5;
  string profile_external_id = 6;
  string project_id = 7;
  string status = 8;
  string token = 9;
}
```

**RPC → Worker operation mapping:**

| RPC Call | Fields Sent to NATS | Worker Operation |
|----------|-------------------|------------------|
| `Upsert(id, platform, token, profile_external_id, auto_properties, custom_properties)` | All fields + `project_id` from auth | `DEVICE_OPERATION_TYPE_UPSERT` |
| `UpdateStatus(id, status)` | `device_id`, `status`, `project_id` | `DEVICE_OPERATION_TYPE_UPDATE_STATUS` |
| `UpdateToken(id, token)` | `device_id`, `token`, `project_id` | `DEVICE_OPERATION_TYPE_UPDATE_TOKEN` |

**Verification:**
- [ ] File at `proto/devices/v1/devices.proto`
- [ ] Package is `devices.v1`
- [ ] `go_package` ends with `;devicesv1`
- [ ] Uses `edition = "2023"` and `features.field_presence = IMPLICIT` (matching other protos)
- [ ] `DeviceOperationMessage` has `project_id` (injected by RPC handler from auth context)

---

### Task 8: Update delivery.proto

**Files:**
- Modify: `proto/delivery/v1/delivery.proto`

**Step 1: Rename subscription references**

Exact changes in `proto/delivery/v1/delivery.proto`:

| Line | Old | New |
|------|-----|-----|
| 37 | `repeated SubscriptionToken subscription_tokens = 5;` | `repeated DeviceToken device_tokens = 5;` |
| 58 | `string subscription_id = 8;` | `string device_id = 8;` |
| 68 (RecordEventRequest) | `string subscription_id = 7;` | `string device_id = 7;` |
| 78 | `message SubscriptionToken {` | `message DeviceToken {` |
| 79 | `string subscription_id = 1;` | `string device_id = 1;` |

**Verification:**
- [ ] No occurrences of `subscription` (case-insensitive) remain in `delivery.proto`
- [ ] Field numbers unchanged (backward-compatible wire format not required since this is a breaking change)

---

### Task 9: Generate proto code and update NATS config

**Files:**
- Modify: `schema/nats/streams.yaml`
- Modify: `schema/nats/consumers.yaml`
- Modify: `internal/deps/nats/subjects.go`

**Step 1: Update streams.yaml**

Replace the subscriptions stream:

| Old | New |
|-----|-----|
| `name: "subscriptions"` | `name: "devices"` |
| `subjects: ["subscriptions.>"]` | `subjects: ["devices.>"]` |
| `description: "Events related to subscription management"` | `description: "Events related to device management"` |

All other fields (retention, max_bytes, etc.) stay the same.

**Step 2: Update consumers.yaml**

Replace the subscription consumer:

| Old | New |
|-----|-----|
| `name: "subscription-processor"` | `name: "device-processor"` |
| `stream_name: "subscriptions"` | `stream_name: "devices"` |
| `durable_name: "subscription-processor-durable"` | `durable_name: "device-processor-durable"` |
| Comment: `# Consumer for subscription events` | Comment: `# Consumer for device events` |

**Step 3: Update subjects.go**

In `internal/deps/nats/subjects.go`:

| Old | New |
|-----|-----|
| `SubscriptionOpsSubject = "subscriptions.ops"` | `DeviceOpsSubject = "devices.ops"` |
| Comment: `// Subscription subjects` | Comment: `// Device subjects` |

**Step 4: Run proto generation**

```bash
make rpc
```

`make rpc` runs `buf lint && buf generate`. This will:
- Lint the new `devices.proto` and updated `delivery.proto`
- Generate Go code into `internal/gen/proto/devices/v1/`
- Regenerate `internal/gen/proto/delivery/v1/` with renamed fields

**Expected generated files:**
- `internal/gen/proto/devices/v1/devices.pb.go` — message types
- `internal/gen/proto/devices/v1/devicesv1connect/devices.connect.go` — Connect RPC stubs

**Troubleshooting:**

| Problem | Fix |
|---------|-----|
| `buf lint` fails with PACKAGE_DIRECTORY_MATCH | Proto file must be at `proto/devices/v1/devices.proto` to match package `devices.v1` |
| `buf lint` fails with ENUM_VALUE_PREFIX | Enum values must start with `DEVICE_OPERATION_TYPE_` matching the enum name |
| `buf generate` fails | Old `internal/gen/proto/subscriptions/` must be deleted first (Task 6) |
| `make rpc` succeeds but old generated code remains | `buf.gen.yaml` has `clean: true` so old generated dirs should be removed — but manually delete `internal/gen/proto/subscriptions/` if not |

**Verification:**
- [ ] `make lint` passes (buf lint)
- [ ] `make rpc` passes (buf generate)
- [ ] `internal/gen/proto/devices/v1/devices.pb.go` exists
- [ ] `internal/gen/proto/devices/v1/devicesv1connect/devices.connect.go` exists
- [ ] `internal/gen/proto/subscriptions/` does NOT exist
- [ ] No `subscription` references in `internal/gen/proto/delivery/v1/delivery.pb.go`
- [ ] `internal/deps/nats/subjects.go` has `DeviceOpsSubject`, not `SubscriptionOpsSubject`
- [ ] NATS YAML files reference `devices`, not `subscriptions`

---

### Milestone 2 Checklist

- [ ] `proto/subscriptions/` directory gone
- [ ] `proto/devices/v1/devices.proto` exists
- [ ] `delivery.proto` has no subscription references
- [ ] `make lint` passes
- [ ] `make rpc` passes
- [ ] NATS streams/consumers YAML updated
- [ ] `subjects.go` updated
- [ ] Commit: `feat: replace subscriptions proto with devices proto and update NATS config`

---

## Milestone 3: Go Service & Delivery Layer (Tasks 10-12)
> After this milestone: core business logic compiles, delivery layer updated.

---

### Task 10: Delete old subscriptions service

**Files:**
- Delete: `internal/core/subscriptions/service.go`

**Step 1: Delete file and directory**

```bash
rm internal/core/subscriptions/service.go
rmdir internal/core/subscriptions
```

---

### Task 11: Write new devices service

**Files:**
- Create: `internal/core/devices/service.go`

**Step 1: Write the service**

```go
package devices

import (
	"context"

	"github.com/fivebitsio/cotton/internal/gen/repo/dbread"
	"github.com/fivebitsio/cotton/internal/gen/repo/dbwrite"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	StatusActive   = "active"
	StatusInactive = "inactive"
)

type Service struct {
	read  *dbread.Queries
	write *dbwrite.Queries
}

func NewService(pgRO *pgxpool.Pool, pgW *pgxpool.Pool) *Service {
	return &Service{
		read:  dbread.New(pgRO),
		write: dbwrite.New(pgW),
	}
}

func (s *Service) SaveDevice(ctx context.Context, params dbwrite.SaveProfileDeviceParams) (dbwrite.ProfileDevice, error) {
	return s.write.SaveProfileDevice(ctx, params)
}

func (s *Service) UpdateDeviceStatus(ctx context.Context, id, projectID, status string) (dbwrite.ProfileDevice, error) {
	return s.write.UpdateProfileDeviceStatus(ctx, dbwrite.UpdateProfileDeviceStatusParams{
		Status:    status,
		ID:        id,
		ProjectID: projectID,
	})
}

func (s *Service) UpdateDeviceToken(ctx context.Context, id, projectID, token string) (dbwrite.ProfileDevice, error) {
	return s.write.UpdateProfileDeviceToken(ctx, dbwrite.UpdateProfileDeviceTokenParams{
		Token:     token,
		ID:        id,
		ProjectID: projectID,
	})
}

func (s *Service) GetActiveDevicesByProject(ctx context.Context, projectID string) ([]dbread.ProfileDevice, error) {
	return s.read.GetActiveProfileDevicesByProject(ctx, projectID)
}

func (s *Service) GetDevicesByProfileID(ctx context.Context, profileID string) ([]dbread.ProfileDevice, error) {
	return s.read.GetProfileDevicesByProfileID(ctx, profileID)
}
```

**Service method reference:**

| Method | Used By | Delegates To |
|--------|---------|-------------|
| `SaveDevice` | Devices worker (upsert handler) | `dbwrite.SaveProfileDevice` (INSERT ON CONFLICT UPDATE) |
| `UpdateDeviceStatus` | Devices worker (status handler) | `dbwrite.UpdateProfileDeviceStatus` |
| `UpdateDeviceToken` | Devices worker (token handler) | `dbwrite.UpdateProfileDeviceToken` |
| `GetActiveDevicesByProject` | Campaign worker | `dbread.GetActiveProfileDevicesByProject` |
| `GetDevicesByProfileID` | Future use (profile detail views) | `dbread.GetProfileDevicesByProfileID` |

**Verification:**
- [ ] Package is `devices` (not `subscriptions`)
- [ ] Imports `dbread` and `dbwrite` (not direct SQL)
- [ ] `SaveDevice` accepts full `SaveProfileDeviceParams` struct (delegates validation to SQL)
- [ ] No `Heartbeat`, `Metadata`, `Updater`, or `LinkToProfile` methods

---

### Task 12: Update delivery layer to use ProfileDevice

**Files:**
- Modify: `internal/core/delivery/service.go` (line 11)
- Modify: `internal/core/delivery/router.go` (line 34)
- Modify: `internal/core/delivery/fcm.go` (line 81, 125, 136)

**Step 1: Update `service.go` interface**

Change:
```go
SendNotification(ctx context.Context, campaign dbread.Campaign, subscription dbread.Subscription) error
```
To:
```go
SendNotification(ctx context.Context, campaign dbread.Campaign, device dbread.ProfileDevice) error
```

**Step 2: Update `router.go`**

In `SendNotification` (line 34):

| Old | New |
|-----|-----|
| `func (r *Router) SendNotification(ctx context.Context, campaign dbread.Campaign, subscription dbread.Subscription) error {` | `func (r *Router) SendNotification(ctx context.Context, campaign dbread.Campaign, device dbread.ProfileDevice) error {` |
| `switch subscription.Platform {` | `switch device.Platform {` |
| `return r.fcmService.SendNotification(ctx, campaign, subscription)` | `return r.fcmService.SendNotification(ctx, campaign, device)` |
| All `slog.String("subscription_id", subscription.ID)` | `slog.String("device_id", device.ID)` |
| All `slog.String("platform", subscription.Platform)` | `slog.String("platform", device.Platform)` |

**Step 3: Update `fcm.go`**

In `SendNotification` (line 81):

| Old | New |
|-----|-----|
| `func (f *FCMService) SendNotification(ctx context.Context, campaign dbread.Campaign, subscription dbread.Subscription) error {` | `func (f *FCMService) SendNotification(ctx context.Context, campaign dbread.Campaign, device dbread.ProfileDevice) error {` |
| `Token: subscription.Token,` (line 125) | `Token: device.Token,` |
| `slog.String("subscription_id", subscription.ID),` (line 136) | `slog.String("device_id", device.ID),` |

**Verification:**
- [ ] `Service` interface has `ProfileDevice` parameter
- [ ] `Router.SendNotification` has `ProfileDevice` parameter
- [ ] `FCMService.SendNotification` has `ProfileDevice` parameter
- [ ] No remaining references to `Subscription` type in delivery package
- [ ] All log keys say `device_id` not `subscription_id`

---

### Milestone 3 Checklist

- [ ] `internal/core/subscriptions/` directory gone
- [ ] `internal/core/devices/service.go` exists
- [ ] All three delivery files use `ProfileDevice`
- [ ] No `Subscription` type references in `internal/core/`
- [ ] Commit: `feat: add devices service and update delivery layer`

---

## Milestone 4: RPC Handler & Worker (Tasks 13-16)
> After this milestone: full request flow works end-to-end (RPC → NATS → Worker → DB).

---

### Task 13: Delete old subscriptions RPC handler and worker

**Files:**
- Delete: `internal/app/server/rpc/sdk/subscriptions/handler.go`
- Delete: `internal/app/workers/subscriptions/processor.go`
- Delete: `internal/app/workers/subscriptions/worker.go`

**Step 1: Delete all files and directories**

```bash
rm -rf internal/app/server/rpc/sdk/subscriptions
rm -rf internal/app/workers/subscriptions
```

---

### Task 14: Write new devices RPC handler

**Files:**
- Create: `internal/app/server/rpc/sdk/devices/handler.go`

**Step 1: Write the handler**

```go
package devices

import (
	"context"
	"log/slog"

	"connectrpc.com/connect"

	"github.com/fivebitsio/cotton/internal/app/server/rpc"
	"github.com/fivebitsio/cotton/internal/deps/nats"
	devicesv1 "github.com/fivebitsio/cotton/internal/gen/proto/devices/v1"
	"github.com/fivebitsio/cotton/internal/slogx"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/proto"
)

type Server struct {
	producer jetstream.JetStream
}

func NewServer(js jetstream.JetStream) (*Server, error) {
	return &Server{
		producer: js,
	}, nil
}

func (s *Server) Upsert(
	ctx context.Context,
	req *connect.Request[devicesv1.UpsertRequest],
) (*connect.Response[devicesv1.UpsertResponse], error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	principal, err := rpc.MustGetPrincipalWithProject(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, err)
	}

	msg := &devicesv1.DeviceOperationMessage{
		OperationType:     devicesv1.DeviceOperationType_DEVICE_OPERATION_TYPE_UPSERT,
		DeviceId:          req.Msg.GetId(),
		AutoProperties:    req.Msg.GetAutoProperties(),
		CustomProperties:  req.Msg.GetCustomProperties(),
		Platform:          req.Msg.GetPlatform(),
		ProfileExternalId: req.Msg.GetProfileExternalId(),
		Token:             req.Msg.GetToken(),
		ProjectId:         principal.Project.ID,
	}

	data, err := proto.Marshal(msg)
	if err != nil {
		slog.ErrorContext(ctx, "failed to marshal device operation message", slogx.Error(err))
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	if _, err = s.producer.Publish(ctx, nats.DeviceOpsSubject, data); err != nil {
		slog.ErrorContext(ctx, "failed to publish device operation to NATS", slogx.Error(err))
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	return connect.NewResponse(&devicesv1.UpsertResponse{}), nil
}

func (s *Server) UpdateStatus(
	ctx context.Context,
	req *connect.Request[devicesv1.UpdateStatusRequest],
) (*connect.Response[devicesv1.UpdateStatusResponse], error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	principal, err := rpc.MustGetPrincipalWithProject(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, err)
	}

	msg := &devicesv1.DeviceOperationMessage{
		OperationType: devicesv1.DeviceOperationType_DEVICE_OPERATION_TYPE_UPDATE_STATUS,
		DeviceId:      req.Msg.GetId(),
		Status:        req.Msg.GetStatus(),
		ProjectId:     principal.Project.ID,
	}

	data, err := proto.Marshal(msg)
	if err != nil {
		slog.ErrorContext(ctx, "failed to marshal device operation message", slogx.Error(err))
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	if _, err = s.producer.Publish(ctx, nats.DeviceOpsSubject, data); err != nil {
		slog.ErrorContext(ctx, "failed to publish device operation to NATS", slogx.Error(err))
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	return connect.NewResponse(&devicesv1.UpdateStatusResponse{}), nil
}

func (s *Server) UpdateToken(
	ctx context.Context,
	req *connect.Request[devicesv1.UpdateTokenRequest],
) (*connect.Response[devicesv1.UpdateTokenResponse], error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	principal, err := rpc.MustGetPrincipalWithProject(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, err)
	}

	msg := &devicesv1.DeviceOperationMessage{
		OperationType: devicesv1.DeviceOperationType_DEVICE_OPERATION_TYPE_UPDATE_TOKEN,
		DeviceId:      req.Msg.GetId(),
		Token:         req.Msg.GetToken(),
		ProjectId:     principal.Project.ID,
	}

	data, err := proto.Marshal(msg)
	if err != nil {
		slog.ErrorContext(ctx, "failed to marshal device operation message", slogx.Error(err))
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	if _, err = s.producer.Publish(ctx, nats.DeviceOpsSubject, data); err != nil {
		slog.ErrorContext(ctx, "failed to publish device operation to NATS", slogx.Error(err))
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	return connect.NewResponse(&devicesv1.UpdateTokenResponse{}), nil
}
```

**RPC handler behavior:**

| RPC | Auth | Publishes To | Operation Type |
|-----|------|-------------|----------------|
| `Upsert` | SDK API key → `principal.Project.ID` | `devices.ops` | `DEVICE_OPERATION_TYPE_UPSERT` |
| `UpdateStatus` | SDK API key → `principal.Project.ID` | `devices.ops` | `DEVICE_OPERATION_TYPE_UPDATE_STATUS` |
| `UpdateToken` | SDK API key → `principal.Project.ID` | `devices.ops` | `DEVICE_OPERATION_TYPE_UPDATE_TOKEN` |

All RPCs are fire-and-forget — they publish to NATS and return immediately. Actual DB writes happen in the worker.

**Verification:**
- [ ] Package is `devices`
- [ ] Imports `devicesv1` (not `subscriptionsv1`)
- [ ] Uses `nats.DeviceOpsSubject` (not `nats.SubscriptionOpsSubject`)
- [ ] `Upsert` sends `ProfileExternalId` in the operation message
- [ ] All RPCs inject `ProjectId` from auth principal
- [ ] No `RegisterSubscription` or `SetProfileExternalID` methods

---

### Task 15: Write new devices worker

**Files:**
- Create: `internal/app/workers/devices/worker.go`
- Create: `internal/app/workers/devices/processor.go`

**Step 1: Write `worker.go`**

```go
package devices

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	natsworker "github.com/fivebitsio/cotton/internal/deps/nats"
	"github.com/fivebitsio/cotton/internal/deps/postgres"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/sethvargo/go-envconfig"
)

func Run(ctx context.Context) error {
	var cfg postgres.Config
	if err := envconfig.Process(ctx, &cfg); err != nil {
		return err
	}

	pgRO, err := postgres.NewReaderPool(ctx, &cfg)
	if err != nil {
		return err
	}
	defer pgRO.Close()

	pgW, err := postgres.NewWriterPool(ctx, &cfg)
	if err != nil {
		return err
	}
	defer pgW.Close()

	natsClient, err := natsworker.New(ctx)
	if err != nil {
		return err
	}
	defer natsClient.Close()

	slog.InfoContext(ctx, "Starting device worker...")
	return StartWorker(ctx, pgRO, pgW, natsClient)
}

func StartWorker(ctx context.Context, pgRO *pgxpool.Pool, pgW *pgxpool.Pool, natsClient *natsworker.NATSClient) error {
	consumerConfig, err := natsClient.GetConsumerConfigByName("device-processor-durable")
	if err != nil {
		return fmt.Errorf("failed to get device consumer config: %w", err)
	}

	deviceWorker := NewWorker(pgRO, pgW)

	messageProcessor := func(ctx context.Context, msg jetstream.Msg) error {
		return deviceWorker.ProcessMessage(ctx, msg.Data())
	}

	config := natsworker.WorkerConfig{
		StreamName:        consumerConfig.StreamName,
		ConsumerName:      consumerConfig.DurableName,
		DurableName:       consumerConfig.DurableName,
		Concurrency:       100,
		ProcessingTimeout: 30 * time.Second,
		MaxDeliver:        consumerConfig.MaxDeliver,
		AckWait:           30 * time.Second,
	}

	worker, err := natsworker.NewWorker(config, messageProcessor)
	if err != nil {
		return err
	}

	return worker.Start(ctx, natsClient)
}
```

**Step 2: Write `processor.go`**

```go
package devices

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/fivebitsio/cotton/internal/core/devices"
	devicesv1 "github.com/fivebitsio/cotton/internal/gen/proto/devices/v1"
	"github.com/fivebitsio/cotton/internal/gen/repo/dbwrite"
	"github.com/fivebitsio/cotton/internal/slogx"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/xid"
	"google.golang.org/protobuf/proto"
)

type Worker struct {
	deviceService *devices.Service
	profilesWrite *dbwrite.Queries
}

func NewWorker(pgRO *pgxpool.Pool, pgW *pgxpool.Pool) *Worker {
	return &Worker{
		deviceService: devices.NewService(pgRO, pgW),
		profilesWrite: dbwrite.New(pgW),
	}
}

func protoMapToAny(m any) (map[string]any, error) {
	data, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	var result map[string]any
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, err
	}
	return result, nil
}

func (w *Worker) ProcessMessage(ctx context.Context, data []byte) error {
	msg := &devicesv1.DeviceOperationMessage{}
	if err := proto.Unmarshal(data, msg); err != nil {
		slog.ErrorContext(ctx, "failed to unmarshal device operation message", slogx.Error(err))
		return err
	}

	switch msg.OperationType {
	case devicesv1.DeviceOperationType_DEVICE_OPERATION_TYPE_UPSERT:
		return w.handleUpsert(ctx, msg)
	case devicesv1.DeviceOperationType_DEVICE_OPERATION_TYPE_UPDATE_STATUS:
		return w.handleUpdateStatus(ctx, msg)
	case devicesv1.DeviceOperationType_DEVICE_OPERATION_TYPE_UPDATE_TOKEN:
		return w.handleUpdateToken(ctx, msg)
	default:
		slog.WarnContext(ctx, "unknown device operation type", slog.Int("type", int(msg.OperationType)))
		return fmt.Errorf("unknown operation type: %v", msg.OperationType)
	}
}

func (w *Worker) handleUpsert(ctx context.Context, msg *devicesv1.DeviceOperationMessage) error {
	// Step 1: Resolve or create profile
	profile, err := w.profilesWrite.SaveProfile(ctx, dbwrite.SaveProfileParams{
		AutoProperties:   map[string]any{},
		CustomProperties: map[string]any{},
		ExternalID:       msg.GetProfileExternalId(),
		ID:               xid.New().String(),
		ProjectID:        msg.GetProjectId(),
	})
	if err != nil {
		slog.ErrorContext(ctx, "failed to save profile for device upsert", slogx.Error(err))
		return err
	}

	// Step 2: Convert proto maps to Go maps
	autoProps, err := protoMapToAny(msg.GetAutoProperties())
	if err != nil {
		slog.ErrorContext(ctx, "failed to convert auto_properties", slogx.Error(err))
		return err
	}

	customProps, err := protoMapToAny(msg.GetCustomProperties())
	if err != nil {
		slog.ErrorContext(ctx, "failed to convert custom_properties", slogx.Error(err))
		return err
	}

	// Step 3: Upsert device
	if _, err := w.deviceService.SaveDevice(ctx, dbwrite.SaveProfileDeviceParams{
		AutoProperties:   autoProps,
		CustomProperties: customProps,
		ID:               msg.GetDeviceId(),
		Platform:         msg.GetPlatform(),
		ProfileID:        profile.ID,
		ProjectID:        msg.GetProjectId(),
		Status:           "active",
		Token:            msg.GetToken(),
	}); err != nil {
		slog.ErrorContext(ctx, "failed to save device", slogx.Error(err))
		return err
	}

	return nil
}

func (w *Worker) handleUpdateStatus(ctx context.Context, msg *devicesv1.DeviceOperationMessage) error {
	if _, err := w.deviceService.UpdateDeviceStatus(ctx, msg.GetDeviceId(), msg.GetProjectId(), msg.GetStatus()); err != nil {
		slog.ErrorContext(ctx, "failed to update device status", slogx.Error(err))
		return err
	}
	return nil
}

func (w *Worker) handleUpdateToken(ctx context.Context, msg *devicesv1.DeviceOperationMessage) error {
	if _, err := w.deviceService.UpdateDeviceToken(ctx, msg.GetDeviceId(), msg.GetProjectId(), msg.GetToken()); err != nil {
		slog.ErrorContext(ctx, "failed to update device token", slogx.Error(err))
		return err
	}
	return nil
}
```

**Worker operation case table:**

| Operation | Input | DB Operations | Success | Failure Cases |
|-----------|-------|---------------|---------|---------------|
| **UPSERT** | device_id, project_id, profile_external_id, platform, token, auto_properties, custom_properties | 1. `SaveProfile` (upsert by project_id + external_id) → 2. `SaveProfileDevice` (upsert by project_id + id) | Device created/updated with resolved profile_id | See below |
| **UPDATE_STATUS** | device_id, project_id, status | `UpdateProfileDeviceStatus` | Status updated | Device not found → pgx error |
| **UPDATE_TOKEN** | device_id, project_id, token | `UpdateProfileDeviceToken` | Token updated | Device not found → pgx error |

**UPSERT edge cases:**

| Case | What Happens | Result |
|------|-------------|--------|
| New profile + new device | `SaveProfile` inserts, `SaveDevice` inserts | Both created |
| Existing profile + new device | `SaveProfile` returns existing (ON CONFLICT), `SaveDevice` inserts | New device linked to existing profile |
| Existing profile + existing device | `SaveProfile` returns existing, `SaveDevice` updates (ON CONFLICT) | Properties merged, platform/token/status overwritten |
| New profile + existing device (profile changed) | `SaveProfile` inserts new profile, `SaveDevice` updates device with new profile_id | Device re-linked to new profile |
| Empty `profile_external_id` | `SaveProfile` will insert with empty `external_id` — relies on unique constraint `(project_id, external_id)` | Should be validated at RPC layer or fail on constraint |
| Empty `auto_properties` / `custom_properties` | `protoMapToAny` returns empty map → `coalesce(@auto_properties, '{}')` → `jsonb_shallow_merge` with `'{}'` is no-op | Existing properties preserved |
| `SaveProfile` fails (DB error) | Returns error, device NOT created/updated | Message retried by NATS (up to max_deliver=5) |
| `SaveDevice` fails (DB error) | Returns error, profile was already created/updated | Message retried — profile upsert is idempotent so no harm |

**Verification:**
- [ ] `worker.go` references `device-processor-durable` consumer name
- [ ] `processor.go` handles exactly 3 operation types
- [ ] `handleUpsert` calls `SaveProfile` BEFORE `SaveDevice`
- [ ] `handleUpsert` passes `profile.ID` (resolved) to `SaveDevice`, not the external_id
- [ ] `handleUpsert` defaults `Status` to `"active"` for new devices
- [ ] No `handleProfileLink`, `handleUpdateHeartbeat`, or `handleUpdateMetadata` methods
- [ ] `protoMapToAny` helper is present (same as old processor)

---

### Task 16: Update delivery RPC handler

**Files:**
- Modify: `internal/app/server/rpc/shared/delivery/handler.go`

**Step 1: Update subscription_id references**

In the `RecordEvent` handler (line 50):

| Old | New |
|-----|-----|
| `SubscriptionId: req.Msg.GetSubscriptionId(),` | `DeviceId: req.Msg.GetDeviceId(),` |

**Verification:**
- [ ] No `SubscriptionId` or `GetSubscriptionId` references in file

---

### Milestone 4 Checklist

- [ ] `internal/app/server/rpc/sdk/subscriptions/` directory gone
- [ ] `internal/app/workers/subscriptions/` directory gone
- [ ] `internal/app/server/rpc/sdk/devices/handler.go` exists with 3 RPCs
- [ ] `internal/app/workers/devices/worker.go` exists
- [ ] `internal/app/workers/devices/processor.go` exists with 3 operation handlers
- [ ] Delivery RPC handler uses `DeviceId`
- [ ] Commit: `feat: add devices RPC handler and worker`

---

## Milestone 5: Wiring & Cleanup (Tasks 17-20)
> After this milestone: everything compiles, builds, and tests pass.

---

### Task 17: Update campaign worker to use devices service

**Files:**
- Modify: `internal/app/workers/campaigns/processor.go`

**Step 1: Update imports**

| Old | New |
|-----|-----|
| `subscriptionssvc "github.com/fivebitsio/cotton/internal/core/subscriptions"` | `devicessvc "github.com/fivebitsio/cotton/internal/core/devices"` |

**Step 2: Update struct**

| Old | New |
|-----|-----|
| `subscriptionService *subscriptionssvc.Service` | `deviceService *devicessvc.Service` |

**Step 3: Update `NewWorker`**

| Old | New |
|-----|-----|
| `subscriptionService: subscriptionssvc.NewService(pgRO, pgW),` | `deviceService: devicessvc.NewService(pgRO, pgW),` |

**Step 4: Update `ProcessMessage`**

| Old (line) | New |
|------------|-----|
| `subscriptions, err := w.subscriptionService.GetSubscriptionsByProject(ctx, campaign.ProjectID)` | `devices, err := w.deviceService.GetActiveDevicesByProject(ctx, campaign.ProjectID)` |
| `return fmt.Errorf("failed to get subscriptions for project %s: ..."` | `return fmt.Errorf("failed to get active devices for project %s: ..."` |
| `slog.Int("subscription_count", len(subscriptions))` | `slog.Int("device_count", len(devices))` |
| `for _, sub := range subscriptions {` | `for _, device := range devices {` |
| `if string(sub.Status) == subscriptionssvc.StatusActive {` | **REMOVE this check** — `GetActiveDevicesByProject` already filters |
| `if err := w.deliveryService.SendNotification(ctx, campaign, sub); err != nil {` | `if err := w.deliveryService.SendNotification(ctx, campaign, device); err != nil {` |
| `slog.String("subscription_id", sub.ID),` | `slog.String("device_id", device.ID),` |
| `slog.Int("total_count", len(subscriptions))` | `slog.Int("total_count", len(devices))` |
| `slog.InfoContext(ctx, "Processing subscriptions for campaign",` | `slog.InfoContext(ctx, "Processing devices for campaign",` |

**Campaign delivery flow (new):**

```
Campaign message received
  → GetActiveDevicesByProject(project_id)
    → For each device:
      → SendNotification(campaign, device)
        → Router switches on device.Platform
          → FCMService.SendNotification(campaign, device)
            → Send to device.Token via Firebase
```

**Campaign delivery case table:**

| Case | What Happens |
|------|-------------|
| No active devices for project | Empty slice returned, loop doesn't execute, campaign marked complete |
| All sends succeed | Campaign marked `complete` |
| Some sends fail | `failCount > 0`, campaign marked `fail`, individual errors logged |
| All sends fail | Campaign marked `fail` |
| `GetActiveDevicesByProject` fails | Returns error, campaign not updated — message retried |

**Verification:**
- [ ] No imports of `subscriptions` or `subscriptionssvc`
- [ ] Uses `GetActiveDevicesByProject` (not `GetSubscriptionsByProject`)
- [ ] No `StatusActive` comparison in the loop (pre-filtered by query)
- [ ] All log keys use `device_id` / `device_count`

---

### Task 18: Update server.go registration

**Files:**
- Modify: `internal/app/server/server.go`

**Step 1: Update imports**

| Old | New |
|-----|-----|
| `"github.com/fivebitsio/cotton/internal/app/server/rpc/sdk/subscriptions"` | `devicesrpc "github.com/fivebitsio/cotton/internal/app/server/rpc/sdk/devices"` |
| `"github.com/fivebitsio/cotton/internal/gen/proto/subscriptions/v1/subscriptionsv1connect"` | `"github.com/fivebitsio/cotton/internal/gen/proto/devices/v1/devicesv1connect"` |

**Step 2: Update handler creation (lines 68-73)**

| Old | New |
|-----|-----|
| `subscriptionsServer, err := subscriptions.NewServer(d.nats.GetJetStream())` | `devicesServer, err := devicesrpc.NewServer(d.nats.GetJetStream())` |
| `subscriptionsPath, subscriptionsHandler := subscriptionsv1connect.NewSubscriptionsServiceHandler(subscriptionsServer, handlerOpts)` | `devicesPath, devicesHandler := devicesv1connect.NewDevicesServiceHandler(devicesServer, handlerOpts)` |

**Step 3: Update mux registration (line 91)**

| Old | New |
|-----|-----|
| `mux.Handle(subscriptionsPath, sdkMW.Wrap(subscriptionsHandler))` | `mux.Handle(devicesPath, sdkMW.Wrap(devicesHandler))` |

**Step 4: Update reflection (line 103)**

| Old | New |
|-----|-----|
| `subscriptionsv1connect.SubscriptionsServiceName,` | `devicesv1connect.DevicesServiceName,` |

**Verification:**
- [ ] No imports containing `subscriptions`
- [ ] `devicesv1connect.NewDevicesServiceHandler` called
- [ ] Devices handler registered under SDK middleware (API key auth, no CORS)
- [ ] Reflection service list includes `devicesv1connect.DevicesServiceName`

---

### Task 19: Update CLI commands and build targets

**Files:**
- Modify: `cmd/cotton/main.go`
- Modify: `cmd/workers/subscription/main.go`
- Modify: `Makefile`

**Step 1: Update `cmd/cotton/main.go`**

Import change:

| Old | New |
|-----|-----|
| `"github.com/fivebitsio/cotton/internal/app/workers/subscriptions"` | `"github.com/fivebitsio/cotton/internal/app/workers/devices"` |

Command change:

| Old | New |
|-----|-----|
| `var subscriptionCmd = &cobra.Command{` | `var deviceCmd = &cobra.Command{` |
| `Use: "subscription",` | `Use: "device",` |
| `Short: "Start the subscription worker",` | `Short: "Start the device worker",` |
| `Run: run(subscriptions.Run),` | `Run: run(devices.Run),` |

Init change:

| Old | New |
|-----|-----|
| `workerCmd.AddCommand(subscriptionCmd)` | `workerCmd.AddCommand(deviceCmd)` |

Dev command change (line 116):

| Old | New |
|-----|-----|
| `g.Go(func() error { return subscriptions.Run(ctx) })` | `g.Go(func() error { return devices.Run(ctx) })` |

**Step 2: Update `cmd/workers/subscription/main.go`**

| Old | New |
|-----|-----|
| `"github.com/fivebitsio/cotton/internal/app/workers/subscriptions"` | `"github.com/fivebitsio/cotton/internal/app/workers/devices"` |
| `if err := subscriptions.Run(ctx); err != nil {` | `if err := devices.Run(ctx); err != nil {` |
| `slog.ErrorContext(ctx, "error starting subscription worker", ...)` | `slog.ErrorContext(ctx, "error starting device worker", ...)` |

**Step 3: Update `Makefile`**

| Old | New |
|-----|-----|
| `go build -o bin/cotton-worker-subscription ./cmd/workers/subscription` | `go build -o bin/cotton-worker-device ./cmd/workers/subscription` |

Note: The directory `cmd/workers/subscription/` can stay as-is (it's just a main.go entrypoint) — only the binary name and import change. Renaming the directory is optional.

**CLI command change:**

| Old | New |
|-----|-----|
| `./bin/cotton worker subscription` | `./bin/cotton worker device` |
| `./bin/cotton-worker-subscription` | `./bin/cotton-worker-device` |

**Verification:**
- [ ] `./bin/cotton worker device` is the new subcommand
- [ ] `./bin/cotton dev` starts device worker (not subscription worker)
- [ ] No `subscriptions` import in `cmd/cotton/main.go`
- [ ] No `subscriptions` import in `cmd/workers/subscription/main.go`
- [ ] Makefile builds `cotton-worker-device`

---

### Task 20: Build, test, and final cleanup

**Step 1: Run code generation**

```bash
make sqlc
make rpc
```

Both should succeed (already done in earlier tasks, but rerun to be safe).

**Step 2: Format code**

```bash
go fmt ./...
```

**Step 3: Build all binaries**

```bash
make build
```

**Build error case table:**

| Error | Likely Cause | Fix |
|-------|-------------|-----|
| `undefined: subscriptionsv1` | Missed an import update | Search for `subscriptionsv1` in all `.go` files |
| `undefined: dbread.Subscription` | Missed a type reference | Search for `Subscription` in non-generated `.go` files |
| `undefined: nats.SubscriptionOpsSubject` | Missed subject constant update | Check `internal/deps/nats/subjects.go` |
| `cannot use device (variable of type dbread.ProfileDevice) as dbread.Subscription` | Missed delivery layer update | Check `internal/core/delivery/` files |
| `too many arguments` or `not enough arguments` | Method signature changed | Check which service method has wrong params |
| `undefined: subscriptions.Run` | Missed CLI import update | Check `cmd/cotton/main.go` and `cmd/workers/subscription/main.go` |

**Step 4: Run tests**

```bash
make test
```

**Step 5: Final grep — no subscription references should remain**

```bash
grep -r "subscription" --include="*.go" --exclude-dir=internal/gen | grep -v "_test.go" | grep -v "vendor"
```

Should return zero results (excluding generated code which was regenerated, and test files).

```bash
grep -r "subscription" --include="*.sql" --include="*.proto" --include="*.yaml"
```

Should return zero results.

**Verification:**
- [ ] `make sqlc` succeeds
- [ ] `make rpc` succeeds
- [ ] `make build` succeeds — all 7 binaries compile
- [ ] `make test` passes
- [ ] Zero `subscription` references in hand-written `.go` files
- [ ] Zero `subscription` references in `.sql`, `.proto`, `.yaml` files

---

### Milestone 5 Checklist

- [ ] Campaign worker uses devices service
- [ ] `server.go` registers DevicesService
- [ ] CLI uses `worker device` subcommand
- [ ] `make build` succeeds
- [ ] `make test` passes
- [ ] No stale subscription references anywhere
- [ ] Commit: `feat: complete subscription to profile_devices migration`

---

## Full File Inventory (final state)

### Deleted
```
schema/postgres/queries/read/subscriptions.sql
schema/postgres/queries/write/subscriptions.sql
proto/subscriptions/v1/subscriptions.proto
internal/gen/proto/subscriptions/ (entire dir)
internal/core/subscriptions/service.go
internal/app/server/rpc/sdk/subscriptions/handler.go
internal/app/workers/subscriptions/processor.go
internal/app/workers/subscriptions/worker.go
```

### Created
```
schema/postgres/migrations/008_replace_subscriptions_with_profile_devices.sql
schema/postgres/queries/read/profile_devices.sql
schema/postgres/queries/write/profile_devices.sql
proto/devices/v1/devices.proto
internal/gen/proto/devices/v1/devices.pb.go (generated)
internal/gen/proto/devices/v1/devicesv1connect/devices.connect.go (generated)
internal/gen/repo/dbread/profile_devices.sql.go (generated)
internal/gen/repo/dbwrite/profile_devices.sql.go (generated)
internal/core/devices/service.go
internal/app/server/rpc/sdk/devices/handler.go
internal/app/workers/devices/processor.go
internal/app/workers/devices/worker.go
```

### Modified
```
proto/delivery/v1/delivery.proto
schema/nats/streams.yaml
schema/nats/consumers.yaml
internal/deps/nats/subjects.go
internal/core/delivery/service.go
internal/core/delivery/router.go
internal/core/delivery/fcm.go
internal/app/server/rpc/shared/delivery/handler.go
internal/app/workers/campaigns/processor.go
internal/app/server/server.go
cmd/cotton/main.go
cmd/workers/subscription/main.go
Makefile
```
