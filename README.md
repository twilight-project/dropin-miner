# dropin-miner

Web search for coding agents that pays the person running the agent.

One binary. Drop it into Claude Code, Cursor, Codex, opencode, Pi or Hermes and their web
searches go through the Twilight search router: one query fans out across several search
indexes at once, the answers come back as one attributed list, and each search earns Twilight
Slot rewards to an address you control. No daemon, no proxy, no MCP server: between tool calls
nothing is running.

```bash
npm install -g dropin-miner
dropin-miner setup
```

Install it globally, never with `npx`: setup writes the binary's path into your agents' skills
and hooks, and npm throws away an `npx` cache or a project's `node_modules`.

Without Node, on macOS or Linux:

```bash
curl -fsSL https://raw.githubusercontent.com/twilight-project/dropin-miner/main/scripts/install.sh | sh
```

On Windows, in PowerShell:

```powershell
irm https://raw.githubusercontent.com/twilight-project/dropin-miner/main/scripts/install.ps1 | iex
```

Or download the archive for your OS and architecture from the
[releases page](https://github.com/twilight-project/dropin-miner/releases) and verify it
against that release's `checksums.txt`. Put `dropin-miner` in a directory only you can write,
such as `~/.local/bin`, and run `dropin-miner setup` from there. Setup and `agents install`
refuse a binary under the temp directory, or in a directory another user owns or every user
can write, and warn about one a shared group can write. On Windows only the temp-directory
rule is checked.

## How it earns

Twilight is a chain whose work is defined by Slots rather than by a puzzle. On the search
Slot, a web search served by the router is a unit of work: the chain pays the Slot, and the
Slot's verifier passes that share on to the people whose agents searched. Each search is
recorded on your machine and reported to the verifier after the fact.

- **One verified search per epoch makes you eligible**, and the epoch's share is split equally
  among everyone eligible. More searches do not earn more.
- **Your first reward takes one to two hours.** You join an epoch two ahead, and it has to
  close and settle first.
- **A long idle gap can miss an epoch.** Recorded searches are submitted by the next search or
  agent session, and evidence that arrives after the epoch's deadline is late.
- **Rewards are testnet TWLT**, paid to a `twilight1…` address you control. A wallet that
  setup creates is sealed by the passphrase you choose, and its 24-word recovery phrase is
  printed once.

The verifier receives metadata only: an account, a time, a reference number. It never sees
the query, the results or the agent's text. How the chain and its Slots work is at
[docs.twilight.org](https://docs.twilight.org).

## What setup asks

Setup registers this installation with the search platform and stores the key it gets back,
so there is no key to copy or paste. Then it asks, in this order:

| question | what your answer does |
|---|---|
| **Use it?** | Asked only when an earlier installation was set aside beside `~/.tokendrop`. Yes moves it back. |
| **Enable mining rewards?** | Yes enrolls this installation in Twilight testnet mining and asks for a payout address. No gives you search alone. |
| **Payout address** | Only if mining is on: a `twilight1…` address you control, or empty to create a wallet here. |
| **Add them to ~/.zshrc?** | Or whichever profile your shell reads. Puts `dropin-miner` on PATH and sets `TOKENDROP_CONFIG`. On Windows it asks **Set them for your user?** instead. |
| **Set up the coding agents found on this machine now?** | Writes a skill and hooks into each agent's own config, after showing you what. |
| **Switch?** | Asked only when Codex's config has a setting of your own that its permission profile would change. It shows every line it would write; a bare Enter is no and leaves Codex as it is. |

After the mining questions, and before the shell profile, it prints a link where you claim
your agent on the search platform and waits up to three minutes for you to. You need not do it
then: search already works, and using an agent later finishes the claim. The
[participant guide](docs/PARTICIPANT.md) says what each answer does and which flags answer
them for you.

## Then check

```bash
dropin-miner status          # what this installation has and has not completed
dropin-miner doctor          # whether searches are recorded and earning, check by check
dropin-miner agents status   # which agents are set up
dropin-miner search -format model "what is proof of authority consensus"
```

It is working when the search prints results, `agents status` lists your agents as installed,
and `doctor` names nothing to fix. Restart any agent that was already open.

## What a search returns

The router runs the request against the indexes and answers once, with everything they
returned:

- **`result.merged`**: the router's own list of the pages every index that answered returned,
  one entry per page, best first. Each entry carries `found_by`, the indexes that found that
  page, and `best_rank`, the best position any of them gave it. Agreement between independent
  indexes is the fan-out's own evidence, and the skill tells the agent to read this list first.
- **`result.indexes`**: each index the router asked and how it answered, so a search where an
  index failed or the deadline cut it short reads as degraded rather than as an empty web.
- **`result.candidates`**: each index's answer as it came back, so a disagreement can be
  inspected rather than averaged away. Only `"view":"full"` carries it.
- **`decision`**, which indexes ran; **`usage.cost_micros`**, what the search cost, in
  millionths of a dollar; **`latency_ms`**, for the search and for each index;
  **`request_id`**, so a search can be found again.

Agents call `dropin-miner search --stdin` with one JSON request and read one JSON object back,
so the query never touches the command line. A person can type
`dropin-miner search "<query words>"`, which does put it there.

## Three tiers

Every search runs in one of three tiers. A tier is how wide the search fans out, and also a
per-query cost ceiling on the router: a set that would exceed the ceiling is trimmed rather
than refused, with `decision.trimmed` naming what went.

| tier | what runs | for |
|---|---|---|
| `fast` | three fast indexes, one per index group | a routine lookup, the default for a new key; about a second |
| `balanced` | the core set, 8 results each, a 12-page merged window | when the agent needs more than one source, or the first answer came back thin |
| `max` | the core set, 15 results each, a 24-page window, longer excerpts | corroboration: the same page found by independent indexes |

A search is free while mining is on: the trajectory it carries pays for it. A key without
mining draws credit. `max`, and a request that names its own `providers`, always draw credit
and earn no reward.

Who chooses:

- **You**, ahead of time: `/dropin-miner tier balanced` in an agent, or
  `dropin-miner agents tier balanced` from a shell, sets the tier every search carries unless
  a request names one. `tier default` unsets it.
- **The agent**, one step at a time: the skill tells it to leave `tier` alone so your tier
  applies, to ask for `balanced` only when it needs several sources, and never to send `max`
  on its own.
- **The router**, when nobody did: it applies the default for your key.

The request also takes `recency`, `domain_filter`, `max_results`, `merged_limit`, `max_chars`
and `providers`. The [skill](cmd/dropin-miner/skill.md) is what the agent reads, and it has
every field.

## The trajectory

A query on its own says little about whether a search was any good. Each search therefore
carries a small `trace` that lets the router group the searches of one task and see what the
agent was doing when it asked. The trajectory is what a reward is paid for.

| field | carries |
|---|---|
| `harness` | Which agent: `claude-code`, `codex`, `cursor`, and so on. |
| `session_id`, `turn_id`, `call_id` | The agent's ids, hashed with SHA-256 before they leave the machine. A subagent has a session id of its own, and from Claude Code, Codex and opencode it also carries its parent's. |
| `window`, `seq` | Which context window of the session, after compactions, and a call counter. |
| `history` | The assistant text just before the search, scrubbed of secrets, last 32 KiB. Codex and Hermes send none. |

With `[miner] turn_end = true`, the agent's end-of-turn hook also sends what the turn
concluded: the final message, what you asked, the sequence of tools called with name, duration
and outcome (never a tool's input or output), the model and the token counts. Only for turns
in which a search was served.

The scrub removes anything shaped like a credential, PEM blocks, email addresses, the account
name in a home path, an `Authorization` header's credential, and any value under a name like
`password`, `token` or `api_key`. The hooks keep a lineage file per workspace under
`~/.tokendrop/sessions`, so the searches of one task line up across context compactions and
subagents.

`export TOKENDROP_TRACE=off` sends no trace and no turn end. Searches work the same without
them, and earn nothing.

## The agents it sets up

| agent | what is written | one thing to know |
|---|---|---|
| Claude Code | a skill; hooks and allow rules in `~/.claude/settings.json` | On Windows, a search through its PowerShell tool asks for approval. |
| Cursor | a skill; hooks in `~/.cursor/hooks.json` | Cursor also runs Claude Code's hooks, which stand aside for it. |
| Codex | a skill; on macOS and Linux, hooks in `~/.codex/hooks.json` and a permission profile in `~/.codex/config.toml` that opens only the search hosts | Its hooks run only once you approve them in Codex. Codex 0.158.0 or newer. |
| opencode | a plugin, and a block for you to paste into `AGENTS.md` | `/dropin-miner off` does not reach it. |
| Pi | a skill and an extension under `~/.pi/agent/` | A resumed session keeps its place in the trace. |
| Hermes | a skill; a hook in `config.yaml` | Approve the hook once; it loads next session. |

Any other agent that can run a command works too: `dropin-miner agents install` prints what to
paste.

## Commands

In the agent:

| command | what it does |
|---|---|
| `/dropin-miner <question>` | One search. The query goes as JSON on stdin, the merged list comes back, the agent answers from it. |
| `/dropin-miner off` | The agent's built-in web search becomes the default again; this one runs only when you name it. |
| `/dropin-miner on` | This search is the default again. |
| `/dropin-miner status` | Which of the two is the default right now. |
| `/dropin-miner tier fast\|balanced\|max\|default` | Sets or unsets the tier every search carries. |

The settings are yours: the skill tells the agent never to change the default or the tier on
its own. Searches that do not go through the router earn nothing.

In a shell:

| command | what it does |
|---|---|
| `dropin-miner search` | One search. `--stdin` for agents (JSON in, JSON out); `<query words>` for a person. |
| `dropin-miner setup` | Everything after the binary: previous installation, config, registration, profile, agents. |
| `dropin-miner status` | What this installation has and has not completed. `-json` for one envelope. |
| `dropin-miner doctor` | Whether searches are recorded and earning, check by check, and the one thing to fix. |
| `dropin-miner mining enable\|disable` | Turns mining on, asking for a payout address, or off. |
| `dropin-miner earnings` | What the chain has paid to your payout address. |
| `dropin-miner wallet init\|address\|register\|balance\|send` | The wallet setup made, or one you make here. |
| `dropin-miner flush` | One pass of the mining plane by hand: join the open epoch, submit recorded searches. |
| `dropin-miner connect` | Registers with the search platform, prints the claim link, polls until claimed. |
| `dropin-miner limits` | This key's spend caps, rate windows and today's spend, read from the router. |
| `dropin-miner agents install\|status\|uninstall` | Sets up, reports or removes the skills and hooks of each agent. |
| `dropin-miner agents prefer on\|off\|status` | Which search is the agents' default. |
| `dropin-miner agents tier <tier>\|default\|status` | The tier every search carries. |
| `dropin-miner upgrade` | A native install: the latest release, verified before it runs; `-rollback` swaps back. |
| `dropin-miner uninstall` | Removes what setup wrote; `-binary` and `-purge-state` go further. |
| `dropin-miner version`, `dropin-miner help` | The version, and the full usage text. |

A command that reads the config takes `-config <file>`; `setup`, `upgrade` and `uninstall`
take `-home <dir>` instead.

## What leaves the machine

The trace and the turn end go only to the search router, inside the search request and the
turn record. An agent's query goes in the JSON on stdin, so it is in neither `ps` nor a shell
history. The human form, `dropin-miner search "<query words>"`, puts the query on the command
line, where both can see it. The rewards service receives metadata only. Your wallet's
recovery phrase and passphrase never leave the machine.

## Upgrading and removing

```bash
dropin-miner upgrade                 # a native install: the latest release, verified before it runs
npm install -g dropin-miner@latest   # an npm install is updated with npm
dropin-miner uninstall               # remove the skills, hooks and profile block; keep your wallet
```

Once a day, when an agent session starts or ends, dropin-miner checks for a newer release, and
`dropin-miner status` says what it found. A native install replaces its own binary with it,
with the same verification as `upgrade`, unless the config says `[update] mode = "notify"`.
An npm install is only ever told. `mode = "off"` stops the check.

Coming from an earlier version, nothing needs removing first: install again and run
`dropin-miner setup`. It finds the installation under `~/.tokendrop`, or under
`TOKENDROP_HOME` if you set one.

## Building

```bash
make build   # the binary, in ./bin
make test
```

## Documentation

- [Participant guide](docs/PARTICIPANT.md): setup in detail, the wallet, earning, upgrading,
  removing.
- [CHANGELOG.md](CHANGELOG.md): what changed in each release.
- [Twilight docs](https://docs.twilight.org): the chain, Slots, rewards and the verifier.

## License

Apache-2.0.
