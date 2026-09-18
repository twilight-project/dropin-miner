package trajectory

// The declared shapes. Everything the reader understands is named in this
// file; anything absent from it is refused. The lists were taken from a
// structure-only survey of Claude Code 2.1.226 through 2.1.276 transcripts
// (key names, type names and counts — no values), and an entry here means
// "its role in a turn is understood", not merely "it has been seen".

// Origin says whose words or data an event carries, so a corpus can later be
// divided wherever counsel draws a line without re-reading anything.
type Origin string

const (
	// OriginParticipant is what the person at the keyboard typed or chose.
	OriginParticipant Origin = "participant"
	// OriginHostModel is what the host's model wrote: text, thinking, the
	// parameters of a tool call, a compaction summary.
	OriginHostModel Origin = "host_model"
	// OriginToolResult is what some other tool returned: file contents,
	// command output, a fetched page. Third-party material lives here.
	OriginToolResult Origin = "tool_result"
	// OriginClient is this client's own output when no router answer is in
	// it: a usage error, a refusal, a human-format rendering.
	OriginClient Origin = "client"
	// OriginRouter is a search result the router answered, identified by the
	// request id inside it.
	OriginRouter Origin = "router"
	// OriginHost is the sixth class, which the phase-1 brief does not name
	// and the survey showed is needed: entries the host itself injects — a
	// background-task notification that starts a turn, a slash-command
	// echo, a system reminder, an attachment. They are neither the
	// participant's words nor the model's, and filing them under either
	// would put a line in the wrong place for whoever divides the corpus.
	OriginHost Origin = "host"
)

// Kind is the type of one event inside a turn.
type Kind string

const (
	KindPrompt         Kind = "prompt"          // the turn-starting entry
	KindAssistantText  Kind = "assistant_text"  // model prose
	KindThinking       Kind = "thinking"        // model reasoning block
	KindToolCall       Kind = "tool_call"       // any tool call that is not our search
	KindToolResult     Kind = "tool_result"     // the result of such a call
	KindSearchCall     Kind = "search_call"     // a call that parses to this client's search
	KindSearchResult   Kind = "search_result"   // what that call returned
	KindHostNote       Kind = "host_note"       // host-injected meta entry or attachment inside a turn
	KindCompaction     Kind = "compaction"      // the host compacted the context mid-turn
	KindCompactSummary Kind = "compact_summary" // the model-written summary that follows
	KindInterrupt      Kind = "interrupt"       // the participant stopped the turn
)

// EndReason says how a turn ended. Only EndYielded is the host's own word
// for it; the others are what the reader can establish when that marker is
// absent, and each is named for the evidence, not for a guess at intent.
type EndReason string

const (
	// EndYielded: the host wrote its turn-duration marker.
	EndYielded EndReason = "yielded"
	// EndYieldedUnmarked: no marker, but the last model message stopped with
	// end_turn. Every subagent turn ends this way — the host writes no
	// turn-duration marker on a sidechain.
	EndYieldedUnmarked EndReason = "yielded_unmarked"
	// EndInterrupted: the host's interruption marker closed the turn.
	EndInterrupted EndReason = "interrupted"
	// EndSuperseded: another turn started while this one had not yielded.
	EndSuperseded EndReason = "superseded"
	// EndOpenAtEOF: the file ended first — a killed or still-running session.
	EndOpenAtEOF EndReason = "open_at_eof"
)

// RefusalReason is the closed vocabulary of things the reader declines.
type RefusalReason string

const (
	RefuseUnknownEntryType     RefusalReason = "unknown_entry_type"
	RefuseUnknownSystemSubtype RefusalReason = "unknown_system_subtype"
	RefuseUnknownBlockType     RefusalReason = "unknown_block_type"
	RefuseMalformedEntry       RefusalReason = "malformed_entry"
	RefuseUnparseableLine      RefusalReason = "unparseable_line"
	RefuseTruncatedFinalLine   RefusalReason = "truncated_final_line"
	RefuseEntryOutsideTurn     RefusalReason = "entry_outside_turn"
	RefuseUnknownLayout        RefusalReason = "unknown_layout"
	RefuseSubagentUnlinked     RefusalReason = "subagent_unlinked"
	RefuseUnreadableFile       RefusalReason = "unreadable_file"
)

// Refusal is one thing the reader declined, and where. Detail is structural
// only — a type name, never a value read from a message.
type Refusal struct {
	File      string // slash-separated, relative to the directory the caller named
	Line      int    // 1-based; 0 when the refusal is about the file itself
	Reason    RefusalReason
	EntryType string // the entry's own "type", made printable; empty when not applicable
	Detail    string
}

// Conversation entry types: the three the turn rule keys on.
const (
	entryUser      = "user"
	entryAssistant = "assistant"
	entrySystem    = "system"
	entryAttach    = "attachment"
)

// bookkeepingEntryTypes are entry types whose role is understood well enough
// to say they carry no turn structure: titles, modes, UI state, cost and
// file-history records. They are skipped knowingly and never decoded past
// their "type" — which matters for one of them: a bridge-session entry
// carries the participant's host account id, so not decoding it is also how
// that id is never read.
var bookkeepingEntryTypes = map[string]bool{
	"queue-operation": true, "atis-latch": true, "last-prompt": true, "ai-title": true,
	"agent-setting": true, "mode": true, "permission-mode": true,
	"file-history-snapshot": true, "file-history-delta": true, "bridge-session": true,
	"custom-title": true, "agent-name": true, "agent-color": true, "frame-link": true,
	"cost-state": true, "continued-in": true, "artifact-comment-monitor": true,
	"artifact-autoreact-ledger": true, "pr-link": true, "relocated": true,
	"worktree-state": true, "fork-context-ref": true,
}

// System subtypes. turn_duration is the yield marker and compact_boundary is
// the compaction marker; the rest are understood to carry no turn structure.
const (
	systemTurnDuration    = "turn_duration"
	systemCompactBoundary = "compact_boundary"
)

var inertSystemSubtypes = map[string]bool{
	"stop_hook_summary": true, "away_summary": true, "local_command": true,
	"informational": true, "model_refusal_fallback": true, "scheduled_task_fire": true,
}

// Content block types, per role.
const (
	blockText       = "text"
	blockThinking   = "thinking"
	blockToolUse    = "tool_use"
	blockToolResult = "tool_result"
	blockImage      = "image"
)

// participantPromptSources are the values of a user entry's promptSource that
// mean a person produced it. In the survey every one of them coincided with
// origin.kind "human"; "system" coincided with a task notification every time.
var participantPromptSources = map[string]bool{
	"typed": true, "suggestion_accepted": true, "queued": true, "sdk": true,
}

const promptSourceSystem = "system"

// interruptMarkerPrefix is the host's own constant for an interruption. It is
// the one place the turn rule reads message text, and it has to: of the
// interruption markers surveyed, only about half carried the structural
// interruptedMessageId key. If the host rewords it, an interrupted turn
// degrades to EndSuperseded followed by an empty turn start — counted, and
// wrong in a way the findings can see, rather than silently merged.
const interruptMarkerPrefix = "[Request interrupted"

// hostWrapperPrefixes mark a promptSource-less user entry as host-written:
// slash-command and local-command echoes and shell-escape records.
var hostWrapperPrefixes = []string{
	"<command-name>", "<command-message>", "<local-command-", "<bash-input>", "<bash-stdout>",
	"<bash-stderr>", "<task-notification>", "<system-reminder>",
}

// shellToolNames are the host tools that run a shell command line.
var shellToolNames = map[string]bool{"Bash": true, "PowerShell": true}

// agentToolNames are the host tools that spawn a subagent.
var agentToolNames = map[string]bool{"Agent": true, "Task": true}
