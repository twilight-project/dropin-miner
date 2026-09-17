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
5. **Never assemble or call an unadvertised operational URL.** Endpoints come from the discovery document; the
   AS origin is **configured, not discovered**; service-document endpoints are same-origin checked; an
   off-origin provider-authorization template is refused unless on the compiled `providerhosts`
   allowlist. `as_url`, `router_url`, `platform.base_url` and `platform.agents_api_url` are all
   https-or-loopback, enforced at config load (T2b) — every search sends the participant's sr- key
   in `Authorization` to `router_url`, and `connect`/`mining enable` send the platform-issued key
   to `agents_api_url`, the same cleartext-credential exposure `as_url`'s rule closes.
   `platform.base_url` carries no credential — nothing is ever dialed there — it is the portal
   origin a printed `claim_url` is checked against (invariant 12); live testing found the real
   deployment splits the human portal and the machine API across two separate hosts, which the
   original single-URL design missed. The sole exception is the self-updater: `internal/selfupdate`
   contacts only the compiled-in canonical `twilight-project/dropin-miner` GitHub release origin,
   carries no participant credential, follows only its bounded HTTPS GitHub redirect allowlist, and
   no environment variable, config or flag may redirect that origin.
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
    fails open and exits 0 on any internal error. **The bridge that carries the envelope is written
    in the syntax of the shell that will run the command** (`bridge.go`, mirrored in
    `agent_trace_common.js` and pinned by `TestBridgeGuardsAgree`), taken from the host's
    declaration — or, for Claude Code, from the tool its payload names, since that host runs two.
    A POSIX prefix handed to PowerShell is looked up as a program name, which is how every traced
    opencode search on Windows lost its lineage (#68). **Provenance (H-R4): only a bridge an adapter
    wrote for this call may carry that adapter's harness** — it removes every assignment it can
    prove standalone in a declared shell's syntax and writes its own, and leaves a command carrying
    one it cannot remove exactly as it found it. The trace is unauthenticated metadata either way:
    the binary cannot prove where an environment variable came from, and nothing treats a harness
    value as evidence of origin.
17. **Verified replacement and explicit destruction.** `dropin-miner upgrade` fetches only from the
    compiled-in canonical `twilight-project/dropin-miner` GitHub release origin — the one exception
    to invariant 5 — over HTTPS, with no participant credential, following only its bounded GitHub
    redirect allowlist, under the frozen size bounds and one three-minute operation deadline that
    also bounds replacement. Every asset the verifier requires is verified before the archive is
    inspected, and only the one root executable is taken out of it. The staged candidate must run
    and report exactly the release's version before it is installed, and the canonical path itself
    must run and report it again before the displaced binary is committed as the one-level
    `.previous`. Before that commit, a recoverable failure restores the prior binary and leaves
    `.previous` byte-identical; if restoration itself fails, the operation returns
    `manual_intervention`, reports every surviving copy, and cleanup preserves every reported path.
    Upgrade never moves to an older release: only `upgrade -rollback` does, by validating a fresh
    copy of that one local `.previous` — never running it in place — with no network, and swapping
    one level. An npm-managed copy is never replaced. Participant state is removed only by
    `uninstall -purge-state` after an interactive typed confirmation, the wallet address or the
    installation path, that `-yes` cannot supply and a non-terminal cannot reach: protection against
    accidents and non-interactive automation, not against a program driving a terminal.
18. **A prompt answers only to a line the participant typed.** A read that ends without one — an
    interrupt, a closed stdin, a console read the terminal aborted — is not the visual default and
    not the opposite of it; it is the absence of an answer, and the operation stops there, records
    nothing, writes nothing, sends nothing, and exits non-zero. `-yes` answers what it already
    answers and never turns an interrupt into an answer. `prompt.go` owns the rule and the two
    readers that apply it (`promptBufio` over the shared `bufio.Reader` a command's prompts take
    turns on, `promptSetup` over `readSetupLine`); a bare `line, _ := br.ReadString('\n')` at a
    question is the defect, not a shortcut. #81 is the cost: at `Enable mining rewards? [y/N]` the
    discarded read error made an interrupt an empty line, an empty line is not `y`, and invariant
    10's runtime authority was written with a decision nobody made — then `connect` registered.
    A typed refusal and an unanswered question are different outcomes and must stay
    distinguishable by exit code, which is why the ones that change nothing either way
    (`agents install`'s `Proceed?`, `wallet send`'s confirmation) still differ there.

## Subsystems and where their rules live
A subsystem's authority is one file, and a subsystem nobody has watched fail is a hypothesis — so
each line names the file that owns the rule and the test that proves it.

- **The mining decision and the health records** — `pkg/auth/mining_state.go` owns the four states
  and what makes one degraded; `pkg/auth/health.go` owns the three components and the closed reason
  vocabulary (`decision_unreadable`, `intake_unwritable`, `sandbox_restricted`, `flush_spawn_failed`,
  `flush_state_unavailable`, `auth_state_unavailable`, `submission_failed`, `spool_backlog`).
  `cmd/dropin-miner/mining_state_health_test.go` drives search and flush through every decision and
  every reason.
- **The flush lock and stamp** — `cmd/dropin-miner/flushlock.go` owns the rule: one `flush.lock`
  beside the intake directory for every flush of every version, because a 0.2.9 flush that overlaps
  another re-spools records it already read and damages them in code no later binary can patch, so
  the lock cannot be split across paths or generations. Where a sandbox denies writing it (Codex's
  block never grants the miner root), `tryFlushLock` in `minerlock_unix.go`/`minerlock_windows.go`
  opens the existing file read-only and takes the same exclusive lock; only a permission denial
  falls back — `classifyFlushLockOpenError`, one per platform: `EACCES` or `EPERM` (what Codex's
  macOS sandbox returns), `ERROR_ACCESS_DENIED` — and an absent lock it cannot create stops the flush
  with `flush_state_unavailable`.
  Setup, the upgrade transaction and every read-write flush create the file; nothing else uses the
  fallback, because the gate, setup, connect and the destructive exclusion run outside any sandbox
  and must refuse a lock they cannot open read-write. The stamp is a cache under `mining.state_dir`,
  written only through `saveFlushStamp`; a failure never stops delivery. `flushlock_test.go`'s
  `TestSandboxedFlushTakesTheLockReadOnlyAndDelivers` (with its fixture self-proof, `EACCES`),
  `TestSandboxedFlushFallsBackOnEPERM` (macOS `chflags uchg`), the per-platform
  `…FlushLockOpenErrorDecision` tables, `TestFlushLockExcludesAcrossProcessesInEveryOpenMode` and
  `TestAFlushPausedAfterReadingIntakeMakesASandboxedFlushBusy` guard it. A permission test that
  cannot establish its condition leaves through `skipPermissionTest`, which fails instead of
  skipping under `CI=true`: CI runs `go test` without `-v`, so only that makes a green job mean the
  test ran.
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
- **The prompt rule** — `cmd/dropin-miner/prompt.go` owns what counts as an answer and the two
  readers that ask; every prompt in the binary goes through one of them.
  `prompt_abort_test.go` drives each real command to each real question, under both an interrupted
  read and a closed stdin, and asserts the non-zero exit, the unwritten decision and the
  uncontacted platform — and, in the same file, that a typed refusal still declines and exits 0.
- **What a destructive run may leave behind** — `cmd/dropin-miner/lifecycle.go` owns the
  exclusion: which operation locks it takes, which of those files it created, and the rule that
  `release` removes exactly those and only when the operation never proceeded (`proceeded()`).
  `excludeForUpgrade` opts out, in its own words, at its own construction. `install.sh`'s EXIT
  trap and `install.ps1`'s try/finally are the same rule for the download: the temporary directory
  goes on every exit path, because a checksum failure is the one case where what is left behind is
  the file just called untrustworthy. `lifecycle_created_locks_test.go` and
  `installer_tempdir_test.go` guard them; the second names where the temporary directory was
  before it claims it is gone, because asserting an empty scratch directory passes just as well
  when nothing was ever created there.
- **Wallet custody and the send journal** — `wallet_store.go` owns the creation lock and the
  wallet directory's layout; `wallet_journal.go` owns `pending_tx.json` and its resolution;
  `wallet_tx.go` hand-encodes the signed bytes. `wallet_lock_test.go` proves creation is exclusive
  and `wallet_journal_test.go` proves a rebroadcast never resolves a journal. `wallet_acl.go` owns
  who can read the wallet: on Windows the wallet directory and every file in it carry their own
  protected owner-only DACL — set by `openWalletDir` on a directory it creates and by
  `writeWalletFile` on every write, repaired recursively by setup and set-aside adoption (a link,
  a reparse point or an object whose DACL cannot be set fails the run), reported by `doctor`'s
  `wallet access` — because neither a search nor a flush reads the wallet, while
  `credentials.json` and `state\`, which a sandboxed search and flush do read, are deliberately
  left to inherit. POSIX is unchanged: a sandboxed agent runs as the participant, which modes
  cannot express. `wallet_acl_test.go` proves the walk and the repair decision on every OS;
  `wallet_acl_windows_test.go` proves, as a second principal holding an inherited read entry on
  the installation, that `credentials.json` opens and the wallet does not.
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
- **Setup and the installers' bridge** — `setup.go` owns the order (binary, previous
  installation, owner-only directories, adoption, config, connect, environment, agents) and
  the rule that the mining question stays connect's: `-yes` never answers it, and
  `[mining] enabled = true` is written only with no terminal and `TOKENDROP_MINING=1`.
  `setup_adopt.go` owns what counts as an installation and adoption by bundle — the
  identity (`state/` and `credentials.json`) moves whole or not at all, and a destination
  identity is a typed conflict that stops setup non-zero before anything moves and before
  connect runs (`TestSetupStopsOnAnIdentityConflictBeforeConnect`). `setup_config.go` owns TOML quoting and
  the migration policy (parse first; `[miner]` present → byte-identical; otherwise append
  only the missing tables and parse again). `setup_env.go` owns the one-well-formed-block
  profile rule and the Windows `setup-env.json` delta journal, over backends in
  `setup_env_unix.go` and `setup_env_windows.go`; `setup_targets.go` owns `-with`, which
  reaches every target kind. `installer_test.go` drives setup in-process against a sandbox
  and asserts the ownership set on every case, `TestSetupSecondRunIsANoOp` byte for byte;
  `installer_bridge_test.go` runs `install.sh` and `install.ps1` down both branches of the
  `setup -h` probe offline through `TOKENDROP_INSTALL_BIN`;
  `TestSetupFilesNeverImportOSExec` keeps setup from running a process. The package's
  `TestMain` (`testmain_test.go`) refuses to run any test unless `os.UserConfigDir()` and
  the home directory resolve under a temporary test root.
- **`doctor`'s probe** — `doctor.go` owns the one bounded local probe operation (it may create
  the intake directory, publishes at most one inert non-`.json` file there, then attempts
  cleanup — three filesystem operations, each reported separately, not "one write") and the
  rule that a skipped probe establishes nothing. `doctor_readonly_test.go` proves `doctor`
  creates no state it was only asked to diagnose; `doctor_recording_test.go` proves the probe's
  every failure stage is reported and that `recording` never turns a heuristic into a verdict
  of NO.
- **The install registry** — `targets.go` owns the interface, the kinds, the views and the
  slice; `agents.go` owns plan execution; the goldens prove a target's plan cannot drift
  silently, and the structural test proves the public ID set. `Detect` answers with **the
  signal** that made a host count — `detectCommand` for the command it is launched by,
  `detectConfigDir` for the directory it keeps — never a bare yes, because setup's "Found on
  this machine" and `agents status` both print it, and #61 was filed against a machine told
  "Cursor not on PATH" while Cursor sat in `~/.cursor` with its Agent CLI on PATH under
  another name. A config directory is evidence on the same footing as a command: it is where
  the skill and hooks go, so if it is there this client writes there either way.
  `host_detect_test.go` holds each host's signal to a literal and drives soak row S18 —
  `-with cursor`, uninstall, plain setup — because uninstall removed by what was installed
  while setup restored by what was detected, so anything installed with `-with` was lost by a
  round trip. A removal carries its host (`agentRemove.surface`) for the same reason a write
  does: `printPlan` groups by host, and a host with only files to delete used to print under
  the previous host's heading (#88). `targets.go` also owns **the
  shell declaration**: per host and per OS, the set of shells that run its tool calls and what
  runs its hook commands, each cell established by documentation, source or a live run —
  `host_shells_test.go` holds the declaration to a written-out table so no cell moves without a
  reviewed diff. `shell_commands.go` renders every command for a declared shell and owns the
  quoting of the paths in it (never Go's `%q`, which is neither shell's); `skill_render.go`
  renders one form per shell a host runs, and keeps v0.2.9's Bash form, with a line in the
  install plan, for a cell nobody has established. `host_exec_test.go` is what makes any of it
  true: it runs the rendered strings through real shells — bash, sh, Git Bash, both PowerShell
  editions through `-EncodedCommand`, `cmd`, and Hermes' own splitter — against a test build
  with loopback-only stubs, and compares the query the router received with the one that was
  sent.
- **Lifecycle coordination** — `cmd/dropin-miner/lifecycle.go` owns the gate `H.lifecycle.lock`
  (a sibling of the installation, never inside it and never deleted), the one lock order
  (gate → `setup.lock` → `connect.lock` → `flush.lock`; for `uninstall -binary` the same sequence
  then `<resolved binary>.update.lock`, the same update-lock identity an upgrade holds after
  gate → `setup.lock`), how setup, connect and flush pass the gate
  (a person's command waits at most five seconds; a detached child, marked by `spawnDetached`,
  makes one attempt and exits 0 recording nothing) and the exclusion a destructive operation holds:
  the gate, then every operation lock, located from the config only once the gate is held.
  `lifecycle_test.go`'s `TestConnectCannotStartUnderAHeldExclusion` and
  `TestFlushCannotStartUnderAHeldExclusion` prove an operation starting after the check meets the
  gate before it reads or writes anything; `TestSetupHoldsSetupLockThroughItsWholeRun` proves setup
  excludes a destructive operation until its closing message.

- **Uninstall** — `cmd/dropin-miner/uninstall.go` owns the order (plan everything, confirm,
  then integrations, environment, revocation, state, binary), what "this installation's" means
  for a skill, hook or profile block, the purge set and its target guard, and the typed
  confirmation `-yes` cannot supply; `setup_env.go` owns the profile-block removal and the
  compare-and-revert of the Windows user environment; `ownership.go` owns the one npm/native
  classifier setup, `uninstall -binary` and upgrade consult. `uninstall_test.go`'s
  `TestPurgeRefusesEveryConfirmationButTheExactOne`, `TestDefaultUninstallPreservesEveryParticipantByte`
  and `TestWindowsUninstallRevertsOnlyWhatSetupStillOwns` guard them.
  **`ownership_match.go` owns which installation an integration belongs to**: one of this
  installation's binaries AND this installation's resolved config, because two installations
  share a binary whenever the second was made by running the first's copy, and matching on the
  binary alone removed both installations' integrations from either one's uninstall (#73). It is
  the rule the profile block already followed, applied everywhere. The spellings come from the
  same per-shell renderers install writes with, never a hand-kept list; the config is compared
  with `samePath`, because v0.2.9's `%q` hands Windows doubled separators naming the same file.
  Codex's sandbox block names directories rather than a config, so `removeOurSandboxBlock`
  attributes it by its writable roots lying under this installation. **Every artifact this client
  writes names the installation that wrote it** — `TestEveryArtifactNamesTheInstallationThatWroteIt`
  — including the JavaScript adapters, which run no command of ours and therefore carry an
  `INSTALL_CONFIG` line for no other purpose; without it opencode's plugin named nothing and a
  disposable installation's purge deleted the main installation's copy. An artifact naming no
  installation is left and reported, never deleted on the assumption it is ours — with the file
  named and the one `agents install` that would stamp it, so the dead end self-heals.
  **One function reads a rendered path**, `unquoteRenderedPath`, and both halves of an
  attribution go through it: the earlier pair disagreed, because the binary half's helper read a
  double-quoted word only through `strconv.Unquote`, which a cmd-rendered Windows path fails on
  `\U`. The config half went red on the Windows runners and the binary half failed OPEN, which
  is the more dangerous direction and the reason the two must not drift apart again.
  `ownership_test.go`'s `TestUninstallingOneInstallationLeavesAnothersIntegrations` drives soak
  row S20 on each CI OS, plain and `-purge-state`, and asks the production matcher over decoded
  JSON rather than scanning bytes — a raw native path never appears in a JSON-escaped file, which
  is the mistake `79ea5ba`, `f97df97` and `41faaac` each made once.
  `TestARenderedPathIsReadBackWhicheverShellQuotedIt` pins every renderer's spelling as a unit
  case, so that defect no longer needs a Windows runner to surface.
  **`namedInArtifact` is the one place that decides HOW an artifact is read** — a file that
  decodes as JSON is decoded, anything else is scanned as rendered text — and production and the
  tests both go through it. Leaving that distinction implicit cost three defects of one shape:
  a hook file escapes a command twice, by the shell and then by JSON, so on POSIX a byte scan is
  right by luck and on Windows it reads `\"C:\\Users\\…\"` as a lone backslash. Never scan
  encoded bytes for a structure that can be decoded.
  `TestNoHookFileCanEnterTheAttributionPlan` pins the invariant that keeps `attributeRemoved`
  away from a hook file: it runs against the agnostic plan, whose probe command begins with a NUL
  byte and so prefix-matches no rendered command, so `planHooksRemove` never sets changed and
  never reaches its removal. Both halves are asserted, because either alone passes while the
  invariant is broken — and breaking it is the #69 family again, a hook file of ours left behind
  running a binary that is gone.

- **Replacement and rollback** — `internal/selfupdate/replace.go` owns both transactions: on POSIX
  a durable same-directory copy, the candidate renamed over the binary, the canonical path run and
  validated, and only then the copy committed as `.previous`; on Windows the probed sequence
  (binary aside, candidate in, validate, aside replaces `.previous`) with `previous_in_use` when a
  process still runs `.previous`. `rollback.go` owns restoring a validated copy of `.previous`
  with no network. `cmd/dropin-miner/upgrade_locks.go` owns the gate, `setup.lock` and
  `<binary>.update.lock` an upgrade holds, and `cmd/dropin-miner/upgrade.go` owns the command:
  the launch classifier first, one operation deadline over everything after it, success only
  after the transaction commits, `Prepared.DiscardAfterInstall` as the only cleanup, and failure
  classes from typed kinds. `upgrade_test.go`'s
  `TestUpgradeCarriesOneOperationDeadlineThroughEveryStage` and
  `TestUpgradeCommandPrintsSuccessOnlyAfterTheCanonicalPathValidates` guard the command. `replace_test.go`'s
  `TestAFailedSecondUpgradeLeavesPreviousByteIdentical` and `acceptance_test.go`'s
  `TestReplacementAcceptanceWithTheRunningImage` (real processes, on every CI runner including
  Windows arm64) guard them.

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
- `internal/selfupdate` ⊄ `cmd/`, `pkg/auth`, `pkg/platform`: the self-updater fetches public release
  assets and holds no participant credential, so it reaches neither the wrapper nor the key and
  authorization store nor the platform client — `TestSelfupdateImportsNoWrapperAndNoCredential` and
  the `selfupdate-boundary` depguard rule. Its one HTTP client (`NewHTTPClient`) is invariant 3's
  explicit choice: it follows a redirect only over HTTPS, at most five hops, and only to
  `api.github.com`, `github.com`, `objects.githubusercontent.com` and
  `release-assets.githubusercontent.com`; anything else is refused with advice to reinstall. The
  release origin is compiled in and no environment variable redirects it.
- State YOUR forbidden edges here and nowhere else. Don't import another repo's edges; a boundary with
  no argument you can state should not exist.

## Conventions
- Prose over bullet-soup in docs: state the rule, then the reason.
- Commits are **single-purpose**; the message carries the reasoning — what was rejected and why. **No
  `Co-Authored-By` or `Claude-Session` trailers**, as of this file existing — a handful of commits from
  before it did (the earliest pkg/ resync work) still carry them under the attribution convention active
  at the time. Not rewritten retroactively; not a license to add more.
- **An issue body has a shape**, the one every issue since #62 uses, because a defect is read by
  whoever fixes it months later: provenance first (where it was seen — the soak and its condition,
  the platform, the version or the CI run and head), then `**What happened.**` with the evidence in
  a fenced block and the code named (`flushStampPath`, `miner.go`), `**Likely cause.**` where there
  is one, `**Why it matters.**` in the participant's terms, and `**Expected.**`, which is what the
  fixer implements against. It closes with `Severity:` — `cosmetic`, `minor` or `must-fix` — and the
  path it is on, because that is what triage sorts by. The GitHub forms ask for the same things in
  the same order for someone filing from a browser; `gh issue create` renders no form, so an issue
  filed that way carries the shape by hand and passes `--label bug` or `--label enhancement`, which
  the form would otherwise have applied. #64 is the worked example; #78 was rewritten into it.
- The flow, as actually practised since #10: bug and feature work starts from an issue where one
  applies — release-only work and documentation maintenance need not invent one; branch from
  canonical `upstream/main` as it stands, never from an unmerged branch; open a PR with the
  template filled in; all eight CI checks green (the `test` matrix on Linux, macOS, Windows and
  Windows arm64, plus `race`, `cross`, `lint` and `vuln`); merge through GitHub, which produces a merge commit; and tag canonical
  upstream only when cutting a release, per `docs/RELEASING.md`. The older `merge <branch>:
  <phrase>` subject line described the hand-merged branches of the first ten PRs and is not what
  the history has looked like since.
- `unsafe` is admitted in exactly one file, `cmd/dropin-miner/setup_env_windows.go`, where the
  Windows environment broadcast must hand `SendMessageTimeoutW` a string's address;
  `TestOnlyTheEnvironmentBroadcastImportsUnsafe` fails on any other import, so a second use is an
  exception argued in review, never a quiet import.
- Keep diffs scoped. **Don't copy a rule you can't explain** — an inherited rule without its argument is
  one the next person deletes.
