package insights

import (
	"google.golang.org/protobuf/proto"

	insightsv1 "github.com/pug-sh/pug/internal/gen/proto/shared/insights/v1"
	"github.com/pug-sh/pug/internal/geo"
)

// mapCountryLimit bounds the country set a map returns. It deliberately exceeds
// TopKQuery.limit's lte:100 wire cap: the rewrite happens inside ExecuteQuery,
// downstream of every protovalidate call, so the rewritten request must never be
// re-validated.
const mapCountryLimit = 250

// topKRequestForMap rewrites a MAP request into the equivalent top-K one, which
// is what keeps the raw builders, the rollup predicate and the result assembly
// unaware that map exists.
func topKRequestForMap(req *insightsv1.QueryRequest) *insightsv1.QueryRequest {
	out := proto.CloneOf(req)
	m := out.GetSpec().GetMap()
	out.Spec.InsightType = insightsv1.InsightType_INSIGHT_TYPE_TOP_K.Enum()
	out.Spec.Map = nil
	out.Spec.TopK = &insightsv1.TopKQuery{
		Dimension:      insightsv1.TopKQuery_DIMENSION_PROPERTY.Enum(),
		Property:       proto.String(geo.PropCountry),
		Scope:          m.GetScope(),
		Metric:         m.GetMetric().Enum(),
		MetricProperty: proto.String(m.GetMetricProperty()),
		Limit:          proto.Int32(mapCountryLimit),
		OmitOthers:     proto.Bool(true),
	}
	return out
}

// keepISOCountries drops every row a choropleth cannot place, so the response
// matches the ISO alpha-2 contract it advertises. That means membership in the
// real code set, not a two-letter shape check: the enricher overwrites $country
// only when the geo provider resolves one, so "USA", "unknown" and unassigned
// codes like "ZZ" all survive ingestion from a client payload.
//
// Ranking happens before this filter, so a project flooded with junk codes can
// still push real countries past mapCountryLimit.
func keepISOCountries(result *insightsv1.TopKResult) *insightsv1.TopKResult {
	kept := result.GetRows()[:0]
	for _, row := range result.GetRows() {
		if geo.IsCountryCode(row.GetDimensionValue()) {
			kept = append(kept, row)
		}
	}
	result.Rows = kept
	return result
}
