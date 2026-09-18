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
    value as evidence of origin. **A search believes its host's channel, not whatever variable it
    finds** (`searchTrace`): a host that exported `TOKENDROP_LINEAGE` has declared the lineage file
    as its channel and writes no bridge, so a bridge beside it is dropped unread — never decoded,
    never a fallback when the declared file is missing — and reported on stderr only, because the
    machine envelope is what the model reads and a model that knows the variable is one of the ways
    a foreign bridge arrives (#91). With `TOKENDROP_SESSION` exported, a lineage file — declared, or
    found by the walk — is used only when it holds that session; a shell that exports none is
    served exactly as before, and two Cursor conversations sharing one workspace file is #109's own
    fix, not this rule's. **A host started by another host as a shell command inherits the outer
    host's declared channel, and so carries the outer session and label.** That is chosen, not a
    gap: the exported environment is the declaration, and the inner search is work the outer
    session asked for.
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
- **Which installation the profile and the agents belong to** — `cmd/dropin-miner/setup.go` owns it
  (`otherInstallation`, `leftForOtherInstallation`). This machine's installation is
  `$TOKENDROP_HOME`, else `~/.tokendrop`; an explicit `-home` naming any other directory is a
  separate installation, and for it the profile step and the agents step are skipped — named as
  skipped through D4's mechanism, asked before `-yes`, `-with` or the terminal are, because none of
  them changes whose profile and whose agents these are (#84: the documented way to make a
  disposable installation repointed the participant's real agents at it). The closing line and
  uninstall's restore hint both name what does configure such an installation, and
  `TOKENDROP_HOME` as the way to say a directory elsewhere IS the machine's own.
  `setup_other_home_test.go` snapshots the whole sandbox, since the claim is about what is not
  touched.
- **Whose content is inside our marked block** — `cmd/dropin-miner/agents.go` owns the split
  (`markedRegion`, `splitMarkedBlock`, `splitCodexBlock`) and `targets.go` owns what uninstall
  does with it (`removeOurSandboxBlock`). It is `ownership_match.go`'s rule one level finer: H5
  decides whether a block is *this installation's* by what the renderer wrote into it rather than
  where it sits; this decides which tables *inside* it are ours the same way, from the one name in
  `codexSandboxTable`. Neither is about position, which is what made both defects possible — #73
  matched a hook by its binary alone, #82 deleted a marker-to-marker byte range and took Codex's
  folder trust and `[windows] sandbox` with it. A block that will not decode is left alone and
  reported, never deleted. The line scan that finds the boundaries is **not trusted**:
  `oursIsOnlyOurs` decodes whatever was classified as ours and requires exactly our one table with
  nothing nested in it before a byte is deleted or rewritten, because a header the scan misses is
  not an error, only a missing boundary, and the text around it still decodes — the first pattern
  said "anything but `]`", Codex keys folder trust by path, and `[projects.'/home/u/work [1]']`
  went with our table, exit 0, no note. The grammar is fixed; the net is what makes the next miss
  a refusal instead of a lost table, and the two are tested independently on purpose. `codex_block_ownership_test.go` drives install and uninstall against
  the shapes Codex produces; `marked_block_sections_test.go` tests the split on its own first,
  because getting it wrong in the removing direction destroys a participant's settings.
  **Where the block sits is also ours to preserve** (#99): install writes it **where it finds
  it** — `replaceBlockInPlace` over `markedRegion`'s own pre and post — and appends only when
  there is none, so **no byte outside our markers ever moves**. It used to strip and append on
  every write, which reordered a file for a change that was only ever to our own table. L2's
  rule is unchanged, and "below the block" now means directly below rather than at the end,
  which also keeps a block that is no longer last from collecting Codex's next append.
  The position is not recorded anywhere, and that is a decision with a stated cost: uninstall
  erases it, so an uninstall-then-install round trip is byte-identical only for a block where
  our own writes put it, at the end. `codex_block_position_test.go` asserts that case by bytes
  and the other by the guarantee that does hold there — not one line of the participant's own
  moves relative to any other.
- **Our entry in a Hermes `hooks:` block we did not write** — `cmd/dropin-miner/hermes_install.go`
  owns it (`findHermesOwnEntry`, `hermesRunIsRendered`), in the file that already owns the rule it
  follows: there is no YAML parser, so a false-positive refusal is cheap and an ambiguous mutation
  is not. An entry is ours only when the file's structure can be vouched for by the same scan
  install trusts, the entry sits exactly where the renderer puts one, its command is this
  installation's under `ownership_match.go`'s rule and ends in exactly the renderer's words, and —
  the net, as in the Codex block — the lines about to go are byte for byte lines `hermesHookLines`
  produces. What goes is a contiguous suffix of the rendered mapping (two, three or four lines, so
  no key is left with a null where Hermes expects a list); every other line is copied as read.
  Found, removable and mentioned are three different answers: a live hook of ours in a form this
  client will not edit — above all the form **Hermes itself** writes, since `save_config` reloads
  the file and dumps it through PyYAML, dropping our markers and folding the command — is counted
  by install and status and named with its lines by uninstall; a command named somewhere the
  structured find will not read earns a sentence and never an edit or an "already set up".
  "Found" is a claim about what Hermes will run — install turns it into "already set up", status
  into "installed" — so every rule about the block itself runs before it is made, and a rule that
  trips answers at the mention tier: one `pre_tool_call:` key, nothing at list depth that is not a
  list entry, no line at a depth a parser would reject, and our own entry made only of lines a
  parser would accept where they stand. Install's refusal then carries the warning that the file
  already names the command, so the paste advice never lands silently beside a live hook.
  `hermes_own_entry_test.go` pairs each removed shape with neighbors one step away that must come
  back byte-identical, and takes its command from a real install rather than a typed string;
  `testdata/hermes/*.resaved.yaml` is the real output of Hermes' dumper (`resave.py`), never typed;
  and `hermes_differential_test.go` runs 2,875 generated files through the real uninstall, with
  `testdata/hermes/oracle.py` to ask PyYAML what each meant before and after — the only judge of
  a by-line YAML edit that is not the code that made it.
- **What the client writes into a participant's files** — `cmd/dropin-miner/setup_config.go`
  renders `tokendrop.toml`, fresh and migrated; `agents.go`, `setup_env.go` and
  `hermes_install.go` render the blocks that go into a host's own config. Every byte any of them
  contributes is ASCII: Windows PowerShell 5.1 reads a file with no byte-order mark in the ANSI
  code page, so an em dash reaches a participant as `â€”` (#88), and in a PowerShell
  script a mis-decoded quotation mark ends a string early and the file stops parsing.
  `generated_config_ascii_test.go` renders each artifact from ASCII inputs and refuses a byte
  above 0x7F — from ASCII inputs, because a participant whose home is `C:\Users\José` is not
  this client's doing. `installer_bridge_test.go` holds the same rule for `scripts/install.ps1`.
- **What a destructive run may leave behind** — `cmd/dropin-miner/lifecycle.go` owns the
  exclusion: which operation locks it takes, which of those files it created, and the rule that
  `release` removes exactly those and only when the operation never proceeded (`proceeded()`).
  `excludeForUpgrade` opts out, in its own words, at its own construction, and that opt-out
  survived review of #103: **an ordinary operation cannot remove its own lock file.** Removing one
  is safe only while the GATE is held, because the gate is what every contender passes before it
  opens an operation lock — with it held nobody can be between opening that file and locking it,
  so unlinking the name cannot strand a contender on an inode that no longer has one. An ordinary
  operation has released the gate by the time it holds its own lock (connect gives it up before
  its poll loop), and taking the gate back at the end is the reverse of the one lock order, which
  `TestSetupConnectsUnderItsOwnAdmission` exists to forbid — a first attempt at removing these in
  place tripped it on its first run. So the destructive exclusion is the only place L5's removal
  can live, and the locks an upgrade, a setup or a connect leaves are named instead:
  `uninstall`'s `sayLeftoverLocks` lists the gate, `setup.lock`, `connect.lock`, `flush.lock` and
  the binary's update lock that still exist, as safe to delete, in both modes (#103, #115).
  `lock_leftover_test.go` holds it to the invariant either way round — a lock still on disk is
  named, one the run removed is not — and scopes its search to that section, because a purge plan
  prints the full path of everything it removes. `install.sh`'s EXIT
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
- **Who is calling a Claude-format hook** — `hook.go` owns it: `claudeEntryEvent` names the five
  entry points `agents install` writes for Claude Code and the event each is installed under, and
  `runByAnotherHost` decides from the **payload**, never the environment. Cursor loads
  `~/.claude/settings.json` and runs those hooks with its own payload (#87); a payload carrying
  `cursor_version`, or a `hook_event_name` that is not byte for byte the installed event, gets
  nothing written, nothing spawned, nothing printed and exit 0 — one flush per Cursor turn
  instead of two, and no Cursor command rewritten with a `claude-code` bridge. A payload that
  names no event is not evidence and is served as before. The three real Cursor payloads are in
  `testdata/hook/`. `hook_caller_test.go`'s
  `TestOneCursorEventStartsOneFlushWithBothHostsInstalled` is the double flush,
  `TestLineageStandsDownForCursorWhateverTheToolName` is the accident the tool-name rule used to
  hide, and `TestTheCallerGateNamesExactlyTheEventsTheInstallWrites` holds the event table to
  `claudeHooks`' own output so a wrong entry cannot silence a hook inside Claude Code itself.
- **Whose lineage file a search may use** — `search.go`'s `searchTrace` owns the channel rule and
  the declared file's session guard; `miner.go`'s `lineageForCwd` owns the walk, which with a
  session exported climbs past a file of another session to the searching session's own, and
  without one is exactly #97's harness rule. `miner.go`'s `replaceViaTemp` is the one writer
  behind the lineage files, the window state and the flush stamp: a failed write or rename removes
  its temporary file, and the sweep takes only `<file>.<pid>.tmp` of another pid older than
  `lineageMaxAge` — any lineage file's for a lineage write, only its own for the other two, since
  the stamp's directory is shared and the window state can live in TMPDIR. Guards:
  `search_channel_test.go`, `lineage_session_test.go`, `lineage_declared_session_test.go` and
  `temp_cleanup_test.go`, all asserted on the bytes the router receives or the files left behind.
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
  **The rule holds at install time too** (#112). A host has one skill directory, and opencode and
  Pi one adapter file; a hook file holds a list, which is why hook entries never had the problem.
  `agents install` for a second installation overwrote the first's skill silently, so the later
  `agents uninstall` removed a skill that did by then name its config: the removal was correct
  and the damage was done at install. `leaveToItsOwner` (`agents.go`) sits in the two planners
  every writer of those files goes through — `agents prefer` included — and `buildUninstallPlan`
  takes another installation's files out of a host's removal, both through one reading,
  `foreignOwner`, and both in `uninstall`'s own sentence (`leftForeign`). Here the **config**
  decides and the binary does not: an installation whose binary moved still names its config and
  must be able to refresh its own skill.   `agents status` asks the same question through `foreignHost` and says
  `belongsTo` — uninstall's own sentence without the removal verb — where it used to say
  "installed", because `Status` answers from the file existing and a host another installation
  set up read as this one's. `agents_skill_ownership_test.go` guards it on real files;
  `TestUninstallingOneInstallationLeavesAnothersIntegrations` changed direction with it, since
  its old premise — the second install's files name the second — was the defect.
  **An allow rule has one current spelling** (#114). A rule was added when its exact text was
  absent, so a rule whose text had changed was never seen as the same rule and stayed beside
  its replacement for ever; the count settled only because today's three forms happen to be a
  superset of v0.2.9's two. `mergeAllowRules` replaces the rules `ruleIsOurs` recognizes — any
  spelling this client has written, for this installation's binary and config — with the
  current set, as a set, because `claudeAllowRules` writes three prefix forms of one permission
  and there is no pairing between old spellings and new. A set already equal to the current one
  is left exactly as it lies, so a second install still writes nothing. The removing half
  needed no change: `planHooksRemove` already asked `ruleIsOurs`.
  `agents_allow_rules_test.go` guards both, and takes its fixtures from `resolveEntry` rather
  than typing a config path — `resolveEntry` puts the path through `filepath.Abs`, so a typed
  POSIX path is a *different installation* on Windows, and the first version of that file was
  red on both Windows runners for exactly that reason.
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
  case, so that defect no longer needs a Windows runner to surface. **Naming a path is one
  reading too**: `describeOther` shows the DECODED reading, the last `unquoteRenderedPath`
  returns, because a double-quoted word yields the literal one first and a JSON-quoted Windows
  path is then spelled with every separator doubled. Matching was never affected — `samePath`
  reads them all — so the file was correctly left alone and only the sentence was wrong;
  `TestTheInstallationNamedInAMessageIsTheDecodedReading` pins it as a unit case.
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
  `TestUpgradeCommandPrintsSuccessOnlyAfterTheCanonicalPathValidates` guard the command.
  `cmd/dropin-miner/upgrade_rerender.go` owns what follows a committed replacement (#111): the
  host integrations **this installation owns** — `ownedHosts`, which is `ownership_match.go`'s
  rule read off the files through uninstall's own attribution, because `Status` calls a skill
  installed when the file merely exists — are rendered again by a **child process, the binary now
  at the launch path**, since the process running `upgrade` is the version being replaced and its
  tables are the old ones. Never before the transaction commits, never as a condition of the exit
  code, never a host that is another installation's or names none, and never a host that was not
  set up (the child is handed `-client` per owned host). `upgrade_rerender_test.go` records which
  version was at the path when the render was asked for — the observable that tells after-commit
  from before — in `TestAnUpgradeThatIsRolledBackLeavesTheHostFilesAsTheyWere` and
  `TestRollbackReRendersWithTheBinaryRolledBackTo`. `replace_test.go`'s
  `TestAFailedSecondUpgradeLeavesPreviousByteIdentical` and `acceptance_test.go`'s
  `TestReplacementAcceptanceWithTheRunningImage` (real processes, on every CI runner including
  Windows arm64) guard them. Two bounded retries, and no others. `candidate.go`: a candidate that
  has not answered `version` is asked once more, and only on a timeout decided by the package's
  own clock — a wrong version, a malformed line, a byte on stderr or a failure to start is
  evidence and is refused on one call — the frozen five-second budget is not raised, and two
  timeouts are `replacement_failed` (`retry`), never `candidate_invalid` (#95;
  `candidate_retry_test.go`, whose evidence rows answer correctly on their second call so a retry
  would show as an acceptance). `replace.go`'s `moveAside`: the Windows move-aside waits about a
  second, five attempts, only for the sharing-violation and access-denied errors of a file
  something else is briefly holding (`transientlyHeld`, constant false off Windows); a read-only
  directory fails earlier, at `reserve` (#78; `move_aside_test.go` for the policy on every OS,
  `rename_windows_test.go` for a real forced hold on the Windows runners, which CI runs without
  `-v` — to see them run, push a throwaway branch with a verbose step as a draft PR inside the
  fork, never against upstream).

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
