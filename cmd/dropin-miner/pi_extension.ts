// dropin-miner lineage extension for Pi — installed by `dropin-miner agents install`.
//
// Auto-discovered from ~/.pi/agent/extensions/ (loader.ts discovers *.ts and
// *.js there; the module default-exports a function taking Pi's ExtensionAPI).
// Before each bash call that runs our search, it prefixes the command with
// TOKENDROP_TRACE_BRIDGE=<envelope> carrying Pi's REAL session identity —
// hashed with the same domain-separated SHA-256 as the Go binary, so the raw
// id never leaves the machine — the compaction generation of this session
// branch, and the assistant text that emitted THIS tool call. The same shape
// the Claude Code hook and the opencode plugin build; `dropin-miner search`
// reads the variable and sends it as the `trace` field of the search request.
//
// The scrub, the caps and the recognizer below the marker are spliced in
// from cmd/dropin-miner/agent_trace_common.js at install time — one
// repository source of truth shared with opencode. Everything the bridge
// carries is redacted and capped HERE, before the envelope is built and
// before the command is touched: the bridge is a process argument, and an
// argument is visible in `ps` the moment the command runs. Handing raw
// assistant text to the binary "to be scrubbed later" would be a disclosure
// that has already happened.
//
// OBSERVER, NEVER AN AUTHORITY. Pi lets a tool_call handler return
// {block: true} to refuse a call, and lets a THROWN error do the same
// (agent-session.ts rethrows as "Extension failed, blocking execution";
// emitToolCall has no try/catch of its own). We must do neither: this
// extension decides whether to attach a trace to a command Pi has already
// decided to run, and recognizing our own search command is not permission
// to run it. So the handler body is wrapped whole and returns nothing on
// every path, including every failure.
//
// FAIL-OPEN: any error leaves the tool call untouched and the search runs
// with its per-shell trace instead. Delete this file (or `dropin-miner agents
// uninstall`) to remove it; TOKENDROP_TRACE=off disables tracing entirely.

// {{TRACE_COMMON}}

// The text parts of one Pi session entry, in the shape the shared
// preparation takes. An assistant message's content is always an array of
// parts (packages/ai/src/types.ts: AssistantMessage.content is
// (TextContent | ThinkingContent | ToolCall)[]), and only `text` parts are
// visible assistant prose — thinking and toolCall parts are not sent.
const assistantTextParts = (entry) => {
  const content = entry?.message?.content
  if (!Array.isArray(content)) return []
  return content
    .filter((c) => c?.type === "text" && typeof c.text === "string" && c.text !== "")
    .map((c) => ({ type: "text", text: c.text }))
}

// A real user turn: a message entry whose role is "user". Pi's user content
// may be a plain string OR an array of parts (UserMessage.content is
// `string | (TextContent | ImageContent)[]`), so this tests the role only.
const isUserTurn = (entry) => entry?.type === "message" && entry?.message?.role === "user"

// Does this entry's assistant message emit the tool call we were handed? A
// tool call inside an assistant message is a content part `{type: "toolCall",
// id, name, arguments}` — the part's field is `id`, and it carries the same
// value as the event's `toolCallId` (agent-session.ts passes toolCall.id as
// toolCallId). Pi drains the assistant message into the session BEFORE
// tool_call fires, so this entry is guaranteed to be on the branch already.
const emitsToolCall = (entry, toolCallId) => {
  const content = entry?.message?.content
  if (entry?.type !== "message" || entry?.message?.role !== "assistant" || !Array.isArray(content)) return false
  return content.some((c) => c?.type === "toolCall" && c.id === toolCallId)
}

// historyForToolCall returns the assistant prose that led to THIS tool call,
// already scrubbed and capped — or "" when the relationship cannot be proven.
//
// "Latest assistant text" is the wrong question and would be wrong in two
// ordinary cases: a search made first in a new user turn would inherit the
// PREVIOUS turn's prose, and a tool call replayed from an older branch would
// pick up whatever is newest instead of its own. So the scan is anchored at
// both ends — capped at the assistant message that actually emits this
// toolCallId, floored at the user message that opened that turn — and any
// failure to establish either anchor sends no history at all rather than a
// plausible guess.
const historyForToolCall = (entries, toolCallId) => {
  if (!Array.isArray(entries) || typeof toolCallId !== "string" || toolCallId === "") return ""
  let cutoff = -1
  for (let i = entries.length - 1; i >= 0; i--) {
    if (emitsToolCall(entries[i], toolCallId)) {
      cutoff = i
      break
    }
  }
  if (cutoff < 0) return "" // no provable owner for this call: no history
  let floor = -1
  for (let i = cutoff - 1; i >= 0; i--) {
    if (isUserTurn(entries[i])) {
      floor = i
      break
    }
  }
  if (floor < 0) return "" // no turn boundary in view: never cross one blind
  for (let i = cutoff; i > floor; i--) {
    const parts = assistantTextParts(entries[i])
    if (parts.length === 0) continue
    const text = prepareTraceHistory(parts)
    if (text === null) return "" // over the complete-source budget: omit whole
    if (text) return text
  }
  return ""
}

// The context window's generation, reconstructed from what Pi persisted
// rather than counted in this process. A compaction is a first-class,
// append-only session entry (`{type: "compaction", summary,
// firstKeptEntryId, …}`, session-manager.ts appendCompaction), so counting
// them on the CURRENT BRANCH is correct across every case a process-global
// counter gets wrong: a resumed session (the entries are read back from the
// session file), a restarted extension, several sessions at once (each has
// its own manager), and tree navigation or a fork (getBranch walks parent
// pointers from the current leaf, so a branch that does not contain a
// compaction does not count it).
const windowGeneration = (entries) => {
  if (!Array.isArray(entries)) return "none"
  const n = entries.filter((e) => e?.type === "compaction").length
  return n > 0 ? String(n) : "none"
}

export default function (pi) {
  pi.on("tool_call", async (event, ctx) => {
    try {
      if (!event || event.toolName !== "bash") return
      const cmd = event.input && event.input.command
      if (!needsTraceBridge(cmd)) return

      const sm = ctx && ctx.sessionManager
      const sid = sm && typeof sm.getSessionId === "function" ? sm.getSessionId() : ""
      if (typeof sid !== "string" || sid === "") return

      // One read of the branch serves both the window generation and the
      // history: both are properties of THIS session branch as persisted.
      let entries = []
      try {
        entries = sm.getBranch() || []
      } catch {
        entries = [] // malformed or unavailable host state: ids still thread
      }

      const env = { v: 1, harness: "pi", session_id: traceHash(sid), window: windowGeneration(entries) }
      const callId = event.toolCallId
      if (typeof callId === "string" && callId !== "") env.call_id = traceHash(sid + "|" + callId)
      // No turn_id: Pi has no turn identifier an extension can read. The
      // turnIndex on turn_start/turn_end is an in-memory counter reset per
      // agent run and never persisted, so there is nothing stable to hash —
      // and inventing one would make two unrelated searches look related.

      const text = historyForToolCall(entries, callId)
      if (text) env.history = [{ role: "assistant", text }]

      const bridge = traceBridge(env)
      if (bridge === null) return // oversized even without history: no bridge
      event.input.command = TRACE_BRIDGE_ENV + "=" + bridge + " " + cmd
    } catch {
      // fail-open: the search runs untraced rather than not at all. Nothing
      // is returned on any path — a truthy return is how a Pi extension
      // blocks a call, and this one is never a permission authority.
    }
  })
}
