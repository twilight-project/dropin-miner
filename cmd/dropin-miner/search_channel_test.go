package main

// A search believes its host's channel, not whatever variable it finds (#91).
//
// THE INVARIANT, stated once: a search never reaches the router labeled with
// a harness other than the one its host declared. Every assertion here is on
// the bytes the router received, because "the right function was called" is
// not what #91 reported — a request under the wrong harness was.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
)

// someoneElsesBridge is a well-formed envelope from somewhere else: the shape
// #91 recorded, a real-looking identity under another host's label.
func someoneElsesBridge(t *testing.T, harness string) string {
	t.Helper()
	b, err := encodeTraceBridge(&traceEnvelope{V: traceVersion, Harness: harness, SessionID: "foreign-session", TurnID: "foreign-turn", CallID: "foreign-call",
		History: []traceHistory{{Role: "assistant", Text: "foreign text"}}})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

type channelProbe struct {
	fr      *fakeRouter
	h       *searchHarness
	cfg     string
	lineage string // the file Cursor's session-start hook would have exported
}

func newChannelProbe(t *testing.T) *channelProbe {
	t.Helper()
	fr, cfg, root := newFakeRouter(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(routerBody)) })
	h := fixedSearchOps(filepath.Join(root, "src"))
	path := lineagePath(filepath.Join(root, "sessions"), root)
	if err := saveLineage(h.ops.hook, path, &lineageFile{Harness: "cursor", SessionID: "cursor-conv", TurnID: "cursor-gen", CallID: "cursor-call", Window: "none", Seq: 3,
		History: []traceHistory{{Role: "assistant", Text: "Let me check."}}}, h.ops.now()); err != nil {
		t.Fatal(err)
	}
	return &channelProbe{fr: fr, h: h, cfg: cfg, lineage: path}
}

func (p *channelProbe) sentTrace(t *testing.T) (map[string]any, []byte) {
	t.Helper()
	_, sent := p.fr.last(t)
	var m map[string]any
	if err := json.Unmarshal(sent, &m); err != nil {
		t.Fatalf("the router received something that is not JSON: %s", sent)
	}
	tr, _ := m["trace"].(map[string]any)
	if tr == nil {
		t.Fatalf("the search sent no trace: %s", sent)
	}
	return tr, sent
}

// Bridge and lineage variable both present: the lineage file's identity
// travels, nothing of the bridge's does, and the file's seq advances as for
// any Cursor search. The third row is the one that shows the rule is about
// the channel: a bridge naming the declared harness is dropped all the same.
func TestABridgeIsForeignToAHostWhoseChannelIsTheLineageFile(t *testing.T) {
	for _, c := range []struct {
		name          string
		bridgeHarness string
		harnessVar    string
	}{
		{"another host's label, harness exported", "claude-code", "cursor"},
		{"another host's label, only the lineage variable survived", "claude-code", ""},
		{"the declared host's own label", "cursor", "cursor"},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := newChannelProbe(t)
			env := map[string]string{"TOKENDROP_API_KEY": "k", lineageEnv: p.lineage, bridgeEnv: someoneElsesBridge(t, c.bridgeHarness)}
			if c.harnessVar != "" {
				env["TOKENDROP_HARNESS"] = c.harnessVar
			}
			code, _, stderr := runSearch(t, p.h, env, "-config", p.cfg, "q")
			if code != exitOK {
				t.Fatalf("exit %d: %s", code, stderr)
			}
			tr, sent := p.sentTrace(t)
			if tr["harness"] != "cursor" {
				t.Errorf("the search reached the router as harness %v; its host declared cursor", tr["harness"])
			}
			if tr["session_id"] != "cursor-conv" || tr["turn_id"] != "cursor-gen" || tr["call_id"] != "cursor-call" {
				t.Errorf("the lineage file's identity did not travel: %v", tr)
			}
			if tr["seq"] != float64(4) {
				t.Errorf("seq %v, want 4: the search did not advance the lineage as any Cursor search does", tr["seq"])
			}
			if bytes.Contains(sent, []byte("foreign")) {
				t.Errorf("something of the bridge reached the router: %s", sent)
			}
			if l, ok := loadLineage(p.h.ops.hook, p.lineage); !ok || l.Seq != 4 {
				t.Errorf("the lineage file's seq was not persisted: %+v", l)
			}
			if !strings.Contains(stderr, "ignoring "+bridgeEnv) {
				t.Errorf("the drop is invisible; stderr: %q", stderr)
			}
		})
	}
}

// The declaration is what drops the bridge, not the file behind it. A host
// that declared the lineage channel and whose file is gone or stale still
// wrote no bridge: the search falls back to an honest per-shell identity
// under the declared harness, never to the foreign envelope.
func TestAForeignBridgeIsNotAFallbackWhenTheDeclaredLineageIsMissing(t *testing.T) {
	p := newChannelProbe(t)
	missing := filepath.Join(filepath.Dir(p.lineage), "gone.json")
	_, _, stderr := runSearch(t, p.h, map[string]string{"TOKENDROP_API_KEY": "k", "TOKENDROP_HARNESS": "cursor", lineageEnv: missing, bridgeEnv: someoneElsesBridge(t, "claude-code")},
		"-config", p.cfg, "q")
	tr, sent := p.sentTrace(t)
	if tr["harness"] != "cursor" || bytes.Contains(sent, []byte("foreign")) {
		t.Errorf("the foreign bridge was used as a fallback: %s", sent)
	}
	if !strings.Contains(stderr, "ignoring "+bridgeEnv) {
		t.Errorf("the drop is invisible; stderr: %q", stderr)
	}
}

// Bridge alone: unchanged. This is every host whose channel IS the bridge
// (Claude Code, OpenCode, Pi, Hermes), and H-R4 is not touched.
func TestABridgeAloneIsBelievedAsBefore(t *testing.T) {
	p := newChannelProbe(t)
	_, _, stderr := runSearch(t, p.h, map[string]string{"TOKENDROP_API_KEY": "k", bridgeEnv: someoneElsesBridge(t, "claude-code")}, "-config", p.cfg, "q")
	tr, _ := p.sentTrace(t)
	if tr["harness"] != "claude-code" || tr["session_id"] != "foreign-session" || tr["turn_id"] != "foreign-turn" {
		t.Errorf("a bridge with no lineage channel declared was not used: %v", tr)
	}
	if strings.Contains(stderr, "ignoring") {
		t.Errorf("a host's own bridge was reported as foreign: %q", stderr)
	}
	if l, _ := loadLineage(p.h.ops.hook, p.lineage); l.Seq != 3 {
		t.Errorf("a bridged search moved a lineage file's seq to %d", l.Seq)
	}
}

// The form Cursor's skill actually renders is `search --stdin`. The same
// identity travels, and the envelope the model reads says nothing about the
// variable: a model that knows the mechanism is one of the ways a foreign
// bridge arrives.
func TestTheMachineFormDropsAForeignBridgeAndDoesNotTeachIt(t *testing.T) {
	p := newChannelProbe(t)
	env := map[string]string{"TOKENDROP_API_KEY": "k", "TOKENDROP_HARNESS": "cursor", lineageEnv: p.lineage, bridgeEnv: someoneElsesBridge(t, "claude-code")}
	getenv := envOf(env)
	p.h.ops.hook.getenv = getenv
	var stdout, stderr bytes.Buffer
	code := searchMain(p.h.ops, []string{"-config", p.cfg, "--stdin"}, strings.NewReader(`{"version":1,"query":"q"}`), &stdout, &stderr, getenv)
	if code != exitOK {
		t.Fatalf("exit %d: %s", code, stdout.String())
	}
	tr, sent := p.sentTrace(t)
	if tr["harness"] != "cursor" || tr["session_id"] != "cursor-conv" || bytes.Contains(sent, []byte("foreign")) {
		t.Errorf("machine form: %s", sent)
	}
	if strings.Contains(stdout.String(), "TOKENDROP") || stderr.Len() != 0 {
		t.Errorf("the machine form explained the variable to its reader: stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}
