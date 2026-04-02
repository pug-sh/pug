package insights

import (
	"fmt"
	"strings"

	chfilters "github.com/fivebitsio/cotton/internal/core/clickhouse"
	insightsv1 "github.com/fivebitsio/cotton/internal/gen/proto/dashboard/insights/v1"
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

	// Pre-build top-level filter fragments — reused across both paths below.
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

	// Multi-event trends without breakdowns: UNION ALL, one sub-query per event.
	if len(events) > 1 && len(breakdowns) == 0 {
		var sb strings.Builder
		var args []any
		for i, ev := range events {
			if i > 0 {
				sb.WriteString("\nUNION ALL\n")
			}
			agg := ev.GetAggregation()
			if agg == insightsv1.AggregationType_AGGREGATION_TYPE_UNSPECIFIED {
				agg = insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL
			}
			fmt.Fprintf(&sb, "SELECT %s(occur_time) AS t,\nkind AS event_kind,\n%s AS value\nFROM events\n",
				granFn, aggregationExpr(agg))
			sb.WriteString("WHERE project_id = ?\nAND occur_time >= ?\nAND occur_time < ?\n")
			args = append(args, projectID, req.GetTimeRange().GetFrom().AsTime(), req.GetTimeRange().GetTo().AsTime())
			if ev.GetKind() != "" {
				sb.WriteString("AND kind = ?\n")
				args = append(args, ev.GetKind())
			}
			for _, ff := range filterFrags {
				sb.WriteString("AND ")
				sb.WriteString(ff.clause)
				sb.WriteString("\n")
				args = append(args, ff.fArgs...)
			}
			for _, f := range ev.GetFilters() {
				clause, fArgs, err := chfilters.FilterClause(f)
				if err != nil {
					return "", nil, err
				}
				sb.WriteString("AND ")
				sb.WriteString(clause)
				sb.WriteString("\n")
				args = append(args, fArgs...)
			}
			sb.WriteString("GROUP BY t, event_kind\n")
		}
		sb.WriteString("ORDER BY t ASC, event_kind ASC")
		return sb.String(), args, nil
	}

	// Single-event (or breakdown) path.
	aggExpr := aggregationExpr(aggregationType(req))

	var sb strings.Builder
	var args []any

	// Build WHERE clause args (used in both CTE and main query when breakdowns present).
	var whereArgs []any
	whereArgs = append(whereArgs, projectID, req.GetTimeRange().GetFrom().AsTime(), req.GetTimeRange().GetTo().AsTime())
	hasKind := len(events) > 0 && events[0].GetKind() != ""
	if hasKind {
		whereArgs = append(whereArgs, events[0].GetKind())
	}
	for _, ff := range filterFrags {
		whereArgs = append(whereArgs, ff.fArgs...)
	}

	// writeWhere writes the WHERE block (args already accumulated in whereArgs).
	writeWhere := func(w *strings.Builder) {
		w.WriteString("WHERE project_id = ?\nAND occur_time >= ?\nAND occur_time < ?\n")
		if hasKind {
			w.WriteString("AND kind = ?\n")
		}
		for _, ff := range filterFrags {
			w.WriteString("AND ")
			w.WriteString(ff.clause)
			w.WriteString("\n")
		}
	}

	// CTE for top-N breakdown values.
	if len(breakdowns) > 0 {
		limit := req.GetBreakdownLimit()
		if limit == 0 {
			limit = 10
		}

		sb.WriteString("WITH top_vals AS (\n")
		sb.WriteString("SELECT ")
		for i, bd := range breakdowns {
			if i > 0 {
				sb.WriteString(", ")
			}
			expr := chfilters.PropertyExpr(bd.GetProperty())
			fmt.Fprintf(&sb, "%s AS breakdown_%d", expr, i)
		}
		sb.WriteString("\nFROM events\n")
		writeWhere(&sb)
		// GROUP BY breakdown columns in CTE
		sb.WriteString("GROUP BY ")
		for i := range breakdowns {
			if i > 0 {
				sb.WriteString(", ")
			}
			fmt.Fprintf(&sb, "breakdown_%d", i)
		}
		sb.WriteString("\n")
		sb.WriteString("ORDER BY count(*) DESC\n")
		sb.WriteString("LIMIT ?\n")
		sb.WriteString(")\n")

		// CTE args: WHERE args + limit
		args = append(args, whereArgs...)
		args = append(args, limit)
	}

	// Main SELECT clause
	sb.WriteString("SELECT ")
	fmt.Fprintf(&sb, "%s(occur_time) AS t", granFn)
	for i, bd := range breakdowns {
		expr := chfilters.PropertyExpr(bd.GetProperty())
		fmt.Fprintf(&sb, ",\nif(%s IN (SELECT breakdown_%d FROM top_vals), %s, '$others') AS breakdown_%d",
			expr, i, expr, i)
	}
	fmt.Fprintf(&sb, ",\n%s AS value\n", aggExpr)
	sb.WriteString("FROM events\n")

	// WHERE clause (main query)
	writeWhere(&sb)

	// Main query args
	args = append(args, whereArgs...)

	// GROUP BY
	sb.WriteString("GROUP BY t")
	for i := range breakdowns {
		fmt.Fprintf(&sb, ", breakdown_%d", i)
	}
	sb.WriteString("\n")

	// ORDER BY
	sb.WriteString("ORDER BY t ASC")
	for i := range breakdowns {
		fmt.Fprintf(&sb, ", breakdown_%d ASC", i)
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

	// Optional kind filter
	if len(req.GetEvents()) > 0 && req.GetEvents()[0].GetKind() != "" {
		sb.WriteString("AND kind = ?\n")
		args = append(args, req.GetEvents()[0].GetKind())
	}

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

	return sb.String(), args, nil
}

// writeEventCondition appends event kind and per-event filter conditions to sb/args.
// Single event: AND kind = ? [AND per-event filters...].
// Multiple events: AND ((kind=? AND ...) OR (kind=? AND ...) ...).
func writeEventCondition(sb *strings.Builder, args *[]any, events []*insightsv1.EventQuery) error {
	if len(events) == 0 {
		return nil
	}
	if len(events) == 1 {
		ev := events[0]
		if ev.GetKind() != "" {
			sb.WriteString("AND kind = ?\n")
			*args = append(*args, ev.GetKind())
		}
		for _, f := range ev.GetFilters() {
			clause, fArgs, err := chfilters.FilterClause(f)
			if err != nil {
				return err
			}
			sb.WriteString("AND ")
			sb.WriteString(clause)
			sb.WriteString("\n")
			*args = append(*args, fArgs...)
		}
		return nil
	}
	// Multiple events: OR-join each event's conditions.
	sb.WriteString("AND (\n")
	for i, ev := range events {
		if i > 0 {
			sb.WriteString("OR ")
		}
		sb.WriteString("(")
		var parts []string
		var evArgs []any
		if ev.GetKind() != "" {
			parts = append(parts, "kind = ?")
			evArgs = append(evArgs, ev.GetKind())
		}
		for _, f := range ev.GetFilters() {
			clause, fArgs, err := chfilters.FilterClause(f)
			if err != nil {
				return err
			}
			parts = append(parts, clause)
			evArgs = append(evArgs, fArgs...)
		}
		if len(parts) > 0 {
			sb.WriteString(strings.Join(parts, " AND "))
		} else {
			sb.WriteString("1=1")
		}
		sb.WriteString(")\n")
		*args = append(*args, evArgs...)
	}
	sb.WriteString(")\n")
	return nil
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
func BuildPropertyValuesQuery(projectID, propertyKey, mapCol, eventKind string) (string, []any) {
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

// BuildEventNamesQuery returns a query against event_names_mv for event names with count and last_seen.
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
