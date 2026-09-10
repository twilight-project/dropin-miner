package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func TestCursorInstalledCommands(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	installed := filepath.Join(root, "installed space", "dropin-miner")
	if err := os.MkdirAll(filepath.Dir(installed), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(installed, []byte("synthetic installed identity"), 0600); err != nil {
		t.Fatal(err)
	}
	// The identity seam models the installed hook; comparison uses real files.
	entry := binEntry{command: installed, cfg: filepath.Join(root, "config space", "tokendrop.toml")}
	search := entry.searchCommand() + ` "ordinary query"`
	foreign := filepath.Join(root, "dropin-miner")
	if err := os.WriteFile(foreign, []byte("another identity"), 0600); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name, cmd    string
		allow, stamp bool
	}{
		{"generated_search", search, true, true},
		{"generated_prefer_on", entry.preferCommand() + " on", true, false},
		{"generated_prefer_off", entry.preferCommand() + " off", true, false},
		{"generated_prefer_status", entry.preferCommand() + " status", true, false},
		{"other_path", (binEntry{command: foreign}).searchCommand() + " query", false, false},
		{"other_copy", (binEntry{command: executable}).searchCommand() + " query", false, false},
		{"basename", "dropin-miner search query", false, false},
		{"semicolon", search + " ; other-command", false, false},
		{"and", search + " && other-command", false, false},
		{"background", search + " &", false, false},
		{"or", search + " || other-command", false, false},
		{"preceding", "other-command ; " + search, false, false},
		{"pipe_before", "other-command | " + search, false, false},
		{"pipe_after", search + " | other-command", false, false},
		{"redirect", search + " > output", false, false},
		{"append", search + " >> output", false, false},
		{"input", search + " < input", false, false},
		{"heredoc", search + " << EOF", false, false},
		{"substitution", search + ` "$(other-command)"`, false, false},
		{"backticks", search + " `other-command`", false, false},
		{"process_substitution", search + " <(other-command)", false, false},
		{"quoted_text", `echo '` + search + `'`, false, false},
		{"newline", search + "\nother-command", false, false},
		{"quoted_newline", search + " 'line\nline'", false, false},
		{"unclosed", search + ` "oops`, false, false},
		{"quoted_operators", entry.searchCommand() + ` 'a ; && | > $(text) ` + "`text`" + `'`, runtime.GOOS != "windows", runtime.GOOS != "windows"},
		{"double_quoted_operators", entry.searchCommand() + ` "a ; && | >"`, true, true},
		{"unknown_expansion", search + " $HOME", false, false},
		{"bare_status", fmt.Sprintf("%q status", installed), false, false},
		{"chat_not_shipped", fmt.Sprintf("%q chat x", installed), false, false},
	}
	for _, link := range []struct {
		name, target string
		allow        bool
	}{{"same_symlink", installed, true}, {"foreign_symlink", foreign, false}} {
		path := filepath.Join(root, link.name)
		if err := os.Symlink(link.target, path); err != nil {
			t.Logf("symlink case unavailable: %v", err)
			continue
		}
		tests = append(tests, struct {
			name, cmd    string
			allow, stamp bool
		}{link.name, (binEntry{command: path, cfg: entry.cfg}).searchCommand() + " query", link.allow, link.allow})
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fs, ops := newFakeHookOps(nil)
			ops.executable = func() (string, error) { return installed, nil }
			hc := hookContext{sessionsDir: "/sessions"}
			path := lineagePath(hc.sessionsDir, "/workspace")
			payload := map[string]any{"conversation_id": "conversation", "generation_id": "generation", "cwd": "/workspace", "command": tc.cmd}
			runHook(t, ops, hc, "cursor sessionStart", payload)
			original := string(fs.files[path])
			out, _ := runHook(t, ops, hc, "cursor beforeShellExecution", payload)
			if got := strings.TrimSpace(out) == `{"permission":"allow"}`; got != tc.allow || (!tc.allow && out != "") {
				t.Fatalf("permission mismatch: allow=%v output=%q", tc.allow, out)
			}
			if !tc.stamp {
				if string(fs.files[path]) != original {
					t.Fatal("non-search command changed lineage")
				}
				return
			}
			l, ok := loadLineage(ops, path)
			if !ok || l.Seq != 1 || len(l.CallID) != 32 || l.SessionID != traceHash("conversation") || l.TurnID != traceHash("conversation|generation") {
				t.Fatalf("search lineage mismatch: %+v", l)
			}
			previous := l.CallID
			runHook(t, ops, hc, "cursor beforeShellExecution", payload)
			l, ok = loadLineage(ops, path)
			if !ok || l.Seq != 2 || l.CallID == previous || len(l.CallID) != 32 {
				t.Fatalf("second search lineage mismatch: %+v", l)
			}
		})
	}
	for _, name := range []string{"unavailable", "unresolvable", "unset"} {
		t.Run(name, func(t *testing.T) {
			fs, ops := newFakeHookOps(nil)
			ops.executable = nil
			if name == "unavailable" {
				ops.executable = func() (string, error) { return "", errors.New("unavailable") }
			}
			if name == "unresolvable" {
				ops.executable = func() (string, error) { return filepath.Join(root, "absent"), nil }
			}
			out, _ := runHook(t, ops, hookContext{sessionsDir: "/sessions"}, "cursor beforeShellExecution", map[string]any{"command": search, "conversation_id": "c", "cwd": "/workspace"})
			if out != "" || len(fs.files) != 0 {
				t.Fatal("unknown identity produced a decision or lineage")
			}
		})
	}
}

func TestCursorCommandPathExtension(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	identity := func() (string, error) { return executable, nil }
	command := fmt.Sprintf("%q chat x", executable)
	paths := append([][]string{}, cursorCommandPaths...)
	paths = append(paths, []string{"chat"})
	if got := recognizeCursorCommand(command, identity, paths); len(got) != 1 || got[0] != "chat" {
		t.Fatal("synthetic path not recognized")
	}
	if recognizeCursorCommand(command+" ; other-command", identity, paths) != nil {
		t.Fatal("extension bypassed simple-command grammar")
	}
	if recognizeCursorCommand(command, identity, cursorCommandPaths) != nil {
		t.Fatal("synthetic path reached production allowlist")
	}
}

func TestCursorPlatformQuoting(t *testing.T) {
	entry := binEntry{command: `C:\Program Files\DropinMiner\dropin-miner.exe`, cfg: `C:\Config Files\tokendrop.toml`}
	args, ok := simpleCommandArgsForPlatform(entry.searchCommand()+` "normal query"`, true)
	want := []string{entry.command, "search", "-config", entry.cfg, "-format", "model", "normal query"}
	if !ok || !slices.Equal(args, want) {
		t.Fatalf("generated Windows words: %q", args)
	}
	for _, suffix := range []string{` "%VALUE%"`, ` "!VALUE!"`, ` "a\"; text"`, ` 'a ; text'`, ` ^text`} {
		command := entry.searchCommand() + suffix
		if _, ok := simpleCommandArgsForPlatform(command, true); ok {
			t.Fatal("shell-dependent Windows words recognized")
		}
	}
}
