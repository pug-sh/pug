package insights

import (
	"fmt"
	"regexp"
	"slices"
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
		{"avg events per session trends", day, func(r *insightsv1.QueryRequest) {
			r.Spec.Session.Metric = insightsv1.SessionMetric_SESSION_METRIC_AVG_EVENTS_PER_SESSION.Enum()
			r.Spec.Breakdowns = nil
		}, true},
		{"avg events per session segmentation", day, func(r *insightsv1.QueryRequest) {
			r.Spec.InsightType = insightsv1.InsightType_INSIGHT_TYPE_SEGMENTATION.Enum()
			r.Spec.Session.Metric = insightsv1.SessionMetric_SESSION_METRIC_AVG_EVENTS_PER_SESSION.Enum()
			r.Spec.Breakdowns = nil
		}, true},
		{"entry pathname trends", day, func(r *insightsv1.QueryRequest) {
			r.Spec.Breakdowns = []*insightsv1.Breakdown{{Property: proto.String("$pathname")}}
		}, true},
		{"sessions by channel", day, func(r *insightsv1.QueryRequest) {
			r.Spec.Session.Metric = insightsv1.SessionMetric_SESSION_METRIC_SESSIONS.Enum()
			r.Spec.Breakdowns = []*insightsv1.Breakdown{{Property: proto.String("$referrerDomain")}}
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

const (
	migration007Path = "../../../schema/clickhouse/migrations/007_create_dashboard_session_rollup.sql"
	migration010Path = "../../../schema/clickhouse/migrations/010_extend_dashboard_session_rollup.sql"
)

// sessionRollupDimSourceExpr returns the inner ClickHouse expression the MV is
// expected to aggregate for a given breakdown dim: bare column for String dims,
// toString(col) for LowCardinality(String) dims (matching events table column
// types in migrations 001 and 008). Hand-coupled to the migration intent on purpose.
func sessionRollupDimSourceExpr(suffix string) string {
	switch suffix {
	case "url", "city", "pathname": // String columns
		return suffix
	default: // LowCardinality(String) columns
		return "toString(" + suffix + ")"
	}
}

// countSessionDimStateExprs returns how many times sql states the given dim's
// entry/exit pair with the correct source expression and merge direction
// (entry=argMinState, exit=argMaxState); mismatched wiring counts as zero. A
// state wired to the wrong source column (e.g. region built from city) or a
// swapped entry/exit aggregate is drift a bare column-name check cannot see.
// The raw builder projects coalesce(col,”) vs the MV's bare/toString column,
// which agree only because every dim column is DEFAULT ” (never NULL); this
// pins the source side of that equivalence.
func countSessionDimStateExprs(t *testing.T, sql, dim string) int {
	t.Helper()
	suffix, ok := sessionRollupDimSuffix(dim)
	if !ok {
		t.Fatalf("missing suffix for %q", dim)
	}
	inner := sessionRollupDimSourceExpr(suffix)
	count := func(prefix, fn string) int {
		// e.g. argMinState(toString(country), occur_time) AS entry_country_state
		pat := fmt.Sprintf(`%s\(\s*%s\s*,\s*occur_time\s*\)\s+AS\s+%s_%s_state`,
			fn, regexp.QuoteMeta(inner), prefix, suffix)
		return len(regexp.MustCompile(pat).FindAllString(sql, -1))
	}
	entry, exit := count("entry", "argMinState"), count("exit", "argMaxState")
	if entry != exit {
		t.Errorf("dim %s: %d entry state(s) but %d exit state(s) — a dim must gain both or neither", dim, entry, exit)
	}
	return entry
}

// TestMigration007Frozen pins migration 007 to its historical content: the
// legacy eleven dims, each stated twice (MV + backfill) with the correct
// source expression and merge direction, and none of the new web-analytics
// states. Guards against editing a shipped migration instead of adding a new one.
func TestMigration007Frozen(t *testing.T) {
	sql := readMigration(t, migration007Path)
	for _, dim := range sessionRollupDims007 {
		if got := countSessionDimStateExprs(t, sql, dim); got != 2 {
			t.Errorf("dim %s: expected MV+backfill = 2 correctly-wired state pairs in 007, found %d", dim, got)
		}
	}
	for _, dim := range sessionRollupDims010 {
		suffix, ok := sessionRollupDimSuffix(dim)
		if !ok {
			t.Fatalf("missing suffix for %q", dim)
		}
		for _, prefix := range []string{"entry", "exit"} {
			if state := prefix + "_" + suffix + "_state"; strings.Contains(sql, state) {
				t.Errorf("migration 007 must not contain the web-analytics state %s", state)
			}
		}
	}
}

// TestMigration010SessionRollupColumnsMatchDims pins the Go dim list to
// migration 010's Up section: every current dim's entry/exit state columns are
// stated by the MODIFY QUERY, and the NEW dims' state columns appear in both
// the ADD COLUMN block and the partial backfill's explicit column list (the
// backfill must list ONLY key columns + new states — an old state column there
// would corrupt merged history, see the migration header).
func TestMigration010SessionRollupColumnsMatchDims(t *testing.T) {
	up := migrationUpSection(t, migration010Path)
	for _, dim := range sessionMaterializedDims {
		suffix, ok := sessionRollupDimSuffix(dim)
		if !ok {
			t.Fatalf("missing suffix for %q", dim)
		}
		for _, prefix := range []string{"entry", "exit"} {
			want := prefix + "_" + suffix + "_state"
			if !strings.Contains(up, want) {
				t.Errorf("migration 010 Up missing %s", want)
			}
		}
	}
	// The backfill INSERT column list must be exactly key columns + new states.
	insertRe := regexp.MustCompile(`(?s)INSERT INTO dashboard_session_rollup \((.*?)\)`)
	m := insertRe.FindStringSubmatch(up)
	if m == nil {
		t.Fatal("migration 010 Up missing the partial backfill INSERT column list")
	}
	var cols []string
	for _, c := range strings.Split(m[1], ",") {
		cols = append(cols, strings.TrimSpace(c))
	}
	want := []string{"project_id", "kind", "session_id"}
	for _, dim := range sessionRollupDims010 {
		suffix, ok := sessionRollupDimSuffix(dim)
		if !ok {
			t.Fatalf("missing suffix for %q", dim)
		}
		want = append(want, "entry_"+suffix+"_state", "exit_"+suffix+"_state")
	}
	slices.Sort(cols)
	slices.Sort(want)
	if !slices.Equal(cols, want) {
		t.Errorf("backfill column list %v != key columns + new states %v", cols, want)
	}
	// The per-migration groups must not overlap: an older group's state listed
	// in 010's partial backfill would be re-inserted over merged history
	// instead of relying on empty-state merge identity. (sessionMaterializedDims
	// being their union needs no assertion — slices.Concat makes it so.)
	for _, dim := range sessionRollupDims010 {
		if slices.Contains(sessionRollupDims007, dim) {
			t.Errorf("dim %s is in both the 007 and 010 groups; 010's backfill would corrupt its merged state", dim)
		}
	}
}

// TestMigration010SessionRollupDimExprsMatch pins migration 010's state
// expressions: in the Up section, pre-existing dims are restated once (the
// MODIFY QUERY) and the new dims twice (MODIFY QUERY + partial backfill), each
// with the correct source column and merge direction.
func TestMigration010SessionRollupDimExprsMatch(t *testing.T) {
	up := migrationUpSection(t, migration010Path)
	for _, dim := range sessionRollupDims007 {
		if got := countSessionDimStateExprs(t, up, dim); got != 1 {
			t.Errorf("dim %s: expected 1 correctly-wired state pair in 010 Up (MODIFY QUERY), found %d", dim, got)
		}
	}
	for _, dim := range sessionRollupDims010 {
		if got := countSessionDimStateExprs(t, up, dim); got != 2 {
			t.Errorf("dim %s: expected 2 correctly-wired state pairs in 010 Up (MODIFY QUERY + backfill), found %d", dim, got)
		}
	}
	if strings.Contains(up, "auto_properties['$") {
		t.Error("migration 010 must not read promoted keys from the auto_properties map")
	}
}
