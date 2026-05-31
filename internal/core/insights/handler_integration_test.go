package insights_test

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/authn"
	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/pug-sh/pug/internal/app/server/rpc"
	insightshandler "github.com/pug-sh/pug/internal/app/server/rpc/shared/insights"
	"github.com/pug-sh/pug/internal/core/insights"
	commonv1 "github.com/pug-sh/pug/internal/gen/proto/common/v1"
	insightsv1 "github.com/pug-sh/pug/internal/gen/proto/shared/insights/v1"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	"github.com/pug-sh/pug/internal/testutil"
)

// TestIntegration_FunnelHandlerIncludeStepTimingDispatch verifies the handler-level
// dispatch through ExecuteQuery — the `if req.GetSpec().GetIncludeStepTiming()` branch must
// route to the timing-aware path (parallel windowFunnel counts + ComputeFunnelTiming merge).
// A regression that drops the check (or wires the wrong builder) would silently downgrade
// timing requests to the counts-only path, returning zero medians/p95s and an empty
// distribution. Earlier core-package integration tests bypass the handler and so cannot
// catch such a dispatch error.
func TestIntegration_FunnelHandlerIncludeStepTimingDispatch(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ch := testutil.SetupClickHouse(t)
	rd := testutil.SetupRedis(t)
	ctx := context.Background()

	seedFunnelEvents(t, ctx, ch)

	executor := insights.NewExecutor(ch.Conn)
	service := insights.NewService(executor, rd.Client)
	srv := insightshandler.NewServer(service, executor)

	principal := &rpc.Principal{Project: &dbread.Project{ID: testProjectID}}
	authedCtx := authn.SetInfo(ctx, principal)

	makeReq := func(includeTiming bool) *connect.Request[insightsv1.QueryRequest] {
		return connect.NewRequest(&insightsv1.QueryRequest{
			Spec: &insightsv1.InsightQuerySpec{
				InsightType:       insightsv1.InsightType_INSIGHT_TYPE_FUNNEL.Enum(),
				IncludeStepTiming: proto.Bool(includeTiming),
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
		})
	}

	t.Run("include_step_timing_true_populates_distribution", func(t *testing.T) {
		resp, err := srv.Query(authedCtx, makeReq(true))
		if err != nil {
			t.Fatalf("Query: %v", err)
		}

		series := resp.Msg.GetFunnel().GetSeries()
		if len(series) != 1 {
			t.Fatalf("expected 1 series, got %d", len(series))
		}
		steps := series[0].GetSteps()
		if len(steps) != 3 {
			t.Fatalf("expected 3 steps, got %d", len(steps))
		}

		// Step 0 (entry): timing sub-message must be absent.
		if steps[0].GetTiming() != nil {
			t.Errorf("step 0 timing: expected nil (entry step), got %+v", steps[0].GetTiming())
		}

		// Step 1 has converters (alice, bob) — timing must be present with the full 8-bucket
		// histogram and a positive median. If the dispatch silently routed to the counts-only
		// path, Timing would be nil.
		timing := steps[1].GetTiming()
		if timing == nil {
			t.Fatal("step 1 timing: expected non-nil (timing path not dispatched?)")
		}
		if got := len(timing.GetDistribution()); got != 8 {
			t.Errorf("step 1 distribution: got len=%d, want 8", got)
		}
		if median := timing.GetMedian().AsDuration(); median <= 0 {
			t.Errorf("step 1 median: got %v, want > 0", median)
		}
		if p95 := timing.GetP95().AsDuration(); p95 <= 0 {
			t.Errorf("step 1 p95: got %v, want > 0", p95)
		}

		// Presence-aware bucket shape: finite buckets carry UpperBound, the open-ended last
		// bucket has UpperBound absent. Verifies the proto-translation contract end-to-end
		// at the dispatch path (not just at the unit-level GroupFunnelSeries test).
		buckets := timing.GetDistribution()
		if buckets[0].UpperBound == nil {
			t.Errorf("bucket 0 (finite): UpperBound should be set")
		}
		if buckets[7].UpperBound != nil {
			t.Errorf("bucket 7 (open-ended): UpperBound should be absent, got %v", buckets[7].GetUpperBound().AsDuration())
		}
	})

	t.Run("include_step_timing_false_omits_timing", func(t *testing.T) {
		resp, err := srv.Query(authedCtx, makeReq(false))
		if err != nil {
			t.Fatalf("Query: %v", err)
		}

		series := resp.Msg.GetFunnel().GetSeries()
		if len(series) != 1 {
			t.Fatalf("expected 1 series, got %d", len(series))
		}
		steps := series[0].GetSteps()
		if len(steps) != 3 {
			t.Fatalf("expected 3 steps, got %d", len(steps))
		}

		// All steps must have an absent timing sub-message when timing is disabled.
		// If a regression always took the timing path, Timing would be populated.
		for i, s := range steps {
			if s.GetTiming() != nil {
				t.Errorf("step %d timing: expected nil (timing disabled), got %+v", i, s.GetTiming())
			}
		}
	})
}

func TestIntegration_UserFlowHandler(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ch := testutil.SetupClickHouse(t)
	rd := testutil.SetupRedis(t)
	ctx := context.Background()

	const userFlowProjectID = "proj_user_flow_handler"
	seedUserFlowEvents(t, ctx, ch, userFlowProjectID)

	executor := insights.NewExecutor(ch.Conn)
	service := insights.NewService(executor, rd.Client)
	srv := insightshandler.NewServer(service, executor)

	principal := &rpc.Principal{Project: &dbread.Project{ID: userFlowProjectID}}
	authedCtx := authn.SetInfo(ctx, principal)

	resp, err := srv.Query(authedCtx, connect.NewRequest(&insightsv1.QueryRequest{
		Spec: &insightsv1.InsightQuerySpec{
			InsightType: insightsv1.InsightType_INSIGHT_TYPE_USER_FLOW.Enum(),
			UserFlow:    &insightsv1.UserFlowQuery{},
		},
		TimeRange: &commonv1.TimeRange{
			From: timestamppb.New(time.Date(2024, 3, 1, 0, 0, 0, 0, time.UTC)),
			To:   timestamppb.New(time.Date(2024, 3, 2, 0, 0, 0, 0, time.UTC)),
		},
		Granularity: insightsv1.Granularity_GRANULARITY_DAY.Enum(),
	}))
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	result := resp.Msg.GetUserFlow()
	if result == nil {
		t.Fatal("expected UserFlow result")
	}
	if len(result.GetLinks()) != 5 {
		t.Fatalf("expected 5 links, got %d", len(result.GetLinks()))
	}
}

// TestIntegration_GetFilterSchemaHandlerForwardsAllowedTypes verifies the
// handler reads req.Msg.GetAllowedTypes() and forwards it to the service. The
// service-level test (TestServiceGetFilterSchema/allowed_types_filters_custom_keys)
// exercises the same path but bypasses the handler — a regression that drops
// the field forwarding (e.g., passing nil) would silently disable filtering on
// every dashboard request and the service-level test would not catch it.
func TestIntegration_GetFilterSchemaHandlerForwardsAllowedTypes(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.Background()
	ch := testutil.SetupClickHouse(t)
	rd := testutil.SetupRedis(t)
	pg := testutil.SetupPostgres(t)

	projectID := seedTestProject(t, ctx, pg)
	seedServiceEvents(t, ctx, ch, projectID)

	executor := insights.NewExecutor(ch.Conn)
	service := insights.NewService(executor, rd.Client)
	srv := insightshandler.NewServer(service, executor)

	authedCtx := authn.SetInfo(ctx, &rpc.Principal{Project: &dbread.Project{ID: projectID}})

	// INTEGER+FLOAT filter: seedServiceEvents seeds Float64 (load_time, revenue)
	// and Int64 (user_id) custom properties. If the handler forwards
	// allowed_types, the response must contain only those keys; if it drops
	// the field, the response includes Bool/String/DateTime keys too.
	resp, err := srv.GetFilterSchema(authedCtx, connect.NewRequest(&commonv1.GetFilterSchemaRequest{
		AllowedTypes: []commonv1.PropertyValueType{
			commonv1.PropertyValueType_PROPERTY_VALUE_TYPE_INTEGER,
			commonv1.PropertyValueType_PROPERTY_VALUE_TYPE_FLOAT,
		},
	}))
	if err != nil {
		t.Fatalf("GetFilterSchema: %v", err)
	}

	got := map[string]bool{}
	for _, k := range resp.Msg.GetCustomPropertyKeys() {
		got[k.GetName()] = true
	}
	for _, name := range []string{"load_time", "user_id", "revenue"} {
		if !got[name] {
			t.Errorf("expected %q in INTEGER+FLOAT-filtered response, got keys: %v (handler may not be forwarding allowed_types)", name, got)
		}
	}
	for _, name := range []string{"is_cached", "plan_name", "shipped_at", "coupon"} {
		if got[name] {
			t.Errorf("did not expect %q in INTEGER+FLOAT-filtered response — handler may be passing nil for allowed_types", name)
		}
	}
}
