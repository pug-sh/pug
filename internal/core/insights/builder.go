package insights

import (
	"fmt"
	"strings"

	chq "github.com/fivebitsio/cotton/internal/core/clickhouse"
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

	// Pre-build top-level filter condition — reused in CTE and every sub-query.
	topLevelFilterCond, err := buildTopLevelFilterCondition(req.GetFilterGroups(), req.GetFilterGroupsOperator())
	if err != nil {
		return "", nil, err
	}

	var topValsCTE *chq.Query

	// CTE for top-N breakdown values (shared across all event sub-queries).
	if len(breakdowns) > 0 {
		limit := req.GetBreakdownLimit()
		if limit == 0 {
			limit = 10
		}

		selectExprs := make([]string, 0, len(breakdowns))
		groupByCols := make([]string, 0, len(breakdowns))
		for i, bd := range breakdowns {
			expr := chq.PropertyExpr(bd.GetProperty())
			selectExprs = append(selectExprs, fmt.Sprintf("%s AS breakdown_%d", expr, i))
			groupByCols = append(groupByCols, fmt.Sprintf("breakdown_%d", i))
		}

		eventCond, err := buildEventCondition(events)
		if err != nil {
			return "", nil, err
		}

		topValsCTE = chq.NewQuery().
			Select(selectExprs...).
			From("events").
			Where(
				chq.Eq("project_id", projectID),
				chq.Gte("occur_time", req.GetTimeRange().GetFrom().AsTime()),
				chq.Lt("occur_time", req.GetTimeRange().GetTo().AsTime()),
				topLevelFilterCond,
				eventCond,
			).
			GroupBy(groupByCols...).
			OrderBy("count(*) DESC").
			Limit(int64(limit))
	}

	queries := make([]*chq.Query, 0, len(events))
	// Build one sub-query per event, joined with UNION ALL.
	for i, ev := range events {
		agg := ev.GetAggregation()
		if agg == insightsv1.AggregationType_AGGREGATION_TYPE_UNSPECIFIED {
			agg = insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL
		}

		selectExprs := []string{
			fmt.Sprintf("%s(occur_time) AS t", granFn),
			"kind AS event_kind",
		}
		for j, bd := range breakdowns {
			expr := chq.PropertyExpr(bd.GetProperty())
			selectExprs = append(selectExprs,
				fmt.Sprintf("if(%s IN (SELECT breakdown_%d FROM top_vals), %s, '$others') AS breakdown_%d", expr, j, expr, j))
		}

		query := chq.NewQuery().
			Select(append(selectExprs, aggregationExpr(agg)+" AS value")...).
			From("events").
			Where(
				chq.Eq("project_id", projectID),
				chq.Gte("occur_time", req.GetTimeRange().GetFrom().AsTime()),
				chq.Lt("occur_time", req.GetTimeRange().GetTo().AsTime()),
				chq.When(ev.GetEvent().GetKind() != "", chq.Eq("kind", ev.GetEvent().GetKind())),
				topLevelFilterCond,
			)

		for _, f := range ev.GetEvent().GetFilters() {
			cond, err := chq.PropertyCondition(f)
			if err != nil {
				return "", nil, err
			}
			query.Where(cond)
		}

		groupByCols := []string{"t", "event_kind"}
		for j := range breakdowns {
			groupByCols = append(groupByCols, fmt.Sprintf("breakdown_%d", j))
		}
		query.GroupBy(groupByCols...)

		if i == 0 && topValsCTE != nil {
			query.With("top_vals", topValsCTE)
		}
		queries = append(queries, query)
	}

	orderBy := []string{"t ASC", "event_kind ASC"}
	for j := range breakdowns {
		orderBy = append(orderBy, fmt.Sprintf("breakdown_%d ASC", j))
	}

	return chq.UnionAll(queries...).OrderBy(orderBy...).Build()
}

func buildSegmentation(req *insightsv1.QueryRequest, projectID string) (string, []any, error) {
	aggExpr := aggregationExpr(aggregationType(req))

	topLevelFilterCond, err := buildTopLevelFilterCondition(req.GetFilterGroups(), req.GetFilterGroupsOperator())
	if err != nil {
		return "", nil, err
	}

	eventCond, err := buildEventCondition(req.GetEvents())
	if err != nil {
		return "", nil, err
	}

	return chq.NewQuery().
		Select(aggExpr+" AS value").
		From("events").
		Where(
			chq.Eq("project_id", projectID),
			chq.Gte("occur_time", req.GetTimeRange().GetFrom().AsTime()),
			chq.Lt("occur_time", req.GetTimeRange().GetTo().AsTime()),
			topLevelFilterCond,
			eventCond,
		).
		Build()
}

// buildEventCondition extracts EventFilter from each EventQuery and delegates
// to clickhouse.EventCondition for SQL generation.
func buildEventCondition(events []*insightsv1.EventQuery) (chq.Condition, error) {
	filters := make([]*commonv1.EventFilter, len(events))
	for i, ev := range events {
		filters[i] = ev.GetEvent()
	}
	return chq.EventCondition(filters)
}

// BuildSegmentUsersQuery builds a ClickHouse SQL query and args from a SegmentUsersRequest.
// The generated query returns a paginated, cursor-keyed list of distinct user IDs.
func BuildSegmentUsersQuery(req *insightsv1.SegmentUsersRequest, projectID string) (string, []any, error) {
	topLevelFilterCond, err := buildTopLevelFilterCondition(req.GetFilterGroups(), req.GetFilterGroupsOperator())
	if err != nil {
		return "", nil, err
	}

	eventCond, err := buildEventCondition(req.GetEvents())
	if err != nil {
		return "", nil, err
	}

	pageSize := req.GetPageSize()
	if pageSize == 0 {
		pageSize = DefaultPageSize
	}

	return chq.NewQuery().
		Select("DISTINCT distinct_id").
		From("events").
		Where(
			chq.Eq("project_id", projectID),
			chq.Gte("occur_time", req.GetTimeRange().GetFrom().AsTime()),
			chq.Lt("occur_time", req.GetTimeRange().GetTo().AsTime()),
			topLevelFilterCond,
			eventCond,
			chq.When(req.GetPageToken() != "", chq.Gt("distinct_id", req.GetPageToken())),
		).
		OrderBy("distinct_id ASC").
		Limit(int64(pageSize)).
		Build()
}

// BuildPropertyValuesQuery returns a query for distinct values of a property key over the last
// 30 days. Uses DISTINCT + LIMIT for early exit — no full aggregation, no insert overhead.
func BuildAutoPropertyValuesQuery(projectID, propertyKey, eventKind string) (string, []any, error) {
	return buildPropertyValuesQuery(projectID, propertyKey, "auto_properties", eventKind)
}

// BuildCustomPropertyValuesQuery returns a query for distinct custom property values.
func BuildCustomPropertyValuesQuery(projectID, propertyKey, eventKind string) (string, []any, error) {
	return buildPropertyValuesQuery(projectID, propertyKey, "custom_properties", eventKind)
}

func buildPropertyValuesQuery(projectID, propertyKey, mapCol, eventKind string) (string, []any, error) {
	selectExpr := mapCol + `[?] AS value`
	propertyNotEmptyClause := mapCol + `[?] != ''`

	q := chq.NewQuery().
		Select("DISTINCT "+selectExpr).
		From("events").
		Where(
			chq.Eq("project_id", projectID),
			chq.When(eventKind != "", chq.Eq("kind", eventKind)),
			chq.RawCond("occur_time >= now() - INTERVAL 30 DAY"),
			chq.RawCond(propertyNotEmptyClause, propertyKey),
		).
		Limit(10)

	sql, args, err := q.Build()
	if err != nil {
		return "", nil, fmt.Errorf("build property values query: %w", err)
	}
	return sql, append([]any{propertyKey}, args...), nil
}

func buildTopLevelFilterCondition(
	groups []*insightsv1.FilterGroup,
	groupsOp insightsv1.LogicalOperator,
) (chq.Condition, error) {
	if len(groups) == 0 {
		return chq.Condition{}, nil
	}

	groupClause, groupArgs, err := buildFilterGroupsClause(groups, groupsOp)
	if err != nil {
		return chq.Condition{}, err
	}
	return chq.RawCond(groupClause, groupArgs...), nil
}

func buildFilterGroupsClause(groups []*insightsv1.FilterGroup, groupsOp insightsv1.LogicalOperator) (string, []any, error) {
	joinGroups := logicalJoin(groupsOp)

	groupClauses := make([]string, 0, len(groups))
	var args []any
	for i, g := range groups {
		clause, gArgs, err := buildSingleFilterGroupClause(g)
		if err != nil {
			return "", nil, fmt.Errorf("filter_groups[%d]: %w", i, err)
		}
		groupClauses = append(groupClauses, clause)
		args = append(args, gArgs...)
	}

	return "(" + strings.Join(groupClauses, " "+joinGroups+" ") + ")", args, nil
}

func buildSingleFilterGroupClause(group *insightsv1.FilterGroup) (string, []any, error) {
	if len(group.GetFilters()) == 0 {
		return "", nil, fmt.Errorf("group must contain at least one filter")
	}

	joinFilters := logicalJoin(group.GetOperator())
	parts := make([]string, 0, len(group.GetFilters()))
	var args []any

	for j, f := range group.GetFilters() {
		clause, fArgs, err := chq.FilterClause(f)
		if err != nil {
			return "", nil, fmt.Errorf("filters[%d]: %w", j, err)
		}
		parts = append(parts, clause)
		args = append(args, fArgs...)
	}

	return "(" + strings.Join(parts, " "+joinFilters+" ") + ")", args, nil
}

func logicalJoin(op insightsv1.LogicalOperator) string {
	switch op {
	case insightsv1.LogicalOperator_LOGICAL_OPERATOR_OR:
		return "OR"
	default:
		return "AND"
	}
}

// BuildEventNamesQuery returns a query against event_names for event names with count and last_seen.
func BuildEventNamesQuery(projectID string) (string, []any) {
	sql, args, err := chq.NewQuery().
		Select("kind", "countMerge(event_count) AS count", "maxMerge(last_seen) AS last_seen").
		From("event_names").
		Where(chq.Eq("project_id", projectID)).
		GroupBy("kind").
		OrderBy("count DESC").
		Limit(1000).
		Build()
	if err != nil {
		panic(fmt.Sprintf("build event names query: %v", err))
	}
	return sql, args
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
	sql, args, err := chq.NewQuery().
		Select("key", "countMerge(event_count) AS count", "maxMerge(last_seen) AS last_seen").
		From("property_keys").
		Where(
			chq.Eq("project_id", projectID),
			chq.Eq("map_type", mapType),
			chq.When(eventKind != "", chq.Eq("kind", eventKind)),
		).
		GroupBy("key").
		OrderBy("count DESC").
		Limit(500).
		Build()
	if err != nil {
		panic(fmt.Sprintf("build property keys query: %v", err))
	}
	return sql, args
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
