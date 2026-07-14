package insights

import (
	"fmt"
	"os"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	chq "github.com/pug-sh/pug/internal/core/clickhouse"
	commonv1 "github.com/pug-sh/pug/internal/gen/proto/common/v1"
	insightsv1 "github.com/pug-sh/pug/internal/gen/proto/shared/insights/v1"
)

func rollupTrendsSpec(agg insightsv1.AggregationType, kind string, breakdown string) *insightsv1.InsightQuerySpec {
	ev := &insightsv1.EventQuery{
		Event:       &commonv1.EventFilter{Kind: proto.String(kind)},
		Aggregation: agg.Enum(),
	}
	spec := &insightsv1.InsightQuerySpec{
		InsightType: insightsv1.InsightType_INSIGHT_TYPE_TRENDS.Enum(),
		Events:      []*insightsv1.EventQuery{ev},
	}
	if breakdown != "" {
		spec.Breakdowns = []*insightsv1.Breakdown{{Property: proto.String(breakdown)}}
	}
	return spec
}

func rollupSegSpec(agg insightsv1.AggregationType, kind string) *insightsv1.InsightQuerySpec {
	return &insightsv1.InsightQuerySpec{
		InsightType: insightsv1.InsightType_INSIGHT_TYPE_SEGMENTATION.Enum(),
		Events:      []*insightsv1.EventQuery{{Event: &commonv1.EventFilter{Kind: proto.String(kind)}, Aggregation: agg.Enum()}},
	}
}

func rollupMultiEventTrendsSpec(kinds ...string) *insightsv1.InsightQuerySpec {
	spec := &insightsv1.InsightQuerySpec{InsightType: insightsv1.InsightType_INSIGHT_TYPE_TRENDS.Enum()}
	for _, k := range kinds {
		spec.Events = append(spec.Events, &insightsv1.EventQuery{
			Event:       &commonv1.EventFilter{Kind: proto.String(k)},
			Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL.Enum(),
		})
	}
	return spec
}

func TestRollupAggExpr(t *testing.T) {
	cases := []struct {
		agg  insightsv1.AggregationType
		want string
		ok   bool
	}{
		{insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL, "toFloat64(sum(cnt))", true},
		{insightsv1.AggregationType_AGGREGATION_TYPE_UNSPECIFIED, "toFloat64(sum(cnt))", true},
		{insightsv1.AggregationType_AGGREGATION_TYPE_UNIQUE_USERS, "toFloat64(uniqMerge(uniq_state))", true},
		{insightsv1.AggregationType_AGGREGATION_TYPE_PER_USER_AVG, "if(uniqMerge(uniq_state) = 0, 0, toFloat64(sum(cnt)) / toFloat64(uniqMerge(uniq_state)))", true},
		{insightsv1.AggregationType_AGGREGATION_TYPE_SUM, "", false},
		{insightsv1.AggregationType_AGGREGATION_TYPE_AVG, "", false},
		{insightsv1.AggregationType_AGGREGATION_TYPE_MIN, "", false},
		{insightsv1.AggregationType_AGGREGATION_TYPE_MAX, "", false},
	}
	for _, c := range cases {
		got, ok := rollupAggExpr(c.agg)
		if got != c.want || ok != c.ok {
			t.Errorf("rollupAggExpr(%v) = (%q, %v), want (%q, %v)", c.agg, got, ok, c.want, c.ok)
		}
	}
}

func TestCanUseEventRollup(t *testing.T) {
	day := insightsv1.Granularity_GRANULARITY_DAY
	cases := []struct {
		name string
		spec *insightsv1.InsightQuerySpec
		gran insightsv1.Granularity
		want bool
	}{
		{"trends count no breakdown", rollupTrendsSpec(insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL, "page_view", ""), day, true},
		{"trends count materialized breakdown", rollupTrendsSpec(insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL, "page_view", "$country"), day, true},
		{"trends unique users", rollupTrendsSpec(insightsv1.AggregationType_AGGREGATION_TYPE_UNIQUE_USERS, "page_view", "$country"), day, true},
		{"trends per user avg", rollupTrendsSpec(insightsv1.AggregationType_AGGREGATION_TYPE_PER_USER_AVG, "page_view", ""), day, true},
		{"week granularity", rollupTrendsSpec(insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL, "page_view", "$country"), insightsv1.Granularity_GRANULARITY_WEEK, true},
		{"hour granularity rejected", rollupTrendsSpec(insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL, "page_view", "$country"), insightsv1.Granularity_GRANULARITY_HOUR, false},
		{"non-materialized breakdown rejected", rollupTrendsSpec(insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL, "page_view", "$customProp"), day, false},
		{"numeric agg rejected", rollupTrendsSpec(insightsv1.AggregationType_AGGREGATION_TYPE_SUM, "page_view", "$country"), day, false},
		{"empty kind rejected", rollupTrendsSpec(insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL, "", ""), day, false},
		{"funnel rejected", &insightsv1.InsightQuerySpec{InsightType: insightsv1.InsightType_INSIGHT_TYPE_FUNNEL.Enum(), Events: []*insightsv1.EventQuery{{Event: &commonv1.EventFilter{Kind: proto.String("page_view")}}}}, day, false},
		{"month granularity accepted", rollupTrendsSpec(insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL, "page_view", "$country"), insightsv1.Granularity_GRANULARITY_MONTH, true},
		{"minute granularity rejected", rollupTrendsSpec(insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL, "page_view", ""), insightsv1.Granularity_GRANULARITY_MINUTE, false},
		{"unspecified granularity rejected", rollupTrendsSpec(insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL, "page_view", ""), insightsv1.Granularity_GRANULARITY_UNSPECIFIED, false},
		{"segmentation no breakdown accepted", rollupSegSpec(insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL, "page_view"), day, true},
		{"segmentation unique users accepted", rollupSegSpec(insightsv1.AggregationType_AGGREGATION_TYPE_UNIQUE_USERS, "page_view"), day, true},
		{"retention rejected", &insightsv1.InsightQuerySpec{InsightType: insightsv1.InsightType_INSIGHT_TYPE_RETENTION.Enum(), Events: []*insightsv1.EventQuery{{Event: &commonv1.EventFilter{Kind: proto.String("page_view")}}}}, day, false},
		{"unspecified insight type rejected", &insightsv1.InsightQuerySpec{Events: []*insightsv1.EventQuery{{Event: &commonv1.EventFilter{Kind: proto.String("page_view")}}}}, day, false},
		{"zero events rejected", &insightsv1.InsightQuerySpec{InsightType: insightsv1.InsightType_INSIGHT_TYPE_TRENDS.Enum()}, day, false},
		{"multi-event all valid accepted", rollupMultiEventTrendsSpec("page_view", "signup"), day, true},
		{"multi-event one empty kind rejected", rollupMultiEventTrendsSpec("page_view", ""), day, false},
		{"multi-event one numeric agg rejected", &insightsv1.InsightQuerySpec{InsightType: insightsv1.InsightType_INSIGHT_TYPE_TRENDS.Enum(), Events: []*insightsv1.EventQuery{
			{Event: &commonv1.EventFilter{Kind: proto.String("page_view")}, Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL.Enum()},
			{Event: &commonv1.EventFilter{Kind: proto.String("signup")}, Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_SUM.Enum()},
		}}, day, false},
	}
	for _, c := range cases {
		if got := canUseEventRollup(c.spec, c.gran); got != c.want {
			t.Errorf("%s: canUseEventRollup = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestCanUseEventRollup_RejectsFilters(t *testing.T) {
	day := insightsv1.Granularity_GRANULARITY_DAY

	topLevelFiltered := rollupTrendsSpec(insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL, "page_view", "$country")
	topLevelFiltered.FilterGroups = []*insightsv1.FilterGroup{{}}
	if canUseEventRollup(topLevelFiltered, day) {
		t.Error("expected rollup rejected when filter_groups present")
	}

	eventFiltered := rollupTrendsSpec(insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL, "page_view", "$country")
	eventFiltered.Events[0].Event.Filters = []*commonv1.PropertyFilter{{Property: proto.String("$os")}}
	if canUseEventRollup(eventFiltered, day) {
		t.Error("expected rollup rejected when per-event filters present")
	}

	twoBreakdowns := rollupTrendsSpec(insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL, "page_view", "$country")
	twoBreakdowns.Breakdowns = append(twoBreakdowns.Breakdowns, &insightsv1.Breakdown{Property: proto.String("$os")})
	if canUseEventRollup(twoBreakdowns, day) {
		t.Error("expected rollup rejected with two breakdowns")
	}
}

// rollupTimeRange builds a TimeRange from RFC3339 strings. Local to this internal
// test file (the timeRange helper in builder_test.go is in package insights_test
// and is not visible here).
func rollupTimeRange(fromRFC, toRFC string) *commonv1.TimeRange {
	from, err := time.Parse(time.RFC3339, fromRFC)
	if err != nil {
		panic(err)
	}
	to, err := time.Parse(time.RFC3339, toRFC)
	if err != nil {
		panic(err)
	}
	return &commonv1.TimeRange{From: timestamppb.New(from), To: timestamppb.New(to)}
}

func rollupDayReq(spec *insightsv1.InsightQuerySpec) *insightsv1.QueryRequest {
	return &insightsv1.QueryRequest{
		Spec:        spec,
		TimeRange:   rollupTimeRange("2024-01-01T00:00:00Z", "2024-01-08T00:00:00Z"),
		Granularity: insightsv1.Granularity_GRANULARITY_DAY.Enum(),
	}
}

func TestBuildTrendsFromRollup_Breakdown(t *testing.T) {
	req := rollupDayReq(rollupTrendsSpec(insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL, "page_view", "$country"))
	q, err := buildTrendsFromRollup(req, "proj_123")
	if err != nil {
		t.Fatalf("buildTrendsFromRollup: %v", err)
	}
	sql := q.SQL()
	for _, want := range []string{
		"FROM dashboard_event_rollup_daily",
		"top_vals",
		"dim_name",
		"if(dim_value IN (SELECT dim_value FROM top_vals), dim_value, '$others') AS breakdown_0",
		"toFloat64(sum(cnt)) AS value",
		"toStartOfDay(toDateTime(day)) AS t",
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("expected SQL to contain %q\nSQL:\n%s", want, sql)
		}
	}
	if len(q.Properties()) != 1 || q.Properties()[0] != "$country" {
		t.Errorf("expected properties [$country], got %v", q.Properties())
	}
}

func TestFillMultiEventTrendZeros(t *testing.T) {
	t1 := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	t2 := time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC)

	flatten := func(rows []TrendRow) map[string]float64 {
		out := map[string]float64{}
		for _, r := range rows {
			bd := ""
			if len(r.Breakdowns) > 0 {
				bd = strings.Join(r.Breakdowns, ",")
			}
			out[r.EventKind+"|"+bd+"|"+r.Time.Format("2006-01-02")] = r.Value
		}
		return out
	}

	t.Run("no_breakdown", func(t *testing.T) {
		rows := []TrendRow{
			{Time: t1, EventKind: "page_view", Value: 2},
			{Time: t1, EventKind: "signup", Value: 1},
			{Time: t2, EventKind: "page_view", Value: 1},
		}
		got := flatten(fillMultiEventTrendZeros(rows, []string{"page_view", "signup"}))
		want := map[string]float64{
			"page_view||2024-01-01": 2,
			"signup||2024-01-01":    1,
			"page_view||2024-01-02": 1,
			"signup||2024-01-02":    0,
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	t.Run("with_breakdown_fills_missing_kind", func(t *testing.T) {
		// Grid cells: (t1,US), (t1,GB), (t2,US). For each, both kinds must appear.
		rows := []TrendRow{
			{Time: t1, EventKind: "page_view", Breakdowns: []string{"US"}, Value: 5},
			{Time: t1, EventKind: "signup", Breakdowns: []string{"US"}, Value: 1},
			{Time: t1, EventKind: "page_view", Breakdowns: []string{"GB"}, Value: 2},
			// signup|GB|t1 missing
			{Time: t2, EventKind: "page_view", Breakdowns: []string{"US"}, Value: 3},
			// signup|US|t2 missing
		}
		got := flatten(fillMultiEventTrendZeros(rows, []string{"page_view", "signup"}))
		want := map[string]float64{
			"page_view|US|2024-01-01": 5,
			"signup|US|2024-01-01":    1,
			"page_view|GB|2024-01-01": 2,
			"signup|GB|2024-01-01":    0,
			"page_view|US|2024-01-02": 3,
			"signup|US|2024-01-02":    0,
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	t.Run("preserves_empty_string_breakdown_value", func(t *testing.T) {
		// A breakdown column whose value is the empty string is a legitimate, single-
		// element slice ([]string{""}). It must NOT be collapsed to a nil breakdown
		// slice on the synthesized zero row — GroupSeries fails the length check
		// against properties otherwise.
		rows := []TrendRow{
			{Time: t1, EventKind: "page_view", Breakdowns: []string{""}, Value: 1},
		}
		got := fillMultiEventTrendZeros(rows, []string{"page_view", "signup"})
		var synthSignup *TrendRow
		for i := range got {
			if got[i].EventKind == "signup" {
				synthSignup = &got[i]
			}
		}
		if synthSignup == nil {
			t.Fatalf("expected synthesized signup row, got: %v", got)
		}
		if len(synthSignup.Breakdowns) != 1 || synthSignup.Breakdowns[0] != "" {
			t.Errorf("synthesized row Breakdowns = %v, want []string{\"\"}", synthSignup.Breakdowns)
		}
	})

	t.Run("non_utc_input_time_keys_consistently", func(t *testing.T) {
		// Same instant in two zones must collide on one grid cell.
		ist := time.FixedZone("IST", 5*3600+30*60)
		t1IST := t1.In(ist) // same UnixNano as t1
		rows := []TrendRow{
			{Time: t1, EventKind: "page_view", Value: 2},
			{Time: t1IST, EventKind: "signup", Value: 1},
		}
		got := fillMultiEventTrendZeros(rows, []string{"page_view", "signup"})
		if len(got) != 2 {
			t.Errorf("expected no synthesized rows (both cells already present in UTC), got %d: %v", len(got), got)
		}
	})

	t.Run("single_event_unchanged", func(t *testing.T) {
		rows := []TrendRow{{Time: t1, EventKind: "page_view", Value: 1}}
		if got := fillMultiEventTrendZeros(rows, []string{"page_view"}); len(got) != len(rows) {
			t.Errorf("single event should be unchanged, got %v", got)
		}
	})
}

func TestBuildTrendsFromRollup_NoBreakdownUsesTotal(t *testing.T) {
	req := rollupDayReq(rollupTrendsSpec(insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL, "page_view", ""))
	q, err := buildTrendsFromRollup(req, "proj_123")
	if err != nil {
		t.Fatalf("buildTrendsFromRollup: %v", err)
	}
	sql := q.SQL()
	if strings.Contains(sql, "top_vals") {
		t.Errorf("no-breakdown trends must not emit a top_vals CTE\nSQL:\n%s", sql)
	}
	if strings.Contains(sql, "breakdown_0") {
		t.Errorf("no-breakdown trends must not select a breakdown column\nSQL:\n%s", sql)
	}
	// dim_name is filtered to the synthetic total dimension; the value is a bound
	// parameter (dim_name = ?), so assert on the args rather than the SQL text.
	found := false
	for _, a := range q.Args() {
		if a == "$__total__" {
			found = true
		}
	}
	if !found {
		t.Errorf("no-breakdown trends must filter dim_name = $__total__; args = %v", q.Args())
	}
}

func TestBuildTrendsFromRollup_UniqueUsers(t *testing.T) {
	req := rollupDayReq(rollupTrendsSpec(insightsv1.AggregationType_AGGREGATION_TYPE_UNIQUE_USERS, "page_view", "$country"))
	q, err := buildTrendsFromRollup(req, "proj_123")
	if err != nil {
		t.Fatalf("buildTrendsFromRollup: %v", err)
	}
	if !strings.Contains(q.SQL(), "toFloat64(uniqMerge(uniq_state)) AS value") {
		t.Errorf("unique-users trends must use uniqMerge\nSQL:\n%s", q.SQL())
	}
}

func TestBuildSegmentationFromRollup(t *testing.T) {
	spec := &insightsv1.InsightQuerySpec{
		InsightType: insightsv1.InsightType_INSIGHT_TYPE_SEGMENTATION.Enum(),
		Events: []*insightsv1.EventQuery{
			{Event: &commonv1.EventFilter{Kind: proto.String("page_view")}, Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL.Enum()},
		},
	}
	req := &insightsv1.QueryRequest{
		Spec:        spec,
		TimeRange:   rollupTimeRange("2024-01-01T00:00:00Z", "2024-01-08T00:00:00Z"),
		Granularity: insightsv1.Granularity_GRANULARITY_DAY.Enum(),
	}
	q, err := buildSegmentationFromRollup(req, "proj_123")
	if err != nil {
		t.Fatalf("buildSegmentationFromRollup: %v", err)
	}
	sql := q.SQL()
	if !strings.Contains(sql, "FROM dashboard_event_rollup_daily") {
		t.Errorf("expected rollup table\nSQL:\n%s", sql)
	}
	if !strings.Contains(sql, "toFloat64(sum(cnt)) AS value") {
		t.Errorf("expected sum(cnt) value\nSQL:\n%s", sql)
	}
	found := false
	for _, a := range q.Args() {
		if a == "$__total__" {
			found = true
		}
	}
	if !found {
		t.Errorf("segmentation must filter dim_name = $__total__; args = %v", q.Args())
	}
}

func TestTrendsExecution_RoutesToRollup(t *testing.T) {
	req := rollupDayReq(rollupTrendsSpec(insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL, "page_view", "$country"))
	q, _, err := trendsQueryForExecution(req, "proj_123", time.Now())
	if err != nil {
		t.Fatalf("trendsQueryForExecution: %v", err)
	}
	if !strings.Contains(q.SQL(), rollupTable) {
		t.Errorf("eligible trends query must route to the rollup\nSQL:\n%s", q.SQL())
	}
}

func TestTrendsExecution_FallsBackToRaw_NonAlignedWindow(t *testing.T) {
	// Rollup-eligible spec (DAY granularity, no filters, materialized breakdown) but
	// a non-day-aligned absolute window (mid-day bounds, in the past) must fall back
	// to raw: the rollup is keyed on whole days and would widen the window, over-
	// counting the partial boundary days. The raw builder filters exact instants.
	req := rollupDayReq(rollupTrendsSpec(insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL, "page_view", "$country"))
	req.TimeRange = rollupTimeRange("2024-01-01T06:00:00Z", "2024-01-08T12:00:00Z")
	q, _, err := trendsQueryForExecution(req, "proj_123", time.Now())
	if err != nil {
		t.Fatalf("trendsQueryForExecution: %v", err)
	}
	if strings.Contains(q.SQL(), rollupTable) {
		t.Errorf("non-day-aligned window must hit raw events, got rollup\nSQL:\n%s", q.SQL())
	}
	if !strings.Contains(q.SQL(), "FROM events") {
		t.Errorf("expected raw events query\nSQL:\n%s", q.SQL())
	}
}

func TestTrendsExecution_FallsBackToRaw_NonUTCTimezone(t *testing.T) {
	// Fully rollup-eligible (DAY, no filters, materialized breakdown, aligned window)
	// EXCEPT the bucketing timezone is non-UTC. The daily rollup is UTC-keyed, so
	// serving a local-calendar query from it would silently bucket on UTC boundaries
	// (off-by-one days for the viewer). The utcBucketing gate must force the raw path.
	req := rollupDayReq(rollupTrendsSpec(insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL, "page_view", "$country"))
	req.Timezone = proto.String("Asia/Kolkata")
	q, usedRollup, err := trendsQueryForExecution(req, "proj_123", time.Now())
	if err != nil {
		t.Fatalf("trendsQueryForExecution: %v", err)
	}
	if usedRollup {
		t.Fatal("non-UTC timezone must not use the UTC-keyed rollup")
	}
	if strings.Contains(q.SQL(), rollupTable) {
		t.Errorf("non-UTC timezone must hit raw events, got rollup\nSQL:\n%s", q.SQL())
	}
	if !strings.Contains(q.SQL(), "FROM events") {
		t.Errorf("expected raw events query\nSQL:\n%s", q.SQL())
	}
	if !strings.Contains(q.SQL(), "toTimeZone(occur_time, 'Asia/Kolkata')") {
		t.Errorf("raw fallback must bucket in the requested zone\nSQL:\n%s", q.SQL())
	}
}

func TestSegmentationExecution_FallsBackToRaw_NonUTCTimezone(t *testing.T) {
	// Same gate on the segmentation dispatch path.
	req := rollupDayReq(rollupSegSpec(insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL, "page_view"))
	req.Timezone = proto.String("Asia/Kolkata")
	q, usedRollup, err := segmentationQueryForExecution(req, "proj_123", time.Now())
	if err != nil {
		t.Fatalf("segmentationQueryForExecution: %v", err)
	}
	if usedRollup {
		t.Fatal("non-UTC timezone must not use the UTC-keyed rollup")
	}
	if strings.Contains(q.SQL(), rollupTable) {
		t.Errorf("non-UTC timezone must hit raw events, got rollup\nSQL:\n%s", q.SQL())
	}
	if !strings.Contains(q.SQL(), "FROM events") {
		t.Errorf("expected raw events query\nSQL:\n%s", q.SQL())
	}
}

func TestRollupWindowAligned(t *testing.T) {
	now := time.Date(2026, 5, 25, 14, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		tr   *commonv1.TimeRange
		want bool
	}{
		{"both midnight aligned", rollupTimeRange("2024-01-01T00:00:00Z", "2024-01-08T00:00:00Z"), true},
		{"single aligned day", rollupTimeRange("2024-01-01T00:00:00Z", "2024-01-02T00:00:00Z"), true},
		{"from midnight, to=now (live preset)", &commonv1.TimeRange{From: timestamppb.New(startOfDayUTC(now)), To: timestamppb.New(now)}, true},
		{"from midnight, to future", &commonv1.TimeRange{From: timestamppb.New(startOfDayUTC(now)), To: timestamppb.New(now.Add(time.Hour))}, true},
		{"from mid-day rejected", rollupTimeRange("2024-01-01T06:00:00Z", "2024-01-08T00:00:00Z"), false},
		{"to past mid-day rejected", rollupTimeRange("2024-01-01T00:00:00Z", "2024-01-04T06:00:00Z"), false},
		{"to earlier today (past, mid-day) rejected", &commonv1.TimeRange{From: timestamppb.New(startOfDayUTC(now)), To: timestamppb.New(now.Add(-2 * time.Hour))}, false},
		// Alignment is evaluated in UTC: a non-UTC instant that *is* midnight UTC is
		// aligned; a wall-clock midnight in a non-UTC zone is not.
		{"non-UTC instant equal to midnight UTC accepted", &commonv1.TimeRange{
			From: timestamppb.New(time.Date(2024, 1, 1, 5, 30, 0, 0, time.FixedZone("IST", 5*3600+30*60))),
			To:   timestamppb.New(time.Date(2024, 1, 8, 5, 30, 0, 0, time.FixedZone("IST", 5*3600+30*60))),
		}, true},
		{"wall-clock midnight in non-UTC zone rejected", &commonv1.TimeRange{
			From: timestamppb.New(time.Date(2024, 1, 1, 0, 0, 0, 0, time.FixedZone("IST", 5*3600+30*60))),
			To:   timestamppb.New(time.Date(2024, 1, 8, 0, 0, 0, 0, time.UTC)),
		}, false},
	}
	for _, c := range cases {
		if got := rollupWindowAligned(c.tr, now); got != c.want {
			t.Errorf("%s: rollupWindowAligned = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestRollupDayBounds(t *testing.T) {
	cases := []struct {
		name             string
		fromRFC, toRFC   string
		wantFrom, wantTo string
	}{
		// `to` is exclusive, so the last included day is the day of (to - 1ns).
		{"aligned week", "2024-01-01T00:00:00Z", "2024-01-08T00:00:00Z", "2024-01-01", "2024-01-07"},
		{"single day, to exclusive midnight", "2024-01-01T00:00:00Z", "2024-01-02T00:00:00Z", "2024-01-01", "2024-01-01"},
		{"to exactly midnight rolls back a day", "2024-01-01T00:00:00Z", "2024-01-04T00:00:00Z", "2024-01-01", "2024-01-03"},
		{"mid-day from floors to its day", "2024-01-01T06:00:00Z", "2024-01-08T12:00:00Z", "2024-01-01", "2024-01-08"},
		{"mid-day to stays on its day", "2024-01-01T00:00:00Z", "2024-01-05T12:00:00Z", "2024-01-01", "2024-01-05"},
		{"to one second past midnight stays on that day", "2024-01-01T00:00:00Z", "2024-01-05T00:00:01Z", "2024-01-01", "2024-01-05"},
		{"non-UTC instant normalized to UTC day", "2024-01-01T05:30:00+05:30", "2024-01-08T05:30:00+05:30", "2024-01-01", "2024-01-07"},
	}
	for _, c := range cases {
		req := &insightsv1.QueryRequest{TimeRange: rollupTimeRange(c.fromRFC, c.toRFC)}
		gotFrom, gotTo := rollupDayBounds(req)
		if gotFrom != c.wantFrom || gotTo != c.wantTo {
			t.Errorf("%s: rollupDayBounds = (%q, %q), want (%q, %q)", c.name, gotFrom, gotTo, c.wantFrom, c.wantTo)
		}
	}
}

func TestTrendsExecution_FallsBackToRaw(t *testing.T) {
	// HOUR granularity is rollup-ineligible → must hit raw events.
	req := rollupDayReq(rollupTrendsSpec(insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL, "page_view", "$country"))
	req.Granularity = insightsv1.Granularity_GRANULARITY_HOUR.Enum()
	q, _, err := trendsQueryForExecution(req, "proj_123", time.Now())
	if err != nil {
		t.Fatalf("trendsQueryForExecution: %v", err)
	}
	if strings.Contains(q.SQL(), rollupTable) {
		t.Errorf("ineligible trends query must hit raw events, got rollup\nSQL:\n%s", q.SQL())
	}
	if !strings.Contains(q.SQL(), "FROM events") {
		t.Errorf("expected raw events query\nSQL:\n%s", q.SQL())
	}
}

func TestSegmentationExecution_RoutesToRollup(t *testing.T) {
	spec := &insightsv1.InsightQuerySpec{
		InsightType: insightsv1.InsightType_INSIGHT_TYPE_SEGMENTATION.Enum(),
		Events: []*insightsv1.EventQuery{
			{Event: &commonv1.EventFilter{Kind: proto.String("page_view")}, Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL.Enum()},
		},
	}
	req := &insightsv1.QueryRequest{
		Spec:        spec,
		TimeRange:   rollupTimeRange("2024-01-01T00:00:00Z", "2024-01-08T00:00:00Z"),
		Granularity: insightsv1.Granularity_GRANULARITY_DAY.Enum(),
	}
	q, _, err := segmentationQueryForExecution(req, "proj_123", time.Now())
	if err != nil {
		t.Fatalf("segmentationQueryForExecution: %v", err)
	}
	if !strings.Contains(q.SQL(), rollupTable) {
		t.Errorf("eligible segmentation query must route to the rollup\nSQL:\n%s", q.SQL())
	}
}

// checkMaterializedDimsMatch verifies that BOTH ARRAY JOIN dimension lists in the
// migration (the incremental MV and the one-time backfill INSERT) contain exactly
// the Go materializedDims plus the total sentinel — no more, no less. It returns a
// descriptive error on any drift: a Go↔migration mismatch in either direction, or
// the MV and backfill copies diverging from each other. A whole-file substring
// check cannot catch either, because the two copies share the same tokens.
func checkMaterializedDimsMatch(sql string, goDims []string, total string) error {
	// `] AS dim` (whitespace-tolerant) terminates each list.
	blockRe := regexp.MustCompile(`(?s)ARRAY JOIN \[(.*?)\]\s+AS\s+dim`)
	blocks := blockRe.FindAllStringSubmatch(sql, -1)
	if len(blocks) != 2 {
		return fmt.Errorf("expected 2 ARRAY JOIN blocks (MV + backfill), found %d", len(blocks))
	}

	want := append([]string{total}, goDims...)
	slices.Sort(want)

	dimRe := regexp.MustCompile(`\('([^']*)',`) // first tuple element, e.g. ('$country',
	for i, block := range blocks {
		var got []string
		for _, m := range dimRe.FindAllStringSubmatch(block[1], -1) {
			got = append(got, m[1])
		}
		slices.Sort(got)
		if !slices.Equal(got, want) {
			return fmt.Errorf("ARRAY JOIN block %d dims %v != materializedDims+%q %v", i, got, total, want)
		}
	}
	return nil
}

// TestCheckMaterializedDimsMatch exercises the drift detector itself: a matching
// migration passes, and every drift direction (MV/backfill divergence, a
// migration-only dim, a Go-only dim) is caught.
func TestCheckMaterializedDimsMatch(t *testing.T) {
	const total = "$__total__"
	goDims := []string{"$a", "$b"}
	mk := func(mv, backfill string) string {
		return "CREATE MATERIALIZED VIEW x AS SELECT a ARRAY JOIN [\n" + mv + "\n] AS dim GROUP BY a;\n" +
			"INSERT INTO x SELECT a ARRAY JOIN [\n" + backfill + "\n] AS dim GROUP BY a;\n"
	}
	good := "('$__total__', ''), ('$a', x), ('$b', y)"

	if err := checkMaterializedDimsMatch(mk(good, good), goDims, total); err != nil {
		t.Errorf("matching migration flagged: %v", err)
	}
	if err := checkMaterializedDimsMatch(mk(good, "('$__total__', ''), ('$a', x)"), goDims, total); err == nil {
		t.Error("expected MV/backfill divergence (backfill missing $b) to be detected")
	}
	withExtra := "('$__total__', ''), ('$a', x), ('$b', y), ('$c', z)"
	if err := checkMaterializedDimsMatch(mk(withExtra, withExtra), goDims, total); err == nil {
		t.Error("expected migration-only dimension ($c) to be detected")
	}
	if err := checkMaterializedDimsMatch(mk(good, good), []string{"$a", "$b", "$d"}, total); err == nil {
		t.Error("expected Go-only dimension ($d) to be detected")
	}
}

// TestMaterializedDimsMatchMigration pins the Go dimension list to BOTH ARRAY JOIN
// lists in migration 006 (the MV and the backfill). Hand-coupled; fails loud on any
// drift in either direction or between the two copies.
func TestMaterializedDimsMatchMigration(t *testing.T) {
	const path = "../../../schema/clickhouse/migrations/006_create_dashboard_event_rollup.sql"
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	if err := checkMaterializedDimsMatch(string(data), materializedDims, totalDimName); err != nil {
		t.Error(err)
	}
}

// checkMaterializedDimExprsMatch verifies that both ARRAY JOIN blocks in migration
// 006 use PropertyExpr-compatible promoted-column expressions for each materialized
// breakdown dimension (not auto_properties map lookups).
func checkMaterializedDimExprsMatch(sql string, goDims []string) error {
	blockRe := regexp.MustCompile(`(?s)ARRAY JOIN \[(.*?)\]\s+AS\s+dim`)
	blocks := blockRe.FindAllStringSubmatch(sql, -1)
	if len(blocks) != 2 {
		return fmt.Errorf("expected 2 ARRAY JOIN blocks (MV + backfill), found %d", len(blocks))
	}
	for _, prop := range goDims {
		expr := chq.AutoPropertyProjectionFor(prop, "").StringSQL
		if expr == "" || strings.Contains(expr, "auto_properties") {
			return fmt.Errorf("property %q has no promoted-column SQL projection", prop)
		}
		// Allow flexible whitespace in the migration SQL formatting.
		tupleRe := regexp.MustCompile(
			fmt.Sprintf(`\(\s*'%s'\s*,\s*%s\s*\)`, regexp.QuoteMeta(prop), regexp.QuoteMeta(expr)),
		)
		for i, block := range blocks {
			if !tupleRe.MatchString(block[1]) {
				return fmt.Errorf("ARRAY JOIN block %d missing tuple ('%s', %s)", i, prop, expr)
			}
		}
	}
	if strings.Contains(sql, "auto_properties['$") {
		return fmt.Errorf("migration still reads promoted keys from auto_properties map")
	}
	return nil
}

// TestMigration006PromotedDimExprsMatch pins migration 006 dim_value expressions to
// AutoPropertyProjectionFor — the same SQL raw insights queries use via PropertyExpr.
func TestMigration006PromotedDimExprsMatch(t *testing.T) {
	const path = "../../../schema/clickhouse/migrations/006_create_dashboard_event_rollup.sql"
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	if err := checkMaterializedDimExprsMatch(string(data), materializedDims); err != nil {
		t.Error(err)
	}
}

func rollupTopKSpec(dim insightsv1.TopKQuery_Dimension, property, scopeKind string, metric insightsv1.AggregationType) *insightsv1.InsightQuerySpec {
	tk := &insightsv1.TopKQuery{
		Dimension: dim.Enum(),
		Metric:    metric.Enum(),
	}
	if property != "" {
		tk.Property = proto.String(property)
	}
	if scopeKind != "" {
		tk.Scope = &commonv1.EventFilter{Kind: proto.String(scopeKind)}
	}
	return &insightsv1.InsightQuerySpec{
		InsightType: insightsv1.InsightType_INSIGHT_TYPE_TOP_K.Enum(),
		TopK:        tk,
	}
}

func TestCanUseTopKRollup(t *testing.T) {
	total := insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL
	cases := []struct {
		name string
		spec *insightsv1.InsightQuerySpec
		want bool
	}{
		{"materialized property", rollupTopKSpec(insightsv1.TopKQuery_DIMENSION_PROPERTY, "$browser", "", total), true},
		{"materialized property with kind scope", rollupTopKSpec(insightsv1.TopKQuery_DIMENSION_PROPERTY, "$country", "page_view", total), true},
		{"event kind", rollupTopKSpec(insightsv1.TopKQuery_DIMENSION_EVENT_KIND, "", "", total), true},
		{"unique users", rollupTopKSpec(insightsv1.TopKQuery_DIMENSION_PROPERTY, "$browser", "", insightsv1.AggregationType_AGGREGATION_TYPE_UNIQUE_USERS), true},
		{"per user avg", rollupTopKSpec(insightsv1.TopKQuery_DIMENSION_EVENT_KIND, "", "", insightsv1.AggregationType_AGGREGATION_TYPE_PER_USER_AVG), true},
		{"unspecified metric resolves to total", rollupTopKSpec(insightsv1.TopKQuery_DIMENSION_PROPERTY, "$browser", "", insightsv1.AggregationType_AGGREGATION_TYPE_UNSPECIFIED), true},
		{"user dimension rejected", rollupTopKSpec(insightsv1.TopKQuery_DIMENSION_USER, "", "", total), false},
		{"non-materialized auto property rejected", rollupTopKSpec(insightsv1.TopKQuery_DIMENSION_PROPERTY, "$referrer", "", total), false},
		{"custom property rejected", rollupTopKSpec(insightsv1.TopKQuery_DIMENSION_PROPERTY, "plan", "", total), false},
		{"numeric metric rejected", rollupTopKSpec(insightsv1.TopKQuery_DIMENSION_PROPERTY, "$browser", "", insightsv1.AggregationType_AGGREGATION_TYPE_SUM), false},
		{"non-top-k insight type rejected", rollupTrendsSpec(total, "page_view", ""), false},
		{"missing top_k rejected", &insightsv1.InsightQuerySpec{InsightType: insightsv1.InsightType_INSIGHT_TYPE_TOP_K.Enum()}, false},
	}
	for _, c := range cases {
		if got := canUseTopKRollup(c.spec); got != c.want {
			t.Errorf("%s: canUseTopKRollup = %v, want %v", c.name, got, c.want)
		}
	}

	t.Run("filter_groups rejected", func(t *testing.T) {
		spec := rollupTopKSpec(insightsv1.TopKQuery_DIMENSION_PROPERTY, "$browser", "", total)
		spec.FilterGroups = []*insightsv1.FilterGroup{{}}
		if canUseTopKRollup(spec) {
			t.Error("expected rollup rejected when filter_groups present")
		}
	})

	t.Run("scope filters rejected", func(t *testing.T) {
		spec := rollupTopKSpec(insightsv1.TopKQuery_DIMENSION_PROPERTY, "$browser", "page_view", total)
		spec.TopK.Scope.Filters = []*commonv1.PropertyFilter{{Property: proto.String("$os")}}
		if canUseTopKRollup(spec) {
			t.Error("expected rollup rejected when scope has per-event filters")
		}
	})
}

func TestBuildTopKFromRollup(t *testing.T) {
	t.Run("property_dimension", func(t *testing.T) {
		req := rollupDayReq(rollupTopKSpec(insightsv1.TopKQuery_DIMENSION_PROPERTY, "$browser", "page_view", insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL))
		req.Spec.TopK.Limit = proto.Int32(5)
		q, err := buildTopKFromRollup(req, "proj_123")
		if err != nil {
			t.Fatalf("buildTopKFromRollup: %v", err)
		}
		sql := q.SQL()
		for _, want := range []string{
			"FROM dashboard_event_rollup_daily",
			"top_vals AS (",
			"dim_value AS top_dim",
			"if(dim_value IN (SELECT top_dim FROM top_vals), dim_value, '$others') AS dim_bucket",
			"AS is_others",
			"toFloat64(sum(cnt)) AS value",
			"GROUP BY dim_bucket, is_others",
			"ORDER BY is_others ASC, value DESC, dim_bucket ASC",
			"SETTINGS use_query_cache = 1, query_cache_ttl = 60",
		} {
			if !strings.Contains(sql, want) {
				t.Errorf("expected SQL to contain %q\nSQL:\n%s", want, sql)
			}
		}
		// The pre-aggregated rollup is tiny and bounded-cardinality, so it must
		// not carry the raw path's spill threshold; guards against an accidental
		// future WithSpillThreshold on the rollup builder.
		if strings.Contains(sql, "max_bytes_before_external_group_by") {
			t.Errorf("rollup path must not set a spill threshold, got: %s", sql)
		}
		// Both passes carry dim_name=$browser and the kind scope; day bounds map
		// [from, to) to inclusive whole days.
		wantArgs := map[any]int{"proj_123": 2, "$browser": 2, "page_view": 2, "2024-01-01": 2, "2024-01-07": 2, int64(5): 1}
		counts := map[any]int{}
		for _, a := range q.Args() {
			counts[a]++
		}
		for v, n := range wantArgs {
			if counts[v] != n {
				t.Errorf("expected arg %v ×%d, got %d: %v", v, n, counts[v], q.Args())
			}
		}
		if q.Limit() != 5 || q.Dimension() != insightsv1.TopKQuery_DIMENSION_PROPERTY {
			t.Errorf("expected Limit 5 / PROPERTY dimension, got %d / %s", q.Limit(), q.Dimension())
		}
	})

	t.Run("event_kind_dimension", func(t *testing.T) {
		req := rollupDayReq(rollupTopKSpec(insightsv1.TopKQuery_DIMENSION_EVENT_KIND, "", "", insightsv1.AggregationType_AGGREGATION_TYPE_UNIQUE_USERS))
		q, err := buildTopKFromRollup(req, "proj_123")
		if err != nil {
			t.Fatalf("buildTopKFromRollup: %v", err)
		}
		sql := q.SQL()
		for _, want := range []string{
			"kind AS top_dim",
			"if(kind IN (SELECT top_dim FROM top_vals), kind, '$others') AS dim_bucket",
			"toFloat64(uniqMerge(uniq_state)) AS value",
		} {
			if !strings.Contains(sql, want) {
				t.Errorf("expected SQL to contain %q\nSQL:\n%s", want, sql)
			}
		}
		// EVENT_KIND reads the synthetic $__total__ dimension rows.
		found := false
		for _, a := range q.Args() {
			if a == totalDimName {
				found = true
			}
		}
		if !found {
			t.Errorf("expected dim_name arg %q, got: %v", totalDimName, q.Args())
		}
	})

	t.Run("default_limit_when_unset", func(t *testing.T) {
		// buildTopKFromRollup carries its own limit==0 → defaultTopKLimit block,
		// independent of the raw BuildTopKQuery copy. Pin it: a regression dropping
		// it would send LIMIT 0 to the rollup (zero ranked rows) on the fast path
		// only, while the raw path stayed correct — exactly the kind of divergence
		// rollup_parity (which uses an explicit limit) would not catch.
		req := rollupDayReq(rollupTopKSpec(insightsv1.TopKQuery_DIMENSION_PROPERTY, "$browser", "page_view", insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL))
		// Limit deliberately left unset (0).
		q, err := buildTopKFromRollup(req, "proj_123")
		if err != nil {
			t.Fatalf("buildTopKFromRollup: %v", err)
		}
		if q.Limit() != defaultTopKLimit {
			t.Errorf("expected default limit %d, got %d", defaultTopKLimit, q.Limit())
		}
		found := false
		for _, a := range q.Args() {
			if a == int64(defaultTopKLimit) {
				found = true
			}
		}
		if !found {
			t.Errorf("expected LIMIT arg %d in args, got: %v", defaultTopKLimit, q.Args())
		}
	})

	t.Run("omit_others", func(t *testing.T) {
		// Mirrors buildTopKEvents' fast path: single aggregation + LIMIT, no
		// top_vals re-aggregation, is_others projected as a constant 0. The
		// query cache stays on; the spill threshold stays off (bounded rollup).
		req := rollupDayReq(rollupTopKSpec(insightsv1.TopKQuery_DIMENSION_PROPERTY, "$browser", "page_view", insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL))
		req.Spec.TopK.Limit = proto.Int32(5)
		req.Spec.TopK.OmitOthers = proto.Bool(true)
		q, err := buildTopKFromRollup(req, "proj_123")
		if err != nil {
			t.Fatalf("buildTopKFromRollup: %v", err)
		}
		sql := q.SQL()
		for _, want := range []string{
			"dim_value AS dim_bucket",
			"0 AS is_others",
			"GROUP BY dim_bucket",
			"ORDER BY value DESC, dim_bucket ASC",
			"LIMIT ?",
			"SETTINGS use_query_cache = 1, query_cache_ttl = 60",
		} {
			if !strings.Contains(sql, want) {
				t.Errorf("expected SQL to contain %q\nSQL:\n%s", want, sql)
			}
		}
		for _, bad := range []string{"top_vals", "'$others'", "is_others ASC", "max_bytes_before_external_group_by"} {
			if strings.Contains(sql, bad) {
				t.Errorf("omit-others rollup SQL must not contain %q\nSQL:\n%s", bad, sql)
			}
		}
		if q.Limit() != 5 {
			t.Errorf("expected Limit 5, got %d", q.Limit())
		}
	})
}

func TestTopKQueryForExecution_Dispatch(t *testing.T) {
	now := time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC)
	eligible := func() *insightsv1.QueryRequest {
		return rollupDayReq(rollupTopKSpec(insightsv1.TopKQuery_DIMENSION_PROPERTY, "$browser", "", insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL))
	}

	t.Run("aligned_eligible_uses_rollup", func(t *testing.T) {
		q, usedRollup, err := topKQueryForExecution(eligible(), "proj_123", now)
		if err != nil {
			t.Fatalf("topKQueryForExecution: %v", err)
		}
		if !usedRollup || !strings.Contains(q.SQL(), rollupTable) {
			t.Errorf("expected rollup-served query, usedRollup=%v SQL: %s", usedRollup, q.SQL())
		}
	})

	t.Run("omit_others_uses_rollup", func(t *testing.T) {
		// omit_others is orthogonal to eligibility: an eligible query still
		// routes to the rollup, and the dispatcher threads the flag through to
		// the omit fast path (single agg + LIMIT, no top_vals re-aggregation).
		// This is the routing guard the integration rollup_parity case can't be:
		// with no duplicate deliveries the raw and rollup omit paths are
		// byte-identical, so output parity alone can't catch a silent raw fallback.
		req := eligible()
		req.Spec.TopK.OmitOthers = proto.Bool(true)
		q, usedRollup, err := topKQueryForExecution(req, "proj_123", now)
		if err != nil {
			t.Fatalf("topKQueryForExecution: %v", err)
		}
		if !usedRollup || !strings.Contains(q.SQL(), rollupTable) {
			t.Errorf("omit query must route to rollup, usedRollup=%v SQL: %s", usedRollup, q.SQL())
		}
		if strings.Contains(q.SQL(), "top_vals") {
			t.Errorf("rollup omit must skip the top_vals re-aggregation, got: %s", q.SQL())
		}
	})

	t.Run("misaligned_window_falls_back_raw", func(t *testing.T) {
		req := eligible()
		req.TimeRange = rollupTimeRange("2024-01-01T12:00:00Z", "2024-01-08T00:00:00Z")
		q, usedRollup, err := topKQueryForExecution(req, "proj_123", now)
		if err != nil {
			t.Fatalf("topKQueryForExecution: %v", err)
		}
		if usedRollup || !strings.Contains(q.SQL(), "FROM events") {
			t.Errorf("expected raw-events query, usedRollup=%v SQL: %s", usedRollup, q.SQL())
		}
	})

	t.Run("user_dimension_falls_back_raw", func(t *testing.T) {
		req := rollupDayReq(rollupTopKSpec(insightsv1.TopKQuery_DIMENSION_USER, "", "", insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL))
		q, usedRollup, err := topKQueryForExecution(req, "proj_123", now)
		if err != nil {
			t.Fatalf("topKQueryForExecution: %v", err)
		}
		if usedRollup || !strings.Contains(q.SQL(), "FROM events") {
			t.Errorf("expected raw-events query, usedRollup=%v SQL: %s", usedRollup, q.SQL())
		}
	})
}
