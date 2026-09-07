package main

// The import-boundary tests: the package graph IS the argument for
// AGENTS.md's invariant 8, so a test fails loudly when someone adds the
// wrong import. Ported from tokendrop-proxy's cmd/tokendrop/boundary_test.go
// (goList/moduleRoot, the machinery), stating this repo's own two
// boundaries rather than the proxy's — this module has no
// internal/forward, internal/observe, or internal/mining/sign to guard.

import (
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
