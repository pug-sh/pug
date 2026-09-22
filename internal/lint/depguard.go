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

// checkDepguardTargets pins every depguard glob and every denied or allowed
// package to a path that exists. depguard reports nothing when a pattern matches
// no file, so a typo or a moved package silently retires the rule rather than
// failing.
func checkDepguardTargets(root string) ([]string, error) {
	var doc struct {
		Linters struct {
			Settings struct {
				Depguard struct {
					Rules map[string]struct {
						Files []string `yaml:"files"`
						Allow []string `yaml:"allow"`
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
			if !modulePathExists(root, d.Pkg) {
				out = append(out, fmt.Sprintf(".golangci.yml: depguard rule %q denies no such package %q", name, d.Pkg))
			}
		}
		// A stale allow is not silent in lax mode, whose deny then catches the port's
		// real imports, but it reads as a rule about a package that is gone.
		for _, pkg := range rule.Allow {
			if !modulePathExists(root, pkg) {
				out = append(out, fmt.Sprintf(".golangci.yml: depguard rule %q allows no such package %q", name, pkg))
			}
		}
	}
	sort.Strings(out)
	return out, nil
}

// modulePathExists reports whether a depguard entry inside this module names a
// real directory, reading past the `$` that makes it exact and the trailing slash
// that makes it a subtree. An entry outside the module is not checked.
func modulePathExists(root, pkg string) bool {
	dir, ok := strings.CutPrefix(pkg, modulePath)
	if !ok {
		return true
	}
	dir = strings.TrimSuffix(strings.TrimSuffix(dir, "$"), "/")
	_, err := os.Stat(filepath.Join(root, dir))
	return err == nil
}

// globDir reduces "**/internal/core/**/*.go" to "internal/core", the literal
// prefix the glob can never look outside of. Taking the prefix rather than
// trimming the tail keeps a filename component ("*.go") from being stat'd as if
// it were a directory. A negation still names a directory; only $test names none.
func globDir(pattern string) (string, bool) {
	pattern = strings.TrimPrefix(pattern, "!")
	if strings.Contains(pattern, "$") {
		return "", false
	}
	p := strings.TrimPrefix(pattern, "**/")
	if i := strings.IndexAny(p, "*?["); i >= 0 {
		p = p[:i]
	}
	return strings.Trim(p, "/"), true
}
