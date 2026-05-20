package rpc

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// ---- fixture self-tests ----

const fixtureViolation = `package h
import "connectrpc.com/connect"
func f() error { return connect.NewError(connect.CodeNotFound, nil) }`

const fixtureExempt = `package h
import "connectrpc.com/connect"
func f() error { return connect.NewError(connect.CodeNotFound, nil) // apperr:exempt — legacy
}`

const fixtureAllowedInternal = `package h
import "connectrpc.com/connect"
func f() error { return connect.NewError(connect.CodeInternal, nil) }`

func TestScanSource(t *testing.T) {
	if got := scanSource(t, fixtureViolation); len(got) != 1 {
		t.Errorf("violation: got %d findings, want 1", len(got))
	}
	if got := scanSource(t, fixtureExempt); len(got) != 0 {
		t.Errorf("exempt: got %d findings, want 0", len(got))
	}
	if got := scanSource(t, fixtureAllowedInternal); len(got) != 0 {
		t.Errorf("internal: got %d findings, want 0", len(got))
	}
}

func scanSource(t *testing.T, src string) []finding {
	t.Helper()
	fset, file := mustParse(t, src)
	return scanFile(fset, file)
}

// ---- scanner (test-scoped) ----

// clientFacingCodes are the Connect codes that MUST go through apperr.* constructors.
var clientFacingCodes = map[string]bool{
	"CodeNotFound": true, "CodeInvalidArgument": true, "CodeAlreadyExists": true,
	"CodePermissionDenied": true, "CodeFailedPrecondition": true, "CodeUnauthenticated": true,
}

type finding struct {
	pos  token.Position
	code string
}

// scanFile reports each connect.NewError(connect.Code<client-facing>, ...) call,
// unless the line carries an "apperr:exempt" marker comment.
func scanFile(fset *token.FileSet, file *ast.File) []finding {
	exempt := exemptLines(fset, file)
	var out []finding
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || !isSelector(call.Fun, "connect", "NewError") || len(call.Args) == 0 {
			return true
		}
		code, ok := connectCodeArg(call.Args[0])
		if !ok || !clientFacingCodes[code] {
			return true
		}
		pos := fset.Position(call.Pos())
		if exempt[pos.Line] {
			return true
		}
		out = append(out, finding{pos: pos, code: code})
		return true
	})
	return out
}

func connectCodeArg(arg ast.Expr) (string, bool) {
	sel, ok := arg.(*ast.SelectorExpr)
	if !ok {
		return "", false
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok || pkg.Name != "connect" {
		return "", false
	}
	return sel.Sel.Name, true
}

func isSelector(e ast.Expr, pkg, name string) bool {
	sel, ok := e.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	return ok && id.Name == pkg && sel.Sel.Name == name
}

func exemptLines(fset *token.FileSet, file *ast.File) map[int]bool {
	lines := map[int]bool{}
	for _, cg := range file.Comments {
		for _, c := range cg.List {
			if strings.Contains(c.Text, "apperr:exempt") {
				lines[fset.Position(c.Pos()).Line] = true
			}
		}
	}
	return lines
}

func mustParse(t interface{ Fatal(...any) }, src string) (*token.FileSet, *ast.File) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "fixture.go", src, parser.ParseComments)
	if err != nil {
		t.Fatal(err)
	}
	return fset, file
}
