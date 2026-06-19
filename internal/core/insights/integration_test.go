package insights_test

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	chcol "github.com/ClickHouse/clickhouse-go/v2/lib/chcol"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	chq "github.com/pug-sh/pug/internal/core/clickhouse"
	"github.com/pug-sh/pug/internal/core/insights"
	commonv1 "github.com/pug-sh/pug/internal/gen/proto/common/v1"
	insightsv1 "github.com/pug-sh/pug/internal/gen/proto/shared/insights/v1"
	"github.com/pug-sh/pug/internal/testutil"
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
			Spec: &insightsv1.InsightQuerySpec{
				InsightType: insightsv1.InsightType_INSIGHT_TYPE_TRENDS.Enum(),
				Events: []*insightsv1.EventQuery{
					{Event: &commonv1.EventFilter{Kind: proto.String("page_view")}, Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL.Enum()},
				},
			},
			TimeRange: &commonv1.TimeRange{
				From: timestamppb.New(time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)),
				To:   timestamppb.New(time.Date(2024, 1, 4, 0, 0, 0, 0, time.UTC)),
			},
			Granularity: insightsv1.Granularity_GRANULARITY_DAY.Enum(),
		}

		q, err := insights.BuildTrendsQuery(req, testProjectID)
		if err != nil {
			t.Fatalf("BuildTrendsQuery: %v", err)
		}

		rows, err := executor.QueryTrends(ctx, testProjectID, q)
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
			Spec: &insightsv1.InsightQuerySpec{
				InsightType: insightsv1.InsightType_INSIGHT_TYPE_TRENDS.Enum(),
				Events: []*insightsv1.EventQuery{
					{Event: &commonv1.EventFilter{Kind: proto.String("page_view")}, Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_UNIQUE_USERS.Enum()},
				},
			},
			TimeRange: &commonv1.TimeRange{
				From: timestamppb.New(time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)),
				To:   timestamppb.New(time.Date(2024, 1, 4, 0, 0, 0, 0, time.UTC)),
			},
			Granularity: insightsv1.Granularity_GRANULARITY_DAY.Enum(),
		}

		q, err := insights.BuildTrendsQuery(req, testProjectID)
		if err != nil {
			t.Fatalf("BuildTrendsQuery: %v", err)
		}

		rows, err := executor.QueryTrends(ctx, testProjectID, q)
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
			Spec: &insightsv1.InsightQuerySpec{
				InsightType: insightsv1.InsightType_INSIGHT_TYPE_TRENDS.Enum(),
				Events: []*insightsv1.EventQuery{
					{Event: &commonv1.EventFilter{Kind: proto.String("page_view")}, Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL.Enum()},
				},
				Breakdowns:     []*insightsv1.Breakdown{{Property: proto.String("$country")}},
				BreakdownLimit: proto.Int32(10),
			},
			TimeRange: &commonv1.TimeRange{
				From: timestamppb.New(time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)),
				To:   timestamppb.New(time.Date(2024, 1, 4, 0, 0, 0, 0, time.UTC)),
			},
			Granularity: insightsv1.Granularity_GRANULARITY_DAY.Enum(),
		}

		q, err := insights.BuildTrendsQuery(req, testProjectID)
		if err != nil {
			t.Fatalf("BuildTrendsQuery: %v", err)
		}

		rows, err := executor.QueryTrends(ctx, testProjectID, q)
		if err != nil {
			t.Fatalf("QueryTrends: %v", err)
		}

		series, err := insights.GroupSeries(ctx, rows, q.Properties(), q.BreakdownLimit())
		if err != nil {
			t.Fatalf("GroupSeries: %v", err)
		}
		if len(series) < 2 {
			t.Fatalf("expected at least 2 breakdown series (US, GB), got %d", len(series))
		}
	})

	t.Run("segmentation", func(t *testing.T) {
		req := &insightsv1.QueryRequest{
			Spec: &insightsv1.InsightQuerySpec{
				InsightType: insightsv1.InsightType_INSIGHT_TYPE_SEGMENTATION.Enum(),
				Events: []*insightsv1.EventQuery{
					{Event: &commonv1.EventFilter{Kind: proto.String("page_view")}, Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL.Enum()},
				},
			},
			TimeRange: &commonv1.TimeRange{
				From: timestamppb.New(time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)),
				To:   timestamppb.New(time.Date(2024, 1, 4, 0, 0, 0, 0, time.UTC)),
			},
		}

		q, err := insights.BuildSegmentationQuery(req, testProjectID)
		if err != nil {
			t.Fatalf("BuildSegmentationQuery: %v", err)
		}

		value, err := executor.QueryScalar(ctx, testProjectID, q)
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
			Spec: &insightsv1.InsightQuerySpec{
				InsightType: insightsv1.InsightType_INSIGHT_TYPE_SEGMENTATION.Enum(),
				Events: []*insightsv1.EventQuery{
					{Event: &commonv1.EventFilter{Kind: proto.String("page_view")}, Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL.Enum()},
				},
				FilterGroups: []*insightsv1.FilterGroup{
					{
						Operator: commonv1.LogicalOperator_LOGICAL_OPERATOR_AND.Enum(),
						Filters: []*commonv1.PropertyFilter{
							{Property: proto.String("$country"), Operator: commonv1.FilterOperator_FILTER_OPERATOR_EQUALS.Enum(), Value: proto.String("US")},
						},
					},
				},
			},
			TimeRange: &commonv1.TimeRange{
				From: timestamppb.New(time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)),
				To:   timestamppb.New(time.Date(2024, 1, 4, 0, 0, 0, 0, time.UTC)),
			},
		}

		q, err := insights.BuildSegmentationQuery(req, testProjectID)
		if err != nil {
			t.Fatalf("BuildSegmentationQuery: %v", err)
		}

		value, err := executor.QueryScalar(ctx, testProjectID, q)
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
			Spec: &insightsv1.InsightQuerySpec{
				InsightType: insightsv1.InsightType_INSIGHT_TYPE_TRENDS.Enum(),
				Events: []*insightsv1.EventQuery{
					{Event: &commonv1.EventFilter{Kind: proto.String("page_view")}, Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_PER_USER_AVG.Enum()},
				},
			},
			TimeRange: &commonv1.TimeRange{
				From: timestamppb.New(time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)),
				To:   timestamppb.New(time.Date(2024, 1, 4, 0, 0, 0, 0, time.UTC)),
			},
			Granularity: insightsv1.Granularity_GRANULARITY_DAY.Enum(),
		}

		q, err := insights.BuildTrendsQuery(req, testProjectID)
		if err != nil {
			t.Fatalf("BuildTrendsQuery: %v", err)
		}

		rows, err := executor.QueryTrends(ctx, testProjectID, q)
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
			{Event: &commonv1.EventFilter{Kind: proto.String("page_view")}, Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL.Enum()},
		}

		// First page: size 2
		req1 := &insightsv1.SegmentUsersRequest{
			TimeRange: tr, Events: events, PageSize: proto.Int32(2),
		}
		sql1, args1, err := insights.BuildSegmentUsersQuery(req1, testProjectID)
		if err != nil {
			t.Fatalf("BuildSegmentUsersQuery page1: %v", err)
		}
		page1, err := executor.QueryStringColumn(ctx, testProjectID, sql1, args1)
		if err != nil {
			t.Fatalf("QueryStringColumn page1: %v", err)
		}
		if len(page1) != 2 {
			t.Fatalf("expected 2 IDs on page 1, got %d: %v", len(page1), page1)
		}

		// Second page: cursor from last ID of page 1
		req2 := &insightsv1.SegmentUsersRequest{
			TimeRange: tr, Events: events, PageSize: proto.Int32(2), PageToken: proto.String(page1[len(page1)-1]),
		}
		sql2, args2, err := insights.BuildSegmentUsersQuery(req2, testProjectID)
		if err != nil {
			t.Fatalf("BuildSegmentUsersQuery page2: %v", err)
		}
		page2, err := executor.QueryStringColumn(ctx, testProjectID, sql2, args2)
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
			Spec: &insightsv1.InsightQuerySpec{
				InsightType: insightsv1.InsightType_INSIGHT_TYPE_TRENDS.Enum(),
				Events: []*insightsv1.EventQuery{
					{Event: &commonv1.EventFilter{Kind: proto.String("purchase")}, Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL.Enum()},
				},
				Breakdowns:     []*insightsv1.Breakdown{{Property: proto.String("$country")}},
				BreakdownLimit: proto.Int32(2), // Only top 2 countries, rest go to $others
			},
			TimeRange: &commonv1.TimeRange{
				From: timestamppb.New(time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)),
				To:   timestamppb.New(time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC)),
			},
			Granularity: insightsv1.Granularity_GRANULARITY_DAY.Enum(),
		}

		q, err := insights.BuildTrendsQuery(req, testProjectID)
		if err != nil {
			t.Fatalf("BuildTrendsQuery: %v", err)
		}

		rows, err := executor.QueryTrends(ctx, testProjectID, q)
		if err != nil {
			t.Fatalf("QueryTrends: %v", err)
		}

		series, err := insights.GroupSeries(ctx, rows, q.Properties(), q.BreakdownLimit())
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
			Spec: &insightsv1.InsightQuerySpec{
				InsightType: insightsv1.InsightType_INSIGHT_TYPE_TRENDS.Enum(),
				Events: []*insightsv1.EventQuery{
					{Event: &commonv1.EventFilter{Kind: proto.String("page_view")}, Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL.Enum()},
					{Event: &commonv1.EventFilter{Kind: proto.String("purchase")}, Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL.Enum()},
				},
			},
			TimeRange: &commonv1.TimeRange{
				From: timestamppb.New(time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)),
				To:   timestamppb.New(time.Date(2024, 1, 4, 0, 0, 0, 0, time.UTC)),
			},
			Granularity: insightsv1.Granularity_GRANULARITY_DAY.Enum(),
		}

		q, err := insights.BuildTrendsQuery(req, testProjectID)
		if err != nil {
			t.Fatalf("BuildTrendsQuery: %v", err)
		}

		rows, err := executor.QueryTrends(ctx, testProjectID, q)
		if err != nil {
			t.Fatalf("QueryTrends: %v", err)
		}

		series, err := insights.GroupSeries(ctx, rows, q.Properties(), q.BreakdownLimit())
		if err != nil {
			t.Fatalf("GroupSeries: %v", err)
		}
		if len(series) != 2 {
			t.Fatalf("expected 2 series (page_view, purchase), got %d", len(series))
		}

		kindSet := map[string]bool{}
		for _, s := range series {
			kindSet[s.GetEventKind()] = true
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
				{Event: &commonv1.EventFilter{Kind: proto.String("page_view")}, Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL.Enum()},
			},
			PageSize: proto.Int32(100),
		}

		sql, args, err := insights.BuildSegmentUsersQuery(req, testProjectID)
		if err != nil {
			t.Fatalf("BuildSegmentUsersQuery: %v", err)
		}

		ids, err := executor.QueryStringColumn(ctx, testProjectID, sql, args)
		if err != nil {
			t.Fatalf("QueryStringColumn: %v", err)
		}

		// 3 distinct users: alice, bob, charlie
		if len(ids) != 3 {
			t.Errorf("expected 3 distinct users, got %d: %v", len(ids), ids)
		}
	})

	t.Run("trends_with_profile_filter", func(t *testing.T) {
		// alice(pro): 3 page_views; bob(free): 2; charlie(no profile): 1.
		// Plus 1 event from alias "alice_anon" which maps to alice (plan=pro).
		// Filter plan=pro → alice's 3 events + 1 alias event = 4.
		seedIntegrationProfiles(t, ctx, ch)

		// Insert an event with an alias distinct_id to exercise the UNION ALL alias branch.
		if err := insertAutoEvent(ctx, ch.Conn,
			testProjectID, uuid.New().String(), "page_view", "alice_anon",
			time.Date(2024, 1, 1, 14, 0, 0, 0, time.UTC),
			variantStringMap(map[string]string{"$country": "US"}),
		); err != nil {
			t.Fatalf("insert alias event: %v", err)
		}

		// Insert an event for the soft-deleted profile to verify is_deleted=0 guard.
		// This event must NOT be included in the profile filter results.
		if err := insertAutoEvent(ctx, ch.Conn,
			testProjectID, uuid.New().String(), "page_view", "deleted",
			time.Date(2024, 1, 1, 15, 0, 0, 0, time.UTC),
			variantStringMap(map[string]string{"$country": "US"}),
		); err != nil {
			t.Fatalf("insert deleted-profile event: %v", err)
		}

		req := &insightsv1.QueryRequest{
			Spec: &insightsv1.InsightQuerySpec{
				InsightType: insightsv1.InsightType_INSIGHT_TYPE_TRENDS.Enum(),
				Events: []*insightsv1.EventQuery{
					{Event: &commonv1.EventFilter{Kind: proto.String("page_view")}, Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL.Enum()},
				},
				FilterGroups: []*insightsv1.FilterGroup{
					{
						Filters: []*commonv1.PropertyFilter{
							{
								Property: proto.String("plan"),
								Operator: commonv1.FilterOperator_FILTER_OPERATOR_EQUALS.Enum(),
								Value:    proto.String("pro"),
								Source:   commonv1.PropertySource_PROPERTY_SOURCE_PROFILE.Enum(),
							},
						},
					},
				},
			},
			TimeRange: &commonv1.TimeRange{
				From: timestamppb.New(time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)),
				To:   timestamppb.New(time.Date(2024, 1, 4, 0, 0, 0, 0, time.UTC)),
			},
			Granularity: insightsv1.Granularity_GRANULARITY_DAY.Enum(),
		}

		q, err := insights.BuildTrendsQuery(req, testProjectID)
		if err != nil {
			t.Fatalf("BuildTrendsQuery: %v", err)
		}

		rows, err := executor.QueryTrends(ctx, testProjectID, q)
		if err != nil {
			t.Fatalf("QueryTrends: %v", err)
		}

		var total float64
		for _, r := range rows {
			total += r.Value
		}
		// alice has 3 page_views + 1 via alias "alice_anon"; bob and charlie must be excluded.
		if total != 4 {
			t.Errorf("expected 4 events for plan=pro users, got %.0f (rows: %v)", total, rows)
		}
	})

	t.Run("funnel_counts", func(t *testing.T) {
		seedFunnelEvents(t, ctx, ch)

		req := &insightsv1.QueryRequest{
			Spec: &insightsv1.InsightQuerySpec{
				InsightType: insightsv1.InsightType_INSIGHT_TYPE_FUNNEL.Enum(),
				Events: []*insightsv1.EventQuery{
					{Event: &commonv1.EventFilter{Kind: proto.String("sign_up")}, Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL.Enum()},
					{Event: &commonv1.EventFilter{Kind: proto.String("add_to_cart")}, Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL.Enum()},
					{Event: &commonv1.EventFilter{Kind: proto.String("purchase")}, Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL.Enum()},
				},
			},
			TimeRange: &commonv1.TimeRange{
				From: timestamppb.New(time.Date(2024, 2, 1, 0, 0, 0, 0, time.UTC)),
				To:   timestamppb.New(time.Date(2024, 2, 8, 0, 0, 0, 0, time.UTC)),
			},
		}

		q, err := insights.BuildFunnelCountsQuery(req, testProjectID)
		if err != nil {
			t.Fatalf("BuildFunnelCountsQuery: %v", err)
		}

		rows, err := executor.QueryFunnel(ctx, testProjectID, q)
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
		seedFunnelEvents(t, ctx, ch)

		req := &insightsv1.QueryRequest{
			Spec: &insightsv1.InsightQuerySpec{
				InsightType:       insightsv1.InsightType_INSIGHT_TYPE_FUNNEL.Enum(),
				IncludeStepTiming: proto.Bool(true),
				Events: []*insightsv1.EventQuery{
					{Event: &commonv1.EventFilter{Kind: proto.String("sign_up")}, Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL.Enum()},
					{Event: &commonv1.EventFilter{Kind: proto.String("add_to_cart")}, Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL.Enum()},
					{Event: &commonv1.EventFilter{Kind: proto.String("purchase")}, Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL.Enum()},
				},
			},
			TimeRange: &commonv1.TimeRange{
				From: timestamppb.New(time.Date(2024, 2, 1, 0, 0, 0, 0, time.UTC)),
				To:   timestamppb.New(time.Date(2024, 2, 8, 0, 0, 0, 0, time.UTC)),
			},
		}

		countsQ, err := insights.BuildFunnelCountsQuery(req, testProjectID)
		if err != nil {
			t.Fatalf("BuildFunnelCountsQuery: %v", err)
		}
		countRows, err := executor.QueryFunnel(ctx, testProjectID, countsQ)
		if err != nil {
			t.Fatalf("QueryFunnel: %v", err)
		}

		timingQ, err := insights.BuildFunnelTimingQuery(req, testProjectID)
		if err != nil {
			t.Fatalf("BuildFunnelTimingQuery: %v", err)
		}

		users, err := executor.QueryFunnelUserEvents(ctx, testProjectID, timingQ)
		if err != nil {
			t.Fatalf("QueryFunnelUserEvents: %v", err)
		}

		timingRows, err := insights.ComputeFunnelTiming(ctx, "", users, timingQ.Kinds(), timingQ.WindowSec(), timingQ.NumBreakdowns())
		if err != nil {
			t.Fatalf("ComputeFunnelTiming: %v", err)
		}

		rows := insights.MergeFunnelCountsAndTiming(countRows, timingRows)

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
		// Step 0 has no timing (entry step) — Timing must be nil.
		if rows[0].Timing != nil {
			t.Errorf("step 0 timing should be nil, got %+v", rows[0].Timing)
		}
		// Step 1 has 2 converters; Timing must be populated with positive avg/median/p95 and 8-bucket distribution.
		if rows[1].Timing == nil {
			t.Fatal("step 1 timing should be non-nil")
		}
		if rows[1].Timing.Avg <= 0 {
			t.Errorf("step 1 avg time should be > 0, got %v", rows[1].Timing.Avg)
		}
		if rows[1].Timing.Median <= 0 {
			t.Errorf("step 1 median should be > 0, got %v", rows[1].Timing.Median)
		}
		if rows[1].Timing.P95 < rows[1].Timing.Median {
			t.Errorf("step 1 p95 (%v) should be >= median (%v)", rows[1].Timing.P95, rows[1].Timing.Median)
		}
		if len(rows[1].Timing.Distribution) != 8 {
			t.Errorf("step 1 distribution length: got %d, want 8", len(rows[1].Timing.Distribution))
		}
		var total int64
		for _, c := range rows[1].Timing.Distribution {
			total += c
		}
		if total != 2 {
			t.Errorf("step 1 distribution bucket sum: got %d, want 2 (matches converter count)", total)
		}
	})

	t.Run("retention", func(t *testing.T) {
		seedRetentionEvents(t, ctx, ch)

		req := &insightsv1.QueryRequest{
			Spec: &insightsv1.InsightQuerySpec{
				InsightType: insightsv1.InsightType_INSIGHT_TYPE_RETENTION.Enum(),
				Events: []*insightsv1.EventQuery{
					{Event: &commonv1.EventFilter{Kind: proto.String("sign_up")}, Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL.Enum()},
					{Event: &commonv1.EventFilter{Kind: proto.String("login")}, Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL.Enum()},
				},
			},
			TimeRange: &commonv1.TimeRange{
				From: timestamppb.New(time.Date(2024, 3, 1, 0, 0, 0, 0, time.UTC)),
				To:   timestamppb.New(time.Date(2024, 3, 8, 0, 0, 0, 0, time.UTC)),
			},
			Granularity: insightsv1.Granularity_GRANULARITY_DAY.Enum(),
		}

		q, err := insights.BuildRetentionQuery(req, testProjectID)
		if err != nil {
			t.Fatalf("BuildRetentionQuery: %v", err)
		}

		rows, err := executor.QueryRetention(ctx, testProjectID, q)
		if err != nil {
			t.Fatalf("QueryRetention: %v", err)
		}

		series, err := insights.GroupRetentionSeries(ctx, rows, nil, 0)
		if err != nil {
			t.Fatalf("GroupRetentionSeries: %v", err)
		}
		// At least 1 series with 1 cohort (Mar 1 sign_ups).
		if len(series) == 0 || len(series[0].Cohorts) == 0 {
			t.Fatal("expected at least 1 retention cohort")
		}
		cohorts := series[0].Cohorts
		// First cohort should have day-0 retention = 100%.
		if cohorts[0].Points[0].GetValue() != 100 {
			t.Errorf("expected 100%% day-0 retention, got %v", cohorts[0].Points[0].GetValue())
		}
		// Cohort size should match seeded sign_ups.
		if cohorts[0].GetCohortSize() != 3 {
			t.Errorf("expected cohort size 3, got %v", cohorts[0].GetCohortSize())
		}
	})

	t.Run("funnel_counts_breakdown_others_bucket", func(t *testing.T) {
		// Reuses seedFunnelEventsWithCountry data (Apr 2024):
		//   US: alice sign_up+purchase, bob sign_up = 3 step-matching events
		//   GB: charlie sign_up+purchase = 2 step-matching events
		// BreakdownLimit=1: US stays (most events), GB → $others.
		seedFunnelEventsWithCountry(t, ctx, ch)

		req := &insightsv1.QueryRequest{
			Spec: &insightsv1.InsightQuerySpec{
				InsightType: insightsv1.InsightType_INSIGHT_TYPE_FUNNEL.Enum(),
				Events: []*insightsv1.EventQuery{
					{Event: &commonv1.EventFilter{Kind: proto.String("sign_up")}},
					{Event: &commonv1.EventFilter{Kind: proto.String("purchase")}},
				},
				Breakdowns:     []*insightsv1.Breakdown{{Property: proto.String("$country")}},
				BreakdownLimit: proto.Int32(1),
			},
			TimeRange: &commonv1.TimeRange{
				From: timestamppb.New(time.Date(2024, 4, 1, 0, 0, 0, 0, time.UTC)),
				To:   timestamppb.New(time.Date(2024, 4, 8, 0, 0, 0, 0, time.UTC)),
			},
		}

		q, err := insights.BuildFunnelCountsQuery(req, testProjectID)
		if err != nil {
			t.Fatalf("BuildFunnelCountsQuery: %v", err)
		}

		rows, err := executor.QueryFunnel(ctx, testProjectID, q)
		if err != nil {
			t.Fatalf("QueryFunnel: %v", err)
		}

		series, err := insights.GroupFunnelSeries(ctx, rows, q.Properties(), q.BreakdownLimit())
		if err != nil {
			t.Fatalf("GroupFunnelSeries: %v", err)
		}

		hasOthers := false
		for _, s := range series {
			if s.Breakdown["$country"] == "$others" {
				hasOthers = true
			}
		}
		if !hasOthers {
			t.Error("expected $others bucket in funnel breakdown series")
		}
	})

	t.Run("retention_breakdown_others_bucket", func(t *testing.T) {
		// Uses seedRetentionEventsForOthersBucket (June 2024):
		//   US: alice+bob sign_up = 2 start events → top 1
		//   GB: charlie sign_up = 1 start event → $others
		seedRetentionEventsForOthersBucket(t, ctx, ch)

		req := &insightsv1.QueryRequest{
			Spec: &insightsv1.InsightQuerySpec{
				InsightType: insightsv1.InsightType_INSIGHT_TYPE_RETENTION.Enum(),
				Events: []*insightsv1.EventQuery{
					{Event: &commonv1.EventFilter{Kind: proto.String("sign_up")}},
					{Event: &commonv1.EventFilter{Kind: proto.String("login")}},
				},
				Breakdowns:     []*insightsv1.Breakdown{{Property: proto.String("$country")}},
				BreakdownLimit: proto.Int32(1),
			},
			TimeRange: &commonv1.TimeRange{
				From: timestamppb.New(time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC)),
				To:   timestamppb.New(time.Date(2024, 6, 8, 0, 0, 0, 0, time.UTC)),
			},
			Granularity: insightsv1.Granularity_GRANULARITY_DAY.Enum(),
		}

		q, err := insights.BuildRetentionQuery(req, testProjectID)
		if err != nil {
			t.Fatalf("BuildRetentionQuery: %v", err)
		}

		rows, err := executor.QueryRetention(ctx, testProjectID, q)
		if err != nil {
			t.Fatalf("QueryRetention: %v", err)
		}

		series, err := insights.GroupRetentionSeries(ctx, rows, q.Properties(), q.BreakdownLimit())
		if err != nil {
			t.Fatalf("GroupRetentionSeries: %v", err)
		}

		hasOthers := false
		for _, s := range series {
			if s.Breakdown["$country"] == "$others" {
				hasOthers = true
			}
		}
		if !hasOthers {
			t.Error("expected $others bucket in retention breakdown series")
		}
	})

	t.Run("funnel_counts_with_breakdown", func(t *testing.T) {
		seedFunnelEventsWithCountry(t, ctx, ch)

		req := &insightsv1.QueryRequest{
			Spec: &insightsv1.InsightQuerySpec{
				InsightType: insightsv1.InsightType_INSIGHT_TYPE_FUNNEL.Enum(),
				Events: []*insightsv1.EventQuery{
					{Event: &commonv1.EventFilter{Kind: proto.String("sign_up")}},
					{Event: &commonv1.EventFilter{Kind: proto.String("purchase")}},
				},
				Breakdowns:     []*insightsv1.Breakdown{{Property: proto.String("$country")}},
				BreakdownLimit: proto.Int32(10),
			},
			TimeRange: &commonv1.TimeRange{
				From: timestamppb.New(time.Date(2024, 4, 1, 0, 0, 0, 0, time.UTC)),
				To:   timestamppb.New(time.Date(2024, 4, 8, 0, 0, 0, 0, time.UTC)),
			},
		}

		q, err := insights.BuildFunnelCountsQuery(req, testProjectID)
		if err != nil {
			t.Fatalf("BuildFunnelCountsQuery: %v", err)
		}

		rows, err := executor.QueryFunnel(ctx, testProjectID, q)
		if err != nil {
			t.Fatalf("QueryFunnel: %v", err)
		}

		series, err := insights.GroupFunnelSeries(ctx, rows, q.Properties(), q.BreakdownLimit())
		if err != nil {
			t.Fatalf("GroupFunnelSeries: %v", err)
		}
		if len(series) < 2 {
			t.Fatalf("expected at least 2 breakdown series, got %d", len(series))
		}

		// Seed data: alice(US) sign_up+purchase, bob(US) sign_up only, charlie(GB) sign_up+purchase.
		// US: step 0 = 2, step 1 = 1. GB: step 0 = 1, step 1 = 1.
		byCountry := map[string]*insightsv1.FunnelSeries{}
		for _, s := range series {
			byCountry[s.Breakdown["$country"]] = s
		}
		us, gb := byCountry["US"], byCountry["GB"]
		if us == nil || gb == nil {
			t.Fatalf("expected US and GB series, got keys: %v", byCountry)
		}
		if len(us.Steps) < 2 || us.Steps[0].GetTotal() != 2 || us.Steps[1].GetTotal() != 1 {
			t.Errorf("US steps: got %+v, want [2, 1]", us.Steps)
		}
		if len(gb.Steps) < 2 || gb.Steps[0].GetTotal() != 1 || gb.Steps[1].GetTotal() != 1 {
			t.Errorf("GB steps: got %+v, want [1, 1]", gb.Steps)
		}
	})

	t.Run("funnel_timing_with_breakdown", func(t *testing.T) {
		seedFunnelEventsWithCountry(t, ctx, ch)

		req := &insightsv1.QueryRequest{
			Spec: &insightsv1.InsightQuerySpec{
				InsightType:       insightsv1.InsightType_INSIGHT_TYPE_FUNNEL.Enum(),
				IncludeStepTiming: proto.Bool(true),
				Events: []*insightsv1.EventQuery{
					{Event: &commonv1.EventFilter{Kind: proto.String("sign_up")}},
					{Event: &commonv1.EventFilter{Kind: proto.String("purchase")}},
				},
				Breakdowns:     []*insightsv1.Breakdown{{Property: proto.String("$country")}},
				BreakdownLimit: proto.Int32(10),
			},
			TimeRange: &commonv1.TimeRange{
				From: timestamppb.New(time.Date(2024, 4, 1, 0, 0, 0, 0, time.UTC)),
				To:   timestamppb.New(time.Date(2024, 4, 8, 0, 0, 0, 0, time.UTC)),
			},
		}

		countsQ, err := insights.BuildFunnelCountsQuery(req, testProjectID)
		if err != nil {
			t.Fatalf("BuildFunnelCountsQuery: %v", err)
		}
		countRows, err := executor.QueryFunnel(ctx, testProjectID, countsQ)
		if err != nil {
			t.Fatalf("QueryFunnel: %v", err)
		}

		timingQ, err := insights.BuildFunnelTimingQuery(req, testProjectID)
		if err != nil {
			t.Fatalf("BuildFunnelTimingQuery: %v", err)
		}

		users, err := executor.QueryFunnelUserEvents(ctx, testProjectID, timingQ)
		if err != nil {
			t.Fatalf("QueryFunnelUserEvents: %v", err)
		}

		timingRows, err := insights.ComputeFunnelTiming(ctx, "", users, timingQ.Kinds(), timingQ.WindowSec(), timingQ.NumBreakdowns())
		if err != nil {
			t.Fatalf("ComputeFunnelTiming: %v", err)
		}

		funnelRows := insights.MergeFunnelCountsAndTiming(countRows, timingRows)

		series, err := insights.GroupFunnelSeries(ctx, funnelRows, countsQ.Properties(), countsQ.BreakdownLimit())
		if err != nil {
			t.Fatalf("GroupFunnelSeries: %v", err)
		}
		if len(series) < 2 {
			t.Fatalf("expected at least 2 breakdown series, got %d", len(series))
		}
		// Each series should have 2 proto steps; step 1 with converters should populate Timing with an 8-bucket distribution.
		for _, s := range series {
			if len(s.Steps) != 2 {
				t.Fatalf("series %v: expected 2 steps, got %d", s.Breakdown, len(s.Steps))
			}
			if s.Steps[1].GetTotal() > 0 {
				timing := s.Steps[1].GetTiming()
				if timing == nil {
					t.Errorf("series %v step 1: expected non-nil Timing, got nil", s.Breakdown)
					continue
				}
				if len(timing.GetDistribution()) != 8 {
					t.Errorf("series %v step 1: expected 8 distribution buckets, got %d",
						s.Breakdown, len(timing.GetDistribution()))
				}
				// Open-ended last bucket must omit upper_bound.
				if last := timing.GetDistribution()[7]; last.UpperBound != nil {
					t.Errorf("series %v last bucket: upper_bound should be absent, got %v",
						s.Breakdown, last.GetUpperBound().AsDuration())
				}
			}
		}
	})

	t.Run("user_flow", func(t *testing.T) {
		const userFlowProjectID = "proj_user_flow"
		seedUserFlowEvents(t, ctx, ch, userFlowProjectID)

		window := &commonv1.TimeRange{
			From: timestamppb.New(time.Date(2024, 3, 1, 0, 0, 0, 0, time.UTC)),
			To:   timestamppb.New(time.Date(2024, 3, 2, 0, 0, 0, 0, time.UTC)),
		}
		userFlowReq := func(uf *insightsv1.UserFlowQuery) *insightsv1.QueryRequest {
			return &insightsv1.QueryRequest{
				Spec: &insightsv1.InsightQuerySpec{
					InsightType: insightsv1.InsightType_INSIGHT_TYPE_USER_FLOW.Enum(),
					UserFlow:    uf,
				},
				TimeRange:   window,
				Granularity: insightsv1.Granularity_GRANULARITY_DAY.Enum(),
			}
		}

		t.Run("event_kind_default", func(t *testing.T) {
			resp, err := insights.ExecuteQuery(ctx, executor, userFlowProjectID, userFlowReq(&insightsv1.UserFlowQuery{}), time.Now())
			if err != nil {
				t.Fatalf("ExecuteQuery: %v", err)
			}
			result := resp.GetUserFlow()
			if result == nil {
				t.Fatal("expected UserFlow result")
			}
			linkMap := userFlowLinkMap(result)
			want := map[[2]string]int64{
				{"login", "dashboard"}:    2,
				{"login", "logout"}:       1,
				{"dashboard", "settings"}: 1,
				{"dashboard", "logout"}:   1,
				{"settings", "logout"}:    1,
			}
			for k, v := range want {
				if linkMap[k] != v {
					t.Errorf("link %v: got %d want %d", k, linkMap[k], v)
				}
			}
		})

		t.Run("scope_restricts_eligible_events", func(t *testing.T) {
			const scopeProjectID = "proj_user_flow_scope"
			seedUserFlowEventsWithNoise(t, ctx, ch, scopeProjectID)

			// Baseline (no scope): the seeded data DOES produce transitions, including
			// the heartbeat-noise edges in session A. Asserting this first proves the
			// scoped 0-link result below is the scope filtering events down to one per
			// session — not simply an empty dataset (the gap this test used to have).
			baseline, err := insights.ExecuteQuery(ctx, executor, scopeProjectID,
				userFlowReq(&insightsv1.UserFlowQuery{}), time.Now())
			if err != nil {
				t.Fatalf("ExecuteQuery baseline: %v", err)
			}
			baseMap := userFlowLinkMap(baseline.GetUserFlow())
			if baseMap[[2]string{"login", "heartbeat"}] == 0 || baseMap[[2]string{"heartbeat", "dashboard"}] == 0 {
				t.Fatalf("baseline should include the heartbeat transitions, got %v", baseMap)
			}

			// Scope to kind=login: each session has exactly one login, so the scoped
			// flow collapses to single-node sessions and emits no transitions.
			resp, err := insights.ExecuteQuery(ctx, executor, scopeProjectID, userFlowReq(&insightsv1.UserFlowQuery{
				Scope: &commonv1.EventFilter{Kind: proto.String("login")},
			}), time.Now())
			if err != nil {
				t.Fatalf("ExecuteQuery: %v", err)
			}
			if links := resp.GetUserFlow().GetLinks(); len(links) != 0 {
				t.Fatalf("scope kind=login should yield no transitions (single-node sessions), got %d links", len(links))
			}
		})

		t.Run("property_url_nodes", func(t *testing.T) {
			const urlProjectID = "proj_user_flow_url"
			seedUserFlowURLEvents(t, ctx, ch, urlProjectID)

			resp, err := insights.ExecuteQuery(ctx, executor, urlProjectID, userFlowReq(&insightsv1.UserFlowQuery{
				NodeKind:     insightsv1.UserFlowQuery_NODE_KIND_PROPERTY.Enum(),
				NodeProperty: proto.String("$url"),
				Scope:        &commonv1.EventFilter{Kind: proto.String("page_view")},
			}), time.Now())
			if err != nil {
				t.Fatalf("ExecuteQuery: %v", err)
			}
			linkMap := userFlowLinkMap(resp.GetUserFlow())
			want := map[[2]string]int64{
				{"/login", "/dashboard"}:    2,
				{"/login", "/logout"}:       1,
				{"/dashboard", "/settings"}: 1,
				{"/dashboard", "/logout"}:   1,
				{"/settings", "/logout"}:    1,
			}
			for k, v := range want {
				if linkMap[k] != v {
					t.Errorf("link %v: got %d want %d", k, linkMap[k], v)
				}
			}
		})

		t.Run("max_hops_truncates_paths", func(t *testing.T) {
			resp, err := insights.ExecuteQuery(ctx, executor, userFlowProjectID, userFlowReq(&insightsv1.UserFlowQuery{
				MaxHops: proto.Int32(2),
			}), time.Now())
			if err != nil {
				t.Fatalf("ExecuteQuery: %v", err)
			}
			linkMap := userFlowLinkMap(resp.GetUserFlow())
			if v := linkMap[[2]string{"settings", "logout"}]; v != 0 {
				t.Errorf("settings->logout should be truncated by max_hops=2, got %d", v)
			}
			if linkMap[[2]string{"login", "dashboard"}] != 2 {
				t.Errorf("login->dashboard: got %d want 2", linkMap[[2]string{"login", "dashboard"}])
			}
		})

		t.Run("others_bucket_collapses_pruned_nodes", func(t *testing.T) {
			const fanProjectID = "proj_user_flow_fan"
			// Four sessions each start at "hub" then branch to a distinct leaf.
			seedUserFlowKindSequences(t, ctx, ch, fanProjectID, map[string][]string{
				"A": {"hub", "a"},
				"B": {"hub", "b"},
				"C": {"hub", "c"},
				"D": {"hub", "d"},
			})
			// max_nodes=2 keeps {hub, a} (a is the lexicographically-first weight-1
			// leaf); b, c, d collapse into the synthetic $others bucket.
			resp, err := insights.ExecuteQuery(ctx, executor, fanProjectID, userFlowReq(&insightsv1.UserFlowQuery{
				MaxNodes: proto.Int32(2),
			}), time.Now())
			if err != nil {
				t.Fatalf("ExecuteQuery: %v", err)
			}
			result := resp.GetUserFlow()
			linkMap := userFlowLinkMap(result)
			if got := linkMap[[2]string{"hub", "a"}]; got != 1 {
				t.Errorf("hub->a: got %d want 1", got)
			}
			if got := linkMap[[2]string{"hub", "$others"}]; got != 3 {
				t.Errorf("hub->$others: got %d want 3 (b+c+d collapsed and summed)", got)
			}
			bucket := false
			nodeIDs := map[string]bool{}
			for _, n := range result.GetNodes() {
				nodeIDs[n.GetId()] = true
				if n.GetIsOthers() {
					bucket = true
					if n.GetId() != "$others" {
						t.Errorf("bucket id: got %q want $others", n.GetId())
					}
				}
			}
			if !bucket {
				t.Error("expected an is_others bucket node")
			}
			for _, leaf := range []string{"b", "c", "d"} {
				if nodeIDs[leaf] {
					t.Errorf("pruned leaf %q should not appear as a real node", leaf)
				}
			}
		})

		t.Run("nodes_ordered_by_time_not_insertion", func(t *testing.T) {
			const orderProjectID = "proj_user_flow_order"
			base := time.Date(2024, 3, 1, 10, 0, 0, 0, time.UTC)
			sid := "00000000-0000-0000-0000-0000000000e5"
			// Insert in REVERSE time order: c (latest), a (earliest), b (middle).
			// arraySort must order the session's nodes by occur_time → a→b→c, not
			// the c→a→b that insertion/scan order would otherwise produce.
			steps := []struct {
				kind   string
				offset time.Duration
			}{
				{"c", 2 * time.Minute},
				{"a", 0},
				{"b", 1 * time.Minute},
			}
			for i, s := range steps {
				if err := insertSessionEvent(ctx, ch.Conn, orderProjectID, uuid.New().String(),
					s.kind, "user_order", sid, base.Add(s.offset), "", ""); err != nil {
					t.Fatalf("seed event %d: %v", i, err)
				}
			}
			resp, err := insights.ExecuteQuery(ctx, executor, orderProjectID, userFlowReq(&insightsv1.UserFlowQuery{}), time.Now())
			if err != nil {
				t.Fatalf("ExecuteQuery: %v", err)
			}
			linkMap := userFlowLinkMap(resp.GetUserFlow())
			if linkMap[[2]string{"a", "b"}] != 1 || linkMap[[2]string{"b", "c"}] != 1 {
				t.Errorf("expected time-ordered a->b->c, got %v", linkMap)
			}
			if got := linkMap[[2]string{"c", "a"}]; got != 0 {
				t.Errorf("c->a must not exist (would mean insertion order, not time order), got %d", got)
			}
		})
	})

	t.Run("retention_with_breakdown", func(t *testing.T) {
		seedRetentionEventsWithCountry(t, ctx, ch)

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
			TimeRange: &commonv1.TimeRange{
				From: timestamppb.New(time.Date(2024, 5, 1, 0, 0, 0, 0, time.UTC)),
				To:   timestamppb.New(time.Date(2024, 5, 8, 0, 0, 0, 0, time.UTC)),
			},
			Granularity: insightsv1.Granularity_GRANULARITY_DAY.Enum(),
		}

		q, err := insights.BuildRetentionQuery(req, testProjectID)
		if err != nil {
			t.Fatalf("BuildRetentionQuery: %v", err)
		}

		rows, err := executor.QueryRetention(ctx, testProjectID, q)
		if err != nil {
			t.Fatalf("QueryRetention: %v", err)
		}

		series, err := insights.GroupRetentionSeries(ctx, rows, q.Properties(), q.BreakdownLimit())
		if err != nil {
			t.Fatalf("GroupRetentionSeries: %v", err)
		}
		if len(series) < 2 {
			t.Fatalf("expected at least 2 breakdown series, got %d", len(series))
		}

		// Seed data: alice(US) sign_up+login day 1, login day 2. bob(GB) sign_up+login day 1.
		// US cohort_size = 1 (alice), GB cohort_size = 1 (bob).
		// US should have day-1 retention (alice logged in day 2); GB should not.
		byCountry := map[string]*insightsv1.RetentionSeries{}
		for _, s := range series {
			byCountry[s.Breakdown["$country"]] = s
		}
		us, gb := byCountry["US"], byCountry["GB"]
		if us == nil || gb == nil {
			t.Fatalf("expected US and GB series, got keys: %v", byCountry)
		}
		if len(us.Cohorts) == 0 || us.Cohorts[0].GetCohortSize() != 1 {
			t.Errorf("US cohort_size: got %+v, want 1", us.Cohorts)
		}
		if len(gb.Cohorts) == 0 || gb.Cohorts[0].GetCohortSize() != 1 {
			t.Errorf("GB cohort_size: got %+v, want 1", gb.Cohorts)
		}
	})

	// Rollup parity: ExecuteQuery routes eligible queries to dashboard_event_rollup_daily
	// (populated by the MV on seed insert); BuildTrendsQuery/BuildSegmentationQuery are the
	// pure raw-events builders. Seeded under a dedicated project so the assertions are
	// independent of events inserted by other subtests under testProjectID (which the MV
	// also rolls up).
	const rollupProjectID = "proj_rollup"
	seedRollupEvents(t, ctx, ch, rollupProjectID)

	t.Run("rollup_parity_trends_breakdown", func(t *testing.T) {
		req := rollupParityTrendsReq(insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL)
		resp, err := insights.ExecuteQuery(ctx, executor, rollupProjectID, req, time.Now())
		if err != nil {
			t.Fatalf("ExecuteQuery (rollup): %v", err)
		}
		rollup := flattenTrendsResp(resp)

		rawQ, err := insights.BuildTrendsQuery(req, rollupProjectID)
		if err != nil {
			t.Fatalf("BuildTrendsQuery (raw): %v", err)
		}
		rawRows, err := executor.QueryTrends(ctx, rollupProjectID, rawQ)
		if err != nil {
			t.Fatalf("QueryTrends (raw): %v", err)
		}
		raw, err := flattenTrendsFromRaw(ctx, rawRows, rawQ)
		if err != nil {
			t.Fatalf("flattenTrendsFromRaw: %v", err)
		}

		if !reflect.DeepEqual(rollup, raw) {
			t.Errorf("rollup vs raw mismatch:\nrollup=%v\nraw=%v", rollup, raw)
		}
		if rollup["page_view|US|2024-01-01"] != 2 {
			t.Errorf("sanity: page_view|US|2024-01-01 = %v, want 2 (all: %v)", rollup["page_view|US|2024-01-01"], rollup)
		}
	})

	t.Run("rollup_parity_trends_unique_users", func(t *testing.T) {
		req := rollupParityTrendsReq(insightsv1.AggregationType_AGGREGATION_TYPE_UNIQUE_USERS)
		resp, err := insights.ExecuteQuery(ctx, executor, rollupProjectID, req, time.Now())
		if err != nil {
			t.Fatalf("ExecuteQuery (rollup): %v", err)
		}
		rollup := flattenTrendsResp(resp)

		rawQ, err := insights.BuildTrendsQuery(req, rollupProjectID)
		if err != nil {
			t.Fatalf("BuildTrendsQuery (raw): %v", err)
		}
		rawRows, err := executor.QueryTrends(ctx, rollupProjectID, rawQ)
		if err != nil {
			t.Fatalf("QueryTrends (raw): %v", err)
		}
		raw, err := flattenTrendsFromRaw(ctx, rawRows, rawQ)
		if err != nil {
			t.Fatalf("flattenTrendsFromRaw: %v", err)
		}

		if !reflect.DeepEqual(rollup, raw) {
			t.Errorf("unique-users rollup vs raw mismatch:\nrollup=%v\nraw=%v", rollup, raw)
		}
	})

	t.Run("rollup_parity_segmentation", func(t *testing.T) {
		req := &insightsv1.QueryRequest{
			Spec: &insightsv1.InsightQuerySpec{
				InsightType: insightsv1.InsightType_INSIGHT_TYPE_SEGMENTATION.Enum(),
				Events: []*insightsv1.EventQuery{
					{Event: &commonv1.EventFilter{Kind: proto.String("page_view")}, Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL.Enum()},
				},
			},
			TimeRange:   &commonv1.TimeRange{From: timestamppb.New(time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)), To: timestamppb.New(time.Date(2024, 1, 4, 0, 0, 0, 0, time.UTC))},
			Granularity: insightsv1.Granularity_GRANULARITY_DAY.Enum(),
		}
		resp, err := insights.ExecuteQuery(ctx, executor, rollupProjectID, req, time.Now())
		if err != nil {
			t.Fatalf("ExecuteQuery (rollup): %v", err)
		}
		rollupTotal := resp.GetSegmentation().GetTotal()

		rawQ, err := insights.BuildSegmentationQuery(req, rollupProjectID)
		if err != nil {
			t.Fatalf("BuildSegmentationQuery (raw): %v", err)
		}
		rawTotal, err := executor.QueryScalar(ctx, rollupProjectID, rawQ)
		if err != nil {
			t.Fatalf("QueryScalar (raw): %v", err)
		}
		if rollupTotal != rawTotal {
			t.Errorf("segmentation rollup=%v raw=%v", rollupTotal, rawTotal)
		}
		if rollupTotal != 6 {
			t.Errorf("segmentation total = %v, want 6", rollupTotal)
		}
	})

	t.Run("rollup_table_populated_by_mv", func(t *testing.T) {
		var total uint64
		if err := ch.Conn.QueryRow(ctx,
			"SELECT toUInt64(sum(cnt)) FROM dashboard_event_rollup_daily WHERE project_id = ? AND dim_name = '$__total__' AND kind = 'page_view'",
			rollupProjectID,
		).Scan(&total); err != nil {
			t.Fatalf("query rollup: %v", err)
		}
		if total != 6 {
			t.Errorf("rollup $__total__ page_view count: got %d, want 6 (MV did not populate)", total)
		}
	})

	t.Run("rollup_parity_trends_week_unique_users", func(t *testing.T) {
		// WEEK granularity over a window where alice appears on days 1/2/3 (all in
		// the same ISO week) forces the rollup's per-day uniq_state to merge across
		// days within one bucket — the cross-day uniqMerge that DAY-granularity
		// tests never exercise. Parity with the raw builder proves the merge is
		// correct, and the per-country totals prove the repeat user collapses to 1.
		req := &insightsv1.QueryRequest{
			Spec: &insightsv1.InsightQuerySpec{
				InsightType: insightsv1.InsightType_INSIGHT_TYPE_TRENDS.Enum(),
				Events:      []*insightsv1.EventQuery{{Event: &commonv1.EventFilter{Kind: proto.String("page_view")}, Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_UNIQUE_USERS.Enum()}},
				Breakdowns:  []*insightsv1.Breakdown{{Property: proto.String("$country")}},
			},
			TimeRange:   &commonv1.TimeRange{From: timestamppb.New(time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)), To: timestamppb.New(time.Date(2024, 1, 8, 0, 0, 0, 0, time.UTC))},
			Granularity: insightsv1.Granularity_GRANULARITY_WEEK.Enum(),
		}
		resp, err := insights.ExecuteQuery(ctx, executor, rollupProjectID, req, time.Now())
		if err != nil {
			t.Fatalf("ExecuteQuery (rollup): %v", err)
		}
		rollup := flattenTrendsResp(resp)

		rawQ, err := insights.BuildTrendsQuery(req, rollupProjectID)
		if err != nil {
			t.Fatalf("BuildTrendsQuery (raw): %v", err)
		}
		rawRows, err := executor.QueryTrends(ctx, rollupProjectID, rawQ)
		if err != nil {
			t.Fatalf("QueryTrends (raw): %v", err)
		}
		raw, err := flattenTrendsFromRaw(ctx, rawRows, rawQ)
		if err != nil {
			t.Fatalf("flattenTrendsFromRaw: %v", err)
		}

		if !reflect.DeepEqual(rollup, raw) {
			t.Errorf("week unique-users rollup vs raw mismatch:\nrollup=%v\nraw=%v", rollup, raw)
		}
		for _, s := range resp.GetTrends().GetSeries() {
			var sum float64
			for _, p := range s.GetPoints() {
				sum += p.GetValue()
			}
			switch country := s.GetBreakdown()["$country"]; country {
			case "US":
				if sum != 2 {
					t.Errorf("US weekly unique users = %v, want 2 (alice merged across days 1-3 + charlie)", sum)
				}
			case "GB":
				if sum != 1 {
					t.Errorf("GB weekly unique users = %v, want 1 (bob merged across days 1-2)", sum)
				}
			}
		}
	})

	t.Run("rollup_parity_segmentation_per_user_avg", func(t *testing.T) {
		// PER_USER_AVG = sum(cnt)/uniqMerge(uniq_state): the numerator and
		// denominator come from different aggregate states, so verify the ratio
		// matches the raw count(*)/uniq(distinct_id) over the same window.
		req := &insightsv1.QueryRequest{
			Spec: &insightsv1.InsightQuerySpec{
				InsightType: insightsv1.InsightType_INSIGHT_TYPE_SEGMENTATION.Enum(),
				Events:      []*insightsv1.EventQuery{{Event: &commonv1.EventFilter{Kind: proto.String("page_view")}, Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_PER_USER_AVG.Enum()}},
			},
			TimeRange:   &commonv1.TimeRange{From: timestamppb.New(time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)), To: timestamppb.New(time.Date(2024, 1, 4, 0, 0, 0, 0, time.UTC))},
			Granularity: insightsv1.Granularity_GRANULARITY_DAY.Enum(),
		}
		resp, err := insights.ExecuteQuery(ctx, executor, rollupProjectID, req, time.Now())
		if err != nil {
			t.Fatalf("ExecuteQuery (rollup): %v", err)
		}
		rollupAvg := resp.GetSegmentation().GetTotal()

		rawQ, err := insights.BuildSegmentationQuery(req, rollupProjectID)
		if err != nil {
			t.Fatalf("BuildSegmentationQuery (raw): %v", err)
		}
		rawAvg, err := executor.QueryScalar(ctx, rollupProjectID, rawQ)
		if err != nil {
			t.Fatalf("QueryScalar (raw): %v", err)
		}
		if rollupAvg != rawAvg {
			t.Errorf("per-user-avg rollup=%v raw=%v", rollupAvg, rawAvg)
		}
		if rollupAvg != 2 {
			t.Errorf("per-user-avg = %v, want 2 (6 events / 3 users)", rollupAvg)
		}
	})

	t.Run("rollup_duplicate_overcount_documented", func(t *testing.T) {
		// Pins the accepted tradeoff: the rollup over-counts duplicate event
		// deliveries that the raw ReplacingMergeTree dedups. Seeded under its own
		// project so the OPTIMIZE below is isolated.
		const dupProjectID = "proj_rollup_dup"
		occur := time.Date(2024, 2, 1, 12, 0, 0, 0, time.UTC)
		eventID := uuid.New().String()
		// Insert the same event twice (identical dedup key): an at-least-once
		// redelivery / client retry. Raw collapses these on merge; the incremental
		// MV fires per insert and sums them.
		for i := 0; i < 2; i++ {
			if err := insertAutoEvent(ctx, ch.Conn, dupProjectID, eventID, "page_view", "alice", occur,
				variantStringMap(map[string]string{"$country": "US"})); err != nil {
				t.Fatalf("seed dup event %d: %v", i, err)
			}
		}
		// Force the raw-side ReplacingMergeTree merge so the raw builder (no FINAL)
		// reads the deduplicated truth, mirroring production eventual consistency.
		// Without this, raw also over-counts pre-merge and the divergence is hidden.
		if err := ch.Conn.Exec(ctx, "OPTIMIZE TABLE events FINAL"); err != nil {
			t.Fatalf("optimize events: %v", err)
		}

		totals := func(agg insightsv1.AggregationType) (rollup, rawVal float64) {
			req := &insightsv1.QueryRequest{
				Spec: &insightsv1.InsightQuerySpec{
					InsightType: insightsv1.InsightType_INSIGHT_TYPE_SEGMENTATION.Enum(),
					Events:      []*insightsv1.EventQuery{{Event: &commonv1.EventFilter{Kind: proto.String("page_view")}, Aggregation: agg.Enum()}},
				},
				// Day-aligned window (midnight→midnight) covering the occur day, so it
				// stays rollup-eligible after the window-alignment guard; a mid-day
				// window would correctly fall back to raw and not exercise the over-count.
				TimeRange:   &commonv1.TimeRange{From: timestamppb.New(time.Date(2024, 2, 1, 0, 0, 0, 0, time.UTC)), To: timestamppb.New(time.Date(2024, 2, 2, 0, 0, 0, 0, time.UTC))},
				Granularity: insightsv1.Granularity_GRANULARITY_DAY.Enum(),
			}
			resp, err := insights.ExecuteQuery(ctx, executor, dupProjectID, req, time.Now())
			if err != nil {
				t.Fatalf("ExecuteQuery: %v", err)
			}
			rawQ, err := insights.BuildSegmentationQuery(req, dupProjectID)
			if err != nil {
				t.Fatalf("BuildSegmentationQuery: %v", err)
			}
			raw, err := executor.QueryScalar(ctx, dupProjectID, rawQ)
			if err != nil {
				t.Fatalf("QueryScalar: %v", err)
			}
			return resp.GetSegmentation().GetTotal(), raw
		}

		// TOTAL: rollup keeps the duplicate (2), raw dedups it (1) — documented drift.
		if rollup, raw := totals(insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL); rollup != 2 || raw != 1 {
			t.Errorf("TOTAL rollup=%v raw=%v, want rollup=2 raw=1 (accepted over-count; see rollup.go canUseEventRollup)", rollup, raw)
		}
		// UNIQUE_USERS: immune — uniqState on distinct_id is idempotent.
		if rollup, raw := totals(insightsv1.AggregationType_AGGREGATION_TYPE_UNIQUE_USERS); rollup != raw || rollup != 1 {
			t.Errorf("UNIQUE_USERS rollup=%v raw=%v, want both=1 (dedup-immune)", rollup, raw)
		}
	})

	// ---- Session insights (issue #161) ----------------------------------------
	// One shared seed across the session subtests; D straddles the Jan1/Jan2 boundary.
	const sessionProjectID = "proj_session_rollup"
	seedSessionEvents(t, ctx, ch, sessionProjectID)
	fullFrom := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	fullTo := time.Date(2024, 1, 3, 0, 0, 0, 0, time.UTC)

	t.Run("session_rollup_parity_all_metrics", func(t *testing.T) {
		// The load-bearing check: every metric returns identical numbers from the
		// rollup fast path and the raw builder over the same data + window.
		for _, m := range []insightsv1.SessionMetric{
			insightsv1.SessionMetric_SESSION_METRIC_SESSIONS,
			insightsv1.SessionMetric_SESSION_METRIC_AVG_DURATION,
			insightsv1.SessionMetric_SESSION_METRIC_BOUNCE_RATE,
		} {
			assertSessionTrendsParity(t, ctx, executor, sessionProjectID, sessionTrendsReq(m, "", fullFrom, fullTo))
			assertSessionSegParity(t, ctx, executor, sessionProjectID, sessionSegReq(m, fullFrom, fullTo))
		}
		// ENTRY/EXIT are trends + one breakdown only.
		assertSessionTrendsParity(t, ctx, executor, sessionProjectID,
			sessionTrendsReq(insightsv1.SessionMetric_SESSION_METRIC_ENTRY, "$url", fullFrom, fullTo))
		assertSessionTrendsParity(t, ctx, executor, sessionProjectID,
			sessionTrendsReq(insightsv1.SessionMetric_SESSION_METRIC_EXIT, "$url", fullFrom, fullTo))
	})

	t.Run("session_scalar_numeric", func(t *testing.T) {
		// SESSIONS: A,B,C,D all start in [Jan1,Jan3) → 4.
		if got := assertSessionSegParity(t, ctx, executor, sessionProjectID,
			sessionSegReq(insightsv1.SessionMetric_SESSION_METRIC_SESSIONS, fullFrom, fullTo)); got != 4 {
			t.Errorf("SESSIONS = %v, want 4", got)
		}
		// AVG_DURATION: (300 + 0 + 1800 + 3600) / 4 = 1425s.
		if got := assertSessionSegParity(t, ctx, executor, sessionProjectID,
			sessionSegReq(insightsv1.SessionMetric_SESSION_METRIC_AVG_DURATION, fullFrom, fullTo)); got != 1425 {
			t.Errorf("AVG_DURATION = %v, want 1425", got)
		}
		// BOUNCE_RATE: only B is single-event → 1/4 = 25%.
		if got := assertSessionSegParity(t, ctx, executor, sessionProjectID,
			sessionSegReq(insightsv1.SessionMetric_SESSION_METRIC_BOUNCE_RATE, fullFrom, fullTo)); got != 25 {
			t.Errorf("BOUNCE_RATE = %v, want 25", got)
		}
	})

	t.Run("session_entry_exit_differ", func(t *testing.T) {
		// ENTRY uses argMin(url, occur_time); EXIT uses argMax. A and D have distinct
		// first/last pages, so the two metrics must produce different buckets — the
		// check that proves entry/exit attribution isn't swapped.
		entryResp, err := insights.ExecuteQuery(ctx, executor, sessionProjectID,
			sessionTrendsReq(insightsv1.SessionMetric_SESSION_METRIC_ENTRY, "$url", fullFrom, fullTo), time.Now())
		if err != nil {
			t.Fatalf("entry ExecuteQuery: %v", err)
		}
		exitResp, err := insights.ExecuteQuery(ctx, executor, sessionProjectID,
			sessionTrendsReq(insightsv1.SessionMetric_SESSION_METRIC_EXIT, "$url", fullFrom, fullTo), time.Now())
		if err != nil {
			t.Fatalf("exit ExecuteQuery: %v", err)
		}
		entry := flattenSessionTrends(entryResp)
		exit := flattenSessionTrends(exitResp)

		// Sessions are bucketed by START day: A,B,D start Jan1; C starts Jan2.
		wantEntry := map[string]float64{
			"/landing|2024-01-01": 1, "/home|2024-01-01": 1, "/x|2024-01-01": 1, "/a|2024-01-02": 1,
		}
		wantExit := map[string]float64{
			"/checkout|2024-01-01": 1, "/home|2024-01-01": 1, "/y|2024-01-01": 1, "/c|2024-01-02": 1,
		}
		if !reflect.DeepEqual(entry, wantEntry) {
			t.Errorf("ENTRY = %v, want %v", entry, wantEntry)
		}
		if !reflect.DeepEqual(exit, wantExit) {
			t.Errorf("EXIT = %v, want %v", exit, wantExit)
		}
		if reflect.DeepEqual(entry, exit) {
			t.Error("ENTRY and EXIT identical — argMin/argMax attribution not exercised")
		}
	})

	t.Run("session_boundary_straddle", func(t *testing.T) {
		// Window [Jan2,Jan3): session D started Jan1 23:30, so it must be EXCLUDED
		// even though its second event (/y) is at Jan2 00:30 — full-session semantics
		// key on start, they do not clip a session's events to the window. Only C
		// (started Jan2 09:00) qualifies. This is the exact case the old raw builder
		// (occur_time WHERE clip) got wrong; rollup and raw must now agree.
		from := time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC)
		to := time.Date(2024, 1, 3, 0, 0, 0, 0, time.UTC)
		req := sessionSegReq(insightsv1.SessionMetric_SESSION_METRIC_SESSIONS, from, to)
		if got := assertSessionSegParity(t, ctx, executor, sessionProjectID, req); got != 1 {
			t.Errorf("SESSIONS in [Jan2,Jan3) = %v, want 1 (only C; D excluded by start)", got)
		}
		// Entry pages: only C's /a, on Jan2. D's /x and /y must be absent.
		entryResp, err := insights.ExecuteQuery(ctx, executor, sessionProjectID,
			sessionTrendsReq(insightsv1.SessionMetric_SESSION_METRIC_ENTRY, "$url", from, to), time.Now())
		if err != nil {
			t.Fatalf("entry ExecuteQuery: %v", err)
		}
		entry := flattenSessionTrends(entryResp)
		want := map[string]float64{"/a|2024-01-02": 1}
		if !reflect.DeepEqual(entry, want) {
			t.Errorf("boundary entry = %v, want %v (D's /x,/y excluded)", entry, want)
		}
		assertSessionTrendsParity(t, ctx, executor, sessionProjectID,
			sessionTrendsReq(insightsv1.SessionMetric_SESSION_METRIC_ENTRY, "$url", from, to))
	})

	t.Run("session_empty_window", func(t *testing.T) {
		// No sessions start in [Jan5,Jan6): scalar metrics must return 0, not NULL/NaN
		// (the if(count()=0,0,…) guards in sessionMetricAggExpr).
		from := time.Date(2024, 1, 5, 0, 0, 0, 0, time.UTC)
		to := time.Date(2024, 1, 6, 0, 0, 0, 0, time.UTC)
		for _, m := range []insightsv1.SessionMetric{
			insightsv1.SessionMetric_SESSION_METRIC_SESSIONS,
			insightsv1.SessionMetric_SESSION_METRIC_AVG_DURATION,
			insightsv1.SessionMetric_SESSION_METRIC_BOUNCE_RATE,
		} {
			req := sessionSegReq(m, from, to)
			resp, err := insights.ExecuteQuery(ctx, executor, sessionProjectID, req, time.Now())
			if err != nil {
				t.Fatalf("metric %v ExecuteQuery: %v", m, err)
			}
			if got := resp.GetSegmentation().GetTotal(); got != 0 {
				t.Errorf("metric %v on empty window = %v, want 0", m, got)
			}
		}
	})

	t.Run("session_rollup_bounce_duplicate_overcount_documented", func(t *testing.T) {
		// Pins the accepted session-rollup tradeoff (see canUseSessionRollup): a
		// duplicate delivery inflates the rollup's event_count_state (countState
		// without event_id), so a genuinely single-event session reads event_count>1
		// and is no longer counted as a bounce — the rollup UNDER-reports bounce rate.
		// The raw path's count() self-corrects once ReplacingMergeTree merges.
		const dupProjectID = "proj_session_bounce_dup"
		occur := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
		sessionID := "00000000-0000-0000-0000-0000000000e5"
		eventID := uuid.New().String()
		// Same event_id twice = an at-least-once redelivery of a one-event session.
		for i := 0; i < 2; i++ {
			if err := insertSessionEvent(ctx, ch.Conn, dupProjectID, eventID,
				"page_view", "alice", sessionID, occur, "/only", "US"); err != nil {
				t.Fatalf("seed dup session event %d: %v", i, err)
			}
		}
		if err := ch.Conn.Exec(ctx, "OPTIMIZE TABLE events FINAL"); err != nil {
			t.Fatalf("optimize events: %v", err)
		}
		from := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
		to := time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC)
		req := sessionSegReq(insightsv1.SessionMetric_SESSION_METRIC_BOUNCE_RATE, from, to)

		resp, err := insights.ExecuteQuery(ctx, executor, dupProjectID, req, time.Now())
		if err != nil {
			t.Fatalf("rollup ExecuteQuery: %v", err)
		}
		rollupBounce := resp.GetSegmentation().GetTotal()

		rawQ, err := insights.BuildSessionSegmentationQuery(req, dupProjectID)
		if err != nil {
			t.Fatalf("BuildSessionSegmentationQuery: %v", err)
		}
		rawBounce, err := executor.QueryScalar(ctx, dupProjectID, rawQ)
		if err != nil {
			t.Fatalf("QueryScalar: %v", err)
		}
		// Documented drift: raw sees a 1-event session (100% bounce); the rollup's
		// duplicate-inflated event_count=2 drops it below the bounce threshold (0%).
		if rawBounce != 100 {
			t.Errorf("raw BOUNCE_RATE = %v, want 100 (single-event session after dedup)", rawBounce)
		}
		if rollupBounce != 0 {
			t.Errorf("rollup BOUNCE_RATE = %v, want 0 (duplicate inflates event_count past the bounce test; see canUseSessionRollup)", rollupBounce)
		}
	})

	t.Run("rollup_parity_trends_multi_event_breakdown", func(t *testing.T) {
		// Two event kinds + a breakdown exercises rollup top_vals over Or(kind...)
		// and parity with the raw multi-event trends builder (Go-side top-N).
		const projectID = "proj_rollup_multi"
		seed := []struct {
			day            int
			kind, user, cc string
		}{
			{1, "page_view", "alice", "US"}, {1, "page_view", "bob", "GB"}, {2, "page_view", "alice", "US"},
			{1, "signup", "alice", "US"}, {2, "signup", "carol", "GB"},
		}
		for _, e := range seed {
			if err := insertAutoEvent(ctx, ch.Conn, projectID, uuid.New().String(), e.kind, e.user,
				time.Date(2024, 1, e.day, 12, 0, 0, 0, time.UTC),
				variantStringMap(map[string]string{"$country": e.cc})); err != nil {
				t.Fatalf("seed: %v", err)
			}
		}
		req := &insightsv1.QueryRequest{
			Spec: &insightsv1.InsightQuerySpec{
				InsightType: insightsv1.InsightType_INSIGHT_TYPE_TRENDS.Enum(),
				Events: []*insightsv1.EventQuery{
					{Event: &commonv1.EventFilter{Kind: proto.String("page_view")}, Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL.Enum()},
					{Event: &commonv1.EventFilter{Kind: proto.String("signup")}, Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL.Enum()},
				},
				Breakdowns: []*insightsv1.Breakdown{{Property: proto.String("$country")}},
			},
			TimeRange:   &commonv1.TimeRange{From: timestamppb.New(time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)), To: timestamppb.New(time.Date(2024, 1, 4, 0, 0, 0, 0, time.UTC))},
			Granularity: insightsv1.Granularity_GRANULARITY_DAY.Enum(),
		}
		resp, err := insights.ExecuteQuery(ctx, executor, projectID, req, time.Now())
		if err != nil {
			t.Fatalf("ExecuteQuery (rollup): %v", err)
		}
		rollup := flattenTrendsResp(resp)

		rawQ, err := insights.BuildTrendsQuery(req, projectID)
		if err != nil {
			t.Fatalf("BuildTrendsQuery (raw): %v", err)
		}
		rawRows, err := executor.QueryTrends(ctx, projectID, rawQ)
		if err != nil {
			t.Fatalf("QueryTrends (raw): %v", err)
		}
		raw, err := flattenTrendsFromRaw(ctx, rawRows, rawQ)
		if err != nil {
			t.Fatalf("flattenTrendsFromRaw: %v", err)
		}

		if !reflect.DeepEqual(rollup, raw) {
			t.Errorf("multi-event breakdown rollup vs raw mismatch:\nrollup=%v\nraw=%v", rollup, raw)
		}
		// Sanity: both kinds present, US page_view spans days 1+2.
		if rollup["page_view|US|2024-01-01"] != 1 || rollup["signup|GB|2024-01-02"] != 1 {
			t.Errorf("unexpected multi-event values: %v", rollup)
		}
	})

	t.Run("rollup_parity_trends_others_bucket", func(t *testing.T) {
		// More breakdown values than breakdown_limit forces top-N + '$others'
		// collapse. Rollup applies it in SQL; raw applies it in GroupSeries.
		// Parity proves they bucket identically (including tie-break on value ASC).
		const projectID = "proj_rollup_others"
		// US=3, GB=2, FR=1 on day 1. With breakdown_limit=2, FR collapses to $others.
		seed := []struct{ user, cc string }{
			{"u1", "US"}, {"u2", "US"}, {"u3", "US"},
			{"u4", "GB"}, {"u5", "GB"},
			{"u6", "FR"},
		}
		for _, e := range seed {
			if err := insertAutoEvent(ctx, ch.Conn, projectID, uuid.New().String(), "page_view", e.user,
				time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC),
				variantStringMap(map[string]string{"$country": e.cc})); err != nil {
				t.Fatalf("seed: %v", err)
			}
		}
		req := &insightsv1.QueryRequest{
			Spec: &insightsv1.InsightQuerySpec{
				InsightType:    insightsv1.InsightType_INSIGHT_TYPE_TRENDS.Enum(),
				Events:         []*insightsv1.EventQuery{{Event: &commonv1.EventFilter{Kind: proto.String("page_view")}, Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL.Enum()}},
				Breakdowns:     []*insightsv1.Breakdown{{Property: proto.String("$country")}},
				BreakdownLimit: proto.Int32(2),
			},
			TimeRange:   &commonv1.TimeRange{From: timestamppb.New(time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)), To: timestamppb.New(time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC))},
			Granularity: insightsv1.Granularity_GRANULARITY_DAY.Enum(),
		}
		resp, err := insights.ExecuteQuery(ctx, executor, projectID, req, time.Now())
		if err != nil {
			t.Fatalf("ExecuteQuery (rollup): %v", err)
		}
		rollup := flattenTrendsResp(resp)

		rawQ, err := insights.BuildTrendsQuery(req, projectID)
		if err != nil {
			t.Fatalf("BuildTrendsQuery (raw): %v", err)
		}
		rawRows, err := executor.QueryTrends(ctx, projectID, rawQ)
		if err != nil {
			t.Fatalf("QueryTrends (raw): %v", err)
		}
		raw, err := flattenTrendsFromRaw(ctx, rawRows, rawQ)
		if err != nil {
			t.Fatalf("flattenTrendsFromRaw: %v", err)
		}

		if !reflect.DeepEqual(rollup, raw) {
			t.Errorf("$others rollup vs raw mismatch:\nrollup=%v\nraw=%v", rollup, raw)
		}
		// FR (below the top-2 cut) must collapse into a single $others bucket.
		if rollup["page_view|$others|2024-01-01"] != 1 {
			t.Errorf("expected FR collapsed into $others=1, got %v (all: %v)", rollup["page_view|$others|2024-01-01"], rollup)
		}
	})

	t.Run("rollup_parity_trends_multi_event_others_bucket", func(t *testing.T) {
		// Multi-event + breakdown + breakdown_limit forcing $others. Seed is shaped
		// so per-kind counts have no ties and the per-kind top-N ordering
		// (US > GB > FR for both kinds) matches the shared-across-kinds ordering
		// (rollup top_vals sums cnt over both kinds: US=9, GB=5, FR=2). Both
		// strategies pick {US, GB} so parity holds AND zero-fill on $others cells
		// is exercised end-to-end. A seed with ties or with per-kind orderings
		// that diverge from the cross-kind ordering would surface a pre-existing
		// rollup-vs-raw top-N strategy difference (rollup uses one shared top_vals
		// over OR(kinds); raw uses per-kind top-N) — out of scope here.
		const projectID = "proj_rollup_multi_others"
		seed := []struct{ kind, user, cc string }{
			{"page_view", "u1", "US"}, {"page_view", "u2", "US"}, {"page_view", "u3", "US"},
			{"page_view", "u4", "US"}, {"page_view", "u5", "US"},
			{"page_view", "u6", "GB"}, {"page_view", "u7", "GB"}, {"page_view", "u8", "GB"},
			{"page_view", "u9", "FR"},
			{"signup", "u10", "US"}, {"signup", "u11", "US"}, {"signup", "u12", "US"}, {"signup", "u13", "US"},
			{"signup", "u14", "GB"}, {"signup", "u15", "GB"},
			{"signup", "u16", "FR"},
		}
		for _, e := range seed {
			if err := insertAutoEvent(ctx, ch.Conn, projectID, uuid.New().String(), e.kind, e.user,
				time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC),
				variantStringMap(map[string]string{"$country": e.cc})); err != nil {
				t.Fatalf("seed: %v", err)
			}
		}
		req := &insightsv1.QueryRequest{
			Spec: &insightsv1.InsightQuerySpec{
				InsightType: insightsv1.InsightType_INSIGHT_TYPE_TRENDS.Enum(),
				Events: []*insightsv1.EventQuery{
					{Event: &commonv1.EventFilter{Kind: proto.String("page_view")}, Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL.Enum()},
					{Event: &commonv1.EventFilter{Kind: proto.String("signup")}, Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL.Enum()},
				},
				Breakdowns:     []*insightsv1.Breakdown{{Property: proto.String("$country")}},
				BreakdownLimit: proto.Int32(2),
			},
			TimeRange:   &commonv1.TimeRange{From: timestamppb.New(time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)), To: timestamppb.New(time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC))},
			Granularity: insightsv1.Granularity_GRANULARITY_DAY.Enum(),
		}
		assertTrendsParity(t, ctx, executor, projectID, req)

		// Sanity-pin the rollup output: both kinds keep US+GB, FR collapses to $others.
		resp, err := insights.ExecuteQuery(ctx, executor, projectID, req, time.Now())
		if err != nil {
			t.Fatalf("ExecuteQuery: %v", err)
		}
		rollup := flattenTrendsResp(resp)
		want := map[string]float64{
			"page_view|US|2024-01-01":      5,
			"page_view|GB|2024-01-01":      3,
			"page_view|$others|2024-01-01": 1,
			"signup|US|2024-01-01":         4,
			"signup|GB|2024-01-01":         2,
			"signup|$others|2024-01-01":    1,
		}
		if !reflect.DeepEqual(rollup, want) {
			t.Errorf("multi-event $others rollup mismatch:\nwant=%v\ngot =%v", want, rollup)
		}
	})

	t.Run("rollup_parity_matrix", func(t *testing.T) {
		// Exhaustive rollup == raw across the eligible grid: trends over
		// {TOTAL, UNIQUE_USERS, PER_USER_AVG} × {DAY, WEEK, MONTH} × {no-breakdown,
		// $country} × {single, multi-event}, plus segmentation over {agg} × {single,
		// multi-event}. Each cell runs the request through ExecuteQuery (rollup) and
		// the raw builder + executor, and asserts identical output. Non-vacuous:
		// rollup_duplicate_overcount_documented proves ExecuteQuery actually routes
		// eligible queries to the rollup (rollup=2 vs raw=1), not silently to raw.
		const projectID = "proj_rollup_matrix"
		seedMatrixEvents(t, ctx, ch, projectID)

		grans := []struct {
			g        insightsv1.Granularity
			from, to time.Time
		}{
			{insightsv1.Granularity_GRANULARITY_DAY, time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2024, 1, 8, 0, 0, 0, 0, time.UTC)},
			{insightsv1.Granularity_GRANULARITY_WEEK, time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2024, 1, 29, 0, 0, 0, 0, time.UTC)},
			{insightsv1.Granularity_GRANULARITY_MONTH, time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2024, 4, 1, 0, 0, 0, 0, time.UTC)},
		}
		aggs := []insightsv1.AggregationType{
			insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL,
			insightsv1.AggregationType_AGGREGATION_TYPE_UNIQUE_USERS,
			insightsv1.AggregationType_AGGREGATION_TYPE_PER_USER_AVG,
		}

		for _, agg := range aggs {
			for _, gr := range grans {
				for _, bd := range []bool{false, true} {
					for _, multi := range []bool{false, true} {
						name := fmt.Sprintf("trends_%s_%s_bd=%v_multi=%v", agg, gr.g, bd, multi)
						t.Run(name, func(t *testing.T) {
							spec := &insightsv1.InsightQuerySpec{
								InsightType: insightsv1.InsightType_INSIGHT_TYPE_TRENDS.Enum(),
								Events:      []*insightsv1.EventQuery{{Event: &commonv1.EventFilter{Kind: proto.String("page_view")}, Aggregation: agg.Enum()}},
							}
							if multi {
								spec.Events = append(spec.Events, &insightsv1.EventQuery{Event: &commonv1.EventFilter{Kind: proto.String("signup")}, Aggregation: agg.Enum()})
							}
							if bd {
								spec.Breakdowns = []*insightsv1.Breakdown{{Property: proto.String("$country")}}
							}
							req := &insightsv1.QueryRequest{
								Spec:        spec,
								TimeRange:   &commonv1.TimeRange{From: timestamppb.New(gr.from), To: timestamppb.New(gr.to)},
								Granularity: gr.g.Enum(),
							}
							assertTrendsParity(t, ctx, executor, projectID, req)
						})
					}
				}
			}
		}

		for _, agg := range aggs {
			for _, multi := range []bool{false, true} {
				name := fmt.Sprintf("segmentation_%s_multi=%v", agg, multi)
				t.Run(name, func(t *testing.T) {
					spec := &insightsv1.InsightQuerySpec{
						InsightType: insightsv1.InsightType_INSIGHT_TYPE_SEGMENTATION.Enum(),
						Events:      []*insightsv1.EventQuery{{Event: &commonv1.EventFilter{Kind: proto.String("page_view")}, Aggregation: agg.Enum()}},
					}
					if multi {
						spec.Events = append(spec.Events, &insightsv1.EventQuery{Event: &commonv1.EventFilter{Kind: proto.String("signup")}, Aggregation: agg.Enum()})
					}
					req := &insightsv1.QueryRequest{
						Spec:        spec,
						TimeRange:   &commonv1.TimeRange{From: timestamppb.New(time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)), To: timestamppb.New(time.Date(2024, 4, 1, 0, 0, 0, 0, time.UTC))},
						Granularity: insightsv1.Granularity_GRANULARITY_DAY.Enum(),
					}
					assertSegParity(t, ctx, executor, projectID, req)
				})
			}
		}
	})

	t.Run("top_k", func(t *testing.T) {
		// Sept 2024 window, isolated from every other seed (Jan–Jun).
		seedTopKEvents(t, ctx, ch)
		seedIntegrationProfiles(t, ctx, ch)
		topKWindow := &commonv1.TimeRange{
			From: timestamppb.New(time.Date(2024, 9, 1, 0, 0, 0, 0, time.UTC)),
			To:   timestamppb.New(time.Date(2024, 9, 8, 0, 0, 0, 0, time.UTC)),
		}
		topKReq := func(tk *insightsv1.TopKQuery) *insightsv1.QueryRequest {
			return &insightsv1.QueryRequest{
				Spec: &insightsv1.InsightQuerySpec{
					InsightType: insightsv1.InsightType_INSIGHT_TYPE_TOP_K.Enum(),
					TopK:        tk,
				},
				TimeRange:   topKWindow,
				Granularity: insightsv1.Granularity_GRANULARITY_DAY.Enum(),
			}
		}
		runTopK := func(t *testing.T, tk *insightsv1.TopKQuery) []insights.TopKRow {
			t.Helper()
			q, err := insights.BuildTopKQuery(topKReq(tk), testProjectID)
			if err != nil {
				t.Fatalf("BuildTopKQuery: %v", err)
			}
			rows, err := executor.QueryTopK(ctx, testProjectID, q)
			if err != nil {
				t.Fatalf("QueryTopK: %v", err)
			}
			return rows
		}

		// Edge-case + USER-metric fixtures live in their own day-aligned windows
		// so the no-scope event_kind test above isn't perturbed by extra kinds.
		seedTopKEdgeEvents(t, ctx, ch)
		seedTopKCollisionProfiles(t, ctx, ch)
		edgeWindow := &commonv1.TimeRange{
			From: timestamppb.New(time.Date(2024, 10, 1, 0, 0, 0, 0, time.UTC)),
			To:   timestamppb.New(time.Date(2024, 10, 8, 0, 0, 0, 0, time.UTC)),
		}
		collisionWindow := &commonv1.TimeRange{
			From: timestamppb.New(time.Date(2024, 11, 1, 0, 0, 0, 0, time.UTC)),
			To:   timestamppb.New(time.Date(2024, 11, 8, 0, 0, 0, 0, time.UTC)),
		}
		runTopKWindow := func(t *testing.T, win *commonv1.TimeRange, tk *insightsv1.TopKQuery) []insights.TopKRow {
			t.Helper()
			req := &insightsv1.QueryRequest{
				Spec: &insightsv1.InsightQuerySpec{
					InsightType: insightsv1.InsightType_INSIGHT_TYPE_TOP_K.Enum(),
					TopK:        tk,
				},
				TimeRange:   win,
				Granularity: insightsv1.Granularity_GRANULARITY_DAY.Enum(),
			}
			q, err := insights.BuildTopKQuery(req, testProjectID)
			if err != nil {
				t.Fatalf("BuildTopKQuery: %v", err)
			}
			rows, err := executor.QueryTopK(ctx, testProjectID, q)
			if err != nil {
				t.Fatalf("QueryTopK: %v", err)
			}
			return rows
		}

		t.Run("property_basic", func(t *testing.T) {
			// tk_view browsers: chrome 5, safari 3, firefox 2, edge 1.
			// K=2 → chrome, safari, then $others = firefox + edge = 3.
			rows := runTopK(t, &insightsv1.TopKQuery{
				Dimension: insightsv1.TopKQuery_DIMENSION_PROPERTY.Enum(),
				Property:  proto.String("$browser"),
				Scope:     &commonv1.EventFilter{Kind: proto.String("tk_view")},
				Limit:     proto.Int32(2),
			})
			want := []insights.TopKRow{
				{DimensionValue: "chrome", IsOthers: false, Value: 5},
				{DimensionValue: "safari", IsOthers: false, Value: 3},
				{DimensionValue: "$others", IsOthers: true, Value: 3},
			}
			if !reflect.DeepEqual(rows, want) {
				t.Errorf("expected %+v, got %+v", want, rows)
			}
		})

		t.Run("event_kind", func(t *testing.T) {
			// Kinds in the window: tk_view 11, tk_click 7, tk_lit 6, tk_purchase 5.
			// K=2 → tk_view, tk_click, then $others = 6 + 5 = 11.
			rows := runTopK(t, &insightsv1.TopKQuery{
				Dimension: insightsv1.TopKQuery_DIMENSION_EVENT_KIND.Enum(),
				Limit:     proto.Int32(2),
			})
			want := []insights.TopKRow{
				{DimensionValue: "tk_view", IsOthers: false, Value: 11},
				{DimensionValue: "tk_click", IsOthers: false, Value: 7},
				{DimensionValue: "$others", IsOthers: true, Value: 11},
			}
			if !reflect.DeepEqual(rows, want) {
				t.Errorf("expected %+v, got %+v", want, rows)
			}
		})

		t.Run("unique_users_others_no_overlap_inflation", func(t *testing.T) {
			// tk_click browsers by UNIQUE_USERS: chrome {u1,u2,u3}, safari {u4,u5},
			// opera {dup}, brave {dup}. K=2 → chrome 3, safari 2. The $others bucket
			// re-aggregates raw events, so dup — active in BOTH non-top browsers —
			// counts once (uniq of the union), not twice (sum of per-dim uniqs).
			// This pins the SQL re-aggregation design for the bucket.
			rows := runTopK(t, &insightsv1.TopKQuery{
				Dimension: insightsv1.TopKQuery_DIMENSION_PROPERTY.Enum(),
				Property:  proto.String("$browser"),
				Scope:     &commonv1.EventFilter{Kind: proto.String("tk_click")},
				Metric:    insightsv1.AggregationType_AGGREGATION_TYPE_UNIQUE_USERS.Enum(),
				Limit:     proto.Int32(2),
			})
			want := []insights.TopKRow{
				{DimensionValue: "chrome", IsOthers: false, Value: 3},
				{DimensionValue: "safari", IsOthers: false, Value: 2},
				{DimensionValue: "$others", IsOthers: true, Value: 1},
			}
			if !reflect.DeepEqual(rows, want) {
				t.Errorf("expected %+v, got %+v", want, rows)
			}
		})

		t.Run("literal_others_value", func(t *testing.T) {
			// tk_lit label values: big 3, literal "$others" 2, small 1. K=2 keeps
			// big and the REAL "$others" value (is_others=false); small collapses
			// into the synthetic bucket, which is also rendered "$others" but
			// carries is_others=true — the flag, not the string, identifies it.
			rows := runTopK(t, &insightsv1.TopKQuery{
				Dimension: insightsv1.TopKQuery_DIMENSION_PROPERTY.Enum(),
				Property:  proto.String("label"),
				Scope:     &commonv1.EventFilter{Kind: proto.String("tk_lit")},
				Limit:     proto.Int32(2),
			})
			want := []insights.TopKRow{
				{DimensionValue: "big", IsOthers: false, Value: 3},
				{DimensionValue: "$others", IsOthers: false, Value: 2},
				{DimensionValue: "$others", IsOthers: true, Value: 1},
			}
			if !reflect.DeepEqual(rows, want) {
				t.Errorf("expected %+v, got %+v", want, rows)
			}
		})

		t.Run("user_sum_alias_resolution_and_enrichment", func(t *testing.T) {
			// Full ExecuteQuery path: dispatch, alias resolution, $others, and
			// profile enrichment. tk_purchase order_amount sums per canonical user:
			// alice = 100 (id) + 50 (external_id) + 25 (alias) = 175,
			// bob = 120 (via external_id bob_ext), ghost (no profile) = 80.
			// K=2 → alice, bob enriched; ghost in un-enriched $others.
			resp, err := insights.ExecuteQuery(ctx, executor, testProjectID, topKReq(&insightsv1.TopKQuery{
				Dimension:      insightsv1.TopKQuery_DIMENSION_USER.Enum(),
				Scope:          &commonv1.EventFilter{Kind: proto.String("tk_purchase")},
				Metric:         insightsv1.AggregationType_AGGREGATION_TYPE_SUM.Enum(),
				MetricProperty: proto.String("order_amount"),
				Limit:          proto.Int32(2),
			}), time.Now())
			if err != nil {
				t.Fatalf("ExecuteQuery: %v", err)
			}
			rows := resp.GetTopK().GetRows()
			if len(rows) != 3 {
				t.Fatalf("expected 3 rows, got %d: %v", len(rows), rows)
			}

			if rows[0].GetDimensionValue() != "alice" || rows[0].GetValue() != 175 || rows[0].GetIsOthers() {
				t.Errorf("row 0: expected alice/175/is_others=false, got %v", rows[0])
			}
			if rows[0].GetProfile().GetExternalId() != "alice_ext" {
				t.Errorf("row 0: expected enrichment external_id alice_ext, got %v", rows[0].GetProfile())
			}
			if got := rows[0].GetProfile().GetProperties().GetFields()["plan"].GetStringValue(); got != "pro" {
				t.Errorf("row 0: expected properties.plan=pro, got %q", got)
			}

			if rows[1].GetDimensionValue() != "bob" || rows[1].GetValue() != 120 || rows[1].GetIsOthers() {
				t.Errorf("row 1: expected bob/120/is_others=false, got %v", rows[1])
			}
			if rows[1].GetProfile().GetExternalId() != "bob_ext" {
				t.Errorf("row 1: expected enrichment external_id bob_ext, got %v", rows[1].GetProfile())
			}

			if !rows[2].GetIsOthers() || rows[2].GetValue() != 80 {
				t.Errorf("row 2: expected $others/80, got %v", rows[2])
			}
			if rows[2].GetProfile() != nil {
				t.Errorf("row 2: $others must not be enriched, got %v", rows[2].GetProfile())
			}
		})

		t.Run("user_unidentified_distinct_id_not_enriched", func(t *testing.T) {
			// K large enough to surface ghost as a top row: it stays keyed by its
			// raw distinct_id with no profile attached.
			resp, err := insights.ExecuteQuery(ctx, executor, testProjectID, topKReq(&insightsv1.TopKQuery{
				Dimension:      insightsv1.TopKQuery_DIMENSION_USER.Enum(),
				Scope:          &commonv1.EventFilter{Kind: proto.String("tk_purchase")},
				Metric:         insightsv1.AggregationType_AGGREGATION_TYPE_SUM.Enum(),
				MetricProperty: proto.String("order_amount"),
				Limit:          proto.Int32(10),
			}), time.Now())
			if err != nil {
				t.Fatalf("ExecuteQuery: %v", err)
			}
			rows := resp.GetTopK().GetRows()
			if len(rows) != 3 {
				t.Fatalf("expected 3 rows (no $others when all fit), got %d: %v", len(rows), rows)
			}
			if rows[2].GetDimensionValue() != "ghost" || rows[2].GetValue() != 80 || rows[2].GetIsOthers() {
				t.Errorf("row 2: expected ghost/80/is_others=false, got %v", rows[2])
			}
			if rows[2].GetProfile() != nil {
				t.Errorf("ghost has no profile and must not be enriched, got %v", rows[2].GetProfile())
			}
		})

		t.Run("rollup_parity", func(t *testing.T) {
			// The Sept window is day-aligned, so ExecuteQuery routes eligible
			// queries (materialized dim / event kind, no filters, kind-only
			// scope) through the rollup; the raw builder is forced via
			// BuildTopKQuery directly. With no duplicate deliveries seeded the
			// two paths must agree exactly — rows, order, and flags.
			parityCases := []struct {
				name string
				tk   func() *insightsv1.TopKQuery
			}{
				{"browser_total_scoped", func() *insightsv1.TopKQuery {
					return &insightsv1.TopKQuery{
						Dimension: insightsv1.TopKQuery_DIMENSION_PROPERTY.Enum(),
						Property:  proto.String("$browser"),
						Scope:     &commonv1.EventFilter{Kind: proto.String("tk_view")},
						Limit:     proto.Int32(2),
					}
				}},
				{"event_kind_unique_users", func() *insightsv1.TopKQuery {
					return &insightsv1.TopKQuery{
						Dimension: insightsv1.TopKQuery_DIMENSION_EVENT_KIND.Enum(),
						Metric:    insightsv1.AggregationType_AGGREGATION_TYPE_UNIQUE_USERS.Enum(),
						Limit:     proto.Int32(2),
					}
				}},
			}
			for _, pc := range parityCases {
				t.Run(pc.name, func(t *testing.T) {
					rawRows := runTopK(t, pc.tk())

					resp, err := insights.ExecuteQuery(ctx, executor, testProjectID, topKReq(pc.tk()), time.Now())
					if err != nil {
						t.Fatalf("ExecuteQuery: %v", err)
					}
					var rollupRows []insights.TopKRow
					for _, r := range resp.GetTopK().GetRows() {
						rollupRows = append(rollupRows, insights.TopKRow{
							DimensionValue: r.GetDimensionValue(),
							IsOthers:       r.GetIsOthers(),
							Value:          r.GetValue(),
						})
					}
					if !reflect.DeepEqual(rawRows, rollupRows) {
						t.Errorf("raw and rollup paths diverge:\nraw:    %+v\nrollup: %+v", rawRows, rollupRows)
					}
				})
			}
		})

		t.Run("user_profile_filter", func(t *testing.T) {
			// PROPERTY_SOURCE_PROFILE plan=pro restricts the ranking to alice's
			// events. Note the existing profileFilterCondition contract: it
			// matches events by distinct_id IN (profile ids ∪ alias ids) — NOT
			// external_id — so alice's event keyed by "alice_ext" is excluded by
			// the filter even though the top-K identity union resolves it to her.
			// The surviving events ("alice" + "alice_anon") still group to the
			// canonical key.
			req := topKReq(&insightsv1.TopKQuery{
				Dimension: insightsv1.TopKQuery_DIMENSION_USER.Enum(),
				Scope:     &commonv1.EventFilter{Kind: proto.String("tk_purchase")},
			})
			req.Spec.FilterGroups = []*insightsv1.FilterGroup{{
				Filters: []*commonv1.PropertyFilter{{
					Property: proto.String("plan"),
					Operator: commonv1.FilterOperator_FILTER_OPERATOR_EQUALS.Enum(),
					Value:    proto.String("pro"),
					Source:   commonv1.PropertySource_PROPERTY_SOURCE_PROFILE.Enum(),
				}},
			}}
			resp, err := insights.ExecuteQuery(ctx, executor, testProjectID, req, time.Now())
			if err != nil {
				t.Fatalf("ExecuteQuery: %v", err)
			}
			rows := resp.GetTopK().GetRows()
			if len(rows) != 1 {
				t.Fatalf("expected 1 row (alice only), got %d: %v", len(rows), rows)
			}
			// TOTAL metric (default): 2 events pass the filter (id + alias).
			if rows[0].GetDimensionValue() != "alice" || rows[0].GetValue() != 2 {
				t.Errorf("expected alice/2, got %v", rows[0])
			}
		})

		t.Run("user_avg_others_remerge", func(t *testing.T) {
			// tk_metric per-user order_amount avgs: m1 15, m2 100, m3 20, m4 50,
			// mnull none (non-numeric → NULL). K=2 → top m2, m4; $others =
			// {m3,m1,mnull}. AVG is not re-mergeable from per-user avgs — the
			// bucket re-divides summed numerator by summed non-NULL denominator =
			// (60+30+0)/(3+2+0) = 18, NOT avg-of-avgs (17.5 or 11.67). The all-NULL
			// mnull user contributes 0 to both sums.
			rows := runTopKWindow(t, edgeWindow, &insightsv1.TopKQuery{
				Dimension:      insightsv1.TopKQuery_DIMENSION_USER.Enum(),
				Scope:          &commonv1.EventFilter{Kind: proto.String("tk_metric")},
				Metric:         insightsv1.AggregationType_AGGREGATION_TYPE_AVG.Enum(),
				MetricProperty: proto.String("order_amount"),
				Limit:          proto.Int32(2),
			})
			want := []insights.TopKRow{
				{DimensionValue: "m2", IsOthers: false, Value: 100},
				{DimensionValue: "m4", IsOthers: false, Value: 50},
				{DimensionValue: "$others", IsOthers: true, Value: 18},
			}
			if !reflect.DeepEqual(rows, want) {
				t.Errorf("expected %+v, got %+v", want, rows)
			}
		})

		t.Run("user_min_others_null_skip", func(t *testing.T) {
			// Per-user min: m1 10, m2 100, m3 10, m4 50, mnull NULL→rank 0. K=2 →
			// top m2, m4; $others = {m1,m3,mnull}. The bucket min skips mnull's
			// NULL partial = min(10,10,NULL) = 10 — NOT 0, which a toFloat64OrZero
			// regression (NULL→0) would produce.
			rows := runTopKWindow(t, edgeWindow, &insightsv1.TopKQuery{
				Dimension:      insightsv1.TopKQuery_DIMENSION_USER.Enum(),
				Scope:          &commonv1.EventFilter{Kind: proto.String("tk_metric")},
				Metric:         insightsv1.AggregationType_AGGREGATION_TYPE_MIN.Enum(),
				MetricProperty: proto.String("order_amount"),
				Limit:          proto.Int32(2),
			})
			want := []insights.TopKRow{
				{DimensionValue: "m2", IsOthers: false, Value: 100},
				{DimensionValue: "m4", IsOthers: false, Value: 50},
				{DimensionValue: "$others", IsOthers: true, Value: 10},
			}
			if !reflect.DeepEqual(rows, want) {
				t.Errorf("expected %+v, got %+v", want, rows)
			}
		})

		t.Run("user_max_others", func(t *testing.T) {
			// Per-user max: m1 20, m2 100, m3 30, m4 50, mnull NULL→rank 0. K=2 →
			// top m2, m4; $others = max over {m1,m3,mnull} = max(20,30,NULL) = 30.
			rows := runTopKWindow(t, edgeWindow, &insightsv1.TopKQuery{
				Dimension:      insightsv1.TopKQuery_DIMENSION_USER.Enum(),
				Scope:          &commonv1.EventFilter{Kind: proto.String("tk_metric")},
				Metric:         insightsv1.AggregationType_AGGREGATION_TYPE_MAX.Enum(),
				MetricProperty: proto.String("order_amount"),
				Limit:          proto.Int32(2),
			})
			want := []insights.TopKRow{
				{DimensionValue: "m2", IsOthers: false, Value: 100},
				{DimensionValue: "m4", IsOthers: false, Value: 50},
				{DimensionValue: "$others", IsOthers: true, Value: 30},
			}
			if !reflect.DeepEqual(rows, want) {
				t.Errorf("expected %+v, got %+v", want, rows)
			}
		})

		t.Run("property_empty_string_dimension", func(t *testing.T) {
			// chrome×3, ""(no browser)×2, safari×1. K=2 keeps chrome and the
			// empty/direct bucket as a REAL ranked row (is_others=false) — it is
			// not hidden; safari collapses into the synthetic $others.
			rows := runTopKWindow(t, edgeWindow, &insightsv1.TopKQuery{
				Dimension: insightsv1.TopKQuery_DIMENSION_PROPERTY.Enum(),
				Property:  proto.String("$browser"),
				Scope:     &commonv1.EventFilter{Kind: proto.String("tk_empty")},
				Limit:     proto.Int32(2),
			})
			want := []insights.TopKRow{
				{DimensionValue: "chrome", IsOthers: false, Value: 3},
				{DimensionValue: "", IsOthers: false, Value: 2},
				{DimensionValue: "$others", IsOthers: true, Value: 1},
			}
			if !reflect.DeepEqual(rows, want) {
				t.Errorf("expected %+v, got %+v", want, rows)
			}
		})

		t.Run("tie_break_determinism", func(t *testing.T) {
			// aaa and bbb tie at 2 events; the secondary dim-ASC sort must place
			// aaa before bbb deterministically — and the rollup path must break
			// the tie identically to the raw path.
			tk := func() *insightsv1.TopKQuery {
				return &insightsv1.TopKQuery{
					Dimension: insightsv1.TopKQuery_DIMENSION_PROPERTY.Enum(),
					Property:  proto.String("$browser"),
					Scope:     &commonv1.EventFilter{Kind: proto.String("tk_tie")},
					Limit:     proto.Int32(2),
				}
			}
			want := []insights.TopKRow{
				{DimensionValue: "aaa", IsOthers: false, Value: 2},
				{DimensionValue: "bbb", IsOthers: false, Value: 2},
				{DimensionValue: "$others", IsOthers: true, Value: 1},
			}
			if raw := runTopKWindow(t, edgeWindow, tk()); !reflect.DeepEqual(raw, want) {
				t.Errorf("raw: expected %+v, got %+v", want, raw)
			}
			// ExecuteQuery over the day-aligned Oct window routes through the rollup.
			req := &insightsv1.QueryRequest{
				Spec:        &insightsv1.InsightQuerySpec{InsightType: insightsv1.InsightType_INSIGHT_TYPE_TOP_K.Enum(), TopK: tk()},
				TimeRange:   edgeWindow,
				Granularity: insightsv1.Granularity_GRANULARITY_DAY.Enum(),
			}
			resp, err := insights.ExecuteQuery(ctx, executor, testProjectID, req, time.Now())
			if err != nil {
				t.Fatalf("ExecuteQuery: %v", err)
			}
			var rollup []insights.TopKRow
			for _, r := range resp.GetTopK().GetRows() {
				rollup = append(rollup, insights.TopKRow{
					DimensionValue: r.GetDimensionValue(),
					IsOthers:       r.GetIsOthers(),
					Value:          r.GetValue(),
				})
			}
			if !reflect.DeepEqual(rollup, want) {
				t.Errorf("rollup: expected %+v, got %+v", want, rollup)
			}
		})

		t.Run("user_alias_collision_no_inflation", func(t *testing.T) {
			// distinct_id "collide" resolves to pA (external_id) AND pB (alias).
			// The identity union's LEFT ANY JOIN must pick ONE canonical id per
			// event, so 3 collide events count as 3 under a single key — not 6
			// split across pA and pB (which a plain LEFT JOIN would produce,
			// adding a third row and pushing "solo" into $others).
			rows := runTopKWindow(t, collisionWindow, &insightsv1.TopKQuery{
				Dimension: insightsv1.TopKQuery_DIMENSION_USER.Enum(),
				Scope:     &commonv1.EventFilter{Kind: proto.String("tk_collide")},
				Limit:     proto.Int32(2),
			})
			if len(rows) != 2 {
				t.Fatalf("expected 2 rows (collide winner + solo, no $others), got %d: %+v", len(rows), rows)
			}
			if rows[0].Value != 3 || rows[0].IsOthers ||
				(rows[0].DimensionValue != "pA" && rows[0].DimensionValue != "pB") {
				t.Errorf("row 0: expected one canonical collide user (pA|pB) value 3, got %+v", rows[0])
			}
			if rows[1].DimensionValue != "solo" || rows[1].Value != 1 || rows[1].IsOthers {
				t.Errorf("row 1: expected solo/1/is_others=false, got %+v", rows[1])
			}
		})
	})
}

func rollupParityTrendsReq(agg insightsv1.AggregationType) *insightsv1.QueryRequest {
	return &insightsv1.QueryRequest{
		Spec: &insightsv1.InsightQuerySpec{
			InsightType: insightsv1.InsightType_INSIGHT_TYPE_TRENDS.Enum(),
			Events:      []*insightsv1.EventQuery{{Event: &commonv1.EventFilter{Kind: proto.String("page_view")}, Aggregation: agg.Enum()}},
			Breakdowns:  []*insightsv1.Breakdown{{Property: proto.String("$country")}},
		},
		TimeRange:   &commonv1.TimeRange{From: timestamppb.New(time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)), To: timestamppb.New(time.Date(2024, 1, 4, 0, 0, 0, 0, time.UTC))},
		Granularity: insightsv1.Granularity_GRANULARITY_DAY.Enum(),
	}
}

func flattenTrendsResp(resp *insightsv1.QueryResponse) map[string]float64 {
	out := map[string]float64{}
	for _, s := range resp.GetTrends().GetSeries() {
		bd := s.GetBreakdown()["$country"]
		for _, p := range s.GetPoints() {
			out[s.GetEventKind()+"|"+bd+"|"+p.GetTime().AsTime().Format("2006-01-02")] = p.GetValue()
		}
	}
	return out
}

func flattenTrendsFromRaw(ctx context.Context, rows []insights.TrendRow, q insights.TrendsQuery) (map[string]float64, error) {
	series, err := insights.GroupSeries(ctx, rows, q.Properties(), q.BreakdownLimit())
	if err != nil {
		return nil, err
	}
	return flattenTrendsResp(&insightsv1.QueryResponse{
		Result: &insightsv1.QueryResponse_Trends{
			Trends: &insightsv1.TrendsResult{Series: series},
		},
	}), nil
}

// seedRollupEvents inserts a fixed set of page_view events under a dedicated
// project so rollup parity assertions are deterministic: day 1 = {alice US,
// bob GB, charlie US}, day 2 = {alice US, bob GB}, day 3 = {alice US}.
func seedRollupEvents(t *testing.T, ctx context.Context, ch *testutil.TestClickHouse, projectID string) {
	t.Helper()
	events := []struct {
		day     int
		user    string
		country string
	}{
		{1, "alice", "US"}, {1, "bob", "GB"}, {1, "charlie", "US"},
		{2, "alice", "US"}, {2, "bob", "GB"},
		{3, "alice", "US"},
	}
	for _, e := range events {
		occurTime := time.Date(2024, 1, e.day, 12, 0, 0, 0, time.UTC)
		if err := insertAutoEvent(ctx, ch.Conn, projectID, uuid.New().String(), "page_view", e.user, occurTime,
			variantStringMap(map[string]string{"$country": e.country})); err != nil {
			t.Fatalf("seed rollup event: %v", err)
		}
	}
}

// seedMatrixEvents inserts page_view + signup events spread across Jan–Mar 2024
// over several users/countries, so the rollup parity matrix has non-trivial buckets
// at DAY, WEEK, and MONTH granularity and across breakdown values.
func seedMatrixEvents(t *testing.T, ctx context.Context, ch *testutil.TestClickHouse, projectID string) {
	t.Helper()
	events := []struct {
		m, d           int
		kind, user, cc string
	}{
		{1, 1, "page_view", "alice", "US"}, {1, 1, "page_view", "bob", "GB"},
		{1, 2, "page_view", "alice", "US"}, {1, 3, "page_view", "carol", "US"},
		{1, 5, "page_view", "alice", "US"}, {1, 8, "page_view", "bob", "GB"},
		{1, 15, "page_view", "alice", "US"}, {1, 22, "page_view", "bob", "GB"},
		{2, 10, "page_view", "alice", "US"}, {2, 20, "page_view", "carol", "US"},
		{3, 5, "page_view", "bob", "GB"},
		{1, 2, "signup", "alice", "US"}, {1, 5, "signup", "carol", "FR"},
		{1, 15, "signup", "alice", "US"}, {3, 5, "signup", "carol", "FR"},
	}
	for _, e := range events {
		occurTime := time.Date(2024, time.Month(e.m), e.d, 12, 0, 0, 0, time.UTC)
		if err := insertAutoEvent(ctx, ch.Conn, projectID, uuid.New().String(), e.kind, e.user, occurTime,
			variantStringMap(map[string]string{"$country": e.cc})); err != nil {
			t.Fatalf("seed matrix event: %v", err)
		}
	}
}

// assertTrendsParity runs req through ExecuteQuery (rollup) and the raw builder and
// asserts identical flattened output. Empties are flagged so a seed/window mismatch
// can't make the assertion vacuously pass.
func assertTrendsParity(t *testing.T, ctx context.Context, executor *insights.Executor, projectID string, req *insightsv1.QueryRequest) {
	t.Helper()
	resp, err := insights.ExecuteQuery(ctx, executor, projectID, req, time.Now())
	if err != nil {
		t.Fatalf("rollup ExecuteQuery: %v", err)
	}
	rollup := flattenTrendsResp(resp)

	rawQ, err := insights.BuildTrendsQuery(req, projectID)
	if err != nil {
		t.Fatalf("raw BuildTrendsQuery: %v", err)
	}
	rawRows, err := executor.QueryTrends(ctx, projectID, rawQ)
	if err != nil {
		t.Fatalf("raw QueryTrends: %v", err)
	}
	raw, err := flattenTrendsFromRaw(ctx, rawRows, rawQ)
	if err != nil {
		t.Fatalf("flattenTrendsFromRaw: %v", err)
	}

	if !reflect.DeepEqual(rollup, raw) {
		t.Errorf("rollup vs raw mismatch:\nrollup=%v\nraw=%v", rollup, raw)
	}
	if len(rollup) == 0 {
		t.Error("empty result — seed/window mismatch would make this parity check vacuous")
	}
}

// assertSegParity mirrors assertTrendsParity for the scalar segmentation total.
func assertSegParity(t *testing.T, ctx context.Context, executor *insights.Executor, projectID string, req *insightsv1.QueryRequest) {
	t.Helper()
	resp, err := insights.ExecuteQuery(ctx, executor, projectID, req, time.Now())
	if err != nil {
		t.Fatalf("rollup ExecuteQuery: %v", err)
	}
	rollup := resp.GetSegmentation().GetTotal()

	rawQ, err := insights.BuildSegmentationQuery(req, projectID)
	if err != nil {
		t.Fatalf("raw BuildSegmentationQuery: %v", err)
	}
	raw, err := executor.QueryScalar(ctx, projectID, rawQ)
	if err != nil {
		t.Fatalf("raw QueryScalar: %v", err)
	}
	if rollup != raw {
		t.Errorf("rollup=%v raw=%v", rollup, raw)
	}
	if rollup == 0 {
		t.Error("zero total — seed/window mismatch would make this parity check vacuous")
	}
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
		err := insertAutoEvent(ctx, ch.Conn,
			testProjectID,
			uuid.New().String(),
			e.kind,
			e.user,
			occurTime,
			variantStringMap(nil),
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
		err := insertAutoEvent(ctx, ch.Conn,
			testProjectID,
			uuid.New().String(),
			e.kind,
			e.user,
			occurTime,
			variantStringMap(nil),
		)
		if err != nil {
			t.Fatalf("insert retention event: %v", err)
		}
	}
}

// seedIntegrationProfiles inserts profiles and aliases into ClickHouse for profile-filter integration tests.
//
// Profiles (project_id = testProjectID):
//
//	alice   → plan=pro, role=admin   (active, id matches events distinct_id)
//	bob     → plan=free, role=member (active, id matches events distinct_id)
//	deleted → plan=pro, role=admin   (is_deleted=1, must be excluded by soft-delete guard)
//	(charlie has no profile)
//
// Aliases:
//
//	alice_anon → alice (exercises UNION ALL alias branch)
func seedIntegrationProfiles(t *testing.T, ctx context.Context, ch *testutil.TestClickHouse) {
	t.Helper()

	now := time.Now().UTC()
	profiles := []struct {
		id         string
		externalID string
		properties string
		isDeleted  uint8
	}{
		{"alice", "alice_ext", `{"plan":"pro","role":"admin"}`, 0},
		{"bob", "bob_ext", `{"plan":"free","role":"member"}`, 0},
		{"deleted", "deleted_ext", `{"plan":"pro","role":"admin"}`, 1},
	}
	for _, p := range profiles {
		if err := ch.Conn.Exec(ctx,
			`INSERT INTO profiles (id, project_id, external_id, properties, is_deleted, create_time, update_time, insert_time) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			p.id, testProjectID, p.externalID, p.properties, p.isDeleted, now, now, now,
		); err != nil {
			t.Fatalf("insert profile %s: %v", p.id, err)
		}
	}

	// Seed an alias so the UNION ALL branch in profileFilterCondition is exercised.
	if err := ch.Conn.Exec(ctx,
		`INSERT INTO profile_aliases (alias_id, profile_id, external_id, project_id) VALUES (?, ?, ?, ?)`,
		"alice_anon", "alice", "alice_ext", testProjectID,
	); err != nil {
		t.Fatalf("insert alias: %v", err)
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
		err := insertAutoEvent(ctx, ch.Conn,
			testProjectID,
			uuid.New().String(),
			"page_view",
			e.user,
			occurTime,
			variantStringMap(map[string]string{"$country": e.country}),
		)
		if err != nil {
			t.Fatalf("insert event: %v", err)
		}
	}
}

// seedFunnelEventsWithCountry inserts funnel events with $country for breakdown integration tests.
//
// Layout (all Apr 2024, project_id = testProjectID):
//
//	alice(US):   sign_up (Apr 1 10:00) → purchase (Apr 1 12:00)
//	bob(US):     sign_up (Apr 1 10:00)
//	charlie(GB): sign_up (Apr 1 10:00) → purchase (Apr 1 11:00)
func seedFunnelEventsWithCountry(t *testing.T, ctx context.Context, ch *testutil.TestClickHouse) {
	t.Helper()

	type event struct {
		user    string
		kind    string
		hour    int
		country string
	}

	events := []event{
		{"alice", "sign_up", 10, "US"},
		{"alice", "purchase", 12, "US"},
		{"bob", "sign_up", 10, "US"},
		{"charlie", "sign_up", 10, "GB"},
		{"charlie", "purchase", 11, "GB"},
	}

	for _, e := range events {
		occurTime := time.Date(2024, 4, 1, e.hour, 0, 0, 0, time.UTC)
		err := insertAutoEvent(ctx, ch.Conn,
			testProjectID,
			uuid.New().String(),
			e.kind,
			e.user,
			occurTime,
			variantStringMap(map[string]string{"$country": e.country}),
		)
		if err != nil {
			t.Fatalf("insert funnel event: %v", err)
		}
	}
}

// seedRetentionEventsWithCountry inserts retention events with $country for breakdown integration tests.
//
// Layout (all May 2024, project_id = testProjectID):
//
//	May 1: alice(US), bob(GB) sign_up (10:00)
//	May 1: alice(US), bob(GB) login (12:00)
//	May 2: alice(US) login
func seedRetentionEventsWithCountry(t *testing.T, ctx context.Context, ch *testutil.TestClickHouse) {
	t.Helper()

	type event struct {
		user    string
		kind    string
		day     int
		hour    int
		country string
	}

	events := []event{
		{"alice", "sign_up", 1, 10, "US"},
		{"bob", "sign_up", 1, 10, "GB"},
		{"alice", "login", 1, 12, "US"},
		{"bob", "login", 1, 12, "GB"},
		{"alice", "login", 2, 12, "US"},
	}

	for _, e := range events {
		occurTime := time.Date(2024, 5, e.day, e.hour, 0, 0, 0, time.UTC)
		err := insertAutoEvent(ctx, ch.Conn,
			testProjectID,
			uuid.New().String(),
			e.kind,
			e.user,
			occurTime,
			variantStringMap(map[string]string{"$country": e.country}),
		)
		if err != nil {
			t.Fatalf("insert retention event: %v", err)
		}
	}
}

// seedRetentionEventsForOthersBucket inserts retention events with asymmetric start-event
// counts per country, so BreakdownLimit=1 deterministically buckets the smaller group into $others.
//
// Layout (all June 2024, project_id = testProjectID):
//
//	June 1: alice(US), bob(US), charlie(GB) sign_up (10:00)
//	June 1: alice(US), bob(US), charlie(GB) login (12:00)
//	June 2: alice(US) login
//
// Start-event (sign_up) count: US=2, GB=1. With BreakdownLimit=1, US stays, GB → $others.
func seedRetentionEventsForOthersBucket(t *testing.T, ctx context.Context, ch *testutil.TestClickHouse) {
	t.Helper()

	type event struct {
		user    string
		kind    string
		day     int
		hour    int
		country string
	}

	events := []event{
		{"alice", "sign_up", 1, 10, "US"},
		{"bob", "sign_up", 1, 10, "US"},
		{"charlie", "sign_up", 1, 10, "GB"},
		{"alice", "login", 1, 12, "US"},
		{"bob", "login", 1, 12, "US"},
		{"charlie", "login", 1, 12, "GB"},
		{"alice", "login", 2, 12, "US"},
	}

	for _, e := range events {
		occurTime := time.Date(2024, 6, e.day, e.hour, 0, 0, 0, time.UTC)
		err := insertAutoEvent(ctx, ch.Conn,
			testProjectID,
			uuid.New().String(),
			e.kind,
			e.user,
			occurTime,
			variantStringMap(map[string]string{"$country": e.country}),
		)
		if err != nil {
			t.Fatalf("insert retention event: %v", err)
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
		err := insertAutoEvent(ctx, ch.Conn,
			testProjectID,
			uuid.New().String(),
			"purchase",
			e.user,
			occurTime,
			variantStringMap(map[string]string{"$country": e.country}),
		)
		if err != nil {
			t.Fatalf("insert purchase event: %v", err)
		}
	}
}

// seedTopKEvents inserts the top-K fixtures in an isolated Sept 2024 window.
//
// Layout (project_id = testProjectID, all Sep 1–3 2024):
//
//	tk_view (11): $browser chrome×5, safari×3, firefox×2, edge×1
//	tk_click (7): $browser chrome by u1,u2,u3; safari by u4,u5; opera + brave both by dup
//	tk_purchase (5): order_amount — alice 100, alice_ext 50, alice_anon 25 (one
//	  canonical user via seedIntegrationProfiles), bob_ext 120, ghost 80 (no profile)
//	tk_lit (6): label big×3, literal "$others"×2, small×1
func seedTopKEvents(t *testing.T, ctx context.Context, ch *testutil.TestClickHouse) {
	t.Helper()

	type event struct {
		kind   string
		user   string
		auto   map[string]string
		custom map[string]chcol.Variant
	}

	browserEvents := func(kind string, perBrowserUsers map[string][]string) []event {
		var out []event
		for browser, users := range perBrowserUsers {
			for _, u := range users {
				out = append(out, event{kind: kind, user: u, auto: map[string]string{"$browser": browser}})
			}
		}
		return out
	}

	var events []event
	events = append(events, browserEvents("tk_view", map[string][]string{
		"chrome":  {"v1", "v2", "v3", "v1", "v2"},
		"safari":  {"v4", "v5", "v4"},
		"firefox": {"v6", "v6"},
		"edge":    {"v7"},
	})...)
	events = append(events, browserEvents("tk_click", map[string][]string{
		"chrome": {"u1", "u2", "u3"},
		"safari": {"u4", "u5"},
		"opera":  {"dup"},
		"brave":  {"dup"},
	})...)
	for _, p := range []struct {
		user   string
		amount float64
	}{
		{"alice", 100}, {"alice_ext", 50}, {"alice_anon", 25},
		{"bob_ext", 120}, {"ghost", 80},
	} {
		events = append(events, event{
			kind: "tk_purchase", user: p.user,
			custom: map[string]chcol.Variant{"order_amount": chcol.NewVariantWithType(p.amount, "Float64")},
		})
	}
	for _, l := range []struct {
		label string
		n     int
	}{
		{"big", 3}, {"$others", 2}, {"small", 1},
	} {
		for i := 0; i < l.n; i++ {
			events = append(events, event{
				kind: "tk_lit", user: fmt.Sprintf("l_%s_%d", l.label, i),
				custom: map[string]chcol.Variant{"label": chcol.NewVariantWithType(l.label, "String")},
			})
		}
	}

	for i, e := range events {
		occurTime := time.Date(2024, 9, 1+i%3, 12, 0, 0, 0, time.UTC)
		batch, err := ch.Conn.PrepareBatch(ctx, chq.EventsInsertStmt)
		if err != nil {
			t.Fatalf("prepare top k batch: %v", err)
		}
		if err := batch.Append(chq.PrepareEventInsertArgs(
			uuid.New().String(), testProjectID, e.user, e.kind,
			variantStringMap(e.auto), e.custom,
			occurTime, uuid.NewString(),
		)...); err != nil {
			t.Fatalf("append top k event: %v", err)
		}
		if err := batch.Send(); err != nil {
			t.Fatalf("send top k event: %v", err)
		}
	}
}

// seedTopKEdgeEvents inserts top-K edge-case fixtures in an isolated, day-aligned
// Oct 2024 window so the no-scope event_kind test (Sept window) is unaffected.
//
// Layout (project_id = testProjectID, Oct 1–3 2024):
//
//	tk_metric: per-user order_amount for AVG/MIN/MAX $others re-merge —
//	  m1 [10,20], m2 [100], m3 [10,20,30], m4 [50], mnull ["x" non-numeric → NULL]
//	tk_empty:  $browser chrome×3, ""(absent)×2, safari×1 (empty value is a real row)
//	tk_tie:    $browser aaa×2, bbb×2, ccc×1 (equal-value tie-break)
func seedTopKEdgeEvents(t *testing.T, ctx context.Context, ch *testutil.TestClickHouse) {
	t.Helper()

	type event struct {
		kind   string
		user   string
		auto   map[string]string
		custom map[string]chcol.Variant
	}
	var events []event

	floatAmt := func(user string, amt float64) event {
		return event{kind: "tk_metric", user: user,
			custom: map[string]chcol.Variant{"order_amount": chcol.NewVariantWithType(amt, "Float64")}}
	}
	events = append(events,
		floatAmt("m1", 10), floatAmt("m1", 20),
		floatAmt("m2", 100),
		floatAmt("m3", 10), floatAmt("m3", 20), floatAmt("m3", 30),
		floatAmt("m4", 50),
		// Non-numeric metric value → toFloat64OrNull NULL; the all-NULL user must
		// fold into $others contributing nothing (and not collapse MIN to 0).
		event{kind: "tk_metric", user: "mnull",
			custom: map[string]chcol.Variant{"order_amount": chcol.NewVariantWithType("x", "String")}},
	)

	browser := func(kind, user, b string) event {
		e := event{kind: kind, user: user}
		if b != "" {
			e.auto = map[string]string{"$browser": b}
		}
		return e
	}
	// tk_empty: two events with NO $browser form the empty/direct bucket.
	events = append(events,
		browser("tk_empty", "e1", "chrome"), browser("tk_empty", "e2", "chrome"), browser("tk_empty", "e3", "chrome"),
		browser("tk_empty", "e4", ""), browser("tk_empty", "e5", ""),
		browser("tk_empty", "e6", "safari"),
	)
	// tk_tie: aaa and bbb tie at 2 events each.
	events = append(events,
		browser("tk_tie", "t1", "aaa"), browser("tk_tie", "t2", "aaa"),
		browser("tk_tie", "t3", "bbb"), browser("tk_tie", "t4", "bbb"),
		browser("tk_tie", "t5", "ccc"),
	)

	for i, e := range events {
		occurTime := time.Date(2024, 10, 1+i%3, 12, 0, 0, 0, time.UTC)
		batch, err := ch.Conn.PrepareBatch(ctx, chq.EventsInsertStmt)
		if err != nil {
			t.Fatalf("prepare top k edge batch: %v", err)
		}
		if err := batch.Append(chq.PrepareEventInsertArgs(
			uuid.New().String(), testProjectID, e.user, e.kind,
			variantStringMap(e.auto), e.custom,
			occurTime, uuid.NewString(),
		)...); err != nil {
			t.Fatalf("append top k edge event: %v", err)
		}
		if err := batch.Send(); err != nil {
			t.Fatalf("send top k edge event: %v", err)
		}
	}
}

// seedTopKCollisionProfiles seeds a deliberate identity collision: distinct_id
// "collide" resolves to profile pA via its external_id AND to profile pB via an
// alias. The USER identity union's LEFT ANY JOIN must pick ONE canonical id per
// event so the collision cannot multiply event rows. Events live in an isolated
// Nov 2024 window.
func seedTopKCollisionProfiles(t *testing.T, ctx context.Context, ch *testutil.TestClickHouse) {
	t.Helper()
	now := time.Now().UTC()

	for _, p := range []struct{ id, externalID string }{
		{"pA", "collide"}, // external_id collides with pB's alias_id below
		{"pB", "pB_ext"},
	} {
		if err := ch.Conn.Exec(ctx,
			`INSERT INTO profiles (id, project_id, external_id, properties, is_deleted, create_time, update_time, insert_time) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			p.id, testProjectID, p.externalID, "{}", uint8(0), now, now, now,
		); err != nil {
			t.Fatalf("insert collision profile %s: %v", p.id, err)
		}
	}
	if err := ch.Conn.Exec(ctx,
		`INSERT INTO profile_aliases (alias_id, profile_id, external_id, project_id) VALUES (?, ?, ?, ?)`,
		"collide", "pB", "pB_ext", testProjectID,
	); err != nil {
		t.Fatalf("insert collision alias: %v", err)
	}

	// 3 events for the colliding distinct_id + 1 clean user.
	n := 0
	for _, e := range []struct {
		user  string
		count int
	}{{"collide", 3}, {"solo", 1}} {
		for i := 0; i < e.count; i++ {
			occurTime := time.Date(2024, 11, 1+n%3, 12, 0, 0, 0, time.UTC)
			n++
			batch, err := ch.Conn.PrepareBatch(ctx, chq.EventsInsertStmt)
			if err != nil {
				t.Fatalf("prepare collision batch: %v", err)
			}
			if err := batch.Append(chq.PrepareEventInsertArgs(
				uuid.New().String(), testProjectID, e.user, "tk_collide",
				nil, nil, occurTime, uuid.NewString(),
			)...); err != nil {
				t.Fatalf("append collision event: %v", err)
			}
			if err := batch.Send(); err != nil {
				t.Fatalf("send collision event: %v", err)
			}
		}
	}
}

func variantStringMap(props map[string]string) map[string]chcol.Variant {
	if len(props) == 0 {
		return nil
	}
	out := make(map[string]chcol.Variant, len(props))
	for k, v := range props {
		out[k] = chcol.NewVariantWithType(v, "String")
	}
	return out
}

func insertAutoEvent(
	ctx context.Context,
	conn driver.Conn,
	projectID, eventID, kind, distinctID string,
	occurTime time.Time,
	autoProps map[string]chcol.Variant,
) error {
	batch, err := conn.PrepareBatch(ctx, chq.EventsInsertStmt)
	if err != nil {
		return err
	}
	if err := batch.Append(chq.PrepareEventInsertArgs(
		eventID, projectID, distinctID, kind,
		autoProps, nil,
		occurTime, uuid.NewString(),
	)...); err != nil {
		return err
	}
	return batch.Send()
}

// insertSessionEvent inserts one event with an explicit session_id and $url, so
// session integration tests can group multiple events into one session (the
// insertAutoEvent helper randomizes session_id per event and can't). occurTime is
// passed through; $url and $country are set as promoted auto-properties.
func insertSessionEvent(
	ctx context.Context,
	conn driver.Conn,
	projectID, eventID, kind, distinctID, sessionID string,
	occurTime time.Time,
	url, country string,
) error {
	batch, err := conn.PrepareBatch(ctx, chq.EventsInsertStmt)
	if err != nil {
		return err
	}
	props := map[string]string{"$url": url}
	if country != "" {
		props["$country"] = country
	}
	if err := batch.Append(chq.PrepareEventInsertArgs(
		eventID, projectID, distinctID, kind,
		variantStringMap(props), nil,
		occurTime, sessionID,
	)...); err != nil {
		return err
	}
	return batch.Send()
}

// seedUserFlowEvents inserts four sessions with known event-kind sequences for
// user-flow integration tests (see docs/architecture/user-flow.md §9).
func seedUserFlowEvents(t *testing.T, ctx context.Context, ch *testutil.TestClickHouse, projectID string) {
	t.Helper()
	seedUserFlowKindSequences(t, ctx, ch, projectID, map[string][]string{
		"A": {"login", "dashboard", "settings", "logout"},
		"B": {"login", "dashboard", "logout"},
		"C": {"login", "logout"},
		"D": {"login"},
	})
}

// seedUserFlowEventsWithNoise inserts the default sequences with a heartbeat event
// after login in session A to exercise scope filtering.
func seedUserFlowEventsWithNoise(t *testing.T, ctx context.Context, ch *testutil.TestClickHouse, projectID string) {
	t.Helper()
	base := time.Date(2024, 3, 1, 10, 0, 0, 0, time.UTC)
	sessionA := "00000000-0000-0000-0000-0000000000a1"
	steps := []struct {
		kind string
		at   time.Duration
	}{
		{"login", 0},
		{"heartbeat", time.Minute},
		{"dashboard", 2 * time.Minute},
		{"settings", 3 * time.Minute},
		{"logout", 4 * time.Minute},
	}
	for _, step := range steps {
		if err := insertSessionEvent(ctx, ch.Conn, projectID, uuid.New().String(),
			step.kind, "user_A", sessionA, base.Add(step.at), "", ""); err != nil {
			t.Fatalf("seed user flow noise event: %v", err)
		}
	}
	seedUserFlowKindSequences(t, ctx, ch, projectID, map[string][]string{
		"B": {"login", "dashboard", "logout"},
		"C": {"login", "logout"},
		"D": {"login"},
	})
}

// seedUserFlowURLEvents inserts page_view events whose $url property encodes the path.
func seedUserFlowURLEvents(t *testing.T, ctx context.Context, ch *testutil.TestClickHouse, projectID string) {
	t.Helper()
	base := time.Date(2024, 3, 1, 10, 0, 0, 0, time.UTC)
	sessionIDs := map[string]string{
		"A": "00000000-0000-0000-0000-0000000000a1",
		"B": "00000000-0000-0000-0000-0000000000b2",
		"C": "00000000-0000-0000-0000-0000000000c3",
		"D": "00000000-0000-0000-0000-0000000000d4",
	}
	urlSequences := map[string][]string{
		"A": {"/login", "/dashboard", "/settings", "/logout"},
		"B": {"/login", "/dashboard", "/logout"},
		"C": {"/login", "/logout"},
		"D": {"/login"},
	}
	for label, urls := range urlSequences {
		for i, url := range urls {
			if err := insertSessionEvent(ctx, ch.Conn, projectID, uuid.New().String(),
				"page_view", "user_"+label, sessionIDs[label],
				base.Add(time.Duration(i)*time.Minute), url, ""); err != nil {
				t.Fatalf("seed user flow url event: %v", err)
			}
		}
	}
}

func seedUserFlowKindSequences(t *testing.T, ctx context.Context, ch *testutil.TestClickHouse, projectID string, sequences map[string][]string) {
	t.Helper()
	base := time.Date(2024, 3, 1, 10, 0, 0, 0, time.UTC)
	sessionIDs := map[string]string{
		"A": "00000000-0000-0000-0000-0000000000a1",
		"B": "00000000-0000-0000-0000-0000000000b2",
		"C": "00000000-0000-0000-0000-0000000000c3",
		"D": "00000000-0000-0000-0000-0000000000d4",
	}
	for label, kinds := range sequences {
		for i, kind := range kinds {
			if err := insertSessionEvent(ctx, ch.Conn, projectID, uuid.New().String(),
				kind, "user_"+label, sessionIDs[label],
				base.Add(time.Duration(i)*time.Minute), "", ""); err != nil {
				t.Fatalf("seed user flow event: %v", err)
			}
		}
	}
}

func userFlowLinkMap(result *insightsv1.UserFlowResult) map[[2]string]int64 {
	out := map[[2]string]int64{}
	if result == nil {
		return out
	}
	for _, l := range result.GetLinks() {
		out[[2]string{l.GetSource(), l.GetTarget()}] = l.GetValue()
	}
	return out
}

// sessionSeed is one event row for seedSessionEvents.
type sessionSeed struct {
	session string
	user    string
	url     string
	country string
	at      time.Time
}

// seedSessionEvents inserts a fixed set of sessions under a dedicated project so
// session parity assertions are deterministic. Layout (all "page_view", Jan 2024):
//
//	A (alice, US): /landing @ Jan1 10:00 → /checkout @ Jan1 10:05  (2 ev, dur 300s, start Jan1)
//	B (bob, GB):   /home    @ Jan1 11:00                            (1 ev, bounce, start Jan1)
//	C (carol, US): /a @ Jan2 09:00 → /b @ Jan2 09:10 → /c @ Jan2 09:30 (3 ev, dur 1800s, start Jan2)
//	D (dave, US):  /x @ Jan1 23:30 → /y @ Jan2 00:30                (2 ev, dur 3600s, start Jan1)
//
// D straddles the Jan1/Jan2 boundary: it starts Jan1 23:30, so a [Jan2,Jan3) window
// must EXCLUDE it entirely (keyed on start, not clipped) even though it has a Jan2
// event — the property that distinguishes full-session semantics from event clipping.
// Entry≠exit for A (/landing vs /checkout) and D (/x vs /y) exercises argMin vs argMax.
func seedSessionEvents(t *testing.T, ctx context.Context, ch *testutil.TestClickHouse, projectID string) {
	t.Helper()
	jan := func(day, hour, min int) time.Time {
		return time.Date(2024, 1, day, hour, min, 0, 0, time.UTC)
	}
	seed := []sessionSeed{
		{"A", "alice", "/landing", "US", jan(1, 10, 0)},
		{"A", "alice", "/checkout", "US", jan(1, 10, 5)},
		{"B", "bob", "/home", "GB", jan(1, 11, 0)},
		{"C", "carol", "/a", "US", jan(2, 9, 0)},
		{"C", "carol", "/b", "US", jan(2, 9, 10)},
		{"C", "carol", "/c", "US", jan(2, 9, 30)},
		{"D", "dave", "/x", "US", jan(1, 23, 30)},
		{"D", "dave", "/y", "US", jan(2, 0, 30)},
	}
	// Stable session UUIDs derived from the label so re-seeds are deterministic.
	sessionID := map[string]string{
		"A": "00000000-0000-0000-0000-0000000000a1",
		"B": "00000000-0000-0000-0000-0000000000b2",
		"C": "00000000-0000-0000-0000-0000000000c3",
		"D": "00000000-0000-0000-0000-0000000000d4",
	}
	for _, e := range seed {
		if err := insertSessionEvent(ctx, ch.Conn, projectID, uuid.New().String(),
			"page_view", e.user, sessionID[e.session], e.at, e.url, e.country); err != nil {
			t.Fatalf("seed session event: %v", err)
		}
	}
}

// sessionTrendsReq builds a session trends request over [from, to) at day granularity.
func sessionTrendsReq(metric insightsv1.SessionMetric, breakdown string, from, to time.Time) *insightsv1.QueryRequest {
	spec := &insightsv1.InsightQuerySpec{
		InsightType: insightsv1.InsightType_INSIGHT_TYPE_TRENDS.Enum(),
		Session:     &insightsv1.SessionQuery{Metric: metric.Enum()},
	}
	if breakdown != "" {
		spec.Breakdowns = []*insightsv1.Breakdown{{Property: proto.String(breakdown)}}
	}
	return &insightsv1.QueryRequest{
		Spec:        spec,
		TimeRange:   &commonv1.TimeRange{From: timestamppb.New(from), To: timestamppb.New(to)},
		Granularity: insightsv1.Granularity_GRANULARITY_DAY.Enum(),
	}
}

// sessionSegReq builds a session segmentation (scalar) request over [from, to).
func sessionSegReq(metric insightsv1.SessionMetric, from, to time.Time) *insightsv1.QueryRequest {
	return &insightsv1.QueryRequest{
		Spec: &insightsv1.InsightQuerySpec{
			InsightType: insightsv1.InsightType_INSIGHT_TYPE_SEGMENTATION.Enum(),
			Session:     &insightsv1.SessionQuery{Metric: metric.Enum()},
		},
		TimeRange:   &commonv1.TimeRange{From: timestamppb.New(from), To: timestamppb.New(to)},
		Granularity: insightsv1.Granularity_GRANULARITY_DAY.Enum(),
	}
}

// flattenSessionTrends keys a session trends response by "url|date" using the $url
// breakdown (empty when the request has no breakdown).
func flattenSessionTrends(resp *insightsv1.QueryResponse) map[string]float64 {
	out := map[string]float64{}
	for _, s := range resp.GetTrends().GetSeries() {
		bd := s.GetBreakdown()["$url"]
		for _, p := range s.GetPoints() {
			out[bd+"|"+p.GetTime().AsTime().Format("2006-01-02")] = p.GetValue()
		}
	}
	return out
}

// assertSessionTrendsParity runs a session trends req through ExecuteQuery (rollup
// when eligible) and the raw builder, asserting identical flattened output. This is
// the load-bearing check that the rollup and raw paths agree numerically.
func assertSessionTrendsParity(t *testing.T, ctx context.Context, executor *insights.Executor, projectID string, req *insightsv1.QueryRequest) {
	t.Helper()
	resp, err := insights.ExecuteQuery(ctx, executor, projectID, req, time.Now())
	if err != nil {
		t.Fatalf("rollup ExecuteQuery: %v", err)
	}
	rollup := flattenSessionTrends(resp)

	rawQ, err := insights.BuildSessionTrendsQuery(req, projectID)
	if err != nil {
		t.Fatalf("raw BuildSessionTrendsQuery: %v", err)
	}
	rawRows, err := executor.QueryTrends(ctx, projectID, rawQ)
	if err != nil {
		t.Fatalf("raw QueryTrends: %v", err)
	}
	raw, err := flattenSessionTrendsFromRaw(ctx, rawRows, rawQ)
	if err != nil {
		t.Fatalf("flattenSessionTrendsFromRaw: %v", err)
	}
	if !reflect.DeepEqual(rollup, raw) {
		t.Errorf("session trends rollup vs raw mismatch:\nrollup=%v\nraw=%v", rollup, raw)
	}
	if len(rollup) == 0 {
		t.Error("empty result — seed/window mismatch would make this parity check vacuous")
	}
}

// flattenSessionTrendsFromRaw mirrors flattenTrendsFromRaw but keys on $url.
func flattenSessionTrendsFromRaw(ctx context.Context, rows []insights.TrendRow, q insights.TrendsQuery) (map[string]float64, error) {
	series, err := insights.GroupSeries(ctx, rows, q.Properties(), q.BreakdownLimit())
	if err != nil {
		return nil, err
	}
	return flattenSessionTrends(&insightsv1.QueryResponse{
		Result: &insightsv1.QueryResponse_Trends{Trends: &insightsv1.TrendsResult{Series: series}},
	}), nil
}

// assertSessionSegParity mirrors assertSessionTrendsParity for the scalar total.
func assertSessionSegParity(t *testing.T, ctx context.Context, executor *insights.Executor, projectID string, req *insightsv1.QueryRequest) float64 {
	t.Helper()
	resp, err := insights.ExecuteQuery(ctx, executor, projectID, req, time.Now())
	if err != nil {
		t.Fatalf("rollup ExecuteQuery: %v", err)
	}
	rollup := resp.GetSegmentation().GetTotal()

	rawQ, err := insights.BuildSessionSegmentationQuery(req, projectID)
	if err != nil {
		t.Fatalf("raw BuildSessionSegmentationQuery: %v", err)
	}
	raw, err := executor.QueryScalar(ctx, projectID, rawQ)
	if err != nil {
		t.Fatalf("raw QueryScalar: %v", err)
	}
	if rollup != raw {
		t.Errorf("session seg rollup=%v raw=%v", rollup, raw)
	}
	return rollup
}
