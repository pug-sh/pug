# PR Review: `feat/multi-query-support`

**Scope:** 55 files, ~4,400 additions, ~990 deletions across 21 commits
**Features:** ClickHouse query builder, filter groups, multi-event queries, funnels, retention insights, profile sync
**Review rounds:** 3 (initial review → fixes → re-review)

---

## Critical Issues (3 found — all resolved: 1 N/A, 1 false positive, 1 fixed)

### ~~1. Proto wire-breaking field renumbering in `activity.proto`~~

**Source:** code-reviewer
**File:** `proto/shared/activity/v1/activity.proto`
**Verdict: NOT APPLICABLE — proto not in production yet, no existing clients**

### ~~2. `sqlStringLiteral` allows SQL injection in funnel step labels~~

**Source:** test-analyzer
**File:** `internal/core/insights/builder.go:457`
**Verdict: FALSE POSITIVE**

Verified: `EventFilter.kind` has proto validation `pattern: "^[a-zA-Z0-9_.-]*$"` (`proto/common/v1/filters.proto:79`), which prevents single quotes and all SQL injection characters. The `\'` escaping is also valid in ClickHouse by default (`backslash_as_escape_sequence=1`). The claim that "ClickHouse uses `''` not `\'`" is incorrect for default settings. While `BuildQuery` is public, any internal caller constructing a `QueryRequest` with a malicious `kind` would be misusing the API, and even then `\'` provides correct escaping under default CH settings. Not a real injection risk.

### ~~3. Profile delete NATS publish errors silently swallowed~~

**Source:** silent-failure-hunter
**File:** `internal/app/server/rpc/shared/profiles/handler.go:69-76`
**Verdict: CONFIRMED — FIXED**

Handler now returns `connect.CodeInternal` error on marshal or publish failure instead of swallowing.

---

## High Priority Issues (5 found — 3 previously fixed, 1 false positive, 1 confirmed)

### ~~4. `panic()` in `BuildEventNamesQuery` / `buildPropertyKeysQuery`~~

**Source:** code-reviewer, silent-failure-hunter, type-design-analyzer
**File:** `internal/core/insights/builder.go:600, 631`
**Verdict: CONFIRMED — FIXED**

Changed signatures to `(string, []any, error)`. Updated callers in `service.go` and tests.

### ~~5. `Condition.err` silently dropped by `And()` / `Or()`~~

**Source:** silent-failure-hunter, type-design-analyzer
**File:** `internal/core/clickhouse/query.go:118-129`
**Verdict: CONFIRMED — FIXED**

Removed dead `err` field from `Condition`. Simplified `isZero()` and removed dead `c.err` check in `Build()`.

### ~~6. `buildPropertyValuesQuery` bypasses query builder arg ordering~~

**Source:** code-reviewer, silent-failure-hunter
**File:** `internal/core/insights/builder.go:505-524`
**Verdict: CONFIRMED — FIXED**

Switched to `SelectExpr()` with `propertyKey` arg. Removed manual `append` prepend. Limit bumped from 10 to 100.

### ~~6b. CTE `top_vals` scoped only to first UNION ALL sub-query~~

**Source:** code-reviewer (round 2, 92% confidence)
**File:** `internal/core/insights/builder.go:128`
**Verdict: FALSE POSITIVE**

Verified by generating actual SQL for multi-event trends with breakdowns. The `WITH top_vals AS (...)` clause is emitted at the top of the entire statement by `query[0].Build()`. ClickHouse's WITH clause in the first SELECT of a UNION ALL is visible to all subsequent SELECTs — this is standard SQL behavior. The second sub-query correctly references `top_vals` without needing its own CTE definition. Args are also correct: CTE args come from query[0]'s Build(), and UnionAll.Build() concatenates all args in order.

### ~~6c. `GroupSeries` silently returns nil on breakdown mismatch~~

**Source:** error-handling review (Critical)
**File:** `internal/core/insights/executor.go:267-268`
**Verdict: CONFIRMED — FIXED**

Changed `GroupSeries` signature to return `([]*insightsv1.Series, error)`. Now returns a descriptive error on breakdown mismatch instead of silent nil. Updated all callers (handler, builder tests, integration tests, service tests).

---

## Medium Priority Issues (8 found — 2 previously fixed, 1 false positive, 5 confirmed)

### 7. No tests for upsert/register/identify workers

**Source:** test-analyzer
**Files:** `internal/app/workers/profiles/upsert/upsert.go`, `register/register.go`, `identify/identify.go`
**Verdict: CONFIRMED**

Verified: `Glob("internal/app/workers/profiles/**/*_test.go")` returns zero files. No test coverage for the new upsert worker or the modified register/identify workers. The `handleIdentify` function has complex branching (idempotent retry, first-time identification, merge path with sub-branches). A bug in the merge path could cause data loss.

### ~~8. Open transaction leaked on early return in identify worker~~

**Source:** silent-failure-hunter
**File:** `internal/app/workers/profiles/identify/identify.go:112-165`
**Verdict: CONFIRMED — FIXED**

Moved same-profile check before `tx.Begin()` to avoid opening an unnecessary transaction.

### ~~9. `withEventAlias` is fragile string replacement~~

**Source:** silent-failure-hunter, type-design-analyzer, test-analyzer
**File:** `internal/core/insights/builder.go:419-430`
**Verdict: CONFIRMED — FIXED**

Eliminated `withEventAlias` entirely. Added alias-aware condition builders: `propertyExpr(name, alias)`, `filterClause(f, alias)`, `EventConditionAliased(events, alias)`, `FilterClauseAliased(f, alias)`. Retention builder now generates aliased conditions from the start (e.g., `e.kind = ?`, `e.auto_properties[...]`) instead of post-hoc string replacement. Also refactored `EventCondition` to use `Or()`/`And()` combinators instead of manual string joining.

### ~~10. `EffectiveWindowSec` silently computes negative for invalid time ranges~~

**Source:** silent-failure-hunter
**File:** `internal/core/insights/funnel.go:91-98`
**Verdict: FALSE POSITIVE**

Verified: `TimeRange` in `proto/common/v1/time.proto:10-15` has CEL validation `this.from < this.to` (enforced by `validate.NewInterceptor()` before handlers run). The `from > to` case is impossible for all RPC callers. `EffectiveWindowSec` only accepts `*insightsv1.QueryRequest` which comes through the RPC chain. Internal callers constructing invalid proto messages would be misusing the API.

### 11. `FunnelUserEvents` uses parallel arrays instead of struct slice

**Source:** type-design-analyzer
**File:** `internal/core/insights/funnel.go:13`
**Verdict: CONFIRMED (design concern, not a bug)**

Verified: `Times []time.Time` and `StepMatches []int64` are parallel arrays. Length mismatch is caught at runtime in `ComputeFunnelTiming` line 39-41. The only producer is `QueryFunnelUserEvents` in `executor.go:210` which scans directly from ClickHouse rows.

### ~~12. Missing nil guard on `nats` in `profiles.NewServer`~~

**Source:** error-handling review, code-reviewer (round 2)
**File:** `internal/app/server/rpc/shared/profiles/handler.go:33`
**Verdict: CONFIRMED — FIXED**

Added `if nats == nil { panic("profiles: nats is nil") }` consistent with `NewExecutor` and `NewService`.

### ~~13. `handleIdentify` empty `upsertID` guard is dead code that hides inconsistency~~

**Source:** error-handling review
**File:** `internal/app/workers/profiles/identify/identify.go:233-235`
**Verdict: CONFIRMED — FIXED**

Added `slog.WarnContext` if the unreachable path is hit, so PG/CH inconsistency would be visible in logs.

### ~~14. Profile Delete: PG committed but NATS publish fails → permanent CH inconsistency~~

**Source:** code-reviewer (round 2)
**File:** `internal/app/server/rpc/shared/profiles/handler.go:50-79`
**Verdict: CONFIRMED — FIXED**

Changed to best-effort NATS publish: PG delete returns success to the client regardless of NATS outcome. Marshal/publish errors are logged for reconciliation but no longer fail the request (retry would get NotFound anyway).

---

## Suggestions

### Code quality
- ~~**`filterZero` mutates input slice**~~ — **FALSE POSITIVE.** `Where()` at `query.go:176-183` pre-filters zero-value conditions before adding to `q.wheres`. So `filterZero(q.wheres)` in `Build()` never finds zeros to filter — no mutation occurs. For `And()`/`Or()`, Go creates fresh slices for variadic args. No real risk.
- ~~**Redundant `RawCond` wrappers**~~ — **FIXED.** Removed deconstruct/reconstruct in `builder.go:362`, pass `startCond` directly.
- ~~**`EventCondition` builds raw SQL strings**~~ — **FIXED.** Refactored `singleEventCondition` to use `And()` combinator and `EventCondition` to use `Or()` combinator. Removed `wrap` parameter.

### Error handling
- ~~**`buildTopLevelFilterCondition` errors lack insight-type context**~~ — **FIXED.** All callers now wrap: `fmt.Errorf("trends: %w", err)`, `fmt.Errorf("segmentation: %w", err)`, etc.
- ~~**Executor `Query*` methods return raw CH errors**~~ — **FIXED.** All 7 methods now wrap: `fmt.Errorf("QueryTrends: %w", err)`, `fmt.Errorf("QueryScalar: %w", err)`, etc.

### Type design
- ~~**`BuildQuery` returns `(string, []any, error)` erasing insight type**~~ — **FIXED.** Added typed query structs (`TrendsQuery`, `ScalarQuery`, `FunnelQuery`, `FunnelTimingQuery`, `RetentionQuery`) with exported typed builders. Executor methods now accept these structs. Handler uses typed builders directly. `BuildQuery` kept for test backward compat.
- ~~**`From()` doc says "sets the FROM table"**~~ — **FIXED.** Updated to "sets the FROM clause".
- ~~**`GroupBy`/`OrderBy` comments say "sets"**~~ — **FIXED.** Updated to "appends".

### Comments
- ~~**Handler comment says "trends, segmentation, and funnel"**~~ — **FIXED.** Added "retention".
- ~~**`BuildAutoPropertyValuesQuery` comment uses wrong function name**~~ — **FIXED.** Updated to `BuildAutoPropertyValuesQuery`.

### Test gaps
- **No executor unit tests** — CONFIRMED but deferred. Infrastructure-dependent; covered by integration tests.
- ~~**Single-event retention** untested~~ — **FIXED.** Added `TestSingleEventRetention`.
- ~~**Multi-event trends with breakdowns** untested~~ — **FIXED.** Added `TestMultiEventTrendsWithBreakdowns`.
- ~~**`GroupSeries` breakdown mismatch** untested~~ — **FIXED.** Added `TestGroupSeries_BreakdownMismatchError`.
- **ClickHouse profiles table lacks `PARTITION BY`** — May be intentional for ID-keyed data. Deferred.

---

## Strengths

- **Query builder design** is the standout — clean `Condition` type with zero-value sentinels and `When` helper creates a composable API. Unexported fields enforce construction through typed constructors. Excellent test coverage (513 lines of tests).
- **Error handling migration** — Moving from `(string, []any)` to `(string, []any, error)` across all builder functions is a significant improvement with thorough propagation.
- **Filter builder** tests all 12 operators including error paths and LIKE metacharacter escaping.
- **Funnel timing tests** cover 7 scenarios including boundary behavior and documented limitations.
- **CLAUDE.md compliance** is strong — slog context variants, `slogx.Error()`, no sentinel error leakage to clients.
- **Builder tests** are behavioral (not implementation-coupled), using `strings.Contains` for SQL verification.
- **Event reader integration tests** are comprehensive — 14+ sub-tests each for activity feed and event explorer.
- **Security comments** — `PropertyExpr` safety comment documenting injection risk and validation boundaries is exemplary.
- **Alias-aware filter builders** — clean refactor eliminating the fragile `withEventAlias` string replacement. Conditions are now generated with correct prefixes from the start.
- **Best-effort NATS publish comment** — explains the design tradeoff, rationale, and operational consequence clearly.

---

## Round 3 Findings (re-review after fixes)

### New findings from re-review

**15. Executor scan/iteration errors lack context wrapping**
**Source:** error-handling review (round 3)
**File:** `executor.go` — all `Query*` methods
The initial `e.ch.Query()` errors are wrapped (`fmt.Errorf("QueryTrends: %w")`), but `rows.Scan()` and `rows.Err()` errors within the same methods are returned bare. A scan failure (e.g., type mismatch) would appear in logs without indicating which executor method produced it.

**16. `WriteEventFilterCondition` is dead code**
**Source:** type-design review (round 3)
**File:** `filters.go`
Zero production callers — only tested in `filters_test.go`. Now that `EventCondition` returns typed `Condition` values usable with the query builder, this legacy bridge function can be removed along with its tests.

**17. `isZero()`/`IsZero()` duplication**
**Source:** type-design review, comment review (round 3)
**File:** `query.go:24-31`
Unexported `isZero()` has identical doc comment to exported `IsZero()`. The private method can be inlined or removed.

**18. Add nil guard in `Query.With()`**
**Source:** type-design review (round 3)
**File:** `query.go`
Passing a nil CTE sub-query to `With()` would panic at `Build()` time when `c.sub.Build()` is called. A nil check would surface the error earlier with a clear message.

### Comment improvements suggested
- `buildFunnel` comment says "CTE chain" but it's a single CTE — should say "single-scan array-based query"
- `identify.go:133` comment references non-existent `upsertProfile` variable name
- `buildPropertyValuesQuery` should document LIMIT 100 and its relationship to cache threshold
- Add field-level comments to `ProfileUpsertMessage` proto (especially `is_deleted` semantics)

### Test gaps suggested
- Direct tests for `FilterClauseAliased`, `EventConditionAliased`, `PropertyCondition`
- Funnel/retention integration tests (currently only SQL structure is verified)
- `GroupRetentionSeries` empty input test

### Previously triaged — re-confirmed
- Proto field renumbering (#1, #2) — re-flagged, still N/A (not in production)
- `sqlStringLiteral` (#3) — re-flagged, still false positive (proto validation)
- `filterZero` (#4) — re-flagged, still false positive (`Where()` pre-filters)
- `EffectiveWindowSec` (#7) — re-flagged, still false positive (proto validation)
- Empty `upsertID` guard (#13) — re-flagged as should return error instead of warn+nil. Currently logs warning per previous fix.

---

## Remaining Open Items

| # | Priority | Issue | Status |
|---|----------|-------|--------|
| 7 | Medium | No worker tests (upsert/register/identify) — requires interface refactoring | Track as follow-up |
| 15 | Low | Executor scan/iteration errors lack wrapping | Track as follow-up |
| 16 | Low | Remove dead `WriteEventFilterCondition` + tests | Track as follow-up |
| 17 | Low | Remove `isZero()`/`IsZero()` duplication | Track as follow-up |
| 18 | Low | Add nil guard in `Query.With()` | Track as follow-up |
| 11 | Low | `FunnelUserEvents` parallel arrays (design concern) | Acceptable |
| ~~—~~ | ~~Low~~ | ~~`BuildQuery` typed query results~~ | **FIXED** |
| — | Low | Direct tests for aliased filter functions | Track as follow-up |
| — | Low | Funnel/retention integration tests | Track as follow-up |
| — | Low | ClickHouse profiles table `PARTITION BY` consideration | May be intentional |
