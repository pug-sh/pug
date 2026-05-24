package insights_test

import (
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	commonv1 "github.com/pug-sh/pug/internal/gen/proto/common/v1"
	insightsv1 "github.com/pug-sh/pug/internal/gen/proto/shared/insights/v1"

	"github.com/pug-sh/pug/internal/core/insights"
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
		Spec: &insightsv1.InsightQuerySpec{
			InsightType: insightsv1.InsightType_INSIGHT_TYPE_TRENDS.Enum(),
			Events: []*insightsv1.EventQuery{
				{Event: &commonv1.EventFilter{Kind: proto.String("page_view")}, Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL.Enum()},
			},
		},
		TimeRange:   timeRange("2024-01-01T00:00:00Z", "2024-01-07T23:59:59Z"),
		Granularity: insightsv1.Granularity_GRANULARITY_DAY.Enum(),
	}

	q, err := insights.BuildTrendsQuery(req, "proj_123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	sql, args := q.SQL(), q.Args()

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
		Spec: &insightsv1.InsightQuerySpec{
			InsightType: insightsv1.InsightType_INSIGHT_TYPE_TRENDS.Enum(),
			Events: []*insightsv1.EventQuery{
				{Event: &commonv1.EventFilter{Kind: proto.String("page_view")}, Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_UNIQUE_USERS.Enum()},
			},
			FilterGroups: []*insightsv1.FilterGroup{
				{
					Operator: commonv1.LogicalOperator_LOGICAL_OPERATOR_AND.Enum(),
					Filters: []*commonv1.PropertyFilter{
						{
							Property: proto.String("$country"),
							Operator: commonv1.FilterOperator_FILTER_OPERATOR_EQUALS.Enum(),
							Value:    proto.String("US"),
						},
					},
				},
			},
		},
		TimeRange:   timeRange("2024-01-01T00:00:00Z", "2024-01-07T23:59:59Z"),
		Granularity: insightsv1.Granularity_GRANULARITY_DAY.Enum(),
	}

	q, err := insights.BuildTrendsQuery(req, "proj_123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	sql, args := q.SQL(), q.Args()

	if !strings.Contains(sql, "toFloat64(uniq(distinct_id))") {
		t.Errorf("expected toFloat64(uniq(distinct_id)) in SQL, got: %s", sql)
	}
	if !strings.Contains(sql, "coalesce(nullIf(CAST(auto_properties['$country'] AS Nullable(String)), ''), CAST(custom_properties['$country'] AS Nullable(String)), '')") {
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
		Spec: &insightsv1.InsightQuerySpec{
			InsightType: insightsv1.InsightType_INSIGHT_TYPE_SEGMENTATION.Enum(),
			Events: []*insightsv1.EventQuery{
				{Event: &commonv1.EventFilter{Kind: proto.String("purchase")}, Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL.Enum()},
			},
		},
		TimeRange: timeRange("2024-01-01T00:00:00Z", "2024-01-07T23:59:59Z"),
	}

	q, err := insights.BuildSegmentationQuery(req, "proj_123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	sql, args := q.SQL(), q.Args()

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

func TestFunnel(t *testing.T) {
	req := &insightsv1.QueryRequest{
		Spec: &insightsv1.InsightQuerySpec{
			InsightType: insightsv1.InsightType_INSIGHT_TYPE_FUNNEL.Enum(),
			Events: []*insightsv1.EventQuery{
				{Event: &commonv1.EventFilter{Kind: proto.String("page_view")}},
				{Event: &commonv1.EventFilter{Kind: proto.String("purchase")}},
			},
		},
		TimeRange: timeRange("2024-01-01T00:00:00Z", "2024-01-07T23:59:59Z"),
	}

	q, err := insights.BuildFunnelCountsQuery(req, "proj_123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	sql, args := q.SQL(), q.Args()

	// windowFunnel-based: single CTE, no JOINs
	if !strings.Contains(sql, "WITH funnel AS") {
		t.Errorf("expected funnel CTE in SQL, got: %s", sql)
	}
	if !strings.Contains(sql, "windowFunnel(") {
		t.Errorf("expected windowFunnel() in SQL, got: %s", sql)
	}
	if !strings.Contains(sql, "countIf(level >= 1)") {
		t.Errorf("expected countIf for step 1 in SQL, got: %s", sql)
	}
	if !strings.Contains(sql, "countIf(level >= 2)") {
		t.Errorf("expected countIf for step 2 in SQL, got: %s", sql)
	}
	if !strings.Contains(sql, "ORDER BY step_index ASC") {
		t.Errorf("expected step ordering in SQL, got: %s", sql)
	}
	if !strings.Contains(sql, "CAST(? AS String) AS event_kind") {
		t.Errorf("expected parameterized event_kind in SQL, got: %s", sql)
	}

	// windowFunnel CTE args: step conditions (page_view, purchase) + step filter OR (page_view, purchase)
	// + WHERE (project_id, from, to) + outer UNION ALL: parameterized event_kind labels (page_view, purchase)
	if len(args) != 9 {
		t.Errorf("expected 9 args for 2-step windowFunnel, got %d: %v", len(args), args)
	}
}

func TestFunnelWithConversionWindow(t *testing.T) {
	req := &insightsv1.QueryRequest{
		Spec: &insightsv1.InsightQuerySpec{
			InsightType: insightsv1.InsightType_INSIGHT_TYPE_FUNNEL.Enum(),
			Events: []*insightsv1.EventQuery{
				{Event: &commonv1.EventFilter{Kind: proto.String("sign_up")}},
				{Event: &commonv1.EventFilter{Kind: proto.String("purchase")}},
			},
			ConversionWindow: durationpb.New(24 * time.Hour),
		},
		TimeRange: timeRange("2024-01-01T00:00:00Z", "2024-01-07T23:59:59Z"),
	}

	q, err := insights.BuildFunnelCountsQuery(req, "proj_123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	sql := q.SQL()

	if !strings.Contains(sql, "windowFunnel(86400)") {
		t.Errorf("expected windowFunnel(86400) for 1-day window, got: %s", sql)
	}
}

func TestFunnelDefaultWindowIsTimeRange(t *testing.T) {
	req := &insightsv1.QueryRequest{
		Spec: &insightsv1.InsightQuerySpec{
			InsightType: insightsv1.InsightType_INSIGHT_TYPE_FUNNEL.Enum(),
			// ConversionWindow absent → should default to time range
			Events: []*insightsv1.EventQuery{
				{Event: &commonv1.EventFilter{Kind: proto.String("a")}},
				{Event: &commonv1.EventFilter{Kind: proto.String("b")}},
			},
		},
		TimeRange: timeRange("2024-01-01T00:00:00Z", "2024-01-08T00:00:00Z"), // 7 days = 604800 seconds
	}

	q, err := insights.BuildFunnelCountsQuery(req, "proj_123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	sql := q.SQL()

	if !strings.Contains(sql, "windowFunnel(604800)") {
		t.Errorf("expected windowFunnel(604800) for 7-day default, got: %s", sql)
	}
}

func TestFunnelWithStepTiming(t *testing.T) {
	req := &insightsv1.QueryRequest{
		Spec: &insightsv1.InsightQuerySpec{
			InsightType:       insightsv1.InsightType_INSIGHT_TYPE_FUNNEL.Enum(),
			IncludeStepTiming: proto.Bool(true),
			ConversionWindow:  durationpb.New(2 * time.Hour),
			Events: []*insightsv1.EventQuery{
				{Event: &commonv1.EventFilter{Kind: proto.String("sign_up")}},
				{Event: &commonv1.EventFilter{Kind: proto.String("purchase")}},
			},
		},
		TimeRange: timeRange("2024-01-01T00:00:00Z", "2024-01-07T23:59:59Z"),
	}

	q, err := insights.BuildFunnelTimingQuery(req, "proj_123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Pin window propagation alongside SQL shape — a regression that wires the wrong window
	// into FunnelTimingQuery would silently disable the conversion-window check in the timing
	// path while the SQL substring assertions below still pass.
	if got, want := q.WindowSec(), int64(2*60*60); got != want {
		t.Errorf("WindowSec(): got %d, want %d", got, want)
	}
	sql, args := q.SQL(), q.Args()

	// Array-based single-scan, not windowFunnel or CTE chain
	if strings.Contains(sql, "windowFunnel") {
		t.Errorf("include_step_timing should use array approach, not windowFunnel: %s", sql)
	}
	if !strings.Contains(sql, "WITH tagged AS") {
		t.Errorf("expected tagged CTE in SQL, got: %s", sql)
	}
	if !strings.Contains(sql, "multiIf(") {
		t.Errorf("expected multiIf step tagging in SQL, got: %s", sql)
	}
	if !strings.Contains(sql, "groupArray(") {
		t.Errorf("expected groupArray for per-user arrays in SQL, got: %s", sql)
	}
	if !strings.Contains(sql, "step_matches") {
		t.Errorf("expected step_matches array in output, got: %s", sql)
	}

	// Args: multiIf(sign_up, purchase) + OR(sign_up, purchase) + WHERE(project, from, to) = 7
	if len(args) != 7 {
		t.Errorf("expected 7 args for 2-step timing funnel, got %d: %v", len(args), args)
	}
}

func TestFunnelWithFilterGroups(t *testing.T) {
	req := &insightsv1.QueryRequest{
		Spec: &insightsv1.InsightQuerySpec{
			InsightType: insightsv1.InsightType_INSIGHT_TYPE_FUNNEL.Enum(),
			Events: []*insightsv1.EventQuery{
				{Event: &commonv1.EventFilter{Kind: proto.String("sign_up")}},
				{Event: &commonv1.EventFilter{Kind: proto.String("purchase")}},
			},
			FilterGroups: []*insightsv1.FilterGroup{
				{
					Filters: []*commonv1.PropertyFilter{
						{Property: proto.String("$country"), Operator: commonv1.FilterOperator_FILTER_OPERATOR_EQUALS.Enum(), Value: proto.String("US")},
					},
				},
			},
		},
		TimeRange: timeRange("2024-01-01T00:00:00Z", "2024-01-07T23:59:59Z"),
	}

	q, err := insights.BuildFunnelCountsQuery(req, "proj_123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	sql := q.SQL()

	// Filter group should appear inside the CTE WHERE
	if !strings.Contains(sql, "$country") {
		t.Errorf("expected filter group in funnel SQL, got: %s", sql)
	}
}

func TestRetention(t *testing.T) {
	req := &insightsv1.QueryRequest{
		Spec: &insightsv1.InsightQuerySpec{
			InsightType: insightsv1.InsightType_INSIGHT_TYPE_RETENTION.Enum(),
			Events: []*insightsv1.EventQuery{
				{Event: &commonv1.EventFilter{Kind: proto.String("signup")}},
				{Event: &commonv1.EventFilter{Kind: proto.String("session")}},
			},
		},
		TimeRange:   timeRange("2024-01-01T00:00:00Z", "2024-01-07T23:59:59Z"),
		Granularity: insightsv1.Granularity_GRANULARITY_DAY.Enum(),
	}

	q, err := insights.BuildRetentionQuery(req, "proj_123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	sql, args := q.SQL(), q.Args()

	if !strings.Contains(sql, "WITH cohorts AS") {
		t.Errorf("expected cohorts CTE in SQL, got: %s", sql)
	}
	if !strings.Contains(sql, "cohort_sizes AS") {
		t.Errorf("expected cohort_sizes CTE in SQL, got: %s", sql)
	}
	if !strings.Contains(sql, "retained AS") {
		t.Errorf("expected retained CTE in SQL, got: %s", sql)
	}
	if !strings.Contains(sql, "(r.retained_users * 100.0) / cs.cohort_size") {
		t.Errorf("expected retention percentage expression in SQL, got: %s", sql)
	}
	if !strings.Contains(sql, "cs.cohort_size") {
		t.Errorf("expected cohort size selected in SQL, got: %s", sql)
	}
	if !strings.Contains(sql, "ORDER BY r.cohort_time ASC, r.t ASC") {
		t.Errorf("expected deterministic retention ordering, got: %s", sql)
	}

	// Cohorts CTE must capture precise first_event_time (not just bucketed cohort_time)
	// to avoid counting return events before the user's actual start.
	if !strings.Contains(sql, "first_event_time") {
		t.Errorf("expected first_event_time in cohorts CTE, got: %s", sql)
	}
	if !strings.Contains(sql, "e.occur_time >= c.first_event_time") {
		t.Errorf("retained CTE should filter by first_event_time, not cohort_time, got: %s", sql)
	}

	// Retained CTE conditions must use e.* aliases to avoid ambiguity in the JOIN.
	if !strings.Contains(sql, "e.kind") {
		t.Errorf("expected e.kind alias in retained CTE, got: %s", sql)
	}

	if len(args) != 8 {
		t.Errorf("expected 8 args for retention query, got %d: %v", len(args), args)
	}
}

// TestAllEvents verifies that an empty events list generates no kind filter (3 args).
func TestAllEvents(t *testing.T) {
	req := &insightsv1.QueryRequest{
		Spec: &insightsv1.InsightQuerySpec{
			InsightType: insightsv1.InsightType_INSIGHT_TYPE_TRENDS.Enum(),
			Events:      []*insightsv1.EventQuery{}, // empty = all events
		},
		TimeRange:   timeRange("2024-01-01T00:00:00Z", "2024-01-07T23:59:59Z"),
		Granularity: insightsv1.Granularity_GRANULARITY_DAY.Enum(),
	}

	q, err := insights.BuildTrendsQuery(req, "proj_123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	sql, args := q.SQL(), q.Args()

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
		Spec: &insightsv1.InsightQuerySpec{
			InsightType: insightsv1.InsightType_INSIGHT_TYPE_TRENDS.Enum(),
			Events: []*insightsv1.EventQuery{
				{Event: &commonv1.EventFilter{Kind: proto.String("page_view")}, Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL.Enum()},
				{Event: &commonv1.EventFilter{Kind: proto.String("purchase")}, Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_UNIQUE_USERS.Enum()},
			},
		},
		TimeRange:   timeRange("2024-01-01T00:00:00Z", "2024-01-07T23:59:59Z"),
		Granularity: insightsv1.Granularity_GRANULARITY_DAY.Enum(),
	}

	q, err := insights.BuildTrendsQuery(req, "proj_123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	sql, args := q.SQL(), q.Args()

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
	if !strings.Contains(sql, "toFloat64(uniq(distinct_id))") {
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
		Spec: &insightsv1.InsightQuerySpec{
			InsightType: insightsv1.InsightType_INSIGHT_TYPE_TRENDS.Enum(),
			Events: []*insightsv1.EventQuery{
				{Event: &commonv1.EventFilter{Kind: proto.String("add_to_cart")}, Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_PER_USER_AVG.Enum()},
			},
		},
		TimeRange:   timeRange("2024-01-01T00:00:00Z", "2024-01-07T23:59:59Z"),
		Granularity: insightsv1.Granularity_GRANULARITY_WEEK.Enum(),
	}

	q, err := insights.BuildTrendsQuery(req, "proj_123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	sql, args := q.SQL(), q.Args()

	if !strings.Contains(sql, "toFloat64(count(*)) / toFloat64(uniq(distinct_id))") {
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

// TestBuildTrendsQuery_WithBreakdown verifies single breakdown generates CTE + conditional bucketing.
func TestBuildTrendsQuery_WithBreakdown(t *testing.T) {
	req := &insightsv1.QueryRequest{
		Spec: &insightsv1.InsightQuerySpec{
			InsightType: insightsv1.InsightType_INSIGHT_TYPE_TRENDS.Enum(),
			Events: []*insightsv1.EventQuery{
				{Event: &commonv1.EventFilter{Kind: proto.String("page_view")}, Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL.Enum()},
			},
			Breakdowns:     []*insightsv1.Breakdown{{Property: proto.String("$country")}},
			BreakdownLimit: proto.Int32(3),
		},
		TimeRange:   timeRange("2024-01-01T00:00:00Z", "2024-01-07T23:59:59Z"),
		Granularity: insightsv1.Granularity_GRANULARITY_DAY.Enum(),
	}

	q, err := insights.BuildTrendsQuery(req, "proj_123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	sql, args := q.SQL(), q.Args()

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
		if a == int64(3) {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected breakdown limit int64(3) in args, got: %v", args)
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
		Spec: &insightsv1.InsightQuerySpec{
			InsightType: insightsv1.InsightType_INSIGHT_TYPE_TRENDS.Enum(),
			Events: []*insightsv1.EventQuery{
				{Event: &commonv1.EventFilter{Kind: proto.String("page_view")}, Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL.Enum()},
			},
			Breakdowns: []*insightsv1.Breakdown{
				{Property: proto.String("$country")},
				{Property: proto.String("$city")},
			},
			BreakdownLimit: proto.Int32(5),
		},
		TimeRange:   timeRange("2024-01-01T00:00:00Z", "2024-01-07T23:59:59Z"),
		Granularity: insightsv1.Granularity_GRANULARITY_DAY.Enum(),
	}

	q, err := insights.BuildTrendsQuery(req, "proj_123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	sql := q.SQL()

	if !strings.Contains(sql, "breakdown_0") {
		t.Errorf("expected 'breakdown_0' in SQL, got: %s", sql)
	}
	if !strings.Contains(sql, "breakdown_1") {
		t.Errorf("expected 'breakdown_1' in SQL, got: %s", sql)
	}
}

// TestBuildTrendsQuery_DefaultBreakdownLimit verifies BreakdownLimit=0 defaults to int64(10).
func TestBuildTrendsQuery_DefaultBreakdownLimit(t *testing.T) {
	req := &insightsv1.QueryRequest{
		Spec: &insightsv1.InsightQuerySpec{
			InsightType: insightsv1.InsightType_INSIGHT_TYPE_TRENDS.Enum(),
			Events: []*insightsv1.EventQuery{
				{Event: &commonv1.EventFilter{Kind: proto.String("page_view")}, Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL.Enum()},
			},
			Breakdowns:     []*insightsv1.Breakdown{{Property: proto.String("$country")}},
			BreakdownLimit: proto.Int32(0),
		},
		TimeRange:   timeRange("2024-01-01T00:00:00Z", "2024-01-07T23:59:59Z"),
		Granularity: insightsv1.Granularity_GRANULARITY_DAY.Enum(),
	}

	q, err := insights.BuildTrendsQuery(req, "proj_123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	args := q.Args()

	found := false
	for _, a := range args {
		if a == int64(10) {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected default breakdown limit int64(10) in args, got: %v", args)
	}
}

// TestFilterOperators verifies each filter operator generates correct SQL.
func TestFilterOperators(t *testing.T) {
	baseReq := func(op commonv1.FilterOperator, val string) *insightsv1.QueryRequest {
		return &insightsv1.QueryRequest{
			Spec: &insightsv1.InsightQuerySpec{
				InsightType: insightsv1.InsightType_INSIGHT_TYPE_SEGMENTATION.Enum(),
				Events: []*insightsv1.EventQuery{
					{Event: &commonv1.EventFilter{Kind: proto.String("page_view")}, Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL.Enum()},
				},
				FilterGroups: []*insightsv1.FilterGroup{
					{
						Operator: commonv1.LogicalOperator_LOGICAL_OPERATOR_AND.Enum(),
						Filters: []*commonv1.PropertyFilter{
							{Property: proto.String("$browser"), Operator: op.Enum(), Value: proto.String(val)},
						},
					},
				},
			},
			TimeRange: timeRange("2024-01-01T00:00:00Z", "2024-01-07T23:59:59Z"),
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
			q, err := insights.BuildSegmentationQuery(baseReq(tc.op, tc.val), "proj_123")
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			sql, args := q.SQL(), q.Args()
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
				Spec: &insightsv1.InsightQuerySpec{
					InsightType: insightsv1.InsightType_INSIGHT_TYPE_SEGMENTATION.Enum(),
					Events: []*insightsv1.EventQuery{
						{Event: &commonv1.EventFilter{Kind: proto.String("page_view")}, Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL.Enum()},
					},
					FilterGroups: []*insightsv1.FilterGroup{
						{
							Operator: commonv1.LogicalOperator_LOGICAL_OPERATOR_AND.Enum(),
							Filters: []*commonv1.PropertyFilter{
								{Property: proto.String("$country"), Operator: tc.op.Enum(), Values: tc.values},
							},
						},
					},
				},
				TimeRange: timeRange("2024-01-01T00:00:00Z", "2024-01-07T23:59:59Z"),
			}
			q, err := insights.BuildSegmentationQuery(req, "proj_123")
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			sql, args := q.SQL(), q.Args()
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
			{Event: &commonv1.EventFilter{Kind: proto.String("purchase")}, Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL.Enum()},
		},
		FilterGroups: []*insightsv1.FilterGroup{
			{
				Operator: commonv1.LogicalOperator_LOGICAL_OPERATOR_AND.Enum(),
				Filters: []*commonv1.PropertyFilter{
					{
						Property: proto.String("$country"),
						Operator: commonv1.FilterOperator_FILTER_OPERATOR_EQUALS.Enum(),
						Value:    proto.String("US"),
					},
				},
			},
		},
		PageSize: proto.Int32(50),
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
	if !strings.Contains(sql, "coalesce(nullIf(CAST(auto_properties['$country'] AS Nullable(String)), ''), CAST(custom_properties['$country'] AS Nullable(String)), '')") {
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
	if args[5] != int64(50) {
		t.Errorf("expected limit arg to be int64(50), got %v", args[5])
	}
}

// TestBuildSegmentUsersQuery_MultiEvent verifies OR-joined event conditions for multiple events.
func TestBuildSegmentUsersQuery_MultiEvent(t *testing.T) {
	req := &insightsv1.SegmentUsersRequest{
		TimeRange: timeRange("2024-01-01T00:00:00Z", "2024-01-07T23:59:59Z"),
		Events: []*insightsv1.EventQuery{
			{
				Event: &commonv1.EventFilter{
					Kind: proto.String("purchase"),
					Filters: []*commonv1.PropertyFilter{
						{Property: proto.String("$country"), Operator: commonv1.FilterOperator_FILTER_OPERATOR_EQUALS.Enum(), Value: proto.String("US")},
					},
				},
				Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL.Enum(),
			},
			{Event: &commonv1.EventFilter{Kind: proto.String("page_view")}, Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL.Enum()},
		},
		PageSize: proto.Int32(50),
	}

	sql, args, err := insights.BuildSegmentUsersQuery(req, "proj_123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(sql, "AND (") {
		t.Errorf("expected OR-joined event condition in SQL, got: %s", sql)
	}
	if !strings.Contains(sql, " OR ") {
		t.Errorf("expected OR between events in SQL, got: %s", sql)
	}

	// args: projectID, from, to, "US" (per-event filter), "purchase", "page_view", limit
	if len(args) != 7 {
		t.Errorf("expected 7 args, got %d: %v", len(args), args)
	}
	if args[6] != int64(50) {
		t.Errorf("expected limit arg to be int64(50), got %v", args[6])
	}
}

// TestBuildSegmentUsersQuery_WithPageToken verifies cursor pagination adds distinct_id > ? clause.
func TestBuildSegmentUsersQuery_WithPageToken(t *testing.T) {
	req := &insightsv1.SegmentUsersRequest{
		TimeRange: timeRange("2024-01-01T00:00:00Z", "2024-01-07T23:59:59Z"),
		Events: []*insightsv1.EventQuery{
			{Event: &commonv1.EventFilter{Kind: proto.String("page_view")}, Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL.Enum()},
		},
		PageToken: proto.String("user_abc"),
		PageSize:  proto.Int32(0), // should default to 100
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
		case int64:
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
		t.Errorf("default page_size 100 (int64) not found in args: %v", args)
	}
	if cursorIdx > limitIdx {
		t.Errorf("cursor arg (idx %d) should come before limit arg (idx %d)", cursorIdx, limitIdx)
	}
}

func TestFilterGroups_Query_ORBetween_ANDWithin(t *testing.T) {
	req := &insightsv1.QueryRequest{
		Spec: &insightsv1.InsightQuerySpec{
			InsightType: insightsv1.InsightType_INSIGHT_TYPE_SEGMENTATION.Enum(),
			Events: []*insightsv1.EventQuery{
				{Event: &commonv1.EventFilter{Kind: proto.String("purchase")}, Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL.Enum()},
			},
			FilterGroupsOperator: commonv1.LogicalOperator_LOGICAL_OPERATOR_OR.Enum(),
			FilterGroups: []*insightsv1.FilterGroup{
				{
					Operator: commonv1.LogicalOperator_LOGICAL_OPERATOR_AND.Enum(),
					Filters: []*commonv1.PropertyFilter{
						{Property: proto.String("$country"), Operator: commonv1.FilterOperator_FILTER_OPERATOR_EQUALS.Enum(), Value: proto.String("US")},
						{Property: proto.String("$browser"), Operator: commonv1.FilterOperator_FILTER_OPERATOR_EQUALS.Enum(), Value: proto.String("Chrome")},
					},
				},
				{
					Operator: commonv1.LogicalOperator_LOGICAL_OPERATOR_AND.Enum(),
					Filters: []*commonv1.PropertyFilter{
						{Property: proto.String("$country"), Operator: commonv1.FilterOperator_FILTER_OPERATOR_EQUALS.Enum(), Value: proto.String("IN")},
						{Property: proto.String("$browser"), Operator: commonv1.FilterOperator_FILTER_OPERATOR_EQUALS.Enum(), Value: proto.String("Safari")},
					},
				},
			},
		},
		TimeRange: timeRange("2024-01-01T00:00:00Z", "2024-01-07T23:59:59Z"),
	}

	q, err := insights.BuildSegmentationQuery(req, "proj_123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	sql, args := q.SQL(), q.Args()

	if !strings.Contains(sql, "AND ((") {
		t.Errorf("expected grouped top-level filter clause in SQL, got: %s", sql)
	}
	if !strings.Contains(sql, ") OR (") {
		t.Errorf("expected OR between filter groups in SQL, got: %s", sql)
	}
	if len(args) != 8 {
		t.Fatalf("expected 8 args (projectID, from, to, 4 filter values, kind), got %d: %v", len(args), args)
	}
}

func TestFilterGroups_SegmentUsers_ORWithinGroup(t *testing.T) {
	req := &insightsv1.SegmentUsersRequest{
		TimeRange: timeRange("2024-01-01T00:00:00Z", "2024-01-07T23:59:59Z"),
		Events: []*insightsv1.EventQuery{
			{Event: &commonv1.EventFilter{Kind: proto.String("page_view")}, Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL.Enum()},
		},
		FilterGroups: []*insightsv1.FilterGroup{
			{
				Operator: commonv1.LogicalOperator_LOGICAL_OPERATOR_OR.Enum(),
				Filters: []*commonv1.PropertyFilter{
					{Property: proto.String("$country"), Operator: commonv1.FilterOperator_FILTER_OPERATOR_EQUALS.Enum(), Value: proto.String("US")},
					{Property: proto.String("$country"), Operator: commonv1.FilterOperator_FILTER_OPERATOR_EQUALS.Enum(), Value: proto.String("IN")},
				},
			},
		},
		PageSize: proto.Int32(10),
	}

	sql, args, err := insights.BuildSegmentUsersQuery(req, "proj_123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(sql, " OR ") {
		t.Errorf("expected OR within filter group in SQL, got: %s", sql)
	}
	if len(args) != 7 {
		t.Fatalf("expected 7 args (projectID, from, to, 2 filter values, kind, limit), got %d: %v", len(args), args)
	}
}

func TestFilterGroups_EmptyGroupReturnsError(t *testing.T) {
	req := &insightsv1.QueryRequest{
		Spec: &insightsv1.InsightQuerySpec{
			InsightType: insightsv1.InsightType_INSIGHT_TYPE_SEGMENTATION.Enum(),
			Events: []*insightsv1.EventQuery{
				{Event: &commonv1.EventFilter{Kind: proto.String("purchase")}, Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL.Enum()},
			},
			FilterGroups: []*insightsv1.FilterGroup{
				{},
			},
		},
		TimeRange: timeRange("2024-01-01T00:00:00Z", "2024-01-07T23:59:59Z"),
	}

	if _, err := insights.BuildSegmentationQuery(req, "proj_123"); err == nil {
		t.Fatal("expected error for empty filter group, got nil")
	} else if !strings.Contains(err.Error(), "filter_groups[0]") {
		t.Fatalf("expected error to mention filter_groups[0], got: %v", err)
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
				Spec: &insightsv1.InsightQuerySpec{
					InsightType: insightsv1.InsightType_INSIGHT_TYPE_SEGMENTATION.Enum(),
					Events: []*insightsv1.EventQuery{
						{Event: &commonv1.EventFilter{Kind: proto.String("click")}, Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL.Enum()},
					},
					FilterGroups: []*insightsv1.FilterGroup{
						{
							Operator: commonv1.LogicalOperator_LOGICAL_OPERATOR_AND.Enum(),
							Filters: []*commonv1.PropertyFilter{
								{Property: proto.String("score"), Operator: op.Enum(), Value: proto.String("not-a-number")},
							},
						},
					},
				},
				TimeRange: timeRange("2024-01-01T00:00:00Z", "2024-01-07T23:59:59Z"),
			}
			if _, err := insights.BuildSegmentationQuery(req, "proj_123"); err == nil {
				t.Fatal("expected error for non-numeric value, got nil")
			} else if !strings.Contains(err.Error(), "invalid numeric value") {
				t.Errorf("expected 'invalid numeric value' in error, got: %v", err)
			}
		})
	}
}

func TestMultipleCombinedFilters(t *testing.T) {
	req := &insightsv1.QueryRequest{
		Spec: &insightsv1.InsightQuerySpec{
			InsightType: insightsv1.InsightType_INSIGHT_TYPE_TRENDS.Enum(),
			Events: []*insightsv1.EventQuery{
				{Event: &commonv1.EventFilter{Kind: proto.String("page_view")}, Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL.Enum()},
			},
			FilterGroups: []*insightsv1.FilterGroup{
				{
					Operator: commonv1.LogicalOperator_LOGICAL_OPERATOR_AND.Enum(),
					Filters: []*commonv1.PropertyFilter{
						{Property: proto.String("$country"), Operator: commonv1.FilterOperator_FILTER_OPERATOR_EQUALS.Enum(), Value: proto.String("US")},
						{Property: proto.String("$browser"), Operator: commonv1.FilterOperator_FILTER_OPERATOR_CONTAINS.Enum(), Value: proto.String("Chrome")},
						{Property: proto.String("age"), Operator: commonv1.FilterOperator_FILTER_OPERATOR_GTE.Enum(), Value: proto.String("18")},
					},
				},
			},
		},
		TimeRange:   timeRange("2024-01-01T00:00:00Z", "2024-01-07T23:59:59Z"),
		Granularity: insightsv1.Granularity_GRANULARITY_DAY.Enum(),
	}

	q, err := insights.BuildTrendsQuery(req, "proj_123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	sql, args := q.SQL(), q.Args()

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

func TestGranularityFunc(t *testing.T) {
	tests := []struct {
		name        string
		granularity insightsv1.Granularity
		wantFunc    string // empty when wantErr is true
		wantErr     bool
	}{
		{name: "minute", granularity: insightsv1.Granularity_GRANULARITY_MINUTE, wantFunc: "toStartOfMinute"},
		{name: "hour", granularity: insightsv1.Granularity_GRANULARITY_HOUR, wantFunc: "toStartOfHour"},
		{name: "day", granularity: insightsv1.Granularity_GRANULARITY_DAY, wantFunc: "toStartOfDay"},
		{name: "week", granularity: insightsv1.Granularity_GRANULARITY_WEEK, wantFunc: "toStartOfWeek"},
		{name: "month", granularity: insightsv1.Granularity_GRANULARITY_MONTH, wantFunc: "toStartOfMonth"},
		{name: "unspecified_errors", granularity: insightsv1.Granularity_GRANULARITY_UNSPECIFIED, wantErr: true},
		{name: "undefined_value_errors", granularity: insightsv1.Granularity(999), wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := &insightsv1.QueryRequest{
				Spec: &insightsv1.InsightQuerySpec{
					InsightType: insightsv1.InsightType_INSIGHT_TYPE_TRENDS.Enum(),
					Events: []*insightsv1.EventQuery{
						{Event: &commonv1.EventFilter{Kind: proto.String("page_view")}, Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL.Enum()},
					},
				},
				TimeRange:   timeRange("2024-01-01T00:00:00Z", "2024-01-07T23:59:59Z"),
				Granularity: tc.granularity.Enum(),
			}

			q, err := insights.BuildTrendsQuery(req, "proj_123")
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !strings.Contains(q.SQL(), tc.wantFunc) {
				t.Errorf("expected %s in SQL, got: %s", tc.wantFunc, q.SQL())
			}
		})
	}
}

// TestGranularityFunc_Retention mirrors TestGranularityFunc through BuildRetentionQuery,
// pinning that granularityFunc routing is consistent across both insights builders.
func TestGranularityFunc_Retention(t *testing.T) {
	tests := []struct {
		name        string
		granularity insightsv1.Granularity
		wantFunc    string // empty when wantErr is true
		wantErr     bool
	}{
		{name: "minute", granularity: insightsv1.Granularity_GRANULARITY_MINUTE, wantFunc: "toStartOfMinute"},
		{name: "hour", granularity: insightsv1.Granularity_GRANULARITY_HOUR, wantFunc: "toStartOfHour"},
		{name: "day", granularity: insightsv1.Granularity_GRANULARITY_DAY, wantFunc: "toStartOfDay"},
		{name: "week", granularity: insightsv1.Granularity_GRANULARITY_WEEK, wantFunc: "toStartOfWeek"},
		{name: "month", granularity: insightsv1.Granularity_GRANULARITY_MONTH, wantFunc: "toStartOfMonth"},
		{name: "unspecified_errors", granularity: insightsv1.Granularity_GRANULARITY_UNSPECIFIED, wantErr: true},
		{name: "undefined_value_errors", granularity: insightsv1.Granularity(999), wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := &insightsv1.QueryRequest{
				Spec: &insightsv1.InsightQuerySpec{
					InsightType: insightsv1.InsightType_INSIGHT_TYPE_RETENTION.Enum(),
					Events: []*insightsv1.EventQuery{
						{Event: &commonv1.EventFilter{Kind: proto.String("signup")}},
						{Event: &commonv1.EventFilter{Kind: proto.String("login")}},
					},
				},
				TimeRange:   timeRange("2024-01-01T00:00:00Z", "2024-01-07T23:59:59Z"),
				Granularity: tc.granularity.Enum(),
			}

			q, err := insights.BuildRetentionQuery(req, "proj_123")
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !strings.Contains(q.SQL(), tc.wantFunc) {
				t.Errorf("expected %s in SQL, got: %s", tc.wantFunc, q.SQL())
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

	series, err := insights.GroupSeries(t.Context(), rows, []string{"$country"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

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
	if series[0].Points[0].GetValue() != 10 {
		t.Errorf("expected first US point value=10, got %v", series[0].Points[0].GetValue())
	}
	if series[1].Points[1].GetValue() != 8 {
		t.Errorf("expected second GB point value=8, got %v", series[1].Points[1].GetValue())
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
				Spec: &insightsv1.InsightQuerySpec{
					InsightType: insightsv1.InsightType_INSIGHT_TYPE_SEGMENTATION.Enum(),
					Events: []*insightsv1.EventQuery{
						{Event: &commonv1.EventFilter{Kind: proto.String("click")}, Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL.Enum()},
					},
					FilterGroups: []*insightsv1.FilterGroup{
						{
							Operator: commonv1.LogicalOperator_LOGICAL_OPERATOR_AND.Enum(),
							Filters: []*commonv1.PropertyFilter{
								{Property: proto.String("url"), Operator: commonv1.FilterOperator_FILTER_OPERATOR_CONTAINS.Enum(), Value: proto.String(tc.val)},
							},
						},
					},
				},
				TimeRange: timeRange("2024-01-01T00:00:00Z", "2024-01-07T23:59:59Z"),
			}

			q, err := insights.BuildSegmentationQuery(req, "proj_123")
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			args := q.Args()

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
		sql, args, err := insights.BuildAutoPropertyValuesQuery("proj_1", "$browser", "page_view")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !strings.Contains(sql, "auto_properties") {
			t.Error("expected auto_properties in SQL")
		}
		if !strings.Contains(sql, "kind = ?") {
			t.Error("expected kind filter in SQL")
		}
		if len(args) != 5 {
			t.Fatalf("expected 5 args, got %d: %v", len(args), args)
		}
		// args: propertyKey, projectID, eventKind, propertyKey, limit
		if args[0] != "$browser" || args[1] != "proj_1" || args[2] != "page_view" || args[3] != "$browser" || args[4] != int64(100) {
			t.Errorf("unexpected args: %v", args)
		}
	})

	t.Run("without_event_kind", func(t *testing.T) {
		sql, args, err := insights.BuildAutoPropertyValuesQuery("proj_1", "$browser", "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !strings.Contains(sql, "auto_properties") {
			t.Error("expected auto_properties in SQL")
		}
		if strings.Contains(sql, "kind = ?") {
			t.Error("should not have kind filter when eventKind is empty")
		}
		if len(args) != 4 {
			t.Fatalf("expected 4 args, got %d: %v", len(args), args)
		}
		if args[0] != "$browser" || args[1] != "proj_1" || args[2] != "$browser" || args[3] != int64(100) {
			t.Errorf("unexpected args: %v", args)
		}
	})

	t.Run("custom_variant", func(t *testing.T) {
		sql, _, err := insights.BuildCustomPropertyValuesQuery("proj_1", "plan", "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !strings.Contains(sql, "custom_properties") {
			t.Error("expected custom_properties in SQL")
		}
		if strings.Contains(sql, "auto_properties") {
			t.Error("should not contain auto_properties")
		}
		// Pin the Variant CAST shape on both the SELECT and the non-empty
		// guard. Dropping CAST(... AS Nullable(String)) silently returns raw
		// Variant strings (e.g. "42 :: Int64") and breaks the != '' filter.
		if !strings.Contains(sql, "CAST(custom_properties[?] AS Nullable(String)) AS value") {
			t.Errorf("expected CAST(custom_properties[?] AS Nullable(String)) AS value in SQL, got: %s", sql)
		}
		if !strings.Contains(sql, "CAST(custom_properties[?] AS Nullable(String)) != ''") {
			t.Errorf("expected CAST(custom_properties[?] AS Nullable(String)) != '' in SQL, got: %s", sql)
		}
	})

	t.Run("auto_uses_cast_like_custom", func(t *testing.T) {
		sql, _, err := insights.BuildAutoPropertyValuesQuery("proj_1", "$browser", "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !strings.Contains(sql, "CAST(auto_properties[?] AS Nullable(String)) AS value") {
			t.Errorf("expected CAST(auto_properties[?] AS Nullable(String)) AS value in SQL, got: %s", sql)
		}
		if !strings.Contains(sql, "CAST(auto_properties[?] AS Nullable(String)) != ''") {
			t.Errorf("expected CAST(auto_properties[?] AS Nullable(String)) != '' in SQL, got: %s", sql)
		}
	})
}

func TestBuildEventNamesQuery(t *testing.T) {
	sql, args, err := insights.BuildEventNamesQuery("proj_1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sql, "event_names") {
		t.Error("expected event_names table in SQL")
	}
	if !strings.Contains(sql, "countMerge(event_count)") {
		t.Error("expected countMerge in SQL")
	}
	if !strings.Contains(sql, "maxMerge(last_seen)") {
		t.Error("expected maxMerge in SQL")
	}
	if len(args) != 2 || args[0] != "proj_1" {
		t.Errorf("expected [proj_1, 1000], got %v", args)
	}
	if len(args) != 2 || args[1] != int64(1000) {
		t.Errorf("expected limit arg int64(1000), got %v", args)
	}
}

func TestBuildPropertyKeysQuery(t *testing.T) {
	t.Run("auto_with_kind", func(t *testing.T) {
		sql, args, err := insights.BuildAutoPropertyKeysQuery("proj_1", "page_view")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(sql, "map_type = ?") {
			t.Error("expected map_type filter")
		}
		if !strings.Contains(sql, "kind = ?") {
			t.Error("expected kind filter")
		}
		if len(args) != 4 {
			t.Fatalf("expected 4 args, got %d", len(args))
		}
		if args[1] != "auto" {
			t.Errorf("expected map_type 'auto', got %v", args[1])
		}
		if args[3] != int64(500) {
			t.Errorf("expected limit 500, got %v", args[3])
		}
	})

	t.Run("custom_without_kind", func(t *testing.T) {
		sql, args, err := insights.BuildCustomPropertyKeysQuery("proj_1", "")
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(sql, "kind = ?") {
			t.Error("should not have kind filter when empty")
		}
		if len(args) != 3 {
			t.Fatalf("expected 3 args, got %d", len(args))
		}
		if args[1] != "custom" {
			t.Errorf("expected map_type 'custom', got %v", args[1])
		}
		if args[2] != int64(500) {
			t.Errorf("expected limit 500, got %v", args[2])
		}
	})

	t.Run("selects_value_type_aggregate", func(t *testing.T) {
		// Pin the value_type aggregation. Dropping it produces a column-count
		// mismatch in QueryAggregateKeys (which dispatches on column presence);
		// silently replacing it with a non-aggregate would break the GROUP BY.
		// argMin gives first-touch semantics so dashboards see a stable type
		// across calls. The max(last_seen) projection is aliased to
		// last_seen_max to avoid ClickHouse aliasing the column inside argMin.
		sql, _, err := insights.BuildCustomPropertyKeysQuery("proj_1", "")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(sql, "argMin(value_type, last_seen) AS value_type") {
			t.Errorf("expected argMin(value_type, last_seen) AS value_type in SQL, got: %s", sql)
		}
		if !strings.Contains(sql, "sum(event_count) AS count") {
			t.Errorf("expected sum(event_count) AS count in SQL, got: %s", sql)
		}
		if !strings.Contains(sql, "max(last_seen) AS last_seen_max") {
			t.Errorf("expected max(last_seen) AS last_seen_max in SQL, got: %s", sql)
		}
	})
}

func TestBuildProfilePropertyKeysQuery(t *testing.T) {
	sql, args, err := insights.BuildProfilePropertyKeysQuery("proj_1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sql, "map_type = ?") {
		t.Error("expected map_type filter")
	}
	if strings.Contains(sql, "kind = ?") {
		t.Error("profile keys should not have kind filter")
	}
	if len(args) != 3 {
		t.Fatalf("expected 3 args, got %d: %v", len(args), args)
	}
	if args[0] != "proj_1" {
		t.Errorf("expected project_id 'proj_1', got %v", args[0])
	}
	if args[1] != "profile" {
		t.Errorf("expected map_type 'profile', got %v", args[1])
	}
}

func TestBuildProfilePropertyValuesQuery(t *testing.T) {
	sql, args, err := insights.BuildProfilePropertyValuesQuery("proj_1", "$name")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sql, "coalesce(CAST(properties.`$name` AS Nullable(String)), '')") {
		t.Errorf("expected JSON subcolumn read for profile property access, got: %s", sql)
	}
	if !strings.Contains(sql, "is_deleted = ?") {
		t.Error("expected is_deleted guard")
	}
	if !strings.Contains(sql, "profiles") {
		t.Error("expected profiles table")
	}
	if len(args) != 3 {
		t.Fatalf("expected 3 args, got %d: %v", len(args), args)
	}
	if args[0] != "proj_1" {
		t.Errorf("expected project_id 'proj_1', got %v", args[0])
	}
	if args[2] != int64(100) {
		t.Errorf("expected limit 100, got %v", args[2])
	}
}

func TestGroupSeries_MultiEvent(t *testing.T) {
	rows := []insights.TrendRow{
		{Time: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC), EventKind: "page_view", Value: 10},
		{Time: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC), EventKind: "purchase", Value: 3},
		{Time: time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC), EventKind: "page_view", Value: 15},
		{Time: time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC), EventKind: "purchase", Value: 5},
	}

	series, err := insights.GroupSeries(t.Context(), rows, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(series) != 2 {
		t.Fatalf("expected 2 series, got %d", len(series))
	}
	if series[0].GetEventKind() != "page_view" {
		t.Errorf("expected first series 'page_view', got %q", series[0].GetEventKind())
	}
	if series[1].GetEventKind() != "purchase" {
		t.Errorf("expected second series 'purchase', got %q", series[1].GetEventKind())
	}
	if len(series[0].Points) != 2 {
		t.Errorf("expected 2 points for page_view, got %d", len(series[0].Points))
	}
	if series[0].Points[0].GetValue() != 10 || series[0].Points[1].GetValue() != 15 {
		t.Errorf("unexpected page_view values: %v, %v", series[0].Points[0].GetValue(), series[0].Points[1].GetValue())
	}
	if series[1].Points[0].GetValue() != 3 || series[1].Points[1].GetValue() != 5 {
		t.Errorf("unexpected purchase values: %v, %v", series[1].Points[0].GetValue(), series[1].Points[1].GetValue())
	}
}

func TestGroupSeries_SortsPointsByTime(t *testing.T) {
	rows := []insights.TrendRow{
		{Time: mustTime("2024-01-03T00:00:00Z"), EventKind: "page_view", Value: 30},
		{Time: mustTime("2024-01-01T00:00:00Z"), EventKind: "page_view", Value: 10},
		{Time: mustTime("2024-01-02T00:00:00Z"), EventKind: "page_view", Value: 20},
	}

	series, err := insights.GroupSeries(t.Context(), rows, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(series) != 1 {
		t.Fatalf("expected 1 series, got %d", len(series))
	}
	pts := series[0].Points
	if len(pts) != 3 {
		t.Fatalf("expected 3 points, got %d", len(pts))
	}
	for i := 1; i < len(pts); i++ {
		prev := pts[i-1].GetTime().AsTime()
		curr := pts[i].GetTime().AsTime()
		if !prev.Before(curr) {
			t.Errorf("points not sorted: pts[%d]=%v >= pts[%d]=%v", i-1, prev, i, curr)
		}
	}
	if pts[0].GetValue() != 10 || pts[1].GetValue() != 20 || pts[2].GetValue() != 30 {
		t.Errorf("unexpected values: %v, %v, %v", pts[0].GetValue(), pts[1].GetValue(), pts[2].GetValue())
	}
}

func TestGroupSeries_SortsPointsByTime_Breakdowns(t *testing.T) {
	// Simulate UNION ALL interleaving: rows arrive scrambled across breakdown groups.
	rows := []insights.TrendRow{
		{Time: mustTime("2024-01-03T00:00:00Z"), EventKind: "page_view", Breakdowns: []string{"US"}, Value: 30},
		{Time: mustTime("2024-01-01T00:00:00Z"), EventKind: "page_view", Breakdowns: []string{"GB"}, Value: 5},
		{Time: mustTime("2024-01-01T00:00:00Z"), EventKind: "page_view", Breakdowns: []string{"US"}, Value: 10},
		{Time: mustTime("2024-01-02T00:00:00Z"), EventKind: "page_view", Breakdowns: []string{"GB"}, Value: 8},
		{Time: mustTime("2024-01-02T00:00:00Z"), EventKind: "page_view", Breakdowns: []string{"US"}, Value: 20},
	}

	series, err := insights.GroupSeries(t.Context(), rows, []string{"$country"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(series) != 2 {
		t.Fatalf("expected 2 series, got %d", len(series))
	}
	for _, s := range series {
		for i := 1; i < len(s.Points); i++ {
			prev := s.Points[i-1].GetTime().AsTime()
			curr := s.Points[i].GetTime().AsTime()
			if prev.After(curr) {
				t.Errorf("series %v: points not sorted: pts[%d]=%v > pts[%d]=%v",
					s.Breakdown, i-1, prev, i, curr)
			}
		}
	}
	// US series: 10, 20, 30
	if series[0].Points[0].GetValue() != 10 || series[0].Points[1].GetValue() != 20 || series[0].Points[2].GetValue() != 30 {
		t.Errorf("unexpected US values: %v, %v, %v",
			series[0].Points[0].GetValue(), series[0].Points[1].GetValue(), series[0].Points[2].GetValue())
	}
	// GB series: 5, 8
	if series[1].Points[0].GetValue() != 5 || series[1].Points[1].GetValue() != 8 {
		t.Errorf("unexpected GB values: %v, %v",
			series[1].Points[0].GetValue(), series[1].Points[1].GetValue())
	}
}

func TestGroupSeries_SortsPointsByTime_MultiEvent(t *testing.T) {
	// Simulate UNION ALL interleaving: rows from different event kinds arrive out of time order.
	rows := []insights.TrendRow{
		{Time: time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC), EventKind: "page_view", Value: 15},
		{Time: time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC), EventKind: "purchase", Value: 5},
		{Time: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC), EventKind: "page_view", Value: 10},
		{Time: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC), EventKind: "purchase", Value: 3},
	}

	series, err := insights.GroupSeries(t.Context(), rows, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(series) != 2 {
		t.Fatalf("expected 2 series, got %d", len(series))
	}
	for _, s := range series {
		for i := 1; i < len(s.Points); i++ {
			prev := s.Points[i-1].GetTime().AsTime()
			curr := s.Points[i].GetTime().AsTime()
			if prev.After(curr) {
				t.Errorf("series %q: points not sorted: pts[%d]=%v > pts[%d]=%v",
					s.GetEventKind(), i-1, prev, i, curr)
			}
		}
	}
	if series[0].Points[0].GetValue() != 10 || series[0].Points[1].GetValue() != 15 {
		t.Errorf("unexpected page_view values: %v, %v", series[0].Points[0].GetValue(), series[0].Points[1].GetValue())
	}
	if series[1].Points[0].GetValue() != 3 || series[1].Points[1].GetValue() != 5 {
		t.Errorf("unexpected purchase values: %v, %v", series[1].Points[0].GetValue(), series[1].Points[1].GetValue())
	}
}

func TestGroupSeries_Empty(t *testing.T) {
	series, err := insights.GroupSeries(t.Context(), nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(series) != 0 {
		t.Errorf("expected 0 series for nil input, got %d", len(series))
	}
}

func TestGroupRetentionSeries(t *testing.T) {
	rows := []insights.RetentionRow{
		{
			CohortTime: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
			Time:       time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
			Value:      100,
			CohortSize: 10,
		},
		{
			CohortTime: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
			Time:       time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC),
			Value:      50,
			CohortSize: 10,
		},
		{
			CohortTime: time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC),
			Time:       time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC),
			Value:      100,
			CohortSize: 5,
		},
	}

	series, err := insights.GroupRetentionSeries(t.Context(), rows, nil)
	if err != nil {
		t.Fatalf("GroupRetentionSeries: %v", err)
	}
	if len(series) != 1 {
		t.Fatalf("expected 1 series (no breakdown), got %d", len(series))
	}
	cohorts := series[0].Cohorts
	if len(cohorts) != 2 {
		t.Fatalf("expected 2 cohorts, got %d", len(cohorts))
	}
	if cohorts[0].GetCohort() != "2024-01-01T00:00:00Z" {
		t.Errorf("unexpected first cohort label: %q", cohorts[0].GetCohort())
	}
	if len(cohorts[0].Points) != 2 {
		t.Errorf("expected 2 points for first cohort, got %d", len(cohorts[0].Points))
	}
	if cohorts[0].Points[1].GetValue() != 50 {
		t.Errorf("unexpected retained value: %v", cohorts[0].Points[1].GetValue())
	}
	if cohorts[0].GetCohortSize() != 10 {
		t.Errorf("unexpected first cohort size: %v", cohorts[0].GetCohortSize())
	}
	if cohorts[1].GetCohortSize() != 5 {
		t.Errorf("unexpected second cohort size: %v", cohorts[1].GetCohortSize())
	}
}

func TestMultiEventTrendsWithFilters(t *testing.T) {
	req := &insightsv1.QueryRequest{
		Spec: &insightsv1.InsightQuerySpec{
			InsightType: insightsv1.InsightType_INSIGHT_TYPE_TRENDS.Enum(),
			FilterGroups: []*insightsv1.FilterGroup{
				{
					Operator: commonv1.LogicalOperator_LOGICAL_OPERATOR_AND.Enum(),
					Filters: []*commonv1.PropertyFilter{
						{Property: proto.String("$country"), Operator: commonv1.FilterOperator_FILTER_OPERATOR_EQUALS.Enum(), Value: proto.String("US")},
					},
				},
			},
			Events: []*insightsv1.EventQuery{
				{
					Event: &commonv1.EventFilter{
						Kind: proto.String("page_view"),
						Filters: []*commonv1.PropertyFilter{
							{Property: proto.String("url"), Operator: commonv1.FilterOperator_FILTER_OPERATOR_CONTAINS.Enum(), Value: proto.String("/blog")},
						},
					},
					Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL.Enum(),
				},
				{Event: &commonv1.EventFilter{Kind: proto.String("purchase")}, Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_UNIQUE_USERS.Enum()},
			},
		},
		TimeRange:   timeRange("2024-01-01T00:00:00Z", "2024-01-07T23:59:59Z"),
		Granularity: insightsv1.Granularity_GRANULARITY_DAY.Enum(),
	}

	q, err := insights.BuildTrendsQuery(req, "proj_123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	sql, args := q.SQL(), q.Args()

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

// TestNotBetweenEventFilterParenthesization verifies that NOT_BETWEEN in an event-level filter is
// properly parenthesized so that AND/OR precedence does not cause other event kinds to leak through.
func TestNotBetweenEventFilterParenthesization(t *testing.T) {
	req := &insightsv1.QueryRequest{
		Spec: &insightsv1.InsightQuerySpec{
			InsightType: insightsv1.InsightType_INSIGHT_TYPE_TRENDS.Enum(),
			Events: []*insightsv1.EventQuery{
				{
					Event: &commonv1.EventFilter{
						Kind: proto.String("add_to_cart"),
						Filters: []*commonv1.PropertyFilter{
							{Property: proto.String("amount"), Operator: commonv1.FilterOperator_FILTER_OPERATOR_NOT_BETWEEN.Enum(), Values: []string{"100", "200"}},
						},
					},
					Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL.Enum(),
				},
			},
		},
		TimeRange:   timeRange("2024-01-01T00:00:00Z", "2024-01-07T23:59:59Z"),
		Granularity: insightsv1.Granularity_GRANULARITY_DAY.Enum(),
	}

	q, err := insights.BuildTrendsQuery(req, "proj_123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// The NOT BETWEEN clause must be parenthesized so that AND binds the kind filter
	// to the full (< OR >) expression, not just its first branch.
	// Without parens the SQL reads: kind = ? AND amount < ? OR amount > ?
	// which is: (kind = ? AND amount < ?) OR (amount > ?) — leaking other event kinds.
	if !strings.Contains(q.SQL(), "(if(nullIf(CAST(auto_properties['amount'] AS Nullable(String)), '') IS NOT NULL,") {
		t.Errorf("expected NOT BETWEEN clause to be parenthesized in SQL, got: %s", q.SQL())
	}
}

// TestMultiEventTrendsWithBreakdowns verifies UNION ALL + CTE for multiple events with breakdowns.
func TestMultiEventTrendsWithBreakdowns(t *testing.T) {
	req := &insightsv1.QueryRequest{
		Spec: &insightsv1.InsightQuerySpec{
			InsightType: insightsv1.InsightType_INSIGHT_TYPE_TRENDS.Enum(),
			Events: []*insightsv1.EventQuery{
				{Event: &commonv1.EventFilter{Kind: proto.String("page_view")}, Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL.Enum()},
				{Event: &commonv1.EventFilter{Kind: proto.String("purchase")}, Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_UNIQUE_USERS.Enum()},
			},
			Breakdowns:     []*insightsv1.Breakdown{{Property: proto.String("$country")}},
			BreakdownLimit: proto.Int32(5),
		},
		TimeRange:   timeRange("2024-01-01T00:00:00Z", "2024-01-07T23:59:59Z"),
		Granularity: insightsv1.Granularity_GRANULARITY_DAY.Enum(),
	}

	q, err := insights.BuildTrendsQuery(req, "proj_123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	sql, args := q.SQL(), q.Args()

	// Must have both UNION ALL and CTE.
	if !strings.Contains(sql, "UNION ALL") {
		t.Error("expected UNION ALL in SQL")
	}
	if !strings.Contains(sql, "top_vals") {
		t.Error("expected CTE 'top_vals' in SQL")
	}

	// Both sub-queries should reference top_vals for breakdown bucketing.
	if strings.Count(sql, "FROM top_vals") != 2 {
		t.Errorf("expected 2 references to top_vals (one per sub-query), got %d", strings.Count(sql, "FROM top_vals"))
	}

	// Both sub-queries should have breakdown_0 in SELECT and GROUP BY.
	if strings.Count(sql, "AS breakdown_0") < 3 {
		t.Errorf("expected at least 3 breakdown_0 aliases (CTE + 2 sub-queries), got %d", strings.Count(sql, "AS breakdown_0"))
	}

	// CTE args: projectID, from, to, kind1, kind2, limit = 6
	// Sub1 args: projectID, from, to, kind1 = 4
	// Sub2 args: projectID, from, to, kind2 = 4
	// Total = 14
	if len(args) != 14 {
		t.Fatalf("expected 14 args (CTE x6 + sub1 x4 + sub2 x4), got %d: %v", len(args), args)
	}

	// Verify the breakdown limit arg (int64(5)) is present.
	found := false
	for _, a := range args {
		if a == int64(5) {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected breakdown limit int64(5) in args, got: %v", args)
	}
}

// TestSingleEventRetention verifies retention with a single event used as both cohort and return.
func TestSingleEventRetention(t *testing.T) {
	req := &insightsv1.QueryRequest{
		Spec: &insightsv1.InsightQuerySpec{
			InsightType: insightsv1.InsightType_INSIGHT_TYPE_RETENTION.Enum(),
			Events: []*insightsv1.EventQuery{
				{Event: &commonv1.EventFilter{Kind: proto.String("login")}},
			},
		},
		TimeRange:   timeRange("2024-01-01T00:00:00Z", "2024-01-31T23:59:59Z"),
		Granularity: insightsv1.Granularity_GRANULARITY_WEEK.Enum(),
	}

	q, err := insights.BuildRetentionQuery(req, "proj_123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	sql, args := q.SQL(), q.Args()

	// Retention structure: cohorts CTE + cohort_sizes CTE + retained CTE + main query.
	if !strings.Contains(sql, "cohorts") {
		t.Error("expected 'cohorts' CTE in SQL")
	}
	if !strings.Contains(sql, "retained") {
		t.Error("expected 'retained' CTE in SQL")
	}

	// The same event kind should appear in both the cohorts CTE and the retained CTE.
	// cohorts: kind = ? (start event)
	// retained: e.kind = ? (return event, aliased)
	if strings.Count(sql, "kind = ?") < 2 {
		t.Errorf("expected kind condition in both cohorts and retained CTEs, got %d occurrences", strings.Count(sql, "kind = ?"))
	}

	// Granularity should be weekly.
	if !strings.Contains(sql, "toStartOfWeek") {
		t.Error("expected toStartOfWeek granularity in SQL")
	}

	// Args should include projectID and time range for both cohorts and retained CTEs,
	// plus the kind arg for each.
	// cohorts: projectID, from, to, kind = 4
	// retained: projectID, from, to, kind = 4
	// Total = 8
	if len(args) != 8 {
		t.Fatalf("expected 8 args (cohorts x4 + retained x4), got %d: %v", len(args), args)
	}

	// Both kind args should be "login".
	kindCount := 0
	for _, a := range args {
		if a == "login" {
			kindCount++
		}
	}
	if kindCount != 2 {
		t.Errorf("expected 2 'login' kind args (start + return), got %d", kindCount)
	}
}

func TestGroupRetentionSeries_Empty(t *testing.T) {
	series, err := insights.GroupRetentionSeries(t.Context(), nil, nil)
	if err != nil {
		t.Fatalf("GroupRetentionSeries(nil): %v", err)
	}
	if len(series) != 0 {
		t.Errorf("expected 0 series for nil input, got %d", len(series))
	}
	series, err = insights.GroupRetentionSeries(t.Context(), []insights.RetentionRow{}, nil)
	if err != nil {
		t.Fatalf("GroupRetentionSeries(empty): %v", err)
	}
	if len(series) != 0 {
		t.Errorf("expected 0 series for empty input, got %d", len(series))
	}
}

func TestRetentionWithFilterGroups(t *testing.T) {
	req := &insightsv1.QueryRequest{
		Spec: &insightsv1.InsightQuerySpec{
			InsightType: insightsv1.InsightType_INSIGHT_TYPE_RETENTION.Enum(),
			Events: []*insightsv1.EventQuery{
				{Event: &commonv1.EventFilter{Kind: proto.String("sign_up")}},
				{Event: &commonv1.EventFilter{Kind: proto.String("login")}},
			},
			FilterGroups: []*insightsv1.FilterGroup{
				{
					Filters: []*commonv1.PropertyFilter{
						{Property: proto.String("$country"), Operator: commonv1.FilterOperator_FILTER_OPERATOR_EQUALS.Enum(), Value: proto.String("US")},
					},
				},
			},
		},
		TimeRange:   timeRange("2024-01-01T00:00:00Z", "2024-01-31T23:59:59Z"),
		Granularity: insightsv1.Granularity_GRANULARITY_DAY.Enum(),
	}

	q, err := insights.BuildRetentionQuery(req, "proj_123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	sql := q.SQL()

	// Filter group should appear in both cohorts and retained CTEs.
	if strings.Count(sql, "$country") < 4 {
		t.Errorf("expected filter group refs in both cohorts and retained CTEs (>=4), got %d", strings.Count(sql, "$country"))
	}

	// The retained CTE conditions must use the "e." alias.
	if !strings.Contains(sql, "e.auto_properties['$country']") {
		t.Errorf("expected aliased filter group in retained CTE (e.auto_properties), got:\n%s", sql)
	}
}

func TestFunnelWithPerStepFilters(t *testing.T) {
	req := &insightsv1.QueryRequest{
		Spec: &insightsv1.InsightQuerySpec{
			InsightType: insightsv1.InsightType_INSIGHT_TYPE_FUNNEL.Enum(),
			Events: []*insightsv1.EventQuery{
				{
					Event: &commonv1.EventFilter{
						Kind: proto.String("page_view"),
						Filters: []*commonv1.PropertyFilter{
							{Property: proto.String("url"), Operator: commonv1.FilterOperator_FILTER_OPERATOR_CONTAINS.Enum(), Value: proto.String("/pricing")},
						},
					},
					Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL.Enum(),
				},
				{
					Event: &commonv1.EventFilter{
						Kind: proto.String("purchase"),
						Filters: []*commonv1.PropertyFilter{
							{Property: proto.String("$amount"), Operator: commonv1.FilterOperator_FILTER_OPERATOR_GTE.Enum(), Value: proto.String("100")},
						},
					},
					Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL.Enum(),
				},
			},
		},
		TimeRange: timeRange("2024-01-01T00:00:00Z", "2024-01-07T23:59:59Z"),
	}

	q, err := insights.BuildFunnelCountsQuery(req, "proj_123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	sql, args := q.SQL(), q.Args()

	// Per-step filters should be combined with kind via AND in windowFunnel conditions.
	if !strings.Contains(sql, "windowFunnel") {
		t.Error("expected windowFunnel in SQL")
	}
	// Step 0: kind = ? AND url LIKE ?
	if !strings.Contains(sql, "'url'") {
		t.Errorf("expected per-step filter for url, got:\n%s", sql)
	}
	// Step 1: kind = ? AND $amount >= ?
	if !strings.Contains(sql, "'$amount'") {
		t.Errorf("expected per-step filter for $amount, got:\n%s", sql)
	}
	// Args: projectID, from, to + windowFunnel step args (kind1, url_like, kind2, amount) = 7
	if len(args) < 7 {
		t.Errorf("expected at least 7 args (project + time + step conditions), got %d: %v", len(args), args)
	}
}

// TestGroupSeries_BreakdownMismatchError verifies that GroupSeries returns an error
// when a row has fewer breakdowns than expected properties.
func TestGroupSeries_BreakdownMismatchError(t *testing.T) {
	rows := []insights.TrendRow{
		{EventKind: "page_view", Breakdowns: []string{}, Value: 10},
	}
	if _, err := insights.GroupSeries(t.Context(), rows, []string{"$country"}); err == nil {
		t.Error("expected error for mismatched breakdowns/properties")
	}
}

// TestFunnelWithBreakdown verifies the SQL structure of a funnel query with a breakdown.
func TestFunnelWithBreakdown(t *testing.T) {
	req := &insightsv1.QueryRequest{
		Spec: &insightsv1.InsightQuerySpec{
			InsightType: insightsv1.InsightType_INSIGHT_TYPE_FUNNEL.Enum(),
			Events: []*insightsv1.EventQuery{
				{Event: &commonv1.EventFilter{Kind: proto.String("sign_up")}},
				{Event: &commonv1.EventFilter{Kind: proto.String("purchase")}},
			},
			Breakdowns:     []*insightsv1.Breakdown{{Property: proto.String("$browser")}},
			BreakdownLimit: proto.Int32(5),
		},
		TimeRange: timeRange("2024-01-01T00:00:00Z", "2024-01-31T23:59:59Z"),
	}

	q, err := insights.BuildFunnelCountsQuery(req, "proj_123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if q.NumBreakdowns() != 1 {
		t.Errorf("expected 1 breakdown, got %d", q.NumBreakdowns())
	}
	if q.Properties()[0] != "$browser" {
		t.Errorf("unexpected property: %q", q.Properties()[0])
	}

	sql := q.SQL()
	// top_vals CTE for top-N bucketing, filtered to step-matching events.
	if !strings.Contains(sql, "top_vals") {
		t.Error("expected top_vals CTE")
	}
	// argMin attribution assigns breakdown value from the user's earliest step-matching event.
	if !strings.Contains(sql, "argMin") {
		t.Error("expected argMin for first-touch attribution")
	}
	// $others fallback for values outside top-N.
	if !strings.Contains(sql, "'$others'") {
		t.Error("expected '$others' fallback in SQL")
	}
	// Breakdown column in outer GROUP BY.
	if !strings.Contains(sql, "GROUP BY breakdown_0") {
		t.Error("expected GROUP BY breakdown_0 in outer query")
	}
	// Funnel CTE must be filtered to step-matching events (OR of step conditions)
	// so argMin attribution is scoped to funnel-relevant events only.
	// Step kinds appear as parameterized args — check the SQL contains an OR structure
	// with both step kind bindings in the args.
	if !strings.Contains(sql, " OR ") {
		t.Error("expected OR of step conditions in funnel CTE WHERE clause")
	}
}

// TestFunnelTimingWithBreakdown verifies the SQL structure of a funnel timing query with a breakdown.
// This path uses a user_arrays CTE (distinct from the counts path which uses a funnel CTE),
// and top_vals is filtered to step-matching events only.
func TestFunnelTimingWithBreakdown(t *testing.T) {
	req := &insightsv1.QueryRequest{
		Spec: &insightsv1.InsightQuerySpec{
			InsightType:       insightsv1.InsightType_INSIGHT_TYPE_FUNNEL.Enum(),
			IncludeStepTiming: proto.Bool(true),
			Events: []*insightsv1.EventQuery{
				{Event: &commonv1.EventFilter{Kind: proto.String("sign_up")}},
				{Event: &commonv1.EventFilter{Kind: proto.String("purchase")}},
			},
			Breakdowns:     []*insightsv1.Breakdown{{Property: proto.String("$browser")}},
			BreakdownLimit: proto.Int32(5),
		},
		TimeRange: timeRange("2024-01-01T00:00:00Z", "2024-01-31T23:59:59Z"),
	}

	q, err := insights.BuildFunnelTimingQuery(req, "proj_123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if q.NumBreakdowns() != 1 {
		t.Errorf("expected 1 breakdown, got %d", q.NumBreakdowns())
	}
	if q.Properties()[0] != "$browser" {
		t.Errorf("unexpected property: %q", q.Properties()[0])
	}

	sql := q.SQL()

	// top_vals CTE for top-N bucketing (filtered to step-matching events).
	if !strings.Contains(sql, "top_vals") {
		t.Error("expected top_vals CTE")
	}
	// user_arrays CTE aggregates per-user arrays + raw argMin values.
	if !strings.Contains(sql, "user_arrays") {
		t.Error("expected user_arrays CTE")
	}
	// argMin must appear exactly once (in user_arrays, not duplicated in outer SELECT).
	if c := strings.Count(sql, "argMin"); c != 1 {
		t.Errorf("expected argMin exactly once, got %d", c)
	}
	// raw_bd_0 carries the argMin result into the outer SELECT.
	if !strings.Contains(sql, "raw_bd_0") {
		t.Error("expected raw_bd_0 column from user_arrays CTE")
	}
	// Outer SELECT applies '$others' bucketing as a scalar.
	if !strings.Contains(sql, "'$others'") {
		t.Error("expected '$others' fallback in SQL")
	}
	if !strings.Contains(sql, "breakdown_0") {
		t.Error("expected breakdown_0 in outer SELECT")
	}
	// top_vals must be filtered to step-matching events (not all events).
	// The OR of step conditions appears in the tagged CTE filter; top_vals inherits it.
	if !strings.Contains(sql, "top_vals") || !strings.Contains(sql, "tagged") {
		t.Error("expected both top_vals and tagged CTEs")
	}
}

// TestRetentionWithBreakdown verifies the SQL structure of a retention query with a breakdown.
func TestRetentionWithBreakdown(t *testing.T) {
	req := &insightsv1.QueryRequest{
		Spec: &insightsv1.InsightQuerySpec{
			InsightType: insightsv1.InsightType_INSIGHT_TYPE_RETENTION.Enum(),
			Events: []*insightsv1.EventQuery{
				{Event: &commonv1.EventFilter{Kind: proto.String("sign_up")}},
				{Event: &commonv1.EventFilter{Kind: proto.String("login")}},
			},
			Breakdowns:     []*insightsv1.Breakdown{{Property: proto.String("$country")}},
			BreakdownLimit: proto.Int32(10),
		},
		TimeRange:   timeRange("2024-01-01T00:00:00Z", "2024-01-31T23:59:59Z"),
		Granularity: insightsv1.Granularity_GRANULARITY_DAY.Enum(),
	}

	q, err := insights.BuildRetentionQuery(req, "proj_123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if q.NumBreakdowns() != 1 {
		t.Errorf("expected 1 breakdown, got %d", q.NumBreakdowns())
	}
	if q.Properties()[0] != "$country" {
		t.Errorf("unexpected property: %q", q.Properties()[0])
	}

	sql := q.SQL()
	if !strings.Contains(sql, "top_vals") {
		t.Error("expected top_vals CTE")
	}
	if !strings.Contains(sql, "cohorts_raw") {
		t.Error("expected cohorts_raw CTE for two-phase aggregation")
	}
	if !strings.Contains(sql, "argMin") {
		t.Error("expected argMin for first-touch attribution in cohorts CTE")
	}
	if !strings.Contains(sql, "'$others'") {
		t.Error("expected '$others' fallback")
	}
	// cohort_sizes must GROUP BY the breakdown column.
	if !strings.Contains(sql, "breakdown_0") {
		t.Error("expected breakdown_0 column in SQL")
	}
	// JOIN condition must include breakdown equality.
	if !strings.Contains(sql, "r.breakdown_0 = cs.breakdown_0") {
		t.Error("expected breakdown equality in final JOIN condition")
	}
}

// TestGroupFunnelSeries_NoBreakdown verifies that a single series with no breakdown is returned.
func TestGroupFunnelSeries_NoBreakdown(t *testing.T) {
	rows := []insights.FunnelRow{
		{StepIndex: 0, EventKind: "sign_up", Value: 100},
		{StepIndex: 1, EventKind: "purchase", Value: 60},
	}
	series, err := insights.GroupFunnelSeries(ctx, rows, nil)
	if err != nil {
		t.Fatalf("GroupFunnelSeries: %v", err)
	}
	if len(series) != 1 {
		t.Fatalf("expected 1 series, got %d", len(series))
	}
	if len(series[0].Steps) != 2 {
		t.Fatalf("expected 2 steps, got %d", len(series[0].Steps))
	}
	if series[0].Steps[0].GetTotal() != 100 || series[0].Steps[1].GetTotal() != 60 {
		t.Errorf("unexpected step totals: %v, %v", series[0].Steps[0].GetTotal(), series[0].Steps[1].GetTotal())
	}
	if len(series[0].Breakdown) != 0 {
		t.Errorf("expected empty breakdown for no-breakdown series, got %v", series[0].Breakdown)
	}
}

// TestGroupFunnelSeries_WithBreakdown verifies that rows are split into one series per breakdown value.
func TestGroupFunnelSeries_WithBreakdown(t *testing.T) {
	rows := []insights.FunnelRow{
		{StepIndex: 0, EventKind: "sign_up", Breakdowns: []string{"Chrome"}, Value: 80},
		{StepIndex: 1, EventKind: "purchase", Breakdowns: []string{"Chrome"}, Value: 50},
		{StepIndex: 0, EventKind: "sign_up", Breakdowns: []string{"Safari"}, Value: 20},
		{StepIndex: 1, EventKind: "purchase", Breakdowns: []string{"Safari"}, Value: 10},
	}
	series, err := insights.GroupFunnelSeries(ctx, rows, []string{"$browser"})
	if err != nil {
		t.Fatalf("GroupFunnelSeries: %v", err)
	}
	if len(series) != 2 {
		t.Fatalf("expected 2 series (Chrome + Safari), got %d", len(series))
	}
	if series[0].Breakdown["$browser"] != "Chrome" {
		t.Errorf("expected first series to be Chrome, got %q", series[0].Breakdown["$browser"])
	}
	if series[0].Steps[0].GetTotal() != 80 || series[0].Steps[1].GetTotal() != 50 {
		t.Errorf("unexpected Chrome steps: %v, %v", series[0].Steps[0].GetTotal(), series[0].Steps[1].GetTotal())
	}
	if series[1].Breakdown["$browser"] != "Safari" {
		t.Errorf("expected second series to be Safari, got %q", series[1].Breakdown["$browser"])
	}
}

// TestGroupRetentionSeries_WithBreakdown verifies that rows are split into one series per breakdown value.
func TestGroupRetentionSeries_WithBreakdown(t *testing.T) {
	rows := []insights.RetentionRow{
		{
			CohortTime: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
			Time:       time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
			Value:      100,
			CohortSize: 50,
			Breakdowns: []string{"US"},
		},
		{
			CohortTime: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
			Time:       time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC),
			Value:      60,
			CohortSize: 50,
			Breakdowns: []string{"US"},
		},
		{
			CohortTime: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
			Time:       time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
			Value:      100,
			CohortSize: 20,
			Breakdowns: []string{"GB"},
		},
	}

	series, err := insights.GroupRetentionSeries(t.Context(), rows, []string{"$country"})
	if err != nil {
		t.Fatalf("GroupRetentionSeries: %v", err)
	}
	if len(series) != 2 {
		t.Fatalf("expected 2 series (US + GB), got %d", len(series))
	}
	if series[0].Breakdown["$country"] != "US" {
		t.Errorf("expected first series to be US, got %q", series[0].Breakdown["$country"])
	}
	if len(series[0].Cohorts) != 1 {
		t.Fatalf("expected 1 cohort in US series, got %d", len(series[0].Cohorts))
	}
	if len(series[0].Cohorts[0].Points) != 2 {
		t.Errorf("expected 2 points in US cohort, got %d", len(series[0].Cohorts[0].Points))
	}
	if series[1].Breakdown["$country"] != "GB" {
		t.Errorf("expected second series to be GB, got %q", series[1].Breakdown["$country"])
	}
}

// TestGroupFunnelSeries_Empty verifies that nil and empty-slice inputs produce an empty result.
func TestGroupFunnelSeries_Empty(t *testing.T) {
	series, err := insights.GroupFunnelSeries(ctx, nil, nil)
	if err != nil {
		t.Fatalf("GroupFunnelSeries(nil): %v", err)
	}
	if len(series) != 0 {
		t.Errorf("expected 0 series for nil input, got %d", len(series))
	}
	series, err = insights.GroupFunnelSeries(ctx, []insights.FunnelRow{}, nil)
	if err != nil {
		t.Fatalf("GroupFunnelSeries(empty): %v", err)
	}
	if len(series) != 0 {
		t.Errorf("expected 0 series for empty input, got %d", len(series))
	}
}

// TestGroupFunnelSeries_MultiBreakdown verifies correct grouping with two breakdown dimensions.
func TestGroupFunnelSeries_MultiBreakdown(t *testing.T) {
	rows := []insights.FunnelRow{
		{StepIndex: 0, EventKind: "sign_up", Breakdowns: []string{"US", "Chrome"}, Value: 50},
		{StepIndex: 1, EventKind: "purchase", Breakdowns: []string{"US", "Chrome"}, Value: 30},
		{StepIndex: 0, EventKind: "sign_up", Breakdowns: []string{"US", "Safari"}, Value: 20},
		{StepIndex: 1, EventKind: "purchase", Breakdowns: []string{"US", "Safari"}, Value: 10},
	}
	series, err := insights.GroupFunnelSeries(ctx, rows, []string{"$country", "$browser"})
	if err != nil {
		t.Fatalf("GroupFunnelSeries: %v", err)
	}
	if len(series) != 2 {
		t.Fatalf("expected 2 series, got %d", len(series))
	}
	if series[0].Breakdown["$country"] != "US" || series[0].Breakdown["$browser"] != "Chrome" {
		t.Errorf("series 0 breakdown: got %v", series[0].Breakdown)
	}
	if series[1].Breakdown["$country"] != "US" || series[1].Breakdown["$browser"] != "Safari" {
		t.Errorf("series 1 breakdown: got %v", series[1].Breakdown)
	}
}

// TestGroupFunnelSeries_BreakdownMismatchError verifies error on mismatched breakdowns/properties.
func TestGroupFunnelSeries_BreakdownMismatchError(t *testing.T) {
	rows := []insights.FunnelRow{
		{StepIndex: 0, EventKind: "sign_up", Breakdowns: []string{}, Value: 10},
	}
	if _, err := insights.GroupFunnelSeries(ctx, rows, []string{"$browser"}); err == nil {
		t.Error("expected error for mismatched breakdowns/properties")
	}
}

// TestGroupRetentionSeries_MultiBreakdown verifies correct grouping with two breakdown dimensions.
func TestGroupRetentionSeries_MultiBreakdown(t *testing.T) {
	ct := mustTime("2024-01-01T00:00:00Z")
	rows := []insights.RetentionRow{
		{CohortTime: ct, Time: ct, Value: 100, CohortSize: 10, Breakdowns: []string{"US", "Chrome"}},
		{CohortTime: ct, Time: ct, Value: 100, CohortSize: 5, Breakdowns: []string{"GB", "Safari"}},
	}
	series, err := insights.GroupRetentionSeries(t.Context(), rows, []string{"$country", "$browser"})
	if err != nil {
		t.Fatalf("GroupRetentionSeries: %v", err)
	}
	if len(series) != 2 {
		t.Fatalf("expected 2 series, got %d", len(series))
	}
	if series[0].Breakdown["$country"] != "US" || series[0].Breakdown["$browser"] != "Chrome" {
		t.Errorf("series 0 breakdown: got %v", series[0].Breakdown)
	}
	if series[1].Breakdown["$country"] != "GB" || series[1].Breakdown["$browser"] != "Safari" {
		t.Errorf("series 1 breakdown: got %v", series[1].Breakdown)
	}
}

// TestGroupRetentionSeries_BreakdownMismatchError verifies error on mismatched breakdowns/properties.
func TestGroupRetentionSeries_BreakdownMismatchError(t *testing.T) {
	ct := mustTime("2024-01-01T00:00:00Z")
	rows := []insights.RetentionRow{
		{CohortTime: ct, Time: ct, Value: 100, CohortSize: 10, Breakdowns: []string{}},
	}
	if _, err := insights.GroupRetentionSeries(t.Context(), rows, []string{"$country"}); err == nil {
		t.Error("expected error for mismatched breakdowns/properties")
	}
}

// TestBuildFunnelCountsQuery_MultiBreakdown verifies SQL structure with two breakdowns.
func TestBuildFunnelCountsQuery_MultiBreakdown(t *testing.T) {
	req := &insightsv1.QueryRequest{
		Spec: &insightsv1.InsightQuerySpec{
			InsightType: insightsv1.InsightType_INSIGHT_TYPE_FUNNEL.Enum(),
			Events: []*insightsv1.EventQuery{
				{Event: &commonv1.EventFilter{Kind: proto.String("sign_up")}},
				{Event: &commonv1.EventFilter{Kind: proto.String("purchase")}},
			},
			Breakdowns: []*insightsv1.Breakdown{
				{Property: proto.String("$country")},
				{Property: proto.String("$browser")},
			},
			BreakdownLimit: proto.Int32(5),
		},
		TimeRange: timeRange("2024-01-01T00:00:00Z", "2024-01-31T23:59:59Z"),
	}

	q, err := insights.BuildFunnelCountsQuery(req, "proj_123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if q.NumBreakdowns() != 2 {
		t.Errorf("expected 2 breakdowns, got %d", q.NumBreakdowns())
	}
	sql := q.SQL()
	if !strings.Contains(sql, "breakdown_0") || !strings.Contains(sql, "breakdown_1") {
		t.Error("expected both breakdown_0 and breakdown_1 in SQL")
	}
}

// TestBuildRetentionQuery_MultiBreakdown verifies SQL structure with two breakdowns.
func TestBuildRetentionQuery_MultiBreakdown(t *testing.T) {
	req := &insightsv1.QueryRequest{
		Spec: &insightsv1.InsightQuerySpec{
			InsightType: insightsv1.InsightType_INSIGHT_TYPE_RETENTION.Enum(),
			Events: []*insightsv1.EventQuery{
				{Event: &commonv1.EventFilter{Kind: proto.String("sign_up")}},
				{Event: &commonv1.EventFilter{Kind: proto.String("login")}},
			},
			Breakdowns: []*insightsv1.Breakdown{
				{Property: proto.String("$country")},
				{Property: proto.String("$browser")},
			},
			BreakdownLimit: proto.Int32(5),
		},
		TimeRange:   timeRange("2024-01-01T00:00:00Z", "2024-01-31T23:59:59Z"),
		Granularity: insightsv1.Granularity_GRANULARITY_DAY.Enum(),
	}

	q, err := insights.BuildRetentionQuery(req, "proj_123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if q.NumBreakdowns() != 2 {
		t.Errorf("expected 2 breakdowns, got %d", q.NumBreakdowns())
	}
	sql := q.SQL()
	if !strings.Contains(sql, "breakdown_0") || !strings.Contains(sql, "breakdown_1") {
		t.Error("expected both breakdown_0 and breakdown_1 in SQL")
	}
	if !strings.Contains(sql, "r.breakdown_1 = cs.breakdown_1") {
		t.Error("expected multi-column JOIN condition for breakdown_1")
	}
}

// TestGroupSeries_MultiBreakdown verifies correct grouping with two breakdown dimensions.
func TestGroupSeries_MultiBreakdown(t *testing.T) {
	rows := []insights.TrendRow{
		{Time: mustTime("2024-01-01T00:00:00Z"), EventKind: "page_view", Breakdowns: []string{"US", "Chrome"}, Value: 10},
		{Time: mustTime("2024-01-02T00:00:00Z"), EventKind: "page_view", Breakdowns: []string{"US", "Chrome"}, Value: 20},
		{Time: mustTime("2024-01-01T00:00:00Z"), EventKind: "page_view", Breakdowns: []string{"GB", "Safari"}, Value: 5},
	}

	series, err := insights.GroupSeries(t.Context(), rows, []string{"$country", "$browser"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(series) != 2 {
		t.Fatalf("expected 2 series, got %d", len(series))
	}
	if series[0].Breakdown["$country"] != "US" || series[0].Breakdown["$browser"] != "Chrome" {
		t.Errorf("series 0 breakdown: got %v", series[0].Breakdown)
	}
	if len(series[0].Points) != 2 {
		t.Errorf("expected 2 points for US/Chrome, got %d", len(series[0].Points))
	}
	if series[1].Breakdown["$country"] != "GB" || series[1].Breakdown["$browser"] != "Safari" {
		t.Errorf("series 1 breakdown: got %v", series[1].Breakdown)
	}
	if len(series[1].Points) != 1 {
		t.Errorf("expected 1 point for GB/Safari, got %d", len(series[1].Points))
	}
}

// TestGroupRetentionSeries_MultiCohortWithBreakdown verifies correct grouping when
// multiple cohort times exist per breakdown series.
func TestGroupRetentionSeries_MultiCohortWithBreakdown(t *testing.T) {
	ct1 := mustTime("2024-01-01T00:00:00Z")
	ct2 := mustTime("2024-01-08T00:00:00Z")

	rows := []insights.RetentionRow{
		{CohortTime: ct1, Time: ct1, Value: 100, CohortSize: 50, Breakdowns: []string{"US"}},
		{CohortTime: ct1, Time: ct2, Value: 60, CohortSize: 50, Breakdowns: []string{"US"}},
		{CohortTime: ct2, Time: ct2, Value: 100, CohortSize: 30, Breakdowns: []string{"US"}},
		{CohortTime: ct1, Time: ct1, Value: 100, CohortSize: 20, Breakdowns: []string{"GB"}},
		{CohortTime: ct2, Time: ct2, Value: 100, CohortSize: 10, Breakdowns: []string{"GB"}},
	}

	series, err := insights.GroupRetentionSeries(t.Context(), rows, []string{"$country"})
	if err != nil {
		t.Fatalf("GroupRetentionSeries: %v", err)
	}
	if len(series) != 2 {
		t.Fatalf("expected 2 series, got %d", len(series))
	}

	// US series: 2 cohorts
	us := series[0]
	if us.Breakdown["$country"] != "US" {
		t.Errorf("expected US series first, got %v", us.Breakdown)
	}
	if len(us.Cohorts) != 2 {
		t.Fatalf("expected 2 cohorts in US series, got %d", len(us.Cohorts))
	}
	if us.Cohorts[0].GetCohortSize() != 50 {
		t.Errorf("US cohort 0: expected size 50, got %v", us.Cohorts[0].GetCohortSize())
	}
	if len(us.Cohorts[0].Points) != 2 {
		t.Errorf("US cohort 0: expected 2 points, got %d", len(us.Cohorts[0].Points))
	}
	if us.Cohorts[1].GetCohortSize() != 30 {
		t.Errorf("US cohort 1: expected size 30, got %v", us.Cohorts[1].GetCohortSize())
	}

	// GB series: 2 cohorts
	gb := series[1]
	if gb.Breakdown["$country"] != "GB" {
		t.Errorf("expected GB series second, got %v", gb.Breakdown)
	}
	if len(gb.Cohorts) != 2 {
		t.Fatalf("expected 2 cohorts in GB series, got %d", len(gb.Cohorts))
	}
}

// TestGroupFunnelSeries_SortedInputPreservesOrder verifies that GroupFunnelSeries correctly
// groups pre-sorted rows (sorted by breakdown, then step_index — as QueryFunnel produces).
func TestGroupFunnelSeries_SortedInputPreservesOrder(t *testing.T) {
	// Rows arrive sorted: GB steps first (sorted by step_index), then US steps.
	rows := []insights.FunnelRow{
		{StepIndex: 0, EventKind: "sign_up", Breakdowns: []string{"GB"}, Value: 20},
		{StepIndex: 1, EventKind: "purchase", Breakdowns: []string{"GB"}, Value: 10},
		{StepIndex: 0, EventKind: "sign_up", Breakdowns: []string{"US"}, Value: 50},
		{StepIndex: 1, EventKind: "purchase", Breakdowns: []string{"US"}, Value: 30},
	}

	series, err := insights.GroupFunnelSeries(ctx, rows, []string{"$country"})
	if err != nil {
		t.Fatalf("GroupFunnelSeries: %v", err)
	}
	if len(series) != 2 {
		t.Fatalf("expected 2 series, got %d", len(series))
	}
	// Verify step order within each series is preserved from sorted input.
	for i, s := range series {
		if len(s.Steps) != 2 {
			t.Fatalf("series %d: expected 2 steps, got %d", i, len(s.Steps))
		}
		if s.Steps[0].GetEventKind() != "sign_up" {
			t.Errorf("series %d step 0: expected sign_up, got %s", i, s.Steps[0].GetEventKind())
		}
		if s.Steps[1].GetEventKind() != "purchase" {
			t.Errorf("series %d step 1: expected purchase, got %s", i, s.Steps[1].GetEventKind())
		}
	}
	if series[0].Breakdown["$country"] != "GB" {
		t.Errorf("expected first series GB, got %v", series[0].Breakdown)
	}
	if series[1].Breakdown["$country"] != "US" {
		t.Errorf("expected second series US, got %v", series[1].Breakdown)
	}
}

// TestGroupFunnelSeries_TimingFieldsProtoTranslation verifies that a populated *StepTiming on
// FunnelRow is correctly translated into the proto FunnelStep.timing sub-message:
//   - median, p95, avg durations are copied.
//   - bucket labels are applied from the package-level bucket table.
//   - upper_bound is set on all but the open-ended last bucket.
//   - counts are copied from the histogram slice.
func TestGroupFunnelSeries_TimingFieldsProtoTranslation(t *testing.T) {
	dist := []int64{3, 0, 1, 0, 0, 2, 0, 0}
	rows := []insights.FunnelRow{
		{StepIndex: 0, EventKind: "sign_up", Value: 10},
		{
			StepIndex: 1,
			EventKind: "purchase",
			Value:     6,
			Timing: &insights.StepTiming{
				Avg:          1000 * time.Second,
				Median:       700 * time.Second,
				P95:          5800 * time.Second,
				Distribution: dist,
			},
		},
	}
	series, err := insights.GroupFunnelSeries(ctx, rows, nil)
	if err != nil {
		t.Fatalf("GroupFunnelSeries: %v", err)
	}
	if len(series) != 1 {
		t.Fatalf("expected 1 series, got %d", len(series))
	}
	steps := series[0].Steps
	if len(steps) != 2 {
		t.Fatalf("expected 2 steps, got %d", len(steps))
	}

	// Step 0: no timing input → timing sub-message must be absent on the wire.
	if steps[0].GetTiming() != nil {
		t.Errorf("step 0 timing: expected nil, got %+v", steps[0].GetTiming())
	}

	// Step 1: durations copied verbatim through the sub-message.
	timing := steps[1].GetTiming()
	if timing == nil {
		t.Fatal("step 1 timing: expected non-nil")
	}
	if got, want := timing.GetAvg().AsDuration(), 1000*time.Second; got != want {
		t.Errorf("step 1 avg: got %v, want %v", got, want)
	}
	if got, want := timing.GetMedian().AsDuration(), 700*time.Second; got != want {
		t.Errorf("step 1 median: got %v, want %v", got, want)
	}
	if got, want := timing.GetP95().AsDuration(), 5800*time.Second; got != want {
		t.Errorf("step 1 p95: got %v, want %v", got, want)
	}

	// Step 1: distribution has 8 buckets with labels in canonical order and correct counts.
	buckets := timing.GetDistribution()
	if len(buckets) != 8 {
		t.Fatalf("step 1 distribution: expected 8 buckets, got %d", len(buckets))
	}
	wantLabels := []string{"0-30s", "30s-2m", "2-5m", "5-15m", "15-60m", "1-6h", "6-24h", "24h+"}
	wantUpper := []time.Duration{
		30 * time.Second,
		2 * time.Minute,
		5 * time.Minute,
		15 * time.Minute,
		1 * time.Hour,
		6 * time.Hour,
		24 * time.Hour,
	} // last bucket is open-ended
	for i, b := range buckets {
		if b.GetLabel() != wantLabels[i] {
			t.Errorf("bucket %d label: got %q, want %q", i, b.GetLabel(), wantLabels[i])
		}
		if b.GetCount() != dist[i] {
			t.Errorf("bucket %d count: got %d, want %d", i, b.GetCount(), dist[i])
		}
		if i < len(wantUpper) {
			if b.UpperBound == nil {
				t.Errorf("bucket %d: upper_bound should be set", i)
				continue
			}
			if got := b.GetUpperBound().AsDuration(); got != wantUpper[i] {
				t.Errorf("bucket %d upper bound: got %v, want %v", i, got, wantUpper[i])
			}
		} else if b.UpperBound != nil {
			t.Errorf("bucket %d (last): upper_bound should be absent, got %v", i, b.GetUpperBound().AsDuration())
		}
	}
}

// TestGroupFunnelSeries_ZeroConverterStepEmitsZeroFilledBuckets verifies the nil-vs-zero-filled
// contract survives the proto boundary: a Timing with an 8-element zero-filled distribution
// becomes 8 proto buckets with count=0 (never coalesced to empty).
func TestGroupFunnelSeries_ZeroConverterStepEmitsZeroFilledBuckets(t *testing.T) {
	rows := []insights.FunnelRow{
		{StepIndex: 0, EventKind: "sign_up", Value: 10},
		{
			StepIndex: 1,
			EventKind: "purchase",
			Value:     0,
			Timing:    &insights.StepTiming{Distribution: make([]int64, 8)},
		},
	}
	series, err := insights.GroupFunnelSeries(ctx, rows, nil)
	if err != nil {
		t.Fatalf("GroupFunnelSeries: %v", err)
	}
	step1 := series[0].Steps[1]
	timing := step1.GetTiming()
	if timing == nil {
		t.Fatal("zero-converter step: timing should be present, got nil")
	}
	if len(timing.GetDistribution()) != 8 {
		t.Fatalf("zero-converter step: expected 8 buckets, got %d", len(timing.GetDistribution()))
	}
	for i, b := range timing.GetDistribution() {
		if b.GetCount() != 0 {
			t.Errorf("bucket %d count: got %d, want 0", i, b.GetCount())
		}
	}
}

// TestGroupFunnelSeries_NilTimingOmitsSubMessage verifies step 0's semantics: a nil Timing on
// FunnelRow produces no Timing sub-message on the proto FunnelStep, distinct from the zero-converter
// case above where Timing is present with a zero-filled distribution.
func TestGroupFunnelSeries_NilTimingOmitsSubMessage(t *testing.T) {
	rows := []insights.FunnelRow{
		{StepIndex: 0, EventKind: "sign_up", Value: 10, Timing: nil},
	}
	series, err := insights.GroupFunnelSeries(ctx, rows, nil)
	if err != nil {
		t.Fatalf("GroupFunnelSeries: %v", err)
	}
	if series[0].Steps[0].GetTiming() != nil {
		t.Errorf("nil-Timing row: expected absent proto Timing sub-message, got %+v",
			series[0].Steps[0].GetTiming())
	}
}

// TestPropertyAggregation_Trends verifies SUM/AVG/MIN/MAX generate correct SQL in trends queries.
func TestPropertyAggregation_Trends(t *testing.T) {
	tests := []struct {
		name        string
		agg         insightsv1.AggregationType
		property    string
		wantContain string
	}{
		{
			name:        "SUM",
			agg:         insightsv1.AggregationType_AGGREGATION_TYPE_SUM,
			property:    "revenue",
			wantContain: "sum(toFloat64OrNull(",
		},
		{
			name:        "AVG",
			agg:         insightsv1.AggregationType_AGGREGATION_TYPE_AVG,
			property:    "revenue",
			wantContain: "ifNull(avg(toFloat64OrNull(",
		},
		{
			name:        "MIN",
			agg:         insightsv1.AggregationType_AGGREGATION_TYPE_MIN,
			property:    "load_time",
			wantContain: "ifNull(min(toFloat64OrNull(",
		},
		{
			name:        "MAX",
			agg:         insightsv1.AggregationType_AGGREGATION_TYPE_MAX,
			property:    "$session_duration",
			wantContain: "ifNull(max(toFloat64OrNull(",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := &insightsv1.QueryRequest{
				Spec: &insightsv1.InsightQuerySpec{
					InsightType: insightsv1.InsightType_INSIGHT_TYPE_TRENDS.Enum(),
					Events: []*insightsv1.EventQuery{
						{
							Event:               &commonv1.EventFilter{Kind: proto.String("purchase")},
							Aggregation:         tc.agg.Enum(),
							AggregationProperty: proto.String(tc.property),
						},
					},
				},
				TimeRange:   timeRange("2024-01-01T00:00:00Z", "2024-01-07T23:59:59Z"),
				Granularity: insightsv1.Granularity_GRANULARITY_DAY.Enum(),
			}

			q, err := insights.BuildTrendsQuery(req, "proj_123")
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			sql := q.SQL()

			if !strings.Contains(sql, tc.wantContain) {
				t.Errorf("expected %q in SQL, got: %s", tc.wantContain, sql)
			}
			if !strings.Contains(sql, "'"+tc.property+"'") {
				t.Errorf("expected property name %q in SQL, got: %s", tc.property, sql)
			}
		})
	}
}

// TestPropertyAggregation_BackwardCompat verifies count-based aggs produce correct SQL
// and ignore a stray aggregation_property when one is set.
func TestPropertyAggregation_BackwardCompat(t *testing.T) {
	tests := []struct {
		name        string
		agg         insightsv1.AggregationType
		property    string
		wantContain string
	}{
		{"TOTAL", insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL, "", "count(*)"},
		{"TOTAL_with_property", insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL, "revenue", "count(*)"},
		{"UNIQUE_USERS", insightsv1.AggregationType_AGGREGATION_TYPE_UNIQUE_USERS, "", "uniq(distinct_id)"},
		{"PER_USER_AVG", insightsv1.AggregationType_AGGREGATION_TYPE_PER_USER_AVG, "", "count(*)) / toFloat64(uniq(distinct_id))"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := &insightsv1.QueryRequest{
				Spec: &insightsv1.InsightQuerySpec{
					InsightType: insightsv1.InsightType_INSIGHT_TYPE_TRENDS.Enum(),
					Events: []*insightsv1.EventQuery{
						{Event: &commonv1.EventFilter{Kind: proto.String("page_view")}, Aggregation: tc.agg.Enum(), AggregationProperty: proto.String(tc.property)},
					},
				},
				TimeRange:   timeRange("2024-01-01T00:00:00Z", "2024-01-07T23:59:59Z"),
				Granularity: insightsv1.Granularity_GRANULARITY_DAY.Enum(),
			}

			q, err := insights.BuildTrendsQuery(req, "proj_123")
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			sql := q.SQL()
			if !strings.Contains(sql, tc.wantContain) {
				t.Errorf("expected %q in SQL, got: %s", tc.wantContain, sql)
			}
			if tc.property != "" && strings.Contains(sql, "toFloat64OrNull(") {
				t.Errorf("count-based agg should not contain toFloat64OrNull when property is set, got: %s", sql)
			}
		})
	}
}

// TestPropertyAggregation_Segmentation verifies numeric aggs generate correct SQL in segmentation queries.
func TestPropertyAggregation_Segmentation(t *testing.T) {
	tests := []struct {
		name        string
		agg         insightsv1.AggregationType
		property    string
		wantContain string
	}{
		{"SUM", insightsv1.AggregationType_AGGREGATION_TYPE_SUM, "revenue", "sum(toFloat64OrNull("},
		{"AVG", insightsv1.AggregationType_AGGREGATION_TYPE_AVG, "revenue", "ifNull(avg(toFloat64OrNull("},
		{"MIN", insightsv1.AggregationType_AGGREGATION_TYPE_MIN, "load_time", "ifNull(min(toFloat64OrNull("},
		{"MAX", insightsv1.AggregationType_AGGREGATION_TYPE_MAX, "$session_duration", "ifNull(max(toFloat64OrNull("},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := &insightsv1.QueryRequest{
				Spec: &insightsv1.InsightQuerySpec{
					InsightType: insightsv1.InsightType_INSIGHT_TYPE_SEGMENTATION.Enum(),
					Events: []*insightsv1.EventQuery{
						{
							Event:               &commonv1.EventFilter{Kind: proto.String("purchase")},
							Aggregation:         tc.agg.Enum(),
							AggregationProperty: proto.String(tc.property),
						},
					},
				},
				TimeRange: timeRange("2024-01-01T00:00:00Z", "2024-01-07T23:59:59Z"),
			}

			q, err := insights.BuildSegmentationQuery(req, "proj_123")
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			sql := q.SQL()

			if !strings.Contains(sql, tc.wantContain) {
				t.Errorf("expected %q in SQL, got: %s", tc.wantContain, sql)
			}
			if !strings.Contains(sql, "'"+tc.property+"'") {
				t.Errorf("expected property name %q in SQL, got: %s", tc.property, sql)
			}
		})
	}
}

// TestPropertyAggregation_MixedEventAggregations verifies trends with multiple events
// using different aggregation types (one numeric, one count-based).
func TestPropertyAggregation_MixedEventAggregations(t *testing.T) {
	req := &insightsv1.QueryRequest{
		Spec: &insightsv1.InsightQuerySpec{
			InsightType: insightsv1.InsightType_INSIGHT_TYPE_TRENDS.Enum(),
			Events: []*insightsv1.EventQuery{
				{
					Event:               &commonv1.EventFilter{Kind: proto.String("purchase")},
					Aggregation:         insightsv1.AggregationType_AGGREGATION_TYPE_SUM.Enum(),
					AggregationProperty: proto.String("revenue"),
				},
				{
					Event:       &commonv1.EventFilter{Kind: proto.String("page_view")},
					Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL.Enum(),
				},
			},
		},
		TimeRange:   timeRange("2024-01-01T00:00:00Z", "2024-01-07T23:59:59Z"),
		Granularity: insightsv1.Granularity_GRANULARITY_DAY.Enum(),
	}

	q, err := insights.BuildTrendsQuery(req, "proj_123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	sql := q.SQL()

	if !strings.Contains(sql, "sum(toFloat64OrNull(") {
		t.Errorf("expected sum(toFloat64OrNull( for purchase event, got: %s", sql)
	}
	if !strings.Contains(sql, "count(*)") {
		t.Errorf("expected count(*) for page_view event, got: %s", sql)
	}
}

func TestAnalyticsCacheSettings(t *testing.T) {
	tr := timeRange("2024-01-01T00:00:00Z", "2024-01-07T23:59:59Z")
	pageView := &insightsv1.EventQuery{
		Event:       &commonv1.EventFilter{Kind: proto.String("page_view")},
		Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL.Enum(),
	}

	// structure is an insight-specific SQL substring asserting the inner query
	// shape survived the wrapper unchanged (catches regressions that drop or
	// truncate inner SQL when WithQueryCache(...).Build() is applied).
	tests := []struct {
		name      string
		structure string
		run       func(t *testing.T) (string, []any)
	}{
		{
			name:      "BuildTrendsQuery",
			structure: "toStartOfDay(occur_time) AS t",
			run: func(t *testing.T) (string, []any) {
				t.Helper()
				q, err := insights.BuildTrendsQuery(&insightsv1.QueryRequest{
					Spec: &insightsv1.InsightQuerySpec{
						InsightType: insightsv1.InsightType_INSIGHT_TYPE_TRENDS.Enum(),
						Events:      []*insightsv1.EventQuery{pageView},
					},
					TimeRange:   tr,
					Granularity: insightsv1.Granularity_GRANULARITY_DAY.Enum(),
				}, "proj_test")
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return q.SQL(), q.Args()
			},
		},
		{
			name:      "BuildSegmentationQuery",
			structure: "toFloat64(count(*))",
			run: func(t *testing.T) (string, []any) {
				t.Helper()
				q, err := insights.BuildSegmentationQuery(&insightsv1.QueryRequest{
					Spec: &insightsv1.InsightQuerySpec{
						InsightType: insightsv1.InsightType_INSIGHT_TYPE_SEGMENTATION.Enum(),
						Events:      []*insightsv1.EventQuery{pageView},
					},
					TimeRange: tr,
				}, "proj_test")
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return q.SQL(), q.Args()
			},
		},
		{
			name:      "BuildFunnelCountsQuery",
			structure: "windowFunnel(",
			run: func(t *testing.T) (string, []any) {
				t.Helper()
				q, err := insights.BuildFunnelCountsQuery(&insightsv1.QueryRequest{
					Spec: &insightsv1.InsightQuerySpec{
						InsightType: insightsv1.InsightType_INSIGHT_TYPE_FUNNEL.Enum(),
						Events:      []*insightsv1.EventQuery{pageView, pageView},
					},
					TimeRange: tr,
				}, "proj_test")
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return q.SQL(), q.Args()
			},
		},
		{
			name:      "BuildFunnelTimingQuery",
			structure: "groupArray(",
			run: func(t *testing.T) (string, []any) {
				t.Helper()
				q, err := insights.BuildFunnelTimingQuery(&insightsv1.QueryRequest{
					Spec: &insightsv1.InsightQuerySpec{
						InsightType: insightsv1.InsightType_INSIGHT_TYPE_FUNNEL.Enum(),
						Events:      []*insightsv1.EventQuery{pageView, pageView},
					},
					TimeRange: tr,
				}, "proj_test")
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return q.SQL(), q.Args()
			},
		},
		{
			name:      "BuildRetentionQuery",
			structure: "cohort_sizes",
			run: func(t *testing.T) (string, []any) {
				t.Helper()
				q, err := insights.BuildRetentionQuery(&insightsv1.QueryRequest{
					Spec: &insightsv1.InsightQuerySpec{
						InsightType: insightsv1.InsightType_INSIGHT_TYPE_RETENTION.Enum(),
						Events:      []*insightsv1.EventQuery{pageView},
					},
					TimeRange:   tr,
					Granularity: insightsv1.Granularity_GRANULARITY_DAY.Enum(),
				}, "proj_test")
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return q.SQL(), q.Args()
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sql, args := tc.run(t)

			// SETTINGS clause is present and well-formed.
			if !strings.Contains(sql, "use_query_cache = 1") {
				t.Errorf("expected use_query_cache = 1 in SQL, got: %s", sql)
			}
			if !strings.Contains(sql, "query_cache_ttl = 60") {
				t.Errorf("expected query_cache_ttl = 60 in SQL, got: %s", sql)
			}
			settingsIdx := strings.Index(sql, "SETTINGS")
			if settingsIdx < 0 {
				t.Fatalf("expected SETTINGS clause, got: %s", sql)
			}

			// SETTINGS must appear after the LAST occurrence of every relevant clause —
			// strings.Index returns the first occurrence, which is misleading for queries
			// with multiple SELECT/FROM/WHERE/ORDER BY (UNIONs and CTEs).
			for _, clause := range []string{"SELECT", "FROM", "WHERE", "GROUP BY", "ORDER BY"} {
				if idx := strings.LastIndex(sql, clause); idx >= 0 && settingsIdx < idx {
					t.Errorf("SETTINGS appears before last %s in SQL: %s", clause, sql)
				}
			}

			// Inner SQL structure survived the wrapper (catches a regression that drops
			// or truncates inner SQL when WithQueryCache(...).Build() is applied).
			if !strings.Contains(sql, tc.structure) {
				t.Errorf("expected %q in SQL, got: %s", tc.structure, sql)
			}

			// Args survived the wrapper. Every insight binds project_id "proj_test"
			// somewhere in its args slice — exact position varies by insight (funnel uses
			// SelectExpr for windowFunnel kind args which emit before WHERE), so we assert
			// presence rather than position. Catches a regression that drops args after
			// WithQueryCache(...).Build().
			if len(args) == 0 {
				t.Fatalf("expected at least one arg, got: %v", args)
			}
			found := false
			for _, a := range args {
				if a == "proj_test" {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("expected project_id %q somewhere in args, got: %v", "proj_test", args)
			}
		})
	}
}

// TestEffectiveWindowSec covers all paths of the funnel-window helper directly:
// explicit window, absent window with a finite range, and the error paths for
// inverted/empty time ranges. Indirect tests (TestFunnelWithConversionWindow,
// TestFunnelDefaultWindowIsTimeRange) only assert on SQL substrings, which
// would not catch a regression that returns the wrong number of seconds.
func TestEffectiveWindowSec(t *testing.T) {
	from := mustTime("2024-01-01T00:00:00Z")
	to := from.Add(7 * 24 * time.Hour)

	cases := []struct {
		name        string
		req         *insightsv1.QueryRequest
		want        int64
		wantErr     bool
		errContains string
	}{
		{
			name: "explicit conversion_window wins",
			req: &insightsv1.QueryRequest{
				Spec: &insightsv1.InsightQuerySpec{
					ConversionWindow: durationpb.New(24 * time.Hour),
				},
				TimeRange: &commonv1.TimeRange{From: timestamppb.New(from), To: timestamppb.New(to)},
			},
			want: 86400,
		},
		{
			name: "absent conversion_window falls back to time range",
			req: &insightsv1.QueryRequest{
				TimeRange: &commonv1.TimeRange{From: timestamppb.New(from), To: timestamppb.New(to)},
			},
			want: 7 * 86400,
		},
		{
			name: "inverted time range with no conversion_window errors",
			req: &insightsv1.QueryRequest{
				TimeRange: &commonv1.TimeRange{From: timestamppb.New(to), To: timestamppb.New(from)},
			},
			wantErr:     true,
			errContains: "time_range",
		},
		{
			name: "zero-length time range with no conversion_window errors",
			req: &insightsv1.QueryRequest{
				TimeRange: &commonv1.TimeRange{From: timestamppb.New(from), To: timestamppb.New(from)},
			},
			wantErr:     true,
			errContains: "time_range",
		},
		{
			// Direct callers (workers/scripts/tests) bypass protovalidate's gte: 1s rule;
			// the in-source `s <= 0` guard catches the sub-second case here.
			name: "sub-second positive window errors (defends non-RPC callers)",
			req: &insightsv1.QueryRequest{
				Spec: &insightsv1.InsightQuerySpec{
					ConversionWindow: durationpb.New(500 * time.Millisecond),
				},
				TimeRange: &commonv1.TimeRange{From: timestamppb.New(from), To: timestamppb.New(to)},
			},
			wantErr:     true,
			errContains: "conversion_window",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := insights.EffectiveWindowSec(tc.req)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil (returned %d)", got)
				}
				if tc.errContains != "" && !strings.Contains(err.Error(), tc.errContains) {
					t.Errorf("error message: got %q, want substring %q", err.Error(), tc.errContains)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %d, want %d", got, tc.want)
			}
		})
	}
}

// TestGroupFunnelSeries_TimingFieldsSubSecondTranslation pins that fractional-second
// internal durations survive the proto-translation seam without truncation. A regression
// that converts time.Duration through a float-seconds intermediate (e.g.
// `time.Duration(s.Seconds()) * time.Second`) would silently round 500ms to 0.
func TestGroupFunnelSeries_TimingFieldsSubSecondTranslation(t *testing.T) {
	rows := []insights.FunnelRow{
		{StepIndex: 0, EventKind: "sign_up", Value: 5},
		{
			StepIndex: 1,
			EventKind: "purchase",
			Value:     3,
			Timing: &insights.StepTiming{
				Avg:          500 * time.Millisecond,
				Median:       250 * time.Millisecond,
				P95:          1500 * time.Millisecond,
				Distribution: make([]int64, 8),
			},
		},
	}
	series, err := insights.GroupFunnelSeries(ctx, rows, nil)
	if err != nil {
		t.Fatalf("GroupFunnelSeries: %v", err)
	}
	if len(series) != 1 {
		t.Fatalf("expected 1 series, got %d", len(series))
	}
	timing := series[0].Steps[1].GetTiming()
	if got, want := timing.GetAvg().AsDuration(), 500*time.Millisecond; got != want {
		t.Errorf("avg: got %v, want %v", got, want)
	}
	if got, want := timing.GetMedian().AsDuration(), 250*time.Millisecond; got != want {
		t.Errorf("median: got %v, want %v", got, want)
	}
	if got, want := timing.GetP95().AsDuration(), 1500*time.Millisecond; got != want {
		t.Errorf("p95: got %v, want %v", got, want)
	}
}

// TestComputeFunnelTimingMatchesCountsAtBoundary verifies that the timing path
// (Go-side window check using strict `>`) and the counts path (windowFunnel using `<=`)
// agree on step counts for a window-boundary scenario. Without parity, requesting
// include_step_timing would silently change visible step counts.
func TestComputeFunnelTimingMatchesCountsAtBoundary(t *testing.T) {
	t0 := time.Date(2024, 1, 1, 10, 0, 0, 0, time.UTC)
	users := []insights.FunnelUserEvents{
		// Exactly at the 1-hour boundary — both paths must include this user at step 1.
		{DistinctID: "u-boundary", Times: []time.Time{t0, t0.Add(1 * time.Hour)}, StepMatches: []int64{0, 1}},
		// One nanosecond past the boundary — both paths must exclude this user at step 1.
		{DistinctID: "u-just-past", Times: []time.Time{t0, t0.Add(1*time.Hour + time.Nanosecond)}, StepMatches: []int64{0, 1}},
	}
	rows, err := insights.ComputeFunnelTiming(ctx, "", users, []string{"a", "b"}, 3600, 0)
	if err != nil {
		t.Fatalf("ComputeFunnelTiming: %v", err)
	}
	// Expect: step 0 = 2 (both users), step 1 = 1 (only u-boundary).
	if rows[0].Value != 2 {
		t.Errorf("timing path step 0: got %v, want 2", rows[0].Value)
	}
	if rows[1].Value != 1 {
		t.Errorf("timing path step 1 at boundary: got %v, want 1 (boundary inclusive)", rows[1].Value)
	}
	// windowFunnel uses `<=` for window inclusion — same result.
	// We can't easily run windowFunnel from a unit test, but TestComputeFunnelTiming_WindowExactBoundary
	// pins the timing-path side; this test pairs with that to cement boundary semantics.
	// See integration_test.go for the end-to-end CH-side equivalence.
}
