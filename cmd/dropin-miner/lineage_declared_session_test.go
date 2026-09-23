package main

// The declared lineage file is adopted only when it holds this session (#109).
//
// Cursor keyed the lineage file by workspace alone until #109's fix, and every
// hook event wrote the current conversation's id into it. Two conversations
// open on one workspace shared one file, and both shells named it in
// TOKENDROP_LINEAGE. No nesting and no lost variable: two chat tabs on a
// project. Cursor's files are now per conversation (cursor_conversation_test.go);
// this guard stays for any file a session did not name for itself — the
// workspace file a conversation started on 0.2.12 still declares, above all.
//
// Asserted on the bytes the router receives. The search runs from inside the
// workspace, as it really does, so the fall-through meets the same file again
// on the walk and has to refuse it there too.

import (
	"bytes"
	"testing"
)

// otherConversationOwnsTheFile: the probe's workspace file, last written by a
// conversation that is not the one searching.
func otherConversationOwnsTheFile(t *testing.T) (*channelProbe, map[string]string) {
	t.Helper()
	p := newChannelProbe(t) // the file holds session "cursor-conv", seq 3, text "Let me check."
	return p, map[string]string{"TOKENDROP_API_KEY": "k", "TOKENDROP_HARNESS": "cursor", lineageEnv: p.lineage}
}

func TestTheDeclaredFileOfAnotherConversationIsNotAdopted(t *testing.T) {
	p, env := otherConversationOwnsTheFile(t)
	env[sessionEnv] = "this-conversation"

	if code, _, stderr := runSearch(t, p.h, env, "-config", p.cfg, "q"); code != exitOK {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	tr, sent := p.sentTrace(t)
	if bytes.Contains(sent, []byte("cursor-conv")) {
		t.Errorf("the search reached the router under the other conversation's session: %s", sent)
	}
	if bytes.Contains(sent, []byte("cursor-gen")) || bytes.Contains(sent, []byte("cursor-call")) {
		t.Errorf("the other conversation's turn or call id traveled: %s", sent)
	}
	if bytes.Contains(sent, []byte("Let me check.")) || tr["history"] != nil {
		t.Errorf("the other conversation's text left the machine with this search: %s", sent)
	}
	if tr["harness"] != "cursor" {
		t.Errorf("harness %v; the host still declared cursor", tr["harness"])
	}
	if id, _ := tr["session_id"].(string); id == "" || id == "this-conversation" {
		t.Errorf("session_id %q, want the honest per-shell identity: usable, and not a value read out of the environment", id)
	}
	if l, ok := loadLineage(p.h.ops.hook, p.lineage); !ok || l.Seq != 3 || l.SessionID != "cursor-conv" {
		t.Errorf("the other conversation's file was advanced or rewritten: %+v", l)
	}
}

// The control: the same file, held by the conversation that is searching, is
// adopted exactly as before — identity, text and an advanced seq.
func TestTheDeclaredFileOfThisConversationIsAdopted(t *testing.T) {
	p, env := otherConversationOwnsTheFile(t)
	env[sessionEnv] = "cursor-conv"

	runSearch(t, p.h, env, "-config", p.cfg, "q")
	tr, sent := p.sentTrace(t)
	if tr["session_id"] != "cursor-conv" || tr["seq"] != float64(4) || !bytes.Contains(sent, []byte("Let me check.")) {
		t.Errorf("a session's own declared file was not adopted: %s", sent)
	}
	if l, _ := loadLineage(p.h.ops.hook, p.lineage); l.Seq != 4 {
		t.Errorf("seq on disk %d, want 4", l.Seq)
	}
}

// With no session exported the declared file is believed by its path, as it
// was: every shell started before the variable existed. Asserted, so that it
// is a decision. It is also #109's defect, still open for those shells, and
// this test says so rather than hiding it.
func TestWithoutTheSessionVariableTheDeclaredFileIsBelievedAsBefore(t *testing.T) {
	p, env := otherConversationOwnsTheFile(t)

	runSearch(t, p.h, env, "-config", p.cfg, "q")
	if tr, _ := p.sentTrace(t); tr["session_id"] != "cursor-conv" || tr["seq"] != float64(4) {
		t.Errorf("the declared file was not adopted with no session exported: %v", tr)
	}
}

// K2's rule is untouched by the guard: a foreign bridge beside a declared
// file of another conversation is still dropped, and still not a fallback.
func TestAForeignBridgeIsNotAFallbackWhenTheDeclaredFileIsAnotherConversations(t *testing.T) {
	p, env := otherConversationOwnsTheFile(t)
	env[sessionEnv] = "this-conversation"
	env[bridgeEnv] = someoneElsesBridge(t, "claude-code")

	runSearch(t, p.h, env, "-config", p.cfg, "q")
	tr, sent := p.sentTrace(t)
	if tr["harness"] != "cursor" || bytes.Contains(sent, []byte("foreign")) || bytes.Contains(sent, []byte("cursor-conv")) {
		t.Errorf("got %s", sent)
	}
}
