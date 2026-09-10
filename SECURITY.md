# Security Policy

dropin-miner is a Go CLI a coding agent shells out to for web search, and the
participant client for Twilight Slot mining. It holds real secrets on the
machine it runs on, under `~/.tokendrop/` (owner-only, 0600/0700): OAuth
refresh tokens and a DPoP key (the mining authorization), a Twilight payout
wallet (BIP39 mnemonic and signing key), and the participant's own sr-
search-router API key. A vulnerability here can expose or move a
participant's funds, impersonate their mining identity, or leak search/task
content the client is designed never to capture at all.

## Reporting a vulnerability

**Do not open a public issue, pull request, or discussion for a security
vulnerability.**

Report vulnerabilities privately through **GitHub Private Vulnerability
Reporting**: on this repository, go to **Security → Report a vulnerability**.
This opens a private advisory visible only to the maintainer.

Please include as much of the following as possible:

- A description of the issue and its impact.
- The affected command, package, commit, or release.
- Reproduction steps or a proof of concept.
- A failing test, if available.
- Any suggested remediation.
- Whether the issue could expose or move funds, expose a credential (refresh
  token, DPoP key, sr- key, wallet key or mnemonic), or leak search/task
  content.

## What to expect

- Acknowledgement within a few business days.
- Initial assessment and severity triage.
- Follow-up questions where needed to reproduce or validate the report.
- Coordinated disclosure once a fix is released.

We will credit the reporter in the advisory unless they prefer to remain
anonymous.

## Scope

A security issue in this repository is in scope if it can affect:

- exposure or misuse of a participant's refresh token, DPoP key, wallet key,
  mnemonic, or sr- API key;
- unauthorized movement of funds from a participant's payout wallet;
- capture of prompt, completion, reasoning, or tool-call content as mining
  evidence — this must never happen at all (AGENTS.md invariant 2);
- a redirect, off-origin request, or unadvertised URL that exposes a
  credential to a host other than the one it is meant for;
- a mining-side failure that blocks or fails a search (AGENTS.md invariant 1);
- impersonation of a participant's mining identity or agent registration.

Examples of in-scope areas: `pkg/auth` (custody, OAuth/DPoP, the key store);
`pkg/platform` (agent-onboarding registration/claim); `cmd/dropin-miner`'s
`wallet.go` and `credentials.go`; the install scripts (`setup.sh`,
`install.ps1`).

## Out of scope

- third-party infrastructure not controlled by this project (the search
  router, the Authorization Server, the search platform, the chain itself);
- attacks requiring control of a reporter's own machine, shell, or agent
  configuration;
- a `TOKENDROP_API_KEY`/`OPENAI_API_KEY` a user chose to export into an
  untrusted environment;
- social engineering, spam, phishing, or abuse reports unrelated to this
  codebase;
- issues already tracked publicly.

If uncertain, report privately rather than opening a public issue.

## Supported versions

dropin-miner is under active development; security fixes target the latest
`main`. A formal supported-version matrix will accompany the first stable
release line.
