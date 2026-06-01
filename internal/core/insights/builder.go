package insights

import (
	"fmt"
	"strings"

	chq "github.com/pug-sh/pug/internal/core/clickhouse"
	commonv1 "github.com/pug-sh/pug/internal/gen/proto/common/v1"
	insightsv1 "github.com/pug-sh/pug/internal/gen/proto/shared/insights/v1"
)

const DefaultPageSize int32 = 100

// PropertyValuesLimit is the max distinct values returned by property-values queries.
// Also used as the cache exhaustion threshold in Service.GetPropertyValues.
const PropertyValuesLimit = 100

// analyticsCacheTTL bounds how long a cached insight result may lag fresh writes.
// 60s balances query latency reduction against dashboard freshness.
//
// Applied to all cacheable public insight builders: BuildTrendsQuery,
// BuildSegmentationQuery, BuildSessionTrendsQuery, BuildSessionSegmentationQuery,
// BuildFunnelCountsQuery, BuildFunnelTimingQuery, BuildRetentionQuery,
// BuildUserFlowQuery. Other public
// builders in this package (property keys/values, segment users, event names)
// intentionally omit WithQueryCache — they either include `now()`
// (BuildAutoPropertyValuesQuery) or back dashboard typeahead where freshness matters
// more than the saved compute.
//
// Cache isolation: ClickHouse keys the query cache by query text + parameters. Pug binds
// project_id as a positional parameter on every cached builder, so per-project isolation
// holds as long as project_id is in the parameter set (verified by the BuildXxxQuery tests).
// Because Pug uses a single ClickHouse user, the server-level per-user partitioning of
// the cache does not provide additional tenant isolation — project_id binding is the
// load-bearing mechanism.
//
// Ops tuning: with multi-tenant traffic and breakdown queries the default 1 GB cache may
// thrash. Tune `query_cache_max_size_in_bytes` and `query_cache_max_entries` server-side
// if the cache hit rate degrades.
//
// Staleness mechanics: ClickHouse caches the post-execution result. Because the events
// table is ReplacingMergeTree without FINAL, a query executed during a window of pre-merge
// duplicates can cache an inflated count; once background merges collapse those duplicates,
// fresh queries return the lower value while cached reads keep the inflated value until
// the TTL expires. Worst-case staleness for the inflated result is one TTL (60s) plus
// the merge lag at the time the row was cached.
const analyticsCacheTTL = 60

// Typed query structs link builder output to the correct executor method at compile time.

// TrendsQuery is the compiled SQL for a trends insight.
type TrendsQuery struct {
	sql            string
	args           []any
	properties     []string // breakdown property names for GroupSeries
	breakdownLimit int
}

func (q TrendsQuery) SQL() string          { return q.sql }
func (q TrendsQuery) Args() []any          { return q.args }
func (q TrendsQuery) NumBreakdowns() int   { return len(q.properties) }
func (q TrendsQuery) Properties() []string { return q.properties }
func (q TrendsQuery) BreakdownLimit() int  { return q.breakdownLimit }

// ScalarQuery is the compiled SQL for a single-value query (segmentation).
type ScalarQuery struct {
	sql  string
	args []any
}

func (q ScalarQuery) SQL() string { return q.sql }
func (q ScalarQuery) Args() []any { return q.args }

// FunnelQuery is the compiled SQL for funnel step counts (no timing).
type FunnelQuery struct {
	sql            string
	args           []any
	properties     []string
	breakdownLimit int
}

func (q FunnelQuery) SQL() string          { return q.sql }
func (q FunnelQuery) Args() []any          { return q.args }
func (q FunnelQuery) NumBreakdowns() int   { return len(q.properties) }
func (q FunnelQuery) Properties() []string { return q.properties }
func (q FunnelQuery) BreakdownLimit() int  { return q.breakdownLimit }

// FunnelTimingQuery is the compiled SQL for per-user funnel event arrays.
type FunnelTimingQuery struct {
	sql            string
	args           []any
	kinds          []string // step event kinds for ComputeFunnelTiming
	windowSec      int64    // conversion window for ComputeFunnelTiming
	properties     []string
	breakdownLimit int
}

func (q FunnelTimingQuery) SQL() string          { return q.sql }
func (q FunnelTimingQuery) Args() []any          { return q.args }
func (q FunnelTimingQuery) Kinds() []string      { return q.kinds }
func (q FunnelTimingQuery) WindowSec() int64     { return q.windowSec }
func (q FunnelTimingQuery) NumBreakdowns() int   { return len(q.properties) }
func (q FunnelTimingQuery) Properties() []string { return q.properties }
func (q FunnelTimingQuery) BreakdownLimit() int  { return q.breakdownLimit }

// RetentionQuery is the compiled SQL for retention cohort analysis.
type RetentionQuery struct {
	sql            string
	args           []any
	properties     []string
	breakdownLimit int
}

func (q RetentionQuery) SQL() string          { return q.sql }
func (q RetentionQuery) Args() []any          { return q.args }
func (q RetentionQuery) NumBreakdowns() int   { return len(q.properties) }
func (q RetentionQuery) Properties() []string { return q.properties }
func (q RetentionQuery) BreakdownLimit() int  { return q.breakdownLimit }

// UserFlowQuery is the compiled SQL for a user-flow (Sankey) graph insight.
type UserFlowQuery struct {
	sql      string
	args     []any
	maxNodes int
	maxLinks int
}

func (q UserFlowQuery) SQL() string   { return q.sql }
func (q UserFlowQuery) Args() []any   { return q.args }
func (q UserFlowQuery) MaxNodes() int { return q.maxNodes }
func (q UserFlowQuery) MaxLinks() int { return q.maxLinks }

func BuildUserFlowQuery(req *insightsv1.QueryRequest, projectID string) (UserFlowQuery, error) {
	resolved := resolveUserFlowParams(req.GetSpec().GetUserFlow())
	q, err := buildUserFlowQuery(req, projectID, resolved)
	if err != nil {
		return UserFlowQuery{}, err
	}
	sql, args, err := q.WithQueryCache(analyticsCacheTTL).Build()
	if err != nil {
		return UserFlowQuery{}, fmt.Errorf("user flow: %w", err)
	}
	return UserFlowQuery{
		sql:      sql,
		args:     args,
		maxNodes: int(resolved.maxNodes),
		maxLinks: int(resolved.maxLinks),
	}, nil
}

func BuildTrendsQuery(req *insightsv1.QueryRequest, projectID string) (TrendsQuery, error) {
	events := normalizeTrendsEvents(req.GetSpec().GetEvents())
	var sql string
	var args []any
	var err error

	if len(events) > 1 && trendsNeedsUnionFallback(events) {
		uq, uerr := buildTrendsUnion(req, projectID, events)
		if uerr != nil {
			return TrendsQuery{}, uerr
		}
		sql, args, err = uq.WithQueryCache(analyticsCacheTTL).Build()
	} else {
		q, qerr := buildTrends(req, projectID)
		if qerr != nil {
			return TrendsQuery{}, qerr
		}
		sql, args, err = q.WithQueryCache(analyticsCacheTTL).Build()
	}
	if err != nil {
		return TrendsQuery{}, fmt.Errorf("trends: %w", err)
	}
	return TrendsQuery{
		sql:            sql,
		args:           args,
		properties:     breakdownProps(req.GetSpec().GetBreakdowns()),
		breakdownLimit: effectiveBreakdownLimit(req.GetSpec().GetBreakdownLimit()),
	}, nil
}

func BuildSegmentationQuery(req *insightsv1.QueryRequest, projectID string) (ScalarQuery, error) {
	q, err := buildSegmentation(req, projectID)
	if err != nil {
		return ScalarQuery{}, err
	}
	sql, args, err := q.WithQueryCache(analyticsCacheTTL).Build()
	if err != nil {
		return ScalarQuery{}, fmt.Errorf("segmentation: %w", err)
	}
	return ScalarQuery{sql: sql, args: args}, nil
}

func BuildSessionTrendsQuery(req *insightsv1.QueryRequest, projectID string) (TrendsQuery, error) {
	q, err := buildSessionTrends(req, projectID)
	if err != nil {
		return TrendsQuery{}, err
	}
	sql, args, err := q.WithQueryCache(analyticsCacheTTL).Build()
	if err != nil {
		return TrendsQuery{}, fmt.Errorf("session trends: %w", err)
	}
	return TrendsQuery{
		sql:            sql,
		args:           args,
		properties:     breakdownProps(req.GetSpec().GetBreakdowns()),
		breakdownLimit: effectiveBreakdownLimit(req.GetSpec().GetBreakdownLimit()),
	}, nil
}

func BuildSessionSegmentationQuery(req *insightsv1.QueryRequest, projectID string) (ScalarQuery, error) {
	q, err := buildSessionSegmentation(req, projectID)
	if err != nil {
		return ScalarQuery{}, err
	}
	sql, args, err := q.WithQueryCache(analyticsCacheTTL).Build()
	if err != nil {
		return ScalarQuery{}, fmt.Errorf("session segmentation: %w", err)
	}
	return ScalarQuery{sql: sql, args: args}, nil
}

func BuildFunnelCountsQuery(req *insightsv1.QueryRequest, projectID string) (FunnelQuery, error) {
	q, err := buildFunnelWindowFunnel(req, projectID)
	if err != nil {
		return FunnelQuery{}, err
	}
	sql, args, err := q.WithQueryCache(analyticsCacheTTL).Build()
	if err != nil {
		return FunnelQuery{}, fmt.Errorf("funnel counts: %w", err)
	}
	return FunnelQuery{
		sql:            sql,
		args:           args,
		properties:     breakdownProps(req.GetSpec().GetBreakdowns()),
		breakdownLimit: effectiveBreakdownLimit(req.GetSpec().GetBreakdownLimit()),
	}, nil
}

// BuildFunnelTimingQuery returns SQL plus per-user event arrays + the kinds and conversion
// window needed by ComputeFunnelTiming downstream.
func BuildFunnelTimingQuery(req *insightsv1.QueryRequest, projectID string) (FunnelTimingQuery, error) {
	q, err := buildFunnelWithTiming(req, projectID)
	if err != nil {
		return FunnelTimingQuery{}, err
	}
	sql, args, err := q.WithQueryCache(analyticsCacheTTL).Build()
	if err != nil {
		return FunnelTimingQuery{}, fmt.Errorf("funnel timing: %w", err)
	}
	windowSec, err := EffectiveWindowSec(req)
	if err != nil {
		return FunnelTimingQuery{}, err
	}
	steps := req.GetSpec().GetEvents()
	kinds := make([]string, len(steps))
	for i, s := range steps {
		kinds[i] = s.GetEvent().GetKind()
	}
	return FunnelTimingQuery{
		sql:            sql,
		args:           args,
		kinds:          kinds,
		windowSec:      windowSec,
		properties:     breakdownProps(req.GetSpec().GetBreakdowns()),
		breakdownLimit: effectiveBreakdownLimit(req.GetSpec().GetBreakdownLimit()),
	}, nil
}

func BuildRetentionQuery(req *insightsv1.QueryRequest, projectID string) (RetentionQuery, error) {
	q, err := buildRetention(req, projectID)
	if err != nil {
		return RetentionQuery{}, err
	}
	sql, args, err := q.WithQueryCache(analyticsCacheTTL).Build()
	if err != nil {
		return RetentionQuery{}, fmt.Errorf("retention: %w", err)
	}
	return RetentionQuery{
		sql:            sql,
		args:           args,
		properties:     breakdownProps(req.GetSpec().GetBreakdowns()),
		breakdownLimit: effectiveBreakdownLimit(req.GetSpec().GetBreakdownLimit()),
	}, nil
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

const defaultBreakdownLimit = 10

// effectiveBreakdownLimit returns the breakdown limit, defaulting to 10 when zero.
func effectiveBreakdownLimit(limit int32) int {
	if limit <= 0 {
		return defaultBreakdownLimit
	}
	return int(limit)
}

// rawArgMinExpr returns an aggregate SELECT expression that computes first-touch attribution
// into a named column (breakdown_N). Use this in the aggregation CTE so argMin is evaluated once.
func rawArgMinExpr(colExpr string, i int) string {
	return fmt.Sprintf("argMin(%s, occur_time) AS breakdown_%d", colExpr, i)
}

func buildTrends(req *insightsv1.QueryRequest, projectID string) (*chq.Query, error) {
	events := normalizeTrendsEvents(req.GetSpec().GetEvents())
	if len(events) == 1 {
		return buildTrendsSingleBranch(req, projectID, events[0], 0)
	}
	return buildTrendsMultiEvent(req, projectID, events)
}

func normalizeTrendsEvents(events []*insightsv1.EventQuery) []*insightsv1.EventQuery {
	if len(events) == 0 {
		return []*insightsv1.EventQuery{{
			Event:       &commonv1.EventFilter{},
			Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL.Enum(),
		}}
	}
	return events
}

func trendsNeedsUnionFallback(events []*insightsv1.EventQuery) bool {
	for _, ev := range events {
		if ev.GetEvent().GetKind() == "" {
			return true
		}
	}
	return false
}

func buildTrendsUnion(req *insightsv1.QueryRequest, projectID string, events []*insightsv1.EventQuery) (*chq.UnionQuery, error) {
	topLevelFilterCond, err := buildTopLevelFilterCondition(req.GetSpec().GetFilterGroups(), req.GetSpec().GetFilterGroupsOperator(), projectID, "")
	if err != nil {
		return nil, fmt.Errorf("trends: %w", err)
	}

	queries := make([]*chq.Query, 0, len(events))
	for i, ev := range events {
		q, err := buildTrendsBranchQuery(req, projectID, ev, i, topLevelFilterCond)
		if err != nil {
			return nil, err
		}
		queries = append(queries, q)
	}

	orderBy := trendsOrderBy(req.GetSpec().GetBreakdowns())
	return chq.UnionAll(queries...).OrderBy(orderBy...), nil
}

func buildTrendsSingleBranch(req *insightsv1.QueryRequest, projectID string, ev *insightsv1.EventQuery, idx int) (*chq.Query, error) {
	topLevelFilterCond, err := buildTopLevelFilterCondition(req.GetSpec().GetFilterGroups(), req.GetSpec().GetFilterGroupsOperator(), projectID, "")
	if err != nil {
		return nil, fmt.Errorf("trends: %w", err)
	}
	q, err := buildTrendsBranchQuery(req, projectID, ev, idx, topLevelFilterCond)
	if err != nil {
		return nil, err
	}
	return q.OrderBy(trendsOrderBy(req.GetSpec().GetBreakdowns())...), nil
}

func buildTrendsBranchQuery(
	req *insightsv1.QueryRequest,
	projectID string,
	ev *insightsv1.EventQuery,
	idx int,
	topLevelFilterCond chq.Condition,
) (*chq.Query, error) {
	granFn, err := granularityFunc(req.GetGranularity())
	if err != nil {
		return nil, fmt.Errorf("trends: %w", err)
	}
	breakdowns := req.GetSpec().GetBreakdowns()

	agg := ev.GetAggregation()
	if agg == insightsv1.AggregationType_AGGREGATION_TYPE_UNSPECIFIED {
		agg = insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL
	}

	selectExprs := []string{
		fmt.Sprintf("%s(occur_time) AS t", granFn),
		"kind AS event_kind",
	}
	for j, bd := range breakdowns {
		selectExprs = append(selectExprs,
			fmt.Sprintf("%s AS breakdown_%d", chq.PropertyExpr(bd.GetProperty()), j))
	}

	aggExpr, err := aggregationExpr(agg, ev.GetAggregationProperty())
	if err != nil {
		return nil, fmt.Errorf("trends: events[%d]: %w", idx, err)
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
			return nil, fmt.Errorf("trends: events[%d]: %w", idx, err)
		}
		query.Where(cond)
	}

	groupByCols := []string{"t", "event_kind"}
	for j := range breakdowns {
		groupByCols = append(groupByCols, fmt.Sprintf("breakdown_%d", j))
	}
	return query.GroupBy(groupByCols...), nil
}

// buildTrendsMultiEvent compiles a multi-event trends query as a single events scan
// with conditional aggregates (countIf/uniqIf/…) per event, unpivoted via CROSS JOIN
// against a compact event-index table — same pattern as funnel counts.
func buildTrendsMultiEvent(req *insightsv1.QueryRequest, projectID string, events []*insightsv1.EventQuery) (*chq.Query, error) {
	granFn, err := granularityFunc(req.GetGranularity())
	if err != nil {
		return nil, fmt.Errorf("trends: %w", err)
	}
	breakdowns := req.GetSpec().GetBreakdowns()
	from := req.GetTimeRange().GetFrom().AsTime()
	to := req.GetTimeRange().GetTo().AsTime()

	topLevelFilterCond, err := buildTopLevelFilterCondition(req.GetSpec().GetFilterGroups(), req.GetSpec().GetFilterGroupsOperator(), projectID, "")
	if err != nil {
		return nil, fmt.Errorf("trends: %w", err)
	}

	orConds := make([]chq.Condition, 0, len(events))
	eventMetaParts := make([]string, 0, len(events))
	valueCases := make([]string, 0, len(events))

	aggCTE := chq.NewQuery().
		Select(fmt.Sprintf("%s(occur_time) AS t", granFn))
	for j, bd := range breakdowns {
		aggCTE.Select(fmt.Sprintf("%s AS breakdown_%d", chq.PropertyExpr(bd.GetProperty()), j))
	}

	for i, ev := range events {
		cond, err := buildEventCondition([]*insightsv1.EventQuery{ev}, projectID)
		if err != nil {
			return nil, fmt.Errorf("trends: events[%d]: %w", i, err)
		}
		if cond.IsZero() {
			return nil, fmt.Errorf("trends: events[%d]: empty event filter", i)
		}
		orConds = append(orConds, cond)

		agg := ev.GetAggregation()
		if agg == insightsv1.AggregationType_AGGREGATION_TYPE_UNSPECIFIED {
			agg = insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL
		}
		valExpr, valArgs, err := aggregationExprIf(cond, agg, ev.GetAggregationProperty())
		if err != nil {
			return nil, fmt.Errorf("trends: events[%d]: %w", i, err)
		}
		aggCTE.SelectExpr(valExpr+fmt.Sprintf(" AS val_%d", i), valArgs...)

		kind := ev.GetEvent().GetKind()
		eventMetaParts = append(eventMetaParts, fmt.Sprintf(
			"SELECT CAST(%d AS Int64) AS event_index, %s AS event_kind",
			i, sqlStringLiteral(kind),
		))
		valueCases = append(valueCases, fmt.Sprintf("WHEN %d THEN a.val_%d", i, i))
	}

	aggGroupBy := []string{"t"}
	for j := range breakdowns {
		aggGroupBy = append(aggGroupBy, fmt.Sprintf("breakdown_%d", j))
	}
	aggCTE.
		From("events").
		Where(
			chq.Eq("project_id", projectID),
			chq.Gte("occur_time", from),
			chq.Lt("occur_time", to),
			topLevelFilterCond,
			chq.Or(orConds...),
		).
		GroupBy(aggGroupBy...)

	eventsMetaExpr := strings.Join(eventMetaParts, " UNION ALL ")
	valueExpr := fmt.Sprintf("CASE s.event_index %s END AS value", strings.Join(valueCases, " "))

	finalSelect := []string{"a.t", "s.event_kind"}
	for j := range breakdowns {
		finalSelect = append(finalSelect, fmt.Sprintf("a.breakdown_%d", j))
	}
	finalSelect = append(finalSelect, valueExpr)

	orderBy := trendsOrderBy(breakdowns)

	return chq.NewQuery().
		With("agg", aggCTE).
		Select(finalSelect...).
		From(fmt.Sprintf("agg a CROSS JOIN (%s) AS s", eventsMetaExpr)).
		OrderBy(orderBy...), nil
}

func trendsOrderBy(breakdowns []*insightsv1.Breakdown) []string {
	orderBy := []string{"t ASC", "event_kind ASC"}
	for j := range breakdowns {
		orderBy = append(orderBy, fmt.Sprintf("breakdown_%d ASC", j))
	}
	return orderBy
}

func buildSegmentation(req *insightsv1.QueryRequest, projectID string) (*chq.Query, error) {
	firstEvent := func() *insightsv1.EventQuery {
		if len(req.GetSpec().GetEvents()) > 0 {
			return req.GetSpec().GetEvents()[0]
		}
		return nil
	}()
	var aggProp string
	if firstEvent != nil {
		aggProp = firstEvent.GetAggregationProperty()
	}
	aggExpr, err := aggregationExpr(aggregationType(req), aggProp)
	if err != nil {
		return nil, fmt.Errorf("segmentation: %w", err)
	}

	topLevelFilterCond, err := buildTopLevelFilterCondition(req.GetSpec().GetFilterGroups(), req.GetSpec().GetFilterGroupsOperator(), projectID, "")
	if err != nil {
		return nil, fmt.Errorf("segmentation: %w", err)
	}

	eventCond, err := buildEventCondition(req.GetSpec().GetEvents(), projectID)
	if err != nil {
		return nil, fmt.Errorf("segmentation: %w", err)
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
		), nil
}

// buildSessionTrends builds the raw-events session trends query: the per-session
// CTE (buildSessionRowsCTE) bucketed by start_time at the requested granularity,
// emitting one series per breakdown value. The metric aggregate (sessionMetricAggExpr)
// is applied across the sessions in each bucket. Mirrors buildSessionTrendsFromRollup.
func buildSessionTrends(req *insightsv1.QueryRequest, projectID string) (*chq.Query, error) {
	session := req.GetSpec().GetSession()
	if session == nil {
		return nil, fmt.Errorf("session trends: session is required")
	}
	granFn, err := granularityFunc(req.GetGranularity())
	if err != nil {
		return nil, fmt.Errorf("session trends: %w", err)
	}
	sessionsCTE, err := buildSessionRowsCTE(req, projectID)
	if err != nil {
		return nil, fmt.Errorf("session trends: %w", err)
	}

	metricExpr, err := sessionMetricAggExpr(session.GetMetric())
	if err != nil {
		return nil, fmt.Errorf("session trends: %w", err)
	}

	breakdowns := req.GetSpec().GetBreakdowns()
	selectExprs := []string{
		fmt.Sprintf("%s(start_time) AS t", granFn),
		sqlStringLiteral(sessionEventKind(session)) + " AS event_kind",
	}
	groupByCols := []string{"t"}
	orderByCols := []string{"t ASC", "event_kind ASC"}
	for j := range breakdowns {
		selectExprs = append(selectExprs, fmt.Sprintf("breakdown_%d", j))
		groupByCols = append(groupByCols, fmt.Sprintf("breakdown_%d", j))
		orderByCols = append(orderByCols, fmt.Sprintf("breakdown_%d ASC", j))
	}
	selectExprs = append(selectExprs, metricExpr+" AS value")

	return chq.NewQuery().
		With("sessions", sessionsCTE).
		Select(selectExprs...).
		From("sessions").
		GroupBy(groupByCols...).
		OrderBy(orderByCols...), nil
}

// buildSessionSegmentation builds the raw-events session segmentation query: the
// per-session CTE (buildSessionRowsCTE) collapsed to a single scalar via the metric
// aggregate. Mirrors buildSessionSegmentationFromRollup.
func buildSessionSegmentation(req *insightsv1.QueryRequest, projectID string) (*chq.Query, error) {
	session := req.GetSpec().GetSession()
	if session == nil {
		return nil, fmt.Errorf("session segmentation: session is required")
	}

	sessionsCTE, err := buildSessionRowsCTE(req, projectID)
	if err != nil {
		return nil, fmt.Errorf("session segmentation: %w", err)
	}
	metricExpr, err := sessionMetricAggExpr(session.GetMetric())
	if err != nil {
		return nil, fmt.Errorf("session segmentation: %w", err)
	}

	return chq.NewQuery().
		With("sessions", sessionsCTE).
		Select(metricExpr + " AS value").
		From("sessions"), nil
}

// buildSessionRowsCTE builds the per-session aggregation CTE shared by the raw
// session trends and segmentation builders. Each row is one session: its start
// (min occur_time), end (max occur_time), scoped event_count, and — when a
// breakdown is requested — the entry (argMin) or exit (argMax) attribute keyed on
// occur_time.
//
// Full-session window semantics: a session is measured over its ENTIRE set of
// (scoped) events and bucketed by its start instant, NOT clipped to the query
// window. The window is therefore applied as a HAVING on the computed start_time
// (`start_time >= from AND start_time < to`), not as a WHERE on occur_time. This
// is what keeps the raw path numerically identical to the session rollup
// (buildSessionRollupRowsCTE), whose merged per-session states span the session's
// whole lifetime. Clipping events to the window in WHERE instead would truncate a
// session straddling the boundary — changing its duration, entry/exit page, and
// bounce classification — and diverge from the rollup. The cost is that the scan
// is not partition-pruned by occur_time; acceptable because this is the fallback
// path (the rollup serves the common day-aligned case) and correctness outranks
// the wider scan. See docs/architecture/insights.md (Session insights).
//
// `scope` (kind + optional filters) and any top-level filter_groups stay as
// row-level WHERE conditions: they decide which events PARTICIPATE in the session
// measurement, mirroring the rollup's per-kind partition. Only the time window
// moves to HAVING.
func buildSessionRowsCTE(req *insightsv1.QueryRequest, projectID string) (*chq.Query, error) {
	spec := req.GetSpec()
	session := spec.GetSession()
	if session == nil {
		return nil, fmt.Errorf("session is required")
	}

	topLevelFilterCond, err := buildTopLevelFilterCondition(spec.GetFilterGroups(), spec.GetFilterGroupsOperator(), projectID, "")
	if err != nil {
		return nil, err
	}
	scopeCond, err := buildSessionScopeCondition(session, projectID, "")
	if err != nil {
		return nil, err
	}

	attrFn := "argMin"
	if session.GetMetric() == insightsv1.SessionMetric_SESSION_METRIC_EXIT {
		attrFn = "argMax"
	}

	q := chq.NewQuery().
		Select(
			"session_id",
			"min(occur_time) AS start_time",
			"max(occur_time) AS end_time",
			"count() AS event_count",
		).
		From("events").
		Where(
			chq.Eq("project_id", projectID),
			topLevelFilterCond,
			scopeCond,
		).
		GroupBy("session_id").
		HavingExpr("start_time >= ? AND start_time < ?",
			req.GetTimeRange().GetFrom().AsTime(),
			req.GetTimeRange().GetTo().AsTime())

	for j, bd := range spec.GetBreakdowns() {
		q.Select(fmt.Sprintf("%s(%s, occur_time) AS breakdown_%d", attrFn, chq.PropertyExpr(bd.GetProperty()), j))
	}
	return q, nil
}

func buildSessionScopeCondition(session *insightsv1.SessionQuery, projectID, alias string) (chq.Condition, error) {
	if session == nil || session.GetScope() == nil {
		return chq.Condition{}, nil
	}
	return chq.EventConditionAliased([]*commonv1.EventFilter{session.GetScope()}, projectID, alias)
}

// sessionEventKind returns the label projected as the trends series `event_kind`
// for a session query. With no scope kind, sessions span all event kinds, so there
// is no real kind to report — the synthetic literal "$session" labels that series
// (the leading "$" matches the reserved-property convention and can't collide with a
// customer event kind). With a scope kind, that kind is the label. Segmentation
// (scalar) does not project a series and ignores this.
func sessionEventKind(session *insightsv1.SessionQuery) string {
	if session == nil || session.GetScope() == nil || session.GetScope().GetKind() == "" {
		return "$session"
	}
	return session.GetScope().GetKind()
}

// sessionMetricAggExpr returns the aggregate applied over the per-session CTE rows
// (one row per session) to produce the metric value. SESSIONS/ENTRY/EXIT all reduce
// to count() of sessions — ENTRY/EXIT differ only in the breakdown attribute the CTE
// already resolved (argMin vs argMax), not in this aggregate. AVG_DURATION and
// BOUNCE_RATE guard count()=0 so an empty bucket yields 0 rather than NULL/NaN.
// Identical between the raw and rollup paths so their numbers match.
func sessionMetricAggExpr(metric insightsv1.SessionMetric) (string, error) {
	switch metric {
	case insightsv1.SessionMetric_SESSION_METRIC_SESSIONS,
		insightsv1.SessionMetric_SESSION_METRIC_ENTRY,
		insightsv1.SessionMetric_SESSION_METRIC_EXIT:
		return "toFloat64(count())", nil
	case insightsv1.SessionMetric_SESSION_METRIC_AVG_DURATION:
		return "if(count() = 0, 0, avg(dateDiff('second', start_time, end_time)))", nil
	case insightsv1.SessionMetric_SESSION_METRIC_BOUNCE_RATE:
		return "if(count() = 0, 0, toFloat64(countIf(event_count = 1)) * 100.0 / toFloat64(count()))", nil
	default:
		return "", fmt.Errorf("unsupported session metric %s", metric)
	}
}

// buildFunnelWindowFunnel generates a funnel counts query using ClickHouse's windowFunnel() aggregate.
// windowFunnel scans the events table once and returns the deepest step reached per user
// within the conversion window. A CROSS JOIN against a compact steps table produces
// cumulative counts per step in a single pass — no UNION ALL, no repeated CTE evaluation.
//
// Breakdown attribution: when breakdowns are requested, each user is assigned a breakdown
// value from their earliest step-matching event in the time range (first-touch attribution). windowFunnel
// then runs over the step-filtered events, and results are grouped by breakdown value.
func buildFunnelWindowFunnel(req *insightsv1.QueryRequest, projectID string) (*chq.Query, error) {
	steps := req.GetSpec().GetEvents()
	if len(steps) == 0 {
		return nil, fmt.Errorf("funnel: at least one step required")
	}
	breakdowns := req.GetSpec().GetBreakdowns()

	topLevelFilterCond, err := buildTopLevelFilterCondition(req.GetSpec().GetFilterGroups(), req.GetSpec().GetFilterGroupsOperator(), projectID, "")
	if err != nil {
		return nil, fmt.Errorf("funnel: %w", err)
	}

	stepExprs := make([]string, len(steps))
	var stepArgs []any
	orConds := make([]chq.Condition, len(steps))
	for i, step := range steps {
		cond, err := buildFunnelStepCondition(step, projectID, i)
		if err != nil {
			return nil, err
		}
		stepExprs[i] = cond.SQL()
		stepArgs = append(stepArgs, cond.Args()...)
		orConds[i] = chq.RawCond(cond.SQL(), cond.Args()...)
	}
	stepFilter := chq.Or(orConds...)

	from := req.GetTimeRange().GetFrom().AsTime()
	to := req.GetTimeRange().GetTo().AsTime()
	windowSec, err := EffectiveWindowSec(req)
	if err != nil {
		return nil, fmt.Errorf("funnel: %w", err)
	}

	windowFunnelExpr := fmt.Sprintf(
		"windowFunnel(%d)(toDateTime(occur_time), %s) AS level",
		windowSec, strings.Join(stepExprs, ", "),
	)

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

	// Build a compact steps table: SELECT 0,'page_view' UNION ALL SELECT 1,'click' ...
	// Event kinds are embedded as escaped SQL literals (validated by proto regex).
	stepParts := make([]string, len(steps))
	for i, step := range steps {
		stepParts[i] = fmt.Sprintf(
			"SELECT CAST(%d AS Int64) AS step_index, %s AS event_kind",
			i, sqlStringLiteral(step.GetEvent().GetKind()),
		)
	}
	stepsExpr := strings.Join(stepParts, " UNION ALL ")

	selectCols := []string{
		"s.step_index",
		"s.event_kind",
	}
	for j := range breakdowns {
		selectCols = append(selectCols, fmt.Sprintf("f.breakdown_%d", j))
	}
	selectCols = append(selectCols, "toFloat64(countIf(f.level >= s.step_index + 1)) AS value")

	groupByCols := make([]string, 0, 2+len(breakdowns))
	groupByCols = append(groupByCols, "s.step_index", "s.event_kind")
	for j := range breakdowns {
		groupByCols = append(groupByCols, fmt.Sprintf("f.breakdown_%d", j))
	}

	orderBy := []string{"s.step_index ASC"}
	for j := range breakdowns {
		orderBy = append(orderBy, fmt.Sprintf("f.breakdown_%d ASC", j))
	}

	q := chq.NewQuery().
		With("funnel", funnelCTE).
		Select(selectCols...).
		From(fmt.Sprintf("funnel f CROSS JOIN (%s) AS s", stepsExpr)).
		GroupBy(groupByCols...).
		OrderBy(orderBy...)
	return q, nil
}

// sqlStringLiteral wraps s as a single-quoted SQL literal with proper escaping.
func sqlStringLiteral(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "\\'") + "'"
}

// buildFunnelWithTiming generates a funnel query that returns per-user event arrays
// for Go-side step matching and timing computation.
//
// Strategy: tag each event with which step it matches (via multiIf), aggregate into
// per-user arrays, then Go walks the arrays to greedily match steps and compute intervals.
//
// Performance: when >= 2 steps, a windowFunnel pre-filter IN subquery restricts the tagged
// CTE to users who actually progressed past step 0. This dramatically reduces groupArray
// output and data transfer for timing computation.
//
// Limitation: multiIf short-circuits — if two steps share the same conditions (e.g., both
// match kind='page_view'), events always tag as the earlier step and the later step never
// matches. This is uncommon in practice (funnel steps are usually distinct events).
//
// Breakdown attribution: when breakdowns are requested, each user's breakdown value is taken
// from their earliest step-matching event (first-touch attribution).
func buildFunnelWithTiming(req *insightsv1.QueryRequest, projectID string) (*chq.Query, error) {
	steps := req.GetSpec().GetEvents()
	if len(steps) == 0 {
		return nil, fmt.Errorf("funnel: at least one step required")
	}
	breakdowns := req.GetSpec().GetBreakdowns()

	topLevelFilterCond, err := buildTopLevelFilterCondition(req.GetSpec().GetFilterGroups(), req.GetSpec().GetFilterGroupsOperator(), projectID, "")
	if err != nil {
		return nil, fmt.Errorf("funnel: %w", err)
	}

	from := req.GetTimeRange().GetFrom().AsTime()
	to := req.GetTimeRange().GetTo().AsTime()
	windowSec, err := EffectiveWindowSec(req)
	if err != nil {
		return nil, fmt.Errorf("funnel: %w", err)
	}

	// Build step conditions once; SQL + args are reused for multiIf, WHERE, and pre-filter.
	type stepCond struct {
		sql  string
		args []any
	}
	sc := make([]stepCond, len(steps))
	for i, step := range steps {
		cond, err := buildFunnelStepCondition(step, projectID, i)
		if err != nil {
			return nil, err
		}
		sc[i] = stepCond{sql: cond.SQL(), args: cond.Args()}
	}

	freshOrConds := func() []chq.Condition {
		conds := make([]chq.Condition, len(sc))
		for i, s := range sc {
			conds[i] = chq.RawCond(s.sql, s.args...)
		}
		return conds
	}

	// multiIf expression to tag each event with its step index (-1 = no match).
	var multiIfParts []string
	var multiIfArgs []any
	for i, s := range sc {
		multiIfParts = append(multiIfParts, fmt.Sprintf("%s, %d", s.sql, i))
		multiIfArgs = append(multiIfArgs, s.args...)
	}
	multiIfExpr := "multiIf(" + strings.Join(multiIfParts, ", ") + ", -1) AS step_match"

	// Pre-filter: restrict to users who reached at least step 2 in windowFunnel.
	// Scans events once with windowFunnel (fast), then the tagged CTE only processes
	// events from users who have timing data to contribute.
	var preFilterCond chq.Condition
	if len(steps) >= 2 {
		pfStepSQLs := make([]string, len(sc))
		var pfHavingArgs []any
		pfHavingArgs = append(pfHavingArgs, windowSec)
		for i, s := range sc {
			pfStepSQLs[i] = s.sql
			pfHavingArgs = append(pfHavingArgs, s.args...)
		}
		wfHaving := fmt.Sprintf(
			"windowFunnel(?)(toDateTime(occur_time), %s) >= 2",
			strings.Join(pfStepSQLs, ", "),
		)

		pfTopLevel, _ := buildTopLevelFilterCondition(
			req.GetSpec().GetFilterGroups(), req.GetSpec().GetFilterGroupsOperator(), projectID, "")
		pfQuery := chq.NewQuery().
			Select("distinct_id").
			From("events").
			Where(
				chq.Eq("project_id", projectID),
				chq.Gte("occur_time", from),
				chq.Lt("occur_time", to),
				chq.Or(freshOrConds()...),
				pfTopLevel,
			).
			GroupBy("distinct_id").
			HavingExpr(wfHaving, pfHavingArgs...)

		pfSQL, pfArgs, err := pfQuery.Build()
		if err != nil {
			return nil, fmt.Errorf("funnel pre-filter: %w", err)
		}
		preFilterCond = chq.RawCond(
			fmt.Sprintf("distinct_id IN (%s)", pfSQL),
			pfArgs...,
		)
	}

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
			chq.Or(freshOrConds()...),
			topLevelFilterCond,
			preFilterCond,
		)

	q := chq.NewQuery().
		With("tagged", taggedCTE).
		Select(
			"distinct_id",
			"arraySort(groupArray(occur_time)) AS times",
			"arrayMap(x -> x.2, arraySort(x -> x.1, arrayZip(groupArray(occur_time), groupArray(toInt64(step_match))))) AS step_matches",
		)
	for i := range breakdowns {
		q.Select(fmt.Sprintf("argMin(bd_%d, occur_time) AS breakdown_%d", i, i))
	}
	q.From("tagged").
		Where(chq.RawCond("step_match >= 0")).
		GroupBy("distinct_id")
	return q, nil
}

func buildRetention(req *insightsv1.QueryRequest, projectID string) (*chq.Query, error) {
	events := req.GetSpec().GetEvents()
	breakdowns := req.GetSpec().GetBreakdowns()

	if len(events) == 0 {
		return nil, fmt.Errorf("retention: requires at least one event")
	}

	startEvent := events[0]
	returnEvent := startEvent
	if len(events) > 1 {
		returnEvent = events[1]
	}

	topLevelFilterCond, err := buildTopLevelFilterCondition(req.GetSpec().GetFilterGroups(), req.GetSpec().GetFilterGroupsOperator(), projectID, "")
	if err != nil {
		return nil, fmt.Errorf("retention: %w", err)
	}

	startCond, err := buildEventCondition([]*insightsv1.EventQuery{startEvent}, projectID)
	if err != nil {
		return nil, fmt.Errorf("retention start event: %w", err)
	}
	if startCond.IsZero() {
		return nil, fmt.Errorf("retention start event: empty event filter")
	}

	// Build return event and top-level filter conditions with "e" alias for the JOINed CTE.
	returnCondAliased, err := buildEventConditionAliased([]*insightsv1.EventQuery{returnEvent}, projectID, "e")
	if err != nil {
		return nil, fmt.Errorf("retention return event: %w", err)
	}
	if returnCondAliased.IsZero() {
		return nil, fmt.Errorf("retention return event: empty event filter")
	}

	topLevelFilterCondAliased, err := buildTopLevelFilterCondition(req.GetSpec().GetFilterGroups(), req.GetSpec().GetFilterGroupsOperator(), projectID, "e")
	if err != nil {
		return nil, fmt.Errorf("retention: %w", err)
	}

	granFn, err := granularityFunc(req.GetGranularity())
	if err != nil {
		return nil, fmt.Errorf("retention: %w", err)
	}
	from := req.GetTimeRange().GetFrom().AsTime()
	to := req.GetTimeRange().GetTo().AsTime()

	// cohorts: assign each user to a time bucket based on their first start event.
	// first_event_time is the precise timestamp (not bucketed) — used to exclude
	// return events that fall within the same bucket but before the actual start.
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
		"toFloat64(uniq(e.distinct_id)) AS retained_users",
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

	return chq.NewQuery().
		With("cohorts", cohortsAgg).
		With("cohort_sizes", cohortSizes).
		With("retained", retained).
		Select(finalSelect...).
		From(fmt.Sprintf("retained r INNER JOIN cohort_sizes cs ON %s", joinCond)).
		Where(chq.RawCond("r.t >= r.cohort_time")).
		OrderBy(orderBy...), nil
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
// Both auto_properties and custom_properties are Map(String, Variant(...)) and must
// be CAST to Nullable(String) so DISTINCT and the non-empty filter operate on a
// string projection.
func buildPropertyValuesQuery(projectID, propertyKey, mapCol, eventKind string) (string, []any, error) {
	var selectExpr, propertyNotEmptyClause string
	var selectArgs, notEmptyArgs []any
	switch mapCol {
	case "auto_properties":
		if dv, ok := chq.AutoPropertyDistinctValuesFor(propertyKey); ok {
			selectExpr = dv.SelectExpr
			propertyNotEmptyClause = dv.NotEmptyClause
			selectArgs = dv.Args
			notEmptyArgs = dv.Args
		} else {
			return "", nil, fmt.Errorf("unsupported auto property key %q", propertyKey)
		}
	case "custom_properties":
		selectExpr = fmt.Sprintf("CAST(%s[?] AS Nullable(String)) AS value", mapCol)
		propertyNotEmptyClause = fmt.Sprintf("CAST(%s[?] AS Nullable(String)) != ''", mapCol)
		selectArgs = []any{propertyKey}
		notEmptyArgs = []any{propertyKey}
	default:
		return "", nil, fmt.Errorf("unsupported property source %q", mapCol)
	}

	q := chq.NewQuery().
		SelectExpr("DISTINCT "+selectExpr, selectArgs...).
		From("events").
		Where(
			chq.Eq("project_id", projectID),
			chq.When(eventKind != "", chq.Eq("kind", eventKind)),
			chq.RawCond("occur_time >= now() - INTERVAL 30 DAY"),
			chq.RawCond(propertyNotEmptyClause, notEmptyArgs...),
		).
		Limit(int64(PropertyValuesLimit))
	return q.Build()
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
// Profile properties are stored in a JSON-typed column and accessed via chq.ProfilePropertyExpr,
// which projects any underlying type into a Nullable(String) coalesced to the empty string
// for missing paths.
//
// Reads raw `profiles`, not the `latest_profiles` CTE that `getSingle`/`List`
// use — matches event-side typeahead semantics (BuildPropertyValuesQuery for
// events also reads raw `events`). Typeahead surfaces historical values: a
// property whose value changed over time appears as multiple distinct entries
// until ReplacingMergeTree background merges complete. Acceptable for typeahead
// (showing the user every value they've ever stored) but worth knowing.
//
// The `!= ”` filter collapses two cases that the underlying JSON column can
// distinguish but the string projection cannot: properties absent from a
// profile and properties stored as the literal empty string both surface as
// "" after the coalesce. Both are excluded from the returned distinct-values
// set; callers needing to surface stored empty strings as a value would need
// a different projection.
//
// SAFETY: propertyKey is validated via chq.ValidateProfilePropertyName before
// interpolation, in addition to the proto regex on
// GetPropertyValuesRequest.property_key.
func BuildProfilePropertyValuesQuery(projectID, propertyKey string) (string, []any, error) {
	if err := chq.ValidateProfilePropertyName(propertyKey); err != nil {
		return "", nil, err
	}
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
		Select(
			"key",
			// A property key can have rows for multiple value_types when the
			// underlying values drift across types over time (rare in practice).
			// argMin(value_type, last_seen) deterministically returns the
			// value_type observed at the earliest last_seen timestamp in the
			// group — first-touch semantics, matching the funnel/retention
			// breakdown rule. Stable across calls so dashboards don't flicker
			// on every refresh when a key has mixed types.
			//
			// The max(last_seen) projection is aliased to last_seen_max (not
			// last_seen) because ClickHouse resolves a bare `last_seen` inside
			// the same SELECT to the aggregate alias rather than the column,
			// which would re-enter argMin and trip "aggregate inside aggregate".
			"argMin(value_type, last_seen) AS value_type",
			"sum(event_count) AS count",
			"max(last_seen) AS last_seen_max",
		).
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
// Returns an error for UNSPECIFIED or undefined enum values. The RPC interceptor rejects UNSPECIFIED
// at the field level via not_in:[0], so this only fires for direct callers (workers, scripts) that
// bypass validation, or for future enum values not yet wired into this switch.
func granularityFunc(g insightsv1.Granularity) (string, error) {
	switch g {
	case insightsv1.Granularity_GRANULARITY_MINUTE:
		return "toStartOfMinute", nil
	case insightsv1.Granularity_GRANULARITY_HOUR:
		return "toStartOfHour", nil
	case insightsv1.Granularity_GRANULARITY_DAY:
		return "toStartOfDay", nil
	case insightsv1.Granularity_GRANULARITY_WEEK:
		return "toStartOfWeek", nil
	case insightsv1.Granularity_GRANULARITY_MONTH:
		return "toStartOfMonth", nil
	default:
		return "", fmt.Errorf("unsupported granularity %v", g)
	}
}

// aggregationType returns the AggregationType for the request, preferring the first event's type.
func aggregationType(req *insightsv1.QueryRequest) insightsv1.AggregationType {
	if len(req.GetSpec().GetEvents()) > 0 {
		agg := req.GetSpec().GetEvents()[0].GetAggregation()
		if agg != insightsv1.AggregationType_AGGREGATION_TYPE_UNSPECIFIED {
			return agg
		}
	}
	return insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL
}

// aggregationExpr returns the SQL aggregation expression for the given type and optional property.
// For TOTAL/UNIQUE_USERS/PER_USER_AVG, the property parameter is unused.
// For SUM/AVG/MIN/MAX, property is required (enforced by proto validation at the RPC boundary
// via `event_query.property_required_for_numeric_agg`).
//
// WARNING for direct callers (workers, scripts) bypassing the RPC interceptor: passing an empty
// property with any numeric aggregation (SUM/AVG/MIN/MAX) produces valid SQL that silently
// returns 0 rather than erroring. For SUM the generated expression is:
//
//	sum(toFloat64OrNull(ifNull(nullIf(auto_properties[EMPTY], EMPTY), custom_properties[EMPTY])))
//
// where EMPTY is the SQL empty-string literal (two single-quotes). For an empty property name:
// auto_properties[EMPTY] returns empty string, nullIf maps empty → NULL, ifNull falls back to
// custom_properties[EMPTY] (also empty), toFloat64OrNull(empty) returns NULL, sum(NULL,…) = 0.
// AVG/MIN/MAX wrap the same toFloat64OrNull(...) in ifNull(agg(...), 0) (see switch arms below)
// so all-NULL inputs collapse to 0 too — same observable result, different mechanism. Pre-validate
// or accept the silent-zero behavior.
//
// The AVG/MIN/MAX ifNull(..., 0) wrapper is also load-bearing for non-numeric data: if all
// property values fail toFloat64OrNull (e.g. strings), the aggregate returns NULL and the wrapper
// coerces it to 0. SUM doesn't need the wrapper because ClickHouse sum() returns 0 for all-NULL
// natively. Either way: "no data" and "actual zero" are indistinguishable in the result —
// consumers should check event counts separately if the distinction matters.
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
			return "", fmt.Errorf("unsupported aggregation type %s", agg)
		}
	case insightsv1.AggregationType_AGGREGATION_TYPE_UNIQUE_USERS:
		// uniq() is an approximate distinct count (HyperLogLog, ~0.5–2% error),
		// chosen over count(DISTINCT) — which resolves to exact uniqExact under the
		// default count_distinct_implementation — for bounded memory and speed on
		// high-cardinality, breakdown, and long-range queries. uniq() is exact for
		// small cohorts. Do not reuse these expressions for billing/quota counts.
		return "toFloat64(uniq(distinct_id))", nil
	case insightsv1.AggregationType_AGGREGATION_TYPE_PER_USER_AVG:
		return "if(uniq(distinct_id) = 0, 0, toFloat64(count(*)) / toFloat64(uniq(distinct_id)))", nil
	default: // TOTAL and UNSPECIFIED
		return "toFloat64(count(*))", nil
	}
}

// aggregationExprIf returns a conditional aggregate expression suitable for a single-scan
// multi-event trends query (countIf, uniqIf, sumIf, …).
func aggregationExprIf(cond chq.Condition, agg insightsv1.AggregationType, property string) (expr string, args []any, err error) {
	if cond.IsZero() {
		return "", nil, fmt.Errorf("empty event condition")
	}
	c := cond.SQL()
	a := cond.Args()
	switch agg {
	case insightsv1.AggregationType_AGGREGATION_TYPE_SUM,
		insightsv1.AggregationType_AGGREGATION_TYPE_AVG,
		insightsv1.AggregationType_AGGREGATION_TYPE_MIN,
		insightsv1.AggregationType_AGGREGATION_TYPE_MAX:
		numeric := "toFloat64OrNull(" + chq.PropertyExpr(property) + ")"
		switch agg {
		case insightsv1.AggregationType_AGGREGATION_TYPE_SUM:
			return "sumIf(" + numeric + ", " + c + ")", a, nil
		case insightsv1.AggregationType_AGGREGATION_TYPE_AVG:
			return "ifNull(avgIf(" + numeric + ", " + c + "), 0)", a, nil
		case insightsv1.AggregationType_AGGREGATION_TYPE_MIN:
			return "ifNull(minIf(" + numeric + ", " + c + "), 0)", a, nil
		case insightsv1.AggregationType_AGGREGATION_TYPE_MAX:
			return "ifNull(maxIf(" + numeric + ", " + c + "), 0)", a, nil
		default:
			return "", nil, fmt.Errorf("unsupported aggregation type %s", agg)
		}
	case insightsv1.AggregationType_AGGREGATION_TYPE_UNIQUE_USERS:
		return "toFloat64(uniqIf(distinct_id, " + c + "))", a, nil
	case insightsv1.AggregationType_AGGREGATION_TYPE_PER_USER_AVG:
		expr = "if(uniqIf(distinct_id, " + c + ") = 0, 0, toFloat64(countIf(" + c + ")) / toFloat64(uniqIf(distinct_id, " + c + ")))"
		args = make([]any, 0, len(a)*3)
		args = append(args, a...)
		args = append(args, a...)
		args = append(args, a...)
		return expr, args, nil
	default: // TOTAL and UNSPECIFIED
		return "toFloat64(countIf(" + c + "))", a, nil
	}
}
