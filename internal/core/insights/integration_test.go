package insights_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/fivebitsio/cotton/internal/core/insights"
	commonv1 "github.com/fivebitsio/cotton/internal/gen/proto/common/v1"
	insightsv1 "github.com/fivebitsio/cotton/internal/gen/proto/shared/insights/v1"
	"github.com/fivebitsio/cotton/internal/testutil"
)

const testProjectID = "proj_integration"

func TestIntegration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ch := testutil.SetupClickHouse(t)
	ctx := context.Background()

	seedEvents(t, ctx, ch)
	executor := insights.NewExecutor(ch.Conn)

	t.Run("trends_daily", func(t *testing.T) {
		req := &insightsv1.QueryRequest{
			InsightType: insightsv1.InsightType_INSIGHT_TYPE_TRENDS,
			TimeRange: &commonv1.TimeRange{
				From: timestamppb.New(time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)),
				To:   timestamppb.New(time.Date(2024, 1, 4, 0, 0, 0, 0, time.UTC)),
			},
			Granularity: insightsv1.Granularity_GRANULARITY_DAY,
			Events: []*insightsv1.EventQuery{
				{Event: &commonv1.EventFilter{Kind: "page_view"}, Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL},
			},
		}

		q, err := insights.BuildTrendsQuery(req, testProjectID)
		if err != nil {
			t.Fatalf("BuildTrendsQuery: %v", err)
		}

		rows, err := executor.QueryTrends(ctx, q)
		if err != nil {
			t.Fatalf("QueryTrends: %v", err)
		}

		// Seed data: 3 page_views on Jan 1, 2 on Jan 2, 1 on Jan 3
		if len(rows) != 3 {
			t.Fatalf("expected 3 daily buckets, got %d", len(rows))
		}
		wantCounts := map[int]float64{1: 3, 2: 2, 3: 1}
		for _, r := range rows {
			day := r.Time.Day()
			if want, ok := wantCounts[day]; ok {
				if r.Value != want {
					t.Errorf("day %d: expected count %.0f, got %.0f", day, want, r.Value)
				}
			}
		}
	})

	t.Run("trends_unique_users", func(t *testing.T) {
		req := &insightsv1.QueryRequest{
			InsightType: insightsv1.InsightType_INSIGHT_TYPE_TRENDS,
			TimeRange: &commonv1.TimeRange{
				From: timestamppb.New(time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)),
				To:   timestamppb.New(time.Date(2024, 1, 4, 0, 0, 0, 0, time.UTC)),
			},
			Granularity: insightsv1.Granularity_GRANULARITY_DAY,
			Events: []*insightsv1.EventQuery{
				{Event: &commonv1.EventFilter{Kind: "page_view"}, Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_UNIQUE_USERS},
			},
		}

		q, err := insights.BuildTrendsQuery(req, testProjectID)
		if err != nil {
			t.Fatalf("BuildTrendsQuery: %v", err)
		}

		rows, err := executor.QueryTrends(ctx, q)
		if err != nil {
			t.Fatalf("QueryTrends: %v", err)
		}

		// Jan 1: alice + bob + charlie = 3 unique, Jan 2: alice + bob = 2, Jan 3: alice = 1
		if len(rows) != 3 {
			t.Fatalf("expected 3 daily buckets, got %d", len(rows))
		}
		wantUnique := map[int]float64{1: 3, 2: 2, 3: 1}
		for _, r := range rows {
			day := r.Time.Day()
			if want, ok := wantUnique[day]; ok {
				if r.Value != want {
					t.Errorf("day %d: expected %v unique users, got %v", day, want, r.Value)
				}
			}
		}
	})

	t.Run("trends_with_breakdown", func(t *testing.T) {
		req := &insightsv1.QueryRequest{
			InsightType: insightsv1.InsightType_INSIGHT_TYPE_TRENDS,
			TimeRange: &commonv1.TimeRange{
				From: timestamppb.New(time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)),
				To:   timestamppb.New(time.Date(2024, 1, 4, 0, 0, 0, 0, time.UTC)),
			},
			Granularity: insightsv1.Granularity_GRANULARITY_DAY,
			Events: []*insightsv1.EventQuery{
				{Event: &commonv1.EventFilter{Kind: "page_view"}, Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL},
			},
			Breakdowns:     []*insightsv1.Breakdown{{Property: "$country"}},
			BreakdownLimit: 10,
		}

		q, err := insights.BuildTrendsQuery(req, testProjectID)
		if err != nil {
			t.Fatalf("BuildTrendsQuery: %v", err)
		}

		rows, err := executor.QueryTrends(ctx, q)
		if err != nil {
			t.Fatalf("QueryTrends: %v", err)
		}

		series, err := insights.GroupSeries(rows, q.Properties())
		if err != nil {
			t.Fatalf("GroupSeries: %v", err)
		}
		if len(series) < 2 {
			t.Fatalf("expected at least 2 breakdown series (US, GB), got %d", len(series))
		}
	})

	t.Run("segmentation", func(t *testing.T) {
		req := &insightsv1.QueryRequest{
			InsightType: insightsv1.InsightType_INSIGHT_TYPE_SEGMENTATION,
			TimeRange: &commonv1.TimeRange{
				From: timestamppb.New(time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)),
				To:   timestamppb.New(time.Date(2024, 1, 4, 0, 0, 0, 0, time.UTC)),
			},
			Events: []*insightsv1.EventQuery{
				{Event: &commonv1.EventFilter{Kind: "page_view"}, Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL},
			},
		}

		q, err := insights.BuildSegmentationQuery(req, testProjectID)
		if err != nil {
			t.Fatalf("BuildSegmentationQuery: %v", err)
		}

		value, err := executor.QueryScalar(ctx, q)
		if err != nil {
			t.Fatalf("QueryScalar: %v", err)
		}

		// Total page_views: 3 + 2 + 1 = 6
		if value != 6 {
			t.Errorf("expected total 6, got %v", value)
		}
	})

	t.Run("segmentation_with_filter", func(t *testing.T) {
		req := &insightsv1.QueryRequest{
			InsightType: insightsv1.InsightType_INSIGHT_TYPE_SEGMENTATION,
			TimeRange: &commonv1.TimeRange{
				From: timestamppb.New(time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)),
				To:   timestamppb.New(time.Date(2024, 1, 4, 0, 0, 0, 0, time.UTC)),
			},
			Events: []*insightsv1.EventQuery{
				{Event: &commonv1.EventFilter{Kind: "page_view"}, Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL},
			},
			FilterGroups: []*insightsv1.FilterGroup{
				{
					Operator: commonv1.LogicalOperator_LOGICAL_OPERATOR_AND,
					Filters: []*commonv1.PropertyFilter{
						{Property: "$country", Operator: commonv1.FilterOperator_FILTER_OPERATOR_EQUALS, Value: "US"},
					},
				},
			},
		}

		q, err := insights.BuildSegmentationQuery(req, testProjectID)
		if err != nil {
			t.Fatalf("BuildSegmentationQuery: %v", err)
		}

		value, err := executor.QueryScalar(ctx, q)
		if err != nil {
			t.Fatalf("QueryScalar: %v", err)
		}

		// US page_views: alice(US)+charlie(US) Jan1 + alice(US) Jan2 + alice(US) Jan3 = 4
		if value != 4 {
			t.Errorf("expected 4 US page_views, got %v", value)
		}
	})

	t.Run("per_user_avg", func(t *testing.T) {
		req := &insightsv1.QueryRequest{
			InsightType: insightsv1.InsightType_INSIGHT_TYPE_TRENDS,
			TimeRange: &commonv1.TimeRange{
				From: timestamppb.New(time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)),
				To:   timestamppb.New(time.Date(2024, 1, 4, 0, 0, 0, 0, time.UTC)),
			},
			Granularity: insightsv1.Granularity_GRANULARITY_DAY,
			Events: []*insightsv1.EventQuery{
				{Event: &commonv1.EventFilter{Kind: "page_view"}, Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_PER_USER_AVG},
			},
		}

		q, err := insights.BuildTrendsQuery(req, testProjectID)
		if err != nil {
			t.Fatalf("BuildTrendsQuery: %v", err)
		}

		rows, err := executor.QueryTrends(ctx, q)
		if err != nil {
			t.Fatalf("QueryTrends: %v", err)
		}

		// Jan 1: 3 events / 3 users = 1.0, Jan 2: 2/2 = 1.0, Jan 3: 1/1 = 1.0
		for _, r := range rows {
			if r.Value != 1.0 {
				t.Errorf("day %d: expected per-user avg 1.0, got %v", r.Time.Day(), r.Value)
			}
		}
	})

	t.Run("segment_users_pagination", func(t *testing.T) {
		tr := &commonv1.TimeRange{
			From: timestamppb.New(time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)),
			To:   timestamppb.New(time.Date(2024, 1, 4, 0, 0, 0, 0, time.UTC)),
		}
		events := []*insightsv1.EventQuery{
			{Event: &commonv1.EventFilter{Kind: "page_view"}, Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL},
		}

		// First page: size 2
		req1 := &insightsv1.SegmentUsersRequest{
			TimeRange: tr, Events: events, PageSize: 2,
		}
		sql1, args1, err := insights.BuildSegmentUsersQuery(req1, testProjectID)
		if err != nil {
			t.Fatalf("BuildSegmentUsersQuery page1: %v", err)
		}
		page1, err := executor.QueryStringColumn(ctx, sql1, args1)
		if err != nil {
			t.Fatalf("QueryStringColumn page1: %v", err)
		}
		if len(page1) != 2 {
			t.Fatalf("expected 2 IDs on page 1, got %d: %v", len(page1), page1)
		}

		// Second page: cursor from last ID of page 1
		req2 := &insightsv1.SegmentUsersRequest{
			TimeRange: tr, Events: events, PageSize: 2, PageToken: page1[len(page1)-1],
		}
		sql2, args2, err := insights.BuildSegmentUsersQuery(req2, testProjectID)
		if err != nil {
			t.Fatalf("BuildSegmentUsersQuery page2: %v", err)
		}
		page2, err := executor.QueryStringColumn(ctx, sql2, args2)
		if err != nil {
			t.Fatalf("QueryStringColumn page2: %v", err)
		}
		if len(page2) != 1 {
			t.Fatalf("expected 1 ID on page 2, got %d: %v", len(page2), page2)
		}

		// No overlap between pages
		for _, id := range page2 {
			for _, prev := range page1 {
				if id == prev {
					t.Errorf("page2 ID %q already appeared on page1", id)
				}
			}
		}
	})

	t.Run("breakdown_others_bucket", func(t *testing.T) {
		// Seed purchase events with 4 distinct countries
		seedPurchases(t, ctx, ch)

		req := &insightsv1.QueryRequest{
			InsightType: insightsv1.InsightType_INSIGHT_TYPE_TRENDS,
			TimeRange: &commonv1.TimeRange{
				From: timestamppb.New(time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)),
				To:   timestamppb.New(time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC)),
			},
			Granularity: insightsv1.Granularity_GRANULARITY_DAY,
			Events: []*insightsv1.EventQuery{
				{Event: &commonv1.EventFilter{Kind: "purchase"}, Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL},
			},
			Breakdowns:     []*insightsv1.Breakdown{{Property: "$country"}},
			BreakdownLimit: 2, // Only top 2 countries, rest go to $others
		}

		q, err := insights.BuildTrendsQuery(req, testProjectID)
		if err != nil {
			t.Fatalf("BuildTrendsQuery: %v", err)
		}

		rows, err := executor.QueryTrends(ctx, q)
		if err != nil {
			t.Fatalf("QueryTrends: %v", err)
		}

		series, err := insights.GroupSeries(rows, q.Properties())
		if err != nil {
			t.Fatalf("GroupSeries: %v", err)
		}

		// Should have top 2 + $others = 3 series
		if len(series) != 3 {
			t.Fatalf("expected 3 series (top 2 + $others), got %d", len(series))
		}

		hasOthers := false
		for _, s := range series {
			if s.Breakdown["$country"] == "$others" {
				hasOthers = true
			}
		}
		if !hasOthers {
			t.Error("expected $others bucket in breakdown series")
		}
	})

	t.Run("multi_event_trends", func(t *testing.T) {
		// Uses seed data: page_view (Jan 1-3) and purchase (Jan 1 only, from seedPurchases)
		req := &insightsv1.QueryRequest{
			InsightType: insightsv1.InsightType_INSIGHT_TYPE_TRENDS,
			TimeRange: &commonv1.TimeRange{
				From: timestamppb.New(time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)),
				To:   timestamppb.New(time.Date(2024, 1, 4, 0, 0, 0, 0, time.UTC)),
			},
			Granularity: insightsv1.Granularity_GRANULARITY_DAY,
			Events: []*insightsv1.EventQuery{
				{Event: &commonv1.EventFilter{Kind: "page_view"}, Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL},
				{Event: &commonv1.EventFilter{Kind: "purchase"}, Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL},
			},
		}

		q, err := insights.BuildTrendsQuery(req, testProjectID)
		if err != nil {
			t.Fatalf("BuildTrendsQuery: %v", err)
		}

		rows, err := executor.QueryTrends(ctx, q)
		if err != nil {
			t.Fatalf("QueryTrends: %v", err)
		}

		series, err := insights.GroupSeries(rows, q.Properties())
		if err != nil {
			t.Fatalf("GroupSeries: %v", err)
		}
		if len(series) != 2 {
			t.Fatalf("expected 2 series (page_view, purchase), got %d", len(series))
		}

		kindSet := map[string]bool{}
		for _, s := range series {
			kindSet[s.EventKind] = true
		}
		if !kindSet["page_view"] || !kindSet["purchase"] {
			t.Errorf("expected both page_view and purchase series, got kinds: %v", kindSet)
		}
	})

	t.Run("segment_users", func(t *testing.T) {
		req := &insightsv1.SegmentUsersRequest{
			TimeRange: &commonv1.TimeRange{
				From: timestamppb.New(time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)),
				To:   timestamppb.New(time.Date(2024, 1, 4, 0, 0, 0, 0, time.UTC)),
			},
			Events: []*insightsv1.EventQuery{
				{Event: &commonv1.EventFilter{Kind: "page_view"}, Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL},
			},
			PageSize: 100,
		}

		sql, args, err := insights.BuildSegmentUsersQuery(req, testProjectID)
		if err != nil {
			t.Fatalf("BuildSegmentUsersQuery: %v", err)
		}

		ids, err := executor.QueryStringColumn(ctx, sql, args)
		if err != nil {
			t.Fatalf("QueryStringColumn: %v", err)
		}

		// 3 distinct users: alice, bob, charlie
		if len(ids) != 3 {
			t.Errorf("expected 3 distinct users, got %d: %v", len(ids), ids)
		}
	})

	t.Run("funnel_counts", func(t *testing.T) {
		seedFunnelEvents(t, ctx, ch)

		req := &insightsv1.QueryRequest{
			InsightType: insightsv1.InsightType_INSIGHT_TYPE_FUNNEL,
			TimeRange: &commonv1.TimeRange{
				From: timestamppb.New(time.Date(2024, 2, 1, 0, 0, 0, 0, time.UTC)),
				To:   timestamppb.New(time.Date(2024, 2, 8, 0, 0, 0, 0, time.UTC)),
			},
			Events: []*insightsv1.EventQuery{
				{Event: &commonv1.EventFilter{Kind: "sign_up"}, Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL},
				{Event: &commonv1.EventFilter{Kind: "add_to_cart"}, Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL},
				{Event: &commonv1.EventFilter{Kind: "purchase"}, Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL},
			},
		}

		q, err := insights.BuildFunnelCountsQuery(req, testProjectID)
		if err != nil {
			t.Fatalf("BuildFunnelCountsQuery: %v", err)
		}

		rows, err := executor.QueryFunnel(ctx, q)
		if err != nil {
			t.Fatalf("QueryFunnel: %v", err)
		}

		if len(rows) != 3 {
			t.Fatalf("expected 3 funnel steps, got %d", len(rows))
		}
		// Seed: 3 sign_ups, 2 add_to_carts, 1 purchase
		if rows[0].Value != 3 {
			t.Errorf("step 0 (sign_up): expected 3 users, got %v", rows[0].Value)
		}
		if rows[1].Value != 2 {
			t.Errorf("step 1 (add_to_cart): expected 2 users, got %v", rows[1].Value)
		}
		if rows[2].Value != 1 {
			t.Errorf("step 2 (purchase): expected 1 user, got %v", rows[2].Value)
		}
	})

	t.Run("funnel_timing", func(t *testing.T) {
		// Uses same seed data from funnel_counts.
		req := &insightsv1.QueryRequest{
			InsightType:      insightsv1.InsightType_INSIGHT_TYPE_FUNNEL,
			IncludeStepTiming: true,
			TimeRange: &commonv1.TimeRange{
				From: timestamppb.New(time.Date(2024, 2, 1, 0, 0, 0, 0, time.UTC)),
				To:   timestamppb.New(time.Date(2024, 2, 8, 0, 0, 0, 0, time.UTC)),
			},
			Events: []*insightsv1.EventQuery{
				{Event: &commonv1.EventFilter{Kind: "sign_up"}, Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL},
				{Event: &commonv1.EventFilter{Kind: "add_to_cart"}, Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL},
				{Event: &commonv1.EventFilter{Kind: "purchase"}, Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL},
			},
		}

		q, err := insights.BuildFunnelTimingQuery(req, testProjectID)
		if err != nil {
			t.Fatalf("BuildFunnelTimingQuery: %v", err)
		}

		users, err := executor.QueryFunnelUserEvents(ctx, q)
		if err != nil {
			t.Fatalf("QueryFunnelUserEvents: %v", err)
		}

		rows, err := insights.ComputeFunnelTiming(users, q.Kinds(), q.WindowSec())
		if err != nil {
			t.Fatalf("ComputeFunnelTiming: %v", err)
		}

		if len(rows) != 3 {
			t.Fatalf("expected 3 funnel steps, got %d", len(rows))
		}
		if rows[0].Value != 3 {
			t.Errorf("step 0 (sign_up): expected 3 users, got %v", rows[0].Value)
		}
		if rows[1].Value != 2 {
			t.Errorf("step 1 (add_to_cart): expected 2 users, got %v", rows[1].Value)
		}
		if rows[2].Value != 1 {
			t.Errorf("step 2 (purchase): expected 1 user, got %v", rows[2].Value)
		}
		// Step 1 avg time should be positive (sign_up → add_to_cart gap).
		if rows[1].AvgConvertSeconds <= 0 {
			t.Errorf("step 1 avg time should be > 0, got %v", rows[1].AvgConvertSeconds)
		}
	})

	t.Run("retention", func(t *testing.T) {
		seedRetentionEvents(t, ctx, ch)

		req := &insightsv1.QueryRequest{
			InsightType: insightsv1.InsightType_INSIGHT_TYPE_RETENTION,
			TimeRange: &commonv1.TimeRange{
				From: timestamppb.New(time.Date(2024, 3, 1, 0, 0, 0, 0, time.UTC)),
				To:   timestamppb.New(time.Date(2024, 3, 8, 0, 0, 0, 0, time.UTC)),
			},
			Granularity: insightsv1.Granularity_GRANULARITY_DAY,
			Events: []*insightsv1.EventQuery{
				{Event: &commonv1.EventFilter{Kind: "sign_up"}, Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL},
				{Event: &commonv1.EventFilter{Kind: "login"}, Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL},
			},
		}

		q, err := insights.BuildRetentionQuery(req, testProjectID)
		if err != nil {
			t.Fatalf("BuildRetentionQuery: %v", err)
		}

		rows, err := executor.QueryRetention(ctx, q)
		if err != nil {
			t.Fatalf("QueryRetention: %v", err)
		}

		cohorts := insights.GroupRetentionCohorts(rows)
		// At least 1 cohort (Mar 1 sign_ups).
		if len(cohorts) == 0 {
			t.Fatal("expected at least 1 retention cohort")
		}
		// First cohort should have day-0 retention = 100%.
		if cohorts[0].Points[0].Value != 100 {
			t.Errorf("expected 100%% day-0 retention, got %v", cohorts[0].Points[0].Value)
		}
		// Cohort size should match seeded sign_ups.
		if cohorts[0].CohortSize != 3 {
			t.Errorf("expected cohort size 3, got %v", cohorts[0].CohortSize)
		}
	})
}

// seedFunnelEvents inserts events for funnel integration tests.
//
// Layout (all Feb 2024, project_id = testProjectID):
//
//	alice:   sign_up (Feb 1 10:00) → add_to_cart (Feb 1 11:00) → purchase (Feb 1 12:00)
//	bob:     sign_up (Feb 1 10:00) → add_to_cart (Feb 1 14:00)
//	charlie: sign_up (Feb 1 10:00)
//
// Funnel: 3 → 2 → 1
func seedFunnelEvents(t *testing.T, ctx context.Context, ch *testutil.TestClickHouse) {
	t.Helper()

	type event struct {
		user string
		kind string
		hour int
	}

	events := []event{
		{"alice", "sign_up", 10},
		{"alice", "add_to_cart", 11},
		{"alice", "purchase", 12},
		{"bob", "sign_up", 10},
		{"bob", "add_to_cart", 14},
		{"charlie", "sign_up", 10},
	}

	for _, e := range events {
		occurTime := time.Date(2024, 2, 1, e.hour, 0, 0, 0, time.UTC)
		err := ch.Conn.Exec(ctx,
			`INSERT INTO events (project_id, event_id, kind, distinct_id, occur_time, auto_properties) VALUES (?, ?, ?, ?, ?, ?)`,
			testProjectID,
			uuid.New().String(),
			e.kind,
			e.user,
			occurTime,
			map[string]string{},
		)
		if err != nil {
			t.Fatalf("insert funnel event: %v", err)
		}
	}
}

// seedRetentionEvents inserts events for retention integration tests.
//
// Layout (all Mar 2024, project_id = testProjectID):
//
//	Mar 1: alice, bob, charlie sign_up (10:00)
//	Mar 1: alice, bob, charlie login (12:00) — day-0 return, all 3 = 100%
//	Mar 2: alice, bob login — day-1 return, 2/3 ≈ 66.7%
//	Mar 3: alice login — day-2 return, 1/3 ≈ 33.3%
func seedRetentionEvents(t *testing.T, ctx context.Context, ch *testutil.TestClickHouse) {
	t.Helper()

	type event struct {
		user string
		kind string
		day  int
		hour int
	}

	events := []event{
		// Cohort entry: all 3 sign up on Mar 1.
		{"alice", "sign_up", 1, 10},
		{"bob", "sign_up", 1, 10},
		{"charlie", "sign_up", 1, 10},
		// Day-0 returns: all 3 login on Mar 1 (after sign_up).
		{"alice", "login", 1, 12},
		{"bob", "login", 1, 12},
		{"charlie", "login", 1, 12},
		// Day-1 returns: alice and bob login.
		{"alice", "login", 2, 12},
		{"bob", "login", 2, 12},
		// Day-2 return: alice only.
		{"alice", "login", 3, 12},
	}

	for _, e := range events {
		occurTime := time.Date(2024, 3, e.day, e.hour, 0, 0, 0, time.UTC)
		err := ch.Conn.Exec(ctx,
			`INSERT INTO events (project_id, event_id, kind, distinct_id, occur_time, auto_properties) VALUES (?, ?, ?, ?, ?, ?)`,
			testProjectID,
			uuid.New().String(),
			e.kind,
			e.user,
			occurTime,
			map[string]string{},
		)
		if err != nil {
			t.Fatalf("insert retention event: %v", err)
		}
	}
}

// seedEvents inserts a deterministic set of events for integration testing.
//
// Layout (all page_view, project_id = testProjectID):
//
//	Jan 1: alice(US), bob(GB), charlie(US)  → 3 events, 3 unique users
//	Jan 2: alice(US), bob(GB)               → 2 events, 2 unique users
//	Jan 3: alice(US)                        → 1 event,  1 unique user
//
// Total: 6 events, 3 unique users, 3 US + 3 GB
func seedEvents(t *testing.T, ctx context.Context, ch *testutil.TestClickHouse) {
	t.Helper()

	type event struct {
		day     int
		user    string
		country string
	}

	events := []event{
		{1, "alice", "US"},
		{1, "bob", "GB"},
		{1, "charlie", "US"},
		{2, "alice", "US"},
		{2, "bob", "GB"},
		{3, "alice", "US"},
	}

	for _, e := range events {
		occurTime := time.Date(2024, 1, e.day, 12, 0, 0, 0, time.UTC)
		err := ch.Conn.Exec(ctx,
			`INSERT INTO events (project_id, event_id, kind, distinct_id, occur_time, auto_properties) VALUES (?, ?, ?, ?, ?, ?)`,
			testProjectID,
			uuid.New().String(),
			"page_view",
			e.user,
			occurTime,
			map[string]string{"$country": e.country},
		)
		if err != nil {
			t.Fatalf("insert event: %v", err)
		}
	}
}

// seedPurchases inserts purchase events with 4 distinct countries for breakdown $others testing.
//
// Layout (all purchase, Jan 1):
//
//	US: 5 events, GB: 3 events, FR: 2 events, JP: 1 event
//
// With breakdown_limit=2, top 2 (US, GB) stay; FR + JP → $others.
func seedPurchases(t *testing.T, ctx context.Context, ch *testutil.TestClickHouse) {
	t.Helper()

	type event struct {
		user    string
		country string
	}

	events := []event{
		{"u1", "US"}, {"u2", "US"}, {"u3", "US"}, {"u4", "US"}, {"u5", "US"},
		{"u6", "GB"}, {"u7", "GB"}, {"u8", "GB"},
		{"u9", "FR"}, {"u10", "FR"},
		{"u11", "JP"},
	}

	occurTime := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	for _, e := range events {
		err := ch.Conn.Exec(ctx,
			`INSERT INTO events (project_id, event_id, kind, distinct_id, occur_time, auto_properties) VALUES (?, ?, ?, ?, ?, ?)`,
			testProjectID,
			uuid.New().String(),
			"purchase",
			e.user,
			occurTime,
			map[string]string{"$country": e.country},
		)
		if err != nil {
			t.Fatalf("insert purchase event: %v", err)
		}
	}
}
