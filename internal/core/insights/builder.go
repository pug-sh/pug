package insights

import (
	"fmt"
	"strings"
	"time"

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
	sql        string
	args       []any
	properties []string // breakdown property names for GroupSeries
}

func (q TrendsQuery) SQL() string          { return q.sql }
func (q TrendsQuery) Args() []any          { return q.args }
func (q TrendsQuery) NumBreakdowns() int   { return len(q.properties) }
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
	sql        string
	args       []any
	properties []string
}

func (q FunnelQuery) SQL() string          { return q.sql }
func (q FunnelQuery) Args() []any          { return q.args }
func (q FunnelQuery) NumBreakdowns() int   { return len(q.properties) }
func (q FunnelQuery) Properties() []string { return q.properties }

// FunnelTimingQuery is the compiled SQL for per-user funnel event arrays.
type FunnelTimingQuery struct {
	sql        string
	args       []any
	kinds      []string // step event kinds for ComputeFunnelTiming
	windowSec  int64    // conversion window for ComputeFunnelTiming
	properties []string
}

func (q FunnelTimingQuery) SQL() string          { return q.sql }
func (q FunnelTimingQuery) Args() []any          { return q.args }
func (q FunnelTimingQuery) Kinds() []string      { return q.kinds }
func (q FunnelTimingQuery) WindowSec() int64     { return q.windowSec }
func (q FunnelTimingQuery) NumBreakdowns() int   { return len(q.properties) }
func (q FunnelTimingQuery) Properties() []string { return q.properties }

// RetentionQuery is the compiled SQL for retention cohort analysis.
type RetentionQuery struct {
	sql        string
	args       []any
	properties []string
}

func (q RetentionQuery) SQL() string          { return q.sql }
func (q RetentionQuery) Args() []any          { return q.args }
func (q RetentionQuery) NumBreakdowns() int   { return len(q.properties) }
func (q RetentionQuery) Properties() []string { return q.properties }

// BuildTrendsQuery builds a trends insight query with breakdown metadata.
func BuildTrendsQuery(req *insightsv1.QueryRequest, projectID string) (TrendsQuery, error) {
	sql, args, err := buildTrends(req, projectID)
	if err != nil {
		return TrendsQuery{}, err
	}
	return TrendsQuery{sql: sql, args: args, properties: breakdownProps(req.GetBreakdowns())}, nil
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
	return FunnelQuery{sql: sql, args: args, properties: breakdownProps(req.GetBreakdowns())}, nil
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
	return FunnelTimingQuery{
		sql:        sql,
		args:       args,
		kinds:      kinds,
		windowSec:  EffectiveWindowSec(req),
		properties: breakdownProps(req.GetBreakdowns()),
	}, nil
}

// BuildRetentionQuery builds a retention cohort analysis query.
func BuildRetentionQuery(req *insightsv1.QueryRequest, projectID string) (RetentionQuery, error) {
	sql, args, err := buildRetention(req, projectID)
	if err != nil {
		return RetentionQuery{}, err
	}
	return RetentionQuery{sql: sql, args: args, properties: breakdownProps(req.GetBreakdowns())}, nil
}

// breakdownProps extracts the property key from each breakdown descriptor.
func breakdownProps(bds []*insightsv1.Breakdown) []string {
	if len(bds) == 0 {
		return nil
	}
	props := make([]string, len(bds))
	for i, bd := range bds {
		props[i] = bd.GetProperty()
	}
	return props
}

// buildTopValsCTE builds a CTE that selects the top-N breakdown value combinations
// by event count over the given time range. Returns nil when there are no breakdowns.
// Defaults to top-10 when limit is zero.
func buildTopValsCTE(breakdowns []*insightsv1.Breakdown, projectID string, from, to time.Time, limit int32, extraConds ...chq.Condition) *chq.Query {
	if len(breakdowns) == 0 {
		return nil
	}
	if limit == 0 {
		limit = 10
	}
	selectExprs := make([]string, len(breakdowns))
	groupByCols := make([]string, len(breakdowns))
	for i, bd := range breakdowns {
		expr := chq.PropertyExpr(bd.GetProperty())
		selectExprs[i] = fmt.Sprintf("%s AS breakdown_%d", expr, i)
		groupByCols[i] = fmt.Sprintf("breakdown_%d", i)
	}
	conds := append([]chq.Condition{
		chq.Eq("project_id", projectID),
		chq.Gte("occur_time", from),
		chq.Lt("occur_time", to),
	}, extraConds...)
	return chq.NewQuery().
		Select(selectExprs...).
		From("events").
		Where(conds...).
		GroupBy(groupByCols...).
		OrderBy("count(*) DESC").
		Limit(int64(limit))
}

// rawArgMinExpr returns an aggregate SELECT expression that computes first-touch attribution
// into a named column (raw_bd_N). Use this in the aggregation CTE so argMin is evaluated once.
func rawArgMinExpr(colExpr string, i int) string {
	return fmt.Sprintf("argMin(%s, occur_time) AS raw_bd_%d", colExpr, i)
}

// bucketRawExpr returns a scalar expression that buckets the pre-computed raw_bd_N column
// against the top_vals CTE, falling back to '$others'. Must be called after rawArgMinExpr
// has materialized raw_bd_N in a CTE row.
func bucketRawExpr(i int) string {
	return fmt.Sprintf(
		"if(raw_bd_%d IN (SELECT breakdown_%d FROM top_vals), raw_bd_%d, '$others') AS breakdown_%d",
		i, i, i, i,
	)
}

func buildTrends(req *insightsv1.QueryRequest, projectID string) (string, []any, error) {
	granFn := granularityFunc(req.GetGranularity())
	breakdowns := req.GetBreakdowns()
	events := req.GetEvents()

	// Normalize: empty events → single unfiltered event with default aggregation.
	if len(events) == 0 {
		events = []*insightsv1.EventQuery{{
			Event:       &commonv1.EventFilter{},
			Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL.Enum(),
		}}
	}

	// Pre-build top-level filter condition — reused in CTE and every sub-query.
	topLevelFilterCond, err := buildTopLevelFilterCondition(req.GetFilterGroups(), req.GetFilterGroupsOperator(), projectID, "")
	if err != nil {
		return "", nil, fmt.Errorf("trends: %w", err)
	}

	// CTE for top-N breakdown values (shared across all event sub-queries).
	var topValsCTE *chq.Query
	if len(breakdowns) > 0 {
		eventCond, err := buildEventCondition(events, projectID)
		if err != nil {
			return "", nil, err
		}
		topValsCTE = buildTopValsCTE(breakdowns, projectID,
			req.GetTimeRange().GetFrom().AsTime(), req.GetTimeRange().GetTo().AsTime(),
			req.GetBreakdownLimit(), topLevelFilterCond, eventCond)
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

		aggExpr, err := aggregationExpr(agg, ev.GetAggregationProperty())
		if err != nil {
			return "", nil, fmt.Errorf("trends: events[%d]: %w", i, err)
		}

		query := chq.NewQuery().
			Select(append(selectExprs, aggExpr+" AS value")...).
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
	firstEvent := func() *insightsv1.EventQuery {
		if len(req.GetEvents()) > 0 {
			return req.GetEvents()[0]
		}
		return nil
	}()
	var aggProp string
	if firstEvent != nil {
		aggProp = firstEvent.GetAggregationProperty()
	}
	aggExpr, err := aggregationExpr(aggregationType(req), aggProp)
	if err != nil {
		return "", nil, fmt.Errorf("segmentation: %w", err)
	}

	topLevelFilterCond, err := buildTopLevelFilterCondition(req.GetFilterGroups(), req.GetFilterGroupsOperator(), projectID, "")
	if err != nil {
		return "", nil, fmt.Errorf("segmentation: %w", err)
	}

	eventCond, err := buildEventCondition(req.GetEvents(), projectID)
	if err != nil {
		return "", nil, fmt.Errorf("segmentation: %w", err)
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

// buildFunnelWindowFunnel generates a funnel counts query using ClickHouse's windowFunnel() aggregate.
// windowFunnel scans the events table once and returns the deepest step reached per user
// within the conversion window. The outer UNION ALL computes cumulative counts per step.
//
// Breakdown attribution: when breakdowns are requested, each user is assigned a breakdown
// value from their earliest step-matching event in the time range (first-touch attribution). windowFunnel
// then runs over the step-filtered events, and results are grouped by breakdown value.
func buildFunnelWindowFunnel(req *insightsv1.QueryRequest, projectID string) (string, []any, error) {
	steps := req.GetEvents()
	breakdowns := req.GetBreakdowns()

	topLevelFilterCond, err := buildTopLevelFilterCondition(req.GetFilterGroups(), req.GetFilterGroupsOperator(), projectID, "")
	if err != nil {
		return "", nil, fmt.Errorf("funnel: %w", err)
	}

	// Build step conditions for windowFunnel boolean expressions and OR filter.
	// The OR filter scopes both top_vals and the funnel CTE to step-matching events,
	// ensuring argMin attribution and top-N bucketing are consistent with the timing path.
	stepExprs := make([]string, len(steps))
	var stepArgs []any
	orConds := make([]chq.Condition, len(steps))
	for i, step := range steps {
		cond, err := buildFunnelStepCondition(step, projectID, i)
		if err != nil {
			return "", nil, err
		}
		stepExprs[i] = cond.SQL()
		stepArgs = append(stepArgs, cond.Args()...)
		orConds[i] = chq.RawCond(cond.SQL(), cond.Args()...)
	}
	stepFilter := chq.Or(orConds...)

	from := req.GetTimeRange().GetFrom().AsTime()
	to := req.GetTimeRange().GetTo().AsTime()
	windowSec := EffectiveWindowSec(req)

	windowFunnelExpr := fmt.Sprintf(
		"windowFunnel(%d)(toDateTime(occur_time), %s) AS level",
		windowSec, strings.Join(stepExprs, ", "),
	)

	topValsCTE := buildTopValsCTE(breakdowns, projectID, from, to, req.GetBreakdownLimit(), stepFilter, topLevelFilterCond)

	// CTE: one row per user with their deepest funnel level.
	// Pre-filtered to step-matching events so argMin attribution considers only
	// events relevant to the funnel (first-touch from step events, not all events).
	// windowFunnel is unaffected — non-matching events contribute nothing to it.
	funnelCTE := chq.NewQuery().Select("distinct_id")
	for i, bd := range breakdowns {
		funnelCTE.Select(rawArgMinExpr(chq.PropertyExpr(bd.GetProperty()), i))
	}
	funnelCTE.
		SelectExpr(windowFunnelExpr, stepArgs...).
		From("events").
		Where(
			chq.Eq("project_id", projectID),
			chq.Gte("occur_time", from),
			chq.Lt("occur_time", to),
			stepFilter,
			topLevelFilterCond,
		).
		GroupBy("distinct_id")

	// Outer: one row per step (per breakdown value when breakdowns are present).
	// countIf(level >= N) gives users who reached at least step N.
	// Breakdown bucketing (raw_bd_N → breakdown_N) is a scalar expression here, not an aggregate.
	bdGroupBy := make([]string, len(breakdowns))
	bdOrderBy := make([]string, len(breakdowns))
	for j := range breakdowns {
		bdGroupBy[j] = fmt.Sprintf("breakdown_%d", j)
		bdOrderBy[j] = fmt.Sprintf("breakdown_%d ASC", j)
	}

	unionQueries := make([]*chq.Query, len(steps))
	for i, step := range steps {
		q := chq.NewQuery().
			Select(fmt.Sprintf("CAST(%d AS Int64) AS step_index", i)).
			SelectExpr("CAST(? AS String) AS event_kind", step.GetEvent().GetKind())
		for j := range breakdowns {
			q.Select(bucketRawExpr(j))
		}
		q.Select(
			fmt.Sprintf("toFloat64(countIf(level >= %d)) AS value", i+1),
			"toFloat64(0) AS avg_time_seconds",
		).
			From("funnel")
		if len(bdGroupBy) > 0 {
			q.GroupBy(bdGroupBy...)
		}
		if i == 0 {
			if topValsCTE != nil {
				q.With("top_vals", topValsCTE)
			}
			q.With("funnel", funnelCTE)
		}
		unionQueries[i] = q
	}

	orderBy := append([]string{"step_index ASC"}, bdOrderBy...)
	return chq.UnionAll(unionQueries...).OrderBy(orderBy...).Build()
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
//
// Breakdown attribution: when breakdowns are requested, each user's breakdown value is taken
// from their earliest step-matching event (first-touch attribution). The value is bucketed
// against the top-N CTE and falls back to '$others'.
func buildFunnelWithTiming(req *insightsv1.QueryRequest, projectID string) (string, []any, error) {
	steps := req.GetEvents()
	breakdowns := req.GetBreakdowns()

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

	// CTE: tag events with step index and raw breakdown property values.
	taggedCTE := chq.NewQuery().
		Select("distinct_id", "occur_time").
		SelectExpr(multiIfExpr, multiIfArgs...)
	for i, bd := range breakdowns {
		taggedCTE.Select(fmt.Sprintf("%s AS bd_%d", chq.PropertyExpr(bd.GetProperty()), i))
	}
	taggedCTE.
		From("events").
		Where(
			chq.Eq("project_id", projectID),
			chq.Gte("occur_time", from),
			chq.Lt("occur_time", to),
			chq.Or(orConds...),
			topLevelFilterCond,
		)

	// When breakdowns are requested, split into two query levels to avoid a double
	// evaluation of argMin:
	//
	//   user_arrays CTE: aggregates per-user event arrays + computes argMin(bd_N) once into raw_bd_N
	//   outer SELECT:    buckets raw_bd_N against top_vals (scalar, not aggregate)
	//
	// top_vals filters to step-matching events (via orConds) to be consistent with
	// user_arrays, which aggregates from the tagged CTE (step-matching events only).
	//
	// Without breakdowns, a single aggregation is sufficient.
	if len(breakdowns) > 0 {
		topValsCTE := buildTopValsCTE(breakdowns, projectID, from, to, req.GetBreakdownLimit(), chq.Or(orConds...), topLevelFilterCond)

		userArraysCTE := chq.NewQuery().
			Select(
				"distinct_id",
				"arraySort(groupArray(occur_time)) AS times",
				"arrayMap(x -> x.2, arraySort(x -> x.1, arrayZip(groupArray(occur_time), groupArray(toInt64(step_match))))) AS step_matches",
			)
		for i := range breakdowns {
			userArraysCTE.Select(fmt.Sprintf("argMin(bd_%d, occur_time) AS raw_bd_%d", i, i))
		}
		userArraysCTE.
			From("tagged").
			Where(chq.RawCond("step_match >= 0")).
			GroupBy("distinct_id")

		outerQ := chq.NewQuery()
		if topValsCTE != nil {
			outerQ.With("top_vals", topValsCTE)
		}
		outerQ.
			With("tagged", taggedCTE).
			With("user_arrays", userArraysCTE).
			Select("distinct_id", "times", "step_matches")
		for i := range breakdowns {
			outerQ.Select(bucketRawExpr(i))
		}
		outerQ.From("user_arrays")
		return outerQ.Build()
	}

	// No breakdowns: single aggregation, no top_vals needed.
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
	breakdowns := req.GetBreakdowns()

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

	// top-N CTE for breakdown bucketing is computed from start-event rows only.
	topValsCTE := buildTopValsCTE(breakdowns, projectID, from, to, req.GetBreakdownLimit(), startCond, topLevelFilterCond)

	// cohorts: assign each user to a time bucket based on their first start event.
	// first_event_time is the precise timestamp (not bucketed) — used to exclude
	// return events that fall within the same bucket but before the actual start.
	//
	// With breakdowns, use two CTEs to compute argMin exactly once per breakdown:
	//   cohorts_raw: aggregates min/argMin into raw_bd_N columns
	//   cohorts:     buckets raw_bd_N against top_vals (scalar, no second argMin call)
	// Without breakdowns, a single CTE suffices.
	cohortsAgg := chq.NewQuery().
		Select(
			"distinct_id",
			fmt.Sprintf("min(%s(occur_time)) AS cohort_time", granFn),
			"min(occur_time) AS first_event_time",
		)
	for i, bd := range breakdowns {
		cohortsAgg.Select(rawArgMinExpr(chq.PropertyExpr(bd.GetProperty()), i))
	}
	cohortsAgg.
		From("events").
		Where(
			chq.Eq("project_id", projectID),
			chq.Gte("occur_time", from),
			chq.Lt("occur_time", to),
			startCond,
			topLevelFilterCond,
		).
		GroupBy("distinct_id")

	// cohort_sizes: count users per (cohort_time[, breakdown...]).
	cohortGroupBy := make([]string, 0, 1+len(breakdowns))
	cohortGroupBy = append(cohortGroupBy, "cohort_time")
	for j := range breakdowns {
		cohortGroupBy = append(cohortGroupBy, fmt.Sprintf("breakdown_%d", j))
	}
	// Use a three-index slice so that append allocates a new backing array
	// instead of modifying cohortGroupBy's elements in-place.
	cohortSizes := chq.NewQuery().
		Select(append(cohortGroupBy[:len(cohortGroupBy):len(cohortGroupBy)], "toFloat64(count(*)) AS cohort_size")...).
		From("cohorts").
		GroupBy(cohortGroupBy...)

	// retained: count return events per cohort per time bucket [per breakdown].
	// Uses first_event_time (not bucketed cohort_time) to exclude return events
	// before the user's actual start. Conditions use "e." alias to avoid ambiguity in the JOIN.
	retainedSelect := []string{
		"c.cohort_time",
		fmt.Sprintf("%s(e.occur_time) AS t", granFn),
		"toFloat64(count(DISTINCT e.distinct_id)) AS retained_users",
	}
	retainedGroupBy := []string{"c.cohort_time", "t"}
	for j := range breakdowns {
		retainedSelect = append(retainedSelect, fmt.Sprintf("c.breakdown_%d", j))
		retainedGroupBy = append(retainedGroupBy, fmt.Sprintf("c.breakdown_%d", j))
	}
	retained := chq.NewQuery().
		Select(retainedSelect...).
		From("cohorts c INNER JOIN events e ON e.distinct_id = c.distinct_id").
		Where(
			chq.Eq("e.project_id", projectID),
			chq.Gte("e.occur_time", from),
			chq.Lt("e.occur_time", to),
			chq.RawCond("e.occur_time >= c.first_event_time"),
			returnCondAliased,
			topLevelFilterCondAliased,
		).
		GroupBy(retainedGroupBy...)

	// Final join: retained × cohort_sizes on (cohort_time[, breakdown...]).
	joinParts := make([]string, 0, 1+len(breakdowns))
	joinParts = append(joinParts, "r.cohort_time = cs.cohort_time")
	for j := range breakdowns {
		joinParts = append(joinParts, fmt.Sprintf("r.breakdown_%d = cs.breakdown_%d", j, j))
	}
	joinCond := strings.Join(joinParts, " AND ")

	finalSelect := []string{
		"r.cohort_time",
		"r.t",
		"if(cs.cohort_size = 0, 0, (r.retained_users * 100.0) / cs.cohort_size) AS value",
		"cs.cohort_size",
	}
	for j := range breakdowns {
		finalSelect = append(finalSelect, fmt.Sprintf("r.breakdown_%d", j))
	}

	orderBy := make([]string, 0, len(breakdowns)+2)
	for j := range breakdowns {
		orderBy = append(orderBy, fmt.Sprintf("r.breakdown_%d ASC", j))
	}
	orderBy = append(orderBy, "r.cohort_time ASC", "r.t ASC")

	q := chq.NewQuery()
	if topValsCTE != nil {
		q.With("top_vals", topValsCTE)
	}
	if len(breakdowns) > 0 {
		// cohorts_raw holds the aggregated + raw argMin values;
		// cohorts buckets them against top_vals.
		cohortsBucket := chq.NewQuery().Select("distinct_id", "cohort_time", "first_event_time")
		for i := range breakdowns {
			cohortsBucket.Select(bucketRawExpr(i))
		}
		cohortsBucket.From("cohorts_raw")
		q.With("cohorts_raw", cohortsAgg).With("cohorts", cohortsBucket)
	} else {
		q.With("cohorts", cohortsAgg)
	}
	return q.
		With("cohort_sizes", cohortSizes).
		With("retained", retained).
		Select(finalSelect...).
		From(fmt.Sprintf("retained r INNER JOIN cohort_sizes cs ON %s", joinCond)).
		Where(chq.RawCond("r.t >= r.cohort_time")).
		OrderBy(orderBy...).
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

// aggregationExpr returns the SQL aggregation expression for the given type and optional property.
// For TOTAL/UNIQUE_USERS/PER_USER_AVG, the property parameter is unused.
// For SUM/AVG/MIN/MAX, property is required (enforced by proto validation at the RPC boundary).
//
// AVG/MIN/MAX use ifNull(..., 0) because these ClickHouse aggregates return NULL when all
// inputs are NULL (e.g. all property values are non-numeric). SUM does not need this because
// ClickHouse sum() returns 0 for all-NULL inputs. The tradeoff is that "no data" and "actual
// zero" are indistinguishable in the result — consumers should check event counts separately
// if the distinction matters.
func aggregationExpr(agg insightsv1.AggregationType, property string) (string, error) {
	switch agg {
	case insightsv1.AggregationType_AGGREGATION_TYPE_SUM,
		insightsv1.AggregationType_AGGREGATION_TYPE_AVG,
		insightsv1.AggregationType_AGGREGATION_TYPE_MIN,
		insightsv1.AggregationType_AGGREGATION_TYPE_MAX:
		numeric := "toFloat64OrNull(" + chq.PropertyExpr(property) + ")"
		switch agg {
		case insightsv1.AggregationType_AGGREGATION_TYPE_SUM:
			return "sum(" + numeric + ")", nil
		case insightsv1.AggregationType_AGGREGATION_TYPE_AVG:
			return "ifNull(avg(" + numeric + "), 0)", nil
		case insightsv1.AggregationType_AGGREGATION_TYPE_MIN:
			return "ifNull(min(" + numeric + "), 0)", nil
		case insightsv1.AggregationType_AGGREGATION_TYPE_MAX:
			return "ifNull(max(" + numeric + "), 0)", nil
		default:
			return "", fmt.Errorf("aggregationExpr: unexpected numeric aggregation type %s", agg)
		}
	case insightsv1.AggregationType_AGGREGATION_TYPE_UNIQUE_USERS:
		return "toFloat64(count(DISTINCT distinct_id))", nil
	case insightsv1.AggregationType_AGGREGATION_TYPE_PER_USER_AVG:
		return "if(count(DISTINCT distinct_id) = 0, 0, toFloat64(count(*)) / toFloat64(count(DISTINCT distinct_id)))", nil
	default: // TOTAL and UNSPECIFIED
		return "toFloat64(count(*))", nil
	}
}
