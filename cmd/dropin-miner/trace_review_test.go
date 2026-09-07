package main

// Batch-1 T3: inverted from the pre-merge review's A6 reproductions
// (originally leak demos — green when vulnerable). Two chokepoints now
// scrub trace history text: capTrace (trace.go), which sits downstream of
// every path that builds a traceEnvelope before it is encoded into the
// bridge or marshaled into the search body; and saveLineage (miner.go),
// which every lineage-sidecar write passes through, including
// hookCursor's afterAgentThought/afterAgentResponse handlers that write
// straight into a lineageFile and never touch a traceEnvelope at all.

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// synthetic, credential-shaped values a model might emit in its VISIBLE
// response while narrating what it just read.
var (
	traceReviewKey    = "sk-or-v1-" + strings.Repeat("a", 24) + "SECRET"
	traceReviewBearer = "Bearer " + strings.Repeat("b", 20) + "TOKEN"
	traceReviewDBURL  = "postgres://admin:" + "hunter2pw" + "@db.internal/prod"
	traceReviewEmail  = "quasarai" + "@" + "protonmail.com"
	traceReviewHome   = "/Users/" + "realname" + "/.aws/credentials"
)

func mustJSONString(t *testing.T, s string) string {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// extractBridge pulls the base64 the hook prepended, exactly as a shell would.
func extractBridge(t *testing.T, cmd string) string {
	t.Helper()
	if !strings.HasPrefix(cmd, bridgeEnv+"=") {
		t.Fatalf("command not bridged: %q", cmd)
	}
	rest := strings.TrimPrefix(cmd, bridgeEnv+"=")
	return rest[:strings.IndexByte(rest, ' ')]
}

// TestCompletionSecretsAreRedactedBeforeTheWire is A6-1 inverted: the
// assistant's visible completion text still rides to the router — the
// brief was explicit that the text channel itself is not this task's to
// remove — but every credential-shaped substring in it is gone by the
// time the router sees it. End to end: transcript -> hook bridge ->
// /v1/search body, the real production path, not a direct call to
// redact.String.
func TestCompletionSecretsAreRedactedBeforeTheWire(t *testing.T) {
	assistantText := "I read your config. The key is " + traceReviewKey +
		", auth header " + traceReviewBearer + ", db " + traceReviewDBURL +
		", owner " + traceReviewEmail + ", file " + traceReviewHome + ". Let me search for how to rotate it."

	fs, hops := newFakeHookOps(nil)
	transcript := strings.Join([]string{
		`{"type":"user","message":{"role":"user","content":"why is my deploy failing"}}`,
		`{"type":"assistant","message":{"role":"assistant","content":[` +
			`{"type":"text","text":` + mustJSONString(t, assistantText) + `},` +
			`{"type":"tool_use","id":"toolu_x","name":"Bash"}]}}`,
	}, "\n")
	fs.files["/t/s.jsonl"] = []byte(transcript)

	hc := hookContext{cfgPath: "/c.toml", sessionsDir: "/sessions"}
	var out strings.Builder
	origCmd := `dropin-miner search "how do I rotate an openrouter key"`
	hookLineage(hops, hc, mustJSON(t, map[string]any{
		"session_id": "sess-1", "prompt_id": "p-1", "tool_use_id": "toolu_x",
		"tool_name": "Bash", "cwd": "/home/u/project", "transcript_path": "/t/s.jsonl",
		"tool_input": map[string]any{"command": origCmd},
	}), &out)

	var resp struct {
		Out struct {
			Input map[string]any `json:"updatedInput"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(out.String()), &resp); err != nil {
		t.Fatalf("hook produced no rewrite: %q", out.String())
	}
	bridge := extractBridge(t, resp.Out.Input["command"].(string))

	// Sanity: the hook captured the completion text at all — checked by a
	// scrub marker, not the raw key, since capTrace has already redacted
	// it by the time it is in the bridge.
	env := decodeTraceBridge(bridge)
	if env == nil || len(env.History) != 1 || !strings.Contains(env.History[0].Text, "[REDACTED]") {
		t.Fatalf("hook did not carry (redacted) completion text: %+v", env)
	}
	for _, secret := range []string{traceReviewKey, traceReviewBearer, traceReviewEmail, traceReviewHome} {
		if strings.Contains(env.History[0].Text, secret) {
			t.Errorf("capTrace did not scrub %q from the bridge envelope", secret)
		}
	}

	// The wire: feed that bridge to `search` and capture the body sent.
	fr, cfg, root := newFakeRouter(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(routerBody)) })
	h := fixedSearchOps(root)
	runSearch(t, h, map[string]string{"TOKENDROP_API_KEY": "sr-fictional", bridgeEnv: bridge},
		"-config", cfg, "how", "do", "I", "rotate")
	_, sent := fr.last(t)
	body := string(sent)

	for _, secret := range []string{traceReviewKey, traceReviewBearer, traceReviewDBURL, traceReviewEmail, traceReviewHome} {
		if strings.Contains(body, secret) {
			t.Errorf("secret %q reached the wire unredacted, body:\n%s", secret, body)
		}
	}
}

// TestCursorDiskWriteIsRedactedWithoutEverTouchingCapTrace guards the one
// path wire-only redaction (capTrace) cannot reach: hookCursor's
// afterAgentThought/afterAgentResponse write p.Text straight into a
// lineageFile and call updateLineage/saveLineage directly — no
// traceEnvelope is ever built, so capTrace never runs. The sidecar file
// itself is read back off "disk" (the fake filesystem) and checked, not
// the lineageFile struct in memory, so this proves what a participant's
// ~/.tokendrop/sessions/<hash>.json actually contains.
func TestCursorDiskWriteIsRedactedWithoutEverTouchingCapTrace(t *testing.T) {
	fs, ops := newFakeHookOps(nil)
	hc := hookContext{sessionsDir: "/sessions"}
	workspace := "/w/proj"
	path := lineagePath(hc.sessionsDir, workspace)

	secretText := "here's what I found: " + traceReviewKey + " and " + traceReviewEmail
	payload := mustJSON(t, map[string]any{
		"conversation_id": "conv-1", "workspace_roots": []string{workspace}, "text": secretText,
	})
	hookCursor(ops, hc, "afterAgentResponse", payload, new(strings.Builder))

	raw, ok := fs.files[path]
	if !ok {
		t.Fatalf("no sidecar written at %s", path)
	}
	if strings.Contains(string(raw), traceReviewKey) || strings.Contains(string(raw), traceReviewEmail) {
		t.Fatalf("secret reached the disk sidecar unredacted:\n%s", raw)
	}
	if !strings.Contains(string(raw), "[REDACTED]") {
		t.Fatalf("expected a scrub marker in the sidecar, got:\n%s", raw)
	}

	l, ok := loadLineage(ops, path)
	if !ok || len(l.History) == 0 {
		t.Fatalf("sidecar did not round-trip a history entry: ok=%v %+v", ok, l)
	}
	if l.History[0].Text == secretText {
		t.Fatalf("the loaded history is byte-identical to the planted secret: never scrubbed")
	}
}

// TestOptOutStillCapturesButNowRedacted is A6-2 inverted. TOKENDROP_TRACE=off
// not stopping disk capture is unchanged — the brief scoped this task to
// redaction, not to the opt-out gap, which is a separate, deferred question
// about the channel itself. What changes: whatever DOES land on disk is no
// longer plaintext.
func TestOptOutStillCapturesButNowRedacted(t *testing.T) {
	fs, hops := newFakeHookOps(map[string]string{"TOKENDROP_TRACE": "off"})
	secret := "my private prose containing " + traceReviewKey
	transcript := strings.Join([]string{
		`{"type":"user","message":{"role":"user","content":"help"}}`,
		`{"type":"assistant","message":{"role":"assistant","content":[` +
			`{"type":"text","text":` + mustJSONString(t, secret) + `},` +
			`{"type":"tool_use","id":"toolu_y","name":"Bash"}]}}`,
	}, "\n")
	fs.files["/t/s.jsonl"] = []byte(transcript)

	hc := hookContext{cfgPath: "/c.toml", sessionsDir: "/sessions"}
	var out strings.Builder
	hookLineage(hops, hc, mustJSON(t, map[string]any{
		"session_id": "s-off", "prompt_id": "p", "tool_use_id": "toolu_y",
		"tool_name": "Bash", "cwd": "/home/u/project", "transcript_path": "/t/s.jsonl",
		"tool_input": map[string]any{"command": `dropin-miner search q`},
	}), &out)

	l, ok := loadLineage(hops, lineagePath("/sessions", "/home/u/project"))
	if !ok {
		t.Fatalf("no lineage file written")
	}
	if len(l.History) == 0 {
		t.Fatalf("expected completion text on disk despite opt-out (unchanged, deferred behavior), got none")
	}
	if strings.Contains(l.History[0].Text, traceReviewKey) {
		t.Errorf("the key reached disk unredacted: %+v", l.History)
	}
	if !strings.Contains(l.History[0].Text, "[REDACTED]") {
		t.Errorf("expected a scrub marker in the disk copy: %+v", l.History)
	}
	// And the hook STILL rewrote the command with a live bridge, despite off.
	if out.Len() == 0 {
		t.Errorf("hook stayed silent under opt-out (would be a behavior change out of this task's scope)")
	}
}
