# Proto `field_presence = IMPLICIT` Migration

Tracking removal of `option features.field_presence = IMPLICIT;` from each proto file.
Edition stays at `"2023"` (buf does not yet support `"2024"`).

## Ordering rationale

`field_presence = IMPLICIT` is a file-level option. Removing it makes all singular scalar
fields in that file's messages become pointer types in generated Go, requiring nil-checks
everywhere those fields are accessed.

Files are ordered so that protos with the most downstream consumers are done last — that
way the common types are already migrated before the files that use them, and each step
has the smallest possible blast radius.

## Group 1 — Standalone (no internal imports, not imported by internal protos)

These can be done in any order and are fully self-contained.

- [x] `proto/public/auth/v1/auth.proto`
- [x] `proto/sdk/events/v1/events.proto`
- [ ] `proto/sdk/devices/v1/devices.proto`
- [ ] `proto/sdk/profiles/v1/profiles.proto`
- [ ] `proto/dashboard/orgs/v1/orgs.proto`
- [ ] `proto/dashboard/projects/v1/projects.proto`
- [ ] `proto/shared/campaigns/v1/campaigns.proto`
- [ ] `proto/shared/delivery/v1/delivery.proto`
- [ ] `proto/shared/profiles/v1/profiles.proto`
- [ ] `proto/workers/profiles/v1/profiles.proto`
- [ ] `proto/common/v1/well_known_events.proto`

## Group 2 — Consumers of common (import common, not imported by others)

Do these after the common protos in Group 3 are migrated, so all imported scalar types
are already pointers when updating the Go side here.

- [ ] `proto/shared/activity/v1/activity.proto` — imports `filter_schema`, `filters`, `time`
- [ ] `proto/shared/insights/v1/insights.proto` — imports `filter_schema`, `filters`, `time`

## Group 3 — Common (imported by Group 2, highest blast radius — do last)

- [ ] `proto/common/v1/time.proto` — imported by `activity`, `insights`
- [ ] `proto/common/v1/filter_schema.proto` — imported by `filters`, `activity`, `insights`
- [ ] `proto/common/v1/filters.proto` — imports `filter_schema`; imported by `activity`, `insights`
