package insights

import (
	chq "github.com/pug-sh/pug/internal/core/clickhouse"
	insightsv1 "github.com/pug-sh/pug/internal/gen/proto/shared/insights/v1"
)

// excludeBots is not metric- or insight-scoped like the cookieless toggle: a
// monitor's pageview is as wrong in a pageview count as in a visitor count.
func excludeBots(spec *insightsv1.InsightQuerySpec) bool {
	return !spec.GetIncludeBots()
}

// botExclusionCond is the row-level `bot = 0`. bot is a key column on both
// rollups, so the same predicate serves the fast path; inclusion is no predicate.
func botExclusionCond(exclude bool, alias string) chq.Condition {
	col := "bot"
	if alias != "" {
		col = alias + ".bot"
	}
	return chq.When(exclude, chq.RawCond(col+" = 0"))
}

// botSessionHaving excludes at the session level: a row-level WHERE would keep
// the untagged half of a straddling session (false bounce, truncated
// entry/exit), and the session rollup is keyed per event, so it must merge first.
func botSessionHaving(q *chq.Query, exclude bool) {
	if exclude {
		q.Having("max(bot) = 0")
	}
}
