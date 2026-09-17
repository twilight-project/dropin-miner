// dropin-miner lineage plugin for opencode — installed by `dropin-miner agents install`.
//
// Runs INSIDE opencode's own runtime (nothing extra to install). Before each
// bash call that runs our search, it prefixes the command with
// TOKENDROP_TRACE_BRIDGE=<envelope> carrying opencode's REAL session
// identity — hashed with the same domain-separated SHA-256 as the Go binary, so the raw
// id never leaves the machine — plus a compaction generation and the
// assistant text before the call. The same shape the Claude Code hook
// builds; `dropin-miner search` reads the variable and sends it as the
// `trace` field of /v1/search.
//
// The scrub, the caps and the recognizer below the marker are spliced in
// from cmd/dropin-miner/agent_trace_common.js at install time — one
// repository source of truth shared with every other JavaScript host. This
// file owns only what is opencode-specific: which events carry the session,
// where its messages come from, and how a tool call is rewritten.
//
// FAIL-OPEN: any error leaves the tool call untouched and the search runs
// with its per-shell trace instead. Delete this file (or `dropin-miner agents
// uninstall`) to remove it; TOKENDROP_TRACE=off disables tracing entirely.

// {{TRACE_COMMON}}

// The shell opencode runs its bash tool in on THIS machine, written in by
// the installer from the host's declaration. The bridge is an environment
// assignment, and its syntax is the shell's: a POSIX prefix handed to
// PowerShell is looked up as a program name and the search never runs (#68).
const HOST_SHELL = "{{HOST_SHELL}}"

// The installation this file belongs to: the config `dropin-miner agents
// install` resolved when it wrote this. Nothing here reads it — the plugin
// runs no command of ours and needs no config — it is how `uninstall` knows
// whose file this is. Every other artifact names its installation in a
// command it teaches; this one teaches none, so before this line existed an
// uninstall had nothing to match and removed another installation's plugin
// (#73). Two installations share this one path, so it names the one that ran
// `agents install` last, and the other leaves it alone.
const INSTALL_CONFIG = "{{INSTALL_CONFIG}}"

export const DropinMinerLineage = async ({ client }) => {
  // sessionID -> how many times this session's context window has compacted.
  const compactions = new Map()
  return {
    event: async ({ event }) => {
      try {
        if (event?.type === "session.compacted") {
          const sid = event?.properties?.sessionID ?? event?.properties?.info?.id
          if (typeof sid === "string" && sid) compactions.set(sid, (compactions.get(sid) ?? 0) + 1)
        }
      } catch {
        // fail-open
      }
    },
    "tool.execute.before": async (input, output) => {
      try {
        if (!input || input.tool !== "bash") return
        const cmd = output?.args?.command
        if (!needsTraceBridge(cmd)) return
        const sid = input.sessionID
        if (typeof sid !== "string" || sid === "") return
        const gen = compactions.get(sid) ?? 0
        const env = { v: 1, harness: "opencode", session_id: traceHash(sid), window: gen > 0 ? String(gen) : "none" }
        if (typeof input.callID === "string" && input.callID !== "") env.call_id = traceHash(sid + "|" + input.callID)
        // The assistant text before this call, from the session's messages.
        try {
          const messages = (await client.session.messages({ path: { id: sid } }))?.data ?? []
          for (let i = messages.length - 1; i >= 0; i--) {
            const m = messages[i]
            if (m?.info?.role !== "assistant") continue
            const text = prepareTraceHistory(m.parts ?? [])
            if (text === null) break
            if (text) {
              env.history = [{ role: "assistant", text }]
              break
            }
          }
        } catch {
          // no history is fine; the ids still thread the search
        }
        const bridge = traceBridge(env)
        if (bridge === null) return
        // H-R4: a bridge already on the command is not ours until we put it
        // there. Every assignment we can prove standalone is removed and
        // this call's own is prepended; a command carrying one we cannot
        // remove with certainty is left exactly as it is.
        const rewritten = withTraceBridge(cmd, bridge, HOST_SHELL)
        if (rewritten === null) return
        output.args.command = rewritten
      } catch {
        // fail-open: the search runs untraced rather than not at all
      }
    },
  }
}
