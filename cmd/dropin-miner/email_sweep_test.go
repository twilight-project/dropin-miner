package main

// The reserved-address sweep, beside boundary_test.go's other structural
// guards. A real personal address served as the "email to redact" test
// sample in trace_review_test.go and pkg/redact/redact_test.go from commit
// 5013536 (2026-09-07) until D.4 replaced both with a reserved-domain
// address (RFC 2606). History is not rewritten for it (the address is not
// a credential, and a rewrite would detach every release tag and break the
// open PRs), so nothing but this guard stops the same mistake from
// returning beside the fix. It walks every non-vendored .go file, every
// *.md, and everything under scripts/ and .github/, and fails on an
// email-shaped literal whose domain is not one of those RFC 2606 reserves
// for exactly this purpose.
//
// Splitting the address across "+" ("quasarai" + "@" + "protonmail.com")
// was precisely what let the original literal sit here for weeks without a
// plain-text scanner catching it — this sweep folds Go constant string
// concatenation the way the compiler does before it checks anything, so
// that trick no longer hides a reintroduced address from it either.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// reservedEmailDomain reports whether domain is safe to use as a literal
// "email address" sample: RFC 2606's reserved namespace (and its
// subdomains), which no registrant can ever hold.
func reservedEmailDomain(domain string) bool {
	domain = strings.ToLower(domain)
	switch domain {
	case "example.com", "example.org", "example.net", "localhost":
		return true
	}
	for _, suffix := range []string{".example.com", ".example.org", ".example.net", ".invalid", ".test"} {
		if strings.HasSuffix(domain, suffix) {
			return true
		}
	}
	return false
}

var sweepEmailPattern = regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}`)

// findEmailLiterals reports every email-shaped match in text whose domain
// is not reserved, skipping a match that is URL userinfo rather than a
// standalone address — the same two syntactic tells pkg/redact's own
// redactEmails documents and skips for the identical reason: immediately
// preceded by '/' or ':' (a URL's scheme or "user:pass@host" authority
// section) or immediately followed by ':' (git's SCP-like remote
// shorthand, or an explicit port).
func findEmailLiterals(text string) []string {
	var found []string
	for _, loc := range sweepEmailPattern.FindAllStringIndex(text, -1) {
		start, end := loc[0], loc[1]
		if start > 0 && (text[start-1] == '/' || text[start-1] == ':') {
			continue
		}
		if end < len(text) && text[end] == ':' {
			continue
		}
		match := text[start:end]
		at := strings.LastIndexByte(match, '@')
		if at < 0 || reservedEmailDomain(match[at+1:]) {
			continue
		}
		found = append(found, match)
	}
	return found
}

// foldConstString evaluates a Go constant string expression — one literal,
// or several joined by "+" — the same evaluation the compiler performs, so
// that "quasarai" + "@" + "protonmail.com" folds to the address it spells
// before this sweep ever checks it.
func foldConstString(expr ast.Expr) (string, bool) {
	switch e := expr.(type) {
	case *ast.BasicLit:
		if e.Kind != token.STRING {
			return "", false
		}
		v, err := strconv.Unquote(e.Value)
		if err != nil {
			return "", false
		}
		return v, true
	case *ast.BinaryExpr:
		if e.Op != token.ADD {
			return "", false
		}
		l, ok := foldConstString(e.X)
		if !ok {
			return "", false
		}
		r, ok := foldConstString(e.Y)
		if !ok {
			return "", false
		}
		return l + r, true
	case *ast.ParenExpr:
		return foldConstString(e.X)
	default:
		return "", false
	}
}

// TestNoRealEmailAddressIsUsedAsATestSample is the guard the removal of
// the maintainer's own address (D.4) asked for: nothing here stops that
// exact mistake — a real personal address used as an "email to redact"
// sample — from returning, split across "+" or not, in any .go file,
// *.md, scripts/ or .github/ this module ships.
func TestNoRealEmailAddressIsUsedAsATestSample(t *testing.T) {
	root := moduleRoot(t)
	fset := token.NewFileSet()

	report := func(t *testing.T, path, email string) {
		t.Helper()
		rel, err := filepath.Rel(root, path)
		if err != nil {
			rel = path
		}
		t.Errorf("%s: email literal %q uses a real-looking domain; use a reserved one instead, e.g. someone@example.com", rel, email)
	}

	checkGoFile := func(t *testing.T, path string) {
		t.Helper()
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			var literal ast.Expr
			switch n.(type) {
			case *ast.BasicLit, *ast.BinaryExpr:
				literal = n.(ast.Expr)
			default:
				return true
			}
			v, ok := foldConstString(literal)
			if !ok {
				return true
			}
			for _, email := range findEmailLiterals(v) {
				report(t, path, email)
			}
			return true
		})
	}

	checkTextFile := func(t *testing.T, path string) {
		t.Helper()
		data, err := os.ReadFile(path) // #nosec G304 -- test-owned repo file under moduleRoot
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		for _, email := range findEmailLiterals(string(data)) {
			report(t, path, email)
		}
	}

	sep := string(filepath.Separator)
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "bin", "dist", "testdata", "node_modules", "vendor":
				return filepath.SkipDir
			}
			return nil
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return rerr
		}
		switch {
		case strings.HasSuffix(path, ".go"):
			checkGoFile(t, path)
		case strings.HasSuffix(path, ".md"):
			checkTextFile(t, path)
		case strings.HasPrefix(rel, "scripts"+sep) || strings.HasPrefix(rel, ".github"+sep):
			checkTextFile(t, path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
