package main

import (
	"cmp"
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"maps"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"time"
)

//go:embed web
var webFS embed.FS

type server struct {
	root     string
	patterns []string
	tests    bool

	mu  sync.RWMutex
	idx *Index
	hl  *Highlighter
}

func main() {
	dir := flag.String("dir", ".", "Go module directory to analyze")
	addr := flag.String("addr", "localhost:8080", "listen address")
	tests := flag.Bool("tests", false, "include _test.go files")
	flag.Parse()
	patterns := flag.Args()
	// Accept the module directory as the first argument too, with flags after it.
	if len(patterns) > 0 && *dir == "." && isModuleDir(patterns[0]) {
		*dir = patterns[0]
		flag.CommandLine.Parse(patterns[1:])
		patterns = flag.Args()
	}
	if len(patterns) == 0 {
		patterns = []string{"./..."}
	}

	root, err := filepath.Abs(*dir)
	if err != nil {
		log.Fatal(err)
	}
	s := &server{root: root, patterns: patterns, tests: *tests}
	if err := s.reload(); err != nil {
		log.Fatal(err)
	}

	static, _ := fs.Sub(webFS, "web")
	mux := http.NewServeMux()
	mux.Handle("GET /", http.FileServerFS(static))
	mux.HandleFunc("GET /style.css", s.handleCSS)
	mux.HandleFunc("GET /api/tree", s.handleTree)
	mux.HandleFunc("GET /api/file", s.handleFile)
	mux.HandleFunc("GET /api/func", s.handleFunc)
	mux.HandleFunc("GET /api/lines", s.handleLines)
	mux.HandleFunc("POST /api/reload", s.handleReload)

	log.Printf("serving %s on http://%s", root, *addr)
	log.Fatal(http.ListenAndServe(*addr, localOnly(mux)))
}

func (s *server) reload() error {
	start := time.Now()
	idx, err := Load(s.root, s.patterns, s.tests)
	if err != nil {
		return err
	}
	for _, e := range idx.Errors {
		log.Printf("package error: %s", e)
	}
	log.Printf("loaded %d functions from %d files in %s", len(idx.Funcs), len(idx.Files), time.Since(start).Round(time.Millisecond))
	if len(idx.Funcs) == 0 {
		return fmt.Errorf("no functions found in %s for patterns %v (usage: gep -dir /path/to/module [patterns...])", s.root, s.patterns)
	}
	s.mu.Lock()
	s.idx, s.hl = idx, NewHighlighter()
	s.mu.Unlock()
	return nil
}

func (s *server) state() (*Index, *Highlighter) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.idx, s.hl
}

func (s *server) handleCSS(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/css")
	writeCSS(w)
}

func (s *server) handleTree(w http.ResponseWriter, r *http.Request) {
	idx, _ := s.state()
	writeJSON(w, slices.Sorted(maps.Keys(idx.Files)))
}

// handleFile returns a whole highlighted file plus the functions defined in it.
func (s *server) handleFile(w http.ResponseWriter, r *http.Request) {
	idx, hl := s.state()
	file := r.URL.Query().Get("file")
	abs, ok := idx.Files[file]
	if !ok {
		http.Error(w, "unknown file", http.StatusNotFound)
		return
	}
	lines, err := hl.Lines(abs)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1]
	}
	type item struct {
		*Func
		Callers int `json:"callers"`
	}
	funcs := []item{}
	for _, f := range idx.Funcs {
		if f.File == file {
			funcs = append(funcs, item{f, len(idx.Callers[f.ID])})
		}
	}
	slices.SortFunc(funcs, func(a, b item) int { return cmp.Compare(a.Start, b.Start) })
	writeJSON(w, map[string]any{"lines": lines, "funcs": funcs})
}

func (s *server) handleFunc(w http.ResponseWriter, r *http.Request) {
	idx, _ := s.state()
	f, ok := idx.Funcs[r.URL.Query().Get("id")]
	if !ok {
		http.Error(w, "unknown function", http.StatusNotFound)
		return
	}
	callers := idx.Callers[f.ID]
	if callers == nil {
		callers = []Call{}
	}
	writeJSON(w, map[string]any{"func": f, "callers": callers})
}

// handleLines returns highlighted HTML for lines [from, to] (1-based, inclusive)
// of a file in the index.
func (s *server) handleLines(w http.ResponseWriter, r *http.Request) {
	idx, hl := s.state()
	q := r.URL.Query()
	abs, ok := idx.Files[q.Get("file")]
	if !ok {
		http.Error(w, "unknown file", http.StatusNotFound)
		return
	}
	lines, err := hl.Lines(abs)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	from, _ := strconv.Atoi(q.Get("from"))
	to, _ := strconv.Atoi(q.Get("to"))
	from = max(from, 1)
	to = min(to, len(lines))
	if from > to {
		http.Error(w, "bad line range", http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]any{"from": from, "lines": lines[from-1 : to]})
}

func (s *server) handleReload(w http.ResponseWriter, r *http.Request) {
	if err := s.reload(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func isModuleDir(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, "go.mod"))
	return err == nil
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

// localOnly rejects requests whose Host isn't a loopback name, which blocks
// DNS-rebinding attacks from other sites.
func localOnly(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.Host)
		if err != nil {
			host = r.Host
		}
		if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		h.ServeHTTP(w, r)
	})
}
