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
// Splitting the address across "+" was precisely what let the original
// literal sit here for weeks without a plain-text scanner catching it —
// this sweep folds Go constant string concatenation the way the compiler
// does before it checks anything, so that trick no longer hides a
// reintroduced address from it either. A second, separate check below
// catches the one place the literal-folding check cannot reach at all: a
// comment or doc sentence that just spells the address out in prose, the
// way an early draft of this very file once did. It lowercases every file
// the walk visits and hashes three kinds of token — each email-shaped
// match's domain, local part and local part cut at its first "+"; every
// alphanumeric run of four or more; every dotted host name — against
// bannedHashes, the SHA-256 of the maintainer's domain and username. The
// denylist is carried as digests so that this file no longer spells what it
// denies: it is scanned like every other file, and guards itself.
//
// What the hashes lose against the raw substring scan this replaced: a
// fragment buried inside a longer token — the username with letters or
// digits glued on either side, or the domain as the tail of a longer host
// name — is no longer caught, because only whole tokens can be hashed. That
// is the price of not carrying the plaintext, and it is the right trade. A
// maintainer who wants the raw scan back locally puts the plain fragments,
// one lowercase substring per line, in .email-sweep-denylist at the module
// root, which .gitignore keeps out of every commit and which CI never has.

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
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

// bannedHashes maps the hex SHA-256 of each banned fragment, lowercased, to
// the name a failure reports it by. Recompute one with
//
//	printf '%s' 'value' | shasum -a 256
var bannedHashes = map[string]string{
	// SHA-256 of the maintainer's email domain.
	"3e2ae79c33547fe9e8c159276ef409a2140d527f75c0646d3f076b27512b6cdd": "the maintainer's email domain",
	// SHA-256 of the maintainer's username, the address's local part.
	"ada5fc655d63a3c81e6de77567df23f0760b5da968fb510d58b7a45ec4885c45": "the maintainer's username",
}

var (
	// sweepRunPattern is what catches the username spelled alone in prose,
	// which the email pattern cannot.
	sweepRunPattern = regexp.MustCompile(`[a-z0-9]{4,}`)
	// sweepHostPattern is what catches the domain spelled alone.
	sweepHostPattern = regexp.MustCompile(`[a-z0-9-]+(?:\.[a-z0-9-]+)+`)
)

// bannedTokenHits returns, once each and in no particular order, the names
// of the bannedHashes entries that some token of lower hashes to. lower must
// already be lowercased. Email matches are taken before the reserved-domain
// filter and the URL-userinfo skip, since the point is to catch the real
// address wherever it is.
func bannedTokenHits(lower string) []string {
	hit := map[string]bool{}
	check := func(token string) {
		sum := sha256.Sum256([]byte(token))
		if name, ok := bannedHashes[hex.EncodeToString(sum[:])]; ok {
			hit[name] = true
		}
	}
	for _, match := range sweepEmailPattern.FindAllString(lower, -1) {
		at := strings.LastIndexByte(match, '@')
		local := match[:at]
		check(match[at+1:])
		check(local)
		if plus := strings.IndexByte(local, '+'); plus >= 0 {
			check(local[:plus])
		}
	}
	for _, run := range sweepRunPattern.FindAllString(lower, -1) {
		check(run)
	}
	for _, host := range sweepHostPattern.FindAllString(lower, -1) {
		check(host)
	}
	names := make([]string, 0, len(hit))
	for name := range hit {
		names = append(names, name)
	}
	return names
}

// sweepDenylistName is the optional private list of plain fragments for the
// raw substring pass, read from the module root on the maintainer's machine
// only. It is gitignored and never committed.
const sweepDenylistName = ".email-sweep-denylist"

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
// that an address split across several literals folds to the value it
// spells before this sweep ever checks it.
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

	denylistPath := filepath.Join(root, sweepDenylistName)
	type denylistEntry struct {
		line     int
		fragment string
	}
	var denylist []denylistEntry
	switch data, err := os.ReadFile(denylistPath); { // #nosec G304 -- fixed name under moduleRoot
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		t.Fatalf("read %s: %v", sweepDenylistName, err)
	default:
		for i, line := range strings.Split(string(data), "\n") {
			if fragment := strings.ToLower(strings.TrimSpace(line)); fragment != "" {
				denylist = append(denylist, denylistEntry{line: i + 1, fragment: fragment})
			}
		}
	}

	relPath := func(path string) string {
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return path
		}
		return rel
	}

	report := func(t *testing.T, path, email string) {
		t.Helper()
		t.Errorf("%s: email literal %q uses a real-looking domain; use a reserved one instead, e.g. someone@example.com", relPath(path), email)
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

	// No banned fragment may appear anywhere in this scope, in any context
	// — literal, concatenated, or prose in a comment or doc. These are scans
	// of the raw bytes, not an AST walk: they are what actually would have
	// caught this file's own first draft, which spelled the address out in
	// an explanatory comment rather than as a test value — the blind spot
	// the literal-and-comment-folding check above structurally cannot close
	// on its own, since a comment is prose, not a constant expression to
	// fold. The hashed token pass always runs; the raw substring pass runs
	// only when the private denylist is present.
	checkNoBannedFragment := func(t *testing.T, path string) {
		t.Helper()
		data, err := os.ReadFile(path) // #nosec G304 -- test-owned repo file under moduleRoot
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		lower := strings.ToLower(string(data))
		for _, name := range bannedTokenHits(lower) {
			t.Errorf("%s: contains a token whose SHA-256 is bannedHashes' entry for %s — the maintainer's own address must never appear here, not even in a comment", relPath(path), name)
		}
		for _, banned := range denylist {
			if strings.Contains(lower, banned.fragment) {
				t.Errorf("%s: contains line %d of %s (raw substring pass) — the maintainer's own address must never appear here, not even in a comment", relPath(path), banned.line, sweepDenylistName)
			}
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
		if path == denylistPath {
			return nil
		}
		checkNoBannedFragment(t, path)
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
