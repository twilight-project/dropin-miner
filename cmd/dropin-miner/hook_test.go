package main

// The hook command against an in-memory machine. The contract under test
// is fail-open: every path that cannot produce an exact envelope produces
// nothing, and the one that can changes exactly one thing.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

type fakeHookFS struct {
	files   map[string][]byte
	flushes []string
	// mtimes holds a modification time for the files a test has aged; any
	// other file reads as written at the fake's fixed "now", which is fresh.
	mtimes map[string]time.Time

	// forceWriteErr / forceMkdirErr / forceRenameErr, when set, make every
	// call to the matching op fail — standing in for a full disk or a
	// permission error on the durable sidecar, without touching real disk.
	forceWriteErr, forceMkdirErr, forceRenameErr error
}

func newFakeHookOps(env map[string]string) (*fakeHookFS, hookOps) {
	f := &fakeHookFS{files: map[string][]byte{}, mtimes: map[string]time.Time{}}
	return f, hookOps{
		executable: os.Executable,
		getenv:     func(k string) string { return env[k] },
		readFile: func(p string) ([]byte, error) {
			b, ok := f.files[p]
			if !ok {
				return nil, fs.ErrNotExist
			}
			return b, nil
		},
		writeFile: func(p string, b []byte, _ os.FileMode) error {
			if f.forceWriteErr != nil {
				return f.forceWriteErr
			}
			f.files[p] = b
			return nil
		},
		mkdirAll: func(string, os.FileMode) error { return f.forceMkdirErr },
		rename: func(from, to string) error {
			if f.forceRenameErr != nil {
				return f.forceRenameErr
			}
			f.files[to] = f.files[from]
			delete(f.files, from)
			return nil
		},
		remove: func(p string) error {
			if _, ok := f.files[p]; !ok {
				return fs.ErrNotExist
			}
			delete(f.files, p)
			delete(f.mtimes, p)
			return nil
		},
		listTemps: func(dir string) ([]tempEntry, error) {
			var out []tempEntry
			for p := range f.files {
				if filepath.Dir(p) != dir || !strings.HasSuffix(p, ".tmp") {
					continue
				}
				mod, aged := f.mtimes[p]
				if !aged {
					mod = time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
				}
				out = append(out, tempEntry{name: filepath.Base(p), modTime: mod})
			}
			return out, nil
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
		hookCursor(ops, hc, args[1], payload, stdout, stderr)
	case "hermes":
		hookHermes(args[1], payload, stdout)
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

func TestIsSearchCommandRecognizesOursAndNothingElse(t *testing.T) {
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
			t.Errorf("not recognized: %q", c)
		}
	}
	for _, c := range no {
		if isSearchCommand(c) {
			t.Errorf("wrongly recognized: %q", c)
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
		// A bridge already on the command is no longer a reason to stand down
		// (H-R4) — it is removed and replaced. What does keep the hook silent
		// is a tool whose shell this client does not know, because the syntax
		// of the prefix is exactly what it cannot guess.
		{"a tool this client does not know", map[string]any{"session_id": "s", "tool_name": "SomeOtherShell", "tool_input": map[string]any{"command": "dropin-miner search q"}}},
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

// The four fixtures below are derived from a real Claude Code transcript
// (record types and field layout byte-faithful, prose and ids scrubbed) of
// exactly #65's shape: assistant text -> Skill tool_use -> tool_result ->
// isMeta user text (the skill body Claude Code injects) -> attachments ->
// Bash tool_use. Before the fix, `isUserTurn` took the isMeta entry as the
// floor and the assistant's sentence was never found.

func readHookFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "hook", name)) // #nosec G304 -- a fixed testdata path this file builds, not an external one
	if err != nil {
		t.Fatalf("fixture %s: %v", name, err)
	}
	return b
}

func TestHookLineageSkipsTheSkillsIsMetaEntryAndFindsTheAssistantSentence(t *testing.T) {
	fs, ops := newFakeHookOps(nil)
	fs.files["/t/s.jsonl"] = readHookFixture(t, "claude_code_skill_ismeta.jsonl")
	hc := hookContext{}
	cmd := `dropin-miner search -format model "latest stable Go release version"`
	out, _ := runHook(t, ops, hc, "lineage", map[string]any{
		"session_id": "s", "prompt_id": "p", "tool_use_id": "toolu_bash1",
		"transcript_path": "/t/s.jsonl", "tool_input": map[string]any{"command": cmd},
	})
	var resp struct {
		Out struct {
			Input map[string]any `json:"updatedInput"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatalf("output: %s", out)
	}
	env := decodeBridgeFromCommand(t, resp.Out.Input["command"].(string))
	if len(env.History) != 1 || env.History[0].Text != "Now performing one web search through my own dropin-miner skill (the Skill tool), per the test spec." {
		t.Errorf("the isMeta entry was taken as the floor, or the wrong turn's prose leaked: %+v", env.History)
	}
}

func TestHookLineageSkipsTheSkillsIsMetaEntryInASubagentTranscript(t *testing.T) {
	fs, ops := newFakeHookOps(nil)
	fs.files["/t/orch.jsonl"] = []byte(`{"type":"user","message":{"role":"user","content":"spawn a subagent"}}`)
	fs.files[filepath.Join("/t", "sess", "subagents", "agent-a1.jsonl")] = readHookFixture(t, "claude_code_skill_ismeta_subagent.jsonl")
	hc := hookContext{}
	out, _ := runHook(t, ops, hc, "lineage", map[string]any{
		"session_id": "sess", "prompt_id": "p", "agent_id": "a1", "tool_use_id": "toolu_subbash1",
		"transcript_path": "/t/orch.jsonl",
		"tool_input":      map[string]any{"command": "dropin-miner search -format model q"},
	})
	var resp struct {
		Out struct {
			Input map[string]any `json:"updatedInput"`
		} `json:"hookSpecificOutput"`
	}
	_ = json.Unmarshal([]byte(out), &resp)
	env := decodeBridgeFromCommand(t, resp.Out.Input["command"].(string))
	if len(env.History) != 1 || env.History[0].Text != "Searching the web via my own dropin-miner skill for this subtask." {
		t.Errorf("subagent's own isMeta entry was taken as the floor: %+v", env.History)
	}
}

func TestHookLineageSkipsTheSkillsIsMetaEntryOnAWindowsTranscriptPath(t *testing.T) {
	// The transcript_path a Windows host reports is used directly as the
	// tail-read key (no filepath.Join here, since there is no AgentID) — the
	// fix does not depend on POSIX-shaped paths.
	fs, ops := newFakeHookOps(nil)
	winPath := `C:\Users\tester\AppData\Roaming\Claude\projects\dropin-miner\transcript.jsonl`
	fs.files[winPath] = readHookFixture(t, "claude_code_skill_ismeta.jsonl")
	hc := hookContext{}
	out, _ := runHook(t, ops, hc, "lineage", map[string]any{
		"session_id": "s", "prompt_id": "p", "tool_use_id": "toolu_bash1",
		"transcript_path": winPath,
		"tool_input":      map[string]any{"command": `dropin-miner search -format model "latest stable Go release version"`},
	})
	var resp struct {
		Out struct {
			Input map[string]any `json:"updatedInput"`
		} `json:"hookSpecificOutput"`
	}
	_ = json.Unmarshal([]byte(out), &resp)
	env := decodeBridgeFromCommand(t, resp.Out.Input["command"].(string))
	if len(env.History) != 1 || env.History[0].Text != "Now performing one web search through my own dropin-miner skill (the Skill tool), per the test spec." {
		t.Errorf("Windows-style transcript path broke the isMeta exclusion: %+v", env.History)
	}
}

// isMeta alone is not the floor-exclusion signal: it also marks a "user"
// entry that STARTS a turn with no tool call behind it at all — a
// continuation prompt, an autonomous-loop tick, a scheduled wake-up. Those
// must still floor the scan (sourceToolUseID is what's absent on all of
// them, and present on every isMeta entry the Skill tool injects). Both
// fixtures here have no assistant text anywhere in the current turn, so a
// correct floor sends no history; the previous turn's assistant prose
// ("Answering the earlier question...") must not leak in as a substitute.
func testHookLineageFloorsAtANoToolCallIsMetaEntry(t *testing.T, fixture, toolUseID string) {
	fs, ops := newFakeHookOps(nil)
	fs.files["/t/s.jsonl"] = readHookFixture(t, fixture)
	hc := hookContext{}
	out, _ := runHook(t, ops, hc, "lineage", map[string]any{
		"session_id": "s", "prompt_id": "p", "tool_use_id": toolUseID,
		"transcript_path": "/t/s.jsonl",
		"tool_input":      map[string]any{"command": `dropin-miner search -format model "latest stable Go release version"`},
	})
	var resp struct {
		Out struct {
			Input map[string]any `json:"updatedInput"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatalf("output: %s", out)
	}
	cmd, ok := resp.Out.Input["command"].(string)
	if !ok {
		t.Fatalf("no rewritten command for fixture %s: %s", fixture, out)
	}
	env := decodeBridgeFromCommand(t, cmd)
	if len(env.History) != 0 {
		t.Errorf("%s: floored past the turn-starting isMeta entry, leaking the previous turn's prose: %+v", fixture, env.History)
	}
}

func TestHookLineageFloorsAtAContinuationIsMetaEntryWithNoToolCall(t *testing.T) {
	testHookLineageFloorsAtANoToolCallIsMetaEntry(t, "claude_code_ismeta_continuation.jsonl", "toolu_bash2")
}

func TestHookLineageFloorsAtAnAutonomousLoopTickIsMetaEntryWithNoToolCall(t *testing.T) {
	testHookLineageFloorsAtANoToolCallIsMetaEntry(t, "claude_code_ismeta_autonomous_loop.jsonl", "toolu_bash3")
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
	path := conversationLineagePath("/sessions", "/w/proj", "conv-1")

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
	out, _ = runHook(t, ops, hc, "cursor beforeShellExecution", with(map[string]any{"command": cursorTestSearch(t, hc.cfgPath)}))
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
	out, _ := runHook(t, ops, hc, "cursor beforeShellExecution", map[string]any{"command": cursorTestSearch(t, hc.cfgPath)})
	if strings.TrimSpace(out) != `{"permission":"allow"}` {
		t.Errorf("allow is owed even with no lineage: %s", out)
	}
	for name := range fs.files {
		t.Errorf("a file was written with nothing to key it on: %s", name)
	}
}

// writeHookConfig gives hookMain a real config file naming a sessions
// directory, so lineage's write side is actually exercised: hookMain
// resolves hc.sessionsDir from disk (loadConfig), not from a test-injected
// hookContext the way runHook's shadow dispatcher does. Without this,
// TestHookMain* below would drive the real hookMain but the sidecar write
// path they mean to inject faults into would never run.
func writeHookConfig(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tokendrop.toml")
	if err := os.WriteFile(path, []byte("[miner]\nsessions_dir = \"/sessions\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestHookMainLineageStillRewritesWhenTheSidecarWriteFails is the fail-open
// contract at the point hook.go's own doc comment states it most strongly:
// "FAIL-OPEN, ALWAYS... emit nothing (or the one output the host requires
// to proceed)". The durable sidecar (the workspace lineage file) and the
// PreToolUse rewrite are two different writes; a disk failure on the
// FORMER must not withhold the LATTER, because the rewrite is the one
// output a hung tool call is waiting on. This drives the real hookMain
// (main.go's cmdHook entry point), not the test-local hookMainWith shadow
// used elsewhere in this file, so the assertion is about the code that
// actually ships.
func TestHookMainLineageStillRewritesWhenTheSidecarWriteFails(t *testing.T) {
	fs, ops := newFakeHookOps(nil)
	fs.forceWriteErr = errors.New("disk full")
	fs.forceMkdirErr = errors.New("disk full")
	fs.forceRenameErr = errors.New("disk full")
	cfgPath := writeHookConfig(t)

	cmd := `dropin-miner search -format model "q"`
	payload, _ := json.Marshal(map[string]any{
		"session_id": "s", "prompt_id": "p", "tool_use_id": "t", "cwd": "/home/u/project",
		"tool_input": map[string]any{"command": cmd},
	})
	var stdout, stderr bytes.Buffer
	code := hookMain(ops, []string{"-config", cfgPath, "lineage"}, bytes.NewReader(payload), &stdout, &stderr)
	if code != exitOK {
		t.Fatalf("hookMain exited %d; a disk failure on the sidecar must not fail the tool call (stderr: %s)", code, stderr.String())
	}
	var resp struct {
		Out struct {
			Event string         `json:"hookEventName"`
			Input map[string]any `json:"updatedInput"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &resp); err != nil || resp.Out.Event != "PreToolUse" {
		t.Fatalf("the rewrite was withheld because the sidecar write failed: %q", stdout.String())
	}
	if cmdOut, _ := resp.Out.Input["command"].(string); !strings.Contains(cmdOut, bridgeEnv+"=") {
		t.Errorf("command was not rewritten with the bridge: %v", resp.Out.Input["command"])
	}
	if len(fs.files) != 0 {
		t.Errorf("the fake reported every write as failing, yet something landed: %v", keys(fs.files))
	}
}

// TestHookMainNeverExitsNonZeroForAKnownSubcommand covers the rest of the
// fail-open surface hookMain's own comment claims: whatever a known
// subcommand's handler does internally — persistence failing, a refused
// flush spawn, a malformed payload — hookMain(...) still returns exitOK,
// because a coding agent's hook step is a gate in front of every tool
// call and a nonzero exit there is what actually blocks the agent.
func TestHookMainNeverExitsNonZeroForAKnownSubcommand(t *testing.T) {
	cfgPath := writeHookConfig(t)
	cases := []struct {
		name    string
		args    []string
		payload []byte
	}{
		{"window session-start, persist fails", []string{"-config", cfgPath, "window", "session-start"}, mustJSON(t, map[string]any{"session_id": "s"})},
		{"cursor sessionStart, persist fails", []string{"-config", cfgPath, "cursor", "sessionStart"}, mustJSON(t, map[string]any{"conversation_id": "c"})},
		{"flush, spawn refused", []string{"-config", cfgPath, "flush"}, nil},
		{"lineage, malformed payload", []string{"-config", cfgPath, "lineage"}, []byte("not json")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, ops := newFakeHookOps(nil)
			ops.spawnFlush = func(string) error { return errors.New("spawn refused") }
			var stdout, stderr bytes.Buffer
			code := hookMain(ops, c.args, bytes.NewReader(c.payload), &stdout, &stderr)
			if code != exitOK {
				t.Fatalf("%s: exited %d, want %d (fail-open): stderr=%q", c.name, code, exitOK, stderr.String())
			}
		})
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func keys(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

var _ = fmt.Sprintf

// cursorTestSearch is the search Cursor's skill renders for this
// installation, in the shell Cursor runs on this OS — which is the only
// thing the hook auto-allows (H-R3). The human form, which keeps the query
// in argv, is not among them: Cursor asks about that one as it would about
// any other command a person types.
func cursorTestSearch(t *testing.T, cfg string) string {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	shells, err := declaredShells(cursorTarget{}, runtime.GOOS, channelTool)
	if err != nil {
		t.Skip("Cursor declares no tool shell on this OS: " + err.Error())
	}
	_, script, err := searchBlockForShell(shells[0], binEntry{command: executable, cfg: cfg}, `{"version":1,"query":"q"}`)
	if err != nil {
		t.Fatal(err)
	}
	return script
}
