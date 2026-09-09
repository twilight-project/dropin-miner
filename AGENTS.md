# AGENTS.md — dropin-miner

Agent instructions for the TokenDrop drop-in mining client. Loaded every session.

## What this is
dropin-miner is the **launch client** for Twilight MINIS: a **no-daemon** Go CLI a coding agent
shells out to (`dropin-miner search …`) plus a lifecycle hook. Between calls nothing runs — no
reverse proxy, no resident process. It earns by capturing each provider call's **event id**
(`request_id` for search, `generation_id` for OpenRouter) and submitting it to the Authorization
Server (AS), which **reconciles against the provider** — the client never self-reports volume. It
holds participant secrets on the participant's own machine: OAuth refresh tokens + a DPoP key, a
Twilight payout wallet (BIP39 + signing key), and — only when the Slot's profile is `OPENROUTER_V1`
(the `provider` command; see `enroll.go`'s `AcceptsOpenRouterProfile`) — an OpenRouter zero-spend
key. Under the default `SEARCH_ROUTER_V1` profile the participant holds no provider credential at
all; the AS verifies with its own operator credential.

## Authority — the protocol is owned upstream, not here
The AS↔client wire contract, source profiles, and fixture corpus are owned in
`tokendrop-auth-server-design` (`docs/spec/`, `docs/wire-fixtures/`, checksum-mirrored here under
`testdata/fixtures/`). **Conform upward.** A frozen requirement that looks wrong is a **spec
escalation** to the design side — never a local reading chosen because it's easier to build, never
an edit to a mirrored fixture. `pkg/` is the **shared protocol implementation, owned by this repo**
and consumed elsewhere by import; `cmd/dropin-miner/` is this client's wrapper. Client-specific
decisions (retry shapes, the trace envelope, install/agent behaviour) have no upstream home —
record them in `docs/` here.

The search platform's control plane (`platform.nyks.dev` — `connect`, `mining enable`; `pkg/platform`)
is a **second, separate contract**, same convention as the AS: owned upstream, this time by
`search-router`'s own design doc (`tokendrop-auth-server-design/docs/implementation/
search-platform-agent-onboarding-design.md`, authored for the `search-router` repository and
mirrored there — not owned here either). `pkg/platform` conforms to it the same way `pkg/auth`
conforms to the AS contract: implementer here, authority elsewhere.

## The verify loop
`make verify` = `build test race vet lint vuln tidy cross`. **Green before every commit.** `lint`
is golangci-lint v2 (gosec, misspell locale US, unconvert, depguard, gofmt, goimports); `vuln` is
govulncheck.

## Hard invariants — a change that violates one is wrong even if it compiles and every test passes
1. **Fail-open on the earning path.** A mining-side write, spawn, or network failure MUST NOT turn
   a successful search/inference into a client-visible failure, and MUST NOT add latency the user
   notices. The hook emits nothing and exits 0 on any internal error; `search` swallows intake/flush
   failures. "A mining-side write must never block or fail the search."
2. **No prompt/completion content as mining evidence.** Prompt, message, completion, reasoning, and
   tool payloads MUST NOT be logged, spooled, hashed, sampled, or sent to the AS as evidence
   (contract PART II). `intakeRecord` is a closed field set — request id, host, status code,
   timestamps, chosen provider name — that structurally excludes the query and the answer; there is
   nothing to redact here because nothing is captured. The search-router **trace** is the one
   channel that carries model-influenceable text off the machine (the assistant text just before a
   search, capped and hashed per `trace.go`); it MUST be redacted before egress and is a conscious
   privacy surface, not a default.
3. **Every credential- or key-bearing `http.Client` has an explicit redirect policy.** No bare
   client on `net/http`'s default (follow-10, body replayed on 307/308). Same-origin bounded
   (`auth.SameOriginRedirects`) or refuse (`http.ErrUseLastResponse`) — per client; the forbidden
   state is *not choosing*. Enforced module-wide by `boundary_test.go`. This is not hypothetical:
   the module-wide test is what it is — walking every `http.Client{}` construction rather than
   trusting a package-scoped review — because hand-enumeration missed a real one here.
   `credentials.go`'s login probe sends the participant's sr- key as a bearer credential on a bare
   client; a targeted review of the redirect-guard fix did not catch it, and the module-wide test
   did on its first run. That is the whole argument for invariant 3 being enforced this way rather
   than by code review.
4. **Secrets never in argv.** stdin or owner-only (0600) files, never a flag. The one documented
   exception is the search *query* (not a credential), called out where it happens.
5. **Never assemble or call an unadvertised URL.** Endpoints come from the discovery document; the
   AS origin is **configured, not discovered**; service-document endpoints are same-origin checked; an
   off-origin provider-authorization template is refused unless on the compiled `providerhosts`
   allowlist. `as_url`, `router_url` and `platform.base_url` are all https-or-loopback, enforced at
   config load (T2b) — every search sends the participant's sr- key in `Authorization` to
   `router_url`, and `connect`/`mining enable` send the platform-issued key to `platform.base_url`,
   the same cleartext-credential exposure `as_url`'s rule closes.
6. **Strict for the AS wire, permissive for the provider response.** AS-facing types conform to the
   frozen, checksum-verified fixtures. The provider **response** shape is not frozen and is decoded
   permissively (no `DisallowUnknownFields`) on purpose — we own the AS contract, not the provider's
   product API. The asymmetry is deliberate; don't "fix" it either way.
7. **Durable before visible; remove only on ack.** An observation is persisted (intake→spool, each
   temp-file + atomic rename, fsync'd) before the user sees the result; a spool record leaves delivery
   only on a durable AS ack (PART X). `client_record_id` is minted once per observation, never
   regenerated on retry.
8. **`pkg/` MUST NOT import `cmd/`.** Keeping `pkg/` wrapper-agnostic is what makes it importable and
   is why this repo owns it and can hand it back to the proxy by import, not copy. Enforced by
   `boundary_test.go`.
9. **The chain SDK module tree is banned.** The client hand-builds the protobuf messages a payout
   needs rather than importing the Cosmos SDK — a lean binary shipped to users must not pull the
   SDK's module tree. `go.mod` carries no cosmos-sdk/cometbft dependency. Enforced by
   `TestNoChainImportsAnywhere`.

## Testing discipline — learned the hard way; hold them
- **A test's name is not its assertion.** A green test can encode the bug.
- **A review's reproduction is a vulnerability *demo* (green when vulnerable), not a guard.** A guard
  is green-on-fixed / red-on-vulnerable. Adopting one: invert polarity, give it a real assertion (not
  `t.Log` on both branches), drive the **package's own** code (never a bare `http.Client` — that tests
  `net/http`), and rename it so a green test isn't still called `…LeaksToken`.
- **Injection-check every non-obvious guarantee:** revert the guard, confirm red (naming the
  `file:line`), restore. A guarantee nobody has watched fail is a hypothesis.
- **A source-reading test can't be injection-checked with `go test -overlay`** (overlay is compile-time;
  the test parses the real file). Disk-edit + restore instead.
- **Count the right observable:** connections dialed, not handler invocations; bytes on the wire, not
  "a call was made." Prove a destructive outcome unreachable by enumerating the input domain.
- **Distrust hand-written provider fixtures**; a test that would run against a stale fixture should
  refuse, not invent a value.
- **Scope the search wide before trusting a targeted fix.** `credentials.go`'s bare client (invariant
  3) survived a redirect-guard review aimed at the AS-facing clients because that review's scope was
  "the clients that carry a credential to the AS" — true, but too narrow; the login probe carries a
  credential to the *router*. The module-wide `boundary_test.go` sweep found it where the targeted
  review could not, on the first run. Prefer a structural, scope-agnostic guard over a review's
  enumerated list whenever the two disagree about where the boundary is.

## Import boundaries (`boundary_test.go`)
- `pkg/` ⊄ `cmd/` (invariant 8): the `cmd/` root may reach a `pkg/` package's dependency graph, but no
  `pkg/` code imports the wrapper.
- The mining path (`flush`, `driver`, `collector`) never dials the router and is never on the search
  response path (invariant 1) — proven, not assumed, by
  `cmd/dropin-miner/flush_test.go`'s `TestFlushNeverDialsTheRouterEvenOnFailure`
  (connection-counted, not request-counted). Not import-expressible — `flush.go`/`driver.go`
  legitimately import `net/http` to reach the AS, so there is no package to deny — which is why it
  lives as a behavioral test rather than a second depguard rule.
- No package in the module graph reaches the chain application or the Cosmos/CometBFT SDK
  (invariant 9) — `TestNoChainImportsAnywhere`. `wallet_tx.go` hand-encodes the six protobuf
  messages a bank send needs instead.
- State YOUR forbidden edges here and nowhere else. Don't import another repo's edges; a boundary with
  no argument you can state should not exist.

## Conventions
- Prose over bullet-soup in docs: state the rule, then the reason.
- Commits are **single-purpose**; the message carries the reasoning — what was rejected and why. **No
  `Co-Authored-By` or `Claude-Session` trailers**, as of this file existing — a handful of commits from
  before it did (the earliest pkg/ resync work) still carry them under the attribution convention active
  at the time. Not rewritten retroactively; not a license to add more. Merges are `--no-ff`, subject
  `merge <branch>: <phrase>`, with a real body.
- Keep diffs scoped. **Don't copy a rule you can't explain** — an inherited rule without its argument is
  one the next person deletes.
