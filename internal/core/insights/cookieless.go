package insights

import (
	"github.com/pug-sh/pug/internal/cookieless"
	chq "github.com/pug-sh/pug/internal/core/clickhouse"
	insightsv1 "github.com/pug-sh/pug/internal/gen/proto/shared/insights/v1"
)

// excludeCookielessForAgg reports whether a query computing agg must exclude
// cookieless-prefixed distinct_ids: user-counting metrics exclude them by
// default (a daily-rotating id counts one human as a new user every day),
// while totals and numeric aggregations always count all traffic — a
// consent-rejector's pageview is still a pageview. The spec toggle re-admits
// them. Session metrics never consult this (they never read distinct_id).
func excludeCookielessForAgg(spec *insightsv1.InsightQuerySpec, agg insightsv1.AggregationType) bool {
	if spec.GetIncludeCookieless() {
		return false
	}
	return agg == insightsv1.AggregationType_AGGREGATION_TYPE_UNIQUE_USERS ||
		agg == insightsv1.AggregationType_AGGREGATION_TYPE_PER_USER_AVG
}

// excludeCookielessForPersons is the person-based-insight variant: funnel,
// retention, user flow and USER-dimension top K resolve events per person,
// which a daily-rotating id structurally breaks across days (retention would be
// identically zero), so they exclude cookieless unless the spec opts in.
func excludeCookielessForPersons(spec *insightsv1.InsightQuerySpec) bool {
	return !spec.GetIncludeCookieless()
}

// cookielessExclusionCond is the raw-path predicate. alias "" targets an
// unaliased events scan; a non-empty alias qualifies the column where `events`
// is joined under a name — retention and top K both join it as `e`. Note `e` is
// the raw events table, NOT a CTE: in those queries the CTE is `c` (the cohort
// side of `cohorts c INNER JOIN events e`), and the predicate must land on the
// event rows. Returns the zero Condition when exclude is false, which Where skips.
func cookielessExclusionCond(exclude bool, alias string) chq.Condition {
	col := "distinct_id"
	if alias != "" {
		col = alias + ".distinct_id"
	}
	return chq.When(exclude, chq.RawCond("NOT startsWith("+col+", '"+cookieless.IDPrefix+"')"))
}
