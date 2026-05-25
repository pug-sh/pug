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

**Closed-bucket watermark pattern (refreshable APPEND).** To keep refreshes idempotent and avoid double-counting, scope the source filter to closed buckets keyed off **event time**, not insert time:

```sql
WHERE toStartOfFiveMinutes(occur_time) BETWEEN
        toStartOfFiveMinutes(now() - INTERVAL 15 MINUTE)
    AND toStartOfFiveMinutes(now() - INTERVAL 5 MINUTE)
```

The trailing 5-minute lag ensures the most recent bucket is closed before it's read; the 10-minute window covers retries and slightly-late events. Late-arriving rows still bucket by their original `occur_time`, and the rollup's `ReplacingMergeTree(last_seen)` collapses retried bucket rows on background merge.

**Use a Go worker, not an MV, when** the work has external side effects (NATS publish, API calls, Postgres writes — e.g. the campaign scheduler at `internal/app/workers/scheduler/scheduler.go`), needs multi-step orchestration, or reads from non-CH sources.

**Version requirement.** Refreshable MVs went stable in ClickHouse 24.10. Cotton's dev infra pins `clickhouse/clickhouse-server:26.5` (`infra/dev/docker-compose.yaml`), so the requirement is satisfied; verify before relying on the feature in any new environment.

New MVs go in `schema/clickhouse/migrations/` as goose migrations; pair the rollup table DDL and the `CREATE MATERIALIZED VIEW` statement in the same migration file.

**Migration editing rule.** If a migration has **never** been applied in any environment whose state must be preserved (for example production, staging, or a shared dev DB), it is acceptable to edit that migration in place. Once a migration has been applied anywhere that matters, treat it as immutable and add a new forward migration instead. Do not create a follow-up migration solely to rewrite an unapplied migration.

## ClickHouse Query Builder Conventions

- Prefer `internal/core/clickhouse` query builder for ClickHouse query construction in core packages (`insights`, `events`, filters-related query helpers).
- Use parameterized limits (`LIMIT ?`) through `Query.Limit(...)` and pass `int64` values consistently.
- Use `RawCond(...)` only for expression-level fragments that are awkward to model otherwise (for example `occur_time >= now() - INTERVAL 30 DAY` or `IN ?` tuple bindings). Keep full query structure (`SELECT/FROM/WHERE/GROUP/ORDER/LIMIT`) in the builder.
- For property-values query helpers, query builder methods now return build errors; callers must propagate those errors instead of relying on raw-SQL fallbacks.
- **Top-k crash on mixed fixed-length sort keys (CH 26.5+).** A wide-table `ORDER BY <DateTime64> DESC, <UUID> DESC LIMIT n` crashes with `TYPE_MISMATCH` in `__topKFilter` (the events pagination shape — `occur_time` + `event_id`), but only when lazy materialization defers wide `Map(Variant)`/`JSON` columns past the `LIMIT`. Call `Query.DisableTopKDynamicFiltering()` on such queries (see `GetEventExplorer`, `GetActivityFeed`). A sort key containing a variable-length column (`String`) is auto-exempt by ClickHouse — profiles pagination (`create_time` + `id String`) needs no opt-out (verified on 26.5.1). Remove the opt-out once upstream fixes the mis-typing.
