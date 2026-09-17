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

Or through npm — installed globally, because every hook and skill setup
writes points at the binary it ran from, and an `npx` cache or a project's
own `node_modules` is a directory npm will discard:

```bash
npm install -g dropin-miner
dropin-miner setup
```

## Coming from an earlier version

Nothing needs removing first. Run the installer again — npm:
`npm install -g dropin-miner@latest`, then `dropin-miner setup`. If the old
installation used a non-default home (`TOKENDROP_HOME` was set for the old
installer), run the installer or `setup -home` with the same one; setup has
no other way to find it.

The installation in `~/.tokendrop` is used as it is: a directory holding an
identity is the installation, and nothing set aside is offered. Your wallet,
identity, search credential and recorded searches are preserved — setup
creates no replacement for a healthy installation; `connect` still runs and
resumes or repairs its own onboarding state under its normal rules (an
unfinished registration is finished, a lost or unreadable record beside a
working key is rebuilt from the platform, an expired unclaimed registration
is replaced). Byte-identical auth state across the upgrade is not promised.

The mining question is not asked again when the old installation already
holds a decision: setup says whether mining is on or off for it; an
interrupted old installation with no decision yet is asked.

The config is parsed and, since the script's already has the `[platform]`
and `[miner]` tables, left byte for byte.

On macOS and Linux, the profile block uses the markers the script wrote, so
there is never a second block; setup's block quotes its paths where the
script's were bare, so the bytes can differ and the profile question may be
asked again — yes replaces the one block in place. On Windows, the existing
user PATH entry and `TOKENDROP_CONFIG` are reused, not duplicated: the first
new setup records in `setup-env.json` that an already-present PATH entry
was not added by setup, and what `TOKENDROP_CONFIG` held before, so a later
`uninstall` removes only what setup itself added and prints what to remove
by hand for the rest.

Agent integrations are reconciled to the current plan: one already correct
is left alone ("nothing to write: already set up"); stale files and hook
entries are updated, not duplicated; a host never set up before is offered
normally, which is the usual case for a Windows 0.2.x installation, whose
installer only advised `agents install`. A second setup is idempotent for what
it owns — its written files, the profile or environment, and the agent
integrations — and keeps the same healthy participant identity;
connect-managed authorization state may still advance.

Want a clean start instead? Move `~/.tokendrop` aside and take it back at
the **Use it?** question below.

The installer fetches a checksummed release and hands off to `dropin-miner
setup`, which asks as it goes, in this order:

1. **Use it?** — only if one is set aside beside
   `~/.tokendrop` (see "Removing it, and coming back" below).
2. The config is written, or an existing one is kept (see Config). Then
   `connect` registers with the search platform and stores the key it mints —
   nothing to copy, nothing to paste — and asks **Enable mining rewards?**
   and, on yes, **Payout address** (empty creates a wallet: it asks for a
   keyfile passphrase, twice, before it prints the 24-word recovery phrase),
   then prints a claim link.
3. **Add them to your shell profile?** — the binary's directory on PATH,
   `TOKENDROP_CONFIG`, and — when a wallet was made here — `TOKENDROP_WALLET_DIR`.
   On Windows there is no profile: the question is whether to set PATH and
   `TOKENDROP_CONFIG` (only) in your user environment.
4. **Set up the coding agents found on this machine now?** — shown with
   exactly what would be written first.

`setup -yes` answers the shell-profile and coding-agents questions, with or
without a terminal, so an automated caller may invoke `setup -yes`; the
installers never add it. Without a terminal and
without `-yes`, setup leaves your profile and your agents alone and prints
the command for each. A set-aside installation is reused only when a person
says yes at a terminal, `-yes` or not, and the mining question is always
connect's.
`setup -dry-run` prints what it would write or move and changes nothing.
Search itself works before you ever visit the claim link — the claim only
gates the reward, once you say yes to mining. The key never goes into a
command line or an agent's config; `TOKENDROP_API_KEY` in the environment
overrides the stored one.

## How it works

Everything happens at four moments the agent already has.

| moment | who runs it | what happens |
|---|---|---|
| install | you, once | `setup`: previous installation, config, connect registers and stores the key, mining question, wallet or address, claim link printed, profile, skill and hooks written per agent |
| session start | a hook | seed the context-window counter, start a flush |
| tool call | the agent | `dropin-miner search` posts to the router with your key and the trace envelope, prints results, records the served request id, starts a flush |
| session end | a hook | start a flush |

A **flush** is the mining plane as one pass: ask the AS which epoch is open,
join it if not joined, hold a participation capability, promote recorded
searches into the spool, submit once, exit. Two flushes never overlap: every
flush, whatever version and wherever it runs, takes the one `flush.lock`
beside the intake directory, and a second flush finds it held and leaves.
Inside a sandbox that lets an agent write the state and intake directories
but not the installation directory itself (Codex), the flush opens that same
lock read-only and holds it just as exclusively. It keeps its stamp,
`flush.json`, in the state directory. Searches are what give a flush
something to submit, but they are not what starts one: the session-start and
session-end hooks each start a flush
of their own, and `dropin-miner flush` runs one by hand. A flush with nothing
recorded simply finds nothing to promote.

The **trace** is how the router groups one task's searches. It comes from
whichever of these the host allows: a hook, plugin or extension that rewrites
the shell command with the envelope in an environment variable (Claude Code,
opencode, Pi, Hermes), a per-workspace
lineage file the hooks write and `search` reads (Cursor, and every host as a
fallback), or a hashed per-shell identity when there is no hook at all. The
assignment is written in the syntax of the shell that will run the command —
`TOKENDROP_TRACE_BRIDGE=<envelope> <command>` in a POSIX shell, and in
PowerShell an assignment the same command removes again when it finishes, so
the next command in a reused shell does not inherit it. Every
identifier is hashed before it leaves the machine; the assistant text just
before a search travels only inside that search's request, capped at 32 KB.
Recent agent context accompanies search to the Twilight search router as part
of the trajectory/search product. `TOKENDROP_TRACE=off` disables trace
transmission. Mining/AS receives metadata observations only.

The trace is **unauthenticated metadata**, and the client treats it that way.
A binary cannot prove where an environment variable came from, so nothing
here is evidence of origin: a lineage adapter removes any bridge it finds
that it can prove is a standalone assignment and writes its own for the call
it is handling, and a command carrying one it cannot remove with certainty is
left exactly as it is, with no lineage claimed for it. Mining evidence is a
separate thing entirely — the AS reconciles request ids against the provider,
and never takes the client's word for volume.

## Per host

Every command a skill teaches is rendered for the shell that host actually
runs, per operating system — a quoted heredoc where that is Bash, and a
single-quoted here-string piped into the call where it is PowerShell, with
the encoding line that makes a query with an apostrophe, a quotation mark or
any non-ASCII character arrive exactly as written on Windows PowerShell 5.1.
Claude Code on Windows is taught both, because which of its two shell tools a
call uses is the model's choice. Where nobody has established what a host
runs, the skill keeps the Bash form and `agents install` says so in its plan
rather than removing a host that works; no host is in that state today.

| host | tool | shell it is taught for | lineage | files written by `agents install` |
|---|---|---|---|---|
| Claude Code | skill | Bash on macOS and Linux; on Windows both Git Bash and PowerShell | full: PreToolUse on its Bash **and** PowerShell tools rewrites the command, in the syntax of whichever one the call used; window hooks; Stop flushes | `~/.claude/skills/dropin-miner/`, five hook entries and three `permissions.allow` rules — the single-quoted spelling the skill renders, plus the quoted and bare ones an agent may repeat from an older skill — in `~/.claude/settings.json`. Those rules are Bash rules: **a search the model sends through Claude Code's PowerShell tool on Windows still prompts**, because what a PowerShell-tool permission rule has to look like is not established yet (#77) and a rule guessed at would never match |
| Cursor | skill | Bash on macOS and Linux; PowerShell on Windows | full: lineage file from sessionStart, shell, thought, response, compaction and stop hooks; the shell hook auto-allows exactly the search the skill renders, and nothing looser | `~/.cursor/skills/dropin-miner/`, six entries in `~/.cursor/hooks.json` |
| Codex | skill | Bash on macOS and Linux; PowerShell on Windows | per-shell | `~/.codex/skills/dropin-miner/`; install also widens `~/.codex/config.toml`'s sandbox (network, plus writable roots: the state directory always, and the intake, sessions and spool directories when `[miner] enabled` — never the config, key or wallet) so searches record and the claim resumes; the flush a search starts runs inside that sandbox too, taking the flush lock read-only (setup and every flush outside the sandbox make sure the lock file exists) and writing its stamp in the state directory. A command inside the sandbox can read `credentials.json` (a search needs the key) and the state directory (a flush needs it); on Windows it cannot read the wallet, whose directory keeps its own owner-only access |
| opencode | AGENTS.md line | Bash on macOS and Linux; PowerShell on Windows | full: in-process plugin rewrites the bash command | `~/.config/opencode/plugins/dropin-miner.js` |
| Pi | skill | Bash everywhere (Git Bash on Windows) | full: an auto-discovered extension rewrites the bash command; history is bound to the tool call that asked for it, and the window generation is read back from the session's own compaction entries | `~/.pi/agent/skills/dropin-miner/`, `~/.pi/agent/extensions/dropin-miner.ts` |
| Hermes | skill | Bash everywhere (Git Bash on Windows) | session, call and turn only: a `pre_tool_call` hook rewrites the command. Its hook payload carries no assistant text and no compaction state, so neither is sent | `<HERMES_HOME or ~/.hermes>/skills/dropin-miner/`, a `hooks:` block in `config.yaml` (loads next session; approve the hook once) |
| anything else | rules line | Bash | per-shell | printed for you to paste |

Uninstall removes exactly those, and only hook entries that name this binary.

## Commands

```
dropin-miner setup [-yes] [-dry-run] [-with id] [-no-profile] [-no-agents] [-home dir]
dropin-miner uninstall [-binary] [-purge-state] [-dry-run] [-yes] [-home dir]
dropin-miner upgrade [-version X.Y.Z | -rollback] [-home dir]
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
register, get claimed at a printed URL, then mine unattended. `setup`, which
both installers hand off to, runs `connect` itself; run it directly yourself for
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
falling back to `TOKENDROP_CONFIG`, then `./tokendrop.toml`, then the
installation's own config (`$TOKENDROP_HOME/tokendrop.toml`, else
`~/.tokendrop/tokendrop.toml`, when that file exists) — and only then
built-in defaults. `status` and `doctor` name the file they resolved, or say
plainly that none was found; `connect` refuses outright when resolution
finds no config file at all, rather than register against built-in-default
state, and says to run `dropin-miner setup`.

`login` reads your sr- key from stdin (never an argument), verifies it with a
zero-spend probe against the router, and writes `~/.tokendrop/credentials.json`
as `0600`. A search takes its key from `TOKENDROP_API_KEY` if set, else that
file (refused if it is a symlink or readable by others). Otherwise, run
`dropin-miner connect` or `dropin-miner login` to set up a search credential.

The wallet is the one part of an installation that neither a search nor a
flush reads, so on Windows it is kept owner-only even when another program
adds inherited access to the installation directory: the wallet directory and
every file in it carry their own owner-only access list, set when the wallet is
created and on every write. `setup` repairs a wallet an earlier version made,
the directory and each file in it, and stops on a wallet object it cannot
secure (a link or junction inside the directory, say); `doctor` reports anyone
else who can read it. A coding agent's sandboxed commands can therefore still
use `credentials.json` and the state directory, which a search and a flush
need, and not the wallet. On macOS and Linux a sandboxed agent runs as you, and
file modes cannot tell it apart from you: there the wallet file is readable to
it, and the key inside stays sealed by its passphrase. That makes the passphrase
the thing protecting it, so give it one you use nowhere else, and keep the 24
words off the machine: anyone who can read the file can copy it and try
passphrases against that copy for as long as they like, on their own hardware,
with nothing to slow them down and nothing to tell you it is happening.

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
so moving it moves those too. The flush stamp lives in the state directory;
a `flush.json` beside the intake directory, from before 0.2.10, is read
once as a starting value and otherwise left alone until a purge. `[mining] enabled` has no default worth
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

`setup` never rewrites a config that is already there. It loads the file
first and refuses one that does not load, naming the file and the error,
whatever the file says. One with a `[miner]` table is left byte for byte. One
without — a `tokendrop-proxy` config, say — gains only the tables it lacks,
`[platform]` and `[miner]`, appended after its own lines, and the result has
to load before it is saved. `[mining] enabled = true` is written only by a
setup with no terminal and `TOKENDROP_MINING=1`; at a terminal the answer is
connect's question.

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
`sandbox_restricted`, `flush_spawn_failed`, `flush_state_unavailable` (a flush
could not take its lock for a reason other than another flush holding it, or
could not write its stamp), `auth_state_unavailable`, `submission_failed`, and
`spool_backlog`. `status` and `doctor` show them;
stopping mining retains earlier capture/flush diagnostics as previous
unresolved degradation.

`doctor` reports seven checks, in this order: authorization server, enrolled,
joined this epoch, payout address, earning, intake writable, and recording. On
Windows an eighth follows, wallet access: `NO` when anyone other than you can
read the installation's wallet directory or a file in it, naming who, with
`dropin-miner setup` as the repair.
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

## Upgrading

```
dropin-miner upgrade                    # the latest release
dropin-miner upgrade -version 0.3.1     # exactly that release, never an older one
dropin-miner upgrade -rollback          # put back the binary the last upgrade replaced
npm install -g dropin-miner@latest      # an npm install is updated with npm
```

`upgrade` replaces a native binary — the one the installer put in
`~/.tokendrop/bin` — with a release from the canonical
`twilight-project/dropin-miner` GitHub repository and nowhere else. It checks the
download against the release's `checksums.txt`, takes only the `dropin-miner`
executable out of the archive, runs the new binary's `version` before installing
it and runs the installed path again afterwards, and only then keeps the binary
it replaced as `dropin-miner.previous`. Before that point, a failure it can recover
from puts the binary you had back and leaves `dropin-miner.previous` exactly as it
was; if putting it back fails too, `upgrade` stops with `manual_intervention`,
lists every copy that survives, and removes none of them.
It never moves to an older release; `-rollback` is the one way back, restoring
`dropin-miner.previous` with no network, and running it again swaps back. It
refuses a development build, and a copy npm installed. The whole operation is
bounded at three minutes, and it will not run while setup or another upgrade of
the same binary is running. Your wallet, registration and config are not touched.

On Windows the running binary is moved aside, the new one moved into its name,
checked, and the old one kept as `dropin-miner.exe.previous`. If an older
DropinMiner process is still running from that `.previous` file, the upgrade
stops with `previous_in_use` and puts the binary you had back, as with any
recoverable failure; close old DropinMiner or agent processes and run it again. This is tested on Windows x64 and Windows
arm64.

When it fails, the first word after `upgrade:` says what to do: `retry` (try
again later; nothing changed), `release_invalid` (the release itself is wrong;
don't retry blindly), `ownership` (this copy is not one `upgrade` may replace),
`filesystem` (fix permissions or the file named), `lifecycle_busy` (another
setup or upgrade is running), `refused` (an older version was asked for), or
`manual_intervention` (a failure's own recovery failed; every surviving copy is
listed).

## Removing it, and coming back

```
dropin-miner uninstall -dry-run                  # what would be removed; changes nothing
dropin-miner uninstall                           # integrations, profile block / user environment
dropin-miner uninstall -binary                   # ...and this installation's own binary
dropin-miner uninstall -purge-state              # ...and the wallet, identity, key, evidence, config
npm uninstall -g dropin-miner                    # an npm install is npm's to remove
```

`uninstall` takes out what setup put on this machine for one installation
(`-home`, default `~/.tokendrop`): the coding agents' skills, hooks and plugins
that run its binary, and the shell-profile block that names its config. On
Windows it reverts the user `PATH` entry and `TOKENDROP_CONFIG` against
`~/.tokendrop/setup-env.json`, the record of what setup changed: the `PATH`
entry goes only if setup added it, and `TOKENDROP_CONFIG` goes back to what it
held only while it still holds setup's value — one you changed since is yours
and is left. Without that record nothing in the environment is guessed at; it
prints what to remove by hand. Anything that runs another installation's
binary, or a profile block naming another config, is left and reported. Your
wallet, registration, stored key, recorded searches and config stay, and
nothing is revoked; it ends by saying how to keep using them (`-config
~/.tokendrop/tokendrop.toml`, or `dropin-miner setup` again). A bare `connect`
afterwards would register this machine anew.

`-binary` also removes `~/.tokendrop/bin/dropin-miner`, only when that is the
binary running and no package manager owns it, together with the
`dropin-miner.previous` an upgrade kept and anything an interrupted upgrade left
beside it — nothing else in that directory. A copy npm installed is refused with
the npm command to use instead. On Windows a running binary cannot be deleted,
so `-binary` moves it out of its name to `bin\.dropin-miner.displaced-<random>`
and prints that path; delete the file once no DropinMiner or agent process is
running it.

`-purge-state` is separate from `-binary` and destroys the participant state in
the installation directory: the wallet, the identity and stored key, recorded
searches, the config and preferences. It needs a terminal and asks you to type
the wallet's address (or, with no wallet, the installation path); `-yes` never
answers it. That guards against accidents, not against a program driving your
terminal. Before removing anything it tries, for at most eight seconds, to
revoke this installation's authorization at the rewards service; a purge still
completes if that fails, and says so. The platform's grant is revoked only at
the console. Directories your config points at outside the installation are
left and listed. `~/.tokendrop.lifecycle.lock`, which only coordinates
DropinMiner commands, is left behind and safe to delete. Close any open agent
sessions first: searches and hooks are not paused, and one that runs
afterwards can recreate an empty `intake/` or `sessions/`.

While uninstall runs it holds a lock that setup, connect and flush wait for, so
none of them starts underneath it. A plain uninstall refuses to start while
setup is running; `-binary` and `-purge-state` also refuse while connect or
flush is. Those are excluded for as long as uninstall holds their locks.

The wallet in `~/.tokendrop` is the only copy unless you kept the 24 words.
Instead of purging, you can set the directory aside as
`~/.tokendrop.bak-<date>` (any `~/.tokendrop.<something>` or
`~/.tokendrop-<something>`). The next setup finds it and says what it holds.
One left in place is simply used. One set aside is offered, newest first, and
moved back only when you say yes at a terminal, so you are not enrolled twice
or paid to a second address:

- **Your identity** — the `state/` directory and the stored key — moves as one
  piece or not at all. If `~/.tokendrop` already holds an identity of its own,
  setup stops before anything moves and before `connect` runs: it names both
  places, and you choose one installation, move the other out of the way, and
  run setup again. A `state/` that only holds the key of an
  enrollment that never finished is renamed aside as `state.unenrolled-<time>`,
  never deleted.
- **Your wallet** moves as a whole. A `~/.tokendrop` that already holds a
  wallet is an installation in its own right: it is used as it is, and nothing
  set aside is offered. A `wallet/` there with no wallet key in it — left by a
  wallet that was never finished — is renamed aside as
  `wallet.incomplete-<time>`, never deleted, and yours moves in.
- **Unsent searches and session files** (`spool/`, `intake/`, `sessions/`) are
  merged file by file, never overwriting one that is already there.
- **The config** moves only if `~/.tokendrop` has none, and is then updated the
  way any existing config is.

Anything that is a symlink rather than a plain file or directory is not moved.
The set-aside directory is removed afterwards only if nothing is left in it;
otherwise setup says what it left.

## License

Apache-2.0.

## The npm wrapper

This package downloads the release binary for your platform on install, verifies it against the release checksums, and forwards every argument to it. The package version is the release tag it fetches.

- `DROPIN_MINER_BINARY=/path` use a binary already on the machine
- `DROPIN_MINER_SKIP_DOWNLOAD=1` install the wrapper without fetching
