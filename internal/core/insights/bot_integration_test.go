package insights_test

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/chcol"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/pug-sh/pug/internal/core/insights"
	commonv1 "github.com/pug-sh/pug/internal/gen/proto/common/v1"
	insightsv1 "github.com/pug-sh/pug/internal/gen/proto/shared/insights/v1"
	"github.com/pug-sh/pug/internal/testutil"
)

// botWindow is the day-aligned UTC window every query below uses, so trends,
// top K and sessions ride the rollup fast path and the raw builders are run
// explicitly for parity.
func botWindow() *commonv1.TimeRange {
	return &commonv1.TimeRange{
		From: timestamppb.New(time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)),
		To:   timestamppb.New(time.Date(2024, 1, 4, 0, 0, 0, 0, time.UTC)),
	}
}

func botTrendsReq(agg insightsv1.AggregationType, include bool) *insightsv1.QueryRequest {
	return &insightsv1.QueryRequest{
		Spec: &insightsv1.InsightQuerySpec{
			InsightType: insightsv1.InsightType_INSIGHT_TYPE_TRENDS.Enum(),
			Events:      []*insightsv1.EventQuery{{Event: &commonv1.EventFilter{Kind: proto.String("page_view")}, Aggregation: agg.Enum()}},
			IncludeBots: proto.Bool(include),
		},
		TimeRange:   botWindow(),
		Granularity: insightsv1.Granularity_GRANULARITY_DAY.Enum(),
	}
}

func botSessionsReq(metric insightsv1.SessionMetric, include bool) *insightsv1.QueryRequest {
	return &insightsv1.QueryRequest{
		Spec: &insightsv1.InsightQuerySpec{
			InsightType: insightsv1.InsightType_INSIGHT_TYPE_TRENDS.Enum(),
			Session:     &insightsv1.SessionQuery{Metric: metric.Enum()},
			IncludeBots: proto.Bool(include),
		},
		TimeRange:   botWindow(),
		Granularity: insightsv1.Granularity_GRANULARITY_DAY.Enum(),
	}
}

// TestIntegrationBotTagging proves migration 012 and the include_bots toggle
// end to end through real ClickHouse: the promoted bot columns round-trip
// through the insert path, both rollups key on bot, each query shape below
// excludes tagged traffic by default and re-admits it on the toggle, rollup
// and raw agree in both states, and a session is judged whole.
func TestIntegrationBotTagging(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ch := testutil.SetupClickHouse(t)
	ctx := context.Background()
	executor := insights.NewExecutor(ch.Conn)
	const projectID = "proj_bot"

	web := chcol.NewVariantWithType("web", "String")
	human := map[string]chcol.Variant{"$platform": web}
	bot := map[string]chcol.Variant{
		"$platform":   web,
		"$bot":        chcol.NewVariantWithType(true, "Bool"),
		"$bot_reason": chcol.NewVariantWithType("HeadlessChrome", "String"),
	}
	day := time.Date(2024, 1, 2, 12, 0, 0, 0, time.UTC)
	for i, v := range []struct {
		id    string
		props map[string]chcol.Variant
	}{{"anon-u1", human}, {"anon-u2", human}, {"anon-monitor", bot}} {
		if err := insertAutoEvent(ctx, ch.Conn, projectID, uuid.NewString(), "page_view", v.id,
			day.Add(time.Duration(i)*time.Minute), v.props); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	total := insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL
	uu := insightsv1.AggregationType_AGGREGATION_TYPE_UNIQUE_USERS

	trendsSum := func(t *testing.T, req *insightsv1.QueryRequest) float64 {
		t.Helper()
		resp, err := insights.ExecuteQuery(ctx, executor, projectID, req, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		return sumTrendsValues(resp)
	}
	t.Run("events_columns_round_trip", func(t *testing.T) {
		rows, err := ch.Conn.Query(ctx,
			"SELECT distinct_id, bot, bot_reason FROM events WHERE project_id = ? ORDER BY distinct_id", projectID)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = rows.Close() }()
		got := map[string]string{}
		for rows.Next() {
			var id, reason string
			var flag bool
			if err := rows.Scan(&id, &flag, &reason); err != nil {
				t.Fatal(err)
			}
			if flag {
				got[id] = reason
			} else {
				got[id] = "human:" + reason
			}
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		want := map[string]string{"anon-u1": "human:", "anon-u2": "human:", "anon-monitor": "HeadlessChrome"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("events bot columns = %v, want %v", got, want)
		}
	})

	t.Run("event_rollup_keyed_by_bot", func(t *testing.T) {
		got := countByBot(ctx, t, ch.Conn,
			"SELECT bot, sum(cnt) FROM dashboard_event_rollup_daily WHERE project_id = ? AND dim_name = '$__total__' GROUP BY bot", projectID)
		if want := map[uint8]uint64{0: 2, 1: 1}; !reflect.DeepEqual(got, want) {
			t.Errorf("$__total__ cnt by bot = %v, want %v", got, want)
		}
	})

	t.Run("session_rollup_keyed_by_bot", func(t *testing.T) {
		for _, kind := range []string{"", "page_view"} {
			got := countByBot(ctx, t, ch.Conn,
				"SELECT bot, count(DISTINCT session_id) FROM dashboard_session_rollup WHERE project_id = ? AND kind = ? GROUP BY bot", projectID, kind)
			if want := map[uint8]uint64{0: 2, 1: 1}; !reflect.DeepEqual(got, want) {
				t.Errorf("kind=%q sessions by bot = %v, want %v", kind, got, want)
			}
		}
	})

	t.Run("trends_exclude_by_default_toggle_includes", func(t *testing.T) {
		for _, agg := range []insightsv1.AggregationType{total, uu} {
			if got := trendsSum(t, botTrendsReq(agg, false)); got != 2 {
				t.Errorf("%s default = %v, want 2 (the monitor is excluded)", agg, got)
			}
			if got := trendsSum(t, botTrendsReq(agg, true)); got != 3 {
				t.Errorf("%s include_bots = %v, want 3", agg, got)
			}
		}
	})

	t.Run("trends_rollup_raw_parity_both_toggles", func(t *testing.T) {
		for _, agg := range []insightsv1.AggregationType{total, uu} {
			for _, include := range []bool{false, true} {
				assertTrendsParity(t, ctx, executor, projectID, botTrendsReq(agg, include))
			}
		}
	})

	t.Run("top_k_and_funnel_exclude_bot_people", func(t *testing.T) {
		for _, include := range []bool{false, true} {
			want := 2.0
			if include {
				want = 3
			}
			topK := &insightsv1.QueryRequest{
				Spec: &insightsv1.InsightQuerySpec{
					InsightType: insightsv1.InsightType_INSIGHT_TYPE_TOP_K.Enum(),
					TopK:        &insightsv1.TopKQuery{Dimension: insightsv1.TopKQuery_DIMENSION_EVENT_KIND.Enum()},
					IncludeBots: proto.Bool(include),
				},
				TimeRange: botWindow(),
			}
			resp, err := insights.ExecuteQuery(ctx, executor, projectID, topK, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			if rows := resp.GetTopK().GetRows(); len(rows) != 1 || rows[0].GetValue() != want {
				t.Errorf("include_bots=%v top K rows = %v, want one page_view row of %v", include, rows, want)
			}
			rawQ, err := insights.BuildTopKQuery(topK, projectID)
			if err != nil {
				t.Fatal(err)
			}
			rawRows, err := executor.QueryTopK(ctx, projectID, rawQ)
			if err != nil {
				t.Fatal(err)
			}
			if len(rawRows) != 1 || rawRows[0].Value != want {
				t.Errorf("include_bots=%v raw top K rows = %v, want one page_view row of %v", include, rawRows, want)
			}

			funnel := &insightsv1.QueryRequest{
				Spec: &insightsv1.InsightQuerySpec{
					InsightType: insightsv1.InsightType_INSIGHT_TYPE_FUNNEL.Enum(),
					Events:      []*insightsv1.EventQuery{{Event: &commonv1.EventFilter{Kind: proto.String("page_view")}}},
					IncludeBots: proto.Bool(include),
				},
				TimeRange: botWindow(),
			}
			q, err := insights.BuildFunnelCountsQuery(funnel, projectID)
			if err != nil {
				t.Fatal(err)
			}
			steps, err := executor.QueryFunnel(ctx, projectID, q)
			if err != nil {
				t.Fatal(err)
			}
			if len(steps) != 1 || steps[0].Value != want {
				t.Errorf("include_bots=%v funnel steps = %v, want one step of %v", include, steps, want)
			}
		}
	})

	// A session that straddles the tag — two human-looking hits, then a tagged
	// one (client-supplied $platform, a per-request ASN, or a deploy mid-session
	// all produce this). One tagged event excludes the session.
	t.Run("sessions_judged_whole", func(t *testing.T) {
		sid := uuid.NewString()
		for i, props := range []map[string]chcol.Variant{human, human, bot} {
			if err := insertEventInSession(ctx, ch.Conn, projectID, uuid.NewString(), "page_view", "anon-straddle", sid,
				day.Add(time.Duration(10+i)*time.Minute), props); err != nil {
				t.Fatalf("seed straddling session: %v", err)
			}
		}

		sessions := insightsv1.SessionMetric_SESSION_METRIC_SESSIONS
		bounce := insightsv1.SessionMetric_SESSION_METRIC_BOUNCE_RATE
		if got := trendsSum(t, botSessionsReq(sessions, false)); got != 2 {
			t.Errorf("SESSIONS default = %v, want 2: a row-level predicate would keep the untagged hits as a third session", got)
		}
		if got := trendsSum(t, botSessionsReq(sessions, true)); got != 4 {
			t.Errorf("SESSIONS include_bots = %v, want 4", got)
		}
		// A row-level predicate would keep the straddler's two human hits as a
		// non-bouncing third session and report 66.7.
		if got := trendsSum(t, botSessionsReq(bounce, false)); got != 100 {
			t.Errorf("BOUNCE_RATE default = %v, want 100", got)
		}
		for _, metric := range []insightsv1.SessionMetric{
			sessions, bounce,
			insightsv1.SessionMetric_SESSION_METRIC_AVG_DURATION,
			insightsv1.SessionMetric_SESSION_METRIC_AVG_EVENTS_PER_SESSION,
			insightsv1.SessionMetric_SESSION_METRIC_ENTRY,
			insightsv1.SessionMetric_SESSION_METRIC_EXIT,
		} {
			t.Run(metric.String(), func(t *testing.T) {
				for _, include := range []bool{false, true} {
					assertSessionTrendsParity(t, ctx, executor, projectID, botSessionsReq(metric, include))
				}
			})
		}

		// The straddler is the only multi-event session, so the only source of
		// links; judged whole, the default graph is empty.
		for _, include := range []bool{false, true} {
			req := &insightsv1.QueryRequest{
				Spec: &insightsv1.InsightQuerySpec{
					InsightType: insightsv1.InsightType_INSIGHT_TYPE_USER_FLOW.Enum(),
					UserFlow:    &insightsv1.UserFlowQuery{},
					IncludeBots: proto.Bool(include),
				},
				TimeRange: botWindow(),
			}
			resp, err := insights.ExecuteQuery(ctx, executor, projectID, req, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			links := resp.GetUserFlow().GetLinks()
			if !include && len(links) != 0 {
				t.Errorf("default user flow links = %v, want none (the straddling session is excluded whole)", links)
			}
			if include && (len(links) != 2 || links[0].GetValue() != 1 || links[1].GetValue() != 1) {
				t.Errorf("include_bots user flow links = %v, want the straddler's two page_view→page_view hops of 1", links)
			}
		}
	})
}

func countByBot(ctx context.Context, t *testing.T, conn driver.Conn, query string, args ...any) map[uint8]uint64 {
	t.Helper()
	rows, err := conn.Query(ctx, query, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	out := map[uint8]uint64{}
	for rows.Next() {
		var flag uint8
		var n uint64
		if err := rows.Scan(&flag, &n); err != nil {
			t.Fatal(err)
		}
		out[flag] = n
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}
