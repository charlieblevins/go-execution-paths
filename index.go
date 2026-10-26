package main

import (
	"cmp"
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"path/filepath"
	"slices"
	"strings"

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

// Call kinds for Callee.Kind.
const (
	KindStatic    = "static"    // a function or concrete method in the index; can be expanded
	KindInterface = "interface" // a method of an interface declared in the loaded packages
	KindDynamic   = "dynamic"   // a call through a function value
)

// Callee is a call made from within a function.
type Callee struct {
	Line int    `json:"line"`
	Col  int    `json:"col"`
	ID   string `json:"id,omitempty"` // Func.ID of the callee; empty for dynamic calls
	Name string `json:"name"`
	Kind string `json:"kind"`
}

type Index struct {
	Root    string
	Funcs   map[string]*Func
	Callers map[string][]Call   // callee ID -> call sites
	Calls   map[string][]Callee // caller ID -> calls it makes
	Files   map[string]string   // relative path -> absolute path
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
		Callers: map[string][]Call{},
		Calls:   map[string][]Callee{},
		Files:   map[string]string{},
	}
	loaded := map[string]bool{}
	for _, p := range pkgs {
		loaded[p.PkgPath] = true
	}
	type edge struct {
		callee string
		call   Call
	}
	type out struct {
		caller string
		callee Callee
	}
	// With tests, files appear in multiple package variants.
	seenEdge, seenOut := map[edge]bool{}, map[out]bool{}

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
				}

				ast.Inspect(d.Body, func(n ast.Node) bool {
					call, ok := n.(*ast.CallExpr)
					if !ok {
						return true
					}
					c, ok := classify(p.TypesInfo, call, loaded)
					if !ok {
						return true
					}
					pos := p.Fset.Position(c.pos)
					c.Line, c.Col = pos.Line, pos.Column
					if o := (out{fn.ID, c.Callee}); !seenOut[o] {
						seenOut[o] = true
						idx.Calls[fn.ID] = append(idx.Calls[fn.ID], c.Callee)
					}
					if c.Kind != KindStatic {
						return true
					}
					e := edge{c.ID, Call{
						Caller:     fn.ID,
						CallerName: fn.Name,
						File:       rel,
						Line:       pos.Line,
						Col:        pos.Column,
					}}
					if !seenEdge[e] {
						seenEdge[e] = true
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
	for id, calls := range idx.Calls {
		// Static calls into code outside the index (stdlib, dependencies) can't be expanded.
		calls = slices.DeleteFunc(calls, func(c Callee) bool { return c.Kind == KindStatic && idx.Funcs[c.ID] == nil })
		slices.SortFunc(calls, func(a, b Callee) int { return cmp.Or(cmp.Compare(a.Line, b.Line), cmp.Compare(a.Col, b.Col)) })
		idx.Calls[id] = calls
	}
	return idx, nil
}

type classified struct {
	Callee
	pos token.Pos
}

// classify describes a call expression. It reports false for conversions,
// builtins, immediately called func literals, and interface methods declared
// outside the loaded packages (e.g. ctx.Done(), err.Error()).
func classify(info *types.Info, call *ast.CallExpr, loaded map[string]bool) (classified, bool) {
	tv := info.Types[call.Fun]
	if tv.IsType() || tv.IsBuiltin() || tv.Type == nil {
		return classified{}, false
	}
	if _, ok := ast.Unparen(call.Fun).(*ast.FuncLit); ok {
		return classified{}, false
	}
	if fn, id := calleeOf(info, call); fn != nil {
		if fn.Pkg() == nil { // e.g. error.Error
			return classified{}, false
		}
		kind := KindStatic
		if recv := fn.Signature().Recv(); recv != nil && types.IsInterface(recv.Type()) {
			if !loaded[fn.Pkg().Path()] {
				return classified{}, false
			}
			kind = KindInterface
		}
		return classified{Callee{ID: fn.FullName(), Name: displayName(fn), Kind: kind}, id.Pos()}, true
	}
	if _, ok := tv.Type.Underlying().(*types.Signature); ok {
		return classified{Callee{Name: types.ExprString(call.Fun), Kind: KindDynamic}, call.Lparen}, true
	}
	return classified{}, false
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
