package lint

import (
	"go/ast"

	"golang.org/x/tools/go/analysis"
)

var SlogxErr = &analysis.Analyzer{
	Name: "slogxerr",
	Doc:  `errors must be logged with slogx.Error(err), never a hand-rolled slog.Any("error", err)`,
	Run:  runSlogxErr,
}

func runSlogxErr(pass *analysis.Pass) (any, error) {
	if pass.Pkg.Path() == slogxPkg {
		return nil, nil
	}
	eachFile(pass, func(w *fileWalk, file *ast.File) {
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) != 2 {
				return true
			}
			fn := callee(pass, call)
			if !isFunc(fn, slogPkg, "Any") && !isFunc(fn, slogPkg, "String") {
				return true
			}
			key, ok := stringConst(pass, call.Args[0])
			if !ok || (key != "error" && key != "err") || w.exempt(call) {
				return true
			}
			pass.Reportf(call.Pos(), "use slogx.Error(err) instead of slog.%s(%q, ...)", fn.Name(), key)
			return true
		})
	})
	return nil, nil
}

var RecordErr = &analysis.Analyzer{
	Name: "recorderr",
	Doc:  "an error logged with slog.ErrorContext must be paired with telemetry.RecordError in the function that detects it",
	Run:  runRecordErr,
}

func runRecordErr(pass *analysis.Pass) (any, error) {
	// Entrypoints and package init run outside any span, so there is no trace for
	// RecordError to reach.
	if pass.Pkg.Path() == telemetryPkg || pass.Pkg.Name() == "main" {
		return nil, nil
	}
	eachFile(pass, func(w *fileWalk, file *ast.File) {
		inits := packageInitBodies(file)
		eachFunc(file, func(name string, body *ast.BlockStmt) {
			if inits[body] {
				return
			}
			type site struct {
				call  *ast.CallExpr
				block *ast.BlockStmt
			}
			var logged []site
			recorded := map[*ast.BlockStmt]bool{}
			parent := map[*ast.BlockStmt]*ast.BlockStmt{}
			var stack []ast.Node
			blocks := []*ast.BlockStmt{}
			ast.Inspect(body, func(n ast.Node) bool {
				if n == nil {
					if _, ok := stack[len(stack)-1].(*ast.BlockStmt); ok {
						blocks = blocks[:len(blocks)-1]
					}
					stack = stack[:len(stack)-1]
					return true
				}
				stack = append(stack, n)
				switch n := n.(type) {
				case *ast.BlockStmt:
					if len(blocks) > 0 {
						parent[n] = blocks[len(blocks)-1]
					}
					blocks = append(blocks, n)
				case *ast.CallExpr:
					switch target := callee(pass, n); {
					case isFunc(target, slogPkg, "ErrorContext"):
						if carriesError(pass, n) {
							logged = append(logged, site{n, blocks[len(blocks)-1]})
						}
					case isFunc(target, telemetryPkg, "RecordError"), isFunc(target, telemetryPkg, "RecordErrorOnSpan"):
						recorded[blocks[len(blocks)-1]] = true
					}
				}
				return true
			})
			for _, s := range logged {
				if recordedAtOrAbove(recorded, parent, s.block) || w.exempt(s.call) {
					continue
				}
				pass.Reportf(s.call.Pos(),
					"%s logs an error without telemetry.RecordError; record it here or mark the line %s",
					name, exemptMarker)
			}
		})
	})
	return nil, nil
}

// packageInitBodies is the file's package-level init bodies. Receiver-checked
// and keyed by body, so a method named init is still analyzed.
func packageInitBodies(file *ast.File) map[*ast.BlockStmt]bool {
	out := map[*ast.BlockStmt]bool{}
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv == nil && fn.Name.Name == "init" && fn.Body != nil {
			out[fn.Body] = true
		}
	}
	return out
}

// carriesError reports whether the log call has an error to record at all — an
// ERROR line about a state change has nothing to pair with. Any error-typed
// subexpression counts, and so does a spread argument list, which is opaque
// here: the rule would rather ask for a needless pairing than miss one.
func carriesError(pass *analysis.Pass, call *ast.CallExpr) bool {
	if call.Ellipsis.IsValid() {
		return true
	}
	found := false
	for _, arg := range call.Args {
		ast.Inspect(arg, func(n ast.Node) bool {
			if e, ok := n.(ast.Expr); ok && isError(pass, e) {
				found = true
			}
			return !found
		})
	}
	return found
}

// recordedAtOrAbove reports whether a RecordError sits in this block or in one
// enclosing it. A record in an enclosing block still covers the log — it runs on
// the same path — but a record in a sibling branch does not, which is what
// stops one telemetry call from clearing every error path in a function.
func recordedAtOrAbove(recorded map[*ast.BlockStmt]bool, parent map[*ast.BlockStmt]*ast.BlockStmt, b *ast.BlockStmt) bool {
	for ; b != nil; b = parent[b] {
		if recorded[b] {
			return true
		}
	}
	return false
}
