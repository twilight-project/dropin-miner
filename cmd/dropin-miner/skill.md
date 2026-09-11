---
name: dropin-miner
description: "{{DESCRIPTION}}"
---

# Web search (dropin-miner)

Search the public web by running:

```bash
{{SEARCH}} "<your query>"
```

It prints one block per provider, the chosen one first: an answer when a provider
gave one, then numbered results with URL, title and snippet. Add `-tier fast` before
the query only when a specific tier is called for. Add `-format json` instead of
`-format model` when you need the router's full JSON.

Recent agent context may accompany a search to the Twilight search router as
part of the trajectory/search product. `TOKENDROP_TRACE=off` disables trace
transmission. Mining/AS receives metadata observations only.

## On and off

If the argument is exactly `on`, `off` or `status`, it is not a query. Run

```bash
{{PREFER}} <argument>
```

show its output, and follow the new setting for the rest of this session. `off`
makes the agent's built-in web search the default and keeps this one for when the
user names it; `on` makes this one the default again. The setting is the user's;
never change it on your own.

## Rules

{{PREFER_RULES}}
- Do not search when the answer is already known and not time-sensitive, or when the
  data is private to this machine or project.
- HTTP 401, or an exit saying `no API key`, means this installation has not
  registered yet. Tell the user to run `dropin-miner connect` — it registers
  and stores a key itself, and search works right after, before any claim.
  If it prints a claim URL, show it to the user exactly as printed: never
  open it yourself, and never shorten, paraphrase, or summarize it away.
  Visiting it is on the user's own time — nothing here blocks on it, and a
  later search picks the claim up automatically once they do. `dropin-miner
  status` says what has been claimed and whether mining is enrolled. Never
  put a key in a command line or a file yourself. Do not retry the search
  until the user says `connect` is done.
- Do not silently fall back to another search tool on an error; the user earns
  through this one. Report the error and let them decide.
