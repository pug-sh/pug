package insights_test

import (
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	insights "github.com/pug-sh/pug/internal/core/insights"
	commonv1 "github.com/pug-sh/pug/internal/gen/proto/common/v1"
	insightsv1 "github.com/pug-sh/pug/internal/gen/proto/shared/insights/v1"
)

const cookielessNeedle = "NOT startsWith(distinct_id, 'cookieless-')"

// cookielessTestReq builds a minimal valid trends request with one page_view
// event carrying the given aggregation, over a day-aligned 7-day UTC window.
func cookielessTestReq(agg insightsv1.AggregationType) *insightsv1.QueryRequest {
	return &insightsv1.QueryRequest{
		Spec: &insightsv1.InsightQuerySpec{
			InsightType: insightsv1.InsightType_INSIGHT_TYPE_TRENDS.Enum(),
			Events: []*insightsv1.EventQuery{{
				Event:       &commonv1.EventFilter{Kind: proto.String("page_view")},
				Aggregation: agg.Enum(),
			}},
		},
		TimeRange:   timeRange("2026-07-13T00:00:00Z", "2026-07-20T00:00:00Z"),
		Granularity: insightsv1.Granularity_GRANULARITY_DAY.Enum(),
	}
}

func TestCookielessExclusion_RawBuilders(t *testing.T) {
	uu := insightsv1.AggregationType_AGGREGATION_TYPE_UNIQUE_USERS

	t.Run("trends_unique_users_default_excludes", func(t *testing.T) {
		q, err := insights.BuildTrendsQuery(cookielessTestReq(uu), "p1")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(q.SQL(), cookielessNeedle) {
			t.Errorf("SQL must exclude cookieless:\n%s", q.SQL())
		}
	})

	t.Run("trends_total_default_includes", func(t *testing.T) {
		q, err := insights.BuildTrendsQuery(cookielessTestReq(insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL), "p1")
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(q.SQL(), cookielessNeedle) {
			t.Errorf("TOTAL must not exclude cookieless:\n%s", q.SQL())
		}
	})

	t.Run("trends_toggle_includes", func(t *testing.T) {
		req := cookielessTestReq(uu)
		req.Spec.IncludeCookieless = proto.Bool(true)
		q, err := insights.BuildTrendsQuery(req, "p1")
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(q.SQL(), cookielessNeedle) {
			t.Errorf("include_cookieless=true must lift the exclusion:\n%s", q.SQL())
		}
	})

	t.Run("multi_event_mixed_scopes_per_branch", func(t *testing.T) {
		// TOTAL + UNIQUE_USERS in one scan: the exclusion must live inside the
		// uniqIf condition only — exactly one occurrence, and the TOTAL branch's
		// countIf stays clean.
		req := cookielessTestReq(insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL)
		req.Spec.Events = append(req.Spec.Events, &insightsv1.EventQuery{
			Event:       &commonv1.EventFilter{Kind: proto.String("signup")},
			Aggregation: uu.Enum(),
		})
		q, err := insights.BuildTrendsQuery(req, "p1")
		if err != nil {
			t.Fatal(err)
		}
		sql := q.SQL()
		if !strings.Contains(sql, "uniqIf") {
			t.Fatalf("expected single-scan multi-event query with uniqIf:\n%s", sql)
		}
		if got := strings.Count(sql, cookielessNeedle); got != 1 {
			t.Errorf("exclusion must appear exactly once (inside the uniqIf condition), got %d:\n%s", got, sql)
		}
		uniqIfPos := strings.Index(sql, "uniqIf")
		needlePos := strings.Index(sql, cookielessNeedle)
		if needlePos < uniqIfPos {
			t.Errorf("exclusion must be inside the uniqIf condition, not before it:\n%s", sql)
		}
	})

	t.Run("segmentation_unique_users_excludes", func(t *testing.T) {
		req := cookielessTestReq(uu)
		req.Spec.InsightType = insightsv1.InsightType_INSIGHT_TYPE_SEGMENTATION.Enum()
		q, err := insights.BuildSegmentationQuery(req, "p1")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(q.SQL(), cookielessNeedle) {
			t.Errorf("segmentation UNIQUE_USERS must exclude cookieless:\n%s", q.SQL())
		}
	})

	t.Run("funnel_excludes_by_default", func(t *testing.T) {
		req := &insightsv1.QueryRequest{
			Spec: &insightsv1.InsightQuerySpec{
				InsightType: insightsv1.InsightType_INSIGHT_TYPE_FUNNEL.Enum(),
				Events: []*insightsv1.EventQuery{
					{Event: &commonv1.EventFilter{Kind: proto.String("page_view")}},
					{Event: &commonv1.EventFilter{Kind: proto.String("purchase")}},
				},
			},
			TimeRange: timeRange("2026-07-13T00:00:00Z", "2026-07-20T00:00:00Z"),
		}
		q, err := insights.BuildFunnelCountsQuery(req, "p1")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(q.SQL(), cookielessNeedle) {
			t.Errorf("funnel must exclude cookieless by default:\n%s", q.SQL())
		}

		req.Spec.IncludeCookieless = proto.Bool(true)
		q, err = insights.BuildFunnelCountsQuery(req, "p1")
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(q.SQL(), cookielessNeedle) {
			t.Errorf("toggle must lift funnel exclusion:\n%s", q.SQL())
		}
	})

	// buildFunnelWithTiming is a SEPARATE entry point from the counts builder,
	// taken whenever include_step_timing is set, and it scans events TWICE: the
	// windowFunnel pre-filter (built only when len(steps) >= 2) and the tagged CTE.
	// Both need the predicate, so this asserts the count rather than mere presence
	// — covering one scan and not the other leaves cookieless events in the arrays
	// the timing percentiles are computed from.
	//
	// Untested until round 3: deleting BOTH predicates left the whole insights
	// suite green. The shipping bug was that one consent-rejecting visitor who
	// enters a funnel before midnight and converts after gets a new daily id, so
	// they appear as two people — an instant converter and a never-converter —
	// skewing p50/p90 toward zero and inflating step counts, while the SAME funnel
	// with the timing checkbox off returned the correct population. One toggle,
	// two answers.
	t.Run("funnel_timing_excludes_both_scans", func(t *testing.T) {
		req := &insightsv1.QueryRequest{
			Spec: &insightsv1.InsightQuerySpec{
				InsightType: insightsv1.InsightType_INSIGHT_TYPE_FUNNEL.Enum(),
				Events: []*insightsv1.EventQuery{
					{Event: &commonv1.EventFilter{Kind: proto.String("page_view")}},
					{Event: &commonv1.EventFilter{Kind: proto.String("purchase")}},
				},
				IncludeStepTiming: proto.Bool(true),
			},
			TimeRange: timeRange("2026-07-13T00:00:00Z", "2026-07-20T00:00:00Z"),
		}
		q, err := insights.BuildFunnelTimingQuery(req, "p1")
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Count(q.SQL(), cookielessNeedle); got != 2 {
			t.Errorf("funnel timing must exclude cookieless in BOTH scans (pre-filter + tagged CTE), got %d:\n%s", got, q.SQL())
		}

		req.Spec.IncludeCookieless = proto.Bool(true)
		q, err = insights.BuildFunnelTimingQuery(req, "p1")
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(q.SQL(), cookielessNeedle) {
			t.Errorf("toggle must lift funnel timing exclusion in both scans:\n%s", q.SQL())
		}
	})

	t.Run("retention_excludes_both_scans", func(t *testing.T) {
		req := &insightsv1.QueryRequest{
			Spec: &insightsv1.InsightQuerySpec{
				InsightType: insightsv1.InsightType_INSIGHT_TYPE_RETENTION.Enum(),
				Events: []*insightsv1.EventQuery{
					{Event: &commonv1.EventFilter{Kind: proto.String("page_view")}},
				},
			},
			TimeRange:   timeRange("2026-07-13T00:00:00Z", "2026-07-20T00:00:00Z"),
			Granularity: insightsv1.Granularity_GRANULARITY_DAY.Enum(),
		}
		q, err := insights.BuildRetentionQuery(req, "p1")
		if err != nil {
			t.Fatal(err)
		}
		sql := q.SQL()
		if !strings.Contains(sql, cookielessNeedle) {
			t.Errorf("retention cohort scan must exclude cookieless:\n%s", sql)
		}
		if !strings.Contains(sql, "NOT startsWith(e.distinct_id, 'cookieless-')") {
			t.Errorf("retention retained scan must exclude via the e. alias:\n%s", sql)
		}
	})

	t.Run("user_flow_excludes_by_default", func(t *testing.T) {
		req := &insightsv1.QueryRequest{
			Spec: &insightsv1.InsightQuerySpec{
				InsightType: insightsv1.InsightType_INSIGHT_TYPE_USER_FLOW.Enum(),
				UserFlow:    &insightsv1.UserFlowQuery{},
			},
			TimeRange:   timeRange("2026-07-13T00:00:00Z", "2026-07-20T00:00:00Z"),
			Granularity: insightsv1.Granularity_GRANULARITY_DAY.Enum(),
		}
		q, err := insights.BuildUserFlowQuery(req, "p1")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(q.SQL(), cookielessNeedle) {
			t.Errorf("user flow must exclude cookieless by default:\n%s", q.SQL())
		}
	})

	t.Run("topk_user_dimension_always_person_based", func(t *testing.T) {
		// TOTAL metric, USER dimension: ranking people is person-based
		// regardless of metric.
		req := topKRequest(&insightsv1.TopKQuery{
			Dimension: insightsv1.TopKQuery_DIMENSION_USER.Enum(),
			Metric:    insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL.Enum(),
		})
		q, err := insights.BuildTopKQuery(req, "p1")
		if err != nil {
			t.Fatal(err)
		}
		// buildTopKUsers scans events under the "e" alias.
		if !strings.Contains(q.SQL(), "NOT startsWith(e.distinct_id, 'cookieless-')") {
			t.Errorf("top-K of users must exclude cookieless even for TOTAL:\n%s", q.SQL())
		}
	})

	t.Run("topk_event_kind_total_includes", func(t *testing.T) {
		req := topKRequest(&insightsv1.TopKQuery{
			Dimension: insightsv1.TopKQuery_DIMENSION_EVENT_KIND.Enum(),
			Metric:    insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL.Enum(),
		})
		q, err := insights.BuildTopKQuery(req, "p1")
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(q.SQL(), cookielessNeedle) {
			t.Errorf("event-kind top-K with TOTAL must count all traffic:\n%s", q.SQL())
		}
	})

	t.Run("topk_property_unique_users_excludes", func(t *testing.T) {
		req := topKRequest(&insightsv1.TopKQuery{
			Dimension: insightsv1.TopKQuery_DIMENSION_PROPERTY.Enum(),
			Property:  proto.String("$pathname"),
			Metric:    insightsv1.AggregationType_AGGREGATION_TYPE_UNIQUE_USERS.Enum(),
		})
		q, err := insights.BuildTopKQuery(req, "p1")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(q.SQL(), cookielessNeedle) {
			t.Errorf("property top-K with UNIQUE_USERS must exclude cookieless:\n%s", q.SQL())
		}
	})
}

// TestBuildSegmentUsersQuery_ExcludesCookieless closes the one person-resolving
// builder that had no exclusion.
//
// SegmentUsers is "the who behind a number on a chart": it returns the raw
// distinct_id list for a window. Without the predicate it returned
// cookieless- ids that every other read surface says do not exist as people —
// migration 011 keeps them out of distinct_id_activity_states_mv, so
// profiles.GetByID on one returns ErrProfileNotFound. A UI listing segment users
// and linking to profiles therefore rendered dead links.
//
// SegmentUsersRequest carries no InsightQuerySpec and so no include_cookieless
// toggle, which is why the exclusion is unconditional rather than spec-driven.
func TestBuildSegmentUsersQuery_ExcludesCookieless(t *testing.T) {
	req := &insightsv1.SegmentUsersRequest{
		Events:    []*insightsv1.EventQuery{{Event: &commonv1.EventFilter{Kind: proto.String("page_view")}}},
		TimeRange: timeRange("2026-07-13T00:00:00Z", "2026-07-20T00:00:00Z"),
	}
	sql, _, err := insights.BuildSegmentUsersQuery(req, "p1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sql, cookielessNeedle) {
		t.Errorf("SegmentUsers enumerates people and must exclude cookieless ids:\n%s", sql)
	}
}
