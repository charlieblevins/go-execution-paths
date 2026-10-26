package main

import (
	"fmt"
	"html"
	"io"
	"os"
	"strings"
	"sync"

	"github.com/alecthomas/chroma/v2"
	chromahtml "github.com/alecthomas/chroma/v2/formatters/html"
	"github.com/alecthomas/chroma/v2/lexers"
	"github.com/alecthomas/chroma/v2/styles"
)

const styleName = "github"

// Highlighter lexes whole files once and caches them as per-line HTML, so
// slicing out a snippet never starts in the middle of a comment or raw string.
type Highlighter struct {
	mu    sync.Mutex
	cache map[string][]string
}

func NewHighlighter() *Highlighter {
	return &Highlighter{cache: map[string][]string{}}
}

func (h *Highlighter) Lines(path string) ([]string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if lines, ok := h.cache[path]; ok {
		return lines, nil
	}
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	it, err := chroma.Coalesce(lexers.Get("go")).Tokenise(nil, string(src))
	if err != nil {
		return nil, err
	}

	var lines []string
	var b strings.Builder
	for _, tok := range it.Tokens() {
		cls := tokenClass(tok.Type)
		for i, part := range strings.Split(tok.Value, "\n") {
			if i > 0 {
				lines = append(lines, b.String())
				b.Reset()
			}
			if part == "" {
				continue
			}
			if cls != "" {
				fmt.Fprintf(&b, `<span class="%s">%s</span>`, cls, html.EscapeString(part))
			} else {
				b.WriteString(html.EscapeString(part))
			}
		}
	}
	lines = append(lines, b.String())
	h.cache[path] = lines
	return lines, nil
}

func tokenClass(t chroma.TokenType) string {
	for _, c := range []chroma.TokenType{t, t.SubCategory(), t.Category()} {
		if s, ok := chroma.StandardTypes[c]; ok {
			return s
		}
	}
	return ""
}

func writeCSS(w io.Writer) error {
	return chromahtml.New(chromahtml.WithClasses(true)).WriteCSS(w, styles.Get(styleName))
}
