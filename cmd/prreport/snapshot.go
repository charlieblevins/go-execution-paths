package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"go/ast"
	"go/printer"
	"go/token"
	"go/types"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"slices"
	"strings"

	"github.com/charlieblevins/go-execution-paths/internal/goload"
	"golang.org/x/mod/modfile"
)

// Snapshot is what the report compares from one version of the code.
type Snapshot struct {
	SHA      string
	Funcs    map[string]*FuncInfo   // types.Func.FullName() -> info
	Packages map[string]*PkgInfo    // import path -> info
	Structs  map[string]*StructInfo // type ID -> JSON-serialized struct
	Targets  map[string]string      // state target ID -> display name
	Specs    map[string]string      // API spec file (e.g. api/openapi.yaml) -> hash
	Mod      *modfile.File          // nil if the module has no go.mod
	Errors   []string
}

// StructInfo is a struct type with at least one JSON-tagged field: part of
// the module's serialized wire format.
type StructInfo struct {
	ID     string // import path + "." + type name
	Name   string // display name, e.g. "defs.APIStaticSource"
	File   string
	Line   int
	Fields map[string]*FieldInfo // by Go field name
}

type FieldInfo struct {
	Name     string // Go field name
	JSON     string // name in JSON
	Opts     string // json tag options, e.g. "omitempty"
	Type     string // e.g. "*float64"
	Line     int
	Exported bool
}

// Desc describes a field as "jsonName type [, opts]".
func (f *FieldInfo) Desc() string {
	s := f.JSON + " " + f.Type
	if f.Opts != "" {
		s += ", " + f.Opts
	}
	return s
}

type PkgInfo struct {
	Path  string
	Name  string
	Dir   string   // relative to the module root
	Files []string // relative to the module root, sorted
	Hash  string   // of all file contents
}

type FuncInfo struct {
	ID       string
	Name     string // display name, e.g. "pkg.(*T).M"
	File     string // relative to the module root
	Start    int
	End      int
	Public   bool
	Sig      string // signature, for spotting signature changes
	Hash     string // declaration without comments, for spotting modifications
	BodyHash string // body only, for spotting renames and moves
	Access   map[Access]int
	Params   []string // type IDs named in the parameters
	Results  []string // type IDs named in the results
	Builds   []string // type IDs of composite literals in the body
}

// Access is a read or write of package state: a package-level variable or a
// struct field.
type Access struct {
	Target string // e.g. "github.com/x/pkg.T.field" or "github.com/x/pkg.varName"
	Write  bool
}

func takeSnapshot(root, sha string, patterns []string, tests bool) (*Snapshot, error) {
	pkgs, err := goload.Load(root, patterns, tests)
	if err != nil {
		return nil, err
	}
	s := &Snapshot{
		SHA:      sha,
		Funcs:    map[string]*FuncInfo{},
		Packages: map[string]*PkgInfo{},
		Structs:  map[string]*StructInfo{},
		Targets:  map[string]string{},
	}
	specs, err := findSpecs(root)
	if err != nil {
		return nil, err
	}
	s.Specs = specs
	if data, err := os.ReadFile(filepath.Join(root, "go.mod")); err == nil {
		if s.Mod, err = modfile.Parse("go.mod", data, nil); err != nil {
			return nil, err
		}
	}

	for _, p := range pkgs {
		for _, e := range p.Errors {
			s.Errors = append(s.Errors, e.Error())
		}
		s.addPackage(root, p.PkgPath, p.Name, p.GoFiles)
		s.addStructs(root, p.Fset, p.Types)
		for _, f := range p.Syntax {
			rel, ok := goload.RelPath(root, p.Fset.File(f.Pos()).Name())
			if !ok {
				continue
			}
			for _, decl := range f.Decls {
				d, ok := decl.(*ast.FuncDecl)
				if !ok || d.Body == nil {
					continue
				}
				obj, _ := p.TypesInfo.Defs[d.Name].(*types.Func)
				if obj == nil {
					continue
				}
				if _, dup := s.Funcs[obj.FullName()]; dup {
					continue // same file in a test variant of the package
				}
				params, results := sigRefs(obj.Signature())
				s.Funcs[obj.FullName()] = &FuncInfo{
					ID:       obj.FullName(),
					Name:     goload.DisplayName(obj),
					File:     rel,
					Start:    p.Fset.Position(d.Pos()).Line,
					End:      p.Fset.Position(d.End()).Line,
					Public:   isPublic(obj),
					Sig:      types.TypeString(obj.Type(), (*types.Package).Name),
					Hash:     hashNode(p.Fset, withoutDoc(d)),
					BodyHash: hashNode(p.Fset, d.Body),
					Access:   s.accesses(p.TypesInfo, d.Body),
					Params:   params,
					Results:  results,
					Builds:   builds(p.TypesInfo, d.Body),
				}
			}
		}
	}
	for _, pkg := range s.Packages {
		slices.Sort(pkg.Files)
		h := sha256.New()
		for _, f := range pkg.Files {
			data, err := os.ReadFile(filepath.Join(root, f))
			if err != nil {
				return nil, err
			}
			h.Write([]byte(f))
			h.Write(data)
		}
		pkg.Hash = hex.EncodeToString(h.Sum(nil))
	}
	return s, nil
}

// addPackage records a package's files. Test variants of a package share its
// import path, so their files are merged in.
func (s *Snapshot) addPackage(root, pkgPath, name string, files []string) {
	pkg := s.Packages[pkgPath]
	for _, f := range files {
		rel, ok := goload.RelPath(root, f)
		if !ok {
			continue
		}
		if pkg == nil {
			pkg = &PkgInfo{Path: pkgPath, Name: name, Dir: path.Dir(rel)}
			s.Packages[pkgPath] = pkg
		}
		if !slices.Contains(pkg.Files, rel) {
			pkg.Files = append(pkg.Files, rel)
		}
	}
}

// addStructs records package-level struct types that have at least one
// JSON-tagged field, i.e. types that are part of a serialized wire format.
func (s *Snapshot) addStructs(root string, fset *token.FileSet, pkg *types.Package) {
	scope := pkg.Scope()
	for _, name := range scope.Names() {
		obj, ok := scope.Lookup(name).(*types.TypeName)
		if !ok || obj.IsAlias() {
			continue
		}
		named, ok := obj.Type().(*types.Named)
		if !ok {
			continue
		}
		st, ok := named.Underlying().(*types.Struct)
		if !ok {
			continue
		}
		id := pkg.Path() + "." + name
		if _, dup := s.Structs[id]; dup {
			continue
		}
		pos := fset.Position(obj.Pos())
		file, ok := goload.RelPath(root, pos.Filename)
		if !ok {
			continue
		}
		info := &StructInfo{ID: id, Name: pkg.Name() + "." + name, File: file, Line: pos.Line, Fields: map[string]*FieldInfo{}}
		for i := range st.NumFields() {
			f := st.Field(i)
			tag, ok := reflect.StructTag(st.Tag(i)).Lookup("json")
			if !ok {
				continue
			}
			jsonName, opts, _ := strings.Cut(tag, ",")
			if jsonName == "-" && opts == "" {
				continue // explicitly not serialized
			}
			if jsonName == "" {
				jsonName = f.Name()
			}
			info.Fields[f.Name()] = &FieldInfo{
				Name:     f.Name(),
				JSON:     jsonName,
				Opts:     opts,
				Type:     types.TypeString(f.Type(), (*types.Package).Name),
				Line:     fset.Position(f.Pos()).Line,
				Exported: f.Exported(),
			}
		}
		if len(info.Fields) > 0 {
			s.Structs[id] = info
		}
	}
}

// builds returns the IDs of named types constructed as composite literals in
// a function body, which connects a serialized type to the code that fills it
// in even when it only reaches the API through an interface field.
func builds(info *types.Info, body *ast.BlockStmt) []string {
	var out []string
	ast.Inspect(body, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		for _, id := range namedTypes(info.Types[lit].Type) {
			if !slices.Contains(out, id) {
				out = append(out, id)
			}
		}
		return true
	})
	return out
}

// sigRefs returns the IDs of named types mentioned in a signature's
// parameters and results.
func sigRefs(sig *types.Signature) (params, results []string) {
	collect := func(t *types.Tuple) []string {
		var out []string
		for i := range t.Len() {
			for _, id := range namedTypes(t.At(i).Type()) {
				if !slices.Contains(out, id) {
					out = append(out, id)
				}
			}
		}
		return out
	}
	return collect(sig.Params()), collect(sig.Results())
}

// namedTypes returns the IDs of named types reachable from t through
// pointers, slices, arrays, maps and channels.
func namedTypes(t types.Type) []string {
	switch t := t.(type) {
	case nil:
		return nil
	case *types.Named:
		obj := t.Obj()
		if obj.Pkg() == nil {
			return nil
		}
		return []string{obj.Pkg().Path() + "." + obj.Name()}
	case *types.Pointer:
		return namedTypes(t.Elem())
	case *types.Slice:
		return namedTypes(t.Elem())
	case *types.Array:
		return namedTypes(t.Elem())
	case *types.Chan:
		return namedTypes(t.Elem())
	case *types.Map:
		return append(namedTypes(t.Key()), namedTypes(t.Elem())...)
	}
	return nil
}

var specNames = map[string]bool{
	"openapi.yaml": true, "openapi.yml": true, "openapi.json": true,
	"swagger.yaml": true, "swagger.yml": true, "swagger.json": true,
}

// findSpecs hashes API description files (OpenAPI/Swagger) in the module, so
// the report can tell whether they were updated alongside serialized types.
func findSpecs(root string) (map[string]string, error) {
	specs := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "vendor", "node_modules", "testdata":
				return fs.SkipDir
			}
			return nil
		}
		if !specNames[strings.ToLower(d.Name())] {
			return nil
		}
		rel, ok := goload.RelPath(root, path)
		if !ok {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		specs[rel] = hex.EncodeToString(sum[:8])
		return nil
	})
	return specs, err
}

// isPublic reports whether a function is part of its package's exported API:
// an exported name, and for methods an exported receiver type.
func isPublic(fn *types.Func) bool {
	if !fn.Exported() {
		return false
	}
	recv := fn.Signature().Recv()
	if recv == nil {
		return true
	}
	t := recv.Type()
	if p, ok := t.(*types.Pointer); ok {
		t = p.Elem()
	}
	named, ok := t.(*types.Named)
	return !ok || named.Obj().Exported()
}

func withoutDoc(d *ast.FuncDecl) *ast.FuncDecl {
	c := *d
	c.Doc = nil
	return &c
}

// hashNode hashes a node's formatted source. Printing a node other than a
// file drops comments, and formatting ignores where the code sits in the file.
func hashNode(fset *token.FileSet, n ast.Node) string {
	var buf bytes.Buffer
	printer.Fprint(&buf, fset, n)
	sum := sha256.Sum256(buf.Bytes())
	return hex.EncodeToString(sum[:8])
}

// accesses counts reads and writes of package-level variables and struct
// fields in body, including inside closures. Assignments, ++/--, range
// assignments, and taking an address (&x.f) count as writes; writing an
// element (x.f[i] = v, *x.f = v) counts as a write of the field.
func (s *Snapshot) accesses(info *types.Info, body *ast.BlockStmt) map[Access]int {
	writes := map[ast.Node]bool{}
	markWrite := func(e ast.Expr) {
		if n := s.writeBase(info, e); n != nil {
			writes[n] = true
		}
	}
	ast.Inspect(body, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.AssignStmt:
			for _, lhs := range n.Lhs {
				markWrite(lhs)
			}
		case *ast.IncDecStmt:
			markWrite(n.X)
		case *ast.UnaryExpr:
			if n.Op == token.AND {
				markWrite(n.X)
			}
		case *ast.RangeStmt:
			if n.Tok == token.ASSIGN {
				if n.Key != nil {
					markWrite(n.Key)
				}
				if n.Value != nil {
					markWrite(n.Value)
				}
			}
		}
		return true
	})

	acc := map[Access]int{}
	ast.Inspect(body, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.SelectorExpr:
			if id := s.fieldTarget(info, n); id != "" {
				acc[Access{id, writes[n]}]++
			}
		case *ast.Ident:
			if id := s.varTarget(info, n); id != "" {
				acc[Access{id, writes[n]}]++
			}
		}
		return true
	})
	return acc
}

// writeBase returns the field selector or variable identifier that writing
// to e modifies, or nil if it's local state.
func (s *Snapshot) writeBase(info *types.Info, e ast.Expr) ast.Node {
	for {
		switch x := e.(type) {
		case *ast.ParenExpr:
			e = x.X
		case *ast.IndexExpr:
			e = x.X
		case *ast.StarExpr:
			e = x.X
		case *ast.SelectorExpr:
			if sel := info.Selections[x]; sel != nil {
				if sel.Kind() == types.FieldVal {
					return x
				}
				return nil
			}
			return x.Sel // qualified identifier: pkg.Var
		case *ast.Ident:
			return x
		default:
			return nil
		}
	}
}

// fieldTarget returns the state target ID for a struct field selection, or "".
func (s *Snapshot) fieldTarget(info *types.Info, sel *ast.SelectorExpr) string {
	selection := info.Selections[sel]
	if selection == nil || selection.Kind() != types.FieldVal {
		return ""
	}
	field := selection.Obj().(*types.Var)
	// For promoted fields, find the embedded struct that declares the field.
	owner := selection.Recv()
	idx := selection.Index()
	for _, i := range idx[:len(idx)-1] {
		st, ok := deref(owner).Underlying().(*types.Struct)
		if !ok {
			return ""
		}
		owner = st.Field(i).Type()
	}
	named, ok := deref(owner).(*types.Named)
	if !ok || field.Pkg() == nil {
		return "" // fields of anonymous structs
	}
	obj := named.Obj()
	id := obj.Pkg().Path() + "." + obj.Name() + "." + field.Name()
	s.Targets[id] = obj.Pkg().Name() + "." + obj.Name() + "." + field.Name()
	return id
}

// varTarget returns the state target ID for a package-level variable, or "".
func (s *Snapshot) varTarget(info *types.Info, ident *ast.Ident) string {
	v, ok := info.Uses[ident].(*types.Var)
	if !ok || v.IsField() || v.Pkg() == nil || v.Parent() != v.Pkg().Scope() {
		return ""
	}
	id := v.Pkg().Path() + "." + v.Name()
	s.Targets[id] = v.Pkg().Name() + "." + v.Name()
	return id
}

func deref(t types.Type) types.Type {
	if p, ok := t.Underlying().(*types.Pointer); ok {
		return p.Elem()
	}
	return t
}
