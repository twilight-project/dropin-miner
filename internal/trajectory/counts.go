package trajectory

import (
	"fmt"
	"io"
	"sort"
)

// Counts is everything scan prints, and it holds numbers only: no field can
// carry a message, a command, a query, a path from inside a transcript or an
// id. The keys of its maps are this package's own closed vocabularies, plus
// host type names passed through printableType.
type Counts struct {
	Transcripts        int
	SubagentRuns       int
	SubagentRunsLinked int
	HostVersions       map[string]bool

	// TurnStarts is every turn the rule opened; Turns is those in which the
	// model did anything. A slash-command echo or a shell escape opens a
	// turn and often nothing follows, so the two differ.
	TurnStarts       int
	Turns            int
	TurnsByEnd       map[EndReason]int
	TurnsByStart     map[Origin]int
	TurnsCompacted   int
	TurnsWithRefusal int
	// TurnsManyPromptIDs counts turns whose entries carry more than one of
	// the host's own turn labels: the reader's boundary against the host's.
	TurnsManyPromptIDs int
	// DuplicateTurns were skipped: a resumed session copies earlier entries
	// into its new file, and a turn is counted once.
	DuplicateTurns int

	TurnsWithSearch  int
	Searches         int
	SearchesMain     int
	SearchesSubagent int
	Invocations      int

	AnchoredPath1      int
	DistinctRequestIDs map[string]bool
	LossByReason       map[LossReason]int
	// SearchesByShape counts searches whose envelope shape decided what could
	// be read out of them. A shape is not a loss: a merged-view search is
	// anchored and simply has no citations to offer.
	SearchesByShape map[ResultShape]int

	// Path 2, against the local lineage files only. Session is the lane id,
	// Turn and Call the finer ones; each counts searches.
	Path2Session, Path2Turn, Path2Call  int
	SearchTurnsPath2Session             int
	SearchTurnsPath2Turn                int
	SessionsInLineage                   int
	AnchoredEither                      int
	LineageFiles, LineageSkipped        int
	LineageSessions, LineageTurnsOrCall int

	RefusalsByReason map[RefusalReason]int
	RefusalsByShape  map[string]int

	SessionIDMismatch int
	// SubstringHits is the overcount: occurrences of the search command's
	// text anywhere in a raw file. SubstringFiles is files with at least one.
	SubstringHits, SubstringFiles int

	seenTurns map[string]bool
}

// NewCounts returns empty counts, carrying the lineage index's own size.
func NewCounts(idx *LineageIndex) *Counts {
	c := &Counts{
		HostVersions: map[string]bool{}, TurnsByEnd: map[EndReason]int{}, TurnsByStart: map[Origin]int{},
		DistinctRequestIDs: map[string]bool{}, LossByReason: map[LossReason]int{},
		SearchesByShape:  map[ResultShape]int{},
		RefusalsByReason: map[RefusalReason]int{}, RefusalsByShape: map[string]int{},
		seenTurns: map[string]bool{},
	}
	if idx != nil {
		c.LineageFiles, c.LineageSkipped = idx.Files, idx.Skipped
		c.LineageSessions, c.LineageTurnsOrCall = len(idx.Sessions), len(idx.Turns)+len(idx.Calls)
	}
	return c
}

// ModelActivity reports whether the model did anything in the turn.
func (t *Turn) ModelActivity() bool {
	for i := range t.Events {
		if t.Events[i].Origin == OriginHostModel && t.Events[i].Kind != KindCompactSummary {
			return true
		}
	}
	return false
}

// startUUID identifies a turn across files.
func (t *Turn) startUUID() string {
	if len(t.Events) == 0 {
		return ""
	}
	return t.Events[0].UUID
}

// Add folds one session into the counts.
func (c *Counts) Add(s *Session, idx *LineageIndex) {
	if s.File != "" {
		c.Transcripts++
	}
	for v := range s.Versions {
		c.HostVersions[v] = true
	}
	for _, r := range s.Refusals {
		c.RefusalsByReason[r.Reason]++
		shape := string(r.Reason)
		if r.EntryType != "" {
			shape += " " + r.EntryType
		}
		if r.Detail != "" {
			shape += "/" + r.Detail
		}
		c.RefusalsByShape[shape]++
	}
	c.SessionIDMismatch += s.SessionIDMismatch
	c.SubstringHits += s.SubstringHits
	if s.SubstringHits > 0 {
		c.SubstringFiles++
	}
	if idx != nil && idx.Sessions[TraceHash(s.SessionID)] {
		c.SessionsInLineage++
	}
	c.addTurns(s.SessionID, "", s.Turns, idx)
	for _, run := range s.Subagents {
		c.SubagentRuns++
		if run.ParentTurn >= 0 {
			c.SubagentRunsLinked++
		}
		c.addTurns(s.SessionID, run.AgentID, run.Turns, idx)
	}
}

func (c *Counts) addTurns(sessionID, agentID string, turns []*Turn, idx *LineageIndex) {
	for _, t := range turns {
		if id := t.startUUID(); id != "" {
			if c.seenTurns[id] {
				c.DuplicateTurns++
				continue
			}
			c.seenTurns[id] = true
		}
		c.TurnStarts++
		if !t.ModelActivity() {
			continue
		}
		c.Turns++
		c.TurnsByEnd[t.End]++
		c.TurnsByStart[t.StartOrigin]++
		if t.Compacted {
			c.TurnsCompacted++
		}
		if t.Refusals > 0 {
			c.TurnsWithRefusal++
		}
		if t.PromptIDs > 1 {
			c.TurnsManyPromptIDs++
		}
		if len(t.Searches) == 0 {
			continue
		}
		c.TurnsWithSearch++
		var turnSession, turnTurn bool
		for _, s := range t.Searches {
			c.Searches++
			c.Invocations += len(s.Invocations)
			if agentID == "" {
				c.SearchesMain++
			} else {
				c.SearchesSubagent++
			}
			if s.Shape != "" {
				c.SearchesByShape[s.Shape]++
			}
			if s.Anchored() {
				c.AnchoredPath1++
				for _, id := range s.RequestIDs {
					c.DistinctRequestIDs[id] = true
				}
			} else {
				c.LossByReason[s.Loss]++
			}
			m := idx.Match(TraceIDsFor(sessionID, agentID, t.PromptID, s.ToolUseID))
			if m.Session {
				c.Path2Session++
			}
			if m.Turn {
				c.Path2Turn++
			}
			if m.Call {
				c.Path2Call++
			}
			if s.Anchored() || m.Session || m.Turn || m.Call {
				c.AnchoredEither++
			}
			turnSession, turnTurn = turnSession || m.Session, turnTurn || m.Turn
		}
		if turnSession {
			c.SearchTurnsPath2Session++
		}
		if turnTurn {
			c.SearchTurnsPath2Turn++
		}
	}
}

// WriteTo prints the counts. It prints nothing else: there is no code path
// from a transcript's content to this writer.
func (c *Counts) WriteTo(w io.Writer) (int64, error) {
	var n int64
	p := func(format string, args ...any) {
		k, _ := fmt.Fprintf(w, format, args...)
		n += int64(k)
	}
	p("counts only; every figure is from parsed structure unless it says substring\n\n")
	p("transcripts                               %d\n", c.Transcripts)
	p("subagent runs                             %d (linked to a parent turn: %d)\n", c.SubagentRuns, c.SubagentRunsLinked)
	p("host versions seen                        %d\n", len(c.HostVersions))
	p("turn starts                               %d (duplicates across files skipped: %d)\n", c.TurnStarts, c.DuplicateTurns)
	p("turns (model did something)               %d\n", c.Turns)
	for _, k := range sortedKeys(c.TurnsByStart) {
		p("  started by %-29s %d\n", k, c.TurnsByStart[k])
	}
	for _, k := range sortedKeys(c.TurnsByEnd) {
		p("  ended %-34s %d\n", k, c.TurnsByEnd[k])
	}
	p("  compacted inside the turn               %d\n", c.TurnsCompacted)
	p("  containing a refused entry              %d\n", c.TurnsWithRefusal)
	p("  carrying more than one host prompt id   %d\n", c.TurnsManyPromptIDs)
	p("turns containing a search                 %d\n", c.TurnsWithSearch)
	p("searches (tool calls)                     %d (main chain %d, subagent %d; invocations %d)\n",
		c.Searches, c.SearchesMain, c.SearchesSubagent, c.Invocations)
	p("anchored searches\n")
	p("  path 1, request id read from the result %d (distinct request ids: %d)\n", c.AnchoredPath1, len(c.DistinctRequestIDs))
	p("  path 2, recomputed id in local lineage  session %d, turn %d, call %d\n", c.Path2Session, c.Path2Turn, c.Path2Call)
	p("  either path                             %d\n", c.AnchoredEither)
	p("search turns anchored by path 2           session %d, turn %d\n", c.SearchTurnsPath2Session, c.SearchTurnsPath2Turn)
	p("transcripts whose session id is in lineage %d (lineage files %d, skipped %d, distinct sessions %d)\n",
		c.SessionsInLineage, c.LineageFiles, c.LineageSkipped, c.LineageSessions)
	for _, shape := range sortedKeys(c.SearchesByShape) {
		p("searches whose envelope shape was %-9s %d (anchored; citations unavailable)\n", shape, c.SearchesByShape[shape])
	}
	p("path 1 losses by reason                   %d\n", c.Searches-c.AnchoredPath1)
	for _, k := range sortedKeys(c.LossByReason) {
		p("  %-40s %d\n", k, c.LossByReason[k])
	}
	total := 0
	for _, v := range c.RefusalsByReason {
		total += v
	}
	p("refusals by reason                        %d\n", total)
	for _, k := range sortedKeys(c.RefusalsByReason) {
		p("  %-40s %d\n", k, c.RefusalsByReason[k])
	}
	p("refusals by shape\n")
	for _, k := range sortedKeys(c.RefusalsByShape) {
		p("  %-40s %d\n", k, c.RefusalsByShape[k])
	}
	p("entries whose session id is not their file's %d\n", c.SessionIDMismatch)
	p("substring comparison (the overcount)      %d occurrences of the search command text in %d files\n",
		c.SubstringHits, c.SubstringFiles)
	return n, nil
}

func sortedKeys[K ~string, V any](m map[K]V) []K {
	keys := make([]K, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	return keys
}
