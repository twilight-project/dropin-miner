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
lock. Searches are what give a flush something to submit, but they are not
what starts one: the session-start and session-end hooks each start a flush
of their own, and `dropin-miner flush` runs one by hand. A flush with nothing
recorded simply finds nothing to promote.

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
| Claude Code | skill | full: PreToolUse on Bash rewrites the command; window hooks; Stop flushes | `~/.claude/skills/dropin-miner/`, five hook entries and two `permissions.allow` rules — the quoted and the bare spelling of the same search command — in `~/.claude/settings.json` |
| Cursor | skill | full: lineage file from sessionStart, shell, thought, response, compaction and stop hooks | `~/.cursor/skills/dropin-miner/`, six entries in `~/.cursor/hooks.json` |
| Codex | skill | per-shell | `~/.codex/skills/dropin-miner/`; install also widens `~/.codex/config.toml`'s sandbox (network, plus writable roots: the state directory always, and the intake, sessions and spool directories when `[miner] enabled` — never the config, key or wallet) so searches record and the claim resumes |
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
dropin-miner provider [-status]                   # only on an OPENROUTER_V1 Slot
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

`provider` belongs to that older path and only to some Slots. It reads a
zero-spend provider verification key from stdin (never an argument) and
registers it with the AS, or reports the current binding with `-status`. It
applies only where the Slot's profile is `OPENROUTER_V1`: under the default
`SEARCH_ROUTER_V1` profile the participant holds no provider credential at
all — the AS verifies with its own operator credential — and `provider` says
so and stops rather than sending you into a refusal. `join` ends by naming it
as the next step, but only after asking the AS whether this Slot accepts that
profile.

Registration is one journaled transaction, so an interrupted `connect` is
recoverable rather than half-done: the platform's answer is written to
`registration_pending.json` before the agent record or the key, and the next
run finishes it from that journal instead of registering a second time. If
the local registration record is lost or unreadable while a platform key is
still on file, `connect` rebuilds it from the platform itself through `GET
/v1/agents/me`, and changes nothing locally until that lookup answers; if
the platform no longer recognizes the key, it stops with a conflict rather
than minting a new agent.

Two different things replace a registration, and they are not the same
thing. `-force` bypasses recovery and authorizes a deliberate replacement
where local state would otherwise refuse a fresh registration — a
credentials file already holding a platform key, an `agent.json` naming a
different identity. Separately, and with no flag at all, an ordinary
foreground `connect` may replace a registration the platform has positively
verified as expired: it asks, gets `expired` back, and only then registers
and publishes the replacement, which changes the platform agent and its key
and nothing else. `-resume`, the detached background poll a search spawns,
never registers, rebuilds or replaces — a new identity is never decided in
the background.

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
confirmations and balances — no light-client verification.

`wallet.lock` is the cross-process lock in the wallet directory, and it
covers more than key creation. Creating or repairing a key takes it, so two
concurrent commands never both generate one. `wallet send` holds it from the
pending-journal check through the classification of its first broadcast, so
a second sender blocked on it re-reads the journal after the first one's
write has landed rather than racing past a stale "nothing pending"; it is
released before the confirmation wait and reacquired only to clear the
journal. `wallet balance` takes it while it resolves a pending send, which
is the other place an unresolved transaction gets noticed.

`agents prefer off` makes the agent's own web search the default and keeps
this one for when you name it; `on` makes this one the default again. Inside
the agent, `/dropin-miner off` and `/dropin-miner on` do the same. The choice
is one file beside the config, the installed skills are rewritten from it,
and a reinstall keeps it. While it is off, searches you do not route here
earn nothing.

## Config

Every key below is one a `dropin-miner` command reads. The value shown after
`#` is what you get by leaving the key out.

```toml
[[provider]]
upstream = "https://router-api.nyks.dev"   # https only; the router_url fallback

[mining]
enabled        = true                            # only a scripted first answer — see below
as_url         = "https://rewards.nyks.dev"      # unset: no AS, so no mining work at all
chain_id       = "twilight-testnet-1"            # required once as_url is set
slot_id        = 3                               # required once as_url is set
state_dir      = "/home/you/.tokendrop/state"    # default: <user config dir>/tokendrop/state
spool_dir      = "/home/you/.tokendrop/spool"    # default: <state_dir>/spool
# payout_address = "twilight1..."   scripted `connect`/`mining enable` answer;
#                                   leave unset to be asked at a terminal instead
# platform_slot  = "twilight-slot-3"  required only if the platform ever offers
#                                     more than one mining slot to enroll into
# target_epoch   = 1042               pin the epoch; unset means ask the AS
# metadata_ttl   = "15m"              how long the AS service document is cached
# collector_max_attempts = 0          0 means no attempt ceiling on delivery

[platform]
base_url       = "https://platform.nyks.dev"    # the human portal and claim pages
agents_api_url = "https://agents-v1.nyks.dev"    # register/status/enroll — a separate host

[miner]
enabled        = true                              # default false
intake_dir     = "/home/you/.tokendrop/intake"     # served request ids, until flushed
sessions_dir   = "/home/you/.tokendrop/sessions"   # per-workspace lineage files
flush_interval = "3m"                              # default 3m: how often a flush re-asks the AS
# router_url = "..."   defaults to the [[provider]] upstream
```

`[miner]`'s two directories default beside the state directory —
`<parent of state_dir>/intake` and `.../sessions` — and `intake_dir`'s
parent is also where `credentials.json` and the flush lock are looked for,
so moving it moves those too. `[mining] enabled` has no default worth
printing, because absence and an explicit `false` are different answers:
the config records which of the two you gave (`MiningEnabledExplicit`), and
an explicit `false` at a terminal is a deliberate opt-out that `connect`
will not re-ask, while an absent key means nobody has answered yet and a
terminal is asked. Neither is the switch — see below.

The `[mining]` block is the proxy's, unchanged: a machine that already runs
`tokendrop-proxy` can point this at the same state directory and be the same
participant. `[platform]` and the two extra `[mining]` keys above are
`connect`/`mining enable`'s own (agent onboarding design, §5.5) — see
`dropin-miner connect -h` / `dropin-miner mining -h`. The two `[platform]`
URLs are genuinely different hosts, not a redundant pair: `base_url` is
only ever compared against, never dialed (it is where a printed claim
URL must point); `agents_api_url` is what `connect`/`mining enable`
actually send requests to. Naming a non-default, non-loopback `base_url`
without an `agents_api_url` is refused rather than guessed at; a loopback
`base_url` alone defaults `agents_api_url` to the same loopback address.

The parser still accepts the proxy's own `[proxy]`, `[transport]`,
`[privacy]`, `[log]` and `[observe]` sections, and `[[provider]]`'s `name`
and `tier`, so one config file can serve both programs. No `dropin-miner`
command reads any of them, which is why none is listed above. Unknown keys
are an error, not a warning.

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

`doctor` reports seven checks, in this order: authorization server, enrolled,
joined this epoch, payout address, earning, intake writable, and recording.
`NO` is a fact and a successful run; `UNKNOWN` is the absence of one. The exit
status reports whether the diagnosis could be made at all, so it is non-zero
only when every check came back UNKNOWN.

It opens only existing state and spool paths, and does not create a state
directory, DPoP key, wallet, enrollment, or repair mining state merely to
diagnose it. It performs one bounded local probe operation, and only when
`[miner]` is enabled and the persisted decision says mining is on: `intake
writable` may create the intake directory, then puts at most one inert probe
file in it — named so a flush can never mistake it for a record, because it
does not end in `.json` — and attempts to remove it before doctor exits,
reporting the pathname if it could not. Not "one write": the directory
creation, the file's publication and its removal are separate operations, and
each is reported on its own when it fails. The intake directory itself is
created when its parent already exists and it does not, because that is what
the first search creates anyway; nothing above it ever is. A
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
the embedded opencode plugin. CI uses Node.js 22. Most of the participant
packages under `pkg/` are copied from `tokendrop-proxy` with their golden
vectors; `pkg/platform` and `pkg/fsx` were written here. `pkg/README.md` has
the per-package table.

## Two things worth knowing

Where the query goes depends on which form you use. The human form,
`dropin-miner search "<query>"`, puts the query on the command line, so it is
visible in `ps` and in your shell history on your own machine. The agent form,
`search --stdin`, takes it as JSON on stdin — every installed skill teaches
that one, and it keeps the query out of the process list entirely. The query
is not a credential in either case, and the key is in neither: it is read from
the environment or the owner-only credentials file and is never put in a
command line.

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

## The npm wrapper

This package downloads the release binary for your platform on install, verifies it against the release checksums, and forwards every argument to it. The package version is the release tag it fetches.

- `DROPIN_MINER_BINARY=/path` use a binary already on the machine
- `DROPIN_MINER_SKIP_DOWNLOAD=1` install the wrapper without fetching
