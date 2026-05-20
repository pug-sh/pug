package rpc

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
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

// Aliased imports must not defeat the scanners — they are the sole enforcement,
// so matching on the literal package name "connect"/"apperr" would be a hole.
func TestScanSource_aliasedImports(t *testing.T) {
	const aliasedRaw = `package h
import conn "connectrpc.com/connect"
func f() error { return conn.NewError(conn.CodeNotFound, nil) }`
	if got := scanSource(t, aliasedRaw); len(got) != 1 {
		t.Errorf("aliased connect import: got %d raw-error findings, want 1", len(got))
	}

	const aliasedReason = `package h
import ae "github.com/pug-sh/pug/internal/apperr"
func f() error { return ae.NotFound(ae.ReasonNotFound, "x") }`
	if got := scanGenericReasonsSrc(t, aliasedReason); len(got) != 1 {
		t.Errorf("aliased apperr import: got %d generic-reason findings, want 1", len(got))
	}
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
	connectName, ok := importName(file, "connectrpc.com/connect")
	if !ok {
		return nil // file cannot reference connect.NewError
	}
	exempt := exemptLines(fset, file)
	var out []finding
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || !isSelector(call.Fun, connectName, "NewError") || len(call.Args) == 0 {
			return true
		}
		code, ok := connectCodeArg(call.Args[0], connectName)
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

func connectCodeArg(arg ast.Expr, connectName string) (string, bool) {
	sel, ok := arg.(*ast.SelectorExpr)
	if !ok {
		return "", false
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok || pkg.Name != connectName {
		return "", false
	}
	return sel.Sel.Name, true
}

// importName returns the local identifier the file uses for an import path,
// honoring aliases. Returns false if the path is not imported, or is imported
// under a name that cannot prefix a selector ("_" or "."). Resolving the alias
// (rather than matching a literal "connect"/"apperr") closes the bypass where a
// renamed import would slip a client-facing error past the scanner.
func importName(file *ast.File, path string) (string, bool) {
	for _, imp := range file.Imports {
		p, err := strconv.Unquote(imp.Path.Value)
		if err != nil || p != path {
			continue
		}
		if imp.Name != nil {
			if imp.Name.Name == "_" || imp.Name.Name == "." {
				return "", false
			}
			return imp.Name.Name, true
		}
		if i := strings.LastIndex(p, "/"); i >= 0 {
			return p[i+1:], true
		}
		return p, true
	}
	return "", false
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

// ---- real-package raw-error enforcement ----

func TestNoClientFacingRawErrors(t *testing.T) {
	fset := token.NewFileSet()
	var findings []finding
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, perr := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if perr != nil {
			return perr
		}
		findings = append(findings, scanFile(fset, file)...)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range findings {
		t.Errorf("%s: client-facing %s must use apperr.* (or // apperr:exempt)", f.pos, f.code)
	}
}

// ---- generic-reason scanner ----

// genericReasons are the code-mirror fallback reasons; using one at a non-auth
// client-facing constructor defeats the "actual reason" guarantee.
var genericReasons = map[string]bool{
	"ReasonUnknown": true, "ReasonCanceled": true, "ReasonInvalidArgument": true,
	"ReasonDeadlineExceeded": true, "ReasonNotFound": true, "ReasonAlreadyExists": true,
	"ReasonPermissionDenied": true, "ReasonResourceExhausted": true, "ReasonFailedPrecondition": true,
	"ReasonAborted": true, "ReasonOutOfRange": true, "ReasonUnimplemented": true,
	"ReasonInternal": true, "ReasonUnavailable": true, "ReasonDataLoss": true,
	"ReasonUnauthenticated": true,
}

// nonAuthClientFacingConstructors are the apperr constructors that must carry a specific reason.
var nonAuthClientFacingConstructors = map[string]bool{
	"NotFound": true, "Invalid": true, "AlreadyExists": true,
	"PermissionDenied": true, "FailedPrecondition": true,
}

// scanGenericReasons flags apperr.<Constructor>(apperr.Reason<Generic>, ...) calls
// at non-auth client-facing constructors, unless the line is apperr:exempt.
func scanGenericReasons(fset *token.FileSet, file *ast.File) []finding {
	apperrName, ok := importName(file, "github.com/pug-sh/pug/internal/apperr")
	if !ok {
		return nil // file cannot reference apperr constructors
	}
	exempt := exemptLines(fset, file)
	var out []finding
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) == 0 {
			return true
		}
		if !isApperrSelector(call.Fun, apperrName, nonAuthClientFacingConstructors) {
			return true
		}
		reasonSel, ok := call.Args[0].(*ast.SelectorExpr)
		if !ok {
			return true
		}
		rpkg, ok := reasonSel.X.(*ast.Ident)
		if !ok || rpkg.Name != apperrName || !genericReasons[reasonSel.Sel.Name] {
			return true
		}
		pos := fset.Position(call.Pos())
		if exempt[pos.Line] {
			return true
		}
		out = append(out, finding{pos: pos, code: reasonSel.Sel.Name})
		return true
	})
	return out
}

// isApperrSelector reports whether e is <apperrName>.<name> with name in names.
func isApperrSelector(e ast.Expr, apperrName string, names map[string]bool) bool {
	sel, ok := e.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	return ok && id.Name == apperrName && names[sel.Sel.Name]
}

// ---- generic-reason fixture self-tests ----

const fixtureGenericReason = `package h
import "github.com/pug-sh/pug/internal/apperr"
func f() error { return apperr.NotFound(apperr.ReasonNotFound, "x") }`

const fixtureSpecificReason = `package h
import "github.com/pug-sh/pug/internal/apperr"
func f() error { return apperr.NotFound(apperr.ReasonProfileNotFound, "x") }`

const fixtureGenericExempt = `package h
import "github.com/pug-sh/pug/internal/apperr"
func f() error { return apperr.AlreadyExists(apperr.ReasonAlreadyExists, "x") // apperr:exempt — legacy
}`

func TestScanGenericReasons(t *testing.T) {
	if got := scanGenericReasonsSrc(t, fixtureGenericReason); len(got) != 1 {
		t.Errorf("generic: got %d, want 1", len(got))
	}
	if got := scanGenericReasonsSrc(t, fixtureSpecificReason); len(got) != 0 {
		t.Errorf("specific: got %d, want 0", len(got))
	}
	if got := scanGenericReasonsSrc(t, fixtureGenericExempt); len(got) != 0 {
		t.Errorf("exempt: got %d, want 0", len(got))
	}
}

func scanGenericReasonsSrc(t *testing.T, src string) []finding {
	t.Helper()
	fset, file := mustParse(t, src)
	return scanGenericReasons(fset, file)
}

// ---- real-package generic-reason enforcement ----

func TestNoGenericReasonAtClientFacing(t *testing.T) {
	fset := token.NewFileSet()
	var findings []finding
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, perr := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if perr != nil {
			return perr
		}
		findings = append(findings, scanGenericReasons(fset, file)...)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range findings {
		t.Errorf("%s: client-facing constructor uses generic reason apperr.%s; use a specific reason (or // apperr:exempt)", f.pos, f.code)
	}
}
