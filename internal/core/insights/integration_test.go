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

		series, err := insights.GroupSeries(ctx, rows, q.Properties())
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

		series, err := insights.GroupSeries(ctx, rows, q.Properties())
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

		series, err := insights.GroupSeries(ctx, rows, q.Properties())
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
		// Uses same seed data from funnel_counts.
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

		q, err := insights.BuildFunnelTimingQuery(req, testProjectID)
		if err != nil {
			t.Fatalf("BuildFunnelTimingQuery: %v", err)
		}

		users, err := executor.QueryFunnelUserEvents(ctx, testProjectID, q)
		if err != nil {
			t.Fatalf("QueryFunnelUserEvents: %v", err)
		}

		rows, err := insights.ComputeFunnelTiming(ctx, "", users, q.Kinds(), q.WindowSec(), q.NumBreakdowns())
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

		series, err := insights.GroupRetentionSeries(ctx, rows, nil)
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

		series, err := insights.GroupFunnelSeries(ctx, rows, q.Properties())
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

		series, err := insights.GroupRetentionSeries(ctx, rows, q.Properties())
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

		series, err := insights.GroupFunnelSeries(ctx, rows, q.Properties())
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
		// Uses same seed data from funnel_counts_with_breakdown.
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

		q, err := insights.BuildFunnelTimingQuery(req, testProjectID)
		if err != nil {
			t.Fatalf("BuildFunnelTimingQuery: %v", err)
		}

		users, err := executor.QueryFunnelUserEvents(ctx, testProjectID, q)
		if err != nil {
			t.Fatalf("QueryFunnelUserEvents: %v", err)
		}

		funnelRows, err := insights.ComputeFunnelTiming(ctx, "", users, q.Kinds(), q.WindowSec(), q.NumBreakdowns())
		if err != nil {
			t.Fatalf("ComputeFunnelTiming: %v", err)
		}

		series, err := insights.GroupFunnelSeries(ctx, funnelRows, q.Properties())
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

		series, err := insights.GroupRetentionSeries(ctx, rows, q.Properties())
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
		raw := flattenTrendsRows(rawRows)

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
		raw := flattenTrendsRows(rawRows)

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
		raw := flattenTrendsRows(rawRows)

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
		// Pins the accepted C1 tradeoff: the rollup over-counts duplicate event
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
				// stays rollup-eligible after the R2-B window-alignment guard; a mid-day
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

	t.Run("rollup_parity_trends_multi_event_breakdown", func(t *testing.T) {
		// R2-D: two event kinds + a breakdown exercises the shared top_vals CTE,
		// which is built over Or(kind...) for all kinds and attached to only the
		// first UNION ALL branch yet referenced by every branch. Parity with the raw
		// builder proves the cross-branch CTE reference and per-kind grouping are
		// correct — the case single-event parity tests never reach.
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
		raw := flattenTrendsRows(rawRows)

		if !reflect.DeepEqual(rollup, raw) {
			t.Errorf("multi-event breakdown rollup vs raw mismatch:\nrollup=%v\nraw=%v", rollup, raw)
		}
		// Sanity: both kinds present, US page_view spans days 1+2.
		if rollup["page_view|US|2024-01-01"] != 1 || rollup["signup|GB|2024-01-02"] != 1 {
			t.Errorf("unexpected multi-event values: %v", rollup)
		}
	})

	t.Run("rollup_parity_trends_others_bucket", func(t *testing.T) {
		// R2-E: more breakdown values than breakdown_limit forces the rollup's top-N
		// + '$others' collapse (top_vals ... LIMIT n, then if(dim_value IN top_vals,
		// dim_value, '$others')). The rollup picks top-N from pre-summed daily cnt
		// while raw picks from rows; parity proves they bucket identically.
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
		raw := flattenTrendsRows(rawRows)

		if !reflect.DeepEqual(rollup, raw) {
			t.Errorf("$others rollup vs raw mismatch:\nrollup=%v\nraw=%v", rollup, raw)
		}
		// FR (below the top-2 cut) must collapse into a single $others bucket.
		if rollup["page_view|$others|2024-01-01"] != 1 {
			t.Errorf("expected FR collapsed into $others=1, got %v (all: %v)", rollup["page_view|$others|2024-01-01"], rollup)
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

func flattenTrendsRows(rows []insights.TrendRow) map[string]float64 {
	out := map[string]float64{}
	for _, r := range rows {
		bd := ""
		if len(r.Breakdowns) > 0 {
			bd = r.Breakdowns[0]
		}
		out[r.EventKind+"|"+bd+"|"+r.Time.Format("2006-01-02")] = r.Value
	}
	return out
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
	raw := flattenTrendsRows(rawRows)

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
	batch, err := conn.PrepareBatch(ctx,
		"INSERT INTO events (project_id, event_id, kind, distinct_id, occur_time, auto_properties)")
	if err != nil {
		return err
	}
	if err := batch.Append(projectID, eventID, kind, distinctID, occurTime, autoProps); err != nil {
		return err
	}
	return batch.Send()
}
