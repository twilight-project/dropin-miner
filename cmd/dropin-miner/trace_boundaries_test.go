package main

import (
	"context"
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/twilight-project/dropin-miner/pkg/redact"
)

func traceBoundaryInputs() map[string]string {
	shapes := map[string]string{ // #nosec G101 -- synthetic redaction canaries, including fictional URL userinfo
		"sk": "sk-SyntheticCredentialBody0123456789", "sr": "sr-SyntheticCredentialBody0123456789",
		"github_o": "gho_SyntheticCredentialBody0123456789",
		"github_u": "ghu_SyntheticCredentialBody0123456789",
		"github_s": "ghs_SyntheticCredentialBody0123456789",
		"github_r": "ghr_SyntheticCredentialBody0123456789",
		"sk_or":    "sk-or-v1-SyntheticCredentialBody0123456789",
		"sk_ant":   "sk-ant-SyntheticCredentialBody0123456789",
		"github":   "ghp_SyntheticCredentialBody0123456789", "github_pat": "github_pat_SyntheticCredentialBody0123456789",
		"aws": "AKIA0123456789ABCDEF", "jwt": "eyJSynthetic.SyntheticPayload.SyntheticSignature",
		"userinfo": "https://synthetic:PasswordCanary0123456789@example.test", "email": "syntheticperson012345@example.test",
		"unix_home": "/home/SyntheticPerson012345/private", "mac_home": "/Users/SyntheticPerson012345/private",
		"windows_home": `C:\Users\SyntheticPerson012345\private`,
	}
	out := map[string]string{}
	for name, secret := range shapes {
		for label, cut := range map[string]int{"before": len(secret) + 2, "crossing": len(secret) / 2, "prefix": 1, "beyond": -2} {
			out[name+"/"+label] = "start " + secret + " " + strings.Repeat(".", traceHistoryCap-len(secret)-1+cut)
		}
		out[name+"/multiple"] = "start " + secret + " " + secret + " " + strings.Repeat(".", traceHistoryCap-len(secret))
		out[name+"/utf8"] = "start " + secret + " " + strings.Repeat("界", traceHistoryCap/3-3) + "é"
	}
	return out
}

func assertPreparedHistory(t *testing.T, text, source string) {
	t.Helper()
	expected := redact.TraceText(source)
	if len(expected) > traceHistoryCap {
		expected = expected[len(expected)-traceHistoryCap:]
		for len(expected) > 0 && !utf8.RuneStart(expected[0]) {
			expected = expected[1:]
		}
	}
	if text != expected {
		t.Fatalf("prepared history differs from complete-source scrub followed by UTF-8 cap (got %d bytes, want %d)", len(text), len(expected))
	}
	if len(text) > traceHistoryCap || !utf8.ValidString(text) {
		t.Fatalf("history exceeds byte cap or is invalid UTF-8: %d", len(text))
	}
	for _, fragment := range []string{"CredentialBody0123456789", "0123456789ABCDEF", "SyntheticPayload", "PasswordCanary0123456789", "syntheticperson012345", "SyntheticPerson012345"} {
		if strings.Contains(text, fragment) {
			t.Fatalf("prepared history retains synthetic marker %q", fragment)
		}
	}
}

func TestTraceRedactionBoundaries(t *testing.T) {
	for name, text := range traceBoundaryInputs() {
		t.Run(name, func(t *testing.T) {
			t.Run("envelope", func(t *testing.T) {
				env := capTrace(&traceEnvelope{V: 1, History: []traceHistory{{Role: "assistant", Text: text}}})
				if env == nil || len(env.History) != 1 {
					t.Fatal("ordinary bounded source lost history")
				}
				assertPreparedHistory(t, env.History[0].Text, text)
			})
			t.Run("transcript", func(t *testing.T) {
				fs, ops := newFakeHookOps(nil)
				record, _ := json.Marshal(map[string]any{"type": "assistant", "message": map[string]any{"content": []map[string]string{{"type": "text", "text": text}}}})
				fs.files["/transcript"] = append([]byte("{\"type\":\"user\",\"message\":{\"content\":\"query\"}}\n"), record...)
				env := capTrace(&traceEnvelope{V: 1, History: []traceHistory{{Role: "assistant", Text: currentAssistantText(ops, hookPayload{TranscriptPath: "/transcript"})}}})
				if env == nil || len(env.History) != 1 || env.History[0].Text == "" {
					t.Fatal("bounded transcript lost history")
				}
				assertPreparedHistory(t, env.History[0].Text, text)
			})
			t.Run("cursor", func(t *testing.T) {
				_, ops := newFakeHookOps(nil)
				runHook(t, ops, hookContext{sessionsDir: "/sessions"}, "cursor afterAgentResponse", map[string]any{"conversation_id": "synthetic", "cwd": "/workspace", "text": text})
				l, ok := loadLineage(ops, lineagePath("/sessions", "/workspace"))
				if !ok || len(l.History) != 1 {
					t.Fatal("missing history")
				}
				assertPreparedHistory(t, l.History[0].Text, text)
			})
		})
	}
}

func TestOpencodeRedactionBoundaries(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Fatal("node is required to verify the embedded opencode plugin")
	}
	script := `
 const fs = await import('node:fs');
 const input = JSON.parse(fs.readFileSync(0,'utf8'));
 const plugin = await import('data:text/javascript;base64,'+Buffer.from(input.plugin).toString('base64'));
 const result={};
 for (const [name,text] of Object.entries(input.cases)) {
  const hooks=await plugin.DropinMinerLineage({client:{session:{messages:async()=>({data:[{info:{role:'assistant'},parts:[{type:'text',text}]}]})}}});
  const output={args:{command:'dropin-miner search query'}};
  await hooks['tool.execute.before']({tool:'bash',sessionID:'synthetic',callID:'call'},output);
  const bridge=output.args.command.split(' ')[0].split('=')[1];
  result[name]=JSON.parse(Buffer.from(bridge,'base64url').toString('utf8'));
 }
 process.stdout.write(JSON.stringify(result));`
	cases := traceBoundaryInputs()
	cases["source_budget"] = strings.Repeat("x", hookTailBytes+1)
	cases["envelope_budget"] = strings.Repeat("\"", traceHistoryCap)
	cases["max_source"] = strings.Repeat("x", hookTailBytes)
	cases["punctuated_source"] = strings.Repeat("x.", hookTailBytes/2)
	cases["userinfo_numeric_prefix"] = "123https://synthetic:PasswordCanary0123456789@example.test"
	cases["email_leading_punctuation"] = ".+syntheticperson012345@example.test"
	cases["ordinary_prose"] = "bearer tokens expire soon; ssh deploy@example.test; git@example.test:repo; https://example.test/home/page"
	cases["utf8_tail"] = strings.Repeat("界", traceHistoryCap/3+2)
	// The rendered artifact — the template with the shared trace-preparation
	// source spliced in — is what `agents install` actually writes, so it is
	// what this acceptance test executes. Every assertion below is unchanged
	// from before the shared source existed: the migration is mechanical or
	// this test says otherwise.
	input, _ := json.Marshal(map[string]any{"plugin": renderAgentScript(opencodePluginJS), "cases": cases})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, node, "--input-type=module", "-e", script) // #nosec G204 -- fixed test script and local Node runtime; synthetic input on stdin
	cmd.Stdin = strings.NewReader(string(input))
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("plugin: %v %s", err, output)
	}
	var results map[string]traceEnvelope
	if err := json.Unmarshal(output, &results); err != nil {
		t.Fatal(err)
	}
	for name, text := range cases {
		t.Run(name, func(t *testing.T) {
			env, ok := results[name]
			if name == "source_budget" || name == "envelope_budget" {
				if !ok || len(env.History) != 0 {
					t.Fatal("over-budget plugin source retained history")
				}
				return
			}
			if !ok || len(env.History) != 1 {
				t.Fatal("bounded plugin source lost history")
			}
			assertPreparedHistory(t, env.History[0].Text, text)
		})
	}
}

// The single-source-of-truth guard. Each JavaScript host keeps its own
// standalone artifact — that is what the host loads — but the scrub and the
// caps inside it are spliced from one file. What makes that real rather
// than aspirational is that neither template contains the logic at all: a
// drifted second copy cannot exist in a file that has no copy.
func TestEveryJSHostRendersTheSharedTraceSource(t *testing.T) {
	shared := strings.TrimRight(agentTraceCommonJS, "\n")
	for name, template := range map[string]string{"opencode": opencodePluginJS, "pi": piExtensionTS} {
		t.Run(name, func(t *testing.T) {
			if n := strings.Count(template, traceCommonMarker); n != 1 {
				t.Fatalf("template carries the trace-common marker %d times, want exactly 1", n)
			}
			rendered := renderAgentScript(template)
			if !strings.Contains(rendered, shared) {
				t.Fatal("the rendered artifact does not contain the shared source verbatim")
			}
			if strings.Contains(rendered, traceCommonMarker) {
				t.Fatal("the rendered artifact still carries an unexpanded marker")
			}
			// The template's own text must hold none of the shared logic:
			// no redaction, no caps, no second recognizer.
			for _, forked := range []string{"[REDACTED]", "32 * 1024", "48 * 1024", "256 * 1024", "createHash", "SEARCH_RE ="} {
				if strings.Contains(template, forked) {
					t.Errorf("the %s template carries its own copy of %q instead of using the shared source", name, forked)
				}
			}
			// And the rendered artifact must have exactly one of each.
			for _, once := range []string{"const scrubTraceText", "const prepareTraceHistory", "const traceBridge", "const SEARCH_RE"} {
				if n := strings.Count(rendered, once); n != 1 {
					t.Errorf("rendered %s has %d definitions of %q, want 1", name, n, once)
				}
			}
		})
	}
}

// The shared source, executed directly: the same canaries the Go and
// opencode boundary tests use, plus the four budget edges, asserted against
// the functions both adapters actually call.
func TestSharedTraceSourceRedactionBoundaries(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Fatal("node is required to verify the shared trace-preparation source")
	}
	script := `
 const fs = await import('node:fs');
 const input = JSON.parse(fs.readFileSync(0,'utf8'));
 const src = input.shared + "\nexport { prepareTraceHistory, traceBridge, needsTraceBridge, traceHash };";
 const m = await import('data:text/javascript;base64,'+Buffer.from(src).toString('base64'));
 const result = {};
 for (const [name, parts] of Object.entries(input.cases)) {
  const text = m.prepareTraceHistory(parts);
  const env = { v: 1, harness: 'test', session_id: m.traceHash('s') };
  if (text) env.history = [{ role: 'assistant', text }];
  result[name] = { text, bridge: m.traceBridge(env), history: env.history ? env.history.length : 0 };
 }
 // An envelope too large even with no history at all: no bridge.
 const huge = { v: 1, harness: 'test', session_id: 'x'.repeat(input.envelopeCap + 1) };
 result['__no_bridge'] = { bridge: m.traceBridge(huge), history: huge.history ? 1 : 0 };
 process.stdout.write(JSON.stringify(result));`

	textCases := traceBoundaryInputs()
	cases := map[string][]map[string]string{}
	for name, text := range textCases {
		cases[name] = []map[string]string{{"type": "text", "text": text}}
	}
	// The complete source budget is measured in BYTES across all parts,
	// joined with one newline each — over it, the entry is omitted whole
	// rather than sliced, because slicing is what hides a severed secret
	// from the scrubber.
	cases["source_budget"] = []map[string]string{{"type": "text", "text": strings.Repeat("x", hookTailBytes+1)}}
	cases["max_source"] = []map[string]string{{"type": "text", "text": strings.Repeat("x", hookTailBytes)}}
	cases["source_budget_across_parts"] = []map[string]string{
		{"type": "text", "text": strings.Repeat("x", hookTailBytes/2)},
		{"type": "text", "text": strings.Repeat("y", hookTailBytes/2)},
	}
	cases["utf8_source_budget"] = []map[string]string{{"type": "text", "text": strings.Repeat("界", hookTailBytes/3+1)}}
	cases["history_cap"] = []map[string]string{{"type": "text", "text": strings.Repeat("z", traceHistoryCap*2)}}
	cases["envelope_drops_history"] = []map[string]string{{"type": "text", "text": strings.Repeat("\"", traceHistoryCap)}}

	input, _ := json.Marshal(map[string]any{"shared": agentTraceCommonJS, "cases": cases, "envelopeCap": traceEnvelopeCap})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, node, "--input-type=module", "-e", script) // #nosec G204 -- fixed test script and local Node runtime; synthetic input on stdin
	cmd.Stdin = strings.NewReader(string(input))
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("shared source: %v\n%s", err, output)
	}
	var results map[string]struct {
		Text    *string `json:"text"`
		Bridge  *string `json:"bridge"`
		History int     `json:"history"`
	}
	if err := json.Unmarshal(output, &results); err != nil {
		t.Fatal(err)
	}

	for name, text := range textCases {
		t.Run(name, func(t *testing.T) {
			got := results[name]
			if got.Text == nil {
				t.Fatal("a bounded source was omitted whole")
			}
			assertPreparedHistory(t, *got.Text, text)
		})
	}
	t.Run("over the source budget is omitted whole", func(t *testing.T) {
		for _, name := range []string{"source_budget", "source_budget_across_parts", "utf8_source_budget"} {
			if got := results[name]; got.Text != nil {
				t.Errorf("%s: sliced an over-budget source instead of omitting it (%d bytes kept)", name, len(*got.Text))
			}
		}
		if results["max_source"].Text == nil {
			t.Error("a source exactly at the budget was omitted")
		}
	})
	t.Run("history is capped at the tail", func(t *testing.T) {
		got := results["history_cap"]
		if got.Text == nil || len(*got.Text) != traceHistoryCap {
			t.Fatalf("history was not capped to %d bytes: %v", traceHistoryCap, got.Text)
		}
	})
	t.Run("an oversized envelope drops history first", func(t *testing.T) {
		got := results["envelope_drops_history"]
		if got.Bridge == nil {
			t.Fatal("no bridge at all, when dropping history would have been enough")
		}
		env := decodeTraceBridge(*got.Bridge)
		if env == nil || len(env.History) != 0 {
			t.Fatalf("history survived an oversized envelope: %+v", env)
		}
	})
	t.Run("an envelope oversized without history yields no bridge", func(t *testing.T) {
		if b := results["__no_bridge"].Bridge; b != nil {
			t.Fatalf("a bridge was built past the envelope cap (%d bytes)", len(*b))
		}
	})
}

func TestTraceSourceBudget(t *testing.T) {
	text := strings.Repeat("x", hookTailBytes+1)
	if got := prepareTraceText(text); got != "" {
		t.Fatal("oversize source retained history")
	}
	fs, ops := newFakeHookOps(nil)
	record, _ := json.Marshal(map[string]any{"type": "assistant", "message": map[string]any{"content": []map[string]string{{"type": "text", "text": text}}}})
	fs.files["/transcript"] = append([]byte("{\"type\":\"user\",\"message\":{\"content\":\"q\"}}\n"), record...)
	if got := currentAssistantText(ops, hookPayload{TranscriptPath: "/transcript"}); got != "" {
		t.Fatal("incomplete transcript entry retained history")
	}
}
