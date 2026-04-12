package insights

import (
	"fmt"
	"strings"

	chq "github.com/fivebitsio/cotton/internal/core/clickhouse"
	commonv1 "github.com/fivebitsio/cotton/internal/gen/proto/common/v1"
	insightsv1 "github.com/fivebitsio/cotton/internal/gen/proto/shared/insights/v1"
)

const DefaultPageSize int32 = 100

// PropertyValuesLimit is the max distinct values returned by property-values queries.
// Also used as the cache exhaustion threshold in Service.GetPropertyValues.
const PropertyValuesLimit = 100

// Typed query structs link builder output to the correct executor method at compile time.

// TrendsQuery is the compiled SQL for a trends insight.
type TrendsQuery struct {
	sql           string
	args          []any
	numBreakdowns int
	properties    []string // breakdown property names for GroupSeries
}

func (q TrendsQuery) SQL() string          { return q.sql }
func (q TrendsQuery) Args() []any          { return q.args }
func (q TrendsQuery) NumBreakdowns() int   { return q.numBreakdowns }
func (q TrendsQuery) Properties() []string { return q.properties }

// ScalarQuery is the compiled SQL for a single-value query (segmentation).
type ScalarQuery struct {
	sql  string
	args []any
}

func (q ScalarQuery) SQL() string { return q.sql }
func (q ScalarQuery) Args() []any { return q.args }

// FunnelQuery is the compiled SQL for funnel step counts (no timing).
type FunnelQuery struct {
	sql  string
	args []any
}

func (q FunnelQuery) SQL() string { return q.sql }
func (q FunnelQuery) Args() []any { return q.args }

// FunnelTimingQuery is the compiled SQL for per-user funnel event arrays.
type FunnelTimingQuery struct {
	sql       string
	args      []any
	kinds     []string // step event kinds for ComputeFunnelTiming
	windowSec int64    // conversion window for ComputeFunnelTiming
}

func (q FunnelTimingQuery) SQL() string      { return q.sql }
func (q FunnelTimingQuery) Args() []any      { return q.args }
func (q FunnelTimingQuery) Kinds() []string  { return q.kinds }
func (q FunnelTimingQuery) WindowSec() int64 { return q.windowSec }

// RetentionQuery is the compiled SQL for retention cohort analysis.
type RetentionQuery struct {
	sql  string
	args []any
}

func (q RetentionQuery) SQL() string { return q.sql }
func (q RetentionQuery) Args() []any { return q.args }

// BuildTrendsQuery builds a trends insight query with breakdown metadata.
func BuildTrendsQuery(req *insightsv1.QueryRequest, projectID string) (TrendsQuery, error) {
	sql, args, err := buildTrends(req, projectID)
	if err != nil {
		return TrendsQuery{}, err
	}
	breakdowns := req.GetBreakdowns()
	properties := make([]string, len(breakdowns))
	for i, bk := range breakdowns {
		properties[i] = bk.GetProperty()
	}
	return TrendsQuery{sql: sql, args: args, numBreakdowns: len(breakdowns), properties: properties}, nil
}

// BuildSegmentationQuery builds a segmentation insight query.
func BuildSegmentationQuery(req *insightsv1.QueryRequest, projectID string) (ScalarQuery, error) {
	sql, args, err := buildSegmentation(req, projectID)
	if err != nil {
		return ScalarQuery{}, err
	}
	return ScalarQuery{sql: sql, args: args}, nil
}

// BuildFunnelCountsQuery builds a funnel step-counts query using windowFunnel().
func BuildFunnelCountsQuery(req *insightsv1.QueryRequest, projectID string) (FunnelQuery, error) {
	sql, args, err := buildFunnelWindowFunnel(req, projectID)
	if err != nil {
		return FunnelQuery{}, err
	}
	return FunnelQuery{sql: sql, args: args}, nil
}

// BuildFunnelTimingQuery builds a funnel query for per-user event arrays with timing metadata.
func BuildFunnelTimingQuery(req *insightsv1.QueryRequest, projectID string) (FunnelTimingQuery, error) {
	sql, args, err := buildFunnelWithTiming(req, projectID)
	if err != nil {
		return FunnelTimingQuery{}, err
	}
	steps := req.GetEvents()
	kinds := make([]string, len(steps))
	for i, s := range steps {
		kinds[i] = s.GetEvent().GetKind()
	}
	return FunnelTimingQuery{sql: sql, args: args, kinds: kinds, windowSec: EffectiveWindowSec(req)}, nil
}

// BuildRetentionQuery builds a retention cohort analysis query.
func BuildRetentionQuery(req *insightsv1.QueryRequest, projectID string) (RetentionQuery, error) {
	sql, args, err := buildRetention(req, projectID)
	if err != nil {
		return RetentionQuery{}, err
	}
	return RetentionQuery{sql: sql, args: args}, nil
}

// Deprecated: BuildQuery builds a ClickHouse SQL query and positional args from a QueryRequest.
// Use the type-specific builders (BuildTrendsQuery, BuildSegmentationQuery,
// BuildFunnelCountsQuery, BuildFunnelTimingQuery, BuildRetentionQuery) which provide
// compile-time safety between builder and executor.
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
			Event:       &commonv1.EventFilter{},
			Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL,
		}}
	}

	// Pre-build top-level filter condition — reused in CTE and every sub-query.
	topLevelFilterCond, err := buildTopLevelFilterCondition(req.GetFilterGroups(), req.GetFilterGroupsOperator(), projectID, "")
	if err != nil {
		return "", nil, fmt.Errorf("trends: %w", err)
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

		eventCond, err := buildEventCondition(events, projectID)
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
			cond, err := chq.PropertyCondition(f, projectID)
			if err != nil {
				return "", nil, fmt.Errorf("trends: events[%d]: %w", i, err)
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

	topLevelFilterCond, err := buildTopLevelFilterCondition(req.GetFilterGroups(), req.GetFilterGroupsOperator(), projectID, "")
	if err != nil {
		return "", nil, fmt.Errorf("segmentation: %w", err)
	}

	eventCond, err := buildEventCondition(req.GetEvents(), projectID)
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

// buildFunnel dispatches to either windowFunnel (fast counts) or single-scan array-based query (with step timing).
func buildFunnel(req *insightsv1.QueryRequest, projectID string) (string, []any, error) {
	if req.GetIncludeStepTiming() {
		return buildFunnelWithTiming(req, projectID)
	}
	return buildFunnelWindowFunnel(req, projectID)
}

// buildFunnelWindowFunnel generates a funnel counts query using ClickHouse's windowFunnel() aggregate.
// windowFunnel scans the events table once and returns the deepest step reached per user
// within the conversion window. The outer UNION ALL computes cumulative counts per step.
func buildFunnelWindowFunnel(req *insightsv1.QueryRequest, projectID string) (string, []any, error) {
	steps := req.GetEvents()
	if len(steps) == 0 {
		return "", nil, fmt.Errorf("funnel requires at least one event step")
	}

	topLevelFilterCond, err := buildTopLevelFilterCondition(req.GetFilterGroups(), req.GetFilterGroupsOperator(), projectID, "")
	if err != nil {
		return "", nil, fmt.Errorf("funnel: %w", err)
	}

	// Build step conditions for windowFunnel boolean expressions.
	stepExprs := make([]string, len(steps))
	var stepArgs []any
	for i, step := range steps {
		cond, err := buildFunnelStepCondition(step, projectID, i)
		if err != nil {
			return "", nil, err
		}
		stepExprs[i] = cond.SQL()
		stepArgs = append(stepArgs, cond.Args()...)
	}

	from := req.GetTimeRange().GetFrom().AsTime()
	to := req.GetTimeRange().GetTo().AsTime()
	windowSec := EffectiveWindowSec(req)

	windowFunnelExpr := fmt.Sprintf(
		"windowFunnel(%d)(toDateTime(occur_time), %s) AS level",
		windowSec, strings.Join(stepExprs, ", "),
	)

	// CTE: one row per user with their deepest funnel level.
	funnelCTE := chq.NewQuery().
		Select("distinct_id").
		SelectExpr(windowFunnelExpr, stepArgs...).
		From("events").
		Where(
			chq.Eq("project_id", projectID),
			chq.Gte("occur_time", from),
			chq.Lt("occur_time", to),
			topLevelFilterCond,
		).
		GroupBy("distinct_id")

	// Outer: one row per step. countIf(level >= N) gives users who reached at least step N.
	unionQueries := make([]*chq.Query, len(steps))
	for i, step := range steps {
		q := chq.NewQuery().
			Select(
				fmt.Sprintf("CAST(%d AS Int64) AS step_index", i),
			).
			SelectExpr("CAST(? AS String) AS event_kind", step.GetEvent().GetKind()).
			Select(
				fmt.Sprintf("toFloat64(countIf(level >= %d)) AS value", i+1),
				"toFloat64(0) AS avg_time_seconds",
			).
			From("funnel")
		if i == 0 {
			q.With("funnel", funnelCTE)
		}
		unionQueries[i] = q
	}

	return chq.UnionAll(unionQueries...).OrderBy("step_index ASC").Build()
}

// buildFunnelWithTiming generates a single-scan funnel query that returns per-user
// event arrays for Go-side step matching and timing computation.
//
// Strategy: tag each event with which step it matches (via multiIf), aggregate into
// per-user arrays, then Go walks the arrays to greedily match steps and compute intervals.
// Single table scan, like windowFunnel, but preserves timestamps.
//
// Limitation: multiIf short-circuits — if two steps share the same conditions (e.g., both
// match kind='page_view'), events always tag as the earlier step and the later step never
// matches. This is uncommon in practice (funnel steps are usually distinct events).
func buildFunnelWithTiming(req *insightsv1.QueryRequest, projectID string) (string, []any, error) {
	steps := req.GetEvents()
	if len(steps) == 0 {
		return "", nil, fmt.Errorf("funnel requires at least one event step")
	}

	topLevelFilterCond, err := buildTopLevelFilterCondition(req.GetFilterGroups(), req.GetFilterGroupsOperator(), projectID, "")
	if err != nil {
		return "", nil, fmt.Errorf("funnel: %w", err)
	}

	from := req.GetTimeRange().GetFrom().AsTime()
	to := req.GetTimeRange().GetTo().AsTime()

	// Build multiIf expression to tag each event with its step index (-1 = no match).
	// Also build OR condition for WHERE to pre-filter to relevant events only.
	var multiIfParts []string
	var multiIfArgs []any
	var orConds []chq.Condition
	for i, step := range steps {
		cond, err := buildFunnelStepCondition(step, projectID, i)
		if err != nil {
			return "", nil, err
		}
		multiIfParts = append(multiIfParts, fmt.Sprintf("%s, %d", cond.SQL(), i))
		multiIfArgs = append(multiIfArgs, cond.Args()...)
		orConds = append(orConds, chq.RawCond(cond.SQL(), cond.Args()...))
	}
	multiIfExpr := "multiIf(" + strings.Join(multiIfParts, ", ") + ", -1) AS step_match"

	// CTE: tag events with step index, pre-filtered to matching events.
	taggedCTE := chq.NewQuery().
		Select("distinct_id", "occur_time").
		SelectExpr(multiIfExpr, multiIfArgs...).
		From("events").
		Where(
			chq.Eq("project_id", projectID),
			chq.Gte("occur_time", from),
			chq.Lt("occur_time", to),
			chq.Or(orConds...),
			topLevelFilterCond,
		)

	// Main: aggregate per-user arrays sorted by time.
	// arraySort with a lambda sorts step_matches by the corresponding occur_time.
	// arrayZip pairs (time, step) tuples; arraySort orders by the first element;
	// arrayMap extracts the sorted components back into separate arrays.
	return chq.NewQuery().
		With("tagged", taggedCTE).
		Select(
			"distinct_id",
			"arraySort(groupArray(occur_time)) AS times",
			"arrayMap(x -> x.2, arraySort(x -> x.1, arrayZip(groupArray(occur_time), groupArray(toInt64(step_match))))) AS step_matches",
		).
		From("tagged").
		Where(chq.RawCond("step_match >= 0")).
		GroupBy("distinct_id").
		Build()
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

	topLevelFilterCond, err := buildTopLevelFilterCondition(req.GetFilterGroups(), req.GetFilterGroupsOperator(), projectID, "")
	if err != nil {
		return "", nil, fmt.Errorf("retention: %w", err)
	}

	startCond, err := buildEventCondition([]*insightsv1.EventQuery{startEvent}, projectID)
	if err != nil {
		return "", nil, fmt.Errorf("retention start event: %w", err)
	}
	if startCond.IsZero() {
		return "", nil, fmt.Errorf("retention start event: empty event filter")
	}

	// Build return event and top-level filter conditions with "e" alias for the JOINed CTE.
	returnCondAliased, err := buildEventConditionAliased([]*insightsv1.EventQuery{returnEvent}, projectID, "e")
	if err != nil {
		return "", nil, fmt.Errorf("retention return event: %w", err)
	}
	if returnCondAliased.IsZero() {
		return "", nil, fmt.Errorf("retention return event: empty event filter")
	}

	topLevelFilterCondAliased, err := buildTopLevelFilterCondition(req.GetFilterGroups(), req.GetFilterGroupsOperator(), projectID, "e")
	if err != nil {
		return "", nil, fmt.Errorf("retention: %w", err)
	}

	granFn := granularityFunc(req.GetGranularity())
	from := req.GetTimeRange().GetFrom().AsTime()
	to := req.GetTimeRange().GetTo().AsTime()

	// cohorts: assign each user to a time bucket based on their first start event.
	// first_event_time is the precise timestamp (not bucketed) — used to exclude
	// return events that fall within the same bucket but before the actual start.
	cohorts := chq.NewQuery().
		Select(
			"distinct_id",
			fmt.Sprintf("min(%s(occur_time)) AS cohort_time", granFn),
			"min(occur_time) AS first_event_time",
		).
		From("events").
		Where(
			chq.Eq("project_id", projectID),
			chq.Gte("occur_time", from),
			chq.Lt("occur_time", to),
			startCond,
			topLevelFilterCond,
		).
		GroupBy("distinct_id")

	cohortSizes := chq.NewQuery().
		Select("cohort_time", "toFloat64(count(*)) AS cohort_size").
		From("cohorts").
		GroupBy("cohort_time")

	// retained: count return events per cohort per time bucket.
	// Filter by first_event_time (not bucketed cohort_time) to avoid counting
	// return events that happened before the user's actual start event.
	// Conditions use "e." alias to avoid ambiguity in the JOIN.
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
			chq.RawCond("e.occur_time >= c.first_event_time"),
			returnCondAliased,
			topLevelFilterCondAliased,
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
// to clickhouse.EventConditionAliased for SQL generation.
// projectID is required to build profile property filter subqueries.
func buildEventCondition(events []*insightsv1.EventQuery, projectID string) (chq.Condition, error) {
	return buildEventConditionAliased(events, projectID, "")
}

// buildEventConditionAliased builds event conditions with column references prefixed by alias.
func buildEventConditionAliased(events []*insightsv1.EventQuery, projectID, alias string) (chq.Condition, error) {
	filters := make([]*commonv1.EventFilter, len(events))
	for i, ev := range events {
		filters[i] = ev.GetEvent()
	}
	return chq.EventConditionAliased(filters, projectID, alias)
}

func buildFunnelStepCondition(step *insightsv1.EventQuery, projectID string, idx int) (chq.Condition, error) {
	if step == nil {
		return chq.Condition{}, fmt.Errorf("funnel step[%d]: event is required", idx)
	}
	cond, err := buildEventCondition([]*insightsv1.EventQuery{step}, projectID)
	if err != nil {
		return chq.Condition{}, fmt.Errorf("funnel step[%d]: %w", idx, err)
	}
	if cond.IsZero() {
		return chq.Condition{}, fmt.Errorf("funnel step[%d]: empty event filter", idx)
	}
	return cond, nil
}

// BuildSegmentUsersQuery builds a ClickHouse SQL query and args from a SegmentUsersRequest.
// The generated query returns a paginated, cursor-keyed list of distinct user IDs.
func BuildSegmentUsersQuery(req *insightsv1.SegmentUsersRequest, projectID string) (string, []any, error) {
	topLevelFilterCond, err := buildTopLevelFilterCondition(req.GetFilterGroups(), req.GetFilterGroupsOperator(), projectID, "")
	if err != nil {
		return "", nil, fmt.Errorf("segment_users: %w", err)
	}

	eventCond, err := buildEventCondition(req.GetEvents(), projectID)
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

// BuildAutoPropertyValuesQuery returns a query for distinct auto-property values of a key over the last
// 30 days. Uses DISTINCT + LIMIT for early exit — no full aggregation, no insert overhead.
func BuildAutoPropertyValuesQuery(projectID, propertyKey, eventKind string) (string, []any, error) {
	return buildPropertyValuesQuery(projectID, propertyKey, "auto_properties", eventKind)
}

// BuildCustomPropertyValuesQuery returns a query for distinct custom property values.
func BuildCustomPropertyValuesQuery(projectID, propertyKey, eventKind string) (string, []any, error) {
	return buildPropertyValuesQuery(projectID, propertyKey, "custom_properties", eventKind)
}

// buildPropertyValuesQuery returns distinct values from mapCol for the given key over the last 30 days.
func buildPropertyValuesQuery(projectID, propertyKey, mapCol, eventKind string) (string, []any, error) {
	selectExpr := mapCol + `[?] AS value`
	propertyNotEmptyClause := mapCol + `[?] != ''`

	return chq.NewQuery().
		SelectExpr("DISTINCT "+selectExpr, propertyKey).
		From("events").
		Where(
			chq.Eq("project_id", projectID),
			chq.When(eventKind != "", chq.Eq("kind", eventKind)),
			chq.RawCond("occur_time >= now() - INTERVAL 30 DAY"),
			chq.RawCond(propertyNotEmptyClause, propertyKey),
		).
		Limit(int64(PropertyValuesLimit)).
		Build()
}

// buildTopLevelFilterCondition builds filter groups into a single Condition
// using And/Or composition. projectID is required for profile property filter subqueries.
// alias is an optional table alias prefix for column references (empty = no prefix).
func buildTopLevelFilterCondition(
	groups []*insightsv1.FilterGroup,
	groupsOp commonv1.LogicalOperator,
	projectID, alias string,
) (chq.Condition, error) {
	if len(groups) == 0 {
		return chq.Condition{}, nil
	}

	groupConds := make([]chq.Condition, 0, len(groups))
	for i, g := range groups {
		cond, err := buildSingleFilterGroupCondition(g, projectID, alias)
		if err != nil {
			return chq.Condition{}, fmt.Errorf("filter_groups[%d]: %w", i, err)
		}
		groupConds = append(groupConds, cond)
	}

	if groupsOp == commonv1.LogicalOperator_LOGICAL_OPERATOR_OR {
		return chq.Or(groupConds...), nil
	}
	return chq.And(groupConds...), nil
}

func buildSingleFilterGroupCondition(group *insightsv1.FilterGroup, projectID, alias string) (chq.Condition, error) {
	if len(group.GetFilters()) == 0 {
		return chq.Condition{}, fmt.Errorf("group must contain at least one filter")
	}

	conds := make([]chq.Condition, 0, len(group.GetFilters()))
	for j, f := range group.GetFilters() {
		cond, err := chq.PropertyConditionAliased(f, projectID, alias)
		if err != nil {
			return chq.Condition{}, fmt.Errorf("filters[%d]: %w", j, err)
		}
		conds = append(conds, cond)
	}

	if group.GetOperator() == commonv1.LogicalOperator_LOGICAL_OPERATOR_OR {
		return chq.Or(conds...), nil
	}
	return chq.And(conds...), nil
}

// BuildEventNamesQuery returns a query against event_names for event names with count and last_seen.
func BuildEventNamesQuery(projectID string) (string, []any, error) {
	return chq.NewQuery().
		Select("kind", "countMerge(event_count) AS count", "maxMerge(last_seen) AS last_seen").
		From("event_names").
		Where(chq.Eq("project_id", projectID)).
		GroupBy("kind").
		OrderBy("count DESC").
		Limit(1000).
		Build()
}

// BuildAutoPropertyKeysQuery returns a query against property_keys for auto_property keys,
// optionally scoped to a specific event kind.
func BuildAutoPropertyKeysQuery(projectID, eventKind string) (string, []any, error) {
	return buildPropertyKeysQuery(projectID, "auto", eventKind)
}

// BuildCustomPropertyKeysQuery returns a query against property_keys for custom_property keys,
// optionally scoped to a specific event kind.
func BuildCustomPropertyKeysQuery(projectID, eventKind string) (string, []any, error) {
	return buildPropertyKeysQuery(projectID, "custom", eventKind)
}

// BuildProfilePropertyKeysQuery returns a query against property_keys for profile property keys.
// Profile keys are project-wide (not scoped to an event kind).
func BuildProfilePropertyKeysQuery(projectID string) (string, []any, error) {
	return buildPropertyKeysQuery(projectID, "profile", "")
}

// BuildProfilePropertyValuesQuery returns distinct values for a profile property from ClickHouse.
// Profile properties are stored in a String column containing JSON and accessed via JSONExtractString.
//
// SAFETY: propertyKey is interpolated directly into SQL. Callers must ensure it is
// proto-validated (pattern ^\\$?[a-zA-Z0-9_.-]+$) before calling this function.
func BuildProfilePropertyValuesQuery(projectID, propertyKey string) (string, []any, error) {
	propExpr := chq.ProfilePropertyExpr(propertyKey)
	return chq.NewQuery().
		Select("DISTINCT "+propExpr+" AS value").
		From("profiles").
		Where(
			chq.Eq("project_id", projectID),
			chq.Eq("is_deleted", 0),
			chq.RawCond(propExpr+" != ''"),
		).
		Limit(int64(PropertyValuesLimit)).
		Build()
}

func buildPropertyKeysQuery(projectID, mapType, eventKind string) (string, []any, error) {
	return chq.NewQuery().
		Select("key", "countMerge(event_count) AS count", "maxMerge(last_seen) AS last_seen").
		From("property_keys").
		Where(
			chq.Eq("project_id", projectID),
			chq.Eq("map_type", mapType),
			chq.When(eventKind != "", chq.Eq("kind", eventKind)),
			chq.RawCond("key != ''"),
		).
		GroupBy("key").
		OrderBy("count DESC").
		Limit(500).
		Build()
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
