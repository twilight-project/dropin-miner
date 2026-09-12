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
