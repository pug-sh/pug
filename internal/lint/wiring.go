package lint

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
)

var entrypoint = regexp.MustCompile(`(?m)^func (Run|StartWorker)\(`)

// entrypointPkgs are the packages under dir that can be started, keyed by their
// module import path.
func entrypointPkgs(root, dir string) (map[string]string, error) {
	out := map[string]string{}
	err := walkGo(filepath.Join(root, dir), func(path string, body []byte) {
		if entrypoint.Match(body) {
			pkg := filepath.Dir(path)
			out["github.com/pug-sh/pug/"+filepath.ToSlash(rel(root, pkg))] = rel(root, pkg)
		}
	})
	return out, err
}

// importsUnder unions the imports of every command directory under cmdDir, so
// which binary (or which file of one) does the importing does not matter.
func importsUnder(root, cmdDir string) (map[string]bool, error) {
	out := map[string]bool{}
	err := filepath.WalkDir(filepath.Join(root, cmdDir), func(path string, d os.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return err
		}
		imports, err := importsOfDir(path)
		if err != nil {
			return err
		}
		for p := range imports {
			out[p] = true
		}
		return nil
	})
	return out, err
}

func checkReachable(root, dir, cmdDir, what string) ([]string, error) {
	pkgs, err := entrypointPkgs(root, dir)
	if err != nil {
		return nil, err
	}
	imported, err := importsUnder(root, cmdDir)
	if err != nil {
		return nil, err
	}
	var out []string
	for importPath, relPath := range pkgs {
		if !imported[importPath] {
			out = append(out, fmt.Sprintf("%s: declares an entrypoint but no %s imports it", relPath, what))
		}
	}
	sort.Strings(out)
	return out, nil
}

func checkWorkerReachable(root string) ([]string, error) {
	return checkReachable(root, "internal/app/workers", "cmd/pug", "cmd/pug command")
}

// A worker wired into the CLI but with no binary under cmd/workers runs under
// `pug dev` and never in the deployment, which has no image to schedule.
func checkWorkerShipped(root string) ([]string, error) {
	return checkReachable(root, "internal/app/workers", "cmd/workers", "cmd/workers binary")
}

func checkCronReachable(root string) ([]string, error) {
	return checkReachable(root, "internal/app/cron", "cmd/cron", "cmd/cron binary")
}
