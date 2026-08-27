package lint

import (
	"go/ast"
	"go/types"

	"golang.org/x/tools/go/analysis"
)

var SentinelErr = &analysis.Analyzer{
	Name: "sentinelerr",
	Doc:  "sentinel errors must not reach connect.NewError; their text is internal and would leak to API consumers",
	Run:  runSentinelErr,
}

func runSentinelErr(pass *analysis.Pass) (any, error) {
	eachFile(pass, func(w *fileWalk, file *ast.File) {
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) != 2 || !isFunc(callee(pass, call), connectPkg, "NewError") {
				return true
			}
			sentinel := wrappedSentinel(pass, call.Args[1])
			if sentinel == nil || w.exempt(call) {
				return true
			}
			pass.Reportf(call.Args[1].Pos(),
				"sentinel %s reaches connect.NewError; pass errors.New with an explicit client-facing message",
				sentinel.Name())
			return true
		})
	})
	return nil, nil
}

// wrappedSentinel looks through fmt.Errorf and errors.Join: %w-wrapping is the
// spelling where the sentinel's text actually reaches the client, since
// Error() concatenates it.
func wrappedSentinel(pass *analysis.Pass, e ast.Expr) *types.Var {
	if v := packageLevelError(pass, e); v != nil {
		return v
	}
	call, ok := e.(*ast.CallExpr)
	if !ok {
		return nil
	}
	fn := callee(pass, call)
	if !isFunc(fn, "fmt", "Errorf") && !isFunc(fn, "errors", "Join") {
		return nil
	}
	for _, arg := range call.Args {
		if v := wrappedSentinel(pass, arg); v != nil {
			return v
		}
	}
	return nil
}

var Principal = &analysis.Analyzer{
	Name: "principal",
	Doc:  "handlers must read the Principal through MustGetPrincipalWith*, never getPrincipalFromContext",
	Run:  runPrincipal,
}

// The two extractors and the two interceptor helpers are the only legitimate
// callers: everywhere else the nil-Customer/nil-Project check is the point.
var principalCallers = map[string]bool{
	"getPrincipalFromContext":      true,
	"MustGetPrincipalWithCustomer": true,
	"MustGetPrincipalWithProject":  true,
	"enrichSpanWithPrincipal":      true,
	"authorizeRoleGated":           true,
}

func runPrincipal(pass *analysis.Pass) (any, error) {
	if pass.Pkg.Path() != rpcPkg {
		return nil, nil
	}
	eachFile(pass, func(w *fileWalk, file *ast.File) {
		eachFunc(file, func(name string, body *ast.BlockStmt) {
			if principalCallers[name] {
				return
			}
			ast.Inspect(body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok || !isFunc(callee(pass, call), rpcPkg, "getPrincipalFromContext") || w.exempt(call) {
					return true
				}
				pass.Reportf(call.Pos(),
					"%s calls getPrincipalFromContext directly; use MustGetPrincipalWithCustomer or MustGetPrincipalWithProject",
					name)
				return true
			})
		})
	})
	return nil, nil
}
