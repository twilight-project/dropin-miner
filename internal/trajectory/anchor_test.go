package trajectory

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestTraceHashMatchesAnIndependentVector pins the derivation to a value
// computed outside Go: printf 'tokendrop-trace-v1|abc' | shasum -a 256, first
// sixteen bytes.
func TestTraceHashMatchesAnIndependentVector(t *testing.T) {
	if got, want := TraceHash("abc"), "df0a99234528599c3085e469ec663800"; got != want {
		t.Fatalf("TraceHash(abc) = %s, want %s", got, want)
	}
	if got := TraceHash(""); got != "" {
		t.Fatalf("TraceHash of nothing = %q, want nothing: the client hashes no empty id", got)
	}
}

// TestTraceHashRestatesTheClientsDerivation reads the client's own source.
// The client is package main and cannot be imported, so this package repeats
// its derivation; if the original changes, path 2 silently joins nothing, and
// this is the test that says so instead.
func TestTraceHashRestatesTheClientsDerivation(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "..", "cmd", "dropin-miner", "trace.go"))
	if err != nil {
		t.Fatalf("the client's trace.go is not where this test expects it: %v", err)
	}
	for _, want := range []string{
		`sha256.Sum256([]byte("` + tracePrefix + `" + raw))`,
		`hex.EncodeToString(sum[:16])`,
	} {
		if !bytes.Contains(src, []byte(want)) {
			t.Errorf("cmd/dropin-miner/trace.go no longer contains %q: TraceHash must be brought back in line with it", want)
		}
	}
	hook, err := os.ReadFile(filepath.Join("..", "..", "cmd", "dropin-miner", "hook.go"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`SessionID: traceHash(p.SessionID),`,
		`env.TurnID = traceHash(p.SessionID + "|" + p.PromptID)`,
		`env.CallID = traceHash(p.SessionID + "|" + p.ToolUseID)`,
		`env.SessionID = traceHash(p.SessionID + "|" + p.AgentID)`,
	} {
		if !bytes.Contains(hook, []byte(want)) {
			t.Errorf("cmd/dropin-miner/hook.go no longer contains %q: TraceIDsFor must be brought back in line with it", want)
		}
	}
}

func TestTraceIDsForFollowsTheHooksComposition(t *testing.T) {
	main := TraceIDsFor("sess", "", "prompt", "toolu")
	if main.Session != TraceHash("sess") || main.Turn != TraceHash("sess|prompt") || main.Call != TraceHash("sess|toolu") {
		t.Fatalf("main-chain ids = %+v", main)
	}
	sub := TraceIDsFor("sess", "agent", "prompt", "toolu")
	if sub.Session != TraceHash("sess|agent") {
		t.Errorf("a subagent threads as its own lane: Session = %s, want the hash of sess|agent", sub.Session)
	}
	if sub.Call != main.Call {
		t.Errorf("the hook hashes a call under the session, not the lane: %s != %s", sub.Call, main.Call)
	}
	if got := TraceIDsFor("sess", "", "", ""); got.Turn != "" || got.Call != "" {
		t.Errorf("absent raw ids must yield absent hashes, got %+v", got)
	}
}

// writeLineage writes a lineage file the way the client does, history
// included, so the test can show the history is never picked up.
func writeLineage(t *testing.T, dir, name string, v int, ids TraceIDs) {
	t.Helper()
	doc := map[string]any{
		"v": v, "harness": "claude-code", "session_id": ids.Session, "turn_id": ids.Turn, "call_id": ids.Call,
		"seq": 3, "updated_at": "2026-01-02T00:00:05Z",
		"history": []map[string]string{{"role": "assistant", "text": contentMarker + "LINEAGE-HISTORY"}},
	}
	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func fixtureLineage(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	// The latest call of the two-search session, and the subagent's lane.
	writeLineage(t, dir, "ws-one.json", 1, TraceIDsFor(sessTwoSearches, "", "prompt-b1", "toolu_search_b2"))
	writeLineage(t, dir, "ws-two.json", 1, TraceIDsFor(sessSubagent, "c0ffee01", "", ""))
	// Not lineage files: the hook's window state, a future version, debris.
	writeLineage(t, dir, "future.json", 2, TraceIDsFor(sessPlain, "", "prompt-a1", ""))
	if err := os.WriteFile(filepath.Join(dir, "window.json"), []byte(`{"v":1,"windows":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ws-one.json.123.tmp"), []byte(`{`), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestLoadLineageIndexesIdsOnlyAndSkipsWhatIsNotLineage(t *testing.T) {
	idx, err := LoadLineage(fixtureLineage(t))
	if err != nil {
		t.Fatal(err)
	}
	if idx.Files != 2 || idx.Skipped != 2 {
		t.Fatalf("Files = %d, Skipped = %d; want 2 and 2 (a future version and window.json; the .tmp is not .json)", idx.Files, idx.Skipped)
	}
	if !idx.Sessions[TraceHash(sessTwoSearches)] || idx.Sessions[TraceHash(sessPlain)] {
		t.Errorf("Sessions = %v: version 2 is not a shape this reads and must not be indexed", idx.Sessions)
	}
	m := idx.Match(TraceIDsFor(sessTwoSearches, "", "prompt-b1", "toolu_search_b1"))
	if !m.Session || !m.Turn || m.Call {
		t.Errorf("first search match = %+v, want session and turn but not call: lineage keeps only the latest call", m)
	}
	if m := (*LineageIndex)(nil).Match(TraceIDs{Session: "x"}); m.Session {
		t.Error("a nil index matched")
	}
}

func TestCountsOverTheFixtures(t *testing.T) {
	idx, err := LoadLineage(fixtureLineage(t))
	if err != nil {
		t.Fatal(err)
	}
	c := NewCounts(idx)
	if err := Walk(fixtureDir, Options{}, func(s *Session) { c.Add(s, idx) }); err != nil {
		t.Fatal(err)
	}
	check := func(name string, got, want int) {
		t.Helper()
		if got != want {
			t.Errorf("%s = %d, want %d", name, got, want)
		}
	}
	check("Transcripts", c.Transcripts, 7)
	check("SubagentRuns", c.SubagentRuns, 2)
	check("SubagentRunsLinked", c.SubagentRunsLinked, 1)
	check("HostVersions", len(c.HostVersions), 5)
	check("Turns", c.Turns, 11)
	check("TurnsByEnd[yielded]", c.TurnsByEnd[EndYielded], 7)
	check("TurnsByEnd[yielded_unmarked]", c.TurnsByEnd[EndYieldedUnmarked], 2)
	check("TurnsByEnd[interrupted]", c.TurnsByEnd[EndInterrupted], 1)
	check("TurnsByEnd[open_at_eof]", c.TurnsByEnd[EndOpenAtEOF], 1)
	check("TurnsByStart[participant]", c.TurnsByStart[OriginParticipant], 9)
	check("TurnsByStart[host_model]", c.TurnsByStart[OriginHostModel], 2)
	check("TurnsCompacted", c.TurnsCompacted, 1)
	check("TurnsWithRefusal", c.TurnsWithRefusal, 2)
	check("TurnsWithSearch", c.TurnsWithSearch, 3)
	check("Searches", c.Searches, 4)
	check("SearchesMain", c.SearchesMain, 3)
	check("SearchesSubagent", c.SearchesSubagent, 1)
	check("AnchoredPath1", c.AnchoredPath1, 2)
	check("DistinctRequestIDs", len(c.DistinctRequestIDs), 2)
	check("LossByReason[human_format]", c.LossByReason[LossHumanFormat], 1)
	check("LossByReason[no_result]", c.LossByReason[LossNoResult], 1)
	// Path 2: both searches of the two-search session by session and turn,
	// only its latest by call; the subagent's by its lane alone.
	check("Path2Session", c.Path2Session, 3)
	check("Path2Turn", c.Path2Turn, 2)
	check("Path2Call", c.Path2Call, 1)
	check("SearchTurnsPath2Session", c.SearchTurnsPath2Session, 2)
	check("SearchTurnsPath2Turn", c.SearchTurnsPath2Turn, 1)
	// The interrupted search is the one neither path reaches.
	check("AnchoredEither", c.AnchoredEither, 3)
	check("SessionsInLineage", c.SessionsInLineage, 1)
	check("RefusalsByReason[unknown_entry_type]", c.RefusalsByReason[RefuseUnknownEntryType], 1)
	check("RefusalsByReason[truncated_final_line]", c.RefusalsByReason[RefuseTruncatedFinalLine], 1)
	check("RefusalsByReason[subagent_unlinked]", c.RefusalsByReason[RefuseSubagentUnlinked], 1)
	if c.RefusalsByShape["unknown_entry_type quantum-state"] != 1 || c.RefusalsByShape["unknown_block_type assistant/hologram"] != 1 {
		t.Errorf("RefusalsByShape = %v: a refusal must name the shape it declined", c.RefusalsByShape)
	}
	if c.SubstringHits <= c.Searches {
		t.Errorf("SubstringHits = %d against %d searches: the fixture's overcount disappeared", c.SubstringHits, c.Searches)
	}

	var out strings.Builder
	if _, err := c.WriteTo(&out); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), contentMarker) {
		t.Fatalf("the counts printed content:\n%s", out.String())
	}
	for _, id := range []string{"req_synthetic_0001", sessTwoSearches, "toolu_search_b1", "/synthetic/workspace"} {
		if strings.Contains(out.String(), id) {
			t.Errorf("the counts printed %q; they hold numbers, not ids or paths", id)
		}
	}
}

func TestATurnCopiedIntoAResumedSessionIsCountedOnce(t *testing.T) {
	dir := t.TempDir()
	data, err := os.ReadFile(filepath.Join(fixtureDir, "-synthetic-workspace", sessTwoSearches+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"first.jsonl", "resumed.jsonl"} {
		// #nosec G703 -- two fixed names under the test's own temporary directory
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	c := NewCounts(nil)
	if err := Walk(dir, Options{}, func(s *Session) { c.Add(s, nil) }); err != nil {
		t.Fatal(err)
	}
	if c.Turns != 1 || c.Searches != 2 || c.DuplicateTurns != 1 {
		t.Fatalf("Turns = %d, Searches = %d, DuplicateTurns = %d; want 1, 2, 1", c.Turns, c.Searches, c.DuplicateTurns)
	}
}
