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

- [ ] Funnels — step-based conversion analysis
- [ ] Retention — cohort-based return analysis
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

- [ ] Support overlaying multiple event lines on one chart (currently uses first EventQuery only)
- [ ] Per-event filters (`EventQuery.filters`) — proto field exists but builder ignores it. Silently dropped today.
