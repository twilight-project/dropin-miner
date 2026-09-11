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
// FAIL-OPEN: any error leaves the tool call untouched and the search runs
// with its per-shell trace instead. Delete this file (or `dropin-miner agents
// uninstall`) to remove it; TOKENDROP_TRACE=off disables tracing entirely.
import { createHash } from "node:crypto"

const PREFIX = "tokendrop-trace-v1|"
const HISTORY_CAP = 32 * 1024
const SOURCE_CAP = 256 * 1024
const ENVELOPE_CAP = 48 * 1024

// Mirror pkg/redact.TraceText for complete source entries before the bridge
// is capped. The Go consumer applies its own preparation again.
// Start URL/email scans at token boundaries to avoid rescanning long words.
const scrub = (text) => text
  .replace(/(?<![a-zA-Z0-9+.-])([a-zA-Z0-9+.-]*:\/\/)[^/@\s]+@/g, (match, prefix) =>
    /[a-zA-Z]/.test(prefix) ? prefix + '[REDACTED]' : match)
  .replace(/\b(?:sk|sr)-[A-Za-z0-9_-]{16,}/g, '[REDACTED]')
  .replace(/\bgh[opsur]_[A-Za-z0-9]{20,}\b|\bgithub_pat_[A-Za-z0-9_]{20,}\b/g, '[REDACTED]')
  .replace(/\bAKIA[0-9A-Z]{16}\b/g, '[REDACTED]')
  .replace(/\beyJ[A-Za-z0-9_-]{4,}\.[A-Za-z0-9_-]{4,}\.[A-Za-z0-9_-]{4,}\b/g, '[REDACTED]')
  .replace(/(?<![A-Za-z0-9._%+-])([.%+-]*)([A-Za-z0-9_][A-Za-z0-9._%+-]*@[A-Za-z0-9.-]+\.[A-Za-z]{2,}\b)/g, (match, leading, email, offset, source) =>
    source[offset + match.length] === ':' || /(?:ssh|scp|rsync|sftp)$/i.test(source.slice(0, offset + leading.length).replace(/[ \t]+$/, '')) ? match : leading + '[REDACTED]')
  .replace(/([A-Z]:\\Users\\)[^\\\s]+/gi, '$1[REDACTED]')
  .replace(/(\/Users\/|\/home\/)[^/\s]+/g, (match, prefix, offset, source) =>
    offset > 0 && /[A-Za-z0-9.]/.test(source[offset - 1]) ? match : prefix + '[REDACTED]')

const prepare = (parts) => {
  const texts = []
  let size = 0
  for (const part of parts) {
    if (part?.type !== 'text' || typeof part.text !== 'string') continue
    size += Buffer.byteLength(part.text) + (texts.length ? 1 : 0)
    if (size > SOURCE_CAP) return null
    texts.push(part.text)
  }
  const bytes = Buffer.from(scrub(texts.join('\n')))
  let start = Math.max(0, bytes.length - HISTORY_CAP)
  while (start < bytes.length && (bytes[start] & 0xc0) === 0x80) start++
  return bytes.subarray(start).toString('utf8')
}
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
            const text = prepare(m.parts ?? [])
            if (text === null) break
            if (text) {
              env.history = [{ role: "assistant", text }]
              break
            }
          }
        } catch {
          // no history is fine; the ids still thread the search
        }
        if (Buffer.byteLength(JSON.stringify(env)) > ENVELOPE_CAP) delete env.history
        if (Buffer.byteLength(JSON.stringify(env)) > ENVELOPE_CAP) return
        const bridge = Buffer.from(JSON.stringify(env)).toString("base64url")
        output.args.command = "TOKENDROP_TRACE_BRIDGE=" + bridge + " " + cmd
      } catch {
        // fail-open: the search runs untraced rather than not at all
      }
    },
  }
}
