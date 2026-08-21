package lint

import (
	"go/ast"
	"go/types"
	"strconv"
	"strings"
	"unicode"

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
			q := &queryScopes{pass: pass, scoping: scoping, conds: map[types.Object]bool{}}
			q.collect(body)
			for _, f := range q.froms {
				if q.scoped[f.key] || (f.computed && q.covered(f.key)) || w.exempt(f.call) {
					continue
				}
				pass.Reportf(f.call.Pos(),
					"%s selects from tenant-scoped table %s without a project_id filter",
					name, f.table)
			}
		})
	})
	return nil, nil
}

// queryScopes resolves scoping per query rather than per function: one filtered
// query in a function must not vouch for an unfiltered one beside it. A query is
// keyed by the variable it is bound to, so the fluent chain and the
// `q := ...; q.Where(...)` form land on the same key; an unbound chain is keyed
// by its own outermost call.
type queryScopes struct {
	pass    *analysis.Pass
	scoping map[*types.Func]bool
	conds   map[types.Object]bool // locals holding a project_id condition
	scoped  map[any]bool
	viaCTE  map[any]bool // declares a CTE that is itself scoped
	mounted map[any]any  // sub-query -> the query that mounts it as a CTE
	froms   []fromSite
}

type fromSite struct {
	call     *ast.CallExpr
	key      any
	table    string
	computed bool
}

func (q *queryScopes) collect(body *ast.BlockStmt) {
	q.scoped, q.viaCTE, q.mounted = map[any]bool{}, map[any]bool{}, map[any]any{}
	bound := boundQueries(q.pass, body)
	handled := map[*ast.CallExpr]bool{}
	ast.Inspect(body, func(n ast.Node) bool {
		q.noteConditions(n)
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if handled[call] {
			return true
		}
		links, root := chainLinks(q.pass, call)
		if len(links) == 0 {
			return true
		}
		key := q.key(call, root, bound)
		for _, link := range links {
			handled[link] = true
			q.visitLink(link, key)
		}
		return true
	})
}

// noteConditions tracks locals that carry a project_id filter, so `conds, err :=
// baseConditions(...)` followed by `.Where(conds...)` reads as scoping.
func (q *queryScopes) noteConditions(n ast.Node) {
	assign, ok := n.(*ast.AssignStmt)
	if !ok {
		return
	}
	for _, rhs := range assign.Rhs {
		if !q.isScopingExpr(rhs) {
			continue
		}
		for _, lhs := range assign.Lhs {
			if id, ok := lhs.(*ast.Ident); ok {
				if v, ok := q.pass.TypesInfo.ObjectOf(id).(*types.Var); ok {
					q.conds[v] = true
				}
			}
		}
	}
}

func (q *queryScopes) visitLink(link *ast.CallExpr, key any) {
	fn := callee(q.pass, link)
	// A helper handed the query applies its filter out of sight; the conditions
	// a helper returns instead reach Where as an argument.
	if q.scoping[fn] {
		q.scoped[key] = true
	}
	for _, arg := range link.Args {
		if q.isScopingExpr(arg) {
			q.scoped[key] = true
		}
	}
	// Mounting runs both ways: a query that mounts a scoped CTE reads that CTE's
	// already-filtered rows, and a CTE mounted on a scoped query is read only
	// through it. Either vouches for a FROM fragment the analyzer cannot read —
	// a literal tenant table still has to carry its own filter.
	if isMethod(fn, chqPkg, "Query", "With") && len(link.Args) == 2 {
		sub := q.exprKey(link.Args[1])
		if q.scoped[sub] {
			q.viaCTE[key] = true
		}
		q.mounted[sub] = key
	}
	if !isMethod(fn, chqPkg, "Query", "From") || len(link.Args) != 1 {
		return
	}
	table, ok := stringConst(q.pass, link.Args[0])
	if !ok {
		// A built FROM fragment cannot be read here; it still has to be scoped,
		// so treat it as tenant-scoped rather than skip.
		q.froms = append(q.froms, fromSite{link, key, "a computed FROM fragment", true})
		return
	}
	if names := fromTables(table); len(names) > 0 {
		q.froms = append(q.froms, fromSite{link, key, strings.Join(names, ", "), false})
	}
}

// key identifies the query a chain belongs to: the variable it reads from, the
// variable it is assigned to, or the chain itself when it is neither.
func (q *queryScopes) key(outer *ast.CallExpr, root ast.Expr, bound map[*ast.CallExpr]types.Object) any {
	if id, ok := root.(*ast.Ident); ok {
		if v, ok := q.pass.TypesInfo.ObjectOf(id).(*types.Var); ok {
			return v
		}
	}
	if v, ok := bound[outer]; ok {
		return v
	}
	return outer
}

// exprKey resolves a sub-query argument back to the key its own chain was filed
// under, so `With("agg", aggCTE)` can ask whether aggCTE is scoped. A slice
// element keys to the slice: the index is a loop variable, so the elements
// cannot be told apart statically — and a loop that builds them builds them
// alike.
func (q *queryScopes) exprKey(e ast.Expr) any {
	switch e := e.(type) {
	case *ast.Ident:
		if v, ok := q.pass.TypesInfo.ObjectOf(e).(*types.Var); ok {
			return v
		}
	case *ast.IndexExpr:
		if id, ok := e.X.(*ast.Ident); ok {
			if v, ok := q.pass.TypesInfo.ObjectOf(id).(*types.Var); ok {
				return v
			}
		}
	}
	return e
}

// covered reports whether a query is filtered, or is read only through one that
// is. Bounded by the number of keys, so a cyclic mount cannot spin.
func (q *queryScopes) covered(key any) bool {
	for range len(q.mounted) + 1 {
		if q.scoped[key] || q.viaCTE[key] {
			return true
		}
		next, ok := q.mounted[key]
		if !ok {
			return false
		}
		key = next
	}
	return false
}

func (q *queryScopes) isScopingExpr(e ast.Expr) bool {
	found := false
	ast.Inspect(e, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.CallExpr:
			fn := callee(q.pass, n)
			if q.scoping[fn] {
				found = true
			}
			if isCondCtor(fn) && len(n.Args) > 0 && namesProjectID(q.pass, n.Args[0]) {
				found = true
			}
		case *ast.Ident:
			if v, ok := q.pass.TypesInfo.Uses[n].(*types.Var); ok && q.conds[v] {
				found = true
			}
		}
		return !found
	})
	return found
}

// boundQueries maps the outermost call of a query chain to the variable it is
// assigned to, so a later chain starting from that variable shares its scoping.
func boundQueries(pass *analysis.Pass, body *ast.BlockStmt) map[*ast.CallExpr]types.Object {
	out := map[*ast.CallExpr]types.Object{}
	bind := func(lhs []ast.Expr, rhs []ast.Expr) {
		if len(lhs) != len(rhs) {
			return
		}
		for i, r := range rhs {
			call, ok := r.(*ast.CallExpr)
			if !ok || !isQueryExpr(pass, r) {
				continue
			}
			target := lhs[i]
			if idx, ok := target.(*ast.IndexExpr); ok {
				target = idx.X
			}
			id, ok := target.(*ast.Ident)
			if !ok {
				continue
			}
			if v, ok := pass.TypesInfo.ObjectOf(id).(*types.Var); ok {
				out[call] = v
			}
		}
	}
	ast.Inspect(body, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.AssignStmt:
			bind(n.Lhs, n.Rhs)
		case *ast.ValueSpec:
			for i, name := range n.Names {
				if i < len(n.Values) {
					bind([]ast.Expr{name}, []ast.Expr{n.Values[i]})
				}
			}
		}
		return true
	})
	return out
}

// chainLinks walks a fluent builder chain from its outermost call down to the
// expression it starts at, returning nothing when the call is not on a *Query.
func chainLinks(pass *analysis.Pass, call *ast.CallExpr) ([]*ast.CallExpr, ast.Expr) {
	if !isQueryExpr(pass, call) {
		return nil, nil
	}
	var links []*ast.CallExpr
	for {
		links = append(links, call)
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return links, nil
		}
		inner, ok := sel.X.(*ast.CallExpr)
		if !ok || !isQueryExpr(pass, sel.X) {
			return links, sel.X
		}
		call = inner
	}
}

func isQueryExpr(pass *analysis.Pass, e ast.Expr) bool {
	ptr, ok := pass.TypesInfo.TypeOf(e).(*types.Pointer)
	if !ok {
		return false
	}
	named, ok := ptr.Elem().(*types.Named)
	return ok && named.Obj().Name() == "Query" &&
		named.Obj().Pkg() != nil && named.Obj().Pkg().Path() == chqPkg
}

// fromTables takes the tenant-scoped relations out of a FROM fragment, which may
// carry aliases and joins ("per_user p JOIN events e ON ..."). Every relation in
// the fragment counts, not just the first: a join reaches the tenant table just
// as a bare FROM does.
func fromTables(from string) []string {
	var out []string
	seen := map[string]bool{}
	for _, tok := range strings.FieldsFunc(from, func(r rune) bool {
		return unicode.IsSpace(r) || strings.ContainsRune("(),", r)
	}) {
		if tenantTables[tok] && !seen[tok] {
			seen[tok] = true
			out = append(out, strconv.Quote(tok))
		}
	}
	return out
}

// scopingFuncs are the package's functions that build a project_id filter. The
// mention has to reach a condition constructor: a Select, a GroupBy or a log
// attribute naming project_id is not a filter, and counting one as scoping is
// what would let an unfiltered query through.
func scopingFuncs(pass *analysis.Pass) map[*types.Func]bool {
	out := make(map[*types.Func]bool)
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
					if fn, ok := pass.TypesInfo.Defs[d.Name].(*types.Func); ok {
						out[fn] = true
					}
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
