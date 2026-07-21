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
// The switch is exhaustive over AggregationType rather than a two-member
// inclusion list, because the list form failed OPEN: an aggregation added to the
// proto and not considered here silently returned false, admitting cookieless
// ids into a metric that may well count people — inflating user counts with no
// error at any layer. Nothing else stops it either; protovalidate's
// enum.defined_only admits any newly defined member, rollupAggExpr's default is
// a silent fall back to the raw path, and aggregationExpr's default returns
// count(*). TestExcludeCookielessForAgg_IsExhaustive ranges over the enum, so
// adding a member without deciding here fails the build rather than shipping.
func excludeCookielessForAgg(spec *insightsv1.InsightQuerySpec, agg insightsv1.AggregationType) bool {
	if spec.GetIncludeCookieless() {
		return false
	}
	switch agg {
	// Counts or divides by people: a daily-rotating id is a new person every day.
	case insightsv1.AggregationType_AGGREGATION_TYPE_UNIQUE_USERS,
		insightsv1.AggregationType_AGGREGATION_TYPE_PER_USER_AVG:
		return true
	// Counts events or event properties, never people. A consent-rejector's
	// pageview is still a pageview, so these must keep counting all traffic —
	// excluding here would UNDER-count, which is its own silent wrong answer.
	case insightsv1.AggregationType_AGGREGATION_TYPE_UNSPECIFIED,
		insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL,
		insightsv1.AggregationType_AGGREGATION_TYPE_SUM,
		insightsv1.AggregationType_AGGREGATION_TYPE_AVG,
		insightsv1.AggregationType_AGGREGATION_TYPE_MIN,
		insightsv1.AggregationType_AGGREGATION_TYPE_MAX:
		return false
	default:
		// Unreachable while the contract test passes. Excluding is the safer of
		// two wrong answers: it under-counts a volume metric visibly, where
		// including would over-count a people metric invisibly.
		return true
	}
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
// the raw events table, NOT a CTE, and the predicate must land on those event
// rows: in retention the CTE is `c` (the cohort side of `cohorts c INNER JOIN
// events e`), while top K joins a subquery aliased `i` over its own
// `latest_profiles`/`per_user`/`ranked` CTEs. Returns the zero Condition when
// exclude is false, which Where skips.
func cookielessExclusionCond(exclude bool, alias string) chq.Condition {
	col := "distinct_id"
	if alias != "" {
		col = alias + ".distinct_id"
	}
	return chq.When(exclude, chq.RawCond("NOT startsWith("+col+", '"+cookieless.IDPrefix+"')"))
}
