# Releasing

This is not automated end to end. Two things ship from one tag, and only one of them
is wired into CI. Written down here because until now it existed only as a pattern
across six releases' worth of git history — discoverable by archaeology, not by
reading anything.

## What CI does and does not do

`ci.yml` runs on every push to `main` and every PR: build, test, vet, cross-compile,
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

Every tag from v0.1.1 through v0.1.6 points at a commit that bumped
`npm/package.json`'s `"version"` field to match the tag about to be cut. No exception
in six releases. No workflow does this — there is no `npm` job anywhere in
`.github/workflows/`, and no automation bumps the version field. It has been done by
hand every time, sometimes as its own commit (e.g. "npm 0.1.4"), sometimes folded into
whatever feature commit happened to be the release point.

**Before tagging `vX.Y.Z`, `npm/package.json`'s `"version"` must already say
`X.Y.Z`**, in the commit you're about to tag or an ancestor of it. Miss this and the
GitHub Release still builds fine (goreleaser doesn't look at `npm/package.json` at
all) — the failure shows up separately, in the npm package, where it stays stale until
someone notices, or `npm publish` refuses outright because npm does not allow
republishing a version number that's already out.

`npm publish` itself is also not in any workflow. It's a manual step, run from
whoever's machine is doing the release, after the version bump and the tag.

## Checklist

1. Confirm `main` is green: `ci.yml` passing is a precondition, not a step — if it
   isn't green, fix that first, don't tag through it.
2. Bump `npm/package.json`'s `"version"` to the target `X.Y.Z`, in the commit you're
   about to tag (or already done in an earlier one).
3. `git tag vX.Y.Z <commit>` and `git push origin vX.Y.Z`. This is what triggers
   `release.yml` — nothing else does.
4. Watch the `release` workflow run; confirm the GitHub Release published with all six
   platform archives plus `checksums.txt`, and that its auto-generated changelog reads
   the way you'd want a participant to read it (this is your last chance to notice a
   commit message that doesn't — there is no second editing pass on the GitHub Release
   body).
5. `npm publish` from `npm/`, by hand. Confirm the published version on npm matches
   the tag.

## What this doesn't cover

Whether `npm/package.json`'s version SHOULD track the GitHub release tag 1:1 (rather
than, say, the npm wrapper having its own independent versioning) is a decision this
document is recording, not making — six-for-six is the pattern that exists; nobody
wrote down that it was the intended rule. If that's not actually the intent, this
document is wrong and should change, not the other way around.

Automating the version bump and `npm publish` into `release.yml` — so a tag push is
the only manual step — is the obvious next move if this keeps being done by hand. Not
attempted here; this document only describes the process as it stands.
