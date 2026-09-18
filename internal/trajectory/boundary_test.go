package trajectory

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// This package only reads. Two structural guards say so, because "I looked
// and it has no write path" stops being true the first time nobody looks.

// TestPackageReachesNoNetworkAndSpawnsNothing walks the dependency graph: a
// reader of somebody's private transcripts has no business being able to
// dial anything or run anything, directly or through an import.
func TestPackageReachesNoNetworkAndSpawnsNothing(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", ".").Output() // #nosec G204 -- fixed arguments
	if err != nil {
		t.Fatalf("go list -deps: %v", err)
	}
	deps := strings.Fields(string(out))
	if len(deps) < 5 {
		t.Fatalf("go list returned %d packages; the guard below would pass on an empty list", len(deps))
	}
	for _, dep := range deps {
		switch {
		case dep == "net" || strings.HasPrefix(dep, "net/"):
			t.Errorf("internal/trajectory reaches %s: nothing it reads may leave the machine", dep)
		case dep == "os/exec":
			t.Errorf("internal/trajectory reaches os/exec: it runs nothing")
		case strings.HasSuffix(dep, "/cmd/dropin-miner") || strings.Contains(dep, "/pkg/"):
			t.Errorf("internal/trajectory reaches %s: the proof of concept stays clear of the shipping client", dep)
		}
	}
}

// writingCalls are the os functions that create, change or remove something
// on disk. Output goes to an io.Writer the caller owns; the one file this
// work ever writes is opened by cmd/trajectory-poc, at a path the operator
// names, not here.
var writingCalls = map[string]bool{
	"Create": true, "CreateTemp": true, "OpenFile": true, "WriteFile": true, "Mkdir": true,
	"MkdirAll": true, "MkdirTemp": true, "Remove": true, "RemoveAll": true, "Rename": true,
	"Truncate": true, "Chmod": true, "Chown": true, "Chtimes": true, "Symlink": true, "Link": true,
}

func TestPackageSourceHasNoFilesystemWrite(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	files, calls := 0, 0
	fset := token.NewFileSet()
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		files++
		ast.Inspect(file, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			calls++
			// os.X by package, and X on anything else: an *os.Root has the
			// same writing methods, so the receiver is not checked.
			if writingCalls[sel.Sel.Name] {
				t.Errorf("%s calls %s: internal/trajectory has no write path", name, sel.Sel.Name)
			}
			return true
		})
	}
	if files < 5 || calls < 50 {
		t.Fatalf("inspected %d files and %d selectors; the guard is not looking at the package", files, calls)
	}
}
