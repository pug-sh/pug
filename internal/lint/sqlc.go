package lint

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Anchored on `update` only at line start so `select ... for update` is not a
// mutation, but unanchored on the rest so a data-modifying CTE — the way a
// mutation actually sneaks into the read set — does not hide mid-line.
var (
	mutatingStmt = regexp.MustCompile(`(?im)\binsert\s+into\b|\bdelete\s+from\b|\bmerge\s+into\b|\btruncate\s|^\s*update\s`)
	sqlComment   = regexp.MustCompile(`(?m)--.*$`)
)

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
		for _, m := range mutatingStmt.FindAll(sqlComment.ReplaceAll(body, nil), -1) {
			out = append(out, fmt.Sprintf("%s: %s statement in the read query set",
				rel(root, f), strings.ToUpper(strings.Join(strings.Fields(string(m)), " "))))
		}
	}
	return out, nil
}

var (
	queryName    = regexp.MustCompile(`(?m)^--\s*name:\s*(\S+)`)
	pascalWithID = regexp.MustCompile(`^[A-Z][A-Za-z0-9]*$`)
	lowercaseID  = regexp.MustCompile(`Id([A-Z]|s?$)`)
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
