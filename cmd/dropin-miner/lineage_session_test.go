package main

// Two sessions of one host are told apart (#104).
//
// #97 made the harness the test of ownership, which closed the cross-host
// case. Two sessions of the SAME host in nested workspaces — a monorepo open
// at its root and again at a package — are one name, and the walk took the
// nearest file whichever session was searching. The session-start hook now
// exports the hashed session id it already writes into the lineage file, and
// the walk requires it when it is there.
//
// The walk is only reached when TOKENDROP_LINEAGE is absent, so every search
// here arrives without it: that is the case being fixed.

import (
	"bytes"
	"encoding/json"
	"io"
	"path/filepath"
	"testing"
	"time"
)

var (
	nestedInner = filepath.Join(adoptRoot, "pkg")
	nestedCwd   = filepath.Join(adoptRoot, "pkg", "src")
)

func walkEnv(session string) map[string]string {
	env := map[string]string{"TOKENDROP_HARNESS": "cursor"}
	if session != "" {
		env[sessionEnv] = session
	}
	return env
}

// The outer session's agent changed into the inner workspace and searched.
// Its own file is found, its own seq advances, and the inner session's file
// is neither adopted nor touched.
func TestTheWalkFindsTheSearchingSessionsOwnFilePastAnotherSessionOfTheSameHost(t *testing.T) {
	p := newLineageProbe(t, nestedCwd, nil)
	outer := p.write(t, adoptRoot, "cursor", "outer-session")
	inner := p.write(t, nestedInner, "cursor", "inner-session")

	env := p.trace(walkEnv("outer-session"))

	if env == nil || env.SessionID != "outer-session" {
		t.Fatalf("the outer session's search went out as %+v, want its own session", env)
	}
	if env.Seq != 8 || p.seqOf(t, outer) != 8 {
		t.Errorf("the searching session's seq: envelope %d, disk %d, want 8 and 8", env.Seq, p.seqOf(t, outer))
	}
	if got := p.seqOf(t, inner); got != 7 {
		t.Errorf("the inner session's seq moved to %d; it was not its search", got)
	}
}

// The same search when the outer session has no file to find: none is
// adopted. Never the inner one — its id and its text stay where they are.
func TestTheWalkAdoptsNothingRatherThanAnotherSessionOfTheSameHost(t *testing.T) {
	p := newLineageProbe(t, nestedCwd, nil)
	inner := p.write(t, nestedInner, "cursor", "inner-session")

	env := p.trace(walkEnv("outer-session"))

	if env == nil {
		t.Fatal("a search that can adopt nothing must still send a usable trace")
	}
	if env.SessionID == "inner-session" || len(env.History) != 0 {
		t.Errorf("the search carried another session's identity or text: %+v", env)
	}
	if env.Harness != "cursor" {
		t.Errorf("harness %q, want cursor", env.Harness)
	}
	if got := p.seqOf(t, inner); got != 7 {
		t.Errorf("the inner session's seq moved to %d", got)
	}
}

// The inner session's own search from the same directory is unaffected: the
// nearest file is its own, and it is the one adopted.
func TestTheInnerSessionsOwnSearchStillFindsItsFile(t *testing.T) {
	p := newLineageProbe(t, nestedCwd, nil)
	outer := p.write(t, adoptRoot, "cursor", "outer-session")
	inner := p.write(t, nestedInner, "cursor", "inner-session")

	env := p.trace(walkEnv("inner-session"))

	if env == nil || env.SessionID != "inner-session" || p.seqOf(t, inner) != 8 || p.seqOf(t, outer) != 7 {
		t.Fatalf("envelope %+v, inner seq %d, outer seq %d", env, p.seqOf(t, inner), p.seqOf(t, outer))
	}
}

// THE FALLBACK, asserted so that it is a decision and not an accident: with
// no session exported the rule is exactly #97's. The nearest file of the
// same harness is adopted — which, in this layout, is the defect #104
// describes, and is what every shell started before the variable existed
// still gets. A test that encodes a deferral says so: this one does.
func TestWithoutTheSessionVariableTheWalkIsExactlyTheHarnessRule(t *testing.T) {
	p := newLineageProbe(t, nestedCwd, nil)
	outer := p.write(t, adoptRoot, "cursor", "outer-session")
	inner := p.write(t, nestedInner, "cursor", "inner-session")

	env := p.trace(walkEnv(""))

	if env == nil || env.SessionID != "inner-session" {
		t.Fatalf("with no session exported the nearest same-harness file is the answer, got %+v", env)
	}
	if p.seqOf(t, inner) != 8 || p.seqOf(t, outer) != 7 {
		t.Errorf("inner seq %d, outer seq %d, want 8 and 7", p.seqOf(t, inner), p.seqOf(t, outer))
	}
}

// A nearer file of ANOTHER host is climbed past when the session is known,
// and stops the walk when it is not (#97's rule, held by
// TestAForeignSidecarStopsTheWalkRatherThanBeingClimbedPast). The two do not
// disagree: what made a more distant file a guess was that only a name
// matched, and a file holding this session's own id is not a guess.
func TestAKnownSessionClimbsPastAnotherHostsFileToItsOwn(t *testing.T) {
	p := newLineageProbe(t, nestedCwd, nil)
	mine := p.write(t, adoptRoot, "cursor", "outer-session")
	theirs := p.write(t, nestedInner, "claude-code", "claude-session")

	env := p.trace(walkEnv("outer-session"))

	if env == nil || env.SessionID != "outer-session" || env.Harness != "cursor" {
		t.Fatalf("got %+v", env)
	}
	if p.seqOf(t, mine) != 8 || p.seqOf(t, theirs) != 7 {
		t.Errorf("mine %d, theirs %d, want 8 and 7", p.seqOf(t, mine), p.seqOf(t, theirs))
	}
}

// Knowing the session does not make a stale file believable, and does not
// let a file of another harness through because its session id agrees.
func TestAKnownSessionStillRequiresFreshnessAndTheHarness(t *testing.T) {
	t.Run("stale", func(t *testing.T) {
		p := newLineageProbe(t, nestedCwd, nil)
		p.write(t, adoptRoot, "cursor", "outer-session")
		p.ops.now = func() time.Time { return p.now.Add(lineageMaxAge + time.Second) }
		if env := p.trace(walkEnv("outer-session")); env == nil || env.SessionID == "outer-session" {
			t.Fatalf("a stale file was adopted: %+v", env)
		}
	})
	t.Run("another harness under the same id", func(t *testing.T) {
		p := newLineageProbe(t, nestedCwd, nil)
		path := p.write(t, adoptRoot, "claude-code", "outer-session")
		env := p.trace(walkEnv("outer-session"))
		if env == nil || (env.SessionID == "outer-session" && env.Seq == 8) || p.seqOf(t, path) != 7 {
			t.Fatalf("a file of another harness was adopted: %+v", env)
		}
	})
}

// End to end, with nothing hand-written between the two halves: Cursor's
// session-start hook is run for two conversations in nested workspaces, and
// the outer one's search runs with exactly the environment that hook
// exported — which since #118 is what its preToolUse hook puts on the
// search's own command.
//
// Before #109 this test dropped the lineage variable and required the walk to
// find the outer session's file. It cannot any more, and that is chosen: the
// file is keyed by conversation, the walk by directory, and a Cursor search
// carries its declared path on its own command, so no shell of its own
// loses it. What a Cursor search that has no declared path gets is asserted
// below: the honest per-shell identity under its harness, and nobody's file.
func TestTheSessionTheHookExportsIsTheOneItsDeclaredFileHolds(t *testing.T) {
	p := newLineageProbe(t, nestedCwd, nil)
	hc := hookContext{sessionsDir: adoptSessions}
	start := func(conversation, workspace string) map[string]string {
		t.Helper()
		var out bytes.Buffer
		hookCursor(p.ops.hook, hc, "sessionStart", mustJSON(t, map[string]any{"conversation_id": conversation, "workspace_roots": []string{workspace}}), &out, io.Discard)
		var answer struct {
			Env map[string]string `json:"env"`
		}
		if err := json.Unmarshal(out.Bytes(), &answer); err != nil {
			t.Fatalf("sessionStart answered %q", out.String())
		}
		return answer.Env
	}
	outerEnv := start("conversation-outer", adoptRoot)
	innerEnv := start("conversation-inner", nestedInner)

	outerFile, ok := loadLineage(p.ops.hook, outerEnv[lineageEnv])
	if !ok {
		t.Fatal("the hook wrote no lineage file for the outer session")
	}
	if outerEnv[sessionEnv] == "" || outerEnv[sessionEnv] != outerFile.SessionID {
		t.Fatalf("exported session %q, the lineage file holds %q: the declared file would be refused", outerEnv[sessionEnv], outerFile.SessionID)
	}
	if outerEnv[sessionEnv] == innerEnv[sessionEnv] {
		t.Fatal("two conversations exported one session id")
	}
	if outerEnv[sessionEnv] == "conversation-outer" {
		t.Error("the raw conversation id was exported; every identifier is hashed")
	}

	env := p.trace(outerEnv)
	if env == nil || env.SessionID != outerFile.SessionID || env.Seq != 1 {
		t.Fatalf("the outer session's search went out as %+v, want session %q at seq 1", env, outerFile.SessionID)
	}
	if inner, _ := loadLineage(p.ops.hook, innerEnv[lineageEnv]); inner == nil || inner.Seq != 0 {
		t.Errorf("the inner session's file was advanced: %+v", inner)
	}

	delete(outerEnv, lineageEnv)
	lost := p.trace(outerEnv)
	if lost == nil || lost.Harness != "cursor" || lost.SessionID == outerFile.SessionID || lost.SessionID == innerEnv[sessionEnv] {
		t.Fatalf("with no declared path the search went out as %+v; want the per-shell identity under cursor", lost)
	}
	if l, _ := loadLineage(p.ops.hook, conversationLineagePath(adoptSessions, adoptRoot, "conversation-outer")); l == nil || l.Seq != 1 {
		t.Fatalf("the outer session's file was advanced by a search that did not declare it: %+v", l)
	}
}

// The session is exported even when no lineage path could be computed — one
// of the two ways a search ends up on the walk — and never for a payload
// that names no conversation.
func TestSessionStartExportsTheSessionWithoutAWorkspaceAndNotWithoutAConversation(t *testing.T) {
	_, ops := newFakeHookOps(nil)
	hc := hookContext{sessionsDir: adoptSessions}
	answer := func(payload map[string]any) map[string]string {
		var out bytes.Buffer
		hookCursor(ops, hc, "sessionStart", mustJSON(t, payload), &out, io.Discard)
		var a struct {
			Env map[string]string `json:"env"`
		}
		_ = json.Unmarshal(out.Bytes(), &a)
		return a.Env
	}
	noWorkspace := answer(map[string]any{"conversation_id": "c"})
	if _, has := noWorkspace[lineageEnv]; has {
		t.Fatalf("a lineage path with no workspace: %v", noWorkspace)
	}
	if noWorkspace[sessionEnv] != traceHash("c") {
		t.Errorf("session %q, want the hash the lineage file would hold", noWorkspace[sessionEnv])
	}
	if env := answer(map[string]any{"workspace_roots": []string{adoptRoot}}); env[sessionEnv] != "" {
		t.Errorf("a session exported for no conversation: %v", env)
	}
}
