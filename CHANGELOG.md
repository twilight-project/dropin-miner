# Changelog

No format has been settled on for this file yet beyond "readable by the person cutting
the next release." See `docs/RELEASING.md` for how a release actually gets cut.
`goreleaser`'s auto-generated changelog (from `git log` between tags) already covers
the mechanical commit-by-commit record — what belongs here is the handful of things a
participant or operator should be told in plain language, that a list of commit
subjects wouldn't make obvious on its own.

## Unreleased

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
