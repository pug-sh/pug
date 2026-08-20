package insights_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/pug-sh/pug/internal/core/insights"
	commonv1 "github.com/pug-sh/pug/internal/gen/proto/common/v1"
	insightsv1 "github.com/pug-sh/pug/internal/gen/proto/shared/insights/v1"
	"github.com/pug-sh/pug/internal/testutil"
)

// A person split across an anon id and their external_id must read as one
// person to funnel, retention, funnel timing, top K and profile filters.
// `whole` is the control throughout — same behavior under one id, so a failure
// on it means the seed is wrong, not the stitching.
func TestIdentityResolution(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ch := testutil.SetupClickHouse(t)
	ctx := context.Background()
	seedIdentitySplit(t, ctx, ch)
	executor := insights.NewExecutor(ch.Conn)

	t.Run("funnel", func(t *testing.T) {
		req := &insightsv1.QueryRequest{
			Spec: &insightsv1.InsightQuerySpec{
				InsightType: insightsv1.InsightType_INSIGHT_TYPE_FUNNEL.Enum(),
				Events: []*insightsv1.EventQuery{
					{Event: &commonv1.EventFilter{Kind: proto.String("ir_landing")}, Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL.Enum()},
					{Event: &commonv1.EventFilter{Kind: proto.String("ir_add_to_cart")}, Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL.Enum()},
					{Event: &commonv1.EventFilter{Kind: proto.String("ir_purchase")}, Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL.Enum()},
				},
			},
			TimeRange: &commonv1.TimeRange{
				From: timestamppb.New(time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC)),
				To:   timestamppb.New(time.Date(2024, 6, 2, 0, 0, 0, 0, time.UTC)),
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
		if rows[0].Value != 2 {
			t.Errorf("step 0 (ir_landing): expected 2 users, got %v", rows[0].Value)
		}
		if rows[1].Value != 2 {
			t.Errorf("step 1 (ir_add_to_cart): expected 2 users, got %v", rows[1].Value)
		}
		if rows[2].Value != 2 {
			t.Errorf("step 2 (ir_purchase): expected 2 users, got %v — split's purchase under "+
				"\"split_ext\" must join the earlier \"anon-split\" steps into one windowFunnel "+
				"chain", rows[2].Value)
		}
	})

	t.Run("retention", func(t *testing.T) {
		req := &insightsv1.QueryRequest{
			Spec: &insightsv1.InsightQuerySpec{
				InsightType: insightsv1.InsightType_INSIGHT_TYPE_RETENTION.Enum(),
				Events: []*insightsv1.EventQuery{
					{Event: &commonv1.EventFilter{Kind: proto.String("ir_signup")}, Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL.Enum()},
					{Event: &commonv1.EventFilter{Kind: proto.String("ir_login")}, Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL.Enum()},
				},
			},
			TimeRange: &commonv1.TimeRange{
				From: timestamppb.New(time.Date(2024, 6, 10, 0, 0, 0, 0, time.UTC)),
				To:   timestamppb.New(time.Date(2024, 6, 14, 0, 0, 0, 0, time.UTC)),
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
		if len(series) == 0 || len(series[0].Cohorts) == 0 {
			t.Fatal("expected at least 1 retention cohort")
		}

		cohort := series[0].Cohorts[0]
		if cohort.GetCohortSize() != 2 {
			t.Fatalf("expected cohort size 2 (split + whole signed up Jun 10), got %v", cohort.GetCohortSize())
		}
		// Points carries only buckets that had returns, so index by date, not position.
		dayOne := -1.0
		for _, pt := range cohort.Points {
			if pt.GetTime().AsTime().UTC().Day() == 11 {
				dayOne = pt.GetValue()
			}
		}
		if dayOne < 0 {
			t.Fatalf("no Jun 11 retention point in %d points", len(cohort.Points))
		}
		if dayOne != 100 {
			t.Errorf("day-1 retention: expected 100%%, got %v%% — split enters the cohort as "+
				"\"anon-split\" and returns as \"split_ext\"; both must resolve to one user_key", dayOne)
		}
	})

	t.Run("funnel_timing", func(t *testing.T) {
		req := &insightsv1.QueryRequest{
			Spec: &insightsv1.InsightQuerySpec{
				InsightType:       insightsv1.InsightType_INSIGHT_TYPE_FUNNEL.Enum(),
				IncludeStepTiming: proto.Bool(true),
				Events: []*insightsv1.EventQuery{
					{Event: &commonv1.EventFilter{Kind: proto.String("ir_landing")}, Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL.Enum()},
					{Event: &commonv1.EventFilter{Kind: proto.String("ir_add_to_cart")}, Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL.Enum()},
					{Event: &commonv1.EventFilter{Kind: proto.String("ir_purchase")}, Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL.Enum()},
				},
			},
			TimeRange: &commonv1.TimeRange{
				From: timestamppb.New(time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC)),
				To:   timestamppb.New(time.Date(2024, 6, 2, 0, 0, 0, 0, time.UTC)),
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

		// Count rows reaching the final step rather than len(users): the timing
		// pre-filter already drops anyone matching under two steps, so an unstitched
		// split_ext is absent and the row count looks correct while the person is not.
		finalStep := int64(len(req.GetSpec().GetEvents()) - 1)
		reachedFinal := 0
		for _, u := range users {
			for _, m := range u.StepMatches {
				if m == finalStep {
					reachedFinal++
					break
				}
			}
		}
		if reachedFinal != 2 {
			t.Errorf("expected 2 users reaching ir_purchase, got %d — split's purchase sits "+
				"under \"split_ext\", whose own chain matches one step; only the stitched "+
				"key reaches the final step", reachedFinal)
		}
	})

	t.Run("topk_user_resolves", func(t *testing.T) {
		rows := runIdentityTopK(t, ctx, executor, false)

		got, ok := topKValue(rows, "split")
		if !ok {
			t.Fatalf("no top-K row for profile \"split\"; got %v", rows)
		}
		if got != 2 {
			t.Errorf("profile \"split\": expected 2 ir_views (anon-split + split_ext), got %v", got)
		}
	})

	t.Run("topk_cookieless_never_merges", func(t *testing.T) {
		rows := runIdentityTopK(t, ctx, executor, true)

		if got, ok := topKValue(rows, "cookieless-abc"); !ok || got != 1 {
			t.Errorf("cookieless-abc: expected its own row with 1 event, got %v (present=%v) — "+
				"a cookieless id must never resolve onto a person", got, ok)
		}
		if got, ok := topKValue(rows, "split"); !ok || got != 2 {
			t.Errorf("profile \"split\": expected 2 with cookieless included, got %v (present=%v) — "+
				"cookieless traffic must not be absorbed into a profile", got, ok)
		}
	})

	t.Run("profile_filter", func(t *testing.T) {
		req := &insightsv1.QueryRequest{
			Spec: &insightsv1.InsightQuerySpec{
				InsightType: insightsv1.InsightType_INSIGHT_TYPE_TRENDS.Enum(),
				Events: []*insightsv1.EventQuery{
					{Event: &commonv1.EventFilter{Kind: proto.String("ir_view")}, Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL.Enum()},
				},
				FilterGroups: planProGroups(),
			},
			TimeRange: &commonv1.TimeRange{
				From: timestamppb.New(time.Date(2024, 6, 20, 0, 0, 0, 0, time.UTC)),
				To:   timestamppb.New(time.Date(2024, 6, 21, 0, 0, 0, 0, time.UTC)),
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
		// 3 pro events (anon-split, split_ext, whole_ext); other_ext is plan=free.
		// A count of 4 would mean the filter stopped filtering.
		if total != 3 {
			t.Errorf("plan=pro ir_view: expected 3 events, got %v — the filter must match "+
				"distinct_id against the full identity set, external_id included", total)
		}
	})

	// A profile that never identified has an empty external_id, but its own id
	// and its aliases are still distinct_ids events carry, so a profile filter
	// must reach them. Only the external_id branch may take the non-empty guard.
	t.Run("anonymous_profile_filter_resolves", func(t *testing.T) {
		run := func(t *testing.T, groups []*insightsv1.FilterGroup) float64 {
			t.Helper()
			req := &insightsv1.QueryRequest{
				Spec: &insightsv1.InsightQuerySpec{
					InsightType: insightsv1.InsightType_INSIGHT_TYPE_TRENDS.Enum(),
					Events: []*insightsv1.EventQuery{
						{Event: &commonv1.EventFilter{Kind: proto.String("ira_view")}, Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL.Enum()},
					},
					FilterGroups: groups,
				},
				TimeRange: &commonv1.TimeRange{
					From: timestamppb.New(time.Date(2024, 6, 22, 0, 0, 0, 0, time.UTC)),
					To:   timestamppb.New(time.Date(2024, 6, 23, 0, 0, 0, 0, time.UTC)),
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
			return total
		}

		if got := run(t, nil); got != 3 {
			t.Fatalf("unfiltered ira_view: expected 3 (anon-only + anon-alias + anon-free), got %v — seed is wrong", got)
		}

		// 2 = the profile's own id + its alias. 0 would mean the external_id
		// guard excluded the profile outright; 3 would mean no filtering.
		if got := run(t, planProGroups()); got != 2 {
			t.Errorf("plan=pro ira_view: expected 2 events, got %v — a profile with no "+
				"external_id must still resolve by profile id and by alias", got)
		}
	})

	// A profile filter and the identity union must agree on which events are a
	// person's: split's two steps sit under different ids, and both must pass
	// plan=pro for the chain to survive the filter.
	t.Run("funnel_profile_filter", func(t *testing.T) {
		funnelReq := func(groups []*insightsv1.FilterGroup) *insightsv1.QueryRequest {
			return &insightsv1.QueryRequest{
				Spec: &insightsv1.InsightQuerySpec{
					InsightType: insightsv1.InsightType_INSIGHT_TYPE_FUNNEL.Enum(),
					Events: []*insightsv1.EventQuery{
						{Event: &commonv1.EventFilter{Kind: proto.String("irf_landing")}, Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL.Enum()},
						{Event: &commonv1.EventFilter{Kind: proto.String("irf_purchase")}, Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL.Enum()},
					},
					FilterGroups: groups,
				},
				TimeRange: &commonv1.TimeRange{
					From: timestamppb.New(time.Date(2024, 6, 2, 0, 0, 0, 0, time.UTC)),
					To:   timestamppb.New(time.Date(2024, 6, 3, 0, 0, 0, 0, time.UTC)),
				},
			}
		}

		run := func(t *testing.T, groups []*insightsv1.FilterGroup) []insights.FunnelRow {
			t.Helper()
			q, err := insights.BuildFunnelCountsQuery(funnelReq(groups), testProjectID)
			if err != nil {
				t.Fatalf("BuildFunnelCountsQuery: %v", err)
			}
			rows, err := executor.QueryFunnel(ctx, testProjectID, q)
			if err != nil {
				t.Fatalf("QueryFunnel: %v", err)
			}
			if len(rows) != 2 {
				t.Fatalf("expected 2 funnel steps, got %d", len(rows))
			}
			return rows
		}

		unfiltered := run(t, nil)
		if unfiltered[1].Value != 2 {
			t.Fatalf("unfiltered step 1: expected 2 (split + other), got %v — seed is wrong", unfiltered[1].Value)
		}

		filtered := run(t, planProGroups())
		if filtered[0].Value != 1 || filtered[1].Value != 1 {
			t.Errorf("plan=pro funnel: expected 1/1 (split only), got %v/%v — the filter must match "+
				"both anon-split and split_ext, and only those", filtered[0].Value, filtered[1].Value)
		}
	})

	t.Run("retention_profile_filter", func(t *testing.T) {
		retentionReq := func(groups []*insightsv1.FilterGroup) *insightsv1.QueryRequest {
			return &insightsv1.QueryRequest{
				Spec: &insightsv1.InsightQuerySpec{
					InsightType: insightsv1.InsightType_INSIGHT_TYPE_RETENTION.Enum(),
					Events: []*insightsv1.EventQuery{
						{Event: &commonv1.EventFilter{Kind: proto.String("irf_signup")}, Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL.Enum()},
						{Event: &commonv1.EventFilter{Kind: proto.String("irf_login")}, Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL.Enum()},
					},
					FilterGroups: groups,
				},
				TimeRange: &commonv1.TimeRange{
					From: timestamppb.New(time.Date(2024, 6, 12, 0, 0, 0, 0, time.UTC)),
					To:   timestamppb.New(time.Date(2024, 6, 16, 0, 0, 0, 0, time.UTC)),
				},
				Granularity: insightsv1.Granularity_GRANULARITY_DAY.Enum(),
			}
		}

		run := func(t *testing.T, groups []*insightsv1.FilterGroup) *insightsv1.RetentionCohort {
			t.Helper()
			q, err := insights.BuildRetentionQuery(retentionReq(groups), testProjectID)
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
			if len(series) == 0 || len(series[0].Cohorts) == 0 {
				t.Fatal("expected at least 1 retention cohort")
			}
			return series[0].Cohorts[0]
		}

		if got := run(t, nil).GetCohortSize(); got != 2 {
			t.Fatalf("unfiltered cohort: expected 2 (split + other), got %v — seed is wrong", got)
		}

		cohort := run(t, planProGroups())
		if cohort.GetCohortSize() != 1 {
			t.Errorf("plan=pro cohort: expected 1 (split only), got %v", cohort.GetCohortSize())
		}
		dayOne := -1.0
		for _, pt := range cohort.Points {
			if pt.GetTime().AsTime().UTC().Day() == 13 {
				dayOne = pt.GetValue()
			}
		}
		if dayOne != 100 {
			t.Errorf("plan=pro day-1 retention: expected 100%%, got %v%% — split's irf_login under "+
				"split_ext must pass the filter and rejoin the anon-split cohort entry", dayOne)
		}
	})

	// A soft-deleted profile drops out of identity_union, so its ids must rank
	// as themselves — a tombstoned person must not reassemble under erasure.
	t.Run("soft_deleted_profile_never_resolves", func(t *testing.T) {
		rows := runIdentityTopKScope(t, ctx, executor, false, "ird_view", 21)

		if _, ok := topKValue(rows, "gone"); ok {
			t.Errorf("profile \"gone\" is soft-deleted and must not appear as a top-K key: %v", rows)
		}
		for _, id := range []string{"anon-gone", "gone_ext"} {
			if got, ok := topKValue(rows, id); !ok || got != 1 {
				t.Errorf("%s: expected its own row with 1 event, got %v (present=%v)", id, got, ok)
			}
		}
	})
}

// planProGroups is the PROPERTY_SOURCE_PROFILE plan=pro filter, which compiles
// to distinct_id IN (ids ∪ external_ids ∪ alias ids of matching profiles).
func planProGroups() []*insightsv1.FilterGroup {
	return []*insightsv1.FilterGroup{{
		Filters: []*commonv1.PropertyFilter{{
			Property: proto.String("plan"),
			Operator: commonv1.FilterOperator_FILTER_OPERATOR_EQUALS.Enum(),
			Value:    proto.String("pro"),
			Source:   commonv1.PropertySource_PROPERTY_SOURCE_PROFILE.Enum(),
		}},
	}}
}

func runIdentityTopK(t *testing.T, ctx context.Context, executor *insights.Executor, includeCookieless bool) []insights.TopKRow {
	t.Helper()
	return runIdentityTopKScope(t, ctx, executor, includeCookieless, "ir_view", 20)
}

func runIdentityTopKScope(t *testing.T, ctx context.Context, executor *insights.Executor, includeCookieless bool, kind string, day int) []insights.TopKRow {
	t.Helper()

	req := &insightsv1.QueryRequest{
		Spec: &insightsv1.InsightQuerySpec{
			InsightType:       insightsv1.InsightType_INSIGHT_TYPE_TOP_K.Enum(),
			IncludeCookieless: proto.Bool(includeCookieless),
			TopK: &insightsv1.TopKQuery{
				Dimension:  insightsv1.TopKQuery_DIMENSION_USER.Enum(),
				Scope:      &commonv1.EventFilter{Kind: proto.String(kind)},
				Limit:      proto.Int32(10),
				OmitOthers: proto.Bool(true),
			},
		},
		TimeRange: &commonv1.TimeRange{
			From: timestamppb.New(time.Date(2024, 6, day, 0, 0, 0, 0, time.UTC)),
			To:   timestamppb.New(time.Date(2024, 6, day+1, 0, 0, 0, 0, time.UTC)),
		},
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

func topKValue(rows []insights.TopKRow, dimension string) (float64, bool) {
	for _, r := range rows {
		if r.DimensionValue == dimension {
			return r.Value, true
		}
	}
	return 0, false
}

// Layout (project_id = testProjectID, June 2024). `split` is one person across
// two ids; `whole` is the same behavior under one id; `other` is plan=free;
// `gone` is soft-deleted, so its ids must stay unresolved; `anon-only` never
// identified, so it owns no external_id at all.
//
//	profiles     split/split_ext (pro), whole/whole_ext (pro), other/other_ext (free),
//	             gone/gone_ext (pro, is_deleted=1), anon-only (pro, no external_id),
//	             anon-free (free, no external_id)
//	aliases      anon-split → split, anon-gone → gone, anon-alias → anon-only
//	Jun 1        anon-split: ir_landing, ir_add_to_cart | split_ext: ir_purchase
//	             whole_ext:  all three
//	Jun 2        anon-split: irf_landing | split_ext: irf_purchase
//	             other_ext:  irf_landing, irf_purchase
//	Jun 10/11    anon-split: ir_signup | split_ext: ir_login
//	             whole_ext:  ir_signup, ir_login
//	Jun 12/13    anon-split: irf_signup | split_ext: irf_login
//	             other_ext:  irf_signup, irf_login
//	Jun 20       anon-split, split_ext, whole_ext, other_ext, cookieless-abc: ir_view
//	Jun 21       anon-gone, gone_ext: ird_view
//	Jun 22       anon-only, anon-alias, anon-free: ira_view
func seedIdentitySplit(t *testing.T, ctx context.Context, ch *testutil.TestClickHouse) {
	t.Helper()

	now := time.Now().UTC()
	profiles := []struct {
		id, externalID, properties string
		deleted                    uint8
	}{
		{"split", "split_ext", `{"plan":"pro"}`, 0},
		{"whole", "whole_ext", `{"plan":"pro"}`, 0},
		{"other", "other_ext", `{"plan":"free"}`, 0},
		{"gone", "gone_ext", `{"plan":"pro"}`, 1},
		{"anon-only", "", `{"plan":"pro"}`, 0},
		{"anon-free", "", `{"plan":"free"}`, 0},
	}
	for _, p := range profiles {
		if err := ch.Conn.Exec(ctx,
			`INSERT INTO profiles (id, project_id, external_id, properties, is_deleted, create_time, update_time, insert_time) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			p.id, testProjectID, p.externalID, p.properties, p.deleted, now, now, now,
		); err != nil {
			t.Fatalf("insert profile %s: %v", p.id, err)
		}
	}

	aliases := [][3]string{
		{"anon-split", "split", "split_ext"},
		{"anon-gone", "gone", "gone_ext"},
		{"anon-alias", "anon-only", ""},
	}
	for _, a := range aliases {
		if err := ch.Conn.Exec(ctx,
			`INSERT INTO profile_aliases (alias_id, profile_id, external_id, project_id) VALUES (?, ?, ?, ?)`,
			a[0], a[1], a[2], testProjectID,
		); err != nil {
			t.Fatalf("insert alias %s: %v", a[0], err)
		}
	}

	events := []struct {
		distinctID string
		kind       string
		day, hour  int
	}{
		{"anon-split", "ir_landing", 1, 10},
		{"anon-split", "ir_add_to_cart", 1, 11},
		{"split_ext", "ir_purchase", 1, 12},
		{"whole_ext", "ir_landing", 1, 10},
		{"whole_ext", "ir_add_to_cart", 1, 11},
		{"whole_ext", "ir_purchase", 1, 12},

		{"anon-split", "ir_signup", 10, 10},
		{"split_ext", "ir_login", 11, 10},
		{"whole_ext", "ir_signup", 10, 10},
		{"whole_ext", "ir_login", 11, 10},

		{"anon-split", "ir_view", 20, 10},
		{"split_ext", "ir_view", 20, 11},
		{"whole_ext", "ir_view", 20, 10},
		{"other_ext", "ir_view", 20, 10},
		{"cookieless-abc", "ir_view", 20, 10},

		// plan=pro is split (stitched across both ids) and not other.
		{"anon-split", "irf_landing", 2, 10},
		{"split_ext", "irf_purchase", 2, 11},
		{"other_ext", "irf_landing", 2, 10},
		{"other_ext", "irf_purchase", 2, 11},

		{"anon-split", "irf_signup", 12, 10},
		{"split_ext", "irf_login", 13, 10},
		{"other_ext", "irf_signup", 12, 10},
		{"other_ext", "irf_login", 13, 10},

		{"anon-gone", "ird_view", 21, 10},
		{"gone_ext", "ird_view", 21, 11},

		{"anon-only", "ira_view", 22, 10},
		{"anon-alias", "ira_view", 22, 11},
		{"anon-free", "ira_view", 22, 10},
	}
	for _, e := range events {
		if err := insertAutoEvent(ctx, ch.Conn,
			testProjectID,
			uuid.New().String(),
			e.kind,
			e.distinctID,
			time.Date(2024, 6, e.day, e.hour, 0, 0, 0, time.UTC),
			variantStringMap(nil),
		); err != nil {
			t.Fatalf("insert event %s/%s: %v", e.distinctID, e.kind, err)
		}
	}
}
