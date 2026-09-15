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

// EVERY http.Transport IN NON-TEST CODE DIALS THROUGH internal/netdial'S SHARED SEAM.
//
// internal/networkfence's Guard (called from every package's TestMain that
// builds a network client) reassigns internal/netdial.Hook for the
// duration of a test run. Every Transport this module builds reaches that
// hook only by setting its own DialContext field to netdial.For(itsOwnDialer)
// — one built with its own net.Dialer instead, or with DialContext left
// unset, is invisible to it.
//
// A DialContext field is set two ways in this module: inline, inside an
// &http.Transport{...} composite literal (pkg/auth's two, wallet_tx.go's),
// or as a separate assignment after http.DefaultTransport.Clone() (the
// login probe and the search client, via client.go's shared
// cloneDefaultTransport; pkg/platform's client; internal/selfupdate's
// source) — Clone() itself produces no composite literal to inspect, so
// this sweep has to catch both an *ast.KeyValueExpr with key DialContext
// and an *ast.AssignStmt whose left side is some value's .DialContext
// field, and require the same thing of either: the value assigned is
// exactly netdial.For(...), a call, never netdial.Hook (or any other name)
// named directly.
//
// "Named directly" is not a hypothetical failure mode: D1c found that
// naming a package variable directly in a Transport literal
// (DialContext: someVar) copies whatever function value the variable held
// at that Transport's construction time into the struct field permanently.
// Every package-level Transport var in this module (constructed once, at
// init, before any TestMain ever runs) was built that way for one commit
// and was never actually reachable by the fence at all; it looked covered
// only because of an unrelated proxy-environment masking bug producing the
// same symptom for a different reason. internal/netdial's own
// TestForObservesAHookInstalledAfterConstruction guards the mechanism this
// sweep exists to make every call site use correctly.
func TestEveryHTTPTransportDialsThroughTheSharedSeam(t *testing.T) {
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
			case "networkfence":
				// internal/networkfence is test-only infrastructure (its
				// own doc comment: "no production code anywhere in this
				// module imports it") and its own DialContext field
				// assignments are test comparison plumbing
				// (AssertTransportFieldsMatch nils both sides' DialContext
				// before comparing them), not a client this sweep needs to
				// prove is fenced.
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
		local := netdialLocalName(file)
		fail := func(pos token.Pos) {
			t.Errorf("%s:%d: DialContext is not set to netdial.For(...) — "+
				"internal/networkfence's test fence cannot reach a client built from this.\n"+
				"  Name the seam explicitly (netdial.For(itsOwnDialer)), the way every other "+
				"Transport in the module does.",
				rel, fset.Position(pos).Line)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.KeyValueExpr:
				id, ok := node.Key.(*ast.Ident)
				if !ok || id.Name != "DialContext" {
					return true
				}
				checked++
				if !isNetdialFor(node.Value, local) {
					fail(node.Pos())
				}
			case *ast.AssignStmt:
				for i, lhs := range node.Lhs {
					sel, ok := lhs.(*ast.SelectorExpr)
					if !ok || sel.Sel.Name != "DialContext" {
						continue
					}
					checked++
					if i >= len(node.Rhs) || !isNetdialFor(node.Rhs[i], local) {
						fail(node.Pos())
					}
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if checked == 0 {
		t.Fatal("no DialContext field was set anywhere in the module; this test is no longer looking at anything")
	}
	t.Logf("checked %d DialContext assignment(s) module-wide", checked)
}

// netdialImportPath is internal/netdial's import path, quoted exactly as
// go/ast represents an ImportSpec's Path.Value.
const netdialImportPath = `"github.com/twilight-project/dropin-miner/internal/netdial"`

// netdialLocalName returns the identifier file uses to refer to
// internal/netdial — its import alias if it has one, else the package's
// own name "netdial" — or "" if the file does not import it at all.
func netdialLocalName(file *ast.File) string {
	for _, imp := range file.Imports {
		if imp.Path.Value != netdialImportPath {
			continue
		}
		if imp.Name != nil {
			return imp.Name.Name
		}
		return "netdial"
	}
	return ""
}

// isNetdialFor reports whether e is exactly netdialLocal.For(...) — a call,
// on the file's own import of internal/netdial, to the function named For.
// Anything else (a bare identifier, a different function, a call to
// something else entirely) does not count.
func isNetdialFor(e ast.Expr, netdialLocal string) bool {
	if netdialLocal == "" {
		return false
	}
	call, ok := e.(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "For" {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == netdialLocal
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
