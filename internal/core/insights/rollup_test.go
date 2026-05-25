package insights

import (
	"testing"

	"google.golang.org/protobuf/proto"

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
