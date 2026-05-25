package insights

import (
	"fmt"
	"slices"
	"time"

	chq "github.com/pug-sh/pug/internal/core/clickhouse"
	insightsv1 "github.com/pug-sh/pug/internal/gen/proto/shared/insights/v1"
)

// rollupTable is the daily dimensional rollup populated by
// dashboard_event_rollup_daily_mv (migration 006).
const rollupTable = "dashboard_event_rollup_daily"

// totalDimName is the synthetic dimension whose single ('') value per
// (project, day, kind) carries the no-breakdown / segmentation totals.
const totalDimName = "$__total__"

// materializedDims are the auto-property breakdown dimensions backed by the
// rollup. This MUST stay in sync with the ARRAY JOIN list in migration
// 006_create_dashboard_event_rollup.sql — TestMaterializedDimsMatchMigration
// enforces it.
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

// rollupBreakdownLimit mirrors buildTopValsCTE's default of top-10.
func rollupBreakdownLimit(limit int32) int64 {
	if limit == 0 {
		return 10
	}
	return int64(limit)
}

// buildTrendsFromRollup builds a trends query against the dimensional rollup,
// mirroring buildTrends' top_vals / $others structure. Caller must have checked
// canUseEventRollup. Returns the same TrendsQuery type as the raw path.
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
			OrderBy("sum(cnt) DESC").
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
		Select(aggExpr + " AS value").
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
