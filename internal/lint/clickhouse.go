package lint

import (
	"go/ast"
	"go/types"
	"strconv"
	"strings"

	"golang.org/x/tools/go/analysis"
)

const chqPkg = "github.com/pug-sh/pug/internal/core/clickhouse"

// Every physical ClickHouse relation carries project_id; the CTE aliases the
// builders select from (base, funnel, per_user, latest_profiles, ...) do not.
var tenantTables = map[string]bool{
	"events":                        true,
	"profiles":                      true,
	"profile_aliases":               true,
	"event_names":                   true,
	"distinct_id_activity_states":   true,
	"dashboard_event_rollup_daily":  true,
	"dashboard_session_rollup":      true,
	"property_keys":                 true,
	"property_keys_event_buckets":   true,
	"property_keys_profile_current": true,
}

var ChqTenant = &analysis.Analyzer{
	Name: "chqtenant",
	Doc:  "a ClickHouse query over a tenant-scoped table must filter on project_id",
	Run:  runChqTenant,
}

func runChqTenant(pass *analysis.Pass) (any, error) {
	scoping := scopingFuncs(pass)
	eachFile(pass, func(w *fileWalk, file *ast.File) {
		eachFunc(file, func(name string, body *ast.BlockStmt) {
			var froms []*ast.CallExpr
			tables := map[*ast.CallExpr]string{}
			scoped := scoping[pass.TypesInfo.Defs[funcIdent(file, body)]]
			ast.Inspect(body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				// A helper that applies the filter (topKBaseConditions and
				// friends) scopes its caller too; the conditions it returns are
				// what reach Where.
				if target := callee(pass, call); target != nil && scoping[target] {
					scoped = true
				}
				if !isMethod(callee(pass, call), chqPkg, "Query", "From") || len(call.Args) != 1 {
					return true
				}
				table, ok := stringConst(pass, call.Args[0])
				if !ok {
					// A built FROM fragment cannot be read here; it still has to
					// be scoped, so treat it as tenant-scoped rather than skip.
					froms, tables[call] = append(froms, call), "a computed FROM fragment"
					return true
				}
				if t := fromTable(table); tenantTables[t] {
					froms, tables[call] = append(froms, call), strconv.Quote(t)
				}
				return true
			})
			if scoped {
				return
			}
			for _, call := range froms {
				if w.exempt(call) {
					continue
				}
				pass.Reportf(call.Pos(),
					"%s selects from tenant-scoped table %s without a project_id filter",
					name, tables[call])
			}
		})
	})
	return nil, nil
}

// fromTable takes the relation name off a FROM fragment, which may carry an
// alias and a join ("events e LEFT ANY JOIN identity_union i ON ...").
func fromTable(from string) string {
	if i := strings.IndexAny(from, " \t\n("); i >= 0 {
		return from[:i]
	}
	return from
}

// funcIdent finds the identifier a body belongs to so its types.Object can be
// looked up; func literals have none, which is why the result may be nil.
func funcIdent(file *ast.File, body *ast.BlockStmt) *ast.Ident {
	for _, decl := range file.Decls {
		if d, ok := decl.(*ast.FuncDecl); ok && d.Body == body {
			return d.Name
		}
	}
	return nil
}

// scopingFuncs are the package's functions that build a project_id filter. The
// mention has to reach a condition constructor: a Select, a GroupBy or a log
// attribute naming project_id is not a filter, and counting one as scoping is
// what would let an unfiltered query through.
func scopingFuncs(pass *analysis.Pass) map[types.Object]bool {
	out := make(map[types.Object]bool)
	for _, file := range pass.Files {
		for _, decl := range file.Decls {
			d, ok := decl.(*ast.FuncDecl)
			if !ok || d.Body == nil {
				continue
			}
			ast.Inspect(d.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok || len(call.Args) == 0 || !isCondCtor(callee(pass, call)) {
					return true
				}
				if namesProjectID(pass, call.Args[0]) {
					out[pass.TypesInfo.Defs[d.Name]] = true
				}
				return true
			})
		}
	}
	return out
}

// namesProjectID reports whether the column expression is project_id, allowing
// for a table alias built by concatenation (prefix+"project_id", "e.project_id").
func namesProjectID(pass *analysis.Pass, col ast.Expr) bool {
	found := false
	ast.Inspect(col, func(n ast.Node) bool {
		e, ok := n.(ast.Expr)
		if !ok {
			return true
		}
		if s, ok := stringConst(pass, e); ok && (s == "project_id" || strings.HasSuffix(s, ".project_id")) {
			found = true
		}
		return !found
	})
	return found
}

func isCondCtor(fn *types.Func) bool {
	switch {
	case isFunc(fn, chqPkg, "Eq"), isFunc(fn, chqPkg, "In"),
		isFunc(fn, chqPkg, "Gt"), isFunc(fn, chqPkg, "Gte"),
		isFunc(fn, chqPkg, "Lt"), isFunc(fn, chqPkg, "Lte"):
		return true
	}
	return false
}
