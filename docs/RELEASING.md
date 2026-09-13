# Releasing

This is not automated end to end. Two things ship from one tag, and only one of them
is wired into CI. This process was originally reconstructed from release history rather
than documented prospectively; this file now records the release invariant and the known
historical exceptions to it.

## What CI does and does not do

`ci.yml` runs on every push to `main` and every PR: build, test, vet, race, cross-compile,
lint, vuln. It is verification. **It does not release anything, and merging to `main`
does not trigger a release.**

`release.yml` triggers on exactly one thing: pushing a tag matching `v*`. When that
happens, `goreleaser` (config in `.goreleaser.yaml`) builds a static `dropin-miner`
binary for linux/darwin/windows × amd64/arm64, packages each with `LICENSE`,
`README.md`, and `scripts/setup.sh`, checksums them into `checksums.txt`, and
publishes a non-draft GitHub Release. The release's changelog is generated from
`git log` between the previous tag and this one (`changelog: use: git`) — there is no
hand-written release-notes step for the GitHub Release itself; your commit messages
are the changelog, which is why they're worth writing for a reader other than you.

**That covers the Go binary. It does not cover the npm package.**

## The step CI doesn't do: npm/package.json

The rule is that the tagged commit already carries `npm/package.json` at the version
being tagged. The run of tags that actually holds it is v0.1.1 through v0.2.0, then
v0.2.3 through v0.2.6 — v0.2.1 and v0.2.2 are the two that broke it, and they are why
the alignment check below exists. `v0.2.1`'s commit still said `0.2.0`; `v0.2.2`'s
still said `0.2.1`. The bump landed a commit late twice running, and nothing in CI
noticed either time.

No workflow does this — there is no `npm` job anywhere in `.github/workflows/`, and no
automation bumps the version field. It has been done by hand every time: sometimes as
its own commit (e.g. "npm 0.1.4"), sometimes folded into whatever feature commit
happened to be the release point. **Beginning with v0.2.7 the bump gets a dedicated
`release/x.y.z` PR of its own**, so the version is never a line buried in a feature
diff and the commit to tag is unambiguous. The earlier shape is history, not a
mistake to rewrite; it is recorded here because it is what the tags before v0.2.7
actually look like.

**Before tagging `vX.Y.Z`, `npm/package.json`'s `"version"` must already say
`X.Y.Z`**, in the commit you're about to tag or an ancestor of it. Miss this and the
GitHub Release still builds fine (goreleaser doesn't look at `npm/package.json` at
all) — the failure shows up separately, in the npm package, where it stays stale until
someone notices, or `npm publish` refuses outright because npm does not allow
republishing a version number that's already out.

`npm publish` itself is also not in any workflow. It's a manual step, run from
whoever's machine is doing the release, after the version bump and the tag.

## Before you tag

These are the checks that have each been bought by a release that shipped without
them. Run them on the commit you are about to tag, not on a branch that resembles it.

- **`CHANGELOG.md` has a heading for this version**, with its date, and the entry says
  what changed for a participant — not a list of commit subjects. `goreleaser`'s
  auto-generated release notes are the commit-by-commit record; this file is the other
  thing.
- **The usage text and the installed skill match the flags actually shipped.** A flag
  named in `usageText` or `cmd/dropin-miner/skill.md` that the binary does not accept,
  or a flag it accepts that neither mentions, is the 0.2.6 finding repeating itself:
  the help surface is the contract an agent reads.
- **Any adapter that has never had a live smoke gets one, or the changelog states the
  exception.** Saying "not live-smoked" in the entry is an acceptable answer; saying
  nothing is not. As of v0.2.6 the Pi and Hermes adapters carried that exception.
- **`make verify` is green on that exact commit**, and `ci.yml` is green on `main`.

## Checklist

1. Confirm `main` is green: `ci.yml` passing is a precondition, not a step — if it
   isn't green, fix that first, don't tag through it.
2. Bump `npm/package.json`'s `"version"` to the target `X.Y.Z` in its own
   `release/x.y.z` PR, and merge that PR. The merge commit is what gets tagged.
3. `git tag -a vX.Y.Z -m "vX.Y.Z" <commit>` and `git push upstream vX.Y.Z`. This is what
   triggers `release.yml` — nothing else does.
   Annotated (`-a`) is the rule from v0.2.8 on — it records who cut the release and
   when, which a lightweight tag does not — while the earlier tags are mixed (v0.1.1
   through v0.1.6 and v0.2.6 annotated; v0.1.7 through v0.2.5 and v0.2.7 lightweight)
   and are left exactly as they are, since `release.yml` matches `v*` either way.
   The tag carries the `v` prefix always: `release.yml` matches `v*`, and
   `npm/install.js` downloads from `releases/download/v${version}`, so a tag named
   `0.2.0` builds nothing and the npm package of that version can never install.
   The tag goes to `twilight-project/dropin-miner` (the `upstream` remote), where the
   releases live and `install.sh`/`install.ps1`/`npm/install.js` look for them — never
   to `origin`, a fork: a tag pushed only there publishes a release nobody's installer
   can find.
4. Watch the `release` workflow run; confirm the GitHub Release published with all six
   platform archives plus `checksums.txt`, and that its auto-generated changelog reads
   the way you'd want a participant to read it (this is your last chance to notice a
   commit message that doesn't — there is no second editing pass on the GitHub Release
   body).
5. `npm publish` from `npm/`, by hand, from that same commit.
6. **Verify alignment.** Four facts, and the fourth is the one that catches what
   registry metadata alone cannot:
   - `git show vX.Y.Z:npm/package.json` reports `X.Y.Z` — the tag's own commit, not a
     descendant of it.
   - the GitHub Release exists for the tag, with the six archives and `checksums.txt`.
   - `npm view dropin-miner version` reports `X.Y.Z`.
   - in a clean temporary directory, `npm install dropin-miner@X.Y.Z` and then
     `npx dropin-miner version` prints `dropin-miner X.Y.Z` — the binary name, then
     the version **without** the `v`. The tag carries the `v`; `.goreleaser.yaml`
     injects `-X main.version={{.Version}}`, which goreleaser resolves to the tag
     with the prefix stripped, and `version` prints `"dropin-miner"` followed by
     that. Expecting `vX.Y.Z` here fails a release that is in fact correct. This is
     the wrapper-to-release alignment: the published wrapper downloading, verifying
     and running the binary the tag built. The three checks above it can all pass
     while this one fails, because they only ever compare one version string to
     another.

## Manually verifying install.ps1

Not covered by CI: `ci.yml`'s Windows runner does `go vet`/`go test`/`go build`
only, never `install.ps1` itself — it downloads a real GitHub release over a
real network call to `api.github.com`, not something worth building a CI stub
for one script. `setup.sh`'s equivalent behavior (the config it writes, ending
with a mining decision on file) has an automated test that actually runs it,
`cmd/dropin-miner/installer_test.go`; `install.ps1` has no PowerShell
equivalent yet. After cutting a release, run it once by hand (a real Windows
machine, or `pwsh` elsewhere — the script is plain PowerShell; the CIM
processor-architecture query is its only genuinely Windows-only line):

1. `irm https://raw.githubusercontent.com/twilight-project/dropin-miner/main/scripts/install.ps1 | iex`
   against a scratch `$env:TOKENDROP_HOME`.
2. Confirm the checksum step actually ran: a deliberately wrong `checksums.txt`
   should throw, not silently pass.
3. Confirm the written `tokendrop.toml` has `[platform]`/`[mining]`/`[miner]`
   blocks, and no unconditional `enabled = true` under `[mining]` unless
   `TOKENDROP_MINING=1` was set with input redirected.
4. Confirm `connect` actually ran: a claim URL printed, and
   `dropin-miner status` afterward showing the registration it made.

## What this doesn't cover

`npm/package.json`'s version tracking the GitHub release tag 1:1 is the release
invariant this document asserts, not merely a pattern it noticed. That is a change from
how this section used to read: it recorded "six-for-six is the pattern that exists" and
declined to call it a rule, and the history has since stopped being six-for-six.
v0.2.1's tagged commit shipped `0.2.0` and v0.2.2's shipped `0.2.1` — two misses, both
documented above, neither caught by anything. An invariant nobody stated is one nobody
checks, which is exactly how it was broken twice running, and the alignment check in
step 6 exists because of those two. If 1:1 tracking is not actually the intent — if the
wrapper should version independently — then this document is wrong and should change,
and the check should go with it; what must not happen again is the rule holding by
habit alone.

Automating the version bump and `npm publish` into `release.yml` — so a tag push is
the only manual step — is the obvious next move if this keeps being done by hand. Not
attempted here; this document only describes the process as it stands.
