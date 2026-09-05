package main

// The hook command: what a coding agent runs around a search. Ported from
// Telem's Node hooks (lineage.mjs, window.mjs) into the binary itself —
// no Node dependency, testable like every other command — and extended to
// hosts that cannot rewrite a tool call.
//
//	hook [-config file] lineage
//	    Claude Code PreToolUse. Reads the payload on stdin. When the tool is
//	    the shell and the command is our own search, builds the trace
//	    envelope, writes it to the workspace's lineage file, and re-emits
//	    the command prefixed with TOKENDROP_TRACE_BRIDGE=<envelope> so the
//	    search carries exact turn and call identity. Any other command:
//	    silence.
//	hook [-config file] window <session-start|pre-compact|post-compact>
//	    Claude Code lifecycle. Maintains the context-window generation the
//	    lineage hook reads. session-start also starts a flush.
//	hook [-config file] cursor <event>
//	    Cursor hooks. Cursor cannot rewrite a command, so every event
//	    updates the workspace's lineage file and `search` reads it:
//	    sessionStart (seed, flush, export TOKENDROP_LINEAGE to the session),
//	    beforeShellExecution (allow our command; stamp turn/call),
//	    afterAgentThought / afterAgentResponse (the text before a search),
//	    preCompact (bump the window), stop (flush).
//	hook [-config file] flush
//	    Start a detached flush. For hosts whose only lifecycle hook is
//	    "stop".
//
// FAIL-OPEN, ALWAYS. Any error, malformed payload, unreadable transcript:
// emit nothing (or the one output the host requires to proceed) and exit
// 0, so the host continues with the original input and the search runs
// untraced. A hook that emits nothing never blocks a call. The transcript
// read is bounded — the last hookTailBytes only, files above
// hookMaxTranscript skipped whole — because this runs synchronously in
// front of every search.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	hookTailBytes     = 256 << 10
	hookMaxTranscript = 64 << 20
	hookStateFile     = "window.json"
	// bridgeEnv is how a rewritten shell command hands `search` its
	// envelope: one environment assignment in front of the command.
	bridgeEnv = "TOKENDROP_TRACE_BRIDGE"
	// lineageEnv names the workspace lineage file for a whole session,
	// set by hosts that can export environment at session start (Cursor).
	lineageEnv = "TOKENDROP_LINEAGE"
)

// hookOps: the machine, injected.
type hookOps struct {
	getenv    func(string) string
	readFile  func(string) ([]byte, error)
	writeFile func(string, []byte, os.FileMode) error
	mkdirAll  func(string, os.FileMode) error
	rename    func(string, string) error
	// readTail returns at most max bytes from the END of the file, or an
	// error; a file larger than hookMaxTranscript is reported as an error.
	readTail func(path string, max int64) ([]byte, error)
	// spawnFlush starts a detached flush. Best effort.
	spawnFlush func(cfgPath string) error
	now        func() time.Time
	pid        int
}

func realHookOps() hookOps {
	return hookOps{
		getenv:     os.Getenv,
		readFile:   os.ReadFile,
		writeFile:  os.WriteFile,
		mkdirAll:   os.MkdirAll,
		rename:     os.Rename,
		readTail:   readFileTail,
		spawnFlush: startFlush,
		now:        time.Now,
		pid:        os.Getpid(),
	}
}

func readFileTail(path string, max int64) ([]byte, error) {
	f, err := os.Open(path) // #nosec G304 -- the path comes from the host's own hook payload; the file is read, bounded, and never retained
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if info.Size() > hookMaxTranscript {
		return nil, fmt.Errorf("transcript larger than the hook budget")
	}
	off := int64(0)
	if info.Size() > max {
		off = info.Size() - max
	}
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return nil, err
	}
	return io.ReadAll(io.LimitReader(f, max))
}

func cmdHook(args []string, stdin io.Reader, stdout, stderr io.Writer, getenv func(string) string) int {
	ops := realHookOps()
	ops.getenv = getenv
	return hookMain(ops, args, stdin, stdout, stderr)
}

// hookContext is what every subcommand needs from the config: where the
// lineage files live, and which config a spawned flush should read.
type hookContext struct {
	cfgPath     string
	sessionsDir string
}

func hookMain(ops hookOps, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	cfgPath := ""
	if len(args) >= 2 && args[0] == "-config" {
		cfgPath, args = args[1], args[2:]
	}
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: dropin-miner hook [-config file] lineage | window <phase> | cursor <event> | flush")
		return exitUsage
	}
	hc := hookContext{cfgPath: cfgPath}
	// The sessions directory comes from the config. A missing or broken
	// config costs the lineage file, never the call: fail-open.
	if cfg, _, err := loadConfig(cfgPath, ops.getenv); err == nil {
		hc.sessionsDir = cfg.Miner.SessionsDir
	}
	payload, _ := io.ReadAll(io.LimitReader(stdin, 4<<20))
	switch args[0] {
	case "lineage":
		hookLineage(ops, hc, payload, stdout)
	case "window":
		if len(args) > 1 {
			hookWindow(ops, hc, args[1], payload)
			if args[1] == "session-start" && ops.spawnFlush != nil {
				_ = ops.spawnFlush(hc.cfgPath)
			}
		}
	case "cursor":
		if len(args) > 1 {
			hookCursor(ops, hc, args[1], payload, stdout)
		}
	case "flush":
		if ops.spawnFlush != nil {
			_ = ops.spawnFlush(hc.cfgPath)
		}
	default:
		fmt.Fprintf(stderr, "dropin-miner hook: unknown subcommand %q\n", args[0])
		return exitUsage
	}
	// Fail-open contract: the hook itself never fails a tool call.
	return exitOK
}

// ── our command, recognised ─────────────────────────────────────────────

// searchCommandRe matches a shell command that runs OUR search: the binary
// by bare name or any path, optionally quoted, optionally .exe, followed by
// the search subcommand. Anything else is somebody else's command and the
// hook stays out of it.
var searchCommandRe = regexp.MustCompile(`(?:^|[\s;&|(]|\$\()\s*(?:&\s*)?(?:[A-Za-z]:)?["']?(?:[^\s"']*[\\/])?dropin-miner(?:\.exe)?["']?\s+search(?:\s|$)`)

func isSearchCommand(cmd string) bool {
	return cmd != "" && searchCommandRe.MatchString(cmd)
}

// ── lineage (Claude Code PreToolUse) ────────────────────────────────────

type hookPayload struct {
	SessionID      string         `json:"session_id"`
	PromptID       string         `json:"prompt_id"`
	ToolUseID      string         `json:"tool_use_id"`
	AgentID        string         `json:"agent_id"`
	TranscriptPath string         `json:"transcript_path"`
	ToolName       string         `json:"tool_name"`
	Cwd            string         `json:"cwd"`
	ToolInput      map[string]any `json:"tool_input"`
}

// hookLineage builds the envelope and re-emits the tool input as an exact
// echo plus one change. On any doubt it prints nothing.
func hookLineage(ops hookOps, hc hookContext, payload []byte, stdout io.Writer) {
	var p hookPayload
	if err := json.Unmarshal(payload, &p); err != nil || p.SessionID == "" || p.ToolInput == nil {
		return
	}
	command, isShell := p.ToolInput["command"].(string)
	if isShell {
		if !isSearchCommand(command) || strings.Contains(command, bridgeEnv+"=") {
			return
		}
	} else if _, exists := p.ToolInput["trace_bridge"]; exists {
		return // never overwrite a bridge that is somehow already there
	}

	env := &traceEnvelope{
		V:         traceVersion,
		Harness:   orString(ops.getenv("TOKENDROP_HARNESS"), "claude-code"),
		SessionID: traceHash(p.SessionID),
		Window:    hookWindowID(ops, hc, p.SessionID),
	}
	if p.PromptID != "" {
		env.TurnID = traceHash(p.SessionID + "|" + p.PromptID)
	}
	if p.ToolUseID != "" {
		env.CallID = traceHash(p.SessionID + "|" + p.ToolUseID)
	}
	if p.AgentID != "" {
		// A subagent threads as its own lane under the session.
		env.SessionID = traceHash(p.SessionID + "|" + p.AgentID)
	}
	if text := currentAssistantText(ops, p); text != "" {
		env.History = []traceHistory{{Role: "assistant", Text: text}}
	}
	env = capTrace(env)
	if env == nil {
		return
	}

	// The lineage file is the durable copy: a host that cannot carry the
	// bridge, or a search run from a subshell that dropped the variable,
	// still finds this. Written before the rewrite so the two never
	// disagree about which call is current.
	if hc.sessionsDir != "" && p.Cwd != "" {
		_ = updateLineage(ops, lineagePath(hc.sessionsDir, p.Cwd), ops.now(), func(l *lineageFile) {
			l.Harness, l.SessionID, l.TurnID, l.CallID, l.Window = env.Harness, env.SessionID, env.TurnID, env.CallID, env.Window
			l.Seq++
			l.History = env.History
		})
	}

	bridge, err := encodeTraceBridge(env)
	if err != nil {
		return
	}
	updated := make(map[string]any, len(p.ToolInput)+1)
	for k, v := range p.ToolInput {
		updated[k] = v
	}
	if isShell {
		updated["command"] = bridgeEnv + "=" + bridge + " " + command
	} else {
		updated["trace_bridge"] = bridge
	}
	out, err := json.Marshal(map[string]any{
		"hookSpecificOutput": map[string]any{
			"hookEventName": "PreToolUse",
			"updatedInput":  updated,
		},
	})
	if err != nil {
		return
	}
	fmt.Fprintln(stdout, string(out))
}

func orString(v, fallback string) string {
	if v != "" {
		return v
	}
	return fallback
}

// currentAssistantText is the visible assistant text that led to THIS
// tool call, read from the tail of the host's JSONL transcript. Three
// rules keep it honest, all learned from Telem's hook:
//
//   - a subagent's payload carries the ORCHESTRATOR's transcript_path; its
//     own transcript is <dir>/<session>/subagents/agent-<id>.jsonl, and
//     reading the parent would attach the spawn line to every subagent
//     search;
//   - the scan is capped at the entry that emits the pending tool_use, so
//     text from a later turn (already on disk on a resumed session) never
//     leaks in;
//   - the scan is floored at the current user turn: with no floor, a
//     search made first in a turn inherits the PREVIOUS turn's prose. When
//     no turn start is in the tail, send nothing rather than guess.
//
// Bounded, fail-open to "".
func currentAssistantText(ops hookOps, p hookPayload) string {
	path := p.TranscriptPath
	if path == "" {
		return ""
	}
	if p.AgentID != "" {
		path = filepath.Join(filepath.Dir(path), p.SessionID, "subagents", "agent-"+p.AgentID+".jsonl")
	}
	tail, err := ops.readTail(path, hookTailBytes)
	if err != nil {
		return ""
	}
	type entry struct {
		Type          string          `json:"type"`
		ToolUseResult json.RawMessage `json:"toolUseResult"`
		Message       struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}
	type block struct {
		Type string `json:"type"`
		Text string `json:"text"`
		ID   string `json:"id"`
	}
	var entries []entry
	for _, line := range bytes.Split(tail, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 || line[0] != '{' {
			continue // the first tail line is usually a partial record
		}
		var e entry
		if json.Unmarshal(line, &e) == nil {
			entries = append(entries, e)
		}
	}
	blocksOf := func(e entry) []block {
		var bs []block
		if len(e.Message.Content) == 0 || e.Message.Content[0] != '[' {
			return nil
		}
		_ = json.Unmarshal(e.Message.Content, &bs)
		return bs
	}
	isAssistant := func(e entry) bool { return e.Type == "assistant" || e.Message.Role == "assistant" }
	// A real user turn, not the tool-result record the host also writes
	// with type "user": those carry toolUseResult, or only tool_result blocks.
	isUserTurn := func(e entry) bool {
		if e.Type != "user" || len(e.ToolUseResult) > 0 {
			return false
		}
		if len(e.Message.Content) > 0 && e.Message.Content[0] == '"' {
			return true
		}
		for _, b := range blocksOf(e) {
			if b.Type != "tool_result" {
				return true
			}
		}
		return false
	}

	cutoff := len(entries)
	if p.ToolUseID != "" {
	scan:
		for i := len(entries) - 1; i >= 0; i-- {
			if !isAssistant(entries[i]) {
				continue
			}
			for _, b := range blocksOf(entries[i]) {
				if b.Type == "tool_use" && b.ID == p.ToolUseID {
					cutoff = i + 1
					break scan
				}
			}
		}
	}
	floor := -1
	for i := cutoff - 1; i >= 0; i-- {
		if isUserTurn(entries[i]) {
			floor = i
			break
		}
	}
	if floor < 0 {
		return ""
	}
	for i := cutoff - 1; i > floor; i-- {
		if !isAssistant(entries[i]) {
			continue
		}
		var b bytes.Buffer
		for _, c := range blocksOf(entries[i]) {
			if c.Type == "text" && c.Text != "" {
				if b.Len() > 0 {
					b.WriteByte('\n')
				}
				b.WriteString(c.Text)
			}
		}
		if b.Len() > 0 {
			text := b.String()
			if len(text) > traceHistoryCap {
				text = text[len(text)-traceHistoryCap:]
			}
			return text
		}
	}
	return ""
}

// ── window generation counter (Claude Code) ─────────────────────────────
//
// One JSON file keyed by session id, so lineage reads window identity
// without touching the transcript. Semantics ported from Telem's
// window.mjs: seed is idempotent (a resumed session keeps its generation);
// exactly one bump per compaction whichever of pre/post the host delivers.

type hookWindowState struct {
	Version  int                        `json:"version"`
	Sessions map[string]hookWindowEntry `json:"sessions"`
}

type hookWindowEntry struct {
	Generation int  `json:"generation"`
	Open       bool `json:"open"`
}

func hookStatePath(ops hookOps, hc hookContext) string {
	if hc.sessionsDir != "" {
		return filepath.Join(hc.sessionsDir, hookStateFile)
	}
	if root := ops.getenv("CLAUDE_PLUGIN_ROOT"); root != "" {
		return filepath.Join(root, hookStateFile)
	}
	if tmp := ops.getenv("TMPDIR"); tmp != "" {
		return filepath.Join(tmp, "dropin-miner-"+hookStateFile)
	}
	return filepath.Join(os.TempDir(), "dropin-miner-"+hookStateFile)
}

func hookReadState(ops hookOps, path string) hookWindowState {
	state := hookWindowState{Version: 1, Sessions: map[string]hookWindowEntry{}}
	b, err := ops.readFile(path)
	if err != nil {
		return state
	}
	var loaded hookWindowState
	if json.Unmarshal(b, &loaded) == nil && loaded.Sessions != nil {
		return loaded
	}
	return state
}

// hookWindow applies one phase event. Never fails; a state-file problem must
// not block a session start or a compaction.
func hookWindow(ops hookOps, hc hookContext, phase string, payload []byte) {
	var p struct {
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal(payload, &p); err != nil || p.SessionID == "" {
		return
	}
	path := hookStatePath(ops, hc)
	state := hookReadState(ops, path)
	entry, existed := state.Sessions[p.SessionID]

	switch phase {
	case "session-start":
		if existed {
			return // resumed session: keep its generation
		}
		state.Sessions[p.SessionID] = hookWindowEntry{}
	case "pre-compact":
		if !entry.Open {
			entry.Generation++
			entry.Open = true
		}
		state.Sessions[p.SessionID] = entry
	case "post-compact":
		if entry.Open {
			entry.Open = false
		} else {
			entry.Generation++
		}
		state.Sessions[p.SessionID] = entry
	default:
		return
	}

	b, err := json.Marshal(state)
	if err != nil {
		return
	}
	// Atomic: temp then rename, so lineage never reads a half-written file.
	if err := ops.mkdirAll(filepath.Dir(path), 0o700); err != nil {
		return
	}
	tmp := path + "." + strconv.Itoa(ops.pid) + ".tmp"
	if err := ops.writeFile(tmp, b, 0o600); err != nil {
		return
	}
	_ = ops.rename(tmp, path)
}

// hookWindowID is what lineage stamps into the envelope: "none" until the
// first compaction, then the generation number.
func hookWindowID(ops hookOps, hc hookContext, sessionID string) string {
	state := hookReadState(ops, hookStatePath(ops, hc))
	entry := state.Sessions[sessionID]
	if entry.Generation == 0 {
		return "none"
	}
	return strconv.Itoa(entry.Generation)
}

// ── Cursor ──────────────────────────────────────────────────────────────

type cursorPayload struct {
	ConversationID string   `json:"conversation_id"`
	GenerationID   string   `json:"generation_id"`
	WorkspaceRoots []string `json:"workspace_roots"`
	Cwd            string   `json:"cwd"`
	Command        string   `json:"command"`
	Text           string   `json:"text"`
}

// hookCursor handles one Cursor event. Every event that carries a
// conversation updates the workspace's lineage file; the two events Cursor
// waits on an answer for (sessionStart, beforeShellExecution) get exactly
// the answer that lets the session proceed.
func hookCursor(ops hookOps, hc hookContext, event string, payload []byte, stdout io.Writer) {
	var p cursorPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		p = cursorPayload{}
	}
	workspace := p.Cwd
	if len(p.WorkspaceRoots) > 0 && p.WorkspaceRoots[0] != "" {
		workspace = p.WorkspaceRoots[0]
	}
	path := ""
	if hc.sessionsDir != "" && workspace != "" {
		path = lineagePath(hc.sessionsDir, workspace)
	}
	now := ops.now()
	update := func(apply func(*lineageFile)) {
		if path == "" || p.ConversationID == "" {
			return
		}
		_ = updateLineage(ops, path, now, func(l *lineageFile) {
			l.Harness = "cursor"
			l.SessionID = traceHash(p.ConversationID)
			apply(l)
		})
	}
	flush := func() {
		if ops.spawnFlush != nil {
			_ = ops.spawnFlush(hc.cfgPath)
		}
	}

	switch event {
	case "sessionStart":
		update(func(l *lineageFile) {
			if l.Window == "" {
				l.Window = "none"
			}
		})
		flush()
		env := map[string]string{"TOKENDROP_HARNESS": "cursor"}
		if path != "" {
			env[lineageEnv] = path
		}
		out, _ := json.Marshal(map[string]any{"env": env})
		fmt.Fprintln(stdout, string(out))
	case "beforeShellExecution":
		if !isSearchCommand(p.Command) {
			return // not ours: no opinion, Cursor applies its own policy
		}
		update(func(l *lineageFile) {
			if p.GenerationID != "" {
				l.TurnID = traceHash(p.ConversationID + "|" + p.GenerationID)
			}
			l.CallID = traceRandomID()
			l.Seq++
		})
		fmt.Fprintln(stdout, `{"permission":"allow"}`)
	case "afterAgentThought":
		if p.Text == "" {
			return
		}
		update(func(l *lineageFile) { l.History = []traceHistory{{Role: "reasoning", Text: p.Text}} })
	case "afterAgentResponse":
		if p.Text == "" {
			return
		}
		update(func(l *lineageFile) { l.History = []traceHistory{{Role: "assistant", Text: p.Text}} })
	case "preCompact":
		update(func(l *lineageFile) {
			n, _ := strconv.Atoi(l.Window)
			l.Window = strconv.Itoa(n + 1)
		})
	case "stop", "sessionEnd":
		flush()
	}
}
