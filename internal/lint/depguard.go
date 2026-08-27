package lint

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

const modulePath = "github.com/pug-sh/pug/"

// checkDepguardTargets pins every depguard glob and denied package to a path
// that exists. depguard reports nothing when a pattern matches no file, so a
// typo or a moved package silently retires the rule rather than failing.
func checkDepguardTargets(root string) ([]string, error) {
	var doc struct {
		Linters struct {
			Settings struct {
				Depguard struct {
					Rules map[string]struct {
						Files []string `yaml:"files"`
						Deny  []struct {
							Pkg string `yaml:"pkg"`
						} `yaml:"deny"`
					} `yaml:"rules"`
				} `yaml:"depguard"`
			} `yaml:"settings"`
		} `yaml:"linters"`
	}
	body, err := os.ReadFile(filepath.Join(root, ".golangci.yml"))
	if err != nil {
		return nil, err
	}
	if err := yaml.Unmarshal(body, &doc); err != nil {
		return nil, err
	}

	var out []string
	for name, rule := range doc.Linters.Settings.Depguard.Rules {
		for _, pattern := range rule.Files {
			dir, ok := globDir(pattern)
			if !ok {
				continue
			}
			if _, err := os.Stat(filepath.Join(root, dir)); err != nil {
				out = append(out, fmt.Sprintf(".golangci.yml: depguard rule %q matches no such path %q; the rule enforces nothing", name, dir))
			}
		}
		for _, d := range rule.Deny {
			dir, ok := strings.CutPrefix(d.Pkg, modulePath)
			if !ok {
				continue
			}
			if _, err := os.Stat(filepath.Join(root, strings.TrimSuffix(dir, "/"))); err != nil {
				out = append(out, fmt.Sprintf(".golangci.yml: depguard rule %q denies no such package %q", name, d.Pkg))
			}
		}
	}
	sort.Strings(out)
	return out, nil
}

// globDir reduces "**/internal/core/**/*.go" to "internal/core", the literal
// prefix the glob can never look outside of. Taking the prefix rather than
// trimming the tail keeps a filename component ("*.go") from being stat'd as if
// it were a directory. Negations and the $test placeholder name no directory,
// so they are skipped.
func globDir(pattern string) (string, bool) {
	if strings.HasPrefix(pattern, "!") || strings.Contains(pattern, "$") {
		return "", false
	}
	p := strings.TrimPrefix(pattern, "**/")
	if i := strings.IndexAny(p, "*?["); i >= 0 {
		p = p[:i]
	}
	return strings.Trim(p, "/"), true
}
