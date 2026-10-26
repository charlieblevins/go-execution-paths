package main

import (
	"cmp"
	"fmt"
	"go/ast"
	"go/types"
	"path/filepath"
	"slices"
	"strings"

	"github.com/charlieblevins/go-execution-paths/internal/paths"
	"golang.org/x/tools/go/packages"
)

// Func is a function or method declared with a body in the loaded packages.
type Func struct {
	ID    string `json:"id"`   // types.Func.FullName(), e.g. "(*pkg/path.T).M"
	Name  string `json:"name"` // short display name, e.g. "pkg.(*T).M"
	Pkg   string `json:"pkg"`
	File  string `json:"file"`  // relative to the index root
	Start int    `json:"start"` // 1-based line of the func keyword
	End   int    `json:"end"`   // 1-based line of the closing brace
}

// Call is a static call site of some function.
type Call struct {
	Caller     string `json:"caller"` // Func.ID of the enclosing function
	CallerName string `json:"callerName"`
	File       string `json:"file"`
	Line       int    `json:"line"`
	Col        int    `json:"col"`
}

type Index struct {
	Root    string
	Funcs   map[string]*Func
	Syntax  map[string]*paths.Func // Func.ID -> declaration, for path enumeration
	Callers map[string][]Call      // callee ID -> call sites
	Files   map[string]string      // relative path -> absolute path
	Errors  []string
}

func Load(root string, patterns []string, tests bool) (*Index, error) {
	cfg := &packages.Config{
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedSyntax |
			packages.NeedTypes | packages.NeedTypesInfo,
		Dir:   root,
		Tests: tests,
	}
	pkgs, err := packages.Load(cfg, patterns...)
	if err != nil {
		return nil, err
	}

	idx := &Index{
		Root:    root,
		Funcs:   map[string]*Func{},
		Syntax:  map[string]*paths.Func{},
		Callers: map[string][]Call{},
		Files:   map[string]string{},
	}
	type edge struct {
		callee string
		call   Call
	}
	seen := map[edge]bool{} // with tests, files appear in multiple package variants

	for _, p := range pkgs {
		for _, e := range p.Errors {
			idx.Errors = append(idx.Errors, e.Error())
		}
		for _, f := range p.Syntax {
			abs := p.Fset.File(f.Pos()).Name()
			rel, err := filepath.Rel(root, abs)
			if err != nil || strings.HasPrefix(rel, "..") {
				continue // generated files in the build cache, etc.
			}
			idx.Files[rel] = abs

			for _, decl := range f.Decls {
				d, ok := decl.(*ast.FuncDecl)
				if !ok || d.Body == nil {
					continue
				}
				obj, _ := p.TypesInfo.Defs[d.Name].(*types.Func)
				if obj == nil {
					continue
				}
				fn := &Func{
					ID:    obj.FullName(),
					Name:  displayName(obj),
					Pkg:   obj.Pkg().Path(),
					File:  rel,
					Start: p.Fset.Position(d.Pos()).Line,
					End:   p.Fset.Position(d.End()).Line,
				}
				if _, dup := idx.Funcs[fn.ID]; !dup {
					idx.Funcs[fn.ID] = fn
					idx.Syntax[fn.ID] = &paths.Func{Fset: p.Fset, File: f, Decl: d, Info: p.TypesInfo}
				}

				ast.Inspect(d.Body, func(n ast.Node) bool {
					call, ok := n.(*ast.CallExpr)
					if !ok {
						return true
					}
					callee, id := calleeOf(p.TypesInfo, call)
					if callee == nil {
						return true
					}
					pos := p.Fset.Position(id.Pos())
					e := edge{callee.FullName(), Call{
						Caller:     fn.ID,
						CallerName: fn.Name,
						File:       rel,
						Line:       pos.Line,
						Col:        pos.Column,
					}}
					if !seen[e] {
						seen[e] = true
						idx.Callers[e.callee] = append(idx.Callers[e.callee], e.call)
					}
					return true
				})
			}
		}
	}

	for _, calls := range idx.Callers {
		slices.SortFunc(calls, func(a, b Call) int {
			return cmp.Or(cmp.Compare(a.File, b.File), cmp.Compare(a.Line, b.Line), cmp.Compare(a.Col, b.Col))
		})
	}
	return idx, nil
}

// calleeOf returns the statically known function called by call, along with
// the identifier naming it. Calls through interfaces resolve to the interface
// method; calls through function values return nil.
func calleeOf(info *types.Info, call *ast.CallExpr) (*types.Func, *ast.Ident) {
	fun := ast.Unparen(call.Fun)
	switch f := fun.(type) { // explicit instantiation: f[int](x)
	case *ast.IndexExpr:
		fun = ast.Unparen(f.X)
	case *ast.IndexListExpr:
		fun = ast.Unparen(f.X)
	}
	var id *ast.Ident
	switch f := fun.(type) {
	case *ast.Ident:
		id = f
	case *ast.SelectorExpr:
		id = f.Sel
	default:
		return nil, nil
	}
	fn, _ := info.Uses[id].(*types.Func)
	if fn == nil {
		return nil, nil
	}
	return fn.Origin(), id
}

func displayName(fn *types.Func) string {
	pkg := fn.Pkg().Name()
	recv := fn.Signature().Recv()
	if recv == nil {
		return pkg + "." + fn.Name()
	}
	t := types.TypeString(recv.Type(), func(*types.Package) string { return "" })
	if strings.HasPrefix(t, "*") {
		return fmt.Sprintf("%s.(%s).%s", pkg, t, fn.Name())
	}
	return fmt.Sprintf("%s.%s.%s", pkg, t, fn.Name())
}
