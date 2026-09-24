package main

// One lineage file per Cursor conversation (#109).
//
// Keyed by workspace alone, two conversations open on one project shared one
// file: each hook event overwrote its session, and both advanced one counter.
// Since #118 a Cursor search carries the path its session declared on its own
// command, so the file can be the conversation's own. Everything here runs the
// real hooks and the real searchTrace over one in-memory machine; nothing
// between the two halves is written by hand.

import (
	"bytes"
	"encoding/json"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// cursorSession is one conversation's later hooks: the probe's machine, with
// the environment its sessionStart exported.
func cursorSession(p *lineageProbe, env map[string]string) hookOps {
	ops := p.ops.hook
	ops.getenv = func(k string) string { return env[k] }
	return ops
}

func cursorSessionStart(t *testing.T, p *lineageProbe, hc hookContext, conversation, workspace string) map[string]string {
	t.Helper()
	var out bytes.Buffer
	hookCursor(p.ops.hook, hc, "sessionStart", mustJSON(t, map[string]any{"conversation_id": conversation, "workspace_roots": []string{workspace}, "cursor_version": "3.21.16"}), &out, io.Discard)
	var answer struct {
		Env map[string]string `json:"env"`
	}
	if err := json.Unmarshal(out.Bytes(), &answer); err != nil {
		t.Fatalf("sessionStart answered %q", out.String())
	}
	return answer.Env
}

func cursorEvent(t *testing.T, ops hookOps, hc hookContext, event, conversation, workspace string, extra map[string]any) string {
	t.Helper()
	payload := map[string]any{"conversation_id": conversation, "generation_id": conversation + "-gen", "workspace_roots": []string{workspace}, "cursor_version": "3.21.16"}
	for k, v := range extra {
		payload[k] = v
	}
	var out bytes.Buffer
	hookCursor(ops, hc, event, mustJSON(t, payload), &out, io.Discard)
	return out.String()
}

// TestTwoCursorConversationsOnOneWorkspaceKeepTwoFiles is #109's case: two
// chat tabs on one project, their hook events interleaved, each search under
// its own session with its own text and its own counter.
func TestTwoCursorConversationsOnOneWorkspaceKeepTwoFiles(t *testing.T) {
	p := newLineageProbe(t, adoptRoot, nil)
	hc := hookContext{sessionsDir: adoptSessions}
	envA := cursorSessionStart(t, p, hc, "conversation-a", adoptRoot)
	envB := cursorSessionStart(t, p, hc, "conversation-b", adoptRoot)
	if envA[lineageEnv] == "" || envA[lineageEnv] == envB[lineageEnv] {
		t.Fatalf("two conversations on one workspace were given one file: %q and %q", envA[lineageEnv], envB[lineageEnv])
	}
	if envA[lineageEnv] != conversationLineagePath(adoptSessions, adoptRoot, "conversation-a") {
		t.Fatalf("sessionStart declared %q, not the conversation's own file", envA[lineageEnv])
	}
	if strings.Contains(envA[lineageEnv], "conversation-a") || strings.Contains(envA[lineageEnv], "project") {
		t.Errorf("the file name reveals the conversation or the project: %q", envA[lineageEnv])
	}

	opsA, opsB := cursorSession(p, envA), cursorSession(p, envB)
	search := cursorTestSearch(t, "")
	// Interleaved exactly as two tabs would be: every event of one lands
	// between the other's.
	cursorEvent(t, opsA, hc, "afterAgentResponse", "conversation-a", adoptRoot, map[string]any{"text": "A: let me look that up."})
	cursorEvent(t, opsB, hc, "afterAgentResponse", "conversation-b", adoptRoot, map[string]any{"text": "B: checking the docs."})
	if out := cursorEvent(t, opsA, hc, "beforeShellExecution", "conversation-a", adoptRoot, map[string]any{"command": search}); strings.TrimSpace(out) != `{"permission":"allow"}` {
		t.Fatalf("A's search was not allowed: %q", out)
	}
	if out := cursorEvent(t, opsB, hc, "beforeShellExecution", "conversation-b", adoptRoot, map[string]any{"command": search}); strings.TrimSpace(out) != `{"permission":"allow"}` {
		t.Fatalf("B's search was not allowed: %q", out)
	}

	trA, trB := p.trace(envA), p.trace(envB)
	for _, c := range []struct {
		name string
		tr   *traceEnvelope
		conv string
		text string
	}{{"A", trA, "conversation-a", "A: let me look that up."}, {"B", trB, "conversation-b", "B: checking the docs."}} {
		if c.tr == nil || c.tr.Harness != "cursor" || c.tr.SessionID != traceHash(c.conv) {
			t.Fatalf("%s's search went out as %+v, want its own session", c.name, c.tr)
		}
		// Its own counter: one stamp by beforeShellExecution, one by the
		// search. On a shared file the two conversations' four steps would
		// leave 3 and 4.
		if c.tr.TurnID != traceHash(c.conv+"|"+c.conv+"-gen") || c.tr.Seq != 2 {
			t.Errorf("%s's search carried turn %q seq %d, want its own turn at seq 2", c.name, c.tr.TurnID, c.tr.Seq)
		}
		if len(c.tr.History) != 1 || c.tr.History[0].Text != c.text {
			t.Errorf("%s's search carried %+v, want its own text", c.name, c.tr.History)
		}
	}
	if trA.CallID == trB.CallID {
		t.Error("two conversations' searches carried one call id")
	}
	if _, shared := p.fs.files[lineagePath(adoptSessions, adoptRoot)]; shared {
		t.Error("a workspace-keyed file was written beside the conversations' own")
	}
}

// TestACursorConversationStartedOn0212KeepsItsWorkspaceFile is the upgrade
// case. A conversation whose sessionStart ran on 0.2.12 has the
// workspace-keyed file declared in its session environment; after the binary
// is replaced its later hooks run 0.2.13. They must keep writing where the
// session declared, preToolUse must carry that same path, and the search must
// adopt it — otherwise the search reads a file nobody stamps, and the hooks
// stamp a file nobody reads.
func TestACursorConversationStartedOn0212KeepsItsWorkspaceFile(t *testing.T) {
	p := newLineageProbe(t, adoptRoot, nil)
	hc := hookContext{sessionsDir: adoptSessions}
	// What 0.2.12's sessionStart did: seed the workspace file and export it.
	declared := lineagePath(adoptSessions, adoptRoot)
	if err := updateLineage(p.ops.hook, declared, p.now, func(l *lineageFile) {
		l.Harness, l.SessionID, l.Window = "cursor", traceHash("conversation-old"), "none"
	}); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"TOKENDROP_HARNESS": "cursor", lineageEnv: declared, sessionEnv: traceHash("conversation-old")}
	ops := cursorSession(p, env)

	cursorEvent(t, ops, hc, "afterAgentThought", "conversation-old", adoptRoot, map[string]any{"text": "thinking before the search"})
	search := cursorTestSearch(t, "")
	out := cursorEvent(t, ops, hc, "preToolUse", "conversation-old", adoptRoot, map[string]any{"tool_name": "Shell", "tool_input": map[string]any{"command": search}})
	if !strings.Contains(out, filepath.Base(declared)) {
		t.Fatalf("preToolUse did not carry the declared workspace file: %q", out)
	}
	if out := cursorEvent(t, ops, hc, "beforeShellExecution", "conversation-old", adoptRoot, map[string]any{"command": search}); strings.TrimSpace(out) != `{"permission":"allow"}` {
		t.Fatalf("the search was not allowed: %q", out)
	}
	if _, wrote := p.fs.files[conversationLineagePath(adoptSessions, adoptRoot, "conversation-old")]; wrote {
		t.Fatal("0.2.13's hooks wrote a conversation file the running session never declared")
	}
	tr := p.trace(env)
	if tr == nil || tr.SessionID != traceHash("conversation-old") || tr.Seq != 2 || tr.CallID == "" ||
		len(tr.History) != 1 || tr.History[0].Text != "thinking before the search" {
		t.Fatalf("the upgraded conversation's search went out as %+v", tr)
	}
}

// TestACursorEventDoesNotAdoptAnotherConversationsDeclaredFile: a declared
// path is used only when the session exported beside it is this payload's
// conversation. An environment that names another conversation's file —
// inherited, or stale — is not written through.
func TestACursorEventDoesNotAdoptAnotherConversationsDeclaredFile(t *testing.T) {
	p := newLineageProbe(t, adoptRoot, nil)
	hc := hookContext{sessionsDir: adoptSessions}
	envA := cursorSessionStart(t, p, hc, "conversation-a", adoptRoot)
	cursorSessionStart(t, p, hc, "conversation-b", adoptRoot)

	cursorEvent(t, cursorSession(p, envA), hc, "afterAgentResponse", "conversation-b", adoptRoot, map[string]any{"text": "B's text"})
	if a, _ := loadLineage(p.ops.hook, envA[lineageEnv]); a == nil || a.SessionID != traceHash("conversation-a") || len(a.History) != 0 {
		t.Fatalf("B's event wrote into A's declared file: %+v", a)
	}
	if b, _ := loadLineage(p.ops.hook, conversationLineagePath(adoptSessions, adoptRoot, "conversation-b")); b == nil || len(b.History) != 1 || b.History[0].Text != "B's text" {
		t.Fatalf("B's event did not reach B's own file: %+v", b)
	}

	outside := map[string]string{"TOKENDROP_HARNESS": "cursor", lineageEnv: "/elsewhere/" + filepath.Base(envA[lineageEnv]), sessionEnv: traceHash("conversation-a")}
	cursorEvent(t, cursorSession(p, outside), hc, "afterAgentResponse", "conversation-a", adoptRoot, map[string]any{"text": "A's text"})
	if _, wrote := p.fs.files[outside[lineageEnv]]; wrote {
		t.Fatal("a declared path outside the sessions directory was written")
	}
	if a, _ := loadLineage(p.ops.hook, envA[lineageEnv]); a == nil || len(a.History) != 1 || a.History[0].Text != "A's text" {
		t.Fatalf("A's event did not fall back to A's own file: %+v", a)
	}
}

// TestConversationFilesFollowTheSweepAndTheAgeRule: the new names have
// lineagePath's shape, so a stale temporary file of one conversation's file is
// swept by a write to another's, a fresh one and this process's own are
// left, and a file past lineageMaxAge is not adopted.
func TestConversationFilesFollowTheSweepAndTheAgeRule(t *testing.T) {
	fs, ops := newFakeHookOps(nil)
	now := ops.now()
	a := conversationLineagePath("/sessions", "/w/proj", "conversation-a")
	b := conversationLineagePath("/sessions", "/w/proj", "conversation-b")
	if !lineageNameRe.MatchString(filepath.Base(a)) {
		t.Fatalf("a conversation file's name %q is not a lineage file's shape; the sweep would never take its leftovers", filepath.Base(a))
	}
	stale, fresh, own := a+".7.tmp", a+".8.tmp", a+".42.tmp"
	for _, tmp := range []string{stale, fresh, own} {
		fs.files[tmp] = []byte("{}")
	}
	fs.mtimes[stale] = now.Add(-lineageMaxAge - time.Minute)
	fs.mtimes[fresh] = now.Add(-time.Minute)
	fs.mtimes[own] = now.Add(-lineageMaxAge - time.Minute)

	if err := updateLineage(ops, b, now, func(l *lineageFile) { l.Harness, l.SessionID = "cursor", traceHash("conversation-b") }); err != nil {
		t.Fatal(err)
	}
	if _, left := fs.files[stale]; left {
		t.Error("a stale temporary file of another conversation's lineage file was not swept")
	}
	for _, kept := range []string{fresh, own} {
		if _, ok := fs.files[kept]; !ok {
			t.Errorf("%s was swept; only another pid's file older than lineageMaxAge may be", filepath.Base(kept))
		}
	}

	p := newLineageProbe(t, adoptRoot, nil)
	hc := hookContext{sessionsDir: adoptSessions}
	env := cursorSessionStart(t, p, hc, "conversation-old", adoptRoot)
	p.ops.now = func() time.Time { return p.now.Add(lineageMaxAge + time.Second) }
	if tr := p.trace(env); tr == nil || tr.SessionID == traceHash("conversation-old") && tr.Window == "none" {
		t.Fatalf("a conversation file past lineageMaxAge was adopted: %+v", tr)
	}
}
