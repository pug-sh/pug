package insights

import (
	"fmt"
	"slices"
	"strconv"
	"time"

	chq "github.com/pug-sh/pug/internal/core/clickhouse"
	commonv1 "github.com/pug-sh/pug/internal/gen/proto/common/v1"
	insightsv1 "github.com/pug-sh/pug/internal/gen/proto/shared/insights/v1"
)

// rollupTable is the daily dimensional rollup populated by
// dashboard_event_rollup_daily_mv (migration 006).
const rollupTable = "dashboard_event_rollup_daily"

// totalDimName is the synthetic dimension whose single empty-string value per
// (project, day, kind) carries the no-breakdown / segmentation totals.
const totalDimName = "$__total__"

// materializedDims are the auto-property breakdown dimensions backed by the
// rollup. This MUST stay in sync with the ARRAY JOIN list in migration
// 006_create_dashboard_event_rollup.sql — TestMaterializedDimsMatchMigration
// checks dim names; TestMigration006PromotedDimExprsMatch checks value expressions.
var materializedDims = []string{
	"$country", "$region", "$city",
	"$os", "$browser", "$device", "$platform",
	"$utmSource", "$utmMedium", "$utmCampaign",
}

func isMaterializedDim(prop string) bool {
	return slices.Contains(materializedDims, prop)
}

// rollupAggExpr returns the rollup value expression for an aggregation, mirroring
// aggregationExpr's raw forms (count(*) / uniq(distinct_id) / per-user avg). ok is
// false for numeric property aggregations (SUM/AVG/MIN/MAX), which need raw
// per-event values the rollup does not store.
func rollupAggExpr(agg insightsv1.AggregationType) (string, bool) {
	switch agg {
	case insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL,
		insightsv1.AggregationType_AGGREGATION_TYPE_UNSPECIFIED:
		return "toFloat64(sum(cnt))", true
	case insightsv1.AggregationType_AGGREGATION_TYPE_UNIQUE_USERS:
		return "toFloat64(uniqMerge(uniq_state))", true
	case insightsv1.AggregationType_AGGREGATION_TYPE_PER_USER_AVG:
		return "if(uniqMerge(uniq_state) = 0, 0, toFloat64(sum(cnt)) / toFloat64(uniqMerge(uniq_state)))", true
	default: // SUM, AVG, MIN, MAX
		return "", false
	}
}

// canUseEventRollup reports whether a trends/segmentation query can be served from
// the dimensional rollup. Conservative by construction: anything it rejects falls
// back to the raw-events builders with identical results.
//
// Accepted accuracy caveat for the rollup-served path: TOTAL and PER_USER_AVG can
// over-count relative to the raw builders under duplicate event delivery. The
// events table is ReplacingMergeTree keyed on (project_id, toStartOfMinute(occur_time),
// kind, event_id) and collapses retries/redeliveries on merge; the incremental MV
// (migration 006) sums count() into a key WITHOUT event_id, so a duplicate insert
// is retained permanently. The drift equals the pipeline's redelivery rate
// (monotonic, never self-correcting). UNIQUE_USERS is immune
// (uniqState on distinct_id is idempotent). This is an accepted, bounded
// inaccuracy for dashboard visualization — see docs/architecture/clickhouse.md;
// pinned by TestIntegration/rollup_duplicate_overcount_documented.
func canUseEventRollup(spec *insightsv1.InsightQuerySpec, gran insightsv1.Granularity) bool {
	switch spec.GetInsightType() {
	case insightsv1.InsightType_INSIGHT_TYPE_TRENDS,
		insightsv1.InsightType_INSIGHT_TYPE_SEGMENTATION:
	default:
		return false
	}

	switch gran {
	case insightsv1.Granularity_GRANULARITY_DAY,
		insightsv1.Granularity_GRANULARITY_WEEK,
		insightsv1.Granularity_GRANULARITY_MONTH:
	default:
		return false
	}

	if len(spec.GetFilterGroups()) != 0 {
		return false
	}

	bds := spec.GetBreakdowns()
	if len(bds) > 1 {
		return false
	}
	if len(bds) == 1 && !isMaterializedDim(bds[0].GetProperty()) {
		return false
	}

	events := spec.GetEvents()
	if len(events) == 0 {
		return false
	}
	for _, ev := range events {
		if ev.GetEvent().GetKind() == "" {
			return false
		}
		if len(ev.GetEvent().GetFilters()) != 0 {
			return false
		}
		if _, ok := rollupAggExpr(ev.GetAggregation()); !ok {
			return false
		}
	}
	return true
}

// rollupDayBounds converts the request's [from, to) instant window to the
// inclusive whole-day bounds the rollup is keyed on. `to` is exclusive, so the
// last included day is the day of (to - 1ns). Formatted YYYY-MM-DD for comparison
// against the Date column.
func rollupDayBounds(req *insightsv1.QueryRequest) (string, string) {
	from := req.GetTimeRange().GetFrom().AsTime().UTC()
	toIncl := req.GetTimeRange().GetTo().AsTime().Add(-time.Nanosecond).UTC()
	const layout = "2006-01-02"
	return from.Format(layout), toIncl.Format(layout)
}

// startOfDayUTC truncates t to midnight UTC.
func startOfDayUTC(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

// rollupWindowAligned reports whether [from, to) maps onto whole rollup days with
// no partial-day truncation, so the day-keyed rollup returns the same event set as
// the raw instant filter (occur_time >= from AND occur_time < to).
//
// `from` must be midnight UTC — a mid-day `from` strands the events before it on the
// day the rollup would include in full. `to` is fine when it is midnight UTC, or
// when it is now/future: the rollup widens the final day to its end, but that
// trailing slice lies at/after `now` and holds no events. A past, mid-day `to` (e.g.
// a "same time on a prior day" comparison) strands real events on the excluded side,
// so it is rejected and the query falls back to the raw builders. Without this guard
// the rollup silently over-counts the partial boundary days. `now` is the
// request's reference time, threaded so a live preset's `to == now` is treated as
// aligned rather than rejected by sub-second skew.
func rollupWindowAligned(tr *commonv1.TimeRange, now time.Time) bool {
	from := tr.GetFrom().AsTime().UTC()
	to := tr.GetTo().AsTime().UTC()
	if !from.Equal(startOfDayUTC(from)) {
		return false
	}
	if to.Equal(startOfDayUTC(to)) {
		return true
	}
	return !to.Before(now)
}

// rollupBreakdownLimit mirrors the default top-N of 10 used by effectiveBreakdownLimit.
func rollupBreakdownLimit(limit int32) int64 {
	if limit == 0 {
		return 10
	}
	return int64(limit)
}

// buildTrendsFromRollup builds a trends query against the dimensional rollup.
// Breakdown top-N bucketing happens in SQL (top_vals CTE + $others); the returned
// TrendsQuery carries breakdownLimit=0 so GroupSeries does not re-bucket.
// Caller must have checked canUseEventRollup.
func buildTrendsFromRollup(req *insightsv1.QueryRequest, projectID string) (TrendsQuery, error) {
	granFn, err := granularityFunc(req.GetGranularity())
	if err != nil {
		return TrendsQuery{}, fmt.Errorf("trends rollup: %w", err)
	}
	// Bucket the Date column through the SAME granularity function the raw path
	// uses (over toDateTime(day)) so week/month boundaries are byte-identical.
	bucketExpr := fmt.Sprintf("%s(toDateTime(day))", granFn)

	spec := req.GetSpec()
	bds := spec.GetBreakdowns()
	events := spec.GetEvents()
	fromDay, toDay := rollupDayBounds(req)

	dimName := totalDimName
	if len(bds) == 1 {
		dimName = bds[0].GetProperty()
	}

	// Shared top_vals CTE over all event kinds (only when a breakdown is present).
	var topVals *chq.Query
	if len(bds) == 1 {
		kindConds := make([]chq.Condition, len(events))
		for i, ev := range events {
			kindConds[i] = chq.Eq("kind", ev.GetEvent().GetKind())
		}
		topVals = chq.NewQuery().
			Select("dim_value").
			From(rollupTable).
			Where(
				chq.Eq("project_id", projectID),
				chq.Eq("dim_name", dimName),
				chq.Gte("day", fromDay),
				chq.Lte("day", toDay),
				chq.Or(kindConds...),
			).
			GroupBy("dim_value").
			// Tie-break on dim_value so the top-N matches the raw Group*Series
			// top-N (total DESC, breakdown value ASC) and $others is deterministic.
			OrderBy("sum(cnt) DESC", "dim_value ASC").
			Limit(rollupBreakdownLimit(spec.GetBreakdownLimit()))
	}

	queries := make([]*chq.Query, 0, len(events))
	for i, ev := range events {
		aggExpr, ok := rollupAggExpr(ev.GetAggregation())
		if !ok {
			return TrendsQuery{}, fmt.Errorf("trends rollup: events[%d]: unsupported aggregation %s", i, ev.GetAggregation())
		}

		selectExprs := []string{
			bucketExpr + " AS t",
			"kind AS event_kind",
		}
		if len(bds) == 1 {
			selectExprs = append(selectExprs,
				"if(dim_value IN (SELECT dim_value FROM top_vals), dim_value, '$others') AS breakdown_0")
		}
		selectExprs = append(selectExprs, aggExpr+" AS value")

		q := chq.NewQuery().
			Select(selectExprs...).
			From(rollupTable).
			Where(
				chq.Eq("project_id", projectID),
				chq.Eq("dim_name", dimName),
				chq.Eq("kind", ev.GetEvent().GetKind()),
				chq.Gte("day", fromDay),
				chq.Lte("day", toDay),
			)

		groupBy := []string{"t", "event_kind"}
		if len(bds) == 1 {
			groupBy = append(groupBy, "breakdown_0")
		}
		q.GroupBy(groupBy...)

		if i == 0 && topVals != nil {
			q.With("top_vals", topVals)
		}
		queries = append(queries, q)
	}

	orderBy := []string{"t ASC", "event_kind ASC"}
	if len(bds) == 1 {
		orderBy = append(orderBy, "breakdown_0 ASC")
	}

	sql, args, err := chq.UnionAll(queries...).OrderBy(orderBy...).WithQueryCache(analyticsCacheTTL).Build()
	if err != nil {
		return TrendsQuery{}, fmt.Errorf("trends rollup: %w", err)
	}
	return TrendsQuery{sql: sql, args: args, properties: breakdownProps(bds)}, nil
}

// trendGridKey identifies a (time-bucket, breakdown-tuple) cell. UnixNano is
// Location-independent, so the int64 keys equal instants from any zone identically.
type trendGridKey struct {
	unixNano   int64
	breakdowns string
}

func newTrendGridKey(t time.Time, breakdowns []string) trendGridKey {
	return trendGridKey{
		unixNano:   t.UnixNano(),
		breakdowns: breakdownKey(breakdowns),
	}
}

func trendRowIdentityKey(kind string, gk trendGridKey) string {
	return kind + "\x00" + gk.breakdowns + "\x00" + strconv.FormatInt(gk.unixNano, 10)
}

// fillMultiEventTrendZeros adds zero-value rows for (time bucket, breakdown, event_kind)
// combinations absent from rollup output. Multi-event raw trends achieve the same via
// CROSS JOIN unpivot; rollup uses per-kind UNION ALL which omits empty cells.
//
// Breakdown slices observed on input rows are preserved verbatim on synthesized rows:
// breakdownKey(nil) == breakdownKey([]string{""}) == "", so reconstructing from the
// joined-string key would lose the arity needed by GroupSeries' length check against
// properties.
func fillMultiEventTrendZeros(rows []TrendRow, eventKinds []string) []TrendRow {
	if len(eventKinds) <= 1 || len(rows) == 0 {
		return rows
	}

	// grid maps each cell to the breakdown slice from the first row observed there.
	// The slice is cloned so synthesized rows don't alias caller storage.
	grid := make(map[trendGridKey][]string)
	existing := make(map[string]struct{}, len(rows))
	for _, r := range rows {
		gk := newTrendGridKey(r.Time, r.Breakdowns)
		if _, ok := grid[gk]; !ok {
			grid[gk] = append([]string(nil), r.Breakdowns...)
		}
		existing[trendRowIdentityKey(r.EventKind, gk)] = struct{}{}
	}

	out := append([]TrendRow(nil), rows...)
	for gk, bdVals := range grid {
		// time.Unix returns Local; force UTC so synthesized rows match the zone
		// of rollup-returned rows.
		t := time.Unix(0, gk.unixNano).UTC()
		for _, kind := range eventKinds {
			id := trendRowIdentityKey(kind, gk)
			if _, ok := existing[id]; ok {
				continue
			}
			out = append(out, TrendRow{Time: t, EventKind: kind, Breakdowns: bdVals, Value: 0})
			existing[id] = struct{}{}
		}
	}
	return out
}

// buildSegmentationFromRollup builds a scalar segmentation query against the
// rollup's $__total__ rows. Caller must have checked canUseEventRollup.
func buildSegmentationFromRollup(req *insightsv1.QueryRequest, projectID string) (ScalarQuery, error) {
	events := req.GetSpec().GetEvents()
	if len(events) == 0 {
		return ScalarQuery{}, fmt.Errorf("segmentation rollup: no events")
	}
	aggExpr, ok := rollupAggExpr(aggregationType(req))
	if !ok {
		return ScalarQuery{}, fmt.Errorf("segmentation rollup: unsupported aggregation %s", aggregationType(req))
	}
	fromDay, toDay := rollupDayBounds(req)

	kindConds := make([]chq.Condition, len(events))
	for i, ev := range events {
		kindConds[i] = chq.Eq("kind", ev.GetEvent().GetKind())
	}

	sql, args, err := chq.NewQuery().
		Select(aggExpr+" AS value").
		From(rollupTable).
		Where(
			chq.Eq("project_id", projectID),
			chq.Eq("dim_name", totalDimName),
			chq.Gte("day", fromDay),
			chq.Lte("day", toDay),
			chq.Or(kindConds...),
		).
		WithQueryCache(analyticsCacheTTL).
		Build()
	if err != nil {
		return ScalarQuery{}, fmt.Errorf("segmentation rollup: %w", err)
	}
	return ScalarQuery{sql: sql, args: args}, nil
}

// trendsQueryForExecution returns the rollup-backed trends query when the request
// is rollup-eligible (structurally per canUseEventRollup and window-wise per
// rollupWindowAligned), else the raw-events query. Keeps BuildTrendsQuery a pure raw
// builder while routing transparently at execution time. The returned bool reports
// whether the rollup builder was used, so the caller classifies a build failure
// correctly without re-evaluating eligibility.
func trendsQueryForExecution(req *insightsv1.QueryRequest, projectID string, now time.Time) (TrendsQuery, bool, error) {
	if req.GetSpec().GetSession() != nil {
		return sessionTrendsQueryForExecution(req, projectID, now)
	}
	if canUseEventRollup(req.GetSpec(), req.GetGranularity()) && rollupWindowAligned(req.GetTimeRange(), now) {
		q, err := buildTrendsFromRollup(req, projectID)
		return q, true, err
	}
	q, err := BuildTrendsQuery(req, projectID)
	return q, false, err
}

// segmentationQueryForExecution mirrors trendsQueryForExecution for segmentation.
func segmentationQueryForExecution(req *insightsv1.QueryRequest, projectID string, now time.Time) (ScalarQuery, bool, error) {
	if req.GetSpec().GetSession() != nil {
		return sessionSegmentationQueryForExecution(req, projectID, now)
	}
	if canUseEventRollup(req.GetSpec(), req.GetGranularity()) && rollupWindowAligned(req.GetTimeRange(), now) {
		q, err := buildSegmentationFromRollup(req, projectID)
		return q, true, err
	}
	q, err := BuildSegmentationQuery(req, projectID)
	return q, false, err
}

// canUseTopKRollup reports whether a top-K query can be served from the
// dimensional rollup: PROPERTY dimension on a materialized dim or EVENT_KIND
// dimension (the rollup's $__total__ rows carry per-kind totals), a
// rollup-expressible metric (TOTAL/UNIQUE_USERS/PER_USER_AVG — numeric
// property metrics need raw per-event values), no filter groups, and at most a
// kind-only scope (the rollup is keyed by kind; per-event property filters
// need raw events). The USER dimension always needs raw events (identity
// resolution + per-user values).
//
// Unlike canUseEventRollup, granularity is NOT consulted: top K has no time
// bucketing, so only the day-alignment window guard (rollupWindowAligned,
// checked by the dispatcher) decides whether the day-keyed rollup matches the
// raw instant filter.
//
// The duplicate-delivery over-count caveat documented on canUseEventRollup
// applies identically here, with the same shape-specific twist as ranking
// anywhere: near-tied dimensions can swap rank order. UNIQUE_USERS is immune.
func canUseTopKRollup(spec *insightsv1.InsightQuerySpec) bool {
	if spec.GetInsightType() != insightsv1.InsightType_INSIGHT_TYPE_TOP_K {
		return false
	}
	tk := spec.GetTopK()
	if tk == nil {
		return false
	}
	if len(spec.GetFilterGroups()) != 0 {
		return false
	}
	if len(tk.GetScope().GetFilters()) != 0 {
		return false
	}
	switch tk.GetDimension() {
	case insightsv1.TopKQuery_DIMENSION_EVENT_KIND:
	case insightsv1.TopKQuery_DIMENSION_PROPERTY:
		if !isMaterializedDim(tk.GetProperty()) {
			return false
		}
	default:
		return false
	}
	_, ok := rollupAggExpr(tk.GetMetric())
	return ok
}

// buildTopKFromRollup builds the top-K query against the dimensional rollup,
// preserving the raw shape's contract exactly: top_vals CTE with the same
// DESC/value-ASC tie-break, $others re-aggregation, is_others flag, and row
// ordering. Two passes over the rollup are fine — it is pre-aggregated and
// tiny relative to events. Caller must have checked canUseTopKRollup.
//
// WithSpillThreshold is deliberately omitted here (unlike the raw
// BuildTopKQuery): the rollup's GROUP BY cardinality is bounded by the
// materialized dimension's distinct values, not the unbounded raw event/user
// space, so it cannot blow the memory limit the way a raw scan can.
//
// Output aliases avoid the table's own dim_value column name (top_dim /
// dim_bucket) so ClickHouse alias substitution cannot turn
// `if(dim_value IN ..., dim_value, ...) AS dim_value` into a circular alias;
// the executor scans positionally, so result column names are free.
func buildTopKFromRollup(req *insightsv1.QueryRequest, projectID string) (TopKQuery, error) {
	tk := req.GetSpec().GetTopK()
	limit := int(tk.GetLimit())
	if limit == 0 {
		limit = defaultTopKLimit
	}

	aggExpr, ok := rollupAggExpr(tk.GetMetric())
	if !ok {
		return TopKQuery{}, fmt.Errorf("top k rollup: unsupported metric %s", tk.GetMetric())
	}

	var dimName, dimExpr string
	switch tk.GetDimension() {
	case insightsv1.TopKQuery_DIMENSION_PROPERTY:
		dimName, dimExpr = tk.GetProperty(), "dim_value"
	case insightsv1.TopKQuery_DIMENSION_EVENT_KIND:
		dimName, dimExpr = totalDimName, "kind"
	default:
		return TopKQuery{}, fmt.Errorf("top k rollup: unsupported dimension %s", tk.GetDimension())
	}

	fromDay, toDay := rollupDayBounds(req)
	scopeKind := tk.GetScope().GetKind()
	// The top_vals CTE and the outer re-aggregation scan the same rollup slice,
	// so they share one condition set.
	conds := []chq.Condition{
		chq.Eq("project_id", projectID),
		chq.Eq("dim_name", dimName),
		chq.Gte("day", fromDay),
		chq.Lte("day", toDay),
		chq.When(scopeKind != "", chq.Eq("kind", scopeKind)),
	}

	topVals := chq.NewQuery().
		Select(dimExpr+" AS top_dim").
		From(rollupTable).
		Where(conds...).
		GroupBy("top_dim").
		// Tie-break matches the raw builders: value DESC, dimension ASC.
		OrderBy(aggExpr+" DESC", "top_dim ASC").
		Limit(int64(limit))

	inTopVals := fmt.Sprintf("%s IN (SELECT top_dim FROM top_vals)", dimExpr)
	sql, args, err := chq.NewQuery().
		With("top_vals", topVals).
		Select(
			fmt.Sprintf("if(%s, %s, '%s') AS dim_bucket", inTopVals, dimExpr, topKOthersValue),
			fmt.Sprintf("if(%s, 0, 1) AS is_others", inTopVals),
			aggExpr+" AS value",
		).
		From(rollupTable).
		Where(conds...).
		GroupBy("dim_bucket", "is_others").
		OrderBy("is_others ASC", "value DESC", "dim_bucket ASC").
		WithQueryCache(analyticsCacheTTL).
		Build()
	if err != nil {
		return TopKQuery{}, fmt.Errorf("top k rollup: %w", err)
	}
	return TopKQuery{
		sql:       sql,
		args:      args,
		limit:     limit,
		dimension: tk.GetDimension(),
	}, nil
}

// topKQueryForExecution mirrors trendsQueryForExecution for top K: the
// rollup-backed query when structurally eligible (canUseTopKRollup) and
// window-aligned, else the raw-events BuildTopKQuery.
func topKQueryForExecution(req *insightsv1.QueryRequest, projectID string, now time.Time) (TopKQuery, bool, error) {
	if canUseTopKRollup(req.GetSpec()) && rollupWindowAligned(req.GetTimeRange(), now) {
		q, err := buildTopKFromRollup(req, projectID)
		return q, true, err
	}
	q, err := BuildTopKQuery(req, projectID)
	return q, false, err
}
