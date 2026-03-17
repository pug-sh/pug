package insights

import (
	"fmt"
	"regexp"
	"strings"

	insightsv1 "github.com/fivebitsio/cotton/internal/gen/proto/insights/v1"
)

const DefaultPageSize int32 = 100

var validPropertyName = regexp.MustCompile(`^[a-zA-Z0-9_.\-]+$`)

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
	aggExpr := aggregationExpr(aggregationType(req))
	breakdowns := req.GetBreakdowns()

	var sb strings.Builder
	var args []any

	// Build WHERE clause args (used in both CTE and main query when breakdowns present).
	var whereArgs []any
	whereArgs = append(whereArgs, projectID, req.GetTimeRange().GetFrom().AsTime(), req.GetTimeRange().GetTo().AsTime())
	hasKind := len(req.GetEvents()) > 0 && req.GetEvents()[0].GetKind() != ""
	if hasKind {
		whereArgs = append(whereArgs, req.GetEvents()[0].GetKind())
	}

	// Build extra filter clauses (shared by CTE and main query).
	type filterFrag struct {
		clause string
		fArgs  []any
	}
	var filterFrags []filterFrag
	for _, f := range req.GetFilters() {
		clause, fArgs, err := filterClause(f)
		if err != nil {
			return "", nil, err
		}
		filterFrags = append(filterFrags, filterFrag{clause, fArgs})
		whereArgs = append(whereArgs, fArgs...)
	}

	// writeWhere writes the WHERE block (without filter args accumulation — those are in whereArgs).
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
			expr, err := propertyExpr(bd.GetProperty())
			if err != nil {
				return "", nil, err
			}
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
		expr, err := propertyExpr(bd.GetProperty())
		if err != nil {
			return "", nil, err
		}
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
		clause, filterArgs, err := filterClause(f)
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

	// Optional kind filter
	if len(req.GetEvents()) > 0 && req.GetEvents()[0].GetKind() != "" {
		sb.WriteString("AND kind = ?\n")
		args = append(args, req.GetEvents()[0].GetKind())
	}

	// Top-level filters
	for _, f := range req.GetFilters() {
		clause, filterArgs, err := filterClause(f)
		if err != nil {
			return "", nil, err
		}
		sb.WriteString("AND ")
		sb.WriteString(clause)
		sb.WriteString("\n")
		args = append(args, filterArgs...)
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
		return "count(DISTINCT distinct_id)"
	case insightsv1.AggregationType_AGGREGATION_TYPE_PER_USER_AVG:
		return "if(count(DISTINCT distinct_id) = 0, 0, toFloat64(count(*)) / toFloat64(count(DISTINCT distinct_id)))"
	default: // TOTAL and UNSPECIFIED
		return "count(*)"
	}
}

// propertyExpr returns the ClickHouse expression to resolve a property.
// It checks auto_properties first; if the value is empty or missing, it falls back to custom_properties.
// Returns error if the property name contains invalid characters.
func propertyExpr(name string) (string, error) {
	if !validPropertyName.MatchString(name) {
		return "", fmt.Errorf("invalid property name: %q", name)
	}
	return fmt.Sprintf("ifNull(nullIf(auto_properties['%s'], ''), custom_properties['%s'])", name, name), nil
}

// escapeLike escapes ClickHouse LIKE metacharacters in a value.
func escapeLike(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `%`, `\%`)
	s = strings.ReplaceAll(s, `_`, `\_`)
	return s
}

// filterClause builds a single WHERE condition fragment for a PropertyFilter.
func filterClause(f *insightsv1.PropertyFilter) (string, []any, error) {
	prop, err := propertyExpr(f.GetProperty())
	if err != nil {
		return "", nil, err
	}

	switch f.GetOperator() {
	case insightsv1.FilterOperator_FILTER_OPERATOR_EQUALS:
		return fmt.Sprintf("%s = ?", prop), []any{f.GetValue()}, nil
	case insightsv1.FilterOperator_FILTER_OPERATOR_NOT_EQUALS:
		return fmt.Sprintf("%s != ?", prop), []any{f.GetValue()}, nil
	case insightsv1.FilterOperator_FILTER_OPERATOR_CONTAINS:
		return fmt.Sprintf("%s LIKE ?", prop), []any{"%" + escapeLike(f.GetValue()) + "%"}, nil
	case insightsv1.FilterOperator_FILTER_OPERATOR_NOT_CONTAINS:
		return fmt.Sprintf("%s NOT LIKE ?", prop), []any{"%" + escapeLike(f.GetValue()) + "%"}, nil
	case insightsv1.FilterOperator_FILTER_OPERATOR_IS_SET:
		return fmt.Sprintf("%s != ''", prop), nil, nil
	case insightsv1.FilterOperator_FILTER_OPERATOR_IS_NOT_SET:
		return fmt.Sprintf("%s = ''", prop), nil, nil
	default:
		return "", nil, fmt.Errorf("unsupported filter operator: %v", f.GetOperator())
	}
}
