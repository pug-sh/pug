package lint

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// propertyField matches a proto property field whose value is interpolated into
// SQL text — ClickHouse map keys and JSON paths cannot be parameterized — so
// the proto pattern is the only thing between a client and the query.
var (
	propertyField  = regexp.MustCompile(`(?m)^\s*(?:repeated\s+|optional\s+)?(?:string|map<\s*string\s*,\s*string\s*>)\s+(\w*propert\w*)\s*=\s*\d+([^;]*);`)
	patternLiteral = regexp.MustCompile(`pattern\s*[:=]\s*"((?:[^"\\]|\\.)*)"`)
)

// The approved patterns admit only alphanumerics, underscore, dot and dash with
// an optional leading $ — no quote or backslash can reach the SQL string. A new
// pattern must be added here deliberately, which is the review this rule exists
// to force.
var safePropertyPatterns = map[string]bool{
	`^\\$?[a-zA-Z0-9_.-]*$`:                    true,
	`^\\$?[a-zA-Z0-9_.-]+$`:                    true,
	`^\\$?[a-zA-Z0-9_-]+(\\.[a-zA-Z0-9_-]+)*$`: true,
}

func checkPropertyPattern(root string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(filepath.Join(root, "proto"), func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".proto") {
			return err
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, m := range propertyField.FindAllStringSubmatch(string(body), -1) {
			pat := patternLiteral.FindStringSubmatch(m[2])
			switch {
			case pat == nil:
				out = append(out, fmt.Sprintf("%s: field %q has no pattern constraint; it is interpolated into SQL", rel(root, path), m[1]))
			case !safePropertyPatterns[pat[1]]:
				out = append(out, fmt.Sprintf("%s: field %q uses unreviewed pattern %s; confirm it excludes quotes and backslashes, then add it to safePropertyPatterns", rel(root, path), m[1], pat[1]))
			}
		}
		return nil
	})
	sort.Strings(out)
	return out, err
}
