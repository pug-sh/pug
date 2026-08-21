package lint

import (
	"go/ast"
	"go/constant"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/types/typeutil"
)

func callee(pass *analysis.Pass, call *ast.CallExpr) *types.Func {
	fn, _ := typeutil.Callee(pass.TypesInfo, call).(*types.Func)
	return fn
}

func isFunc(fn *types.Func, pkgPath, name string) bool {
	return fn != nil && fn.Name() == name && fn.Pkg() != nil && fn.Pkg().Path() == pkgPath
}

func isMethod(fn *types.Func, pkgPath, recv, name string) bool {
	if !isFunc(fn, pkgPath, name) || fn.Signature().Recv() == nil {
		return false
	}
	t := fn.Signature().Recv().Type()
	if ptr, ok := t.(*types.Pointer); ok {
		t = ptr.Elem()
	}
	named, ok := t.(*types.Named)
	return ok && named.Obj().Name() == recv
}

func stringConst(pass *analysis.Pass, e ast.Expr) (string, bool) {
	tv, ok := pass.TypesInfo.Types[e]
	if !ok || tv.Value == nil || tv.Value.Kind() != constant.String {
		return "", false
	}
	return constant.StringVal(tv.Value), true
}

var errorIface = types.Universe.Lookup("error").Type().Underlying().(*types.Interface)

func packageLevelError(pass *analysis.Pass, e ast.Expr) *types.Var {
	var id *ast.Ident
	switch x := e.(type) {
	case *ast.Ident:
		id = x
	case *ast.SelectorExpr:
		id = x.Sel
	default:
		return nil
	}
	v, ok := pass.TypesInfo.Uses[id].(*types.Var)
	if !ok || v.Pkg() == nil || v.Parent() != v.Pkg().Scope() {
		return nil
	}
	if !types.Implements(v.Type(), errorIface) {
		return nil
	}
	return v
}

type fileWalk struct {
	pass  *analysis.Pass
	lines map[int]bool
}

// A marker anywhere in the node's line span counts: on a multi-line call it
// lands naturally on the closing line, not the line the call starts on.
func (w *fileWalk) exempt(n ast.Node) bool {
	last := w.pass.Fset.Position(n.End()).Line
	for l := w.pass.Fset.Position(n.Pos()).Line; l <= last; l++ {
		if w.lines[l] {
			return true
		}
	}
	return false
}

func eachFile(pass *analysis.Pass, fn func(w *fileWalk, file *ast.File)) {
	for _, file := range pass.Files {
		fn(&fileWalk{pass: pass, lines: exemptLines(pass, file)}, file)
	}
}

func eachFunc(file *ast.File, fn func(name string, body *ast.BlockStmt)) {
	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			if d.Body != nil {
				fn(d.Name.Name, d.Body)
			}
		case *ast.GenDecl:
			// var handler = func(...) {...} is a body too.
			ast.Inspect(d, func(n ast.Node) bool {
				if lit, ok := n.(*ast.FuncLit); ok {
					fn(declName(d), lit.Body)
				}
				return true
			})
		}
	}
}

func declName(d *ast.GenDecl) string {
	for _, spec := range d.Specs {
		if v, ok := spec.(*ast.ValueSpec); ok && len(v.Names) > 0 {
			return v.Names[0].Name
		}
	}
	return "declaration"
}

// exemptLines mirrors the apperr:exempt idiom already used in the rpc package:
// a carve-out is a marker on the offending line, never a config entry.
func exemptLines(pass *analysis.Pass, file *ast.File) map[int]bool {
	out := make(map[int]bool)
	for _, group := range file.Comments {
		for _, c := range group.List {
			if strings.Contains(c.Text, exemptMarker) {
				out[pass.Fset.Position(c.Pos()).Line] = true
			}
		}
	}
	return out
}

func rel(root, path string) string {
	r, err := filepath.Rel(root, path)
	if err != nil {
		return path
	}
	return r
}

func walkGo(root string, fn func(path string, body []byte)) error {
	return filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		switch {
		case err != nil:
			return err
		case d.IsDir() && (d.Name() == "gen" || d.Name() == "node_modules"):
			return filepath.SkipDir
		case d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go"):
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		fn(path, body)
		return nil
	})
}

// importsOfDir unions the imports of every file in dir: which file of a package
// holds an import is not something a convention should depend on.
func importsOfDir(dir string) (map[string]bool, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		f, err := parser.ParseFile(token.NewFileSet(), filepath.Join(dir, e.Name()), nil, parser.ImportsOnly)
		if err != nil {
			return nil, err
		}
		for _, imp := range f.Imports {
			p, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				return nil, err
			}
			out[p] = true
		}
	}
	return out, nil
}
