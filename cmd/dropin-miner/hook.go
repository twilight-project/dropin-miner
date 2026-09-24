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
//	    Cursor hooks. Every event updates the session's lineage file and
//	    `search` reads it:
//	    sessionStart (seed, flush, export the identity to the session's
//	    later hooks), preToolUse (put that identity on our exact rendered
//	    search, #118), beforeShellExecution (allow our command; stamp
//	    turn/call),
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
	"strings"
	"time"
	"unicode/utf16"
	"unicode/utf8"
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
	remove     func(string) error
	// listTemps returns the entries of dir whose names end in ".tmp", with
	// their modification times. Only replaceViaTemp's sweep reads it.
	listTemps func(dir string) ([]tempEntry, error)
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
		remove:     os.Remove,
		listTemps:  listTempFiles,
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
			hookCursor(ops, hc, args[1], payload, stdout, stderr)
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
	// The error is still discarded — a state-file problem never blocks a
	// session start or a compaction — but a failed rename no longer leaves
	// its temporary file behind (#100). Only this file's own leftovers are
	// swept: with no sessions directory configured the state file lives in
	// the plugin root or TMPDIR, which are not this client's to tidy.
	_ = replaceViaTemp(ops, path, b, ops.now(), false)
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
// conversation updates that conversation's lineage file; the events Cursor
// waits on an answer for get exactly the answer that lets the session
// proceed.
func hookCursor(ops hookOps, hc hookContext, event string, payload []byte, stdout, stderr io.Writer) {
	var p cursorPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		p = cursorPayload{}
	}
	workspace := p.Cwd
	if len(p.WorkspaceRoots) > 0 && p.WorkspaceRoots[0] != "" {
		workspace = p.WorkspaceRoots[0]
	}
	// One file per conversation (#109). sessionStart names it and exports it;
	// every later event writes to the file the session DECLARED, when its
	// environment holds one for this very conversation, and computes the same
	// name otherwise. Declared first is what serves a conversation whose
	// sessionStart ran on 0.2.12: it exported the workspace-keyed file, its
	// searches carry that path, and its later events must keep writing there
	// or its search would read a file nobody stamps.
	path := ""
	if hc.sessionsDir != "" && workspace != "" && p.ConversationID != "" {
		path = conversationLineagePath(hc.sessionsDir, workspace, p.ConversationID)
	}
	if event != "sessionStart" && p.ConversationID != "" && ops.getenv(sessionEnv) == traceHash(p.ConversationID) {
		if declared := ops.getenv(lineageEnv); isLineageFileIn(hc.sessionsDir, declared) {
			path = declared
		}
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
			// Exported even when no path could be computed: the file a
			// search declares is adopted only when it holds this session.
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
		recognized := recognizeCursorCommand(ops, hc, p.Command, shells)
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
	case "preToolUse":
		shells, err := declaredShells(cursorTarget{}, runtime.GOOS, channelTool)
		if err != nil {
			return
		}
		runners, err := declaredShells(cursorTarget{}, runtime.GOOS, channelHook)
		if err != nil {
			return
		}
		cursorPreToolUse(ops, hc, payload, shells, runners, stdout, stderr)
	case "afterAgentThought":
		if p.Text == "" {
			return
		}
		p.Text = repairStoredText(p.Text, stderr)
		update(func(l *lineageFile) { l.History = []traceHistory{{Role: "reasoning", Text: p.Text}} })
	case "afterAgentResponse":
		if p.Text == "" {
			return
		}
		p.Text = repairStoredText(p.Text, stderr)
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

// ── a payload Cursor's wrapper double-encoded (#113) ────────────────────
//
// On Windows Cursor runs a hook as
//
//	$OutputEncoding = [System.Text.Encoding]::UTF8; Get-Content -LiteralPath '…\payload.json' -Raw | & { $input | & '<hook>' }
//
// and Windows PowerShell 5.1's Get-Content reads a file with no byte-order
// mark in the ANSI code page, one character per byte, which the pipe then
// encodes as UTF-8 again: `é`, c3 a9, reaches this hook as c3 83 c2 a9.
// Measured against Cursor's own hook log, which holds the same sentence
// intact; reported to Cursor (forum thread 172787).
//
// The shape is unambiguous enough to reverse. Text that is valid UTF-8, whose
// every character is one the ANSI reading can produce from a single byte, and
// whose bytes so recovered are valid UTF-8 again, was one UTF-8 string read a
// byte at a time. It is reversed once, on the text the hooks STORE — the
// assistant's words and its reasoning. Never on the command: preToolUse hands
// the command back to Cursor to run, and a guess there would change the
// search (a non-ASCII command is not rewritten on Windows at all).
//
// "A single byte" is Latin-1 (U+0000–U+00FF, which covers the five bytes
// cp1252 leaves undefined, as the measurement showed for 0x9d) and cp1252's
// 27 characters for 0x80–0x9F. The second half is what reaches this hook for
// the text a model writes most: `—` (e2 80 94) arrives as `â€”`, `’` as `â€™`
// — #88's shape — and a Latin-1-only reading would leave every one of them.

// cp1252High is cp1252's mapping of 0x80–0x9F, reversed.
var cp1252High = map[rune]byte{
	'€': 0x80, '‚': 0x82, 'ƒ': 0x83, '„': 0x84, '…': 0x85, '†': 0x86, '‡': 0x87,
	'ˆ': 0x88, '‰': 0x89, 'Š': 0x8a, '‹': 0x8b, 'Œ': 0x8c, 'Ž': 0x8e,
	'‘': 0x91, '’': 0x92, '“': 0x93, '”': 0x94, '•': 0x95, '–': 0x96, '—': 0x97,
	'˜': 0x98, '™': 0x99, 'š': 0x9a, '›': 0x9b, 'œ': 0x9c, 'ž': 0x9e, 'Ÿ': 0x9f,
}

// undoDoubleEncoding returns s read back as the UTF-8 it was, and true, when
// s has the double-encoded shape; otherwise s unchanged and false.
func undoDoubleEncoding(s string) (string, bool) {
	if isASCII(s) || !utf8.ValidString(s) {
		return s, false
	}
	b := make([]byte, 0, len(s))
	for _, r := range s {
		if r <= 0xff {
			b = append(b, byte(r)) // #nosec G115 -- r <= 0xff, checked on the line above
			continue
		}
		c, ok := cp1252High[r]
		if !ok {
			return s, false // a character no single byte reads as: s is what it says
		}
		b = append(b, c)
	}
	// The second reading. s is not ASCII, so b holds a byte ≥ 0x80, and valid
	// UTF-8 with such a byte is at least one multibyte sequence. Plain Latin-1
	// prose — `café` as c3 a9 — gives e9 here, alone, which is not UTF-8.
	if !utf8.Valid(b) {
		return s, false
	}
	return string(b), true
}

// repairStoredText is undoDoubleEncoding for a string about to be stored,
// counted on stderr so that a repair is never silent.
func repairStoredText(s string, stderr io.Writer) string {
	fixed, ok := undoDoubleEncoding(s)
	if ok {
		fmt.Fprintln(stderr, "dropin-miner hook: 1 double-encoded text repaired before storing (#113)")
	}
	return fixed
}

// ── Cursor's identity, carried on the command (#118) ────────────────────
//
// A `sessionStart` hook's `env` does not reach the shell Cursor's agent runs,
// and Cursor never said it would: its documentation promises that
// "session-scoped environment variables from sessionStart hooks are passed to
// all subsequent hook executions within that session" — later HOOKS, not the
// agent's shell and not the terminal. Measured on macOS and Windows, both
// terminal profiles: every Cursor search reached the router as `cli`.
//
// So the variables sessionStart exports arrive here, in the preToolUse hook,
// and this hook has a documented way to change the command the agent is about
// to run (`updated_input`). For the one command that is ours — byte for byte
// the search this installation's skill renders, recognized exactly as
// beforeShellExecution recognizes it for its allow — the three assignments
// go in front of it in the syntax of the shell it was rendered for, and
// searchTrace finds them set. Nothing else is touched, and this hook never
// denies: a command it does not rewrite gets no answer at all.
//
// Provenance (invariant 16, H-R4): only a prefix this adapter writes, with
// the values this session's own sessionStart exported, may say `cursor`. The
// harness must be exactly that, the session a hashed id, and the lineage path
// a lineage file in this installation's sessions directory; anything else in
// the environment is not what our sessionStart wrote, and is not carried.

// cursorIdentity is what a Cursor session's sessionStart exported.
type cursorIdentity struct {
	lineage string
	session string
}

// hashedIDRe is traceHash's shape: 32 lowercase hex digits.
var hashedIDRe = regexp.MustCompile(`^[0-9a-f]{32}$`)

// cursorIdentityFromEnv reads the identity from the hook's own environment,
// and only when it is exactly the shape sessionStart exports.
func cursorIdentityFromEnv(getenv func(string) string, sessionsDir string) (cursorIdentity, bool) {
	if getenv("TOKENDROP_HARNESS") != "cursor" {
		return cursorIdentity{}, false
	}
	id := cursorIdentity{lineage: getenv(lineageEnv), session: getenv(sessionEnv)}
	if !hashedIDRe.MatchString(id.session) || !isLineageFileIn(sessionsDir, id.lineage) {
		return cursorIdentity{}, false
	}
	return id, true
}

// isLineageFileIn reports whether p names a lineage file directly inside dir.
func isLineageFileIn(dir, p string) bool {
	return dir != "" && p != "" && samePath(filepath.Dir(p), dir) && lineageNameRe.MatchString(filepath.Base(p))
}

// quotableFor reports whether value can be carried inside the single-quoted
// literal sh's quoting writes, meaning exactly itself. A control character is
// refused in both: a line break inside the prefix would put the rest of it on
// a line of its own, and none of this client's values ever holds one.
// PowerShell also reads the typographic quotes U+2018–U+201B as single
// quotes, which powerShellQuoteArg does not double, so one of those would end
// the literal early.
func quotableFor(sh shellKind, value string) bool {
	if !utf8.ValidString(value) {
		return false
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return false
		}
		if sh == shellPowerShell && r >= '‘' && r <= '‛' {
			return false
		}
	}
	return true
}

// cursorIdentityPrefix renders the three assignments for sh, each value quoted
// with the quoting the renderer owns. ok is false when a value cannot be
// quoted for sh, and then nothing is rendered: never a partial prefix.
func cursorIdentityPrefix(sh shellKind, id cursorIdentity) (string, bool) {
	var b strings.Builder
	for _, kv := range [][2]string{{"TOKENDROP_HARNESS", "cursor"}, {lineageEnv, id.lineage}, {sessionEnv, id.session}} {
		if !quotableFor(sh, kv[1]) {
			return "", false
		}
		switch sh {
		case shellPOSIX:
			// NAME='v' words in front of one command: POSIX scopes them to
			// that command.
			b.WriteString(kv[0] + "=" + posixQuoteArg(kv[1]) + " ")
		case shellPowerShell:
			b.WriteString("$env:" + kv[0] + "=" + powerShellQuoteArg(kv[1]) + "; ")
		default:
			return "", false
		}
	}
	return b.String(), true
}

// withCursorIdentity puts prefix on a command rendered for sh. In PowerShell
// the skill's $OutputEncoding line stays first — it is what makes the
// here-string reach the binary as UTF-8 — so the assignments go after it.
func withCursorIdentity(sh shellKind, prefix, command string) (string, bool) {
	switch sh {
	case shellPOSIX:
		return prefix + command, true
	case shellPowerShell:
		head := psOutputEncodingLine + "\n"
		if !strings.HasPrefix(command, head) {
			return "", false
		}
		return head + prefix + command[len(head):], true
	}
	return "", false
}

// withoutCursorIdentity is withCursorIdentity's exact inverse: the command
// with this prefix, and only this prefix, removed from where
// withCursorIdentity puts it. ok is false when it is not there.
func withoutCursorIdentity(sh shellKind, prefix, command string) (string, bool) {
	switch sh {
	case shellPOSIX:
		return strings.CutPrefix(command, prefix)
	case shellPowerShell:
		head := psOutputEncodingLine + "\n"
		rest, ok := strings.CutPrefix(command, head+prefix)
		if !ok {
			return "", false
		}
		return head + rest, true
	}
	return "", false
}

// recognizeCursorCommand is beforeShellExecution's recognizer. A command is
// ours when it is exactly one of the rendered forms, as before, or exactly
// the rendered SEARCH with this session's identity prefix in front of it —
// the prefix rebuilt here from this hook's own environment, the same values
// preToolUse wrote, and compared byte for byte. Any other prefix, one
// carrying other values, or this prefix on anything but the search, is not a
// command this client wrote (#91's discipline, for a prefix we write
// ourselves), and Cursor asks about it as about any other.
func recognizeCursorCommand(ops hookOps, hc hookContext, command string, shells []shellKind) *recognizedForm {
	if f := recognizeRenderedForm(command, ops.executable, hc.cfgPath, shells); f != nil {
		return f
	}
	id, ok := cursorIdentityFromEnv(ops.getenv, hc.sessionsDir)
	if !ok {
		return nil
	}
	for _, sh := range shells {
		prefix, ok := cursorIdentityPrefix(sh, id)
		if !ok {
			continue
		}
		rest, ok := withoutCursorIdentity(sh, prefix, command)
		if !ok {
			continue
		}
		if f := recognizeRenderedForm(rest, ops.executable, hc.cfgPath, []shellKind{sh}); isSearchForm(f) {
			return f
		}
	}
	return nil
}

func isSearchForm(f *recognizedForm) bool {
	return f != nil && len(f.path) == 1 && f.path[0] == "search"
}

// hookInputIntact reports whether the runners Cursor starts its hooks with
// hand this hook the payload's bytes unchanged. POSIX alone does. On Windows
// Cursor wraps a hook in PowerShell that reads the payload file in the ANSI
// code page and re-encodes it, so every non-ASCII character arrives changed
// (#113) — in whatever code page that machine has, so the change cannot be
// undone with certainty.
func hookInputIntact(runners []shellKind) bool {
	if len(runners) == 0 {
		return false
	}
	for _, sh := range runners {
		if sh != shellPOSIX {
			return false
		}
	}
	return true
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

// cursorPreToolUse answers Cursor's preToolUse. shells are the tool shells
// Cursor runs on this OS and runners its hook runners; both are parameters so
// that every OS's forms are exercised on every CI runner.
func cursorPreToolUse(ops hookOps, hc hookContext, payload []byte, shells, runners []shellKind, stdout, stderr io.Writer) {
	var p struct {
		CursorVersion *string                    `json:"cursor_version"`
		ToolInput     map[string]json.RawMessage `json:"tool_input"`
	}
	if json.Unmarshal(payload, &p) != nil || p.CursorVersion == nil || *p.CursorVersion == "" || p.ToolInput == nil {
		return
	}
	var command string
	if json.Unmarshal(p.ToolInput["command"], &command) != nil || command == "" {
		return
	}
	id, ok := cursorIdentityFromEnv(ops.getenv, hc.sessionsDir)
	if !ok {
		return
	}
	for _, sh := range shells {
		if !isSearchForm(recognizeRenderedForm(command, ops.executable, hc.cfgPath, []shellKind{sh})) {
			continue
		}
		// The command goes back to Cursor as the command it will run. If the
		// bytes this hook received are not the bytes the agent wrote, echoing
		// them would change the search itself — the query is in the command —
		// and a lost label is the only acceptable cost of that doubt.
		if !isASCII(command) && !hookInputIntact(runners) {
			fmt.Fprintln(stderr, "dropin-miner hook: search not labeled: its command carries non-ASCII text, and Cursor's hook runner on this OS re-encodes it (#113)")
			return
		}
		prefix, ok := cursorIdentityPrefix(sh, id)
		if !ok {
			fmt.Fprintf(stderr, "dropin-miner hook: search not labeled: the session's identity cannot be quoted for %s\n", sh)
			return
		}
		rewritten, ok := withCursorIdentity(sh, prefix, command)
		if !ok {
			return
		}
		// Cursor uses updated_input INSTEAD of the tool's input, so every
		// other field it sent (the working directory, the timeout) goes back
		// as it came and only the command changes.
		encoded, err := asciiJSON(rewritten)
		if err != nil {
			return
		}
		p.ToolInput["command"] = encoded
		out, err := asciiJSON(map[string]any{"permission": "allow", "updated_input": p.ToolInput})
		if err != nil {
			return
		}
		fmt.Fprintln(stdout, string(out))
		return
	}
}

// asciiJSON encodes v with every non-ASCII character escaped, so the answer
// reaches Cursor as the same text whatever code page a wrapper between this
// process and Cursor reads it in.
func asciiJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	var out bytes.Buffer
	for _, r := range strings.TrimSuffix(buf.String(), "\n") {
		switch {
		case r < 0x80:
			out.WriteRune(r)
		case r > 0xffff:
			hi, lo := utf16.EncodeRune(r)
			fmt.Fprintf(&out, `\u%04x\u%04x`, hi, lo)
		default:
			fmt.Fprintf(&out, `\u%04x`, r)
		}
	}
	return out.Bytes(), nil
}
