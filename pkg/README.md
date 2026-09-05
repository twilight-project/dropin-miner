# pkg/ — the participant protocol

These packages are the Twilight mining participant as the proxy implements
it: installation identity (DPoP), enrollment and join, participation
capability, the durable spool and submitter, the observation wire format,
the wallet, and the draw derivation with its golden vectors.

They were copied from `twilight-project/tokendrop-proxy` at the commit named
in `PROVENANCE`, with only the import paths changed, and are public here so
the proxy can import them back. Golden-vector tests travel with them:
`testdata/vectors` and `testdata/fixtures` are byte-identical to the source.

Change protocol code in ONE place. Until the proxy imports from here, a
change here is a change that must be mirrored there, and the vectors are
what catch a drift.
