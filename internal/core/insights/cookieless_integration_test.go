package insights_test

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/pug-sh/pug/internal/cookieless"
	"github.com/pug-sh/pug/internal/core/insights"
	commonv1 "github.com/pug-sh/pug/internal/gen/proto/common/v1"
	insightsv1 "github.com/pug-sh/pug/internal/gen/proto/shared/insights/v1"
	"github.com/pug-sh/pug/internal/testutil"
)

// clTrendsReq mirrors rollupParityTrendsReq's day-aligned window without a
// breakdown: one page_view event with the given aggregation over 2024-01-01..04.
func clTrendsReq(agg insightsv1.AggregationType, include bool) *insightsv1.QueryRequest {
	return &insightsv1.QueryRequest{
		Spec: &insightsv1.InsightQuerySpec{
			InsightType:       insightsv1.InsightType_INSIGHT_TYPE_TRENDS.Enum(),
			Events:            []*insightsv1.EventQuery{{Event: &commonv1.EventFilter{Kind: proto.String("page_view")}, Aggregation: agg.Enum()}},
			IncludeCookieless: proto.Bool(include),
		},
		TimeRange:   &commonv1.TimeRange{From: timestamppb.New(time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)), To: timestamppb.New(time.Date(2024, 1, 4, 0, 0, 0, 0, time.UTC))},
		Granularity: insightsv1.Granularity_GRANULARITY_DAY.Enum(),
	}
}

func sumTrendsValues(resp *insightsv1.QueryResponse) float64 {
	var sum float64
	for _, s := range resp.GetTrends().GetSeries() {
		for _, p := range s.GetPoints() {
			sum += p.GetValue()
		}
	}
	return sum
}

// TestIntegrationCookieless proves the exclusion end to end through real
// ClickHouse (migration 011 applied by testutil): the derived counts, the
// toggle, and rollup↔raw parity in both toggle states.
func TestIntegrationCookieless(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ch := testutil.SetupClickHouse(t)
	ctx := context.Background()
	executor := insights.NewExecutor(ch.Conn)
	const projectID = "proj_cookieless"

	// Seed on 2024-01-02: consented anon-u1 (2 events) + anon-u2 (1 event),
	// cookieless visitors v1 + v2 (1 event each). All page_view.
	day := time.Date(2024, 1, 2, 12, 0, 0, 0, time.UTC)
	for i, id := range []string{"anon-u1", "anon-u1", "anon-u2",
		cookieless.IDPrefix + "v1", cookieless.IDPrefix + "v2"} {
		if err := insertAutoEvent(ctx, ch.Conn, projectID, uuid.NewString(),
			"page_view", id, day.Add(time.Duration(i)*time.Minute), nil); err != nil {
			t.Fatalf("seed cookieless corpus: %v", err)
		}
	}

	uu := insightsv1.AggregationType_AGGREGATION_TYPE_UNIQUE_USERS

	t.Run("unique_users_default_excludes", func(t *testing.T) {
		resp, err := insights.ExecuteQuery(ctx, executor, projectID, clTrendsReq(uu, false), time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if got := sumTrendsValues(resp); got != 2 {
			t.Errorf("UNIQUE_USERS = %v, want 2 (cookieless visitors excluded)", got)
		}
	})

	t.Run("unique_users_toggle_includes", func(t *testing.T) {
		resp, err := insights.ExecuteQuery(ctx, executor, projectID, clTrendsReq(uu, true), time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if got := sumTrendsValues(resp); got != 4 {
			t.Errorf("UNIQUE_USERS = %v, want 4 (2 users + 2 cookieless day-ids)", got)
		}
	})

	t.Run("total_always_includes", func(t *testing.T) {
		resp, err := insights.ExecuteQuery(ctx, executor, projectID,
			clTrendsReq(insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL, false), time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if got := sumTrendsValues(resp); got != 5 {
			t.Errorf("TOTAL = %v, want 5 (all events count)", got)
		}
	})

	t.Run("per_user_avg_consistent_population", func(t *testing.T) {
		resp, err := insights.ExecuteQuery(ctx, executor, projectID,
			clTrendsReq(insightsv1.AggregationType_AGGREGATION_TYPE_PER_USER_AVG, false), time.Now())
		if err != nil {
			t.Fatal(err)
		}
		// Consented only: 3 events / 2 users = 1.5 — a mixed-population ratio
		// (5/2 or 5/4) would prove numerator and denominator diverged.
		if got := sumTrendsValues(resp); got != 1.5 {
			t.Errorf("PER_USER_AVG = %v, want 1.5 (3 consented events / 2 consented users)", got)
		}
	})

	t.Run("rollup_raw_parity_both_toggles", func(t *testing.T) {
		for _, include := range []bool{false, true} {
			req := clTrendsReq(uu, include)
			resp, err := insights.ExecuteQuery(ctx, executor, projectID, req, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			rollup := flattenTrendsResp(resp)

			rawQ, err := insights.BuildTrendsQuery(req, projectID)
			if err != nil {
				t.Fatal(err)
			}
			rawRows, err := executor.QueryTrends(ctx, projectID, rawQ)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := flattenTrendsFromRaw(ctx, rawRows, rawQ)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(rollup, raw) {
				t.Errorf("include=%v rollup vs raw mismatch:\nrollup=%v\nraw=%v", include, rollup, raw)
			}
		}
	})

	t.Run("funnel_excludes_cookieless_sequences", func(t *testing.T) {
		// One consented and one cookieless visitor each complete
		// view_item -> buy_item within the window.
		for _, id := range []string{"anon-funnel", cookieless.IDPrefix + "vf"} {
			for i, kind := range []string{"view_item", "buy_item"} {
				if err := insertAutoEvent(ctx, ch.Conn, projectID, uuid.NewString(),
					kind, id, day.Add(time.Duration(i)*time.Minute), nil); err != nil {
					t.Fatalf("seed funnel events: %v", err)
				}
			}
		}
		req := &insightsv1.QueryRequest{
			Spec: &insightsv1.InsightQuerySpec{
				InsightType: insightsv1.InsightType_INSIGHT_TYPE_FUNNEL.Enum(),
				Events: []*insightsv1.EventQuery{
					{Event: &commonv1.EventFilter{Kind: proto.String("view_item")}},
					{Event: &commonv1.EventFilter{Kind: proto.String("buy_item")}},
				},
			},
			TimeRange: &commonv1.TimeRange{From: timestamppb.New(time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)), To: timestamppb.New(time.Date(2024, 1, 4, 0, 0, 0, 0, time.UTC))},
		}

		q, err := insights.BuildFunnelCountsQuery(req, projectID)
		if err != nil {
			t.Fatal(err)
		}
		rows, err := executor.QueryFunnel(ctx, projectID, q)
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 2 {
			t.Fatalf("expected 2 funnel steps, got %d", len(rows))
		}
		if rows[0].Value != 1 || rows[1].Value != 1 {
			t.Errorf("funnel steps = (%v, %v), want (1, 1): only the consented visitor sequences", rows[0].Value, rows[1].Value)
		}

		req.Spec.IncludeCookieless = proto.Bool(true)
		q, err = insights.BuildFunnelCountsQuery(req, projectID)
		if err != nil {
			t.Fatal(err)
		}
		rows, err = executor.QueryFunnel(ctx, projectID, q)
		if err != nil {
			t.Fatal(err)
		}
		if rows[0].Value != 2 || rows[1].Value != 2 {
			t.Errorf("included funnel steps = (%v, %v), want (2, 2)", rows[0].Value, rows[1].Value)
		}
	})
}
