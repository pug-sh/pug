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
	// counting the partial boundary days (R2-B). The raw builder filters exact instants.
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

func TestRollupWindowAligned(t *testing.T) {
	now := time.Date(2026, 5, 25, 14, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		tr   *commonv1.TimeRange
		want bool
	}{
		{"both midnight aligned", rollupTimeRange("2024-01-01T00:00:00Z", "2024-01-08T00:00:00Z"), true},
		{"from midnight, to=now (live preset)", &commonv1.TimeRange{From: timestamppb.New(startOfDayUTC(now)), To: timestamppb.New(now)}, true},
		{"from midnight, to future", &commonv1.TimeRange{From: timestamppb.New(startOfDayUTC(now)), To: timestamppb.New(now.Add(time.Hour))}, true},
		{"from mid-day rejected", rollupTimeRange("2024-01-01T06:00:00Z", "2024-01-08T00:00:00Z"), false},
		{"to past mid-day rejected", rollupTimeRange("2024-01-01T00:00:00Z", "2024-01-04T06:00:00Z"), false},
	}
	for _, c := range cases {
		if got := rollupWindowAligned(c.tr, now); got != c.want {
			t.Errorf("%s: rollupWindowAligned = %v, want %v", c.name, got, c.want)
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

// TestMaterializedDimsMatchMigration pins the Go dimension list to the MV's
// ARRAY JOIN list in migration 006. They are coupled by hand; this fails loud if
// they drift (a new dimension added to one but not the other).
func TestMaterializedDimsMatchMigration(t *testing.T) {
	const path = "../../../schema/clickhouse/migrations/006_create_dashboard_event_rollup.sql"
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	sql := string(data)
	for _, d := range materializedDims {
		// Each dim appears as the first tuple element, e.g. ('$country', ...).
		if !strings.Contains(sql, "('"+d+"',") {
			t.Errorf("materializedDims has %q but migration 006 has no ('%s', ...) ARRAY JOIN entry", d, d)
		}
	}
	if !strings.Contains(sql, "('"+totalDimName+"',") {
		t.Errorf("migration 006 missing the ('%s', '') total row", totalDimName)
	}
}
