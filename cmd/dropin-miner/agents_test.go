package main

// The agents command against an in-memory machine: which files it writes,
// what it merges into a host's own config, and that uninstall removes
// exactly what install wrote and nothing else.

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fakeFileInfo struct {
	name string
	size int64
	dir  bool
}

func (f fakeFileInfo) Name() string       { return f.name }
func (f fakeFileInfo) Size() int64        { return f.size }
func (f fakeFileInfo) Mode() os.FileMode  { return 0o600 }
func (f fakeFileInfo) ModTime() time.Time { return time.Time{} }
func (f fakeFileInfo) IsDir() bool        { return f.dir }
func (f fakeFileInfo) Sys() any           { return nil }

type fakeMachine struct {
	files    map[string][]byte
	onPath   map[string]string
	removed  []string
	terminal bool
}

// slash keys every path the same way on every OS: ops.paths joins with
// the host separator, the tests write forward slashes, and Windows would
// otherwise see two different files.
func slash(p string) string { return filepath.ToSlash(p) }

func newFakeMachine(onPath ...string) (*fakeMachine, agentOps) {
	m := &fakeMachine{files: map[string][]byte{}, onPath: map[string]string{}}
	for _, p := range onPath {
		m.onPath[p] = "/usr/local/bin/" + p
	}
	ops := agentOps{
		home: "/home/u",
		lookPath: func(name string) (string, error) {
			if p, ok := m.onPath[name]; ok {
				return p, nil
			}
			return "", errors.New("not found")
		},
		executable: func() (string, error) { return "/home/u/.tokendrop/bin/dropin-miner", nil },
		readFile: func(p string) ([]byte, error) {
			b, ok := m.files[slash(p)]
			if !ok {
				return nil, fs.ErrNotExist
			}
			return b, nil
		},
		writeFile: func(p string, b []byte, _ os.FileMode) error { m.files[slash(p)] = b; return nil },
		mkdirAll:  func(string, os.FileMode) error { return nil },
		stat: func(p string) (os.FileInfo, error) {
			p = slash(p)
			if _, ok := m.files[p]; ok {
				return fakeFileInfo{name: filepath.Base(p)}, nil
			}
			for f := range m.files {
				if strings.HasPrefix(f, p+"/") {
					return fakeFileInfo{name: filepath.Base(p), dir: true}, nil
				}
			}
			return nil, fs.ErrNotExist
		},
		removeAll: func(p string) error {
			p = slash(p)
			m.removed = append(m.removed, p)
			for f := range m.files {
				if f == p || strings.HasPrefix(f, p+"/") {
					delete(m.files, f)
				}
			}
			return nil
		},
		isTerminal: func() bool { return m.terminal },
	}
	return m, ops
}

func runAgents(t *testing.T, ops agentOps, env map[string]string, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := agentsMain(ops, args, strings.NewReader(""), &out, &errOut, envOf(env))
	return code, out.String(), errOut.String()
}

const testCfg = "/home/u/.tokendrop/tokendrop.toml"

func hooksOf(t *testing.T, m *fakeMachine, path string) map[string]any {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(m.files[path], &doc); err != nil {
		t.Fatalf("%s: %v\n%s", path, err, m.files[path])
	}
	h, _ := doc["hooks"].(map[string]any)
	return h
}

func allowOf(t *testing.T, m *fakeMachine, path string) []string {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(m.files[path], &doc); err != nil {
		t.Fatalf("%s: %v\n%s", path, err, m.files[path])
	}
	perms, _ := doc["permissions"].(map[string]any)
	list, _ := perms["allow"].([]any)
	var out []string
	for _, e := range list {
		out = append(out, e.(string))
	}
	return out
}

func TestAgentsDryRunDetectsAgentsAndWritesNothing(t *testing.T) {
	m, ops := newFakeMachine("claude", "cursor")
	code, out, _ := runAgents(t, ops, nil, "install", "-config", testCfg, "-dry-run")
	if code != exitOK {
		t.Fatalf("exit %d\n%s", code, out)
	}
	out = filepath.ToSlash(out)
	for _, want := range []string{"agents: Claude Code, Cursor", "skills/dropin-miner/SKILL.md", "settings.json", "hooks.json", "(dry run"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if len(m.files) != 0 {
		t.Errorf("dry run wrote: %v", keysOf(m.files))
	}
}

func TestAgentsInstallWritesClaudeSkillAndMergesHooksIntoSettings(t *testing.T) {
	m, ops := newFakeMachine("claude")
	settings := "/home/u/.claude/settings.json"
	m.files[settings] = []byte(`{"model":"opus","permissions":{"allow":["Bash(git status:*)"]},"hooks":{"PreToolUse":[{"matcher":"Write","hooks":[{"type":"command","command":"lint"}]}]}}`)
	code, out, errOut := runAgents(t, ops, nil, "install", "-config", testCfg, "-yes")
	if code != exitOK {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	skill := string(m.files["/home/u/.claude/skills/dropin-miner/SKILL.md"])
	if !strings.Contains(skill, `"/home/u/.tokendrop/bin/dropin-miner" search -config "`) || !strings.Contains(skill, `tokendrop.toml" -format model`) || !strings.Contains(skill, "name: dropin-miner") {
		t.Errorf("skill:\n%s", skill)
	}
	var doc map[string]any
	_ = json.Unmarshal(m.files[settings], &doc)
	if doc["model"] != "opus" {
		t.Error("an unrelated setting was lost")
	}
	hooks := hooksOf(t, m, settings)
	pre := hooks["PreToolUse"].([]any)
	if len(pre) != 2 || pre[0].(map[string]any)["matcher"] != "Write" {
		t.Fatalf("existing PreToolUse group not preserved first: %v", pre)
	}
	ours := pre[1].(map[string]any)
	if ours["matcher"] != "Bash" || !strings.Contains(ours["hooks"].([]any)[0].(map[string]any)["command"].(string), `tokendrop.toml" lineage`) {
		t.Errorf("our PreToolUse group: %v", ours)
	}
	for _, ev := range []string{"SessionStart", "PreCompact", "PostCompact", "Stop"} {
		if _, ok := hooks[ev]; !ok {
			t.Errorf("no %s hook", ev)
		}
	}
	if bytes.Contains(m.files[settings], []byte("sr-")) || bytes.Contains(m.files[settings], []byte("TOKENDROP_API_KEY")) {
		t.Error("a key reached settings.json")
	}
	allow := allowOf(t, m, settings)
	if len(allow) != 3 || allow[0] != "Bash(git status:*)" {
		t.Fatalf("the user's own allow rule must come first, then ours: %v", allow)
	}
	// The config path is absolutized by the host (a drive letter on Windows),
	// so match around it rather than on it.
	for i, prefix := range []string{`Bash("/home/u/.tokendrop/bin/dropin-miner" search -config "`, `Bash(/home/u/.tokendrop/bin/dropin-miner search -config "`} {
		r := allow[i+1]
		if !strings.HasPrefix(r, prefix) || !strings.HasSuffix(r, `tokendrop.toml":*)`) {
			t.Errorf("allow rule %d: %s", i+1, r)
		}
	}
	for _, r := range allow[1:] {
		if strings.Contains(r, "hook") || strings.Contains(r, "flush") || strings.Contains(r, "wallet") {
			t.Errorf("a rule allows more than search: %s", r)
		}
	}

	// Idempotent: a second install changes nothing.
	before := string(m.files[settings])
	code, out, _ = runAgents(t, ops, nil, "install", "-config", testCfg, "-yes")
	if code != exitOK || !strings.Contains(out, "already installed") || string(m.files[settings]) != before {
		t.Errorf("second install: exit %d\n%s", code, out)
	}
}

func TestAgentsInstallWritesCursorSkillAndHooksFileWithVersion(t *testing.T) {
	m, ops := newFakeMachine("cursor")
	code, out, _ := runAgents(t, ops, nil, "install", "-config", testCfg, "-yes")
	if code != exitOK {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if _, ok := m.files["/home/u/.cursor/skills/dropin-miner/SKILL.md"]; !ok {
		t.Error("no cursor skill")
	}
	hooksPath := "/home/u/.cursor/hooks.json"
	var doc map[string]any
	if err := json.Unmarshal(m.files[hooksPath], &doc); err != nil {
		t.Fatalf("hooks.json: %v", err)
	}
	if doc["version"] != float64(1) {
		t.Errorf("version: %v", doc["version"])
	}
	hooks := hooksOf(t, m, hooksPath)
	for _, ev := range []string{"sessionStart", "beforeShellExecution", "afterAgentThought", "afterAgentResponse", "preCompact", "stop"} {
		list, _ := hooks[ev].([]any)
		if len(list) != 1 {
			t.Errorf("%s: %v", ev, list)
			continue
		}
		cmd := list[0].(map[string]any)["command"].(string)
		if !strings.Contains(cmd, "hook -config") || !strings.HasSuffix(cmd, "cursor "+ev) {
			t.Errorf("%s command: %q", ev, cmd)
		}
	}
}

func TestAgentsInstallRefusesAHooksFileItCannotParse(t *testing.T) {
	m, ops := newFakeMachine("cursor")
	m.files["/home/u/.cursor/hooks.json"] = []byte("{ // a comment\n \"version\": 1 }")
	code, out, _ := runAgents(t, ops, nil, "install", "-config", testCfg, "-yes")
	if code != exitTransport || !strings.Contains(out, "refused: Cursor") || !strings.Contains(out, "not plain JSON") {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if !bytes.Contains(m.files["/home/u/.cursor/hooks.json"], []byte("// a comment")) {
		t.Error("the file was rewritten")
	}
	if _, ok := m.files["/home/u/.cursor/skills/dropin-miner/SKILL.md"]; !ok {
		t.Error("the skill, which needed no merge, was withheld")
	}
}

func TestAgentsUninstallRemovesOnlyWhatInstallWrote(t *testing.T) {
	m, ops := newFakeMachine("claude", "cursor", "codex", "opencode")
	m.files["/home/u/.claude/settings.json"] = []byte(`{"permissions":{"allow":["Bash(git status:*)"]},"hooks":{"PreToolUse":[{"matcher":"Write","hooks":[{"type":"command","command":"lint"}]}],"Stop":[{"hooks":[{"type":"command","command":"say done"}]}]}}`)
	m.files["/home/u/.cursor/hooks.json"] = []byte(`{"version":1,"hooks":{"stop":[{"command":"./hooks/mine.sh"}]}}`)
	if code, out, _ := runAgents(t, ops, nil, "install", "-config", testCfg, "-yes"); code != exitOK {
		t.Fatalf("install: %d\n%s", code, out)
	}
	code, out, _ := runAgents(t, ops, nil, "uninstall", "-config", testCfg, "-yes")
	if code != exitOK {
		t.Fatalf("uninstall: %d\n%s", code, out)
	}
	for _, gone := range []string{
		"/home/u/.claude/skills/dropin-miner/SKILL.md",
		"/home/u/.cursor/skills/dropin-miner/SKILL.md",
		"/home/u/.codex/skills/dropin-miner/SKILL.md",
		"/home/u/.config/opencode/plugins/dropin-miner.js",
	} {
		if _, ok := m.files[gone]; ok {
			t.Errorf("still present: %s", gone)
		}
	}
	claude := hooksOf(t, m, "/home/u/.claude/settings.json")
	if pre := claude["PreToolUse"].([]any); len(pre) != 1 || pre[0].(map[string]any)["matcher"] != "Write" {
		t.Errorf("Claude PreToolUse after uninstall: %v", pre)
	}
	if stop := claude["Stop"].([]any); len(stop) != 1 {
		t.Errorf("the user's own Stop hook was touched: %v", stop)
	}
	if allow := allowOf(t, m, "/home/u/.claude/settings.json"); len(allow) != 1 || allow[0] != "Bash(git status:*)" {
		t.Errorf("allow rules after uninstall: %v", allow)
	}
	for _, ev := range []string{"SessionStart", "PreCompact", "PostCompact"} {
		if _, ok := claude[ev]; ok {
			t.Errorf("Claude %s not removed", ev)
		}
	}
	cursor := hooksOf(t, m, "/home/u/.cursor/hooks.json")
	if stop := cursor["stop"].([]any); len(stop) != 1 || stop[0].(map[string]any)["command"] != "./hooks/mine.sh" {
		t.Errorf("the user's own Cursor stop hook was touched: %v", stop)
	}
	if _, ok := cursor["sessionStart"]; ok {
		t.Error("Cursor sessionStart not removed")
	}
	for _, r := range m.removed {
		if !strings.Contains(r, "dropin-miner") {
			t.Errorf("removed something not ours: %s", r)
		}
	}
}

func TestAgentsRefusesToWriteWithoutATerminalOrYes(t *testing.T) {
	m, ops := newFakeMachine("claude")
	code, _, errOut := runAgents(t, ops, nil, "install", "-config", testCfg)
	if code != exitUsage || !strings.Contains(errOut, "-yes") || len(m.files) != 0 {
		t.Fatalf("exit %d err %q files %v", code, errOut, keysOf(m.files))
	}
}

func TestAgentsPrintsRulesWhenNoAgentIsFoundAndClientOverridesDetection(t *testing.T) {
	m, ops := newFakeMachine()
	code, out, _ := runAgents(t, ops, nil, "install", "-config", testCfg, "-yes")
	if code != exitOK || !strings.Contains(out, "no coding agent found") || !strings.Contains(out, "For public-web search, run:") {
		t.Fatalf("exit %d\n%s", code, out)
	}
	code, out, _ = runAgents(t, ops, nil, "install", "-config", testCfg, "-yes", "-client", "codex")
	if code != exitOK {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if _, ok := m.files["/home/u/.codex/skills/dropin-miner/SKILL.md"]; !ok {
		t.Error("-client codex did not install the codex skill")
	}
	if !strings.Contains(out, "sandboxed") {
		t.Error("the Codex network caveat was not printed")
	}
}

func TestAgentsStatusReportsPathAndInstallState(t *testing.T) {
	m, ops := newFakeMachine("cursor")
	_, out, _ := runAgents(t, ops, nil, "status", "-config", testCfg)
	if !strings.Contains(out, "Cursor       on PATH      not installed") || !strings.Contains(out, "Claude Code  not on PATH  not installed") {
		t.Fatalf("status:\n%s", out)
	}
	_, _, _ = runAgents(t, ops, nil, "install", "-config", testCfg, "-yes")
	_, out, _ = runAgents(t, ops, nil, "status", "-config", testCfg)
	if !strings.Contains(out, "Cursor       on PATH      installed (skill+hooks)") {
		t.Fatalf("status after install:\n%s", out)
	}
	_ = m
}

func TestAgentsOpencodePluginRewritesOurCommandOnly(t *testing.T) {
	m, ops := newFakeMachine("opencode")
	if code, out, _ := runAgents(t, ops, nil, "install", "-config", testCfg, "-yes"); code != exitOK {
		t.Fatalf("exit %d\n%s", code, out)
	}
	js := string(m.files["/home/u/.config/opencode/plugins/dropin-miner.js"])
	for _, want := range []string{`input.tool !== "bash"`, "TOKENDROP_TRACE_BRIDGE=", "tokendrop-trace-v1|", "export const DropinMinerLineage"} {
		if !strings.Contains(js, want) {
			t.Errorf("plugin lacks %q", want)
		}
	}
	if strings.Contains(js, "{{") {
		t.Error("an unexpanded placeholder is in the plugin")
	}
}

func keysOf(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
