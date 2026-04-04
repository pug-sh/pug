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
	case insightsv1.InsightType_INSIGHT_TYPE_FUNNEL:
		return buildFunnel(req, projectID)
	case insightsv1.InsightType_INSIGHT_TYPE_RETENTION:
		return buildRetention(req, projectID)
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

func buildFunnel(req *insightsv1.QueryRequest, projectID string) (string, []any, error) {
	steps := req.GetEvents()
	if len(steps) == 0 {
		return "", nil, fmt.Errorf("funnel requires at least one event step")
	}

	topLevelFilterCond, err := buildTopLevelFilterCondition(req.GetFilterGroups(), req.GetFilterGroupsOperator())
	if err != nil {
		return "", nil, err
	}

	stepQueries := make([]*chq.Query, 0, len(steps))
	for i, step := range steps {
		stepCond, err := buildFunnelStepCondition(step, i)
		if err != nil {
			return "", nil, err
		}

		fromTable := "events e"
		conds := []chq.Condition{
			chq.Eq("e.project_id", projectID),
			chq.Gte("e.occur_time", req.GetTimeRange().GetFrom().AsTime()),
			chq.Lt("e.occur_time", req.GetTimeRange().GetTo().AsTime()),
			chq.RawCond(stepCond.SQL(), stepCond.Args()...),
		}

		if !topLevelFilterCond.IsZero() {
			conds = append(conds, chq.RawCond(topLevelFilterCond.SQL(), topLevelFilterCond.Args()...))
		}

		if i > 0 {
			fromTable = fmt.Sprintf("events e INNER JOIN step_%d prev ON e.distinct_id = prev.distinct_id", i-1)
			conds = append(conds, chq.RawCond("e.occur_time >= prev.step_time"))
		}

		stepQuery := chq.NewQuery().
			Select("e.distinct_id", "min(e.occur_time) AS step_time").
			From(fromTable).
			Where(conds...).
			GroupBy("e.distinct_id")

		stepQueries = append(stepQueries, stepQuery)
	}

	unionQueries := make([]*chq.Query, 0, len(steps))
	for i := range steps {
		query := chq.NewQuery()
		for j := 0; j <= i; j++ {
			query.With(fmt.Sprintf("step_%d", j), stepQueries[j])
		}
		unionQueries = append(unionQueries, query.
			Select(
				fmt.Sprintf("CAST(%d AS Int64) AS step_index", i),
				fmt.Sprintf("%s AS event_kind", sqlStringLiteral(steps[i].GetEvent().GetKind())),
				"toFloat64(count(*)) AS value",
			).
			From(fmt.Sprintf("step_%d", i)))
	}

	return chq.UnionAll(unionQueries...).OrderBy("step_index ASC").Build()
}

func buildRetention(req *insightsv1.QueryRequest, projectID string) (string, []any, error) {
	events := req.GetEvents()
	if len(events) == 0 {
		return "", nil, fmt.Errorf("retention requires at least one event")
	}

	startEvent := events[0]
	returnEvent := startEvent
	if len(events) > 1 {
		returnEvent = events[1]
	}

	topLevelFilterCond, err := buildTopLevelFilterCondition(req.GetFilterGroups(), req.GetFilterGroupsOperator())
	if err != nil {
		return "", nil, err
	}

	startCond, err := buildEventCondition([]*insightsv1.EventQuery{startEvent})
	if err != nil {
		return "", nil, fmt.Errorf("retention start event: %w", err)
	}
	if startCond.IsZero() {
		return "", nil, fmt.Errorf("retention start event: empty event filter")
	}

	returnCond, err := buildEventCondition([]*insightsv1.EventQuery{returnEvent})
	if err != nil {
		return "", nil, fmt.Errorf("retention return event: %w", err)
	}
	if returnCond.IsZero() {
		return "", nil, fmt.Errorf("retention return event: empty event filter")
	}

	granFn := granularityFunc(req.GetGranularity())
	from := req.GetTimeRange().GetFrom().AsTime()
	to := req.GetTimeRange().GetTo().AsTime()

	cohorts := chq.NewQuery().
		Select("distinct_id", fmt.Sprintf("min(%s(occur_time)) AS cohort_time", granFn)).
		From("events").
		Where(
			chq.Eq("project_id", projectID),
			chq.Gte("occur_time", from),
			chq.Lt("occur_time", to),
			chq.RawCond(startCond.SQL(), startCond.Args()...),
			topLevelFilterCond,
		).
		GroupBy("distinct_id")

	cohortSizes := chq.NewQuery().
		Select("cohort_time", "toFloat64(count(*)) AS cohort_size").
		From("cohorts").
		GroupBy("cohort_time")

	retained := chq.NewQuery().
		Select(
			"c.cohort_time",
			fmt.Sprintf("%s(e.occur_time) AS t", granFn),
			"toFloat64(count(DISTINCT e.distinct_id)) AS retained_users",
		).
		From("cohorts c INNER JOIN events e ON e.distinct_id = c.distinct_id").
		Where(
			chq.Eq("e.project_id", projectID),
			chq.Gte("e.occur_time", from),
			chq.Lt("e.occur_time", to),
			chq.RawCond("e.occur_time >= c.cohort_time"),
			chq.RawCond(returnCond.SQL(), returnCond.Args()...),
			topLevelFilterCond,
		).
		GroupBy("c.cohort_time", "t")

	return chq.NewQuery().
		With("cohorts", cohorts).
		With("cohort_sizes", cohortSizes).
		With("retained", retained).
		Select(
			"r.cohort_time",
			"r.t",
			"if(cs.cohort_size = 0, 0, (r.retained_users * 100.0) / cs.cohort_size) AS value",
			"cs.cohort_size",
		).
		From("retained r INNER JOIN cohort_sizes cs ON r.cohort_time = cs.cohort_time").
		Where(chq.RawCond("r.t >= r.cohort_time")).
		OrderBy("r.cohort_time ASC", "r.t ASC").
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

func buildFunnelStepCondition(step *insightsv1.EventQuery, idx int) (chq.Condition, error) {
	if step == nil {
		return chq.Condition{}, fmt.Errorf("funnel step[%d]: event is required", idx)
	}
	cond, err := buildEventCondition([]*insightsv1.EventQuery{step})
	if err != nil {
		return chq.Condition{}, fmt.Errorf("funnel step[%d]: %w", idx, err)
	}
	if cond.IsZero() {
		return chq.Condition{}, fmt.Errorf("funnel step[%d]: empty event filter", idx)
	}
	return cond, nil
}

func sqlStringLiteral(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "\\'") + "'"
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
