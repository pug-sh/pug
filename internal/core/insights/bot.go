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

func botExclusionCond(exclude bool, alias string) chq.Condition {
	return chq.BotFilter(!exclude, alias)
}

func botSessionHaving(q *chq.Query, exclude bool) {
	chq.SessionBotHaving(q, !exclude)
}
