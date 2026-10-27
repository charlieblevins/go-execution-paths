// Package goload loads type-checked Go packages from a module directory and
// provides helpers shared by the commands in this repository.
package goload

import (
	"fmt"
	"go/types"
	"path/filepath"
	"strings"

	"golang.org/x/tools/go/packages"
)

// Load loads the packages matching patterns in the module at root, with
// syntax trees and type information.
func Load(root string, patterns []string, tests bool) ([]*packages.Package, error) {
	return packages.Load(&packages.Config{
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedSyntax |
			packages.NeedTypes | packages.NeedTypesInfo,
		Dir:   root,
		Tests: tests,
	}, patterns...)
}

// RelPath returns path relative to root, or false if it lies outside root
// (e.g. cgo-generated files in the build cache).
func RelPath(root, path string) (string, bool) {
	rel, err := filepath.Rel(root, path)
	if err != nil || strings.HasPrefix(rel, "..") {
		return "", false
	}
	return filepath.ToSlash(rel), true
}

// DisplayName returns a short name like "pkg.Func", "pkg.T.M", or "pkg.(*T).M".
func DisplayName(fn *types.Func) string {
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
