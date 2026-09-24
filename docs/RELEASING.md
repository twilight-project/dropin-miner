# Releasing

A release has two halves, and the line between them is the point of this document.

**Choosing what to release is a human decision.** Which commit becomes `vX.Y.Z` is
decided by a person, recorded by an annotated tag, and pushed deliberately. Nothing
in CI creates, moves or deletes a tag, and nothing merges a release for you.

**Everything after that tag is mechanical, and is now done by `release.yml`.** Building
six binaries, publishing the GitHub Release, checking that the release actually carries
what the npm wrapper will ask for, publishing the wrapper, waiting out the registry, and
installing the published package on three operating systems to watch the binary say its
own version — all of it used to be a checklist, and a checklist is a thing a person
performs correctly right up until the day they don't. `v0.2.1` and `v0.2.2` are what
that looks like: the npm version bump landed a commit late twice running, and nothing
noticed either time.

This process was originally reconstructed from release history rather than documented
prospectively; this file now records the release invariant, the known historical
exceptions, and the automation that enforces the invariant going forward.

## What CI does and does not do

`ci.yml` runs on every push to `main` and every PR: build, test, vet, race,
cross-compile, lint, vuln. It is verification. **It does not release anything, and
merging to `main` does not trigger a release.**

`release.yml` triggers on exactly one thing: pushing a tag matching `v*`. Nothing else
does.

## The human half

1. **Confirm `main` is green.** `ci.yml` passing on `main` is a precondition, not a
   step — if it isn't green, fix that first, don't tag through it.
2. **Merge a dedicated `release/x.y.z` PR** that bumps `npm/package.json`'s `"version"`
   to the target `X.Y.Z` and writes `CHANGELOG.md`'s entry for the release. Its merge
   commit is what gets tagged. The bump is deliberately *not* automated: CI does not
   commit to this repository, and the number belongs in the commit under review rather
   than in a machine's afterthought.
3. **Run the checks under "Before you tag"** on the exact commit you are about to tag.
4. **Tag and push:**

   ```
   git tag -a vX.Y.Z -m "vX.Y.Z" <commit>
   git push upstream vX.Y.Z
   ```

   Annotated (`-a`) is the rule from v0.2.8 on — it records who cut the release and
   when, which a lightweight tag does not — and the workflow now enforces it. The
   earlier tags are mixed (v0.1.1 through v0.1.6 and v0.2.6 annotated; v0.1.7 through
   v0.2.5 and v0.2.7 lightweight) and are left exactly as they are: retagging a
   published release would move refs that `install.sh`, `install.ps1`, `npm/install.js`
   and an already-published `checksums.txt` resolve against, for a cosmetic gain.

   The tag carries the `v` prefix always: `release.yml` matches `v*`, and
   `npm/install.js` downloads from `releases/download/v${version}`, so a tag named
   `0.2.0` builds nothing and the npm package of that version can never install.

   The tag goes to `twilight-project/dropin-miner` (the `upstream` remote), where the
   releases live and `install.sh`/`install.ps1`/`npm/install.js` look for them — never
   to `origin`, a fork: a tag pushed only there publishes a release nobody's installer
   can find.
5. **Watch the run**, and read the preflight job's diagnostics even when it passes —
   it prints the tagged commit, canonical main, and the asset names it expects.
6. **Run the `install.ps1` check** below. It and the upgrade acceptance in step 7 are
   the acceptance steps still done by hand; the workflow attempts neither.
7. **From the first release after v0.2.9, accept the upgrade by hand.** A real native
   installation of v0.2.9 must run `dropin-miner upgrade` and land on the release just
   published: `dropin-miner version` afterwards reports it, and `dropin-miner upgrade
   -rollback` puts the previous one back. At least one of those upgrades is on a
   Windows desktop, because CI's Windows runners do not represent the antivirus and
   endpoint software a participant's machine runs. v0.2.9 itself cannot be accepted
   this way: no earlier release has `upgrade`.

## Before you tag

These are the checks that have each been bought by a release that shipped without them.
Run them on the commit you are about to tag, not on a branch that resembles it. The
first two are now also enforced by the workflow, which is the point — but a release
that fails preflight is a tag already pushed, and a pushed tag is awkward to take back.

- **`CHANGELOG.md` has a heading for this version**, with its date, and the entry says
  what changed for a participant — not a list of commit subjects. `goreleaser`'s
  auto-generated release notes are the commit-by-commit record; this file is the other
  thing.
- **`npm/package.json`'s `"version"` already says `X.Y.Z`** in the commit you're about
  to tag, or an ancestor of it.
- **The usage text and the installed skill match the flags actually shipped.** A flag
  named in `usageText` or `cmd/dropin-miner/skill.md` that the binary does not accept,
  or a flag it accepts that neither mentions, is the 0.2.6 finding repeating itself:
  the help surface is the contract an agent reads.
- **Any adapter that has never had a live smoke gets one, or the changelog states the
  exception.** Saying "not live-smoked" in the entry is an acceptable answer; saying
  nothing is not. As of v0.2.6 the Pi and Hermes adapters carried that exception.
- **`make verify` is green on that exact commit**, and `ci.yml` is green on `main`.
- **For a release whose updater has never run under antivirus** (v0.2.9 is the first
  such release; step 7's real acceptance only starts with the release after it): an
  optional substitute is available. On a Windows desktop with real-time antivirus
  protection **on**, at the commit to be tagged, run
  `go test ./internal/selfupdate ./cmd/dropin-miner -run 'Replace|Rollback|Upgrade|Uninstall' -count=1`
  — the acceptance test performs the replacement transaction inside the process
  started from the pathname being moved, which is the running-image case CI's runners
  cover with Defender off. If no such machine is available before the tag, the
  changelog entry says so in one sentence, and the check is not claimed to have run.

## The automated half

`release.yml` is five jobs, each depending on the last. The ordering is a dependency
chain rather than a preference: **the npm wrapper carries no binary of its own.** Its
postinstall downloads the GitHub Release's archive for the running platform and verifies
it against `checksums.txt`. So the release must exist, and be complete, before the
wrapper pointing at it is published — and an npm version, once published, cannot be
taken back.

The substantive checks live in `tools/releasecheck`, a small Go program with its own
tests, rather than in shell embedded in the workflow. A release gate nobody can run
except by cutting a release is a gate nobody has tested; every rejection below has a
test that drives it. `tools/releasecheck` is release tooling and not part of the client:
`.goreleaser.yaml` builds `./cmd/dropin-miner` and nothing else, so none of it ships in
the binary, and nothing in `cmd/` or `pkg/` imports it.

Every `uses:` in both workflows, `release.yml` and `ci.yml`, is pinned to a full commit
SHA, with the release it corresponds to in a trailing comment. A tag like `@v4` is a
moving pointer the action's owner can repoint at any time, and `release.yml` holds a
publishing credential — "whatever `@v4` means today" is not an acceptable answer for
what runs beside it. `ci.yml` holds no credential and publishes nothing, and is pinned
anyway, because a CI that changes under you without a diff is the other thing pins
prevent: floating tags would have picked up the node24 releases of `actions/checkout`
and `actions/setup-go` on their own and hidden the deprecation that made them due. The
cost is that pins do not update themselves: bumping them is a deliberate PR, and the
comment is there so a reader can see at a glance which release each SHA is. The CI
hygiene PR that moved both workflows to node24 was the first such bump.

### 1. `preflight` — may this tag become a release?

Nothing is built until all of this holds. Every file is read out of the tag with
`git show`, never out of the worktree beside it, because "npm/package.json at the tag"
has to mean the bytes the tag actually carries — a later fix on `main` does not make a
wrong tag right.

- **The ref is `vX.Y.Z`**, exactly. Pre-release and build-metadata suffixes are refused:
  they would sail through goreleaser and publish to npm's `latest` dist-tag, and nothing
  downstream has ever been exercised against one.
- **The tag is annotated**, per the v0.2.8 convention above.
- **The tagged commit is an ancestor of canonical `main`.** Canonical main is fetched
  explicitly into its own ref and named on the command line — never inferred from the
  checkout branch, which on a tag push *is* the tag. This is the security-relevant one:
  a syntactically valid `v*` tag pointing at some commit in the repository is not a
  release, and write access enough to create a tag must not by itself be enough to
  publish a package under this project's name. See "What this assumes about the
  repository" below for the other half of that argument.
- **`npm/package.json` at the tag says `X.Y.Z`.** This is v0.2.1 and v0.2.2, caught.
- **`CHANGELOG.md` at the tag has a `## vX.Y.Z` heading.**
- **`.goreleaser.yaml` and `npm/install.js` name the same assets.** The two are
  independent statements of what a release file is called — goreleaser's
  `name_template` and the wrapper's own string interpolation — and a release is only
  installable while they agree. The expected asset names are *derived from
  `.goreleaser.yaml` at the tag* rather than hard-coded, so a change to the project
  name, the OS or architecture matrix, the archive format overrides or the checksum
  filename changes what is expected; a change the tool does not model (a second build,
  `targets:`, `ignore:`, a template field it cannot resolve) stops the release rather
  than being guessed at.
- **`npm pack ./npm --dry-run` succeeds.** Note the form: `npm --prefix npm pack
  --dry-run` is *not* the same command — `--prefix` sets the install prefix, not the
  pack target, so it reads `package.json` from the repository root and fails with
  ENOENT.

It then asks GitHub whether a release for this tag already exists, and classifies it
`absent`, `complete` or `partial`. That is what makes a rerun safe; see "When something
fails" below.

### 2. `release-binaries` — goreleaser

Unchanged in substance from what this repo has always done: six static binaries
(linux/darwin/windows × amd64/arm64), each packaged with the binary, `LICENSE` and
`README.md`, checksummed into `checksums.txt`, published as a non-draft GitHub
Release whose body is generated from `git log` between tags and opens with a link to
`CHANGELOG.md` at this tag.

This is the only job with `contents: write`, and it is skipped entirely when preflight
classified the release `complete`.

### 3. `verify-release` — read it back

A separate job reads the release from the API and requires it to carry **exactly** the
expected assets: the six archives and `checksums.txt`, no more and no fewer. Missing is
the obvious failure. An *unexpected* asset fails too — it means the release config grew
an artifact kind this check cannot name, and a verifier that shrugs at files it does not
understand is not verifying the release.

Goreleaser exiting zero is not accepted as the answer to "did the release publish", for
the same reason a test asserts on an observable rather than on the fact that a function
was called.

### 4. `publish-npm` — the wrapper

Publishes `npm/` at the version already committed at the tag. Nothing here bumps a
version, commits, or pushes.

`--ignore-scripts` is passed so publication never executes a lifecycle script from the
package while the OIDC-issued publish credential is live. The package's own `postinstall`
still ships and still runs for whoever installs it; only the publish-time hooks are
suppressed.

This is the only job that references the `release` environment and the only one that
declares `id-token: write` — the permission npm's trusted-publishing exchange needs to
mint a short-lived publish token, and the only credential this job ever holds.

### 5. `smoke-npm` — install it for real, on three operating systems

The only release check that has run the released binary. Everything before it compares
one version string to another, and all of those can pass on a release whose published
wrapper downloads the wrong archive.

On Ubuntu, macOS and Windows, each in a genuinely fresh temporary directory:
`npm install dropin-miner@X.Y.Z`, let the package's own postinstall pick the platform
archive, download it from the GitHub Release, verify it against `checksums.txt` and
unpack it, then `npx --no-install dropin-miner version` and require exactly:

```
dropin-miner X.Y.Z
```

The binary name, then the version **without** the `v`. The tag carries the `v`;
`.goreleaser.yaml` injects `-X main.version={{.Version}}`, which goreleaser resolves to
the tag with the prefix stripped. Expecting `vX.Y.Z` here fails a release that is in
fact correct.

`--no-install` is load-bearing: without it `npx` would happily fetch the package itself
and report a version even if the install had produced nothing.

Between the publish and the install there is a **bounded wait** for the registry to
serve the exact version. `v0.2.7` is why: for about twenty seconds after a successful
publish, `npm view` still reported the previous version. A stale first answer is
propagation, not a failed release, so a single immediate lookup is the wrong check. An
answer that never arrives *is* a failure, so the wait is bounded — ten minutes, polled
every ten seconds — and says so when it expires.

**This Windows job does not run `scripts/install.ps1`.** It exercises the npm path on
Windows: ZIP selection, checksum, unpack, and the `.exe` reporting its version. The
`install.ps1` download check below remains manual and separate; its setup hand-off and
legacy branch run offline in `ci.yml`.

## When something fails

npm publication is irreversible and a tag is awkward to move, so each failure state has
one right answer. A rerun of the workflow never moves the tag, never builds a different
commit, and never invents a version to escape a failure.

**Preflight failed.** Nothing was built and nothing was published. The tag is wrong, or
the commit is. **The failed version number is burned: leave the tag where it is, fix the
problem on `main`, and tag the next patch version.**

This document used to say to delete the tag and reuse the number. That cannot be done —
the "release tag immutability" ruleset carries `deletion`, `update` and
`non_fast_forward` with no bypass actors at all, so the deletion is refused for everyone,
an administrator included — and it should not be. A ruleset cannot tell a published tag
from an unpublished one, and the guarantee it buys is the one this document argues for
fifty lines above: a published tag never moves, because `install.sh`, `install.ps1`,
`npm/install.js` and an already-published `checksums.txt` all resolve against it. A
version number is cheap. That guarantee is not, and it is worth strictly more than the
number, because it is what makes every already-published tag trustworthy rather than
merely usually-trustworthy.

A tag that failed preflight is inert, not dangerous: nothing was built against it and
nothing resolves to it. It sits in the tag list as a version that never shipped, which is
what the CHANGELOG will also say.

The mitigation is to not get here. **Every preflight check can be run locally, on the
commit you are about to tag, before the tag exists** — that is what "Before you tag"
above is for. Run them there and the failure costs you a minute instead of a version
number.

**The GitHub Release succeeded and something after it failed.** Re-run the workflow.
Preflight will classify the release `complete` and goreleaser is skipped — it is not
re-run, because that would replace assets a published npm package may already be
downloading. The run resumes at verification and proceeds to npm.

**The GitHub Release is partial** — some assets uploaded, some did not. The workflow
stops and says which are missing. This one is *not* resumed automatically: re-running
goreleaser over a partial release replaces what did upload, and whether that is safe
depends on whether anything has fetched it yet. Look at the release, delete it and
re-run or repair it by hand, then re-run the workflow.

**npm publish failed.** Re-run. The publish job asks the registry whether
`dropin-miner@X.Y.Z` already exists before attempting anything. If it does not, it
publishes. If it does, it does **not** treat "the version exists" as proof the release
is correct: it downloads the published tarball and compares its contents against `npm/`
at this tag, and only then moves on to the smoke test. A published version whose
contents differ from the tag stops the workflow — nothing is republished, because an npm
version is immutable, and the mismatch is a thing to diagnose rather than paper over.

**The registry never served the version.** The publish step reported success, so this is
a registry or package-state problem. Do not publish again; the version is immutable.

**A smoke install failed.** This is the asymmetric one. **The npm publish has already
happened and cannot be undone**, so the workflow fails loudly and stops, and deliberately
does nothing else: it does not move, delete or recreate the tag, and it does not attempt
to republish. A released package that does not install is a defect to diagnose, and the
next release is the fix. The per-platform matrix does not fail fast, so the run tells you
whether one operating system is broken or all three.

## What installed updaters depend on

From v0.2.9 a native installation can upgrade itself, and every copy that can do so
carries its expectations of a release compiled in. A release that breaks one of them
cannot be upgraded into by any installed updater, and no later release can fix that for
the copies already out there. So these are contracts, and each has a test that fails in
CI before a tag can break it.

**The version command.** A release binary's `dropin-miner version` prints exactly
`dropin-miner X.Y.Z` and a newline — the bare version, no `v`, nothing else — and writes
nothing to stderr, under an environment with nothing of the participant's in it. The
updater runs it on the downloaded candidate before installing anything, and it accepts
nothing else. GoReleaser stamps it with `-X main.version={{.Version}}`, which is the bare
version. `cmd/dropin-miner`'s `TestVersionOutputIsAReleaseCompatibilityContract` builds
the binary that way and runs it through the updater's own validator, and checks
`.goreleaser.yaml` still stamps the bare version.

**The asset names.** The updater computes the archive name from the version and platform
(`dropin-miner_X.Y.Z_<os>_<arch>.tar.gz`, `.zip` on Windows) and expects `checksums.txt`
beside it, with a small checked-in function rather than GoReleaser's templates.
`tools/releasecheck`'s `TestSelfUpdaterAssetNamesMatchGoReleaser` derives the whole matrix
from `.goreleaser.yaml` and requires the two to agree, so changing the naming, the
platforms, the archive format or the checksum file's name fails here first.

**A stable, published release.** The updater takes GitHub's latest release, or exactly
`vX.Y.Z` when asked, and refuses drafts, pre-releases and any tag that is not canonical
`vX.Y.Z`. It downloads only from the canonical repository, following redirects only to
GitHub's own hosts, under frozen bounds: 1 MiB of release metadata, 64 KiB of checksums,
a 64 MiB archive, a 64 MiB executable and 128 MiB of total declared expansion. The v0.2.8
archives are 7–8 MB. A release that approaches half of a bound is a question for review
before it ships, not a reason to raise the bound.

## What this assumes about the repository

Two settings live in GitHub's configuration, not in this repository's files. **This PR
does not configure either of them**; both should be set by a repository administrator.

**A tag-protection ruleset on `v*`,** restricting who may create a release tag to the
authorized release maintainers. This is the other half of the ancestry check, and the
halves are not interchangeable. A tag push runs the workflow file *as it exists at the
tagged commit*, so a crafted commit could carry a `release.yml` with the ancestry check
removed. What such a workflow cannot do is obtain the npm credential — that requires
declaring the `release` environment and satisfying its protection rules. So: the ruleset
constrains **who** may create a release tag, the preflight ancestry check constrains
**what commit** an honest tag may release, and the environment boundary constrains
**what any workflow can reach**. Each covers a case the others do not.

**A GitHub Environment named `release`,** with required reviewers configured on it. There
is no npm secret to hold any more — see below.

## npm authentication — trusted publishing (OIDC)

Publication authenticates with npm's **Trusted Publishing**: `publish-npm` declares
`id-token: write` and exchanges the resulting OIDC token for a short-lived npm publish
token, so no npm secret exists in this repository or its environments, and none is read
by the job. Authorization instead comes from a trusted-publisher record configured on the
`dropin-miner` package on npmjs.com, naming this workflow exactly:

- **Owner:** `twilight-project`
- **Repository:** `dropin-miner`
- **Workflow file:** `release.yml`
- **Environment:** `release`

npm checks the OIDC token's claims against that record before minting a publish token, so
a fork or a differently-named workflow cannot authenticate even with `id-token: write` —
the record is npm-side, not something this repository's files can widen. The `release`
environment's required reviewers remain the boundary that decides which run can reach the
token exchange at all: `id-token: write` is granted to the job, but only a run that also
passes the environment's protection rules gets to use it — the one control a workflow
file cannot route around.

## The npm version, and the two releases that got it wrong

The rule is that the tagged commit already carries `npm/package.json` at the version
being tagged. The run of tags that actually holds it is v0.1.1 through v0.2.0, then
v0.2.3 through v0.2.7 — v0.2.1 and v0.2.2 are the two that broke it, and they are why
preflight checks it. `v0.2.1`'s commit still said `0.2.0`; `v0.2.2`'s still said
`0.2.1`. The bump landed a commit late twice running, and nothing in CI noticed either
time.

Before v0.2.7 the bump was done by hand, sometimes as its own commit (e.g. "npm 0.1.4"),
sometimes folded into whatever feature commit happened to be the release point.
**Beginning with v0.2.7 it gets a dedicated `release/x.y.z` PR of its own**, so the
version is never a line buried in a feature diff and the commit to tag is unambiguous.
The earlier shape is history, not a mistake to rewrite.

Miss the bump and the GitHub Release still builds fine — goreleaser doesn't look at
`npm/package.json` at all. That is precisely why the check is where it is: the failure
is silent on the side that gets watched, and shows up later in the npm package, where it
stays stale until someone notices.

## Manually verifying install.ps1

Half of `install.ps1` is now covered by CI and half is not, and the line between them
is the network.

**Covered by CI.** `ci.yml`'s Windows runner runs the script itself, offline, through
`TOKENDROP_INSTALL_BIN` (`cmd/dropin-miner/installer_bridge_test.go`). With the binary
built from the tree it proves the script probes `setup -h`, hands off to
`dropin-miner.exe setup`, and never runs its legacy config, PATH and connect blocks.
With a stand-in whose `setup -h` exits 2 — what v0.2.8 and older answer — it proves the
legacy blocks run instead. `install.sh` gets the same two runs on the POSIX runners,
its legacy branch being the `setup.sh` shipped beside the binary. What setup itself
writes is covered in-process by `cmd/dropin-miner/installer_test.go` on all three
runners.

**Not covered.** The part `TOKENDROP_INSTALL_BIN` skips: asking `api.github.com` for the
latest release, downloading the ZIP and `checksums.txt`, verifying the checksum, and
unpacking. That needs a real release over a real network, and `release.yml`'s Windows
smoke job installs the npm package, which is a different path.

**Until v0.2.9, the first release with `setup`, is the latest release**, `install.ps1`
on `main` downloads a binary with no `setup`, so what a participant actually runs is
the legacy branch. The manual check
therefore covers both: the download and the branch it lands in. After cutting a release,
run it once by hand (a real Windows machine, or `pwsh` elsewhere — the CIM
processor-architecture query is its only genuinely Windows-only line):

1. `irm https://raw.githubusercontent.com/twilight-project/dropin-miner/main/scripts/install.ps1 | iex`
   against a scratch `$env:TOKENDROP_HOME`.
2. Confirm the download, verify and unpack ran: `==> Latest release: vX.Y.Z -
   downloading …` printed, and a deliberately wrong `checksums.txt` throws rather than
   passing silently.
3. Confirm which branch it took, and that it was the right one for that release. A
   release with `setup`: setup's own narration (`Using binary:`, `Search context`,
   `Setup complete.`) and none of `==> Wrote`, `==> Connecting` or `Installed and
   connected.`. A release without it: those three legacy lines, a `tokendrop.toml` with
   `[platform]`/`[mining]`/`[miner]` blocks, and no unconditional `enabled = true`
   under `[mining]` unless `TOKENDROP_MINING=1` was set with input redirected.
4. Either way, confirm `connect` actually ran: a claim URL printed, and
   `dropin-miner status` afterward showing the registration it made.

The legacy branches of both installers, and `scripts/setup.sh` itself, are kept
through v0.2.10 and v0.3.0, so those releases carry no installer-code delta over
what was validated on v0.2.9; they are not removed in this repository — the
search client continues in a successor repository, and the cleanup happens at
the import there. Once v0.2.9 is the latest release, though, step 3 above lands
on the setup branch regardless — every release from here on ships a binary with
`setup`.

## What this doesn't cover

`npm/package.json`'s version tracking the GitHub release tag 1:1 is the release
invariant this document asserts, not merely a pattern it noticed. That is a change from
how this section used to read: it recorded "six-for-six is the pattern that exists" and
declined to call it a rule, and the history has since stopped being six-for-six.
v0.2.1's tagged commit shipped `0.2.0` and v0.2.2's shipped `0.2.1` — two misses, both
documented above, neither caught by anything. An invariant nobody stated is one nobody
checks, which is exactly how it was broken twice running. It is now checked before
anything is built. If 1:1 tracking is not actually the intent — if the wrapper should
version independently — then this document is wrong and should change, and the check
should go with it.

Still outside the automation, deliberately:

- **The version bump itself.** CI does not commit to this repository. The number belongs
  in a reviewed commit.
- **Which commit gets released.** The whole design boundary.
- **`install.ps1`'s download, verify and unpack**, above.
- **Pi and Hermes live-host smokes**, which need real hosts and are not release
  engineering.

And one thing the workflow cannot prove about itself: a tag-triggered workflow only ever
runs the version of itself that exists at the tagged commit, so changes to `release.yml`
are exercised for the first time by the next real release. There is no way to rehearse a
tag push without pushing a tag.
