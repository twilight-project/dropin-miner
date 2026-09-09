package platform

// TestNoBareHTTPClientInThisPackage is this package's own copy of
// cmd/dropin-miner/boundary_test.go's module-wide sweep, scoped to this
// directory: every http.Client{} construction here must set
// CheckRedirect, because Status and Enroll carry the participant's sr-
// key in Authorization. One constructor already does this
// (newPlatformClient) — this is what keeps a second, bare one from
// being added next to it later.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNoBareHTTPClientInThisPackage(t *testing.T) {
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	checked := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		path := filepath.Join(dir, name)
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.CompositeLit)
			if !ok || !isHTTPClientLiteral(lit.Type) {
				return true
			}
			checked++
			for _, elt := range lit.Elts {
				if kv, ok := elt.(*ast.KeyValueExpr); ok {
					if id, ok := kv.Key.(*ast.Ident); ok && id.Name == "CheckRedirect" {
						return true
					}
				}
			}
			t.Errorf("%s:%d: an http.Client is constructed with no CheckRedirect; use newPlatformClient or set auth.SameOriginRedirects explicitly",
				name, fset.Position(lit.Pos()).Line)
			return true
		})
	}
	if checked == 0 {
		t.Fatal("found no http.Client{} construction to check — the AST walk itself may be broken")
	}
}

func isHTTPClientLiteral(e ast.Expr) bool {
	sel, ok := e.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "http" && sel.Sel.Name == "Client"
}
