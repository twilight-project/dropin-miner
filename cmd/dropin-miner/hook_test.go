package main

// The hook command against an in-memory machine. The contract under test
// is fail-open: every path that cannot produce an exact envelope produces
// nothing, and the one that can changes exactly one thing.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fakeHookFS struct {
	files   map[string][]byte
	flushes []string
}

func newFakeHookOps(env map[string]string) (*fakeHookFS, hookOps) {
	f := &fakeHookFS{files: map[string][]byte{}}
	return f, hookOps{
		getenv: func(k string) string { return env[k] },
		readFile: func(p string) ([]byte, error) {
			b, ok := f.files[p]
			if !ok {
				return nil, fs.ErrNotExist
			}
			return b, nil
		},
		writeFile: func(p string, b []byte, _ os.FileMode) error { f.files[p] = b; return nil },
		mkdirAll:  func(string, os.FileMode) error { return nil },
		rename: func(from, to string) error {
			f.files[to] = f.files[from]
			delete(f.files, from)
			return nil
		},
		readTail: func(p string, max int64) ([]byte, error) {
			b, ok := f.files[p]
			if !ok {
				return nil, fs.ErrNotExist
			}
			if int64(len(b)) > max {
				b = b[int64(len(b))-max:]
			}
			return b, nil
		},
		spawnFlush: func(cfg string) error { f.flushes = append(f.flushes, cfg); return nil },
		now:        func() time.Time { return time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC) },
		pid:        42,
	}
}

func runHook(t *testing.T, ops hookOps, hc hookContext, sub string, payload any) (string, string) {
	t.Helper()
	b, _ := json.Marshal(payload)
	var out, errOut bytes.Buffer
	args := append([]string{}, strings.Fields(sub)...)
	_ = ops // hookMain reads config itself; tests pass sessionsDir through hc by env-less config
	code := hookMainWith(ops, hc, args, bytes.NewReader(b), &out, &errOut)
	if code != exitOK {
		t.Fatalf("hook %s exited %d: %s", sub, code, errOut.String())
	}
	return out.String(), errOut.String()
}

// hookMainWith is hookMain with the config already resolved, so tests do
// not need a TOML file on disk to name the sessions directory.
func hookMainWith(ops hookOps, hc hookContext, args []string, stdin *bytes.Reader, stdout, stderr *bytes.Buffer) int {
	payload, _ := io.ReadAll(stdin)
	if len(args) == 0 {
		return exitUsage
	}
	switch args[0] {
	case "lineage":
		hookLineage(ops, hc, payload, stdout)
	case "window":
		hookWindow(ops, hc, args[1], payload)
		if args[1] == "session-start" {
			_ = ops.spawnFlush(hc.cfgPath)
		}
	case "cursor":
		hookCursor(ops, hc, args[1], payload, stdout)
	case "flush":
		_ = ops.spawnFlush(hc.cfgPath)
	default:
		return exitUsage
	}
	return exitOK
}

func decodeBridgeFromCommand(t *testing.T, cmd string) *traceEnvelope {
	t.Helper()
	if !strings.HasPrefix(cmd, bridgeEnv+"=") {
		t.Fatalf("command not prefixed with the bridge: %q", cmd)
	}
	rest := strings.TrimPrefix(cmd, bridgeEnv+"=")
	b64 := rest[:strings.IndexByte(rest, ' ')]
	env := decodeTraceBridge(b64)
	if env == nil {
		t.Fatalf("bridge does not decode: %q", b64)
	}
	return env
}

func TestIsSearchCommandRecognisesOursAndNothingElse(t *testing.T) {
	yes := []string{
		`dropin-miner search "how do ports work"`,
		`"/Users/x y/.tokendrop/bin/dropin-miner" search -config "/a b/c.toml" -format model "q"`,
		`/usr/local/bin/dropin-miner search q`,
		`cd /tmp && dropin-miner search q`,
		`C:\Users\x\bin\dropin-miner.exe search q`,
		`& "C:\Program Files\dropin-miner.exe" search q`,
		`echo $(dropin-miner search q)`,
	}
	no := []string{
		`dropin-miner-search q`,
		`dropin-miner flush`,
		`mydropin-miner search q`,
		`grep dropin-miner search.go`,
		`tokendrop-proxy search q`,
		``,
	}
	for _, c := range yes {
		if !isSearchCommand(c) {
			t.Errorf("not recognised: %q", c)
		}
	}
	for _, c := range no {
		if isSearchCommand(c) {
			t.Errorf("wrongly recognised: %q", c)
		}
	}
}

func TestHookLineageRewritesOurShellCommandAndWritesTheLineageFile(t *testing.T) {
	fs, ops := newFakeHookOps(nil)
	transcript := strings.Join([]string{
		`{"type":"user","message":{"role":"user","content":"old turn"}}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"old prose"}]}}`,
		`{"type":"user","message":{"role":"user","content":"new turn"}}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Let me look that up."},{"type":"tool_use","id":"toolu_1","name":"Bash"}]}}`,
	}, "\n")
	fs.files["/t/s.jsonl"] = []byte(transcript)
	hc := hookContext{cfgPath: "/c.toml", sessionsDir: "/sessions"}
	cmd := `dropin-miner search -format model "how do ports work"`
	out, _ := runHook(t, ops, hc, "lineage", map[string]any{
		"session_id": "sess-1", "prompt_id": "p-1", "tool_use_id": "toolu_1", "tool_name": "Bash",
		"cwd": "/home/u/project", "transcript_path": "/t/s.jsonl",
		"tool_input": map[string]any{"command": cmd, "description": "search"},
	})
	var resp struct {
		Out struct {
			Event string         `json:"hookEventName"`
			Input map[string]any `json:"updatedInput"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(out), &resp); err != nil || resp.Out.Event != "PreToolUse" {
		t.Fatalf("output: %s", out)
	}
	if resp.Out.Input["description"] != "search" || len(resp.Out.Input) != 2 {
		t.Errorf("input not echoed exactly: %v", resp.Out.Input)
	}
	env := decodeBridgeFromCommand(t, resp.Out.Input["command"].(string))
	if !strings.HasSuffix(resp.Out.Input["command"].(string), " "+cmd) {
		t.Errorf("original command altered: %q", resp.Out.Input["command"])
	}
	if env.Harness != "claude-code" || env.SessionID == "" || env.TurnID == "" || env.CallID == "" || env.Window != "none" {
		t.Errorf("envelope ids: %+v", env)
	}
	if strings.Contains(env.SessionID, "sess-1") || strings.Contains(env.TurnID, "p-1") {
		t.Error("raw host ids leaked into the envelope")
	}
	if len(env.History) != 1 || env.History[0].Text != "Let me look that up." {
		t.Errorf("history should be the CURRENT turn's text only: %+v", env.History)
	}
	l, ok := loadLineage(ops, lineagePath("/sessions", "/home/u/project"))
	if !ok || l.SessionID != env.SessionID || l.TurnID != env.TurnID || l.Seq != 1 || len(l.History) != 1 {
		t.Fatalf("lineage file: ok=%v %+v", ok, l)
	}
}

func TestHookLineageStaysSilentWhenItShould(t *testing.T) {
	_, ops := newFakeHookOps(nil)
	hc := hookContext{sessionsDir: "/sessions"}
	cases := []struct {
		name    string
		payload any
	}{
		{"someone else's command", map[string]any{"session_id": "s", "tool_input": map[string]any{"command": "ls -la"}}},
		{"already bridged", map[string]any{"session_id": "s", "tool_input": map[string]any{"command": "TOKENDROP_TRACE_BRIDGE=abc dropin-miner search q"}}},
		{"no session id", map[string]any{"tool_input": map[string]any{"command": "dropin-miner search q"}}},
		{"no tool input", map[string]any{"session_id": "s"}},
		{"not json", "garbage"},
	}
	for _, c := range cases {
		var out bytes.Buffer
		b, _ := json.Marshal(c.payload)
		if s, ok := c.payload.(string); ok {
			b = []byte(s)
		}
		hookLineage(ops, hc, b, &out)
		if out.Len() != 0 {
			t.Errorf("%s: hook spoke: %s", c.name, out.String())
		}
	}
}

func TestHookLineageFloorsAtTheCurrentTurn(t *testing.T) {
	// A search as the FIRST action of a turn must not inherit the previous
	// turn's prose; with no user turn on the tail, send nothing.
	fs, ops := newFakeHookOps(nil)
	hc := hookContext{}
	fs.files["/t/s.jsonl"] = []byte(strings.Join([]string{
		`{"type":"user","message":{"role":"user","content":"first"}}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"prose from the previous turn"}]}}`,
		`{"type":"user","message":{"role":"user","content":"second"}}`,
	}, "\n"))
	out, _ := runHook(t, ops, hc, "lineage", map[string]any{
		"session_id": "s", "prompt_id": "p2", "tool_use_id": "t2", "transcript_path": "/t/s.jsonl",
		"tool_input": map[string]any{"command": "dropin-miner search q"},
	})
	var resp struct {
		Out struct {
			Input map[string]any `json:"updatedInput"`
		} `json:"hookSpecificOutput"`
	}
	_ = json.Unmarshal([]byte(out), &resp)
	env := decodeBridgeFromCommand(t, resp.Out.Input["command"].(string))
	if len(env.History) != 0 {
		t.Errorf("previous turn's prose leaked: %+v", env.History)
	}

	// No user turn in the tail at all: still no history.
	fs.files["/t/s.jsonl"] = []byte(`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"orphan"}]}}`)
	out, _ = runHook(t, ops, hc, "lineage", map[string]any{
		"session_id": "s", "prompt_id": "p3", "transcript_path": "/t/s.jsonl",
		"tool_input": map[string]any{"command": "dropin-miner search q"},
	})
	_ = json.Unmarshal([]byte(out), &resp)
	if env := decodeBridgeFromCommand(t, resp.Out.Input["command"].(string)); len(env.History) != 0 {
		t.Errorf("text with no turn start was sent: %+v", env.History)
	}
}

func TestHookLineageReadsTheSubagentsOwnTranscript(t *testing.T) {
	fs, ops := newFakeHookOps(nil)
	hc := hookContext{}
	fs.files["/t/orch.jsonl"] = []byte(strings.Join([]string{
		`{"type":"user","message":{"role":"user","content":"do it"}}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"spawning a subagent"}]}}`,
	}, "\n"))
	fs.files[filepath.Join("/t", "sess", "subagents", "agent-a1.jsonl")] = []byte(strings.Join([]string{
		`{"type":"user","message":{"role":"user","content":"task"}}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"the subagent's own reasoning"}]}}`,
	}, "\n"))
	out, _ := runHook(t, ops, hc, "lineage", map[string]any{
		"session_id": "sess", "prompt_id": "p", "agent_id": "a1", "transcript_path": "/t/orch.jsonl",
		"tool_input": map[string]any{"command": "dropin-miner search q"},
	})
	var resp struct {
		Out struct {
			Input map[string]any `json:"updatedInput"`
		} `json:"hookSpecificOutput"`
	}
	_ = json.Unmarshal([]byte(out), &resp)
	env := decodeBridgeFromCommand(t, resp.Out.Input["command"].(string))
	if len(env.History) != 1 || env.History[0].Text != "the subagent's own reasoning" {
		t.Errorf("subagent got the orchestrator's text: %+v", env.History)
	}
	top := traceHash("sess")
	if env.SessionID == top {
		t.Error("subagent did not get its own lane")
	}
}

func TestHookWindowCountsExactlyOnePerCompactionAndSessionStartFlushes(t *testing.T) {
	fs, ops := newFakeHookOps(nil)
	hc := hookContext{cfgPath: "/c.toml", sessionsDir: "/sessions"}
	sess := map[string]any{"session_id": "s"}
	runHook(t, ops, hc, "window session-start", sess)
	if len(fs.flushes) != 1 || fs.flushes[0] != "/c.toml" {
		t.Fatalf("session-start did not start a flush: %v", fs.flushes)
	}
	if got := hookWindowID(ops, hc, "s"); got != "none" {
		t.Fatalf("fresh window: %q", got)
	}
	runHook(t, ops, hc, "window pre-compact", sess)
	runHook(t, ops, hc, "window post-compact", sess)
	if got := hookWindowID(ops, hc, "s"); got != "1" {
		t.Errorf("after one compaction: %q", got)
	}
	runHook(t, ops, hc, "window post-compact", sess) // a host that only delivers post
	if got := hookWindowID(ops, hc, "s"); got != "2" {
		t.Errorf("after a post-only compaction: %q", got)
	}
	runHook(t, ops, hc, "window session-start", sess) // resumed: keeps its generation
	if got := hookWindowID(ops, hc, "s"); got != "2" {
		t.Errorf("resume reset the generation: %q", got)
	}
	if _, ok := fs.files[filepath.Join("/sessions", hookStateFile)]; !ok {
		t.Errorf("window state not kept under the sessions dir: %v", keys(fs.files))
	}
}

func TestHookCursorEventsBuildTheLineageFileAndAnswerTheHost(t *testing.T) {
	fs, ops := newFakeHookOps(nil)
	hc := hookContext{cfgPath: "/c.toml", sessionsDir: "/sessions"}
	base := map[string]any{"conversation_id": "conv-1", "generation_id": "gen-1", "workspace_roots": []string{"/w/proj"}}
	with := func(kv map[string]any) map[string]any {
		m := map[string]any{}
		for k, v := range base {
			m[k] = v
		}
		for k, v := range kv {
			m[k] = v
		}
		return m
	}
	path := lineagePath("/sessions", "/w/proj")

	out, _ := runHook(t, ops, hc, "cursor sessionStart", base)
	var start struct {
		Env map[string]string `json:"env"`
	}
	if err := json.Unmarshal([]byte(out), &start); err != nil || start.Env["TOKENDROP_HARNESS"] != "cursor" || start.Env[lineageEnv] != path {
		t.Fatalf("sessionStart output: %s", out)
	}
	if len(fs.flushes) != 1 {
		t.Errorf("sessionStart did not flush: %v", fs.flushes)
	}

	runHook(t, ops, hc, "cursor afterAgentThought", with(map[string]any{"text": "I should search for this"}))
	runHook(t, ops, hc, "cursor afterAgentResponse", with(map[string]any{"text": "Let me check the docs."}))

	out, _ = runHook(t, ops, hc, "cursor beforeShellExecution", with(map[string]any{"command": "ls"}))
	if out != "" {
		t.Errorf("a foreign command got an opinion: %s", out)
	}
	out, _ = runHook(t, ops, hc, "cursor beforeShellExecution", with(map[string]any{"command": `dropin-miner search -format model "q"`}))
	if strings.TrimSpace(out) != `{"permission":"allow"}` {
		t.Errorf("our command was not allowed: %s", out)
	}

	l, ok := loadLineage(ops, path)
	if !ok {
		t.Fatal("no lineage file")
	}
	if l.Harness != "cursor" || l.SessionID != traceHash("conv-1") || l.TurnID != traceHash("conv-1|gen-1") || l.CallID == "" || l.Seq != 1 {
		t.Errorf("lineage ids: %+v", l)
	}
	if len(l.History) != 1 || l.History[0].Role != "assistant" || l.History[0].Text != "Let me check the docs." {
		t.Errorf("history should be the latest text: %+v", l.History)
	}
	runHook(t, ops, hc, "cursor preCompact", base)
	runHook(t, ops, hc, "cursor preCompact", base)
	l, _ = loadLineage(ops, path)
	if l.Window != "2" {
		t.Errorf("window after two compactions: %q", l.Window)
	}
	runHook(t, ops, hc, "cursor stop", base)
	if len(fs.flushes) != 2 {
		t.Errorf("stop did not flush: %v", fs.flushes)
	}
}

func TestHookCursorWithoutAConversationDoesNothingButAllow(t *testing.T) {
	fs, ops := newFakeHookOps(nil)
	hc := hookContext{sessionsDir: "/sessions"}
	out, _ := runHook(t, ops, hc, "cursor beforeShellExecution", map[string]any{"command": "dropin-miner search q"})
	if strings.TrimSpace(out) != `{"permission":"allow"}` {
		t.Errorf("allow is owed even with no lineage: %s", out)
	}
	for name := range fs.files {
		t.Errorf("a file was written with nothing to key it on: %s", name)
	}
}

func keys(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

var _ = fmt.Sprintf
