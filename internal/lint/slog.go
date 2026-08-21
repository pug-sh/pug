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
	Doc:  "slog.ErrorContext must be paired with telemetry.RecordError in the function that detects the error",
	Run:  runRecordErr,
}

func runRecordErr(pass *analysis.Pass) (any, error) {
	if pass.Pkg.Path() == telemetryPkg {
		return nil, nil
	}
	eachFile(pass, func(w *fileWalk, file *ast.File) {
		eachFunc(file, func(name string, body *ast.BlockStmt) {
			var logged []*ast.CallExpr
			recorded := false
			ast.Inspect(body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				switch target := callee(pass, call); {
				case isFunc(target, slogPkg, "ErrorContext"):
					logged = append(logged, call)
				case isFunc(target, telemetryPkg, "RecordError"), isFunc(target, telemetryPkg, "RecordErrorOnSpan"):
					recorded = true
				}
				return true
			})
			if recorded {
				return
			}
			for _, call := range logged {
				if w.exempt(call) {
					continue
				}
				pass.Reportf(call.Pos(),
					"%s logs an error without telemetry.RecordError; record it here or mark the line %s",
					name, exemptMarker)
			}
		})
	})
	return nil, nil
}
