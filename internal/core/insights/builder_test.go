package insights_test

import (
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	commonv1 "github.com/fivebitsio/cotton/internal/gen/proto/common/v1"
	insightsv1 "github.com/fivebitsio/cotton/internal/gen/proto/shared/insights/v1"

	"github.com/fivebitsio/cotton/internal/core/insights"
)

func mustTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

func timeRange(from, to string) *commonv1.TimeRange {
	return &commonv1.TimeRange{
		From: timestamppb.New(mustTime(from)),
		To:   timestamppb.New(mustTime(to)),
	}
}

// TestBasicTrends verifies the SQL structure for a simple daily trends query.
func TestBasicTrends(t *testing.T) {
	req := &insightsv1.QueryRequest{
		InsightType: insightsv1.InsightType_INSIGHT_TYPE_TRENDS,
		TimeRange:   timeRange("2024-01-01T00:00:00Z", "2024-01-07T23:59:59Z"),
		Granularity: insightsv1.Granularity_GRANULARITY_DAY,
		Events: []*insightsv1.EventQuery{
			{Kind: "page_view", Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL},
		},
	}

	sql, args, err := insights.BuildQuery(req, "proj_123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify SQL structure
	if !strings.Contains(sql, "toStartOfDay") {
		t.Errorf("expected toStartOfDay in SQL, got: %s", sql)
	}
	if !strings.Contains(sql, "toFloat64(count(*))") {
		t.Errorf("expected toFloat64(count(*)) in SQL, got: %s", sql)
	}
	if !strings.Contains(sql, "GROUP BY") {
		t.Errorf("expected GROUP BY in SQL, got: %s", sql)
	}
	if !strings.Contains(sql, "ORDER BY") {
		t.Errorf("expected ORDER BY in SQL, got: %s", sql)
	}
	if !strings.Contains(sql, "occur_time") {
		t.Errorf("expected occur_time in SQL, got: %s", sql)
	}

	// Verify args: projectID, from, to, kind
	if len(args) != 4 {
		t.Errorf("expected 4 args (projectID, from, to, kind), got %d: %v", len(args), args)
	}
}

// TestTrendsWithFilters verifies DISTINCT and filter args for unique users + country filter.
func TestTrendsWithFilters(t *testing.T) {
	req := &insightsv1.QueryRequest{
		InsightType: insightsv1.InsightType_INSIGHT_TYPE_TRENDS,
		TimeRange:   timeRange("2024-01-01T00:00:00Z", "2024-01-07T23:59:59Z"),
		Granularity: insightsv1.Granularity_GRANULARITY_DAY,
		Events: []*insightsv1.EventQuery{
			{Kind: "page_view", Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_UNIQUE_USERS},
		},
		Filters: []*commonv1.PropertyFilter{
			{
				Property: "$country",
				Operator: commonv1.FilterOperator_FILTER_OPERATOR_EQUALS,
				Value:    "US",
			},
		},
	}

	sql, args, err := insights.BuildQuery(req, "proj_123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(sql, "toFloat64(count(DISTINCT distinct_id))") {
		t.Errorf("expected toFloat64(count(DISTINCT distinct_id)) in SQL, got: %s", sql)
	}
	if !strings.Contains(sql, "ifNull(nullIf(auto_properties['$country'], ''), custom_properties['$country'])") {
		t.Errorf("expected property resolution expression in SQL, got: %s", sql)
	}

	// args: projectID, from, to, kind, "US"
	if len(args) != 5 {
		t.Errorf("expected 5 args (projectID, from, to, kind, value), got %d: %v", len(args), args)
	}
	if args[4] != "US" {
		t.Errorf("expected last arg to be 'US', got %v", args[4])
	}
}

// TestSegmentation verifies segmentation queries have no GROUP BY time bucket.
func TestSegmentation(t *testing.T) {
	req := &insightsv1.QueryRequest{
		InsightType: insightsv1.InsightType_INSIGHT_TYPE_SEGMENTATION,
		TimeRange:   timeRange("2024-01-01T00:00:00Z", "2024-01-07T23:59:59Z"),
		Events: []*insightsv1.EventQuery{
			{Kind: "purchase", Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL},
		},
	}

	sql, args, err := insights.BuildQuery(req, "proj_123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if strings.Contains(sql, "GROUP BY") {
		t.Errorf("segmentation should not have GROUP BY, got: %s", sql)
	}
	if strings.Contains(sql, "toStartOfDay") || strings.Contains(sql, "toStartOfHour") {
		t.Errorf("segmentation should not have time bucketing, got: %s", sql)
	}
	if !strings.Contains(sql, "toFloat64(count(*))") {
		t.Errorf("expected toFloat64(count(*)) in SQL, got: %s", sql)
	}

	// args: projectID, from, to, kind
	if len(args) != 4 {
		t.Errorf("expected 4 args, got %d: %v", len(args), args)
	}
}

// TestAllEvents verifies that an empty events list generates no kind filter (3 args).
func TestAllEvents(t *testing.T) {
	req := &insightsv1.QueryRequest{
		InsightType: insightsv1.InsightType_INSIGHT_TYPE_TRENDS,
		TimeRange:   timeRange("2024-01-01T00:00:00Z", "2024-01-07T23:59:59Z"),
		Granularity: insightsv1.Granularity_GRANULARITY_DAY,
		Events:      []*insightsv1.EventQuery{}, // empty = all events
	}

	sql, args, err := insights.BuildQuery(req, "proj_123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if strings.Contains(sql, "kind = ?") {
		t.Errorf("empty events should not add kind filter, got: %s", sql)
	}

	// args: projectID, from, to (no kind)
	if len(args) != 3 {
		t.Errorf("expected 3 args (projectID, from, to), got %d: %v", len(args), args)
	}
}

// TestMultiEventTrends verifies UNION ALL is generated for multiple events with one series per event.
func TestMultiEventTrends(t *testing.T) {
	req := &insightsv1.QueryRequest{
		InsightType: insightsv1.InsightType_INSIGHT_TYPE_TRENDS,
		TimeRange:   timeRange("2024-01-01T00:00:00Z", "2024-01-07T23:59:59Z"),
		Granularity: insightsv1.Granularity_GRANULARITY_DAY,
		Events: []*insightsv1.EventQuery{
			{Kind: "page_view", Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL},
			{Kind: "purchase", Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_UNIQUE_USERS},
		},
	}

	sql, args, err := insights.BuildQuery(req, "proj_123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(sql, "UNION ALL") {
		t.Errorf("expected UNION ALL in SQL, got: %s", sql)
	}
	if !strings.Contains(sql, "kind AS event_kind") {
		t.Errorf("expected 'kind AS event_kind' in SQL, got: %s", sql)
	}
	// Event kinds are passed as args, not inlined.
	if args[3] != "page_view" {
		t.Errorf("expected first event kind arg to be 'page_view', got %v", args[3])
	}
	if args[7] != "purchase" {
		t.Errorf("expected second event kind arg to be 'purchase', got %v", args[7])
	}
	if !strings.Contains(sql, "toFloat64(count(*))") {
		t.Errorf("expected total aggregation for page_view in SQL, got: %s", sql)
	}
	if !strings.Contains(sql, "toFloat64(count(DISTINCT distinct_id))") {
		t.Errorf("expected unique users aggregation for purchase in SQL, got: %s", sql)
	}

	// args: (projectID, from, to, kind) x2 — no top-level filters
	if len(args) != 8 {
		t.Errorf("expected 8 args (4 per event), got %d: %v", len(args), args)
	}
}

// TestPerUserAvg verifies the toFloat64 division expression is used.
func TestPerUserAvg(t *testing.T) {
	req := &insightsv1.QueryRequest{
		InsightType: insightsv1.InsightType_INSIGHT_TYPE_TRENDS,
		TimeRange:   timeRange("2024-01-01T00:00:00Z", "2024-01-07T23:59:59Z"),
		Granularity: insightsv1.Granularity_GRANULARITY_WEEK,
		Events: []*insightsv1.EventQuery{
			{Kind: "add_to_cart", Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_PER_USER_AVG},
		},
	}

	sql, args, err := insights.BuildQuery(req, "proj_123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(sql, "toFloat64(count(*)) / toFloat64(count(DISTINCT distinct_id))") {
		t.Errorf("expected toFloat64 division in SQL, got: %s", sql)
	}
	if !strings.Contains(sql, "toStartOfWeek") {
		t.Errorf("expected toStartOfWeek in SQL, got: %s", sql)
	}

	// args: projectID, from, to, kind
	if len(args) != 4 {
		t.Errorf("expected 4 args, got %d: %v", len(args), args)
	}
}

// TestGranularityDefault verifies UNSPECIFIED granularity defaults to DAY.
func TestGranularityDefault(t *testing.T) {
	req := &insightsv1.QueryRequest{
		InsightType: insightsv1.InsightType_INSIGHT_TYPE_TRENDS,
		TimeRange:   timeRange("2024-01-01T00:00:00Z", "2024-01-07T23:59:59Z"),
		Granularity: insightsv1.Granularity_GRANULARITY_UNSPECIFIED,
		Events: []*insightsv1.EventQuery{
			{Kind: "page_view", Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL},
		},
	}

	sql, _, err := insights.BuildQuery(req, "proj_123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(sql, "toStartOfDay") {
		t.Errorf("expected default granularity toStartOfDay in SQL, got: %s", sql)
	}
}

// TestBuildTrendsQuery_WithBreakdown verifies single breakdown generates CTE + conditional bucketing.
func TestBuildTrendsQuery_WithBreakdown(t *testing.T) {
	req := &insightsv1.QueryRequest{
		InsightType: insightsv1.InsightType_INSIGHT_TYPE_TRENDS,
		TimeRange:   timeRange("2024-01-01T00:00:00Z", "2024-01-07T23:59:59Z"),
		Granularity: insightsv1.Granularity_GRANULARITY_DAY,
		Events: []*insightsv1.EventQuery{
			{Kind: "page_view", Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL},
		},
		Breakdowns:     []*insightsv1.Breakdown{{Property: "$country"}},
		BreakdownLimit: 3,
	}

	sql, args, err := insights.BuildQuery(req, "proj_123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// SQL structure checks
	if !strings.Contains(sql, "top_vals") {
		t.Errorf("expected CTE 'top_vals' in SQL, got: %s", sql)
	}
	if !strings.Contains(sql, "breakdown_0") {
		t.Errorf("expected 'breakdown_0' in SQL, got: %s", sql)
	}
	if !strings.Contains(sql, "$others") {
		t.Errorf("expected '$others' in SQL, got: %s", sql)
	}

	// breakdown limit 3 should appear as an arg
	found := false
	for _, a := range args {
		if a == int32(3) {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected breakdown limit int32(3) in args, got: %v", args)
	}

	// WHERE args must be duplicated: CTE uses same WHERE as main query.
	// Base args: projectID, from, to, kind = 4. With duplication = 8 + 1 (limit).
	// CTE: projectID, from, to, kind (4 args) + limit (1) = 5
	// Main: projectID, from, to, kind (4 args) = 4
	// Total = 9
	if len(args) != 9 {
		t.Fatalf("expected 9 args (CTE where x4 + limit + main where x4), got %d: %v", len(args), args)
	}
	// First 4 and args[5:9] should be the same projectID/from/to/kind pair
	for i := 0; i < 4; i++ {
		if args[i] != args[5+i] {
			t.Errorf("arg[%d]=%v != arg[%d]=%v: WHERE args not duplicated", i, args[i], 5+i, args[5+i])
		}
	}
}

// TestBuildTrendsQuery_MultipleBreakdowns verifies two breakdowns produce breakdown_0 and breakdown_1.
func TestBuildTrendsQuery_MultipleBreakdowns(t *testing.T) {
	req := &insightsv1.QueryRequest{
		InsightType: insightsv1.InsightType_INSIGHT_TYPE_TRENDS,
		TimeRange:   timeRange("2024-01-01T00:00:00Z", "2024-01-07T23:59:59Z"),
		Granularity: insightsv1.Granularity_GRANULARITY_DAY,
		Events: []*insightsv1.EventQuery{
			{Kind: "page_view", Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL},
		},
		Breakdowns: []*insightsv1.Breakdown{
			{Property: "$country"},
			{Property: "$city"},
		},
		BreakdownLimit: 5,
	}

	sql, _, err := insights.BuildQuery(req, "proj_123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(sql, "breakdown_0") {
		t.Errorf("expected 'breakdown_0' in SQL, got: %s", sql)
	}
	if !strings.Contains(sql, "breakdown_1") {
		t.Errorf("expected 'breakdown_1' in SQL, got: %s", sql)
	}
}

// TestBuildTrendsQuery_DefaultBreakdownLimit verifies BreakdownLimit=0 defaults to int32(10).
func TestBuildTrendsQuery_DefaultBreakdownLimit(t *testing.T) {
	req := &insightsv1.QueryRequest{
		InsightType: insightsv1.InsightType_INSIGHT_TYPE_TRENDS,
		TimeRange:   timeRange("2024-01-01T00:00:00Z", "2024-01-07T23:59:59Z"),
		Granularity: insightsv1.Granularity_GRANULARITY_DAY,
		Events: []*insightsv1.EventQuery{
			{Kind: "page_view", Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL},
		},
		Breakdowns:     []*insightsv1.Breakdown{{Property: "$country"}},
		BreakdownLimit: 0,
	}

	_, args, err := insights.BuildQuery(req, "proj_123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	found := false
	for _, a := range args {
		if a == int32(10) {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected default breakdown limit int32(10) in args, got: %v", args)
	}
}

// TestFilterOperators verifies each filter operator generates correct SQL.
func TestFilterOperators(t *testing.T) {
	baseReq := func(op commonv1.FilterOperator, val string) *insightsv1.QueryRequest {
		return &insightsv1.QueryRequest{
			InsightType: insightsv1.InsightType_INSIGHT_TYPE_SEGMENTATION,
			TimeRange:   timeRange("2024-01-01T00:00:00Z", "2024-01-07T23:59:59Z"),
			Events: []*insightsv1.EventQuery{
				{Kind: "page_view", Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL},
			},
			Filters: []*commonv1.PropertyFilter{
				{Property: "$browser", Operator: op, Value: val},
			},
		}
	}

	tests := []struct {
		name       string
		op         commonv1.FilterOperator
		val        string
		wantSQL    string
		wantArgVal any
		wantNoArg  bool
	}{
		{
			name:       "equals",
			op:         commonv1.FilterOperator_FILTER_OPERATOR_EQUALS,
			val:        "Chrome",
			wantSQL:    "= ?",
			wantArgVal: "Chrome",
		},
		{
			name:       "not_equals",
			op:         commonv1.FilterOperator_FILTER_OPERATOR_NOT_EQUALS,
			val:        "Firefox",
			wantSQL:    "!= ?",
			wantArgVal: "Firefox",
		},
		{
			name:       "contains",
			op:         commonv1.FilterOperator_FILTER_OPERATOR_CONTAINS,
			val:        "rom",
			wantSQL:    "LIKE ?",
			wantArgVal: "%rom%",
		},
		{
			name:       "not_contains",
			op:         commonv1.FilterOperator_FILTER_OPERATOR_NOT_CONTAINS,
			val:        "IE",
			wantSQL:    "NOT LIKE ?",
			wantArgVal: "%IE%",
		},
		{
			name:      "is_set",
			op:        commonv1.FilterOperator_FILTER_OPERATOR_IS_SET,
			val:       "",
			wantSQL:   "!= ''",
			wantNoArg: true,
		},
		{
			name:      "is_not_set",
			op:        commonv1.FilterOperator_FILTER_OPERATOR_IS_NOT_SET,
			val:       "",
			wantSQL:   "= ''",
			wantNoArg: true,
		},
		{
			name:       "lte",
			op:         commonv1.FilterOperator_FILTER_OPERATOR_LTE,
			val:        "100",
			wantSQL:    "<= ?",
			wantArgVal: float64(100),
		},
		{
			name:       "gte",
			op:         commonv1.FilterOperator_FILTER_OPERATOR_GTE,
			val:        "5.5",
			wantSQL:    ">= ?",
			wantArgVal: float64(5.5),
		},
		{
			name:       "lt",
			op:         commonv1.FilterOperator_FILTER_OPERATOR_LT,
			val:        "100",
			wantSQL:    "< ?",
			wantArgVal: float64(100),
		},
		{
			name:       "gt",
			op:         commonv1.FilterOperator_FILTER_OPERATOR_GT,
			val:        "5.5",
			wantSQL:    "> ?",
			wantArgVal: float64(5.5),
		},
	}

	inTests := []struct {
		name    string
		op      commonv1.FilterOperator
		values  []string
		wantSQL string
	}{
		{
			name:    "in",
			op:      commonv1.FilterOperator_FILTER_OPERATOR_IN,
			values:  []string{"US", "CA", "GB"},
			wantSQL: "IN (?, ?, ?)",
		},
		{
			name:    "not_in",
			op:      commonv1.FilterOperator_FILTER_OPERATOR_NOT_IN,
			values:  []string{"bot", "crawler"},
			wantSQL: "NOT IN (?, ?)",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sql, args, err := insights.BuildQuery(baseReq(tc.op, tc.val), "proj_123")
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !strings.Contains(sql, tc.wantSQL) {
				t.Errorf("expected %q in SQL, got: %s", tc.wantSQL, sql)
			}
			if !tc.wantNoArg {
				// Filter arg comes before event kind arg (writeEventCondition appends kind last).
				// args layout: projectID, from, to, filterValue..., kind
				if len(args) < 2 {
					t.Fatalf("expected at least 2 args but got %d", len(args))
				}
				filterArg := args[len(args)-2]
				if filterArg != tc.wantArgVal {
					t.Errorf("expected filter arg %q, got %v", tc.wantArgVal, filterArg)
				}
			}
		})
	}

	for _, tc := range inTests {
		t.Run(tc.name, func(t *testing.T) {
			req := &insightsv1.QueryRequest{
				InsightType: insightsv1.InsightType_INSIGHT_TYPE_SEGMENTATION,
				TimeRange:   timeRange("2024-01-01T00:00:00Z", "2024-01-07T23:59:59Z"),
				Events: []*insightsv1.EventQuery{
					{Kind: "page_view", Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL},
				},
				Filters: []*commonv1.PropertyFilter{
					{Property: "$country", Operator: tc.op, Values: tc.values},
				},
			}
			sql, args, err := insights.BuildQuery(req, "proj_123")
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !strings.Contains(sql, tc.wantSQL) {
				t.Errorf("expected %q in SQL, got: %s", tc.wantSQL, sql)
			}
			// Filter value args come before the event kind arg (writeEventCondition appends kind last).
			// args layout: projectID, from, to, filterValues..., kind
			n := len(tc.values)
			if len(args) < n+1 {
				t.Fatalf("expected at least %d args, got %d: %v", n+1, len(args), args)
			}
			for i, want := range tc.values {
				got := args[len(args)-1-n+i]
				if got != want {
					t.Errorf("arg[%d]: expected %q, got %v", i, want, got)
				}
			}
		})
	}
}

// TestBuildSegmentUsersQuery verifies DISTINCT, ORDER BY, LIMIT, and filter handling.
func TestBuildSegmentUsersQuery(t *testing.T) {
	req := &insightsv1.SegmentUsersRequest{
		TimeRange: timeRange("2024-01-01T00:00:00Z", "2024-01-07T23:59:59Z"),
		Events: []*insightsv1.EventQuery{
			{Kind: "purchase", Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL},
		},
		Filters: []*commonv1.PropertyFilter{
			{
				Property: "$country",
				Operator: commonv1.FilterOperator_FILTER_OPERATOR_EQUALS,
				Value:    "US",
			},
		},
		PageSize: 50,
	}

	sql, args, err := insights.BuildSegmentUsersQuery(req, "proj_123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(sql, "SELECT DISTINCT distinct_id") {
		t.Errorf("expected DISTINCT distinct_id in SQL, got: %s", sql)
	}
	if !strings.Contains(sql, "ORDER BY distinct_id ASC") {
		t.Errorf("expected ORDER BY distinct_id ASC in SQL, got: %s", sql)
	}
	if !strings.Contains(sql, "LIMIT ?") {
		t.Errorf("expected LIMIT ? in SQL, got: %s", sql)
	}
	if !strings.Contains(sql, "ifNull(nullIf(auto_properties['$country'], ''), custom_properties['$country'])") {
		t.Errorf("expected property filter expression in SQL, got: %s", sql)
	}

	// args: projectID, from, to, filter_value, kind, limit
	if len(args) != 6 {
		t.Errorf("expected 6 args (projectID, from, to, filter_value, kind, limit), got %d: %v", len(args), args)
	}
	if args[0] != "proj_123" {
		t.Errorf("expected first arg to be 'proj_123', got %v", args[0])
	}
	if args[3] != "US" {
		t.Errorf("expected filter arg to be 'US', got %v", args[3])
	}
	if args[5] != int32(50) {
		t.Errorf("expected limit arg to be int32(50), got %v", args[5])
	}
}

// TestBuildSegmentUsersQuery_MultiEvent verifies OR-joined event conditions for multiple events.
func TestBuildSegmentUsersQuery_MultiEvent(t *testing.T) {
	req := &insightsv1.SegmentUsersRequest{
		TimeRange: timeRange("2024-01-01T00:00:00Z", "2024-01-07T23:59:59Z"),
		Events: []*insightsv1.EventQuery{
			{
				Kind:        "purchase",
				Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL,
				Filters: []*commonv1.PropertyFilter{
					{Property: "$country", Operator: commonv1.FilterOperator_FILTER_OPERATOR_EQUALS, Value: "US"},
				},
			},
			{Kind: "page_view", Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL},
		},
		PageSize: 50,
	}

	sql, args, err := insights.BuildSegmentUsersQuery(req, "proj_123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(sql, "AND (\n") {
		t.Errorf("expected OR-joined event condition in SQL, got: %s", sql)
	}
	if !strings.Contains(sql, "OR ") {
		t.Errorf("expected OR between events in SQL, got: %s", sql)
	}

	// args: projectID, from, to, "US" (per-event filter), "purchase", "page_view", limit
	if len(args) != 7 {
		t.Errorf("expected 7 args, got %d: %v", len(args), args)
	}
	if args[6] != int32(50) {
		t.Errorf("expected limit arg to be int32(50), got %v", args[6])
	}
}

// TestBuildSegmentUsersQuery_WithPageToken verifies cursor pagination adds distinct_id > ? clause.
func TestBuildSegmentUsersQuery_WithPageToken(t *testing.T) {
	req := &insightsv1.SegmentUsersRequest{
		TimeRange: timeRange("2024-01-01T00:00:00Z", "2024-01-07T23:59:59Z"),
		Events: []*insightsv1.EventQuery{
			{Kind: "page_view", Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL},
		},
		PageToken: "user_abc",
		PageSize:  0, // should default to 100
	}

	sql, args, err := insights.BuildSegmentUsersQuery(req, "proj_123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(sql, "AND distinct_id > ?") {
		t.Errorf("expected cursor clause 'AND distinct_id > ?' in SQL, got: %s", sql)
	}

	// Cursor value should appear before LIMIT arg
	var cursorIdx, limitIdx int
	for i, a := range args {
		switch v := a.(type) {
		case string:
			if v == "user_abc" {
				cursorIdx = i
			}
		case int32:
			if v == 100 {
				limitIdx = i
			}
		}
	}
	if cursorIdx == 0 && args[0] != "user_abc" {
		// cursorIdx of 0 would only be valid if "user_abc" is the projectID, which it's not
		t.Errorf("cursor token 'user_abc' not found in args: %v", args)
	}
	if limitIdx == 0 {
		t.Errorf("default page_size 100 (int32) not found in args: %v", args)
	}
	if cursorIdx > limitIdx {
		t.Errorf("cursor arg (idx %d) should come before limit arg (idx %d)", cursorIdx, limitIdx)
	}
}

func TestUnsupportedInsightType(t *testing.T) {
	req := &insightsv1.QueryRequest{
		InsightType: insightsv1.InsightType_INSIGHT_TYPE_UNSPECIFIED,
		TimeRange:   timeRange("2024-01-01T00:00:00Z", "2024-01-07T23:59:59Z"),
		Granularity: insightsv1.Granularity_GRANULARITY_DAY,
	}

	_, _, err := insights.BuildQuery(req, "proj_123")
	if err == nil {
		t.Fatal("expected error for unsupported insight type, got nil")
	}
	if !strings.Contains(err.Error(), "unsupported insight type") {
		t.Errorf("expected 'unsupported insight type' in error, got: %v", err)
	}
}

func TestUnsupportedFilterOperator(t *testing.T) {
	req := &insightsv1.QueryRequest{
		InsightType: insightsv1.InsightType_INSIGHT_TYPE_SEGMENTATION,
		TimeRange:   timeRange("2024-01-01T00:00:00Z", "2024-01-07T23:59:59Z"),
		Events: []*insightsv1.EventQuery{
			{Kind: "page_view", Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL},
		},
		Filters: []*commonv1.PropertyFilter{
			{Property: "$browser", Operator: commonv1.FilterOperator_FILTER_OPERATOR_UNSPECIFIED, Value: "x"},
		},
	}

	_, _, err := insights.BuildQuery(req, "proj_123")
	if err == nil {
		t.Fatal("expected error for unsupported filter operator, got nil")
	}
	if !strings.Contains(err.Error(), "unsupported filter operator") {
		t.Errorf("expected 'unsupported filter operator' in error, got: %v", err)
	}
}

func TestNumericFilterRejectsNonNumericValue(t *testing.T) {
	operators := []commonv1.FilterOperator{
		commonv1.FilterOperator_FILTER_OPERATOR_LTE,
		commonv1.FilterOperator_FILTER_OPERATOR_GTE,
		commonv1.FilterOperator_FILTER_OPERATOR_LT,
		commonv1.FilterOperator_FILTER_OPERATOR_GT,
	}
	for _, op := range operators {
		t.Run(op.String(), func(t *testing.T) {
			req := &insightsv1.QueryRequest{
				InsightType: insightsv1.InsightType_INSIGHT_TYPE_SEGMENTATION,
				TimeRange:   timeRange("2024-01-01T00:00:00Z", "2024-01-07T23:59:59Z"),
				Events: []*insightsv1.EventQuery{
					{Kind: "click", Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL},
				},
				Filters: []*commonv1.PropertyFilter{
					{Property: "score", Operator: op, Value: "not-a-number"},
				},
			}
			_, _, err := insights.BuildQuery(req, "proj_123")
			if err == nil {
				t.Fatal("expected error for non-numeric value, got nil")
			}
			if !strings.Contains(err.Error(), "invalid numeric value") {
				t.Errorf("expected 'invalid numeric value' in error, got: %v", err)
			}
		})
	}
}

func TestMultipleCombinedFilters(t *testing.T) {
	req := &insightsv1.QueryRequest{
		InsightType: insightsv1.InsightType_INSIGHT_TYPE_TRENDS,
		TimeRange:   timeRange("2024-01-01T00:00:00Z", "2024-01-07T23:59:59Z"),
		Granularity: insightsv1.Granularity_GRANULARITY_DAY,
		Events: []*insightsv1.EventQuery{
			{Kind: "page_view", Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL},
		},
		Filters: []*commonv1.PropertyFilter{
			{Property: "$country", Operator: commonv1.FilterOperator_FILTER_OPERATOR_EQUALS, Value: "US"},
			{Property: "$browser", Operator: commonv1.FilterOperator_FILTER_OPERATOR_CONTAINS, Value: "Chrome"},
			{Property: "age", Operator: commonv1.FilterOperator_FILTER_OPERATOR_GTE, Value: "18"},
		},
	}

	sql, args, err := insights.BuildQuery(req, "proj_123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// All three filters should appear as AND clauses
	if !strings.Contains(sql, "= ?") {
		t.Errorf("expected EQUALS clause in SQL, got: %s", sql)
	}
	if !strings.Contains(sql, "LIKE ?") {
		t.Errorf("expected LIKE clause in SQL, got: %s", sql)
	}
	if !strings.Contains(sql, ">= ?") {
		t.Errorf("expected GTE clause in SQL, got: %s", sql)
	}

	// args: projectID, from, to, kind, "US", "%Chrome%", 18.0
	if len(args) != 7 {
		t.Fatalf("expected 7 args, got %d: %v", len(args), args)
	}
	if args[4] != "US" {
		t.Errorf("expected args[4]='US', got %v", args[4])
	}
	if args[5] != "%Chrome%" {
		t.Errorf("expected args[5]='%%Chrome%%', got %v", args[5])
	}
	if args[6] != float64(18) {
		t.Errorf("expected args[6]=18.0, got %v", args[6])
	}
}

func TestGranularityHourAndMonth(t *testing.T) {
	tests := []struct {
		name        string
		granularity insightsv1.Granularity
		wantFunc    string
	}{
		{name: "hour", granularity: insightsv1.Granularity_GRANULARITY_HOUR, wantFunc: "toStartOfHour"},
		{name: "month", granularity: insightsv1.Granularity_GRANULARITY_MONTH, wantFunc: "toStartOfMonth"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := &insightsv1.QueryRequest{
				InsightType: insightsv1.InsightType_INSIGHT_TYPE_TRENDS,
				TimeRange:   timeRange("2024-01-01T00:00:00Z", "2024-01-07T23:59:59Z"),
				Granularity: tc.granularity,
				Events: []*insightsv1.EventQuery{
					{Kind: "page_view", Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL},
				},
			}

			sql, _, err := insights.BuildQuery(req, "proj_123")
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !strings.Contains(sql, tc.wantFunc) {
				t.Errorf("expected %s in SQL, got: %s", tc.wantFunc, sql)
			}
		})
	}
}

func TestGroupSeries_Breakdowns(t *testing.T) {
	rows := []insights.TrendRow{
		{Time: mustTime("2024-01-01T00:00:00Z"), EventKind: "page_view", Breakdowns: []string{"US"}, Value: 10},
		{Time: mustTime("2024-01-02T00:00:00Z"), EventKind: "page_view", Breakdowns: []string{"US"}, Value: 20},
		{Time: mustTime("2024-01-01T00:00:00Z"), EventKind: "page_view", Breakdowns: []string{"GB"}, Value: 5},
		{Time: mustTime("2024-01-02T00:00:00Z"), EventKind: "page_view", Breakdowns: []string{"GB"}, Value: 8},
		{Time: mustTime("2024-01-01T00:00:00Z"), EventKind: "page_view", Breakdowns: []string{"US"}, Value: 3},
	}

	series := insights.GroupSeries(rows, []string{"$country"})

	if len(series) != 2 {
		t.Fatalf("expected 2 series, got %d", len(series))
	}
	if series[0].Breakdown["$country"] != "US" {
		t.Errorf("expected first series breakdown country=US, got %v", series[0].Breakdown)
	}
	if len(series[0].Points) != 3 {
		t.Errorf("expected 3 points for US, got %d", len(series[0].Points))
	}
	if series[1].Breakdown["$country"] != "GB" {
		t.Errorf("expected second series breakdown country=GB, got %v", series[1].Breakdown)
	}
	if len(series[1].Points) != 2 {
		t.Errorf("expected 2 points for GB, got %d", len(series[1].Points))
	}
	if series[0].Points[0].Value != 10 {
		t.Errorf("expected first US point value=10, got %v", series[0].Points[0].Value)
	}
	if series[1].Points[1].Value != 8 {
		t.Errorf("expected second GB point value=8, got %v", series[1].Points[1].Value)
	}
}

func TestContainsEscapesLIKEMetacharacters(t *testing.T) {
	tests := []struct {
		name    string
		val     string
		wantArg string
	}{
		{name: "percent", val: "100%", wantArg: `%100\%%`},
		{name: "underscore", val: "a_b", wantArg: `%a\_b%`},
		{name: "backslash", val: `a\b`, wantArg: `%a\\b%`},
		{name: "all_three", val: `10%_\x`, wantArg: `%10\%\_\\x%`},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := &insightsv1.QueryRequest{
				InsightType: insightsv1.InsightType_INSIGHT_TYPE_SEGMENTATION,
				TimeRange:   timeRange("2024-01-01T00:00:00Z", "2024-01-07T23:59:59Z"),
				Events: []*insightsv1.EventQuery{
					{Kind: "click", Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL},
				},
				Filters: []*commonv1.PropertyFilter{
					{Property: "url", Operator: commonv1.FilterOperator_FILTER_OPERATOR_CONTAINS, Value: tc.val},
				},
			}

			_, args, err := insights.BuildQuery(req, "proj_123")
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			// Filter arg comes before event kind arg (writeEventCondition appends kind last).
			filterArg := args[len(args)-2]
			if filterArg != tc.wantArg {
				t.Errorf("expected LIKE arg %q, got %q", tc.wantArg, filterArg)
			}
		})
	}
}

func TestBuildAutoPropertyValuesQuery(t *testing.T) {
	t.Run("with_event_kind", func(t *testing.T) {
		sql, args := insights.BuildAutoPropertyValuesQuery("proj_1", "$browser", "page_view")
		if !strings.Contains(sql, "auto_properties") {
			t.Error("expected auto_properties in SQL")
		}
		if !strings.Contains(sql, "kind = ?") {
			t.Error("expected kind filter in SQL")
		}
		if len(args) != 4 {
			t.Fatalf("expected 4 args, got %d: %v", len(args), args)
		}
		// args: propertyKey, projectID, eventKind, propertyKey
		if args[0] != "$browser" || args[1] != "proj_1" || args[2] != "page_view" || args[3] != "$browser" {
			t.Errorf("unexpected args: %v", args)
		}
	})

	t.Run("without_event_kind", func(t *testing.T) {
		sql, args := insights.BuildAutoPropertyValuesQuery("proj_1", "$browser", "")
		if !strings.Contains(sql, "auto_properties") {
			t.Error("expected auto_properties in SQL")
		}
		if strings.Contains(sql, "kind = ?") {
			t.Error("should not have kind filter when eventKind is empty")
		}
		if len(args) != 3 {
			t.Fatalf("expected 3 args, got %d: %v", len(args), args)
		}
		if args[0] != "$browser" || args[1] != "proj_1" || args[2] != "$browser" {
			t.Errorf("unexpected args: %v", args)
		}
	})

	t.Run("custom_variant", func(t *testing.T) {
		sql, _ := insights.BuildCustomPropertyValuesQuery("proj_1", "plan", "")
		if !strings.Contains(sql, "custom_properties") {
			t.Error("expected custom_properties in SQL")
		}
		if strings.Contains(sql, "auto_properties") {
			t.Error("should not contain auto_properties")
		}
	})
}

func TestBuildEventNamesQuery(t *testing.T) {
	sql, args := insights.BuildEventNamesQuery("proj_1")
	if !strings.Contains(sql, "event_names") {
		t.Error("expected event_names table in SQL")
	}
	if !strings.Contains(sql, "countMerge(event_count)") {
		t.Error("expected countMerge in SQL")
	}
	if !strings.Contains(sql, "maxMerge(last_seen)") {
		t.Error("expected maxMerge in SQL")
	}
	if len(args) != 1 || args[0] != "proj_1" {
		t.Errorf("expected [proj_1], got %v", args)
	}
}

func TestBuildPropertyKeysQuery(t *testing.T) {
	t.Run("auto_with_kind", func(t *testing.T) {
		sql, args := insights.BuildAutoPropertyKeysQuery("proj_1", "page_view")
		if !strings.Contains(sql, "map_type = ?") {
			t.Error("expected map_type filter")
		}
		if !strings.Contains(sql, "kind = ?") {
			t.Error("expected kind filter")
		}
		if len(args) != 3 {
			t.Fatalf("expected 3 args, got %d", len(args))
		}
		if args[1] != "auto" {
			t.Errorf("expected map_type 'auto', got %v", args[1])
		}
	})

	t.Run("custom_without_kind", func(t *testing.T) {
		sql, args := insights.BuildCustomPropertyKeysQuery("proj_1", "")
		if strings.Contains(sql, "kind = ?") {
			t.Error("should not have kind filter when empty")
		}
		if len(args) != 2 {
			t.Fatalf("expected 2 args, got %d", len(args))
		}
		if args[1] != "custom" {
			t.Errorf("expected map_type 'custom', got %v", args[1])
		}
	})
}

func TestGroupSeries_MultiEvent(t *testing.T) {
	rows := []insights.TrendRow{
		{Time: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC), EventKind: "page_view", Value: 10},
		{Time: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC), EventKind: "purchase", Value: 3},
		{Time: time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC), EventKind: "page_view", Value: 15},
		{Time: time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC), EventKind: "purchase", Value: 5},
	}

	series := insights.GroupSeries(rows, nil)

	if len(series) != 2 {
		t.Fatalf("expected 2 series, got %d", len(series))
	}
	if series[0].EventKind != "page_view" {
		t.Errorf("expected first series 'page_view', got %q", series[0].EventKind)
	}
	if series[1].EventKind != "purchase" {
		t.Errorf("expected second series 'purchase', got %q", series[1].EventKind)
	}
	if len(series[0].Points) != 2 {
		t.Errorf("expected 2 points for page_view, got %d", len(series[0].Points))
	}
	if series[0].Points[0].Value != 10 || series[0].Points[1].Value != 15 {
		t.Errorf("unexpected page_view values: %v, %v", series[0].Points[0].Value, series[0].Points[1].Value)
	}
	if series[1].Points[0].Value != 3 || series[1].Points[1].Value != 5 {
		t.Errorf("unexpected purchase values: %v, %v", series[1].Points[0].Value, series[1].Points[1].Value)
	}
}

func TestGroupSeries_Empty(t *testing.T) {
	series := insights.GroupSeries(nil, nil)
	if len(series) != 0 {
		t.Errorf("expected 0 series for nil input, got %d", len(series))
	}
}

func TestMultiEventTrendsWithFilters(t *testing.T) {
	req := &insightsv1.QueryRequest{
		InsightType: insightsv1.InsightType_INSIGHT_TYPE_TRENDS,
		TimeRange:   timeRange("2024-01-01T00:00:00Z", "2024-01-07T23:59:59Z"),
		Granularity: insightsv1.Granularity_GRANULARITY_DAY,
		Filters: []*commonv1.PropertyFilter{
			{Property: "$country", Operator: commonv1.FilterOperator_FILTER_OPERATOR_EQUALS, Value: "US"},
		},
		Events: []*insightsv1.EventQuery{
			{
				Kind:        "page_view",
				Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL,
				Filters: []*commonv1.PropertyFilter{
					{Property: "url", Operator: commonv1.FilterOperator_FILTER_OPERATOR_CONTAINS, Value: "/blog"},
				},
			},
			{Kind: "purchase", Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_UNIQUE_USERS},
		},
	}

	sql, args, err := insights.BuildQuery(req, "proj_123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(sql, "UNION ALL") {
		t.Error("expected UNION ALL in SQL")
	}
	// Top-level filter should appear in both sub-queries.
	// PropertyExpr references the key twice (auto_properties['$country'], custom_properties['$country']),
	// so 2 sub-queries × 2 refs = 4.
	if strings.Count(sql, "$country") != 4 {
		t.Errorf("expected top-level filter in both sub-queries (4 refs), got %d", strings.Count(sql, "$country"))
	}
	// Per-event filter only in first sub-query (2 refs from PropertyExpr).
	if strings.Count(sql, "'url'") != 2 {
		t.Errorf("expected per-event filter in one sub-query (2 refs), got %d", strings.Count(sql, "'url'"))
	}
	// Verify we have args for both sub-queries' top-level + per-event filters
	// Sub1: projectID, from, to, kind, $country=US, url LIKE %/blog%
	// Sub2: projectID, from, to, kind, $country=US
	if len(args) != 11 {
		t.Errorf("expected 11 args, got %d: %v", len(args), args)
	}
}
