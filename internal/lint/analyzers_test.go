package lint_test

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/packages"

	"github.com/pug-sh/pug/internal/lint"
)

const moduleRoot = "../.."

const fixtureSlogxErr = `package fixtures

import (
	"context"
	"log/slog"
	sl "log/slog"

	"github.com/pug-sh/pug/internal/slogx"
)

func good(ctx context.Context, err error) {
	slog.ErrorContext(ctx, "upsert failed", slogx.Error(err))
	slog.InfoContext(ctx, "ok", slog.String("error_code", "E42"))
}

func bad(ctx context.Context, err error) {
	slog.ErrorContext(ctx, "upsert failed", slog.Any("error", err))
	slog.ErrorContext(ctx, "upsert failed", slog.Any("err", err))
	slog.ErrorContext(ctx, "upsert failed", slog.String("error", err.Error()))
}

func aliasedImport(ctx context.Context, err error) {
	sl.ErrorContext(ctx, "upsert failed", sl.Any("error", err))
}

func exempted(ctx context.Context, err error) {
	slog.ErrorContext(ctx, "upsert failed", slog.Any("error", err)) // puglint:exempt
}
`

const fixtureSentinelErr = `package fixtures

import (
	"errors"
	"fmt"

	"connectrpc.com/connect"
	conn "connectrpc.com/connect"

	coreprofiles "github.com/pug-sh/pug/internal/core/profiles"
)

var ErrNotFound = errors.New("campaign row missing from postgres")

func bad() error {
	return connect.NewError(connect.CodeNotFound, ErrNotFound)
}

func badCrossPackage() error {
	return connect.NewError(connect.CodeNotFound, coreprofiles.ErrProfileNotFound)
}

func badAliasedImport() error {
	return conn.NewError(conn.CodeNotFound, ErrNotFound)
}

func good() error {
	return connect.NewError(connect.CodeNotFound, errors.New("campaign not found"))
}

func goodFormatted(id string) error {
	return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("invalid campaign id %q", id))
}

func goodLocalErr(err error) error {
	return connect.NewError(connect.CodeInternal, err)
}

func badWrapped() error {
	return connect.NewError(connect.CodeInternal, fmt.Errorf("saving campaign: %w", ErrNotFound))
}

func exempted() error {
	return connect.NewError(connect.CodeNotFound, ErrNotFound) // puglint:exempt
}

func exemptedMultiline() error {
	return connect.NewError(
		connect.CodeNotFound,
		ErrNotFound) // puglint:exempt
}
`

const fixtureRecordErr = `package fixtures

import (
	"context"
	"log/slog"

	"github.com/pug-sh/pug/internal/deps/telemetry"
	"github.com/pug-sh/pug/internal/slogx"
)

func paired(ctx context.Context, err error) {
	telemetry.RecordError(ctx, err)
	slog.ErrorContext(ctx, "query failed", slogx.Error(err))
}

func unpaired(ctx context.Context, err error) {
	slog.ErrorContext(ctx, "query failed", slogx.Error(err))
}

func unpairedViaLogger(ctx context.Context, l *slog.Logger, err error) {
	l.ErrorContext(ctx, "query failed", slogx.Error(err))
}

func exempted(ctx context.Context, err error) {
	slog.ErrorContext(ctx, "bad client input", slogx.Error(err)) // puglint:exempt
}

func warnIsNotAnError(ctx context.Context) {
	slog.WarnContext(ctx, "cache miss, falling back")
}

func exemptedMultiline(ctx context.Context, err error) {
	slog.ErrorContext(ctx, "unhandled error",
		slog.String("procedure", "p"),
		slogx.Error(err)) // puglint:exempt
}

func recordedInOneBranchOnly(ctx context.Context, err, err2 error) {
	if err != nil {
		telemetry.RecordError(ctx, err)
		slog.ErrorContext(ctx, "first failed", slogx.Error(err))
	}
	if err2 != nil {
		slog.ErrorContext(ctx, "second failed", slogx.Error(err2))
	}
}

func recordedAboveTheBranch(ctx context.Context, err error) {
	telemetry.RecordError(ctx, err)
	if err != nil {
		slog.ErrorContext(ctx, "failed", slogx.Error(err))
	}
}
`

// The fixture is overlaid into the real rpc package, so it calls the real
// getPrincipalFromContext rather than a stub that could drift from it.
const fixturePrincipal = `package rpc

import "context"

func lintFixtureHandler(ctx context.Context) error {
	_, err := getPrincipalFromContext(ctx)
	return err
}

func lintFixtureExempt(ctx context.Context) error {
	_, err := getPrincipalFromContext(ctx) // puglint:exempt
	return err
}
`

const fixtureChqTenant = `package fixtures

import chq "github.com/pug-sh/pug/internal/core/clickhouse"

func leaky() *chq.Query {
	return chq.NewQuery().Select("count()").From("events")
}

func scopedInline(projectID string) *chq.Query {
	return chq.NewQuery().From("events").Where(chq.Eq("project_id", projectID))
}

func scopedViaHelper(projectID string) *chq.Query {
	return chq.NewQuery().From("events").Where(baseConds(projectID)...)
}

func baseConds(projectID string) []chq.Condition {
	return []chq.Condition{chq.Eq("project_id", projectID)}
}

func cteAliasIsNotATable() *chq.Query {
	return chq.NewQuery().Select("x").From("per_user")
}

// Naming the column is not filtering on it.
func selectsButNeverFilters() *chq.Query {
	return chq.NewQuery().Select("project_id").From("events").GroupBy("project_id")
}

func joinFragment() string {
	return "events e LEFT ANY JOIN identity_union i ON i.distinct_id = e.distinct_id"
}

func computedFromIsNotAPass() *chq.Query {
	return chq.NewQuery().From(joinFragment())
}

func aliasQualified(projectID string) *chq.Query {
	return chq.NewQuery().From(joinFragment()).Where(chq.Eq("e.project_id", projectID))
}

func exempted() *chq.Query {
	return chq.NewQuery().From("events") // puglint:exempt
}

// One filtered query does not vouch for the unfiltered one beside it.
func siblingQueryIsNotVouchedFor(projectID string) (*chq.Query, *chq.Query) {
	scoped := chq.NewQuery().From("events").Where(chq.Eq("project_id", projectID))
	return scoped, chq.NewQuery().From("profiles")
}

// The tenant table is reached through the join, not the leading relation.
func joinedTenantTable() *chq.Query {
	return chq.NewQuery().From("per_user p JOIN events e ON e.user_key = p.user_key")
}

func scopedInALaterStatement(projectID string) *chq.Query {
	q := chq.NewQuery().From("events")
	q.Where(chq.Eq("project_id", projectID))
	return q
}

func scopedThroughItsCTE(projectID string) *chq.Query {
	agg := chq.NewQuery().From("events").Where(chq.Eq("project_id", projectID))
	return chq.NewQuery().With("agg", agg).From(joinFragment())
}

// A CTE is read only through the query that mounts it, even from a slice.
func mountedOnAScopedQuery(projectID string) *chq.Query {
	subs := make([]*chq.Query, 1)
	subs[0] = chq.NewQuery().From(joinFragment())
	return chq.NewQuery().From("events").Where(chq.Eq("project_id", projectID)).With("sub", subs[0])
}

// Mounting excuses a fragment that cannot be read, never a named table.
func mountingDoesNotExcuseALiteralTable(projectID string) *chq.Query {
	sub := chq.NewQuery().From("profiles")
	return chq.NewQuery().From("events").Where(chq.Eq("project_id", projectID)).With("sub", sub)
}
`

func TestChqTenant(t *testing.T) {
	got := analyzeFixture(t, lint.ChqTenant, "internal/lint/fixtures", fixtureChqTenant)
	want := []string{
		`27: selectsButNeverFilters selects from tenant-scoped table "events" without a project_id filter`,
		"35: computedFromIsNotAPass selects from tenant-scoped table a computed FROM fragment without a project_id filter",
		`49: siblingQueryIsNotVouchedFor selects from tenant-scoped table "profiles" without a project_id filter`,
		`54: joinedTenantTable selects from tenant-scoped table "events" without a project_id filter`,
		`6: leaky selects from tenant-scoped table "events" without a project_id filter`,
		`77: mountingDoesNotExcuseALiteralTable selects from tenant-scoped table "profiles" without a project_id filter`,
	}
	assertDiagnostics(t, got, want)
}

func TestSlogxErr(t *testing.T) {
	got := analyzeFixture(t, lint.SlogxErr, "internal/lint/fixtures", fixtureSlogxErr)
	want := []string{
		`17: use slogx.Error(err) instead of slog.Any("error", ...)`,
		`18: use slogx.Error(err) instead of slog.Any("err", ...)`,
		`19: use slogx.Error(err) instead of slog.String("error", ...)`,
		`23: use slogx.Error(err) instead of slog.Any("error", ...)`,
	}
	assertDiagnostics(t, got, want)
}

// slogx.Error is the one legitimate slog.Any("error", ...) call site.
func TestSlogxErrSkipsSlogxItself(t *testing.T) {
	got := analyze(t, lint.SlogxErr, nil, "github.com/pug-sh/pug/internal/slogx")
	assertDiagnostics(t, got, nil)
}

func TestSentinelErr(t *testing.T) {
	got := analyzeFixture(t, lint.SentinelErr, "internal/lint/fixtures", fixtureSentinelErr)
	want := []string{
		"16: sentinel ErrNotFound reaches connect.NewError; pass errors.New with an explicit client-facing message",
		"20: sentinel ErrProfileNotFound reaches connect.NewError; pass errors.New with an explicit client-facing message",
		"24: sentinel ErrNotFound reaches connect.NewError; pass errors.New with an explicit client-facing message",
		"40: sentinel ErrNotFound reaches connect.NewError; pass errors.New with an explicit client-facing message",
	}
	assertDiagnostics(t, got, want)
}

func TestPrincipal(t *testing.T) {
	got := analyzeFixture(t, lint.Principal, "internal/app/server/rpc", fixturePrincipal)
	want := []string{
		"6: lintFixtureHandler calls getPrincipalFromContext directly; use MustGetPrincipalWithCustomer or MustGetPrincipalWithProject",
	}
	assertDiagnostics(t, got, want)
}

func TestRecordErr(t *testing.T) {
	got := analyzeFixture(t, lint.RecordErr, "internal/lint/fixtures", fixtureRecordErr)
	want := []string{
		"17: unpaired logs an error without telemetry.RecordError; record it here or mark the line puglint:exempt",
		"21: unpairedViaLogger logs an error without telemetry.RecordError; record it here or mark the line puglint:exempt",
		// The sibling branch's RecordError does not cover this path; the one
		// above recordedAboveTheBranch's if does cover its log.
		"44: recordedInOneBranchOnly logs an error without telemetry.RecordError; record it here or mark the line puglint:exempt",
	}
	assertDiagnostics(t, got, want)
}

// analyzerDebt is the violations that predate a rule, per file. Keyed by file
// rather than totalled, so fixing one site while adding another elsewhere does
// not net out to a pass. It is a ratchet: fix sites and lower the number.
var analyzerDebt = map[string]map[string]int{
	"recorderr": recordErrDebt,
}

// Driven by lint.Analyzers() rather than a hand-written list, so an analyzer
// added to the registry is enforced without touching this test.
func TestAnalyzersAcrossRepo(t *testing.T) {
	found := analyzeAll(t, lint.Analyzers(), nil, "./...")
	for _, a := range lint.Analyzers() {
		t.Run(a.Name, func(t *testing.T) {
			want := analyzerDebt[a.Name]
			got := map[string]int{}
			for _, d := range found[a.Name] {
				got[d[:strings.Index(d, ":")]]++
			}
			for file, n := range got {
				if n > want[file] {
					t.Errorf("%s: %d violation(s), baseline %d:\n%s",
						file, n, want[file], strings.Join(violationsIn(found[a.Name], file), "\n"))
				}
			}
			for file, n := range want {
				if got[file] < n {
					t.Errorf("%s: improved to %d; lower analyzerDebt[%q][%q] to match", file, got[file], a.Name, file)
				}
			}
		})
	}
}

func violationsIn(all []string, file string) []string {
	var out []string
	for _, d := range all {
		if strings.HasPrefix(d, file+":") {
			out = append(out, d)
		}
	}
	return out
}

func analyzeFixture(t *testing.T, a *analysis.Analyzer, dir, src string) []string {
	t.Helper()
	abs, err := filepath.Abs(filepath.Join(moduleRoot, dir, "zz_lint_fixture.go"))
	if err != nil {
		t.Fatal(err)
	}
	pattern := "github.com/pug-sh/pug/" + dir
	all := analyze(t, a, map[string][]byte{abs: []byte(src)}, pattern)
	var out []string
	for _, d := range all {
		if after, ok := strings.CutPrefix(d, filepath.ToSlash(filepath.Join(dir, "zz_lint_fixture.go"))+":"); ok {
			out = append(out, after)
		}
	}
	return out
}

func analyze(t *testing.T, a *analysis.Analyzer, overlay map[string][]byte, patterns ...string) []string {
	t.Helper()
	return analyzeAll(t, []*analysis.Analyzer{a}, overlay, patterns...)[a.Name]
}

// analyzeAll loads the package set once and runs every analyzer over it; a
// second packages.Load of ./... costs more than all the analyzers together.
func analyzeAll(t *testing.T, analyzers []*analysis.Analyzer, overlay map[string][]byte, patterns ...string) map[string][]string {
	t.Helper()
	cfg := &packages.Config{
		Dir:     moduleRoot,
		Overlay: overlay,
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedCompiledGoFiles |
			packages.NeedSyntax | packages.NeedTypes | packages.NeedTypesInfo |
			packages.NeedImports | packages.NeedDeps,
	}
	pkgs, err := packages.Load(cfg, patterns...)
	if err != nil {
		t.Fatalf("load %v: %v", patterns, err)
	}
	out := make(map[string][]string, len(analyzers))
	visited := 0
	packages.Visit(pkgs, nil, func(pkg *packages.Package) {
		if !strings.HasPrefix(pkg.PkgPath, "github.com/pug-sh/pug/") {
			return
		}
		// A package that fails to load is analyzed as if it were clean, which
		// for a zero-baseline analyzer is indistinguishable from a pass.
		for _, e := range pkg.Errors {
			t.Fatalf("package %s failed to load, so the analyzers did not see it: %v", pkg.PkgPath, e)
		}
		if len(pkg.Syntax) == 0 {
			return
		}
		visited++
		for _, a := range analyzers {
			pass := &analysis.Pass{
				Analyzer:  a,
				Fset:      pkg.Fset,
				Files:     pkg.Syntax,
				Pkg:       pkg.Types,
				TypesInfo: pkg.TypesInfo,
				ResultOf:  map[*analysis.Analyzer]any{},
				Report: func(d analysis.Diagnostic) {
					pos := pkg.Fset.Position(d.Pos)
					out[a.Name] = append(out[a.Name], fmt.Sprintf("%s:%d: %s", repoPath(pos.Filename), pos.Line, d.Message))
				},
			}
			if _, err := a.Run(pass); err != nil {
				t.Fatalf("%s on %s: %v", a.Name, pkg.PkgPath, err)
			}
		}
	})
	for name := range out {
		slices.Sort(out[name])
	}
	if slices.Contains(patterns, "./...") && visited < minPugPackages {
		t.Fatalf("only %d pug packages analyzed (expected at least %d); the analyzers went quiet, they did not pass",
			visited, minPugPackages)
	}
	return out
}

// minPugPackages is a floor, not a count: it only has to be high enough that a
// package set that failed to load cannot masquerade as a clean tree.
const minPugPackages = 100

func repoPath(filename string) string {
	root, err := filepath.Abs(moduleRoot)
	if err != nil {
		return filename
	}
	rel, err := filepath.Rel(root, filename)
	if err != nil {
		return filename
	}
	return filepath.ToSlash(rel)
}

func assertDiagnostics(t *testing.T, got, want []string) {
	t.Helper()
	if slices.Equal(got, want) {
		return
	}
	t.Errorf("diagnostics mismatch\n got: %s\nwant: %s",
		strings.Join(got, "\n      "), strings.Join(want, "\n      "))
}
