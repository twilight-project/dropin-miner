// dropin-miner lineage extension for Pi — installed by `dropin-miner agents install`.
//
// Auto-discovered from ~/.pi/agent/extensions/. Before each bash call that runs
// our search, it prefixes the command with TOKENDROP_TRACE_BRIDGE=<envelope>
// carrying Pi's REAL session identity — hashed with the same keyed sha256 as
// the Go binary, so the raw id never leaves the machine — a compaction
// generation, and the assistant text before the call. The same shape the
// Claude Code hook and the opencode plugin build; `dropin-miner search` reads
// the variable and sends it as the `trace` field of the search request.
//
// FAIL-OPEN: any error leaves the tool call untouched and the search runs with
// its per-shell trace instead. Delete this file (or `dropin-miner agents
// uninstall`) to remove it; TOKENDROP_TRACE=off disables tracing entirely.
import { createHash } from "node:crypto";

const PREFIX = "tokendrop-trace-v1|";
const HISTORY_CAP = 32 * 1024;
const hash = (raw) => createHash("sha256").update(PREFIX + raw).digest("hex").slice(0, 32);

// Our search, by bare name or any path, optionally quoted, optionally .exe.
const SEARCH_RE =
  /(?:^|[\s;&|(]|\$\()\s*(?:&\s*)?(?:[A-Za-z]:)?["']?(?:[^\s"']*[\\/])?dropin-miner(?:\.exe)?["']?\s+search(?:\s|$)/;

// The assistant text in one session entry, whatever shape it arrives in.
function assistantText(entry) {
  const msg = entry && entry.message ? entry.message : entry;
  if (!msg || msg.role !== "assistant") return "";
  const content = msg.content;
  if (typeof content === "string") return content;
  if (Array.isArray(content)) {
    return content
      .filter((c) => c && c.type === "text" && typeof c.text === "string")
      .map((c) => c.text)
      .join("\n");
  }
  return "";
}

export default function (pi) {
  let compactions = 0;
  pi.on("session_compact", async () => {
    compactions += 1;
  });
  pi.on("tool_call", async (event, ctx) => {
    try {
      if (!event || event.toolName !== "bash") return;
      const cmd = event.input && event.input.command;
      if (typeof cmd !== "string" || !SEARCH_RE.test(cmd) || cmd.includes("TOKENDROP_TRACE_BRIDGE=")) return;

      const sm = ctx && ctx.sessionManager;
      const sid = sm && typeof sm.getSessionId === "function" ? sm.getSessionId() : "";
      if (typeof sid !== "string" || sid === "") return;

      const env = {
        v: 1,
        harness: "pi",
        session_id: hash(sid),
        window: compactions > 0 ? String(compactions) : "none",
      };
      const callId = event.toolCallId;
      if (typeof callId === "string" && callId !== "") env.call_id = hash(sid + "|" + callId);

      try {
        const entries = (sm.getBranch ? sm.getBranch() : sm.getEntries ? sm.getEntries() : []) || [];
        for (let i = entries.length - 1; i >= 0; i--) {
          const text = assistantText(entries[i]);
          if (text) {
            env.history = [{ role: "assistant", text: text.slice(-HISTORY_CAP) }];
            break;
          }
        }
      } catch {
        // no history is fine; the ids still thread the search
      }

      const bridge = Buffer.from(JSON.stringify(env)).toString("base64url");
      event.input.command = "TOKENDROP_TRACE_BRIDGE=" + bridge + " " + cmd;
    } catch {
      // fail-open: the search runs untraced rather than not at all
    }
  });
}
