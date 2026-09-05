// dropin-miner lineage plugin for opencode — installed by `dropin-miner agents install`.
//
// Runs INSIDE opencode's own runtime (nothing extra to install). Before each
// bash call that runs our search, it prefixes the command with
// TOKENDROP_TRACE_BRIDGE=<envelope> carrying opencode's REAL session
// identity — hashed with the same keyed sha256 as the Go binary, so the raw
// id never leaves the machine — plus a compaction generation and the
// assistant text before the call. The same shape the Claude Code hook
// builds; `dropin-miner search` reads the variable and sends it as the
// `trace` field of /v1/search.
//
// FAIL-OPEN: any error leaves the tool call untouched and the search runs
// with its per-shell trace instead. Delete this file (or `dropin-miner agents
// uninstall`) to remove it; TOKENDROP_TRACE=off disables tracing entirely.
import { createHash } from "node:crypto"

const PREFIX = "tokendrop-trace-v1|"
const HISTORY_CAP = 32 * 1024
const hash = (raw) => createHash("sha256").update(PREFIX + raw).digest("hex").slice(0, 32)

// Our search, by bare name or any path, optionally quoted, optionally .exe.
const SEARCH_RE = /(?:^|[\s;&|(]|\$\()\s*(?:&\s*)?(?:[A-Za-z]:)?["']?(?:[^\s"']*[\\/])?dropin-miner(?:\.exe)?["']?\s+search(?:\s|$)/

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
        if (typeof cmd !== "string" || !SEARCH_RE.test(cmd) || cmd.includes("TOKENDROP_TRACE_BRIDGE=")) return
        const sid = input.sessionID
        if (typeof sid !== "string" || sid === "") return
        const gen = compactions.get(sid) ?? 0
        const env = { v: 1, harness: "opencode", session_id: hash(sid), window: gen > 0 ? String(gen) : "none" }
        if (typeof input.callID === "string" && input.callID !== "") env.call_id = hash(sid + "|" + input.callID)
        // The assistant text before this call, from the session's messages.
        try {
          const messages = (await client.session.messages({ path: { id: sid } }))?.data ?? []
          for (let i = messages.length - 1; i >= 0; i--) {
            const m = messages[i]
            if (m?.info?.role !== "assistant") continue
            const text = (m.parts ?? []).filter((p) => p?.type === "text" && typeof p.text === "string").map((p) => p.text).join("\n")
            if (text) {
              env.history = [{ role: "assistant", text: text.slice(-HISTORY_CAP) }]
              break
            }
          }
        } catch {
          // no history is fine; the ids still thread the search
        }
        const bridge = Buffer.from(JSON.stringify(env)).toString("base64url")
        output.args.command = "TOKENDROP_TRACE_BRIDGE=" + bridge + " " + cmd
      } catch {
        // fail-open: the search runs untraced rather than not at all
      }
    },
  }
}
