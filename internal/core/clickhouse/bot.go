package clickhouse

const botColumn = "bot"

// BotFilter is the row-level `bot = 0`, or no predicate when bots are included. bot is a key
// column on both rollups, so the same predicate serves their fast path; a session-scoped read
// wants HAVING max(bot) = 0 instead (docs/architecture/bot-detection.md).
func BotFilter(includeBots bool, alias string) Condition {
	return When(!includeBots, RawCond(tablePrefix(alias)+botColumn+" = 0"))
}
