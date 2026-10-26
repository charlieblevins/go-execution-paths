// Package paths enumerates execution paths through a Go function's
// control-flow graph (go/cfg) and works out which source lines each path runs.
package paths

import (
	"go/ast"
	"go/token"
	"go/types"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"

	"golang.org/x/tools/go/cfg"
)

// maxWork bounds the blocks visited in one enumeration, so functions with
// astronomically many paths can't hang the caller.
const maxWork = 2_000_000

// Func is a function declaration plus the type information for its package.
// It is safe for concurrent use.
type Func struct {
	Fset *token.FileSet
	File *ast.File
	Decl *ast.FuncDecl
	Info *types.Info

	once   sync.Once
	g      *cfg.CFG
	src    []byte
	srcErr error
}

// A Decision is the outcome of one branch along a path.
type Decision struct {
	Line  int    `json:"line"`
	Label string `json:"label"` // e.g. "if err != nil"
	Taken string `json:"taken"` // e.g. "true", "match", "body runs"
	Key   bool   `json:"key"`   // false for "no match" / "not chosen", which are implied by the taken case
}

// A Path is one route through the function.
type Path struct {
	Visible   []int      `json:"visible"` // 1-based file lines that are part of the path, sorted
	Decisions []Decision `json:"decisions"`
	EndLine   int        `json:"endLine"`
	End       string     `json:"end"` // "returns", "never returns", "loops back", or "reaches line"
}

func (f *Func) init() {
	f.once.Do(func() {
		f.g = cfg.New(f.Decl.Body, f.mayReturn)
		f.src, f.srcErr = os.ReadFile(f.Fset.File(f.Decl.Pos()).Name())
	})
}

func (f *Func) line(p token.Pos) int { return f.Fset.Position(p).Line }

// Paths returns up to max paths through the function. If through > 0, only
// paths that reach that line are returned, each ending there.
func (f *Func) Paths(max, through int) (paths []Path, truncated bool, err error) {
	f.init()
	if f.srcErr != nil {
		return nil, false, f.srcErr
	}
	paths = []Path{}
	truncated = f.enumerate(max, through, func(blocks []*cfg.Block, steps []step, loopsBack *cfg.Block) {
		paths = append(paths, f.path(blocks, steps, loopsBack, through))
	})
	return paths, truncated, nil
}

// Count returns the number of paths, stopping at max.
func (f *Func) Count(max, through int) (n int, truncated bool) {
	f.init()
	truncated = f.enumerate(max, through, func([]*cfg.Block, []step, *cfg.Block) { n++ })
	return n, truncated
}

// mayReturn reports false for calls known never to return, so paths end there.
func (f *Func) mayReturn(call *ast.CallExpr) bool {
	var id *ast.Ident
	switch fn := ast.Unparen(call.Fun).(type) {
	case *ast.Ident:
		id = fn
	case *ast.SelectorExpr:
		id = fn.Sel
	default:
		return true
	}
	switch obj := f.Info.Uses[id].(type) {
	case *types.Builtin:
		return obj.Name() != "panic"
	case *types.Func:
		switch obj.FullName() {
		case "os.Exit", "log.Fatal", "log.Fatalf", "log.Fatalln", "log.Panic", "log.Panicf", "log.Panicln":
			return false
		}
	}
	return true
}

// A step is a decision taken at a block with two successors.
type step struct {
	block *cfg.Block
	succ  int
}

// enumerate calls visit for each path from the entry block to a block with no
// successors (a return, or a call that never returns). Loops run their body
// at most once. On getting back to the head of a for-cond or range loop, the
// walk leaves the loop through the head's other successor; on getting back to
// the top of any other loop (for {}, goto), the path ends there and visit is
// called with loopsBack set to that block.
//
// If through > 0, only paths reaching a block with code on that line are
// visited, and they end at that block.
func (f *Func) enumerate(max, through int, visit func(blocks []*cfg.Block, steps []step, loopsBack *cfg.Block)) (truncated bool) {
	g := f.g
	var isTarget, canReach []bool
	if through > 0 {
		isTarget = make([]bool, len(g.Blocks))
		for _, b := range g.Blocks {
			for _, n := range b.Nodes {
				if f.line(n.Pos()) <= through && through <= f.line(n.End()) {
					isTarget[b.Index] = true
				}
			}
		}
		canReach = reaching(g, isTarget)
	}
	ok := func(b *cfg.Block) bool { return canReach == nil || canReach[b.Index] }

	n, work := 0, 0
	onPath := make([]bool, len(g.Blocks))
	var blocks []*cfg.Block
	var steps []step

	var walk func(b *cfg.Block)
	walk = func(b *cfg.Block) {
		work++
		if n >= max || work > maxWork {
			truncated = true
			return
		}
		onPath[b.Index] = true
		blocks = append(blocks, b)
		defer func() {
			onPath[b.Index] = false
			blocks = blocks[:len(blocks)-1]
		}()
		if isTarget != nil && isTarget[b.Index] {
			n++
			visit(blocks, steps, nil)
			return
		}
		if len(b.Succs) == 0 {
			if isTarget == nil {
				n++
				visit(blocks, steps, nil)
			}
			return
		}
		for i, s := range b.Succs {
			if len(b.Succs) > 1 {
				steps = append(steps, step{b, i})
			}
			switch {
			case !onPath[s.Index]:
				if ok(s) {
					walk(s)
				}
			case s.Kind == cfg.KindForLoop || s.Kind == cfg.KindRangeLoop:
				for j, t := range s.Succs {
					if !onPath[t.Index] && ok(t) {
						steps = append(steps, step{s, j})
						walk(t)
						steps = steps[:len(steps)-1]
					}
				}
			case isTarget != nil:
				// loops back without reaching the target line
			case n < max:
				n++
				visit(blocks, steps, s)
			default:
				truncated = true
			}
			if len(b.Succs) > 1 {
				steps = steps[:len(steps)-1]
			}
		}
	}
	walk(g.Blocks[0])
	return truncated
}

// reaching reports, for each block, whether a target block is reachable from it.
func reaching(g *cfg.CFG, isTarget []bool) []bool {
	preds := make([][]*cfg.Block, len(g.Blocks))
	for _, b := range g.Blocks {
		for _, s := range b.Succs {
			preds[s.Index] = append(preds[s.Index], b)
		}
	}
	can := slices.Clone(isTarget)
	var queue []*cfg.Block
	for _, b := range g.Blocks {
		if can[b.Index] {
			queue = append(queue, b)
		}
	}
	for len(queue) > 0 {
		b := queue[0]
		queue = queue[1:]
		for _, p := range preds[b.Index] {
			if !can[p.Index] {
				can[p.Index] = true
				queue = append(queue, p)
			}
		}
	}
	return can
}

func (f *Func) path(blocks []*cfg.Block, steps []step, loopsBack *cfg.Block, through int) Path {
	vis := map[int]bool{}
	for _, b := range blocks {
		for _, n := range b.Nodes {
			for l := f.line(n.Pos()); l <= f.line(n.End()); l++ {
				vis[l] = true
			}
		}
	}
	f.addStructure(vis, blocks)

	p := Path{Decisions: []Decision{}}
	for l := range vis {
		p.Visible = append(p.Visible, l)
	}
	slices.Sort(p.Visible)
	for _, s := range steps {
		p.Decisions = append(p.Decisions, f.describe(s))
	}

	last := blocks[len(blocks)-1]
	p.EndLine, p.End = f.line(f.Decl.Body.Rbrace), "returns"
	switch {
	case through > 0:
		p.EndLine, p.End = through, "reaches line"
	case loopsBack != nil:
		p.End = "loops back"
		if loopsBack.Stmt != nil {
			p.EndLine = f.line(loopsBack.Stmt.Pos())
		}
	case len(last.Nodes) > 0:
		node := last.Nodes[len(last.Nodes)-1]
		p.EndLine = f.line(node.Pos())
		if _, ok := node.(*ast.ReturnStmt); !ok {
			p.End = "never returns"
		}
	}
	return p
}

// addStructure makes the code around visible lines readable: the signature,
// statements go/cfg leaves out of entered blocks, braces of blocks that are
// entered or whose header is shown, case labels of taken cases, labels, and
// comments directly above visible lines.
func (f *Func) addStructure(vis map[int]bool, blocks []*cfg.Block) {
	markNode := func(n ast.Node) {
		for l := f.line(n.Pos()); l <= f.line(n.End()); l++ {
			vis[l] = true
		}
	}
	for l := f.line(f.Decl.Pos()); l <= f.line(f.Decl.Body.Lbrace); l++ {
		vis[l] = true
	}
	vis[f.line(f.Decl.Body.Rbrace)] = true

	// go/cfg has no nodes for type declarations or branch statements (break,
	// continue, goto, fallthrough). Show them in statement lists the path
	// enters: type declarations always, branches when they come first or
	// follow a visible statement (so they were reached).
	enter := func(list []ast.Stmt) {
		for i, s := range list {
			switch s := s.(type) {
			case *ast.DeclStmt:
				if s.Decl.(*ast.GenDecl).Tok == token.TYPE {
					markNode(s)
				}
			case *ast.BranchStmt:
				if i == 0 || vis[f.line(list[i-1].End())] {
					markNode(s)
				}
			}
		}
	}
	for _, b := range blocks {
		switch s := b.Stmt.(type) {
		case *ast.BlockStmt: // function body
			enter(s.List)
		case *ast.IfStmt:
			if b.Kind == cfg.KindIfThen {
				enter(s.Body.List)
			} else if els, ok := s.Else.(*ast.BlockStmt); ok && b.Kind == cfg.KindIfElse {
				enter(els.List)
			}
		case *ast.ForStmt:
			if b.Kind == cfg.KindForBody {
				enter(s.Body.List)
			}
		case *ast.RangeStmt:
			if b.Kind == cfg.KindRangeBody {
				enter(s.Body.List)
			}
		case *ast.CaseClause:
			if b.Kind == cfg.KindSwitchCaseBody {
				vis[f.line(s.Case)] = true
				enter(s.Body)
			}
		case *ast.CommClause:
			if b.Kind == cfg.KindSelectCaseBody {
				vis[f.line(s.Case)] = true
				enter(s.Body)
			}
		}
	}

	anyVisible := func(from, to int) bool {
		for l := from; l <= to; l++ {
			if vis[l] {
				return true
			}
		}
		return false
	}
	clause := func(caseLine int, body []ast.Stmt, end token.Pos) {
		if len(body) > 0 && anyVisible(f.line(body[0].Pos()), f.line(end)) {
			vis[caseLine] = true
		}
	}
	// Two passes: a label precedes its loop, whose line may only be revealed
	// by the loop body's braces.
	for range 2 {
		ast.Inspect(f.Decl.Body, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.FuncLit:
				return false // closures are inside statement nodes and shown whole
			case *ast.BlockStmt:
				lb, rb := f.line(n.Lbrace), f.line(n.Rbrace)
				if anyVisible(lb+1, rb-1) {
					vis[lb] = true
				}
				if vis[lb] {
					vis[rb] = true
				}
			case *ast.CaseClause:
				clause(f.line(n.Case), n.Body, n.End())
			case *ast.CommClause:
				clause(f.line(n.Case), n.Body, n.End())
			case *ast.LabeledStmt:
				if vis[f.line(n.Stmt.Pos())] {
					vis[f.line(n.Pos())] = true
				}
			}
			return true
		})
	}
	start, end := f.line(f.Decl.Pos()), f.line(f.Decl.End())
	for _, cg := range f.File.Comments {
		from, to := f.line(cg.Pos()), f.line(cg.End())
		if from > start && to < end && vis[to+1] {
			for l := from; l <= to; l++ {
				vis[l] = true
			}
		}
	}
}

func (f *Func) describe(s step) Decision {
	b, i := s.block, s.succ
	pick := func(yes, no string) string { return []string{yes, no}[i] }
	t := b.Succs[0]
	switch t.Kind {
	case cfg.KindIfThen:
		cond := b.Nodes[len(b.Nodes)-1]
		return Decision{f.line(cond.Pos()), "if " + f.text(cond), pick("true", "false"), true}
	case cfg.KindForBody:
		fs := t.Stmt.(*ast.ForStmt)
		return Decision{f.line(fs.For), "for " + f.text(fs.Cond), pick("body runs", "loop ends"), true}
	case cfg.KindRangeBody:
		rs := t.Stmt.(*ast.RangeStmt)
		return Decision{f.line(rs.For), "range " + f.text(rs.X), pick("body runs", "loop ends"), true}
	case cfg.KindSwitchCaseBody:
		cc := t.Stmt.(*ast.CaseClause)
		var parts []string
		for _, e := range cc.List {
			parts = append(parts, f.text(e))
		}
		return Decision{f.line(cc.Case), "case " + strings.Join(parts, ", "), pick("match", "no match"), i == 0}
	case cfg.KindSelectCaseBody:
		cc := t.Stmt.(*ast.CommClause)
		return Decision{f.line(cc.Case), "select " + f.text(cc.Comm), pick("chosen", "not chosen"), i == 0}
	}
	line := f.line(f.Decl.Pos())
	if len(b.Nodes) > 0 {
		line = f.line(b.Nodes[len(b.Nodes)-1].Pos())
	}
	return Decision{line, "branch", pick("true", "false"), true}
}

var spaces = regexp.MustCompile(`\s+`)

func (f *Func) text(n ast.Node) string {
	if n == nil {
		return ""
	}
	from, to := f.Fset.Position(n.Pos()).Offset, f.Fset.Position(n.End()).Offset
	s := []rune(spaces.ReplaceAllString(string(f.src[from:to]), " "))
	if len(s) > 60 {
		return string(s[:57]) + "…"
	}
	return string(s)
}
