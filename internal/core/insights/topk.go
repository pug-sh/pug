package insights

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"

	chq "github.com/pug-sh/pug/internal/core/clickhouse"
	"github.com/pug-sh/pug/internal/core/profiles"
	"github.com/pug-sh/pug/internal/deps/telemetry"
	commonv1 "github.com/pug-sh/pug/internal/gen/proto/common/v1"
	insightsv1 "github.com/pug-sh/pug/internal/gen/proto/shared/insights/v1"
	"github.com/pug-sh/pug/internal/slogx"
)

// defaultTopKLimit mirrors the default breakdown top-N of 10.
const defaultTopKLimit = 10

// topKOthersValue is the synthetic dimension value for the overflow bucket.
// Clients identify the bucket by TopKRow.is_others, not this literal — a real
// dimension value can legitimately be the string "$others".
const topKOthersValue = "$others"

// TopKQuery is the compiled SQL for a top-K ranking insight.
type TopKQuery struct {
	sql       string
	args      []any
	limit     int
	dimension insightsv1.TopKQuery_Dimension
}

func (q TopKQuery) SQL() string                               { return q.sql }
func (q TopKQuery) Args() []any                               { return q.args }
func (q TopKQuery) Limit() int                                { return q.limit }
func (q TopKQuery) Dimension() insightsv1.TopKQuery_Dimension { return q.dimension }

// BuildTopKQuery builds the ranked raw-events top-K query for the request's
// dimension, dispatching to buildTopKEvents (PROPERTY/EVENT_KIND) or
// buildTopKUsers (USER). Execution-time dispatch (topKQueryForExecution in
// rollup.go) may route eligible PROPERTY/EVENT_KIND queries to the rollup
// instead; this builder stays a pure raw-events builder like BuildTrendsQuery.
// Each shape documents its own structure and $others handling.
//
// The top_vals sort key is numeric (Float64 aggExpr) with a String dim_value
// tie-break, never DateTime64/UUID — that sidesteps the mixed-type
// __topKFilter dynamic-filtering issue, so DisableTopKDynamicFiltering is not
// needed. TestBuildTopKQuery_PropertyPromoted pins the numeric inner ORDER BY.
func BuildTopKQuery(req *insightsv1.QueryRequest, projectID string) (TopKQuery, error) {
	tk := req.GetSpec().GetTopK()
	if tk == nil {
		return TopKQuery{}, fmt.Errorf("top k: top_k is required")
	}
	limit := int(tk.GetLimit())
	if limit == 0 {
		limit = defaultTopKLimit
	}

	var q *chq.Query
	var err error
	switch tk.GetDimension() {
	case insightsv1.TopKQuery_DIMENSION_PROPERTY, insightsv1.TopKQuery_DIMENSION_EVENT_KIND:
		q, err = buildTopKEvents(req, projectID, limit)
	case insightsv1.TopKQuery_DIMENSION_USER:
		q, err = buildTopKUsers(req, projectID, limit)
	default:
		return TopKQuery{}, fmt.Errorf("top k: unsupported dimension %s", tk.GetDimension())
	}
	if err != nil {
		return TopKQuery{}, fmt.Errorf("top k: %w", err)
	}

	sql, args, err := q.
		WithQueryCache(analyticsCacheTTL).
		WithSpillThreshold(insightsSpillThresholdBytes).
		Build()
	if err != nil {
		return TopKQuery{}, fmt.Errorf("top k: %w", err)
	}
	return TopKQuery{
		sql:       sql,
		args:      args,
		limit:     limit,
		dimension: tk.GetDimension(),
	}, nil
}

// topKBaseConditions returns the WHERE conditions shared by the top_vals CTE
// and the outer aggregation: project/time window, optional event scope, and
// top-level filter groups (including profile-source filters, which compile to
// a distinct_id IN (profiles ∪ aliases) subquery valid in any events WHERE).
func topKBaseConditions(req *insightsv1.QueryRequest, projectID, alias string) ([]chq.Condition, error) {
	spec := req.GetSpec()
	tk := spec.GetTopK()

	prefix := ""
	if alias != "" {
		prefix = alias + "."
	}

	conds := []chq.Condition{
		chq.Eq(prefix+"project_id", projectID),
		chq.Gte(prefix+"occur_time", req.GetTimeRange().GetFrom().AsTime()),
		chq.Lt(prefix+"occur_time", req.GetTimeRange().GetTo().AsTime()),
	}

	// User-counting metrics exclude cookieless ids unless opted in; a top-K of
	// USERS ranks people themselves, so it is person-based regardless of metric.
	excludeCookieless := excludeCookielessForAgg(spec, topKMetric(tk)) ||
		(tk.GetDimension() == insightsv1.TopKQuery_DIMENSION_USER && excludeCookielessForPersons(spec))
	conds = append(conds, cookielessExclusionCond(excludeCookieless, alias))

	if tk.GetScope() != nil {
		scopeCond, err := chq.EventConditionAliased([]*commonv1.EventFilter{tk.GetScope()}, projectID, alias)
		if err != nil {
			return nil, fmt.Errorf("scope: %w", err)
		}
		conds = append(conds, scopeCond)
	}

	filterCond, err := buildTopLevelFilterCondition(spec.GetFilterGroups(), spec.GetFilterGroupsOperator(), projectID, alias)
	if err != nil {
		return nil, err
	}
	conds = append(conds, filterCond)
	return conds, nil
}

// buildTopKEvents builds the PROPERTY / EVENT_KIND shape: a single GROUP BY
// over events, ranked in the top_vals CTE, re-aggregated with the dimension
// collapsed to $others outside the top K.
//
// This shape is deliberately two-pass (top_vals CTE scans events, then the outer
// query scans them again) — the opposite choice from buildTopKUsers' single-pass
// row_number ranking. The asymmetry is intentional: there is no join or
// latest_profiles aggregation to repeat here, so the second scan is just a scan,
// and re-scanning yields an exact UNIQUE_USERS for $others "for free" (a true
// uniq over the bucket's events) without threading uniqState/uniqMerge partials
// through a ranked CTE. buildTopKUsers cannot afford a second pass because its
// scan drags a join + profile aggregation, so it pays the partial-state plumbing
// instead. If the raw event scan ever dominates here, revisit with the
// single-pass technique (and add uniqState partials for exact UNIQUE_USERS).
//
// Empty-string dimension values participate as a real row (consistent with
// breakdown behavior — for "top referrers" the empty/direct bucket is usually
// the largest and hiding it would mislead).
func buildTopKEvents(req *insightsv1.QueryRequest, projectID string, limit int) (*chq.Query, error) {
	tk := req.GetSpec().GetTopK()

	var dimExpr string
	switch tk.GetDimension() {
	case insightsv1.TopKQuery_DIMENSION_PROPERTY:
		if tk.GetProperty() == "" {
			return nil, fmt.Errorf("property is required for PROPERTY dimension")
		}
		dimExpr = chq.PropertyExpr(tk.GetProperty())
	case insightsv1.TopKQuery_DIMENSION_EVENT_KIND:
		dimExpr = "kind"
	default:
		return nil, fmt.Errorf("unsupported dimension %s", tk.GetDimension())
	}

	aggExpr, err := aggregationExpr(topKMetric(tk), tk.GetMetricProperty())
	if err != nil {
		return nil, err
	}

	// The top_vals CTE and the outer re-aggregation scan the same window-pruned
	// events filter, so they share one condition set.
	conds, err := topKBaseConditions(req, projectID, "")
	if err != nil {
		return nil, err
	}

	// Omit-$others fast path: a single aggregation with LIMIT is the natural
	// "top K, no overflow", so the two-pass top_vals CTE is skipped entirely —
	// there is no tail to re-aggregate. is_others stays projected (always 0) so
	// the executor scan and the TopKRow contract are unchanged.
	if tk.GetOmitOthers() {
		return chq.NewQuery().
			Select(
				dimExpr+" AS dim_value",
				"0 AS is_others",
				aggExpr+" AS value",
			).
			From("events").
			Where(conds...).
			GroupBy("dim_value").
			OrderBy("value DESC", "dim_value ASC").
			Limit(int64(limit)), nil
	}

	topVals := chq.NewQuery().
		Select(dimExpr+" AS dim_value").
		From("events").
		Where(conds...).
		GroupBy("dim_value").
		// Tie-break on dim_value ASC so the top-N is deterministic and matches
		// the breakdown bucketing convention (total DESC, value ASC).
		OrderBy(aggExpr+" DESC", "dim_value ASC").
		Limit(int64(limit))

	inTopVals := fmt.Sprintf("%s IN (SELECT dim_value FROM top_vals)", dimExpr)
	return chq.NewQuery().
		With("top_vals", topVals).
		Select(
			fmt.Sprintf("if(%s, %s, '%s') AS dim_value", inTopVals, dimExpr, topKOthersValue),
			fmt.Sprintf("if(%s, 0, 1) AS is_others", inTopVals),
			aggExpr+" AS value",
		).
		From("events").
		Where(conds...).
		GroupBy("dim_value", "is_others").
		OrderBy("is_others ASC", "value DESC", "dim_value ASC"), nil
}

// buildTopKUsers builds the USER shape as a single pass over events:
//
//	per_user: events LEFT ANY JOIN the profile identity union, grouped by the
//	          canonical user key, emitting re-mergeable partial aggregates
//	ranked:   row_number() over per_user by the per-user metric value
//	outer:    rank-split into top-K rows vs the $others bucket, re-merging the
//	          partials (exact for every supported metric — see topKUserMetric)
//
// Unlike the two-pass top_vals shape, nothing here is referenced twice:
// ClickHouse inlines a CTE per reference, so a top_vals CTE over the joined
// scan would re-execute the events scan, the latest_profiles aggregation, and
// the join hash-table build. The ranking sort costs O(users log users) over
// already-aggregated rows — far cheaper than a second scan+join — and spills
// past insightsSpillThresholdBytes. TestBuildTopKQuery_UserDimension pins the
// single-reference shape (one events scan, one profiles read, one join) so a
// regression to a twice-referenced CTE is caught.
//
// The identity union maps every distinct_id of a profile — its id, external_id
// (via one ARRAY JOIN pass so latest_profiles aggregates once, not once per
// union branch), and alias ids — to the canonical profile id; unidentified
// distinct_ids stay as themselves. LEFT ANY JOIN picks one identity row per
// event so pathological mappings (a distinct_id matching multiple identity
// rows, e.g. one profile's external_id colliding with another's alias) cannot
// multiply event rows and inflate metrics; which canonical id wins in that
// case is arbitrary but stable within a query.
func buildTopKUsers(req *insightsv1.QueryRequest, projectID string, limit int) (*chq.Query, error) {
	tk := req.GetSpec().GetTopK()

	metric, err := topKUserMetricExprs(tk)
	if err != nil {
		return nil, err
	}

	conds, err := topKBaseConditions(req, projectID, "e")
	if err != nil {
		return nil, err
	}

	perUserCols := make([]string, len(metric.partials)+1)
	perUserCols[0] = "if(i.profile_id = '', e.distinct_id, i.profile_id) AS user_key"
	rankedCols := make([]string, len(metric.partials)+1)
	rankedCols[0] = "user_key"
	for i, p := range metric.partials {
		perUserCols[i+1] = p.expr + " AS " + p.col
		rankedCols[i+1] = p.col
	}

	perUser := chq.NewQuery().
		Select(perUserCols...).
		From(`events e LEFT ANY JOIN (
SELECT dist_id AS distinct_id, p.id AS profile_id
FROM latest_profiles p
ARRAY JOIN arrayDistinct(arrayFilter(x -> x != '', [p.id, p.external_id])) AS dist_id
WHERE p.is_deleted = 0
UNION ALL
SELECT pa.alias_id AS distinct_id, pa.profile_id AS profile_id
FROM latest_profile_aliases pa
INNER JOIN latest_profiles p ON p.id = pa.profile_id
WHERE p.is_deleted = 0
) i ON i.distinct_id = e.distinct_id`).
		Where(conds...).
		GroupBy("user_key")

	ranked := chq.NewQuery().
		Select(append(
			rankedCols,
			"row_number() OVER (ORDER BY "+metric.rankExpr+" DESC, user_key ASC) AS rn",
		)...).
		From("per_user")

	base := chq.NewQuery().
		With("latest_profiles", profiles.LatestProfilesCTE(projectID)).
		With("latest_profile_aliases", profiles.LatestProfileAliasesCTE(projectID)).
		With("per_user", perUser).
		With("ranked", ranked)

	// Omit-$others fast path: prune to the top `limit` ranked rows instead of
	// bucketing the tail. rn is a window-function alias from the ranked CTE, so
	// it is filtered on this outer scan over ranked (it cannot be referenced in
	// the SELECT that defines it). Each ranked row is one user, so the GROUP BY
	// yields singleton groups and mergeExpr reduces to rankExpr. is_others stays
	// projected (always 0) so the executor scan and the TopKRow contract hold.
	if tk.GetOmitOthers() {
		return base.
			Select(
				"user_key AS dim_value",
				"0 AS is_others",
				metric.mergeExpr+" AS value",
			).
			From("ranked").
			Where(chq.Lte("rn", int64(limit))).
			GroupBy("dim_value", "is_others").
			OrderBy("value DESC", "dim_value ASC"), nil
	}

	return base.
		SelectExpr(fmt.Sprintf("if(rn <= ?, user_key, '%s') AS dim_value", topKOthersValue), int64(limit)).
		SelectExpr("if(rn <= ?, 0, 1) AS is_others", int64(limit)).
		Select(metric.mergeExpr+" AS value").
		From("ranked").
		GroupBy("dim_value", "is_others").
		OrderBy("is_others ASC", "value DESC", "dim_value ASC"), nil
}

// topKMetric resolves the request metric, defaulting UNSPECIFIED to TOTAL.
func topKMetric(tk *insightsv1.TopKQuery) insightsv1.AggregationType {
	if m := tk.GetMetric(); m != insightsv1.AggregationType_AGGREGATION_TYPE_UNSPECIFIED {
		return m
	}
	return insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL
}

// topKPartial is one re-mergeable per-user aggregate: its expression and the
// column alias it is projected as. Pairing expr+col in a single struct makes the
// per_user-projection and ranked-carry-through slices share one source, so a
// *length* mismatch between them can no longer arise. It does not — and cannot,
// for string-built SQL — guarantee that a metric's rankExpr/mergeExpr reference
// only the cols declared here; that coupling stays prose- and test-enforced.
type topKPartial struct {
	expr string // aggregate projection in per_user, e.g. "sum(...)"
	col  string // the alias it is projected AS, carried through the ranked CTE
}

// topKUserMetric holds the SQL pieces of a USER-dimension metric in
// re-mergeable form, so the single-pass query can rank users by their final
// value and still compute the $others bucket exactly from per-user partials:
//
//   - partials: the per-user aggregate projections (expr + carried column);
//     each becomes "expr AS col" in per_user and "col" in the ranked CTE
//   - rankExpr: the per-user metric value (over partials) used for ranking;
//     identical in semantics to aggregationExpr's per-group value, including
//     the AVG/MIN/MAX collapse of all-NULL groups to 0
//   - mergeExpr: the metric over a *group* of users (over the same partials).
//     For a top row the group is one user and mergeExpr reduces to rankExpr;
//     for $others it is the exact metric over the bucket's raw events: sums
//     add, AVG re-divides summed numerator/denominator, MIN/MAX re-minimize
//     (NULL partials skipped by min/max, matching raw NULL-skipping)
type topKUserMetric struct {
	partials  []topKPartial
	rankExpr  string
	mergeExpr string
}

// topKUserMetricExprs builds the topKUserMetric for the request's metric.
//
// UNIQUE_USERS and PER_USER_AVG are rejected by proto CEL at the RPC boundary
// (each USER group is a single user, so they are degenerate); the error here is
// defensive for direct callers.
func topKUserMetricExprs(tk *insightsv1.TopKQuery) (topKUserMetric, error) {
	metric := topKMetric(tk)
	if metric == insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL {
		return topKUserMetric{
			partials:  []topKPartial{{expr: "count(*)", col: "cnt"}},
			rankExpr:  "toFloat64(cnt)",
			mergeExpr: "toFloat64(sum(cnt))",
		}, nil
	}

	if tk.GetMetricProperty() == "" {
		return topKUserMetric{}, fmt.Errorf("metric_property is required for %s", metric)
	}
	num := "toFloat64OrNull(" + chq.PropertyExprAliased(tk.GetMetricProperty(), "e") + ")"

	switch metric {
	case insightsv1.AggregationType_AGGREGATION_TYPE_SUM:
		return topKUserMetric{
			partials:  []topKPartial{{expr: "sum(" + num + ")", col: "sum_num"}},
			rankExpr:  "sum_num",
			mergeExpr: "sum(sum_num)",
		}, nil
	case insightsv1.AggregationType_AGGREGATION_TYPE_AVG:
		// avg is not re-mergeable from per-user avgs (users contribute different
		// event counts), so carry numerator + non-NULL denominator separately.
		return topKUserMetric{
			partials: []topKPartial{
				{expr: "sum(" + num + ")", col: "sum_num"},
				{expr: "count(" + num + ")", col: "cnt_num"},
			},
			rankExpr:  "if(cnt_num = 0, 0, sum_num / cnt_num)",
			mergeExpr: "if(sum(cnt_num) = 0, 0, sum(sum_num) / sum(cnt_num))",
		}, nil
	case insightsv1.AggregationType_AGGREGATION_TYPE_MIN:
		return topKUserMetric{
			partials:  []topKPartial{{expr: "min(" + num + ")", col: "min_num"}},
			rankExpr:  "ifNull(min_num, 0)",
			mergeExpr: "ifNull(min(min_num), 0)",
		}, nil
	case insightsv1.AggregationType_AGGREGATION_TYPE_MAX:
		return topKUserMetric{
			partials:  []topKPartial{{expr: "max(" + num + ")", col: "max_num"}},
			rankExpr:  "ifNull(max_num, 0)",
			mergeExpr: "ifNull(max(max_num), 0)",
		}, nil
	default:
		return topKUserMetric{}, fmt.Errorf("unsupported metric %s for USER dimension", metric)
	}
}

// BuildTopKProfilesQuery builds the enrichment lookup for the winning user
// keys: the latest external_id and properties of each (non-deleted) profile
// whose id is in ids. Keys that are raw distinct_ids (no profile) simply match
// no row. Returns empty SQL when ids is empty — callers skip the query.
//
// Intentionally not query-cached: freshness over compute at K ≤ 100, matching
// the property-values / segment-users cache policy.
//
// This deliberately re-derives the "latest profile row" semantics inline (argMax
// by insert_time + an is_deleted HAVING over the raw `profiles` table) instead of
// reusing profiles.LatestProfilesCTE, so the `id IN (...)` filter is pushed into
// the scan and only the ≤K winning ids are grouped — far cheaper than
// materializing every project profile through the CTE just to keep K of them. The
// cost is a second copy of the ReplacingMergeTree "latest" rule: if that
// dedup/tie-break convention changes, update LatestProfilesCTE AND this query.
func BuildTopKProfilesQuery(projectID string, ids []string) (string, []any, error) {
	if len(ids) == 0 {
		return "", nil, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?, ", len(ids)), ", ")
	inArgs := make([]any, len(ids))
	for i, id := range ids {
		inArgs[i] = id
	}
	return chq.NewQuery().
		Select(
			"id",
			"argMax(external_id, insert_time) AS external_id",
			// toJSONString keeps the scan a plain string; the executor decodes
			// it into map[string]any without the chcol.JSON machinery.
			"toJSONString(argMax(properties, insert_time)) AS properties_json",
		).
		From("profiles").
		Where(
			chq.Eq("project_id", projectID),
			chq.RawCond("id IN ("+placeholders+")", inArgs...),
		).
		GroupBy("id").
		Having("argMax(is_deleted, insert_time) = 0").
		Build()
}

// buildTopKResult translates executor rows into the proto result, attaching
// profile enrichment to USER-dimension rows whose key resolved to a profile.
// The $others bucket and unidentified distinct_ids stay un-enriched. Row order
// is preserved from SQL: top rows metric-descending, $others last. A property
// conversion failure degrades that one row to an un-propertied profile (see the
// inline note) rather than aborting the query.
func buildTopKResult(ctx context.Context, executor *Executor, projectID string, q TopKQuery, rows []TopKRow) (*insightsv1.TopKResult, error) {
	var profilesByID map[string]TopKProfileRow
	if q.Dimension() == insightsv1.TopKQuery_DIMENSION_USER {
		ids := make([]string, 0, len(rows))
		for _, r := range rows {
			if !r.IsOthers {
				ids = append(ids, r.DimensionValue)
			}
		}
		if len(ids) > 0 {
			sql, args, err := BuildTopKProfilesQuery(projectID, ids)
			if err != nil {
				// ids come from our own query output, so a build failure is an
				// internal bug, not client input.
				slog.ErrorContext(ctx, "failed to build top k profiles query", slogx.Error(err),
					slog.String("project_id", projectID))
				telemetry.RecordError(ctx, err)
				return nil, err
			}
			profilesByID, err = executor.QueryTopKProfiles(ctx, projectID, sql, args)
			if err != nil {
				if isContextError(err) {
					// Request cancellation / deadline is a lifecycle signal, not a
					// transient enrichment fault: propagate it so queryFailed and
					// dashboards.renderInsightTile surface CodeCanceled /
					// CodeDeadlineExceeded rather than masking it as a 200 with
					// partial (un-enriched) data.
					return nil, err
				}
				// Enrichment is strictly additive to an already-complete ranking:
				// QueryTopK has produced the ranked rows + values; profile id /
				// external_id / properties only decorate the USER rows. A transient
				// failure of this second query (already logged+recorded inside
				// QueryTopKProfiles) degrades to un-enriched rows rather than
				// discarding a good ranking — matching the per-row property-decode
				// degradation below. The BuildTopKProfilesQuery failure above stays
				// fatal on purpose: it builds from our own query output, so a failure
				// there signals a code bug, not a transient operational fault.
				slog.WarnContext(ctx, "top k profile enrichment query failed; returning un-enriched rows",
					slogx.Error(err), slog.String("project_id", projectID))
				profilesByID = nil
			}
		}
	}

	result := &insightsv1.TopKResult{Rows: make([]*insightsv1.TopKRow, 0, len(rows))}
	for _, r := range rows {
		row := &insightsv1.TopKRow{
			DimensionValue: proto.String(r.DimensionValue),
			Value:          proto.Float64(r.Value),
			IsOthers:       proto.Bool(r.IsOthers),
		}
		if p, ok := profilesByID[r.DimensionValue]; ok && !r.IsOthers {
			prof := &insightsv1.TopKProfile{Id: proto.String(p.ID), ExternalId: proto.String(p.ExternalID)}
			if len(p.Properties) > 0 {
				props, err := structpb.NewStruct(p.Properties)
				if err != nil {
					// Should be unreachable: ClickHouse property keys are always
					// strings, and json.Unmarshal into map[string]any only yields
					// types structpb.NewStruct accepts. Degrade to an un-propertied
					// profile rather than failing the whole query.
					slog.ErrorContext(ctx, "failed to convert top k profile properties", slogx.Error(err),
						slog.String("project_id", projectID), slog.String("profile_id", p.ID))
					telemetry.RecordError(ctx, err)
				} else {
					prof.Properties = props
				}
			}
			row.Profile = prof
		}
		result.Rows = append(result.Rows, row)
	}
	return result, nil
}
