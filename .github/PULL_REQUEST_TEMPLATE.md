<!-- Thanks for contributing to dropin-miner. Keep PRs focused and explain the WHY. -->

## Summary

<!-- What does this change do and why? Link any related issue: Closes #NNN -->

## Type of change

- [ ] Bug fix
- [ ] Feature
- [ ] Refactor / internal
- [ ] Docs
- [ ] CI / tooling

## Affected area

- [ ] `pkg/auth` — AS wire client, OAuth/DPoP, the key store, custody (**security-sensitive**)
- [ ] `pkg/platform` — search-platform agent onboarding (register / claim / enroll)
- [ ] `pkg/mining/*` — spool, promote, collector, draw derivation
- [ ] `pkg/wire` / `testdata/fixtures` — the frozen AS wire contract (**conform upward, see AGENTS.md — never edit a mirrored fixture**)
- [ ] `pkg/redact` — trace redaction
- [ ] `cmd/dropin-miner` — CLI commands (search, hook, flush, connect, mining enable/disable, wallet, agents, doctor, status)
- [ ] `pkg/config` / install scripts (`setup.sh`, `install.ps1`)
- [ ] docs / CHANGELOG

## Checklist

- [ ] `make verify` passes locally (build, test, race, vet, lint, vuln, tidy, cross-compile)
- [ ] Tests added/updated for the change
- [ ] Commit messages carry the reasoning (what was rejected and why), no `Co-Authored-By`/`Claude-Session` trailers (AGENTS.md convention)
- [ ] Docs/CHANGELOG updated if participant- or operator-visible behavior changed

## Safety & invariants

Required when the change touches the earning path, custody, or the wire contract — see AGENTS.md's hard invariants:

- [ ] Fail-open on the earning path: a mining-side write/spawn/network failure never fails or blocks a search
- [ ] No prompt/completion content captured as mining evidence
- [ ] Every credential-bearing `http.Client` has an explicit redirect policy
- [ ] Secrets never in argv (stdin or owner-only 0600 files only)
- [ ] No unadvertised URL assembled or called; new hosts are configured, not discovered
- [ ] AS-facing types still conform to the frozen wire fixtures; the provider response decode stays permissive
- [ ] `pkg/` still does not import `cmd/`, and still carries no chain-SDK import

## Security considerations

- [ ] No secrets, API keys, refresh tokens, mnemonics, or wallet material included
- [ ] This PR does not introduce a public security disclosure
- [ ] Security-sensitive behavior (custody, redirect policy, credential handling) is called out for reviewer attention below

Notes:

<!-- Mention any trust-boundary, credential-handling, or custody concerns. -->

## Review

- [ ] Injection-checked every non-obvious guarantee this PR adds (revert the guard, confirm red at the file:line, restore) — see AGENTS.md's testing discipline
- [ ] Risky files / assumptions called out below

## Reviewer notes

<!-- Highlight files, assumptions, edge cases, or areas that need careful review. -->
