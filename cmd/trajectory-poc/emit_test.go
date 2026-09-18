package main

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const emitFixtureDir = "../../internal/trajectory/testdata/emit"

// redirectHome points every way of finding a home directory at a temporary
// one. A test that ran emit against the real home would read the operator's
// real config and consent records, and an in-process test here once wrote
// into a real user directory for want of exactly this.
func redirectHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	return home
}

func TestEmitWithNoConfigAndNoConsentWritesLevelOneAndSaysSo(t *testing.T) {
	redirectHome(t)
	before := snapshot(t, emitFixtureDir)
	output := filepath.Join(t.TempDir(), "records.jsonl")
	var stdout, stderr bytes.Buffer
	if code := run([]string{"emit", emitFixtureDir, output}, &stdout, &stderr); code != exitOK {
		t.Fatalf("emit exited %d: %s", code, stderr.String())
	}
	if stdout.Len() != 0 {
		t.Errorf("emit wrote to stdout: %q", stdout.String())
	}
	data, err := os.ReadFile(output) // #nosec G304 -- the test's own output file
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 3 {
		t.Fatalf("records = %d, want 3", len(lines))
	}
	for _, line := range lines {
		if !strings.Contains(line, `"level":1`) || !strings.Contains(line, `"gates":[]`) {
			t.Errorf("a default record is not level 1 with no gates open: %s", line)
		}
	}
	if strings.Contains(string(data), "ZEBRA-") || strings.Contains(string(data), "example.test") {
		t.Fatalf("the default output carries content:\n%s", data)
	}
	report := stderr.String()
	for _, want := range []string{"no config at", "every record is level 1", "level_2", "level_3", "prompts", "responses", "tool_details", "tool_content", "host_and_model", "workspace", "config, consent"} {
		if !strings.Contains(report, want) {
			t.Errorf("stderr lacks %q:\n%s", want, report)
		}
	}
	if strings.Contains(report, "ZEBRA-") {
		t.Fatalf("the report carries content:\n%s", report)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(output)
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("output mode = %o, want 600", perm)
		}
	}
	after := snapshot(t, emitFixtureDir)
	for p, was := range before {
		if after[p] != was {
			t.Errorf("emit changed %s\n  before: %s\n  after:  %s", p, was, after[p])
		}
	}
	if len(after) != len(before) {
		t.Errorf("emit changed the directory it read: %d entries before, %d after", len(before), len(after))
	}
}

func TestEmitRefusesToWriteWhereAHostOrAConfigLives(t *testing.T) {
	home := redirectHome(t)
	transcripts := t.TempDir()
	for _, dir := range []string{".claude/projects", ".tokendrop", ".trajectory-poc/consent", ".config/x"} {
		if err := os.MkdirAll(filepath.Join(home, filepath.FromSlash(dir)), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for name, output := range map[string]string{
		"inside the directory being read": filepath.Join(transcripts, "out.jsonl"),
		"inside a host's directory":       filepath.Join(home, ".claude", "projects", "out.jsonl"),
		"inside the client's directory":   filepath.Join(home, ".tokendrop", "out.jsonl"),
		"inside its own consent store":    filepath.Join(home, ".trajectory-poc", "consent", "out.jsonl"),
		"inside a config directory":       filepath.Join(home, ".config", "x", "out.jsonl"),
	} {
		var stdout, stderr bytes.Buffer
		if code := run([]string{"emit", transcripts, output}, &stdout, &stderr); code != exitUsage {
			t.Errorf("%s: emit exited %d, want %d", name, code, exitUsage)
		}
		if _, err := os.Lstat(output); err == nil {
			t.Errorf("%s: emit created %s", name, output)
		}
	}
}

func TestEmitNeverReplacesAnExistingFile(t *testing.T) {
	redirectHome(t)
	output := filepath.Join(t.TempDir(), "precious.jsonl")
	if err := os.WriteFile(output, []byte("already here\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"emit", emitFixtureDir, output}, &stdout, &stderr); code != exitError {
		t.Fatalf("emit over an existing file exited %d, want %d", code, exitError)
	}
	data, err := os.ReadFile(output) // #nosec G304 -- the test's own file
	if err != nil || string(data) != "already here\n" {
		t.Fatalf("the existing file was disturbed: %q, %v", data, err)
	}
}

// TestTheCommandCreatesExactlyOneFile holds trap 2 for this package: the one
// thing this work writes is its own output. scan's and emit's source may open
// a file for writing in one place and no other, and may not remove, rename or
// make anything.
func TestTheCommandCreatesExactlyOneFile(t *testing.T) {
	forbidden := map[string]bool{
		"Create": true, "CreateTemp": true, "WriteFile": true, "Mkdir": true, "MkdirAll": true,
		"MkdirTemp": true, "Remove": true, "RemoveAll": true, "Rename": true, "Truncate": true,
		"Chmod": true, "Chown": true, "Chtimes": true, "Symlink": true, "Link": true,
	}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	opens, files := 0, 0
	fset := token.NewFileSet()
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
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
			if forbidden[sel.Sel.Name] {
				t.Errorf("%s calls %s", name, sel.Sel.Name)
			}
			if sel.Sel.Name == "OpenFile" {
				opens++
			}
			return true
		})
	}
	if files < 2 || opens != 1 {
		t.Fatalf("inspected %d files and found %d OpenFile calls; want the one output file and nothing else", files, opens)
	}
}
