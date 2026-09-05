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

func stateChangeIsNotAnError(ctx context.Context, stream string) {
	slog.ErrorContext(ctx, "too many consecutive failures, restarting", slog.String("stream", stream))
}

func errorUnderAnotherKey(ctx context.Context, err error) {
	slog.ErrorContext(ctx, "failed", slog.Any("cause", err))
}

func spreadArgsAreOpaque(ctx context.Context, args []any) {
	slog.ErrorContext(ctx, "rpc error", args...)
}

func init() {
	slog.ErrorContext(context.Background(), "counter registration failed", slogx.Error(context.Canceled))
}

type registry struct{}

func (registry) init(ctx context.Context, err error) {
	slog.ErrorContext(ctx, "registry init failed", slogx.Error(err))
}
`

// Overlaid into a real entrypoint package: the same body flagged in fixtures
// above must go quiet in package main.
const fixtureRecordErrMain = `package main

import (
	"context"
	"log/slog"

	"github.com/pug-sh/pug/internal/slogx"
)

func lintFixtureFatal(ctx context.Context, err error) {
	slog.ErrorContext(ctx, "fatal error", slogx.Error(err))
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

import (
	"fmt"

	chq "github.com/pug-sh/pug/internal/core/clickhouse"
)

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

func metaExpr() string {
	return "SELECT 1 AS event_index"
}

// A fragment leading with a CTE this query mounts reads that CTE's rows.
func scopedThroughItsCTE(projectID string) *chq.Query {
	agg := chq.NewQuery().From("events").Where(chq.Eq("project_id", projectID))
	return chq.NewQuery().With("agg", agg).From(fmt.Sprintf("agg a CROSS JOIN (%s) AS s", metaExpr()))
}

// Mounting a scoped CTE says nothing about a fragment that reads a table.
func mountingAScopedCTEIsNotEnough(projectID string) *chq.Query {
	agg := chq.NewQuery().From("events").Where(chq.Eq("project_id", projectID))
	return chq.NewQuery().With("agg", agg).From(joinFragment())
}

// A CTE is read only through the query that mounts it, even from a slice.
func mountedOnAScopedQuery(projectID string) *chq.Query {
	names := make([]string, 1)
	subs := make([]*chq.Query, 1)
	names[0], subs[0] = "sub_0", chq.NewQuery().From(names[0])
	return chq.NewQuery().From("events").Where(chq.Eq("project_id", projectID)).With(names[0], subs[0])
}

// Mounting excuses a fragment that names the CTE, never a named table.
func mountingDoesNotExcuseALiteralTable(projectID string) *chq.Query {
	sub := chq.NewQuery().From("profiles")
	return chq.NewQuery().From("events").Where(chq.Eq("project_id", projectID)).With("sub", sub)
}
`

func TestChqTenant(t *testing.T) {
	got := analyzeFixture(t, lint.ChqTenant, "internal/lint/fixtures", fixtureChqTenant)
	want := []string{
		`10: leaky selects from tenant-scoped table "events" without a project_id filter`,
		`31: selectsButNeverFilters selects from tenant-scoped table "events" without a project_id filter`,
		"39: computedFromIsNotAPass selects from tenant-scoped table a computed FROM fragment without a project_id filter",
		`53: siblingQueryIsNotVouchedFor selects from tenant-scoped table "profiles" without a project_id filter`,
		`58: joinedTenantTable selects from tenant-scoped table "events" without a project_id filter`,
		"80: mountingAScopedCTEIsNotEnough selects from tenant-scoped table a computed FROM fragment without a project_id filter",
		`93: mountingDoesNotExcuseALiteralTable selects from tenant-scoped table "profiles" without a project_id filter`,
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
		// stateChangeIsNotAnError above these two carries nothing to record; an
		// error under any key, or hidden in a spread, still has to be recorded.
		"60: errorUnderAnotherKey logs an error without telemetry.RecordError; record it here or mark the line puglint:exempt",
		"64: spreadArgsAreOpaque logs an error without telemetry.RecordError; record it here or mark the line puglint:exempt",
		// The package init above is skipped; a method that happens to be named
		// init is not.
		"74: init logs an error without telemetry.RecordError; record it here or mark the line puglint:exempt",
	}
	assertDiagnostics(t, got, want)
}

func TestRecordErrSkipsEntrypoints(t *testing.T) {
	got := analyzeFixture(t, lint.RecordErr, "cmd/pug", fixtureRecordErrMain)
	assertDiagnostics(t, got, nil)
}

const fixtureExhaustiveIgnore = `package fixtures

import (
	"fmt"
	"log"
	"os"
)

type Kind int

const (
	KindA Kind = iota
	KindB
	KindC
)

func rejectsWithError(k Kind) (string, error) {
	//exhaustive:ignore the default rejects every other member
	switch k {
	case KindA:
		return "a", nil
	default:
		return "", fmt.Errorf("unsupported kind: %v", k)
	}
}

func rejectsWithFalse(k Kind) bool {
	//exhaustive:ignore not eligible falls back to the slow path
	switch k {
	case KindA, KindB:
	default:
		return false
	}
	return true
}

func dispatchesFromDefault(k Kind) Kind {
	//exhaustive:ignore
	switch k {
	case KindA:
		return KindA
	default:
		return KindC
	}
}

func emptyDefault(k Kind) Kind {
	//exhaustive:ignore
	switch k {
	case KindA:
		return KindA
	default:
	}
	return KindC
}

func noDefault(k Kind) Kind {
	//exhaustive:ignore
	switch k {
	case KindA:
		return KindA
	}
	return KindC
}

func dangling(k Kind) Kind {
	//exhaustive:ignore
	if k == KindA {
		return KindB
	}
	return KindC
}

func panics(k Kind) Kind {
	//exhaustive:ignore the default cannot fall through
	switch k {
	case KindA:
		return KindA
	default:
		panic("unhandled kind")
	}
}

func rejectsWithNamedZero(k Kind) (Kind, error) {
	//exhaustive:ignore KindA is the zero member, so this is a rejection
	switch k {
	case KindA:
		return KindB, nil
	default:
		return KindA, fmt.Errorf("unsupported kind: %v", k)
	}
}

func rejectsWithFloatZero(k Kind) float64 {
	//exhaustive:ignore
	switch k {
	case KindA:
		return 1.5
	default:
		return 0.0
	}
}

func dispatchesAboveRejection(k Kind) (Kind, error) {
	//exhaustive:ignore
	switch k {
	case KindA:
		return KindA, nil
	default:
		if k == KindB {
			return KindC, nil
		}
		return KindA, fmt.Errorf("unsupported kind: %v", k)
	}
}

func bareReturn(k Kind, out *Kind) {
	//exhaustive:ignore
	switch k {
	case KindA:
		*out = KindA
		return
	default:
		*out = KindC
		return
	}
}

func neitherReturnsNorPanics(k Kind) Kind {
	//exhaustive:ignore
	switch k {
	case KindA:
		return KindA
	default:
		_ = k
	}
	return KindC
}

func fallsThroughPastRejection(k Kind) (Kind, error) {
	//exhaustive:ignore
	switch k {
	case KindA:
		return KindA, nil
	default:
		if k == KindB {
			return KindA, fmt.Errorf("unsupported kind: %v", k)
		}
		_ = k
	}
	return KindC, nil
}

type fakeExit struct{}

func (fakeExit) Exit(int) {}

type fakeLog struct{}

func (fakeLog) Fatalf(string, ...any) {}

func shadowedPanic(k Kind) Kind {
	panic := func(string) {}
	//exhaustive:ignore
	switch k {
	case KindA:
		return KindA
	default:
		panic("unhandled kind")
	}
	return KindC
}

func shadowedExit(k Kind) Kind {
	var os fakeExit
	//exhaustive:ignore
	switch k {
	case KindA:
		return KindA
	default:
		os.Exit(1)
	}
	return KindC
}

func shadowedFatal(k Kind) Kind {
	var log fakeLog
	//exhaustive:ignore
	switch k {
	case KindA:
		return KindA
	default:
		log.Fatalf("unhandled kind: %v", k)
	}
	return KindC
}

func exits(k Kind) Kind {
	//exhaustive:ignore the default cannot fall through
	switch k {
	case KindA:
		return KindA
	default:
		os.Exit(1)
	}
	return KindC
}

func fatals(k Kind) Kind {
	//exhaustive:ignore the default cannot fall through
	switch k {
	case KindA:
		return KindA
	default:
		log.Fatalf("unhandled kind: %v", k)
	}
	return KindC
}
`

func TestExhaustiveIgnore(t *testing.T) {
	got := analyzeFixture(t, lint.ExhaustiveIgnore, "internal/lint/fixtures", fixtureExhaustiveIgnore)
	want := []string{
		"105: //exhaustive:ignore on a switch that returns from its default without rejecting; name every member instead",
		"118: //exhaustive:ignore on a switch that returns from its default without rejecting; name every member instead",
		"130: //exhaustive:ignore on a switch that has a default that can fall through; name every member instead",
		"141: //exhaustive:ignore on a switch that has a default that can fall through; name every member instead",
		"164: //exhaustive:ignore on a switch that has a default that can fall through; name every member instead",
		"176: //exhaustive:ignore on a switch that has a default that can fall through; name every member instead",
		"188: //exhaustive:ignore on a switch that has a default that can fall through; name every member instead",
		"38: //exhaustive:ignore on a switch that returns from its default without rejecting; name every member instead",
		"48: //exhaustive:ignore on a switch that has an empty default; name every member instead",
		"58: //exhaustive:ignore on a switch that has no default; name every member instead",
		"67: //exhaustive:ignore is not attached to a switch; delete it",
	}
	assertDiagnostics(t, got, want)
}

// Driven by lint.Analyzers() rather than a hand-written list, so an analyzer
// added to the registry is enforced without touching this test.
func TestAnalyzersAcrossRepo(t *testing.T) {
	found := analyzeAll(t, lint.Analyzers(), nil, "./...")
	for _, a := range lint.Analyzers() {
		t.Run(a.Name, func(t *testing.T) {
			if v := found[a.Name]; len(v) > 0 {
				t.Errorf("%d violation(s):\n%s", len(v), strings.Join(v, "\n"))
			}
		})
	}
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
