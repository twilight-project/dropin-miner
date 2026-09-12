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
   allowlist. `as_url`, `router_url`, `platform.base_url` and `platform.agents_api_url` are all
   https-or-loopback, enforced at config load (T2b) — every search sends the participant's sr- key
   in `Authorization` to `router_url`, and `connect`/`mining enable` send the platform-issued key
   to `agents_api_url`, the same cleartext-credential exposure `as_url`'s rule closes.
   `platform.base_url` carries no credential — nothing is ever dialed there — it is the portal
   origin a printed `claim_url` is checked against (invariant 12); live testing found the real
   deployment splits the human portal and the machine API across two separate hosts, which the
   original single-URL design missed.
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
10. **One mining authority.** `mining_decision.json` in the state directory is the only runtime
    authority on whether mining is on: `enabled`, `disabled`, `undecided`, `degraded`. Search
    intake, the flush, `connect`, its resume and `status` all read it through `auth.MiningDecision`
    and `miningActive`. `[mining] enabled` in the config is a scripted first answer for onboarding,
    never a switch; `[miner] enabled` says only that intake is configured; `as_url` being non-empty
    is what says an AS exists (`miningASConfigured`). An unreadable or unsafe decision is DEGRADED
    and stops mining.
11. **A claim or console URL is printed, never opened.** `connect.go` and `mining.go` import no
    `os/exec`. Structural, and tested.
12. **`platform.base_url` is compared against, never dialed.** It is the origin a platform-supplied
    URL must match (`validatePlatformURL`); every request goes to `agents_api_url`.
13. **Registration is one journaled transaction.** A complete `register` response is journaled
    (`registration_pending.json`) before `agent.json` or the credential is written, and finished
    from the journal on the next run, never by a second `register`. The journal is consulted before
    anything else. A lost or unreadable record beside a stored credential is rebuilt through
    `GET /v1/agents/me` with nothing local changed before a valid answer. `-force` bypasses
    recovery and authorizes deliberate replacement where local state would otherwise refuse a
    fresh registration. Separately and without `-force`, an ordinary foreground `connect` may
    replace a registration the platform has positively verified as expired; that replacement
    changes the platform agent and its key. `-resume` never registers, rebuilds or replaces.
14. **A wallet send is journaled before broadcast and resolved only by the chain.**
    `wallet/pending_tx.json` is written before the first broadcast; only that first broadcast's
    non-zero CheckTx clears it; a rebroadcast never resolves the journal on any result, because
    CheckTx is not inclusion; resolution is a `/tx` confirmation (matching hash, positive height,
    explicit code) or an explicit `-abandon-pending`. The same signed bytes are re-sent, never a
    second signature. `wallet.lock` spans the pending check through first-broadcast classification.
15. **The machine protocol is versioned, closed and typed.** `search --stdin`, `status -json`,
    `doctor -json` and `connect -json` emit one envelope with the mandatory header (`version`,
    `command`, `ok`, `exit_code`, `status`, `code`, `retryable`, `action`), a closed action set
    (`none`, `retry`, `fix_input`, `connect`, `login`, `check_access`, `report`) and exit classes
    0 through 4. Classification is by typed errors and exact codes, never by message text
    (`trace_unsupported` is the one exact-code retry; a router 401 is `login`, never `connect`).
    Machine mode never invents a participant decision: where a terminal would ask, it answers
    `human_decision_required`. The mandatory header and its semantics are what is versioned
    (`machineVersion` in `machine.go`): changing the required header's shape or the meaning of
    `ok`, `status`, `code`, `retryable`, `action` or `exit_code` requires a protocol version
    change; command-specific payload additions that preserve that contract do not.
16. **Trace text follows one pipeline, and it scrubs before it cuts.** The shared
    `agent_trace_common.js` order is: complete bounded source → scrub → UTF-8 byte accounting →
    32 KiB history tail → envelope → 48 KiB envelope check → base64url. A source entry too large
    to scrub whole is omitted whole, never sliced, because a slice can cut a secret in half. Raw
    host history never enters a command line; hosts that expose less (Hermes) send less; the hook
    fails open and exits 0 on any internal error.

## Subsystems and where their rules live
A subsystem's authority is one file, and a subsystem nobody has watched fail is a hypothesis — so
each line names the file that owns the rule and the test that proves it.

- **The mining decision and the health records** — `pkg/auth/mining_state.go` owns the four states
  and what makes one degraded; `pkg/auth/health.go` owns the three components and the closed reason
  vocabulary. `cmd/dropin-miner/mining_state_health_test.go` drives search and flush through every
  decision and every reason.
- **The registration journal and the rebuild** — `cmd/dropin-miner/connect.go` owns the order
  (journal, publish, clear); `pkg/auth/store.go` owns the journal and the agent record;
  `pkg/platform/client.go` owns `Register`, `Status` and `Me`. Its guards, in
  `agent_onboarding_test.go`, named exactly because a trailing ellipsis is not a test:
  `TestPendingRegistrationRecoveryPublishesWithoutRegister` (the journal is finished, never
  re-registered), `TestConnectRebuildsClaimedRegistrationFromThePlatform` (the `/v1/agents/me`
  rebuild), `TestForegroundConnectReplacesExpiredRegistrationAndPropagatesNewIdentity` (the
  no-flag replacement of a positively-verified expired identity) and
  `TestConnectRefusesCorruptRegistrationWithExistingPlatformCredential` (the refusal that
  `-force` exists to override).
- **Wallet custody and the send journal** — `wallet_store.go` owns the creation lock and the
  wallet directory's layout; `wallet_journal.go` owns `pending_tx.json` and its resolution;
  `wallet_tx.go` hand-encodes the signed bytes. `wallet_lock_test.go` proves creation is exclusive
  and `wallet_journal_test.go` proves a rebroadcast never resolves a journal.
- **Durable evidence delivery** — `pkg/fsx` owns the atomic, fsync'd write (Windows included);
  `pkg/mining/spool` owns the queue and its quarantine; `pkg/mining/collector` owns attempts and
  the next-attempt time; `pkg/auth/submit.go` owns the AS exchange and what counts as an ack.
  `cmd/dropin-miner/delivery_health_test.go` proves a quarantine cannot be cleared by an unrelated
  success.
- **The search protocol** — `search.go` owns the request and the fail-open intake;
  `search_protocol.go` owns the deadline, the bounded read and the one exact-code retry;
  `search_machine.go` owns search's classification; `machine.go` owns the header, the action set
  and the exit/status correspondence; `report_json.go` owns `status`/`doctor`/`connect` JSON.
  `search_protocol_test.go` and `report_json_test.go` guard them.
- **The trace bridge** — `agent_trace_common.js` owns the pipeline and is spliced into both
  JavaScript hosts (`opencode_plugin.js`, `pi_extension.ts`) by `renderAgentScript`;
  `hermes_hook.go` is Hermes' own, and sends less because its host payload carries less.
  `trace_boundaries_test.go`'s `TestEveryJSHostRendersTheSharedTraceSource` keeps the splice
  honest; `pi_extension_test.go` and `hermes_hook_test.go` guard the two adapters.
- **`doctor`'s probe** — `doctor.go` owns the one bounded local probe operation (it may create
  the intake directory, publishes at most one inert non-`.json` file there, then attempts
  cleanup — three filesystem operations, each reported separately, not "one write") and the
  rule that a skipped probe establishes nothing. `doctor_readonly_test.go` proves `doctor`
  creates no state it was only asked to diagnose; `doctor_recording_test.go` proves the probe's
  every failure stage is reported and that `recording` never turns a heuristic into a verdict
  of NO.

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
- **A mutation is evidence only if it landed.** Commit the known-good implementation *before* any
  mutation, so there is a baseline to restore from. Apply the mutation and confirm with `git diff
  --stat` that it actually changed the file; a mutation that did not apply, or that does not
  compile, proves nothing and must not be reported as though it did. Restore only from committed
  bytes — never by retyping what you think was there — and rerun green. Report the baseline SHA,
  the mutation, the test that went red, and the failing line.
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
  at the time. Not rewritten retroactively; not a license to add more.
- The flow, as actually practised since #10: bug and feature work starts from an issue where one
  applies — release-only work and documentation maintenance need not invent one; branch from
  canonical `upstream/main` as it stands, never from an unmerged branch; open a PR with the
  template filled in; all seven CI checks green (the three-OS `test` matrix, plus `race`, `cross`,
  `lint` and `vuln`); merge through GitHub, which produces a merge commit; and tag canonical
  upstream only when cutting a release, per `docs/RELEASING.md`. The older `merge <branch>:
  <phrase>` subject line described the hand-merged branches of the first ten PRs and is not what
  the history has looked like since.
- Keep diffs scoped. **Don't copy a rule you can't explain** — an inherited rule without its argument is
  one the next person deletes.
