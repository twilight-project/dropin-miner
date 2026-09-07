package main

// The import-boundary tests: the package graph IS the argument for
// AGENTS.md's invariant 8, so a test fails loudly when someone adds the
// wrong import. Ported from tokendrop-proxy's cmd/tokendrop/boundary_test.go
// (goList/moduleRoot, the machinery), stating this repo's own two
// boundaries rather than the proxy's — this module has no
// internal/forward, internal/observe, or internal/mining/sign to guard.

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const module = "github.com/twilight-project/dropin-miner"

func goList(t *testing.T, patterns ...string) []string {
	t.Helper()
	args := append([]string{"list", "-deps"}, patterns...)
	cmd := exec.Command("go", args...) // #nosec G204 -- fixed binary, test-owned patterns
	cmd.Dir = moduleRoot(t)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list %v: %v", patterns, err)
	}
	return strings.Fields(string(out))
}

// moduleRoot: this file lives at cmd/dropin-miner/, same depth as the
// proxy's cmd/tokendrop/, so the relative path back to the repo root is
// unchanged.
func moduleRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	return root
}

// TestPkgDoesNotImportCmd is AGENTS.md invariant 8: pkg/ is the shared
// protocol implementation this repo owns and hands back to the proxy by
// import, which only works if pkg/ never reaches into the wrapper around
// it. A pkg/ package importing cmd/dropin-miner — directly or
// transitively — would make that impossible.
func TestPkgDoesNotImportCmd(t *testing.T) {
	forbidden := module + "/cmd/dropin-miner"
	for _, dep := range goList(t, "./pkg/...") {
		if dep == forbidden {
			t.Fatalf("pkg/ reaches %s: pkg/ must not import the cmd/ wrapper (AGENTS.md invariant 8)", forbidden)
		}
	}
}

// EVERY http.Client IN THE MODULE CARRIES AN EXPLICIT REDIRECT POLICY.
//
// Ported from tokendrop-proxy's cmd/tokendrop/boundary_test.go, unmodified in
// substance: net/http's default follows up to ten redirects, replays the
// method AND the body on a 307/308, and strips only Authorization/Cookie on a
// cross-host hop — a client built without CheckRedirect is one open redirect
// away from handing whatever it carries to another host.
//
// THE SCOPE IS THE POINT, here more than it was when this was ported. Writing
// this test module-wide (not scoped to, say, "the clients that call the AS")
// is what found credentials.go's login probe: a redirect-guard review aimed
// at AS-facing clients had no reason to look at a client calling the router.
// A scope chosen by where the reviewer expected the risk is exactly the scope
// this test refuses to have.
//
// It asserts PRESENCE, not a particular policy — auth.SameOriginRedirects and
// http.ErrUseLastResponse are both correct answers for different clients, and
// which one a client wants is a judgement its author must make deliberately.
// What is forbidden is not making it.
//
// Test files are exempt: a bare client in a test harnesses something rather
// than shipping it, and several deliberately drive redirects to observe them.
func TestEveryHTTPClientInTheModuleSetsARedirectPolicy(t *testing.T) {
	root := moduleRoot(t)
	fset := token.NewFileSet()
	checked := 0

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "bin", "dist", "testdata", "node_modules":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return fmt.Errorf("parse %s: %w", path, perr)
		}
		rel, _ := filepath.Rel(root, path)
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
			t.Errorf("%s:%d: an http.Client is constructed with no CheckRedirect.\n"+
				"  net/http then follows up to ten redirects, replaying the body on a 307/308 "+
				"and carrying every header it does not strip.\n"+
				"  Set one deliberately: auth.SameOriginRedirects to stay on the origin, or "+
				"http.ErrUseLastResponse to refuse redirects outright.",
				rel, fset.Position(lit.Pos()).Line)
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// A test that found nothing would pass forever after a rename.
	if checked == 0 {
		t.Fatal("no http.Client construction was found anywhere in the module; " +
			"this test is no longer looking at anything")
	}
	t.Logf("checked %d http.Client construction(s) module-wide", checked)
}

func isHTTPClientLiteral(e ast.Expr) bool {
	sel, ok := e.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "http" && sel.Sel.Name == "Client"
}
