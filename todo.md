# Insights Query Engine — Deferred Work

## Done

- [x] Proto definition (InsightsService with Query + SegmentUsers)
- [x] Query builder — trends + segmentation with filters
- [x] Breakdown support — top-N + $others bucketing, multiple breakdowns
- [x] SegmentUsers query builder with cursor pagination
- [x] ClickHouse executor (trends, trends with breakdowns, scalar, distinct IDs)
- [x] Server deps — ClickHouse reader connection
- [x] RPC handler wired with JWT dashboard auth
- [x] Integration test with testcontainers (trends, breakdowns, segmentation, filters, segment users)

## Aggregation Types

- [ ] Property sum/avg/min/max — requires `toFloat64OrNull()` casts for numeric properties stored as `Map(String, String)`

## Insight Types

- [x] Funnels — windowFunnel() for counts, array-based single-scan for timing
- [x] Retention — cohort-based return analysis
- [ ] Funnel timing statistics — median, p95, distribution (just change Go aggregation in `ComputeFunnelTiming`)
- [ ] User paths/flows — exploratory sequence analysis

## Performance

- [ ] Minute granularity — currently limited to hour+ to avoid expensive queries
- [ ] Precomputed segment cache — for faster campaign delivery
- [ ] Query result caching — cache hot queries with TTL

## Segmentation

- [ ] Behavioral segments — "did X but NOT Y"
- [ ] Event frequency conditions — "did X more than N times"

## Proto Cleanup

- [ ] `Series.label` — user-editable series name. Requires saved dashboards/widgets persistence to store it.

## Multiple Event Queries

- [x] Support overlaying multiple event lines on one chart (UNION ALL with per-event aggregation)
- [x] Per-event filters (`EventQuery.event.filters`) — handled via `EventCondition`

## Error Handling

- [ ] Standardized error codes/subcodes — currently all internal errors return a generic `{"code":"internal","message":"internal error"}` with no machine-readable subcode. Consider a structured error model (e.g., `code` + `subcode` + `detail`) so the client can distinguish between ClickHouse query failures, invalid filter expressions, timeout errors, etc. and surface actionable feedback in the UI.

## Code Quality

- [ ] Log errors at source — executor `Query*` methods wrap errors with `fmt.Errorf` but don't log; handlers log downstream. Move logging into executor, remove duplicate logging from handlers.
- [ ] Worker test coverage — `identify`, `register`, `upsert` workers have zero tests. Requires extracting interfaces for `profiles.Worker` dependencies (`dbread.Queries`, `dbwrite.Queries`, `pgxpool.Pool`) to enable mocking.
- [x] Typed query results — `BuildTrendsQuery`, `BuildFunnelCountsQuery`, etc. return typed structs with compile-time safety between builder and executor.
