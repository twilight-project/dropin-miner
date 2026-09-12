# pkg/ — the participant protocol

These packages are the Twilight mining participant as the proxy implements
it: installation identity (DPoP), enrollment and join, participation
capability, the durable spool and submitter, the observation wire format,
the wallet, and the draw derivation with its golden vectors.

They were copied from `twilight-project/tokendrop-proxy` at the commit named
in `PROVENANCE`, import paths changed and a handful of stale doc-comment
references to the proxy's own shape (`internal/auth`, `internal/forward`,
and similar) rewritten to describe this repo's actual tree, and are public
here so the proxy can import them back. Golden-vector tests travel with
them: `testdata/vectors` and `testdata/fixtures` are byte-identical to the
source.

Change protocol code in ONE place. Until the proxy imports from here, a
change here is a change that must be mirrored there, and the vectors are
what catch a drift.

Two packages are exceptions to all of the above, and `PROVENANCE` covers
neither: `pkg/platform` and `pkg/fsx` were both written here.

`pkg/platform` implements a second, separately-owned contract — the search
platform's agent-onboarding control plane, authored in `search-router`'s own
design doc, not the AS's — the same "implementer here, authority elsewhere"
convention, just a different upstream. Two hosts, not one:
`agents-v1.nyks.dev` (register/status/enroll) and `platform.nyks.dev` (the
human portal a claim_url/console_url points at) — found by live testing, not
in the design doc's original text. See `AGENTS.md`'s authority section.

`pkg/fsx` is this repository's own, and its contract is ours to change. It
was extracted in #31 from the atomic-write code that had been living inside
`pkg/auth/store.go`, so that the spool, the collector and the wallet could
all write through one durable writer with Windows write-through rather than
three near-copies. The package boundary and its API — `WriteFileAtomic`,
`WriteFileExclusive`, the publication/mutation split — are new here; the
write logic inside descends from the imported code it replaced.

## Where each package came from

`PROVENANCE` names one proxy commit and nothing per package, so the origin
below is the commit in *this* repository that first added the package, read
off `git log --follow --diff-filter=A`. Note what `PROVENANCE` actually
records: the copied packages were imported at proxy commit `1949ff62` and
later re-synced to `86265b6`, which is the commit the file now names — the
resync target, not the original copy source.

| package | origin | owner of its contract |
|---|---|---|
| `pkg/auth` | copied from the proxy; added by `7b05458` (the repo's root commit) | AS wire contract, owned in `tokendrop-auth-server-design` |
| `pkg/config` | copied from the proxy; added by `7b05458` | shared with the proxy; the `[platform]` block is ours |
| `pkg/fsx` | written here; added by `07d20b8` (#31) | this repository |
| `pkg/mining/collector` | copied from the proxy; added by `7b05458` | AS wire contract (delivery, PART X) |
| `pkg/mining/draw` | copied from the proxy; added by `7b05458` | AS wire contract; golden vectors travel with it |
| `pkg/mining/promote` | copied from the proxy; added by `7b05458` | AS wire contract (source profiles) |
| `pkg/mining/scope` | copied from the proxy; added by `7b05458` | AS wire contract |
| `pkg/mining/spool` | copied from the proxy; added by `7b05458` | AS wire contract (durable queue) |
| `pkg/observe` | copied from the proxy; added by `7b05458` | AS wire contract (the observation shape) |
| `pkg/platform` | written here; added by `5fab3dd` (#6) | `search-router`'s agent-onboarding design |
| `pkg/redact` | copied from the proxy; added by `7b05458` | this repository; the proxy is expected to import it back |
| `pkg/wire` | copied from the proxy; added by `7b05458` | AS wire contract, checksum-verified against the fixtures |

`pkg/mining` itself holds no Go files; it is the namespace its five
subpackages arrived under, in the same root commit.
