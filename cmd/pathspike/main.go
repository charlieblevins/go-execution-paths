// Command pathspike is a prototype for viewing individual execution paths
// through a Go function. It exports static HTML pages where selecting a path
// shows only the code on it.
//
//	pathspike -dir ~/projects/mediamtx -survey ./...        # path counts for every function (TSV)
//	pathspike -dir ~/projects/mediamtx -out _paths -func 'core.(*path).initialize' ./internal/core
package main

import (
	_ "embed"
	"flag"
	"fmt"
	"go/ast"
	"go/types"
	"html/template"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/charlieblevins/go-execution-paths/internal/paths"
	"golang.org/x/tools/go/packages"
)

//go:embed page.html
var pageHTML string

func main() {
	dir := flag.String("dir", ".", "Go module directory")
	out := flag.String("out", "_paths", "output directory for HTML")
	survey := flag.Bool("survey", false, "print path counts for every function as TSV instead of exporting HTML")
	maxPaths := flag.Int("max", 200, "stop enumerating a function's paths after this many")
	var funcs []string
	flag.Func("func", "function to export, e.g. 'core.(*path).initialize' (repeatable)", func(s string) error {
		funcs = append(funcs, s)
		return nil
	})
	flag.Parse()
	patterns := flag.Args()
	if len(patterns) == 0 {
		patterns = []string{"./..."}
	}

	root, err := filepath.Abs(*dir)
	if err != nil {
		log.Fatal(err)
	}
	pkgs, err := packages.Load(&packages.Config{
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedSyntax | packages.NeedTypes | packages.NeedTypesInfo,
		Dir:  root,
	}, patterns...)
	if err != nil {
		log.Fatal(err)
	}

	var decls []*decl
	for _, p := range pkgs {
		for _, f := range p.Syntax {
			for _, d := range f.Decls {
				fd, ok := d.(*ast.FuncDecl)
				if !ok || fd.Body == nil {
					continue
				}
				obj, _ := p.TypesInfo.Defs[fd.Name].(*types.Func)
				if obj == nil {
					continue
				}
				pf := &paths.Func{Fset: p.Fset, File: f, Decl: fd, Info: p.TypesInfo}
				decls = append(decls, &decl{Func: pf, obj: obj, root: root})
			}
		}
	}

	if *survey {
		fmt.Println("paths\tlines\treturns\tif\tswitch\tselect\tloop\tfunc\tfile")
		for _, d := range decls {
			n, trunc := d.Count(10000, 0)
			s := d.stats()
			plus := ""
			if trunc {
				plus = "+"
			}
			fmt.Printf("%d%s\t%d\t%d\t%d\t%d\t%d\t%d\t%s\t%s\n", n, plus, s.lines, s.returns, s.ifs, s.switches, s.selects, s.loops, displayName(d.obj), d.relPath())
		}
		return
	}

	if len(funcs) == 0 {
		log.Fatal("no functions given; use -func (or -survey to find candidates)")
	}
	tmpl := template.Must(template.New("page").Parse(pageHTML))
	css, err := chromaCSS()
	if err != nil {
		log.Fatal(err)
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		log.Fatal(err)
	}

	type indexEntry struct {
		Href, Name, File string
		Paths            int
		Truncated        bool
	}
	var index []indexEntry
	for i, want := range funcs {
		d := findDecl(decls, want)
		if d == nil {
			log.Fatalf("no function matching %q", want)
		}
		page, err := d.export(*maxPaths)
		if err != nil {
			log.Fatal(err)
		}
		href := fmt.Sprintf("%d.html", i+1)
		if err := writeTemplate(tmpl, filepath.Join(*out, href), map[string]any{"CSS": css, "Func": page}); err != nil {
			log.Fatal(err)
		}
		index = append(index, indexEntry{href, page.Name, page.File, len(page.Paths), page.Truncated})
		log.Printf("%s: %d paths%s", page.Name, len(page.Paths), map[bool]string{true: " (truncated)"}[page.Truncated])
	}
	if err := writeTemplate(tmpl, filepath.Join(*out, "index.html"), map[string]any{"CSS": css, "Index": index}); err != nil {
		log.Fatal(err)
	}
	log.Printf("wrote %s", filepath.Join(*out, "index.html"))
}

// findDecl matches a short name like "core.(*path).run" or a full name.
func findDecl(decls []*decl, want string) *decl {
	for _, d := range decls {
		if displayName(d.obj) == want || d.obj.FullName() == want {
			return d
		}
	}
	return nil
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

func writeTemplate(t *template.Template, path string, data any) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return t.Execute(f, data)
}

type decl struct {
	*paths.Func
	obj  *types.Func
	root string
}

func (d *decl) line(n ast.Node) (start, end int) {
	return d.Fset.Position(n.Pos()).Line, d.Fset.Position(n.End()).Line
}

func (d *decl) relPath() string {
	abs := d.Fset.File(d.Decl.Pos()).Name()
	if rel, err := filepath.Rel(d.root, abs); err == nil {
		return rel
	}
	return abs
}

type stats struct{ lines, returns, ifs, switches, selects, loops int }

func (d *decl) stats() stats {
	start, end := d.line(d.Decl)
	s := stats{lines: end - start + 1}
	ast.Inspect(d.Decl.Body, func(n ast.Node) bool {
		switch n.(type) {
		case *ast.FuncLit:
			return false
		case *ast.ReturnStmt:
			s.returns++
		case *ast.IfStmt:
			s.ifs++
		case *ast.SwitchStmt, *ast.TypeSwitchStmt:
			s.switches++
		case *ast.SelectStmt:
			s.selects++
		case *ast.ForStmt, *ast.RangeStmt:
			s.loops++
		}
		return true
	})
	return s
}

type FuncPage struct {
	Name      string       `json:"name"`
	File      string       `json:"file"`
	Start     int          `json:"start"`
	Lines     []string     `json:"lines"` // highlighted HTML, Start..end of func
	Paths     []paths.Path `json:"paths"`
	Truncated bool         `json:"truncated"`
}

func (d *decl) export(max int) (*FuncPage, error) {
	src, err := os.ReadFile(d.Fset.File(d.Decl.Pos()).Name())
	if err != nil {
		return nil, err
	}
	lines, err := highlightLines(src)
	if err != nil {
		return nil, err
	}
	list, truncated, err := d.Paths(max, 0)
	if err != nil {
		return nil, err
	}
	start, end := d.line(d.Decl)
	return &FuncPage{
		Name:      displayName(d.obj),
		File:      d.relPath(),
		Start:     start,
		Lines:     lines[start-1 : end],
		Paths:     list,
		Truncated: truncated,
	}, nil
}
