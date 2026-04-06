# Unified Identify RPC Design

## Problem

The current SDK profiles API exposes two RPCs — `Register(profile_id, properties)` and `Identify(profile_id, external_id)` — requiring SDK integrators to manage an internal `profile_id`, understand the two-step lifecycle, and coordinate ordering between the calls. Analytics platforms like Segment and Mixpanel offer a single `identify(userId, traits?)` call. Cotton should match that ergonomics.

## Design

### Proto — `sdk.profiles.v1`

Replace both RPCs with a single `Identify`:

```protobuf
service ProfilesSDKService {
  rpc Identify(IdentifyRequest) returns (IdentifyResponse);
}

message IdentifyRequest {
  string external_id = 1;            // required, min_len: 1
  google.protobuf.Struct traits = 2; // optional — user properties to merge
  string anonymous_id = 3;           // optional — anonymous profile to merge+delete
}

message IdentifyResponse {}

message ProfileIdentifyMessage {
  string external_id = 1;
  google.protobuf.Struct traits = 2;
  string anonymous_id = 3;
  string project_id = 4;
}
```

**Deleted:** `RegisterRequest`, `RegisterResponse`, `ProfileRegisterMessage`, and the `Register` RPC.

### Handler

Single `Identify` method on `Server`. Extracts principal via `MustGetPrincipalWithProject`, builds a `ProfileIdentifyMessage`, publishes to `profiles.identify`.

### Worker — Two Paths

**Path 1: No `anonymous_id` (common case)**

1. Upsert profile by `(project_id, external_id)` using new SQL query.
2. Server generates `profile_id` (xid) for new profiles.
3. Shallow-merge traits into existing properties on conflict.
4. Publish `ProfileUpsertMessage` to ClickHouse.

**Path 2: With `anonymous_id` (anonymous merge)**

1. Upsert-by-external-id (same as Path 1) to ensure target profile exists.
2. Look up anonymous profile by `(project_id, anonymous_id)`.
3. If found: merge properties into target, reassign devices, soft-delete anonymous profile, publish alias.
4. If not found: skip merge (idempotent retry or already merged).

### New SQL Query

```sql
-- name: UpsertProfileByExternalID :one
INSERT INTO profiles (id, project_id, external_id, properties)
VALUES (@id, @project_id, @external_id, coalesce(@properties::jsonb, '{}'))
ON CONFLICT (project_id, external_id) DO UPDATE SET
  properties = jsonb_shallow_merge(profiles.properties, excluded.properties)
RETURNING *;
```

Existing queries reused for the merge path: `MergeProfileProperties`, `ReassignProfileDevices`, `DeleteProfileByIDAndProjectID`.

### NATS Changes

- **Deleted:** `ProfileRegisterSubject` (`profiles.register`), `DLQProfilesRegisterSubject` (`dlq.profiles.register`), `profile-register-processor-durable` consumer.
- **Kept:** `ProfileIdentifySubject` (`profiles.identify`) carries the new `ProfileIdentifyMessage` format.

### Seed Data

`internal/app/seed/postgres/seed.go` changes from `RegisterProfile` + `SetProfileExternalID` to the new `UpsertProfileByExternalID` query.

### Unchanged

- **Shared `ProfilesService`** (Get, GetByExternalId, List, Delete) — reads are unaffected.
- **Alias worker** — still receives `ProfileAliasMessage` from merge path.
- **Upsert worker** — still receives `ProfileUpsertMessage` for ClickHouse sync.
- **Device subscribe** — already supports `profile_external_id`, no changes.
- **Events ingestion** — uses `distinct_id`, independent of profiles.

## Files to Create/Modify

| Action | File |
|--------|------|
| Modify | `proto/sdk/profiles/v1/profiles.proto` |
| Regenerate | `internal/gen/proto/sdk/profiles/v1/` |
| Modify | `internal/app/server/rpc/sdk/profiles/handler.go` |
| Modify | `internal/app/workers/profiles/identify/identify.go` |
| Delete | `internal/app/workers/profiles/register/register.go` |
| Modify | `internal/deps/nats/subjects.go` |
| Add query | `schema/postgres/queries/write/profiles.sql` |
| Regenerate | `internal/gen/repo/dbwrite/` |
| Modify | `internal/app/seed/postgres/seed.go` |
| Modify | NATS migration (remove register consumer) |
| Modify | `internal/app/` CLI wiring (remove register worker startup) |
