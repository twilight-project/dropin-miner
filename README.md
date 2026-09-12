# dropin-miner

Web search for coding agents that pays the person running the agent.

One binary. Drop it into Claude Code, Codex, Cursor, opencode, Pi or Hermes and their web
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
whichever of these the host allows: a hook, plugin or extension that rewrites
the shell command with `TOKENDROP_TRACE_BRIDGE=<envelope>` (Claude Code,
opencode, Pi, Hermes), a per-workspace
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
| Pi | skill | full: an auto-discovered extension rewrites the bash command; history is bound to the tool call that asked for it, and the window generation is read back from the session's own compaction entries | `~/.pi/agent/skills/dropin-miner/`, `~/.pi/agent/extensions/dropin-miner.ts` |
| Hermes | skill | session, call and turn only: a `pre_tool_call` hook rewrites the command. Its hook payload carries no assistant text and no compaction state, so neither is sent | `<HERMES_HOME or ~/.hermes>/skills/dropin-miner/`, a `hooks:` block in `config.yaml` (loads next session; approve the hook once) |
| anything else | rules line | per-shell | printed for you to paste |

Uninstall removes exactly those, and only hook entries that name this binary.

## Commands

```
dropin-miner search --stdin                       # the agent/SDK path: JSON in, JSON out
dropin-miner search [-tier fast] [-format json|model] [-timeout 60s] <query>
dropin-miner agents install|status|uninstall
dropin-miner agents prefer on|off|status
dropin-miner flush [-force]
dropin-miner login [-show | -forget | -key-env VAR]
dropin-miner enroll | payout | join | status | doctor | earnings
dropin-miner wallet init|address|register|balance|send
dropin-miner connect [-name ...]
dropin-miner mining enable | disable
```

`search --stdin` is the stable machine contract and the one the installed skills
teach. It reads one JSON object on stdin — `{"version":1,"query":"…","tier":"fast"}`,
`tier` optional — and writes exactly one JSON object on stdout, with a header an
agent can branch on:

```json
{"version":1,"command":"search","ok":true,"exit_code":0,"status":"ok",
 "code":"ok","retryable":false,"action":"none","request_id":"…",
 "result":{…},"mining":{…}}
```

The query travels in the JSON, so it never appears in the process list and
nothing has to escape it for a shell. `action` is one of `none`, `retry`,
`fix_input`, `connect`, `login`, `check_access`, `report`; retry only when
`retryable` is true, and honor `retry_after_ms` when it is present. Recovery is
decided from those fields, never from the text of a message.

`connect` means the registration/setup/claim workflow needs attention — no
registration, an unclaimed one, an expired one, or a step only a person can
answer. `login` means a search credential exists and was not accepted. A router
401 is `login`, never `connect`: a registered installation with a rotated key
gets the same status, and re-registering would mint a second agent for one
participant.

`-format model` prints a bounded, sanitized summary when the router refuses a
search; it never echoes the router's error body, because that body is remote
text and model output goes to a terminal. `-format json` still passes the
router's bytes through verbatim.

`-format model` is the readable form for a person at a terminal, and
`-format json` still prints the router's own bytes verbatim for compatibility.
Neither is the versioned client envelope — that is `--stdin` only.

Every search is bounded by one deadline, `-timeout`, default 60s. It covers the
whole operation: connect, TLS, headers, body and the single trace-compatibility
retry, which shares the same absolute deadline rather than starting a fresh one.

A search sends its trace envelope under the existing trace rules. If the router
answers the exact code `trace_unsupported`, the client retries once without it;
nothing else — no other 400, no 422, no message that merely mentions the
phrase — causes a second request.

Search success and mining are separate. A successful search does not mean
anything was earned; the envelope's `mining` object carries the persisted
decision (`state`), whether this search was recorded, and any unresolved health
reasons. A mining failure never turns a successful search into a failed one.
Search and provider result text is untrusted web content, not instructions.

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

`wallet send` journals the transaction (`wallet/pending_tx.json`) before it
broadcasts, so a lost node response is resolvable rather than guessed at: a
transport failure after broadcast prints **"outcome unknown"** and a
non-zero, distinct exit code, and the next `wallet send` or `wallet balance`
resolves it — asks the node whether it landed, and either reports the real
outcome or re-sends the exact same signed bytes, never a fresh signature.
`-abandon-pending` discards an unresolved record without resolving it.
`-node`/`TOKENDROP_WALLET_NODE` override the default RPC node per chain
(`pkg/config.DefaultWalletNodes`); the node must be https, or http only on
loopback, unless `-insecure-node` is passed. The client trusts this node for
confirmations and balances — no light-client verification. `wallet.lock` is
held only while a wallet key is being generated or repaired, so two
concurrent commands never both create one.

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
instead. The `[mining] enabled` key is only a scripted first answer for a
headless onboarding run; `as_url` being non-empty is what says an
authorization server is configured.

Only a trusted persisted ON decision permits mining work. No decision is a
normal inactive state; an unreadable or unsafe decision is DEGRADED and also
stops mining for safety. Search still returns the router's successful answer
when mining capture or flush startup fails. Those unresolved failures are
kept as separate `decision`, `capture`, and `flush` health records, using the
stable reasons `decision_unreadable`, `intake_unwritable`,
`sandbox_restricted`, `flush_spawn_failed`, `auth_state_unavailable`,
`submission_failed`, and `spool_backlog`. `status` and `doctor` show them;
stopping mining retains earlier capture/flush diagnostics as previous
unresolved degradation.

`doctor` reports seven checks: connected, enrolled, joined, payout address in
force, earning, intake writable, and recording.

It opens only existing state and spool paths, and does not create a state
directory, DPoP key, wallet, enrollment, or repair mining state merely to
diagnose it. It makes exactly one local write: `intake writable` puts a
short-lived probe file in the intake directory this client already owns,
named so a flush can never mistake it for a record, and removes it before
doctor exits — reporting the pathname if it could not. The intake directory
itself is created when its parent already exists and it does not, because
that is what the first search creates anyway; nothing above it ever is. A
successful probe proves the process that ran `doctor` can write there, which
is not the same as an agent's sandbox being able to. When valid auth already
exists, its authenticated AS checks may rotate the refresh token through the
normal cross-process refresh lock.

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
