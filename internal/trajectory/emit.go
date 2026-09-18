package trajectory

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
)

// A record is one agent turn in which at least one search went through this
// client. What it carries is decided per workspace by the gates, and every
// string in it has been through the scrubber first.
//
//	level 1  the turn's searches in order: hashed session, turn and call ids
//	         — the ones the router already holds — and request ids. No text.
//	level 2  level 1 and outcome labels (labels.go). Positions and ids.
//	level 3  level 2 and the turn's other events, each tagged by origin. An
//	         event's text rides only when its own content class is open too.
//
// Level 3 is defined as level 2 plus the rest, so it needs both level gates.
// Needing a gate is not opening it: level_3 alone leaves a record at level 1
// and names level_2 as what withheld the rest.

const recordVersion = 1

const hostClaudeCode = "claude-code"

// Record is one line of emit's output.
type Record struct {
	V             int    `json:"v"`
	PolicyVersion string `json:"policy_version"`
	Level         int    `json:"level"`
	// Gates lists the gates that were open for this record's workspace.
	Gates        []Gate        `json:"gates"`
	SessionID    string        `json:"session_id"`
	TurnID       string        `json:"turn_id,omitempty"`
	Subagent     bool          `json:"subagent,omitempty"`
	ParentTurnID string        `json:"parent_turn_id,omitempty"`
	Host         string        `json:"host,omitempty"`
	HostVersion  string        `json:"host_version,omitempty"`
	Model        string        `json:"model,omitempty"`
	Workspace    string        `json:"workspace,omitempty"`
	End          EndReason     `json:"end,omitempty"`
	Events       []RecordEvent `json:"events"`
	Labels       []Label       `json:"labels,omitempty"`
}

// RecordEvent is one event of a record. Origin is on every one of them, at
// every level: it is what lets a corpus be divided later without re-reading.
type RecordEvent struct {
	Seq        int             `json:"seq"`
	Kind       Kind            `json:"kind"`
	Origin     Origin          `json:"origin"`
	CallID     string          `json:"call_id,omitempty"`
	RequestIDs []string        `json:"request_ids,omitempty"`
	Tool       string          `json:"tool,omitempty"`
	IsError    bool            `json:"is_error,omitempty"`
	Bytes      int             `json:"bytes,omitempty"`
	Text       string          `json:"text,omitempty"`
	Input      json.RawMessage `json:"input,omitempty"`
	Query      string          `json:"query,omitempty"`
	// Omitted says the scrubber withheld this event's content whole, and why.
	Omitted ScrubClass `json:"omitted,omitempty"`
}

// EmitStats is what emit reports on stderr. Counts only.
type EmitStats struct {
	Records        int
	RecordsByLevel map[int]int
	// Withheld counts, per closed gate, the records that held material the
	// gate kept out; Blockers says why each such gate was closed.
	Withheld map[Gate]int
	Blockers map[Gate]map[string]int
	// Refused are gates a file asked for that the ceiling will never open.
	Refused map[Gate]bool

	// Measured, not written. Every qualifying record is also sized as it
	// would be at each level with every buildable gate open, and scrubbed in
	// full, so the findings can say what a record costs and what the
	// scrubber catches without anything being emitted that a gate withheld.
	MeasuredBytes  map[int]int64
	ScrubOmitted   map[ScrubClass]int
	ScrubRewritten map[ScrubClass]int
	ScrubbedItems  int
	Labels         map[string]int
	SearchesNoURLs int // searches whose result offered no citation to label
	// SearchesCitationsUnavailable counts searches whose envelope shape left
	// no citations to read. Kept apart from SearchesNoURLs: a result that
	// offered none and a result whose shape carries none in the first place
	// are different facts, and only the second is this client's own doing.
	SearchesCitationsUnavailable int
}

// scrubbed is an event's content after the scrubber, computed once.
type scrubbed struct {
	text, query string
	input       json.RawMessage
	omit        ScrubClass
}

type emitContext struct {
	sessionID, agentID, parentTurnID string
}

func newEmitStats() *EmitStats {
	return &EmitStats{
		RecordsByLevel: map[int]int{}, Withheld: map[Gate]int{}, Blockers: map[Gate]map[string]int{},
		Refused: map[Gate]bool{}, MeasuredBytes: map[int]int64{}, ScrubOmitted: map[ScrubClass]int{},
		ScrubRewritten: map[ScrubClass]int{}, Labels: map[string]int{},
	}
}

// Emit writes one record per qualifying turn under dir to w, as JSON lines.
// w is the only thing it writes to; the caller opened it.
func Emit(dir string, policy *Policy, scrub *Scrubber, w io.Writer) (*EmitStats, error) {
	stats := newEmitStats()
	if policy.Config != nil {
		for gate, asked := range policy.Config.Gates {
			if asked && !compiledCeiling[gate] {
				stats.Refused[gate] = true
			}
		}
	}
	seen := map[string]bool{}
	var writeErr error
	one := func(t *Turn, ctx emitContext) {
		if writeErr != nil || len(t.Searches) == 0 {
			return
		}
		if id := t.startUUID(); id != "" {
			if seen[id] {
				return
			}
			seen[id] = true
		}
		writeErr = emitTurn(t, ctx, policy.For(t.Cwd), scrub, stats, w)
	}
	err := Walk(dir, Options{KeepContent: true}, func(s *Session) {
		for _, t := range s.Turns {
			one(t, emitContext{sessionID: s.SessionID})
		}
		for _, run := range s.Subagents {
			ctx := emitContext{sessionID: s.SessionID, agentID: run.AgentID}
			if parent := parentTurnOf(s, run); parent != nil && parent.PromptID != "" {
				ctx.parentTurnID = TraceHash(s.SessionID + "|" + parent.PromptID)
			}
			for _, t := range run.Turns {
				one(t, ctx)
			}
		}
	})
	if err != nil {
		return stats, err
	}
	return stats, writeErr
}

func parentTurnOf(s *Session, run *SubagentRun) *Turn {
	turns := s.Turns
	if run.ParentAgentID != "" {
		turns = nil
		for _, other := range s.Subagents {
			if other.AgentID == run.ParentAgentID {
				turns = other.Turns
			}
		}
	}
	if run.ParentTurn < 0 || run.ParentTurn >= len(turns) {
		return nil
	}
	return turns[run.ParentTurn]
}

// measureTurn is everything emit does to a turn except write it: the content
// is scrubbed, the labels derived, and the record sized at each level with
// every buildable gate open. Emit calls it for the record it goes on to
// write, and measure calls it for the numbers alone — so what the findings
// say a record costs is what emit's records cost, not a second count that
// happens to agree.
func measureTurn(t *Turn, ctx emitContext, scrub *Scrubber, stats *EmitStats) ([]scrubbed, []Label, error) {
	content := make([]scrubbed, len(t.Events))
	for i, ev := range t.Events {
		content[i] = scrubEvent(ev, scrub, stats)
	}
	labels := labelsOf(t)
	for _, l := range labels {
		stats.Labels[l.Type]++
	}
	for _, s := range t.Searches {
		switch {
		case s.Shape == ShapeMergedView:
			stats.SearchesCitationsUnavailable++
		case len(s.Citations) == 0:
			stats.SearchesNoURLs++
		}
	}
	for level, set := range measuringGateSets() {
		line, err := json.Marshal(buildRecord(t, ctx, set, content, labels))
		if err != nil {
			return nil, nil, err
		}
		stats.MeasuredBytes[level] += int64(len(line)) + 1
	}
	return content, labels, nil
}

func emitTurn(t *Turn, ctx emitContext, gates GateSet, scrub *Scrubber, stats *EmitStats, w io.Writer) error {
	content, labels, err := measureTurn(t, ctx, scrub, stats)
	if err != nil {
		return err
	}

	rec := buildRecord(t, ctx, gates, content, labels)
	noteWithheld(t, gates, labels, stats)
	line, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	if _, err := w.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("write record: %w", err)
	}
	stats.Records++
	stats.RecordsByLevel[rec.Level]++
	return nil
}

// measuringGateSets are the three gate sets a record is sized under: nothing
// open, level 2 alone, and everything the ceiling allows.
func measuringGateSets() map[int]GateSet {
	sets := map[int]GateSet{1: {}, 2: {}, 3: {}}
	for _, gate := range AllGates {
		sets[1][gate] = []string{BlockedByConfig}
		sets[2][gate] = []string{BlockedByConfig}
		sets[3][gate] = []string{BlockedByCeiling}
		if compiledCeiling[gate] {
			sets[3][gate] = nil
		}
	}
	sets[2][GateLevel2] = nil
	return sets
}

// scrubEvent scrubs whatever of an event could ever be written. A search
// call's parameters are never among it: the command line carries the trace
// bridge, a base64 envelope that can hold model prose and that no pattern
// here can see into. Only the query parsed out of it is a candidate.
func scrubEvent(ev Event, scrub *Scrubber, stats *EmitStats) scrubbed {
	var out scrubbed
	note := func(res ScrubResult) {
		stats.ScrubbedItems++
		if res.Omit != "" {
			out.omit = res.Omit
		}
		for class, n := range res.Rewrites {
			stats.ScrubRewritten[class] += n
		}
	}
	switch ev.Kind {
	case KindPrompt, KindAssistantText, KindToolResult, KindSearchResult:
		var res ScrubResult
		out.text, res = scrub.Text(ev.Text)
		note(res)
	case KindToolCall:
		var res ScrubResult
		out.input, res = scrub.JSON(ev.Input)
		note(res)
	case KindSearchCall:
		if ev.Search != nil && len(ev.Search.queries) == 1 {
			var res ScrubResult
			out.query, res = scrub.Text(ev.Search.queries[0])
			note(res)
		}
	}
	if out.omit != "" {
		stats.ScrubOmitted[out.omit]++
		out.text, out.query, out.input = "", "", nil
	}
	return out
}

// contentGate is the class an event kind's content belongs to. Kinds absent
// from it carry no content at any setting: model reasoning, which the host
// itself never exports; host notes and attachments, which have no class; and
// a compaction summary, which describes turns other than this one.
var contentGate = map[Kind]Gate{
	KindPrompt: GatePrompts, KindAssistantText: GateResponses,
	KindToolCall: GateToolDetails, KindSearchCall: GateToolDetails,
	KindToolResult: GateToolContent, KindSearchResult: GateToolContent,
}

func buildRecord(t *Turn, ctx emitContext, gates GateSet, content []scrubbed, labels []Label) Record {
	ids := TraceIDsFor(ctx.sessionID, ctx.agentID, t.PromptID, "")
	rec := Record{
		V: recordVersion, PolicyVersion: PolicyVersion, Level: 1, Gates: gates.OpenGates(),
		SessionID: ids.Session, TurnID: ids.Turn, Subagent: ctx.agentID != "", ParentTurnID: ctx.parentTurnID,
		Events: []RecordEvent{},
	}
	if rec.Gates == nil {
		rec.Gates = []Gate{}
	}
	if gates.Open(GateLevel2) {
		rec.Level = 2
		rec.Labels = labels
		if gates.Open(GateLevel3) {
			rec.Level = 3
			rec.End = t.End
		}
	}
	if gates.Open(GateHostAndModel) {
		rec.Host, rec.HostVersion, rec.Model = hostClaudeCode, safeName(t.HostVersion), safeName(t.Model)
	}
	if gates.Open(GateWorkspace) && t.Cwd != "" {
		rec.Workspace = WorkspaceHash(t.Cwd)
	}
	for i, ev := range t.Events {
		isSearch := ev.Kind == KindSearchCall || ev.Kind == KindSearchResult
		if !isSearch && rec.Level < 3 {
			continue
		}
		out := RecordEvent{Seq: i, Kind: ev.Kind, Origin: ev.Origin}
		if ev.ToolUseID != "" {
			out.CallID = TraceHash(ctx.sessionID + "|" + ev.ToolUseID)
		}
		if ev.Kind == KindSearchResult && ev.Search != nil {
			out.RequestIDs = ev.Search.RequestIDs
		}
		if rec.Level == 3 {
			out.Tool, out.IsError, out.Bytes = safeName(ev.ToolName), ev.IsError, ev.Bytes
			if class, ok := contentGate[ev.Kind]; ok && gates.Open(class) {
				c := content[i]
				out.Text, out.Input, out.Query, out.Omitted = c.text, c.input, c.query, c.omit
			}
		}
		rec.Events = append(rec.Events, out)
	}
	return rec
}

// safeName passes a host-supplied name — a tool, a model, a version — only
// if it is shaped like one. These are not scrubbed as text, so they are held
// to an identifier's alphabet instead; nothing stays nothing.
func safeName(s string) string {
	if s == "" {
		return ""
	}
	return printableType(s)
}

// noteWithheld records, for each closed gate, whether this turn held
// something the gate kept out — the list emit names on stderr.
func noteWithheld(t *Turn, gates GateSet, labels []Label, stats *EmitStats) {
	has := map[Gate]bool{
		GateLevel2:       len(labels) > 0,
		GateLevel3:       len(t.Events) > 0,
		GateHostAndModel: t.Model != "" || t.HostVersion != "",
		GateWorkspace:    t.Cwd != "",
	}
	for _, ev := range t.Events {
		if class, ok := contentGate[ev.Kind]; ok && ev.Bytes > 0 {
			has[class] = true
		}
	}
	for _, gate := range AllGates {
		if gates.Open(gate) || !has[gate] {
			continue
		}
		stats.Withheld[gate]++
		if stats.Blockers[gate] == nil {
			stats.Blockers[gate] = map[string]int{}
		}
		for _, why := range gates[gate] {
			stats.Blockers[gate][why]++
		}
	}
}

// WriteTo prints the stats. Counts and this package's own vocabulary only.
func (s *EmitStats) WriteTo(w io.Writer) (int64, error) {
	var n int64
	p := func(format string, args ...any) {
		k, _ := fmt.Fprintf(w, format, args...)
		n += int64(k)
	}
	p("records written                 %d\n", s.Records)
	for _, level := range []int{1, 2, 3} {
		p("  at level %d                    %d\n", level, s.RecordsByLevel[level])
	}
	if s.Records == s.RecordsByLevel[1] {
		p("every record is level 1: ids only, no text. That is the default with no config and no consent record.\n")
	}
	p("closed gates that withheld something (records affected; why closed)\n")
	for _, gate := range AllGates {
		if s.Withheld[gate] == 0 {
			continue
		}
		p("  %-16s %6d  %s\n", gate, s.Withheld[gate], reasons(s.Blockers[gate]))
	}
	for _, gate := range AllGates {
		if s.Refused[gate] {
			p("  %-16s asked for in the config and refused: the compiled ceiling keeps it closed, and nothing is built behind it\n", gate)
		}
	}
	p("measured, not written — what these records would cost with every buildable gate open\n")
	for _, level := range []int{1, 2, 3} {
		avg := int64(0)
		if s.Records > 0 {
			avg = s.MeasuredBytes[level] / int64(s.Records)
		}
		p("  level %d                        %d bytes total, %d per record\n", level, s.MeasuredBytes[level], avg)
	}
	p("scrubber, over all %d items of content, written or not\n", s.ScrubbedItems)
	for _, class := range sortedKeys(s.ScrubOmitted) {
		p("  omitted whole: %-16s %d\n", class, s.ScrubOmitted[class])
	}
	for _, class := range sortedKeys(s.ScrubRewritten) {
		p("  rewritten:     %-16s %d\n", class, s.ScrubRewritten[class])
	}
	p("outcome labels derivable\n")
	for _, typ := range sortedKeys(s.Labels) {
		p("  %-30s %d\n", typ, s.Labels[typ])
	}
	p("  searches whose result offered no citation to label against: %d\n", s.SearchesNoURLs)
	if s.SearchesCitationsUnavailable > 0 {
		p("  searches whose envelope shape carried no citations at all:   %d\n", s.SearchesCitationsUnavailable)
	}
	return n, nil
}

func reasons(m map[string]int) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := ""
	for i, k := range keys {
		if i > 0 {
			out += ", "
		}
		out += k
	}
	return out
}
