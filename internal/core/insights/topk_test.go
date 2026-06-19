package insights_test

import (
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	commonv1 "github.com/pug-sh/pug/internal/gen/proto/common/v1"
	insightsv1 "github.com/pug-sh/pug/internal/gen/proto/shared/insights/v1"

	"github.com/pug-sh/pug/internal/core/insights"
)

func topKRequest(tk *insightsv1.TopKQuery) *insightsv1.QueryRequest {
	return &insightsv1.QueryRequest{
		Spec: &insightsv1.InsightQuerySpec{
			InsightType: insightsv1.InsightType_INSIGHT_TYPE_TOP_K.Enum(),
			TopK:        tk,
		},
		TimeRange:   timeRange("2024-01-01T00:00:00Z", "2024-01-07T00:00:00Z"),
		Granularity: insightsv1.Granularity_GRANULARITY_DAY.Enum(),
	}
}

// TestBuildTopKQuery_PropertyPromoted verifies the PROPERTY-dimension shape for
// a promoted auto property: top_vals CTE, $others collapse keyed on is_others,
// query cache, and the default limit of 10.
func TestBuildTopKQuery_PropertyPromoted(t *testing.T) {
	req := topKRequest(&insightsv1.TopKQuery{
		Dimension: insightsv1.TopKQuery_DIMENSION_PROPERTY.Enum(),
		Property:  proto.String("$browser"),
	})

	q, err := insights.BuildTopKQuery(req, "proj_123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	sql, args := q.SQL(), q.Args()

	for _, want := range []string{
		"top_vals AS (",
		"'$others'",
		"AS is_others",
		"GROUP BY dim_value, is_others",
		// Inner top_vals sort key is (Float64, String) — pins the numeric ORDER BY
		// the DisableTopKDynamicFiltering-not-needed rationale in topk.go relies on.
		"ORDER BY toFloat64(count(*)) DESC, dim_value ASC",
		"ORDER BY is_others ASC, value DESC, dim_value ASC",
		"SETTINGS use_query_cache = 1, query_cache_ttl = 60, max_bytes_before_external_group_by = 1073741824, max_bytes_before_external_sort = 1073741824",
		// $browser is a promoted column, not a map access.
		"browser",
		"toFloat64(count(*))", // metric defaults to TOTAL
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("expected %q in SQL, got: %s", want, sql)
		}
	}
	if strings.Contains(sql, "auto_properties['$browser']") {
		t.Errorf("expected promoted column read for $browser, got map access: %s", sql)
	}

	// Cache tenant isolation: project_id must be a positional arg (once per scan).
	projCount := 0
	limitSeen := false
	for _, a := range args {
		if a == "proj_123" {
			projCount++
		}
		if a == int64(10) {
			limitSeen = true
		}
	}
	if projCount != 2 {
		t.Errorf("expected project_id twice in args (CTE + outer scan), got %d: %v", projCount, args)
	}
	if !limitSeen {
		t.Errorf("expected default limit 10 in args, got: %v", args)
	}
	if q.Limit() != 10 {
		t.Errorf("expected Limit() 10, got %d", q.Limit())
	}
}

// TestBuildTopKQuery_CustomProperty verifies the map-access read for a
// non-promoted custom property dimension and an explicit limit.
func TestBuildTopKQuery_CustomProperty(t *testing.T) {
	req := topKRequest(&insightsv1.TopKQuery{
		Dimension: insightsv1.TopKQuery_DIMENSION_PROPERTY.Enum(),
		Property:  proto.String("plan"),
		Limit:     proto.Int32(5),
	})

	q, err := insights.BuildTopKQuery(req, "proj_123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(q.SQL(), "custom_properties['plan']") {
		t.Errorf("expected custom_properties map access, got: %s", q.SQL())
	}
	limitSeen := false
	for _, a := range q.Args() {
		if a == int64(5) {
			limitSeen = true
		}
	}
	if !limitSeen {
		t.Errorf("expected limit 5 in args, got: %v", q.Args())
	}
	if q.Limit() != 5 {
		t.Errorf("expected Limit() 5, got %d", q.Limit())
	}
}

// TestBuildTopKQuery_EventKind verifies the EVENT_KIND dimension groups on the
// kind column.
func TestBuildTopKQuery_EventKind(t *testing.T) {
	req := topKRequest(&insightsv1.TopKQuery{
		Dimension: insightsv1.TopKQuery_DIMENSION_EVENT_KIND.Enum(),
		Metric:    insightsv1.AggregationType_AGGREGATION_TYPE_UNIQUE_USERS.Enum(),
	})

	q, err := insights.BuildTopKQuery(req, "proj_123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	sql := q.SQL()
	if !strings.Contains(sql, "if(kind IN (SELECT dim_value FROM top_vals), kind, '$others')") {
		t.Errorf("expected kind-based $others collapse, got: %s", sql)
	}
	if !strings.Contains(sql, "toFloat64(uniq(distinct_id))") {
		t.Errorf("expected uniq(distinct_id) metric, got: %s", sql)
	}
}

// TestBuildTopKQuery_ScopeAndFiltersInBothScans verifies the event scope and
// top-level filter groups (including a profile-source filter) land in both the
// top_vals CTE and the outer aggregation scan.
func TestBuildTopKQuery_ScopeAndFiltersInBothScans(t *testing.T) {
	req := topKRequest(&insightsv1.TopKQuery{
		Dimension: insightsv1.TopKQuery_DIMENSION_PROPERTY.Enum(),
		Property:  proto.String("$referrer"),
		Scope:     &commonv1.EventFilter{Kind: proto.String("page_view")},
	})
	req.Spec.FilterGroups = []*insightsv1.FilterGroup{{
		Filters: []*commonv1.PropertyFilter{
			{
				Property: proto.String("$country"),
				Operator: commonv1.FilterOperator_FILTER_OPERATOR_EQUALS.Enum(),
				Value:    proto.String("US"),
			},
			{
				Property: proto.String("tier"),
				Operator: commonv1.FilterOperator_FILTER_OPERATOR_EQUALS.Enum(),
				Value:    proto.String("pro"),
				Source:   commonv1.PropertySource_PROPERTY_SOURCE_PROFILE.Enum(),
			},
		},
	}}

	q, err := insights.BuildTopKQuery(req, "proj_123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	sql, args := q.SQL(), q.Args()

	if !strings.Contains(sql, "kind = ?") {
		t.Errorf("expected scope kind condition, got: %s", sql)
	}
	if !strings.Contains(sql, "distinct_id IN (") {
		t.Errorf("expected profile-source filter subquery, got: %s", sql)
	}

	// Args layout is [CTE scan args..., limit, outer scan args...]; the two
	// scans share the same WHERE, so the halves around the LIMIT arg must be
	// identical — proving scope + filters apply to both top_vals and the outer
	// aggregation.
	limitIdx := -1
	for i, a := range args {
		if a == int64(10) {
			limitIdx = i
			break
		}
	}
	if limitIdx < 0 {
		t.Fatalf("expected limit arg in args, got: %v", args)
	}
	cteArgs, outerArgs := args[:limitIdx], args[limitIdx+1:]
	if len(cteArgs) != len(outerArgs) {
		t.Fatalf("expected identical CTE and outer scan args, got %v vs %v", cteArgs, outerArgs)
	}
	for i := range cteArgs {
		if cteArgs[i] != outerArgs[i] {
			t.Errorf("arg %d differs between CTE (%v) and outer scan (%v)", i, cteArgs[i], outerArgs[i])
		}
	}
	for _, v := range []any{"page_view", "US", "pro"} {
		found := false
		for _, a := range cteArgs {
			if a == v {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected arg %v in scan args, got: %v", v, cteArgs)
		}
	}
}

// TestBuildTopKQuery_UserDimension verifies the single-pass USER shape:
// identity union via one ARRAY JOIN pass, LEFT ANY JOIN, e.-aliased
// conditions, per-user partials with row_number ranking, and — critically —
// that events and latest_profiles each appear exactly once (a CTE referenced
// twice would re-execute its scan).
func TestBuildTopKQuery_UserDimension(t *testing.T) {
	req := topKRequest(&insightsv1.TopKQuery{
		Dimension:      insightsv1.TopKQuery_DIMENSION_USER.Enum(),
		Metric:         insightsv1.AggregationType_AGGREGATION_TYPE_SUM.Enum(),
		MetricProperty: proto.String("order_amount"),
	})

	q, err := insights.BuildTopKQuery(req, "proj_123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	sql := q.SQL()
	for _, want := range []string{
		"latest_profiles AS (",
		"latest_profile_aliases AS (",
		"per_user AS (",
		"ranked AS (",
		"LEFT ANY JOIN",
		"ARRAY JOIN arrayDistinct(arrayFilter(x -> x != '', [p.id, p.external_id]))",
		"i.distinct_id = e.distinct_id",
		"if(i.profile_id = '', e.distinct_id, i.profile_id) AS user_key",
		"e.project_id = ?",
		"e.occur_time >= ?",
		"AS sum_num", // per-user partial
		"row_number() OVER (ORDER BY sum_num DESC, user_key ASC) AS rn",
		"if(rn <= ?, user_key, '$others') AS dim_value",
		"sum(sum_num) AS value", // re-merge over partials
		"SETTINGS use_query_cache = 1",
		// WithSpillThreshold sets both group-by and sort spill thresholds.
		"max_bytes_before_external_group_by = 1073741824",
		"max_bytes_before_external_sort = 1073741824",
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("expected %q in SQL, got: %s", want, sql)
		}
	}
	if !strings.Contains(sql, "custom_properties['order_amount']") {
		t.Errorf("expected order_amount map access, got: %s", sql)
	}
	// Single-pass invariants: one events scan, one latest_profiles aggregation
	// source plus its two CTE references (ARRAY JOIN branch + alias join), one
	// join build. Regressing to a twice-referenced CTE would double these.
	if got := strings.Count(sql, "FROM events"); got != 1 {
		t.Errorf("expected exactly 1 events scan, got %d: %s", got, sql)
	}
	if got := strings.Count(sql, "FROM profiles"); got != 1 {
		t.Errorf("expected exactly 1 profiles read (inside latest_profiles), got %d: %s", got, sql)
	}
	if got := strings.Count(sql, "FROM latest_profiles"); got != 1 {
		t.Errorf("expected exactly 1 ARRAY JOIN reference to latest_profiles, got %d: %s", got, sql)
	}
	if got := strings.Count(sql, "LEFT ANY JOIN"); got != 1 {
		t.Errorf("expected exactly 1 identity join, got %d: %s", got, sql)
	}
	if q.Dimension() != insightsv1.TopKQuery_DIMENSION_USER {
		t.Errorf("expected USER dimension on query, got %s", q.Dimension())
	}

	// The limit rides as the rank-split arg (twice: dim_value + is_others).
	limitArgs := 0
	for _, a := range q.Args() {
		if a == int64(10) {
			limitArgs++
		}
	}
	if limitArgs != 2 {
		t.Errorf("expected default limit 10 twice in args (rank split), got %d: %v", limitArgs, q.Args())
	}
}

// TestBuildTopKQuery_UserAvgPartials pins the AVG re-merge contract: avg is
// not re-mergeable from per-user avgs, so the query must carry numerator and
// non-NULL denominator separately and re-divide their sums.
func TestBuildTopKQuery_UserAvgPartials(t *testing.T) {
	req := topKRequest(&insightsv1.TopKQuery{
		Dimension:      insightsv1.TopKQuery_DIMENSION_USER.Enum(),
		Metric:         insightsv1.AggregationType_AGGREGATION_TYPE_AVG.Enum(),
		MetricProperty: proto.String("order_amount"),
	})

	q, err := insights.BuildTopKQuery(req, "proj_123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, want := range []string{
		"AS sum_num",
		"AS cnt_num",
		"row_number() OVER (ORDER BY if(cnt_num = 0, 0, sum_num / cnt_num) DESC, user_key ASC) AS rn",
		"if(sum(cnt_num) = 0, 0, sum(sum_num) / sum(cnt_num)) AS value",
	} {
		if !strings.Contains(q.SQL(), want) {
			t.Errorf("expected %q in SQL, got: %s", want, q.SQL())
		}
	}
}

// TestBuildTopKQuery_UserScopeUsesAliasedColumn verifies the event scope in a
// USER-dimension query references the e.-aliased column inside the per_user
// CTE. A regression where topKBaseConditions drops the "e" prefix would only
// surface in integration tests against ClickHouse otherwise.
func TestBuildTopKQuery_UserScopeUsesAliasedColumn(t *testing.T) {
	req := topKRequest(&insightsv1.TopKQuery{
		Dimension:      insightsv1.TopKQuery_DIMENSION_USER.Enum(),
		Metric:         insightsv1.AggregationType_AGGREGATION_TYPE_SUM.Enum(),
		MetricProperty: proto.String("order_amount"),
		Scope:          &commonv1.EventFilter{Kind: proto.String("signup")},
	})

	q, err := insights.BuildTopKQuery(req, "proj_123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	sql := q.SQL()
	if !strings.Contains(sql, "e.kind = ?") {
		t.Errorf("expected e.-aliased scope condition in per_user CTE, got: %s", sql)
	}
	if strings.Contains(sql, "(kind = ?") || strings.Contains(sql, " kind = ?") {
		t.Errorf("scope must not use the unaliased kind column, got: %s", sql)
	}
}

// TestBuildTopKQuery_UserMinPartials pins the MIN re-merge contract: per-user
// min(...) partial, ifNull(...,0) ranking, and ifNull(min(...),0) merge.
func TestBuildTopKQuery_UserMinPartials(t *testing.T) {
	req := topKRequest(&insightsv1.TopKQuery{
		Dimension:      insightsv1.TopKQuery_DIMENSION_USER.Enum(),
		Metric:         insightsv1.AggregationType_AGGREGATION_TYPE_MIN.Enum(),
		MetricProperty: proto.String("order_amount"),
	})

	q, err := insights.BuildTopKQuery(req, "proj_123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, want := range []string{
		"AS min_num",
		"row_number() OVER (ORDER BY ifNull(min_num, 0) DESC, user_key ASC) AS rn",
		"ifNull(min(min_num), 0) AS value",
	} {
		if !strings.Contains(q.SQL(), want) {
			t.Errorf("expected %q in SQL, got: %s", want, q.SQL())
		}
	}
}

// TestBuildTopKQuery_UserMaxPartials pins the MAX re-merge contract: per-user
// max(...) partial, ifNull(...,0) ranking, and ifNull(max(...),0) merge.
func TestBuildTopKQuery_UserMaxPartials(t *testing.T) {
	req := topKRequest(&insightsv1.TopKQuery{
		Dimension:      insightsv1.TopKQuery_DIMENSION_USER.Enum(),
		Metric:         insightsv1.AggregationType_AGGREGATION_TYPE_MAX.Enum(),
		MetricProperty: proto.String("order_amount"),
	})

	q, err := insights.BuildTopKQuery(req, "proj_123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, want := range []string{
		"AS max_num",
		"row_number() OVER (ORDER BY ifNull(max_num, 0) DESC, user_key ASC) AS rn",
		"ifNull(max(max_num), 0) AS value",
	} {
		if !strings.Contains(q.SQL(), want) {
			t.Errorf("expected %q in SQL, got: %s", want, q.SQL())
		}
	}
}

// TestBuildTopKQuery_Errors verifies defensive builder errors for inputs the
// proto CEL rules reject at the RPC boundary (direct callers bypass them).
func TestBuildTopKQuery_Errors(t *testing.T) {
	tests := []struct {
		name string
		tk   *insightsv1.TopKQuery
	}{
		{name: "missing_top_k", tk: nil},
		{name: "unspecified_dimension", tk: &insightsv1.TopKQuery{}},
		{
			name: "property_dimension_without_property",
			tk: &insightsv1.TopKQuery{
				Dimension: insightsv1.TopKQuery_DIMENSION_PROPERTY.Enum(),
			},
		},
		{
			name: "user_dimension_unique_users",
			tk: &insightsv1.TopKQuery{
				Dimension: insightsv1.TopKQuery_DIMENSION_USER.Enum(),
				Metric:    insightsv1.AggregationType_AGGREGATION_TYPE_UNIQUE_USERS.Enum(),
			},
		},
		{
			name: "user_dimension_per_user_avg",
			tk: &insightsv1.TopKQuery{
				Dimension: insightsv1.TopKQuery_DIMENSION_USER.Enum(),
				Metric:    insightsv1.AggregationType_AGGREGATION_TYPE_PER_USER_AVG.Enum(),
			},
		},
		{
			name: "user_dimension_sum_without_property",
			tk: &insightsv1.TopKQuery{
				Dimension: insightsv1.TopKQuery_DIMENSION_USER.Enum(),
				Metric:    insightsv1.AggregationType_AGGREGATION_TYPE_SUM.Enum(),
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := insights.BuildTopKQuery(topKRequest(tt.tk), "proj_123"); err == nil {
				t.Error("expected builder error, got nil")
			}
		})
	}
}

// TestBuildTopKProfilesQuery verifies placeholder expansion and the empty-ids
// short-circuit of the enrichment lookup.
func TestBuildTopKProfilesQuery(t *testing.T) {
	t.Run("empty_ids_short_circuit", func(t *testing.T) {
		sql, args, err := insights.BuildTopKProfilesQuery("proj_123", nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if sql != "" || args != nil {
			t.Errorf("expected empty SQL and args, got %q / %v", sql, args)
		}
	})

	t.Run("placeholder_expansion", func(t *testing.T) {
		ids := []string{"p1", "p2", "p3"}
		sql, args, err := insights.BuildTopKProfilesQuery("proj_123", ids)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !strings.Contains(sql, "id IN (?, ?, ?)") {
			t.Errorf("expected 3 placeholders, got: %s", sql)
		}
		// project_id + 3 ids
		if len(args) != 4 {
			t.Errorf("expected 4 args, got %d: %v", len(args), args)
		}
		for _, want := range []string{
			"argMax(external_id, insert_time) AS external_id",
			"toJSONString(argMax(properties, insert_time)) AS properties_json",
			"HAVING argMax(is_deleted, insert_time) = 0",
		} {
			if !strings.Contains(sql, want) {
				t.Errorf("expected %q in SQL, got: %s", want, sql)
			}
		}
		if strings.Contains(sql, "use_query_cache") {
			t.Errorf("enrichment lookup must not be query-cached, got: %s", sql)
		}
	})
}
