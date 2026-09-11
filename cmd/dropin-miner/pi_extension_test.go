package main

// The Pi lineage extension, executed for real.
//
// Source-text greps cannot answer the questions that matter about this
// file — whether the history it attaches belongs to THIS tool call, whether
// a secret survives the caps, whether a malformed session leaves the tool
// call untouched — so these tests run the rendered extension in Node
// against a synthetic Pi, the same mechanism TestOpencodeRedactionBoundaries
// uses for the opencode plugin. Every identifier here is synthetic.

import (
	"context"
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// ── a synthetic Pi ──────────────────────────────────────────────────────

// piCall is one tool_call event delivered to the extension. Several calls in
// one case share ONE extension instance, which is how "a new session does
// not inherit the last one's window" and "a fork reconstructs its own
// generation" are asked as questions about process-wide state.
type piCall struct {
	Session      string         `json:"session"`
	Entries      any            `json:"entries"`
	Command      string         `json:"command"`
	ToolCallID   string         `json:"toolCallId"`
	ToolName     string         `json:"toolName"`
	NoSession    bool           `json:"noSession"`
	BranchThrows bool           `json:"branchThrows"`
	Event        map[string]any `json:"event"`
}

type piOutcome struct {
	Command  string          `json:"command"`
	Returned json.RawMessage `json:"returned"`
	Threw    string          `json:"threw"`
}

// piEntry builders mirror Pi's persisted session entries: a message entry
// wraps an AgentMessage, an assistant message's content is always an array
// of parts, a tool call inside one is a part whose id field is `id`, and a
// compaction is its own top-level entry type.
func piUser(text string) map[string]any {
	return map[string]any{"type": "message", "message": map[string]any{"role": "user", "content": text}}
}

func piAssistant(text string, toolCallIDs ...string) map[string]any {
	content := []map[string]any{}
	if text != "" {
		content = append(content, map[string]any{"type": "text", "text": text})
	}
	for _, id := range toolCallIDs {
		content = append(content, map[string]any{"type": "toolCall", "id": id, "name": "bash", "arguments": map[string]any{}})
	}
	return map[string]any{"type": "message", "message": map[string]any{"role": "assistant", "content": content}}
}

func piCompaction() map[string]any {
	return map[string]any{"type": "compaction", "summary": "synthetic summary", "firstKeptEntryId": "e1", "tokensBefore": 1000}
}

const piSearchCommand = `dropin-miner search -format model "q"`

// runPiExtension executes the rendered extension in Node, one fresh
// extension instance per case, and returns each call's outcome.
func runPiExtension(t *testing.T, cases map[string][]piCall) map[string][]piOutcome {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Fatal("node is required to verify the embedded Pi extension")
	}
	script := `
 const fs = await import('node:fs');
 const input = JSON.parse(fs.readFileSync(0,'utf8'));
 const mod = await import('data:text/javascript;base64,'+Buffer.from(input.extension).toString('base64'));
 const result = {};
 for (const [name, calls] of Object.entries(input.cases)) {
  let handler;
  // The synthetic Pi: an extension registers with pi.on(event, handler).
  mod.default({ on: (ev, fn) => { if (ev === 'tool_call') handler = fn; } });
  const outcomes = [];
  for (const c of calls) {
   const event = c.event ?? { type: 'tool_call', toolName: c.toolName || 'bash', toolCallId: c.toolCallId, input: { command: c.command } };
   const ctx = c.noSession ? {} : { sessionManager: {
     getSessionId: () => c.session,
     getBranch: () => { if (c.branchThrows) throw new Error('synthetic host failure'); return c.entries; },
   } };
   const out = { command: null, returned: null, threw: '' };
   try { out.returned = (await handler(event, ctx)) ?? null; } catch (e) { out.threw = String(e); }
   out.command = event?.input?.command ?? null;
   outcomes.push(out);
  }
  result[name] = outcomes;
 }
 process.stdout.write(JSON.stringify(result));`
	input, err := json.Marshal(map[string]any{"extension": renderAgentScript(piExtensionTS), "cases": cases})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, node, "--input-type=module", "-e", script) // #nosec G204 -- fixed test script and local Node runtime; synthetic input on stdin
	cmd.Stdin = strings.NewReader(string(input))
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("extension: %v\n%s", err, output)
	}
	var results map[string][]piOutcome
	if err := json.Unmarshal(output, &results); err != nil {
		t.Fatalf("%v\n%s", err, output)
	}
	return results
}

// piBridge decodes the envelope the extension put in front of the command,
// and fails the test if there is none.
func piBridge(t *testing.T, o piOutcome) *traceEnvelope {
	t.Helper()
	if o.Threw != "" {
		t.Fatalf("extension threw, which BLOCKS the tool call in Pi: %s", o.Threw)
	}
	return decodeBridgeFromCommand(t, o.Command)
}

// piNoBridge asserts the command came back exactly as it went in.
func piNoBridge(t *testing.T, o piOutcome, original string) {
	t.Helper()
	if o.Threw != "" {
		t.Fatalf("extension threw, which BLOCKS the tool call in Pi: %s", o.Threw)
	}
	if o.Command != original {
		t.Fatalf("command was rewritten when it should not have been:\n got %q\nwant %q", o.Command, original)
	}
}

// ── the adapter is an observer, never an authority ──────────────────────

// A Pi tool_call handler gates execution by RETURNING something: {block:
// true} refuses the call, and a thrown error refuses it too (Pi rethrows as
// "Extension failed, blocking execution"). This extension must do neither,
// for any input at all — recognizing our own search command tells us where a
// trace belongs, and says nothing about whether the command may run. The
// guard is deliberately independent of search recognition: it holds for the
// commands we rewrite and the ones we ignore alike.
func TestPiExtensionNeverAuthorizesOrBlocksAToolCall(t *testing.T) {
	cases := map[string][]piCall{
		"our search":      {{Session: "s", Command: piSearchCommand, ToolCallID: "c1", Entries: []any{}}},
		"foreign command": {{Session: "s", Command: "rm -rf /", ToolCallID: "c1", Entries: []any{}}},
		"already bridged": {{Session: "s", Command: "TOKENDROP_TRACE_BRIDGE=x " + piSearchCommand, ToolCallID: "c1", Entries: []any{}}},
		"other tool":      {{Session: "s", ToolName: "read", Command: piSearchCommand, ToolCallID: "c1", Entries: []any{}}},
		"no session":      {{NoSession: true, Command: piSearchCommand, ToolCallID: "c1", Entries: []any{}}},
		"branch throws":   {{Session: "s", Command: piSearchCommand, ToolCallID: "c1", BranchThrows: true}},
		"entries garbage": {{Session: "s", Command: piSearchCommand, ToolCallID: "c1", Entries: "not-a-branch"}},
		"event garbage":   {{Event: map[string]any{"toolName": "bash"}}},
		"empty event":     {{Event: map[string]any{}}},
	}
	for name, outcomes := range runPiExtension(t, cases) {
		t.Run(name, func(t *testing.T) {
			o := outcomes[0]
			if o.Threw != "" {
				t.Fatalf("extension threw, which Pi turns into a blocked tool call: %s", o.Threw)
			}
			if s := strings.TrimSpace(string(o.Returned)); s != "null" {
				t.Fatalf("handler returned %s; a truthy return is how a Pi extension gates execution, and this one must never gate anything", s)
			}
		})
	}
}

// ── recognition ─────────────────────────────────────────────────────────

// The extension must reach the SAME verdict as the binary's own
// recognizer for the same command — that is what the drift guard on the
// pattern is for, checked here as behavior rather than as text.
func TestPiExtensionRewritesOurSearchAndNothingElse(t *testing.T) {
	entries := []any{piUser("find me a thing"), piAssistant("Looking that up now.", "c1")}
	commands := map[string]string{
		"bare":              piSearchCommand,
		"absolute path":     `/opt/bin/dropin-miner search "q"`,
		"quoted path":       `"/opt/bin/dropin-miner" search "q"`,
		"windows exe":       `C:\bin\dropin-miner.exe search "q"`,
		"after a pipe":      `echo hi | dropin-miner search "q"`,
		"lookalike binary":  "dropin-miner-helper search q",
		"another tool":      "grep -rn dropinminer .",
		"another verb":      "dropin-miner status",
		"our name in prose": "echo 'run dropin-miner to search'",
		"already bridged":   "TOKENDROP_TRACE_BRIDGE=stale " + piSearchCommand,
	}
	cases := map[string][]piCall{}
	for name, cmd := range commands {
		cases[name] = []piCall{{Session: "s", Command: cmd, ToolCallID: "c1", Entries: entries}}
	}
	// A tool that is not the shell is never rewritten, whatever it carries.
	cases["other tool"] = []piCall{{Session: "s", ToolName: "read", Command: piSearchCommand, ToolCallID: "c1", Entries: entries}}

	got := runPiExtension(t, cases)
	for name, cmd := range commands {
		t.Run(name, func(t *testing.T) {
			want := isSearchCommand(cmd) && !strings.Contains(cmd, bridgeEnv+"=")
			if !want {
				piNoBridge(t, got[name][0], cmd)
				return
			}
			if env := piBridge(t, got[name][0]); env.Harness != "pi" {
				t.Errorf("harness = %q, want pi", env.Harness)
			}
		})
	}
	piNoBridge(t, got["other tool"][0], piSearchCommand)
}

// ── identity ────────────────────────────────────────────────────────────

func TestPiExtensionHashesHostIdentifiers(t *testing.T) {
	const sid, callID = "pi-session-synthetic-0123", "toolcall-synthetic-0123"
	got := runPiExtension(t, map[string][]piCall{
		"hashed": {{Session: sid, Command: piSearchCommand, ToolCallID: callID,
			Entries: []any{piUser("q"), piAssistant("Searching.", callID)}}},
	})
	o := got["hashed"][0]
	env := piBridge(t, o)
	if env.SessionID != traceHash(sid) {
		t.Errorf("session_id = %q, want the domain-separated hash", env.SessionID)
	}
	if env.CallID != traceHash(sid+"|"+callID) {
		t.Errorf("call_id = %q, want hash(session|toolCallId)", env.CallID)
	}
	// Pi exposes no turn identifier to an extension, so the envelope must
	// carry none rather than a fabricated one.
	if env.TurnID != "" {
		t.Errorf("turn_id = %q, but Pi has no turn id an extension can read", env.TurnID)
	}
	if strings.Contains(o.Command, sid) || strings.Contains(o.Command, callID) {
		t.Error("a raw host identifier reached the command line")
	}
}

// ── history belongs to THIS tool call ───────────────────────────────────

// "The latest assistant text" is the wrong question, and these are the
// cases where it gives a wrong answer: prose from the previous turn, prose
// from a sibling tool call's own turn, and a first search in a fresh turn
// whose only assistant text is older than the user's last message.
func TestPiExtensionBindsHistoryToTheCurrentToolCall(t *testing.T) {
	const stale = "PREVIOUS TURN PROSE, must never be inherited"
	const current = "Current turn: looking that up now."
	previousTurn := []any{piUser("first question"), piAssistant(stale, "c-old")}
	cases := map[string][]piCall{
		// The assistant message that emitted this call carries the prose.
		"current call": {{Session: "s", ToolCallID: "c1", Command: piSearchCommand,
			Entries: append(append([]any{}, previousTurn...), piUser("second question"), piAssistant(current, "c1"))}},
		// A call id nobody on the branch emitted: no provable owner.
		"unrelated call": {{Session: "s", ToolCallID: "c-unknown", Command: piSearchCommand,
			Entries: append(append([]any{}, previousTurn...), piUser("second question"), piAssistant(current, "c1"))}},
		// The current turn opened with a tool call and no prose of its own;
		// the only assistant text on the branch is the previous turn's.
		"first search in a new turn": {{Session: "s", ToolCallID: "c1", Command: piSearchCommand,
			Entries: append(append([]any{}, previousTurn...), piUser("second question"), piAssistant("", "c1"))}},
		// No user message at all: no turn boundary to floor the scan at.
		"no turn boundary": {{Session: "s", ToolCallID: "c1", Command: piSearchCommand,
			Entries: []any{piAssistant(stale, "c1")}}},
		// The call is emitted by an older assistant message; text that came
		// after it belongs to a later turn and must not be picked up.
		"later turn ignored": {{Session: "s", ToolCallID: "c-old", Command: piSearchCommand,
			Entries: append(append([]any{}, previousTurn...), piUser("second question"), piAssistant(current, "c1"))}},
	}
	got := runPiExtension(t, cases)

	env := piBridge(t, got["current call"][0])
	if len(env.History) != 1 || env.History[0].Text != current {
		t.Fatalf("current call did not get its own assistant text: %+v", env.History)
	}
	for _, name := range []string{"unrelated call", "first search in a new turn", "no turn boundary"} {
		env := piBridge(t, got[name][0])
		if len(env.History) != 0 {
			t.Errorf("%s: attached history it could not prove belongs here: %+v", name, env.History)
		}
	}
	older := piBridge(t, got["later turn ignored"][0])
	if len(older.History) != 1 || older.History[0].Text != stale {
		t.Fatalf("a call from an earlier turn got the wrong turn's prose: %+v", older.History)
	}
	for _, name := range []string{"unrelated call", "first search in a new turn", "no turn boundary"} {
		if strings.Contains(got[name][0].Command, "PREVIOUS") {
			t.Errorf("%s: previous-turn prose reached the bridge", name)
		}
	}
}

// ── the window generation, reconstructed from persisted state ───────────

// A process-global counter gets every one of these wrong. The generation is
// counted from the compaction entries on the current branch, so it survives
// a restart (the entries are read back from the session file), stays
// per-session when two sessions share one extension instance, and follows a
// fork rather than the process's own history.
func TestPiExtensionReconstructsTheWindowGeneration(t *testing.T) {
	turn := func(extra ...any) []any {
		return append(extra, piUser("q"), piAssistant("Searching.", "c1"))
	}
	cases := map[string][]piCall{
		"fresh":    {{Session: "s1", ToolCallID: "c1", Command: piSearchCommand, Entries: turn()}},
		"one":      {{Session: "s1", ToolCallID: "c1", Command: piSearchCommand, Entries: turn(piCompaction())}},
		"multiple": {{Session: "s1", ToolCallID: "c1", Command: piSearchCommand, Entries: turn(piCompaction(), piCompaction(), piCompaction())}},
		// A restart: this extension instance never saw the compaction
		// happen, it only reads the branch Pi persisted.
		"resumed": {{Session: "s1", ToolCallID: "c1", Command: piSearchCommand, Entries: turn(piCompaction(), piCompaction())}},
		// One instance, two sessions: the second must not inherit the first.
		"second session": {
			{Session: "s1", ToolCallID: "c1", Command: piSearchCommand, Entries: turn(piCompaction())},
			{Session: "s2", ToolCallID: "c1", Command: piSearchCommand, Entries: turn()},
		},
		// A branch that does not contain the compaction does not count it.
		"fork": {
			{Session: "s1", ToolCallID: "c1", Command: piSearchCommand, Entries: turn(piCompaction(), piCompaction())},
			{Session: "s1", ToolCallID: "c1", Command: piSearchCommand, Entries: turn(piCompaction())},
		},
	}
	got := runPiExtension(t, cases)
	for _, tc := range []struct {
		name, want string
		call       int
	}{
		{"fresh", "none", 0}, {"one", "1", 0}, {"multiple", "3", 0}, {"resumed", "2", 0},
		{"second session", "none", 1}, {"fork", "1", 1},
	} {
		if w := piBridge(t, got[tc.name][tc.call]).Window; w != tc.want {
			t.Errorf("%s: window = %q, want %q", tc.name, w, tc.want)
		}
	}
}

// ── malformed host data fails open ──────────────────────────────────────

func TestPiExtensionFailsOpenOnMalformedHostData(t *testing.T) {
	cases := map[string][]piCall{
		"branch throws":     {{Session: "s", ToolCallID: "c1", Command: piSearchCommand, BranchThrows: true}},
		"entries not array": {{Session: "s", ToolCallID: "c1", Command: piSearchCommand, Entries: "nonsense"}},
		"entries null":      {{Session: "s", ToolCallID: "c1", Command: piSearchCommand, Entries: nil}},
		"junk entries": {{Session: "s", ToolCallID: "c1", Command: piSearchCommand,
			Entries: []any{nil, 7, "x", map[string]any{"type": "message"}, map[string]any{"type": "message", "message": map[string]any{"role": "assistant", "content": "a string, not parts"}}}}},
		"no session manager": {{NoSession: true, ToolCallID: "c1", Command: piSearchCommand}},
		"empty session":      {{Session: "", ToolCallID: "c1", Command: piSearchCommand, Entries: []any{}}},
		"no tool call id": {{Session: "s", ToolCallID: "", Command: piSearchCommand,
			Entries: []any{piUser("q"), piAssistant("text", "c1")}}},
	}
	got := runPiExtension(t, cases)
	// Host state we cannot read costs the history, never the search: the
	// identifiers still thread where the session is known.
	for _, name := range []string{"branch throws", "entries not array", "entries null", "junk entries", "no tool call id"} {
		env := piBridge(t, got[name][0])
		if len(env.History) != 0 {
			t.Errorf("%s: invented history out of unreadable state", name)
		}
	}
	// No session identity at all: no bridge, and the search falls back to
	// its own per-shell trace rather than to a bridge that means nothing.
	for _, name := range []string{"no session manager", "empty session"} {
		piNoBridge(t, got[name][0], piSearchCommand)
	}
}

// ── redaction and the caps, at the boundary ─────────────────────────────

// The bridge is a process argument: whatever is in it is visible in `ps` the
// moment the command runs. So the extension must scrub BEFORE it truncates
// and before it builds the envelope — never hand raw text to the binary to
// be cleaned up later. These are the same synthetic canaries the Go and
// opencode boundary tests use, placed so that each one straddles the cap.
func TestPiExtensionRedactsAndCapsBeforeTheBridge(t *testing.T) {
	cases := map[string][]piCall{}
	inputs := traceBoundaryInputs()
	for name, text := range inputs {
		cases[name] = []piCall{{Session: "s", ToolCallID: "c1", Command: piSearchCommand,
			Entries: []any{piUser("q"), piAssistant(text, "c1")}}}
	}
	// A complete entry over the source budget is omitted whole, never
	// sliced: slicing is what cuts a secret in half and hides it from the
	// scrubber.
	oversize := "sk-SyntheticCredentialBody0123456789 " + strings.Repeat("x", hookTailBytes)
	cases["source_budget"] = []piCall{{Session: "s", ToolCallID: "c1", Command: piSearchCommand,
		Entries: []any{piUser("q"), piAssistant(oversize, "c1")}}}
	// An envelope too large even after history is dropped yields no bridge
	// at all — a trace must never push a valid search past the host's
	// command-length limit.
	cases["envelope_budget"] = []piCall{{Session: "s", ToolCallID: "c1", Command: piSearchCommand,
		Entries: []any{piUser("q"), piAssistant(strings.Repeat("\"", traceHistoryCap), "c1")}}}

	got := runPiExtension(t, cases)
	for name, text := range inputs {
		t.Run(name, func(t *testing.T) {
			env := piBridge(t, got[name][0])
			if len(env.History) != 1 {
				t.Fatal("bounded source lost history")
			}
			assertPreparedHistory(t, env.History[0].Text, text)
		})
	}
	t.Run("source_budget", func(t *testing.T) {
		o := got["source_budget"][0]
		if env := piBridge(t, o); len(env.History) != 0 {
			t.Fatal("an over-budget entry was sliced instead of omitted")
		}
		if strings.Contains(o.Command, "CredentialBody0123456789") {
			t.Fatal("a synthetic credential reached the command line")
		}
	})
	t.Run("envelope_budget", func(t *testing.T) {
		env := piBridge(t, got["envelope_budget"][0])
		if len(env.History) != 0 {
			t.Fatal("an oversized envelope kept its history")
		}
	})
}
