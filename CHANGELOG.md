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
  key has its own default.

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
