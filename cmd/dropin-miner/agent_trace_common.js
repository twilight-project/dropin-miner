// dropin-miner: shared trace preparation for the JavaScript hosts.
//
// ONE source of truth. Every host whose lineage channel is a JavaScript or
// TypeScript artifact we install — opencode's in-process plugin, Pi's
// auto-discovered extension — is RENDERED from its own template with this
// file spliced in where that template carries the trace-common marker
// (renderAgentScript in agents.go). The installed files stay standalone,
// with no import of ours to resolve at runtime and no package manager in
// the picture, but the scrub and the caps below exist exactly once in the
// repository.
//
// That is the point. A second, weaker copy of this logic in a second
// adapter is the failure this file exists to prevent: the adapter is what
// builds the bridge, so an adapter that scrubs late — or not at all — puts
// raw assistant text into a process argument before the Go binary ever
// sees it. The order here is not negotiable:
//
//	complete bounded source -> scrub -> UTF-8 byte accounting ->
//	32 KiB history tail -> envelope -> 48 KiB envelope check -> base64url
//
// Never truncate before scrubbing: a slice can cut a secret in half and
// leave both halves unrecognizable to the redactor. An entry too large to
// scrub whole is omitted whole instead.
//
// Everything here fails open. A host adapter that throws leaves the tool
// call exactly as it found it and the search runs with its per-shell
// trace, which is the whole fallback contract: tracing must never be the
// reason a search does not run.
import { createHash } from "node:crypto"

// The trace hash: domain-separated SHA-256 over a PUBLIC prefix, truncated
// to 16 bytes and hex-encoded — the same derivation as traceHash in
// trace.go, so a host id hashed here and one hashed in Go agree. It is not
// keyed and is not a secret: it exists so a stable identifier can leave the
// machine without the host's own id leaving with it.
const TRACE_PREFIX = "tokendrop-trace-v1|"
// The visible-text cap: what may ride as history, after scrubbing.
const TRACE_HISTORY_CAP = 32 * 1024
// The complete-source budget: an entry larger than this is omitted whole
// rather than sliced, so redaction always sees a complete entry. Matches
// hookTailBytes in hook.go, which prepareTraceText applies on the Go side.
const TRACE_SOURCE_CAP = 256 * 1024
// The whole-envelope cap (compact JSON). Above it history is dropped
// first; an envelope still above it is dropped whole. A trace must never
// be the reason a search fails, and must never push a valid search
// invocation past the host's command-length limit.
const TRACE_ENVELOPE_CAP = 48 * 1024

// Our search command, by bare name or any path, optionally quoted,
// optionally .exe. Pinned by test to searchCommandRe in hook.go — the one
// recognizer every host goes through — so no adapter can quietly loosen
// into a substring match. This is lineage-injection detection: it decides
// whether we thread a trace onto a command the host has ALREADY decided to
// run. It is not an authorization decision and must never become one.
const SEARCH_RE = /(?:^|[\s;&|(]|\$\()\s*(?:&\s*)?(?:[A-Za-z]:)?["']?(?:[^\s"']*[\\/])?dropin-miner(?:\.exe)?["']?\s+search(?:\s|$)/

const TRACE_BRIDGE_ENV = "TOKENDROP_TRACE_BRIDGE"

const traceHash = (raw) => createHash("sha256").update(TRACE_PREFIX + raw).digest("hex").slice(0, 32)

// Mirror pkg/redact.TraceText for complete source entries before the bridge
// is capped. The Go consumer applies its own preparation again.
// Start URL/email scans at token boundaries to avoid rescanning long words.
const scrubTraceText = (text) => text
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

// prepareTraceHistory takes the COMPLETE text parts of one assistant
// message — `{type: 'text', text}` entries, the shape opencode's message
// parts already have and the shape every other adapter converts to — and
// returns the scrubbed, capped tail that may ride as history.
//
// Returns null when the complete source is over budget: the entry is
// omitted whole, never sliced to fit, because slicing would hand the
// scrubber a severed secret. Returns "" when there is nothing to send.
const prepareTraceHistory = (parts) => {
  const texts = []
  let size = 0
  for (const part of parts) {
    if (part?.type !== 'text' || typeof part.text !== 'string') continue
    size += Buffer.byteLength(part.text) + (texts.length ? 1 : 0)
    if (size > TRACE_SOURCE_CAP) return null
    texts.push(part.text)
  }
  const bytes = Buffer.from(scrubTraceText(texts.join('\n')))
  // The tail, not the head: the words nearest the search are the ones that
  // explain it. Walk forward off any continuation byte so the cut lands on
  // a rune boundary and the result is still valid UTF-8.
  let start = Math.max(0, bytes.length - TRACE_HISTORY_CAP)
  while (start < bytes.length && (bytes[start] & 0xc0) === 0x80) start++
  return bytes.subarray(start).toString('utf8')
}

// traceBridge serializes the envelope and enforces the envelope cap:
// history is dropped first, and an envelope still too large yields null —
// no bridge at all, rather than a command the host cannot run. Returns the
// base64url bridge value.
const traceBridge = (env) => {
  if (Buffer.byteLength(JSON.stringify(env)) > TRACE_ENVELOPE_CAP) delete env.history
  if (Buffer.byteLength(JSON.stringify(env)) > TRACE_ENVELOPE_CAP) return null
  return Buffer.from(JSON.stringify(env)).toString("base64url")
}

// needsTraceBridge: our search command, not already carrying a bridge.
// Recognition only — the host has already decided to run this command.
const needsTraceBridge = (cmd) =>
  typeof cmd === "string" && SEARCH_RE.test(cmd) && !cmd.includes(TRACE_BRIDGE_ENV + "=")
