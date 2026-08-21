package lint_test

// recordErrDebt is the slog.ErrorContext calls that predate the recorderr rule,
// per file. Fix sites and lower the numbers; a file that reaches zero comes out
// of the map. Line numbers are deliberately absent — they churn with every edit
// above a site, and the file is a precise enough anchor to stop a new violation
// hiding behind a fixed one.
var recordErrDebt = map[string]int{
	"cmd/cron/usage/main.go":                             1,
	"cmd/migrate/clickhouse/main.go":                     1,
	"cmd/migrate/nats/main.go":                           1,
	"cmd/migrate/postgres/main.go":                       1,
	"cmd/pug/main.go":                                    5,
	"cmd/server/main.go":                                 1,
	"cmd/workers/compliance/main.go":                     1,
	"cmd/workers/demo/main.go":                           1,
	"cmd/workers/email/main.go":                          1,
	"cmd/workers/events/main.go":                         1,
	"cmd/workers/profile/alias/main.go":                  1,
	"cmd/workers/profile/identify/main.go":               1,
	"cmd/workers/profile/upsert/main.go":                 1,
	"internal/app/server/deps.go":                        5,
	"internal/app/server/rpc/error.go":                   1,
	"internal/app/server/rpc/logging_interceptor.go":     1,
	"internal/app/server/server.go":                      2,
	"internal/app/workers/campaigns/worker.go":           1,
	"internal/app/workers/compliance/compliance.go":      2,
	"internal/app/workers/demo/demo.go":                  1,
	"internal/app/workers/devices/worker.go":             1,
	"internal/app/workers/email/worker.go":               1,
	"internal/app/workers/events/worker.go":              1,
	"internal/app/workers/profiles/alias/alias.go":       1,
	"internal/app/workers/profiles/identify/identify.go": 1,
	"internal/app/workers/profiles/upsert/upsert.go":     1,
	"internal/app/workers/scheduler/scheduler.go":        1,
	"internal/core/profiles/service.go":                  2,
	"internal/deps/clickhouse/clickhouse.go":             5,
	"internal/deps/nats/worker.go":                       2,
	"internal/deps/postgres/postgres.go":                 2,
	"internal/deps/redis/redis.go":                       8,
}
