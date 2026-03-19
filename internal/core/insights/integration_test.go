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
