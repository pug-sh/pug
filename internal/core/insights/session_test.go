package insights

import (
	"os"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	commonv1 "github.com/pug-sh/pug/internal/gen/proto/common/v1"
	insightsv1 "github.com/pug-sh/pug/internal/gen/proto/shared/insights/v1"
)

func sessionReq(metric insightsv1.SessionMetric, kind string, breakdown string) *insightsv1.QueryRequest {
	spec := &insightsv1.InsightQuerySpec{
		InsightType: insightsv1.InsightType_INSIGHT_TYPE_TRENDS.Enum(),
		Session: &insightsv1.SessionQuery{
			Metric: metric.Enum(),
			Scope:  &commonv1.EventFilter{Kind: proto.String(kind)},
		},
	}
	if breakdown != "" {
		spec.Breakdowns = []*insightsv1.Breakdown{{Property: proto.String(breakdown)}}
	}
	return &insightsv1.QueryRequest{
		Spec: spec,
		TimeRange: &commonv1.TimeRange{
			From: timestamppb.New(time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)),
			To:   timestamppb.New(time.Date(2024, 1, 8, 0, 0, 0, 0, time.UTC)),
		},
		Granularity: insightsv1.Granularity_GRANULARITY_DAY.Enum(),
	}
}

func TestBuildSessionTrendsQuery_EntryPageRaw(t *testing.T) {
	req := sessionReq(insightsv1.SessionMetric_SESSION_METRIC_ENTRY, "page_view", "$url")
	q, err := BuildSessionTrendsQuery(req, "proj_123")
	if err != nil {
		t.Fatalf("BuildSessionTrendsQuery: %v", err)
	}
	for _, want := range []string{
		"FROM events",
		"argMin(coalesce(url, ''), occur_time) AS breakdown_0",
		"toFloat64(count()) AS value",
		"'page_view' AS event_kind",
		"GROUP BY session_id",
	} {
		if !strings.Contains(q.SQL(), want) {
			t.Errorf("expected SQL to contain %q\nSQL:\n%s", want, q.SQL())
		}
	}
	if got := q.Properties(); len(got) != 1 || got[0] != "$url" {
		t.Errorf("properties = %v, want [$url]", got)
	}
}

func TestBuildSessionSegmentationQuery_AvgDurationRaw(t *testing.T) {
	req := sessionReq(insightsv1.SessionMetric_SESSION_METRIC_AVG_DURATION, "", "")
	req.Spec.InsightType = insightsv1.InsightType_INSIGHT_TYPE_SEGMENTATION.Enum()
	q, err := BuildSessionSegmentationQuery(req, "proj_123")
	if err != nil {
		t.Fatalf("BuildSessionSegmentationQuery: %v", err)
	}
	for _, want := range []string{
		"FROM events",
		"avg(dateDiff('second', start_time, end_time))",
		"'$session'",
	} {
		if want == "'$session'" {
			if strings.Contains(q.SQL(), want) {
				t.Errorf("segmentation SQL should not project an event_kind\nSQL:\n%s", q.SQL())
			}
			continue
		}
		if !strings.Contains(q.SQL(), want) {
			t.Errorf("expected SQL to contain %q\nSQL:\n%s", want, q.SQL())
		}
	}
}

func TestSessionTrendsExecution_RoutesToRollup(t *testing.T) {
	req := sessionReq(insightsv1.SessionMetric_SESSION_METRIC_ENTRY, "page_view", "$url")
	q, usedRollup, err := trendsQueryForExecution(req, "proj_123", time.Now())
	if err != nil {
		t.Fatalf("trendsQueryForExecution: %v", err)
	}
	if !usedRollup {
		t.Fatal("expected session trends to use rollup")
	}
	for _, want := range []string{
		sessionRollupTable,
		"argMinMerge(entry_url_state) AS breakdown_0",
		"minMerge(start_state) AS start_time",
	} {
		if !strings.Contains(q.SQL(), want) {
			t.Errorf("expected rollup SQL to contain %q\nSQL:\n%s", want, q.SQL())
		}
	}
}

func TestSessionTrendsExecution_FallsBackToRawForUnalignedWindow(t *testing.T) {
	req := sessionReq(insightsv1.SessionMetric_SESSION_METRIC_ENTRY, "page_view", "$url")
	req.TimeRange = rollupTimeRange("2024-01-01T06:00:00Z", "2024-01-08T12:00:00Z")
	q, usedRollup, err := trendsQueryForExecution(req, "proj_123", time.Now())
	if err != nil {
		t.Fatalf("trendsQueryForExecution: %v", err)
	}
	if usedRollup {
		t.Fatal("expected non-aligned session trends to fall back to raw")
	}
	if strings.Contains(q.SQL(), sessionRollupTable) || !strings.Contains(q.SQL(), "FROM events") {
		t.Errorf("expected raw events query\nSQL:\n%s", q.SQL())
	}
}

func TestCanUseSessionRollup(t *testing.T) {
	day := insightsv1.Granularity_GRANULARITY_DAY
	cases := []struct {
		name string
		req  *insightsv1.QueryRequest
		want bool
	}{
		{"entry url trends", sessionReq(insightsv1.SessionMetric_SESSION_METRIC_ENTRY, "page_view", "$url"), true},
		{"avg duration trends no breakdown", sessionReq(insightsv1.SessionMetric_SESSION_METRIC_AVG_DURATION, "", ""), true},
		{"custom breakdown rejected", sessionReq(insightsv1.SessionMetric_SESSION_METRIC_ENTRY, "page_view", "plan"), false},
	}
	for _, c := range cases {
		if got := canUseSessionRollup(c.req.GetSpec(), day); got != c.want {
			t.Errorf("%s: canUseSessionRollup = %v, want %v", c.name, got, c.want)
		}
	}

	filtered := sessionReq(insightsv1.SessionMetric_SESSION_METRIC_ENTRY, "page_view", "$url")
	filtered.Spec.Session.Scope.Filters = []*commonv1.PropertyFilter{{Property: proto.String("$os")}}
	if canUseSessionRollup(filtered.GetSpec(), day) {
		t.Error("expected scoped filters to reject session rollup")
	}
}

func TestMigration007SessionRollupColumnsMatchDims(t *testing.T) {
	const path = "../../../schema/clickhouse/migrations/007_create_dashboard_session_rollup.sql"
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	sql := string(data)
	for _, dim := range sessionMaterializedDims {
		suffix, ok := sessionRollupDimSuffix(dim)
		if !ok {
			t.Fatalf("missing suffix for %q", dim)
		}
		for _, prefix := range []string{"entry", "exit"} {
			want := prefix + "_" + suffix + "_state"
			if !strings.Contains(sql, want) {
				t.Errorf("migration missing %s", want)
			}
		}
	}
}
