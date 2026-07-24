package insights_test

import (
	"strings"
	"testing"

	"github.com/pug-sh/pug/internal/cookieless"
	insightsv1 "github.com/pug-sh/pug/internal/gen/proto/shared/insights/v1"

	"github.com/pug-sh/pug/internal/core/insights"
)

// sessionMetricReq builds a minimal session request measuring metric over a
// day-aligned UTC window, for the exported session builders. Session insights
// read only session-level state, so no insight_type dispatch or breakdown is
// needed. timeRange is the shared helper from builder_test.go.
func sessionMetricReq(metric insightsv1.SessionMetric) *insightsv1.QueryRequest {
	return &insightsv1.QueryRequest{
		Spec: &insightsv1.InsightQuerySpec{
			Session: &insightsv1.SessionQuery{Metric: metric.Enum()},
		},
		TimeRange:   timeRange("2026-07-13T00:00:00Z", "2026-07-20T00:00:00Z"),
		Granularity: insightsv1.Granularity_GRANULARITY_DAY.Enum(),
	}
}

// TestSessionMetrics_NeverExcludeCookieless is the SessionMetric-axis mirror of
// TestExcludeCookielessForAgg_IsExhaustive: it pins the invariant that session
// metrics ALWAYS count cookieless visitors.
//
// Unlike the count/agg metrics there is no include_cookieless branch to flip for
// sessions — session builders measure per-session state and never read
// distinct_id, so cookieless traffic is counted unconditionally. The ingest
// layer's events.cookieless_session_degraded_total metric description leans on
// exactly this ("session metrics coarsen ... data is intact either way"); no
// toggle can explain a session count that silently dropped consent-rejecting
// visitors. That makes it a STRUCTURAL invariant a refactor can violate: add a
// distinct_id filter or per-user dedup to a session CTE and sessions / bounce-rate
// quietly start excluding cookieless traffic while every count/agg test stays
// green.
//
// The table ranges over every SessionMetric the proto defines, so adding a new
// one without a decision here fails the build — forcing the "does this read
// distinct_id?" question whose only acceptable answer for a session metric is no.
func TestSessionMetrics_NeverExcludeCookieless(t *testing.T) {
	// buildable == an aggregatable session metric (sessionMetricAggExpr returns
	// SQL rather than erroring). UNSPECIFIED is the sole non-buildable member; a
	// newly-added metric absent from this table fails the completeness check below.
	buildable := map[insightsv1.SessionMetric]bool{
		insightsv1.SessionMetric_SESSION_METRIC_UNSPECIFIED:            false,
		insightsv1.SessionMetric_SESSION_METRIC_SESSIONS:               true,
		insightsv1.SessionMetric_SESSION_METRIC_AVG_DURATION:           true,
		insightsv1.SessionMetric_SESSION_METRIC_BOUNCE_RATE:            true,
		insightsv1.SessionMetric_SESSION_METRIC_ENTRY:                  true,
		insightsv1.SessionMetric_SESSION_METRIC_EXIT:                   true,
		insightsv1.SessionMetric_SESSION_METRIC_AVG_EVENTS_PER_SESSION: true,
	}

	// The exclusion, if ever wrongly applied, embeds the reserved id prefix as a
	// SQL literal ("NOT startsWith(..., 'cookieless-')"). The prefix is therefore
	// the least-brittle needle: it cannot appear in a session query for any other
	// reason.
	needle := cookieless.IDPrefix

	for num, name := range insightsv1.SessionMetric_name {
		metric := insightsv1.SessionMetric(num)
		ok, decided := buildable[metric]
		if !decided {
			t.Errorf("%s has no session-cookieless decision: add it to buildable. A session "+
				"metric must read only session-level state, never distinct_id, so it must NOT "+
				"exclude cookieless.", name)
			continue
		}
		if !ok {
			continue // UNSPECIFIED: sessionMetricAggExpr errors; not a countable metric.
		}

		req := sessionMetricReq(metric)

		trends, err := insights.BuildSessionTrendsQuery(req, "p1")
		if err != nil {
			t.Errorf("%s: BuildSessionTrendsQuery: %v", name, err)
		} else if strings.Contains(trends.SQL(), needle) {
			t.Errorf("%s: session trends must not exclude cookieless (session metrics count all traffic):\n%s",
				name, trends.SQL())
		}

		seg, err := insights.BuildSessionSegmentationQuery(req, "p1")
		if err != nil {
			t.Errorf("%s: BuildSessionSegmentationQuery: %v", name, err)
		} else if strings.Contains(seg.SQL(), needle) {
			t.Errorf("%s: session segmentation must not exclude cookieless (session metrics count all traffic):\n%s",
				name, seg.SQL())
		}
	}
}
