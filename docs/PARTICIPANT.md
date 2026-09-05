# Earn on Twilight Slot 3 with your coding agent's searches

`dropin-miner` gives your coding agent a web search that goes through the
Twilight search router. The router meters every search against your key, and
one verified search per epoch makes you eligible for that epoch's reward.
Rewards go to a Twilight address you control.

There is nothing to keep running. The miner lives inside your agent's tool
calls: each search records itself and starts a short background step that
joins the open epoch and submits; your agent's start and stop do the same.

## You need three things

| | |
|---|---|
| **A search-router account** | `platform.nyks.dev` |
| **An API key** | project → Keys → `sr-…`. Shown once at creation; mint a new one if you closed that dialog. |
| **Somewhere to be paid** | setup makes a wallet for you, or you paste a `twilight1…` address you already control (Keplr, say). |

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
| **An enrollment token** | Generate it when asked, not before: it lasts 15 minutes and works once. `platform.nyks.dev → Mining → Slot 3 → Generate enrollment token` |
| **Wallet, or your own address?** | The wallet prints its 24 words once. Have paper ready. |
| **Your sr- API key** | Typed with echo off, checked against the router without spending, stored owner-only in `~/.tokendrop/credentials.json`. Press Enter to skip and run `dropin-miner login` later. |
| **Add settings to your shell profile?** | Puts the binary on PATH and sets `TOKENDROP_CONFIG`. Saying no just means longer commands. |
| **Set up the coding agents found here?** | Writes a skill and, where the agent supports them, hook entries into its own config. Shown before anything is written. |

Use a key from the **same account you enrolled with**. A key from a different
account returns a clean 200 and earns nothing, with no error anywhere. That is
the easiest mistake to make.

To change or check the stored key later:

```bash
dropin-miner login          # paste a new key; replaces the stored one
dropin-miner login -show    # where a search would get its key, masked
dropin-miner login -forget  # remove the stored key
```

`TOKENDROP_API_KEY` in an agent's environment, when set, takes precedence over
the stored key — useful for one shell on a second account.

## Then check

```bash
dropin-miner payout show     # ACTIVE, and the address as the chain renders it
dropin-miner login -show     # which key a search would use
dropin-miner agents status   # which agents are set up
dropin-miner doctor          # connected, enrolled, joined, paid, earning
```

Restart any agent that was already open, and search as you normally would.
To try it by hand:

```bash
dropin-miner search -format model "what is proof of authority consensus"
dropin-miner flush
```

## What travels with a search, and how to turn it off

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

**Codex** gets a skill. Its sandbox runs shell commands with no network by
default; allow network for `dropin-miner search` or searches fail silently.

**opencode** gets an in-process plugin that threads the search, plus a line to
paste into `AGENTS.md`.

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
