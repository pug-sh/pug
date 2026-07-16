package testutil_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// setupHelpers are the testutil entry points that start a container shared by a
// whole package. The scan below keys off these names, so a rename that missed
// this list would leave that helper's packages unchecked while the rest kept the
// scan looking healthy — assertScanIsLive is what makes that loud.
var setupHelpers = []string{"SetupPostgres", "SetupClickHouse", "SetupNATS", "SetupRedis"}

// pkgScan is what one walk over the repo's test files yields. Every field is
// keyed by directory, which is also the unit a test binary is built from.
type pkgScan struct {
	// helperDirs maps each Setup* helper to the directories that call it.
	helperDirs map[string][]string
	// setupDirs holds every directory calling any Setup* helper.
	setupDirs map[string]bool
	// declaresMain holds directories declaring a TestMain, wired or not.
	declaresMain map[string]bool
	// wiresMain holds directories whose TestMain reaches testutil.Main.
	wiresMain map[string]bool
	// parallelFiles maps a directory to the test files in it calling Parallel().
	parallelFiles map[string][]string
}

// TestSetupCallersWireMain asserts that every package whose tests call a
// testutil.Setup* helper hands its run to testutil.Main.
//
// The helpers share one container per test binary, so the container's teardown
// hangs off Main rather than any single test's t.Cleanup. A package that skips it
// still passes — it just strands its containers, with no failing test to point at.
// This is that failing test.
func TestSetupCallersWireMain(t *testing.T) {
	s := scanTests(t)
	assertScanIsLive(t, s)

	var missing, unwired []string
	for dir := range s.setupDirs {
		switch {
		case !s.declaresMain[dir]:
			missing = append(missing, relTo(t, dir))
		case !s.wiresMain[dir]:
			unwired = append(unwired, relTo(t, dir))
		}
	}
	slices.Sort(missing)
	slices.Sort(unwired)

	const remedy = "\n\nhand the run to testutil.Main:\n\n\tfunc TestMain(m *testing.M) { testutil.Main(m) }"

	if len(missing) > 0 {
		t.Errorf("these packages call testutil.Setup* without declaring TestMain, so their shared containers outlive the run:\n\t%s%s",
			strings.Join(missing, "\n\t"), remedy)
	}
	// Checking for TestMain by name alone would pass the idiomatic
	// os.Exit(m.Run()) — a TestMain that never tears a container down, and the
	// likeliest way to arrive at one is to add it for something else first.
	if len(unwired) > 0 {
		t.Errorf("these packages declare TestMain but never reach testutil.Main, so their shared containers outlive the run:\n\t%s%s",
			strings.Join(unwired, "\n\t"), remedy)
	}
}

// TestSetupCallersDoNotUseParallel asserts that no package sharing a container
// runs its tests concurrently.
//
// The per-test isolation the helpers provide is not uniform. Postgres and
// ClickHouse hand out a private database and would survive concurrency; NATS and
// Redis have nowhere to put one, so SetupNATS rebuilds every stream in the
// container and SetupRedis recycles 16 logical databases and flushes on entry.
// Either will wipe a concurrently running test's state from under it.
//
// The guard is package-wide rather than per-helper because a package mixes the
// helpers freely, and because the damage is silent: the victim either fails
// somewhere unrelated to the cause, or passes having asserted over state that was
// deleted mid-test.
func TestSetupCallersDoNotUseParallel(t *testing.T) {
	s := scanTests(t)
	assertScanIsLive(t, s)

	var offenders []string
	for dir := range s.setupDirs {
		for _, file := range s.parallelFiles[dir] {
			offenders = append(offenders, relTo(t, file))
		}
	}
	slices.Sort(offenders)
	offenders = slices.Compact(offenders)

	if len(offenders) > 0 {
		t.Errorf("these files call Parallel() in a package that shares a container, which lets one test delete another's state mid-run:\n\t%s\n\nrun them sequentially, or give the package a namespace per test that survives concurrency (see SetupPostgres) before parallelising it",
			strings.Join(offenders, "\n\t"))
	}
}

// scanTests parses every _test.go in the repo once and reports what the guards
// above need to know about each directory.
func scanTests(t *testing.T) pkgScan {
	t.Helper()

	root := repoRoot(t)
	fset := token.NewFileSet()
	s := pkgScan{
		helperDirs:    map[string][]string{},
		setupDirs:     map[string]bool{},
		declaresMain:  map[string]bool{},
		wiresMain:     map[string]bool{},
		parallelFiles: map[string][]string{},
	}

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if skipDir(root, path, d) {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, "_test.go") {
			return nil
		}

		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}

		dir := filepath.Dir(path)
		ast.Inspect(f, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.FuncDecl:
				if node.Recv == nil && node.Name.Name == "TestMain" {
					s.declaresMain[dir] = true
					if callsTestutilMain(node) {
						s.wiresMain[dir] = true
					}
				}
			case *ast.SelectorExpr:
				if id, ok := node.X.(*ast.Ident); ok && id.Name == "testutil" && strings.HasPrefix(node.Sel.Name, "Setup") {
					s.setupDirs[dir] = true
					s.helperDirs[node.Sel.Name] = append(s.helperDirs[node.Sel.Name], dir)
				}
			case *ast.CallExpr:
				if isParallelCall(node) {
					s.parallelFiles[dir] = append(s.parallelFiles[dir], path)
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk repo: %v", err)
	}

	return s
}

// assertScanIsLive guards against the scan quietly matching nothing — a renamed
// helper, a moved root — which would leave these tests passing forever without
// checking anything.
func assertScanIsLive(t *testing.T, s pkgScan) {
	t.Helper()

	// Per helper, not merely in total: three helpers keeping the total non-zero
	// would mask a fourth whose packages had stopped being checked.
	for _, helper := range setupHelpers {
		if len(s.helperDirs[helper]) == 0 {
			t.Fatalf("scan found no caller of testutil.%s; it has drifted and no longer checks that helper's packages (update setupHelpers if it was renamed or retired)", helper)
		}
	}
}

// skipDir keeps the walk out of trees with no hand-written tests to check.
//
// The generated-code case has to compare the path relative to the root:
// fs.DirEntry.Name reports the final element alone, so matching "internal/gen"
// against it would silently never fire.
func skipDir(root, path string, d fs.DirEntry) bool {
	switch d.Name() {
	case ".git", "node_modules", "bin":
		return true
	}
	rel, err := filepath.Rel(root, path)
	return err == nil && rel == filepath.Join("internal", "gen")
}

// callsTestutilMain reports whether fn's body hands the run to testutil.Main.
func callsTestutilMain(fn *ast.FuncDecl) bool {
	var found bool
	ast.Inspect(fn, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if id, ok := sel.X.(*ast.Ident); ok && id.Name == "testutil" && sel.Sel.Name == "Main" {
			found = true
			return false
		}
		return true
	})
	return found
}

// isParallelCall reports whether n is a no-argument Parallel() call. The receiver
// is left unmatched: subtests rename their *testing.T freely, and nothing else in
// a test file offers that signature.
func isParallelCall(n *ast.CallExpr) bool {
	sel, ok := n.Fun.(*ast.SelectorExpr)
	return ok && sel.Sel.Name == "Parallel" && len(n.Args) == 0
}

func relTo(t *testing.T, path string) string {
	t.Helper()
	rel, err := filepath.Rel(repoRoot(t), path)
	if err != nil {
		return path
	}
	return rel
}

func repoRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("unable to determine source file path")
	}
	return filepath.Join(filepath.Dir(thisFile), "..", "..")
}
