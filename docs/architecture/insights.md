# Insights

Detailed reference for the insights subsystem (`internal/core/insights`, `proto/shared/insights`). Linked from the root [`CLAUDE.md`](../../CLAUDE.md) — read this when working on insights queries (trends, funnel, retention, segmentation).

## Insights Breakdown

Breakdowns are supported for trends, funnel, and retention. Segmentation does not support breakdowns.

- `QueryRequest.breakdowns` is `repeated Breakdown` — list of property keys to break down by (e.g. `[{property: "$country"}, {property: "$browser"}]`).
- **Attribution:** first-touch — each user is assigned the breakdown value(s) from their earliest matching event (`argMin(property, occur_time)`). This keeps funnel and retention per-user logic correct by not splitting a user across multiple groups.
- **Top-N bucketing:** the query builds a `top_vals` CTE and groups values outside the top N into `'$others'` to keep result sets bounded. The event scope of `top_vals` matches the query's aggregation scope:
  - Trends: `top_vals` covers all events matching any query event kind in the time range.
  - Funnel (counts and timing): `top_vals` is filtered to step-matching events.
  - Retention: `top_vals` is filtered to start-event rows only.
- **Two-phase aggregation pattern:** funnel (counts, timing) and retention breakdown queries avoid evaluating `argMin` twice by splitting into:
  1. An aggregation CTE that computes `argMin(expr, occur_time) AS raw_bd_N` once.
  2. A downstream CTE or SELECT that buckets `raw_bd_N` against `top_vals` as a plain scalar expression.
- **Response shape:** funnel and retention responses wrap their results in series objects keyed by breakdown combination:
  - `FunnelResult.series` → `repeated FunnelSeries` with `breakdown map<string,string>` + `steps repeated FunnelStep`
  - `RetentionResult.series` → `repeated RetentionSeries` with `breakdown map<string,string>` + `cohorts repeated RetentionCohort`
  - When no breakdowns are requested, a single series with an empty `breakdown` map is returned.

## Funnel Timing Statistics

When `include_step_timing` is true, each `FunnelStep` includes a `StepTiming` sub-message with per-step conversion time statistics computed in Go from per-user event timestamps (no extra ClickHouse query needed). All timing scalars are `google.protobuf.Duration`:

- `StepTiming.avg` — mean
- `StepTiming.median` — average-of-two-middles median
- `StepTiming.p95` — nearest-rank ceiling p95
- `StepTiming.distribution` — `repeated DistributionBucket` histogram across 8 fixed buckets: `0-30s`, `30s-2m`, `2-5m`, `5-15m`, `15-60m`, `1-6h`, `6-24h`, `24h+`

`FunnelStep.timing` is **absent** (nil) for step 0 (the entry step has no conversion time) and when `include_step_timing` is false. Steps with zero converters still emit `Timing` with a zero-filled distribution slice (not absent) — this distinguishes "timing not applicable" (step 0, `nil`) from "nobody converted yet" (allocated zeros). `DistributionBucket.upper_bound` (`google.protobuf.Duration`) is absent on the open-ended last bucket; clients should use `label` for display in that case.

The request-side `QueryRequest.conversion_window` is also a `google.protobuf.Duration`. Validated by two rules at the interceptor: a field-level `gte: 1s` rejects sub-second values (e.g. `500ms`), and a CEL `whole_seconds` rule rejects fractional-second values (e.g. `2.5s`, `1s + 1ns`) — `windowFunnel` accepts only integer-second windows, so anything else would silently truncate. When absent, the conversion window defaults to the full time range.

**Implementation:** `internal/core/insights/funnel_buckets.go` holds the bucket table (`funnelBucket` struct: `upper time.Duration`, `label string`, `openEnded bool`) and three pure helpers for median, percentile, and bucketing a pre-sorted slice. The funnel-timing compute function collects raw per-user deltas (`time.Duration`), sorts once, and calls the helpers; the proto-translation layer wraps the `time.Duration` results in `*durationpb.Duration` at the package boundary. Tests pin both the structural bucket invariants (strictly ascending bounds, `time.Duration(math.MaxInt64)` sentinel for the open-ended last bucket, exactly one open-ended bucket) and the user-visible nil-vs-zero-filled distribution distinction.

## Insights Granularity

`QueryRequest.granularity` controls the time-bucket size for trends and retention queries. Supported values (ordered finest → coarsest):

| Enum value             | ClickHouse function  | Max time range |
| ---------------------- | -------------------- | -------------- |
| `GRANULARITY_MINUTE`   | `toStartOfMinute`    | 6 hours        |
| `GRANULARITY_HOUR`     | `toStartOfHour`      | 14 days        |
| `GRANULARITY_DAY`      | `toStartOfDay`       | 365 days       |
| `GRANULARITY_WEEK`     | `toStartOfWeek`      | 4 years        |
| `GRANULARITY_MONTH`    | `toStartOfMonth`     | 10 years       |

- Limits are enforced by five `buf.validate.message.cel` rules on `QueryRequest` in `proto/shared/insights/v1/insights.proto` (ids `query_request.granularity_{minute,hour,day,week,month}_max_range`). The `validate.NewInterceptor()` wired on the Connect handler rejects violating requests with `CodeInvalidArgument` before the handler runs.
- The minute/hour/day limits are sized to keep per-series data point counts at ≤365 (MINUTE=360, HOUR=336, DAY=365). WEEK is capped at 1461 days (~4 years, ~209 buckets); MONTH is capped at 3652 days (~10 years, ~120 buckets), bounding partition scan range since the events table partitions monthly.
- **Retention caveat**: retention queries multiply cohort buckets × follow-up buckets (filtered to a triangular shape via `r.t >= r.cohort_time`). At WEEK granularity over the 4-year cap that's roughly (209 × 210)/2 ≈ 21,945 rows per series before breakdowns — substantially larger than the trends-equivalent ≤365 bound. The cap protects against unbounded scan cost, not against large retention result sets.
- **MINUTE granularity caveat**: charts visualize at the same boundary as the ClickHouse dedup key (`toStartOfMinute(occur_time)`), so any pre-merge transient duplicates show at full magnitude per bucket. Coarser granularities amortize duplicates across multiple minutes per bucket. See "ClickHouse Events Table" in [`clickhouse.md`](clickhouse.md) for dedup details.
- The caps fire for any `QueryRequest` regardless of `insight_type`, so funnel/segmentation requests with an oversized `granularity`/`time_range` combo are also rejected even though those insight types ignore granularity at query-build time.
- `GRANULARITY_UNSPECIFIED` is rejected at the field level via `not_in: [0]` — clients must explicitly choose a granularity. `granularityFunc` returns an error for UNSPECIFIED and any undefined enum value (e.g. a future enum added to the proto but not yet wired into the switch); the error surfaces through the `Build*Query` error path. Direct callers (workers, scripts) bypassing the interceptor must set `Granularity` explicitly.

## Dashboard Tile Default Time Ranges

Dashboard insight tiles persist `QueryRequest.granularity` and expose `DashboardTile.default_time_range` as a `common.v1.TimeRangePreset`. Supported relative presets are `LAST_1_HOUR`, `LAST_6_HOURS`, `LAST_24_HOURS`, `LAST_7_DAYS`, `LAST_14_DAYS`, `LAST_30_DAYS`, `LAST_90_DAYS`, `LAST_180_DAYS`, and `LAST_365_DAYS`. `UNSPECIFIED` is accepted at the RPC boundary; insight tiles normalize it to `LAST_30_DAYS`, while markdown tiles normalize any preset to `UNSPECIFIED`.

`DashboardTile.view_mode` is a `dashboard.dashboards.v1.DashboardTileViewMode` (`LINE`, `AREA`, `BAR_GROUPED`, `BAR_STACKED`, `TABLE`). Insight tiles normalize `UNSPECIFIED`/unknown to `LINE`; markdown tiles normalize any mode to `UNSPECIFIED`.

Both fields are stored as dedicated range-checked `dashboard_tiles` `smallint` columns (not in the `insight_query` JSONB) and mirror the core `TileViewMode` / `TileDefaultTimeRange` enums in `internal/core/dashboards/dashboards.go`. `granularity`, the absolute `time_range`, breakdowns/group-by, and filters stay in `insight_query`. `UpdateTile` full-replaces `view_mode`/`default_time_range` (like `layouts` and `insight_query`), so a client must send them on every update or they reset to the insight defaults.

`DashboardsService.QueryDashboard` loads stored insight tiles for a dashboard, resolves each tile's effective time range in priority order (`time_range_override`, then `insight_query.time_range` when present, then `default_time_range` preset), executes the query server-side, and returns `repeated DashboardTileQueryResult` keyed by `tile_id` in dashboard tile order. Markdown tiles are omitted. Per-tile failures populate `error_message` without failing the whole batch. Tile metadata stays on `Get`; this RPC returns analytics results only.

## Insights Filter Model

- Top-level insights filters are **group-based only**. In `shared.insights.v1`, use `filter_groups` and `filter_groups_operator` on `QueryRequest` and `SegmentUsersRequest`.
- Legacy top-level `filters` fields are removed from `proto/shared/insights/v1/insights.proto`. Tags are intentionally not reserved (pre-release, never shipped). Do not reintroduce them.
- Group semantics:
  - Within a group, conditions are combined using `FilterGroup.operator` (`AND` by default when unspecified).
  - Between groups, conditions are combined using `filter_groups_operator` (`AND` by default when unspecified).
- Per-event filters remain on `EventQuery.event.filters` and are independent of top-level filter groups.

## Retention Insight

- `shared.insights.v1.InsightType` supports `INSIGHT_TYPE_RETENTION`.
- Retention query semantics in `QueryRequest.events`:
  - `events[0]` = cohort/start event (required)
  - `events[1]` = return event (optional; defaults to `events[0]` when omitted)
- Retention responses use `QueryResponse.retention` (a `RetentionResult`):
  - `RetentionResult.series` is `repeated RetentionSeries` — one entry per breakdown combination (single entry when no breakdown)
  - `RetentionSeries.breakdown` is a `map<string, string>` of property key → value for this series
  - `RetentionSeries.cohorts` contains `repeated RetentionCohort`, one per cohort bucket
  - `RetentionCohort.cohort` stores the cohort timestamp (RFC3339)
  - `RetentionCohort.cohort_size` stores the number of users in the cohort
  - `RetentionCohort.points[].value` is retention percentage (`0..100`) across time buckets

## Insights Query Builders

Always use the type-specific builders — they provide compile-time safety between builder and executor:

| Insight type         | Builder                  | Query type          |
| -------------------- | ------------------------ | ------------------- |
| Trends               | `BuildTrendsQuery`       | `TrendsQuery`       |
| Segmentation         | `BuildSegmentationQuery` | `ScalarQuery`       |
| Funnel (counts)      | `BuildFunnelCountsQuery` | `FunnelQuery`       |
| Funnel (with timing) | `BuildFunnelTimingQuery` | `FunnelTimingQuery` |
| Retention            | `BuildRetentionQuery`    | `RetentionQuery`    |

All query types expose `.SQL()` and `.Args()`. All types except `ScalarQuery` also expose `.Properties()` and `.NumBreakdowns()`. `FunnelTimingQuery` also exposes `.Kinds()` and `.WindowSec()`.

All five emit `SETTINGS use_query_cache = 1, query_cache_ttl = 60` via `WithQueryCache(analyticsCacheTTL)` on the outermost query. Cache isolation between projects relies on `project_id` being a positional parameter on every cached builder; a builder that interpolates `project_id` into raw SQL would silently break tenant isolation. Property keys/values (including profile property keys/values), segment-users, and event-names builders intentionally omit the cache. See `analyticsCacheTTL` in `internal/core/insights/builder.go` for staleness mechanics with ReplacingMergeTree.
