# Profiles

Detailed reference for the profiles subsystem (`internal/core/profiles`, `internal/app/server/rpc/shared/profiles`, `proto/shared/profiles`). Linked from the root [`CLAUDE.md`](../../CLAUDE.md) — read this when working on profile reads, the activity summary, soft-delete, or device subscriptions.

## Profile Properties

Profiles store properties as a single JSONB field (`properties`) rather than separate `auto_properties` and `custom_properties` fields. This simplifies the data model while preserving the ability to distinguish between auto-populated and custom properties at the application level through property naming conventions (e.g., `$` prefix for auto-properties).

## Profiles Read API

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
- Unsupported auto keys should stay explicit errors. In particular, list filters do not currently support channel/referrer/UTM fields or typed numeric auto-properties such as `$bot_score`.
- Numeric auto-property operators (`<`, `<=`, `>`, `>=`, `BETWEEN`, `NOT BETWEEN`) only work for auto fields that provide a numeric expression. The current profile-list auto summary fields are string-only, so numeric operators against them must fail rather than silently coerce.

## Profile Soft-Delete

Profiles use soft-delete via a `deletion_time timestamptz` column (NULL = active). All read queries filter `deletion_time IS NULL`. The `SoftDeleteProfileByIDAndProjectID` query sets `deletion_time = now()`; the `deletion_time IS NULL` guard makes it idempotent (0 rows if already deleted). Soft-delete is the normal path **and** phase 1 of GDPR/DPDP erasure — the compliance worker additionally **hard-deletes** the row via `HardDeleteProfileByIDAndProjectID` (see [`../compliance/4.1-erasure-scope.md`](../compliance/4.1-erasure-scope.md)).

ClickHouse profiles use `is_deleted UInt8` for the same purpose; the identify worker publishes `ProfileUpsertMessage` with `is_deleted=true` to sync soft-deletes. The erasure path does **not** publish a ClickHouse tombstone — the compliance worker physically deletes the ClickHouse profile row instead (a separate tombstone would race that delete and could resurrect a hidden row).

## Device Subscriptions

`profile_devices.profile_id` is nullable. Devices can be registered anonymously (no profile exists yet). When the SDK later calls Subscribe with a `profile_id` or `profile_external_id`, the upsert links the device via `coalesce(excluded.profile_id, profile_devices.profile_id)` — a re-subscribe with a profile never unlinks an already-linked device. The identify worker uses `LinkDeviceToProfile` which always overwrites `profile_id` to support account switching (old profile → new profile).

The FK uses `ON DELETE SET NULL`. This is load-bearing for GDPR/DPDP erasure: the compliance worker hard-deletes the profile, so it must delete `profile_devices` **first** (via `DeleteDevicesByProfileID`) — otherwise SET NULL would orphan a device row still holding the push token + endpoint. Outside erasure, profiles are soft-deleted and devices are deactivated within the same transaction.
