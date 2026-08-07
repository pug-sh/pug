package insights

import (
	"fmt"
	"slices"
	"time"

	chq "github.com/pug-sh/pug/internal/core/clickhouse"
	insightsv1 "github.com/pug-sh/pug/internal/gen/proto/shared/insights/v1"
)

const sessionRollupTable = "dashboard_session_rollup"

// Session-rollup entry/exit dimensions, grouped by the migration that
// introduced them, on the same freeze rule as the event rollup's groups.
var (
	// sessionRollupDims007 are migration 007's originals (TestMigration007Frozen).
	sessionRollupDims007 = []string{
		"$url",
		"$country", "$region", "$city",
		"$os", "$browser", "$device", "$platform",
		"$utmSource", "$utmMedium", "$utmCampaign",
	}
	// sessionRollupDims010 are the dims migration 010 added, and exactly the
	// entry/exit state pairs its partial-column backfill INSERT may carry:
	// the omitted state columns rely on empty-state merge identity, so listing
	// an older group's state there would corrupt merged history (see the
	// migration header). TestMigration010SessionRollupColumnsMatchDims.
	sessionRollupDims010 = []string{
		"$pathname", "$referrerDomain", "$channel", "$utmTerm", "$utmContent",
	}
)

// sessionMaterializedDims are the entry/exit breakdown dimensions backed by
// the session-grain rollup MV: the union of every applied migration's group,
// matching the LATEST MV definition (010's MODIFY QUERY) by construction.
// TestMigration010SessionRollupColumnsMatchDims pins the column names against
// that MV and TestMigration010SessionRollupDimExprsMatch pins the
// argMin/argMaxState value expressions against the raw builder's projection.
var sessionMaterializedDims = slices.Concat(sessionRollupDims007, sessionRollupDims010)

func isSessionMaterializedDim(prop string) bool {
	return slices.Contains(sessionMaterializedDims, prop)
}

// canUseSessionRollup reports whether a session trends/segmentation query can be
// served from the session-grain rollup. Conservative by construction: anything it
// rejects falls back to BuildSessionTrendsQuery/BuildSessionSegmentationQuery,
// which compute the same full-session, keyed-on-start metric over raw events with
// identical results.
//
// Accepted accuracy caveat for the rollup-served path: BOUNCE_RATE can under-report
// bounces relative to the raw builders under duplicate event delivery. The events
// table is ReplacingMergeTree and collapses redeliveries on merge, so the raw path's
// count() per session self-corrects; the rollup's event_count_state is countState()
// keyed on (project_id, kind, session_id) WITHOUT event_id (migration 007), so a
// duplicate insert is retained permanently and a genuinely single-event session can
// read event_count > 1 — i.e. no longer counted as a bounce. The drift equals the
// pipeline's redelivery rate (monotonic, never self-correcting). SESSIONS, ENTRY,
// and EXIT are immune (they count session-id groups, not events); AVG_DURATION is
// immune (min/max occur_time are idempotent under duplicates);
// AVG_EVENTS_PER_SESSION inflates on the rollup path for the same countState
// reason while the raw path self-corrects. This is an accepted, bounded
// inaccuracy for dashboard visualization — see docs/architecture/clickhouse.md;
// pinned by TestIntegration/session_rollup_bounce_duplicate_overcount_documented.
func canUseSessionRollup(spec *insightsv1.InsightQuerySpec, gran insightsv1.Granularity) bool {
	session := spec.GetSession()
	if session == nil {
		return false
	}

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

	if len(spec.GetFilterGroups()) != 0 || len(spec.GetEvents()) != 0 {
		return false
	}
	// The rollup is keyed only on `kind` (the MV stores an all-events row with
	// kind='' plus one row per kind). A scope carrying property filters can't be
	// satisfied from those pre-aggregated states, so fall back to the raw builder,
	// which applies the filters per event. Same result, no fast path.
	if session.GetScope() != nil && len(session.GetScope().GetFilters()) != 0 {
		return false
	}

	bds := spec.GetBreakdowns()
	if len(bds) > 1 {
		return false
	}
	if len(bds) == 1 && !isSessionMaterializedDim(bds[0].GetProperty()) {
		return false
	}

	switch session.GetMetric() {
	case insightsv1.SessionMetric_SESSION_METRIC_SESSIONS,
		insightsv1.SessionMetric_SESSION_METRIC_AVG_DURATION,
		insightsv1.SessionMetric_SESSION_METRIC_BOUNCE_RATE,
		insightsv1.SessionMetric_SESSION_METRIC_AVG_EVENTS_PER_SESSION:
		return true
	case insightsv1.SessionMetric_SESSION_METRIC_ENTRY,
		insightsv1.SessionMetric_SESSION_METRIC_EXIT:
		return spec.GetInsightType() == insightsv1.InsightType_INSIGHT_TYPE_TRENDS && len(bds) == 1
	default:
		return false
	}
}

func buildSessionTrendsFromRollup(req *insightsv1.QueryRequest, projectID string) (TrendsQuery, error) {
	granFn, err := granularityFunc(req.GetGranularity())
	if err != nil {
		return TrendsQuery{}, fmt.Errorf("session trends rollup: %w", err)
	}
	sessionsCTE, err := buildSessionRollupRowsCTE(req, projectID)
	if err != nil {
		return TrendsQuery{}, fmt.Errorf("session trends rollup: %w", err)
	}
	metricExpr, err := sessionMetricAggExpr(req.GetSpec().GetSession().GetMetric())
	if err != nil {
		return TrendsQuery{}, fmt.Errorf("session trends rollup: %w", err)
	}

	breakdowns := req.GetSpec().GetBreakdowns()
	selectExprs := []string{
		fmt.Sprintf("%s(start_time) AS t", granFn),
		sqlStringLiteral(sessionEventKind(req.GetSpec().GetSession())) + " AS event_kind",
	}
	groupByCols := []string{"t"}
	orderByCols := []string{"t ASC", "event_kind ASC"}
	for j := range breakdowns {
		selectExprs = append(selectExprs, fmt.Sprintf("breakdown_%d", j))
		groupByCols = append(groupByCols, fmt.Sprintf("breakdown_%d", j))
		orderByCols = append(orderByCols, fmt.Sprintf("breakdown_%d ASC", j))
	}
	selectExprs = append(selectExprs, metricExpr+" AS value")

	sql, args, err := chq.NewQuery().
		With("sessions", sessionsCTE).
		Select(selectExprs...).
		From("sessions").
		GroupBy(groupByCols...).
		OrderBy(orderByCols...).
		WithQueryCache(analyticsCacheTTL).
		Build()
	if err != nil {
		return TrendsQuery{}, fmt.Errorf("session trends rollup: %w", err)
	}
	return TrendsQuery{
		sql:            sql,
		args:           args,
		properties:     breakdownProps(breakdowns),
		breakdownLimit: effectiveBreakdownLimit(req.GetSpec().GetBreakdownLimit()),
	}, nil
}

func buildSessionSegmentationFromRollup(req *insightsv1.QueryRequest, projectID string) (ScalarQuery, error) {
	sessionsCTE, err := buildSessionRollupRowsCTE(req, projectID)
	if err != nil {
		return ScalarQuery{}, fmt.Errorf("session segmentation rollup: %w", err)
	}
	metricExpr, err := sessionMetricAggExpr(req.GetSpec().GetSession().GetMetric())
	if err != nil {
		return ScalarQuery{}, fmt.Errorf("session segmentation rollup: %w", err)
	}
	sql, args, err := chq.NewQuery().
		With("sessions", sessionsCTE).
		Select(metricExpr + " AS value").
		From("sessions").
		WithQueryCache(analyticsCacheTTL).
		Build()
	if err != nil {
		return ScalarQuery{}, fmt.Errorf("session segmentation rollup: %w", err)
	}
	return ScalarQuery{sql: sql, args: args}, nil
}

func buildSessionRollupRowsCTE(req *insightsv1.QueryRequest, projectID string) (*chq.Query, error) {
	session := req.GetSpec().GetSession()
	if session == nil {
		return nil, fmt.Errorf("session is required")
	}

	kind := ""
	if session.GetScope() != nil {
		kind = session.GetScope().GetKind()
	}

	q := chq.NewQuery().
		Select(
			"session_id",
			"minMerge(start_state) AS start_time",
			"maxMerge(end_state) AS end_time",
			"countMerge(event_count_state) AS event_count",
		).
		From(sessionRollupTable).
		Where(
			chq.Eq("project_id", projectID),
			chq.Eq("kind", kind),
		).
		GroupBy("session_id").
		HavingExpr("start_time >= ? AND start_time < ?", req.GetTimeRange().GetFrom().AsTime(), req.GetTimeRange().GetTo().AsTime())

	breakdowns := req.GetSpec().GetBreakdowns()
	if len(breakdowns) == 1 {
		stateName, err := sessionBreakdownStateName(session.GetMetric(), breakdowns[0].GetProperty())
		if err != nil {
			return nil, err
		}
		q.Select(fmt.Sprintf("%s(%s) AS breakdown_0", sessionBreakdownMergeFunc(session.GetMetric()), stateName))
	}
	return q, nil
}

func sessionBreakdownMergeFunc(metric insightsv1.SessionMetric) string {
	if metric == insightsv1.SessionMetric_SESSION_METRIC_EXIT {
		return "argMaxMerge"
	}
	return "argMinMerge"
}

func sessionBreakdownStateName(metric insightsv1.SessionMetric, prop string) (string, error) {
	suffix, ok := sessionRollupDimSuffix(prop)
	if !ok {
		return "", fmt.Errorf("unsupported session rollup breakdown %q", prop)
	}
	prefix := "entry"
	if metric == insightsv1.SessionMetric_SESSION_METRIC_EXIT {
		prefix = "exit"
	}
	return prefix + "_" + suffix + "_state", nil
}

// sessionRollupDimSuffix maps a session breakdown dimension to the middle of
// its rollup state-column names (entry_<suffix>_state / exit_<suffix>_state).
// The suffix IS the events promoted column, so it is read off the
// authoritative promotedAutoColumns table rather than restated here; a dim
// listed in sessionMaterializedDims but not backed by a promoted column
// therefore reports false instead of naming a column migration 010 never
// created. Both gates matter: without the membership check every promoted
// property would yield a suffix, including ones with no session state pair.
func sessionRollupDimSuffix(prop string) (string, bool) {
	if !isSessionMaterializedDim(prop) {
		return "", false
	}
	return chq.PromotedColumnFor(prop)
}

func sessionTrendsQueryForExecution(req *insightsv1.QueryRequest, projectID string, now time.Time) (TrendsQuery, bool, error) {
	if utcBucketing(req.GetTimezone()) && canUseSessionRollup(req.GetSpec(), req.GetGranularity()) && rollupWindowAligned(req.GetTimeRange(), now) {
		q, err := buildSessionTrendsFromRollup(req, projectID)
		return q, true, err
	}
	q, err := BuildSessionTrendsQuery(req, projectID)
	return q, false, err
}

func sessionSegmentationQueryForExecution(req *insightsv1.QueryRequest, projectID string, now time.Time) (ScalarQuery, bool, error) {
	if utcBucketing(req.GetTimezone()) && canUseSessionRollup(req.GetSpec(), req.GetGranularity()) && rollupWindowAligned(req.GetTimeRange(), now) {
		q, err := buildSessionSegmentationFromRollup(req, projectID)
		return q, true, err
	}
	q, err := BuildSessionSegmentationQuery(req, projectID)
	return q, false, err
}
