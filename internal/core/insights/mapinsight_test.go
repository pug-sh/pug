package insights

import (
	"strings"
	"testing"
	"time"

	"buf.build/go/protovalidate"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	commonv1 "github.com/pug-sh/pug/internal/gen/proto/common/v1"
	insightsv1 "github.com/pug-sh/pug/internal/gen/proto/shared/insights/v1"
)

func mapRequest(m *insightsv1.MapQuery) *insightsv1.QueryRequest {
	from := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	return &insightsv1.QueryRequest{
		Spec: &insightsv1.InsightQuerySpec{
			InsightType: insightsv1.InsightType_INSIGHT_TYPE_MAP.Enum(),
			Map:         m,
		},
		Granularity: insightsv1.Granularity_GRANULARITY_DAY.Enum(),
		TimeRange: &commonv1.TimeRange{
			From: timestamppb.New(from),
			To:   timestamppb.New(from.Add(7 * 24 * time.Hour)),
		},
	}
}

func TestTopKRequestForMap(t *testing.T) {
	t.Run("fixes the dimension to country with no top-N and no others bucket", func(t *testing.T) {
		out := topKRequestForMap(mapRequest(&insightsv1.MapQuery{}))
		tk := out.GetSpec().GetTopK()

		if got := out.GetSpec().GetInsightType(); got != insightsv1.InsightType_INSIGHT_TYPE_TOP_K {
			t.Errorf("insight type = %s, want TOP_K", got)
		}
		if out.GetSpec().GetMap() != nil {
			t.Error("map should be cleared on the rewritten spec")
		}
		if got := tk.GetDimension(); got != insightsv1.TopKQuery_DIMENSION_PROPERTY {
			t.Errorf("dimension = %s, want PROPERTY", got)
		}
		if got := tk.GetProperty(); got != "$country" {
			t.Errorf("property = %q, want $country", got)
		}
		if got := tk.GetLimit(); got != mapCountryLimit {
			t.Errorf("limit = %d, want %d", got, mapCountryLimit)
		}
		if !tk.GetOmitOthers() {
			t.Error("omit_others = false, want true — an $others bucket cannot be drawn on a map")
		}
	})

	t.Run("carries the metric and scope through", func(t *testing.T) {
		out := topKRequestForMap(mapRequest(&insightsv1.MapQuery{
			Scope:          &commonv1.EventFilter{Kind: proto.String("page_view")},
			Metric:         insightsv1.AggregationType_AGGREGATION_TYPE_SUM.Enum(),
			MetricProperty: proto.String("order_amount"),
		}))
		tk := out.GetSpec().GetTopK()

		if got := tk.GetScope().GetKind(); got != "page_view" {
			t.Errorf("scope kind = %q, want page_view", got)
		}
		if got := tk.GetMetric(); got != insightsv1.AggregationType_AGGREGATION_TYPE_SUM {
			t.Errorf("metric = %s, want SUM", got)
		}
		if got := tk.GetMetricProperty(); got != "order_amount" {
			t.Errorf("metric_property = %q, want order_amount", got)
		}
	})

	t.Run("shares no state with the caller's request", func(t *testing.T) {
		req := mapRequest(&insightsv1.MapQuery{
			Scope: &commonv1.EventFilter{Kind: proto.String("page_view")},
		})
		before := proto.CloneOf(req)
		out := topKRequestForMap(req)
		if !proto.Equal(req, before) {
			t.Error("the request was mutated; every tile on a board shares the TimeRange it is handed")
		}
		if out.GetSpec().GetTopK().GetScope() == req.GetSpec().GetMap().GetScope() {
			t.Error("scope is aliased into the rewrite rather than cloned")
		}
	})

	// The rewrite carries spec-level state by cloning rather than by copying named
	// fields. Dropping filter_groups would both return unfiltered numbers and make the
	// query rollup-eligible, so the two assertions belong together.
	t.Run("carries spec-level filters through", func(t *testing.T) {
		req := mapRequest(&insightsv1.MapQuery{})
		req.Spec.IncludeCookieless = proto.Bool(true)
		req.Spec.FilterGroups = []*insightsv1.FilterGroup{{
			Filters: []*commonv1.PropertyFilter{{
				Property: proto.String("$browser"),
				Operator: commonv1.FilterOperator_FILTER_OPERATOR_EQUALS.Enum(),
				Value:    proto.String("Chrome"),
			}},
		}}
		out := topKRequestForMap(req)

		if !out.GetSpec().GetIncludeCookieless() {
			t.Error("include_cookieless was dropped by the rewrite")
		}
		if len(out.GetSpec().GetFilterGroups()) != 1 {
			t.Fatalf("filter_groups = %d, want 1", len(out.GetSpec().GetFilterGroups()))
		}
		if canUseTopKRollup(out.GetSpec()) {
			t.Error("a filtered map must fall back to the raw builder")
		}
	})

	// mapCountryLimit deliberately exceeds TopKQuery.limit's lte:100. That is only safe
	// because the rewrite runs downstream of every protovalidate call — this pins the
	// tradeoff so a defensive re-validate fails here rather than on every map tile.
	t.Run("is deliberately not schema-valid", func(t *testing.T) {
		out := topKRequestForMap(mapRequest(&insightsv1.MapQuery{}))
		if err := protovalidate.Validate(out); err == nil {
			t.Error("the rewritten request now validates; mapCountryLimit can be dropped to the wire cap")
		}
	})

	// PROPERTY dimension + a numeric metric is the shape only map reaches by default —
	// every other top-K numeric test goes through the USER-dimension builder.
	t.Run("builds a numeric metric over the country dimension", func(t *testing.T) {
		out := topKRequestForMap(mapRequest(&insightsv1.MapQuery{
			Metric:         insightsv1.AggregationType_AGGREGATION_TYPE_SUM.Enum(),
			MetricProperty: proto.String("order_amount"),
		}))
		q, err := BuildTopKQuery(out, "proj_123")
		if err != nil {
			t.Fatalf("BuildTopKQuery: %v", err)
		}
		if !strings.Contains(q.SQL(), "sum(") || !strings.Contains(q.SQL(), "country") {
			t.Errorf("SQL does not sum over country:\n%s", q.SQL())
		}
	})

	t.Run("rides the top-K rollup fast path", func(t *testing.T) {
		out := topKRequestForMap(mapRequest(&insightsv1.MapQuery{
			Scope:  &commonv1.EventFilter{Kind: proto.String("page_view")},
			Metric: insightsv1.AggregationType_AGGREGATION_TYPE_UNIQUE_USERS.Enum(),
		}))
		if !canUseTopKRollup(out.GetSpec()) {
			t.Error("a scoped, unfiltered map should be rollup-eligible: $country is a materialized dim")
		}
	})
}

// $country is client-writable whenever geo enrichment resolves nothing, so the junk
// cases here are reachable from an untrusted caller, not hypothetical.
func TestKeepISOCountries(t *testing.T) {
	row := func(v string) *insightsv1.TopKRow {
		return &insightsv1.TopKRow{DimensionValue: proto.String(v), Value: proto.Float64(1)}
	}
	got := keepISOCountries(&insightsv1.TopKResult{
		Rows: []*insightsv1.TopKRow{row("US"), row(""), row("USA"), row("unknown"), row("us"), row("G1"), row("GB")},
	})
	var kept []string
	for _, r := range got.GetRows() {
		kept = append(kept, r.GetDimensionValue())
	}
	if len(kept) != 2 || kept[0] != "US" || kept[1] != "GB" {
		t.Errorf("rows = %v, want [US GB]", kept)
	}
}
