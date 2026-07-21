# Profiles

Detailed reference for the profiles subsystem (`internal/core/profiles`, `internal/app/server/rpc/shared/profiles`, `proto/shared/profiles`). Linked from the root [`CLAUDE.md`](../../CLAUDE.md) — read this when working on profile reads, the activity summary, soft-delete, or device subscriptions.

## Profile Properties

Profiles store properties as a single JSONB field (`properties`) rather than separate `auto_properties` and `custom_properties` fields. This simplifies the data model while preserving the ability to distinguish between auto-populated and custom properties at the application level through property naming conventions (e.g., `$` prefix for auto-properties).

## Profiles Read API

`shared.profiles.v1.ProfilesService` is backed by ClickHouse for reads and PostgreSQL for delete-side effects.

- `Get`, `GetByExternalId`, and `List` all return the same `Profile` shape from `proto/shared/profiles/v1/profiles.proto`.
- `Profile` includes the base profile record (`id`, `external_id`, `properties`, timestamps, `project_id`) plus an optional nested `activity` summary.
- `Delete` is different: it enqueues GDPR/DPDP erasure (see [`../compliance/4.1-erasure-scope.md`](../compliance/4.1-erasure-scope.md)); the synchronous prelude soft-deletes the PostgreSQL profile and deactivates devices.

**The person model: identified profiles ∪ derived anonymous persons.**

Reads serve *persons*, not just profile rows. An app that never calls `identify` still has real users — every unclaimed `distinct_id` with activity is a first-class person:

- **Identified profiles** are rows in ClickHouse `profiles`, written only by the identify pipeline (`profile-upsert` worker).
- **Derived anonymous persons** are synthesized at read time from `distinct_id_activity_states` — one person per unclaimed `distinct_id`, with `id = distinct_id`, empty `external_id`, empty `properties`, `create_time` = merged first-seen, `update_time` = merged last-seen. No row is ever written for them; the rollup **is** the materialization.
- A `distinct_id` is **claimed** (must not surface as its own person) when it appears in `claimed_ids` (`claimedIDsCTE` in `internal/core/profiles/service.go`): alias_ids from `profile_aliases`, profile ids (including soft-deleted tombstones), and non-empty external_ids (post-identify events are keyed by `external_id` — without this every identified user would grow a trait-less anonymous doppelgänger).
- **Cookieless ids never become persons.** Migration 011's `WHERE NOT startsWith(distinct_id, 'cookieless-')` on the activity MV means a cookieless visitor (server-derived daily-rotating id, see [ingestion.md](ingestion.md)) mints no derived anonymous person — without it, every visitor-day would create a ghost. `GetByID` of a cookieless id is `ErrProfileNotFound`; the claimed-id machinery is untouched. Identify/alias with a cookieless id is structurally rejected by `anonymous_id`'s existing `^$|^anon-` pattern (pinned by `TestIdentify_RejectsCookielessAnonymousID`). **Bare-id erasure of a cookieless id does not work** — `RequestErasureByID` gates on `hasActivity`, which probes the very rollup 011 filters, so the call returns `ErrProfileNotFound` and writes no ledger row while the events remain. `RequestErasureByExternalID` / `DeleteDataSubject` carry no activity probe and do erase. That gap is defensible rather than a bug — a cookieless id is unknowable to the visitor it describes and can never be identified, so no data subject can present one, and re-admitting these ids to the MV would recreate the ghost persons 011 exists to prevent — but the bare-id route must not be documented as reaching them.
- The derived design is **resurrection-proof by construction**: a late event for a merged (aliased) `distinct_id` re-fires the activity MV, but the alias keeps the id claimed and its activity folds into the canonical profile. There is no anon row to un-delete — the failure mode that would exist if anon profiles were materialized into the `ReplacingMergeTree`.

**Read data sources.**

- Base identified-profile fields come from ClickHouse `profiles` (`schema/clickhouse/migrations/003_create_profiles.sql`), queried through the `latest_profiles` CTE in `internal/core/profiles/service.go`. This is the current-state projection of the `ReplacingMergeTree(insert_time)` table.
- Alias resolution comes from ClickHouse `profile_aliases` (`schema/clickhouse/migrations/002_create_profile_aliases.sql`), queried through `latest_profile_aliases`.
- Activity fields come from `distinct_id_activity_states` (`schema/clickhouse/migrations/005_create_profile_activity_summary.sql`), an incremental `AggregatingMergeTree` rollup keyed by `(project_id, distinct_id)`.
- The per-IDENTIFIED-profile activity summary is built by `profileActivitySummaryCTE`: it unions the canonical profile ID, the external_id, and all alias IDs for that profile, joins those identities to `distinct_id_activity_states`, then re-aggregates to one row per `(project_id, profile_id)`. Derived anonymous persons carry their own single-`distinct_id` summary from `anonPersonsCTE` — no alias fan-out to re-aggregate.
- Every read query is assembled by `personsQuery`: `persons` (union of `latest_profiles` and `anon_persons`) LEFT JOIN `persons_activity` (union of the identified summary and the anon summaries), under the `p` / `activity_summary` aliases that filter conditions bind against. The anon-side `properties` is a `CAST('{}', 'JSON(...)')` matching the profiles column type, so profile-property filters (dot paths, `IS_NOT_SET`, numeric subcolumns) treat an anonymous person exactly like a trait-less profile.
- **Get resolution order** (`GetByID`): identified profile or derived anon person via the union → if NotFound, alias lookup (`resolveAliasTarget`) redirects a claimed anon id to its canonical profile (single-hop by construction), so pre-identify URLs and event links keep working after a merge → NotFound. `GetByExternalID("")` fails fast — an empty external_id would otherwise arbitrarily match an anonymous person.
- **Cost posture:** the anon branch is a streaming GROUP BY over the states table's own primary key, O(project distinct_ids) per query — the same class as the identified summary CTE. If per-project anonymous cardinality outgrows this (order 10⁷), the escape hatch is a dedicated first-seen-ordered person index maintained by an insert-once writer; a pure ClickHouse MV cannot maintain one (per-batch first-seen drifts under `ReplacingMergeTree` merge collapse, which is why anon persons are derived rather than materialized into `profiles`).

**`Profile.activity` semantics.**

- `activity` is omitted (`nil`) when the profile has no recorded events in ClickHouse (`total_events == 0`).
- `first_seen`, `last_seen`, `total_events`, and `sessions` come from aggregate states over all events for the resolved identity set.
- `pageviews` is derived as `sum(kind = 'page_view')` in the ClickHouse rollup.
- `browser`, `browser_version`, `os`, `os_version`, `device`, `country`, `region`, and `city` come from the latest event per distinct ID via `argMaxState(..., occur_time)`, then are merged across aliases at the profile level.
- There is currently no `channel` field in the profile API. The stable channel derivation now exists — `internal/attribution/channel.go` is the single normative taxonomy, feeding the event-level `$channel` auto-property ([ingestion.md](ingestion.md)) — but exposing a profile-level channel still requires a deliberate proto field and an attribution-window decision; do not invent one ad hoc in handlers.

**List / pagination behavior.**

- `ProfilesService.List` is a server-streaming RPC. The server emits `ListResponse` pages until exhaustion.
- Page size is server-controlled (`const pageSize = 100` in `internal/app/server/rpc/shared/profiles/handler.go`); the client cannot request a custom size.
- Pagination is keyset-based on `(create_time DESC, id DESC)`. `next_page_token` is an opaque base64url cursor carrying the last row's `create_time` and `id`.
- For derived anonymous persons `create_time` is the merged first-seen — immutable except when a backfill delivers an *older* event mid-pagination (the person can shift earlier and be missed on that pass; bounded, same class of race the events explorer accepts).

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

ClickHouse profiles use `is_deleted UInt8` for the same purpose; the identify worker publishes `ProfileUpsertMessage` with `is_deleted=true` to sync soft-deletes. The erasure path does **not** publish a ClickHouse tombstone — the compliance worker physically deletes the ClickHouse profile rows instead (a separate tombstone would race that delete and could resurrect a hidden row). The worker's profiles delete covers **every frozen distinct_id**, not just the canonical profile id, so merge-tombstone rows keyed by absorbed anon ids are physically removed too.

Erasure **by id** (`Delete`) also accepts non-profile person ids via a resolution ladder — claimed alias → canonical subject, external_id-shaped id → external_id path, derived anonymous person (`id` IS the `distinct_id`) → bare-id erasure with no Postgres side effects. See [`../compliance/4.1-erasure-scope.md`](../compliance/4.1-erasure-scope.md).

## Device Subscriptions

`profile_devices.profile_id` is nullable. Devices can be registered anonymously (no profile exists yet). When the SDK later calls Subscribe with a `profile_id` or `profile_external_id`, the upsert links the device via `coalesce(excluded.profile_id, profile_devices.profile_id)` — a re-subscribe with a profile never unlinks an already-linked device. The identify worker uses `LinkDeviceToProfile` which always overwrites `profile_id` to support account switching (old profile → new profile).

The FK uses `ON DELETE SET NULL`. This is load-bearing for GDPR/DPDP erasure: the compliance worker hard-deletes the profile, so it must delete `profile_devices` **first** (via `DeleteDevicesByProfileID`) — otherwise SET NULL would orphan a device row still holding the push token + endpoint. Outside erasure, profiles are soft-deleted and devices are deactivated within the same transaction.
