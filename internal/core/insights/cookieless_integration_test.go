package insights_test

import (
	"context"
	"fmt"
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

// TestIntegrationRollupBreakdownOrderIndependent is the end-to-end pin for the
// rollup breakdown ranking: a trends query whose events carry DIFFERENT
// aggregations must return the same numbers regardless of the order of the
// `events` array.
//
// The shared top_vals CTE keyed its cookieless predicate off events[0] alone
// while each branch keyed off its own aggregation, so reordering `events`
// swapped the ranking population out from under every series. The corpus makes
// that swap maximally visible: C01..C10 carry only cookieless traffic (high
// event volume, zero consented users) while C11/C12 carry only consented
// traffic (low volume, real users). Ranked over all traffic the top-10 is
// C01..C10; ranked over consented-only it is C11/C12 — disjoint, so a
// regression cannot squeak through on a tie.
//
// Note WHAT gets corrupted: the page_view series is TOTAL and carries no
// cookieless predicate in either ordering, so "TOTAL always counts all traffic"
// still holds in aggregate. It broke only in the breakdown decomposition, which
// is why an assertion on totals alone would not catch it.
func TestIntegrationRollupBreakdownOrderIndependent(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ch := testutil.SetupClickHouse(t)
	ctx := context.Background()
	executor := insights.NewExecutor(ch.Conn)
	const projectID = "proj_breakdown_order"

	day := time.Date(2024, 1, 2, 12, 0, 0, 0, time.UTC)
	seed := func(country, distinctID, kind string, n int) {
		t.Helper()
		for i := range n {
			if err := insertAutoEvent(ctx, ch.Conn, projectID, uuid.NewString(), kind, distinctID,
				day.Add(time.Duration(i)*time.Minute),
				variantStringMap(map[string]string{"$country": country}),
			); err != nil {
				t.Fatalf("seed %s/%s: %v", country, kind, err)
			}
		}
	}
	for i := 1; i <= 10; i++ {
		c := fmt.Sprintf("C%02d", i)
		seed(c, cookieless.IDPrefix+"v"+c, "page_view", 3)
		seed(c, cookieless.IDPrefix+"v"+c, "signup", 1)
	}
	for _, c := range []string{"C11", "C12"} {
		seed(c, "anon-"+c, "page_view", 1)
		seed(c, "anon-"+c, "signup", 1)
	}

	pv := &insightsv1.EventQuery{
		Event:       &commonv1.EventFilter{Kind: proto.String("page_view")},
		Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL.Enum(),
	}
	su := &insightsv1.EventQuery{
		Event:       &commonv1.EventFilter{Kind: proto.String("signup")},
		Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_UNIQUE_USERS.Enum(),
	}
	req := func(events ...*insightsv1.EventQuery) *insightsv1.QueryRequest {
		return &insightsv1.QueryRequest{
			Spec: &insightsv1.InsightQuerySpec{
				InsightType: insightsv1.InsightType_INSIGHT_TYPE_TRENDS.Enum(),
				Events:      events,
				Breakdowns:  []*insightsv1.Breakdown{{Property: proto.String("$country")}},
			},
			TimeRange: &commonv1.TimeRange{
				From: timestamppb.New(time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)),
				To:   timestamppb.New(time.Date(2024, 1, 4, 0, 0, 0, 0, time.UTC)),
			},
			Granularity: insightsv1.Granularity_GRANULARITY_DAY.Enum(),
		}
	}

	// totals maps "<kind>|<breakdown value>" to the series total. Values, not
	// mere series names: a multi-event response also carries zero-filled cells
	// synthesized for every (kind, breakdown) pair, so comparing name sets would
	// compare the union of both kinds' top-Ns rather than each kind's own.
	totals := func(r *insightsv1.QueryRequest) map[string]float64 {
		t.Helper()
		resp, err := insights.ExecuteQuery(ctx, executor, projectID, r, time.Now())
		if err != nil {
			t.Fatalf("ExecuteQuery: %v", err)
		}
		out := map[string]float64{}
		for _, s := range resp.GetTrends().GetSeries() {
			var sum float64
			for _, p := range s.GetPoints() {
				sum += p.GetValue()
			}
			out[s.GetEventKind()+"|"+s.GetBreakdown()["$country"]] += sum
		}
		return out
	}

	forward := totals(req(pv, su))
	reversed := totals(req(su, pv))

	if !reflect.DeepEqual(forward, reversed) {
		t.Fatalf("breakdown results depend on the order of `events`:\n  [page_view,signup] = %v\n  [signup,page_view] = %v",
			forward, reversed)
	}

	// Positive checks, so the equality above cannot pass by both sides being
	// equally wrong. page_view's $others is the discriminator: ranked over ALL
	// traffic only C11+C12 fold into it (1+1=2). Had it ranked over the
	// consented-only population — what events[0]=UNIQUE_USERS used to impose —
	// C01..C10 would fold instead and $others would total 30.
	if got := forward["page_view|$others"]; got != 2 {
		t.Errorf("page_view $others = %v, want 2 (only consented-only C11+C12 fold); "+
			"30 means it ranked over the consented population instead of all traffic", got)
	}
	if got := forward["page_view|C01"]; got != 3 {
		t.Errorf("page_view C01 = %v, want 3 (named by volume across all traffic)", got)
	}
	// signup is UNIQUE_USERS, so its own ranking excludes cookieless: the
	// consented-only countries are the ones it names.
	if got := forward["signup|C11"]; got != 1 {
		t.Errorf("signup C11 = %v, want 1 (consented user named by the UNIQUE_USERS ranking)", got)
	}
	if got := forward["signup|C01"]; got != 0 {
		t.Errorf("signup C01 = %v, want 0 (cookieless-only country has no consented users)", got)
	}
}
