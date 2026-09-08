# Changelog

No format has been settled on for this file yet beyond "readable by the person cutting
the next release" — see `docs/RELEASING.md` for how a release actually gets cut, and
note that `goreleaser`'s auto-generated changelog (from `git log` between tags) already
covers the mechanical commit-by-commit record. What belongs here is the handful of
things a participant or operator should be told in plain language, that a list of
commit subjects wouldn't make obvious on its own.

## Unreleased

- **Fresh installs now default to the public testnet.** `setup.sh`, `install.ps1`,
  `README.md`, and `npm/README.md` point new installs at `twilight-testnet-1` /
  `rewards.nyks.dev` instead of the internal `twilight-devnet-3` / `minis.nyks.dev`.
  This does not touch existing installs — a config file's values always win over the
  script's defaults, so an existing `tokendrop.toml` keeps enrolling against whatever
  it already names. It does mean an install from before this release and one from
  after it are, by default, mining different chains, and the wallet's chain-id in
  signed payouts differs accordingly. The search router (`router-api.nyks.dev`) is
  unchanged either way.

- **`miner.router_url` now refuses a routable `http://` value at config load**,
  matching the rule `mining.as_url` already had — every search sends the
  participant's sr- key in `Authorization` to `router_url`, so this closes the same
  cleartext-credential exposure the `as_url` rule exists for. The loopback carve-out
  is an exact match (`isLoopbackHost`): `127.0.0.1`/`localhost`/`::1` are fine over
  plain `http://`, but `192.168.x.x` and `host.docker.internal` are **not** loopback
  and are refused too, even though both are plausible choices for a locally-run
  router. `search` fails with a clear, loud message naming the rule. `hook`'s config
  load failure is swallowed by design (the hook fails open, never blocking a tool
  call) — an installation whose `router_url` this newly refuses will find lineage and
  tracing quietly stop, with nothing printed, rather than erroring.

- **Trace redaction (`pkg/redact`) patterns narrowed and one output format changed.**
  Fixed measured false positives (CSS classes and BCP-47 locale tags shaped like
  `sr-`/`sk-` keys, generic cache keys, ssh/git remote targets, URL path segments,
  ordinary prose containing the word "bearer") that were being redacted out of trace
  text. Separately, the URL-userinfo replacement no longer reintroduces a trailing
  `@` after its placeholder (`user:pass@host` now becomes `[REDACTED]host`, not
  `[REDACTED]@host`). `pkg/redact` is the shared implementation `AGENTS.md` names as
  eventually consumed by `tokendrop-proxy` by import — this format change will also
  change the proxy's own redacted log output whenever that import happens, not just
  this client's.
