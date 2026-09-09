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

`pkg/platform` is the one exception to all of the above: it was never copied
from `tokendrop-proxy` and carries no `PROVENANCE` entry. It implements a
second, separately-owned contract — the search platform's agent-onboarding
control plane (`platform.nyks.dev`), authored in `search-router`'s own design
doc, not the AS's — the same "implementer here, authority elsewhere"
convention, just a different upstream. See `AGENTS.md`'s authority section.
