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

	"github.com/pug-sh/pug/internal/cookieless"
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

// rollupEventSpec names one event of a multi-event trends spec. Unlike
// rollupMultiEventTrendsSpec (which hardcodes TOTAL everywhere) it lets a test
// vary aggregation ACROSS events — the shape nothing in InsightQuerySpec's CEL
// rules forbids, and the one the shared top_vals CTE mis-ranked.
type rollupEventSpec struct {
	kind string
	agg  insightsv1.AggregationType
}

func rollupTrendsSpecFor(breakdown string, evs ...rollupEventSpec) *insightsv1.InsightQuerySpec {
	spec := &insightsv1.InsightQuerySpec{InsightType: insightsv1.InsightType_INSIGHT_TYPE_TRENDS.Enum()}
	for _, e := range evs {
		spec.Events = append(spec.Events, &insightsv1.EventQuery{
			Event:       &commonv1.EventFilter{Kind: proto.String(e.kind)},
			Aggregation: e.agg.Enum(),
		})
	}
	if breakdown != "" {
		spec.Breakdowns = []*insightsv1.Breakdown{{Property: proto.String(breakdown)}}
	}
	return spec
}

// cteBody returns the text of the named CTE so an assertion can bind a predicate
// to the specific event whose ranking it governs, rather than counting
// occurrences across the whole statement (which cannot tell WHICH event a
// predicate landed on — exactly the confusion that let C-1 ship).
func cteBody(t *testing.T, sql, name string) string {
	t.Helper()
	open := name + " AS ("
	i := strings.Index(sql, open)
	if i < 0 {
		t.Fatalf("CTE %s not found in:\n%s", name, sql)
	}
	rest := sql[i+len(open):]
	j := strings.Index(rest, "\n)")
	if j < 0 {
		t.Fatalf("unterminated CTE %s in:\n%s", name, sql)
	}
	return rest[:j]
}

// TestBuildTrendsFromRollup_TopValsMirrorsApplyTrendsTopN pins the three axes on
// which the rollup's SQL top-N must equal the raw path's applyTrendsTopN. The
// rollup returns breakdownLimit=0, so GroupSeries never re-ranks and this SQL is
// the SOLE arbiter of which breakdown values are named vs folded into $others.
// Any drift here silently changes chart contents rather than failing a query.
func TestBuildTrendsFromRollup_TopValsMirrorsApplyTrendsTopN(t *testing.T) {
	total := insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL
	uu := insightsv1.AggregationType_AGGREGATION_TYPE_UNIQUE_USERS

	t.Run("ranks_by_each_events_own_metric", func(t *testing.T) {
		req := rollupDayReq(rollupTrendsSpecFor("$country", rollupEventSpec{"page_view", uu}))
		q, err := buildTrendsFromRollup(req, "proj_123")
		if err != nil {
			t.Fatal(err)
		}
		// applyTrendsTopN accumulates `entry.total += r.Value` bucket by bucket, so
		// the rollup must (a) evaluate this event's metric at the query's own bucket
		// grain and (b) rank by the SUM of those values.
		//
		// Ranking by sum(cnt) would order countries by page views — a plain "unique
		// users by country" tile naming wrong values. Ranking by a WINDOW-WIDE
		// uniqMerge is subtler and was the last axis to be fixed: it equals the
		// per-bucket sum for TOTAL, so only a multi-day non-additive-metric corpus
		// exposes it (rollup_parity_trends_multiday_unique_users_top_n).
		if !strings.Contains(cteBody(t, q.SQL(), "top_grain_0"), "toFloat64(uniqMerge(uniq_state)) AS v") {
			t.Errorf("UNIQUE_USERS grain CTE must evaluate uniqMerge per bucket, not raw event volume:\n%s", q.SQL())
		}
		if !strings.Contains(cteBody(t, q.SQL(), "top_vals_0"), "ORDER BY sum(v) DESC, dim_value ASC") {
			t.Errorf("top-N must rank by the SUM of per-bucket values, matching applyTrendsTopN:\n%s", q.SQL())
		}
	})

	t.Run("ranks_per_event_kind", func(t *testing.T) {
		req := rollupDayReq(rollupTrendsSpecFor("$country",
			rollupEventSpec{"page_view", total}, rollupEventSpec{"signup", total}))
		q, err := buildTrendsFromRollup(req, "proj_123")
		if err != nil {
			t.Fatal(err)
		}
		sql := q.SQL()
		// applyTrendsTopN partitions byEventKind BEFORE ranking. One global top-N
		// would let a high-volume kind dictate a low-volume kind's named values.
		for _, want := range []string{"top_vals_0 AS (", "top_vals_1 AS ("} {
			if !strings.Contains(sql, want) {
				t.Errorf("expected per-event ranking CTE %q:\n%s", want, sql)
			}
		}
		if got := strings.Count(sql, "AND kind = ?"); got != 4 {
			t.Errorf("expected 2 CTEs + 2 branches each scoped to one kind (4), got %d:\n%s", got, sql)
		}
	})
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
		"top_vals_0",
		"dim_name",
		"if(dim_value IN (SELECT dim_value FROM top_vals_0), dim_value, '$others') AS breakdown_0",
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

const (
	migration006Path = "../../../schema/clickhouse/migrations/006_create_dashboard_event_rollup.sql"
	migration009Path = "../../../schema/clickhouse/migrations/009_extend_dashboard_event_rollup.sql"
	migration011Path = "../../../schema/clickhouse/migrations/011_cookieless_identity.sql"
)

func readMigration(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	return string(data)
}

// migrationUpSection returns the `-- +goose Up` portion of a migration file so
// dim checks don't trip over Down-section restatements of older MV definitions.
func migrationUpSection(t *testing.T, path string) string {
	t.Helper()
	sql := readMigration(t, path)
	up, _, found := strings.Cut(sql, "-- +goose Down")
	if !found {
		t.Fatalf("%s has no goose Down marker", path)
	}
	return up
}

// extractArrayJoinBlocks returns each `ARRAY JOIN [ ... ] AS dim` tuple-list
// body, in order of appearance.
func extractArrayJoinBlocks(sql string) []string {
	blockRe := regexp.MustCompile(`(?s)ARRAY JOIN \[(.*?)\]\s+AS\s+dim`)
	var out []string
	for _, m := range blockRe.FindAllStringSubmatch(sql, -1) {
		out = append(out, m[1])
	}
	return out
}

// arrayJoinDimNames extracts the first tuple element of each dim tuple in a
// block, e.g. ('$country', ...) → $country.
func arrayJoinDimNames(block string) []string {
	dimRe := regexp.MustCompile(`\('([^']*)',`)
	var out []string
	for _, m := range dimRe.FindAllStringSubmatch(block, -1) {
		out = append(out, m[1])
	}
	return out
}

// checkDimList verifies a block carries exactly want (order-insensitive).
func checkDimList(block string, want []string) error {
	got := arrayJoinDimNames(block)
	slices.Sort(got)
	wantSorted := slices.Clone(want)
	slices.Sort(wantSorted)
	if !slices.Equal(got, wantSorted) {
		return fmt.Errorf("ARRAY JOIN dims %v != expected %v", got, wantSorted)
	}
	return nil
}

// TestCheckDimListHelpers exercises the drift detector itself: a matching block
// passes, and every drift direction (a migration-only dim, a Go-only dim) is
// caught.
func TestCheckDimListHelpers(t *testing.T) {
	good := "('$__total__', ''), ('$a', x), ('$b', y)"
	if err := checkDimList(good, []string{"$__total__", "$a", "$b"}); err != nil {
		t.Errorf("matching block flagged: %v", err)
	}
	if err := checkDimList(good, []string{"$__total__", "$a"}); err == nil {
		t.Error("expected migration-only dimension ($b) to be detected")
	}
	if err := checkDimList(good, []string{"$__total__", "$a", "$b", "$c"}); err == nil {
		t.Error("expected Go-only dimension ($c) to be detected")
	}
}

// TestMaterializedDimsMatchMigration pins the Go dimension lists to the LATEST
// event-rollup MV definition — migration 011's restated MODIFY QUERY, which
// must carry exactly materializedDims + $__total__ (011 changes no dims; it
// adds the cookieless key column, so it has no backfill block). Also pins the
// internal coherence of the per-migration Go groups.
func TestMaterializedDimsMatchMigration(t *testing.T) {
	up := migrationUpSection(t, migration011Path)
	blocks := extractArrayJoinBlocks(up)
	if len(blocks) != 1 {
		t.Fatalf("expected 1 ARRAY JOIN block in 011 Up (MODIFY QUERY only, no backfill), found %d", len(blocks))
	}
	if err := checkDimList(blocks[0], append([]string{totalDimName}, materializedDims...)); err != nil {
		t.Errorf("MODIFY QUERY block: %v", err)
	}
	// The per-migration groups must not overlap: a dim in two groups would be
	// backfilled by both, doubling its cnt. (materializedDims being their
	// union needs no assertion — slices.Concat makes it so.)
	for _, dim := range eventRollupDims009 {
		if slices.Contains(eventRollupDims006, dim) {
			t.Errorf("dim %s is in both the 006 and 009 groups; 009's backfill would double its cnt", dim)
		}
	}
}

// deleteDimNames extracts the dim_name list of the rollup DELETE guard.
func deleteDimNames(sql string) []string {
	re := regexp.MustCompile(`(?s)ALTER TABLE dashboard_event_rollup_daily DELETE\s+WHERE dim_name IN \((.*?)\)`)
	m := re.FindStringSubmatch(sql)
	if m == nil {
		return nil
	}
	var out []string
	for _, q := range regexp.MustCompile(`'([^']*)'`).FindAllStringSubmatch(m[1], -1) {
		out = append(out, q[1])
	}
	return out
}

// TestMigration009BackfillDeleteCoversNewDims pins the delete-before-backfill
// guard to eventRollupDims009. That DELETE is the only thing making the delta
// backfill re-runnable — cnt is SimpleAggregateFunction(sum), so a partial
// INSERT plus the natural re-run would double it permanently. A dim added to
// the backfill but missed here would silently lose that protection for itself.
func TestMigration009BackfillDeleteCoversNewDims(t *testing.T) {
	got := deleteDimNames(migrationUpSection(t, migration009Path))
	if got == nil {
		t.Fatal("009 Up carries no dashboard_event_rollup_daily DELETE guard before the delta backfill")
	}
	slices.Sort(got)
	want := slices.Clone(eventRollupDims009)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("DELETE dim_name list %v != eventRollupDims009 %v", got, want)
	}
}

// TestMigration006Frozen pins migration 006 to its historical content: both
// ARRAY JOIN blocks carry exactly the legacy ten dims + $__total__, with the
// promoted-column expressions raw queries used at the time. Guards against
// editing a shipped migration instead of adding a new one.
func TestMigration006Frozen(t *testing.T) {
	sql := readMigration(t, migration006Path)
	blocks := extractArrayJoinBlocks(sql)
	if len(blocks) != 2 {
		t.Fatalf("expected 2 ARRAY JOIN blocks in 006 (MV + backfill), found %d", len(blocks))
	}
	for i, block := range blocks {
		if err := checkDimList(block, append([]string{totalDimName}, eventRollupDims006...)); err != nil {
			t.Errorf("block %d: %v", i, err)
		}
	}
	if err := checkDimExprs(blocks, eventRollupDims006); err != nil {
		t.Error(err)
	}
	if strings.Contains(sql, "auto_properties['$") {
		t.Error("migration 006 must not read promoted keys from the auto_properties map")
	}
}

// checkDimExprs verifies every given dim appears in every given block as a
// tuple with its PropertyExpr-compatible promoted-column expression (not an
// auto_properties map lookup) — the same SQL raw insights queries use.
func checkDimExprs(blocks []string, dims []string) error {
	for _, prop := range dims {
		expr := chq.AutoPropertyProjectionFor(prop, "").StringSQL
		if expr == "" || strings.Contains(expr, "auto_properties") {
			return fmt.Errorf("property %q has no promoted-column SQL projection", prop)
		}
		// Allow flexible whitespace in the migration SQL formatting.
		tupleRe := regexp.MustCompile(
			fmt.Sprintf(`\(\s*'%s'\s*,\s*%s\s*\)`, regexp.QuoteMeta(prop), regexp.QuoteMeta(expr)),
		)
		for i, block := range blocks {
			if !tupleRe.MatchString(block) {
				return fmt.Errorf("ARRAY JOIN block %d missing tuple ('%s', %s)", i, prop, expr)
			}
		}
	}
	return nil
}

// TestMigration011PromotedDimExprsMatch pins the latest MV's dim_value
// expressions (migration 011's restated MODIFY QUERY) to
// AutoPropertyProjectionFor. The auto_properties ban is scoped to the ARRAY
// JOIN block because reading the map is legitimate in a derivation mutation
// (008's, over historical rows that were never split) and never in a rollup
// that must read the promoted columns.
func TestMigration011PromotedDimExprsMatch(t *testing.T) {
	up := migrationUpSection(t, migration011Path)
	blocks := extractArrayJoinBlocks(up)
	if len(blocks) != 1 {
		t.Fatalf("expected 1 ARRAY JOIN block in 011 Up, found %d", len(blocks))
	}
	if err := checkDimExprs(blocks, materializedDims); err != nil {
		t.Errorf("MODIFY QUERY block: %v", err)
	}
	for i, block := range blocks {
		if strings.Contains(block, "auto_properties['$") {
			t.Errorf("ARRAY JOIN block %d reads promoted keys from the auto_properties map", i)
		}
	}
}

// TestMigration009Frozen pins migration 009 to its historical content, exactly
// as TestMigration006Frozen freezes 006: now that 011 restates the MV, 009's
// file must never be edited again. Its MODIFY QUERY block carries the full
// 21-dim list, its delta backfill exactly the 009 group, with promoted-column
// expressions in both. (The DELETE-guard list is pinned separately by
// TestMigration009BackfillDeleteCoversNewDims.)
func TestMigration009Frozen(t *testing.T) {
	up := migrationUpSection(t, migration009Path)
	blocks := extractArrayJoinBlocks(up)
	if len(blocks) != 2 {
		t.Fatalf("expected 2 ARRAY JOIN blocks in 009 Up (MODIFY QUERY + delta backfill), found %d", len(blocks))
	}
	if err := checkDimList(blocks[0], append([]string{totalDimName}, materializedDims...)); err != nil {
		t.Errorf("MODIFY QUERY block: %v", err)
	}
	if err := checkDimList(blocks[1], eventRollupDims009); err != nil {
		t.Errorf("delta backfill block: %v", err)
	}
	if err := checkDimExprs(blocks[:1], materializedDims); err != nil {
		t.Errorf("MODIFY QUERY block exprs: %v", err)
	}
	if err := checkDimExprs(blocks[1:], eventRollupDims009); err != nil {
		t.Errorf("delta backfill block exprs: %v", err)
	}
	for i, block := range blocks {
		if strings.Contains(block, "auto_properties['$") {
			t.Errorf("ARRAY JOIN block %d reads promoted keys from the auto_properties map", i)
		}
	}
	// 009 predates the cookieless key column; its file must not grow one.
	if strings.Contains(up, "cookieless") {
		t.Error("migration 009 is frozen and must not mention cookieless — that is 011's job")
	}
}

// TestMigration011CookielessPrefixMatchesGo pins the SQL prefix literals to
// cookieless.IDPrefix — the id format is permanent storage, so drift between
// the Go constant and the migration would silently corrupt exclusion.
func TestMigration011CookielessPrefixMatchesGo(t *testing.T) {
	up := migrationUpSection(t, migration011Path)
	want := "startsWith(distinct_id, '" + cookieless.IDPrefix + "')"
	if got := strings.Count(up, want); got != 2 {
		t.Errorf("011 Up must reference %q exactly twice (activity WHERE + rollup key), got %d", want, got)
	}
}

// TestMigration011ActivityStatesExcludeCookieless pins the derived-persons
// exclusion: the activity MV must filter cookieless ids or every daily
// rotation mints a ghost person.
func TestMigration011ActivityStatesExcludeCookieless(t *testing.T) {
	up := migrationUpSection(t, migration011Path)
	if !strings.Contains(up, "ALTER TABLE distinct_id_activity_states_mv MODIFY QUERY") {
		t.Fatal("011 must MODIFY QUERY the activity-states MV (never DROP->CREATE)")
	}
	if !strings.Contains(up, "WHERE NOT startsWith(distinct_id, '"+cookieless.IDPrefix+"')") {
		t.Error("activity-states MV must exclude cookieless ids")
	}
	if !strings.Contains(up, "minState(occur_time)") {
		t.Error("011 must restate the full 005 state list")
	}
}

// TestMigration011RollupCookielessKeyColumn pins the rollup flag: computed
// from the prefix, part of the sorting key, present in the MV GROUP BY.
func TestMigration011RollupCookielessKeyColumn(t *testing.T) {
	up := migrationUpSection(t, migration011Path)
	for _, want := range []string{
		// No DEFAULT expression: ClickHouse forbids defaulted columns joining a
		// sorting key (code 36); the bare UInt8 reads type-default 0 on old rows.
		"ADD COLUMN IF NOT EXISTS cookieless UInt8,",
		"MODIFY ORDER BY (project_id, kind, dim_name, day, dim_value, cookieless)",
		"toUInt8(startsWith(distinct_id, '" + cookieless.IDPrefix + "')) AS cookieless",
		"GROUP BY project_id, day, kind, dim_name, dim_value, cookieless",
	} {
		if !strings.Contains(up, want) {
			t.Errorf("011 Up missing %q", want)
		}
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
