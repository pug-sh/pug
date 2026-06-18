package insights

import (
	"fmt"
	"os"
	"regexp"
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

func TestSessionTrendsExecution_FallsBackToRawForNonUTCTimezone(t *testing.T) {
	// The session rollup is UTC-keyed too; a non-UTC bucketing zone must force raw.
	req := sessionReq(insightsv1.SessionMetric_SESSION_METRIC_ENTRY, "page_view", "$url")
	req.Timezone = proto.String("Asia/Kolkata")
	q, usedRollup, err := trendsQueryForExecution(req, "proj_123", time.Now())
	if err != nil {
		t.Fatalf("trendsQueryForExecution: %v", err)
	}
	if usedRollup {
		t.Fatal("non-UTC timezone must not use the UTC-keyed session rollup")
	}
	if strings.Contains(q.SQL(), sessionRollupTable) || !strings.Contains(q.SQL(), "FROM events") {
		t.Errorf("expected raw events query\nSQL:\n%s", q.SQL())
	}
	if !strings.Contains(q.SQL(), "toTimeZone(start_time, 'Asia/Kolkata')") {
		t.Errorf("raw session fallback must bucket start_time in the requested zone\nSQL:\n%s", q.SQL())
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
		gran insightsv1.Granularity
		// mutate adjusts the base request (entry/$url trends) into the case under test.
		mutate func(*insightsv1.QueryRequest)
		want   bool
	}{
		{"entry url trends", day, nil, true},
		{"avg duration no breakdown", day, func(r *insightsv1.QueryRequest) {
			r.Spec.Session.Metric = insightsv1.SessionMetric_SESSION_METRIC_AVG_DURATION.Enum()
			r.Spec.Breakdowns = nil
		}, true},
		{"sessions metric trends", day, func(r *insightsv1.QueryRequest) {
			r.Spec.Session.Metric = insightsv1.SessionMetric_SESSION_METRIC_SESSIONS.Enum()
			r.Spec.Breakdowns = nil
		}, true},
		{"avg duration segmentation", day, func(r *insightsv1.QueryRequest) {
			r.Spec.InsightType = insightsv1.InsightType_INSIGHT_TYPE_SEGMENTATION.Enum()
			r.Spec.Session.Metric = insightsv1.SessionMetric_SESSION_METRIC_AVG_DURATION.Enum()
			r.Spec.Breakdowns = nil
		}, true},
		// --- disqualifiers ---
		{"custom breakdown rejected", day, func(r *insightsv1.QueryRequest) {
			r.Spec.Breakdowns = []*insightsv1.Breakdown{{Property: proto.String("plan")}}
		}, false},
		{"funnel insight rejected", day, func(r *insightsv1.QueryRequest) {
			r.Spec.InsightType = insightsv1.InsightType_INSIGHT_TYPE_FUNNEL.Enum()
		}, false},
		{"hour granularity rejected", insightsv1.Granularity_GRANULARITY_HOUR, nil, false},
		{"unspecified granularity rejected", insightsv1.Granularity_GRANULARITY_UNSPECIFIED, nil, false},
		{"filter_groups rejected", day, func(r *insightsv1.QueryRequest) {
			r.Spec.FilterGroups = []*insightsv1.FilterGroup{{
				Filters: []*commonv1.PropertyFilter{{Property: proto.String("$os")}},
			}}
		}, false},
		{"events rejected", day, func(r *insightsv1.QueryRequest) {
			r.Spec.Events = []*insightsv1.EventQuery{{Event: &commonv1.EventFilter{Kind: proto.String("page_view")}}}
		}, false},
		{"two breakdowns rejected", day, func(r *insightsv1.QueryRequest) {
			r.Spec.Breakdowns = []*insightsv1.Breakdown{
				{Property: proto.String("$url")}, {Property: proto.String("$country")},
			}
		}, false},
		{"scope filters rejected", day, func(r *insightsv1.QueryRequest) {
			r.Spec.Session.Scope.Filters = []*commonv1.PropertyFilter{{Property: proto.String("$os")}}
		}, false},
		{"entry without breakdown rejected", day, func(r *insightsv1.QueryRequest) {
			r.Spec.Breakdowns = nil
		}, false},
		{"entry segmentation rejected", day, func(r *insightsv1.QueryRequest) {
			r.Spec.InsightType = insightsv1.InsightType_INSIGHT_TYPE_SEGMENTATION.Enum()
		}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := sessionReq(insightsv1.SessionMetric_SESSION_METRIC_ENTRY, "page_view", "$url")
			if c.mutate != nil {
				c.mutate(req)
			}
			if got := canUseSessionRollup(req.GetSpec(), c.gran); got != c.want {
				t.Errorf("canUseSessionRollup = %v, want %v", got, c.want)
			}
		})
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

// sessionRollupDimSourceExpr returns the inner ClickHouse expression the MV is
// expected to aggregate for a given breakdown dim: bare column for String dims,
// toString(col) for LowCardinality(String) dims (matching events table column
// types in migration 001). Hand-coupled to the migration intent on purpose.
func sessionRollupDimSourceExpr(suffix string) string {
	switch suffix {
	case "url", "city": // String columns
		return suffix
	default: // LowCardinality(String) columns
		return "toString(" + suffix + ")"
	}
}

// TestMigration007SessionRollupDimExprsMatch pins, per dimension, BOTH the source
// column expression AND the merge direction (entry=argMinState, exit=argMaxState)
// in migration 007. TestMigration007SessionRollupColumnsMatchDims only checks
// column names exist; this catches a state wired to the wrong source column (e.g.
// region built from city) or a swapped entry/exit aggregate — drift the name check
// cannot see. The raw builder projects coalesce(col,”) vs the MV's bare/toString
// column, which agree only because every dim column is DEFAULT ” (never NULL); this
// test pins the source side of that equivalence.
func TestMigration007SessionRollupDimExprsMatch(t *testing.T) {
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
		inner := sessionRollupDimSourceExpr(suffix)
		for _, d := range []struct{ prefix, fn string }{
			{"entry", "argMinState"},
			{"exit", "argMaxState"},
		} {
			// e.g. argMinState(toString(country), occur_time) AS entry_country_state
			pat := fmt.Sprintf(`%s\(\s*%s\s*,\s*occur_time\s*\)\s+AS\s+%s_%s_state`,
				d.fn, regexp.QuoteMeta(inner), d.prefix, suffix)
			matches := regexp.MustCompile(pat).FindAllString(sql, -1)
			// MV + backfill = two occurrences expected.
			if len(matches) < 2 {
				t.Errorf("dim %s: expected MV+backfill %s_%s_state = %s(%s, occur_time); found %d match(es)",
					dim, d.prefix, suffix, d.fn, inner, len(matches))
			}
		}
	}
}
