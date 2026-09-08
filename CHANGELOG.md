# Changelog

No format has been settled on for this file yet beyond "readable by the person cutting
the next release." A separate PR adds `docs/RELEASING.md`, describing how a release
actually gets cut; not yet in this tree as of this commit, so not linked here directly.
`goreleaser`'s auto-generated changelog (from `git log` between tags) already covers
the mechanical commit-by-commit record — what belongs here is the handful of things a
participant or operator should be told in plain language, that a list of commit
subjects wouldn't make obvious on its own.

## Unreleased

### In this PR's own diff

- **Fresh installs now default to the public testnet.** `setup.sh`, `install.ps1`,
  `README.md`, and `npm/README.md` point new installs at `twilight-testnet-1` /
  `rewards.nyks.dev` instead of the internal `twilight-devnet-3` / `minis.nyks.dev`.
  This does not touch existing installs — a config file's values always win over the
  script's defaults, so an existing `tokendrop.toml` keeps enrolling against whatever
  it already names. It does mean an install from before this release and one from
  after it are, by default, mining different chains, and the wallet's chain-id in
  signed payouts differs accordingly. The search router (`router-api.nyks.dev`) is
  unchanged either way.

### Landing separately, via PR #1 — not in this PR's diff

Collected here because this branch is the more plausible next tag point, not because
this PR's diff contains them. This PR is a dependent of #1: it must not merge before
#1 does. Given that ordering, both entries are already true of `main` by the time this
PR lands — #1 merges first, then this one merges on top of a `main` that already has
them.

- **`miner.router_url` will refuse a routable `http://` value at config load**,
  matching the rule `mining.as_url` already has — every search sends the
  participant's sr- key in `Authorization` to `router_url`, so this closes the same
  cleartext-credential exposure the `as_url` rule exists for. The loopback carve-out
  is an exact match (`isLoopbackHost`): `127.0.0.1`/`localhost`/`::1` are fine over
  plain `http://`, but `192.168.x.x` and `host.docker.internal` are **not** loopback
  and are refused too, even though both are plausible choices for a locally-run
  router. `search` fails with a clear, loud message naming the rule. `hook`'s config
  load failure is swallowed by design (the hook fails open, never blocking a tool
  call) — an installation whose `router_url` this newly refuses will find lineage and
  tracing quietly stop, with nothing printed, rather than erroring.

- **Trace redaction (`pkg/redact`) patterns will be narrowed and one output format
  changed.** Fixes measured false positives (CSS classes and BCP-47 locale tags shaped
  like `sr-`/`sk-` keys, generic cache keys, ssh/git remote targets, URL path segments,
  ordinary prose containing the word "bearer") that were being redacted out of trace
  text. Separately, the URL-userinfo replacement no longer reintroduces a trailing
  `@` after its placeholder (`user:pass@host` becomes `[REDACTED]host`, not
  `[REDACTED]@host`). `pkg/redact` is the shared implementation PR #1 establishes as
  eventually consumed by `tokendrop-proxy` by import — this format change will also
  change the proxy's own redacted log output whenever that import happens, not just
  this client's.
