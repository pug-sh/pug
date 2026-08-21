package lint_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/pug-sh/pug/internal/lint"
)

// knownFindings are the violations that exist today, listed verbatim so a new
// one fails the build and a fixed one fails too, prompting its removal here.
// They come from one cause: devices, campaigns and scheduler are complete
// worker packages that no command wires up and no image ships, and two of them
// look up consumers that schema/nats/consumers.yaml never declared.
var knownFindings = map[string][]string{
	"nats-consumer-declared": {
		`internal/app/workers/campaigns/worker.go: consumer "campaign-processor-durable" is not declared in schema/nats/consumers.yaml`,
		`internal/app/workers/devices/worker.go: consumer "device-processor-durable" is not declared in schema/nats/consumers.yaml`,
	},
	"worker-reachable": {
		"internal/app/workers/campaigns: declares an entrypoint but no cmd/pug command imports it",
		"internal/app/workers/devices: declares an entrypoint but no cmd/pug command imports it",
		"internal/app/workers/scheduler: declares an entrypoint but no cmd/pug command imports it",
	},
	"worker-shipped": {
		"internal/app/workers/campaigns: declares an entrypoint but no cmd/workers binary imports it",
		"internal/app/workers/devices: declares an entrypoint but no cmd/workers binary imports it",
		"internal/app/workers/scheduler: declares an entrypoint but no cmd/workers binary imports it",
	},
}

func TestChecksAcrossRepo(t *testing.T) {
	for _, c := range lint.Checks() {
		t.Run(c.Name, func(t *testing.T) {
			got, err := c.Run(moduleRoot)
			if err != nil {
				t.Fatalf("%s: %v", c.Name, err)
			}
			slices.Sort(got)
			want := knownFindings[c.Name]
			if !slices.Equal(got, want) {
				t.Errorf("%s\n got: %s\nwant: %s", c.Doc,
					strings.Join(got, "\n      "), strings.Join(want, "\n      "))
			}
		})
	}
}

func TestChecksDetectViolations(t *testing.T) {
	tests := []struct {
		check string
		files map[string]string
		want  string
	}{
		{
			check: "sqlc-read-is-read-only",
			files: map[string]string{
				"schema/postgres/queries/read/a.sql": "-- name: GetThing :one\nselect * from things;\n\n-- name: BumpThing :exec\nupdate things set n = n + 1;\n",
			},
			want: "UPDATE statement in the read query set",
		},
		{
			check: "sqlc-read-is-read-only",
			files: map[string]string{
				// Grouping the query set into subdirectories must not hide it.
				"schema/postgres/queries/read/campaigns/a.sql": "-- name: BumpThing :exec\nupdate things set n = n + 1;\n",
			},
			want: "UPDATE statement in the read query set",
		},
		{
			check: "sqlc-read-is-read-only",
			files: map[string]string{
				"schema/postgres/queries/read/a.sql": "-- name: GetAndPurge :many\nwith d as (delete from things returning *) select * from d;\n",
			},
			want: "DELETE FROM statement in the read query set",
		},
		{
			check: "sqlc-read-is-read-only",
			files: map[string]string{
				// `for update` is a row lock, not a mutation.
				"schema/postgres/queries/read/a.sql": "-- name: LockThing :one\nselect 1\nfor update;\n",
			},
			want: "",
		},
		{
			check: "sqlc-query-naming",
			files: map[string]string{
				"schema/postgres/queries/read/a.sql": "-- name: GetThingById :one\nselect 1;\n",
			},
			want: `must spell ID in uppercase`,
		},
		{
			check: "sqlc-query-naming",
			files: map[string]string{
				"schema/postgres/queries/read/a.sql": "-- name: GetProjectIds :many\nselect 1;\n",
			},
			want: `must spell ID in uppercase`,
		},
		{
			check: "sqlc-query-naming",
			files: map[string]string{
				"schema/postgres/queries/read/a.sql": "-- name: get_thing :one\nselect 1;\n",
			},
			want: `is not PascalCase`,
		},
		{
			check: "migration-numbering",
			files: map[string]string{
				"schema/postgres/migrations/001_a.sql":   "",
				"schema/postgres/migrations/002_b.sql":   "",
				"schema/postgres/migrations/002_c.sql":   "",
				"schema/clickhouse/migrations/001_a.sql": "",
			},
			want: "migration 2 is used twice",
		},
		{
			check: "migration-numbering",
			files: map[string]string{
				"schema/postgres/migrations/001_a.sql":   "",
				"schema/postgres/migrations/003_c.sql":   "",
				"schema/clickhouse/migrations/001_a.sql": "",
			},
			want: "numbering has a gap at 2",
		},
		{
			check: "nats-consumer-declared",
			files: map[string]string{
				"schema/nats/consumers.yaml":  "consumers:\n  - name: \"real\"\n    stream_name: \"events\"\n    durable_name: \"real-durable\"\n",
				"schema/nats/streams.yaml":    "streams:\n  - name: \"events\"\n",
				"internal/app/workers/x/w.go": "package x\nfunc Run() { c, _ := n.GetConsumerConfigByName(\"ghost-durable\"); _ = c }\n",
			},
			want: `consumer "ghost-durable" is not declared`,
		},
		{
			check: "nats-consumer-declared",
			files: map[string]string{
				"schema/nats/consumers.yaml": "consumers:\n  - name: \"real\"\n    stream_name: \"nope\"\n    durable_name: \"real-durable\"\n",
				"schema/nats/streams.yaml":   "streams:\n  - name: \"events\"\n",
			},
			want: `binds unknown stream "nope"`,
		},
		{
			check: "worker-reachable",
			files: map[string]string{
				"cmd/pug/main.go":                  "package main\nimport _ \"github.com/pug-sh/pug/internal/app/workers/wired\"\n",
				"internal/app/workers/wired/w.go":  "package wired\nfunc Run() {}\n",
				"internal/app/workers/orphan/w.go": "package orphan\nfunc StartWorker() {}\n",
			},
			want: "internal/app/workers/orphan: declares an entrypoint",
		},
		{
			check: "cron-reachable",
			files: map[string]string{
				"internal/app/cron/orphan/c.go": "package orphan\nimport \"context\"\nfunc Run(ctx context.Context) error { return nil }\n",
				"cmd/cron/.keep":                "",
			},
			want: "no cmd/cron binary imports it",
		},
		{
			check: "depguard-targets-exist",
			files: map[string]string{
				".golangci.yml": "linters:\n  settings:\n    depguard:\n      rules:\n        typo:\n          files: [\"**/internal/coer/**\", \"!$test\"]\n          deny:\n            - pkg: connectrpc.com/connect\n",
			},
			want: "matches no such path",
		},
		{
			check: "depguard-targets-exist",
			files: map[string]string{
				".golangci.yml": "linters:\n  settings:\n    depguard:\n      rules:\n        moved:\n          files: [\"!$test\"]\n          deny:\n            - pkg: github.com/pug-sh/pug/internal/gone\n",
			},
			want: "denies no such package",
		},
		{
			check: "property-pattern-pinned",
			files: map[string]string{
				"proto/a.proto": "message M {\n  string property = 1 [(buf.validate.field).string.min_len = 1];\n}\n",
			},
			want: "has no pattern constraint",
		},
		{
			check: "property-pattern-pinned",
			files: map[string]string{
				"proto/a.proto": "message M {\n  repeated string property_keys = 1;\n}\n",
			},
			want: "has no pattern constraint",
		},
		{
			check: "property-pattern-pinned",
			files: map[string]string{
				"proto/a.proto": "message M {\n  optional string property_name = 1;\n}\n",
			},
			want: "has no pattern constraint",
		},
		{
			check: "nats-consumer-declared",
			files: map[string]string{
				"schema/nats/consumers.yaml":  "consumers:\n  - name: \"real\"\n    stream_name: \"events\"\n    durable_name: \"real-durable\"\n",
				"schema/nats/streams.yaml":    "streams:\n  - name: \"events\"\n",
				"internal/app/workers/x/w.go": "package x\nconst durable = \"ghost\"\nfunc Run() { c, _ := n.GetConsumerConfigByName(durable); _ = c }\n",
			},
			want: "is not a literal",
		},
		{
			check: "property-pattern-pinned",
			files: map[string]string{
				"proto/a.proto": "message M {\n  string metric_property = 1 [(buf.validate.field) = {\n    string: {pattern: \"^.*$\"}\n  }];\n}\n",
			},
			want: "uses unreviewed pattern",
		},
	}

	for _, tt := range tests {
		t.Run(tt.check+"/"+tt.want, func(t *testing.T) {
			root := t.TempDir()
			for name, body := range tt.files {
				path := filepath.Join(root, name)
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			got := runCheck(t, tt.check, root)
			if tt.want == "" {
				if len(got) > 0 {
					t.Errorf("want no findings, got: %v", got)
				}
				return
			}
			if !slices.ContainsFunc(got, func(s string) bool { return strings.Contains(s, tt.want) }) {
				t.Errorf("want a finding containing %q, got: %v", tt.want, got)
			}
		})
	}
}

// A query directory that has moved must be an error, not zero findings: a glob
// that matches nothing is indistinguishable from a clean tree.
func TestSqlcChecksFailOnMissingQueryDir(t *testing.T) {
	for _, name := range []string{"sqlc-read-is-read-only", "sqlc-query-naming"} {
		t.Run(name, func(t *testing.T) {
			for _, c := range lint.Checks() {
				if c.Name != name {
					continue
				}
				if _, err := c.Run(t.TempDir()); err == nil {
					t.Errorf("%s: want an error for a missing query directory, got nil", name)
				}
			}
		})
	}
}

func runCheck(t *testing.T, name, root string) []string {
	t.Helper()
	for _, c := range lint.Checks() {
		if c.Name != name {
			continue
		}
		got, err := c.Run(root)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return got
	}
	t.Fatalf("no such check: %s", name)
	return nil
}
