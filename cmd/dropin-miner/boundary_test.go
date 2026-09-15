package main

// The import-boundary tests: the package graph IS the argument for
// AGENTS.md's import-boundary invariants, so a test fails loudly when
// someone adds the wrong import. Ported from tokendrop-proxy's
// cmd/tokendrop/boundary_test.go (goList/moduleRoot, the machinery),
// stating this repo's own boundaries rather than the proxy's — this
// module has no internal/forward, internal/observe, or
// internal/mining/sign to guard.

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

// TestSelfupdateImportsNoWrapperAndNoCredential is the self-updater's
// boundary: internal/selfupdate fetches public release assets and must never
// reach the cmd/ wrapper, the authorization and key store (pkg/auth) or the
// platform client (pkg/platform) — directly or transitively — so no
// participant credential can ride along with a release download.
func TestSelfupdateImportsNoWrapperAndNoCredential(t *testing.T) {
	deps := goList(t, "./internal/selfupdate/...")
	if len(deps) == 0 {
		t.Fatal("go list found no dependencies for internal/selfupdate")
	}
	for _, dep := range deps {
		switch dep {
		case module + "/cmd/dropin-miner", module + "/pkg/auth", module + "/pkg/platform":
			t.Errorf("internal/selfupdate reaches %s (AGENTS.md import boundaries)", dep)
		}
	}
}

// TestNoChainImportsAnywhere is AGENTS.md invariant 9. Ported from the
// proxy's cmd/tokendrop/boundary_test.go rule 1, adapted to this module:
// twilight-core is the same forbidden chain application, and the
// Cosmos/CometBFT graph is banned for the same reason it is there — a
// lean binary shipped to users must not pull the SDK's module tree.
// wallet_tx.go hand-encodes the six protobuf messages a bank send needs
// instead of importing cosmos-sdk to build them; this is what proves
// that decision holds, not just documents it.
func TestNoChainImportsAnywhere(t *testing.T) {
	banned := []string{
		"github.com/twilight-project/twilight-core",
		"cosmossdk.io/",
		"github.com/cosmos/",
		"github.com/cometbft/",
	}
	for _, dep := range goList(t, "./...") {
		for _, b := range banned {
			if strings.HasPrefix(dep, b) {
				t.Errorf("banned dependency in module graph: %s", dep)
			}
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

// EVERY CUSTOM TRANSPORT EITHER HAS A DIAL SEAM OR IS A NAMED, REVIEWED GAP.
//
// D1b's network_fence_test.go refuses every non-loopback dial this test
// binary attempts, but it can only do that by replacing http.DefaultTransport
// — which reaches any `&http.Client{}` that leaves Transport nil, and
// nothing else. A client that builds its own `*http.Transport` (this module's
// own AS-isolation discipline: `Proxy: nil` so no environment proxy can
// interpose on a credential-bearing client's identity) dials through its own,
// separate default dialer instead, invisible to that swap.
//
// unfencedCustomTransports is the reviewed list of exactly which files do
// that today with no seam of their own — pkg/auth's discovery and DPoP
// transports, out of scope for a seam until a separate ruling accepts one
// there (client_network_fence_test.go's
// TestPkgAuthTransportsAreNotYetCoveredByTheNetworkFence is the guard-side
// half of this same fact). wallet_tx.go builds the same shape but is NOT
// listed here, because it also gives itself a DialContext seam
// (rpcClientDialContext) that a guard test arms — this sweep accepts that as
// covered by checking for the word DialContext anywhere in the same file,
// not just inside the Transport literal itself, since the seam is wired in a
// separate statement after construction, not inside the literal.
//
// The point of running this test at all, rather than trusting the four rows
// above to stay current by review: a FUTURE file that builds a fifth isolated
// Transport, anywhere in the module, trips this test instead of silently
// opening a hole the fence cannot see — exactly what "give it a test seam and
// cover that" has to mean for the client that comes after this one.
var unfencedCustomTransports = map[string]bool{
	"pkg/auth/discovery.go": true,
	"pkg/auth/transport.go": true,
}

func TestEveryCustomTransportEitherHasADialSeamOrIsAReviewedGap(t *testing.T) {
	root := moduleRoot(t)
	fset := token.NewFileSet()
	checked := 0
	seen := map[string]bool{}

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
		rel = filepath.ToSlash(rel)
		hasDialSeamInFile := fileMentionsDialContext(file)
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.CompositeLit)
			if !ok || !isHTTPTransportLiteral(lit.Type) || !literalIsolatesProxy(lit) {
				return true
			}
			checked++
			if hasDialSeamInFile {
				return true
			}
			if unfencedCustomTransports[rel] {
				seen[rel] = true
				return true
			}
			t.Errorf("%s:%d: a new http.Transport{Proxy: nil} with no DialContext seam anywhere in the file — "+
				"the network fence's http.DefaultTransport swap cannot reach a client built from this.\n"+
				"  Give it a DialContext seam (wallet_tx.go's rpcClientDialContext is the pattern) and a guard "+
				"test in client_network_fence_test.go, or add this file to unfencedCustomTransports with the "+
				"ruling that accepts the gap.",
				rel, fset.Position(lit.Pos()).Line)
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if checked == 0 {
		t.Fatal("no isolated http.Transport{Proxy: nil} was found anywhere in the module; " +
			"this test is no longer looking at anything")
	}
	for rel := range unfencedCustomTransports {
		if !seen[rel] {
			t.Errorf("unfencedCustomTransports names %s but no Transport{Proxy: nil} literal was found there "+
				"anymore — remove the row, or check whether it moved", rel)
		}
	}
}

// fileMentionsDialContext reports whether the identifier DialContext
// appears anywhere in file — as a struct field key inside a composite
// literal, or as a plain identifier such as a package-local seam variable
// or an assignment to one (wallet_tx.go's rpcClientDialContext is the
// latter shape: the seam is wired in a statement after the Transport
// literal, not inside it). An AST walk rather than a source-text search:
// this file already parses every candidate with go/parser, and gosec's
// filesystem-in-a-WalkDir-callback rule (G122) is exactly the reason not to
// also read the raw bytes separately just for a substring check.
func fileMentionsDialContext(file *ast.File) bool {
	found := false
	ast.Inspect(file, func(n ast.Node) bool {
		if found {
			return false
		}
		if id, ok := n.(*ast.Ident); ok && id.Name == "DialContext" {
			found = true
			return false
		}
		return true
	})
	return found
}

func isHTTPTransportLiteral(e ast.Expr) bool {
	sel, ok := e.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "http" && sel.Sel.Name == "Transport"
}

// literalIsolatesProxy reports whether the composite literal sets
// Proxy: nil — the isolation shape every custom Transport in this module
// uses instead of leaving Transport nil, so that no environment proxy can
// silently interpose on a credential-bearing client's identity.
func literalIsolatesProxy(lit *ast.CompositeLit) bool {
	for _, elt := range lit.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		id, ok := kv.Key.(*ast.Ident)
		if !ok || id.Name != "Proxy" {
			continue
		}
		v, ok := kv.Value.(*ast.Ident)
		return ok && v.Name == "nil"
	}
	return false
}

// TestConnectAndMiningNeverImportOSExec is invariant 11 (agent
// onboarding design §6): the client never launches a process from the
// connect path — the claim URL is printed, never opened. Scoped to
// connect.go and mining.go specifically, not the module: os/exec is
// legitimately imported elsewhere (detach_unix.go's spawnDetached is
// the one real process spawn in this codebase, which search.go's
// startFlush/startConnectResume both call into — importing os, not
// os/exec, themselves) and by this very file, to shell out to `go list`
// for the test above.
func TestConnectAndMiningNeverImportOSExec(t *testing.T) {
	root := moduleRoot(t)
	fset := token.NewFileSet()
	checked := 0
	for _, name := range []string{"connect.go", "mining.go"} {
		path := filepath.Join(root, "cmd", "dropin-miner", name)
		file, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		checked++
		for _, imp := range file.Imports {
			if imp.Path.Value == `"os/exec"` {
				t.Errorf("%s imports os/exec: the connect path must never launch a process; "+
					"the claim URL is printed, never opened", name)
			}
		}
	}
	if checked != 2 {
		t.Fatalf("checked %d files, want 2 (connect.go, mining.go) — this test is no longer looking at anything", checked)
	}
}

// unsafeAllowed is the one file in the module that may import unsafe: setup's
// Windows environment broadcast, which has to hand SendMessageTimeoutW the
// address of a UTF-16 string and has no other way to form it. Anything else
// that reaches for unsafe is a new exception, and belongs in review rather
// than in a quiet import.
const unsafeAllowed = "cmd/dropin-miner/setup_env_windows.go"

// TestOnlyTheEnvironmentBroadcastImportsUnsafe walks every .go file in the
// module — every package, every build tag, tests included, since the parser
// reads files regardless of GOOS — and fails on any unsafe import outside
// unsafeAllowed. It also fails if the allowed file stops importing it, so the
// exception cannot outlive its reason unnoticed.
func TestOnlyTheEnvironmentBroadcastImportsUnsafe(t *testing.T) {
	root := moduleRoot(t)
	fset := token.NewFileSet()
	checked, allowedSeen := 0, false
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if path != root && (strings.HasPrefix(name, ".") || name == "testdata" || name == "node_modules") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		file, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			return fmt.Errorf("parse %s: %w", path, err)
		}
		checked++
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		for _, imp := range file.Imports {
			if imp.Path.Value != `"unsafe"` {
				continue
			}
			if rel == unsafeAllowed {
				allowedSeen = true
				continue
			}
			t.Errorf("%s imports unsafe; the module admits it only in %s", rel, unsafeAllowed)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if checked < 100 {
		t.Fatalf("walked only %d .go files under %s; the walk is not covering the module", checked, root)
	}
	if !allowedSeen {
		t.Errorf("%s no longer imports unsafe: remove the exception from unsafeAllowed, AGENTS.md and pkg/auth/refreshlock_windows.go", unsafeAllowed)
	}
}
