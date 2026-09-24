# Changelog

One heading per tag, newest first, each beginning `## vX.Y.Z — YYYY-MM-DD`. There is no
catch-all section for work that has merged but not yet shipped: it belongs under the
heading of the release it will be cut as, written by that release's own PR. Every tag
from v0.2.0 on has a heading here, dated. See `docs/RELEASING.md`
for how a release actually gets cut. `goreleaser`'s auto-generated changelog (from
`git log` between tags) already covers the mechanical commit-by-commit record — what
belongs here is the handful of things a participant or operator should be told in plain
language, that a list of commit subjects wouldn't make obvious on its own.

An entry describes the release it sits under, as that release behaved. A later release
superseding something does not make the older entry wrong, and older entries are not
rewritten to match newer behaviour; the newer entry says what changed.

## v0.3.0 — 2026-09-24

0.3.0 is a declaration, not a change. The setup, uninstall and native
upgrade lifecycle that 0.2.9 shipped for field validation has completed
live acceptance, and this release establishes it as the supported 0.3
line. The 0.2.9 soak ran on macOS and Windows against the published
release, through the public installers and npm (#57). The native
`upgrade` has since been exercised live, on real installations, into
every release after it: on macOS into 0.2.10, 0.2.11, 0.2.12 and
0.2.13, and on Windows into 0.2.10, 0.2.11 and 0.2.12, on a desktop
with real-time antivirus protection on throughout; `upgrade -rollback`
put the previous release back with the network off on the 0.2.11 and
0.2.13 acceptances. `setup` and `uninstall` ran through the soak's
install and removal rows and again in the 0.2.11 and 0.2.12 release
checks. The upgrade's re-rendering of the host integrations it owns was
seen live in both directions: in the rollback direction on the 0.2.12
acceptance and in the forward direction on the 0.2.13 one. Pi and
Hermes have been run live on Windows, in the 0.2.9 soak, and Hermes
again in the 0.2.11 and 0.2.12 checks, which discharges the exception
carried since v0.2.6. The binary carries no change over 0.2.13.

Coming from 0.2.13: run `dropin-miner upgrade` on a native install, or
`npm install -g dropin-miner@latest` on an npm install. The upgrade
renders again the host integrations it owns, so once it has succeeded
your skills and hooks are already current and there is nothing to run
by hand. Your wallet, identity, credential, recorded searches and
config are kept, and `upgrade -rollback` puts the previous version back
with no network. Coming from earlier: the older entries below apply
first.

This release closes the 0.3.0 epic (#50): native `setup`, `uninstall`
and `upgrade` behind one install registry, built across PRs A to D and
validated across five releases. It closes the 0.2.9 soak (#57): its
findings became the 0.2.10 through 0.2.13 fixes, and what remains open
from the field is Cursor's, listed below.

The 0.2 line is complete; the next release of the search client is
announced separately.

- **Stated exceptions.** The Cursor identity on the command has been
  seen end to end at the router from Cursor's command-line agent on
  macOS, on the 0.2.13 check and again on the 0.2.13 acceptance, but
  not yet from the Cursor editor; that row, and the Windows half of the
  0.2.13 upgrade acceptance, run when the Windows team is back. Whether
  Cursor reuses one PowerShell process across conversations is
  unmeasured; if it does, a search that was not rewritten in a later
  conversation could carry an earlier one's identity, on Windows under
  a PowerShell profile. Three Cursor issues are reported upstream and
  are Cursor's to fix: its sandbox prompt on a session's first search
  (#135), its command-line agent launched from Git Bash on Windows
  (#101), and a PowerShell-profile query corruption seen once on 3.20
  and not reproduced on 3.21 (#117). One finding of ours is open and
  is not fixed in the 0.2 line: a rollback across a version that added
  a hook event leaves that event's entry in place, inert, until the
  next upgrade — seen on the 0.2.13 acceptance when 0.2.12 was put
  back and Cursor's `preToolUse` entry stayed (#142). The upgrade
  acceptance from 0.2.13 into this release runs after this tag, so
  this entry does not claim it.

## v0.2.13 — 2026-09-24

0.2.13 closes what the 0.2.12 checks found. Cursor searches finally
carry Cursor's identity, so they are attributed to the conversation that
made them rather than arriving as a bare command-line search. Two Cursor
conversations on one workspace no longer share a lineage file. Text that
Cursor's Windows hook wrapper double-encodes is read back as it was. And
the last two lock-list wordings from the 0.2.12 checks are fixed. It is
smaller than 0.2.12 on purpose.

Coming from 0.2.12: run `dropin-miner upgrade` on a native install, or
`npm install -g dropin-miner@latest` on an npm install. This is the
first upgrade carried out by a binary that renders again the host
integrations it owns, so once it has succeeded your skills and hooks are
already current and there is nothing to run by hand; the upgrade's own
output lists what it refreshed. Your wallet, identity, credential,
recorded searches and config are kept, and `upgrade -rollback` puts the
previous version back with no network. Coming from earlier: the older
entries below apply first — and an upgrade from 0.2.11 or earlier still
needs `dropin-miner agents install` once after it, because that older
binary carries out the upgrade and touches no host file.

- **Cursor searches carry Cursor's identity.** Cursor applies the
  environment a session-start hook exports to its later hooks, not to
  the shell its agent runs commands in — measured on both platforms, and
  what Cursor's documentation says — so every Cursor search used to
  reach the router as a plain command-line search. Now a `preToolUse`
  hook puts the conversation's identity in front of the exact search the
  skill renders, and nothing else, in that shell's own syntax. The shell
  hook allows exactly that form and nothing looser: another session's
  values, an extra assignment, or the prefix on any other command is
  left to Cursor to ask about. `agents install` writes this as the
  seventh entry in Cursor's `hooks.json`.

- **One lineage file per Cursor conversation.** The session-start hook
  now keys the file by workspace and conversation, so two conversations
  open on one workspace no longer take turns relabelling each other's
  searches. A conversation begun on 0.2.12 keeps working: its hooks go
  on writing the file its session declared, which is the one its
  searches read.

- **Text Cursor's Windows wrapper double-encoded is read back as it
  was.** On Windows PowerShell 5.1, Cursor's hook wrapper reads the
  payload in the ANSI code page and encodes it again, so non-ASCII text
  reached our hooks doubled — `é` as `Ã©`, an em dash as `â€”`. The
  assistant and reasoning text the hooks store is now repaired when it
  has exactly that shape, cp1252's characters included, and each repair
  is reported on stderr. The command is never repaired: it is handed
  back to Cursor to run, and a guess there would change the search, so
  under a PowerShell profile on Windows a search whose query is not
  plain ASCII is left exactly as written and runs without Cursor's
  label. Reported to Cursor.

- **The skill says when a search did not run.** When `ok` is false, or
  the command could not run at all — blocked, sandboxed, refused, or no
  JSON came back — the agent tells the user the search did not run and
  stops, never answering the question as if it had. This comes from a
  Cursor 3.21 measurement in which a sandboxed first attempt failed and
  the agent answered anyway.

- **Uninstall's two lock lists agree with the disk.** The list of what
  remains after a default uninstall names only files that exist; it used
  to name `flush.lock` whenever its path could be predicted, while the
  summary in the same output correctly left it out. The "safe to delete"
  summary now names all seven lock files, the refresh-token lock and the
  wallet's lock included.

- **Documented.** Known limits gain Cursor's command-line agent launched
  from Git Bash on Windows, which rejects every hook and so every search
  through the skill, because Cursor runs its PowerShell hook wrapper with
  bash: launch it from PowerShell instead; reported to Cursor. The note
  on a corrupted non-ASCII query under a Cursor PowerShell profile now
  says what was measured: seen once, on Cursor 3.20, and intact on 3.21
  with the current skill. `docs/PARTICIPANT.md` describes Cursor's seven
  hooks and the one exception above.

- **Under the hood.** CI's actions moved to their node24 releases, still
  pinned to commit SHAs, and the Go module cache restores again on every
  runner. `docs/RELEASING.md` says both workflows are pinned, and why.

- **Deferred.** The Cursor PowerShell-profile corruption, seen once and
  not reproduced: #117. Cursor's command-line agent launched from Git
  Bash on Windows, Cursor's to fix: #101. Cursor's sandbox prompt on a
  session's first search, Cursor's to fix: #135. The goreleaser action's
  node24 bump, which only a tag push runs and so is proved on a fork tag
  first: no issue.

- **Stated exceptions.** The Cursor identity on the command has not been
  seen end to end from a Cursor editor: CI proves the rendering and the
  recogniser, one Cursor command-line search on macOS proves the
  router's label, and the Windows editor row runs when that team is
  back. Whether Cursor reuses one PowerShell process across
  conversations is unmeasured; if it does, a search that was not
  rewritten in a later conversation could carry an earlier one's
  identity, on Windows under a PowerShell profile. Pi and Hermes have
  been run live on Windows; on macOS and Linux they still rest on
  reading each host's own source, not a live run. The upgrade acceptance
  from 0.2.12 into this release runs after this tag, so this entry does
  not claim it — and it is the one that proves the re-render forward.

## v0.2.12 — 2026-09-19

0.2.12 is the cleanup the 0.2.11 release check and upgrade acceptance
asked for, and the first release carrying search-experience work. An
upgrade now refreshes the agent integrations it owns, so a fix that
lives in rendered skill or hook text reaches a host without a second
command. Two installations on one machine stop overwriting each other's
files. Hermes' own rewriting of its configuration is understood in every
form its writers leave. Hosts that run each other's hooks stand down.
And a search can say what it wants and read back what it got: the
request options the router accepts, what the router decided and what the
search cost, and one merged list of pages across every provider that
answered.

Coming from 0.2.11: run `dropin-miner upgrade` on a native install, or
`npm install -g dropin-miner@latest` on an npm install, and then run
`dropin-miner agents install` once — for the last time. The upgrade into
this release is carried out by the 0.2.11 binary, which replaces the
program and touches no host file, and most of what changed for a host
this release is rendered text; from 0.2.12 onward the upgrade refreshes
the integrations it owns by itself, so the next release will not ask for
this. Your wallet, identity, credential, recorded searches and config
are kept, and `upgrade -rollback` still puts the previous version back
with no network. Coming from earlier: the older entries below apply
first.

- **An upgrade refreshes the agent integrations it owns.** Once the
  replacement has succeeded — and only then, so an upgrade that is put
  back leaves them as they were — the new binary renders again the
  skills and hook entries this installation already set up. It sets up
  no host that was not set up, and leaves, by name, anything belonging
  to another installation or naming none. A failure never fails the
  upgrade: the binary is in place, and the line says so and gives the
  one command that finishes the job. `-rollback` does the same with the
  binary it restores. And `dropin-miner agents status` now names any
  file an earlier version rendered that this one would write
  differently, which is the diagnosis that did not exist before — a
  skill of this installation's included when it names a binary at
  another path, since for a file a host has only one of it is the config
  that decides whose it is.

- **Two installations on one machine leave each other's files alone.** A
  host has one skill directory, and opencode and Pi one adapter file
  each. A second installation's `agents install` used to overwrite the
  first's silently — the damage was done there, rather than in the later
  uninstall that correctly removed what by then read as someone else's.
  Install now leaves a single-slot file that is another installation's,
  says whose it is, and goes on with the rest of that host; `agents
  uninstall` asks the same question the same way, and `agents status`
  says which installation a host belongs to instead of calling it
  installed. Codex's sandbox block is one of these files too, and it
  names directories rather than a config, so it is attributed by the
  four an installation's config names — `state_dir`, `spool_dir`,
  `intake_dir` and `sessions_dir`. Another installation's block is left
  and named; this installation's is refreshed wherever those directories
  happen to live, rather than only under its own home.

- **Claude Code's allow rules stop accumulating.** A rule naming this
  installation's binary and config is one rule in any spelling this
  client has ever written, so superseded spellings are replaced rather
  than added beside, and a duplicate collapses to one. A rule used to be
  added whenever its exact text was absent, which left every earlier
  spelling in place for ever. Another installation's rules are
  untouched, and a set already equal to the current one is left exactly
  as it lies, so a second install still writes nothing.

- **Codex's config keeps its order.** dropin-miner's block is rewritten
  where it stands, and appended only when there is none, so a reinstall
  moves no byte outside our own markers. Every write used to strip the
  block and append it, reordering a file for a change that was only ever
  to our own table. An uninstall followed by an install restores the
  file byte for byte when the block was last, and otherwise moves no
  line of yours relative to another. Tables Codex added inside our block
  are still kept.

- **Lock files are named, not deleted.** `uninstall` now lists every
  lock file DropinMiner commands coordinate through that is still
  present — the lifecycle gate, `setup.lock`, `connect.lock`,
  `flush.lock` and the binary's update lock — in both modes, and says
  each is safe to delete. `-dry-run` prints that same section, from the
  files present when it runs. They are named rather than removed because
  an ordinary operation cannot remove its own lock file: unlinking one
  is safe only while the gate is held, and an operation has released the
  gate by the time it holds its own lock, so taking the gate back at the
  end would reverse the one lock order.

- **Hosts that run each other's hooks stand down.** Cursor loads Claude
  Code's `settings.json` and runs those hooks with its own payload. The
  Claude Code hooks now recognise a caller that is not Claude Code from
  the payload alone — never from the environment — and write nothing,
  spawn nothing, print nothing and exit 0. That is one flush per Cursor
  turn instead of two, and a Cursor command is never rewritten with a
  `claude-code` trace bridge.

- **A search believes its host's channel, not whatever variable it
  finds.** A host that exported the lineage file has declared that file
  as its trace channel and writes no bridge, so a bridge variable found
  beside it is dropped unread — never decoded, and never a fallback when
  the declared file is missing — and reported on stderr only, because
  the machine envelope is what the model reads and a model is one of the
  ways a foreign bridge arrives. With a session id exported, a lineage
  file is used only when it holds that session. A host started by
  another host as a shell command inherits the outer host's channel, and
  so its session and label: that is chosen rather than overlooked,
  because the exported environment is the declaration and the inner
  search is work the outer session asked for.

- **Nested workspaces, and a leftover temporary file.** A same-host
  search from inside a nested workspace no longer adopts the inner
  workspace's session: with a session id exported, the walk climbs past
  a lineage file of another session to the searching session's own. And
  a lineage write that fails removes its own temporary file, while stale
  ones another process left behind are swept.

- **Self-update on a loaded or scanned machine.** A new binary slow to
  answer its version check is asked once more, and only on a timeout — a
  wrong version, a malformed line, a byte on stderr or a failure to
  start is evidence, and is refused on the first answer. Two timeouts
  report `retry` rather than calling the release bad. On Windows the
  move-aside now waits about a second, over five attempts, for a file
  something else is briefly holding, instead of failing on the first
  sharing violation.

- **Hermes: every form its own writers leave is understood.** Hermes
  rewrites its own `config.yaml` in two styles — a whole-file re-dump
  that drops our markers, and a round trip that keeps them and folds the
  command onto a second line — and after either, `agents status` counts
  the hook, `agents install` reports it already set up and writes
  nothing, and `agents uninstall` removes the entry. A block belonging
  to another installation, holding a line we did not write, or followed
  by something Hermes added inside our `hooks:` mapping, is left and
  named with its lines, never cut. The by-hand step uninstall used to
  end in is gone for every form Hermes' own writers leave; it remains
  only for an entry this client will not edit.

- **Search: the request options the router accepts.** A `search --stdin`
  request may now carry `tier`, `recency` (`day`, `week`, `month` or
  `year`), `domain_filter` (up to 16 bare hostnames), `max_results` (1
  to 25) and `view`. A malformed value, an empty list or an explicit
  null is answered with `fix_input` before any router call is made, so a
  caller fixes its request instead of paying for a search that was never
  the one it meant.

- **Search: what the router decided and what it cost.** The envelope now
  carries the router's own `decision` — which tier ran, which providers
  it dispatched, and which it dropped for cost — and its `usage`: what
  the search cost the network in millionths of a dollar, whether it was
  served from cache, how many arms are still running, and the latency.
  Each candidate carries its own cost and latency too. In the terminal
  the providers and the cost are printed on one line, and nothing is
  printed when the router sent neither block: a zero cost is never
  invented for one the router never stated.

- **Search: one merged list across providers.** `result.merged` is every
  answering provider's pages deduplicated by URL identity, each page
  naming which providers found it (`found_by`), where each of them cited
  it (`found_in` — one candidate-and-citation position per provider,
  into the result the router stores under `request_id`) and its best
  rank; pages are ordered by how many providers found one, then by that
  best rank. It is present in every view, so an agent may always read it
  first. `"view":"merged"` drops the per-provider candidate list and
  keeps everything else, which is what makes those positions worth
  carrying: without them nothing maps a page back into the stored
  result. A citation whose URL names neither a host nor a path is not a
  page and is dropped.

- **The skill teaches what is now true.** Fast is the default and
  answers from one provider; balanced is for when the user needs to see
  several sources. Each option is named with one example, and `recency`
  and `domain_filter` are stated as preferences the router passes to its
  providers rather than restrictions it enforces: a page from another
  date or another host coming back is not a failure to report, while
  `max_results` is a cap and is applied. `merged` is what to read first,
  and `"view":"merged"` is described as dropping the providers' own
  answer texts — ask for it when the pages are what matters. Cost is
  mentioned only if the user asks. The skill's frontmatter description
  is now encoded as a YAML scalar rather than quoted by the template
  around it, so a description carrying the examples' own quotes and
  colons is still valid YAML for every consumer that parses it.

- **The hint for a host without a skill directory is the skill's own
  block.** opencode's `AGENTS.md` note, and the line any unknown agent
  is given, now carry the same fenced search block the installed skills
  teach, rendered for the shell that host runs — the quoted heredoc on
  POSIX, the here-string with the encoding line on Windows. Before, they
  carried a bare command with the JSON request on the line below it,
  which nothing carried to stdin: a participant who pasted the two lines
  got no search at all.

- **Documented.** Known limits: a Claude Code session using its
  PowerShell tool gets no trace and a skill command it cannot run, and
  text the model writes in the same message as a search never reaches
  the trace — both are host limits with no workaround in this client. A
  warning for Cursor on Windows with a PowerShell terminal profile: a
  query containing non-ASCII characters can reach the router corrupted,
  so a search can quietly answer a different question; use a Git Bash
  terminal profile or keep queries ASCII while the cause is being
  measured. And `docs/RELEASING.md` no longer says to delete a tag that
  failed preflight: the tag rulesets refuse the deletion for everyone,
  an administrator included, so the failed version number is burned and
  the next patch version is tagged instead.

- **Deferred.** Cursor on Windows under a PowerShell terminal profile,
  where the query and the stored assistant text both reach the router
  double-encoded, the cause still under measurement: #117, #113.
  Cursor's exported environment not reaching the shell its agent runs,
  so a search carries no session — confirmed for the command-line agent
  on macOS, the editor half still to be measured: #118. Two Cursor
  conversations on one workspace sharing one lineage file and
  relabelling each other's searches: #109. The Cursor CLI launched from
  Git Bash on Windows blocking every search on its own hook wrapper:
  #101.

- **Stated exceptions.** This release's check ran on macOS only. Its two
  Windows-only rows — the Cursor editor hook log, and a live Hermes run
  — run with the Windows upgrade acceptance after the tag; until then
  CI's own Windows runners, the Hermes differential test against PyYAML,
  and the captured Cursor payloads stand for them. Pi and Hermes have
  been run live on Windows; on macOS and Linux they still rest on
  reading each host's own source, not a live run. The upgrade acceptance
  from 0.2.11 into this release runs after this tag, so this entry does
  not claim it — and it cannot prove the re-render above either. The
  re-render runs inside an upgrade, and the first upgrade carried out by
  a binary that has it is the one *from* 0.2.12.

## v0.2.11 — 2026-09-18

0.2.11 carries the rest of the fixes from 0.2.9's field validation. Two of
them reach installations that are working today: the one regression 0.2.10
shipped, where Cursor on Windows with a Git Bash terminal was taught a
command that mangled a non-ASCII query instead of failing on it, and a
permission fix for every Claude Code participant in manual permission mode,
or in a headless run, whose searches were prompted one at a time or refused
outright. The advisory v0.2.10 promised for its Windows wallet fix is
published as GHSA-w246-j75w-wh8g.

Coming from 0.2.10 or 0.2.9: run `dropin-miner upgrade` on a native install,
or `npm install -g dropin-miner@latest` on an npm install, and then run
`dropin-miner agents install` once. That second step is not optional this
time. The upgrade replaces the binary and touches no host file, and the
Cursor fix below lives in the skill text a host reads rather than in the
binary, so it reaches your hosts only when the skill and hook files are
rendered again. Your wallet, identity, credential, recorded searches and
config are kept either way, and `upgrade -rollback` still puts the previous
version back with no network. Coming from earlier: the older entries below
apply first.

- **Claude Code: a search no longer prompts or is refused.** The hook that
  rewrites a search to carry the trace bridge now answers the permission
  question for exactly the command the skill renders — the rendered search
  rebuilt and compared, naming this installation's own binary and config,
  nothing looser. Because the rewrite puts the bridge before the binary, no
  installed allow rule could match what actually ran: in manual permission
  mode every search asked, and a headless run refused it outright as
  obfuscated. Automatic mode was never affected — its classifier allows what
  the heuristic refused — which is why this went unseen for a release. The
  allow rules are still installed, for hosts and versions that do not run
  this hook, and a search this installation never rendered is still left to
  the permission system.

- **Cursor on Windows: the terminal is yours, so the skill teaches both
  forms.** Which shell Cursor runs is whatever the participant's terminal
  profile says, so the skill now teaches a runnable form for each and labels
  them by terminal — "If your terminal is PowerShell", "If your terminal is
  Git Bash". 0.2.10 declared that cell PowerShell alone, which is true of a
  default install and of the Agent CLI and false of an editor set to Git
  Bash: there bash expanded the PowerShell form's encoding line before
  PowerShell ever saw it, and `café 東京` arrived as `caf? ??`. The search
  succeeded and answered a different question, which is worse than one that
  cannot run at all.

- **A search carries only its own host's lineage.** The walk that looks up
  the directory tree for a session's trace file now adopts one only when the
  searching host has said what it is and the file names that same host. A
  host that writes no lineage of its own used to take whichever file it
  found above it: a Cursor CLI search in a subdirectory, and a plain
  terminal search in the same tree, both reached the router as Claude Code,
  carrying that session's id and its assistant text, and advancing its
  counter. Present identically in 0.2.9, on every platform.

- **Uninstall removes only this installation's integrations.** An agent
  integration is this installation's when it runs one of this installation's
  binaries *and* names this installation's config — both, not either. Two
  installations sharing one binary, which is what `setup -home` makes, each
  planned the removal of the other's hooks, skills and allow rules. What is
  not this installation's is now left in place and reported, saying whose it
  is.

- **Cursor is found however it was installed.** Detection answers with
  `cursor`, `cursor-agent` or `~/.cursor`, so the Agent CLI and an installed
  editor whose participant never added the shell command are both found, not
  only a `cursor` command on PATH. `setup` and `agents status` print which
  of the three was the evidence, and a host nobody found now reads "not
  found" instead of a claim about PATH.

- **`setup -home` no longer reaches past the installation it names.** This
  machine's installation is `TOKENDROP_HOME`, or `~/.tokendrop`; an explicit
  `-home` naming any other directory is a separate installation, and setup
  now leaves the shell profile and the coding agents alone for it, `-yes` or
  not, naming the steps it skipped and `agents install -config
  <home>/tokendrop.toml` as the command that sets that installation's agents
  up. Before, a disposable installation planned the real user's agents and
  environment against the scratch config. Relocating the real installation
  is done with `TOKENDROP_HOME`; a `-home` that names the default, however
  it is spelled or linked, still counts as the default.

- **Setup's questions abort on an interrupt.** Ctrl+C at "Enable mining
  rewards?", or at any question the binary asks, now aborts the operation:
  nothing recorded, nothing written, nothing sent, and a non-zero exit.
  Before, the absence of an answer was read as an empty line, an empty line
  is not "yes", and a mining decision the participant never made was saved
  and reused by the next run without asking.

- **Codex: uninstall keeps what Codex added inside our block.** Codex
  appends its own tables to the end of its config, which is where our block
  sits, so removal marker to marker took them with it — one tester's folder
  trust and sandbox choice, on the host whose sandbox settings decide
  whether a search records at all. Uninstall now removes only the one table
  this client writes, keeps anything else found between the markers and
  moves it to the end of the file, and refuses rather than cut when it
  cannot establish what a marked block contains.

- **Hermes: a hooks block that already holds our entry is not a refusal.** A
  config whose `hooks:` block already carried our entry was refused on every
  run, and because setup had not written it, nothing tracked it: it was
  never listed and never removed, while Hermes went on invoking it. Install
  now reports it as already set up and `agents status` counts it, including
  in the form Hermes itself writes when it saves its own config; uninstall
  removes the entry when it is exactly what this client writes, and
  otherwise names its lines and says how to remove them by hand. A file the
  scan cannot read reliably gets a warning and never an edit, and a `hooks:`
  block without our entry still refuses, in the same words as before.

- **Windows installer and lifecycle leftovers.** `install.ps1` now removes
  the download on every exit, instead of leaving an archive in `%TEMP%` that
  its own checksum step had just called untrustworthy. And an `uninstall
  -purge-state` or `-binary` that aborts removes the lock file its own run
  created, rather than leaving behind one that was never there; a lock the
  participant already had is never removed.

- **Output that tells the truth.** Each host's writes and removals are
  grouped under its own heading in the setup and uninstall plans, instead of
  a host with only removals printing its lines under the previous host's
  name. The hint after an uninstall names `setup -home`, which finds the
  state that was left and reuses the same agent and the same wallet, ahead
  of the bare `connect` that would register the machine anew. Generated
  config comments carry an ASCII dash. Setup's closing line names the steps
  it skipped rather than reporting that everything was already in place. And
  a flush that obtains authorization, or finds it already held, clears a
  stale authorization failure from `status` instead of leaving it for an
  unrelated delivery to clear.

- **Documented.** The wallet directory variable the profile block sets
  (`TOKENDROP_WALLET_DIR`), and the keyfile passphrase the wallet prompt
  asks for and what it protects.

- **Deferred to 0.2.12.** Hosts running each other's hooks, and a trace
  prefix written by another host's hook: #87, #91, in PR #107. Lineage
  inside a nested workspace, and a leftover temporary file: #104, #100, in
  PR #107. Two Cursor conversations on one workspace sharing a lineage file
  and relabelling each other's searches: #109, whose immediate guard is in
  PR #107 and whose proper fix is its own. Cursor searches reaching the
  router without their session, because the hook's exported environment does
  not reach the shell: #118. On a Cursor PowerShell terminal profile on
  Windows, the query and the stored assistant text both reaching the router
  double-encoded — inherited from 0.2.10, not a regression of this release:
  #117, #113. Self-update timing on a loaded or scanned machine: #95, #78,
  in PR #107. Cursor CLI launched from Git Bash on Windows: #101. Codex
  config order after a reinstall: #99. The empty update lock left after an
  upgrade, and a `connect.lock` left behind by a setup interrupted at the
  mining question: #103, #115. An upgrade leaving the host skill and hook
  files as the previous version rendered them, which is why the step above
  exists, a second installation's `agents install` overwriting the machine
  installation's skills, and `agents install` accumulating superseded allow
  rules for the same binary and config instead of replacing them: #111,
  #112, #114. Hermes once Hermes has saved its own config — status, the
  marked block's ownership check, and uninstall's by-hand step: #105, #106,
  #108. The Claude Code PowerShell tool and text written in the same message
  as the search, which are host limits: #77, #93.

- **Stated exceptions.** The Windows-desktop-with-real-time-antivirus
  exercise of the replacement transaction, promised in v0.2.9 and again in
  v0.2.10, has now run: on 2026-09-17, as the Windows half of 0.2.10's
  upgrade acceptance, a Windows 11 desktop with Microsoft Defender real-time
  protection on upgraded from 0.2.9 through the shipped updater, and the
  binary it installed was byte-identical to the release asset. Pi and Hermes
  have been run live on Windows; on macOS and Linux they still rest on
  reading each host's own source, not a live run. The upgrade acceptance
  from 0.2.10 into this release runs after this tag, so this entry does not
  claim it.

## v0.2.10 — 2026-09-17

0.2.10 is the stabilization release: the fixes from 0.2.9's field validation
that were ready are cut now, so every 0.2.9 installation can make its first
real native upgrade — through the shipped updater and the trusted-publishing
pipeline — before the remaining fixes land in 0.2.11. This is the first
release published to npm with a provenance attestation.

Coming from 0.2.9: run `dropin-miner upgrade` on a native install, or `npm
install -g dropin-miner@latest` on an npm install. There is nothing else to
do — wallet, identity, credential, recorded searches and config are all
kept, and a second `setup` is idempotent. `upgrade -rollback` puts 0.2.9
back with no network. Coming from earlier than 0.2.9: the v0.2.9 entry
below still applies first.

- **Every host is taught the command its own shell can run.** The skill's
  search command is now rendered per host and per OS: a quoted heredoc
  where the host runs Bash (macOS, Linux, Git Bash), and on Windows a
  single-quoted here-string piped into the call — for Cursor, opencode,
  Codex and Claude Code's PowerShell tool — with the line that keeps a
  non-ASCII query intact on Windows PowerShell 5.1. Claude Code on Windows
  is taught both forms, since a model can call either its Bash tool or its
  PowerShell tool. Codex on Windows is taught the PowerShell form too: a
  live run showed it runs PowerShell under a PowerShell-fenced skill and
  reaches for the WSL bash launcher, which fails, under a Bash-fenced one
  — the shell every host actually runs is now established, on every OS.
  The binary tolerates one leading byte-order mark on `search --stdin` and
  on hook input.

- **Cursor auto-allows exactly the search its skill teaches, and lineage
  follows it.** The shell hook now recognizes exactly the rendered command
  for Cursor's own shell on each OS — the exact binary, the exact
  arguments, one JSON body — and nothing looser; a human-typed search
  still asks.

- **Hook commands and trace prefixes are written for the shell that runs
  them.** Hook commands no longer carry Go-quoted paths that only a
  Bash-style shell can parse, and Claude Code's hook now fires for both of
  its shell tools. The trace bridge is written as a PowerShell assignment
  where PowerShell runs the command, and an adapter replaces a bridge it
  did not write rather than trusting one already in the command. Cursor's
  hooks on Windows keep the v0.2.9 form for now — no single form was
  proven to run under cmd, Windows PowerShell 5.1 and pwsh together — and
  `agents install` says its runner is not established there. A Claude Code
  search made through its PowerShell tool on Windows still prompts once:
  no permission rule is written for it yet.

- **Claude Code: earlier assistant text reaches the router with a
  search.** A skill's own injected text was mistaken for a new user turn,
  which floored the scan for the assistant's sentence one step too late
  and left every search made through the skill without it. The assistant
  text from an earlier message in the same turn now reaches the router
  with the search again. Text written in the same message as the search
  itself still does not: Claude Code writes that message's own entry
  after the hook runs, so there is nothing yet to read it from (#93,
  0.2.11).

- **Codex: the flush a search starts now runs inside the sandbox.** The
  flush lock stays at its one existing location for every binary, but a
  flush that cannot open it for writing — because the lock lives beside
  the config, outside the directories the sandbox lets a search write —
  now opens it read-only and takes the same exclusive lock instead of
  failing silently. The flush
  stamp moves under the state directory, which a sandboxed flush can
  write. A Codex-only participant's searches are delivered.

- **Windows: the wallet directory keeps its own owner-only access.** The
  wallet directory gets its own protected, owner-only permissions at
  creation, and every file written into it keeps that access — reapplied,
  recursively, by `setup` on an existing installation whose wallet had
  inherited broader access from its parent directory. `doctor`'s new
  `wallet access` check reports when another principal can read the
  wallet or a file in it, says the entry can come back, and names
  `dropin-miner setup` as the repair. On macOS and Linux a sandboxed agent
  runs as you, so the passphrase is what protects the wallet file there:
  one you use nowhere else, with the 24 words kept off the machine. No
  mechanism beyond that, this release — the advisory carries the rest.

- **Commands name the config they use, and `connect` refuses without
  one.** Every command resolves its config in the same order now —
  `-config`, then `TOKENDROP_CONFIG`, then `./tokendrop.toml`, then the
  installation's own config, then built-in defaults — instead of silently
  falling back to a default state location that isn't the installation
  actually running. `status` and `doctor` print which config they loaded,
  or say plainly that none was found; `connect` refuses outright when
  resolution finds no config file at all, and names `dropin-miner setup`.

- **Dry runs list what the real run does.** `setup -dry-run` now plans
  every host's install from the config the real run would write or
  migrate, instead of from no config at all, so its listing matches what
  actually gets written. `uninstall -dry-run` lists the flush lock a real
  purge's own locking would create, and nothing a real run never touches.

- **`doctor` tells the truth about recording and payout.** The `recording`
  check now judges suspicious activity against the epoch the rewards
  service reports as current, and a hook-triggered flush with nothing to
  deliver no longer counts as activity, so a healthy installation no
  longer reads `recording UNKNOWN` for part of nearly every epoch. When a
  payout binding is held, the `payout address` check now reports the
  hold — naming both the address currently active and the one this
  installation would declare, and the operator action to take — instead
  of reporting `OK`.

- **Released with provenance.** CI's third-party actions are pinned to
  commit SHAs, the Windows test matrix no longer cancels its siblings when
  one job fails, and the npm package is published through npm's trusted
  publishing with a provenance attestation instead of a long-lived token.

- **Deferred to 0.2.11.** Documentation and status: #60, #62, #75. Cursor
  detection and the Claude Code PowerShell rule: #61, #77. Lifecycle
  defects from the Windows validation: #73, #81, #82, #83, #84, #85, #86,
  #87, #88. The transient Windows upgrade sharing violation: #78. Trace
  lineage defects found after this cut: #91, #93.

- **Stated exceptions.** The Windows-desktop-with-real-time-antivirus
  exercise of the replacement transaction, stated in v0.2.9, still has not
  run — no such machine has been available. Pi and Hermes were run live
  on Windows during this release's field validation; macOS and Linux
  still rest on reading each host's own source, not a live run. The
  upgrade acceptance from 0.2.9 runs after this tag, so this entry does
  not claim it.

## v0.2.9 — 2026-09-14

0.2.9 is the field-validation release for the upcoming 0.3.0 line. It contains the new
setup, uninstall and native upgrade lifecycle so those paths can be exercised through the
real GitHub and npm distribution channels before 0.3.0 is declared ready. The next
release is the first real native upgrade acceptance from 0.2.9.

Coming from an earlier version: nothing needs removing first — run the installer again
(npm: `npm install -g dropin-miner@latest`, then `dropin-miner setup`), or `setup -home`
with the same `TOKENDROP_HOME` your old installer used, if it had one. The installation
in `~/.tokendrop` is used as it is — your wallet, identity, search credential and
recorded searches are preserved, and setup creates no replacement for a healthy
installation. The mining question is not asked again once a decision is on file; your
config is parsed and, since the script's already carries `[platform]` and `[miner]`,
left byte for byte; your shell profile or Windows user environment is reused rather than
duplicated; and your agent integrations are reconciled to the current plan rather than
rewritten. A second `setup` run is idempotent for setup-owned files, the profile or
environment, and the agent integrations, and keeps the same healthy participant
identity — connect-managed authorization state may still advance.

- **`setup` lives in the binary now.** The same questions as before, in the same order,
  and now also on Windows: a previous installation set aside beside `~/.tokendrop` is
  offered and moved back whole — identity and key together, wallet whole, unsent
  searches merged, config only if none is present — and only when a person says yes at a
  terminal. An existing config is migrated, never overwritten. The installation
  directory is made owner-only before anything is written into it. `setup -yes` answers
  the profile and agents questions and never the mining one; `-dry-run` changes nothing;
  `-no-profile` and `-no-agents` each skip their own step, and `-yes` overrides neither;
  `-with <id>` sets up a named target whether or not it was detected.

- **The installers fetch, verify and hand off.** `install.sh` and `install.ps1` download
  a checksummed release and run `dropin-miner setup`. A binary older than 0.2.9 has no
  `setup`, so the same scripts fall back to the old flow for it while such a binary is
  the latest release; from 0.2.9 on, a fresh install no longer takes that branch.
  `TOKENDROP_INSTALL_NO_SETUP=1` stops after fetching the binary and prints the next
  step by hand.

- **Installed through npm, it stays npm's.** Setup refuses to run from an `npx` cache, a
  project's own `node_modules`, or npm's binary run directly around its launcher — the
  supported route is a global `npm install -g dropin-miner`, then `dropin-miner setup`.
  `upgrade` and `uninstall -binary` refuse an npm-installed copy the same way and print
  the npm command to use instead.

- **`uninstall`.** The default removes only what setup put there: the coding agents'
  skills, hooks and plugins that run this binary, and the shell-profile block (on
  Windows, the user PATH entry and `TOKENDROP_CONFIG` — reverted only against setup's
  own record, and only while they still hold what setup set). Nothing is revoked and
  your wallet, registration, stored key, recorded searches and config stay; it tells you
  how to keep using them. `-binary` also removes the installation's own binary; on
  Windows, where a running binary cannot be deleted, it is moved aside instead and the
  path is printed. `-purge-state` is the one that destroys participant state, and it
  needs a terminal and the typed wallet address (or the installation path) — `-yes`
  cannot answer it. Before removing anything it tries, for at most eight seconds, to
  revoke this installation's authorization at the rewards service; the platform's own
  grant is revoked only at the console, never by a purge. `-dry-run` changes nothing.

- **`upgrade`.** Present from this release, though its first real use is the upgrade
  into the next one — until then it reports that 0.2.9 is already the latest. It fetches
  only from the canonical GitHub repository, with no participant credential, verifies
  the release by checksum, and runs the new binary both before and after it is
  installed, keeping the replaced one as `.previous`. A recoverable failure before that
  point restores the old binary byte-identical; if the restoration itself fails, the
  operation reports `manual_intervention` and lists every surviving copy rather than
  guessing. `-rollback` puts `.previous` back with no network; `-version X.Y.Z` never
  installs something older than what is running. When it fails, the first word after
  `upgrade:` says what to do — `retry`, `release_invalid`, `ownership`, `filesystem`,
  `lifecycle_busy`, `refused` or `manual_intervention`. The whole operation is bounded
  at three minutes. On Windows, a `.previous` a process is still running gives
  `previous_in_use` rather than a bare failure. The replacement transaction runs in CI
  on every runner, native Windows arm64 included.

- **Commands no longer cross each other.** `setup`, `connect`, `flush`, `uninstall` and
  `upgrade` now share one lifecycle gate beside the installation
  (`~/.tokendrop.lifecycle.lock`, safe to delete when nothing runs), with each
  operation's own lock behind it. A person's command waits five seconds for the gate,
  then refuses rather than risk crossing a destructive operation; a background child
  gives up quietly and records nothing. `connect -json` answers a held gate with
  `lifecycle_busy`, `retry_after_ms` and action `retry`; the envelope's mandatory header
  is unchanged, so `machineVersion` is not bumped.

- **On Windows, a second `agents install` no longer duplicates hooks, and `agents
  uninstall` now actually removes them.** The matching that decided whether a hook entry
  or a Claude Code permission rule was already ours compared raw substrings against text
  that quoting had changed on Windows paths, so the exact bytes never matched. It now
  matches the exact prefix each is written with instead.

- **`dropin-miner version` is now a release-compatibility contract.** It prints exactly
  `dropin-miner X.Y.Z` and nothing else, on stdout only; the updater accepts nothing
  else from a candidate before installing it. Anyone building the binary themselves for
  the updater to trust stamps it the same bare way: `-X main.version=1.2.3`, no `v`.

- **Groundwork, nothing shipped on it yet.** The install registry puts every installable
  surface — currently the same six coding-agent hosts as before — behind one interface,
  so a later integration installs by name instead of a new hand-wired branch. The
  registry itself adds no new host or participant-facing integration in this release,
  and no OpenRouter earning is advertised.

- **CI now runs the suite on Windows arm64 too.** A pull request shows eight checks, not
  seven: the `test` matrix across Linux, macOS, Windows and Windows arm64, plus `race`,
  `cross`, `lint` and `vuln`.

- **Stated exceptions.** Neither adapter has been live-smoked against its real host —
  Pi and Hermes are still covered only by tests that execute the installed artifacts,
  carried forward from v0.2.6 with no live run yet performed. Separately, this release's
  replacement transaction has not been exercised on a Windows desktop with real-time
  antivirus protection on — no such machine was available before this tag — so that
  proof is where the upgrade into the next release stands, not here.

## v0.2.8 — 2026-09-13

- **Releases are published by the repository now, not by hand.** Pushing an annotated
  `vX.Y.Z` tag is the whole of a maintainer's part in a release; everything after it —
  the six platform binaries, the GitHub Release, the npm wrapper — runs in CI. Which
  commit gets released is still a person's decision and deliberately stays one. What
  changed is the mechanical half, because that is where the mistakes actually came from:
  v0.2.1 and v0.2.2 both shipped with `npm/package.json` a version behind the tag they
  were cut as, and nothing noticed either time.

- **A tag is now refused before anything is built if it is not a release.** A tag whose
  commit is not on `main`, a tag that is not annotated, a tag whose commit's
  `npm/package.json` does not say the version the tag names, and a tag with no matching
  `## vX.Y.Z` heading in this file are each rejected at the start of the run: no GitHub
  Release is created and nothing reaches npm. The ancestry check is the one worth
  calling out to an operator — a valid-looking version tag pointing at some commit in
  the repository is not a release, and the ability to create a tag is not by itself the
  ability to publish under this project's name.

- **What gets published is checked against what should have been.** The GitHub Release
  is read back and required to carry exactly the expected assets — the six platform
  archives and `checksums.txt`, no more and no fewer — before the npm wrapper is
  published. The wrapper holds no binary of its own; it downloads one of those archives
  and verifies it against that `checksums.txt`, so publishing it against an incomplete
  release would ship a package that cannot install, in a version npm does not allow
  anyone to withdraw. Once published, the package is then installed from the registry
  into a fresh directory on Ubuntu, macOS and Windows, and the binary each install
  produces must report `dropin-miner X.Y.Z` for the release to pass. That is the only
  check in the chain that runs the released binary rather than comparing one version
  string to another.

- **A release's notes now open with a link to this file as it stood at that tag.** The
  GitHub Release body is a list of commit subjects, which is the mechanical record and
  worth keeping; it is not the handful of things a participant should be told in plain
  language. The two never pointed at each other, and the Release is where people
  actually land, since it carries the `Latest` badge and every installer points at it.

- **Annotated tags are the convention from this release on.** An annotated tag records
  who cut a release and when; a lightweight one is a bare pointer, and that record then
  survives only in the push event. Tags up to v0.2.7 are mixed and are left exactly as
  they are — retagging a published release would move refs that `install.sh`,
  `install.ps1`, `npm/install.js` and an already-published `checksums.txt` resolve
  against, for a cosmetic gain.

- **Operational note on the npm credential.** Publication for this release authenticated
  with an npm access token held as a **repository** secret in GitHub Actions, scoped to
  this package. It is a bridge, and it is being treated as one: the token is revoked
  once this release is out rather than left in place. npm Trusted Publishing, which
  removes the long-lived credential entirely, is the intended replacement and is not in
  place yet — package-side authorization for it is not available to this project today.
  None of this changes what is published or how it is verified; it is recorded here
  because a publishing credential's lifetime is an operator's business.

- **No client behaviour changed.** Nothing under `cmd/` or `pkg/` differs from v0.2.7:
  the whole difference between the two tags is the release workflow, the release tooling
  it runs, and the documents describing them. Upgrading from 0.2.7 to 0.2.8 gets a
  binary that behaves identically. This release is deliberately that shape — the first
  run of a new release process belongs on a release where nothing else can go wrong.

## v0.2.7 — 2026-09-13

- **`dropin-miner help` describes the binary that actually shipped.** 0.2.6 added the
  `search --stdin` machine protocol, the whole-search `-timeout`, and `-json` for
  `connect`, `status` and `doctor` — and the built-in help mentioned none of them. It
  now documents both search forms, names the real default timeout rather than a
  hand-typed one, names all six supported hosts, says that `connect -json` reports a
  required participant decision instead of prompting for one, and corrects the search
  exit codes: a 2xx the client cannot use is exit 4, not 0.

- **A lost or unreadable registration is rebuilt from the platform instead of
  refused.** An `agent.json` that had gone missing or would not decode, beside a
  platform key that still worked, used to stop `connect` outright and require a person
  to run `connect -force` — which mints a second agent for one participant. The search
  platform now answers `GET /v1/agents/me`, a self-lookup authenticated by the key this
  installation already holds, so the identity is rebuilt unattended from the platform's
  own record. Nothing local is touched until that lookup answers: a corrupt record is
  set aside only after a successful reply, and any failure leaves it byte-identical and
  refuses exactly as before, naming `-force`. `-force` skips the rebuild deliberately: it
  bypasses recovery and authorizes a deliberate replacement where local state would
  otherwise refuse a fresh registration — it means "replace", not "recover". It is not,
  and was not before this release, the only path to a replacement: an ordinary foreground
  `connect` may also replace a registration the platform has positively verified as
  expired, changing the platform agent and its key, and that path needs no flag.
  `-resume`, the detached background poll, neither registers, rebuilds nor replaces: a
  new identity is not something to decide in the background. A rebuilt registration that
  is still unclaimed but whose claim link the platform did not return prints a one-line
  notice rather than a link, and the poll's own timeout narration no longer tells anyone
  to approve a URL that was never printed.

- **`doctor` gains `intake writable` and `recording`, bringing it to seven checks.**
  Between them they answer the question the other five could not: searches succeed, and
  nothing is earned. `intake writable` runs one bounded probe operation using the real
  intake writer's own sequence — create the directory, publish one file, remove it —
  because a directory's mode bits do not settle whether *this* process can write in it.
  At most one ephemeral file, named so the flush's reader can never see it (`readIntake`
  considers only `.json`), in a directory this client already owns; cleanup is attempted
  after a failed publication as well as a successful one, and a leftover is reported by
  pathname. It is the one bounded probe operation `doctor` performs — three filesystem
  operations, each reported on its own when it fails, not "one write" — and it is
  disclosed here rather than left to be discovered.

  `recording` correlates recent mining-plane activity with what is in intake, in the
  spool, in quarantine, in the capture health record, and at the AS. **It never answers
  `NO`.** Every input it reads is circumstantial — a flush stamp proves the mining plane
  ran, not that a search did — so a verdict of NO would assert a fault this evidence
  cannot establish. It is UNKNOWN in two different situations, and the wording keeps them
  apart. The suspicious one is "recent miner activity, but nothing is queued locally or
  verified at the AS", which comes with the one thing worth checking: whether
  `miner.intake_dir` is really the directory the agent's own `search` writes into.
  Everything else is `could not determine — <reason>`: an input that would not read, a
  flush stamp dated in the future, a probe that was skipped or failed, an AS that did not
  answer for this epoch, or an AS answer that claims verified activity and a verified
  count of zero — a contradiction it reports rather than resolving against the
  participant. "No recent activity" stays OK even when the AS is unreachable, because
  with nothing recorded and nothing having run there is nothing to explain.

  `intake writable` is UNKNOWN only when `miner.intake_dir` points somewhere whose parent
  does not exist — `doctor` will not build a directory tree merely to test one. On the
  default layout the parent always exists, so a fresh install gets OK and the intake
  directory created, which is what the first search would have created anyway. Neither
  check changes `doctor`'s exit code, which is non-zero only when *every* check came back
  UNKNOWN: a NO is a successful diagnosis.

- **The documentation was audited against this binary, and the changelog restructured.**
  Releases 0.2.1 through 0.2.4 had no entry at all, and the 0.2.5 and 0.2.6 material sat
  together in one undated pending section; every tag from v0.2.0 on now has its own dated
  heading, and nothing is left pending. Alongside it, every sentence in `README.md`, `npm/README.md`,
  `docs/PARTICIPANT.md`, `AGENTS.md`, `pkg/README.md` and `docs/RELEASING.md` was checked
  against the code that has to make it true. What a participant will notice: the npm
  page was five releases stale and is now the top-level README verbatim, held there by a
  test; `wallet.lock` is described as what it actually covers; the config section lists
  every key this client reads and no key it does not; and "the query is visible in `ps`"
  is corrected to the human form only — the agent form has passed the query on stdin
  since 0.2.6. No behaviour changed.

## v0.2.6 — 2026-09-12

- **Agents now call search through a versioned JSON protocol.**
  `dropin-miner search --stdin` reads one `{"version":1,"query":"…"}` object on
  stdin and writes exactly one JSON object back. The query travels in the JSON,
  so it never appears in the process list and nothing has to escape it for a
  shell. The reply's `ok`, `retryable` and `action` fields say what happened and
  what to do next, so an agent no longer has to read prose to decide whether to
  retry. `-format model` and `-format json` are unchanged and remain the human
  and router-compatibility forms.

- **A search now has a deadline.** One budget — `-timeout`, default 60s —
  covers the whole operation: connecting, headers, reading the body, and the
  single trace-compatibility retry, which shares the same deadline instead of
  starting a fresh one. A search against a stalled router used to be able to
  wait forever.

- **A 2xx from the router is no longer taken on trust.** The response is read
  against a ceiling and refused if it exceeds it, must be exactly one JSON
  object, and must carry a request identity. A truncated, malformed or
  interrupted answer is reported as a server failure rather than parsed as a
  short one, and no mining observation is recorded from it.

- **The trace-compatibility retry now needs the router to say so.** The client
  used to resend a search without its trace on any 400 or 422, which meant an
  invalid query or an unknown tier quietly cost a second request. It now retries
  only when the router answers the exact code `trace_unsupported`.

- **Search result text can no longer steer your terminal.** Provider answers,
  titles, snippets and URLs are remote text. Escape sequences, cursor controls
  and bidirectional overrides in them are replaced before anything is printed,
  every truncation lands on a character boundary, and only `http` and `https`
  links are rendered as links — a `javascript:`, `data:` or `file:` citation is
  shown as an inert note instead.

- **`connect -json` will not answer the mining question for you.** The
  first-run "enable mining rewards?" question is answered by a terminal, or by
  an explicit `mining.enabled` in the config, or by a decision already on file.
  Asking for JSON output is none of those, so where that question would come up
  unanswered, `connect -json` now stops before registering and says so in the
  envelope rather than quietly taking the default. Scripted installs that set
  `mining.enabled` explicitly are unaffected.

- **`status`, `doctor` and `connect` take `-json`.** Same checks, same
  decisions, same output by default; the JSON is a second rendering of the facts
  the text report already gathered, for scripts and SDKs that would otherwise
  have to scrape it. No credential appears in it.

- **The installed agent instructions no longer say every search earns.** They
  now teach the JSON protocol, explain that a successful search and mining
  credit are separate things, point at the mining state for the latter, and say
  that result text is untrusted web content rather than instructions. Blanket
  "never retry" and "always use this one" rules are gone: the envelope says what
  is retryable, and which search tool to fall back to stays the user's choice.
  All six supported hosts — Claude Code, Codex, Cursor, opencode, Pi and Hermes
  — get the updated text, and Hermes' also explains its one-time hook-approval
  prompt.

- **A failing authorization is recognized by what the error is, not by how
  it is worded.** Whether `status`/`doctor` tell you your authorization
  needs attention or that delivery failed was decided by matching phrases
  inside error messages; it is now decided by the error's own type and the
  HTTP status the AS actually answered with. Every message reads exactly as
  before.

- **Wallet files are now written through the same durable writer as the
  rest of the client.** The wallet's own writer did everything but sync the
  directory, so a crash at the wrong moment could leave a file whose bytes
  were on disk but whose directory entry was not — recoverable only from
  the 24 words. Nothing about what is written, or where, changes.

- **Pi and Hermes are supported hosts**, each with a skill and its own
  lineage channel: an auto-discovered extension for Pi
  (`~/.pi/agent/extensions/`), a `pre_tool_call` hook in `config.yaml` for
  Hermes. What rides with a search differs by host and is now described
  honestly in both places it is documented — Pi carries the session, the
  call, the assistant text that led to that search and the context-window
  generation; Hermes' hook payload exposes no assistant text and no
  compaction state, so it carries the session, call and turn only. Every
  identifier is hashed before it leaves the machine, and the assistant text
  Pi sends is redacted and capped *in the extension*, before it is ever put
  on a command line. Two behaviors worth knowing: Hermes asks once to
  approve the hook (or `--accept-hooks`), and a `config.yaml` that already
  has a `hooks:` section of its own is left untouched with the snippet
  printed to paste, rather than edited on a guess — YAML silently keeps the
  last of two identical keys, so guessing wrong would delete hooks you
  wrote. `agents prefer`, `agents status` and `-client` all know both hosts.
  The original Pi and Hermes integration was contributed by @AhmadAshraf2; the
  hardening above sits on top of that work. **Neither adapter has been live-smoked
  against its real host** — both are covered by tests that execute the installed
  artifacts, and both were validated by the contributor during development, but a
  smoke run against a live Pi and a live Hermes remains outstanding and is a
  pre-release check for whichever release performs it.

## v0.2.5 — 2026-09-11

- **Wallet custody hardening: exclusive/recoverable creation, a bounded keyfile
  decoder, a send journal, and a stricter confirmation rule.** Wallet creation
  (`wallet init` and `mining enable`'s address question) now goes through one
  locked path, so two commands started at the same time can never both
  generate a key — one generates, the other recovers the same result rather
  than silently overwriting it. `wallet send` journals a transaction before
  broadcasting it (`wallet/pending_tx.json`): a lost node response now reports
  **"outcome unknown"** instead of silently retrying with a fresh signature,
  and the next `send` or `balance` resolves it against the node first. A
  transfer is now reported confirmed only when the node's response actually
  matches the transaction sent, and `wallet send` refuses to sign if the
  node's own chain id does not match what was configured. `-abandon-pending`
  and `-insecure-node` are new flags; `wallet.lock` and `pending_tx.json` are
  new files in the wallet directory. See `README.md`/`docs/PARTICIPANT.md`
  for what "outcome unknown" means and what to do about it.
- **Every network default the binary carries now lives in one place**
  (`pkg/config.Default*`), including a per-chain table for the wallet's
  default RPC node; the installers' own literals are checked against it by a
  test. The RPC node default used to be a single plain-http devnet address —
  it is now an https testnet endpoint, chosen from `[mining] chain_id`.

## v0.2.4 — 2026-09-11

- **Delivering the same observation twice can no longer cost you the credit for it.**
  Every observation is given a `client_record_id` the moment a search records it at
  intake, and keeps it through promotion, every retry, a quarantine and a restart. That
  identity is what makes a redelivery recognizable as the same piece of evidence rather
  than a second one, so delivery is idempotent and the economic credit is at most once.
  A record whose identity predates this release is upgraded as it passes through
  recovery, not discarded.

- **Retry state now survives the process.** The attempt count and the time the next
  attempt is due are persisted, so a machine that restarts between attempts resumes the
  backoff it was in rather than starting over — and a record that has been marked
  terminal is marked *before* it is moved to quarantine, so a recovery pass can never
  pick it back up and submit it again.

- **The AS's answer has to be about the record it is answering.** An acknowledgement is
  read against a ceiling, must be exactly one JSON document, and must carry both a
  non-empty observation id and the same `client_record_id` the client sent, before any
  local evidence is eligible for removal. Anything malformed, oversized, incomplete or
  belonging to a different record leaves the evidence in place for the next attempt. The
  AS answering that it has already accepted this record is a terminal success, not
  something to retry — a duplicate is the AS agreeing with us, not a failure.

- **Every durable write in the delivery path now goes through one writer** (`pkg/fsx`):
  temp file, fsync, atomic rename, with a Windows path that publishes write-through
  rather than pretending the platform behaves like POSIX. The code that did this lived
  inside the auth store and was near-copied elsewhere; there is one copy now, and the
  spool, the collector and the wallet all use it.

## v0.2.3 — 2026-09-11

- **An interrupted `connect` no longer risks a second identity for one participant.**
  Registration became one journaled transaction: the platform's complete, validated
  answer is written to `registration_pending.json` before the credential or the agent
  record is published locally, and the next run finds that journal and finishes the
  publication from it. It never registers again to recover, and the journal is cleared
  only once both halves have been published and verified. A `Register` whose response
  was lost leaves nothing fabricated on disk and may simply be retried.

- **An unclaimed registration that has expired is replaced only by a foreground
  `connect`, and only on the platform's word.** The replacement requires an
  authenticated live status that explicitly says `expired`; a missing credential, a
  status call that fails, an agent the platform does not know, and a healthy
  registration each leave the identity alone. `connect -resume` — the detached
  background poll — records the expiry and never mints anything. What the replacement
  changes is the platform agent and its key; the wallet, the payout choice, the
  authorization state and the mining evidence are all preserved.

- **A corrupt registration record beside a stored credential is a conflict, not a
  reason to register again.** It refuses, and says that `-force` is what replaces a
  credential deliberately. The journal itself is owner-only and strictly validated: it
  briefly holds the platform key, so it is held to the same rules as the credential
  file it is about to write.

## v0.2.2 — 2026-09-11

- **One file decides whether mining is on, and it is not the config.**
  `mining_decision.json` in the state directory is the runtime authority, with four
  distinct states — enabled, disabled, undecided and degraded. Undecided is a normal
  first-run state; degraded means the local authority could not be trusted, and mining
  stops until it is repaired rather than being quietly treated as off or on. `[mining]
  enabled` in the config is the scripted first answer for a headless onboarding run and
  nothing more; whether an authorization server exists is decided by `as_url` being
  set, independently of that answer.

- **Degradation is now recorded instead of being forgotten.** Three health records —
  `decision`, `capture` and `flush` — persist why mining is not working, using a fixed
  reason vocabulary (`decision_unreadable`, `intake_unwritable`, `sandbox_restricted`,
  `flush_spawn_failed`, `auth_state_unavailable`, `submission_failed`, `spool_backlog`)
  so a script can read them and a person can search for them. `status` and `doctor`
  both show them. Each clears only when its own failure is proven recovered: one
  component succeeding never erases another's unresolved record, and a successful
  target lookup does not clear a submission failure that has nothing to do with it. A
  normal "no open target yet" answer is not degradation and records nothing.

- **A search still succeeds when the mining side does not.** Capture and flush-spawn
  failures are recorded and reported; they never change the search's result or its exit
  code.

- **`doctor` opens only what already exists.** It creates no state directory, no DPoP
  key, no enrollment, and repairs no mining state merely to diagnose it. An
  authenticated check may still rotate a refresh token it already holds, through the
  ordinary cross-process refresh lock.

## v0.2.1 — 2026-09-11

- **A search authenticates only from the credential sources this client defines, and
  `OPENAI_API_KEY` is no longer one of them.** It had been the last fallback, on the
  reasoning that every SDK user already exports it; the effect was that a personal key
  sitting in a shell could silently become the key a search was metered against. Key
  resolution is now `TOKENDROP_API_KEY`, then the owner-only credentials file
  `login`/`connect` wrote, and nothing else. An installation that had been relying on
  `OPENAI_API_KEY` for searches needs `dropin-miner login` (or `connect`) once.

- **Trace text is scrubbed before it is cut, not after.** Redaction now runs over
  complete, bounded source entries, and the history size cap is applied to the result —
  the other order can slice a secret in half and leave neither half recognizable to the
  redactor. The same rule holds in the installed opencode plugin, which is exercised in
  the test suite by actually running it under Node.

- **Cursor's automatic permission decision got stricter.** The hook grants one only
  when the command names the hook's own installed executable and matches an explicitly
  supported command shape; compound commands, unrelated copies of a similarly-named
  binary and anything it does not recognize get no automatic decision at all. The same
  strict recognition is what stamps search lineage, so the two cannot disagree.

- **What travels with a search is stated plainly** in `README.md` and
  `docs/PARTICIPANT.md`: recent agent context accompanies a search to the Twilight
  search router as part of the trajectory/search product, `TOKENDROP_TRACE=off`
  disables trace transmission, and mining/AS receives metadata observations only.

## v0.2.0 — 2026-09-10 (the release that makes `connect` the install path)

A minor bump, not a patch: two new commands (`connect`, `mining disable`), a new
config key (`platform.agents_api_url`), and the installers move from the manual
enrollment-token flow to `connect`. Nothing an existing config names stops
working, but every installation from before this release is expected to be
removed and reinstalled rather than upgraded in place — the state directory's
meaning changed (one decision file) and no migration is carried for it.

- **Agent onboarding: `dropin-miner connect` and `dropin-miner mining enable`.**
  A headless coding agent can now register itself with the search platform and
  search immediately at a reduced, unclaimed tier; the participant then claims
  it with one visit to a printed URL, at which point mining enrollment, wallet
  creation (or an address typed at a terminal) and payout declaration all
  proceed unattended. Enabling mining is one question, asked once, at whichever
  terminal is present — `connect`'s first run or, later, `mining enable` — and
  the answer is a decision the client honors from then on, not a default it
  recomputes. **`setup.sh` and `install.ps1` now drive this path** — see the
  installer bullet below; the manual enrollment-token flow they used to run
  is still in the binary, for the portal's older path, just no longer what a
  fresh install runs.
  Full design: `tokendrop-auth-server-design`'s
  `docs/implementation/search-platform-agent-onboarding-design.md`.

- **The installers register instead of enrolling.** `setup.sh` and
  `install.ps1` no longer generate an enrollment token, prompt for an sr- key,
  or run `join`: they write the config and run `connect`, which asks the
  mining question at whichever terminal is present, registers with the
  search platform (storing the key it mints), creates or takes a wallet, and
  prints a claim link — search works before that link is ever visited.
  `[mining] enabled = true` is no longer written unconditionally;
  a terminal's answer is the decision, and a genuinely non-interactive run
  (`TOKENDROP_MINING=1`, no terminal present) writes it instead, exactly
  matching `connect`'s own scripted-install path. A fresh install is
  therefore never left with no mining decision on file at all — which is
  also why an absent decision file now reads as **stopped**, not active:
  that default existed only for these installers' old shape, and the last
  installation still running it will be reinstalled, not migrated.
  `enroll -assertion`, `login`, `join`, `wallet register` and `payout set`
  keep working for the portal's older, manual path; they are simply not
  what either installer runs anymore.

- **`connect` asks the mining question before it registers, not after.**
  The claim page's mining pre-tick reads the `requested_scopes` hint sent at
  registration; a fresh `connect` used to register first and ask second, so
  the hint could only ever come from a flag, not the answer — every install
  from `setup.sh`/`install.ps1` passed it unconditionally, so the claim page
  pre-ticked mining even for a participant who had just answered no. The
  hint is now built from the terminal's answer (or, non-interactively,
  `[mining].enabled`): `["mining"]` for yes, nothing for no. The `-mining`
  flag is gone — it was the only source of the hint and is now redundant.

- **`[platform]` is now two URLs, not one — fixes a real live-test failure,
  not a hypothetical.** `connect`/`mining enable` assumed the search platform's
  human portal and its machine-facing `/v1/agents/*` API shared one origin
  (`platform.nyks.dev`); the first live test against the real deployment
  registered a `404`, because the API actually lives on a separate host,
  `agents-v1.nyks.dev`. New `platform.agents_api_url` (defaults to
  `https://agents-v1.nyks.dev`) is what register/status/enroll actually dial;
  `platform.base_url` (unchanged, `https://platform.nyks.dev`) is now purely
  the origin a returned `claim_url` is checked against, never dialed itself.
  An existing config naming only `base_url` keeps working unchanged — the new
  key has its own default. Config safety, found in review before this shipped:
  a config naming only a loopback `base_url` (every local dev/test setup)
  defaults `agents_api_url` to that same loopback address rather than
  silently registering against real production; a custom non-loopback,
  non-default `base_url` (a devnet, a staging portal) is refused outright
  unless `agents_api_url` is also given explicitly, rather than guessed.

- **`mining enable`'s re-approval step now sends you to the right place.**
  Previously it reprinted the agent's original claim link — already
  consumed by the first claim, so re-submitting it always failed with
  "this claim link is not valid any more." search-router's poll response
  now carries a direct `console_url` once an agent is claimed, and
  `mining enable` uses it: one link, no failed code submission first. An
  agent that was never claimed at all, or whose registration expired
  before being claimed, gets its own correct message instead of being
  routed through this same fallback.

- **`dropin-miner mining disable`.** Stops mining for this installation's
  agent: a best-effort self-service revocation of its own AS family (RFC
  7009), separate from the platform's own granted scope, which only a
  human at the console can revoke — `status` says so plainly (`mining
  here: stopped` / `platform authorization: still granted — to revoke
  the authorization itself, use the console`) for as long as that holds.
  The stop itself never depends on the network: the decision and the
  enrollment record are cleared locally first, and the AS-side
  revocation is attempted after, best-effort; if the AS can't be
  reached, a marker survives for the next flush or resume to retry, and
  `mining enable` — the existing command, no new path — mints a fresh
  family the normal way. The spool is never purged on disable: an
  installation re-enabled inside the capability window can still
  deliver what it already captured.

- **One decision, not three.** `[miner] enabled`, `[mining] enabled`, and
  the stored mining decision used to each gate a different slice of
  whether mining was actually active — search intake and the flush read
  the first, enrollment and declaration fell back to the second (config)
  when the third (a stored decision) had never been written. Collapsed
  into one: `mining_decision.json`, read by search intake, the flush,
  connect, its resume, and `status` alike, and nothing else. `[miner]
  enabled` now means only "router intake is configured" and never
  overrides it; a legacy install that only ever set `[mining] enabled =
  true` in its config (`setup.sh`'s own shape, which asks no terminal
  question of its own to persist a decision from) defaults to active,
  matching what that config has always meant in practice.

- **`doctor`'s "joined this epoch" local fallback never actually fired.**
  It read `enrollment.json` via `SaveEnrollment`/`LoadEnrollment`, and
  nothing has ever called `SaveEnrollment` — the driver's own epoch-join
  bookkeeping (`JoinState`) is in-memory only, so this was always empty
  on every installation, ever. Replaced with what the disk actually
  records: a stored AS authorization, and — when the agent-onboarding
  registration recorded one — when and for which platform slot it was
  obtained. It can no longer name a specific epoch, because nothing
  durable does. `SaveEnrollment`/`LoadEnrollment` are deleted.

- **`doctor`'s "enrolled" fix pointed at the legacy `enroll` command, even
  on a `connect`-based install.** An installation that ran `connect` has
  no enrollment token to redeem by hand, so `dropin-miner enroll -config
  <file>` did nothing useful when printed as the remediation. The fix
  text now depends on whether this installation ever registered with the
  search platform: the legacy command for one that never did, and
  connect-appropriate guidance otherwise — claim the printed URL and run
  `connect` again for no authorization at all, `mining disable` then
  `mining enable` for a stored authorization the AS rejected.

- **Logged, not fixed:** `RemoveProviderCredential` (unbinding an
  OpenRouter provider credential at the AS) has no caller — `provider`
  can register a key and has no way to unregister one, mining disable or
  not. Out of scope here (mining disable's own scope is the
  `SEARCH_ROUTER_V1` family, which holds no provider credential at all
  — §35.1); tracked for the `OPENROUTER_V1` path next release.

## v0.1.7

- **Fresh installs now default to the public testnet.** `setup.sh`, `install.ps1`,
  `README.md`, and `npm/README.md` point new installs at `twilight-testnet-1` /
  `rewards.nyks.dev` instead of the internal `twilight-devnet-3` / `minis.nyks.dev`.
  This does not touch existing installs — a config file's values always win over the
  script's defaults, so an existing `tokendrop.toml` keeps enrolling against whatever
  it already names. It does mean an install from before this and one from after it
  are, by default, mining different chains, and the wallet's chain-id in signed
  payouts differs accordingly. The search router (`router-api.nyks.dev`) is
  unchanged either way.

- **`miner.router_url` refuses a routable `http://` value at config load**, matching
  the rule `mining.as_url` already has — every search sends the participant's sr- key
  in `Authorization` to `router_url`, so this closes the same cleartext-credential
  exposure the `as_url` rule exists for. The loopback carve-out is an exact match
  (`isLoopbackHost`): `127.0.0.1`/`localhost`/`::1` are fine over plain `http://`,
  but `192.168.x.x` and `host.docker.internal` are **not** loopback and are refused
  too, even though both are plausible choices for a locally-run router. `search`
  fails with a clear, loud message naming the rule. `hook`'s config load failure is
  swallowed by design (the hook fails open, never blocking a tool call) — an
  installation whose `router_url` this newly refuses will find lineage and tracing
  quietly stop, with nothing printed, rather than erroring.

- **Trace redaction (`pkg/redact`) patterns are narrower, and one output format
  changed.** Fixes measured false positives (CSS classes and BCP-47 locale tags shaped
  like `sr-`/`sk-` keys, generic cache keys, ssh/git remote targets, URL path segments,
  ordinary prose containing the word "bearer") that were being redacted out of trace
  text. Separately, the URL-userinfo replacement no longer reintroduces a trailing
  `@` after its placeholder (`user:pass@host` becomes `[REDACTED]host`, not
  `[REDACTED]@host`). `pkg/redact` is the shared implementation this repo owns and
  `tokendrop-proxy` is expected to eventually consume by import — this format change
  will also change the proxy's own redacted log output whenever that import happens,
  not just this client's.
