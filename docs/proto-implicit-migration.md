# Proto `field_presence = IMPLICIT` Migration

Tracking removal of `option features.field_presence = IMPLICIT;` from each proto file.
Edition stays at `"2023"` (buf does not yet support `"2024"`).

## Step-by-step procedure for each file

Follow these steps exactly for each proto file in order.

### 1. Read the proto file

Identify every singular scalar field (`string`, `bool`, `int32`, `uint32`, `int64`, `uint64`,
`float`, `double`, `bytes`) in every message. Message-type fields (`google.protobuf.Timestamp`,
`google.protobuf.Struct`, oneofs) are always pointer types and are unaffected.

### 2. Audit validation annotations on every scalar field

With IMPLICIT, a missing `(buf.validate.field).required = true` still enforces
"must be non-empty" when an `in`, `string.email`, `string.uuid`, `string.pattern`, or any
other value constraint is present — an unset field defaults to `""` which fails the
constraint. With EXPLICIT (after removing IMPLICIT), **unset optional fields skip all field
constraints entirely**. Only `required = true` forces constraint evaluation on unset fields.

**Rule:** for every scalar field that has a value constraint (e.g. `in`, `email`, `uuid`,
`pattern`, `min_len`) but no `(buf.validate.field).required = true`, ask: "should this field
be required?" If yes, add `required = true`. Common cases:

- `platform` / `status` with `in: [...]` — almost always required; add `required = true`.
- `email` with `string.email = true` — almost always required; add `required = true`.
- Optional fields with `in: [...]` or `min_len` that represent genuinely optional inputs do
  NOT get `required = true` — leave them as is. An unset optional field is valid.

**Also check `min_len`:** after migration, `required = true` alone only checks presence
(non-nil). If an empty string must be rejected, add `string.min_len = 1` alongside
`required = true`. This came up for `EventBatch.project_id`.

**CEL constraints are unaffected:** message-level `cel` expressions always evaluate.
Accessing an unset optional string in CEL returns `""` (zero value), so expressions like
`this.field == ''` continue to work correctly for "not set" checks. No CEL changes needed.

### 3. Remove IMPLICIT and regenerate

```
# Remove the line:
option features.field_presence = IMPLICIT;

# Regenerate:
make rpc
```

`make rpc` runs `buf lint` then `buf generate`. Fix any lint errors before continuing.

### 4. Fix Go call sites — construction

After regen, all scalar fields in the affected messages become pointer types (e.g. `*string`,
`*uint32`). Any struct literal that sets a scalar field directly will fail to compile.

**Pattern — string fields:**
```go
// Before
msg := &foov1.Foo{Name: someString}

// After
msg := &foov1.Foo{Name: proto.String(someString)}
```

Import `"google.golang.org/protobuf/proto"` if not already present.

**Pattern — non-string scalar fields (uint32, bool, etc.):**
```go
// Before
resp := &foov1.Response{Accepted: uint32(n)}

// After
accepted := uint32(n)
resp := &foov1.Response{Accepted: &accepted}
// or use a typed helper if one exists
```

Note: `uint32(n)` is not addressable; store in a local variable first.

**Empty-string trap:** `proto.String("")` creates a *present* field (non-nil pointer to `""`).
This passes `required = true` because required only checks presence, not value. The correct
fix is to add `string.min_len = 1` to the proto field so that empty strings are rejected by
value constraint regardless of how the field was set:
```proto
string project_id = 3 [
  (buf.validate.field).required = true,
  (buf.validate.field).string.min_len = 1
];
```
This keeps call sites simple (`proto.String(v)` always) and enforces the constraint in one
place. Do NOT work around this with conditional nil-setting in Go — it is verbose and moves
the constraint out of the proto definition where it belongs.

### 5. Fix Go call sites — reading

Direct field access (`msg.Field`) compiles fine for pointer types but dereferences are
unsafe if nil. Prefer the generated getters, which always return zero values for nil fields:

```go
// Prefer this everywhere (safe, returns "" for nil)
msg.GetName()

// Only use direct access when you know the field is set (e.g. after required validation)
*msg.Name
```

In tests that directly compare fields after unmarshaling, switch direct comparisons to
getters:
```go
// Before
if ident.ExternalId != "user-42" {

// After
if ident.GetExternalId() != "user-42" {
```

### 6. Build and test

```
go build ./...
make test
```

Ignore IDE diagnostics showing `cannot use *string as string` — these are stale and will
clear on the next language server refresh. `go build ./...` is authoritative.

**What to look for in failing tests after migration:**

- `expected validation error for empty X, got nil` — an empty-string field that previously
  failed a value constraint now passes because the unset field skips constraints with
  EXPLICIT. Likely needs `min_len = 1` added to the proto field (or `required = true` if
  not already present).
- `panic: nil pointer dereference` in a test that passes empty strings and expects
  validation to stop execution — the validation passed (see above), and execution reached
  code that calls a database or another nil dependency. Root cause is the same: missing
  `min_len` or `required` on the field.
- `nil.AsTime()` returning Unix epoch instead of Go zero time — any code that uses
  `.IsZero()` to detect an unset `*timestamppb.Timestamp` is wrong. Use `== nil` instead.

### 7. Commit

Stage: the `.proto` file, the entire `internal/gen/proto/<pkg>/` directory, all modified
Go files, and the updated checklist in this doc.

---

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
- [x] `proto/sdk/devices/v1/devices.proto`
- [x] `proto/sdk/profiles/v1/profiles.proto`
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
