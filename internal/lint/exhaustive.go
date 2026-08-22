package lint

import (
	"fmt"
	"go/ast"
	"go/constant"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/types/typeutil"
)

const exhaustiveIgnore = "//exhaustive:ignore"

var ExhaustiveIgnore = &analysis.Analyzer{
	Name: "exhaustiveignore",
	Doc:  "an //exhaustive:ignore is only valid on a switch whose default rejects on every path: it panics, or ends in a return and every return in it yields zero values or an error",
	Run:  runExhaustiveIgnore,
}

// The comment claims the switch never needs every enum member named, which is
// true only while its default rejects rather than picking a value. Nothing ties
// the comment to that shape, so a later edit turning the default into a real
// answer would leave the exemption silently covering a dispatch bug.
//
// Only switch statements are checked, matching `check: [switch]` in
// .golangci.yml; adding `map` there needs a map-literal arm here too.
func runExhaustiveIgnore(pass *analysis.Pass) (any, error) {
	for _, file := range pass.Files {
		cmap := ast.NewCommentMap(pass.Fset, file, file.Comments)

		claimed := map[*ast.Comment]bool{}
		ast.Inspect(file, func(n ast.Node) bool {
			sw, ok := n.(*ast.SwitchStmt)
			if !ok {
				return true
			}
			c := ignoreComment(cmap[n])
			if c == nil {
				return true
			}
			claimed[c] = true
			if reason := whyNotRejecting(pass, sw); reason != "" {
				pass.Reportf(c.Pos(), "//exhaustive:ignore on a switch that %s; name every member instead", reason)
			}
			return true
		})

		for _, group := range file.Comments {
			for _, c := range group.List {
				if strings.HasPrefix(c.Text, exhaustiveIgnore) && !claimed[c] {
					pass.Reportf(c.Pos(), "//exhaustive:ignore is not attached to a switch; delete it")
				}
			}
		}
	}
	return nil, nil
}

func ignoreComment(groups []*ast.CommentGroup) *ast.Comment {
	for _, g := range groups {
		for _, c := range g.List {
			if strings.HasPrefix(c.Text, exhaustiveIgnore) {
				return c
			}
		}
	}
	return nil
}

// whyNotRejecting returns why sw's default is not a rejection, or "" when it is.
func whyNotRejecting(pass *analysis.Pass, sw *ast.SwitchStmt) string {
	var def *ast.CaseClause
	for _, stmt := range sw.Body.List {
		if cc, ok := stmt.(*ast.CaseClause); ok && cc.List == nil {
			def = cc
		}
	}
	switch {
	case def == nil:
		return "has no default"
	case len(def.Body) == 0:
		// An empty default falls through to whatever follows the switch, which is
		// the silent-dispatch shape the comment must not cover.
		return "has an empty default"
	}

	// Every return must reject. Latching on the last one seen would let a real
	// dispatch pass just by sitting above a rejecting fallback.
	allReject := true
	for _, stmt := range def.Body {
		ast.Inspect(stmt, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.FuncLit:
				return false
			case *ast.ReturnStmt:
				if !rejectingResults(pass, x.Results) {
					allReject = false
				}
			}
			return true
		})
	}
	switch {
	case !allReject:
		return "returns from its default without rejecting"
	case !terminates(pass, def.Body):
		// Rejecting on the paths that do return is not enough: a conditional
		// return leaves a path that walks off the default and carries on.
		return "has a default that can fall through"
	}
	return ""
}

// terminates reports whether the body ends in something no path falls out of: a
// return, which the caller has already proven rejecting, or a call that never
// comes back. Resolved by object identity, so a shadowed panic or a local named
// os does not pass as one.
func terminates(pass *analysis.Pass, body []ast.Stmt) bool {
	last := body[len(body)-1]
	if _, ok := last.(*ast.ReturnStmt); ok {
		return true
	}
	stmt, ok := last.(*ast.ExprStmt)
	if !ok {
		return false
	}
	call, ok := stmt.X.(*ast.CallExpr)
	if !ok {
		return false
	}
	if b, ok := typeutil.Callee(pass.TypesInfo, call).(*types.Builtin); ok {
		return b.Name() == "panic"
	}
	fn := callee(pass, call)
	if isFunc(fn, "os", "Exit") {
		return true
	}
	return fn != nil && fn.Pkg() != nil && fn.Pkg().Path() == "log" && strings.HasPrefix(fn.Name(), "Fatal")
}

// A bare return rejects nothing: it hands back whatever the named results or
// out-params already hold.
func rejectingResults(pass *analysis.Pass, results []ast.Expr) bool {
	if len(results) == 0 {
		return false
	}
	for _, r := range results {
		if !zeroValue(pass, r) && !isError(pass, r) {
			return false
		}
	}
	return true
}

func zeroValue(pass *analysis.Pass, e ast.Expr) bool {
	if tv, ok := pass.TypesInfo.Types[e]; ok && tv.Value != nil {
		switch tv.Value.Kind() {
		case constant.Bool:
			return !constant.BoolVal(tv.Value)
		case constant.String:
			return constant.StringVal(tv.Value) == ""
		case constant.Int, constant.Float:
			return constant.Sign(tv.Value) == 0
		case constant.Unknown, constant.Complex:
			return false
		}
	}
	switch x := e.(type) {
	case *ast.Ident:
		return x.Name == "nil"
	case *ast.CompositeLit:
		return len(x.Elts) == 0
	}
	return false
}

func isError(pass *analysis.Pass, e ast.Expr) bool {
	t := pass.TypesInfo.TypeOf(e)
	return t != nil && types.Implements(t, errorIface)
}

// The analyzer above loads non-test packages only, so a directive in a _test.go
// would silence upstream exhaustive with nothing checking its default. A test
// switch has no business needing the exemption, so ban it outright. internal/lint
// is skipped because that is where the rule's own fixtures live.
func checkExhaustiveIgnoreInTests(root string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, "_test.go") {
			return err
		}
		if strings.HasPrefix(rel(root, path), "internal/lint/") {
			return nil
		}
		// Parsed rather than grepped so a test holding the directive as fixture
		// data is not accused of declaring one.
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, nil, parser.ParseComments|parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		for _, g := range f.Comments {
			for _, c := range g.List {
				if strings.HasPrefix(c.Text, exhaustiveIgnore) {
					out = append(out, fmt.Sprintf("%s:%d: %s in a test is unchecked; name every member instead", rel(root, path), fset.Position(c.Pos()).Line, exhaustiveIgnore))
				}
			}
		}
		return nil
	})
	sort.Strings(out)
	return out, err
}
