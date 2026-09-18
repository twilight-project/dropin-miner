package trajectory

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The fixtures under testdata/projects are synthetic: written by hand to the
// shapes shapes.go declares, never copied from a real transcript. Every
// string a person or a model might have written carries the marker below, so
// a test can prove that none of it reaches an output.
const contentMarker = "ZEBRA-"

const fixtureDir = "testdata/projects"

const (
	sessPlain       = "00000000-0000-4000-8000-00000000000a"
	sessTwoSearches = "00000000-0000-4000-8000-00000000000b"
	sessSubagent    = "00000000-0000-4000-8000-00000000000c"
	sessInterrupted = "00000000-0000-4000-8000-00000000000d"
	sessCompaction  = "00000000-0000-4000-8000-00000000000e"
	sessUnknown     = "00000000-0000-4000-8000-00000000000f"
	sessTruncated   = "00000000-0000-4000-8000-000000000010"
)

func readFixtures(t *testing.T, opts Options) map[string]*Session {
	t.Helper()
	sessions := map[string]*Session{}
	if err := Walk(fixtureDir, opts, func(s *Session) { sessions[s.SessionID] = s }); err != nil {
		t.Fatalf("Walk: %v", err)
	}
	return sessions
}

func session(t *testing.T, id string) *Session {
	t.Helper()
	s := readFixtures(t, Options{})[id]
	if s == nil {
		t.Fatalf("fixture session %s was not read", id)
	}
	return s
}

func kinds(turn *Turn) []Kind {
	out := make([]Kind, len(turn.Events))
	for i, ev := range turn.Events {
		out[i] = ev.Kind
	}
	return out
}

func wantKinds(t *testing.T, turn *Turn, want ...Kind) {
	t.Helper()
	got := kinds(turn)
	if len(got) != len(want) {
		t.Fatalf("turn %d events = %v, want %v", turn.Index, got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("turn %d events = %v, want %v", turn.Index, got, want)
		}
	}
}

func refusalsOf(s *Session, reason RefusalReason) []Refusal {
	var out []Refusal
	for _, r := range s.Refusals {
		if r.Reason == reason {
			out = append(out, r)
		}
	}
	return out
}

func TestPlainTurnRunsFromThePromptToTheHostsYieldMarker(t *testing.T) {
	s := session(t, sessPlain)
	if len(s.Turns) != 1 {
		t.Fatalf("turns = %d, want 1", len(s.Turns))
	}
	turn := s.Turns[0]
	wantKinds(t, turn, KindPrompt, KindThinking, KindAssistantText)
	if turn.End != EndYielded {
		t.Errorf("End = %q, want %q: the host wrote its turn_duration marker", turn.End, EndYielded)
	}
	if turn.StartOrigin != OriginParticipant || turn.Events[0].Origin != OriginParticipant {
		t.Errorf("a typed prompt must be participant-origin, got %q", turn.StartOrigin)
	}
	if turn.Events[2].Origin != OriginHostModel {
		t.Errorf("assistant text origin = %q, want host_model", turn.Events[2].Origin)
	}
	if turn.PromptID != "prompt-a1" || turn.PromptIDs != 1 {
		t.Errorf("PromptID = %q (%d distinct), want prompt-a1 (1)", turn.PromptID, turn.PromptIDs)
	}
	if len(s.Refusals) != 0 {
		t.Errorf("a plain turn among declared bookkeeping entries refused %v", s.Refusals)
	}
	if s.Versions["2.1.276"] == 0 {
		t.Errorf("host version not recorded: %v", s.Versions)
	}
}

func TestSearchesAreCountedFromParsedCallsNotFromTheCommandsText(t *testing.T) {
	s := session(t, sessTwoSearches)
	if len(s.Turns) != 1 {
		t.Fatalf("turns = %d, want 1", len(s.Turns))
	}
	turn := s.Turns[0]
	// The fixture mentions the search command in the skill text, in the
	// model's prose, inside the first search's own query and in an echo and
	// a grep that are not searches. Two tool calls run the search.
	if len(turn.Searches) != 2 {
		t.Fatalf("searches = %d, want 2", len(turn.Searches))
	}
	if s.SubstringHits <= len(turn.Searches) {
		t.Fatalf("SubstringHits = %d: the fixture is meant to overcount well past %d", s.SubstringHits, len(turn.Searches))
	}
	first, second := turn.Searches[0], turn.Searches[1]
	if first.ToolUseID != "toolu_search_b1" || !first.Invocations[0].Stdin {
		t.Errorf("first search = %+v, want the --stdin call toolu_search_b1", first)
	}
	// Its id sits in a JSON document inside a JSON string, and the flattened
	// result has a stderr line in front of it: the structured stdout is what
	// makes it readable.
	if !first.Anchored() || first.RequestIDs[0] != "req_synthetic_0001" || first.Loss != "" {
		t.Errorf("first search: RequestIDs = %v, Loss = %q; want req_synthetic_0001 and no loss", first.RequestIDs, first.Loss)
	}
	if second.Anchored() || second.Loss != LossHumanFormat {
		t.Errorf("second search: RequestIDs = %v, Loss = %q; want none and %q", second.RequestIDs, second.Loss, LossHumanFormat)
	}
	var results []Origin
	for _, ev := range turn.Events {
		if ev.Kind == KindSearchResult {
			results = append(results, ev.Origin)
		}
	}
	if len(results) != 2 || results[0] != OriginRouter || results[1] != OriginClient {
		t.Errorf("search result origins = %v, want [router client]", results)
	}
}

func TestSubagentSearchStaysInItsRunAndNamesItsParentTurn(t *testing.T) {
	s := session(t, sessSubagent)
	if len(s.Turns) != 2 {
		t.Fatalf("main turns = %d, want 2", len(s.Turns))
	}
	for _, turn := range s.Turns {
		if len(turn.Searches) != 0 {
			t.Fatalf("main turn %d holds %d searches: a subagent's search was flattened into its parent", turn.Index, len(turn.Searches))
		}
	}
	if len(s.Subagents) != 2 {
		t.Fatalf("subagent runs = %d, want 2", len(s.Subagents))
	}
	run := s.Subagents[0]
	if run.AgentID != "c0ffee01" || run.ParentToolUseID != "toolu_agent_c1" {
		t.Fatalf("run = %+v", run)
	}
	// The spawning call is in the SECOND main turn; index 0 would be the
	// answer of a reader that did not look.
	if run.ParentTurn != 1 {
		t.Errorf("ParentTurn = %d, want 1", run.ParentTurn)
	}
	if len(run.Turns) != 1 || len(run.Turns[0].Searches) != 1 || run.Turns[0].Searches[0].RequestIDs[0] != "req_synthetic_0002" {
		t.Fatalf("subagent run turns = %+v", run.Turns)
	}
	sub := run.Turns[0]
	if sub.StartOrigin != OriginHostModel {
		t.Errorf("subagent StartOrigin = %q: its prompt was written by the parent's model, not a participant", sub.StartOrigin)
	}
	if sub.End != EndYieldedUnmarked {
		t.Errorf("subagent End = %q, want %q: a sidechain has no turn_duration marker", sub.End, EndYieldedUnmarked)
	}

	fork := s.Subagents[1]
	if len(fork.Turns) != 1 || fork.Turns[0].Events[0].Kind != KindAssistantText {
		t.Fatalf("a fork starts at its first model entry; got %+v", fork.Turns)
	}
	if fork.ParentTurn != -1 {
		t.Errorf("fork ParentTurn = %d: its sidecar names a call the parent never made, so it must stay unresolved", fork.ParentTurn)
	}
	unlinked := refusalsOf(s, RefuseSubagentUnlinked)
	if len(unlinked) != 1 || !strings.HasSuffix(unlinked[0].File, "agent-c0ffee02.jsonl") {
		t.Errorf("subagent_unlinked refusals = %+v, want one naming agent-c0ffee02.jsonl", unlinked)
	}
}

func TestInterruptedTurnEndsAtTheInterruptionNotAtTheNextPrompt(t *testing.T) {
	s := session(t, sessInterrupted)
	if len(s.Turns) != 2 {
		t.Fatalf("turns = %d, want 2", len(s.Turns))
	}
	first := s.Turns[0]
	if first.End != EndInterrupted {
		t.Fatalf("End = %q, want %q", first.End, EndInterrupted)
	}
	// The attachment the host wrote after the interruption belongs to no
	// turn. A boundary widened to the next participant entry would pull it
	// in as a fourth event.
	wantKinds(t, first, KindPrompt, KindSearchCall, KindInterrupt)
	if first.EndLine != 3 {
		t.Errorf("EndLine = %d, want 3 (the interruption marker)", first.EndLine)
	}
	if len(first.Searches) != 1 || first.Searches[0].HasResult || first.Searches[0].Loss != LossNoResult {
		t.Errorf("interrupted search = %+v, want no result and %q", first.Searches[0], LossNoResult)
	}
	if second := s.Turns[1]; second.End != EndYielded || second.StartLine != 6 {
		t.Errorf("second turn = start line %d, End %q; want 6, yielded", second.StartLine, second.End)
	}
}

func TestCompactionInsideATurnDoesNotSplitIt(t *testing.T) {
	s := session(t, sessCompaction)
	if len(s.Turns) != 1 {
		t.Fatalf("turns = %d, want 1: the compaction summary is a user entry and must not start a turn", len(s.Turns))
	}
	turn := s.Turns[0]
	if !turn.Compacted || turn.End != EndYielded {
		t.Errorf("Compacted = %v, End = %q; want true, yielded", turn.Compacted, turn.End)
	}
	wantKinds(t, turn, KindPrompt, KindToolCall, KindToolResult, KindCompaction, KindCompactSummary, KindAssistantText)
	if o := turn.Events[4].Origin; o != OriginHostModel {
		t.Errorf("compaction summary origin = %q, want host_model: the model wrote it", o)
	}
	if o := turn.Events[2].Origin; o != OriginToolResult {
		t.Errorf("file read result origin = %q, want tool_result", o)
	}
}

func TestUnknownShapesAreRefusedByNameAndMarkTheirTurn(t *testing.T) {
	s := session(t, sessUnknown)
	want := []Refusal{
		{Line: 2, Reason: RefuseUnknownEntryType, EntryType: "quantum-state"},
		{Line: 3, Reason: RefuseUnknownBlockType, EntryType: "assistant", Detail: "hologram"},
		{Line: 4, Reason: RefuseUnknownSystemSubtype, EntryType: "system", Detail: "warp_drive"},
	}
	if len(s.Refusals) != len(want) {
		t.Fatalf("refusals = %+v, want %d", s.Refusals, len(want))
	}
	for i, w := range want {
		got := s.Refusals[i]
		w.File = "-synthetic-workspace/" + sessUnknown + ".jsonl"
		if got != w {
			t.Errorf("refusal %d = %+v, want %+v", i, got, w)
		}
	}
	if len(s.Turns) != 1 {
		t.Fatalf("turns = %d, want 1", len(s.Turns))
	}
	turn := s.Turns[0]
	if turn.Refusals != 3 {
		t.Errorf("turn.Refusals = %d, want 3: a consumer must be able to leave this turn out", turn.Refusals)
	}
	// What the reader did understand is still there, and nothing from the
	// refused entries became an event.
	wantKinds(t, turn, KindPrompt, KindAssistantText)
}

func TestTruncatedFinalLineIsACountedRefusalNotAnError(t *testing.T) {
	s := session(t, sessTruncated)
	got := refusalsOf(s, RefuseTruncatedFinalLine)
	if len(got) != 1 || got[0].Line != 3 {
		t.Fatalf("truncated_final_line refusals = %+v, want one at line 3", s.Refusals)
	}
	if len(s.Refusals) != 1 {
		t.Errorf("refusals = %+v, want only the truncated line", s.Refusals)
	}
	if len(s.Turns) != 1 || s.Turns[0].End != EndOpenAtEOF {
		t.Fatalf("turns = %+v, want one ending %q", s.Turns, EndOpenAtEOF)
	}
	wantKinds(t, s.Turns[0], KindPrompt, KindToolCall)
}

func TestAnUnparseableLineInTheMiddleIsNotCalledTruncation(t *testing.T) {
	dir := t.TempDir()
	body := `{"type":"user","uuid":"u","sessionId":"s","promptSource":"typed","message":{"role":"user","content":"x"}}` + "\n" +
		`{"type":"assistant","uuid":` + "\n" +
		`{"type":"system","subtype":"turn_duration","uuid":"d"}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "s.jsonl"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	var got *Session
	if err := Walk(dir, Options{}, func(s *Session) { got = s }); err != nil {
		t.Fatal(err)
	}
	if len(got.Refusals) != 1 || got.Refusals[0].Reason != RefuseUnparseableLine || got.Refusals[0].Line != 2 {
		t.Fatalf("refusals = %+v, want unparseable_line at line 2", got.Refusals)
	}
}

func TestModelActivityWithNoOpenTurnIsRefusedNotFoldedBack(t *testing.T) {
	dir := t.TempDir()
	body := `{"type":"user","uuid":"u","sessionId":"s","promptSource":"typed","message":{"role":"user","content":"x"}}` + "\n" +
		`{"type":"assistant","uuid":"a1","message":{"role":"assistant","stop_reason":"end_turn","content":[{"type":"text","text":"y"}]}}` + "\n" +
		`{"type":"system","subtype":"turn_duration","uuid":"d"}` + "\n" +
		`{"type":"assistant","uuid":"a2","message":{"role":"assistant","stop_reason":"end_turn","content":[{"type":"text","text":"z"}]}}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "s.jsonl"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	var got *Session
	if err := Walk(dir, Options{}, func(s *Session) { got = s }); err != nil {
		t.Fatal(err)
	}
	if len(got.Turns) != 1 || len(got.Turns[0].Events) != 2 {
		t.Fatalf("turns = %+v: the entry after the yield must not join the turn before it", got.Turns)
	}
	if r := refusalsOf(got, RefuseEntryOutsideTurn); len(r) != 1 || r[0].Line != 4 {
		t.Fatalf("refusals = %+v, want entry_outside_turn at line 4", got.Refusals)
	}
}

func TestAnUndeclaredPromptSourceRefusesTheTurnItWouldHaveStarted(t *testing.T) {
	dir := t.TempDir()
	body := `{"type":"user","uuid":"u","sessionId":"s","promptSource":"telepathy","message":{"role":"user","content":"x"}}` + "\n" +
		`{"type":"assistant","uuid":"a1","message":{"role":"assistant","stop_reason":"end_turn","content":[{"type":"text","text":"y"}]}}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "s.jsonl"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	var got *Session
	if err := Walk(dir, Options{}, func(s *Session) { got = s }); err != nil {
		t.Fatal(err)
	}
	if len(got.Turns) != 0 {
		t.Fatalf("turns = %+v: nobody declared who writes a %q prompt, so no origin may be guessed for it", got.Turns, "telepathy")
	}
	if len(got.Refusals) != 1 || got.Refusals[0].Reason != RefuseMalformedEntry || got.Refusals[0].Detail != "promptSource" {
		t.Fatalf("refusals = %+v, want exactly one, for promptSource", got.Refusals)
	}
}

func TestContentIsRetainedOnlyWhenAsked(t *testing.T) {
	for _, s := range readFixtures(t, Options{}) {
		for _, turn := range allTurns(s) {
			for _, ev := range turn.Events {
				if ev.Text != "" || len(ev.Input) != 0 {
					t.Fatalf("%s line %d: a %s event retained content with KeepContent off", s.File, ev.Line, ev.Kind)
				}
			}
		}
	}
	kept := readFixtures(t, Options{KeepContent: true})[sessPlain].Turns[0]
	if !strings.HasPrefix(kept.Events[0].Text, contentMarker) {
		t.Fatalf("KeepContent did not retain the prompt: %+v", kept.Events[0])
	}
	if kept.Events[0].Bytes != len(kept.Events[0].Text) {
		t.Errorf("Bytes = %d, want the content's size %d", kept.Events[0].Bytes, len(kept.Events[0].Text))
	}
}

func allTurns(s *Session) []*Turn {
	turns := append([]*Turn{}, s.Turns...)
	for _, run := range s.Subagents {
		turns = append(turns, run.Turns...)
	}
	return turns
}

// TestWalkNeverReadsOutsideTheNamedDirectory plants a transcript outside the
// directory and a link to it inside. The link is found by the walk and must
// not be followed: the outside file holds a turn, and no turn may come back.
func TestWalkNeverReadsOutsideTheNamedDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating a symbolic link needs a privilege the Windows runner does not grant")
	}
	outside, named := t.TempDir(), t.TempDir()
	body := `{"type":"user","uuid":"u","sessionId":"s","promptSource":"typed","message":{"role":"user","content":"x"}}` + "\n" +
		`{"type":"assistant","uuid":"a","message":{"role":"assistant","stop_reason":"end_turn","content":[{"type":"text","text":"y"}]}}` + "\n"
	target := filepath.Join(outside, "secret.jsonl")
	if err := os.WriteFile(target, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(named, "link.jsonl")); err != nil {
		t.Fatal(err)
	}
	visited := 0
	err := Walk(named, Options{}, func(s *Session) {
		visited++
		if len(s.Turns) != 0 {
			t.Errorf("read %d turn(s) through a link that leaves the named directory", len(s.Turns))
		}
		if r := refusalsOf(s, RefuseUnreadableFile); len(r) != 1 {
			t.Errorf("refusals = %+v, want the link refused as unreadable_file", s.Refusals)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if visited != 1 {
		t.Fatalf("visited %d sessions, want 1: the link must be seen and refused, not silently skipped", visited)
	}
}
