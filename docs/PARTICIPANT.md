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
not create a new identity. If local registration metadata is unreadable while
a platform key exists, connect stops with a local-state conflict; use
`dropin-miner connect -force` only when you mean to replace that credential.

## Then check

```bash
dropin-miner status          # what this installation has and has not completed
dropin-miner payout show     # ACTIVE, and the address as the chain renders it
dropin-miner agents status   # which agents are set up
dropin-miner doctor          # connected, enrolled, joined, paid, earning
```

Restart any agent that was already open, and search as you normally would.
To try it by hand:

```bash
dropin-miner search -format model "what is proof of authority consensus"
dropin-miner flush
```

## Making it the default, or not

Out of the box the skill tells your agent to prefer this search over its
built-in one. If you would rather your agent use its own search unless you
ask for this one, say so once:

```
/dropin-miner off      # in Claude Code, Codex or Cursor
/dropin-miner on       # back to this search as the default
/dropin-miner status
```

or, from a shell, `dropin-miner agents prefer off|on|status`. Either way the
choice is recorded beside your config and the installed skills are rewritten,
so it holds in every agent, from its next start, and across reinstalls.
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

The query itself rides in the command's arguments, so it is visible in `ps`
and your shell history on your own machine. It is not a credential.

## Per agent

**Claude Code** gets a skill and five hook entries in `~/.claude/settings.json`:
one on Bash that threads each search into the current turn, three that track
context compaction, and one on Stop that flushes. It also adds a permission
rule for the search command, so Claude Code runs it without asking each
time; nothing else the binary does is allowed by that rule.

**Cursor** gets a skill and six entries in `~/.cursor/hooks.json`. Cursor
cannot rewrite a command, so its hooks maintain the lineage file and the
search reads it. The shell hook also allows our command, so Cursor never
prompts for it.

**Codex** gets a skill, and — whenever a config is present — a small marked
block in `~/.codex/config.toml` that widens its sandbox just enough: network
on, and the tokendrop intake, sessions, state and spool directories made
writable, so the mining observation can be recorded and the detached claim
resume can write there after every search. Deliberately those directories and
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
Code hook and the opencode plugin do — full session, turn and history lineage,
not just per-shell.

**Hermes** gets a skill in its skills directory (`HERMES_HOME`, else
`~/.hermes/skills/`); Hermes loads skills at session start, so it picks the
search up next time you launch it, threading a per-shell lineage. Both earn the
same as every other agent.

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
elsewhere. `wallet.lock` is a third small file this creates, held only for
the moment a wallet key is generated or repaired, so two commands started at
the same time can never both create one.

## Manual enrollment

The portal's older path — `dropin-miner enroll -assertion`, `login`, `join`,
`wallet register`, `payout set` — still works and coexists with `connect`,
for the rare case scripting against those specific commands directly is what
you want. `dropin-miner help` describes each. Everything above this line is
the path setup actually takes; this one it does not.

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
