package insights

import (
	"fmt"
	"strings"

	chfilters "github.com/fivebitsio/cotton/internal/core/clickhouse"
	commonv1 "github.com/fivebitsio/cotton/internal/gen/proto/common/v1"
	insightsv1 "github.com/fivebitsio/cotton/internal/gen/proto/shared/insights/v1"
)

const DefaultPageSize int32 = 100

// BuildQuery builds a ClickHouse SQL query and positional args from a QueryRequest.
func BuildQuery(req *insightsv1.QueryRequest, projectID string) (string, []any, error) {
	switch req.GetInsightType() {
	case insightsv1.InsightType_INSIGHT_TYPE_TRENDS:
		return buildTrends(req, projectID)
	case insightsv1.InsightType_INSIGHT_TYPE_SEGMENTATION:
		return buildSegmentation(req, projectID)
	default:
		return "", nil, fmt.Errorf("unsupported insight type: %v", req.GetInsightType())
	}
}

func buildTrends(req *insightsv1.QueryRequest, projectID string) (string, []any, error) {
	granFn := granularityFunc(req.GetGranularity())
	breakdowns := req.GetBreakdowns()
	events := req.GetEvents()

	// Normalize: empty events → single unfiltered event with default aggregation.
	if len(events) == 0 {
		events = []*insightsv1.EventQuery{{
			Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL,
		}}
	}

	// Pre-build top-level filter fragments — reused in CTE and every sub-query.
	type filterFrag struct {
		clause string
		fArgs  []any
	}
	var filterFrags []filterFrag
	for _, f := range req.GetFilters() {
		clause, fArgs, err := chfilters.FilterClause(f)
		if err != nil {
			return "", nil, err
		}
		filterFrags = append(filterFrags, filterFrag{clause, fArgs})
	}

	var sb strings.Builder
	var args []any

	// CTE for top-N breakdown values (shared across all event sub-queries).
	if len(breakdowns) > 0 {
		limit := req.GetBreakdownLimit()
		if limit == 0 {
			limit = 10
		}

		sb.WriteString("WITH top_vals AS (\nSELECT ")
		for i, bd := range breakdowns {
			if i > 0 {
				sb.WriteString(", ")
			}
			expr := chfilters.PropertyExpr(bd.GetProperty())
			fmt.Fprintf(&sb, "%s AS breakdown_%d", expr, i)
		}
		sb.WriteString("\nFROM events\nWHERE project_id = ?\nAND occur_time >= ?\nAND occur_time < ?\n")
		args = append(args, projectID, req.GetTimeRange().GetFrom().AsTime(), req.GetTimeRange().GetTo().AsTime())
		for _, ff := range filterFrags {
			sb.WriteString("AND ")
			sb.WriteString(ff.clause)
			sb.WriteString("\n")
			args = append(args, ff.fArgs...)
		}
		if err := writeEventCondition(&sb, &args, events); err != nil {
			return "", nil, err
		}
		sb.WriteString("GROUP BY ")
		for i := range breakdowns {
			if i > 0 {
				sb.WriteString(", ")
			}
			fmt.Fprintf(&sb, "breakdown_%d", i)
		}
		sb.WriteString("\nORDER BY count(*) DESC\nLIMIT ?\n)\n")
		args = append(args, limit)
	}

	// Build one sub-query per event, joined with UNION ALL.
	for i, ev := range events {
		if i > 0 {
			sb.WriteString("\nUNION ALL\n")
		}

		agg := ev.GetAggregation()
		if agg == insightsv1.AggregationType_AGGREGATION_TYPE_UNSPECIFIED {
			agg = insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL
		}

		// SELECT
		fmt.Fprintf(&sb, "SELECT %s(occur_time) AS t,\nkind AS event_kind", granFn)
		for j, bd := range breakdowns {
			expr := chfilters.PropertyExpr(bd.GetProperty())
			fmt.Fprintf(&sb, ",\nif(%s IN (SELECT breakdown_%d FROM top_vals), %s, '$others') AS breakdown_%d",
				expr, j, expr, j)
		}
		fmt.Fprintf(&sb, ",\n%s AS value\nFROM events\n", aggregationExpr(agg))

		// WHERE
		sb.WriteString("WHERE project_id = ?\nAND occur_time >= ?\nAND occur_time < ?\n")
		args = append(args, projectID, req.GetTimeRange().GetFrom().AsTime(), req.GetTimeRange().GetTo().AsTime())
		if ev.GetEvent().GetKind() != "" {
			sb.WriteString("AND kind = ?\n")
			args = append(args, ev.GetEvent().GetKind())
		}
		for _, ff := range filterFrags {
			sb.WriteString("AND ")
			sb.WriteString(ff.clause)
			sb.WriteString("\n")
			args = append(args, ff.fArgs...)
		}
		for _, f := range ev.GetEvent().GetFilters() {
			clause, fArgs, err := chfilters.FilterClause(f)
			if err != nil {
				return "", nil, err
			}
			sb.WriteString("AND ")
			sb.WriteString(clause)
			sb.WriteString("\n")
			args = append(args, fArgs...)
		}

		// GROUP BY
		sb.WriteString("GROUP BY t, event_kind")
		for j := range breakdowns {
			fmt.Fprintf(&sb, ", breakdown_%d", j)
		}
		sb.WriteString("\n")
	}

	// ORDER BY (applies to the full UNION ALL result).
	sb.WriteString("ORDER BY t ASC, event_kind ASC")
	for j := range breakdowns {
		fmt.Fprintf(&sb, ", breakdown_%d ASC", j)
	}

	return sb.String(), args, nil
}

func buildSegmentation(req *insightsv1.QueryRequest, projectID string) (string, []any, error) {
	aggExpr := aggregationExpr(aggregationType(req))

	var sb strings.Builder
	var args []any

	// SELECT clause
	fmt.Fprintf(&sb, "SELECT %s AS value\nFROM events\n", aggExpr)

	// WHERE clause
	sb.WriteString("WHERE project_id = ?\nAND occur_time >= ?\nAND occur_time < ?\n")
	args = append(args, projectID, req.GetTimeRange().GetFrom().AsTime(), req.GetTimeRange().GetTo().AsTime())

	// Top-level filters
	for _, f := range req.GetFilters() {
		clause, filterArgs, err := chfilters.FilterClause(f)
		if err != nil {
			return "", nil, err
		}
		sb.WriteString("AND ")
		sb.WriteString(clause)
		sb.WriteString("\n")
		args = append(args, filterArgs...)
	}

	// Event condition (kind + per-event filters, OR-joined for multiple events)
	if err := writeEventCondition(&sb, &args, req.GetEvents()); err != nil {
		return "", nil, err
	}

	return sb.String(), args, nil
}

// writeEventCondition extracts EventFilter from each EventQuery and delegates
// to chfilters.WriteEventFilterCondition for the actual SQL generation.
func writeEventCondition(sb *strings.Builder, args *[]any, events []*insightsv1.EventQuery) error {
	filters := make([]*commonv1.EventFilter, len(events))
	for i, ev := range events {
		filters[i] = ev.GetEvent()
	}
	return chfilters.WriteEventFilterCondition(sb, args, filters)
}

// BuildSegmentUsersQuery builds a ClickHouse SQL query and args from a SegmentUsersRequest.
// The generated query returns a paginated, cursor-keyed list of distinct user IDs.
func BuildSegmentUsersQuery(req *insightsv1.SegmentUsersRequest, projectID string) (string, []any, error) {
	var sb strings.Builder
	var args []any

	// SELECT clause
	sb.WriteString("SELECT DISTINCT distinct_id\nFROM events\n")

	// WHERE clause
	sb.WriteString("WHERE project_id = ?\nAND occur_time >= ?\nAND occur_time < ?\n")
	args = append(args, projectID, req.GetTimeRange().GetFrom().AsTime(), req.GetTimeRange().GetTo().AsTime())

	// Top-level filters
	for _, f := range req.GetFilters() {
		clause, filterArgs, err := chfilters.FilterClause(f)
		if err != nil {
			return "", nil, err
		}
		sb.WriteString("AND ")
		sb.WriteString(clause)
		sb.WriteString("\n")
		args = append(args, filterArgs...)
	}

	// Event condition (kind + per-event filters, OR-joined for multiple events)
	if err := writeEventCondition(&sb, &args, req.GetEvents()); err != nil {
		return "", nil, err
	}

	// Cursor pagination
	if req.GetPageToken() != "" {
		sb.WriteString("AND distinct_id > ?\n")
		args = append(args, req.GetPageToken())
	}

	// ORDER BY
	sb.WriteString("ORDER BY distinct_id ASC\n")

	// LIMIT
	pageSize := req.GetPageSize()
	if pageSize == 0 {
		pageSize = DefaultPageSize
	}
	sb.WriteString("LIMIT ?")
	args = append(args, pageSize)

	return sb.String(), args, nil
}

// BuildPropertyValuesQuery returns a query for distinct values of a property key over the last
// 30 days. Uses DISTINCT + LIMIT for early exit — no full aggregation, no insert overhead.
func BuildAutoPropertyValuesQuery(projectID, propertyKey, eventKind string) (string, []any) {
	return buildPropertyValuesQuery(projectID, propertyKey, "auto_properties", eventKind)
}

// BuildCustomPropertyValuesQuery returns a query for distinct custom property values.
func BuildCustomPropertyValuesQuery(projectID, propertyKey, eventKind string) (string, []any) {
	return buildPropertyValuesQuery(projectID, propertyKey, "custom_properties", eventKind)
}

func buildPropertyValuesQuery(projectID, propertyKey, mapCol, eventKind string) (string, []any) {
	if eventKind != "" {
		sql := `SELECT DISTINCT ` + mapCol + `[?] AS value
FROM events
WHERE project_id = ?
AND kind = ?
AND occur_time >= now() - INTERVAL 30 DAY
AND ` + mapCol + `[?] != ''
LIMIT 10`
		return sql, []any{propertyKey, projectID, eventKind, propertyKey}
	}
	sql := `SELECT DISTINCT ` + mapCol + `[?] AS value
FROM events
WHERE project_id = ?
AND occur_time >= now() - INTERVAL 30 DAY
AND ` + mapCol + `[?] != ''
LIMIT 10`
	return sql, []any{propertyKey, projectID, propertyKey}
}

// BuildEventNamesQuery returns a query against event_names for event names with count and last_seen.
func BuildEventNamesQuery(projectID string) (string, []any) {
	sql := `SELECT kind, countMerge(event_count) AS count, maxMerge(last_seen) AS last_seen
FROM event_names
WHERE project_id = ?
GROUP BY kind
ORDER BY count DESC
LIMIT 1000`
	return sql, []any{projectID}
}

// BuildAutoPropertyKeysQuery returns a query against property_keys for auto_property keys,
// optionally scoped to a specific event kind.
func BuildAutoPropertyKeysQuery(projectID, eventKind string) (string, []any) {
	return buildPropertyKeysQuery(projectID, "auto", eventKind)
}

// BuildCustomPropertyKeysQuery returns a query against property_keys for custom_property keys,
// optionally scoped to a specific event kind.
func BuildCustomPropertyKeysQuery(projectID, eventKind string) (string, []any) {
	return buildPropertyKeysQuery(projectID, "custom", eventKind)
}

func buildPropertyKeysQuery(projectID, mapType, eventKind string) (string, []any) {
	if eventKind != "" {
		sql := `SELECT key, countMerge(event_count) AS count, maxMerge(last_seen) AS last_seen
FROM property_keys
WHERE project_id = ?
AND map_type = ?
AND kind = ?
GROUP BY key
ORDER BY count DESC
LIMIT 500`
		return sql, []any{projectID, mapType, eventKind}
	}
	sql := `SELECT key, countMerge(event_count) AS count, maxMerge(last_seen) AS last_seen
FROM property_keys
WHERE project_id = ?
AND map_type = ?
GROUP BY key
ORDER BY count DESC
LIMIT 500`
	return sql, []any{projectID, mapType}
}

// granularityFunc returns the ClickHouse time-bucketing function name for the given granularity.
func granularityFunc(g insightsv1.Granularity) string {
	switch g {
	case insightsv1.Granularity_GRANULARITY_HOUR:
		return "toStartOfHour"
	case insightsv1.Granularity_GRANULARITY_WEEK:
		return "toStartOfWeek"
	case insightsv1.Granularity_GRANULARITY_MONTH:
		return "toStartOfMonth"
	default: // DAY and UNSPECIFIED both default to day
		return "toStartOfDay"
	}
}

// aggregationType returns the AggregationType for the request, preferring the first event's type.
func aggregationType(req *insightsv1.QueryRequest) insightsv1.AggregationType {
	if len(req.GetEvents()) > 0 {
		agg := req.GetEvents()[0].GetAggregation()
		if agg != insightsv1.AggregationType_AGGREGATION_TYPE_UNSPECIFIED {
			return agg
		}
	}
	return insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL
}

// aggregationExpr returns the SQL aggregation expression for the given type.
func aggregationExpr(agg insightsv1.AggregationType) string {
	switch agg {
	case insightsv1.AggregationType_AGGREGATION_TYPE_UNIQUE_USERS:
		return "toFloat64(count(DISTINCT distinct_id))"
	case insightsv1.AggregationType_AGGREGATION_TYPE_PER_USER_AVG:
		return "if(count(DISTINCT distinct_id) = 0, 0, toFloat64(count(*)) / toFloat64(count(DISTINCT distinct_id)))"
	default: // TOTAL and UNSPECIFIED
		return "toFloat64(count(*))"
	}
}
