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

The installer fetches a checksummed release, writes the config, then hands
off to `connect`: it registers with the search platform, stores the key it
mints (nothing to copy, nothing to paste), asks whether to enable mining at
whichever terminal is present, creates or takes a wallet, and prints a claim
link. Then it asks about your shell profile and which coding agents to set
up. Search itself works before you ever visit that link — the claim only
gates the reward, once you say yes to mining. The key never goes into a
command line or an agent's config; `TOKENDROP_API_KEY` in the environment
overrides the stored one.

## How it works

Everything happens at four moments the agent already has.

| moment | who runs it | what happens |
|---|---|---|
| install | you, once | mining question, wallet or address, connect registers and stores the key, skill and hooks written per agent, claim link printed |
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
Recent agent context accompanies search to the Twilight search router as part
of the trajectory/search product. `TOKENDROP_TRACE=off` disables trace
transmission. Mining/AS receives metadata observations only.

## Per host

| host | tool | lineage | files written by `agents install` |
|---|---|---|---|
| Claude Code | skill | full: PreToolUse on Bash rewrites the command; window hooks; Stop flushes | `~/.claude/skills/dropin-miner/`, five hook entries and an allow rule for the search command in `~/.claude/settings.json` |
| Cursor | skill | full: lineage file from sessionStart, thought, response, shell and compaction hooks | `~/.cursor/skills/dropin-miner/`, six entries in `~/.cursor/hooks.json` |
| Codex | skill | per-shell | `~/.codex/skills/dropin-miner/`; install also widens `~/.codex/config.toml`'s sandbox (network, plus the four tokendrop directories made writable — never the config, key or wallet) so searches record and the claim resumes |
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
dropin-miner connect [-name ...]
dropin-miner mining enable | disable
```

`connect` and `mining enable` are the search platform's agent-onboarding path —
register, get claimed at a printed URL, then mine unattended. `setup.sh` and
`install.ps1` both run `connect` themselves now; run it directly yourself for
a second agent, a re-run, or a scripted install (see Config below). `mining
disable` stops mining for this installation's agent — a best-effort
self-service revocation at the AS, distinct from the platform's own granted
scope, which only a human at the console can revoke; `status` says so plainly
whenever this state holds. `enroll`, `login`, `join`, `wallet register` and
`payout set` are the portal's older, manual path — still work, coexist with
`connect`, and are not part of what the installers run.

`dropin-miner help` describes each. Every command takes `-config <file>`,
falling back to `TOKENDROP_CONFIG`, then `./tokendrop.toml`.

`login` reads your sr- key from stdin (never an argument), verifies it with a
zero-spend probe against the router, and writes `~/.tokendrop/credentials.json`
as `0600`. A search takes its key from `TOKENDROP_API_KEY` if set, else that
file (refused if it is a symlink or readable by others). Otherwise, run
`dropin-miner connect` or `dropin-miner login` to set up a search credential.

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
enabled        = true
as_url         = "https://rewards.nyks.dev"
chain_id       = "twilight-testnet-1"
slot_id        = 3
state_dir      = "/home/you/.tokendrop/state"
spool_dir      = "/home/you/.tokendrop/spool"
# payout_address = "twilight1..."  scripted `connect`/`mining enable` answer;
#                                  leave unset to be asked at a terminal instead
# platform_slot  = "twilight-slot-3"  required only if the platform ever offers
#                                     more than one mining slot to enroll into

[platform]
base_url       = "https://platform.nyks.dev"    # the human portal and claim pages
agents_api_url = "https://agents-v1.nyks.dev"    # register/status/enroll — a separate host

[miner]
enabled        = true
intake_dir     = "/home/you/.tokendrop/intake"     # served request ids, until flushed
sessions_dir   = "/home/you/.tokendrop/sessions"   # per-workspace lineage files
flush_interval = "3m"                             # how often a flush re-asks the AS
# router_url = "..."   defaults to the provider upstream
```

The `[mining]` block is the proxy's, unchanged: a machine that already runs
`tokendrop-proxy` can point this at the same state directory and be the same
participant. `[platform]` and the two extra `[mining]` keys above are
`connect`/`mining enable`'s own (agent onboarding design, §5.5) — see
`dropin-miner connect -h` / `dropin-miner mining -h`. The two `[platform]`
URLs are genuinely different hosts, not a redundant pair: `base_url` is
only ever compared against, never dialed (it is where a printed claim
URL must point); `agents_api_url` is what `connect`/`mining enable`
actually send requests to.

`[miner] enabled` means only that router intake is configured — it is not
the mining on/off switch. That decision lives in one place: whatever
`connect`'s first run, `mining enable`, or `mining disable` last decided,
persisted to the state directory and read by search intake, the flush,
connect, its resume, and `status` alike. Editing `[mining] enabled` by hand
after the fact does nothing on its own; run `mining enable`/`mining disable`
instead.

## Building

```bash
make build      # bin/dropin-miner
make verify     # build, test, race, vet (incl. Windows), lint, vuln, tidy, cross-compile
```

Go 1.25 or newer; tests require a modern Node.js runtime capable of executing
the embedded opencode plugin. CI uses Node.js 22. The participant packages under `pkg/` are copied from
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
