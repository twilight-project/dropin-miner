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

The wallet setup makes lives in `~/.tokendrop/wallet`. Nothing but you needs to
read it: a search uses the stored key in `credentials.json` and a flush uses the
state folder, and neither touches the wallet. On Windows, setup gives the wallet
folder and every file in it an access list of their own with only you on it, so
access another program adds to `~/.tokendrop` — a coding agent's sandbox, for
instance — reaches the rest of the installation and not the wallet. Running
`dropin-miner setup` again puts that right for a wallet an earlier version made
(and stops, saying why, if something in the wallet folder cannot be secured, such
as a link to somewhere else); `doctor`'s `wallet access` line says whether anyone
else can read it. On macOS and Linux an agent's sandbox runs as you, and file
permissions cannot tell the two apart, so a sandboxed command can read the
wallet file there; the key inside only spends with your passphrase. That makes
the passphrase the thing protecting it, so choose one you use nowhere else, and
write the 24 words on paper rather than keeping them on the machine. Anyone who
can read the file can take a copy and try passphrases against it at their
leisure, on their own computer — there is no limit on the attempts and nothing
tells you they are trying.

## Setup

macOS and Linux:

```bash
curl -fsSL https://raw.githubusercontent.com/twilight-project/dropin-miner/main/scripts/install.sh | sh
```

Windows, in PowerShell:

```powershell
irm https://raw.githubusercontent.com/twilight-project/dropin-miner/main/scripts/install.ps1 | iex
```

Or, if you use npm, install it globally and run setup yourself:

```bash
npm install -g dropin-miner
dropin-miner setup
```

### Coming from an earlier version

Nothing needs removing first. Run the installer again (npm: `npm install -g
dropin-miner@latest`, then `dropin-miner setup`). If your old installation used a
non-default home (you had `TOKENDROP_HOME` set for the old installer), run the
installer, or `dropin-miner setup`, with `TOKENDROP_HOME` set to that same one — setup
has no other way to find it. `TOKENDROP_HOME` is how you say "this directory is this
machine's installation". `setup -home <dir>` by itself, for any other directory, makes
a separate installation there: it leaves your shell profile and your coding agents
alone, even with `-yes`, because those belong to the machine's own installation, and
tells you the `agents install -config` command that sets agents up for the new one.
That is what makes `-home` safe for a disposable installation. One limit to know:
an agent has room for one dropin-miner skill, and it stays with the installation
that put it there. If your main installation already set an agent up, the second
one's `agents install` leaves that skill alone and tells you whose it is, so that
agent keeps searching through your main installation.

The installation in `~/.tokendrop` is used as it is: a directory holding an identity
is the installation, and nothing set aside is offered. Your existing wallet,
identity, search credential and recorded searches are kept; setup creates no
replacement for a healthy installation. `connect` still runs and resumes or repairs
its own onboarding under its normal rules: an unfinished registration is finished,
a lost or unreadable record beside a working key is rebuilt from the platform, and
an expired unclaimed registration is replaced. Your auth state afterward is not
promised to be byte-identical to what it was.

The mining question is not asked again if your old installation already holds a
decision: setup tells you whether mining is on or off for it. If it was interrupted
before that decision was made, you are asked.

Your config is parsed and, since the script's already has the `[platform]` and
`[miner]` tables, left byte for byte.

On macOS and Linux, the profile block uses the markers the script wrote, so you
never get a second block; setup's block quotes its paths where the script's were
bare, so the bytes can differ and you may be asked the profile question again —
saying yes replaces the one block in place. On Windows, your existing user PATH
entry and `TOKENDROP_CONFIG` are reused, not duplicated: the first new setup
records in `setup-env.json` whether an already-present PATH entry was added by it,
and what `TOKENDROP_CONFIG` held before, so a later `uninstall` only removes what
setup itself added and prints the rest for you to remove by hand.

Your agent integrations are reconciled to the current plan: one already correct is
left alone ("nothing to write: already set up"); stale files and hook entries are
updated, never duplicated; a host you never set up before is offered normally —
the usual case if you installed on Windows before v0.2.9, since that installer
only told you to run `agents install`. A second setup is idempotent for what
it owns — the files it writes, your profile or environment, and your agent
integrations — and keeps the same healthy identity; your connect-managed
authorization state may still advance.

Want a clean start instead? Move `~/.tokendrop` aside and take it back at the
**Use it?** question below.

Globally, not with `npx`: setup writes this binary's location into your agents'
skills and hooks, and a copy npm keeps only for one `npx` run, or inside one
project's `node_modules`, would disappear from under them. Setup refuses to run
from either and says so.

Setup asks, in order:

| it asks | what to know |
|---|---|
| **Use it? [Y/n]** | Only if a previous installation is set aside beside `~/.tokendrop` (one inside `~/.tokendrop` is simply used). Setup first says where it is and what it holds. Yes brings back your wallet, registration and key; the questions below that they answer are then skipped. |
| **Enable mining rewards? [y/N]** | A bare Enter answers no — search still works either way. Yes asks the next question now; changing your mind later is `dropin-miner mining enable` (or `mining disable` to stop). |
| **Payout address (leave empty to create a wallet here):** | Only asked after yes above. Paste a `twilight1…` address you control, or leave it empty for a wallet: it asks for a keyfile passphrase (**keyfile passphrase:**, then **again:** to confirm) before it prints the 24 words once. Have paper ready. |
| **Add them to ~/.zshrc? [Y/n]** | (or `~/.bashrc`, whichever your shell reads). Puts the binary on PATH, sets `TOKENDROP_CONFIG`, and — when a wallet was made here — `TOKENDROP_WALLET_DIR`, in one marked block. Saying no just means longer commands. On Windows the question is **Set them for your user? [Y/n]**: PATH and `TOKENDROP_CONFIG` only, in your user environment. |
| **Set up the coding agents found on this machine now? [Y/n]** | Writes a skill and, where the agent supports them, hook entries into its own config. Shown before anything is written, with each agent named beside what made it count as present — the command it is launched by, or its own config directory. An agent you have but do not see listed is one neither signal found; `setup -with <id>` sets it up anyway. |

Each of these counts only an answer you typed. If you interrupt one, or its
input closes, setup stops at that question rather than guessing: nothing is
recorded, nothing further is written, and it exits non-zero saying so. That
matters most at **Enable mining rewards?**, where a bare Enter means no — an
interrupt is not a bare Enter, and leaves no decision on file, so the next run
asks again instead of reusing one you never made. The same rule holds for
`dropin-miner agents install`'s **Proceed?**, for uninstall's confirmations and
for `wallet send`'s: a typed refusal declines and exits 0, an unanswered
question aborts and exits non-zero, and the two are never the same thing.

`dropin-miner setup -yes` answers yes to **Add them to ~/.zshrc?** (on Windows,
**Set them for your user?**) and **Set up the coding agents found on this
machine now?**, whether or not there is a terminal, so a script or CI job that
runs setup itself may pass it to set those up. The installers never add it:
run through them, you answer at the terminal. Without `-yes` and without
a terminal, setup leaves your profile and your agents alone and prints the
command for each. `-yes` answers **Use it?** only at a terminal: a set-aside
installation is never reused by a script. And it never answers **Enable mining
rewards?**, which is always yours. `-no-profile` leaves the shell profile (Windows: the
user environment) alone, and `-no-agents` skips coding-agent detection (`-with <id>` still
sets up a named agent even so) — `-yes` does not override either one. `dropin-miner setup
-dry-run` lists every file it would write or move and changes nothing.

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
                             # writable, recording (on Windows, also
                             # wallet access)
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

The skill each agent is given carries that command written for the shell
that agent actually runs, with the paths of your own installation already
quoted for it. Where the shell is PowerShell — opencode on Windows, Claude
Code's PowerShell tool, and Cursor on Windows unless your terminal profile
says otherwise — it is a here-string piped into the call instead, with a
first line that sets the output encoding:

```powershell
$OutputEncoding = [System.Text.UTF8Encoding]::new($false)
@'
{"version":1,"query":"what is proof of authority consensus"}
'@ | & 'C:\Users\you\.tokendrop\bin\dropin-miner.exe' search --stdin
```

That first line is what makes a query with an apostrophe, a quotation mark
or any non-ASCII character arrive exactly as written: without it, Windows
PowerShell 5.1 replaces every non-ASCII character with a question mark on
the way to the program, and the search answers a different question.

Cursor on Windows is the one host that gets both forms, because it runs
commands in whatever terminal `terminal.integrated.defaultProfile.windows`
names and that is your setting, not something this client can read. Its skill
labels the two blocks "If your terminal is PowerShell" and "If your terminal
is Git Bash"; use the one that matches yours. v0.2.10 taught the PowerShell
form alone, and on a Git Bash terminal the encoding line was expanded away
before PowerShell saw it, so a query went out mangled and the search
succeeded anyway.

That prints exactly one JSON object. Eight fields are always there —
`version`, `command`, `ok`, `exit_code`, `status`, `code`, `retryable` and
`action` — and they say what happened and what to do about it; `result`
carries the answer and its citations, and `mining` carries the mining state.
`exit_code` is the process's own status: 0 for a valid answer, 1 transport,
2 usage, 3 a 4xx, 4 a 5xx or a response the client could not use.
`status`, `doctor` and `connect` take `-json` and answer the same way.

The request may also ask for `"tier":"balanced"` (several providers,
attributed, instead of the default `fast`'s one), `recency`, `domain_filter`
or `max_results`, and for `"view":"merged"` — a deduplicated list of pages
across every provider that answered, each naming which ones found it, in
place of the full per-provider candidate list. `recency` and `domain_filter`
are preferences the router passes to its providers, so a result from another
date or host is not a fault; `max_results` is a cap. The skill your agent was
given teaches all of this; you never have to ask for it by hand.

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
miner stores none of it beyond the lineage files under
`~/.tokendrop/sessions` that the hooks maintain, one per workspace, or for
Cursor one per conversation.

One of those files is only ever read back by the agent that wrote it. If you
have an editor open at a repository root and another agent working in a
subdirectory, the second one's searches carry its own identity, not the
first's — a search that cannot say which agent it belongs to gets a plain
per-shell identity instead of borrowing the nearest session above it. Before
0.2.11 it borrowed, which meant one agent's narration could be sent as
another's.

Cursor's hooks also tell the search which session it belongs to, by putting
the session id on the search command itself. When that session id is there, a
lineage file is used only if it holds that same session; a shell that carries
no session id is served exactly as before. Each Cursor chat has its own
lineage file, so two chats open on the same project no longer relabel each
other's searches.

If one agent starts another as a shell command — Claude Code launched from a
Cursor agent's terminal, say — the inner one inherits the outer one's channel,
and its searches carry the outer session and the outer agent's label. That is
deliberate: the inner agent's search is work the outer session asked for.

To send no trace at all:

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
context compaction, and one on Stop that flushes. It also adds three
`permissions.allow` rules for the search command — the single-quoted, quoted
and bare spellings of the same command, because a shell may strip the quotes
— so Claude Code runs it without asking each time; nothing else the binary
does is allowed by those rules. Running `agents install` again replaces those
rules rather than adding more beside them, so the list does not grow each time
you reinstall or upgrade.

The hook that threads the search also answers the permission question, for
exactly the search command your own skill renders and nothing else. It has to,
because the hook is what causes the question: it prefixes the command with the
trace envelope, and an `allow` rule matches on how a command *begins*, so the
prefixed command no longer matches the rule that was installed for it. Without
that answer every search waits for approval — and in a headless session it is
refused outright, since there is nobody to ask. The rules stay for versions
and hosts that do not run the hook.

If you have Cursor as well: Cursor loads Claude Code's hooks from
`~/.claude/settings.json` and runs them beside its own. Those hooks recognize
that the caller is not Claude Code and do nothing at all, so a Cursor turn
ends with one flush instead of two and Cursor's command is never rewritten as
though Claude Code had sent it. Cursor still starts the three commands each
turn — that part is Cursor's — and each exits immediately.

**Cursor** gets a skill and seven entries in `~/.cursor/hooks.json`. Its
hooks keep one lineage file per conversation, and the `preToolUse` hook puts
that conversation's identity in front of our search, rewriting the exact
search the skill renders and nothing else, so the search reads the right
file. The shell hook also allows our command, so Cursor never prompts for
it. One exception, on Windows under a PowerShell profile: Cursor's hook
wrapper re-encodes non-ASCII text before our hook sees it, so a search whose
query is not plain ASCII is left exactly as the agent wrote it. It still
runs, but without Cursor's label, until Cursor fixes its wrapper.

**Codex** gets a skill, and — whenever a config is present — a small marked
block in `~/.codex/config.toml` that widens its sandbox just enough (and,
when it rewrites that block later, leaves it exactly where it already sits, so
nothing else in the file moves): network
on, and a short list of writable directories. Codex writes its own tables into
that file too — a project's folder trust, a `[windows] sandbox` choice — and
because it appends them at the end they can land between dropin-miner's
markers. Only the one table dropin-miner writes is ever removed from there:
anything else inside the markers is yours, is kept exactly as you left it, and
is named in the plan before anything is written. The state directory is always
on that list, because the detached claim resume writes there after every
search whether you mine or not; the intake, sessions and spool directories
join it when `[miner] enabled` is set, which is where the mining observation
is recorded. Deliberately those directories and
never the home itself: `tokendrop.toml`, `credentials.json` and `wallet/`
stay read-only to sandboxed commands, so a command that goes wrong inside
Codex cannot rewrite where your credentials are sent. Reading is a different
matter: a sandboxed command can read `credentials.json`, because a search needs
your key, and the state directory, because a flush needs it. On Windows it
cannot read `wallet/`, which keeps an access list of its own (see
[Where rewards land](#where-rewards-land)). Without the block,
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

Once those lines are under your own `hooks:` section, install sees them: it
says the hook is already set up and leaves your file alone, and `agents status`
counts it. Uninstall removes them again — those lines and no others. It takes
an entry out only when it is written exactly as dropin-miner writes it, names
this installation's binary and config, and sits under `pre_tool_call:` where
the four lines put it; if that entry was the only one there, the `pre_tool_call:`
line goes with it, and `hooks:` too if nothing else is left under it, so no
empty key stays behind. Anything it cannot be that sure of — an entry you
added a `timeout:` to, one with a comment between it and its heading, the
same entry twice — it leaves where it is and tells you, because a hook left
for you to delete is a smaller mistake than somebody else's hook deleted.

Hermes rewrites `config.yaml` in its own style when it saves it, and which
style depends on which save. `hermes config set`, the setup wizard and its own
`save_config` re-dump the whole file: comments go, so dropin-miner's markers go
with them, and the long command becomes a plain scalar folded over two lines.
A model switch, a setting changed in session and a personality change keep the
comments — markers and all — and fold the command inside them. The hook fires
either way, install and `agents status` see it either way, and uninstall
removes it either way: it recognizes both forms, undoes the folding, and takes
out the same lines it would have taken out before Hermes touched the file.

What it still will not do is edit a third form. The lines have to be the ones
dropin-miner writes, or the ones Hermes' own dumper writes, and to name this
installation; anything else — an entry you added a `timeout:` to, a command
written in a quoting style neither produces, the same entry twice — is left
where it is and reported with its line numbers. If your file names the hook
command somewhere uninstall cannot make sense of at all, it says so, with the
line, rather than reporting that Hermes is not installed.

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

## Known limits

Four things worth knowing before you go looking for a setting that is not
there. The first two are limits of Claude Code itself and the last is one of
Cursor's command-line agent; DropinMiner cannot work around any of the three.

**In Claude Code, the sentence right before a search does not reach the
trace.** If the model writes something and searches in the same message — the
usual shape — Claude Code only records that message after the search hook has
already run, so the hook cannot see the text. Narration in an earlier message
of the same turn does travel. Your search, your session and your rewards are
unaffected; it is only the text that goes with the trace (#93).

**In Claude Code on Windows, a search through its PowerShell tool asks for
approval each time.** The search itself works. The permission rules setup
writes only cover its Bash tool, and Claude Code's documentation does not
establish what a PowerShell rule would have to look like — a guessed one would
look installed and never match, which is worse — so none is written (#77). If
you have Git for Windows, the searches Claude Code sends through its Bash tool
are approved automatically; which tool it picks is up to the model.

**In the Cursor editor on Windows with a PowerShell terminal profile, a query
with accented or non-Latin characters once reached the router corrupted.** It
was seen once, on Cursor 3.20 on 2026-09-18: the router stored the query
double-encoded, so the search quietly answered a different question instead of
failing. On Cursor 3.21 on 2026-09-21, with the current skill, the same query
reached the router intact under both terminal profiles. A Git Bash terminal
profile was never affected (#117). If you want to rule it out, set Cursor's
terminal profile to Git Bash, or keep the query ASCII.

**On Windows, Cursor's command-line agent started from Git Bash cannot run a
search through the skill.** Cursor wraps each hook command in a PowerShell
script of its own and then runs that script with bash, which cannot parse it,
so every hook is rejected and every search with it. The Cursor editor is
unaffected, and so is the command-line agent started from PowerShell: start it
from PowerShell instead. The fix is Cursor's, and it has been reported to them
(#101; Cursor forum thread 172789).

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

## Upgrading

Run `dropin-miner upgrade`. It downloads the newest DropinMiner release from the
project's own GitHub repository, checks it, runs it once before installing it
and once after, and keeps the binary it replaced as `dropin-miner.previous`. If
something goes wrong before that, it puts the binary you had back and leaves
`dropin-miner.previous` as it was. In the rare case that putting it back fails as
well, it stops with `manual_intervention` and lists every copy that still
exists, and it deletes none of them. `dropin-miner upgrade
-rollback` puts the previous one back, without downloading anything, and running
it again swaps back. `-version X.Y.Z` picks an exact release, but never an older
one than you have. Installed with npm? Use `npm install -g dropin-miner@latest`
instead; `upgrade` will tell you so.

When the new binary is in place, `upgrade` has it write again the skills and
hooks you had already set up, so a fix in that text reaches your agents without
a second command. It adds no agent you had not set up, and it leaves alone, and
names, anything that belongs to another installation. If that step fails your
upgrade has still succeeded, and the message gives the one command to finish
it. Restart any agent that was open. If an agent still behaves like the old
version, `dropin-miner agents status` names any file an earlier version wrote
that this one would write differently, and `dropin-miner agents install`
refreshes it. One exception: an upgrade *from* 0.2.11 or earlier is carried out
by that older binary, which does not do this — run `dropin-miner agents
install` once afterwards.

If the new program is slow to answer the first time it is run — a virus
scanner inspecting a file it has never seen — the upgrade asks it once more
before giving up, and then says `retry` rather than calling the release bad.
It never asks twice about a wrong answer.

On Windows, if an older DropinMiner or agent process is still running, the
upgrade may stop with `previous_in_use` and put the binary you had back; close
those programs and run it again. If something only briefly holds the program
file — a scanner, an indexer — the upgrade waits about a second for it before
reporting a failure.

## Removing it, and coming back

`dropin-miner uninstall` takes out what setup put on this machine: the skills,
hooks and plugin in your agents that run this installation's binary, and the
shell-profile block (on Windows, the user PATH entry and `TOKENDROP_CONFIG`,
put back exactly as setup's record says, and only where they still hold what
setup set). Your wallet, registration, stored key, unsent searches and config
stay, nothing is revoked, and it tells you how to keep using them. Run it with
`-dry-run` first to see the list. Installed with npm? `npm uninstall -g
dropin-miner` removes the binary.

`-binary` also deletes `~/.tokendrop/bin/dropin-miner`, its
`dropin-miner.previous`, and anything an interrupted upgrade left beside it. You will also find
small `.lock` files — beside the installation, in it, and next to the binary. They are how
DropinMiner's commands avoid running over each other, they hold nothing once a command has
finished, and `uninstall` lists the ones still there and says they are safe to delete. On
Windows, which cannot delete a running program, it moves the binary out of the
way instead and tells you the file to delete later. `-purge-state` is the one that
destroys things: the wallet, your registration and key, unsent searches and
the config. It asks you, at a terminal, to type your wallet's address; `-yes`
cannot answer it, and neither can a script without a terminal. It tries
briefly to revoke this installation's authorization first and finishes either
way, saying which. Your approval on the platform is revoked only at the
console. If you made the wallet here, the 24 words you wrote down are the only
other copy, so do not purge until you have them or have moved the funds.

If you stop it — answer the address prompt wrong, say no to the plain
uninstall — the installation is left exactly as it was found. That includes the
lock files uninstall has to take to be sure nothing else is running: one it had
to create is removed again, one that was already there is left alone.

Coming back later, run the installer again, or `dropin-miner setup` — with
`-home <dir>` if this installation is not the default one. Setup is the way
back: it finds the state uninstall left and reuses it, the same agent and the
same wallet, with no new registration. Do not run `dropin-miner connect` on its
own to come back. Uninstall removed the profile block (on Windows, the user
environment entries) that named this installation, so connect would look at the
default location instead, find nothing, and register this machine anew.

Setup looks
for `~/.tokendrop`, or a set-aside copy beside it (`~/.tokendrop.bak-<date>`,
`~/.tokendrop.old`), tells you what it holds — the wallet's address, whether it
is enrolled, whether a key is stored, any unsent searches — and asks before
using a set-aside one. Saying no starts fresh beside it. Saying yes moves it
back in pieces that belong together:

- **Your registration and your stored key** travel as one. If `~/.tokendrop`
  already has a registration of its own, setup stops right there — nothing is
  moved and no new registration is made. It names both places; choose the one
  you mean to keep, move the other out of the folder they share, and run setup
  again. If
  `~/.tokendrop` only has the half-made key of a setup that stopped early, that
  is renamed aside (`state.unenrolled-<time>`), not deleted.
- **Your wallet** moves whole. If `~/.tokendrop` already has a wallet, it is an
  installation already and is simply used; nothing set aside is offered. A
  wallet folder there with no key in it, from a wallet that was never finished,
  is renamed aside (`wallet.incomplete-<time>`), not deleted, and yours moves in.
- **Unsent searches and session files** are merged in, one file at a time,
  never replacing a file already there.
- **Your config** moves only if `~/.tokendrop` has none.

Nothing that is a symlink is moved. The set-aside folder is deleted afterwards
only if it is empty; otherwise setup lists what it left in it.

Setup's shell-profile lines are one block between `# >>> dropin-miner >>>` and
`# <<< dropin-miner <<<` — delete the block to undo them. If the block has been
edited so that it no longer has exactly one start and one end line, setup will
not touch the file; it prints the lines to add by hand instead.
