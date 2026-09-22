package lint

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
)

// tableOwners names the one package that may write each table. A package doc that
// calls itself a table's only writer is otherwise a promise nobody checks: sqlc
// exports every write query to anything that imports dbwrite.
var tableOwners = map[string]string{
	"billing_entitlements":        "internal/core/billing/entitlement",
	"billing_entitlement_history": "internal/core/billing/entitlement",
	"billing_subscriptions":       "internal/core/billing/mandate",
	"billing_webhook_deliveries":  "internal/core/billing/mandate",
	"billing_checkout_sessions":   "internal/core/billing/mandate",
}

// mutatedTable captures the table a mutating statement targets, schema prefix
// included. An upsert's `do update set` captures "set", which no owner names.
var mutatedTable = regexp.MustCompile(`(?i)\b(?:insert\s+into|delete\s+from|merge\s+into|truncate(?:\s+table)?|update)\s+([a-z_][a-z0-9_.]*)`)

// checkTableOwners reads which owned table each write query mutates from the SQL
// itself, so a new writer is covered the day it is written, and fails a call to
// one from outside the owning package. Tests are exempt: seeding a row is not a
// second writer.
func checkTableOwners(root string) ([]string, error) {
	files, err := sqlFiles(root, "schema/postgres/queries/write")
	if err != nil {
		return nil, err
	}
	writes := map[string][]string{}
	written := map[string]bool{}
	for _, f := range files {
		body, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		for _, q := range splitQueries(body) {
			scrubbed := rowLock.ReplaceAll(sqlComment.ReplaceAll(q.sql, nil), []byte("for share"))
			for _, m := range mutatedTable.FindAllSubmatch(scrubbed, -1) {
				table := strings.ToLower(string(m[1]))
				if i := strings.LastIndexByte(table, '.'); i >= 0 {
					table = table[i+1:]
				}
				if _, owned := tableOwners[table]; owned && !slices.Contains(writes[q.name], table) {
					writes[q.name] = append(writes[q.name], table)
					written[table] = true
				}
			}
		}
	}

	var out []string
	// An owned table no query writes is a rename the map missed, which would
	// otherwise pass with the table unguarded.
	for table := range tableOwners {
		if !written[table] {
			out = append(out, fmt.Sprintf(
				"internal/lint/writers.go: %s has an owner but no write query mutates it; the owner map is stale", table))
		}
	}
	err = walkGo(root, func(path string, body []byte) {
		file := filepath.ToSlash(rel(root, path))
		dir := filepath.ToSlash(filepath.Dir(rel(root, path)))
		for name, tables := range writes {
			if !bytes.Contains(body, []byte("."+name+"(")) {
				continue
			}
			for _, table := range tables {
				if owner := tableOwners[table]; dir != owner {
					out = append(out, fmt.Sprintf("%s: calls %s, which writes %s; only %s may", file, name, table, owner))
				}
			}
		}
	})
	sort.Strings(out)
	return out, err
}

type sqlQuery struct {
	name string
	sql  []byte
}

// splitQueries cuts a query file at its `-- name:` headers, so a mutation is
// charged to the query it belongs to rather than to the file.
func splitQueries(body []byte) []sqlQuery {
	idx := queryName.FindAllSubmatchIndex(body, -1)
	out := make([]sqlQuery, 0, len(idx))
	for i, m := range idx {
		end := len(body)
		if i+1 < len(idx) {
			end = idx[i+1][0]
		}
		out = append(out, sqlQuery{name: string(body[m[2]:m[3]]), sql: body[m[1]:end]})
	}
	return out
}
