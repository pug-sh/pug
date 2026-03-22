package insights_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/fivebitsio/cotton/internal/core/insights"
	insightsv1 "github.com/fivebitsio/cotton/internal/gen/proto/insights/v1"
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
			TimeRange: &insightsv1.TimeRange{
				From: timestamppb.New(time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)),
				To:   timestamppb.New(time.Date(2024, 1, 4, 0, 0, 0, 0, time.UTC)),
			},
			Granularity: insightsv1.Granularity_GRANULARITY_DAY,
			Events: []*insightsv1.EventQuery{
				{Kind: "page_view", Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL},
			},
		}

		sql, args, err := insights.BuildQuery(req, testProjectID)
		if err != nil {
			t.Fatalf("BuildQuery: %v", err)
		}

		rows, err := executor.QueryTrends(ctx, sql, args)
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
			TimeRange: &insightsv1.TimeRange{
				From: timestamppb.New(time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)),
				To:   timestamppb.New(time.Date(2024, 1, 4, 0, 0, 0, 0, time.UTC)),
			},
			Granularity: insightsv1.Granularity_GRANULARITY_DAY,
			Events: []*insightsv1.EventQuery{
				{Kind: "page_view", Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_UNIQUE_USERS},
			},
		}

		sql, args, err := insights.BuildQuery(req, testProjectID)
		if err != nil {
			t.Fatalf("BuildQuery: %v", err)
		}

		rows, err := executor.QueryTrends(ctx, sql, args)
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
			TimeRange: &insightsv1.TimeRange{
				From: timestamppb.New(time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)),
				To:   timestamppb.New(time.Date(2024, 1, 4, 0, 0, 0, 0, time.UTC)),
			},
			Granularity: insightsv1.Granularity_GRANULARITY_DAY,
			Events: []*insightsv1.EventQuery{
				{Kind: "page_view", Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL},
			},
			Breakdowns:     []*insightsv1.Breakdown{{Property: "country"}},
			BreakdownLimit: 10,
		}

		sql, args, err := insights.BuildQuery(req, testProjectID)
		if err != nil {
			t.Fatalf("BuildQuery: %v", err)
		}

		rows, err := executor.QueryTrendsWithBreakdowns(ctx, sql, args, 1)
		if err != nil {
			t.Fatalf("QueryTrendsWithBreakdowns: %v", err)
		}

		series := insights.GroupBreakdownSeries(rows, []string{"country"})
		if len(series) < 2 {
			t.Fatalf("expected at least 2 breakdown series (US, GB), got %d", len(series))
		}
	})

	t.Run("segmentation", func(t *testing.T) {
		req := &insightsv1.QueryRequest{
			InsightType: insightsv1.InsightType_INSIGHT_TYPE_SEGMENTATION,
			TimeRange: &insightsv1.TimeRange{
				From: timestamppb.New(time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)),
				To:   timestamppb.New(time.Date(2024, 1, 4, 0, 0, 0, 0, time.UTC)),
			},
			Events: []*insightsv1.EventQuery{
				{Kind: "page_view", Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL},
			},
		}

		sql, args, err := insights.BuildQuery(req, testProjectID)
		if err != nil {
			t.Fatalf("BuildQuery: %v", err)
		}

		value, err := executor.QueryScalar(ctx, sql, args)
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
			TimeRange: &insightsv1.TimeRange{
				From: timestamppb.New(time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)),
				To:   timestamppb.New(time.Date(2024, 1, 4, 0, 0, 0, 0, time.UTC)),
			},
			Events: []*insightsv1.EventQuery{
				{Kind: "page_view", Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL},
			},
			Filters: []*insightsv1.PropertyFilter{
				{Property: "country", Operator: insightsv1.FilterOperator_FILTER_OPERATOR_EQUALS, Value: "US"},
			},
		}

		sql, args, err := insights.BuildQuery(req, testProjectID)
		if err != nil {
			t.Fatalf("BuildQuery: %v", err)
		}

		value, err := executor.QueryScalar(ctx, sql, args)
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
			TimeRange: &insightsv1.TimeRange{
				From: timestamppb.New(time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)),
				To:   timestamppb.New(time.Date(2024, 1, 4, 0, 0, 0, 0, time.UTC)),
			},
			Granularity: insightsv1.Granularity_GRANULARITY_DAY,
			Events: []*insightsv1.EventQuery{
				{Kind: "page_view", Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_PER_USER_AVG},
			},
		}

		sql, args, err := insights.BuildQuery(req, testProjectID)
		if err != nil {
			t.Fatalf("BuildQuery: %v", err)
		}

		rows, err := executor.QueryTrends(ctx, sql, args)
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
		tr := &insightsv1.TimeRange{
			From: timestamppb.New(time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)),
			To:   timestamppb.New(time.Date(2024, 1, 4, 0, 0, 0, 0, time.UTC)),
		}
		events := []*insightsv1.EventQuery{
			{Kind: "page_view", Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL},
		}

		// First page: size 2
		req1 := &insightsv1.SegmentUsersRequest{
			TimeRange: tr, Events: events, PageSize: 2,
		}
		sql1, args1, err := insights.BuildSegmentUsersQuery(req1, testProjectID)
		if err != nil {
			t.Fatalf("BuildSegmentUsersQuery page1: %v", err)
		}
		page1, err := executor.QueryDistinctIDs(ctx, sql1, args1)
		if err != nil {
			t.Fatalf("QueryDistinctIDs page1: %v", err)
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
		page2, err := executor.QueryDistinctIDs(ctx, sql2, args2)
		if err != nil {
			t.Fatalf("QueryDistinctIDs page2: %v", err)
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
			TimeRange: &insightsv1.TimeRange{
				From: timestamppb.New(time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)),
				To:   timestamppb.New(time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC)),
			},
			Granularity: insightsv1.Granularity_GRANULARITY_DAY,
			Events: []*insightsv1.EventQuery{
				{Kind: "purchase", Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL},
			},
			Breakdowns:     []*insightsv1.Breakdown{{Property: "country"}},
			BreakdownLimit: 2, // Only top 2 countries, rest go to $others
		}

		sql, args, err := insights.BuildQuery(req, testProjectID)
		if err != nil {
			t.Fatalf("BuildQuery: %v", err)
		}

		rows, err := executor.QueryTrendsWithBreakdowns(ctx, sql, args, 1)
		if err != nil {
			t.Fatalf("QueryTrendsWithBreakdowns: %v", err)
		}

		series := insights.GroupBreakdownSeries(rows, []string{"country"})

		// Should have top 2 + $others = 3 series
		if len(series) != 3 {
			t.Fatalf("expected 3 series (top 2 + $others), got %d", len(series))
		}

		hasOthers := false
		for _, s := range series {
			if s.Breakdown["country"] == "$others" {
				hasOthers = true
			}
		}
		if !hasOthers {
			t.Error("expected $others bucket in breakdown series")
		}
	})

	t.Run("segment_users", func(t *testing.T) {
		req := &insightsv1.SegmentUsersRequest{
			TimeRange: &insightsv1.TimeRange{
				From: timestamppb.New(time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)),
				To:   timestamppb.New(time.Date(2024, 1, 4, 0, 0, 0, 0, time.UTC)),
			},
			Events: []*insightsv1.EventQuery{
				{Kind: "page_view", Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL},
			},
			PageSize: 100,
		}

		sql, args, err := insights.BuildSegmentUsersQuery(req, testProjectID)
		if err != nil {
			t.Fatalf("BuildSegmentUsersQuery: %v", err)
		}

		ids, err := executor.QueryDistinctIDs(ctx, sql, args)
		if err != nil {
			t.Fatalf("QueryDistinctIDs: %v", err)
		}

		// 3 distinct users: alice, bob, charlie
		if len(ids) != 3 {
			t.Errorf("expected 3 distinct users, got %d: %v", len(ids), ids)
		}
	})
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
			map[string]string{"country": e.country},
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
			map[string]string{"country": e.country},
		)
		if err != nil {
			t.Fatalf("insert purchase event: %v", err)
		}
	}
}
