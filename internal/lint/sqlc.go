package lint

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// `update` is matched anywhere, not just at line start, so a data-modifying CTE
// — the way a mutation actually sneaks into the read set — cannot hide behind
// `with ... as (...) update`. Row locks are masked out first because
// `select ... for update` is not a mutation.
var (
	mutatingStmt = regexp.MustCompile(`(?i)\binsert\s+into\b|\bdelete\s+from\b|\bmerge\s+into\b|\btruncate\s|\bupdate\s`)
	rowLock      = regexp.MustCompile(`(?i)\bfor\s+(?:no\s+key\s+)?update\b`)
	commentOrStr = regexp.MustCompile(`(?s)--[^\n]*|/\*.*?\*/|'(?:[^']|'')*'`)
)

// scrubSQL masks comments, string literals and row locks for the SQL checks. One
// pass, so an apostrophe in a comment cannot open a literal that hides real SQL.
func scrubSQL(sql []byte) []byte {
	return rowLock.ReplaceAll(commentOrStr.ReplaceAll(sql, []byte(" ")), []byte("for share"))
}

// sqlFiles walks dir for .sql files. A missing directory is an error, not an
// empty result: a glob that matches nothing looks exactly like a clean tree.
func sqlFiles(root, dir string) ([]string, error) {
	base := filepath.Join(root, dir)
	if _, err := os.Stat(base); err != nil {
		return nil, fmt.Errorf("query directory %s: %w", dir, err)
	}
	var out []string
	err := filepath.WalkDir(base, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(path, ".sql") {
			out = append(out, path)
		}
		return nil
	})
	return out, err
}

func checkSqlcReadOnly(root string) ([]string, error) {
	files, err := sqlFiles(root, "schema/postgres/queries/read")
	if err != nil {
		return nil, err
	}
	var out []string
	for _, f := range files {
		body, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		for _, m := range mutatingStmt.FindAll(scrubSQL(body), -1) {
			out = append(out, fmt.Sprintf("%s: %s statement in the read query set",
				rel(root, f), strings.ToUpper(strings.Join(strings.Fields(string(m)), " "))))
		}
	}
	return out, nil
}

// A lock needs a write transaction, so a query that takes one stays in the write
// set without mutating. Matched after scrubSQL, which rewrites `for update` to
// `for share`.
var lockingStmt = regexp.MustCompile(`(?i)\bfor\s+(?:key\s+)?share\b|\bpg_(?:try_)?advisory_(?:xact_)?(?:un)?lock(?:_shared|_all)?\s*\(`)

func checkSqlcWriteMutatesOrLocks(root string) ([]string, error) {
	files, err := sqlFiles(root, "schema/postgres/queries/write")
	if err != nil {
		return nil, err
	}
	var out []string
	for _, f := range files {
		body, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		for _, q := range splitQueries(body) {
			if sql := scrubSQL(q.sql); !mutatingStmt.Match(sql) && !lockingStmt.Match(sql) {
				out = append(out, fmt.Sprintf("%s: query %s neither mutates nor locks; move it to the read query set",
					rel(root, f), q.name))
			}
		}
	}
	return out, nil
}

var (
	queryName    = regexp.MustCompile(`(?m)^--\s*name:\s*(\S+)`)
	pascalWithID = regexp.MustCompile(`^[A-Z][A-Za-z0-9]*$`)
	lowercaseID  = regexp.MustCompile(`Ids?([A-Z0-9]|$)`)
)

func checkSqlcNaming(root string) ([]string, error) {
	files, err := sqlFiles(root, "schema/postgres/queries")
	if err != nil {
		return nil, err
	}
	var out []string
	for _, f := range files {
		body, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		for _, m := range queryName.FindAllStringSubmatch(string(body), -1) {
			switch name := m[1]; {
			case !pascalWithID.MatchString(name):
				out = append(out, fmt.Sprintf("%s: query %q is not PascalCase", rel(root, f), name))
			case lowercaseID.MatchString(name):
				out = append(out, fmt.Sprintf("%s: query %q must spell ID in uppercase", rel(root, f), name))
			}
		}
	}
	return out, nil
}
