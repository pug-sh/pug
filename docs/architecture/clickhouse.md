# ClickHouse

Detailed reference for ClickHouse query construction and storage (`internal/core/clickhouse`, `internal/deps/clickhouse`, `schema/clickhouse`). Linked from the root [`CLAUDE.md`](../../CLAUDE.md) — read this when building ClickHouse queries, touching the events table, or adding materialized views.

## ClickHouse Query Builder

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

## ClickHouse Events Table

- **Engine:** `ReplacingMergeTree(insert_time)` — on merge, keeps the row with the highest `insert_time` per dedup key. Avoid `SELECT ... FINAL` — it forces synchronous deduplication at query time and is expensive. Background merges provide eventual consistency, which is sufficient for all current queries including per-user event history. Only use `FINAL` if a query has a hard correctness requirement that cannot tolerate transient duplicates.
- **Dedup key (ORDER BY):** `(project_id, toStartOfMinute(occur_time), kind, event_id)` — minute granularity matches the finest time resolution dashboards use (per-minute charts). Full-precision `occur_time` is stored in the column.
- **Partitioning:** `PARTITION BY toYYYYMM(occur_time)` — ReplacingMergeTree **never** deduplicates across partitions.
- **occur_time stability:** `occur_time` is required (enforced by proto validation). Clients must send a stable value on retries — a different value that crosses a minute boundary lands in a different sort-key bucket (dedup fails); if it crosses a month boundary it lands in a different partition (permanent duplicate).
- **Unknown PropertyValue variants drop the offending property and continue.** The proto-to-Variant translator maps each `*commonv1.PropertyValue` oneof case to its typed `chcol.Variant` slot; an unsupported variant drops the offending key from the row and the rest of the batch still inserts. Drops are observable via `events.property_dropped_total{stage, reason}` (worker-side `stage="ingestion"`, enrichment-side `stage="enrichment"`). The error path is unreachable through the validated RPC ingress (`oneof.required`) and only fires on proto-future drift or nil values. The SDK is not signalled per-property; it still sees `accepted=N` for the batch.

## ClickHouse Materialized Views

Pure CH→CH aggregation work (read from CH, aggregate, write to a CH rollup table, no external side effects) belongs in a materialized view, not a Go cron worker. CH-native scheduling, lifecycle, and refresh history (`system.view_refreshes`) come for free, and refreshable MVs read `FROM ... FINAL` once per refresh instead of forcing every dashboard query to pay that cost.

Pick the MV flavor based on whether aggregates are mergeable and whether the source needs dedup:

| Flavor                                              | When to use                                                                                              | Mechanics                                                                                                                                |
| --------------------------------------------------- | -------------------------------------------------------------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------- |
| Incremental (`TO target`, no `REFRESH`)             | Source is append-only and aggregates are partial-state-mergeable (`countState`, `maxState`, `sumState`)  | Runs as an insert trigger. Rollup uses `AggregatingMergeTree` with `*State`/`*Merge` aggregates. No staleness, no watermark, no `FINAL`. |
| Refreshable rebuild (`REFRESH EVERY ... TO target`) | Whole-table rebuild from a current-state source (e.g. `profiles`) where late-binding `FINAL` matters     | Replaces the entire target on every refresh. Bounded staleness equal to the refresh interval.                                            |
| Refreshable APPEND (`REFRESH EVERY ... APPEND TO`)  | Need `FROM events FINAL` for dedup and prefer incremental scan to full rebuild                           | Requires a closed-bucket watermark (below). Rollup engine should dedup on bucket key, e.g. `ReplacingMergeTree(last_seen)`.              |

Default to incremental when both conditions hold — refreshable MVs trade refresh-interval staleness and watermark complexity for `FINAL`, only worth it when you actually need it. Concrete examples in `schema/clickhouse/migrations/004_create_filter_schema_mvs.sql`: `event_names_mv` is incremental, `property_keys_event_buckets_mv` is refreshable APPEND, `property_keys_profile_current_mv` is refreshable rebuild.

**Dashboard dimensional rollup (incremental, EAV).** `dashboard_event_rollup_daily` (migration 006, extended by 009) is an `AggregatingMergeTree` keyed by `(project_id, kind, dim_name, day, dim_value, cookieless)` (migration 011 appended the `cookieless` key column) storing `SimpleAggregateFunction(sum, UInt64)` event counts and `AggregateFunction(uniq, String)` distinct-user state. Its incremental MV `ARRAY JOIN`s every event into one row per materialized auto-property dimension (twenty since 009) plus a synthetic `$__total__` row — 21 rows/event — so a single table answers both breakdown and total trends/segmentation. **Dimension values must read promoted auto-property columns** (`country`, `browser`, `pathname`, `channel`, …) — not `auto_properties['$…']` map keys, because ingest strips promoted keys into dedicated columns at write time. The **latest** MV definition (011's `MODIFY QUERY`) is the one pinned against the Go list and `PropertyExpr` / `AutoPropertyProjectionFor` (`TestMaterializedDimsMatchMigration`, `TestMigration011PromotedDimExprsMatch`); migration 006 is frozen at its historical content (`TestMigration006Frozen`). Reads re-aggregate the partial states with `sum(cnt)` / `uniqMerge(uniq_state)` (no `FINAL`): trends `GROUP BY` the derived time bucket (`granularityFunc(toDateTime(day))`), `kind`, and the top-N-bucketed breakdown value; segmentation is a single scalar aggregate over the window with no `GROUP BY`. The routing predicate and rollup query builders live in `internal/core/insights/rollup.go`.

**Accepted accuracy tradeoff (incremental rollup vs. dedup).** The rollup key omits `event_id` and the incremental MV sums `count()` at insert time, so it cannot dedup the at-least-once retries/redeliveries that the raw `events` `ReplacingMergeTree(insert_time)` collapses on merge. Consequently `TOTAL` and `PER_USER_AVG` served from the rollup over-count vs. the raw builders by the pipeline's redelivery rate — permanent and monotonic (the raw side converges to truth post-merge; the rollup never does). `UNIQUE_USERS` is immune (`uniqState` on `distinct_id` is idempotent). Accepted as a bounded inaccuracy for dashboard visualization; the divergence emerges only after the raw side merges, so a parity test must force a merge (or read raw `FINAL`) to observe it. For exact reconciliation with the raw insights path, switch to the refreshable-`APPEND` + `FROM events FINAL` + closed-bucket-watermark flavor above.

**Dashboard session rollup (incremental, session-grain).** `dashboard_session_rollup` (migration 007, extended by 010) is a second `AggregatingMergeTree`, keyed by `(project_id, kind, session_id)` — one logical row per session, not per day. It stores mergeable per-session states: `minState`/`maxState` of `occur_time` (session start/end), `countState()` (event count), and `argMinState`/`argMaxState(<dim>, occur_time)` for each materialized dimension's entry/exit value (sixteen dims → 32 state columns since 010). Its incremental MV emits two rows per event via `UNION ALL` — one with `kind=''` (the all-events aggregate) and one with the event's real `kind` — so a session metric scoped to a kind and the unscoped (all-events) metric both read from pre-aggregated state. Reads `GROUP BY session_id` and re-merge with `minMerge`/`maxMerge`/`countMerge`/`argMin(Max)Merge`, then apply the time window as a `HAVING` on the merged `start_time` (sessions are selected by start, never clipped — see [insights.md](insights.md#session-insights)). Dimension source columns mirror the raw builder (`toString(col)` for `LowCardinality`, bare for `String`); `TestMigration010SessionRollupColumnsMatchDims` pins the current column names against 010's `MODIFY QUERY` and `TestMigration010SessionRollupDimExprsMatch` pins the per-dimension source expression + entry/exit aggregate; migration 007 is frozen at its historical content (`TestMigration007Frozen`). The predicate `canUseSessionRollup` and the rollup builders live in `internal/core/insights/session_rollup.go`.

**Extending a live rollup (the migration-008/009/010 pattern).** The app is live, so rollup schema changes must never lose or double history. Three rules, each load-bearing:

1. **`MODIFY QUERY`, never DROP→CREATE.** `ALTER TABLE <mv> MODIFY QUERY` swaps a TO-table MV's SELECT atomically (supported on ClickHouse 26.5); dropping and recreating would lose *all* dims — including `$__total__` — for events inserted in the gap, permanently (rollups never reconcile).
2. **EAV rollup → delta backfill restricted to the NEW `dim_name`s.** New EAV key rows are disjoint from every existing row (`dim_name` is in the ORDER BY key), so there is no merge hazard — and existing dims must NOT be re-inserted (a full-list backfill would double their `cnt`). The sub-second MODIFY→INSERT overlap can double-count new-dim `cnt` only (`uniq_state` is idempotent) — accepted, same class as the redelivery over-count; a cutoff can't fully close it because an insert in flight during MODIFY is ambiguous either way.
3. **State-column rollup → partial-column backfill INSERT.** List ONLY the key columns + the new `AggregateFunction` states; omitted state columns take their implicit default — the **empty aggregate state, which is the merge identity** — so existing counts/durations/entry-exit states are untouched. This is the one backfill that is silently catastrophic if done naively (re-inserting `countState()` doubles every session's event count); pinned by `TestIntegrationWebAnalytics/session_rollup_partial_insert_merge_identity`. The argMin/argMax states are idempotent under duplicate merge, so the MODIFY→INSERT overlap is harmless here.

When the new dims derive from new columns (008), the derivation mutation lives in the **column** migration only and the rollup migration assumes an already-derived table — both run in the same PreSync job, so a copy of the mutation in the rollup migration would be a guaranteed no-op full-table rewrite, plus a second copy of the hairiest SQL in the repo to keep in sync. The rows that miss the mutation (ingested by old binaries between the migrate job and the last old pod draining) are recovered by re-running mutation → DELETE → backfill by hand, which is the one repair path for every such gap — see [web-analytics.md](web-analytics.md)'s deploy runbook. The mutation is pinned against the Go deriver by `TestIntegrationWebAnalytics/mutation_008_matches_attribution_derive`. Shipped migrations are never edited — the frozen-content tests enforce that the extension lands as a new migration.

**Adding a key column to a live rollup (the migration-011 pattern).** Migration 011 gave `dashboard_event_rollup_daily` a `cookieless` key column (computed in the MV as `toUInt8(startsWith(distinct_id, 'cookieless-'))`) so user-count reads exclude with `WHERE cookieless = 0` while TOTAL reads merge both key values. The `uniq_state` rows *are* split by the new key; correctness comes from `uniqMerge` being merge-safe across both values, so an unpredicated read is exact and both toggle states stay fast-path. Two constraints discovered the hard way, now load-bearing: (a) the column must join the ORDER BY **in the same ALTER that adds it** (`ADD COLUMN …, MODIFY ORDER BY (…)` — the only position ClickHouse accepts for a new key column), and (b) it must carry **no DEFAULT expression** — ClickHouse rejects defaulted columns joining a sorting key (code 36); a bare `UInt8` reads the type default 0 on pre-011 rows, which is historically exact since no cookieless rows predate the migration, so there is **nothing to backfill**. 011 also `MODIFY QUERY`s the 005 activity-states MV with `WHERE NOT startsWith(distinct_id, 'cookieless-')` (cookieless visitors must not mint derived persons — [profiles.md](profiles.md)). The prefix literal is pinned to `cookieless.IDPrefix` by `TestMigration011CookielessPrefixMatchesGo`; 009 is frozen by `TestMigration009Frozen` now that 011 restates the MV.

**Session-rollup accuracy tradeoff.** Same `event_id`-less keying as the dimensional rollup, affecting the two metrics that count events within a session: **`BOUNCE_RATE`** and **`AVG_EVENTS_PER_SESSION`**. A duplicate delivery inflates `event_count_state`, so a genuinely single-event (bounced) session reads `event_count > 1` and drops out of the bounce numerator — the rollup **under-reports** bounce rate by the redelivery rate — and `AVG_EVENTS_PER_SESSION` correspondingly **over-reports** (raw `count()` self-corrects post-merge for both). `SESSIONS`/`ENTRY`/`EXIT` count `session_id` groups (immune); `AVG_DURATION` uses idempotent min/max (immune). Pinned by `TestIntegration/session_rollup_bounce_duplicate_overcount_documented`.

**Closed-bucket watermark pattern (refreshable APPEND).** To keep refreshes idempotent and avoid double-counting, scope the source filter to closed buckets keyed off **event time**, not insert time:

```sql
WHERE toStartOfFiveMinutes(occur_time) BETWEEN
        toStartOfFiveMinutes(now() - INTERVAL 15 MINUTE)
    AND toStartOfFiveMinutes(now() - INTERVAL 5 MINUTE)
```

The trailing 5-minute lag ensures the most recent bucket is closed before it's read; the 10-minute window covers retries and slightly-late events. Late-arriving rows still bucket by their original `occur_time`, and the rollup's `ReplacingMergeTree(last_seen)` collapses retried bucket rows on background merge.

**Use a Go worker, not an MV, when** the work has external side effects (NATS publish, API calls, Postgres writes — e.g. the campaign scheduler at `internal/app/workers/scheduler/scheduler.go`), needs multi-step orchestration, or reads from non-CH sources.

**Version requirement.** Refreshable MVs went stable in ClickHouse 24.10. Pug's dev infra pins `clickhouse/clickhouse-server:26.5` (`infra/dev/docker-compose.yaml`), so the requirement is satisfied; verify before relying on the feature in any new environment.

New MVs go in `schema/clickhouse/migrations/` as goose migrations; pair the rollup table DDL and the `CREATE MATERIALIZED VIEW` statement in the same migration file.

**Migration editing rule.** If a migration has **never** been applied in any environment whose state must be preserved (for example production, staging, or a shared dev DB), it is acceptable to edit that migration in place. Once a migration has been applied anywhere that matters, treat it as immutable and add a new forward migration instead. Do not create a follow-up migration solely to rewrite an unapplied migration.

## ClickHouse Query Builder Conventions

- Prefer `internal/core/clickhouse` query builder for ClickHouse query construction in core packages (`insights`, `events`, filters-related query helpers).
- Use parameterized limits (`LIMIT ?`) through `Query.Limit(...)` and pass `int64` values consistently.
- Use `RawCond(...)` only for expression-level fragments that are awkward to model otherwise (for example `occur_time >= now() - INTERVAL 30 DAY` or `IN ?` tuple bindings). Keep full query structure (`SELECT/FROM/WHERE/GROUP/ORDER/LIMIT`) in the builder.
- For property-values query helpers, query builder methods now return build errors; callers must propagate those errors instead of relying on raw-SQL fallbacks.
- **Top-k crash on mixed fixed-length sort keys (CH 26.5+).** A wide-table `ORDER BY <DateTime64> DESC, <UUID> DESC LIMIT n` crashes with `TYPE_MISMATCH` in `__topKFilter` (the events pagination shape — `occur_time` + `event_id`), but only when lazy materialization defers wide `Map(Variant)`/`JSON` columns past the `LIMIT`. Call `Query.DisableTopKDynamicFiltering()` on such queries (see `GetEventExplorer`, `GetActivityFeed`). A sort key containing a variable-length column (`String`) is auto-exempt by ClickHouse — profiles pagination (`create_time` + `id String`) needs no opt-out (verified on 26.5.1). Remove the opt-out once upstream fixes the mis-typing.
