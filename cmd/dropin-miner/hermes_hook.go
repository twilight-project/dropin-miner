package main

// Hermes' pre_tool_call shell hook.
//
// Hermes runs a declared hook as a subprocess, writes the tool call to its
// stdin as JSON, and reads one JSON directive back from its stdout. On a
// terminal command that runs our search we answer with a modify directive
// prefixing the command with TOKENDROP_TRACE_BRIDGE=<envelope>; on anything
// else we print nothing at all and exit 0, which Hermes treats as a clean
// no-op and the search runs with its per-shell trace instead.
//
// OBSERVER, NEVER AN AUTHORITY. Hermes' pre_tool_call vocabulary has exactly
// two verbs, `block` and `modify`, and blocking is also reachable by exiting
// 2. We emit `modify` and only `modify`, and we exit 0 on every path: this
// hook rewrites a command Hermes has already decided to run. Recognizing our
// own search command tells us where a trace belongs; it is not a judgement
// about whether that command may execute, and the moment a lineage adapter
// starts answering that second question it has become a security control
// nobody reviewed it as. Hermes' own escalation verb (`approve`, with its
// `rule_key` allowlist grain) reaches the human approval gate, so it is in
// the forbidden set below even though the shell-hook parser cannot reach it
// today — the gap between the two vocabularies is not ours to rely on.
//
// EVERY FIELD READ HERE IS ONE HERMES DOCUMENTS. The payload is a closed
// six-key object (hook_event_name, tool_name, tool_input, session_id, cwd,
// extra); the tool-call and turn identifiers live under `extra`, and both
// may be present-but-empty, so each is used only when non-empty. Nothing is
// inferred: a field we could not cite is a field we do not send.

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// hermesHookPayload is the pre_tool_call wire Hermes sends a shell hook.
// tool_input is the raw tool-arguments object — for the terminal tool it
// carries "command" — and is null on events that have no tool.
type hermesHookPayload struct {
	HookEventName string          `json:"hook_event_name"`
	ToolInput     json.RawMessage `json:"tool_input"`
	SessionID     string          `json:"session_id"`
	// Extra holds the identifiers Hermes does not promote to the top level.
	// Each is coerced from nil to "" before dispatch, so "present" and
	// "usable" are different questions and only the second one matters.
	// It stays raw and is decoded field by field: an `extra` of some shape
	// we did not expect should cost the optional identifiers it was
	// carrying, never the whole bridge.
	Extra json.RawMessage `json:"extra"`
}

// hermesExtraID reads one optional identifier from the payload's `extra`
// object, or "" if it is absent, empty, or not a string.
func hermesExtraID(extra json.RawMessage, key string) string {
	if len(extra) == 0 {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal(extra, &m); err != nil {
		return ""
	}
	s, _ := m[key].(string)
	return s
}

// hermesModifyDirective is the ONLY shape this hook ever prints. It is a
// struct, not a map, so the set of keys that can reach Hermes' parser is
// fixed at compile time and a permission field cannot be added to it by
// accident — which is what TestHermesHookNeverEmitsAnApproval checks from
// the outside.
type hermesModifyDirective struct {
	Decision  string         `json:"decision"`
	ToolInput map[string]any `json:"tool_input"`
}

// hookHermes handles one pre_tool_call event. Any other event, any other
// command, any doubt at all: print nothing.
func hookHermes(event string, payload []byte, stdout io.Writer) {
	if event != "pre_tool_call" {
		return
	}
	var p hermesHookPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return
	}
	if p.HookEventName != "" && p.HookEventName != "pre_tool_call" {
		return // the payload disagrees with the argv: do nothing
	}
	// No session, no bridge. Hermes serializes a missing session id as "",
	// never null, and a hashed empty string or a random stand-in would be a
	// lineage that groups unrelated searches together — worse than none.
	// With no bridge the search falls back to its own per-shell trace, which
	// is the correct answer here and not a degraded one.
	if p.SessionID == "" {
		return
	}
	var input map[string]any
	if err := json.Unmarshal(p.ToolInput, &input); err != nil || input == nil {
		return
	}
	cmd, _ := input["command"].(string)
	if !isSearchCommand(cmd) || strings.Contains(cmd, bridgeEnv+"=") {
		return
	}

	env := &traceEnvelope{
		V:         traceVersion,
		Harness:   "hermes",
		SessionID: traceHash(p.SessionID),
	}
	// Deterministic host correlation, never a random id: the same session
	// and the same host call id must hash to the same value in every
	// process that sees them, or the router cannot join a retry to the call
	// it retried. Both ids are hashed WITH the session, so neither raw host
	// id travels and two sessions cannot collide on a shared counter-ish id.
	if id := hermesExtraID(p.Extra, "tool_call_id"); id != "" {
		env.CallID = traceHash(p.SessionID + "|" + id)
	}
	if id := hermesExtraID(p.Extra, "turn_id"); id != "" {
		env.TurnID = traceHash(p.SessionID + "|" + id)
	}
	// No window and no history: Hermes' hook payload exposes neither a
	// compaction generation nor the assistant text before the call. An
	// absent dimension is left absent — claiming window "none" would assert
	// "this session has never compacted", which this payload cannot know.
	env = capTrace(env)
	if env == nil {
		return
	}
	bridge, err := encodeTraceBridge(env)
	if err != nil {
		return
	}

	// Echo the tool input with one key changed. Hermes shallow-merges a
	// modify payload over the original arguments, so timeout, workdir and
	// every other terminal argument survive either way; echoing keeps this
	// hook's output true to what it was handed.
	updated := make(map[string]any, len(input))
	for k, v := range input {
		updated[k] = v
	}
	updated["command"] = bridgeEnv + "=" + bridge + " " + cmd
	out, err := json.Marshal(hermesModifyDirective{Decision: "modify", ToolInput: updated})
	if err != nil {
		return
	}
	fmt.Fprintln(stdout, string(out))
}
