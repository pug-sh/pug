# PR #36 Review — Separate Public/Private Key Auth

Reviewed at: `cc89b99` (fixes code review comments)

## Overview

This PR separates project authentication into public and private API keys, adds Redis caching for key lookups, and includes seed tooling. Changes span 31 files (~1047 additions) across schema, proto, core services, server deps, infra config, and seed commands.

## Changed Files

- `.env.example`
- `Makefile`
- `cmd/cotton/main.go`
- `infra/dev/docker-compose.yaml`
- `internal/app/seed/clickhouse/csv.go`
- `internal/app/seed/clickhouse/deps.go`
- `internal/app/seed/clickhouse/generator.go`
- `internal/app/seed/clickhouse/seed.go`
- `internal/app/seed/postgres/deps.go`
- `internal/app/seed/postgres/seed.go`
- `internal/app/server/deps.go`
- `internal/app/server/rpc/auth.go`
- `internal/app/server/rpc/dashboard/projects/handler.go`
- `internal/app/server/rpc/dashboard/projects/types.go`
- `internal/app/server/server.go`
- `internal/core/auth/service.go`
- `internal/core/projects/repo.go`
- `internal/core/projects/service.go`
- `internal/deps/redis/config.go`
- `internal/deps/redis/redis.go`
- `internal/gen/proto/projects/v1/projects.pb.go` (generated)
- `internal/gen/repo/dbread/models.go` (generated)
- `internal/gen/repo/dbread/projects.sql.go` (generated)
- `internal/gen/repo/dbwrite/models.go` (generated)
- `internal/gen/repo/dbwrite/projects.sql.go` (generated)
- `proto/projects/v1/projects.proto`
- `schema/postgres/migrations/002_create_projects.sql`
- `schema/postgres/queries/read/projects.sql`
- `schema/postgres/queries/write/projects.sql`

---

## Critical

### 1. Postgres seed uses deleted column `api_key` — will crash at runtime

**File:** `internal/app/seed/postgres/seed.go:61-63`

The INSERT references `api_key`, which was renamed to `private_api_key`/`public_api_key` in this PR's migration. This seed command will always fail with `column "api_key" does not exist`.

**Fix:**

```go
privateKey := "prv_" + xid.New().String()
publicKey := "pub_" + xid.New().String()
_, err = s.deps.pg.Exec(ctx,
    `INSERT INTO projects (id, customer_id, display_name, private_api_key, public_api_key) VALUES ($1, $2, $3, $4, $5)`,
    projectID, customerID, "default", privateKey, publicKey,
)
```

Update the log output to print both keys.

### 2. `err == pgx.ErrNoRows` instead of `errors.Is()` — fragile error comparison

**File:** `internal/app/server/rpc/auth.go:51, 106, 124, 145`

Four places use direct `==` comparison instead of `errors.Is(err, pgx.ErrNoRows)`. If errors are ever wrapped (e.g., via the new Redis caching repo), invalid keys will be misreported as server errors. The rest of the codebase (workers, seed at `seed/postgres/seed.go:37`) consistently uses `errors.Is()`.

**Fix:** Replace all four instances:

```go
// Lines 51, 124, 145: instead of
if err == pgx.ErrNoRows {
// use
if errors.Is(err, pgx.ErrNoRows) {

// Line 106: instead of
if err != pgx.ErrNoRows {
// use
if !errors.Is(err, pgx.ErrNoRows) {
```

### 3. SDK auth accepts private keys — defeats the public/private separation

**File:** `internal/app/server/rpc/auth.go:42, 49` + `schema/postgres/queries/read/projects.sql:28`

`WithSDKAuth` is documented as "Accepts both public and private keys" (line 41) and uses `GetProjectAndCustomerByApiKey` which queries `WHERE public_api_key = $1 OR private_api_key = $1`. This is intentional per the code comment, but undermines the security boundary this PR introduces — a private key leaked through an SDK client would authenticate on both SDK and dashboard endpoints.

**Suggestion:** Create a dedicated `GetProjectAndCustomerByPublicApiKey` query for SDK auth. If accepting both is a deliberate design choice, document the security rationale.

### 4. Cache entries never expire (TTL = 0) — revoked keys stay valid forever

**File:** `internal/core/projects/repo.go:47, 77`

Both cache writes use `TTL: 0` (never expires). Combined with no cache invalidation (see #7), a revoked/rotated API key remains valid indefinitely through the cache. This is a security concern for an auth system.

**Fix:**

```go
const apiKeyCacheTTL = 5 * time.Minute
r.cache.Set(ctx, cacheKey, data, apiKeyCacheTTL)
```

---

## Medium

### 5. EOF check via string comparison instead of `errors.Is()`

**File:** `internal/app/seed/clickhouse/seed.go:166`

Uses `err.Error() == "EOF"`. This works today because `csv.go:51` returns raw `io.EOF`, but it's fragile — if the reader wraps the error, the check fails and a successful import reports failure.

**Fix:**

```go
if errors.Is(err, io.EOF) {
```

### 6. Proto field number reassignment — wire-breaking change

**File:** `proto/projects/v1/projects.proto:40-47`

The old `api_key` was field 1, `customer_id` was 2, `display_name` was 3, `fcm_service_json` was 4, `id` was 5. Now `customer_id` is 1, `display_name` is 2, etc. In protobuf, field numbers are the wire format identity — existing clients or serialized data using the old field numbers will decode fields incorrectly. If there are no deployed consumers, this is acceptable. Otherwise, old field numbers should be `reserved` and new fields should use higher numbers.

### 7. No cache invalidation path

**File:** `internal/core/projects/repo.go` + `internal/core/projects/service.go`

The `Repo` caches auth lookups but the `Service` (which handles creates/updates/deletes) has no mechanism to invalidate stale entries. When a project is deleted or a key rotated, the old cached entry persists until Redis is flushed or the TTL expires (currently never, per #4).

**Fix:** Add an `Invalidate` method to `Repo` and call it from `Service` on write operations, or give `Service` a reference to the cache.

### 8. Context cancellation returns `nil` instead of `ctx.Err()`

**File:** `internal/app/seed/clickhouse/seed.go:52-53, 152-153`

Ctrl+C during seeding exits with code 0 (success). In CI/CD pipelines, this masks incomplete seeding.

**Fix:**

```go
// Instead of:
return nil
// Use:
return ctx.Err()
```

---

## Low

### 9. ClickHouse close error silently discarded

**File:** `internal/app/seed/clickhouse/deps.go:20`

`_ = d.ch.Close()` discards the close error. The Redis wrapper (`redis.go:37-39`) and the main ClickHouse dep (`clickhouse.go:60`) both log close errors.

**Fix:**

```go
if err := d.ch.Close(); err != nil {
    slog.Error("error closing clickhouse connection", slog.Any("error", err))
}
```

### 10. Extract key generation into named functions

**File:** `internal/core/projects/service.go:32-33`

The key format (`"prv_" + xid`) implicitly couples a 4-char prefix with xid's 20-char output to match the `char(24)` column. A named function makes this self-documenting.

### 11. Add comment to `roToRPCMsg` explaining intentional `PrivateApiKey` omission

**File:** `internal/app/server/rpc/dashboard/projects/types.go:9-17`

The security-critical decision to omit `PrivateApiKey` in read responses is correct but undocumented.

### 12. Use distinct cache key prefixes for different lookup methods

**File:** `internal/core/projects/repo.go:13`

Both `GetProjectAndCustomerByPrivateApiKey` and `GetProjectAndCustomerByApiKey` share the prefix `"project:apikey:"`. Today the row types are structurally identical (both embed `Project` + `Customer`), so this works. But using distinct prefixes (`project:pubkey:`, `project:prvkey:`) would be more defensive if the query shapes ever diverge.

---

## Test Coverage

The project has no test files. This PR adds security-critical auth changes, a Redis caching layer, and schema migrations with no tests. Highest priority test gaps:

1. **Auth middleware** — public vs. private key enforcement matrix (public key accepted by `WithSDKAuth`, rejected by `WithDualAuth`)
2. **Redis cache** — hit/miss/error fallback paths
3. **Key generation** — prefix correctness, length matches `char(24)` column

---

## Strengths

- **Sound cache-aside pattern** — Redis failures fall through to Postgres gracefully, maintaining availability
- **Good security pattern** — private key only exposed at creation time via separate `roToRPCMsg`/`wToRPCMsg` conversion functions
- **Clean refactoring** — key generation moved from handlers into `Service.CreateProject`, reducing misuse surface area
- **Key prefixes** (`prv_`/`pub_`) provide visual disambiguation for debugging
- **Proper resource cleanup** in `server/deps.go` — closes previously-allocated resources when a later dependency fails
- **Idempotent seed design** — postgres seed is safe to run multiple times
- **Consistent logging style** — redis package follows the established `internal/deps/` convention of using `slog.Any("error", err)`

---

## False Positives Caught During Verification

The following were initially flagged by automated review agents but determined to be false positives after checking against the actual code:

### ~~Cache key collision between lookup methods~~

**Initially reported as:** Medium — `GetProjectAndCustomerByPrivateApiKey` and `GetProjectAndCustomerByApiKey` share the cache prefix `"project:apikey:"` and cache different Go types, risking type confusion on deserialization.

**Why false positive:** Both `GetProjectAndCustomerByApiKeyRow` and `GetProjectAndCustomerByPrivateApiKeyRow` are structurally identical — each embeds `Project` + `Customer` with the same fields in the same order (`dbread/projects.sql.go:19-22, 54-57`). The JSON round-trip produces the same bytes regardless of which type was marshaled. There is no actual type confusion or data corruption. Downgraded to a minor defensive suggestion (#12).

### ~~Inconsistent slog error attribute style in redis.go~~

**Initially reported as:** Low — `internal/deps/redis/redis.go` uses `slog.Any("error", err)` while the project standard is `slogx.Error(err)`.

**Why false positive:** All `internal/deps/` packages consistently use `slog.Any("error", err)` — postgres (`postgres.go:19,27`), clickhouse (`clickhouse.go:19,25,30,60`), and nats (`worker.go` in 12 places). The `slogx.Error(err)` helper is used in higher layers (`internal/core/`, `internal/app/server/rpc/`). Redis correctly follows the `internal/deps/` layer convention.
