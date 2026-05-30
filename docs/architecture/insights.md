# Insights

Detailed reference for the insights subsystem (`internal/core/insights`, `proto/shared/insights`). Linked from the root [`CLAUDE.md`](../../CLAUDE.md) — read this when working on insights queries (trends, funnel, retention, segmentation).

## Insights Breakdown

Breakdowns are supported for trends, funnel, and retention. Segmentation does not support breakdowns.

- `QueryRequest.spec.breakdowns` (on the nested `InsightQuerySpec`) is `repeated Breakdown` — list of property keys to break down by (e.g. `[{property: "$country"}, {property: "$browser"}]`).
- **Attribution:** first-touch — each user is assigned the breakdown value(s) from their earliest matching event (`argMin(property, occur_time)`). This keeps funnel and retention per-user logic correct by not splitting a user across multiple groups.
- **Top-N bucketing:** raw-events queries return all breakdown values from ClickHouse; top-N collapse into `'$others'` happens in Go via `GroupSeries` / `GroupFunnelSeries` / `GroupRetentionSeries` using `QueryRequest.spec.breakdown_limit` (default 10). Tie-breaking on equal totals uses breakdown value ascending so bucketing matches the rollup fast path. The rollup path (`buildTrendsFromRollup`) applies top-N in SQL instead — see [Rollup Fast Path](#rollup-fast-path) below.
- **Funnel/retention builders** compute breakdown values with first-touch `argMin(property, occur_time)` in a single aggregation pass (no separate top_vals CTE).
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

The request-side `QueryRequest.spec.conversion_window` is also a `google.protobuf.Duration`. Validated by two rules at the interceptor: a field-level `gte: 1s` rejects sub-second values (e.g. `500ms`), and a CEL `whole_seconds` rule rejects fractional-second values (e.g. `2.5s`, `1s + 1ns`) — `windowFunnel` accepts only integer-second windows, so anything else would silently truncate. When absent, the conversion window defaults to the full time range.

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

## Dashboard Window (Default Time Range + Granularity)

The time window and granularity are **dashboard-level**, not per-tile: `dashboards.default_time_range` (a `common.v1.TimeRangePreset`) and `default_granularity` (a `shared.insights.v1.Granularity`) are dedicated columns, so one time picker drives every insight tile. Supported relative presets are `LAST_1_HOUR`, `LAST_6_HOURS`, `LAST_24_HOURS`, `LAST_7_DAYS`, `LAST_14_DAYS`, `LAST_30_DAYS`, `LAST_90_DAYS`, `LAST_180_DAYS`, and `LAST_365_DAYS`. `UNSPECIFIED` is accepted at the RPC boundary and normalized to `LAST_30_DAYS` / `GRANULARITY_DAY` (`DashboardDefaultTimeRangePresetFromDB` / `DashboardGranularityFromDB`).

`DashboardTile.view_mode` (a `dashboard.dashboards.v1.DashboardTileViewMode`: `LINE`, `AREA`, `BAR_GROUPED`, `BAR_STACKED`, `TABLE`, `KPI`) stays per-tile — each chart picks its own visualization. Insight tiles normalize `UNSPECIFIED`/unknown to `LINE`; markdown tiles normalize any mode to `UNSPECIFIED`.

A tile's `insight_query` JSONB stores only an `InsightQuerySpec` (insight_type, events, breakdowns, filters, conversion_window, include_step_timing) — **no** time_range or granularity. `Create`/`Update` full-replace the dashboard window + display fields; `Upsert` is the only tile-mutation RPC and full-replaces every per-tile column on every write (see CLAUDE.md "Dashboards" for the reconcile contract).

`DashboardsService.QueryDashboard` returns a single self-contained `RenderedDashboard`: dashboard metadata plus every tile in order, each `RenderedTile` embedding the full `DashboardTile` plus (for insight tiles) a `oneof outcome { result | error_message }`; markdown tiles carry no outcome. The effective `(time_range, granularity)` is resolved **once** — request override → dashboard default — then applied to every tile: each tile's `InsightQuerySpec` is assembled into a full `QueryRequest` with that window and re-validated with `protovalidate.Validate` (so the per-granularity range caps apply per tile). A cap violation or build/execution error surfaces as that tile's `error_message` without failing the whole RPC — **except** a request-level context cancellation/deadline, which is propagated so the errgroup cancels siblings and the handler maps it to `CodeCanceled`/`CodeDeadlineExceeded`, rather than masked as a per-tile failure in a 200 response. This is the *render* endpoint — structure + data together — unlike `Get` (structure only).

## Insights Filter Model

- Top-level insights filters are **group-based only**. In `shared.insights.v1`, use `filter_groups` and `filter_groups_operator` on `QueryRequest.spec` (the nested `InsightQuerySpec`) and on `SegmentUsersRequest`.
- Legacy top-level `filters` fields are removed from `proto/shared/insights/v1/insights.proto`. Tags are intentionally not reserved (pre-release, never shipped). Do not reintroduce them.
- Group semantics:
  - Within a group, conditions are combined using `FilterGroup.operator` (`AND` by default when unspecified).
  - Between groups, conditions are combined using `filter_groups_operator` (`AND` by default when unspecified).
- Per-event filters remain on `EventQuery.event.filters` and are independent of top-level filter groups.

## Retention Insight

- `shared.insights.v1.InsightType` supports `INSIGHT_TYPE_RETENTION`.
- Retention query semantics in `QueryRequest.spec.events`:
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

## Rollup Fast Path

`ExecuteQuery` serves eligible trends and segmentation queries from the pre-aggregated `dashboard_event_rollup_daily` rollup (see [clickhouse.md](clickhouse.md)) instead of scanning raw events. The decision is `canUseEventRollup` (in `internal/core/insights/rollup.go`):

- insight type TRENDS or SEGMENTATION;
- every event aggregation in `{TOTAL, UNIQUE_USERS, PER_USER_AVG}` (numeric-property aggs SUM/AVG/MIN/MAX need raw per-event values);
- at most one breakdown and, if present, on a materialized dimension (`materializedDims`);
- no filter groups and no per-event filters;
- non-empty event kind;
- DAY/WEEK/MONTH granularity.

The dispatchers `trendsQueryForExecution` / `segmentationQueryForExecution` pick `buildTrendsFromRollup` / `buildSegmentationFromRollup` when eligible, else the raw `BuildTrendsQuery` / `BuildSegmentationQuery`. The public builders therefore stay pure raw-events builders, and any query the predicate rejects (filtered, multi-dimension, sub-day, numeric-aggregation, custom-property breakdown, funnel/retention) runs unchanged on raw events.

No-breakdown trends and segmentation read the synthetic `$__total__` dimension row. The exclusive `[from, to)` window maps to inclusive whole-day bounds (the `day` of `to - 1ns`), and the time bucket reuses the raw `granularityFunc` over `toDateTime(day)` so week/month boundaries match raw exactly. Value expressions mirror the raw aggregations: `sum(cnt)` for TOTAL, `uniqMerge(uniq_state)` for UNIQUE_USERS, and their ratio for PER_USER_AVG.

**Accuracy caveat.** The bucket boundaries and aggregation expressions match raw exactly, but the *counts* can diverge: the rollup cannot dedup duplicate event deliveries (its key omits `event_id`, whereas the raw `events` `ReplacingMergeTree` collapses retries on merge), so rollup-served TOTAL and PER_USER_AVG over-count vs. the raw builders by the pipeline's redelivery rate (monotonic, never self-correcting). UNIQUE_USERS is immune. Accepted as a bounded inaccuracy for dashboard visualization — see [clickhouse.md](clickhouse.md).

## Session Insights

A session insight is expressed by setting `InsightQuerySpec.session` (a `SessionQuery`) instead of `events` — the two are mutually exclusive (CEL `session_no_events`). The existing `insight_type` still chooses the response shape: TRENDS → time series, SEGMENTATION → scalar. `SessionMetric` selects what is measured:

- `SESSIONS` — count of distinct sessions started in the window.
- `AVG_DURATION` — mean of (last event − first event) per session, in seconds.
- `BOUNCE_RATE` — percent of sessions with exactly one (scoped) event.
- `ENTRY` / `EXIT` — count of sessions bucketed by their **first** / **last** matching event's breakdown value (entry/exit page). These require TRENDS with **exactly one** breakdown (CEL `session_page_metrics_require_trends_breakdown`); the other three metrics accept either insight type and any breakdown count.

The optional `SessionQuery.scope` (a reused `common.v1.EventFilter`) restricts which events participate: an empty scope measures all events in the session; a `kind` and/or `filters` scope measures only matching events. A `kind` also labels the trends series; with no scope the synthetic label `$session` is used.

**Window semantics — keyed on session start, not clipped.** A session is measured over its **entire** set of (scoped) events and attributed to its start instant; the window selects sessions whose **start** is in `[from, to)` and never clips a session's events. A session straddling the lower boundary is counted in full on its start side and excluded entirely from the later window — never split, so duration / entry / exit / bounce stay well-defined. The raw builder enforces this by applying the window as a `HAVING` on the computed `start_time` (not a `WHERE` on `occur_time`); `buildSessionRowsCTE` in `builder.go`. The cost is that the raw fallback scan is not partition-pruned by `occur_time` — accepted because the rollup serves the common day-aligned case and correctness outranks the wider scan.

**Rollup fast path.** `canUseSessionRollup` (`session_rollup.go`) routes eligible session queries to the `dashboard_session_rollup` rollup (migration 007), under the same `rollupWindowAligned` day-alignment guard as the event rollup. Eligibility: trends or segmentation; DAY/WEEK/MONTH granularity; no filter groups, no per-event scope filters; at most one breakdown on a materialized session dimension (`sessionMaterializedDims`); and for ENTRY/EXIT, trends with exactly one breakdown. Anything rejected falls back to the raw builders with identical results. **Accuracy caveat:** only `BOUNCE_RATE` is affected by duplicate deliveries (a redelivered single-event session reads `event_count > 1` and stops counting as a bounce, so the rollup under-reports); SESSIONS/ENTRY/EXIT (session-id groups) and AVG_DURATION (idempotent min/max) are immune. Pinned by `TestIntegration/session_rollup_bounce_duplicate_overcount_documented`. → [clickhouse.md](clickhouse.md)
