// Package lint holds the pug conventions from CLAUDE.md that no off-the-shelf
// linter covers. Anything an upstream linter already enforces — context-aware
// slog variants, error wrapping, span hygiene, layering — belongs in
// .golangci.yml instead. Both halves are enforced by this package's tests, not
// by a separate binary.
package lint

import "golang.org/x/tools/go/analysis"

const (
	exemptMarker = "puglint:exempt"

	connectPkg   = "connectrpc.com/connect"
	rpcPkg       = "github.com/pug-sh/pug/internal/app/server/rpc"
	slogPkg      = "log/slog"
	slogxPkg     = "github.com/pug-sh/pug/internal/slogx"
	telemetryPkg = "github.com/pug-sh/pug/internal/deps/telemetry"
)

// A Check is a convention over an artifact the Go AST cannot see: SQL files,
// migrations, the NATS schema, .proto files, the CLI wiring.
type Check struct {
	Name string
	Doc  string
	Run  func(root string) ([]string, error)
}

// Analyzers is the registry the enforcement test iterates, so a new analyzer is
// enforced the moment it is added here.
func Analyzers() []*analysis.Analyzer {
	return []*analysis.Analyzer{SlogxErr, SentinelErr, Principal, RecordErr, ChqTenant, ExhaustiveIgnore}
}

func Checks() []Check {
	return []Check{
		{"sqlc-read-is-read-only", "queries under queries/read must not mutate; they generate into dbread, whose handle is the read-only pool unless a caller binds it to a write tx", checkSqlcReadOnly},
		{"sqlc-query-naming", "sqlc query names are PascalCase with an uppercase ID", checkSqlcNaming},
		{"migration-numbering", "migration numbers are unique and contiguous; git will not flag a duplicate", checkMigrationNumbering},
		{"nats-consumer-declared", "every consumer a worker looks up must exist in schema/nats/consumers.yaml", checkNATSConsumers},
		{"worker-reachable", "every worker package must be wired into the pug CLI", checkWorkerReachable},
		{"worker-shipped", "every worker package must have a cmd/workers binary to deploy it", checkWorkerShipped},
		{"cron-reachable", "every cron package must have a cmd/cron binary to ship it", checkCronReachable},
		{"depguard-targets-exist", "every depguard glob and denied package names a real path; depguard reports nothing when a pattern matches no file", checkDepguardTargets},
		{"property-pattern-pinned", "property fields keep the pattern that makes PropertyExpr injection-safe", checkPropertyPattern},
		{"exhaustive-ignore-not-in-tests", "//exhaustive:ignore in a _test.go silences exhaustive where the analyzer cannot see it", checkExhaustiveIgnoreInTests},
	}
}
