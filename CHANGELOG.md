# Changelog

No format has been settled on for this file yet beyond "readable by the person cutting
the next release." See `docs/RELEASING.md` for how a release actually gets cut.
`goreleaser`'s auto-generated changelog (from `git log` between tags) already covers
the mechanical commit-by-commit record — what belongs here is the handful of things a
participant or operator should be told in plain language, that a list of commit
subjects wouldn't make obvious on its own.

## Unreleased

- **Agent onboarding: `dropin-miner connect` and `dropin-miner mining enable`.**
  A headless coding agent can now register itself with the search platform and
  search immediately at a reduced, unclaimed tier; the participant then claims
  it with one visit to a printed URL, at which point mining enrollment, wallet
  creation (or an address typed at a terminal) and payout declaration all
  proceed unattended. Enabling mining is one question, asked once, at whichever
  terminal is present — `connect`'s first run or, later, `mining enable` — and
  the answer is a decision the client honors from then on, not a default it
  recomputes. **This does not change what a fresh install does yet**:
  `setup.sh`/`install.ps1` still drive the existing manual enrollment-token
  flow; wiring the installer to this instead is a separate, later change.
  Full design: `tokendrop-auth-server-design`'s
  `docs/implementation/search-platform-agent-onboarding-design.md`.

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
