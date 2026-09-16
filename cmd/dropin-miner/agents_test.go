package main

// The agents command against an in-memory machine: which files it writes,
// what it merges into a host's own config, and that uninstall removes
// exactly what install wrote and nothing else.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
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
	// The paths are single-quoted from H2 on: that is POSIX quoting, where
	// nothing expands, rather than Go's %q, where $ and ` still do.
	if !strings.Contains(skill, `'/home/u/.tokendrop/bin/dropin-miner' search -config '`) || !strings.Contains(skill, `tokendrop.toml' -format model`) || !strings.Contains(skill, "name: dropin-miner") {
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
	// The matcher covers both shell tools from H3 on (#77): a search the model
	// sends through Claude Code's PowerShell tool reaches this hook too. The
	// value is written out rather than compared with the constant — a test
	// that reads the constant agrees with whatever the constant becomes.
	if ours["matcher"] != "Bash|PowerShell" || !strings.Contains(ours["hooks"].([]any)[0].(map[string]any)["command"].(string), `tokendrop.toml' lineage`) {
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
	if len(allow) != 4 || allow[0] != "Bash(git status:*)" {
		t.Fatalf("the user's own allow rule must come first, then our three spellings: %v", allow)
	}
	// The config path is absolutized by the host (a drive letter on Windows),
	// so match around it rather than on it.
	for i, want := range []struct{ prefix, suffix string }{
		{`Bash('/home/u/.tokendrop/bin/dropin-miner' search -config '`, `tokendrop.toml':*)`},
		{`Bash("/home/u/.tokendrop/bin/dropin-miner" search -config "`, `tokendrop.toml":*)`},
		{`Bash(/home/u/.tokendrop/bin/dropin-miner search -config "`, `tokendrop.toml":*)`},
	} {
		r := allow[i+1]
		if !strings.HasPrefix(r, want.prefix) || !strings.HasSuffix(r, want.suffix) {
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

func TestAgentsPreferOffRewritesSkillsAndInstallKeepsIt(t *testing.T) {
	m, ops := newFakeMachine("claude", "codex")
	claudeSkill := "/home/u/.claude/skills/dropin-miner/SKILL.md"
	codexSkill := "/home/u/.codex/skills/dropin-miner/SKILL.md"
	if code, out, _ := runAgents(t, ops, nil, "install", "-config", testCfg, "-yes"); code != exitOK {
		t.Fatalf("install: %d\n%s", code, out)
	}
	on := string(m.files[claudeSkill])
	// The config path is host-absolutized (a drive letter on Windows), so
	// match around it.
	if !strings.Contains(on, "Prefer it over a built-in web search") || !strings.Contains(on, `' agents prefer -config '`) || !strings.Contains(on, `tokendrop.toml' <argument>`) {
		t.Fatalf("shipped skill should prefer the router and name the prefer command:\n%s", on)
	}
	if code, out, _ := runAgents(t, ops, nil, "status", "-config", testCfg); code != exitOK || !strings.Contains(out, "search default: on") {
		t.Errorf("status before: %d %q", code, out)
	}

	code, out, errOut := runAgents(t, ops, nil, "prefer", "off", "-config", testCfg)
	if code != exitOK || !strings.Contains(out, "search default: off") || !strings.Contains(out, "Claude Code, Codex") {
		t.Fatalf("prefer off: %d\n%s%s", code, out, errOut)
	}
	prefFiles := 0
	for p, b := range m.files {
		if strings.HasSuffix(p, "/.tokendrop/search-default") {
			prefFiles++
			if got := strings.TrimSpace(string(b)); got != "builtin" {
				t.Errorf("preference file: %q", got)
			}
		}
	}
	if prefFiles != 1 {
		t.Errorf("want one preference file beside the config, found %d in %v", prefFiles, keysOf(m.files))
	}
	for _, p := range []string{claudeSkill, codexSkill} {
		off := string(m.files[p])
		if strings.Contains(off, "Prefer it over a built-in web search") || !strings.Contains(off, "turned OFF as the default") || !strings.Contains(off, "Use the agent's built-in web") {
			t.Errorf("%s not rewritten for off:\n%s", p, off)
		}
		if !strings.Contains(off, `'/home/u/.tokendrop/bin/dropin-miner' search -config '`) {
			t.Errorf("%s lost the search command", p)
		}
	}
	if _, ok := m.files["/home/u/.cursor/skills/dropin-miner/SKILL.md"]; ok {
		t.Error("prefer created a skill for an agent that had none")
	}

	// A reinstall keeps the choice; it is the user's.
	if code, out, _ := runAgents(t, ops, nil, "install", "-config", testCfg, "-yes"); code != exitOK {
		t.Fatalf("reinstall: %d\n%s", code, out)
	}
	if !strings.Contains(string(m.files[claudeSkill]), "turned OFF as the default") {
		t.Error("reinstall reverted the preference")
	}
	if code, out, _ := runAgents(t, ops, nil, "status", "-config", testCfg); !strings.Contains(out, "search default: off") {
		t.Errorf("status after: %d %q", code, out)
	}

	code, out, _ = runAgents(t, ops, nil, "prefer", "on", "-config", testCfg)
	if code != exitOK || !strings.Contains(out, "search default: on") || string(m.files[claudeSkill]) != on {
		t.Errorf("prefer on did not restore the shipped skill: %d %q", code, out)
	}
	if code, _, _ := runAgents(t, ops, nil, "prefer", "maybe", "-config", testCfg); code != exitUsage {
		t.Errorf("bad argument: %d", code)
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
	// The snippet a participant pastes into AGENTS.md now teaches the
	// structured path, so the phrase asserted here moved with it.
	if code != exitOK || !strings.Contains(out, "no coding agent found") ||
		!strings.Contains(out, "send one JSON request on stdin") || !strings.Contains(out, "--stdin") {
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
	if !strings.Contains(out, "Cursor       found: cursor on PATH      not installed") || !strings.Contains(out, "Claude Code  not found                  not installed") {
		t.Fatalf("status:\n%s", out)
	}
	_, _, _ = runAgents(t, ops, nil, "install", "-config", testCfg, "-yes")
	_, out, _ = runAgents(t, ops, nil, "status", "-config", testCfg)
	if !strings.Contains(out, "Cursor       found: cursor on PATH      installed (skill+hooks)") {
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

// TestAgentsUninstallSparesAHookEntryForADifferentInstallationOfTheSameName
// probes the boundary TestAgentsUninstallRemovesOnlyWhatInstallWrote does
// not: entryIsOurs/ruleIsOurs decide "ours" by strings.Contains(command,
// bin), where bin is THIS process's full absolute path (ops.executable()).
// A hook or allow rule left by a DIFFERENT dropin-miner installation — a
// stale entry from before a reinstall moved the binary, or a second copy
// entirely, sharing only the basename — must survive uninstall exactly
// like any other foreign entry. If the match were ever loosened to the
// basename alone, this is the entry that would start disappearing.
func TestAgentsUninstallSparesAHookEntryForADifferentInstallationOfTheSameName(t *testing.T) {
	m, ops := newFakeMachine("claude")
	other := "/opt/other-vendor/dropin-miner"
	m.files["/home/u/.claude/settings.json"] = []byte(fmt.Sprintf(`{
		"permissions":{"allow":["Bash(%s search:*)"]},
		"hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":%q}]}]}
	}`, other, other+" hook lineage"))
	if code, out, _ := runAgents(t, ops, nil, "install", "-config", testCfg, "-yes"); code != exitOK {
		t.Fatalf("install: %d\n%s", code, out)
	}
	code, out, _ := runAgents(t, ops, nil, "uninstall", "-config", testCfg, "-yes")
	if code != exitOK {
		t.Fatalf("uninstall: %d\n%s", code, out)
	}
	claude := hooksOf(t, m, "/home/u/.claude/settings.json")
	pre, _ := claude["PreToolUse"].([]any)
	if len(pre) != 1 {
		t.Fatalf("a different installation's PreToolUse group was removed: %v", pre)
	}
	if got := pre[0].(map[string]any)["hooks"].([]any)[0].(map[string]any)["command"]; got != other+" hook lineage" {
		t.Errorf("the foreign group survived but was altered: %v", got)
	}
	allow := allowOf(t, m, "/home/u/.claude/settings.json")
	found := false
	for _, r := range allow {
		if strings.Contains(r, other) {
			found = true
		}
	}
	if !found {
		t.Errorf("a different installation's allow rule was removed: %v", allow)
	}
}

// TestAgentsHookAndAllowRuleMatchingSurvivesAWindowsStyleBinaryPath guards
// entryIsOurs and ruleIsOurs against the defect found on PR #51's Windows
// run: both used strings.Contains(command, bin), but binEntry's commands
// and claudeAllowRules' quoted rule are written with %q, which doubles
// every backslash. On a Windows path like the one used here, bin's own
// single-backslash bytes never occur as a contiguous run inside the
// quoted text, so the substring test never matched — a second install
// duplicated every Claude and Cursor hook, and uninstall removed none of
// them (nor the quoted half of the Claude allow rules). This runs on
// every OS: the path is only ever a string embedded in JSON, nothing
// executes it.
func TestAgentsHookAndAllowRuleMatchingSurvivesAWindowsStyleBinaryPath(t *testing.T) {
	m, ops := newFakeMachine("claude", "cursor")
	ops.executable = func() (string, error) { return `C:\Users\u\.tokendrop\bin\dropin-miner.exe`, nil }

	for i := 1; i <= 2; i++ {
		if code, out, errOut := runAgents(t, ops, nil, "install", "-config", testCfg, "-yes"); code != exitOK {
			t.Fatalf("install #%d: exit %d\n%s%s", i, code, out, errOut)
		}
	}

	settings := "/home/u/.claude/settings.json"
	claudeHooksAfter := hooksOf(t, m, settings)
	for _, ev := range []string{"PreToolUse", "SessionStart", "PreCompact", "PostCompact", "Stop"} {
		list, _ := claudeHooksAfter[ev].([]any)
		if len(list) != 1 {
			t.Errorf("Claude %s after two installs: want 1 entry, got %d: %v", ev, len(list), list)
		}
	}
	// Three spellings of the same prefix rule, and still three after a
	// second install: the single-quoted path the skill now renders, and
	// v0.2.9's %q-quoted and bare ones, which an agent may still be
	// repeating from a skill it read before the upgrade.
	if allow := allowOf(t, m, settings); len(allow) != 3 {
		t.Fatalf("Claude allow rules after two installs: want 3, got %d: %v", len(allow), allow)
	}

	cursorPath := "/home/u/.cursor/hooks.json"
	cursorHooksAfter := hooksOf(t, m, cursorPath)
	for _, ev := range []string{"sessionStart", "beforeShellExecution", "afterAgentThought", "afterAgentResponse", "preCompact", "stop"} {
		list, _ := cursorHooksAfter[ev].([]any)
		if len(list) != 1 {
			t.Errorf("Cursor %s after two installs: want 1 entry, got %d: %v", ev, len(list), list)
		}
	}

	if code, out, errOut := runAgents(t, ops, nil, "uninstall", "-config", testCfg, "-yes"); code != exitOK {
		t.Fatalf("uninstall: exit %d\n%s%s", code, out, errOut)
	}
	// Both files held nothing but what this client wrote — every hook entry,
	// every allow rule, and the `version` key install adds — so from H5 they
	// are removed rather than left as an empty shell. Leaving ~/.cursor/
	// hooks.json behind as `{"hooks":{},"version":1}` is what made a machine
	// that never had Cursor detect as Cursor forever after (H4's leftover).
	for _, path := range []string{settings, cursorPath} {
		if _, ok := m.files[path]; ok {
			t.Errorf("%s held nothing but this installation's entries and was not removed: %s", path, m.files[path])
		}
	}
}

func keysOf(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// wantHookCommand is what this client would write for tg's hook event on
// this OS, through the same renderer the installer uses.
func wantHookCommand(t *testing.T, tg installTarget, e binEntry, sub ...string) string {
	t.Helper()
	shells, err := declaredShells(tg, runtime.GOOS, channelHook)
	if err != nil {
		t.Fatalf("%s hook shells on %s: %v", tg.ID(), runtime.GOOS, err)
	}
	cmd, _, err := e.hookCommandForRunners(shells, sub...)
	if err != nil {
		t.Fatalf("%s hook command for %v: %v", tg.ID(), shells, err)
	}
	return cmd
}

// TestEveryHookSpellingIsReplacedOnInstallAndRemovedOnUninstall seeds both
// hook files with an entry in every spelling this client has ever written a
// hook command in, then requires install to leave exactly one — the current
// rendering — and uninstall to leave none.
//
// No other test covered this. Every other one installs into a file this
// client wrote itself, so the recognizer was only ever fed the spelling of
// the version under test. The spelling that matters is the one already on
// disk: an installation upgraded from v0.2.9 carries %q entries, and those
// are exactly the ones that do not parse in PowerShell (#69). Until this
// commit install saw its own binary in them, decided there was nothing to
// do, and the fix never reached an upgraded machine.
//
// The Windows-style path is deliberate: with a POSIX path, %q and the cmd
// double-quoted form are the same string and would not be two spellings.
// Nothing here executes the path; it is a string inside JSON on every OS.
func TestEveryHookSpellingIsReplacedOnInstallAndRemovedOnUninstall(t *testing.T) {
	const bin = `C:\Users\u\.tokendrop\bin\dropin-miner.exe`
	// The installer resolves -config with filepath.Abs before it renders
	// anything, so on Windows the path it writes is rooted on the runner's
	// current drive (D:\home\u\... on the CI image) and is spelled with
	// backslashes. The expectation has to come from the same resolution
	// rather than from the literal flag value: building it from the literal
	// is how the first version of this test failed on both Windows runners
	// while the part it exists to check — the quoting — was already right.
	cfg, err := filepath.Abs(testCfg)
	if err != nil {
		t.Fatal(err)
	}
	entry := binEntry{command: bin, cfg: cfg}
	spellings := func(sub string) []string {
		return []string{
			strconv.Quote(bin) + " hook -config " + strconv.Quote(cfg) + " " + sub,
			bin + " hook -config " + cfg + " " + sub,
			posixQuoteArg(bin) + " hook -config " + posixQuoteArg(cfg) + " " + sub,
			"& " + powerShellQuoteArg(bin) + " hook -config " + powerShellQuoteArg(cfg) + " " + sub,
			`"` + bin + `" hook -config "` + cfg + `" ` + sub,
		}
	}
	// Five spellings must be five distinct strings, or this test is weaker
	// than it reads.
	seen := map[string]bool{}
	for _, s := range spellings("flush") {
		if seen[s] {
			t.Fatalf("two spellings are the same string: %q", s)
		}
		seen[s] = true
	}

	m, ops := newFakeMachine("claude", "cursor")
	ops.executable = func() (string, error) { return bin, nil }

	claudeStop := []any{map[string]any{"hooks": []any{map[string]any{"type": "command", "command": "say done"}}}}
	for _, c := range spellings("flush") {
		claudeStop = append(claudeStop, map[string]any{"hooks": []any{map[string]any{"type": "command", "command": c}}})
	}
	cursorStop := []any{map[string]any{"command": "./hooks/mine.sh"}}
	for _, c := range spellings("cursor stop") {
		cursorStop = append(cursorStop, map[string]any{"command": c})
	}
	seed := func(path string, doc map[string]any) {
		b, err := json.Marshal(doc)
		if err != nil {
			t.Fatal(err)
		}
		m.files[path] = b
	}
	const settings, cursorPath = "/home/u/.claude/settings.json", "/home/u/.cursor/hooks.json"
	seed(settings, map[string]any{"hooks": map[string]any{"Stop": claudeStop}})
	seed(cursorPath, map[string]any{"version": 1, "hooks": map[string]any{"stop": cursorStop}})

	claude, _ := targetByID(installTargets, "claude")
	cursor, _ := targetByID(installTargets, "cursor")

	// Twice: the first install replaces five stale entries with one, the
	// second must find nothing left to do.
	for i := 1; i <= 2; i++ {
		if code, out, errOut := runAgents(t, ops, nil, "install", "-config", testCfg, "-yes"); code != exitOK {
			t.Fatalf("install #%d: exit %d\n%s%s", i, code, out, errOut)
		}
		for _, c := range []struct {
			what, path, event string
			want, user        string
			command           func(any) string
		}{
			{"Claude", settings, "Stop", wantHookCommand(t, claude, entry, "flush"), "say done", claudeGroupCommand},
			{"Cursor", cursorPath, "stop", wantHookCommand(t, cursor, entry, "cursor", "stop"), "./hooks/mine.sh", cursorEntryCommand},
		} {
			list, _ := hooksOf(t, m, c.path)[c.event].([]any)
			if len(list) != 2 {
				t.Fatalf("%s %s after install #%d: want the user's entry and exactly one of ours, got %d: %v",
					c.what, c.event, i, len(list), list)
			}
			if got := c.command(list[0]); got != c.user {
				t.Errorf("%s %s: the user's own entry was disturbed: %q", c.what, c.event, got)
			}
			if got := c.command(list[1]); got != c.want {
				t.Errorf("%s %s after install #%d:\n got %q\nwant %q", c.what, c.event, i, got, c.want)
			}
		}
	}

	if code, out, errOut := runAgents(t, ops, nil, "uninstall", "-config", testCfg, "-yes"); code != exitOK {
		t.Fatalf("uninstall: exit %d\n%s%s", code, out, errOut)
	}
	for _, c := range []struct {
		what, path, event, user string
		command                 func(any) string
	}{
		{"Claude", settings, "Stop", "say done", claudeGroupCommand},
		{"Cursor", cursorPath, "stop", "./hooks/mine.sh", cursorEntryCommand},
	} {
		list, _ := hooksOf(t, m, c.path)[c.event].([]any)
		if len(list) != 1 || c.command(list[0]) != c.user {
			t.Errorf("%s %s after uninstall: want only the user's %q, got %v", c.what, c.event, c.user, list)
		}
	}
}

func claudeGroupCommand(e any) string {
	g, _ := e.(map[string]any)
	hs, _ := g["hooks"].([]any)
	if len(hs) == 0 {
		return ""
	}
	h, _ := hs[0].(map[string]any)
	s, _ := h["command"].(string)
	return s
}

func cursorEntryCommand(e any) string {
	h, _ := e.(map[string]any)
	s, _ := h["command"].(string)
	return s
}

// TestInstallUpgradesAV029InstallationInPlace is the upgrade a real machine
// actually performs: exactly ONE v0.2.9 entry per event, in v0.2.9's %q
// spelling, plus v0.2.9's two allow rules.
//
// TestEveryHookSpellingIsReplacedOnInstallAndRemovedOnUninstall cannot see
// what this sees. It seeds every spelling at once, so the merge loop replaces
// because it counted more than one entry of ours — never because it compared
// the one it found against what it would write now. A sameJSONValue that
// answered "the same" for any two entries leaves that test green and leaves
// every upgraded installation with the hook that does not parse in
// PowerShell (#69). Here there is exactly one entry and it is stale, so the
// replacement happens only if the comparison is real.
//
// The Windows-style binary path makes the stale spelling differ from the
// current rendering on every runner: %q doubles the backslashes, and no
// shell's quoting does.
func TestInstallUpgradesAV029InstallationInPlace(t *testing.T) {
	const bin = `C:\Users\u\.tokendrop\bin\dropin-miner.exe`
	cfg, err := filepath.Abs(testCfg)
	if err != nil {
		t.Fatal(err)
	}
	entry := binEntry{command: bin, cfg: cfg}
	v029 := func(sub ...string) string {
		return strconv.Quote(bin) + " hook -config " + strconv.Quote(cfg) + " " + strings.Join(sub, " ")
	}

	claudeEvents := []struct {
		event string
		sub   []string
	}{
		{"PreToolUse", []string{"lineage"}},
		{"SessionStart", []string{"window", "session-start"}},
		{"PreCompact", []string{"window", "pre-compact"}},
		{"PostCompact", []string{"window", "post-compact"}},
		{"Stop", []string{"flush"}},
	}
	cursorEvents := []string{"sessionStart", "beforeShellExecution", "afterAgentThought", "afterAgentResponse", "preCompact", "stop"}

	m, ops := newFakeMachine("claude", "cursor")
	ops.executable = func() (string, error) { return bin, nil }

	// v0.2.9's settings.json: one group per event, PreToolUse still matching
	// Bash alone, and one hook of the participant's own on two events.
	claudeHooksSeed := map[string]any{}
	for _, ce := range claudeEvents {
		group := map[string]any{"hooks": []any{map[string]any{"type": "command", "command": v029(ce.sub...)}}}
		if ce.event == "PreToolUse" {
			group["matcher"] = "Bash"
		}
		list := []any{group}
		if ce.event == "PreToolUse" || ce.event == "Stop" {
			list = append(list, map[string]any{"hooks": []any{map[string]any{"type": "command", "command": "the participant's own " + ce.event}}})
		}
		claudeHooksSeed[ce.event] = list
	}
	cursorHooksSeed := map[string]any{}
	for _, ev := range cursorEvents {
		list := []any{map[string]any{"command": v029("cursor", ev)}}
		if ev == "stop" {
			list = append(list, map[string]any{"command": "./hooks/mine.sh"})
		}
		cursorHooksSeed[ev] = list
	}
	seed := func(path string, doc map[string]any) {
		b, err := json.Marshal(doc)
		if err != nil {
			t.Fatal(err)
		}
		m.files[path] = b
	}
	const settings, cursorPath = "/home/u/.claude/settings.json", "/home/u/.cursor/hooks.json"
	seed(settings, map[string]any{
		"hooks": claudeHooksSeed,
		"permissions": map[string]any{"allow": []any{
			fmt.Sprintf("Bash(%q search -config %q:*)", bin, cfg),
			fmt.Sprintf("Bash(%s search -config %q:*)", bin, cfg),
			"Bash(git status:*)",
		}},
	})
	seed(cursorPath, map[string]any{"version": 1, "hooks": cursorHooksSeed})

	if code, out, errOut := runAgents(t, ops, nil, "install", "-config", testCfg, "-yes"); code != exitOK {
		t.Fatalf("install: exit %d\n%s%s", code, out, errOut)
	}

	claude, _ := targetByID(installTargets, "claude")
	cursor, _ := targetByID(installTargets, "cursor")

	claudeAfter := hooksOf(t, m, settings)
	for _, ce := range claudeEvents {
		list, _ := claudeAfter[ce.event].([]any)
		want := wantHookCommand(t, claude, entry, ce.sub...)
		var ours, foreign int
		for _, e := range list {
			switch got := claudeGroupCommand(e); {
			case got == want:
				ours++
			case got == v029(ce.sub...):
				t.Errorf("Claude %s: the v0.2.9 entry was left exactly as it was: %q", ce.event, got)
			default:
				foreign++
			}
		}
		if ours != 1 {
			t.Errorf("Claude %s: %d entries carry the current rendering %q, want 1: %v", ce.event, ours, want, list)
		}
		wantForeign := 0
		if ce.event == "PreToolUse" || ce.event == "Stop" {
			wantForeign = 1
		}
		if foreign != wantForeign {
			t.Errorf("Claude %s: %d foreign entries, want %d: %v", ce.event, foreign, wantForeign, list)
		}
	}
	// The replaced PreToolUse group carries the matcher this version writes,
	// not v0.2.9's Bash-only one (#77).
	for _, e := range claudeAfter["PreToolUse"].([]any) {
		g, _ := e.(map[string]any)
		if claudeGroupCommand(e) == wantHookCommand(t, claude, entry, "lineage") && g["matcher"] != claudeToolMatcher {
			t.Errorf("the replaced PreToolUse group still matches %v", g["matcher"])
		}
	}

	cursorAfter := hooksOf(t, m, cursorPath)
	for _, ev := range cursorEvents {
		list, _ := cursorAfter[ev].([]any)
		want := wantHookCommand(t, cursor, entry, "cursor", ev)
		var ours, foreign int
		for _, e := range list {
			switch got := cursorEntryCommand(e); {
			case got == want:
				ours++
			case got == v029("cursor", ev):
				t.Errorf("Cursor %s: the v0.2.9 entry was left exactly as it was: %q", ev, got)
			default:
				foreign++
			}
		}
		if ours != 1 {
			t.Errorf("Cursor %s: %d entries carry the current rendering %q, want 1: %v", ev, ours, want, list)
		}
		wantForeign := 0
		if ev == "stop" {
			wantForeign = 1
		}
		if foreign != wantForeign {
			t.Errorf("Cursor %s: %d foreign entries, want %d: %v", ev, foreign, wantForeign, list)
		}
	}

	// Every rule this version writes is present, the participant's own is
	// untouched, and nothing is duplicated.
	allow := allowOf(t, m, settings)
	for _, rule := range claudeAllowRules(entry) {
		if n := countString(allow, rule); n != 1 {
			t.Errorf("allow rule %q appears %d times, want 1: %v", rule, n, allow)
		}
	}
	if countString(allow, "Bash(git status:*)") != 1 {
		t.Errorf("the participant's own allow rule was disturbed: %v", allow)
	}

	// And the upgrade settles: a second install writes nothing at all.
	before, beforeCursor := string(m.files[settings]), string(m.files[cursorPath])
	if code, out, errOut := runAgents(t, ops, nil, "install", "-config", testCfg, "-yes"); code != exitOK {
		t.Fatalf("second install: exit %d\n%s%s", code, out, errOut)
	}
	if string(m.files[settings]) != before {
		t.Errorf("a second install rewrote settings.json:\n got %s\nwant %s", m.files[settings], before)
	}
	if string(m.files[cursorPath]) != beforeCursor {
		t.Errorf("a second install rewrote hooks.json:\n got %s\nwant %s", m.files[cursorPath], beforeCursor)
	}
}

func countString(list []string, want string) int {
	n := 0
	for _, s := range list {
		if s == want {
			n++
		}
	}
	return n
}
