# Earn on Twilight Slot 3 with your coding agent's searches

`dropin-miner` gives your coding agent a web search that goes through the
Twilight search router. The router meters every search against your key, and
one verified search per epoch makes you eligible for that epoch's reward.
Rewards go to a Twilight address you control.

There is nothing to keep running. The miner lives inside your agent's tool
calls: each search records itself and starts a short background step that
joins the open epoch and submits; your agent's start and stop do the same.

## You need two things

| | |
|---|---|
| **A moment to claim your agent** | Setup prints a link. Visit it, sign in (or create an account — `platform.nyks.dev`), and approve mining. Search itself works before you do this; only the reward waits on it. |
| **Somewhere to be paid** | Setup makes a wallet for you, or you paste a `twilight1…` address you already control (Keplr, say). |

There is no API key to go and mint first. Setup registers your agent with
the search platform itself and stores the key it gets back — nothing to
copy, nothing to paste.

### Where rewards land

If setup makes the wallet, it prints a **24-word recovery phrase exactly
once**. Write it down on paper before you continue. It is stored nowhere, it
is never shown again, and anyone who has it controls the money. The key on
this machine only ever receives; spending needs the passphrase you chose.

If you would rather be paid into a wallet you already control, paste that
address when asked. Nothing else changes.

## Setup

macOS and Linux:

```bash
curl -fsSL https://raw.githubusercontent.com/twilight-project/dropin-miner/main/scripts/install.sh | sh
```

Windows, in PowerShell:

```powershell
irm https://raw.githubusercontent.com/twilight-project/dropin-miner/main/scripts/install.ps1 | iex
```

Setup asks, in order:

| it asks | what to know |
|---|---|
| **Use the previous installation?** | Only if one is found in `~/.tokendrop` or set aside beside it. Yes keeps your wallet, registration and key; the questions below that they answer are then skipped. |
| **Enable mining rewards?** | A bare Enter answers no — search still works either way. Yes asks the next question now; changing your mind later is `dropin-miner mining enable` (or `mining disable` to stop). |
| **Wallet, or your own address?** | Only asked after yes above. The wallet prints its 24 words once. Have paper ready. |
| **Add settings to your shell profile?** | Puts the binary on PATH and sets `TOKENDROP_CONFIG`. Saying no just means longer commands. |
| **Set up the coding agents found here?** | Writes a skill and, where the agent supports them, hook entries into its own config. Shown before anything is written. |

Then it prints a claim link and waits a few minutes for you to visit it. Not
required right there and then: search already works, and revisiting the link
later (or just using an agent — it resumes on its own) finishes the rest.

Registration is recoverable if the command is interrupted while it is saving
the new platform identity. An unclaimed registration that expires is replaced
by a foreground `dropin-miner connect`; the replacement changes only the
platform agent and its key, preserving the wallet, payout choice, auth state
and mining evidence. A detached `connect -resume` records the expiry but does
not create a new identity. If local registration metadata is lost or
unreadable while a platform key still exists, connect rebuilds it from the
platform itself rather than minting a new agent — nothing local changes until
that lookup answers. If the platform no longer recognizes the key (revoked,
or genuinely unknown), connect stops with a local-state conflict instead; use
`dropin-miner connect -force` only when you mean to replace that credential.
A rebuilt registration that is still unclaimed but whose claim link the
platform did not return prints a one-line notice instead of a link — wait for
it to be claimed or expire, or `connect -force` to start over. `-resume`
never attempts a rebuild (or a fresh registration): it is the unattended
background poll, not the place a new identity gets decided.

## Then check

```bash
dropin-miner status          # what this installation has and has not completed
dropin-miner payout show     # ACTIVE, and the address as the chain renders it
dropin-miner agents status   # which agents are set up
dropin-miner doctor          # authorization server, enrolled, joined this
                             # epoch, payout address, earning, intake
                             # writable, recording
```

Restart any agent that was already open, and search as you normally would.
To try it by hand:

```bash
dropin-miner search -format model "what is proof of authority consensus"
dropin-miner flush
```

Your agent does not use that form. It uses the machine path, which takes the
query as JSON on stdin so nothing has to escape it and it never reaches the
process list:

```bash
dropin-miner search --stdin <<'JSON'
{"version":1,"query":"what is proof of authority consensus"}
JSON
```

That prints exactly one JSON object. Eight fields are always there —
`version`, `command`, `ok`, `exit_code`, `status`, `code`, `retryable` and
`action` — and they say what happened and what to do about it; `result`
carries the answer and its citations, and `mining` carries the mining state.
`exit_code` is the process's own status: 0 for a valid answer, 1 transport,
2 usage, 3 a 4xx, 4 a 5xx or a response the client could not use.
`status`, `doctor` and `connect` take `-json` and answer the same way.

A search is bounded by `-timeout`, default 60s, covering the whole operation.

## Making it the default, or not

Out of the box the skill tells your agent to prefer this search over its
built-in one. If you would rather your agent use its own search unless you
ask for this one, say so once:

```
/dropin-miner off      # in any agent that got a skill: Claude Code, Codex,
                       # Cursor, Pi or Hermes
/dropin-miner on       # back to this search as the default
/dropin-miner status
```

or, from a shell, `dropin-miner agents prefer off|on|status`. Either way the
choice is recorded beside your config and every installed skill is rewritten
from it, so it holds in each of those agents from its next start, and across
reinstalls. opencode is the exception: it has no skill directory, so its
AGENTS.md line carries no preference and the choice does not reach it.
While it is off, "search through dropin-miner" or "use the router" in a
request still routes that one search here. Searches that do not come here
earn nothing.

## What travels with a search, and how to turn it off

Recent agent context travels to the Twilight search router as part of the
trajectory/search product. Mining/AS receives metadata observations only.
`TOKENDROP_TRACE=off` disables trace transmission.

Each search carries a small `trace` beside the query so the router can group
one task's searches: hashed session and turn identifiers (your agent's real
ids never leave the machine), a call counter, and the assistant text just
before the search, capped at 32 KB. That text is conversation content leaving
your machine; it goes only inside the search request, to the router, and the
miner stores none of it beyond a per-workspace lineage file under
`~/.tokendrop/sessions` that the hooks maintain. To send no trace at all:

```bash
export TOKENDROP_TRACE=off
```

Searches are metered and earn exactly the same either way.

The query itself travels differently in the two forms. Typed by hand,
`dropin-miner search "<query>"` puts it in the command's arguments, so it is
visible in `ps` and your shell history on your own machine. Your agent does
not use that form: `search --stdin`, which every installed skill teaches,
takes the query as JSON on stdin, and it never reaches the process list. It
is not a credential either way, and the key is in neither form — that is
read from the environment or the owner-only credentials file.

## Per agent

**Claude Code** gets a skill and five hook entries in `~/.claude/settings.json`:
one on Bash that threads each search into the current turn, three that track
context compaction, and one on Stop that flushes. It also adds two
`permissions.allow` rules for the search command — the quoted and the bare
spelling of the same command, because a shell may strip the quotes — so
Claude Code runs it without asking each time; nothing else the binary does is
allowed by those rules.

**Cursor** gets a skill and six entries in `~/.cursor/hooks.json`. Cursor
cannot rewrite a command, so its hooks maintain the lineage file and the
search reads it. The shell hook also allows our command, so Cursor never
prompts for it.

**Codex** gets a skill, and — whenever a config is present — a small marked
block in `~/.codex/config.toml` that widens its sandbox just enough: network
on, and a short list of writable directories. The state directory is always
on that list, because the detached claim resume writes there after every
search whether you mine or not; the intake, sessions and spool directories
join it when `[miner] enabled` is set, which is where the mining observation
is recorded. Deliberately those directories and
never the home itself: `tokendrop.toml`, `credentials.json` and `wallet/`
stay read-only to sandboxed commands, so a command that goes wrong inside
Codex cannot rewrite where your credentials are sent. Without the block,
Codex's default `workspace-write` sandbox lets the search return results but
silently blocks the write, so searches earn nothing and the claim is never
picked up.
If you already keep your own `[sandbox_workspace_write]` table, install leaves
it alone and prints the settings to add by hand.

**opencode** gets an in-process plugin that threads the search, plus a line to
paste into `AGENTS.md`.

**Pi** gets a skill in `~/.pi/agent/skills/dropin-miner/` and an
auto-discovered extension in `~/.pi/agent/extensions/`. The extension rewrites
the bash command that runs the search with the trace bridge, the way the Claude
Code hook and the opencode plugin do: the session, the call, the assistant text
that led to that particular search, and how many times the context window has
compacted — read back from the session Pi itself persisted, so a resumed
session keeps its place.

**Hermes** gets a skill in its skills directory (`HERMES_HOME`, else
`~/.hermes/skills/`) and a `pre_tool_call` hook in `config.yaml` that rewrites
the search command with the trace bridge. Less rides with it than with Pi, and
that is a property of Hermes rather than a gap here: its hook payload carries
the session, the tool call and the turn — which is what we send, each hashed —
and no assistant text and no compaction state, so neither is sent. Both load at
session start, so they take effect next launch. Two Hermes notes: it asks once
to approve the hook the first time it fires (approve it, or start Hermes with
`--accept-hooks`, `HERMES_ACCEPT_HOOKS=1`, or `hooks_auto_accept: true` — a
run that cannot prompt and has none of those simply skips the hook), and its
shell tool lives in the `terminal`/`coding` toolsets, so run Hermes with one of
those for the search to execute. All these agents earn the same as every other.

If your `config.yaml` already has a `hooks:` section of its own, install will
not touch the file: it prints the four lines to paste under your own section
instead. That refusal is deliberate and errs on the cautious side — YAML keeps
the last of two identical keys and says nothing, so a config edited on a guess
could lose the hooks you wrote, with no error to tell you.

`dropin-miner agents uninstall` removes exactly those files and entries.

## Four things worth knowing

**Your first reward takes 1–2 hours.** You join an epoch two ahead; it then has
to close, reconcile and settle. Nothing is wrong during the wait.

**One verified search per epoch makes you eligible.** More searches do not earn
more; eligibility is a threshold, not a weight.

**The pot splits equally among everyone eligible**, so your share falls as more
people join. That is the design.

**A long idle gap can miss an epoch.** Recorded searches are submitted by the
next search or the next agent session. If neither happens before the epoch's
verification deadline, that epoch's evidence is late. `dropin-miner flush` by
hand submits whatever is pending.

## When `doctor` says `recording UNKNOWN`

`doctor` ends with two checks about whether searches are actually being
recorded, and they are the ones to read when everything else says OK and you
still are not earning.

**`recording UNKNOWN`** with "recent miner activity, but nothing is queued
locally or verified at the AS" means the mining plane ran recently — a search,
a session hook, or a manual flush — and yet nothing is waiting in your intake
directory, nothing is in the spool, and the AS has nothing for this epoch.
There is one thing to check: that `miner.intake_dir` is the mining intake
directory the agent's own `dropin-miner search` command writes into. The record
is written by `search` itself; the session hooks maintain lineage and start
flushes, they do not write it. So the usual cause is an agent running with a
different config — or, under a sandbox, one that cannot write there at all.
`dropin-miner agents status` shows what is installed; re-run `dropin-miner
agents install` if the agent is using another config or lacks sandbox access.
This check is a heuristic and says so — it never reports a failure, because
none of the evidence it reads can prove one.

`recording` can also say **`could not determine — …`**. That is not a
failure either; it means something it depends on could not settle the
question, so it declined to guess. The reason is on the line, and it is one
of: an input it could not read (the health records, the flush stamp, the
intake directory, the spool), a flush stamp dated in the future — which would
make "recent" meaningless — an intake probe that was skipped or failed (see
`intake writable`), an AS that did not report this epoch's activity, or an AS
answer that contradicts itself by claiming verified activity and a verified
count of zero. What it never says is `NO`: nothing it reads could prove a
fault, so it does not assert one. "No recent activity" stays `OK` even when
the AS is unreachable, because with nothing recorded and nothing having run
there is nothing to explain.

`intake writable` can say **`could not determine`** too, when neither the
intake directory nor its parent exists yet. That is not a fault: `doctor` will
not build a directory tree merely to test one, so it reports that it did not
look rather than guessing. On the default layout the parent always exists, so
this only comes up when `miner.intake_dir` names a custom path somewhere that
has not been created — a fresh default install gets `OK`, and `doctor` creates
the intake directory itself, which is what the first search would have done
anyway.

**`intake writable`** speaks only for the process that ran `doctor` — usually
you, at a terminal. A search runs inside your agent's sandbox, which may have
different permissions, so `OK` here does not prove a search can write there.
If it says `NO` and you use Codex, re-run `dropin-miner agents install`: it
configures the sandbox to allow that directory.

## Changing the payout address later

Your first address is in force as soon as you set it. Setting a *different*
one is a change, and a Slot operator has to approve it: you keep being paid
at the old address until they do. To request a change, contact the Slot
operator at <https://platform.nyks.dev/contact-us> and say which address you
want to change *to*. Nobody needs your API key, your recovery phrase, or the
contents of `~/.tokendrop/` to approve a payout address, and no operator will
ask you for them.

## Sending funds with `wallet send`

`dropin-miner wallet send -to <address> -amount <n>` moves funds out of this
wallet. Before it broadcasts anything it writes a small record —
`wallet/pending_tx.json` — naming the transaction it is about to send; that
record is what lets a lost network response be resolved instead of guessed
at. If the node accepts the transaction but its reply never arrives, `send`
reports **"outcome unknown"** rather than either "sent" or "failed" — the
transaction may or may not have gone through, and it is not safe to just try
again with a new one. Run `wallet send` (or `wallet balance`) again: it
checks the node for that same transaction first, either reports what
actually happened and clears the record, or re-sends the exact same signed
bytes if the node still has not seen it — never a second, independently
signed transaction. `wallet send -abandon-pending` discards an unresolved
record without finding out what happened to it, if you are certain it never
matters.

The client trusts whichever RPC node it talks to (`-node`, or
`TOKENDROP_WALLET_NODE`, or the one built-in default for the testnet) for
balances and confirmations; it does not independently verify the chain the
way a light client would. That node must be `https`, or plain `http` only on
your own machine (loopback) — pass `-insecure-node` to override this and
accept the risk if you really mean to point it at a plain-http node
elsewhere.

`wallet.lock` is a third small file this creates. It is held while a wallet
key is generated or repaired, so two commands started at the same time can
never both create one — and also by `wallet send`, from the moment it reads
the pending record through to knowing what its first broadcast did, so a
second `send` started alongside it waits and then finds the journal the
first one wrote rather than racing past an empty one. `wallet balance` takes
it too, for as long as it spends resolving a pending send.

## Manual enrollment

The portal's older path — `dropin-miner enroll -assertion`, `login`, `join`,
`wallet register`, `payout set` — still works and coexists with `connect`,
for the rare case scripting against those specific commands directly is what
you want. `dropin-miner help` describes each. Everything above this line is
the path setup actually takes; this one it does not.

One command on that path depends on which Slot you are on. `dropin-miner
provider` reads a zero-spend provider verification key from stdin and
registers it with the authorization server, and `provider -status` reports
what is bound. **It applies only when the Slot's profile is
`OPENROUTER_V1`.** On the default `SEARCH_ROUTER_V1` profile — the one
everything above describes — you hold no provider credential at all:
verification runs on the Slot operator's own, and you supply nothing.
If you run `provider` there it tells you so and stops, rather than failing
somewhere further in. `join` names it as the next step only after asking the
AS whether this Slot accepts that profile; if it does not, `join` says the
next step is nothing.

## Removing it, and coming back

`dropin-miner agents uninstall` takes the skills, hooks and plugin out of
your agents and touches nothing else. Delete the binary if you like. Do not
delete `~/.tokendrop` unless you mean to lose the wallet in it: if you made
the wallet here, the 24 words you wrote down are the only other copy.

Coming back later, run the installer again. It looks for `~/.tokendrop`, or
a set-aside copy beside it (`~/.tokendrop.bak-<date>`, `~/.tokendrop.old`),
tells you what it holds — the wallet's address, whether it is enrolled,
whether a key is stored — and asks before using it. Saying yes carries the
wallet, the enrollment, the key and any unsent spool over; the steps that
would have made new ones are skipped. Saying no starts fresh beside it.
