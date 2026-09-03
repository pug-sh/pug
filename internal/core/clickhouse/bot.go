package clickhouse

const botColumn = "bot"

// BotFilter is the row-level `bot = 0`, or no predicate when bots are included. bot is a key
// column on both rollups, so the same predicate serves their fast path; a session-scoped read
// wants SessionBotHaving instead (docs/architecture/bot-detection.md).
func BotFilter(includeBots bool, alias string) Condition {
	return When(!includeBots, RawCond(tablePrefix(alias)+botColumn+" = 0"))
}

// SessionBotHaving excludes at the session level: a row-level WHERE would keep the
// untagged half of a straddling session (false bounce, truncated entry/exit), and the
// session rollup is keyed per event, so it must merge first.
func SessionBotHaving(q *Query, includeBots bool) {
	if !includeBots {
		q.Having("max(" + botColumn + ") = 0")
	}
}
