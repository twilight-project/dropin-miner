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
//	    Claude Code Stop. Start a detached flush.
//
// The lineage, window and flush entry points are installed for Claude Code
// and for nobody else. Another host that loads Claude Code's settings and
// runs them with its own payload (Cursor does, #87) gets nothing from them:
// see runByAnotherHost.
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
	"runtime"
	"strconv"
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
	// sessionEnv carries the hashed session id of the session a shell was
	// started in — the same value that session's hook writes into its lineage
	// file, so it is nothing the file does not already hold and nothing the
	// envelope does not already send. It exists for the walk: when the
	// lineage variable is lost, this says WHICH session of a host the search
	// belongs to, which the host's name alone cannot (#104).
	sessionEnv = "TOKENDROP_SESSION"
)

// hookOps: the machine, injected.
type hookOps struct {
	executable func() (string, error)
	getenv     func(string) string
	readFile   func(string) ([]byte, error)
	writeFile  func(string, []byte, os.FileMode) error
	mkdirAll   func(string, os.FileMode) error
	rename     func(string, string) error
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
		executable: os.Executable,
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
	// One leading byte-order mark is tolerated here for the same reason as on
	// `search --stdin`: the shell in front of a hook is the host's choice,
	// and a mark it adds would otherwise cost the whole payload — a hook that
	// parses nothing emits nothing, and the search loses its lineage.
	payload, _ := io.ReadAll(io.LimitReader(stdin, 4<<20))
	payload = trimUTF8BOM(payload)
	// A hook installed for Claude Code stands down when another host runs it
	// (#87): nothing written, nothing spawned, nothing printed, exit 0.
	if event, claudeFormat := claudeEntryEvent(args); claudeFormat && runByAnotherHost(payload, event) {
		return exitOK
	}
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
	case "hermes":
		if len(args) > 1 {
			hookHermes(args[1], payload, stdout)
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

// ── who is calling (#87) ────────────────────────────────────────────────
//
// Cursor loads Claude Code's hooks from ~/.claude/settings.json ("Include
// Third-Party Plugins, Skills, and Other Configs", on by default) and runs
// them beside its own, with ITS payload. Measured from Cursor 3.20.21's hook
// log on macOS and Windows: `window session-start` at sessionStart,
// `lineage` at preToolUse, `flush` at stop — three extra processes and a
// second flush every turn, and with v0.2.9 a `lineage` answer Cursor honored,
// which sent a Cursor search to the router labeled claude-code (#91).
//
// The decision is taken from the payload, never from the environment: the
// environment is whatever the calling host's process happened to inherit,
// and the payload is what the caller itself says.

// claudeEntryEvent names the Claude Code event a Claude-format entry point is
// installed under (`claudeHooks` is the authority; a test holds the two
// together). ok is false for every other subcommand: Cursor's and Hermes'
// own entry points are theirs and are never gated here.
func claudeEntryEvent(args []string) (event string, ok bool) {
	if len(args) == 0 {
		return "", false
	}
	switch args[0] {
	case "lineage":
		return "PreToolUse", true
	case "flush":
		return "Stop", true
	case "window":
		if len(args) < 2 {
			return "", false
		}
		switch args[1] {
		case "session-start":
			return "SessionStart", true
		case "pre-compact":
			return "PreCompact", true
		case "post-compact":
			return "PostCompact", true
		}
	}
	return "", false
}

// runByAnotherHost reports whether the payload itself shows that the caller
// is not Claude Code. Two signals, each taken from payloads in hand
// (testdata/hook/cursor-3.20.21-*.json, and Claude Code 2.1.274 probed live):
//
//   - `cursor_version` is present. Every payload Cursor sends carries it and
//     Claude Code sends none. This is the measured minimum.
//   - `hook_event_name` is present and is not, byte for byte, the event this
//     entry point is installed under. Claude Code reports exactly the name
//     the entry was registered under (`PreToolUse`, `SessionStart`, `Stop`);
//     Cursor's loader translates the registration to its own event and
//     reports that one (`preToolUse`, `sessionStart`, `stop`). A host that
//     copies Cursor's loader does the same without ever saying
//     `cursor_version`, and by its own statement it is not delivering the
//     Claude Code event.
//
// Everything else is NOT evidence and changes nothing: a payload that names
// no event at all, or one that is not JSON, is handled exactly as before.
// Nothing is known about a caller that says nothing — Copilot CLI's payload
// carries no `hook_event_name` and Codex uses Claude Code's own names — and
// a rule that stood down for them would be a guess.
func runByAnotherHost(payload []byte, event string) bool {
	var keys map[string]json.RawMessage
	if json.Unmarshal(payload, &keys) != nil {
		return false
	}
	if _, cursor := keys["cursor_version"]; cursor {
		return true
	}
	raw, named := keys["hook_event_name"]
	if !named {
		return false
	}
	var name string
	return json.Unmarshal(raw, &name) != nil || name != event
}

// ── our command, recognized ─────────────────────────────────────────────

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
	// Which shell will run this command is not a property of the host but of
	// the TOOL: Claude Code runs the Bash tool through Git Bash and the
	// PowerShell tool through PowerShell, and its payload names which one
	// (#77, H-R5). A tool this client does not know gets no rewrite — the
	// syntax of its shell is exactly what is not known.
	bridgeShell := shellPOSIX
	// ours is true when the command is EXACTLY the search this installation's
	// own skill renders for that tool's shell — the same question Cursor's
	// hook asks, answered by rebuilding the string rather than by matching a
	// pattern. It is what the allow decision below is taken from.
	// isSearchCommand is a pattern and matches any spelling of any search, so
	// it decides only whether to look at this command at all; it must never
	// decide a permission answer.
	ours := false
	if isShell {
		if !isSearchCommand(command) {
			return
		}
		sh, known := bridgeShellForTool(p.ToolName)
		if !known {
			return
		}
		bridgeShell = sh
		if f := recognizeRenderedForm(command, ops.executable, hc.cfgPath, []shellKind{bridgeShell}); f != nil {
			ours = len(f.path) == 1 && f.path[0] == "search"
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
		// H-R4: every bridge assignment this can prove standalone is removed
		// and ours is prepended; a command carrying one it cannot remove is
		// left alone, and no lineage is claimed for it.
		rewritten, ok := withTraceBridge(bridgeShell, bridge, command)
		if !ok {
			return
		}
		updated["command"] = rewritten
	} else {
		updated["trace_bridge"] = bridge
	}
	hso := map[string]any{
		"hookEventName": "PreToolUse",
		"updatedInput":  updated,
	}
	// The permission question this hook caused, answered by this hook (#98).
	//
	// `agents install` writes permissions.allow PREFIX rules naming the binary
	// first. The rewrite above puts the bridge in front of it, so the command
	// Claude Code evaluates no longer begins with the binary and no prefix
	// rule can match it; the call falls through to the Bash safety
	// heuristics, where the skill's own heredoc reads as
	// "Contains brace with quote character (expansion obfuscation)" — a
	// prompt interactively and a refusal in a headless session. Measured:
	// under permissions.defaultMode "default" that heredoc is refused with
	// exactly that message, and the same call with this answer runs.
	//
	// Only the exact rendered search is allowed, and only when this
	// installation's own binary and config are the ones named. Everything
	// else is rewritten as before and left to the permission system, which is
	// what it was already doing. The three allow rules stay for hosts and
	// versions that do not run this hook.
	if ours {
		hso["permissionDecision"] = "allow"
		hso["permissionDecisionReason"] = "dropin-miner: this is the search command its own skill renders, and the trace bridge this hook just added is why the installed allow rule no longer matches it"
	}
	out, err := json.Marshal(map[string]any{"hookSpecificOutput": hso})
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
// tool call, read from the tail of the host's JSONL transcript. Four
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
//   - the floor is a REAL user turn: a `type: "user"` entry the host wrote
//     for itself IN RESPONSE TO A TOOL CALL — carrying a non-empty
//     `sourceToolUseID` — is not one. The Skill tool's injected skill body
//     is the case that reaches every search made through this skill, and it
//     always carries this field; without the exclusion that entry floors
//     the scan one step too late and the assistant's sentence right before
//     the Skill call is never found (#65). `isMeta` alone is NOT the signal:
//     it also marks entries that START a turn or sit between turns with no
//     tool call behind them at all — "Continue from where you left off.",
//     an autonomous-loop tick, a scheduled wake-up — and those are exactly
//     the turn starts the floor exists to find.
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
		Type            string          `json:"type"`
		ToolUseResult   json.RawMessage `json:"toolUseResult"`
		SourceToolUseID string          `json:"sourceToolUseID"`
		Message         struct {
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
	// A real user turn, not a host-injected "user"-typed record written IN
	// RESPONSE TO A TOOL CALL: the tool-result the host also writes
	// (toolUseResult, or only tool_result blocks) or an entry carrying a
	// non-empty sourceToolUseID, such as the Skill tool's injected skill
	// body (#65). isMeta on its own is not the signal — it also marks
	// entries that start a turn with no tool call behind them at all
	// ("Continue from where you left off.", an autonomous-loop tick), and
	// those must still floor the scan.
	isUserTurn := func(e entry) bool {
		if e.Type != "user" || len(e.ToolUseResult) > 0 || e.SourceToolUseID != "" {
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
			return prepareTraceText(b.String())
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
		if p.ConversationID != "" {
			// Exported even when no path could be computed: that is one of
			// the two ways a search ends up on the walk, and the walk is
			// where this is read.
			env[sessionEnv] = traceHash(p.ConversationID)
		}
		out, _ := json.Marshal(map[string]any{"env": env})
		fmt.Fprintln(stdout, string(out))
	case "beforeShellExecution":
		// The shells Cursor runs on this OS, from the declaration: the command
		// this hook is asked about was rendered for one of them, and a form
		// rendered for a shell Cursor does not use is not ours to allow.
		shells, err := declaredShells(cursorTarget{}, runtime.GOOS, channelTool)
		if err != nil {
			return // nothing established: allow nothing, stamp nothing
		}
		recognized := recognizeRenderedForm(p.Command, ops.executable, hc.cfgPath, shells)
		if recognized == nil {
			return
		}
		if len(recognized.path) == 1 && recognized.path[0] == "search" {
			update(func(l *lineageFile) {
				if p.GenerationID != "" {
					l.TurnID = traceHash(p.ConversationID + "|" + p.GenerationID)
				}
				l.CallID = traceRandomID()
				l.Seq++
			})
		}
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
