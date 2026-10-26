package main

import (
	"fmt"
	"html"
	"html/template"
	"strings"

	"github.com/alecthomas/chroma/v2"
	chromahtml "github.com/alecthomas/chroma/v2/formatters/html"
	"github.com/alecthomas/chroma/v2/lexers"
	"github.com/alecthomas/chroma/v2/styles"
)

// highlightLines lexes a whole file and returns it as one HTML string per line.
func highlightLines(src []byte) ([]string, error) {
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
	return append(lines, b.String()), nil
}

func tokenClass(t chroma.TokenType) string {
	for _, c := range []chroma.TokenType{t, t.SubCategory(), t.Category()} {
		if s, ok := chroma.StandardTypes[c]; ok {
			return s
		}
	}
	return ""
}

func chromaCSS() (template.CSS, error) {
	var b strings.Builder
	err := chromahtml.New(chromahtml.WithClasses(true)).WriteCSS(&b, styles.Get("github"))
	return template.CSS(b.String()), err
}
