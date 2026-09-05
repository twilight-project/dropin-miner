# dropin-miner

Web search for coding agents that pays the person running the agent.

One binary. Drop it into Claude Code, Codex, Cursor or opencode and their web
searches go through the Twilight search router, carry the agent's
trajectory, and earn Twilight Slot rewards to an address you control. No
daemon, no proxy, no MCP server: between tool calls nothing is running.

```bash
curl -fsSL https://raw.githubusercontent.com/twilight-project/dropin-miner/main/scripts/install.sh | sh
```

Windows, in PowerShell:

```powershell
irm https://raw.githubusercontent.com/twilight-project/dropin-miner/main/scripts/install.ps1 | iex
```

The installer fetches a checksummed release, then asks its questions in
order: enrollment token, where to be paid (a wallet it makes here, or an
address you paste), the first join, your sr- API key (typed with echo off,
checked against the router, stored owner-only in
`~/.tokendrop/credentials.json`), your shell profile, and which coding agents
to set up. The key never goes into a command line or an agent's config;
`TOKENDROP_API_KEY` in the environment overrides the stored one.

## How it works

Everything happens at four moments the agent already has.

| moment | who runs it | what happens |
|---|---|---|
| install | you, once | enroll, payout address, first join, skill and hooks written per agent |
| session start | a hook | seed the context-window counter, start a flush |
| tool call | the agent | `dropin-miner search` posts to the router with your key and the trace envelope, prints results, records the served request id, starts a flush |
| session end | a hook | start a flush |

A **flush** is the mining plane as one pass: ask the AS which epoch is open,
join it if not joined, hold a participation capability, promote recorded
searches into the spool, submit once, exit. Two flushes at once queue on a
lock. A machine that never searches never runs one.

The **trace** is how the router groups one task's searches. It comes from
whichever of these the host allows: a hook that rewrites the shell command
with `TOKENDROP_TRACE_BRIDGE=<envelope>` (Claude Code, opencode), a per-workspace
lineage file the hooks write and `search` reads (Cursor, and every host as a
fallback), or a hashed per-shell identity when there is no hook at all. Every
identifier is hashed before it leaves the machine; the assistant text just
before a search travels only inside that search's request, capped at 32 KB.
`TOKENDROP_TRACE=off` sends none.

## Per host

| host | tool | lineage | files written by `agents install` |
|---|---|---|---|
| Claude Code | skill | full: PreToolUse on Bash rewrites the command; window hooks; Stop flushes | `~/.claude/skills/dropin-miner/`, five hook entries and an allow rule for the search command in `~/.claude/settings.json` |
| Cursor | skill | full: lineage file from sessionStart, thought, response, shell and compaction hooks | `~/.cursor/skills/dropin-miner/`, six entries in `~/.cursor/hooks.json` |
| Codex | skill | per-shell | `~/.codex/skills/dropin-miner/` (allow network for the command: its sandbox is offline by default) |
| opencode | AGENTS.md line | full: in-process plugin rewrites the bash command | `~/.config/opencode/plugins/dropin-miner.js` |
| anything else | rules line | per-shell | printed for you to paste |

Uninstall removes exactly those, and only hook entries that name this binary.

## Commands

```
dropin-miner search [-tier fast] [-format json|model] <query>
dropin-miner agents install|status|uninstall
dropin-miner agents prefer on|off|status
dropin-miner flush [-force]
dropin-miner login [-show | -forget | -key-env VAR]
dropin-miner enroll | payout | join | status | doctor | earnings
dropin-miner wallet init|address|register|balance|send
```

`dropin-miner help` describes each. Every command takes `-config <file>`,
falling back to `TOKENDROP_CONFIG`, then `./tokendrop.toml`.

`login` reads your sr- key from stdin (never an argument), verifies it with a
zero-spend probe against the router, and writes `~/.tokendrop/credentials.json`
as `0600`. A search takes its key from `TOKENDROP_API_KEY` if set, else that
file (refused if it is a symlink or readable by others), else `OPENAI_API_KEY`.

`agents prefer off` makes the agent's own web search the default and keeps
this one for when you name it; `on` makes this one the default again. Inside
the agent, `/dropin-miner off` and `/dropin-miner on` do the same. The choice
is one file beside the config, the installed skills are rewritten from it,
and a reinstall keeps it. While it is off, searches you do not route here
earn nothing.

## Config

```toml
[[provider]]
name     = "search-router"
upstream = "https://router-api.nyks.dev"

[mining]
enabled   = true
as_url    = "https://minis.nyks.dev"
chain_id  = "twilight-devnet-3"
slot_id   = 3
state_dir = "/home/you/.tokendrop/state"
spool_dir = "/home/you/.tokendrop/spool"

[miner]
enabled        = true
intake_dir     = "/home/you/.tokendrop/intake"     # served request ids, until flushed
sessions_dir   = "/home/you/.tokendrop/sessions"   # per-workspace lineage files
flush_interval = "3m"                             # how often a flush re-asks the AS
# router_url = "..."   defaults to the provider upstream
```

The `[mining]` block is the proxy's, unchanged: a machine that already runs
`tokendrop-proxy` can point this at the same state directory and be the same
participant.

## Building

```bash
make build      # bin/dropin-miner
make verify     # test, race, vet (incl. Windows), cross-compile
```

Go 1.25 or newer. The participant packages under `pkg/` are copied from
`tokendrop-proxy` with their golden vectors; see `pkg/README.md`.

## Two things worth knowing

The query rides in process arguments, so it is visible in `ps` and shell
history on your own machine. It is not a credential; the key is read from the
environment or the owner-only credentials file and never put in a command line.

Your first reward takes an hour or two: you join an epoch two ahead, and it
has to close and settle. One verified search per epoch makes you eligible,
and the pot splits equally among everyone eligible.

## Removing it, and coming back

```
dropin-miner agents uninstall     # the skills, hooks and plugin, nothing else
rm ~/.tokendrop/bin/dropin-miner  # the binary
```

Nothing we ship deletes `~/.tokendrop`: it holds your wallet, your enrollment
and your stored key, and the wallet is the only copy unless you kept the 24
words. Leave it, or set it aside as `~/.tokendrop.bak-<date>`. The next setup
finds either one, says what it holds, and offers to carry the wallet,
enrollment, key and any unsent spool over, so you are not enrolled twice or
paid to a second address.

## License

Apache-2.0.
