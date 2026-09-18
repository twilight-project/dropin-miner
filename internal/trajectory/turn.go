package trajectory

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
)

// Event is one typed thing that happened inside a turn, in file order.
type Event struct {
	Kind      Kind
	Origin    Origin
	Line      int
	UUID      string
	Timestamp string
	Model     string // host_model events
	ToolUseID string // tool calls, search calls and their results
	ToolName  string
	IsError   bool
	// Bytes is the size of the event's content. It is a count, recorded at
	// every setting, so what a record would cost can be measured without
	// keeping the content.
	Bytes int
	// Text and Input are kept only under Options.KeepContent.
	Text   string
	Input  json.RawMessage
	Search *Search // search_call and search_result events share one
}

// Turn is one agent turn: from a turn-starting entry to the agent yielding.
type Turn struct {
	Index       int
	PromptID    string // the host's own label for the turn, when it wrote one
	StartOrigin Origin // participant, or host for an injected start
	StartLine   int
	EndLine     int
	End         EndReason
	Events      []Event
	Searches    []*Search
	Compacted   bool
	// Refusals counts entries inside this turn that the reader declined. A
	// turn with any is incomplete by the reader's own account.
	Refusals int
	// PromptIDs counts the distinct host turn labels seen on this turn's
	// entries. More than one means the reader's boundary and the host's own
	// disagree, which the findings measure rather than assume away.
	PromptIDs int

	promptIDs map[string]bool
	results   map[string]*Search // tool_use id → awaiting its result
	lastStop  string
}

// Options controls what the reader retains.
type Options struct {
	// KeepContent retains message text and tool parameters on events. scan
	// leaves it false: what is never held cannot be printed.
	KeepContent bool
}

// turnParser applies the turn rule to one file.
//
// The rule. A turn STARTS at a user entry that carries no tool_result block,
// is not flagged isCompactSummary and is not the host's interruption marker;
// promptSource says whether a person or the host wrote it. An isMeta entry is
// a host note inside an open turn and a host-written start when none is open
// (a scheduled task fires this way). In a subagent's file the start was
// written by the parent's model, not by a person, and a fork — which inherits
// its parent's context and so has no prompt entry — starts at its first model
// entry. A turn ENDS at the first of: the host's system/turn_duration entry
// (yielded); the interruption marker (interrupted); the next turn start
// (yielded_unmarked if the last model message stopped with end_turn, else
// superseded); end of file (yielded_unmarked on the same test, else
// open_at_eof). It never extends to the next participant entry by default:
// what falls between a yield and the next start belongs to no turn, and a
// conversation entry found there is refused as entry_outside_turn instead of
// being folded into the turn before it. A compaction inside a turn does not
// end it: the boundary and the model-written summary become events and the
// turn is marked Compacted.
type turnParser struct {
	opts Options
	file string
	// sidechain is true for a subagent's file.
	sidechain bool
	turns     []*Turn
	refusals  []Refusal
	versions  map[string]int
	open      *Turn
	// skipping is set when a turn start was refused: what follows belongs to
	// a turn the reader declined, and the one refusal covers it.
	skipping bool
	// sessionMismatch counts entries whose own sessionId differs from want.
	wantSession     string
	sessionMismatch int
	substringHits   int
	// agentCalls maps an Agent tool call's id to the turn that made it.
	agentCalls map[string]int
}

// searchCommandText is counted as a bare substring for one purpose only: to
// measure how far a substring scan overcounts. No other count uses it.
var searchCommandText = []byte("search --stdin")

func (p *turnParser) refuse(line int, reason RefusalReason, entryType, detail string) {
	p.refusals = append(p.refusals, Refusal{File: p.file, Line: line, Reason: reason, EntryType: entryType, Detail: detail})
	if p.open != nil && reason != RefuseEntryOutsideTurn {
		p.open.Refusals++
	}
}

func (p *turnParser) parse(r io.Reader) error {
	br := bufio.NewReaderSize(r, 1<<20)
	line := 0
	for {
		raw, err := br.ReadBytes('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			p.closeAtBoundary(line, true)
			return err
		}
		atEOF := errors.Is(err, io.EOF)
		if trimmed := bytes.TrimSpace(raw); len(trimmed) > 0 {
			line++
			p.substringHits += bytes.Count(trimmed, searchCommandText)
			p.entry(trimmed, line, atEOF)
		} else if !atEOF {
			line++
		}
		if atEOF {
			break
		}
	}
	p.closeAtBoundary(line, true)
	return nil
}

func (p *turnParser) entry(raw []byte, line int, unterminated bool) {
	var head entryHead
	if err := json.Unmarshal(raw, &head); err != nil {
		reason := RefuseUnparseableLine
		if unterminated {
			// The last line has no newline and does not parse: the host was
			// writing it when the file was copied or the process died.
			reason = RefuseTruncatedFinalLine
		}
		p.refuse(line, reason, "", "")
		return
	}
	switch {
	case bookkeepingEntryTypes[head.Type]:
		return
	case head.Type == entryUser || head.Type == entryAssistant || head.Type == entrySystem || head.Type == entryAttach:
	default:
		p.refuse(line, RefuseUnknownEntryType, printableType(head.Type), "")
		return
	}
	var e rawEntry
	if err := json.Unmarshal(raw, &e); err != nil {
		p.refuse(line, RefuseMalformedEntry, printableType(head.Type), "fields")
		return
	}
	if e.Version != "" {
		p.versions[e.Version]++
	}
	if p.wantSession != "" && e.SessionID != "" && e.SessionID != p.wantSession {
		p.sessionMismatch++
	}
	switch e.Type {
	case entrySystem:
		p.system(&e, line)
	case entryAttach:
		if p.open != nil {
			p.add(Event{Kind: KindHostNote, Origin: OriginHost, Line: line, UUID: e.UUID, Timestamp: e.Timestamp})
		}
	case entryUser:
		p.user(&e, line)
	case entryAssistant:
		p.assistant(&e, line)
	}
}

func (p *turnParser) system(e *rawEntry, line int) {
	switch {
	case e.Subtype == systemTurnDuration:
		if p.open != nil {
			p.close(EndYielded, line)
		}
	case e.Subtype == systemCompactBoundary:
		if p.open != nil {
			p.open.Compacted = true
			p.add(Event{Kind: KindCompaction, Origin: OriginHost, Line: line, UUID: e.UUID, Timestamp: e.Timestamp})
		}
	case inertSystemSubtypes[e.Subtype]:
	default:
		p.refuse(line, RefuseUnknownSystemSubtype, entrySystem, printableType(e.Subtype))
	}
}

func (p *turnParser) user(e *rawEntry, line int) {
	if e.Message == nil {
		p.refuse(line, RefuseMalformedEntry, entryUser, "message")
		return
	}
	blocks, ok := blocksOf(e.Message.Content)
	if !ok {
		p.refuse(line, RefuseMalformedEntry, entryUser, "content")
		return
	}
	hasResult := false
	var text strings.Builder
	for _, b := range blocks {
		switch b.Type {
		case blockText:
			text.WriteString(b.Text)
		case blockImage:
		case blockToolResult:
			hasResult = true
		default:
			p.refuse(line, RefuseUnknownBlockType, entryUser, printableType(b.Type))
			return
		}
	}
	switch {
	case hasResult:
		p.toolResults(e, blocks, line)
	case e.IsCompactSummary:
		if p.open != nil {
			p.add(p.textEvent(KindCompactSummary, OriginHostModel, e, line, text.String()))
		}
	case e.IsMeta && p.open != nil:
		p.add(p.textEvent(KindHostNote, OriginHost, e, line, text.String()))
	case strings.HasPrefix(strings.TrimSpace(text.String()), interruptMarkerPrefix):
		if p.open != nil {
			p.add(Event{Kind: KindInterrupt, Origin: OriginParticipant, Line: line, UUID: e.UUID, Timestamp: e.Timestamp})
			p.close(EndInterrupted, line)
		}
	default:
		p.start(e, line, text.String())
	}
}

// start opens a turn at a turn-starting entry.
func (p *turnParser) start(e *rawEntry, line int, text string) {
	p.closeAtBoundary(line, false)
	origin, ok := startOrigin(e.PromptSource, text)
	switch {
	case !ok:
		p.refuse(line, RefuseMalformedEntry, entryUser, "promptSource")
		p.skipping = true
		return
	case p.sidechain:
		// A subagent is prompted by the model that spawned it.
		origin = OriginHostModel
	case e.IsMeta:
		origin = OriginHost
	}
	p.openTurn(e.PromptID, origin, line)
	p.add(p.textEvent(KindPrompt, origin, e, line, text))
}

func (p *turnParser) openTurn(promptID string, origin Origin, line int) {
	p.skipping = false
	p.open = &Turn{
		Index: len(p.turns), PromptID: promptID, StartOrigin: origin, StartLine: line,
		promptIDs: map[string]bool{}, results: map[string]*Search{},
	}
	p.notePromptID(promptID)
}

// startOrigin decides who wrote a turn-starting entry. ok is false for a
// promptSource value this package has not declared.
func startOrigin(promptSource, text string) (Origin, bool) {
	switch {
	case participantPromptSources[promptSource]:
		return OriginParticipant, true
	case promptSource == promptSourceSystem:
		return OriginHost, true
	case promptSource != "":
		return "", false
	}
	// Older entries, command echoes and shell escapes carry no promptSource.
	trimmed := strings.TrimSpace(text)
	for _, prefix := range hostWrapperPrefixes {
		if strings.HasPrefix(trimmed, prefix) {
			return OriginHost, true
		}
	}
	return OriginParticipant, true
}

func (p *turnParser) toolResults(e *rawEntry, blocks []rawBlock, line int) {
	if p.open == nil {
		if !p.skipping {
			p.refuse(line, RefuseEntryOutsideTurn, entryUser, blockToolResult)
		}
		return
	}
	p.notePromptID(e.PromptID)
	for _, b := range blocks {
		if b.Type != blockToolResult {
			continue
		}
		text := textOf(b.Content)
		ev := Event{
			Kind: KindToolResult, Origin: OriginToolResult, Line: line, UUID: e.UUID, Timestamp: e.Timestamp,
			ToolUseID: b.ToolUseID, IsError: b.IsError, Bytes: len(text),
		}
		if s := p.open.results[b.ToolUseID]; s != nil {
			delete(p.open.results, b.ToolUseID)
			scraped := text
			if stdout, ok := stdoutOf(e.ToolUseResult); ok && strings.TrimSpace(stdout) != "" {
				scraped = stdout
			}
			s.settle(scraped, b.IsError, e.ToolDenialKind != "", line)
			ev.Kind, ev.Search, ev.ToolName = KindSearchResult, s, s.Tool
			// A result the router answered is the router's; anything else a
			// search printed is this client's own output.
			ev.Origin = OriginClient
			if s.Anchored() {
				ev.Origin = OriginRouter
			}
		}
		if p.opts.KeepContent {
			ev.Text = text
		}
		p.add(ev)
	}
}

func (p *turnParser) assistant(e *rawEntry, line int) {
	if p.open == nil && p.sidechain && !p.skipping {
		// A fork: no prompt entry exists to start from.
		p.openTurn("", OriginHostModel, line)
	}
	if p.open == nil {
		if !p.skipping {
			p.refuse(line, RefuseEntryOutsideTurn, entryAssistant, "")
		}
		return
	}
	if e.Message == nil {
		p.refuse(line, RefuseMalformedEntry, entryAssistant, "message")
		return
	}
	blocks, ok := blocksOf(e.Message.Content)
	if !ok {
		p.refuse(line, RefuseMalformedEntry, entryAssistant, "content")
		return
	}
	for _, b := range blocks {
		if b.Type != blockText && b.Type != blockThinking && b.Type != blockToolUse {
			p.refuse(line, RefuseUnknownBlockType, entryAssistant, printableType(b.Type))
			return
		}
	}
	if e.Message.StopReason != nil {
		p.open.lastStop = *e.Message.StopReason
	}
	for _, b := range blocks {
		ev := Event{Origin: OriginHostModel, Line: line, UUID: e.UUID, Timestamp: e.Timestamp, Model: e.Message.Model}
		switch b.Type {
		case blockText:
			ev.Kind, ev.Bytes = KindAssistantText, len(b.Text)
			if p.opts.KeepContent {
				ev.Text = b.Text
			}
		case blockThinking:
			ev.Kind, ev.Bytes = KindThinking, len(b.Thinking)
		case blockToolUse:
			ev.Kind, ev.ToolUseID, ev.ToolName, ev.Bytes = KindToolCall, b.ID, b.Name, len(b.Input)
			if s := searchOf(b, line); s != nil {
				ev.Kind, ev.Search = KindSearchCall, s
				p.open.Searches = append(p.open.Searches, s)
				p.open.results[b.ID] = s
			}
			if agentToolNames[b.Name] {
				p.agentCalls[b.ID] = p.open.Index
			}
			if p.opts.KeepContent {
				ev.Input = b.Input
			}
		}
		p.add(ev)
	}
}

// searchOf recognizes a search by parsing the call's command line.
func searchOf(b rawBlock, line int) *Search {
	if !shellToolNames[b.Name] {
		return nil
	}
	var input struct {
		Command string `json:"command"`
	}
	if json.Unmarshal(b.Input, &input) != nil || input.Command == "" {
		return nil
	}
	invs := parseSearchInvocations(input.Command)
	if len(invs) == 0 {
		return nil
	}
	return &Search{ToolUseID: b.ID, Tool: b.Name, Invocations: invs, CallLine: line, Loss: LossNoResult}
}

func (p *turnParser) textEvent(kind Kind, origin Origin, e *rawEntry, line int, text string) Event {
	ev := Event{Kind: kind, Origin: origin, Line: line, UUID: e.UUID, Timestamp: e.Timestamp, Bytes: len(text)}
	if p.opts.KeepContent {
		ev.Text = text
	}
	return ev
}

func (p *turnParser) add(ev Event) { p.open.Events = append(p.open.Events, ev) }

func (p *turnParser) notePromptID(id string) {
	if id != "" && !p.open.promptIDs[id] {
		p.open.promptIDs[id] = true
		p.open.PromptIDs++
	}
}

// closeAtBoundary ends an open turn that reached another start or the end of
// the file without a marker of its own.
func (p *turnParser) closeAtBoundary(line int, eof bool) {
	if p.open == nil {
		return
	}
	switch {
	case p.open.lastStop == "end_turn":
		p.close(EndYieldedUnmarked, line)
	case eof:
		p.close(EndOpenAtEOF, line)
	default:
		p.close(EndSuperseded, line)
	}
}

func (p *turnParser) close(reason EndReason, line int) {
	t := p.open
	t.End, t.EndLine = reason, line
	t.promptIDs, t.results = nil, nil
	p.turns = append(p.turns, t)
	p.open = nil
}
