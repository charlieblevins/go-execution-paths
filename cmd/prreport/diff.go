package main

import (
	"cmp"
	"fmt"
	"path"
	"slices"
	"strings"

	"golang.org/x/mod/modfile"
	"golang.org/x/mod/semver"
)

// Report is the data behind the HTML page.
type Report struct {
	Title    string
	Base     Rev
	Head     Rev
	Sections []Section
	Errors   []string
}

type Rev struct {
	Ref, SHA, URL string
}

type Section struct {
	ID, Title, Note string
	Groups          []Group
}

func (s Section) Total() int {
	n := 0
	for _, g := range s.Groups {
		n += len(g.Items)
	}
	return n
}

// Cell is the summary table cell for a group: its count, or "" if the
// section has no such group.
func (s Section) Cell(label string) string {
	for _, g := range s.Groups {
		if g.Label == label {
			return fmt.Sprint(len(g.Items))
		}
	}
	return ""
}

// Labels lists group labels in display order, for the summary table.
func (Report) Labels() []string { return GroupLabels }

// Total is the number of items across all sections.
func (r Report) Total() int {
	n := 0
	for _, s := range r.Sections {
		n += s.Total()
	}
	return n
}

// Count returns the number of items in the group with the given label.
func (s Section) Count(label string) int {
	for _, g := range s.Groups {
		if g.Label == label {
			return len(g.Items)
		}
	}
	return 0
}

type Group struct {
	Label string // "Added", "Removed", "Modified", "Renamed or moved"
	Items []Item
}

type Item struct {
	Name   string
	Detail string
	Links  []Link
}

type Link struct {
	Label, URL string
}

const (
	added    = "Added"
	removed  = "Removed"
	modified = "Modified"
	moved    = "Renamed or moved"
)

var GroupLabels = []string{added, removed, modified, moved}

// differ compares two snapshots. dir is the module's directory within the
// repository, for building links; gh is the GitHub repository URL, or "".
type differ struct {
	base, head *Snapshot
	dir, gh    string
	moves      map[string]string // head func ID -> base func ID for renamed/moved functions
}

func (d *differ) report() []Section {
	pub, priv := d.funcs()
	reads, writes := d.state()
	return []Section{
		d.jsonTypes(),
		pub, priv,
		reads, writes,
		d.packages(),
		d.deps(),
	}
}

// fileURL links to lines of a file at a revision.
func (d *differ) fileURL(sha, file string, start, end int) string {
	if d.gh == "" {
		return ""
	}
	u := fmt.Sprintf("%s/blob/%s/%s", d.gh, sha, path.Join(d.dir, file))
	if start > 0 {
		u += fmt.Sprintf("#L%d", start)
		if end > start {
			u += fmt.Sprintf("-L%d", end)
		}
	}
	return u
}

func (d *differ) funcLink(label string, s *Snapshot, f *FuncInfo) Link {
	return Link{fmt.Sprintf("%s %s:%d", label, f.File, f.Start), d.fileURL(s.SHA, f.File, f.Start, f.End)}
}

// funcs compares functions, split into public and private by the head
// version (base version for removed functions).
func (d *differ) funcs() (pub, priv Section) {
	pub = Section{ID: "public", Title: "Public functions", Note: "Exported functions, and exported methods on exported types."}
	priv = Section{ID: "private", Title: "Private functions", Note: "Unexported functions and methods, and methods on unexported types."}
	groups := func(public bool) *Section {
		if public {
			return &pub
		}
		return &priv
	}
	add := func(public bool, label string, it Item) {
		s := groups(public)
		for i := range s.Groups {
			if s.Groups[i].Label == label {
				s.Groups[i].Items = append(s.Groups[i].Items, it)
				return
			}
		}
		s.Groups = append(s.Groups, Group{Label: label, Items: []Item{it}})
	}
	for _, s := range []*Section{&pub, &priv} {
		for _, l := range GroupLabels {
			s.Groups = append(s.Groups, Group{Label: l})
		}
	}

	var addedIDs, removedIDs []string
	for _, id := range sortedKeys(d.head.Funcs) {
		h := d.head.Funcs[id]
		b, ok := d.base.Funcs[id]
		switch {
		case !ok:
			addedIDs = append(addedIDs, id)
		case b.Hash != h.Hash:
			detail := "body changed"
			if b.Sig != h.Sig {
				detail = "signature changed: " + b.Sig + " → " + h.Sig
			}
			add(h.Public, modified, Item{h.Name, detail, []Link{d.funcLink("head", d.head, h), d.funcLink("base", d.base, b)}})
		}
	}
	for _, id := range sortedKeys(d.base.Funcs) {
		if _, ok := d.head.Funcs[id]; !ok {
			removedIDs = append(removedIDs, id)
		}
	}

	// A removed and an added function with the same non-trivial body are
	// reported as one rename or move, if the body is unique on both sides.
	d.moves = map[string]string{}
	byBody := func(s *Snapshot, ids []string) map[string][]string {
		m := map[string][]string{}
		for _, id := range ids {
			if f := s.Funcs[id]; f.End-f.Start >= 3 {
				m[f.BodyHash] = append(m[f.BodyHash], id)
			}
		}
		return m
	}
	baseBodies := byBody(d.base, removedIDs)
	for hash, hids := range byBody(d.head, addedIDs) {
		if bids := baseBodies[hash]; len(hids) == 1 && len(bids) == 1 {
			d.moves[hids[0]] = bids[0]
		}
	}
	movedFrom := map[string]bool{}
	for _, bid := range d.moves {
		movedFrom[bid] = true
	}

	for _, id := range addedIDs {
		h := d.head.Funcs[id]
		if bid, ok := d.moves[id]; ok {
			b := d.base.Funcs[bid]
			add(h.Public, moved, Item{h.Name, "was " + b.Name, []Link{d.funcLink("head", d.head, h), d.funcLink("base", d.base, b)}})
			continue
		}
		add(h.Public, added, Item{h.Name, "", []Link{d.funcLink("head", d.head, h)}})
	}
	for _, id := range removedIDs {
		if movedFrom[id] {
			continue
		}
		b := d.base.Funcs[id]
		add(b.Public, removed, Item{b.Name, "", []Link{d.funcLink("base", d.base, b)}})
	}
	return pub, priv
}

// state compares, per function, how many times it reads and writes each
// package-level variable or struct field. Renamed or moved functions are
// compared with their old version.
func (d *differ) state() (reads, writes Section) {
	reads = Section{ID: "reads", Title: "State reads", Note: "Reads of package-level variables and struct fields, per function. Modified means the number of read sites changed."}
	writes = Section{ID: "writes", Title: "State writes", Note: "Writes to package-level variables and struct fields (assignment, ++/--, taking the address), per function. Modified means the number of write sites changed."}
	type row struct {
		target, fn string
		item       Item
	}
	var rows [2][3][]row // [read/write][added/removed/modified]

	target := func(id string) string {
		if n, ok := d.head.Targets[id]; ok {
			return n
		}
		return d.base.Targets[id]
	}
	sites := func(n int) string {
		if n == 1 {
			return "1 site"
		}
		return fmt.Sprintf("%d sites", n)
	}
	compare := func(h, b *FuncInfo) {
		keys := map[Access]bool{}
		var hAcc, bAcc map[Access]int
		if h != nil {
			hAcc = h.Access
		}
		if b != nil {
			bAcc = b.Access
		}
		for k := range hAcc {
			keys[k] = true
		}
		for k := range bAcc {
			keys[k] = true
		}
		for k := range keys {
			hn, bn := hAcc[k], bAcc[k]
			if hn == bn {
				continue
			}
			rw := 0
			if k.Write {
				rw = 1
			}
			var kind int
			var it Item
			switch {
			case bn == 0:
				kind, it = 0, Item{target(k.Target), "in " + h.Name + " · " + sites(hn), []Link{d.funcLink("head", d.head, h)}}
			case hn == 0:
				f := b
				if h != nil {
					f = h
				}
				it = Item{target(k.Target), "in " + f.Name + " · " + sites(bn), nil}
				if h != nil {
					it.Links = append(it.Links, d.funcLink("head", d.head, h))
				}
				it.Links = append(it.Links, d.funcLink("base", d.base, b))
				kind = 1
			default:
				kind, it = 2, Item{target(k.Target), fmt.Sprintf("in %s · %d → %d sites", h.Name, bn, hn), []Link{d.funcLink("head", d.head, h), d.funcLink("base", d.base, b)}}
			}
			name := ""
			if h != nil {
				name = h.Name
			} else {
				name = b.Name
			}
			rows[rw][kind] = append(rows[rw][kind], row{it.Name, name, it})
		}
	}

	seenBase := map[string]bool{}
	for id, h := range d.head.Funcs {
		bid := id
		if old, ok := d.moves[id]; ok {
			bid = old
		}
		b := d.base.Funcs[bid]
		if b != nil {
			seenBase[bid] = true
			if b.Hash == h.Hash && bid == id {
				continue
			}
		}
		compare(h, b)
	}
	for id, b := range d.base.Funcs {
		if !seenBase[id] {
			compare(nil, b)
		}
	}

	build := func(rw int) []Group {
		var gs []Group
		for kind, label := range []string{added, removed, modified} {
			rs := rows[rw][kind]
			slices.SortFunc(rs, func(a, b row) int { return cmp.Or(cmp.Compare(a.target, b.target), cmp.Compare(a.fn, b.fn)) })
			g := Group{Label: label}
			for _, r := range rs {
				g.Items = append(g.Items, r.item)
			}
			gs = append(gs, g)
		}
		return gs
	}
	reads.Groups, writes.Groups = build(0), build(1)
	return reads, writes
}

// jsonTypes compares struct fields carrying json tags: the module's
// serialized wire format, which outside clients depend on even when the Go
// packages are internal.
func (d *differ) jsonTypes() Section {
	s := Section{ID: "json", Title: "Serialized types (JSON)",
		Note: "Struct fields with a json tag, which outside clients see over the wire. Modified means the JSON name, type, or tag options changed."}

	structLink := func(snap *Snapshot, st *StructInfo, line int) Link {
		return Link{fmt.Sprintf("%s:%d", st.File, line), d.fileURL(snap.SHA, st.File, line, 0)}
	}
	var a, r, m []Item
	for _, id := range sortedKeys(d.head.Structs) {
		h := d.head.Structs[id]
		b, ok := d.base.Structs[id]
		if !ok {
			a = append(a, Item{h.Name, fmt.Sprintf("new serialized type · %d JSON fields%s", len(h.Fields), d.usedBy(id)), []Link{structLink(d.head, h, h.Line)}})
			continue
		}
		for _, name := range sortedKeys(h.Fields) {
			hf := h.Fields[name]
			bf, ok := b.Fields[name]
			switch {
			case !ok:
				a = append(a, Item{h.Name + "." + hf.JSON, fmt.Sprintf("%s · field %s%s", hf.Desc(), hf.Name, d.usedBy(id)), []Link{structLink(d.head, h, hf.Line)}})
			case bf.Desc() != hf.Desc():
				m = append(m, Item{h.Name + "." + hf.JSON, fmt.Sprintf("%s → %s%s", bf.Desc(), hf.Desc(), d.usedBy(id)),
					[]Link{structLink(d.head, h, hf.Line), {"base", d.fileURL(d.base.SHA, b.File, bf.Line, 0)}}})
			}
		}
		for _, name := range sortedKeys(b.Fields) {
			if _, ok := h.Fields[name]; !ok {
				bf := b.Fields[name]
				r = append(r, Item{h.Name + "." + bf.JSON, fmt.Sprintf("%s · field %s%s", bf.Desc(), bf.Name, d.usedBy(id)), []Link{structLink(d.base, b, bf.Line)}})
			}
		}
	}
	for _, id := range sortedKeys(d.base.Structs) {
		if _, ok := d.head.Structs[id]; !ok {
			b := d.base.Structs[id]
			r = append(r, Item{b.Name, fmt.Sprintf("serialized type removed · %d JSON fields", len(b.Fields)), []Link{structLink(d.base, b, b.Line)}})
		}
	}
	s.Groups = []Group{{added, a}, {removed, r}, {modified, m}}

	if s.Total() > 0 && len(d.head.Specs)+len(d.base.Specs) > 0 {
		var changed, same []string
		for _, f := range sortedKeys(d.head.Specs) {
			if d.base.Specs[f] == d.head.Specs[f] {
				same = append(same, f)
			} else {
				changed = append(changed, f)
			}
		}
		switch {
		case len(changed) > 0:
			s.Note += fmt.Sprintf(" API spec updated in this change: %s.", strings.Join(changed, ", "))
		case len(same) > 0:
			s.Note += fmt.Sprintf(" ⚠ %s did not change — check whether it needs updating.", strings.Join(same, ", "))
		}
	}
	return s
}

// usedBy lists exported functions whose signature mentions a type, so a field
// change can be traced to the API that exposes it.
func (d *differ) usedBy(typeID string) string {
	type use struct{ name, verb string }
	var uses []use
	for _, id := range sortedKeys(d.head.Funcs) {
		f := d.head.Funcs[id]
		switch {
		case f.Public && slices.Contains(f.Results, typeID):
			uses = append(uses, use{f.Name, "returned by"})
		case f.Public && slices.Contains(f.Params, typeID):
			uses = append(uses, use{f.Name, "passed to"})
		case slices.Contains(f.Builds, typeID):
			uses = append(uses, use{f.Name, "built in"})
		}
	}
	if len(uses) == 0 {
		return ""
	}
	const max = 3
	var parts []string
	for _, u := range uses[:min(len(uses), max)] {
		parts = append(parts, u.verb+" "+u.name)
	}
	out := " · " + strings.Join(parts, ", ")
	if len(uses) > max {
		out += fmt.Sprintf(" and %d more", len(uses)-max)
	}
	return out
}

func (d *differ) packages() Section {
	s := Section{ID: "packages", Title: "Packages", Note: "Modified means a file in the package changed (including comments)."}
	link := func(snap *Snapshot, p *PkgInfo) Link {
		u := ""
		if d.gh != "" {
			u = fmt.Sprintf("%s/tree/%s/%s", d.gh, snap.SHA, path.Join(d.dir, p.Dir))
		}
		return Link{p.Dir, u}
	}
	var a, r, m []Item
	for _, id := range sortedKeys(d.head.Packages) {
		h := d.head.Packages[id]
		b, ok := d.base.Packages[id]
		switch {
		case !ok:
			a = append(a, Item{h.Path, fmt.Sprintf("%d files", len(h.Files)), []Link{link(d.head, h)}})
		case b.Hash != h.Hash:
			m = append(m, Item{h.Path, changedFiles(b.Files, h.Files), []Link{link(d.head, h)}})
		}
	}
	for _, id := range sortedKeys(d.base.Packages) {
		if _, ok := d.head.Packages[id]; !ok {
			b := d.base.Packages[id]
			r = append(r, Item{b.Path, "", []Link{link(d.base, b)}})
		}
	}
	s.Groups = []Group{{added, a}, {removed, r}, {modified, m}}
	return s
}

func changedFiles(base, head []string) string {
	var added, removed int
	for _, f := range head {
		if !slices.Contains(base, f) {
			added++
		}
	}
	for _, f := range base {
		if !slices.Contains(head, f) {
			removed++
		}
	}
	if added == 0 && removed == 0 {
		return ""
	}
	return fmt.Sprintf("+%d / −%d files", added, removed)
}

// deps compares go.mod: required modules, plus the go and toolchain
// directives and replace directives.
func (d *differ) deps() Section {
	s := Section{ID: "modules", Title: "Module dependencies (go.mod)", Note: "Modified means the required version, the go or toolchain directive, or a replace directive changed."}
	reqs := func(f *modfile.File) map[string]*modfile.Require {
		m := map[string]*modfile.Require{}
		if f != nil {
			for _, r := range f.Require {
				m[r.Mod.Path] = r
			}
		}
		return m
	}
	modLink := func(path, version string) []Link {
		return []Link{{"pkg.go.dev", "https://pkg.go.dev/" + path + "@" + version}}
	}
	indirect := func(r *modfile.Require) string {
		if r.Indirect {
			return " (indirect)"
		}
		return ""
	}
	hr, br := reqs(d.head.Mod), reqs(d.base.Mod)
	var a, r, m []Item
	for _, p := range sortedKeys(hr) {
		h := hr[p]
		b, ok := br[p]
		switch {
		case !ok:
			a = append(a, Item{p, h.Mod.Version + indirect(h), modLink(p, h.Mod.Version)})
		case b.Mod.Version != h.Mod.Version:
			dir := "upgraded"
			if semver.Compare(h.Mod.Version, b.Mod.Version) < 0 {
				dir = "downgraded"
			}
			m = append(m, Item{p, fmt.Sprintf("%s %s → %s%s", dir, b.Mod.Version, h.Mod.Version, indirect(h)), modLink(p, h.Mod.Version)})
		}
	}
	for _, p := range sortedKeys(br) {
		if _, ok := hr[p]; !ok {
			r = append(r, Item{p, br[p].Mod.Version + indirect(br[p]), nil})
		}
	}

	directive := func(name string, get func(*modfile.File) string) {
		bv, hv := get(d.base.Mod), get(d.head.Mod)
		if bv != hv {
			m = append(m, Item{name + " directive", fmt.Sprintf("%s → %s", orNone(bv), orNone(hv)), nil})
		}
	}
	directive("go", func(f *modfile.File) string {
		if f == nil || f.Go == nil {
			return ""
		}
		return f.Go.Version
	})
	directive("toolchain", func(f *modfile.File) string {
		if f == nil || f.Toolchain == nil {
			return ""
		}
		return f.Toolchain.Name
	})
	replaces := func(f *modfile.File) string {
		if f == nil {
			return ""
		}
		var rs []string
		for _, r := range f.Replace {
			rs = append(rs, fmt.Sprintf("%s => %s %s", r.Old.Path, r.New.Path, r.New.Version))
		}
		slices.Sort(rs)
		return strings.Join(rs, "; ")
	}
	directive("replace", replaces)

	s.Groups = []Group{{added, a}, {removed, r}, {modified, m}}
	return s
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
